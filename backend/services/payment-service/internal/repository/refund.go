package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 退款那条路上的业务错误。
var (
	// ErrRefundExceedsRefundable：这次要退的金额超过了这张支付单**剩下的**可退额。
	//
	// 它是一个结论（调用方拿去改金额），不是故障——所以它在 service 那侧落成一张
	// status='failed' 的退款单，而不是一个 error。
	ErrRefundExceedsRefundable = errors.New("refund amount exceeds the refundable amount of the payment")
	// ErrPaymentNotRefundable：这张支付单不在能退的状态（还没收妥、已经失败、已经关掉）。
	//
	// 「还没收妥」是这条判据真正要挡的：一笔 pending 的钱退不出去，而渠道对它的回答会是一句
	// 与本意无关的「查无此单」。在这里挡下来，错误串里说得清是为什么。
	ErrPaymentNotRefundable = errors.New("payment is not in a refundable state")
	// ErrRefundNotFound：退款单不存在。
	ErrRefundNotFound = errors.New("refund not found")
	// ErrRefundNotAdvanceable：这张退款单不在能走到目标状态的那个状态上（已经 succeeded 了，
	// 或者已经从另一个入口被推走了）。
	//
	// 它**不是** ErrRefundNotFound：那一条说的是「没有这张单」，这一条说的是「有，但它已经
	// 不在你读到的那个状态上了」。混成一个值会让「并发下我输了这一次 CAS」看起来像数据丢了。
	ErrRefundNotAdvanceable = errors.New("refund is not in a state that can advance")
)

// refundColumns 是 payment_refunds 表的读取列。
//
// UUID 列一律 ::text，理由同 paymentColumns。列顺序与 scanRefund 严格一一对应。
const refundColumns = `id::text, refund_no, legacy_id, payment_id::text, payment_no, order_no,
	after_sale_no, order_line_id::text, user_id::text, amount, reason, status,
	provider_refund_id, failure_code, failure_message, request_id, succeeded_at,
	created_at, updated_at`

const refundFundingColumns = `id::text, refund_id::text, funding_id::text, line_no, line_type,
	amount, status, provider_refund_id, failure_code, account_entry_id::text,
	succeeded_at, created_at, updated_at`

