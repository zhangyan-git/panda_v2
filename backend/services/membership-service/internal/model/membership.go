package model

import "time"

// 会员状态。与 migrations/membership/001 里 memberships.status 的 CHECK 逐字一致。
//
// 判「算不算会员价」只看 active：frozen 是权益暂停（争议/风控，到期时间不动，解冻后继续），
// 停着的时候不该享价。expired 与 revoked 的差别只在对用户怎么说：前者是自然走完，
// 后者是被撤销、不再恢复。
const (
	MembershipStatusActive  = "active"
	MembershipStatusFrozen  = "frozen"
	MembershipStatusExpired = "expired"
	MembershipStatusRevoked = "revoked"
)

// Membership 是一条会员资格：谁现在是会员、到什么时候。
//
// 一个用户只有一条（memberships_user_unique）。**续费不新建行**，是把 ExpireAt 往后加
// ——原型就是「在当前剩余天数上增加 N 天」。历史期次靠 membership_changes 的
// from_expire_at / to_expire_at 还原，不另建期次表。
//
// 换套餐（比如包月转年卡）也是改这一行：PlanID 与那三个快照跟着换。所以这行上的快照
// 描述的是「当前生效的这一期是按什么卖的」，不是「第一次买的是什么」——后者在
// membership_changes 里。
type Membership struct {
	ID string `db:"id"`
	// UserID 是 user-service 的用户 ID，仅作值引用（跨库不建外键）。
	UserID string `db:"user_id"`
	// LegacyID 是老库 user_memberships._id，只有迁移过来的会员才有值。
	LegacyID string `db:"legacy_id"`
	// StoreID 是**归属门店**：这个人是谁拉来的，不是「他在哪家店能用会员价」。
	//
	// 它与会员权益毫无关系——会员价在哪家店用都一样，核销不看它，下单不看它，也不参与分账。
	// 它只回答一个运营问题：「这个会员算哪家店的业绩」。
	//
	// 固化规则（三条，判据全在 renewMembership 里那个 base 分叉上）：
	//
	//  1. 第一次成为会员那一刻写入；
	//  2. 续费、升级、换套餐**不覆盖**——还在有效期内就说明他是同一个人拉来的，多交一次钱
	//     不改变这件事；
	//  3. 会员**过期之后**重新开通才可变——上一段会员关系已经结束，这一次是新的拉新。
	//
	// 空串表示没有归属（小程序线上买会员、后台开通时没选门店），与 LegacyID 同一条写法：
	// 可空 UUID 用 *string 会让每一处读都多一层判空，而「空串 = 没有归属门店」是一个说得清
	// 的状态。
	StoreID string `db:"store_id"`
	PlanID  string `db:"plan_id"`
	// PlanCode / PlanName 是成交当时的套餐快照。套餐改名、改价、下架、改权益都不能改变
	// 已经卖出去的会员：只靠 PlanID 现查套餐，等于让一次后台编辑改写所有历史会员的权益。
	PlanCode string `db:"plan_code"`
	PlanName string `db:"plan_name"`
	// MemberPriceMode 与下面两列同样是成交快照。发券按这条记录发，不按套餐现查：
	// 套餐改了模式、换了券模板或改了张数，都不能追溯改变已经买了的人下一期收到什么。
	MemberPriceMode             string    `db:"member_price_mode"`
	MemberPriceCouponTemplateID *string   `db:"member_price_coupon_template_id"`
	MemberPriceCouponsPerPeriod *int32    `db:"member_price_coupons_per_period"`
	Status                      string    `db:"status"`
	StartAt                     time.Time `db:"start_at"`
	ExpireAt                    time.Time `db:"expire_at"`
	// AutoRenew 是**用户可关**的那个开关，与套餐的 auto_renew（产品是否支持自动续费）
	// 不是一回事：套餐说「这个产品可以签代扣」，这里说「这个用户现在开着」。关掉只影响
	// 下一期，本期权益到 ExpireAt 为止。
	//
	// **它是 membership_subscriptions 的投影，不是第二份事实**：存在一条活着的订阅
	// （pending_sign 不算，active / suspended 算）就是真，一条都没有就是假。凡是订阅状态变了
	// 的地方都在同一个事务里把它拨过来（见 repository.applyAutoRenew），而权威的判据永远是
	// 订阅行——要判断「这个人下个月还会不会被扣」时去查 subscriptions，不要查这一列。
	AutoRenew      bool       `db:"auto_renew"`
	AutoRenewOffAt *time.Time `db:"auto_renew_off_at"`
	// RenewalCount / LastRenewedAt 是累计值。续费不新建行，所以这两个数只能落在这里，
	// 否则「续了几次」就没地方查了。
	RenewalCount  int32      `db:"renewal_count"`
	LastRenewedAt *time.Time `db:"last_renewed_at"`
	FrozenAt      *time.Time `db:"frozen_at"`
	FreezeReason  string     `db:"freeze_reason"`
	RevokedAt     *time.Time `db:"revoked_at"`
	RevokeReason  string     `db:"revoke_reason"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`
}

// IsUsable 表示此刻这条会员算不算数——既在有效期内、也没被冻结或撤销。
//
// now 是参数而不是 time.Now()：判定与写库必须在同一个时刻上（读的时候还在期内、写下去的
// 时候已经过期，就会记出一条「过期了还续费成功」的流水），也让测试不用去改系统时钟。
func (m *Membership) IsUsable(now time.Time) bool {
	return m.Status == MembershipStatusActive && m.ExpireAt.After(now)
}

// IsExpiredBy 表示这条会员按时间已经走完了——状态还停在 active、但过期时间已过。
//
// 到期扫描 worker 就按它筛。跟 IsUsable 的区别在于它是「该改状态了」，不是「还能不能用」：
// 扫描会把这个条件为真的行翻成 expired。分成两个方法是因为它们会被问在不同的问题上
// ——前者问「要不要给会员价」，后者问「要不要写一条 expire 流水」。
func (m *Membership) IsExpiredBy(now time.Time) bool {
	return m.Status == MembershipStatusActive && !m.ExpireAt.After(now)
}
