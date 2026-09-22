package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 本文件是「一期扣款成功之后，先在订单域记一张单」这一步。
//
// # 它为什么要单独一个文件
//
// 因为**顺序**是这一步的全部内容，而顺序最容易在别处被改坏：它必须在
// repository.SettleCharge **之前**跑（调用点在 event.go 的 handleChargeEvent 里，那里有一整
// 段说明）。把这段逻辑塞回事件处理那个函数的中间，下一个人调整那几行时会看不出这两步有先后。
//
// # 它与结算的分工
//
//	本文件     钱在订单域记成一张单（另一个域的事实，跨一次 gRPC）
//	SettleCharge 钱在会员域续成一期会员（本域的事实，一个事务）
//
// 两件事都在「扣款成功」那一刻发生，但**只有后者能和前者分开重做**：结算有自己的幂等（渠道
// 流水号），订单也有（同一个号在订单域是幂等键）。所以重投整个 handler 是安全的，前提是两步
// 都是幂等的——它们都是。

// recordRenewalOrderFor 是 handleChargeEvent 那个调用点的入口，只判一件事：**这一支该不该建单**。
//
// 失败的那一期**不建单**：那笔钱没收上来，没有订单可言（老系统同此——`createRenewalOrder` 只在
// 成功那一支被调用）。失败的期次留在 payment_agreement_charges 里，后台订阅详情的「续费明细」
// 看的就是它，那里有金额、失败原因和已试次数，比一张标着「已支付」的空单有用得多。
//
// 判定用的是 target 而不是事件名：target 是 chargeTargetFor 拿**事件名与载荷里的 status 对了
// 一遍**之后得出的（见那边），比单看其中一个可靠。
func (s *MembershipService) recordRenewalOrderFor(ctx context.Context, target, agreementID, providerTransactionID string, amount int64) (string, error) {
	if target != dto.ChargeStatusSucceeded {
		return "", nil
	}
	return s.recordRenewalOrder(ctx, agreementID, providerTransactionID, amount)
}

// recordRenewalOrder 为这一期扣款在订单域建一张续费单，返回它的 id。
//
// # 返回值有两个「没有订单」的出口，它们不是一回事
//
//	("", nil)   这份协议名下没有订阅 —— 不该建单，而且**不是错误**：结算那一步会给出同一个
//	            结论并安静地 ack（见 handleChargeEvent）。在这里抢先报错会把一条「不属于本域
//	            的事件」变成一条死信。
//	("", err)   没建成。除了下面那条之外，一律返回错误让整条消息重投。
//
// # 空流水号是硬错误
//
// 成功事件里它必然非空（扣到钱才有号，见 payment-service 的 MarkChargeAttempt），空值说明发出
// 方把两条事件写岔了。**不重投也不会变非空**，所以它进死信等人看——而不是建一张没有幂等键的
// 订单（那等于放弃幂等：下一次投递会建出第二张，而钱只收了一次）。
//
// # 金额与快照都取自冻结值，不现查套餐
//
// 订阅上的 price_cents / period / period_count 与会员行上的 plan_code / plan_name 与会员价
// 那三列都是签约时约定死的。现查 membership_plans 会让一次后台改套餐改写一个正在被扣款的用户
// 这一期买到了什么——与 carriedSnapshot 那条规矩逐字相同，只是那一个服务的是本域的续期，这一个
// 服务的是订单域那张单。
func (s *MembershipService) recordRenewalOrder(ctx context.Context, agreementID, providerTransactionID string, amount int64) (string, error) {
	if s.orders == nil {
		// 装配缺失。与签约那条路同一个处置（见 charge.go 里那处 ErrChannelUnavailable）：没有
		// 订单域就记不成这张单，而钱已经收了——不能假装成功。
		return "", ErrChannelUnavailable
	}
	if strings.TrimSpace(providerTransactionID) == "" {
		return "", fmt.Errorf("%w: a succeeded charge carries no providerTransactionId", ErrInvalidEvent)
	}

	chargeCtx, err := s.repository.LoadChargeContext(ctx, agreementID)
	switch {
	case errors.Is(err, repository.ErrSubscriptionNotFound):
		// 这条协议不属于本域。**不回错误**：上面那段说了，结算那一步会给出同一个结论并 ack。
		return "", nil
	case err != nil:
		// 其中包括 ErrMembershipNotFound：订阅挂在一个不存在的会员上是一行坏数据（membership_id
		// 上有外键），要让这条消息停下来等人看，而不是当「没有会员」糊过去。
		return "", err
	}

	subscription, membership := chargeCtx.Subscription, chargeCtx.Membership
	order, err := s.orders.CreateRenewal(ctx, dto.RenewalOrderParams{
		// 渠道流水号既是这一期扣款的身份证，也是订单域的幂等键。用它而不是期次：期次是
		// 本服务按 next_charge_at 派生的，同一期在一次重试里可能算出不同的值，而流水号是渠道
		// 给的、一次扣款只有一个。
		ThirdPartyOrderNo: strings.TrimSpace(providerTransactionID),
		// 下单人取自**订阅行**，不是事件体里那个 userId。两者本来就该校验成同一个人（事件处理
		// 那一头在结算之后会比一遍，对不上进死信），而订单是对着本域这一行记的账——它引用的是
		// 我们记着的那个人。
		UserID: subscription.UserID,
		// 金额取**事件里那个**（这一期实际扣了多少），不是订阅上的 price_cents：两者正常相等
		// （发起扣款时传的就是 price_cents），而真出现差额时，订单该记的是钱实际动了多少。
		Amount:       amount,
		Plan:         renewalPlanSnapshot(subscription, membership),
		MembershipID: subscription.MembershipID,
		// 备注留空：订单域会拼上「会员续费扣款」，那是它对这一类订单的固定说法（老系统的
		// 「订阅续费」同一个位置）。在这里再写一遍就会变成两句。
	})
	if err != nil {
		return "", err
	}
	return order.OrderID, nil
}

