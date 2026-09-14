package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

type TemplateStats struct {
	Total    int64 `json:"total"`
	Claimed  int64 `json:"claimed"`
	Redeemed int64 `json:"redeemed"`
	Expired  int64 `json:"expired"`
}

type CouponTemplateRepository interface {
	ListTemplates(context.Context, dto.CouponTemplateQuery) ([]*model.CouponTemplate, int64, error)
	GetTemplate(context.Context, string) (*model.CouponTemplate, error)
	CreateTemplate(context.Context, *model.CouponTemplate) (*model.CouponTemplate, error)
	UpdateTemplate(context.Context, *model.CouponTemplate) (*model.CouponTemplate, error)
	DeleteTemplate(context.Context, string) error
	AuditTemplate(context.Context, string, string, string, string) (*model.CouponTemplate, error)
	SetTemplateStatus(context.Context, string, string) (*model.CouponTemplate, error)
	TemplateStats(context.Context, string) (*TemplateStats, error)
}

// templateListWhere 拼模板列表的筛选子句与绑定参数。抽成单独一个函数是为了能被
// 单测钉住：占位符编号和 args 顺序错位编译期看不出来，只会在 pgx 的 Bind 阶段炸，
// 而这个包的 template_query_test.go 已经在防这一类（见那里的说明）。
func templateListWhere(q dto.CouponTemplateQuery) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if q.Name != "" {
		add("name ILIKE '%%' || $%d || '%%'", q.Name)
	}
	if q.Status != "" {
		add("status=$%d", q.Status)
	}
	if q.AuditStatus != "" {
		add("audit_status=$%d", q.AuditStatus)
	}
	return strings.Join(where, " AND "), args
}

