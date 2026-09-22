package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
)

// 这个文件盯的是取货码那条路上 HTTP 边界的同一件事，外加一档刷卡机那条没有的：
//
// # 一、扣款的两条结论必须活着走到合作方那里
//
// 「取货码不对」（403）与「余额不够」（409）都不能落进 503 那一支。后果不是状态码难看：机器
// 前面站着一个人，合作方要靠这两句决定**跟他说什么**（重敲一次，还是去充值）。回 503 等于
// 说「等一会儿再来」，而这两件事等多久都不会变——那个人会一直等一句永远不会来的「可以了」。
//
// # 二、报文里没有金额，也不该有
//
// 定价权在订单域（见 dto.PickupRequest）。这一条靠「多一个 amount 就是 400」钉住：解码不许
// 未知字段，所以那个字段一旦被谁加回来，这里立刻红——而不是等到某天有人发现扣的钱跟单子上的
// 钱对不上。

// newPickupController 与 newDeviceOrderController 是同一次装配（两个方法在同一个 service 上），
// 这里的名字只为了让下面每一行的读者知道测的是哪一条路。
func newPickupController(recorder service.DeviceOrderRecorder) *OpenAPIController {
	return newDeviceOrderController(recorder)
}

// pickupBody 是一份最小的、合理的取货码报文。
const pickupBody = `{
	"thirdPartyOrderNo":"TP20260917000002",
	"deviceSerial":"SN-0001",
	"drinkCode":"A01",
	"pickupPassword":"1234",
	"remark":"取货"
}`

// TestWriteOpenAPIErrorKeepsThePickupConclusionsApart 是这一层最要紧的一张表：扣款那两条结论
// 各自的档位，以及它们与「没问到」之间那条不能越过的线。
func TestWriteOpenAPIErrorKeepsThePickupConclusionsApart(t *testing.T) {
	cases := []struct {
		name                 string
		err                  error
		wantStatus           int
		wantCode             string
		wantMessage          string
		sentinelIsTheMessage bool
	}{
		{
			// 报文要改（含「这一杯没配取货码价」）。**不能回 503**：一杯没定价的饮品重投
			// 一百次还是没价，而合作方要做的是换一杯。
			name: "订单域说这份报文不成立", err: client.ErrPickupInvalid,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST", wantMessage: msgOpenAPIPickupInvalid,
		},
		{
			// 复用刷卡机那条的句子：对合作方来说这是同一件事（他名下有一台机器我们这儿没有），
			// 两句不同的话只会让他以为两条接口对设备的要求不一样。
			name: "设备序列号在我们的库里查不到", err: client.ErrPickupDeviceNotFound,
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND", wantMessage: msgOpenAPIDeviceNotFound,
			sentinelIsTheMessage: true,
		},
		{
			// 403 而不是 400：报文一个字都没写错，错的是**报文里那个人提供的一个值**。
			name: "取货码不对", err: client.ErrPickupCodeRejected,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN", wantMessage: msgOpenAPIPickupCodeRejected,
		},
		{
			// 409 而不是 503：这是**确定的结论**，不是故障。咖啡机域在同一个事务里判的，
			// 拒了就一个字段都没写。
			name: "设备余额不够", err: client.ErrPickupNotEnough,
			wantStatus: http.StatusConflict, wantCode: "CONFLICT", wantMessage: msgOpenAPIPickupNotEnough,
		},
		{
			// 这条路上唯一可以重投的一档（幂等键在对方单号上，重投不会扣两次）。
			name: "没问到（订单域或咖啡机域）", err: client.ErrPickupUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantCode: "SERVICE_UNAVAILABLE", wantMessage: msgOpenAPIPickupUnavailable,
			sentinelIsTheMessage: true,
		},
		{
			name: "上下文里没有合作方身份", err: service.ErrCallerMissing,
			wantStatus: http.StatusInternalServerError, wantCode: "INTERNAL_ERROR", wantMessage: msgOpenAPIInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOpenAPIError(rec, signedRequest(http.MethodPost, openAPIDevicePickupPath, ""), tc.err, "test")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			success, code, message := decodeEnvelope(t, rec)
			if success {
				t.Fatal("success = true，want false")
			}
			if code != tc.wantCode || message != tc.wantMessage {
				t.Fatalf("(code, message) = (%q, %q), want (%q, %q)", code, message, tc.wantCode, tc.wantMessage)
			}
			if !tc.sentinelIsTheMessage && strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("响应体里出现了内部错误串: %s", rec.Body.String())
			}
		})
	}
}

