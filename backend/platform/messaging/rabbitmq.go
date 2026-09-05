package messaging

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"sync"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitConfig describes the exchange and queue used by RabbitMQ.
type RabbitConfig struct {
	URL          string
	Exchange     string
	ExchangeType string
	Queue        string
	RoutingKey   string
	DLX          string
	DLQ          string
	RetryLimit   int
}

func RabbitConfigFromEnv() RabbitConfig {
	limit, _ := strconv.Atoi(os.Getenv("RABBITMQ_RETRY_LIMIT"))
	if limit < 0 {
		limit = 0
	}
	return RabbitConfig{
		URL: os.Getenv("RABBITMQ_URL"), Exchange: envOr("RABBITMQ_EXCHANGE", "panda.events"),
		ExchangeType: envOr("RABBITMQ_EXCHANGE_TYPE", "topic"), Queue: os.Getenv("RABBITMQ_QUEUE"),
		RoutingKey: os.Getenv("RABBITMQ_ROUTING_KEY"), DLX: os.Getenv("RABBITMQ_DLX"),
		DLQ: os.Getenv("RABBITMQ_DLQ"), RetryLimit: limit,
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (c RabbitConfig) validate() error {
	if c.URL == "" { // Noop is deliberately valid without any Rabbit settings.
		return nil
	}
	if c.Exchange == "" {
		return fmt.Errorf("messaging: RabbitMQ exchange is required")
	}
	if c.ExchangeType == "" {
		return fmt.Errorf("messaging: RabbitMQ exchange type is required")
	}
	if c.RetryLimit < 0 {
		return fmt.Errorf("messaging: RabbitMQ retry limit cannot be negative")
	}
	if c.DLQ != "" && c.DLX == "" {
		return fmt.Errorf("messaging: RabbitMQ DLX is required when DLQ is configured")
	}
	if c.RetryLimit > 0 && (c.DLX == "" || c.DLQ == "") {
		return fmt.Errorf("messaging: RabbitMQ DLX and DLQ are required when retries are enabled")
	}
	return nil
}

// NewRabbitMQ returns Noop when RABBITMQ_URL is not configured.
func NewRabbitMQ(cfg RabbitConfig) (Publisher, Consumer, io.Closer, error) {
	if cfg.URL == "" {
		return Noop{}, Noop{}, nopCloser{}, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, nil, err
	}
	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, nil, nil, err
	}
	pubCh, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	if err := pubCh.Confirm(false); err != nil {
		_ = pubCh.Close()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("messaging: enable publisher confirms: %w", err)
	}
	if err := pubCh.ExchangeDeclare(cfg.Exchange, cfg.ExchangeType, true, false, false, false, nil); err != nil {
		_ = pubCh.Close()
		_ = conn.Close()
		return nil, nil, nil, err
	}
	consumerCh := pubCh
	if cfg.Queue != "" {
		consumerCh, err = conn.Channel()
		if err != nil {
			_ = pubCh.Close()
			_ = conn.Close()
			return nil, nil, nil, err
		}
		if err := declareTopology(consumerCh, cfg); err != nil {
			_ = consumerCh.Close()
			_ = pubCh.Close()
			_ = conn.Close()
			return nil, nil, nil, err
		}
	}
	client := &rabbitClient{pubCh: pubCh, consumeCh: consumerCh, conn: conn, cfg: cfg, returns: make(chan amqp.Return, 16), pendingReturns: make(map[string][]chan amqp.Return)}
	pubCh.NotifyReturn(client.returns)
	go client.dispatchReturns()
	return client, client, client, nil
}

func declareTopology(ch *amqp.Channel, cfg RabbitConfig) error {
	if cfg.DLX != "" {
		if err := ch.ExchangeDeclare(cfg.DLX, "topic", true, false, false, false, nil); err != nil {
			return err
		}
	}
	args := amqp.Table{}
	if cfg.DLX != "" {
		args["x-dead-letter-exchange"] = cfg.DLX
		args["x-dead-letter-routing-key"] = cfg.RoutingKey
	}
	if _, err := ch.QueueDeclare(cfg.Queue, true, false, false, false, args); err != nil {
		return err
	}
	if err := ch.QueueBind(cfg.Queue, cfg.RoutingKey, cfg.Exchange, false, nil); err != nil {
		return err
	}
	if cfg.DLQ != "" {
		if _, err := ch.QueueDeclare(cfg.DLQ, true, false, false, false, nil); err != nil {
			return err
		}
		key := cfg.RoutingKey
		if key == "" {
			key = "#"
		}
		if err := ch.QueueBind(cfg.DLQ, key, cfg.DLX, false, nil); err != nil {
			return err
		}
	}
	return nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type rabbitClient struct {
	pubCh, consumeCh *amqp.Channel
	conn             *amqp.Connection
	cfg              RabbitConfig
	closeOnce        sync.Once
	closeErr         error
	returns          chan amqp.Return
	returnMu         sync.Mutex
	pendingReturns   map[string][]chan amqp.Return
	closed           chan struct{}
}

func (r *rabbitClient) Close() error {
	return r.close(func() error {
		if r.consumeCh != r.pubCh {
			_ = r.consumeCh.Close()
		}
		_ = r.pubCh.Close()
		return r.conn.Close()
	})
}

// CloseContext force-closes the connection when the context has a deadline.
// Closing the connection directly also closes its channels and consumers.
func (r *rabbitClient) CloseContext(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		return r.close(func() error { return r.conn.CloseDeadline(deadline) })
	}
	return r.Close()
}

func (r *rabbitClient) close(closeConn func() error) error {
	r.closeOnce.Do(func() {
		r.closeErr = closeConn()
	})
	return r.closeErr
}

func (r *rabbitClient) Publish(ctx context.Context, e Envelope) error {
	if e.EventID == "" {
		e.EventID = uuid.NewString()
	}
	confirmation, err := r.pubCh.PublishWithDeferredConfirm(
		r.cfg.Exchange, r.cfg.RoutingKey, true, false, amqp.Publishing{
			ContentType: "application/json", MessageId: e.EventID, Type: e.EventType,
			Headers: amqp.Table{"event_version": e.EventVersion, "trace_id": e.TraceID}, Body: e.Payload,
		},
	)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return fmt.Errorf("messaging: publisher confirmation unavailable")
	}
	acked, err := r.waitConfirmation(ctx, confirmation, e.EventID)
	if err != nil {
		return err
	}
	if !acked {
		return fmt.Errorf("messaging: publisher confirmation rejected")
	}
	return nil
}

