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

// StoreRepository 门店数据访问接口
type StoreRepository interface {
	FindAll(ctx context.Context, f StoreFilter) ([]*model.Store, error)
	FindByID(ctx context.Context, id string) (*model.Store, error)
	Create(ctx context.Context, s *model.Store) error
	Update(ctx context.Context, s *model.Store) error
	UpdateStatus(ctx context.Context, id, status string) error
	SetAudit(ctx context.Context, id, auditStatus, remark, by string) error
	Delete(ctx context.Context, id string) error
	// FindNames is the store-side counterpart of BrandRepository.FindNames.
	FindNames(ctx context.Context, ids []string) (map[string]string, error)
}

// StoreFilter 列表过滤条件，零值表示不过滤
type StoreFilter struct {
	MerchantID  string
	BrandID     string
	Name        string
	Status      string
	AuditStatus string
}

// storeSnapshot 理由同 brandSnapshot：photos 这类图片列表不进快照。
// 归属（merchant_id/brand_id）必须记——门店改挂品牌会直接改变它属于谁。
type storeSnapshot struct {
	MerchantID  string `json:"merchant_id"`
	BrandID     string `json:"brand_id,omitempty"`
	Name        string `json:"name"`
	City        string `json:"city,omitempty"`
	Address     string `json:"address,omitempty"`
	Status      string `json:"status"`
	AuditStatus string `json:"audit_status,omitempty"`
	AuditRemark string `json:"audit_remark,omitempty"`
	Visible     bool   `json:"visible"`
}

// selectStoreSnapshot 读锁定行并组装快照；brand_id 可空，归一化为空字符串。
func selectStoreSnapshot(ctx context.Context, tx pgx.Tx, id string) (storeSnapshot, error) {
	const q = `
		SELECT merchant_id, COALESCE(brand_id::text, ''), name, city, address,
			status, audit_status, audit_remark, visible
		FROM stores WHERE id = $1 FOR UPDATE`
	var s storeSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&s.MerchantID, &s.BrandID, &s.Name, &s.City, &s.Address,
		&s.Status, &s.AuditStatus, &s.AuditRemark, &s.Visible,
	)
	return s, err
}

type pgStoreRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewStoreRepository(pool *pgxpool.Pool, recorder audit.Recorder) StoreRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgStoreRepo{pool: pool, audit: recorder}
}

// storeColumns 统一 SELECT 列表；merchant_name/brand_name 为联表计算列，仅展示用
const storeColumns = `s.id, s.merchant_id, s.brand_id, s.name, s.logo, s.photos,
	s.province, s.city, s.district, s.province_code, s.city_code, s.district_code,
	s.address, s.longitude, s.latitude,
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

func (r *pgStoreRepo) FindNames(ctx context.Context, ids []string) (map[string]string, error) {
	return findNames(ctx, r.pool, "stores", ids)
}

// findNames reads id/name pairs for one table in a single round trip. table is
// interpolated into the statement, so every caller passes a literal and never
// anything that reached the process from outside.
func findNames(ctx context.Context, pool *pgxpool.Pool, table string, ids []string) (map[string]string, error) {
	names := map[string]string{}
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := pool.Query(ctx, `SELECT id::text, name FROM `+table+` WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

func (r *pgStoreRepo) Create(ctx context.Context, s *model.Store) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO stores (id, merchant_id, brand_id, name, logo, photos,
			province, city, district, province_code, city_code, district_code,
			address, longitude, latitude,
			phone, contact_name, contact_phone, detail, business_hours,
			status, audit_status, audit_remark, audit_by, remark, visible, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, '{}'::text[]), $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)`
	if _, err := tx.Exec(ctx, q,
		s.ID, s.MerchantID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.ProvinceCode, s.CityCode, s.DistrictCode,
		s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.Status, s.AuditStatus, s.AuditRemark, s.AuditBy, s.Remark, s.Visible, s.CreatedBy,
		s.CreatedAt, s.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "create", Operation: "新增门店",
		TargetType: "store", TargetID: s.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		After:      audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update 平台直接编辑，不碰状态与审核字段
func (r *pgStoreRepo) Update(ctx context.Context, s *model.Store) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE stores
		SET brand_id = $2, name = $3, logo = $4, photos = COALESCE($5, '{}'::text[]),
			province = $6, city = $7, district = $8,
			province_code = $9, city_code = $10, district_code = $11,
			address = $12, longitude = $13, latitude = $14,
			phone = $15, contact_name = $16, contact_phone = $17, detail = $18, business_hours = $19,
			remark = $20, visible = $21, updated_at = $22
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q,
		s.ID, s.BrandID, s.Name, s.Logo, s.Photos,
		s.Province, s.City, s.District, s.ProvinceCode, s.CityCode, s.DistrictCode,
		s.Address, s.Longitude, s.Latitude,
		s.Phone, s.ContactName, s.ContactPhone, s.Detail, s.BusinessHours,
		s.Remark, s.Visible, s.UpdatedAt,
	); err != nil {
		return err
	}
	after, err := selectStoreSnapshot(ctx, tx, s.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "update", Operation: "修改门店",
		TargetType: "store", TargetID: s.ID, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgStoreRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `UPDATE stores SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "update_status", Operation: "修改门店状态",
		TargetType: "store", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgStoreRepo) SetAudit(ctx context.Context, id, auditStatus, remark, by string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `
		UPDATE stores
		SET audit_status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW(), updated_at = NOW()
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id, auditStatus, remark, by); err != nil {
		return err
	}
	after, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "audit", Operation: "审核门店",
		TargetType: "store", TargetID: id, TargetName: after.Name,
		MerchantID: after.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgStoreRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectStoreSnapshot(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM stores WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "stores", Action: "delete", Operation: "删除门店",
		TargetType: "store", TargetID: id, TargetName: before.Name,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanStore(row pgx.Row) (*model.Store, error) {
	s := &model.Store{}
	err := row.Scan(
		&s.ID, &s.MerchantID, &s.BrandID, &s.Name, &s.Logo, &s.Photos,
		&s.Province, &s.City, &s.District, &s.ProvinceCode, &s.CityCode, &s.DistrictCode,
		&s.Address, &s.Longitude, &s.Latitude,
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
