package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
)

// 这个文件只盯这一层真正负责的两件事，其余一概不测：
//
//  1. **失败关闭**：上下文里没有合作方身份时，一个字段都不许往下走。这条路径正常情况下走不到
//     （handler 被 Guard 包着），但它值得单独钉住，因为这条路上漏一次的代价与查询那条不同——
//     查询漏了是「谁都能看别人的会员权益」，这一条漏了是**谁都能扣一台设备的钱**；
//  2. **零加工**：报文里的字段原样转给订单域。这一层抄一份「什么才算一份合法的取货报文」，
//     就等于给同一个判断留了两个迟早不一致的版本（真正的校验在订单域与咖啡机域）。
//
// 分档（订单域/咖啡机域的答复算哪一档）是 client 与 controller 那两层的事，这里不重复测。

// fakePickupRecorder 只实现这个文件用得上的那一个方法。Create 也实现——接口要求的——但它
// 一被调用就该被发现：取货这条路上没有任何理由走到刷卡那一条去。
type fakePickupRecorder struct {
	received client.PickupInput
	order    *client.PickupOrder
	err      error
	calls    int
}

func (f *fakePickupRecorder) Create(context.Context, client.DeviceOrderInput) (*client.DeviceOrder, error) {
	f.calls++
	return nil, errors.New("取货码这条路上不该调用 Create")
}

func (f *fakePickupRecorder) CreatePickup(_ context.Context, in client.PickupInput) (*client.PickupOrder, error) {
	f.calls++
	f.received = in
	if f.err != nil {
		return nil, f.err
	}
	return f.order, nil
}

// pickupRequest 是一份普通的取货码报文，取货码**带着前后空格**——它是要被原样转下去的证据。
func pickupRequest() dto.PickupRequest {
	return dto.PickupRequest{
		ThirdPartyOrderNo: "TP20260917000002",
		DeviceSerial:      "SN-0001",
		DrinkCode:         "A01",
		PickupPassword:    " 1234 ",
		Remark:            "取货",
	}
}

// TestCreatePickupOrderRefusesWithoutACaller 钉住失败关闭：没有身份就**一次下游调用都不发**。
//
// 这条断言的重点不是状态码（controller 那层翻成 500），而是 calls == 0：一旦这条防线被放开，
// 一条没有验过签的请求会直接扣掉一台设备的钱并建出一张已支付的订单，而机器前面没有人。
func TestCreatePickupOrderRefusesWithoutACaller(t *testing.T) {
	recorder := &fakePickupRecorder{order: &client.PickupOrder{OrderID: "o-1"}}
	service := NewOpenAPIService(nil, recorder)

	_, err := service.CreatePickupOrder(context.Background(), pickupRequest())
	if !errors.Is(err, ErrCallerMissing) {
		t.Fatalf("CreatePickupOrder = %v, want %v", err, ErrCallerMissing)
	}
	if recorder.calls != 0 {
		t.Fatalf("下游被调用了 %d 次，want 0——没验过签的请求不能扣钱", recorder.calls)
	}
}

// TestCreatePickupOrderPassesTheRequestThroughUntouched 钉住零加工，逐字段对照。
//
// 取货码那一行是本条路上最容易「顺手修一下」的地方（TrimSpace / 判空看起来无害），而它恰恰
// 不能动：空码与错码在咖啡机域是同一个结论，我们在这儿先判一次就会出现两处判断，而它们迟早
// 不一致——不一致的那天，机器前面的人敲的码在我们这儿是对的、在那边是错的。
func TestCreatePickupOrderPassesTheRequestThroughUntouched(t *testing.T) {
	recorder := &fakePickupRecorder{order: &client.PickupOrder{
		OrderID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo: "SO202609170000000002",
		Created: true,
	}}
	service := NewOpenAPIService(nil, recorder)

	ctx := ingress.WithCaller(context.Background(), ingress.Caller{
		PartnerID: "p-1", PartnerCode: "fengxuan", APIKeyID: "k-1", APIKeyMask: "ak****1",
	})
	response, err := service.CreatePickupOrder(ctx, pickupRequest())
	if err != nil {
		t.Fatalf("CreatePickupOrder = %v", err)
	}
	if response.OrderID != recorder.order.OrderID || response.OrderNo != recorder.order.OrderNo || !response.Created {
		t.Fatalf("response = %+v, want %+v", response, recorder.order)
	}
	want := pickupRequest()
	if recorder.received.ThirdPartyOrderNo != want.ThirdPartyOrderNo ||
		recorder.received.DeviceSerial != want.DeviceSerial ||
		recorder.received.DrinkCode != want.DrinkCode ||
		recorder.received.Remark != want.Remark {
		t.Fatalf("到达下游的报文 = %+v, want %+v", recorder.received, want)
	}
	if recorder.received.PickupPassword != " 1234 " {
		t.Fatalf("pickup_password = %q, want %q（这一层不 trim、不判空）",
			recorder.received.PickupPassword, " 1234 ")
	}
}

// TestCreatePickupOrderDoesNotTranslateTheDownstreamConclusion 钉住这一层**不翻译**下游结论。
//
// 翻成「给合作方看的话」是 HTTP 边界的事（见 controller/openapi.go 那张表），而边界上还要知道
// 这是哪一条接口、要不要进日志。在这里翻一次、在那边再翻一次，就会出现两套句子——而它们会在
// 某一天不一致，那句话不一致的接口正好是机器前面那个人等答案的那一条。
func TestCreatePickupOrderDoesNotTranslateTheDownstreamConclusion(t *testing.T) {
	for name, downstream := range map[string]error{
		"取货码不对": client.ErrPickupCodeRejected,
		"余额不够":  client.ErrPickupNotEnough,
		"没问到":   client.ErrPickupUnavailable,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := &fakePickupRecorder{err: downstream}
			service := NewOpenAPIService(nil, recorder)

			ctx := ingress.WithCaller(context.Background(), ingress.Caller{PartnerID: "p-1"})
			response, err := service.CreatePickupOrder(ctx, pickupRequest())
			if !errors.Is(err, downstream) {
				t.Fatalf("CreatePickupOrder = %v, want 原样上抛 %v", err, downstream)
			}
			if response != (dto.PickupResponse{}) {
				t.Fatalf("response = %+v, want 零值——失败时不能带回半份事实", response)
			}
		})
	}
}
