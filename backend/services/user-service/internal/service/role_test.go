package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// countingReloader 记录重载次数，并可被指定为总是失败。
type countingReloader struct {
	calls int
	err   error
}

func (c *countingReloader) Reload() error {
	c.calls++
	return c.err
}

type fakeRoleRepo struct {
	repository.AdminRoleRepository
	role      *model.AdminRole
	deleted   []string
	deleteErr error
}

func (f *fakeRoleRepo) FindByID(context.Context, string) (*model.AdminRole, error) {
	if f.role == nil {
		return nil, errors.New("role not found")
	}
	return f.role, nil
}

func (f *fakeRoleRepo) Create(_ context.Context, r *model.AdminRole) error {
	f.role = r
	return nil
}

func (f *fakeRoleRepo) Update(_ context.Context, r *model.AdminRole) error {
	f.role = r
	return nil
}

func (f *fakeRoleRepo) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

type fakePermRepo struct {
	repository.AdminPermissionRepository
	perm      *model.AdminPermission
	deleted   []string
	deleteErr error
}

func (f *fakePermRepo) FindByID(context.Context, string) (*model.AdminPermission, error) {
	if f.perm == nil {
		return nil, errors.New("permission not found")
	}
	return f.perm, nil
}

func (f *fakePermRepo) Create(_ context.Context, p *model.AdminPermission) error {
	f.perm = p
	return nil
}

func (f *fakePermRepo) Update(_ context.Context, p *model.AdminPermission) error {
	f.perm = p
	return nil
}

func (f *fakePermRepo) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

// 删除角色必须重载策略。仓储层已经在同一个事务里清掉了 casbin_rule 的 p/g 行，
// 但 enforcer 手里还是旧快照——不重载的话，被删角色的成员继续通过校验，
// 而「撤销没生效」在界面上看不出任何异常。这是权限撤销不生效，不是延迟生效。
func TestAdminRoleDeleteReloadsPolicy(t *testing.T) {
	repo := &fakeRoleRepo{role: &model.AdminRole{ID: "r1", Code: "ops"}}
	reloader := &countingReloader{}
	svc := NewAdminRoleService(repo, reloader)

	if err := svc.Delete(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "r1" {
		t.Fatalf("want the repository to delete r1, got %v", repo.deleted)
	}
	if reloader.calls != 1 {
		t.Fatalf("deleting a role must reload the policy exactly once, got %d", reloader.calls)
	}
}

// 仓储层失败时不许重载：那说明压根没保存成功，不是「已保存但没生效」。
func TestAdminRoleDeleteDoesNotReloadWhenRepositoryFails(t *testing.T) {
	repo := &fakeRoleRepo{role: &model.AdminRole{ID: "r1"}, deleteErr: errors.New("db down")}
	reloader := &countingReloader{}
	svc := NewAdminRoleService(repo, reloader)

	if err := svc.Delete(context.Background(), "r1"); err == nil {
		t.Fatal("want the repository error to surface")
	}
	if reloader.calls != 0 {
		t.Fatalf("a failed delete must not reload, got %d calls", reloader.calls)
	}
}

// 重载失败要包成 ErrRolePolicyReload：数据其实已经提交了，
// handler 需要据此提示「再存一次」而不是「保存失败」。这个区分是提示正确的前提。
func TestAdminRoleDeleteWrapsReloadFailure(t *testing.T) {
	repo := &fakeRoleRepo{role: &model.AdminRole{ID: "r1"}}
	reloader := &countingReloader{err: errors.New("redis down")}
	svc := NewAdminRoleService(repo, reloader)

	if err := svc.Delete(context.Background(), "r1"); !errors.Is(err, ErrRolePolicyReload) {
		t.Fatalf("want ErrRolePolicyReload, got %v", err)
	}
}

// 权限码就是策略里的 obj，删掉它等于撤走所有角色对它的授权——同一条道理。
func TestAdminPermissionDeleteReloadsPolicy(t *testing.T) {
	repo := &fakePermRepo{perm: &model.AdminPermission{ID: "p1", Code: "brands:view"}}
	reloader := &countingReloader{}
	svc := NewAdminPermissionService(repo, reloader)

	if err := svc.Delete(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "p1" {
		t.Fatalf("want the repository to delete p1, got %v", repo.deleted)
	}
	if reloader.calls != 1 {
		t.Fatalf("deleting a permission must reload the policy exactly once, got %d", reloader.calls)
	}
}

// 改名同样要重载：仓储层把 casbin_rule.v2 从旧码迁到新码，
// enforcer 不重新加载就还在按旧码放行。
func TestAdminPermissionUpdateReloadsPolicy(t *testing.T) {
	repo := &fakePermRepo{perm: &model.AdminPermission{ID: "p1", Code: "brands:view"}}
	reloader := &countingReloader{}
	svc := NewAdminPermissionService(repo, reloader)

	if _, err := svc.Update(context.Background(), "p1", "brands:edit", "编辑品牌", "", "品牌"); err != nil {
		t.Fatal(err)
	}
	if reloader.calls != 1 {
		t.Fatalf("renaming a permission must reload the policy exactly once, got %d", reloader.calls)
	}
}

// Create 不重载是有意的：新建的权限码一条绑定都没有，策略不会因此改变。
// 钉下来免得以后被当成漏掉的一步补上。
func TestAdminPermissionCreateDoesNotReloadPolicy(t *testing.T) {
	repo := &fakePermRepo{}
	reloader := &countingReloader{}
	svc := NewAdminPermissionService(repo, reloader)

	p, err := svc.Create(context.Background(), "brands:view", "查看品牌", "", "品牌")
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != "brands:view" {
		t.Fatalf("want the created permission back, got %+v", p)
	}
	if reloader.calls != 0 {
		t.Fatalf("creating an unbound permission must not reload, got %d calls", reloader.calls)
	}
}
