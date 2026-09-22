package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// ScopeCreateAgreement 是发起签约那条幂等记录的 scope。
//
// 它与 ScopeCreatePayment 分开而不是共用一个：两者的键空间来自不同的调用方（发起支付是
// order-service，签约是 membership-service），一个 uuid 撞上另一个 uuid 的概率可以忽略，
// 但**语义**撞上不行——同一个键在两边各查各的，混在一个 scope 里会让一次「用相同的
// request_id 发起支付」变成一次幂等冲突，而那不是任何人能读懂的报错。
const ScopeCreateAgreement = "payment.agreement.create"

// ErrAgreementNotFound：协议不存在。与 ErrPaymentNotFound 同形，供 rpc 翻成 NotFound。
var ErrAgreementNotFound = errors.New("payment agreement not found")

// CreateAgreementParams 是一次发起签约的落库口径。
//
// 它把三件事放在**一个事务**里：幂等记录、协议行、渠道调用流水。为什么流水也进来（别处是
// 刻意放在事务外的，见 insertProviderCallInTx 的注释）：纯签约没有出网调用，服务端只算了
// 一个签名，所以这一次「调用」与协议行必须同生共死。
type CreateAgreementParams struct {
	// AgreementNo 由 service 生成，**同时就是交给渠道的 contract_code**。
	AgreementNo string
	UserID      string
	// Provider 是渠道名（如 `wechat_pay`），Method 是支付方式的 code（如 `wechat_papay`）。
	Provider string
	Method   string
	Subject  string
	// PlanCode 是业务侧的套餐标识；Metadata 里装渠道侧那几个值（见 model 的
	// AgreementMetaProviderPlanID）。
	PlanCode        string
	MaxChargeAmount int64
	Metadata        map[string]any
	// RequestID 与 RequestHash 是幂等键与「同一个键换了请求体」的判据。
	RequestID   string
	RequestHash string
	// —— 渠道调用流水（operation=agreement_sign）——
	// 它是**脱敏后**的摘要：不放签名原文与密钥（见 provider.AgreementSignResult）。
	TraceID             string
	CallSummary         map[string]any
	CallResponseSummary map[string]any
	// Response 是这次发起成功时要回放给同一个 request_id 的快照（service 的响应结构体）。
	Response any
}

// CreateAgreement 在一个事务里落下一次签约：幂等记录 + 协议行 + 状态流水 + 渠道调用流水。
//
// 返回值第三个是**幂等命中**——命中时第一个返回值为 nil、第二个是上一次那份响应快照。
// 调用方必须把快照原样回给客户端：重新算一遍签名会让用户在微信那边拿到两个不同的
// request_serial（在渠道看来那是两次签约）。
//
// # recycled 那一路为什么什么都不用收拾
//
// CreatePayment 那边回收陈旧幂等记录时要先 retire 上一次的支付单（半截尝试会把支付单留在
// created，且 payments_request_id_key 挡着新的插入）。签约这边不需要，因为**协议行与幂等行
// 在同一个事务里**：进程在半路死掉时两者都没提交，回收之后是一次干干净净的重来。这也是
// 这里不做「先抢键、后建行」那种两段写法的原因。
func (r *PostgresRepository) CreateAgreement(ctx context.Context, p CreateAgreementParams) (*model.PaymentAgreement, []byte, bool, error) {
	metadata, err := json.Marshal(p.Metadata)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode agreement metadata: %w", err)
	}
	snapshot, err := json.Marshal(p.Response)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode agreement response: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	replay, hit, _, err := beginIdempotentOperation(ctx, tx, ScopeCreateAgreement,
		p.RequestID, p.RequestHash, "agreement", "")
	if err != nil {
		return nil, nil, false, err
	}
	if hit {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, err
		}
		return nil, replay, true, nil
	}

	agreement, err := scanAgreement(tx.QueryRow(ctx, `INSERT INTO payment_agreements
		(agreement_no, user_id, provider, payment_method, contract_no, subject, plan_code,
		 max_charge_amount, status, metadata)
		VALUES ($1,$2,$3,$4,'',$5,$6,$7,'pending',$8)
		RETURNING `+agreementColumns,
		p.AgreementNo, p.UserID, p.Provider, p.Method, p.Subject, p.PlanCode,
		p.MaxChargeAmount, metadata))
	if err != nil {
		return nil, nil, false, mapAgreementError(err)
	}

	// from 传空串：这一行是「从没有到有」，与建支付单那一步同形，让聚合的完整历史从第一行
	// 起就能读出来。
	if err := recordTransition(ctx, tx, model.AggregateAgreement, agreement.ID, "",
		model.AgreementStatusPending, "agreement created", p.RequestID, model.ActorSystem, nil,
		map[string]any{"provider": p.Provider, "paymentMethod": p.Method, "planCode": p.PlanCode}); err != nil {
		return nil, nil, false, err
	}

	if err := insertProviderCallInTx(ctx, tx, ProviderCallParams{
		Provider:        p.Provider,
		Operation:       model.CallOperationAgreementSign,
		AgreementNo:     p.AgreementNo,
		RequestID:       p.RequestID,
		TraceID:         p.TraceID,
		AttemptNo:       1,
		RequestSummary:  p.CallSummary,
		ResponseSummary: p.CallResponseSummary,
		// 纯签约没有出网调用，所以没有 HTTP 状态、没有重试次数、耗时近乎为零。记 0 次尝试
		// 会让「发出去过一次」显示成「一次都没发」，所以是 1（见 provider.AgreementSignResult
		// 里为什么不给这两个字段留坑）。
		Result: model.CallSuccess,
	}); err != nil {
		return nil, nil, false, err
	}

	if err := completeIdempotentOperation(ctx, tx, ScopeCreateAgreement, p.RequestID,
		agreement.ID, json.RawMessage(snapshot)); err != nil {
		return nil, nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return agreement, nil, false, nil
}

