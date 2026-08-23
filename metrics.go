package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// 地震はめったに起きないので、「通知が来ない」だけでは正常なのか死んでいるのか
// 区別がつきません。このサービスに関しては死活監視が機能そのものと同じくらい
// 重要で、Grafana Cloud へ定期的に押し込む値がその唯一の生存証明です。
//
// 監視すべきは p2pquake_last_message_age_seconds です。上流は地震が無くても
// ピア分布(code 555)を定期的に流すので、この値が伸び続けているなら
// 通知経路のどこかが死んでいます。No Data も同じ意味で扱ってください。
var (
	messagesReceived  atomic.Int64
	decodeFailures    atomic.Int64
	duplicatesDropped atomic.Int64
	notifySent        atomic.Int64
	notifyFailures    atomic.Int64
	wsReconnects      atomic.Int64
	backfilledEvents  atomic.Int64

	wsConnected   atomic.Bool
	lastMessageAt atomic.Int64 // Unix milliseconds
)

const metricJobLabel = "p2pquake-notify"

type metricsPusher struct {
	client   *http.Client
	url      string
	username string
	password string
	interval time.Duration
	started  time.Time
}

// run は interval ごとにメトリクスを押し込みます。押し込み先が未設定なら
// 何もせず戻ります(ローカル実行やサンドボックス確認のため)。
func (p *metricsPusher) run(ctx context.Context) {
	if p.url == "" {
		log.Printf("metrics: no remote_write URL configured, heartbeat disabled")
		return
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.push(ctx); err != nil {
				// メトリクスが押せないこと自体で通知を止めはしません。
				// ログには必ず残して、監視の穴として気付けるようにします。
				log.Printf("metrics: push failed: %v", err)
			}
		}
	}
}

func (p *metricsPusher) push(ctx context.Context) error {
	pushCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return p.remoteWrite(pushCtx, buildTimeSeries(time.Now(), p.started))
}

func buildTimeSeries(now time.Time, started time.Time) []prompb.TimeSeries {
	timestamp := now.UnixMilli()
	base := map[string]string{}

	// 最終受信からの経過秒。まだ一度も受信していない間は起動からの経過を
	// 返します。ゼロを返すと「たった今受信した」に見えてしまい、
	// 起動直後に接続できていない状態を隠してしまいます。
	age := now.Sub(started).Seconds()
	if last := lastMessageAt.Load(); last > 0 {
		age = now.Sub(time.UnixMilli(last)).Seconds()
	}

	connected := 0.0
	if wsConnected.Load() {
		connected = 1
	}

	return []prompb.TimeSeries{
		gauge("p2pquake_up", base, 1, timestamp),
		gauge("p2pquake_ws_connected", base, connected, timestamp),
		gauge("p2pquake_last_message_age_seconds", base, round(age, 1), timestamp),
		gauge("p2pquake_uptime_seconds", base, round(now.Sub(started).Seconds(), 1), timestamp),

		gauge("p2pquake_messages_received_total", base, float64(messagesReceived.Load()), timestamp),
		gauge("p2pquake_decode_failures_total", base, float64(decodeFailures.Load()), timestamp),
		gauge("p2pquake_duplicates_dropped_total", base, float64(duplicatesDropped.Load()), timestamp),
		gauge("p2pquake_notifications_sent_total", base, float64(notifySent.Load()), timestamp),
		gauge("p2pquake_notification_failures_total", base, float64(notifyFailures.Load()), timestamp),
		gauge("p2pquake_ws_reconnects_total", base, float64(wsReconnects.Load()), timestamp),
		gauge("p2pquake_backfilled_events_total", base, float64(backfilledEvents.Load()), timestamp),
	}
}

func gauge(name string, labels map[string]string, value float64, timestamp int64) prompb.TimeSeries {
	merged := map[string]string{"__name__": name, "job": metricJobLabel}
	maps.Copy(merged, labels)

	names := make([]string, 0, len(merged))
	for key := range merged {
		names = append(names, key)
	}
	sort.Strings(names)

	pairs := make([]prompb.Label, 0, len(names))
	for _, key := range names {
		pairs = append(pairs, prompb.Label{Name: key, Value: merged[key]})
	}

	return prompb.TimeSeries{
		Labels:  pairs,
		Samples: []prompb.Sample{{Value: value, Timestamp: timestamp}},
	}
}

func (p *metricsPusher) remoteWrite(ctx context.Context, series []prompb.TimeSeries) error {
	request := &prompb.WriteRequest{Timeseries: series}
	raw, err := request.Marshal()
	if err != nil {
		return fmt.Errorf("marshal write request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(snappy.Encode(nil, raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.SetBasicAuth(p.username, p.password)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError{code: resp.StatusCode, body: string(body)}
	}
	return nil
}

func round(value float64, places int) float64 {
	factor := 1.0
	for range places {
		factor *= 10
	}
	return float64(int64(value*factor+0.5)) / factor
}
