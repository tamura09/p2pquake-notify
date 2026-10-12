package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func eewEvent(t *testing.T, serial string, scaleTo int, cancelled, test bool) *event {
	t.Helper()
	return decodeOrFail(t, fmt.Sprintf(`{
		"code": 556,
		"time": "2026/10/12 10:00:00.000",
		"test": %t,
		"cancelled": %t,
		"issue": {"time": "2026/10/12 10:00:00", "eventId": "20261012100000", "serial": %q},
		"earthquake": {
			"originTime": "2026/10/12 09:59:50",
			"hypocenter": {"name": "岩手県沖", "depth": 50, "magnitude": 6.8}
		},
		"areas": [{"pref": "岩手", "name": "岩手県沿岸北部", "scaleFrom": 45, "scaleTo": %d}]
	}`, test, cancelled, serial, scaleTo))
}

func tsunamiEvent(t *testing.T, grade string, cancelled bool) *event {
	t.Helper()
	return decodeOrFail(t, fmt.Sprintf(`{
		"code": 552,
		"time": "2026/10/12 10:03:00.000",
		"cancelled": %t,
		"issue": {"source": "気象庁", "time": "2026/10/12 10:03:00", "type": "Focus"},
		"areas": [{"grade": %q, "immediate": false, "name": "岩手県"}]
	}`, cancelled, grade))
}

func testPushoverSink() *pushoverSink {
	return newPushoverSink(pushoverCredentials{Token: "token", User: "user"}, &http.Client{Timeout: time.Second}, testZone())
}

func TestPushoverRouteTakesOnlyRealWarnings(t *testing.T) {
	r := pushoverRoute()
	if !r.matches(eewEvent(t, "1", scale5Upper, false, false)) {
		t.Error("an earthquake early warning did not match")
	}
	if !r.matches(tsunamiEvent(t, "Watch", false)) {
		t.Error("a tsunami forecast did not match")
	}
	if r.matches(eewEvent(t, "1", scale5Upper, false, true)) {
		t.Error("a training early warning matched; it must never reach a phone")
	}
	if r.matches(decodeOrFail(t, quakeAt(time.Date(2026, 10, 12, 10, 0, 0, 0, testZone())))) {
		t.Error("an earthquake report (551) matched; it is a record of what already happened")
	}
}

func TestPushoverSendsAnEarthquakeOnlyWhenItGetsWorse(t *testing.T) {
	recorder := &pushoverRecorder{}
	p := recorder.sink(t, func(int, http.ResponseWriter) {})

	for _, e := range []*event{
		eewEvent(t, "1", scale5Upper, false, false), // 最初の報
		eewEvent(t, "2", scale5Upper, false, false), // 同じ予想
		eewEvent(t, "3", scale6Lower, false, false), // 予想が上がった
		eewEvent(t, "4", scale5Upper, false, false), // 予想が下がった
		eewEvent(t, "5", scale5Upper, true, false),  // 取消
	} {
		p.deliver(context.Background(), e)
	}

	got := recorder.priorities()
	want := []string{"1", "1", "0"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pushed priorities %v, want %v (first, raised, cancellation)", got, want)
	}
}

