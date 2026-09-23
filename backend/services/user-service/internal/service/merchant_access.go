package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// ErrMerchantAccountMismatch 表示这个账号行与请求它的身份对不上：行没了、id 变了、
// 或者它属于另一个商户。它区别于「账号被停用」——那一类有各自的错误，会给用户看。
var ErrMerchantAccountMismatch = errors.New("merchant account does not match the requesting identity")

// MerchantAccess 是一个商户账号**此刻**的数据边界。
//
// StoreIDs 是边界本身，不是边界的描述：下游的设备表与订单表都只持有 store_id，
// 而方案 §5.2.3 明确禁止为了让过滤统一就给业务表加商户字段。三档范围展开成同一
// 组点位 id，下游就只有一条代码路径。
type MerchantAccess struct {
	MerchantID string
	ScopeType  string
	// ScopeIDs 是范围目标本身（品牌或门店 id 的一组），StoreIDs 是它们展开后的点位。
	// 下游过滤只用 StoreIDs；这一组跟着走是为了让「边界是怎么来的」还能被读出来。
	ScopeIDs []string
	StoreIDs []string
}

// MerchantAccessService 回答「这个商户账号现在能看见哪些点位」。
//
// 它每次调用都重新读账号行与商户状态，这就是它有别于「把范围签进令牌」的地方：
// access token 的有效期是 24 小时，签进去意味着改一次范围要等一天才生效，而停用
// 一个账号同样要等一天才落地。§5.2.4 禁止的正是这个。
type MerchantAccessService struct {
	users repository.MerchantUserRepository
	auth  *MerchantAuthService
	scope MerchantResourceAccess
}

func NewMerchantAccessService(users repository.MerchantUserRepository, auth *MerchantAuthService, scope MerchantResourceAccess) *MerchantAccessService {
	return &MerchantAccessService{users: users, auth: auth, scope: scope}
}

// Resolve 按 userID 解析数据边界，并核对它确实属于 tenant。
//
// 调用方（rpc 层）已经验过令牌的 realm 与 Subject，这里再查一遍账号行，是因为
// 那些都是**令牌说的**：账号可能已经被删、被停用，或者范围被改过。令牌没变，
// 答案可以完全不同——这正是每请求重算的全部意义。
func (s *MerchantAccessService) Resolve(ctx context.Context, userID, tenant string) (MerchantAccess, error) {
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		// pgx.ErrNoRows 原样往上：账号不存在是「未认证」，不是「依赖不可用」，
		// 这个区分由 rpc 层来做，因为只有它知道该翻成哪个 gRPC 码。
		return MerchantAccess{}, err
	}
	if user == nil || user.ID != userID || strings.TrimSpace(user.MerchantID) == "" {
		return MerchantAccess{}, ErrMerchantAccountMismatch
	}
	if user.MerchantID != tenant {
		return MerchantAccess{}, ErrMerchantAccountMismatch
	}
	// 账号状态与商户主体状态复用登录链路那一套判定：同一个账号，从哪条路径进来
	// 都该得到同一个答案。商户被暂停时登录会被拒，已经登录的也必须在下一个请求
	// 上就失效——两处各写一遍判定，迟早会分叉成「登录进不来、但进得来的还能用」。
	if err := s.auth.CheckAccess(ctx, user); err != nil {
		return MerchantAccess{}, err
	}
	return s.ScopeOf(ctx, user)
}

