package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"redis-catalog/internal/idempotency"
	"redis-catalog/internal/obs"
)

const maxStoredResponseBody = 256 << 10 // 256 KiB

type idempotencyMiddleware struct {
	store *idempotency.Store
	log   *slog.Logger
}

func newIdempotencyMiddleware(store *idempotency.Store, log *slog.Logger) func(http.Handler) http.Handler {
	m := &idempotencyMiddleware{store: store, log: log}
	return m.wrap
}

func (m *idempotencyMiddleware) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required for POST")
			return
		}
		if len(key) < 8 || len(key) > 255 {
			writeError(w, http.StatusBadRequest, "idempotency_key_invalid", "Idempotency-Key must be between 8 and 255 characters")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "cannot read request body")
			return
		}
		if len(body) > maxRequestBody {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		requestHash := hashRequest(r, body)
		begin, err := m.store.Begin(r.Context(), key, requestHash)
		if err != nil {
			switch {
			case errors.Is(err, idempotency.ErrMismatch):
				obs.IdempotencyEvents.WithLabelValues("mismatch").Inc()
				writeError(w, http.StatusUnprocessableEntity, "idempotency_mismatch",
					"this Idempotency-Key was already used with a different request")
				return
			case errors.Is(err, idempotency.ErrInProgress):
				obs.IdempotencyEvents.WithLabelValues("in_progress").Inc()
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusConflict, "idempotency_in_progress",
					"a request with this Idempotency-Key is already in progress")
				return
			}
			// Идемпотентность — важная, но не единственная защита. Если Redis
			// недоступен, не блокируем запись: лучше возможный дубль, чем отказ.
			m.log.Warn("idempotency: redis error, executing without protection", "err", err)
			obs.IdempotencyEvents.WithLabelValues("error").Inc()
			next.ServeHTTP(w, r)
			return
		}

		if begin.Replay != nil {
			obs.IdempotencyEvents.WithLabelValues("replayed").Inc()
			for k, v := range begin.Replay.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Idempotency-Replayed", "true")
			w.Header().Set("Content-Length", strconv.Itoa(len(begin.Replay.Body)))
			w.WriteHeader(begin.Replay.Status)
			_, _ = io.WriteString(w, begin.Replay.Body)
			return
		}

		obs.IdempotencyEvents.WithLabelValues("new").Inc()
		rec := newBufferedResponse()
		next.ServeHTTP(rec, r)

		status := rec.Status()
		completeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		defer cancel()

		if status >= 200 && status < 300 {
			if rec.buf.Len() <= maxStoredResponseBody {
				record := idempotency.Record{
					RequestHash: requestHash,
					Status:      status,
					Headers:     storeResponseHeaders(rec.header),
					Body:        rec.buf.String(),
				}
				if err := m.store.Complete(completeCtx, key, begin.Token, record); err != nil {
					m.log.Warn("idempotency: failed to store response", "err", err)
					obs.IdempotencyEvents.WithLabelValues("error").Inc()
				}
			} else {
				m.log.Warn("idempotency: response too large to store", "status", status, "bytes", rec.buf.Len())
				_ = m.store.Abort(completeCtx, key, begin.Token)
				obs.IdempotencyEvents.WithLabelValues("released").Inc()
			}
		} else {
			// 4xx/5xx не кэшируем: клиент может исправить запрос и повторить
			// с тем же ключом.
			_ = m.store.Abort(completeCtx, key, begin.Token)
			obs.IdempotencyEvents.WithLabelValues("released").Inc()
		}

		copyBufferedResponse(w, rec)
	})
}

func hashRequest(r *http.Request, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(r.Method))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(r.URL.RequestURI()))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func storeResponseHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vv := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Date", "Transfer-Encoding", "Connection", "X-Request-Id":
			continue
		}
		out[k] = strings.Join(vv, ", ")
	}
	return out
}
