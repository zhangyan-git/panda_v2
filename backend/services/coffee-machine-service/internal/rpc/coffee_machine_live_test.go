package rpc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
	coffeemachinev1 "github.com/panda-dev/panda-v2/contracts/proto/coffee_machine/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// 与 cmd/main.go 用的是同一枚共享服务令牌的形态（32 字节以上）。
const testServiceToken = "0123456789abcdef0123456789abcdef"

// fakeMasterData 只实现这两条用例要打的方法，其余靠嵌入的 nil 接口顶着：真调到了
// 别的办法会 panic，而不是安静地返回零值。
type fakeMasterData struct {
	repository.MasterDataRepository
}

func (f *fakeMasterData) GetDeviceBySerial(_ context.Context, serialUnique string) (*model.Device, error) {
	if serialUnique != "SN-1" {
		return nil, repository.ErrDeviceNotFound
	}
	return &model.Device{ID: "11111111-1111-1111-1111-111111111111", SerialUnique: serialUnique, Status: "active"}, nil
}

func (f *fakeMasterData) GetDeviceDrink(_ context.Context, deviceID, drinkCode string) (*model.Drink, error) {
	if deviceID != "11111111-1111-1111-1111-111111111111" || drinkCode != "1001" {
		return nil, repository.ErrDrinkNotFound
	}
	drinkType := "milk_coffee"
	return &model.Drink{
		ID: "22222222-2222-2222-2222-222222222222", ManufacturerID: "33333333-3333-3333-3333-333333333333",
		OriginID: "", ProductNum: "1001", ProductName: "集成测试饮品",
		DrinkType: &drinkType, ProductImg: "https://example.test/drink.png",
		Price: 1500, VipPrice: 1200, PickupCodePrice: 1400, Status: "off_shelf",
	}, nil
}

// fakeBalance 是扣余额那条路上的仓储替身：只认几个固定的 request_id，其余一律报设备
// 不存在——用例要的是「状态码翻对了没有」，不是再验一遍 SQL（那一层由
// repository/device_balance_integration_test.go 盯）。
//
// 金额当成两档判据：1500 是够，99999 是不够。故意不看设备本身，免得用例为了造出
// 「余额刚好差一分」这种状态去拼一个假仓储。
type fakeBalance struct {
	repository.DeviceBalanceRepository
}

func (f *fakeBalance) DeductDeviceBalance(_ context.Context, in repository.DeductDeviceBalanceParams) (*repository.DeductDeviceBalanceResult, error) {
	if in.DeviceID != "11111111-1111-1111-1111-111111111111" {
		return nil, repository.ErrDeviceNotFound
	}
	// 验证码那两档替身：真比对在仓储里（那一层由 repository 的集成用例盯着），这里只
	// 需要把两个哨兵错误造出来，确认 rpc 层把它们翻成了同一个状态码。
	if in.PickupPassword == "no-password-on-device" {
		return nil, repository.ErrPickupPasswordNotSet
	}
	if in.PickupPassword != "1357" {
		return nil, repository.ErrPickupPasswordMismatch
	}
	if in.Amount > 5000 {
		return nil, repository.ErrInsufficientBalance
	}
	// 重投：这个 request_id 已经扣过了，本次一个字段都没写。
	if in.RequestID == "req-replayed" {
		return &repository.DeductDeviceBalanceResult{BalanceAfter: 700, Applied: false}, nil
	}
	return &repository.DeductDeviceBalanceResult{BalanceAfter: 3500, Applied: true}, nil
}

