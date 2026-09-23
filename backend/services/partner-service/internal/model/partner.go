// Package model 是 partner-service 的库内形状：合作方、API 密钥、调用日志。
//
// 三个结构体与 migrations/partner 的三张表一一对应，**只有本服务拥有它们**。合作方的
// 用户、订单、券、会员资格在这个包里一个字段都没有——那些是别的服务的事实，本服务只把
// 一个 partner_id 当值传过去（见 internal/client 与 internal/service 的转发）。
//
// 一个刻意的形状选择：**签名密钥的明文不出现在这里的任何结构体上**。库里那一列是密文信封
// （platform/secret），明文只在两个瞬间存在——签发的那一刻（写进创建响应就没了）与验签的
// 那一刻（从信封里解出来就立刻用掉，用完不留在任何长期存活的结构里）。把明文放进 APIKey
// 结构体的代价是它会跟着结构体到处走，而某一天有人会把它 json.Marshal 进一个日志。
package model

import (
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/secret"
)

// Status 是合作方与密钥共用的启停词表。值与库里的 CHECK 约束逐字一致。
//
// 共用一个类型不是省事：两处判断的语义确实是同一个（「还能不能用」），而分成两个类型只会
// 让「把密钥的状态赋给合作方」这种错误在编译期看不出来——它们本来就不该互赋，但共用词表
// 让互赋至少在值上是诚实的。
type Status string

const (
	StatusEnabled  Status = "enabled"
	StatusDisabled Status = "disabled"
)

// PartnerAccount 是 partner_accounts 的一行。
type PartnerAccount struct {
	ID           string
	Code         string
	Name         string
	ContactName  string
	ContactPhone string
	ContactEmail string
	Description  string
	Status       Status
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AllowsAt 回答「此刻这个合作方本身能不能调用」。
//
// 与合作方名下的密钥是**与**的关系（见 APIKey.AllowsAt）：两个都要过。这不是防御性编程，
// 是那张表注释里写清楚的两层语义——「这家合作方我们不想再合作了」与「这一把钥匙泄了要换
// 一把」是两次不同的操作，任何一次都该立刻生效。
func (a *PartnerAccount) AllowsAt(now time.Time) bool {
	if a == nil {
		return false
	}
	if a.Status != StatusEnabled {
		return false
	}
	// 边界取「到点即失效」：expires_at 与被签名的 X-Timestamp 都是秒级，留一个「恰好等于」
	// 的模糊区间只会让两侧对同一次调用给出不同答案。
	return a.ExpiresAt == nil || now.Before(*a.ExpiresAt)
}

// APIKey 是 partner_api_keys 的一行，**不含明文签名密钥**（理由见包注释）。
type APIKey struct {
	ID        string
	PartnerID string
	// APIKey 是 X-API-Key 的值。它是公开标识，明文存、明文读。
	APIKey string
	// Secret 是签名密钥的密文信封。它只在验签那一刻被 Open。
	Secret     secret.Envelope
	APIKeyMask string
	SecretMask string
	Name       string
	Status     Status
	ExpiresAt  *time.Time
	// IPWhitelist 为空表示不限制来源（不是「全拒」），见 migrations/partner。
	IPWhitelist []string
	// RateLimitPerMinute 按密钥计，不是按来源 IP。
	RateLimitPerMinute int
	LastUsedAt         *time.Time
	CallCount          int64
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AllowsAt 回答「此刻这一把钥匙能不能调用」。合作方的总开关由调用方另行判定。
func (k *APIKey) AllowsAt(now time.Time) bool {
	if k == nil {
		return false
	}
	if k.Status != StatusEnabled {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

// CallLog 是 partner_call_logs 的一行。
//
// 它是**只增**的：本服务没有任何 UPDATE / DELETE 路径碰这张表。
type CallLog struct {
	ID           int64
	PartnerID    string
	APIKeyID     string
	APIKeyMask   string
	Method       string
	Path         string
	Query        string
	RequestIP    string
	RequestBody  string
	ResponseBody string
	StatusCode   int
	DurationMS   int64
	// ErrorCode 是内部失败原因，**只进这张表、不回给调用方**（防枚举）。
	ErrorCode string
	CreatedAt time.Time
}

// SecretSlot 是签名密钥在密文信封里的槽名。
//
// 每把密钥只有一个密钥，所以槽名是一个常量而不是一个字段。它**必须**在两边逐字一致
// （签发时 Seal、验签时 Open）：platform/secret 把槽名绑进 AAD，两边写得不一致的表现是
// 「每一把新签的密钥都解不开」，而错误串里只有 "could not be decrypted"——那会让人先去
// 怀疑主密钥，而不是怀疑一个字符串常量。
//
// 叫 signing 而不是 secret：它描述的是这把密钥**用来干什么**（签名），而不是它是什么。
// 将来若真的出现第二个槽（比如一个回调校验密钥），命名规则已经在这里了。
const SecretSlot = "signing"

// DefaultRateLimitPerMinute 是密钥行上没给额度时的兜底，也是库上那一列的 DEFAULT。
//
// 放在 model 而不是 ingress 或 service：**它必须只有一处**。签发的写路径要把「没填」
// （请求里的 0）归一成它，读路径（ingress 的限流）要在拿到一个非正数时用它兜底，两边各写
// 一个 60 的那天，「页面上显示 60 而实际按 100 在限」会变成一个没人能解释的差异。
//
// 值 60 与老系统同档（它的默认是 60/分钟），也与 migrations/partner 里
// partner_api_keys.rate_limit_per_minute 那一列的 DEFAULT 逐字一致。
const DefaultRateLimitPerMinute = 60

// UnknownPartnerID 是「认不出调用方」时写进日志的占位 id（全零 UUID）。
//
// 为什么不用 NULL：这三列是 NOT NULL，而「一次带着不存在的 X-API-Key 的调用」恰恰是最需要
// 留下记录的那一类——它是有人在试。把它写成 NULL 会让「没有记录」与「记录了一个不认识的人」
// 收敛成同一种查询结果，而那正是排查时最想分开的两件事。
const UnknownPartnerID = "00000000-0000-0000-0000-000000000000"
