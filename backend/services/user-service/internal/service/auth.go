package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
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
		Subject:     claims.UserID,
		UserID:      claims.UserID,
		AccountID:   claims.UserID,
		Tenant:      claims.Tenant,
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
type AdminUserService struct {
	users repository.AdminUserRepository
}

func NewAdminUserService(users repository.AdminUserRepository) *AdminUserService {
	return &AdminUserService{users: users}
}

func (s *AdminUserService) List(ctx context.Context) ([]*model.AdminUser, error) {
	return s.users.FindAll(ctx)
}

func (s *AdminUserService) GetByID(ctx context.Context, id string) (*model.AdminUser, error) {
	return s.users.FindByID(ctx, id)
}

func (s *AdminUserService) CreateUser(ctx context.Context, username, password, name, email string) (*model.AdminUser, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &model.AdminUser{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: string(hash),
		Name:         name,
		Email:        email,
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *AdminUserService) UpdateStatus(ctx context.Context, id, status string) error {
	return s.users.UpdateStatus(ctx, id, status)
}

// MerchantAuthService 商户账号登录/token 签发。
// 账号由平台在「商户管理」中创建，username 全局唯一，登录只需 username+password。
type MerchantAuthService struct {
	users     repository.MerchantUserRepository
	merchants repository.MerchantRepository
	jwtSvc    *auth.Service
}

func NewMerchantAuthService(users repository.MerchantUserRepository, merchants repository.MerchantRepository, jwtSvc *auth.Service) *MerchantAuthService {
	return &MerchantAuthService{users: users, merchants: merchants, jwtSvc: jwtSvc}
}

// Login 校验链：账号存在 → 密码正确 → 账号 active → 商户 active；
// 成功后尽力而为地记录最后登录时间/IP/次数，失败不影响登录
func (s *MerchantAuthService) Login(ctx context.Context, username, password, ip string) (*LoginResult, error) {
	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	if user.Status != "active" {
		return nil, ErrMerchantUserDisabled
	}
	merchant, err := s.merchants.FindByID(ctx, user.MerchantID)
	if err != nil {
		return nil, err
	}
	if merchant.Status != "active" {
		if merchant.Status == "pending" {
			return nil, ErrMerchantPending
		}
		return nil, ErrMerchantSuspended
	}

	grant := auth.Grant{
		Subject:   user.ID,
		UserID:    user.ID,
		AccountID: user.ID,
		Tenant:    user.MerchantID,
	}
	accessToken, err := s.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		return nil, err
	}
	refreshToken, err := s.jwtSvc.SignRefreshGrant(grant)
	if err != nil {
		return nil, err
	}
	_ = s.users.TouchLogin(ctx, user.ID, ip)
	return &LoginResult{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

// Profile 返回当前登录商户账号的信息
func (s *MerchantAuthService) Profile(ctx context.Context, userID string) (*model.MerchantUser, error) {
	return s.users.FindByID(ctx, userID)
}

// MerchantName 返回账号所属商户的名称，供 /users/me 回显
func (s *MerchantAuthService) MerchantName(ctx context.Context, merchantID string) (string, error) {
	merchant, err := s.merchants.FindByID(ctx, merchantID)
	if err != nil {
		return "", err
	}
	return merchant.Name, nil
}
