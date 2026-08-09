package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWebSocketURL = "wss://api.p2pquake.net/v2/ws"
	defaultHistoryURL   = "https://api.p2pquake.net/v2/history"

	// サンドボックスはダミーの地震を流し続けてくれるので、本物の地震を待たずに
	// 通知の見た目とルーティングを確認できます。デプロイ前の動作確認用。
	sandboxWebSocketURL = "wss://api-realtime-sandbox.p2pquake.net/v2/ws"
)

// parameterResolver は SSM Parameter Store から値を読む口。テストで差し替えます。
type parameterResolver interface {
	parameterString(ctx context.Context, name string) (string, error)
}

type config struct {
	WebSocketURL string
	HistoryURL   string
	Routes       []route

	// Zone は上流の時刻表記を解釈するタイムゾーン。P2P地震情報はJST固定です。
	Zone *time.Location

	// BackfillLimit は再接続時に /v2/history から読み直す件数。
	// 0 なら履歴補完をしません。
	BackfillLimit int

	// StaleAfter はこの時間まったくメッセージが来なければ接続が死んだとみなす閾値。
	// code 555(ピア分布)が定期的に流れてくる前提に依存しています。
	StaleAfter time.Duration
}

// loadConfig は環境変数を読み、webhook URLだけ SSM から解決します。
//
// webhook URL を環境変数に直接置かず SSM SecureString 参照にしているのは、
// この値が漏れると誰でも通知先へ書き込めてしまうためです。ECSのタスク定義は
// コンソールからもAPIからも平文で読めるので、値そのものを置く場所ではありません。
func loadConfig(ctx context.Context, resolver parameterResolver) (config, error) {
	zone, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return config{}, fmt.Errorf("load Asia/Tokyo: %w", err)
	}

	cfg := config{
		WebSocketURL:  envOr("P2PQUAKE_WS_URL", defaultWebSocketURL),
		HistoryURL:    envOr("P2PQUAKE_HISTORY_URL", defaultHistoryURL),
		Zone:          zone,
		BackfillLimit: envInt("P2PQUAKE_BACKFILL_LIMIT", 20),
		StaleAfter:    envDuration("P2PQUAKE_STALE_AFTER", 10*time.Minute),
	}

	if _, err := url.Parse(cfg.WebSocketURL); err != nil {
		return config{}, fmt.Errorf("parse P2PQUAKE_WS_URL: %w", err)
	}

	// ルートは3本とも任意。webhookのパラメータ名が未設定なら、そのルートは
	// 単に無効になります。全部未設定なら通知先が無いので起動を止めます
	// (通知先ゼロで動き続けるのは、動いているように見えて何も届かない
	//  一番たちの悪い壊れ方なので、起動時に落とします)。
	definitions := []struct {
		name        string
		env         string
		build       func(url string) route
		description string
	}{
		{
			name: "dev",
			env:  "P2PQUAKE_DEV_WEBHOOK_PARAMETER_NAME",
			build: func(webhook string) route {
				return route{
					Name:        "dev",
					WebhookURL:  webhook,
					MinScale:    scaleUnknown,
					IncludeTest: true,
					Quiet:       true,
				}
			},
		},
		{
			name: "alert",
			env:  "P2PQUAKE_ALERT_WEBHOOK_PARAMETER_NAME",
			build: func(webhook string) route {
				return route{
					Name:       "alert",
					WebhookURL: webhook,
					Codes:      codeSet(codeEEW, codeJMATsunami),
					MinScale:   scaleUnknown,
				}
			},
		},
		{
			name: "local",
			env:  "P2PQUAKE_LOCAL_WEBHOOK_PARAMETER_NAME",
			build: func(webhook string) route {
				return route{
					Name:       "local",
					WebhookURL: webhook,
					// 554(揺れ検知)は地域情報を持たないので、地域フィルタと
					// 併用しても決して一致しません。ここには入れません。
					Codes:       codeSet(codeJMAQuake, codeJMATsunami, codeEEW),
					MinScale:    envInt("P2PQUAKE_LOCAL_MIN_SCALE", scaleUnknown),
					Prefectures: envList("P2PQUAKE_LOCAL_PREFECTURES", []string{"岩手県"}),
				}
			},
		},
	}

	for _, definition := range definitions {
		parameterName := strings.TrimSpace(os.Getenv(definition.env))
		if parameterName == "" {
			continue
		}
		value, err := resolver.parameterString(ctx, parameterName)
		if err != nil {
			return config{}, fmt.Errorf("read %s (%s): %w", definition.env, parameterName, err)
		}
		webhook, err := validateWebhookURL(value)
		if err != nil {
			return config{}, fmt.Errorf("%s (%s): %w", definition.env, parameterName, err)
		}
		cfg.Routes = append(cfg.Routes, definition.build(webhook))
	}

	if len(cfg.Routes) == 0 {
		return config{}, errors.New("no routes configured: set at least one of P2PQUAKE_{DEV,ALERT,LOCAL}_WEBHOOK_PARAMETER_NAME")
	}

	return cfg, nil
}

// validateWebhookURL は Discord の webhook URL であることを確認します。
// SSMのパラメータを取り違えた場合、ここで落とさないと「送っているのに
// 届かない」という切り分けの難しい状態になります。
func validateWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("webhook URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse webhook URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("webhook URL must be https, got %q", parsed.Scheme)
	}
	if !strings.HasSuffix(parsed.Host, "discord.com") && !strings.HasSuffix(parsed.Host, "discordapp.com") {
		return "", fmt.Errorf("webhook URL host %q is not Discord", parsed.Host)
	}
	return raw, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envList(name string, fallback []string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parts := strings.Split(value, ",")
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			list = append(list, trimmed)
		}
	}
	if len(list) == 0 {
		return fallback
	}
	return list
}
