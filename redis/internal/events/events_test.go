package events_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"redis-catalog/internal/events"
)

func TestPublisherSubscriber(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	received := make(chan events.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sub := events.NewSubscriber(rdb, "test:events", func(ev events.Event) {
		received <- ev
	}, log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub.Run(ctx)
	}()

	pub := events.NewPublisher(rdb, "test:events", log)
	want := events.Event{Type: events.ProductChanged, ProductID: "p-1", At: time.Now().UTC()}

	// Подписка устанавливается асинхронно, поэтому публикуем с ретраями.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := pub.Publish(ctx, want); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-received:
			if got.Type != want.Type || got.ProductID != want.ProductID {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("subscriber did not stop")
			}
			return
		case <-time.After(50 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("event was not delivered")
			}
		}
	}
}
