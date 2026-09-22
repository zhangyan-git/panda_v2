package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
)

// 这个文件是分账后台的读写路径：规则（连同它的项）、接收方账户，以及只读的任务与明细。
//
// # 与 settlement.go 的分工
//
// 那个文件是**执行**路径：发起支付时命中规则、算金额、把计划写进任务表。它读的是「命中哪一条
// 规则、这一项有没有可用账户」这几个为计算而生的字段。
//
// 这个文件是**配置**路径：人在后台一栏一栏地填，读回来的行要多带展示与审计要的列（创建时间、
// 备注、主体名、账户的展示信息），而且要能整体替换一条规则的项。两边共用同一批表，但**不共用
// 行类型**——执行那边只取它算得动的几列，硬凑成一个类型会让「哪些列是计算需要、哪些是给人看
// 的」这件事再也看不出来。
//
// # 规则的项为什么整体替换
//
// 「同一条规则下比例合计 ≤ 1、平台项至多一条」这类约束是**跨行**的：拆成子资源逐行改，就没有
// 任何一个时刻能在一个事务里看见完整的一套项，也就校验不了。所以规则带 items 整体提交，本文件
// 在同一个事务里删掉旧项、写上新项。

// 分账后台的错误值。与 payment 那一族的 ErrPaymentNotFound 并列：controller 按它们翻状态码。
var (
	// ErrSettlementRuleNotFound / ErrSettlementAccountNotFound / ErrSettlementTaskNotFound：
	// 查无此规则 / 账户 / 任务。
	ErrSettlementRuleNotFound    = errors.New("settlement rule not found")
	ErrSettlementAccountNotFound = errors.New("settlement account not found")
	ErrSettlementTaskNotFound    = errors.New("settlement task not found")
	// ErrSettlementRuleInUse / ErrSettlementAccountInUse：被引用过，删不掉（外键 RESTRICT）。
	//
	// 这是一个**正常的业务结论**而不是事故：改过的规则会被历史任务指着（任务上存的是当初命中
	// 的那条规则的 id），账户会被规则项与历史接收方指着。出口是停用，不是删除。
	ErrSettlementRuleInUse    = errors.New("settlement rule is referenced by settlement tasks")
	ErrSettlementAccountInUse = errors.New("settlement account is referenced by rules or receivers")
	// ErrSettlementScopeConflict：同一业务分类 + 同一档位 + 同一范围上已经有一条启用中的规则。
	//
	// 那条部分唯一索引（settlement_rules_scope_uniq）是「命中哪一条」这件事唯一说得清的依据：
	// 两条同时启用时命中哪条取决于查询的返回顺序。**停用之后可以再配一条同档位的**——索引带
	// WHERE status='enabled'，这句话要出现在页面的报错里。
	ErrSettlementScopeConflict = errors.New("an enabled settlement rule already covers this scope")
	// ErrSettlementAccountConflict：同一渠道下这个接收方号已经被别的启用账户登记
	// （settlement_accounts_receiver_uniq）——017 之后这是账户表上唯一的一条唯一约束。
	ErrSettlementAccountConflict = errors.New("settlement account conflicts with an existing one")
	// ErrSettlementRuleItemConflict：同一账户在同一条规则里出现了两次。
	ErrSettlementRuleItemConflict = errors.New("the same account appears twice in one rule")
	// ErrSettlementAccountChannelInUse：这个账户正被启用中的规则引用着，改渠道会让那些规则
	// **静默少分一笔**。
	//
	// 外键只兜得住删除（三处全是 ON DELETE RESTRICT），**兜不住改渠道**——而改渠道的后果与
	// 删掉一样重：规则项在执行期是按 `a.provider = 这笔支付走的那条渠道` 做 LEFT JOIN 的，
	// 账户一换渠道，那一项在任何一笔支付上都被滤掉，钱按差额倒挤回平台，页面上一点异常都
	// 看不出来。更别扭的是那条规则此后**再也存不回去**：service 的同渠道校验会拿同一条规则
	// 里几个账户的 provider 互相比，一保存就回「必须在同一条渠道上」——用户被锁在一个改不动
	// 也存不下的状态里。
	//
	// 出口是**先改规则**：把引用它的那几项指向新渠道上的账户（或者先把那条规则停用），再回来
	// 改渠道。所以它是 409 而不是 400——同一个请求换个时间点就能成功。
	//
	// **停用（enabled → disabled）不在这条校验里**：它同样是「这一项从此命不中」，但停用是
	// 这个域里唯一的退场动作（删不掉，见 ErrSettlementAccountInUse 的说明），把它也拦掉会让
	// 一个配错的账户彻底没有出口。那一半是已知且有意留着的问题。
	ErrSettlementAccountChannelInUse = errors.New("the account is referenced by enabled settlement rules")
)

// settlementAccountColumns 是账户在后台的形状。**不含 legacy_id**：那是老库 ObjectID 的对照键，
// 只对迁移过来的行有意义，页面上没有它的位置。
//
// 没有 channel_id 这一列了：渠道与支付方式在 009 之后是代码里的目录，账户上存的是渠道名
// （provider）。账户上也没有钱，所以没有金额列。
const settlementAccountColumns = `id::text, party_name, party_type,
	provider, receiver_type, receiver_id,
	status, remark, created_at, updated_at`