// renewalPlanSnapshot 把订阅与会员两行上冻结的那份快照拼成订单域要的形状。
//
// # 一期的时长取自订阅，不取会员行
//
// 会员行上根本没有 period / period_count 这两列——那是「这一期有多长」，只有订阅上有
// （与 carriedSnapshot 同一个理由，那边是给本域续期用的，这里是给订单用的）。
//
// # 券那两列照原样搬，不按模式清空
//
// auto 模式下它们本来就是 NULL（memberships 上那条 CHECK 钉着），搬出来就是零值；而万一库里
// 是一对脏值，订单域那边也只是把一段不对的 JSON 记进快照——**不在这里替它决定**。真正的发券
// 判据在 coupon-service，它读的是快照里的 memberPriceMode，而不是「这两列空不空」。
func renewalPlanSnapshot(subscription *model.Subscription, membership *model.Membership) dto.RenewalPlanSnapshot {
	return dto.RenewalPlanSnapshot{
		PlanID:   subscription.PlanID,
		PlanCode: membership.PlanCode,
		PlanName: membership.PlanName,
		// PriceCents 是签约时约定死的那一期价（subscription.price_cents），不是会员行上的
		// 任何一列：会员行记的是「他买到了什么」，订阅记的是「每期扣多少」。
		PriceCents:                  subscription.PriceCents,
		Period:                      subscription.Period,
		PeriodCount:                 subscription.PeriodCount,
		MemberPriceMode:             membership.MemberPriceMode,
		MemberPriceCouponTemplateID: textOrEmpty(membership.MemberPriceCouponTemplateID),
		MemberPriceCouponsPerPeriod: countOrZero(membership.MemberPriceCouponsPerPeriod),
		AutoRenew:                   membership.AutoRenew,
	}
}

// countOrZero 把可空的券张数摊成 0。
//
// 摊成 0 之后**不再回写**任何地方，只是让跨服务的那份快照形状稳定（proto 的整数没有「空」这一
// 态）。真正的判据是同一个结构里的 MemberPriceMode：auto 模式下 coupon-service 不看这个数。
// 空串那一列复用 subscription.go 里的 textOrEmpty，理由与那边逐字相同（nil 与空串在券模板 id
// 上是同一件事：没配）。
func countOrZero(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