// ListTemplates 按 q 里的条件分页查模板。WHERE 的拼法照 ListBatches：count 与取数
// 共用同一段 whereSQL——两处分头拼是分页列表最经典的错法（total 是全表、items 是
// 筛过的，翻页器跟着一起错）。
func (r *postgresRepository) ListTemplates(ctx context.Context, q dto.CouponTemplateQuery) ([]*model.CouponTemplate, int64, error) {
	whereSQL, args := templateListWhere(q)

	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM coupon_templates WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	off := (q.Page - 1) * q.PageSize
	args = append(args, q.PageSize, off)
	rows, err := r.pool.Query(ctx, `SELECT `+templateColumns+` FROM coupon_templates WHERE `+whereSQL+
		fmt.Sprintf(` ORDER BY created_at DESC,id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items, err := scanTemplates(rows)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachScopes(ctx, items); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// attachScopes 把 coupon_template_scopes 的范围挂到已查出的模板上。
//
// 必须批量查：列表页一页最多 dto.MaxPageSize 行，逐行查就是那么多次往返。注意模板列表也要带
// 范围——后台编辑弹窗用的是列表行的数据，列表不带的话，编辑后保存（PUT 是全量
// 覆盖）会把范围静默清空。
func (r *postgresRepository) attachScopes(ctx context.Context, items []*model.CouponTemplate) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, t := range items {
		ids = append(ids, t.ID)
	}
	rows, err := r.pool.Query(ctx, `SELECT template_id::text,scope_type,scope_id::text FROM coupon_template_scopes WHERE template_id = ANY($1::uuid[]) ORDER BY scope_type,scope_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTemplate := map[string][]*model.CouponTemplateScope{}
	for rows.Next() {
		s := &model.CouponTemplateScope{}
		if err := rows.Scan(&s.TemplateID, &s.ScopeType, &s.ScopeID); err != nil {
			return err
		}
		byTemplate[s.TemplateID] = append(byTemplate[s.TemplateID], s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range items {
		if s := byTemplate[t.ID]; len(s) > 0 {
			t.Scopes = s
		}
	}
	return nil
}

// replaceTemplateScopes 全量替换某个模板的范围行，必须在调用方的事务里执行。
//
// 「全量替换」而不是增量 diff：PUT 的语义就是全量覆盖，传空数组等于清空范围
// （= 不限），与 coupon_templates 其余字段的行为一致。
func replaceTemplateScopes(ctx context.Context, tx pgx.Tx, templateID string, scopes []*model.CouponTemplateScope) error {
	if _, err := tx.Exec(ctx, `DELETE FROM coupon_template_scopes WHERE template_id=$1`, templateID); err != nil {
		return err
	}
	for _, s := range scopes {
		if _, err := tx.Exec(ctx, `INSERT INTO coupon_template_scopes(template_id,scope_type,scope_id) VALUES($1,$2,$3)`, templateID, s.ScopeType, s.ScopeID); err != nil {
			return err
		}
	}
	return nil
}

func scanTemplates(rows pgx.Rows) ([]*model.CouponTemplate, error) {
	var out []*model.CouponTemplate
	for rows.Next() {
		t := &model.CouponTemplate{}
		err := rows.Scan(&t.ID, &t.CouponTypeID, &t.MerchantID, &t.Name, &t.ShortTitle, &t.Description, &t.CoverImage, &t.UseRuleDescription, &t.FaceValue, &t.MinPurchaseAmount, &t.PurchasePrice, &t.TotalQuantity, &t.IssuedQuantity, &t.ReservedQuantity, &t.ValidityMode, &t.ValidFrom, &t.ValidTo, &t.ValidDays, &t.ClaimLimitMode, &t.ClaimPeriodUnit, &t.ClaimPeriodQuantity, &t.RedemptionType, &t.ExternalUseMethod, &t.AuditStatus, &t.AuditRemark, &t.AuditedAt, &t.AuditedBy, &t.Status, &t.IsHot, &t.IsRecommended, &t.SortOrder, &t.Visible, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const templateColumns = `id::text,coupon_type_id::text,merchant_id::text,name,short_title,description,cover_image,use_rule_description,face_value,min_purchase_amount,purchase_price,total_quantity,issued_quantity,reserved_quantity,validity_mode,valid_from,valid_to,valid_days,claim_limit_mode,claim_period_unit,claim_period_quantity,redemption_type,external_use_method,audit_status,audit_remark,audited_at,audited_by::text,status,is_hot,is_recommended,sort_order,visible,created_by::text,created_at,updated_at`

func (r *postgresRepository) GetTemplate(ctx context.Context, id string) (*model.CouponTemplate, error) {
	t := &model.CouponTemplate{}
	err := r.pool.QueryRow(ctx, `SELECT `+templateColumns+` FROM coupon_templates WHERE id=$1`, id).Scan(&t.ID, &t.CouponTypeID, &t.MerchantID, &t.Name, &t.ShortTitle, &t.Description, &t.CoverImage, &t.UseRuleDescription, &t.FaceValue, &t.MinPurchaseAmount, &t.PurchasePrice, &t.TotalQuantity, &t.IssuedQuantity, &t.ReservedQuantity, &t.ValidityMode, &t.ValidFrom, &t.ValidTo, &t.ValidDays, &t.ClaimLimitMode, &t.ClaimPeriodUnit, &t.ClaimPeriodQuantity, &t.RedemptionType, &t.ExternalUseMethod, &t.AuditStatus, &t.AuditRemark, &t.AuditedAt, &t.AuditedBy, &t.Status, &t.IsHot, &t.IsRecommended, &t.SortOrder, &t.Visible, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := r.attachScopes(ctx, []*model.CouponTemplate{t}); err != nil {
		return nil, err
	}
	return t, nil
}

func templateArgs(t *model.CouponTemplate) []any {
	return []any{t.CouponTypeID, t.MerchantID, t.Name, t.ShortTitle, t.Description, t.CoverImage, t.UseRuleDescription, t.FaceValue, t.MinPurchaseAmount, t.PurchasePrice, t.TotalQuantity, t.ValidityMode, t.ValidFrom, t.ValidTo, t.ValidDays, t.ClaimLimitMode, t.ClaimPeriodUnit, t.ClaimPeriodQuantity, t.RedemptionType, t.ExternalUseMethod, t.IsHot, t.IsRecommended, t.SortOrder, t.Visible, t.CreatedBy}
}

// templateInsertQuery 与 templateUpdateQuery 都独立成常量：字段顺序由
// templateArgs 统一决定，语句里的占位符必须与它一一对应，把语句提出来才能
// 让测试断言这条不变式（见 template_query_test.go）。
const templateInsertQuery = `INSERT INTO coupon_templates(coupon_type_id,merchant_id,name,short_title,description,cover_image,use_rule_description,face_value,min_purchase_amount,purchase_price,total_quantity,validity_mode,valid_from,valid_to,valid_days,claim_limit_mode,claim_period_unit,claim_period_quantity,redemption_type,external_use_method,is_hot,is_recommended,sort_order,visible,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25) RETURNING ` + templateColumns

func (r *postgresRepository) CreateTemplate(ctx context.Context, t *model.CouponTemplate) (*model.CouponTemplate, error) {
	// 模板行和范围行必须一起提交：分成两次写会出现「模板建好了但范围没写上」
	// （PUT 全量覆盖，下次保存看着就像范围丢了）这种半截状态。
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := scanTemplateRow(tx.QueryRow(ctx, templateInsertQuery, templateArgs(t)...), t)
	if err != nil {
		return nil, err
	}
	if err := replaceTemplateScopes(ctx, tx, out.ID, out.Scopes); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
func scanTemplateRow(row pgx.Row, t *model.CouponTemplate) (*model.CouponTemplate, error) {
	err := row.Scan(&t.ID, &t.CouponTypeID, &t.MerchantID, &t.Name, &t.ShortTitle, &t.Description, &t.CoverImage, &t.UseRuleDescription, &t.FaceValue, &t.MinPurchaseAmount, &t.PurchasePrice, &t.TotalQuantity, &t.IssuedQuantity, &t.ReservedQuantity, &t.ValidityMode, &t.ValidFrom, &t.ValidTo, &t.ValidDays, &t.ClaimLimitMode, &t.ClaimPeriodUnit, &t.ClaimPeriodQuantity, &t.RedemptionType, &t.ExternalUseMethod, &t.AuditStatus, &t.AuditRemark, &t.AuditedAt, &t.AuditedBy, &t.Status, &t.IsHot, &t.IsRecommended, &t.SortOrder, &t.Visible, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// templateUpdateQuery 单独提出来，是为了让「占位符数量 == 绑定值数量」这条不变式
// 可以被测试直接断言。SET 里没有 created_by，所以它比 templateArgs 少一个参数；
// 多绑一个 pgx 只会在运行时抛 bind 错误，编译期完全看不出来。
const templateUpdateQuery = `UPDATE coupon_templates SET coupon_type_id=$2,merchant_id=$3,name=$4,short_title=$5,description=$6,cover_image=$7,use_rule_description=$8,face_value=$9,min_purchase_amount=$10,purchase_price=$11,total_quantity=$12,validity_mode=$13,valid_from=$14,valid_to=$15,valid_days=$16,claim_limit_mode=$17,claim_period_unit=$18,claim_period_quantity=$19,redemption_type=$20,external_use_method=$21,is_hot=$22,is_recommended=$23,sort_order=$24,visible=$25,updated_at=NOW() WHERE id=$1 RETURNING ` + templateColumns

func (r *postgresRepository) UpdateTemplate(ctx context.Context, t *model.CouponTemplate) (*model.CouponTemplate, error) {
	// UPDATE 不修改 created_by，但字段顺序要复用 templateArgs：它的最后一项正是
	// created_by，这里必须丢掉。
	a := templateArgs(t)
	a = append([]any{t.ID}, a[:len(a)-1]...)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	out, err := scanTemplateRow(tx.QueryRow(ctx, templateUpdateQuery, a...), &model.CouponTemplate{})
	if err != nil {
		return nil, err
	}
	// 全量替换：请求里没带的品牌/门店会被删掉（= 不限）。
	if err := replaceTemplateScopes(ctx, tx, out.ID, t.Scopes); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// scanTemplateRow 扫的是 coupon_templates 一行，不含范围；这里补上本次写进去的，
	// 让 PUT 的响应与随后的 GET 一致。
	out.Scopes = t.Scopes
	return out, nil
}
func (r *postgresRepository) DeleteTemplate(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM coupon_templates WHERE id=$1 AND issued_quantity=0`, id)
	return err
}
func (r *postgresRepository) AuditTemplate(ctx context.Context, id, status, remark, actor string) (*model.CouponTemplate, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	t := &model.CouponTemplate{}
	_, err = scanTemplateRow(tx.QueryRow(ctx, `UPDATE coupon_templates SET audit_status=$2,audit_remark=$3,audited_at=NOW(),audited_by=$4,status=CASE WHEN $2 <> 'approved' AND status='active' THEN 'disabled' ELSE status END,updated_at=NOW() WHERE id=$1 RETURNING `+templateColumns, id, status, remark, actor), t)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO coupon_state_transitions(aggregate_type,aggregate_id,to_status,reason,actor_id) VALUES('template',$1,$2,$3,$4)`, id, status, remark, actor)
	if err != nil {
		return nil, err
	}
	err = tx.Commit(ctx)
	return t, err
}
func (r *postgresRepository) SetTemplateStatus(ctx context.Context, id, status string) (*model.CouponTemplate, error) {
	t := &model.CouponTemplate{}
	_, err := scanTemplateRow(r.pool.QueryRow(ctx, `UPDATE coupon_templates SET status=$2,updated_at=NOW() WHERE id=$1 AND ($2 <> 'active' OR audit_status='approved') RETURNING `+templateColumns, id, status), t)
	return t, err
}
func (r *postgresRepository) TemplateStats(ctx context.Context, id string) (*TemplateStats, error) {
	s := &TemplateStats{}
	err := r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='claimed'),count(*) FILTER(WHERE status='redeemed'),count(*) FILTER(WHERE status='expired') FROM user_coupons WHERE template_id=$1`, id).Scan(&s.Total, &s.Claimed, &s.Redeemed, &s.Expired)
	return s, err
}
