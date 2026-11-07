package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultLinePushEndpoint = "https://api.line.me/v2/bot/message/push"

	// lineTextLimit はテキストメッセージの上限(文字数)。超えると 400 で丸ごと弾かれます。
	lineTextLimit = 5000

	// lineRetryDelay は最初の再試行までの待ち時間。以後は倍々にします。
	lineRetryDelay = time.Second
)

// lineGroupPattern はグループID。先頭の C に16進32桁が続く形です。
var lineGroupPattern = regexp.MustCompile(`^C[0-9a-f]{32}$`)

// lineRoute は LINE へ流すイベントの条件。Discord の alert ルートと同じにしています:
// 緊急地震速報(556)と津波予報(552)、それに速報を流した地震の地震情報(551)。
//
// alert と違って訓練報・テスト配信も通しますが、届け先はテスト用グループだけです
// (lineSink.recipients)。本番のグループには決して流しません。
func lineRoute() route {
	return route{
		Name:          "line",
		Codes:         codeSet(codeEEW, codeJMATsunami),
		MinScale:      scaleUnknown,
		IncludeTest:   true,
		QuakeAfterEEW: true,
	}
}

// lineCredentials は line-charge-reminder と同じ公式アカウントで送るための設定。
// 送り先も同じで、公式アカウントが参加している全グループが本番、
// 固定のテスト用グループがテストです。
type lineCredentials struct {
	Token string

	// TestGroup はテスト用グループのID。設定されていなければ空で、
	// 訓練報・テスト配信はどこにも送りません。
	TestGroup string

	// TestOnly なら本番のイベントもテスト用グループだけに送ります。
	// サンドボックスへ繋いでいる時(ダミーの地震が流れ続ける)に使います。
	TestOnly bool
}

// lineGroupDirectory は公式アカウントが参加しているグループを引く口。
// 本番は line-charge-reminder の Webhook が参加・退出を記録している DynamoDB の
// テーブルです。テストで差し替えます。
type lineGroupDirectory interface {
	activeGroups(ctx context.Context) ([]string, error)
	isActive(ctx context.Context, groupID string) (bool, error)
}

// lineSink は Discord の sink と同じく独立した goroutine とキューを持ちます。
// LINE が詰まっても Discord への通知は遅れません。
type lineSink struct {
	route       route
	credentials lineCredentials
	endpoint    string
	client      *http.Client
	zone        *time.Location
	queue       chan *event
	retryDelay  time.Duration

	groups lineGroupDirectory

	// lastGroups は最後に読めた参加中のグループ。地震の最中にテーブルが
	// 読めなかった時は、送らないよりはこれに送ります。
	lastGroups []string

	// eews は緊急地震速報のどの報を送るかを決めます(throttle.go)。
	eews eewThrottle

	// quakes は GroupKey -> 地震情報(551)の積み上げと送信状況。
	// 送信goroutineだけが触るのでロックは要りません。
	quakes map[string]lineQuake
}

// lineQuake は1つの地震についての地震情報の状態。
type lineQuake struct {
	// merged は届いた報をすべて積み上げたもの。送らなかった報も積みます。
	// 「震源に関する情報」は送りませんが、その震源は次に送る報に載せたいためです。
	merged jmaQuake

	sent       bool // いずれかの報を送ったか
	sentDetail bool // 各地の震度に関する情報を送ったか
	sentScale  int  // 送った中で最大の震度

	written time.Time
}

func newLineSink(credentials lineCredentials, groups lineGroupDirectory, client *http.Client, zone *time.Location) *lineSink {
	return &lineSink{
		route:       lineRoute(),
		credentials: credentials,
		groups:      groups,
		endpoint:    defaultLinePushEndpoint,
		client:      client,
		zone:        zone,
		queue:       make(chan *event, queueDepth),
		retryDelay:  lineRetryDelay,
		eews:        newEEWThrottle(),
		quakes:      map[string]lineQuake{},
	}
}

// offer は受信ループから呼ばれるので決してブロックしません。満杯なら最古の1件を
// 捨てます(sink.offer と同じ理由)。
func (l *lineSink) offer(e *event) {
	for {
		select {
		case l.queue <- e:
			return
		default:
		}
		select {
		case dropped := <-l.queue:
			log.Printf("route=%s queue full, dropped code=%d", l.route.Name, dropped.Code)
		default:
		}
	}
}

func (l *lineSink) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-l.queue:
			l.deliver(ctx, e)
		}
	}
}

