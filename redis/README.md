# redis-catalog: Go + Redis без «hello world»

Небольшой сервис каталога товаров, в котором Redis используется так, как он обычно
используется в проде: как **кэш перед БД**, а не как «записали значение по ключу».

Проект специально сделан как учебный, но с реальными механизмами:

- cache-aside с двумя уровнями: **L1 в процессе + L2 в Redis**;
- **stale-while-revalidate** (отдаём старые данные, пока обновляем их в фоне);
- **negative caching** — кэшируем не только найденные, но и отсутствующие сущности;
- **защита от stampede**: `singleflight` внутри инстанса + распределённый lock в Redis;
- **TTL с jitter**, чтобы ключи не протухали синхронно;
- инвалидация через **delete + версионирование ключей + Pub/Sub + double delete**;
- **rate limiting** на Lua и ZSET (sliding window log);
- **идемпотентность POST** через `Idempotency-Key` с атомарным завершением на Lua;
- **graceful degradation**: Redis упал — сервис продолжает работать через БД;
- Prometheus-метрики, health/readiness, debug-инспекция кэша;
- тесты на `miniredis`, включая гонки и отказ Redis.

«База данных» здесь — in-memory заглушка с искусственной задержкой: так видно, когда
запрос реально дошёл до БД, а когда его поглотил кэш. Всё, что не относится к Redis
(настоящая БД, auth, транзакции), намеренно упрощено — см. раздел
[«Что упрощено»](#что-упрощено-и-что-делать-в-проде).

---

## Быстрый старт

Нужен Docker (или локальный Redis + Go 1.25).

```bash
# Redis + API в контейнерах
docker compose up --build -d

curl -s localhost:8080/healthz
curl -s localhost:8080/api/v1/products/p-1001
```

Локальный запуск:

```bash
docker compose up -d redis
go run ./cmd/api
# в другом терминале
curl -s localhost:8080/api/v1/products/p-1001
```

Все настройки имеют дефолты, поэтому `go run` работает сразу после старта Redis.
Если хочется переопределить окружение из `.env.example`:

```bash
set -a; source .env.example; set +a
go run ./cmd/api
```

Тесты не требуют Docker — используется `miniredis`:

```bash
make test
make test-race
```

---

## Архитектура

```text
                      ┌────────────────────────────────────────────┐
 HTTP :8080           │ middleware: request-id → log → metrics →   │
 ───────────────────► │ rate limit → idempotency → handlers        │
                      └───────────────────┬────────────────────────┘
                                          │
                                  catalog.Service
                    ┌─────────────────────┼──────────────────────┐
                    ▼                     ▼                      ▼
              L1 (in-process)       Redis L2 (cache)       Store («БД»)
              map + TTL             envelope + TTL         in-memory + latency
                    │                     │
                    │                     ├── distributed locks (SET NX PX + Lua)
                    │                     ├── list version (INCR)
                    │                     ├── rate limit (ZSET + Lua)
                    │                     └── idempotency (SET NX + Lua)
                    │
                    └──────────── Pub/Sub invalidation ────────────┐
                                                                   ▼
                                                    все инстансы сбрасывают свой L1
```

Ключевая идея: **Redis — не источник истины**. Источник истины — БД; Redis можно
потерять, перезапустить или временно отключить, и сервис продолжит отвечать.

---

## API

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/healthz` | liveness, всегда 200 |
| `GET` | `/readyz` | зависит ли сервис от БД/Redis; Redis down → `degraded`, но 200 |
| `GET` | `/metrics` | Prometheus |
| `GET` | `/api/v1/products?category=electronics&limit=20` | список с кэшем и версией |
| `GET` | `/api/v1/products/{id}` | карточка товара |
| `POST` | `/api/v1/products` | создание, требует `Idempotency-Key` |
| `PUT` | `/api/v1/products/{id}` | обновление + инвалидация |
| `DELETE` | `/api/v1/products/{id}` | удаление + инвалидация |
| `GET` | `/debug/cache/products/{id}` | ключ, TTL, конверт, L1, lock |
| `DELETE` | `/debug/cache/products/{id}` | руками сбросить кэш товара |
| `GET` | `/debug/stats` | сколько запросов дошло до БД, размеры L1 |

`X-Cache` в ответе показывает, откуда взялись данные:

| Значение | Что произошло |
|---|---|
| `l1` | попадание в локальный кэш процесса |
| `redis` | попадание в Redis, значение свежее |
| `stale` | значение просрочено, но отдано; refresh запущен в фоне |
| `miss` | промах, сходили в БД |
| `negative` | в Redis лежит negative-запись (товара нет) |
| `bypass` | Redis недоступен, сразу пошли в БД |

---

## Сценарии, которые стоит потрогать

### 1. Cache-aside и два уровня кэша

```bash
for i in 1 2 3; do
  curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1001 | grep -i x-cache
  sleep 2.1   # больше CACHE_L1_TTL=2s, но меньше fresh TTL=20s
done
```

Ожидаемо:

```text
X-Cache: miss     # сходили в «БД» (~80ms)
X-Cache: l1       # L1, без сети
X-Cache: redis    # L1 истёк, но Redis ещё свежий
```

Что лежит в Redis:

```bash
docker exec -it redis-catalog-redis redis-cli
> GET shop:v1:product:p-1001
> PTTL shop:v1:product:p-1001
> SCAN 0 MATCH shop:v1:* COUNT 100
```

Значение — не голый JSON товара, а «конверт»:

```json
{
  "v": 1,
  "found": true,
  "value": { "id": "p-1001", "name": "Mechanical Keyboard", "...": "..." },
  "cached_at_ms": 1794039937227,
  "fresh_until_ms": 1794039957227,
  "stale_until_ms": 1794040117227
}
```

`fresh_until` — до этого момента отдаём как есть. Между `fresh_until` и
`stale_until` — можно отдать старое и обновить в фоне. После `stale_until` —
считаем промахом. Redis TTL ставится на `stale_until + 1s`, чтобы ключ исчез сам,
даже если инвалидация потерялась.

### 2. Negative caching

```bash
curl -s -o /dev/null -D - localhost:8080/api/v1/products/nope | grep -i x-cache
curl -s -o /dev/null -D - localhost:8080/api/v1/products/nope | grep -i x-cache
```

```text
X-Cache: miss
X-Cache: negative
```

Второй запрос не идёт в БД. Negative-запись живёт `CACHE_NEGATIVE_TTL` (20s), потом
снова будет попытка. Это защита от **cache penetration** — потока запросов
несуществующих ключей.

### 3. Stale-while-revalidate

Для наглядности перезапустим сервис с коротким fresh TTL:

```bash
CACHE_PRODUCT_FRESH_TTL=2s CACHE_PRODUCT_STALE_TTL=30s CACHE_L1_TTL=500ms \
  go run ./cmd/api
```

```bash
curl -s localhost:8080/debug/stats          # db_queries: 0
curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1002 | grep -i x-cache  # miss
sleep 2.2
curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1002 | grep -i x-cache  # stale
sleep 0.5
curl -s localhost:8080/debug/stats          # db_queries: 2, refresh уже прошёл
curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1002 | grep -i x-cache  # l1/redis
```

Пользователь не ждёт похода в БД: получает stale-значение сразу, а обновление идёт
в фоне. В это время `singleflight` + распределённый lock не дают нескольким
запросам (в том числе с разных инстансов) запустить несколько refresh.

### 4. Stampede: 50 параллельных запросов — один поход в БД

```bash
# на сервере с RATE_LIMIT=1000, иначе часть запросов получит 429
docker exec redis-catalog-redis redis-cli DEL shop:v1:product:p-1010 >/dev/null
curl -s localhost:8080/debug/stats
seq 1 50 | xargs -P50 -I{} curl -s -o /dev/null localhost:8080/api/v1/products/p-1010
curl -s localhost:8080/debug/stats
```

В `db_queries` прибавится **единица** (в пределах одного инстанса; между
инстансами работает Redis-lock). Без защиты было бы до 50 запросов в БД.

Как это устроено:

1. `singleflight` схлопывает одинаковые промахи внутри процесса;
2. победитель берёт `SET lock:product:p-1010 <token> NX PX 3000`;
3. остальные ждут ~до `CACHE_LOCK_WAIT` и проверяют кэш;
4. если lock взять не удалось и кэш так и не появился — идём в БД напрямую
   (лучше лишний запрос, чем ошибка/вечное ожидание);
5. lock снимается Lua-скриптом только если в нём всё ещё **наш** token.

### 5. Инвалидация: товар, список и другие инстансы

```bash
curl -s -X PUT localhost:8080/api/v1/products/p-1001 \
  -H 'Content-Type: application/json' \
  -d '{"name":"Mechanical Keyboard v2","description":"updated","category":"electronics","price_cents":13900,"stock":40}'

curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1001 | grep -i x-cache
# miss: ключ удалён, первый запрос снова идёт в БД

docker exec redis-catalog-redis redis-cli GET shop:v1:listver
# счётчик увеличился
```

При изменении:

1. обновляем БД;
2. `DEL` ключа товара (не `SET` — почему, разобрано ниже);
3. `INCR catalog:listver` — версия входит в ключ списков, поэтому все старые
   списки становятся недостижимы без `SCAN`/`KEYS` и удаления по шаблону;
4. `PUBLISH` события — остальные инстансы сбрасывают свой L1;
5. через 250ms делаем второй `DEL` (double delete) — страховка от гонки, когда
   конкурентный читатель успел положить старое значение обратно.

Проверить Pub/Sub можно, запустив два инстанса на 8080 и 8081: прогрейте L1 на
втором, обновите товар через первый — в логах второго появится
`cache: local invalidation`, а следующий `GET` уже не будет `l1`. В демо у каждого
инстанса своя in-memory «БД», поэтому после инвалидации второй перечитает свою
копию; в реальном проекте БД общая, и он увидит новое значение.

### 6. Rate limit на Lua

```bash
RATE_LIMIT=3 RATE_WINDOW=1m go run ./cmd/api
for i in 1 2 3 4; do
  curl -s -o /dev/null -w "req $i: %{http_code}\n" \
    -H 'X-API-Key: demo-key' localhost:8080/api/v1/products/p-1001
done
```

```text
req 1: 200
req 2: 200
req 3: 200
req 4: 429
```

В ответах есть `X-RateLimit-Limit`, `X-RateLimit-Remaining`, у 429 — `Retry-After`.
Идентичность — `X-API-Key`, иначе IP. В Redis это ZSET, а не `INCR`: sliding window
log не даёт двойного burst на границе окна. Скрипт атомарный, поэтому несколько
инстансов не могут одновременно «увидеть» остаток и пропустить лишнее.

### 7. Idempotency-Key

```bash
KEY="demo-$(date +%s)"
BODY='{"name":"Idempotent Product","description":"d","category":"home","price_cents":1000,"stock":5}'

curl -s -X POST localhost:8080/api/v1/products \
  -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' -d "$BODY"

curl -s -i -X POST localhost:8080/api/v1/products \
  -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' -d "$BODY" \
  | grep -i -E 'HTTP|Idempotency-Replayed'
```

Второй запрос вернёт тот же `201` и `Idempotency-Replayed: true`, не создавая
второй товар. Если тем же ключом отправить другой body — `422`. Если первый запрос
ещё выполняется — `409` + `Retry-After`.

Кэшируются только `2xx`-ответы; `4xx/5xx` освобождают ключ, чтобы клиент мог
исправить запрос и повторить.

### 8. Redis упал

```bash
docker compose stop redis

curl -s -o /dev/null -D - localhost:8080/api/v1/products/p-1007 | grep -i x-cache
# X-Cache: bypass, ответ 200, но чуть медленнее

curl -s localhost:8080/readyz
# {"db":"up","redis":"down","status":"degraded"}

docker compose start redis
# сервис сам переподключится, кэш снова заработает
```

Redis здесь опциональный: ошибки кэша логируются и не превращаются в ошибку
запроса. Плата — задержка на таймауты Redis (`CACHE_OP_TIMEOUT`, по умолчанию
300ms), поэтому в проде дополнительно нужен circuit breaker / локальный fallback.

### 9. Метрики

```bash
curl -s localhost:8080/metrics | grep -E 'catalog_(cache|store|redis|http)' | head -40
```

Что смотреть:

- `catalog_cache_events_total{cache="product",layer="redis",result="hit|miss|stale|...}"`
- `catalog_cache_operation_duration_seconds`
- `catalog_store_queries_total` — сколько реально дошло до БД;
- `catalog_redis_commands_total{status="error"}` и `catalog_redis_up`;
- `catalog_http_requests_total{route,status}`, `..._duration_seconds`;
- `catalog_http_rate_limited_total`, `catalog_http_idempotency_events_total`.

Хороший дашборд: hit ratio по слоям, p95/p99 latency Redis-команд, доля ошибок,
число eviction'ов (`INFO stats`), размер L1.

---

## Как это устроено внутри

### Ключи и версия кэша

Все ключи строятся в одном месте — `internal/cache/keys.go`:

```text
shop:v1:product:{id}
shop:v1:list:{listVersion}:{category}:{limit}
shop:v1:listver
shop:v1:lock:{cacheKey}
shop:v1:events
shop:v1:rl:{sha256(identity)}
shop:v1:idem:{sha256(key)}:resp|lock
```

`CACHE_KEY_VERSION` — дешёвый способ «обнулить» весь кэш при несовместимом
изменении формата: подняли версию, старые ключи доживают свой TTL и больше не
читаются. Для списков используется отдельный счётчик версии, чтобы не удалять
ключи по шаблону.

### TTL, jitter и stale-while-revalidate

`internal/cache/cache.go`, `Entry`:

- `FreshUntilMS` — граница «можно отдавать без refresh»;
- `StaleUntilMS` — граница «можно отдавать, но пора обновлять»;
- Redis TTL = `StaleUntilMS + 1s`;
- к `fresh`/`stale` добавляется случайный положительный jitter (`CACHE_JITTER`),
  чтобы много ключей не протухло в одну миллисекунду (cache avalanche).

Если значение stale, `GetOrLoad` возвращает его сразу и запускает
`startRefresh` в отдельной goroutine. Refresh защищён `singleflight` и тем же
распределённым lock, что и обычная загрузка.

### Защита от stampede

`singleflight.Group` — только внутри процесса. Между инстансами:

```text
SET shop:v1:lock:product:p-1 <random-token> NX PX 3000
```

Освобождение — сравниваем token и удаляем только своё:

```lua
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
```

Если бы удаляли просто `DEL`, можно было бы снять чужой lock, который успел
истечь и перейти другому процессу. Но даже с token lock — **оптимизация, а не
гарантия корректности**: он может истечь, пока владелец ещё работает. Для
критичных инвариантов нужны fencing tokens / версии в БД.

### Negative cache

Если `store.Get` вернул `ErrNotFound`, кэш кладёт `Entry{Found:false}` с коротким
TTL. В L1 negative-записи не кладём: они нужны прежде всего чтобы отсечь поток
одинаковых 404 на уровне Redis.

Если в Redis окажется старый/битый формат, `read` удалит ключ и обработает это как
промах. `v` в конверте позволяет заметить несовместимую схему.

### Инвалидация и её гонки

Используется схема «delete after commit», а не «set after commit»:

- `SET` после записи может проиграть гонку и оставить в кэше старое значение;
- `DEL` безопаснее: следующий читатель загрузит актуальное из БД.

Но и у `DEL` есть окно: читатель мог прочитать старое значение из БД **до**
коммита, а записать в кэш **после** `DEL`. Поэтому:

- TTL ограничивает время жизни такой записи;
- второй `DEL` через 250ms убирает типичную гонку;
- для строгой консистентности в проде используют versioned values, CDC/outbox,
  транзакционную публикацию событий или отказ от кэша для критичных чтений.

Списки инвалидируются версией: `INCR listver` делает старые ключи недостижимыми,
не требуя `SCAN`/`KEYS` (которые блокируют Redis на больших объёмах).

### Rate limiting

`internal/ratelimit/limiter.go` — sliding window log:

```lua
ZREMRANGEBYSCORE key -inf (now - window)
count = ZCARD key
if count >= limit then return {0, retryAfter} end
ZADD key now member
PEXPIRE key window+1000
return {1, remaining}
```

Плюсы: точный лимит, нет burst на границе окна. Минусы: память `O(limit)` на
ключ. Для больших лимитов обычно берут token bucket/GCRA. Идентичность хэшируется,
чтобы не хранить API-ключи в Redis в открытом виде.

При ошибке Redis лимитер **fail-open**: запрос пропускаем, пишем метрику
`rate_limit_errors_total`. Для security-critical сценариев политику нужно
выбирать осознанно (fail-closed или локальный fallback).

Время сейчас берётся из приложения. Если между инстансами возможен заметный
рассинхрон часов, надёжнее брать `TIME` внутри Lua — тогда все решения принимает
сам Redis.

### Идемпотентность

`internal/idempotency/store.go` + `internal/httpapi/idempotency.go`:

1. `GET idem:{hash}:resp` — если ответ уже есть, проверяем хэш запроса и
   возвращаем сохранённый ответ;
2. иначе `SET idem:{hash}:lock <token> NX PX 30s`;
3. если lock не взяли — `409` (или replay, если ответ успел появиться);
4. выполнили handler;
5. для `2xx` атомарно (Lua) проверяем владение lock, сохраняем ответ на 24h и
   удаляем lock; иначе просто освобождаем lock.

Проверка token при завершении важна: если lock истёк и ключом уже владеет другой
запрос, старый не должен перезаписать его результат.

Ограничение: если процесс упал после side effect, но до сохранения ответа,
повтор может создать дубль. Настоящая защита — уникальный индекс/транзакция в БД
плюс идемпотентный обработчик; Redis здесь помогает, но не является системой
истины.

### Graceful degradation

Все операции с кэшем возвращают ошибку наверх только как «кэш недоступен», а
сервисный слой в этом случае идёт в БД. `REDIS_REQUIRED=true` переключает
поведение на fail-fast, если Redis критичен для старта.

`/readyz` возвращает `200` и `status=degraded`, если недоступен только Redis:
приложение живо и может обслуживать запросы. Если недоступна БД — `503`.

---

## Конфигурация

Полный список — в [`.env.example`](.env.example). Главное:

| Переменная | Default | Зачем |
|---|---|---|
| `HTTP_ADDR` | `:8080` | адрес HTTP |
| `REDIS_ADDR` | `localhost:6379` | Redis |
| `REDIS_REQUIRED` | `false` | падать ли на старте без Redis |
| `CACHE_PREFIX` | `shop` | namespace ключей |
| `CACHE_KEY_VERSION` | `v1` | версия формата/ключа |
| `CACHE_PRODUCT_FRESH_TTL` | `20s` | свежесть товара |
| `CACHE_PRODUCT_STALE_TTL` | `3m` | окно stale/жёсткий TTL |
| `CACHE_LIST_FRESH_TTL` | `15s` | свежесть списка |
| `CACHE_LIST_STALE_TTL` | `2m` | окно stale списка |
| `CACHE_NEGATIVE_TTL` | `20s` | TTL 404 |
| `CACHE_L1_TTL` | `2s` | локальный кэш; `0` — выключить |
| `CACHE_L1_MAX_ENTRIES` | `10000` | размер L1 |
| `CACHE_LOCK_TTL` | `3s` | TTL distributed lock |
| `CACHE_LOCK_WAIT` | `300ms` | сколько ждать чужой lock |
| `CACHE_OP_TIMEOUT` | `300ms` | таймаут одной Redis-операции |
| `CACHE_JITTER` | `0.10` | разброс TTL |
| `RATE_LIMIT` / `RATE_WINDOW` | `60` / `1m` | лимит на API-ключ/IP |
| `IDEMPOTENCY_TTL` | `24h` | сколько хранить ответ |
| `IDEMPOTENCY_LOCK_TTL` | `30s` | TTL in-progress lock |
| `STORE_LATENCY` | `80ms` | искусственная задержка «БД» |
| `ENABLE_DEBUG` | `true` | debug-ручки; в проде выключить |

---

## Структура проекта

```text
cmd/api/main.go                 запуск, сигналы, graceful shutdown
internal/app/app.go             wiring зависимостей, health loop

internal/cache/
  cache.go                      cache-aside, SWR, singleflight, lock, degradation
  entry.go                      конверт значения, distributed lock, Lua release
  keys.go                      命名 ключей и версий
  l1.go                        in-process TTL-кэш

internal/catalog/service.go     бизнес-логика, инвалидация, list version
internal/store/                 интерфейс БД + in-memory заглушка с latency

internal/httpapi/
  router.go                     маршруты, handlers, readiness, debug
  middleware.go                 request-id, log, metrics, rate limit, recover
  idempotency.go                Idempotency-Key middleware
  response.go                   JSON/ошибки, recorder'ы

internal/ratelimit/limiter.go   sliding window на Lua
internal/idempotency/store.go   Redis-хранилище идемпотентных ответов
internal/events/events.go       Pub/Sub publisher/subscriber
internal/obs/metrics.go         Prometheus + hook для go-redis
internal/config/config.go       env-конфиг
```

---

## Тесты

```bash
go test ./...
go test -race ./...
go vet ./...
```

Что проверяется:

- 50 параллельных промахов → **один** вызов loader'а (`singleflight`);
- L1 возвращает значение без Redis;
- stale-значение отдаётся сразу, refresh происходит в фоне;
- negative-запись истекает и снова идёт в БД;
- битый JSON в Redis не отдаётся, ключ перезаписывается;
- lock снимается только владельцем;
- sliding window: 3 разрешено, 4-й — 429, после окна снова можно;
- idempotency: replay, `409` при параллельном запросе, `422` при другом body,
  старый владелец lock не может перезаписать ответ;
- HTTP-интеграция: `miss → l1 → redis`, 404 → `negative`, update инвалидирует
  товар и список, Redis down → `bypass`, `/readyz` → `degraded`;
- Pub/Sub доставляет событие подписчику.

---

## Что упрощено и что делать в проде

Это учебный проект, а не готовый микросервис. Вот честный список:

- **БД** — in-memory map с задержкой. В проде: Postgres/MySQL, транзакции,
 unique constraints, outbox для событий.
- **Инвалидация через Pub/Sub** — best-effort: сообщение можно потерять при
  реконнекте. Для надёжной доставки: Redis Streams + consumer groups,
  transactional outbox или CDC (Debezium).
- **Double delete** — прагматичная заплатка, а не строгая консистентность.
  Для критичных данных — версионирование значений, блокировки на запись или
  отказ от кэша.
- **Идемпотентность в Redis** — не system of record; падение между side effect и
  сохранением ответа всё ещё может дать дубль. В БД нужен уникальный ключ.
- **Rate limit fail-open** — осознанный выбор для доступности. Для антифрода
  нужен fail-closed/локальный token bucket.
- **L1** — простая TTL-map со случайным вытеснением. В проде: ristretto/lru,
  метрики, аккуратная инвалидация.
- **Нет auth/tenant isolation** — `X-API-Key` используется только как identity
  для лимитера.
- **Нет tracing** — стоит добавить OpenTelemetry, особенно для Redis-спанов.
- **Нет Sentinel/Cluster** — клиент настроен на один узел. В проде:
  `redis.NewFailoverClient` для Sentinel или `redis.NewClusterClient`.
  Мультиключевые Lua-скрипты должны использовать hash tags (в idempotency это
  уже сделано через `{hash}`).
- **Redis persistence/eviction** — в compose `allkeys-lru` без AOF. Для кэша это
  нормально; для очередей/данных — другая конфигурация.
- **Debug-ручки** — включены по умолчанию для демо. В проде выключить
  (`ENABLE_DEBUG=false`) или закрыть auth.
- **Нет нагрузочного теста** — перед продом: `hey`/`k6`, p99, hit ratio,
  поведение при падении Redis, проверка размера ключей.

---

## Что почитать дальше

- [Redis docs: client-side caching](https://redis.io/docs/latest/develop/clients/client-side-caching/)
- [Redis docs: pipelining](https://redis.io/docs/latest/develop/using-commands/pipelining/)
- [Redis docs: transactions and Lua](https://redis.io/docs/latest/develop/programmability/)
- [Redis docs: keyspace notifications](https://redis.io/docs/latest/develop/notifications/)
- [go-redis guide](https://redis.uptrace.dev/guide/)
- [AWS: caching challenges and strategies](https://aws.amazon.com/builders-library/caching-challenges-and-strategies/)
- [Martin Kleppmann: how to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)

Шпаргалка по вопросам с собеседований и разбор этого проекта «за 2 минуты» —
в [`docs/INTERVIEW.md`](docs/INTERVIEW.md).
