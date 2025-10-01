package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript — атомарный sliding window log на ZSET.
//
// Почему не INCR+EXPIRE: фиксированное окно допускает burst на границе
// (limit запросов в конце окна + limit в начале следующего). Sliding window
// log держит точное число событий за последние window миллисекунд.
// Цена — память O(limit) на ключ; для больших лимитов лучше GCRA/token bucket.
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]

local cutoff = now - window
redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)

local count = redis.call('ZCARD', key)
if count >= limit then
	local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
	local retry = window
	if oldest[2] then
		retry = tonumber(oldest[2]) + window - now
		if retry < 0 then retry = 0 end
	end
	return {0, 0, retry}
end

redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, window + 1000)
return {1, limit - count - 1, window}
`)

type Options struct {
	Prefix    string
	Limit     int
	Window    time.Duration
	OpTimeout time.Duration
	Logger    *slog.Logger
}

type Limiter struct {
	rdb       redis.UniversalClient
	prefix    string
	limit     int
	window    time.Duration
	opTimeout time.Duration
	log       *slog.Logger

	// Now вынесен для тестов; в проде — time.Now.
	Now func() time.Time
}

type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

func New(rdb redis.UniversalClient, opts Options) *Limiter {
	if opts.OpTimeout <= 0 {
		opts.OpTimeout = 200 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Limiter{
		rdb:       rdb,
		prefix:    opts.Prefix,
		limit:     opts.Limit,
		window:    opts.Window,
		opTimeout: opts.OpTimeout,
		log:       opts.Logger,
		Now:       time.Now,
	}
}

func (l *Limiter) Allow(ctx context.Context, identity string) (Result, error) {
	now := l.Now()
	ctx, cancel := context.WithTimeout(ctx, l.opTimeout)
	defer cancel()

	// Идентичность может быть API-ключом — не храним её в Redis в открытом виде.
	sum := sha256.Sum256([]byte(identity))
	key := fmt.Sprintf("%s:%s", l.prefix, hex.EncodeToString(sum[:12]))
	member := fmt.Sprintf("%d-%016x", now.UnixMilli(), rand.Uint64())

	raw, err := slidingWindowScript.Run(ctx, l.rdb, []string{key},
		now.UnixMilli(), l.window.Milliseconds(), l.limit, member,
	).Int64Slice()
	if err != nil {
		return Result{Allowed: true, Limit: l.limit}, err
	}
	if len(raw) != 3 {
		return Result{Allowed: true, Limit: l.limit}, fmt.Errorf("ratelimit: unexpected script result %v", raw)
	}

	res := Result{
		Allowed:   raw[0] == 1,
		Limit:     l.limit,
		Remaining: int(raw[1]),
	}
	if raw[2] > 0 {
		res.RetryAfter = time.Duration(raw[2]) * time.Millisecond
	}
	return res, nil
}
