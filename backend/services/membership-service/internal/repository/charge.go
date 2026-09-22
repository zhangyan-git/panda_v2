package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// ChargeSettleParams 是一期扣款的结果落到订阅上所需的全部输入。
//
// 它是 `payment.agreement.charge_succeeded` / `charge_failed` 两条事件在本域的形状（见
// dto.AgreementChargeEventPayload），由 service 从事件体里读出来。
type ChargeSettleParams struct {
	// AgreementID 是命中这一行的**唯一**键：事件体里没有订阅 id，而库上
	// membership_subscriptions_agreement_unique 保证一份协议最多对一行。
	AgreementID string
	// UserID 是事件里说的签约人。它不参与定位，只用来核对（见 SettleCharge）。
	UserID string
	// Target 取 dto.ChargeStatusSucceeded 或 dto.ChargeStatusFailed——**是这一期的结论，不是
	// 协议的状态**：协议始终是 active，扣款成不成功是另一回事。
	Target string
	// BizPeriod 是这一期的期次（由订阅的 next_charge_at 派生，见 model.ChargePeriod）。它只进
	// 流水的 metadata：**不是幂等键**——幂等靠渠道流水号（成功那条）与事件本身的去重（失败那条）。
	BizPeriod string
	// Amount 是这一期扣了多少，单位为分。会员库里没有金额列（金额的权威在支付域），它只进流水
	// 的 metadata，答的是「这一期的续费流水对应多少钱」。
	Amount int64
	// ProviderTransactionID 是渠道侧的流水号。成功那条它非空，并且就是流水的幂等键。
	//
	// 失败那条它可能为空（渠道拒一笔时不给号），而**那里不能拿它当幂等键**：一次失败的号可能
	// 是空的，硬拿空串去撞唯一索引等于没有索引（004 那条索引的 WHERE 也把空串排除在外）。
	ProviderTransactionID string
	FailureCode           string
	FailureMessage        string
	// OrderID 是**上游为这一期建的续费单**（order-service 的 orders.id），由 service 在调本方法
	// **之前**调订单域拿到。
	//
	// 它落进两个地方，两处都不是可有可无的：
	//
	//	membership_changes.order_id   这一次续费的账面对应哪一张单（后台时间线上能看到）
	//	membership.renewed 事件的 orderId   **下游发券的判据**——coupon-service 拿它当
	//	                              「这是一次真实成交」的凭据，空值它当成「后台人工改了有效期」，
	//	                              直接不发券（见那条事件的说明与 coupon-service 的 consumer）
	//
	// 失败那条路上它是空串，而且**必须**是空的：那一期没扣到钱，没有订单可言。
	OrderID string
	// OccurredAt 是渠道给出这个结论的时刻（本服务拿不到，用事件到达的时刻）。它是**续期的起点**
	// 候选之一：这条订阅的会员如果已经过期，就从这一刻重新起算（见 renewMembership）。
	OccurredAt time.Time
	TraceID    string
}

// ChargeContext 是「这一期该记成一张什么样的订单」所需的全部事实：这条订阅，以及它挂在谁的
// 哪一份会员上。
//
// 它存在的理由是**顺序**：续费单必须在结算之前建（见 service 里那段说明）。而建单要的那份套餐
// 快照只在这两行上——订阅上的 price_cents / period / period_count 是签约时约定死的一期，会员行
// 上的 plan_code / plan_name 与会员价那三列是成交当时的副本。两者都不能现查套餐：那等于让一次
// 后台改套餐改写一个正在被扣款的用户这一期买到了什么。
type ChargeContext struct {
	Subscription *model.Subscription
	Membership   *model.Membership
}

