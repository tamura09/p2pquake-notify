package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testLineGroup  = "C0123456789abcdef0123456789abcdef"
	testLineGroup2 = "Cfedcba9876543210fedcba9876543210"
	testLineTest   = "C00000000000000000000000000000000"
)

// fakeLineGroups は参加中のグループの表の代わり。
type fakeLineGroups struct {
	active []string
	err    error
}

func (f *fakeLineGroups) activeGroups(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.active...), nil
}

func (f *fakeLineGroups) isActive(_ context.Context, groupID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return slices.Contains(f.active, groupID), nil
}

// quakeReport は eewEvent(t, ...) と同じ地震についての地震情報(551)を1件組み立てます。
// 発生時刻は速報の推定発生時刻から数秒ずらしてあります(実際にもずれます)。
func quakeReport(t *testing.T, issueType, issued, hypocenterName string, maxScale, points int) *event {
	t.Helper()
	pointList := make([]string, 0, points)
	for i := range points {
		pointList = append(pointList, fmt.Sprintf(`{"pref": "岩手県", "addr": "宮古市%d", "isArea": false, "scale": %d}`, i, maxScale))
	}
	magnitude, depth := 6.9, 50
	if hypocenterName == "" {
		magnitude, depth = -1, -1
	}
	return decodeOrFail(t, fmt.Sprintf(`{
		"code": 551,
		"time": "2026/10/12 %s.000",
		"earthquake": {
			"time": "2026/10/12 09:59:55",
			"hypocenter": {"name": %q, "depth": %d, "magnitude": %v},
			"maxScale": %d,
			"domesticTsunami": "Checking"
		},
		"issue": {"source": "気象庁", "time": "2026/10/12 %s", "type": %q, "correct": "None"},
		"points": [%s]
	}`, issued, hypocenterName, depth, magnitude, maxScale, issued, issueType, strings.Join(pointList, ",")))
}

func testLineSink() *lineSink {
	return newLineSink(
		lineCredentials{Token: "token", TestGroup: testLineTest},
		&fakeLineGroups{active: []string{testLineGroup}},
		&http.Client{Timeout: time.Second}, testZone())
}

type lineRequest struct {
	authorization string
	retryKey      string
	to            string
	text          string
}

type lineRecorder struct {
	mu       sync.Mutex
	requests []lineRequest
}

// sink は記録用サーバへ送る lineSink を作ります。再試行の待ちは詰めます。
func (r *lineRecorder) sink(t *testing.T, respond func(attempt int, w http.ResponseWriter)) *lineSink {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var payload struct {
			To       string `json:"to"`
			Messages []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &payload)
		request := lineRequest{
			authorization: req.Header.Get("Authorization"),
			retryKey:      req.Header.Get("X-Line-Retry-Key"),
			to:            payload.To,
		}
		if len(payload.Messages) == 1 && payload.Messages[0].Type == "text" {
			request.text = payload.Messages[0].Text
		}

		r.mu.Lock()
		attempt := len(r.requests)
		r.requests = append(r.requests, request)
		r.mu.Unlock()
		respond(attempt, w)
	}))
	t.Cleanup(server.Close)

	l := testLineSink()
	l.endpoint = server.URL
	l.retryDelay = time.Millisecond
	return l
}

func (r *lineRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	texts := make([]string, 0, len(r.requests))
	for _, request := range r.requests {
		texts = append(texts, request.text)
	}
	return texts
}

func TestLineRouteMirrorsTheAlertChannel(t *testing.T) {
	r := lineRoute()
	if !r.matches(eewEvent(t, "1", scale5Upper, false, false)) {
		t.Error("an earthquake early warning did not match")
	}
	if !r.matches(tsunamiEvent(t, "Watch", false)) {
		t.Error("a tsunami forecast did not match")
	}
	if !r.matches(eewEvent(t, "1", scale5Upper, false, true)) {
		t.Error("a training early warning did not match; it must reach the test group")
	}
	if !r.QuakeAfterEEW {
		t.Error("the LINE route must carry the earthquake report of a warned earthquake, as alert does")
	}
}

