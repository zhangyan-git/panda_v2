package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// ErrOrderOutOfScope 表示这一单不在调用者的数据范围内。
//
// 与「订单不存在」映射成同一个 404：越界与不存在在这里必须给出同一个答案，否则响应
// 本身成了一条存在性预言机——拿着别人的订单号逐个试，403 与 404 的差别会把对方有哪些
// 单说出来。判定放在 service 而不是 controller，因为「看得见什么」是这条链路的规则，
// 将来多一条商户域路由也不该各自再实现一遍。
var ErrOrderOutOfScope = errors.New("order is out of the caller's data scope")

// MerchantOrderQuery 是商户端订单列表的过滤条件。
//
// 它**没有门店字段**，与设备那一侧同一个理由：边界只能来自 scope，查询串里能带门店
// 就等于把范围交回给调用方。列表里门店照样看得见（每一行都带 storeId），只是不能拿来筛。
type MerchantOrderQuery struct {
	Status      string
	OrderNo     string
	Source      string
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Page        int
	PageSize    int
}

// MerchantOrderService 是商户域的只读面：订单列表与详情。
//
// 它包着 OrderService 而不是自己去接仓储：读订单那一套（错误映射、userID 归属判定、
// 详情的五条查询）与后台、小程序两条路用的是同一份实现，抄一遍出来只会在某次改动后
// 与那份分叉。范围是这条路上唯一多出来的东西，所以它也只加这一样。
type MerchantOrderService struct {
	orders *OrderService
}

func NewMerchantOrderService(orders *OrderService) *MerchantOrderService {
	return &MerchantOrderService{orders: orders}
}

// List 返回数据范围内的订单。total 与列表用同一段 where（见 OrderFilter.StoreIDs），
// 所以「先取全量再在内存里过滤」这种写法在这里一出现就会被 total 对不上抓住。
func (s *MerchantOrderService) List(ctx context.Context, scope auth.StoreScope, q MerchantOrderQuery) ([]*repository.OrderRow, int, error) {
	// userID 传空串：这是商户视角，不是某个终端用户查自己的单。
	return s.orders.ListOrders(ctx, repository.OrderFilter{
		Status:      q.Status,
		OrderNo:     q.OrderNo,
		Source:      q.Source,
		CreatedFrom: q.CreatedFrom,
		CreatedTo:   q.CreatedTo,
		// AuthorizedStoreIDs 保证非 nil：空切片编码成 '{}'，ANY('{}') 恒假——一个点位
		// 都没授权的账号命中零行，而不是不过滤。
		StoreIDs: scope.AuthorizedStoreIDs(),
		Page:     q.Page,
		PageSize: q.PageSize,
	}, "")
}

// Get 返回范围内的一单。范围外的订单与不存在的订单都返回 ErrOrderOutOfScope。
func (s *MerchantOrderService) Get(ctx context.Context, scope auth.StoreScope, orderID string) (*repository.OrderDetail, error) {
	detail, err := s.orders.GetOrderDetail(ctx, orderID, "")
	if err != nil {
		return nil, err
	}
	// 归属检查就在这一次包含判断里：范围是从本商户展开出来的，所以「在集合里」已经
	// 蕴含「属于本商户」，不必再比一次商户——那种重复判断会在某次改动后与这里分叉，
	// 而分叉出来的那一份仍然是放行。
	//
	// StoreID 为 nil 的订单（后台建的线下单可能还没挂点位）同样出局：没有任何一个商户
	// 账号能被授权到一笔不属于任何点位的订单上。
	if detail.Order == nil || detail.Order.StoreID == nil ||
		!slices.Contains(scope.AuthorizedStoreIDs(), *detail.Order.StoreID) {
		return nil, ErrOrderOutOfScope
	}
	return detail, nil
}
