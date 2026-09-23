package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 领域事件类型。方案 7.3 / 7.4：订单状态变更、支付结果与售后退款都以事件发出去，
// 由下游（优惠券、账户、履约、分账）各自幂等消费。
const (
	EventOrderCreated       = "order.created"
	EventOrderPaid          = "order.paid"
	EventOrderPaymentFailed = "order.payment_failed"
	EventOrderCancelled     = "order.cancelled"
	EventOrderExpired       = "order.expired"
	// EventOrderCompleted 是「这一单完成了」——福卡的发放时点就是它（原型口径：订单完成后
	// 到账，制作中不入账）。**人工标记完成与将来的履约完成事件共用这一个类型**：触发源不同，
	// 发放口径一个字不差，所以后台那个临时入口敢先上线，履约接上来时下游一行不用改。
	EventOrderCompleted = "order.completed"
	// EventAfterSaleApplied / EventAfterSaleReviewed 是售后这一段的两个交接点（方案 7.4）：
	// 退款链是「Order 创建售后申请 → Payment 创建退款单」，事件就是这个接力棒。
	// payment-service 未建之前 reviewed 里 approved 的那条会被静默丢弃，等它建起来消费，
	// 本服务的形状一个字不用改。
	//
	// 这三个事件今天有一个**真实的**消费者：account-service。它用 applied 冻结这一单的福卡
	// （申请即冻，用户提交申请的那一刻起那些卡不能拿去抽奖）、用 reviewed 里被驳回的那条
	// 与 cancelled 解冻。
	EventAfterSaleApplied  = "order.after_sale.applied"
	EventAfterSaleReviewed = "order.after_sale.reviewed"
	// EventAfterSaleCancelled 是用户撤销自己那张还没被审核的申请。
	//
	// 它曾经不发事件，理由是「这一刻还没有任何下游动作可撤」——申请阶段没有退款单，
	// payments 侧什么都没做过。福卡冻结让那句话不再成立：**有**动作可撤了，冻着的卡得放回去。
	// 所以撤销必须发出来，否则用户撤了申请、卡还锁着，而他手上再没有任何能解开它的动作。
	EventAfterSaleCancelled = "order.after_sale.cancelled"
	// EventAfterSaleRefunded / EventAfterSaleRefundFailed 是退款链的**收口**：refunding 只有
	// 这两个出口，而结论只有 payment-service 知道（见 refund.go 的 AdvanceRefund）。
	//
	// 它们分开成两个类型而不是一条带 status 的，因为消费方要做的两件事是相反的：
	//
	//	refunded       钱退成了 → account-service 追回这一单送出去的福卡（余额真的少掉）
	//	refund_failed  钱没退成 → account-service 解冻，卡原样放回去（余额一分不动）
	//
	// 绑错一条不会报错，只会安静地把卡扣掉或者永远留着——所以要绑就绑两条。
	//
	// 失败那条**必须发**：它是这张售后单唯一的解冻信号。少了它，用户重新申请时在途冻结
	// 已经吃光可用，新冻结行只冻得到 0 张，于是第二次退款成功时一张卡都追不回来。
	EventAfterSaleRefunded     = "order.after_sale.refunded"
	EventAfterSaleRefundFailed = "order.after_sale.refund_failed"
	eventVersion               = "v1"

	// idempotencyScopeCreate 是下单幂等键的 scope。写成常量而不是散在各处的字面量：
	// 它同时出现在「抢占」和「回填响应」两处，写歪一处就变成两个互不相认的命名空间。
	idempotencyScopeCreate = "order.create"
	// idempotencyScopeAfterSaleApply 是退款申请幂等键的 scope。与下单分开命名空间：
	// 同一个 Idempotency-Key 值在两端各自成立，不会互相顶掉。
	idempotencyScopeAfterSaleApply = "order.after_sale.apply"
)

func newEventID() string { return uuid.NewString() }

