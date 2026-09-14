package service

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

// 商户登录链路的状态类错误，handler 映射为 403 + 明确提示
var (
	ErrMerchantUserDisabled = errors.New("账号已禁用")
	ErrMerchantPending      = errors.New("商户待审核")
	ErrMerchantSuspended    = errors.New("商户已暂停")
)

// AdminAuthService 平台管理员登录/token 签发
type AdminAuthService struct {
	users    repository.AdminUserRepository
	bindings repository.AdminBindingRepository
	jwtSvc   *auth.Service
}

func NewAdminAuthService(users repository.AdminUserRepository, bindings repository.AdminBindingRepository, jwtSvc *auth.Service) *AdminAuthService {
	return &AdminAuthService{users: users, bindings: bindings, jwtSvc: jwtSvc}
}

// Profile 返回当前登录管理员的账号信息
func (s *AdminAuthService) Profile(ctx context.Context, userID string) (*model.AdminUser, error) {
	return s.users.FindByID(ctx, userID)
}

// LiveAccess 返回用户在库中当前绑定的角色代码与权限码。
// JWT claims 里的角色/权限是登录时签发的，分配变更后不会同步；
// /users/me 用实时数据回显，前端权限与菜单才不会停留在旧 token 的状态。
func (s *AdminAuthService) LiveAccess(ctx context.Context, userID string) (roles, perms []string, err error) {
	boundRoles, err := s.bindings.FindRolesByUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	roles = make([]string, 0, len(boundRoles))
	for _, r := range boundRoles {
		roles = append(roles, r.Code)
	}
	perms, err = s.bindings.FindPermissionCodesByUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if perms == nil {
		perms = []string{}
	}
	return roles, perms, nil
}

type LoginResult struct {
	AccessToken  string
	RefreshToken string
}

func (s *AdminAuthService) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	roles, err := s.bindings.FindRolesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	roleCodes := make([]string, len(roles))
	for i, r := range roles {
		roleCodes[i] = r.Code
	}
	isSuper := false
	for _, role := range roles {
		if role.Code == "super_admin" {
			isSuper = true
			break
		}
	}

	permCodes, err := s.bindings.FindPermissionCodesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	grant := auth.Grant{
		Subject:     user.ID,
		UserID:      user.ID,
		AccountID:   user.ID,
		Tenant:      "",
		Realm:       auth.RealmPlatform,
		Roles:       roleCodes,
		Permissions: permCodes,
		IsSuper:     isSuper,
	}
	accessToken, err := s.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		return nil, err
	}
	refreshToken, err := s.jwtSvc.SignRefreshGrant(grant)
	if err != nil {
		return nil, err
	}
	return &LoginResult{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

func (s *AdminAuthService) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	claims, err := s.jwtSvc.ParseRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	// 重新查询权限，保证 token 刷新后权限是最新的
	roles, err := s.bindings.FindRolesByUser(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	roleCodes := make([]string, len(roles))
	for i, r := range roles {
		roleCodes[i] = r.Code
	}
	isSuper := false
	for _, role := range roles {
		if role.Code == "super_admin" {
			isSuper = true
			break
		}
	}
	permCodes, err := s.bindings.FindPermissionCodesByUser(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	grant := auth.Grant{
		Subject:   claims.UserID,
		UserID:    claims.UserID,
		AccountID: claims.UserID,
		Tenant:    claims.Tenant,
		// 从被解析的 refresh token 上继承，而不是重写成平台域：Refresh 只签发
		// 与来token同一批人的新 token，改了域就等于用一个接口把身份换掉了。
		Realm:       claims.Realm,
		Roles:       roleCodes,
		Permissions: permCodes,
		IsSuper:     isSuper,
	}
	accessToken, err := s.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		return nil, err
	}
	newRefresh, err := s.jwtSvc.SignRefreshGrant(grant)
	if err != nil {
		return nil, err
	}
	return &LoginResult{AccessToken: accessToken, RefreshToken: newRefresh}, nil
}

// AdminUserService 平台管理员信息管理
