package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// fakeMerchantRepo 内存版 MerchantRepository，仅覆盖单测需要的行为
type fakeMerchantRepo struct {
	repository.MerchantRepository
	merchants map[string]*model.Merchant
	hasUsers  map[string]bool
}

func newFakeMerchantRepo() *fakeMerchantRepo {
	return &fakeMerchantRepo{
		merchants: map[string]*model.Merchant{},
		hasUsers:  map[string]bool{},
	}
}

func (f *fakeMerchantRepo) FindByID(_ context.Context, id string) (*model.Merchant, error) {
	m, ok := f.merchants[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return m, nil
}

func (f *fakeMerchantRepo) Create(_ context.Context, m *model.Merchant) error {
	f.merchants[m.ID] = m
	return nil
}

func (f *fakeMerchantRepo) UpdateStatus(_ context.Context, id, status string) error {
	m, ok := f.merchants[id]
	if !ok {
		return pgx.ErrNoRows
	}
	m.Status = status
	return nil
}

func (f *fakeMerchantRepo) HasUsers(_ context.Context, id string) (bool, error) {
	return f.hasUsers[id], nil
}

func (f *fakeMerchantRepo) Delete(_ context.Context, id string) error {
	delete(f.merchants, id)
	return nil
}

// fakeMerchantUserRepo 内存版 MerchantUserRepository
type fakeMerchantUserRepo struct {
	repository.MerchantUserRepository
	users map[string]*model.MerchantUser
}

func newFakeMerchantUserRepo() *fakeMerchantUserRepo {
	return &fakeMerchantUserRepo{users: map[string]*model.MerchantUser{}}
}

func (f *fakeMerchantUserRepo) FindByUsername(_ context.Context, username string) (*model.MerchantUser, error) {
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeMerchantUserRepo) FindByID(_ context.Context, id string) (*model.MerchantUser, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeMerchantUserRepo) Create(_ context.Context, u *model.MerchantUser) error {
	f.users[u.ID] = u
	return nil
}

func seedMerchant(f *fakeMerchantRepo, id, status string) {
	f.merchants[id] = &model.Merchant{ID: id, Name: "商户" + id, Status: status, CreatedAt: time.Now()}
}

func TestValidMerchantTransition(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"pending", "active", true},   // 审核通过
		{"active", "suspended", true}, // 暂停
		{"suspended", "active", true}, // 恢复
		{"pending", "suspended", false},
		{"suspended", "pending", false},
		{"active", "active", false},
		{"active", "pending", false},
		{"pending", "pending", false},
	}
	for _, c := range cases {
		if got := validMerchantTransition(c.from, c.to); got != c.want {
			t.Errorf("validMerchantTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestMerchantUpdateStatus(t *testing.T) {
	repo := newFakeMerchantRepo()
	svc := NewAdminMerchantService(repo, newFakeMerchantUserRepo(), nil, nil)
	ctx := context.Background()

	seedMerchant(repo, "m1", "pending")
	if err := svc.UpdateStatus(ctx, "m1", "active"); err != nil {
		t.Fatalf("pending→active 应成功: %v", err)
	}
	if repo.merchants["m1"].Status != "active" {
		t.Fatalf("状态未落库: %s", repo.merchants["m1"].Status)
	}
	if err := svc.UpdateStatus(ctx, "m1", "active"); !errors.Is(err, ErrMerchantStatusTransition) {
		t.Fatalf("active→active 应拒绝, got %v", err)
	}

	seedMerchant(repo, "m2", "pending")
	if err := svc.UpdateStatus(ctx, "m2", "suspended"); !errors.Is(err, ErrMerchantStatusTransition) {
		t.Fatalf("pending→suspended 应拒绝, got %v", err)
	}

	if err := svc.UpdateStatus(ctx, "missing", "active"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("商户不存在应返回 pgx.ErrNoRows, got %v", err)
	}
}

func TestMerchantCreateForcesPending(t *testing.T) {
	repo := newFakeMerchantRepo()
	svc := NewAdminMerchantService(repo, newFakeMerchantUserRepo(), nil, nil)

	m, err := svc.Create(context.Background(), "测试商户", "", "", "")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if m.Status != "pending" {
		t.Fatalf("新商户状态应为 pending, got %s", m.Status)
	}
	if _, err := svc.Create(context.Background(), "", "", "", ""); !errors.Is(err, ErrMerchantNameRequired) {
		t.Fatalf("空名称应拒绝, got %v", err)
	}
}

func TestMerchantDeleteBlockedByUsers(t *testing.T) {
	repo := newFakeMerchantRepo()
	svc := NewAdminMerchantService(repo, newFakeMerchantUserRepo(), nil, nil)
	ctx := context.Background()

	seedMerchant(repo, "m1", "active")
	repo.hasUsers["m1"] = true
	if err := svc.Delete(ctx, "m1"); !errors.Is(err, ErrMerchantHasUsers) {
		t.Fatalf("有账号时应拒绝删除, got %v", err)
	}

	repo.hasUsers["m1"] = false
	if err := svc.Delete(ctx, "m1"); err != nil {
		t.Fatalf("无账号时应删除成功: %v", err)
	}
	if _, ok := repo.merchants["m1"]; ok {
		t.Fatal("商户未被删除")
	}
}

func TestMerchantCreateUserDuplicateUsername(t *testing.T) {
	merchants := newFakeMerchantRepo()
	users := newFakeMerchantUserRepo()
	svc := NewAdminMerchantService(merchants, users, nil, nil)
	ctx := context.Background()

	seedMerchant(merchants, "m1", "active")
	if _, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "", ""); err != nil {
		t.Fatalf("首次创建应成功: %v", err)
	}
	// 全局查重：换一个商户用同名账号也要拒绝
	seedMerchant(merchants, "m2", "active")
	if _, err := svc.CreateUser(ctx, "m2", "boss", "pass1234", "另一个老板", "", "", false, "", ""); !errors.Is(err, ErrMerchantUsernameTaken) {
		t.Fatalf("重复 username 应拒绝, got %v", err)
	}
	if _, err := svc.CreateUser(ctx, "missing", "newuser", "pass1234", "", "", "", false, "", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("商户不存在应返回 pgx.ErrNoRows, got %v", err)
	}
}
