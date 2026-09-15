package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// ScopeCreatePayment 是发起支付那条幂等记录的 scope。
//
// scope 与 idempotency_key 一起唯一。带上动作名而不是只留 key：同一个调用方可能用同一串
// 随机号去开不同的动作，scope 把它们分开，两边的记录互不干扰。
const ScopeCreatePayment = "payment.create"

// BeginPaymentParams 是建支付单（发起支付的第一段事务）需要的全部输入。
//
// 它是个纯数据包，service 把校验做完再传进来；仓储不再判「金额该不该是正数」——
// 那些判断要在没有数据库的时候也能测（见 service.Repository 的注释）。
type BeginPaymentParams struct {
	// PaymentNo 是我们自己的支付单号，由 service 生成。
	PaymentNo string
	OrderNo   string
	UserID    string
	// Amount 单位为分，由调用方权威给出。
	Amount      int64
	FundingType string
	ChannelID   string
	MethodID    string
	Subject     string
	Attach      map[string]string
	RequestID   string
	ExpiresAt   time.Time
	RequestHash string
}

// BeginPayment 在**一个事务里**抢占幂等键、建支付单、写状态流水。
//
// 返回 (支付单, 已完成的响应体, 是否命中幂等, error)。命中且上次是 succeeded 时
// 调用方直接把响应体回放给客户端，不再执行任何副作用——这是「同一个 Idempotency-Key
// 重放发起支付只落一张单」的全部依据。
//
// 为什么建单与写流水必须在同一事务：支付单存在而流水不存在的话，「这笔支付怎么来的」
// 就没有依据了，而状态流水是排查时唯一能回答这个问题的东西。
//
// 陈旧的幂等键能被回收重开，靠的是**两步一起做**：beginIdempotentOperation 回收那行
// 幂等记录，retireAbandonedPayment 把上一次那半截尝试留下的支付单从同一个 request_id
// 上摘下来。少了第二步，下面这句 INSERT INTO payments 必然撞 payments_request_id_key，
// 于是「回收」变成一条走不通的路——而它正是 MarkPaymentPending 注释里写的那个已知窗口
// （进程死在两次提交之间）唯一的出路。
func (r *PostgresRepository) BeginPayment(ctx context.Context, p BeginPaymentParams) (*model.Payment, []byte, bool, error) {
	attach, err := json.Marshal(p.Attach)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode payment attach: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	replay, hit, recycled, err := beginIdempotentOperation(ctx, tx, ScopeCreatePayment, p.RequestID, p.RequestHash, "payment", "")
	if err != nil {
		return nil, nil, false, err
	}
	if hit {
		// 提交而不是回滚：beginIdempotentOperation 在 processing 分支会 DELETE +
		// 重新 INSERT 抢占那一行，回滚会把那次回收一起撤销，那条陈旧记录就永远回收不了。
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, err
		}
		return nil, replay, true, nil
	}
	if recycled {
		if err := retireAbandonedPayment(ctx, tx, p.RequestID); err != nil {
			return nil, nil, false, err
		}
	}

	payment, err := scanPayment(tx.QueryRow(ctx, `INSERT INTO payments
		(payment_no, order_no, user_id, amount, funding_type, channel_id, payment_method_id,
		 status, subject, attach, request_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,'created',$8,$9,$10,$11)
		RETURNING `+paymentColumns,
		p.PaymentNo, p.OrderNo, p.UserID, p.Amount, p.FundingType, p.ChannelID, p.MethodID,
		p.Subject, attach, p.RequestID, p.ExpiresAt))
	if err != nil {
		// 走 mapPGError：撞上 payments_request_id_key 或 payments_one_succeeded_per_order
		// 都是一个说得清的幂等结论，不该以裸的 pgconn.PgError 冒到 handler 变成 500。
		return nil, nil, false, mapPGError(err)
	}

	// from 传空串：这一行是「从没有到有」，不是一次状态迁移。与 order-service 里
	// 建单那一步的写法一致，让聚合的完整历史从第一行起就能读出来。
	if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, "",
		model.PaymentCreated, "payment created", p.RequestID, model.ActorSystem, nil, nil); err != nil {
		return nil, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return payment, nil, false, nil
}

