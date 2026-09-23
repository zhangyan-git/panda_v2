package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// ErrProviderTransactionMismatch：回调里的渠道交易号与建单时记下的那个对不上。
//
// 签名之外的第二道：签名证明「这条回调是我们的密钥签的」，但一把泄露的密钥（或一次
// 渠道侧的串号）能签出一条指向**别人那笔支付**的合法回调。交易号对不上就说明这条回调
// 说的不是这一单。
var ErrProviderTransactionMismatch = errors.New("payment notification provider transaction id does not match the payment")

// ErrPaymentChannelMismatch：这条回调的渠道与支付单建单时挂着的渠道不是同一条。
//
// 签名之外的另一道，管的是签名管不了的那件事：密钥是**按渠道**配的，所以 B 渠道的密钥
// 签得出一条 status 合法的报文，而报文里的 payment_no 指向一张挂在 A 渠道上的支付单——
// 签名验得过、金额也可能正好对得上（同一家门店、同一个价位）。签名回答的是「这条报文是
// 不是 B 渠道发的」，它回答不了「这一单是不是 B 渠道的单」。判据在**支付单**那一侧：
// payments.provider 是建单时按支付方式钉下来的，回调没有任何办法影响它。
//
// 与 ErrProviderTransactionMismatch 是两条独立的防线，各自拦得住对方拦不住的东西：那一条
// 靠渠道给的交易号（B 渠道可以把 A 渠道的交易号写进签名报文里，那时只有本错误拦得住）；本
// 错误靠我们自己的数据（B 渠道自编一个交易号时，只有那一条拦得住）。两道都过不了才结算。
var ErrPaymentChannelMismatch = errors.New("payment notification channel does not match the payment channel")

// notificationChannelMatches 判断回调来的渠道与支付单的渠道是不是同一条。
//
// 判据是「必须能证明两边是同一条」，不是「两边不同就拒」——所以三种输入都收在下面，且除了
// 「两边都非空且相等」以外一律拒绝：
//
//   - 通知没带渠道（空串）：证明不了，拒。今天 service 那条路必然带上（回调 URL 里那段
//     就是渠道名），所以这是一个走不到的兜底；写出来是为了让这个函数在任何调用点都
//     fail-closed。
//   - 支付单没有渠道：账户出资（咖啡豆）的单 provider 为空（见 create.go 的 paymentRoute）。
//     它**本来就不该收到渠道回调**——回调地址是公开的，任何人 POST 一份签名正确的报文就能把
//     一张用豆付的单推成 succeeded，而豆那边该扣的已经扣了。拒。
//   - 两条渠道不同：拒。比的是**渠道名**（provider，如 `ums`），因为那是今天唯一还在的事实：
//     payments.provider 与回调 URL 里那一段都是它。从前这里比的是渠道行的 uuid，而那个 id
//     随 payment_channels 一起没了——今天能指向渠道的东西只剩这个名字。
//
// 它收的是「本地那一行挂着的渠道」而不是一张支付单，所以签约通知那条路也用同一个函数
// （payment_agreements.provider 与 payments.provider 是同一套渠道名）。两条路各写一份的话，
// 总有一天会有一条漏掉「账户出资没有渠道」那一支。
func notificationChannelMatches(recordProvider, notifiedProvider string) error {
	if notifiedProvider == "" {
		return fmt.Errorf("%w: the notification does not carry a channel", ErrPaymentChannelMismatch)
	}
	if recordProvider == "" {
		return fmt.Errorf("%w: the notification arrived on channel %s but the local record has no channel (account funded)",
			ErrPaymentChannelMismatch, notifiedProvider)
	}
	if recordProvider != notifiedProvider {
		return fmt.Errorf("%w: the local record belongs to channel %s but the notification arrived on channel %s",
			ErrPaymentChannelMismatch, recordProvider, notifiedProvider)
	}
	return nil
}

// NotificationParams 是一条渠道回调的落库口径。
type NotificationParams struct {
	// Provider 是这条回调来自哪条渠道（回调 URL 里那段），也是 payment_notifications.provider。
	//
	// 从前这里还有一个 ChannelID（渠道行的 uuid）。两者记的是同一件事，而 id 指向的那张表
	// 已经不存在，所以只剩这一个。
	Provider          string
	NotificationID    string
	EventType         string
	PaymentNo         string
	Body              []byte
	BodySHA256        string
	Headers           map[string]string
	SignatureVerified bool
	Status            string
	FailureReason     string
}

