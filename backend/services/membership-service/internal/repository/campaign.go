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

// campaignColumns 是 membership_campaigns 的列清单。与本包其它实体同一条规矩：全局唯一一份。
//
// 全是裸列名（没有表达式），所以列表查询里 prefixColumns(campaignColumns, "c") 能整份加别名——join 上
// membership_plans 之后，name / status / created_at 这些名字两张表都有，不加别名就是一句
// ambiguous column（42702，一条 500）。
const campaignColumns = `id, name, scene, store_id, plan_id, gift_days, start_at, end_at,
	status, coupon_template_id, coupon_count, qr_code_url, qr_code_generated_at,
	created_by, updated_by, created_at, updated_at`

// claimColumns 是 membership_campaign_claims 的列清单。
const claimColumns = `id, campaign_id, user_id, store_id, plan_id, gift_days,
	coupon_template_id, coupon_count, membership_id, membership_expire_at, created_at`

// CampaignRow 是后台列表要的一行：活动本身 + 它送的套餐名。
//
// 名字来自 join（membership_plans 在**同一个库**里，不跨库）。与 SubscriptionRow 同一条写法，
// 理由也相同：不把 plan_name 塞进 model.Campaign——那个结构体逐一对应表上的列，一行就是一行。
type CampaignRow struct {
	*model.Campaign
	PlanName string
}

// CampaignParams 是新建与修改活动要写进去的东西。
//
// 与 dto.CampaignRequest 一一对应但分成两个类型：dto 是 HTTP 契约，这里是写库的输入。
type CampaignParams struct {
	Name     string
	Scene    string
	StoreID  string
	PlanID   string
	GiftDays int32
	StartAt  time.Time
	EndAt    time.Time
	// CouponTemplateID / CouponCount 是每次领取送的券，两列同生共死（库上那条 CHECK）。
	// 空 = 这场活动只送会员天数。
	CouponTemplateID *string
	CouponCount      *int32
	// Status 只在新建时用；启停走 SetCampaignStatus，不进这里（与套餐同一条分工：
	// 合在一起的话，运营改一句活动名就会把正开着的活动顺手存回 draft）。
	Status string
}

// campaignSnapshot 是活动写进审计的那几个字段。
//
// 与 planSnapshot 同一条理由：单独一个结构体、不用 model.Campaign，字段只挑能被后台改动的
// 那些。grants 里带上 gift_days 与 store_id，因为这两列正是「这次领取送出去的是什么」。
type campaignSnapshot struct {
	Name     string    `json:"name"`
	Scene    string    `json:"scene"`
	StoreID  string    `json:"storeId"`
	PlanID   string    `json:"planId"`
	GiftDays int32     `json:"giftDays"`
	StartAt  time.Time `json:"startAt"`
	EndAt    time.Time `json:"endAt"`
	Status   string    `json:"status"`
	// 券那两列进审计：审计要答的是「这次改动把活动变成了什么样」，而「原来送券、现在不送了」
	// 正是最该看见的那种改动。空值序列化成 null，与库上的 NULL 一致。
	CouponTemplateID *string `json:"couponTemplateId"`
	CouponCount      *int32  `json:"couponCount"`
}

// selectCampaignSnapshot 在事务里锁住这一行并读出待审字段。
func selectCampaignSnapshot(ctx context.Context, tx pgx.Tx, id string) (campaignSnapshot, error) {
	var s campaignSnapshot
	err := tx.QueryRow(ctx, `SELECT name, scene, store_id, plan_id, gift_days, start_at, end_at, status,
		coupon_template_id, coupon_count
		FROM membership_campaigns WHERE id=$1 FOR UPDATE`, id).
		Scan(&s.Name, &s.Scene, &s.StoreID, &s.PlanID, &s.GiftDays, &s.StartAt, &s.EndAt, &s.Status,
			&s.CouponTemplateID, &s.CouponCount)
	if err != nil {
		if isNoRows(err) {
			return s, ErrCampaignNotFound
		}
		return s, mapPGError(err)
	}
	return s, nil
}

