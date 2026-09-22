package dto

// 本文件是 order-service 那份 internal/dto/order_event.go 的**逐字镜像**。
//
// 为什么要抄一份而不是共享：两个服务是各自的 Go module，事件契约在这个仓库里的形状
// 就是「发送方与消费方各持一份 Go struct + json tag」（见 order-service 消费
// PaymentEventPayload 的做法）。抄错一个字段名不会编译失败，但会在消费时被
// DisallowUnknownFields 当场炸出来——那正是我们要的联调信号，比静默忽略强。
// 改这里就必须同步改 order-service 那一份。

// EventOrderCompleted 是订单完成事件的类型，也是 RabbitMQ 的 routing key（信封上的
// EventType 就是这两者）。订单完成有两条来路，但只有这一个类型——后台的「标记完成」与
// 将来的履约完成事件发的是同一条消息，所以这里的代码一个字都不用改。
const EventOrderCompleted = "order.completed"

// OrderCompletedEventPayload 是 order.completed 的事件体。
//
// 订单完成有两条来路：后台的「标记完成」（人工干预）与将来履约服务的完成事件。两者
// 发的是同一个事件类型、同一个形状，所以发放口径一个字不用改。
type OrderCompletedEventPayload struct {
	OrderNo string `json:"orderNo"`
	OrderID string `json:"orderId"`
	UserID  string `json:"userId"`
	// FinishedAtUnix 是订单的完成时间。流水用它的原因见 fortune_card_entries.occurred_at
	// 的列注释：补投一条旧事件时，NOW() 会把上周完成的那单记成刚才。
	FinishedAtUnix int64 `json:"finishedAtUnix"`
	// FortuneCards 是这一单要记进福卡流水的每一笔发放。空 = 这单不送福卡。
	FortuneCards []OrderFortuneGrant `json:"fortuneCards"`
}

// OrderFortuneGrant 是一条要记进福卡流水的发放。
//
// 构成由订单域拆好再发过来：承诺福卡的快照在订单库里，账户域既不解析那个 JSON，也不
// 持有发放规则（方案 5.6「Account 只拥有账户和账变数据」）。
type OrderFortuneGrant struct {
	// Kind 是 base 或 bonus。
	Kind string `json:"kind"`
	// CampaignID / CampaignName 只有 bonus 有。
	CampaignID   string `json:"campaignId"`
	CampaignName string `json:"campaignName"`
	// Amount 恒 > 0。
	Amount int64 `json:"amount"`
	// EntryKey 是这一笔的幂等键（形状见 migrations/account/001 的列注释）。
	EntryKey string `json:"entryKey"`
}

// 售后这一段的五个事件类型。与 order-service 的 repository.EventAfterSale* 逐字一致。
//
// 后两个是退款链的收口（refunding 只有这两个出口），本服务拿它们做最后半步：
// refunded ⇒ 追回福卡，refund_failed ⇒ 解冻。两条都要绑，绑错一条不会报错，
// 只会安静地把卡扣掉或者永远冻着。
const (
	EventAfterSaleApplied      = "order.after_sale.applied"
	EventAfterSaleReviewed     = "order.after_sale.reviewed"
	EventAfterSaleCancelled    = "order.after_sale.cancelled"
	EventAfterSaleRefunded     = "order.after_sale.refunded"
	EventAfterSaleRefundFailed = "order.after_sale.refund_failed"
)

// 售后单在 reviewed / cancelled 事件里的状态值，与 order-service 的 model.AfterSaleStatus*
// 逐字一致。本服务只认其中一个：**被驳回的那条**——驳回意味着钱不退，冻着的卡凭什么锁着。
const (
	AfterSaleStatusApproved  = "approved"
	AfterSaleStatusRejected  = "rejected"
	AfterSaleStatusCancelled = "cancelled"
)

