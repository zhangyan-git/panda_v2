package routes

import (
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
)

// RegisterPayment 挂载支付服务的 HTTP 路由。
//
// 一条路由，而且**不套任何中间件**——这是本仓库里唯一一条这样的入口，理由见包注释：
// 渠道回调的凭据是签名，不是我们的令牌，验签在 service 里做。
//
// 路径模板里的 {channelCode} 是给注册与指标用的（运行时中间件从路径模板读 operation 与
// path 标签，不读 r.URL.Path），真正被解析的是控制器的前缀裁剪。两处共用同一个常量，
// 改了前缀不会一边改一边忘。
func RegisterPayment(r *runtime.HTTPRouter, callback *controller.CallbackController) {
	r.HandleFunc(controller.CallbackPath+"/{channelCode}", callback.Callback)
}
