package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrMerchantUsernameTaken = errors.New("用户名已存在")
	ErrScopeTypeInvalid      = errors.New("无效的数据范围类型")
	ErrScopeIDRequired       = errors.New("数据范围目标不能为空")
	ErrScopeOutOfMerchant    = errors.New("数据范围目标不属于该商户")
)

// MerchantAccountService manages merchant login accounts and their data scopes.
type MerchantAccountService struct {
	merchants MerchantAccessPort
	users     repository.MerchantUserRepository
	resources MerchantResourceAccess
}

func NewMerchantAccountService(merchants MerchantAccessPort, users repository.MerchantUserRepository, resources MerchantResourceAccess) *MerchantAccountService {
	return &MerchantAccountService{merchants: merchants, users: users, resources: resources}
}

// ListUsers 返回某商户下的一页账号及其总数。
//
// scope 名称只对当前页解析：那是逐行展示用的补充信息，给整批账号预先解析
// 等于把分页省下的开销又还回去。
func (s *MerchantAccountService) ListUsers(ctx context.Context, merchantID string, page, pageSize int) ([]*model.MerchantUser, int64, error) {
	if _, err := s.merchants.FindStatus(ctx, merchantID); err != nil {
		return nil, 0, err
	}
	users, err := s.users.FindPage(ctx, merchantID, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	if err := s.decorateScopeNames(ctx, users...); err != nil {
		return nil, 0, err
	}
	total, err := s.users.Count(ctx, merchantID)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// decorateScopeNames fills in each account's scope display names. The names come
// from the merchant database, so one RPC answers the whole batch instead of a
// join the identity database can no longer make. A scope that no longer exists
// is absent from the answer and simply leaves that account's name empty:
// display data must not turn a listing into an error.
//
// ScopeNames comes back in the same order and with the same length as ScopeIDs:
// the two are read side by side, and a name that is dropped or reordered pairs
// with the wrong target. An id that cannot be resolved keeps its slot as an
// empty string rather than being squeezed out of the slice.
func (s *MerchantAccountService) decorateScopeNames(ctx context.Context, users ...*model.MerchantUser) error {
	var brandIDs, storeIDs []string
	for _, u := range users {
		if u == nil {
			continue
		}
		switch u.ScopeType {
		case "brand":
			brandIDs = append(brandIDs, u.ScopeIDs...)
		case "store":
			storeIDs = append(storeIDs, u.ScopeIDs...)
		}
	}
	if len(brandIDs) == 0 && len(storeIDs) == 0 {
		return nil
	}
	brandNames, storeNames, err := s.resources.ScopeNames(ctx, brandIDs, storeIDs)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u == nil {
			continue
		}
		names := make([]string, 0, len(u.ScopeIDs))
		for _, id := range u.ScopeIDs {
			switch u.ScopeType {
			case "brand":
				names = append(names, brandNames[id])
			case "store":
				names = append(names, storeNames[id])
			}
		}
		u.ScopeNames = names
	}
	return nil
}

// validateScope 逐个核对范围目标确实属于这个商户。
//
// 锚点是**每一个**目标，不是第一个：账号 A 的范围里混进一个 B 商户的品牌 id，展开出来的
// 点位集合就跨了商户，而这条错误在界面上看不出来（列表里只是多了一个陌生的名字）。
// 品牌档与门店档只差一个查询函数，分成两段写会让「新增一档」变成抄一整段。
func (s *MerchantAccountService) validateScope(ctx context.Context, merchantID, scopeType string, scopeIDs []string) error {
	switch scopeType {
	case "merchant":
		return nil
	case "brand", "store":
		if len(scopeIDs) == 0 {
			return ErrScopeIDRequired
		}
		owner := s.resources.FindBrandMerchantID
		if scopeType == "store" {
			owner = s.resources.FindStoreMerchantID
		}
		for _, id := range scopeIDs {
			merchantResourceID, err := owner(ctx, id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrScopeOutOfMerchant
				}
				return err
			}
			if merchantResourceID != merchantID {
				return ErrScopeOutOfMerchant
			}
		}
		return nil
	default:
		return ErrScopeTypeInvalid
	}
}

// normalizeScope 把空档收成 merchant 档，并把目标归一成一组：去掉空串、去重、排序。
//
// 排序是给回显用的：scope_names 与 scope_ids 同序回给界面，而数组在库里存的是插入顺序，
// 不排的话同一组目标在两次读取里可能顺序不同，列表上看着像被人改过。去重顺带把
// 「同一个品牌勾了两次」这种客户端重复挡在库外。
func normalizeScope(scopeType string, scopeIDs []string) (string, []string) {
	if scopeType == "" || scopeType == "merchant" {
		// 商户档的目标必须是空：库里真存了一组也不采用——让一个用不上的字段参与
		// 决定边界，等于留了条谁都不知道的旁路（同 ScopeOf）。
		return "merchant", []string{}
	}
	ids := make([]string, 0, len(scopeIDs))
	seen := make(map[string]struct{}, len(scopeIDs))
	for _, id := range scopeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return scopeType, ids
}

func (s *MerchantAccountService) CreateUser(ctx context.Context, merchantID, username, password, name, email, phone string, isAdmin bool, scopeType string, scopeIDs []string) (*model.MerchantUser, error) {
	if _, err := s.merchants.FindStatus(ctx, merchantID); err != nil {
		return nil, err
	}
	scopeType, scopeIDs = normalizeScope(scopeType, scopeIDs)
	if err := s.validateScope(ctx, merchantID, scopeType, scopeIDs); err != nil {
		return nil, err
	}
	if existing, err := s.users.FindByUsername(ctx, username); err == nil && existing != nil {
		return nil, ErrMerchantUsernameTaken
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &model.MerchantUser{ID: uuid.NewString(), MerchantID: merchantID, Username: username, PasswordHash: string(hash), Name: name, Email: email, Phone: phone, Status: "active", IsAdmin: isAdmin, ScopeType: scopeType, ScopeIDs: scopeIDs, CreatedAt: now, UpdatedAt: now}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	// 账号已经落库，这里只是把范围名称补进响应；解析失败就留空，
	// 不能让一次「已成功」的创建看起来像失败。列表接口会再查一次。
	_ = s.decorateScopeNames(ctx, u)
	return u, nil
}

func (s *MerchantAccountService) UpdateUserScope(ctx context.Context, id, scopeType string, scopeIDs []string, isAdmin bool) error {
	u, err := s.users.FindByID(ctx, id)
	if err != nil {
		return err
	}
	scopeType, scopeIDs = normalizeScope(scopeType, scopeIDs)
	if err := s.validateScope(ctx, u.MerchantID, scopeType, scopeIDs); err != nil {
		return err
	}
	return s.users.UpdateScope(ctx, id, scopeType, scopeIDs, isAdmin)
}

func (s *MerchantAccountService) UpdateUserStatus(ctx context.Context, id, status string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.UpdateStatus(ctx, id, status)
}

func (s *MerchantAccountService) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.Delete(ctx, id)
}
