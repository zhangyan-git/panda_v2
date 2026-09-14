package service

import "github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"

// orderTransitions 是订单主状态机：从哪个状态能走到哪些状态。
//
// 它是**一处描述**，不是唯一的权威判定。真正的判定在写路径里：仓储在 FOR UPDATE 锁住
// 那一行之后再看一次状态——并发下只有锁内那次判断算数（两个请求同时付一笔单，谁先拿到
// 锁谁改，后到的看到的是 paid）。这张表的作用是让取消/支付这类入口在动手之前就能给出
// 一句说得清的拒绝，并且让「这个状态还能不能动」只有一个地方可以读。
//
// 这一版能真正走到的转换（下单闭环）：
//
//	pending_payment → paid（支付成功事件）
//	pending_payment → cancelled（用户或后台取消）
//	pending_payment → expired（超时关单扫描）
//
// 其余箭头是状态机的完整形状，驱动的服务还没到位：paid → completed 要等履约完成事件
// （fulfillment-service），paid → refunding → refunded 要等退款单（payment-service）。
// 它们写在这里是为了让「为什么 paid 不能直接取消」有一处可查，而不是让后来的人以为
// 这份状态机只有三条边。
var orderTransitions = map[string][]string{
	model.OrderStatusPendingPayment: {
		model.OrderStatusPaid,
		model.OrderStatusCancelled,
		model.OrderStatusExpired,
	},
	model.OrderStatusPaid: {
		model.OrderStatusCompleted,
		model.OrderStatusRefunding,
	},
	model.OrderStatusCompleted: {
		model.OrderStatusRefunding,
	},
	model.OrderStatusRefunding: {
		model.OrderStatusRefunded,
		// 退款被驳回：订单回到退款前的状态。这一版一律回 paid——完成了的订单被驳回后
		// 应当回 completed，要按退款单上记录的原状态恢复。退款单归 payment-service，
		// 售后这一版只到「审核通过」为止（见 after_sale_state.go），所以 paid/completed
		// 已经退过款的路径仍然走不到 refunding：订单主状态不会因为一次审核通过而改变。
		model.OrderStatusPaid,
	},
	// cancelled / expired / refunded 是终态，没有出边。
	model.OrderStatusCancelled: nil,
	model.OrderStatusExpired:   nil,
	model.OrderStatusRefunded:  nil,
}

// CanTransition 判断订单主状态能不能从 from 走到 to。
//
// from 不在表里（数据库里出现了一个这一版不认识的状态）时返回 false：宁可拒绝一次
// 操作让人来查，也不要让一个未知状态被推着往下走——那会让订单落进一个既不在状态机里、
// 也没人知道怎么处理的状态。
func CanTransition(from, to string) bool {
	return canTransition(orderTransitions, from, to)
}

// canTransition 是订单与售后两张状态机表共用的判定（售后那张见 after_sale_state.go）。
//
// 抽出来不是为了省几行，而是为了让「未知来源一律拒绝」这条规则只有一处：两张表分开写
// 判定，迟早有一张写成「未知状态也放行」，而那种错误不会在测试里出现，只会在线上出现
// 一个没人认识的状态。
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
