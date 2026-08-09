package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// P2P地震情報 JSON API v2 のメッセージ型。
//
// WebSocketとREST (/v2/history) は同じ形のJSONを返すので、再接続後のギャップ埋めで
// 読み直した履歴もこのままデコードできます。
//
// 上流は無保証の無償サービスで、フィールドが増減しても告知があるとは限りません。
// そのため必須として扱うのは code だけにして、他は「無ければゼロ値」で成立するよう
// 書いています。未知の code が来ても落とさず読み飛ばします。
const (
	codeJMAQuake            = 551  // 地震情報(震源・震度)
	codeJMATsunami          = 552  // 津波予報
	codeEEWDetection        = 554  // 緊急地震速報 検出(P2Pクライアント側の検知であって気象庁の発表ではない)
	codeAreaPeers           = 555  // ピア分布。定期的に流れてくるので死活監視に使う
	codeEEW                 = 556  // 緊急地震速報(警報)
	codeUserquake           = 561  // 揺れた報告
	codeUserquakeEvaluation = 9611 // 揺れた報告の統計
)

// envelope はどのcodeにも共通する部分。まずこれだけデコードして
// codeを見てから本体の型に振り分けます。
type envelope struct {
	ID   string `json:"id"`
	Code int    `json:"code"`
	Time string `json:"time"`
}

