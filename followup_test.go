package main

import (
	"net/http"
	"testing"
	"time"
)

// alertIngest は alert ルート1本だけを持つ受信側を組み立てます。webhookには
// 到達できないアドレスを渡します。ここで見たいのは「キューに載ったかどうか」で、
// 送信そのものは別のテストの領分です。
func alertIngest(t *testing.T) (*ingest, *sink) {
	t.Helper()

	alert := route{
		Name:          "alert",
		WebhookURL:    "http://127.0.0.1:1/webhook",
		Codes:         codeSet(codeEEW, codeJMATsunami),
		MinScale:      scaleUnknown,
		QuakeAfterEEW: true,
	}
	s := newSink(alert, &http.Client{Timeout: time.Second}, testZone())

	return &ingest{
		cfg:   config{Zone: testZone()},
		dedup: newDedupCache(time.Hour, 100),
		sinks: []*sink{s},
	}, s
}

func queuedCodes(s *sink) []int {
	codes := make([]int, 0, len(s.queue))
	for {
		select {
		case e := <-s.queue:
			codes = append(codes, e.Code)
		default:
			return codes
		}
	}
}

// 緊急地震速報を流したチャンネルには、その地震の地震情報(551)も出します。
// 速報は「これから揺れる」という予想でしかないので、これが無いと
// 「震度5強を予想」で話が終わり、実際に何が起きたのか分かりません。
func TestQuakeAfterEEWReachesTheAlertRoute(t *testing.T) {
	receiver, s := alertIngest(t)

	receiver.dispatch(decodeOrFail(t, eewIwateSerial1))
	receiver.dispatch(decodeOrFail(t, quakeSanriku))

	got := queuedCodes(s)
	want := []int{codeEEW, codeJMAQuake}
	if len(got) != len(want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queued %v, want %v", got, want)
		}
	}
}

// 緊急地震速報が出ていない地震の地震情報は、これまで通り alert には流しません。
// ここが壊れると、日に何度もある小さな地震で速報チャンネルが埋まります。
func TestQuakeWithoutEEWStaysOffTheAlertRoute(t *testing.T) {
	receiver, s := alertIngest(t)

	receiver.dispatch(decodeOrFail(t, quakeTokyo))

	if got := queuedCodes(s); len(got) != 0 {
		t.Fatalf("queued %v, want nothing", got)
	}
}

// 速報を出した地震の12分後に起きた余震は別の地震です。発生時刻の許容幅が
// 広すぎると、これを「速報を出した地震の続き」として流してしまいます。
func TestAftershockIsNotTheEEWQuake(t *testing.T) {
	receiver, s := alertIngest(t)

	receiver.dispatch(decodeOrFail(t, eewIwateSerial1))
	receiver.dispatch(decodeOrFail(t, quakeSanriku))
	receiver.dispatch(decodeOrFail(t, quakeSanrikuAftershock))

	got := queuedCodes(s)
	if len(got) != 2 {
		t.Fatalf("queued %v, want only the EEW and its own quake report", got)
	}
}

// 訓練報は alert に流れないので、訓練報をきっかけに地震情報が流れてもいけません。
// 流れると、訓練のたびに本物と見分けのつかない地震情報が速報チャンネルに出ます。
func TestTrainingEEWDoesNotOpenTheAlertRoute(t *testing.T) {
	receiver, s := alertIngest(t)

	receiver.dispatch(decodeOrFail(t, eewTraining))
	receiver.dispatch(decodeOrFail(t, quakeSanriku))

	if got := queuedCodes(s); len(got) != 0 {
		t.Fatalf("queued %v, want nothing", got)
	}
}

