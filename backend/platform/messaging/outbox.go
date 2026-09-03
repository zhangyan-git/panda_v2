package messaging

import (
	"context"
	"sync"
)

// Outbox stores events transactionally with application state. Implementations
// may bind Append and application writes to the same database transaction.
type Outbox interface {
	Append(context.Context, Envelope) error
	Pending(context.Context, int) ([]Envelope, error)
	MarkPublished(context.Context, string) error
}

// Inbox prevents duplicate processing of an event. Implementations should
// atomically claim an event ID before invoking a handler.
type Inbox interface {
	Claim(context.Context, string) (bool, error)
}

// MemoryOutboxInbox is a small process-local implementation for tests and
// development. It is not durable and must not be used for production state.
type MemoryOutboxInbox struct {
	mu      sync.Mutex
	pending []Envelope
	seen    map[string]struct{}
}

func NewMemoryOutboxInbox() *MemoryOutboxInbox {
	return &MemoryOutboxInbox{seen: make(map[string]struct{})}
}

func (m *MemoryOutboxInbox) Append(_ context.Context, event Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, event)
	return nil
}

func (m *MemoryOutboxInbox) Pending(_ context.Context, limit int) ([]Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.pending) {
		limit = len(m.pending)
	}
	result := append([]Envelope(nil), m.pending[:limit]...)
	return result, nil
}

func (m *MemoryOutboxInbox) MarkPublished(_ context.Context, eventID string) error {
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

func (m *MemoryOutboxInbox) Claim(_ context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.seen[eventID]; exists {
		return false, nil
	}
	m.seen[eventID] = struct{}{}
	return true, nil
}
