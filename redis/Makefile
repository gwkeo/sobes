.PHONY: run build test test-race vet tidy redis-up redis-down up down logs

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

tidy:
	go mod tidy

redis-up:
	docker compose up -d redis

redis-down:
	docker compose stop redis

up:
	docker compose up --build -d

down:
	docker compose down -v

logs:
	docker compose logs -f api