// settlementRuleColumns 是规则在后台的形状。**没有 legacy_id**，理由同账户。
//
// 规则上没有渠道列——它有不了：一条规则按业务分类与范围命中，同一套分法在不同渠道下都成立，
// 而「钱分给谁」的账户才带渠道。这件事的取舍写在 SettlementRuleWrite 的注释里。
const settlementRuleColumns = `id::text, name, biz_type, scope_type, scope_ref, allocation_mode,
	status, remark, created_at, updated_at`

// settlementRuleItemColumns 是规则项的形状。
//
// ratio 出来的是**百分数的百分之一**（ratio × 10000，整数）：列是 NUMERIC(20,6)，直接用 float8
// 取出来会有 44.999999999 这种尾数，而它要原样显示在页面那个百分比输入框里。整数换算在 SQL 里
// 做（numeric 运算是精确的），到了 Go 这边只剩除以 100。
const settlementRuleItemColumns = `i.id::text, i.party_type, i.calc_type,
	(i.ratio * 10000)::bigint, i.fixed_amount, i.sort_order, i.remark,
	COALESCE(i.account_id::text, ''), COALESCE(a.party_name, ''), COALESCE(a.receiver_id, '')`

// SettlementAccountRow 是一个接收方账户在后台的完整形状。
//
// 没有 account_no / receiver_name：两列在 017 里删了（见那条迁移的文件头）。这一行的名字就是
// PartyName，渠道那边要的号是 ReceiverID。
type SettlementAccountRow struct {
	ID           string
	PartyName    string
	PartyType    string
	Provider     string
	ReceiverType string
	ReceiverID   string
	Status       string
	Remark       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func scanSettlementAccount(row scanner) (*SettlementAccountRow, error) {
	account := &SettlementAccountRow{}
	if err := row.Scan(&account.ID, &account.PartyName, &account.PartyType,
		&account.Provider,
		&account.ReceiverType, &account.ReceiverID,
		&account.Status, &account.Remark, &account.CreatedAt, &account.UpdatedAt); err != nil {
		return nil, mapSettlementError(err, ErrSettlementAccountNotFound, ErrSettlementAccountInUse)
	}
	return account, nil
}

// SettlementRuleRow 是一条规则连同它的项。
//
// Items 在**列表**里是空的：列表页只显示规则本身，带上项意味着每页 20 条要多跑 20 次子查询
// （或者一次 IN 查询加分组），而那些列一个都用不到项。详情与编辑表单才需要它。
type SettlementRuleRow struct {
	ID             string
	Name           string
	BizType        string
	ScopeType      string
	ScopeRef       string
	AllocationMode string
	Status         string
	Remark         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Items          []SettlementRuleItemRow
}

// SettlementRuleItemRow 是规则里的一项，带上账户的展示名。
//
// RatioHundredths 是「百分数 × 100」的整数：45.00% 是 4500。用整数而不是 float，理由与金额
// 一律 int64 分相同——它是钱的乘数，浮点尾差会一路走到分账明细上。
type SettlementRuleItemRow struct {
	ID              string
	PartyType       string
	CalcType        string
	RatioHundredths int64
	FixedAmount     int64
	SortOrder       int
	Remark          string
	AccountID       string
	AccountName     string
	AccountReceiver string
}

// SettlementRuleWrite 是一次创建或整体更新一条规则的输入。
//
// # 为什么规则上没有渠道
//
// 规则回答的是「这一类业务、这个范围上的钱怎么分」，而一笔钱走哪条渠道是发起支付那一刻才定下来
// 的（订单侧选的方式）。同一档规则在银联商务与微信上都可以成立，把它钉死在一条渠道上等于同一套
// 分法要按渠道配两遍。
//
// **渠道关系统一落在账户上**，规则因此只要求一件事：同一条规则里所有非平台项的账户必须在**同
// 一条渠道**上。这条要求不是洁癖——分账指令是随某一条渠道的支付下发的，接收方号也只在那条渠道
// 里认；一条规则里挂着两条渠道的账户，意味着其中一半在任何一笔支付上都会被过滤掉（见
// settlementRuleItems 的 JOIN 条件），而那种过滤在数据上完全看不见，表现是「这家门店的钱总是
// 分少了」。所以它由 service 层在写入口挡下来。
type SettlementRuleWrite struct {
	Name           string
	BizType        string
	ScopeType      string
	ScopeRef       string
	AllocationMode string
	Status         string
	Remark         string
	Items          []SettlementRuleItemWrite
}

// SettlementRuleItemWrite 是规则项的一次写入。
//
// RatioHundredths 与 SettlementRuleItemRow 同名同义（百分数 × 100）。AccountID 为空表示平台项
// ——008 的 CHECK 把「平台项没有账户」写死了，别的项没账户会被数据库拒掉。
type SettlementRuleItemWrite struct {
	PartyType       string
	CalcType        string
	RatioHundredths int64
	FixedAmount     int64
	SortOrder       int
	Remark          string
	AccountID       string
}

// SettlementAccountWrite 是一次创建或整体更新一个账户的输入。
type SettlementAccountWrite struct {
	PartyName    string
	PartyType    string
	Provider     string
	ReceiverType string
	ReceiverID   string
	Status       string
	Remark       string
}

// ============================================================
// 账户
// ============================================================

// ListSettlementAccounts 读一页账户，按创建时间倒序。
func (r *PostgresRepository) ListSettlementAccounts(ctx context.Context, q dto.SettlementAccountQuery) ([]*SettlementAccountRow, int, error) {
	const from = ` FROM settlement_accounts a`
	w := whereClause{}
	if q.Keyword != "" {
		// 一个关键词搜两列：运营分不清手上那串是主体名还是子商户号，而两列各建一个框只会让人每个
		// 都试一遍。同一个值占两个占位符，序号由 whereClause.add 算（见那里的说明）。
		pattern := "%" + escapeLike(q.Keyword) + "%"
		w.add(`(a.party_name ILIKE $%d OR a.receiver_id ILIKE $%d)`, pattern, pattern)
	}
	if q.PartyType != "" {
		w.add(`a.party_type = $%d`, q.PartyType)
	}
	if q.Provider != "" {
		w.add(`a.provider = $%d`, q.Provider)
	}
	if q.Status != "" {
		w.add(`a.status = $%d`, q.Status)
	}

	total, err := countRows(ctx, r.pool, from, w)
	if err != nil {
		return nil, 0, err
	}
	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("a", settlementAccountColumns)+from+
		w.sql()+` ORDER BY a.created_at DESC, a.id DESC`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*SettlementAccountRow, 0, q.PageSize)
	for rows.Next() {
		account, err := scanSettlementAccount(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, account)
	}
	return items, total, rows.Err()
}

