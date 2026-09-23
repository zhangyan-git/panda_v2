package model

import (
	"encoding/json"
	"time"
)

// OrderLine 对应 order_lines 表，订单行：这一单实际买了哪几件东西，一件一行。
//
// 饮品行（drink）、加购品行（addon）、会员行（membership）在同一张表里，因为
// 「这一单有哪些东西」永远是一张清单；拆表之后下单、详情、退款、对账每一处都要
// UNION，金额之和与序号规则也会在每张表上各写一遍。
type OrderLine struct {
	ID      string `db:"id"`
	OrderID string `db:"order_id"`
	LineNo  int    `db:"line_no"`
	// drink=饮品，addon=加购品（幸运杯套一类的活动商品），membership=会员套餐。
	LineType string  `db:"line_type"`
	LegacyID *string `db:"legacy_id"`
	// 商品快照：item_id 是值引用（饮品目录 / 会员套餐），编码、名称、图片是下单当时的副本。
	// 加购品可能只有活动、没有独立商品目录，那时 item_id 为空，身份看 campaign_id。
	ItemID    *string `db:"item_id"`
	ItemCode  string  `db:"item_code"`
	ItemName  string  `db:"item_name"`
	ItemImage string  `db:"item_image"`
	Quantity  int     `db:"quantity"`
	// 算钱一律从 original_unit_price（目录价）出发；unit_price 只是「当时标价多少」的
	// 展示值，不参与任何恒等式——否则同一笔会员价优惠会在单价里少一次、又在优惠额里再算一次。
	//
	//   payable_amount = original_unit_price * quantity - discount_amount
	//   discount_amount = price_discount_amount + coupon_discount_amount
	OriginalUnitPrice int64 `db:"original_unit_price"`
	UnitPrice         int64 `db:"unit_price"`
	// 标价优惠：目录价与成交单价之间的差额（会员价、加购活动价、套餐活动价）。
	PriceDiscountAmount int64 `db:"price_discount_amount"`
	DiscountAmount      int64 `db:"discount_amount"`
	PayableAmount       int64 `db:"payable_amount"`
	// 券挂在**行**上，因为它抵的是某一件商品的价钱：咖啡券只优惠饮品，不抵扣杯套。
	CouponID             *string `db:"coupon_id"`
	CouponDiscountAmount int64   `db:"coupon_discount_amount"`
	// 饮品规格（冷热/杯型/糖量）与固定选品快照。结构归 coffee-machine-service，
	// 本库按收到的样子原样存档，不参与查询条件。加购品与会员行是空对象。
	Specs             json.RawMessage `db:"specs"`
	SelectionSnapshot json.RawMessage `db:"selection_snapshot"`
	// 加购活动（原型里的「幸运杯套」）：活动 ID 是值引用，名称/活动价/每件赠几张福卡
	// 是下单当时的快照。活动改规则或下架都不能改变历史订单。
	CampaignID       *string         `db:"campaign_id"`
	CampaignSnapshot json.RawMessage `db:"campaign_snapshot"`
	// 会员套餐快照，只有会员行有。会员资格本身（等级、到期时间）在 membership-service。
	MembershipPlanSnapshot json.RawMessage `db:"membership_plan_snapshot"`
	// 出杯执行与取杯凭证，只有饮品行有。
	//
	// device_id 是订单上那台机器的副本：冗余在这里是为了让 (device_id, device_order_no)
	// 能建唯一索引——厂商单号的唯一性作用域是「同一台机器」。
	DeviceID          *string `db:"device_id"`
	DeviceOrderNo     string  `db:"device_order_no"`
	FulfillmentTaskNo string  `db:"fulfillment_task_no"`
	// 取杯号：屏幕/取杯口上显示的短号（原型里的 C031），支付成功时生成，全局唯一。
	// 用户侧和后台都看得到——取杯口那块屏幕本来就把它大字摆着。
	//
	// 它曾经被拆成两列（pickup_no「显示的短号」+ pickup_code「唯一凭据」），并由此派生出一条
	// 「后台不得展示取杯码」的规则。那个拆分是凭空造的：原型里只有一个 order.pickup 字段，
	// 用户侧叫取杯号、屏幕上叫取杯码，是同一个值。库里的列名就是 pickup_code，没有 pickup_no。
	PickupCode *string   `db:"pickup_code"`
	Remark     string    `db:"remark"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

// 行类型取值，与 order_lines.line_type 的 CHECK 逐字一致。
const (
	LineTypeDrink      = "drink"
	LineTypeAddon      = "addon"
	LineTypeMembership = "membership"
)

// 分账业务分类，发起支付时随请求交给支付域，命中 settlement_rules.biz_type。
//
// 与 payment 库 settlement_rules.biz_type 的 CHECK 前三个值逐字一致（第四个 store_consume
// 是「到店消费」，老系统 store_pos 的存量口径，V2 没有产生它的来源，所以这里不列）。这份常量
// 在支付域另有一份**完整的**词表用于校验——两边都对着 settlement_rules.biz_type 的 CHECK 写，
// 改词表要一起改。
const (
	SettlementBizCoffee       = "coffee"
	SettlementBizMembership   = "membership"
	SettlementBizAddonProduct = "addon_product"
)