// LINE には Discord の alert と同じものが届きます。速報を出した地震の地震情報は
// 届き、速報の無い地震の地震情報は届きません。
func TestFanOutHandsTheAlertChannelToLine(t *testing.T) {
	receiver, alert := alertIngest(t)
	receiver.line = testLineSink()

	receiver.dispatch(decodeOrFail(t, quakeAt(time.Date(2026, 10, 12, 8, 0, 0, 0, testZone()))))
	receiver.dispatch(eewEvent(t, "1", scale5Upper, false, false))
	receiver.dispatch(quakeReport(t, "ScalePrompt", "10:01:30", "", scale5Upper, 1))
	receiver.dispatch(tsunamiEvent(t, "Watch", false))

	got := make([]int, 0, len(receiver.line.queue))
	for len(receiver.line.queue) > 0 {
		got = append(got, (<-receiver.line.queue).Code)
	}
	want := []int{codeEEW, codeJMAQuake, codeJMATsunami}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("LINE queue holds %v, want %v", got, want)
	}
	if alertGot := queuedCodes(alert); fmt.Sprint(alertGot) != fmt.Sprint(want) {
		t.Errorf("alert queue holds %v; LINE must match it (%v)", alertGot, want)
	}
}

func TestLineSendsAnEarthquakeWarningOnlyWhenItGetsWorse(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})

	for _, e := range []*event{
		eewEvent(t, "1", scale5Upper, false, false), // 最初の報
		eewEvent(t, "2", scale5Upper, false, false), // 同じ予想
		eewEvent(t, "3", scale6Lower, false, false), // 予想が上がった
		eewEvent(t, "4", scale5Upper, false, false), // 予想が下がった
		eewEvent(t, "5", scale5Upper, true, false),  // 取消
	} {
		l.deliver(context.Background(), e)
	}

	texts := recorder.texts()
	if len(texts) != 3 {
		t.Fatalf("sent %d messages, want 3 (first, raised, cancellation):\n%s", len(texts), strings.Join(texts, "\n---\n"))
	}
	if !strings.HasPrefix(texts[0], "【緊急地震速報(警報) 第1報】\n") {
		t.Errorf("first message does not lead with the title:\n%s", texts[0])
	}
	if !strings.Contains(texts[1], "予想最大震度6弱") {
		t.Errorf("raised message does not carry the new forecast:\n%s", texts[1])
	}
}

// 地震情報は、最初の報と最初の「各地の震度」だけを送ります。震源に関する情報は
// 単独では送らず、次に送る報に震源として載ります。
func TestLineSendsTheFirstAndTheDetailedEarthquakeReport(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})

	for _, e := range []*event{
		quakeReport(t, "ScalePrompt", "10:01:30", "", scale5Upper, 1),
		quakeReport(t, "Destination", "10:03:00", "岩手県沖", scaleUnknown, 0),
		quakeReport(t, "DetailScale", "10:06:00", "", scale5Upper, 20),
		quakeReport(t, "DetailScale", "10:10:00", "岩手県沖", scale5Upper, 25),
	} {
		l.deliver(context.Background(), e)
	}

	texts := recorder.texts()
	if len(texts) != 2 {
		t.Fatalf("sent %d messages, want 2 (first and detailed):\n%s", len(texts), strings.Join(texts, "\n---\n"))
	}
	if !strings.Contains(texts[1], "岩手県沖") {
		t.Errorf("the detailed report lost the hypocenter from the report before it:\n%s", texts[1])
	}
	if !strings.Contains(texts[1], "宮古市") {
		t.Errorf("the detailed report does not list where it shook:\n%s", texts[1])
	}
}