// newPickupCode 生成取杯号：取杯口屏幕上那个短号（用户侧叫取杯号、屏幕上叫取杯码）。
//
// 为什么是随机而不是从订单号派生：这个号会大字摆在取杯口的屏幕上，谁站在那儿都看得见，
// 而订单号出现在客服系统、对账文件、后台列表里。两者能互相推导就等于把「谁的单」和
// 「屏幕上的号」连了起来。它本身不是凭据——取走咖啡靠的是这一单的状态，不是念出号码。
// order_lines_pickup_code_key 唯一索引兜住碰撞，撞了就是一次 500 重试。
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
		if err := insertOrderLine(ctx, tx, id, line); err != nil {
			return nil, false, err
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

// insertOrderLine 往事务里写一行订单行。
//
// 下单（CreateOrder）与设备单（CreateDeviceOrder）共用它：两边唯一的分歧是行从哪儿算出来，
// 而「写进哪几列」必须是一个答案——两份列清单只要有一处漂移，其中一条路上的行就会静默
// 少一个字段（比如 device_id 那一列，它撑着 (device_id, device_order_no) 的唯一索引）。
//
// device_id 冗余在行上，是为了那条唯一索引：厂商单号的唯一性作用域是「同一台机器」。V2
// 一台机器服务一次下单，所以行的 device_id 就是订单上那台机器的副本。
func insertOrderLine(ctx context.Context, tx pgx.Tx, orderID string, line *OrderLineInsert) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_lines
		(order_id, line_no, line_type, item_id, item_code, item_name, item_image, quantity,
		 original_unit_price, unit_price, price_discount_amount, discount_amount,
		 payable_amount, coupon_id, coupon_discount_amount, specs, selection_snapshot,
		 campaign_id, campaign_snapshot, membership_plan_snapshot, device_id, remark)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		orderID, line.LineNo, line.LineType, line.ItemID, line.ItemCode, line.ItemName,
		line.ItemImage, line.Quantity, line.OriginalUnitPrice, line.UnitPrice,
		line.PriceDiscountAmount, line.DiscountAmount, line.PayableAmount, line.CouponID,
		line.CouponDiscountAmount, line.Specs, line.SelectionSnapshot, line.CampaignID,
		line.CampaignSnapshot, line.MembershipPlanSnapshot, line.DeviceID, line.Remark)
	return mapPGError(err)
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
	OrderNo   string
	PaymentNo string
	Amount    int64
	// PaymentMethod 是**用户选的那一种支付方式**（catalog 的 code，如 ums_h5_alipay）。
	//
	// **一个值写两处**：orders.payment_method（后台订单列表那一列显示的就是它），以及
	// order_payment_lines.line_type。从前这是两个字段——订单表存 code、出资行存另一套
	// 「出资渠道」词表，代价是加一种支付方式要同时在两套词表里找档位（支付宝在后者里没有档，
	// 只能落 `other`，于是后台把一笔支付宝单显示成「其他」）。那套词表连同
	// payments.funding_type 一列已经退场，出资行存的也是 code，见 migrations/payment 与 migrations/order。
	//
	// 所以往 order_payment_lines 插这个值时**不再有词表拦着**（那条出资渠道词表的 CHECK 已
	// 换成 line_type <> ''），但它仍然是「用户点了什么」，不是「这一笔从哪个通道出」。
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
		//
		// 补的是 PaymentMethod 本身。从前这里要另取一个字段（出资渠道词表），因为 line_type
		// 有词表而 PaymentMethod 是 catalog 的 code，插进去会撞 CHECK；那条 CHECK 已经换成
		// `line_type <> ''`（见 migrations/order），两者是同一个值了。
		//
		// **空值不兜底**：从前这里是 `if method == "" { method = "other" }`。`other` 不再是
		// 合法值，而更要紧的是——事件没带支付方式时凭空写一个，等于替用户编一句「这笔钱从哪
		// 出」。所以这一格为空就**在这里报错**，让消息进死信由人来看：钱在支付侧已经收了而
		// 我们答不出它从哪来，这正是不能让一条事件悄悄推过去的那种情况。
		//
		// 这条路径本该走不到：payments.payment_method 是 NOT NULL，支付事件里那一格
		// 取自它（见 payment-service 的 paymentEvent）。
		method := strings.TrimSpace(p.PaymentMethod)
		if method == "" {
			return nil, false, fmt.Errorf("%w: payment event carries no payment method", ErrPaymentAmountMismatch)
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
	// 行号接着已有的最大值往下排，不能从 1 重数：这张订单上可能已经躺着几行失败的尝试
	// （见 settleFailed），而「换一种支付方式重付」正是那些失败行存在的理由。从 1 重数
	// 会撞 UNIQUE(order_id, line_no)，整个落单事务回滚——而钱在支付侧已经收了，订单
	// 永远变不成 paid，重放也照撞，只能人工改数据。
	nextLineNo, err := nextPaymentLineNo(ctx, tx, order.ID)
	if err != nil {
		return nil, false, err
	}
	for i, f := range fundings {
		if _, err := tx.Exec(ctx, `INSERT INTO order_payment_lines
			(order_id, line_no, line_type, amount, status, payment_no, provider_transaction_id,
			 account_entry_id, succeeded_at)
			VALUES ($1,$2,$3,$4,'succeeded',$5,$6,$7,$8)`,
			order.ID, nextLineNo+i, f.LineType, f.Amount, f.PaymentNo, p.ProviderTransactionID,
			f.AccountEntryID, paidAt); err != nil {
			return nil, false, mapPGError(err)
		}
	}
	// 取杯号在这一刻生成：付款之前这单还没成，屏幕上也没什么可显示；付款之后才叫号。
	// 用户侧与后台都看得到（见 controller 的响应构造）。
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
	// 会员行的那份套餐快照随事件带出去：`order.paid` 是「有人买了会员并且付了钱」在会员域
	// 那一侧**唯一**的触发源（买会员是订单库里的一行，会员库没有订单表）。带的是订单行上
	// 存着的那一份**副本**，不是现查的套餐——用户买的时候套餐是什么样，他就拿到什么样。
	//
	// 没有会员行的订单不带这一段。绝大多数订单都是这样，而对会员域来说「没带」就等于
	// 「这一单不用做什么」，不是错误（见 membership-service 的 dto.OrderPaidEventPayload）。
	membership, err := membershipPlanSnapshot(ctx, tx, order.ID)
	if err != nil {
		return nil, false, err
	}
	event := map[string]any{
		"orderId": order.ID, "orderNo": order.OrderNo, "userId": order.UserID,
		"paidAmount": p.Amount, "paymentNo": p.PaymentNo, "paymentMethod": p.PaymentMethod,
		"fundings": fundings, "paidAt": paidAt.UTC().Format(time.RFC3339),
		// storeId 是这一单成交的门店，用来定会员的**归属门店**（「这个人是谁拉来的」，不参与
		// 任何金额计算）。会员域在**同一次改动里**加了镜像字段，两边的字段名必须一起动：
		// 那边用 DisallowUnknownFields 消费，先发后加会让整条 order.paid 进死信。
		"storeId": order.StoreID,
	}
	// userId 为 nil 时上面那一格会编成 JSON 的 null，不是 ""——"userId": null 是「这一单
	// 没有用户」的如实表达，空串会读成「有一个用户，他的 id 是空」。今天走不到这里：设备单
	// 建出来就是 paid，不会再有支付结果回来（见 CreateDeviceOrder）。
	if len(membership) > 0 {
		event["membership"] = json.RawMessage(membership)
	}
	if err := appendOutbox(ctx, tx, EventOrderPaid, eventVersion, p.TraceID, event); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: "paid", Applied: true}, true, nil
}

