package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
)

// RegisterOpenAPI 挂载开放接口树（/v1/openapi/*）。
//
// # 这一棵树故意不挂我们的认证
//
// 合作方没有我们的令牌，它带的是自己算的签名（X-API-Key + X-Signature）。「认证」在这条路上
// 的等价物是验签，而验签、时间窗、nonce 去重、启停、过期、白名单、限流七道全在
// ingress.Guard 里（见那边的包注释）。挂 auth.Middleware 只会让每一次调用都 401，而且是
// **安静地**失败：合作方看到的是「对面不收」，我们看到的是一条永远没被处理的请求。
// 与 payment-service 的回调树是同一条理由。
//
// # guard 为 nil 时全 401
//
// 与 admin.go 的 protect 同一个约定：**「忘了装配」必须表现为「谁都进不去」**，不是
// 「谁都能进」。那一条尤其重要：少了 Guard 的开放接口是**匿名**的——任何人都能查任何人的
// 会员权益，而接口本身会一切正常地返回 200。设备刷卡回执（写的那一条）在这件事上更重：
// 少了 Guard，匿名请求会**直接建出一张已支付的订单**，而钱并没有在机器上收过。
//
// guard 为 nil 时回的那句话与 ingress 的 unauthorizedBody 逐字一致（见 middleware.go 里
// 那个常量）：同一个接口上出现两种 401 的报文形状会让对接方以为撞上了两个系统。**改那里
// 的常量时要顺手改这一行**——两份写法是这个函数与那个常量之间唯一的耦合，而它没有测试
// 兜底（guard 为 nil 是装配错误，正常路径上走不到）。
func RegisterOpenAPI(r *runtime.HTTPRouter, openapi *controller.OpenAPIController, guard *ingress.Guard) {
	var protect func(http.Handler) http.Handler
	if guard == nil {
		protect = func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
			})
		}
	} else {
		protect = guard.Handler
	}

	// 一条路径，一个 handler。**写完整路径而不是前缀**：这一棵树上没有可以按前缀分发的子
	// 资源（今天三条），而写成前缀会让「网关把一条我们没实现的路径也转过来了」这种情况在这里
	// 静默地变成一个 handler 身上的 404/405，而不是一句明确的「这条路径不存在」。
	//
	// # 三条路径，两种性质
	//
	//   GET  /v1/openapi/member-price-entitlement  查询（只读的转发）
	//   POST /v1/openapi/device/sync-order         设备刷卡回执（**写**：钱已经在机器上收过了）
	//   POST /v1/openapi/device/pickup             取货码（**写**：钱从这台设备的余额里扣）
	//
	// 后两行是本服务对合作方的写能力（线下刷卡机与取货码，方案 §四）。它们都不是「顺手加的
	// 一条开放接口」：刷卡那条的钱已经在机器上收过了，报文进来就是一笔既成事实；取货码那条
	// 更重一层——那是**我们自己账上的钱**，一条没验过签的报文会实打实地扣掉一台设备的余额。
	// 所以加一条写路径之前要先问一遍「这条报文会不会在别域落一行我们既复制不了、也撤不掉的
	// 数据」——今天这批开放接口里只有这两条答得上来。
	//
	// 动词**不在这里绑**（.Methods() 能做到，但这里不绑）：本服务今天这一棵树上没有任何一条
	// 需要按方法分发到不同 handler 的路径，而动词落在各自的 handler 里判，能给出与其他响应
	// 同一套信封的答复（Entitlement 回 404、CreateDeviceOrder 回 405——判据见 controller 里
	// 那两处的说明）。交给路由层判的话，被挡下来的请求拿到的是路由自己的 405（不是本服务的
	// JSON 信封），而这一棵树的每一条响应都必须长成同一个信封——对接方会把它当成两个系统。
	r.HandleFunc("/v1/openapi/member-price-entitlement",
		protect(http.HandlerFunc(openapi.Entitlement)).ServeHTTP)
	// ⚠️ 这个串必须与 controller.openAPIDeviceOrderPath、compose 里的说明、以及对接文档逐字
	// 一致。网关认的是前缀（/v1/openapi），所以这里写错了它照样转得过来——表现是「网关没问题、
	// 这里 404」，而两边都不报错。
	r.HandleFunc("/v1/openapi/device/sync-order",
		protect(http.HandlerFunc(openapi.CreateDeviceOrder)).ServeHTTP)
	// ⚠️ 同上，这个串必须与 controller.openAPIDevicePickupPath、compose 里的说明、以及对接
	// 文档逐字一致（/v1/openapi/device/pickup）。它与上一行的路径**互为前缀关系之外的第二个
	// 陷阱**：两条路径都以 /v1/openapi/device/ 开头，把 pickup 拼成 pickup-code 之类的错法
	// 与把 sync-order 拼错是同一种表现（网关转得过来、这里 404），而这里多一层危险——它看起来
	// 像「同一条接口的另一个版本」。它们不是。
	r.HandleFunc("/v1/openapi/device/pickup",
		protect(http.HandlerFunc(openapi.CreatePickupOrder)).ServeHTTP)
}