// hypocenter は震源。緊急地震速報の第1報では深さやマグニチュードが
// 未確定のことがあり、その場合 -1 や 0 が入ります。
type hypocenter struct {
	Name       string  `json:"name"`
	ReduceName string  `json:"reduceName"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Depth      int     `json:"depth"`
	Magnitude  float64 `json:"magnitude"`
}

type quakeIssue struct {
	Source  string `json:"source"`
	Time    string `json:"time"`
	Type    string `json:"type"`
	Correct string `json:"correct"`
}

type quakeDetail struct {
	Time            string     `json:"time"`
	Hypocenter      hypocenter `json:"hypocenter"`
	MaxScale        int        `json:"maxScale"`
	DomesticTsunami string     `json:"domesticTsunami"`
	ForeignTsunami  string     `json:"foreignTsunami"`
}

type quakePoint struct {
	Pref   string `json:"pref"`
	Addr   string `json:"addr"`
	IsArea bool   `json:"isArea"`
	Scale  int    `json:"scale"`
}

// jmaQuake は code 551。地震が起きた「後」の観測情報なので速報性はありません。
type jmaQuake struct {
	envelope
	Earthquake quakeDetail  `json:"earthquake"`
	Issue      quakeIssue   `json:"issue"`
	Points     []quakePoint `json:"points"`
}

type tsunamiHeight struct {
	Type        string  `json:"type"`
	Condition   string  `json:"condition"`
	Description string  `json:"description"`
	Value       float64 `json:"value"`
}

type tsunamiArea struct {
	Grade       string         `json:"grade"`
	Immediate   bool           `json:"immediate"`
	Name        string         `json:"name"`
	FirstHeight *tsunamiHeight `json:"firstHeight"`
	MaxHeight   *tsunamiHeight `json:"maxHeight"`
}

// jmaTsunami は code 552。
type jmaTsunami struct {
	envelope
	Cancelled bool          `json:"cancelled"`
	Issue     quakeIssue    `json:"issue"`
	Areas     []tsunamiArea `json:"areas"`
}

type eewIssue struct {
	Time    string `json:"time"`
	EventID string `json:"eventId"`
	Serial  string `json:"serial"`
}

type eewEarthquake struct {
	OriginTime  string     `json:"originTime"`
	ArrivalTime string     `json:"arrivalTime"`
	Condition   string     `json:"condition"`
	Hypocenter  hypocenter `json:"hypocenter"`
}

type eewArea struct {
	Pref        string `json:"pref"`
	Name        string `json:"name"`
	ScaleFrom   int    `json:"scaleFrom"`
	ScaleTo     int    `json:"scaleTo"`
	KindCode    string `json:"kindCode"`
	ArrivalTime string `json:"arrivalTime"`
}

// eew は code 556。気象庁の緊急地震速報(警報)のみで、震度4以下相当の
// 「予報」は流れてきません。同一地震について第1報から最終報まで複数届き、
// Issue.EventID が同じで Serial が増えていきます。
type eew struct {
	envelope
	Test       bool          `json:"test"`
	Cancelled  bool          `json:"cancelled"`
	Earthquake eewEarthquake `json:"earthquake"`
	Issue      eewIssue      `json:"issue"`
	Areas      []eewArea     `json:"areas"`
}

// eewDetection は code 554。P2Pクライアントが揺れを検知したという意味で、
// 気象庁の発表ではありません。誤検知もあります。
type eewDetection struct {
	envelope
	Type string `json:"type"`
}

type areaPeer struct {
	ID   int `json:"id"`
	Peer int `json:"peer"`
}

// areaPeers は code 555。内容そのものに用はありませんが、地震が無くても
// 定期的に流れてくる唯一のメッセージなので、接続が生きている証拠として使います。
type areaPeers struct {
	envelope
	Areas []areaPeer `json:"areas"`
}

// event はデコード済みメッセージ。ルーティング判定と描画がこれを見ます。
// 具体型は Payload に入れ、判定に使う値だけを平坦化して持たせています。
type event struct {
	Code    int
	Raw     json.RawMessage
	Payload any

	// DedupKey は同じ内容の再配信をまとめるための鍵。再接続直後の履歴補完で
	// WebSocketで受信済みのものを読み直すため、これが無いと二重通知します。
	DedupKey string

	// GroupKey は「同じ地震についての一連の報」をまとめる鍵。緊急地震速報だけが
	// 使い、第2報以降はDiscordのメッセージを新規投稿せず既存を書き換えます。
	GroupKey string

	// MaxScale は最大震度(未知なら scaleUnknown)。震度しきい値の判定に使います。
	MaxScale int

	// Prefectures はこのイベントが言及している都道府県。地域フィルタが見ます。
	Prefectures []string

	// Test は訓練報・テスト配信。既定では通知しません。
	Test bool
}

// 震度階級。P2P地震情報は震度を10倍した整数で表し、5弱/5強・6弱/6強は
// 45/50・55/60 に割り当てています。-1 は不明。
const (
	scaleUnknown = -1
	scale1       = 10
	scale3       = 30
	scale4       = 40
	scale5Lower  = 45
	scale5Upper  = 50
	scale6Lower  = 55
	scale6Upper  = 60
	scale7       = 70
)

var scaleLabels = map[int]string{
	scaleUnknown: "震度不明",
	0:            "震度0",
	scale1:       "震度1",
	20:           "震度2",
	scale3:       "震度3",
	scale4:       "震度4",
	scale5Lower:  "震度5弱",
	scale5Upper:  "震度5強",
	scale6Lower:  "震度6弱",
	scale6Upper:  "震度6強",
	scale7:       "震度7",
}

func scaleLabel(scale int) string {
	if label, ok := scaleLabels[scale]; ok {
		return label
	}
	// 未知の値でも通知そのものは出したいので、生の数値を見せて落とさない。
	return fmt.Sprintf("震度階級%d", scale)
}

// scaleRangeLabel は緊急地震速報の予想震度。下限と上限が違えば「5強〜6弱」の形にします。
func scaleRangeLabel(from, to int) string {
	if from == scaleUnknown && to == scaleUnknown {
		return "震度不明"
	}
	if to == scaleUnknown || from == to {
		return scaleLabel(from)
	}
	if from == scaleUnknown {
		return scaleLabel(to)
	}
	return fmt.Sprintf("%s〜%s", scaleLabel(from), strings.TrimPrefix(scaleLabel(to), "震度"))
}

var tsunamiGradeLabels = map[string]string{
	"MajorWarning": "大津波警報",
	"Warning":      "津波警報",
	"Watch":        "津波注意報",
	"Unknown":      "不明",
}

func tsunamiGradeLabel(grade string) string {
	if label, ok := tsunamiGradeLabels[grade]; ok {
		return label
	}
	return grade
}

var domesticTsunamiLabels = map[string]string{
	"None":         "なし",
	"Unknown":      "不明",
	"Checking":     "調査中",
	"NonEffective": "若干の海面変動",
	"Watch":        "津波注意報",
	"Warning":      "津波警報・注意報",
}

func domesticTsunamiLabel(value string) string {
	if label, ok := domesticTsunamiLabels[value]; ok {
		return label
	}
	return value
}

var quakeIssueTypeLabels = map[string]string{
	"ScalePrompt":         "震度速報",
	"Destination":         "震源に関する情報",
	"ScaleAndDestination": "震度・震源に関する情報",
	"DetailScale":         "各地の震度に関する情報",
	"Foreign":             "遠地地震に関する情報",
	"Other":               "その他の情報",
}

func quakeIssueTypeLabel(value string) string {
	if label, ok := quakeIssueTypeLabels[value]; ok {
		return label
	}
	return value
}

// p2pTimeLayout は上流の時刻表記。ミリ秒付き("2006/01/02 15:04:05.000")の
// フィールドもあるので、パースは両方試します。タイムゾーン表記は無く、常にJSTです。
const (
	p2pTimeLayout      = "2006/01/02 15:04:05"
	p2pTimeLayoutMilli = "2006/01/02 15:04:05.000"
)

func parseP2PTime(value string, zone *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{p2pTimeLayoutMilli, p2pTimeLayout} {
		if parsed, err := time.ParseInLocation(layout, value, zone); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
