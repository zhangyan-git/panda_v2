package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// 下面是本服务拥有的流水文案（fortune_card_entries.title 的列注释：文案属于账户域，
// 调用方给的是起因）。
//
// 放在这里而不是让订单域把文案拼进事件：文案是会改的，「订单完成赠送」将来可能变成
// 「饮品完成赠送」，那是一次发版，不该要求订单域跟着发。
const (
	titleOrderGrantBase = "订单完成赠送"
	// 加赠活动没有名字时（订单域只拆得出张数），仍然要有一条能看的文案。
	titleOrderGrantBonusFallback = "订单完成赠送（活动加赠）"
	titleDraw                    = "参与抽奖"
	titleReverse                 = "冲正"
)

// HandleOrderEvent 消费订单事件。它是 runtime.Options.ConsumerHandler 的实现，
// 返回值决定这条消息的归宿：nil 是 ack，非 nil 是「没处理成功」，由平台重投或送死信。
//
// 它认四件事，正好是本服务与订单域的全部交接点：
//
//	order.completed          这一单完成了  → 发放福卡，余额增加
//	order.after_sale.applied 用户申请退款  → 冻结福卡，这些卡不能抽奖了
//	order.after_sale.reviewed 管理员审核   → 驳回：解冻福卡（通过不解冻，钱还没退）
//	                                        通过：冲正咖啡豆（驳回不动，豆已经扣走了）
//	order.after_sale.cancelled 用户撤销    → 解冻福卡
//
// 判断只有三条（对每一个类型都成立）：
//   - 不认识的事件类型：ack。主题里有别的类型，那不是发给我们的。
//   - 事件体不合法（少用户、少单号、张数非正数、没有幂等键）：报错。重投不会让它
//     变合法——这是发出方坏了，得有人看。假装成功把它 ack 掉才是真的丢福卡。
//   - 事件说得明明白白「不用做事」（这单不送福卡、审核是通过、售后单上没有要冻的键）：
//     ack。那是常态，不是异常。
//
// 重复投递不是错误：幂等压在各自业务表的唯一索引上（流水的 entry_key、冻结的
// after_sale_no），重投不会产生第二笔流水、也不会冻两次。所以这里没有「已经处理过」的
// 分支——那个分支要靠一次额外查询，而真正的幂等保证在数据库那把索引上，不在这里。
func (s *AccountService) HandleOrderEvent(ctx context.Context, event messaging.Envelope) error {
	switch event.EventType {
	case dto.EventOrderCompleted:
		return s.handleOrderCompleted(ctx, event)
	case dto.EventAfterSaleApplied:
		return s.handleAfterSaleApplied(ctx, event)
	case dto.EventAfterSaleReviewed:
		return s.handleAfterSaleReviewed(ctx, event)
	case dto.EventAfterSaleCancelled:
		return s.handleAfterSaleCancelled(ctx, event)
	default:
		return nil
	}
}

// newDecoder 是四条消费路径共用的解码器。
//
// 多发一个字段就报错：契约漂移要在联调时炸出来，而不是被静默忽略——被忽略的那次
// 漂移可能正是订单域换了字段名，而我们还按旧名字在记账（与 order-service 消费
// 支付结果时的取舍一致）。
func newDecoder(event messaging.Envelope) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	return decoder
}

