package messaging

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Outbox stores events transactionally with application state. Implementations
// may bind Append and application writes to the same database transaction.
type Outbox interface {
	Append(context.Context, Envelope) error
	Pending(context.Context, int) ([]Envelope, error)
	MarkPublished(context.Context, string) error
}

// LeasedEnvelope is an outbox event claimed by a particular worker.
type LeasedEnvelope struct {
	Envelope
	LeaseOwner string
	LeaseToken string
	LeaseUntil time.Time
}

// DurableOutbox adds lease-based processing while preserving Outbox callers.
type DurableOutbox interface {
	ClaimPending(context.Context, int, string, time.Duration) ([]LeasedEnvelope, error)
	MarkSuccess(context.Context, string, string) error
	MarkFailure(context.Context, string, string, error, time.Time) error
}

// Inbox prevents duplicate processing of an event. Implementations should
// atomically claim an event ID before invoking a handler.
type Inbox interface {
	Claim(context.Context, string) (bool, error)
}

// DurableInbox provides ownership-aware inbox processing.
type DurableInbox interface {
	ClaimDurable(context.Context, string, string, time.Duration) (bool, string, error)
	Complete(context.Context, string, string) error
	Release(context.Context, string, string) error
}

// MemoryOutboxInbox is a small process-local implementation for tests and
// development. It is not durable and must not be used for production state.
type MemoryOutboxInbox struct {
	mu      sync.Mutex
	pending []Envelope
	seen    map[string]struct{}
	leases  map[string]memoryLease
	next    map[string]time.Time
}

type memoryLease struct {
	owner string
	token string
	until time.Time
}

func NewMemoryOutboxInbox() *MemoryOutboxInbox {
	return &MemoryOutboxInbox{seen: make(map[string]struct{}), leases: make(map[string]memoryLease), next: make(map[string]time.Time)}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (m *MemoryOutboxInbox) Append(ctx context.Context, event Envelope) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if event.EventID == "" {
		return errors.New("messaging: event ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.pending {
		if existing.EventID != event.EventID {
			continue
		}
		if existing.EventType == event.EventType && existing.EventVersion == event.EventVersion && existing.TraceID == event.TraceID && string(existing.Payload) == string(event.Payload) {
			return nil
		}
		return fmt.Errorf("messaging: outbox event %q conflicts with existing event", event.EventID)
	}
	copyEvent := event
	copyEvent.Payload = append([]byte(nil), event.Payload...)
	m.pending = append(m.pending, copyEvent)
	return nil
}

func (m *MemoryOutboxInbox) Pending(ctx context.Context, limit int) ([]Envelope, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.pending) {
		limit = len(m.pending)
	}
	result := make([]Envelope, 0, limit)
	for _, event := range m.pending[:limit] {
		copyEvent := event
		copyEvent.Payload = append([]byte(nil), event.Payload...)
		result = append(result, copyEvent)
	}
	return result, nil
}

func (m *MemoryOutboxInbox) MarkPublished(ctx context.Context, eventID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, event := range m.pending {
		if event.EventID == eventID {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MemoryOutboxInbox) Claim(ctx context.Context, eventID string) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if eventID == "" {
		return false, errors.New("messaging: event ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.seen[eventID]; exists {
		return false, nil
	}
	m.seen[eventID] = struct{}{}
	return true, nil
}

func (m *MemoryOutboxInbox) ClaimPending(ctx context.Context, limit int, owner string, lease time.Duration) ([]LeasedEnvelope, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, errors.New("messaging: lease owner is required")
	}
	if lease <= 0 {
		return nil, errors.New("messaging: lease duration must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	result := make([]LeasedEnvelope, 0, limit)
	for _, event := range m.pending {
		if limit > 0 && len(result) >= limit {
			break
		}
		if current, ok := m.leases[event.EventID]; ok && current.until.After(now) {
			continue
		}
		if retryAt, ok := m.next[event.EventID]; ok && retryAt.After(now) {
			continue
		}
		until := now.Add(lease)
		token := uuid.NewString()
		m.leases[event.EventID] = memoryLease{owner: owner, token: token, until: until}
		result = append(result, LeasedEnvelope{Envelope: event, LeaseOwner: owner, LeaseToken: token, LeaseUntil: until})
	}
	return result, nil
}

func (m *MemoryOutboxInbox) MarkSuccess(ctx context.Context, eventID, token string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[eventID]
	if !ok || lease.token != token {
		return fmt.Errorf("messaging: outbox event %q lease is not owned", eventID)
	}
	delete(m.leases, eventID)
	for i, event := range m.pending {
		if event.EventID == eventID {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			break
		}
	}
	return nil
}

func (m *MemoryOutboxInbox) MarkFailure(ctx context.Context, eventID, token string, failure error, next time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[eventID]
	if !ok || lease.token != token {
		return fmt.Errorf("messaging: outbox event %q lease is not owned", eventID)
	}
	_ = failure
	delete(m.leases, eventID)
	m.next[eventID] = next
	return nil
}

func (m *MemoryOutboxInbox) ClaimDurable(ctx context.Context, eventID, owner string, lease time.Duration) (bool, string, error) {
	if err := contextError(ctx); err != nil {
		return false, "", err
	}
	if eventID == "" || owner == "" || lease <= 0 {
		return false, "", errors.New("messaging: invalid inbox lease")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, completed := m.seen[eventID]; completed {
		return false, "", nil
	}
	if current, ok := m.leases["inbox:"+eventID]; ok && current.until.After(time.Now()) {
		return false, "", nil
	}
	token := uuid.NewString()
	m.leases["inbox:"+eventID] = memoryLease{owner: owner, token: token, until: time.Now().Add(lease)}
	return true, token, nil
}

func (m *MemoryOutboxInbox) Complete(ctx context.Context, eventID, token string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := "inbox:" + eventID
	lease, ok := m.leases[key]
	if !ok || lease.token != token {
		return fmt.Errorf("messaging: inbox event %q lease is not owned", eventID)
	}
	delete(m.leases, key)
	m.seen[eventID] = struct{}{}
	return nil
}

func (m *MemoryOutboxInbox) Release(ctx context.Context, eventID, token string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := "inbox:" + eventID
	lease, ok := m.leases[key]
	if !ok || lease.token != token {
		return fmt.Errorf("messaging: inbox event %q lease is not owned", eventID)
	}
	delete(m.leases, key)
	return nil
}
