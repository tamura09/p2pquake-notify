package main

import "time"

// eewQuakes は「このルートが緊急地震速報(556)を流した地震」の発生時刻を覚えておく
// ための表です。あとから届く地震情報(551)を、同じルートへ通してよいかの判定に使います。
//
// 緊急地震速報は「これから揺れる」という予想でしかなく、実際にどこがどれだけ揺れたのかは
// 後から出る地震情報にしか載りません。速報だけを流すチャンネルは「震度5弱を予想」で
// 話が止まり、結果が分からないままになります。だから、速報を出した地震については
// 結果も同じチャンネルへ出します。
//
// 突き合わせの鍵は発生時刻です。556 の issue.eventId は 551 に存在せず、両者を直接
// 結ぶIDが無いためです。556 の earthquake.originTime は推定値、551 の earthquake.time は
// 解析後の確定値なので、同じ地震でも数秒ずれます。一方で大きな地震の後は余震が数分おきに
// 起きるため、窓を広げすぎると別の地震を同じものとして拾います。tolerance はその間を
// 取った値です。
type eewQuakes struct {
	// tolerance は「同じ地震」とみなす発生時刻のずれ。ゼロなら既定値。
	tolerance time.Duration

	// ttl は速報を出した地震を覚えておく期間。ゼロなら既定値。
	// 実時刻で計り、発生時刻では計りません(履歴補完では過去のイベントを
	// 扱うので、発生時刻で切ると登録した端から消えます)。
	ttl time.Duration

	// now はテストで差し替えます。ゼロなら time.Now。
	now func() time.Time

	// origins はルート名 -> 覚えている発生時刻。ルートごとに分けるのは、
	// 速報を流したのがどのルートかによって続きを出す先も変わるためです。
	origins map[string][]eewQuakeEntry
}

type eewQuakeEntry struct {
	origin   time.Time // 地震の発生時刻(JST)
	recorded time.Time // 覚えた実時刻。期限切れの判定に使います。
}

const (
	// defaultEEWQuakeTolerance は発生時刻のずれの許容。緊急地震速報の推定発生時刻は
	// 続報で数秒動きますが、余震はもっと間隔が空くので、2分あれば取り違えません。
	defaultEEWQuakeTolerance = 2 * time.Minute

	// defaultEEWQuakeTTL は地震情報を待つ期間。各地の震度は数分で出ますが、
	// 訂正報はもっと後に来ることがあるので長めに取ります。
	defaultEEWQuakeTTL = 3 * time.Hour
)

func (q *eewQuakes) clock() time.Time {
	if q.now == nil {
		return time.Now()
	}
	return q.now()
}

func (q *eewQuakes) window() time.Duration {
	if q.tolerance <= 0 {
		return defaultEEWQuakeTolerance
	}
	return q.tolerance
}

func (q *eewQuakes) lifetime() time.Duration {
	if q.ttl <= 0 {
		return defaultEEWQuakeTTL
	}
	return q.ttl
}

// record はこのルートが緊急地震速報を流した地震の発生時刻を覚えます。
// すでに同じ地震として覚えているものがあれば増やしません(1回の地震で
// 第1報から最終報まで10件以上届くため)。
func (q *eewQuakes) record(routeName string, origin time.Time) {
	if origin.IsZero() {
		return
	}
	q.prune()
	if q.matches(routeName, origin) {
		return
	}
	if q.origins == nil {
		q.origins = map[string][]eewQuakeEntry{}
	}
	q.origins[routeName] = append(q.origins[routeName], eewQuakeEntry{origin: origin, recorded: q.clock()})
}

// matches は、この発生時刻の地震について、そのルートが緊急地震速報を流したかを返します。
func (q *eewQuakes) matches(routeName string, at time.Time) bool {
	if at.IsZero() {
		return false
	}
	q.prune()
	tolerance := q.window()
	for _, entry := range q.origins[routeName] {
		if difference(entry.origin, at) <= tolerance {
			return true
		}
	}
	return false
}

func (q *eewQuakes) prune() {
	if len(q.origins) == 0 {
		return
	}
	now := q.clock()
	ttl := q.lifetime()
	for name, entries := range q.origins {
		kept := entries[:0]
		for _, entry := range entries {
			if now.Sub(entry.recorded) <= ttl {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(q.origins, name)
			continue
		}
		q.origins[name] = kept
	}
}

func difference(a, b time.Time) time.Duration {
	if a.After(b) {
		return a.Sub(b)
	}
	return b.Sub(a)
}
