package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
)

// 这个文件盯的是设备刷卡回执那条路上 HTTP 边界的两件事，都是「错了不会有人报错」的那种。
//
// # 一、有结论的 4xx 不许落到兜底那一支
//
// writeOpenAPIError 的兜底是 500。订单域明说了「这份报文不成立」（400）或「设备号不认识」
// （404），把它们包成 500 的后果不是状态码难看：合作方会一直重投一条**永远不可能成功**的
// 请求，而钱早就收过了。这一条靠一张逐项对照表钉住。
//
// # 二、幂等命中不是错误
//
// 重投拿回既有那张单时，HTTP 必须是 200 且 created=false（不是 4xx、也不是 200 但把 created
// 吞掉）。合作方要靠它区分「新建了」与「重投了一次」，而这两种情况在他那边要做的处理完全不同。

// fakeDeviceOrderRecorder 是 service.DeviceOrderRecorder 的桩：把收到的入参记下来，回事先
// 放好的结果。它按**真的会到达订单域的那份值**构造（六个字段原样），这样断言看的就是报文，
// 不是本层加工过的东西。
type fakeDeviceOrderRecorder struct {
	received client.DeviceOrderInput
	order    *client.DeviceOrder
	err      error
	calls    int

	// 取货码那一条（见 openapi_pickup_test.go）。与刷卡机那条共用 calls：两条路**都是**
	// 「往订单域记一笔」，而断言「这条请求一条都没走到订单域」时不该关心是哪一条。
	pickupReceived client.PickupInput
	pickupOrder    *client.PickupOrder
	pickupErr      error
	pickupCalls    int
}

func (f *fakeDeviceOrderRecorder) Create(_ context.Context, in client.DeviceOrderInput) (*client.DeviceOrder, error) {
	f.calls++
	f.received = in
	if f.err != nil {
		return nil, f.err
	}
	return f.order, nil
}

func (f *fakeDeviceOrderRecorder) CreatePickup(_ context.Context, in client.PickupInput) (*client.PickupOrder, error) {
	f.calls++
	f.pickupCalls++
	f.pickupReceived = in
	if f.pickupErr != nil {
		return nil, f.pickupErr
	}
	return f.pickupOrder, nil
}

// newDeviceOrderController 装配一个不经过路由的控制器。会员那条依赖传 nil：这些测试一条都
// 走不到它，而传一个桩只会让人以为它被测到了。
func newDeviceOrderController(recorder service.DeviceOrderRecorder) *OpenAPIController {
	return NewOpenAPIController(service.NewOpenAPIService(nil, recorder))
}

// signedRequest 造一条「已经验过签」的请求：handler 只应该在上下文里已经有 Caller 时才被调用
// （那是 ingress.Guard 干的事，见 routes/openapi.go）。
func signedRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	return r.WithContext(ingress.WithCaller(r.Context(), ingress.Caller{
		PartnerID: "p-1", PartnerCode: "fengxuan", APIKeyID: "k-1", APIKeyMask: "ak****1",
	}))
}

