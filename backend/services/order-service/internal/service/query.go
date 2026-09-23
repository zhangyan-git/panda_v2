package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// GetOrderDetail 读一张订单的全量。
//
// userID 非空表示这是终端用户在查自己的单：先确认归属。
//
// 这里曾经按「取杯码是凭据」剥掉后台视角的 PickupCode。那个定性建立在一次凭空的列拆分上
// （库里只有 pickup_code 一列，没有 pickup_no）：原型里取杯口那块屏幕本来就把它大字
// 摆着，用户侧叫取杯号、屏幕上叫取杯码，是同一个值，从来不是秘密。后台要看它，客服最常被
// 问的就是「我的号是多少」。
func (s *OrderService) GetOrderDetail(ctx context.Context, orderID, userID string) (*repository.OrderDetail, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, ErrOrderNotFound
	}
	detail, err := s.repository.GetOrderDetail(ctx, orderID)
	if err != nil {
		return nil, mapWriteError(err)
	}
	if userID != "" && !userMatches(detail.Order.UserID, userID) {
		// 同 CancelOrder：别人的单回「不存在」。
		return nil, ErrOrderNotFound
	}
	return detail, nil
}

// FindOrder 按 ID 读订单头部（不带行与流水）。后台列表点进详情前用它确认存在。
func (s *OrderService) FindOrder(ctx context.Context, orderID string) (*model.Order, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, ErrOrderNotFound
	}
	order, err := s.repository.FindOrderByID(ctx, orderID)
	if err != nil {
		return nil, mapWriteError(err)
	}
	return order, nil
}

// ListOrders 分页查订单。
//
// userID 非空时**覆盖**筛选条件里的 UserID，而不是「顺便也筛一下」：小程序端传上来的
// 任何筛选值都是用户可控的，如果它传的是别人的 user_id 而我们只是「也筛一下自己」，
// 就会变成一次越权查询。终端用户的那条路只有一个答案——他自己的单。
func (s *OrderService) ListOrders(ctx context.Context, f repository.OrderFilter, userID string) ([]*repository.OrderRow, int, error) {
	if userID != "" {
		f.UserID = userID
	}
	return s.repository.ListOrders(ctx, f)
}
