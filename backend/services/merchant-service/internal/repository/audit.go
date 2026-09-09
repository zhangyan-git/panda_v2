package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// AuditRecordRepository 审核记录数据访问接口。
// 品牌/门店两张审核表结构相同，仅表名与目标列名不同，用同一实现参数化。
type AuditRecordRepository interface {
	Create(ctx context.Context, r *model.AuditRecord) error
	FindLatestPending(ctx context.Context, targetID string) (*model.AuditRecord, error)
	SetAudited(ctx context.Context, id, status, remark, by string) error
}

type pgAuditRepo struct {
	pool         *pgxpool.Pool
	table        string // brand_audit_records / store_audit_records
	targetColumn string // brand_id / store_id
}

func NewBrandAuditRepository(pool *pgxpool.Pool) AuditRecordRepository {
	return &pgAuditRepo{pool: pool, table: "brand_audit_records", targetColumn: "brand_id"}
}

func NewStoreAuditRepository(pool *pgxpool.Pool) AuditRecordRepository {
	return &pgAuditRepo{pool: pool, table: "store_audit_records", targetColumn: "store_id"}
}

func (r *pgAuditRepo) Create(ctx context.Context, rec *model.AuditRecord) error {
	q := `INSERT INTO ` + r.table + `
		(id, ` + r.targetColumn + `, type, status, old_data, new_data,
		 submit_remark, audit_remark, audit_by, submitted_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	var oldData, newData any
	if rec.OldData != "" {
		oldData = []byte(rec.OldData)
	}
	newData = []byte(rec.NewData)
	_, err := r.pool.Exec(ctx, q,
		rec.ID, rec.TargetID, rec.Type, rec.Status, oldData, newData,
		rec.SubmitRemark, rec.AuditRemark, rec.AuditBy, rec.SubmittedBy, rec.CreatedAt,
	)
	return err
}

func (r *pgAuditRepo) FindLatestPending(ctx context.Context, targetID string) (*model.AuditRecord, error) {
	q := `SELECT id, ` + r.targetColumn + `, type, status,
			COALESCE(old_data::text, ''), new_data::text, submit_remark, audit_remark,
			audit_by, audit_at, submitted_by, created_at
		FROM ` + r.table + `
		WHERE ` + r.targetColumn + ` = $1 AND status = 'pending'
		ORDER BY created_at DESC
		LIMIT 1`
	return scanAuditRecord(r.pool.QueryRow(ctx, q, targetID))
}

// SetAudited 审核落章：状态 + 备注 + 审核人 + 审核时间
func (r *pgAuditRepo) SetAudited(ctx context.Context, id, status, remark, by string) error {
	q := `UPDATE ` + r.table + `
		SET status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id, status, remark, by)
	return err
}

func scanAuditRecord(row pgx.Row) (*model.AuditRecord, error) {
	rec := &model.AuditRecord{}
	err := row.Scan(
		&rec.ID, &rec.TargetID, &rec.Type, &rec.Status,
		&rec.OldData, &rec.NewData, &rec.SubmitRemark, &rec.AuditRemark,
		&rec.AuditBy, &rec.AuditAt, &rec.SubmittedBy, &rec.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return rec, nil
}