// retireAbandonedPayment 把上一次那半截尝试留下的支付单从幂等键上摘下来，给重开的这次让路。
//
// 什么时候会走到这里：BeginPayment 提交之后、MarkPaymentPending 提交之前进程死掉（见
// MarkPaymentPending 的注释）。支付单停在 created、幂等行停在 processing。15 分钟后那条
// 幂等行被回收（beginIdempotentOperation），但**支付单还挂在这个 request_id 上**，
// 不摘掉的话新单插不进去——回收就成了一次必然撞 payments_request_id_key 的尝试。
//
// 摘的方式是把 request_id 置空，不是删掉那张单：payments 表的注释写着「失败的那张单留着，
// 用户换一种方式再付是新的一行」，一次发起过的事实不该因为没人付款就消失。request_id 的列
// 注释是「发起支付请求的幂等号，**非空时唯一**」，空串正是「这个键不再属于任何一张单」的写法。
//
// **`status <> 'succeeded'` 是这条语句唯一的安全阀**：一次成功收款与它的幂等键之间不能断。
// 断开之后同一个 request_id 再来就找不到那张单、会去建第二张（然后被
// payments_one_succeeded_per_order 挡住，报一个与真实原因无关的错）。
//
// 剩下一种情形这条语句不处理，也不该由它处理：那张单**已经成功**、而幂等行还是 processing。
// 它不是「半截尝试」的两半对不上，而是渠道回调在两次提交之间插了进来——SettlePayment 按
// payment_no 找到这张单并把它结掉（provider_transaction_id 两边必有一边为空时那道校验会跳过，
// 所以它结得掉），而回调那条路不碰幂等行。此时这里不动它，下面那句 INSERT 撞
// payments_request_id_key，经 mapPGError 变成 ErrPaymentNotPending——调用方拿到的是一个可重试
// 的 Aborted，不是 500，也不会多出一张单。真正的修法是让创建请求能回放一张回调结掉的单，
// 那要在仓储里构造服务层的响应快照（response 列此刻是空的），不在这一处改动里做。
func retireAbandonedPayment(ctx context.Context, tx pgx.Tx, requestID string) error {
	if requestID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE payments SET request_id='', updated_at=NOW()
		WHERE request_id=$1 AND status <> 'succeeded'`, requestID)
	return err
}

// MarkPaymentPendingParams 是发起支付成功那一段的输入。
type MarkPaymentPendingParams struct {
	PaymentID             string
	ProviderTransactionID string
	// FundingLines 是这次支付的逐笔出资。本轮只有渠道出资一条，写成切片是为了让账户出资
	// （咖啡豆）接进来时不用改事务的形状。
	FundingLines []FundingLine
	// IdempotencyResponse 是回放给客户端的响应快照。它写进幂等表，同一个 request_id
	// 再来时原样回放——客户端拿到的支付参数必须前后一致。
	IdempotencyResponse any
	// RequestID 是幂等键本身，用来定位要更新的那一行。
	RequestID string
}

// FundingLine 是一笔出资的写入口径。
type FundingLine struct {
	LineNo                int
	LineType              string
	Amount                int64
	ProviderTransactionID string
}

// MarkPaymentPending 在**一个事务里**把支付单推进到 pending、落出资行、写状态流水、
// 完成幂等记录。
//
// 这一段在渠道网络调用**之外**：provider.Create 已经回来了，我们只是把它的结论落下来。
// 渠道调用绝不放在 PG 事务里——一次几秒的第三方往返不该让一个数据库事务一直开着锁。
//
// 已知窗口：BeginPayment 提交之后、这个事务提交之前进程死掉，支付单会停在 created、
// 幂等停在 processing，15 分钟后可以被回收重开。渠道侧可能多出一张没人付的预支付单，
// 但那不会收钱。最终防线是 payments_one_succeeded_per_order 这条部分唯一索引——
// 订单只能被收一次钱，不靠应用层的判断。
func (r *PostgresRepository) MarkPaymentPending(ctx context.Context, p MarkPaymentPendingParams) (*model.Payment, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	payment, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments
		SET status='pending', provider_transaction_id=$2, updated_at=NOW()
		WHERE id=$1 AND status='created'
		RETURNING `+paymentColumns, p.PaymentID, p.ProviderTransactionID))
	if err != nil {
		// 0 行只有一种解释：这单已经不在 created 了（并发的另一次推进、或者被关单扫描
		// 收走了）。返回说得清的业务错误，别让它变成一次裸的 pgx.ErrNoRows。
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentNotPending
		}
		return nil, err
	}

	for _, line := range p.FundingLines {
		if _, err := tx.Exec(ctx, `INSERT INTO payment_fundings
			(payment_id, line_no, line_type, amount, status, provider_transaction_id)
			VALUES ($1,$2,$3,$4,'reserved',$5)`,
			p.PaymentID, line.LineNo, line.LineType, line.Amount, line.ProviderTransactionID); err != nil {
			return nil, err
		}
	}

	if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, model.PaymentCreated,
		model.PaymentPending, "payment accepted by provider", p.RequestID, model.ActorSystem, nil, nil); err != nil {
		return nil, err
	}
	if err := completeIdempotentOperation(ctx, tx, ScopeCreatePayment, p.RequestID, payment.ID, p.IdempotencyResponse); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return payment, nil
}

