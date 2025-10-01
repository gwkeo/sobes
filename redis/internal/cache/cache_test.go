package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/cache"
)

func newCache(t *testing.T, opts cache.Options) (*cache.Cache[string], *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	keys := cache.NewKeys("test", "v1")
	return cache.New[string](cache.Deps{
		RDB:    rdb,
		Keys:   keys,
		Locker: cache.NewLocker(rdb, time.Second),
	}, opts), rdb
}

func baseOptions() cache.Options {
	return cache.Options{
		Name:         "test",
		Fresh:        100 * time.Millisecond,
		Stale:        2 * time.Second,
		NegativeTTL:  300 * time.Millisecond,
		L1TTL:        0,
		L1MaxEntries: 128,
		Jitter:       0,
		LockWait:     200 * time.Millisecond,
		OpTimeout:    300 * time.Millisecond,
		LoadTimeout:  2 * time.Second,
	}
}

func TestGetOrLoadMissThenRedisHit(t *testing.T) {
	c, _ := newCache(t, baseOptions())
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "value", true, nil
	}

	v, found, src, err := c.GetOrLoad(ctx, "key", load)
	if err != nil || !found || v != "value" || src != cache.SourceMiss {
		t.Fatalf("first load: v=%q found=%v src=%s err=%v", v, found, src, err)
	}

	v, found, src, err = c.GetOrLoad(ctx, "key", load)
	if err != nil || !found || v != "value" || src != cache.SourceRedis {
		t.Fatalf("second load: v=%q found=%v src=%s err=%v", v, found, src, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1", got)
	}
}

func TestL1ServesWithoutRedis(t *testing.T) {
	opts := baseOptions()
	opts.L1TTL = time.Second
	c, _ := newCache(t, opts)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "value", true, nil
	}

	if _, _, src, _ := c.GetOrLoad(ctx, "key", load); src != cache.SourceMiss {
		t.Fatalf("first source = %s", src)
	}
	if _, _, src, _ := c.GetOrLoad(ctx, "key", load); src != cache.SourceL1 {
		t.Fatalf("second source = %s, want l1", src)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSingleflightCollapsesConcurrentMisses(t *testing.T) {
	opts := baseOptions()
	opts.Fresh = 5 * time.Second
	opts.Stale = 10 * time.Second
	c, _ := newCache(t, opts)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return "value", true, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, found, _, err := c.GetOrLoad(ctx, "hot", load)
			if err != nil {
				errs <- err
				return
			}
			if !found || v != "value" {
				errs <- errors.New("unexpected value")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1", got)
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	opts := baseOptions()
	opts.Fresh = 50 * time.Millisecond
	opts.Stale = 2 * time.Second
	c, _ := newCache(t, opts)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "v", true, nil
	}

	if _, _, src, _ := c.GetOrLoad(ctx, "key", load); src != cache.SourceMiss {
		t.Fatalf("first source = %s", src)
	}
	time.Sleep(80 * time.Millisecond)

	v, found, src, err := c.GetOrLoad(ctx, "key", load)
	if err != nil || !found || v != "v" || src != cache.SourceStale {
		t.Fatalf("stale read: v=%q found=%v src=%s err=%v", v, found, src, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("background refresh did not happen, calls=%d", calls.Load())
	}

	if _, _, src, _ := c.GetOrLoad(ctx, "key", load); src != cache.SourceRedis {
		t.Fatalf("after refresh source = %s, want redis", src)
	}
}

func TestNegativeCaching(t *testing.T) {
	opts := baseOptions()
	opts.NegativeTTL = 100 * time.Millisecond
	c, _ := newCache(t, opts)
	ctx := context.Background()

	var calls atomic.Int32
	load := func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "", false, nil
	}

	if _, found, src, _ := c.GetOrLoad(ctx, "missing", load); found || src != cache.SourceMiss {
		t.Fatalf("first: found=%v src=%s", found, src)
	}
	if _, found, src, _ := c.GetOrLoad(ctx, "missing", load); found || src != cache.SourceNegative {
		t.Fatalf("second: found=%v src=%s", found, src)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}

	time.Sleep(150 * time.Millisecond)
	if _, found, src, _ := c.GetOrLoad(ctx, "missing", load); found || src != cache.SourceMiss {
		t.Fatalf("after negative TTL: found=%v src=%s", found, src)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestCorruptEntryIsDropped(t *testing.T) {
	c, rdb := newCache(t, baseOptions())
	ctx := context.Background()
	key := "test:v1:product:corrupt"

	if err := rdb.Set(ctx, key, "{not-json", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	v, found, src, err := c.GetOrLoad(ctx, key, func(context.Context) (string, bool, error) {
		calls.Add(1)
		return "fresh", true, nil
	})
	if err != nil || !found || v != "fresh" || src != cache.SourceMiss {
		t.Fatalf("v=%q found=%v src=%s err=%v", v, found, src, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}

	raw, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var entry cache.Entry[string]
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("entry was not replaced: %v", err)
	}
	if !entry.Found || entry.Value != "fresh" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestLockerReleasesOnlyOwnToken(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	l := cache.NewLocker(rdb, time.Second)
	token, ok, err := l.Acquire(ctx, "lock:key")
	if err != nil || !ok || token == "" {
		t.Fatalf("acquire: token=%q ok=%v err=%v", token, ok, err)
	}
	if _, ok, _ := l.Acquire(ctx, "lock:key"); ok {
		t.Fatal("second acquire should fail")
	}
	if err := l.Release(ctx, "lock:key", "wrong-token"); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, "lock:key").Result(); n != 1 {
		t.Fatal("lock must still exist after wrong-token release")
	}
	if err := l.Release(ctx, "lock:key", token); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.Exists(ctx, "lock:key").Result(); n != 0 {
		t.Fatal("lock must be deleted by owner")
	}
}