// 551 は同じ地震について何度も発表されます。すべて通さないと、alert に出るのが
// 震度速報のままになり、各地の震度で書き換わりません。
func TestEveryQuakeReportOfTheSameEEWPasses(t *testing.T) {
	receiver, s := alertIngest(t)

	// 熊本の3報に合わせた緊急地震速報。発生時刻だけが突き合わせに効きます。
	eewKumamoto := `{
		"code": 556,
		"time": "2026/08/10 09:48:20.000",
		"test": false,
		"cancelled": false,
		"earthquake": {
			"originTime": "2026/08/10 09:48:02",
			"hypocenter": {"name": "熊本県天草・芦北地方", "depth": 10, "magnitude": 4.0}
		},
		"issue": {"time": "2026/08/10 09:48:20", "eventId": "20260810094800", "serial": "1"},
		"areas": [{"pref": "熊本", "name": "熊本県天草・芦北", "scaleFrom": 30, "scaleTo": 40, "kindCode": "11"}]
	}`

	receiver.dispatch(decodeOrFail(t, eewKumamoto))
	for _, raw := range []string{quakeScalePrompt, quakeDestination, quakeDetailScale} {
		receiver.dispatch(decodeOrFail(t, raw))
	}

	if got := queuedCodes(s); len(got) != 4 {
		t.Fatalf("queued %v, want the EEW and all three quake reports", got)
	}
}

// 取消報(実際には揺れない)で地震情報を待ち始めてはいけません。
func TestCancelledEEWIsNotRemembered(t *testing.T) {
	receiver, s := alertIngest(t)

	cancelled := `{
		"code": 556,
		"time": "2024/01/01 16:10:40.000",
		"test": false,
		"cancelled": true,
		"earthquake": {"originTime": "2024/01/01 16:10:00", "hypocenter": {"name": "三陸沖", "depth": 30, "magnitude": 7.2}},
		"issue": {"time": "2024/01/01 16:10:40", "eventId": "20240101161000", "serial": "9"},
		"areas": []
	}`

	receiver.dispatch(decodeOrFail(t, cancelled))
	receiver.dispatch(decodeOrFail(t, quakeSanriku))

	got := queuedCodes(s)
	if len(got) != 1 || got[0] != codeEEW {
		t.Fatalf("queued %v, want only the cancellation itself", got)
	}
}

// 覚えておく期間を過ぎたら忘れます。忘れないと、何時間も後に出た訂正報や
// 別の地震の情報が速報チャンネルに現れます。
func TestEEWQuakesForgetsAfterTTL(t *testing.T) {
	now := time.Date(2024, 1, 1, 16, 10, 30, 0, testZone())
	quakes := eewQuakes{ttl: time.Hour, now: func() time.Time { return now }}

	origin := time.Date(2024, 1, 1, 16, 10, 0, 0, testZone())
	quakes.record("alert", origin)
	if !quakes.matches("alert", origin) {
		t.Fatal("the quake was not remembered right after the warning")
	}

	now = now.Add(2 * time.Hour)
	if quakes.matches("alert", origin) {
		t.Error("the quake is still remembered after the TTL")
	}
}

// 覚えるのはルートごとです。あるルートが速報を流したからといって、
// 流していないルートに地震情報が出てはいけません。
func TestEEWQuakesAreRememberedPerRoute(t *testing.T) {
	quakes := eewQuakes{}
	origin := time.Date(2024, 1, 1, 16, 10, 0, 0, testZone())

	quakes.record("alert", origin)

	if !quakes.matches("alert", origin) {
		t.Error("alert forgot its own warning")
	}
	if quakes.matches("local", origin) {
		t.Error("local matched a warning it never sent")
	}
}

// 推定発生時刻と確定発生時刻のずれは許し、別の地震は拾わない、という境目の確認。
func TestEEWQuakeToleranceBoundary(t *testing.T) {
	quakes := eewQuakes{}
	origin := time.Date(2024, 1, 1, 16, 10, 0, 0, testZone())
	quakes.record("alert", origin)

	if !quakes.matches("alert", origin.Add(20*time.Second)) {
		t.Error("a 20 second difference should still be the same quake")
	}
	if quakes.matches("alert", origin.Add(defaultEEWQuakeTolerance+time.Second)) {
		t.Error("a difference beyond the tolerance should be a different quake")
	}
}

// 1回の地震で速報は10報以上届きます。報ごとに覚えていると表が太るので、
// 同じ地震として覚え直さないことを確かめます。
func TestRepeatedWarningsAboutOneQuakeAreRememberedOnce(t *testing.T) {
	receiver, _ := alertIngest(t)

	receiver.dispatch(decodeOrFail(t, eewIwateSerial1))
	receiver.dispatch(decodeOrFail(t, eewIwateSerial2))

	if got := len(receiver.eewQuakes.origins["alert"]); got != 1 {
		t.Errorf("remembered %d origins for one earthquake, want 1", got)
	}
}
