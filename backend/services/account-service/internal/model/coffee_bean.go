package model

import "time"

// 咖啡豆账变类型。与 coffee_bean_entries.entry_type 的 CHECK 一字不差。
//
// 三个名字都带 Bean 前缀，不为了好看：福卡那边已经有一组 EntryTypeGrant / Draw / Reverse，
// 而两张表的词表并不相同（grant / draw / reverse vs adjust / consume / reverse）。前缀让
// 「把福卡的类型写进豆的流水」在编译期就报错，而不是等库里那条 CHECK 在运行期炸出来。
const (
	// BeanEntryTypeAdjust 是后台人工调整：充值或纠错。**一个词管两个方向**——列本身带符号，
	// 充值为正、纠错为负。分成 recharge / refund 两个词会让人以为 recharge 只能为正，
	// 而「把充错的豆调回来」正是要往负的方向走。
	BeanEntryTypeAdjust = "adjust"
	// BeanEntryTypeConsume 是纯豆出资的扣减：一张订单全额用豆支付，落流水时是负数。
	BeanEntryTypeConsume = "consume"
	// BeanEntryTypeReverse 是冲正：退款成功，新增一条反向记录，不原地改数。
	BeanEntryTypeReverse = "reverse"
)

// 咖啡豆账变起因的对象类型，对应 reference_type。与 migrations/account/006 的列注释
// 是同一套说法：本列记的是「为什么余额变了」，与事件日志（message_outbox）不是一回事。
const (
	// BeanReferenceTypeManual 是后台人工调整。
	BeanReferenceTypeManual = "manual"
	// BeanReferenceTypeOrder 是纯豆出资的扣减。与福卡的 ReferenceTypeOrder 同值，但那时
	// 它指的是「这一笔发放由哪张订单送的」，这里指的是「这一笔扣减为哪张订单付的钱」。
	BeanReferenceTypeOrder = "order"
	// BeanReferenceTypeAfterSale 是退款冲正。起因是那张**售后单**而不是订单——同一张订单
	// 可能先后有多次售后，冲正是哪一次退的只能由售后单回答。
	BeanReferenceTypeAfterSale = "after_sale"
)

// CoffeeBeanAccount 对应 coffee_bean_accounts：一个用户一行，第一次调整或第一次扣减时懒创建。
//
// ⚠️ 它是**用户维度**的咖啡豆账户，键是 user_id。另有一个名字很像的东西——
// panda_coffee_machine.devices.coffee_balance（配 device_balance_ledger）——那是**设备
// 维度**的预付余额，按 device_id 记账、没有 user_id，老系统里是「每次打咖啡扣机器余额」。
// 两者没有任何关系、不共用代码、也不共用表名，只是中文都叫「咖啡余额」。
//
// 没有 FrozenBalance：豆在支付的那一刻就已经从余额里扣走了，退款窗口里没有可保护的东西，
// 所以这一半没有冻结、没有解冻，只有一个方向——把钱还回去（见 BeanEntryTypeReverse）。
// 也没有过期列：豆永不过期，不加 expires_at、不加到期 worker。
type CoffeeBeanAccount struct {
	UserID string `db:"user_id"`
	// Balance 单位为分，与 orders.payable_amount 同单位。是 coffee_bean_entries 里该用户
	// 全部 amount 之和，两者在同一次事务里更新；存下来是为了只读查询与「余额不会被扣穿」
	// 这条 CHECK——只有一列能被约束。
	Balance   int64     `db:"balance"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// CoffeeBeanEntry 对应 coffee_bean_entries，只允许追加。
//
// Amount **有符号**，单位为分：充值为正、纠错为负、扣减为负、冲正与它冲掉的那笔相反。
// BalanceAfter 是变动后余额，明细页显示的就是它；冲正重放时也只有它能回答「当时是多少」。
//
// OperatorID 只对 entry_type=adjust 有值：后台调整是谁动的，这是审计之外的**第二处留痕**
// （与 device_balance_ledger 的 operator_id 同形——两个服务的后台写入者是同一类东西）。
//
// OperatorName 今天**恒为空串**，与 device_balance_ledger.operator_name 一样：令牌里没有
// 用户名（见 platform/audit 的 Entry 说明），展示名要由读侧按 ID 去身份库解析。列留着是
// 为了形状与设备那边的流水一致，而不是假装这里已经有人名。
type CoffeeBeanEntry struct {
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
	OperatorID      string    `db:"operator_id"`
	OperatorName    string    `db:"operator_name"`
	Remark          string    `db:"remark"`
	OccurredAt      time.Time `db:"occurred_at"`
	CreatedAt       time.Time `db:"created_at"`
}

// BeanAdjustKey / BeanConsumeKey / BeanReverseKey 是咖啡豆流水 entry_key 的三种形状，
// 与 migrations/account/005、006 的列注释是同一套说法。
//
// 它们必须全局唯一（那把唯一索引是幂等性的全部依据），所以进键的都是 UUID 而不是序号：
// 重发一次后台调整、重试一次扣减、重投一条 order.after_sale.refunded，都撞在同一个键上，
// 回放原样而不是新增第二行。
//
// 这几个形状刻意不在库里做约束：「键长什么样」是服务侧的规则，改它不该要一次迁移。
func BeanAdjustKey(requestID string) string { return "adjust:" + requestID }

// BeanConsumeKey 用**订单 ID 而不是支付单号**。
//
// 冲正要从 order.after_sale.refunded 反查「这单扣了多少豆」，而那条事件带的是 orderId；
// 用订单做键，账户域自己就能查到那笔扣减，不必让订单域把账变 ID 塞进事件载荷。
// payments_one_succeeded_per_order 保证一张订单只会有一条成功的扣减，所以订单做键不会撞。
func BeanConsumeKey(orderID string) string { return "order:" + orderID }

// BeanReverseKey 用**售后单号**，而不是像福卡的 ReverseKey 那样用被冲正那笔的 ID。
//
// 差别来自金额：福卡的冲正是「原样反向」，一笔流水只能被冲一次，所以从流水 ID 派生键就够；
// 豆的冲正允许只冲一部分（先退加购行、再退整单），同一笔扣减会被冲多次，得由「哪一次售后」
// 来区分。一张售后单只能冲一次，这条唯一索引就是这句话的实现。
func BeanReverseKey(afterSaleNo string) string { return "after_sale:" + afterSaleNo }
