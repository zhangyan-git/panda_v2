package repository

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// BrandRepository 品牌数据访问接口
type BrandRepository interface {
	FindAll(ctx context.Context, f BrandFilter) ([]*model.Brand, error)
	FindByID(ctx context.Context, id string) (*model.Brand, error)
	Create(ctx context.Context, b *model.Brand) error
	Update(ctx context.Context, b *model.Brand) error
	UpdateStatus(ctx context.Context, id, status string) error
	SetAudit(ctx context.Context, id, auditStatus, remark, by string) error
	Delete(ctx context.Context, id string) error
	HasStores(ctx context.Context, id string) (bool, error)
}

// BrandFilter 列表过滤条件，零值表示不过滤
type BrandFilter struct {
	MerchantID  string
	Name        string
	Status      string
	AuditStatus string
}

type pgBrandRepo struct {
	pool *pgxpool.Pool
}

func NewBrandRepository(pool *pgxpool.Pool) BrandRepository {
	return &pgBrandRepo{pool: pool}
}

// brandColumns 统一 SELECT 列表；merchant_name 为联表计算列，仅展示用
const brandColumns = `b.id, b.merchant_id, b.name, b.logo, b.banner, b.description,
	b.status, b.audit_status, b.audit_remark, b.audit_at, b.audit_by,
	b.remark, b.visible, b.sort, b.created_by, b.created_at, b.updated_at,
	COALESCE(m.name, '')`

func (r *pgBrandRepo) FindAll(ctx context.Context, f BrandFilter) ([]*model.Brand, error) {
	q := `SELECT ` + brandColumns + `
		FROM brands b LEFT JOIN merchants m ON m.id = b.merchant_id`
	conds, args := buildFilter(f.MerchantID, f.Name, f.Status, f.AuditStatus, "b.")
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY b.sort, b.created_at DESC`
	rows, err := r.pool.Query(ctx, q, args...)
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

func (r *pgBrandRepo) FindByID(ctx context.Context, id string) (*model.Brand, error) {
	q := `SELECT ` + brandColumns + `
		FROM brands b LEFT JOIN merchants m ON m.id = b.merchant_id
		WHERE b.id = $1 LIMIT 1`
	return scanBrand(r.pool.QueryRow(ctx, q, id))
}

func (r *pgBrandRepo) Create(ctx context.Context, b *model.Brand) error {
	const q = `
		INSERT INTO brands (id, merchant_id, name, logo, banner, description,
			status, audit_status, audit_remark, audit_by, remark, visible, sort, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`
	_, err := r.pool.Exec(ctx, q,
		b.ID, b.MerchantID, b.Name, b.Logo, b.Banner, b.Description,
		b.Status, b.AuditStatus, b.AuditRemark, b.AuditBy, b.Remark, b.Visible, b.Sort, b.CreatedBy,
		b.CreatedAt, b.UpdatedAt,
	)
	return err
}

// Update 平台直接编辑，不碰状态与审核字段
func (r *pgBrandRepo) Update(ctx context.Context, b *model.Brand) error {
	const q = `
		UPDATE brands
		SET name = $2, logo = $3, banner = $4, description = $5,
			remark = $6, visible = $7, sort = $8, updated_at = $9
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q,
		b.ID, b.Name, b.Logo, b.Banner, b.Description,
		b.Remark, b.Visible, b.Sort, b.UpdatedAt,
	)
	return err
}

func (r *pgBrandRepo) UpdateStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE brands SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, q, status, id)
	return err
}

