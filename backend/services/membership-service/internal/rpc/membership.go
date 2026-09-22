// Package rpc 实现 membership-service 的 gRPC 面。包名用 rpc 而不是 grpc，
// 以免遮蔽 google.golang.org/grpc。
//
// 这一层是**薄的**：鉴权、把 proto 消息翻成业务入参、把业务结论翻成响应，别的什么都不做。
// 它持的是同一个 *service.MembershipService，与 HTTP 那两棵树共用一套判定——两条路各写一份
// 规则是「同一个用户在下单页与会员中心里算不算会员」不一致的来源。
//
// # 它只有两条 RPC，而且两条都是**该有**的
//
// GetMemberPriceEntitlement 回答「这个用户此刻算不算会员价」——价格归
// coffee-machine-service，能不能按会员价卖归这里。
//
// GetMembershipPlan 回答「这个套餐现在卖的是什么」——下单要冻进订单行的那份快照（价格、时长、
// 会员价路径）只有本域给得出来。让调用方自己填就等于让调用方定价。
//
// 两条都是**同步读**：客户端在这一刻要的就是这一刻的答案。小程序与后台读会员状态、套餐、流水
// 走 HTTP：那些请求要的是「我的会员长什么样」，形状宽得多，也从来不跨服务。两条入口服务的
// 不是同一批人，所以不合成一套。
package rpc

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
	membershipv1 "github.com/panda-dev/panda-v2/contracts/proto/membership/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MembershipService 是会员服务对内的 gRPC 面。
type MembershipService struct {
	membershipv1.UnimplementedMembershipServiceServer

	memberships *service.MembershipService
}

// NewMembershipService 构造 gRPC 服务。
func NewMembershipService(memberships *service.MembershipService) *MembershipService {
	return &MembershipService{memberships: memberships}
}

// GetMemberPriceEntitlement 是 order-service 下单定价与 coffee-machine-service 展示共用的
// 那条查询。
//
// # 它响应里的每一个字段都是「此刻」的
//
// 没有缓存、没有版本号：调用方在**一次请求内**拿到的答案就是那一次定价的依据。会员恰好在
// 这一毫秒过期，两个服务可能给出差一拍的答案——那没关系，差的那一拍只值一杯咖啡的差价，
// 而为了消除它引入一套快照传播，代价大得多（见契约里那句「它不是会员查询」）。
//
// # 失败只有一种
//
// user_id 不是 uuid → InvalidArgument。**不是会员回 active=false，不是 NotFound、更不是
// 5xx**：那是绝大多数请求的正常答案（每一个下单的人都要来问一次），把它做成错误会让调用方
// 用错误分支处理常态。
func (s *MembershipService) GetMemberPriceEntitlement(ctx context.Context, req *membershipv1.GetMemberPriceEntitlementRequest) (*membershipv1.GetMemberPriceEntitlementResponse, error) {
	// 只认服务令牌。这个面只对内部开放（目前 order-service 与 coffee-machine-service），
	// 与 payment-service 的 CreatePayment 同一条规矩：代表自己操作的基础设施调用才用得起它，
	// 用户令牌在这里没有意义——「谁在问」与「问谁」是两件事，后者在请求体里。
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	entitlement, err := s.memberships.MemberPriceEntitlement(ctx, req.GetUserId())
	if err != nil {
		return nil, entitlementError(err)
	}

	response := &membershipv1.GetMemberPriceEntitlementResponse{
		Active:            entitlement.Active,
		GrantsMemberPrice: entitlement.GrantsMemberPrice,
		MemberPriceMode:   entitlement.MemberPriceMode,
		PlanCode:          entitlement.PlanCode,
		PlanName:          entitlement.PlanName,
	}
	// 不是会员时**不填 expire_at_unix**（契约里写的是「不是会员时为 0」）。
	// 这里显式判一次而不是直接 Unix()：零值时间的 Unix() 是一个实实在在的负数
	// （-62135596800），调用方拿它去减 now 会得到一个荒唐的天数。
	if !entitlement.ExpireAt.IsZero() {
		response.ExpireAtUnix = entitlement.ExpireAt.Unix()
	}
	return response, nil
}

