package operationlog

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgreSQL struct{ pool *pgxpool.Pool }

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{pool: pool} }

const selectColumns = `id, admin_user_id, admin_username, admin_name, module, action, operation,
	target_type, target_id, target_name, merchant_id, result, error_code, error_message,
	before_data, after_data, occurred_at`

func scan(row pgx.Row) (*Record, error) {
	var r Record
	var adminID, targetID, merchantID *string
	var before, after []byte
	err := row.Scan(&r.ID, &adminID, &r.AdminUsername, &r.AdminName, &r.Module, &r.Action, &r.Operation,
		&r.TargetType, &targetID, &r.TargetName, &merchantID, &r.Result, &r.ErrorCode, &r.ErrorMessage,
		&before, &after, &r.OccurredAt)
	if err != nil {
		return nil, err
	}
	if adminID != nil {
		r.AdminUserID = *adminID
	}
	if targetID != nil {
		r.TargetID = *targetID
	}
	if merchantID != nil {
		r.MerchantID = *merchantID
	}
	if len(before) > 0 {
		if err := json.Unmarshal(before, &r.BeforeData); err != nil {
			return nil, err
		}
	}
	if len(after) > 0 {
		if err := json.Unmarshal(after, &r.AfterData); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func (p *PostgreSQL) Create(ctx context.Context, r *Record) error {
	before, err := json.Marshal(r.BeforeData)
	if err != nil {
		return err
	}
	after, err := json.Marshal(r.AfterData)
	if err != nil {
		return err
	}
	return p.pool.QueryRow(ctx, `INSERT INTO admin_operation_logs
		(admin_user_id, admin_username, admin_name, module, action, operation, target_type, target_id,
		target_name, merchant_id, result, error_code, error_message, before_data, after_data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+selectColumns,
		nullable(r.AdminUserID), r.AdminUsername, r.AdminName, r.Module, r.Action, r.Operation,
		r.TargetType, nullable(r.TargetID), r.TargetName, nullable(r.MerchantID), r.Result,
		r.ErrorCode, r.ErrorMessage, before, after).Scan(
		&r.ID, new(*string), &r.AdminUsername, &r.AdminName, &r.Module, &r.Action, &r.Operation,
		&r.TargetType, new(*string), &r.TargetName, new(*string), &r.Result, &r.ErrorCode, &r.ErrorMessage,
		new([]byte), new([]byte), &r.OccurredAt)
}

func (p *PostgreSQL) FindAll(ctx context.Context, q Query) ([]*Record, int, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 20
	}
	if q.PageSize > 100 {
		q.PageSize = 100
	}
	args := []any{nullable(q.AdminUserID), nullable(q.Module), nullable(q.Action), nullable(q.TargetType), nullable(q.Result), nullable(q.MerchantID)}
	where := ` WHERE ($1::uuid IS NULL OR admin_user_id=$1) AND ($2 IS NULL OR module=$2) AND ($3 IS NULL OR action=$3) AND ($4 IS NULL OR target_type=$4) AND ($5 IS NULL OR result=$5) AND ($6::uuid IS NULL OR merchant_id=$6)`
	var total int
	if err := p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_operation_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, q.PageSize, (q.Page-1)*q.PageSize)
	rows, err := p.pool.Query(ctx, `SELECT `+selectColumns+` FROM admin_operation_logs`+where+` ORDER BY occurred_at DESC, id DESC LIMIT $7 OFFSET $8`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]*Record, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

var _ = strconv.Itoa