// TestWriteOpenAPIErrorNeverCallsAConclusivePickupRetryable 把上一条表里最要命的那处分界单独
// 钉一次，不靠人眼看状态码：**4xx 与「等一会儿再来」必须互斥**。
//
// 这条断言值得单独存在，因为改错它的方式很隐蔽：把某一档从 switch 里删掉，它就会静默落进
// 兜底（500，同样不可重投、同样错，但看起来「至少没回 503」）——而上一条表正好能抓住它。
func TestWriteOpenAPIErrorNeverCallsAConclusivePickupRetryable(t *testing.T) {
	conclusive := map[string]error{
		"报文要改":  client.ErrPickupInvalid,
		"机器没登记": client.ErrPickupDeviceNotFound,
		"取货码不对": client.ErrPickupCodeRejected,
		"余额不够":  client.ErrPickupNotEnough,
	}
	for name, err := range conclusive {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOpenAPIError(rec, signedRequest(http.MethodPost, openAPIDevicePickupPath, ""), err, "test")
			if rec.Code == http.StatusServiceUnavailable {
				t.Fatalf("%v 回成了 503——合作方会一直重投一次不可能成功的取货", err)
			}
			if rec.Code >= 500 {
				t.Fatalf("%v 回成了 %d——有结论的拒绝不能落进兜底那一支", err, rec.Code)
			}
		})
	}
}

// TestCreatePickupOrderReportsAnIdempotentHitAsSuccess 钉住「重投拿回既有那张单」不是错误，
// 并且报文原样到达下游——包括取货码的前后空格（本服务一个字符都不动）。
func TestCreatePickupOrderReportsAnIdempotentHitAsSuccess(t *testing.T) {
	recorder := &fakeDeviceOrderRecorder{pickupOrder: &client.PickupOrder{
		OrderID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo: "SO202609170000000002",
		Created: false,
	}}
	controller := newPickupController(recorder)

	rec := httptest.NewRecorder()
	controller.CreatePickupOrder(rec, signedRequest(http.MethodPost, openAPIDevicePickupPath, `{
		"thirdPartyOrderNo":"TP20260917000002",
		"deviceSerial":"SN-0001",
		"drinkCode":"A01",
		"pickupPassword":" 1234 ",
		"remark":"取货"
	}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（幂等命中不是错误）: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool               `json:"success"`
		Data    dto.PickupResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if !body.Success {
		t.Fatalf("success = false: %s", rec.Body.String())
	}
	if body.Data.Created {
		t.Fatal("created = true，want false——重投的那一次必须如实回 false，否则合作方分不出新建与命中")
	}
	if body.Data.OrderID != recorder.pickupOrder.OrderID || body.Data.OrderNo != recorder.pickupOrder.OrderNo {
		t.Fatalf("data = %+v, want 订单域那张单", body.Data)
	}
	if recorder.pickupReceived.ThirdPartyOrderNo != "TP20260917000002" ||
		recorder.pickupReceived.DeviceSerial != "SN-0001" ||
		recorder.pickupReceived.DrinkCode != "A01" ||
		recorder.pickupReceived.Remark != "取货" {
		t.Fatalf("到达下游的报文 = %+v, want 报文里的字段原样", recorder.pickupReceived)
	}
	if recorder.pickupReceived.PickupPassword != " 1234 " {
		t.Fatalf("pickupPassword = %q, want %q（空格也要原样过去）", recorder.pickupReceived.PickupPassword, " 1234 ")
	}
}

// TestCreatePickupOrderRejectsBadRequestsBeforeCallingDownstream 是几条**不碰下游**的拒绝。
//
// 最后两条是这一条路上独有的：报文里塞一个 amount（定价权不在报文里），或者自报一个
// partnerId（身份来自凭据）。两条都必须是明确的 400，而不是「传了但没人看」的谜题。
func TestCreatePickupOrderRejectsBadRequestsBeforeCallingDownstream(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "动词不对", method: http.MethodGet, path: openAPIDevicePickupPath, body: "", wantStatus: http.StatusMethodNotAllowed, wantCode: "METHOD_NOT_ALLOWED"},
		{name: "报文是空的", method: http.MethodPost, path: openAPIDevicePickupPath, body: "", wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{name: "报文不是 JSON", method: http.MethodPost, path: openAPIDevicePickupPath, body: "{not json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{
			name: "字段名不在契约里", method: http.MethodPost, path: openAPIDevicePickupPath,
			body:       `{"thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","pickupPasswrd":"1234"}`,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
		{
			// 定价权在订单域。这个字段一旦被加回来，扣的钱就由报文说了算。
			name: "报文里带了金额", method: http.MethodPost, path: openAPIDevicePickupPath,
			body:       `{"thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","pickupPassword":"1234","amount":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
		{
			name: "报文里自报合作方", method: http.MethodPost, path: openAPIDevicePickupPath,
			body:       `{"partnerId":"p-2","thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","pickupPassword":"1234"}`,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
		{
			// 路径拼错（多一段）走 404，不是 405：**这条路径上确实有 POST 可用**，但打过来的
			// 那个地址不是它。这一条同时钉住 handler 里的 restOf 判断还在。
			name: "路径上多了一段", method: http.MethodPost, path: openAPIDevicePickupPath + "/extra",
			body:       pickupBody,
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &fakeDeviceOrderRecorder{pickupOrder: &client.PickupOrder{OrderID: "o-1"}}
			controller := newPickupController(recorder)

			rec := httptest.NewRecorder()
			controller.CreatePickupOrder(rec, signedRequest(tc.method, tc.path, tc.body))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				if _, code, _ := decodeEnvelope(t, rec); code != tc.wantCode {
					t.Fatalf("errorCode = %q, want %q", code, tc.wantCode)
				}
			}
			if recorder.calls != 0 {
				t.Fatalf("下游被调用了 %d 次，want 0——这些请求一条都不该走到订单域", recorder.calls)
			}
			if tc.wantStatus == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
			}
		})
	}
}

