package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// HandleEvent 消费本服务订阅的全部事件。它是 runtime.Options.ConsumerHandler 的实现，返回值
// 决定这条消息的归宿：nil 是 ack，非 nil 是「没处理成功」，由平台重投或送死信。
//
// 今天它认五件事，来自两个域：
//
//	order.paid                          有人买了一张带会员套餐的订单并且付款成功了 → 开通 / 续期
//	payment.agreement.signed            微信委托代扣的协议生效了              → 订阅转 active
//	payment.agreement.terminated        协议结束了（用户解约，或渠道说它没了）  → 订阅转 cancelled
//	payment.agreement.charge_succeeded  这一期扣到钱了                    → 续一期会员 + 推进订阅
//	payment.agreement.charge_failed     这一期没扣到                      → 失败计数 +1，连到阈值就停扣
//
// 后两条与中间那两条**说的不是一件事**：协议事件说的是授权（签了、解了），扣款事件说的是钱。协议
// active 而某一期没扣成是完全正常的一种组合——所以它们落在不同的仓储方法上（SettleSubscription
// 对 SettleCharge），不共用判据。
//
// 订阅哪几条由 RABBITMQ_ROUTING_KEY 决定（逗号分隔）——**绑定加不上时这里什么都不会发生**：
// 消息投到交易所就算成功，队列没绑上就是静默丢弃（见 messaging 里的实测记录）。所以新增一个
// case 的同时必须把那个键加进 deploy 的两处配置，否则症状是「用户签了约，后台那条订阅永远停在
// 待签约」，而两边日志里都干干净净。
//
// # 判定只有三条
//
//   - 不认识的事件类型：ack。这两个主题上还有一堆别的事件（下单、完成、售后、退款），它们
//     不是发给我们的，每一条都报错会把消费者日志变成噪音。
//   - 事件体不合法（少用户、少单号、时间解不开、快照缺一半）：报错。重投不会让它变合法
//     ——那是发出方坏了，得有人看。假装成功 ack 掉，用户就是付了钱没有会员。
//   - 事件说得明明白白「不用做事」（这一单里没有会员行）：ack。见下面那段。
//
// 重复投递不是错误：`membership_changes_order_unique (order_id, change_type)` 把「同一单的
// 同一种变更」钉成一条不变式，重投撞上它时仓储回 ErrDuplicateChange，这里把它翻成 ack——
// 那是「这件事已经做过了」，正是重投想要的答案。
func (s *MembershipService) HandleEvent(ctx context.Context, event messaging.Envelope) error {
	switch event.EventType {
	case dto.EventOrderPaid:
		return s.handleOrderPaid(ctx, event)
	case dto.EventAgreementSigned, dto.EventAgreementTerminated:
		return s.handleAgreementEvent(ctx, event)
	case dto.EventAgreementChargeSucceeded, dto.EventAgreementChargeFailed:
		return s.handleChargeEvent(ctx, event)
	default:
		return nil
	}
}

// newDecoder 是本服务的解码器，与 account-service 的同一套写法。
//
// 多发一个字段就报错：契约漂移要在联调时炸出来，而不是被静默忽略——被忽略的那次漂移可能正是
// 订单域改了字段名，而我们还按旧名字在处理。**这条规则是 dto.OrderPaidEventPayload 上那四
// 个「本服务不读但必须镜像」的字段存在的全部理由**：少镜像一个，整条消息就解不开。
func newDecoder(event messaging.Envelope) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	return decoder
}