// membershipPlanSnapshot 取这张订单上会员行的那份套餐快照；没有会员行时返回 nil。
//
// 返回的是**原样的 JSON 字节**（列里存什么就带什么），不解析成结构体再编回去：那份字节是
// 下单时服务端从会员域取回来、由 dto.MembershipPlanSnapshot 编出来的，形状已经是对外的契约，
// 在这里解一遍再编一遍只会多一处能编歪的地方（比如漏掉 coupon 模式下会员域要读的两个字段）。
//
// 两种「没有」在这里合成同一个答案：这一单压根没有会员行（绝大多数订单），或者会员行上那份
// 快照是空对象（库上那列是 `NOT NULL DEFAULT '{}'`，历史数据与手写的行都可能是空的）。
// 空对象不能往外发——会员域会把一份没有 planId 的快照当成坏消息报错，而它其实是「没有」。
func membershipPlanSnapshot(ctx context.Context, tx pgx.Tx, orderID string) ([]byte, error) {
	var snapshot []byte
	err := tx.QueryRow(ctx, `SELECT membership_plan_snapshot FROM order_lines
		WHERE order_id=$1 AND line_type='membership'`, orderID).Scan(&snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPGError(err)
	}
	if len(bytes.TrimSpace(snapshot)) == 0 || bytes.Equal(bytes.TrimSpace(snapshot), []byte("{}")) {
		return nil, nil
	}
	return snapshot, nil
}

