package repository

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// planColumns 是 membership_plans 的列清单。
//
// 它是**唯一**一份：写路径读回刚写的那一行、后台列表、小程序套餐列表都用它。抄一份的后果是
// 「后台和 C 端看到的同一个套餐字段不一样」，而那只有在有人对着两个页面对数时才会被发现。
//
// benefits 用 COALESCE 兜底成 '[]'：这一列有 NOT NULL DEFAULT，本不该为空，但 JSON 列一旦
// 被直接改库写成 NULL，读路径会在解析处炸——而炸的是「所有套餐都列不出来」。
const planColumns = `id, code, name, description, COALESCE(benefits, '[]'::jsonb),
	price_cents, period, period_count, auto_renew, wechat_plan_id,
	member_price_mode, member_price_coupon_template_id, member_price_coupons_per_period,
	sort_order, status, legacy_id, created_by, created_at, updated_at`

// PlanParams 是新建与修改套餐要写进去的东西。
//
// 字段与 dto.PlanRequest 一一对应，但**分成两个类型**：dto 是 HTTP 契约（json tag 要与前端
// 逐字一致），这里是写库的输入。让 repository 依赖 dto 会把 HTTP 的形状变成数据层的形状，
// 改一个 json tag 就牵动 SQL。
//
// 没有 Code：编码不可修改（库上有触发器钉着），它只在 CreatePlan 时单独传。
type PlanParams struct {
	Name        string
	Description string
	// Benefits 是已经序列化好的 JSONB 数组。序列化在 service 里做——它要先把 []string 校一遍
	// （去空、限长），而那属于业务规则，不属于数据访问。
	Benefits                    []byte
	PriceCents                  int64
	Period                      string
	PeriodCount                 int32
	AutoRenew                   bool
	WechatPlanID                string
	MemberPriceMode             string
	MemberPriceCouponTemplateID *string
	MemberPriceCouponsPerPeriod *int32
	SortOrder                   int32
	// Status 只在新建时用；修改走后端的上下架接口，不进这里（见 dto.PlanRequest 的说明）。
	Status    string
	CreatedBy *string
}

// planSnapshot 是套餐写进审计的那几个字段。
//
// 单独一个结构体、不用 model.Plan：model 上挂着 db tag、没有 json tag，直接快照出来是一串
// 大写的列名，读日志的人对不上后台表单里的label。字段只挑**能被后台改动的**那些——id、
// created_at、legacy_id 记下来只是把一行日志撑长。
type planSnapshot struct {
	Name                        string          `json:"name"`
	Description                 string          `json:"description"`
	Benefits                    json.RawMessage `json:"benefits,omitempty"`
	PriceCents                  int64           `json:"priceCents"`
	Period                      string          `json:"period"`
	PeriodCount                 int32           `json:"periodCount"`
	AutoRenew                   bool            `json:"autoRenew"`
	WechatPlanID                string          `json:"wechatPlanId,omitempty"`
	MemberPriceMode             string          `json:"memberPriceMode"`
	MemberPriceCouponTemplateID *string         `json:"memberPriceCouponTemplateId,omitempty"`
	MemberPriceCouponsPerPeriod *int32          `json:"memberPriceCouponsPerPeriod,omitempty"`
	SortOrder                   int32           `json:"sortOrder"`
	Status                      string          `json:"status"`
}

// selectPlanSnapshot 在事务里锁住这一行并读出待审字段。
//
// FOR UPDATE 不是为了防并发改（UPDATE 自己会锁），而是为了让 before 与 after 之间不会有别人
// 插进来——否则日志上写着的「从 A 改成 B」可能是从 A' 改成 B。
func selectPlanSnapshot(ctx context.Context, tx pgx.Tx, id string) (planSnapshot, error) {
	var s planSnapshot
	var benefits []byte
	err := tx.QueryRow(ctx, `SELECT name, description, benefits, price_cents, period, period_count,
		auto_renew, wechat_plan_id, member_price_mode, member_price_coupon_template_id,
		member_price_coupons_per_period, sort_order, status
		FROM membership_plans WHERE id=$1 FOR UPDATE`, id).
		Scan(&s.Name, &s.Description, &benefits, &s.PriceCents, &s.Period, &s.PeriodCount,
			&s.AutoRenew, &s.WechatPlanID, &s.MemberPriceMode, &s.MemberPriceCouponTemplateID,
			&s.MemberPriceCouponsPerPeriod, &s.SortOrder, &s.Status)
	if err != nil {
		if isNoRows(err) {
			return s, ErrPlanNotFound
		}
		return s, mapPGError(err)
	}
	s.Benefits = json.RawMessage(benefits)
	return s, nil
}

