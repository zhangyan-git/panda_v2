package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// CompleteOrderInput 是一次「标记完成」。
//
// 没有 UserID：这是后台动作，不是「用户完成自己的单」——用户这一侧没有这个动作，
// 出杯完成由履约服务说了算（见 state.go 里那条边的说明）。
type CompleteOrderInput struct {
	OrderID string
	// ActorID 是点下这一下的人。它写进状态流水与审计，和取消订单同一套规矩：
	// 人工干预必须能回答「是谁做的」。
	ActorID string
	TraceID string
}

// CompleteOrder 把一笔已付款的订单标记为完成。
//
// 这个入口是**临时的**：`paid → completed` 这条边本来的驱动者是履约完成事件，而
// fulfillment-service 还没建。有了它，订单不会永远停在 paid，福卡才有到账的那一天。
// 履约接上来时共用同一个 order.completed，发放口径一个字不用改（见仓储里那个常量）。
//
// 只有 paid 能完成。先在这里判一次是为了给出一句说得清的错误，真正的权威判定在仓储里
// （FOR UPDATE 锁住行之后再判一次，那也是唯一能挡住并发重复标记的地方）。
func (s *OrderService) CompleteOrder(ctx context.Context, in CompleteOrderInput) (*repository.OrderPaymentResult, error) {
	in.OrderID = strings.TrimSpace(in.OrderID)
	if in.OrderID == "" {
		return nil, ErrOrderNotFound
	}
	if !CanTransition(model.OrderStatusPaid, model.OrderStatusCompleted) {
		// 表里没这条路说明状态机被改坏了，而不是这一单不能完成。这种「不可能」要炸出来。
		return nil, ErrOrderNotCompletable
	}
	actorID := strings.TrimSpace(in.ActorID)
	result, err := s.repository.CompleteOrder(ctx, repository.CompleteOrderParams{
		OrderID:   in.OrderID,
		TraceID:   in.TraceID,
		ActorType: model.ActorAdmin,
		ActorID:   &actorID,
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	return result, nil
}
