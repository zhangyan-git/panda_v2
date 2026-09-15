package controller

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// maxCallbackBody 是回调报文的大小上限。
//
// 定长上限在这里是必须的，因为这个接口**没有认证**——它靠验签，而验签要看完整报文，所以
// 没法先限流再读。不设限就等于让任何人在内存里写任意大的东西。64 KiB 留了很大余量：真实
// 渠道的通知报文是几 KB（微信 v3 那条最大的也只是把 resource 加密后折成 base64），没有
// 一个正常的成交通知会接近它。
const maxCallbackBody = 64 << 10

// CallbackController 接收渠道回调。它是本服务唯一的 HTTP 入口（见包注释）。
type CallbackController struct{ payments *service.PaymentService }

func NewCallbackController(payments *service.PaymentService) *CallbackController {
	return &CallbackController{payments: payments}
}

// Callback 处理 POST {CallbackPath}/{channelCode}。
//
// 这一层薄到几乎没什么可写的，只有两件它必须自己做的事：把**原始报文**读进来（任何在中间
// 做的规范化都会让验签对不上，而那种失败看起来像「渠道的签名算错了」），以及按渠道的约定把
// 结论答回去。至于「这条回调算不算数」全部在 service 里判——两条路各写一份规则是「同一个
// 动作在不同入口行为不同」的来源。
func (c *CallbackController) Callback(w http.ResponseWriter, r *http.Request) {
	// 只接受 POST。回 405 而不是 404：这个 URL 确实存在、只是方法不对，排查的人需要分清
	// 「我打错地址了」和「我用了错的动词」。
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelCode, ok := callbackChannelCode(r.URL.Path)
	if !ok {
		// 路径形状不对，连这是哪个渠道都认不出来，也就没有任何适配器能告诉我们该怎么答。
		// 这一条只能回一个与渠道无关的 404。
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		// 读不动：要么超了上限，要么连接断了。按「我们没收下」答，让渠道重投。
		slog.WarnContext(r.Context(), "cannot read a payment callback body",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	result, err := c.payments.HandleNotification(r.Context(), service.CallbackRequest{
		ChannelCode: channelCode,
		Body:        body,
		Headers:     r.Header,
	})
	if err != nil {
		// 应答体里一个字都不放，只在日志里带上错误（见 provider.Ack 的注释：把内部原因
		// 回给渠道既没用又是信息泄漏）。写库失败、验签不过、金额对不上都走这一条，
		// 它们的共同含义只有一个：这条通知我们还没认下来。
		slog.WarnContext(r.Context(), "refused a payment callback",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	// 走到这里，支付状态已经改完、事件已经写进 outbox 了。答不出去也没关系：渠道会重投，
	// 重投撞上 payment_notifications 的唯一键，走 handleRedelivery 那条路拿到 success 应答，
	// 状态一个字段都不再动。答出去之前就崩溃同理——那正是这套「先落库、再应答」的顺序
	// 要买到的东西。
	slog.InfoContext(r.Context(), "handled a payment callback",
		"channel", channelCode,
		"payment_no", result.PaymentNo,
		"event_type", result.EventType,
		"duplicate", result.Duplicate,
		"settled", result.Settled)
	writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, true))
}
