package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testZone() *time.Location { return time.FixedZone("JST", 9*60*60) }

func TestRenderEEWLeadsWithTheEssentials(t *testing.T) {
	e := decodeOrFail(t, eewIwateSerial1)
	payload := renderPayload(e, testZone())

	// 埋め込みはスマホのプッシュ通知プレビューに出ないことがあるので、
	// 震源と予想最大震度は本文にも置きます。
	if !strings.Contains(payload.Content, "三陸沖") || !strings.Contains(payload.Content, "震度5強") {
		t.Errorf("Content = %q, want the hypocenter and expected max scale", payload.Content)
	}

	if len(payload.Embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(payload.Embeds))
	}
	embed := payload.Embeds[0]
	if embed.Title != "緊急地震速報(警報) 第1報" {
		t.Errorf("Title = %q", embed.Title)
	}
	if embed.Color != colorEEW {
		t.Errorf("Color = %#x, want %#x", embed.Color, colorEEW)
	}

	// 予想震度は範囲で表示します(45〜50 → 「震度5弱〜5強」)。
	if !hasField(embed, "震度5弱〜5強", "岩手県沿岸北部") {
		t.Errorf("expected a 震度5弱〜5強 field listing 岩手県沿岸北部; got %+v", embed.Fields)
	}
}

func TestRenderTrainingReportIsLabelled(t *testing.T) {
	e := decodeOrFail(t, eewTraining)
	payload := renderPayload(e, testZone())

	if !strings.HasPrefix(payload.Embeds[0].Title, "【訓練】") {
		t.Errorf("Title = %q, want a 訓練 prefix", payload.Embeds[0].Title)
	}
	// 訓練報でスマホを鳴らさないよう、本文は空にします。
	if payload.Content != "" {
		t.Errorf("Content = %q, want empty for a training report", payload.Content)
	}
}

func TestRenderTsunamiOrdersBySeverity(t *testing.T) {
	e := decodeOrFail(t, tsunamiIwate)
	payload := renderPayload(e, testZone())
	embed := payload.Embeds[0]

	if len(embed.Fields) < 2 {
		t.Fatalf("got %d fields, want at least 2", len(embed.Fields))
	}
	// 大津波警報が注意報より先に来ないと、どこが危ないのか読み取れません。
	if embed.Fields[0].Name != "大津波警報" {
		t.Errorf("first field = %q, want 大津波警報", embed.Fields[0].Name)
	}
	if !strings.Contains(embed.Description, "直ちに") {
		t.Errorf("Description = %q, want the immediate-arrival warning", embed.Description)
	}
}

