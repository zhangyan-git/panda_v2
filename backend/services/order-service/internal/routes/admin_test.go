package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// 这个文件只驱动一条路由：补建取货码单那个运维入口（POST /v1/admin/orders/pickup-repairs）。
// 它值得单独测，是因为它的**路径形状与订单详情一样**（/v1/admin/orders/ 后面跟一段），而
// 两条路落在同一个 handler 的不同分支上——注册顺序或者 switch 顺序改一下，它就会被解析成一个
// 叫 "pickup-repairs" 的订单 id，而且**不会报错**：那条路只是回一句「找不到这一单」。
//
// 这里同时钉住它出口上的两个 409：它们不是「请求写错了」，而是两条不同的处置结论（这个单号
// 不能补 / 这笔钱没扣过），回错了运维会照着错的方向去查。

const adminRepairOrderID = "9d1c2b3a-4e5f-4a6b-8c7d-0e1f2a3b4c5d"

const adminRepairDeviceSerial = "SN-ADMIN-REPAIR"

// fakePickupDevices 是设备两个读端加唯一那个写口的最小假实现——只需要走通取货码那条路。
type fakePickupDevices struct {
	service.DeviceReader

	device *client.Device
	drink  *client.Drink

	deductErr   error
	deduct      *client.DeductBalanceResult
	deductCalls int
	gotDeduct   client.DeductBalanceInput
}

func (f *fakePickupDevices) GetBySerial(_ context.Context, serial string) (*client.Device, bool, error) {
	if f.device == nil || f.device.SerialUnique != serial {
		return nil, false, nil
	}
	return f.device, true, nil
}

func (f *fakePickupDevices) GetDeviceDrink(_ context.Context, _, _ string) (*client.Drink, bool, error) {
	if f.drink == nil {
		return nil, false, nil
	}
	return f.drink, true, nil
}

func (f *fakePickupDevices) DeductBalance(_ context.Context, in client.DeductBalanceInput) (*client.DeductBalanceResult, error) {
	f.deductCalls++
	f.gotDeduct = in
	if f.deductErr != nil {
		return nil, f.deductErr
	}
	if f.deduct != nil {
		return f.deduct, nil
	}
	// 默认：这一次真的扣了，金额就是这一单算出来的价（首次扣减的常态）。
	return &client.DeductBalanceResult{BalanceAfter: 3600, Applied: true, Amount: in.Amount}, nil
}

// fakePickupRepo 只实现补单这条路走到的两个方法。
type fakePickupRepo struct {
	service.Repository

	found   *repository.DeviceOrderByThirdPartyNo
	findErr error

	created bool
	got     repository.CreateDeviceOrderParams
	calls   int
}

func (f *fakePickupRepo) FindDeviceOrderByThirdPartyNo(_ context.Context, _ string) (*repository.DeviceOrderByThirdPartyNo, error) {
	return f.found, f.findErr
}

func (f *fakePickupRepo) CreateDeviceOrder(_ context.Context, p repository.CreateDeviceOrderParams) (*repository.CreateDeviceOrderResult, bool, error) {
	f.calls++
	f.got = p
	return &repository.CreateDeviceOrderResult{OrderID: adminRepairOrderID, OrderNo: p.OrderNo, Created: f.created}, f.created, nil
}

// pickupRepairDevices 是一台能走取货码的机器：序列号对得上，在售一杯取货码价 1400 的拿铁。
func pickupRepairDevices() *fakePickupDevices {
	return &fakePickupDevices{
		device: &client.Device{
			ID: "3f0a6a1e-1111-4222-8333-444455556666", StoreID: "9a8b7c6d-1111-4222-8333-444455556666",
			SerialUnique: adminRepairDeviceSerial, Status: "active",
		},
		drink: &client.Drink{
			ID: "5c4b3a29-1111-4222-8333-444455556666", OriginID: "origin-1",
			Name: "拿铁", Price: 1800, PickupCodePrice: 1400,
		},
	}
}

type adminAPI struct {
	server *khttp.Server
	jwt    *auth.Service
}

// newAdminAPI 装的是真路由表（RegisterAdmin 与它那个从长到短的注册顺序），假掉的只有权限
// 中间件（换成只做认证的那一个）与两个下游。权限码那一段由 cmd/main.go 的 adminAuthorizer
// 负责，不在这里重复一遍。
func newAdminAPI(t *testing.T, repo service.Repository, devices service.DeviceReader) *adminAPI {
	t.Helper()
	jwt := testJWT(t)
	authorize := func(_ ...string) func(http.Handler) http.Handler {
		return auth.Middleware(jwt)
	}
	orders := service.New(repo, devices, nil, nil, nil, nil, service.Options{})
	server := khttp.NewServer()
	RegisterAdmin(runtime.NewHTTPRouter(server),
		controller.NewAdminOrderController(orders),
		controller.NewAdminAfterSaleController(orders),
		authorize)
	return &adminAPI{server: server, jwt: jwt}
}

func (a *adminAPI) pickupRepair(t *testing.T, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/v1/admin/orders/pickup-repairs", strings.NewReader(body))
	r.Header.Set("Authorization", testToken(t, a.jwt, auth.Grant{Subject: "a1", UserID: "a1"}))
	w := httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	return w
}

