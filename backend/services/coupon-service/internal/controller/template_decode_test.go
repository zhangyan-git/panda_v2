package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
)

// 金额改成「分」的 int64 之后，原来那条「垃圾金额别报成 500」的保证并没有消失，
// 只是挪到了解码这一步：类型不对的载荷在 json.Unmarshal 就失败，controller 返
// 400 INVALID_ARGUMENT，请求根本到不了 service，更到不了 Postgres。
//
// 这个包以前没有测试文件，这一组是特意补的——只测 service 那一层会漏掉整条链路
// 里最容易回流的部分：把字段改回 string / any，或者换成 json.Number，service 的
// 测试全绿，而接口重新开始收 "12.5" 这种「元」的定点字符串。
//
// coupons 是 nil 也不影响：这些用例全都在 decodeBody 那一行就返回了，永远走不到
// service（走得到的那个用例会 nil panic，所以「合法载荷」的成功路径不在这里测——
// 那属于集成测试，见 repository/postgres_integration_test.go）。
func TestTemplateCreateRejectsNonIntegerMoney(t *testing.T) {
	cases := []struct {
		name string
		body string
		why  string
	}{
		{"元字符串", `{"name":"t","faceValue":"12.50"}`, "旧接口的载荷形态，必须显式被拒"},
		{"元小数", `{"name":"t","faceValue":12.5}`, "小数说明客户端还在按元发"},
		{"垃圾串", `{"name":"t","faceValue":"abc"}`, "以前会进 numeric 列抛 22P02 变 500"},
		{"空串", `{"name":"t","faceValue":""}`, "以前 normalizeMoney 把空串当 0，现在应当显式拒绝"},
		{"字符串零", `{"name":"t","purchasePrice":"0"}`, "字符串形式的整数也不收，单位约定只认一种表达"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/coupons/templates", strings.NewReader(c.body))
			(&AdminCouponController{}).Templates(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（%s）；body = %s", rec.Code, c.why, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "INVALID_ARGUMENT") {
				t.Errorf("响应里没有 INVALID_ARGUMENT：%s", rec.Body.String())
			}
		})
	}
}

// 反过来也要钉住：整数分是合法载荷，不该被解码拦下来。这里只断言 decodeBody
// 本身通过——它正是 controller 里在碰 service 之前做的第一步。
func TestTemplateRequestBodyAcceptsIntegerMoney(t *testing.T) {
	for _, body := range []string{
		`{"name":"t","faceValue":0}`,
		`{"name":"t","faceValue":1250,"minPurchaseAmount":5000,"purchasePrice":990}`,
	} {
		var req dto.TemplateRequest
		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if err := decodeBody(rec, httpReq, &req); err != nil {
			t.Errorf("decodeBody(%s) = %v, want nil", body, err)
		}
	}
}
