package controller

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// AgreementChargeNotifyPath 是扣款结果通知的前缀，它在 routes 里被拼成
// {AgreementChargeNotifyPath}/{channelCode}。
//
// 与 AgreementNotifyPath 是并排的两段而不是一段加后缀：微信那边这两条通知的 notify_url 是
// **分别配在两次不同的调用上的**（签约配签约的地址、扣款配这一笔自己的地址），所以它们本来
// 就是两个地址。而这里最贵的一次事故正是这两条撞在一起——老系统的续费扣款与签约回调共用一条
// 路由，回调按 out_trade_no 去查订阅表、那个号从来没写进那张表，于是永久重推（见迁移 013）。
const AgreementChargeNotifyPath = "/v1/payments/agreement-charge-notify"

// AgreementChargeNotifyController 接收扣款结果通知。
//
// 与另两个控制器分成三个类型而不是一个类型上的三个方法：它们各自持的是同一个
// *service.PaymentService，而**注册的路由与应答形状可以不同**——今天三者恰好一样（都回
// provider.Ack），那是巧合而不是约束（见 AgreementNotifyController 的注释）。
type AgreementChargeNotifyController struct{ payments *service.PaymentService }

func NewAgreementChargeNotifyController(payments *service.PaymentService) *AgreementChargeNotifyController {
	return &AgreementChargeNotifyController{payments: payments}
}

// Notify 处理 POST {AgreementChargeNotifyPath}/{channelCode}。
//
// 与另两条路逐字同构（读原始报文 → 交给 service → 按渠道的约定答回去）。应答形状问的还是
// AckFor：这条通知与签约通知在微信那边是同一个 return_code 协议，而「怎么答这家渠道」只有
// 适配器知道（见 service.AckFor 的注释）。
func (c *AgreementChargeNotifyController) Notify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelCode, ok := channelCodeFromPath(r.URL.Path, AgreementChargeNotifyPath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCallbackBody))
	if err != nil {
		slog.WarnContext(r.Context(), "cannot read a charge notification body",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	result, err := c.payments.HandleAgreementChargeNotification(r.Context(), service.ChargeNotificationRequest{
		ChannelCode: channelCode,
		Body:        body,
		Headers:     r.Header,
		HTTPMethod:  r.Method,
		RequestPath: r.URL.Path,
	})
	if err != nil {
		// 与另两条路同一条：应答体里一个字都不放，原因只进日志。验签不过、渠道对不上、查无
		// 此期、**金额对不上**都走这一条，它们的共同含义只有一个——这条通知我们还没认下来。
		//
		// 金额对不上时回失败应答是**有意**的：渠道会重投，而重投还是对不上（我们记的数没变），
		// 直到有人去查那两条报文为什么说的不是同一笔。宁可让它一直重投到有人看见，也不要一次
		// 「收下了」把一条对不上的账安静地记进去。
		slog.WarnContext(r.Context(), "refused a charge notification",
			"channel", channelCode, "error", err)
		writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, false))
		return
	}

	// 走到这里，那一期已经推完、事件已经写进 outbox 了。答不出去也没关系：微信会重投，重投
	// 撞上 payment_notifications 的唯一键，走 decideRedelivery 那条路拿到成功应答，那一期一个
	// 字段都不再动（第二次的 applyChargeTarget 也只会说「没改」）。
	slog.InfoContext(r.Context(), "handled a charge notification",
		"channel", channelCode,
		"out_trade_no", result.OutTradeNo,
		"agreement_no", result.AgreementNo,
		"biz_period", result.BizPeriod,
		"event_type", result.EventType,
		"duplicate", result.Duplicate,
		"settled", result.Settled)
	writeCallbackAck(w, c.payments.AckFor(r.Context(), channelCode, true))
}
