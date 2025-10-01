package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const entryVersion = 1

// Entry — «конверт» поверх значения. Хранит не только данные, но и границы
// свежести. Это даёт stale-while-revalidate: после FreshUntil значение ещё
// можно отдать пользователю, но нужно запустить фоновое обновление.
type Entry[T any] struct {
	V            int   `json:"v"`
	Found        bool  `json:"found"`
	Value        T     `json:"value"`
	CachedAtMS   int64 `json:"cached_at_ms"`
	FreshUntilMS int64 `json:"fresh_until_ms"`
	StaleUntilMS int64 `json:"stale_until_ms"`
}

func (e Entry[T]) FreshIn(now time.Time) time.Duration {
	return time.UnixMilli(e.FreshUntilMS).Sub(now)
}

func (e Entry[T]) StaleIn(now time.Time) time.Duration {
	return time.UnixMilli(e.StaleUntilMS).Sub(now)
}

var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// Locker — распределённый lock на SET NX PX + безопасном удалении по токену.
//
// Важно: этот lock — оптимизация против stampede, а не гарантия корректности.
// Для критичных инвариантов нужны fencing tokens/version в самой БД: lock может
// истечь, пока его владелец ещё работает.
type Locker struct {
	rdb redis.UniversalClient
	ttl time.Duration
}

func NewLocker(rdb redis.UniversalClient, ttl time.Duration) *Locker {
	return &Locker{rdb: rdb, ttl: ttl}
}

func (l *Locker) Acquire(ctx context.Context, key string) (token string, acquired bool, err error) {
	token = randomToken()
	ok, err := l.rdb.SetNX(ctx, key, token, l.ttl).Result()
	if err != nil {
		return "", false, err
	}
	return token, ok, nil
}

// Release удаляет lock только если в нём всё ещё наш токен. Иначе можно
// случайно снять чужой lock, который успел истечь и перейти другому процессу.
func (l *Locker) Release(ctx context.Context, key, token string) error {
	// Release не должен отменяться вместе с запросом: если клиент ушёл,
	// lock всё равно надо освободить.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	defer cancel()
	_, err := releaseLockScript.Run(ctx, l.rdb, []string{key}, token).Result()
	return err
}

func (l *Locker) TTL(ctx context.Context, key string) (time.Duration, error) {
	ttl, err := l.rdb.PTTL(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	if ttl < 0 {
		return 0, nil
	}
	return ttl, nil
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand на практике не падает; но не хотим зависеть от паники.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
