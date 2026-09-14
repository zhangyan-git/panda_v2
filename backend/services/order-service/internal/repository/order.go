package repository

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
)

// 领域事件类型。方案 7.3 / 7.4：订单状态变更、支付结果与售后退款都以事件发出去，
// 由下游（优惠券、账户、履约、分账）各自幂等消费。
const (
	EventOrderCreated       = "order.created"
	EventOrderPaid          = "order.paid"
	EventOrderPaymentFailed = "order.payment_failed"
	EventOrderCancelled     = "order.cancelled"
	EventOrderExpired       = "order.expired"
	// EventAfterSaleApplied / EventAfterSaleReviewed 是售后这一段的两个交接点（方案 7.4）：
	// 退款链是「Order 创建售后申请 → Payment 创建退款单」，事件就是这个接力棒。
	// 今天没有消费者（payment-service 未建），事件投出去会被静默丢弃；等它建起来，
	// 消费 reviewed 里 approved 的那条就能建退款单，本服务的形状一个字不用改。
	//
	// 用户撤销 pending 不发事件：那一刻还没有任何下游动作可撤（申请阶段没有退款单），
	// 发出去只是一条没人需要的通知。
	EventAfterSaleApplied  = "order.after_sale.applied"
	EventAfterSaleReviewed = "order.after_sale.reviewed"
	eventVersion           = "v1"

	// idempotencyScopeCreate 是下单幂等键的 scope。写成常量而不是散在各处的字面量：
	// 它同时出现在「抢占」和「回填响应」两处，写歪一处就变成两个互不相认的命名空间。
	idempotencyScopeCreate = "order.create"
	// idempotencyScopeAfterSaleApply 是退款申请幂等键的 scope。与下单分开命名空间：
	// 同一个 Idempotency-Key 值在两端各自成立，不会互相顶掉。
	idempotencyScopeAfterSaleApply = "order.after_sale.apply"
)

func newEventID() string { return uuid.NewString() }

// newPickupCode 生成取杯码：一个不透明的短凭据。
//
// 为什么是随机而不是从订单号派生：取杯码是**凭据**，谁拿到它就能取走那杯咖啡。
// 从订单号派生等于把凭据公开——订单号会出现在客服系统、对账文件、后台列表里。
// order_lines_pickup_code_key 唯一索引兜住碰撞，撞了就是一次 500 重试，不是安全问题。
func newPickupCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去掉 I/O/0/1，避免口头/肉眼混淆
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// CreateOrderParams 是一次下单要落进库的全部事实。金额与订单号由 service 算好，
// 仓储不参与定价——它只负责「这些事实要么一起进库，要么一个都不进」。
type CreateOrderParams struct {
	IdempotencyKey string
	TraceID        string
	// RequestID 写进 orders.request_id，同一个请求号只能落一单（orders_request_id_key）。
	RequestID string
	Order     *OrderInsert
	Lines     []*OrderLineInsert
}

// OrderInsert 是主表待插入的列。ID / created_at / updated_at 由数据库默认值给。
type OrderInsert struct {
	OrderNo              string
	UserID               string
	Source               string
	Status               string
	FulfillmentStatus    string
	StoreID              *string
	StoreName            string
	DeviceID             *string
	DeviceNo             string
	SceneToken           string
	OriginalAmount       int64
	DiscountAmount       int64
	PayableAmount        int64
	MembershipID         *string
	MembershipSnapshot   []byte
	FortuneCardsExpected int
	FortuneCardSnapshot  []byte
	Remark               string
	ExpiresAt            time.Time
}

// OrderLineInsert 是订单行待插入的列。line_no 由调用方排好：行的顺序是下单语境里的
// 事实（第一行是那杯咖啡），不该由数据库按插入顺序再猜一遍。
type OrderLineInsert struct {
	LineNo                 int
	LineType               string
	ItemID                 *string
	ItemCode               string
	ItemName               string
	ItemImage              string
	Quantity               int
	OriginalUnitPrice      int64
	UnitPrice              int64
	PriceDiscountAmount    int64
	DiscountAmount         int64
	PayableAmount          int64
	CouponID               *string
	CouponDiscountAmount   int64
	Specs                  []byte
	SelectionSnapshot      []byte
	CampaignID             *string
	CampaignSnapshot       []byte
	MembershipPlanSnapshot []byte
	DeviceID               *string
	Remark                 string
}

