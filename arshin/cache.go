package arshin

import (
	"encoding/json"
	"sync"
	"time"
)

// ttlCache — карточки в памяти на время разговора.
//
// Без вытеснения по LRU: потолок по числу записей нужен лишь затем,
// чтобы сервер, проработавший месяц, не держал в памяти весь реестр.
// При переполнении чистятся просроченные, а если таких нет — всё:
// это дешевле, чем список порядка обращений, и кэш здесь — экономия
// лимита частоты, а не источник данных.
type ttlCache struct {
	ttl time.Duration
	max int

	mu    sync.Mutex
	items map[string]cacheItem
}

type cacheItem struct {
	raw     json.RawMessage
	expires time.Time
}

func newTTLCache(ttl time.Duration, max int) *ttlCache {
	return &ttlCache{ttl: ttl, max: max, items: map[string]cacheItem{}}
}

func (c *ttlCache) get(key string) (json.RawMessage, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		return nil, false
	}
	return it.raw, true
}

func (c *ttlCache) put(key string, raw json.RawMessage) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.max {
		now := time.Now()
		for k, it := range c.items {
			if now.After(it.expires) {
				delete(c.items, k)
			}
		}
		if len(c.items) >= c.max {
			c.items = map[string]cacheItem{}
		}
	}
	c.items[key] = cacheItem{raw: raw, expires: time.Now().Add(c.ttl)}
}