func (r *rabbitClient) Consume(ctx context.Context, handler Handler) error {
	if r.cfg.Queue == "" {
		return fmt.Errorf("messaging: RabbitMQ queue is required for consume")
	}
	tag := "panda-consumer-" + uuid.NewString()
	deliveries, err := r.consumeCh.Consume(r.cfg.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	defer func() { _ = r.consumeCh.Cancel(tag, false) }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("messaging: delivery channel closed")
			}
			envelope := envelopeFromDelivery(d)
			if err := handler(ctx, envelope); err == nil {
				if ackErr := d.Ack(false); ackErr != nil {
					return ackErr
				}
			} else if retryCount(d.Headers) < r.cfg.RetryLimit {
				if err := r.republishRetry(ctx, d); err != nil {
					return err
				}
				if err := d.Ack(false); err != nil {
					return err
				}
			} else if err := d.Reject(false); err != nil {
				return err
			}
		}
	}
}

func envelopeFromDelivery(d amqp.Delivery) Envelope {
	e := Envelope{EventID: d.MessageId, EventType: d.Type, Payload: d.Body}
	if v, ok := d.Headers["event_version"].(string); ok {
		e.EventVersion = v
	}
	if v, ok := d.Headers["trace_id"].(string); ok {
		e.TraceID = v
	}
	return e
}

func (r *rabbitClient) republishRetry(ctx context.Context, d amqp.Delivery) error {
	headers := amqp.Table{}
	maps.Copy(headers, d.Headers)
	headers["x-retry-count"] = retryCount(d.Headers) + 1
	confirmation, err := r.pubCh.PublishWithDeferredConfirm(
		r.cfg.Exchange, r.cfg.RoutingKey, true, false, amqp.Publishing{
			ContentType: d.ContentType, MessageId: d.MessageId, Type: d.Type, Headers: headers, Body: d.Body,
		},
	)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return fmt.Errorf("messaging: retry publisher confirmation unavailable")
	}
	acked, err := r.waitConfirmation(ctx, confirmation, d.MessageId)
	if err != nil {
		return err
	}
	if !acked {
		return fmt.Errorf("messaging: retry publisher confirmation rejected")
	}
	return nil
}

func (r *rabbitClient) dispatchReturns() {
	for returned := range r.returns {
		r.returnMu.Lock()
		waiters := r.pendingReturns[returned.MessageId]
		if len(waiters) > 0 {
			waiter := waiters[0]
			if len(waiters) == 1 {
				delete(r.pendingReturns, returned.MessageId)
			} else {
				r.pendingReturns[returned.MessageId] = waiters[1:]
			}
			r.returnMu.Unlock()
			waiter <- returned
			close(waiter)
			continue
		}
		r.returnMu.Unlock()
	}
}

func (r *rabbitClient) waitConfirmation(ctx context.Context, confirmation *amqp.DeferredConfirmation, messageID string) (bool, error) {
	type confirmationResult struct {
		acked bool
		err   error
	}
	result := make(chan confirmationResult, 1)
	returnedCh := make(chan amqp.Return, 1)
	r.returnMu.Lock()
	r.pendingReturns[messageID] = append(r.pendingReturns[messageID], returnedCh)
	r.returnMu.Unlock()
	defer func() {
		r.returnMu.Lock()
		waiters := r.pendingReturns[messageID]
		for i, waiter := range waiters {
			if waiter == returnedCh {
				r.pendingReturns[messageID] = append(waiters[:i], waiters[i+1:]...)
				if len(r.pendingReturns[messageID]) == 0 {
					delete(r.pendingReturns, messageID)
				}
				break
			}
		}
		r.returnMu.Unlock()
	}()
	go func() {
		acked, err := confirmation.WaitContext(ctx)
		result <- confirmationResult{acked: acked, err: err}
	}()
	for {
		select {
		case outcome := <-result:
			return outcome.acked, outcome.err
		case returned := <-returnedCh:
			return false, fmt.Errorf("messaging: message returned: %s", returned.ReplyText)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func retryCount(headers amqp.Table) int {
	if value, ok := retryHeaderValue(headers["x-retry-count"]); ok {
		return value
	}
	if deaths, ok := headers["x-death"].([]any); ok {
		count := 0
		for _, death := range deaths {
			if table, ok := death.(amqp.Table); ok {
				if value, ok := retryHeaderValue(table["count"]); ok {
					count += value
				}
			}
		}
		return count
	}
	return 0
}

func retryHeaderValue(value any) (int, bool) {
	const maxInt = int(^uint(0) >> 1)
	switch value := value.(type) {
	case int:
		if value < 0 {
			return 0, false
		}
		return value, true
	case int8:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int16:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int32:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int64:
		if value < 0 || value > int64(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint:
		if value > uint(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		if uint64(value) > uint64(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint64:
		if value > uint64(maxInt) {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}
