package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// UserRepository 是 C 端（小程序）用户的数据访问接口。
//
// 这里没有 audit.Recorder，和 admin_user/merchant_user 不同：那两张表的写入是
// 「平台管理员对别人做了什么」，audit 的 Entry 是以 ActorID 为主键语义的；而
// 本仓库的写入全是用户对自己的自助操作，没有管理员 Actor 可填，硬塞一个 C 端
// user_id 进 admin_operation_logs 只会污染那张表。C 端的安全事件另有落点——
// user_login_events，它记的是「谁在什么时候用什么方式登录/失败」，不做 outbox
// 广播。将来后台要提供禁用/解禁 C 端用户的入口时，那才是需要审计的调用方，
// 届时给它单独包一层，而不是把审计塞进这里的每个方法。
type UserRepository interface {
	// FindByID / FindByPhone 都不过滤 status。调用方必须能区分「账号不存在」与
	// 「账号已禁用/已注销」——前者引导注册，后者要回一个明确的拒绝理由，
	// 在 SQL 里悄悄加 status = 'active' 会让两种情况都退化成同一个 404。
	FindByID(ctx context.Context, id string) (*model.User, error)
	FindByPhone(ctx context.Context, phone string) (*model.User, error)

	// FindWechatIdentity 按唯一键 (app_type, openid) 查一条微信绑定，
	// 命中后由调用方用 UserID 去取用户主体。
	FindWechatIdentity(ctx context.Context, appType, openid string) (*model.UserWechatIdentity, error)
	// FindWechatIdentityByUser 按 (user_id, app_type) 正查，是上面那条的反方向：
	// 登录链路手里只有 openid、要问「这是谁」，而渠道支付手里只有 user_id、要问
	// 「用哪个 openid 去发起」。查不到时返回 pgx.ErrNoRows，由调用方决定是「没有绑定」
	// 还是「查不到这个人」——两者在这一层是同一件事，SQL 里补不出区别。
	FindWechatIdentityByUser(ctx context.Context, userID, appType string) (*model.UserWechatIdentity, error)
	// ListWechatIdentitiesByUser 列出一个账号绑定的全部微信身份，供后台详情页查看。
	// 一个用户将来可能同时挂着小程序和公众号两条：登录链路只按 openid 反查一条，
	// 这里要的是「他到底绑了哪些」。
	ListWechatIdentitiesByUser(ctx context.Context, userID string) ([]*model.UserWechatIdentity, error)
	// RegisterWithWechat 在同一个事务里建用户主体和它的微信身份。
	// 必须同事务：只写了一半的话，这个 openid 下次登录会撞唯一键，
	// 却又找不到对应的账号，用户就永久卡在「已被绑定但登录不进去」。
	RegisterWithWechat(ctx context.Context, u *model.User, ident *model.UserWechatIdentity) error
	// BindWechatIdentity 把一个新微信身份挂到已存在的账号上（微信一键登录时
	// 手机号已命中另一账号，走的是合并/引导路径，这里是「同一个人补绑」）。
	BindWechatIdentity(ctx context.Context, ident *model.UserWechatIdentity) error
	// TouchWechatLogin 更新这条微信身份的最后登录时间，尽力而为。
	TouchWechatLogin(ctx context.Context, appType, openid string) error

	// CreateWithPhone 建一个以手机号注册的账号（手机号 + 短信验证码登录）。
	CreateWithPhone(ctx context.Context, u *model.User) error
	// BindPhone 给已登录账号绑定或换绑手机号。手机号已被占用时返回
	// model.ErrPhoneTaken，由 service 决定引导文案。
	BindPhone(ctx context.Context, userID, phone string) error
	// UpdateProfile 是资料的部分更新，见 UserProfileUpdate。
	UpdateProfile(ctx context.Context, userID string, upd UserProfileUpdate) error
	// UpdateStatus 改账号状态。注意它不会顺带撤销会话——调用方在把状态改成
	// 非 active 之后必须再调 RevokeUserSessions，否则用户手里的 refresh token
	// 还能继续换新，禁用只是界面上的。真正的兜底是刷新时重新读一次 status，
	// 两者都要有：这里管「立刻踢下线」，那里管「漏网的那一枚也用不了」。
	UpdateStatus(ctx context.Context, userID, status string) error
	// TouchLogin 登录成功后记最后登录时间/IP 并累加次数，尽力而为。
	TouchLogin(ctx context.Context, userID, ip string) error

	// RecordLoginEvent 追加一条登录审计。成功和失败都记，见 model.UserLoginEvent。
	RecordLoginEvent(ctx context.Context, e *model.UserLoginEvent) error
	// ListLoginEventsByUser 返回该账号最近 limit 条登录尝试，新的在前，供后台详情页。
	// 按 user_id 查而不是按 identifier：失败记录里 user_id 为空，那些行只属于
	// 「有人在试某个手机号」，不属于某个确定的账号，不在这条查询的语义里。
	ListLoginEventsByUser(ctx context.Context, userID string, limit int) ([]*model.UserLoginEvent, error)
}

