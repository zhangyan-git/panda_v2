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
)

// afterSaleColumns 是 order_after_sales 的读取列。
//
// 带 a. 前缀不是风格问题：读售后单的查询基本上都要 JOIN orders（福卡承诺）与 order_lines
// （退的是哪一行），而 orders 也有 id/status/created_at，不加前缀就会撞成 ambiguous。
// 列顺序与 scanAfterSale 的扫描顺序严格一一对应，两边必须一起改。
const afterSaleColumns = `a.id::text, a.legacy_id, a.after_sale_no, a.order_id::text, a.order_no,
	a.user_id::text, a.type, a.scope, a.order_line_id::text, a.status, a.reason, a.images,
	a.refund_amount, a.refund_no, a.failure_code, a.reviewed_by::text, a.reviewed_at,
	a.review_remark, a.created_at, a.updated_at, a.refunded_at`

// afterSaleDerivedColumns 是审核要看、但不属于售后单本身的三列（跟在 afterSaleColumns 后面）。
//
// 行的列用 COALESCE 抹平成零值而不是扫成指针：整单退（order_line_id 为空）时它们全是
// NULL，而「零值的行摘要」与「没有行摘要」在 Go 侧是一件事——用 COALESCE 就不用在每个
// 字段上判一次 nil。
const afterSaleDerivedColumns = `o.fortune_cards_expected, o.fortune_card_snapshot,
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
		&item.Images, &item.RefundAmount, &item.RefundNo, &item.FailureCode, &item.ReviewedBy,
		&item.ReviewedAt, &item.ReviewRemark, &item.CreatedAt, &item.UpdatedAt, &item.RefundedAt}
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
		&lineID, &lineNo, &lineType, &itemName, &quantity, &payableAmount)
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
	if p.UserID != "" && order.UserID != p.UserID {
		// 别人的单回「不存在」而不是「无权限」：403 等于告诉调用方这个 id 是真的。
		return nil, false, ErrOrderNotFound
	}
	if order.Status != model.OrderStatusPaid && order.Status != model.OrderStatusCompleted {
		return nil, false, fmt.Errorf("%w: order %s is %s", ErrOrderNotRefundable, order.OrderNo, order.Status)
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
	freezeKeys := fortuneCardFreezeKeys(order.ID, order.FortuneCardsExpected, order.FortuneCardSnapshot, p.Scope)
	if err := appendOutbox(ctx, tx, EventAfterSaleApplied, eventVersion, p.TraceID, map[string]any{
		"afterSaleId": id, "afterSaleNo": p.AfterSaleNo, "orderId": order.ID,
		"orderNo": order.OrderNo, "userId": order.UserID, "scope": p.Scope,
		"orderLineId": p.OrderLineID, "refundAmount": amount,
		"fortuneCardEntryKeys": freezeKeys,
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
// 通过只是「同意退这笔钱」，**不动订单主状态**：orders.status 上的 refunding 含义是
// 「退款单已经建了」，而今天没有服务建得了退款单（payment-service 未建）。真标上去，
// 订单就只能永远停在 refunding——回 paid/completed 的路径要按退款单上记录的原状态恢复。
// 所以这一版把它留在 order.after_sale.reviewed 事件里，等 payment-service 消费它。
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

	if err := appendOutbox(ctx, tx, EventAfterSaleReviewed, eventVersion, p.TraceID, map[string]any{
		"afterSaleId": item.ID, "afterSaleNo": item.AfterSaleNo, "orderId": item.OrderID,
		"orderNo": item.OrderNo, "userId": item.UserID, "status": target,
		"action": p.Action, "scope": item.Scope, "orderLineId": item.OrderLineID,
		"refundAmount": item.RefundAmount,
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
	if err := appendOutbox(ctx, tx, EventAfterSaleCancelled, eventVersion, p.TraceID, map[string]any{
		"afterSaleId": item.ID, "afterSaleNo": item.AfterSaleNo, "orderId": item.OrderID,
		"orderNo": item.OrderNo, "userId": item.UserID, "status": model.AfterSaleStatusCancelled,
		"reason": p.Reason,
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
