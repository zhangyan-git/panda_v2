package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// membershipColumns 是 memberships 的列清单，与本包其它实体同一条规矩：全局唯一一份。
//
// legacy_id 归一化成空串（与 user-service 的 userColumns 同一条写法）：这一列可空——迁移过来
// 的会员才有值，新开通的都是 NULL——而 model.Membership.LegacyID 是个普通 string。不归一化的
// 话，**每一个新开通的会员都读不出来**，报的还是 "cannot scan NULL into *string" 这种与会员
// 毫不相干的扫描错误。其余可空列（券模板、四个时间戳）在模型上是 *T，NULL 本来就装得下。
const membershipColumns = `id, user_id, COALESCE(legacy_id, ''), COALESCE(store_id::text, ''), plan_id, plan_code, plan_name,
	member_price_mode, member_price_coupon_template_id, member_price_coupons_per_period,
	status, start_at, expire_at, auto_renew, auto_renew_off_at,
	renewal_count, last_renewed_at, frozen_at, freeze_reason, revoked_at, revoke_reason,
	created_at, updated_at`

// MembershipSnapshot 是一次成交带来的会员配置。
//
// 它是 membership_plans 的一组列在**下单那一刻**的拷贝，来源是订单行上的
// membership_plan_snapshot（见 dto.OrderMembershipSnapshot）。开通与续期都原样写进
// memberships 的同名快照列——**不按 PlanID 现查套餐**：现查等于让一次后台改价改写所有在途订单。
type MembershipSnapshot struct {
	PlanID                      string
	PlanCode                    string
	PlanName                    string
	MemberPriceMode             string
	MemberPriceCouponTemplateID string
	MemberPriceCouponsPerPeriod int32
	// Period / PeriodCount 是这一单买到的一段时长（month/1 或 year/1），续期按日历加。
	Period      string
	PeriodCount int32
	// AutoRenew 是**套餐层面**的「这个产品要签代扣」，写进新开通的 memberships.auto_renew 作为
	// 初始值。它不是用户的开关：续期时一个字节都不动（见 renewMembership）。
	AutoRenew bool
}

// couponColumns 把券配置翻成两个可空列。
//
// auto 模式必须写入 NULL 而不是空串/0：memberships 上有一条 CHECK 规定 auto 模式下这两列必须
// 为 NULL，coupon 模式下必须都有值。写成空串会撞 CHECK 报 23514（一条 500），而它其实是调用方
// 漏了配置——所以这里只做「模式对不对」的搬运，配没配齐由 service 在校验里拦。
func (s MembershipSnapshot) couponColumns() (*string, *int32) {
	if s.MemberPriceMode != model.MemberPriceModeCoupon {
		return nil, nil
	}
	template, count := s.MemberPriceCouponTemplateID, s.MemberPriceCouponsPerPeriod
	return &template, &count
}

// PaidOrderParams 是一张已支付订单对会员的全部输入。
//
// 订单号、用户、快照、业务时刻四样就是本服务需要的一切：**钱的事到 order-service 为止**,
// 金额只进流水备查，本服务不做任何定价与退款计算。
type PaidOrderParams struct {
	UserID string
	// OrderID 是幂等凭据：membership_changes 上有 order_id 上的部分唯一索引（限开通与续期，
	// 见 migrations/membership），同一单被处理两遍会撞上它（见 ErrDuplicateChange）。
	// 按 order_id 而不是 order_id+变更类型：重投走的是另一支（首次开通、重投续期）。
	OrderID string
	// OccurredAt 是**支付成功时刻**，不是处理时刻。补投一条上周的事件时，会员的 start_at
	// 与叠加基点都该是上周的那个时刻，否则「这个月开了多少会员」会随一次重投而变。
	OccurredAt time.Time
	Snapshot   MembershipSnapshot
	// StoreID 是这一单成交所在的门店，用作**归属门店**的来源（见 model.Membership.StoreID）。
	//
	// 空串表示这一单没有门店（小程序线上买会员就是这种），等于「这一次没有归属可定」——
	// 此时已过期重开的那一支会保留原值，而不是把归属擦掉。
	StoreID string
	// TraceID / RequestID 一路透传到 outbox 与流水，让「这一单为什么让他成了会员」可追。
	TraceID   string
	RequestID string
}