// handleOrderPaid 是「订单支付成功」⇒ 开通或续期。
//
// 这条链路的另一端已经接上了（2026-09）：order-service 在订单带会员行时会把
// order_lines.membership_plan_snapshot 原样放进来（见 dto 里那段说明）。所以「带没带」
// 就是「这一单是不是买会员的」——没有会员段时 ack，那不是故障，绝大多数订单本来就与会员
// 无关。
func (s *MembershipService) handleOrderPaid(ctx context.Context, event messaging.Envelope) error {
	var payload dto.OrderPaidEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidUserID, payload.UserID)
	}
	orderID := strings.TrimSpace(payload.OrderID)
	if orderID == "" {
		// 没有单号就没有幂等凭据（唯一索引建在 order_id 上），重投会把会员续两次。
		// 宁可让这条消息进死信，也不能在没有凭据的情况下动权益。
		return fmt.Errorf("%w: orderId is required", ErrInvalidEvent)
	}
	if payload.Membership == nil {
		// 这一单里没有会员行。**这是常态，不是异常**：奶茶、咖啡、加购都是这么过的。
		// 报错会让绝大多数订单都进死信。
		return nil
	}

	snapshot, err := snapshotFromEvent(userID, *payload.Membership)
	if err != nil {
		return err
	}
	occurredAt, err := parsePaidAt(payload.PaidAt)
	if err != nil {
		return err
	}

	// 归属门店：这一单成交所在的门店，也就是「这个人为什么算这家店的会员」。
	//
	// 没有门店是**正常的**（小程序线上买会员就是这种），空串传下去表示「这一次没有归属可定」
	// ——仓储那边遇到已过期重开时会保留原值，而不是把归属擦掉。
	//
	// 有门店但不是 UUID 时报错、不静默丢掉：`memberships.store_id` 是 UUID 列，硬写进去是一条
	// 22P02（500）；而丢掉它等于把一个人的归属悄悄抹平，事后查不出来。两种都不行，所以让它与
	// 上面 userID 那条一样进死信——那说明发出方拼错了字段，要人看。
	storeID := ""
	if payload.StoreID != nil {
		storeID = strings.TrimSpace(*payload.StoreID)
	}
	if storeID != "" {
		if _, err := uuid.Parse(storeID); err != nil {
			return fmt.Errorf("%w: storeId %q", ErrInvalidEvent, storeID)
		}
	}

	_, err = s.repository.ApplyPaidOrder(ctx, repository.PaidOrderParams{
		UserID:     userID,
		OrderID:    orderID,
		Snapshot:   snapshot,
		OccurredAt: occurredAt,
		StoreID:    storeID,
		TraceID:    event.TraceID,
		RequestID:  orderID,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDuplicateChange):
		// 这一单的这一种变更已经记过了。支付成功回调会被重放，老系统同一个回调重试三四次
		// 是常事——**它不是故障，是「这件事已经做过了」**，正是重投想要的答案。
		return nil
	default:
		return err
	}
}

// snapshotFromEvent 把事件里的会员快照校一遍并翻成仓储的输入。
//
// # 为什么校得这么细
//
// 这些值会被原样写进 memberships 的快照列，而库上有一组 CHECK 钉着它们的**配对**（coupon 模式
// 必须配齐模板与张数、auto 模式必须都为空）。少校一条，撞上 CHECK 就是一条 23514（500），
// 而它其实是发出方拼错了一个字段——该被说清楚，而不是变成「服务器错误」。
//
// 校的这四条同时是**下单时**该挡的（order-service 那侧），这里是第二道：一份坏快照写进
// memberships 之后，那个人的会员价资格就永远说不清了。
func snapshotFromEvent(userID string, snapshot dto.OrderMembershipSnapshot) (repository.MembershipSnapshot, error) {
	planID := strings.TrimSpace(snapshot.PlanID)
	if _, err := uuid.Parse(planID); err != nil {
		// plan_id 上有外键指着 membership_plans。一个不存在的套餐会让整条消息永远投不进来
		// ——那说明订单域拿着一个本服务没见过的套餐在卖，这是要人看的。
		return repository.MembershipSnapshot{}, fmt.Errorf("%w: membership planId %q", ErrInvalidEvent, snapshot.PlanID)
	}
	planCode := strings.TrimSpace(snapshot.PlanCode)
	planName := strings.TrimSpace(snapshot.PlanName)
	if planCode == "" || planName == "" {
		// 快照缺了名字，写进去会撞 memberships.plan_code / plan_name 上的
		// CHECK (char_length(trim(...)) > 0)。
		return repository.MembershipSnapshot{}, fmt.Errorf("%w: membership snapshot for user %s has no plan name", ErrInvalidEvent, userID)
	}

	mode := strings.TrimSpace(snapshot.MemberPriceMode)
	couponTemplate := strings.TrimSpace(snapshot.MemberPriceCouponTemplateID)
	switch mode {
	case model.MemberPriceModeCoupon:
		if _, err := uuid.Parse(couponTemplate); err != nil {
			return repository.MembershipSnapshot{}, fmt.Errorf("%w: coupon mode needs a template id, got %q", ErrInvalidEvent, snapshot.MemberPriceCouponTemplateID)
		}
		if snapshot.MemberPriceCouponsPerPeriod <= 0 {
			// 张数不是正数，「每期发 N 张」就会变成发 0 张——用户开完包月发现会员价用不了，
			// 而这在系统里看不出任何异常。
			return repository.MembershipSnapshot{}, fmt.Errorf("%w: coupon mode needs a positive count, got %d", ErrInvalidEvent, snapshot.MemberPriceCouponsPerPeriod)
		}
	case model.MemberPriceModeAuto:
		// auto 模式下这两列必须是空的（库上的 CHECK 也这么要求）。这里**不报错**而是清掉：
		// 多带两个值不改变任何行为（auto 模式本来就不发券），为它让一条已经付过钱的消息进
		// 死信是不划算的。这与上面那两条「少了必需的东西」正好相反——少了没法补，多了可以丢。
		couponTemplate = ""
		snapshot.MemberPriceCouponsPerPeriod = 0
	default:
		return repository.MembershipSnapshot{}, fmt.Errorf("%w: unknown member price mode %q", ErrInvalidEvent, snapshot.MemberPriceMode)
	}

	period := strings.TrimSpace(snapshot.Period)
	if period != model.PeriodMonth && period != model.PeriodYear {
		return repository.MembershipSnapshot{}, fmt.Errorf("%w: unknown period %q", ErrInvalidEvent, snapshot.Period)
	}
	if snapshot.PeriodCount <= 0 {
		// 零或负的期数会让 expire_at 等于 start_at，而 memberships 上有 CHECK (expire_at > start_at)
		// ——撞上去是一条 23514（500）。这里先拦，让它是「发出方给了一个买不了时长的订单」。
		return repository.MembershipSnapshot{}, fmt.Errorf("%w: period count is %d", ErrInvalidEvent, snapshot.PeriodCount)
	}

	return repository.MembershipSnapshot{
		PlanID:                      planID,
		PlanCode:                    planCode,
		PlanName:                    planName,
		MemberPriceMode:             mode,
		MemberPriceCouponTemplateID: couponTemplate,
		MemberPriceCouponsPerPeriod: snapshot.MemberPriceCouponsPerPeriod,
		Period:                      period,
		PeriodCount:                 snapshot.PeriodCount,
		AutoRenew:                   snapshot.AutoRenew,
	}, nil
}

