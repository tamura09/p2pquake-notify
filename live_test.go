package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/coder/websocket"
)

// 実際のDiscordチャンネルへ1通だけ送ります。地震を待たずに埋め込みの見た目を
// 確認するためのもので、ネットワークにもAWSにも出るので既定では走らせません。
//
//	AWS_REGION=ap-northeast-1 P2PQUAKE_SEND_TEST_NOTIFICATION=1 go test -run TestLiveSendOne -v ./...
//
// 送るのは訓練報です。本物と見分けのつかない緊急地震速報をチャンネルに置くと、
// 後から見た人が実際の警報と取り違えます。訓練報なら題名に【訓練】が付き、
// スマホを鳴らす本文も付かないうえ、地域ごとの予想震度をまとめる一番複雑な
// 描画経路をそのまま通ります。
func TestLiveSendOneNotificationToDev(t *testing.T) {
	if os.Getenv("P2PQUAKE_SEND_TEST_NOTIFICATION") == "" {
		t.Skip("set P2PQUAKE_SEND_TEST_NOTIFICATION=1 to post one message to the dev channel")
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	application := &app{parameters: ssm.NewFromConfig(cfg)}

	parameterName := envOr("P2PQUAKE_DEV_WEBHOOK_PARAMETER_NAME", "/p2pquake-notify/discord/dev-webhook-url")
	value, err := application.parameterString(ctx, parameterName)
	if err != nil {
		t.Fatalf("read %s: %v", parameterName, err)
	}
	webhook, err := validateWebhookURL(value)
	if err != nil {
		t.Fatalf("%s: %v", parameterName, err)
	}

	s := newSink(route{
		Name:        "manual",
		WebhookURL:  webhook,
		MinScale:    scaleUnknown,
		IncludeTest: true,
	}, newAPIClient(), testZone())

	e := decodeOrFail(t, eewTraining)
	payload := renderPayload(e, testZone())
	t.Logf("sending: %s", payload.Embeds[0].Title)

	before := notifyFailures.Load()
	sent := notifySent.Load()

	s.deliver(ctx, e)

	if notifyFailures.Load() != before {
		t.Fatal("Discord rejected the message; see the log line above for the status code")
	}
	if notifySent.Load() != sent+1 {
		t.Fatalf("expected exactly one message to be sent, counter went %d -> %d", sent, notifySent.Load())
	}
	t.Log("delivered 1 message to the dev channel")
}

// 上流のサンドボックスに実際に接続し、届いたメッセージがこのリポジトリの型で
// 読めることを確認します。ネットワークに出るので既定では走らせません。
//
//	P2PQUAKE_LIVE_TEST=1 go test -run TestLive -v ./...
//
// 型定義 (types.go) は上流のドキュメント頼りで、しかも上流は無保証の無償サービスです。
// フィールドが変わっても告知があるとは限らないので、上流の仕様を疑う場面では
// まずこれを走らせてください。
func TestLiveSandboxMessagesDecode(t *testing.T) {
	if os.Getenv("P2PQUAKE_LIVE_TEST") == "" {
		t.Skip("set P2PQUAKE_LIVE_TEST=1 to run against the upstream sandbox")
	}

	endpoint := envOr("P2PQUAKE_WS_URL", sandboxWebSocketURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: newWebSocketClient()})
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	zone := testZone()
	seen := map[int]int{}

	for range 12 {
		readCtx, readCancel := context.WithTimeout(ctx, 60*time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}

		e, err := decodeEvent(json.RawMessage(data))
		if err != nil {
			t.Errorf("decodeEvent failed for %s: %v", truncate(string(data), 400), err)
			continue
		}
		seen[e.Code]++

		// 未対応のcodeはこの型定義が追いついていない印なので、失敗ではなく
		// 記録として残します。dev用ルートには生JSONで流れます。
		if e.Payload == nil {
			t.Logf("unhandled code %d: %s", e.Code, truncate(string(data), 400))
			continue
		}

		payload := renderPayload(e, zone)
		if len(payload.Embeds) == 0 {
			t.Errorf("code %d rendered no embed", e.Code)
		}
		t.Logf("code=%d maxScale=%d prefectures=%v title=%q",
			e.Code, e.MaxScale, e.Prefectures, payload.Embeds[0].Title)
	}

	if len(seen) == 0 {
		t.Fatal("no messages received from the sandbox")
	}
	t.Logf("messages by code: %v", seen)
}

// 履歴補完を本番の /v2/history に対して実行します。
//
// これは実際に本番で起きた壊れ方への回帰テストです。履歴の取得にWebSocket用の
// クライアント(HTTP/1.1固定)を使い回していたため、HTTP/2で応える上流に対して
// "malformed HTTP response \x00\x00\x12\x04..." で毎回失敗していました。
// 補完の失敗はログを出して続行する設計なので、通知は正常に動いたまま
// 再接続時のギャップ埋めだけが黙って死んでいる状態になります。
// httptest はHTTP/1.1で応えるので、ローカルのテストではこの差が出ません。
func TestLiveHistoryBackfill(t *testing.T) {
	if os.Getenv("P2PQUAKE_LIVE_TEST") == "" {
		t.Skip("set P2PQUAKE_LIVE_TEST=1 to run against the upstream")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receiver := &ingest{
		cfg: config{
			HistoryURL:    defaultHistoryURL,
			BackfillLimit: 10,
			Zone:          testZone(),
		},
		apiClient: newAPIClient(),
		dedup:     newDedupCache(time.Hour, 100),
	}

	// notify=false なので通知はせず、鍵の登録だけを行います。
	if err := receiver.backfill(ctx, false); err != nil {
		t.Fatalf("backfill against %s: %v", defaultHistoryURL, err)
	}
	if receiver.dedup.len() == 0 {
		t.Error("history returned nothing; the gap-filling on reconnect would be a no-op")
	}
	t.Logf("primed %d events from history", receiver.dedup.len())
}
