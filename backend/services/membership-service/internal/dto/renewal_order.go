package dto

// 订单域在本服务这一侧的形状：一张续费单**怎么写**（RenewalOrderParams），以及任意一张订单
// **怎么读**（OrderSummary）。
//
// 与 agreement.go 同一个位置、同一条理由：这些是**跨服务调用的输入输出**，client 与 service
// 都要用，放在任何一边都会让另一边反过来 import。与 order v1 的 proto 一比一，但**不直接用生成
// 的类型**——那会把 proto 的字段名与可空语义泄进 service 层。

// RenewalOrderParams 是把一期扣款记成一张订单所需的事实。
//
// # 每一个字段都来自本域的冻结值，没有一个是现查的
//
// 这一整条路的规矩是「签约时约定死的算数」：
//
//	ThirdPartyOrderNo   渠道流水号 —— 这一期扣款的身份证，也是订单域的幂等键
//	UserID              **订阅行上的 user_id**（不是事件里的那个，见 service 里的核对）
//	Amount              订阅上的 price_cents（套餐后来调价不影响已签约的人）
//	Plan.*              会员行上那份成交快照 + 订阅上那一期的时长
//	MembershipID        订阅指向的那条会员
//
// 让订单域拿 plan_id 现取套餐是不行的：那等于让一次后台改套餐改写一个正在被扣款的用户这一期
// 买到了什么（见 proto 里 CreateRenewalOrder 的说明）。
type RenewalOrderParams struct {
	// ThirdPartyOrderNo 是渠道流水号（payment_agreement_charges.provider_transaction_id）。
	//
	// 它是订单域的幂等键，而且**只有成功那一期才有**：没扣到钱就没有号，也就没有订单可言
	// （失败的那一期不建单，见 proto）。
	ThirdPartyOrderNo string
	UserID            string
	// Amount 是这一期实付的金额，单位为分。
	Amount int64
	// Plan 是签约时冻结的套餐快照，**整份给全**。
	Plan RenewalPlanSnapshot
	// MembershipID 落进订单，让后台从会员看订单能查到它。可空只是因为它是一个值引用。
	MembershipID string
	// Remark 可空，订单域会拼在「会员续费扣款」之后。
	Remark string
}

// RenewalPlanSnapshot 是协议里 MembershipPlanSnapshot 在本服务这一侧的形状。
//
// 字段与 order-service 的 dto.MembershipPlanSnapshot、与 memberships 上那几个快照列**逐字同构**
// ——同一个结构编出来的 JSON 要能直接落进 order_lines.membership_plan_snapshot，中间不做任何
// 改名或换算。
type RenewalPlanSnapshot struct {
	PlanID   string
	PlanCode string
	PlanName string
	// PriceCents 是这一期实付的套餐价（分），也就是订单行上的原价与应付。
	PriceCents int64
	// Period / PeriodCount 是一期的时长（连续包月 = month/1），取自**订阅**行。
	Period      string
	PeriodCount int32
	// MemberPriceMode 与它下面两个：这一期给不给会员价券、给几张。发券那一条读的是它
	// （见 coupon-service）。
	MemberPriceMode             string
	MemberPriceCouponTemplateID string
	MemberPriceCouponsPerPeriod int32
	// AutoRenew 是套餐层面的「这个产品要签代扣」，不是用户的开关。
	AutoRenew bool
}

// RenewalOrder 是订单域对一次建单的回答。
type RenewalOrder struct {
	OrderID string
	OrderNo string
	// Created 为 true 表示这一次真的建了单；false 表示**幂等命中**，返回的是既有那张单。
	//
	// 与设备单那条同一条：它不是错误，也不是「什么都没发生」——重投是常态，而调用方要能区分
	// 「新建了」与「重投了一次」（见 proto 的说明）。**订单号两种情况都非空**，这正是调用方
	// 需要的：重投时它要写进流水的是同一张单的 id，不是一张新单。
	Created bool
}

// OrderSummary 是一张订单的摘要，后台「包月订阅」详情抽屉里**首月支付信息**那一块读的就是它。
//
// 它与 RenewalOrderParams 正好相反：那个是写，这个是读；那个说「这一期该怎么记」，这个说
// 「那张单上当时记了什么」。
//
// # 它同时是「首月支付的订单是哪一张」这条链的第一跳
//
// 那一段要显示渠道流水号，而**渠道流水号不在订单域**（对账凭据按既定分工只有 payment-service
// 对外给，见 proto 里 GetOrder 的说明）。所以这一跳只拿 payment_no，拿去 payment 的 GetPayment
// 换第二跳。两跳之间没有任何加工：本服务不代转、也不缓存。
type OrderSummary struct {
	OrderID string
	OrderNo string
	UserID  string
	// Source 取 miniapp / screen_qr / device / renewal（orders_source_check）。
	Source string
	Status string
	// PaidAmount 是实付金额（分），未支付时是 0。
	PaidAmount int64
	// PaymentMethod 是 payment 目录里的 code。
	PaymentMethod string
	// PaymentNo 是订单库上那一列的值引用。**不是渠道流水号**，而且设备单与续费单上它是空串
	// （那两条路上没有支付单）——拿到空串时那一格不该去问 payment（见 service 的说明）。
	PaymentNo string
	// PaidAt 是 RFC3339（UTC），未支付时是空串。**它保持字符串**，不在这一层解成时间：这个值
	// 一路原样交给前端展示，解一次再编回去只会多一个出错的环节（同 dto/event.go 里那两个时间
	// 字段的取舍）。
	PaidAt string
}
