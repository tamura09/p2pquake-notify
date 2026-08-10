package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// quakeAt は指定時刻の地震情報(551)を1件組み立てます。補完が通知するかどうかは
// イベントの新しさで決まるので、固定日付の文字列では試験になりません。
func quakeAt(at time.Time) string {
	stamp := at.In(testZone()).Format("2006/01/02 15:04:05.000")
	issued := at.In(testZone()).Format("2006/01/02 15:04:05")
	return fmt.Sprintf(`{
		"code": 551,
		"time": %q,
		"earthquake": {
			"time": %q,
			"hypocenter": {"name": "東京湾", "depth": 40, "magnitude": 4.2},
			"maxScale": 30,
			"domesticTsunami": "None"
		},
		"issue": {"source": "気象庁", "time": %q, "type": "DetailScale", "correct": "None"},
		"points": [{"pref": "東京都", "addr": "千代田区", "isArea": false, "scale": 30}]
	}`, stamp, issued, issued)
}

// historyRequests は補完が上流へ投げたクエリの記録。
type historyRequests struct {
	mu    sync.Mutex
	codes []string
}

func (h *historyRequests) record(code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codes = append(h.codes, code)
}

func (h *historyRequests) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.codes...)
}

// testIngest は code ごとに応答を出し分ける履歴サーバを立てます。上流は
// 1リクエストにつき1つの code しか受け付けないので、それを再現しないと
// 「code を指定せずに叩いてピア分布しか返ってこない」という本番の壊れ方が
// テストをすり抜けます。
func testIngest(t *testing.T, historyBody string) (*ingest, *sink, *historyRequests) {
	t.Helper()

	wantCode := ""
	var messages []json.RawMessage
	if err := json.Unmarshal([]byte(historyBody), &messages); err == nil && len(messages) > 0 {
		var head envelope
		if err := json.Unmarshal(messages[0], &head); err == nil {
			wantCode = strconv.Itoa(head.Code)
		}
	}

	requests := &historyRequests{}
	history := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("codes")
		requests.record(code)

		w.Header().Set("Content-Type", "application/json")
		if code != "" && code == wantCode {
			_, _ = fmt.Fprint(w, historyBody)
			return
		}
		_, _ = fmt.Fprint(w, "[]")
	}))
	t.Cleanup(history.Close)

	// webhookには到達できないアドレスを渡します。この一連のテストが見たいのは
	// 「キューに載ったかどうか」なので、送信そのものは行いません。
	s := testSink(t, "http://127.0.0.1:1/webhook")

	return &ingest{
		cfg: config{
			HistoryURL:    history.URL,
			BackfillLimit: 20,
			Zone:          testZone(),
			StaleAfter:    time.Minute,
		},
		apiClient: &http.Client{Timeout: 5 * time.Second},
		dedup:     newDedupCache(time.Hour, 100),
		sinks:     []*sink{s},
	}, s, requests
}

// 起動直後の履歴読み込みで通知してはいけません。ここで通知すると、
// タスクが再配置されるたびに直近の地震がまとめて再通知されます。
func TestStartupBackfillPrimesWithoutNotifying(t *testing.T) {
	receiver, s, _ := testIngest(t, "["+quakeTokyo+"]")

	if err := receiver.backfill(context.Background(), false); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("startup backfill queued %d events, want 0", got)
	}

	// そのうえで、同じイベントがWebSocketから届いても重複として落ちること。
	receiver.handle([]byte(quakeTokyo))
	if got := len(s.queue); got != 0 {
		t.Errorf("an event already seen in history was queued %d time(s), want 0", got)
	}
}

// 再接続時の履歴読み込みは、切断中に起きたイベントを拾って通知します。
func TestReconnectBackfillNotifiesUnseenEvents(t *testing.T) {
	receiver, s, _ := testIngest(t, "["+quakeAt(time.Now().Add(-2*time.Minute))+"]")

	if err := receiver.backfill(context.Background(), true); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := len(s.queue); got != 1 {
		t.Errorf("reconnect backfill queued %d events, want 1", got)
	}

	// 二度目は重複として落ちます。切断が続いても同じ通知は繰り返しません。
	if err := receiver.backfill(context.Background(), true); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if got := len(s.queue); got != 1 {
		t.Errorf("queue holds %d events after a repeat backfill, want 1", got)
	}
}

