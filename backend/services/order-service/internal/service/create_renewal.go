package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// renewalOrderPaymentMethod 是续费单的支付方式：微信代扣。
//
// 取值与 payment-service 支付目录里的那条 code 一致（代扣签约与扣款用的就是它）。它在这里是
// 一个**字面量而不是 import**：order-service 不依赖 payment 域的目录包，就像设备单那边写死
// `card_pay` 一样。两条路的口径差别在于——`card_pay` 只是「钱是在机器上收的」一句说明，
// 而这一条**真的对应 payment 域里的一个支付方式**（扣款行 payment_agreement_charges 是它收的），
// 后台按支付方式筛订单时要能和支付域对得上。
//
// 但这一单**仍然没有支付单**：payment_no 留空，因为一条 payments 行的语义是「用户发起了一次
// 支付」，而代扣是到期自动发生的、没有那一次发起。查询与对账走 payment_agreement_charges
// 那张表（后台订阅详情的「续费明细」读的就是它）。
const renewalOrderPaymentMethod = "wechat_papay"

// renewalOrderReason 是续费单建单时写进状态流水的原因，也是订单备注的前缀。
//
// 「扣款」两个字是必要的：这一单的触发者不是用户，是到期自动扣。后台看到这一行要能立刻
// 明白钱是**从用户账上划走的**，而不是他点了一次。
const renewalOrderReason = "会员续费扣款"

// CreateRenewalOrderInput 是一次会员续费建单的全部输入。
//
// 它与 CreateDeviceOrderInput 是亲戚（都是「钱已在别处收过」），但**有用户**：
// orders.user_id 非空，那一期的会员费是某个人的。
//
// # 这里没有 paid_at
//
// 记账时刻取本服务收到调用的时刻，老系统同此（createRenewalOrder 写的也是 time.Now()）。
// 渠道那边真正扣款的那一刻在 payment-service 的扣款行上，不在这里重造一个。
type CreateRenewalOrderInput struct {
	// ThirdPartyOrderNo 是**渠道流水号**（payment_agreement_charges.provider_transaction_id），
	// 这条路唯一的幂等键。非空。
	ThirdPartyOrderNo string
	// UserID 是下单用户。**非空**——与设备单相反。
	UserID string
	// Amount 是这一期的实付金额（分），权威来自调用方（订阅行上冻结的 price_cents）。
	Amount int64
	// Plan 是签约时冻结的套餐快照。**整份由调用方给全**，理由见 order.proto 里
	// CreateRenewalOrder 的说明：续费必须用签约时那一份，让本服务拿 plan_id 去会员域现取
	// 等于让一次后台改套餐改写一个正在被扣款的用户这一期买到了什么。
	Plan dto.MembershipPlanSnapshot
	// MembershipID 是 memberships.id，可空。填了后台从会员看订单查得到。
	MembershipID string
	Remark       string
}

// CreateRenewalOrderResult 是建单结果。
type CreateRenewalOrderResult struct {
	OrderID string
	OrderNo string
	// Created=false 表示这次命中的是已有的那一张（渠道重投或消息重投），什么都没写。
	Created bool
}