func scanRefund(row scanner) (*model.Refund, error) {
	refund := &model.Refund{}
	err := row.Scan(&refund.ID, &refund.RefundNo, &refund.LegacyID, &refund.PaymentID,
		&refund.PaymentNo, &refund.OrderNo, &refund.AfterSaleNo, &refund.OrderLineID,
		&refund.UserID, &refund.Amount, &refund.Reason, &refund.Status,
		&refund.ProviderRefundID, &refund.FailureCode, &refund.FailureMessage,
		&refund.RequestID, &refund.SucceededAt, &refund.CreatedAt, &refund.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return refund, nil
}

func scanRefundFunding(row scanner) (*model.RefundFunding, error) {
	funding := &model.RefundFunding{}
	err := row.Scan(&funding.ID, &funding.RefundID, &funding.FundingID, &funding.LineNo,
		&funding.LineType, &funding.Amount, &funding.Status, &funding.ProviderRefundID,
		&funding.FailureCode, &funding.AccountEntryID, &funding.SucceededAt,
		&funding.CreatedAt, &funding.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return funding, nil
}

func loadRefundFundings(ctx context.Context, tx pgx.Tx, refundID string) ([]*model.RefundFunding, error) {
	rows, err := tx.Query(ctx, `SELECT `+refundFundingColumns+`
		FROM payment_refund_fundings WHERE refund_id=$1 ORDER BY line_no`, refundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fundings []*model.RefundFunding
	for rows.Next() {
		funding, err := scanRefundFunding(rows)
		if err != nil {
			return nil, err
		}
		fundings = append(fundings, funding)
	}
	return fundings, rows.Err()
}

// refundableFunding 是一行**还能退**的出资：它还有多少额度，以及这额度挂在哪个来源上。
type refundableFunding struct {
	id       string
	lineNo   int
	lineType string
	amount   int64
}

// refundableFundingsQuery 列出这张支付单还能退的出资行与各自的剩余额度，按 line_no 升序。
//
// 剩余额度 = 出资额 − 已经许出去的那部分。「许出去」必须把**在途**的也算上：一张停在
// pending/processing 的退款单还没有把出资行置成 reversed，但它的钱已经承诺出去了，第二次
// 退款再按原始额度分摊就会退超。
//
// 判据里的那组状态（pending/processing/succeeded）与 BeginRefund 里算 reserved 的那条 SQL
// 逐字一致，**两处必须一起改**：一处把某种状态当成「钱已经出去了」而另一处当成「还没出去」，
// 分摊出来的数与可退余额就对不上。
//
// 只取 status='succeeded' 的出资行：reserved/failed/released 的钱从来没到过我们手上，
// reversed 的已经退干净了（剩余额度也会算成 0，这里再挡一次是让查询的意图直接读得出来）。
//
// funding_id 为空是允许的（存量迁移过来的退款找不到对应出资行），那种行不进 consumed 的
// 分组，也不会被这里选中——它本来就没有对应的 payment_fundings 可分摊。
const refundableFundingsQuery = `WITH consumed AS (
		SELECT rf.funding_id, SUM(rf.amount) AS amount
		FROM payment_refund_fundings rf
		JOIN payment_refunds r ON r.id = rf.refund_id
		WHERE r.status IN ('pending','processing','succeeded')
		GROUP BY rf.funding_id
	)
	SELECT f.id::text, f.line_no, f.line_type, f.amount - COALESCE(c.amount, 0)
	FROM payment_fundings f
	LEFT JOIN consumed c ON c.funding_id = f.id
	WHERE f.payment_id = $1::uuid
		AND f.status = 'succeeded'
		AND f.amount > COALESCE(c.amount, 0)
	ORDER BY f.line_no`

// BeginRefundParams 是建退款单（退款第一段事务）需要的全部输入。
type BeginRefundParams struct {
	// RefundNo 是本服务生成的退款单号，由 service 生成。
	RefundNo string
	// PaymentNo 是**内部支付单号**（payments.payment_no）。仓储按它去锁那张支付单，
	// 顺手取回 order_no / user_id / 各行出资。
	PaymentNo string
	// AfterSaleNo 是 order-service 的售后单号，也是这张退款单的幂等键。
	AfterSaleNo string
	// OrderLineID 可空，整单退时为空。
	OrderLineID string
	// Amount 单位为分，由调用方权威给出。
	Amount int64
	Reason string
	// RequestID 是这一次调用的请求号，只落库供排查；**幂等靠 AfterSaleNo，不靠它**。
	RequestID string
}

// BeginRefund 在**一个事务里**锁住支付单、校验可退余额、建退款单与逐行冲正计划。
//
// 返回 (退款单, 它的各行, 是否命中幂等, error)。命中时调用方拿回的是上次那张退款单，
// 以及它当时算好的那几行——不重新算，因为可退余额此刻已经变了（上一次退款已经占掉了它）。
//
// # 可退余额在锁内算，这是本函数存在的第二个理由
//
// 余额 = payments.amount − SUM(已成功或正在处理的退款单的金额)。两个并发请求（用户连点、
// 或者调用方重试踩在第一次还没落库的窗口上）各自读到同一个余额、各建一张退款单，退的钱就
// 超过了收的钱——而渠道那边两笔都会成功。所以这笔账必须在 `SELECT … FOR UPDATE` 锁住那张
// 支付单之后算，这是唯一能把它算对的地方。
//
// **幂等键不能替代它**：after_sale_no 整表唯一挡的是「同一张售后单重发」，挡不住「两张不同
// 的售后单各自退了整单」（客服手工开的第二条售后单就是这种情况）。
//
// # 出资行怎么分
//
// 把这次要退的金额**分摊**到还能退的出资行上：行号、来源、以及被冲的那一行出资的 ID 都沿用
// 出资行，金额是分摊出来的那一份（见 refundableFundingsQuery 与下面那段）。
//
// **分摊而不是照抄**，因为退款额可以小于支付额：售后单按行退（order-service 的 scope=line）
// 就是这种，而发起退款时发给渠道的金额是这些行累加出来的——照抄全额等于按整单退给渠道。
//
// **这里不筛 line_type**——哪些行要真的走渠道是 service 的判断（见 service/refund.go），
// 仓储只负责把「这笔钱当初是怎么来的、这次退掉了它多少」搬到退款这一侧。
func (r *PostgresRepository) BeginRefund(ctx context.Context, p BeginRefundParams) (*model.Refund, []*model.RefundFunding, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先看幂等键。用 after_sale_no 直接查而不是走 payment_idempotency_keys：退款单号自己
	// 就是那把唯一的钥匙（payment_refunds.after_sale_no 整表唯一），多一张表只多一处能对不上。
	existing, err := findRefundByAfterSaleNo(ctx, tx, p.AfterSaleNo)
	switch {
	case err == nil:
		fundings, err := loadRefundFundings(ctx, tx, existing.ID)
		if err != nil {
			return nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, err
		}
		return existing, fundings, true, nil
	case !errors.Is(err, ErrRefundNotFound):
		return nil, nil, false, err
	}

	// 锁住那张支付单。这一句同时回答三件事，所以不用再查一次：
	//   - 它在不在（不存在的支付单退不了）；
	//   - 它是不是 succeeded（**只有收妥的钱能退**，见 ErrPaymentNotRefundable）；
	//   - 它当初是谁、哪一单、多少钱。
	//
	// FOR UPDATE 是这一整段的要害：它把「算可退余额」与「建退款单」之间的窗口关掉了。
	var paymentID, orderNo, userID string
	var paymentAmount int64
	err = tx.QueryRow(ctx, `SELECT id::text, order_no, user_id::text, amount
		FROM payments WHERE payment_no=$1 AND status='succeeded' FOR UPDATE`,
		p.PaymentNo).Scan(&paymentID, &orderNo, &userID, &paymentAmount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 两种成因（没有这张单 / 它还没收妥）在这一层分不开，也不该在这里分：调用方
			// 需要知道的是「这笔钱现在退不了」，而不是哪一条 SQL 没命中。
			return nil, nil, false, ErrPaymentNotRefundable
		}
		return nil, nil, false, err
	}

	// 已成功与正在处理中的都要算进已退额：正在处理的那一笔**可能**会成功，把它当成没退过
	// 会让用户在同一笔钱上退第二次。失败的与取消的不算——那些钱没有出去。
	var reserved int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM payment_refunds
		WHERE payment_id=$1 AND status IN ('succeeded','processing','pending')`,
		paymentID).Scan(&reserved); err != nil {
		return nil, nil, false, err
	}
	if p.Amount > paymentAmount-reserved {
		return nil, nil, false, fmt.Errorf("%w: refundable %d, requested %d",
			ErrRefundExceedsRefundable, paymentAmount-reserved, p.Amount)
	}

	refund, err := scanRefund(tx.QueryRow(ctx, `INSERT INTO payment_refunds
		(refund_no, payment_id, payment_no, order_no, after_sale_no, order_line_id,
		 user_id, amount, reason, status, request_id)
		VALUES ($1,$2::uuid,$3,$4,$5,NULLIF($6,'')::uuid,$7::uuid,$8,$9,'pending',$10)
		RETURNING `+refundColumns,
		p.RefundNo, paymentID, p.PaymentNo, orderNo, p.AfterSaleNo, p.OrderLineID,
		userID, p.Amount, p.Reason, p.RequestID))
	if err != nil {
		return nil, nil, false, mapPGError(err)
	}

	// 把这次要退的钱**分摊**到还能退的出资行上。
	//
	// 不是照抄出资行的全额。退款额可以小于支付额（售后单按行退就是这种，见 order-service
	// 的 scope=line），照抄会让渠道收到一笔比售后单大得多的退款——发起退款时发给渠道的那个
	// 金额，正是把这些行的金额累加出来的（见 service/refund.go 的 splitRefundFundings）。
	//
	// 也不是按原始金额比例分：分摊只认「哪一行还有钱」，按 line_no 顺序吃到够为止。混合出资
	// （渠道 + 咖啡豆）落地时这条规则同样成立——两个来源各退各的那一份，账户那一份不会跑到
	// 渠道去。
	//
	// line_no 与 line_type 沿用被冲那一行的取值，让两条链的行能按序号对上。
	rows, err := tx.Query(ctx, refundableFundingsQuery, paymentID)
	if err != nil {
		return nil, nil, false, err
	}
	var refundable []refundableFunding
	for rows.Next() {
		var f refundableFunding
		if err := rows.Scan(&f.id, &f.lineNo, &f.lineType, &f.amount); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		refundable = append(refundable, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}

	remaining := p.Amount
	for _, f := range refundable {
		if remaining == 0 {
			break
		}
		take := min(f.amount, remaining)
		if _, err := tx.Exec(ctx, `INSERT INTO payment_refund_fundings
			(refund_id, funding_id, line_no, line_type, amount, status)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,'pending')`,
			refund.ID, f.id, f.lineNo, f.lineType, take); err != nil {
			return nil, nil, false, err
		}
		remaining -= take
	}
	// 分摊不满这次退款额，说明可退余额算出来的数与出资行的账对不上（出资行没有一行是
	// succeeded 的、被谁改过、或者那组状态的判据两处不一致）。宁可在这里炸，也不要建一张
	// 「退款单说退了 3000、它的出资行只凑得出 2000」的单——差额在渠道那边看不见，而
	// payment_transactions 会照着这些行逐笔记账，账面上就永远差这一笔。
	if remaining != 0 {
		return nil, nil, false, fmt.Errorf(
			"payment %s: refund of %d cannot be covered by its fundings, %d left unallocated",
			p.PaymentNo, p.Amount, remaining)
	}

	fundings, err := loadRefundFundings(ctx, tx, refund.ID)
	if err != nil {
		return nil, nil, false, err
	}

	if err := recordTransition(ctx, tx, model.AggregateRefund, refund.ID, "",
		model.RefundPending, "refund created", p.RequestID, model.ActorSystem, nil,
		map[string]any{"afterSaleNo": p.AfterSaleNo, "amount": p.Amount}); err != nil {
		return nil, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return refund, fundings, false, nil
}

// findRefundByAfterSaleNo 按售后单号找退款单，没有时返回 ErrRefundNotFound。
func findRefundByAfterSaleNo(ctx context.Context, tx pgx.Tx, afterSaleNo string) (*model.Refund, error) {
	refund, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+`
		FROM payment_refunds WHERE after_sale_no=$1`, afterSaleNo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefundNotFound
		}
		return nil, err
	}
	return refund, nil
}

// MarkRefundSucceededParams 是退款成功那一段（事务 2）的输入。
type MarkRefundSucceededParams struct {
	RefundID string
	// ProviderRefundID 是渠道侧的退款单号，账户出资那一路为空串（没有渠道）。
	ProviderRefundID string
	// SucceededAt 是钱退回去的时刻。渠道给了成交时间就用它，没给就用调用方那一刻
	// （见 service 里 refundSucceededAt 那段）。
	SucceededAt time.Time
	// NoOpLineTypes 是**不在这张退款单里走渠道**的那些出资行的来源（今天只有
	// `coffee_bean` 这一种支付方式）。它们的钱由 account-service 退——退款成功这件事经
	// 订单域转成 order.after_sale.refunded 之后，账户域才把那笔豆还回去，与这里标记成功
	// 是同一拍；本服务这一侧只把这一行标成成功，不发冲正请求（见 service.CreateRefund）。
	//
	// 值域是**支付方式的 code**，与 payment_refund_fundings.line_type 同一套——那套「出资
	// 渠道」词表已经退场，别按 wechat/unionpay/other 那些老值来传。
	//
	// 传进来的每一个值都必须真的在 payment_refund_fundings 里出现过——这句话由下面那条
	// UPDATE 的 line_type = ANY(...) 保证：写错一个名字的后果是那一行留在 pending，
	// 而整张退款单已经是 succeeded 了。
	//
	// **「一行都没有」要传空切片，不能传 nil**：nil 编成 SQL NULL，而
	// `NOT (line_type = ANY(NULL))` 是 NULL，走渠道的那些行会一行都匹配不上——退款单成了
	// succeeded，它的出资行却停在 pending。调用方用 splitRefundFundings，它返回的永远是
	// 空切片（见那里的注释）。
	NoOpLineTypes []string
	TraceID       string
}

// MarkRefundSucceeded 在**一个事务里**把退款单推到 succeeded、逐行标记、冲正出资、
// 记资金流水与状态流水、发 outbox。
//
// 事务顺序与结算那条路（SettleAccountPayment）刻意保持一致：改状态 → 各行 → 出资冲正 →
// 资金流水 → 状态流水 → outbox。**改动任何一处时都要一起改。**
//
// # 出资行的 reversed 是这条链对支付侧的唯一改动
//
// 一条出资行成功退掉之后，payment_fundings.status 从 succeeded 变 reversed。这是
// fundingTransitions 里唯一一条从 succeeded 出发的边（见 service/state.go），而它必须
// 与退款单在同一个事务里改：退款单说「退成了」而出资行还说「钱在这儿」的话，对账会认为
// 这笔钱同时存在两个地方。
//
// **只冲成功的那些行**：pending/reserved 的出资行没有钱可冲（见那条 UPDATE 的
// `status='succeeded'`）。一条支付单成功时它的出资行必然全是 succeeded，所以正常情况下
// 每一行都会被冲到；那个条件是给异常路径兜底的（对账发现的行）。
func (r *PostgresRepository) MarkRefundSucceeded(ctx context.Context, p MarkRefundSucceededParams) (*model.Refund, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 锁内再判一次状态：并发的退款查询 worker 与调用方可能同时走到这里，只有先拿到锁的那次
	// 算数。`status IN ('pending','processing')` 就是这个判定，与 canSettle 同一个形状。
	refund, err := scanRefund(tx.QueryRow(ctx, `UPDATE payment_refunds
		SET status='succeeded', provider_refund_id=$2, failure_code='', failure_message='',
			succeeded_at=$3, updated_at=NOW()
		WHERE id=$1 AND status IN ('pending','processing')
		RETURNING `+refundColumns, p.RefundID, p.ProviderRefundID, p.SucceededAt))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefundNotAdvanceable
		}
		return nil, err
	}

	// 走渠道的那些行：整批标成功。它们**每一行都成功**才走到这里——一行失败时整张退款单
	// 落的是 failed（见 MarkRefundFailed），不会出现「一半成功一半失败」的退款单。
	if _, err := tx.Exec(ctx, `UPDATE payment_refund_fundings
		SET status='succeeded', provider_refund_id=$2, failure_code='', succeeded_at=$3,
			updated_at=NOW()
		WHERE refund_id=$1 AND status='pending' AND NOT (line_type = ANY($4::text[]))`,
		refund.ID, p.ProviderRefundID, p.SucceededAt, p.NoOpLineTypes); err != nil {
		return nil, err
	}
	// 不走渠道的那些行（账户出资）：钱由 account-service 退，本服务只记录它已经还回去了。
	// provider_refund_id 留空——那一列记的是「渠道那边这笔退款叫什么」，而这一行根本没有
	// 渠道，填一个渠道单号是记账上的撒谎。
	if len(p.NoOpLineTypes) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE payment_refund_fundings
			SET status='succeeded', succeeded_at=$2, updated_at=NOW()
			WHERE refund_id=$1 AND status='pending' AND line_type = ANY($3::text[])`,
			refund.ID, p.SucceededAt, p.NoOpLineTypes); err != nil {
			return nil, err
		}
	}

	// 冲正出资本身。只冲这张退款单真的覆盖到的那些行（refund_fundings 里挂着 funding_id 的），
	// 而不是整张支付单的所有出资行。
	//
	// **只在这行被退干净的时候才置 reversed**：一条出资行可以分几次退（两次按行退的售后单
	// 打在同一笔支付上），只退出去了它的一部分时它仍然是「钱在这儿」，置成 reversed 会让
	// 对账以为这行已经冲光了，剩下那部分再退时也无从判断。
	//
	// 「退干净」= 许出去的总数 ≥ 出资额，判据与 refundableFundingsQuery 里那条互为反面，
	// 状态那组取值也必须一致。
	if _, err := tx.Exec(ctx, `UPDATE payment_fundings f
		SET status='reversed', reversed_at=$2, updated_at=NOW()
		FROM payment_refund_fundings rf
		WHERE rf.refund_id=$1 AND rf.funding_id=f.id AND f.status='succeeded'
			AND (SELECT COALESCE(SUM(rf2.amount), 0)
				FROM payment_refund_fundings rf2
				JOIN payment_refunds r2 ON r2.id = rf2.refund_id
				WHERE rf2.funding_id = f.id
					AND r2.status IN ('pending','processing','succeeded')) >= f.amount`,
		refund.ID, p.SucceededAt); err != nil {
		return nil, err
	}

	// 资金流水：**不可变的对账基准**（方案 5.9）。方向是 out（钱从这个渠道流回去），
	// 与支付那一笔的 in 成对。逐行记，金额取各行自己的那份，不记总额——总额在退款单上。
	// provider 从**那张支付单**取，不存一份在退款单上：退款走的一定是收钱时那条渠道，
	// 而在退款单上再存一列只会多一个能对不上的地方。账户出资的单 provider 本来就是空串
	// （见 SettleAccountPayment），所以这一句对两条路都对。
	if _, err := tx.Exec(ctx, `INSERT INTO payment_transactions
		(kind, payment_no, refund_no, funding_line_no, line_type, direction, amount,
		 provider, provider_transaction_id, account_entry_id, occurred_at)
		SELECT 'refund', r.payment_no, r.refund_no, rf.line_no, rf.line_type, 'out', rf.amount,
			pay.provider, rf.provider_refund_id, rf.account_entry_id, $2
		FROM payment_refund_fundings rf
		JOIN payment_refunds r ON r.id = rf.refund_id
		JOIN payments pay ON pay.id = r.payment_id
		WHERE rf.refund_id=$1 ORDER BY rf.line_no`,
		refund.ID, p.SucceededAt); err != nil {
		return nil, err
	}

	fundings, err := loadRefundFundings(ctx, tx, refund.ID)
	if err != nil {
		return nil, err
	}

	// actor 记 system：这一行是渠道的应答推出来的，不是「有人在支付服务里点了退款」。
	// 真正的那个动作人在订单侧留了痕（order_after_sales.reviewed_by），这里不复写一遍。
	if err := recordTransition(ctx, tx, model.AggregateRefund, refund.ID,
		model.RefundPending, model.RefundSucceeded, "refund succeeded", p.ProviderRefundID,
		model.ActorSystem, nil,
		map[string]any{"providerRefundId": p.ProviderRefundID}); err != nil {
		return nil, err
	}

	eventType, payload := refundEvent(refund, fundings)
	if err := appendOutbox(ctx, tx, eventType, dto.EventVersion, p.TraceID, payload); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return refund, nil
}

