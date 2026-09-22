package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

var (
	ErrInvalidIssueRequest = errors.New("invalid coupon issue request")
	// ErrIdempotencyConflict 就是仓储那一个，不另起一个同义的值：并发那一格是在仓储里判出来的
	// （见 beginIdempotentOperation），两处如果不是同一个值，controller 只认得这一层，仓储报的
	// 冲突就会掉进 default 变成 500。
	ErrIdempotencyConflict = repository.ErrIdempotencyConflict
)

type IssueResult = dto.IssueCouponsResponse

// issueHashPayload 是发券幂等哈希的**冻结**输入形状。
//
// 这些 tag 故意留着 snake_case，且字段顺序必须与 dto.IssueCouponsRequest 一致：
// 旧代码直接 json.Marshal 那个 DTO 算哈希，所以按这份结构体渲染出来的是逐字节
// 相同的 JSON，改名前后哈希不变——这就是为什么线上已存在的 Idempotency-Key
// 重试仍然命中，而不是被判成 IDEMPOTENCY_CONFLICT。
//
// 这张表只存了摘要、没存原始请求体，所以哈希无法事后迁移，只能这样钉住。
// 改动这里的任何一个 tag 或字段顺序，都等于让全部存量 key 失效。
type issueHashPayload struct {
	TemplateID          string   `json:"template_id"`
	UserIDs             []string `json:"user_ids"`
	QuantityPerUser     int      `json:"quantity_per_user"`
	Reason              string   `json:"reason"`
	SkipLimitValidation bool     `json:"skip_limit_validation"`
}

func issueRequestHash(req dto.IssueCouponsRequest) string {
	raw, err := json.Marshal(issueHashPayload{
		TemplateID:          req.TemplateID,
		UserIDs:             req.UserIDs,
		QuantityPerUser:     req.QuantityPerUser,
		Reason:              req.Reason,
		SkipLimitValidation: req.SkipLimitValidation,
	})
	if err != nil {
		// 结构体里全是 string/int/[]string，Marshal 不会失败；真失败也只可能
		// 是编码器坏了，此时算出的哈希没有意义，直接返回空串让 Find 落空。
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type IssueRepository interface {
	IssueCoupons(context.Context, string, string, string, dto.IssueCouponsRequest) (*IssueResult, error)
}

// IssueCoupons validates and dispatches the administrative issue operation.
func (s *CouponService) IssueCoupons(ctx context.Context, idempotencyKey, actorID string, req dto.IssueCouponsRequest) (*IssueResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" || strings.TrimSpace(req.TemplateID) == "" || len(req.UserIDs) < 1 || len(req.UserIDs) > 1000 || req.QuantityPerUser < 1 || req.QuantityPerUser > 100 || len([]rune(req.Reason)) > 200 {
		return nil, ErrInvalidIssueRequest
	}
	seen := make(map[string]struct{}, len(req.UserIDs))
	normalizedIDs := make([]string, 0, len(req.UserIDs))
	for _, id := range req.UserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, ErrInvalidIssueRequest
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		normalizedIDs = append(normalizedIDs, id)
	}
	req.UserIDs = normalizedIDs
	hash := issueRequestHash(req)
	if s.idempotency != nil {
		existing, err := s.idempotency.Find(ctx, "admin.coupons.issue", idempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if existing.RequestHash != hash {
				return nil, ErrIdempotencyConflict
			}
			if existing.Status == "processing" {
				return nil, repository.ErrIdempotencyInProgress
			}
			if existing.Status != "succeeded" && !(existing.Status == "" && len(existing.Response) > 0) {
				return nil, fmt.Errorf("idempotency operation has unsupported status %q", existing.Status)
			}
			var out IssueResult
			if err := json.Unmarshal(existing.Response, &out); err != nil {
				return nil, fmt.Errorf("decode idempotent response: %w", err)
			}
			return &out, nil
		}
	}
	issuer, ok := s.batches.(IssueRepository)
	if !ok {
		return nil, errors.New("coupon issue repository is not configured")
	}
	return issuer.IssueCoupons(ctx, idempotencyKey, actorID, hash, req)
}
