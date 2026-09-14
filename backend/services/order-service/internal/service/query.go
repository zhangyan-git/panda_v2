package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// GetOrderDetail 读一张订单的全量。
//
// userID 非空表示这是终端用户在查自己的单：先确认归属，再决定要不要给取杯码。
// 传空串是后台/内部查询，一律不给取杯码。
//
// 取杯码的取舍写在这里而不是 controller 的响应映射里：它是**凭据**，谁拿到谁就能取走
// 那杯咖啡。放在映射层意味着每新增一个返回订单的接口都要记得剥一次，而漏掉的那次不会
// 报错、只会静静地把凭据发出去。放在这里，默认就是没有。
func (s *OrderService) GetOrderDetail(ctx context.Context, orderID, userID string) (*repository.OrderDetail, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, ErrOrderNotFound
	}
	detail, err := s.repository.GetOrderDetail(ctx, orderID)
	if err != nil {
		return nil, mapWriteError(err)
	}
	if userID != "" {
		if detail.Order.UserID != userID {
			// 同 CancelOrder：别人的单回「不存在」。
			return nil, ErrOrderNotFound
		}
		return detail, nil
	}
	for _, line := range detail.Lines {
		line.PickupCode = nil
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
