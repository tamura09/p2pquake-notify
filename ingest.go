package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

const (
	// readLimit はWebSocketの1メッセージあたりの上限。各地の震度が数百地点
	// 入る地震情報でも数十KBなので、1MBあれば十分な余裕があります。
	readLimit = 1 << 20

	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second
)

// ingest はWebSocket接続を維持し、届いたメッセージをルートへ配ります。
type ingest struct {
	cfg    config
	client *http.Client
	dedup  *dedupCache
	sinks  []*sink
}

// run は接続が切れても諦めずに繋ぎ直し続けます。ctx がキャンセルされるまで戻りません。
func (i *ingest) run(ctx context.Context) {
	// 起動時の履歴読み込みは「通知せずに鍵だけ覚える」ためのものです。
	// ここで通知してしまうと、タスクが再配置されるたびに直近の地震が
	// まとめて再通知されます。
	if err := i.backfill(ctx, false); err != nil {
		log.Printf("startup backfill failed (continuing): %v", err)
	}

	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := i.connectAndRead(ctx)
		if ctx.Err() != nil {
			return
		}

		// 十分に長く繋がっていたなら、それは「一度は成功した接続」なので
		// バックオフを初期値に戻します。そうしないと、たまたま長時間動いた後の
		// 1回の切断で以後ずっと最大待ち時間になってしまいます。
		if time.Since(start) > time.Minute {
			backoff = minBackoff
		}

		log.Printf("websocket disconnected after %s: %v (reconnecting in %s)",
			time.Since(start).Round(time.Second), err, backoff.Round(time.Millisecond))
		wsConnected.Store(false)
		wsReconnects.Add(1)

		if err := sleep(ctx, backoff); err != nil {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff は指数バックオフにジッタを乗せます。ジッタが要るのは、上流が
// 落ちて全クライアントが同時に切断された時、揃って同じ秒数後に再接続すると
// 復旧しかけた上流をまた倒しにいくためです。
func nextBackoff(current time.Duration) time.Duration {
	next := time.Duration(math.Min(float64(current)*2, float64(maxBackoff)))
	jitter := time.Duration(rand.Int64N(int64(next / 2)))
	return next/2 + jitter
}

func (i *ingest) connectAndRead(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, i.cfg.WebSocketURL, &websocket.DialOptions{
		HTTPClient: i.client,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	log.Printf("websocket connected to %s", i.cfg.WebSocketURL)
	wsConnected.Store(true)
	lastMessageAt.Store(time.Now().UnixMilli())

	// 再接続後は切断中に起きたイベントを履歴から拾い直します。今回が
	// 起動直後の初回接続なら、上の startup backfill が鍵を入れているので
	// ここで重複通知にはなりません。
	if wsReconnects.Load() > 0 {
		if err := i.backfill(ctx, true); err != nil {
			log.Printf("reconnect backfill failed (continuing): %v", err)
		}
	}

	for {
		// 読み取りに期限を付けるのが接続の生死判定そのものです。TCPは
		// 相手が黙って消えても切れたことにならないので、期限が無いと
		// 死んだ接続の前で永遠に待ち続けます。地震が無くても code 555 が
		// 定期的に流れてくるため、無音が続く=異常と判断できます。
		readCtx, readCancel := context.WithTimeout(ctx, i.cfg.StaleAfter)
		messageType, data, err := conn.Read(readCtx)
		readCancel()

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("no message for %s", i.cfg.StaleAfter)
			}
			return fmt.Errorf("read: %w", err)
		}
		if messageType != websocket.MessageText {
			continue
		}

		lastMessageAt.Store(time.Now().UnixMilli())
		messagesReceived.Add(1)
		i.handle(data)
	}
}

// handle は1メッセージをデコードして、通すべきルートへ配ります。
func (i *ingest) handle(data []byte) {
	e, err := decodeEvent(json.RawMessage(data))
	if err != nil {
		log.Printf("decode failed (dropping): %v: %s", err, truncate(string(data), 300))
		decodeFailures.Add(1)
		return
	}
	if e == nil {
		return
	}
	i.dispatch(e)
}

func (i *ingest) dispatch(e *event) {
	// ピア分布は接続が生きている印として毎回届くので、重複排除の枠を
	// 食い潰さないよう鍵を覚えません。dev用ルートへはそのまま流します。
	if e.Code != codeAreaPeers && i.dedup.seen(e.DedupKey) {
		duplicatesDropped.Add(1)
		return
	}

	for _, s := range i.sinks {
		if s.route.matches(e) {
			s.offer(e)
		}
	}
}

// backfill は /v2/history を読んで、切断中に発生したイベントを拾います。
//
// notify が false なら通知せず鍵だけを登録します(起動直後用)。true なら
// 未見のイベントを通常どおり配ります。履歴は新しい順に返るので、通知の
// 順序が自然になるよう古い順に並べ替えてから流します。
func (i *ingest) backfill(ctx context.Context, notify bool) error {
	if i.cfg.BackfillLimit <= 0 {
		return nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	endpoint, err := url.Parse(i.cfg.HistoryURL)
	if err != nil {
		return fmt.Errorf("parse history URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("limit", strconv.Itoa(i.cfg.BackfillLimit))
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError{code: resp.StatusCode, body: string(body)}
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(body, &messages); err != nil {
		return fmt.Errorf("decode history: %w", err)
	}

	for index := len(messages) - 1; index >= 0; index-- {
		e, err := decodeEvent(messages[index])
		if err != nil || e == nil {
			continue
		}
		// 履歴に必ず含まれるピア分布は、切断中に起きた「出来事」ではないので
		// 補完の対象にしません。
		if e.Code == codeAreaPeers {
			continue
		}
		if i.dedup.seen(e.DedupKey) {
			continue
		}
		if !notify {
			continue
		}
		log.Printf("backfilled event code=%d from history", e.Code)
		backfilledEvents.Add(1)
		for _, s := range i.sinks {
			if s.route.matches(e) {
				s.offer(e)
			}
		}
	}
	return nil
}
