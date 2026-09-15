// Package client 持有 payment-service 的出网 gRPC 客户端。
//
// 今天只有一个方向：account-service 的咖啡豆账本（纯豆出资要扣余额）。支付域与账户域
// 都持有一部分「这笔钱」的事实——钱从哪儿来在 payment_fundings，账变本身在
// coffee_bean_entries——所以这条调用是同步的、在发起支付那一段事务之外，理由与
// 「渠道调用绝不放在 PG 事务里」逐字相同：一次跨服务的往返不该让数据库事务一直开着锁。
package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrInsufficientBeans：用户在账户域的咖啡豆不够付这一单。
//
// 它**不是**故障，是「本服务认识的业务结果」：调用方（service）把它翻成一次发起即失败的
// 支付结果（status='failed'、failure_code='insufficient_coffee_beans'），客户端可以换一种
// 支付方式重试。把它当成普通错误会变成一次 5xx，而用户看到的会是「系统繁忙」。
//
// 定义在这里而不是 service 里，因为「哪个 gRPC 码翻成这个结论」是传输层的知识：
// 账户域的契约写着「余额不足回 FailedPrecondition」（见 coffee_bean.proto）。service
// 那边有一个同名的别名（service.ErrInsufficientCoffeeBeans），业务的判据写在业务层，
// 映射留在这里。
var ErrInsufficientBeans = errors.New("coffee bean balance is insufficient")

// DeductRequest 是一次纯豆出资的扣减请求。
//
// OrderID 必填，而且它就是**幂等键的来源**：账户域按 `order:{orderId}` 派生 entry_key，
// 所以同一张订单重试多少次都只扣一笔——哪怕两次的支付单号不同（用户第一次发起超时了，
// 又发起了一次，那是两张支付单，但订单只有一张、钱只该被收一次）。这正是「幂等号取自
// 订单而不是取自支付单」的全部理由，也是为什么这里没有 request_id 字段可传。
type DeductRequest struct {
	UserID  string
	OrderID string
	OrderNo string
	// Amount 单位为分。
	Amount int64
	// PaymentNo 只进流水备注（账变那一行的 remark），供两侧人工对账时互相对上号。
	// 它**不参与**幂等：账户域不认识支付单。
	PaymentNo string
}

// DeductResult 是账户域回的那两个数。
type DeductResult struct {
	// EntryID 是 coffee_bean_entries.id。它会顺着 payment_fundings.account_entry_id 一路
	// 落到 order_payment_lines.account_entry_id——退款与对账都要按它反查这笔账变。
	EntryID string
	// BalanceAfter 是这一笔之后账户域的余额。
	BalanceAfter int64
	// Replayed 为 true 表示账户域之前就扣过同一张订单了（我们超时重试，或者上一次的响应
	// 丢了）。**这不是错误**：钱只扣了一次，支付可以照常完成。
	Replayed bool
}

// CoffeeBeanClient 是 account-service 咖啡豆账本的客户端。
type CoffeeBeanClient struct {
	beans accountv1.CoffeeBeanServiceClient
	token string
}

// token 是服务令牌：这条 RPC 用 auth.WithServiceToken 走 metadata，**不放请求字段**——
// 放进字段就等于允许调用方替别人声明身份（与 account-service 的 AdminAccessResolver
// 同一条理由，只是那边用的是调用方自己的 access token）。
func NewCoffeeBeanClient(conn grpc.ClientConnInterface, token string) *CoffeeBeanClient {
	return &CoffeeBeanClient{beans: accountv1.NewCoffeeBeanServiceClient(conn), token: token}
}

// Deduct 扣减咖啡豆。
//
// 只有「余额不足」被翻译成业务结论，其余一律保持普通错误：账户服务不可达、超时、内部报错
// 都是真故障，而且**不能**被当成拒绝——那会让一张其实还没扣到钱的支付单标成 failed，而
// 用户可能已经付了别的渠道。service 那边对它们的处置是「支付单停在 created，等超时关单收」。
func (c *CoffeeBeanClient) Deduct(ctx context.Context, req DeductRequest) (DeductResult, error) {
	if c == nil || c.beans == nil {
		return DeductResult{}, errors.New("coffee bean client is not configured")
	}
	resp, err := c.beans.DeductCoffeeBeans(auth.WithServiceToken(ctx, c.token), &accountv1.DeductCoffeeBeansRequest{
		UserId:  req.UserID,
		OrderId: req.OrderID,
		OrderNo: req.OrderNo,
		Amount:  req.Amount,
		Remark:  "支付单 " + req.PaymentNo,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition:
			return DeductResult{}, fmt.Errorf("%w: user %s", ErrInsufficientBeans, req.UserID)
		default:
			return DeductResult{}, fmt.Errorf("deduct coffee beans for order %s: %w", req.OrderNo, err)
		}
	}
	return DeductResult{
		EntryID:      resp.GetEntryId(),
		BalanceAfter: resp.GetBalanceAfter(),
		Replayed:     resp.GetReplayed(),
	}, nil
}
