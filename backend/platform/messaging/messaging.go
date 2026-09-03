package messaging

import "context"

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

type Noop struct{}

func (Noop) Publish(context.Context, Envelope) error { return nil }
func (Noop) Consume(ctx context.Context, _ Handler) error {
	<-ctx.Done()
	return ctx.Err()
}