// GetSettlementAccount 按 id 读一个账户。
func (r *PostgresRepository) GetSettlementAccount(ctx context.Context, id string) (*SettlementAccountRow, error) {
	return scanSettlementAccount(r.pool.QueryRow(ctx,
		`SELECT `+settlementAccountColumns+` FROM settlement_accounts WHERE id = $1::uuid`, id))
}

// GetSettlementAccounts 一次读多个账户，给「校验一条规则的每一项挂的账户」用。
//
// 一次 IN 查询而不是每条项各查一次：一条规则的项是个位数，但校验要同时知道「有没有这个
// 账户」「它启没启用」「它在哪条渠道上」——分三次问同一行不如一次取回来。**返回的顺序不作
// 保证**，调用方按 id 建索引自己找。
//
// 查不到的 id 不会出现在结果里，也**不在这里报错**：哪一项挂了一个不存在的账户，是调用方
// （service 层）要拿着项去对的事，这一层只知道「这些 id 里有这么几行」。
func (r *PostgresRepository) GetSettlementAccounts(ctx context.Context, ids []string) ([]*SettlementAccountRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+settlementAccountColumns+` FROM settlement_accounts WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := make([]*SettlementAccountRow, 0, len(ids))
	for rows.Next() {
		account, err := scanSettlementAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// CreateSettlementAccount 建一个账户，并与审计写在同一个事务里。
func (r *PostgresRepository) CreateSettlementAccount(ctx context.Context, in SettlementAccountWrite) (*SettlementAccountRow, error) {
	var created *SettlementAccountRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO settlement_accounts
			(party_name, party_type, provider, receiver_type, receiver_id, status, remark)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING id::text`,
			in.PartyName, in.PartyType,
			in.Provider, in.ReceiverType, in.ReceiverID,
			in.Status, in.Remark).Scan(&id); err != nil {
			return mapSettlementError(err, ErrSettlementAccountNotFound, ErrSettlementAccountInUse)
		}
		row, err := scanSettlementAccount(tx.QueryRow(ctx,
			`SELECT `+settlementAccountColumns+` FROM settlement_accounts WHERE id = $1::uuid`, id))
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "create", Operation: "新增分账账户",
			TargetType: "settlement_account", TargetID: row.ID, TargetName: row.PartyName,
			After: audit.Snapshot(accountSnapshotOf(row)),
		}); err != nil {
			return err
		}
		created = row
		return nil
	})
	return created, err
}

// UpdateSettlementAccount 整体更新一个账户，before / after 都进审计。
func (r *PostgresRepository) UpdateSettlementAccount(ctx context.Context, id string, in SettlementAccountWrite) (*SettlementAccountRow, error) {
	var updated *SettlementAccountRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// 先锁住再读：并发两次保存各自读到的都是旧值，而两次写入会一前一后覆盖，没有报错。
		before, err := scanSettlementAccount(tx.QueryRow(ctx,
			`SELECT `+settlementAccountColumns+` FROM settlement_accounts WHERE id = $1::uuid FOR UPDATE`, id))
		if err != nil {
			return err
		}
		// 换渠道之前看看谁在指着它——理由见 ErrSettlementAccountChannelInUse。
		//
		// 这一步放在事务里、挨着上面那句 FOR UPDATE，而不是搬到 service 层：判断要用的是
		// 加锁读出来的那一行（`before.Provider`），搬上去就变成「读一次、再写一次」两次独立
		// 往返，中间那一小段里别人正好也来改一次的话，比的就是一个已经不成立的旧值了。
		if in.Provider != before.Provider {
			referencing, err := countEnabledRulesUsingAccount(ctx, tx, id)
			if err != nil {
				return err
			}
			if referencing > 0 {
				return fmt.Errorf("%w: %d enabled rule(s) reference it, %s -> %s",
					ErrSettlementAccountChannelInUse, referencing, before.Provider, in.Provider)
			}
		}
		row, err := scanSettlementAccount(tx.QueryRow(ctx, `UPDATE settlement_accounts
			SET party_name=$2, party_type=$3, provider=$4, receiver_type=$5,
			    receiver_id=$6, status=$7, remark=$8, updated_at=NOW()
			WHERE id=$1::uuid
			RETURNING `+settlementAccountColumns,
			id, in.PartyName, in.PartyType,
			in.Provider, in.ReceiverType, in.ReceiverID,
			in.Status, in.Remark))
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "update", Operation: "修改分账账户",
			TargetType: "settlement_account", TargetID: row.ID, TargetName: row.PartyName,
			Before: audit.Snapshot(accountSnapshotOf(before)), After: audit.Snapshot(accountSnapshotOf(row)),
		}); err != nil {
			return err
		}
		updated = row
		return nil
	})
	return updated, err
}

