package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config собирается из переменных окружения. Все значения имеют безопасные
// дефолты, чтобы `go run ./cmd/api` работал сразу после `docker compose up -d redis`.
type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration
	StoreLatency    time.Duration
	EnableDebug     bool
	LogLevel        slog.Level
	LogFormat       string

	Redis       Redis
	Cache       Cache
	RateLimit   RateLimit
	Idempotency Idempotency
}

type Redis struct {
	Addr         string
	Password     string
	DB           int
	PoolSize     int
	MinIdleConns int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// Required=true превращает недоступность Redis на старте в фатальную ошибку.
	// В демо Redis — опциональный кэш, поэтому по умолчанию false (graceful degradation).
	Required bool
}

type Cache struct {
	Prefix  string
	Version string

	ProductFresh time.Duration
	ProductStale time.Duration
	ListFresh    time.Duration
	ListStale    time.Duration
	NegativeTTL  time.Duration

	L1TTL        time.Duration
	L1MaxEntries int

	LockTTL     time.Duration
	LockWait    time.Duration
	OpTimeout   time.Duration
	LoadTimeout time.Duration
	Jitter      float64
}

type RateLimit struct {
	Limit  int
	Window time.Duration
}

type Idempotency struct {
	TTL     time.Duration
	LockTTL time.Duration
}

func Default() Config {
	return Config{
		HTTPAddr:        ":8080",
		ShutdownTimeout: 10 * time.Second,
		StoreLatency:    80 * time.Millisecond,
		EnableDebug:     true,
		LogLevel:        slog.LevelInfo,
		LogFormat:       "text",
		Redis: Redis{
			Addr:         "localhost:6379",
			DB:           0,
			PoolSize:     20,
			MinIdleConns: 5,
			DialTimeout:  time.Second,
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
			Required:     false,
		},
		Cache: Cache{
			Prefix:       "shop",
			Version:      "v1",
			ProductFresh: 20 * time.Second,
			ProductStale: 3 * time.Minute,
			ListFresh:    15 * time.Second,
			ListStale:    2 * time.Minute,
			NegativeTTL:  20 * time.Second,
			L1TTL:        2 * time.Second,
			L1MaxEntries: 10_000,
			LockTTL:      3 * time.Second,
			LockWait:     300 * time.Millisecond,
			OpTimeout:    300 * time.Millisecond,
			LoadTimeout:  5 * time.Second,
			Jitter:       0.10,
		},
		RateLimit: RateLimit{
			Limit:  60,
			Window: time.Minute,
		},
		Idempotency: Idempotency{
			TTL:     24 * time.Hour,
			LockTTL: 30 * time.Second,
		},
	}
}