// ApplyPaidOrder 处理「一张买会员的订单付成功了」。
//
// 一个事务里做完四件事：查/建这条会员、按日历叠加有效期、写变更流水、落 outbox 事件。
//
// # 开通与续期是同一条路径
//
// 先锁住这个用户的会员行再决定走哪一支，而不是让调用方先查一次再选函数：读与写之间的那段
// 空隙里可以插进另一笔支付（同一张单的回调重放、用户连点两次），两次都读到「还没有会员」
// 就会各自建一条，撞 memberships_user_unique 报 500——而它们本该是「一次开通 + 一次续期」。
// 锁 + 单条路径把这件事变成一句「有没有行」。
//
// # 续期的叠加基点
//
//   - 还在有效期内（active / frozen 且 expire_at 未来）：在 expire_at 上叠加。这是原型的
//     「在当前剩余天数上增加」——提前续费不该把没用完的那几天吞掉。
//   - 已经过期：从**支付时刻**重新起算。在一个过期时刻上叠加会让用户买完立刻又是过期的，
//     而这正是「续费后会员没生效」这类工单的成因。
//
// # 快照每次成交都覆盖
//
// 用户可以从连续包月换成年度会员，两份套餐的会员价路径不同（coupon → auto）。行的快照列表达
// 的是**这份会员现在的权益来自哪一次成交**，所以它跟着最近一次成交走；历史在
// membership_changes 里（每条流水都记着自己那一次的 plan_id），两条序列合起来既答「现在
// 按什么算」也答「一路怎么变过来的」。
//
// # 已撤销（revoked）的会员不自动恢复
//
// 撤销是人工做的决定（风控、争议、退款），一次付款不该把它推翻；但也不能默默吞掉这笔钱。
// 所以这里返回错误让消息进死信——那里是人会来看的地方（见 dto 里关于 ack 规则的说明）。
func (r *PostgresRepository) ApplyPaidOrder(ctx context.Context, p PaidOrderParams) (*model.Membership, error) {
	var result *model.Membership
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		occurredAt := utc(p.OccurredAt)
		current, err := membershipForUpdate(ctx, tx, p.UserID)
		if err != nil && !errors.Is(err, ErrMembershipNotFound) {
			return err
		}

		var (
			membership *model.Membership
			changeType string
			fromStatus string
			fromExpire *time.Time
		)

		if current == nil {
			membership, err = createMembership(ctx, tx, createParams{
				UserID:   p.UserID,
				Snapshot: p.Snapshot,
				StartAt:  occurredAt,
				ExpireAt: model.AddPeriod(occurredAt, p.Snapshot.Period, p.Snapshot.PeriodCount),
				StoreID:  p.StoreID,
				// 支付那条：套餐层面的 auto_renew 就是这条会员的初值（见 MembershipSnapshot）。
				AutoRenew: p.Snapshot.AutoRenew,
			})
			if err != nil {
				return err
			}
			changeType = model.ChangeActivate
		} else {
			if current.Status == model.MembershipStatusRevoked {
				return ErrMembershipRevoked
			}
			fromStatus = current.Status
			expire := current.ExpireAt
			fromExpire = &expire
			membership, err = renewMembership(ctx, tx, current, renewParams{
				Snapshot: p.Snapshot,
				StoreID:  p.StoreID,
				// 支付那条按套餐日历加（month/1、year/1），不按天数。
				Extend: func(base time.Time) time.Time {
					return model.AddPeriod(base, p.Snapshot.Period, p.Snapshot.PeriodCount)
				},
			}, occurredAt)
			if err != nil {
				return err
			}
			changeType = model.ChangeRenew
		}

		if err := insertChange(ctx, tx, model.Change{
			MembershipID: membership.ID,
			UserID:       membership.UserID,
			ChangeType:   changeType,
			FromStatus:   optionalText(fromStatus),
			ToStatus:     optionalText(membership.Status),
			FromExpireAt: fromExpire,
			ToExpireAt:   &membership.ExpireAt,
			PlanID:       optionalText(membership.PlanID),
			OrderID:      optionalText(p.OrderID),
			// 这一条是**系统**记的：它由支付成功事件驱动，不是用户点的、也不是后台点的。
			// 把它记成 user 会让「谁动了这个会员」在流水里指错人。
			OperatorType: model.OperatorSystem,
			OccurredAt:   occurredAt,
		}); err != nil {
			return err
		}

		if err := appendOutbox(ctx, tx, eventTypeFor(changeType), dto.EventVersion, p.TraceID,
			mustJSON(membershipEvent(membership, p.OrderID, occurredAt))); err != nil {
			return err
		}
		result = membership
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// createParams 是「新开通一条会员」所需的全部输入，两条开通路径共用。
//
// 它从 PaidOrderParams 里拆出来，是因为后台直接开通（GrantMembership）**没有订单**：起算
// 时刻与到期日都由调用方算好给进来，而不是从支付时刻按套餐日历加出来。
type createParams struct {
	UserID   string
	Snapshot MembershipSnapshot
	// StartAt / ExpireAt 由调用方算好：支付那条从支付时刻按套餐日历加；后台那条到期日由
	// 操作员给（没给才按套餐算）。「起点取哪个时刻」是两条路各自的业务规则，不是写库的事。
	StartAt  time.Time
	ExpireAt time.Time
	// StoreID 是归属门店，空串写成 NULL（见 model.Membership.StoreID）。
	StoreID string
	// AutoRenew 是 memberships.auto_renew 的初值。**两条路取值不同**：支付那条抄快照上的
	// 套餐字段（那个产品支持代扣，用户口径上就是开着）；后台那条恒为 false——后台开通没有
	// 任何签约，置 true 会让界面显示「已开启自动续费」却永远扣不了款。
	AutoRenew bool
}

// createMembership 新开通一条会员。有效期由调用方给，这里不做任何日期推算。
func createMembership(ctx context.Context, tx pgx.Tx, p createParams) (*model.Membership, error) {
	template, count := p.Snapshot.couponColumns()
	membership, err := scanMembership(tx.QueryRow(ctx, `INSERT INTO memberships
		(user_id, store_id, plan_id, plan_code, plan_name,
		 member_price_mode, member_price_coupon_template_id, member_price_coupons_per_period,
		 status, start_at, expire_at, auto_renew)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING `+membershipColumns,
		p.UserID, optionalID(p.StoreID), p.Snapshot.PlanID,
		trimOrEmpty(p.Snapshot.PlanCode), trimOrEmpty(p.Snapshot.PlanName),
		p.Snapshot.MemberPriceMode, template, count,
		model.MembershipStatusActive, utc(p.StartAt), utc(p.ExpireAt),
		p.AutoRenew))
	if err != nil {
		return nil, mapPGError(err)
	}
	return membership, nil
}

// renewParams 是「把一条已有会员往后叠一段」所需的全部输入。
//
// 它从 PaidOrderParams 里拆出来，理由与 createParams 逐字相同：**两条路（支付、店铺码活动
// 领取）的差别只是「加多久」，不是「从哪加」**。基点那条规则（还有剩余就叠在剩余上，已经
// 过期就从这一笔的时刻重新起算）只有一份，所以它留在 renewMembership 里；调用方只回答
// 「在基点上加多久」——支付那条按套餐日历加，领取那条按天加。
type renewParams struct {
	Snapshot MembershipSnapshot
	// StoreID 为空表示这一次没有归属可定（保持原值）。归属规则的三条判据见 renewMembership。
	StoreID string
	// Extend 从叠加基点算出新的到期时刻。
	Extend func(base time.Time) time.Time
}

// renewMembership 把有效期往后叠一段，并覆盖成交快照。
func renewMembership(ctx context.Context, tx pgx.Tx, current *model.Membership, p renewParams, occurredAt time.Time) (*model.Membership, error) {
	base := occurredAt
	// 还有剩余有效期就在剩余的基础上叠加；已经过期则从这一笔的时刻重新起算（见 ApplyPaidOrder）。
	if current.ExpireAt.After(occurredAt) {
		base = current.ExpireAt
	}
	// 过期了才重新变 active；frozen 原样留着——续费加的是时间，不该顺手把风控的暂停解开
	// （那是一个需要人来做的决定，见 Unfreeze）。
	status := current.Status
	if status == model.MembershipStatusExpired {
		status = model.MembershipStatusActive
	}

	// 归属门店：**只有「这一次是一次新的开通」时才重定**，而那正好就是上面 base 走 occurredAt
	// 的那一支——已经过期 = 上一段会员关系结束了，谁拉来的可以重新算（店铺码活动、在门店买
	// 咖啡时开会员，就是从这里改归属的）。还在有效期内（提前续费、升级、换套餐）一律保留原值：
	// 多交一次钱不改变他是谁拉来的。
	//
	// 判据与 base 逐字相同（expire_at 过没过，不看 status），不是新加一套逻辑，而是让同一件事
	// 只有一个判据。写成 SQL 里的 CASE 就是第二个地方，两处迟早走偏。
	//
	// 这一次没带门店（p.StoreID 为空）时保留原值而不是清空：线上买会员留下的是一条没有归属的
	// 记录，不该把一段真实存在过的归属关系擦掉。
	storeID := current.StoreID
	if !current.ExpireAt.After(occurredAt) && p.StoreID != "" {
		storeID = p.StoreID
	}

	template, count := p.Snapshot.couponColumns()
	membership, err := scanMembership(tx.QueryRow(ctx, `UPDATE memberships SET
		plan_id=$2, plan_code=$3, plan_name=$4,
		member_price_mode=$5, member_price_coupon_template_id=$6, member_price_coupons_per_period=$7,
		status=$8, expire_at=$9, store_id=$10,
		renewal_count=renewal_count+1, last_renewed_at=$11, updated_at=NOW()
		WHERE id=$1
		RETURNING `+membershipColumns,
		current.ID, p.Snapshot.PlanID, trimOrEmpty(p.Snapshot.PlanCode), trimOrEmpty(p.Snapshot.PlanName),
		p.Snapshot.MemberPriceMode, template, count,
		status, utc(p.Extend(base)),
		optionalID(storeID), occurredAt))
	if err != nil {
		return nil, mapPGError(err)
	}
	return membership, nil
}

// GrantParams 是「后台直接开通一个会员」的全部输入。
type GrantParams struct {
	UserID string
	// Snapshot 是**开通那一刻**从套餐现查出来的那份配置，不是订单带过来的成交快照。
	//
	// 这是「快照说了算」这条本域反复申明的规矩唯一的例外，必须说清楚：订单那条路带的是下单
	// 那一刻的拷贝，后台开通**没有订单**，只能当场取套餐。取完立刻落进快照列，之后套餐再改
	// 都不追溯——最终形状与成交那条路一致，只是取值时点从「下单」变成「开通」。
	Snapshot MembershipSnapshot
	// StartAt 恒为开通时刻；ExpireAt 是操作员给的绝对到期日（没给才由 service 按套餐算）。
	StartAt  time.Time
	ExpireAt time.Time
	// StoreID 是操作员选的门店，可空。它只写进这条新会员，不参与任何判定与金额计算。
	StoreID string
	// AdminID / Reason / Remark 是这次人工开通的留痕：进流水、进审计、进事件。
	AdminID string
	Reason  string
	Remark  string
	TraceID string
	// RequestID 既是流水上的留痕串，也是这次点击的**幂等键**：同一个用户、同一个 activate、
	// 同一个 requestId 只落一条流水（membership_changes_request_unique）。没有它，一次网络
	// 抖动后的重试会让操作员看到「这个人已经是会员了」——而那在这种场景下是假警报，他刚亲手
	// 建的那条。
	RequestID string
}

// GrantMembership 由后台直接开通一个会员，不经过任何订单或支付。
//
// 它存在的原因是一个运营上的死口子：客服补偿、线下活动、渠道争议里，用户说「你们店长答应
// 送我一年会员」，而本域在此之前**只有**状态迁移（冻结/解冻/撤销/改有效期），没有创建入口
// ——没有会员就无从调整，那个承诺在系统里做不出来。
//
// # 已有会员一律拒绝，不叠加
//
// 一个用户只有一条会员（memberships_user_unique），叠加等于把「开通」偷偷变成「续期」，而
// 会员详情页已经有专门的「调整有效期」。所以这里不区分「已过期」与「还在有效期」——两条都
// 回 ErrMembershipExists。
//
// 由此推出一条边界：**后台不改归属门店**。「会员过期后重新开通可变归属」那个例外只点名了
// 两个渠道——店铺码活动、在门店买咖啡时开会员——都是「一次新的成交或领取」，而后台开通不是
// 成交。将来若要让后台也能改，是把下面那句 return 换成「已过期则放行」，判据与 renewMembership
// 里同一处，改动是一行而不是一层。
//
// # 与支付那条路的差别只有三处
//
// 起算时刻与到期日由调用方给、auto_renew 强制 false（后台开通没有签约，置 true 会让界面
// 显示「已开启自动续费」却永远扣不了款）、以及**记审计**——这是人工白送钱，符合方案 §11.6
// 「影响用户资产归属的人工操作」。
func (r *PostgresRepository) GrantMembership(ctx context.Context, p GrantParams) (*model.Membership, error) {
	var result *model.Membership
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		occurredAt := utc(p.StartAt)
		current, err := membershipForUpdate(ctx, tx, p.UserID)
		if err != nil && !errors.Is(err, ErrMembershipNotFound) {
			return err
		}

		if current != nil {
			replayed, err := hasChangeWithRequest(ctx, tx, p.UserID, model.ChangeActivate, p.RequestID)
			if err != nil {
				return err
			}
			if !replayed {
				return ErrMembershipExists
			}
			// 是这次点击的重放：回他刚建的那条，什么都不写。第二次提交与第一次的结果必须
			// 一模一样，否则客户端重试会得到一个「可能是我们刚建的，也可能是别人建的」的答案。
			result = current
			return nil
		}

		membership, err := createMembership(ctx, tx, createParams{
			UserID:    p.UserID,
			Snapshot:  p.Snapshot,
			StartAt:   occurredAt,
			ExpireAt:  utc(p.ExpireAt),
			StoreID:   p.StoreID,
			AutoRenew: false, // 见上面第三段：后台开通没有签约。
		})
		if err != nil {
			return err
		}

		// FromStatus / FromExpireAt 留空：之前没有这条会员（见 model.Change 的说明）。
		if err := insertChange(ctx, tx, model.Change{
			MembershipID: membership.ID,
			UserID:       membership.UserID,
			ChangeType:   model.ChangeActivate,
			ToStatus:     optionalText(membership.Status),
			ToExpireAt:   &membership.ExpireAt,
			PlanID:       optionalText(membership.PlanID),
			OperatorType: model.OperatorAdmin,
			OperatorID:   optionalID(p.AdminID),
			Reason:       p.Reason,
			Remark:       p.Remark,
			RequestID:    p.RequestID,
			OccurredAt:   occurredAt,
		}); err != nil {
			return err
		}

		// 与支付驱动那条路发**同一种**事件：下游关心的是「这个人成了会员」，不关心是谁促成的。
		// 订单号为空是准确的——这次确实没有订单。
		if err := appendOutbox(ctx, tx, eventTypeFor(model.ChangeActivate), dto.EventVersion, p.TraceID,
			mustJSON(membershipEvent(membership, "", occurredAt))); err != nil {
			return err
		}

		// 审计条件不像 mutateMembership 那样挂在 operatorType 上：这条路径只可能是人工开的，
		// 没有人会替系统调它。所以它无条件记，少一个「忘了传 operatorType 就没有审计」的分支。
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			ActorID: p.AdminID, Module: "membership",
			Action: model.ChangeActivate, Operation: "开通会员",
			TargetType: "membership", TargetID: membership.ID, TargetName: membership.PlanName,
			After: audit.Snapshot(map[string]any{
				"status": membership.Status, "expireAt": membership.ExpireAt,
				"storeId": membership.StoreID, "reason": p.Reason,
			}),
		}); err != nil {
			return err
		}

		result = membership
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// hasChangeWithRequest 回答「这个用户的这种变更，是不是已经用这个 request_id 记过一次」。
//
// 它服务于重试：调用方在「已经有一条会员」时先问它，是重放就回那条、不是才报冲突。
// request_id 为空串时恒为 false——部分唯一索引的 WHERE 也把空串排除在外
// （见 membership_changes_request_unique）。
func hasChangeWithRequest(ctx context.Context, q querier, userID, changeType, requestID string) (bool, error) {
	if requestID == "" {
		return false, nil
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM membership_changes
		WHERE user_id=$1 AND change_type=$2 AND request_id=$3)`,
		userID, changeType, requestID).Scan(&exists); err != nil {
		return false, mapPGError(err)
	}
	return exists, nil
}

// membershipForUpdate 按 user_id 取那条唯一的会员并锁住它。
//
// **锁的是行，不是「有没有行」**：一个还没有会员的用户被两笔支付同时处理时，两边的 FOR UPDATE
// 都锁不到东西、各自建行，最后撞 memberships_user_unique——第二次会拿到 ErrMembershipExists
// 而不是静默建重。这是可以接受的（重投一次就走续期分支），但要知道它存在。
//
// 查不到时返回 ErrMembershipNotFound，调用方据此判断走开通分支。
func membershipForUpdate(ctx context.Context, tx pgx.Tx, userID string) (*model.Membership, error) {
	membership, err := scanMembership(tx.QueryRow(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE user_id=$1 FOR UPDATE`, userID))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrMembershipNotFound
		}
		return nil, mapPGError(err)
	}
	return membership, nil
}

