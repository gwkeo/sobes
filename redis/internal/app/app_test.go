package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"redis-catalog/internal/app"
	"redis-catalog/internal/config"
)

type testEnv struct {
	app    *app.App
	server *httptest.Server
	client *http.Client
	redis  *miniredis.Miniredis
}

func newTestEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	return newTestEnvWithRedis(t, mr, mutate)
}

func newTestEnvWithRedis(t *testing.T, mr *miniredis.Miniredis, mutate func(*config.Config)) *testEnv {
	t.Helper()

	cfg := config.Default()
	cfg.Redis.Addr = mr.Addr()
	cfg.StoreLatency = 0
	cfg.Cache.L1TTL = 100 * time.Millisecond
	cfg.Cache.ProductFresh = 2 * time.Second
	cfg.Cache.ProductStale = 10 * time.Second
	cfg.Cache.ListFresh = 2 * time.Second
	cfg.Cache.ListStale = 10 * time.Second
	cfg.Cache.NegativeTTL = 2 * time.Second
	cfg.Cache.Jitter = 0
	cfg.Cache.LockWait = 100 * time.Millisecond
	cfg.RateLimit.Limit = 1000
	cfg.EnableDebug = true
	if mutate != nil {
		mutate(&cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	server := httptest.NewServer(application.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = application.Close()
	})

	return &testEnv{
		app:    application,
		server: server,
		client: &http.Client{Timeout: 5 * time.Second},
		redis:  mr,
	}
}

func (e *testEnv) do(t *testing.T, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp, raw
}

func TestProductCacheLifecycle(t *testing.T) {
	env := newTestEnv(t, nil)

	resp, body := env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Cache"); got != "miss" {
		t.Fatalf("first X-Cache = %q, want miss", got)
	}

	resp, _ = env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	if got := resp.Header.Get("X-Cache"); got != "l1" {
		t.Fatalf("second X-Cache = %q, want l1", got)
	}

	time.Sleep(120 * time.Millisecond) // L1 истёк, Redis ещё свежий
	resp, _ = env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	if got := resp.Header.Get("X-Cache"); got != "redis" {
		t.Fatalf("third X-Cache = %q, want redis", got)
	}

	resp, body = env.do(t, http.MethodGet, "/debug/cache/products/p-1001", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("debug status = %d body=%s", resp.StatusCode, body)
	}
	var debug struct {
		Inspection struct {
			RedisExists bool `json:"redis_exists"`
			Entry       *struct {
				Found bool `json:"found"`
			} `json:"entry"`
		} `json:"inspection"`
	}
	if err := json.Unmarshal(body, &debug); err != nil {
		t.Fatal(err)
	}
	if !debug.Inspection.RedisExists || debug.Inspection.Entry == nil || !debug.Inspection.Entry.Found {
		t.Fatalf("unexpected debug payload: %s", body)
	}
}

func TestNotFoundIsNegativeCached(t *testing.T) {
	env := newTestEnv(t, nil)

	resp, _ := env.do(t, http.MethodGet, "/api/v1/products/missing", nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Cache") != "miss" {
		t.Fatalf("first: status=%d cache=%s", resp.StatusCode, resp.Header.Get("X-Cache"))
	}
	resp, _ = env.do(t, http.MethodGet, "/api/v1/products/missing", nil, nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Cache") != "negative" {
		t.Fatalf("second: status=%d cache=%s", resp.StatusCode, resp.Header.Get("X-Cache"))
	}
}

func TestUpdateInvalidatesProductAndLists(t *testing.T) {
	env := newTestEnv(t, nil)

	// Прогреваем L1 продукта и списка.
	env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)

	resp, listBody := env.do(t, http.MethodGet, "/api/v1/products?category=electronics&limit=20", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, listBody)
	}
	resp, _ = env.do(t, http.MethodGet, "/api/v1/products?category=electronics&limit=20", nil, nil)
	if got := resp.Header.Get("X-Cache"); got != "l1" {
		t.Fatalf("list second X-Cache=%q", got)
	}

	update := map[string]any{
		"name":        "Mechanical Keyboard X",
		"description": "new",
		"category":    "electronics",
		"price_cents": 9900,
		"stock":       3,
	}
	resp, body := env.do(t, http.MethodPut, "/api/v1/products/p-1001", update, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Cache") == "l1" {
		t.Fatal("product must not be served from stale L1 after update")
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Mechanical Keyboard X" {
		t.Fatalf("name = %q", p.Name)
	}

	resp, body = env.do(t, http.MethodGet, "/api/v1/products?category=electronics&limit=20", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Cache"); got == "l1" {
		t.Fatal("list must not be served from old L1 after update")
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range list.Items {
		if item.ID == "p-1001" {
			found = true
			if item.Name != "Mechanical Keyboard X" {
				t.Fatalf("stale list item name = %q", item.Name)
			}
		}
	}
	if !found {
		t.Fatal("updated product is missing from list")
	}
}

func TestIdempotentCreate(t *testing.T) {
	env := newTestEnv(t, nil)
	const key = "idem-test-123456"
	create := map[string]any{
		"name":        "Idempotent Product",
		"description": "d",
		"category":    "home",
		"price_cents": 1000,
		"stock":       5,
	}

	resp, body := env.do(t, http.MethodPost, "/api/v1/products", create, map[string]string{"Idempotency-Key": key})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status=%d body=%s", resp.StatusCode, body)
	}
	var first struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}

	resp, body = env.do(t, http.MethodPost, "/api/v1/products", create, map[string]string{"Idempotency-Key": key})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatal("missing Idempotency-Replayed header")
	}
	var replay struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay id=%q, want %q", replay.ID, first.ID)
	}

	other := map[string]any{
		"name":        "Different Product",
		"description": "d",
		"category":    "home",
		"price_cents": 1000,
		"stock":       5,
	}
	resp, body = env.do(t, http.MethodPost, "/api/v1/products", other, map[string]string{"Idempotency-Key": key})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = env.do(t, http.MethodPost, "/api/v1/products", other, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key status=%d body=%s", resp.StatusCode, body)
	}
}

func TestRateLimit(t *testing.T) {
	env := newTestEnv(t, func(cfg *config.Config) {
		cfg.RateLimit.Limit = 2
		cfg.RateLimit.Window = time.Minute
	})

	for i := 0; i < 2; i++ {
		resp, body := env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, resp.StatusCode, body)
		}
	}

	resp, body := env.do(t, http.MethodGet, "/api/v1/products/p-1001", nil, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestRedisDownFallsBackToDatabase(t *testing.T) {
	env := newTestEnv(t, nil)
	env.redis.Close()

	resp, body := env.do(t, http.MethodGet, "/api/v1/products/p-1005", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Cache"); got != "bypass" {
		t.Fatalf("X-Cache=%q, want bypass", got)
	}

	resp, body = env.do(t, http.MethodGet, "/readyz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready status=%d body=%s", resp.StatusCode, body)
	}
	var ready map[string]string
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	if ready["status"] != "degraded" || ready["redis"] != "down" {
		t.Fatalf("ready=%v", ready)
	}
}
