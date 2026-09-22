package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

type miniappUserRepoStub struct {
	repository.UserRepository
	user           *model.User
	findErr        error
	updateErr      error
	profileUpdates int
}

func (r *miniappUserRepoStub) FindByID(context.Context, string) (*model.User, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.user, nil
}

func (r *miniappUserRepoStub) UpdateProfile(context.Context, string, repository.UserProfileUpdate) error {
	r.profileUpdates++
	return r.updateErr
}

// 资料写入与 Profile / BindPhone 同一道闸：access token 有 24 小时有效期，
// 期间账号可能已经被禁用或注销，只验签名的话这枚令牌还能继续改资料。
func TestUpdateProfileRefusesInactiveAccount(t *testing.T) {
	nickname := "新昵称"
	for _, tc := range []struct {
		name   string
		status string
		want   error
	}{
		{"禁用", model.UserStatusDisabled, ErrConsumerDisabled},
		{"已注销", model.UserStatusDeleted, ErrConsumerDeleted},
		{"未知状态按不可用处理", "something-else", ErrConsumerDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &miniappUserRepoStub{user: &model.User{ID: "u-1", Status: tc.status}}
			svc := NewMiniappUserService(repo, nil)

			_, err := svc.UpdateProfile(context.Background(), "u-1", ProfileUpdate{Nickname: &nickname})
			if !errors.Is(err, tc.want) {
				t.Fatalf("UpdateProfile() error = %v, want %v", err, tc.want)
			}
			if repo.profileUpdates != 0 {
				t.Fatal("不可用的账号不该写到仓储")
			}
		})
	}
}

func TestUpdateProfileWritesForActiveAccount(t *testing.T) {
	nickname := "新昵称"
	repo := &miniappUserRepoStub{user: &model.User{ID: "u-1", Status: model.UserStatusActive}}
	svc := NewMiniappUserService(repo, nil)

	if _, err := svc.UpdateProfile(context.Background(), "u-1", ProfileUpdate{Nickname: &nickname}); err != nil {
		t.Fatalf("UpdateProfile() error = %v", err)
	}
	if repo.profileUpdates != 1 {
		t.Fatalf("profile updates = %d, want 1", repo.profileUpdates)
	}
}

// 账号查不到时错误原样上抛，不吞成「改完了」。
func TestUpdateProfilePropagatesLookupFailure(t *testing.T) {
	nickname := "新昵称"
	repo := &miniappUserRepoStub{findErr: pgx.ErrNoRows}
	svc := NewMiniappUserService(repo, nil)

	if _, err := svc.UpdateProfile(context.Background(), "u-1", ProfileUpdate{Nickname: &nickname}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("UpdateProfile() error = %v, want %v", err, pgx.ErrNoRows)
	}
	if repo.profileUpdates != 0 {
		t.Fatal("账号都读不到，不该走到写")
	}
}
