package service

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

// 这一组守的是**「本来该是 400 的东西不要变成 500」**。
//
// 001_coupon_templates 上有两条跨列的 CHECK：periodic 必须同时有周期单位与张数、
// 其余两档必须两个都为空；external_code 必须给外部核销方式。它们是等值翻译——
// 区别只在报错：约束失败是 500（一句 Postgres 的行话），validateTemplate 不过
// 是 400 invalid coupon template（调用方知道是自己参数不对）。
//
// 后台表单已经拦住了这两档，但接口没有：直接调 PUT 的调用方（对账脚本、将来的
// 商户端）拿到的会是 500。另外判空必须用 nil 而不是空串——两列的 CHECK 都是
// `IS NULL OR IN (...)`，"" 在库里既不等于 NULL 也不在集合里，判空串会放过一个
// 必然撞约束的请求。

func template(overrides func(t *model.CouponTemplate)) *model.CouponTemplate {
	t := &model.CouponTemplate{
		CouponTypeID:   "ctype-1",
		Name:           "满减券",
		TotalQuantity:  10,
		ValidityMode:   "relative",
		ClaimLimitMode: "once_ever",
		RedemptionType: "platform",
	}
	if overrides != nil {
		overrides(t)
	}
	return t
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

func TestValidateTemplateBaseRules(t *testing.T) {
	if !validateTemplate(template(nil)) {
		t.Fatal("一份最小可用的模板应当通过校验")
	}

	for _, tt := range []struct {
		name string
		mut  func(t *model.CouponTemplate)
	}{
		{"没有券类型", func(t *model.CouponTemplate) { t.CouponTypeID = "  " }},
		{"没有名字", func(t *model.CouponTemplate) { t.Name = "" }},
		{"发行量为 0", func(t *model.CouponTemplate) { t.TotalQuantity = 0 }},
		{"有效期模式不认识", func(t *model.CouponTemplate) { t.ValidityMode = "rolling" }},
		{"领取限制不认识", func(t *model.CouponTemplate) { t.ClaimLimitMode = "sometimes" }},
		{"核销方式不认识", func(t *model.CouponTemplate) { t.RedemptionType = "paper" }},
	} {
		if validateTemplate(template(tt.mut)) {
			t.Errorf("%s：应当被拒", tt.name)
		}
	}
}

func TestValidateTemplateClaimPeriodIsPairedWithMode(t *testing.T) {
	// periodic 必须两个都给。
	if validateTemplate(template(func(t *model.CouponTemplate) {
		t.ClaimLimitMode = "periodic"
	})) {
		t.Error("periodic 但周期字段两个都空：应当被拒（库里那条 CHECK 会拒）")
	}
	if validateTemplate(template(func(t *model.CouponTemplate) {
		t.ClaimLimitMode = "periodic"
		t.ClaimPeriodUnit = strPtr("month")
	})) {
		t.Error("periodic 只给了单位没给张数：应当被拒")
	}
	if !validateTemplate(template(func(t *model.CouponTemplate) {
		t.ClaimLimitMode = "periodic"
		t.ClaimPeriodUnit = strPtr("month")
		t.ClaimPeriodQuantity = intPtr(2)
	})) {
		t.Error("periodic 两个都给全：应当通过")
	}

	// 其余两档必须两个都空 —— 从 periodic 切走时表单残留的旧值正是走到这里。
	for _, mode := range []string{"once_ever", "unlimited_after_use"} {
		if validateTemplate(template(func(t *model.CouponTemplate) {
			t.ClaimLimitMode = mode
			t.ClaimPeriodUnit = strPtr("month")
		})) {
			t.Errorf("%s 却带着周期单位：应当被拒（切走模式后的残留值）", mode)
		}
		if validateTemplate(template(func(t *model.CouponTemplate) {
			t.ClaimLimitMode = mode
			t.ClaimPeriodQuantity = intPtr(1)
		})) {
			t.Errorf("%s 却带着周期张数：应当被拒", mode)
		}
	}
}

func TestValidateTemplateExternalCodeNeedsUseMethod(t *testing.T) {
	if validateTemplate(template(func(t *model.CouponTemplate) {
		t.RedemptionType = "external_code"
	})) {
		t.Error("external_code 没有外部核销方式：应当被拒")
	}
	if !validateTemplate(template(func(t *model.CouponTemplate) {
		t.RedemptionType = "external_code"
		t.ExternalUseMethod = strPtr("copy_code")
	})) {
		t.Error("external_code 给了核销方式：应当通过")
	}
	// 平台核销带着一个外部方式不撞约束，不拦。
	if !validateTemplate(template(func(t *model.CouponTemplate) {
		t.ExternalUseMethod = strPtr("copy_code")
	})) {
		t.Error("platform 带着外部核销方式：库里不拦，这里也不该拦")
	}
}
