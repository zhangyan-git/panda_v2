package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 代扣（续费扫描那条路）。**这一段只发起，不结算。**
//
// 钱到没到由渠道推回来的两条事件决定（见 event.go 对 charge_succeeded / charge_failed 的
// 处置），本文件一行订阅状态都不改。这个分工不是洁癖，是这一刀里最容易搞错的那件事：
//
//   - 渠道同步回的 SUCCESS **只说明受理**。老系统在这里直接把这一期记成成功、顺手把
//     next_charge_at 推一期（subscription_service.go:634,650），于是钱没扣到而会员多了一个月。
//   - 反过来，如果这里在发起之后就把 next_charge_at 推走，紧接着到达的那条「扣款成功」通知
//     会再推一次（它按同一个字段算到期日），一期被算成两期。
//
// 所以权威的推进只发生在一个地方：扣款成功那条事件的消费里。

// ChargeDue 扫一批到点的订阅，逐条向支付服务发起这一期的扣款，返回**发起了几条**。
//
// 返回的那个数只用来写日志、以及让 worker 判断这一轮是不是满载；它不代表扣到了钱，也不代表
// 对方受理了（受理与否在各条日志里）。
func (s *MembershipService) ChargeDue(ctx context.Context, limit int, traceID string) (int, error) {
	if s.agreements == nil {
		// 没有支付域这条连接就发不起扣款。与签约那条路同一个判断（见 signing.go）：这里没有
		// 「退化成空操作」可退，静默什么都不发等于悄悄地不续费。
		return 0, ErrChannelUnavailable
	}
	due, err := s.repository.ListDueSubscriptions(ctx, s.Now(), limit)
	if err != nil {
		return 0, err
	}

	charged := 0
	for _, row := range due {
		// **一条失败不拦后面的人**：一次 gRPC 超时不该让这一批剩下的几十条订阅这一轮全都发不
		// 出去。下一轮扫描（十分钟后）还会捞到它们，而幂等键保证了重复发起无害。
		if err := s.chargeSubscription(ctx, row, traceID); err != nil {
			slog.WarnContext(ctx, "membership: charge a due subscription",
				"subscriptionId", row.ID, "userId", row.UserID,
				"nextChargeAt", row.NextChargeAt, "error", err)
			continue
		}
		charged++
	}
	return charged, nil
}

// chargeSubscription 发起一条订阅这一期的扣款。
//
// # 期次从 NextChargeAt 派生
//
// 见 model.ChargePeriod。**绝不能改成「现在是什么日期」**：这一期扣成之前 NextChargeAt 不
// 推进，所以三次重试算出来的是同一个期次、落在支付库同一行上；换成今天，重试就成了新的一期，
// 也就是重复扣款。
func (s *MembershipService) chargeSubscription(ctx context.Context, row *repository.SubscriptionRow, traceID string) error {
	subscription := row.Subscription
	if subscription.NextChargeAt == nil {
		// 理论上到不了：ListDueSubscriptions 那条查询拿 next_charge_at 比过大小，NULL 比不出
		// 真；库上也有 CHECK 钉着「active 必须有 next_charge_at」。但这里是要**解引用**它的地方，
		// 而一次空指针会让整个 worker goroutine 崩掉——那一天所有的续费都不再发生，且现场只剩
		// 一行 panic。宁可这一条不发。
		return errors.New("membership: active subscription " + subscription.ID + " has no next charge time")
	}
	if subscription.ContractCode == "" {
		// 同理：签约那一刻就写进去了，空值只可能是本服务自己写坏的。原样抛错，不编一句「这份
		// 协议没签过」——代扣这一侧没有可查的替代键。
		return errors.New("membership: subscription " + subscription.ID + " has no contract code")
	}

	period := model.ChargePeriod(*subscription.NextChargeAt)
	result, err := s.agreements.Charge(ctx, dto.ChargeAgreementParams{
		AgreementNo: subscription.ContractCode,
		BizPeriod:   period,
		// 金额取订阅上的快照，不是套餐今天的价：代扣金额签约时就约定死了，套餐后来调价不影响
		// 已签约的人（见 model.Subscription.PriceCents）。
		Amount:  subscription.PriceCents,
		Subject: row.PlanName,
		// 只进支付侧的流水（幂等键是 agreement_no + biz_period）。它的用处是排查时把两边的日志
		// 按同一把钥匙对上，所以带上订阅与期次这两个能定位到这一行的东西。
		RequestID: chargeRequestID(subscription.ID, period),
	})
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "membership: charge requested",
		"subscriptionId", subscription.ID, "userId", subscription.UserID,
		"agreementNo", result.AgreementNo, "bizPeriod", result.BizPeriod,
		"amount", subscription.PriceCents, "status", result.Status,
		"providerTransactionId", result.ProviderTransactionID,
		"failureCode", result.FailureCode, "failureMessage", result.FailureMessage,
		"traceId", traceID)

	switch result.Status {
	case dto.ChargeStatusCharging:
		// 正常路径：渠道受理了，钱到没到等通知。这里**什么都不做**。
	case dto.ChargeStatusFailed:
		// 渠道当场拒了，或者上一轮的失败还没到重试时间。支付那边已经排好了退避、并且会发一条
		// charge_failed（订阅的失败计数由那条事件累加，见 event.go），这里不重复记一遍。
		slog.WarnContext(ctx, "membership: charge was rejected",
			"subscriptionId", subscription.ID, "bizPeriod", result.BizPeriod,
			"failureCode", result.FailureCode, "failureMessage", result.FailureMessage)
	case dto.ChargeStatusSucceeded, dto.ChargeStatusPending, dto.ChargeStatusSkipped, dto.ChargeStatusCancelled:
		// 这一期在支付那边**已经有结论了**——多半是上一轮的失败/成功还没结算到本服务，或者通知
		// 已经先到了。同样什么都不做：推进计数与到期日的只有那条事件。
		slog.InfoContext(ctx, "membership: charge already has an outcome",
			"subscriptionId", subscription.ID, "bizPeriod", result.BizPeriod, "status", result.Status)
	default:
		// 支付侧加了一个本服务还不认识的状态。**不改任何东西**，但要让它在日志里显形——静默
		// 走进 default 的表现是「这一期看起来发出去了、然后什么都没有」。
		slog.WarnContext(ctx, "membership: charge returned an unknown status",
			"subscriptionId", subscription.ID, "bizPeriod", result.BizPeriod, "status", result.Status)
	}
	return nil
}

// chargeRequestID 是发起扣款时带的那个流水号。
//
// 它**不是幂等键**（那是支付侧的 agreement_no + biz_period），所以这里只要「同一行同一期算
// 出来是同一串、换一行或换一期就不一样」就够。用它而不是随机数，是为了排查时一眼能从支付侧的
// 流水回到本服务这一行。
func chargeRequestID(subscriptionID, period string) string {
	return "charge:" + subscriptionID + ":" + period
}
