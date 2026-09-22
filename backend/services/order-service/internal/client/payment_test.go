package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakePaymentConn 只实现 Invoke：生成的客户端除了流式方法都走这一条。内嵌接口是为了让
// NewStream 之类的方法留在「没实现」的状态，而不是在这里编一个假的流出来。
type fakePaymentConn struct {
	grpc.ClientConnInterface
	reply *paymentv1.CreatePaymentResponse
	err   error
	// token 记下这次调用带出去的服务令牌，用于验证 Create 确实认了证。
	token []string
}

func (c *fakePaymentConn) Invoke(ctx context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		c.token = md.Get(auth.MetadataServiceToken)
	}
	if c.err != nil {
		return c.err
	}
	if c.reply == nil {
		return nil
	}
	// 不能按值拷：protobuf 消息里带 MessageState，赋值会被 vet 拦下（也真的会拷锁）。
	out, ok := reply.(proto.Message)
	if !ok {
		return status.Errorf(codes.Internal, "fakePaymentConn: unexpected reply type %T", reply)
	}
	proto.Merge(out, c.reply)
	return nil
}

func newPaymentCreator(t *testing.T, conn grpc.ClientConnInterface) *PaymentCreator {
	t.Helper()
	p, err := NewPaymentCreator(conn, "0123456789012345678901234567890123456789", time.Second)
	if err != nil {
		t.Fatalf("NewPaymentCreator = %v", err)
	}
	return p
}

// createInput 是一次最普通的发起：金额由订单给，其余只是过路。
func createInput() CreatePaymentInput {
	return CreatePaymentInput{
		OrderID:       "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo:       "SO202609150000000001",
		UserID:        "11111111-2222-3333-4444-555555555555",
		Amount:        3400,
		PaymentMethod: "22222222-3333-4444-5555-666666666666",
		RequestID:     "req-20260915-0001",
	}
}

// TestCreateAlwaysHandsTheClientAnObjectForPayParams 钉住 payParams 出去时是 {} 而不是 null。
//
// 这一条是 e2e 抓出来的：gRPC 的 map 字段分不出「空」与「没有」——支付侧那边两种都收敛成
// 空 map，但到了这一侧 GetPayParams() 一律回 nil，序列化出去就是 JSON 的 null，而客户端
// 把 null 读成 undefined，不是空对象。账户出资（扣豆成功、没有第三方要调起）正好是那个
// 「没有渠道参数」的常态，所以坏掉的是最常走的一条路，而且没有任何一层会报错——收银台
// 只是空的。dto.PayAction 对客户端承诺过这里不是 null（见 dto/pay.go 的字段注释）。
func TestCreateAlwaysHandsTheClientAnObjectForPayParams(t *testing.T) {
	cases := []struct {
		name string
		// params 是支付侧应答里那个 map 的三种形态：字段压根没有（nil）、写了但是空的、
		// 以及有渠道参数的。前两种在 gRPC 这一侧是同一个值，正是这条测试要盖住的模糊。
		params map[string]string
	}{
		{name: "应答里没有这个字段", params: nil},
		{name: "应答里是空 map", params: map[string]string{}},
		{name: "有渠道参数", params: map[string]string{"manualPayUrl": "https://pay.example.test/x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakePaymentConn{reply: &paymentv1.CreatePaymentResponse{
				PaymentNo: "PAY20260915120000000001",
				Status:    "pending",
				Action:    "jump_miniapp",
				PayParams: tc.params,
			}}
			action, err := newPaymentCreator(t, conn).Create(context.Background(), createInput())
			if err != nil {
				t.Fatalf("Create = %v", err)
			}
			if action.PayParams == nil {
				t.Fatal("payParams 必须是空对象而不是 nil：客户端把 null 读成 undefined")
			}
			if len(action.PayParams) != len(tc.params) {
				t.Fatalf("payParams = %+v, want %+v", action.PayParams, tc.params)
			}
			// 原样透传：补空对象是一件事，顺手改动有值的参数是另一件事。
			for k, want := range tc.params {
				if got := action.PayParams[k]; got != want {
					t.Errorf("payParams[%q] = %q, want %q", k, got, want)
				}
			}
			// 上面断言的是 Go 里的值；真正对客户端承诺的是序列化之后那个形状，
			// 所以再按 JSON 看一眼——nil map 到这一步就会变回 null。
			body, err := json.Marshal(action)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(body), `"payParams":null`) {
				t.Fatalf("payParams 序列化成了 null: %s", body)
			}
		})
	}
}

