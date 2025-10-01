package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"redis-catalog/internal/catalog"
	"redis-catalog/internal/idempotency"
	"redis-catalog/internal/model"
	"redis-catalog/internal/obs"
	"redis-catalog/internal/ratelimit"
)

type Deps struct {
	Service     *catalog.Service
	Limiter     *ratelimit.Limiter
	Idempotency *idempotency.Store
	Logger      *slog.Logger
	EnableDebug bool
	RedisPing   func(ctx context.Context) error
}

type API struct {
	svc         *catalog.Service
	log         *slog.Logger
	enableDebug bool
	redisPing   func(ctx context.Context) error
}

func NewRouter(deps Deps) http.Handler {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	api := &API{
		svc:         deps.Service,
		log:         log,
		enableDebug: deps.EnableDebug,
		redisPing:   deps.RedisPing,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", api.index)
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.ready)
	mux.Handle("GET /metrics", promhttp.Handler())

	mux.HandleFunc("GET /api/v1/products", api.listProducts)
	mux.HandleFunc("GET /api/v1/products/{id}", api.getProduct)
	mux.HandleFunc("PUT /api/v1/products/{id}", api.updateProduct)
	mux.HandleFunc("DELETE /api/v1/products/{id}", api.deleteProduct)

	create := http.Handler(http.HandlerFunc(api.createProduct))
	if deps.Idempotency != nil {
		create = newIdempotencyMiddleware(deps.Idempotency, log)(create)
	}
	mux.Handle("POST /api/v1/products", create)

	if deps.EnableDebug {
		mux.HandleFunc("GET /debug/cache/products/{id}", api.debugProductCache)
		mux.HandleFunc("DELETE /debug/cache/products/{id}", api.debugInvalidateProduct)
		mux.HandleFunc("GET /debug/stats", api.debugStats)
	}

	mux.HandleFunc("/", api.notFound)

	var handler http.Handler = mux
	handler = rateLimitMiddleware(deps.Limiter, log)(handler)
	handler = metricsMiddleware(handler)
	handler = accessLogMiddleware(log)(handler)
	handler = requestIDMiddleware(log)(handler)
	handler = recoverMiddleware(log)(handler)
	return handler
}

func (a *API) index(w http.ResponseWriter, r *http.Request) {
	endpoints := []string{
		"GET /healthz",
		"GET /readyz",
		"GET /metrics",
		"GET /api/v1/products?category=electronics&limit=20",
		"GET /api/v1/products/{id}",
		"POST /api/v1/products (Idempotency-Key required)",
		"PUT /api/v1/products/{id}",
		"DELETE /api/v1/products/{id}",
	}
	if a.enableDebug {
		endpoints = append(endpoints,
			"GET /debug/cache/products/{id}",
			"DELETE /debug/cache/products/{id}",
			"GET /debug/stats",
		)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "redis-catalog",
		"endpoints": endpoints,
	})
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()

	dbStatus := "up"
	code := http.StatusOK
	status := "ok"
	if err := a.svc.Ping(ctx); err != nil {
		dbStatus = "down"
		status = "unavailable"
		code = http.StatusServiceUnavailable
	}

	redisStatus := "unknown"
	if a.redisPing != nil {
		if err := a.redisPing(ctx); err != nil {
			redisStatus = "down"
			obs.RedisUp.Set(0)
			if status == "ok" {
				// Redis здесь — опциональный кэш: сервис продолжает работать,
				// поэтому readiness не 503, а degraded.
				status = "degraded"
			}
		} else {
			redisStatus = "up"
			obs.RedisUp.Set(1)
		}
	}

	writeJSON(w, code, map[string]string{
		"status": status,
		"db":     dbStatus,
		"redis":  redisStatus,
	})
}

func (a *API) getProduct(w http.ResponseWriter, r *http.Request) {
	p, src, err := a.svc.GetProduct(r.Context(), r.PathValue("id"))
	w.Header().Set("X-Cache", string(src))
	if err != nil {
		a.handleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimSpace(r.URL.Query().Get("category"))
	if len(category) > 64 {
		writeError(w, http.StatusBadRequest, "validation_error", "category is too long")
		return
	}

	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 100 {
			writeError(w, http.StatusBadRequest, "validation_error", "limit must be an integer between 1 and 100")
			return
		}
		limit = v
	}

	items, src, err := a.svc.ListProducts(r.Context(), category, limit)
	w.Header().Set("X-Cache", string(src))
	if err != nil {
		a.handleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

func (a *API) createProduct(w http.ResponseWriter, r *http.Request) {
	var in model.CreateProductInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}

	p, err := a.svc.CreateProduct(r.Context(), in)
	if err != nil {
		a.handleError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/products/"+p.ID)
	writeJSON(w, http.StatusCreated, p)
}

func (a *API) updateProduct(w http.ResponseWriter, r *http.Request) {
	var in model.UpdateProductInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeDecodeError(w, err)
		return
	}

	p, err := a.svc.UpdateProduct(r.Context(), r.PathValue("id"), in)
	if err != nil {
		a.handleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) deleteProduct(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteProduct(r.Context(), r.PathValue("id")); err != nil {
		a.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) debugProductCache(w http.ResponseWriter, r *http.Request) {
	ins, err := a.svc.InspectProduct(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis_error", "cannot inspect cache: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"inspection": ins,
		"stats":      a.svc.Stats(),
		"now_ms":     time.Now().UnixMilli(),
	})
}

func (a *API) debugInvalidateProduct(w http.ResponseWriter, r *http.Request) {
	a.svc.InvalidateProduct(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) debugStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.Stats())
}

func (a *API) notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

func (a *API) handleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "product not found")
	case errors.Is(err, model.ErrValidation):
		msg := strings.TrimPrefix(err.Error(), "validation error: ")
		writeError(w, http.StatusBadRequest, "validation_error", msg)
	case errors.Is(err, context.Canceled):
		// Клиент уже ушёл; писать ответ обычно бессмысленно.
		return
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "request timed out")
	default:
		loggerFrom(r.Context(), a.log).Error("http: handler error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