// GetMembership 按用户 ID 取会员。这是**会员价判定**的读路径，会被每一次下单调用。
func (r *PostgresRepository) GetMembership(ctx context.Context, userID string) (*model.Membership, error) {
	return r.scanMembershipFrom(ctx, r.pool,
		`SELECT `+membershipColumns+` FROM memberships WHERE user_id=$1`, userID)
}

// GetMembershipByID 按 ID 取会员。后台详情页走它（列表给的是会员 ID）。
func (r *PostgresRepository) GetMembershipByID(ctx context.Context, id string) (*model.Membership, error) {
	return r.scanMembershipFrom(ctx, r.pool,
		`SELECT `+membershipColumns+` FROM memberships WHERE id=$1`, id)
}

func (r *PostgresRepository) scanMembershipFrom(ctx context.Context, q querier, sql string, args ...any) (*model.Membership, error) {
	membership, err := scanMembership(q.QueryRow(ctx, sql, args...))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrMembershipNotFound
		}
		return nil, mapPGError(err)
	}
	return membership, nil
}

// ListMemberships 是后台会员列表。
//
// 排序固定 expire_at DESC：运营打开这个页面最常做的事是「看快到期的那批」，而到期时间也是
// 唯一一个所有筛选条件组合下都说得通的排序键（按创建时间排会让「这周到期的」散在全表里）。
func (r *PostgresRepository) ListMemberships(ctx context.Context, q dto.MembershipQuery) ([]*model.Membership, int, error) {
	const from = ` FROM memberships`
	where := membershipFilter(q)

	total, err := r.countRows(ctx, from, where)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	args, page := pageClause(where.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+membershipColumns+from+where.sql()+
		` ORDER BY expire_at DESC, id`+page, args...)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	defer rows.Close()

	memberships := make([]*model.Membership, 0, q.PageSize)
	for rows.Next() {
		membership, err := scanMembership(rows)
		if err != nil {
			return nil, 0, mapPGError(err)
		}
		memberships = append(memberships, membership)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapPGError(err)
	}
	return memberships, total, nil
}