// countEnabledRulesUsingAccount 数一数有几条**启用中的**规则挂着这个账户。
//
// 只数启用中的：停用的规则本来就不会命中，换渠道影响不到它。历史接收方
// （settlement_receivers）也不算：那是建任务那一刻的快照，里头存的就是当初那个子商户号，
// 账户后来换到哪条渠道与它无关。
func countEnabledRulesUsingAccount(ctx context.Context, tx pgx.Tx, accountID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM settlement_rule_items i
		JOIN settlement_rules r ON r.id = i.rule_id
		WHERE i.account_id = $1::uuid AND r.status = 'enabled'`, accountID).Scan(&count)
	return count, err
}

// DeleteSettlementAccount 删一个账户，被引用时回 ErrSettlementAccountInUse。
//
// 账户被两处指着：规则项（settlement_rule_items.account_id）与历史接收方
// （settlement_receivers.account_id），两处都是 ON DELETE RESTRICT。被接收方指着是常态——那个
// 账户真的分过钱，删掉它会让历史明细指向一个不存在的主体。出口是停用（status=disabled）：停用
// 之后不再参与新分账，历史一行不动。
func (r *PostgresRepository) DeleteSettlementAccount(ctx context.Context, id string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanSettlementAccount(tx.QueryRow(ctx,
			`SELECT `+settlementAccountColumns+` FROM settlement_accounts WHERE id = $1::uuid FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM settlement_accounts WHERE id = $1::uuid`, id); err != nil {
			return mapSettlementError(err, ErrSettlementAccountNotFound, ErrSettlementAccountInUse)
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "delete", Operation: "删除分账账户",
			TargetType: "settlement_account", TargetID: before.ID, TargetName: before.PartyName,
			Before: audit.Snapshot(accountSnapshotOf(before)),
		})
	})
}

// settlementAccountSnapshot 是账户进审计快照的形状：**不含两个时间戳**。它们每次写都在变，
// 带上只会让 before / after 的差异看上去像一次改动。
type settlementAccountSnapshot struct {
	PartyName    string `json:"party_name"`
	PartyType    string `json:"party_type"`
	Provider     string `json:"provider"`
	ReceiverType string `json:"receiver_type"`
	ReceiverID   string `json:"receiver_id"`
	Status       string `json:"status"`
	Remark       string `json:"remark"`
}

func accountSnapshotOf(row *SettlementAccountRow) settlementAccountSnapshot {
	if row == nil {
		return settlementAccountSnapshot{}
	}
	return settlementAccountSnapshot{
		PartyName: row.PartyName, PartyType: row.PartyType,
		Provider: row.Provider, ReceiverType: row.ReceiverType, ReceiverID: row.ReceiverID,
		Status: row.Status, Remark: row.Remark,
	}
}

// ============================================================
// 规则
// ============================================================

// ListSettlementRules 读一页规则，按创建时间倒序。**不带项**（见 SettlementRuleRow）。
func (r *PostgresRepository) ListSettlementRules(ctx context.Context, q dto.SettlementRuleQuery) ([]*SettlementRuleRow, int, error) {
	const from = ` FROM settlement_rules r`
	w := whereClause{}
	if q.Name != "" {
		w.add(`r.name ILIKE $%d`, "%"+escapeLike(q.Name)+"%")
	}
	if q.BizType != "" {
		w.add(`r.biz_type = $%d`, q.BizType)
	}
	if q.ScopeType != "" {
		w.add(`r.scope_type = $%d`, q.ScopeType)
	}
	if q.Status != "" {
		w.add(`r.status = $%d`, q.Status)
	}

	total, err := countRows(ctx, r.pool, from, w)
	if err != nil {
		return nil, 0, err
	}
	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("r", settlementRuleColumns)+from+
		w.sql()+` ORDER BY r.created_at DESC, r.id DESC`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*SettlementRuleRow, 0, q.PageSize)
	for rows.Next() {
		rule := &SettlementRuleRow{}
		if err := rows.Scan(&rule.ID, &rule.Name, &rule.BizType, &rule.ScopeType, &rule.ScopeRef,
			&rule.AllocationMode, &rule.Status, &rule.Remark,
			&rule.CreatedAt, &rule.UpdatedAt); err != nil {
			return nil, 0, mapSettlementError(err, ErrSettlementRuleNotFound, ErrSettlementRuleInUse)
		}
		rule.Items = []SettlementRuleItemRow{}
		items = append(items, rule)
	}
	return items, total, rows.Err()
}

// GetSettlementRule 按 id 读一条规则连同它的项。
func (r *PostgresRepository) GetSettlementRule(ctx context.Context, id string) (*SettlementRuleRow, error) {
	rule, err := readSettlementRule(ctx, r.pool, id, false)
	if err != nil {
		return nil, err
	}
	return rule, nil
}

// CreateSettlementRule 建一条规则与它的项，全部在一个事务里。
func (r *PostgresRepository) CreateSettlementRule(ctx context.Context, in SettlementRuleWrite) (*SettlementRuleRow, error) {
	var created *SettlementRuleRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO settlement_rules
			(name, biz_type, scope_type, scope_ref, allocation_mode, status, remark)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
			in.Name, in.BizType, in.ScopeType, in.ScopeRef, in.AllocationMode,
			in.Status, in.Remark).Scan(&id); err != nil {
			return mapSettlementError(err, ErrSettlementRuleNotFound, ErrSettlementRuleInUse)
		}
		if err := insertSettlementRuleItems(ctx, tx, id, in.Items); err != nil {
			return err
		}
		row, err := readSettlementRule(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "create", Operation: "新增分账规则",
			TargetType: "settlement_rule", TargetID: row.ID, TargetName: row.Name,
			After: audit.Snapshot(ruleSnapshotOf(row)),
		}); err != nil {
			return err
		}
		created = row
		return nil
	})
	return created, err
}

// UpdateSettlementRule 整体更新一条规则**连同它的项**：旧项全删、新项全写。
//
// 整体替换而不是逐项 diff：项没有稳定的业务键（同一条规则下同一个账户只能出现一次，但改比例
// 就是同一行的另一次写入），逐项对账要写一堆「这一项是新的、那一项被删了」的判据，而两种写法的
// 最终状态是一样的。**已经建好的任务不受影响**——它们存的是快照（金额、比例、主体名、子商户
// 号），不是对项的引用。
func (r *PostgresRepository) UpdateSettlementRule(ctx context.Context, id string, in SettlementRuleWrite) (*SettlementRuleRow, error) {
	var updated *SettlementRuleRow
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// FOR UPDATE：读旧值的同时把它锁住。并发的两次保存会各自删掉对方刚写进去的项，而两边
		// 都不会报错——这一句让第二次保存排队，它看到的是第一次保存后的完整状态。
		before, err := readSettlementRule(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE settlement_rules
			SET name=$2, biz_type=$3, scope_type=$4, scope_ref=$5, allocation_mode=$6,
			    status=$7, remark=$8, updated_at=NOW()
			WHERE id=$1::uuid`,
			id, in.Name, in.BizType, in.ScopeType, in.ScopeRef, in.AllocationMode,
			in.Status, in.Remark); err != nil {
			return mapSettlementError(err, ErrSettlementRuleNotFound, ErrSettlementRuleInUse)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM settlement_rule_items WHERE rule_id = $1::uuid`, id); err != nil {
			return err
		}
		if err := insertSettlementRuleItems(ctx, tx, id, in.Items); err != nil {
			return err
		}
		row, err := readSettlementRule(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "update", Operation: "修改分账规则",
			TargetType: "settlement_rule", TargetID: row.ID, TargetName: row.Name,
			Before: audit.Snapshot(ruleSnapshotOf(before)), After: audit.Snapshot(ruleSnapshotOf(row)),
		}); err != nil {
			return err
		}
		updated = row
		return nil
	})
	return updated, err
}

// DeleteSettlementRule 删一条规则，被任务引用时回 ErrSettlementRuleInUse。
//
// settlement_tasks.rule_id 是 ON DELETE RESTRICT：有任务指着它就只能停用。停用还有一个副作用是
// 运营需要的——settlement_rules_scope_uniq 带 WHERE status='enabled'，停用之后同一个档位可以再
// 配一条。
func (r *PostgresRepository) DeleteSettlementRule(ctx context.Context, id string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := readSettlementRule(ctx, tx, id, true)
		if err != nil {
			return err
		}
		// 项是 ON DELETE CASCADE，跟着走。
		if _, err := tx.Exec(ctx, `DELETE FROM settlement_rules WHERE id = $1::uuid`, id); err != nil {
			return mapSettlementError(err, ErrSettlementRuleNotFound, ErrSettlementRuleInUse)
		}
		return r.recorder.Record(ctx, tx, audit.Entry{
			Module: "payment", Action: "delete", Operation: "删除分账规则",
			TargetType: "settlement_rule", TargetID: before.ID, TargetName: before.Name,
			Before: audit.Snapshot(ruleSnapshotOf(before)),
		})
	})
}

// readSettlementRule 在给定的 querier 上读一条规则连同它的项。
//
// forUpdate 只对事务有意义：写路径要用它把规则行锁住（见 UpdateSettlementRule 的说明），读路径
// 传 false——一个只读快照事务里的 FOR UPDATE 会因为 AccessMode=ReadOnly 直接报错。
func readSettlementRule(ctx context.Context, q querier, id string, forUpdate bool) (*SettlementRuleRow, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	rule := &SettlementRuleRow{}
	if err := q.QueryRow(ctx, `SELECT `+settlementRuleColumns+`
		FROM settlement_rules WHERE id = $1::uuid`+lock, id).
		Scan(&rule.ID, &rule.Name, &rule.BizType, &rule.ScopeType, &rule.ScopeRef,
			&rule.AllocationMode, &rule.Status, &rule.Remark,
			&rule.CreatedAt, &rule.UpdatedAt); err != nil {
		return nil, mapSettlementError(err, ErrSettlementRuleNotFound, ErrSettlementRuleInUse)
	}
	items, err := settlementRuleItemRows(ctx, q, rule.ID)
	if err != nil {
		return nil, err
	}
	rule.Items = items
	return rule, nil
}

// settlementRuleItemRows 读一条规则的项。
//
// **不带「账户可用」这个条件**（执行路径那份读法带着它）：这里的读者是编辑这条规则的人，他要
// 看到自己配的那一项指着哪个账户，哪怕那个账户已经停用了——停用之后页面上那一行显示的是账户名
// 加一个标记，而不是一个空下拉。
func settlementRuleItemRows(ctx context.Context, q querier, ruleID string) ([]SettlementRuleItemRow, error) {
	rows, err := q.Query(ctx, `SELECT `+settlementRuleItemColumns+`
		FROM settlement_rule_items i
		LEFT JOIN settlement_accounts a ON a.id = i.account_id
		WHERE i.rule_id = $1::uuid
		ORDER BY i.sort_order, i.id`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]SettlementRuleItemRow, 0, 4)
	for rows.Next() {
		var item SettlementRuleItemRow
		if err := rows.Scan(&item.ID, &item.PartyType, &item.CalcType, &item.RatioHundredths,
			&item.FixedAmount, &item.SortOrder, &item.Remark,
			&item.AccountID, &item.AccountName, &item.AccountReceiver); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// insertSettlementRuleItems 写一条规则的全部项。
//
// 逐条 INSERT 而不是一条多值 INSERT：项的条数是个位数，而每一条的占位符都要跟着列走，拼一条
// 动态 SQL 的收益远小于它带来的「参数序号对不上」的风险。
//
// 比例那一列写成 `$n::numeric / 10000`：进来的是百分数的百分之一（4500 = 45.00%），除在
// numeric 上是精确的，直接写 float 会带进二进制尾数。
func insertSettlementRuleItems(ctx context.Context, tx pgx.Tx, ruleID string, items []SettlementRuleItemWrite) error {
	for _, item := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO settlement_rule_items
			(rule_id, party_type, calc_type, ratio, fixed_amount, account_id, sort_order, remark)
			VALUES ($1::uuid,$2,$3,$4::numeric / 10000,$5,NULLIF($6,'')::uuid,$7,$8)`,
			ruleID, item.PartyType, item.CalcType, item.RatioHundredths,
			item.FixedAmount, item.AccountID, item.SortOrder, item.Remark); err != nil {
			return mapSettlementError(err, ErrSettlementAccountNotFound, ErrSettlementAccountInUse)
		}
	}
	return nil
}

