package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound — доменная ошибка «сущность не найдена». HTTP-слой маппит её в 404,
// а кэш — в negative cache (короткий TTL), чтобы не долбить БД одинаковыми запросами.
var ErrNotFound = errors.New("not found")

// ErrValidation — ошибка валидации входных данных (HTTP 400).
var ErrValidation = errors.New("validation error")

type Product struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	PriceCents  int64     `json:"price_cents"`
	Stock       int       `json:"stock"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateProductInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceCents  int64  `json:"price_cents"`
	Stock       int    `json:"stock"`
}

type UpdateProductInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceCents  int64  `json:"price_cents"`
	Stock       int    `json:"stock"`
}

func (in *CreateProductInput) Normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	return validate(in.Name, in.Category, in.PriceCents, in.Stock)
}

func (in *UpdateProductInput) Normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	return validate(in.Name, in.Category, in.PriceCents, in.Stock)
}

func validate(name, category string, priceCents int64, stock int) error {
	switch {
	case name == "":
		return validationErrorf("name is required")
	case len(name) > 200:
		return validationErrorf("name is too long (max 200 chars)")
	case category == "":
		return validationErrorf("category is required")
	case len(category) > 64:
		return validationErrorf("category is too long (max 64 chars)")
	case priceCents < 0:
		return validationErrorf("price_cents must be >= 0")
	case stock < 0:
		return validationErrorf("stock must be >= 0")
	}
	return nil
}

func validationErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
