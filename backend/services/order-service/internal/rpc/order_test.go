package rpc

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// 这一层只测一件事：**业务结论翻成哪个 gRPC 状态码**。它值得单独钉住，因为挑错档不会报错、
// 也不会挂——只会让 partner-service 给合作方回一句误导的话：
//
//	NotFound →404「这台机器没登记」；InvalidArgument →400「这份报文要改」。
//
// 所以「饮品编号对不上」必须是 InvalidArgument：回成 NotFound 会让合作方去重新同步一台
// 早就登记好的机器，而这一单永远建不出来。表里每一行都对着 partner-service
// internal/client/order.go 与 internal/controller/openapi.go 的分档写。
func TestCreateDeviceOrderErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "missing third party order no", err: service.ErrThirdPartyOrderNoRequired, want: codes.InvalidArgument},
		{name: "missing device serial", err: service.ErrDeviceSerialRequired, want: codes.InvalidArgument},
		{name: "missing drink code", err: service.ErrDrinkCodeRequired, want: codes.InvalidArgument},
		// 这一条是本文件存在的理由（见上）。
		{name: "drink code matches nothing on this device", err: service.ErrDrinkNotFound, want: codes.InvalidArgument},
		// 这个单号已经被另一类设备单（取货码那条）占了。**不是** Unavailable：重投一百次
		// 还是同一个结果，合作方要做的是换一个单号，或者去查他是不是把号用重复了。
		{name: "the order no belongs to another kind of device order", err: service.ErrThirdPartyOrderNoTaken, want: codes.InvalidArgument},
		// 这条路将来新增一条校验、而这里忘了加 case 时走的兜底档，同样是「报文要改」。
		{name: "a validation this switch has no case for", err: fmt.Errorf("%w: quantity", service.ErrInvalidQuantity), want: codes.InvalidArgument},
		// **只有**设备查不到是 404。
		{name: "device serial is not registered", err: service.ErrDeviceNotFound, want: codes.NotFound},
		{name: "device lookup did not answer", err: fmt.Errorf("%w: connection refused", service.ErrDeviceLookupUnavailable), want: codes.Unavailable},
		{name: "drink lookup did not answer", err: fmt.Errorf("%w: connection refused", service.ErrDrinkLookupUnavailable), want: codes.Unavailable},
		{name: "our own fault", err: errors.New("boom"), want: codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := status.Code(createDeviceOrderError(tc.err))
			if got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
			if msg := status.Convert(createDeviceOrderError(tc.err)).Message(); msg == "" {
				t.Error("status message is empty")
			}
		})
	}
}