// TestCreateSendsTheServiceToken 守住这一层认了证：不带令牌的调用支付侧一律拒，但那是
// 上了真连接才会发现的事——桩不回话的时候，令牌没传不会有任何动静。
func TestCreateSendsTheServiceToken(t *testing.T) {
	conn := &fakePaymentConn{reply: &paymentv1.CreatePaymentResponse{PaymentNo: "PAY1"}}
	if _, err := newPaymentCreator(t, conn).Create(context.Background(), createInput()); err != nil {
		t.Fatalf("Create = %v", err)
	}
	if len(conn.token) == 0 {
		t.Fatal("调用必须带上服务令牌")
	}
}

// TestCreateTreatsAnEmptyResponseAsUnavailable 守住「应答里没有支付单号」那条 guard。
//
// 这不是一次正常回答。当成「没问到」而不是把一个空支付单交给客户端：客户端拿它去调起
// 支付只会得到一句渠道的报错，而我们就此丢掉了一个能查的支付单号。
func TestCreateTreatsAnEmptyResponseAsUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		reply *paymentv1.CreatePaymentResponse
	}{
		// 支付侧回了一个空壳：字段都在，就是没给单号。
		{name: "应答里没有支付单号", reply: &paymentv1.CreatePaymentResponse{Status: "pending"}},
		// 桩什么都不回填，生成的客户端就会交出一个零值应答——真到了线上，这是连接那头
		// 给了一个我们看不懂的东西。
		{name: "应答整个是空的", reply: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := newPaymentCreator(t, &fakePaymentConn{reply: tc.reply}).
				Create(context.Background(), createInput())
			if !errors.Is(err, ErrPaymentServiceUnavailable) {
				t.Fatalf("Create = %v, want %v", err, ErrPaymentServiceUnavailable)
			}
			if action != nil {
				t.Fatalf("action = %+v, want nil——空支付单不能交给客户端", action)
			}
		})
	}
}

// TestMapPaymentErrorBuckets 钉住「支付侧的每一种答复，到了这边算哪一档」。
//
// 这一层是「有结论」与「没结论」的分水岭，判断错了的后果不对称：把没结论的读成有结论，
// 用户会以为钱没出去，于是换一种方式再付一次——而渠道那边那张预支付单可能仍然有效。
// 所以每个状态码各自落到哪一档，必须逐条写死，不能靠读代码去推。
func TestMapPaymentErrorBuckets(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
		want error
		// carriesMessage：支付侧那句固定句子该不该跟着回到调用方手里。
		carriesMessage bool
	}{
		{name: "支付侧不接受这次请求", code: codes.InvalidArgument, want: ErrPaymentRejected, carriesMessage: true},
		{name: "这个支付方式不存在", code: codes.NotFound, want: ErrPaymentRejected, carriesMessage: true},
		{name: "余额不足也走拒绝", code: codes.FailedPrecondition, want: ErrPaymentRejected, carriesMessage: true},
		// 「对面没实现这个 RPC」不是业务结论，是版本错配：让用户换一种支付方式没有任何用，
		// 该做的是把部署对齐。所以它归「没结论」那一档，也不带回支付侧的消息。
		{name: "这一档支付侧压根没实现", code: codes.Unimplemented, want: ErrPaymentServiceUnavailable},
		{name: "同一个幂等号的上一笔还在跑", code: codes.Aborted, want: ErrPaymentConflict, carriesMessage: true},
		{name: "那把钥匙被换过请求体", code: codes.AlreadyExists, want: ErrPaymentConflict, carriesMessage: true},
		{name: "渠道超时，没有结论", code: codes.Unavailable, want: ErrPaymentUncertain},
		{name: "我们自己那条 deadline 先到", code: codes.DeadlineExceeded, want: ErrPaymentUncertain},
		{name: "支付服务内部出错", code: codes.Internal, want: ErrPaymentServiceUnavailable},
		{name: "看不懂的状态码", code: codes.DataLoss, want: ErrPaymentServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPaymentError(status.Error(tc.code, paymentSentence))
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapPaymentError(%s) = %v, want %v", tc.code, got, tc.want)
			}
			if has := strings.Contains(got.Error(), paymentSentence); has != tc.carriesMessage {
				t.Fatalf("带回支付侧的消息 = %v, want %v：%v", has, tc.carriesMessage, got)
			}
		})
	}

	// 连不上时落到这一层的不是 gRPC status，而是传输层的普通错误：status.Code 会把它读成
	// Unknown，落到默认档——也就是「没问到」，不是「被拒了」。这两句话对调用方的意思正相反。
	if got := mapPaymentError(errors.New("connection reset by peer")); !errors.Is(got, ErrPaymentServiceUnavailable) {
		t.Fatalf("传输层错误必须读成没问到，got %v", got)
	}
}

// paymentSentence 冒充支付侧那些「分得清该怎么办」的固定句子。用一句可搜的原文，
// 好断言它到底有没有跟着错误回到调用方手里。
const paymentSentence = "payment side sentence"
