package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminSessionRepository 是**平台后台管理员** Refresh Token 会话的数据访问接口。
//
// 与 UserSessionRepository 方法集相同、语义逐条相同，但打的是 admin_sessions 表
// （migrations/identity）：管理员住在 admin_users 里，和 users 一行都不重叠，所以两者
// 不能共用一张表——user_sessions 的外键是 user_id → users(id)，写管理员 id 进去
// 是 23503。方法集的注释不再重复一遍，看 UserSessionRepository。
//
// 方法集**故意**与 UserSessionRepository 保持一致而不是各取所需：这样任何一份
// 实现了前者的替身（测试里的 fake、以后的包装器）都能直接拿来当后者用，
// 两边不会因为「这边多一个方法」而悄悄分叉。
type AdminSessionRepository interface {
	CreateSession(ctx context.Context, s *model.UserSession) error
	FindSessionByHash(ctx context.Context, hash string) (*model.UserSession, error)
	RevokeSession(ctx context.Context, id, reason string) error
	RevokeUserSessions(ctx context.Context, userID, reason string) error
	CountActiveByUser(ctx context.Context, userID string) (int64, error)
	TouchSessionUsed(ctx context.Context, id string) error
}

// NewAdminSessionRepository 构造后台管理员的会话仓库（admin_sessions 表）。
func NewAdminSessionRepository(pool *pgxpool.Pool) AdminSessionRepository {
	return &sessionStore{pool: pool, table: "admin_sessions"}
}
