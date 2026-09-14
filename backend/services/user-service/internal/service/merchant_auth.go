package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

type MerchantAuthService struct {
	users  repository.MerchantUserRepository
	access MerchantAccessPort
	jwtSvc *auth.Service
}

func NewMerchantAuthService(users repository.MerchantUserRepository, access MerchantAccessPort, jwtSvc *auth.Service) *MerchantAuthService {
	return &MerchantAuthService{users: users, access: access, jwtSvc: jwtSvc}
}

// Login 校验链：账号存在 → 密码正确 → 账号 active → 商户 active；
// 成功后尽力而为地记录最后登录时间/IP/次数，失败不影响登录
func (s *MerchantAuthService) Login(ctx context.Context, username, password, ip string) (*LoginResult, error) {
	user, err := s.users.FindByUsername(ctx, username)
	if err != nil || user == nil {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := s.CheckAccess(ctx, user); err != nil {
		return nil, err
	}

	grant := auth.Grant{
		Subject:   user.ID,
		UserID:    user.ID,
		AccountID: user.ID,
		Tenant:    user.MerchantID,
		Realm:     auth.RealmMerchant,
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

// CheckAccess 校验账号及所属商户的当前状态，供登录和 /users/me 复用。
// 商户查询错误原样返回，由调用方区分商户缺失与依赖不可用。
func (s *MerchantAuthService) CheckAccess(ctx context.Context, user *model.MerchantUser) error {
	if user == nil || strings.TrimSpace(user.ID) == "" || strings.TrimSpace(user.MerchantID) == "" {
		return errors.New("invalid merchant user profile")
	}
	if user.Status != "active" {
		return ErrMerchantUserDisabled
	}
	merchantStatus, err := s.access.FindStatus(ctx, user.MerchantID)
	if err != nil {
		return err
	}
	switch merchantStatus {
	case "active":
		return nil
	case "pending":
		return ErrMerchantPending
	case "suspended":
		return ErrMerchantSuspended
	default:
		return errors.New("invalid merchant status")
	}
}

// Profile 返回当前登录商户账号的信息
func (s *MerchantAuthService) Profile(ctx context.Context, userID string) (*model.MerchantUser, error) {
	return s.users.FindByID(ctx, userID)
}

// MerchantName 返回账号所属商户的名称，供 /users/me 回显
func (s *MerchantAuthService) MerchantName(ctx context.Context, merchantID string) (string, error) {
	merchantName, err := s.access.FindName(ctx, merchantID)
	if err != nil {
		return "", err
	}
	return merchantName, nil
}
