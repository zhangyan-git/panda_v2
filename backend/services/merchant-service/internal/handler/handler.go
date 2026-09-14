package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
)

func adminIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromRequest(r)
		if !ok || identity.Tenant != "" {
			api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Register mounts the live-authorized admin JWT routes.
//
// Service-to-service traffic no longer appears here: the internal merchant and
// ownership lookups moved to gRPC (see internal/rpc) and authenticate with the
// shared service token instead of the retired X-Service-Token header.
// A nil authorizer fails closed; signed authorization snapshots never grant access.
func Register(s *runtime.HTTPRouter, m *AdminMerchantHandler, b *AdminBrandHandler, st *AdminStoreHandler, up *AdminUploadHandler, jwtService *auth.Service, authorizer *AdminAuthorizer) {
	authenticated := auth.Middleware(jwtService)
	protected := func(permission string, h http.HandlerFunc) http.HandlerFunc {
		return authenticated(adminIdentity(authorizer.middleware(auth.RequirePermission(permission)(h)))).ServeHTTP
	}
	// protectedAny 与 protected 相同，只是持有任意一个权限码即可通过。
	//
	// 上传接口用它复用既有的两个 manage 码，而不是新造一个 upload 码：新增权限码
	// 要跟着迁移身份库、并给现有角色补授权，漏一步就会让非超管用户的上传按钮
	// 直接失效（只有 super_admin 还能用）。同时它也不是裸认证——只读管理员拿不到
	// 往公开 bucket 写文件的能力，而能合法贴图的人本来就持有这两个码之一。
	// 链路仍然经过 authorizer.middleware，被停用或回收权限的人实时被挡。
	protectedAny := func(permissions []string, h http.HandlerFunc) http.HandlerFunc {
		return authenticated(adminIdentity(authorizer.middleware(auth.RequirePermission(permissions...)(h)))).ServeHTTP
	}
	method := func(method string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != method {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	methods := func(handlers map[string]http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h, ok := handlers[r.Method]
			if !ok {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}

	s.HandleFunc("/v1/admin/merchants", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:merchants:view", m.List), http.MethodPost: protected("admin:merchants:manage", m.Create)}))
	s.HandleFunc("/v1/admin/merchants/{id}", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:merchants:view", m.Get), http.MethodPut: protected("admin:merchants:manage", m.Update), http.MethodDelete: protected("admin:merchants:delete", m.Delete)}))
	s.HandleFunc("/v1/admin/merchants/{id}/status", protected("admin:merchants:manage", method(http.MethodPatch, m.UpdateStatus)))
	s.HandleFunc("/v1/admin/brands", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:brands:view", b.List), http.MethodPost: protected("admin:brands:manage", b.Create)}))
	s.HandleFunc("/v1/admin/brands/{id}", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:brands:view", b.Get), http.MethodPut: protected("admin:brands:manage", b.Update), http.MethodDelete: protected("admin:brands:delete", b.Delete)}))
	s.HandleFunc("/v1/admin/brands/{id}/status", protected("admin:brands:manage", method(http.MethodPatch, b.UpdateStatus)))
	s.HandleFunc("/v1/admin/brands/{id}/audit", protected("admin:brands:manage", method(http.MethodPatch, b.Audit)))
	s.HandleFunc("/v1/admin/stores", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:stores:view", st.List), http.MethodPost: protected("admin:stores:manage", st.Create)}))
	s.HandleFunc("/v1/admin/stores/{id}", methods(map[string]http.HandlerFunc{http.MethodGet: protected("admin:stores:view", st.Get), http.MethodPut: protected("admin:stores:manage", st.Update), http.MethodDelete: protected("admin:stores:delete", st.Delete)}))
	s.HandleFunc("/v1/admin/stores/{id}/status", protected("admin:stores:manage", method(http.MethodPatch, st.UpdateStatus)))
	s.HandleFunc("/v1/admin/stores/{id}/audit", protected("admin:stores:manage", method(http.MethodPatch, st.Audit)))
	// 上传服务品牌 logo/banner 与门店 logo/照片，所以这两个 manage 码都算数。
	// 上传器没配好时路由依然在，返回 503 并点名缺哪个变量。
	s.HandleFunc("/v1/admin/uploads/images", protectedAny([]string{"admin:brands:manage", "admin:stores:manage"}, method(http.MethodPost, up.UploadImage)))
}