// GetMembershipPlan 是 order-service 下单定价问的那一条。
//
// # 它给的是「此刻」的套餐，调用方必须存下来
//
// 本服务不缓存、不做版本控制：调用方在一次请求内拿到的答案就是那一单的定价依据。而**答案本身
// 要被冻进订单行**（快照），付款之后随 order.paid 回来。所以运营在这之后改价、改时长、下架，
// 都改不动已经卖出去的那一单——开通与发券读的是快照，不是这里再查一次。
//
// # 认证与服务令牌
//
// 与 GetMemberPriceEntitlement 同一条规矩：只认服务令牌。代表某个用户来问的是 HTTP 那条路
// （套餐列表），它不需要令牌就能看——那是「卖什么」，本来就是给人看的。
func (s *MembershipService) GetMembershipPlan(ctx context.Context, req *membershipv1.GetMembershipPlanRequest) (*membershipv1.GetMembershipPlanResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	// 拿的是 GetSellablePlan 而不是 GetPlan：草稿与下架的套餐**买不了**，而这条 RPC 的
	// 唯一用途就是下单。让 order-service 自己去判 status，等于把「哪些套餐能卖」这条规则
	// 复制到第二个服务里，两份迟早不一样。
	plan, err := s.memberships.GetSellablePlan(ctx, req.GetPlanId())
	if err != nil {
		return nil, planError(err)
	}

	return &membershipv1.GetMembershipPlanResponse{Plan: &membershipv1.MembershipPlan{
		PlanId:          plan.ID,
		Code:            plan.Code,
		Name:            plan.Name,
		PriceCents:      plan.PriceCents,
		Period:          plan.Period,
		PeriodCount:     plan.PeriodCount,
		AutoRenew:       plan.AutoRenew,
		MemberPriceMode: plan.MemberPriceMode,
		// 两列在库里可空（auto 模式都是 NULL），proto 里没有「没有」这个值——空串与 0
		// 就是「没有」。契约里写明了这三个字段的配对规则，调用方按 member_price_mode 读。
		MemberPriceCouponTemplateId: derefString(plan.MemberPriceCouponTemplateID),
		MemberPriceCouponsPerPeriod: derefInt32(plan.MemberPriceCouponsPerPeriod),
	}}, nil
}

// derefString / derefInt32 把库上那两列可空的券配置读成 proto 的标量。
//
// 只有这两处用到，所以没有像 controller 那样抽一层通用转换：多一层间接，读的人还要回去看
// 它到底做了什么。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

// planError 把取套餐的错误翻成 gRPC 状态码。
//
//	InvalidArgument    plan_id 不是 uuid。改了才能成，重试没用。
//	NotFound           没有这个套餐——多半是客户端拿了一个过期的 id。
//	FailedPrecondition 套餐存在但**不卖**（draft / disabled）。与 NotFound 分开，是因为调用方
//	                   该做的处理不同：前者要换 id，后者要等它上架；而在后台这两句话也不同
//	                   （「没有这个套餐」vs「这个套餐下架了」）。
//	Internal           我们自己的故障（库连不上、扫行失败）。调用方该退避重试。
//
// **状态消息不回传原始错误文本**：里面的表名与 SQL 片段不该跨服务边界跑（与 entitlementError
// 同一条规矩）。
func planError(err error) error {
	switch {
	case service.IsValidationError(err):
		return status.Error(codes.InvalidArgument, "plan id is not a valid uuid")
	case errors.Is(err, service.ErrPlanNotFound):
		return status.Error(codes.NotFound, "membership plan not found")
	case errors.Is(err, service.ErrPlanNotSellable):
		return status.Error(codes.FailedPrecondition, "membership plan is not on sale")
	default:
		return status.Error(codes.Internal, "membership service error")
	}
}

// entitlementError 把业务错误翻成 gRPC 状态码。
//
// 只有两种可能，这也正是这条 RPC 简单的另一面：
//
//	InvalidArgument  调用方传了个不是 uuid 的 user_id。改了才能成，重试没用。
//	Internal         我们自己的故障（库连不上、扫行失败）。调用方该退避重试。
//
// **状态消息不回传原始错误文本**：里面的表名与 SQL 片段不该跨服务边界跑。排查要用的信息在
// 服务端日志里（writeMembershipError 那边同一条规矩）。
//
// 注意这里**没有 NotFound 分支**：不是会员不是错误，见 MemberPriceEntitlement 的说明。
func entitlementError(err error) error {
	if service.IsValidationError(err) {
		return status.Error(codes.InvalidArgument, "user id is not a valid uuid")
	}
	return status.Error(codes.Internal, "membership service error")
}
