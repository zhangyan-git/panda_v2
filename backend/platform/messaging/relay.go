package messaging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RelayConfig controls polling and lease behavior for an outbox relay.
type RelayConfig struct {
	Owner        string
	BatchSize    int
	Lease        time.Duration
	PollInterval time.Duration
	RetryDelay   time.Duration
	OnError      func(error)
}

func (c RelayConfig) withDefaults() RelayConfig {
	if c.Owner == "" {
		c.Owner = uuid.NewString()
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.Lease <= 0 {
		c.Lease = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = time.Second
	}
	return c
}

// Relay publishes claimed outbox events and records the result. It is safe to
// run multiple relays against the same durable outbox; the store owns claim
// exclusion and lease ownership.
type Relay struct {
	outbox    DurableOutbox
	publisher Publisher
	config    RelayConfig
}

func NewRelay(outbox DurableOutbox, publisher Publisher, config RelayConfig) (*Relay, error) {
	if outbox == nil {
		return nil, errors.New("messaging: outbox relay requires an outbox")
	}
	if publisher == nil {
		return nil, errors.New("messaging: outbox relay requires a publisher")
	}
	return &Relay{outbox: outbox, publisher: publisher, config: config.withDefaults()}, nil
}

// Run polls until ctx is canceled. Store or publish failures are recorded and
// retried; infrastructure failures do not silently terminate the relay.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if r.config.OnError != nil {
				r.config.OnError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce claims and processes one batch. It returns claim or bookkeeping
// errors, while still attempting every event in the claimed batch.
func (r *Relay) RunOnce(ctx context.Context) error {
	events, err := r.outbox.ClaimPending(ctx, r.config.BatchSize, r.config.Owner, r.config.Lease)
	if err != nil {
		return fmt.Errorf("messaging: claim outbox events: %w", err)
	}
	var errs []error
	for _, event := range events {
		if err := r.publisher.Publish(ctx, event.Envelope); err != nil {
			next := time.Now().Add(r.config.RetryDelay)
			if markErr := r.outbox.MarkFailure(ctx, event.EventID, event.LeaseToken, err, next); markErr != nil {
				recoveryErr := r.releaseLease(ctx, event)
				errs = append(errs, fmt.Errorf("event %q publish: %w; mark failure: %v; lease recovery: %v", event.EventID, err, markErr, recoveryErr))
			} else {
				errs = append(errs, fmt.Errorf("event %q publish: %w", event.EventID, err))
			}
			continue
		}
		if err := r.outbox.MarkSuccess(ctx, event.EventID, event.LeaseToken); err != nil {
			recoveryErr := r.releaseLease(ctx, event)
			errs = append(errs, fmt.Errorf("event %q mark success: %w; lease recovery: %v", event.EventID, err, recoveryErr))
		}
	}
	return errors.Join(errs...)
}

func (r *Relay) releaseLease(ctx context.Context, event LeasedEnvelope) error {
	releaser, ok := r.outbox.(interface {
		Release(context.Context, string, string) error
	})
	if !ok {
		return errors.New("outbox does not support lease release")
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return releaser.Release(recoveryCtx, event.EventID, event.LeaseToken)
}