// NotificationRecord 是一条回调落库的结果。
type NotificationRecord struct {
	// ID 是 payment_notifications.id。**已有记录时也返回它**：调用方要拿它去更新那一行
	// 的状态（比如把一条重复投递的失败通知标成 ignored）。
	ID string
	// Inserted 为 false 表示这条通知已经在了（渠道重投）。
	Inserted bool
	// ExistingStatus 只在 Inserted=false 时有意义：那一行现在的状态。
	//
	// 调用方**必须**看它，不能见到重复就一律回成功应答：
	//   - processed / ignored → 上次确实处理完了，回成功应答，不碰状态；
	//   - failed → 上次我们拒了这条通知，这次也得拒（否则渠道会以为我们收下了，
	//     而那条 failed 行还挂着等人查）；
	//   - received，或读不到（并发下另一条投递还没提交）→ 上次的处理可能正飞在半路，
	//     让渠道稍后再投，别替一次还没落地的处理做决定。
	//
	// 空串表示「冲突了但读不到那一行」——那是并发，按上面第三条处理。
	ExistingStatus string
	// ExistingAgeSeconds 只在 Inserted=false 且读到了那一行时有意义：那一行从落库到现在
	// 过了多久。调用方拿它把 received 分成「还在飞」与「死在半路」两种——后者是进程被
	// kill、机器掉电留下的孤儿行，没有任何东西会去动它，一直按「稍后再投」答的话渠道投满
	// 重试次数就放弃了，而钱已经收了。
	//
	// 用**数据库自己的钟**算（EXTRACT(EPOCH FROM NOW() - received_at)），不拿应用进程的钟
	// 去减 received_at：两边时钟不同步时，那个差值会让一条刚落库的通知看起来像很久以前的，
	// 于是被当成孤儿重复结算。
	ExistingAgeSeconds int64
}

// InsertNotification 落一条回调原文。
//
// UNIQUE (provider, notification_id) 是防重放的那道闸：渠道重投是常态（它没收到我们的
// 应答就会重发），所以重复不是错误，而是一个有明确处理方式的状态（见 ExistingStatus）。
//
// Body 是渠道原始报文，只落在这一张表里：方案 11.5 禁止未脱敏的完整回调报文进日志。
func (r *PostgresRepository) InsertNotification(ctx context.Context, p NotificationParams) (*NotificationRecord, error) {
	headers, err := json.Marshal(p.Headers)
	if err != nil {
		return nil, fmt.Errorf("encode notification headers: %w", err)
	}
	var id string
	// 列里**没有 channel_id**：这一行记的是渠道名（provider），那张表已经不存在了。
	err = r.pool.QueryRow(ctx, `INSERT INTO payment_notifications
		(provider, notification_id, event_type, payment_no, body, body_sha256,
		 headers, signature_verified, status, failure_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (provider, notification_id) DO NOTHING
		RETURNING id::text`,
		p.Provider, p.NotificationID, p.EventType, p.PaymentNo, p.Body,
		p.BodySHA256, headers, p.SignatureVerified, p.Status, p.FailureReason).Scan(&id)
	if err == nil {
		return &NotificationRecord{ID: id, Inserted: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// ON CONFLICT DO NOTHING 没有 RETURNING 行 = 这条通知已经在了。再读一次拿它的状态：
	// 这是**另一条语句**，在 READ COMMITTED 下拿的是新快照，所以刚提交的那一行看得见。
	// 看不见只可能是「另一条投递还在事务里没提交」——那不是错误，留给调用方按并发处理。
	var existingStatus string
	var existingAgeSeconds int64
	if err := r.pool.QueryRow(ctx, `SELECT id::text, status,
		EXTRACT(EPOCH FROM (NOW() - received_at))::bigint FROM payment_notifications
		WHERE provider=$1 AND notification_id=$2`, p.Provider, p.NotificationID).
		Scan(&id, &existingStatus, &existingAgeSeconds); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &NotificationRecord{}, nil
		}
		return nil, err
	}
	return &NotificationRecord{ID: id, ExistingStatus: existingStatus, ExistingAgeSeconds: existingAgeSeconds}, nil
}