// liveClient 起一个**真的 gRPC server**（bufconn）并把生成的客户端交给用例。
//
// 装配与 cmd/main.go 一致：同一个实现、同一个鉴权拦截器（auth.GRPCServerOption 内部装的
// 就是它）、同一枚共享服务令牌。唯一的差别是传输层——这里是裸 grpc.NewServer，没有
// kratos 的 server 包装。上一轮只有「编译过即注册过」的证据，这一条把生成的方法名、
// 注册表、拦截器与实现一起真打一遍：名字对不上或没注册都会回 Unimplemented。
func liveClient(t *testing.T) coffeemachinev1.CoffeeMachineServiceClient {
	t.Helper()
	jwtService, err := auth.NewService([]byte(strings.Repeat("test", 8)), "test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(jwtService, testServiceToken)))
	coffeemachinev1.RegisterCoffeeMachineServiceServer(server, NewCoffeeMachineService(
		service.NewMasterDataService(&fakeMasterData{}),
		service.NewDeviceBalanceService(&fakeBalance{})))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return coffeemachinev1.NewCoffeeMachineServiceClient(conn)
}

func serviceCtx() context.Context {
	return auth.WithServiceToken(context.Background(), testServiceToken)
}

// TestGetDeviceBySerialOverRealGRPC 补上一轮缺的那一次活调用。
func TestGetDeviceBySerialOverRealGRPC(t *testing.T) {
	client := liveClient(t)

	device, err := client.GetDeviceBySerial(serviceCtx(), &coffeemachinev1.GetDeviceBySerialRequest{SerialUnique: "SN-1"})
	if err != nil {
		t.Fatalf("get device by serial: %v", err)
	}
	if device.GetId() != "11111111-1111-1111-1111-111111111111" || device.GetSerialUnique() != "SN-1" {
		t.Fatalf("device = %v, want the row the serial points at", device)
	}

	if _, err := client.GetDeviceBySerial(serviceCtx(), &coffeemachinev1.GetDeviceBySerialRequest{SerialUnique: "SN-unknown"}); status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound for an unregistered serial", status.Code(err))
	}
	if _, err := client.GetDeviceBySerial(serviceCtx(), &coffeemachinev1.GetDeviceBySerialRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument for an empty serial", status.Code(err))
	}
}

// TestGetDeviceDrinkOverRealGRPC 是设备回调建单（方案 §四）找饮品那一步的端到端：
// 生成的方法名 → 注册表 → 鉴权拦截器 → 实现 → proto 应答，整条真打一遍。
func TestGetDeviceDrinkOverRealGRPC(t *testing.T) {
	client := liveClient(t)

	drink, err := client.GetDeviceDrink(serviceCtx(), &coffeemachinev1.GetDeviceDrinkRequest{
		DeviceId: "11111111-1111-1111-1111-111111111111", DrinkCode: "1001"})
	if err != nil {
		t.Fatalf("get device drink: %v", err)
	}
	if drink.GetId() != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("id = %q, want the matched row", drink.GetId())
	}
	if drink.GetProductName() != "集成测试饮品" || drink.GetPrice() != 1500 || drink.GetVipPrice() != 1200 ||
		drink.GetPickupCodePrice() != 1400 {
		t.Fatalf("drink = %v, want the price fields carried through unchanged", drink)
	}
	if drink.GetDrinkType() != "milk_coffee" {
		t.Fatalf("drink_type = %q, want the nullable column carried through", drink.GetDrinkType())
	}
	// 下架也认：钱已经在机器上收过了，这一条不能因为 status 变成 NotFound。
	if drink.GetStatus() != "off_shelf" {
		t.Fatalf("status = %q, want off_shelf to be returned rather than filtered out", drink.GetStatus())
	}

	cases := []struct {
		name     string
		req      *coffeemachinev1.GetDeviceDrinkRequest
		wantCode codes.Code
	}{
		{"编号不在库里", &coffeemachinev1.GetDeviceDrinkRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", DrinkCode: "no-such-code"}, codes.NotFound},
		{"没给设备", &coffeemachinev1.GetDeviceDrinkRequest{DrinkCode: "1001"}, codes.InvalidArgument},
		{"没给编号", &coffeemachinev1.GetDeviceDrinkRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111"}, codes.InvalidArgument},
		{"编号只有空格", &coffeemachinev1.GetDeviceDrinkRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", DrinkCode: "   "}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetDeviceDrink(serviceCtx(), tc.req)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v (err = %v)", status.Code(err), tc.wantCode, err)
			}
		})
	}

	// 不带服务令牌：证明这条路上真的装着生产那枚拦截器，不是裸 server。
	if _, err := client.GetDeviceDrink(context.Background(), &coffeemachinev1.GetDeviceDrinkRequest{
		DeviceId: "11111111-1111-1111-1111-111111111111", DrinkCode: "1001"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated without the service token", status.Code(err))
	}
}

