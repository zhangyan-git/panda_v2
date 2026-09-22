package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

var (
	ErrTemplateRepositoryUnavailable = errors.New("coupon template repository is not configured")
	ErrInvalidTemplate               = errors.New("invalid coupon template")
	// ErrInvalidTemplateAmount 单独一个哨兵：金额这类「参数问题」必须能从
	// ErrInvalidTemplate 里分出来，否则客户端拿到的还是那句笼统的
	// invalid coupon template，看不出该改哪个字段。
	ErrInvalidTemplateAmount = errors.New("invalid coupon template amount")
	ErrInvalidAudit          = errors.New("invalid template audit")
	ErrInvalidTemplateStatus = errors.New("invalid template status")
)

// maxMoneyCents 是单个金额字段的上限，单位「分」。
//
// 列是 BIGINT，理论上能装到 9.2e18；但响应里的金额是 JSON number，JavaScript
// 超过 2^53-1 就开始丢精度——前端算出来的分再传回来就不是同一个数了。所以在
// 2^53-1 处截断（约 90 万亿元，远超业务量级），顺带把「把分当元填」这类错单位
// 写进库的路径也堵上。
const maxMoneyCents = 1<<53 - 1

// checkMoney 校验一个「分」金额：非负、不超上限。
//
// 这里不再有字符串解析：类型不对（传 "12.5"、传小数、传字符串）在 controller
// 的 json.Unmarshal 阶段就是 400，根本到不了这一层。以前那套 decimalDigits
// 是给 Postgres 的 numeric 列兜底的——列改成 BIGINT 之后，它要防的那个 500
// 已经不存在了。
func checkMoney(field string, cents int64) error {
	if cents < 0 || cents > maxMoneyCents {
		return fmt.Errorf("%w: %s", ErrInvalidTemplateAmount, field)
	}
	return nil
}

func templateFromRequest(req dto.TemplateRequest, actor string) (*model.CouponTemplate, error) {
	// 报错要指到具体字段，否则客户端还是只知道「invalid coupon template」。
	for _, m := range []struct {
		field string
		cents int64
	}{
		{"faceValue", req.FaceValue},
		{"minPurchaseAmount", req.MinPurchaseAmount},
		{"purchasePrice", req.PurchasePrice},
	} {
		if err := checkMoney(m.field, m.cents); err != nil {
			return nil, err
		}
	}
	return &model.CouponTemplate{CouponTypeID: req.CouponTypeID, MerchantID: req.MerchantID, Name: req.Name, ShortTitle: req.ShortTitle, Description: req.Description, CoverImage: req.CoverImage, UseRuleDescription: req.UseRuleDescription, FaceValue: req.FaceValue, MinPurchaseAmount: req.MinPurchaseAmount, PurchasePrice: req.PurchasePrice, TotalQuantity: req.TotalQuantity, ValidityMode: req.ValidityMode, ValidFrom: req.ValidFrom, ValidTo: req.ValidTo, ValidDays: req.ValidDays, ClaimLimitMode: req.ClaimLimitMode, ClaimPeriodUnit: req.ClaimPeriodUnit, ClaimPeriodQuantity: req.ClaimPeriodQuantity, RedemptionType: req.RedemptionType, ExternalUseMethod: req.ExternalUseMethod, IsHot: req.IsHot, IsRecommended: req.IsRecommended, SortOrder: req.SortOrder, Visible: req.Visible, CreatedBy: func() *string {
		if actor == "" {
			return nil
		}
		return &actor
	}()}, nil
}

// normalizeScopes 把请求里的品牌/门店 id 数组归一化成范围行。
//
// 只做形状校验（枚举 + UUID + 去重），不做存在性校验：coupon-service 与商户库是
// 两个库，没有到 merchant-service 的 client，品牌/门店的真伪它验不了。这和
// merchant_id 的处理一致（裸 UUID、无 FK），真值靠后台的下拉框保证。空串当没填，
// 空数组表示不限。
func normalizeScopes(brandIDs, storeIDs []string) ([]*model.CouponTemplateScope, bool) {
	out := make([]*model.CouponTemplateScope, 0, len(brandIDs)+len(storeIDs))
	seen := make(map[string]struct{}, len(brandIDs)+len(storeIDs))
	for _, level := range []struct {
		typ string
		ids []string
	}{{"brand", brandIDs}, {"store", storeIDs}} {
		for _, raw := range level.ids {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, err := uuid.Parse(id); err != nil {
				return nil, false
			}
			key := level.typ + ":" + id
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, &model.CouponTemplateScope{ScopeType: level.typ, ScopeID: id})
		}
	}
	return out, true
}