// SettleNotificationParams 是把一条回调落进业务事务的输入。
type SettleNotificationParams struct {
	// NotificationID 是 payment_notifications.id，在同一个事务里被标成 processed——
	// 回调记录与支付状态必须一起变，否则会出现「钱的状态变了但回调还挂着 received」。
	NotificationID string
	PaymentNo      string
	// Provider 是**收到这条回调的那条渠道**（回调 URL 里那段，见 service.HandleNotification），
	// 不是报文里说的任何东西。它用来与支付单上的 provider 对一遍（见
	// notificationChannelMatches）：签名证明得了「这条报文是这条渠道签的」，证明不了
	// 「这一单是这条渠道的单」。
	Provider string
	// Succeeded 为 false 表示这是一条失败通知。
	Succeeded             bool
	Amount                int64
	ProviderTransactionID string
	// PaidAt 是渠道给的成交时间，零值表示渠道没给，用 NOW() 兜底。
	PaidAt         time.Time
	FailureCode    string
	FailureMessage string
	TraceID        string
}

// PaymentSettlement 是一次回调落地之后的全部结果。
type PaymentSettlement struct {
	Payment  *model.Payment
	Fundings []*model.PaymentFunding
	// AlreadySettled 表示这单早就到终态了：重复回调、或者一条迟到的失败通知打在已经
	// 成功的单上。调用方遇到它**不发事件**——事件是「状态变了」的广播，状态没变就没有
	// 可广播的东西，再发一次只会让 order-service 的幂等去兜底。
	AlreadySettled bool
}

