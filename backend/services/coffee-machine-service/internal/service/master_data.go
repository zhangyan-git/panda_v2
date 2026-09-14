// Package service 是设备域主数据的业务层。
package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// 列表接口的分页边界。MaxPageSize 比 api.MaxPageSize（200）紧一档，与优惠券侧
// 一致：收紧要改调用方，放宽不用，所以先取紧的。它对外可见，是因为控制器要把它
// 传给 api.ParsePage 当作拒收上限——上限值只在这一处定义。
const (
	defaultPageSize = 20
	MaxPageSize     = 100
)

// MasterDataService 提供设备域主数据的读操作。
type MasterDataService struct {
	master repository.MasterDataRepository
}

func NewMasterDataService(master repository.MasterDataRepository) *MasterDataService {
	return &MasterDataService{master: master}
}

// GetDevice 按 ID 返回一台设备；不存在时返回 repository.ErrDeviceNotFound。
func (s *MasterDataService) GetDevice(ctx context.Context, id string) (*model.Device, error) {
	return s.master.GetDevice(ctx, id)
}

func (s *MasterDataService) ListDevices(ctx context.Context, q dto.DeviceQuery) ([]*model.Device, int64, error) {
	q.Page, q.PageSize = normalizePage(q.Page, q.PageSize)
	return s.master.ListDevices(ctx, repository.DeviceFilter{
		ManufacturerID: q.ManufacturerID,
		StoreID:        q.StoreID,
		Status:         q.Status,
		Page:           q.Page,
		PageSize:       q.PageSize,
	})
}

func (s *MasterDataService) ListManufacturers(ctx context.Context) ([]*model.Manufacturer, error) {
	return s.master.ListManufacturers(ctx)
}

func (s *MasterDataService) ListDrinks(ctx context.Context, q dto.DrinkQuery) ([]*model.Drink, int64, error) {
	q.Page, q.PageSize = normalizePage(q.Page, q.PageSize)
	return s.master.ListDrinks(ctx, repository.DrinkFilter{
		ManufacturerID: q.ManufacturerID,
		Status:         q.Status,
		Page:           q.Page,
		PageSize:       q.PageSize,
	})
}

func (s *MasterDataService) ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.DeviceDrink, error) {
	return s.master.ListDeviceDrinks(ctx, deviceID)
}

// normalizePage 把越界的分页参数收进合法范围，而不是报错：页码越界是前端筛选后
// 的正常结果，回一个 400 只会让列表页在筛选时整页报错。
func normalizePage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 || size > MaxPageSize {
		size = defaultPageSize
	}
	return page, size
}