// MarkPaymentFailedParams 是发起支付失败那一段的输入。
type MarkPaymentFailedParams struct {
	PaymentID      string
	FailureCode    string
	FailureMessage string
	// IdempotencyResponse 是给客户端看的失败响应快照。
	IdempotencyResponse any
	RequestID           string
}

// MarkPaymentFailed 把一次发起失败落下来。
//
// 失败也要**提交**，不是回滚：payments 表的注释写着「第一次支付失败、超时被关，用户可以
// 再发起一次，那是新的一行」——失败留痕是这条规则的前提，回滚掉就没有「第一次」可查了。
// 失败的形状与成功的技术上完全对称，只是没有出资行、没有 provider_transaction_id。
func (r *PostgresRepository) MarkPaymentFailed(ctx context.Context, p MarkPaymentFailedParams) (*model.Payment, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	payment, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments
		SET status='failed', failure_code=$2, failure_message=$3, updated_at=NOW()
		WHERE id=$1 AND status='created'
		RETURNING `+paymentColumns, p.PaymentID, p.FailureCode, p.FailureMessage))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentNotPending
		}
		return nil, err
	}

	if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, model.PaymentCreated,
		model.PaymentFailed, p.FailureMessage, p.RequestID, model.ActorSystem, nil, nil); err != nil {
		return nil, err
	}
	// 幂等行也要标 failed。order-service 那边失败时幂等行会随事务回滚消失（重试 = 全新
	// 一次尝试），支付这边失败会提交，所以必须显式标它——否则同一个 request_id 重试时
	// 会看到一条 processing，白等 15 分钟才被回收。
	if err := failIdempotentOperation(ctx, tx, ScopeCreatePayment, p.RequestID, payment.ID, p.IdempotencyResponse); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return payment, nil
}

// ErrAccountEntryIDRequired：账户域回了一个空的账变 ID。
//
// 这不是「没拿到就没拿到」的可选字段：account_entry_id 是这笔扣减在本地唯一的落点，
// 空着等于豆扣了而没有痕迹——正是 RecordAccountDeduction 存在的理由。账户域契约保证
// 扣豆成功必回 entry_id，所以空值说明对面出了我们没预期的事，当场报错比默默写个 NULL 好。
var ErrAccountEntryIDRequired = errors.New("account entry id is required to record a bean deduction")

// RecordAccountDeduction 把一次成功的扣豆留痕到支付单上。
//
// 调用点在 createAccountPayment 里，紧跟在账户域返回之后、**在结算之前**。它是一条独立的
// UPDATE，不跟结算共用一个事务——共用的话，结算失败会把这条留痕一起回滚掉，而它要覆盖的
// 恰恰就是结算失败那个窗口（见 005 迁移）。
//
// **故意不带状态判断**：这张单可能已经不在 created/pending 了（超时关单刚把它收走，这正是
// 最需要留痕的那条路），带上 `AND status IN (...)` 会让最该写下的一行写不下去。它不是一次
// 状态迁移，是一次事实登记——「账户域扣了这笔豆」在写下这一刻已经成立，与本地那张单后来
// 怎么样无关。
func (r *PostgresRepository) RecordAccountDeduction(ctx context.Context, paymentID, accountEntryID string, fundedAt time.Time) error {
	if strings.TrimSpace(accountEntryID) == "" {
		return ErrAccountEntryIDRequired
	}
	tag, err := r.pool.Exec(ctx, `UPDATE payments
		SET account_entry_id=$2, account_funded_at=$3, updated_at=NOW()
		WHERE id=$1`, paymentID, accountEntryID, fundedAt)
	if err != nil {
		return mapPGError(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPaymentNotFound
	}
	return nil
}

// SettleAccountPaymentParams 是账户出资（纯豆）成功那一段的输入。
type SettleAccountPaymentParams struct {
	PaymentID string
	// AccountEntryID 是账户域那笔扣减的流水 ID（coffee_bean_entries.id）。它落进
	// payment_fundings.account_entry_id，并随 payment.succeeded 事件一路走到
	// order_payment_lines.account_entry_id——退款与对账都要按它反查这笔账变。
	AccountEntryID string
	// PaidAt 是成交时间。账户出资没有渠道、没有第三方给的成交时间，所以由调用方给
	// （service 用它自己那个可注入的 now）。
	PaidAt              time.Time
	IdempotencyResponse any
	RequestID           string
}

// SettleAccountPayment 在**一个事务里**把一张纯豆支付单推到 succeeded、落出资行与资金
// 流水、写状态流水、发 outbox、完成幂等记录。
//
// 为什么不复用 settleInTx（callback.go）：那个是围绕一条**回调记录**写的——它要标记
// payment_notifications、要拿渠道交易号与建单时那个对一遍、要处理「成功回调打在已关的单上」
// 这条真事故。账户出资没有回调、没有渠道、没有交易号，为它塞一条假的通知记录比多写这四十行
// 更难解释。**两者的事务顺序（改状态 → 出资行 → 资金流水 → 状态流水 → outbox → 幂等）
// 逐条一致，改动任何一处时都要一起改。**
//
// 与 MarkPaymentPending 的差别只有一处：账户出资不会停在 pending。渠道支付要等第三方，
// 所以有 created→pending→（回调）succeeded 三段；这里的钱在我们自己的库里，扣成功就是成功，
// 所以是一次 created→succeeded。判据里仍然带上 pending，是因为并发（另一条路径恰好推进过）
// 下宁可让它成功也不要留一张永远收不了款的单。
func (r *PostgresRepository) SettleAccountPayment(ctx context.Context, p SettleAccountPaymentParams) (*model.Payment, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	payment, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments
		SET status='succeeded', paid_at=$2, failure_code='', failure_message='', updated_at=NOW(),
			account_entry_id=COALESCE(account_entry_id, NULLIF($3,'')::uuid),
			account_funded_at=COALESCE(account_funded_at, $2)
		WHERE id=$1 AND status IN ('created','pending')
		RETURNING `+paymentColumns, p.PaymentID, p.PaidAt, p.AccountEntryID))
	if err != nil {
		// 0 行只有一种解释：这单已经不在能结算的状态了（被关单扫描收走、或者并发下另一条
		// 路径已经结算过）。返回说得清的业务错误，别让它变成一次裸的 pgx.ErrNoRows。
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentNotPending
		}
		return nil, err
	}

	// 出资行直接落成 succeeded，没有 reserved 那一步：reserved 是「钱还没到」的占位，
	// 而豆在账户域已经被扣走了。写成先 reserved 再 succeeded 会让对账看到一段不存在的
	// 等待期，也会让超时关单扫描误以为这笔出资可以释放。
	if _, err := tx.Exec(ctx, `INSERT INTO payment_fundings
		(payment_id, line_no, line_type, amount, status, account_entry_id, succeeded_at)
		VALUES ($1,1,$2,$3,'succeeded',NULLIF($4,'')::uuid,$5)`,
		payment.ID, payment.FundingType, payment.Amount, p.AccountEntryID, p.PaidAt); err != nil {
		return nil, err
	}

	// 资金流水是**不可变的对账基准**（方案 5.9），与渠道那条路同一张表、同一形状。
	// channel_id 落 NULL：账户出资没有渠道行，这正是 channel_id 可空的原因。
	// account_entry_id 也一起写：这一行单独拿出来就能指回账户域那笔扣减，不必 join 出资行。
	if _, err := tx.Exec(ctx, `INSERT INTO payment_transactions
		(kind, payment_no, refund_no, funding_line_no, line_type, direction, amount,
		 channel_id, provider_transaction_id, account_entry_id, occurred_at)
		VALUES ('payment',$1,'',1,$2,'in',$3,$4,'',NULLIF($5,'')::uuid,$6)`,
		payment.PaymentNo, payment.FundingType, payment.Amount, payment.ChannelID,
		p.AccountEntryID, p.PaidAt); err != nil {
		return nil, err
	}

	fundings, err := loadFundings(ctx, tx, payment.ID)
	if err != nil {
		return nil, err
	}

	// from 记 created：这条路上没有 pending 那一刻（见函数头）。request_id 用幂等键本身，
	// 与渠道那条路记通知 id 的位置对应——它让这一行能追回是哪一次请求把它推过去的。
	if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, model.PaymentCreated,
		model.PaymentSucceeded, "payment settled with coffee beans", p.RequestID,
		model.ActorSystem, nil, map[string]any{"accountEntryId": p.AccountEntryID}); err != nil {
		return nil, err
	}

	eventType, payload := paymentEvent(payment, fundings)
	if err := appendOutbox(ctx, tx, eventType, dto.EventVersion, "", payload); err != nil {
		return nil, err
	}

	if err := completeIdempotentOperation(ctx, tx, ScopeCreatePayment, p.RequestID, payment.ID,
		p.IdempotencyResponse); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return payment, nil
}