// settlementRuleSnapshot 是规则进审计快照的形状：**不含两个时间戳**，理由同账户那份。
type settlementRuleSnapshot struct {
	Name           string                       `json:"name"`
	BizType        string                       `json:"biz_type"`
	ScopeType      string                       `json:"scope_type"`
	ScopeRef       string                       `json:"scope_ref"`
	AllocationMode string                       `json:"allocation_mode"`
	Status         string                       `json:"status"`
	Remark         string                       `json:"remark"`
	Items          []settlementRuleItemSnapshot `json:"items"`
}

// settlementRuleItemSnapshot 是快照里的一项，存的是 account_id 而不是账户上的那些标签：账户名
// 改了就改了，审计要能对着当时的那条账户查（而且 017 之后账户上也没有「账户号」可存了）。
// 项自己带 id：一项被删掉又加回来时，只有 id 能说明它们是同一条还是两条。
type settlementRuleItemSnapshot struct {
	PartyType       string `json:"party_type"`
	CalcType        string `json:"calc_type"`
	RatioHundredths int64  `json:"ratio_hundredths"`
	FixedAmount     int64  `json:"fixed_amount"`
	AccountID       string `json:"account_id"`
	SortOrder       int    `json:"sort_order"`
	Remark          string `json:"remark"`
}

