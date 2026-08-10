package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	method string
	path   string
	query  string
	body   discordWebhookPayload
}

type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(attempt int, w http.ResponseWriter)
}

func newRecordingServer(t *testing.T, respond func(attempt int, w http.ResponseWriter)) *recordingServer {
	t.Helper()
	server := &recordingServer{respond: respond}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload discordWebhookPayload
		_ = json.Unmarshal(raw, &payload)

		server.mu.Lock()
		attempt := len(server.requests)
		server.requests = append(server.requests, recordedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   payload,
		})
		server.mu.Unlock()

		server.respond(attempt, w)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *recordingServer) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

func testSink(t *testing.T, webhookURL string) *sink {
	t.Helper()
	zone := time.FixedZone("JST", 9*60*60)
	return newSink(route{Name: "test", WebhookURL: webhookURL, MinScale: scaleUnknown, IncludeTest: true},
		&http.Client{Timeout: 5 * time.Second}, zone)
}

// 緊急地震速報の第2報以降は、新規投稿ではなく第1報のメッセージを書き換えます。
// これが壊れると、大きな地震のたびに10通以上の通知が並び、スマホが鳴り続けます。
func TestFollowUpReportEditsTheFirstMessage(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"111"}`))
	})
	s := testSink(t, server.URL)
	ctx := context.Background()

	s.deliver(ctx, decodeOrFail(t, eewIwateSerial1))
	s.deliver(ctx, decodeOrFail(t, eewIwateSerial2))

	requests := server.recorded()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	if requests[0].method != http.MethodPost {
		t.Errorf("first request method = %s, want POST", requests[0].method)
	}
	// wait=true を付けないとDiscordはメッセージIDを返さず、書き換えができません。
	if !strings.Contains(requests[0].query, "wait=true") {
		t.Errorf("first request query = %q, want wait=true", requests[0].query)
	}
	if requests[1].method != http.MethodPatch {
		t.Errorf("second request method = %s, want PATCH", requests[1].method)
	}
	if !strings.HasSuffix(requests[1].path, "/messages/111") {
		t.Errorf("second request path = %q, want it to target message 111", requests[1].path)
	}
}

// 1回の地震についての3報が1通にまとまり、最終的な表示に情報の後退が
// 無いことを確かめます。ユーザーから「『震源調査中 で地震』が残っているのは嫌だ」
// と指摘された挙動への回帰テストです。
func TestQuakeReportsCollapseIntoOneMessage(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"777"}`))
	})
	s := testSink(t, server.URL)
	ctx := context.Background()

	for _, raw := range []string{quakeScalePrompt, quakeDestination, quakeDetailScale} {
		s.deliver(ctx, decodeOrFail(t, raw))
	}

	requests := server.recorded()
	if len(requests) != 3 {
		t.Fatalf("got %d requests, want 3", len(requests))
	}
	if requests[0].method != http.MethodPost {
		t.Errorf("first request = %s, want POST", requests[0].method)
	}
	for _, index := range []int{1, 2} {
		if requests[index].method != http.MethodPatch {
			t.Errorf("request %d = %s, want PATCH (a follow-up must edit, not pile up)", index, requests[index].method)
		}
	}

	// 2通目(震源に関する情報)の時点で、震源が入りつつ最大震度が残っていること。
	// 合成しないとここが「最大震度不明」に後退します。
	middle := requests[1].body.Embeds[0]
	if !strings.Contains(middle.Description, "熊本県天草・芦北地方") {
		t.Errorf("second edit description = %q, want the newly known hypocenter", middle.Description)
	}
	if strings.Contains(middle.Description, "震度不明") {
		t.Errorf("second edit description = %q; the intensity from the first report was lost", middle.Description)
	}

	// 最終形。「震源調査中」がどこにも残っていないこと。
	final := requests[2].body.Embeds[0]
	if strings.Contains(final.Description, "震源調査中") {
		t.Errorf("final description = %q still says the hypocenter is being investigated", final.Description)
	}
	if !strings.Contains(final.Description, "最大震度3") {
		t.Errorf("final description = %q, want 最大震度3", final.Description)
	}
	if !hasField(final, "規模", "M4.0") {
		t.Errorf("final embed lost the magnitude: %+v", final.Fields)
	}
	if !hasField(final, "震度3", "上天草市大矢野町") {
		t.Errorf("final embed lost the detailed points: %+v", final.Fields)
	}
}

// 別の地震(EventIDが違う)は書き換えではなく新規投稿でなければなりません。
func TestDifferentEventPostsANewMessage(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"111"}`))
	})
	s := testSink(t, server.URL)
	ctx := context.Background()

	s.deliver(ctx, decodeOrFail(t, eewIwateSerial1))
	s.deliver(ctx, decodeOrFail(t, eewTraining))

	requests := server.recorded()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	if requests[1].method != http.MethodPost {
		t.Errorf("second request method = %s, want POST for a different EventID", requests[1].method)
	}
}

// 429 は再試行します。大地震では各地の震度と津波予報が短時間に集中し、
// webhookのレート制限に必ず当たります。ここで諦めると肝心の通知が消えます。
func TestRateLimitedRequestIsRetried(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":0.01}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"222"}`))
	})
	s := testSink(t, server.URL)

	before := notifySent.Load()
	s.deliver(context.Background(), decodeOrFail(t, quakeTokyo))

	if got := len(server.recorded()); got != 2 {
		t.Fatalf("got %d requests, want 2 (one rejected, one retried)", got)
	}
	if notifySent.Load() != before+1 {
		t.Error("a successful retry was not counted as sent")
	}
}

// 400 はペイロードかURLが悪いので、投げ直しても結果は変わりません。
// 再試行するとレート制限を無駄に消費します。
func TestBadRequestIsNotRetried(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Invalid Form Body"}`))
	})
	s := testSink(t, server.URL)

	before := notifyFailures.Load()
	s.deliver(context.Background(), decodeOrFail(t, quakeTokyo))

	if got := len(server.recorded()); got != 1 {
		t.Errorf("got %d requests, want 1 (no retry on 400)", got)
	}
	if notifyFailures.Load() != before+1 {
		t.Error("a permanent failure was not counted")
	}
}

// 受信ループは決してブロックさせません。webhookが詰まっている間もWebSocketは
// 読み続ける必要があり、そこで止まると接続ごと失います。
func TestOfferNeverBlocksWhenQueueIsFull(t *testing.T) {
	s := testSink(t, "http://example.invalid")
	e := decodeOrFail(t, quakeTokyo)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range queueDepth + 10 {
			s.offer(e)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("offer blocked when the queue was full")
	}

	if len(s.queue) > queueDepth {
		t.Errorf("queue holds %d events, want at most %d", len(s.queue), queueDepth)
	}
}

// 書き換え対象が古くなったら新規投稿に戻します。数時間前のメッセージを
// 書き換えても、誰も見ていないところで更新されるだけです。
func TestStaleGroupFallsBackToPosting(t *testing.T) {
	server := newRecordingServer(t, func(attempt int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"111"}`))
	})
	s := testSink(t, server.URL)
	ctx := context.Background()

	s.deliver(ctx, decodeOrFail(t, eewIwateSerial1))
	for key, message := range s.messages {
		s.messages[key] = groupMessage{id: message.id, written: time.Now().Add(-2 * groupTTL)}
	}
	s.deliver(ctx, decodeOrFail(t, eewIwateSerial2))

	requests := server.recorded()
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	if requests[1].method != http.MethodPost {
		t.Errorf("second request method = %s, want POST once the group went stale", requests[1].method)
	}
}
