package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

// ErrAuditNotPending 这行已经不在待审核状态了：并发的第二次审核、或者一条早就审过的
// 记录，都会落到这里。谓词（而不是「先读再判断」）才是唯一权威——两次审核同时读到
// pending 的时候，只有先拿到行锁的那次能改成 1 行。
var ErrAuditNotPending = errors.New("该记录已被审核过")

// execer 让审核记录的写入只写一遍 SQL：*pgxpool.Pool 与 pgx.Tx 都满足它，
// 「独立提交」与「在调用方事务里提交」于是共用同一份语句。
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AuditRecordRepository 审核记录数据访问接口。
// 品牌/门店两张审核表结构相同，仅表名与目标列名不同，用同一实现参数化。
//
// 本文件的写入刻意不产生后台审计事件：SetAudited* 总是紧跟在同一次审核的
// brands.SetAuditInTx / stores.SetAuditInTx 之后（见 service/brand.go），那一步已经记了
// 一条含前后快照的审核审计。在这里再记一次，同一次点击会变成两条记录。
type AuditRecordRepository interface {
	// Create 独立提交一条审核记录。只留给「实体与记录不在同一次写入」的编辑链路
	// （平台直接编辑，记录是留痕而不是待审项）；新建与审核两条链路必须走 CreateInTx，
	// 否则进程死在两次提交之间会留下一个不进待审列表的实体。
	Create(ctx context.Context, r *model.AuditRecord) error
	// CreateInTx 在调用方的事务里落一条审核记录，与实体写入同生共死。
	CreateInTx(ctx context.Context, tx pgx.Tx, r *model.AuditRecord) error
	// FindLatestPendingInTx 在调用方的事务里找最近一条待审记录；没有时返回 pgx.ErrNoRows。
	FindLatestPendingInTx(ctx context.Context, tx pgx.Tx, targetID string) (*model.AuditRecord, error)
	// SetAuditedInTx 审核落章：状态 + 备注 + 审核人 + 审核时间。谓词限定 pending，
	// 已经审过的记录改不动，受影响行数为 0 时返回 ErrAuditNotPending。
	SetAuditedInTx(ctx context.Context, tx pgx.Tx, id, status, remark, by string) error
	// SetAudited 同 SetAuditedInTx，但自己提交；只给不参与跨表事务的调用方。
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
	return r.insert(ctx, r.pool, rec)
}

func (r *pgAuditRepo) CreateInTx(ctx context.Context, tx pgx.Tx, rec *model.AuditRecord) error {
	return r.insert(ctx, tx, rec)
}

func (r *pgAuditRepo) insert(ctx context.Context, db execer, rec *model.AuditRecord) error {
	q := `INSERT INTO ` + r.table + `
		(id, ` + r.targetColumn + `, type, status, old_data, new_data,
		 submit_remark, audit_remark, audit_by, submitted_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	var oldData, newData any
	if rec.OldData != "" {
		oldData = []byte(rec.OldData)
	}
	newData = []byte(rec.NewData)
	_, err := db.Exec(ctx, q,
		rec.ID, rec.TargetID, rec.Type, rec.Status, oldData, newData,
		rec.SubmitRemark, rec.AuditRemark, rec.AuditBy, rec.SubmittedBy, rec.CreatedAt,
	)
	return err
}

func (r *pgAuditRepo) FindLatestPendingInTx(ctx context.Context, tx pgx.Tx, targetID string) (*model.AuditRecord, error) {
	q := `SELECT id, ` + r.targetColumn + `, type, status,
			COALESCE(old_data::text, ''), new_data::text, submit_remark, audit_remark,
			audit_by, audit_at, submitted_by, created_at
		FROM ` + r.table + `
		WHERE ` + r.targetColumn + ` = $1 AND status = 'pending'
		ORDER BY created_at DESC
		LIMIT 1`
	return scanAuditRecord(tx.QueryRow(ctx, q, targetID))
}

func (r *pgAuditRepo) SetAudited(ctx context.Context, id, status, remark, by string) error {
	return r.setAudited(ctx, r.pool, id, status, remark, by)
}

func (r *pgAuditRepo) SetAuditedInTx(ctx context.Context, tx pgx.Tx, id, status, remark, by string) error {
	return r.setAudited(ctx, tx, id, status, remark, by)
}

func (r *pgAuditRepo) setAudited(ctx context.Context, db execer, id, status, remark, by string) error {
	q := `UPDATE ` + r.table + `
		SET status = $2, audit_remark = $3, audit_by = $4, audit_at = NOW()
		WHERE id = $1 AND status = 'pending'`
	tag, err := db.Exec(ctx, q, id, status, remark, by)
	if err != nil {
		return err
	}
	// 0 行 = 这条记录在本次审核读它之后已经被别人盖过章了。静默成功的话，
	// 两个并发的「通过 / 驳回」会双双汇报成功。
	if tag.RowsAffected() == 0 {
		return ErrAuditNotPending
	}
	return nil
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