// handleAgreementEvent 是「委托代扣协议变了」⇒ 把订阅收口到它现在实际该处于的状态。
//
// # 它是 confirm 那条路的补课，不是另一套规则
//
// 用户从微信回来点「我签好了」只是**我们主动去问的一次**。用户没回来（关掉小程序、直接退出）
// 时，协议照样在渠道那边生效了——这条事件就是那一刻从支付域推过来的。两条路落的库必须是同一个
// 结论，所以它调的是同一个 repository.SettleSubscription（见那段说明）。
//
// # 与 confirm 的一处差别：这里没有人可以问
//
// ConfirmSubscription 拿不准时可以「什么都别做，让用户过一会儿再来」（渠道说 pending 时）。
// 事件是**已经发生的事实**，没有那个选项：status 只有 active / terminated 两个取值，别的取值
// 是发出方坏了，报错进死信。
//
// # 幂等靠状态本身
//
// 重投（broker 重连、处理完没 ack 时崩溃）与「confirm 已经改过了」都会让这次收口发现
// current.Status 已经等于目标状态，仓储据此短路成「没改动」。所以这里不需要 request_id：
// **目标状态本身就是幂等键**，而 membership_changes 那一侧也不写（见下）。
func (s *MembershipService) handleAgreementEvent(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AgreementEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	agreementID := strings.TrimSpace(payload.AgreementID)
	if _, err := uuid.Parse(agreementID); err != nil {
		// 它是命中订阅的唯一键（membership_subscriptions.agreement_id 是 UUID 列），解不开就
		// 什么都定位不到。报错而不是 ack：这条事件里说的协议我们没记，说明两边对不上。
		return fmt.Errorf("%w: agreementId %q", ErrInvalidEvent, payload.AgreementID)
	}
	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: userId %q", ErrInvalidEvent, payload.UserID)
	}

	target, ok := settleTargetFor(strings.TrimSpace(payload.Status))
	if !ok {
		// 既不是 active 也不是 terminated。**不 ack**：这是契约漂移（发出方加了第三种状态而
		// 我们还按两种理解），静默丢掉会让订阅永久停在错的档位上。
		return fmt.Errorf("%w: unknown agreement status %q", ErrInvalidEvent, payload.Status)
	}

	row, _, err := s.repository.SettleSubscription(ctx, repository.SettleParams{
		// 只有协议 id：事件体里没有订阅 id，一份协议也只对一条订阅（库上那个部分唯一索引）。
		AgreementID:   agreementID,
		Target:        target,
		AgreementNo:   strings.TrimSpace(payload.AgreementNo),
		CancelReason:  cancelReasonForEvent(event.EventType),
		ProviderState: strings.TrimSpace(payload.Status),
		// 事件体里没有时刻（Envelope 只有 id / type / version / traceId / payload），用当下。
		// 它只在「订阅转 active 且这个人此刻没有有效会员」时被当作起点，见 repository.applySettle。
		OccurredAt: s.Now(),
		TraceID:    event.TraceID,
		// AdminID 留空：这不是人工操作，痕迹在 payment_notifications 与 outbox 里。
	})
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrSubscriptionNotFound):
		// 这份协议名下没有订阅。**ack**：绝大多数情况下它压根不是本域的东西（协议是支付域的
		// 通用能力，别的域将来也可能签），为它让整条队列堵在一条重投上不划算。但也不是「正常」
		// ——留一条日志，真出现「签了约却没有订阅」时那是唯一的线索。
		slog.WarnContext(ctx, "membership: agreement event has no subscription",
			"agreementId", agreementID, "eventType", event.EventType, "userId", userID)
		return nil
	default:
		return err
	}

	// 归属对不上：事件里的 userId 与订阅上的不是同一个人。
	//
	// **收口已经做完了**（上面那一步按协议 id 定位，不看 userId——命中的那份协议确实是这条订阅
	// 的，所以改的是对的那一行）。这里报的是另一件事：支付域记的签约人与本域记的不是同一个人，
	// 这是两边数据对不上的硬信号，得有人看。所以返回错误让它进死信，**不是为了重做**（重投还是
	// 同一条事件），而是为了让它停在一个人看得见的地方；本地状态此时已经是对的，不会被这几跳改坏。
	if row != nil && row.UserID != userID {
		return fmt.Errorf("%w: agreement %s belongs to user %s, event says %s",
			ErrInvalidEvent, agreementID, row.UserID, userID)
	}
	return nil
}

