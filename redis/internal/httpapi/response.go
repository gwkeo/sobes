package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
)

const maxRequestBody = 1 << 20 // 1 MiB

type ctxKey string

const (
	ctxRequestID ctxKey = "request_id"
	ctxLogger    ctxKey = "logger"
)

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorEnvelope
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Ловим мусор после первого JSON-объекта.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain exactly one JSON object")
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap нужен, чтобы работали http.ResponseController/Flusher.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusRecorder) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// bufferedResponse копит ответ в памяти, чтобы idempotency-middleware мог
// сохранить его в Redis и затем отдать клиенту.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *bufferedResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(p)
}

func (w *bufferedResponse) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func copyBufferedResponse(w http.ResponseWriter, rec *bufferedResponse) {
	for k, vv := range rec.header {
		w.Header()[k] = vv
	}
	body := rec.buf.Bytes()
	if rec.status != http.StatusNoContent && rec.status != http.StatusNotModified {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(rec.Status())
	_, _ = w.Write(body)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	if fallback == nil {
		return slog.Default()
	}
	return fallback
}
