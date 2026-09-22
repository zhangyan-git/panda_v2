// Package rpc 实现 coffee-machine-service 的 gRPC 面。包名用 rpc 而不是 grpc，
// 以免遮蔽 google.golang.org/grpc。
package rpc

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
	coffeemachinev1 "github.com/panda-dev/panda-v2/contracts/proto/coffee_machine/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CoffeeMachineService 服务内部调用方读设备域主数据，外加一个扣余额的写口。
//
// 读的一侧只实现 GetDevice、GetDeviceBySerial 与 GetDeviceDrink 三条**按一个键取一行**
// 的接口。契约里其余的 List* / Upsert / Delete 一律落到嵌入的 Unimplemented 实现上回
// codes.Unimplemented——这与 merchant-service 的做法一致：列表要分页语义、写入要校验与
// 审计，而这两件事现在都由 HTTP 侧（后台界面）承担，内部契约在有真实调用方之前不发明
// 一套。需要时按调用方的需要补，不预先铺开。
//
// ListDrinks 保持 Unimplemented 是有意的：设备回调找的是特定那一杯，不是一份菜单，而
// 列表是分页的——机器上饮品一多，匹配会在某一页之后静默失败，那时钱已经收过了。见
// GetDeviceDrink 的说明。
//
// DeductDeviceBalance 是唯一一个写口，它不违反上面那条：它没有做成 HTTP 接口，也没有
// 走「后台那套分页与审计」，因为调用方是服务（order-service），不是人。
type CoffeeMachineService struct {
	coffeemachinev1.UnimplementedCoffeeMachineServiceServer

	master  *service.MasterDataService
	balance *service.DeviceBalanceService
}

func NewCoffeeMachineService(master *service.MasterDataService, balance *service.DeviceBalanceService) *CoffeeMachineService {
	return &CoffeeMachineService{master: master, balance: balance}
}

// GetDevice 是 order-service 下单校验设备状态（方案 5.8）走的那条路。
//
// 它一次给出两个互不替代的事实：status 是「我们让不让这台设备接单」，
// vendorOnline 是「厂商说它此刻通不通」。调用方要按 null 分出「从未同步过」
// 这第三种情况，不要把 null 当成离线。
func (s *CoffeeMachineService) GetDevice(ctx context.Context, req *coffeemachinev1.DeviceID) (*coffeemachinev1.Device, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	device, err := s.master.GetDevice(ctx, req.GetId())
	if err != nil {
		return nil, deviceError(err)
	}
	return deviceMessage(device), nil
}

// GetDeviceBySerial 是设备回调建单（方案 §四）的第一步：线下刷卡机只报自己认识的
// 序列号，我们要把它换成库里那一行——门店就挂在那行上，而订单要落 store_id。
//
// 空序列号回 InvalidArgument 而不是 NotFound：「没给」与「没这台机器」在调用方那里
// 是两个不同的动作，前者是把编号补上，后者是这台机器没接入。
func (s *CoffeeMachineService) GetDeviceBySerial(ctx context.Context, req *coffeemachinev1.GetDeviceBySerialRequest) (*coffeemachinev1.Device, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	device, err := s.master.GetDeviceBySerial(ctx, req.GetSerialUnique())
	if err != nil {
		return nil, deviceError(err)
	}
	return deviceMessage(device), nil
}

// GetDeviceDrink 是设备回调建单（方案 §四）找饮品的那一条：机器只知道「我这杯是几号」，
// 既不知道我们的 uuid，也不知道那个编号落在 product_num 还是 origin_id——两件事都由本
// 服务兜掉。
//
// 它是**查一条，不是列一份菜单**：走 ListDrinks 再自己翻页去找，匹配会在某一页之后
// 静默失败，而那时钱已经收过了。
//
// 取不到回 NotFound；device_id 或 drink_code 为空回 InvalidArgument——「没给」与
// 「这杯不在库里」在调用方那里是两个动作。
func (s *CoffeeMachineService) GetDeviceDrink(ctx context.Context, req *coffeemachinev1.GetDeviceDrinkRequest) (*coffeemachinev1.Drink, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	drink, err := s.master.GetDeviceDrink(ctx, req.GetDeviceId(), req.GetDrinkCode())
	if err != nil {
		return nil, deviceError(err)
	}
	return drinkMessage(drink), nil
}

