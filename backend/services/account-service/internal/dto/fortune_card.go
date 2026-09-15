package dto

import "time"

// MaxPageSize 是福卡各列表接口 pageSize 的上限，取值与 platform/api.MaxPageSize 一致。
//
// 与 coupon-service 的 dto.MaxPageSize 同一个理由：前端「要全集」的统一参数
// （admin-web 的 FULL_PAGE_PARAMS，pageSize=200）在更小的上限上会直接 400，然后把
// 一次翻译静默变成一列 ID。HTTP 层交给 api.ParsePage 校验，服务层与仓储层的兜底
// 也用它，避免漂移成三个数字。
const MaxPageSize = 200

// EntryQuery 是福卡流水查询的过滤条件。零值全部表示「不限」。
//
// From/To 是闭开区间 [From, To)：同一条流水只会落在相邻两天的其中一边，导出与对账
// 拼接时不会漏也不会重。指针而不是零值时间，是为了区分「没传」与「传了零时刻」。
type EntryQuery struct {
	UserID    string
	OrderNo   string
	EntryType string
	From      *time.Time
	To        *time.Time
	Page      int
	PageSize  int
}

// EntryResponse 是流水对外的一行。
//
// 字段名与 admin-web / 小程序读的是同一套（json tag 就是契约）：明细页要显示的
// 「变动后余额」是 BalanceAfter，不是当前余额——冲正重放时，只有它能回答「当时是多少」。
type EntryResponse struct {
	ID              string    `json:"id"`
	UserID          string    `json:"userId"`
	EntryType       string    `json:"entryType"`
	Amount          int64     `json:"amount"`
	BalanceAfter    int64     `json:"balanceAfter"`
	Title           string    `json:"title"`
	ReferenceType   string    `json:"referenceType"`
	ReferenceID     string    `json:"referenceId"`
	ReferenceNo     string    `json:"referenceNo"`
	ReversesEntryID string    `json:"reversesEntryId"`
	Remark          string    `json:"remark"`
	OccurredAt      time.Time `json:"occurredAt"`
	CreatedAt       time.Time `json:"createdAt"`
}

// AccountResponse 是一个用户的福卡账户。
//
// 没有账户行时余额是 0 而不是 404：账户行是第一次发放时才懒创建的，一个从没收到过
// 福卡的用户查自己的余额，答案是「0 张」这件事本身，不是「查不到这个人」。
//
// 三个余额一起给：客服最常问的是「我明明有 2 张，为什么抽奖说不够」——答案在
// FrozenBalance 上（有一张退款申请正冻着），只看 Balance 会看不出这句话哪里错了。
// AvailableBalance 是 Balance - FrozenBalance，算好了给，免得每个消费方各自减一次。
type AccountResponse struct {
	UserID           string    `json:"userId"`
	Balance          int64     `json:"balance"`
	FrozenBalance    int64     `json:"frozenBalance"`
	AvailableBalance int64     `json:"availableBalance"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	// HasAccount 区分「有过账户、余额为 0」与「从来没有过账户」。客服看到
	// 「0 张」时会想知道是哪一种；两者在流水页上差别很大（一个有历史，一个没有）。
	HasAccount bool `json:"hasAccount"`
}

// FreezeQuery 是冻结核对的过滤条件。零值全部表示「不限」。
type FreezeQuery struct {
	UserID   string
	OrderNo  string
	Status   string
	Page     int
	PageSize int
}

// FreezeResponse 是冻结对外的一行。
//
// EntryKeys 一起给出来：客服判断「冻的是哪张卡」要看它（base 是订单基础赠送、
// bonus:{campaignId} 是加购加赠），而按退款范围冻的语义全在这几个键里。
type FreezeResponse struct {
	ID          string     `json:"id"`
	UserID      string     `json:"userId"`
	AfterSaleNo string     `json:"afterSaleNo"`
	OrderID     string     `json:"orderId"`
	OrderNo     string     `json:"orderNo"`
	EntryKeys   []string   `json:"entryKeys"`
	Amount      int64      `json:"amount"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	OccurredAt  time.Time  `json:"occurredAt"`
	ReleasedAt  *time.Time `json:"releasedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// MiniappCardsResponse 是小程序端一次拿全的回答：余额 + 一页流水。
//
// 合成一个响应是因为明细页永远两个都要——分成两个接口只会让那一次渲染发两个请求，
// 还得处理「余额是新的、流水是旧的」这种半新半旧的状态。
type MiniappCardsResponse struct {
	Balance int64 `json:"balance"`
	// FrozenBalance / AvailableBalance 见 AccountResponse 的注释。小程序端页面上要写的是
	// 「可用 1 张（1 张因退款申请冻结中）」，而不是一个对不上账的余额。
	FrozenBalance    int64            `json:"frozenBalance"`
	AvailableBalance int64            `json:"availableBalance"`
	Entries          []*EntryResponse `json:"entries"`
	Total            int              `json:"total"`
	Page             int              `json:"page"`
	PageSize         int              `json:"pageSize"`
}
