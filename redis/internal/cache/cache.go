package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Source — откуда handler получил данные. Отдаём в X-Cache, чтобы вживую
// видеть, как работает многоуровневый кэш.
type Source string

const (
	SourceL1       Source = "l1"
	SourceRedis    Source = "redis"
	SourceStale    Source = "stale"
	SourceMiss     Source = "miss"
	SourceNegative Source = "negative"
	SourceBypass   Source = "bypass" // Redis недоступен, сходили в БД напрямую
)

type Loader[T any] func(ctx context.Context) (value T, found bool, err error)

type Observer interface {
	CacheEvent(cache, layer, result string)
	CacheDuration(cache, op string, d time.Duration)
}

type NopObserver struct{}

func (NopObserver) CacheEvent(string, string, string)           {}
func (NopObserver) CacheDuration(string, string, time.Duration) {}

type Deps struct {
	RDB      redis.UniversalClient
	Keys     Keys
	Locker   *Locker
	Logger   *slog.Logger
	Observer Observer
}

type Options struct {
	Name        string
	Fresh       time.Duration
	Stale       time.Duration
	NegativeTTL time.Duration

	L1TTL        time.Duration
	L1MaxEntries int

	Jitter      float64
	LockWait    time.Duration
	OpTimeout   time.Duration
	LoadTimeout time.Duration
}

// Cache — типизированный cache-aside с:
//   - L1 (in-process) + L2 (Redis);
//   - stale-while-revalidate;
//   - negative caching;
//   - singleflight внутри процесса + distributed lock между инстансами;
//   - graceful degradation: ошибки Redis не роняют запрос, а ведут в БД.
type Cache[T any] struct {
	name   string
	rdb    redis.UniversalClient
	keys   Keys
	locker *Locker
	log    *slog.Logger
	obs    Observer

	l1 *L1[T]

	group        singleflight.Group
	refreshGroup singleflight.Group

	fresh       time.Duration
	stale       time.Duration
	negativeTTL time.Duration
	jitter      float64
	l1TTL       time.Duration
	lockWait    time.Duration
	opTimeout   time.Duration
	loadTimeout time.Duration
}

func New[T any](deps Deps, opts Options) *Cache[T] {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Observer == nil {
		deps.Observer = NopObserver{}
	}
	if opts.OpTimeout <= 0 {
		opts.OpTimeout = 300 * time.Millisecond
	}
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = 5 * time.Second
	}
	if opts.LockWait <= 0 {
		opts.LockWait = 300 * time.Millisecond
	}
	c := &Cache[T]{
		name:        opts.Name,
		rdb:         deps.RDB,
		keys:        deps.Keys,
		locker:      deps.Locker,
		log:         deps.Logger,
		obs:         deps.Observer,
		l1:          NewL1[T](opts.L1MaxEntries),
		fresh:       opts.Fresh,
		stale:       opts.Stale,
		negativeTTL: opts.NegativeTTL,
		jitter:      opts.Jitter,
		l1TTL:       opts.L1TTL,
		lockWait:    opts.LockWait,
		opTimeout:   opts.OpTimeout,
		loadTimeout: opts.LoadTimeout,
	}
	return c
}

func (c *Cache[T]) GetOrLoad(ctx context.Context, key string, load Loader[T]) (T, bool, Source, error) {
	if c.l1TTL > 0 {
		if v, ok := c.l1.Get(key); ok {
			c.obs.CacheEvent(c.name, "l1", "hit")
			return v, true, SourceL1, nil
		}
		c.obs.CacheEvent(c.name, "l1", "miss")
	}

	entry, state, err := c.read(ctx, key, true)
	if err != nil {
		// Redis недоступен/таймаут. Это не ошибка запроса: идём в БД.
		r, loadErr := c.loadBypass(ctx, key, load)
		return r.value, r.found, r.source, loadErr
	}

	switch state {
	case stateFresh:
		c.fillL1(key, entry)
		return entry.Value, entry.Found, SourceRedis, nil
	case stateNegative:
		var zero T
		return zero, false, SourceNegative, nil
	case stateStale:
		c.startRefresh(key, load)
		return entry.Value, entry.Found, SourceStale, nil
	default:
		r, loadErr := c.loadDedup(ctx, key, load)
		return r.value, r.found, r.source, loadErr
	}
}

