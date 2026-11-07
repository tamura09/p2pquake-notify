package main

import "time"

// eewThrottle は、送ったものを書き換えられない送り先(Pushover・LINE)向けに、
// 緊急地震速報のどの報を送るかを決めます。
//
// 緊急地震速報は1つの地震で10報以上届きます。Discordなら最初のメッセージを
// 書き換えて済みますが、書き換えのできない送り先に全報を送るとスマホが鳴り続け、
// LINEでは月間の送信上限まで食い潰します。そこで送るのは
//
//   - その地震の最初の報
//   - 予想最大震度が、それまでに送ったものより上がった報
//   - 送ったことのある地震の取消報
//
// に絞ります。続報で予想地域だけが広がった場合は送りません(Discord 側は書き換えで追えます)。
//
// 送信goroutineだけが触るのでロックは要りません。
type eewThrottle struct {
	// peaks は GroupKey -> その地震について送った最大の予想震度。
	peaks map[string]eewPeak
}

type eewPeak struct {
	scale   int
	written time.Time
}

func newEEWThrottle() eewThrottle {
	return eewThrottle{peaks: map[string]eewPeak{}}
}

// allow はこの報を送るべきかを返します。送った扱いにするのは remember を
// 呼んだ時だけで、ここでは何も覚えません。
func (t *eewThrottle) allow(e *event, message eew) bool {
	t.prune()

	peak, sent := t.peaks[e.GroupKey]
	if e.GroupKey == "" {
		sent = false
	}

	switch {
	case message.Cancelled:
		return sent
	case sent && e.MaxScale <= peak.scale:
		return false
	}
	return true
}

// remember は送り先が受け付けた緊急地震速報の予想震度を覚えます。送信前に
// 覚えると、第1報の送信に失敗した地震で、同じ震度の続報まで「送信済み」として
// 抑止され、一度も届かないまま終わります。
func (t *eewThrottle) remember(e *event) {
	message, ok := e.Payload.(eew)
	if !ok || message.Cancelled || e.GroupKey == "" {
		return
	}
	scale := e.MaxScale
	if peak, sent := t.peaks[e.GroupKey]; sent {
		scale = max(scale, peak.scale)
	}
	t.peaks[e.GroupKey] = eewPeak{scale: scale, written: time.Now()}
}

func (t *eewThrottle) prune() {
	for key, peak := range t.peaks {
		if time.Since(peak.written) > groupTTL {
			delete(t.peaks, key)
		}
	}
}
