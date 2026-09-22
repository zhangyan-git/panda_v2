// Package client 是 partner-service 对**下游服务**的适配器。
//
// 两个客户端，方向相同（都是出向的 gRPC）、性质完全不同：
//
//   - MembershipEntitlementReader：开放接口那条路上的业务调用（查会员权益）。
//   - AdminAccessResolver：后台那条路上的实时鉴权（问 user-service「这个账号现在能做什么」）。
//
// 它们各留一份而不抽到 platform：两者都是**平台契约的适配器**（把 contracts 里某个具体服务
// 的 RPC 翻译成本服务认得的形状），抽到 platform 会让 platform 依赖 contracts 里某个服务的
// 版本，而那正是 platform 不该知道的事。与 payment-service / order-service 等六个服务里那几
// 对同名的适配器是同一份实现、同一条理由。
package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	membershipv1 "github.com/panda-dev/panda-v2/contracts/proto/membership/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 会员权益读不到的两种说法。与 order-service 的 MembershipPlanReader 同一套切法：
//
//	ErrEntitlementUserInvalid   我们（或合作方）把一个不是 uuid 的 user_id 发出去了 → 400。
//	ErrMembershipUnavailable    会员服务没答上来（连不上、内部错误、应答是空的）→ 503。
//
// 分开的理由很具体：**不是会员**与**问不到**在这一条接口上极容易混成一个「active=false」。
// 把它们合起来，合作方会把一次下游抖动当成「这个人不是会员」写进他自己的业务——而那是他的
// 用户，他没有任何办法知道该重试。
var (
	// ErrEntitlementUserInvalid：user_id 不是 uuid，会员域拒了这个参数。
	ErrEntitlementUserInvalid = errors.New("user id is not a valid UUID")
	// ErrMembershipUnavailable：会员服务没答上来。
	ErrMembershipUnavailable = errors.New("membership service is unavailable")
)

// MembershipEntitlement 是「这个人此刻算不算会员价」的那一组事实。
//
// 只带开放接口要回给合作方的几个字段，**不整个透传 proto**：透传会把会员域的字段名变成
// 我们的对外契约，于是对方改一次 proto（哪怕只是加一个字段）都会牵动我们的对接文档。
// 这一份是冻结的形状，改它得有人想一遍「合作方那边怎么办」。
//
// 字段含义照抄 membership.proto 的注释，两处不一致时以那边为准：
//
//	Active            此刻是不是有效会员（冻结中的会员这里是 false）
//	GrantsMemberPrice 此刻能不能**直接**按会员价算（只有年度会员 auto 为真）
//	MemberPriceMode   auto=本人自动享 / coupon=靠会员价体验券；不是会员时空串
//	PlanCode/PlanName 成交快照上的套餐（后台改过套餐名不影响这里）
//	ExpireAtUnix      到期时间（Unix 秒）；不是会员时 0
type MembershipEntitlement struct {
	Active            bool
	GrantsMemberPrice bool
	MemberPriceMode   string
	PlanCode          string
	PlanName          string
	ExpireAtUnix      int64
}

// MembershipEntitlementReader 按 user_id 读会员价资格。
//
// # 它带服务令牌，而请求里没有合作方
//
// 这次调用代表**平台**去问会员域一个事实，不代表合作方，也不代表那个用户：用户身份是我们
// 从合作方的请求里拿到的（那个 ID 是合作方给的），而合作方身份**进不了这条 RPC**——
// GetMemberPriceEntitlementRequest 只有 user_id 一个字段，而本仓库也没有「操作人元数据」
// 这种约定（platform/auth 与 platform/client 里都没有 WithOperator 之类的东西）。
//
// 所以会员域看不到「这次查询是哪个合作方发起的」。要让下游能审计这件事，得先给 proto 加一个
// 字段或者定一层元数据约定——两件事都不在本轮范围内（不能往别人的 proto 里加东西）。这个
// 缺口在回报里列出来，不在代码里绕过去（例如把 partner_id 塞进 user_id 是绝对不行的）。
type MembershipEntitlementReader struct {
	memberships membershipv1.MembershipServiceClient
	token       string
	timeout     time.Duration
}

// NewMembershipEntitlementReader 复用调用方那条连接：main 只拨一次，多个调用点共享同一个
// *grpc.ClientConn。
//
// 三个参数都拒绝空值/零值：一个没有令牌的客户端会在运行时被会员域拒掉（Unauthenticated），
// 而那看起来像一次权限事故而不是一次装配错误。
func NewMembershipEntitlementReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*MembershipEntitlementReader, error) {
	if conn == nil {
		return nil, errors.New("membership service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("membership service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("membership service timeout must be positive")
	}
	return &MembershipEntitlementReader{
		memberships: membershipv1.NewMembershipServiceClient(conn),
		token:       token,
		timeout:     timeout,
	}, nil
}

// GetMemberPriceEntitlement 读一个用户的会员价资格。
//
// 超时是**这一层**加的（context.WithTimeout），不是靠调用方传进来的 ctx：合作方那头可能
// 永远不超时，而这条链路上每一跳都必须有自己的上限。服务令牌走 metadata
// （auth.WithServiceToken），不走请求字段——调用方无法在这个字段上声称自己是别人。
//
// **没有「用户不存在」这一支**：会员域对查不到的 user_id 回的是 active=false（一个不是会员
// 的人与一个不存在的 id 在「能不能按会员价买」这个问题上确实是同一个答案）。所以这里不需要
// NotFound 映射——多一个分支只会让人以为存在一个「查不到这个人」的状态。
func (r *MembershipEntitlementReader) GetMemberPriceEntitlement(ctx context.Context, userID string) (*MembershipEntitlement, error) {
	if r == nil || r.memberships == nil {
		return nil, errors.New("membership entitlement reader is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()

	resp, err := r.memberships.GetMemberPriceEntitlement(ctx,
		&membershipv1.GetMemberPriceEntitlementRequest{UserId: userID})
	if err != nil {
		switch status.Code(err) {
		case codes.InvalidArgument:
			return nil, ErrEntitlementUserInvalid
		default:
			return nil, ErrMembershipUnavailable
		}
	}
	// resp 为 nil 是「一次坏掉的回答」，不是「这个人不是会员」：grpc 在成功码上返回 nil
	// 应答时，把它解释成「非会员」等于让一次协议层的意外变成一条业务结论。
	if resp == nil {
		return nil, ErrMembershipUnavailable
	}
	return &MembershipEntitlement{
		Active:            resp.GetActive(),
		GrantsMemberPrice: resp.GetGrantsMemberPrice(),
		MemberPriceMode:   resp.GetMemberPriceMode(),
		PlanCode:          resp.GetPlanCode(),
		PlanName:          resp.GetPlanName(),
		ExpireAtUnix:      resp.GetExpireAtUnix(),
	}, nil
}
