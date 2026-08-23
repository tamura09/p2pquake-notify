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
	colorModerate = 0xF59F00 // 震度4
	colorInfo     = 0x4C6EF5 // 震度3以下、その他の地震情報
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
// 観測点名(「つくばみらい市加藤」)をそのまま並べると同じ市が何度も出てきて
// どこが揺れたのか読み取れないので、市区町村まで丸めて重複を畳み、
// 都道府県ごとの入れ子箇条書きにします。
// 大地震では観測点が数百に達するので、フィールド化する震度は上から4段階までに
// 絞ります。震度1・2まで全部載せても埋め込み上限で切り捨てられるだけです。
func quakePointFields(points []quakePoint) []discordEmbedField {
	if len(points) == 0 {
		return nil
	}

	grouped := map[int][]quakePoint{}
	for _, point := range points {
		grouped[point.Scale] = append(grouped[point.Scale], point)
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
		fields = append(fields, discordEmbedField{
			Name:  scaleLabel(scale),
			Value: municipalityList(grouped[scale], discordEmbedFieldValueLimit),
		})
	}
	return fields
}

// prefGroup は1つの都道府県ぶんの市区町村。気象庁の並び(北から南)をそのまま
// 見せたいので、mapではなくスライスで順序を持ちます。
type prefGroup struct {
	pref  string
	areas []string
}

// groupPointsByPref は観測点を都道府県ごとの市区町村一覧に畳みます。
// 同じ市の観測点が何十個あっても1行にまとめるのが目的です。
func groupPointsByPref(points []quakePoint) []prefGroup {
	groups := []prefGroup{}
	position := map[string]int{}
	seen := map[string]bool{}

	for _, point := range points {
		area := pointArea(point)
		if area == "" {
			continue
		}
		// 都道府県が無いのは上流の欠損。捨てると地点ごと消えてしまうので、
		// 見出しだけ「その他」にして残します。
		pref := firstNonEmpty(point.Pref, "その他")
		if seen[pref+"\x00"+area] {
			continue
		}
		seen[pref+"\x00"+area] = true

		index, ok := position[pref]
		if !ok {
			index = len(groups)
			position[pref] = index
			groups = append(groups, prefGroup{pref: pref})
		}
		groups[index].areas = append(groups[index].areas, area)
	}
	return groups
}

// municipalityIndent は都道府県の見出しにぶら下がる市区町村の行頭。
const municipalityIndent = "  "

// municipalityList は都道府県ごとの箇条書きを組み立てます。県を見出しにして、
// その県の市区町村は1行に並べます。1件1行にすると数十行に伸びて、
// スマホでは1つの震度を見るだけでスクロールが必要になるためです。
// 畳んでもなお上限に収まらないことがある(震度2・3は数百地点)ので、
// その場合は打ち切って残り件数を添え、省略したと分かるようにします。
func municipalityList(points []quakePoint, limit int) string {
	groups := groupPointsByPref(points)
	if len(groups) == 0 {
		return ""
	}

	if value, dropped := buildMunicipalityList(groups, limit); dropped == 0 {
		return value
	}
	// 注記ぶんの余白を空けて詰め直します。残り件数は総数以下なので、
	// 総数で見積もっておけば注記が入りきらなくなることはありません。
	total := 0
	for _, group := range groups {
		total += len(group.areas)
	}
	value, dropped := buildMunicipalityList(groups, limit-len(municipalityOverflowNote(total))-1)
	note := municipalityOverflowNote(dropped)
	if value == "" {
		return truncate(note, limit)
	}
	return truncate(value+"\n"+note, limit)
}

func municipalityOverflowNote(count int) string {
	return fmt.Sprintf("…ほか %d市区町村", count)
}

// buildMunicipalityList は budget バイトに収まるだけを返し、入りきらなかった
// 市区町村の数を添えます。余白は実バイト数で数えます。日本語は1文字3バイトなので、
// 文字数で見積もると上限を超えてDiscordに丸ごと拒否されます。
func buildMunicipalityList(groups []prefGroup, budget int) (string, int) {
	lines := []string{}
	used := 0
	written := 0
	total := 0
	for _, group := range groups {
		total += len(group.areas)
	}

	for _, group := range groups {
		header := "- " + group.pref
		// 見出しと市区町村の行、2行ぶんの改行を先に見込みます。
		base := used + len(header) + 1
		line := municipalityIndent
		count := 0
		full := true
		for _, area := range group.areas {
			addition := area
			if count > 0 {
				addition = "、" + area
			}
			if base+len(line)+len(addition)+1 > budget {
				full = false
				break
			}
			line += addition
			count++
		}
		// 見出しだけ書いて中身が無いと「揺れていない県」に見えるので、
		// 1件も入らない県は見出しごと落とします。
		if count == 0 {
			break
		}
		lines = append(lines, header, line)
		used = base + len(line) + 1
		written += count
		if !full {
			break
		}
	}
	return strings.Join(lines, "\n"), total - written
}

