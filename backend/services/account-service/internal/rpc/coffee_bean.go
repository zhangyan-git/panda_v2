package rpc

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CoffeeBeanService 是咖啡豆账户对内的 gRPC 面（方案 5.6 的 account-service 里豆那一半）。
//
// 与 FortuneCardService 同形，但**少一个维度**：没有冻结，也没有后台调整。豆在支付时就已经
// 扣走，退款窗口里没有可保护的东西；而人工调整是真人在后台点的，走 HTTP，那条路上有权限码
// 与审计——服务令牌面不该有一条能凭空造币的 RPC。
//
// 调用方今天只有一个：payment-service 的纯豆出资（DeductCoffeeBeans）。冲正由本服务自己的
// 售后事件消费者调 service 层，不经过这一层，但方法照样在这里实现并测——它是这个服务的对外
// 契约（退款单那一轮会用），也是 account-service 对 payment-service 唯一能验证的形状。
type CoffeeBeanService struct {
	accountv1.UnimplementedCoffeeBeanServiceServer

	accounts *service.AccountService
}

func NewCoffeeBeanService(accounts *service.AccountService) *CoffeeBeanService {
	return &CoffeeBeanService{accounts: accounts}
}

// GetCoffeeBeanBalance 读一个用户的豆余额（单位分）。
//
// 没有账户行就是 0，不是 NotFound：账户行是第一次调整或第一次扣减时懒创建的，支付在扣减
// 之前先看一眼，拿到 0 才好说那句「咖啡豆余额不足」。
func (s *CoffeeBeanService) GetCoffeeBeanBalance(ctx context.Context, req *accountv1.GetCoffeeBeanBalanceRequest) (*accountv1.GetCoffeeBeanBalanceResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	account, err := s.accounts.BeanAccount(ctx, req.GetUserId())
	if err != nil {
		return nil, coffeeBeanError(err)
	}
	return &accountv1.GetCoffeeBeanBalanceResponse{Balance: account.Balance}, nil
}

// DeductCoffeeBeans 扣减咖啡豆：一张订单用豆全额付掉了。
//
// 重放（同一张订单的第二次扣减）回放已有那笔，replayed 为真——payment-service 在「扣了但
// 事务没落」之后拿同一个订单重试时会看到它，那次重试不会扣第二次。余额不足是
// FailedPrecondition：调用方应当把它翻成一次发起即失败的支付结果，而不是当成服务故障去
// 重试或告警。
func (s *CoffeeBeanService) DeductCoffeeBeans(ctx context.Context, req *accountv1.DeductCoffeeBeansRequest) (*accountv1.DeductCoffeeBeansResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.accounts.ConsumeBeans(ctx, service.BeanConsumeRequest{
		UserID:  req.GetUserId(),
		OrderID: req.GetOrderId(),
		OrderNo: req.GetOrderNo(),
		Amount:  req.GetAmount(),
		// 两个文案字段**必须转发**：proto 上有、这里不读的字段是静默丢的——调用方写进
		// 流水的那句话（payment-service 写的是支付单号）会消失，而链路上没有一处会报错，
		// 两边各自的测试也都过得去。见 DeductCoffeeBeansRequest 的字段注释。
		Title:  req.GetTitle(),
		Remark: req.GetRemark(),
	})
	if err != nil {
		return nil, coffeeBeanError(err)
	}
	return &accountv1.DeductCoffeeBeansResponse{
		BalanceAfter: result.BalanceAfter,
		EntryId:      result.EntryID,
		Replayed:     result.Replayed,
	}, nil
}

// ReverseCoffeeBeanEntry 把一单扣掉的豆还回去（退款冲正）。
//
// 与福卡那个冲正**不一样**：它可能什么都不做，而且那不是错误。那笔订单不是用豆付的
// （绝大多数订单是渠道支付）、或者这条售后已经冲过了，都回 reversed=false。把它做成错误
// 会让一条完全正常的事件一路重试到死信。
//
// 冲正额超过「这笔扣减还没冲回的部分」是 FailedPrecondition：订单域已经按「实付 - 已退 -
// 在途」钳过一次，真撞到说明有一处算错了，欠退比错退更容易被忽略，所以不静默钳制。
func (s *CoffeeBeanService) ReverseCoffeeBeanEntry(ctx context.Context, req *accountv1.ReverseCoffeeBeanEntryRequest) (*accountv1.ReverseCoffeeBeanEntryResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	reversed, err := s.accounts.ReverseBeans(ctx, service.BeanReverseRequest{
		OrderID:     req.GetOrderId(),
		AfterSaleID: req.GetAfterSaleId(),
		AfterSaleNo: req.GetAfterSaleNo(),
		Amount:      req.GetAmount(),
		Remark:      req.GetRemark(),
	})
	if err != nil {
		return nil, coffeeBeanError(err)
	}
	return &accountv1.ReverseCoffeeBeanEntryResponse{Reversed: reversed}, nil
}

// coffeeBeanError 把业务错误翻成 gRPC 状态码。集中在一处，三个方法才能给出同一套回答。
//
// 与 fortuneCardError 分开写而不是合并：两边的哨兵错误名不同（豆是 ErrBean*），合并要么
// 让福卡的映射去认识豆的哨兵（那一半从此不能单独看），要么靠一层别名把两套名字糊起来。
// 重复的只有 switch 的形状，理由却各自独立——豆这边多一条 ErrReverseUncovered 的业务
// 含义，少一条「不可冲正」。
func coffeeBeanError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidUserID),
		errors.Is(err, service.ErrInvalidOrderID),
		errors.Is(err, service.ErrInvalidAmount),
		errors.Is(err, service.ErrInvalidAfterSaleNo):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, repository.ErrInsufficientCoffeeBeans),
		errors.Is(err, repository.ErrReverseUncovered),
		errors.Is(err, repository.ErrBeanAmountNotPositive):
		// 余额不足、冲正超出剩余可冲额、扣减额不是正数：都是「本服务认识的业务结果」，
		// 调用方知道该说什么，不是服务故障。
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repository.ErrBeanEntryNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, repository.ErrBeanEntryKeyConflict):
		// 幂等键撞到了别人的流水：调用方把号发重了，重试也解决不了。
		return status.Error(codes.AlreadyExists, err.Error())
	default:
		return status.Error(codes.Internal, "coffee bean operation failed")
	}
}