// handleChargeEvent 是「某一期代扣出结果了」⇒ 结算这一期。
//
// # 它与 handleAgreementEvent 的分工
//
// 那一条收口的是**授权**（协议在不在），这一条落的是**钱**（这一期扣到没有）。收到一条
// charge_failed 就以为协议没了，会把一份还在微信上挂着的授权从本地划掉——用户下个月照样被扣，
// 而我们这边显示「已解约」。
//
// # 幂等
//
// 成功那条靠渠道流水号（写进 membership_changes.request_id，撞唯一索引即「这一笔已经续过了」）；
// 失败那条**没有幂等键**，因为它本来就可以合理地发生多次（一期失败三次正是停扣判据的来源），
// 靠平台的收件箱按 event id 去重。这两件事的差别写在 repository.SettleCharge 的说明里。
//
// # 定位不到订阅时 ack
//
// 与协议事件那条一样的处置：协议是支付域的通用能力，别的域将来也可能签，一份不属于本域的协议
// 收到扣款结果不该让整条队列堵住。留一条 warn 日志——真出现「扣了钱却没有订阅」时那是唯一的线索。
func (s *MembershipService) handleChargeEvent(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AgreementChargeEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	agreementID := strings.TrimSpace(payload.AgreementID)
	if _, err := uuid.Parse(agreementID); err != nil {
		// 它是命中订阅的唯一键（membership_subscriptions.agreement_id 是 UUID 列），解不开就什么
		// 都定位不到。报错而不是 ack：这条事件里说的协议我们记不下来。
		return fmt.Errorf("%w: agreementId %q", ErrInvalidEvent, payload.AgreementID)
	}
	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: userId %q", ErrInvalidEvent, payload.UserID)
	}

	target, ok := chargeTargetFor(event.EventType, strings.TrimSpace(payload.Status))
	if !ok {
		// 事件名说的结果与载荷里那个状态对不上，或者状态是个我们不认识的词。**不 ack**：这两条
		// 事件共用一个载荷，认错了的后果是把一次失败当成一次成功——那会凭一条没扣到钱的事件给用户
		// 续一期会员。
		return fmt.Errorf("%w: %s carries status %q", ErrInvalidEvent, event.EventType, payload.Status)
	}
	bizPeriod := strings.TrimSpace(payload.BizPeriod)
	if bizPeriod == "" {
		// 期次只进流水，但它同时也是对账时手里拿的那个号（见 model.ChargePeriod）。缺了它这条
		// 流水答不出「续的是哪一期」，而这不是可以靠重投补上的东西。
		return fmt.Errorf("%w: bizPeriod is required", ErrInvalidEvent)
	}

	// **先建单，再结算**：这个顺序反不得。
	//
	// 反过来的话（结算完再建单），一条重投会先被结算那一步的幂等挡住——SettleCharge 发现这笔
	// 渠道流水已经续过了，回 changed=false，函数在下面那个 `!outcome.Changed` 的分支里安静地
	// ack，**建单那一步永远轮不到**。而「重投」在这里恰恰是常态（broker 重连、处理完没 ack 时
	// 崩溃），所以订单会**静默地少掉**，谁都不报错——这正是这一刀要修的那个缺陷的形状。
	//
	// 换过来之后两边都还是对的：建单幂等（同一个渠道流水号命中同一张单），结算幂等（撞唯一索引
	// → ack）。而且第一次投递崩在两步之间时，重投能把订单补上——反过来那个顺序补不了，因为
	// 事件已经发出去过了（发券那一条读的就是事件里的订单号）。
	//
	// 建单失败就是没成：返回错误让平台重投（≤5 次后进死信），**不往下走结算**——钱收了而账
	// 记不上，这时候给用户续上会员只是让账面更难对。
	orderID, err := s.recordRenewalOrderFor(ctx, target, agreementID, payload.ProviderTransactionID, payload.Amount)
	if err != nil {
		return err
	}

	outcome, err := s.repository.SettleCharge(ctx, repository.ChargeSettleParams{
		// 只有协议 id：事件体里没有订阅 id，一份协议也只对一条订阅（库上那个部分唯一索引）。
		AgreementID: agreementID,
		UserID:      userID,
		Target:      target,
		BizPeriod:   bizPeriod,
		Amount:      payload.Amount,
		// 这一期在订单域那张续费单。它落进续费流水，也随 membership.renewed 出去——**下游发券
		// 的判据就是它**（见 repository.ChargeSettleParams.OrderID）。
		OrderID: orderID,
		// 渠道流水号：成功那条非空且是幂等键；失败那条可能是空的，而空串在那里是**对的**——见
		// repository.SettleCharge 里关于失败流水为什么不能拿它当幂等键的那段。
		ProviderTransactionID: strings.TrimSpace(payload.ProviderTransactionID),
		FailureCode:           strings.TrimSpace(payload.FailureCode),
		FailureMessage:        strings.TrimSpace(payload.FailureMessage),
		// 事件体里没有时刻（Envelope 只有 id / type / version / traceId / payload），用当下。对成功
		// 那条它是续期的起点候选之一：这个人的会员如果已经过期，就从这一刻重新起算。
		OccurredAt: s.Now(),
		TraceID:    event.TraceID,
	})
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrDuplicateChange):
		// 同一笔渠道流水已经续过了。支付那边对同一次扣款可能发出两遍（通知重投、补发），而
		// **重投不是故障**——正是幂等想要的答案（与 handleOrderPaid 那条逐字相同）。
		return nil
	case errors.Is(err, repository.ErrSubscriptionNotFound):
		slog.WarnContext(ctx, "membership: charge event has no subscription",
			"agreementId", agreementID, "bizPeriod", bizPeriod, "eventType", event.EventType)
		return nil
	default:
		// 其中包括 ErrChargeEndedSubscription（一条解约了的订阅扣到了钱）：它要的是人来看，不是
		// 重试，返回错误让它停在死信里（见那条错误的说明）。
		return err
	}

	// 归属对不上：事件里的 userId 与订阅上的不是同一个人。
	//
	// **结算已经做完了**（上面那一步按协议 id 定位，改的是对的那一行），这里报的是另一件事：
	// 支付域记的签约人与本域记的不是同一个人。与 handleAgreementEvent 逐字同一条处置——返回错误
	// 让它进死信，不是为了重做，而是为了让它停在一个人看得见的地方。
	if outcome.Subscription != nil && outcome.Subscription.UserID != userID {
		return fmt.Errorf("%w: agreement %s belongs to user %s, charge event says %s",
			ErrInvalidEvent, agreementID, outcome.Subscription.UserID, userID)
	}
	if !outcome.Changed {
		// 什么都没写，两种原因、轻重不同，所以分开说：
		//
		//   - 订阅已经结束了（解约 / 走完）：这一期扣成没扣成对它都没有意义。钱没动过的那种不必
		//     惊动人；扣到了钱的那种已经在仓储里报错进了死信，走不到这里。
		//   - 这一笔已经结算过（同一笔渠道流水的重复投递）：**正常**，是幂等生效的样子。
		switch outcome.Subscription.Status {
		case model.SubscriptionStatusCancelled, model.SubscriptionStatusExpired:
			slog.WarnContext(ctx, "membership: charge result for a subscription that is no longer charging",
				"agreementId", agreementID, "bizPeriod", bizPeriod, "eventType", event.EventType,
				"subscriptionStatus", outcome.Subscription.Status)
		default:
			slog.InfoContext(ctx, "membership: charge result already settled; nothing to do",
				"agreementId", agreementID, "bizPeriod", bizPeriod, "eventType", event.EventType)
		}
		return nil
	}

	// 下面两条是运营要能看见的信号。成功那条走 info：一次正常的续费不该是告警；**停扣走 warn**——
	// 那是这一整条链上唯一一个「从此不再扣这个人的钱」的动作，而它今天不对外发事件。
	if outcome.Suspended {
		slog.WarnContext(ctx, "membership: auto charge suspended after consecutive failures",
			"agreementId", agreementID, "bizPeriod", bizPeriod,
			"consecutiveFailedCount", outcome.Subscription.ConsecutiveFailedCount,
			"failureCode", strings.TrimSpace(payload.FailureCode))
		return nil
	}
	slog.InfoContext(ctx, "membership: charge settled",
		"agreementId", agreementID, "bizPeriod", bizPeriod, "target", target,
		"amount", payload.Amount, "providerTransactionId", strings.TrimSpace(payload.ProviderTransactionID))
	return nil
}

