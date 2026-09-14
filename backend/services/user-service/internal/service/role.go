package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// PolicyReloader refreshes authorization after a committed role change.
// 生产实现是 casbin.Broadcaster：本实例重载之外，还会把这次变更广播给其它副本。
type PolicyReloader interface {
	Reload() error
}

var ErrRolePolicyReload = errors.New("角色已保存，但权限刷新失败，请重试保存")

// reloadPolicy 把「已保存，但生效这一步失败」统一成可重试的错误。
// 角色与绑定两个服务共用：对调用方来说，本地重载失败和广播失败是同一件事。
func reloadPolicy(reloader PolicyReloader) error {
	if err := reloader.Reload(); err != nil {
		return fmt.Errorf("%w: %w", ErrRolePolicyReload, err)
	}
	return nil
}

// AdminRoleService 平台角色管理
type AdminRoleService struct {
	roles    repository.AdminRoleRepository
	reloader PolicyReloader
}

func NewAdminRoleService(roles repository.AdminRoleRepository, reloader PolicyReloader) *AdminRoleService {
	return &AdminRoleService{roles: roles, reloader: reloader}
}

func (s *AdminRoleService) reload() error {
	return reloadPolicy(s.reloader)
}

func (s *AdminRoleService) List(ctx context.Context, page, pageSize int) ([]*model.AdminRole, int64, error) {
	roles, err := s.roles.FindPage(ctx, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.roles.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	return roles, total, nil
}

func (s *AdminRoleService) GetByID(ctx context.Context, id string) (*model.AdminRole, error) {
	return s.roles.FindByID(ctx, id)
}

func (s *AdminRoleService) Create(ctx context.Context, code, name, description string) (*model.AdminRole, error) {
	if err := model.ValidateRoleCodeChange("", code); err != nil {
		return nil, err
	}
	now := time.Now()
	r := &model.AdminRole{
		ID:          uuid.NewString(),
		Code:        code,
		Name:        name,
		Description: description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.roles.Create(ctx, r); err != nil {
		return nil, err
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *AdminRoleService) Update(ctx context.Context, id, code, name, description string) (*model.AdminRole, error) {
	if err := model.ValidateRoleCode(code); err != nil {
		return nil, err
	}
	r, err := s.roles.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateRoleCodeChange(r.Code, code); err != nil {
		return nil, err
	}
	r.Code = code
	r.Name = name
	r.Description = description
	r.UpdatedAt = time.Now()
	if err := s.roles.Update(ctx, r); err != nil {
		return nil, err
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Delete 删掉角色之后必须重载：仓储层已经清掉了该角色的 p/g 规则，
// 但 enforcer 手里还是旧快照。少了这一步，权限撤销要等下一次重载或重启才生效——
// 而「撤销没生效」在界面上看不出来，是最不该留的那种 bug。
func (s *AdminRoleService) Delete(ctx context.Context, id string) error {
	if err := s.roles.Delete(ctx, id); err != nil {
		return err
	}
	return s.reload()
}

// AdminPermissionService 平台权限管理
//
// Create 不重载：新建的权限码没有任何绑定，策略不会因此改变。
// Update/Delete 会——权限码就是策略里的 obj，改名和删除都要让 enforcer 重新加载。