func validateTemplate(t *model.CouponTemplate) bool {
	if !(strings.TrimSpace(t.CouponTypeID) != "" && strings.TrimSpace(t.Name) != "" && t.TotalQuantity > 0 && (t.ValidityMode == "fixed" || t.ValidityMode == "relative") && (t.ClaimLimitMode == "once_ever" || t.ClaimLimitMode == "unlimited_after_use" || t.ClaimLimitMode == "periodic") && (t.RedemptionType == "platform" || t.RedemptionType == "external_code" || t.RedemptionType == "show_qr")) {
		return false
	}
	// 下面两条是库里那两条 CHECK 的等值翻译（001_coupon_templates）。写在这里不是
	// 重复劳动：没有它们，一个直接调接口的调用方（后台表单已经拦住了，接口没有）会
	// 拿到一个 500 —— 约束失败的报错讲不出「哪一栏填错了」，而这本来是一句
	// 400 invalid coupon template 就能说清的事。判空用 nil 而不是空串：两列的 CHECK
	// 都是 `IS NULL OR IN (...)`，所以 "" 与 NULL 在库里不是一回事，判空串会放过
	// 一个必然撞约束的请求。
	if t.ClaimLimitMode == "periodic" {
		if t.ClaimPeriodUnit == nil || t.ClaimPeriodQuantity == nil {
			return false
		}
	} else if t.ClaimPeriodUnit != nil || t.ClaimPeriodQuantity != nil {
		return false
	}
	if t.RedemptionType == "external_code" && t.ExternalUseMethod == nil {
		return false
	}
	return true
}
func (s *CouponService) templateRepo() (repository.CouponTemplateRepository, error) {
	r, ok := s.batches.(repository.CouponTemplateRepository)
	if !ok || r == nil {
		return nil, ErrTemplateRepositoryUnavailable
	}
	return r, nil
}
func (s *CouponService) ListTemplates(ctx context.Context, q dto.CouponTemplateQuery) ([]*model.CouponTemplate, int64, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, 0, e
	}
	if q.Page < 1 {
		q.Page = 1
	}
	// 兜底：HTTP 层已由 api.ParsePage 按 dto.MaxPageSize 挡过一道，这里是防止别的
	// 调用方（测试、将来的 gRPC）绕过 controller 直接传越界值。
	if q.PageSize < 1 || q.PageSize > dto.MaxPageSize {
		q.PageSize = 20
	}
	return r.ListTemplates(ctx, q)
}
func (s *CouponService) GetTemplate(ctx context.Context, id string) (*model.CouponTemplate, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrInvalidTemplate
	}
	return r.GetTemplate(ctx, id)
}
func (s *CouponService) CreateTemplate(ctx context.Context, req dto.TemplateRequest, actor string) (*model.CouponTemplate, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	t, err := templateFromRequest(req, actor)
	if err != nil {
		return nil, err
	}
	scopes, ok := normalizeScopes(req.BrandIDs, req.StoreIDs)
	if !validateTemplate(t) || !ok {
		return nil, ErrInvalidTemplate
	}
	t.Scopes = scopes
	return r.CreateTemplate(ctx, t)
}
func (s *CouponService) UpdateTemplate(ctx context.Context, id string, req dto.TemplateRequest) (*model.CouponTemplate, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	t, err := templateFromRequest(req, "")
	if err != nil {
		return nil, err
	}
	t.ID = strings.TrimSpace(id)
	scopes, ok := normalizeScopes(req.BrandIDs, req.StoreIDs)
	if !validateTemplate(t) || !ok || t.ID == "" {
		return nil, ErrInvalidTemplate
	}
	t.Scopes = scopes
	return r.UpdateTemplate(ctx, t)
}
func (s *CouponService) DeleteTemplate(ctx context.Context, id string) error {
	r, e := s.templateRepo()
	if e != nil {
		return e
	}
	if strings.TrimSpace(id) == "" {
		return ErrInvalidTemplate
	}
	return r.DeleteTemplate(ctx, id)
}
func (s *CouponService) AuditTemplate(ctx context.Context, id string, req dto.AuditRequest, actor string) (*model.CouponTemplate, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	if req.Status != "approved" && req.Status != "rejected" {
		return nil, ErrInvalidAudit
	}
	return r.AuditTemplate(ctx, id, req.Status, req.Remark, actor)
}
func (s *CouponService) SetTemplateStatus(ctx context.Context, id string, req dto.StatusRequest) (*model.CouponTemplate, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	switch req.Status {
	case "draft", "active", "disabled", "closed":
	default:
		return nil, ErrInvalidTemplateStatus
	}
	return r.SetTemplateStatus(ctx, id, req.Status)
}
func (s *CouponService) TemplateStats(ctx context.Context, id string) (*repository.TemplateStats, error) {
	r, e := s.templateRepo()
	if e != nil {
		return nil, e
	}
	return r.TemplateStats(ctx, id)
}
