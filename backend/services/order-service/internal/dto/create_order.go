package dto

import (
	"encoding/json"
	"time"
)

// CreateOrderRequest 是下单请求。
//
// ⚠️ 价格来自调用方，这一点是**当前阶段的已知缺口**，不是设计意图。
//
// 方案 5.8 要求下单时校验「设备状态、商品供应及选品上下文」，价格应当由服务端从
// 商品目录现查（coffee-machine-service 的饮品目录 + 每机覆盖价）。但那边今天只实现了
// GetDevice 一个 RPC（ListDrinks / ListDeviceDrinks 还没在 gRPC server 上注册），
// 拿不到任何价格，所以这一版先把行快照整行收下来：服务端仍然重算 discount_amount 与
// payable_amount 并校验全部恒等式，但 original_unit_price 是**信任调用方**的。
//
// 接上目录之后的改法：把 originalUnitPrice / unitPrice / itemCode / itemName / itemImage
// 从请求里删掉，改成只收 itemId（和活动 ID），其余由服务端按 device + drink 现查填充。
// 在那之前，这个接口只对可信调用方开放——小程序端不能直接把它暴露给用户。
type CreateOrderRequest struct {
	// miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单。
	Source string `json:"source"`
	// 点位与咖啡机。有饮品行时 deviceId 必填：设备是「这台机器属于哪个点位」的唯一来源，
	// storeId 传了就必须与设备上的点位一致，不一致直接拒（见 service.createOrder）。
	StoreID    *string `json:"storeId"`
	StoreName  string  `json:"storeName"`
	DeviceID   *string `json:"deviceId"`
	SceneToken string  `json:"sceneToken"`
	// 下单时的会员资格快照，原样存下来用于还原当时的会员价。结构归 membership-service。
	MembershipID       *string         `json:"membershipId"`
	MembershipSnapshot json.RawMessage `json:"membershipSnapshot"`
	// 这一单承诺发多少张福卡（基础赠送 + 各加购活动加赠）。规则本身（方案 3.1）仍是待定项，
	// 没有任何服务拥有它，所以这里不重算，只如实存下调用方算好的承诺数。
	FortuneCardsExpected int               `json:"fortuneCardsExpected"`
	FortuneCardSnapshot  json.RawMessage   `json:"fortuneCardSnapshot"`
	Lines                []CreateOrderLine `json:"lines"`
	Remark               string            `json:"remark"`
}

// CreateOrderLine 是下单请求里的一行商品。
//
// 没有 discountAmount / payableAmount：这两个值由服务端从 PriceDiscountAmount 与
// CouponDiscountAmount 算出来（discount = price + coupon，payable = 目录单价 × 数量 −
// 优惠），收进来就等于给了同一笔钱两个来源。
//
// 也没有 pickupCode / deviceOrderNo / fulfillmentTaskNo：取杯码是凭据，由服务端在
// 支付成功时生成；厂商单号与履约任务号属于履约侧，不从小程序收。
type CreateOrderLine struct {
	// drink / addon / membership。
	LineType  string  `json:"lineType"`
	ItemID    *string `json:"itemId"`
	ItemCode  string  `json:"itemCode"`
	ItemName  string  `json:"itemName"`
	ItemImage string  `json:"itemImage"`
	Quantity  int     `json:"quantity"`
	// 目录价与成交单价，单位分。unitPrice 只是展示值，不参与任何恒等式。
	OriginalUnitPrice   int64 `json:"originalUnitPrice"`
	UnitPrice           int64 `json:"unitPrice"`
	PriceDiscountAmount int64 `json:"priceDiscountAmount"`
	// 券只优惠饮品：加购行与会员行带 couponId 会被拒（DB 的
	// order_lines_coupon_only_on_drink 也挡这一条）。
	CouponID             *string `json:"couponId"`
	CouponDiscountAmount int64   `json:"couponDiscountAmount"`
	// 饮品规格与固定选品快照，原样存档不参与查询。加购行与会员行留空。
	Specs             json.RawMessage `json:"specs"`
	SelectionSnapshot json.RawMessage `json:"selectionSnapshot"`
	// 加购活动：加购行必须有 campaignId，饮品行与会员行不能有。
	CampaignID       *string         `json:"campaignId"`
	CampaignSnapshot json.RawMessage `json:"campaignSnapshot"`
	// 会员套餐快照，只有会员行有。
	MembershipPlanSnapshot json.RawMessage `json:"membershipPlanSnapshot"`
	Remark                 string          `json:"remark"`
}

// CreateOrderResponse 是下单成功的响应体，同时也是写进幂等表的响应快照：
// 同一个 Idempotency-Key 重放时原样回放它，不重新执行一遍副作用。
type CreateOrderResponse struct {
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	// 主状态机的初始状态（pending_payment）与履约汇总的初始状态（有饮品行是 pending，
	// 纯会员单是 none）。
	Status            string `json:"status"`
	FulfillmentStatus string `json:"fulfillmentStatus"`
	// 金额，单位分。payableAmount = originalAmount − discountAmount，由服务端算出。
	OriginalAmount int64 `json:"originalAmount"`
	DiscountAmount int64 `json:"discountAmount"`
	PayableAmount  int64 `json:"payableAmount"`
	// 待支付超时时间，客户端据此显示倒计时；到期由关单扫描置为 expired。
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
