package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 三个列表接口的 pageSize 上限必须与 dto.MaxPageSize 一致，且越界是 400 而不是
// 「静默按 20 条返回」——后者会让客户端以为自己拿到了 200 条。
//
// 三个 handler 各自按路径前缀分派，所以用例要指明调哪一个（拿 Batches 去打
// templates 的路径会走到别的分支，测的就成了「路径不认识」）。
//
// coupons 为 nil 不影响：越界在 ParsePage 那一行就返回了，走不到 service
// （合法值会走到，所以这里不测「200 能过」——那属于集成测试）。
func TestListRejectsPageSizeAboveMax(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		handler func(*AdminCouponController, http.ResponseWriter, *http.Request)
	}{
		{"模板列表", "/v1/admin/coupons/templates?page=1&pageSize=201",
			func(c *AdminCouponController, w http.ResponseWriter, r *http.Request) { c.Templates(w, r) }},
		{"发券批次", "/v1/admin/coupons/batches?page=1&pageSize=201",
			func(c *AdminCouponController, w http.ResponseWriter, r *http.Request) { c.Batches(w, r) }},
		{"用户券列表", "/v1/admin/coupons/user-coupons?page=1&pageSize=201",
			func(c *AdminCouponController, w http.ResponseWriter, r *http.Request) { c.UserCoupons(w, r) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			c.handler(&AdminCouponController{}, rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: 期望 400，实际 %d（body=%s）", c.path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "pageSize must be between 1 and 200") {
				t.Fatalf("%s: 错误信息里没写清上限是多少：%s", c.path, rec.Body.String())
			}
		})
	}
}
