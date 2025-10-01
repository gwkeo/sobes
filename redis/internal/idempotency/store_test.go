package idempotency_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/idempotency"
)

func newStore(t *testing.T) *idempotency.Store {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return idempotency.NewStore(rdb, "test:idem", time.Hour, time.Second)
}

func TestCompleteAndReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const key = "key-12345678"
	const hash = "hash-1"

	begin, err := s.Begin(ctx, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if begin.Token == "" || begin.Replay != nil {
		t.Fatalf("unexpected begin: %+v", begin)
	}

	// Пока первый запрос выполняется, второй с тем же ключом получает 409.
	if _, err := s.Begin(ctx, key, hash); !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("begin while in progress: err=%v", err)
	}

	rec := idempotency.Record{
		RequestHash: hash,
		Status:      201,
		Headers:     map[string]string{"Content-Type": "application/json"},
		Body:        `{"id":"p-2001"}`,
	}
	if err := s.Complete(ctx, key, begin.Token, rec); err != nil {
		t.Fatal(err)
	}

	replay, err := s.Begin(ctx, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Replay == nil || replay.Replay.Status != 201 || replay.Replay.Body != rec.Body {
		t.Fatalf("unexpected replay: %+v", replay.Replay)
	}

	if _, err := s.Begin(ctx, key, "another-hash"); !errors.Is(err, idempotency.ErrMismatch) {
		t.Fatalf("mismatch: err=%v", err)
	}
}

func TestAbortAllowsRetryAndOldOwnerCannotComplete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const key = "key-abcdefgh"
	const hash = "hash-1"

	begin, err := s.Begin(ctx, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Abort(ctx, key, begin.Token); err != nil {
		t.Fatal(err)
	}

	retry, err := s.Begin(ctx, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Token == "" || retry.Token == begin.Token {
		t.Fatalf("retry must get a new token: %+v", retry)
	}

	// Старый владелец потерял lock и не имеет права записывать результат.
	err = s.Complete(ctx, key, begin.Token, idempotency.Record{
		RequestHash: hash,
		Status:      201,
		Body:        `{"old":true}`,
	})
	if !errors.Is(err, idempotency.ErrLockLost) {
		t.Fatalf("old complete: err=%v, want ErrLockLost", err)
	}
}
