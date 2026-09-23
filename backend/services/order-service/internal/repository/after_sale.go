package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// —— 售后专用的哨兵错误 ——
//
// 与 postgres.go 里那几个同一个理由：一个说得清楚的业务冲突要有一个调用方分得清的名字，
// 否则「这一行已经退过款了」会变成 500「服务器错误」。
//
// 它们都定义在仓储而不是 service，是因为**判据全都在锁内的事实上**：这张单能不能退、
// 这一行属于谁、有没有别人在退同一笔钱——这些只有把行锁住之后读到的才算数。service
// 只负责请求形状（见 service/after_sale.go），锁内的事实由这里给出。
var (
	// ErrOrderNotRefundable：订单不在可退的状态（没付钱、已取消、已经在退、退完了）。
	ErrOrderNotRefundable = errors.New("order is not refundable")
	// ErrAfterSaleNotFound：售后单不存在，或者不属于这个用户。
	// 后一种与前者共用一个错误是有意的：回「无权限」等于告诉调用方这个单号是真的。
	ErrAfterSaleNotFound = errors.New("after sale not found")
	// ErrAfterSaleNotPending：这张售后单已经不在待处理状态（审过了、撤销过了）。
	ErrAfterSaleNotPending = errors.New("after sale is not awaiting review")
	// ErrAfterSaleAlreadyOpen：这一行（或整张单）已经有一张还没结束的售后单。
	ErrAfterSaleAlreadyOpen = errors.New("an after sale on this order is still open")
	// ErrAfterSaleAlreadyRefunded：这一行已经退过款了。
	ErrAfterSaleAlreadyRefunded = errors.New("this order line has already been refunded")
	// ErrAfterSaleLineMismatch：指的那一行不属于这张订单。
	ErrAfterSaleLineMismatch = errors.New("the order line does not belong to this order")
	// ErrAfterSaleNothingToRefund：算下来没有可退的金额（没付过，或者已经退完了）。
	ErrAfterSaleNothingToRefund = errors.New("no refundable amount left on this order")
	// ErrAfterSaleExceedsRefundable：按行退的金额超过了这张订单的剩余可退额。
	ErrAfterSaleExceedsRefundable = errors.New("refund amount exceeds the remaining refundable amount")
	// ErrFortuneCardConfirmationRequired：这一单承诺过福卡，审核通过前必须显式确认
	// 「赠送的福卡没有参与过抽奖」（规则见 dto.ReviewAfterSaleRequest 的注释）。
	ErrFortuneCardConfirmationRequired = errors.New("fortune card confirmation is required to approve")
	// ErrAfterSaleNotApproved：这张售后单还没被审核通过，不能发起退款。
	//
	// 它与 ErrAfterSaleNotPending 是两个结论而不是一个：后者是「审核窗口已经关了」
	// （驳回过、撤销过、已经在退），前者是「还没人批过」。重试出口（StartRefund）只认
	// approved，所以这两句提示指向的下一步动作完全不同。
	ErrAfterSaleNotApproved = errors.New("after sale is not approved for refund")
	// ErrAfterSaleNotRefunding：这张售后单不在退款推进中（还没发起，或者已经有结论了）。
	ErrAfterSaleNotRefunding = errors.New("after sale is not awaiting a refund result")
	// ErrAfterSaleRefundMismatch：收到的退款单号与这张售后单上记的那一个对不上。
	//
	// 它是**要人来看**的信号，不能当成重放忽略掉：售后单上已经记着 A 号退款单，事件却说
	// 退的是 B 号——那说明要么这张单被换过（不该发生，退款单号只写一次），要么这条事件
	// 说的是另一张售后单的钱。两种都只能人工追查。
	ErrAfterSaleRefundMismatch = errors.New("refund number does not match the after sale")
	// ErrOrderHasNoPayment：这一单没有支付单，退款这条链在结构上装不下它。
	//
	// 判据是 orders.payment_no 为空：线下刷卡机与取货码那两类设备单**根本没有 payments 行**
	// （钱在机器那边就收过了，见 migrations/order），而 payment-service 的
	// payment_refunds.payment_id 是 NOT NULL REFERENCES payments(id)。放它过去的结果是
	// 审核通过之后在建退款单那一步炸掉，而那时钱已经承诺要退了。
	ErrOrderHasNoPayment = errors.New("order has no payment to refund against")
)

// afterSaleColumns 是 order_after_sales 的读取列。
//
// 带 a. 前缀不是风格问题：读售后单的查询基本上都要 JOIN orders（福卡承诺）与 order_lines
// （退的是哪一行），而 orders 也有 id/status/created_at，不加前缀就会撞成 ambiguous。
// 列顺序与 scanAfterSale 的扫描顺序严格一一对应，两边必须一起改。
const afterSaleColumns = `a.id::text, a.legacy_id, a.after_sale_no, a.order_id::text, a.order_no,
	a.user_id::text, a.type, a.scope, a.order_line_id::text, a.status, a.reason, a.images,
	a.refund_amount, a.refund_no, a.failure_code, a.failure_message, a.reviewed_by::text,
	a.reviewed_at, a.review_remark, a.created_at, a.updated_at, a.refunded_at`

// afterSaleDerivedColumns 是审核与退款要看、但不属于售后单本身的那几列（跟在 afterSaleColumns 后面）。
//
// 行的列用 COALESCE 抹平成零值而不是扫成指针：整单退（order_line_id 为空）时它们全是
// NULL，而「零值的行摘要」与「没有行摘要」在 Go 侧是一件事——用 COALESCE 就不用在每个
// 字段上判一次 nil。
//
// o.payment_no 是**要退的那张支付单**。它必须跟着售后单一起读出来，而不是等到发起退款时
// 再查一次订单：审核通过之后紧接着就要拿它去调支付域，中间隔一次查询就有一个窗口能读到
// 一张改过的订单。而且它是 ApplyAfterSale 收窄入口的判据（见那里的 ErrOrderHasNoPayment），
// 两条路读的是同一列，判定与使用就不会分叉。
const afterSaleDerivedColumns = `o.fortune_cards_expected, o.fortune_card_snapshot,
	COALESCE(o.payment_no, ''),
	COALESCE(l.id::text, ''), COALESCE(l.line_no, 0), COALESCE(l.line_type, ''),
	COALESCE(l.item_name, ''), COALESCE(l.quantity, 0), COALESCE(l.payable_amount, 0)`

const afterSaleFrom = ` FROM order_after_sales a
	JOIN orders o ON o.id = a.order_id
	LEFT JOIN order_lines l ON l.id = a.order_line_id`

