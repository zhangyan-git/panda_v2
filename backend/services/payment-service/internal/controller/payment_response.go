package controller

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// CallbackPath 是渠道回调的前缀，它在 routes 里被拼成 {CallbackPath}/{channelCode}。
//
// 用 /v1/payments 这个**独立前缀**而不是挂在小程序那棵树下面：网关的 /v1/miniapp case 会
// 吃掉一切小程序路径，回调若挂在它下面就必须插在那条 case 之前，那是一个只能靠注释维持的
// 隐式顺序。独立前缀既不撞任何现有 case，也符合语义——回调来自渠道，不来自小程序。
const CallbackPath = "/v1/payments/callback"

// writeCallbackAck 把适配器给的应答原样写给渠道。
//
// **不套 api.Success/api.Error**：那套信封是我们自己的客户端协议（success/errorCode/
// errorMessage），渠道不认它，认它的是「HTTP 状态 + 我约定的那段体」。所以这一层不 import
// platform/api，出错也按渠道的约定答，而不是换成我们的错误信封。
//
// Status 为 0 时兜底成 200 而不是原样 WriteHeader：WriteHeader(0) 会 panic（不是合法状态
// 码），而一个在支付回调路径上 panic 的 handler 会把一条本来只是「答复写得不对」的问题变成
// 一条渠道收不到任何应答、只能反复重投的问题。适配器漏填 Status 是它的 bug，但不该用崩溃
// 来表达。
func writeCallbackAck(w http.ResponseWriter, ack provider.Ack) {
	if ack.Status == 0 {
		ack.Status = http.StatusOK
	}
	if ack.ContentType != "" {
		w.Header().Set("Content-Type", ack.ContentType)
	}
	w.WriteHeader(ack.Status)
	if len(ack.Body) > 0 {
		_, _ = w.Write(ack.Body)
	}
}

// channelCodeFromPath 从 `<前缀>/<渠道码>` 里取出渠道码。
//
// 回调与回跳两条路由共用它（见 callback.go 与 return.go）：两者的路径形状逐字相同
// （`/v1/payments/{callback,return}/{channelCode}`），差异只在方法与前缀。**前缀由调用方
// 传进来**而不是在这里 if 一下：这个函数的职责是切路径，多一个「猜这是哪棵树」的分支就等于
// 把注册表抄了第二份。
//
// 手写前缀裁剪而不是取 gorilla/mux 的路由变量：路径模板写在 routes 里只是为了注册与
// 指标里的路径标签好看，真正被解析的是 r.URL.Path。两处共用同一个常量（CallbackPath /
// ReturnPath），改了前缀两边一起改。
//
// 多一段（`/callback/a/b`）一律当作不匹配回 404：渠道码是一段，多出来的那一段不该被当成
// 它的一部分去查库——那只会让「有人拼错了 URL」看起来像「这个渠道没配置」。
func channelCodeFromPath(path, prefix string) (string, bool) {
	rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}
