package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 本文件是扣款结果通知的入口：`POST /v1/payments/agreement-charge-notify/{channelCode}`。
//
// 它是第三条并排的渠道入口（支付结果 / 协议变更 / 扣款结果），与 agreement_notify.go 共用那
// 四步骨架，注释里只写这条路上特有的三处，其余不重复。
//
// # 这条路是「受理不等于扣到钱」那句话的落点
//
// 发起扣款时渠道只回一句「收下了」（见 model.ChargeStatusCharging）；钱到底动没动，只有这条
// 通知能说。所以这一期扣款的状态机有一半长在这里——**没有它，代扣就只是把请求发出去**。
//
// # 与老系统那条路的关系
//
// 老系统的续费回调按 out_trade_no 去查 user_subscriptions，而那个号是交易记录的 _id、从来没
// 写进那张表，于是必然查不到 → 回 FAIL → 微信永久重推（见迁移 013 的文件头）。这里对上的键
// 是 payment_agreement_charges.out_trade_no，它在这一期建行时就存下来了。

// ErrChargeNotifyUnsupported：这条渠道不实现 provider.AgreementChargeNotifier。
//
// 与 ErrAgreementNotifyUnsupported 同一个理由、分开一个值：它们说的是两条不同的路，而
// 「把一份协议变更通知投到扣款通知的地址上」与「这条渠道根本不推扣款结果」是两件事——混成
// 一个值会让排查的人去查渠道配置，而事实只是 notify_url 配错了。
var ErrChargeNotifyUnsupported = errors.New("this channel does not deliver charge result notifications")

// ChargeNotificationRequest 是一条扣款结果通知的原始输入。与另两条路同形，同样**不共用类型**
// （见 AgreementNotificationRequest 的注释：合成一个会让「这条路只处理扣款结果」在签名上消失）。
type ChargeNotificationRequest struct {
	ChannelCode string
	Body        []byte
	Headers     http.Header
	HTTPMethod  string
	RequestPath string
}

// ChargeNotificationResult 是一条扣款结果通知处理的结果，控制器拿它决定回给渠道什么。
type ChargeNotificationResult struct {
	NotificationID string
	OutTradeNo     string
	AgreementNo    string
	BizPeriod      string
	// EventType 是这条通知对应的事件类型（payment.agreement.charge_succeeded / charge_failed）。
	EventType string
	// Duplicate 表示这是重投，且上一次已经处理完了。**没有副作用**。
	Duplicate bool
	// Settled 表示这次真的推进了这一期、发了事件。
	Settled bool
}