// UserProfileUpdate 是一组部分更新：nil 表示「本次不改这一项」。
//
// 用指针而不是零值判断，是因为昵称和地区都可以被清空，空串是合法的目标值，
// 拿空串当「没传」会让用户再也改不掉填错的内容。Birthday 多一个 ClearBirthday，
// 因为日期的「清空」没有对应的零值可表达。
type UserProfileUpdate struct {
	Nickname      *string
	AvatarURL     *string
	Gender        *string
	Birthday      *time.Time
	ClearBirthday bool
	RegionCode    *string
	RegionName    *string
}

type pgUserRepo struct {
	pool *pgxpool.Pool
}

// NewUserRepository 构造 C 端用户仓库。
func NewUserRepository(pool *pgxpool.Pool) UserRepository {
	return &pgUserRepo{pool: pool}
}

// userColumns 统一 SELECT 列表。可空列归一化为空串：legacy_id 和 phone 库里可
// 为空，其余列都有 NOT NULL DEFAULT，扫描时不需要再包一层 COALESCE。
// 顺序必须与 scanUser 的 Scan 逐字段对齐。
const userColumns = `u.id, COALESCE(u.legacy_id, ''), COALESCE(u.phone, ''), u.nickname,
	u.avatar_url, u.gender, u.birthday, u.region_code, u.region_name,
	u.status, u.register_source, u.last_login_at, COALESCE(u.last_login_ip, ''),
	u.login_count, u.created_at, u.updated_at`

// insertUserSQL 和 userWechatIdentityInsertSQL 被注册与绑定两条路径共用，
// 参数顺序由 userInsertArgs / identityInsertArgs 保证，不在这里就地拼字面量：
// 十二个占位符的语句手抄第二遍，错位的概率比打错字高得多。
const insertUserSQL = `
	INSERT INTO users (id, phone, nickname, avatar_url, gender, birthday,
		region_code, region_name, status, register_source, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

const insertUserWechatIdentitySQL = `
	INSERT INTO user_wechat_identities (id, user_id, app_type, openid, unionid,
		last_login_at, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// userInsertArgs 按 insertUserSQL 的占位符顺序排列参数。
// phone 走 nullEmpty：库里空串和 NULL 是两种东西，手机号列上只有 NULL 才表示
// 「未绑定」，写空串进去会让第二个未绑定手机号的用户撞上唯一索引。
func userInsertArgs(u *model.User) []any {
	return []any{
		u.ID, nullEmpty(u.Phone), u.Nickname, u.AvatarURL, u.Gender, u.Birthday,
		u.RegionCode, u.RegionName, u.Status, u.RegisterSource, u.CreatedAt, u.UpdatedAt,
	}
}

func identityInsertArgs(ident *model.UserWechatIdentity) []any {
	return []any{
		ident.ID, ident.UserID, ident.AppType, ident.OpenID, nullEmpty(ident.UnionID),
		ident.LastLoginAt, ident.CreatedAt, ident.UpdatedAt,
	}
}

// userWriteError 把库里的唯一键冲突翻译成 model 的哨兵错误，其它错误原样返回。
// 不翻译的话 SQLSTATE 和约束名会一路冒到响应里，既暴露表结构，调用方也没法分支。
func userWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "users_phone_key":
		return fmt.Errorf("%w: %w", model.ErrPhoneTaken, err)
	case "user_wechat_identities_app_openid_key":
		return fmt.Errorf("%w: %w", model.ErrWechatIdentityTaken, err)
	default:
		return err
	}
}