// GetDrink 是 order-service 下单时给饮品行定价的那一条：客户端只给 drink_id，名称、图片
// 与三级价由这里现查。契约里那条「订单行上那份快照不能由调用方给」的论证写在 proto 上，
// 这里只说实现上容易踩的两处。
//
// **不按 status 过滤，原样回给调用方。** 这与 GetDeviceDrink 有意不拦 status 是同一个判断
// 的两面：同一条饮品在两条路上该不该卖，分歧点不在 status 那一列，而在**钱收没收到**——
// 设备回调那条路钱已经在机器上收过了（拦了就是丢单），下单这条路还没有。那是订单域的事实，
// 判断留在 order-service；本服务只回答「这一行是什么」。所以本条**不会**回
// FailedPrecondition，别照 GetMembershipPlan 给它补一个：下架是一列事实，不是一次拒绝。
//
// 取不到回 NotFound；id 为空回 InvalidArgument。空值尤其要挡住——调用方会把「没给 id」
// 读成「这杯不在目录里」，那是一次填漏的参数被报成一次业务拒绝。
func (s *CoffeeMachineService) GetDrink(ctx context.Context, req *coffeemachinev1.DrinkID) (*coffeemachinev1.Drink, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	drink, err := s.master.GetDrink(ctx, req.GetId())
	if err != nil {
		return nil, deviceError(err)
	}
	return drinkMessage(drink), nil
}

// DeductDeviceBalance 从某台设备的咖啡余额里扣一笔（方案 §四 取货码那条路）。
//
// 调用方是 order-service，且**只该由它调**：这条路的顺序是「先扣钱、后建单」，
// 中间断了是「扣了没建单」（可补），反过来是「建了单没扣钱」（白送一杯）。
//
// 三种失败分档，因为调用方对它们的反应完全不同：
//   - NotFound：这台设备没接入。一单做不成，重试无用。
//   - InvalidArgument：报文里缺了设备号/金额/唯一值。补参数，不是重试。
//   - PermissionDenied：验证码不对，或者这台设备根本没配过码。也是重试无用。
//   - FailedPrecondition：余额不够。这一单真的做不成，且**一个字段都没写**。
//
// applied=false 不是失败：那个 request_id 上次已经扣成功了，本次一个字都没改。
// 调用方必须当成成功继续建单——否则对方的一次重投会让一单已经收到的钱永远建不出单。
func (s *CoffeeMachineService) DeductDeviceBalance(ctx context.Context, req *coffeemachinev1.DeductDeviceBalanceRequest) (*coffeemachinev1.DeductDeviceBalanceResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.balance.DeductDeviceBalance(ctx, req.GetDeviceId(), service.DeductDeviceBalanceInput{
		Amount:         req.GetAmount(),
		RequestID:      req.GetRequestId(),
		Remark:         req.GetRemark(),
		PickupPassword: req.GetPickupPassword(),
	})
	if err != nil {
		return nil, deductError(err)
	}
	return &coffeemachinev1.DeductDeviceBalanceResponse{
		BalanceAfter: result.BalanceAfter,
		Applied:      result.Applied,
		Amount:       result.Amount,
	}, nil
}