// SettlePayment 在**一个事务里**把渠道回调的结论落到支付单、出资行、资金流水、状态流水
// 与 outbox 上。
//
// 顺序与判据（每一条都是「宁可拒绝也不猜」）：
//
//  1. 锁住支付单（FOR UPDATE）。并发下的判断只有锁内这一次算数。
//  2. 已经是终态 → 幂等返回；但**成功回调打在失败/关单上必须报错不 ack**
//     （ErrPaymentAlreadySettled）：钱收了、单关了，这是真事故，回渠道一个「收到」
//     会让这条线索永久消失。
//  3. 渠道对不上 → 报错不 ack（见 ErrPaymentChannelMismatch）。放在金额之前：金额对不上的
//     前提是「这一单本来该由这条渠道收钱」，而渠道对不上时那个前提不成立，报出来的金额差异
//     会把人引到错误的结论上。放在「已经是终态」之前：一条走错门的回调不该因为目标单恰好
//     已经成功就被回一句「收到」——那会让渠道把它从重投队列里划掉，而这条错投没有留下任何
//     需要人看的东西。
//  4. 金额对不上 → 报错不 ack。与 order-service 的 SettlePayment 同一条约定：一个错
//     的事件把订单标成已支付，比一次失败难查得多。
//  5. 渠道交易号对不上 → 报错不 ack（见 ErrProviderTransactionMismatch）。
//  6. 改状态、落流水、写 outbox，全部在同一事务里。事务回滚的事件不能发出去，提交了的
//     事件也不能因为 broker 当时不通就丢。
func (r *PostgresRepository) SettlePayment(ctx context.Context, p SettleNotificationParams) (*PaymentSettlement, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	settlement, err := settleInTx(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return settlement, nil
}

func settleInTx(ctx context.Context, tx pgx.Tx, p SettleNotificationParams) (*PaymentSettlement, error) {
	payment, err := scanPayment(tx.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE payment_no = $1 FOR UPDATE`, p.PaymentNo))
	if err != nil {
		return nil, mapPGError(err)
	}

	// 渠道一致性（见 ErrPaymentChannelMismatch）。在锁内做、在下面每一个分支之前：对不上的
	// 回调既不改状态、也不 markNotification，因为调用方会把它标成 failed（见
	// service.HandleNotification 的错误出口），那正是我们要留的痕。
	//
	// 它**读的是支付单那一侧的事实**，与报文说的任何东西无关，所以放在验签之后、金额之前
	// 是安全的：报文里唯一被用到的字段是 payment_no（它就是这一行被读出来的原因）。
	if err := notificationChannelMatches(payment.Provider, p.Provider); err != nil {
		return nil, err
	}

	target := model.PaymentFailed
	if p.Succeeded {
		target = model.PaymentSucceeded
	}

	if payment.Status == target {
		// 同一个结论来第二次：标 processed 然后走人，不改任何状态、不发事件。
		if err := markNotificationInTx(ctx, tx, p.NotificationID, model.NotificationProcessed, ""); err != nil {
			return nil, err
		}
		fundings, err := loadFundings(ctx, tx, payment.ID)
		if err != nil {
			return nil, err
		}
		return &PaymentSettlement{Payment: payment, Fundings: fundings, AlreadySettled: true}, nil
	}
	if !canSettle(payment.Status, target) {
		if p.Succeeded {
			// 成功回调打在一个已经关掉/失败的单上：报错不 ack。
			//
			// 这里的「不 ack」对 HTTP 回调意味着**回一个失败应答**（我们没有 MQ 那种 DLQ，
			// 见 controller/callback.go 的说明）：渠道会重投，我们这边的落点是
			// payment_notifications 里一条 status='failed' 的行，那条局部索引
			// payment_notifications_unprocessed_idx 就是给人工巡查用的。钱收了、单关了，
			// 回一个「收到」会让渠道不再推、而这条线索只剩我们自己的日志。
			return nil, fmt.Errorf("%w: payment %s is %s", ErrPaymentAlreadySettled, payment.PaymentNo, payment.Status)
		}
		// 失败回调打在已经关掉/成功过的单上：没有钱进来，也没状态要改。标 ignored
		// 提交——把它当事故会让人白查一次，而这只是一条迟到的、没有后果的通知。
		if err := markNotificationInTx(ctx, tx, p.NotificationID, model.NotificationIgnored, "payment is already "+payment.Status); err != nil {
			return nil, err
		}
		fundings, err := loadFundings(ctx, tx, payment.ID)
		if err != nil {
			return nil, err
		}
		return &PaymentSettlement{Payment: payment, Fundings: fundings, AlreadySettled: true}, nil
	}

	if p.Succeeded {
		if p.Amount != payment.Amount {
			return nil, fmt.Errorf("%w: notification says %d, payment is %d",
				ErrPaymentNotificationAmountMismatch, p.Amount, payment.Amount)
		}
		// 「两边都非空才比」这一条**不能收紧**，收紧会把一整族渠道的回调全部拒掉：建单
		// 时记不下渠道交易号的适配器确实存在（ums 的注释写着它的下单应答里有没有
		// targetOrderId「我们手上没有证据」，所以 CreateResult 留空，见
		// internal/provider/ums/ums.go 的「已知缺口」）。要求回调侧非空等于要求它凭空与我们
		// 的库对上一个我们从来没记过的值——那不是更严，是那一族渠道一笔都结不了。
		//
		// 它松掉的那一截由上面那条渠道一致性补上：两条边都空时，能被结算的单至少必须挂在
		// 收到回调的那条渠道上，而伪造者控制不了 payments.provider。
		// 真正的收口在适配器侧：拿到一份真实的 ums 下单应答、把交易号记下来，这条比较自动
		// 就在那一族上生效——**不动这里**。
		if p.ProviderTransactionID != "" && payment.ProviderTransactionID != "" &&
			p.ProviderTransactionID != payment.ProviderTransactionID {
			return nil, fmt.Errorf("%w: notification says %q, payment is %q",
				ErrProviderTransactionMismatch, p.ProviderTransactionID, payment.ProviderTransactionID)
		}
	}

	from := payment.Status
	paidAt := p.PaidAt
	if paidAt.IsZero() {
		// 渠道没给成交时间。用 NOW() 而不是零值：零值会写成 1970 年，一条 1970 年的
		// 资金流水会让对账报表永远对不平。
		paidAt = time.Now()
	}
	if p.Succeeded {
		payment, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments
			SET status='succeeded',
			    provider_transaction_id=COALESCE(NULLIF($2,''), provider_transaction_id),
			    paid_at=$3, failure_code='', failure_message='', updated_at=NOW()
			WHERE id=$1
			RETURNING `+paymentColumns, payment.ID, p.ProviderTransactionID, paidAt))
	} else {
		payment, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments
			SET status='failed', failure_code=$2, failure_message=$3, updated_at=NOW()
			WHERE id=$1
			RETURNING `+paymentColumns, payment.ID, p.FailureCode, p.FailureMessage))
	}
	if err != nil {
		return nil, err
	}

	var fundings []*model.PaymentFunding
	if p.Succeeded {
		fundings, err = succeedFundings(ctx, tx, payment, p.ProviderTransactionID, paidAt)
	} else {
		fundings, err = failFundings(ctx, tx, payment.ID, p.FailureCode)
	}
	if err != nil {
		return nil, err
	}

	if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, from, payment.Status,
		notificationReason(p), p.TraceID, model.ActorSystem, nil,
		map[string]any{"notificationId": p.NotificationID, "amount": p.Amount}); err != nil {
		return nil, err
	}

	if p.Succeeded {
		// 支付成功，分账也就是成功了：子单是随下单报文一起下去的（见 succeedSettlementInTx
		// 的说明），渠道回「支付成功」就是终局，没有第二个要等的回执。
		if err := succeedSettlementInTx(ctx, tx, payment.ID, p.ProviderTransactionID, paidAt); err != nil {
			return nil, err
		}
	} else {
		// 渠道回了一条失败：支付单进终态，还停在 pending 的分账任务跟着作废。理由与
		// MarkPaymentFailed 那一处相同（同事务、否则会留下一条被扫待发起的任务）。
		if err := cancelPendingSettlementInTx(ctx, tx, payment.ID); err != nil {
			return nil, err
		}
	}

	eventType, payload := paymentEvent(payment, fundings)
	if err := appendOutbox(ctx, tx, eventType, dto.EventVersion, p.TraceID, payload); err != nil {
		return nil, err
	}

	if err := markNotificationInTx(ctx, tx, p.NotificationID, model.NotificationProcessed, ""); err != nil {
		return nil, err
	}
	return &PaymentSettlement{Payment: payment, Fundings: fundings}, nil
}

// succeedFundings 把这一单的出资行置为成功，并为每一行落一条资金流水。
//
// 资金流水是**不可变的对账基准**（方案 5.9）：这里只追加，不改不删。冲正将来也走追加
// 反向记录，不回头改这一行。
func succeedFundings(ctx context.Context, tx pgx.Tx, payment *model.Payment, providerTransactionID string, paidAt time.Time) ([]*model.PaymentFunding, error) {
	if _, err := tx.Exec(ctx, `UPDATE payment_fundings
		SET status='succeeded', succeeded_at=$2,
		    provider_transaction_id=COALESCE(NULLIF($3,''), provider_transaction_id),
		    updated_at=NOW()
		WHERE payment_id=$1 AND status='reserved'`,
		payment.ID, paidAt, providerTransactionID); err != nil {
		return nil, err
	}
	fundings, err := loadFundings(ctx, tx, payment.ID)
	if err != nil {
		return nil, err
	}
	if len(fundings) == 0 {
		// 没有出资行是异常（建单那一步必写一行）。真出现时也不让对账基准留白：按支付单
		// 的总额补一条 funding_line_no=0 的流水，并让 user_message 能查出来。line_type
		// 取支付单自己的支付方式——既然没有出资行可读，这条方式就是这笔钱唯一的出处。
		fundings = []*model.PaymentFunding{{LineNo: 0, LineType: payment.PaymentMethod, Amount: payment.Amount, Status: model.FundingSucceeded}}
	}
	for _, funding := range fundings {
		if _, err := tx.Exec(ctx, `INSERT INTO payment_transactions
			(kind, payment_no, refund_no, funding_line_no, line_type, direction, amount,
			 provider, provider_transaction_id, occurred_at)
			VALUES ('payment',$1,'',$2,$3,'in',$4,$5,$6,$7)`,
			payment.PaymentNo, funding.LineNo, funding.LineType, funding.Amount,
			payment.Provider, providerTransactionID, paidAt); err != nil {
			return nil, err
		}
	}
	return fundings, nil
}

func failFundings(ctx context.Context, tx pgx.Tx, paymentID, failureCode string) ([]*model.PaymentFunding, error) {
	if _, err := tx.Exec(ctx, `UPDATE payment_fundings
		SET status='failed', failure_code=$2, updated_at=NOW()
		WHERE payment_id=$1 AND status='reserved'`, paymentID, failureCode); err != nil {
		return nil, err
	}
	return loadFundings(ctx, tx, paymentID)
}

func loadFundings(ctx context.Context, tx pgx.Tx, paymentID string) ([]*model.PaymentFunding, error) {
	rows, err := tx.Query(ctx, `SELECT `+fundingColumns+`
		FROM payment_fundings WHERE payment_id=$1 ORDER BY line_no`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fundings []*model.PaymentFunding
	for rows.Next() {
		funding, err := scanFunding(rows)
		if err != nil {
			return nil, err
		}
		fundings = append(fundings, funding)
	}
	return fundings, rows.Err()
}

// markNotificationInTx 在业务事务里更新一条回调记录的状态。
//
// 与下面的 MarkNotification 是**两个不同的东西**，不是重复：这个的更新必须与支付状态
// 一起提交——「钱的状态变了但回调还挂着 received」是不可接受的；那个相反，必须在业务事务
// 之外生效（那些路径上业务事务根本没开）。
func markNotificationInTx(ctx context.Context, tx pgx.Tx, notificationID, status, reason string) error {
	if notificationID == "" {
		// 没有记录 id 说明这条回调根本没落库（验签就挂了，或者调用方没传）。不是错误：
		// 那种情况下本来就没有记录要更新。
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE payment_notifications
		SET status=$2, failure_reason=$3, processed_at=NOW()
		WHERE id=$1`, notificationID, status, reason)
	return err
}