// HandleAgreementChargeNotification 处理一条扣款结果通知。
//
// 四步与 protocol 那条路一致：找渠道 → 验签 → 落 payment_notifications → 一个事务里推状态 +
// 写 outbox。这条路上特有的：
//
//   - **金额必须对得上**（见 repository.ErrChargeAmountMismatch）。老系统完全没有这道闸：
//     它把报文里的数直接当成这一期的账。判据放在仓储的锁内，这里不重复判一次——两次判之间
//     的那一瞬正好是另一个通知改这一行的时候。
//   - **结果翻成状态是总函数**：渠道只可能说扣到了或没扣到（provider.AgreementChargeNotification
//     的 Result 只有两个取值），第三个取值是适配器的编码错误，这里报错而不是猜一个。
//   - **notification_id 同样是报文自身的 sha256**：微信的扣款通知里也没有通知号。同一个
//     说法与协议通知各用一处，但**两条路的报文不同、摘要也就不同**，所以它们不会在
//     payment_notifications 里互相撞——真撞上（报文一模一样）也只能是同一份报文被同时投到
//     两个地址，而那是 notify_url 配错的症状，不该被安静地吞掉。
func (s *PaymentService) HandleAgreementChargeNotification(ctx context.Context, in ChargeNotificationRequest) (*ChargeNotificationResult, error) {
	if strings.TrimSpace(in.ChannelCode) == "" {
		return nil, ErrChannelCodeRequired
	}
	if len(in.Body) == 0 {
		return nil, ErrNotificationEmpty
	}

	channel, err := s.catalog.Channel(in.ChannelCode)
	if err != nil {
		return nil, err
	}
	if channel == nil {
		return nil, fmt.Errorf("%w: %s", ErrChannelNotFound, in.ChannelCode)
	}
	adapter, err := s.providers.Lookup(channel.Provider)
	if err != nil {
		return nil, err
	}

	notifier, ok := adapter.(provider.AgreementChargeNotifier)
	if !ok {
		unsupported := fmt.Errorf("%w: %s", ErrChargeNotifyUnsupported, channel.Code)
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, unsupported)
		return nil, unsupported
	}

	method := methodFromChannel(channel)
	notification, verifyErr := notifier.VerifyAgreementChargeNotification(ctx, provider.NotificationRequest{
		ChannelCode: channel.Code,
		Body:        in.Body,
		Headers:     in.Headers,
		HTTPMethod:  in.HTTPMethod,
		RequestPath: in.RequestPath,
		Method:      method,
		// 槽与扣款那条路同源：验签的密钥就是签名那把（见 wechatpay.Provider.SecretSlots）。
		Secrets: resolveSecrets(s.resolveSecret, channel, adapter, method),
	})
	if verifyErr != nil {
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, verifyErr)
		return nil, verifyErr
	}

	target, err := chargeTarget(notification.Result)
	if err != nil {
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, err)
		return nil, err
	}
	eventType := chargeResultEventType(notification.Result)

	inserted, err := s.repository.InsertNotification(ctx, repository.NotificationParams{
		Provider:       channel.Provider,
		NotificationID: chargeNotificationID(in.Body),
		EventType:      eventType,
		// PaymentNo 留空：代扣不建 payments 行（见 model.PaymentAgreementCharge.PaymentNo）。
		// 定位这一行的键是 out_trade_no，它随报文进 payment_notifications.body。
		PaymentNo:         "",
		Body:              in.Body,
		BodySHA256:        bodySHA256(in.Body),
		Headers:           recordableHeaders(adapter, in.Headers),
		SignatureVerified: true,
		Status:            model.NotificationReceived,
	})
	if err != nil {
		return nil, err
	}
	if !inserted.Inserted {
		duplicate, handled, err := s.decideRedelivery(ctx, inserted, chargeNotificationID(in.Body),
			"out_trade_no", notification.OutTradeNo, "transaction_id", notification.ProviderTransactionID)
		if err != nil {
			return nil, err
		}
		if handled {
			return &ChargeNotificationResult{
				NotificationID: inserted.ID,
				OutTradeNo:     notification.OutTradeNo,
				EventType:      eventType,
				Duplicate:      duplicate,
			}, nil
		}
	}

	settlement, err := s.repository.SettleChargeNotification(ctx, repository.ChargeNotificationParams{
		NotificationID: inserted.ID,
		// 收到这条通知的那条渠道（URL 里那段），不是报文里说的任何东西。判据在仓储里读的是
		// **协议上的**渠道——扣款行自己没有 provider 列，而一份协议的扣款只可能来自它签约的
		// 那条渠道（见 repository.lockChargeForNotification）。
		Provider:              channel.Provider,
		OutTradeNo:            notification.OutTradeNo,
		ProviderTransactionID: notification.ProviderTransactionID,
		Amount:                notification.Amount,
		Target:                target,
		FailureCode:           notification.FailureCode,
		FailureMessage:        notification.FailureMessage,
		Reason:                chargeNotifyReason(notification),
		TraceID:               audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		// 业务事务已经回滚，这条通知记录还是 received。用**独立连接**标成 failed：那次回滚
		// 不该把「我们拒了这条通知」的留痕一起带走（同 HandleNotification）。
		//
		// 金额对不上也走这一条：它是**拒绝入账**，不是「以通知为准」。那两条报文说的不是同一笔
		// （渠道串了单，或者有人拿一条别的单的通知打到了这个地址上），两种都要人来看。
		if markErr := s.repository.MarkNotification(ctx, inserted.ID, model.NotificationFailed, err.Error()); markErr != nil {
			slog.ErrorContext(ctx, "failed to record a rejected charge notification",
				"notification_id", inserted.ID, "error", markErr)
		}
		slog.WarnContext(ctx, "refused a charge notification",
			"out_trade_no", notification.OutTradeNo,
			"transaction_id", notification.ProviderTransactionID,
			"amount", notification.Amount, "error", err)
		return nil, err
	}

	return &ChargeNotificationResult{
		NotificationID: inserted.ID,
		OutTradeNo:     settlement.Charge.OutTradeNo,
		AgreementNo:    settlement.Charge.AgreementNo,
		BizPeriod:      settlement.Charge.BizPeriod,
		EventType:      eventType,
		// Settled 为 false 是「这一期早就结过了、这次什么都没改」：重复通知，或者一条迟到的
		// 通知打在一期已经推进过的扣款上。回给渠道的仍然是成功应答——它确实不需要再投了。
		Settled: settlement.Changed,
	}, nil
}

// chargeNotificationID 给一条扣款通知合成 notification_id。
//
// 与协议通知同一条理由、同一个算法（报文自身的 sha256）：微信的扣款通知里也没有通知号，而
// 「同一个报文重推就是同一件事」正是 UNIQUE (provider, notification_id) 那道闸要的语义。
//
// 两条路各有一个函数而不是共用一个：它们是**两份契约**，今天算法恰好相同。哪天一方改了
// （比如渠道开始给通知号），改哪一处是明确的。
func chargeNotificationID(body []byte) string {
	return bodySHA256(body)
}

// chargeTarget 把渠道给的结论翻成本库那一期的目标状态。
//
// 总函数：适配器只可能给两个结果，第三个取值是它自己的编码错误（provider 那侧没有别的
// 取值），所以这里报错而不是猜——猜错的后果是一条本该被结掉的通知安静地过去，而这一期永远
// 停在 charging 上等人去查。
func chargeTarget(result provider.Result) (string, error) {
	switch result {
	case provider.ResultSuccess:
		return model.ChargeStatusSucceeded, nil
	case provider.ResultFailed:
		return model.ChargeStatusFailed, nil
	default:
		return "", fmt.Errorf("%w: unknown charge notification result %q", ErrProviderResultUncertain, result)
	}
}

// chargeResultEventType 把渠道的结论翻成事件类型（也就是 routing key）。
//
// 与 chargeTarget 分开两个函数，理由与 agreementStateEventType 那一对逐字相同：它们答的是
// 两个问题（「本地那一行改成什么」与「下游该收到哪条消息」），而它们是两份契约。
func chargeResultEventType(result provider.Result) string {
	if result == provider.ResultSuccess {
		return dto.EventAgreementChargeSucceeded
	}
	return dto.EventAgreementChargeFailed
}

// chargeNotifyReason 是写进状态流水的那句短话。排查时先看的是它。
func chargeNotifyReason(notification provider.AgreementChargeNotification) string {
	if notification.Result == provider.ResultSuccess {
		return "provider notified: the charge succeeded"
	}
	reason := "provider notified: the charge failed"
	if notification.FailureCode != "" {
		reason += " (" + notification.FailureCode + ")"
	}
	if notification.FailureMessage != "" {
		reason += " " + notification.FailureMessage
	}
	return reason
}
