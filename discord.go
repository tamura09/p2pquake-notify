package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	// queueDepth は1ルートあたりの送信待ち上限。大地震では各地の震度と
	// 津波予報が短時間に集中するので、受信ループを止めないための緩衝です。
	queueDepth = 64

	// groupTTL は「同じ地震の続報」としてDiscordのメッセージを書き換え続ける期間。
	// 緊急地震速報の一連の報はふつう数分で終わるので、それを過ぎたら
	// 次に同じEventIDが来ても新規投稿に戻します。
	groupTTL = 30 * time.Minute

	sendTimeout    = 15 * time.Second
	maxSendRetries = 4
)

// sink は1つのDiscord webhookへの送信担当。ルートごとに1つ作り、
// それぞれ独立したgoroutineとキューを持ちます。片方のwebhookが
// レート制限で詰まっても、もう片方の通知は遅れません。
type sink struct {
	route  route
	client *http.Client
	zone   *time.Location
	queue  chan *event

	// messages は GroupKey -> 投稿済みメッセージID。送信goroutineだけが
	// 触るのでロックは要りません。
	messages map[string]groupMessage
}

type groupMessage struct {
	id      string
	written time.Time
}

func newSink(r route, client *http.Client, zone *time.Location) *sink {
	return &sink{
		route:    r,
		client:   client,
		zone:     zone,
		queue:    make(chan *event, queueDepth),
		messages: map[string]groupMessage{},
	}
}

// offer はイベントを送信キューに載せます。受信ループから呼ばれるので
// 決してブロックしません。満杯なら最古の1件を捨てて場所を空けます
// (緊急地震速報では古い報より新しい報のほうが価値が高い)。
func (s *sink) offer(e *event) {
	for {
		select {
		case s.queue <- e:
			return
		default:
		}
		select {
		case dropped := <-s.queue:
			log.Printf("route=%s queue full, dropped code=%d", s.route.Name, dropped.Code)
		default:
			// 競合で誰かが先に読んだだけ。もう一度入れにいきます。
		}
	}
}

func (s *sink) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.queue:
			s.deliver(ctx, e)
		}
	}
}

func (s *sink) deliver(ctx context.Context, e *event) {
	payload := renderPayload(e, s.zone)

	// 緊急地震速報の第2報以降は、新しいメッセージを積むのではなく
	// 第1報のメッセージを書き換えます。大きな地震では10報以上届くので、
	// 都度投稿するとチャンネルが埋まり、かつスマホが鳴り続けます。
	if e.GroupKey != "" {
		if existing, ok := s.messages[e.GroupKey]; ok && time.Since(existing.written) < groupTTL {
			if err := s.edit(ctx, existing.id, payload); err == nil {
				s.messages[e.GroupKey] = groupMessage{id: existing.id, written: time.Now()}
				return
			} else {
				// 書き換えに失敗したら新規投稿に切り替えます。続報が
				// まったく届かないよりは、重複してでも届くほうがましです。
				log.Printf("route=%s edit message %s failed, posting new: %v", s.route.Name, existing.id, err)
			}
		}
	}

	id, err := s.post(ctx, payload)
	if err != nil {
		log.Printf("route=%s post failed code=%d: %v", s.route.Name, e.Code, err)
		notifyFailures.Add(1)
		return
	}
	notifySent.Add(1)

	if e.GroupKey != "" && id != "" {
		s.messages[e.GroupKey] = groupMessage{id: id, written: time.Now()}
		s.pruneGroups()
	}
}

func (s *sink) pruneGroups() {
	for key, message := range s.messages {
		if time.Since(message.written) > groupTTL {
			delete(s.messages, key)
		}
	}
}

// post は webhook へ新規投稿し、投稿されたメッセージIDを返します。
// wait=true を付けないとDiscordは 204 を返すだけでIDを教えてくれず、
// 続報での書き換えができません。
func (s *sink) post(ctx context.Context, payload discordWebhookPayload) (string, error) {
	url := s.route.WebhookURL + "?wait=true"
	body, err := s.send(ctx, http.MethodPost, url, payload)
	if err != nil {
		return "", err
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		// 投稿自体は成功しているので、IDが読めなくてもエラーにはしません。
		// 続報が書き換えではなく新規投稿になるだけです。
		log.Printf("route=%s could not read message id: %v", s.route.Name, err)
		return "", nil
	}
	return created.ID, nil
}

func (s *sink) edit(ctx context.Context, messageID string, payload discordWebhookPayload) error {
	url := fmt.Sprintf("%s/messages/%s", s.route.WebhookURL, messageID)
	_, err := s.send(ctx, http.MethodPatch, url, payload)
	return err
}

// send はレート制限とサーバエラーを吸収しつつ1リクエストを投げます。
func (s *sink) send(ctx context.Context, method, url string, payload discordWebhookPayload) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	for attempt := range maxSendRetries {
		responseBody, retryAfter, err := s.attempt(ctx, method, url, body)
		if err == nil {
			return responseBody, nil
		}
		lastErr = err

		// ペイロードやURLが悪い場合(429以外の4xx)は、何度投げても同じ答えが
		// 返るだけなので即座に諦めます。
		var status statusError
		if errors.As(err, &status) && !status.retryable() {
			return nil, err
		}
		if attempt == maxSendRetries-1 {
			break
		}

		// Discordが待つべき時間を教えてくれたならそれに従います。自前のバックオフで
		// 短く再試行すると制限がさらに延びるので、こちらを優先します。
		delay := retryAfter
		if delay <= 0 {
			delay = time.Duration(math.Pow(2, float64(attempt))) * time.Second
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", maxSendRetries, lastErr)
}

func (s *sink) attempt(ctx context.Context, method, url string, body []byte) ([]byte, time.Duration, error) {
	requestCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	// 本文は上限付きで読み切ります。読み切らないとkeep-aliveの接続が
	// 再利用されず、次の送信で毎回TLSからやり直しになります。
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, 0, readErr
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, retryAfterDuration(resp, responseBody), statusError{code: resp.StatusCode, body: string(responseBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, statusError{code: resp.StatusCode, body: string(responseBody)}
	}
	return responseBody, 0, nil
}

// retryAfterDuration は 429 応答から待ち時間を読みます。Discordは本文のJSONと
// Retry-After ヘッダの両方で返しますが、片方しか無い場合もあるので両方見ます。
func retryAfterDuration(resp *http.Response, body []byte) time.Duration {
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter > 0 {
		return time.Duration(payload.RetryAfter * float64(time.Second))
	}
	if header := resp.Header.Get("Retry-After"); header != "" {
		if seconds, err := strconv.ParseFloat(header, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	// どちらも読めない場合の保険。短すぎると制限を延ばすだけなので長めに。
	return 5 * time.Second
}

type statusError struct {
	code int
	body string
}

func (e statusError) Error() string {
	return fmt.Sprintf("discord returned %d: %s", e.code, truncate(e.body, 500))
}

// retryable は再試行する価値があるかどうか。400番台のうち 429 以外は
// ペイロードかURLが間違っているので、何度投げても結果は変わりません。
func (e statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
