package main

import (
	"encoding/json"
	"testing"
	"time"
)

// 上流のサンプルに合わせた最小構成のメッセージ。フィールドを増やすより、
// 判定に効く値だけを持たせて意図を読み取りやすくしています。
const (
	eewIwateSerial1 = `{
		"code": 556,
		"time": "2024/01/01 16:10:20.000",
		"test": false,
		"cancelled": false,
		"earthquake": {
			"originTime": "2024/01/01 16:10:00",
			"arrivalTime": "2024/01/01 16:10:12",
			"hypocenter": {"name": "三陸沖", "latitude": 39.5, "longitude": 143.2, "depth": 30, "magnitude": 7.2}
		},
		"issue": {"time": "2024/01/01 16:10:20", "eventId": "20240101161000", "serial": "1"},
		"areas": [
			{"pref": "岩手県", "name": "岩手県沿岸北部", "scaleFrom": 45, "scaleTo": 50, "kindCode": "11"},
			{"pref": "青森県", "name": "青森県三八上北", "scaleFrom": 40, "scaleTo": 40, "kindCode": "11"}
		]
	}`

	eewIwateSerial2 = `{
		"code": 556,
		"time": "2024/01/01 16:10:35.000",
		"test": false,
		"cancelled": false,
		"earthquake": {
			"originTime": "2024/01/01 16:10:00",
			"hypocenter": {"name": "三陸沖", "depth": 30, "magnitude": 7.6}
		},
		"issue": {"time": "2024/01/01 16:10:35", "eventId": "20240101161000", "serial": "2"},
		"areas": [{"pref": "岩手県", "name": "岩手県沿岸北部", "scaleFrom": 55, "scaleTo": 60, "kindCode": "11"}]
	}`

	eewTraining = `{
		"code": 556,
		"time": "2024/01/01 10:00:00.000",
		"test": true,
		"issue": {"time": "2024/01/01 10:00:00", "eventId": "99999999999999", "serial": "1"},
		"earthquake": {"hypocenter": {"name": "訓練", "depth": 10, "magnitude": 7.0}},
		"areas": [{"pref": "岩手県", "name": "岩手県内陸北部", "scaleFrom": 50, "scaleTo": 50}]
	}`

	// eewTraining と同じ eventId の第2報。続報が第1報のメッセージを書き換えるか
	// 確かめるために使います。
	eewTrainingSerial2 = `{
		"code": 556,
		"time": "2024/01/01 10:00:12.000",
		"test": true,
		"issue": {"time": "2024/01/01 10:00:12", "eventId": "99999999999999", "serial": "2"},
		"earthquake": {"hypocenter": {"name": "訓練", "depth": 10, "magnitude": 7.4}},
		"areas": [{"pref": "岩手県", "name": "岩手県内陸北部", "scaleFrom": 55, "scaleTo": 60}]
	}`

	// 2026/08/10 09:48 の熊本の地震について気象庁が出した3報。/v2/history の
	// 実データに合わせています。続報が必ずしも詳しくないことがこれで分かります
	// (2報目は震源が判明する代わりに最大震度が -1 に戻る)。
	quakeScalePrompt = `{
		"code": 551,
		"time": "2026/08/10 09:49:45.000",
		"earthquake": {
			"time": "2026/08/10 09:48:00",
			"hypocenter": {"name": "", "latitude": -200, "longitude": -200, "depth": -1, "magnitude": -1},
			"maxScale": 30,
			"domesticTsunami": "Checking"
		},
		"issue": {"source": "気象庁", "time": "2026/08/10 09:49:44", "type": "ScalePrompt", "correct": "None"},
		"points": [{"pref": "熊本県", "addr": "熊本県天草・芦北", "isArea": true, "scale": 30}]
	}`

	quakeDestination = `{
		"code": 551,
		"time": "2026/08/10 09:51:03.000",
		"earthquake": {
			"time": "2026/08/10 09:48:00",
			"hypocenter": {"name": "熊本県天草・芦北地方", "latitude": 32.3, "longitude": 130.5, "depth": 10, "magnitude": 4.0},
			"maxScale": -1,
			"domesticTsunami": "None"
		},
		"issue": {"source": "気象庁", "time": "2026/08/10 09:51:02", "type": "Destination", "correct": "None"},
		"points": []
	}`

	quakeDetailScale = `{
		"code": 551,
		"time": "2026/08/10 09:52:21.000",
		"earthquake": {
			"time": "2026/08/10 09:48:00",
			"hypocenter": {"name": "熊本県天草・芦北地方", "latitude": 32.3, "longitude": 130.5, "depth": 10, "magnitude": 4.0},
			"maxScale": 30,
			"domesticTsunami": "None"
		},
		"issue": {"source": "気象庁", "time": "2026/08/10 09:52:20", "type": "DetailScale", "correct": "None"},
		"points": [
			{"pref": "熊本県", "addr": "上天草市大矢野町", "isArea": false, "scale": 30},
			{"pref": "熊本県", "addr": "芦北町芦北", "isArea": false, "scale": 30},
			{"pref": "熊本県", "addr": "八代市千丁町", "isArea": false, "scale": 20}
		]
	}`

	quakeTokyo = `{
		"code": 551,
		"time": "2024/01/01 12:34:56.000",
		"earthquake": {
			"time": "2024/01/01 12:30:00",
			"hypocenter": {"name": "東京湾", "latitude": 35.5, "longitude": 139.8, "depth": 40, "magnitude": 4.2},
			"maxScale": 30,
			"domesticTsunami": "None"
		},
		"issue": {"source": "気象庁", "time": "2024/01/01 12:34:00", "type": "DetailScale", "correct": "None"},
		"points": [
			{"pref": "東京都", "addr": "千代田区", "isArea": false, "scale": 30},
			{"pref": "神奈川県", "addr": "横浜市中区", "isArea": false, "scale": 20}
		]
	}`

	tsunamiIwate = `{
		"code": 552,
		"time": "2024/01/01 16:20:00.000",
		"cancelled": false,
		"issue": {"source": "気象庁", "time": "2024/01/01 16:20:00", "type": "Focus"},
		"areas": [
			{"grade": "MajorWarning", "immediate": true, "name": "岩手県", "maxHeight": {"description": "１０ｍ以上", "value": 10}},
			{"grade": "Watch", "immediate": false, "name": "青森県太平洋沿岸", "maxHeight": {"description": "１ｍ", "value": 1}}
		]
	}`

	areaPeersMessage = `{"code": 555, "time": "2024/01/01 00:00:00.000", "areas": [{"id": 10, "peer": 5}, {"id": 20, "peer": 7}]}`

	// 本番のdevチャンネルに流れてきた実データ。P2P地震情報のネットワーク内部の
	// 様子で、気象庁の発表ではありません。少し揺れただけで561が数十件、その集計の
	// 9611が数秒おきに届きます。
	userquakeMessage = `{"_id":"6a784fafc587570007042981","area":231,"code":561,"created_at":"2026/08/09 19:00:15.258","expire":"2026/08/09 19:01:14","hop":2,"time":"2026/08/09 19:00:13.452","uid":"0177520260809190014232","ver":"20150406"}`

	userquakeEvaluationMessage = `{"_id":"6a784faf2c4d3735f4b92fa7","area_confidences":{"231":{"confidence":0.032413164232655035,"count":6,"display":"E"},"250":{"confidence":0.04127080865158951,"count":13,"display":"E"}},"code":9611,"confidence":0.98052,"count":40,"started_at":"2026/08/09 18:59:56.452","time":"2026/08/09 19:00:15.874","updated_at":"2026/08/09 19:00:13.862","ver":"20260612"}`

	// /v2/history から取得した実データをそのまま貼ったもの。上流の表記ゆれを
	// 記録しておくのが目的で、要点は areas[].pref が「熊本」「鹿児島」と
	// 県が付かない形で来ることです。地震情報(551)の points[].pref は
	// 「鹿児島県」と付くので、上流の中で揃っていません。
	eewRealKumamoto = `{
		"areas": [
			{"arrivalTime": "2026/07/29 22:19:44", "kindCode": "19", "name": "熊本県熊本", "pref": "熊本", "scaleFrom": 45, "scaleTo": 45},
			{"arrivalTime": "2026/07/29 22:19:44", "kindCode": "19", "name": "熊本県天草・芦北", "pref": "熊本", "scaleFrom": 45, "scaleTo": 45},
			{"arrivalTime": "2026/07/29 22:19:43", "kindCode": "19", "name": "鹿児島県薩摩", "pref": "鹿児島", "scaleFrom": 40, "scaleTo": 40}
		],
		"cancelled": false,
		"code": 556,
		"earthquake": {
			"arrivalTime": "2026/07/29 22:19:39",
			"condition": "",
			"hypocenter": {"depth": 10, "latitude": 32.4, "longitude": 130.5, "magnitude": 4.5, "name": "熊本県天草・芦北地方", "reduceName": "熊本県"},
			"originTime": "2026/07/29 22:19:36"
		},
		"id": "6a69fdf1e88ee598246bf002",
		"issue": {"eventId": "20260729221939", "serial": "1", "time": "2026/07/29 22:19:44"},
		"time": "2026/07/29 22:19:45.168",
		"ver": "20231023"
	}`

	// 実データ。津波予報の解除は areas が空配列で cancelled だけが立ちます。
	tsunamiRealCancelled = `{
		"areas": [],
		"cancelled": true,
		"code": 552,
		"id": "6a6871f3e88ee598246bef07",
		"issue": {"source": "気象庁", "time": "2026/07/28 18:10:10", "type": "Focus"},
		"time": "2026/07/28 18:10:11.31",
		"ver": "20231023"
	}`
)