func (c *Cache[T]) Delete(ctx context.Context, key string) error {
	c.l1.Delete(key)
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()
	start := time.Now()
	err := c.rdb.Del(opCtx, key).Err()
	c.obs.CacheDuration(c.name, "delete", time.Since(start))
	if err != nil {
		c.obs.CacheEvent(c.name, "redis", "error")
		return err
	}
	return nil
}

// InvalidateLocal используется подписчиком Pub/Sub: сбрасываем только L1,
// Redis-ключ уже удалён автором изменения (или дождётся TTL).
func (c *Cache[T]) InvalidateLocal(key string) { c.l1.Delete(key) }

func (c *Cache[T]) PurgeLocal() { c.l1.Purge() }

func (c *Cache[T]) LocalLen() int { return c.l1.Len() }

type Inspection[T any] struct {
	Key         string    `json:"key"`
	RedisExists bool      `json:"redis_exists"`
	RedisTTLMS  int64     `json:"redis_ttl_ms"`
	Entry       *Entry[T] `json:"entry,omitempty"`
	L1Present   bool      `json:"l1_present"`
	L1TTLMS     int64     `json:"l1_ttl_ms"`
	LockTTLMS   int64     `json:"lock_ttl_ms"`
}

func (c *Cache[T]) Inspect(ctx context.Context, key string) (Inspection[T], error) {
	ctx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	ins := Inspection[T]{Key: key}
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		return ins, err
	}
	if err == nil {
		ins.RedisExists = true
		var entry Entry[T]
		if json.Unmarshal(raw, &entry) == nil {
			ins.Entry = &entry
		}
	}
	if ttl, err := c.rdb.PTTL(ctx, key).Result(); err == nil && ttl > 0 {
		ins.RedisTTLMS = ttl.Milliseconds()
	}
	if ttl, ok := c.l1.Info(key); ok {
		ins.L1Present = true
		ins.L1TTLMS = ttl.Milliseconds()
	}
	if ttl, err := c.locker.TTL(ctx, c.keys.Lock(key)); err == nil && ttl > 0 {
		ins.LockTTLMS = ttl.Milliseconds()
	}
	return ins, nil
}

type entryState int

const (
	stateMiss entryState = iota
	stateFresh
	stateStale
	stateNegative
)

func (c *Cache[T]) read(ctx context.Context, key string, observe bool) (Entry[T], entryState, error) {
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	start := time.Now()
	raw, err := c.rdb.Get(opCtx, key).Bytes()
	c.obs.CacheDuration(c.name, "get", time.Since(start))

	if err != nil {
		if errors.Is(err, redis.Nil) {
			if observe {
				c.obs.CacheEvent(c.name, "redis", "miss")
			}
			return Entry[T]{}, stateMiss, nil
		}
		if observe {
			c.obs.CacheEvent(c.name, "redis", "error")
		}
		return Entry[T]{}, stateMiss, err
	}

	var entry Entry[T]
	if err := json.Unmarshal(raw, &entry); err != nil || entry.V != entryVersion {
		// Старый формат/битые данные: не отдаём мусор, удаляем ключ.
		c.log.Warn("cache: dropping corrupt entry", "cache", c.name, "key", key, "version", entry.V, "err", err)
		_ = c.rdb.Del(opCtx, key).Err()
		if observe {
			c.obs.CacheEvent(c.name, "redis", "corrupt")
		}
		return Entry[T]{}, stateMiss, nil
	}

	now := time.Now()
	if !entry.Found {
		if now.UnixMilli() < entry.StaleUntilMS {
			if observe {
				c.obs.CacheEvent(c.name, "redis", "negative")
			}
			return entry, stateNegative, nil
		}
		return entry, stateMiss, nil
	}

	switch {
	case now.UnixMilli() < entry.FreshUntilMS:
		if observe {
			c.obs.CacheEvent(c.name, "redis", "hit")
		}
		return entry, stateFresh, nil
	case now.UnixMilli() < entry.StaleUntilMS:
		if observe {
			c.obs.CacheEvent(c.name, "redis", "stale")
		}
		return entry, stateStale, nil
	default:
		if observe {
			c.obs.CacheEvent(c.name, "redis", "miss")
		}
		return entry, stateMiss, nil
	}
}

