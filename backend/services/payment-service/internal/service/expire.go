package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// failureCodeAccountFundedNotSettled 是「豆已经扣了，这张单却结不了」的失败码。
//
// 它落到 payments.failure_code 上，是给**运营**看的：这一笔的钱在账户域已经出账，而本地这张
// 支付单没有生效，只有人工能把那笔豆还回去。写成常量而不是就地拼一个字符串，理由与
// failureCodeInsufficientBeans 逐字相同——后台筛选与 e2e 要引用同一个值。
const failureCodeAccountFundedNotSettled = "account_funded_without_settlement"

// ExpireOverduePayments 关掉到点未支付的支付单，返回关掉的条数。
//
// 这一层几乎是空的——判定条件（status ∈ created|pending 且 expires_at 已过）全部在仓储的
// 一条 SQL 里，因为那是一个「选中一批行、在同一个事务里改掉」的动作，拆成 service 判条件 +
// 仓储执行会让并发的两个副本各选一批、互相覆盖。留这一层是为了**关单的语义有一个归属**：
// worker 只负责按周期调用它，不负责知道「什么算过期」。
//
// **不调渠道关单**：Provider 接口没有 Close（见 provider.go 的注释——只给 manual 一家写
// 一个桩正是要避免的写法）。这不只是省事，也是当前正确的做法：我们关的是本地那张待支付单，
// 渠道侧的预支付单有自己的有效期（provider.CreateRequest.ExpiresAt 就是让它不长于我们的），
// 到点自然失效。真实渠道接进来时，如果它的预支付单不会自己过期，那才是必须补 Close 的时候。
//
// **不发事件**：支付单过期不是一次支付结果。对 order-service 来说「这笔支付没成」与
// 「这笔支付从没发生过」是一样的——订单有自己的超时关单，它不需要知道这件事。
func (s *PaymentService) ExpireOverduePayments(ctx context.Context, limit int) (int, error) {
	return s.repository.ExpireOverduePayments(ctx, limit)
}

