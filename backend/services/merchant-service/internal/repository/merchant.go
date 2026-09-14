package repository

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// MerchantRepository 商户主体数据访问接口
type MerchantRepository interface {
	// FindPage 返回一页商户；总数走 Count。两个方法共用 merchantWhere 拼条件，
	// 保证「翻到第 2 页」和「告诉前端一共几页」用的是同一组筛选。
	FindPage(ctx context.Context, name, status string, limit, offset int) ([]*model.Merchant, error)
	Count(ctx context.Context, name, status string) (int64, error)
	FindByID(ctx context.Context, id string) (*model.Merchant, error)
	Create(ctx context.Context, m *model.Merchant) error
	Update(ctx context.Context, m *model.Merchant) error
	UpdateStatus(ctx context.Context, id, status string) error
	Delete(ctx context.Context, id string) error
}

// merchantSnapshot 是写入审计 before_data / after_data 的形状。
// 商户名称与状态可用性都属于经营主体的关键属性，联系人一并留痕。
type merchantSnapshot struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	ContactName  string `json:"contact_name,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
	ContactEmail string `json:"contact_email,omitempty"`
}

// selectMerchantSnapshot 读锁定行并组装快照。前后快照都走它：变更后在同一事务里
// 重读，记下的就是库里真实的最终值，而不是「按入参推断应该写成什么」。
func selectMerchantSnapshot(ctx context.Context, tx pgx.Tx, id string) (merchantSnapshot, error) {
	const q = `
		SELECT name, status, COALESCE(contact_name, ''), COALESCE(contact_phone, ''), COALESCE(contact_email, '')
		FROM merchants WHERE id = $1 FOR UPDATE`
	var s merchantSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(&s.Name, &s.Status, &s.ContactName, &s.ContactPhone, &s.ContactEmail)
	return s, err
}

type pgMerchantRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewMerchantRepository(pool *pgxpool.Pool, recorder audit.Recorder) MerchantRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgMerchantRepo{pool: pool, audit: recorder}
}

// merchantColumns 统一 SELECT 列表，可空联系人列归一化为空字符串方便扫描
const merchantColumns = `id, name, status, COALESCE(contact_name, ''), COALESCE(contact_phone, ''), COALESCE(contact_email, ''), created_at, updated_at`

// merchantWhere name 模糊匹配、status 等值过滤，两者均可为空。
//
// 抽出来给 FindPage 和 Count 共用：两处各写一遍的话，哪天给列表加了筛选条件却
// 只改了其中一处，症状是「总数说有 30 条，翻到第 2 页却什么都没有」——不报错，
// 只是分页器上的页码点不动。
func merchantWhere(name, status string) (string, []any) {
	conds := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if name != "" {
		args = append(args, "%"+name+"%")
		conds = append(conds, "name ILIKE $"+strconv.Itoa(len(args)))
	}
	if status != "" {
		args = append(args, status)
		conds = append(conds, "status = $"+strconv.Itoa(len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	return ` WHERE ` + strings.Join(conds, " AND "), args
}

// FindPage 排序带上 id 作决胜位：created_at 相同的商户（批量导入时很常见）
// 单靠时间没有稳定次序，翻页会重复或漏行。
func (r *pgMerchantRepo) FindPage(ctx context.Context, name, status string, limit, offset int) ([]*model.Merchant, error) {
	where, args := merchantWhere(name, status)
	q := `SELECT ` + merchantColumns + ` FROM merchants` + where + ` ORDER BY created_at DESC, id LIMIT $` +
		strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	rows, err := r.pool.Query(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func (r *pgMerchantRepo) Count(ctx context.Context, name, status string) (int64, error) {
	where, args := merchantWhere(name, status)
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM merchants`+where, args...).Scan(&n)
	return n, err
}

func (r *pgMerchantRepo) FindByID(ctx context.Context, id string) (*model.Merchant, error) {
	q := `SELECT ` + merchantColumns + ` FROM merchants WHERE id = $1 LIMIT 1`
	return scanMerchant(r.pool.QueryRow(ctx, q, id))
}

func (r *pgMerchantRepo) Create(ctx context.Context, m *model.Merchant) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO merchants (id, name, status, contact_name, contact_phone, contact_email, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.Exec(ctx, q,
		m.ID, m.Name, m.Status,
		nullEmpty(m.ContactName), nullEmpty(m.ContactPhone), nullEmpty(m.ContactEmail),
		m.CreatedAt, m.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectMerchantSnapshot(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchants", Action: "create", Operation: "新增商户",
		TargetType: "merchant", TargetID: m.ID, TargetName: m.Name,
		MerchantID: m.ID,
		After:      audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update 只更新名称与联系人信息，状态流转走 UpdateStatus
func (r *pgMerchantRepo) Update(ctx context.Context, m *model.Merchant) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantSnapshot(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE merchants
		SET name = $2, contact_name = $3, contact_phone = $4, contact_email = $5, updated_at = $6
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q,
		m.ID, m.Name,
		nullEmpty(m.ContactName), nullEmpty(m.ContactPhone), nullEmpty(m.ContactEmail),
		m.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectMerchantSnapshot(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchants", Action: "update", Operation: "修改商户",
		TargetType: "merchant", TargetID: m.ID, TargetName: after.Name,
		MerchantID: m.ID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMerchantRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `UPDATE merchants SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after, err := selectMerchantSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchants", Action: "update_status", Operation: "修改商户状态",
		TargetType: "merchant", TargetID: id, TargetName: after.Name,
		MerchantID: id,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMerchantRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantSnapshot(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM merchants WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchants", Action: "delete", Operation: "删除商户",
		TargetType: "merchant", TargetID: id, TargetName: before.Name,
		MerchantID: id,
		Before:     audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanMerchant(row pgx.Row) (*model.Merchant, error) {
	m := &model.Merchant{}
	err := row.Scan(
		&m.ID, &m.Name, &m.Status,
		&m.ContactName, &m.ContactPhone, &m.ContactEmail,
		&m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// nullEmpty 空字符串在库中存 NULL
func nullEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
