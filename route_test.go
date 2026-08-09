package main

import (
	"strings"
	"testing"
	"time"
)

// テスト用に本番と同じ3本を組み立てます。loadConfig は SSM を触るので、
// ルーティングの検証だけはここで直接作ります。
func testRoutes() (dev, alert, local route) {
	dev = route{Name: "dev", MinScale: scaleUnknown, IncludeTest: true}
	alert = route{Name: "alert", Codes: codeSet(codeEEW, codeJMATsunami), MinScale: scaleUnknown}
	local = route{
		Name:        "local",
		Codes:       codeSet(codeJMAQuake, codeJMATsunami, codeEEW),
		MinScale:    scaleUnknown,
		Prefectures: []string{"岩手県"},
	}
	return
}

func TestRoutingMatrix(t *testing.T) {
	dev, alert, local := testRoutes()

	cases := []struct {
		name                          string
		raw                           string
		wantDev, wantAlert, wantLocal bool
	}{
		{
			// 岩手県を含む緊急地震速報は3本すべてに乗ります。
			name:    "EEW covering Iwate",
			raw:     eewIwateSerial1,
			wantDev: true, wantAlert: true, wantLocal: true,
		},
		{
			// 東京の地震情報は dev のみ。alert は code 551 を含まず、
			// local は岩手県を含まないため。
			name:    "quake in Tokyo",
			raw:     quakeTokyo,
			wantDev: true, wantAlert: false, wantLocal: false,
		},
		{
			// 津波は alert に乗り、予報区名の部分一致で local にも乗ります。
			name:    "tsunami including Iwate",
			raw:     tsunamiIwate,
			wantDev: true, wantAlert: true, wantLocal: true,
		},
		{
			// 訓練報が本番の通知先に出てはいけません。ここが壊れると
			// 訓練のたびに誤報が飛び、本物の速報が信用されなくなります。
			name:    "training report",
			raw:     eewTraining,
			wantDev: true, wantAlert: false, wantLocal: false,
		},
		{
			// ピア分布はどのルートへも流しません。人間が読んで意味のある情報を
			// 何も含まないうえ、地震が無くても届き続けるので、devに流すだけでも
			// 他の通知を押し流します。生存確認はメトリクス側の仕事です。
			name:    "area peers",
			raw:     areaPeersMessage,
			wantDev: false, wantAlert: false, wantLocal: false,
		},
		{
			// 揺れた報告は利用者1人が「揺れた」と押した1点の記録です。単体では
			// 何も意味せず、少し揺れただけで数十件が数秒のうちに流れます。
			name:    "userquake report",
			raw:     userquakeMessage,
			wantDev: false, wantAlert: false, wantLocal: false,
		},
		{
			// その集計は1回の地震のあいだ中、数値が更新されるたびに新しい
			// メッセージとして届き続けます。
			name:    "userquake evaluation",
			raw:     userquakeEvaluationMessage,
			wantDev: false, wantAlert: false, wantLocal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := decodeOrFail(t, tc.raw)
			if got := dev.matches(e); got != tc.wantDev {
				t.Errorf("dev.matches = %t, want %t", got, tc.wantDev)
			}
			if got := alert.matches(e); got != tc.wantAlert {
				t.Errorf("alert.matches = %t, want %t", got, tc.wantAlert)
			}
			if got := local.matches(e); got != tc.wantLocal {
				t.Errorf("local.matches = %t, want %t", got, tc.wantLocal)
			}
		})
	}
}

func TestMinScaleIgnoresUnknownScales(t *testing.T) {
	// 震度が判明していないイベント(津波予報など)をしきい値で落とすと、
	// 「小さい地震を黙らせる」という意図と正反対に、一番重い通知が消えます。
	r := route{Name: "threshold", MinScale: scale4}

	tsunami := decodeOrFail(t, tsunamiIwate)
	if tsunami.MaxScale != scaleUnknown {
		t.Fatalf("precondition: tsunami MaxScale = %d, want %d", tsunami.MaxScale, scaleUnknown)
	}
	if !r.matches(tsunami) {
		t.Error("tsunami with unknown scale was dropped by a scale threshold")
	}

	quake := decodeOrFail(t, quakeTokyo) // 震度3
	if r.matches(quake) {
		t.Error("quake below the threshold was not dropped")
	}
}

func TestPrefectureMatchingIsSubstring(t *testing.T) {
	if !matchesPrefectures([]string{"青森県"}, []string{"青森県太平洋沿岸"}) {
		t.Error("tsunami forecast area containing the prefecture name did not match")
	}
	if matchesPrefectures([]string{"岩手県"}, []string{"青森県太平洋沿岸"}) {
		t.Error("unrelated prefecture matched")
	}
}

func TestRouteStringDescribesFilters(t *testing.T) {
	_, _, local := testRoutes()
	described := local.String()
	for _, want := range []string{"name=local", "codes=551,552,556", "prefectures=岩手県"} {
		if !strings.Contains(described, want) {
			t.Errorf("route.String() = %q, missing %q", described, want)
		}
	}
}

func TestDedupCacheExpiresAndBounds(t *testing.T) {
	now := time.Unix(0, 0)
	cache := newDedupCache(time.Hour, 3)
	cache.now = func() time.Time { return now }

	if cache.seen("a") {
		t.Error("first sighting of a reported as duplicate")
	}
	if !cache.seen("a") {
		t.Error("second sighting of a not reported as duplicate")
	}

	// 件数の上限を超えたら古いものから落ちます。
	cache.seen("b")
	cache.seen("c")
	cache.seen("d")
	if cache.len() > 3 {
		t.Errorf("cache grew to %d entries, want at most 3", cache.len())
	}

	// 有効期限を過ぎた鍵は忘れます。
	now = now.Add(2 * time.Hour)
	if cache.seen("d") {
		t.Error("entry survived past its TTL")
	}

	// 鍵を作れなかったイベントは重複排除の対象外(取りこぼすより重複を選ぶ)。
	if cache.seen("") {
		t.Error("empty key was treated as already seen")
	}
}