func ruleSnapshotOf(row *SettlementRuleRow) settlementRuleSnapshot {
	if row == nil {
		return settlementRuleSnapshot{}
	}
	items := make([]settlementRuleItemSnapshot, 0, len(row.Items))
	for _, item := range row.Items {
		items = append(items, settlementRuleItemSnapshot{
			PartyType: item.PartyType, CalcType: item.CalcType,
			RatioHundredths: item.RatioHundredths, FixedAmount: item.FixedAmount,
			AccountID: item.AccountID, SortOrder: item.SortOrder, Remark: item.Remark,
		})
	}
	return settlementRuleSnapshot{
		Name: row.Name, BizType: row.BizType, ScopeType: row.ScopeType, ScopeRef: row.ScopeRef,
		AllocationMode: row.AllocationMode, Status: row.Status, Remark: row.Remark, Items: items,
	}
}

// ============================================================
// 任务（只读）
// ============================================================

// settlementTaskFrom 是任务列表与详情的同一个 FROM。
//
// LEFT JOIN payments 是为了四个页面上要显示的列：order_no（分账任务上只有支付单号）、provider、
// payment_method。JOIN 而不是在任务表上加列：那些事实属于支付单，一张分账任务上再存一份就是
// 同一件事的第二个写法。
//
// LEFT JOIN settlement_rules 只为规则名——rule_id 可空是常态（没命中规则的单照样建任务，整单
// 归平台），INNER JOIN 会把那一大半记录整个吞掉。
const settlementTaskFrom = ` FROM settlement_tasks t
	LEFT JOIN payments p ON p.id = t.payment_id
	LEFT JOIN settlement_rules r ON r.id = t.rule_id`