// TestCreatePickupOrderErrorMapping 是取货码那条路的分档表。它比刷卡机那条多三档，而这三档
// 恰恰是「钱」那一侧多出来的部分，挑错一个的后果都比那边重：
//
//	PermissionDenied   码不对。合作方要能把它与「余额不够」分开——机器前面是两句不同的话。
//	FailedPrecondition 余额不够。**绝不能落到 Unavailable**：那会让合作方一直重投一次永远
//	                   不可能成功的扣款，而机器前面那个人等的是「余额不足，请充值」。
//	InvalidArgument    报文要改，其中包含「这一杯没配取货码价」——回 503 会让它一直重投。
//
// 表里每一行都对着 partner-service internal/client/order.go 与 controller/openapi.go 的分档写。
func TestCreatePickupOrderErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "missing third party order no", err: service.ErrThirdPartyOrderNoRequired, want: codes.InvalidArgument},
		{name: "missing device serial", err: service.ErrDeviceSerialRequired, want: codes.InvalidArgument},
		{name: "missing drink code", err: service.ErrDrinkCodeRequired, want: codes.InvalidArgument},
		{name: "drink code matches nothing on this device", err: service.ErrDrinkNotFound, want: codes.InvalidArgument},
		// 两个价都是 0。它**不是**「没问到」：重投一百次还是没价，合作方要做的是换一杯。
		{name: "the drink cannot be priced for a pickup", err: service.ErrDrinkNotPickupPriced, want: codes.InvalidArgument},
		// 扣减那边回「参数不成立」：报文要重新对一遍。
		{name: "the deduction was rejected", err: service.ErrDeviceBalanceRejected, want: codes.InvalidArgument},
		// 这个单号已经被一张**刷卡机**单占了。这条路在扣款之前就判（判在这里才有意义：
		// 放到建单那一步发现的代价是钱扣了、而这一笔挂在一张与它无关的订单上）。
		// 与刷卡机那条同档：合作方把一个单号用了两次是他那侧的错，重投不会变好。
		{name: "the order no belongs to another kind of device order", err: service.ErrThirdPartyOrderNoTaken, want: codes.InvalidArgument},
		{name: "a validation this switch has no case for", err: fmt.Errorf("%w: quantity", service.ErrInvalidQuantity), want: codes.InvalidArgument},
		// 码不对与余额不够：两条都必须保住自己的档。
		{name: "wrong pickup code", err: service.ErrDeviceBalancePasswordRejected, want: codes.PermissionDenied},
		{name: "the machine is short", err: service.ErrDeviceBalanceNotEnough, want: codes.FailedPrecondition},
		// 两个 NotFound 都是「这台机器没登记」，合作方要去重新同步设备。
		{name: "device serial is not registered", err: service.ErrDeviceNotFound, want: codes.NotFound},
		{name: "the device vanished before the deduction", err: service.ErrDeviceBalanceDeviceMissing, want: codes.NotFound},
		// 三种「没问到」是这条路上唯一可以重投的一档。
		{name: "device lookup did not answer", err: fmt.Errorf("%w: connection refused", service.ErrDeviceLookupUnavailable), want: codes.Unavailable},
		{name: "drink lookup did not answer", err: fmt.Errorf("%w: connection refused", service.ErrDrinkLookupUnavailable), want: codes.Unavailable},
		{name: "the coffee machine service did not answer", err: service.ErrDeviceBalanceServiceUnavailable, want: codes.Unavailable},
		{name: "our own fault", err: errors.New("boom"), want: codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := createPickupOrderError(tc.err)
			if code := status.Code(got); code != tc.want {
				t.Fatalf("code = %v, want %v", code, tc.want)
			}
			if msg := status.Convert(got).Message(); msg == "" {
				t.Error("status message is empty")
			}
		})
	}
}

// TestCreatePickupOrderErrorKeepsTheTwoMoneyConclusionsApart 是这条路上最要紧的一条分界：
// 「码不对」与「余额不够」不能合并，也不能有一个落进兜底档（合作方会把 5xx 当成可以重投）。
func TestCreatePickupOrderErrorKeepsTheTwoMoneyConclusionsApart(t *testing.T) {
	wrongCode := status.Code(createPickupOrderError(service.ErrDeviceBalancePasswordRejected))
	short := status.Code(createPickupOrderError(service.ErrDeviceBalanceNotEnough))
	if wrongCode == short {
		t.Fatalf("both conclusions came back as %v; the machine cannot tell the user which one it is", wrongCode)
	}
	for name, code := range map[string]codes.Code{"wrong code": wrongCode, "short balance": short} {
		if code == codes.Unavailable || code == codes.Internal {
			t.Errorf("%s maps to %v, but neither is retryable and neither is our fault", name, code)
		}
	}
}

// TestCreateRenewalOrderErrorMapping 是续费那条路的分档表。它比前两张**短**，因为调用方
// 不是合作方而是 membership-service：它手里那个渠道流水号是微信给的，改不了，所以前两张表里
// 「报文要改，换一个再投」那一档在这条路上没有对应的处置。
//
// 这张表决定的是这次故障**在日志与死信里长什么样**，而两处有意与设备那两条不同：
//
//	ErrThirdPartyOrderNoTaken → FailedPrecondition（设备那两条是 InvalidArgument）。
//	                           那边换个单号重投就行，这边换不了——撞车意味着有人把微信流水号
//	                           与商户单号串了，是一次要立刻查的串线，不是报文写错了。
//	没有任何一档是 Unavailable：这条路本服务不调任何下游（金额与套餐快照都由调用方给全），
//	                           所以不存在「下游抖了，稍后重投可能就好」。真出了没预料到的错误，
//	                           它落 Internal——对调用方是同一件事（重投），对排查的人是另一件事。
func TestCreateRenewalOrderErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "missing third party order no", err: service.ErrThirdPartyOrderNoRequired, want: codes.InvalidArgument},
		{name: "missing user", err: service.ErrUserRequired, want: codes.InvalidArgument},
		{name: "negative amount", err: service.ErrRenewalAmountInvalid, want: codes.InvalidArgument},
		{name: "missing plan snapshot", err: service.ErrRenewalPlanRequired, want: codes.InvalidArgument},
		// 这条路将来新增一条校验、而这里忘了加 case 时走的兜底档。
		{name: "a validation this switch has no case for", err: fmt.Errorf("%w: quantity", service.ErrInvalidQuantity), want: codes.InvalidArgument},
		// 与设备那两条**有意不同档**，见上。
		{name: "the order no belongs to another kind of order", err: service.ErrThirdPartyOrderNoTaken, want: codes.FailedPrecondition},
		{name: "our own fault", err: errors.New("boom"), want: codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := status.Code(createRenewalOrderError(tc.err))
			if got != tc.want {
				t.Fatalf("code = %v, want %v", got, tc.want)
			}
			if msg := status.Convert(createRenewalOrderError(tc.err)).Message(); msg == "" {
				t.Error("status message is empty")
			}
		})
	}
}

