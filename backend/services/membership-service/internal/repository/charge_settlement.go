package repository

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 本文件是「钱收了、账没落成」那张待办表（membership_charge_settlements）的读写。
//
// # 它为什么存在
//
// 扣款成功那条链上的第一步是在订单域建一张续费单，第二步是把会员续一期（顺序反不得，见
// service/renewal_order.go）。第一步失败时**不能靠平台重投**：那 5 次重投是毫秒级的、之后进
// panda.events.dlq，而那个队列今天没有消费者也没有监控（见 migrations/membership）。
// 所以失败的那一笔要被本服务自己记下来，由 worker 慢慢重试。
//
// # 它是一张工作队列
//
// 所以这一组方法里**只有读、写、删，没有查历史**：行在 = 还没落成，行没了 = 落成了。谁想知道
// 「上个月补过哪几笔」要去翻日志，事实在 membership_changes 与订单域那两张表里。
//
// # 三个写方法各自的身份
//
//	ParkChargeSettlement        事件消费失败时落一行（同一笔钱只落一行）
//	ClaimDueChargeSettlements   worker 取一批到点的，并把它们的 next_attempt_at 往后推（认领租约）
//	RescheduleChargeSettlement  这一次没成，推后再来，记下为什么
//	DeleteChargeSettlement      落成了，删掉

// chargeSettlementColumns 是待办表的列清单，一处维护（占位符顺序与 scanChargeSettlement 一致）。
const chargeSettlementColumns = `provider_transaction_id, agreement_id, user_id, target, biz_period,
	amount, occurred_at, trace_id, attempts, next_attempt_at, last_error, created_at`

// DefaultSettlementBatch 是待办重试一轮最多处理几行。
//
// 比到期扫描（200）小、比代扣扫描（50）大：每一行的代价是一次 gRPC 加一次本库事务（不像代扣
// 那一行还挂着一次渠道出网），而正常情况下这张表是**空的**——积压只在订单域刚恢复的那一阵子
// 出现，那时候宁可一轮多做一些。
const DefaultSettlementBatch = 100

// settlementLease 是一行被认领之后多久才会再次被别人看到。
//
// 它必须**明显长于** worker 的扫描周期（见 worker.DefaultSettlementInterval，一分钟）：短了的话
// 同一行会在同一轮里被反复领走，而每一次都是一次 gRPC。长了也没关系——worker 跑完这一行会立刻
// 把它删掉或者按退避推后，租期只在「worker 崩在半路上」时才有机会到期。
const settlementLease = 5 * time.Minute

