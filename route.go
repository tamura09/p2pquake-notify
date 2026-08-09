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
}

// neverNotified はどのルートへも流さない code。
//
// いずれもP2P地震情報のネットワーク内部の様子であって、気象庁の発表ではありません。
// 3つとも「1件が来る」ではなく「秒単位で来続ける」性質を持っていて、通知に混ぜると
// 本当に読みたい地震情報を押し流します。
//
//   - 555 ピア分布: 地域ごとの接続台数。地震の有無に関係なく一定間隔で届きます。
//     用途は「上流と繋がっている」ことの証明だけで、それは
//     p2pquake_last_message_age_seconds として Grafana に出ています。
//   - 561 揺れた報告: 利用者1人が「揺れた」と押した、という1点の記録。単体では
//     何も意味せず、少し揺れただけで数十件が数秒のうちに流れます。
//   - 9611 揺れた報告の統計: 561を集計した推定値。1回の地震のあいだ中、
//     数値が更新されるたびに新しいメッセージとして届き続けます。
//
// 561と9611に描画を用意していないのもこのためです。通知しないと決めたものに
// 見た目を与える理由がありません。
var neverNotified = map[int]struct{}{
	codeAreaPeers:           {},
	codeUserquake:           {},
	codeUserquakeEvaluation: {},
}

func (r route) matches(e *event) bool {
	if _, blocked := neverNotified[e.Code]; blocked {
		return false
	}

	if e.Test && !r.IncludeTest {
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
	return strings.Join(parts, " ")
}