// AfterSaleRow 是售后单加上「审核要看的三样派生事实」。
type AfterSaleRow struct {
	AfterSale *model.OrderAfterSale
	// 订单承诺赠送的福卡张数与构成快照。福卡的发放与消耗不在这两个服务里（归
	// account-service），本库有的只是**承诺**——福卡规则要靠它判断该不该放行。
	FortuneCardsExpected int
	FortuneCardSnapshot  []byte
	// 按行退时被退的那一行；整单退为 nil。
	OrderLine *AfterSaleLineBrief
	// PaymentNo 是要退的那张支付单（orders.payment_no），空串表示这一单没有支付单
	// ——设备单就是这样，而它在 ApplyAfterSale 那里就被拦掉了。
	//
	// 它是审核通过之后交给支付域的那把钥匙（client.CreateRefundInput.PaymentNo）。
	PaymentNo string
}

// AfterSaleLineBrief 是「退的是哪一行」的摘要，给客服对账用。
type AfterSaleLineBrief struct {
	ID            string
	LineNo        int
	LineType      string
	ItemName      string
	Quantity      int
	PayableAmount int64
}

func scanAfterSaleWith(row scanner, extra ...any) (*model.OrderAfterSale, error) {
	item := &model.OrderAfterSale{}
	targets := []any{&item.ID, &item.LegacyID, &item.AfterSaleNo, &item.OrderID, &item.OrderNo,
		&item.UserID, &item.Type, &item.Scope, &item.OrderLineID, &item.Status, &item.Reason,
		&item.Images, &item.RefundAmount, &item.RefundNo, &item.FailureCode, &item.FailureMessage,
		&item.ReviewedBy, &item.ReviewedAt, &item.ReviewRemark, &item.CreatedAt, &item.UpdatedAt,
		&item.RefundedAt}
	targets = append(targets, extra...)
	if err := row.Scan(targets...); err != nil {
		return nil, err
	}
	return item, nil
}

func scanAfterSale(row scanner) (*model.OrderAfterSale, error) { return scanAfterSaleWith(row) }

// scanAfterSaleRow 按 afterSaleColumns + afterSaleDerivedColumns 的顺序扫一行。
func scanAfterSaleRow(row scanner) (*AfterSaleRow, error) {
	result := &AfterSaleRow{}
	var lineID, lineType, itemName string
	var lineNo, quantity int
	var payableAmount int64
	item, err := scanAfterSaleWith(row, &result.FortuneCardsExpected, &result.FortuneCardSnapshot,
		&result.PaymentNo, &lineID, &lineNo, &lineType, &itemName, &quantity, &payableAmount)
	if err != nil {
		return nil, err
	}
	result.AfterSale = item
	if lineID != "" {
		result.OrderLine = &AfterSaleLineBrief{
			ID: lineID, LineNo: lineNo, LineType: lineType,
			ItemName: itemName, Quantity: quantity, PayableAmount: payableAmount,
		}
	}
	return result, nil
}

// mapAfterSaleError 把「查不到售后单」翻成售后自己的错误。
//
// 不能直接用 mapPGError：那个把 ErrNoRows 翻成 ErrOrderNotFound，而一张不存在的售后单
// 报成「订单不存在」会把调用方指向一个查错方向。
func mapAfterSaleError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAfterSaleNotFound
	}
	return mapPGError(err)
}

func lockedAfterSaleByNo(ctx context.Context, tx pgx.Tx, afterSaleNo string) (*model.OrderAfterSale, error) {
	item, err := scanAfterSale(tx.QueryRow(ctx,
		`SELECT `+afterSaleColumns+` FROM order_after_sales a WHERE a.after_sale_no=$1 FOR UPDATE`,
		afterSaleNo))
	if err != nil {
		return nil, mapAfterSaleError(err)
	}
	return item, nil
}

// afterSaleRowByNo 读一张售后单的完整形状（含派生列）。写路径在事务里用它回填响应，
// 保证响应里的就是刚刚提交的那个状态。
func afterSaleRowByNo(ctx context.Context, tx pgx.Tx, afterSaleNo string) (*AfterSaleRow, error) {
	row, err := scanAfterSaleRow(tx.QueryRow(ctx,
		`SELECT `+afterSaleColumns+`, `+afterSaleDerivedColumns+afterSaleFrom+` WHERE a.after_sale_no=$1`,
		afterSaleNo))
	if err != nil {
		return nil, mapAfterSaleError(err)
	}
	return row, nil
}

// afterSaleOccupancy 是这张订单上已经存在的、会挡住新申请的售后单。
//
// 未结束的与已退款成功的一次读出来：两者都只在这张订单的售后单里筛，分开查两边一样贵，
// 而分成两处判断就会有人只改其中一处。
type afterSaleOccupancy struct {
	// OpenAmount 是未结束的售后单承诺要退的金额之和。整单退要减掉它，按行退要拿它算
	// 剩余额度——不减就会把同一笔钱退两次。
	OpenAmount int64
	// OpenLineIDs 是未结束的按行售后单退的那几行；OpenWholeOrder 表示有一张未结束的整单退。
	OpenLineIDs    map[string]bool
	OpenWholeOrder bool
	// RefundedLineIDs 是已经退款成功的那几行：钱已经出去了，不能再退第二次。
	// 它们不占用互斥位（那张单已经结束了）。
	RefundedLineIDs map[string]bool
}

// blocks 判断这次的申请会不会与已有的售后单撞上。
//
// 三条规则，各自对应一类「同一笔钱退两次」：
//   - 整单退覆盖所有行，所以它与这张单上**任何**未结束的售后单互斥；
//   - 按行退与同一行的未结束单互斥，也与未结束的整单退互斥；
//   - 已经退款成功的那一行不能再退第二次。被驳回、已撤销、退款失败的不算——那三种
//     情况下钱没出去，用户本来就该能再申请一次。
func (o *afterSaleOccupancy) blocks(scope, lineID string) error {
	if scope == model.AfterSaleScopeAll {
		if o.OpenWholeOrder || len(o.OpenLineIDs) > 0 {
			return ErrAfterSaleAlreadyOpen
		}
		return nil
	}
	if o.OpenWholeOrder || o.OpenLineIDs[lineID] {
		return ErrAfterSaleAlreadyOpen
	}
	if o.RefundedLineIDs[lineID] {
		return ErrAfterSaleAlreadyRefunded
	}
	return nil
}

