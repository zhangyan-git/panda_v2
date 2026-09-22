package dto

import (
	"encoding/json"
	"time"
)

// EventVersion 是本服务**发出**的事件版本。所有事件共用它。
//
// 与 order-service 的 eventVersion 同一个取值、同一个用法：版本是对**消费方**的承诺，
// 不是我们自己的迭代号。改变了某个字段的含义就要动它。
const EventVersion = "1"

// 本服务发出的事件类型。事件类型同时是 RabbitMQ 的路由键（messaging 用事件类型做 routeKey），
// 所以这几个字符串改一个就要动一次交易所绑定。
//
// ⚠️ **今天一个消费者都没有**：券要发（coupon-service）、别的域要知道会员变了，都还没有落地
// 的实现。事件照样要落下来，理由与订单域那几条一样——「会员确实变了却没有留下事件」
// 这件事一旦发生就补不回来，而它恰恰是**唯一**能让别的域知道会员状态变了的途径：会员库没有
// 外键能指过来，别的服务手里只有一个 user_id。
//
// 名字用「发生过的事」而不是「要别人做的事」：membership.activated 是「这个人成了会员」，
// 不是「请去发券」。后者会把本域的策略塞进下游，而发券的张数与模板是**成交快照**决定的
// （见 memberships 的 member_price_coupons_per_period），下游拿事件里的快照自己判就行。
const (
	// EventMembershipActivated 是「这个人第一次成为会员」（memberships 上新写了一行）。
	EventMembershipActivated = "membership.activated"
	// EventMembershipRenewed 是「有效期被往后叠了一段」（续费、或后台调整有效期）。
	EventMembershipRenewed = "membership.renewed"
	// EventMembershipExpired 是「到期了」（扫描把 active 翻成 expired）。
	//
	// 它**不是**「权益没了」的唯一信号：用户看的是 expire_at 过没过，不是 status。这条事件
	// 是给下游做记录与提醒用的。
	EventMembershipExpired = "membership.expired"
	// EventMembershipRevoked 是「会员被撤销 / 冻结」——后台人工动作，不可逆或需要人工解冻。
	//
	// 它进审计（admin.operation.logged）之外**还要发一条**：审计答「谁动了手」，这条答
	// 「会员因此变成了什么样」，而下游要的是后者。
	EventMembershipRevoked = "membership.revoked"
	// EventMembershipCampaignClaimed 是「有人扫门店码领了会员」。
	//
	// **它是券那一半的触发源**：一次领取送两样东西——N 天会员（本服务自己发，落在
	// membership_campaign_claims 那一行上）与 M 张券（coupon-service 发）。发券要用券库的
	// 账本，本服务一个字都不能替它写，所以这件事必须发出去；而「会员确实领到了却没有留下
	// 事件」一旦发生就补不回来——本库没有外键能指到券库去。
	//
	// **没配券的活动也发**：事件名说的是「发生过的事」（有人领了），不是「请去发券」。载荷里
	// 券那两个字段为空就是「这场活动只送会员天数」，下游看见空值什么都不做——发一条「没有券」
	// 的事件比在生产者这里分支更省事，也让将来别的域（比如统计）能收到全部的领取。
	EventMembershipCampaignClaimed = "membership.campaign.claimed"
)

// EventOrderPaid 是本服务**消费**的唯一一个事件类型。
//
// 它是 order-service 发出的路由键，逐字照抄那边 dto 里的常量。改一个字就等于退订：消息照样
// 发出来，只是再也不会进本服务的队列，而**这不会有任何报错**——队列绑定不上时 RabbitMQ 不会
// 通知任何人。
const EventOrderPaid = "order.paid"

// EventAgreementSigned / EventAgreementTerminated 是本服务**消费**的另外两条，都由
// payment-service 发出（委托代扣协议的变更）。
//
// **逐字照抄那边 dto/agreement_event.go 里的常量**，理由与 EventOrderPaid 一字不差：改一个字
// 就等于退订，而退订不会报错——队列绑不上时 RabbitMQ 不通知任何人，症状是「用户签了约，
// 后台那条订阅永远停在待签约」，而两边日志里都干干净净。
//
// ⚠️ 这两个键是**三段**（payment.agreement.signed），`payment.*` 那个通配**匹配不到它们**
// ——topic 交换机里 `*` 只匹配恰好一段。RABBITMQ_ROUTING_KEY 要把两条都列全（逗号分隔），
// 见 deploy/compose/dev/docker-compose.yml 里本服务那一段。
const (
	// EventAgreementSigned 是「协议生效了，以后可以扣款」。
	EventAgreementSigned = "payment.agreement.signed"
	// EventAgreementTerminated 是「协议结束了」（用户解约，或渠道说它没了）。
	//
	// 它是终态：一份解约的协议不会再签回来，所以收到它的一方把订阅直接收掉，不必等反向事件。
	EventAgreementTerminated = "payment.agreement.terminated"
	// EventAgreementChargeSucceeded / EventAgreementChargeFailed 是**某一期扣款**的结果，
	// 同样由 payment-service 发出，取值逐字照抄那边 dto/agreement_event.go。
	//
	// 与上面那两条的关系是「说的是两件事」：那两条说协议（签了、解了），这两条说钱（这一期扣到了
	// 没有）。协议 active 而某一期没扣成，是完全正常的一种组合——正因如此，它们不能复用协议那两条
	// 事件的词表：把「这一期没扣到」翻成协议 terminated 会**把一份还在的授权从本地划掉**。
	//
	// 路由键同样是三段，`payment.*` 那个通配匹配不到，RABBITMQ_ROUTING_KEY 要把四条都列全。
	EventAgreementChargeSucceeded = "payment.agreement.charge_succeeded"
	EventAgreementChargeFailed    = "payment.agreement.charge_failed"
)

