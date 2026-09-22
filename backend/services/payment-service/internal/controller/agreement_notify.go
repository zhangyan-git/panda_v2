package controller

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// AgreementNotifyPath 是签约通知的前缀，它在 routes 里被拼成 {AgreementNotifyPath}/{channelCode}。
//
// 它**不挂在 CallbackPath 下面**（那样也能跑，路径形状完全一样），而是并排的第三段：两条路
// 收的是两种报文、落的是两张表，共用前缀会让「这个渠道的回调地址是什么」在配置上变成一个
// 需要看后缀才能说清的问题。微信那边 notify_url 是按用途分别配的（签约用签约的地址、扣款用
// 扣款那一笔自己的地址），一个前缀装不下。
const AgreementNotifyPath = "/v1/payments/agreement-notify"

// AgreementNotifyController 接收协议变更通知（签约 / 解约）。
//
// 与 CallbackController 分成两个类型而不是一个控制器上的两个方法：它们各自持的是同一个
// *service.PaymentService，但**注册的路由与应答形状可以不同**——今天两者恰好一样（都回
// provider.Ack），那是巧合而不是约束。分成两个之后，将来的分叉只改一条路。
type AgreementNotifyController struct{ payments *service.PaymentService }

func NewAgreementNotifyController(payments *service.PaymentService) *AgreementNotifyController {
	return &AgreementNotifyController{payments: payments}
}

// Notify 处理 POST {AgreementNotifyPath}/{channelCode}。
//
// 与 Callback.Callback 逐字同构（读原始报文 → 交给 service → 按渠道的约定答回去），注释里
// 只写这条路上特有的一处：**应答形状问的还是 AckFor**。协议变更通知与支付结果通知在微信
// 那边是同一个 return_code 协议，而「怎么答这家渠道」只有适配器知道——这里再写一份判断
// 就是第二处真相。见 service.AckFor 的注释。
func (c *AgreementNotifyController) Notify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelCode, ok := channelCodeFromPath(r.URL.Path, AgreementNotifyPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		slog.WarnContext(r.Context(), "cannot read an agreement notification body",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	result, err := c.payments.HandleAgreementNotification(r.Context(), service.AgreementNotificationRequest{
		ChannelCode: channelCode,
		Body:        body,
		Headers:     r.Header,
		HTTPMethod:  r.Method,
		RequestPath: r.URL.Path,
	})
	if err != nil {
		// 与支付回调那条路同一条：应答体里一个字都不放，原因只进日志。验签不过、渠道对不上、
		// 查无此约都走这一条，它们的共同含义只有一个——这条通知我们还没认下来。
		slog.WarnContext(r.Context(), "refused an agreement notification",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	// 走到这里，协议已经改完、事件已经写进 outbox 了。答不出去也没关系：微信会重投，重投
	// 撞上 payment_notifications 的唯一键，走 decideRedelivery 那条路拿到成功应答，协议
	// 一个字段都不再动（第二次的 applyAgreementTarget 也只会说「没改」）。
	slog.InfoContext(r.Context(), "handled an agreement notification",
		"channel", channelCode,
		"agreement_no", result.AgreementNo,
		"contract_no", result.ContractNo,
		"event_type", result.EventType,
		"duplicate", result.Duplicate,
		"settled", result.Settled)
	writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, true))
}
