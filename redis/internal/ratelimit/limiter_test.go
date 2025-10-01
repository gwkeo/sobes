package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/ratelimit"
)

func newLimiter(t *testing.T, limit int) (*ratelimit.Limiter, *time.Time, *time.Duration) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	l := ratelimit.New(rdb, ratelimit.Options{
		Prefix:    "test:rl",
		Limit:     limit,
		Window:    time.Minute,
		OpTimeout: time.Second,
	})
	now := time.Unix(1_700_000_000, 0)
	offset := time.Duration(0)
	l.Now = func() time.Time { return now.Add(offset) }
	return l, &now, &offset
}

func TestSlidingWindow(t *testing.T) {
	l, _, offset := newLimiter(t, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		res, err := l.Allow(ctx, "client")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Allowed {
			t.Fatalf("request %d must be allowed, res=%+v", i+1, res)
		}
		if res.Remaining != 3-i-1 {
			t.Fatalf("remaining = %d, want %d", res.Remaining, 3-i-1)
		}
	}

	res, err := l.Allow(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("4th request must be rejected")
	}
	if res.RetryAfter <= 0 {
		t.Fatalf("retry after = %v, want > 0", res.RetryAfter)
	}

	*offset = 61 * time.Second
	res, err = l.Allow(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Allowed {
		t.Fatal("request after window must be allowed")
	}
}

func TestIdentitiesAreIndependent(t *testing.T) {
	l, _, _ := newLimiter(t, 1)
	ctx := context.Background()

	if res, _ := l.Allow(ctx, "a"); !res.Allowed {
		t.Fatal("a must be allowed")
	}
	if res, _ := l.Allow(ctx, "a"); res.Allowed {
		t.Fatal("second a must be rejected")
	}
	if res, _ := l.Allow(ctx, "b"); !res.Allowed {
		t.Fatal("b must be allowed")
	}
}
