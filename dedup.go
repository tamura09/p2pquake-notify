package main

import (
	"container/list"
	"time"
)

// dedupCache は「この内容はもう通知した」を覚えておくための有効期限付きの集合です。
//
// これが必要なのは、同じイベントが2つの経路で届くからです。WebSocketで受け取った
// ものと、再接続直後に /v2/history から読み直したものは同じ地震を指します。
// 内容から作った鍵で突き合わせないと、切断のたびに同じ通知が並びます。
//
// 件数と時間の両方で上限を持たせています。時間だけだと大量のメッセージが来た時に
// 際限なく太り、件数だけだと静かな時間帯に古い鍵がいつまでも残ります。
type dedupCache struct {
	ttl     time.Duration
	maxSize int
	now     func() time.Time

	entries map[string]*list.Element
	order   *list.List // 前が古い。値は dedupEntry。
}

type dedupEntry struct {
	key  string
	seen time.Time
}

func newDedupCache(ttl time.Duration, maxSize int) *dedupCache {
	return &dedupCache{
		ttl:     ttl,
		maxSize: maxSize,
		now:     time.Now,
		entries: map[string]*list.Element{},
		order:   list.New(),
	}
}

// seen は鍵を記録し、すでに記録済みだった場合に true を返します。
// 「初めて見た」なら false を返し、呼び出し側はそのイベントを通知します。
func (c *dedupCache) seen(key string) bool {
	if key == "" {
		// 鍵を作れなかったイベントは重複排除の対象外にします。
		// 取りこぼすより重複するほうがまし、という判断です。
		return false
	}

	now := c.now()
	c.evictExpired(now)

	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*dedupEntry)
		entry.seen = now
		c.order.MoveToBack(element)
		return true
	}

	element := c.order.PushBack(&dedupEntry{key: key, seen: now})
	c.entries[key] = element

	for c.order.Len() > c.maxSize {
		c.removeOldest()
	}
	return false
}

func (c *dedupCache) evictExpired(now time.Time) {
	for {
		oldest := c.order.Front()
		if oldest == nil {
			return
		}
		if now.Sub(oldest.Value.(*dedupEntry).seen) <= c.ttl {
			return
		}
		c.removeOldest()
	}
}

func (c *dedupCache) removeOldest() {
	oldest := c.order.Front()
	if oldest == nil {
		return
	}
	c.order.Remove(oldest)
	delete(c.entries, oldest.Value.(*dedupEntry).key)
}

func (c *dedupCache) len() int { return c.order.Len() }
