package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

type AdminBindingService struct {
	bindings repository.AdminBindingRepository
	reloader PolicyReloader
}

func NewAdminBindingService(bindings repository.AdminBindingRepository, reloader PolicyReloader) *AdminBindingService {
	return &AdminBindingService{bindings: bindings, reloader: reloader}
}

func (s *AdminBindingService) AssignPermissionsToRole(ctx context.Context, roleID string, permissionIDs []string) error {
	if err := s.bindings.AssignPermissionsToRole(ctx, roleID, permissionIDs); err != nil {
		return err
	}
	return reloadPolicy(s.reloader)
}

func (s *AdminBindingService) RemovePermissionFromRole(ctx context.Context, roleID, permissionID string) error {
	if err := s.bindings.RemovePermissionFromRole(ctx, roleID, permissionID); err != nil {
		return err
	}
	return reloadPolicy(s.reloader)
}

func (s *AdminBindingService) AssignRolesToUser(ctx context.Context, userID string, roleIDs []string) error {
	if err := s.bindings.AssignRolesToUser(ctx, userID, roleIDs); err != nil {
		return err
	}
	return reloadPolicy(s.reloader)
}

func (s *AdminBindingService) RemoveRoleFromUser(ctx context.Context, userID, roleID string) error {
	if err := s.bindings.RemoveRoleFromUser(ctx, userID, roleID); err != nil {
		return err
	}
	return reloadPolicy(s.reloader)
}

func (s *AdminBindingService) ListRolesByUser(ctx context.Context, userID string) ([]*model.AdminRole, error) {
	return s.bindings.FindRolesByUser(ctx, userID)
}
