package handler

import (
	"context"
	"sync"
	"time"
)

// Store persists refresh-token JTIs. Implementations consume and revoke atomically.
type Store interface {
	Register(context.Context, TokenRecord) error
	Consume(context.Context, string, time.Time) (bool, error)
	Revoke(context.Context, string, time.Time) (bool, error)
}

type TokenRecord struct {
	JTI       string
	AccountID string
	UserID    string
	ExpiresAt time.Time
}

// JTIStore is the process-local fallback used by tests and development.
type JTIStore struct {
	mu    sync.Mutex
	items map[string]TokenRecord
}

func NewJTIStore() *JTIStore { return &JTIStore{items: make(map[string]TokenRecord)} }
func (s *JTIStore) Add(jti string, expiresAt time.Time) {
	_ = s.register(TokenRecord{JTI: jti, ExpiresAt: expiresAt})
}
func (s *JTIStore) register(record TokenRecord) error {
	if record.JTI == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(time.Now())
	s.items[record.JTI] = record
	return nil
}
func (s *JTIStore) Consume(jti string, now time.Time) bool { ok, _ := s.consume(jti, now); return ok }
func (s *JTIStore) consume(jti string, now time.Time) (bool, error) {
	if jti == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.items[jti]
	if !ok {
		return false, nil
	}
	delete(s.items, jti)
	return record.ExpiresAt.After(now), nil
}
func (s *JTIStore) Revoke(jti string, now time.Time) bool { ok, _ := s.revoke(jti, now); return ok }
func (s *JTIStore) revoke(jti string, now time.Time) (bool, error) {
	if jti == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked(now)
	if _, ok := s.items[jti]; !ok {
		return false, nil
	}
	delete(s.items, jti)
	return true, nil
}
func (s *JTIStore) removeExpiredLocked(now time.Time) {
	for jti, record := range s.items {
		if !record.ExpiresAt.After(now) {
			delete(s.items, jti)
		}
	}
}

type MemoryStore struct{ *JTIStore }

func NewMemoryStore() *MemoryStore { return &MemoryStore{NewJTIStore()} }
func (s *MemoryStore) Register(_ context.Context, record TokenRecord) error {
	return s.register(record)
}
func (s *MemoryStore) Consume(_ context.Context, jti string, now time.Time) (bool, error) {
	return s.consume(jti, now)
}
func (s *MemoryStore) Revoke(_ context.Context, jti string, now time.Time) (bool, error) {
	return s.revoke(jti, now)
}

var _ Store = (*MemoryStore)(nil)