// OrderCreatedEvent 是 order.created 的事件体。只带下游需要的 ID 与金额，不带快照：
// 事件是通知，不是另一个真相来源，谁要细节谁按 order_no 来查。
type OrderCreatedEvent struct {
	OrderID           string `json:"orderId"`
	OrderNo           string `json:"orderNo"`
	UserID            string `json:"userId"`
	StoreID           string `json:"storeId,omitempty"`
	DeviceID          string `json:"deviceId,omitempty"`
	PayableAmount     int64  `json:"payableAmount"`
	FulfillmentStatus string `json:"fulfillmentStatus"`
	ExpiresAt         string `json:"expiresAt"`
}

// CreateOrder 在一个事务里落订单、行、状态流水与 order.created 事件，并写幂等记录。
//
// 幂等键命中且已完成时回放当时的响应（replayed=true），调用方据此知道这次什么都没做。
// 这是「用户连点两下确认支付/确认下单只落一单」的全部依据——数据库的唯一索引只能挡住
// 同一个 request_id，挡不住「同一个请求换了 request_id 重发」。
func (r *PostgresRepository) CreateOrder(ctx context.Context, p CreateOrderParams) (*CreateOrderResult, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	hash := requestHash(map[string]any{
		"user_id": p.Order.UserID, "source": p.Order.Source, "store_id": p.Order.StoreID,
		"device_id": p.Order.DeviceID, "payable_amount": p.Order.PayableAmount,
		"lines": len(p.Lines), "request_id": p.RequestID,
	})
	response, done, err := beginIdempotentOperation(ctx, tx, idempotencyScopeCreate, p.IdempotencyKey, hash, "order", "")
	if err != nil {
		return nil, false, err
	}
	if done {
		var replayed CreateOrderResult
		if err := json.Unmarshal(response, &replayed); err != nil {
			return nil, false, err
		}
		return &replayed, true, tx.Commit(ctx)
	}

	order := p.Order
	var id string
	var createdAt, updatedAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO orders
		(order_no, user_id, source, status, fulfillment_status, store_id, store_name, device_id,
		 device_no, scene_token, original_amount, discount_amount, payable_amount, paid_amount,
		 refunded_amount, membership_id, membership_snapshot, fortune_cards_expected,
		 fortune_card_snapshot, request_id, remark, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0,0,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id::text, created_at, updated_at`,
		order.OrderNo, order.UserID, order.Source, order.Status, order.FulfillmentStatus,
		order.StoreID, order.StoreName, order.DeviceID, order.DeviceNo, order.SceneToken,
		order.OriginalAmount, order.DiscountAmount, order.PayableAmount, order.MembershipID,
		order.MembershipSnapshot, order.FortuneCardsExpected, order.FortuneCardSnapshot,
		p.RequestID, order.Remark, order.ExpiresAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, false, mapPGError(err)
	}

	for _, line := range p.Lines {
		// device_id 冗余在行上，是为了 (device_id, device_order_no) 这条唯一索引：
		// 厂商单号的唯一性作用域是「同一台机器」。V2 一台机器服务一次下单，所以
		// 行的 device_id 就是订单上那台机器的副本。
		if _, err := tx.Exec(ctx, `INSERT INTO order_lines
			(order_id, line_no, line_type, item_id, item_code, item_name, item_image, quantity,
			 original_unit_price, unit_price, price_discount_amount, discount_amount,
			 payable_amount, coupon_id, coupon_discount_amount, specs, selection_snapshot,
			 campaign_id, campaign_snapshot, membership_plan_snapshot, device_id, remark)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
			id, line.LineNo, line.LineType, line.ItemID, line.ItemCode, line.ItemName,
			line.ItemImage, line.Quantity, line.OriginalUnitPrice, line.UnitPrice,
			line.PriceDiscountAmount, line.DiscountAmount, line.PayableAmount, line.CouponID,
			line.CouponDiscountAmount, line.Specs, line.SelectionSnapshot, line.CampaignID,
			line.CampaignSnapshot, line.MembershipPlanSnapshot, line.DeviceID, line.Remark); err != nil {
			return nil, false, mapPGError(err)
		}
	}

	if err := recordTransition(ctx, tx, "order", id, "", order.Status, "", p.RequestID, "user", nil, nil); err != nil {
		return nil, false, err
	}

	event := OrderCreatedEvent{
		OrderID: id, OrderNo: order.OrderNo, UserID: order.UserID,
		PayableAmount: order.PayableAmount, FulfillmentStatus: order.FulfillmentStatus,
		ExpiresAt: order.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if order.StoreID != nil {
		event.StoreID = *order.StoreID
	}
	if order.DeviceID != nil {
		event.DeviceID = *order.DeviceID
	}
	if err := appendOutbox(ctx, tx, EventOrderCreated, eventVersion, p.TraceID, event); err != nil {
		return nil, false, err
	}

	result := &CreateOrderResult{
		OrderID: id, OrderNo: order.OrderNo, Status: order.Status,
		FulfillmentStatus: order.FulfillmentStatus, OriginalAmount: order.OriginalAmount,
		DiscountAmount: order.DiscountAmount, PayableAmount: order.PayableAmount,
		ExpiresAt: order.ExpiresAt, CreatedAt: createdAt,
	}
	if err := completeIdempotentOperation(ctx, tx, idempotencyScopeCreate, p.IdempotencyKey, id, result); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return result, false, nil
}