// SettleOverdueAccountPayments 把「豆已经扣了、到点还没结算」的支付单结算掉，返回结算的条数。
//
// 这些单是那两次写库（扣豆 / 结算）之间出的岔子留下的：进程被 kill、库抖动、或者这张单在
// 结算之前正好被关单扫描收走。**钱已经动了**——账户域那边扣豆是个独立提交，回来之后才轮到
// 我们写本地。所以它们既不能被关掉（关单会把一次真实出账说成「钱从来没动过」），也不能
// 就那么躺着等一个恰好会重试的用户。既然卖出去了，就得把它结完。
//
// 必须在 ExpireOverduePayments **之前**跑（见 worker）：关单那条查询排除了这些行，所以两批
// 互不相交，但如果先关一批、下一轮再结算，中间那段时间里用户看到的还是一张「已过期」而钱
// 已经扣了的单。顺序不能反。
//
// 逐条处理、单条失败不中断整批：一条坏的（比如它对应的订单已经被另一张支付单收过钱了）不该
// 让后面所有本该结算的单跟着卡住。这类单的结论是 **failed + failure_code**，不是留在原地
// 反复重试——见 settleOverdueAccountPayment。
func (s *PaymentService) SettleOverdueAccountPayments(ctx context.Context, limit int) (int, error) {
	payments, err := s.repository.FindOverdueAccountFundedPayments(ctx, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range payments {
		payment := &payments[i]
		// 第二个返回值是**这一次结成功了没有**，不是「有没有出错」：结不了但已经落成终态的
		// 那些不算结算，它们不该进这个计数（worker 拿它决定要不要记一行「结了几条」）。
		settled, err := s.settleOverdueAccountPayment(ctx, payment)
		if err != nil {
			// 记下 payment_no 与 account_entry_id：这条日志是「有一笔豆出了账、而这张单没能
			// 结算」的唯一出口，两个 id 缺一条就没法去账户库把账变找回来。
			slog.ErrorContext(ctx, "failed to settle an overdue account-funded payment",
				"payment_no", payment.PaymentNo, "account_entry_id", accountEntryIDOf(payment), "error", err)
			continue
		}
		if settled {
			count++
		}
	}
	return count, nil
}

// settleOverdueAccountPayment 结算一张「豆已经扣了」的单。
//
// 它复用 createAccountPayment 终点上那个同一个事务（SettleAccountPayment），所以出去的事件、
// 出资行、状态流水与正常发起时**逐字一致**——补偿路径不能有自己的一套记账写法，那正是两边
// 会慢慢对不上的地方。差的只有一件事：CreateResult.Action 这里得自己查一次（正常路径手上
// 就有那个支付方式），而它只影响一次幂等回放的字段，查不到就留空，不挡结算。
// 第二个返回值表示**这一次真的结成功了**。它不是「没出错」的同义词：一张结不了、但已经被
// 落成终态的单也不出错（见 concludeUnsettleableAccountPayment），而它不该被算进结算条数。
func (s *PaymentService) settleOverdueAccountPayment(ctx context.Context, payment *model.Payment) (bool, error) {
	// 成交时间用扣豆那一刻，不是现在：这两步之间可能隔了整整一个扫描周期甚至更久，
	// 而钱是在扣豆那一刻离开账户的。account_funded_at 由 RecordAccountDeduction 写下，
	// 被选中的行必然非空（筛选条件就是它非空）；真读到一个零值时退回现在。
	paidAt := s.now()
	if payment.AccountFundedAt != nil {
		paidAt = *payment.AccountFundedAt
	}

	result := &CreateResult{
		PaymentNo: payment.PaymentNo,
		Status:    model.PaymentSucceeded,
		Action:    s.accountFundingAction(ctx, payment),
		PayParams: map[string]string{},
	}

	_, err := s.repository.SettleAccountPayment(ctx, repository.SettleAccountPaymentParams{
		PaymentID:           payment.ID,
		AccountEntryID:      accountEntryIDOf(payment),
		PaidAt:              paidAt,
		IdempotencyResponse: result,
		RequestID:           payment.RequestID,
	})
	if err == nil {
		slog.WarnContext(ctx, "settled a payment whose coffee beans had already been deducted",
			"payment_no", payment.PaymentNo, "account_entry_id", accountEntryIDOf(payment))
		return true, nil
	}
	if !errors.Is(err, ErrPaymentNotPending) {
		// 真故障（库不可达之类）：留给下一轮。它**不会**被关单扫描收走——关单排除这些行。
		return false, err
	}
	return false, s.concludeUnsettleableAccountPayment(ctx, payment)
}

// concludeUnsettleableAccountPayment 给一张结不了的「已扣豆」单下一个结论。
//
// 走到这里只有一种情形：这张单已经不在能结算的状态了（最常见的是一次并发——同一张订单的
// 另一张支付单先成了，payments_one_succeeded_per_order 把这一张挡在外面）。豆是在账户域
// 独立扣走的，本地没有任何办法把它还回去（支付域对账户域只有扣减一个动作，冲正走的是
// 退款结果那条事件，见 BeanLedger 的注释）。所以这里能做、也必须要做的，
// 是**不让它继续隐身**：
//
//   - 先看清楚它是不是已经成了——并发下这很常见，成了就什么都不用做，也不能动它。
//   - 否则标成 failed + account_funded_without_settlement。这是一条**终止**状态：下一轮
//     扫描不会再选中它（筛选条件就是 created|pending），不会每分钟重试一次刷日志；而
//     failure_code 与留在行上的 account_entry_id 一起，把「哪一笔豆要还」说清楚了。
//
// 不标的话，这些单会永远停在 created：既不结算，也不过期，日志里每分钟一行同样的错误。
func (s *PaymentService) concludeUnsettleableAccountPayment(ctx context.Context, payment *model.Payment) error {
	current, err := s.repository.FindPaymentByNo(ctx, payment.PaymentNo)
	if err == nil && current.Status == model.PaymentSucceeded {
		return nil
	}
	// 读不到当前状态不算致命：MarkPaymentFailed 自己带 `AND status='created'` 的条件，
	// 它写不下去时会回 ErrPaymentNotPending，那说明有别的路径动过这一行，同样不该当成
	// 我们要处理的故障。
	message := "coffee beans were deducted but this payment could not be settled; the account entry needs a manual reversal"
	failed := &CreateResult{
		PaymentNo:      payment.PaymentNo,
		Status:         model.PaymentFailed,
		Action:         s.accountFundingAction(ctx, payment),
		PayParams:      map[string]string{},
		FailureCode:    failureCodeAccountFundedNotSettled,
		FailureMessage: message,
	}
	if _, err := s.repository.MarkPaymentFailed(ctx, repository.MarkPaymentFailedParams{
		PaymentID:           payment.ID,
		FailureCode:         failureCodeAccountFundedNotSettled,
		FailureMessage:      message,
		IdempotencyResponse: failed,
		RequestID:           payment.RequestID,
	}); err != nil && !errors.Is(err, ErrPaymentNotPending) {
		return err
	}
	slog.ErrorContext(ctx, "a payment with deducted coffee beans cannot be settled and needs a manual reversal",
		"payment_no", payment.PaymentNo, "account_entry_id", accountEntryIDOf(payment))
	return nil
}

// accountFundingAction 查出这张单当初那档支付方式的 action，给幂等回放用。
//
// 查不到就回空串。**不报错**：它影响的只是一次幂等回放里那个动作字段，而这次结算本身
// （钱进哪张单、发什么事件）与它无关——为它挡住一次真实的收款是把轻重搞反了。
//
// 从前的「查不到」有两条来源（支付方式被删了、渠道行没了）。今天支付方式是代码里的常量，
// 所以剩下的唯一来源是**这张单上那个 code 是历史值**——写在一次目录变更之前，跟不上了。
// 这一条同样是 warn 而不是错误，理由不变。
func (s *PaymentService) accountFundingAction(ctx context.Context, payment *model.Payment) string {
	if payment.PaymentMethod == "" {
		return ""
	}
	method, err := s.catalog.Method(payment.PaymentMethod)
	if err != nil {
		slog.WarnContext(ctx, "cannot resolve the payment method action while settling an overdue payment",
			"payment_no", payment.PaymentNo, "method", payment.PaymentMethod, "error", err)
		return ""
	}
	return string(method.Action)
}

// accountEntryIDOf 取支付单上记着的账变 ID，取不到时回空串。
//
// account_entry_id 是**唯一**能把一次结算指回账户域那笔账变的东西，补偿路径上读到 NULL
// 说明这一行本身就不该被选中（筛选条件就是它非空）。回空串而不是 panic：SettleAccountPayment
// 会把空的落成 NULL，钱依然收得进去，而这件事已经由这次结算的那行日志留了痕。
func accountEntryIDOf(payment *model.Payment) string {
	if payment.AccountEntryID == nil {
		return ""
	}
	return *payment.AccountEntryID
}