// 2026年8月10日の事故への回帰テスト。
//
// 上流が8時間ほどで接続を切ってくるので再接続の補完が走り、7月28日の
// 緊急地震速報と津波注意報が「今起きたこと」として一斉に届きました。履歴の
// limit は件数で効くため、めったに発表されないcodeでは20件が数週間前まで遡ります。
// 重複排除の鍵は起動時に登録済みでしたが、dedupTTLを過ぎて消えていました。
func TestBackfillNeverNotifiesStaleEvents(t *testing.T) {
	old := quakeAt(time.Now().Add(-14 * 24 * time.Hour)) // 2週間前
	receiver, s, _ := testIngest(t, "["+old+"]")

	if err := receiver.backfill(context.Background(), true); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("queued %d two-week-old events, want 0", got)
	}
	// 通知しないだけで、鍵は登録します。この後WebSocketで同じものが
	// 流れてきた時に二重にならないためです。
	if receiver.dedup.len() == 0 {
		t.Error("the stale event was not registered for deduplication")
	}
}

// 地平線の境目。内側は通知し、外側は通知しません。
func TestBackfillHorizonBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		want int
	}{
		{"just inside the horizon", backfillHorizon - time.Minute, 1},
		{"just outside the horizon", backfillHorizon + time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receiver, s, _ := testIngest(t, "["+quakeAt(time.Now().Add(-tc.age))+"]")
			if err := receiver.backfill(context.Background(), true); err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if got := len(s.queue); got != tc.want {
				t.Errorf("queued %d events, want %d", got, tc.want)
			}
		})
	}
}

// 時刻が読めないイベントは通知しません。上流が表記を変えた時に、日付の
// 分からない緊急地震速報を送るより送らないほうが安全です。
func TestBackfillSkipsEventsWithUnreadableTime(t *testing.T) {
	broken := `{"code":551,"time":"not a timestamp","earthquake":{"maxScale":30,"hypocenter":{"name":"東京湾"}},"issue":{"source":"気象庁","type":"DetailScale","time":"not a timestamp"}}`
	receiver, s, _ := testIngest(t, "["+broken+"]")

	if err := receiver.backfill(context.Background(), true); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("queued %d undateable events, want 0", got)
	}
}

// 重複排除の保持期間は補完の地平線より長くなければなりません。短いと、
// 地平線の内側にあるイベントの鍵が先に消え、再接続のたびに再通知されます。
// この関係が壊れると事故が再発するので、定数の側で縛ります。
func TestDedupTTLOutlivesBackfillHorizon(t *testing.T) {
	if dedupTTL <= backfillHorizon {
		t.Fatalf("dedupTTL (%s) must be longer than backfillHorizon (%s); events inside the horizon would be re-notified once their key expires",
			dedupTTL, backfillHorizon)
	}
}

// 補完は必ず code を指定して問い合わせます。指定しないと上流は絶えず記録されて
// いるピア分布(555)で limit の枠を埋め尽くし、地震も津波も1件も返しません。
// 補完は成功したように見えて常に空振りし、切断中のイベントは永久に失われます。
func TestBackfillQueriesEachCodeSeparately(t *testing.T) {
	receiver, _, requests := testIngest(t, "["+quakeTokyo+"]")

	if err := receiver.backfill(context.Background(), false); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	seen := requests.seen()
	for _, code := range seen {
		if code == "" {
			t.Fatalf("backfill made a request with no codes filter; requests were %v", seen)
		}
	}

	// 上流は1リクエストにつき1つの code しか受け付けないので、通知対象の
	// 3つそれぞれに1本ずつ投げる必要があります。
	want := []string{"551", "552", "556"}
	if len(seen) != len(want) {
		t.Fatalf("made %d requests (%v), want one per code %v", len(seen), seen, want)
	}
	for _, code := range want {
		if !slices.Contains(seen, code) {
			t.Errorf("no history request for code %s; requests were %v", code, seen)
		}
	}
}

