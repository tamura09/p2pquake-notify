package main

import (
	"fmt"
	"sort"
	"strings"
)

// route は「どのイベントを、どのDiscord webhookへ流すか」の1本ぶんの定義です。
// 3本を並列に動かし、1本が詰まっても他が止まらないよう送信も別々に持ちます。
type route struct {
	Name       string
	WebhookURL string

	// Codes が空なら全 code を通します。
	Codes map[int]struct{}

	// MinScale は最大震度のしきい値。scaleUnknown なら判定しません。
	// 震度が付かないイベント(津波予報など)はこのしきい値の対象外です。
	MinScale int

	// Prefectures が空でなければ、いずれかに部分一致するイベントだけ通します。
	// 部分一致なのは津波予報区が「青森県太平洋沿岸」のように県名を含む
	// 長い名前で来るためで、"青森県" の指定でこれを拾えるようにしています。
	Prefectures []string

	// IncludeTest は訓練報・テスト配信を通すか。dev以外では必ず false です。
	// 訓練報を本番の通知先に流すと、いざという時に誰も信じなくなります。
	IncludeTest bool

	// Quiet は「地震そのものではない報せ」(揺れた報告とその統計)を通すか。
	// dev用ルートだけ true にします。
	Quiet bool
}

// noisyCodes は地震の発生とは限らない code。既定では通知しません。
var noisyCodes = map[int]struct{}{
	codeUserquake:           {},
	codeUserquakeEvaluation: {},
}

func (r route) matches(e *event) bool {
	// ピア分布はどのルートへも流しません。人間が読んで意味のある情報を何も
	// 含まず、地震が無くても一定間隔で届き続けるので、通知に混ぜると
	// 他の通知を押し流すだけです。
	//
	// このcodeの用途は「上流と繋がっている」ことの証明だけで、それは
	// p2pquake_last_message_age_seconds として Grafana へ押し込んでいます。
	// 目視用にdevへ流す必要はありません。
	if e.Code == codeAreaPeers {
		return false
	}

	if e.Test && !r.IncludeTest {
		return false
	}

	if _, noisy := noisyCodes[e.Code]; noisy && !r.Quiet {
		return false
	}

	if len(r.Codes) > 0 {
		if _, ok := r.Codes[e.Code]; !ok {
			return false
		}
	}

	// 震度しきい値は「震度が分かっているイベント」にだけ効かせます。
	// 津波予報や緊急地震速報の第1報は震度不明で届くことがあり、そこで
	// 落とすとしきい値の意図(小さい地震を黙らせる)と逆の結果になります。
	if r.MinScale > scaleUnknown && e.MaxScale > scaleUnknown && e.MaxScale < r.MinScale {
		return false
	}

	if len(r.Prefectures) > 0 && !matchesPrefectures(r.Prefectures, e.Prefectures) {
		return false
	}

	return true
}

func matchesPrefectures(wanted, actual []string) bool {
	for _, target := range wanted {
		for _, candidate := range actual {
			if strings.Contains(candidate, target) {
				return true
			}
		}
	}
	return false
}

func codeSet(codes ...int) map[int]struct{} {
	set := make(map[int]struct{}, len(codes))
	for _, code := range codes {
		set[code] = struct{}{}
	}
	return set
}

func (r route) String() string {
	parts := []string{fmt.Sprintf("name=%s", r.Name)}
	if len(r.Codes) == 0 {
		parts = append(parts, "codes=all")
	} else {
		codes := make([]string, 0, len(r.Codes))
		for code := range r.Codes {
			codes = append(codes, fmt.Sprintf("%d", code))
		}
		sort.Strings(codes)
		parts = append(parts, "codes="+strings.Join(codes, ","))
	}
	if r.MinScale > scaleUnknown {
		parts = append(parts, "minScale="+scaleLabel(r.MinScale))
	}
	if len(r.Prefectures) > 0 {
		parts = append(parts, "prefectures="+strings.Join(r.Prefectures, ","))
	}
	if r.IncludeTest {
		parts = append(parts, "includeTest=true")
	}
	if r.Quiet {
		parts = append(parts, "quiet=true")
	}
	return strings.Join(parts, " ")
}
