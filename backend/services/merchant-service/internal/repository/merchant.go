package repository

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// MerchantRepository 商户主体数据访问接口
type MerchantRepository interface {
	FindAll(ctx context.Context, name, status string) ([]*model.Merchant, error)
	FindByID(ctx context.Context, id string) (*model.Merchant, error)
	Create(ctx context.Context, m *model.Merchant) error
	Update(ctx context.Context, m *model.Merchant) error
	UpdateStatus(ctx context.Context, id, status string) error
	Delete(ctx context.Context, id string) error
	HasUsers(ctx context.Context, id string) (bool, error)
}

type pgMerchantRepo struct {
	pool *pgxpool.Pool
}

func NewMerchantRepository(pool *pgxpool.Pool) MerchantRepository {
	return &pgMerchantRepo{pool: pool}
}

// merchantColumns 统一 SELECT 列表，可空联系人列归一化为空字符串方便扫描
const merchantColumns = `id, name, status, COALESCE(contact_name, ''), COALESCE(contact_phone, ''), COALESCE(contact_email, ''), created_at, updated_at`

// FindAll name 模糊匹配、status 等值过滤，两者均可为空
func (r *pgMerchantRepo) FindAll(ctx context.Context, name, status string) ([]*model.Merchant, error) {
	q := `SELECT ` + merchantColumns + ` FROM merchants`
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
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, args...)
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

func (r *pgMerchantRepo) FindByID(ctx context.Context, id string) (*model.Merchant, error) {
	q := `SELECT ` + merchantColumns + ` FROM merchants WHERE id = $1 LIMIT 1`
	return scanMerchant(r.pool.QueryRow(ctx, q, id))
}

func (r *pgMerchantRepo) Create(ctx context.Context, m *model.Merchant) error {
	const q = `
		INSERT INTO merchants (id, name, status, contact_name, contact_phone, contact_email, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := r.pool.Exec(ctx, q,
		m.ID, m.Name, m.Status,
		nullEmpty(m.ContactName), nullEmpty(m.ContactPhone), nullEmpty(m.ContactEmail),
		m.CreatedAt, m.UpdatedAt,
	)
	return err
}

// Update 只更新名称与联系人信息，状态流转走 UpdateStatus
func (r *pgMerchantRepo) Update(ctx context.Context, m *model.Merchant) error {
	const q = `
		UPDATE merchants
		SET name = $2, contact_name = $3, contact_phone = $4, contact_email = $5, updated_at = $6
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q,
		m.ID, m.Name,
		nullEmpty(m.ContactName), nullEmpty(m.ContactPhone), nullEmpty(m.ContactEmail),
		m.UpdatedAt,
	)
	return err
}

func (r *pgMerchantRepo) UpdateStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE merchants SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, q, status, id)
	return err
}

func (r *pgMerchantRepo) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM merchants WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}

func (r *pgMerchantRepo) HasUsers(ctx context.Context, id string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM merchant_users WHERE merchant_id = $1)`
	var exists bool
	err := r.pool.QueryRow(ctx, q, id).Scan(&exists)
	return exists, err
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
