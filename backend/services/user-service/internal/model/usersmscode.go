package model

import "time"

// UserSMSCode 对应 user_sms_codes 表，一个手机号当前有效的短信验证码。
//
// 一个手机号同时只有一条：重发是覆盖而不是新增（见 010 迁移的文件头——否则
// 反复请求就能把猜中的概率乘上去）。
type UserSMSCode struct {
	Phone string `db:"phone"`
	// Purpose 取值同 model.SMSPurpose*，库里有 CHECK 约束。
	Purpose string `db:"purpose"`
	// CodeHash 是验证码的 bcrypt 哈希。这里必须是慢哈希：验证码只有 6 位，
	// 空间是 10^6，裸 SHA 对着一张导出表把候选全算一遍是瞬间的事。
	// 与 user_sessions.refresh_token_hash 的选择相反，理由见那个文件。
	CodeHash string `db:"code_hash"`
	// Attempts 是校验失败次数。达到 MaxSMSCodeAttempts 即作废整条记录，
	// 必须重新获取——6 位码的空间容不下不限次数的试。
	Attempts  int       `db:"attempts"`
	ExpiresAt time.Time `db:"expires_at"`
	CreatedAt time.Time `db:"created_at"`
}

// 验证码用途，与 user_sms_codes.purpose 的 CHECK 约束一致。
const (
	// SMSPurposeLogin 手机号 + 短信验证码登录
	SMSPurposeLogin = "login"
	// SMSPurposeBindPhone 已登录用户绑定或换绑手机号
	SMSPurposeBindPhone = "bind_phone"
)

// MaxSMSCodeAttempts 是单条验证码允许的校验失败次数。上限是每请求重置的，
// 所以它界定的是「一条码能被猜几次」，而不是「一个手机号能被猜几次」——
// 后者由重发频控负责，两者都要有，缺一个都能被绕。
const MaxSMSCodeAttempts = 5

// ValidSMSPurpose 报告 p 是否是 user_sms_codes.purpose 允许的取值。
// 库里有同名 CHECK，这里再判一次是为了在写库前返回一个能说清原因的 400，
// 而不是把 SQLSTATE 23514 漏到响应里（同 model.ValidLoginType 的理由）。
func ValidSMSPurpose(p string) bool {
	switch p {
	case SMSPurposeLogin, SMSPurposeBindPhone:
		return true
	default:
		return false
	}
}