type loadResult[T any] struct {
	value  T
	found  bool
	source Source
}

func (c *Cache[T]) loadBypass(ctx context.Context, key string, load Loader[T]) (loadResult[T], error) {
	return c.do(ctx, "bypass:"+key, func(loadCtx context.Context) (loadResult[T], error) {
		v, found, err := load(loadCtx)
		if err != nil {
			return loadResult[T]{}, err
		}
		return loadResult[T]{value: v, found: found, source: SourceBypass}, nil
	})
}

func (c *Cache[T]) loadDedup(ctx context.Context, key string, load Loader[T]) (loadResult[T], error) {
	return c.do(ctx, "load:"+key, func(loadCtx context.Context) (loadResult[T], error) {
		// Двойная проверка: пока мы ждали singleflight, другой goroutine
		// мог уже наполнить кэш.
		if entry, state, err := c.read(loadCtx, key, false); err == nil {
			switch state {
			case stateFresh:
				c.fillL1(key, entry)
				return loadResult[T]{value: entry.Value, found: entry.Found, source: SourceRedis}, nil
			case stateNegative:
				return loadResult[T]{found: false, source: SourceNegative}, nil
			}
		}

		lockKey := c.keys.Lock(key)
		token, acquired, lockErr := c.locker.Acquire(loadCtx, lockKey)
		if lockErr == nil && acquired {
			defer func() {
				if err := c.locker.Release(loadCtx, lockKey, token); err != nil {
					c.log.Debug("cache: lock release failed", "key", lockKey, "err", err)
				}
			}()

			// Ещё одна проверка уже под локом: между read и Acquire другой
			// инстанс мог сходить в БД и записать значение.
			if entry, state, err := c.read(loadCtx, key, false); err == nil {
				switch state {
				case stateFresh:
					c.fillL1(key, entry)
					return loadResult[T]{value: entry.Value, found: entry.Found, source: SourceRedis}, nil
				case stateNegative:
					return loadResult[T]{found: false, source: SourceNegative}, nil
				}
			}

			v, found, err := load(loadCtx)
			if err != nil {
				return loadResult[T]{}, err
			}
			entry := c.set(loadCtx, key, v, found)
			c.fillL1(key, entry)
			return loadResult[T]{value: v, found: found, source: SourceMiss}, nil
		}

		if lockErr == nil && !acquired {
			// Кто-то другой уже грузит. Подождём чуть-чуть и проверим кэш,
			// чтобы не делать ту же работу параллельно.
			if r, ok := c.waitForFill(loadCtx, key); ok {
				return r, nil
			}
		}

		// Lock не взяли/Redis недоступен/долго ждали — деградируем:
		// лучше лишний запрос в БД, чем ошибка или вечное ожидание.
		v, found, err := load(loadCtx)
		if err != nil {
			return loadResult[T]{}, err
		}
		entry := c.set(loadCtx, key, v, found)
		c.fillL1(key, entry)
		return loadResult[T]{value: v, found: found, source: SourceMiss}, nil
	})
}

func (c *Cache[T]) waitForFill(ctx context.Context, key string) (loadResult[T], bool) {
	deadline := time.NewTimer(c.lockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return loadResult[T]{}, false
		case <-deadline.C:
			return loadResult[T]{}, false
		case <-ticker.C:
			entry, state, err := c.read(ctx, key, false)
			if err != nil {
				continue
			}
			switch state {
			case stateFresh:
				c.fillL1(key, entry)
				return loadResult[T]{value: entry.Value, found: entry.Found, source: SourceRedis}, true
			case stateNegative:
				return loadResult[T]{found: false, source: SourceNegative}, true
			}
		}
	}
}