// handleOrderCompleted 是订单完成 ⇒ 福卡到账。
func (s *AccountService) handleOrderCompleted(ctx context.Context, event messaging.Envelope) error {
	var payload dto.OrderCompletedEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidUserID, payload.UserID)
	}
	orderNo := strings.TrimSpace(payload.OrderNo)
	orderID := strings.TrimSpace(payload.OrderID)
	if orderNo == "" || orderID == "" {
		return fmt.Errorf("%w: orderNo and orderId are required", ErrInvalidEvent)
	}
	if len(payload.FortuneCards) == 0 {
		// 这单不送福卡。收款、出杯都不受影响，所以是 ack 而不是错误。
		return nil
	}

	occurredAt := s.now()
	if payload.FinishedAtUnix > 0 {
		// 用订单给的完成时间而不是 NOW()：补投或重放一条旧事件时，NOW() 会把上周完成的
		// 那单记成刚才，明细页上的顺序就乱了（见 occurred_at 的列注释）。
		occurredAt = time.Unix(payload.FinishedAtUnix, 0).UTC()
	}

	lines := make([]repository.GrantLine, 0, len(payload.FortuneCards))
	for _, grant := range payload.FortuneCards {
		entryKey := strings.TrimSpace(grant.EntryKey)
		if entryKey == "" {
			// 没有幂等键就没有「只发一次」这回事，宁可让这条消息进死信。
			return fmt.Errorf("%w: fortune card grant is missing an entry key", ErrInvalidEvent)
		}
		if grant.Amount <= 0 {
			return fmt.Errorf("%w: fortune card grant amount is %d", ErrInvalidAmount, grant.Amount)
		}
		title, err := grantTitle(grant)
		if err != nil {
			return err
		}
		lines = append(lines, repository.GrantLine{
			Title:    title,
			Amount:   grant.Amount,
			EntryKey: entryKey,
		})
	}

	_, err := s.repository.GrantOrderFortune(ctx, repository.GrantParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    orderNo,
		OccurredAt: occurredAt,
		Lines:      lines,
	})
	return err
}

// handleAfterSaleApplied 是「用户提交了退款申请」⇒ 冻结这一单的福卡。
//
// 冻结时点是**申请**而不是审核通过：从申请到审核之间有一个窗口，卡在这个窗口里被花掉
// 就再也追不回来了——「已经抽过奖的福卡追不回来」是这条规则存在的全部理由。
func (s *AccountService) handleAfterSaleApplied(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AfterSaleAppliedEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidUserID, payload.UserID)
	}
	afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
	if afterSaleNo == "" {
		// 冻结的幂等键缺了。进死信让有人看见，而不是冻一行事后解不开的账。
		return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidEvent)
	}
	if len(payload.FortuneCardEntryKeys) == 0 {
		// 这一单没承诺福卡，或者退的是不送福卡的会员套餐。没有要冻的东西，ack。
		return nil
	}

	return s.Freeze(ctx, userID, afterSaleNo, payload.OrderID, payload.OrderNo,
		payload.FortuneCardEntryKeys, FreezeReasonApplied)
}

// handleAfterSaleReviewed 是「管理员审核了这张售后单」。
//
// 两件事在这条事件上分开走，因为它们保护的是两种不同的东西：
//
//	福卡  驳回 → 解冻。**通过不解冻**：通过了只代表「同意退」，钱还没出去（退款单在
//	      payment-service，未建）。冻结一直保持到退款成功、追回福卡那一刻。在这里解冻
//	      会让用户在拿到退款之前先拿到可用的卡，恰好把冻结想挡的那件事放出去。
//	咖啡豆 通过 → 冲正。**驳回什么都不做**：豆在支付的那一刻就已经扣走了，用户手上不再
//	      有这些豆，没有可保护的东西，自然也没有要放开的东西。而通过就是资金侧完整的
//	      退款（纯豆单的钱就是我们自己收的豆，全额、无外部渠道），所以把钱还回去。
//
// 「通过不解冻福卡、通过即冲正豆」不是矛盾：福卡是**还没花的凭证**（还回去等于放它出去），
// 豆是**已经收下的钱**（还回去正是退款本身）。
func (s *AccountService) handleAfterSaleReviewed(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AfterSaleReviewedEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	switch strings.TrimSpace(payload.Status) {
	case dto.AfterSaleStatusRejected:
		afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
		if afterSaleNo == "" {
			return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidEvent)
		}
		return s.Release(ctx, afterSaleNo, FreezeReasonRejected)
	case dto.AfterSaleStatusApproved:
		return s.reverseBeansForAfterSale(ctx, payload)
	default:
		// 别的状态（pending 之类）不该从这条事件上来。解冻是不可逆地放松保护、冲正是不可逆
		// 地把钱还出去，认不出来就别动——与「认不出来的 scope 不冻」同一个方向。
		return nil
	}
}