func TestLineSendsAnEarthquakeReportWhoseScaleWentUp(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})

	l.deliver(context.Background(), quakeReport(t, "ScalePrompt", "10:01:30", "", scale5Lower, 1))
	l.deliver(context.Background(), quakeReport(t, "ScalePrompt", "10:02:00", "", scale5Upper, 2))
	l.deliver(context.Background(), quakeReport(t, "ScalePrompt", "10:02:30", "", scale5Lower, 2))

	if got := len(recorder.texts()); got != 2 {
		t.Errorf("sent %d messages, want 2 (first, raised)", got)
	}
}

func TestLineRetriesAReportWhoseSendFailed(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(attempt int, w http.ResponseWriter) {
		// 各地震の最初の報だけが失敗します。
		if attempt == 0 || attempt == 2 {
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	l.deliver(context.Background(), eewEvent(t, "1", scale5Upper, false, false))
	l.deliver(context.Background(), eewEvent(t, "2", scale5Upper, false, false))
	l.deliver(context.Background(), quakeReport(t, "Destination", "10:03:00", "岩手県沖", scaleUnknown, 0))
	l.deliver(context.Background(), quakeReport(t, "Destination", "10:04:00", "岩手県沖", scaleUnknown, 0))

	if got := len(recorder.texts()); got != 4 {
		t.Errorf("made %d requests; each failed first report must be retried with the next one", got)
	}
}

func TestLinePushSendsTheMessage(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})

	if err := l.send(context.Background(), []string{testLineGroup, testLineGroup2}, "本文"); err != nil {
		t.Fatalf("send: %v", err)
	}

	if len(recorder.requests) != 2 {
		t.Fatalf("made %d requests, want one per recipient", len(recorder.requests))
	}
	for i, to := range []string{testLineGroup, testLineGroup2} {
		got := recorder.requests[i]
		if got.to != to || got.text != "本文" || got.authorization != "Bearer token" || got.retryKey == "" {
			t.Errorf("request %d = %+v", i, got)
		}
	}
	if recorder.requests[0].retryKey == recorder.requests[1].retryKey {
		t.Error("two recipients shared a retry key; LINE would drop the second as a duplicate")
	}
}

func TestLinePushRetriesServerErrorsWithTheSameKey(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(attempt int, w http.ResponseWriter) {
		switch attempt {
		case 0:
			w.WriteHeader(http.StatusInternalServerError)
		case 1:
			// 1回目が実は受け付けられていた場合の応答。
			w.Header().Set("X-Line-Accepted-Request-Id", "accepted")
			w.WriteHeader(http.StatusConflict)
		}
	})

	if err := l.send(context.Background(), []string{testLineGroup}, "本文"); err != nil {
		t.Fatalf("send: %v (a 409 for an accepted retry key means it was delivered)", err)
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("made %d requests, want a retry after the 500", len(recorder.requests))
	}
	if recorder.requests[0].retryKey != recorder.requests[1].retryKey {
		t.Error("the retry used a new key; a request LINE had already accepted would be delivered twice")
	}
}

