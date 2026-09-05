package messaging

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type recordingConsumer struct {
	event   Envelope
	handler Handler
	err     error
}

func (c *recordingConsumer) Consume(_ context.Context, handler Handler) error {
	c.handler = handler
	if c.err != nil {
		return c.err
	}
	return handler(context.Background(), c.event)
}

type fakeInbox struct {
	claimed bool
	err     error
	eventID string
}

func (i *fakeInbox) Claim(_ context.Context, eventID string) (bool, error) {
	i.eventID = eventID
	return i.claimed, i.err
}

func TestWithInboxDuplicateSkipsHandler(t *testing.T) {
	consumer := &recordingConsumer{event: Envelope{EventID: "duplicate"}}
	inbox := &fakeInbox{}
	handled := false
	if err := WithInbox(consumer, inbox).Consume(context.Background(), func(context.Context, Envelope) error {
		handled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("duplicate event was handled")
	}
	if inbox.eventID != "duplicate" {
		t.Fatalf("claimed event ID = %q", inbox.eventID)
	}
}

func TestWithInboxClaimError(t *testing.T) {
	claimErr := errors.New("claim failed")
	consumer := &recordingConsumer{event: Envelope{EventID: "event-1"}}
	inbox := &fakeInbox{err: claimErr}
	if err := WithInbox(consumer, inbox).Consume(context.Background(), func(context.Context, Envelope) error {
		t.Fatal("handler called after claim error")
		return nil
	}); !errors.Is(err, claimErr) {
		t.Fatalf("error = %v, want %v", err, claimErr)
	}
}

type durableFakeInbox struct {
	claimed, completed, released bool
	owners                       []string
}

func (i *durableFakeInbox) Claim(_ context.Context, _ string) (bool, error) { return false, nil }
func (i *durableFakeInbox) ClaimDurable(_ context.Context, _, owner string, _ time.Duration) (bool, string, error) {
	i.claimed = true
	i.owners = append(i.owners, owner)
	return true, owner + "-token", nil
}
func (i *durableFakeInbox) Complete(_ context.Context, _, _ string) error {
	i.completed = true
	return nil
}
func (i *durableFakeInbox) Release(_ context.Context, _, _ string) error {
	i.released = true
	return nil
}

func TestWithInboxDurableLifecycle(t *testing.T) {
	inbox := &durableFakeInbox{}
	consumer := &recordingConsumer{event: Envelope{EventID: "durable"}}
	if err := WithInbox(consumer, inbox).Consume(context.Background(), func(context.Context, Envelope) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !inbox.claimed || !inbox.completed || inbox.released {
		t.Fatalf("lifecycle = claimed %v completed %v released %v", inbox.claimed, inbox.completed, inbox.released)
	}
}

func TestWithInboxDurableReleaseOnFailure(t *testing.T) {
	inbox := &durableFakeInbox{}
	consumer := &recordingConsumer{event: Envelope{EventID: "durable"}}
	want := errors.New("handler failed")
	if err := WithInbox(consumer, inbox).Consume(context.Background(), func(context.Context, Envelope) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if !inbox.claimed || inbox.completed || !inbox.released {
		t.Fatalf("lifecycle = claimed %v completed %v released %v", inbox.claimed, inbox.completed, inbox.released)
	}
}

func TestWithInboxUsesDistinctOwners(t *testing.T) {
	inbox := &durableFakeInbox{}
	consumer := &recordingConsumer{event: Envelope{EventID: "durable"}}
	handler := func(context.Context, Envelope) error { return nil }
	if err := WithInbox(consumer, inbox).Consume(context.Background(), handler); err != nil {
		t.Fatal(err)
	}
	if err := WithInbox(consumer, inbox).Consume(context.Background(), handler); err != nil {
		t.Fatal(err)
	}
	if len(inbox.owners) != 2 || inbox.owners[0] == inbox.owners[1] {
		t.Fatalf("owners = %#v, want distinct owners", inbox.owners)
	}
}

func TestMessagingMigrationAddsLeaseColumnsBeforeIndexes(t *testing.T) {
	data, err := os.ReadFile("001_create_message_tables.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(data)
	alter := strings.Index(migration, "ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_until")
	index := strings.Index(migration, "CREATE INDEX IF NOT EXISTS message_outbox_lease_idx")
	if alter < 0 || index < 0 || alter > index {
		t.Fatal("lease column migration must precede lease index creation")
	}
}

func TestMemoryDurableInboxReleaseAllowsRetry(t *testing.T) {
	store := NewMemoryOutboxInbox()
	ctx := context.Background()
	claimed, token, err := store.ClaimDurable(ctx, "event", "a", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.Release(ctx, "event", token); err != nil {
		t.Fatal(err)
	}
	claimed, _, err = store.ClaimDurable(ctx, "event", "b", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("retry claim = %v, %v", claimed, err)
	}
}

func TestWithInboxHandlerPath(t *testing.T) {
	consumer := &recordingConsumer{event: Envelope{EventID: "event-1", Payload: []byte("body")}}
	inbox := &fakeInbox{claimed: true}
	handlerErr := errors.New("handler failed")
	var got Envelope
	if err := WithInbox(consumer, inbox).Consume(context.Background(), func(_ context.Context, event Envelope) error {
		got = event
		return handlerErr
	}); !errors.Is(err, handlerErr) {
		t.Fatalf("error = %v, want %v", err, handlerErr)
	}
	if got.EventID != consumer.event.EventID || string(got.Payload) != "body" {
		t.Fatalf("handler event = %#v", got)
	}
}

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

func TestOutboxErrorSummaryRedactsAndBoundsDetails(t *testing.T) {
	failure := errors.New("connect postgres://alice:secret@example.test/db password=hunter2 token=abc " + strings.Repeat("x", 2000))
	got := sanitizeOutboxError(failure)
	if len(got) > maxOutboxErrorSummary {
		t.Fatalf("summary length = %d, want <= %d", len(got), maxOutboxErrorSummary)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "hunter2") || strings.Contains(got, "token=abc") {
		t.Fatalf("summary contains credentials: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("summary = %q, want redaction marker", got)
	}
}

func TestRabbitConfigFromEnvReadsSettings(t *testing.T) {
	env := map[string]string{
		"RABBITMQ_URL": "amqp://example", "RABBITMQ_EXCHANGE": "events",
		"RABBITMQ_EXCHANGE_TYPE": "fanout", "RABBITMQ_QUEUE": "queue",
		"RABBITMQ_ROUTING_KEY": "route", "RABBITMQ_DLX": "dead.exchange",
		"RABBITMQ_DLQ": "dead.queue", "RABBITMQ_RETRY_LIMIT": "3",
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	cfg := RabbitConfigFromEnv()
	want := RabbitConfig{URL: "amqp://example", Exchange: "events", ExchangeType: "fanout", Queue: "queue", RoutingKey: "route", DLX: "dead.exchange", DLQ: "dead.queue", RetryLimit: 3}
	if cfg != want {
		t.Fatalf("config = %#v, want %#v", cfg, want)
	}
}

func TestRabbitConfigFromEnvNormalizesRetryLimit(t *testing.T) {
	t.Setenv("RABBITMQ_RETRY_LIMIT", "-2")
	if got := RabbitConfigFromEnv().RetryLimit; got != 0 {
		t.Fatalf("negative retry limit = %d, want 0", got)
	}
	t.Setenv("RABBITMQ_RETRY_LIMIT", "not-a-number")
	if got := RabbitConfigFromEnv().RetryLimit; got != 0 {
		t.Fatalf("invalid retry limit = %d, want 0", got)
	}
}

func TestRabbitConfigValidateNoopWithoutURL(t *testing.T) {
	if err := (RabbitConfig{Exchange: "", RetryLimit: -1}).validate(); err != nil {
		t.Fatalf("no-op config validation error = %v", err)
	}
}

func TestEnvelopeFromDeliveryAndRetryHeaders(t *testing.T) {
	delivery := amqp.Delivery{MessageId: "id", Type: "created", Body: []byte("payload"), Headers: amqp.Table{
		"event_version": "v2", "trace_id": "trace",
	}}
	got := envelopeFromDelivery(delivery)
	if got.EventID != "id" || got.EventType != "created" || got.EventVersion != "v2" || got.TraceID != "trace" || string(got.Payload) != "payload" {
		t.Fatalf("envelope = %#v", got)
	}
	if got := retryCount(amqp.Table{"x-retry-count": "bad"}); got != 0 {
		t.Fatalf("invalid retry header count = %d, want 0", got)
	}
	if got := retryCount(amqp.Table{"x-death": []any{"bad", amqp.Table{"count": int8(2)}}}); got != 2 {
		t.Fatalf("mixed x-death count = %d, want 2", got)
	}
}

func TestRabbitClientCloseHelperRunsOnce(t *testing.T) {
	client := &rabbitClient{}
	var calls int
	var mu sync.Mutex
	closeConn := func() error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("close failed")
	}
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.close(closeConn); err == nil || err.Error() != "close failed" {
				t.Errorf("close error = %v", err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("close callback calls = %d, want 1", calls)
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