// TestAdminPickupRepairsIsNotSwallowedByTheOrderIDRoute 是注册顺序那一条：这个路径必须走到
// 补单的分支里，而不是被当成一张订单的 id。
//
// 判据是回哪一句话：走到补单分支时缺幂等键是**「thirdPartyOrderNo is required」**；落进
// {id} 兜底则是「找不到这一单」（那条路拿 "pickup-repairs" 去查询，得到 NotFound）——两句
// 话都长得像一次正常的 4xx，只有这里分得开。
func TestAdminPickupRepairsIsNotSwallowedByTheOrderIDRoute(t *testing.T) {
	devices, repo := pickupRepairDevices(), &fakePickupRepo{}
	api := newAdminAPI(t, repo, devices)

	w := api.pickupRepair(t, http.MethodPost, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "thirdPartyOrderNo is required") {
		t.Fatalf("body = %s, want the pickup repair branch's own message", w.Body.String())
	}
	// 它没往下走：既没问设备，也没碰订单库。
	if devices.deductCalls != 0 || repo.calls != 0 {
		t.Error("a rejected repair still reached the device or the order table")
	}
}

// TestAdminPickupRepairsIsPostOnly：这条路径上的读取没有意义（补单是一个动作），非 POST 一律
// 404——与同一棵路径上另外两条一样，而不是给它一个「查不了」的 405。
func TestAdminPickupRepairsIsPostOnly(t *testing.T) {
	api := newAdminAPI(t, &fakePickupRepo{}, pickupRepairDevices())

	if w := api.pickupRepair(t, http.MethodGet, ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

// TestAdminPickupRepairRefusesAnotherKindsOrderNo 是第一个 409：这个单号属于一张**刷卡机**
// 单，补不了一张取货码单出来。
//
// 这一档必须在扣款之前判出来（见 service.CreatePickupOrder），所以这里断言**一分钱都没动**:
// 回 409 的同时设备扣减一次都没被调用。
func TestAdminPickupRepairRefusesAnotherKindsOrderNo(t *testing.T) {
	devices, repo := pickupRepairDevices(), &fakePickupRepo{
		found: &repository.DeviceOrderByThirdPartyNo{
			OrderID: adminRepairOrderID, OrderNo: "DEV202609160001", PaymentMethod: "card_pay",
		},
	}
	api := newAdminAPI(t, repo, devices)

	w := api.pickupRepair(t, http.MethodPost,
		`{"thirdPartyOrderNo":"TP-1","deviceSerial":"`+adminRepairDeviceSerial+`","drinkCode":"P001"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ORDER_NO_TAKEN") {
		t.Fatalf("body = %s, want the ORDER_NO_TAKEN code", w.Body.String())
	}
	if devices.deductCalls != 0 {
		t.Error("a taken order no still reached the device balance")
	}
	if repo.calls != 0 {
		t.Error("a taken order no still wrote an order")
	}
}

// TestAdminPickupRepairSaysWhenTheMoneyWasNeverCharged 是第二个 409：空码被咖啡机域拒了，
// 意思是这个 request_id 在设备余额流水上没有对应的扣减——**这笔钱从来没扣过**。
//
// 它必须与「码填错了」分开：这条接口上根本没有取货码这一格，运维看到「取货码不对」会去核对
// 一个不存在的输入。NOT_CHARGED 说的是「这一单不该补，先去查合作方到底有没有发起过」。
func TestAdminPickupRepairSaysWhenTheMoneyWasNeverCharged(t *testing.T) {
	devices, repo := pickupRepairDevices(), &fakePickupRepo{}
	devices.deductErr = service.ErrDeviceBalancePasswordRejected
	api := newAdminAPI(t, repo, devices)

	w := api.pickupRepair(t, http.MethodPost,
		`{"thirdPartyOrderNo":"TP-1","deviceSerial":"`+adminRepairDeviceSerial+`","drinkCode":"P001"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NOT_CHARGED") {
		t.Fatalf("body = %s, want the NOT_CHARGED code", w.Body.String())
	}
	if repo.calls != 0 {
		t.Error("a repair that was never charged still wrote an order")
	}
}

// TestAdminPickupRepairBuildsTheOrderWithAnEmptyPickupCode 是成功那一条，钉住两个形状：
//
//  1. 带下去的是**空码**。这不是省事，是这条路的安全前提：空码过不了那台设备的校验（没配过码
//     的设备一律拒绝、配过码的设备空串对不上），所以这条接口不可能引起一次新的扣款——它只能
//     把已经扣过的那一单补出来。
//  2. 金额照旧由本服务定价，报文里给不了。
func TestAdminPickupRepairBuildsTheOrderWithAnEmptyPickupCode(t *testing.T) {
	devices, repo := pickupRepairDevices(), &fakePickupRepo{created: true}
	api := newAdminAPI(t, repo, devices)

	w := api.pickupRepair(t, http.MethodPost,
		`{"thirdPartyOrderNo":"TP-1","deviceSerial":"`+adminRepairDeviceSerial+`","drinkCode":"P001"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"created":true`) {
		t.Fatalf("body = %s, want created=true", w.Body.String())
	}
	if devices.gotDeduct.PickupPassword != "" {
		t.Errorf("pickupPassword = %q, want the empty code this path always sends", devices.gotDeduct.PickupPassword)
	}
	if devices.gotDeduct.RequestID != "TP-1" {
		t.Errorf("requestID = %q, want the third party order no (一个键两个身份)", devices.gotDeduct.RequestID)
	}
	if repo.got.PayableAmount != 1400 {
		t.Errorf("payable = %d, want the pickup code price 1400", repo.got.PayableAmount)
	}
	if repo.got.PaymentMethod != "pickup_code" {
		t.Errorf("paymentMethod = %q, want pickup_code", repo.got.PaymentMethod)
	}
}
