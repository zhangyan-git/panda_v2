package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	orderv1 "github.com/panda-dev/panda-v2/contracts/proto/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 这个文件盯的是两条设备回调（刷卡回执与取货码）上「订单域与咖啡机域的每一种答复，到了这边
// 算哪一档」。
//
// 判断错了的后果不对称，所以每一档都写死在这里：
//
//   - 把「没问到」（故障）读成「你的报文不对」，合作方会去改一份本来没错的报文；
//   - 把「报文不对」（有结论）读成故障，他会一直重投一份永远不会被接受的报文。
//
// 两次都发生在**钱已经收过或者已经扣掉**的那两条路上，所以这两句都不是「回错了状态码」那么轻。

// fakeOrderConn 只实现 Invoke：生成的客户端除了流式方法都走这一条。内嵌接口是为了让
// NewStream 之类留在「没实现」的状态，而不是在这里编一个假的流出来（与 order-service 的
// fakePaymentConn 同一种写法）。
type fakeOrderConn struct {
	grpc.ClientConnInterface
	reply *orderv1.CreateDeviceOrderResponse
	err   error
	// request 记下这次发出去的报文，用于验「字段原样、没多没少」。
	request *orderv1.CreateDeviceOrderRequest
	// token 记下这次调用带出去的服务令牌，用于验 Create 确实认了证。
	token []string

	// 取货码那一条（见下面那组测试）。两条路走的是同一个下游、同一枚令牌，所以共用 err 与
	// token；报文与应答各记各的——它们的形状不同，混在一起就看不出「有没有多传一个金额」。
	pickupReply   *orderv1.CreatePickupOrderResponse
	pickupRequest *orderv1.CreatePickupOrderRequest
}

// Invoke 按**报文类型**分发：生成的客户端两条 RPC 都走这一条。用一个 Conn 而不是两个，是因为
// 生产里它们共用同一个 *DeviceOrderCreator 与同一条连接——分成两个假连接会掩盖「两个方法其实
// 打到同一个服务」这件事，而那正是把它们放在一个接口里的理由。
func (c *fakeOrderConn) Invoke(ctx context.Context, _ string, args, reply any, _ ...grpc.CallOption) error {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		c.token = md.Get(auth.MetadataServiceToken)
	}
	switch in := args.(type) {
	case *orderv1.CreateDeviceOrderRequest:
		c.request = in
	case *orderv1.CreatePickupOrderRequest:
		c.pickupRequest = in
	}
	if c.err != nil {
		return c.err
	}
	// 按字段赋值而不是整份拷：protobuf 消息里带 MessageState，按值拷会被 vet 拦下
	// （也真的会拷锁）。
	switch out := reply.(type) {
	case *orderv1.CreateDeviceOrderResponse:
		if c.reply != nil {
			out.OrderId = c.reply.OrderId
			out.OrderNo = c.reply.OrderNo
			out.Created = c.reply.Created
		}
	case *orderv1.CreatePickupOrderResponse:
		if c.pickupReply != nil {
			out.OrderId = c.pickupReply.OrderId
			out.OrderNo = c.pickupReply.OrderNo
			out.Created = c.pickupReply.Created
		}
	default:
		return status.Errorf(codes.Internal, "fakeOrderConn: unexpected reply type %T", reply)
	}
	return nil
}

func newDeviceOrderCreator(t *testing.T, conn grpc.ClientConnInterface) *DeviceOrderCreator {
	t.Helper()
	creator, err := NewDeviceOrderCreator(conn, "0123456789012345678901234567890123456789", time.Second)
	if err != nil {
		t.Fatalf("NewDeviceOrderCreator = %v", err)
	}
	return creator
}

// deviceOrderInput 是一次最普通的回执：金额由设备报，其余只是过路。
func deviceOrderInput() DeviceOrderInput {
	return DeviceOrderInput{
		ThirdPartyOrderNo: "TP20260917000001",
		DeviceSerial:      "SN-0001",
		DrinkCode:         "A01",
		Amount:            1800,
		BrewFailed:        false,
		Remark:            "刷卡",
	}
}