// membershipFilter 拼会员列表的筛选条件。
func membershipFilter(q dto.MembershipQuery) whereClause {
	var where whereClause
	if userID := trimOrEmpty(q.UserID); userID != "" {
		where.add("user_id = $%d", userID)
	}
	if status := trimOrEmpty(q.Status); status != "" {
		where.add("status = $%d", status)
	}
	// 快照上的 code，不是 join 套餐现查：用户按当初买的那个产品筛，而套餐编码不可修改，
	// 所以这两个值今天恒等——但 join 会让一次套餐改名把筛选结果和历史对不上。
	if planCode := trimOrEmpty(q.PlanCode); planCode != "" {
		where.add("plan_code = $%d", planCode)
	}
	if q.ExpireFrom != nil {
		where.add("expire_at >= $%d", utc(*q.ExpireFrom))
	}
	if q.ExpireTo != nil {
		where.add("expire_at < $%d", utc(*q.ExpireTo))
	}
	// 三态：只看开着的、只看没开的、不筛。所以这里不能用 add（它每次都要塞一个参数），
	// 而布尔字面量本来也不需要占位符（见 whereClause.addRaw 的说明）。
	switch {
	case q.AutoRenew == nil:
	case *q.AutoRenew:
		where.addRaw("auto_renew")
	default:
		where.addRaw("NOT auto_renew")
	}
	return where
}

