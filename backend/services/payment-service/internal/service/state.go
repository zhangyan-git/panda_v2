package service

import "github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"

// paymentTransitions 是支付单状态机：从哪个状态能走到哪些状态。
//
// 与 order-service 的 orderTransitions 同一种定位——它是**一处描述**，不是唯一的权威判定。
// 真正的判定在写路径里：仓储在 FOR UPDATE 锁住那一行之后再看一次状态（repository/callback.go
// 的 canSettle、payment.go 的 `WHERE status='created'`）。并发下只有锁内那次判断算数。
// 这张表的作用是让「这单还能不能往下推」有一处可读，并且让状态的完整形状集中在一处。
//
// 这一版能真正走到的转换：
//
//	created → pending     （渠道返回支付参数，见 create.go 的事务 2）
//	created → failed      （渠道拒了，见 create.go 的事务 3）
//	created → expired     （建单后没走到 pending 就超时，见 worker/expiry.go）
//	pending → succeeded   （渠道成功回调）
//	pending → failed      （渠道失败回调）
//	pending → expired     （超时关单扫描）
//
// closed 的两个箭头本轮走不到：关单的触发方是订单取消，而这一版不消费订单事件（见
// 「明确不做」）。退款相关的状态**不在这份状态机里**，也不在 payments.status 的取值里：
// 退款是 payment_refunds 表上的独立聚合（见下面的 refundTransitions），退款期间支付单
// 本身仍是 succeeded。把退款塞进这张表会让「这笔钱收没收到」变得要读两个状态才知道。
var paymentTransitions = map[string][]string{
	model.PaymentCreated: {
		model.PaymentPending,
		model.PaymentFailed,
		model.PaymentClosed,
		model.PaymentExpired,
	},
	model.PaymentPending: {
		model.PaymentSucceeded,
		model.PaymentFailed,
		model.PaymentClosed,
		model.PaymentExpired,
	},
	// succeeded 没有出边：收钱这件事已经发生了。退款不改它。
	model.PaymentSucceeded: nil,
	// failed 是终态。用户换一种方式再付是**新的一张支付单**（payments 的注释），
	// 不是把这一张改回去——同一张单曾经失败过又成功，会让「失败了几次」这个数丢掉。
	model.PaymentFailed:  nil,
	model.PaymentClosed:  nil,
	model.PaymentExpired: nil,
}

// fundingTransitions 是出资行的状态机。
//
// 从 reserved 出去的三条边，区别在「这笔钱为什么没扣成」：
//
//	reserved → succeeded  渠道确认收到，或账户余额扣成功
//	reserved → failed     渠道明确拒绝（余额不足、风控拦截），或支付单被渠道判失败
//	reserved → released   预占解开，钱从来没被碰过——超时关单走这条（见 worker/expiry.go）
//
// 对账时这两者要分开看：failed 会在渠道侧留一条拒绝记录，released 在渠道侧什么都没有。
// 混成一个值会让「这个月被拒了多少笔」和「多少笔没人付」算成同一个数。
//
// 四条出边都是终态。**尤其 failed/released 不能回到 reserved**：重新发起支付是**新的一张
// 支付单**（payments 的注释），于是也有一批新的出资行。复用同一行会让「这笔出资尝试过几次」
// 丢掉，而这正是对账要看的。
var fundingTransitions = map[string][]string{
	model.FundingReserved: {
		model.FundingSucceeded,
		model.FundingFailed,
		model.FundingReleased,
	},
	model.FundingSucceeded: {model.FundingReversed},
	model.FundingFailed:    nil,
	model.FundingReleased:  nil,
	model.FundingReversed:  nil,
}

// refundTransitions 是退款单的状态机。
//
// 它是 payment_refunds 上的独立聚合，与 paymentTransitions **不共用任何状态**——这就是
// paymentTransitions 里那句「退款不改 payments.status」的实现方式：钱退到哪一步只在这张
// 表上，而「这笔钱收没收到」永远只读 payments。
//
// 三条能走到的转换：
//
//	pending → processing   渠道收下了这次退款请求，但还没给结论（应答 PROCESSING/UNKNOWN，
//	                       或者调用超时）。之后由退款查询 worker 问出结论。
//	pending → succeeded    账户出资那一路：没有第三方要问，建单即完成（见 service/refund.go
//	                       里「豆那条路为什么不发渠道请求」那一段）。
//	pending → failed       渠道明确拒绝，或者发起之前就发现这笔退不了（可退余额不够）。
//	processing → succeeded 退款查询问出了 SUCCESS。
//	processing → failed    退款查询问出了 FAIL。
//
// 没有 succeeded/failed 的出边：钱退回去了、或者退不了，两件都是终态。一笔退款**不可重试**
// 也就是这个意思——重试是**同一张售后单**重发 CreateRefund，那会命中 after_sale_no 的幂等，
// 拿回的是同一张退款单，而不是新开一张。
var refundTransitions = map[string][]string{
	model.RefundPending: {
		model.RefundProcessing,
		model.RefundSucceeded,
		model.RefundFailed,
	},
	model.RefundProcessing: {
		model.RefundSucceeded,
		model.RefundFailed,
	},
	model.RefundSucceeded: nil,
	model.RefundFailed:    nil,
	// cancelled 今天没有写路径（见 model.RefundCancelled），挂在这里是为了让「它是个终态」
	// 这件事在这张表上是看得见的，而不是靠读代码去确认没人写它。
	model.RefundCancelled: nil,
}

// refundFundingTransitions 是退款出资行的状态机。
//
// 它比出资本身的 fundingTransitions **短得多**，因为退款这一侧没有预占那一步：一行要么
// 还没退、要么退了、要么退不成。三条边全是终态——一笔出资的退款失败之后不会自己再试，
// 要人看着那张退款单决定。
var refundFundingTransitions = map[string][]string{
	model.RefundFundingPending: {
		model.RefundFundingSucceeded,
		model.RefundFundingFailed,
	},
	model.RefundFundingSucceeded: nil,
	model.RefundFundingFailed:    nil,
}

// CanTransition 判断支付单状态能不能从 from 走到 to。
//
// from 不在表里（库里出现了一个这一版不认识的状态）时返回 false——与 order-service 同一条
// 约定：宁可拒绝一次操作让人来查，也不要让一个未知状态被推着往下走。
func CanTransition(from, to string) bool {
	return canTransition(paymentTransitions, from, to)
}

// CanFundingTransition 判断出资行状态能不能从 from 走到 to。
func CanFundingTransition(from, to string) bool {
	return canTransition(fundingTransitions, from, to)
}

// CanRefundTransition 判断退款单状态能不能从 from 走到 to。
func CanRefundTransition(from, to string) bool {
	return canTransition(refundTransitions, from, to)
}

// CanRefundFundingTransition 判断退款出资行的状态能不能从 from 走到 to。
func CanRefundFundingTransition(from, to string) bool {
	return canTransition(refundFundingTransitions, from, to)
}

// canTransition 是支付单与出资行两张状态机表共用的判定。
//
// 抽出来不是为了省几行，而是为了让「未知来源一律拒绝」这条规则只有一处。两张表分开写
// 判定，迟早有一张写成「未知状态也放行」，而那种错误不会在测试里出现，只会在线上出现
// 一个没人认识的状态。写法照抄 order-service/internal/service/state.go 的同名函数。
func canTransition(table map[string][]string, from, to string) bool {
	targets, known := table[from]
	if !known {
		return false
	}
	for _, target := range targets {
		if target == to {
			return true
		}
	}
	return false
}