func (r *pgUserRepo) FindByID(ctx context.Context, id string) (*model.User, error) {
	q := `SELECT ` + userColumns + `
		FROM users u
		WHERE u.id = $1
		LIMIT 1`
	return scanUser(r.pool.QueryRow(ctx, q, id))
}

func (r *pgUserRepo) FindByPhone(ctx context.Context, phone string) (*model.User, error) {
	q := `SELECT ` + userColumns + `
		FROM users u
		WHERE u.phone = $1
		LIMIT 1`
	return scanUser(r.pool.QueryRow(ctx, q, phone))
}

func (r *pgUserRepo) FindWechatIdentity(ctx context.Context, appType, openid string) (*model.UserWechatIdentity, error) {
	const q = `
		SELECT id, user_id, app_type, openid, COALESCE(unionid, ''),
			last_login_at, created_at, updated_at
		FROM user_wechat_identities
		WHERE app_type = $1 AND openid = $2
		LIMIT 1`
	ident := &model.UserWechatIdentity{}
	err := r.pool.QueryRow(ctx, q, appType, openid).Scan(
		&ident.ID, &ident.UserID, &ident.AppType, &ident.OpenID, &ident.UnionID,
		&ident.LastLoginAt, &ident.CreatedAt, &ident.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return ident, nil
}

// FindWechatIdentityByUser 的 ORDER BY 与 LIMIT 不是可有可无的：库里只有
// UNIQUE (app_type, openid)，同一个用户在同一应用下**可以**留下两行（一个 openid
// 换绑过一次就会），所以「这个用户在 miniapp 下的 openid」严格来说是一组而不是一个。
// 与 ListWechatIdentitiesByUser 取同一个次序、同一个理由：先绑的那条才是他原本的账号，
// 而且没有 ORDER BY 的 LIMIT 1 拿到的行是任意的——同一个用户两次调用可能拿到两个不同的
// openid，那会让一次支付重试失败在「换了请求体」上。
func (r *pgUserRepo) FindWechatIdentityByUser(ctx context.Context, userID, appType string) (*model.UserWechatIdentity, error) {
	const q = `
		SELECT id, user_id, app_type, openid, COALESCE(unionid, ''),
			last_login_at, created_at, updated_at
		FROM user_wechat_identities
		WHERE user_id = $1 AND app_type = $2
		ORDER BY created_at, id
		LIMIT 1`
	ident := &model.UserWechatIdentity{}
	err := r.pool.QueryRow(ctx, q, userID, appType).Scan(
		&ident.ID, &ident.UserID, &ident.AppType, &ident.OpenID, &ident.UnionID,
		&ident.LastLoginAt, &ident.CreatedAt, &ident.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return ident, nil
}

// ListWechatIdentitiesByUser 按创建时间排序，最早的绑定在前：同一个用户如果
// 补绑过第二个微信，先来的那条是他原本的账号，顺序本身是有信息量的。
func (r *pgUserRepo) ListWechatIdentitiesByUser(ctx context.Context, userID string) ([]*model.UserWechatIdentity, error) {
	const q = `
		SELECT id, user_id, app_type, openid, COALESCE(unionid, ''),
			last_login_at, created_at, updated_at
		FROM user_wechat_identities
		WHERE user_id = $1
		ORDER BY created_at, id`
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.UserWechatIdentity
	for rows.Next() {
		ident := &model.UserWechatIdentity{}
		if err := rows.Scan(
			&ident.ID, &ident.UserID, &ident.AppType, &ident.OpenID, &ident.UnionID,
			&ident.LastLoginAt, &ident.CreatedAt, &ident.UpdatedAt,
		); err != nil {
			return nil, err
		}
		list = append(list, ident)
	}
	return list, rows.Err()
}

func (r *pgUserRepo) ListLoginEventsByUser(ctx context.Context, userID string, limit int) ([]*model.UserLoginEvent, error) {
	const q = `
		SELECT id, COALESCE(user_id::text, ''), login_type, identifier, success,
			fail_reason, ip, user_agent, created_at
		FROM user_login_events
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`
	rows, err := r.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.UserLoginEvent
	for rows.Next() {
		e := &model.UserLoginEvent{}
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.LoginType, &e.Identifier, &e.Success,
			&e.FailReason, &e.IP, &e.UserAgent, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}

func (r *pgUserRepo) RegisterWithWechat(ctx context.Context, u *model.User, ident *model.UserWechatIdentity) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, insertUserSQL, userInsertArgs(u)...); err != nil {
		return userWriteError(err)
	}
	if _, err := tx.Exec(ctx, insertUserWechatIdentitySQL, identityInsertArgs(ident)...); err != nil {
		return userWriteError(err)
	}
	return tx.Commit(ctx)
}

func (r *pgUserRepo) BindWechatIdentity(ctx context.Context, ident *model.UserWechatIdentity) error {
	_, err := r.pool.Exec(ctx, insertUserWechatIdentitySQL, identityInsertArgs(ident)...)
	return userWriteError(err)
}

func (r *pgUserRepo) TouchWechatLogin(ctx context.Context, appType, openid string) error {
	const q = `
		UPDATE user_wechat_identities
		SET last_login_at = NOW(), updated_at = NOW()
		WHERE app_type = $1 AND openid = $2`
	_, err := r.pool.Exec(ctx, q, appType, openid)
	return err
}

func (r *pgUserRepo) CreateWithPhone(ctx context.Context, u *model.User) error {
	_, err := r.pool.Exec(ctx, insertUserSQL, userInsertArgs(u)...)
	return userWriteError(err)
}

func (r *pgUserRepo) BindPhone(ctx context.Context, userID, phone string) error {
	const q = `UPDATE users SET phone = $2, updated_at = NOW() WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, userID, phone)
	if err != nil {
		return userWriteError(err)
	}
	// 0 行只有一个解释：账号在这一瞬间被删了（或 id 根本不存在）。
	// 返回 ErrNoRows 而不是静默成功，否则「绑定成功」的响应会发给一个没有账号的人。
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// UpdateProfile 动态拼 SET 子句：只有传了的字段才出现在语句里，没传的保持原值。
// 不能用「空值即未传」的写法——昵称清空是合法操作，见 UserProfileUpdate。
func (r *pgUserRepo) UpdateProfile(ctx context.Context, userID string, upd UserProfileUpdate) error {
	set := make([]string, 0, 7)
	args := []any{userID}
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if upd.Nickname != nil {
		add("nickname", *upd.Nickname)
	}
	if upd.AvatarURL != nil {
		add("avatar_url", *upd.AvatarURL)
	}
	if upd.Gender != nil {
		add("gender", *upd.Gender)
	}
	// 清空优先于赋值：两个都传时按调用方的显式意图走，不静默偏向其中一个。
	switch {
	case upd.ClearBirthday:
		set = append(set, "birthday = NULL")
	case upd.Birthday != nil:
		add("birthday", *upd.Birthday)
	}
	if upd.RegionCode != nil {
		add("region_code", *upd.RegionCode)
	}
	if upd.RegionName != nil {
		add("region_name", *upd.RegionName)
	}
	// 一个字段都没传就不发语句：UPDATE ... SET updated_at = NOW() 会把
	// 「什么都没改」记成一次修改，让 updated_at 失去「资料最后变更时间」的含义。
	if len(set) == 0 {
		return nil
	}
	set = append(set, "updated_at = NOW()")

	q := `UPDATE users SET ` + strings.Join(set, ", ") + ` WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *pgUserRepo) UpdateStatus(ctx context.Context, userID, status string) error {
	const q = `UPDATE users SET status = $2, updated_at = NOW() WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, userID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *pgUserRepo) TouchLogin(ctx context.Context, userID, ip string) error {
	const q = `
		UPDATE users
		SET last_login_at = NOW(), last_login_ip = $2, login_count = login_count + 1,
			updated_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, userID, ip)
	return err
}

func (r *pgUserRepo) RecordLoginEvent(ctx context.Context, e *model.UserLoginEvent) error {
	const q = `
		INSERT INTO user_login_events (id, user_id, login_type, identifier, success,
			fail_reason, ip, user_agent, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	_, err := r.pool.Exec(ctx, q,
		e.ID, nullEmpty(e.UserID), e.LoginType, e.Identifier, e.Success,
		e.FailReason, e.IP, e.UserAgent, e.CreatedAt,
	)
	return err
}

func scanUser(row pgx.Row) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(
		&u.ID, &u.LegacyID, &u.Phone, &u.Nickname,
		&u.AvatarURL, &u.Gender, &u.Birthday, &u.RegionCode, &u.RegionName,
		&u.Status, &u.RegisterSource, &u.LastLoginAt, &u.LastLoginIP,
		&u.LoginCount, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}
