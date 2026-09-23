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

// ErrChargeNotFound：这一期扣款不存在（多半是拿了一个我们不认识的渠道单号来对账）。
var ErrChargeNotFound = errors.New("payment agreement charge not found")

// ErrChargeAmountMismatch：通知里报的金额与这一期记的金额对不上。
//
// 它是**拒绝入账**的信号，不是「以通知为准」：金额是签约时与用户约定、又由我们自己在发起
// 扣款时写进报文的，渠道推回来的数若与它不同，说明这两条报文说的不是同一笔（渠道串了单、
// 或者有人拿着一条别的单的通知打到了这个回调上）。两种都不该把这一期结掉。
//
// 老系统没有这道闸：通知里的钱数被直接当成这一期的账。
var ErrChargeAmountMismatch = errors.New("the notified amount does not match the charge amount")

// CreateOrFindChargeParams 是「取或建这一期扣款」的输入。
type CreateOrFindChargeParams struct {
	AgreementID string
	AgreementNo string
	// BizPeriod 是期次（由 membership-service 从订阅的 next_charge_at 派生）。
	BizPeriod string
	Amount    int64
	// OutTradeNo 是**本次调用方现算出来的**商户单号，只有在这一行真的是新建的时候才会被用上
	// ——已有那一行保留它自己那个号。调用方每次都算一个新值是无害的（算出来即丢弃），但这
	// 意味着**它不能省**：省了的话新建那一行就没有号，而扣款结果通知回来时唯一的关联键就是
	// 它（见 payment_agreement_charges.out_trade_no 的列注释）。
	OutTradeNo string
	RequestID  string
}

