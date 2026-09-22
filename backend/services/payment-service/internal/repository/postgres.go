// Package repository 是支付库的数据访问层。
//
// 按聚合拆文件：payment.go 是支付单的写路径（发起支付的三段事务），callback.go 是回调
// 落单的事务，payment_query.go 是读路径（选中支付方式、按单号反查）。跨表的业务事务由
// service 编排，repository 只负责「一个事务里把这几个事实写进去」。
//
// 这一份的 appendOutbox / recordTransition / begin|completeIdempotentOperation /
// mapPGError 与 order-service 的同名函数几乎逐行一致——幂等与 outbox 在这个仓库里只有
// 一种语义，换服务时不该重新理解一遍。**唯一的差别是表名前缀**。
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

var (
	// ErrIdempotencyInProgress：同一个幂等键的操作还在处理中。
	ErrIdempotencyInProgress = errors.New("idempotency operation is still processing")
	// ErrIdempotencyConflict：同一个幂等键换了请求体。前者用 409 让调用方等一下，
	// 后者用 409 告诉调用方「这个键你用过了」，不能混成一个错误。
	ErrIdempotencyConflict = errors.New("idempotency key request hash conflict")
	// ErrPaymentNotFound：支付单不存在。
	ErrPaymentNotFound = errors.New("payment not found")
	// ErrPaymentNotPending：支付单已经不在能推进的状态（成功过了、关掉了）。
	ErrPaymentNotPending = errors.New("payment is not awaiting a result")
	// ErrPaymentNotificationConflict：渠道回调用的通知号已经落过库。
	ErrPaymentNotificationConflict = errors.New("payment notification is already recorded")
	// ErrPaymentNotificationAmountMismatch：回调里的金额与支付单对不上。
	//
	// 与 order-service 的同名约定一致：**报错不 ack**。钱可能真收了，但金额对不上说明
	// 我们对这笔交易的理解是错的，假装成功才是丢钱。
	ErrPaymentNotificationAmountMismatch = errors.New("payment notification amount does not match the payment")
	// ErrPaymentAlreadySettled：渠道说成功了，但本地这单已经失败或关掉了。
	//
	// 这是**真事故**：钱收了、单关了。必须报错不 ack，进 DLQ 人工介入——退回渠道的成功应答
	// 会让这条线索永久消失。
	ErrPaymentAlreadySettled = errors.New("payment is already closed or failed but the provider reports success")
)

// IdempotencyRecoveryWindow 是一条 processing 记录还能被认为「有事务正在跑」的时长。
//
// 事务提交后这条记录就变成 succeeded/failed，所以一条比这个窗口还旧的 processing 行
// 只可能来自「进程在提交前死掉」。到那时它不代表任何活着的操作，可以回收重试；
// 在窗口内则必须先回 409，否则两个并发请求会各自执行一遍副作用。
const IdempotencyRecoveryWindow = 15 * time.Minute

// paymentColumns 是 payments 表的读取列。
//
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步，少了它 Scan 会报类型不匹配。
// 列顺序与 scanPayment 的扫描顺序严格一一对应，两边必须一起改。
const paymentColumns = `id::text, payment_no, legacy_id, order_no, user_id::text, amount,
	provider, payment_method, status, subject, attach,
	provider_transaction_id, failure_code, failure_message, request_id,
	expires_at, paid_at, closed_at, created_at, updated_at,
	account_entry_id::text, account_funded_at`

const fundingColumns = `id::text, payment_id::text, line_no, line_type, amount, status,
	provider_transaction_id, failure_code, account_entry_id::text,
	created_at, updated_at, succeeded_at, reversed_at`

const transitionColumns = `id::text, aggregate_type, aggregate_id::text, from_status,
	to_status, reason, request_id, actor_type, actor_id::text, metadata, created_at`

// PostgresRepository 是支付库的数据访问实现。
type PostgresRepository struct {
	pool     *pgxpool.Pool
	recorder audit.Recorder
}

// NewPostgresRepository 构造仓储。recorder 为 nil 时用 audit.Noop：调用点的审计语句
// 保持无条件执行，不留「忘了传 recorder 就没有审计」的分支。
func NewPostgresRepository(pool *pgxpool.Pool, recorder audit.Recorder) *PostgresRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &PostgresRepository{pool: pool, recorder: recorder}
}

type scanner interface{ Scan(...any) error }

// newEventID 是 outbox 事件的 ID。抽成函数是为了让「一条事件一个 ID」只有一处实现。
func newEventID() string { return uuid.NewString() }

