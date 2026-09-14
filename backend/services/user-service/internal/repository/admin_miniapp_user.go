package repository

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// MiniappUserFilter 是后台用户列表的筛选条件，零值表示不筛选。
type MiniappUserFilter struct {
	// Keyword 匹配手机号前缀或昵称片段，空串表示不限。
	Keyword string
	// Status 取值同 model.UserStatus*，空串表示不限（含已注销）。
	Status string
}

// AdminMiniappUserRepository 是后台（B 端）对小程序用户的管理入口。
//
// 单独一个仓库而不是往 UserRepository 上加方法：那个仓库的写入全是用户对自己的
// 自助操作，没有管理员 Actor 可填，它的接口注释里也写明了「将来后台要提供
// 禁用/解禁入口时单独包一层」。这里的每一次调用都是管理员对别人做的事，
// 都要留 before/after。
type AdminMiniappUserRepository interface {
	// FindPage 返回一页用户；总数走 Count。两者的筛选条件必须同源
	// （共用 miniappUsersWhere 与 miniappUsersArgs），否则会分叉成
	// 「第 3 页是空的，总数却说还有 200 条」。
	FindPage(ctx context.Context, f MiniappUserFilter, limit, offset int) ([]*model.User, error)
	Count(ctx context.Context, f MiniappUserFilter) (int64, error)
	// UpdateStatus 改账号状态，并在同一个事务里撤销该用户全部有效会话，
	// 返回被撤销的会话数。status 传非 active 时撤销才发生。
	UpdateStatus(ctx context.Context, id, status string) (revokedSessions int64, err error)
}

// miniappUsersWhere 是列表与计数共用的筛选条件，$1 = status，$2 = 手机号前缀，
// $3 = 昵称片段。
//
// 写成「参数为空则这条恒真」而不是在 Go 里按条件拼字符串：拼字符串意味着
// Count 和 FindPage 各持一份分支，两者错开的症状只在翻到第二页之后才出现。
const miniappUsersWhere = `
	WHERE ($1 = '' OR u.status = $1)
	  AND ($2 = '' OR u.phone LIKE $2 || '%' OR u.nickname ILIKE $3 ESCAPE '\')`

// escapeLikePattern 转义 LIKE 的通配符。
//
// 不转义的话，搜索框里打一个 "%" 会命中全部用户。这不是注入（参数照样是参数化的），
// 而是「搜一个不存在的昵称却返回一整页人」，看起来像搜索坏了。
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// miniappUsersArgs 按 miniappUsersWhere 的占位符顺序排列参数。
func miniappUsersArgs(f MiniappUserFilter) []any {
	keyword := strings.TrimSpace(f.Keyword)
	escaped := escapeLikePattern(keyword)
	return []any{f.Status, escaped, "%" + escaped + "%"}
}

// miniappUserAuditSnapshot 是写进审计的字段集。
//
// 手机号走 model.MaskPhone 而不是原文：审计条目会被 outbox relay 发到消息总线，
// 完整号码在那儿就是又一份需要单独保护的 PII 副本，而这条记录要回答的问题
// （谁、在什么时候、把哪个账号改成了什么状态）用打码号码一样答得出来。
type miniappUserAuditSnapshot struct {
	Phone          string `json:"phone"`
	Nickname       string `json:"nickname"`
	Status         string `json:"status"`
	RegisterSource string `json:"register_source"`
}

type pgAdminMiniappUserRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

// NewAdminMiniappUserRepository 构造后台用户管理仓库。
func NewAdminMiniappUserRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminMiniappUserRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminMiniappUserRepo{pool: pool, audit: recorder}
}

// FindPage 按注册时间倒序：后台看的是「最新注册的人」，列表和翻页都从近往远走。
// id 作为排序决胜位，理由同 adminUserRepo.FindPage——created_at 相同的行
// （同一次批量迁移很常见）单靠时间排序没有稳定次序，翻页会重复或漏行。
func (r *pgAdminMiniappUserRepo) FindPage(ctx context.Context, f MiniappUserFilter, limit, offset int) ([]*model.User, error) {
	q := `SELECT ` + userColumns + `
		FROM users u` + miniappUsersWhere + `
		ORDER BY u.created_at DESC, u.id DESC
		LIMIT $4 OFFSET $5`
	rows, err := r.pool.Query(ctx, q, append(miniappUsersArgs(f), limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func (r *pgAdminMiniappUserRepo) Count(ctx context.Context, f MiniappUserFilter) (int64, error) {
	q := `SELECT count(*) FROM users u` + miniappUsersWhere
	var n int64
	err := r.pool.QueryRow(ctx, q, miniappUsersArgs(f)...).Scan(&n)
	return n, err
}

// UpdateStatus 改状态并撤销会话，两件事必须同事务：分开做的话，中间失败会留下
// 「状态已禁用、refresh token 却还能换新令牌」的账号，而调用方看到的是失败，
// 以为什么都没发生。
func (r *pgAdminMiniappUserRepo) UpdateStatus(ctx context.Context, id, status string) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// FOR UPDATE 锁住这一行：并发的两次变更（同时点「禁用」和「启用」）会在此串行，
	// 后到的那次读到的是前一次写完的状态，审计里的 before 才是真的。
	const selectSQL = `
		SELECT COALESCE(phone, ''), nickname, status, register_source
		FROM users WHERE id = $1 FOR UPDATE`
	var phone, nickname, currentStatus, registerSource string
	if err := tx.QueryRow(ctx, selectSQL, id).Scan(
		&phone, &nickname, &currentStatus, &registerSource,
	); err != nil {
		return 0, err
	}
	// 已注销的账号不允许从后台复活。这条判定放在这里而不是 service：它取决于
	// 这一行此刻的取值，读一次再判才作数（先读后判的窗口里它可能刚被注销）。
	if currentStatus == model.UserStatusDeleted {
		return 0, model.ErrUserDeleted
	}
	before := miniappUserAuditSnapshot{
		Phone: model.MaskPhone(phone), Nickname: nickname,
		Status: currentStatus, RegisterSource: registerSource,
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET status = $2, updated_at = NOW() WHERE id = $1`, id, status); err != nil {
		return 0, err
	}

	var revoked int64
	if status != model.UserStatusActive {
		// 不看旧状态，非 active 就重撤一遍：从 disabled 再改成 disabled 之间
		// 可能有新会话签出来（刷新时校验 status 是兜底，撤销才是立刻生效的那道）。
		//
		// 语句与 pgUserSessionRepo.RevokeUserSessions 相同，但没有复用它：那个实现
		// 走连接池、自带事务，在这里调它等于把撤销挪到本事务之外，正是上面注释里
		// 要避免的那种半成品状态。
		const revokeSQL = `
			UPDATE user_sessions
			SET revoked_at = NOW(), revoke_reason = $2, updated_at = NOW()
			WHERE user_id = $1 AND revoked_at IS NULL`
		tag, err := tx.Exec(ctx, revokeSQL, id, model.RevokeReasonDisabled)
		if err != nil {
			return 0, err
		}
		revoked = tag.RowsAffected()
	}

	after := before
	after.Status = status
	targetName := before.Nickname
	if targetName == "" {
		targetName = before.Phone
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "miniapp_users", Action: "update_status", Operation: "修改小程序用户状态",
		TargetType: "miniapp_user", TargetID: id, TargetName: targetName,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return revoked, nil
}
