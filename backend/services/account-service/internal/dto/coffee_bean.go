package dto

import "time"

// 咖啡豆这一半的对外形状。与 fortune_card.go 那份刻意保持同形（流水一行一个结构体、
// 账户一个、查询条件一个），差别来自口径：没有冻结，所以账户上没有 frozen/available 两项；
// 金额是**分**（int64），所以到处是「单位分」这句话要写。

// BeanEntryQuery 是咖啡豆流水查询的过滤条件。零值全部表示「不限」。
//
// 过滤列叫 ReferenceNo 而不是 EntryQuery 里的 OrderNo：豆流水的 reference_no 有三种
// 形状（调整的幂等号、订单号、售后单号），用订单命名会让「按调整号查」变成一件说不出口
// 的事。From/To 是闭开区间 [From, To)，理由与福卡那份逐字相同。
type BeanEntryQuery struct {
	UserID      string
	ReferenceNo string
	EntryType   string
	From        *time.Time
	To          *time.Time
	Page        int
	PageSize    int
}

// BeanAccountResponse 是一个用户的咖啡豆账户。
//
// 没有账户行时余额是 0 而不是 404：账户行是第一次调整或第一次扣减时才懒创建的，一个从没
// 充过豆的用户问自己的余额，答案是「0 分」这件事本身，不是「查不到这个人」。
//
// 没有 frozenBalance / availableBalance 两项（福卡那边有）：豆在支付的那一刻就已经扣走，
// 不存在「冻着但还在」的钱，可用就是余额。补上那两项等于凭空造出一个恒等于余额的数字。
type BeanAccountResponse struct {
	UserID  string `json:"userId"`
	Balance int64  `json:"balance"` // 单位为分
	// HasAccount 区分「有过账户、余额为 0」与「从来没有过账户」——客服看到 0 分时会想知道
	// 是哪一种，两者在流水页上差别很大（一个有历史，一个没有）。
	HasAccount bool      `json:"hasAccount"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// BeanEntryResponse 是咖啡豆流水对外的一行。
//
// 字段名与 admin-web / 小程序读的是同一套（json tag 就是契约）。BalanceAfter 是「变动后
// 余额」而不是当前余额：冲正重放时，只有它能回答「当时是多少」。
type BeanEntryResponse struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	EntryType string `json:"entryType"`
	// Amount 单位为分，**带符号**：充值为正、纠错为负、扣减为负、冲正与它冲掉的那笔相反。
	Amount        int64  `json:"amount"`
	BalanceAfter  int64  `json:"balanceAfter"` // 单位为分
	Title         string `json:"title"`
	ReferenceType string `json:"referenceType"`
	ReferenceID   string `json:"referenceId"`
	ReferenceNo   string `json:"referenceNo"`
	// ReversesEntryID 只有冲正那一条有值：它指向被冲回的那笔扣减。后台客服判断
	// 「这 900 分是退哪一笔的」看的就是它。
	ReversesEntryID string `json:"reversesEntryId"`
	// OperatorID 只有后台调整有值。展示名不在这里：令牌里没有用户名，要按 ID 去身份库解析
	// （见 platform/audit 的 Entry 说明），所以这一层不返回一个恒为空串的 operatorName。
	OperatorID string    `json:"operatorId"`
	Remark     string    `json:"remark"`
	OccurredAt time.Time `json:"occurredAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

// BeanAdjustInput 是后台「余额调整」的请求体。
//
// Amount **带符号**（单位为分）：充值为正、把充错的豆调回来为负。没有单独的「扣回」接口，
// 一个带符号的金额就够了——分成两个接口会让「-100 走哪一个」变成一个新问题，而答案取决于
// 那个接口当时的措辞。
//
// RequestID 是幂等号，由后台每次打开弹窗时生成一个新的：网络抖一下再点一次不会充两次。
// 但同一个 requestId 再来一次**不是**静默回放，而是 409（见 repository.ErrBeanDuplicateRequest）
// ——点这一次的是人，回一句「成功了」会让它再点一次，于是真的充两次。后台拿到 409 应当重新
// 拉余额、先对流水，再决定换不换号重发。
type BeanAdjustInput struct {
	Amount    int64  `json:"amount"`
	RequestID string `json:"requestId"`
	// Remark 是操作人填的理由。它会落进流水的 remark，也是它唯一的去处：
	// admin_operation_logs 没有 reason 列（见 platform/audit 的调用处）。
	Remark string `json:"remark"`
}

// BeanAdjustResponse 是调整之后立刻回给后台的那两个数。
//
// 余额一起给，前端就能把页面上的数字直接改对，不必再发一次读——而且**这个**余额是刚写完
// 那一笔之后的数，重新读一次拿到的可能是别人又动过的余额，与它刚提交的那次调整对不上。
type BeanAdjustResponse struct {
	Balance int64  `json:"balance"` // 单位为分
	EntryID string `json:"entryId"`
}

// MiniappBeansResponse 是小程序端一次拿全的回答：余额 + 一页流水。
//
// 合成一个响应是因为明细页永远两个都要——分成两个接口只会让那一次渲染发两个请求，还得处理
// 「余额是新的、流水是旧的」这种半新半旧的状态。
type MiniappBeansResponse struct {
	Balance int64 `json:"balance"` // 单位为分
	// Entries 只是第一页；Total/Page/PageSize 一起给，滚动加载时不用再问一次「还有没有」。
	Entries  []*BeanEntryResponse `json:"entries"`
	Total    int                  `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"pageSize"`
}
