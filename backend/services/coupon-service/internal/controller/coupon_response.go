package controller

import (
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

// 这一层只做 model → DTO 的搬运：model 上只有 db tag，直接序列化会输出
// PascalCase，前端按下划线取值会全部落空。所有对外响应都必须经过这里。

func couponTypeResponse(t *model.CouponType) *dto.CouponTypeResponse {
	return &dto.CouponTypeResponse{
		ID:          t.ID,
		Code:        t.Code,
		Name:        t.Name,
		Description: t.Description,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func couponTypeResponses(items []*model.CouponType) []*dto.CouponTypeResponse {
	out := make([]*dto.CouponTypeResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, couponTypeResponse(item))
		}
	}
	return out
}

func templateResponse(t *model.CouponTemplate) *dto.CouponTemplateResponse {
	// 空切片而不是 nil：response 上这两个字段没有 omitempty，nil 会序列化成 null，
	// 前端就得多写一层兜底才能判断「不限」。
	brandIDs := make([]string, 0, len(t.Scopes))
	storeIDs := make([]string, 0, len(t.Scopes))
	for _, s := range t.Scopes {
		if s == nil {
			continue
		}
		switch s.ScopeType {
		case "brand":
			brandIDs = append(brandIDs, s.ScopeID)
		case "store":
			storeIDs = append(storeIDs, s.ScopeID)
		}
	}
	return &dto.CouponTemplateResponse{
		ID:                  t.ID,
		CouponTypeID:        t.CouponTypeID,
		MerchantID:          t.MerchantID,
		Name:                t.Name,
		ShortTitle:          t.ShortTitle,
		Description:         t.Description,
		CoverImage:          t.CoverImage,
		UseRuleDescription:  t.UseRuleDescription,
		FaceValue:           t.FaceValue,
		MinPurchaseAmount:   t.MinPurchaseAmount,
		PurchasePrice:       t.PurchasePrice,
		TotalQuantity:       t.TotalQuantity,
		IssuedQuantity:      t.IssuedQuantity,
		ReservedQuantity:    t.ReservedQuantity,
		ValidityMode:        t.ValidityMode,
		ValidFrom:           t.ValidFrom,
		ValidTo:             t.ValidTo,
		ValidDays:           t.ValidDays,
		ClaimLimitMode:      t.ClaimLimitMode,
		ClaimPeriodUnit:     t.ClaimPeriodUnit,
		ClaimPeriodQuantity: t.ClaimPeriodQuantity,
		RedemptionType:      t.RedemptionType,
		ExternalUseMethod:   t.ExternalUseMethod,
		AuditStatus:         t.AuditStatus,
		AuditRemark:         t.AuditRemark,
		AuditedAt:           t.AuditedAt,
		AuditedBy:           t.AuditedBy,
		Status:              t.Status,
		IsHot:               t.IsHot,
		IsRecommended:       t.IsRecommended,
		SortOrder:           t.SortOrder,
		Visible:             t.Visible,
		CreatedBy:           t.CreatedBy,
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
		BrandIDs:            brandIDs,
		StoreIDs:            storeIDs,
	}
}

func templateResponses(items []*model.CouponTemplate) []*dto.CouponTemplateResponse {
	out := make([]*dto.CouponTemplateResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, templateResponse(item))
		}
	}
	return out
}

func batchResponse(b *model.CouponBatch) *dto.CouponBatchResponse {
	return &dto.CouponBatchResponse{
		ID:               b.ID,
		TemplateID:       b.TemplateID,
		BatchNo:          b.BatchNo,
		Source:           b.Source,
		TotalQuantity:    b.TotalQuantity,
		ReservedQuantity: b.ReservedQuantity,
		IssuedQuantity:   b.IssuedQuantity,
		ReleasedQuantity: b.ReleasedQuantity,
		Status:           b.Status,
		OrderID:          b.OrderID,
		UserID:           b.UserID,
		RequestID:        b.RequestID,
		CreatedBy:        b.CreatedBy,
		CreatedAt:        b.CreatedAt,
		UpdatedAt:        b.UpdatedAt,
	}
}

func batchResponses(items []*model.CouponBatch) []*dto.CouponBatchResponse {
	out := make([]*dto.CouponBatchResponse, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, batchResponse(item))
		}
	}
	return out
}

// userCouponResponse 刻意不带 RedemptionCodeDigest：核销凭据摘要属于敏感字段，
// 后台列表和详情都不回传。
func userCouponResponse(c *model.UserCoupon) *dto.UserCouponDetail {
	return &dto.UserCouponDetail{
		ID:                c.ID,
		TemplateID:        c.TemplateID,
		BatchID:           c.BatchID,
		UserID:            c.UserID,
		CouponTypeCode:    c.CouponTypeCode,
		ClaimType:         c.ClaimType,
		IssueReason:       c.IssueReason,
		Status:            c.Status,
		FaceValue:         c.FaceValue,
		MinPurchaseAmount: c.MinPurchaseAmount,
		RedemptionType:    c.RedemptionType,
		ValidFrom:         c.ValidFrom,
		ExpiredAt:         c.ExpiredAt,
		ClaimedAt:         c.ClaimedAt,
		HeldAt:            c.HeldAt,
		RedeemedAt:        c.RedeemedAt,
		RefundedAt:        c.RefundedAt,
		InvalidatedAt:     c.InvalidatedAt,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
	}
}

func userCouponResponses(items []*model.UserCoupon) []*dto.UserCouponDetail {
	out := make([]*dto.UserCouponDetail, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, userCouponResponse(item))
		}
	}
	return out
}
