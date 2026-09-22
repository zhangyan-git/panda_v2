package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
)

// RegisterMerchant mounts the merchant console's own routes.
//
// 与 Register 分开，是因为这条链路里有一样东西是后台那条完全没有的：数据范围。
// 后台的每条路由先过权限码再过数据，商户域**没有权限码**（009 删掉商户角色表之后，
// 商户账号只有范围、没有角色），所以这里的判定只剩一件事——把这个账号的授权点位
// 集合展开、放到 context 上，剩下的由查询去过滤。加一个 RequirePermission 进来
// 是无意义的：没有任何权限码可以授予它。
//
// 中间件顺序：auth.Middleware（验令牌）→ authorizer.middleware（实时取范围）。
// 没有单独的 realm 闸门：MerchantMiddleware 在发起那次 gRPC 之前就先看 realm 与
// tenant，非商户 realm 与空 tenant 都在本地就被挡成 403（见 platform/authz/merchant.go
// 的第 3、4 步），再加一层只是把同一条规则写两遍。
func RegisterMerchant(s *runtime.HTTPRouter, st *MerchantStoreHandler, jwtService *auth.Service, authorizer *MerchantAuthorizer) {
	authenticated := auth.Middleware(jwtService)
	// 只读：这里注册的每一个都必须是 GET。商户端的写能力不在本轮范围内。
	protected := func(h http.HandlerFunc) http.HandlerFunc {
		return authenticated(authorizer.middleware(h)).ServeHTTP
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

	s.HandleFunc("/v1/merchant/stores", method(http.MethodGet, protected(st.List)))
	s.HandleFunc("/v1/merchant/stores/{id}", method(http.MethodGet, protected(st.Get)))
}
