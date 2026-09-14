package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

type AdminPermissionService struct {
	perms    repository.AdminPermissionRepository
	reloader PolicyReloader
}

func NewAdminPermissionService(perms repository.AdminPermissionRepository, reloader PolicyReloader) *AdminPermissionService {
	return &AdminPermissionService{perms: perms, reloader: reloader}
}

func (s *AdminPermissionService) reload() error {
	return reloadPolicy(s.reloader)
}

func (s *AdminPermissionService) List(ctx context.Context, page, pageSize int) ([]*model.AdminPermission, int64, error) {
	perms, err := s.perms.FindPage(ctx, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.perms.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	return perms, total, nil
}

func (s *AdminPermissionService) GetByID(ctx context.Context, id string) (*model.AdminPermission, error) {
	return s.perms.FindByID(ctx, id)
}

func (s *AdminPermissionService) ListByRole(ctx context.Context, roleID string) ([]*model.AdminPermission, error) {
	return s.perms.FindByRole(ctx, roleID)
}

func (s *AdminPermissionService) Create(ctx context.Context, code, name, description, group string) (*model.AdminPermission, error) {
	now := time.Now()
	p := &model.AdminPermission{
		ID:          uuid.NewString(),
		Code:        code,
		Name:        name,
		Description: description,
		PermGroup:   group,
		CreatedAt:   now,
	}
	if err := s.perms.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *AdminPermissionService) Update(ctx context.Context, id, code, name, description, group string) (*model.AdminPermission, error) {
	p, err := s.perms.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Code = code
	p.Name = name
	p.Description = description
	p.PermGroup = group
	if err := s.perms.Update(ctx, p); err != nil {
		return nil, err
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *AdminPermissionService) Delete(ctx context.Context, id string) error {
	if err := s.perms.Delete(ctx, id); err != nil {
		return err
	}
	return s.reload()
}

// AdminBindingService 角色-权限、用户-角色绑定管理
//
// 绑定改动直接决定谁能做什么，所以每次改动后都要刷新策略并广播，
// 走的是和角色变更同一条 PolicyReloader，不是直接操作 enforcer。
