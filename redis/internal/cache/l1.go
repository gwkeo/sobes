package cache

import (
	"sync"
	"time"
)

type l1Item[T any] struct {
	value     T
	expiresAt time.Time
}

// L1 — маленький in-process кэш перед Redis.
//
// Зачем: самые горячие ключи обслуживаются без сетевого round-trip.
// Цена: данные могут быть stale на других инстансах, поэтому TTL короткий,
// а при изменении мы рассылаем invalidation через Pub/Sub.
//
// Реализация намеренно простая (TTL + случайное вытеснение при переполнении).
// В проде обычно берут ristretto / lru с нормальной политикой и метриками.
type L1[T any] struct {
	mu    sync.RWMutex
	items map[string]l1Item[T]
	max   int
}

func NewL1[T any](max int) *L1[T] {
	if max <= 0 {
		max = 1024
	}
	return &L1[T]{
		items: make(map[string]l1Item[T], 64),
		max:   max,
	}
}

func (c *L1[T]) Get(key string) (T, bool) {
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		var zero T
		return zero, false
	}
	if !time.Now().Before(item.expiresAt) {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		var zero T
		return zero, false
	}
	return item.value, true
}

func (c *L1[T]) Set(key string, value T, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.items) >= c.max {
		// Случайное вытеснение: O(1), без отдельного списка. Для L1 это ок —
		// промах стоит одного похода в Redis.
		for k := range c.items {
			delete(c.items, k)
			break
		}
	}
	c.items[key] = l1Item[T]{value: value, expiresAt: now.Add(ttl)}
}

func (c *L1[T]) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

func (c *L1[T]) Purge() {
	c.mu.Lock()
	c.items = make(map[string]l1Item[T], 64)
	c.mu.Unlock()
}

func (c *L1[T]) Info(key string) (ttl time.Duration, ok bool) {
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return 0, false
	}
	remaining := time.Until(item.expiresAt)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

func (c *L1[T]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
