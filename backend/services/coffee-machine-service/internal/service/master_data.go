// Package service 是设备域主数据的业务层。
package service

import (
	"context"
	"strings"

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

// GetDeviceBySerial 按机器序列号返回一台设备；序列号为空时返回
// ErrDeviceSerialRequired，不存在时返回 repository.ErrDeviceNotFound。
//
// 空串在这里挡掉，不下到 SQL：devices.serial_unique 是 text 列，空串查下去只会得到一次
// 空结果，调用方会把「没给序列号」读成「这台机器不存在」——一句填漏的参数被报成一台
// 不存在的设备，是最难查的那种错。trim 与写路径一致（buildDevice 落库前也先 trim），
// 否则「 AB 」会查不到库里那台「AB」。
func (s *MasterDataService) GetDeviceBySerial(ctx context.Context, serialUnique string) (*model.Device, error) {
	serialUnique = strings.TrimSpace(serialUnique)
	if serialUnique == "" {
		return nil, ErrDeviceSerialRequired
	}
	return s.master.GetDeviceBySerial(ctx, serialUnique)
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

// GetDeviceDrink 按机器报的饮品编号返回那台设备上的那一杯；deviceID 或 drinkCode 为空时
// 分别返回 ErrDrinkLookupDeviceRequired / ErrDrinkLookupCodeRequired，库中没有时返回
// repository.ErrDrinkNotFound。
//
// 编号同时比 product_num 与 origin_id（仓储那一层做）：它落在哪一列取决于这家厂商当初
// 的同步来源，调用方不该知道这件事，也不该为了这件事去翻一份菜单再自己找。
//
// **不按 status 过滤**：饮品已下架也得匹配上。钱已经在机器上收过了，这时回 NotFound
// 等于这笔钱在库里没有任何对应的饮品记录——那比「卖了一杯已下架的饮品」严重得多。
//
// 两个参数都先 trim 再判空、再查，与写路径（buildDevice / buildDrink）口径一致。
func (s *MasterDataService) GetDeviceDrink(ctx context.Context, deviceID, drinkCode string) (*model.Drink, error) {
	deviceID = strings.TrimSpace(deviceID)
	drinkCode = strings.TrimSpace(drinkCode)
	if deviceID == "" {
		return nil, ErrDrinkLookupDeviceRequired
	}
	if drinkCode == "" {
		return nil, ErrDrinkLookupCodeRequired
	}
	return s.master.GetDeviceDrink(ctx, deviceID, drinkCode)
}

// GetDrink 按我们的 uuid 返回一杯饮品；id 为空时返回 ErrDrinkIDRequired，库中没有时返回
// repository.ErrDrinkNotFound。
//
// 空串在进 SQL 之前挡掉，理由与上面那条同源但后果更贵：这一条的调用方是 order-service
// 下单，把「没给 id」读成「这杯不在目录里」，用户看到的是一句「该饮品已下架」——一次填漏
// 的参数被报成一次业务拒绝，排查时两边都觉得自己没错。
//
// **不按 status 过滤**：调用方要判「这杯还卖不卖」，而那个判断的依据就是 status 本身。
// 见仓储实现上的说明。
func (s *MasterDataService) GetDrink(ctx context.Context, id string) (*model.Drink, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrDrinkIDRequired
	}
	return s.master.GetDrink(ctx, id)
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
