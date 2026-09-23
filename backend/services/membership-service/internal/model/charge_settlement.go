package model

import "time"

// ChargeSettlement 对应 membership_charge_settlements 表：**一笔收了钱、而账还没落成的扣款**。
//
// 它不是业务数据，是一张工作队列（见 migrations/membership 里「扣款待办」那一节）。一行在，表示这笔渠道
// 流水的结论已经收到、而续费单与会员续期还没做成；行被删掉，表示这一期彻底落完了。
//
// # 为什么值得为它建一张表
//
// 因为这一格的失败**没有别的目击者**：渠道那边钱动了，而本域与订单域两边都没有痕迹。平台的 5 次
// 重投是毫秒级的，之后进那个没有人看的死信队列（见 migrations/membership 里「扣款待办」那一节）。所以「等一会儿再试」这
// 件事必须由本服务自己记住——记在库里，而不是记在内存或队列里：内存里的一次重启就没了，队列里
// 的正是那个已经失效的机制。
type ChargeSettlement struct {
	// ProviderTransactionID 是渠道流水号，既是主键也是结算的幂等键（见 migrations 里那段）。
	ProviderTransactionID string `db:"provider_transaction_id"`
	AgreementID           string `db:"agreement_id"`
	UserID                string `db:"user_id"`
	// Target 是这一期的结论（dto.ChargeStatus*）。今天只会是 succeeded——失败那一期不建单，
	// 也就没有待办可言。
	Target    string `db:"target"`
	BizPeriod string `db:"biz_period"`
	Amount    int64  `db:"amount"`
	// OccurredAt 是**事件到达的时刻**，不是重试的时刻：它是续期的起点候选之一，用重试时刻会让
	// 「这一期从哪天开始」随一次订单域抖动而漂。
	OccurredAt time.Time `db:"occurred_at"`
	TraceID    string    `db:"trace_id"`

	// Attempts 是试过几次（认领时加一），退避由 worker 按它算。
	Attempts      int       `db:"attempts"`
	NextAttemptAt time.Time `db:"next_attempt_at"`
	// LastError 是上一次为什么没成，只给人看（不参与任何判定，重试是无条件的）。
	LastError *string   `db:"last_error"`
	CreatedAt time.Time `db:"created_at"`
}
