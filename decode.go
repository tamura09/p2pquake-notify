package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// decodeEvent は受信した1メッセージを event に変換します。
// 対象外の code は (nil, nil) を返し、呼び出し側は黙って捨てます。
func decodeEvent(raw json.RawMessage) (*event, error) {
	var head envelope
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}

	switch head.Code {
	case codeJMAQuake:
		return decodeJMAQuake(raw)
	case codeJMATsunami:
		return decodeJMATsunami(raw)
	case codeEEW:
		return decodeEEW(raw)
	case codeEEWDetection:
		return decodeEEWDetection(raw)
	case codeAreaPeers:
		return decodeAreaPeers(raw)
	default:
		// 561 / 9611 とまだ知らない code。dev用ルートに素通しできるよう
		// event 自体は作りますが、描画は生JSONの要約になります。
		return &event{
			Code:     head.Code,
			Raw:      raw,
			DedupKey: contentKey(head.Code, raw),
			MaxScale: scaleUnknown,
		}, nil
	}
}

func decodeJMAQuake(raw json.RawMessage) (*event, error) {
	var message jmaQuake
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode 551: %w", err)
	}

	prefectures := make([]string, 0, len(message.Points))
	for _, point := range message.Points {
		if point.Pref != "" {
			prefectures = append(prefectures, point.Pref)
		}
	}

	// 重複排除の鍵に id を使わないのは、WebSocketで受けた1件と、再接続時に
	// /v2/history から読み直した同じ1件で id の有無が食い違うと二重通知に
	// なるためです。内容から鍵を作れば経路が違っても同じ値になります。
	// 続報(訂正報や「震度速報」→「各地の震度」)は別イベントとして通知したいので、
	// 発表種別と発表時刻まで鍵に含めます。
	key := dedupKey(codeJMAQuake,
		message.Issue.Source, message.Issue.Type, message.Issue.Time,
		message.Issue.Correct, message.Earthquake.Time)

	return &event{
		Code:        codeJMAQuake,
		Raw:         raw,
		Payload:     message,
		DedupKey:    key,
		MaxScale:    message.Earthquake.MaxScale,
		Prefectures: uniqueStrings(prefectures),
	}, nil
}

func decodeJMATsunami(raw json.RawMessage) (*event, error) {
	var message jmaTsunami
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode 552: %w", err)
	}

	// 津波予報区は「青森県太平洋沿岸」のように県名を含む名前なので、
	// 区名をそのまま持たせておけば地域フィルタ側の部分一致で拾えます。
	prefectures := make([]string, 0, len(message.Areas))
	for _, area := range message.Areas {
		if area.Name != "" {
			prefectures = append(prefectures, area.Name)
		}
	}

	key := dedupKey(codeJMATsunami,
		message.Issue.Source, message.Issue.Type, message.Issue.Time,
		fmt.Sprintf("cancelled=%t", message.Cancelled))

	return &event{
		Code:        codeJMATsunami,
		Raw:         raw,
		Payload:     message,
		DedupKey:    key,
		MaxScale:    scaleUnknown,
		Prefectures: uniqueStrings(prefectures),
	}, nil
}

func decodeEEW(raw json.RawMessage) (*event, error) {
	var message eew
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode 556: %w", err)
	}

	// 地域名(area.Name)も一緒に持たせるのが要点です。緊急地震速報の area.Pref は
	// 「熊本」「鹿児島」のように県が付かない形で来ます(地震情報 551 の points[].pref は
	// 「鹿児島県」と付くので、上流の中で表記が揃っていません)。pref だけを見ていると
	// 「岩手県」という設定が 556 に一度も一致せず、地域ルートが緊急地震速報を
	// 取りこぼします。area.Name は「岩手県沿岸北部」のように県名を含む細分区域名なので、
	// これを併せて持たせることで部分一致が成立します。
	prefectures := make([]string, 0, len(message.Areas)*2)
	maxScale := scaleUnknown
	for _, area := range message.Areas {
		if area.Pref != "" {
			prefectures = append(prefectures, area.Pref)
		}
		if area.Name != "" {
			prefectures = append(prefectures, area.Name)
		}
		// 予想震度は範囲で来るので、上限を最大震度として扱います。
		// 上限が不明なら下限で代用します(第1報で起きがち)。
		for _, scale := range []int{area.ScaleTo, area.ScaleFrom} {
			if scale > maxScale {
				maxScale = scale
			}
		}
	}

	// 第1報と第2報は別々に通知したいので鍵は serial まで含めます。
	// 「同じ地震か」の判定は GroupKey が担い、そちらは eventId だけで決まります。
	key := dedupKey(codeEEW, message.Issue.EventID, message.Issue.Serial,
		fmt.Sprintf("cancelled=%t", message.Cancelled))

	group := ""
	if message.Issue.EventID != "" {
		group = dedupKey(codeEEW, message.Issue.EventID)
	}

	return &event{
		Code:        codeEEW,
		Raw:         raw,
		Payload:     message,
		DedupKey:    key,
		GroupKey:    group,
		MaxScale:    maxScale,
		Prefectures: uniqueStrings(prefectures),
		Test:        message.Test,
	}, nil
}

func decodeEEWDetection(raw json.RawMessage) (*event, error) {
	var message eewDetection
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode 554: %w", err)
	}
	return &event{
		Code:     codeEEWDetection,
		Raw:      raw,
		Payload:  message,
		DedupKey: dedupKey(codeEEWDetection, message.Time, message.Type),
		MaxScale: scaleUnknown,
	}, nil
}

func decodeAreaPeers(raw json.RawMessage) (*event, error) {
	var message areaPeers
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode 555: %w", err)
	}
	return &event{
		Code:     codeAreaPeers,
		Raw:      raw,
		Payload:  message,
		DedupKey: dedupKey(codeAreaPeers, message.Time),
		MaxScale: scaleUnknown,
	}, nil
}

// dedupKey は与えられた要素を区切り文字で連結した鍵を返します。
// 要素そのものに区切り文字が現れない前提は置けないので、長さを前置して
// 「A|B」と「A|」+「B」が衝突しないようにしています。
func dedupKey(code int, parts ...string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d", code)
	for _, part := range parts {
		fmt.Fprintf(&builder, "|%d:%s", len(part), part)
	}
	return builder.String()
}

// contentKey は構造を知らないメッセージ用の鍵。生JSONのハッシュを使います。
func contentKey(code int, raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%d|sha256:%s", code, hex.EncodeToString(sum[:8]))
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
