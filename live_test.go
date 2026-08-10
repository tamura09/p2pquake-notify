package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
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

// 続報がDiscord上で本当に第1報のメッセージを書き換えているかを、投稿した
// メッセージを読み戻して確かめます。
//
//	AWS_REGION=ap-northeast-1 P2PQUAKE_SEND_TEST_NOTIFICATION=1 go test -run TestLiveFollowUp -v ./...
//
// ローカルのテストは自前のHTTPサーバ相手にPATCHが飛んだことしか見ていません。
// 実際にDiscordが書き換えを受け付けたかは、本物に投げて読み返すまで分かりません。
func TestLiveFollowUpEditsTheMessageInDiscord(t *testing.T) {
	if os.Getenv("P2PQUAKE_SEND_TEST_NOTIFICATION") == "" {
		t.Skip("set P2PQUAKE_SEND_TEST_NOTIFICATION=1 to post to the dev channel")
	}

	ctx := context.Background()
	webhook := devWebhookURL(ctx, t)
	client := newAPIClient()
	s := newSink(route{
		Name:        "manual",
		WebhookURL:  webhook,
		MinScale:    scaleUnknown,
		IncludeTest: true,
	}, client, testZone())

	first := decodeOrFail(t, eewTraining)
	s.deliver(ctx, first)

	posted, ok := s.messages[first.GroupKey]
	if !ok || posted.id == "" {
		t.Fatal("no message id was captured from the POST; a follow-up has nothing to edit")
	}
	t.Logf("posted message %s", posted.id)

	second := decodeOrFail(t, eewTrainingSerial2)
	if second.GroupKey != first.GroupKey {
		t.Fatalf("GroupKey differs between serials (%q vs %q); they would never be joined",
			first.GroupKey, second.GroupKey)
	}
	s.deliver(ctx, second)

	after := s.messages[second.GroupKey]
	if after.id != posted.id {
		t.Errorf("follow-up used message %q, want the original %q (it posted a new message instead of editing)",
			after.id, posted.id)
	}

	// Discordから読み戻します。ここが本題で、こちらがPATCHを投げたことでは
	// なく、向こうが実際に書き換えたことを確かめます。
	title := fetchWebhookMessageTitle(ctx, t, client, webhook, posted.id)
	t.Logf("message now reads: %q", title)
	if !strings.Contains(title, "第2報") {
		t.Errorf("message %s still reads %q; the edit did not land", posted.id, title)
	}
}

func devWebhookURL(ctx context.Context, t *testing.T) string {
	t.Helper()

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
	return webhook
}

func fetchWebhookMessageTitle(ctx context.Context, t *testing.T, client *http.Client, webhook, messageID string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webhook+"/messages/"+messageID, nil)
	if err != nil {
		t.Fatalf("build read-back request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("read back message: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("read back message: %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var message struct {
		Embeds []struct {
			Title string `json:"title"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("decode read-back message: %v", err)
	}
	if len(message.Embeds) == 0 {
		t.Fatal("read-back message has no embed")
	}
	return message.Embeds[0].Title
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