// loadAfterSaleOccupancy 读这张订单上还占着位置的售后单。必须在锁住订单那一行之后调用。
func loadAfterSaleOccupancy(ctx context.Context, tx pgx.Tx, orderID string) (*afterSaleOccupancy, error) {
	statuses := make([]string, 0, len(model.AfterSaleOpenStatuses)+1)
	statuses = append(statuses, model.AfterSaleOpenStatuses...)
	statuses = append(statuses, model.AfterSaleStatusRefunded)

	rows, err := tx.Query(ctx, `SELECT status, scope, COALESCE(order_line_id::text, ''), refund_amount
		FROM order_after_sales
		WHERE order_id = $1 AND status = ANY($2::text[])`, orderID, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	occupancy := &afterSaleOccupancy{
		OpenLineIDs:     map[string]bool{},
		RefundedLineIDs: map[string]bool{},
	}
	for rows.Next() {
		var status, scope, lineID string
		var amount int64
		if err := rows.Scan(&status, &scope, &lineID, &amount); err != nil {
			return nil, err
		}
		if status == model.AfterSaleStatusRefunded {
			if lineID != "" {
				occupancy.RefundedLineIDs[lineID] = true
			}
			continue
		}
		occupancy.OpenAmount += amount
		if scope == model.AfterSaleScopeAll {
			occupancy.OpenWholeOrder = true
		} else if lineID != "" {
			occupancy.OpenLineIDs[lineID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return occupancy, nil
}

// ApplyAfterSaleParams 是一次退款申请要落进库的全部事实。
//
// 金额不在这里：它由仓储在事务里算（要看得见订单的已付/已退与未结束售后单占用的额度，
// 锁外算出来的数字在并发下就是错的）。
type ApplyAfterSaleParams struct {
	IdempotencyKey string
	TraceID        string
	RequestID      string
	OrderID        string
	// UserID 来自令牌：非空表示用户退自己的单，仓储在锁住订单那一行之后再对一次归属。
	UserID      string
	AfterSaleNo string
	Scope       string
	OrderLineID *string
	Reason      string
	// Images 是已经序列化好的 JSON 数组（形状由 service 校过）。与 OrderInsert 的快照列
	// 同一个约定：仓储不解释内容，只负责把它写进 JSONB。
	Images    []byte
	ActorType string
	ActorID   *string
}

// ApplyAfterSaleResult 是写进幂等表的那份快照：它回答「这个幂等键产出的是哪一张售后单」，
// 重放时靠它找到那张单，而不是靠调用方再传一次单号。
//
// 它不是返回给调用方的形状——申请书、撤销、审核三条路都返回 AfterSaleRow，外面只有一个
// 售后单的对外形状（controller 的 afterSaleView）。这里不是那个形状，所以没有理由长得像它。
type ApplyAfterSaleResult struct {
	AfterSaleID  string    `json:"afterSaleId"`
	AfterSaleNo  string    `json:"afterSaleNo"`
	OrderID      string    `json:"orderId"`
	OrderNo      string    `json:"orderNo"`
	Status       string    `json:"status"`
	Scope        string    `json:"scope"`
	OrderLineID  *string   `json:"orderLineId"`
	RefundAmount int64     `json:"refundAmount"`
	CreatedAt    time.Time `json:"createdAt"`
}

// FortuneCardFreezeGate 是受理退款申请前的一次**便宜的先看一眼**：这一单这次退款范围要冻住
// 哪几笔发放、一共几张。
//
// 它不开事务、不加锁，读的是一份**会变**的快照。这不是 Authority：权威的那一次判在
// ApplyAfterSale 的锁内（它自己重读订单、重拆一遍键）。这里存在的唯一理由是那条业务规则
// 要先**问一次账户域**（一单赠送的福卡一张都没被用过才允许申请），而账户域是一条网络
// 调用——把它放进锁里等于握着一行订单的锁去等一个下游。
//
// checkable=false 是「这个判断在这里做不了，也别拿它去回一句话」：订单不存在、不是本人的、
// 状态不可退，三种情形都由 ApplyAfterSale 给出权威答复（而且说的是各自真正的原因）。
// 少了这个闸，一张还没付款的订单来申请退款会收到「福卡已使用」——一句不相干的冤枉话。
func (r *PostgresRepository) FortuneCardFreezeGate(ctx context.Context, orderID, userID, scope string) (FortuneCardFreezePlan, bool, error) {
	var (
		owner    *string
		status   string
		expected int
		raw      []byte
	)
	err := r.pool.QueryRow(ctx, `SELECT user_id::text, status, fortune_cards_expected, fortune_card_snapshot
		FROM orders WHERE id = NULLIF($1,'')::uuid`, orderID).Scan(&owner, &status, &expected, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return FortuneCardFreezePlan{}, false, nil
	}
	if err != nil {
		return FortuneCardFreezePlan{}, false, mapPGError(err)
	}
	// 与锁内那两判同一套条件（见 ApplyAfterSale）：归属回「不存在」，状态只认 paid / completed。
	if userID != "" && (owner == nil || *owner != userID) {
		return FortuneCardFreezePlan{}, false, nil
	}
	if status != model.OrderStatusPaid && status != model.OrderStatusCompleted {
		return FortuneCardFreezePlan{}, false, nil
	}
	return fortuneCardFreezePlan(orderID, expected, raw, scope), true, nil
}

// ApplyAfterSale 在一个事务里落一张售后申请、状态流水与 order.after_sale.applied 事件。
//
// 锁住订单那一行是这里的关键：它同时是两件事的支点——归属（user_id 与订单对得上）与
// 互斥（同一行/整单有没有别的未结束的售后单）。两者都在锁外判完再写的话，两个同时到达
// 的申请会各自算出「没有冲突」，然后一起落库，同一笔钱就有了两张要退的单。
func (r *PostgresRepository) ApplyAfterSale(ctx context.Context, p ApplyAfterSaleParams) (*AfterSaleRow, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	hash := requestHash(map[string]any{
		"order_id": p.OrderID, "user_id": p.UserID, "scope": p.Scope,
		"order_line_id": p.OrderLineID, "reason": p.Reason, "images": string(p.Images),
	})
	response, done, err := beginIdempotentOperation(ctx, tx, idempotencyScopeAfterSaleApply, p.IdempotencyKey, hash, "after_sale", "")
	if err != nil {
		return nil, false, err
	}
	if done {
		// 重放：这次调用什么都没写。回的是那张单**现在**的样子，不是当初存下来的快照——
		// 幂等键保证的是「不会再落第二张」，不是「把调用方按回提交那一刻」。一张已经审核过
		// 的单，重放时告诉调用方它还是 pending 才是真的错。
		var stored ApplyAfterSaleResult
		if err := json.Unmarshal(response, &stored); err != nil {
			return nil, false, err
		}
		row, err := afterSaleRowByNo(ctx, tx, stored.AfterSaleNo)
		if err != nil {
			return nil, false, err
		}
		return row, true, tx.Commit(ctx)
	}

	order, err := lockedOrderByID(ctx, tx, p.OrderID)
	if err != nil {
		return nil, false, err
	}
	// user_id 可空（设备单没有用户，见 migrations/order），nil 与任何调用方都不相等：一张没有主人的
	// 单不能被谁认领成自己的。设备单也本来就走不到这里——它建出来就是 paid，而申请售后的
	// 那条路是用户拿自己的单来退钱，机器卖出去的那一杯没有对应的用户可退。
	if p.UserID != "" && (order.UserID == nil || *order.UserID != p.UserID) {
		// 别人的单回「不存在」而不是「无权限」：403 等于告诉调用方这个 id 是真的。
		return nil, false, ErrOrderNotFound
	}
	if order.Status != model.OrderStatusPaid && order.Status != model.OrderStatusCompleted {
		return nil, false, fmt.Errorf("%w: order %s is %s", ErrOrderNotRefundable, order.OrderNo, order.Status)
	}
	// 没有支付单的单**在入口就收窄**，不留到发起退款那一步才炸。
	//
	// 判据是 orders.payment_no 为空：线下刷卡机与取货码那两类设备单的钱在机器上就收过了，
	// 它们没有 payments 行，而 payment-service 的 payment_refunds.payment_id 是
	// NOT NULL REFERENCES payments(id)——这一条链**结构上**装不下它们。真让它们走到审核
	// 通过，用户拿到的是一句「已同意退款」，而钱永远退不出去，只能线下补。
	//
	// 设备余额怎么退是设备域的事（要退到咖啡卡里还是原路返回，规则还没有），不是这里
	// 补一个分支能解决的，所以这里只负责说清楚「这一单不在这条链上」。
	if strings.TrimSpace(order.PaymentNo) == "" {
		return nil, false, fmt.Errorf("%w: order %s has no payment", ErrOrderHasNoPayment, order.OrderNo)
	}

	lineID := ""
	if p.OrderLineID != nil {
		lineID = *p.OrderLineID
	}
	var amount int64
	if p.Scope != model.AfterSaleScopeAll {
		// 按行退的金额是这一行的应付额。两件事在这里一起判，都是外键看不到的：
		// 「这一行是不是这一单的」，以及「这一行的类型是不是 scope 说的那个」。
		// 后者不能省：退款链的下游（§7.4）读的是 scope，scope=drink 指着一条 addon 行
		// 会造出一条自己就在说谎的售后单，而金额是对的、约束也是过的，没有谁会拦它。
		var lineType string
		err := tx.QueryRow(ctx, `SELECT payable_amount, line_type FROM order_lines
			WHERE id=NULLIF($1,'')::uuid AND order_id=$2`, lineID, order.ID).Scan(&amount, &lineType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrAfterSaleLineMismatch
		}
		if err != nil {
			return nil, false, err
		}
		if lineType != p.Scope {
			return nil, false, fmt.Errorf("%w: scope=%s line %s is %s", ErrAfterSaleLineMismatch, p.Scope, lineID, lineType)
		}
	}

	occupancy, err := loadAfterSaleOccupancy(ctx, tx, order.ID)
	if err != nil {
		return nil, false, err
	}
	if err := occupancy.blocks(p.Scope, lineID); err != nil {
		return nil, false, err
	}

	remaining := order.PaidAmount - order.RefundedAmount - occupancy.OpenAmount
	if p.Scope == model.AfterSaleScopeAll {
		amount = remaining
	}
	if amount <= 0 {
		return nil, false, fmt.Errorf("%w: order %s", ErrAfterSaleNothingToRefund, order.OrderNo)
	}
	if amount > remaining {
		// 按行退的金额（这一行的应付额）与「这张单还能退多少」是两件事：部分支付、或者
		// 同一单的别的行已经占掉额度时，两者会不一致。不一致就拒，不裁——把差额悄悄
		// 退出去，比一次 400 贵得多。
		return nil, false, fmt.Errorf("%w: line=%d remaining=%d", ErrAfterSaleExceedsRefundable, amount, remaining)
	}

	var id string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO order_after_sales
		(after_sale_no, order_id, order_no, user_id, type, scope, order_line_id, status, reason, images, refund_amount)
		VALUES ($1,$2,$3,$4,'refund',$5,NULLIF($6,'')::uuid,'pending',$7,$8,$9)
		RETURNING id::text, created_at`,
		p.AfterSaleNo, order.ID, order.OrderNo, order.UserID, p.Scope, lineID, p.Reason, p.Images, amount).
		Scan(&id, &createdAt); err != nil {
		return nil, false, mapPGError(err)
	}

	if err := recordTransition(ctx, tx, model.AggregateAfterSale, id, "", model.AfterSaleStatusPending,
		p.Reason, p.RequestID, p.ActorType, p.ActorID, map[string]any{
			"scope": p.Scope, "refundAmount": amount,
		}); err != nil {
		return nil, false, err
	}
	// 福卡冻结要冻哪几笔发放，在这里（锁内、与那一行订单同一个时刻）拆出来随事件带走：
	// 承诺快照是订单的事实，账户域不解析它、也不持有发放规则（方案 5.6）。按 scope 分层，
	// 退加购行只冻加赠那张；快照拆不动时整单全冻，理由见 fortuneCardFreezeKeys。
	//
	// 这一单没承诺福卡时是空数组，不是缺字段：消费方开着 DisallowUnknownFields，
	// 少一个字段整条消息就进死信——退款申请跟着一起丢，那比不冻卡严重得多。
	//
	// 事件体用 dto 里那个结构体而不是内联的 map：字段名只有一处定义，改名字编译器会拦
	// （account-service 那份镜像也跟着改），而 map 的键是字符串，删掉一个键谁也不会发现。
	freezeKeys := fortuneCardFreezeKeys(order.ID, order.FortuneCardsExpected, order.FortuneCardSnapshot, p.Scope)
	if err := appendOutbox(ctx, tx, EventAfterSaleApplied, eventVersion, p.TraceID,
		dto.AfterSaleAppliedEventPayload{
			AfterSaleID: id, AfterSaleNo: p.AfterSaleNo, OrderID: order.ID,
			// 订单的 user_id 可空（设备单没有用户），事件体里是 string：与
			// OrderCompletedEventPayload 同一处取舍（见 order.go 那个 derefString），
			// 空指针发出去是空串。消费方拿它 uuid.Parse，两种都过不了，结论一样。
			OrderNo: order.OrderNo, UserID: derefString(order.UserID), Scope: p.Scope,
			OrderLineID: p.OrderLineID, RefundAmount: amount,
			FortuneCardEntryKeys: freezeKeys,
		}); err != nil {
		return nil, false, err
	}

	result := &ApplyAfterSaleResult{
		AfterSaleID: id, AfterSaleNo: p.AfterSaleNo, OrderID: order.ID, OrderNo: order.OrderNo,
		Status: model.AfterSaleStatusPending, Scope: p.Scope, OrderLineID: p.OrderLineID,
		RefundAmount: amount, CreatedAt: createdAt,
	}
	if err := completeIdempotentOperation(ctx, tx, idempotencyScopeAfterSaleApply, p.IdempotencyKey, id, result); err != nil {
		return nil, false, err
	}
	// 落库后按唯一的对外形状读回来，而不是拿手上这几个字段现拼一个：申请书、撤销、
	// 审核三条路各自拼一份的话，福卡快照、行摘要这些派生列迟早会在其中一份里漏掉。
	row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return row, false, nil
}

// ReviewAfterSaleParams 是一次审核的输入。
type ReviewAfterSaleParams struct {
	AfterSaleNo string
	// Action 取 model.AfterSaleActionApprove / AfterSaleActionReject。
	Action string
	Remark string
	// FortuneCardUnusedConfirmed：审核人已确认本单赠送的福卡未参与抽奖。
	FortuneCardUnusedConfirmed bool
	// ReviewedBy 是审核人（来自令牌，不来自请求体——让被审计的人自己填审核人等于没有审核）。
	ReviewedBy string
	RequestID  string
	TraceID    string
}

// ReviewAfterSale 审核一张售后单：通过或驳回。
//
// 通过只是「同意退这笔钱」，在这一步**不动订单主状态**：orders.status 上的 refunding 含义是
// 「退款单已经建了」，而建单是紧接着的另一次推进（startRefundTx，approved → refunding），
// 由本服务向 payment-service 发起退款时做；失败时 AdvanceRefund 按那里记下的原状态把订单
// 放回去，所以退款链上的订单不会停在 refunding。
//
// 这条事件因此只承载「同意退」这一个语义：账户域拿它只做一件事——驳回 ⇒ 解冻福卡，通过
// 什么都不做（钱还没出去）。卡与豆都等退款结果那条事件（order.after_sale.refunded /
// .refund_failed）。
func (r *PostgresRepository) ReviewAfterSale(ctx context.Context, p ReviewAfterSaleParams) (*AfterSaleRow, error) {
	target := model.AfterSaleStatusApproved
	operation := "通过退款申请"
	reason := "审核通过"
	if p.Action == model.AfterSaleActionReject {
		target = model.AfterSaleStatusRejected
		operation = "驳回退款申请"
		reason = p.Remark
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	item, err := lockedAfterSaleByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, err
	}
	if item.Status != model.AfterSaleStatusPending {
		return nil, fmt.Errorf("%w: after sale %s is %s", ErrAfterSaleNotPending, item.AfterSaleNo, item.Status)
	}

	// 福卡规则：这一单承诺过福卡时，必须有人显式确认「赠送的福卡没有参与过抽奖」，
	// 否则整笔不可退。判定和状态判定一起放在锁内——它是这次审核的一部分，不是请求的形状。
	//
	// 为什么是人工确认：order 库只有「承诺发几张」（orders.fortune_cards_expected），
	// 福卡有没有发出去、有没有用去抽奖，在 account-service 与 lottery-service 里，两个
	// 服务都还没建。判不了就不能静默放行，所以做到「没有确认一律不放行」为止。
	// lottery-service 落地后这里换成一次实时查询，字段与行为不变（人核 → 机器核）。
	if target == model.AfterSaleStatusApproved {
		var expected int
		if err := tx.QueryRow(ctx, `SELECT fortune_cards_expected FROM orders WHERE id=$1`,
			item.OrderID).Scan(&expected); err != nil {
			return nil, mapPGError(err)
		}
		if expected > 0 && !p.FortuneCardUnusedConfirmed {
			return nil, fmt.Errorf("%w: order %s promised %d fortune cards",
				ErrFortuneCardConfirmationRequired, item.OrderNo, expected)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE order_after_sales
		SET status=$2, reviewed_by=NULLIF($3,'')::uuid, reviewed_at=NOW(), review_remark=$4, updated_at=NOW()
		WHERE id=$1`, item.ID, target, p.ReviewedBy, p.Remark); err != nil {
		return nil, mapPGError(err)
	}

	reviewer := p.ReviewedBy
	if err := recordTransition(ctx, tx, model.AggregateAfterSale, item.ID, item.Status, target,
		reason, p.RequestID, model.ActorAdmin, &reviewer, map[string]any{
			"action": p.Action, "refundAmount": item.RefundAmount,
			// 把「有人确认过福卡没抽奖」记进流水：这一笔退款能放行，靠的就是这个确认，
			// 事后追查时必须看得到它。
			"fortuneCardUnusedConfirmed": p.FortuneCardUnusedConfirmed,
		}); err != nil {
		return nil, err
	}

	// 审核是管理员动用户资产的动作，与取消订单同一个理由要留审计：能回答「是谁在什么时候
	// 同意了这笔退款、为什么」。它在同一个事务里写进 outbox，由 relay 落到身份库。
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "after_sales", Action: p.Action, Operation: operation,
		TargetType: "after_sale", TargetID: item.ID, TargetName: item.AfterSaleNo,
		Before: audit.Snapshot(map[string]any{"status": item.Status}),
		After: audit.Snapshot(map[string]any{
			"status": target, "remark": p.Remark, "refundAmount": item.RefundAmount,
		}),
	}); err != nil {
		return nil, err
	}

	if err := appendOutbox(ctx, tx, EventAfterSaleReviewed, eventVersion, p.TraceID,
		dto.AfterSaleReviewedEventPayload{
			AfterSaleID: item.ID, AfterSaleNo: item.AfterSaleNo, OrderID: item.OrderID,
			OrderNo: item.OrderNo, UserID: item.UserID, Status: target,
			Action: p.Action, Scope: item.Scope, OrderLineID: item.OrderLineID,
			RefundAmount: item.RefundAmount,
		}); err != nil {
		return nil, err
	}

	row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// FindAfterSaleByNo 按单号读一张售后单（含派生列），不加锁。
//
// 它只服务于**重试出口**：重试要在调支付域之前先看清楚这张单现在是什么状态、退多少钱、
// 拿哪张支付单去退。判定本身不靠这次读——那三条（状态、退款单号、订单状态）全在
// StartRefund 的锁里重新判一遍。
func (r *PostgresRepository) FindAfterSaleByNo(ctx context.Context, afterSaleNo string) (*AfterSaleRow, error) {
	row, err := scanAfterSaleRow(r.pool.QueryRow(ctx,
		`SELECT `+afterSaleColumns+`, `+afterSaleDerivedColumns+afterSaleFrom+` WHERE a.after_sale_no=$1`,
		afterSaleNo))
	if err != nil {
		return nil, mapAfterSaleError(err)
	}
	return row, nil
}

// StartRefundParams 是把一张已审核通过的售后单推去退款的输入。
type StartRefundParams struct {
	AfterSaleNo string
	// RefundNo 是支付侧刚建出来的退款单号。退款单在支付域，这里只存一个值引用。
	RefundNo string
	// ActorID 是推动这次退款的后台账号：审核通过时是审核人，走重试出口时是点重试的那个人。
	// 空串表示系统推的（今天没有这条路，留一个说得通的兜底）。
	ActorID   string
	RequestID string
}

// StartRefund 把一张 approved 的售后单推进到 refunding，并把订单标成退款中。
//
// 它是**审核那条三步里的事务 B**（见 service.after_sale.go）：事务外已经拿着这张单的
// 支付单号去支付域建过退款单了，这里只负责把「建成了」这件事落下来。所以它必须在
// refund_no 已经拿到之后才调用——先写 refunding 再建退款单，失败时留下的是一张
// 「在退但支付侧什么都没有」的单，那比停在 approved 难查得多。
//
// 返回的 bool 是「这次调用什么都没写」（重放）：同一张售后单被推过两次时，
// 第二次拿回它现在的样子。
func (r *PostgresRepository) StartRefund(ctx context.Context, p StartRefundParams) (*AfterSaleRow, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	item, err := lockedAfterSaleByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, false, err
	}
	// 重放：这张单已经在退、或者已经退完了。**判据是退款单号对得上**——对不上说明这张单
	// 上记的是另一张退款单，那是要人来看的（见 ErrAfterSaleRefundMismatch）。
	if item.Status == model.AfterSaleStatusRefunding || item.Status == model.AfterSaleStatusRefunded {
		if item.RefundNo != p.RefundNo {
			return nil, false, fmt.Errorf("%w: after sale %s holds %q, got %q",
				ErrAfterSaleRefundMismatch, item.AfterSaleNo, item.RefundNo, p.RefundNo)
		}
		row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
		if err != nil {
			return nil, false, err
		}
		return row, true, tx.Commit(ctx)
	}
	if item.Status != model.AfterSaleStatusApproved {
		// 还没批过、或者已经结束（驳回/撤销/失败）。**不在这里把 pending 也放过去**：
		// 审核通过是同意退款的唯一依据，绕过它等于后台任何一个人都能退钱。
		return nil, false, fmt.Errorf("%w: after sale %s is %s", ErrAfterSaleNotApproved, item.AfterSaleNo, item.Status)
	}

	if _, err := startRefundTx(ctx, tx, item, p.RefundNo, p.RequestID, p.ActorID); err != nil {
		return nil, false, err
	}

	row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return row, false, nil
}

// startRefundTx 是 approved → refunding 那一段写操作，调用方保证 item 已经锁住且确实是
// approved。返回的是**订单被推进 refunding 之前的状态**——退款失败时要按它把订单放回去，
// 而「之前是 paid 还是 completed」只有这一刻读得到最准。
//
// 抽出来是因为它有两个入口：正常路径（StartRefund），以及事件先到时补写那一步
// （AdvanceRefund 里那次 adopt）。两处写的东西必须逐字相同，否则补写出来的历史会缺一段。
func startRefundTx(ctx context.Context, tx pgx.Tx, item *model.OrderAfterSale,
	refundNo, requestID, actorID string) (string, error) {
	order, err := lockedOrderByID(ctx, tx, item.OrderID)
	if err != nil {
		return "", err
	}
	if order.Status != model.OrderStatusPaid && order.Status != model.OrderStatusCompleted {
		// 订单已经不在「钱在我们这儿、单还活着」的状态上（被别人退过、关过、或者已经在退）。
		return "", fmt.Errorf("%w: order %s is %s", ErrOrderNotRefundable, order.OrderNo, order.Status)
	}
	previous := order.Status

	if _, err := tx.Exec(ctx, `UPDATE order_after_sales
		SET status=$2, refund_no=$3, updated_at=NOW() WHERE id=$1`,
		item.ID, model.AfterSaleStatusRefunding, refundNo); err != nil {
		return "", mapPGError(err)
	}

	actorType, actor := actorFor(actorID)
	if err := recordTransition(ctx, tx, model.AggregateAfterSale, item.ID, item.Status,
		model.AfterSaleStatusRefunding, "发起退款", requestID, actorType, actor, map[string]any{
			"refundNo": refundNo, "refundAmount": item.RefundAmount,
		}); err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `UPDATE orders SET status=$2, updated_at=NOW() WHERE id=$1`,
		order.ID, model.OrderStatusRefunding); err != nil {
		return "", mapPGError(err)
	}
	// 订单这一条记 system 而不是上面那个人：推动订单进退款中的是**这张售后单被推进**
	// 这件事本身，不是某个人点了什么。人的动作记在售后单那条流水与审计里。
	if err := recordTransition(ctx, tx, model.AggregateOrder, order.ID, previous,
		model.OrderStatusRefunding, "售后退款", requestID, model.ActorSystem, nil, map[string]any{
			"afterSaleNo": item.AfterSaleNo, "refundNo": refundNo,
		}); err != nil {
		return "", err
	}
	return previous, nil
}