func scanPayment(row scanner) (*model.Payment, error) {
	payment := &model.Payment{}
	err := row.Scan(&payment.ID, &payment.PaymentNo, &payment.LegacyID, &payment.OrderNo,
		&payment.UserID, &payment.Amount, &payment.Provider,
		&payment.PaymentMethod, &payment.Status, &payment.Subject, &payment.Attach,
		&payment.ProviderTransactionID, &payment.FailureCode, &payment.FailureMessage,
		&payment.RequestID, &payment.ExpiresAt, &payment.PaidAt, &payment.ClosedAt,
		&payment.CreatedAt, &payment.UpdatedAt,
		&payment.AccountEntryID, &payment.AccountFundedAt)
	if err != nil {
		return nil, err
	}
	return payment, nil
}

func scanFunding(row scanner) (*model.PaymentFunding, error) {
	funding := &model.PaymentFunding{}
	err := row.Scan(&funding.ID, &funding.PaymentID, &funding.LineNo, &funding.LineType,
		&funding.Amount, &funding.Status, &funding.ProviderTransactionID, &funding.FailureCode,
		&funding.AccountEntryID, &funding.CreatedAt, &funding.UpdatedAt, &funding.SucceededAt,
		&funding.ReversedAt)
	if err != nil {
		return nil, err
	}
	return funding, nil
}

// appendOutbox 在业务事务里追加一条领域事件。
//
// 为什么必须走 outbox 而不是直接投递：事件与它描述的事实得一起落地。事务回滚的事件
// 不能发出去，提交了的事件也不能因为 broker 当时不通就丢——前者会让下游看到一笔不存在的
// 收款，后者会让订单永远停在待支付。
func appendOutbox(ctx context.Context, tx pgx.Tx, eventType, eventVersion, traceID string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	return messaging.NewPostgreSQLWithQuerier(tx).Append(ctx, messaging.Envelope{
		EventID:      newEventID(),
		EventType:    eventType,
		EventVersion: eventVersion,
		TraceID:      traceID,
		Payload:      encoded,
	})
}

// recordTransition 追加一条状态流水。这张表只追加，触发器挡改和删。
func recordTransition(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, from, to, reason, requestID, actorType string, actorID *string, metadata any) error {
	var encoded []byte
	if metadata != nil {
		var err error
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("encode transition metadata: %w", err)
		}
	} else {
		encoded = []byte("{}")
	}
	_, err := tx.Exec(ctx, `INSERT INTO payment_state_transitions
		(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,actor_type,actor_id,metadata)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		aggregateType, aggregateID, from, to, reason, requestID, actorType, actorID, encoded)
	return err
}

// RequestHash 算幂等键上那条「同一个 key 换了请求体」判定用的哈希。
//
// 导出是为了让 service 决定**哈希哪些字段**（那是业务判断：subject 与 attach 不该算进去），
// 而实现留在这里（与 order-service 的同名算法一致）。传进来的应该是一个具名结构体而不是
// map——map 的序列化顺序虽然按 key 排序，但字段多一个少一个要靠读代码才发现。
func RequestHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// beginIdempotentOperation 抢占一个幂等键，返回 (已完成的响应, 是否命中, 是否回收了陈旧记录, error)。
//
// 命中且状态是 succeeded 时把当时的响应体回放给调用方，调用方不再执行副作用——
// 这是「重复发起支付只落一张支付单」的全部依据。
//
// 第三个返回值 recycled 为 true 表示这次**没有命中**、而且要接着往下走，但走之前得先把上一次
// 那半截尝试留下的东西收拾掉。清理什么由调用方决定：这个函数只认幂等键，不认识支付单，
// 而陈旧的 processing 记录必然对应着调用方那一侧某个未完成的资源（见 retireAbandonedPayment）。
func beginIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, hash, resourceType, resourceID string) ([]byte, bool, bool, error) {
	result, err := tx.Exec(ctx, `INSERT INTO payment_idempotency_keys
		(scope,idempotency_key,request_hash,resource_type,resource_id,status)
		VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing')
		ON CONFLICT (scope, idempotency_key) DO NOTHING`, scope, key, hash, resourceType, resourceID)
	if err != nil {
		return nil, false, false, err
	}
	if result.RowsAffected() == 1 {
		return nil, false, false, nil
	}
	var existingHash, status string
	var response []byte
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT request_hash,status,response,created_at
		FROM payment_idempotency_keys WHERE scope=$1 AND idempotency_key=$2 FOR UPDATE`,
		scope, key).Scan(&existingHash, &status, &response, &createdAt); err != nil {
		return nil, false, false, err
	}
	if existingHash != hash {
		return nil, false, false, ErrIdempotencyConflict
	}
	switch status {
	case model.IdempotencySucceeded, model.IdempotencyFailed:
		// succeeded 与 failed **回放的是同一种东西**：上次的响应快照。这是「一个幂等键
		// 只有一个结论」的完整含义——不只是「不会重复执行」，也是「重复调用得到同一个答复」。
		//
		// order-service 那边没有 failed 这个分支，因为它的失败会随事务回滚、幂等行跟着消失，
		// 重试就是一次全新的尝试。支付这边失败要留痕（payments 表的注释：失败的那张单留着，
		// 用户换一种方式再付是新的一行），所以幂等行会提交成 failed，必须在这里被认出来。
		//
		// 没有这个 case 的后果不是「重试了一次」，而是一次**格式错误**：default 分支会把
		// 一行正常写下的 failed 记录报成 unsupported status。
		return response, true, false, nil
	case model.IdempotencyProcessing:
		if createdAt.After(time.Now().Add(-IdempotencyRecoveryWindow)) {
			return nil, false, false, ErrIdempotencyInProgress
		}
		// 比窗口还旧的 processing 行不可能是活着的操作（事务提交后它就不是 processing 了），
		// 所以回收它再重试。删掉重建而不是就地改状态：新事务应当拿到一个干净的行。
		if _, err := tx.Exec(ctx, `DELETE FROM payment_idempotency_keys WHERE scope=$1 AND idempotency_key=$2`, scope, key); err != nil {
			return nil, false, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_idempotency_keys
			(scope,idempotency_key,request_hash,resource_type,resource_id,status)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing')`, scope, key, hash, resourceType, resourceID); err != nil {
			return nil, false, false, err
		}
		// recycled=true：键回收了，调用方接着往下走，但必须先清理上一次那半截尝试留下的资源
		// ——上一次的支付单还挂在同一个 request_id 上，不腾开的话下面那句 INSERT INTO payments
		// 会撞 payments_request_id_key，回收就成了一次必然失败的尝试。
		return nil, false, true, nil
	default:
		return nil, false, false, fmt.Errorf("idempotency operation has unsupported status %q", status)
	}
}

