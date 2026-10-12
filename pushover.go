package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPushoverEndpoint = "https://api.pushover.net/1/messages.json"

	// Pushover の上限は文字数で、超えると 400 で丸ごと弾かれます。
	pushoverTitleLimit   = 250
	pushoverMessageLimit = 1024

	// Pushover の優先度。2(緊急)は確認するまで鳴り続ける代わりに retry/expire の
	// 指定が要り、地震の続報のたびに積み上がるので使いません。1 はおやすみモードを
	// 無視して鳴らすので、本当に急ぐものだけに付けます。
	pushoverPriorityNormal = 0
	pushoverPriorityHigh   = 1

	// pushoverRetryDelay は最初の再試行までの待ち時間。Pushover は5秒未満での
	// 再送をやめるよう求めています。以後は倍々にします。
	pushoverRetryDelay = 5 * time.Second
)

// pushoverKeyPattern は Pushover のアプリトークンとユーザキーの形。どちらも
// 英数字30桁です。SSMにプレースホルダが入ったままの時に、ここで弾きます。
var pushoverKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)

// pushoverRoute は Pushover へ流すイベントの条件。緊急地震速報と津波予報だけで、
// 地震情報(551)は流しません。551 は「もう揺れた後」の記録で、スマホを鳴らして
// 急がせる理由が無いためです。訓練報も流しません。
func pushoverRoute() route {
	return route{
		Name:     "pushover",
		Codes:    codeSet(codeEEW, codeJMATsunami),
		MinScale: scaleUnknown,
	}
}

type pushoverCredentials struct {
	Token string
	User  string
}

// pushoverSink は Discord の sink と同じく独立した goroutine とキューを持ちます。
// Pushover が詰まっても Discord への通知は遅れません。
type pushoverSink struct {
	route       route
	credentials pushoverCredentials
	endpoint    string
	client      *http.Client
	zone        *time.Location
	queue       chan *event
	retryDelay  time.Duration

	// eewPeaks は GroupKey -> その地震について送った最大の予想震度。
	// 送信goroutineだけが触るのでロックは要りません。
	eewPeaks map[string]eewPeak
}

type eewPeak struct {
	scale   int
	written time.Time
}

type pushoverMessage struct {
	Title    string
	Message  string
	Priority int
}

func newPushoverSink(credentials pushoverCredentials, client *http.Client, zone *time.Location) *pushoverSink {
	return &pushoverSink{
		route:       pushoverRoute(),
		credentials: credentials,
		endpoint:    defaultPushoverEndpoint,
		client:      client,
		zone:        zone,
		queue:       make(chan *event, queueDepth),
		retryDelay:  pushoverRetryDelay,
		eewPeaks:    map[string]eewPeak{},
	}
}

// offer は受信ループから呼ばれるので決してブロックしません。満杯なら最古の1件を
// 捨てます(sink.offer と同じ理由)。
func (p *pushoverSink) offer(e *event) {
	for {
		select {
		case p.queue <- e:
			return
		default:
		}
		select {
		case dropped := <-p.queue:
			log.Printf("route=%s queue full, dropped code=%d", p.route.Name, dropped.Code)
		default:
		}
	}
}

func (p *pushoverSink) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-p.queue:
			p.deliver(ctx, e)
		}
	}
}

func (p *pushoverSink) deliver(ctx context.Context, e *event) {
	message, ok := p.compose(e)
	if !ok {
		return
	}
	if err := p.send(ctx, message); err != nil {
		log.Printf("route=%s post failed code=%d: %v", p.route.Name, e.Code, err)
		pushoverFailures.Add(1)
		return
	}
	pushoverSent.Add(1)
	p.rememberEEW(e)
}

// compose は送るべきイベントなら Pushover のメッセージを返します。
//
// Pushover は送ったものを書き換えられません。緊急地震速報は1つの地震で10報以上
// 届くので、全報を送るとスマホが鳴り続けます。そこで送るのは
//
//   - その地震の最初の報
//   - 予想最大震度が、それまでに送ったものより上がった報
//   - 送ったことのある地震の取消報
//
// に絞ります。続報で予想地域だけが広がった場合は送りません(Discord 側は書き換えで追えます)。
func (p *pushoverSink) compose(e *event) (pushoverMessage, bool) {
	switch payload := e.Payload.(type) {
	case eew:
		return p.composeEEW(e, payload)
	case jmaTsunami:
		return composeTsunami(payload, p.zone), true
	default:
		return pushoverMessage{}, false
	}
}

func (p *pushoverSink) composeEEW(e *event, message eew) (pushoverMessage, bool) {
	p.pruneEEWPeaks()

	peak, sent := p.eewPeaks[e.GroupKey]
	if e.GroupKey == "" {
		sent = false
	}

	priority := pushoverPriorityHigh
	switch {
	case message.Cancelled:
		if !sent {
			return pushoverMessage{}, false
		}
		priority = pushoverPriorityNormal
	case sent && e.MaxScale <= peak.scale:
		return pushoverMessage{}, false
	}

	composed := fromDiscordPayload(renderEEW(message, p.zone))
	composed.Priority = priority
	return composed, true
}