// actorFor 把「有 id 就是后台的人、没有就是系统」这条约定收在一处。
func actorFor(actorID string) (string, *string) {
	if strings.TrimSpace(actorID) == "" {
		return model.ActorSystem, nil
	}
	id := actorID
	return model.ActorAdmin, &id
}

// orderStatusBeforeRefunding 找回「订单被推进 refunding 之前是哪个状态」。
//
// 为什么不把原状态存在售后单上：那要多一列，而这一列与状态流水里已经有的是同一件事，
// 两份记着迟早会分叉。状态流水是只追加的（order_state_transitions_append_only），
// 所以往回读是可靠的。
//
// 同一张订单上不会有两次**并行**的退款（占用互斥位挡住了），所以最近那一条
// to_status='refunding' 就是这一次的；前一次失败的退款留下的那条更早，排在后面。
func orderStatusBeforeRefunding(ctx context.Context, tx pgx.Tx, orderID string) (string, error) {
	var from string
	err := tx.QueryRow(ctx, `SELECT from_status FROM order_state_transitions
		WHERE aggregate_type=$1 AND aggregate_id=$2 AND to_status=$3
		ORDER BY created_at DESC LIMIT 1`,
		model.AggregateOrder, orderID, model.OrderStatusRefunding).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return from, nil
}

