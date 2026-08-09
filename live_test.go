package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
)

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
