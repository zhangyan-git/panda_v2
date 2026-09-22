// Package service 是资产账户域的业务规则。
//
// 两半都在这里（方案 5.6）：福卡（余额、流水、发放 / 扣减 / 冲正 / 冻结）与咖啡豆（余额、
// 流水、扣减 / 冲正 / 人工调整）。做法与边界对两半是同一套：
//
//   - 它拥有**账户与账变**——余额、不可变流水、每一笔增删。这是它唯一能改的东西。
//   - 它不拥有**发放规则**。一单该送几张、基础送多少、哪个活动加赠多少，是订单域在下单时
//     冻结下来的承诺快照说了算；它们拆好再发过来，本服务只把结果记账。
//   - 它不拥有**抽奖规则**，也不拥有**支付规则**。扣几张、扣多少分都是调用方给的，本服务
//     只判「够不够扣」。
//
// 所以这一层薄，但它不是转发：幂等键、有符号金额、余额不会被扣穿、文案，都是在这里定的。
// 把这些散到 controller 或 repository 里去，同一件事就会有两处说法。
//
// 后台人工调整单独一个服务类型（admin_bean.go）：它持的是带审计的那条写路径，规则也不同
// ——金额带符号、幂等号撞车要回 409。那是全系统唯一能凭空改动豆余额的入口。
package service

import (
	"context"
	"errors"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

var (
	// ErrInvalidEvent：order.completed 的事件体不合法。重投不会让它变合法，
	// 所以它是一条要进死信、要有人看的错误——而不是被 ack 掉的噪音。
	ErrInvalidEvent = errors.New("invalid order.completed event")
	// ErrInvalidUserID：用户 ID 缺失或不是 UUID。
	ErrInvalidUserID = errors.New("user id must be a UUID")
	// ErrInvalidEntryID：流水 ID 缺失或不是 UUID。
	ErrInvalidEntryID = errors.New("entry id must be a UUID")
	// ErrInvalidAmount：张数不是正数。扣 0 张或扣负数都不是一次账变。
	ErrInvalidAmount = errors.New("amount must be positive")
	// ErrInvalidRequestID：调用方没给幂等号。扣减必须能重放，没有它就只能靠运气。
	ErrInvalidRequestID = errors.New("request id is required")
	// ErrInvalidAfterSaleNo：售后单号缺失。它是冻结的幂等键，没有它就没有「只冻一次」。
	ErrInvalidAfterSaleNo = errors.New("after sale no is required")
	// ErrInvalidOrderID：订单 ID 缺失或不是 UUID。它不只是「哪张订单」——豆扣减的幂等键
	// 由它派生、冲正也按它反查，所以它是一次豆扣减的必填字段，不是备注。
	ErrInvalidOrderID = errors.New("order id must be a UUID")
)

// Repository 是本服务需要的持久化面。
//
// 定义在这里而不是直接要 *repository.PostgresRepository，是为了让「文案怎么渲染」「事件
// 怎么校验」这几条与 SQL 无关的规则能用一根桩测到——它们是本服务真正拥有的东西。
type Repository interface {
	GrantOrderFortune(context.Context, repository.GrantParams) ([]repository.EntryResult, error)
	Deduct(context.Context, repository.DeductParams) (repository.EntryResult, error)
	Reverse(context.Context, repository.ReverseParams) (repository.EntryResult, error)
	FreezeAfterSale(context.Context, repository.FreezeParams) (bool, error)
	PreviewFreezeAfterSale(context.Context, repository.PreviewFreezeParams) (int64, int64, error)
	ReleaseAfterSale(context.Context, repository.ReleaseParams) (bool, error)
	RecoverAfterSale(context.Context, repository.RecoverParams) (int64, error)
	GetAccount(context.Context, string) (*model.FortuneCardAccount, error)
	ListEntries(context.Context, dto.EntryQuery) ([]*model.FortuneCardEntry, int, error)
	ListFreezes(context.Context, dto.FreezeQuery) ([]*model.FortuneCardFreeze, int, error)

	// 咖啡豆那一半。只有三条：扣减、退款冲正、读账户与流水。**没有冻结那一组**——豆在
	// 支付时就已经扣走，退款窗口里没有可保护的东西，见 repository/coffee_bean.go。
	ConsumeBeans(context.Context, repository.BeanConsumeParams) (repository.EntryResult, error)
	ReverseBeans(context.Context, repository.BeanReverseParams) (bool, error)
	GetBeanAccount(context.Context, string) (*model.CoffeeBeanAccount, error)
	ListBeanEntries(context.Context, dto.BeanEntryQuery) ([]*model.CoffeeBeanEntry, int, error)
}

// AccountService 是福卡账户的应用服务。
type AccountService struct {
	repository Repository
	// now 让「用哪个时刻」在被调用方决定之外还能被测试决定。流水的时间要么来自事件的
	// 业务时间，要么是此刻——两者都必须能说清楚，而不是散落的 time.Now()。
	now func() time.Time
}

func New(r Repository) *AccountService {
	return &AccountService{repository: r, now: func() time.Time { return time.Now().UTC() }}
}