// TestDeviceOrderCreatorRejectsUnusableWiring 钉住三个构造参数都不能是空值/零值。
//
// 一个没有令牌的客户端会在运行时被订单域拒掉（Unauthenticated），而那看起来像一次权限事故
// 而不是一次装配错误——这条路上的排查成本很高（钱已经收过了）。
func TestDeviceOrderCreatorRejectsUnusableWiring(t *testing.T) {
	const token = "0123456789012345678901234567890123456789"
	cases := []struct {
		name    string
		conn    grpc.ClientConnInterface
		token   string
		timeout time.Duration
	}{
		{name: "没有连接", conn: nil, token: token, timeout: time.Second},
		{name: "没有令牌", conn: &fakeOrderConn{}, token: "  ", timeout: time.Second},
		{name: "没有超时", conn: &fakeOrderConn{}, token: token, timeout: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewDeviceOrderCreator(tc.conn, tc.token, tc.timeout); err == nil {
				t.Fatal("构造成功，want 一个装配错误")
			}
		})
	}
}

// TestCreateSendsTheRequestWithTheServiceToken 钉住两件在报文上看得见的事：
//
//  1. 六个字段原样转过去（一个不多、一个不少）——多一个字段就是把「我们这边的形状」塞进了
//     订单域，少一个就是静默丢事实；
//  2. 身份走的是**服务令牌**（metadata），不是请求字段。合同不允许内部 RPC 收调用方自报的
//     身份，所以这条断言同时也在防「有人后来往 proto 里加了一个 partner 字段」。
func TestCreateSendsTheRequestWithTheServiceToken(t *testing.T) {
	conn := &fakeOrderConn{reply: &orderv1.CreateDeviceOrderResponse{
		OrderId: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo: "SO202609170000000001",
		Created: true,
	}}
	in := deviceOrderInput()

	order, err := newDeviceOrderCreator(t, conn).Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if order.OrderID != conn.reply.OrderId || order.OrderNo != conn.reply.OrderNo || !order.Created {
		t.Fatalf("Create = %+v, want %+v", order, conn.reply)
	}
	if conn.request == nil {
		t.Fatal("没有发出请求")
	}
	if got := conn.request.GetThirdPartyOrderNo(); got != in.ThirdPartyOrderNo {
		t.Fatalf("third_party_order_no = %q, want %q", got, in.ThirdPartyOrderNo)
	}
	if got := conn.request.GetDeviceSerial(); got != in.DeviceSerial {
		t.Fatalf("device_serial = %q, want %q", got, in.DeviceSerial)
	}
	if got := conn.request.GetDrinkCode(); got != in.DrinkCode {
		t.Fatalf("drink_code = %q, want %q", got, in.DrinkCode)
	}
	if got := conn.request.GetAmount(); got != in.Amount {
		t.Fatalf("amount = %d, want %d", got, in.Amount)
	}
	if got := conn.request.GetBrewFailed(); got != in.BrewFailed {
		t.Fatalf("brew_failed = %v, want %v", got, in.BrewFailed)
	}
	if got := conn.request.GetRemark(); got != in.Remark {
		t.Fatalf("remark = %q, want %q", got, in.Remark)
	}
	if len(conn.token) != 1 || conn.token[0] == "" {
		t.Fatalf("服务令牌 = %v, want 恰好一个非空值", conn.token)
	}
}

// TestCreateTreatsAnEmptyResponseAsUnavailable 钉住「建了单却没给单号」不算成功。
//
// 合作方拿不到单号就没法把这张单记到自己的账上，而我们也丢掉了唯一一个能查它的线索——
// 当成成功回一句 200，等于把一次协议层的意外变成一条业务结论。归「没问到」是因为重投安全
// （幂等键在对方单号上）。
func TestCreateTreatsAnEmptyResponseAsUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		reply *orderv1.CreateDeviceOrderResponse
	}{
		{name: "应答整个是空的", reply: nil},
		{name: "有单号以外的字段，就是没有 order_id", reply: &orderv1.CreateDeviceOrderResponse{OrderNo: "SO1", Created: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order, err := newDeviceOrderCreator(t, &fakeOrderConn{reply: tc.reply}).
				Create(context.Background(), deviceOrderInput())
			if !errors.Is(err, ErrDeviceOrderUnavailable) {
				t.Fatalf("Create = %v, want %v", err, ErrDeviceOrderUnavailable)
			}
			if order != nil {
				t.Fatalf("order = %+v, want nil——没有单号的订单不能交给合作方", order)
			}
		})
	}
}

// pickupInput 是一次最普通的取货码回调：没有金额（定价权在我们），多一个取货码。
func pickupInput() PickupInput {
	return PickupInput{
		ThirdPartyOrderNo: "TP20260917000002",
		DeviceSerial:      "SN-0001",
		DrinkCode:         "A01",
		PickupPassword:    " 1234 ",
		Remark:            "取货",
	}
}

