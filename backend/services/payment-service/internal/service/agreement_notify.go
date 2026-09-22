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

// 本文件是签约通知的入口：`POST /v1/payments/agreement-notify/{channelCode}`。
//
// 它与 callback.go 那条路（支付结果通知）是**并排的两条**，不是一条路上的两个分支：报文形状
// 不同（没有金额、没有支付单号）、判据不同（钱的事宁可拒也不猜；协议的事只判「哪份协议变成
// 什么状态」）、落库的目标也不同（协议 vs 支付单）。共用一条入口的代价是每一次改动都要同时
// 想清楚另一件事该怎么处理，而它们之间没有一处是共享的判断。
//
// 共用的是**四步骨架**，那四步一条都不能少：
//
//  1. 在目录里找那条渠道（认 URL 里那段，不认报文里说的任何东西）
//  2. 验签 —— 在碰报文里的任何字段之前
//  3. 落 payment_notifications（这条路上 notification_id 是**报文自身的 sha256**，见
//     agreementNotificationID）
//  4. 一个事务里改协议 + 写状态流水 + 写 outbox
//
// 验签失败、渠道对不上、查无此约这三条路径上**协议一个字段都不动**，只是把这条通知记成
// failed。这是「伪造通知不会把会员续费改乱」的全部依据。

// ErrAgreementNotifyUnsupported：这条渠道不实现 provider.AgreementNotifier。
//
// 它说的是「这条渠道没有签约通知这条路」，与「报文不对」是两件事：把一份银联商务的支付报文
// POST 到签约通知的地址上，得到的应该是这句话，而不是一句「验签失败」——那会让人去查密钥。
// 与 ErrAgreementUnsupported（发起签约那条路）分开，是因为它们拒绝的是不同的动作：那一个是
// 「这条支付方式签不了约」，本一个是「这条渠道不给我们推协议通知」。
var ErrAgreementNotifyUnsupported = errors.New("this channel does not deliver agreement notifications")

// AgreementNotificationRequest 是一条协议变更通知的原始输入。
//
// 与 CallbackRequest 同形但**是两个类型**：两条路的请求形状今天一样（渠道码 + 原始报文 +
// 请求头 + 被投递到的方法与路径），而合成一个类型会让「这条路只处理协议」这件事在签名上
// 消失——那时往里塞一个支付回调也能编译通过。
type AgreementNotificationRequest struct {
	ChannelCode string
	Body        []byte
	Headers     http.Header
	// HTTPMethod / RequestPath 与支付回调那条路同一个理由：有的协议族把两者签进了待签串。
	// 微信 APIv2 不用它们（签名全在报文的 sign 字段里），但适配器的入参是共用的。
	HTTPMethod  string
	RequestPath string
}

// AgreementNotificationResult 是一条协议通知处理的结果，控制器拿它决定回给渠道什么。
type AgreementNotificationResult struct {
	NotificationID string
	AgreementNo    string
	ContractNo     string
	// EventType 是这条通知对应的事件类型（payment.agreement.signed / terminated）。
	EventType string
	// Duplicate 表示这是重投，且上一次已经处理完了。**没有副作用**。
	Duplicate bool
	// Settled 表示这次真的改了协议状态、发了事件。
	Settled bool
}