func (r *pgBrandRepo) SetAudit(ctx context.Context, id, auditStatus, remark, by string) error {
	const q = `
		UPDATE brands
		SET audit_status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW(), updated_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id, auditStatus, remark, by)
	return err
}

func (r *pgBrandRepo) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM brands WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
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

// StoreRepository 门店数据访问接口
type StoreRepository interface {
	FindAll(ctx context.Context, f StoreFilter) ([]*model.Store, error)
	FindByID(ctx context.Context, id string) (*model.Store, error)
	Create(ctx context.Context, s *model.Store) error
	Update(ctx context.Context, s *model.Store) error
	UpdateStatus(ctx context.Context, id, status string) error
	SetAudit(ctx context.Context, id, auditStatus, remark, by string) error
	Delete(ctx context.Context, id string) error
}

// StoreFilter 列表过滤条件，零值表示不过滤
type StoreFilter struct {
	MerchantID  string
	BrandID     string
	Name        string
	Status      string
	AuditStatus string
}

type pgStoreRepo struct {
	pool *pgxpool.Pool
}

func NewStoreRepository(pool *pgxpool.Pool) StoreRepository {
	return &pgStoreRepo{pool: pool}
}

// storeColumns 统一 SELECT 列表；merchant_name/brand_name 为联表计算列，仅展示用
const storeColumns = `s.id, s.merchant_id, s.brand_id, s.name, s.logo, s.photos,
	s.province, s.city, s.district, s.address, s.longitude, s.latitude,
	s.phone, s.contact_name, s.contact_phone, s.detail, s.business_hours,
	s.status, s.audit_status, s.audit_remark, s.audit_at, s.audit_by,
	s.remark, s.visible, s.created_by, s.created_at, s.updated_at,
	COALESCE(m.name, ''), COALESCE(br.name, '')`

func (r *pgStoreRepo) FindAll(ctx context.Context, f StoreFilter) ([]*model.Store, error) {
	q := `SELECT ` + storeColumns + `
		FROM stores s
		LEFT JOIN merchants m ON m.id = s.merchant_id
		LEFT JOIN brands br ON br.id = s.brand_id`
	conds, args := buildFilter(f.MerchantID, f.Name, f.Status, f.AuditStatus, "s.")
	if f.BrandID != "" {
		args = append(args, f.BrandID)
		conds = append(conds, "s.brand_id = $"+strconv.Itoa(len(args)))
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY s.created_at DESC`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.Store
	for rows.Next() {
		s, err := scanStore(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

func (r *pgStoreRepo) FindByID(ctx context.Context, id string) (*model.Store, error) {
	q := `SELECT ` + storeColumns + `
		FROM stores s
		LEFT JOIN merchants m ON m.id = s.merchant_id
		LEFT JOIN brands br ON br.id = s.brand_id
		WHERE s.id = $1 LIMIT 1`
	return scanStore(r.pool.QueryRow(ctx, q, id))
}

func (r *pgStoreRepo) Create(ctx context.Context, s *model.Store) error {
	const q = `
		INSERT INTO stores (id, merchant_id, brand_id, name, logo, photos,
			province, city, district, address, longitude, latitude,
			phone, contact_name, contact_phone, detail, business_hours,
			status, audit_status, audit_remark, audit_by, remark, visible, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)`
	_, err := r.pool.Exec(ctx, q,
		s.ID, s.MerchantID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.Status, s.AuditStatus, s.AuditRemark, s.AuditBy, s.Remark, s.Visible, s.CreatedBy,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

// Update 平台直接编辑，不碰状态与审核字段
func (r *pgStoreRepo) Update(ctx context.Context, s *model.Store) error {
	const q = `
		UPDATE stores
		SET brand_id = $2, name = $3, logo = $4, photos = $5,
			province = $6, city = $7, district = $8, address = $9, longitude = $10, latitude = $11,
			phone = $12, contact_name = $13, contact_phone = $14, detail = $15, business_hours = $16,
			remark = $17, visible = $18, updated_at = $19
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q,
		s.ID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.Remark, s.Visible, s.UpdatedAt,
	)
	return err
}

func (r *pgStoreRepo) UpdateStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE stores SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, q, status, id)
	return err
}

func (r *pgStoreRepo) SetAudit(ctx context.Context, id, auditStatus, remark, by string) error {
	const q = `
		UPDATE stores
		SET audit_status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW(), updated_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id, auditStatus, remark, by)
	return err
}

func (r *pgStoreRepo) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM stores WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}

func scanStore(row pgx.Row) (*model.Store, error) {
	s := &model.Store{}
	err := row.Scan(
		&s.ID, &s.MerchantID, &s.BrandID, &s.Name, &s.Logo, &s.Photos,
		&s.Province, &s.City, &s.District, &s.Address, &s.Longitude, &s.Latitude,
		&s.Phone, &s.ContactName, &s.ContactPhone, &s.Detail, &s.BusinessHours,
		&s.Status, &s.AuditStatus, &s.AuditRemark, &s.AuditAt, &s.AuditBy,
		&s.Remark, &s.Visible, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&s.MerchantName, &s.BrandName,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// buildFilter 商户/名称模糊/状态/审核状态 四个通用过滤条件；
// alias 为主表别名前缀（联表后 status 等列名会歧义，必须限定）
func buildFilter(merchantID, name, status, auditStatus, alias string) ([]string, []any) {
	conds := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if merchantID != "" {
		args = append(args, merchantID)
		conds = append(conds, alias+"merchant_id = $"+strconv.Itoa(len(args)))
	}
	if name != "" {
		args = append(args, "%"+name+"%")
		conds = append(conds, alias+"name ILIKE $"+strconv.Itoa(len(args)))
	}
	if status != "" {
		args = append(args, status)
		conds = append(conds, alias+"status = $"+strconv.Itoa(len(args)))
	}
	if auditStatus != "" {
		args = append(args, auditStatus)
		conds = append(conds, alias+"audit_status = $"+strconv.Itoa(len(args)))
	}
	return conds, args
}