// decodeEnvelope 把一封响应拆成 (success, errorCode, errorMessage)。
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (bool, string, string) {
	t.Helper()
	var body struct {
		Success      bool   `json:"success"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return body.Success, body.ErrorCode, body.ErrorMessage
}

// TestWriteOpenAPIErrorKeepsConclusiveRejectionsOutOfTheFallback 是那张逐项对照表。
//
// 每一行都对着一个具体的错法：把 400 写成 500（合作方永远重投）、把 503 写成 400（合作方去改
// 一份没错的报文）、或者把下游的错误串透出去（枚举的入口，见这个文件的包注释）。
func TestWriteOpenAPIErrorKeepsConclusiveRejectionsOutOfTheFallback(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
		// sentinelIsTheMessage：这个哨兵的文案本身**就是**给合作方的那句话（与
		// client.ErrMembershipUnavailable 同一种写法），所以「响应体里出现了它」不算泄漏。
		// 其余几档的文案是内部说法，一个字都不许出现在响应体里。
		sentinelIsTheMessage bool
	}{
		{
			name: "订单域说这份报文不成立", err: client.ErrDeviceOrderInvalid,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST", wantMessage: msgOpenAPIDeviceOrderInvalid,
		},
		{
			name: "设备序列号在我们的库里查不到", err: client.ErrDeviceOrderNotFound,
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND", wantMessage: msgOpenAPIDeviceNotFound,
			sentinelIsTheMessage: true,
		},
		{
			// 可重投（幂等键在对方单号上），所以是 503 而不是 500——500 对客户端意味着「别再试了」。
			name: "订单域没答上来", err: client.ErrDeviceOrderUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantCode: "SERVICE_UNAVAILABLE", wantMessage: msgOpenAPIOrderUnavailable,
			sentinelIsTheMessage: true,
		},
		{
			// 装配错误（handler 被挂在 Guard 之外）。对外的句子与一次普通内部错误完全一样：
			// 告诉调用方「你这条请求没走验签」等于告诉他怎么绕过它。
			name: "上下文里没有合作方身份", err: service.ErrCallerMissing,
			wantStatus: http.StatusInternalServerError, wantCode: "INTERNAL_ERROR", wantMessage: msgOpenAPIInternal,
		},
		{
			name: "谁都没见过的一个错误", err: errors.New("something broke deep inside"),
			wantStatus: http.StatusInternalServerError, wantCode: "INTERNAL_ERROR", wantMessage: msgOpenAPIInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOpenAPIError(rec, signedRequest(http.MethodPost, openAPIDeviceOrderPath, ""), tc.err, "test")
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
			// 内部错误串一个字节都不许出现在响应体里（那是一次枚举的机会，见文件头）。
			if !tc.sentinelIsTheMessage && strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("响应体里出现了内部错误串: %s", rec.Body.String())
			}
		})
	}
}

// TestCreateDeviceOrderReportsAnIdempotentHitAsSuccess 钉住「重投拿回既有那张单」不是错误。
//
// 三件事一起断言：200、created=false 如实回给合作方、以及报文原样到达下游（六个字段）。
func TestCreateDeviceOrderReportsAnIdempotentHitAsSuccess(t *testing.T) {
	recorder := &fakeDeviceOrderRecorder{order: &client.DeviceOrder{
		OrderID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo: "SO202609170000000001",
		Created: false,
	}}
	controller := newDeviceOrderController(recorder)

	rec := httptest.NewRecorder()
	controller.CreateDeviceOrder(rec, signedRequest(http.MethodPost, openAPIDeviceOrderPath, `{
		"thirdPartyOrderNo":"TP20260917000001",
		"deviceSerial":"SN-0001",
		"drinkCode":"A01",
		"amount":1800,
		"brewFailed":false,
		"remark":"刷卡"
	}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（幂等命中不是错误）: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool                    `json:"success"`
		Data    dto.DeviceOrderResponse `json:"data"`
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
	if body.Data.OrderID != recorder.order.OrderID || body.Data.OrderNo != recorder.order.OrderNo {
		t.Fatalf("data = %+v, want 订单域那张单", body.Data)
	}
	if recorder.received.ThirdPartyOrderNo != "TP20260917000001" || recorder.received.DeviceSerial != "SN-0001" ||
		recorder.received.DrinkCode != "A01" || recorder.received.Amount != 1800 ||
		recorder.received.BrewFailed || recorder.received.Remark != "刷卡" {
		t.Fatalf("到达下游的报文 = %+v, want 报文里的六个字段原样", recorder.received)
	}
}

// TestCreateDeviceOrderRejectsBadRequestsBeforeCallingDownstream 是三条**不碰下游**的拒绝：
// 动词不对、报文读不动、字段名不在契约里（含「报文里塞一个 partnerId 试试」）。
//
// 尤其是最后一条：身份来自凭据，不来自字段。它必须是一个明确的 400，而不是一个「传了但没人
// 看」的谜题——后者会让对接方以为那个字段生效了。
func TestCreateDeviceOrderRejectsBadRequestsBeforeCallingDownstream(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "动词不对", method: http.MethodGet, body: "", wantStatus: http.StatusMethodNotAllowed, wantCode: "METHOD_NOT_ALLOWED"},
		{name: "报文是空的", method: http.MethodPost, body: "", wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{name: "报文不是 JSON", method: http.MethodPost, body: "{not json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST"},
		{
			name: "字段名不在契约里", method: http.MethodPost,
			body:       `{"thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","amount":1,"brewFaild":true}`,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
		{
			name: "报文里自报合作方", method: http.MethodPost,
			body:       `{"partnerId":"p-2","thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","amount":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_REQUEST",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &fakeDeviceOrderRecorder{order: &client.DeviceOrder{OrderID: "o-1"}}
			controller := newDeviceOrderController(recorder)

			rec := httptest.NewRecorder()
			controller.CreateDeviceOrder(rec, signedRequest(tc.method, openAPIDeviceOrderPath, tc.body))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if _, code, _ := decodeEnvelope(t, rec); code != tc.wantCode {
				t.Fatalf("errorCode = %q, want %q", code, tc.wantCode)
			}
			if recorder.calls != 0 {
				t.Fatalf("下游被调用了 %d 次，want 0——这些请求一条都不该走到订单域", recorder.calls)
			}
			// 405 要带上 Allow：对接方是照着响应头改代码的。
			if tc.wantStatus == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
			}
		})
	}
}

// TestCreateDeviceOrderRefusesWithoutACaller 钉住失败关闭：上下文里没有 Caller 时**不建单**。
//
// 这条路径正常情况下走不到（handler 被 Guard 包着，而 Guard 只在九道校验全过之后才放进
// Caller）。留着它是因为「handler 被单独注册到别处」这种改动不会报错——放开的话，一条没有
// 验过签的请求会直接建出一张已支付的订单，而钱并没有在机器上收过。
func TestCreateDeviceOrderRefusesWithoutACaller(t *testing.T) {
	recorder := &fakeDeviceOrderRecorder{order: &client.DeviceOrder{OrderID: "o-1"}}
	controller := newDeviceOrderController(recorder)

	rec := httptest.NewRecorder()
	controller.CreateDeviceOrder(rec, httptest.NewRequest(http.MethodPost, openAPIDeviceOrderPath,
		strings.NewReader(`{"thirdPartyOrderNo":"TP1","deviceSerial":"SN-1","drinkCode":"A01","amount":1}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（装配错误）: %s", rec.Code, rec.Body.String())
	}
	if recorder.calls != 0 {
		t.Fatalf("下游被调用了 %d 次，want 0——没验过签的请求不能建单", recorder.calls)
	}
}
