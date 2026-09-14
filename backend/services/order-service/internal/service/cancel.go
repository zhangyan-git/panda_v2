package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// CancelOrderInput 是一次取消。
type CancelOrderInput struct {
	OrderID string
	Reason  string
	// UserID 非空表示这是「用户本人取消自己的单」：先确认这张单确实属于他。
	// 后台取消传空串，走的是另一条（有权限门槛的）入口。
	UserID string
	// ActorType 与 ActorID 写进状态流水：谁取消的。取值同 model 里的 Actor* 常量。
	ActorType string
	ActorID   *string
	TraceID   string
}

// CancelOrder 取消一笔待支付的订单。
//
// 只有待支付能取消。付过款的要走退款（售后，方案 7.4），已关单的不动产——这两条都由
// 状态机挡住，先在这里判一次是为了给出一句说得清的错误，真正的权威判定在仓储里
// （FOR UPDATE 锁住行之后再判一次，那也是唯一能挡住并发取消的地方）。
func (s *OrderService) CancelOrder(ctx context.Context, in CancelOrderInput) (*repository.OrderPaymentResult, error) {
	in.OrderID = strings.TrimSpace(in.OrderID)
	in.Reason = strings.TrimSpace(in.Reason)
	if in.OrderID == "" {
		return nil, ErrOrderNotFound
	}
	if in.Reason == "" {
		return nil, ErrCancelReasonRequired
	}
	if in.UserID != "" {
		order, err := s.repository.FindOrderByID(ctx, in.OrderID)
		if err != nil {
			return nil, mapWriteError(err)
		}
		if order.UserID != in.UserID {
			// 别人的单回「不存在」而不是「无权限」：回 403 等于告诉调用方「这个 id 是真的」，
			// 那就把一个能拿来遍历单号的接口送出去了。
			return nil, ErrOrderNotFound
		}
	}
	if !CanTransition(model.OrderStatusPendingPayment, model.OrderStatusCancelled) {
		// 表里没这条路说明状态机被改坏了，而不是这一单不能取消。这种「不可能」要炸出来。
		return nil, ErrOrderNotPending
	}

	result, err := s.repository.CancelOrder(ctx, repository.CancelOrderParams{
		OrderID:   in.OrderID,
		Reason:    in.Reason,
		RequestID: in.TraceID,
		TraceID:   in.TraceID,
		ActorType: in.ActorType,
		ActorID:   in.ActorID,
		// 后台取消留审计：它是管理员对用户资产的一次人工干预，要能回答「是谁在什么时候
		// 取消了这一单」。用户自己取消不留——那不是管理操作，订单的状态流水已经记了。
		Audit: in.ActorType == model.ActorAdmin,
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	return result, nil
}

// ExpireOverdue 关掉到点未支付的订单，返回关掉的条数。超时关单的任务调用它。
func (s *OrderService) ExpireOverdue(ctx context.Context, limit int, traceID string) (int, error) {
	return s.repository.ExpireOverdue(ctx, limit, traceID)
}
