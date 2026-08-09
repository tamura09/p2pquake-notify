package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Discord の埋め込みには文字数上限があり、超えると 400 で丸ごと弾かれます。
// 大地震では各地の震度が数百地点ぶん来るので、必ず切り詰めてから送ります。
const (
	discordEmbedTotalLimit       = 6000
	discordEmbedFieldLimit       = 25
	discordEmbedTitleLimit       = 256
	discordEmbedDescriptionLimit = 4096
	discordEmbedFieldNameLimit   = 256
	discordEmbedFieldValueLimit  = 1024
	discordContentLimit          = 2000
)

// 震度と警報の色。スマホの通知一覧では色帯が最初に目に入るので、
// 深刻さの順に赤へ寄せています。
const (
	colorEEW      = 0xE03131 // 緊急地震速報(警報)
	colorTsunami  = 0xC2255C // 津波警報・注意報
	colorSevere   = 0xF76707 // 震度5弱以上
	colorModerate = 0xF59F00 // 震度3〜4
	colorInfo     = 0x4C6EF5 // 震度1〜2、その他の地震情報
	colorMuted    = 0x868E96 // 地震の発生とは限らないもの、未対応のcode
)

type discordWebhookPayload struct {
	Content         string                 `json:"content,omitempty"`
	Username        string                 `json:"username,omitempty"`
	Embeds          []discordEmbed         `json:"embeds,omitempty"`
	AllowedMentions *discordAllowedMention `json:"allowed_mentions,omitempty"`
}