// settlementTaskColumns 是任务在后台的形状。金额两列都是分，原样出来（页面自己格式化）。
const settlementTaskColumns = `t.id::text, t.task_no, t.payment_no, COALESCE(p.order_no,''),
	t.base_amount, t.platform_amount, t.status,
	t.scope_type, t.scope_ref, t.store_ref, t.brand_ref, t.merchant_ref,
	COALESCE(t.rule_id::text,''), COALESCE(r.name,''),
	COALESCE(p.provider,''), COALESCE(p.payment_method,''),
	t.provider_task_no, t.provider_transaction_id, t.attempts, t.last_error,
	t.created_at, t.finished_at, t.updated_at`

// SettlementTaskRow 是一条分账任务在后台的形状。
type SettlementTaskRow struct {
	ID             string
	TaskNo         string
	PaymentNo      string
	OrderNo        string
	BaseAmount     int64
	PlatformAmount int64
	Status         string
	ScopeType      string
	ScopeRef       string
	StoreRef       string
	BrandRef       string
	MerchantRef    string
	RuleID         string
	RuleName       string
	Provider       string
	Method         string
	// ProviderTaskNo 是我们发给渠道的分账单号。**今天恒为空**：分账指令随下单报文下发，没有一次
	// 单独的分账调用，也就没有单独的分账单号。列留着（008 建的），等真去查分账时用。
	ProviderTaskNo        string
	ProviderTransactionID string
	Attempts              int
	LastError             string
	CreatedAt             time.Time
	FinishedAt            *time.Time
	UpdatedAt             time.Time
}

func scanSettlementTask(row scanner) (*SettlementTaskRow, error) {
	task := &SettlementTaskRow{}
	if err := row.Scan(&task.ID, &task.TaskNo, &task.PaymentNo, &task.OrderNo,
		&task.BaseAmount, &task.PlatformAmount, &task.Status,
		&task.ScopeType, &task.ScopeRef, &task.StoreRef, &task.BrandRef, &task.MerchantRef,
		&task.RuleID, &task.RuleName, &task.Provider, &task.Method,
		&task.ProviderTaskNo, &task.ProviderTransactionID, &task.Attempts, &task.LastError,
		&task.CreatedAt, &task.FinishedAt, &task.UpdatedAt); err != nil {
		return nil, err
	}
	return task, nil
}

// ListSettlementTasks 读一页分账任务，按创建时间倒序。
//
// 门店 / 品牌 / 商户三个维度各一个等值条件：老系统按这三个维度分了三个接口，这里一个接口三个
// 参数——它们是同一张表上的同一件事，拆开之后「按门店和品牌一起筛」就没地方写了。
func (r *PostgresRepository) ListSettlementTasks(ctx context.Context, q dto.SettlementTaskQuery) ([]*SettlementTaskRow, int, error) {
	w := whereClause{}
	if q.TaskNo != "" {
		w.add(`t.task_no ILIKE $%d`, "%"+escapeLike(q.TaskNo)+"%")
	}
	if q.PaymentNo != "" {
		w.add(`t.payment_no ILIKE $%d`, "%"+escapeLike(q.PaymentNo)+"%")
	}
	if q.OrderNo != "" {
		w.add(`p.order_no ILIKE $%d`, "%"+escapeLike(q.OrderNo)+"%")
	}
	if q.Status != "" {
		w.add(`t.status = $%d`, q.Status)
	}
	if q.ScopeType != "" {
		w.add(`t.scope_type = $%d`, q.ScopeType)
	}
	if q.StoreRef != "" {
		w.add(`t.store_ref = $%d`, q.StoreRef)
	}
	if q.BrandRef != "" {
		w.add(`t.brand_ref = $%d`, q.BrandRef)
	}
	if q.MerchantRef != "" {
		w.add(`t.merchant_ref = $%d`, q.MerchantRef)
	}
	if q.CreatedFrom != nil {
		w.add(`t.created_at >= $%d`, *q.CreatedFrom)
	}
	if q.CreatedTo != nil {
		w.add(`t.created_at <= $%d`, *q.CreatedTo)
	}

	total, err := countRows(ctx, r.pool, settlementTaskFrom, w)
	if err != nil {
		return nil, 0, err
	}
	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+settlementTaskColumns+settlementTaskFrom+
		w.sql()+` ORDER BY t.created_at DESC, t.id DESC`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*SettlementTaskRow, 0, q.PageSize)
	for rows.Next() {
		task, err := scanSettlementTask(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, task)
	}
	return items, total, rows.Err()
}

// SettlementReceiverRow 是一条接收方明细，**全部是快照列**。
//
// 它不回查账户表补当前的名字与子商户号：008 的设计就是把这几个值冻结在行上（当初实际发出去的
// 那个号），回头现查会让历史明细随着账户改名而变——而那条明细的意义正是「当时分给了谁」。
type SettlementReceiverRow struct {
	ID               string
	AccountID        string
	PartyType        string
	PartyName        string
	MerchantRef      string
	BrandRef         string
	StoreRef         string
	ReceiverType     string
	ReceiverID       string
	RatioHundredths  int64
	Amount           int64
	ReversedAmount   int64
	ProviderDetailNo string
	Status           string
	LastError        string
	CreatedAt        time.Time
}

// SettlementTaskDetail 是任务详情：任务本身加上它的接收方明细。
type SettlementTaskDetail struct {
	Task      *SettlementTaskRow
	Receivers []*SettlementReceiverRow
}

