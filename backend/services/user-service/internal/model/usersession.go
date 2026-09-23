package model

import "time"

// UserSession 是一枚已签发的 Refresh Token 在库里的那一行。
//
// 两张表共用它：user_sessions（小程序顾客，UserID 是 users.id）与 admin_sessions
// （平台管理员，UserID 是 admin_users.id，见 migrations/identity）。两表同形，字段含义
// 逐字相同，所以没有第二个结构体；**读代码时要注意 UserID 指向哪张账号表，
// 取决于这一行是从哪个仓库取回来的**。
//
// 表里存的是 RefreshTokenHash 而不是 token 本身：这一行一旦随备份或导出泄露，
// 明文就等于可直接冒用的登录态。
//
// 这里的哈希是确定性的 SHA-256，和 user_sms_codes.code_hash 的 bcrypt 不同：
// 刷新时只有 token 原文，要靠哈希反查这一行，所以哈希必须无盐可索引——带随机盐
// 的 bcrypt 每次结果都不同，只能全表逐行比对，那个 UNIQUE 约束也就没了意义。
// 这么做是安全的，因为 token 原文是 256 位随机字节，没有可枚举的空间；验证码
// 只有 6 位，才必须用慢哈希顶住离线爆破。两者要求相反，不该套用同一种算法。
//
// 撤销靠 RevokedAt（NULL 表示有效），不是删行：refresh token 被撤销后仍然可能
// 被再拿来用一次，那时要能查出一枚「已撤销的 token 又被使用」，这是 refresh
// token 轮换里发现盗用的唯一信号，行删了就没了。
type UserSession struct {
	ID               string     `db:"id"`
	UserID           string     `db:"user_id"`
	RefreshTokenHash string     `db:"refresh_token_hash"`
	IssuedAt         time.Time  `db:"issued_at"`
	ExpiresAt        time.Time  `db:"expires_at"`
	RevokedAt        *time.Time `db:"revoked_at"`
	RevokeReason     string     `db:"revoke_reason"`
	LastUsedAt       *time.Time `db:"last_used_at"`
	IP               string     `db:"ip"`
	UserAgent        string     `db:"user_agent"`
	CreatedAt        time.Time  `db:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at"`
}

// 会话撤销原因，写进 user_sessions.revoke_reason。分开记是为了事后能判断
// 「这个人的登录态为什么没了」：正常退出和疑似盗用是两回事。
const (
	// RevokeReasonLogout 用户主动退出
	RevokeReasonLogout = "logout"
	// RevokeReasonRotated 刷新时轮换，旧的那枚正常作废
	RevokeReasonRotated = "rotated"
	// RevokeReasonReuseDetected 已经轮换/撤销过的 token 又被使用，
	// 视为泄露，连同该用户的活动会话一起撤销
	RevokeReasonReuseDetected = "reuse_detected"
	// RevokeReasonDisabled 管理员在后台禁用/注销了账号，连带撤销其在场会话。
	// 和 logout 分开记：两者都让登录态消失，但一个是用户自己的动作，
	// 一个不是——用户来问「我怎么突然要重新登录」，答案就在这个字段里。
	RevokeReasonDisabled = "disabled"
)
