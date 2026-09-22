package service

import (
	"context"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/partnerkey"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
)

// 这个文件是**密钥**的治理：签发、改访问控制、启停、列表。
//
// # 明文只在这一条路径上存在
//
// 签名密钥的明文在 IssueAPIKey 里产生（partnerkey.Issue 取 32 位随机串），在同一处被封进
// 密文信封（platform/secret 的 Seal），然后跟着返回值走到 controller 的响应体——**去两个
// 地方，两个都不落库**：一个进 HTTP 响应（只此一次，见 dto.APIKeyIssued），另一个被 Seal
// 吃掉变成密文。
//
// 这个包里没有第二个地方碰明文：改访问控制（UpdateAPIKey）与启停（SetAPIKeyStatus）连密钥
// 行上的 secret_sealed 都不读——它们改的是另外几列。
//
// # 没有「轮换密钥」这个方法
//
// 轮换由两次调用组成：签发一把新的 + 停用旧的（见 repository.UpdateAPIKey 的说明）。做成
// 一个「就地换密钥」的原子操作看起来更省事，代价是对接方在同一瞬间失去调用能力——而密钥的
// 交接必然有一次双方不同步的窗口，那个窗口只能由「新旧并存一段时间」来吸收。

// ListAPIKeys 读一个合作方名下的全部密钥。**只有掩码**（见 dto.APIKeyItem）。
func (s *AdminService) ListAPIKeys(ctx context.Context, partnerID string) ([]*model.APIKey, error) {
	return s.repository.ListAPIKeys(ctx, partnerID)
}

// IssueAPIKey 签发一把新密钥，并把**明文**带回给调用方一次。
//
// # 顺序是有讲究的
//
// 先校验（不碰库、不碰随机数）、再取随机数与封存、最后才插入。反过来的话，一次「白名单里
// 写错一个字母」的请求会在库里留下一行已经算好密钥但没插入成功的……不，它什么都没留下，
// 只是白花一次 crypto/rand 与一次 AES-GCM。这条顺序真正的意义在于**失败点**：任何一步失败
// 时，库里都还没有这一行——不存在「密钥行在了、但运营没拿到明文」这种只能靠重新签一把来
// 收拾的中间态。
//
// # 白名单为什么用 ingress 的解析器
//
// 因为它就是**读**那一侧的解析器（中间件每个请求都用它判来源）。写入口换一个「大概等价」的
// 校验，两边迟早会在某个取值上分家（例如 `10.0.0.1/24` 这种主机位不为零的写法），而分家的
// 表现是「存得进去、每次调用都 401」。共用一个函数是这一条唯一的实现方式。
func (s *AdminService) IssueAPIKey(ctx context.Context, partnerID string, in dto.APIKeyInput, operator string) (*IssuedAPIKey, error) {
	if strings.TrimSpace(operator) == "" {
		return nil, ErrOperatorRequired
	}
	access, err := parseKeyAccess(in)
	if err != nil {
		return nil, err
	}

	issued, err := partnerkey.Issue()
	if err != nil {
		return nil, err
	}
	// KindText：签名密钥是一行随机串（不是 PEM）。kind 进 AAD，所以它与验签时 Open 用的那个
	// 必须一致——两边都写死 secret.KindText，没有任何一处从配置里读它。
	envelope, err := s.keyring.Seal(model.SecretSlot, secret.KindText, issued.Secret)
	if err != nil {
		// 主密钥缺失或形状不对。这时**不能**退化成明文入库：那一列是 NOT NULL，写明文就
		// 等于把「能签名的东西」放进一张 psql 里谁都读得到的表。宁可这次签发失败。
		return nil, ErrSecretUnavailable
	}

	key, err := s.repository.CreateAPIKey(ctx, partnerID, repository.APIKeyWrite{
		APIKey:             issued.APIKey,
		Secret:             envelope,
		APIKeyMask:         issued.APIKeyMask,
		SecretMask:         issued.SecretMask,
		Name:               access.Name,
		ExpiresAt:          access.ExpiresAt,
		IPWhitelist:        access.IPWhitelist,
		RateLimitPerMinute: access.RateLimitPerMinute,
	}, operator)
	if err != nil {
		return nil, err
	}
	return &IssuedAPIKey{Key: key, Secret: issued.Secret}, nil
}

// UpdateAPIKey 整份覆盖一把密钥的**可改列**：备注、有效期、IP 白名单、每分钟额度。
//
// 密钥本身（api_key / secret_sealed）不在这里，而且不是「忽略」而是**没有这个入参**
// ——dto.APIKeyInput 里根本没有 secret 字段（见那个类型的说明）。
func (s *AdminService) UpdateAPIKey(ctx context.Context, partnerID, keyID string, in dto.APIKeyInput) (*model.APIKey, error) {
	access, err := parseKeyAccess(in)
	if err != nil {
		return nil, err
	}
	return s.repository.UpdateAPIKey(ctx, partnerID, keyID, repository.APIKeyWrite{
		Name:               access.Name,
		ExpiresAt:          access.ExpiresAt,
		IPWhitelist:        access.IPWhitelist,
		RateLimitPerMinute: access.RateLimitPerMinute,
	})
}

// SetAPIKeyStatus 启用或停用一把密钥。**下一个请求就生效**（中间件现查这一行，没有缓存）。
func (s *AdminService) SetAPIKeyStatus(ctx context.Context, partnerID, keyID, status string) (*model.APIKey, error) {
	normalized, err := normalizeStatus(status)
	if err != nil {
		return nil, err
	}
	return s.repository.SetAPIKeyStatus(ctx, partnerID, keyID, normalized)
}

// keyAccess 是一把密钥上「访问控制」那几列，签发与修改共用。
type keyAccess struct {
	Name               string
	ExpiresAt          *time.Time
	IPWhitelist        []string
	RateLimitPerMinute int
}

// parseKeyAccess 校验并归一密钥的访问控制字段。
//
// 签发与修改共用同一份：分开写的那天会出现「签发时白名单校验过了、修改时没校验」，而修改
// 那条路是**运营改一个字母就会走到**的——一条写坏的白名单让这家合作方从下一个请求起全部
// 401（中间件解析失败即整条作废，失败关闭）。
//
// name 可以为空（库里那一列允许空串）：它是给人看的备注，不是判据。**不 trim 掉
// 中间的空格**（「生产环境 主用」是正常的备注），只去两端的。
func parseKeyAccess(in dto.APIKeyInput) (keyAccess, error) {
	expiresAt, err := parseExpiresAt(in.ExpiresAt)
	if err != nil {
		return keyAccess{}, err
	}
	rateLimit, err := normalizeRateLimit(in.RateLimitPerMinute)
	if err != nil {
		return keyAccess{}, err
	}
	// 白名单：解析一遍只为校验，**存的是调用方填的原串**（解析结果会被丢掉）。回显成归一化
	// 后的形状（例如把 10.0.0.1 变成 10.0.0.1/32）会让运营看到与自己填的不一样的东西，
	// 而那个差别的解释成本全在他那边。
	if _, err := ingress.ParseIPAllowList(in.IPWhitelist); err != nil {
		return keyAccess{}, ErrIPWhitelistInvalid
	}
	return keyAccess{
		Name:               strings.TrimSpace(in.Name),
		ExpiresAt:          expiresAt,
		IPWhitelist:        in.IPWhitelist,
		RateLimitPerMinute: rateLimit,
	}, nil
}