// CreateCampaign 新建一场店铺码会员活动并读回它。
//
// 新活动一律是 draft（老后台也是这么建的）：一个还没配完的活动被扫到，用户领到的是半成品，
// 而页面上看不出哪里不对。要开扫得再调一次启停接口。
func (r *PostgresRepository) CreateCampaign(ctx context.Context, p CampaignParams, adminID string) (*CampaignRow, error) {
	var created *CampaignRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		inserted, err := scanCampaign(tx.QueryRow(ctx, `INSERT INTO membership_campaigns
			(name, scene, store_id, plan_id, gift_days, start_at, end_at, status,
			 coupon_template_id, coupon_count, created_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+campaignColumns,
			trimOrEmpty(p.Name), trimOrEmpty(p.Scene), p.StoreID, p.PlanID, p.GiftDays,
			utc(p.StartAt), utc(p.EndAt), p.Status, p.CouponTemplateID, p.CouponCount,
			optionalID(adminID)))
		if err != nil {
			return mapPGError(err)
		}
		after, err := selectCampaignSnapshot(ctx, tx, inserted.ID)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_campaigns", Action: "create", Operation: "新增店铺码会员活动",
			TargetType: "membership_campaign", TargetID: inserted.ID, TargetName: after.Name,
			After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		// 读回的那一行用同一个事务（不是提交后再查一次）：审计里的 after 与这里回给调用方的
		// 必须是同一份东西，分两次读会多出一个「这两者之间被别人改过」的窗口。
		created, err = r.getCampaign(ctx, tx, inserted.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateCampaign 改一场活动并读回它。
//
// **它不改 status**：启停走 SetCampaignStatus。合成一条 SQL 的话，运营改一句活动名就会把正
// 开着的活动顺手存回 draft——而那一刻可能正有人拿着码在扫。
//
// # scene / store_id 在 enabled 时改不动（ErrCampaignLocked）
//
// 判据落在**这里**，不落在 service：service 要先读一次当前状态才判得了，而两次读之间隔着一个
// 「刚被别的人启用」的窗口——那一刻码可能已经发出去了。这里读到的 before 是 FOR UPDATE 锁住的
// 那一份，判据与写入是同一个瞬间的事，没有窗口。
//
// 为什么偏是这两列：scene 进了码，改它会让已经印出去、发出去的码静默指向别人；store_id 是
// 归属门店，改了等于把已经领过的人的归属追溯性地挪走。要改先停用，停用是个说得明白的动作。
func (r *PostgresRepository) UpdateCampaign(ctx context.Context, id string, p CampaignParams, adminID string) (*CampaignRow, error) {
	var updated *CampaignRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if before.Status == model.CampaignStatusEnabled &&
			(before.Scene != trimOrEmpty(p.Scene) || before.StoreID != p.StoreID) {
			return ErrCampaignLocked
		}
		if _, err := tx.Exec(ctx, `UPDATE membership_campaigns SET
			name=$2, scene=$3, store_id=$4, plan_id=$5, gift_days=$6, start_at=$7, end_at=$8,
			coupon_template_id=$9, coupon_count=$10, updated_by=$11, updated_at=NOW()
			WHERE id=$1`,
			id, trimOrEmpty(p.Name), trimOrEmpty(p.Scene), p.StoreID, p.PlanID, p.GiftDays,
			utc(p.StartAt), utc(p.EndAt), p.CouponTemplateID, p.CouponCount,
			optionalID(adminID)); err != nil {
			return mapPGError(err)
		}
		after, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_campaigns", Action: "update", Operation: "修改店铺码会员活动",
			TargetType: "membership_campaign", TargetID: id, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		updated, err = r.getCampaign(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// SetCampaignStatus 把活动切到 draft / enabled / disabled 并读回它。
//
// 单独一条 SQL 而不是复用 UpdateCampaign 的那些列：它只动 status 与 updated_by、updated_at，
// 其余列一个都不碰——一次启停不该顺手把别人刚改的天数写回去。与 SetPlanStatus 同一条分工。
func (r *PostgresRepository) SetCampaignStatus(ctx context.Context, id, status, adminID string) (*CampaignRow, error) {
	var updated *CampaignRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE membership_campaigns SET
			status=$2, updated_by=$3, updated_at=NOW() WHERE id=$1`,
			id, status, optionalID(adminID)); err != nil {
			return mapPGError(err)
		}
		after, err := selectCampaignSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_campaigns", Action: "update_status", Operation: "修改店铺码会员活动状态",
			TargetType: "membership_campaign", TargetID: id, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		updated, err = r.getCampaign(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// GetCampaign 按 ID 取一场活动（带套餐名）。
func (r *PostgresRepository) GetCampaign(ctx context.Context, id string) (*CampaignRow, error) {
	return r.getCampaign(ctx, r.pool, id)
}

func (r *PostgresRepository) getCampaign(ctx context.Context, q querier, id string) (*CampaignRow, error) {
	row, err := scanCampaignRow(q.QueryRow(ctx, `SELECT `+prefixColumns(campaignColumns, "c")+`, p.name
		FROM membership_campaigns c JOIN membership_plans p ON p.id = c.plan_id
		WHERE c.id=$1`, id))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrCampaignNotFound
		}
		return nil, mapPGError(err)
	}
	return row, nil
}

// GetCampaignByScene 按 scene 取一场活动。
//
// 扫码进来的请求手里只有 scene。这条读路径**不带套餐名**（join 是给人看的，这里只用来定位），
// 所以它回的是 model 本身，领取那条路要用的是它。
func (r *PostgresRepository) GetCampaignByScene(ctx context.Context, scene string) (*model.Campaign, error) {
	campaign, err := scanCampaign(r.pool.QueryRow(ctx,
		`SELECT `+campaignColumns+` FROM membership_campaigns WHERE scene=$1`, trimOrEmpty(scene)))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrCampaignNotFound
		}
		return nil, mapPGError(err)
	}
	return campaign, nil
}