// FindAgreementByNo 按我们自己的协议号读一份协议。
//
// 找不到时返回 ErrAgreementNotFound：签约通知、后台同步、确认三条路都靠它，而它们对
// 「查无此约」的处置是一样的——要么是我们收错了报文，要么是调用方给了别人的号，两种都
// 不该被当成「没有这件事」悄悄过去。
func (r *PostgresRepository) FindAgreementByNo(ctx context.Context, agreementNo string) (*model.PaymentAgreement, error) {
	return scanAgreement(r.pool.QueryRow(ctx,
		`SELECT `+agreementColumns+` FROM payment_agreements WHERE agreement_no = $1`, agreementNo))
}

// SettleAgreementParams 是「把渠道查回来的结论落到协议上」的输入。
type SettleAgreementParams struct {
	AgreementNo string
	// Target 是这次核出来的目标状态，取 model.AgreementStatusActive / terminated / pending。
	//
	// pending 表示渠道说「还在进行中」——那时的正确动作是**什么都不做**，不是把本地往回推
	// （见下面 settleAgreement 的判据）。
	Target string
	// ContractNo 是渠道侧的协议号。空串表示渠道没给，不覆盖本地已有的值。
	ContractNo string
	Reason     string
	RequestID  string
	// NotificationID 是这次变更**由哪条通知触发**的（签约通知那条路才填，见
	// SettleAgreementNotification）。它进状态流水的 metadata，是「这条流水对应哪份报文」的
	// 唯一线索——payment_notifications 上没有一列指向协议，所以这条线索只能记在协议这一侧。
	//
	// 空串（查约那条路）不进 metadata：写一个 "notificationId": "" 会让「没有通知」与
	// 「通知号是空串」长得一样。
	NotificationID string
}

