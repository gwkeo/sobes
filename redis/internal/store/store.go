package store

import (
	"context"

	"redis-catalog/internal/model"
)

// Store — интерфейс «настоящей БД». В демо реализован in-memory хранилищем
// с искусственной задержкой, чтобы было видно эффект кэша и stampede.
// В проде здесь был бы Postgres/MySQL/whatever.
type Store interface {
	Get(ctx context.Context, id string) (model.Product, error)
	List(ctx context.Context, category string, limit int) ([]model.Product, error)
	Create(ctx context.Context, in model.CreateProductInput) (model.Product, error)
	Update(ctx context.Context, id string, in model.UpdateProductInput) (model.Product, error)
	Delete(ctx context.Context, id string) error
	Ping(ctx context.Context) error
}
