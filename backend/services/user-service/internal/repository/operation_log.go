package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// OperationLogFilter 是后台日志列表的筛选条件，零值表示不筛选。
//
// 时间范围用指针：零值 time.Time 是 0001 年，没法表达「不限」——用它是查不到
// 任何东西，而不是查全部，症状是列表永远空的。
type OperationLogFilter struct {
	// Module / Action 精确匹配，取值来自库中实际出现过的枚举（ListFacets）。
	Module string
	Action string
	// Result 取值同 model.OperationResult*。
	Result string
	// Operator 同时匹配 admin_username 与 admin_name 的前缀：有人记得的是登录名，
	// 有人记得的是姓名，让调用方自己选一个等于把这事儿推给用户。
	Operator string
	// Keyword 匹配目标名称或操作描述（子串）。
	Keyword string
	// StartTime / EndTime 按 occurred_at 过滤，闭区间。
	StartTime *time.Time
	EndTime   *time.Time
}

// OperationLogFacets 是库中实际出现过的模块与动作，供前端做下拉选项。
//
// 从数据里取而不是在代码里写死一份：模块名由各服务的审计调用点决定（miniapp_users、
// merchants、roles…），写死的那份会在下一个模块上线时静默少一项，而「筛选里没有
// 我要找的模块」看起来就是日志没记上。
type OperationLogFacets struct {
	Modules []string
	Actions []string
}

