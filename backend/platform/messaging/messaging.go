package messaging

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Envelope struct {
	EventID, EventType, EventVersion, TraceID string
	Payload                                   []byte
}

type Publisher interface {
	Publish(context.Context, Envelope) error
}

type Handler func(context.Context, Envelope) error

type Consumer interface {
	Consume(context.Context, Handler) error
}

// WithInbox wraps a consumer with durable, atomic duplicate suppression.
// Duplicate deliveries are acknowledged by the underlying consumer because
// the wrapper returns nil without invoking the application handler.
func WithInbox(consumer Consumer, inbox Inbox) Consumer {
	return inboxConsumer{consumer: consumer, inbox: inbox, owner: uuid.NewString()}
}

type inboxConsumer struct {
	consumer Consumer
	inbox    Inbox
	owner    string
}

func (c inboxConsumer) Consume(ctx context.Context, handler Handler) error {
	if durable, ok := c.inbox.(DurableInbox); ok {
		owner := c.owner
		return c.consumer.Consume(ctx, func(handlerCtx context.Context, event Envelope) error {
			if event.EventID == "" {
				return errors.New("messaging: event ID is required")
			}
			claimed, token, err := durable.ClaimDurable(handlerCtx, event.EventID, owner, time.Minute)
			if err != nil || !claimed {
				return err
			}
			if err := handler(handlerCtx, event); err != nil {
				// Cleanup uses a detached, bounded context so a canceled delivery
				// cannot leave a durable lease held until it expires.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				releaseErr := durable.Release(cleanupCtx, event.EventID, token)
				cancel()
				return errors.Join(err, releaseErr)
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			completeErr := durable.Complete(cleanupCtx, event.EventID, token)
			cancel()
			return completeErr
		})
	}
	return c.consumer.Consume(ctx, func(handlerCtx context.Context, event Envelope) error {
		if event.EventID == "" {
			return errors.New("messaging: event ID is required")
		}
		claimed, err := c.inbox.Claim(handlerCtx, event.EventID)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		return handler(handlerCtx, event)
	})
}

type Noop struct{}

func (Noop) Publish(context.Context, Envelope) error { return nil }
func (Noop) Consume(ctx context.Context, _ Handler) error {
	<-ctx.Done()
	return ctx.Err()
}
