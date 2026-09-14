package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// SMSCodeRepository 是短信验证码的数据访问接口。
//
// 一个手机号同时只有一条记录，主键就是 phone：重发是覆盖而不是新增，所以
// 「用户刚收到的那条」永远只有一条，不存在旧码还在有效期内被猜中的窗口。
// 这条记录的生存期由 expires_at 决定，过期行在下一次 Upsert 时被顺手覆盖，
// 不需要清理任务——行数上限是「曾经请求过验证码的手机号数」。
type SMSCodeRepository interface {
	// UpsertSMSCode 写入一条新验证码，覆盖该手机号原有的记录并把失败次数清零。
	// 注意它不保留旧码：用户点了重发之后，上一条立即失效。
	//
	// cutoff 是重发频控的落点：只有原记录的 created_at 早于它时才允许覆盖，
	// 否则返回 pgx.ErrNoRows。这个判断必须写在语句里而不是由调用方先读后判——
	// 并发请求会同时读到「刚发过/没发过」的同一个旧值，各写各的，60 秒一条
	// 就退化成「每个并发一次」。放在 ON CONFLICT 的 WHERE 里，它和行锁一起
	// 变成原子的，并发下只有一条能写进去。
	UpsertSMSCode(ctx context.Context, c *model.UserSMSCode, cutoff time.Time) error
	// FindSMSCode 取该手机号当前的验证码记录，不存在返回 pgx.ErrNoRows。
	FindSMSCode(ctx context.Context, phone string) (*model.UserSMSCode, error)
	// IncrementSMSCodeAttempts 原子地把失败次数加一，并返回加完之后的值。
	//
	// 必须由数据库做加法并返回结果，不能让调用方「读出来 +1 再写回去」：
	// 并发猜测下每个请求都读到同一个旧值，各自写回同一个新值，五次上限会退化成
	// 「每个并发请求都有五次」。返回值让调用方知道这一票是第几次、是否已经越界。
	//
	// 调用方必须**先自增、再比对验证码**——顺序反过来的话，越界的那些请求
	// 在自增之前就已经把码验完了，上限同样拦不住。
	IncrementSMSCodeAttempts(ctx context.Context, phone string) (int, error)
	// DeleteSMSCode 消费掉验证码。返回 pgx.ErrNoRows 表示这行已经不在了——
	// 并发提交同一个正确验证码时只有一方能删掉它，另一方据此判定为已失效。
	DeleteSMSCode(ctx context.Context, phone string) error
}

type pgSMSCodeRepo struct {
	pool *pgxpool.Pool
}

// NewSMSCodeRepository 构造短信验证码仓库。
func NewSMSCodeRepository(pool *pgxpool.Pool) SMSCodeRepository {
	return &pgSMSCodeRepo{pool: pool}
}

func (r *pgSMSCodeRepo) UpsertSMSCode(ctx context.Context, c *model.UserSMSCode, cutoff time.Time) error {
	// purpose 也跟着覆盖：同一个手机号先用 login 拿码，再改用 bind_phone 拿码，
	// 库里的用途必须是最后一次请求的那个，否则校验时会对错了用途而放行。
	//
	// WHERE 挂在 DO UPDATE 上（而不是 ON CONFLICT 之后）：这样它约束的是冲突
	// 分支，首次插入不受频控影响——没有原记录就没有「发得太频繁」可言。
	const q = `
		INSERT INTO user_sms_codes (phone, purpose, code_hash, attempts, expires_at, created_at)
		VALUES ($1, $2, $3, 0, $4, $5)
		ON CONFLICT (phone) DO UPDATE
		SET purpose = EXCLUDED.purpose, code_hash = EXCLUDED.code_hash,
			attempts = 0, expires_at = EXCLUDED.expires_at, created_at = EXCLUDED.created_at
		WHERE user_sms_codes.created_at <= $6`
	tag, err := r.pool.Exec(ctx, q, c.Phone, c.Purpose, c.CodeHash, c.ExpiresAt, c.CreatedAt, cutoff)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *pgSMSCodeRepo) FindSMSCode(ctx context.Context, phone string) (*model.UserSMSCode, error) {
	const q = `
		SELECT phone, purpose, code_hash, attempts, expires_at, created_at
		FROM user_sms_codes
		WHERE phone = $1
		LIMIT 1`
	c := &model.UserSMSCode{}
	err := r.pool.QueryRow(ctx, q, phone).Scan(
		&c.Phone, &c.Purpose, &c.CodeHash, &c.Attempts, &c.ExpiresAt, &c.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (r *pgSMSCodeRepo) IncrementSMSCodeAttempts(ctx context.Context, phone string) (int, error) {
	const q = `UPDATE user_sms_codes SET attempts = attempts + 1 WHERE phone = $1 RETURNING attempts`
	var attempts int
	err := r.pool.QueryRow(ctx, q, phone).Scan(&attempts)
	if err != nil {
		return 0, err
	}
	return attempts, nil
}

func (r *pgSMSCodeRepo) DeleteSMSCode(ctx context.Context, phone string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM user_sms_codes WHERE phone = $1`, phone)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
