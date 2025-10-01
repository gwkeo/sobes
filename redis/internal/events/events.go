package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const ProductChanged = "product.changed"

type Event struct {
	Type      string    `json:"type"`
	ProductID string    `json:"product_id,omitempty"`
	At        time.Time `json:"at"`
}

type Publisher struct {
	rdb     redis.UniversalClient
	channel string
	log     *slog.Logger
}

func NewPublisher(rdb redis.UniversalClient, channel string, log *slog.Logger) *Publisher {
	return &Publisher{rdb: rdb, channel: channel, log: log}
}

func (p *Publisher) Publish(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return p.rdb.Publish(ctx, p.channel, payload).Err()
}

// Subscriber разносит invalidation по всем инстансам сервиса.
//
// Pub/Sub — fire-and-forget: если инстанс в момент публикации переподключался,
// сообщение он не получит. Поэтому L1 живёт с коротким TTL и является лишь
// ускорителем, а не источником истины. Для гарантированной доставки в проде —
// Redis Streams/outbox, но для инвалидации кэша обычно достаточно этого.
type Subscriber struct {
	rdb     redis.UniversalClient
	channel string
	handler func(Event)
	log     *slog.Logger
}

func NewSubscriber(rdb redis.UniversalClient, channel string, handler func(Event), log *slog.Logger) *Subscriber {
	return &Subscriber{rdb: rdb, channel: channel, handler: handler, log: log}
}

func (s *Subscriber) Run(ctx context.Context) {
	for ctx.Err() == nil {
		pubsub := s.rdb.Subscribe(ctx, s.channel)
		ch := pubsub.Channel(redis.WithChannelSize(128))
		s.log.Info("cache events: subscribed", "channel", s.channel)

		alive := true
		for alive {
			select {
			case <-ctx.Done():
				_ = pubsub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					alive = false
					break
				}
				var ev Event
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
					s.log.Warn("cache events: bad payload", "err", err)
					continue
				}
				s.handle(ev)
			}
		}
		_ = pubsub.Close()

		if ctx.Err() != nil {
			return
		}
		s.log.Warn("cache events: subscription lost, reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Subscriber) handle(ev Event) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("cache events: handler panic", "panic", r)
		}
	}()
	s.handler(ev)
}