// scanMembership 把一行读成 model.Membership。列的顺序必须与 membershipColumns 逐字对应。
func scanMembership(row scanner) (*model.Membership, error) {
	var m model.Membership
	if err := row.Scan(
		&m.ID, &m.UserID, &m.LegacyID, &m.StoreID, &m.PlanID, &m.PlanCode, &m.PlanName,
		&m.MemberPriceMode, &m.MemberPriceCouponTemplateID, &m.MemberPriceCouponsPerPeriod,
		&m.Status, &m.StartAt, &m.ExpireAt, &m.AutoRenew, &m.AutoRenewOffAt,
		&m.RenewalCount, &m.LastRenewedAt, &m.FrozenAt, &m.FreezeReason,
		&m.RevokedAt, &m.RevokeReason, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

// ============================================================
// 人工干预路径
// ============================================================

// FreezeMembership 暂停权益。到期时间不动，解冻后继续。
//
// 它只接受 active：已经过期的会员没有什么可暂停的，重复冻结也不该覆盖掉第一次的原因——
// 冻结原因是给下一个看这条记录的人（客服、风控）看的，被后一次覆盖等于丢掉第一次的线索。
func (r *PostgresRepository) FreezeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error) {
	return r.mutateMembership(ctx, mutateParams{
		membershipID:   id,
		adminID:        adminID,
		reason:         reason,
		traceID:        traceID,
		requestID:      requestID,
		changeType:     model.ChangeFreeze,
		auditOperation: "冻结会员",
		apply: func(m *model.Membership) error {
			if m.Status != model.MembershipStatusActive {
				return ErrMembershipNotActive
			}
			return nil
		},
		sql: `UPDATE memberships SET status='frozen', frozen_at=NOW(), freeze_reason=$2, updated_at=NOW()
			WHERE id=$1 RETURNING ` + membershipColumns,
		extraArgs: func(m *model.Membership, p mutateParams) []any { return []any{p.reason} },
	})
}

// UnfreezeMembership 恢复被暂停的权益。
func (r *PostgresRepository) UnfreezeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error) {
	return r.mutateMembership(ctx, mutateParams{
		membershipID:   id,
		adminID:        adminID,
		reason:         reason,
		traceID:        traceID,
		requestID:      requestID,
		changeType:     model.ChangeUnfreeze,
		auditOperation: "解冻会员",
		apply: func(m *model.Membership) error {
			if m.Status != model.MembershipStatusFrozen {
				return ErrMembershipNotFrozen
			}
			return nil
		},
		// freeze_reason 留着不清：它记录的是**上一次为什么被冻**，而解冻原因写在流水里
		// （reason）。清掉它会让「这个人被冻过没有」在行上再也看不出来。
		sql: `UPDATE memberships SET status='active', frozen_at=NULL, updated_at=NOW()
			WHERE id=$1 RETURNING ` + membershipColumns,
	})
}

// RevokeMembership 撤销会员，不可恢复。
func (r *PostgresRepository) RevokeMembership(ctx context.Context, id, adminID, reason, traceID, requestID string) (*model.Membership, error) {
	return r.mutateMembership(ctx, mutateParams{
		membershipID:   id,
		adminID:        adminID,
		reason:         reason,
		traceID:        traceID,
		requestID:      requestID,
		changeType:     model.ChangeRevoke,
		auditOperation: "撤销会员",
		// 没有 apply：撤销接受除「已经被撤销」之外的任何状态（过期、冻结中都能撤销），而那一
		// 条由 mutateMembership 的骨架统一拦。
		sql: `UPDATE memberships SET status='revoked', revoked_at=NOW(), revoke_reason=$2,
			auto_renew=FALSE, updated_at=NOW()
			WHERE id=$1 RETURNING ` + membershipColumns,
		extraArgs: func(m *model.Membership, p mutateParams) []any { return []any{p.reason} },
	})
}

// AdjustExpireAt 后台人工调整有效期。
//
// 这是**最危险的一个后台操作**：它直接改写一个人还能不能用会员价，却看不出任何系统性的痕迹。
// 所以它同时做三件事——改行、记流水（含前后 expire_at）、发审计。三者缺一的话，「这个人
// 的会员为什么明年才到期」就没有答案。
func (r *PostgresRepository) AdjustExpireAt(ctx context.Context, id, adminID string, expireAt time.Time, reason, remark, traceID, requestID string) (*model.Membership, error) {
	return r.mutateMembership(ctx, mutateParams{
		membershipID:   id,
		adminID:        adminID,
		reason:         reason,
		remark:         remark,
		traceID:        traceID,
		requestID:      requestID,
		changeType:     model.ChangeAdminAdjust,
		auditOperation: "调整会员有效期",
		// 这个字段必须在：extraArgs 取的是 p.expireAt 而不是外面那个闭包变量。漏了它，参数会
		// 落成 time.Time 的零值（0001-01-01），于是每次调整都撞 CHECK (expire_at > start_at)
		// ——一次后台操作变成一条 23514（500），而且报出来的话里没有一个字提到「有效期」。
		expireAt: expireAt,
		apply: func(m *model.Membership) error {
			// 只允许把有效期改到开始时刻之后：memberships 上有 CHECK (expire_at > start_at)，
			// 撞上去是一条 23514（500）。这里先拦，让它变成一句说得清的话。
			if !utc(expireAt).After(utc(m.StartAt)) {
				return ErrExpireBeforeStart
			}
			return nil
		},
		sql: `UPDATE memberships SET expire_at=$2, updated_at=NOW()
			WHERE id=$1 RETURNING ` + membershipColumns,
		extraArgs: func(m *model.Membership, p mutateParams) []any { return []any{utc(p.expireAt)} },
	})
}

