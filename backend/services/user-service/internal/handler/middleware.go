package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	casbinpkg "github.com/panda-dev/panda-v2/backend/services/user-service/internal/casbin"
)

// RequirePermission returns a middleware that enforces Casbin policy.
// code is the permission code stored in admin_permissions (e.g. "admin:roles:view").
// The policy row in casbin_rule has act="*", so we pass "*" as the act here.
func RequirePermission(enforcer *casbinpkg.Enforcer, code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := auth.IdentityFromRequest(r)
			if !ok {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
				return
			}
			// 超级管理员直接放行：兼容新旧 token 中的 super_admin 角色标识
			if isSuperAdmin(identity) {
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

func isSuperAdmin(identity auth.Identity) bool {
	if identity.IsSuper {
		return true
	}
	for _, role := range identity.Roles {
		if role == "super_admin" || role == "超级管理员" {
			return true
		}
	}
	return false
}