// MarkNotification 用**独立的连接**把一条回调标成某个结论。
//
// 用独立连接而不是业务事务，正是要的语义：调用它的路径上业务事务要么根本没开（验签失败时
// 我们连支付单都还没碰），要么刚刚回滚了（金额对不上）。「记下这次没认下来」与「别改支付
// 状态」必须是两个互不相干的动作——共用事务的话，一次拒绝的留痕会跟着回滚消失，而那正是
// 我们要留下的东西。
//
// status 由调用方给（failed = 拒绝了、ignored = 与收款无关），不做成两个函数：两者的
// 实现逐字相同，差别只在语义，而语义已经在调用点写着了。
func (r *PostgresRepository) MarkNotification(ctx context.Context, notificationID, status, reason string) error {
	if notificationID == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx, `UPDATE payment_notifications
		SET status=$2, failure_reason=$3, processed_at=NOW()
		WHERE id=$1`, notificationID, status, reason)
	return err
}

// canSettle 判断一个支付单能不能被推进到 target。
//
// 这是**锁内那次判定**——并发下只有它算数：两个回调同时到，谁先拿到 FOR UPDATE 谁改，
// 后到的看到的是终态、走幂等分支。
//
// 完整的支付单状态机在 service/state.go，那张表的作用是让入口（gRPC / 回调）在动手
// 之前就能给出一句说得清的拒绝。两者不重复：那边是给人看的完整形状，这边是写路径上
// 唯一算数的窄判定。与 order-service 里「状态机表 + 仓储锁内判定」的分工一致。
//
// created 也能直接到 succeeded/failed：真实的渠道可能在我们的第二段事务（建单 → pending）
// 提交之前就把回调打回来。不是理论情况，是网络快过数据库提交的常规情况。
func canSettle(from, target string) bool {
	if from != model.PaymentCreated && from != model.PaymentPending {
		return false
	}
	return target == model.PaymentSucceeded || target == model.PaymentFailed
}

