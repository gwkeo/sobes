package idempotency

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrMismatch   = errors.New("idempotency key was reused with a different request")
	ErrInProgress = errors.New("request with this idempotency key is already in progress")
	ErrLockLost   = errors.New("idempotency lock is no longer owned")
)

// Record — сохранённый ответ на успешный запрос.
type Record struct {
	RequestHash string            `json:"request_hash"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body"`
	CreatedAt   time.Time         `json:"created_at"`
}

type BeginResult struct {
	Token string
	// Replay != nil — ключ уже использован, нужно вернуть сохранённый ответ.
	Replay *Record
}

type Store struct {
	rdb       redis.UniversalClient
	prefix    string
	ttl       time.Duration
	lockTTL   time.Duration
	opTimeout time.Duration
}

var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// completeScript атомарно проверяет владение lock и сохраняет ответ.
// Без этой проверки запрос, чей lock уже истёк, мог бы перезаписать ответ
// более нового запроса с тем же ключом.
var completeScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return 0
end
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
redis.call("DEL", KEYS[1])
return 1
`)

func NewStore(rdb redis.UniversalClient, prefix string, ttl, lockTTL time.Duration) *Store {
	return &Store{
		rdb:       rdb,
		prefix:    prefix,
		ttl:       ttl,
		lockTTL:   lockTTL,
		opTimeout: 300 * time.Millisecond,
	}
}

func (s *Store) Begin(ctx context.Context, key, requestHash string) (BeginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()

	respKey, lockKey := s.keys(key)

	if rec, err := s.getRecord(ctx, respKey, requestHash); err != nil {
		return BeginResult{}, err
	} else if rec != nil {
		return BeginResult{Replay: rec}, nil
	}

	token := newToken()
	ok, err := s.rdb.SetNX(ctx, lockKey, token, s.lockTTL).Result()
	if err != nil {
		return BeginResult{}, err
	}
	if ok {
		return BeginResult{Token: token}, nil
	}

	// Пока мы пытались взять lock, первый запрос мог успеть завершиться.
	if rec, err := s.getRecord(ctx, respKey, requestHash); err != nil {
		return BeginResult{}, err
	} else if rec != nil {
		return BeginResult{Replay: rec}, nil
	}
	return BeginResult{}, ErrInProgress
}

func (s *Store) Complete(ctx context.Context, key, token string, rec Record) error {
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()

	respKey, lockKey := s.keys(key)
	rec.CreatedAt = time.Now().UTC()
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	// Сохраняем ответ и отпускаем lock одной Lua-операцией: только владелец
	// lock имеет право записать результат.
	ok, err := completeScript.Run(ctx, s.rdb, []string{lockKey, respKey},
		token, payload, s.ttl.Milliseconds(),
	).Int()
	if err != nil {
		return err
	}
	if ok == 0 {
		return ErrLockLost
	}
	return nil
}

func (s *Store) Abort(ctx context.Context, key, token string) error {
	_, lockKey := s.keys(key)
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	return s.release(ctx, lockKey, token)
}

func (s *Store) getRecord(ctx context.Context, respKey, requestHash string) (*Record, error) {
	raw, err := s.rdb.Get(ctx, respKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if rec.RequestHash != requestHash {
		return nil, ErrMismatch
	}
	return &rec, nil
}

func (s *Store) release(ctx context.Context, lockKey, token string) error {
	_, err := releaseLockScript.Run(ctx, s.rdb, []string{lockKey}, token).Result()
	return err
}

func (s *Store) keys(key string) (respKey, lockKey string) {
	sum := sha256.Sum256([]byte(key))
	suffix := hex.EncodeToString(sum[:])
	// Hash tag {} держит оба ключа в одном слоте Redis Cluster:
	// иначе мультиключевой Lua-скрипт там работать не будет.
	tag := "{" + suffix + "}"
	return s.prefix + ":" + tag + ":resp", s.prefix + ":" + tag + ":lock"
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
