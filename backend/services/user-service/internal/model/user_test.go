package model_test

import (
	"context"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type fakeRepo struct{ user model.User }

func (f fakeRepo) Create(_ context.Context, name string) (model.User, error) {
	f.user.Name = name
	return f.user, nil
}
func (f fakeRepo) GetByID(context.Context, int64) (model.User, error) { return f.user, nil }
func (f fakeRepo) Update(context.Context, int64, model.UserUpdate) (model.User, error) {
	return f.user, nil
}

func TestRegisterTrimsAndValidatesName(t *testing.T) {
	s := service.NewService(fakeRepo{})
	u, err := s.Register(context.Background(), "  Alice ")
	if err != nil || u.Name != "Alice" {
		t.Fatalf("got user=%+v err=%v", u, err)
	}
	if _, err := s.Register(context.Background(), " "); err != model.ErrInvalidName {
		t.Fatalf("got err=%v", err)
	}
}