// CreateOrderResult 是下单的结果，也是写进幂等表的响应快照：同一个幂等键重放时
// 原样回放它，不再执行一遍副作用。字段与 dto.CreateOrderResponse 一一对应。
type CreateOrderResult struct {
	OrderID           string    `json:"orderId"`
	OrderNo           string    `json:"orderNo"`
	Status            string    `json:"status"`
	FulfillmentStatus string    `json:"fulfillmentStatus"`
	OriginalAmount    int64     `json:"originalAmount"`
	DiscountAmount    int64     `json:"discountAmount"`
	PayableAmount     int64     `json:"payableAmount"`
	ExpiresAt         time.Time `json:"expiresAt"`
	CreatedAt         time.Time `json:"createdAt"`
}

// SettlePaymentParams 是支付结果事件落库需要的全部输入。
type SettlePaymentParams struct {
	OrderNo               string
	PaymentNo             string
	Amount                int64
	PaymentMethod         string
	Fundings              []FundingLine
	ProviderTransactionID string
	PaidAt                *time.Time
	// RequestID 是事件的 event_id：写进状态流水，让「哪条消息推动了这次变更」可查。
	RequestID string
	TraceID   string
	// Outcome 是 payment.succeeded / payment.failed。
	Outcome        string
	FailureCode    string
	FailureMessage string
}

// FundingLine 是一笔出资分摊。
type FundingLine struct {
	LineType       string
	Amount         int64
	PaymentNo      string
	AccountEntryID *string
}

