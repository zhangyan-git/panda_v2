package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// 这个文件是**合作方账号**的增、改、启停与列表。
//
// 三个写方法都是同一个三段式（与 payment-service 的 admin_config.go 逐字同形）：
//
//	事务 → `SELECT … FOR UPDATE` 取 before 快照 → INSERT / UPDATE → 取 after 快照 →
//	`recorder.Record` 记审计 → commit。
//
// before 必须在同一个事务里锁着读，不能由调用方传旧值：传进来的旧值来自「上一次读」与
// 「这一次写」之间，中间隔着别人的一次改动，那会记下一条库里从未出现过的 before。审计与
// 写入也必须同一个事务——写成功了审计没记，等于这次改动没有 operator；审计记了写回滚了，
// 操作日志里就有一条查无实据的改动。

// PartnerWrite 是一条合作方的写入值。
type PartnerWrite struct {
	// Code 只在新建时被采纳。修改路径上它只用来核对「你要改的是不是同一行」——UPDATE
	// 语句里根本没有这一列（见 UpdatePartner）。
	Code         string
	Name         string
	ContactName  string
	ContactPhone string
	ContactEmail string
	Description  string
	// ExpiresAt 为 nil 表示不过期（写成 SQL 的 NULL）。
	ExpiresAt *time.Time
}