func decodeOrFail(t *testing.T, raw string) *event {
	t.Helper()
	e, err := decodeEvent(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if e == nil {
		t.Fatal("decodeEvent returned nil event")
	}
	return e
}

func TestDecodeEEW(t *testing.T) {
	e := decodeOrFail(t, eewIwateSerial1)

	if e.Code != codeEEW {
		t.Errorf("Code = %d, want %d", e.Code, codeEEW)
	}
	// 予想震度は範囲で来るので、上限(5強=50)を最大震度として扱います。
	if e.MaxScale != scale5Upper {
		t.Errorf("MaxScale = %d, want %d", e.MaxScale, scale5Upper)
	}
	// 県名と細分区域名の両方を持ちます(理由は
	// TestEEWCarriesFullPrefectureNamesForFiltering を参照)。
	for _, want := range []string{"岩手県", "青森県"} {
		if !matchesPrefectures([]string{want}, e.Prefectures) {
			t.Errorf("%s did not match %v", want, e.Prefectures)
		}
	}
	if e.GroupKey == "" {
		t.Error("GroupKey is empty; follow-up reports would post as new messages")
	}
	if e.Test {
		t.Error("Test = true for a real report")
	}
}

// 第1報と第2報は別々に通知したいので DedupKey は異なり、同じ地震の続報として
// まとめたいので GroupKey は一致する必要があります。ここが崩れると、
// 続報が握り潰されるか、逆に通知が報の数だけ積み上がります。
func TestEEWSerialsShareGroupButNotDedupKey(t *testing.T) {
	first := decodeOrFail(t, eewIwateSerial1)
	second := decodeOrFail(t, eewIwateSerial2)

	if first.GroupKey != second.GroupKey {
		t.Errorf("GroupKey differs across serials: %q vs %q", first.GroupKey, second.GroupKey)
	}
	if first.DedupKey == second.DedupKey {
		t.Errorf("DedupKey is identical across serials (%q); the follow-up would be dropped", first.DedupKey)
	}
	if second.MaxScale != scale6Upper {
		t.Errorf("second report MaxScale = %d, want %d", second.MaxScale, scale6Upper)
	}
}

func TestDecodeQuake(t *testing.T) {
	e := decodeOrFail(t, quakeTokyo)

	if e.Code != codeJMAQuake {
		t.Errorf("Code = %d, want %d", e.Code, codeJMAQuake)
	}
	if e.MaxScale != scale3 {
		t.Errorf("MaxScale = %d, want %d", e.MaxScale, scale3)
	}
	if e.GroupKey == "" {
		t.Error("GroupKey is empty; follow-up reports would post as new messages")
	}
	want := []string{"東京都", "神奈川県"}
	if !sameStringSet(e.Prefectures, want) {
		t.Errorf("Prefectures = %v, want %v", e.Prefectures, want)
	}
}

// 1回の地震についての3報は同じ鍵でまとまり、かつ別々のイベントとして
// 処理される必要があります。まとまらないと「震源調査中 で地震」という
// 途中経過の表示がチャンネルに残り続けます。
func TestQuakeReportsOfOneEarthquakeShareGroupKey(t *testing.T) {
	prompt := decodeOrFail(t, quakeScalePrompt)
	destination := decodeOrFail(t, quakeDestination)
	detail := decodeOrFail(t, quakeDetailScale)

	if prompt.GroupKey != destination.GroupKey || prompt.GroupKey != detail.GroupKey {
		t.Errorf("GroupKeys differ across reports of one earthquake: %q / %q / %q",
			prompt.GroupKey, destination.GroupKey, detail.GroupKey)
	}
	// 別の地震とは混ざりません。
	if other := decodeOrFail(t, quakeTokyo); other.GroupKey == prompt.GroupKey {
		t.Error("a different earthquake shares the GroupKey")
	}
	// 鍵が同じでも各報は個別に処理されます(重複排除で落ちてはいけません)。
	keys := map[string]bool{prompt.DedupKey: true, destination.DedupKey: true, detail.DedupKey: true}
	if len(keys) != 3 {
		t.Errorf("the three reports collapsed into %d dedup keys", len(keys))
	}
}

// 続報は必ずしも詳しくありません。合成しないと、震源が判明する2報目で
// 最大震度が「不明」に後退します。
func TestMergeQuakeReportsKeepsWhatWasAlreadyKnown(t *testing.T) {
	prompt := decodeOrFail(t, quakeScalePrompt).Payload.(jmaQuake)
	destination := decodeOrFail(t, quakeDestination).Payload.(jmaQuake)

	merged := mergeQuakeReports(prompt, destination)

	// 2報目で新たに判明したもの。
	if merged.Earthquake.Hypocenter.Name != "熊本県天草・芦北地方" {
		t.Errorf("hypocenter = %q, want the newly reported name", merged.Earthquake.Hypocenter.Name)
	}
	if merged.Earthquake.Hypocenter.Magnitude != 4.0 {
		t.Errorf("magnitude = %v, want 4", merged.Earthquake.Hypocenter.Magnitude)
	}
	if merged.Earthquake.Hypocenter.Depth != 10 {
		t.Errorf("depth = %v, want 10", merged.Earthquake.Hypocenter.Depth)
	}
	// 1報目でしか分かっていないもの。ここが落ちると表示が後退します。
	if merged.Earthquake.MaxScale != scale3 {
		t.Errorf("maxScale = %d, want %d carried over from the first report", merged.Earthquake.MaxScale, scale3)
	}
	if len(merged.Points) != 1 {
		t.Errorf("points = %d, want the first report's 1 kept (the second carries none)", len(merged.Points))
	}
	// 「調査中」から確定した値へは進みます。
	if merged.Earthquake.DomesticTsunami != "None" {
		t.Errorf("domesticTsunami = %q, want the settled None", merged.Earthquake.DomesticTsunami)
	}

	// 3報目は各地の震度を伴うので、そちらが優先されます。
	detail := decodeOrFail(t, quakeDetailScale).Payload.(jmaQuake)
	final := mergeQuakeReports(merged, detail)
	if len(final.Points) != 3 {
		t.Errorf("points = %d, want the detailed report's 3", len(final.Points))
	}
	if final.Earthquake.MaxScale != scale3 {
		t.Errorf("maxScale = %d, want %d", final.Earthquake.MaxScale, scale3)
	}
}

func TestDecodeTsunamiKeepsAreaNames(t *testing.T) {
	e := decodeOrFail(t, tsunamiIwate)

	// 予報区名をそのまま持たせることで、"青森県" という指定でも
	// "青森県太平洋沿岸" を部分一致で拾えるようにしています。
	want := []string{"岩手県", "青森県太平洋沿岸"}
	if !sameStringSet(e.Prefectures, want) {
		t.Errorf("Prefectures = %v, want %v", e.Prefectures, want)
	}
}

// 上流の実データに対する回帰テスト。緊急地震速報の areas[].pref は県が付かない
// 形で来るので、pref だけを持たせていると「岩手県」のような設定が 556 に
// 一度も一致しません。地域ルートが緊急地震速報だけ取りこぼすという、
// 本物の地震が起きるまで気付けない壊れ方をします。
func TestEEWCarriesFullPrefectureNamesForFiltering(t *testing.T) {
	e := decodeOrFail(t, eewRealKumamoto)

	if !matchesPrefectures([]string{"熊本県"}, e.Prefectures) {
		t.Errorf("熊本県 did not match %v; the local route would miss this EEW", e.Prefectures)
	}
	if !matchesPrefectures([]string{"鹿児島県"}, e.Prefectures) {
		t.Errorf("鹿児島県 did not match %v", e.Prefectures)
	}
	if matchesPrefectures([]string{"岩手県"}, e.Prefectures) {
		t.Errorf("岩手県 matched an EEW covering only 熊本/鹿児島: %v", e.Prefectures)
	}
}

// 津波予報の解除は areas が空で届きます。ここで落ちると「解除されたことが
// 分からない」状態になり、警報だけが出たまま残ります。
func TestCancelledTsunamiWithNoAreasRenders(t *testing.T) {
	e := decodeOrFail(t, tsunamiRealCancelled)
	payload := renderPayload(e, testZone())

	if len(payload.Embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(payload.Embeds))
	}
	if payload.Embeds[0].Title != "津波予報 解除" {
		t.Errorf("Title = %q, want 津波予報 解除", payload.Embeds[0].Title)
	}
	// 解除でスマホを鳴らす必要はありません。
	if payload.Content != "" {
		t.Errorf("Content = %q, want empty for a cancellation", payload.Content)
	}
}