// SetAutoRenew 改自动续费开关。用户自己在小程序点、或后台替他点，走同一条路径——
// 差别只在 operatorType 与审计里记的人是谁。
//
// **它只动 memberships.auto_renew**，不去动 membership_subscriptions：签约与解约要调
// payment-service，那是服务层的编排（一次跨服务调用不能待在事务里，见 service 的说明）。
// 这里只负责把用户看到的那句话（「每月自动续费：开」）落成事实。
//
// # 调用方必须保证这个人名下没有活着的订阅
//
// 这个开关是订阅的投影（见 applyAutoRenew），所以「名下有活订阅时把关掉」不是改这一列，
// 而是**解约**——那件事走 SettleSubscription / CancelSubscription，它们会在同一个事务里
// 顺手把这一列拨灭。绕过它们直接调这里，留下的正是这一整块要防的那一格：界面显示已关闭，
// 而到期扫描照样按订阅行去扣款。
//
// 不在这里加一条断言（「有活订阅就不给改」）：这个函数拿到的是会员行，查订阅要多一次读，
// 而它挡住的是一处**写错就编译得过去**的调用——那种保证该在服务层的编排里说清（见
// service.SetAutoRenewByUser），不在这里多一道今天永远走不到的守卫。
func (r *PostgresRepository) SetAutoRenew(ctx context.Context, id string, enabled bool, operatorType, operatorID, reason, traceID, requestID string) (*model.Membership, error) {
	changeType := model.ChangeAutoRenewOff
	if enabled {
		changeType = model.ChangeAutoRenewOn
	}
	return r.mutateMembership(ctx, mutateParams{
		membershipID: id,
		adminID:      operatorID,
		operatorType: operatorType,
		reason:       reason,
		traceID:      traceID,
		requestID:    requestID,
		changeType:   changeType,
		apply: func(m *model.Membership) error {
			// **它只在打开时判**。给一个已经过期的会员打开自动续费，下一次扣款要等到明年——
			// 用户以为开了，实际上这个月什么都没发生。让他先续上再开。
			//
			// 关的时候一条判据都不要：过期的人恰恰更需要能把它关掉（他名下可能还挂着一条停扣
			// 的订阅，而这是他手上唯一还按得动的开关）。在这里多判一次，就是把「这个人永远关不掉
			// 自己的自动续费」变成一个没人会去试的 bug。
			if enabled && m.Status == model.MembershipStatusExpired {
				return ErrMembershipExpired
			}
			return nil
		},
		sql: `UPDATE memberships SET auto_renew=$2,
			auto_renew_off_at = CASE WHEN $2 THEN NULL ELSE NOW() END,
			updated_at=NOW()
			WHERE id=$1 RETURNING ` + membershipColumns,
		extraArgs: func(_ *model.Membership, p mutateParams) []any { return []any{p.enabled} },
		enabled:   enabled,
	})
}

// mutateParams 是各个人工干预路径的公共输入。
type mutateParams struct {
	membershipID string
	adminID      string
	// operatorType 是 user / admin / system / worker。它是流水里「谁动的」的第一层区分，
	// 缺了它就没法回答「这次是用户自己关的，还是客服帮他关的」。
	operatorType string
	reason       string
	remark       string
	traceID      string
	requestID    string
	changeType   string
	// auditOperation 是审计条目上那句人话（「冻结会员」）。空表示这次不记审计——系统与用户
	// 自己的动作不记，见 mutateMembership 的说明。
	auditOperation string
	// apply 是这一支自己的前置检查，在锁住行之后、写之前跑。返回错误即整体回滚。
	apply func(*model.Membership) error
	// sql 是这一支的 UPDATE，$1 恒为会员 ID，其余参数由 extraArgs 给出。
	sql string
	// extraArgs 拼 $2 起的参数。
	extraArgs func(*model.Membership, mutateParams) []any
	enabled   bool
	expireAt  time.Time
}