// AdminOperationLogRepository 读写平台后台操作日志。
//
// 写入侧由审计消费者调用，插入必须能安全重放——消费者可能在「写成功、标记 inbox
// 完成失败」之后重投同一条消息，所以 Insert 靠 event_id 幂等。
//
// 读取侧只服务后台日志页面，是只读的：这张表是审计证据，没有 UPDATE、没有 DELETE，
// 也没有「清空日志」这种接口。要保留多久由 DBA 按留存策略处理，不由后台页面决定。
type AdminOperationLogRepository interface {
	Insert(ctx context.Context, eventID string, entry audit.Entry) error
	// FindPage 返回一页日志；总数走 Count。两者的筛选条件必须同源
	// （共用 operationLogsWhere 与 operationLogsArgs），否则会分叉成
	// 「第 3 页是空的，总数却说还有 200 条」。
	FindPage(ctx context.Context, f OperationLogFilter, limit, offset int) ([]*model.AdminOperationLog, error)
	Count(ctx context.Context, f OperationLogFilter) (int64, error)
	// ListFacets 返回模块与动作的可选值，用于下拉筛选。
	ListFacets(ctx context.Context) (*OperationLogFacets, error)
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

// operationLogColumns 统一 SELECT 列表。三个可空的 UUID 列 COALESCE 成空串，
// 扫出来就是字符串；两张 JSONB 快照列直接扫成 []byte，空值即 nil。
// 顺序必须与 scanAdminOperationLog 的 Scan 逐字段对齐。
const operationLogColumns = `l.id,
	COALESCE(l.admin_user_id::text, ''), l.admin_username, l.admin_name,
	l.module, l.action, l.operation,
	l.target_type, COALESCE(l.target_id::text, ''), l.target_name,
	COALESCE(l.merchant_id::text, ''), l.result, l.error_code, l.error_message,
	l.before_data, l.after_data, l.occurred_at`

// operationLogsWhere 是列表与计数共用的筛选条件，占位符顺序由 operationLogsArgs
// 保证。写成「参数为空则这条恒真」而不是按条件拼字符串：拼字符串意味着 Count 和
// FindPage 各持一份分支，两者错开的症状只在翻到第二页之后才出现。
//
// 两个时间参数显式转 timestamptz：$7 IS NULL 里没有可供推断的信息，不转的话 PG
// 无法确定参数类型。
//
// LIKE 模式里的通配符已在 Go 侧转义（escapeLikePattern），这里声明 ESCAPE '\'
// 与之一致；漏了它，搜索框里打一个 % 会命中全部日志。
const operationLogsWhere = `
	WHERE ($1 = '' OR l.module = $1)
	  AND ($2 = '' OR l.action = $2)
	  AND ($3 = '' OR l.result = $3)
	  AND ($4 = '' OR l.admin_username ILIKE $4 ESCAPE '\' OR l.admin_name ILIKE $5 ESCAPE '\')
	  AND ($6 = '' OR l.target_name ILIKE $6 ESCAPE '\' OR l.operation ILIKE $6 ESCAPE '\')
	  AND ($7::timestamptz IS NULL OR l.occurred_at >= $7::timestamptz)
	  AND ($8::timestamptz IS NULL OR l.occurred_at <= $8::timestamptz)`

// operationLogsArgs 按 operationLogsWhere 的占位符顺序排列参数，$4/$5 是同一个
// 操作人前缀（用户名和姓名各匹配一次），$6 是关键词子串。
//
// 空筛选传空串而不是 '%'：那个 '%' 会绕过上面「操作人为空则恒真」的短路，让一个
// 空搜索框把整张表的人名都匹配上。
func operationLogsArgs(f OperationLogFilter) []any {
	operatorPattern := ""
	if operator := escapeLikePattern(strings.TrimSpace(f.Operator)); operator != "" {
		operatorPattern = operator + "%"
	}
	keywordPattern := ""
	if keyword := escapeLikePattern(strings.TrimSpace(f.Keyword)); keyword != "" {
		keywordPattern = "%" + keyword + "%"
	}
	return []any{
		strings.TrimSpace(f.Module), strings.TrimSpace(f.Action), f.Result,
		operatorPattern, operatorPattern, keywordPattern,
		f.StartTime, f.EndTime,
	}
}

// FindPage 按发生时间倒序，最近的日志在最前——运维查日志永远是「刚才那次操作」。
// id 作为排序决胜位：同一次批量操作里的若干条事件时间戳可能完全相同，单靠时间
// 排序没有稳定次序，翻页会重复或漏行（同 adminUserRepo.FindPage）。
func (r *pgOperationLogRepo) FindPage(ctx context.Context, f OperationLogFilter, limit, offset int) ([]*model.AdminOperationLog, error) {
	q := `SELECT ` + operationLogColumns + `
		FROM admin_operation_logs l` + operationLogsWhere + `
		ORDER BY l.occurred_at DESC, l.id DESC
		LIMIT $9 OFFSET $10`
	rows, err := r.pool.Query(ctx, q, append(operationLogsArgs(f), limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminOperationLog
	for rows.Next() {
		entry, err := scanAdminOperationLog(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, entry)
	}
	return list, rows.Err()
}

func (r *pgOperationLogRepo) Count(ctx context.Context, f OperationLogFilter) (int64, error) {
	q := `SELECT count(*) FROM admin_operation_logs l` + operationLogsWhere
	var n int64
	err := r.pool.QueryRow(ctx, q, operationLogsArgs(f)...).Scan(&n)
	return n, err
}

// 两条取候选值的语句分开写死，不拼列名：列名一旦是变量，这里就成了把标识符拼进
// SQL 的地方，而它未来很容易被调用方「顺手」传成参数。
const (
	listOperationLogModulesSQL = `SELECT DISTINCT module FROM admin_operation_logs WHERE module <> '' ORDER BY 1`
	listOperationLogActionsSQL = `SELECT DISTINCT action FROM admin_operation_logs WHERE action <> '' ORDER BY 1`
)

// ListFacets 取库中实际出现过的模块与动作。
//
// 两条 DISTINCT 各是一次全表聚合，没有索引支撑，这里也不建：候选值一旦出现就永久
// 有效，为「取一列去重值」建索引用处极小，而索引的维护成本落在写入侧——这张表的
// 写入是审计，慢在那里等于拖住业务事务。日志量到十万级之前，这两条都在几十毫秒内。
func (r *pgOperationLogRepo) ListFacets(ctx context.Context) (*OperationLogFacets, error) {
	modules, err := r.queryStrings(ctx, listOperationLogModulesSQL)
	if err != nil {
		return nil, err
	}
	actions, err := r.queryStrings(ctx, listOperationLogActionsSQL)
	if err != nil {
		return nil, err
	}
	return &OperationLogFacets{Modules: modules, Actions: actions}, nil
}

func (r *pgOperationLogRepo) queryStrings(ctx context.Context, q string) ([]string, error) {
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

// scanAdminOperationLog 把一行扫成模型。
//
// 两张 JSONB 快照扫进 []byte 再赋给 json.RawMessage，而不是直接扫进 RawMessage：
// pgx 对 jsonb 的目标类型是按 []byte / string / json.Unmarshaler 分派的，中间隔
// 一层 []byte 是这三条路径里最确定的一条。
func scanAdminOperationLog(row pgx.Row) (*model.AdminOperationLog, error) {
	var (
		entry      model.AdminOperationLog
		beforeData []byte
		afterData  []byte
	)
	if err := row.Scan(
		&entry.ID,
		&entry.AdminUserID, &entry.AdminUsername, &entry.AdminName,
		&entry.Module, &entry.Action, &entry.Operation,
		&entry.TargetType, &entry.TargetID, &entry.TargetName,
		&entry.MerchantID, &entry.Result, &entry.ErrorCode, &entry.ErrorMessage,
		&beforeData, &afterData, &entry.OccurredAt,
	); err != nil {
		return nil, err
	}
	// 空快照保持 nil，与 audit.Entry 那边「没有快照就是空」的表示法一致，
	// 序列化成 JSON 时是 null 而不是空对象。
	if len(beforeData) > 0 {
		entry.BeforeData = beforeData
	}
	if len(afterData) > 0 {
		entry.AfterData = afterData
	}
	return &entry, nil
}

// jsonOrNull 把空快照变成 SQL NULL：JSONB 列接受 NULL，但不接受空串。
func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
