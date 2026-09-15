package messaging

import (
	"context"
	"errors"
	"slices"
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

// 两个方向都要守住：显式配 0 是「别重试」，不能被默认值盖掉；配坏了才是
// 「没配」。反过来（配坏了退 0）会让失败消息静默消失，正是要避免的那个结果。
func TestRabbitConfigFromEnvNormalizesRetryLimit(t *testing.T) {
	t.Setenv("RABBITMQ_RETRY_LIMIT", "-2")
	if got := RabbitConfigFromEnv().RetryLimit; got != 0 {
		t.Fatalf("negative retry limit = %d, want 0", got)
	}
	t.Setenv("RABBITMQ_RETRY_LIMIT", "0")
	if got := RabbitConfigFromEnv().RetryLimit; got != 0 {
		t.Fatalf("explicit zero retry limit = %d, want 0", got)
	}
	t.Setenv("RABBITMQ_RETRY_LIMIT", "not-a-number")
	if got := RabbitConfigFromEnv().RetryLimit; got != defaultRetryLimit {
		t.Fatalf("invalid retry limit = %d, want the non-zero default %d", got, defaultRetryLimit)
	}
	t.Setenv("RABBITMQ_RETRY_LIMIT", "")
	if got := RabbitConfigFromEnv().RetryLimit; got != defaultRetryLimit {
		t.Fatalf("unset retry limit = %d, want the non-zero default %d", got, defaultRetryLimit)
	}
}

// 没配死信拓扑时默认值必须自己自洽——否则 NewRabbitMQ 会在 validate 上直接
// 拒绝启动，把一个「更好的默认」变成一次起不来。
func TestRabbitConfigDefaultsCarryDeadLetterTopology(t *testing.T) {
	t.Setenv("RABBITMQ_URL", "amqp://example")
	t.Setenv("RABBITMQ_DLX", "")
	t.Setenv("RABBITMQ_DLQ", "")
	cfg := RabbitConfigFromEnv()
	if cfg.DLX == "" || cfg.DLQ == "" {
		t.Fatalf("dead letter defaults = %q/%q, want both set", cfg.DLX, cfg.DLQ)
	}
	if cfg.RetryLimit <= 0 {
		t.Fatalf("retry limit = %d, want a non-zero default", cfg.RetryLimit)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestRabbitConfigValidateNoopWithoutURL(t *testing.T) {
	if err := (RabbitConfig{Exchange: "", RetryLimit: -1}).validate(); err != nil {
		t.Fatalf("no-op config validation error = %v", err)
	}
}

// outbox 的 relay 只能挂在真的会投递的发布器上：Noop 对每次投递都回成功，
// 挂在它上面的 relay 会把事件标成已投递然后丢掉，outbox 反而被清空。
func TestDeliversRejectsNoopPublisher(t *testing.T) {
	if Delivers(Noop{}) {
		t.Error("Delivers(Noop{}) = true, want false")
	}
	if Delivers(nil) {
		t.Error("Delivers(nil) = true, want false")
	}
	if !Delivers(&recordingPublisher{}) {
		t.Error("Delivers(custom publisher) = false, want true")
	}
}

type recordingPublisher struct{ published []Envelope }

func (p *recordingPublisher) Publish(_ context.Context, event Envelope) error {
	p.published = append(p.published, event)
	return nil
}

// 发布键与订阅键是一对，必须一起断言：只改一半，消息要么发不出去，要么发出去
// 没人订阅（后者不回退，因为发布是 mandatory + confirm 的，会以退回消息的形式失败）。
func TestRabbitRoutingKeyFollowsEventType(t *testing.T) {
	cfg := RabbitConfig{RoutingKey: "configured"}
	if got := cfg.routeKey("admin.operation.logged"); got != "admin.operation.logged" {
		t.Fatalf("routeKey = %q, want the event type", got)
	}
	if got := cfg.routeKey(""); got != "configured" {
		t.Fatalf("routeKey fallback = %q, want the configured key", got)
	}
	if got := (RabbitConfig{}).routeKey(""); got != "" {
		t.Fatalf("routeKey with nothing configured = %q, want empty", got)
	}
}

// 未配置订阅键时队列订阅全部：topic 交换机只投递有绑定匹配的消息，空绑定会把
// 每一条事件都变成退回消息。
func TestRabbitBindKeysDefaultsToEverything(t *testing.T) {
	if got := (RabbitConfig{}).bindKeys(); !slices.Equal(got, []string{"#"}) {
		t.Fatalf("bindKeys without config = %q, want [%q]", got, "#")
	}
	if got := (RabbitConfig{RoutingKey: "configured"}).bindKeys(); !slices.Equal(got, []string{"configured"}) {
		t.Fatalf("bindKeys = %q, want the configured key", got)
	}
	// 只有分隔符（`,`、`,,`、空白）与没配置等价：切完一个键都不剩，就该退到全订阅，
	// 而不是留一个空绑定——空绑定会把每一条事件都变成退回消息。
	if got := (RabbitConfig{RoutingKey: " , , "}).bindKeys(); !slices.Equal(got, []string{"#"}) {
		t.Fatalf("bindKeys with only separators = %q, want [%q]", got, "#")
	}
}

// 死信的去向键取的是**配置原文**，空就是空——尤其不能跟着 bindKeys 退到 `#`。
//
// 这条错起来没有任何声音：DLX 是 topic 交换机，`#` 匹配每一条，而「配了队列名、没配
// 路由键」的服务若往 DLX 上绑一把 `#`，它就把别家的死信捞进自己的 DLQ——别人的失败
// 消息从此查不到（DLQ 名字是可配的，defaultDLQ 只是恰好大家都用同一个）。空绑定只认
// 路由键为空的那一条。
//
// 空值是有效的、不被当「没设」（实测 2026-09-15，本地 broker：空的
// x-dead-letter-routing-key 让死信带着空 key 投出来，落到空绑定上，而不是退回原始
// 路由键）。所以「配了 DLX 却没配键」不会让死信变成孤儿。
func TestRabbitDeadLetterKeyStaysEmptyWhenUnconfigured(t *testing.T) {
	if got := (RabbitConfig{}).deadLetterKey(); got != "" {
		t.Fatalf("deadLetterKey without config = %q, want empty", got)
	}
	// 只有分隔符与没配置**不等价**：主队列照样全收（切成零个键就退到 `#`），而死信
	// 键仍是那一串原文。两边各自自洽，不等才是对的。
	if got := (RabbitConfig{RoutingKey: " , , "}).deadLetterKey(); got != " , , " {
		t.Fatalf("deadLetterKey with only separators = %q, want the configured text", got)
	}
	// 通配与逗号都原样带走：这一串在 topic 交换机上是一整个 routing key，拆开与否不
	// 重要，重要的是它与 DLQ 上那条绑定逐字相同。
	for _, key := range []string{"payment.*", "order.completed,order.after_sale.applied"} {
		if got := (RabbitConfig{RoutingKey: key}).deadLetterKey(); got != key {
			t.Fatalf("deadLetterKey = %q, want %q", got, key)
		}
	}
}

// 一个队列可以同时订阅几件具体的事（account-service 收订单完成与售后的三种结果）。
// 切分要吃掉每段两侧的空白、丢掉空段，顺序保持配置里的顺序——绑定的顺序不影响投递，
// 但让断言与配置逐字对得上，读的人不必在脑子里排序。
func TestRabbitBindKeysSplitsOnCommas(t *testing.T) {
	cfg := RabbitConfig{RoutingKey: "order.completed, order.after_sale.applied ,,order.after_sale.cancelled"}
	want := []string{"order.completed", "order.after_sale.applied", "order.after_sale.cancelled"}
	if got := cfg.bindKeys(); !slices.Equal(got, want) {
		t.Fatalf("bindKeys = %q, want %q", got, want)
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