// chargeTargetFor 判这条事件说的是一次成功还是一次失败。
//
// **两份都要对**：事件名与载荷里的 status 必须一致。载荷带 status 是有意的冗余（见 dto 里那段），
// 而冗余的唯一用途就是拿来对一遍——只读其中一个的话，那个字段永远不会和另一个不一致，也就永远
// 发现不了发出方把它们写岔了。写岔的后果是不对称的：把失败读成成功会给一个没付钱的人续一期会员。
func chargeTargetFor(eventType, status string) (string, bool) {
	var want string
	switch eventType {
	case dto.EventAgreementChargeSucceeded:
		want = dto.ChargeStatusSucceeded
	case dto.EventAgreementChargeFailed:
		want = dto.ChargeStatusFailed
	default:
		return "", false
	}
	if status != want {
		return "", false
	}
	return want, true
}

// cancelReasonForEvent 是订阅上 cancel_reason 取什么值。
//
// 中文、一句人话，与老系统同一个口径：那边解约与同步写的是「管理员同步：微信侧已解约」
// 「combo退款-会员部分退款」。这一列在后台订阅详情里**直接显示**（admin-web 的「取消原因」），
// 写成 user_terminated 那样的码没人看得懂。
//
// 与 confirm 那条路**故意不同**：那边渠道只说「这份协议没了」，说不出为什么，所以留空；而事件名
// 本身就是结论——`terminated` 这条事件在支付域由解约通知触发。
func cancelReasonForEvent(eventType string) string {
	if eventType == dto.EventAgreementTerminated {
		return "微信侧已解约"
	}
	return ""
}

// parsePaidAt 解出支付成功时刻。
//
// 用事件里的时刻而不是 time.Now()：补投一条上周的事件时，会员的 start_at 与叠加基点都该是
// 上周的那个时刻，否则「这个月开了多少会员」会随一次重投而变，提前续费的人也会被吞掉几天。
//
// 解不开就报错，**不回退到「现在」**：一个解不开的时间串说明发出方换了格式，而回退会把这个
// 事实变成一批时间戳错了的会员记录——它们看起来完全正常。
func parsePaidAt(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("%w: paidAt is required", ErrInvalidEvent)
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: paidAt %q is not RFC3339", ErrInvalidEvent, value)
	}
	return parsed.UTC(), nil
}