// 1つの code が読めなくても、他の code の補完は続けます。津波の履歴が
// 引けないことを理由に緊急地震速報の取りこぼしまで諦める理由はありません。
func TestBackfillSurvivesOneFailingCode(t *testing.T) {
	history := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("codes") {
		case "551":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, "["+quakeAt(time.Now().Add(-2*time.Minute))+"]")
		case "552":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, "[]")
		}
	}))
	t.Cleanup(history.Close)

	s := testSink(t, "http://127.0.0.1:1/webhook")
	receiver := &ingest{
		cfg:       config{HistoryURL: history.URL, BackfillLimit: 20, Zone: testZone()},
		apiClient: &http.Client{Timeout: 5 * time.Second},
		dedup:     newDedupCache(time.Hour, 100),
		sinks:     []*sink{s},
	}

	if err := receiver.backfill(context.Background(), true); err != nil {
		t.Fatalf("backfill reported failure despite one code succeeding: %v", err)
	}
	if got := len(s.queue); got != 1 {
		t.Errorf("queued %d events, want 1 from the code that did respond", got)
	}
}

func TestDispatchDropsDuplicates(t *testing.T) {
	receiver, s, _ := testIngest(t, "[]")

	receiver.handle([]byte(quakeTokyo))
	receiver.handle([]byte(quakeTokyo))

	if got := len(s.queue); got != 1 {
		t.Errorf("queued %d events for the same message, want 1", got)
	}
}

// P2Pネットワーク内部の様子を伝えるだけのcodeは、受信はしても通知しません。
// 実際にこれが本番のdevチャンネルを埋め尽くしました。
func TestInternalNetworkCodesAreNeverNotified(t *testing.T) {
	receiver, s, _ := testIngest(t, "[]")

	for _, raw := range []string{userquakeMessage, userquakeEvaluationMessage, areaPeersMessage} {
		receiver.handle([]byte(raw))
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("queued %d internal messages, want 0", got)
	}
}

// ピア分布は通知しませんが、受信そのものは接続が生きている印です。
// 重複排除の枠を食い潰さないよう鍵は覚えません。
func TestAreaPeersIsNeverNotifiedAndNeverDeduplicated(t *testing.T) {
	receiver, s, _ := testIngest(t, "[]")

	receiver.handle([]byte(areaPeersMessage))
	receiver.handle([]byte(areaPeersMessage))

	if got := len(s.queue); got != 0 {
		t.Errorf("queued %d peer messages, want 0 (they carry nothing a human can read)", got)
	}
	// 鍵を覚えてしまうと、絶え間なく届くピア分布で重複排除のキャッシュが
	// 埋まり、本来まとめたい地震の再配信を取りこぼします。
	if receiver.dedup.len() != 0 {
		t.Errorf("dedup cache holds %d entries, want 0", receiver.dedup.len())
	}
}

func TestHandleSurvivesMalformedJSON(t *testing.T) {
	receiver, s, _ := testIngest(t, "[]")

	before := decodeFailures.Load()
	receiver.handle([]byte(`{"code": `))

	if decodeFailures.Load() != before+1 {
		t.Error("a malformed message was not counted as a decode failure")
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("a malformed message queued %d events, want 0", got)
	}
}

// バックオフは上限で頭打ちになり、かつジッタで散ること。上流が落ちて全クライアントが
// 同時に切断されたとき、揃って同じ秒数後に再接続すると復旧しかけた上流をまた倒します。
func TestBackoffIsBoundedAndJittered(t *testing.T) {
	if got := nextBackoff(maxBackoff); got > maxBackoff {
		t.Errorf("nextBackoff(%s) = %s, over the cap", maxBackoff, got)
	}

	seen := map[time.Duration]bool{}
	for range 20 {
		delay := nextBackoff(10 * time.Second)
		if delay <= 0 || delay > maxBackoff {
			t.Fatalf("nextBackoff returned %s, outside (0, %s]", delay, maxBackoff)
		}
		seen[delay] = true
	}
	if len(seen) == 1 {
		t.Error("nextBackoff returned a constant; reconnects would be synchronised across clients")
	}
}
