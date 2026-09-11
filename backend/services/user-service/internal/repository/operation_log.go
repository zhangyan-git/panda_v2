package repository

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
)

// AdminOperationLogRepository 写入平台后台操作日志。
//
// 它只有写入方、没有查询方：审计日志的读取走的是后台查询接口之外的路子，
// 现阶段只需要把事件落库。插入必须能安全重放——消费者可能在「写成功、
// 标记 inbox 完成失败」之后重投同一条消息。
type AdminOperationLogRepository interface {
	Insert(ctx context.Context, eventID string, entry audit.Entry) error
}

type pgOperationLogRepo struct{ pool *pgxpool.Pool }

func NewAdminOperationLogRepository(pool *pgxpool.Pool) AdminOperationLogRepository {
	return &pgOperationLogRepo{pool: pool}
}

// Insert 按 event_id 幂等：重复投递走 ON CONFLICT DO NOTHING 而不是报错，
// 这样消费者的重投既不会写重影，也不需要它自己判断「是不是已经写过了」。
//
// 操作人用户名/名称在这里按 admin_user_id 现查。事件里没有它们，因为 JWT 里就没有
// 用户名，而改名是低频操作——用现查值当快照，比让每个服务在 token 里多带一份
// 用户名、或者为了一次改名做一次跨服务调用，都更划算。操作人已被删除时
// （admin_user_id 是 ON DELETE SET NULL，列可为空）快照留空，日志本身照写。
//
// 空的 UUID 字符串一律经 NULLIF 转成 NULL 再转型：拿空串直接转 uuid 会报错。
func (r *pgOperationLogRepo) Insert(ctx context.Context, eventID string, entry audit.Entry) error {
	const q = `
		INSERT INTO admin_operation_logs (
			event_id, admin_user_id, admin_username, admin_name,
			module, action, operation, target_type, target_id, target_name, merchant_id,
			result, error_code, error_message, before_data, after_data
		)
		SELECT $1, u.id, COALESCE(u.username, ''), COALESCE(u.name, ''),
			$3, $4, $5, $6, NULLIF($7, '')::uuid, $8, NULLIF($9, '')::uuid,
			$10, $11, $12, $13::jsonb, $14::jsonb
		FROM (SELECT 1) AS base
		LEFT JOIN admin_users u ON u.id = NULLIF($2, '')::uuid
		ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO NOTHING`
	_, err := r.pool.Exec(ctx, q,
		eventID, entry.ActorID,
		entry.Module, entry.Action, entry.Operation,
		entry.TargetType, entry.TargetID, entry.TargetName, entry.MerchantID,
		entry.Result, entry.ErrorCode, entry.ErrorMessage,
		jsonOrNull(entry.Before), jsonOrNull(entry.After),
	)
	return err
}

// jsonOrNull 把空快照变成 SQL NULL：JSONB 列接受 NULL，但不接受空串。
func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
