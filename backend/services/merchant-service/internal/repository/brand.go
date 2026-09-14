package repository

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// BrandRepository 品牌数据访问接口
type BrandRepository interface {
	// FindPage 返回一页品牌；总数走 Count，两者都经 buildFilter 拼条件，
	// 筛选语义只有一处定义。
	FindPage(ctx context.Context, f BrandFilter, limit, offset int) ([]*model.Brand, error)
	Count(ctx context.Context, f BrandFilter) (int64, error)
	FindByID(ctx context.Context, id string) (*model.Brand, error)
	Create(ctx context.Context, b *model.Brand) error
	Update(ctx context.Context, b *model.Brand) error
	UpdateStatus(ctx context.Context, id, status string) error
	SetAudit(ctx context.Context, id, auditStatus, remark, by string) error
	Delete(ctx context.Context, id string) error
	HasStores(ctx context.Context, id string) (bool, error)
	// FindNames resolves a batch of ids to display names in one query. An id with
	// no row is absent from the result rather than an error: callers show an
	// account without a scope name after its scope is deleted.
	FindNames(ctx context.Context, ids []string) (map[string]string, error)
}

// BrandFilter 列表过滤条件，零值表示不过滤
type BrandFilter struct {
	MerchantID  string
	Name        string
	Status      string
	AuditStatus string
}

// brandSnapshot 是写入审计 before_data / after_data 的形状。
// banner/photos 这类图片列表不进快照（体积大、内容不构成经营事实），
// 名称、描述、状态、审核结论、可见性与排序才是要能追溯的变更。
type brandSnapshot struct {
	MerchantID  string `json:"merchant_id"`
	Name        string `json:"name"`
	Logo        string `json:"logo,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	AuditStatus string `json:"audit_status,omitempty"`
	AuditRemark string `json:"audit_remark,omitempty"`
	Visible     bool   `json:"visible"`
	Sort        int    `json:"sort"`
}

func selectBrandSnapshot(ctx context.Context, tx pgx.Tx, id string) (brandSnapshot, error) {
	const q = `
		SELECT merchant_id, name, logo, description, status,
			audit_status, audit_remark, visible, sort
		FROM brands WHERE id = $1 FOR UPDATE`
	var s brandSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&s.MerchantID, &s.Name, &s.Logo, &s.Description, &s.Status,
		&s.AuditStatus, &s.AuditRemark, &s.Visible, &s.Sort,
	)
	return s, err
}

type pgBrandRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewBrandRepository(pool *pgxpool.Pool, recorder audit.Recorder) BrandRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgBrandRepo{pool: pool, audit: recorder}
}

// brandColumns 统一 SELECT 列表；merchant_name 为联表计算列，仅展示用
const brandColumns = `b.id, b.merchant_id, b.name, b.logo, b.banner, b.description,
	b.status, b.audit_status, b.audit_remark, b.audit_at, b.audit_by,
	b.remark, b.visible, b.sort, b.created_by, b.created_at, b.updated_at,
	COALESCE(m.name, '')`

// FindPage 排序末尾补 b.id：b.sort 大量重复、b.created_at 也可能撞上，
// 少了决胜位翻页就会重复或漏行。
func (r *pgBrandRepo) FindPage(ctx context.Context, f BrandFilter, limit, offset int) ([]*model.Brand, error) {
	conds, args := buildFilter(f.MerchantID, f.Name, f.Status, f.AuditStatus, "b.")
	q := `SELECT ` + brandColumns + `
		FROM brands b LEFT JOIN merchants m ON m.id = b.merchant_id` + whereClause(conds) +
		` ORDER BY b.sort, b.created_at DESC, b.id LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	rows, err := r.pool.Query(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.Brand
	for rows.Next() {
		b, err := scanBrand(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, b)
	}
	return list, rows.Err()
}

// Count 不 JOIN merchants：筛选条件全部落在 b.* 上，联表对计数没有影响，
// 白搭一次连接。将来若加了按商户名搜索，这里必须把 LEFT JOIN 补回来。
func (r *pgBrandRepo) Count(ctx context.Context, f BrandFilter) (int64, error) {
	conds, args := buildFilter(f.MerchantID, f.Name, f.Status, f.AuditStatus, "b.")
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM brands b`+whereClause(conds), args...).Scan(&n)
	return n, err
}

func (r *pgBrandRepo) FindByID(ctx context.Context, id string) (*model.Brand, error) {
	q := `SELECT ` + brandColumns + `
		FROM brands b LEFT JOIN merchants m ON m.id = b.merchant_id
		WHERE b.id = $1 LIMIT 1`
	return scanBrand(r.pool.QueryRow(ctx, q, id))
}

func (r *pgBrandRepo) FindNames(ctx context.Context, ids []string) (map[string]string, error) {
	return findNames(ctx, r.pool, "brands", ids)
}

func (r *pgBrandRepo) Create(ctx context.Context, b *model.Brand) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO brands (id, merchant_id, name, logo, banner, description,
			status, audit_status, audit_remark, audit_by, remark, visible, sort, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`
	if _, err := tx.Exec(ctx, q,
		b.ID, b.MerchantID, b.Name, b.Logo, b.Banner, b.Description,
		b.Status, b.AuditStatus, b.AuditRemark, b.AuditBy, b.Remark, b.Visible, b.Sort, b.CreatedBy,
		b.CreatedAt, b.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectBrandSnapshot(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "brands", Action: "create", Operation: "新增品牌",
		TargetType: "brand", TargetID: b.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		After:      audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update 平台直接编辑，不碰状态与审核字段
func (r *pgBrandRepo) Update(ctx context.Context, b *model.Brand) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectBrandSnapshot(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE brands
		SET name = $2, logo = $3, banner = $4, description = $5,
			remark = $6, visible = $7, sort = $8, updated_at = $9
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q,
		b.ID, b.Name, b.Logo, b.Banner, b.Description,
		b.Remark, b.Visible, b.Sort, b.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectBrandSnapshot(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "brands", Action: "update", Operation: "修改品牌",
		TargetType: "brand", TargetID: b.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgBrandRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectBrandSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `UPDATE brands SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after, err := selectBrandSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "brands", Action: "update_status", Operation: "修改品牌状态",
		TargetType: "brand", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgBrandRepo) SetAudit(ctx context.Context, id, auditStatus, remark, by string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectBrandSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `
		UPDATE brands
		SET audit_status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW(), updated_at = NOW()
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id, auditStatus, remark, by); err != nil {
		return err
	}
	after, err := selectBrandSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "brands", Action: "audit", Operation: "审核品牌",
		TargetType: "brand", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgBrandRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectBrandSnapshot(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM brands WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "brands", Action: "delete", Operation: "删除品牌",
		TargetType: "brand", TargetID: id, TargetName: before.Name,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgBrandRepo) HasStores(ctx context.Context, id string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM stores WHERE brand_id = $1)`
	var exists bool
	err := r.pool.QueryRow(ctx, q, id).Scan(&exists)
	return exists, err
}

func scanBrand(row pgx.Row) (*model.Brand, error) {
	b := &model.Brand{}
	err := row.Scan(
		&b.ID, &b.MerchantID, &b.Name, &b.Logo, &b.Banner, &b.Description,
		&b.Status, &b.AuditStatus, &b.AuditRemark, &b.AuditAt, &b.AuditBy,
		&b.Remark, &b.Visible, &b.Sort, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt,
		&b.MerchantName,
	)
	if err != nil {
		return nil, err
	}
	return b, nil
}
