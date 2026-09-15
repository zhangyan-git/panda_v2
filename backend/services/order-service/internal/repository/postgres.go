// Package repository 是订单库的数据访问层。
//
// 按聚合拆文件：order.go 是 Order 聚合的写路径（下单、支付落单、取消、超时关单，
// 每条都在一个事务里连带写状态流水与 outbox），order_query.go 是读路径。
// 跨表的业务事务由 service 编排，repository 只负责「一个事务里把这几个事实写进去」。
package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

var (
	// ErrIdempotencyInProgress：同一个幂等键的操作还在处理中。
	ErrIdempotencyInProgress = errors.New("idempotency operation is still processing")
	// ErrIdempotencyConflict：同一个幂等键换了请求体。前者用 409 让调用方等一下，
	// 后者用 409 告诉调用方「这个键你用过了」，不能混成一个错误。
	ErrIdempotencyConflict = errors.New("idempotency key request hash conflict")
	// ErrOrderNotFound：订单不存在。
	ErrOrderNotFound = errors.New("order not found")
	// ErrOrderNotPending：订单不在待支付状态（已经付过、退过或关过）。
	// 支付结果事件遇到它要幂等地当成功处理，取消请求遇到它要回 409。
	ErrOrderNotPending = errors.New("order is not awaiting payment")
	// ErrPaymentAmountMismatch：事件里的金额与订单应付金额对不上。
	ErrPaymentAmountMismatch = errors.New("payment amount does not match the order")
	// ErrDuplicateRequest：同一个 request_id 已经落过一张订单。
	ErrDuplicateRequest = errors.New("order request id is already used")
	// ErrOrderNotCompletable：订单不在可完成的状态（还没付钱、已经退过、关过了）。
	// 与 ErrOrderNotPending 分开：前者说的是「这单付过款了吗」，后者是「这单还没付吗」，
	// 合并成一个会让调用方分不清自己该去催付款还是该去查退款。
	ErrOrderNotCompletable = errors.New("order is not awaiting completion")
	// ErrCouponAlreadyUsed：这张券已经落在别的订单行上了。
	ErrCouponAlreadyUsed = errors.New("coupon is already attached to another order line")
)

// IdempotencyRecoveryWindow 是一条 processing 记录还能被认为「有事务正在跑」的时长。
//
// 事务提交后这条记录就变成 succeeded/failed，所以一条比这个窗口还旧的 processing 行
// 只可能来自「进程在提交前死掉」。到那时它不代表任何活着的操作，可以回收重试；
// 在窗口内则必须先回 409，否则两个并发请求会各自执行一遍副作用。
const IdempotencyRecoveryWindow = 15 * time.Minute

// orderColumns 是 orders 表的读取列。
//
// UUID 列一律 ::text：pgx 把 uuid 扫进 string 需要这一步，少了它 Scan 会报类型不匹配。
// 列顺序与 scanOrder 的扫描顺序严格一一对应，两边必须一起改。
const orderColumns = `id::text, order_no, legacy_id, user_id::text, source, status,
	fulfillment_status, store_id::text, store_name, device_id::text, device_no, scene_token,
	original_amount, discount_amount, payable_amount, paid_amount, refunded_amount,
	membership_id::text, membership_snapshot, fortune_cards_expected, fortune_card_snapshot,
	payment_method, payment_no, paid_at, finished_at, cancelled_at, cancellation_reason,
	expires_at, remark, request_id, created_at, updated_at`

const orderLineColumns = `id::text, order_id::text, line_no, line_type, legacy_id,
	item_id::text, item_code, item_name, item_image, quantity, original_unit_price, unit_price,
	price_discount_amount, discount_amount, payable_amount, coupon_id::text,
	coupon_discount_amount, specs, selection_snapshot, campaign_id::text, campaign_snapshot,
	membership_plan_snapshot, device_id::text, device_order_no, fulfillment_task_no,
	pickup_code, remark, created_at, updated_at`

const paymentLineColumns = `id::text, order_id::text, line_no, line_type, amount, status,
	payment_no, provider_transaction_id, failure_code, account_entry_id::text,
	created_at, updated_at, succeeded_at, reversed_at`

const transitionColumns = `id::text, aggregate_type, aggregate_id::text, from_status,
	to_status, reason, request_id, actor_type, actor_id::text, metadata, created_at`

// PostgresRepository 是订单库的数据访问实现。
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

func scanOrder(row scanner) (*model.Order, error) {
	return scanOrderWith(row)
}

// scanOrderWith 按 orderColumns 的顺序扫一张订单，后面再接若干附加列。
//
// 附加列必须跟在订单列之后（见 orderColumns 的注释：列顺序与扫描顺序一一对应）。
// 拆出这个变体是为了让「列表多查两个派生列」不必把 32 个字段抄第二遍——抄一遍就多一处
// 会与 orderColumns 漂移的地方。
func scanOrderWith(row scanner, extra ...any) (*model.Order, error) {
	order := &model.Order{}
	targets := []any{&order.ID, &order.OrderNo, &order.LegacyID, &order.UserID, &order.Source,
		&order.Status, &order.FulfillmentStatus, &order.StoreID, &order.StoreName, &order.DeviceID,
		&order.DeviceNo, &order.SceneToken, &order.OriginalAmount, &order.DiscountAmount,
		&order.PayableAmount, &order.PaidAmount, &order.RefundedAmount, &order.MembershipID,
		&order.MembershipSnapshot, &order.FortuneCardsExpected, &order.FortuneCardSnapshot,
		&order.PaymentMethod, &order.PaymentNo, &order.PaidAt, &order.FinishedAt,
		&order.CancelledAt, &order.CancellationReason, &order.ExpiresAt, &order.Remark,
		&order.RequestID, &order.CreatedAt, &order.UpdatedAt}
	targets = append(targets, extra...)
	if err := row.Scan(targets...); err != nil {
		return nil, err
	}
	return order, nil
}