// CreateRenewalOrder 落一笔会员续费订单：钱是微信代扣收的，这里把既成事实记下来。
//
// # 为什么是「直接落成已支付」
//
// 与设备单同一条：回调到达时那笔钱已经划走了。这条路上**更不能**先落 pending_payment——
// 它没有支付单、没有待支付期、也没有人会去付它，落成 pending_payment 只会得到一张永远
// 等不到支付的单，关单扫描还会把它当超时单关掉，而钱是真的收了。所以 status 直接是 paid、
// paid_at 是收到调用的时刻、payment_no 为空。
//
// # 顺序：调用方先建单、后结算（这一条决定了整条链的可重投性）
//
// 这一单**必须**在调用方（membership-service）落续费与发券**之前**建出来。反过来的话，
// 消息重投时调用方的结算会先撞唯一索引被当成幂等命中 ack 掉，建单那一步永远轮不到——
// 订单静默缺失，而钱只收了一次。这一条写在 membership-service 的 handleChargeEvent 里，
// 但根因在这里：**订单号是下游发券的判据**（见下）。
//
// # 订单号为什么要给出去
//
// 用户每期扣款之后要发一批会员价券，而下游发券的判据之一是这一单的订单号。老系统也是
// 这么串的。所以本服务回的是**真订单号**，而不是一个内部 id——调用方把它写进自己的
// 续费流水（membership_changes.order_id）与事件里，券才发得出来。
//
// # 幂等
//
// 按渠道流水号幂等，靠 orders_third_party_order_no_key 兜（不是先查后插，见
// repository.CreateRenewalOrder）。命中时**不报错**，返回既有那张单、created=false：
// 扣款结果通知是 MQ 消息，至少一次投递，重投是常态。
//
// 命中时那一张还必须**是同一类**单：单号空间与设备那两条共用，撞上一张刷卡机单时回
// ErrThirdPartyOrderNoTaken。判据与写那一步在同一个事务里（见仓储）。
//
// # 不校验金额与套餐快照（有意的）
//
// 与设备单的 amount 看似同形，其实是相反的两件事：那边我们**手里没有基准**，所以谈不上核对；
// 这边基准明确（订阅与会员行上的冻结值），但**核对没有意义**——调用方填的就是那一份冻结值，
// 而这一次调用发生在**钱已经收走之后**，本服务此时唯一有价值的行为是把它如实记下来。
// 校验不过就拒收的后果是：钱扣了、会员没续、消息重投五次后进死信，而人工要在死信队列里
// 找回一笔真实发生过的交易。所以这一条路与前两条一样，是「记录既成事实」，不是「审批」。
func (s *OrderService) CreateRenewalOrder(ctx context.Context, in CreateRenewalOrderInput) (*CreateRenewalOrderResult, error) {
	in.ThirdPartyOrderNo = strings.TrimSpace(in.ThirdPartyOrderNo)
	in.UserID = strings.TrimSpace(in.UserID)
	in.Plan.PlanID = strings.TrimSpace(in.Plan.PlanID)
	if in.ThirdPartyOrderNo == "" {
		// 没有幂等键这条路就不成立：重投会变成第二张单，而钱只收了一次。
		return nil, ErrThirdPartyOrderNoRequired
	}
	if in.UserID == "" {
		// 与设备单相反：这一单有主人。user_id 为 NULL 的续费单在后台「按用户看订单」里
		// 查不到，而用户自己那一期扣款也就无处可归。
		return nil, ErrUserRequired
	}
	if in.Amount < 0 {
		// 没有「倒找钱」的续费。负数过不了库上的 CHECK，也不该被吞成 0——那会把一次调用方
		// 的算错记成一张 0 元单，而真实的扣款金额就再也追不回来了。这一条是**调用方写坏了**，
		// 拒收并让它重投/告警，比落一张错单好。
		return nil, ErrRenewalAmountInvalid
	}
	if in.Plan.PlanID == "" {
		// 会员行没有套餐值引用就成了无主的一行：后台看不出这一期买的是什么，事后也追不回
		// 是哪一档套餐。名称与编码可以为空（那只是显示），这一格不行。
		return nil, ErrRenewalPlanRequired
	}

	snapshot, err := json.Marshal(in.Plan)
	if err != nil {
		// 入参全是标量，编不出来只可能是代码写错了（与 create.go 里那份快照同一条判断）。
		return nil, fmt.Errorf("encode membership plan snapshot: %w", err)
	}

	now := s.now().UTC()
	result, created, err := s.repository.CreateRenewalOrder(ctx, repository.CreateRenewalOrderParams{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		OrderNo:           generateOrderNo(now),
		UserID:            in.UserID,
		MembershipID:      normalizedID(&in.MembershipID),
		Amount:            in.Amount,
		PaymentMethod:     renewalOrderPaymentMethod,
		PaidAt:            now,
		Remark:            renewalOrderRemark(in.Remark),
		TransitionReason:  renewalOrderReason,
		Line: &repository.OrderLineInsert{
			// 一单一行：一期续费就是一期会员，没有加购、没有饮品。
			LineNo:   1,
			LineType: model.LineTypeMembership,
			// item_id 是套餐的值引用；编码与名称是签约当时的副本（套餐改名不影响历史订单）。
			// 图留空：套餐没有图片这一说。
			ItemID:   &in.Plan.PlanID,
			ItemCode: in.Plan.PlanCode,
			ItemName: in.Plan.PlanName,
			Quantity: 1,
			// 三个价格同值：会员套餐没有「标价」与「成交价」之分（会员价那条优惠是给饮品的，
			// 不是给会员套餐本身的），优惠额一律为零——与 applyMembershipPlan 同一条口径。
			OriginalUnitPrice: in.Amount,
			UnitPrice:         in.Amount,
			PayableAmount:     in.Amount,
			// 四列 NOT NULL DEFAULT '{}' 的快照全部显式写值：这一单没有规格、没有屏幕选品、
			// 没有加购活动，套餐快照就是上面 marshal 的那一份。留 nil 会直接撞 NOT NULL
			// ——INSERT 里每一列都写了值，不写的列才会走 DEFAULT。
			Specs:                  jsonObject(nil),
			SelectionSnapshot:      jsonObject(nil),
			CampaignSnapshot:       jsonObject(nil),
			MembershipPlanSnapshot: snapshot,
			// DeviceID 留空：纯会员订单没有设备（见 repository 里那段逐列说明）。
		},
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	return &CreateRenewalOrderResult{OrderID: result.OrderID, OrderNo: result.OrderNo, Created: created}, nil
}

// renewalOrderRemark 拼订单备注：固定前缀 + 调用方给的备注。
//
// 前缀写死，与设备单同一条理由：「这一单从哪来」不该由调用方决定，调用方给的那段只能当备注。
func renewalOrderRemark(remark string) string {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return renewalOrderReason
	}
	return renewalOrderReason + "：" + remark
}
