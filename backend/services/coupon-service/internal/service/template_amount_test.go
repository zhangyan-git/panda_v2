package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// 这一组用例守的还是原来那个线上症状：金额填错必须是一个**参数问题**，
// 不能伪装成服务端故障。只是它现在落在了不同的地方——
//
// 以前金额是「元」的定点字符串，垃圾串会一路走到 Postgres 的 numeric 列上抛
// 22P02，被兜成 500 INTERNAL_ERROR；所以当时在 service 里压了一层
// normalizeMoney/decimalDigits 做字符串校验。
//
// 现在金额是「分」的 int64，"12.5" / "abc" 这种载荷在 controller 的
// json.Unmarshal 阶段就被拒了（400），压根到不了 service。service 这层只剩
// 两条 int64 装不下的规则：非负、不超 maxMoneyCents。解码那一半的用例在
// controller 包里（见 template_decode_test.go）。

func TestCheckMoneyAcceptsInRangeCents(t *testing.T) {
	for _, cents := range []int64{0, 1, 100, 1250, 999999999, maxMoneyCents} {
		if err := checkMoney("faceValue", cents); err != nil {
			t.Errorf("checkMoney(%d) = %v, want nil", cents, err)
		}
	}
}

func TestCheckMoneyRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		cents int64
		why   string
	}{
		{-1, "负数：这几个字段按业务都不该为负，前端也是 min={0} 约束的"},
		{-1250, "负数"},
		{maxMoneyCents + 1, "超过 JSON number 的安全整数范围，前端算出来的分再传回来已经不是同一个数"},
	}
	for _, c := range cases {
		err := checkMoney("faceValue", c.cents)
		if !errors.Is(err, ErrInvalidTemplateAmount) {
			t.Errorf("checkMoney(%d) = %v, want ErrInvalidTemplateAmount（%s）", c.cents, err, c.why)
			continue
		}
		// 报错要指到具体字段，否则客户端还是只知道「invalid coupon template」。
		if !strings.Contains(err.Error(), "faceValue") {
			t.Errorf("checkMoney(%d) 的报错 %q 里没提到字段名", c.cents, err.Error())
		}
		// 不能被当成笼统的 ErrInvalidTemplate 吞掉，否则 controller 那句
		// 分不出字段的 400 消息就白加了。
		if errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("checkMoney(%d) 同时命中了 ErrInvalidTemplate，两个哨兵串了", c.cents)
		}
	}
}

func TestTemplateFromRequestAcceptsZeroAmounts(t *testing.T) {
	// face_value / min_purchase_amount / purchase_price 在 schema 里都是
	// NOT NULL DEFAULT 0，本来就是可选项；面值 0 也合法。不发这三个字段
	// （JSON 里缺省即 0）不该报错。
	tmpl, err := templateFromRequest(dto.TemplateRequest{Name: "t"}, "actor-1")
	if err != nil {
		t.Fatalf("templateFromRequest() error = %v, want nil", err)
	}
	if tmpl.FaceValue != 0 || tmpl.MinPurchaseAmount != 0 || tmpl.PurchasePrice != 0 {
		t.Errorf("金额 = (%d, %d, %d), want 全 0", tmpl.FaceValue, tmpl.MinPurchaseAmount, tmpl.PurchasePrice)
	}
}

func TestTemplateFromRequestCarriesCentsThrough(t *testing.T) {
	// 这版最容易出的错是「页面填 12.50、库里存成 12」这类单位错位，所以把
	// 「请求里的数原样落到 model」钉住：service 不做任何换算，×100 是前端在
	// 提交前做的，÷100 是展示时做的。
	tmpl, err := templateFromRequest(dto.TemplateRequest{
		Name:              "t",
		FaceValue:         1250,
		MinPurchaseAmount: 5000,
		PurchasePrice:     990,
	}, "")
	if err != nil {
		t.Fatalf("templateFromRequest() error = %v, want nil", err)
	}
	if tmpl.FaceValue != 1250 || tmpl.MinPurchaseAmount != 5000 || tmpl.PurchasePrice != 990 {
		t.Errorf("金额 = (%d, %d, %d), want (1250, 5000, 990)",
			tmpl.FaceValue, tmpl.MinPurchaseAmount, tmpl.PurchasePrice)
	}
}

func TestTemplateFromRequestReportsOffendingField(t *testing.T) {
	cases := []struct {
		name  string
		req   dto.TemplateRequest
		field string
	}{
		{"faceValue", dto.TemplateRequest{FaceValue: -1}, "faceValue"},
		{"minPurchaseAmount", dto.TemplateRequest{MinPurchaseAmount: -500}, "minPurchaseAmount"},
		{"purchasePrice", dto.TemplateRequest{PurchasePrice: maxMoneyCents + 1}, "purchasePrice"},
	}
	for _, c := range cases {
		_, err := templateFromRequest(c.req, "")
		if !errors.Is(err, ErrInvalidTemplateAmount) {
			t.Errorf("%s: error = %v, want ErrInvalidTemplateAmount", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: 报错 %q 里没提到字段名 %q", c.name, err.Error(), c.field)
		}
	}
}
