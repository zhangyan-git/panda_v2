// Package service 是设备域主数据的业务层。
package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// 列表接口的分页边界，取值与 platform/api.MaxPageSize 一致。
//
// 这里曾经是 100，理由是「没有一个要全集的下拉」。这个前提后来不成立了：设备详情页
// 的饮品 tab 要按设备名显示与筛选，饮品管理页要把 device_id 翻成设备名，两处都先用
// 前端「要全集」的统一参数（admin-web/src/services/pagination.ts 的 FULL_PAGE_PARAMS，
// pageSize=200）拉一份设备全集 —— 在 100 的接口上直接 400，而调用点把失败吞掉退回显示
// 原始 id，于是页面上安静地铺一串 uuid，正是 2026-09 优惠券侧踩过的那一跤。
//
// 它对外可见，是因为控制器要把它传给 api.ParsePage 当作拒收上限——上限值只在这一处定义。
const (
	defaultPageSize = 20
	MaxPageSize     = 200
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
		StoreIDs:       q.StoreIDs,
		Status:         q.Status,
		Keyword:        q.Keyword,
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
		DeviceID:       q.DeviceID,
		ManufacturerID: q.ManufacturerID,
		Status:         q.Status,
		Page:           q.Page,
		PageSize:       q.PageSize,
	})
}

func (s *MasterDataService) ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.Drink, error) {
	return s.master.ListDeviceDrinks(ctx, deviceID)
}

// ListDeviceBalanceEntries 返回一台设备的余额流水一页。设备不存在时返回
// repository.ErrDeviceNotFound。
func (s *MasterDataService) ListDeviceBalanceEntries(
	ctx context.Context, deviceID string, page, pageSize int,
) ([]*model.DeviceBalanceEntry, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	return s.master.ListDeviceBalanceEntries(ctx, deviceID, page, pageSize)
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