// AfterSaleAppliedEventPayload 是 order.after_sale.applied 的事件体。
//
// 本服务读它是为了**冻结**：用户提交退款申请的那一刻起，这一单的福卡不能拿去抽奖。
// 与 order.completed 是同一个方向的两件事——那边加，这边锁住加进来的那些。
type AfterSaleAppliedEventPayload struct {
	AfterSaleID  string  `json:"afterSaleId"`
	AfterSaleNo  string  `json:"afterSaleNo"`
	OrderID      string  `json:"orderId"`
	OrderNo      string  `json:"orderNo"`
	UserID       string  `json:"userId"`
	Scope        string  `json:"scope"`
	OrderLineID  *string `json:"orderLineId"`
	RefundAmount int64   `json:"refundAmount"`
	// FortuneCardEntryKeys 是要冻住的发放幂等键，由订单域按退款范围拆好：
	// 退饮品行给 base 那张，退加购行给 bonus:{campaignId} 那张，整单退给全部。
	// 空 = 这一单没承诺福卡（或退的是不送福卡的会员套餐），不建冻结行——空是常态。
	//
	// 本服务**不去重算**这个分层：哪张卡是哪一行送的，只有订单域知道。
	FortuneCardEntryKeys []string `json:"fortuneCardEntryKeys"`
}

// AfterSaleReviewedEventPayload 是 order.after_sale.reviewed 的事件体。
//
// **驳回 ⇒ 解冻福卡；通过 ⇒ 什么都不做。**
//
//	rejected  解冻福卡——不退就不该继续冻着。
//	approved  空分支。只代表「同意退」，钱还没出去（退款单紧接着才向渠道发起，成没成
//	          由 order.after_sale.refunded / .refund_failed 回来）。福卡此时既不该解冻
//	          （用户还没拿到钱）也不该收走（钱还没退），豆更不能还（退款可以失败）。
//
// 所以本服务在「钱的两种结局」上各做一个动作，中间那一拍不动任何一边：退成 ⇒ 追回福卡
// + 冲正豆，没退成 ⇒ 解冻福卡、豆原样不动（它从未被动过）。这条事件因此只承担「驳回」
// 这一种动作，refundAmount 等字段收下是为了与 order-service 那份逐字一致，无人读取。
type AfterSaleReviewedEventPayload struct {
	AfterSaleID  string  `json:"afterSaleId"`
	AfterSaleNo  string  `json:"afterSaleNo"`
	OrderID      string  `json:"orderId"`
	OrderNo      string  `json:"orderNo"`
	UserID       string  `json:"userId"`
	Status       string  `json:"status"`
	Action       string  `json:"action"`
	Scope        string  `json:"scope"`
	OrderLineID  *string `json:"orderLineId"`
	RefundAmount int64   `json:"refundAmount"`
}

// AfterSaleRefundEventPayload 是 order.after_sale.refunded / order.after_sale.refund_failed 的
// 事件体，与 order-service 那一份逐字镜像。
//
// 本服务读它做两件事：
//
//	refunded       钱退成了 → 追回：解冻 + 冲正那几笔发放（余额真的少掉），
//	                          并按 refundAmount 冲正这一单扣掉的咖啡豆
//	refund_failed  钱没退成 → 解冻：卡原样放回去，余额一分不动；豆也一分不动
//
// 福卡那两件事只认 afterSaleNo：要冲哪几笔发放、总共几张，都在冻结行上（entry_keys 与
// amount）——订单域在 applied 那一刻已经拆好送过来了，这边不重算。豆那件事要用
// refundAmount 与 orderId。其余字段收下不用，是排查时要看的上下文。
type AfterSaleRefundEventPayload struct {
	AfterSaleID    string `json:"afterSaleId"`
	AfterSaleNo    string `json:"afterSaleNo"`
	OrderID        string `json:"orderId"`
	OrderNo        string `json:"orderNo"`
	UserID         string `json:"userId"`
	RefundNo       string `json:"refundNo"`
	RefundAmount   int64  `json:"refundAmount"`
	RefundedAtUnix int64  `json:"refundedAtUnix"`
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// AfterSaleCancelledEventPayload 是 order.after_sale.cancelled 的事件体：用户撤销了自己那张
// 还没被审核的申请。本服务读它是为了**解冻**——撤销之后用户手上再没有任何能解开它的动作，
// 这条消息就是唯一的信号。
type AfterSaleCancelledEventPayload struct {
	AfterSaleID string `json:"afterSaleId"`
	AfterSaleNo string `json:"afterSaleNo"`
	OrderID     string `json:"orderId"`
	OrderNo     string `json:"orderNo"`
	UserID      string `json:"userId"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
}