// ListCampaigns 是后台的活动列表：按状态筛、按名字或 scene 模糊搜。
//
// 排序固定 created_at DESC（与订阅列表同一条）：这个页面看的是「最近加的那些」，而创建时间是
// 唯一一个所有筛选组合下都说得通的排序键。
func (r *PostgresRepository) ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*CampaignRow, int, error) {
	const from = ` FROM membership_campaigns c JOIN membership_plans p ON p.id = c.plan_id`
	where := campaignFilter(q)

	total, err := r.countRows(ctx, from, where)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	args, page := pageClause(where.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+prefixColumns(campaignColumns, "c")+`, p.name`+from+where.sql()+
		` ORDER BY c.created_at DESC, c.id`+page, args...)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	defer rows.Close()

	campaigns := make([]*CampaignRow, 0, q.PageSize)
	for rows.Next() {
		row, err := scanCampaignRow(rows)
		if err != nil {
			return nil, 0, mapPGError(err)
		}
		campaigns = append(campaigns, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapPGError(err)
	}
	return campaigns, total, nil
}

// campaignFilter 拼活动列表的 WHERE 子句与实参。
func campaignFilter(q dto.CampaignQuery) whereClause {
	var where whereClause
	if status := trimOrEmpty(q.Status); status != "" {
		where.add("c.status = $%d", status)
	}
	if keyword := trimOrEmpty(q.Keyword); keyword != "" {
		// 一个输入框搜两列：运营手里那半截可能是活动名，也可能是码上的 scene。
		where.addColumns([]string{"c.name", "c.scene"}, "ILIKE", "%"+keyword+"%")
	}
	return where
}

// ListCampaignClaims 是一场活动的领取记录，一页一次。
func (r *PostgresRepository) ListCampaignClaims(ctx context.Context, campaignID string, q dto.CampaignClaimQuery) ([]*model.CampaignClaim, int, error) {
	const from = ` FROM membership_campaign_claims`
	var where whereClause
	where.add("campaign_id = $%d", campaignID)

	total, err := r.countRows(ctx, from, where)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	args, page := pageClause(where.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+claimColumns+from+where.sql()+
		` ORDER BY created_at DESC, id`+page, args...)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	defer rows.Close()

	claims := make([]*model.CampaignClaim, 0, q.PageSize)
	for rows.Next() {
		claim, err := scanClaim(rows)
		if err != nil {
			return nil, 0, mapPGError(err)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapPGError(err)
	}
	return claims, total, nil
}

// ClaimParams 是一次扫码领取的全部输入。
type ClaimParams struct {
	// Scene 是小程序码带上来的那个值。它同时是定位活动与幂等的键——领取记录的唯一索引是
	// (campaign_id, user_id)，而 campaign_id 由它解出来。
	Scene  string
	UserID string
	// OccurredAt 是**用户扫码的那一刻**（调用方给，不是这里的 NOW()）：活动窗口、会员的
	// start_at、赠送天数的起点都该是它。
	OccurredAt time.Time
	TraceID    string
}

// ClaimOutcome 是一次领取的结果，含「这是不是一次重放」。
//
// 重放（Replayed）与首次领取的**结果完全相同**——同一条领取记录、同一个到期时刻。它多出来的
// 只是给客户端的一句话：这一次没有再发一次，是重复扫码。页面据此说「你已经领过了」而不是再弹
// 一次「领取成功」：两个都对，但只有一个是真的。
//
// 带上 CampaignName / PlanName 是为了让 service 不必为了拼回执再查一次——活动行本来就在事务里
// 读出来了（而且那一刻是锁着的），出了事务再查一遍可能读到的是刚被改过的值。
type ClaimOutcome struct {
	Claim        *model.CampaignClaim
	CampaignName string
	PlanName     string
	Replayed     bool
}

// ClaimCampaign 处理一次「扫码领会员」。
//
// # 一个事务里做完五件事
//
// 锁住活动行、核对这一刻能不能领、开通或续期这条会员、写领取记录、写变更流水与事件。
// 任何一步失败整体回滚——**领取记录不留失败行**：留着它会被唯一索引挡住，那个人就再也领不了
// 了，而失败的原因（并发、商户域抖动）本来是可重试的。
//
// # 为什么是「开通或续期」，而不是后台开通那种一律拒绝
//
// 后台开通（GrantMembership）遇到已有会员一律 409，因为那是一次人工补偿，「叠加」等于把开通
// 偷偷变成续期。领活动不是：老系统在这里就是**叠天数**（`expire_date.AddDate(0,0,giftDays)`），
// 而本域归属规则的第三条恰好只对这条链路开口——「会员到期之后重新开通，归属门店可变」的那个
// 例外，点名的两个渠道之一就是这个。所以这里走 renewMembership 的同一条分叉：还在有效期内
// 就叠加（归属不动），已经过期就从这一刻重新起算（归属改成本次活动门店）。判据一个字不用改。
//
// # auto_renew 恒为 false
//
// 与后台开通同一条理由：**这条链路没有任何签约**。扫码领会员不是签约——签约要走微信那一跳
// （用户在微信里点同意「每月自动扣款」），扫个门店码做不到这件事。置 true 会让界面显示「已开启
// 自动续费」却没有任何协议在扣款，而用户点那个开关想关掉它时**也关不掉任何东西**：关自动续费
// 在本域是「去解约」，这里没有约可解（见 service.SetAutoRenewByUser）。
//
// # 不记平台审计
//
// 审计（admin_operation_logs）记的是**后台人工操作**，而这是用户自己扫的码。它的痕迹在
// membership_changes（一行 user 操作人）与领取记录上，两者都只增不改。
func (r *PostgresRepository) ClaimCampaign(ctx context.Context, p ClaimParams) (*ClaimOutcome, error) {
	var outcome *ClaimOutcome
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		occurredAt := utc(p.OccurredAt)

		// FOR UPDATE 让同一场活动的并发领取排成一队。它同时挡住两件事：一个人连点两次时
		// 两边都读到「还没有领取记录」（于是都去发一次会员），以及「读到 enabled、发放时
		// 已被下架」的错位。领取是人点一次的动作，不是高频路径，排队的代价可以忽略。
		campaign, err := scanCampaign(tx.QueryRow(ctx,
			`SELECT `+campaignColumns+` FROM membership_campaigns WHERE scene=$1 FOR UPDATE`,
			trimOrEmpty(p.Scene)))
		if err != nil {
			if isNoRows(err) {
				return ErrCampaignNotFound
			}
			return mapPGError(err)
		}
		if !campaign.IsClaimable(occurredAt) {
			return ErrCampaignNotClaimable
		}

		// 套餐现查（不是活动建的时候抄一份）：这是「快照说了算」那条规矩在无订单路径上的
		// 同一个例外，见 GrantMembership 那段说明——取完立刻落进快照列，之后套餐再改都不追溯。
		//
		// 它排在「领过没有」之前，因为**重放的回执也要套餐名**（用户看的是「你领到了哪一款
		// 会员」）。套餐不可能缺席：活动上那条外键是 RESTRICT，套餐还在活动就删不掉。
		plan, err := scanPlan(tx.QueryRow(ctx,
			`SELECT `+planColumns+` FROM membership_plans WHERE id=$1`, campaign.PlanID))
		if err != nil {
			if isNoRows(err) {
				return ErrPlanNotFound
			}
			return mapPGError(err)
		}

		// 已经领过就回原来那条，什么都不写。第二次扫码与第一次必须给出同一个结果，否则
		// 「我到底领上没有」在客户端上是个说不清的问题。
		existing, err := claimForUser(ctx, tx, campaign.ID, p.UserID)
		if err != nil {
			return err
		}
		if existing != nil {
			outcome = &ClaimOutcome{
				Claim:        existing,
				CampaignName: campaign.Name,
				PlanName:     plan.Name,
				Replayed:     true,
			}
			return nil
		}
		snapshot := snapshotOfPlan(plan)
		// 送的是**天数**，不是套餐的 period/period_count：这是一次赠送。两处（开通与续期）
		// 共用同一个闭包，免得「新开通按 30 天、续期按 30 天但算法不同」这种漂移。
		extend := func(base time.Time) time.Time { return base.AddDate(0, 0, int(campaign.GiftDays)) }

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
				UserID:    p.UserID,
				Snapshot:  snapshot,
				StartAt:   occurredAt,
				ExpireAt:  extend(occurredAt),
				StoreID:   campaign.StoreID,
				AutoRenew: false, // 见上面那段：这条链路没有签约。
			})
			changeType = model.ChangeActivate
		} else {
			// 已撤销的会员不自动恢复：撤销是人工做的决定（风控、争议、退款），一次扫码不该把它
			// 推翻。与支付那条路同一个结论，复用同一个错误值。
			if current.Status == model.MembershipStatusRevoked {
				return ErrMembershipRevoked
			}
			fromStatus = current.Status
			expire := current.ExpireAt
			fromExpire = &expire
			membership, err = renewMembership(ctx, tx, current, renewParams{
				Snapshot: snapshot,
				StoreID:  campaign.StoreID,
				Extend:   extend,
			}, occurredAt)
			changeType = model.ChangeRenew
		}
		if err != nil {
			return err
		}

		claim, err := scanClaim(tx.QueryRow(ctx, `INSERT INTO membership_campaign_claims
			(campaign_id, user_id, store_id, plan_id, gift_days,
			 coupon_template_id, coupon_count, membership_id, membership_expire_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+claimColumns,
			campaign.ID, p.UserID, campaign.StoreID, campaign.PlanID, campaign.GiftDays,
			campaign.CouponTemplateID, campaign.CouponCount,
			membership.ID, utc(membership.ExpireAt)))
		if err != nil {
			return mapPGError(err)
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
			// 操作人是**用户本人**：这是他扫的码，不是系统定时跑的、也不是后台点的。
			OperatorType: model.OperatorUser,
			OperatorID:   optionalID(p.UserID),
			Reason:       "店铺码活动领取",
			Remark:       campaign.Name,
			OccurredAt:   occurredAt,
		}); err != nil {
			return err
		}

		if err := appendOutbox(ctx, tx, eventTypeFor(changeType), dto.EventVersion, p.TraceID,
			mustJSON(membershipEvent(membership, "", occurredAt))); err != nil {
			return err
		}

		// 第二条事件：这次领取**还承诺了券**，而券归 coupon-service 发（本服务一个字都不写券库
		// 的账）。这条事件是两笔账之间唯一的桥——没配券的活动也发，载荷里券那两个字段为空就是
		// 「只送会员天数」，下游看见空值什么都不做（见 dto.EventMembershipCampaignClaimed）。
		if err := appendOutbox(ctx, tx, dto.EventMembershipCampaignClaimed, dto.EventVersion, p.TraceID,
			mustJSON(campaignClaimedEvent(claim, plan.Name, occurredAt))); err != nil {
			return err
		}

		outcome = &ClaimOutcome{
			Claim:        claim,
			CampaignName: campaign.Name,
			PlanName:     plan.Name,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// campaignClaimedEvent 把一次领取翻成发出去的事件体。
//
// 券那两个字段**取自领取记录那份快照**（claim），不是活动当前的值：这一次承诺的是什么，就
// 告诉下游什么——活动在发放之后被改过，已经领过的那次也不该跟着变。
//
// 没配券就写空值，下游靠空值判「这场活动只送会员天数」。这不新开一条事件类型：那样下游要写
// 两个几乎一样的结构体，而它们迟早会漂移。
func campaignClaimedEvent(claim *model.CampaignClaim, planName string, occurredAt time.Time) dto.CampaignClaimedEvent {
	event := dto.CampaignClaimedEvent{
		ClaimID:    claim.ID,
		CampaignID: claim.CampaignID,
		UserID:     claim.UserID,
		StoreID:    claim.StoreID,
		PlanName:   planName,
		GiftDays:   claim.GiftDays,
		// 用调用方给的「用户扫码那一刻」，不是这一行落库的 NOW()：两者在同一个事务里、相差
		// 毫秒，但只有一个是对的（与会员那条事件同一条规矩）。
		OccurredAt: occurredAt,
	}
	if claim.CouponTemplateID != nil {
		event.CouponTemplateID = *claim.CouponTemplateID
	}
	if claim.CouponCount != nil {
		event.CouponCount = *claim.CouponCount
	}
	return event
}

// claimForUser 取这条领取记录；没有时回 (nil, nil)。
//
// 「没有」不是错误，所以不借 ErrCampaignNotFound 之类的错误值转述——那个值在这一层的含义是
// 「活动不存在」，用它表达「这个人还没领过」会让上面那段判断读起来完全相反。
func claimForUser(ctx context.Context, q querier, campaignID, userID string) (*model.CampaignClaim, error) {
	claim, err := scanClaim(q.QueryRow(ctx, `SELECT `+claimColumns+`
		FROM membership_campaign_claims WHERE campaign_id=$1 AND user_id=$2`, campaignID, userID))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, mapPGError(err)
	}
	return claim, nil
}

// snapshotOfPlan 把一份现查出来的套餐翻成交快照。
//
// 与后台开通那条路上逐字相同的一段（见 service.GrantMembership）：券那两个字段在 auto 模式下
// 是 NULL，快照里跟着留空——写库时由 MembershipSnapshot.couponColumns 按模式决定写不写。
func snapshotOfPlan(plan *model.Plan) MembershipSnapshot {
	snapshot := MembershipSnapshot{
		PlanID:          plan.ID,
		PlanCode:        plan.Code,
		PlanName:        plan.Name,
		MemberPriceMode: plan.MemberPriceMode,
		Period:          plan.Period,
		PeriodCount:     plan.PeriodCount,
		AutoRenew:       plan.AutoRenew,
	}
	if plan.MemberPriceCouponTemplateID != nil {
		snapshot.MemberPriceCouponTemplateID = *plan.MemberPriceCouponTemplateID
	}
	if plan.MemberPriceCouponsPerPeriod != nil {
		snapshot.MemberPriceCouponsPerPeriod = *plan.MemberPriceCouponsPerPeriod
	}
	return snapshot
}

// scanCampaign 把一行读成 model.Campaign。列的顺序必须与 campaignColumns 逐字对应。
func scanCampaign(row scanner) (*model.Campaign, error) {
	var campaign model.Campaign
	if err := row.Scan(
		&campaign.ID, &campaign.Name, &campaign.Scene, &campaign.StoreID, &campaign.PlanID,
		&campaign.GiftDays, &campaign.StartAt, &campaign.EndAt, &campaign.Status,
		&campaign.CouponTemplateID, &campaign.CouponCount,
		&campaign.QRCodeURL, &campaign.QRCodeGeneratedAt, &campaign.CreatedBy, &campaign.UpdatedBy,
		&campaign.CreatedAt, &campaign.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &campaign, nil
}

// scanCampaignRow 把一行读成「活动 + 套餐名」。名字排在 campaignColumns 之后。
func scanCampaignRow(row scanner) (*CampaignRow, error) {
	var (
		campaign model.Campaign
		planName string
	)
	if err := row.Scan(
		&campaign.ID, &campaign.Name, &campaign.Scene, &campaign.StoreID, &campaign.PlanID,
		&campaign.GiftDays, &campaign.StartAt, &campaign.EndAt, &campaign.Status,
		&campaign.CouponTemplateID, &campaign.CouponCount,
		&campaign.QRCodeURL, &campaign.QRCodeGeneratedAt, &campaign.CreatedBy, &campaign.UpdatedBy,
		&campaign.CreatedAt, &campaign.UpdatedAt,
		&planName,
	); err != nil {
		return nil, err
	}
	return &CampaignRow{Campaign: &campaign, PlanName: planName}, nil
}

// scanClaim 把一行读成 model.CampaignClaim。
func scanClaim(row scanner) (*model.CampaignClaim, error) {
	var claim model.CampaignClaim
	if err := row.Scan(
		&claim.ID, &claim.CampaignID, &claim.UserID, &claim.StoreID, &claim.PlanID,
		&claim.GiftDays, &claim.CouponTemplateID, &claim.CouponCount,
		&claim.MembershipID, &claim.MembershipExpireAt, &claim.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &claim, nil
}
