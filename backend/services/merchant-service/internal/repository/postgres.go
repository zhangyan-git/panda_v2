package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/domain"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type querier interface {
	rowQuerier
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type PostgreSQL struct {
	pool querier
	exec func(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{pool: pool, exec: pool.Exec} }

const merchantColumns = "id, name, contact_name, contact_phone, business_license, address, status, expire_date, created_at, updated_at"
const storeColumns = "id, merchant_id, brand_id, name, logo, phone, province, city, district, address, business_hours, longitude, latitude, status, audit_status, audit_remark, visible, created_at, updated_at"
const accountColumns = "id, merchant_id, account_id, real_name, is_admin, permission_type, brand_ids, store_ids, created_at, updated_at"
const auditColumns = "id, store_id, type, status, new_data, old_data, submitted_by, audited_by, audit_remark, created_at, updated_at"

func scanMerchant(row pgx.Row) (domain.Merchant, error) {
	var v domain.Merchant
	err := row.Scan(&v.ID, &v.Name, &v.ContactName, &v.ContactPhone, &v.BusinessLicense, &v.Address, &v.Status, &v.ExpireDate, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}
func scanStore(row pgx.Row) (domain.Store, error) {
	var v domain.Store
	err := row.Scan(&v.ID, &v.MerchantID, &v.BrandID, &v.Name, &v.Logo, &v.Phone, &v.Province, &v.City, &v.District, &v.Address, &v.BusinessHours, &v.Longitude, &v.Latitude, &v.Status, &v.AuditStatus, &v.AuditRemark, &v.Visible, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}
func scanAccount(row pgx.Row) (domain.MerchantAccount, error) {
	var v domain.MerchantAccount
	err := row.Scan(&v.ID, &v.MerchantID, &v.AccountID, &v.RealName, &v.IsAdmin, &v.PermissionType, &v.BrandIDs, &v.StoreIDs, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}
func scanAudit(row pgx.Row) (domain.StoreAuditRecord, error) {
	var v domain.StoreAuditRecord
	err := row.Scan(&v.ID, &v.StoreID, &v.Type, &v.Status, &v.NewData, &v.OldData, &v.SubmittedBy, &v.AuditedBy, &v.AuditRemark, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (r *PostgreSQL) CreateMerchant(ctx context.Context, v domain.Merchant) (domain.Merchant, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	err := r.pool.QueryRow(ctx, `INSERT INTO merchants (id,name,contact_name,contact_phone,business_license,address,status,expire_date) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+merchantColumns, v.ID, v.Name, v.ContactName, v.ContactPhone, v.BusinessLicense, v.Address, v.Status, v.ExpireDate).Scan(&v.ID, &v.Name, &v.ContactName, &v.ContactPhone, &v.BusinessLicense, &v.Address, &v.Status, &v.ExpireDate, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}
func (r *PostgreSQL) GetMerchant(ctx context.Context, id string) (domain.Merchant, error) {
	v, e := scanMerchant(r.pool.QueryRow(ctx, `SELECT `+merchantColumns+` FROM merchants WHERE id=$1`, id))
	return v, notFound(e)
}
func (r *PostgreSQL) ListMerchants(ctx context.Context) ([]domain.Merchant, error) {
	rows, e := r.pool.Query(ctx, `SELECT `+merchantColumns+` FROM merchants ORDER BY created_at DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Merchant{}
	for rows.Next() {
		v, e := scanMerchant(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) UpdateMerchant(ctx context.Context, id string, v domain.Merchant) (domain.Merchant, error) {
	x, e := scanMerchant(r.pool.QueryRow(ctx, `UPDATE merchants SET name=$2,contact_name=$3,contact_phone=$4,business_license=$5,address=$6,status=$7,expire_date=$8,updated_at=NOW() WHERE id=$1 RETURNING `+merchantColumns, id, v.Name, v.ContactName, v.ContactPhone, v.BusinessLicense, v.Address, v.Status, v.ExpireDate))
	return x, notFound(e)
}
func (r *PostgreSQL) SetMerchantStatus(ctx context.Context, id string, s domain.Status) (domain.Merchant, error) {
	x, e := scanMerchant(r.pool.QueryRow(ctx, `UPDATE merchants SET status=$2,updated_at=NOW() WHERE id=$1 RETURNING `+merchantColumns, id, s))
	return x, notFound(e)
}
func (r *PostgreSQL) CreateStore(ctx context.Context, v domain.Store) (domain.Store, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	x, e := scanStore(r.pool.QueryRow(ctx, `INSERT INTO stores (id,merchant_id,brand_id,name,logo,phone,province,city,district,address,business_hours,longitude,latitude,status,audit_status,audit_remark,visible) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING `+storeColumns, v.ID, v.MerchantID, v.BrandID, v.Name, v.Logo, v.Phone, v.Province, v.City, v.District, v.Address, v.BusinessHours, v.Longitude, v.Latitude, v.Status, v.AuditStatus, v.AuditRemark, v.Visible))
	return x, e
}
func (r *PostgreSQL) GetStore(ctx context.Context, id string) (domain.Store, error) {
	v, e := scanStore(r.pool.QueryRow(ctx, `SELECT `+storeColumns+` FROM stores WHERE id=$1`, id))
	return v, notFound(e)
}
func (r *PostgreSQL) ListStoresByMerchant(ctx context.Context, id string) ([]domain.Store, error) {
	rows, e := r.pool.Query(ctx, `SELECT `+storeColumns+` FROM stores WHERE merchant_id=$1 ORDER BY created_at DESC`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Store{}
	for rows.Next() {
		v, e := scanStore(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *PostgreSQL) UpdateStore(ctx context.Context, id string, v domain.Store) (domain.Store, error) {
	x, e := scanStore(r.pool.QueryRow(ctx, `UPDATE stores SET brand_id=$2,name=$3,logo=$4,phone=$5,province=$6,city=$7,district=$8,address=$9,business_hours=$10,longitude=$11,latitude=$12,status=$13,audit_status=$14,audit_remark=$15,visible=$16,updated_at=NOW() WHERE id=$1 RETURNING `+storeColumns, id, v.BrandID, v.Name, v.Logo, v.Phone, v.Province, v.City, v.District, v.Address, v.BusinessHours, v.Longitude, v.Latitude, v.Status, v.AuditStatus, v.AuditRemark, v.Visible))
	return x, notFound(e)
}
func (r *PostgreSQL) SetStoreStatus(ctx context.Context, id string, s domain.Status) (domain.Store, error) {
	x, e := scanStore(r.pool.QueryRow(ctx, `UPDATE stores SET status=$2,updated_at=NOW() WHERE id=$1 RETURNING `+storeColumns, id, s))
	return x, notFound(e)
}
func (r *PostgreSQL) DeleteStore(ctx context.Context, id string) error {
	if r.exec == nil {
		return errors.New("merchant repository executor is nil")
	}
	tag, e := r.exec(ctx, `DELETE FROM stores WHERE id=$1`, id)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
func (r *PostgreSQL) CreateMerchantAccount(ctx context.Context, v domain.MerchantAccount) (domain.MerchantAccount, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	x, e := scanAccount(r.pool.QueryRow(ctx, `INSERT INTO merchant_accounts (id,merchant_id,account_id,real_name,is_admin,permission_type,brand_ids,store_ids) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+accountColumns, v.ID, v.MerchantID, v.AccountID, v.RealName, v.IsAdmin, v.PermissionType, v.BrandIDs, v.StoreIDs))
	return x, e
}
func (r *PostgreSQL) GetMerchantAccountByAccountID(ctx context.Context, id string) (domain.MerchantAccount, error) {
	v, e := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM merchant_accounts WHERE account_id=$1`, id))
	return v, notFound(e)
}
func (r *PostgreSQL) UpdateMerchantAccount(ctx context.Context, id string, v domain.MerchantAccount) (domain.MerchantAccount, error) {
	x, e := scanAccount(r.pool.QueryRow(ctx, `UPDATE merchant_accounts SET merchant_id=$2,account_id=$3,real_name=$4,is_admin=$5,permission_type=$6,brand_ids=$7,store_ids=$8,updated_at=NOW() WHERE id=$1 RETURNING `+accountColumns, id, v.MerchantID, v.AccountID, v.RealName, v.IsAdmin, v.PermissionType, v.BrandIDs, v.StoreIDs))
	return x, notFound(e)
}
func (r *PostgreSQL) CreateAudit(ctx context.Context, v domain.StoreAuditRecord) (domain.StoreAuditRecord, error) {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	x, e := scanAudit(r.pool.QueryRow(ctx, `INSERT INTO store_audits (id,store_id,type,status,new_data,old_data,submitted_by,audited_by,audit_remark) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+auditColumns, v.ID, v.StoreID, v.Type, v.Status, v.NewData, v.OldData, v.SubmittedBy, v.AuditedBy, v.AuditRemark))
	return x, e
}
func (r *PostgreSQL) GetAudit(ctx context.Context, id string) (domain.StoreAuditRecord, error) {
	v, e := scanAudit(r.pool.QueryRow(ctx, `SELECT `+auditColumns+` FROM store_audits WHERE id=$1`, id))
	return v, notFound(e)
}
func (r *PostgreSQL) UpdateAudit(ctx context.Context, id string, v domain.StoreAuditRecord) (domain.StoreAuditRecord, error) {
	q := `WITH updated AS (UPDATE store_audits SET status=$2,audited_by=$3,audit_remark=$4,updated_at=NOW() WHERE id=$1 AND status='pending' AND $2 IN ('approved','rejected') AND EXISTS (SELECT 1 FROM stores WHERE id=store_audits.store_id) RETURNING ` + auditColumns + `), synced AS (UPDATE stores s SET audit_status=u.status,audit_remark=u.audit_remark,updated_at=NOW() FROM updated u WHERE s.id=u.store_id RETURNING s.id) SELECT u.` + auditColumns + ` FROM updated u JOIN synced s ON s.id=u.store_id`
	x, e := scanAudit(r.pool.QueryRow(ctx, q, id, v.Status, v.AuditedBy, v.AuditRemark))
	if e == nil {
		return x, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return x, e
	}
	// Distinguish a missing audit from a concurrent/previously finalized audit.
	var currentStatus domain.AuditStatus
	checkErr := r.pool.QueryRow(ctx, `SELECT status FROM store_audits WHERE id=$1`, id).Scan(&currentStatus)
	if checkErr == nil {
		return domain.StoreAuditRecord{}, domain.ErrConflict
	}
	return x, notFound(checkErr)
}
func (r *PostgreSQL) ListAuditsByStore(ctx context.Context, id string) ([]domain.StoreAuditRecord, error) {
	q := `SELECT ` + auditColumns + ` FROM store_audits`
	args := []any{}
	if id != "" {
		q += ` WHERE store_id=$1`
		args = append(args, id)
	}
	q += ` ORDER BY created_at DESC`
	rows, e := r.pool.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.StoreAuditRecord{}
	for rows.Next() {
		v, e := scanAudit(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var _ domain.Repository = (*PostgreSQL)(nil)
