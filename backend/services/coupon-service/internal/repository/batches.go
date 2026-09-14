package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
)

type BatchRepositoryQuery interface {
	ListBatches(context.Context, dto.CouponBatchQuery) ([]*model.CouponBatch, int64, error)
	GetBatch(context.Context, string) (*model.CouponBatch, error)
	BatchStats(context.Context, string) (*dto.CouponBatchStats, error)
}

const batchColumns = `id::text,template_id::text,batch_no,source,total_quantity,reserved_quantity,issued_quantity,released_quantity,status,order_id::text,user_id::text,request_id,created_by::text,created_at,updated_at`

func (r *postgresRepository) ListBatches(ctx context.Context, q dto.CouponBatchQuery) ([]*model.CouponBatch, int64, error) {
	where := []string{"1=1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if q.TemplateID != "" {
		add("template_id=$%d", q.TemplateID)
	}
	if q.Status != "" {
		add("status=$%d", q.Status)
	}
	if q.Source != "" {
		add("source=$%d", q.Source)
	}
	if q.BatchNo != "" {
		add("batch_no ILIKE '%%' || $%d || '%%'", q.BatchNo)
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM coupon_batches WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset := (q.Page - 1) * q.PageSize
	args = append(args, q.PageSize, offset)
	rows, err := r.pool.Query(ctx, "SELECT "+batchColumns+" FROM coupon_batches WHERE "+whereSQL+fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var items []*model.CouponBatch
	for rows.Next() {
		b := &model.CouponBatch{}
		if err := rows.Scan(&b.ID, &b.TemplateID, &b.BatchNo, &b.Source, &b.TotalQuantity, &b.ReservedQuantity, &b.IssuedQuantity, &b.ReleasedQuantity, &b.Status, &b.OrderID, &b.UserID, &b.RequestID, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, b)
	}
	return items, total, rows.Err()
}

func (r *postgresRepository) GetBatch(ctx context.Context, id string) (*model.CouponBatch, error) {
	b := &model.CouponBatch{}
	err := r.pool.QueryRow(ctx, "SELECT "+batchColumns+" FROM coupon_batches WHERE id=$1", id).Scan(&b.ID, &b.TemplateID, &b.BatchNo, &b.Source, &b.TotalQuantity, &b.ReservedQuantity, &b.IssuedQuantity, &b.ReleasedQuantity, &b.Status, &b.OrderID, &b.UserID, &b.RequestID, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func (r *postgresRepository) BatchStats(ctx context.Context, id string) (*dto.CouponBatchStats, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM coupon_batches WHERE id=$1)", id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, pgx.ErrNoRows
	}
	s := &dto.CouponBatchStats{}
	err := r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE status='claimed'),count(*) FILTER (WHERE status='redeemed'),count(*) FILTER (WHERE status='expired'),count(*) FILTER (WHERE status='refunded') FROM user_coupons WHERE batch_id=$1`, id).Scan(&s.Total, &s.Claimed, &s.Redeemed, &s.Expired, &s.Refunded)
	return s, err
}

var _ BatchRepositoryQuery = (*postgresRepository)(nil)
