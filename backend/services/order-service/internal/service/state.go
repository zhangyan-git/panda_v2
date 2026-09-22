package service

import "github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"

// orderTransitions 是订单主状态机：从哪个状态能走到哪些状态。
//
// 它是**一处描述**，不是唯一的权威判定。真正的判定在写路径里：仓储在 FOR UPDATE 锁住
// 那一行之后再看一次状态——并发下只有锁内那次判断算数（两个请求同时付一笔单，谁先拿到
// 锁谁改，后到的看到的是 paid）。这张表的作用是让取消/支付这类入口在动手之前就能给出
// 一句说得清的拒绝，并且让「这个状态还能不能动」只有一个地方可以读。
//
// 这一版能真正走到的转换：
//
//	pending_payment → paid（支付成功事件）
//	pending_payment → cancelled（用户或后台取消）
//	pending_payment → expired（超时关单扫描）
//	paid → completed（后台标记完成，见 service/complete.go）
//	paid/completed → refunding（售后审核通过、退款单建成）
//	refunding → refunded（整单退款成功的事件）
//	refunding → paid/completed（退款失败：回到退款前那个状态）
//
// 三条出边里两条指向同一个状态不是冗余：**回到哪个状态要看当初是从哪来的**。原状态由
// 仓储从状态流水里读回来（见 repository.orderStatusBeforeRefunding），一条已经取过杯的
// 订单退失败之后应当还停在 completed，而不是被降回 paid。
//
// 只有整单退成功才落到 refunded：按行退（退一杯）的订单还活着，剩下的行还要履约。
// 那个判断在 repository.AdvanceRefund 里，判据是售后单自己的 scope。
//
// paid → completed 那条边**本该**由履约完成事件驱动，而 fulfillment-service 还没建；
// 在它到位之前，后台的「标记完成」是这条边唯一的触发源——两者发的是同一个
// order.completed，所以履约接上来时下游一个字都不用改。
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
		// 退款失败（渠道明确拒绝）：钱没退成，单回到退款前。
		model.OrderStatusPaid,
		// 同上，只是这一单当初已经取过杯了。少了这一条，一次失败的退款会把一张完成了的
		// 订单降级成 paid——那是「还没做完」的意思，与事实正好相反。
		model.OrderStatusCompleted,
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