func Load() (Config, error) {
	cfg := Default()

	cfg.HTTPAddr = env("HTTP_ADDR", cfg.HTTPAddr)
	cfg.LogFormat = strings.ToLower(env("LOG_FORMAT", cfg.LogFormat))

	var err error
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return cfg, err
	}
	if cfg.StoreLatency, err = envDuration("STORE_LATENCY", cfg.StoreLatency); err != nil {
		return cfg, err
	}
	if cfg.EnableDebug, err = envBool("ENABLE_DEBUG", cfg.EnableDebug); err != nil {
		return cfg, err
	}
	if cfg.LogLevel, err = parseLevel(env("LOG_LEVEL", "info")); err != nil {
		return cfg, err
	}

	cfg.Redis.Addr = env("REDIS_ADDR", cfg.Redis.Addr)
	cfg.Redis.Password = env("REDIS_PASSWORD", cfg.Redis.Password)
	if cfg.Redis.DB, err = envInt("REDIS_DB", cfg.Redis.DB); err != nil {
		return cfg, err
	}
	if cfg.Redis.PoolSize, err = envInt("REDIS_POOL_SIZE", cfg.Redis.PoolSize); err != nil {
		return cfg, err
	}
	if cfg.Redis.MinIdleConns, err = envInt("REDIS_MIN_IDLE_CONNS", cfg.Redis.MinIdleConns); err != nil {
		return cfg, err
	}
	if cfg.Redis.DialTimeout, err = envDuration("REDIS_DIAL_TIMEOUT", cfg.Redis.DialTimeout); err != nil {
		return cfg, err
	}
	if cfg.Redis.ReadTimeout, err = envDuration("REDIS_READ_TIMEOUT", cfg.Redis.ReadTimeout); err != nil {
		return cfg, err
	}
	if cfg.Redis.WriteTimeout, err = envDuration("REDIS_WRITE_TIMEOUT", cfg.Redis.WriteTimeout); err != nil {
		return cfg, err
	}
	if cfg.Redis.Required, err = envBool("REDIS_REQUIRED", cfg.Redis.Required); err != nil {
		return cfg, err
	}

	cfg.Cache.Prefix = env("CACHE_PREFIX", cfg.Cache.Prefix)
	cfg.Cache.Version = env("CACHE_KEY_VERSION", cfg.Cache.Version)
	if cfg.Cache.ProductFresh, err = envDuration("CACHE_PRODUCT_FRESH_TTL", cfg.Cache.ProductFresh); err != nil {
		return cfg, err
	}
	if cfg.Cache.ProductStale, err = envDuration("CACHE_PRODUCT_STALE_TTL", cfg.Cache.ProductStale); err != nil {
		return cfg, err
	}
	if cfg.Cache.ListFresh, err = envDuration("CACHE_LIST_FRESH_TTL", cfg.Cache.ListFresh); err != nil {
		return cfg, err
	}
	if cfg.Cache.ListStale, err = envDuration("CACHE_LIST_STALE_TTL", cfg.Cache.ListStale); err != nil {
		return cfg, err
	}
	if cfg.Cache.NegativeTTL, err = envDuration("CACHE_NEGATIVE_TTL", cfg.Cache.NegativeTTL); err != nil {
		return cfg, err
	}
	if cfg.Cache.L1TTL, err = envDuration("CACHE_L1_TTL", cfg.Cache.L1TTL); err != nil {
		return cfg, err
	}
	if cfg.Cache.L1MaxEntries, err = envInt("CACHE_L1_MAX_ENTRIES", cfg.Cache.L1MaxEntries); err != nil {
		return cfg, err
	}
	if cfg.Cache.LockTTL, err = envDuration("CACHE_LOCK_TTL", cfg.Cache.LockTTL); err != nil {
		return cfg, err
	}
	if cfg.Cache.LockWait, err = envDuration("CACHE_LOCK_WAIT", cfg.Cache.LockWait); err != nil {
		return cfg, err
	}
	if cfg.Cache.OpTimeout, err = envDuration("CACHE_OP_TIMEOUT", cfg.Cache.OpTimeout); err != nil {
		return cfg, err
	}
	if cfg.Cache.LoadTimeout, err = envDuration("CACHE_LOAD_TIMEOUT", cfg.Cache.LoadTimeout); err != nil {
		return cfg, err
	}
	if cfg.Cache.Jitter, err = envFloat("CACHE_JITTER", cfg.Cache.Jitter); err != nil {
		return cfg, err
	}

	if cfg.RateLimit.Limit, err = envInt("RATE_LIMIT", cfg.RateLimit.Limit); err != nil {
		return cfg, err
	}
	if cfg.RateLimit.Window, err = envDuration("RATE_WINDOW", cfg.RateLimit.Window); err != nil {
		return cfg, err
	}

	if cfg.Idempotency.TTL, err = envDuration("IDEMPOTENCY_TTL", cfg.Idempotency.TTL); err != nil {
		return cfg, err
	}
	if cfg.Idempotency.LockTTL, err = envDuration("IDEMPOTENCY_LOCK_TTL", cfg.Idempotency.LockTTL); err != nil {
		return cfg, err
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch {
	case c.HTTPAddr == "":
		return fmt.Errorf("HTTP_ADDR must not be empty")
	case c.LogFormat != "text" && c.LogFormat != "json":
		return fmt.Errorf("LOG_FORMAT must be text or json")
	case c.ShutdownTimeout <= 0:
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be > 0")
	case c.StoreLatency < 0:
		return fmt.Errorf("STORE_LATENCY must be >= 0")
	case c.Redis.PoolSize <= 0:
		return fmt.Errorf("REDIS_POOL_SIZE must be > 0")
	case c.Redis.MinIdleConns < 0 || c.Redis.MinIdleConns > c.Redis.PoolSize:
		return fmt.Errorf("REDIS_MIN_IDLE_CONNS must be between 0 and REDIS_POOL_SIZE")
	case c.Cache.ProductStale <= c.Cache.ProductFresh:
		return fmt.Errorf("CACHE_PRODUCT_STALE_TTL must be greater than CACHE_PRODUCT_FRESH_TTL")
	case c.Cache.ListStale <= c.Cache.ListFresh:
		return fmt.Errorf("CACHE_LIST_STALE_TTL must be greater than CACHE_LIST_FRESH_TTL")
	case c.Cache.NegativeTTL <= 0:
		return fmt.Errorf("CACHE_NEGATIVE_TTL must be > 0")
	case c.Cache.L1TTL < 0:
		return fmt.Errorf("CACHE_L1_TTL must be >= 0")
	case c.Cache.L1MaxEntries <= 0:
		return fmt.Errorf("CACHE_L1_MAX_ENTRIES must be > 0")
	case c.Cache.LockTTL <= 0 || c.Cache.LockWait <= 0:
		return fmt.Errorf("CACHE_LOCK_TTL and CACHE_LOCK_WAIT must be > 0")
	case c.Cache.OpTimeout <= 0 || c.Cache.LoadTimeout <= 0:
		return fmt.Errorf("CACHE_OP_TIMEOUT and CACHE_LOAD_TIMEOUT must be > 0")
	case c.Cache.Jitter < 0 || c.Cache.Jitter > 1:
		return fmt.Errorf("CACHE_JITTER must be between 0 and 1")
	case c.RateLimit.Limit <= 0:
		return fmt.Errorf("RATE_LIMIT must be > 0")
	case c.RateLimit.Window <= 0:
		return fmt.Errorf("RATE_WINDOW must be > 0")
	case c.Idempotency.TTL <= 0 || c.Idempotency.LockTTL <= 0:
		return fmt.Errorf("IDEMPOTENCY_TTL and IDEMPOTENCY_LOCK_TTL must be > 0")
	}
	return nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL: unknown level %q", raw)
	}
}
