package model

import (
	"encoding/json"
	"time"
)

// PaymentMethod 对应 payment_methods 表：前台/设备上「用户可以选哪几种支付方式」。
//
// 「哪台设备能用哪几种方式」**不在这里**：那是 coffee_machine 库的 device_payment_methods，
// 按设备显式配置。本表只管「这条方式是什么形态、怎么起支付」。
type PaymentMethod struct {
	ID string `db:"id"`
	// 老库 payment_methods._id（ObjectID 的 24 位 hex），仅迁移过来的行有值。
	LegacyID *string `db:"legacy_id"`
	// 支付方式代码，老库风格的点数/饭卡类为 points_xxx、meal_card_xxx。
	// 给人看的标识（后台配置、迁移对账、日志排查），**不用来做行为分派**——分派认 Action。
	Code        string `db:"code"`
	Name        string `db:"name"`
	Description string `db:"description"`
	Icon        string `db:"icon"`
	// 外部渠道；账户出资方式（咖啡豆）为空。
	ChannelID *string `db:"channel_id"`
	// 选中这条方式之后怎么起支付，客户端只 switch 它，不 switch Code。
	Action string `db:"action"`
	// 方式级启动参数（跳转 path、附加 query、查单与退款策略等），**不放密钥**；
	// 渠道级参数在 payment_channels.config，返回客户端时两层合并。
	Params json.RawMessage `db:"params"`
	// 本方式对应的出资类型，词表与 order_payment_lines.line_type 一致。
	FundingType string `db:"funding_type"`
	// enabled / disabled。
	Status    string    `db:"status"`
	SortOrder int       `db:"sort_order"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// 支付方式状态，与 payment_methods.status 的 CHECK 逐字一致。
const (
	MethodEnabled  = "enabled"
	MethodDisabled = "disabled"
)

// Action 取值的字符串形式，与 payment_methods.action 的 CHECK 逐字一致。
//
// 这里是字符串常量而不是 provider.Action 类型：model 层是表的镜像，不该 import 业务包。
// 由 provider.NewMethod 负责把列值翻译成带类型的 Action 并校验。
const (
	ActionJumpMiniapp = "jump_miniapp"
	ActionNativePay   = "native_pay"
	ActionDirectPay   = "direct_pay"
	ActionQrcode      = "qrcode"
	ActionH5          = "h5"
	ActionAccount     = "account"
)