// transitionMetadata 拼一条状态流水的 metadata：调用方自己那几个键，加上「这次是谁触发的」。
//
// 抽出来的理由只有一个——NotificationID 那条「空串不进 map」的规矩只该写一遍。两条分支各写
// 一次的话，早晚有一条会在没有通知的路径上写出一个 "notificationId": ""。
func (p SettleAgreementParams) transitionMetadata(extra map[string]any) map[string]any {
	metadata := make(map[string]any, len(extra)+1)
	maps.Copy(metadata, extra)
	if p.NotificationID != "" {
		metadata["notificationId"] = p.NotificationID
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// SettleAgreement 把一次查约的结论落到协议上，返回是否真的改了。
//
// 锁内判定（FOR UPDATE），并发下只有先到的那一次算数。四条判据，每一条都是「宁可不动也不猜」：
//
//  1. 本地已经是 terminated / expired 时**一律不动**。解约是终态：用户已经不授权了，
//     把一份本地已解约的协议按渠道的一句话改回 active，等于在一份他以为关掉的授权上扣钱。
//     真出现「渠道说 active、本地记 terminated」（比如我们解约失败但记录了成功），那是一个
//     要人来看的矛盾，不是一句自动纠正能消掉的——所以它留在两边对不上的样子上。
//  2. 目标与现状相同、且渠道协议号已经记下了：不动，返回 false。后台那个「同步」按钮靠它
//     区分「已同步」与「无需更正」。
//  3. 渠道说 terminated：本地收掉（active/pending → terminated）。
//  4. 渠道说 active：本地补上 channel 侧协议号并置 active。**contract_no 只在本地为空时写**
//     ——渠道换了一个协议号意味着这不是同一份协议，那种情况该由人来查，不该由一次同步覆盖掉。
func (r *PostgresRepository) SettleAgreement(ctx context.Context, p SettleAgreementParams) (*model.PaymentAgreement, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	agreement, err := scanAgreement(tx.QueryRow(ctx,
		`SELECT `+agreementColumns+` FROM payment_agreements WHERE agreement_no = $1 FOR UPDATE`,
		p.AgreementNo))
	if err != nil {
		return nil, false, mapAgreementError(err)
	}

	updated, changed, err := applyAgreementTarget(ctx, tx, agreement, p)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return updated, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// AgreementNotificationParams 是把一条协议变更通知落进业务事务的输入。
type AgreementNotificationParams struct {
	// NotificationID 是 payment_notifications.id，在同一个事务里被标成 processed。
	// 协议状态与通知记录必须一起变，否则会出现「协议改了但通知还挂着 received」。
	NotificationID string
	// Provider 是**收到这条通知的那条渠道**（通知 URL 里那段），不是报文里说的任何东西。
	Provider string
	// AgreementNo 是报文里的 contract_code，也就是我们自己的协议号。
	//
	// **可能是空的**：微信的解约通知不带 contract_code（见 wechatpay.ParseAgreementNotify）。
	// 空了就按 ContractNo 找。
	AgreementNo string
	// ContractNo 是渠道侧的协议号，两类通知都带它。
	ContractNo string
	// Target 是这条通知说的目标状态，取 model.AgreementStatusActive / terminated。
	Target string
	// Reason 进状态流水，也是解约时 terminate_reason 的内容。
	Reason  string
	TraceID string
}

// AgreementSettlement 是一条协议通知落地之后的结果。
type AgreementSettlement struct {
	Agreement *model.PaymentAgreement
	// Changed 表示这次真的改了协议状态。false 时**不发事件**——事件是「状态变了」的广播，
	// 状态没变就没有可广播的东西（与 PaymentSettlement.AlreadySettled 同一条规矩）。
	//
	// 它同时是**重复通知的第二道防线**：第一道是 UNIQUE (provider, notification_id)，但那条
	// 只挡得住一模一样的报文；渠道若对同一次变更推了两份内容略有差别的报文（多一个字段、
	// 时间戳不同），两条都会落库，而在这里被同一条判据挡住第二次。
	Changed bool
}

// SettleAgreementNotification 在**一个事务里**把一条协议变更通知落到协议、状态流水与 outbox 上。
//
// 顺序与判据（每一条都是「宁可拒绝也不猜」）：
//
//  1. 锁住协议行（FOR UPDATE）。并发下只有先到的那一次算数。
//  2. 渠道对不上 → 报错不 ack（见 notificationChannelMatches）。签名证明得了「这条报文是这条
//     渠道签的」，证明不了「这份协议是这条渠道的协议」。
//  3. 走 applyAgreementTarget —— 与查约那条路**同一套判定**：终态不复活、已生效不重写。
//  4. 改了才写 outbox，无论改没改都把通知标 processed。
//
// # 为什么按 contract_no 找那一支是必要的
//
// 微信的解约通知里没有 contract_code，只有 contract_id。没有这一支，用户的每一次解约都会
// 落成一句「查无此约」——而那是本服务里最不该出现的一种误判：本地订阅会一直是 active，
// 直到下一次扣款被渠道拒掉才有人发现。
func (r *PostgresRepository) SettleAgreementNotification(ctx context.Context, p AgreementNotificationParams) (*AgreementSettlement, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	agreement, err := lockAgreementForNotification(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := notificationChannelMatches(agreement.Provider, p.Provider); err != nil {
		return nil, err
	}

	updated, changed, err := applyAgreementTarget(ctx, tx, agreement, SettleAgreementParams{
		AgreementNo: agreement.AgreementNo,
		Target:      p.Target,
		ContractNo:  p.ContractNo,
		Reason:      p.Reason,
		// RequestID 留空：这条路上没有调用方给的 request_id，而把通知的 uuid 写进那一列会是
		// 一个谎（它不是任何一个请求的 id）。通知与这条流水的关联走 metadata 的 notificationId。
		NotificationID: p.NotificationID,
	})
	if err != nil {
		return nil, err
	}

	if changed {
		if err := appendOutbox(ctx, tx, agreementEventType(updated.Status), dto.EventVersion, p.TraceID,
			agreementEvent(updated)); err != nil {
			return nil, err
		}
	}
	if err := markNotificationInTx(ctx, tx, p.NotificationID, model.NotificationProcessed, ""); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &AgreementSettlement{Agreement: updated, Changed: changed}, nil
}

// lockAgreementForNotification 找出这条通知说的是哪一份协议，并锁住它。
//
// 两个入口，按报文里有什么走哪一个：
//
//	有 contract_code（签约通知，以及一切我们自己发的报文）→ 按 agreement_no 找
//	只有 contract_id（微信的解约通知）                    → 按 contract_no 找
//
// 按 agreement_no 那一支不筛渠道：agreement_no 是我们自己生成的、全局唯一（建表时就带的
// UNIQUE），筛选只会多一个能对不上的地方。按 contract_no 那一支**必须**筛渠道，因为渠道侧的
// 协议号只在渠道内唯一——不同渠道各自发号，撞号不是不可能的。
func lockAgreementForNotification(ctx context.Context, tx pgx.Tx, p AgreementNotificationParams) (*model.PaymentAgreement, error) {
	if p.AgreementNo != "" {
		agreement, err := scanAgreement(tx.QueryRow(ctx,
			`SELECT `+agreementColumns+` FROM payment_agreements WHERE agreement_no = $1 FOR UPDATE`,
			p.AgreementNo))
		if err != nil {
			return nil, mapAgreementError(err)
		}
		return agreement, nil
	}
	if p.ContractNo == "" {
		// 两个都没有：报文里既没有我们的协议号也没有渠道协议号。到这个函数时验签已经过了，
		// 所以这不是伪造，而是一条我们认不出对象的通知——只能拒，让人去看那条报文。
		return nil, fmt.Errorf("%w: the notification carries neither an agreement_no nor a contract_no", ErrAgreementNotFound)
	}
	agreement, err := scanAgreement(tx.QueryRow(ctx,
		`SELECT `+agreementColumns+` FROM payment_agreements
		WHERE contract_no = $1 AND provider = $2 FOR UPDATE`, p.ContractNo, p.Provider))
	if err != nil {
		return nil, mapAgreementError(err)
	}
	return agreement, nil
}

// agreementEventType 把变更之后的状态翻成事件类型。
//
// 只有 active 与 terminated 两种能到这里（applyAgreementTarget 的判据里 pending 不改状态、
// 词表外的目标是编码错误）。落进 else 的那一支意味着有人加了第七种状态而没想清楚它对下游
// 意味着什么——发 terminated 是**保守**的那一侧：解约事件让订阅停下来，比发一个下游读不懂的
// 事件（或者干脆不发、让订阅继续按一份作废的协议续费）都安全。
func agreementEventType(status string) string {
	if status == model.AgreementStatusActive {
		return dto.EventAgreementSigned
	}
	return dto.EventAgreementTerminated
}

// agreementEvent 把一张协议翻成那份事件体。字段的取舍见 dto.AgreementEventPayload 的注释。
func agreementEvent(agreement *model.PaymentAgreement) dto.AgreementEventPayload {
	return dto.AgreementEventPayload{
		AgreementID: agreement.ID,
		AgreementNo: agreement.AgreementNo,
		ContractNo:  agreement.ContractNo,
		UserID:      agreement.UserID,
		Status:      agreement.Status,
	}
}

// applyAgreementTarget 是 settleAgreement 那条判据的落点，单独一个函数是为了让
// **签约通知**那条路（#134）能复用同一套判定：通知说的是「渠道刚告诉我什么」，查约说的是
// 「渠道现在是什么」，两者要落到同一个状态机上——两份实现就是「同一个渠道状态在两条路上
// 得到不同结论」的来源。
func applyAgreementTarget(ctx context.Context, tx pgx.Tx, agreement *model.PaymentAgreement, p SettleAgreementParams) (*model.PaymentAgreement, bool, error) {
	isTerminal := agreement.Status == model.AgreementStatusTerminated || agreement.Status == model.PaymentExpired
	// 判据 1：解约是终态，不动。
	if isTerminal {
		return agreement, false, nil
	}

	switch p.Target {
	case model.AgreementStatusPending:
		// 渠道说还在进行中。本地本来就是 pending，什么都不用改。
		return agreement, false, nil

	case model.AgreementStatusActive:
		// 判据 2：状态已经是 active 且渠道协议号已经记下——没有可纠正的东西。
		if agreement.Status == model.AgreementStatusActive && agreement.ContractNo != "" {
			return agreement, false, nil
		}
		updated, err := scanAgreement(tx.QueryRow(ctx, `UPDATE payment_agreements
			SET status='active',
			    contract_no=COALESCE(NULLIF($2,''), contract_no),
			    signed_at=COALESCE(signed_at, NOW()),
			    activated_at=COALESCE(activated_at, NOW()),
			    updated_at=NOW()
			WHERE id=$1
			RETURNING `+agreementColumns, agreement.ID, p.ContractNo))
		if err != nil {
			return nil, false, err
		}
		if err := recordTransition(ctx, tx, model.AggregateAgreement, agreement.ID,
			agreement.Status, model.AgreementStatusActive, p.Reason, p.RequestID,
			model.ActorSystem, nil, p.transitionMetadata(map[string]any{"contractNo": p.ContractNo})); err != nil {
			return nil, false, err
		}
		return updated, true, nil

	case model.AgreementStatusTerminated:
		updated, err := scanAgreement(tx.QueryRow(ctx, `UPDATE payment_agreements
			SET status='terminated', terminated_at=NOW(), terminate_reason=$2, updated_at=NOW()
			WHERE id=$1
			RETURNING `+agreementColumns, agreement.ID, p.Reason))
		if err != nil {
			return nil, false, err
		}
		if err := recordTransition(ctx, tx, model.AggregateAgreement, agreement.ID,
			agreement.Status, model.AgreementStatusTerminated, p.Reason, p.RequestID,
			model.ActorSystem, nil, p.transitionMetadata(nil)); err != nil {
			return nil, false, err
		}
		return updated, true, nil

	default:
		// 词表外的目标状态是**我们自己的编码错误**（渠道状态到本地状态的映射在 service 里，
		// 那里只可能产出上面三个值），所以它报错而不是当成「不用改」——后者的后果是一份本该
		// 被收掉的协议安静地留着。
		return nil, false, fmt.Errorf("unknown agreement target status %q", p.Target)
	}
}

// mapAgreementError 把唯一约束冲突与「查无此约」翻成调用方分得清的业务错误。
//
// 按约束名判而不是按错误文本：与 mapPGError 同一条理由（PG 换措辞或换语言时静默失效）。
func mapAgreementError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgreementNotFound
	}
	return err
}

// agreementColumns 是 payment_agreements 的列清单，顺序与 scanAgreement 严格一一对应
// （UUID 列照旧 ::text）。
const agreementColumns = `id::text, agreement_no, legacy_id, user_id, provider, payment_method,
	contract_no, subject, plan_code, max_charge_amount, next_charge_at, status, metadata,
	signed_at, activated_at, terminated_at, terminate_reason, created_at, updated_at`

func scanAgreement(row scanner) (*model.PaymentAgreement, error) {
	item := &model.PaymentAgreement{}
	err := row.Scan(&item.ID, &item.AgreementNo, &item.LegacyID, &item.UserID, &item.Provider,
		&item.PaymentMethod, &item.ContractNo, &item.Subject, &item.PlanCode,
		&item.MaxChargeAmount, &item.NextChargeAt, &item.Status, &item.Metadata,
		&item.SignedAt, &item.ActivatedAt, &item.TerminatedAt, &item.TerminateReason,
		&item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}