// CreatePlan 新建一个套餐并读回它。
//
// 写与审计在同一个事务里：套餐是会被卖出去的东西，运营改完价格、第二天有人来问「这个价格是谁
// 定的、什么时候定的」时，只有审计这一条线索（membership_plans 上只有一个 updated_at，没有
// 谁改的）。事务里落不下审计就整件事回滚——宁可这次新建失败，也不要一条无法追溯的套餐。
//
// ActorID 留给 platform/audit 从请求身份里取：handler 的 authenticate 中间件已经把登录态放进
// ctx，这里再传一遍只是把同一个值抄两遍。
func (r *PostgresRepository) CreatePlan(ctx context.Context, code string, p PlanParams) (*model.Plan, error) {
	var created *model.Plan
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		inserted, err := scanPlan(tx.QueryRow(ctx, `INSERT INTO membership_plans
			(code,name,description,benefits,price_cents,period,period_count,auto_renew,wechat_plan_id,
			 member_price_mode,member_price_coupon_template_id,member_price_coupons_per_period,
			 sort_order,status,created_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING `+planColumns,
			trimOrEmpty(code), trimOrEmpty(p.Name), trimOrEmpty(p.Description), p.Benefits,
			p.PriceCents, p.Period, p.PeriodCount, p.AutoRenew, trimOrEmpty(p.WechatPlanID),
			p.MemberPriceMode, p.MemberPriceCouponTemplateID, p.MemberPriceCouponsPerPeriod,
			p.SortOrder, p.Status, p.CreatedBy))
		if err != nil {
			return mapPGError(err)
		}
		after, err := selectPlanSnapshot(ctx, tx, inserted.ID)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_plans", Action: "create", Operation: "新增会员套餐",
			TargetType: "membership_plan", TargetID: inserted.ID, TargetName: after.Name,
			After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		created = inserted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdatePlan 改一个套餐并读回它。
//
// 不用「先查再改」的**旧值当前提**：改的每一列都由调用方整份给出，不拿读到的行做判断。这里的
// 那次读（selectPlanSnapshot）只服务于审计的 before——它在同一个事务里、带 FOR UPDATE，读到的
// 就是这次改动真正推翻的那一份。
//
// **它不改 status**：上下架走 SetPlanStatus。合成一条 SQL 的话，运营改一句描述就会把正在
// 售的套餐顺手存回 draft——而那一刻可能正有人在支付页上。
func (r *PostgresRepository) UpdatePlan(ctx context.Context, id string, p PlanParams) (*model.Plan, error) {
	var updated *model.Plan
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectPlanSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		result, err := scanPlan(tx.QueryRow(ctx, `UPDATE membership_plans SET
			name=$2, description=$3, benefits=$4, price_cents=$5, period=$6, period_count=$7,
			auto_renew=$8, wechat_plan_id=$9, member_price_mode=$10,
			member_price_coupon_template_id=$11, member_price_coupons_per_period=$12,
			sort_order=$13, updated_at=NOW()
			WHERE id=$1
			RETURNING `+planColumns,
			id, trimOrEmpty(p.Name), trimOrEmpty(p.Description), p.Benefits,
			p.PriceCents, p.Period, p.PeriodCount, p.AutoRenew, trimOrEmpty(p.WechatPlanID),
			p.MemberPriceMode, p.MemberPriceCouponTemplateID, p.MemberPriceCouponsPerPeriod,
			p.SortOrder))
		if err != nil {
			if isNoRows(err) {
				return ErrPlanNotFound
			}
			return mapPGError(err)
		}
		after, err := selectPlanSnapshot(ctx, tx, result.ID)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_plans", Action: "update", Operation: "修改会员套餐",
			TargetType: "membership_plan", TargetID: result.ID, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		updated = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// SetPlanStatus 把套餐切到 draft / active / disabled 并读回它。
//
// 单独一条 SQL 而不是复用 UpdatePlan 的那些列：它只动 status 与 updated_at，其余列一个都不
// 碰——一次上下架不该顺手把别人刚改的价格写回去。这是「先查再改」在**同一个后台**里的另一种
// 形态：两个运营同时开着一个套餐的编辑页，其中一个点了下架。
func (r *PostgresRepository) SetPlanStatus(ctx context.Context, id, status string) (*model.Plan, error) {
	var updated *model.Plan
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := selectPlanSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		result, err := scanPlan(tx.QueryRow(ctx, `UPDATE membership_plans SET
			status=$2, updated_at=NOW()
			WHERE id=$1
			RETURNING `+planColumns, id, status))
		if err != nil {
			if isNoRows(err) {
				return ErrPlanNotFound
			}
			return mapPGError(err)
		}
		// 快照整份、不只记 status 那两个值：这一条日志要能自证「上架的那一刻，卖的是这个价」。
		// 只记 status 的话，改价与上架挨在一起发生时，日志上只剩两个 active。
		after, err := selectPlanSnapshot(ctx, tx, result.ID)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "membership_plans", Action: "update_status", Operation: "修改会员套餐状态",
			TargetType: "membership_plan", TargetID: result.ID, TargetName: after.Name,
			Before: audit.Snapshot(before), After: audit.Snapshot(after),
		}); err != nil {
			return err
		}
		updated = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// GetPlan 按 ID 取一个套餐。
func (r *PostgresRepository) GetPlan(ctx context.Context, id string) (*model.Plan, error) {
	return r.getPlan(ctx, r.pool, id)
}

// GetPlanByCode 按编码取一个套餐。
//
// 下单与签约都靠编码对接（那是稳定的业务标识），所以这条读路径是外部调用的入口，不是内部
// 便利函数。
func (r *PostgresRepository) GetPlanByCode(ctx context.Context, code string) (*model.Plan, error) {
	return r.scanPlanFrom(ctx, r.pool, `SELECT `+planColumns+` FROM membership_plans WHERE code=$1`,
		trimOrEmpty(code))
}

// getPlan 让事务里的写路径也能读到刚写的那一行。querier 参数的理由见 postgres.go。
func (r *PostgresRepository) getPlan(ctx context.Context, q querier, id string) (*model.Plan, error) {
	return r.scanPlanFrom(ctx, q, `SELECT `+planColumns+` FROM membership_plans WHERE id=$1`, id)
}

func (r *PostgresRepository) scanPlanFrom(ctx context.Context, q querier, sql string, args ...any) (*model.Plan, error) {
	plan, err := scanPlan(q.QueryRow(ctx, sql, args...))
	if err != nil {
		if isNoRows(err) {
			return nil, ErrPlanNotFound
		}
		return nil, mapPGError(err)
	}
	return plan, nil
}

// ListPlans 是后台的套餐列表：按状态筛、按名字或编码模糊搜。
//
// 分页走 count + 分页查询两条 SQL，两条共用 filter 的拼法，否则「总数说有 30 条、只列出 12 条」
// 这种不一致会在加上筛选条件的那一刻出现。
func (r *PostgresRepository) ListPlans(ctx context.Context, q dto.PlanQuery) ([]*model.Plan, int, error) {
	const from = ` FROM membership_plans`
	where := planFilter(q)

	total, err := r.countRows(ctx, from, where)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	// 计数与取页共用同一份 where（同一份 args），差别只在这里多接一段 LIMIT/OFFSET。
	args, page := pageClause(where.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+planColumns+from+where.sql()+
		// sort_order 是运营定的展示顺序；code 是稳定的第二排序键，让「两条 sort_order 一样」
		// 时翻页不会漏条或重条。
		` ORDER BY sort_order, code`+page, args...)
	if err != nil {
		return nil, 0, mapPGError(err)
	}
	defer rows.Close()

	plans := make([]*model.Plan, 0, q.PageSize)
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, 0, mapPGError(err)
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, mapPGError(err)
	}
	return plans, total, nil
}