// TestCreatePickupOrderRefusesWithoutACaller 钉住失败关闭，而这条路上它的代价最重：上下文里
// 没有 Caller 时**不扣钱、不建单**——少了这一句，一条没验过签的报文能实打实地扣掉一台设备的
// 余额，而机器前面根本没有人。
func TestCreatePickupOrderRefusesWithoutACaller(t *testing.T) {
	recorder := &fakeDeviceOrderRecorder{pickupOrder: &client.PickupOrder{OrderID: "o-1"}}
	controller := newPickupController(recorder)

	rec := httptest.NewRecorder()
	controller.CreatePickupOrder(rec, httptest.NewRequest(http.MethodPost, openAPIDevicePickupPath,
		strings.NewReader(pickupBody)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（装配错误）: %s", rec.Code, rec.Body.String())
	}
	if recorder.calls != 0 {
		t.Fatalf("下游被调用了 %d 次，want 0——没验过签的请求不能扣钱", recorder.calls)
	}
}

// TestCreatePickupOrderKeepsTheTwoPickupFailuresDistinct 是一次端到端的读法：同一份**完全正确
// 的报文**，下游换两种结论，合作方拿到的必须是两个不同的状态码。
//
// 上面那几张表逐档钉的是映射；这一条钉的是「报文一样时，差别只来自下游」——如果哪天有人在
// handler 里提前判了什么（比如自己去比一次取货码），这一条会红。
func TestCreatePickupOrderKeepsTheTwoPickupFailuresDistinct(t *testing.T) {
	statuses := map[string]int{}
	for name, downstream := range map[string]error{
		"取货码不对": client.ErrPickupCodeRejected,
		"余额不够":  client.ErrPickupNotEnough,
	} {
		recorder := &fakeDeviceOrderRecorder{pickupErr: downstream}
		controller := newPickupController(recorder)

		rec := httptest.NewRecorder()
		controller.CreatePickupOrder(rec, signedRequest(http.MethodPost, openAPIDevicePickupPath, pickupBody))
		statuses[name] = rec.Code
	}
	if statuses["取货码不对"] == statuses["余额不够"] {
		t.Fatalf("两种结论回成了同一个状态码 %d；合作方分不出该让顾客重敲还是去充值", statuses["取货码不对"])
	}
}