// LoadChargeContext 在建单之前读一次。
//
// # 它是一对不加锁的读，这是有意的
//
// 这一对读只用来**拼一份快照**，不用来判定任何事：能不能扣、这一期算不算数，判断都在
// SettleCharge 的锁里做。所以这里不加 FOR UPDATE——加了会让人以为这个锁跨到了后面那次 gRPC
// 调用上（一次网络往返，锁会被持有几百毫秒），而它其实什么都保护不到。
//
// 两条都是主键/唯一索引上的等值查找：订阅按 agreement_id（库上那份部分唯一索引保证最多一条），
// 会员按它的 id。所以这是两次索引查找，不是两次扫描。
func (r *PostgresRepository) LoadChargeContext(ctx context.Context, agreementID string) (*ChargeContext, error) {
	subscription, err := scanSubscription(r.pool.QueryRow(ctx,
		`SELECT `+subscriptionColumns+` FROM membership_subscriptions WHERE agreement_id = $1`, agreementID))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, mapPGError(err)
	}
	// 会员不在时回 ErrMembershipNotFound：订阅挂在一个不存在的会员上是一行坏数据（membership_id
	// 上有外键，正常写不出来），要让这条消息停在死信里等人看，而不是当「没有会员」糊过去。
	membership, err := r.GetMembershipByID(ctx, subscription.MembershipID)
	if err != nil {
		return nil, err
	}
	return &ChargeContext{Subscription: subscription, Membership: membership}, nil
}

// ChargeSettleOutcome 是一次结算的结果。
type ChargeSettleOutcome struct {
	// Subscription 是结算之后的那一行（没改动时就是没动过的那一行）。调用方用它核对 user_id。
	Subscription *SubscriptionRow
	// Changed 为假表示**这次什么都没写**：重复投递撞上了幂等键，或者这条订阅已经结束了。
	//
	// 它与 SettleSubscription 的 changed 同一个用途：事件是「结果变了」的广播，而重复投递不是
	// 新结果。调用方据此安静地 ack。
	Changed bool
	// Suspended 表示这一条**把订阅停掉了**（连续失败到阈值）。它是这次唯一的「对外有后果」的
	// 动作，所以单独给一个字段让调用方能记一条 warn 级别的日志——一条订阅被停扣是运营要知道的
	// 事，而它今天不对外发事件（订阅状态不在 membership.* 那四条事件的语义里）。
	Suspended bool
}

// SettleCharge 把一期扣款的结论落到订阅与会员上，一个事务做完。
//
// # 两支的共同点
//
// 都从 `lockSubscription(agreementID)` 开始：事件体里只有协议 id，而**先锁再判**是这一层的
// 老规矩（与 SettleSubscription 逐字相同）——读与写之间插进来另一条事件时，两边会各自读到同一个
// 旧计数，连续失败次数就会少算一次。
//
// # 成功那一支
//
// 一个事务里：续会员（复用 renewMembership）+ 推进订阅 + 写 `renew` 流水 + 出
// `membership.renewed`。**扣到钱就一定要续到权益**：两件事分开做，中间失败的那一侧就是「用户的
// 钱收了、会员没加上」。
//
// **它不管订单**：那一期扣款对应的续费单是**上游**在调它之前就建好的（见 ChargeSettleParams.OrderID
// 与 service 里那段顺序说明）——本方法的入参里那个 order_id 已经是一个既成事实，它只负责写下来。
//
// 续期用的快照是**会员行上那一份，原样抄回去**（见 carriedSnapshot），不按 subscription.plan_id
// 现查套餐。理由与这一层反复申明的那条一致：快照说了算，现查等于让一次后台改套餐/改券配置改写
// 一个正在被扣款的用户，而他下个月扣的还是签约时约定死的价（PriceCents 是同一套道理）。
//
// # 失败那一支
//
// 只累加两个计数，**不动 next_charge_at**：这一期还没扣成，它没走完，还该被扫到。这正是重试的
// 机制——下一轮扫描拿同一个期次再发起一次，支付侧按 (协议, 期次) 幂等地退回那一行。
//
// 连到阈值就 `suspended`：`ListDueSubscriptions` 只捞 active，所以那之后这个人的扣款就停了。
// **停扣不是解约**（协议还在渠道上挂着），本服务不替用户撤回授权（见 model.MaxConsecutiveChargeFailures）。
func (r *PostgresRepository) SettleCharge(ctx context.Context, p ChargeSettleParams) (*ChargeSettleOutcome, error) {
	var outcome ChargeSettleOutcome
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, err := lockSubscription(ctx, tx, "", p.AgreementID)
		if err != nil {
			return err
		}
		occurredAt := utc(p.OccurredAt)

		switch p.Target {
		case dto.ChargeStatusSucceeded:
			changed, err := settleChargeSucceeded(ctx, tx, current, p, occurredAt)
			if err != nil {
				return err
			}
			outcome.Changed = changed
		case dto.ChargeStatusFailed:
			suspended, changed, err := settleChargeFailed(ctx, tx, current, p, occurredAt)
			if err != nil {
				return err
			}
			outcome.Suspended, outcome.Changed = suspended, changed
		default:
			// 目标状态由 service 从两条事件里给，走到这里说明我们自己写错了（比如把协议事件的
			// 状态词传了进来）——让它当场炸，而不是静默不改。
			return errUnsupportedChargeTarget(p.Target)
		}

		outcome.Subscription = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &outcome, nil
}

