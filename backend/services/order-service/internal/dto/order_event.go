package dto

// 订单域发出去的领域事件体。用 Go struct + json tag 描述，不用 proto：仓库里的事件契约
// 一直是这个形状（信封是 messaging.Envelope，EventType 就是 routing key），
// 单独为它引一套生成链路只会多一处会漂的东西。
//
// account-service 的手工镜像 struct 与本文件逐字对应（那边也叫 dto/order_event.go）。
// 两边不一致的后果不是编译错，而是**运行时的静默丢件**：消费方开了
// DisallowUnknownFields，多发或改名一个字段会让整条消息进死信，一张福卡都不发。

// OrderCompletedEventPayload 是 order.completed 的事件体：一单完成了，以及这一单要发多少
// 福卡。人工标记完成与将来的履约完成事件**共用这一个类型**——发放口径不因为触发源不同
// 而不同，这是「标记完成」这个临时入口敢先上线的全部理由。
type OrderCompletedEventPayload struct {
	OrderNo string `json:"orderNo"`
	OrderID string `json:"orderId"`
	UserID  string `json:"userId"`
	// FinishedAtUnix 是订单的完成时刻（秒）。消费方拿它当流水时间，而不是收到消息的
	// 那一刻：补投一条上周完成的事件时，明细页上的顺序必须还是上周。
	FinishedAtUnix int64 `json:"finishedAtUnix"`
	// FortuneCards 空表示这一单不送福卡——那是常态，不是异常。
	FortuneCards []OrderFortuneGrant `json:"fortuneCards"`
}

// OrderFortuneGrant 是一条要记进福卡流水的发放。
//
// 构成由**订单域**拆好（承诺快照是订单的事实），账户域不解析那个 JSON、不持有发放规则
// ——方案 5.6：account-service 只拥有账户和账变数据。
type OrderFortuneGrant struct {
	// Kind 取 FortuneGrantKindBase / FortuneGrantKindBonus。
	Kind string `json:"kind"`
	// CampaignID / CampaignName 只有 bonus 有。
	CampaignID   string `json:"campaignId"`
	CampaignName string `json:"campaignName"`
	// Amount 恒 > 0。消费方把它当「加几张」，不是有符号的变动量。
	Amount int64 `json:"amount"`
	// EntryKey 是幂等键，由订单域生成：它才知道这一单的活动是哪一次。
	// 格式见 repository 里的 grantKey 函数。
	EntryKey string `json:"entryKey"`
}

// 发放种类。字符串与 account-service 的 model.GrantKind* 一致——那边认的就是这两个值，
// 认不出来的 kind 会让整条消息进死信（它宁可报错也不退回 base，见那边的 grantTitle）。
// 所以加第三种发放要**两个服务一起改**，不是这里加一个常量就完事。
const (
	FortuneGrantKindBase  = "base"
	FortuneGrantKindBonus = "bonus"
)

// AfterSaleAppliedEventPayload 是 order.after_sale.applied 的事件体：用户提交了一张退款申请。
//
// 除退款链本身（方案 7.4，等 payment-service）之外，它今天还有一个真实消费者：
// account-service 拿 FortuneCardEntryKeys 冻结这一单的福卡——**申请即冻**，从这一刻起
// 这些卡不能拿去抽奖，直到申请被驳回或用户撤销。
type AfterSaleAppliedEventPayload struct {
	AfterSaleID  string  `json:"afterSaleId"`
	AfterSaleNo  string  `json:"afterSaleNo"`
	OrderID      string  `json:"orderId"`
	OrderNo      string  `json:"orderNo"`
	UserID       string  `json:"userId"`
	Scope        string  `json:"scope"`
	OrderLineID  *string `json:"orderLineId"`
	RefundAmount int64   `json:"refundAmount"`
	// FortuneCardEntryKeys 是这次要冻住的发放幂等键，由订单域按 scope 拆好：
	// 退饮品行给 base 那张，退加购行给 bonus:{campaignId} 那张，整单退给全部；
	// 会员套餐与「这一单没承诺福卡」都给空——空是常态，不是异常。
	//
	// 给键而不是给张数：申请可能早于发放（paid 状态就允许申请退款，见 repository.ApplyAfterSale），
	// 那时流水还不存在，但键已经定了——账户域按这些键反查，发放落库时把张数补进来。
	FortuneCardEntryKeys []string `json:"fortuneCardEntryKeys"`
}

// AfterSaleReviewedEventPayload 是 order.after_sale.reviewed 的事件体：管理员通过了或驳回了。
//
// 通过**不解冻**：通过了只代表「同意退」，钱还没出去（退款单是审核通过之后紧接着发起的，
// 成没成由 payment.refund.* 回来）。冻结一直保持到退款成功、追回福卡那一刻。驳回则立刻
// 解冻——钱不退，卡凭什么锁着。
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

// AfterSaleCancelledEventPayload 是 order.after_sale.cancelled 的事件体：用户撤销了自己那张
// 还没被审核的申请。存在理由只有一个——**把冻结的福卡放回去**。撤销之后用户手上再没有
// 任何能解开它的动作了，所以这条事件丢了就等于卡永远锁着；账户域那边因此把它做成幂等的
// no-op（找不到冻结行也返回成功），让重投是安全的。
// AfterSaleRefundEventPayload 是 order.after_sale.refunded / order.after_sale.refund_failed 的
// 事件体：钱退成了，或者明确没退成。它是退款这条链在订单侧的**收口**——`refunding` 只有
// 这两个出口，而结论只有 payment-service 知道。
//
// 两个类型共用一个形状（学 payment-service 发 payment.refund.* 的做法：失败那条里成功相关
// 的字段为空，而不是两个几乎一样的结构体）。分成两个类型而不是一条带 status 的，是因为
// 消费方要做的两件事是**相反**的：退成 ⇒ 追回福卡（余额真的少掉），没退成 ⇒ 解冻
// （卡原样放回去，余额不动）。绑错一条不会报错，只会安静地把卡扣掉或者留着。
type AfterSaleRefundEventPayload struct {
	AfterSaleID string `json:"afterSaleId"`
	AfterSaleNo string `json:"afterSaleNo"`
	OrderID     string `json:"orderId"`
	OrderNo     string `json:"orderNo"`
	UserID      string `json:"userId"`
	// RefundNo 是 payment-service 的退款单号，排查用的值引用。
	RefundNo string `json:"refundNo"`
	// RefundAmount 是这一笔退款的总额（分）。**失败事件里也是同一个数**，不是 0。
	RefundAmount int64 `json:"refundAmount"`
	// RefundedAtUnix 是钱退回去的时刻（秒）。0 表示事件没带，消费方用收到的那一刻。
	RefundedAtUnix int64 `json:"refundedAtUnix"`
	// FailureCode / FailureMessage 只在失败事件里有值。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// AfterSaleCancelledEventPayload 是 order.after_sale.cancelled 的事件体：用户撤销了自己那张
// 还没被审核的申请。存在理由只有一个——**把冻结的福卡放回去**。撤销之后用户手上再没有
// 任何能解开它的动作了，所以这条事件丢了就等于卡永远锁着；账户域那边因此把它做成幂等的
// no-op（找不到冻结行也返回成功），让重投是安全的。
type AfterSaleCancelledEventPayload struct {
	AfterSaleID string `json:"afterSaleId"`
	AfterSaleNo string `json:"afterSaleNo"`
	OrderID     string `json:"orderId"`
	OrderNo     string `json:"orderNo"`
	UserID      string `json:"userId"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
}
