// Package repository 是合作方库（panda_partner）的数据访问层。
//
// 三个文件按聚合拆开：partner.go 是合作方账号，apikey.go 是密钥（含验签要用的那条热路径
// 查询），calllog.go 是调用日志。跨表的事务（新建密钥要同时读回合作方、写审计）由这一层
// 编排，因为它就是「一个事务里把这几个事实写进去」的那个地方。
//
// 这一份的 querier / whereClause / pageClause / mapPGError 与 payment-service 的同名函数
// 几乎逐行一致——分页与唯一键翻译在这个仓库里只有一种语义，换服务时不该重新理解一遍。
// **唯一的差别是表名与前缀**。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
)

var (
	// ErrPartnerNotFound：要改的合作方不存在。
	ErrPartnerNotFound = errors.New("partner account not found")
	// ErrPartnerCodeTaken：合作方编码撞了库里已有的行（唯一键 23505）。是 409 不是 500：
	// 编码是运营自己填的，撞了要改的是那次输入。
	ErrPartnerCodeTaken = errors.New("partner account code is already taken")
	// ErrPartnerCodeImmutable：请求里的编码与库里那行不一致。
	//
	// 与「撞了」分开：这条说的是「你想把编码改掉」，那是这一层不提供的能力（UPDATE 语句里
	// 根本没有那一列），不是「这个名字被别人占了」。
	ErrPartnerCodeImmutable = errors.New("partner account code cannot be changed")
	// ErrAPIKeyNotFound：要改的密钥不存在（或者不属于路径上那个合作方）。
	//
	// 「不属于那个合作方」并入这一条：路径是 /partners/{id}/keys/{keyId}，两者对不上时
	// 正确的答案是 404 而不是「找到了但不给你改」——后者会让人以为密钥存在，只是权限不够。
	ErrAPIKeyNotFound = errors.New("partner api key not found")
	// ErrAPIKeyTaken：随机生成的 api_key 撞了库里已有的行。
	//
	// 32 位 base62 撞一次的概率可以忽略，但**不是零**，而它撞上时的表现必须是 409 或者一次
	// 重试，不能是一个裸 PgError 冒成 500——那会让一次「重新签发」变成一条没有线索的报障。
	ErrAPIKeyTaken = errors.New("partner api key is already taken")
)

// PostgresRepository 是合作方库的数据访问实现。
type PostgresRepository struct {
	pool     *pgxpool.Pool
	recorder audit.Recorder
}

// NewPostgresRepository 构造仓储。recorder 为 nil 时用 audit.Noop：调用点的审计语句保持
// 无条件执行，不留「忘了传 recorder 就没有审计」的分支。
func NewPostgresRepository(pool *pgxpool.Pool, recorder audit.Recorder) *PostgresRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &PostgresRepository{pool: pool, recorder: recorder}
}

// querier 让同一段读逻辑既能用连接池也能用事务。
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type scanner interface{ Scan(...any) error }

// pageClause 拼出 LIMIT / OFFSET 片段并把两个参数追加到 args 后面。
//
// 占位符序号必须接在筛选条件后面，所以只能由这里算：写死 $3/$4 会在筛选条件多一个的时候
// 静默拿到错误的数（或者直接报参数越界）。
//
// 默认页大小用 platform/api 的 DefaultPageSize（20），与后台其它列表一致。
func pageClause(args []any, page, pageSize int) ([]any, string) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = api.DefaultPageSize
	}
	full := append(append([]any{}, args...), pageSize, api.PageOffset(page, pageSize))
	return full, fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
}

// whereClause 把 (条件, 值) 累积器拼成 WHERE 子句。占位符序号与 args 的下标同步。
type whereClause struct {
	conds []string
	args  []any
}

func (w *whereClause) add(clause string, value any) {
	w.args = append(w.args, value)
	w.conds = append(w.conds, fmt.Sprintf(clause, len(w.args)))
}

// addMany 把一个条件写成多个占位符（同一个值要出现在两处时用，例如「编码或名称」）。
//
// 单独一个方法而不是让调用方自己算序号：序号算错的表现是**查询报参数越界**（好），或者更糟
// ——两个 $n 指到别的值上（比如把状态当成搜索词），那会静默返回一批不相干的行。让占位符的
// 序号只由这里产生，调用方只写一个带 %d 的模板。
func (w *whereClause) addMany(clause string, values ...any) {
	positions := make([]any, 0, len(values))
	for _, value := range values {
		w.args = append(w.args, value)
		positions = append(positions, len(w.args))
	}
	w.conds = append(w.conds, fmt.Sprintf(clause, positions...))
}