// settleChargeSucceeded 是「这一期扣到了钱」的落点。返回这次有没有真的写。
func settleChargeSucceeded(ctx context.Context, tx pgx.Tx, current *SubscriptionRow, p ChargeSettleParams, occurredAt time.Time) (bool, error) {
	// 一条已经结束（解约）的订阅收到「扣到了钱」，见 ErrChargeEndedSubscription 那条说明：
	// 报错进死信，不在这里替它做决定。
	if current.Status == model.SubscriptionStatusCancelled {
		return false, ErrChargeEndedSubscription
	}

	// 幂等：同一笔渠道流水只能聚一次会员。
	//
	// 事件的重复投递由平台收件箱挡在第一道（按 event id 去重），这一道挡的是另一件事：**两条
	// 内容不同的事件说的是同一笔钱**（支付侧对同一次扣款发过两遍——通知重投、或者补发）。渠道
	// 流水号是那笔钱的身份证，所以幂等键用它，写进流水撞 membership_changes_request_unique。
	//
	// 与 ApplyPaidOrder 一样，**约束落在最后那一条 INSERT 上**：前面的续期与计数器已经改过了，
	// 是它撞上索引让整个事务回滚，才没有把会员续两期。这里先查一次是**为了能安静地 ack**，
	// 不是幂等的全部——真正兜底的是那条索引（查完到写入之间的空隙由它收）。
	done, err := hasChangeWithRequest(ctx, tx, current.UserID, model.ChangeRenew, p.ProviderTransactionID)
	if err != nil {
		return false, err
	}
	if done {
		return false, nil
	}

	membership, err := membershipByIDForUpdate(ctx, tx, current.MembershipID)
	if err != nil {
		return false, err
	}
	// 撤销是人工做的决定（风控、争议、退款），一次扣款不该把它推翻，也不能默默吞掉这笔钱——
	// 与 ApplyPaidOrder 对 revoked 的处置逐字相同，返回错误让它进死信。
	if membership.Status == model.MembershipStatusRevoked {
		return false, ErrMembershipRevoked
	}

	renewed, err := renewMembership(ctx, tx, membership, renewParams{
		Snapshot: carriedSnapshot(membership, current),
		// 归属门店不传（空串 = 保持原值）：代扣是一次提前续费，而「多交一次钱不改变他是谁拉来的」
		// 是这一列的规矩（见 renewMembership）。
		StoreID: "",
		// 叠加的时长取自**订阅**上的 period / period_count，不是会员行、也不是套餐现查：那是签约
		// 时约定死的一期有多长，代扣按它扣、就该按它叠（与 PriceCents 同一条道理）。
		Extend: func(base time.Time) time.Time {
			return model.AddPeriod(base, current.Period, current.PeriodCount)
		},
	}, occurredAt)
	if err != nil {
		return false, err
	}

	// 下一次扣款 = 新的会员到期日。
	//
	// **这不是一条新规则**：renewMembership 里算 expire_at 用的就是 applySettle 那条基点判据
	// （还有剩余就叠在剩余上，已经过期就从这一刻重新起算）加同一个周期，所以会员的新到期日与
	// 「从旧基点叠加」逐字相等。照抄一遍 AddPeriod 等于把那条判据写两处，两处迟早走偏——而走偏
	// 的表现是「用户被扣了两次，因为两次算出了两个期次」。
	nextChargeAt := renewed.ExpireAt

	// 扣到钱了，就把订阅留成能继续扣的样子。
	//
	// **suspended → active**：停扣的目的是止损，不是惩罚一个已经付了钱的人——而把他留在
	// suspended 里会让这一行**再也不会被 ListDueSubscriptions 捞到**，从此静悄悄地不再续期，
	// 钱却已经收了。suspended_at 一并清掉，它是「为什么停的」那一段的墓碑，不是历史。
	//
	// 其余状态原样不动（active 还是 active；pending_sign 到不了这里——没签约就没有协议可扣）。
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE membership_subscriptions SET
		status = CASE WHEN status = $2 THEN $3 ELSE status END,
		suspended_at = CASE WHEN status = $2 THEN NULL ELSE suspended_at END,
		last_charge_at = $4, charge_count = charge_count + 1,
		failed_count = 0, consecutive_failed_count = 0,
		next_charge_at = $5, updated_at = NOW()
		WHERE id = $1
		RETURNING `+subscriptionColumns,
		current.ID, model.SubscriptionStatusSuspended, model.SubscriptionStatusActive,
		occurredAt, utc(nextChargeAt)))
	if err != nil {
		return false, mapPGError(err)
	}
	current.Subscription = updated

	// 流水记的是**会员**身上发生的事（挂在 membership_id 上，与订阅那条 subscribe 流水同一个
	// 位置），类型写 renew：对用户来说这就是一次续费，与买会员买出来的那一次在时间线上是同一句话
	// （见 model.ChangeRenew）。
	if err := insertChange(ctx, tx, model.Change{
		MembershipID: renewed.ID,
		UserID:       renewed.UserID,
		ChangeType:   model.ChangeRenew,
		FromStatus:   optionalText(membership.Status),
		ToStatus:     optionalText(renewed.Status),
		FromExpireAt: &membership.ExpireAt,
		ToExpireAt:   &renewed.ExpireAt,
		PlanID:       optionalText(renewed.PlanID),
		// OrderID 是上游为这一期建的续费单（order-service 的 orders.id）——**代扣是有订单的**，
		// 它与别的购买在后台订单管理里长得一样（source=renewal）。写它同时拿到一道库上的兜底：
		// membership_changes_order_unique 把那 (order_id, type) 组合钉成一条不变式。
		OrderID:      optionalText(p.OrderID),
		OperatorType: model.OperatorSystem,
		Metadata: mustJSON(map[string]any{
			"agreementId":           p.AgreementID,
			"bizPeriod":             p.BizPeriod,
			"amount":                p.Amount,
			"providerTransactionId": p.ProviderTransactionID,
			"autoCharge":            true,
		}),
		// 渠道流水号就是这条流水的幂等凭据（见上面那段）。
		RequestID:  p.ProviderTransactionID,
		OccurredAt: occurredAt,
	}); err != nil {
		return false, err
	}

	// 事件**必须**带订单号：coupon-service 的发券判据里有一条就是「orderId 非空」——它拿这一位
	// 区分「一次真实成交」与「后台人工改了有效期」（后者也发 membership.renewed，但不该发券）。
	// 空着的话，包月用户每期扣了钱、当期那批会员价券一张都不会发，而且两边都不报错。
	//
	// 它同时是发券的幂等键（coupon_batches.request_id = membershipID:orderID），所以这一位必须在
	// **同一个订单号**上稳定——重投时 p.OrderID 来自订单域的幂等回放，落的是同一张单。
	if err := appendOutbox(ctx, tx, eventTypeFor(model.ChangeRenew), dto.EventVersion, p.TraceID,
		mustJSON(membershipEvent(renewed, p.OrderID, occurredAt))); err != nil {
		return false, err
	}
	return true, nil
}

// settleChargeFailed 是「这一期没扣到」的落点。返回（这次是否停掉了订阅，这次有没有真的写）。
func settleChargeFailed(ctx context.Context, tx pgx.Tx, current *SubscriptionRow, p ChargeSettleParams, occurredAt time.Time) (bool, bool, error) {
	// 一条已经结束的订阅收到「这一期没扣成」：什么都不用做。它本来就不再扣款了，而给它累加失败
	// 计数会把一条正常的终点写成「连续失败」的假象。钱没动，所以这里没有值得惊动任何人的事。
	if current.Status == model.SubscriptionStatusCancelled || current.Status == model.SubscriptionStatusExpired {
		return false, false, nil
	}

	// 计数在 SQL 里自己加（`consecutive_failed_count + 1`），不在 Go 里算好再写：这一行已经被
	// FOR UPDATE 锁住了没错，但「读到几就是几」这种写法一旦有人在锁外调它就会静默少算一次
	// ——而少算的那次正好是「该停扣了」那一次。
	//
	// 判停的那两列读的都是**更新前**的值（PostgreSQL 的 SET 表达式全部对着旧行求值），所以
	// status 与 suspended_at 看到的是同一个判断，不会一个翻了一个没翻。
	updated, err := scanSubscription(tx.QueryRow(ctx, `UPDATE membership_subscriptions SET
		failed_count = failed_count + 1,
		consecutive_failed_count = consecutive_failed_count + 1,
		status = CASE WHEN status = $2 AND consecutive_failed_count + 1 >= $3 THEN $4 ELSE status END,
		suspended_at = CASE WHEN status = $2 AND consecutive_failed_count + 1 >= $3 THEN $5 ELSE suspended_at END,
		updated_at = NOW()
		WHERE id = $1
		RETURNING `+subscriptionColumns,
		current.ID, model.SubscriptionStatusActive, model.MaxConsecutiveChargeFailures,
		model.SubscriptionStatusSuspended, occurredAt))
	if err != nil {
		return false, false, mapPGError(err)
	}
	suspended := updated.Status == model.SubscriptionStatusSuspended &&
		current.Status != model.SubscriptionStatusSuspended
	current.Subscription = updated

	// 失败流水：**request_id 留空是有意的**。
	//
	// 一期可以合理地失败三次（那正是 consecutive_failed_count 数到 3 的来源），拿期次或协议号
	// 当幂等键会让后面两条撞上唯一索引、被静默当成重复投递丢掉——计数永远到不了 3，**扣款永远
	// 不会停**。所以这一条的重复投递只由平台的收件箱按 event id 挡（支付侧对同一期只会发出一条
	// failed：状态机拒绝把已经失败的一期再推一次）。
	if err := insertChange(ctx, tx, model.Change{
		MembershipID: updated.MembershipID,
		UserID:       updated.UserID,
		ChangeType:   model.ChangeChargeFailed,
		// from/to status 留空：这一行不改会员的任何东西（会员的状态与到期日都没动，见
		// ChangeChargeFailed 的说明），写一对相同的值只会让时间线多出一句「什么都没变」。
		// 变的是**订阅**，那三个字在 metadata 里。
		PlanID:       optionalText(updated.PlanID),
		OperatorType: model.OperatorSystem,
		Metadata: mustJSON(map[string]any{
			"agreementId":           p.AgreementID,
			"bizPeriod":             p.BizPeriod,
			"amount":                p.Amount,
			"providerTransactionId": p.ProviderTransactionID,
			"failureCode":           p.FailureCode,
			"failureMessage":        p.FailureMessage,
			"subscriptionStatus":    updated.Status,
			"failedCount":           updated.FailedCount,
		}),
		OccurredAt: occurredAt,
	}); err != nil {
		return false, false, err
	}

	if suspended {
		// 停扣单独记一条：它是这一支唯一的**后果**（后面这个人不再被扣款），而上面那条
		// charge_failed 说的是原因。两件事一条流水的话，运营在时间线上看到的是「第 3 次失败」，
		// 看不到「从这一刻起不会再扣了」。
		if err := insertChange(ctx, tx, model.Change{
			MembershipID: updated.MembershipID,
			UserID:       updated.UserID,
			ChangeType:   model.ChangeSuspend,
			// 同样不写 from/to status：那是**会员**的状态列，而 suspended 不是会员状态
			// （见 model.MembershipStatus*）。订阅的状态变化在 metadata 里。
			PlanID:       optionalText(updated.PlanID),
			OperatorType: model.OperatorSystem,
			Metadata: mustJSON(map[string]any{
				"agreementId":            p.AgreementID,
				"bizPeriod":              p.BizPeriod,
				"failureCode":            p.FailureCode,
				"consecutiveFailedCount": updated.ConsecutiveFailedCount,
				"subscriptionStatus":     updated.Status,
			}),
			OccurredAt: occurredAt,
		}); err != nil {
			return false, false, err
		}
	}

	// **不发事件**：停扣改的是「以后还扣不扣」，改不了这个人的会员资格（expire_at 一个字节都
	// 没动，判会员价读的是它）。emitsEvent 对这两种变更都是 false，这里是那句判断的落点。
	return suspended, true, nil
}

// carriedSnapshot 把会员行上现有的那份成交快照原样抄成一个 renewParams 的输入。
//
// 代扣续的是**已经买过的那份权益**，不是一次新的选购：套餐、会员价怎么算，签约那一刻就定了
// （subscription.price_cents 与它同一套道理）。所以这里一个字段都不现查——按 subscription.plan_id
// 去读 membership_plans 会让一次后台改价/改券配置改写一个正在被扣款的用户，而他下个月扣的还是
// 签约时那个数。
//
// Period / PeriodCount 取自**订阅**而不是会员行：会员行上根本没有这两列，而订阅上那一对是签约
// 时约定死的一期时长（renewParams.Extend 用的就是它）。这两个值不进 memberships 的 UPDATE
// （见 renewMembership 的列清单），只在这一次续期的计算里用。
func carriedSnapshot(m *model.Membership, sub *SubscriptionRow) MembershipSnapshot {
	snapshot := MembershipSnapshot{
		PlanID:          m.PlanID,
		PlanCode:        m.PlanCode,
		PlanName:        m.PlanName,
		MemberPriceMode: m.MemberPriceMode,
		Period:          sub.Period,
		PeriodCount:     sub.PeriodCount,
		AutoRenew:       m.AutoRenew,
	}
	// 两个券列在 model 上是指针（库上可空），在快照上是值。只在 coupon 模式下取值：auto 模式下
	// 它们必须写成 NULL 而不是空串/0（memberships 上那条 CHECK 钉着），couponColumns 会照模式
	// 决定写不写——所以这里只要不把指针里的垃圾值带出来就行。
	if m.MemberPriceCouponTemplateID != nil {
		snapshot.MemberPriceCouponTemplateID = *m.MemberPriceCouponTemplateID
	}
	if m.MemberPriceCouponsPerPeriod != nil {
		snapshot.MemberPriceCouponsPerPeriod = *m.MemberPriceCouponsPerPeriod
	}
	return snapshot
}

// errUnsupportedChargeTarget 是「给了一个不认识的目标状态」。
//
// 它不单独进错误清单（调用方不该去 errors.Is 它）：走到这里说明本服务自己写错了调用，而不是
// 外部的某种正常状况——与 SettleSubscription 那条 unsupported settle target 同一个处置。
func errUnsupportedChargeTarget(target string) error {
	return errors.New("membership: unsupported charge settle target " + target)
}