// ParkChargeSettlement 把一笔「钱收了、账没落成」的扣款落成一条待办。
//
// # 入参就是一次没能执行的结算
//
// 类型是 ChargeSettleParams（见 charge.go）而不是另造一个结构体：这一行存的就是**一次还没能
// 执行的结算**，只差 OrderID 那一格——那一格恰恰是失败的那一步要产出的东西，所以它不在表上，
// 由重试时建单拿到。剩下的字段与表上的列一一对应。
//
// # 同一笔钱只落一行
//
// 冲突键就是主键（渠道流水号），撞上时 **DO NOTHING**：两条事件说的是同一笔钱（投递重放、或者
// 支付侧补发），两份事实一模一样，第二份不该把第一份的 attempts 清零重来。这与结算那一侧的
// 幂等是同一个身份——那一边靠 membership_changes.request_id 上那条唯一索引，这里靠主键。
func (r *PostgresRepository) ParkChargeSettlement(ctx context.Context, p ChargeSettleParams) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO membership_charge_settlements (
		provider_transaction_id, agreement_id, user_id, target, biz_period, amount, occurred_at, trace_id
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (provider_transaction_id) DO NOTHING`,
		trimOrEmpty(p.ProviderTransactionID), trimOrEmpty(p.AgreementID), trimOrEmpty(p.UserID),
		trimOrEmpty(p.Target), trimOrEmpty(p.BizPeriod), p.Amount, utc(p.OccurredAt),
		trimOrEmpty(p.TraceID))
	return mapPGError(err)
}

// ClaimDueChargeSettlements 取一批到点的待办，并**顺手把它们推后一个租期**。
//
// 认领即推后：这一行被取走之后，下一个副本（或者同一轮的下一次调用）在租期内看不到它，不需要
// lease_owner 那三列（理由见 migrations/membership）。真正的保护也不在锁上——重试的两步
// 都是幂等的，多跑一遍的后果只是一次多余的 gRPC，见 service.SettlePendingCharges。
//
// limit <= 0 时取 DefaultSettlementBatch（一个 0 会变成 LIMIT 0，表现是「worker 一直在跑、
// 一条也不处理」）。
//
// 时刻用库的 NOW() 而不是 Go 那边传进来的：认领是**多个副本之间**的协调，只有库的时钟是共同的
// 那一只。这与 ClaimPending（platform/messaging）同一个写法。
func (r *PostgresRepository) ClaimDueChargeSettlements(ctx context.Context, limit int) ([]*model.ChargeSettlement, error) {
	if limit <= 0 {
		limit = DefaultSettlementBatch
	}
	rows, err := r.pool.Query(ctx, `WITH claimed AS (
		SELECT provider_transaction_id FROM membership_charge_settlements
		WHERE next_attempt_at <= NOW()
		ORDER BY next_attempt_at, provider_transaction_id
		LIMIT $1 FOR UPDATE SKIP LOCKED
	)
	UPDATE membership_charge_settlements s
	SET next_attempt_at = NOW() + make_interval(secs => $2), attempts = s.attempts + 1
	FROM claimed WHERE s.provider_transaction_id = claimed.provider_transaction_id
	RETURNING `+prefixColumns(chargeSettlementColumns, "s"), limit, settlementLease.Seconds())
	if err != nil {
		return nil, mapPGError(err)
	}
	defer rows.Close()

	due := make([]*model.ChargeSettlement, 0, limit)
	for rows.Next() {
		settlement, err := scanChargeSettlement(rows)
		if err != nil {
			return nil, mapPGError(err)
		}
		due = append(due, settlement)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError(err)
	}
	return due, nil
}

// DeleteChargeSettlement 删掉一条落成了的待办。
//
// 它**不检查这一点有没有落成**（不做那一次读）：删错了的后果是「这一笔不再重试」，而调用方
// （service.SettlePendingCharges）只在结算那一步明确答过话之后才调它。加一次读只会在「读到了、
// 但别处刚删掉」这种时候多一条要处理的错误。
func (r *PostgresRepository) DeleteChargeSettlement(ctx context.Context, providerTransactionID string) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM membership_charge_settlements WHERE provider_transaction_id = $1`,
		trimOrEmpty(providerTransactionID))
	return mapPGError(err)
}

// RescheduleChargeSettlement 记下这次为什么没成，并把下一次的时间推到 nextAttemptAt。
//
// nextAttemptAt 由调用方（worker）按 attempts 算好——退避是**调度策略**，不是数据访问：放在这里
// 的话，这个包就得知道「一秒起步、封顶一小时」那条规则，而它是 worker 的。
//
// lastError **截断到一列装得下的长度**：它可能是任何错误（含 gRPC 的应答体），而不截断的后果是
// 一条越来越长的日志字段被反复重写。截断在写入端而不是读取端：读的人今天只看到一行。
func (r *PostgresRepository) RescheduleChargeSettlement(ctx context.Context, providerTransactionID string, nextAttemptAt time.Time, lastError string) error {
	_, err := r.pool.Exec(ctx, `UPDATE membership_charge_settlements
		SET next_attempt_at = $2, last_error = $3
		WHERE provider_transaction_id = $1`,
		trimOrEmpty(providerTransactionID), utc(nextAttemptAt), truncate(lastError, maxSettlementErrorLength))
	return mapPGError(err)
}

// maxSettlementErrorLength 是 last_error 的截断长度。
//
// 与 outbox 的 maxOutboxErrorSummary（1024）同一个数量级：那一列也是只给人看的失败摘要。
const maxSettlementErrorLength = 1024

// truncate 按**字节**截断（列是 TEXT，截在半个多字节字符上会被 PG 拒掉，所以退到最后一个完整的
// 字符边界）。只用于 last_error 这一类给人看的调试字段。
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// scanChargeSettlement 读回一行待办。列的清单与顺序见 chargeSettlementColumns。
func scanChargeSettlement(row scanner) (*model.ChargeSettlement, error) {
	var settlement model.ChargeSettlement
	if err := row.Scan(&settlement.ProviderTransactionID, &settlement.AgreementID, &settlement.UserID,
		&settlement.Target, &settlement.BizPeriod, &settlement.Amount, &settlement.OccurredAt,
		&settlement.TraceID, &settlement.Attempts, &settlement.NextAttemptAt,
		&settlement.LastError, &settlement.CreatedAt); err != nil {
		return nil, err
	}
	return &settlement, nil
}