func TestLinePushDoesNotRetryARejectedRequest(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		recorder := &lineRecorder{}
		l := recorder.sink(t, func(_ int, w http.ResponseWriter) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"You have reached your monthly limit."}`))
		})

		err := l.send(context.Background(), []string{testLineGroup}, "本文")
		if err == nil || !strings.Contains(err.Error(), "monthly limit") {
			t.Errorf("%d: err = %v, want LINE's reason", code, err)
		}
		if len(recorder.requests) != 1 {
			t.Errorf("%d: sent %d times; it must not be retried", code, len(recorder.requests))
		}
	}
}

func TestLineTextFitsTheLimit(t *testing.T) {
	text := lineText(discordWebhookPayload{Embeds: []discordEmbed{{
		Title:       "津波予報",
		Description: strings.Repeat("震", 6000),
	}}})
	if n := len([]rune(text)); n != lineTextLimit {
		t.Errorf("got %d characters, want %d", n, lineTextLimit)
	}
}

// 本番のイベントは参加中の全グループへ、訓練報はテスト用グループだけへ届きます。
func TestLineSendsRealEventsToEveryGroupAndTrainingToTheTestGroup(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})
	l.groups = &fakeLineGroups{active: []string{testLineGroup, testLineGroup2, testLineTest}}

	l.deliver(context.Background(), tsunamiEvent(t, "Warning", false))
	l.deliver(context.Background(), eewEvent(t, "1", scale5Upper, false, true))

	got := []string{}
	for _, request := range recorder.requests {
		got = append(got, request.to)
	}
	want := []string{testLineGroup, testLineGroup2, testLineTest, testLineTest}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sent to %v, want %v (every group, then the test group only)", got, want)
	}
}

// テスト用グループが設定されていない・退出済みなら、訓練報は本番へ切り替えずに落とします。
func TestLineNeverSendsTrainingToRealGroups(t *testing.T) {
	for name, configure := range map[string]func(*lineSink){
		"not configured": func(l *lineSink) { l.credentials.TestGroup = "" },
		"not active":     func(l *lineSink) { l.groups = &fakeLineGroups{active: []string{testLineGroup}} },
	} {
		recorder := &lineRecorder{}
		l := recorder.sink(t, func(int, http.ResponseWriter) {})
		configure(l)

		l.deliver(context.Background(), eewEvent(t, "1", scale5Upper, false, true))

		if len(recorder.requests) != 0 {
			t.Errorf("%s: a training warning was sent to %v", name, recorder.requests[0].to)
		}
	}
}

// サンドボックスへ繋いでいる時は、本物の形をしたイベントもテスト用グループだけへ送ります。
func TestLineTestOnlyKeepsEverythingInTheTestGroup(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})
	l.groups = &fakeLineGroups{active: []string{testLineGroup, testLineTest}}
	l.credentials.TestOnly = true

	l.deliver(context.Background(), tsunamiEvent(t, "Warning", false))

	if len(recorder.requests) != 1 || recorder.requests[0].to != testLineTest {
		t.Errorf("sent %+v, want the test group only", recorder.requests)
	}
}

// 地震の最中にグループの表が読めなくても、最後に読めた一覧へ送ります。
func TestLineFallsBackToTheLastKnownGroups(t *testing.T) {
	recorder := &lineRecorder{}
	l := recorder.sink(t, func(int, http.ResponseWriter) {})
	groups := &fakeLineGroups{active: []string{testLineGroup}}
	l.groups = groups

	if _, err := l.activeGroups(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	groups.err = errors.New("throttled")

	l.deliver(context.Background(), tsunamiEvent(t, "Warning", false))

	if len(recorder.requests) != 1 || recorder.requests[0].to != testLineGroup {
		t.Errorf("sent %+v, want the last known group", recorder.requests)
	}
}

func TestReadLineCredentials(t *testing.T) {
	resolver := mapResolver{"/token": "MANAGED_OUTSIDE_TERRAFORM", "/test": testLineTest}
	if _, err := readLineCredentials(context.Background(), resolver, "/token", "table", "/test"); err == nil {
		t.Error("the Terraform placeholder was accepted as a token")
	}

	resolver = mapResolver{"/token": "abc+/=\n", "/test": testLineTest + "\n"}
	credentials, err := readLineCredentials(context.Background(), resolver, "/token", "table", "/test")
	if err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}
	if credentials.Token != "abc+/=" || credentials.TestGroup != testLineTest {
		t.Errorf("got %+v, want both trimmed", credentials)
	}

	// テスト用グループが読めなくても、本番への送信は止めません。
	resolver = mapResolver{"/token": "abc", "/test": "MANAGED_OUTSIDE_TERRAFORM"}
	credentials, err = readLineCredentials(context.Background(), resolver, "/token", "table", "/test")
	if err != nil || credentials.TestGroup != "" {
		t.Errorf("got %+v, %v; want production enabled and the test group disabled", credentials, err)
	}

	if _, err := readLineCredentials(context.Background(), resolver, "/token", "", "/test"); err == nil {
		t.Error("a LINE without a group table was accepted")
	}
}
