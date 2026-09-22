package model

import (
	"encoding/json"
	"time"
)

// Order 对应 orders 表，订单主表。
//
// 这里没有 order_type：一次支付可以只买咖啡、只买会员，也可以两样一起买，
// 「这一单是什么类型」由它有哪些 order_lines 决定（见迁移 001 的说明）。
type Order struct {
	ID       string  `db:"id"`
	OrderNo  string  `db:"order_no"`
	LegacyID *string `db:"legacy_id"`
	// 下单用户。nil = 这一单**没有用户**，只有线下刷卡机这一条路会这样（source=device）：
	// 钱在机器上收过了，我们只是把既成事实记下来，它不属于任何用户。
	//
	// 指针而不是空串：空串与「某个真的用户」长得一样，uuid 列上也根本存不进去——空串要靠
	// NULLIF 才会变成 NULL，任何一处忘了写就是 22P02。而「没有用户」在这条路上是**常态**，
	// 不是一个要绕开的边界。
	UserID *string `db:"user_id"`
	// 下单来源：miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单，
	// device=线下刷卡机设备回调（partner-service 验签后经 gRPC 建单），
	// renewal=会员续费代扣（membership-service 在扣款成功之后经 gRPC 建单）。
	Source string `db:"source"`
	// 主状态机：pending_payment → paid → completed，或 pending_payment → cancelled / expired。
	Status string `db:"status"`
	// 履约汇总，事实由 fulfillment-service 的出杯任务产生；本库只接收事件更新这一个字段。
	// none 表示本单没有任何需要履约的行（纯会员订单）。
	FulfillmentStatus string `db:"fulfillment_status"`
	// 点位与咖啡机的值引用 + 下单当时的快照名/编号。点位名与设备编号是快照，
	// 改名换号以后历史订单仍能还原成用户当时看到的样子。
	StoreID   *string `db:"store_id"`
	StoreName string  `db:"store_name"`
	DeviceID  *string `db:"device_id"`
	DeviceNo  string  `db:"device_no"`
	// 屏幕选品的会话，只作值引用；选品快照落在饮品行上。
	SceneToken string `db:"scene_token"`
	// 金额，单位分。payable = original - discount 是定义，由 orders_payable_matches 约束保证。
	OriginalAmount int64 `db:"original_amount"`
	DiscountAmount int64 `db:"discount_amount"`
	PayableAmount  int64 `db:"payable_amount"`
	PaidAmount     int64 `db:"paid_amount"`
	RefundedAmount int64 `db:"refunded_amount"`
	// 下单时的会员资格快照。结构归 membership-service，本库只存一份副本用于还原当时的会员价。
	MembershipID       *string         `db:"membership_id"`
	MembershipSnapshot json.RawMessage `db:"membership_snapshot"`
	// 这一单承诺发多少张福卡（基础赠送 + 各加购活动加赠）。只存承诺，不存发放结果。
	FortuneCardsExpected int             `db:"fortune_cards_expected"`
	FortuneCardSnapshot  json.RawMessage `db:"fortune_card_snapshot"`
	// 主支付渠道与支付单号（payment-service 的值引用）。逐笔出资分摊在 order_payment_lines。
	PaymentMethod      string     `db:"payment_method"`
	PaymentNo          string     `db:"payment_no"`
	PaidAt             *time.Time `db:"paid_at"`
	FinishedAt         *time.Time `db:"finished_at"`
	CancelledAt        *time.Time `db:"cancelled_at"`
	CancellationReason string     `db:"cancellation_reason"`
	// 待支付超时时间，关单扫描按它扫。
	ExpiresAt *time.Time `db:"expires_at"`
	Remark    string     `db:"remark"`
	// 对方单号，设备回调的幂等键（orders_third_party_order_no_key）。空串表示这一单没有
	// 对方单号——小程序与屏幕扫码那两条路都是空的。
	ThirdPartyOrderNo string `db:"third_party_order_no"`
	// 下单请求号：同一个 request_id 只能落一个订单（orders_request_id_key）。
	RequestID string    `db:"request_id"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// OrderStatus 是 orders.status 的取值全集，与迁移里的 CHECK 逐字一致。
const (
	OrderStatusPendingPayment = "pending_payment"
	OrderStatusPaid           = "paid"
	OrderStatusCompleted      = "completed"
	OrderStatusCancelled      = "cancelled"
	OrderStatusExpired        = "expired"
	OrderStatusRefunding      = "refunding"
	OrderStatusRefunded       = "refunded"
)

// FulfillmentStatus 是 orders.fulfillment_status 的取值全集。
const (
	FulfillmentPending   = "pending"
	FulfillmentMaking    = "making"
	FulfillmentReady     = "ready"
	FulfillmentCompleted = "completed"
	FulfillmentFailed    = "failed"
	FulfillmentCancelled = "cancelled"
	// FulfillmentNone：本单没有任何需要履约的行（纯会员订单）。没有这个值，
	// 会员订单会永远停在 pending，后台「待制作」列表里就会混进一堆做不完的会员单。
	FulfillmentNone = "none"
)

// OrderSource 是 orders.source 的取值全集，与迁移里的 CHECK 逐字一致。
const (
	SourceMiniapp  = "miniapp"
	SourceScreenQR = "screen_qr"
	// SourceDevice 是线下刷卡机：partner-service 验完厂商签名后经 gRPC 建单，没有用户、
	// 没有支付单，**直接落成已支付**（钱在机器上已经收过了，见 order/005）。
	SourceDevice = "device"
	// SourceRenewal 是**会员续费**：连续包月每期代扣成功之后，由 membership-service 经 gRPC
	// 建单，同样直接落成已支付（钱是微信代扣收的，我们手里只有渠道流水号）。
	//
	// 这一条**是这三条「钱已在别处收过」的路里唯一有用户的一条**（user_id 非空）。它买的是
	// 会员时长，没有任何需要履约的行——fulfillment_status 恒为 none（纯会员订单）。
	//
	// 为什么不复用 miniapp：那条路的语义是「用户在小程序里点了下单」，而续费根本没有这一下
	// ——没有人点，是到期自动扣的。后台列表的来源筛选、以及按来源看的每一张报表，靠的都是
	// 这一列，把它们混在一起就再也分不开了（见 order/009）。
	//
	// **订单号不另起前缀**（老系统续费单是 SUB 开头）：单号格式是收单渠道的约定
	// （3CYM + 时间 + 随机，见 generateOrderNo），而「这是续费」这件事已经由本列说清楚了
	// ——用前缀表达的代价是所有认单号格式的地方都要多认一种。
	SourceRenewal = "renewal"
)

// IsOrderSource 判断一个值是不是合法的下单来源，与 orders_source_check 逐字一致。
//
// 后台列表的 source 筛选用它**在进 SQL 之前**挡下打错的值（照 IsPaymentStatus 那条）：
// 一个不在词表里的值送进 SQL 不报错，只会安静地返回空列表，而运营会把它当成「这个来源
// 真的没有订单」。
func IsOrderSource(source string) bool {
	switch source {
	case SourceMiniapp, SourceScreenQR, SourceDevice, SourceRenewal:
		return true
	default:
		return false
	}
}
