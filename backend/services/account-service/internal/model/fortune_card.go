// Package model 是资产账户库的表映射。
//
// 两半都在这里（方案 5.6）：福卡的余额账户 + 只追加流水 + 退款冻结行，以及咖啡豆的余额账户
// + 只追加流水（payment 的 action=account 今天起会真的扣减它）。两半由同一个服务拥有、表形状
// 刻意保持同形，差别只在币种、词表和「豆没有冻结」——各自的文件头里都写着理由。
package model

import "time"

// 账变类型。与 fortune_card_entries.entry_type 的 CHECK 一字不差。
const (
	// EntryTypeGrant 是发放：订单完成赠送。
	EntryTypeGrant = "grant"
	// EntryTypeDraw 是扣减：参与抽奖。
	EntryTypeDraw = "draw"
	// EntryTypeReverse 是冲正：新增一条反向记录，不原地改数。
	EntryTypeReverse = "reverse"
)

// 发放的种类。订单域拆好了再发过来（它才拥有「承诺福卡快照」），本服务不解析那个快照、
// 也不持有发放规则。
const (
	// GrantKindBase 是每单基础的赠送。
	GrantKindBase = "base"
	// GrantKindBonus 是加购加赠活动（幸运杯套）额外赠送的那一份。
	GrantKindBonus = "bonus"
)

// 账变起因的对象类型，对应 reference_type。记的是「为什么余额变了」，
// 与事件日志（message_outbox）不是一回事。
const (
	ReferenceTypeOrder = "order"
	ReferenceTypeDraw  = "draw"
	ReferenceTypeEntry = "entry"
)

// 冻结状态，与 fortune_card_freezes.status 的 CHECK 一字不差。
const (
	// FreezeStatusFrozen 是冻结中：这一单有退款申请在跑，这些卡不能拿去抽奖。
	FreezeStatusFrozen = "frozen"
	// FreezeStatusReleased 是已解冻：申请被驳回、用户撤销、或者退款失败。三种情况下
	// 钱都没出去，冻着的卡凭什么锁着。
	FreezeStatusReleased = "released"
	// FreezeStatusRecovered 是已追回：退款成功，这一单送出去的卡已经从账上冲回。
	//
	// 它与 released 分开而不是合成一个「结束」：解冻与追回在账上是**相反**的两件事——
	// 解冻只是「这些卡又能抽了」，余额一分不动；追回是「这些卡收回去了」，余额真的少了。
	// 合成一个取值会让这一格在页面上同时指两件事，而这一格正是客服照着回答问题的词。
	FreezeStatusRecovered = "recovered"
)

// FortuneCardAccount 对应 fortune_card_accounts：一个用户一行，第一次发放时懒创建。
//
// Balance 是 fortune_card_entries 里该用户全部 amount 之和，两者在同一次事务里更新。
// 存下来是为了只读查询与「余额不会被扣穿」这条 CHECK——只有一列能被约束。
//
// FrozenBalance 是退款冻结中的张数。**可用张数 = Balance - FrozenBalance**，它不落库：
// 抽奖扣减看的是可用，而冻结不是账变——余额与流水都不动，动的只是「这些张还能不能用」。
type FortuneCardAccount struct {
	UserID        string    `db:"user_id"`
	Balance       int64     `db:"balance"`
	FrozenBalance int64     `db:"frozen_balance"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// Available 是可用张数：能拿去抽奖的那些。
func (a FortuneCardAccount) Available() int64 { return a.Balance - a.FrozenBalance }

// FortuneCardFreeze 对应 fortune_card_freezes：一张售后申请一行。
//
// 它不是账本（Status/Amount 会变），所以那张表上没有只追加触发器。Amount 允许为 0：
// 申请可能早于发放，或者这张卡已经被抽掉了——后者正是「追不回来」在余额上的样子。
//
// ReleasedAt 与 RecoveredAt 互斥：一条冻结行只会走到两者之一，走到哪个由 Status 说。
type FortuneCardFreeze struct {
	ID          string     `db:"id"`
	UserID      string     `db:"user_id"`
	AfterSaleNo string     `db:"after_sale_no"`
	OrderID     string     `db:"order_id"`
	OrderNo     string     `db:"order_no"`
	EntryKeys   []string   `db:"entry_keys"`
	Amount      int64      `db:"amount"`
	Status      string     `db:"status"`
	Reason      string     `db:"reason"`
	OccurredAt  time.Time  `db:"occurred_at"`
	ReleasedAt  *time.Time `db:"released_at"`
	RecoveredAt *time.Time `db:"recovered_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// FortuneCardEntry 对应 fortune_card_entries，只允许追加。
//
// Amount 有符号：发放为正，扣减为负，冲正与它冲掉的那笔相反。BalanceAfter 是变动后
// 余额，明细页显示的就是它。
type FortuneCardEntry struct {
	ID              string    `db:"id"`
	UserID          string    `db:"user_id"`
	EntryType       string    `db:"entry_type"`
	Amount          int64     `db:"amount"`
	BalanceAfter    int64     `db:"balance_after"`
	Title           string    `db:"title"`
	ReferenceType   string    `db:"reference_type"`
	ReferenceID     string    `db:"reference_id"`
	ReferenceNo     string    `db:"reference_no"`
	EntryKey        string    `db:"entry_key"`
	ReversesEntryID *string   `db:"reverses_entry_id"`
	Remark          string    `db:"remark"`
	OccurredAt      time.Time `db:"occurred_at"`
	CreatedAt       time.Time `db:"created_at"`
}

// BaseGrantKey / BonusGrantKey / DrawKey / ReverseKey 是 entry_key 的三种形状，
// 与 migrations/account/001 的列注释是同一套说法。
//
// 它们必须全局唯一（那把唯一索引是幂等性的全部依据），所以订单与抽奖的 ID 直接进键：
// 这两类 ID 是 UUID，不会在两个用户之间撞。重投一条 order.completed、重试一次扣减，
// 都撞在同一个键上，不会新增第二行。
//
// 这几个形状刻意不在库里做约束：「键长什么样」是服务侧的规则，改它不该要一次迁移。
func BaseGrantKey(orderID string) string { return "order:" + orderID + ":base" }

// BonusGrantKey 带上活动 ID：同一单可能命中多个加赠活动，一单一份。
func BonusGrantKey(orderID, campaignID string) string {
	return "order:" + orderID + ":bonus:" + campaignID
}

// DrawKey 用调用方给的幂等号（抽奖侧就是参与记录的 ID）。
func DrawKey(requestID string) string { return "draw:" + requestID }

// ReverseKey 从被冲正的那笔派生：一笔流水只能被冲正一次，不需要调用方再给一个号。
func ReverseKey(entryID string) string { return "reverse:" + entryID }