// HandleAgreementNotification 处理一条协议变更通知。
//
// 与 HandleNotification 逐字同构，差别只在第 4 步落的是哪张表。注释里不重复那四步的理由，
// 只写这条路上特有的三处：
//
//   - **渠道要额外认一次接口**（provider.AgreementNotifier）：支付回调那条路是每个适配器都
//     有的 Provider.Verify，而协议通知只有懂这套协议的适配器才认。
//   - **notification_id 是报文自身的 sha256**：微信的签约通知没有通知号（见
//     agreementNotificationID）。
//   - **结算的返回是「改了没有」而不是「结算成什么了」**：协议的目标状态在报文里，不在
//     本地状态机上，所以没有 PaymentSettlement 那种「已经是终态了」的分支——重复通知由
//     applyAgreementTarget 自己的判据挡掉（见 repository.SettleAgreementNotification）。
func (s *PaymentService) HandleAgreementNotification(ctx context.Context, in AgreementNotificationRequest) (*AgreementNotificationResult, error) {
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

	// 接口探测：探不到就是「这条渠道不会给我们推协议通知」。落一条留痕再拒——与「报文不对」
	// 一样要留痕，因为「有人往这个地址打了一条我们处理不了的报文」这件事本身要有人能看见。
	notifier, ok := adapter.(provider.AgreementNotifier)
	if !ok {
		unsupported := fmt.Errorf("%w: %s", ErrAgreementNotifyUnsupported, channel.Code)
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, unsupported)
		return nil, unsupported
	}

	method := methodFromChannel(channel)
	notification, verifyErr := notifier.VerifyAgreementNotification(ctx, provider.NotificationRequest{
		ChannelCode: channel.Code,
		Body:        in.Body,
		Headers:     in.Headers,
		HTTPMethod:  in.HTTPMethod,
		RequestPath: in.RequestPath,
		Method:      method,
		// 槽与签约那条路同源：验签的密钥就是签名那把（见 wechatpay.Provider.SecretSlots）。
		Secrets: resolveSecrets(s.resolveSecret, channel, adapter, method),
	})
	if verifyErr != nil {
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, verifyErr)
		return nil, verifyErr
	}

	// 目标状态与事件类型都在这里定下来，而不是在结算那里按本地状态再推一遍：事件类型同时要
	// 写进 payment_notifications.event_type 与 outbox 的 routing key，两处必须是同一个串。
	//
	// 翻不出来就地拒（落一条留痕）：那意味着渠道给了一个我们不认识的协议状态，而**我们不知道
	// 它是什么意思时不能拿它去改会员的授权**。放在插通知之前，是为了不落一条 event_type
	// 写着某个状态的记录——那条记录会让人以为我们知道这是什么。
	target, err := agreementTarget(notification.State)
	if err != nil {
		s.recordRejectedNotification(ctx, adapter, channel, in.Body, in.Headers, err)
		return nil, err
	}
	eventType := agreementStateEventType(notification.State)

	inserted, err := s.repository.InsertNotification(ctx, repository.NotificationParams{
		Provider:       channel.Provider,
		NotificationID: agreementNotificationID(in.Body),
		EventType:      eventType,
		// PaymentNo 留空：这条通知说的是协议，不是支付单。**它不是「忘了填」**——一份协议上
		// 永远不会有支付单号（代扣不建 payments 行，见 model.PaymentAgreementCharge.PaymentNo）。
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
		duplicate, handled, err := s.decideRedelivery(ctx, inserted, agreementNotificationID(in.Body),
			"agreement_no", notification.AgreementNo, "contract_no", notification.ContractNo)
		if err != nil {
			return nil, err
		}
		if handled {
			return &AgreementNotificationResult{
				NotificationID: inserted.ID,
				AgreementNo:    notification.AgreementNo,
				ContractNo:     notification.ContractNo,
				EventType:      eventType,
				Duplicate:      duplicate,
			}, nil
		}
	}

	settlement, err := s.repository.SettleAgreementNotification(ctx, repository.AgreementNotificationParams{
		NotificationID: inserted.ID,
		// 收到这条通知的那条渠道（URL 里那段），不是报文里说的任何东西：判据必须是「谁把这条
		// 报文投进来的」。签名证明得了「这条报文是这条渠道签的」，证明不了「这份协议是这条
		// 渠道的协议」。
		Provider:    channel.Provider,
		AgreementNo: notification.AgreementNo,
		ContractNo:  notification.ContractNo,
		Target:      target,
		Reason:      agreementNotifyReason(notification),
		TraceID:     audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		// 业务事务已经回滚了，所以这条通知记录还是 received。用**独立连接**把它标成 failed：
		// 那次回滚不该把「我们拒了这条通知」的留痕一起带走（同 HandleNotification）。
		if markErr := s.repository.MarkNotification(ctx, inserted.ID, model.NotificationFailed, err.Error()); markErr != nil {
			slog.ErrorContext(ctx, "failed to record a rejected agreement notification",
				"notification_id", inserted.ID, "error", markErr)
		}
		slog.WarnContext(ctx, "refused an agreement notification",
			"agreement_no", notification.AgreementNo, "contract_no", notification.ContractNo, "error", err)
		return nil, err
	}

	return &AgreementNotificationResult{
		NotificationID: inserted.ID,
		AgreementNo:    settlement.Agreement.AgreementNo,
		ContractNo:     settlement.Agreement.ContractNo,
		EventType:      eventType,
		// Changed 为 false 是「状态早就到了、这次什么都没改」：重复通知，或者一条迟到的
		// 解约通知打在已经解约的协议上。回给渠道的仍然是成功应答——它确实不需要再投了。
		Settled: settlement.Changed,
	}, nil
}

// agreementNotificationID 给一条协议通知合成 notification_id。
//
// **微信的签约通知里没有通知号**（报文里只有 contract_code / contract_id / change_type 这些
// 描述协议本身的字段），所以用报文自身的 sha256：同一个报文重推就是同一件事，这正是
// UNIQUE (provider, notification_id) 那道闸要的语义。
//
// 与「验签失败」那种留痕的 rejected:<sha> 前缀不同（见 rejectedNotificationID）：这里不带
// 前缀，因为这一行是**我们认下来的**通知，不是「我们没认下来的报文」。两者不会撞：一个报文
// 要么验得过要么验不过，不会在两条路上各留一行。
//
// 今天不可能与渠道给的通知号撞：这条渠道（wechat_pay）在支付回调那条路上只会回
// ErrNotAPaymentNotification，永远不会落一行带渠道通知号的记录。别的渠道真要做签约通知时，
// 这里要重新想一次——那时同一个 provider 下两种 id 的形态会混在一起。
func agreementNotificationID(body []byte) string {
	return bodySHA256(body)
}

// agreementStateEventType 把渠道说的状态翻成事件类型（也就是 routing key）。
//
// 与 agreementTarget（在 agreement.go，查约那条路也用它）分开两个函数，是因为它们答的是两个
// 问题：「本地那一行改成什么」与「下游该收到哪条消息」。今天两者一一对应，但它们是**两份
// 契约**——本地词表加了第七种状态时事件名不一定跟着加。
//
// 没有 default：词表外的状态在上面的 agreementTarget 那里就已经把这条通知拒掉了，走不到这里。
func agreementStateEventType(state provider.AgreementState) string {
	if state == provider.AgreementTerminated {
		return dto.EventAgreementTerminated
	}
	return dto.EventAgreementSigned
}

// agreementNotifyReason 是写进状态流水的那句短话。排查时先看的是它。
func agreementNotifyReason(notification provider.AgreementNotification) string {
	reason := "provider notification: agreement change " + string(notification.State)
	// provider_state 是渠道的原话（微信的 contract_state），有就带上：渠道说 state=0 而我们
	// 记成 terminated 时，这一句是唯一能把两边对上的东西。
	if notification.ProviderState != "" {
		reason += " (provider state " + notification.ProviderState + ")"
	}
	return reason
}