// rememberEEW は Pushover が受け付けた緊急地震速報の予想震度を覚えます。送信前に
// 覚えると、第1報の送信に失敗した地震で、同じ震度の続報まで「送信済み」として
// 抑止され、スマホが一度も鳴らないまま終わります。
func (p *pushoverSink) rememberEEW(e *event) {
	message, ok := e.Payload.(eew)
	if !ok || message.Cancelled || e.GroupKey == "" {
		return
	}
	scale := e.MaxScale
	if peak, sent := p.eewPeaks[e.GroupKey]; sent {
		scale = max(scale, peak.scale)
	}
	p.eewPeaks[e.GroupKey] = eewPeak{scale: scale, written: time.Now()}
}

// composeTsunami は津波警報以上だけを優先度高にします。注意報と解除まで
// おやすみモードを破って鳴らすと、本当に逃げるべき時に慣れで無視されます。
func composeTsunami(message jmaTsunami, zone *time.Location) pushoverMessage {
	composed := fromDiscordPayload(renderTsunami(message, zone))
	composed.Priority = pushoverPriorityNormal
	if message.Cancelled {
		return composed
	}

	highest := 0
	for _, area := range message.Areas {
		highest = max(highest, tsunamiGradeRank(tsunamiGradeLabel(area.Grade)))
	}
	if highest >= tsunamiGradeRank("津波警報") {
		composed.Priority = pushoverPriorityHigh
	}
	if highest > 0 {
		// 「津波予報」より等級のほうが一目で分かります。
		for label, rank := range tsunamiGradeOrder {
			if rank == highest {
				composed.Title = label
			}
		}
	}
	return composed
}

// fromDiscordPayload は Discord 用の描画を平文に直します。地震の読み方
// (震度のまとめ方、予報区の並べ方)を2か所で持たないためです。
func fromDiscordPayload(payload discordWebhookPayload) pushoverMessage {
	if len(payload.Embeds) == 0 {
		return pushoverMessage{Title: "P2P地震情報", Message: payload.Content}
	}
	embed := payload.Embeds[0]

	lines := []string{}
	if description := strings.TrimSpace(strings.ReplaceAll(embed.Description, "**", "")); description != "" {
		lines = append(lines, description)
	}
	for _, field := range embed.Fields {
		lines = append(lines, field.Name+": "+field.Value)
	}

	return pushoverMessage{
		Title:   embed.Title,
		Message: strings.Join(lines, "\n"),
	}
}

func (p *pushoverSink) pruneEEWPeaks() {
	for key, peak := range p.eewPeaks {
		if time.Since(peak.written) > groupTTL {
			delete(p.eewPeaks, key)
		}
	}
}

// send はサーバエラーと通信エラーだけを再試行します。4xx は「何度送っても同じ」と
// Pushover が明言していて、再送し続けるとアプリごと止められます。429 も例外では
// なく、月間の送信上限に達したという意味なので月が替わるまで通りません。
func (p *pushoverSink) send(ctx context.Context, message pushoverMessage) error {
	form := url.Values{}
	form.Set("token", p.credentials.Token)
	form.Set("user", p.credentials.User)
	form.Set("title", truncateRunes(message.Title, pushoverTitleLimit))
	form.Set("message", truncateRunes(firstNonEmpty(message.Message, message.Title), pushoverMessageLimit))
	form.Set("priority", strconv.Itoa(message.Priority))
	body := form.Encode()

	var lastErr error
	for attempt := range maxSendRetries {
		err := p.attempt(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err

		var status statusError
		if errors.As(err, &status) && status.code < http.StatusInternalServerError {
			return err
		}
		if attempt == maxSendRetries-1 {
			break
		}
		if err := sleep(ctx, p.retryDelay*time.Duration(math.Pow(2, float64(attempt)))); err != nil {
			return err
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxSendRetries, lastErr)
}

func (p *pushoverSink) attempt(ctx context.Context, body string) error {
	requestCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		// url.Error はリクエストURLを含みますが、本文(トークン)は含みません。
		return err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError{service: "pushover", code: resp.StatusCode, body: pushoverErrors(responseBody)}
	}
	return nil
}

// pushoverErrors は応答の errors だけを取り出します。応答には送ったトークンが
// 含まれないものの、ログに残すのは原因の分かる部分だけで足ります。
func pushoverErrors(body []byte) string {
	var payload struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Errors) == 0 {
		return string(body)
	}
	return strings.Join(payload.Errors, "; ")
}

// truncateRunes は文字数で切ります。Pushover の上限はバイトではなく文字数なので、
// バイトで切る truncate を使うと日本語では上限の3分の1しか使えません。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