// MarkRefundFailedParams 是退款失败那一段（事务 3）的输入。
type MarkRefundFailedParams struct {
	RefundID       string
	FailureCode    string
	FailureMessage string
	// From 是这条失败是从哪个状态出发的（pending 还是 processing）。它只影响状态流水那一行
	// 的 from 列，不影响改动本身——两条边都是合法的（见 refundTransitions）。
	From    string
	TraceID string
}

// MarkRefundFailed 把退款单落成 failed：渠道拒绝、可退余额不够、或者渠道回了一句我们能读懂
// 的失败。**不发 succeeded 那条事件**，发的是 refund.failed。
//
// 出资行整批标 failed，payment_fundings 一个字段都不动——钱没有退回去，那些出资行仍然是
// succeeded。这是这张退款单与那些出资行唯一正确的相对状态。
//
// 资金流水**不记**：钱没有动过。
func (r *PostgresRepository) MarkRefundFailed(ctx context.Context, p MarkRefundFailedParams) (*model.Refund, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	refund, err := scanRefund(tx.QueryRow(ctx, `UPDATE payment_refunds
		SET status='failed', failure_code=$2, failure_message=$3, updated_at=NOW()
		WHERE id=$1 AND status IN ('pending','processing')
		RETURNING `+refundColumns, p.RefundID, p.FailureCode, p.FailureMessage))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefundNotAdvanceable
		}
		return nil, err
	}

	if _, err := tx.Exec(ctx, `UPDATE payment_refund_fundings
		SET status='failed', failure_code=$2, updated_at=NOW()
		WHERE refund_id=$1 AND status='pending'`, refund.ID, p.FailureCode); err != nil {
		return nil, err
	}

	fundings, err := loadRefundFundings(ctx, tx, refund.ID)
	if err != nil {
		return nil, err
	}

	if err := recordTransition(ctx, tx, model.AggregateRefund, refund.ID, p.From,
		model.RefundFailed, "refund failed: "+p.FailureMessage, p.RefundID,
		model.ActorSystem, nil, map[string]any{"failureCode": p.FailureCode}); err != nil {
		return nil, err
	}

	eventType, payload := refundEvent(refund, fundings)
	if err := appendOutbox(ctx, tx, eventType, dto.EventVersion, p.TraceID, payload); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return refund, nil
}