// GetSettlementTask 按 id 读一条任务连同它的接收方明细，全在同一个只读快照里。
//
// 两条查询之间落进来一次回调时，页面会出现「任务已经 succeeded、明细还停在 pending」这种从未
// 存在过的组合——而这张页面的全部意义就是「这一笔到底分给谁了」。与支付单详情同一个理由，走
// RepeatableRead + 只读。
func (r *PostgresRepository) GetSettlementTask(ctx context.Context, id string) (*SettlementTaskDetail, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	task, err := scanSettlementTask(tx.QueryRow(ctx,
		`SELECT `+settlementTaskColumns+settlementTaskFrom+` WHERE t.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		// **这一条是唯一的「查无此行」出口**，必须在这里翻成哨兵：controller 是按哨兵决定回
		// 404 还是 500 的，裸的 pgx.ErrNoRows 一个分支都不命中，会落到 default 变成 500 加
		// 一句「服务暂时不可用」——手敲一个不存在的 id 就是这个结果。
		//
		// 不复用 mapSettlementError：那个三参形状的第三参是「被引用删不掉」（23503），读路径
		// 上不存在那种局面，传一个占位值只会让下一个人以为这里有故事。
		return nil, ErrSettlementTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text, account_id::text, party_type, party_name,
			merchant_ref, brand_ref, store_ref, receiver_type, receiver_id,
			(ratio * 10000)::bigint, amount, reversed_amount, provider_detail_no,
			status, last_error, created_at
		FROM settlement_receivers WHERE task_id = $1::uuid
		ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	receivers := make([]*SettlementReceiverRow, 0, 4)
	for rows.Next() {
		receiver := &SettlementReceiverRow{}
		if err := rows.Scan(&receiver.ID, &receiver.AccountID, &receiver.PartyType,
			&receiver.PartyName, &receiver.MerchantRef, &receiver.BrandRef, &receiver.StoreRef,
			&receiver.ReceiverType, &receiver.ReceiverID, &receiver.RatioHundredths,
			&receiver.Amount, &receiver.ReversedAmount, &receiver.ProviderDetailNo,
			&receiver.Status, &receiver.LastError, &receiver.CreatedAt); err != nil {
			return nil, err
		}
		receivers = append(receivers, receiver)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &SettlementTaskDetail{Task: task, Receivers: receivers}, nil
}

// ============================================================
// 公用
// ============================================================

// inTx 把一段写逻辑包进事务。
//
// 本服务此前每一处写都手写 Begin / defer Rollback / Commit（发起支付、回调、退款那几条路），
// 那些路径的回滚条件各不相同，值得各自写清楚。分账后台这一批是同一形状的几组 CRUD：开事务、
// 写、记审计、提交，回滚条件都是「任何一步出错」——所以这里摊成一个函数，而不是把那六行抄十遍。
func (r *PostgresRepository) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// mapSettlementError 把分账后台这几条语句的 PostgreSQL 错误翻成上面那组哨兵错误。
//
// 三个 PostgreSQL 错误码在这里各有含义，而它们都是**正常的业务结论**，不该以 500 的面目弹出来：
//
//	23505 唯一约束：同档位已有启用中的规则、同一渠道下接收方号重复、同一账户在一条规则里出现两次
//	23503 外键：删除被引用（RESTRICT）——这是「改成停用」那个出口存在的原因
//	23502 非空：漏了一个必填列。service 层已经逐个查过，到这里说明校验与表定义之间脱了一处
//
// notFound / inUse 由调用方给：同一条 23503 在「删规则」与「删账户」上意思不同，而错误值本身
// 分不出是哪一条语句报的（pgErr.ConstraintName 给的是外键名，那要再维护一张名字到语义的表，
// 而这张表只有一个调用方看得懂）。
func mapSettlementError(err error, notFound, inUse error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		switch pgErr.ConstraintName {
		case "settlement_rules_scope_uniq":
			return ErrSettlementScopeConflict
		case "settlement_rule_items_account_uniq", "settlement_rule_items_platform_uniq":
			return ErrSettlementRuleItemConflict
		default:
			// 账户上那条 receiver_uniq（017 之后这是账户表上的**唯一**一条唯一约束）与任何
			// 将来加上的唯一约束都归到这里：对用户来说都是「这一行和已有的撞了」。
			return ErrSettlementAccountConflict
		}
	case "23503":
		return inUse
	case "23502", "23514", "22P02":
		// 23502 非空 / 23514 CHECK / 22P02 类型转换。三个都归成「写进去的这份数据不合表上的
		// 约束」：**它们都不该出现**（service 层逐列查过、uuid 也 parse 过），留着是因为表定义
		// 与校验之间真脱开时，一个 500 加一句「column X is null」比一个静默的空串好查得多，
		// 而如果它落进 default 那一支，用户看到的会是一句「服务暂时不可用」——没有任何线索
		// 指向是哪个字段。
		return fmt.Errorf("%w: %s %s", ErrSettlementConstraintViolation, pgErr.Code, pgErr.ColumnName)
	}
	return err
}

// ErrSettlementConstraintViolation 是「写进去的这份数据不合数据库上的约束」。
//
// 它是一条**兜底**，不是业务结论：真正会被用户碰到的那些（同档位撞车、接收方号重复、删被引用
// 的行）在上面各有各的哨兵错误与中文解释。走到这里说明 service 层的校验漏了一处，用户看到的是
// 一句笼统的「配置不合法」，日志里有 PostgreSQL 的原话。
var ErrSettlementConstraintViolation = errors.New("settlement write violates a database constraint")