// discordAllowedMention を明示的に空で送り、通知本文に "@everyone" のような
// 文字列がそのまま入っていても実際のメンションにはしないようにします。
// 本文は上流のJSON由来なので、こちらで制御できない文字列です。
type discordAllowedMention struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Footer      *discordEmbedFooter `json:"footer,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

func noMentions() *discordAllowedMention {
	return &discordAllowedMention{Parse: []string{}}
}

// renderPayload はイベントをDiscordのペイロードに変換します。
func renderPayload(e *event, zone *time.Location) discordWebhookPayload {
	switch payload := e.Payload.(type) {
	case eew:
		return renderEEW(payload, zone)
	case jmaTsunami:
		return renderTsunami(payload, zone)
	case jmaQuake:
		return renderQuake(payload, zone)
	case eewDetection:
		return renderEEWDetection(payload)
	default:
		return renderUnknown(e)
	}
}

func renderEEW(message eew, zone *time.Location) discordWebhookPayload {
	serial := strings.TrimSpace(message.Issue.Serial)
	title := "緊急地震速報(警報)"
	switch {
	case message.Cancelled:
		title = "緊急地震速報(警報) 取消"
	case serial != "":
		title = fmt.Sprintf("緊急地震速報(警報) 第%s報", serial)
	}
	if message.Test {
		title = "【訓練】" + title
	}

	hypocenter := message.Earthquake.Hypocenter
	name := firstNonEmpty(hypocenter.Name, hypocenter.ReduceName, "震源不明")

	maxScale := scaleUnknown
	for _, area := range message.Areas {
		for _, scale := range []int{area.ScaleTo, area.ScaleFrom} {
			if scale > maxScale {
				maxScale = scale
			}
		}
	}

	embed := discordEmbed{
		Title: truncate(title, discordEmbedTitleLimit),
		Color: colorEEW,
	}

	if message.Cancelled {
		embed.Description = "先ほどの緊急地震速報は取り消されました。"
	} else {
		embed.Description = fmt.Sprintf("**%s** で地震\n予想最大%s", name, scaleLabel(maxScale))
	}

	fields := []discordEmbedField{}
	if !message.Cancelled {
		fields = append(fields,
			discordEmbedField{Name: "規模", Value: magnitudeLabel(hypocenter.Magnitude), Inline: true},
			discordEmbedField{Name: "深さ", Value: depthLabel(hypocenter.Depth), Inline: true},
			discordEmbedField{Name: "発生時刻", Value: timeLabel(message.Earthquake.OriginTime, zone), Inline: true},
		)
		if message.Earthquake.Condition != "" {
			// 「仮定震源要素」= PLUM法などによる暫定値。数値を鵜呑みにすると
			// 誤解を招くので、付いている時は必ず見せます。
			fields = append(fields, discordEmbedField{
				Name:  "条件",
				Value: truncate(message.Earthquake.Condition, discordEmbedFieldValueLimit),
			})
		}
		fields = append(fields, eewAreaFields(message.Areas)...)
	}
	embed.Fields = fitFields(&embed, fields)
	embed.Footer = &discordEmbedFooter{Text: truncate(eewFooter(message), 2048)}

	content := ""
	if !message.Cancelled && !message.Test {
		// 埋め込みはスマホのプッシュ通知プレビューに出ないことがあるので、
		// 一番大事な一行だけ本文にも置きます。
		content = fmt.Sprintf("🚨 緊急地震速報 %s 予想最大%s", name, scaleLabel(maxScale))
	}

	return discordWebhookPayload{
		Content:         truncate(content, discordContentLimit),
		Embeds:          []discordEmbed{embed},
		AllowedMentions: noMentions(),
	}
}

// eewAreaFields は予想震度ごとに地域をまとめます。地域を1つずつフィールドに
// すると25個の上限をすぐ超えるので、震度でグループ化しています。
func eewAreaFields(areas []eewArea) []discordEmbedField {
	grouped := map[string][]string{}
	order := []string{}
	for _, area := range areas {
		label := scaleRangeLabel(area.ScaleFrom, area.ScaleTo)
		if _, ok := grouped[label]; !ok {
			order = append(order, label)
		}
		grouped[label] = append(grouped[label], firstNonEmpty(area.Name, area.Pref))
	}
	sort.SliceStable(order, func(i, j int) bool {
		return scaleRank(order[i]) > scaleRank(order[j])
	})

	fields := make([]discordEmbedField, 0, len(order))
	for _, label := range order {
		fields = append(fields, discordEmbedField{
			Name:  truncate(label, discordEmbedFieldNameLimit),
			Value: truncate(strings.Join(uniqueStrings(grouped[label]), "、"), discordEmbedFieldValueLimit),
		})
	}
	return fields
}

// scaleRank は「震度5強〜6弱」のようなラベルを並べ替えるための粗い順位。
// ラベルから震度を引き直すより、既知のラベルを順序付きで持つほうが確実です。
var scaleOrder = []string{"震度不明", "震度0", "震度1", "震度2", "震度3", "震度4", "震度5弱", "震度5強", "震度6弱", "震度6強", "震度7"}

func scaleRank(label string) int {
	rank := 0
	for index, known := range scaleOrder {
		if strings.HasPrefix(label, known) {
			rank = index
		}
	}
	return rank
}

func eewFooter(message eew) string {
	parts := []string{}
	if message.Issue.EventID != "" {
		parts = append(parts, "EventID "+message.Issue.EventID)
	}
	if message.Issue.Time != "" {
		parts = append(parts, "発表 "+message.Issue.Time)
	}
	parts = append(parts, "P2P地震情報 (code 556)")
	return strings.Join(parts, " / ")
}

func renderTsunami(message jmaTsunami, zone *time.Location) discordWebhookPayload {
	title := "津波予報"
	description := ""
	if message.Cancelled {
		title = "津波予報 解除"
		description = "津波予報は解除されました。"
	}

	embed := discordEmbed{
		Title: truncate(title, discordEmbedTitleLimit),
		Color: colorTsunami,
	}

	// 予報区を等級ごとにまとめます。大津波警報と注意報が混在するので、
	// 等級を見出しにしないとどこが危ないのか読み取れません。
	grouped := map[string][]string{}
	order := []string{}
	immediate := false
	for _, area := range message.Areas {
		label := tsunamiGradeLabel(area.Grade)
		if _, ok := grouped[label]; !ok {
			order = append(order, label)
		}
		entry := area.Name
		if area.MaxHeight != nil && area.MaxHeight.Description != "" {
			entry += fmt.Sprintf("(%s)", area.MaxHeight.Description)
		}
		grouped[label] = append(grouped[label], entry)
		if area.Immediate {
			immediate = true
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return tsunamiGradeRank(order[i]) > tsunamiGradeRank(order[j])
	})

	if description == "" && immediate {
		description = "**直ちに津波が来襲すると予想されます。**"
	}
	embed.Description = truncate(description, discordEmbedDescriptionLimit)

	fields := make([]discordEmbedField, 0, len(order))
	for _, label := range order {
		fields = append(fields, discordEmbedField{
			Name:  truncate(label, discordEmbedFieldNameLimit),
			Value: truncate(strings.Join(grouped[label], "、"), discordEmbedFieldValueLimit),
		})
	}
	embed.Fields = fitFields(&embed, fields)
	embed.Timestamp = rfc3339(message.Issue.Time, zone)
	embed.Footer = &discordEmbedFooter{Text: fmt.Sprintf("%s / P2P地震情報 (code 552)", firstNonEmpty(message.Issue.Source, "気象庁"))}

	content := ""
	if !message.Cancelled && len(order) > 0 {
		content = "🌊 " + order[0] + " " + strings.Join(grouped[order[0]], "、")
	}

	return discordWebhookPayload{
		Content:         truncate(content, discordContentLimit),
		Embeds:          []discordEmbed{embed},
		AllowedMentions: noMentions(),
	}
}

var tsunamiGradeOrder = map[string]int{"大津波警報": 3, "津波警報": 2, "津波注意報": 1}

func tsunamiGradeRank(label string) int { return tsunamiGradeOrder[label] }

func renderQuake(message jmaQuake, zone *time.Location) discordWebhookPayload {
	hypocenter := message.Earthquake.Hypocenter
	name := firstNonEmpty(hypocenter.Name, "震源調査中")

	embed := discordEmbed{
		Title: truncate(fmt.Sprintf("地震情報 (%s)", quakeIssueTypeLabel(message.Issue.Type)), discordEmbedTitleLimit),
		Color: quakeColor(message.Earthquake.MaxScale),
		Description: truncate(fmt.Sprintf("**%s** で地震\n最大%s",
			name, scaleLabel(message.Earthquake.MaxScale)), discordEmbedDescriptionLimit),
		Timestamp: rfc3339(message.Earthquake.Time, zone),
	}

	fields := []discordEmbedField{
		{Name: "規模", Value: magnitudeLabel(hypocenter.Magnitude), Inline: true},
		{Name: "深さ", Value: depthLabel(hypocenter.Depth), Inline: true},
		{Name: "津波", Value: domesticTsunamiLabel(message.Earthquake.DomesticTsunami), Inline: true},
	}
	if message.Issue.Correct != "" && message.Issue.Correct != "None" {
		fields = append(fields, discordEmbedField{Name: "訂正", Value: message.Issue.Correct})
	}
	fields = append(fields, quakePointFields(message.Points)...)

	embed.Fields = fitFields(&embed, fields)
	embed.Footer = &discordEmbedFooter{
		Text: fmt.Sprintf("%s / P2P地震情報 (code 551)", firstNonEmpty(message.Issue.Source, "気象庁")),
	}

	return discordWebhookPayload{
		Embeds:          []discordEmbed{embed},
		AllowedMentions: noMentions(),
	}
}

// quakePointFields は各地の震度を震度別にまとめ、強い順に並べます。
// 大地震では観測点が数百に達するので、フィールド化する震度は上から4段階までに
// 絞ります。震度1・2まで全部載せても埋め込み上限で切り捨てられるだけです。
func quakePointFields(points []quakePoint) []discordEmbedField {
	if len(points) == 0 {
		return nil
	}

	grouped := map[int][]string{}
	for _, point := range points {
		label := point.Addr
		if label == "" {
			label = point.Pref
		}
		grouped[point.Scale] = append(grouped[point.Scale], label)
	}

	scales := make([]int, 0, len(grouped))
	for scale := range grouped {
		scales = append(scales, scale)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(scales)))
	if len(scales) > 4 {
		scales = scales[:4]
	}

	fields := make([]discordEmbedField, 0, len(scales))
	for _, scale := range scales {
		names := uniqueStrings(grouped[scale])
		value := strings.Join(names, "、")
		// 切り詰めで末尾が欠けるより、件数を添えて省略したと分かるほうが親切です。
		// 余白は接尾辞の実バイト数から引きます。日本語の接尾辞は1文字3バイトなので、
		// 文字数で見積もると上限を超えてDiscordに丸ごと拒否されます。
		if len(value) > discordEmbedFieldValueLimit {
			suffix := fmt.Sprintf(" ほか (計%d地点)", len(names))
			value = truncate(value, discordEmbedFieldValueLimit-len(suffix)) + suffix
		}
		fields = append(fields, discordEmbedField{
			Name:  scaleLabel(scale),
			Value: value,
		})
	}
	return fields
}

func quakeColor(maxScale int) int {
	switch {
	case maxScale >= scale5Lower:
		return colorSevere
	case maxScale >= scale3:
		return colorModerate
	default:
		return colorInfo
	}
}

func renderEEWDetection(message eewDetection) discordWebhookPayload {
	return discordWebhookPayload{
		Embeds: []discordEmbed{{
			Title: "緊急地震速報を検知",
			// 気象庁の発表ではないことを毎回書きます。これを省くと
			// 誤検知のたびに本物の速報と取り違えられます。
			Description: fmt.Sprintf("P2P地震情報のクライアントが緊急地震速報を検知しました (type: %s)。\n気象庁の発表そのものではなく、誤検知の可能性があります。", firstNonEmpty(message.Type, "unknown")),
			Color:       colorModerate,
			Footer:      &discordEmbedFooter{Text: "P2P地震情報 (code 554)"},
		}},
		AllowedMentions: noMentions(),
	}
}

// ピア分布(code 555)に描画はありません。どのルートへも流さないためです
// (理由は route.matches を参照)。

// renderUnknown は知らない code のための最後の砦。dev用ルートにだけ流れます。
// 上流がメッセージ形式を増やしたとき、ここに出てくることで気付けます。
func renderUnknown(e *event) discordWebhookPayload {
	body := string(e.Raw)
	return discordWebhookPayload{
		Embeds: []discordEmbed{{
			Title:       fmt.Sprintf("未対応のメッセージ (code %d)", e.Code),
			Description: truncate("```json\n"+body+"\n```", discordEmbedDescriptionLimit),
			Color:       colorMuted,
			Footer:      &discordEmbedFooter{Text: "P2P地震情報"},
		}},
		AllowedMentions: noMentions(),
	}
}

// fitFields は埋め込み全体の文字数上限に収まるぶんだけフィールドを残します。
// 上限を超えたペイロードは 400 で丸ごと拒否されるため、「全部送れないなら
// 一部だけでも送る」ほうが確実に価値があります。
func fitFields(embed *discordEmbed, fields []discordEmbedField) []discordEmbedField {
	used := len(embed.Title) + len(embed.Description)
	if embed.Footer != nil {
		used += len(embed.Footer.Text)
	}

	fitted := make([]discordEmbedField, 0, len(fields))
	for _, field := range fields {
		if len(fitted) >= discordEmbedFieldLimit {
			break
		}
		cost := len(field.Name) + len(field.Value)
		if used+cost > discordEmbedTotalLimit {
			break
		}
		used += cost
		fitted = append(fitted, field)
	}
	return fitted
}

func magnitudeLabel(magnitude float64) string {
	// 未知のマグニチュードは -1 で来ます。0.0 と表示すると
	// 「非常に小さい地震」に見えてしまうので区別します。
	if magnitude < 0 {
		return "不明"
	}
	return fmt.Sprintf("M%.1f", magnitude)
}

func depthLabel(depth int) string {
	switch {
	case depth < 0:
		return "不明"
	case depth == 0:
		return "ごく浅い"
	default:
		return fmt.Sprintf("%dkm", depth)
	}
}

func timeLabel(value string, zone *time.Location) string {
	if parsed, ok := parseP2PTime(value, zone); ok {
		return parsed.Format("15:04:05")
	}
	if value == "" {
		return "不明"
	}
	return value
}

// rfc3339 は埋め込みのtimestampフィールド用。Discordは閲覧者のタイムゾーンで
// 表示してくれるので、JSTのまま文字列で入れるより読み手に優しくなります。
func rfc3339(value string, zone *time.Location) string {
	parsed, ok := parseP2PTime(value, zone)
	if !ok {
		return ""
	}
	return parsed.Format(time.RFC3339)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// truncate はUTF-8の途中で切らないよう、ルーン境界で丸めます。
func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	const ellipsis = "…"
	budget := limit - len(ellipsis)
	if budget <= 0 {
		return ""
	}
	runes := []rune(value)
	size := 0
	for index, r := range runes {
		width := len(string(r))
		if size+width > budget {
			return string(runes[:index]) + ellipsis
		}
		size += width
	}
	return value
}