func TestDecodeUnknownCodeIsNotAnError(t *testing.T) {
	// 上流がメッセージ形式を増やしても落ちないこと。dev用ルートへは
	// 生JSONとして流れ、そこで気付けるのが狙いです。
	e := decodeOrFail(t, `{"code": 12345, "time": "2024/01/01 00:00:00.000"}`)
	if e.Code != 12345 {
		t.Errorf("Code = %d, want 12345", e.Code)
	}
	if e.DedupKey == "" {
		t.Error("DedupKey is empty for an unknown code")
	}
}

func TestDedupKeyDoesNotCollideOnSeparatorShifts(t *testing.T) {
	// "A|B" と "A" + "|B" のような境界のずれで同じ鍵にならないこと。
	// 衝突すると別の地震の通知が握り潰されます。
	if dedupKey(551, "a|b", "c") == dedupKey(551, "a", "b|c") {
		t.Error("dedupKey collided across differently split parts")
	}
}

func TestParseP2PTime(t *testing.T) {
	zone := time.FixedZone("JST", 9*60*60)

	for _, value := range []string{"2024/01/01 16:10:20", "2024/01/01 16:10:20.000"} {
		parsed, ok := parseP2PTime(value, zone)
		if !ok {
			t.Fatalf("parseP2PTime(%q) failed", value)
		}
		if parsed.Hour() != 16 || parsed.Minute() != 10 {
			t.Errorf("parseP2PTime(%q) = %v", value, parsed)
		}
	}

	if _, ok := parseP2PTime("", zone); ok {
		t.Error("parseP2PTime(\"\") reported success")
	}
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, value := range got {
		seen[value] = true
	}
	for _, value := range want {
		if !seen[value] {
			return false
		}
	}
	return true
}