// CreateOrFindCharge 取这一期扣款那一行；没有就建一行。
//
// 返回值第二个是「这次是新建的」。调用方**不能**拿它当「该不该发起扣款」的判据——那是状态
// 的事（见 service 的状态分流），它只说明这一行是不是刚落地。
//
// # 幂等的落点是库上那条唯一键
//
// `ON CONFLICT (agreement_id, biz_period) DO NOTHING` 让并发下只有一个调用者能建行，其余
// 回读同一行。这一条**不是**为了省一次查询：它是「同一期只扣一次」的根。两个 worker 副本同时
// 扫到同一份到期订阅时，进程内的锁帮不上忙（老系统正是栽在这里，见计划 §六.3），能挡住第二次
// 扣款的只有这条唯一键——两个请求带着各自的 out_trade_no 冲到渠道，但只有一个能拿到行、才对
// 得上通知。
//
// 冲突之后回读那一行时**不加 FOR UPDATE**：调用方拿到的是一个快照，接下来它会试图发起扣款，
// 而那一行会被 MarkChargeAttempt 重新锁住再改。这里握着锁不放会把一次出网调用的时长摊到别的
// 请求身上。
func (r *PostgresRepository) CreateOrFindCharge(ctx context.Context, p CreateOrFindChargeParams) (*model.PaymentAgreementCharge, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	charge, err := scanCharge(tx.QueryRow(ctx, `INSERT INTO payment_agreement_charges
		(agreement_id, agreement_no, biz_period, amount, out_trade_no)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (agreement_id, biz_period) DO NOTHING
		RETURNING `+chargeColumns,
		p.AgreementID, p.AgreementNo, p.BizPeriod, p.Amount, p.OutTradeNo))
	switch {
	case err == nil:
		// from 传空串：与建协议那一步同形，让这一期的完整历史从第一行起就能读出来
		// （「谁建的、什么时候、金额多少」——金额若在后续版本里变过，流水上的这一行是原始值）。
		if err := recordTransition(ctx, tx, model.AggregateCharge, charge.ID, "",
			model.ChargeStatusPending, "charge created", p.RequestID, model.ActorSystem, nil,
			map[string]any{"agreementNo": p.AgreementNo, "bizPeriod": p.BizPeriod, "amount": p.Amount}); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return charge, true, nil

	case errors.Is(err, pgx.ErrNoRows):
		// 冲突了：这一期已经有人建过。回读它，**并把这次现算的 out_trade_no 丢掉**——这正是
		// 「重试复用同一个号」那句话在代码里的样子（见 model.PaymentAgreementCharge.OutTradeNo）。
		existing, err := scanCharge(tx.QueryRow(ctx, `SELECT `+chargeColumns+`
			FROM payment_agreement_charges WHERE agreement_id = $1 AND biz_period = $2`,
			p.AgreementID, p.BizPeriod))
		if err != nil {
			return nil, false, mapChargeError(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return existing, false, nil

	default:
		return nil, false, mapChargeError(err)
	}
}

// FindChargeByTradeNo 按商户单号读一期扣款。**不加锁**：它是给人看的（后台与排查），
// 真正要改这一行的那条路自己会锁（见 SettleChargeNotification）。
func (r *PostgresRepository) FindChargeByTradeNo(ctx context.Context, outTradeNo string) (*model.PaymentAgreementCharge, error) {
	return scanCharge(r.pool.QueryRow(ctx,
		`SELECT `+chargeColumns+` FROM payment_agreement_charges WHERE out_trade_no = $1`, outTradeNo))
}

// ChargeAttemptParams 是「记一次尝试」的输入。
type ChargeAttemptParams struct {
	ChargeID string
	// Status 是这次尝试之后这一期的状态，取 model.ChargeStatusCharging（渠道受理了，等通知）
	// 或 model.ChargeStatusFailed（渠道当场拒了）。
	//
	// **没有 succeeded**：扣到钱这件事只有渠道的通知能说，受理不是吗（见
	// model.ChargeStatusCharging 与计划 §六.1）。
	Status string
	// ProviderTransactionID 是渠道受理时给的流水号。同步被拒时通常为空，空串不覆盖已有的值
	// （同一期的上一次尝试可能留下过一个号——一个已受理的尝试后来又失败了，那个号仍然要留着
	// 给人查）。
	ProviderTransactionID string
	FailureCode           string
	FailureMessage        string
	// NextRetryAt 是下一次可以再试的时间；nil 表示不再重试（已试满 model.ChargeMaxAttempts）。
	// 由调用方用 model.ChargeNextRetryAt 算，**算出来再传进来**是为了让这一层的判定保持是
	// 「写什么」，而不是「该不该重试」。
	NextRetryAt *time.Time
	RequestID   string
	TraceID     string
}

// MarkChargeAttempt 在一个事务里把一次尝试落到那一行上：attempt_count 加一、状态往前推、
// 记失败原因与退避时间，写一条状态流水；**确定失败的尝试还要发 charge_failed 事件**。
//
// # 为什么 attempt_count 在锁内加
//
// 它是「还剩几次可以试」的唯一凭据，而退避与封顶都由它派生（model.ChargeNextRetryAt）。
// 在事务外读、事务内加，两个并发的重试会各自读到同一个旧值——两边的退避都算成同一个时间点，
// 而封顶那条判据（>= 3）会晚一次才生效。所以锁住那一行、让 UPDATE 自己 `attempt_count + 1`。
//
// # 受理与失败共用这一个入口
//
// 两条路都要加一次尝试数、都要写一条流水，差别只在几个字段上。分成两个方法的话，「哪些字段在
// 受理时该写」这件事会被答两遍。
//
// # 只有失败发事件
//
// 受理（charging）**不发**：受理不是结论，而事件是给消费方推进续费的凭据——收到一条
// 「受理了」就去续一个月，正是老系统那个 bug 的升级版。失败要发，因为「连续失败到该停扣」
// 这件事只有消费方（membership-service）能决定，而它只知道事件。
//
// 于是同一次扣款的失败有两条入口会发这条事件：这一条（渠道当场拒），以及扣款结果通知那条
// （渠道事后说没扣成）。两条都会走到库上同一行的状态机，而**同一期只会有一条到达 failed**
// ——applyChargeTarget 与这里都拒绝把已失败的一期再推一次。
func (r *PostgresRepository) MarkChargeAttempt(ctx context.Context, p ChargeAttemptParams) (*model.PaymentAgreementCharge, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, userID, err := lockChargeForAttempt(ctx, tx, p.ChargeID)
	if err != nil {
		return nil, err
	}

	updated, err := scanCharge(tx.QueryRow(ctx, `UPDATE payment_agreement_charges
		SET status=$2,
		    attempt_count=attempt_count+1,
		    provider_transaction_id=COALESCE(NULLIF($3,''), provider_transaction_id),
		    failure_code=$4,
		    failure_message=$5,
		    next_retry_at=$6,
		    updated_at=NOW()
		WHERE id=$1
		RETURNING `+chargeColumns,
		p.ChargeID, p.Status, p.ProviderTransactionID, p.FailureCode, p.FailureMessage, p.NextRetryAt))
	if err != nil {
		return nil, mapChargeError(err)
	}

	if err := recordTransition(ctx, tx, model.AggregateCharge, p.ChargeID,
		before.Status, p.Status, chargeAttemptReason(p), p.RequestID, model.ActorSystem, nil,
		chargeAttemptMetadata(p)); err != nil {
		return nil, err
	}

	if updated.Status == model.ChargeStatusFailed {
		if err := appendOutbox(ctx, tx, chargeEventType(updated.Status), dto.EventVersion, p.TraceID,
			chargeEvent(updated, userID)); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// ChargeNotificationParams 是把一条扣款结果通知落进业务事务的输入。
type ChargeNotificationParams struct {
	// NotificationID 是 payment_notifications.id，在同一个事务里被标成 processed。
	NotificationID string
	// Provider 是**收到这条通知的那条渠道**（通知 URL 里那段），不是报文里说的任何东西。
	Provider string
	// OutTradeNo 是报文里的商户单号，也是定位这一期的**唯一**键——扣款通知里没有协议号、
	// 没有期次（见 payment_agreement_charges.out_trade_no 的列注释）。
	OutTradeNo string
	// ProviderTransactionID 是渠道侧的流水号，成功时非空。
	ProviderTransactionID string
	// Amount 是通知里报的金额，单位分。**必须与这一期记的金额一致**，否则拒绝入账
	// （见 ErrChargeAmountMismatch）。
	Amount int64
	// Target 是这条通知说的结果，取 model.ChargeStatusSucceeded 或 model.ChargeStatusFailed。
	Target         string
	FailureCode    string
	FailureMessage string
	Reason         string
	TraceID        string
}

// ChargeSettlement 是一条扣款结果通知落地之后的结果。
type ChargeSettlement struct {
	Charge *model.PaymentAgreementCharge
	// Changed 表示这次真的推进了这一期。false 时**不发事件**——事件是「结果变了」的广播，
	// 结果没变就没有可广播的东西（与 AgreementSettlement.Changed 同一条规矩）。
	//
	// 它同时是**重复通知的第二道防线**：第一道是 UNIQUE (provider, notification_id)，但那条
	// 只挡得住一模一样的报文；渠道对同一次扣款推两份内容略有差别的报文（时间戳不同）时，两条
	// 都会落库，而在这里被同一条判据挡住第二次。
	Changed bool
}

// SettleChargeNotification 在**一个事务里**把一条扣款结果通知落到那一期、状态流水与 outbox 上。
//
// 形状照抄 SettleAgreementNotification（repository/agreement.go:282），判据按扣款这件事改：
//
//  1. 按 out_trade_no 锁住那一期（FOR UPDATE OF c，只锁扣款行；协议行只读不锁——扣款结果
//     不改协议，协议仍然是 active）。
//  2. 渠道对不上 → 报错不 ack（见 notificationChannelMatches）。**这里读的是协议上的渠道**
//     ——扣款行自己没有 provider 列，而一份协议的扣款只可能来自它签约的那条渠道。
//  3. 金额对不上 → 报错不 ack（见 ErrChargeAmountMismatch）。**这条判据必须在锁内**：放到
//     service 里先读一次再进来，中间那一瞬正好是另一个通知改这一行的时候。
//  4. 只有 pending / charging 能推进。已是 succeeded / failed / skipped / cancelled 的行一律
//     不动——重复投递落在这里，而「已结算的结果被另一条通知翻过来」是本表最不能发生的事。
//  5. 改了才写 outbox，无论改没改都把通知标 processed。
func (r *PostgresRepository) SettleChargeNotification(ctx context.Context, p ChargeNotificationParams) (*ChargeSettlement, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	charge, provider, userID, err := lockChargeForNotification(ctx, tx, p.OutTradeNo)
	if err != nil {
		return nil, err
	}
	if err := notificationChannelMatches(provider, p.Provider); err != nil {
		return nil, err
	}
	if p.Amount != charge.Amount {
		return nil, fmt.Errorf("%w: notified %d but the charge is %d (out_trade_no %s)",
			ErrChargeAmountMismatch, p.Amount, charge.Amount, p.OutTradeNo)
	}

	updated, changed, err := applyChargeTarget(ctx, tx, charge, p)
	if err != nil {
		return nil, err
	}

	if changed {
		if err := appendOutbox(ctx, tx, chargeEventType(updated.Status), dto.EventVersion, p.TraceID,
			chargeEvent(updated, userID)); err != nil {
			return nil, err
		}
	}
	if err := markNotificationInTx(ctx, tx, p.NotificationID, model.NotificationProcessed, ""); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &ChargeSettlement{Charge: updated, Changed: changed}, nil
}

// applyChargeTarget 是扣款那条状态机的落点。
//
// 判据只有一条：**只有还在路上的那一期能被推进**（pending / charging → succeeded / failed）。
// 其余一律不动，且**不是报错**——重复投递会走到这里，而它该是一个安静的无变化（调用方据
// Changed=false 不发事件）。
//
// # 为什么不在这里补记 attempt_count
//
// 尝试次数在发起扣款时就加过了（MarkChargeAttempt）。一条结果通知不是一次新的尝试，它说的是
// 某一次已经发生过的尝试的结局；在这里再加一次，会把「这一期问过渠道几次」算成两倍——而那个
// 数正是封顶判据（model.ChargeMaxAttempts）的输入。
func applyChargeTarget(ctx context.Context, tx pgx.Tx, charge *model.PaymentAgreementCharge, p ChargeNotificationParams) (*model.PaymentAgreementCharge, bool, error) {
	if charge.Status != model.ChargeStatusPending && charge.Status != model.ChargeStatusCharging {
		return charge, false, nil
	}

	switch p.Target {
	case model.ChargeStatusSucceeded:
		updated, err := scanCharge(tx.QueryRow(ctx, `UPDATE payment_agreement_charges
			SET status='succeeded',
			    provider_transaction_id=COALESCE(NULLIF($2,''), provider_transaction_id),
			    failure_code='',
			    failure_message='',
			    next_retry_at=NULL,
			    charged_at=NOW(),
			    updated_at=NOW()
			WHERE id=$1
			RETURNING `+chargeColumns, charge.ID, p.ProviderTransactionID))
		if err != nil {
			return nil, false, err
		}
		if err := recordTransition(ctx, tx, model.AggregateCharge, charge.ID,
			charge.Status, model.ChargeStatusSucceeded, p.Reason, "", model.ActorSystem, nil,
			chargeNotificationMetadata(p)); err != nil {
			return nil, false, err
		}
		return updated, true, nil

	case model.ChargeStatusFailed:
		// 退避与封顶在这一刻定下来：这一期**这一轮**的结局是「失败」，但还试不试、隔多久再试，
		// 由 attempt_count 决定（model.ChargeNextRetryAt）。写 NULL 表示不再重试。
		//
		// 下一次尝试仍会走到这一行上（MarkChargeAttempt 把 failed 推回 charging），这正是
		// 「重试复用同一行」那条规矩——新开一行就可能扣两次。
		nextRetryAt := model.ChargeNextRetryAt(time.Now(), charge.AttemptCount)
		updated, err := scanCharge(tx.QueryRow(ctx, `UPDATE payment_agreement_charges
			SET status='failed',
			    provider_transaction_id=COALESCE(NULLIF($2,''), provider_transaction_id),
			    failure_code=$3,
			    failure_message=$4,
			    next_retry_at=$5,
			    updated_at=NOW()
			WHERE id=$1
			RETURNING `+chargeColumns,
			charge.ID, p.ProviderTransactionID, p.FailureCode, p.FailureMessage, nextRetryAt))
		if err != nil {
			return nil, false, err
		}
		if err := recordTransition(ctx, tx, model.AggregateCharge, charge.ID,
			charge.Status, model.ChargeStatusFailed, p.Reason, "", model.ActorSystem, nil,
			chargeNotificationMetadata(p)); err != nil {
			return nil, false, err
		}
		return updated, true, nil

	default:
		// 词表外的目标状态是**我们自己的编码错误**。报错而不是当成「不用改」：后者的后果是
		// 一条本该被结掉的通知安静地过去，而这一期永远停在 charging 上等人去查。
		return nil, false, fmt.Errorf("unknown charge target status %q", p.Target)
	}
}

// lockChargeForNotification 按商户单号找出这一期，锁住它，并顺带把判定要用的两件事读出来：
// 这份协议的渠道，以及签约用户（事件体里要带，消费方拿它对订阅的 user_id）。
//
// `FOR UPDATE OF c` 只锁扣款行。协议那一行是**不该被锁**的：这份协议上的其他期扣款可能正在
// 别的请求里推进，而这一条通知与它们没有关系。读到的是 a.provider 与 a.user_id 两个建行之后
// 就不再变的值，不锁也读得对。
func lockChargeForNotification(ctx context.Context, tx pgx.Tx, outTradeNo string) (*model.PaymentAgreementCharge, string, string, error) {
	if outTradeNo == "" {
		// 扣款通知**必须**带 out_trade_no——它是报文里唯一能定位到这一行的东西。到这个函数时
		// 验签已经过了，所以这不是伪造，而是一条我们认不出对象的通知：只能拒，让人去看报文
		// （老系统收到的续费通知里带的是另一个号，正是这条判据缺失的后果）。
		return nil, "", "", fmt.Errorf("%w: the notification carries no out_trade_no", ErrChargeNotFound)
	}

	var (
		charge   model.PaymentAgreementCharge
		provider string
		userID   string
	)
	err := tx.QueryRow(ctx, `SELECT c.id::text, c.agreement_id::text, c.agreement_no, c.biz_period,
			c.amount, c.out_trade_no, c.provider_transaction_id, c.payment_no, c.status,
			c.attempt_count, c.next_retry_at, c.failure_code, c.failure_message, c.charged_at,
			c.created_at, c.updated_at, a.provider, a.user_id
		FROM payment_agreement_charges c
		JOIN payment_agreements a ON a.id = c.agreement_id
		WHERE c.out_trade_no = $1
		FOR UPDATE OF c`, outTradeNo).Scan(&charge.ID, &charge.AgreementID, &charge.AgreementNo,
		&charge.BizPeriod, &charge.Amount, &charge.OutTradeNo, &charge.ProviderTransactionID,
		&charge.PaymentNo, &charge.Status, &charge.AttemptCount, &charge.NextRetryAt,
		&charge.FailureCode, &charge.FailureMessage, &charge.ChargedAt, &charge.CreatedAt,
		&charge.UpdatedAt, &provider, &userID)
	if err != nil {
		return nil, "", "", mapChargeError(err)
	}
	return &charge, provider, userID, nil
}

// lockChargeForAttempt 锁住一期扣款，供「记一次尝试」那条路读改前的状态（流水上的 from），
// 并把签约用户带出来——确定失败时要发的那条事件体里要有它。
//
// userID 在 payment_agreements 上，所以要 join 一次。**只锁扣款行**（FOR UPDATE OF c）：
// 协议那一行这一路上不会被改，锁它只会让同一份协议上并发的其他期扣款互相等。
func lockChargeForAttempt(ctx context.Context, tx pgx.Tx, id string) (*model.PaymentAgreementCharge, string, error) {
	var (
		charge model.PaymentAgreementCharge
		userID string
	)
	err := tx.QueryRow(ctx, `SELECT c.id::text, c.agreement_id::text, c.agreement_no, c.biz_period,
			c.amount, c.out_trade_no, c.provider_transaction_id, c.payment_no, c.status,
			c.attempt_count, c.next_retry_at, c.failure_code, c.failure_message, c.charged_at,
			c.created_at, c.updated_at, a.user_id
		FROM payment_agreement_charges c
		JOIN payment_agreements a ON a.id = c.agreement_id
		WHERE c.id = $1
		FOR UPDATE OF c`, id).Scan(&charge.ID, &charge.AgreementID, &charge.AgreementNo,
		&charge.BizPeriod, &charge.Amount, &charge.OutTradeNo, &charge.ProviderTransactionID,
		&charge.PaymentNo, &charge.Status, &charge.AttemptCount, &charge.NextRetryAt,
		&charge.FailureCode, &charge.FailureMessage, &charge.ChargedAt, &charge.CreatedAt,
		&charge.UpdatedAt, &userID)
	if err != nil {
		return nil, "", mapChargeError(err)
	}
	return &charge, userID, nil
}

// chargeEventType 把结果翻成事件类型。两条事件成对取名（见 dto 的注释），所以这里的 else
// 分支只能是 succeeded 之外的失败态——发失败事件是**保守**的那一侧：消费方据此累计失败次数、
// 该停就停，比发一个下游读不懂的事件（或者不发、让订阅一直等一个不会来的成功）都安全。
func chargeEventType(status string) string {
	if status == model.ChargeStatusSucceeded {
		return dto.EventAgreementChargeSucceeded
	}
	return dto.EventAgreementChargeFailed
}

// chargeEvent 把一期扣款翻成那份事件体。字段的取舍见 dto.AgreementChargeEventPayload。
func chargeEvent(charge *model.PaymentAgreementCharge, userID string) dto.AgreementChargeEventPayload {
	return dto.AgreementChargeEventPayload{
		AgreementID:           charge.AgreementID,
		AgreementNo:           charge.AgreementNo,
		UserID:                userID,
		BizPeriod:             charge.BizPeriod,
		Amount:                charge.Amount,
		ProviderTransactionID: charge.ProviderTransactionID,
		Status:                charge.Status,
		FailureCode:           charge.FailureCode,
		FailureMessage:        charge.FailureMessage,
	}
}

// chargeAttemptReason 把一次尝试写成一句给人看的话，同时进状态流水与失败原因。
func chargeAttemptReason(p ChargeAttemptParams) string {
	if p.Status == model.ChargeStatusCharging {
		return "charge accepted by provider, waiting for the result notification"
	}
	if p.FailureMessage != "" {
		return "charge rejected by provider: " + p.FailureMessage
	}
	return "charge rejected by provider"
}

// chargeAttemptMetadata 是一次尝试的流水元数据。
//
// **不放商户单号以外的报文内容**：签名原文与密钥不进来（方案 11.5），要完整报文的人去
// payment_provider_calls 那一行看脱敏摘要。nextRetryAt 为 nil 时不写这个键——写一个 null 会让
// 「不再重试」与「这个字段没填」长得一样。
func chargeAttemptMetadata(p ChargeAttemptParams) map[string]any {
	metadata := map[string]any{"status": p.Status}
	if p.ProviderTransactionID != "" {
		metadata["providerTransactionId"] = p.ProviderTransactionID
	}
	if p.FailureCode != "" {
		metadata["failureCode"] = p.FailureCode
	}
	if p.NextRetryAt != nil {
		metadata["nextRetryAt"] = p.NextRetryAt
	}
	return metadata
}

// ListChargesByAgreementNo 读一份协议下的**全部期次**，按期次升序。
//
// 它是给「订阅详情」那一页看的：那份订阅每一期扣了没、扣了多少、失败的原因是什么。
// 只读、无锁、不分页——一份协议的期数是订阅活着多少个月，而它今天最多几十行（一个月一行，
// 加失败重试不会多出期次：重试落在同一行上，attempt_count 加一）。
//
// # 为什么按协议号查而不是协议 ID
//
// 调用方（membership-service）手里那份副本就是协议号（membership_subscriptions.contract_code），
// 而 UUID 是支付库内部的主键。让调用方先拿号换一次 ID 再多查一次，只是把一次索引查找变成
// 两次——而 agreement_no 上是唯一索引，这一条本身就是一次索引查找。
//
// # 为什么升序
//
// 期次是 yyyyMMdd（业务方派生，见 model.PaymentAgreementCharge.BizPeriod），字典序就是时间序。
// 调用方拿它当时间轴渲染，「最早的在前」是那一页唯一说得通的排法。
//
// # 它为什么在这个接口里（而不是像后台那几个读一样单开一个）
//
// 因为它是**这一族写路径自己的读**：CreateOrFindCharge / MarkChargeAttempt /
// SettleChargeNotification 三个写方法与它读的是同一张表、同一个聚合，而「这一期现在怎样」
// 本来就是发起扣款那条路要回答的问题。后台那几个读单开一个接口（见 admin_query.go）是因为
// 它们是**另一个消费者**的另一张页面，与这一条不是同一种东西。
func (r *PostgresRepository) ListChargesByAgreementNo(ctx context.Context, agreementNo string) ([]*model.PaymentAgreementCharge, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+chargeColumns+` FROM payment_agreement_charges
		WHERE agreement_no = $1 ORDER BY biz_period`, agreementNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 非 nil 的空切片：调用方（以及它的 JSON 响应）要的是 `[]` 而不是 `null`，
	// 而 nil 切片在编码时正是后者。
	charges := make([]*model.PaymentAgreementCharge, 0, 12)
	for rows.Next() {
		charge, err := scanCharge(rows)
		if err != nil {
			return nil, err
		}
		charges = append(charges, charge)
	}
	return charges, rows.Err()
}

// chargeNotificationMetadata 是一条结果通知的流水元数据。与上面那份的差别只有一条：这条路
// 多一个「是哪条通知说的」，而那正是排查时要的第一样东西。
func chargeNotificationMetadata(p ChargeNotificationParams) map[string]any {
	if p.NotificationID == "" {
		return nil
	}
	return map[string]any{"notificationId": p.NotificationID}
}

// mapChargeError 把「查无此期」翻成业务错误。与 mapAgreementError 同形，按错误类型判而不是
// 按错误文本（PG 换措辞或换语言时那种判法会静默失效）。
//
// 唯一约束冲突**不翻**：out_trade_no 与 provider_transaction_id 那两条唯一索引一旦撞上，
// 含义是「这一期的号被别人占了」，那是要人来看的事故（见 payment_agreement_charges.provider_transaction_id 的列注释），原样报出去比
// 翻成一个业务错误更能让人停下。
func mapChargeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChargeNotFound
	}
	return err
}

// chargeColumns 是 payment_agreement_charges 的列清单，顺序与 scanCharge 严格一一对应
// （UUID 列照旧 ::text）。
const chargeColumns = `id::text, agreement_id::text, agreement_no, biz_period, amount,
	out_trade_no, provider_transaction_id, payment_no, status, attempt_count, next_retry_at,
	failure_code, failure_message, charged_at, created_at, updated_at`

func scanCharge(row scanner) (*model.PaymentAgreementCharge, error) {
	item := &model.PaymentAgreementCharge{}
	err := row.Scan(&item.ID, &item.AgreementID, &item.AgreementNo, &item.BizPeriod, &item.Amount,
		&item.OutTradeNo, &item.ProviderTransactionID, &item.PaymentNo, &item.Status,
		&item.AttemptCount, &item.NextRetryAt, &item.FailureCode, &item.FailureMessage,
		&item.ChargedAt, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}
