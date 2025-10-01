package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"redis-catalog/internal/obs"
	"redis-catalog/internal/ratelimit"
)

func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					reqLog := log.With("request_id", w.Header().Get("X-Request-ID"))
					reqLog.Error("http: panic", "panic", rec, "stack", string(debug.Stack()))
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func requestIDMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
			if id == "" || len(id) > 128 {
				id = newRequestID()
			}
			w.Header().Set("X-Request-ID", id)
			reqLog := log.With("request_id", id)
			ctx := context.WithValue(r.Context(), ctxRequestID, id)
			ctx = context.WithValue(ctx, ctxLogger, reqLog)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func accessLogMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				route := r.Pattern
				if route == "" {
					route = "unmatched"
				}
				loggerFrom(r.Context(), log).Info("http request",
					"method", r.Method,
					"path", r.URL.Path,
					"route", route,
					"status", rec.Status(),
					"bytes", rec.bytes,
					"duration", time.Since(start),
				)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			obs.ObserveHTTP(r.Method, route, rec.Status(), time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

func rateLimitMiddleware(limiter *ratelimit.Limiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil || skipRateLimit(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			res, err := limiter.Allow(r.Context(), clientIdentity(r))
			if err != nil {
				// Fail-open: Redis — не единственная точка отказа. Для
				// security-critical лимитов можно fail-closed или локальный fallback.
				loggerFrom(r.Context(), log).Warn("ratelimit: redis error, allowing request", "err", err)
				obs.RateLimitErrors.Inc()
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(res.Limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(res.Remaining))
			if !res.Allowed {
				obs.RateLimitRejected.Inc()
				retrySeconds := int(math.Ceil(res.RetryAfter.Seconds()))
				if retrySeconds < 1 {
					retrySeconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
				writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, retry later")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func skipRateLimit(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	default:
		return false
	}
}

func clientIdentity(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return "key:" + key
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return "ip:" + host
	}
	return "ip:" + r.RemoteAddr
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
