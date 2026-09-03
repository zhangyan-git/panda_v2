package messaging

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

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
	client := &rabbitClient{pubCh: pubCh, consumeCh: consumerCh, conn: conn, cfg: cfg}
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
	return r.pubCh.PublishWithContext(ctx, r.cfg.Exchange, r.cfg.RoutingKey, false, false, amqp.Publishing{
		ContentType: "application/json", MessageId: e.EventID, Type: e.EventType,
		Headers: amqp.Table{"event_version": e.EventVersion, "trace_id": e.TraceID}, Body: e.Payload,
	})
}

func (r *rabbitClient) Consume(ctx context.Context, handler Handler) error {
	if r.cfg.Queue == "" {
		return fmt.Errorf("messaging: RabbitMQ queue is required for consume")
	}
	deliveries, err := r.consumeCh.Consume(r.cfg.Queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
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
				if err := republishRetry(ctx, r.pubCh, r.cfg, d); err != nil {
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

func republishRetry(ctx context.Context, ch *amqp.Channel, cfg RabbitConfig, d amqp.Delivery) error {
	headers := amqp.Table{}
	for key, value := range d.Headers {
		headers[key] = value
	}
	headers["x-retry-count"] = retryCount(d.Headers) + 1
	return ch.PublishWithContext(ctx, cfg.Exchange, cfg.RoutingKey, false, false, amqp.Publishing{
		ContentType: d.ContentType, MessageId: d.MessageId, Type: d.Type, Headers: headers, Body: d.Body,
	})
}

func retryCount(headers amqp.Table) int {
	if value, ok := retryHeaderValue(headers["x-retry-count"]); ok {
		return value
	}
	if deaths, ok := headers["x-death"].([]interface{}); ok {
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

func retryHeaderValue(value interface{}) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case uint:
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		return int(value), true
	case uint64:
		return int(value), true
	default:
		return 0, false
	}
}
