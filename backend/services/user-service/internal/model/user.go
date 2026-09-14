package model

import (
	"errors"
	"strings"
	"time"
)

// 唯一键冲突的哨兵错误：repository 把 SQLSTATE 23505 翻译成它们，service 再
// 映射成 409。分成两个而不是一个笼统的「写入失败」，因为用户看到的下一步动作
// 不同——手机号被占用要引导他用验证码登录，微信被占用要引导他换一个微信号。
var (
	// ErrPhoneTaken 该手机号已绑定另一个账号。
	ErrPhoneTaken = errors.New("手机号已被其他账号绑定")
	// ErrWechatIdentityTaken 该微信已绑定另一个账号。
	ErrWechatIdentityTaken = errors.New("该微信已绑定其他账号")
	// ErrUserDeleted 账号已注销。注销是用户自己的决定（软删除），后台不提供
	// 「复活」入口：控制台把人恢复成 active，等于替用户撤销了他自己做过的选择，
	// 而库里没有任何东西能证明这次撤销是对的。
	ErrUserDeleted = errors.New("该账号已注销，不能通过后台修改状态")
)

// MaskPhone 保留前 3 位和后 2 位，中间打码。
//
// 手机号进入任何长期留存或会被导出的地方——审计表、事件总线、日志——之前都要
// 过这里：那些地方是给安全侧查的，不是客服工具，完整号码在那里只会变成一份
// 需要额外保护的 PII 清单。需要完整号码的场景是后台用户详情页，它按 id 现查库。
func MaskPhone(phone string) string {
	if len(phone) < 7 {
		return strings.Repeat("*", len(phone))
	}
	return phone[:3] + strings.Repeat("*", len(phone)-5) + phone[len(phone)-2:]
}

// AdminSettableStatus 报告 status 是否能由后台设置。
//
// 只有 active / disabled 两种：deleted 是用户注销留下的终态（见 ErrUserDeleted），
// 后台能改的只有「能不能登录」，不包括「这个账号还算不算存在过」。
func AdminSettableStatus(status string) bool {
	return status == UserStatusActive || status == UserStatusDisabled
}

// User 对应 users 表，小程序（C 端）用户。
//
// 与 MerchantUser / AdminUser 的关系：这三张表都在身份库，但服务的是三类完全
// 不同的人——平台管理员、商户员工、小程序顾客。C 端用户没有 password_hash 列，
// 登录一律走微信或短信验证码；也不要给它加，一旦有了密码就等于多了一条可以
// 被撞库的入口，而现有需求里没有密码登录。
type User struct {
	ID string `db:"id"`
	// LegacyID 老系统 users 集合的 ObjectID，只用于迁移对账。
	// V2 原生注册的用户为空串——仓库惯例是把可空文本 COALESCE 成 ''，
	// 所以这里区分不了 NULL 与空串，两者都表示「不是从老系统迁来的」。
	LegacyID string `db:"legacy_id"`
	// Phone 手机号，登录标识之一；空串表示尚未绑定手机号。
	// 库里这一列可空且唯一：空串在 Go 侧统一写成 NULL（见 nullEmpty），
	// 所以多行「未绑定手机号」不会在唯一索引上互撞。
	Phone          string     `db:"phone"`
	Nickname       string     `db:"nickname"`
	AvatarURL      string     `db:"avatar_url"`
	Gender         string     `db:"gender"` // unknown / male / female
	Birthday       *time.Time `db:"birthday"`
	RegionCode     string     `db:"region_code"`
	RegionName     string     `db:"region_name"`
	Status         string     `db:"status"`          // active / disabled / deleted
	RegisterSource string     `db:"register_source"` // wechat_miniapp / wechat_phone / sms_code
	LastLoginAt    *time.Time `db:"last_login_at"`
	LastLoginIP    string     `db:"last_login_ip"`
	LoginCount     int        `db:"login_count"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
}

// 账号状态。禁用与注销都要在鉴权时拦截：老系统的 users.status 存在但登录链路
// 根本不查，禁用用户照样能登录，这个债不能带过来。
const (
	// UserStatusActive 正常
	UserStatusActive = "active"
	// UserStatusDisabled 禁用，管理员操作，可恢复
	UserStatusDisabled = "disabled"
	// UserStatusDeleted 已注销，用户自己发起，软删除
	UserStatusDeleted = "deleted"
)

// 注册/登录来源，与 user_login_events.login_type 的取值保持一致。
const (
	// LoginTypeWechatMiniapp 微信一键登录（只拿得到 openid）
	LoginTypeWechatMiniapp = "wechat_miniapp"
	// LoginTypeWechatPhone 微信手机号快捷登录（getPhoneNumber）
	LoginTypeWechatPhone = "wechat_phone"
	// LoginTypeSMSCode 手机号 + 短信验证码
	LoginTypeSMSCode = "sms_code"
)

// ValidLoginType 报告 t 是否是 user_login_events.login_type 允许的取值。
// 库里有同名 CHECK 约束，这里再判一次是为了在写库前就返回一个能说清原因的
// 400，而不是把 SQLSTATE 23514 漏到响应里。
func ValidLoginType(t string) bool {
	switch t {
	case LoginTypeWechatMiniapp, LoginTypeWechatPhone, LoginTypeSMSCode:
		return true
	default:
		return false
	}
}

// ValidGender 报告 g 是否是 users.gender 允许的取值。
func ValidGender(g string) bool {
	switch g {
	case "unknown", "male", "female":
		return true
	default:
		return false
	}
}
