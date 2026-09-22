package dto

import "time"

// PlanQuery 是后台套餐列表的筛选条件。
type PlanQuery struct {
	// draft / active / disabled，空表示不筛。
	Status string
	// Name 是模糊搜（ILIKE）。按 code 搜也走它——code 是给系统看的稳定标识，但运营手里
	// 常常只有「那个 monthly_auto」，让他搜不到只能去翻。
	Keyword  string
	Page     int
	PageSize int
}

// MembershipQuery 是后台会员列表的筛选条件。
type MembershipQuery struct {
	// 精确匹配用户 ID。后台最常见的一次查询就是「这个用户是不是会员」，而客服手上只有 ID。
	UserID string
	// active / frozen / expired / revoked，空表示不筛。
	Status string
	// 按套餐编码筛（成交快照上的 code，不是现查套餐）。
	PlanCode string
	// 到期时间的闭开区间 [ExpireFrom, ExpireTo]。
	//
	// 指针是因为「没筛这一端」与「筛到零值时刻」是两件事。运营用它做两件具体的活：
	// 「这周到期的都谁」（筛 From/To 两端）与「今天之后就没了的」（只筛 From）。
	ExpireFrom *time.Time
	ExpireTo   *time.Time
	// 只看开着自动续费的（true）或只看没开的（false）。三态，nil 不筛。
	//
	// 它是「到期前该扣款的那批」在后台的入口：客服接到「为什么我这个月没扣款」时，
	// 先看这个人到底开没开自动续费。
	AutoRenew *bool
	Page      int
	PageSize  int
}

// SubscriptionQuery 是后台连续包月订阅列表的筛选条件。
//
// 只有两个字段，因为这两个页面上真正会被问到的就是这两件事：「这个人签没签」与「这批签约
// 现在什么状态」。老系统的筛选面板也是这两栏。
type SubscriptionQuery struct {
	// 精确匹配用户 ID。客服接到「为什么我这个月没扣款」，手上只有这个。
	UserID string
	// pending_sign / active / suspended / cancelled / expired。
	//
	// **空表示不筛，而「不筛」是不等于 pending_sign**（不是不过滤）——见仓储里那段说明：
	// 一屏等着用户去微信点完的单子对运营没有任何用处。
	Status   string
	Page     int
	PageSize int
}