func (l *lineSink) deliver(ctx context.Context, e *event) {
	text, ok := l.compose(e)
	if !ok {
		return
	}
	recipients, err := l.recipients(ctx, e)
	if err != nil {
		log.Printf("route=%s no recipients code=%d test=%t: %v", l.route.Name, e.Code, e.Test, err)
		lineFailures.Add(1)
		return
	}
	if len(recipients) == 0 {
		log.Printf("route=%s no recipients, nothing sent code=%d test=%t testOnly=%t", l.route.Name, e.Code, e.Test, l.credentials.TestOnly)
		return
	}
	delivered, err := l.send(ctx, recipients, text)
	if err != nil {
		log.Printf("route=%s post failed code=%d: %v", l.route.Name, e.Code, err)
		lineFailures.Add(1)
	}
	if delivered == 0 {
		return
	}
	lineSent.Add(1)
	// 1つでも届いたら送った扱いにします。全員に届くまで送った扱いにしないと、
	// 退出を取りこぼしたグループが1つあるだけで(毎回 4xx になります)、
	// 緊急地震速報の続報が毎回「最初の報」と判定され、届いているグループにも
	// 全報が流れます。失敗したグループは、その次に送る報から受け取ります。
	l.remember(e)
}

// recipients はこのイベントの送り先を返します。
//
// 訓練報・テスト配信はテスト用グループだけへ送ります。本番のグループへ訓練報が
// 飛ぶと、いざという時に誰も信じなくなります。テスト用グループが設定されて
// いない・退出済みなら、本番へ切り替えずにエラーにします(line-charge-reminder の
// テスト配信と同じ扱い)。
func (l *lineSink) recipients(ctx context.Context, e *event) ([]string, error) {
	if e.Test || l.credentials.TestOnly {
		group := l.credentials.TestGroup
		if group == "" {
			// テスト配信を無効にしている正当な設定なので、失敗としては数えません
			// (数えると訓練報のたびに p2pquake_line_failures_total が増え、本番の
			// 送信失敗と見分けがつかなくなります)。本番へ切り替えずに送らずに終わります。
			return nil, nil
		}
		active, err := l.groups.isActive(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("look up test group: %w", err)
		}
		if !active {
			return nil, errors.New("test group is not active")
		}
		return []string{group}, nil
	}
	return l.activeGroups(ctx)
}

// activeGroups は参加中の全グループを返します。テーブルが読めなかった時は
// 最後に読めた一覧を使います。
func (l *lineSink) activeGroups(ctx context.Context) ([]string, error) {
	groups, err := l.groups.activeGroups(ctx)
	if err == nil {
		l.lastGroups = groups
		return groups, nil
	}
	if l.lastGroups == nil {
		return nil, fmt.Errorf("list active groups: %w", err)
	}
	log.Printf("route=%s list active groups failed, using the last known %d: %v", l.route.Name, len(l.lastGroups), err)
	return l.lastGroups, nil
}

// compose は送るべきイベントならメッセージ本文を返します。
//
// LINE は送ったものを書き換えられず、無料枠の月間送信数も限られています
// (グループ宛ては人数ぶん数えられます)。Discord の alert と同じ地震を扱いますが、
// 1つの地震で何通も届かないよう、緊急地震速報と地震情報は送る報を絞ります。
func (l *lineSink) compose(e *event) (string, bool) {
	switch payload := e.Payload.(type) {
	case eew:
		if !l.eews.allow(e, payload) {
			return "", false
		}
		return lineText(renderEEW(payload, l.zone)), true
	case jmaTsunami:
		// 津波予報はめったに出ず、1報ごとに状況が変わるので全報送ります。
		return lineText(renderTsunami(payload, l.zone)), true
	case jmaQuake:
		merged, ok := l.composeQuake(e, payload)
		if !ok {
			return "", false
		}
		return lineText(renderQuake(merged, l.zone)), true
	default:
		return "", false
	}
}

// composeQuake は地震情報(551)を送るかどうかを決め、送るなら前の報と積み上げた
// 内容を返します。送るのは
//
//   - その地震の最初の報(多くは震度速報。実際に揺れたかが最初に分かる)
//   - 最初の「各地の震度に関する情報」(どこがどれだけ揺れたかの全体像)
//   - 最大震度がそれまでに送ったものより上がった報
//
// で、1つの地震でふつう2通です。震源に関する情報は積み上げて次に送る報に載せ、
// 単独では送りません。ただし、それがその地震で最初に届いた報だった場合(または
// 先に届いた報の送信が失敗していた場合)は「最初の報」として送ります。何も
// 届いていないより、震源だけでも先に知らせるほうがよいためです。
func (l *lineSink) composeQuake(e *event, message jmaQuake) (jmaQuake, bool) {
	l.pruneQuakes()

	state, known := l.quakes[e.GroupKey]
	if e.GroupKey == "" {
		known = false
	}

	merged := message
	if known {
		merged = mergeQuakeReports(state.merged, message)
	}
	if e.GroupKey != "" {
		state.merged = merged
		state.written = time.Now()
		l.quakes[e.GroupKey] = state
	}

	switch {
	case !state.sent:
		return merged, true
	case message.Issue.Type == "DetailScale" && !state.sentDetail:
		return merged, true
	case merged.Earthquake.MaxScale > state.sentScale:
		return merged, true
	}
	return jmaQuake{}, false
}