// OrderPaymentResult 是支付结果落单的结果。
type OrderPaymentResult struct {
	OrderID string
	OrderNo string
	Status  string
	// Applied=false 表示这条事件已经被处理过，调用方正常 ack 即可。
	Applied bool
}

// nextPaymentLineNo 给出这张订单上下一笔出资流水该用的行号。
//
// 出资流水的 line_no 只是一张订单内部的排序号（唯一约束 UNIQUE(order_id, line_no) 让
// 「同一单里第几笔」这件事可读），真正的业务约束是 order_payment_lines_one_live_success。
// 所以它必须**接着已有的最大值往下排**，不能各写各的：成功路径从 1 重数、失败路径从
// MAX+1 排，两边一碰就撞唯一索引，而这张表里同时存在成功行与失败行是常态——用户用微信
// 付失败、改用咖啡豆付成功，正是最常见的那条路。
//
// 唯一索引撞车在这里的后果不对称：settleFailed 与 SettlePayment 都在 FOR UPDATE 锁住的
// 订单行事务里，撞了就是整个事务回滚——钱在支付侧已经收了，本地的订单却永远停在
// pending_payment，而且这条失败行还在，事件重放也一样撞，只能人工改数据。
//
// 调用方已经持有订单行的写锁，所以 MAX+1 不会与并发的另一笔结算抢到同一个号。
func nextPaymentLineNo(ctx context.Context, tx pgx.Tx, orderID string) (int, error) {
	var next int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(line_no), 0) + 1 FROM order_payment_lines WHERE order_id=$1`,
		orderID).Scan(&next); err != nil {
		return 0, mapPGError(err)
	}
	return next, nil
}

// settleFailed 记录一次失败的支付尝试：主状态不动，只留一笔失败的出资流水与状态流水。
//
// 主状态不改是有意的：一次支付失败不等于订单作废，用户可以换一种方式再付一次
// （微信失败改用咖啡豆），订单仍是 pending_payment 直到超时或用户取消。
func settleFailed(ctx context.Context, tx pgx.Tx, order *lockedOrder, p SettlePaymentParams) error {
	// 与上面补出资行那一处同一个值、同一个取舍：line_type 存的就是支付方式 code（那套出资
	// 渠道词表已经退场，见 migrations/order），而且**空值不兜底**——编一个 line_type 出来等于替用户
	// 编一句「这笔钱从哪出」。所以拿不到方式时这一笔失败尝试不落行（见下面）。
	method := strings.TrimSpace(p.PaymentMethod)
	amount := p.Amount
	if amount <= 0 {
		// 失败事件里的金额可能是 0（渠道还没扣款就拒了），但 order_payment_lines.amount
		// 有 CHECK (amount > 0)。失败流水记的是「这次尝试想扣多少」，用订单应付额最诚实。
		amount = order.PayableAmount
		if amount <= 0 {
			amount = 1
		}
	}
	// 没有支付方式时不落这一行（见上面 method 那段）。失败事件照发：这次尝试失败是订单侧
	// 的事实，不因为出资行补不出来就不告诉下游。这条路径本该走不到——payments.payment_method
	// 是 NOT NULL，正常发不出不带方式的事件。
	if method != "" {
		lineNo, err := nextPaymentLineNo(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		var lineID string
		if err := tx.QueryRow(ctx, `INSERT INTO order_payment_lines
			(order_id, line_no, line_type, amount, status, payment_no, provider_transaction_id, failure_code)
			VALUES ($1,$7,$2,$3,'failed',$4,$5,$6)
			RETURNING id::text`, order.ID, method, amount, p.PaymentNo, p.ProviderTransactionID, p.FailureCode, lineNo).Scan(&lineID); err != nil {
			return mapPGError(err)
		}
		if err := recordTransition(ctx, tx, "payment_line", lineID, "", "failed", p.FailureMessage, p.RequestID, "system", nil, nil); err != nil {
			return err
		}
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
	ID      string
	OrderNo string
	// UserID 可空：设备单没有用户（见 migrations/order 与 model.Order.UserID）。这里的每一处
	// 用到它的判定都必须把 nil 当成「谁都不是」——nil 与任何调用方的 id 都不相等。
	UserID *string
	// StoreID 可空，也是要随 order.paid 发出去的：它是会员域**归属门店**的来源
	// （见 membership-service 的 model.Membership.StoreID）。nil 时编成 JSON 的 null。
	StoreID              *string
	Status               string
	PayableAmount        int64
	PaidAmount           int64
	RefundedAmount       int64
	FortuneCardsExpected int
	// PaymentNo 是这一单的支付单（payment-service 的值引用），空串表示根本没有——
	// 设备单就是这样，而退款那条链要拿它当钥匙（见 ApplyAfterSale 里的收窄与
	// ErrOrderHasNoPayment）。
	PaymentNo string
	// FortuneCardSnapshot 是承诺福卡的构成快照（调用方给的 JSON，形状见 order_event.go）。
	// 标记完成要用它把承诺拆成流水条目，而拆的结果要写进事件——所以它必须在锁内读到，
	// 与那一行订单同属一个时刻。
	FortuneCardSnapshot []byte
}

// user_id 可空，扫进 *string（见 lockedOrder.UserID）。
const lockedOrderColumns = `id::text, order_no, user_id::text, status, payable_amount,
	paid_amount, refunded_amount, fortune_cards_expected, fortune_card_snapshot, payment_no, store_id::text`

func scanLockedOrder(row scanner) (*lockedOrder, error) {
	order := &lockedOrder{}
	err := row.Scan(&order.ID, &order.OrderNo, &order.UserID, &order.Status,
		&order.PayableAmount, &order.PaidAmount, &order.RefundedAmount, &order.FortuneCardsExpected,
		&order.FortuneCardSnapshot, &order.PaymentNo, &order.StoreID)
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

// CompleteOrderParams 是标记完成的输入。
type CompleteOrderParams struct {
	OrderID string
	TraceID string
	// ActorType 与 ActorID 写进状态流水。今天只有后台一个来源，所以没有 CancelOrder
	// 那样的 Audit 开关：解除这个方法的唯一调用方就是一次人工干预，必审（方案 11.6）。
	// 履约完成事件接上来时另开一条路，那条路不带 actor——它不是人做的。
	ActorType string
	ActorID   *string
}

// CompleteOrder 把一笔已付款的订单标记为完成，并在同一个事务里发出 order.completed。
//
// 只有 paid 能完成：没付钱的没什么可完成，已完成的重放要挡（同一单发两次福卡），
// 已取消/已退款的更不该被推着往前走。越界的状态用 ErrOrderNotCompletable 报出去，
// controller 翻成 409，而不是 500。
//
// 事件在同一个事务里追加：**状态改了而事件丢了**，订单就成了「完成了但福卡永远不发」，
// 而不发福卡这件事没有任何人会察觉——那正是 outbox 存在的理由，不是「改完再发」。
func (r *PostgresRepository) CompleteOrder(ctx context.Context, p CompleteOrderParams) (*OrderPaymentResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	order, err := lockedOrderByID(ctx, tx, p.OrderID)
	if err != nil {
		return nil, err
	}
	if order.Status != model.OrderStatusPaid {
		// 判据是订单上的事实而不是请求，所以不在 service.ValidationErrors 里。
		return nil, ErrOrderNotCompletable
	}
	// finished_at 由数据库给，并把落库后的那个值读回来：事件的流水时间必须与订单上写的
	// 是同一个时刻，否则「订单完成于 X」会有两个说法（见 finished_at 那一列的用法）。
	var finishedAt time.Time
	if err := tx.QueryRow(ctx, `UPDATE orders
		SET status='completed', finished_at=NOW(), updated_at=NOW()
		WHERE id=$1 RETURNING finished_at`, order.ID).Scan(&finishedAt); err != nil {
		return nil, mapPGError(err)
	}
	if err := recordTransition(ctx, tx, "order", order.ID, order.Status, "completed", "", p.TraceID, p.ActorType, p.ActorID, nil); err != nil {
		return nil, err
	}
	grants := fortuneCardGrants(order.ID, order.FortuneCardsExpected, order.FortuneCardSnapshot)
	if err := appendOutbox(ctx, tx, EventOrderCompleted, eventVersion, p.TraceID, dto.OrderCompletedEventPayload{
		OrderNo: order.OrderNo,
		OrderID: order.ID,
		// 设备单没有用户，这一格就是空串（账户域按 userId 记账，空用户意味着「没有人的
		// 资产要动」——而设备单的 fortune_cards_expected 恒为 0，本来也没有要动的）。
		UserID:         derefString(order.UserID),
		FinishedAtUnix: finishedAt.Unix(),
		FortuneCards:   grants,
	}); err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "orders", Action: "complete", Operation: "标记完成",
		TargetType: "order", TargetID: order.ID, TargetName: order.OrderNo,
		Before: audit.Snapshot(map[string]any{"status": order.Status}),
		// 把发放的张数写进审计：这一栏是事后追「这几张福卡是谁放出去的」时唯一的入口
		// （账户侧的流水只说「订单完成赠送」，不说谁点的完成）。
		After: audit.Snapshot(map[string]any{
			"status":              "completed",
			"fortuneCardsGranted": grantedTotal(grants),
		}),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &OrderPaymentResult{OrderID: order.ID, OrderNo: order.OrderNo, Status: "completed", Applied: true}, nil
}

// derefString 把可空字符串读成空串。
//
// 只在事件体那种「字段是 string、而值可能没有」的地方用：编出来的字节里空串与 null 对
// 消费方是一回事（Go 的 encoding/json 把 null 解进 string 字段时不报错、留空值），而事件
// 形状是跨服务的契约，不因为某条路上没有用户就换一个类型。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// grantedTotal 是一次发放的总张数，只用于审计那一栏。
func grantedTotal(grants []dto.OrderFortuneGrant) int64 {
	var total int64
	for _, grant := range grants {
		total += grant.Amount
	}
	return total
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
	// userID 是 *string：设备单没有用户（migrations/order）。今天扫不到它——设备单一建出来就是
	// paid，永远不会出现在「到点未支付」这一批里——但列可空，扫进 string 会在某天后端补
	// 一条数据时变成一个 500，而不是一条带 null 的事件。
	type expired struct {
		id, orderNo string
		userID      *string
	}
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
