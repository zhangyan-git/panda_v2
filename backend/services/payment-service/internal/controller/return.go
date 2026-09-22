package controller

import (
	"log/slog"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// ReturnPath 是结果页回跳的前缀，它在 routes 里被拼成 {ReturnPath}/{channelCode}。
//
// 与 CallbackPath 并排放在 /v1/payments 这个独立前缀下面，同一条理由：这条路来自渠道
// （浏览器是被渠道送回来的），不来自我们自己的客户端，所以它既不该挂在那几棵客户端树下面、
// 也不该套认证中间件。
const ReturnPath = "/v1/payments/return"

// ReturnController 接收结果页回跳（H5 支付完成后用户在浏览器里被送回来的那次 GET）。
//
// 它与 CallbackController 是**两个类型**，不是一个类型上的两个方法：两者对「能不能改支付
// 状态」这个问题的答案正好相反（一个专门改、一个结构上改不了），而那个差别是这一层的读者最
// 需要一眼看到的东西。共用一个类型会让「这个 handler 会不会动钱」变成要去读方法体才知道的
// 事。
type ReturnController struct{ payments *service.PaymentService }

func NewReturnController(payments *service.PaymentService) *ReturnController {
	return &ReturnController{payments: payments}
}

// Return 处理 GET {ReturnPath}/{channelCode}。
//
// # 它与回调最要紧的两处差别
//
//   - **应答是给浏览器看的，不是给渠道看的。** 回调那条路的应答形状来自适配器（provider.Ack，
//     渠道要认它才会停止重投）；这条路的观众是人：验签过了就把他送到结果页，没过就告诉他
//     「这个链接不对」。所以这里不走 writeCallbackAck，也不该走。
//   - **失败不落库**（见 service.HandleReturn）。所以这一层连「记一条拒绝」都没有，只有日志。
//
// # 一处安全上的要点：302 的目标不是请求里的值
//
// http.Redirect 在这里是安全的，因为它跳去的地址来自**部署配置**
// （service.Options.ReturnPageURL），不是查询串里的任何东西。若哪天有人把它改成「跳回
// 渠道传过来的某个 url 参数」，这里就是一个开放重定向（拿我们的域名把用户送去任何地方）。
// 那句话写在这里，是因为那正是这类改动最常见的样子——一个「更方便」的参数。
func (c *ReturnController) Return(w http.ResponseWriter, r *http.Request) {
	// 只接受 GET。回跳是浏览器被渠道送回来时发起的导航，它就是 GET；POST 上来的一定不是
	// 这条路上的东西。回 405 而不是 404，理由同回调那边：地址存在、动词不对，排查的人要能
	// 分清这两件事。
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelCode, ok := channelCodeFromPath(r.URL.Path, ReturnPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	result, err := c.payments.HandleReturn(r.Context(), service.ReturnRequest{
		ChannelCode: channelCode,
		// r.URL.Query() 会解析并解码，但**解码只影响我们读到的值**：验签那一步用的是
		// 适配器按同样规则归一化之后的参数表（它自己再拼回待签串），不是这里的 map 顺序。
		// 传递顺序无所谓——map 本来就没有顺序，验签那边按 ASCII 排序（见 ums/keyappend.go）。
		Query:       r.URL.Query(),
		Headers:     r.Header,
		RequestPath: r.URL.Path,
	})
	if err != nil {
		// 日志里带错误、应答里一个字都不带：这条路的观众是浏览器，把验签失败的具体原因
		// （哪个参数没对、密钥槽叫什么）渲染给他看，等于把内部结构递给一个正在试签名的人。
		slog.WarnContext(r.Context(), "refused a result-page return",
			"channel", channelCode, "error", err)
		writeReturnRefused(w)
		return
	}

	slog.InfoContext(r.Context(), "handled a result-page return",
		"channel", channelCode, "payment_no", result.PaymentNo,
		"redirect", result.RedirectURL != "")

	if result.RedirectURL != "" {
		// 302 而不是 301：结果页地址是**部署配置**，改它是运维的常规动作，而 301 会被浏览器
		// 永久缓存——一次改配置之后，之前来过的用户再也回不到新地址，且没有任何服务端日志
		// 能看出来（浏览器根本不问我们了）。
		http.Redirect(w, r, result.RedirectURL, http.StatusFound)
		return
	}

	// 没配结果页（见 service.Options.ReturnPageURL）：真验过签了，只是没地方可送。回 200 +
	// 一句人话，而不是 404 或者空体——用户此刻刚付完钱，看到一个「找不到页面」会以为钱丢了。
	//
	// 文案只说我们**确实知道**的事：我们收到了渠道的回跳，它验过签。**不说「支付成功」**——
	// 那句话的依据是回调或查单，不是这次跳转（规范原文 §3）。真要写一句让人安心的话，正确的
	// 那句是「以订单页显示为准」。
	writeReturnPage(w)
}

// writeReturnRefused 是回跳验签没过时的应答。
//
// 400 而不是 403：我们没有身份可以「拒绝」谁，这是一条**参数不对**的请求（缺签名、签名对
// 不上、或者干脆没带参数）。文案是给误入的用户看的，所以他需要知道的是「这个地址必须由渠道
// 带你回来」，而不是「签名错误」。
func writeReturnRefused(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte("这个页面需要从支付渠道跳转进入，请返回小程序查看订单结果。\n"))
}

// writeReturnPage 是没有配置结果页时的兜底页面。
func writeReturnPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("已收到支付渠道的回跳，请返回小程序查看订单结果。\n"))
}