// TestCreateRenewalOrderErrorDoesNotEchoTheCause：与另外两条同一条规矩。
func TestCreateRenewalOrderErrorDoesNotEchoTheCause(t *testing.T) {
	wrapped := fmt.Errorf("%w: dial tcp 127.0.0.1:5432: connection refused", service.ErrRenewalPlanRequired)
	msg := status.Convert(createRenewalOrderError(wrapped)).Message()
	if strings.Contains(msg, "dial tcp") || strings.Contains(msg, "5432") {
		t.Errorf("status message = %q; it echoes the cause", msg)
	}
}

// TestParseOrderID 钉住 GetOrder 那一格：**空串与形状不对是同一档（InvalidArgument）**，
// 而不是 NotFound。
//
// 差别不在措辞：把空串记成「订单不存在」会让一次数据损坏（订阅行上的 first_payment_order_id
// 是空的）看起来像一次正常的缺失，而调用方会据此把那一块当成「这一单本来就没有」安静地跳过。
func TestParseOrderID(t *testing.T) {
	cases := []struct {
		name    string
		give    string
		want    codes.Code
		wantOut string
	}{
		{name: "a real uuid", give: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e", want: codes.OK, wantOut: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"},
		{name: "blank padding is trimmed", give: "  0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e  ", want: codes.OK, wantOut: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"},
		{name: "empty", give: "", want: codes.InvalidArgument},
		{name: "blank", give: "   ", want: codes.InvalidArgument},
		{name: "not a uuid at all", give: "3CYM20260922100000123456", want: codes.InvalidArgument},
		{name: "a uuid with a stray character", give: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6z", want: codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrderID(tc.give)
			if code := status.Code(err); code != tc.want {
				t.Fatalf("code = %v, want %v", code, tc.want)
			}
			if tc.want != codes.OK {
				if err == nil || !strings.Contains(status.Convert(err).Message(), "orderId") {
					t.Errorf("err = %v; the message should name the field the caller got wrong", err)
				}
				return
			}
			if got != tc.wantOut {
				t.Errorf("orderId = %q, want %q", got, tc.wantOut)
			}
		})
	}
}

// TestCreatePickupOrderErrorDoesNotEchoTheCause：与刷卡机那条同一条规矩——包装在里面的原始
// 错误文本不跨服务边界跑，排查要用的信息在服务端日志里。
func TestCreatePickupOrderErrorDoesNotEchoTheCause(t *testing.T) {
	wrapped := fmt.Errorf("%w: dial tcp 127.0.0.1:19094: connection refused", service.ErrDeviceBalanceServiceUnavailable)
	msg := status.Convert(createPickupOrderError(wrapped)).Message()
	if strings.Contains(msg, "dial tcp") || strings.Contains(msg, "19094") {
		t.Errorf("status message = %q; it echoes the cause", msg)
	}
}

// TestCreateDeviceOrderErrorDoesNotEchoTheCause：状态消息是固定句子，包装在里面的原始错误
// 文本（表名、SQL 片段、下游的报错）不跨服务边界跑——与 membership-service 那几条同一条规矩。
// 排查要用的信息在服务端日志里。
func TestCreateDeviceOrderErrorDoesNotEchoTheCause(t *testing.T) {
	wrapped := fmt.Errorf("%w: dial tcp 127.0.0.1:19094: connection refused", service.ErrDeviceLookupUnavailable)
	msg := status.Convert(createDeviceOrderError(wrapped)).Message()
	if strings.Contains(msg, "dial tcp") || strings.Contains(msg, "19094") {
		t.Errorf("status message = %q; it echoes the cause", msg)
	}
}
