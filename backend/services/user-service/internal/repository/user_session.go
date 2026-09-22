package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// UserSessionRepository 是**小程序顾客** Refresh Token 会话的数据访问接口。
//
// 撤销用「置 revoked_at」而不是删行：一枚已经轮换掉的 token 再被使用时，那一行
// 还在才能识别出「盗用」这件事。行删了，攻击者拿着旧 token 来换，系统看到的只
// 是一个查不到的哈希，和有人的 token 打错字完全没法区分。
//
// 实现是 sessionStore（见 session_store.go），与后台管理员的 AdminSessionRepository
// 共用同一段 SQL：两张表同形，差别只有表名。这里只留接口与构造函数。
type UserSessionRepository interface {
	CreateSession(ctx context.Context, s *model.UserSession) error
	// FindSessionByHash 按 refresh token 的确定性哈希反查。用哈希而不是原文，
	// 是因为表里根本不存原文；返回的行可能已撤销或已过期，判断留给调用方。
	FindSessionByHash(ctx context.Context, hash string) (*model.UserSession, error)
	// RevokeSession 撤销一枚会话。只对当前有效的行生效：已经撤销过的行会返回
	// pgx.ErrNoRows，而不是把新的撤销原因覆盖到旧的上面——首次撤销的原因才是
	// 事实（rotated 就是 rotated，不该被后来的 reuse_detected 改写）。
	//
	// 返回 ErrNoRows 同时还是一个乐观锁信号：并发刷新同一枚 token 时，两条
	// 请求都会先读到「有效」，但只有一条能把 revoked_at 从 NULL 改掉，另一条
	// 拿到 0 行。刷新链路据此判定为盗用并踢掉整条会话链，见 service。
	RevokeSession(ctx context.Context, id, reason string) error
	// RevokeUserSessions 撤销某用户当前全部有效会话，用于退出登录、改绑手机号、
	// 账号被禁用/注销，以及检测到 token 盗用。已经撤销过的行不动，所以
	// reuse_detected 这个原因只落在「盗用发生时還活着的那些」会话上，
	// 一批会话在同一时刻被标成它，本身就是盗用发生过的证据。
	//
	// 注意它走连接池、自带事务，所以后台「禁用并踢下线」那条路径不能在它自己的
	// 事务里调它——状态与撤销必须同事务提交，见 repository/admin_miniapp_user.go。
	RevokeUserSessions(ctx context.Context, userID, reason string) error
	// CountActiveByUser 当前仍然有效的会话数（未撤销且未过期），后台详情页用。
	// 过期但未撤销的行不算：它们换不出新令牌，把它们算进去等于让页面显示一个
	// 比实际更大的数字，而管理员正是靠这个数字判断「禁用后还有没有人挂在线上」。
	CountActiveByUser(ctx context.Context, userID string) (int64, error)
	// TouchSessionUsed 记录这枚 refresh token 最近一次被使用的时间，尽力而为。
	TouchSessionUsed(ctx context.Context, id string) error
}

// NewUserSessionRepository 构造小程序顾客的会话仓库（user_sessions 表）。
func NewUserSessionRepository(pool *pgxpool.Pool) UserSessionRepository {
	return &sessionStore{pool: pool, table: "user_sessions"}
}