// reverseBeansForAfterSale 是「审核通过」这一步对咖啡豆做的事。
//
// 绝大多数订单是渠道支付的，那些单在这里**什么都不发生**：账户域按 `order:{orderId}`
// 找不到扣减流水，ReverseBeans 返回 (false, nil) 而不报错。这是常态而不是异常，所以
// 把返回的 bool 丢掉——今天没有任何调用方需要知道这一次到底冲没冲（重投也一样，见下）。
//
// 金额取自事件的 refundAmount，不自己算：它是订单域在申请那一刻按 `实付 − 已退 − 在途`
// 钳过的数（order-service 的 applyAfterSale 里 `remaining`），部分退（先退加购行、再退
// 整单）因此天然是两次不同的金额。本服务不重算那份口径，也不整单照抄。
//
// 幂等有两道：同一条售后重投由 `after_sale:{afterSaleNo}` 的唯一索引挡住（不产生第二笔
// 冲正），而「已经冲回多少」由这笔扣减的所有冲正流水之和算出来，所以第二次部分退只补差额。
//
// ⚠️ 留给退款单那一轮的接缝：`approved` 今天在订单域是**终点**——`orders.refunded_amount`
// 从来没有被写过，`refunding`/`refunded` 两个状态没有任何写路径。等 payment-service 的
// 退款单落地、多出一条「退款成功」的事件时：
//
//   - **不能再冲一次豆**。同一张售后单重复投递由上面那把唯一索引挡着，但一个**新的**触发源
//     需要显式判断「这笔豆已经冲过了」，而不是靠事件幂等（那是两回事：幂等挡「同一条消息
//     投两次」，挡不住「另一条消息说同一件事」）。
//   - 反查入口是 `order_payment_lines.account_entry_id`（那笔扣减的流水 ID），不是这里的
//     事件——它已经由 payment_fundings 一路带到了订单域。
func (s *AccountService) reverseBeansForAfterSale(ctx context.Context, payload dto.AfterSaleReviewedEventPayload) error {
	if payload.RefundAmount <= 0 {
		// 订单域在申请那一刻就挡掉了 `amount <= 0`（ErrAfterSaleNothingToRefund），所以
		// 这个数不该出现在一条 approved 事件里。报错而不是跳过：跳过等于欠退，而欠退比
		// 错退更难被发现。
		return fmt.Errorf("%w: after sale refund amount is %d", ErrInvalidAmount, payload.RefundAmount)
	}
	_, err := s.ReverseBeans(ctx, BeanReverseRequest{
		OrderID:     strings.TrimSpace(payload.OrderID),
		AfterSaleID: strings.TrimSpace(payload.AfterSaleID),
		AfterSaleNo: strings.TrimSpace(payload.AfterSaleNo),
		Amount:      payload.RefundAmount,
		Remark:      "售后单 " + strings.TrimSpace(payload.AfterSaleNo) + " 审核通过",
	})
	return err
}

// handleAfterSaleCancelled 是「用户撤销了自己那张还没被审核的申请」⇒ 解冻。
//
// 撤销之后用户手上再没有任何能解开它的动作了，所以这条消息是唯一的解冻信号；
// Release 那边把它做成幂等的 no-op，让重投与乱序都是安全的。
func (s *AccountService) handleAfterSaleCancelled(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AfterSaleCancelledEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
	if afterSaleNo == "" {
		return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidEvent)
	}
	return s.Release(ctx, afterSaleNo, FreezeReasonCancelled)
}

// grantTitle 把一笔发放翻成用户能看懂的那一行字。
//
// 种类不认识时报错而不是退回 base：那说明订单域加了第三种发放而我们还不认识它，
// 记成「订单完成赠送」会让这一笔再也查不出来是怎么来的。
func grantTitle(grant dto.OrderFortuneGrant) (string, error) {
	switch strings.TrimSpace(grant.Kind) {
	case model.GrantKindBase:
		return titleOrderGrantBase, nil
	case model.GrantKindBonus:
		name := strings.TrimSpace(grant.CampaignName)
		if name == "" {
			return titleOrderGrantBonusFallback, nil
		}
		return titleOrderGrantBase + "（" + name + "）", nil
	default:
		return "", fmt.Errorf("%w: unknown fortune card grant kind %q", ErrInvalidEvent, grant.Kind)
	}
}
