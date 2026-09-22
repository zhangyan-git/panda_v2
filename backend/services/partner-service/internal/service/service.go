// Package service 是开放平台（合作方接入）的业务层。
//
// 它管两件互不相干的事，分在两个文件里：
//
//   - admin.go / apikey.go —— **治理**：合作方与密钥的增改启停、密钥签发。这是后台的写路径，
//     每一行都挂 partner:manage，每一条都要留审计。
//   - openapi.go —— **转发**：合作方调进来的那几条开放接口。它们不碰本库的任何业务数据，
//     只把请求里那个用户 ID 递给对应的服务。
//
// # 这一层的分工
//
// 校验都在这里，仓储只管落库。理由与其它服务逐字相同：controller 是 HTTP 边界（解析请求、
// 把错误翻成给用户看的中文），而「合作方编码能长什么样」「IP 白名单里写的东西解释得了吗」
// 是业务规则，写两遍就一定会出现两处不一致——那意味着一条存得进去、调用时才失败的密钥。
//
// # 明文密钥只在这一个包里存在
//
// 签名密钥的明文产生于 apikey.go 的 IssueAPIKey（partnerkey.Issue），在同一处被封进密文信封
// （platform/secret 的 Seal），明文只跟着那一个返回值往上走一层到 controller 的响应体里。
// 之后任何一次读（列表、详情、审计快照）拿到的都只有掩码——库里根本没有明文可读。
package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
)

// PartnerRepository 是治理层要的那几个方法。
//
// 定义成接口而不是直接用 *repository.PostgresRepository，理由与 payment-service 的同名接口
// 相同：这一层里最容易出错的是**不碰数据库的那部分**（编码形状、白名单解析、额度归一、
// 有效期解析），而那些逻辑不该为了测它去起一个 PG。
//
// 读方法（合作方列表 / 详情 / 密钥列表 / 调用日志）与写方法在同一个接口里，**不像 payment
// 那样分两个**：那边分是因为「后台只读」与「后台可配」是两枚码、两批接口；这边两枚码的分界
// （partner:read / partner:manage）落在**路由**上（见 routes/admin.go 的 methodRoute），
// 而服务层的行为是同一套——读一个合作方与改一个合作方要用同一份校验和同一套错误值。
type PartnerRepository interface {
	// —— 合作方 ——
	ListPartners(ctx context.Context, q dto.PartnerQuery) ([]*repository.PartnerListRow, int, error)
	FindPartner(ctx context.Context, id string) (*model.PartnerAccount, error)
	CreatePartner(ctx context.Context, in repository.PartnerWrite, createdBy string) (*model.PartnerAccount, error)
	UpdatePartner(ctx context.Context, id string, in repository.PartnerWrite) (*model.PartnerAccount, error)
	SetPartnerStatus(ctx context.Context, id, status string) (*model.PartnerAccount, error)

	// —— 密钥 ——
	ListAPIKeys(ctx context.Context, partnerID string) ([]*model.APIKey, error)
	CreateAPIKey(ctx context.Context, partnerID string, in repository.APIKeyWrite, createdBy string) (*model.APIKey, error)
	UpdateAPIKey(ctx context.Context, partnerID, keyID string, in repository.APIKeyWrite) (*model.APIKey, error)
	SetAPIKeyStatus(ctx context.Context, partnerID, keyID, status string) (*model.APIKey, error)

	// —— 调用日志（只读）——
	ListCallLogs(ctx context.Context, q dto.CallLogQuery) ([]*repository.CallLogRow, int, error)
}

// AdminService 是后台治理服务。
//
// 它不知道「用户」是谁：operator 由 controller 从令牌里取出来、当参数传进来（与
// merchant-service 的 store/brand 那两条写路径同一种做法）。这一层因此可以在没有 HTTP
// 请求的情况下被测，也不会因为某个 handler 忘了校验身份而写出一条没有操作人的审计。
type AdminService struct {
	repository PartnerRepository
	// keyring 是加密签名密钥的主密钥。**可以为 nil**（只测校验那一半的测试），而 nil 的表现
	// 是「签发一律失败」，不是「明文存进去」——platform/secret 的 Seal 在 nil 接收者上返回
	// ErrMasterKeyMissing。
	//
	// 生产上不可能拿到 nil：config.Load 对 partner-service 拒空 PARTNER_SECRET_KEY，
	// cmd/main.go 再把它 Parse 成 Keyring，两步都不通过就起不来。
	keyring *secret.Keyring
}

func NewAdminService(repository PartnerRepository, keyring *secret.Keyring) *AdminService {
	return &AdminService{repository: repository, keyring: keyring}
}

// IssuedAPIKey 是一次密钥签发的产物：库里的那一行，加上**只在这一次响应里出现**的明文。
//
// Secret 是明文签名密钥。它不在 model.APIKey 上（那个结构体会跟着到处走，迟早有人把它
// json.Marshal 进日志），只在这一个类型的这一个字段上——于是「明文能到哪里去」在类型上
// 就是可数的：从 IssueAPIKey 到 controller 的响应体，一条边。
type IssuedAPIKey struct {
	Key *model.APIKey
	// Secret 是签名密钥的明文。
	Secret string
}

// ============================================================
// 校验
// ============================================================

