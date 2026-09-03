package domain

import (
	"context"
	"testing"
)

type fakeRepo struct{ user User }

func (f fakeRepo) Create(_ context.Context, name string) (User, error) {
	f.user.Name = name
	return f.user, nil
}
func (f fakeRepo) GetByID(context.Context, int64) (User, error) { return f.user, nil }

func TestRegisterTrimsAndValidatesName(t *testing.T) {
	s := NewService(fakeRepo{})
	u, err := s.Register(context.Background(), "  Alice ")
	if err != nil || u.Name != "Alice" {
		t.Fatalf("got user=%+v err=%v", u, err)
	}
	if _, err := s.Register(context.Background(), " "); err != ErrInvalidName {
		t.Fatalf("got err=%v", err)
	}
}