// remember は LINE が受け付けた報を覚えます。送信前に覚えると、送信に失敗した
// 報の続きまで「送信済み」として抑止され、一度も届かないまま終わります。
func (l *lineSink) remember(e *event) {
	switch payload := e.Payload.(type) {
	case eew:
		l.eews.remember(e)
	case jmaQuake:
		state, known := l.quakes[e.GroupKey]
		if e.GroupKey == "" || !known {
			return
		}
		state.sent = true
		if payload.Issue.Type == "DetailScale" {
			state.sentDetail = true
		}
		state.sentScale = max(state.sentScale, state.merged.Earthquake.MaxScale)
		state.written = time.Now()
		l.quakes[e.GroupKey] = state
	}
}

func (l *lineSink) pruneQuakes() {
	for key, state := range l.quakes {
		if time.Since(state.written) > groupTTL {
			delete(l.quakes, key)
		}
	}
}

// lineText は Discord 用の描画を LINE のテキストに直します。地震の読み方
// (震度のまとめ方、予報区の並べ方)を何か所にも持たないため、Pushover と同じく
// Discord の描画を平文にしたものを使います。
//
// 複数行のフィールド(各地の震度の「- 岩手県 / 宮古市」)は見出しを1行に置きます。
// 「震度5強: - 岩手県」と続けると、どこまでが見出しか読み取れません。
func lineText(payload discordWebhookPayload) string {
	if len(payload.Embeds) == 0 {
		return truncateRunes(strings.TrimSpace(payload.Content), lineTextLimit)
	}
	embed := payload.Embeds[0]

	lines := []string{}
	if embed.Title != "" {
		lines = append(lines, "【"+embed.Title+"】")
	}
	if description := strings.TrimSpace(strings.ReplaceAll(embed.Description, "**", "")); description != "" {
		lines = append(lines, description)
	}
	for _, field := range embed.Fields {
		if strings.Contains(field.Value, "\n") {
			lines = append(lines, "", field.Name, field.Value)
			continue
		}
		lines = append(lines, field.Name+": "+field.Value)
	}
	return truncateRunes(strings.Join(lines, "\n"), lineTextLimit)
}

// send は送り先ごとにプッシュメッセージを送り、届いた数を返します。
//
// 送り先は並行して送ります。1つずつ送ると、最後のグループには前のグループの
// 往復時間(1件0.2秒ほど)がすべて積み上がって届きます。1つの送り先で失敗しても
// 残りへの送信は続けます。
func (l *lineSink) send(ctx context.Context, recipients []string, text string) (int, error) {
	results := make([]error, len(recipients))
	var wg sync.WaitGroup
	for i, recipient := range recipients {
		wg.Go(func() {
			results[i] = l.push(ctx, recipient, text)
		})
	}
	wg.Wait()

	var failures []error
	for _, err := range results {
		if err != nil {
			failures = append(failures, err)
		}
	}
	delivered := len(recipients) - len(failures)
	if len(failures) > 0 {
		return delivered, fmt.Errorf("%d of %d recipients failed: %w", len(failures), len(recipients), errors.Join(failures...))
	}
	return delivered, nil
}

// push は1つの送り先へ送ります。再試行するのはサーバエラーと通信エラーだけです。
// 4xx は何度送っても同じで、429 も月間の送信上限に達したという意味のことが
// 多く、月が替わるまで通りません。
//
// 再試行では同じ X-Line-Retry-Key を使います。タイムアウトで応答を受け取れなかった
// だけで LINE 側は受け付けていた場合、2回目は 409 で弾かれ、二重に届きません。
func (l *lineSink) push(ctx context.Context, recipient, text string) error {
	body, err := json.Marshal(map[string]any{
		"to":       recipient,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	retryKey := newRetryKey()

	var lastErr error
	for attempt := range maxSendRetries {
		err := l.attempt(ctx, body, retryKey)
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
		if err := sleep(ctx, l.retryDelay*time.Duration(math.Pow(2, float64(attempt)))); err != nil {
			return err
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxSendRetries, lastErr)
}

func (l *lineSink) attempt(ctx context.Context, body []byte, retryKey string) error {
	requestCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, l.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.credentials.Token)
	req.Header.Set("X-Line-Retry-Key", retryKey)

	resp, err := l.client.Do(req)
	if err != nil {
		// url.Error はリクエストURLを含みますが、ヘッダ(トークン)は含みません。
		return err
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return readErr
	}

	// 同じ再試行キーの要求をすでに受け付けていた、という意味。前の試行は
	// 届いているので成功として扱います。
	if resp.StatusCode == http.StatusConflict && resp.Header.Get("X-Line-Accepted-Request-Id") != "" {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError{service: "line", code: resp.StatusCode, body: lineErrorMessage(responseBody)}
	}
	return nil
}

// lineErrorMessage は応答の message だけを取り出します。
func lineErrorMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Message == "" {
		return string(body)
	}
	return payload.Message
}

// newRetryKey は X-Line-Retry-Key に使う UUID v4 を作ります。
func newRetryKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