// ScopeOf 展开一个**账号与商户状态都已校验过**的账号的数据边界。
//
// 拆出来是给 `/v1/merchant/users/me` 用的：那条路径在同一次请求里已经做过 Profile 与
// CheckAccess，再走一遍 Resolve 就是把同样的读取与判定各做两遍——而两份判定迟早会分叉
// 成「列表接口放行、个人信息接口拦下」这种只有用户看得见的分歧。
// 调用方必须先做完那两步：ScopeOf 只看账号行怎么写，不看它有没有资格。
func (s *MerchantAccessService) ScopeOf(ctx context.Context, user *model.MerchantUser) (MerchantAccess, error) {
	if user == nil || strings.TrimSpace(user.MerchantID) == "" {
		return MerchantAccess{}, ErrMerchantAccountMismatch
	}
	scopeType := normalizeScopeType(user.ScopeType)
	if !validScopeType(scopeType) {
		// 认不出的档位不能当成商户全量，也不能当成空集：前者放开，后者让人以为
		// 账号没授权却查不出原因。报错，让中间件回 503。
		return MerchantAccess{}, ErrScopeTypeInvalid
	}
	scopeIDs := user.ScopeIDs
	if scopeType == "merchant" {
		// 商户档的目标应当为空。库里真存了一组也不采用——让一个用不上的字段参与
		// 决定边界，等于留了条谁都不知道的旁路。
		scopeIDs = []string{}
	}
	if scopeIDs == nil {
		// 与 StoreIDs 同理：nil 是不该往上传的形状。这一组跟着边界走（见
		// MerchantAccess.ScopeIDs），消费端会读它的长度，nil 在那里同样意味着
		// 「没这一回事」而不是「一个都没有」。
		scopeIDs = []string{}
	}
	storeIDs, err := s.scope.ListStoreIDs(ctx, user.MerchantID, scopeType, scopeIDs)
	if err != nil {
		// 展开不出来就是**给不出边界**：不降级成空集（那会让人以为是没授权），
		// 更不降级成全量。
		return MerchantAccess{}, err
	}
	if storeIDs == nil {
		// 空集是一个正常答案；nil 不是。下游的过滤谓词判的正是 nil 与空切片，
		// 而 nil 在那里意味着「不过滤」。这里收口一次，消费端就不会拿到它。
		storeIDs = []string{}
	}
	return MerchantAccess{
		MerchantID: user.MerchantID,
		ScopeType:  scopeType,
		ScopeIDs:   scopeIDs,
		StoreIDs:   storeIDs,
	}, nil
}

// normalizeScopeType 把空值收成 merchant 档。
//
// merchant_users.scope_type 有默认值 'merchant'，但列可空、历史行也可能为空串。
// 空值唯一能安全读成的东西就是「本商户」——它比任何其他档位都窄不了，却仍然
// 被 merchant_id 锚住，不会跨商户。把空值读成「不过滤」才是危险的。
func normalizeScopeType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "merchant"
	}
	return value
}

func validScopeType(value string) bool {
	switch value {
	case "merchant", "brand", "store":
		return true
	default:
		return false
	}
}

// MerchantScopeNames 解析范围目标的显示名，供商户端回显「你的数据范围」。
//
// 商户档没有目标可解析，回空切片：那一档的名字是界面文案（「全部门店」），属于
// 调用方的词汇，不是这条服务的事实。品牌/门店档查不到（范围目标被删了）同样留空
// ——展示数据不该把一次读取变成错误。
//
// 返回的切片与 access.ScopeIDs **同序等长**，查不到的 id 占一个空串，理由见
// MerchantAccountService.decorateScopeNames。
func (s *MerchantAccessService) MerchantScopeNames(ctx context.Context, access MerchantAccess) ([]string, error) {
	switch access.ScopeType {
	case "merchant":
		return []string{}, nil
	case "brand", "store":
		var brandNames, storeNames map[string]string
		var err error
		if access.ScopeType == "brand" {
			brandNames, _, err = s.scope.ScopeNames(ctx, access.ScopeIDs, nil)
		} else {
			_, storeNames, err = s.scope.ScopeNames(ctx, nil, access.ScopeIDs)
		}
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(access.ScopeIDs))
		for _, id := range access.ScopeIDs {
			if access.ScopeType == "brand" {
				names = append(names, brandNames[id])
				continue
			}
			names = append(names, storeNames[id])
		}
		return names, nil
	default:
		return nil, ErrScopeTypeInvalid
	}
}