// partnerSnapshot 是一条合作方在审计里的样子。
//
// 不把 model.PartnerAccount 直接丢给 audit.Snapshot：model 的字段没有 json tag（那些给 db
// 层用），序列化出来是一串大写开头的字段名，改一次字段名审计里的历史载荷就换了形状。
// created_at / updated_at 不进快照——它们是「这次改动」自己留下的痕迹，放进快照会让每一对
// before/after 都必然不同，那些差异对「改坏了好回滚」没有任何用。
//
// **这里没有密钥**，一个字段都没有：合作方的密钥是另一张表（partner_api_keys），而那张表的
// 快照连密文都不进（见 apikey.go）。
type partnerSnapshot struct {
	ID           string     `json:"id"`
	Code         string     `json:"code"`
	Name         string     `json:"name"`
	ContactName  string     `json:"contact_name"`
	ContactPhone string     `json:"contact_phone"`
	ContactEmail string     `json:"contact_email"`
	Description  string     `json:"description"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedBy    string     `json:"created_by"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// partnerColumns 是 partner_accounts 的读取列。列顺序与 scanPartner 的扫描顺序严格一一对应，
// 两边必须一起改。
const partnerColumns = `id::text, code, name, contact_name, contact_phone, contact_email,
	description, status, expires_at, created_by, created_at, updated_at`

func scanPartner(row scanner) (*model.PartnerAccount, error) {
	partner := &model.PartnerAccount{}
	err := row.Scan(&partner.ID, &partner.Code, &partner.Name, &partner.ContactName,
		&partner.ContactPhone, &partner.ContactEmail, &partner.Description, &partner.Status,
		&partner.ExpiresAt, &partner.CreatedBy, &partner.CreatedAt, &partner.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return partner, nil
}

// PartnerListRow 是列表里的一行：合作方加上它名下密钥的条数。
type PartnerListRow struct {
	Partner *model.PartnerAccount
	// KeyCount 含已停用的密钥（列表页要回答的是「这家有没有配过钥匙」）。
	KeyCount int64
}

// partnerListFrom 是列表与计数的同一个 FROM。
//
// LEFT JOIN 一个**预聚合的子查询**而不是直接 JOIN partner_api_keys：直接 JOIN 会让
// COUNT(*) 数的是「合作方×密钥」的行数而不是合作方条数——有三把钥匙的合作方会被数成三条，
// 而那个数在分页时直接决定第二页有没有数据。
//
// 子查询按 partner_id 分组，聚合列上走 partner_api_keys_partner_idx。
const partnerListFrom = ` FROM partner_accounts p
	LEFT JOIN (SELECT partner_id, COUNT(*) AS key_count FROM partner_api_keys GROUP BY partner_id) k
		ON k.partner_id = p.id`

// ListPartners 读一页合作方，按创建时间倒序。
//
// keyword 同时匹配编码与名称：运营手上要么是编码（对接方报过来的），要么是名字（他自己起的）。
// 两个 ILIKE 都吃不到索引，但合作方是**运营手工维护的一张小表**（几十到几百行），这里不需要
// 为查询性能做任何取舍。
func (r *PostgresRepository) ListPartners(ctx context.Context, q dto.PartnerQuery) ([]*PartnerListRow, int, error) {
	w := whereClause{}
	if q.Keyword != "" {
		// 同一个搜索词出现在两个 ILIKE 上。用 addMany 而不是把 pattern 拼进 SQL：拼进去就是
		// 一次注入（搜索框的内容直接进语句），而这里走的是两个独立参数。
		pattern := "%" + escapeLike(q.Keyword) + "%"
		w.addMany(`(p.code ILIKE $%d OR p.name ILIKE $%d)`, pattern, pattern)
	}
	if q.Status != "" {
		w.add(`p.status = $%d`, q.Status)
	}

	total, err := countRows(ctx, r.pool, partnerListFrom, w)
	if err != nil {
		return nil, 0, err
	}
	args, page := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("p", partnerColumns)+`, COALESCE(k.key_count, 0)`+
		partnerListFrom+w.sql()+` ORDER BY p.created_at DESC, p.id DESC`+page, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*PartnerListRow, 0, q.PageSize)
	for rows.Next() {
		item := &PartnerListRow{Partner: &model.PartnerAccount{}}
		p := item.Partner
		err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.ContactName, &p.ContactPhone,
			&p.ContactEmail, &p.Description, &p.Status, &p.ExpiresAt, &p.CreatedBy,
			&p.CreatedAt, &p.UpdatedAt, &item.KeyCount)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// FindPartner 按 id 读一条合作方。不存在时返回 ErrPartnerNotFound。
func (r *PostgresRepository) FindPartner(ctx context.Context, id string) (*model.PartnerAccount, error) {
	partner, err := scanPartner(r.pool.QueryRow(ctx,
		`SELECT `+partnerColumns+` FROM partner_accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPartnerNotFound
	}
	if err != nil {
		return nil, err
	}
	return partner, nil
}

// CreatePartner 新增一条合作方。
//
// 编码撞车交给库上的唯一键（partner_accounts_code_key → ErrPartnerCodeTaken → 409），这里
// 不先查一次：「查了没有」与「插的时候没有」之间隔着一次并发，多出来的那次往返只买到一点
// 更早的提示，买到的是一个看起来更确定、实际并不更确定的结果。
//
// created_by 由 service 从后台身份里取（见 service.CreatePartner），不是这里读上下文——仓储
// 只认参数。
func (r *PostgresRepository) CreatePartner(ctx context.Context, in PartnerWrite, createdBy string) (*model.PartnerAccount, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx, `INSERT INTO partner_accounts
		(code, name, contact_name, contact_phone, contact_email, description, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text`,
		in.Code, in.Name, in.ContactName, in.ContactPhone, in.ContactEmail,
		in.Description, in.ExpiresAt, createdBy).Scan(&id)
	if err != nil {
		return nil, mapPGError(err)
	}

	after, err := readPartnerSnapshot(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partners", Action: "create", Operation: "新增合作方",
		TargetType: "partner_account", TargetID: id, TargetName: after.Name,
		After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	created, err := scanPartner(tx.QueryRow(ctx, `SELECT `+partnerColumns+` FROM partner_accounts WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdatePartner 整份覆盖一条合作方。
//
// # 为什么 UPDATE 里没有 code 这一列
//
// 编码是合作方与调用日志、对接文档之间的稳定标识，改掉它等于换了一个合作方的身份。这张表上
// **没有触发器**，所以「不可修改」在这里只有一种实现：语句里根本不写它。请求里带着的 code
// 只用来核对——与库里那行不一致时返回 ErrPartnerCodeImmutable，让前端看到一句「编码不可
// 修改」，而不是「保存成功了但 code 没变」。
func (r *PostgresRepository) UpdatePartner(ctx context.Context, id string, in PartnerWrite) (*model.PartnerAccount, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := lockPartnerSnapshot(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if in.Code != "" && in.Code != before.Code {
		return nil, ErrPartnerCodeImmutable
	}

	if _, err := tx.Exec(ctx, `UPDATE partner_accounts SET
		name = $2, contact_name = $3, contact_phone = $4, contact_email = $5,
		description = $6, expires_at = $7, updated_at = NOW()
		WHERE id = $1`,
		id, in.Name, in.ContactName, in.ContactPhone, in.ContactEmail,
		in.Description, in.ExpiresAt); err != nil {
		return nil, mapPGError(err)
	}

	after, err := readPartnerSnapshot(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partners", Action: "update", Operation: "修改合作方",
		TargetType: "partner_account", TargetID: id, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	updated, err := scanPartner(tx.QueryRow(ctx, `SELECT `+partnerColumns+` FROM partner_accounts WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// SetPartnerStatus 启用或停用一条合作方。
//
// **停用立即生效**：验签那条路每个请求都现查这一行（没有缓存），停掉之后合作方名下的**所有**
// 密钥同时失效，不需要逐把去停。审计里也因此是 update_status 而不是 update——操作日志按
// 动作筛的时候，「这次事故是停用合作方停出来的」要能一眼看出来（那是一刀切掉一个客户的全部
// 调用，与改个联系方式不是一回事）。
func (r *PostgresRepository) SetPartnerStatus(ctx context.Context, id, status string) (*model.PartnerAccount, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	before, err := lockPartnerSnapshot(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE partner_accounts SET status = $2, updated_at = NOW() WHERE id = $1`, id, status); err != nil {
		return nil, mapPGError(err)
	}
	after, err := readPartnerSnapshot(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := r.recorder.Record(ctx, tx, audit.Entry{
		Module: "partners", Action: "update_status", Operation: partnerStatusOperation(status),
		TargetType: "partner_account", TargetID: id, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}

	updated, err := scanPartner(tx.QueryRow(ctx, `SELECT `+partnerColumns+` FROM partner_accounts WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

// lockPartnerSnapshot 锁住一行并读回它的快照。before 与 after 都走它（理由见 payment-service
// 的同名函数：两个方向共用一份实现就不会出现「before 读了 12 列、after 读了 11 列」）。
func lockPartnerSnapshot(ctx context.Context, tx pgx.Tx, id string) (*partnerSnapshot, error) {
	snapshot := &partnerSnapshot{}
	err := tx.QueryRow(ctx, `SELECT `+partnerColumns+`
		FROM partner_accounts WHERE id = $1 FOR UPDATE`, id).Scan(
		&snapshot.ID, &snapshot.Code, &snapshot.Name, &snapshot.ContactName,
		&snapshot.ContactPhone, &snapshot.ContactEmail, &snapshot.Description, &snapshot.Status,
		&snapshot.ExpiresAt, &snapshot.CreatedBy, &snapshot.CreatedAt, &snapshot.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPartnerNotFound
	}
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func readPartnerSnapshot(ctx context.Context, tx pgx.Tx, id string) (*partnerSnapshot, error) {
	return lockPartnerSnapshot(ctx, tx, id)
}

// partnerStatusOperation 是审计里的中文动作名。
//
// 按目标状态选词（而不是「修改状态」一句话）：操作日志是按动作筛的，「谁把这家合作方停掉了」
// 要能一眼筛出来，而「修改状态」四个字把启用和停用混成了同一条。
func partnerStatusOperation(status string) string {
	if status == string(model.StatusEnabled) {
		return "启用合作方"
	}
	return "停用合作方"
}
