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

// 会员套餐读不到的两种说法。分开是因为**调用方该做的处理不同**：
//
//	ErrMembershipPlanNotFound   这个套餐不存在（或者已经下架）。换一个 id 才有用，
//	                            重发一模一样的一次没用——所以它是一条 400，不是 503。
//	ErrMembershipPlanUnavailable 会员服务没答上来。它是故障，不是业务结论，稍后重试即可。
//
// 混成一个的代价与支付那边一样：把下游抖动说成「你选的套餐买不了」，用户会换一个套餐，
// 而他其实什么也没做错。
var (
	// ErrMembershipPlanNotFound：套餐不存在，或者存在但不可售（draft / disabled）。
	//
	// 两种情况在这里合成一个：对下单的人来说它们是同一件事——**这个套餐现在买不了**——
	// 而「下架了」与「没有这个 id」的区别是给运营看的线索，不需要穿过两个服务。
	ErrMembershipPlanNotFound = errors.New("membership plan is not on sale")
	// ErrMembershipPlanUnavailable：会员服务没答上来（连不上、内部错误、应答是空的）。
	ErrMembershipPlanUnavailable = errors.New("membership service is unavailable")
	// ErrMemberPriceUnavailable：问「这个人算不算会员价」没问到。
	//
	// 与上面那条分开是因为**问的不是同一件事**：那一条问「这个套餐卖什么」，这一条问
	// 「这个人能不能便宜」。都落在 503 上，但排查时「套餐查不到」与「资格没问到」是两条
	// 完全不同的线索。
	//
	// InvalidArgument 不单独分档（与 Get 的处理不同）：这里的 userID 来自验过的令牌，不是
	// 客户端填的，会员域判它「不是 uuid」只可能是我们自己的 bug——那种情况下无论回什么，
	// 调用方都无事可做。
	ErrMemberPriceUnavailable = errors.New("member price entitlement is unavailable")
)

// MembershipPlan 是下单要用的**那一组**套餐事实。
//
// 只带这几个字段，不整个透传 proto：这一份会被冻进订单行，冻进去的每一个字段都得有人读
// ——多带一个（描述、权益文案、上架状态）就多一处「两个服务对它的理解迟早不一样」，
// 而它们对订单没有任何判断依据。
type MembershipPlan struct {
	ID   string
	Code string
	Name string
	// PriceCents 是套餐价（分），单位与订单一致。**它才是会员行的价格**：调用方拿它算
	// 原价与应付额，不读请求里的任何价格。
	PriceCents int64
	// Period / PeriodCount 是每期时长（month/1 = 连续包月，year/1 = 年度会员）。
	Period      string
	PeriodCount int32
	// AutoRenew 是套餐层面的「这个产品要签代扣」，不是用户开关。
	AutoRenew bool
	// MemberPriceMode 与下面两列是会员价的来路（auto 自动享 / coupon 发券）。
	// coupon 模式下后两列才有值，库上 auto 模式它们是 NULL。
	MemberPriceMode             string
	MemberPriceCouponTemplateID string
	MemberPriceCouponsPerPeriod int32
}

// MembershipPlanReader 读会员域答得上来的那两个问题：这个套餐卖什么（Get）、这个人此刻算不算
// 会员价（Entitlement）。名字里只剩「Plan」确实窄了，而这里不改成「Client」是有意的——它指的是
// **一个下游服务一条连接**这个事实（与 DeviceReader 同一条约定，那边也留着一个说清楚了的名字）。
//
// 它只读不写：下单不碰会员域的任何状态，「谁成了会员」是付款成功之后由 order.paid 触发的，
// 那条路走事件不走这里。
type MembershipPlanReader struct {
	plans   membershipv1.MembershipServiceClient
	token   string
	timeout time.Duration
}

