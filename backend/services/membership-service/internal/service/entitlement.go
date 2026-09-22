package service

import (
	"context"
	"errors"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// Entitlement 是「这个人此刻与会员价的关系」的完整答案。
//
// 它故意不是 一个 bool。三个字段分别回答三个不同的问题，而**只有第一个能由第二个推出来**
// 的反面不成立：
//
//	Active            今天是会员吗（在有效期内、没被冻结、没被撤销）
//	GrantsMemberPrice 这单可以直接按会员价算吗
//	MemberPriceMode   他的会员价是哪条来路（auto=直接算，coupon=得用券）
//
// 「是会员但要靠券」与「不是会员」在界面上要显示完全不同的话（前者是「你有 20 张会员价券」，
// 后者是「开通会员立享会员价」），而只回一个 bool 会逼调用方自己去推这两句话——推出来的
// 那一份迟早在某一端与这里不一致。
type Entitlement struct {
	Active bool
	// GrantsMemberPrice 为真时，调用方**直接按会员价计算这一单**，不需要任何额外凭据。
	//
	// 它只在 Active 且套餐是 auto 模式时为真。coupon 模式的会员拿到的是 false——不是「他不
	// 是会员」，而是「他的会员价要凭一张券换」，见 MemberPriceMode。
	GrantsMemberPrice bool
	// MemberPriceMode 取自**成交快照**（memberships.member_price_mode），不是现查套餐：
	// 用户买的时候是什么模式，他这一期就是什么模式，后台改套餐不该改写已经卖出去的会员。
	//
	// 不是会员时为空串。
	MemberPriceMode string
	// PlanCode / PlanName 是成交快照上的套餐身份，可以拿来在界面上说「你是年度会员」。
	PlanCode string
	PlanName string
	// ExpireAt 是到期时刻，UTC。不是会员时为零值，调用方**不该拿它做减法**——先看 Active。
	ExpireAt time.Time
}

// MemberPriceEntitlement 回答「这个用户此刻算不算会员价」。
//
// # 它不是会员时返回 (零值, nil)，不是错误
//
// 「不是会员」是绝大多数请求的答案（每一个下单的人都要来问一次），把它做成错误会让调用方
// 用错误分支处理常态——而错误分支里多写一句日志就是每天几万行噪音。真正的异常（user_id 不
// 是 uuid、库挂了）才回错误。
//
// # 判定只有这一处
//
// 在有效期内、没被冻结、没被撤销、这个套餐的会员价是自动给还是要用券——这条规则的完整表述
// 只在这里有一份。别的服务不该读本库，也不该按 mode 自己推：那样的话「冻结算不算会员」
// 这个问题会有第二个答案，而两个答案不一致时没有任何东西会报错。
func (s *MembershipService) MemberPriceEntitlement(ctx context.Context, userID string) (Entitlement, error) {
	id, err := requiredID(userID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return Entitlement{}, err
	}

	membership, err := s.repository.GetMembership(ctx, id)
	if err != nil {
		if errors.Is(err, ErrMembershipNotFound) {
			// 不是会员。见上面那段说明：这是常态，不是错误。
			return Entitlement{}, nil
		}
		return Entitlement{}, err
	}

	now := s.Now()
	if !membership.IsUsable(now) {
		// 有效期内但不作数的几种情况（冻结、撤销、已过期）在这里一并落成 false。
		//
		// **回的是完整快照之外的空串模式**：一个冻结中的会员不该被下游按 mode 去发券或算价，
		// 所以连模式都不给——给了它就有被误用的可能。
		return Entitlement{}, nil
	}

	return Entitlement{
		Active:            true,
		GrantsMemberPrice: membership.MemberPriceMode == model.MemberPriceModeAuto,
		MemberPriceMode:   membership.MemberPriceMode,
		PlanCode:          membership.PlanCode,
		PlanName:          membership.PlanName,
		ExpireAt:          membership.ExpireAt.UTC(),
	}, nil
}