// startRefresh — stale-while-revalidate. Пользователь уже получил stale-значение,
// обновление идёт в фоне. refreshGroup не даёт запустить несколько refresh
// одного ключа в рамках процесса, distributed lock — в рамках всех инстансов.
func (c *Cache[T]) startRefresh(key string, load Loader[T]) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("cache: refresh panic", "cache", c.name, "key", key, "panic", r)
			}
		}()
		_, _, _ = c.refreshGroup.Do("refresh:"+key, func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), c.loadTimeout)
			defer cancel()

			if entry, state, err := c.read(ctx, key, false); err == nil && state == stateFresh {
				c.fillL1(key, entry)
				return nil, nil
			}

			lockKey := c.keys.Lock(key)
			token, acquired, err := c.locker.Acquire(ctx, lockKey)
			if err != nil || !acquired {
				return nil, nil
			}
			defer func() { _ = c.locker.Release(ctx, lockKey, token) }()

			if entry, state, err := c.read(ctx, key, false); err == nil && state == stateFresh {
				c.fillL1(key, entry)
				return nil, nil
			}

			v, found, err := load(ctx)
			if err != nil {
				c.log.Warn("cache: background refresh failed", "cache", c.name, "key", key, "err", err)
				return nil, nil
			}
			entry := c.set(ctx, key, v, found)
			c.fillL1(key, entry)
			return nil, nil
		})
	}()
}

func (c *Cache[T]) set(ctx context.Context, key string, value T, found bool) Entry[T] {
	now := time.Now()
	fresh := jitter(c.fresh, c.jitter)
	stale := jitter(c.stale, c.jitter)
	if !found {
		if c.negativeTTL <= 0 {
			return Entry[T]{}
		}
		fresh = jitter(c.negativeTTL, c.jitter)
		stale = fresh
	}

	entry := Entry[T]{
		V:            entryVersion,
		Found:        found,
		Value:        value,
		CachedAtMS:   now.UnixMilli(),
		FreshUntilMS: now.Add(fresh).UnixMilli(),
		StaleUntilMS: now.Add(stale).UnixMilli(),
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		c.log.Error("cache: marshal failed", "cache", c.name, "key", key, "err", err)
		return entry
	}

	// Redis TTL = граница hard-stale + небольшой запас. После неё ключ
	// исчезнет сам, даже если инвалидация потерялась.
	ttl := time.Until(time.UnixMilli(entry.StaleUntilMS)) + time.Second
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	start := time.Now()
	err = c.rdb.Set(opCtx, key, payload, ttl).Err()
	c.obs.CacheDuration(c.name, "set", time.Since(start))
	if err != nil {
		c.obs.CacheEvent(c.name, "redis", "error")
		c.log.Warn("cache: set failed", "cache", c.name, "key", key, "err", err)
	}
	return entry
}

func (c *Cache[T]) fillL1(key string, entry Entry[T]) {
	if c.l1TTL <= 0 || !entry.Found {
		return
	}
	remaining := entry.FreshIn(time.Now())
	if remaining <= 0 {
		return
	}
	if remaining > c.l1TTL {
		remaining = c.l1TTL
	}
	c.l1.Set(key, entry.Value, remaining)
}

func (c *Cache[T]) do(ctx context.Context, key string, fn func(context.Context) (loadResult[T], error)) (loadResult[T], error) {
	ch := c.group.DoChan(key, func() (any, error) {
		// Отвязываем загрузку от контекста первого клиента: если он отменит
		// запрос, остальные ожидающие не должны получить его ошибку.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.loadTimeout)
		defer cancel()
		r, err := fn(loadCtx)
		if err != nil {
			return nil, err
		}
		return r, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return loadResult[T]{}, res.Err
		}
		r, ok := res.Val.(loadResult[T])
		if !ok {
			return loadResult[T]{}, fmt.Errorf("cache: unexpected singleflight result type")
		}
		return r, nil
	case <-ctx.Done():
		return loadResult[T]{}, ctx.Err()
	}
}

func jitter(d time.Duration, pct float64) time.Duration {
	if d <= 0 || pct <= 0 {
		return d
	}
	// Только положительный jitter: иначе можно получить stale < fresh.
	return d + time.Duration(rand.Float64()*pct*float64(d))
}