// TestCreatePickupSendsTheRequestWithTheServiceToken 与 Create 那条逐项相同，但有三处值得单独
// 钉住，它们都是这条路上独有的：
//
//  1. 报文里**没有金额**——定价权在订单域（它拿取货码价、为 0 回落目录价）。这个字段一旦被
//     加回来（「顺带带上设备报的价」），收的就是设备认的价，而钱是从我们账上扣的；
//  2. 取货码**原样**转过去，一个字符都不动（下面这个值带着前后空格）。它是顾客敲进去的东西，
//     归一它等于在我们这儿先判了一次——而比对在持有那一列的服务里做；
//  3. 身份走服务令牌，不走字段（这条调用代表平台去动一台设备的账，既不代表合作方也不代表
//     某个用户）。
func TestCreatePickupSendsTheRequestWithTheServiceToken(t *testing.T) {
	conn := &fakeOrderConn{pickupReply: &orderv1.CreatePickupOrderResponse{
		OrderId: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		OrderNo: "SO202609170000000002",
		Created: true,
	}}
	in := pickupInput()

	order, err := newDeviceOrderCreator(t, conn).CreatePickup(context.Background(), in)
	if err != nil {
		t.Fatalf("CreatePickup = %v", err)
	}
	if order.OrderID != conn.pickupReply.OrderId || order.OrderNo != conn.pickupReply.OrderNo || !order.Created {
		t.Fatalf("CreatePickup = %+v, want %+v", order, conn.pickupReply)
	}
	if conn.pickupRequest == nil {
		t.Fatal("没有发出请求")
	}
	if got := conn.pickupRequest.GetThirdPartyOrderNo(); got != in.ThirdPartyOrderNo {
		t.Fatalf("third_party_order_no = %q, want %q", got, in.ThirdPartyOrderNo)
	}
	if got := conn.pickupRequest.GetDeviceSerial(); got != in.DeviceSerial {
		t.Fatalf("device_serial = %q, want %q", got, in.DeviceSerial)
	}
	if got := conn.pickupRequest.GetDrinkCode(); got != in.DrinkCode {
		t.Fatalf("drink_code = %q, want %q", got, in.DrinkCode)
	}
	// 原样，含前后空格（见上面第 2 条）。
	if got := conn.pickupRequest.GetPickupPassword(); got != in.PickupPassword {
		t.Fatalf("pickup_password = %q, want %q（一个字符都不许动）", got, in.PickupPassword)
	}
	if got := conn.pickupRequest.GetRemark(); got != in.Remark {
		t.Fatalf("remark = %q, want %q", got, in.Remark)
	}
	if len(conn.token) != 1 || conn.token[0] == "" {
		t.Fatalf("服务令牌 = %v, want 恰好一个非空值", conn.token)
	}
}

// TestCreatePickupTreatsAnEmptyResponseAsUnavailable：与 Create 那条同一条理由——建了单却没给
// 单号不算成功，合作方拿不到单号就没法把这一笔记到自己的账上。归「没问到」是因为重投安全
// （这一次的幂等键在对方单号上，重投不会扣两次）。
func TestCreatePickupTreatsAnEmptyResponseAsUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		reply *orderv1.CreatePickupOrderResponse
	}{
		{name: "应答整个是空的", reply: nil},
		{name: "有单号以外的字段，就是没有 order_id", reply: &orderv1.CreatePickupOrderResponse{OrderNo: "SO1", Created: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order, err := newDeviceOrderCreator(t, &fakeOrderConn{pickupReply: tc.reply}).
				CreatePickup(context.Background(), pickupInput())
			if !errors.Is(err, ErrPickupUnavailable) {
				t.Fatalf("CreatePickup = %v, want %v", err, ErrPickupUnavailable)
			}
			if order != nil {
				t.Fatalf("order = %+v, want nil——没有单号的一笔取货不能算成功", order)
			}
		})
	}
}