// ProviderCallParams 是一次渠道调用的留痕（方案 6：所有第三方适配器都要有调用流水）。
type ProviderCallParams struct {
	ChannelID       string
	Provider        string
	Operation       string
	PaymentNo       string
	RequestID       string
	TraceID         string
	AttemptNo       int
	RequestSummary  map[string]any
	ResponseSummary map[string]any
	HTTPStatus      *int
	ProviderCode    string
	ProviderMessage string
	Result          string
	DurationMS      *int
}

// RecordProviderCall 落一条渠道调用流水。
//
// **不走业务事务，失败了也只是一条错误日志**：流水是给运维查的，钱的状态比它重要。
// 让一次流水写入失败连带把支付单的推进回滚掉，是把「查不到」升级成了「收不到钱」。
// 调用方（service）负责记日志，不把它当成操作失败。
func (r *PostgresRepository) RecordProviderCall(ctx context.Context, p ProviderCallParams) error {
	request, err := json.Marshal(p.RequestSummary)
	if err != nil {
		return fmt.Errorf("encode provider call request: %w", err)
	}
	response, err := json.Marshal(p.ResponseSummary)
	if err != nil {
		return fmt.Errorf("encode provider call response: %w", err)
	}
	attempt := p.AttemptNo
	if attempt <= 0 {
		attempt = 1
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_provider_calls
		(channel_id, provider, operation, payment_no, request_id, trace_id, attempt_no,
		 request_summary, response_summary, http_status, provider_code, provider_message,
		 result, duration_ms)
		VALUES (NULLIF($1,'')::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		p.ChannelID, p.Provider, p.Operation, p.PaymentNo, p.RequestID, p.TraceID, attempt,
		request, response, p.HTTPStatus, p.ProviderCode, p.ProviderMessage, p.Result, p.DurationMS)
	return err
}
