package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/idempotency"
)

func TestIdempotencyMiddlewareInProgressAndReplay(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := idempotency.NewStore(rdb, "test:idem", time.Hour, 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	h := newIdempotencyMiddleware(store, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"p-1"}`)
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	post := func() (*http.Response, string) {
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Idempotency-Key", "test-key-123456")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(body)
	}

	firstDone := make(chan *http.Response, 1)
	go func() {
		resp, _ := post()
		firstDone <- resp
	}()
	<-started

	resp, _ := post()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("in-progress status = %d, want 409", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After for in-progress request")
	}

	close(release)
	if resp := <-firstDone; resp.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d", resp.StatusCode)
	}

	resp, body := post()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatal("replay header is missing")
	}
	if body != `{"id":"p-1"}` {
		t.Fatalf("replay body = %q", body)
	}
}