// deductError 把扣减这条路上的哨兵错误翻成 gRPC 状态码。
//
// 与 deviceError 分开：那个是读路径的（设备/饮品取不到），这个只认写路径自己的三条，
// 认不出来的一律 Internal。合起来写会让「余额不够」和「设备不存在」共用一条不该共用的
// 分支——两者在调用方那里一个是终态、一个要换参数。
func deductError(err error) error {
	switch {
	case errors.Is(err, repository.ErrDeviceNotFound):
		return status.Error(codes.NotFound, "device not found")
	case errors.Is(err, repository.ErrInsufficientBalance):
		return status.Error(codes.FailedPrecondition, "device balance is not enough")
	// 两种验证码问题回同一档：对调用方它们都是「你没资格动这台机器的钱」。分开只会
	// 告诉一个拿着错码的人「这台机器配的码是空的」——那是这台设备的内部状态。
	case errors.Is(err, repository.ErrPickupPasswordMismatch),
		errors.Is(err, repository.ErrPickupPasswordNotSet):
		return status.Error(codes.PermissionDenied, "pickup password is not accepted")
	case errors.Is(err, service.ErrDeductDeviceRequired):
		return status.Error(codes.InvalidArgument, "device_id is required")
	case errors.Is(err, service.ErrDeductAmountInvalid):
		return status.Error(codes.InvalidArgument, "amount must be positive")
	case errors.Is(err, service.ErrDeductRequestIDRequired):
		return status.Error(codes.InvalidArgument, "request_id is required")
	}
	return status.Error(codes.Internal, "device balance service error")
}

func deviceError(err error) error {
	if errors.Is(err, repository.ErrDeviceNotFound) {
		return status.Error(codes.NotFound, "device not found")
	}
	if errors.Is(err, repository.ErrDrinkNotFound) {
		return status.Error(codes.NotFound, "drink not found")
	}
	// 空参数这类输入问题在 service 层就挡下了，没有落到 SQL。它不该回 Internal：
	// 重试一次还是同样的结果，调用方要做的是把参数补上。
	if errors.Is(err, service.ErrDeviceSerialRequired) {
		return status.Error(codes.InvalidArgument, "serial_unique is required")
	}
	if errors.Is(err, service.ErrDrinkLookupDeviceRequired) {
		return status.Error(codes.InvalidArgument, "device_id is required")
	}
	if errors.Is(err, service.ErrDrinkLookupCodeRequired) {
		return status.Error(codes.InvalidArgument, "drink_code is required")
	}
	if errors.Is(err, service.ErrDrinkIDRequired) {
		return status.Error(codes.InvalidArgument, "drink_id is required")
	}
	return status.Error(codes.Internal, "device service error")
}

// drinkMessage 把库里那一行交给调用方。字段与 proto 的 Drink 一一对应：
// 三级价格原样传出（单位是分），drink_type 是 nullable 列，null 折成空串——proto 里
// 空串就是「未分类」，不是另一种状态。
func drinkMessage(d *model.Drink) *coffeemachinev1.Drink {
	message := &coffeemachinev1.Drink{
		Id:              d.ID,
		ManufacturerId:  d.ManufacturerID,
		OriginId:        d.OriginID,
		ProductNum:      d.ProductNum,
		ProductName:     d.ProductName,
		ProductDesc:     d.ProductDesc,
		ProductImg:      d.ProductImg,
		Price:           d.Price,
		VipPrice:        d.VipPrice,
		PickupCodePrice: d.PickupCodePrice,
		Status:          d.Status,
	}
	if d.DrinkType != nil {
		message.DrinkType = *d.DrinkType
	}
	// device_id 也是 nullable 列（历史上真有没挂设备的饮品行），null 同样折成空串：
	// proto 里空串就是「还没挂到设备上」，调用方据此跳过设备比对。
	if d.DeviceID != nil {
		message.DeviceId = *d.DeviceID
	}
	return message
}

func deviceMessage(d *model.Device) *coffeemachinev1.Device {
	message := &coffeemachinev1.Device{
		Id:             d.ID,
		ManufacturerId: d.ManufacturerID,
		SerialUnique:   d.SerialUnique,
		DeviceName:     d.DeviceName,
		Status:         d.Status,
		// 指针直接传递：proto3 的 optional bool 也是指针，null 才走得下去。
		VendorOnline:     d.VendorOnline,
		LastFaultCode:    d.LastFaultCode,
		LastFaultMessage: d.LastFaultMessage,
	}
	if d.StoreID != nil {
		message.StoreId = *d.StoreID
	}
	// 同样是 optional：没有同步过就没有这个时间，别用 0 冒充 1970 年。
	if d.LastSyncedAt != nil {
		syncedAt := d.LastSyncedAt.Unix()
		message.LastSyncedAtUnix = &syncedAt
	}
	return message
}