// ListActivePlans 是小程序的套餐列表：只有 active 的、没有分页。
//
// 不分页不是偷懒：卖的东西就是那么几款（原型就两个），而 C 端的套餐列表**必须完整**——少一个
// 用户就买不到那一款，而他在界面上看不出少了什么。真到了需要分页的那天，这个函数的签名会先
// 变得不合适，那正是要重新想的时候。
func (r *PostgresRepository) ListActivePlans(ctx context.Context) ([]*model.Plan, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+planColumns+` FROM membership_plans
		WHERE status=$1 ORDER BY sort_order, code`, model.PlanStatusActive)
	if err != nil {
		return nil, mapPGError(err)
	}
	defer rows.Close()

	plans := make([]*model.Plan, 0, 4)
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, mapPGError(err)
		}
		plans = append(plans, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError(err)
	}
	return plans, nil
}

// planFilter 拼套餐列表的 WHERE 子句与实参。
func planFilter(q dto.PlanQuery) whereClause {
	var where whereClause
	if status := trimOrEmpty(q.Status); status != "" {
		where.add("status = $%d", status)
	}
	if keyword := trimOrEmpty(q.Keyword); keyword != "" {
		// 一个输入框搜两列（名字与编码）：运营手里那半截可能是中文名，也可能是 monthly_auto，
		// 让他先决定「按哪个搜」只会多一步。
		where.addColumns([]string{"name", "code"}, "ILIKE", "%"+keyword+"%")
	}
	return where
}

// scanPlan 把一行读成 model.Plan。列的顺序必须与 planColumns 逐字对应。
func scanPlan(row scanner) (*model.Plan, error) {
	var plan model.Plan
	if err := row.Scan(
		&plan.ID, &plan.Code, &plan.Name, &plan.Description, &plan.Benefits,
		&plan.PriceCents, &plan.Period, &plan.PeriodCount, &plan.AutoRenew, &plan.WechatPlanID,
		&plan.MemberPriceMode, &plan.MemberPriceCouponTemplateID, &plan.MemberPriceCouponsPerPeriod,
		&plan.SortOrder, &plan.Status, &plan.LegacyID, &plan.CreatedBy,
		&plan.CreatedAt, &plan.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &plan, nil
}
