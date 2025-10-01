package catalog

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/cache"
	"redis-catalog/internal/events"
	"redis-catalog/internal/model"
	"redis-catalog/internal/obs"
	"redis-catalog/internal/store"
)

type Deps struct {
	Store          store.Store
	RDB            redis.UniversalClient
	Keys           cache.Keys
	Products       *cache.Cache[model.Product]
	Lists          *cache.Cache[[]model.Product]
	Publisher      *events.Publisher
	Logger         *slog.Logger
	CacheOpTimeout time.Duration
}

type Service struct {
	store     store.Store
	rdb       redis.UniversalClient
	keys      cache.Keys
	products  *cache.Cache[model.Product]
	lists     *cache.Cache[[]model.Product]
	publisher *events.Publisher
	log       *slog.Logger
	opTimeout time.Duration
}

func New(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.CacheOpTimeout <= 0 {
		deps.CacheOpTimeout = 300 * time.Millisecond
	}
	return &Service{
		store:     deps.Store,
		rdb:       deps.RDB,
		keys:      deps.Keys,
		products:  deps.Products,
		lists:     deps.Lists,
		publisher: deps.Publisher,
		log:       deps.Logger,
		opTimeout: deps.CacheOpTimeout,
	}
}

func (s *Service) GetProduct(ctx context.Context, id string) (model.Product, cache.Source, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return model.Product{}, cache.SourceMiss, model.ErrValidation
	}

	p, found, src, err := s.products.GetOrLoad(ctx, s.keys.Product(id), func(loadCtx context.Context) (model.Product, bool, error) {
		p, err := s.store.Get(loadCtx, id)
		obs.DBQuery("get_product", err)
		if errors.Is(err, model.ErrNotFound) {
			return model.Product{}, false, nil
		}
		if err != nil {
			return model.Product{}, false, err
		}
		return p, true, nil
	})
	if err != nil {
		return model.Product{}, src, err
	}
	if !found {
		return model.Product{}, src, model.ErrNotFound
	}
	return p, src, nil
}

func (s *Service) ListProducts(ctx context.Context, category string, limit int) ([]model.Product, cache.Source, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	version := s.listVersion(ctx)
	key := s.keys.ProductList(category, limit, version)

	items, _, src, err := s.lists.GetOrLoad(ctx, key, func(loadCtx context.Context) ([]model.Product, bool, error) {
		items, err := s.store.List(loadCtx, category, limit)
		obs.DBQuery("list_products", err)
		if err != nil {
			return nil, false, err
		}
		return items, true, nil
	})
	if err != nil {
		return nil, src, err
	}
	if items == nil {
		items = []model.Product{}
	}
	return items, src, nil
}

func (s *Service) CreateProduct(ctx context.Context, in model.CreateProductInput) (model.Product, error) {
	if err := in.Normalize(); err != nil {
		return model.Product{}, err
	}
	p, err := s.store.Create(ctx, in)
	obs.DBQuery("create_product", err)
	if err != nil {
		return model.Product{}, err
	}
	s.invalidateProduct(ctx, p.ID)
	return p, nil
}

func (s *Service) UpdateProduct(ctx context.Context, id string, in model.UpdateProductInput) (model.Product, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return model.Product{}, model.ErrValidation
	}
	if err := in.Normalize(); err != nil {
		return model.Product{}, err
	}
	p, err := s.store.Update(ctx, id, in)
	obs.DBQuery("update_product", err)
	if err != nil {
		return model.Product{}, err
	}
	s.invalidateProduct(ctx, p.ID)
	return p, nil
}

func (s *Service) DeleteProduct(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return model.ErrValidation
	}
	if err := s.store.Delete(ctx, id); err != nil {
		obs.DBQuery("delete_product", err)
		return err
	}
	obs.DBQuery("delete_product", nil)
	s.invalidateProduct(ctx, id)
	return nil
}

// HandleCacheEvent вызывается подписчиком Pub/Sub на каждом инстансе.
// Redis-ключ уже удалён автором изменения; здесь сбрасываем только локальный L1.
func (s *Service) HandleCacheEvent(ev events.Event) {
	if ev.Type != events.ProductChanged || ev.ProductID == "" {
		return
	}
	s.products.InvalidateLocal(s.keys.Product(ev.ProductID))
	s.lists.PurgeLocal()
	s.log.Debug("cache: local invalidation", "product_id", ev.ProductID)
}

func (s *Service) InspectProduct(ctx context.Context, id string) (cache.Inspection[model.Product], error) {
	return s.products.Inspect(ctx, s.keys.Product(id))
}

func (s *Service) InvalidateProduct(ctx context.Context, id string) {
	s.invalidateProduct(ctx, id)
}

func (s *Service) Ping(ctx context.Context) error {
	return s.store.Ping(ctx)
}

func (s *Service) Stats() Stats {
	st := Stats{
		ProductL1: s.products.LocalLen(),
		ListL1:    s.lists.LocalLen(),
	}
	if q, ok := s.store.(interface{ Queries() int64 }); ok {
		st.DBQueries = q.Queries()
	}
	return st
}

type Stats struct {
	DBQueries int64 `json:"db_queries"`
	ProductL1 int   `json:"product_l1_entries"`
	ListL1    int   `json:"list_l1_entries"`
}

func (s *Service) invalidateProduct(ctx context.Context, id string) {
	key := s.keys.Product(id)

	if err := s.products.Delete(ctx, key); err != nil {
		// Не фатально: TTL ограничит время жизни старого значения.
		s.log.Warn("cache: product delete failed", "key", key, "err", err)
	}
	// L1 списков содержит старые элементы, а версия списка уже изменится.
	s.lists.PurgeLocal()

	if err := s.bumpListVersion(ctx); err != nil {
		s.log.Warn("cache: list version bump failed", "err", err)
	}
	s.publishProductChanged(id)
	s.scheduleSecondDelete(key)
}

// bumpListVersion делает все закэшированные списки недостижимыми: версия
// входит в ключ. Старые ключи не удаляем через KEYS/SCAN — они истекут по TTL.
func (s *Service) bumpListVersion(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opTimeout)
	defer cancel()
	_, err := s.rdb.Incr(ctx, s.keys.ListVersion()).Result()
	return err
}

func (s *Service) listVersion(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, s.opTimeout)
	defer cancel()
	raw, err := s.rdb.Get(ctx, s.keys.ListVersion()).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.log.Debug("cache: list version read failed", "err", err)
		}
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		s.log.Warn("cache: bad list version", "value", raw, "err", err)
		return 0
	}
	return v
}

func (s *Service) publishProductChanged(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := s.publisher.Publish(ctx, events.Event{
		Type:      events.ProductChanged,
		ProductID: id,
		At:        time.Now().UTC(),
	})
	if err != nil {
		// Pub/Sub не гарантирует доставку; при ошибке полагаемся на короткий TTL L1.
		s.log.Warn("cache: publish invalidation failed", "product_id", id, "err", err)
	}
}

// scheduleSecondDelete — известный приём «double delete». Между UPDATE в БД и
// DEL кэша конкурентный читатель мог успеть положить старое значение обратно.
// Второй DEL через небольшой интервал убирает такую гонку. Полностью проблема
// решается версионированием значений/более сложными схемами, но TTL + double
// delete — прагматичный минимум.
func (s *Service) scheduleSecondDelete(key string) {
	go func() {
		time.Sleep(250 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), s.opTimeout)
		defer cancel()
		if err := s.products.Delete(ctx, key); err != nil {
			s.log.Debug("cache: second delete failed", "key", key, "err", err)
		}
	}()
}