// failIdempotentOperation 把幂等记录置为 failed。
//
// 与 order-service 不同的一处：那边失败时不改幂等行（事务回滚，行随之消失，重试就是
// 一次全新的尝试）。支付这边失败也**提交**（失败要留在 payments 表里），所以幂等行会
// 跟着提交；不标它的话，同一个 request_id 重试会以为「上次还在处理中」，白等 15 分钟。
func failIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, resourceID string, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode idempotent response: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE payment_idempotency_keys
		SET resource_id=NULLIF($2,'')::uuid, response=$3, status='failed'
		WHERE scope=$1 AND idempotency_key=$4`, scope, resourceID, encoded, key)
	return err
}

func completeIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, resourceID string, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode idempotent response: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE payment_idempotency_keys
		SET resource_id=NULLIF($2,'')::uuid, response=$3, status='succeeded'
		WHERE scope=$1 AND idempotency_key=$4`, scope, resourceID, encoded, key)
	return err
}

// mapPGError 把唯一约束冲突翻成调用方分得清的业务错误。
//
// 没有它，同一笔订单收到两次成功回调会变成 500「服务器错误」，而它其实是一个说得清楚的
// 幂等结论。约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时
// 静默失效。
//
// 下面的名字是**从 dev 库里读出来的实际值**，不是照 DDL 拼的：PG 的自动命名会把
// UNIQUE (kind,payment_no,refund_no,funding_line_no) 截到 63 字节，
// payment_transactions_kind_payment_no_refund_no_funding_line_no_key 这个名字在库里
// 根本不存在（实际是 ..._funding_line_key）。照 DDL 拼会写成一个永不命中的 case，
// 而它只在重复记账时才走到——正是最不该静默失效的那条路。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPaymentNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		// 订单只能被收一次钱：第二张成功支付单在数据库层就被挡掉，不靠应用判断。
		// 这是一条**部分唯一索引**，名字不随列名生成，可以照写。
		case "payments_one_succeeded_per_order":
			return ErrPaymentNotPending
		case "payments_payment_no_key":
			// 支付单号撞了。单号带随机后缀，正常不会发生；当成「这单已经有了」比当成
			// 未知错误好——重试拿到的是同一张单，不是一笔新的钱。
			return ErrPaymentNotPending
		case "payments_request_id_key":
			return ErrPaymentNotPending
		case "payment_fundings_one_live_success":
			return ErrPaymentNotPending
		case "payment_notifications_provider_notification_id_key":
			return ErrPaymentNotificationConflict
		case "payment_transactions_kind_payment_no_refund_no_funding_line_key":
			// 同一笔出资已经记过账。重投的正常路径不该走到这里（notification 唯一键
			// 先挡一层），走到说明是别的路径重复调用——当成幂等成功，别重复记账。
			return ErrPaymentNotificationConflict
		}
	}
	return err
}
