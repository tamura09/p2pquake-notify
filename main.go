package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	// タイムゾーンデータをバイナリに埋め込みます。実行イメージは distroless の
	// static で /usr/share/zoneinfo を持たないため、これが無いと
	// time.LoadLocation("Asia/Tokyo") が起動時に失敗します。
	_ "time/tzdata"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// 重複排除の保持期間と件数。履歴補完で読み直す範囲(既定20件)を十分に覆えれば
// よく、長く持ちすぎると訂正報のような「同じ地震についての正当な続報」まで
// 落としかねないので、2時間で切ります。
const (
	dedupTTL     = 2 * time.Hour
	dedupMaxSize = 2000
)

type app struct {
	parameters *ssm.Client
}

func (a *app) parameterString(ctx context.Context, name string) (string, error) {
	withDecryption := true
	out, err := a.parameters.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &name,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return "", err
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", errors.New("parameter has no value")
	}
	return *out.Parameter.Value, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	// SIGTERM は ECS がタスクを止める時に送ってきます。受け取ったら送信中の
	// リクエストを終わらせてから抜けます。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}
	application := &app{parameters: ssm.NewFromConfig(awsConfig)}

	cfg, err := loadConfig(ctx, application)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	apiClient := newAPIClient()
	sinks := make([]*sink, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		log.Printf("route configured: %s", r)
		sinks = append(sinks, newSink(r, apiClient, cfg.Zone))
	}

	// Discordへの接続を先に温めておきます。最初の1通が緊急地震速報だった場合、
	// そこでDNS解決とTLSハンドシェイクから始めると通知が数百ミリ秒遅れます。
	// 平常時に済ませられる仕事を、一番急いでいる瞬間に持ち込まないための準備です。
	warmUpDiscord(ctx, apiClient)

	pusher := &metricsPusher{
		client:   apiClient,
		interval: envDuration("METRICS_INTERVAL", time.Minute),
		started:  time.Now(),
	}
	if err := pusher.resolve(ctx, application); err != nil {
		log.Printf("metrics: disabled: %v", err)
	}

	receiver := &ingest{
		cfg:    cfg,
		client: newWebSocketClient(),
		dedup:  newDedupCache(dedupTTL, dedupMaxSize),
		sinks:  sinks,
	}

	var wg sync.WaitGroup
	for _, s := range sinks {
		wg.Add(1)
		go func(s *sink) {
			defer wg.Done()
			s.run(ctx)
		}(s)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		pusher.run(ctx)
	}()

	receiver.run(ctx)

	// 受信ループが戻るのは ctx がキャンセルされた時だけです。送信goroutineが
	// 手元のキューを片付けるのを少しだけ待ちます。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		log.Printf("shutdown: senders did not finish in time")
	}
	log.Printf("shutdown complete")
}

// newAPIClient はDiscordとGrafanaへ送るためのクライアント。接続を使い回すのが
// 目的なので、アイドル接続を長めに保持します。
func newAPIClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 4
	transport.IdleConnTimeout = 5 * time.Minute
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// newWebSocketClient はWebSocket接続専用。通常のAPIクライアントと分けるのは
// 2つの理由からです。
//
//   - Timeout を設定してはいけません。http.Client の Timeout は接続全体の寿命に
//     効くので、張りっぱなしにしたい接続がその時間で必ず切られます。
//   - HTTP/2 を無効にします。WebSocketのハンドシェイクは HTTP/1.1 の Upgrade で、
//     ALPN で h2 が選ばれると成立しません。
func newWebSocketClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &http.Client{Transport: transport}
}

// warmUpDiscord はDNS解決とTLSハンドシェイクを事前に済ませます。認証の要らない
// 公開エンドポイントを1回叩くだけで、以後の webhook POST は同じホストへの
// アイドル接続を再利用できます。失敗しても実害はないのでログだけ残します。
func warmUpDiscord(ctx context.Context, client *http.Client) {
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(warmCtx, http.MethodGet, "https://discord.com/api/v10/gateway", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("warm-up request to discord.com failed (continuing): %v", err)
		return
	}
	defer resp.Body.Close()
	// 本文を読み切らないとこの接続は再利用されず、温めた意味がなくなります。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
}

// resolve は Grafana Cloud の接続情報を SSM から読みます。URLのパラメータ名が
// 未設定ならメトリクス送信は無効のままにします(ローカル実行用)。
func (p *metricsPusher) resolve(ctx context.Context, resolver parameterResolver) error {
	urlParameter := strings.TrimSpace(os.Getenv("GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME"))
	if urlParameter == "" {
		return errors.New("GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME is not set")
	}
	usernameParameter := strings.TrimSpace(os.Getenv("GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME"))
	tokenParameter := strings.TrimSpace(os.Getenv("GRAFANA_PUSH_TOKEN_PARAMETER_NAME"))
	if usernameParameter == "" || tokenParameter == "" {
		return errors.New("GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME and GRAFANA_PUSH_TOKEN_PARAMETER_NAME must both be set")
	}

	var err error
	if p.url, err = resolver.parameterString(ctx, urlParameter); err != nil {
		return err
	}
	if p.username, err = resolver.parameterString(ctx, usernameParameter); err != nil {
		return err
	}
	if p.password, err = resolver.parameterString(ctx, tokenParameter); err != nil {
		return err
	}
	return nil
}
