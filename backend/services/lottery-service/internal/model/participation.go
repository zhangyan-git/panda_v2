package model

import "time"

// Participation 对应 lottery_participations 表：一次参与一行。
//
// 「同一活动可多次参与」是原型的明确文案，所以**没有** (round_id, user_id) 唯一约束
// ——幂等键不同就是不同的参与。
//
// 这张行的主键 ID 有一个额外身份：它**就是交给 account-service 的 request_id**。
// 账户域按 `draw:{requestId}` 派生 entry_key，所以「同一笔参与重试多少次都只扣一张卡」
// 挂在这个 ID 的唯一性上，而不是靠调用方自觉（见 service/participate.go 的三段事务）。
type Participation struct {
	ID string `db:"id"`
	// 参与时锁住的那一期。与开奖互斥（开奖对同一行 FOR UPDATE）。
	RoundID string `db:"round_id"`
	// 冗余一份活动，为了「我的参与」少一次 join。
	CampaignID   string `db:"campaign_id"`
	CampaignName string `db:"campaign_name"`
	RoundNo      string `db:"round_no"`
	// 小程序用户 ID，属于身份服务，仅作跨库值引用。
	UserID string `db:"user_id"`
	// 这一笔参与是从哪张订单的福卡来的（原型参与详情里的「来源订单」）。为空 =
	// 「直接参与」。同样是值引用，不建外键。
	SourceOrderID *string `db:"source_order_id"`
	// SourceOrderNo 是给后台看的订单号快照。**目前没有任何写入方，这一列一直是空的**：
	// 请求体里只有 sourceOrderId，订单号在 order-service 那边，而本服务没有对它的出向依赖
	// （contracts/proto/order/v1 只有 CreateDeviceOrder，没有按 id 读单的 RPC）。要填它得先
	// 有一个数据源——给请求体加一个字段（它会变成小程序能随便填的展示文本），或者加一次同步
	// 查询（本服务第一条对订单域的出向依赖）。在那之前它保持为空，而不是编一个看起来像订单
	// 号的字符串。
	SourceOrderNo string `db:"source_order_no"`
	// 参与当时这台设备 / 这个点位的快照，用于中奖记录里的「来源点位」。
	SourceMachineID  *string `db:"source_machine_id"`
	SourceLocationID *string `db:"source_location_id"`
	// 这一笔扣了几张福卡。本轮恒为 1——「一次参与一张」是原型规则，但列留着
	// （CHECK cost > 0），将来「连抽」是改这一列的值，不是改表。
	Cost int32 `db:"cost"`
	// pending / confirmed / failed / reversed，见下面的常量。
	Status      string `db:"status"`
	FailureCode string `db:"failure_code"`
	// 账户服务回的账变流水 ID（值引用），对账时从这一笔参与反查那次扣卡。
	FortuneEntryID *string `db:"fortune_entry_id"`
	// 补偿时冲正那一笔的账变流水 ID。与上面的区别是：那个是「扣」，这个是「还回去」。
	ReverseEntryID *string `db:"reverse_entry_id"`
	// 修复 worker 用：看不见的失败次数与最后一次错。**都不是状态**，只是观测——
	// 判 failed 要有确定的依据，传输层一直失败的行是「真的不知道扣没扣」。
	Attempts  int32   `db:"attempts"`
	LastError *string `db:"last_error"`
	// 幂等键，由**服务端**派生，不给客户端自由发挥：
	//   order:{orderId}           从某张订单参与——「一笔订单只能参与一次」落在这个键上
	//   {Idempotency-Key 请求头}   从抽奖中心直接参与
	IdempotencyKey string     `db:"idempotency_key"`
	CreatedAt      time.Time  `db:"created_at"`
	ConfirmedAt    *time.Time `db:"confirmed_at"`
}

// 参与状态，与 lottery_participations.status 的 CHECK 逐字一致。
const (
	// ParticipationPending 是**悬而未决**：本地已经记下意图，但还不知道账户域扣没扣成功。
	//
	// 它不等于失败。传输层超时的时候我们真的不知道对面做没做，把这种行判成 failed 就是
	// 撒谎——用户可能已经被扣了卡却什么也得不到。修复 worker 用同一个 request_id 重跑
	// 扣减（幂等，不会二次扣），拿到确定结论才推进。
	ParticipationPending = "pending"
	// ParticipationConfirmed 是终态：卡确实扣掉了，这一笔计入期次。
	ParticipationConfirmed = "confirmed"
	// ParticipationFailed 是终态：**有确定依据**的失败（余额不足、请求非法、期次已关）。
	ParticipationFailed = "failed"
	// ParticipationReversed 是终态：扣成功之后又被冲正还回去了（补偿路径）。
	ParticipationReversed = "reversed"
)

// 参与失败的机器可读原因，落在 lottery_participations.failure_code 上。
//
// 前端按它决定提示哪一句话，所以这几个字符串是**对 C 端的契约**，不是内部备注。
const (
	// FailureInsufficientFortuneCards：福卡不够。用户该做的就是去下一单。
	FailureInsufficientFortuneCards = "insufficient_fortune_cards"
	// FailureRoundClosed：扣卡飞行途中期次关了或开了奖，卡已经退回。
	FailureRoundClosed = "round_closed"
	// FailureInvalidRequest：账户域说这个请求本身不合法。这是 bug 不是用户错，
	// 所以它同时配一条 ERROR 级日志。
	FailureInvalidRequest = "invalid_request"
	// FailureRoundNotOpen：入口就发现期次已经不再收人（收满转 closed、已开奖、已作废），
	// 一次扣卡都没发生。
	FailureRoundNotOpen = "round_not_open"
)

// ParticipationKeyFromOrder 是「从某张订单参与」的幂等键。
//
// 键取自**订单**而不是取自请求：同一张订单送出的福卡只能参与一次，这个不变量就落在
// lottery_participations_key_unique 上——不靠应用判断。与 account-service 用
// `order:{orderId}` 做豆扣减的幂等键是同一条理由。
func ParticipationKeyFromOrder(orderID string) string { return "order:" + orderID }