// paymentEvent 把一张支付单与它的出资行翻成 outbox 里那份事件。
//
// 事件体在 dto 里，字段与 json tag 与 order-service 的镜像逐字一致；这里只负责填值。
// **Amount 一律用支付单的 amount**，失败事件也不例外：order-service 的 settleFailed
// 拿它记「这次尝试想扣多少」，记 0 会逼它去兜底（见 dto.PaymentEventPayload 的注释）。
func paymentEvent(payment *model.Payment, fundings []*model.PaymentFunding) (string, dto.PaymentEventPayload) {
	payload := dto.PaymentEventPayload{
		OrderNo:   payment.OrderNo,
		PaymentNo: payment.PaymentNo,
		Amount:    payment.Amount,
		// 一个值：用户选的那一种支付方式（catalog 的 code，如 `ums_h5_alipay`）。从前这里
		// 还发一个 fundingType（出资渠道词表），下游只能靠它落 order_payment_lines.line_type，
		// 于是支付宝只能落 `other`。那套词表已经退场。
		PaymentMethod:         payment.PaymentMethod,
		ProviderTransactionID: payment.ProviderTransactionID,
		FailureCode:           payment.FailureCode,
		FailureMessage:        payment.FailureMessage,
	}
	if payment.PaidAt != nil {
		payload.PaidAtUnix = payment.PaidAt.Unix()
	}
	for _, funding := range fundings {
		payload.Fundings = append(payload.Fundings, dto.PaymentFunding{
			LineType: funding.LineType,
			Amount:   funding.Amount,
			// 每笔出资都带支付单号：order-service 的 order_payment_lines.payment_no
			// 逐行记它，退款时按这行反查是哪张支付单出的钱。
			PaymentNo:      payment.PaymentNo,
			AccountEntryID: funding.AccountEntryID,
		})
	}
	if payment.Status == model.PaymentSucceeded {
		return dto.EventPaymentSucceeded, payload
	}
	return dto.EventPaymentFailed, payload
}

