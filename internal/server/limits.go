// limits.go — простые in-memory примитивы: token-bucket на IP для /dl
// и кэш результатов резолва с TTL (ретраи шортката не дёргают апстрим).
package server

import (
	"sync"
	"time"
)

// rateLimiter — token bucket, refill rpm/60 в секунду, burst на старте.
// Карта на N сидящих IP с ленивой чисткой: персональный сервис, не CDN.
type rateLimiter struct {
	mu      sync.Mutex
	rps     float64
	burst   float64
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rpm, burst int) *rateLimiter {
	if rpm <= 0 {
		rpm = 10
	}
	if burst <= 0 {
		burst = 5
	}
	return &rateLimiter{
		rps:     float64(rpm) / 60.0,
		burst:   float64(burst),
		buckets: map[string]*bucket{},
		lastGC:  time.Now(),
	}
}

func (l *rateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// раз в 10 минут подчищаем брошенные ведёрки
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Hour {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// resolveCache — url → прямая ссылка, TTL+размер ограничены.
type resolveCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items map[string]cacheItem
}

type cacheItem struct {
	bests   []string
	expires time.Time
}

func newResolveCache(max int, ttl time.Duration) *resolveCache {
	return &resolveCache{ttl: ttl, max: max, items: map[string]cacheItem{}}
}

func (c *resolveCache) get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		if ok {
			delete(c.items, key)
		}
		return nil, false
	}
	return it.bests, true
}

func (c *resolveCache) put(key string, bests []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.max {
		// выкидываем самое просроченное
		var oldestK string
		var oldestT time.Time
		first := true
		for k, it := range c.items {
			if first || it.expires.Before(oldestT) {
				oldestK, oldestT, first = k, it.expires, false
			}
		}
		if oldestK != "" {
			delete(c.items, oldestK)
		}
	}
	c.items[key] = cacheItem{bests: bests, expires: time.Now().Add(c.ttl)}
}
