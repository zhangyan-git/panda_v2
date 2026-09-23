package dto

import "time"

// 分账后台的请求与响应形状。
//
// 它是三张页面（分账规则 / 分账账户 / 分账明细）与 admin-web 的 services/settlement.ts 之间的
// 契约：json tag 那一边是逐字照抄的，改一个 tag 要三处同批改（tag、前端字段、前端标签表）。
//
// # 金额与比例的单位
//
// 金额一律 **int64 分**，与全仓 HTTP 层一致（方案里写过「金额用字符串」，那条没落地，沿用现状）。
//
// 比例用 **ratioPercent**：一个百分数，最多两位小数（45 表示 45.00%）。不直接传 0~1 的比值，
// 是因为页面上的输入框写的就是百分数，而「0.45 到底是 45% 还是 0.45%」这种错在多一个零的
// 时候不会有任何人发现。落库时换算成 ratio = 值 / 100（列是 NUMERIC(20,6)）。
//
// # 为什么规则项不单独开路由
//
// 「同一条规则下比例合计不超过 1、平台项至多一条」是**跨行**的约束：拆成子资源逐行改，就没有
// 任何一个时刻能在一个事务里看见完整的一套项，也就校验不了。所以规则带 items 整体提交，
// 详情与更新都按整体处理。

// ============================================================
// 规则
// ============================================================

// SettlementRuleQuery 是规则列表的筛选条件。
type SettlementRuleQuery struct {
	// Name 模糊匹配，空表示不筛。
	Name string
	// BizType / ScopeType / Status 先过词表白名单再进 SQL，空表示不筛。理由与支付单那条一样：
	// 打错一个字母的筛选值不该静默返回空列表（见 controller 的说明）。
	BizType   string
	ScopeType string
	Status    string

	Page     int
	PageSize int
}

// SettlementRuleItem 是规则里的一项。
//
// AccountName / ReceiverID 是账户的展示列（主体名与渠道侧的接收方号），读的时候 JOIN 出来，
// 写的时候不收——写只认 accountId。
type SettlementRuleItem struct {
	ID           string  `json:"id"`
	PartyType    string  `json:"partyType"`
	CalcType     string  `json:"calcType"`
	RatioPercent float64 `json:"ratioPercent"`
	FixedAmount  int64   `json:"fixedAmount"`
	AccountID    string  `json:"accountId"`
	AccountName  string  `json:"accountName"`
	ReceiverID   string  `json:"receiverId"`
	SortOrder    int     `json:"sortOrder"`
	Remark       string  `json:"remark"`
}

// SettlementRule 是列表与详情共用的规则形状。列表里 items 是空数组（见 repository 的说明）。
type SettlementRule struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	BizType        string    `json:"bizType"`
	ScopeType      string    `json:"scopeType"`
	ScopeRef       string    `json:"scopeRef"`
	AllocationMode string    `json:"allocationMode"`
	Status         string    `json:"status"`
	Remark         string    `json:"remark"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`

	Items []SettlementRuleItem `json:"items"`
}

// SettlementRuleItemInput 是提交一项时的形状。
type SettlementRuleItemInput struct {
	PartyType    string  `json:"partyType"`
	CalcType     string  `json:"calcType"`
	RatioPercent float64 `json:"ratioPercent"`
	FixedAmount  int64   `json:"fixedAmount"`
	// AccountID 为空表示平台项。settlement_rule_items 的 CHECK 把「平台项没有账户」写死了，所以平台项只能空、
	// 非平台项必须有。
	AccountID string `json:"accountId"`
	SortOrder int    `json:"sortOrder"`
	Remark    string `json:"remark"`
}

// SettlementRuleInput 是创建与整体更新一条规则的请求体。
type SettlementRuleInput struct {
	Name           string `json:"name"`
	BizType        string `json:"bizType"`
	ScopeType      string `json:"scopeType"`
	ScopeRef       string `json:"scopeRef"`
	AllocationMode string `json:"allocationMode"`
	// Status 空表示 enabled：新建的规则默认参与命中，这是绝大多数情况；停用是一个明确动作
	// （把一条正在生效的规则关掉），不该是「忘了传」的默认值。
	Status string                    `json:"status"`
	Remark string                    `json:"remark"`
	Items  []SettlementRuleItemInput `json:"items"`
}

// ============================================================
// 账户
// ============================================================

// SettlementAccountQuery 是账户列表的筛选条件。
type SettlementAccountQuery struct {
	// Keyword 一个词搜两样（主体名 / 子商户号）。运营手上那串是什么，他自己多半也说不清，
	// 而两个框会让人每个都试一遍。账户表上已经没有账户号这一列了。
	Keyword   string
	PartyType string
	Provider  string
	Status    string

	Page     int
	PageSize int
}