// pointArea は観測点の表示名。
func pointArea(point quakePoint) string {
	addr := strings.TrimSpace(point.Addr)
	if addr == "" {
		return ""
	}
	// isArea の点(震度速報)は市区町村ではなく「熊本県天草・芦北」のような
	// 地域名なので、見出しの都道府県と重ならないよう県名だけ落とします。
	if point.IsArea {
		return firstNonEmpty(strings.TrimPrefix(addr, point.Pref), addr)
	}

	area := municipality(addr)
	if !strings.HasSuffix(area, "区") {
		return area
	}
	// 東京23区は区のままが分かりやすいので、見出しの都道府県と重なる
	// 「東京」だけ落とします(「東京足立区伊興」→「足立区」)。
	if point.Pref == "東京都" || strings.HasPrefix(area, "東京") {
		return firstNonEmpty(strings.TrimPrefix(area, "東京"), area)
	}
	// 政令指定都市の区は市まで丸めます。「さいたま北区」「さいたま大宮区」が
	// 別々に並ぶのは、都道府県ごとの市町村一覧としては細かすぎます。
	if city := wardCity(area); city != "" {
		return city
	}
	return area
}

// wardCities は区を持つ市(政令指定都市)。上流は「さいたま北区」「大阪此花区」と
// 市名に区名を続けた形で送ってくるので、市名だけ持って前方一致で拾います。
// 東京23区以外の区はすべてこのいずれかに属します。
var wardCities = []string{
	"札幌", "仙台", "さいたま", "千葉", "横浜", "川崎", "相模原", "新潟", "静岡", "浜松",
	"名古屋", "京都", "大阪", "堺", "神戸", "岡山", "広島", "北九州", "福岡", "熊本",
}

// wardCity は「さいたま北区」→「さいたま市」。拾えなければ空文字を返し、
// 呼び出し側は上流の表記のまま出します。知らない形を捨てるよりましです。
func wardCity(area string) string {
	for _, city := range wardCities {
		if strings.HasPrefix(area, city) {
			return city + "市"
		}
	}
	return ""
}

// municipalityMarkers は市区町村名の末尾の字。
const municipalityMarkers = "市区町村"

// municipalityExceptions は名前の途中に市区町村の字を含み、素直に先頭から
// 探すと切る位置を間違えるもの。「東村山市本町」を「東村」にしないための表です。
// 先頭の1文字は必ず名前の一部として飛ばすので、「市原市」「町田市」「村上市」は
// ここに要りません。逆に「阿波市市場町」「会津坂下町市中」のように地区名が
// 市区町村の字で始まる観測点があるため、隣接する字をまとめて拾う方法は使えません。
var municipalityExceptions = []string{
	"東村山市", "武蔵村山市", "田村市", "羽村市", "大村市",
	"大町市", "大町町", "十日町市", "上市町", "下市町",
	"四日市市", "廿日市市", "野々市市", "名古屋中村区",
}

// municipality は観測点名から市区町村までを取り出します
// (「つくばみらい市加藤」→「つくばみらい市」)。気象庁の観測点名は市区町村名に
// 地区名を続けた形なので、最初の市区町村の字で切るのが基本です。政令指定都市と
// 東京23区は「さいたま北区」「東京足立区」のように市名や都県名が前に付いた形で
// 来ますが、直すと「大阪狭山市」→「狭山市」のような取り違えが起きるので、
// 気象庁の表記のまま出します。
func municipality(addr string) string {
	for _, name := range municipalityExceptions {
		if strings.HasPrefix(addr, name) {
			return name
		}
	}

	runes := []rune(addr)
	for index := 1; index < len(runes); index++ {
		if strings.ContainsRune(municipalityMarkers, runes[index]) {
			return string(runes[:index+1])
		}
	}
	// 想定外の形(「〜郡」など)は、情報を落とさないようそのまま出します。
	return addr
}

// quakeColor は最大震度から色帯を選びます。スマホの通知一覧では色が最初に
// 目に入るので、しきい値は「見て身構えるかどうか」の境目に置きます。
// 震度3は物が落ちる揺れではないので、色を変えるのは震度4からです。
func quakeColor(maxScale int) int {
	switch {
	case maxScale >= scale5Lower:
		return colorSevere
	case maxScale >= scale4:
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
