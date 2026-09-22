package service

import (
	"context"
	"errors"
	"slices"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// ErrDeviceOutOfScope 表示这台设备不在调用者的数据范围内。
//
// 与「设备不存在」映射成同一个 404：越界与不存在在这里必须给出同一个答案，否则响应
// 本身成了一条存在性预言机——拿着别人的 id 逐个试，403 与 404 的差别会把对方有哪些
// 设备说出来。判定放在 service 而不是 controller，因为「看得见什么」是这条链路的规则，
// 将来多一条商户域路由也不该各自再实现一遍。
var ErrDeviceOutOfScope = errors.New("device is out of the caller's data scope")

// MerchantDeviceQuery 是商户端设备列表的过滤条件。
//
// 它**没有门店字段**，这是刻意的：边界只能来自 scope，查询串里能带门店就等于把范围
// 交回给调用方。给一个能表达「按门店筛」的入参，早晚会有人把它接着往下传。
type MerchantDeviceQuery struct {
	ManufacturerID string
	Status         string
	Keyword        string
	Page           int
	PageSize       int
}

// MerchantDeviceService 是商户域的只读面：设备列表与详情。
//
// 与 MasterDataService 分开，理由与门店那一侧相同——不是权限（商户域没有权限码），
// 而是入参：后台那一侧的过滤条件全部来自查询串，商户这一侧多了一项由服务端解析出来的
// 数据范围，且那一项不可被请求覆盖。混在一个 List 里，早晚会有人把范围当成一个可选的
// 过滤字段传。
type MerchantDeviceService struct {
	master repository.MasterDataRepository
}

func NewMerchantDeviceService(master repository.MasterDataRepository) *MerchantDeviceService {
	return &MerchantDeviceService{master: master}
}

// List 返回数据范围内的设备。total 与列表用同一组条件，所以「先取全量再在内存里过滤」
// 这种写法在这里一出现就会被 total 对不上抓住。
func (s *MerchantDeviceService) List(ctx context.Context, scope auth.StoreScope, q MerchantDeviceQuery) ([]*model.Device, int64, error) {
	page, pageSize := normalizePage(q.Page, q.PageSize)
	return s.master.ListDevices(ctx, repository.DeviceFilter{
		ManufacturerID: q.ManufacturerID,
		// AuthorizedStoreIDs 保证非 nil：空切片编码成 '{}'，ANY('{}') 恒假——一个点位
		// 都没授权的账号命中零行，而不是不过滤。
		StoreIDs: scope.AuthorizedStoreIDs(),
		Status:   q.Status,
		Keyword:  q.Keyword,
		Page:     page,
		PageSize: pageSize,
	})
}

// Get 返回范围内的一台设备。范围外的设备与不存在的设备都返回 ErrDeviceOutOfScope。
func (s *MerchantDeviceService) Get(ctx context.Context, scope auth.StoreScope, id string) (*model.Device, error) {
	device, err := s.master.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	// 归属检查就在这一次包含判断里：范围是从本商户展开出来的，所以「在集合里」已经
	// 蕴含「属于本商户」，不必再比一次商户——那种重复判断会在某次改动后与这里分叉，
	// 而分叉出来的那一份仍然是放行。
	//
	// StoreID 为 nil 的设备（还没挂到任何点位上）同样出局：没有任何一个商户账号能被
	// 授权到一台不属于任何点位的设备上。
	if device.StoreID == nil || !slices.Contains(scope.AuthorizedStoreIDs(), *device.StoreID) {
		return nil, ErrDeviceOutOfScope
	}
	return device, nil
}