// MarkRefundProcessing 把退款单落成 processing：渠道收下了这次请求，但没给结论。
//
// **不发任何事件**：processing 不是结论，下游拿着它什么也做不了（order-service 的售后单
// 会一直停在 refunding，那正是对的——钱确实还没退回去）。结论由退款查询 worker 问出来。
//
// 出资行也留在 pending，一个字都不改：它们同样没有结论。
func (r *PostgresRepository) MarkRefundProcessing(ctx context.Context, p MarkRefundProcessingParams) (*model.Refund, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	refund, err := scanRefund(tx.QueryRow(ctx, `UPDATE payment_refunds
		SET status='processing', provider_refund_id=$2, updated_at=NOW()
		WHERE id=$1 AND status='pending' RETURNING `+refundColumns,
		p.RefundID, p.ProviderRefundID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefundNotAdvanceable
		}
		return nil, err
	}

	if err := recordTransition(ctx, tx, model.AggregateRefund, refund.ID,
		model.RefundPending, model.RefundProcessing,
		"provider accepted the refund but has not concluded", p.ProviderRefundID,
		model.ActorSystem, nil,
		map[string]any{"providerRefundId": p.ProviderRefundID}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return refund, nil
}

// MarkRefundProcessingParams 是 MarkRefundProcessing 的输入。
type MarkRefundProcessingParams struct {
	RefundID string
	// ProviderRefundID 是渠道在应答里给的退款单号，可能为空（渠道还没建出那一笔）。
	ProviderRefundID string
}

// FindRefundByNo 按退款单号读一张退款单，连带它的各行。给只读查询用。
func (r *PostgresRepository) FindRefundByNo(ctx context.Context, refundNo string) (*model.Refund, []*model.RefundFunding, error) {
	refund, err := scanRefund(r.pool.QueryRow(ctx, `SELECT `+refundColumns+`
		FROM payment_refunds WHERE refund_no=$1`, refundNo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrRefundNotFound
		}
		return nil, nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+refundFundingColumns+`
		FROM payment_refund_fundings WHERE refund_id=$1 ORDER BY line_no`, refund.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var fundings []*model.RefundFunding
	for rows.Next() {
		funding, err := scanRefundFunding(rows)
		if err != nil {
			return nil, nil, err
		}
		fundings = append(fundings, funding)
	}
	return refund, fundings, rows.Err()
}

// ListStaleProcessingRefunds 找出「向渠道发起之后一直没结论」的退款单。
//
// 判据与 ListStalePendingPayments 逐字同形：**updated_at 而不是 created_at**。一条
// processing 的退款单每被问一次都会刷新 updated_at，所以「很久没动过」才是「该去问一句了」，
// 而「建得很早」在一笔托管了三天才处理的退款上会一直成立。
//
// 排序也用 updated_at：最老的先问，与那条查询同一条理由——积压时每次都从同一头开始扫，
// 真正的老单反而永远排在后面取不到。
func (r *PostgresRepository) ListStaleProcessingRefunds(ctx context.Context, staleBefore time.Time, limit int) ([]model.Refund, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+refundColumns+`
		FROM payment_refunds
		WHERE status='processing' AND updated_at <= $1
		ORDER BY updated_at
		LIMIT $2`, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refunds []model.Refund
	for rows.Next() {
		refund, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		refunds = append(refunds, *refund)
	}
	return refunds, rows.Err()
}

// TouchRefund 把一条退款单的 updated_at 推到此刻，用于「问了一轮、但还没有结论」。
//
// 没有它，一笔问不出结论的退款单每一轮都会被重新捞出来问——退款查询 worker 每分钟一轮，
// 一批 20 笔全卡在同一笔上时，其余积压的退款单永远轮不到。刷新它等于说「这一笔刚问过，
// 让后面那些先来」，代价只是那一笔的下一次询问晚一个周期。
//
// 状态一个字都不改：它仍然没有结论。
func (r *PostgresRepository) TouchRefund(ctx context.Context, refundID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE payment_refunds SET updated_at=NOW()
		WHERE id=$1 AND status='processing'`, refundID)
	return err
}

// refundEvent 把一张退款单与它的各行翻成 outbox 里那份事件。
//
// 与 paymentEvent 同一种分工：事件体在 dto 里，这里只负责填值。**Amount 一律用退款单的
// amount**，失败事件也不例外——order-service 要拿它对着售后单上的 refund_amount 复核，
// 记 0 会让它以为这次退款是零元。
func refundEvent(refund *model.Refund, fundings []*model.RefundFunding) (string, dto.RefundEventPayload) {
	payload := dto.RefundEventPayload{
		OrderNo:        refund.OrderNo,
		PaymentNo:      refund.PaymentNo,
		RefundNo:       refund.RefundNo,
		AfterSaleNo:    refund.AfterSaleNo,
		Amount:         refund.Amount,
		FailureCode:    refund.FailureCode,
		FailureMessage: refund.FailureMessage,
	}
	if refund.SucceededAt != nil {
		payload.SucceededAtUnix = refund.SucceededAt.Unix()
	}
	for _, funding := range fundings {
		payload.Fundings = append(payload.Fundings, dto.RefundFunding{
			LineType:       funding.LineType,
			Amount:         funding.Amount,
			AccountEntryID: funding.AccountEntryID,
		})
	}
	if refund.Status == model.RefundSucceeded {
		return dto.EventPaymentRefundSucceeded, payload
	}
	return dto.EventPaymentRefundFailed, payload
}