// 校验错误。文案是英文的（Go 的惯例，也让 grep 日志的语义保持清楚），给用户看的中文句子在
// controller 那张表里（见 controller/admin_partner.go 的 writeAdminPartnerError）。
var (
	// ErrCodeInvalid：合作方编码不符合形如 fengxuan / shouchuang_01 的约定。
	//
	// 收紧到小写字母数字下划线，与支付域的编码同一条理由：这个值会出现在调用日志、工单、
	// 对接文档与对账邮件里，允许大写或连字符的结果是「同一家合作方在两个地方写法不同」，
	// 而它建好之后不可修改。
	ErrCodeInvalid = errors.New("partner code must match ^[a-z0-9][a-z0-9_]{1,63}$")
	// ErrNameRequired：名称为空（或只有空白）。库上也有 CHECK，但撞 CHECK 是 23514，冒到
	// handler 只是 500——「名字没填」不该长这样。
	ErrNameRequired = errors.New("partner name is required")
	// ErrExpiresAtInvalid：expiresAt 不是 RFC3339，或者形状对但不是一个真实时刻。
	//
	// 单独一条而不是笼统的「参数不合法」：这个字段的输入来自两个 date picker，错了要能指着
	// 它说话。**不校验「必须在未来」**：把一个已经过去的时间填进去是一个明确的「立刻失效」
	// 的手势（比停用更轻，因为到点之后还能改回来），拦它只会逼运营去点停用。
	ErrExpiresAtInvalid = errors.New("expiresAt must be an RFC3339 timestamp or empty")
	// ErrStatusInvalid：status 不在 enabled / disabled 里。
	ErrStatusInvalid = errors.New("status is not one of the allowed values")
	// ErrRateLimitInvalid：每分钟额度不是正数。
	//
	// 库上有 CHECK (> 0)，所以负数与 0 都进不去；0 在这里**不是错误**，它是「没填」，
	// service 会把它归一成默认额度（见 normalizeRateLimit）。
	ErrRateLimitInvalid = errors.New("rateLimitPerMinute must be a positive integer")
	// ErrIPWhitelistInvalid：IP 白名单里有一条解释不了。
	//
	// 这一条尤其要在写入口拦住：一个写错的 CIDR 存进库之后，中间件解析它时**整条白名单
	// 作废**（失败关闭），表现是这家合作方的每一次调用都 401 + IP_NOT_ALLOWED，而运营在
	// 页面上看到的是一个「填得好好的」白名单。见 ingress.ParseIPAllowList——写与读共用一个
	// 解析器，所以「存得进去」与「用得了」不可能不一致。
	ErrIPWhitelistInvalid = errors.New("ipWhitelist contains an entry that is not a CIDR or an IP address")
	// ErrSecretUnavailable：签名密钥封不起来（没配主密钥 / 主密钥不对）。
	//
	// 它是**我们这边**的故障，不是这次输入的问题：签发一把没有密钥的密钥（库里那列 NOT NULL，
	// 但明文的来源是这个 Seal）会让那把钥匙永远验不过签。
	ErrSecretUnavailable = errors.New("partner signing secret cannot be sealed")
	// ErrErrorCodeInvalid：调用日志的 errorCode 筛选项不在我们写过的那套码里。
	//
	// 词表来自 ingress.ErrorCodes()——中间件真正会写的那些。传一个不认识的码时返回空列表的
	// 代价是运营以为「这段时间没有这类失败」，而真相只是他打字打错了。
	ErrErrorCodeInvalid = errors.New("errorCode is not one of the recorded partner error codes")
	// ErrStatusCodeInvalid：调用日志的 statusCode 筛选项不是一个 HTTP 状态码。
	ErrStatusCodeInvalid = errors.New("statusCode must be a three-digit HTTP status code")
	// ErrOperatorRequired：调用方没有给出操作人。
	//
	// 写路径上它不该出现（路由已经把身份验过了），但它是一条**结构上的**保证：审计里
	// created_by 为空的行意味着「不知道是谁签发的这把密钥」，而那正是事后追责时要看的一列。
	ErrOperatorRequired = errors.New("operator is required")
)

var codePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{1,63}$`)

// normalizeStatus 校验并归一 status。
func normalizeStatus(status string) (string, error) {
	trimmed := strings.TrimSpace(status)
	switch model.Status(trimmed) {
	case model.StatusEnabled, model.StatusDisabled:
		return trimmed, nil
	default:
		return "", ErrStatusInvalid
	}
}

// parseExpiresAt 把请求里的 RFC3339 串转成 *time.Time（空串 → nil）。
//
// 统一转成 UTC 再落库：库里的列是 timestamptz，两种时区写进去的是同一个时刻，但读回来的
// 展示依赖连接时区。归一在这里，读路径就不必猜。
func parseExpiresAt(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, ErrExpiresAtInvalid
	}
	utc := parsed.UTC()
	return &utc, nil
}

// normalizeRateLimit 把「没填」归一成默认额度，并挡下负数。
//
// 0 与负数分开处理，是因为它们的意思完全不同：**0 是「没填」**（前端那一栏留空），而负数
// 只可能来自一次错误的输入。库上那一列是 NOT NULL DEFAULT 60 且有 CHECK (> 0)，所以把 0
// 原样传下去会当场撞 CHECK——一条「额度没填」的 500。
func normalizeRateLimit(perMinute int) (int, error) {
	switch {
	case perMinute == 0:
		return model.DefaultRateLimitPerMinute, nil
	case perMinute < 0:
		return 0, ErrRateLimitInvalid
	default:
		return perMinute, nil
	}
}