// NewMembershipPlanReader 复用调用方那条连接：main 只拨一次；多个调用点共享同一个 *grpc.ClientConn。
//
// 带的是**服务令牌**：这一次调用代表订单域去问会员域一个事实，不代表某个用户。用户身份
// 在下单这条路上早就用过了（令牌解出来的 userID 是订单的归属），到这里只剩一个值引用。
//
// 没有「按用户读套餐」这种形状是有意的：套餐是公开的商品目录，每个人的都一样，区别只在
// 「这个人能不能按会员价买」。后者是另一条 RPC（GetMemberPriceEntitlement），本条不碰它。
func NewMembershipPlanReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*MembershipPlanReader, error) {
	if conn == nil {
		return nil, errors.New("membership service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("membership service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("membership service timeout must be positive")
	}
	return &MembershipPlanReader{plans: membershipv1.NewMembershipServiceClient(conn), token: token, timeout: timeout}, nil
}

// Get 读一个套餐。found 为 false 表示会员域说这个套餐买不了（不存在或已下架）。
//
// 三种失败分得很清，理由与 DeviceReader.Get 同一条：调用方要能分辨「这个套餐买不了」
// （拒单，400）与「会员服务没答上来」（不拒单也成不了，503）。空应答按后者处理——一个没有
// plan_id 的应答不是「套餐存在但没有 id」，它是一次坏掉的回答。
//
// **价格与时长完全以这里的应答为准**：请求里的任何价格都不参与会员行的计算。一条
// `originalUnitPrice=1`、快照写着年卡的请求，在这里会被套餐自己的价格与时长覆盖掉。
func (r *MembershipPlanReader) Get(ctx context.Context, planID string) (*MembershipPlan, bool, error) {
	if r == nil || r.plans == nil {
		return nil, false, errors.New("membership plan reader is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()

	resp, err := r.plans.GetMembershipPlan(ctx, &membershipv1.GetMembershipPlanRequest{PlanId: planID})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound, codes.FailedPrecondition:
			// 「没有这个套餐」与「这个套餐下架了」在下单这条路上是同一件事：现在买不了。
			// 会员域那边分得清（它的后台要用那个区别），订单域用不上。
			return nil, false, nil
		case codes.InvalidArgument:
			// plan_id 不是 uuid。这是**我们**把一个坏值发出去了（service 层本该先挡），
			// 当成一次调用方错误往上抛，让它是 400 而不是「会员服务挂了」。
			return nil, false, ErrMembershipPlanNotFound
		default:
			return nil, false, ErrMembershipPlanUnavailable
		}
	}
	plan := resp.GetPlan()
	if plan == nil || plan.GetPlanId() == "" {
		return nil, false, ErrMembershipPlanUnavailable
	}
	return &MembershipPlan{
		ID:                          plan.GetPlanId(),
		Code:                        plan.GetCode(),
		Name:                        plan.GetName(),
		PriceCents:                  plan.GetPriceCents(),
		Period:                      plan.GetPeriod(),
		PeriodCount:                 plan.GetPeriodCount(),
		AutoRenew:                   plan.GetAutoRenew(),
		MemberPriceMode:             plan.GetMemberPriceMode(),
		MemberPriceCouponTemplateID: plan.GetMemberPriceCouponTemplateId(),
		MemberPriceCouponsPerPeriod: plan.GetMemberPriceCouponsPerPeriod(),
	}, true, nil
}

// MemberPriceEntitlement 是「这个人此刻算不算会员价」的答案——饮品行定价要的那一格。
//
// 只带 GrantsMemberPrice，不带 active 与 member_price_mode：本服务问这一句只为了定价，而
// 定价只有「按会员价」与「按原价」两种走法（连续包月是后者，会员价长在券上）。那两个字段
// 是把「你是会员、但要用券」与「你不是会员」分开给人看的，那是收银台文案的事，本服务没有
// 收银台。哪天有调用方真要用，从 proto 上取就是——它们一直在应答里。
type MemberPriceEntitlement struct {
	// GrantsMemberPrice 为 true 表示这一单的饮品**直接**按会员价算（只有年度会员为真）。
	//
	// false 不是「没会员」：连续包月的会员这里也是 false，他的会员价在会员价体验券上，
	// 那是券那条路的事。把它读成「没会员」会让包月用户白丢权益，读成「有会员」会让所有人
	// 都按会员价卖——两种读法都比按原价算糟。
	GrantsMemberPrice bool
}

// Entitlement 问会员域「这个人此刻能不能按会员价买饮品」。
//
// 它复用与 Get 同一条连接、同一个服务令牌、同一个超时：这里问的是**会员域的一个事实**，
// 不代表某个用户——用户身份在下单那条路上早就用过了（令牌解出来的 userID 是订单的归属），
// 到这里只剩一个值引用。
//
// **不是会员不是错误**：那是绝大多数请求的正常答案，应答里 grants=false。所以这个方法没有
// 「found」这一档，它要么给出一个结论，要么报 ErrMemberPriceUnavailable。
func (r *MembershipPlanReader) Entitlement(ctx context.Context, userID string) (*MemberPriceEntitlement, error) {
	if r == nil || r.plans == nil {
		return nil, errors.New("membership plan reader is not configured")
	}
	if strings.TrimSpace(userID) == "" {
		// 空 userID 在这里挡掉：会员域会回 InvalidArgument，而那句话在跨服务的路上会变成
		// 一次多余的往返。调用方拿不到用户 id 是它自己的问题，不该问出去。
		return nil, errors.New("user id is required")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.plans.GetMemberPriceEntitlement(ctx, &membershipv1.GetMemberPriceEntitlementRequest{UserId: userID})
	if err != nil {
		return nil, ErrMemberPriceUnavailable
	}
	// 空应答按「没答上来」处理：一个 nil 的应答不是「这个人不是会员」，它是一次坏掉的回答。
	// 与 Get 里那条同一条规矩——差别在于那条能靠 plan_id 是否为空认出来，这条认不出来，
	// 所以只挡最明显的 nil。
	if resp == nil {
		return nil, ErrMemberPriceUnavailable
	}
	return &MemberPriceEntitlement{GrantsMemberPrice: resp.GetGrantsMemberPrice()}, nil
}