// TestDeductDeviceBalanceOverRealGRPC 是取货码那条路收款动作的活调用：生成的方法名 →
// 注册表 → 鉴权拦截器 → 实现 → proto 应答。
//
// 它盯的是**状态码分档**，因为调用方（order-service）对这几档的反应完全不同：
// NotFound 是这台机器没接入（换设备也没用）、InvalidArgument 是报文缺东西、而
// FailedPrecondition 是钱不够——这三件都不能混成 Internal，混了调用方就只能一律重试。
func TestDeductDeviceBalanceOverRealGRPC(t *testing.T) {
	client := liveClient(t)

	res, err := client.DeductDeviceBalance(serviceCtx(), &coffeemachinev1.DeductDeviceBalanceRequest{
		DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500, RequestId: "req-1", PickupPassword: "1357"})
	if err != nil {
		t.Fatalf("deduct device balance: %v", err)
	}
	if !res.GetApplied() || res.GetBalanceAfter() != 3500 {
		t.Fatalf("res = %v, want applied=true and the balance after", res)
	}

	// 重投：applied=false 而不是错误——调用方要拿它继续把订单建出来，否则一次已经
	// 收到钱的取货会永远建不出单。
	replayed, err := client.DeductDeviceBalance(serviceCtx(), &coffeemachinev1.DeductDeviceBalanceRequest{
		DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500, RequestId: "req-replayed",
		PickupPassword: "1357"})
	if err != nil {
		t.Fatalf("replayed deduct: %v", err)
	}
	if replayed.GetApplied() {
		t.Fatal("applied = true, want false on a replay")
	}
	if replayed.GetBalanceAfter() != 700 {
		t.Fatalf("balance_after = %d, want the balance recorded by the first call", replayed.GetBalanceAfter())
	}

	cases := []struct {
		name     string
		req      *coffeemachinev1.DeductDeviceBalanceRequest
		wantCode codes.Code
	}{
		{"余额不够", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 99999, RequestId: "req-1", PickupPassword: "1357"}, codes.FailedPrecondition},
		{"设备没接入", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "99999999-9999-9999-9999-999999999999", Amount: 1500, RequestId: "req-1", PickupPassword: "1357"}, codes.NotFound},
		{"验证码不对", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500, RequestId: "req-1", PickupPassword: "0000"}, codes.PermissionDenied},
		{"设备没配验证码", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500, RequestId: "req-1",
			PickupPassword: "no-password-on-device"}, codes.PermissionDenied},
		{"没给设备", &coffeemachinev1.DeductDeviceBalanceRequest{Amount: 1500, RequestId: "req-1", PickupPassword: "1357"}, codes.InvalidArgument},
		{"金额为零", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", RequestId: "req-1", PickupPassword: "1357"}, codes.InvalidArgument},
		{"金额为负", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", Amount: -1500, RequestId: "req-1", PickupPassword: "1357"}, codes.InvalidArgument},
		{"没给幂等键", &coffeemachinev1.DeductDeviceBalanceRequest{
			DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.DeductDeviceBalance(serviceCtx(), tc.req)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v (err = %v)", status.Code(err), tc.wantCode, err)
			}
		})
	}

	// 这是本服务唯一一个写口，比读口更不能裸奔：不带令牌必须回 Unauthenticated，
	// 而不是先扣了钱再说。
	if _, err := client.DeductDeviceBalance(context.Background(), &coffeemachinev1.DeductDeviceBalanceRequest{
		DeviceId: "11111111-1111-1111-1111-111111111111", Amount: 1500, RequestId: "req-1", PickupPassword: "1357"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated without the service token", status.Code(err))
	}
}
