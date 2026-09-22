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
// 它认六件事，正好是本服务与订单域的全部交接点：
//
//	order.completed            这一单完成了 → 发放福卡，余额增加
//	order.after_sale.applied   用户申请退款 → 冻结福卡，这些卡不能抽奖了
//	order.after_sale.reviewed  管理员审核   → 驳回：解冻福卡；通过：**什么都不做**（钱还没退）
//	order.after_sale.cancelled 用户撤销     → 解冻福卡
//	order.after_sale.refunded  退款成功     → 追回福卡（解冻 + 冲正那几笔发放），并冲正咖啡豆
//	order.after_sale.refund_failed 退款失败 → 解冻福卡（钱没出去，卡凭什么锁着），豆一分不动
//
// 最后两条是退款链的收口，也是**唯一**能让冻结行走完的两条路：一支把它结在 recovered
// （卡收回来了），一支把它结在 released（卡放回去了）。冻结不再有「一直冻着」这个结局。
//
// 钱的两种结局也是本服务**所有**还钱动作的时点：豆与福卡都不在「审核通过」那一拍动。
// 审核通过只说明「同意退」，钱还在渠道那边；那时候就把赠品收走、把豆还回去，撞上退款
// 失败就得再还一遍原样的东西回去。挂在钱的结果上，每一种结局只对应一个动作。
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
	case dto.EventAfterSaleRefunded:
		return s.handleAfterSaleRefunded(ctx, event)
	case dto.EventAfterSaleRefundFailed:
		return s.handleAfterSaleRefundFailed(ctx, event)
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
// 这条事件只做一件事：**驳回 ⇒ 解冻福卡**。通过什么都不做，豆的冲正也不在这里。
//
// 通过之所以是一个空分支，是因为它只代表「同意退」，钱还没出去——退款单是紧接着才向
// 渠道发起的，成没成由 order.after_sale.refunded / .refund_failed 回来。在这个时点上：
//
//	福卡  解冻等于把刚压住的卡又放出去，用户还没拿到钱；收走则更没道理，钱还没退。
//	咖啡豆 还回去等于按「同意退」付款——退款可以失败，那时候这笔豆就得原样再扣一次。
//
// 所以两者都等钱的结果：退成 ⇒ 收走福卡、还回豆（handleAfterSaleRefunded），没退成 ⇒
// 什么都不用做（豆从没动过，卡由 handleAfterSaleRefundFailed 放回）。**一个动作只对应
// 钱的一种结局**，不必在「通过」和「退成」之间分两步走同一件事。
//
// 驳回也不动豆：豆在支付那一刻就已经扣走了，驳回不改变「用户花掉了这些豆」这个事实。
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
		return nil
	default:
		// 别的状态（pending 之类）不该从这条事件上来。解冻是不可逆地放松保护，认不出来
		// 就别动——与「认不出来的 scope 不冻」同一个方向。
		return nil
	}
}

