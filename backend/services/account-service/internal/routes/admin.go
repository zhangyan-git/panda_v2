package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/controller"
)

// RegisterAdmin 挂载后台的资产账户路由：福卡一棵树、咖啡豆一棵树。认证由调用方套上
// （cmd/main.go 的 adminAuthorizer），这样「忘了装配」不会变成 fail-open——这里没有可选的
// 认证。
//
// 注册顺序按「从长到短」：gorilla/mux 取第一个匹配的路径，/entries 或 /freezes 注册在
// /{userId} 之后就会被永远解析成 userId="entries"。这一棵树路径不多，但顺序反了的后果不是
// 404 而是一个看起来像 UUID 校验失败的 400，所以顺序这件事在这里同样写明白。
//
// 福卡三条都只读，共用同一个权限码 account:read——「能看福卡账户」「能看福卡流水」「能看退款
// 冻结」是客服的同一件事，拆成几个码只会让人配错。咖啡豆树上的读用同一个 account:read（客服
// 看余额和看流水还是同一件事），**写**单独一个 account:manage：人工调整余额是全系统唯一能凭空
// 改动豆余额的入口，它不该跟着「看」一起被发出去。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	cards *controller.AdminFortuneCardController,
	beans *controller.AdminCoffeeBeanController,
	authenticate func(...string) func(http.Handler) http.Handler,
) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
	})
	protect := func(permission string, next http.Handler) http.Handler {
		if authenticate == nil {
			return unauthorized
		}
		return authenticate(permission)(next)
	}

	readCards := protect("account:read", http.HandlerFunc(cards.Cards))
	r.HandleFunc("/v1/admin/fortune-cards/entries", readCards.ServeHTTP)
	r.HandleFunc("/v1/admin/fortune-cards/freezes", readCards.ServeHTTP)
	r.HandleFunc("/v1/admin/fortune-cards/{userId}", readCards.ServeHTTP)

	readBeans := protect("account:read", http.HandlerFunc(beans.Beans))
	adjustBeans := protect("account:manage", http.HandlerFunc(beans.Beans))
	// 从长到短：entries 与 {userId}/adjustments 都必须排在 {userId} 之前。
	r.HandleFunc("/v1/admin/coffee-beans/entries", readBeans.ServeHTTP)
	r.HandleFunc("/v1/admin/coffee-beans/{userId}/adjustments", adjustBeans.ServeHTTP)
	r.HandleFunc("/v1/admin/coffee-beans/{userId}", readBeans.ServeHTTP)
}