// 色帯のしきい値。震度3では色を変えず、震度4から変わります。境目は
// 見た目だけの話に見えて、通知一覧で最初に目に入る情報なので固定します。
func TestQuakeColourThresholds(t *testing.T) {
	cases := []struct {
		scale int
		want  int
		name  string
	}{
		{scaleUnknown, colorInfo, "unknown"},
		{scale1, colorInfo, "震度1"},
		{scale3, colorInfo, "震度3"},
		{scale4, colorModerate, "震度4"},
		{scale5Lower, colorSevere, "震度5弱"},
		{scale7, colorSevere, "震度7"},
	}
	for _, tc := range cases {
		if got := quakeColor(tc.scale); got != tc.want {
			t.Errorf("quakeColor(%s) = %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

func TestRenderQuakeGroupsPointsByScale(t *testing.T) {
	e := decodeOrFail(t, quakeTokyo)
	embed := renderPayload(e, testZone()).Embeds[0]

	if !hasField(embed, "震度3", "千代田区") {
		t.Errorf("expected 千代田区 under 震度3; got %+v", embed.Fields)
	}
	if !hasField(embed, "規模", "M4.2") {
		t.Errorf("expected the magnitude field; got %+v", embed.Fields)
	}
	// 地震情報は速報ではないので、スマホを鳴らす本文は付けません。
	if renderPayload(e, testZone()).Content != "" {
		t.Error("Content should be empty for a 地震情報")
	}
}

// 大地震では観測点が数百に達します。上限を超えたペイロードは400で丸ごと
// 拒否されるので、「全部送れないなら一部だけでも送る」ことを確認します。
func TestLargeQuakeStaysWithinDiscordLimits(t *testing.T) {
	points := make([]quakePoint, 0, 600)
	for index := range 600 {
		points = append(points, quakePoint{
			Pref:  "岩手県",
			Addr:  fmt.Sprintf("架空市第%d観測点", index),
			Scale: []int{scale7, scale6Upper, scale6Lower, scale5Upper, scale4, scale3}[index%6],
		})
	}
	message := jmaQuake{
		Earthquake: quakeDetail{
			Hypocenter:      hypocenter{Name: "三陸沖", Depth: 30, Magnitude: 9.0},
			MaxScale:        scale7,
			DomesticTsunami: "Warning",
		},
		Issue:  quakeIssue{Source: "気象庁", Type: "DetailScale"},
		Points: points,
	}

	embed := renderQuake(message, testZone()).Embeds[0]

	if len(embed.Fields) > discordEmbedFieldLimit {
		t.Errorf("got %d fields, want at most %d", len(embed.Fields), discordEmbedFieldLimit)
	}
	total := len(embed.Title) + len(embed.Description)
	if embed.Footer != nil {
		total += len(embed.Footer.Text)
	}
	for _, field := range embed.Fields {
		if len(field.Value) > discordEmbedFieldValueLimit {
			t.Errorf("field %q value is %d bytes, over the %d limit", field.Name, len(field.Value), discordEmbedFieldValueLimit)
		}
		total += len(field.Name) + len(field.Value)
	}
	if total > discordEmbedTotalLimit {
		t.Errorf("embed totals %d bytes, over the %d limit", total, discordEmbedTotalLimit)
	}
	// 切り詰めても一番強い震度は残っていなければ意味がありません。
	// 弱い震度から順に落とすので、震度7は必ず生き残ります。
	if !hasField(embed, "震度7", "架空市") {
		t.Errorf("震度7 was dropped from a truncated embed; got %+v", fieldNames(embed))
	}
	if hasField(embed, "震度3", "架空市") {
		t.Errorf("震度3 survived; weak scales should be dropped first, got %+v", fieldNames(embed))
	}
}

// 通知本文は上流のJSON由来で、こちらでは中身を選べません。"@everyone" が
// 紛れ込んでも実際のメンションにならないことを確かめます。
func TestPayloadsSuppressMentions(t *testing.T) {
	for _, raw := range []string{eewIwateSerial1, tsunamiIwate, quakeTokyo, areaPeersMessage} {
		payload := renderPayload(decodeOrFail(t, raw), testZone())
		if payload.AllowedMentions == nil || len(payload.AllowedMentions.Parse) != 0 {
			t.Errorf("AllowedMentions = %+v, want an empty parse list", payload.AllowedMentions)
		}
	}
}

func TestRenderUnknownCodeShowsRawJSON(t *testing.T) {
	e := decodeOrFail(t, `{"code": 12345, "time": "2024/01/01 00:00:00.000"}`)
	embed := renderPayload(e, testZone()).Embeds[0]

	if !strings.Contains(embed.Title, "12345") {
		t.Errorf("Title = %q, want the unknown code", embed.Title)
	}
	if !strings.Contains(embed.Description, `"code"`) {
		t.Errorf("Description = %q, want the raw JSON", embed.Description)
	}
}

func TestUnknownValuesAreNotShownAsZero(t *testing.T) {
	// マグニチュード -1 を "M0.0" と出すと、非常に小さい地震に見えます。
	if got := magnitudeLabel(-1); got != "不明" {
		t.Errorf("magnitudeLabel(-1) = %q, want 不明", got)
	}
	if got := depthLabel(-1); got != "不明" {
		t.Errorf("depthLabel(-1) = %q, want 不明", got)
	}
	if got := depthLabel(0); got != "ごく浅い" {
		t.Errorf("depthLabel(0) = %q, want ごく浅い", got)
	}
	if got := scaleLabel(scaleUnknown); got != "震度不明" {
		t.Errorf("scaleLabel(unknown) = %q", got)
	}
}

func TestTruncateDoesNotSplitMultibyteRunes(t *testing.T) {
	// 日本語を途中で切ると壊れた文字が残り、Discordの検証で弾かれることがあります。
	truncated := truncate(strings.Repeat("岩", 100), 40)
	if len(truncated) > 40 {
		t.Errorf("truncate returned %d bytes, over the 40 limit", len(truncated))
	}
	if !json.Valid([]byte(fmt.Sprintf("%q", truncated))) {
		t.Errorf("truncate produced invalid UTF-8: %q", truncated)
	}
	if !strings.HasSuffix(truncated, "…") {
		t.Errorf("truncate = %q, want an ellipsis to mark the cut", truncated)
	}

	// 上限に収まる文字列はそのまま返します。
	if got := truncate("岩手県", 40); got != "岩手県" {
		t.Errorf("truncate(short) = %q", got)
	}
}

func TestScaleRangeLabel(t *testing.T) {
	cases := []struct {
		from, to int
		want     string
	}{
		{scale5Lower, scale5Upper, "震度5弱〜5強"},
		{scale4, scale4, "震度4"},
		{scale4, scaleUnknown, "震度4"},
		{scaleUnknown, scaleUnknown, "震度不明"},
	}
	for _, tc := range cases {
		if got := scaleRangeLabel(tc.from, tc.to); got != tc.want {
			t.Errorf("scaleRangeLabel(%d, %d) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func fieldNames(embed discordEmbed) []string {
	names := make([]string, 0, len(embed.Fields))
	for _, field := range embed.Fields {
		names = append(names, field.Name)
	}
	return names
}

func hasField(embed discordEmbed, name, wantValueSubstring string) bool {
	for _, field := range embed.Fields {
		if field.Name == name && strings.Contains(field.Value, wantValueSubstring) {
			return true
		}
	}
	return false
}
