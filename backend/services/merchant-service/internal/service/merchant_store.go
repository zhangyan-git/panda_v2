package service

import (
	"context"
	"errors"
	"slices"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// ErrStoreOutOfScope 表示这个门店不在调用者的数据范围内。
//
// 它被映射成 404 而不是 403：越界与不存在在这里是同一个答案，不然响应本身就
// 成了一条存在性预言机——拿着别人的 id 逐个试，403 与 404 会把对方有哪些门店
// 说出来。这个判定在 service 层，不在 handler 里，因为「看得见什么」是这条链路
// 的规则，将来多一条商户域路由也不该各自再实现一遍。
var ErrStoreOutOfScope = errors.New("store is out of the caller's data scope")

// MerchantStoreService 是商户域的门店只读面：列表与详情。
//
// 与 AdminStoreService 分开，不是为了权限码（商户域没有权限码），而是因为入参不同：
// 后台那一侧的过滤条件来自查询串，商户这一侧有一项来自服务端自己解析出来的数据范围，
// 且那一项不可被请求覆盖。混在一个 List 里，早晚会有人把范围当成一个可选的过滤字段传。
type MerchantStoreService struct {
	stores repository.StoreRepository
}

func NewMerchantStoreService(stores repository.StoreRepository) *MerchantStoreService {
	return &MerchantStoreService{stores: stores}
}

// List 返回数据范围内的门店。请求里的 merchantId 一律被忽略——边界只从 scope 来，
// 让查询串能指定商户就等于把范围交给了调用方。
func (s *MerchantStoreService) List(ctx context.Context, scope auth.StoreScope, f repository.StoreFilter, page, pageSize int) ([]*model.Store, int64, error) {
	f.MerchantID = scope.MerchantID
	f.StoreIDs = scope.AuthorizedStoreIDs()
	list, err := s.stores.FindPage(ctx, f, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	// total 与列表用同一组条件，所以「先取全量再在内存里过滤」这种写法在这里
	// 一出现就会被 total 对不上抓住。
	total, err := s.stores.Count(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// Get 返回范围内的一家门店。范围外的门店与不存在的门店都返回 ErrStoreOutOfScope。
func (s *MerchantStoreService) Get(ctx context.Context, scope auth.StoreScope, id string) (*model.Store, error) {
	store, err := s.stores.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// 归属检查就在这一次包含判断里：范围是从本商户展开出来的，所以「在集合里」
	// 已经蕴含「属于本商户」，不必再比一次 merchant_id——那种重复判断会在某次
	// 改动后与这里分叉，而分叉出来的那一份仍然是放行。
	if !slices.Contains(scope.AuthorizedStoreIDs(), store.ID) {
		return nil, ErrStoreOutOfScope
	}
	return store, nil
}
