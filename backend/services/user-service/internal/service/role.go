package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	casbinpkg "github.com/panda-dev/panda-v2/backend/services/user-service/internal/casbin"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// AdminRoleService 平台角色管理
type AdminRoleService struct {
	roles repository.AdminRoleRepository
}

func NewAdminRoleService(roles repository.AdminRoleRepository) *AdminRoleService {
	return &AdminRoleService{roles: roles}
}

func (s *AdminRoleService) List(ctx context.Context) ([]*model.AdminRole, error) {
	return s.roles.FindAll(ctx)
}

func (s *AdminRoleService) GetByID(ctx context.Context, id string) (*model.AdminRole, error) {
	return s.roles.FindByID(ctx, id)
}

func (s *AdminRoleService) Create(ctx context.Context, code, name, description string) (*model.AdminRole, error) {
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
	return r, nil
}

func (s *AdminRoleService) Update(ctx context.Context, id, code, name, description string) (*model.AdminRole, error) {
	r, err := s.roles.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r.Code = code
	r.Name = name
	r.Description = description
	r.UpdatedAt = time.Now()
	if err := s.roles.Update(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *AdminRoleService) Delete(ctx context.Context, id string) error {
	return s.roles.Delete(ctx, id)
}

// AdminPermissionService 平台权限管理
type AdminPermissionService struct {
	perms repository.AdminPermissionRepository
}

func NewAdminPermissionService(perms repository.AdminPermissionRepository) *AdminPermissionService {
	return &AdminPermissionService{perms: perms}
}

func (s *AdminPermissionService) List(ctx context.Context) ([]*model.AdminPermission, error) {
	return s.perms.FindAll(ctx)
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
	return p, nil
}

func (s *AdminPermissionService) Delete(ctx context.Context, id string) error {
	return s.perms.Delete(ctx, id)
}

// AdminBindingService 角色-权限、用户-角色绑定管理
type AdminBindingService struct {
	bindings repository.AdminBindingRepository
	enforcer *casbinpkg.Enforcer
}

func NewAdminBindingService(bindings repository.AdminBindingRepository, enforcer *casbinpkg.Enforcer) *AdminBindingService {
	return &AdminBindingService{bindings: bindings, enforcer: enforcer}
}

func (s *AdminBindingService) AssignPermissionsToRole(ctx context.Context, roleID string, permissionIDs []string) error {
	if err := s.bindings.AssignPermissionsToRole(ctx, roleID, permissionIDs); err != nil {
		return err
	}
	return s.enforcer.Reload()
}

func (s *AdminBindingService) RemovePermissionFromRole(ctx context.Context, roleID, permissionID string) error {
	if err := s.bindings.RemovePermissionFromRole(ctx, roleID, permissionID); err != nil {
		return err
	}
	return s.enforcer.Reload()
}

func (s *AdminBindingService) AssignRolesToUser(ctx context.Context, userID string, roleIDs []string) error {
	if err := s.bindings.AssignRolesToUser(ctx, userID, roleIDs); err != nil {
		return err
	}
	return s.enforcer.Reload()
}

func (s *AdminBindingService) RemoveRoleFromUser(ctx context.Context, userID, roleID string) error {
	if err := s.bindings.RemoveRoleFromUser(ctx, userID, roleID); err != nil {
		return err
	}
	return s.enforcer.Reload()
}

func (s *AdminBindingService) ListRolesByUser(ctx context.Context, userID string) ([]*model.AdminRole, error) {
	return s.bindings.FindRolesByUser(ctx, userID)
}
