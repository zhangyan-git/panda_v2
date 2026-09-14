package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminRoleRepository 平台角色数据访问接口
type AdminRoleRepository interface {
	// FindPage 返回一页角色；总数走 Count，两者不带筛选条件、必须始终对同一批行生效。
	FindPage(ctx context.Context, limit, offset int) ([]*model.AdminRole, error)
	Count(ctx context.Context) (int64, error)
	FindByID(ctx context.Context, id string) (*model.AdminRole, error)
	Create(ctx context.Context, r *model.AdminRole) error
	Update(ctx context.Context, r *model.AdminRole) error
	Delete(ctx context.Context, id string) error
}

// roleSnapshot 是写入审计 before_data / after_data 的形状。
//
// 显式列字段而不是序列化 model.AdminRole，有两条理由：模型的字段只有 db tag，
// 直接序列化写出的是 Go 字段名；而且同一个包里的 AdminUser 带着 password_hash——
// 审计快照一旦习惯性照抄模型，迟早会把口令散列写进日志表。
type roleSnapshot struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func snapshotOfRole(role *model.AdminRole) roleSnapshot {
	return roleSnapshot{Code: role.Code, Name: role.Name, Description: role.Description}
}

type pgAdminRoleRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewAdminRoleRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminRoleRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminRoleRepo{pool: pool, audit: recorder}
}

// FindPage 以 id 作排序决胜位，理由同 adminUserRepo.FindPage：时间相同的行
// 没有稳定次序，翻页会重复或漏行。
func (r *pgAdminRoleRepo) FindPage(ctx context.Context, limit, offset int) ([]*model.AdminRole, error) {
	// description 可空且没有默认值，而模型里是 string：NULL 直接 Scan 会报错。
	// 接口建角色总会写 ''，只有手工 SQL 造的数据会留 NULL——那时候错的是登录/列表，
	// 看不出跟角色有关。这里按空串读，与 menu.go、merchant_user.go 的写法一致。
	const q = `
		SELECT id, code, name, COALESCE(description, ''), created_at, updated_at
		FROM admin_roles
		ORDER BY created_at, id
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminRole
	for rows.Next() {
		role := &model.AdminRole{}
		if err := rows.Scan(&role.ID, &role.Code, &role.Name, &role.Description, &role.CreatedAt, &role.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, role)
	}
	return list, rows.Err()
}

func (r *pgAdminRoleRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM admin_roles`).Scan(&n)
	return n, err
}

func (r *pgAdminRoleRepo) FindByID(ctx context.Context, id string) (*model.AdminRole, error) {
	const q = `
		SELECT id, code, name, COALESCE(description, ''), created_at, updated_at
		FROM admin_roles WHERE id = $1`
	role := &model.AdminRole{}
	err := r.pool.QueryRow(ctx, q, id).Scan(
		&role.ID, &role.Code, &role.Name, &role.Description, &role.CreatedAt, &role.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return role, nil
}

func (r *pgAdminRoleRepo) Create(ctx context.Context, role *model.AdminRole) error {
	if err := model.ValidateRoleCodeChange("", role.Code); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, q,
		role.ID, role.Code, role.Name, role.Description, role.CreatedAt, role.UpdatedAt,
	); err != nil {
		return roleCodeError(err)
	}
	// 角色的唯一 code 由 admin_roles_code_key 保证，冲突在 roleCodeError 里转成
	// ErrRoleCodeConflict。
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "create", Operation: "新增角色",
		TargetType: "role", TargetID: role.ID, TargetName: role.Name,
		After: audit.Snapshot(snapshotOfRole(role)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminRoleRepo) Update(ctx context.Context, role *model.AdminRole) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// 读整行而不只是 code：锁已经拿到了，审计的 before 快照顺带取出来，
	// 不必在审计时再查一次（那时数据已被本次更新覆盖，也就取不到了）。
	before := &model.AdminRole{}
	if err := tx.QueryRow(ctx,
		`SELECT id, code, name, COALESCE(description, ''), created_at, updated_at FROM admin_roles WHERE id = $1 FOR UPDATE`,
		role.ID,
	).Scan(&before.ID, &before.Code, &before.Name, &before.Description, &before.CreatedAt, &before.UpdatedAt); err != nil {
		return err
	}
	oldCode := before.Code
	if err := model.ValidateRoleCodeChange(oldCode, role.Code); err != nil {
		return err
	}
	const q = `
		UPDATE admin_roles
		SET code = $1, name = $2, description = $3, updated_at = $4
		WHERE id = $5`
	if _, err := tx.Exec(ctx, q, role.Code, role.Name, role.Description, role.UpdatedAt, role.ID); err != nil {
		return roleCodeError(err)
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "update", Operation: "修改角色",
		TargetType: "role", TargetID: role.ID, TargetName: role.Name,
		Before: audit.Snapshot(snapshotOfRole(before)), After: audit.Snapshot(snapshotOfRole(role)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func roleCodeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "admin_roles_code_key" {
		return fmt.Errorf("%w: %w", model.ErrRoleCodeConflict, err)
	}
	return err
}

// 这里曾经有个 checkRolePolicyConflict：新建/改名角色时拒绝 code 与 casbin_rule 里的
// 残留规则同名。它的存在前提是「策略表可能带着一个已不存在角色的规则」——那是双写漂移
// 的产物。策略现在从 admin_roles 派生，规则随角色行生灭，残留规则既不存在也不可能生效，
// 检查因此没有意义：留着反而会让一条谁也删不掉的垃圾行永久占住一个角色码。
func (r *pgAdminRoleRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 删除之后行就没了，before 快照只能在删之前取。取不到说明这个角色本来就不存在：
	// 保持原来的语义（删不存在的角色不算错误），只是这次没有可记的操作。
	before := &model.AdminRole{}
	err = tx.QueryRow(ctx,
		`SELECT id, code, name, COALESCE(description, ''), created_at, updated_at FROM admin_roles WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&before.ID, &before.Code, &before.Name, &before.Description, &before.CreatedAt, &before.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_roles WHERE id = $1`, id); err != nil {
		return err
	}
	// 该角色的授权随这一行一起消失：admin_role_permissions 与 admin_user_role_bindings
	// 都有 ON DELETE CASCADE，而策略正是从这两张表派生的，不需要再去清理别的地方。
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "delete", Operation: "删除角色",
		TargetType: "role", TargetID: id, TargetName: before.Name,
		Before: audit.Snapshot(snapshotOfRole(before)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