func (w *whereClause) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// countRows 跑一条 COUNT 并把筛选条件接上。
//
// 计数与取页**必须用同一份 w.args**（在 pageClause 追加 LIMIT/OFFSET 之前）：两份 args 各自
// 追加过参数的话，两边的占位符序号会在某个筛选字段被加进来那天悄悄错开。计数与取页分两条
// 查询、各拿自己的快照，所以翻页时 total 可能与 items 差一行——后台列表上这不是问题。
func countRows(ctx context.Context, q querier, from string, w whereClause) (int, error) {
	var total int
	err := q.QueryRow(ctx, `SELECT COUNT(*) `+from+w.sql(), w.args...).Scan(&total)
	return total, err
}

// escapeLike 把用户输入里的 ILIKE 元字符转义掉。
//
// 不转的话，运营在搜索框里打进一个 `_`（编码里真的有下划线）会当场变成单字符通配，搜出
// 所有长度对得上的行；打进 `%` 则是整个列表。这种「搜了但搜出来的是别的」比搜不到更难察觉。
//
// 不需要在 SQL 里写 ESCAPE：PostgreSQL 的 LIKE / ILIKE 默认转义符就是反斜杠，所以反斜杠
// 本身也要转。
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// qualify 给一份列清单里的每一项加上表别名前缀。
//
// **前提：清单里每一项都是「列名」或「列名::类型」**——本包所有的 xxxColumns 都是这个形状，
// 没有函数调用、没有嵌套逗号。往里加一个带括号的表达式会让它拼出一句语法错误的 SQL，那会在
// 第一次跑这条查询时当场炸掉，不会静默出错。
//
// 它在这里是为了合作方的列表：那条查询 JOIN 了一个预聚合的子查询（见 partner.go 的
// partnerListFrom），两边都有 id / created_at 同名列，不限定别名就是一个「列指代不明」的
// 运行时报错——而 partnerColumns 是同一个常量，详情查询还要用它（那边没有歧义）。
func qualify(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// emptyIfNil 把 nil 切片换成空切片。
//
// 只为一类坑存在：pgx 把 nil 切片编成 **NULL** 而不是空数组，而 ip_whitelist 那一列是
// NOT NULL。service 传 nil 表示「不限制来源」，落在 SQL 上必须是 `'{}'::text[]`；不换的话，
// 「不填白名单」会以一句违反非空约束的 500 收场——而它恰恰是最常见的那种输入。
//
// 不改成「让 service 保证不传 nil」：那是把一条 SQL 语义记在调用方脑子里，而任何新增的调用
// 方都得重新踩一次。
func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// marshalEnvelope 把密文信封编码成 JSONB 参数。
//
// 编码成一个字符串再由语句里的 `$n::jsonb` 转，而不是直接把 secret.Envelope 交给 pgx：
// pgx 对未注册的自定义结构体的编码行为取决于驾驶层有没有推断出 OID，而这个类型的 json tag
// 决定了落库的形状——那条推断链上任何一环的变化都会变成一次「写进去的行读不出来」。
// 显式的 JSON 文本加显式转换，链路上没有可推断的东西。
//
// 错误**不带底层 json 错误**：它可能把被编码的值原样回显在错误串里，而这个值正是密文。
// 这个类型（三个字符串字段）编码失败是不可能的，所以这里不需要那份细节。
func marshalEnvelope(envelope secret.Envelope) (string, error) {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("encode partner signing secret")
	}
	return string(encoded), nil
}

// mapPGError 把唯一约束冲突翻成调用方分得清的业务错误。
//
// 没有它，一个重名的合作方会以裸 PgError 冒到 handler 变成 500：运营看到「服务暂时不可用」，
// 而真相只是那个编码被别人用了。
//
// 约束名是这里唯一稳定可依赖的东西：靠错误文本匹配会在 PG 换语言或换措辞时静默失效。
// 下面两个名字是**从 dev 库里读出来的实际值**（\d partner_accounts），不是照 DDL 拼的。
func mapPGError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPartnerNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "partner_accounts_code_key":
			return ErrPartnerCodeTaken
		case "partner_api_keys_api_key_key":
			return ErrAPIKeyTaken
		}
	}
	return err
}
