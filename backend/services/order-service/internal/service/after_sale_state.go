package service

import "github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"

// afterSaleTransitions 是售后单的状态机：从哪个状态能走到哪些状态。
//
// 与 orderTransitions（state.go）同一套约定：它是**一处描述**，不是权威判定。权威判定在
// 写路径里——仓储把售后单那一行 FOR UPDATE 锁住之后再判一次，并发下只有锁内那次算数。
// 这张表的作用是让「这张单还能不能审」在动手之前就有一句说得清的拒绝。
//
// 这一版能真正走到的转换只有三条（都是人推的，钱还没动）：
//
//	pending → approved（后台审核通过）
//	pending → rejected（后台审核驳回）
//	pending → cancelled（用户自己撤销申请）
//
// 其余箭头要等 payment-service：approved → refunding 发生在**退款单建成**的那一刻，
// refunding → refunded/failed 是渠道回调的结果。它们写在这里，是为了让后来的人看到
// 「通过了为什么还没有 refunding」时有处可查，而不是以为这张表只有三条边。
//
// 注意它和订单主状态机的关系：审核通过**不改** orders.status（见 service/after_sale.go）。
// 订单上的 refunding 含义是「退款单已经建了」，今天没人建得了退款单，标上去就再也回不来了。
var afterSaleTransitions = map[string][]string{
	model.AfterSaleStatusPending: {
		model.AfterSaleStatusApproved,
		model.AfterSaleStatusRejected,
		model.AfterSaleStatusCancelled,
	},
	model.AfterSaleStatusApproved: {
		model.AfterSaleStatusRefunding,
	},
	model.AfterSaleStatusRefunding: {
		model.AfterSaleStatusRefunded,
		model.AfterSaleStatusFailed,
	},
	// rejected / cancelled / refunded / failed 是终态，没有出边：被驳回、已撤销、
	// 退款失败这三种都意味着「钱没出去」，用户要再退就重新申请一张新的售后单——
	// 复用同一张会让一张单的 status 讲两个故事。
	model.AfterSaleStatusRejected:  nil,
	model.AfterSaleStatusCancelled: nil,
	model.AfterSaleStatusRefunded:  nil,
	model.AfterSaleStatusFailed:    nil,
}

// CanTransitionAfterSale 判断售后单能不能从 from 走到 to。
//
// from 不在表里时返回 false：宁可拒绝一次操作让人来查，也不要让一个未知状态被推着走。
func CanTransitionAfterSale(from, to string) bool {
	return canTransition(afterSaleTransitions, from, to)
}
