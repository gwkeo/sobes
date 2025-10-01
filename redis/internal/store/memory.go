package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"redis-catalog/internal/model"
)

// Memory — заглушка БД. Специально медленная (по умолчанию 80ms на запрос),
// чтобы в демо были видны:
//   - разница latency между cache hit и cache miss;
//   - эффект stampede, если убрать singleflight/лок;
//   - попадания в БД по счётчику queries.
type Memory struct {
	mu      sync.RWMutex
	items   map[string]model.Product
	latency time.Duration
	seq     atomic.Uint64
	queries atomic.Int64
}

var _ Store = (*Memory)(nil)

func NewMemory(latency time.Duration) *Memory {
	m := &Memory{
		items:   make(map[string]model.Product, 32),
		latency: latency,
	}
	m.seed()
	return m
}

func (m *Memory) seed() {
	now := time.Now().UTC()
	seed := []struct {
		name, description, category string
		price                       int64
		stock                       int
	}{
		{"Mechanical Keyboard", "Hot-swappable 75% keyboard with QMK", "electronics", 12900, 42},
		{"Noise Cancelling Headphones", "Over-ear ANC headphones, 30h battery", "electronics", 24900, 17},
		{"27\" 4K Monitor", "IPS panel, USB-C power delivery", "electronics", 39900, 8},
		{"USB-C Hub", "7-in-1 dock with HDMI and Ethernet", "electronics", 5900, 120},
		{"The Go Programming Language", "Donovan & Kernighan", "books", 4500, 31},
		{"Designing Data-Intensive Applications", "Kleppmann, 1st edition", "books", 6200, 12},
		{"Refactoring", "Martin Fowler, 2nd edition", "books", 7100, 6},
		{"Espresso Grinder", "Burr grinder for home espresso", "home", 18900, 15},
		{"Pour-over Kettle", "Gooseneck kettle with temperature control", "home", 8900, 23},
		{"Cast Iron Skillet", "12 inch, pre-seasoned", "home", 4200, 55},
		{"Yoga Mat", "6mm natural rubber mat", "sports", 3900, 64},
		{"Adjustable Dumbbells", "2x24kg, quick-change mechanism", "sports", 29900, 4},
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range seed {
		id := fmt.Sprintf("p-%d", 1001+i)
		created := now.Add(-time.Duration(i+1) * time.Hour)
		m.items[id] = model.Product{
			ID:          id,
			Name:        s.name,
			Description: s.description,
			Category:    s.category,
			PriceCents:  s.price,
			Stock:       s.stock,
			CreatedAt:   created,
			UpdatedAt:   created,
		}
	}
	m.seq.Store(2000)
}

func (m *Memory) Get(ctx context.Context, id string) (model.Product, error) {
	m.queries.Add(1)
	if err := m.sleep(ctx); err != nil {
		return model.Product{}, err
	}
	m.mu.RLock()
	p, ok := m.items[id]
	m.mu.RUnlock()
	if !ok {
		return model.Product{}, model.ErrNotFound
	}
	return p, nil
}

func (m *Memory) List(ctx context.Context, category string, limit int) ([]model.Product, error) {
	m.queries.Add(1)
	if err := m.sleep(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}

	m.mu.RLock()
	out := make([]model.Product, 0, limit)
	for _, p := range m.items {
		if category != "" && p.Category != category {
			continue
		}
		out = append(out, p)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Create(ctx context.Context, in model.CreateProductInput) (model.Product, error) {
	m.queries.Add(1)
	if err := m.sleep(ctx); err != nil {
		return model.Product{}, err
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("p-%d", m.seq.Add(1))
	p := model.Product{
		ID:          id,
		Name:        in.Name,
		Description: in.Description,
		Category:    in.Category,
		PriceCents:  in.PriceCents,
		Stock:       in.Stock,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	m.mu.Lock()
	m.items[id] = p
	m.mu.Unlock()
	return p, nil
}

func (m *Memory) Update(ctx context.Context, id string, in model.UpdateProductInput) (model.Product, error) {
	m.queries.Add(1)
	if err := m.sleep(ctx); err != nil {
		return model.Product{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok {
		return model.Product{}, model.ErrNotFound
	}
	p.Name = in.Name
	p.Description = in.Description
	p.Category = in.Category
	p.PriceCents = in.PriceCents
	p.Stock = in.Stock
	p.UpdatedAt = time.Now().UTC()
	m.items[id] = p
	return p, nil
}

func (m *Memory) Delete(ctx context.Context, id string) error {
	m.queries.Add(1)
	if err := m.sleep(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return model.ErrNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }

// Queries — сколько запросов реально дошло до «БД». Используем в тестах
// (доказываем, что singleflight схлопнул параллельные промахи) и в debug-ручке.
func (m *Memory) Queries() int64 { return m.queries.Load() }

func (m *Memory) sleep(ctx context.Context) error {
	if m.latency <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(m.latency)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
