package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/cache"
	"redis-catalog/internal/catalog"
	"redis-catalog/internal/config"
	"redis-catalog/internal/events"
	"redis-catalog/internal/httpapi"
	"redis-catalog/internal/idempotency"
	"redis-catalog/internal/model"
	"redis-catalog/internal/obs"
	"redis-catalog/internal/ratelimit"
	"redis-catalog/internal/store"
)

type App struct {
	log     *slog.Logger
	rdb     *redis.Client
	handler http.Handler

	cancel context.CancelFunc
	wg     *sync.WaitGroup
	once   sync.Once
}

func New(parent context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	if log == nil {
		log = slog.Default()
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:            cfg.Redis.Addr,
		Password:        cfg.Redis.Password,
		DB:              cfg.Redis.DB,
		PoolSize:        cfg.Redis.PoolSize,
		MinIdleConns:    cfg.Redis.MinIdleConns,
		DialTimeout:     cfg.Redis.DialTimeout,
		ReadTimeout:     cfg.Redis.ReadTimeout,
		WriteTimeout:    cfg.Redis.WriteTimeout,
		MaxRetries:      1,
		ClientName:      "redis-catalog",
		ConnMaxIdleTime: 5 * time.Minute,
	})
	rdb.AddHook(obs.RedisHook{})

	pingCtx, cancelPing := context.WithTimeout(parent, time.Second)
	err := rdb.Ping(pingCtx).Err()
	cancelPing()
	if err != nil {
		if cfg.Redis.Required {
			_ = rdb.Close()
			return nil, fmt.Errorf("redis is required but unavailable at %s: %w", cfg.Redis.Addr, err)
		}
		log.Warn("redis is unavailable at startup; running in degraded mode (DB fallback)", "addr", cfg.Redis.Addr, "err", err)
		obs.RedisUp.Set(0)
	} else {
		log.Info("redis connected", "addr", cfg.Redis.Addr, "db", cfg.Redis.DB)
		obs.RedisUp.Set(1)
	}

	keys := cache.NewKeys(cfg.Cache.Prefix, cfg.Cache.Version)
	locker := cache.NewLocker(rdb, cfg.Cache.LockTTL)
	memStore := store.NewMemory(cfg.StoreLatency)
	observer := obs.CacheObserver{}

	products := cache.New[model.Product](cache.Deps{
		RDB:      rdb,
		Keys:     keys,
		Locker:   locker,
		Logger:   log,
		Observer: observer,
	}, cache.Options{
		Name:         "product",
		Fresh:        cfg.Cache.ProductFresh,
		Stale:        cfg.Cache.ProductStale,
		NegativeTTL:  cfg.Cache.NegativeTTL,
		L1TTL:        cfg.Cache.L1TTL,
		L1MaxEntries: cfg.Cache.L1MaxEntries,
		Jitter:       cfg.Cache.Jitter,
		LockWait:     cfg.Cache.LockWait,
		OpTimeout:    cfg.Cache.OpTimeout,
		LoadTimeout:  cfg.Cache.LoadTimeout,
	})

	lists := cache.New[[]model.Product](cache.Deps{
		RDB:      rdb,
		Keys:     keys,
		Locker:   locker,
		Logger:   log,
		Observer: observer,
	}, cache.Options{
		Name:         "list",
		Fresh:        cfg.Cache.ListFresh,
		Stale:        cfg.Cache.ListStale,
		L1TTL:        cfg.Cache.L1TTL,
		L1MaxEntries: cfg.Cache.L1MaxEntries,
		Jitter:       cfg.Cache.Jitter,
		LockWait:     cfg.Cache.LockWait,
		OpTimeout:    cfg.Cache.OpTimeout,
		LoadTimeout:  cfg.Cache.LoadTimeout,
	})

	publisher := events.NewPublisher(rdb, keys.EventsChannel(), log)
	svc := catalog.New(catalog.Deps{
		Store:          memStore,
		RDB:            rdb,
		Keys:           keys,
		Products:       products,
		Lists:          lists,
		Publisher:      publisher,
		Logger:         log,
		CacheOpTimeout: cfg.Cache.OpTimeout,
	})

	ctx, cancel := context.WithCancel(parent)
	subscriber := events.NewSubscriber(rdb, keys.EventsChannel(), svc.HandleCacheEvent, log)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		subscriber.Run(ctx)
	}()

	// Фоновый health-check, чтобы gauge redis_up не зависел от того,
	// дергали ли readiness probe.
	wg.Add(1)
	go func() {
		defer wg.Done()
		healthLoop(ctx, rdb, log)
	}()

	idemStore := idempotency.NewStore(rdb, keys.Base()+":idem", cfg.Idempotency.TTL, cfg.Idempotency.LockTTL)
	limiter := ratelimit.New(rdb, ratelimit.Options{
		Prefix:    keys.Base() + ":rl",
		Limit:     cfg.RateLimit.Limit,
		Window:    cfg.RateLimit.Window,
		OpTimeout: cfg.Cache.OpTimeout,
		Logger:    log,
	})

	handler := httpapi.NewRouter(httpapi.Deps{
		Service:     svc,
		Limiter:     limiter,
		Idempotency: idemStore,
		Logger:      log,
		EnableDebug: cfg.EnableDebug,
		RedisPing: func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		},
	})

	return &App{
		log:     log,
		rdb:     rdb,
		handler: handler,
		cancel:  cancel,
		wg:      wg,
	}, nil
}

func (a *App) Handler() http.Handler { return a.handler }

func (a *App) Close() error {
	var err error
	a.once.Do(func() {
		a.cancel()
		done := make(chan struct{})
		go func() {
			a.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			a.log.Error("app: background goroutines did not stop in time", "stack", string(buf[:n]))
		}
		err = a.rdb.Close()
	})
	return err
}

func healthLoop(ctx context.Context, rdb *redis.Client, log *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	up := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, time.Second)
			err := rdb.Ping(pingCtx).Err()
			cancel()
			if err != nil {
				obs.RedisUp.Set(0)
				if up {
					log.Warn("redis health check failed; cache is bypassed", "err", err)
					up = false
				}
				continue
			}
			obs.RedisUp.Set(1)
			if !up {
				log.Info("redis is back")
				up = true
			}
		}
	}
}