func scanOrderLine(row scanner) (*model.OrderLine, error) {
	line := &model.OrderLine{}
	err := row.Scan(&line.ID, &line.OrderID, &line.LineNo, &line.LineType, &line.LegacyID,
		&line.ItemID, &line.ItemCode, &line.ItemName, &line.ItemImage, &line.Quantity,
		&line.OriginalUnitPrice, &line.UnitPrice, &line.PriceDiscountAmount, &line.DiscountAmount,
		&line.PayableAmount, &line.CouponID, &line.CouponDiscountAmount, &line.Specs,
		&line.SelectionSnapshot, &line.CampaignID, &line.CampaignSnapshot,
		&line.MembershipPlanSnapshot, &line.DeviceID, &line.DeviceOrderNo,
		&line.FulfillmentTaskNo, &line.PickupCode, &line.Remark,
		&line.CreatedAt, &line.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return line, nil
}

func scanPaymentLine(row scanner) (*model.OrderPaymentLine, error) {
	line := &model.OrderPaymentLine{}
	err := row.Scan(&line.ID, &line.OrderID, &line.LineNo, &line.LineType, &line.Amount,
		&line.Status, &line.PaymentNo, &line.ProviderTransactionID, &line.FailureCode,
		&line.AccountEntryID, &line.CreatedAt, &line.UpdatedAt, &line.SucceededAt, &line.ReversedAt)
	if err != nil {
		return nil, err
	}
	return line, nil
}

func scanTransition(row scanner) (*model.OrderStateTransition, error) {
	transition := &model.OrderStateTransition{}
	err := row.Scan(&transition.ID, &transition.AggregateType, &transition.AggregateID,
		&transition.FromStatus, &transition.ToStatus, &transition.Reason, &transition.RequestID,
		&transition.ActorType, &transition.ActorID, &transition.Metadata, &transition.CreatedAt)
	if err != nil {
		return nil, err
	}
	return transition, nil
}

// appendOutbox 在业务事务里追加一条领域事件。
//
// 为什么必须走 outbox 而不是直接投递：事件与它描述的事实得一起落地。事务回滚的事件
// 不能发出去，提交了的事件也不能因为 broker 当时不通就丢——前者会让下游看到一笔不存在的
// 订单，后者会让履约永远等不到。
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
	_, err := tx.Exec(ctx, `INSERT INTO order_state_transitions
		(aggregate_type,aggregate_id,from_status,to_status,reason,request_id,actor_type,actor_id,metadata)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		aggregateType, aggregateID, from, to, reason, requestID, actorType, actorID, encoded)
	return err
}

func requestHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// beginIdempotentOperation 抢占一个幂等键，返回 (已完成的响应, 是否命中, error)。
//
// 命中且状态是 succeeded 时把当时的响应体回放给调用方，调用方不再执行副作用——
// 这是「重复下单只落一单」的全部依据。
func beginIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, hash, resourceType, resourceID string) ([]byte, bool, error) {
	result, err := tx.Exec(ctx, `INSERT INTO order_idempotency_keys
		(scope,idempotency_key,request_hash,resource_type,resource_id,status)
		VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing')
		ON CONFLICT (scope, idempotency_key) DO NOTHING`, scope, key, hash, resourceType, resourceID)
	if err != nil {
		return nil, false, err
	}
	if result.RowsAffected() == 1 {
		return nil, false, nil
	}
	var existingHash, status string
	var response []byte
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT request_hash,status,response,created_at
		FROM order_idempotency_keys WHERE scope=$1 AND idempotency_key=$2 FOR UPDATE`,
		scope, key).Scan(&existingHash, &status, &response, &createdAt); err != nil {
		return nil, false, err
	}
	if existingHash != hash {
		return nil, false, ErrIdempotencyConflict
	}
	switch status {
	case model.IdempotencySucceeded:
		return response, true, nil
	case model.IdempotencyProcessing:
		if createdAt.After(time.Now().Add(-IdempotencyRecoveryWindow)) {
			return nil, false, ErrIdempotencyInProgress
		}
		// 比窗口还旧的 processing 行不可能是活着的操作（事务提交后它就不是 processing 了），
		// 所以回收它再重试。删掉重建而不是就地改状态：新事务应当拿到一个干净的行。
		if _, err := tx.Exec(ctx, `DELETE FROM order_idempotency_keys WHERE scope=$1 AND idempotency_key=$2`, scope, key); err != nil {
			return nil, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_idempotency_keys
			(scope,idempotency_key,request_hash,resource_type,resource_id,status)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,'processing')`, scope, key, hash, resourceType, resourceID); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("idempotency operation has unsupported status %q", status)
	}
}

func completeIdempotentOperation(ctx context.Context, tx pgx.Tx, scope, key, resourceID string, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode idempotent response: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE order_idempotency_keys
		SET resource_id=NULLIF($2,'')::uuid, response=$3, status='succeeded'
		WHERE scope=$1 AND idempotency_key=$4`, scope, resourceID, encoded, key)
	return err
}

// mapPGError 把唯一约束冲突翻成调用方分得清的业务错误。
//
// 没有它，一次重复用券会变成 500「服务器错误」，而它其实是一个说得清楚的业务冲突。
// 约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时静默失效。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOrderNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "order_lines_coupon_key":
			return ErrCouponAlreadyUsed
		case "orders_request_id_key":
			return ErrDuplicateRequest
		case "orders_payment_no_key":
			return ErrOrderNotPending
		}
	}
	return err
}