func TestPushoverRetriesAnEarthquakeWhosePushFailed(t *testing.T) {
	recorder := &pushoverRecorder{}
	p := recorder.sink(t, func(attempt int, w http.ResponseWriter) {
		if attempt == 0 {
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	p.deliver(context.Background(), eewEvent(t, "1", scale5Upper, false, false))
	p.deliver(context.Background(), eewEvent(t, "2", scale5Upper, false, false))

	if got := len(recorder.priorities()); got != 2 {
		t.Errorf("made %d requests; the second report must be pushed because the first never got through", got)
	}
}

func TestPushoverEEWMessageCarriesTheForecast(t *testing.T) {
	message, _ := testPushoverSink().compose(eewEvent(t, "1", scale6Lower, false, false))
	if message.Title != "緊急地震速報(警報) 第1報" {
		t.Errorf("title = %q", message.Title)
	}
	for _, want := range []string{"岩手県沖 で地震", "予想最大震度6弱", "岩手県沿岸北部"} {
		if !strings.Contains(message.Message, want) {
			t.Errorf("message does not contain %q:\n%s", want, message.Message)
		}
	}
	if strings.Contains(message.Message, "**") {
		t.Errorf("Discord markdown leaked into the message:\n%s", message.Message)
	}
}

func TestPushoverTsunamiPriorityFollowsTheGrade(t *testing.T) {
	cases := []struct {
		grade        string
		cancelled    bool
		wantTitle    string
		wantPriority int
	}{
		{"MajorWarning", false, "大津波警報", pushoverPriorityHigh},
		{"Warning", false, "津波警報", pushoverPriorityHigh},
		{"Watch", false, "津波注意報", pushoverPriorityNormal},
		{"Warning", true, "津波予報 解除", pushoverPriorityNormal},
	}
	for _, c := range cases {
		message, sent := testPushoverSink().compose(tsunamiEvent(t, c.grade, c.cancelled))
		if !sent {
			t.Fatalf("%s cancelled=%t: not sent", c.grade, c.cancelled)
		}
		if message.Title != c.wantTitle || message.Priority != c.wantPriority {
			t.Errorf("%s cancelled=%t: title=%q priority=%d, want %q %d",
				c.grade, c.cancelled, message.Title, message.Priority, c.wantTitle, c.wantPriority)
		}
	}
}

type pushoverRecorder struct {
	mu       sync.Mutex
	requests []url.Values
}

// sink は記録用サーバへ送る pushoverSink を作ります。再試行の待ちは詰めます。
func (r *pushoverRecorder) sink(t *testing.T, respond func(attempt int, w http.ResponseWriter)) *pushoverSink {
	t.Helper()
	p := testPushoverSink()
	p.endpoint = r.server(t, respond).URL
	p.retryDelay = time.Millisecond
	return p
}

func (r *pushoverRecorder) priorities() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := make([]string, 0, len(r.requests))
	for _, request := range r.requests {
		got = append(got, request.Get("priority"))
	}
	return got
}

func (r *pushoverRecorder) server(t *testing.T, respond func(attempt int, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		r.mu.Lock()
		attempt := len(r.requests)
		r.requests = append(r.requests, req.PostForm)
		r.mu.Unlock()
		respond(attempt, w)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPushoverSendPostsTheForm(t *testing.T) {
	recorder := &pushoverRecorder{}
	p := recorder.sink(t, func(int, http.ResponseWriter) {})

	if err := p.send(context.Background(), pushoverMessage{Title: "t", Message: "m", Priority: pushoverPriorityHigh}); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := recorder.requests[0]
	for field, want := range map[string]string{"token": "token", "user": "user", "title": "t", "message": "m", "priority": "1"} {
		if got.Get(field) != want {
			t.Errorf("%s = %q, want %q", field, got.Get(field), want)
		}
	}
}

func TestPushoverSendDoesNotRetryARejectedRequest(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		recorder := &pushoverRecorder{}
		p := recorder.sink(t, func(_ int, w http.ResponseWriter) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"user":"invalid","errors":["user identifier is invalid"],"status":0}`))
		})

		err := p.send(context.Background(), pushoverMessage{Title: "t", Message: "m"})

		var status statusError
		if !errors.As(err, &status) || status.code != code {
			t.Fatalf("err = %v, want a %d statusError", err, code)
		}
		if !strings.Contains(err.Error(), "user identifier is invalid") {
			t.Errorf("error does not carry Pushover's reason: %v", err)
		}
		if len(recorder.requests) != 1 {
			t.Errorf("%d: sent %d times; it must not be retried", code, len(recorder.requests))
		}
	}
}

func TestPushoverSendRetriesServerErrors(t *testing.T) {
	recorder := &pushoverRecorder{}
	p := recorder.sink(t, func(attempt int, w http.ResponseWriter) {
		if attempt == 0 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	if err := p.send(context.Background(), pushoverMessage{Title: "t", Message: "m"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(recorder.requests) != 2 {
		t.Errorf("sent %d times, want a retry after the 500", len(recorder.requests))
	}
}

type mapResolver map[string]string

func (m mapResolver) parameterString(_ context.Context, name string) (string, error) {
	value, ok := m[name]
	if !ok {
		return "", errors.New("ParameterNotFound")
	}
	return value, nil
}

func TestReadPushoverCredentialsRejectsThePlaceholder(t *testing.T) {
	valid := strings.Repeat("a", 30)

	resolver := mapResolver{"/token": "MANAGED_OUTSIDE_TERRAFORM", "/user": valid}
	if _, err := readPushoverCredentials(context.Background(), resolver, "/token", "/user"); err == nil {
		t.Error("the Terraform placeholder was accepted as a token")
	}

	resolver = mapResolver{"/token": valid + "\n", "/user": valid}
	credentials, err := readPushoverCredentials(context.Background(), resolver, "/token", "/user")
	if err != nil {
		t.Fatalf("valid keys rejected: %v", err)
	}
	if credentials.Token != valid {
		t.Errorf("token = %q, want it trimmed", credentials.Token)
	}

	if _, err := readPushoverCredentials(context.Background(), resolver, "/token", ""); err == nil {
		t.Error("a half-configured Pushover was accepted")
	}
}

func TestTruncateRunesCountsCharacters(t *testing.T) {
	value := strings.Repeat("震", 1100)
	got := truncateRunes(value, pushoverMessageLimit)
	if n := len([]rune(got)); n != pushoverMessageLimit {
		t.Errorf("got %d characters, want %d", n, pushoverMessageLimit)
	}
	if truncateRunes("短い", pushoverMessageLimit) != "短い" {
		t.Error("a short value was changed")
	}
}

func TestFanOutHandsWarningsToPushover(t *testing.T) {
	receiver, _ := alertIngest(t)
	receiver.pushover = testPushoverSink()

	receiver.dispatch(eewEvent(t, "1", scale5Upper, false, false))
	receiver.dispatch(decodeOrFail(t, quakeAt(time.Date(2026, 10, 12, 10, 0, 0, 0, testZone()))))

	if got := len(receiver.pushover.queue); got != 1 {
		t.Errorf("pushover queue holds %d events, want only the early warning", got)
	}
}

func TestPushoverDoesNotSendACancellationNobodyWasWarnedAbout(t *testing.T) {
	p := testPushoverSink()
	if _, sent := p.compose(eewEvent(t, "1", scale5Upper, true, false)); sent {
		t.Error("a cancellation for an unseen earthquake was sent")
	}
}
