package model

import "time"

// UserWechatIdentity 对应 user_wechat_identities 表，一条微信身份绑定。
//
// openid 是「按应用」的标识：同一个微信用户在两个小程序里拿到的是两个不同的
// openid，跨应用稳定的只有 unionid。所以库里的唯一键是 (app_type, openid)
// 而不是 openid 单列——将来接入公众号，也只是多一行 app_type 不同的记录，
// 而不是给 users 加列再回填。
type UserWechatIdentity struct {
	ID      string `db:"id"`
	UserID  string `db:"user_id"`
	AppType string `db:"app_type"` // miniapp / official_account
	OpenID  string `db:"openid"`
	// UnionID 未绑定微信开放平台的小程序拿不到，因此可空。
	// 仓库惯例把可空文本 COALESCE 成 ''，扫描进来区分不了 NULL 与空串。
	UnionID     string     `db:"unionid"`
	LastLoginAt *time.Time `db:"last_login_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// 微信身份所属的应用类型，与 user_wechat_identities.app_type 的 CHECK 一致。
const (
	// WechatAppMiniapp 小程序
	WechatAppMiniapp = "miniapp"
	// WechatAppOfficialAccount 公众号；当前无写入方，列在这里是为了让 CHECK
	// 约束的取值在 Go 侧有对应常量，而不是散落的字面量。
	WechatAppOfficialAccount = "official_account"
)