// reverseBeansForRefund 是「钱退成了」这一步对咖啡豆做的事：把这一单扣掉的豆还回去。
//
// **唯一的触发点是退款成功**，不是审核通过。豆是支付时就已经收下的钱，还回去就是退款
// 本身，所以它必须跟着钱走：钱没退成，豆一分不动，不需要任何补救；钱退成了，豆就是退
// 款的一部分。挂在「通过」上会多出一次「钱没出去但豆已经还了」的中间态——那正是这一
// 刀要消掉的东西。
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
// 换触发点没有削弱这两道：`after_sale.refunded` 与 reviewed 一样由 order-service 的
// outbox 与事实同事务发出，重放分支不重发（见 AdvanceRefund）。
//
// 反查入口一直是那笔扣减的流水（`coffee_bean_entries` 里 `order:{orderId}` 那条 consume），
// 不是事件本身——所以这里不需要「这笔豆已经冲过了吗」这种额外判断：`after_sale:{no}`
// 那把唯一索引问的就是同一件事，而且答案在数据库上，不依赖事件投递的语义。
func (s *AccountService) reverseBeansForRefund(ctx context.Context, payload dto.AfterSaleRefundEventPayload, occurredAt time.Time) error {
	if payload.RefundAmount <= 0 {
		// 订单域在申请那一刻就挡掉了 `amount <= 0`（ErrAfterSaleNothingToRefund），所以
		// 这个数不该出现在一条退款成功事件里。报错而不是跳过：跳过等于欠退，而欠退比
		// 错退更难被发现。
		return fmt.Errorf("%w: after sale refund amount is %d", ErrInvalidAmount, payload.RefundAmount)
	}
	_, err := s.ReverseBeans(ctx, BeanReverseRequest{
		OrderID:     strings.TrimSpace(payload.OrderID),
		AfterSaleID: strings.TrimSpace(payload.AfterSaleID),
		AfterSaleNo: strings.TrimSpace(payload.AfterSaleNo),
		Amount:      payload.RefundAmount,
		Remark:      "售后单 " + strings.TrimSpace(payload.AfterSaleNo) + " 退款成功",
		// 与同一拍上的福卡冲正用同一个时刻（调用方算好的那个）：退款成了一条事件，
		// 两处账变的业务时间就必须是同一刻，否则补投旧事件时明细页上它们的先后是乱的。
		OccurredAt: occurredAt,
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

// handleAfterSaleRefunded 是「钱退成了」⇒ 两件还钱的事一起做。
//
// 一、追回这一单送出去的福卡。冻结从**申请那一刻**就压着了（handleAfterSaleApplied），
// 所以这里不用再确认「卡还在不在」：冻着的卡抽不了奖，从申请到这一刻之间它不可能被花掉。
// 这里做的是把那份冻结**结掉**——解冻 + 冲正（余额真的少掉），余额上的结果就是「钱退了，
// 赠品也退了」。追不回来的那部分（申请之前就已经被抽掉的）由仓储按冻结额钳住，不报错、
// 也不影响这条消息的归宿：钱是真的退回去了，为它失败只会让这条事件一路重试到死信。
//
// 二、把这一单扣掉的咖啡豆还回去（reverseBeansForRefund）。纯豆单的钱就是我们自己收的
// 豆，钱退成的那一刻就是豆该还回去的那一刻；渠道单在这里是 no-op。
//
// 两件事写在同一拍上，但各自独立成事务、各自幂等：一条重投进来时，先跑完福卡追回
// （`after_sale_no` 那把唯一索引 + 冻结行的状态判断让它成为 no-op），再跑豆的冲正
// （`after_sale:{no}` 唯一索引拦第二笔），谁也不会被重复做一遍。追回失败时豆那一步
// 不会执行——这条消息仍然进重试，钱已经退出去的事实不会因此丢。
func (s *AccountService) handleAfterSaleRefunded(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AfterSaleRefundEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
	if afterSaleNo == "" {
		// 冻结行的幂等键缺了，追回就无从下手。进死信让有人看见。
		return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidEvent)
	}

	// 用钱的时刻而不是收到消息的那一刻：补投一条前几天的事件时，流水上的顺序必须
	// 还是那几天——与冲正、发放同一条规矩。
	occurredAt := s.now()
	if payload.RefundedAtUnix > 0 {
		occurredAt = time.Unix(payload.RefundedAtUnix, 0).UTC()
	}

	if _, err := s.Recover(ctx, afterSaleNo, occurredAt); err != nil {
		return err
	}
	return s.reverseBeansForRefund(ctx, payload, occurredAt)
}

// handleAfterSaleRefundFailed 是「钱没退成」⇒ 解冻。
//
// 走的就是驳回/撤销那条路（Release），一行新逻辑都不用写：三种情况下钱都没出去，
// 冻着的卡凭什么锁着。豆也一分不动——冲正挂在退款成功那一拍上，钱没退成时它从未发生
// 过，所以这里没有要撤销的动作（这正是把豆挪到这一拍的收益：不存在「白退了豆」）。
//
// 这一步不是收尾工作，是**下一个回合的前提**：退款失败后那张售后单是终态，用户要重新
// 申请；而在途冻结已经把可用吃光，重开的那张冻结行只冻得到 0 张——不在这里解开，第二次
// 退款成功时一张卡都追不回来。
func (s *AccountService) handleAfterSaleRefundFailed(ctx context.Context, event messaging.Envelope) error {
	var payload dto.AfterSaleRefundEventPayload
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
	if afterSaleNo == "" {
		return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidEvent)
	}
	return s.Release(ctx, afterSaleNo, FreezeReasonRefundFailed)
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