// AdvanceRefundParams 是一条退款结果事件落进售后的全部事实。
type AdvanceRefundParams struct {
	AfterSaleNo string
	// RefundNo 是支付侧的退款单号。它与库里记的那个对不上时整条事件被拒，见
	// ErrAfterSaleRefundMismatch。
	RefundNo string
	// Succeeded 是这次退款的结论。**只有两档**——渠道明确退成了，或者明确拒绝了。
	// 「没结论」（PROCESSING / UNKNOWN）不发事件，所以这里没有第三种取值可收。
	Succeeded bool
	// RefundedAt 是钱退回去的时刻（事件里的 succeededAtUnix）。零值用 NOW()。
	RefundedAt time.Time
	// FailureCode / FailureMessage 只在失败时有值，写进 order_after_sales.failure_code。
	FailureCode    string
	FailureMessage string
	RequestID      string
	// TraceID 跟着 outbox 走。这条链的结论要发出去给账户域（见下面那两条事件），
	// 少了它，下游看到的那条消息在链路上是断头的——与其余几条领域事件同一条规矩。
	TraceID string
}

// AdvanceRefund 按一条退款结果事件推进售后单与订单。返回的 bool 是「这次什么都没写」（重放）。
//
// 事件是这条链上**钱的结论的唯一来源**：渠道退成没退成只有 payment-service 知道，而它
// 只在有结论时才发事件。所以这里做的是「把那个结论落到订单域的事实上」：
//
//	succeeded → 售后 refunded（记 refunded_at），订单 refunded（整单退）或回到退款前
//	failed    → 售后 failed（记 failure_code），订单回到退款前
//
// **按行退成功不把整单判死**：只退了一杯的订单还活着，把它标成 refunded 会让剩下的行
// 连履约都做不了。判据是售后单自己的 scope，不是金额——金额可能刚好等于整单。
func (r *PostgresRepository) AdvanceRefund(ctx context.Context, p AdvanceRefundParams) (*AfterSaleRow, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	item, err := lockedAfterSaleByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, false, err
	}

	// 重放：已经有结论了。消息队列保证的是至少一次，同一条事件被投两遍是常态而不是异常。
	if item.Status == model.AfterSaleStatusRefunded || item.Status == model.AfterSaleStatusFailed {
		if item.RefundNo != p.RefundNo {
			return nil, false, fmt.Errorf("%w: after sale %s holds %q, got %q",
				ErrAfterSaleRefundMismatch, item.AfterSaleNo, item.RefundNo, p.RefundNo)
		}
		row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
		if err != nil {
			return nil, false, err
		}
		return row, true, tx.Commit(ctx)
	}

	var previous string
	switch item.Status {
	case model.AfterSaleStatusApproved:
		// 事务 B 没跑成，而钱已经退了：审核通过 → 建退款单 → 写 refunding 这三步之间挂掉
		// 过一次。**这时候不能把事件扔掉**——钱是真的出去了，扔掉它就等于订单永远不知道
		// 这笔钱退过。补写这一步，让流水上每一段迁移都是合法的，然后照常落结论。
		if item.RefundNo != "" && item.RefundNo != p.RefundNo {
			return nil, false, fmt.Errorf("%w: after sale %s holds %q, got %q",
				ErrAfterSaleRefundMismatch, item.AfterSaleNo, item.RefundNo, p.RefundNo)
		}
		previous, err = startRefundTx(ctx, tx, item, p.RefundNo, p.RequestID, "")
		if err != nil {
			return nil, false, err
		}
	case model.AfterSaleStatusRefunding:
		if item.RefundNo != p.RefundNo {
			return nil, false, fmt.Errorf("%w: after sale %s holds %q, got %q",
				ErrAfterSaleRefundMismatch, item.AfterSaleNo, item.RefundNo, p.RefundNo)
		}
		previous, err = orderStatusBeforeRefunding(ctx, tx, item.OrderID)
		if err != nil {
			return nil, false, err
		}
		if previous == "" {
			// 到不了：订单能停在 refunding 就说明有一条迁进来的流水。真落在这里只可能是
			// 有人手工改过库，而 paid 是唯一一句「单还活着、钱还在我们这儿」的诚实话。
			previous = model.OrderStatusPaid
		}
		// 订单这一行必须锁住再改：并发的那条路（另一条事件、或者有人手工动单）会在这里排队。
		if _, err := lockedOrderByID(ctx, tx, item.OrderID); err != nil {
			return nil, false, err
		}
	default:
		// 还没批过（pending）、或者已经结束（驳回/撤销）。这两种情况下都不该有钱出去，
		// 收到了退款结果事件说明状态机被绕过了一次，只能人来查。
		return nil, false, fmt.Errorf("%w: after sale %s is %s", ErrAfterSaleNotRefunding, item.AfterSaleNo, item.Status)
	}

	target := model.AfterSaleStatusFailed
	reason := "退款失败"
	if p.Succeeded {
		target = model.AfterSaleStatusRefunded
		reason = "退款成功"
	}
	// failure_code 与 failure_message 都取事件里的值，而且一起写：成功那条路上事件带的正是
	// 两个空串，所以两列永远描述同一个结论，不会留下「已退成却还挂着失败原因」的半截状态
	// （审计流水里两者都留了一份，那是查历史的地方）。
	if _, err := tx.Exec(ctx, `UPDATE order_after_sales
		SET status=$2, refunded_at=$3, failure_code=$4, failure_message=$5, updated_at=NOW()
		WHERE id=$1`,
		item.ID, target, nullableTime(p.RefundedAt), p.FailureCode, p.FailureMessage); err != nil {
		return nil, false, mapPGError(err)
	}
	if err := recordTransition(ctx, tx, model.AggregateAfterSale, item.ID, item.Status, target,
		reason, p.RequestID, model.ActorSystem, nil, map[string]any{
			"refundNo": p.RefundNo, "refundAmount": item.RefundAmount,
			"failureCode": p.FailureCode, "failureMessage": p.FailureMessage,
		}); err != nil {
		return nil, false, err
	}

	// 订单侧的新状态：退成了整单就是 refunded，其余一律回到退款前那个状态——
	// 失败与按行退都是「这张单还活着，只是少了一部分钱」。
	orderTarget := previous
	refunded := int64(0)
	if p.Succeeded {
		refunded = item.RefundAmount
		if item.Scope == model.AfterSaleScopeAll {
			orderTarget = model.OrderStatusRefunded
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orders
		SET status=$2, refunded_amount=refunded_amount+$3, updated_at=NOW() WHERE id=$1`,
		item.OrderID, orderTarget, refunded); err != nil {
		return nil, false, mapPGError(err)
	}
	if err := recordTransition(ctx, tx, model.AggregateOrder, item.OrderID,
		model.OrderStatusRefunding, orderTarget, reason, p.RequestID, model.ActorSystem, nil, map[string]any{
			"afterSaleNo": item.AfterSaleNo, "refundNo": p.RefundNo, "refundedAmount": refunded,
		}); err != nil {
		return nil, false, err
	}

	// 结论发出去。这是退款链上唯一一条离开订单域的消息，账户域拿它做最后半步：
	// 退成 ⇒ 追回这一单送的福卡（余额真的少掉），没退成 ⇒ 解冻（卡原样放回去）。
	//
	// 放在这个事务里、与两条 UPDATE 同一个提交：钱退了这个事实与「下游该去追回」是
	// 一件事的两面，分开落就会出现「钱退成了但没人知道要追回」。重放分支在上面提前
	// return 了，所以同一条事件投两遍也只有一条 outbox。
	//
	// 载荷不带「要冻哪几笔发放」：那几把键已经落在冻结行上了（applied 事件带来的），
	// 重发一份就是把 fortuneCardFreezeKeys 的拆分规则复制到第二个地方。
	refundTopic := EventAfterSaleRefunded
	if !p.Succeeded {
		refundTopic = EventAfterSaleRefundFailed
	}
	if err := appendOutbox(ctx, tx, refundTopic, eventVersion, p.TraceID,
		dto.AfterSaleRefundEventPayload{
			AfterSaleID: item.ID, AfterSaleNo: item.AfterSaleNo, OrderID: item.OrderID,
			OrderNo: item.OrderNo, UserID: item.UserID, RefundNo: p.RefundNo,
			RefundAmount: item.RefundAmount, RefundedAtUnix: unixOrZero(p.RefundedAt),
			FailureCode: p.FailureCode, FailureMessage: p.FailureMessage,
		}); err != nil {
		return nil, false, err
	}

	row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return row, false, nil
}

// nullableTime 把零值时间换成 NULL。
//
// 事件里的 succeededAtUnix 可以为 0（支付侧没带），那时该用 NOW() 而不是 1970 年——
// refunded_at 是要给人看「什么时候退的」，一个 1970 比空更糟：它看起来像个真的时刻。
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// unixOrZero 把零值时间翻成 0 而不是 1970 年那个巨大的负数。
//
// 与 nullableTime 同一件事的秒数版：事件里的 succeededAtUnix 可以为 0（支付侧没带），
// 消费方约定 0 就是「没带、用你收到的那一刻」。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

// CancelAfterSaleParams 是撤销一次退款申请。
type CancelAfterSaleParams struct {
	AfterSaleNo string
	// UserID 非空 = 用户撤销自己的申请；别人的单一律回「不存在」。
	UserID    string
	Reason    string
	RequestID string
	TraceID   string
}

// CancelAfterSale 撤销一张还没被审核的售后单。
//
// 只能撤 pending：审核通过之后钱已经在路上了，撤销不再由用户发起（那条边留给退款单）。
//
// 发 order.after_sale.cancelled。这里曾经不发事件，理由是「申请阶段没有退款单，payments 侧
// 什么都没做过，没有下游动作可撤」——福卡冻结让那句话不再成立：申请即冻，撤销是把这个动作
// 撤掉，冻着的卡得放回去。而且撤销之后用户手上再没有任何能解开它的动作了，所以这条事件
// 不是通知，是唯一的解冻信号。
func (r *PostgresRepository) CancelAfterSale(ctx context.Context, p CancelAfterSaleParams) (*AfterSaleRow, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	item, err := lockedAfterSaleByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, err
	}
	if p.UserID != "" && item.UserID != p.UserID {
		return nil, ErrAfterSaleNotFound
	}
	if item.Status != model.AfterSaleStatusPending {
		return nil, fmt.Errorf("%w: after sale %s is %s", ErrAfterSaleNotPending, item.AfterSaleNo, item.Status)
	}

	if _, err := tx.Exec(ctx, `UPDATE order_after_sales
		SET status=$2, updated_at=NOW() WHERE id=$1`, item.ID, model.AfterSaleStatusCancelled); err != nil {
		return nil, mapPGError(err)
	}

	actorType := model.ActorSystem
	var actorID *string
	if p.UserID != "" {
		actorType = model.ActorUser
		userID := p.UserID
		actorID = &userID
	}
	if err := recordTransition(ctx, tx, model.AggregateAfterSale, item.ID, item.Status,
		model.AfterSaleStatusCancelled, p.Reason, p.RequestID, actorType, actorID, nil); err != nil {
		return nil, err
	}

	// 与 reviewed 同形，少一个 action（撤销不是一次审核动作）。解冻只认 afterSaleNo，
	// 其余字段是给下一个消费者（payment 侧的退款单）准备的同一份上下文。
	if err := appendOutbox(ctx, tx, EventAfterSaleCancelled, eventVersion, p.TraceID,
		dto.AfterSaleCancelledEventPayload{
			AfterSaleID: item.ID, AfterSaleNo: item.AfterSaleNo, OrderID: item.OrderID,
			OrderNo: item.OrderNo, UserID: item.UserID, Status: model.AfterSaleStatusCancelled,
			Reason: p.Reason,
		}); err != nil {
		return nil, err
	}

	row, err := afterSaleRowByNo(ctx, tx, p.AfterSaleNo)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// AfterSaleFilter 是售后单列表的筛选条件。零值 = 不筛。
//
// 时间与 OrderFilter 同一个约定：闭开区间 [CreatedFrom, CreatedTo)，时区由调用方换算。
type AfterSaleFilter struct {
	AfterSaleNo string
	OrderNo     string
	UserID      string
	Status      string
	Scope       string
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Page        int
	PageSize    int
}

// ListAfterSales 按筛选条件分页读售后单。
//
// 列表与详情用同一组列（含派生列）：售后单本身只有二十来个字段，而「退的是哪一行、
// 这单承诺过几张福卡」正是审核时要看的东西，拆成两种形状只会多一处会漂移的映射。
func (r *PostgresRepository) ListAfterSales(ctx context.Context, f AfterSaleFilter) ([]*AfterSaleRow, int, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	// userId 是唯一的 uuid 列筛选（见 checkUUIDFilter）：不验形状就绑进 a.user_id，
	// 一次「筛选框里少打一个字符」会变成 22P02 → 500。
	if err := checkUUIDFilter("userId", f.UserID); err != nil {
		return nil, 0, err
	}

	// 参数按顺序追加，$n 跟着 len(args) 走：手写编号最容易在加条件时错位。
	var args []any
	conditions := make([]string, 0, 8)
	bind := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.AfterSaleNo != "" {
		conditions = append(conditions, "a.after_sale_no = "+bind(f.AfterSaleNo))
	}
	if f.OrderNo != "" {
		conditions = append(conditions, "a.order_no = "+bind(f.OrderNo))
	}
	if f.UserID != "" {
		conditions = append(conditions, "a.user_id = "+bind(f.UserID))
	}
	if f.Status != "" {
		conditions = append(conditions, "a.status = "+bind(f.Status))
	}
	if f.Scope != "" {
		conditions = append(conditions, "a.scope = "+bind(f.Scope))
	}
	if f.CreatedFrom != nil {
		conditions = append(conditions, "a.created_at >= "+bind(*f.CreatedFrom))
	}
	if f.CreatedTo != nil {
		conditions = append(conditions, "a.created_at < "+bind(*f.CreatedTo))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)`+afterSaleFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		// 一条都没有时不必再查一次：分页参数可能超出范围，结果必然是空。
		return nil, 0, nil
	}

	pageArgs := append(append([]any{}, args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+afterSaleColumns+`, `+afterSaleDerivedColumns+afterSaleFrom+where+
		fmt.Sprintf(" ORDER BY a.created_at DESC, a.id LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*AfterSaleRow, 0, f.PageSize)
	for rows.Next() {
		item, err := scanAfterSaleRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