// TestMapPickupErrorBuckets 是取货码那条的分档表。它比刷卡机那条多两档，而**多出来的两档正是
// 这条路上唯一会造成实际损失的部分**：
//
//	PermissionDenied   码不对 → 403。合作方要让顾客重敲一次。
//	FailedPrecondition 余额不够 → 409。合作方要让顾客去充值。
//
// 这两条都**不能**落到 503：回 503 等于说「等一会儿再来」，而它们等多久都不会变——机器前面
// 那个人会一直等一句永远不会来的「可以了」。这一点在 controller 那一侧还有一张同样的表。
func TestMapPickupErrorBuckets(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
		want error
	}{
		{name: "报文不成立（字段缺失、饮品匹配不到、这一杯没定价）", code: codes.InvalidArgument, want: ErrPickupInvalid},
		{name: "扣减那边说参数不成立", code: codes.InvalidArgument, want: ErrPickupInvalid},
		{name: "设备序列号查不到", code: codes.NotFound, want: ErrPickupDeviceNotFound},
		{name: "取货码不对（或这台机器没配过码）", code: codes.PermissionDenied, want: ErrPickupCodeRejected},
		{name: "设备余额不够", code: codes.FailedPrecondition, want: ErrPickupNotEnough},
		{name: "订单域或咖啡机域没答上来", code: codes.Unavailable, want: ErrPickupUnavailable},
		{name: "我们自己那条 deadline 先到", code: codes.DeadlineExceeded, want: ErrPickupUnavailable},
		{name: "下游内部出错", code: codes.Internal, want: ErrPickupUnavailable},
		{name: "这一档下游还没实现", code: codes.Unimplemented, want: ErrPickupUnavailable},
		{name: "看不懂的状态码", code: codes.DataLoss, want: ErrPickupUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPickupError(status.Error(tc.code, "order service said something"))
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapPickupError(%s) = %v, want %v", tc.code, got, tc.want)
			}
			// 下游那句话**一个字节都不回给合作方**：对外的句子在 controller 那张表里。
			if got.Error() == "order service said something" {
				t.Fatalf("把下游的错误串带出来了: %v", got)
			}
		})
	}
}

// TestMapPickupErrorKeepsTheRetryableAPartFromTheConclusive 把上一条表里最要紧的那处分界单独
// 钉一次：**只有**「没问到」那一档是可重投的。另外四档都是确定的结论——重投一百次也是同一个
// 结果，而把它们混进可重投那一档，合作方那边就是一台永远在重试的机器。
func TestMapPickupErrorKeepsTheRetryableAPartFromTheConclusive(t *testing.T) {
	conclusive := map[string]codes.Code{
		"报文要改":  codes.InvalidArgument,
		"机器没登记": codes.NotFound,
		"取货码不对": codes.PermissionDenied,
		"余额不够":  codes.FailedPrecondition,
	}
	retryable := mapPickupError(status.Error(codes.Unavailable, "down"))
	for name, code := range conclusive {
		got := mapPickupError(status.Error(code, "down"))
		if errors.Is(got, retryable) {
			t.Errorf("%s（%s）落进了「等一会儿重投」那一档: %v", name, code, got)
		}
	}
}

// TestMapDeviceOrderErrorBuckets 把订单域的每一种答复钉到具体那一档上，不靠读代码去推。
//
// Unimplemented（我们比对面新，喊了一个它还没有的 RPC）也归「没问到」：让合作方去改一份本来
// 没错的报文，是这几种里最没有用的一种回答。
func TestMapDeviceOrderErrorBuckets(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
		want error
	}{
		{name: "报文不成立（字段缺失、饮品匹配不到）", code: codes.InvalidArgument, want: ErrDeviceOrderInvalid},
		{name: "设备序列号查不到", code: codes.NotFound, want: ErrDeviceOrderNotFound},
		{name: "订单域没答上来", code: codes.Unavailable, want: ErrDeviceOrderUnavailable},
		{name: "我们自己那条 deadline 先到", code: codes.DeadlineExceeded, want: ErrDeviceOrderUnavailable},
		{name: "订单域内部出错", code: codes.Internal, want: ErrDeviceOrderUnavailable},
		{name: "这一档订单域还没实现", code: codes.Unimplemented, want: ErrDeviceOrderUnavailable},
		{name: "看不懂的状态码", code: codes.DataLoss, want: ErrDeviceOrderUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapDeviceOrderError(status.Error(tc.code, "order service said something"))
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapDeviceOrderError(%s) = %v, want %v", tc.code, got, tc.want)
			}
			// 订单域那句话**一个字节都不回给合作方**：对外的句子在 controller 那张表里。
			if got.Error() == "order service said something" {
				t.Fatalf("把订单域的错误串带出来了: %v", got)
			}
		})
	}
}