// mutateMembership 是人工干预路径的公共骨架：锁行 → 前置检查 → 改行 → 记流水 → 落事件 → 审计。
//
// # 为什么这些路径全都挤在一个骨架里
//
// 它们长得几乎一样，而**漏掉其中一步的后果各不一样**：漏了流水，「这个人的会员为什么变了」
// 查不出来；漏了事件，下游永远不知道会员变了；漏了审计，方案 §11.6 那条人工操作必审的要求
// 就没落地。写五遍就会有五处漏的机会。
//
// # 审计只在有人动手时记
//
// 调用方通过 operatorType 说清是谁在动：admin 才记审计（方案 §11.6：影响用户资产归属的人工
// 操作必审），user 与 system 不记——用户自己关掉自动续费、系统把到期的标成过期，每一天都在
// 发生，记进审计表只会把真正要看的几条淹掉。它们的痕迹在 membership_changes 里。
func (r *PostgresRepository) mutateMembership(ctx context.Context, p mutateParams) (*model.Membership, error) {
	if p.operatorType == "" {
		p.operatorType = model.OperatorAdmin
	}
	var result *model.Membership
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		current, err := membershipByIDForUpdate(ctx, tx, p.membershipID)
		if err != nil {
			return err
		}
		// 撤销是终态：被撤销的会员不接受任何人工动作。这一条放在**骨架**里，而不是各支自己的
		// apply 里——四个入口（冻结/解冻/调整有效期/开关自动续费）撞上它时该说的是同一句话。
		// 分开写的话，解冻那一支会说「这个会员没有被冻结，不用解冻」，而真实原因是「他被撤销
		// 了」，运营会照着那句话去检查一个毫不相关的东西。
		if current.Status == model.MembershipStatusRevoked {
			return ErrMembershipRevoked
		}
		if p.apply != nil {
			if err := p.apply(current); err != nil {
				return err
			}
		}

		args := []any{p.membershipID}
		if p.extraArgs != nil {
			args = append(args, p.extraArgs(current, p)...)
		}
		updated, err := scanMembership(tx.QueryRow(ctx, p.sql, args...))
		if err != nil {
			if isNoRows(err) {
				return ErrMembershipNotFound
			}
			return mapPGError(err)
		}

		// 前后两个快照都记：只记「变成了什么」时，「为什么会变」要靠翻上一次流水才能拼出来，
		// 而这条流水本身就该自足。
		fromExpire := current.ExpireAt
		if err := insertChange(ctx, tx, model.Change{
			MembershipID: updated.ID,
			UserID:       updated.UserID,
			ChangeType:   p.changeType,
			FromStatus:   optionalText(current.Status),
			ToStatus:     optionalText(updated.Status),
			FromExpireAt: &fromExpire,
			ToExpireAt:   &updated.ExpireAt,
			PlanID:       optionalText(updated.PlanID),
			OperatorType: p.operatorType,
			OperatorID:   optionalID(p.adminID),
			Reason:       p.reason,
			Remark:       p.remark,
			RequestID:    p.requestID,
			OccurredAt:   time.Now().UTC(),
		}); err != nil {
			return err
		}

		// 冻结与撤销要告诉下游（见 eventTypeFor）：它们的共同后果是「权益现在就没了」，
		// 而下单那一刻的会员价判定如果只看 expire_at 是看不出来的。
		if emitsEvent(p.changeType) {
			eventType := eventTypeFor(p.changeType)
			if err := appendOutbox(ctx, tx, eventType, dto.EventVersion, p.traceID,
				mustJSON(membershipEvent(updated, "", time.Now().UTC()))); err != nil {
				return err
			}
		}

		if p.operatorType == model.OperatorAdmin && p.auditOperation != "" {
			if err := r.recorder.Record(ctx, tx, audit.Entry{
				ActorID: p.adminID, Module: "membership",
				Action: p.changeType, Operation: p.auditOperation,
				TargetType: "membership", TargetID: updated.ID, TargetName: updated.PlanName,
				Before: audit.Snapshot(map[string]any{
					"status": current.Status, "expireAt": current.ExpireAt,
				}),
				After: audit.Snapshot(map[string]any{
					"status": updated.Status, "expireAt": updated.ExpireAt, "reason": p.reason,
				}),
			}); err != nil {
				return err
			}
		}

		result = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// membershipByIDForUpdate 按 ID 取会员并锁住它。人工干预路径的入口——它们手上只有后台列表
// 给的那个会员 ID。
func membershipByIDForUpdate(ctx context.Context, tx pgx.Tx, id string) (*model.Membership, error) {
	membership, err := scanMembership(tx.QueryRow(ctx,
		`SELECT `+membershipColumns+` FROM memberships WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrMembershipNotFound
		}
		return nil, mapPGError(err)
	}
	return membership, nil
}

// ============================================================
// 到期扫描
// ============================================================

// ExpireDue 把已经过期但状态还是 active 的会员标成 expired，返回处理了几条。
//
// # 它为什么存在
//
// 权益判定（IsUsable）看的是 expire_at，不看 status，所以**不扫也不会有人多享一天会员价**。
// 扫描是为了让 status 与 expire_at 说的是同一件事：后台列表按状态筛「有效会员」，客服查
// 「这人还是会员吗」，读的都是 status；一个停在 active 的过期会员会让这两个地方都给出反话。
//
// # 为什么按批、为什么 SKIP LOCKED
//
// 一次把全表扫完会开一个长事务，把每一个后面进来的支付回调堵在行锁上。SKIP LOCKED 让两个
// 副本同时扫描时各拿各的那一批，不会互相等——两边的 WHERE 都是 status='active'，被对方锁住
// 的行直接跳过，下一轮再扫。
//
// 一批一个事务（不是一批一条）：批内的流水与事件要么一起落地，要么一条都不落地，重扫一遍
// 即可。批与批之间可以重复，因为每一批都重新按 status 筛。
func (r *PostgresRepository) ExpireDue(ctx context.Context, limit int, traceID string) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	now := time.Now().UTC()
	var expired int
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+membershipColumns+` FROM memberships
			WHERE status=$1 AND expire_at <= $2
			ORDER BY expire_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED`, model.MembershipStatusActive, now, limit)
		if err != nil {
			return mapPGError(err)
		}
		due := make([]*model.Membership, 0, limit)
		for rows.Next() {
			membership, err := scanMembership(rows)
			if err != nil {
				rows.Close()
				return mapPGError(err)
			}
			due = append(due, membership)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapPGError(err)
		}
		if len(due) == 0 {
			return nil
		}

		for _, membership := range due {
			fromStatus, fromExpire := membership.Status, membership.ExpireAt
			updated, err := scanMembership(tx.QueryRow(ctx, `UPDATE memberships
				SET status='expired', updated_at=NOW()
				WHERE id=$1 RETURNING `+membershipColumns, membership.ID))
			if err != nil {
				return mapPGError(err)
			}
			// 到期**不关自动续费开关**：开关是用户的意图（「我要续」），扣款失败是另一回事（一期
			// 没扣到由订阅那一侧记失败计数，连到阈值才停扣）。这里关掉它，等于系统替用户改主意。
			if err := insertChange(ctx, tx, model.Change{
				MembershipID: updated.ID,
				UserID:       updated.UserID,
				ChangeType:   model.ChangeExpire,
				FromStatus:   optionalText(fromStatus),
				ToStatus:     optionalText(updated.Status),
				FromExpireAt: &fromExpire,
				ToExpireAt:   &updated.ExpireAt,
				PlanID:       optionalText(updated.PlanID),
				// worker 而不是 system：这条是**定时扫描**扫出来的，与「事件推过来顺手做的」
				// 分开，排查时能一眼看出权益是到点没的，不是被谁改没的。
				OperatorType: model.OperatorWorker,
				Reason:       "expired",
				OccurredAt:   now,
			}); err != nil {
				return err
			}
			if err := appendOutbox(ctx, tx, dto.EventMembershipExpired, dto.EventVersion, traceID,
				mustJSON(membershipEvent(updated, "", now))); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// ============================================================
// 变更流水
// ============================================================

// insertChange 记一条变更流水。
//
// 只 INSERT：表上有只增触发器，UPDATE / DELETE 会被数据库直接拒绝。所以这里不提供「改一条
// 流水」的入口——不是没写，是写了也跑不动。
//
// 撞 membership_changes_order_unique（order_id 上、限开通与续期的部分唯一索引）时翻成
// ErrDuplicateChange，由 service 判定为「这件事已经做过了」而不是故障（见那个错误值的说明）。
//
// **这个约束落在 insertChange 上，落在最后一步**：重投一条 order.paid 时，前面的续期 UPDATE
// 已经改过 memberships 了，是这一条 INSERT 撞上索引、让整个事务回滚，才没有把有效期叠两次。
func insertChange(ctx context.Context, tx pgx.Tx, c model.Change) error {
	_, err := tx.Exec(ctx, `INSERT INTO membership_changes
		(membership_id, user_id, change_type, from_status, to_status, from_expire_at, to_expire_at,
		 plan_id, order_id, operator_type, operator_id, reason, remark, metadata, request_id, occurred_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		c.MembershipID, c.UserID, c.ChangeType, c.FromStatus, c.ToStatus,
		c.FromExpireAt, c.ToExpireAt, c.PlanID, c.OrderID,
		c.OperatorType, c.OperatorID, trimOrEmpty(c.Reason), trimOrEmpty(c.Remark),
		jsonOrEmpty(c.Metadata), trimOrEmpty(c.RequestID), utc(c.OccurredAt))
	return mapPGError(err)
}

// ListChanges 取一条会员的变更时间线，倒序。
//
// 不分页：它是详情页的一部分，跟着一条会员走。一条会员的变更次数以年计（开通 + 每月一次
// 续费 + 偶尔的开关），上百分页只会在界面上多出一个永远点不到的「下一页」。
func (r *PostgresRepository) ListChanges(ctx context.Context, membershipID string) ([]*model.Change, error) {
	rows, err := r.pool.Query(ctx, `SELECT
		id, membership_id, user_id, change_type, from_status, to_status, from_expire_at, to_expire_at,
		plan_id, order_id, operator_type, operator_id, reason, remark, metadata, request_id,
		occurred_at, created_at
		FROM membership_changes WHERE membership_id=$1 ORDER BY occurred_at DESC, id`, membershipID)
	if err != nil {
		return nil, mapPGError(err)
	}
	defer rows.Close()

	changes := make([]*model.Change, 0, 16)
	for rows.Next() {
		var c model.Change
		if err := rows.Scan(
			&c.ID, &c.MembershipID, &c.UserID, &c.ChangeType, &c.FromStatus, &c.ToStatus,
			&c.FromExpireAt, &c.ToExpireAt, &c.PlanID, &c.OrderID, &c.OperatorType, &c.OperatorID,
			&c.Reason, &c.Remark, &c.Metadata, &c.RequestID, &c.OccurredAt, &c.CreatedAt,
		); err != nil {
			return nil, mapPGError(err)
		}
		changes = append(changes, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError(err)
	}
	return changes, nil
}

// ============================================================
// 事件
// ============================================================

// emitsEvent 说明一种变更要不要对外发事件。
//
// 它单独一个函数是因为**发不发**与**发哪一种**是两个问题：eventTypeFor 在发不出的类型上
// panic（那是配置漏了，是 bug），而这里 false 是完全正常的（开关自动续费不发事件）。
// 合成一个函数就得让「不发的类型」也在 switch 里有一个分支，那种分支迟早会被人填成一个
// 看起来合理的返回值。
func emitsEvent(changeType string) bool {
	switch changeType {
	case model.ChangeActivate, model.ChangeRenew, model.ChangeExpire,
		model.ChangeRevoke, model.ChangeFreeze:
		return true
	default:
		return false
	}
}

// eventTypeFor 把变更类型映射成事件类型。
//
// 只有四种变更发事件（开通 / 续费 / 到期 / 撤销），其余的人工动作**不发**：冻结与开关自动续费
// 是会员自己的属性变化，下游要判会员价只需读 expire_at 与 status（那条 gRPC），不需要为每一次
// 开关都收一条消息。发全了只会让消费者为一件不影响它的事写分支。
func eventTypeFor(changeType string) string {
	switch changeType {
	case model.ChangeActivate:
		return dto.EventMembershipActivated
	case model.ChangeRenew:
		return dto.EventMembershipRenewed
	case model.ChangeExpire:
		return dto.EventMembershipExpired
	case model.ChangeRevoke, model.ChangeFreeze:
		return dto.EventMembershipRevoked
	default:
		// 调用方只在这几种上调用。走到这里说明新加了一种变更类型却忘了配事件——返回空串会
		// 让 outbox 落一条没有路由键的记录（永远发不出去，也永远没人发现）。
		panic("membership: no event type mapped for change " + changeType)
	}
}

// membershipEvent 把当前会员的样子翻成事件体。
//
// 四类事件共用一个形状（见 dto 的说明），所以这里只认会员、不认事件类型。
func membershipEvent(m *model.Membership, orderID string, occurredAt time.Time) dto.MembershipChangedEvent {
	event := dto.MembershipChangedEvent{
		MembershipID:    m.ID,
		UserID:          m.UserID,
		PlanCode:        m.PlanCode,
		PlanName:        m.PlanName,
		MemberPriceMode: m.MemberPriceMode,
		Status:          m.Status,
		ExpireAt:        m.ExpireAt,
		OrderID:         orderID,
		OccurredAt:      occurredAt,
	}
	// 券配置来自**成交快照**，原样带上：下游据此发券，不回头查本服务的套餐（套餐随时会改）。
	if m.MemberPriceCouponTemplateID != nil {
		event.MemberPriceCouponTemplateID = *m.MemberPriceCouponTemplateID
	}
	if m.MemberPriceCouponsPerPeriod != nil {
		event.MemberPriceCouponsPerPeriod = *m.MemberPriceCouponsPerPeriod
	}
	return event
}
