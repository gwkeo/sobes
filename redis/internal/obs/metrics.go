package obs

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

var (
	CacheEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "cache",
		Name:      "events_total",
		Help:      "Cache hits/misses/errors by cache and layer.",
	}, []string{"cache", "layer", "result"})

	CacheOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "catalog",
		Subsystem: "cache",
		Name:      "operation_duration_seconds",
		Help:      "Redis cache operation latency.",
		Buckets:   []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"cache", "op"})

	DBQueries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "store",
		Name:      "queries_total",
		Help:      "Queries that reached the database.",
	}, []string{"op", "result"})

	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "HTTP requests by route and status.",
	}, []string{"method", "route", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "catalog",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "HTTP request latency.",
		Buckets:   []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"method", "route"})

	RedisCommands = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "redis",
		Name:      "commands_total",
		Help:      "Redis commands issued by the client.",
	}, []string{"command", "status"})

	RedisCommandDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "catalog",
		Subsystem: "redis",
		Name:      "command_duration_seconds",
		Help:      "Redis command latency.",
		Buckets:   []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
	}, []string{"command"})

	RedisUp = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "catalog",
		Subsystem: "redis",
		Name:      "up",
		Help:      "1 if the last Redis PING succeeded, 0 otherwise.",
	})

	RateLimitRejected = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "http",
		Name:      "rate_limited_total",
		Help:      "Requests rejected by the rate limiter.",
	})

	RateLimitErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "http",
		Name:      "rate_limit_errors_total",
		Help:      "Rate limiter failures (requests are allowed in fail-open mode).",
	})

	IdempotencyEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "catalog",
		Subsystem: "http",
		Name:      "idempotency_events_total",
		Help:      "Idempotency outcomes (new, replayed, in_progress, mismatch, error).",
	}, []string{"result"})
)

type CacheObserver struct{}

func (CacheObserver) CacheEvent(cache, layer, result string) {
	CacheEvents.WithLabelValues(cache, layer, result).Inc()
}

func (CacheObserver) CacheDuration(cache, op string, d time.Duration) {
	CacheOpDuration.WithLabelValues(cache, op).Observe(d.Seconds())
}

func DBQuery(op string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	DBQueries.WithLabelValues(op, result).Inc()
}

func ObserveHTTP(method, route string, status int, d time.Duration) {
	HTTPRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	HTTPDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

func statusClass(status int) string {
	// Не кардинализируем метрику полным кодом: 2xx/3xx/4xx/5xx достаточно.
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// RedisHook меряет команды go-redis. redis.Nil — это штатный cache miss,
// а не ошибка, поэтому в status=error он не попадает.
type RedisHook struct{}

var _ redis.Hook = RedisHook{}

func (RedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		status := "ok"
		if err != nil && !errors.Is(err, redis.Nil) {
			status = "error"
		}
		RedisCommands.WithLabelValues(cmd.Name(), status).Inc()
		RedisCommandDuration.WithLabelValues(cmd.Name()).Observe(time.Since(start).Seconds())
		return err
	}
}

func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		status := "ok"
		if err != nil && !errors.Is(err, redis.Nil) {
			status = "error"
		}
		RedisCommands.WithLabelValues("pipeline", status).Inc()
		RedisCommandDuration.WithLabelValues("pipeline").Observe(time.Since(start).Seconds())
		return err
	}
}