// SettlementAccount 是账户在页面上的形状。
//
// **没有账户号与接收方名**（账户表上没有这两列）：主体名就是这一行给人看的名字，而它在规则项的
// 账户下拉、分账接收方快照与列表里都是同一个名字。渠道侧只需要一个号，那是 ReceiverID。
type SettlementAccount struct {
	ID           string    `json:"id"`
	PartyName    string    `json:"partyName"`
	PartyType    string    `json:"partyType"`
	Provider     string    `json:"provider"`
	ReceiverType string    `json:"receiverType"`
	ReceiverID   string    `json:"receiverId"`
	Status       string    `json:"status"`
	Remark       string    `json:"remark"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// SettlementAccountInput 是创建与更新一个账户的请求体。
type SettlementAccountInput struct {
	// PartyName 必填：它是这一行给人看的名字，账户表上没有账户号那列，没东西给它兜底。
	PartyName string `json:"partyName"`
	PartyType string `json:"partyType"`
	// Provider 是渠道名（catalog 里的那个值，如 `ums`）。**不是 uuid**：渠道与支付方式不在库里，
	// 它们是代码里的目录，没有可指向的那张表。
	Provider     string `json:"provider"`
	ReceiverType string `json:"receiverType"`
	ReceiverID   string `json:"receiverId"`
	Status       string `json:"status"`
	Remark       string `json:"remark"`
}

// SettlementChannel 是「钱能分到哪条渠道上」的取值，给账户表单的渠道下拉用。
//
// 它来自**代码里的目录**（catalog.Channel.Settlement），不是一张表：能分账的渠道由「哪条渠道的
// 下单报文里能带子单」决定，那是发版的事，不是运营的输入。
type SettlementChannel struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Provider 是账户上要存的那个值。
	Provider string `json:"provider"`
}

// ============================================================
// 任务（只读）
// ============================================================

// SettlementTaskQuery 是分账明细列表的筛选条件。
type SettlementTaskQuery struct {
	// TaskNo / PaymentNo / OrderNo 模糊匹配（单号只记得一截是常态，见 PaymentQuery 的同一条取舍）。
	TaskNo    string
	PaymentNo string
	// OrderNo 要 JOIN payments：分账任务上没有订单号。
	OrderNo   string
	Status    string
	ScopeType string
	// 三个主体维度各一个等值条件。老系统按这三个维度分了三个接口，这里一个接口三个参数——
	// 它们是同一张表上的同一件事，拆开之后「按门店和品牌一起筛」就没地方写了。
	StoreRef    string
	BrandRef    string
	MerchantRef string

	CreatedFrom *time.Time
	CreatedTo   *time.Time

	Page     int
	PageSize int
}

// SettlementTask 是分账明细列表与详情共用的任务形状。
//
// OrderNo / Provider / Method 来自 JOIN payments（见 repository 的 settlementTaskFrom）——
// 那些是支付单上的事实，任务表上再存一份就是同一件事的第二个写法。
type SettlementTask struct {
	ID        string `json:"id"`
	TaskNo    string `json:"taskNo"`
	PaymentNo string `json:"paymentNo"`
	OrderNo   string `json:"orderNo"`
	// BaseAmount 是分账基数（实付金额，券后），单位分；PlatformAmount 是平台自留，差额倒挤。
	// 恒等式：BaseAmount = PlatformAmount + Σ Receivers.Amount。
	BaseAmount     int64  `json:"baseAmount"`
	PlatformAmount int64  `json:"platformAmount"`
	Status         string `json:"status"`
	ScopeType      string `json:"scopeType"`
	ScopeRef       string `json:"scopeRef"`
	StoreRef       string `json:"storeRef"`
	BrandRef       string `json:"brandRef"`
	MerchantRef    string `json:"merchantRef"`
	// RuleID 为空是常态：门店没配规则时这条任务照样建，整单归平台。
	RuleID   string `json:"ruleId"`
	RuleName string `json:"ruleName"`
	// Provider / Method 是这笔支付走的渠道名与支付方式 code。
	Provider string `json:"provider"`
	Method   string `json:"method"`

	ProviderTaskNo        string `json:"providerTaskNo"`
	ProviderTransactionID string `json:"providerTransactionId"`
	Attempts              int    `json:"attempts"`
	LastError             string `json:"lastError"`

	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// SettlementReceiver 是一条接收方明细，**全部是快照列**。
//
// 它不回查账户补当前的名字与号：这张表的设计是把这几个值冻结在行上（当初实际发出去的那个号），
// 回头现查会让历史明细随着账户改名而变——而那条明细的意义正是「当时分给了谁」。
type SettlementReceiver struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	PartyType string `json:"partyType"`
	PartyName string `json:"partyName"`
	// 三个主体值引用是建任务那一刻的快照。
	MerchantRef  string `json:"merchantRef"`
	BrandRef     string `json:"brandRef"`
	StoreRef     string `json:"storeRef"`
	ReceiverType string `json:"receiverType"`
	ReceiverID   string `json:"receiverId"`
	// RatioPercent 是当初生效的比例快照（百分数）；固定额项记 0，它的金额在 amount 上。
	RatioPercent     float64   `json:"ratioPercent"`
	Amount           int64     `json:"amount"`
	ReversedAmount   int64     `json:"reversedAmount"`
	ProviderDetailNo string    `json:"providerDetailNo"`
	Status           string    `json:"status"`
	LastError        string    `json:"lastError"`
	CreatedAt        time.Time `json:"createdAt"`
}

// SettlementTaskDetail 是分账明细详情：任务本身加上它的接收方明细。
type SettlementTaskDetail struct {
	Task      SettlementTask       `json:"task"`
	Receivers []SettlementReceiver `json:"receivers"`
}
