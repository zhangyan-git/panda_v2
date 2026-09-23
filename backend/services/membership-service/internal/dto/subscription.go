package dto

import "time"

// 连续包月订阅的对外形状（后台只读 + 一个「取消」）。
//
// 与 model.Subscription 不是一一对应：两个派生字段（签约场景、首月支付）在这里算出来，三个
// 来源 id 不外露。理由是那两列在页面上就是两个字符串，而把三个 id 也发出去只会让前端多一遍
// 同样的判断——判断写两处，迟早两处不一样。

// 签约场景。库里没有这一列：它由三个来源 id 现推（见 SignSceneFor）。
//
// 取值与老后台那两个中文字符串是同一件事，只是这里存英文码、由前端的文案表翻。
const (
	// SubscriptionSceneCoffeeOrder 是「扫码点单时顺便签的」。
	SubscriptionSceneCoffeeOrder = "coffee_order"
	// SubscriptionSceneStoreCampaign 是「扫店铺码参加活动时签的」。
	SubscriptionSceneStoreCampaign = "store_campaign"
	// SubscriptionSceneMemberCenter 是「在会员中心直接买的」——默认那一档。
	SubscriptionSceneMemberCenter = "member_center"
)

// SubscriptionResponse 是一条订阅。
type SubscriptionResponse struct {
	ID           string `json:"id"`
	MembershipID string `json:"membershipId"`
	UserID       string `json:"userId"`
	PlanID       string `json:"planId"`
	// PlanName 是 join 出来的（同一库），列表上「会员等级」那一列就是它。
	PlanName string `json:"planName"`
	Status   string `json:"status"`
	// PriceCents 是签约时的每期扣款金额**快照**：套餐后来调价，已签约用户仍按这个数扣。
	PriceCents  int64  `json:"priceCents"`
	Period      string `json:"period"`
	PeriodCount int32  `json:"periodCount"`
	// SignScene 见上面三个常量。**今天恒为 member_center**：另外两条路要小程序端把
	// coffee_order_id / campaign_claim_id 写进来，而小程序端还没接。
	SignScene string `json:"signScene"`
	// FirstPaymentOrderID 是老后台「首月支付」那一列：只对 member_center 场景有值
	// （另外两条路上首月那笔钱在咖啡订单/活动单里，不在订阅上）。**今天恒为空**，同上的原因。
	FirstPaymentOrderID string `json:"firstPaymentOrderId"`
	// ChargeCount / FailedCount 是累计数，ConsecutiveFailedCount 是连续数（触发暂停用的）。
	// 分开是因为「历史失败过 5 次但已经好了」与「连续失败 5 次还在坏着」是两个问题。
	ChargeCount            int32 `json:"chargeCount"`
	FailedCount            int32 `json:"failedCount"`
	ConsecutiveFailedCount int32 `json:"consecutiveFailedCount"`
	// NextChargeAt 是下一次该扣的时间。active 一定有值（库上的 CHECK 钉着）。
	NextChargeAt *time.Time `json:"nextChargeAt"`
	LastChargeAt *time.Time `json:"lastChargeAt"`
	SuspendedAt  *time.Time `json:"suspendedAt"`
	CancelAt     *time.Time `json:"cancelAt"`
	CancelReason string     `json:"cancelReason"`
	// CancelledBy 是解约发起人（后台账户 id）。用户自己在小程序点的、或系统触发的都留空。
	CancelledBy string    `json:"cancelledBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// SubscriptionDetailResponse 是订阅**详情**的形状：订阅本身 + 两块来自别的域的只读回显。
//
// # 为什么嵌 SubscriptionResponse 而不是复制一遍字段
//
// 抽屉上半部分与列表用的是同一批字段，复制一份等于让「一条订阅长什么样」有两处定义，而它们迟早
// 会不一样（列表改了、详情忘了）。嵌入之后 JSON 上仍是摊平的——匿名字段不带 key，前端读到的
// 形状与列表里那一条逐字相同，多出来的就是下面这三项。
//
// # 三块「可能有、可能没有」的分寸不一样
//
//	ContractCode  本库的列，一定有值（活动发放与后台开通那两条路为空串 —— 它们没有渠道协议）
//	Charges       别的域的读，**失败或没有协议号时是空数组**，不是 null
//	FirstPayment  别的域的读，失败或没有首月订单时是 **nil**（前端整块不渲染）
//
// 后两项的失败一律降级：这一页的权限码是 membership:read，运营要看的是「谁签的、下一次什么时候
// 扣」，那在本库里，一个字都没毛病。让支付域抖一下就把整页打成 500 是本末倒置（见
// service.subscriptionDetail）。
type SubscriptionDetailResponse struct {
	*SubscriptionResponse
	// ContractCode 是渠道那份代扣协议的协议号——老后台抽屉里就有这一格，出了争议时运营拿它去
	// 微信商户平台查这份约。本服务自己那套四个动作（签约、查约、扣款、解约）用的就是它。
	ContractCode string `json:"contractCode"`
	// Charges 是这一份协议下的扣款期次（后台的「续费明细」），按期次升序。
	//
	// 含**还没扣成的**那些（charging、退避重试中、failed）：代扣这条链上「这一期为什么没扣到」
	// 比「扣成了哪几期」更需要被人看见。空数组表示「这一份协议下还没有期次」或「没读到」——
	// 两种在这一页上都不该显示成 null。
	Charges []AgreementCharge `json:"charges"`
	// FirstPayment 是首月那一笔的支付信息，**没有就让它是 nil**。
	//
	// 今天它恒为 nil：first_payment_order_id 要小程序端写进来，而小程序端还没接（见
	// SubscriptionResponse.FirstPaymentOrderID）。硬摆一块全是「—」的表格比不显示更糟。
	FirstPayment *SubscriptionFirstPayment `json:"firstPayment"`
}

// SubscriptionFirstPayment 是「首月那一笔」在后台的展示形状：订单域的订单 + 支付域的渠道流水。
//
// 两个域的值合在一个结构里，是因为老后台那一块本来就是一张表（签约场景 / 首月状态 / 实付金额 /
// 首月订单号 / 微信流水 / 支付方式 / 支付时间），拆成两段反而要在前端拼一次。**签约场景不在这里**
// ——它已经在 SubscriptionResponse.SignScene 上。
//
// 七个字段全是「有就有、没有就是空串/零」的展示值：它们不参与任何判定，缺一格不该让整页报错。
type SubscriptionFirstPayment struct {
	// OrderID / OrderNo 是首月那张订单的 id 与订单号。id 是订阅行上的那一列，订单号是订单域
	// 给的——后台要链去订单详情页时用的是 id。
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	// Status 是订单状态（paid / pending …），取值同订单域的 order_status。
	Status string `json:"status"`
	// PaidAmount 是实付金额（分）。首月那一笔可能带优惠，所以它未必等于每期扣款额。
	PaidAmount int64 `json:"paidAmount"`
	// PaymentMethod 是支付目录里的 code。取自**订单**上那一列，不是支付单上的——一张订单一种
	// 方式，而支付单那个字段是渠道视角的，两者在这一页上没有分歧的必要。
	PaymentMethod string `json:"paymentMethod"`
	// PaymentNo 是订单上那个支付单号，空串表示这张单还没发起过支付。
	PaymentNo string `json:"paymentNo"`
	// ProviderTransactionID 是渠道流水号（首月那笔的微信流水），**这一块里唯一要绕两跳的值**：
	// 订单 → payment_no → 支付单 → 渠道流水。任一跳断掉它留空，其余几格照常显示。
	ProviderTransactionID string `json:"providerTransactionId"`
	// PaidAt 是 RFC3339（UTC）字符串，未支付时为空串。
	PaidAt string `json:"paidAt"`
}

// SubscriptionStats 是订阅列表页头上那两张卡。
type SubscriptionStats struct {
	// ActiveCount 只数 active。suspended 不算——它已经被停掉了（连续扣款失败到阈值），混进来会
	// 让「现在有多少人在正常续费」这个数虚高。
	ActiveCount int `json:"activeCount"`
	// DueCount 是「已经到点该扣、但还没扣」的条数（active 且 next_charge_at <= now）。
	// 判据与 model.Subscription.IsDue 逐字相同。
	DueCount int `json:"dueCount"`
}

// CreateSubscriptionRequest 是小程序端「开通连续包月」的请求体。
//
// **没有 user_id**：签的是发起请求的那个人，身份只认令牌（见 controller.MiniappMembershipController）。
// 让客户端自称 user_id 等于把「用谁的名义签一份代扣协议」交给调用方。
//
// **没有金额与时长**：每期扣多少、扣多久全部取自套餐（price_cents / period / period_count），
// 而且要在签约那一刻**冻进订阅**（membership_subscriptions 上那组快照列）。客户端能影响的只有
// 选哪一款套餐——钱的事一个字都不该由它说。
type CreateSubscriptionRequest struct {
	// PlanID 必填，必须是一个开启了连续包月（autoRenew）的套餐。
	PlanID string `json:"planId"`
	// RequestID 是这次点击的幂等键，必填。
	//
	// 与后台开通那条（dto.GrantRequest）同一条理由：签约会**在渠道那边留一份协议**，而服务端
	// 没有别的东西能认出「这是同一次点击」。缺了它，一次网络抖动后的重试会在微信里堆出两份
	// 待签协议——用户点开其中一个签完，另一份还在那儿等着。
	RequestID string `json:"requestId"`
	// CoffeeOrderID / CampaignClaimID 是「这次签约从哪来」，可留空。
	//
	// 签约场景（后台列表上那一列）由它们现推，不落库（见 migrations/membership）：从一杯
	// 咖啡的订单页进来的传前者，从店铺码活动页进来的传后者，在会员中心直接点的两个都不传。
	// 两者都给时**咖啡订单优先**——那是用户当下真正在做的事，与老系统同一条口径。
	//
	// 它们是跨库值引用（订单库 / membership_campaign_claims），本服务**不校验存在性**：那要
	// 多一次跨服务查询，而这两个号只用来算一个展示字段。传错了的后果是后台那一列显示成
	// 「咖啡订单」，不影响任何一笔钱。
	CoffeeOrderID   string `json:"coffeeOrderId"`
	CampaignClaimID string `json:"campaignClaimId"`
	// OrderID 是「首月那一笔」的订单号，可留空，**只对会员中心那一档填**。
	//
	// 它喂给后台列表的「首月支付」列。另外两条路上首月那笔钱在咖啡订单或活动单里，硬摆到这一列
	// 会让人以为那是订阅的首期扣款（那是两笔不同的钱），所以那两种场景下它会被服务层丢掉。
	OrderID string `json:"orderId"`
}

// SubscriptionSigningResponse 是「发起签约」的应答：订阅 + 客户端要拿去跳转的东西。
//
// 它与 SubscriptionResponse 分开而不是往那个结构体上加两个字段：后台的列表与详情用的是后者，
// 而签约参数**只在这一次应答里有意义**（它是一次性的，且含签名）。
type SubscriptionSigningResponse struct {
	// Subscription 是刚落的那条订阅（pending_sign）。客户端拿它的 id 去做后续的确认。
	Subscription *SubscriptionResponse `json:"subscription"`
	// Action 是支付方式目录里的行为码（今天的签约是 jump_miniapp），客户端只 switch 它。
	Action string `json:"action"`
	// PayParams 是跳转微信签约小程序要带的参数（协议的 8 个字段 + sign + 跳转目标）。
	//
	// ⚠️ **它含签名，是一次性凭据**：只进这一次应答，不进日志、不进任何流水。丢了下一次点击
	// 会重新领一份（requestId 幂等，渠道那边不会多出一份协议）。
	PayParams map[string]string `json:"payParams"`
}

// ConfirmSubscriptionRequest 是「用户从微信回来，问签成没有」的请求体。
//
// 订阅 ID **走请求体而不是路径**：它是发起签约那一次应答里带回来的，客户端手上只有它，而这一棵
// 树上其余接口都是「一个路径一件事」的写法（没有 `{id}` 那种带参数的路径，见 routes/miniapp.go）。
// 为一条路径引入路径参数，会让这一棵树的读法多出一种。
//
// **没有 requestId**：确认这个动作本身不改钱也不改协议，它只是「回渠道核一次」。渠道那边按
// 协议号核，重复调用得到的结论一样（见 service.ConfirmSubscription 的幂等说明）。
type ConfirmSubscriptionRequest struct {
	// SubscriptionID 必填，必须是发起签约时返回的那一个（别人的 ID 回 404）。
	SubscriptionID string `json:"subscriptionId"`
}

// SubscriptionSyncResponse 是后台「同步」的应答：核过之后的订阅 + 「这一下有没有改到东西」。
//
// 为什么不是裸的 SubscriptionResponse：**两个都是成功**，而运营要知道自己那一下起没起作用
// ——「同步完成」与「无需更正」是两句不同的话。让前端拿前后两次状态自己比，等于把这条判据
// 抄到前端去，而它抄错的样子是「点了没反应」。
type SubscriptionSyncResponse struct {
	Subscription *SubscriptionResponse `json:"subscription"`
	// Changed 是这次收口有没有真的改到本地（仓储按「状态是否已经等于目标」判，见
	// repository.SettleSubscription）。false 的两种情况都是正常的：渠道说还等着（还没签成），
	// 或者本地早就是渠道说的那个状态了（用户在小程序里已经确认过）。
	Changed bool `json:"changed"`
	// ProviderState 是渠道的原话（signed / pending / terminated），**不翻译**。运营看到
	// 「同步了但没变」时，下一句要问的就是它——今天这个按钮的另一半用处在这里。
	ProviderState string `json:"providerState"`
}

// CancelSubscriptionRequest 是后台取消一条订阅的请求体。
//
// 只有一个原因，与四个会员动作同一个形状（见 AdjustRequest）。**没有「改状态」这种通用
// 接口**：取消是这里唯一能做的事，把 status 做成一个字段就等于允许把订阅改成任何状态，
// 而那里面有几种组合会直接撞上库上的 CHECK。
type CancelSubscriptionRequest struct {
	// Reason 必填。空串时服务层回一句中文（「请填写取消原因」），不是静默填一个默认值——
	// 这是别人钱袋子上的一次人工操作，事后的留痕里不该有一句系统替他写的话。
	Reason string `json:"reason"`
}
