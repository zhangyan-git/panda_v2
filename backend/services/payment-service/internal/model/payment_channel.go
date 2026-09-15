package model

import (
	"encoding/json"
	"time"
)

// PaymentChannel 对应 payment_channels 表：一家渠道的对接配置。
//
// 与 payment_methods 的分工：渠道是「怎么对接」，支付方式是「给用户看什么、怎么起支付」。
// 一个渠道可以对应多个支付方式（同一个微信商户号下的小程序支付与扫码支付，action 不同）。
type PaymentChannel struct {
	ID string `db:"id"`
	// 老库 payment_methods._id（ObjectID 的 24 位 hex），仅迁移过来的行有值。
	LegacyID *string `db:"legacy_id"`
	// 渠道代码，如 wechat_miniapp、unionpay、fengxuan_wanlian、youlian、manual_dev。
	// 回调地址里的那段就是它。
	Code string `db:"code"`
	Name string `db:"name"`
	// 适配器的名字：provider.Registry 按它查实现（本轮只有 manual）。
	Provider string `db:"provider"`
	// sandbox / live。
	Mode string `db:"mode"`
	// enabled / disabled / legacy_readonly。legacy_readonly 是渠道停止新交易之后仍要能
	// 退款、能对账的那一档，不能删行。
	Status string `db:"status"`
	// 非敏感对接参数：商户号、appid、回调地址、证书序列号等。
	Config json.RawMessage `db:"config"`
	// 密钥在受控 Secret 管理系统里的键名（或环境变量名），**不是密钥本身**。
	SecretRef string    `db:"secret_ref"`
	Remark    string    `db:"remark"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// 渠道状态，与 payment_channels.status 的 CHECK 逐字一致。
const (
	ChannelEnabled  = "enabled"
	ChannelDisabled = "disabled"
	// ChannelLegacyReadonly：渠道已停新交易，但仍要能退款、能对账，所以不能删行。
	ChannelLegacyReadonly = "legacy_readonly"
)