// AgreementChargeEventPayload 是 payment-service 那份同名结构在本服务这一侧的镜像。
//
// 规矩与另外两份镜像一样：消费时用 DisallowUnknownFields，**多发一个字段就整条解不开**，所以
// 那边有的字段这里一个都不能少——哪怕本服务今天不读它（ProviderTransactionID 就是一例：成功
// 那条靠它做幂等，失败那条只往流水里抄一份）。
//
// 一条事件只说一期。bizPeriod 是那一期的标识，而它在扣成之前不推进，所以重投拿到的也是同一个值。
type AgreementChargeEventPayload struct {
	// AgreementID 是 payment_agreements.id，对应 membership_subscriptions.agreement_id。
	// **按它命中那一行订阅**，别的字段都定位不到。
	AgreementID string `json:"agreementId"`
	// AgreementNo 是我们自己的协议号（= 渠道侧的 contract_code），给人看与排查用。本服务不按它
	// 定位——订阅上的 contract_code 就是它，但事件里那份没有经过本域的写入，不该拿来当键。
	AgreementNo string `json:"agreementNo"`
	// UserID 是签约用户。与订阅上的 user_id 对不上时**什么都不做**（见 service.handleChargeEvent）：
	// 宁可漏一次续期，也不能把别人的钱续到这个人的会员上。
	UserID string `json:"userId"`
	// BizPeriod 是这一期的标识（由本服务从订阅的 next_charge_at 派生，见 model.ChargePeriod）。
	BizPeriod string `json:"bizPeriod"`
	// Amount 是这一期的金额，单位为分。本库不存金额（金额的权威在支付域），它只进变更流水的
	// metadata，答「这一期扣了多少钱」。
	Amount int64 `json:"amount"`
	// ProviderTransactionID 是渠道侧的流水号。**成功那条非空**，并且就是那条续期流水的幂等键；
	// 失败那条可能是空的（渠道拒一笔时不给号）。
	ProviderTransactionID string `json:"providerTransactionId"`
	// Status 是这一期**变更之后**的状态，取 dto.ChargeStatusSucceeded 或 ChargeStatusFailed。
	//
	// 与协议事件一样给状态而不是「这是哪次变更」：事件名已经说了那次变更，而本域要判的是「现在
	// 是什么」。这一条尤其要紧——两条事件共用一个载荷，不读 Status 的话失败那条会被当成成功。
	Status string `json:"status"`
	// FailureCode / FailureMessage 只在失败那条上有值（渠道给的原因）。它们进变更流水的 metadata，
	// **不是给用户看的文案**（用户看到的那句话由本域组织，而这一刀还没有那个界面）。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// AgreementEventPayload 是 payment-service 那份 AgreementEventPayload 在本服务这一侧的镜像。
//
// 与 OrderPaidEventPayload 同一条规矩：**镜像而不是共享**（跨 module 不能互相 import），
// 消费时用 DisallowUnknownFields，所以生产者多发一个字段、这里少镜像一个，整条消息就进死信。
// 那边在注释里写明「这份还没有镜像，写消费方的人负责把字段对上」——这个结构体就是那一步。
//
// 五个字段都要，**一个都不能少**：AgreementID 是命中订阅的键，UserID 是对一遍「这条协议真是
// 这个人的」，Status 是目标状态，另外两个是人读的号。标成「用不上就不镜像」的那个字段正是
// DisallowUnknownFields 会炸的地方。
type AgreementEventPayload struct {
	// AgreementID 是 payment_agreements.id，对应 membership_subscriptions.agreement_id。
	AgreementID string `json:"agreementId"`
	// AgreementNo 是我们自己的协议号（同时就是交给渠道的 contract_code）。
	AgreementNo string `json:"agreementNo"`
	// ContractNo 是渠道侧的协议号（微信的 contract_id）。
	//
	// **本服务不存它**（镜像了但不用）：订阅上的 contract_code 装的是我们自己的商户协议号
	// （= AgreementNo），回渠道查协议、解约、对账用的都是那一个。两者长得像，混用会得到一句
	// 看不懂的错，所以这里标清楚（见 dto.AgreementState.ContractNo 那段）。
	ContractNo string `json:"contractNo"`
	// UserID 是签约用户。与订阅上的 user_id 对不上时**不动那一行**：宁可漏改也不能改错人的会员。
	UserID string `json:"userId"`
	// Status 是协议**变更之后**的状态：active 或 terminated。
	Status string `json:"status"`
}

// OrderPaidEventPayload 是 order-service 的 `order.paid` 在本服务这一侧的镜像。
//
// **镜像而不是共享**：两边各写一份结构体与 json tag，消费时用 DisallowUnknownFields
// （见 service.HandleEvent）。共享一个包会让 producer 加一个字段就悄悄改掉 consumer 的
// 行为，而跨服务契约的每一次漂移都该在联调时炸出来。这与 account-service 消费
// order.completed 是同一套写法。
//
// **Membership 这一段已经接上了**（2026-09）。order-service 发的是
// `{orderId, orderNo, userId, paidAmount, paymentNo, paymentMethod, fundings, paidAt, membership?}`，
// 最后那一段只有会员行才有。它在那边由 repository.SettlePayment 从
// order_lines.membership_plan_snapshot **原样搬过来**（不重新拼、不回头现查套餐），所以下面
// 这个结构体与那边 dto.MembershipPlanSnapshot 是**同一份内容的两处声明**：任何一边改字段，
// 另一边必须在同一次改动里跟着改，否则 DisallowUnknownFields 会让整条消息进死信。
//
// 字段缺席时**ack，不报错**：那是「这不是一张买会员的单」，而绝大多数订单都是这样
// （见 service.handleOrderPaid）。这一段是本服务开通与续费**唯一**的触发源——买会员是订单
// 库里的一行，本库没有订单表——所以它没带上来时没有任何补救路径，只能靠那边发对。
type OrderPaidEventPayload struct {
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	UserID  string `json:"userId"`
	// PaidAt 是支付成功时刻，RFC3339。用它而不是处理时刻：补投一条上周的事件时，
	// 会员的 start_at 该是上周（见 service 里的说明）。
	PaidAt string `json:"paidAt"`
	// PaymentNo / PaymentMethod / Fundings / PaidAmount 本服务**不读**，但必须出现在这里：
	// DisallowUnknownFields 要求镜像结构覆盖生产者发过来的每一个字段，否则整条消息解不开、
	// 进死信。它们不是「顺便收下」，是「镜像的代价」——想少一个就得让 order-service 也少发一个。
	PaidAmount    int64             `json:"paidAmount"`
	PaymentNo     string            `json:"paymentNo"`
	PaymentMethod string            `json:"paymentMethod"`
	Fundings      []json.RawMessage `json:"fundings"`
	// Membership 是这一单买的那份会员套餐的快照，**只有会员行才有**。没有它 = 这一单
	// 不产生会员变更。
	Membership *OrderMembershipSnapshot `json:"membership"`
	// StoreID 是这一单成交所在的门店，**可能为 null**（小程序线上买会员就没有门店）。
	//
	// 它喂给 memberships.store_id，也就是**归属门店**——「这个人是谁拉来的」。规则见
	// model.Membership.StoreID：第一次成为会员那一刻固化，续费不覆盖，会员过期后重新开通才
	// 可变。订单带上门店，正是规则第 3 条例外里那条「在门店买咖啡时同时开通会员」。
	//
	// 用 *string 而不是 string：`null` 与「没有这个字段」在这里是同一件事（没门店），但**下标
	// 一行注释是为了让下一个人看清**——生产者有没有发这个字段，是 DisallowUnknownFields 那套
	// 契约里唯一需要人肉确认的部分。
	StoreID *string `json:"storeId"`
}

// OrderMembershipSnapshot 是订单行上那份会员套餐快照在事件里的形状。
//
// 它逐字对应 membership_plans 的一组列，因为它是**下单那一刻**套餐长什么样的拷贝：套餐后来
// 改价、改时长、下架都不能改变已经买了的人拿到什么（memberships 上那一组快照列就是这么来的，
// 这里是它们的来源）。
//
// 金额与时长都从快照取，**不去按 PlanID 现查套餐**：现查等于让一次后台编辑改写所有在途订单
// ——用户付的是旧价，本服务却按新时长开权益。
type OrderMembershipSnapshot struct {
	// PlanID / PlanCode / PlanName 是套餐的身份。PlanID 用来建外键（本库内），
	// PlanCode 与 PlanName 存进快照列。
	PlanID   string `json:"planId"`
	PlanCode string `json:"planCode"`
	PlanName string `json:"planName"`
	// PriceCents 是这一单实付的套餐价（分）。本服务记进 membership_changes.metadata 备查，
	// 不做任何定价——钱是订单域的事。
	PriceCents int64 `json:"priceCents"`
	// Period / PeriodCount 是这一单买到的时长（month/1 或 year/1）。续期按日历加。
	Period      string `json:"period"`
	PeriodCount int32  `json:"periodCount"`
	// AutoRenew 是套餐层面的「这个产品要签代扣」，不是用户开关（那个在 memberships 上）。
	AutoRenew bool `json:"autoRenew"`
	// MemberPriceMode / MemberPriceCouponTemplateID / MemberPriceCouponsPerPeriod 是会员价
	// 路径的快照，原样抄进 memberships 的三个同名快照列——发券按那份记录发，不按套餐现查。
	MemberPriceMode             string `json:"memberPriceMode"`
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod"`
}

// MembershipActivatedEvent / MembershipRenewedEvent / MembershipExpiredEvent /
// MembershipRevokedEvent 是发出去的四条事件体。
//
// 四条共用一个形状而不是各写各的字段：下游要回答的都是同一串问题（谁、哪条会员、什么时候、
// 从什么变成什么），分四套只会让下游写四个几乎一样的结构体。**事件类型已经说明是哪一种了**
// （它就是路由键），载荷里再放一个 eventType 字段是冗余，而冗余的那一份迟早会与真身不一致。
type MembershipChangedEvent struct {
	MembershipID string `json:"membershipId"`
	UserID       string `json:"userId"`
	PlanCode     string `json:"planCode"`
	PlanName     string `json:"planName"`
	// MemberPriceMode 是**成交快照**上的那条路径。下游据此决定「这个人是怎么享会员价的」
	// ——coupon 模式下要发券，auto 模式下什么都不用做。
	MemberPriceMode string `json:"memberPriceMode"`
	// 下面三列只在 MemberPriceMode=coupon 时有值，是发券的完整依据（模板、张数）。
	// 同样来自快照，不是现查套餐。
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId,omitempty"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod,omitempty"`
	// Status / ExpireAt 是这条事件发生后会员的新样子。
	Status   string    `json:"status"`
	ExpireAt time.Time `json:"expireAt"`
	// OrderID 在开通与续费上有值，到期与撤销上没有。
	//
	// ⚠️ **下游拿它判「这次变更是不是钱换来的」**：本服务在后台人工调整有效期时也发
	// membership.renewed，那条路上 OrderID 是空的。coupon 模式的下游（发会员价券）必须靠它
	// 把「支付驱动的续期」与「后台改了个日期」分开，否则后台每改一次有效期就白送一批券。
	OrderID string `json:"orderId,omitempty"`
	// OccurredAt 是**这件事发生的时刻**，不是处理时刻：补投一条旧事件时下游看到的时间
	// 应该还是原来那个，否则「这个月开了多少会员」的统计会随一次重投而变。
	OccurredAt time.Time `json:"occurredAt"`
}

// CampaignClaimedEvent 是 EventMembershipCampaignClaimed 的事件体。
//
// 它把「这次领取送出去什么」讲全：券那两个字段是**领取那一刻的快照**（活动后来改了券的
// 配置，已经领过的那次不受影响），张数配在活动上，而券怎么发、发失败怎么办是券服务的事
// ——本服务只承诺，不执行。
//
// **字段一改就要两边一起改**：coupon-service 那边有一份逐字镜像的结构体，用
// DisallowUnknownFields 消费，少镜像一个字段整条消息就解不开（那就是一次静默的「没有券」）。
type CampaignClaimedEvent struct {
	// ClaimID 是 membership_campaign_claims 那一行，也是券服务发券的幂等键——一个人一场活动
	// 只能领一次，重投时靠它命中上一次的批次。
	ClaimID    string `json:"claimId"`
	CampaignID string `json:"campaignId"`
	UserID     string `json:"userId"`
	// StoreID 是活动门店，也是这次领到的会员的归属门店。券服务用它**把券锁在这家店**
	// （老系统同一口径：活动券只能在活动门店核销）。
	StoreID  string `json:"storeId"`
	PlanName string `json:"planName"`
	GiftDays int32  `json:"giftDays"`
	// CouponTemplateID / CouponCount 是这次承诺发的券。**空 = 这场活动没配券**（只送会员
	// 天数），下游看见空值什么都不做——这不是错误，活动上那两列本来就是可选的。
	CouponTemplateID string    `json:"couponTemplateId,omitempty"`
	CouponCount      int32     `json:"couponCount,omitempty"`
	OccurredAt       time.Time `json:"occurredAt"`
}