// SettlePayment 把支付结果落到订单上。
//
// changed=false 表示这条事件已经被处理过（订单已是 paid/completed），调用方据此正常
// ack——重复回调是常态，不是错误：broker 重连、消费者在 ack 前崩掉都会重投一次。
// 真正的异常是「钱到了但订单已经关了」，那用 ErrOrderNotPending 报出去，让消息进
// 死信队列由人来看，而不是假装成功把它丢掉。
func (r *PostgresRepository) SettlePayment(ctx context.Context, p SettlePaymentParams) (*OrderPaymentResult, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	order, err := lockedOrderByNo(ctx, tx, p.OrderNo)
	if err != nil {
		return nil, false, err
	}
	switch order.Status {
	case "paid", "completed":
		// 已经处理过这条（或更晚的）结果，幂等返回。
		return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: order.Status, Applied: false}, false, tx.Commit(ctx)
	case "pending_payment":
		// 继续往下处理。
	default:
		return nil, false, ErrOrderNotPending
	}

	if p.Outcome == "payment.failed" {
		if err := settleFailed(ctx, tx, order, p); err != nil {
			return nil, false, err
		}
		return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: order.Status, Applied: false}, false, tx.Commit(ctx)
	}

	// 金额必须和订单的应付金额一致。信事件里的数字而不对账，等于让一条错误的事件
	// 把订单标成已支付；对不上就报错进死信，由人来查这笔钱到底该挂在哪儿。
	if p.Amount != order.PayableAmount {
		return nil, false, fmt.Errorf("%w: event=%d order=%d", ErrPaymentAmountMismatch, p.Amount, order.PayableAmount)
	}
	fundings := p.Fundings
	if len(fundings) == 0 {
		// 纯渠道支付是最常见的一种，事件可以不带分摊；这里补成一条，而不是让
		// order_payment_lines 空着——退款要按来源冲正，一行都没有就无从冲起。
		method := strings.TrimSpace(p.PaymentMethod)
		if method == "" {
			method = "other"
		}
		fundings = []FundingLine{{LineType: method, Amount: p.Amount, PaymentNo: p.PaymentNo}}
	}
	var total int64
	for _, f := range fundings {
		if f.Amount <= 0 {
			return nil, false, fmt.Errorf("%w: funding amount must be positive", ErrPaymentAmountMismatch)
		}
		total += f.Amount
	}
	if total != p.Amount {
		return nil, false, fmt.Errorf("%w: fundings total %d, event %d", ErrPaymentAmountMismatch, total, p.Amount)
	}

	paidAt := time.Now()
	if p.PaidAt != nil {
		paidAt = *p.PaidAt
	}
	if _, err := tx.Exec(ctx, `UPDATE orders
		SET status='paid', paid_amount=$2, payment_method=$3, payment_no=$4, paid_at=$5, updated_at=NOW()
		WHERE id=$1`, order.ID, p.Amount, p.PaymentMethod, p.PaymentNo, paidAt); err != nil {
		return nil, false, mapPGError(err)
	}
	for i, f := range fundings {
		if _, err := tx.Exec(ctx, `INSERT INTO order_payment_lines
			(order_id, line_no, line_type, amount, status, payment_no, provider_transaction_id,
			 account_entry_id, succeeded_at)
			VALUES ($1,$2,$3,$4,'succeeded',$5,$6,$7,$8)`,
			order.ID, i+1, f.LineType, f.Amount, f.PaymentNo, p.ProviderTransactionID,
			f.AccountEntryID, paidAt); err != nil {
			return nil, false, mapPGError(err)
		}
	}
	// 取杯码在这一刻生成：付款之前它只是一张空头凭据，付款之后才是取走那杯咖啡的凭据。
	// 只在用户本人查自己的订单时返回（见 controller 的响应构造）。
	code, err := newPickupCode()
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE order_lines SET pickup_code=$2, updated_at=NOW()
		WHERE order_id=$1 AND line_type='drink' AND pickup_code IS NULL`, order.ID, code); err != nil {
		return nil, false, mapPGError(err)
	}

	if err := recordTransition(ctx, tx, "order", order.ID, order.Status, "paid", "支付成功", p.RequestID, "system", nil, nil); err != nil {
		return nil, false, err
	}
	if err := appendOutbox(ctx, tx, EventOrderPaid, eventVersion, p.TraceID, map[string]any{
		"orderId": order.ID, "orderNo": order.OrderNo, "userId": order.UserID,
		"paidAmount": p.Amount, "paymentNo": p.PaymentNo, "paymentMethod": p.PaymentMethod,
		"fundings": fundings, "paidAt": paidAt.UTC().Format(time.RFC3339),
	}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: "paid", Applied: true}, true, nil
}

// OrderPaymentResult 是支付结果落单的结果。
type OrderPaymentResult struct {
	OrderID string
	OrderNo string
	Status  string
	// Applied=false 表示这条事件已经被处理过，调用方正常 ack 即可。
	Applied bool
}

// settleFailed 记录一次失败的支付尝试：主状态不动，只留一笔失败的出资流水与状态流水。
//
// 主状态不改是有意的：一次支付失败不等于订单作废，用户可以换一种方式再付一次
// （微信失败改用咖啡豆），订单仍是 pending_payment 直到超时或用户取消。
func settleFailed(ctx context.Context, tx pgx.Tx, order *lockedOrder, p SettlePaymentParams) error {
	method := strings.TrimSpace(p.PaymentMethod)
	if method == "" {
		method = "other"
	}
	amount := p.Amount
	if amount <= 0 {
		// 失败事件里的金额可能是 0（渠道还没扣款就拒了），但 order_payment_lines.amount
		// 有 CHECK (amount > 0)。失败流水记的是「这次尝试想扣多少」，用订单应付额最诚实。
		amount = order.PayableAmount
		if amount <= 0 {
			amount = 1
		}
	}
	var lineID string
	if err := tx.QueryRow(ctx, `INSERT INTO order_payment_lines
		(order_id, line_no, line_type, amount, status, payment_no, provider_transaction_id, failure_code)
		VALUES ($1, COALESCE((SELECT MAX(line_no) FROM order_payment_lines WHERE order_id=$1), 0) + 1,
		        $2, $3, 'failed', $4, $5, $6)
		RETURNING id::text`, order.ID, method, amount, p.PaymentNo, p.ProviderTransactionID, p.FailureCode).Scan(&lineID); err != nil {
		return mapPGError(err)
	}
	if err := recordTransition(ctx, tx, "payment_line", lineID, "", "failed", p.FailureMessage, p.RequestID, "system", nil, nil); err != nil {
		return err
	}
	return appendOutbox(ctx, tx, EventOrderPaymentFailed, eventVersion, p.TraceID, map[string]any{
		"orderId": order.ID, "orderNo": order.OrderNo, "failureCode": p.FailureCode,
		"failureMessage": p.FailureMessage,
	})
}

// lockedOrder 是 FOR UPDATE 锁住的那一行订单的只读副本。
//
// 列是「锁内判定要用到的那几列」，不是整行：取消要看状态，支付落单要对应付额对账，
// 售后申请要算可退余额与福卡承诺。多一列只是多一次无用的读，少一列则会让某个判定
// 退化成「事务外先读一遍」——那正是并发下最容易错的地方。
type lockedOrder struct {
	ID                   string
	OrderNo              string
	UserID               string
	Status               string
	PayableAmount        int64
	PaidAmount           int64
	RefundedAmount       int64
	FortuneCardsExpected int
}

const lockedOrderColumns = `id::text, order_no, user_id::text, status, payable_amount,
	paid_amount, refunded_amount, fortune_cards_expected`

func scanLockedOrder(row scanner) (*lockedOrder, error) {
	order := &lockedOrder{}
	err := row.Scan(&order.ID, &order.OrderNo, &order.UserID, &order.Status,
		&order.PayableAmount, &order.PaidAmount, &order.RefundedAmount, &order.FortuneCardsExpected)
	if err != nil {
		return nil, err
	}
	return order, nil
}

func lockedOrderByNo(ctx context.Context, tx pgx.Tx, orderNo string) (*lockedOrder, error) {
	order, err := scanLockedOrder(tx.QueryRow(ctx,
		`SELECT `+lockedOrderColumns+` FROM orders WHERE order_no=$1 FOR UPDATE`, orderNo))
	if err != nil {
		return nil, mapPGError(err)
	}
	return order, nil
}

// CancelOrderParams 是取消订单的输入。
type CancelOrderParams struct {
	OrderID   string
	Reason    string
	RequestID string
	TraceID   string
	// ActorType 与 ActorID 写进状态流水：谁取消的。
	ActorType string
	ActorID   *string
	// Audit 为真时在同一个事务里向身份库的审计链路追加一条后台操作记录。
	// 用户自己取消为假——那不是「管理操作」，订单自己的状态流水已经记了。
	Audit bool
}

// CancelOrder 取消一笔待支付的订单。
//
// 只有 pending_payment 能取消：付过款的要退钱（走售后，方案 7.4），已关单的不动产。
// 越界的状态用 ErrOrderNotPending 报出去，controller 翻成 409，而不是 500。
func (r *PostgresRepository) CancelOrder(ctx context.Context, p CancelOrderParams) (*OrderPaymentResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	order, err := lockedOrderByID(ctx, tx, p.OrderID)
	if err != nil {
		return nil, err
	}
	if order.Status != "pending_payment" {
		return nil, ErrOrderNotPending
	}
	if _, err := tx.Exec(ctx, `UPDATE orders
		SET status='cancelled', cancelled_at=NOW(), cancellation_reason=$2, updated_at=NOW()
		WHERE id=$1`, order.ID, p.Reason); err != nil {
		return nil, mapPGError(err)
	}
	if err := recordTransition(ctx, tx, "order", order.ID, order.Status, "cancelled", p.Reason, p.RequestID, p.ActorType, p.ActorID, nil); err != nil {
		return nil, err
	}
	if err := appendOutbox(ctx, tx, EventOrderCancelled, eventVersion, p.TraceID, map[string]any{
		"orderId": order.ID, "orderNo": order.OrderNo, "userId": order.UserID,
		"reason": p.Reason, "actorType": p.ActorType,
	}); err != nil {
		return nil, err
	}
	if p.Audit {
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "orders", Action: "cancel", Operation: "取消订单",
			TargetType: "order", TargetID: order.ID, TargetName: order.OrderNo,
			Before: audit.Snapshot(map[string]any{"status": order.Status}),
			After:  audit.Snapshot(map[string]any{"status": "cancelled", "reason": p.Reason}),
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: "cancelled", Applied: true}, nil
}

func lockedOrderByID(ctx context.Context, tx pgx.Tx, id string) (*lockedOrder, error) {
	order, err := scanLockedOrder(tx.QueryRow(ctx,
		`SELECT `+lockedOrderColumns+` FROM orders WHERE id=NULLIF($1,'')::uuid FOR UPDATE`, id))
	if err != nil {
		return nil, mapPGError(err)
	}
	return order, nil
}

// ExpireOverdue 把到点还没付款的订单关掉，返回关掉的条数。
//
// 用 SKIP LOCKED 而不是普通 FOR UPDATE：多个副本同时扫同一张表时，抢不到行的那个
// 直接跳过而不是排队等锁——关单是补偿任务，等锁只会让它越跑越慢。
func (r *PostgresRepository) ExpireOverdue(ctx context.Context, limit int, traceID string) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `WITH overdue AS (
			SELECT id FROM orders
			WHERE status='pending_payment' AND expires_at IS NOT NULL AND expires_at <= NOW()
			ORDER BY expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE orders o
		SET status='expired', cancelled_at=NOW(), cancellation_reason='超时未支付', updated_at=NOW()
		FROM overdue
		WHERE o.id = overdue.id
		RETURNING o.id::text, o.order_no, o.user_id::text`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct{ id, orderNo, userID string }
	var closed []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.orderNo, &e.userID); err != nil {
			rows.Close()
			return 0, err
		}
		closed = append(closed, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, e := range closed {
		if err := recordTransition(ctx, tx, "order", e.id, "pending_payment", "expired", "超时未支付", "", "system", nil, nil); err != nil {
			return 0, err
		}
		if err := appendOutbox(ctx, tx, EventOrderExpired, eventVersion, traceID, map[string]any{
			"orderId": e.id, "orderNo": e.orderNo, "userId": e.userID,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(closed), nil
}