// notificationReason 是写进状态流水的短句。它要能单独被人读懂——排查时先看的是它。
func notificationReason(p SettleNotificationParams) string {
	if p.Succeeded {
		return "provider reported payment succeeded"
	}
	if p.FailureMessage != "" {
		return "provider reported payment failed: " + p.FailureMessage
	}
	return "provider reported payment failed"
}

// FindOverdueAccountFundedPayments 找出「豆已经扣了、到点还没结算」的支付单。
//
// 普通业务路径不会走到这里：createAccountPayment 扣完豆立刻结算，中间只隔一次写库。
// 能留下这种行的只有那两次写库之间出的岔子——进程被 kill、库抖动、或者这张单在结算前
// 正好被超时关单收走（见 payments.account_entry_id 的列注释）。这张单上的豆已经真的扣走了，所以它必须被结算，
// 而不是被人发现。
//
// 扫描走 payments_overdue_account_funding_idx，与关单扫描同一个形状；**不带 FOR UPDATE**：
// 拿一批 id 出来就够了，真正改状态的是 SettleAccountPayment 自己的那个事务，在那里锁行
// 才是对的（在这里先锁住，等于让一个跨服务的流程一直握着行锁）。
func (r *PostgresRepository) FindOverdueAccountFundedPayments(ctx context.Context, limit int) ([]model.Payment, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments
		WHERE status IN ('created','pending') AND expires_at IS NOT NULL AND expires_at <= NOW()
			AND account_entry_id IS NOT NULL
		ORDER BY expires_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var payments []model.Payment
	for rows.Next() {
		payment, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		payments = append(payments, *payment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return payments, nil
}

// ListStalePendingPayments 找出「已经向渠道发起过、还停在 pending、而且发起之后一直没动过」
// 的支付单。
//
// 它是主动查单那条补偿任务的读（见 service.ReconcilePendingPayments）。一笔渠道支付的结果
// 本来只可能从两条路回来，而这两条路都答不了「钱到底收到没有」：
//
//	回调      会丢、会被网络挡在外面、会因为这一侧的验签没配好而被我们主动拒收
//	          （银联商务那条就要第二把密钥，见 catalog 里 commKey 的说明）
//	超时关单  只说「我们不再等了」，从不说钱没收到
//
// 这条查询开出第三条路：去问渠道。老系统就是这么做的——它每 60 秒扫一批卡在
// confirmed/paying 超 5 分钟的咖啡订单，向银联查单（panda_serve:miniapp/routes.go:354 与
// order_service.go:1988），而**回调那条路它压根不验签**。结论是：那边能收三年钱靠的是查单，
// 不是回调。这里把这一层补回来。
//
// 判据用 updated_at 而不是 created_at：建单与 MarkPaymentPending 是同一次发起的两个阶段，
// pending 那一刻才意味着「渠道那边已经有这张单了」，而 updated_at 正是那一刻。停在 pending
// 之后，这条路上没有任何写入会再动这一行（结算与关单都会把它带离 pending），所以
// `updated_at <= NOW() - 滞后` 就是「问了渠道这么久还是没结论」。
//
// 只挑 provider 非空的行：账户出资（咖啡豆）没有渠道可问，那一条的补偿是
// FindOverdueAccountFundedPayments，与这里不是同一件事。
//
// **不带 FOR UPDATE**，理由同 FindOverdueAccountFundedPayments：这里只是取一批出来问渠道，
// 真正改状态的是 SettlePayment 自己的事务，在那里锁行才算数。查单本身要出网、可能慢上几秒，
// 在这个查询里先锁住等于让一次网络调用一直握着行锁。
//
// 扫描走 payments_pending_reconcile_idx (updated_at) WHERE status = 'pending'。
func (r *PostgresRepository) ListStalePendingPayments(ctx context.Context, staleBefore time.Time, limit int) ([]model.Payment, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments
		WHERE status = 'pending' AND provider <> '' AND updated_at <= $1
		ORDER BY updated_at
		LIMIT $2`, staleBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var payments []model.Payment
	for rows.Next() {
		payment, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		payments = append(payments, *payment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return payments, nil
}

// ExpireOverduePayments 关掉到点未支付的支付单，返回关掉的条数。
//
// 扫描走 payments_pending_expiry_idx (status, expires_at)，FOR UPDATE SKIP LOCKED
// 让多个副本同时扫不会互相等锁——这台机器上只有一个副本，但补偿任务不该假设这一点。
//
// **不发事件**：支付单过期不是一次支付结果，订单有自己的超时关单。对 order-service 来说
// 「这笔支付没成」与「这笔支付从没发生过」是一样的，它不需要知道。
//
// **豆已经扣过的不关**（account_entry_id IS NOT NULL，见 payments.account_entry_id 的列注释）。关单会把这张单的
// 出资行标成 released——那是对「钱从来没动过」的描述，而这里的钱已经动了；把它关掉，那笔
// 扣减就再没有任何一条路径能把它推到成功，只能等人拿着账变去补。它们由补偿任务结算
// （FindOverdueAccountFundedPayments → service.SettleOverdueAccountPayments），那条路在
// 这一次扫描之前跑。
func (r *PostgresRepository) ExpireOverduePayments(ctx context.Context, limit int) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id::text FROM payments
		WHERE status IN ('created','pending') AND expires_at IS NOT NULL AND expires_at <= NOW()
			AND account_entry_id IS NULL
		ORDER BY expires_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	closed := 0
	for _, id := range ids {
		payment, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments
			SET status='expired', closed_at=NOW(), updated_at=NOW()
			WHERE id=$1
			RETURNING `+paymentColumns, id))
		if err != nil {
			return closed, err
		}
		// 出资行跟着释放：预占解开、不再扣。用 released 而不是 failed——钱从来没动过，
		// 与「渠道拒绝了这笔」不是一回事。
		if _, err := tx.Exec(ctx, `UPDATE payment_fundings
			SET status='released', updated_at=NOW()
			WHERE payment_id=$1 AND status='reserved'`, id); err != nil {
			return closed, err
		}
		// 状态流水里的 from 用这次更新之前的状态，但我们没法在一条 UPDATE 里读到它——
		// 所以记 pending 之外的那个可能：扫描的条件就是这两者，而 created 是更早的一步。
		// 为准确起见再读一次是不可能的（行已经改了），因此这里记 from 为空串并让
		// metadata 说明原状态。宁可少一个字面值，也不要在流水里写一个可能是错的 from。
		if err := recordTransition(ctx, tx, model.AggregatePayment, payment.ID, "",
			model.PaymentExpired, "payment expired before the provider reported a result", "",
			model.ActorSystem, nil, nil); err != nil {
			return closed, err
		}
		// 关掉的这一单，它的分账任务也一起作废。**这一步不能等到扫描之后补**：任务在发起支付时
		// 就建好了，而扫待发起的任务只按 status 捞——留在 pending 的那条会被当成「该发了」，
		// 把一笔已经关掉的单的钱发去分账。
		if err := cancelPendingSettlementInTx(ctx, tx, payment.ID); err != nil {
			return closed, err
		}
		closed++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return closed, nil
}
