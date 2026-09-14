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

// CoffeeMachineService 服务内部调用方读设备域主数据。
//
// 只实现 GetDevice。契约里其余的 List* / Upsert / Delete 一律落到嵌入的
// Unimplemented 实现上回 codes.Unimplemented——这与 merchant-service 的做法一致：
// 列表要分页语义、写入要校验与审计，而这两件事现在都由 HTTP 侧（后台界面）承担，
// 内部契约在有真实调用方之前不发明一套。需要时按调用方的需要补，不预先铺开。
type CoffeeMachineService struct {
	coffeemachinev1.UnimplementedCoffeeMachineServiceServer

	master *service.MasterDataService
}

func NewCoffeeMachineService(master *service.MasterDataService) *CoffeeMachineService {
	return &CoffeeMachineService{master: master}
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

func deviceError(err error) error {
	if errors.Is(err, repository.ErrDeviceNotFound) {
		return status.Error(codes.NotFound, "device not found")
	}
	return status.Error(codes.Internal, "device service error")
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
