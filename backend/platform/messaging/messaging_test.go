package messaging

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestMemoryOutboxInbox(t *testing.T) {
	store := NewMemoryOutboxInbox()
	event := Envelope{EventID: "event-1", Payload: []byte("payload")}
	if err := store.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Pending(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v, err = %v", pending, err)
	}
	if err := store.MarkPublished(context.Background(), event.EventID); err != nil {
		t.Fatal(err)
	}
	pending, _ = store.Pending(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatalf("pending after publish = %#v", pending)
	}
	claimed, err := store.Claim(context.Background(), event.EventID)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	claimed, err = store.Claim(context.Background(), event.EventID)
	if err != nil || claimed {
		t.Fatalf("duplicate claim = %v, %v", claimed, err)
	}
}

func TestRabbitConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("RABBITMQ_URL", "")
	t.Setenv("RABBITMQ_EXCHANGE", "")
	cfg := RabbitConfigFromEnv()
	if cfg.Exchange != "panda.events" || cfg.ExchangeType != "topic" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	_, _, closer, err := NewRabbitMQ(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRabbitConfigValidateStrictSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  RabbitConfig
	}{
		{name: "missing exchange", cfg: RabbitConfig{URL: "amqp://localhost", ExchangeType: "topic"}},
		{name: "missing exchange type", cfg: RabbitConfig{URL: "amqp://localhost", Exchange: "events"}},
		{name: "DLQ without DLX", cfg: RabbitConfig{URL: "amqp://localhost", Exchange: "events", ExchangeType: "topic", DLQ: "dead"}},
		{name: "retry without dead letter topology", cfg: RabbitConfig{URL: "amqp://localhost", Exchange: "events", ExchangeType: "topic", RetryLimit: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRetryCountHandlesRabbitHeaderTypes(t *testing.T) {
	if got := retryCount(amqp.Table{"x-retry-count": int64(3)}); got != 3 {
		t.Fatalf("retry count = %d, want 3", got)
	}
	deaths := []interface{}{amqp.Table{"count": int32(2)}, amqp.Table{"count": uint64(4)}}
	if got := retryCount(amqp.Table{"x-death": deaths}); got != 6 {
		t.Fatalf("x-death retry count = %d, want 6", got)
	}
}
