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
