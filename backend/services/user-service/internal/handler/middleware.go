package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// RoleFinder 是鉴权需要的角色来源：库里的实时绑定，不是 token 里的角色快照。
//
// 超管判定直查绑定表而不是问 Casbin：Enforce 只能回答「有没有某个权限码」，而超管的
// 定义是「持有 super_admin 角色」——它绕过整个权限模型，与权限绑定表无关。按权限判
// 会让超管少拿到没绑上的那些码（dev 库实测差 22 条），而且 Casbin 手里的还是进程内
// 快照，撤销角色要等下一次重载；绑定表则是每请求直查，没有窗口期。
type RoleFinder interface {
	FindRolesByUser(ctx context.Context, userID string) ([]*model.AdminRole, error)
}

// PolicyEnforcer 是非超管的策略判定入口。生产实现是 casbin.Enforcer，策略由角色-权限
// 关系表派生（见 internal/casbin）；收窄成接口是为了让这个中间件不依赖具体策略存储。
type PolicyEnforcer interface {
	Enforce(sub, dom, obj, act string) (bool, error)
}

// AccountFinder 是账号状态的实时来源。停用账号不该继续享受授权：access token 的
// TTL 是 24 小时，而「停用」是即时的管理动作，token 里没有任何东西能反映它。
// 判定规则与 GetAdminAccess（gRPC）和 GET /v1/admin/users/me 保持一致——
// 那两处本来就在查状态，只有这条中间件漏了。
type AccountFinder interface {
	FindByID(ctx context.Context, id string) (*model.AdminUser, error)
}

// RequirePermission returns a middleware that enforces the policy.
// code is the permission code stored in admin_permissions (e.g. "admin:roles:view").
// 派生出来的规则 act 恒为 "*"（授权以权限码整体给出），所以这里也传 "*"。
//
// 超管判定同样读库：token 里签发的角色在过期前不会变，用它就意味着「把某人
// 踢出超管」要等最长 24 小时才生效。
func RequirePermission(enforcer PolicyEnforcer, roles RoleFinder, accounts AccountFinder, code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := auth.IdentityFromRequest(r)
			if !ok || identity.UserID == "" {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
				return
			}
			// 这条中间件只挂在平台接口上，所以必须先挡掉非平台身份：C 端 token
			// 的 UserID 同样非空，再往下走只会拿一个 C 端 id 去 admin_users 里查。
			if !identity.IsPlatformAdmin() {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "非平台管理员")
				return
			}
			// 账号状态先查，且必须在超管短路之前：停用的超管同样没有权限。
			account, err := accounts.FindByID(r.Context(), identity.UserID)
			if errors.Is(err, pgx.ErrNoRows) {
				// 身份可信但账号没了（被删）——当作未登录，与 Me 一致。
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "账号不存在")
				return
			}
			if err != nil || account == nil || account.ID != identity.UserID {
				// 答非所问或查不出来都失败关闭：不退回 token 里的角色/权限快照。
				api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "账号服务暂不可用")
				return
			}
			if account.Status != "active" {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "账号已禁用")
				return
			}
			super, err := liveSuper(r.Context(), roles, identity.UserID)
			if err != nil {
				// 查不到角色就什么都判不了：不当作普通用户（那会让超管莫名 403），
				// 也不放行——失败关闭。
				api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "权限服务暂不可用")
				return
			}
			if super {
				next.ServeHTTP(w, r)
				return
			}
			allowed, err := enforcer.Enforce(identity.UserID, identity.Tenant, code, "*")
			if err != nil || !allowed {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "没有权限执行此操作")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// liveSuper 判断该用户当前是否仍持有超级管理员角色。兼容 super_admin 与
// 「超级管理员」两个写法：后者是早期数据里出现过的角色码。
func liveSuper(ctx context.Context, roles RoleFinder, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	granted, err := roles.FindRolesByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, role := range granted {
		if role != nil && model.IsSuperRoleCode(role.Code) {
			return true, nil
		}
	}
	return false, nil
}
