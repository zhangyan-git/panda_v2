package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
)

// whereClause 把 (条件, 值) 累积器拼成 WHERE 子句。
//
// 三个筛选字段以上的列表各写一遍这段是重复的，而重复的那一段里有一个很容易写错的细节：
// 占位符序号必须与 args 的下标同步。共用一个累积器就不会有第二个地方算错。
//
// 与 payment-service 的同名类型逐行一致：跨服务重复一个纯拼串的小工具
// 是值得的，抽成共享包会让两个域为了改各自的筛选语法而互相牵制。
type whereClause struct {
	conds []string
	args  []any
}

// add 追加一条带参数的条件。clause 里用 %d 占位，序号由累积器自己填。
func (w *whereClause) add(clause string, value any) {
	w.args = append(w.args, value)
	w.conds = append(w.conds, fmt.Sprintf(clause, len(w.args)))
}

// addColumns 让**同一个值**在多列上做一组 OR 比较（形如 `name ILIKE $1 OR code ILIKE $1`）。
//
// 与 add 分开是因为占位符的个数与值的个数在这里不相等：一个值，N 个 $n，而且它们必须是同一个
// 序号。手写这组条件的写法（先 add 再回头改字符串）在列数变化时会漏掉一处，而漏掉的后果是
// 搜到一半——运营只会觉得「这个名字明明有却搜不出来」。
func (w *whereClause) addColumns(columns []string, op string, value any) {
	w.args = append(w.args, value)
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, fmt.Sprintf("%s %s $%d", column, op, len(w.args)))
	}
	w.conds = append(w.conds, "("+strings.Join(parts, " OR ")+")")
}

// addRaw 追加一条**不带参数**的条件。
//
// 它与 add 分开是必须的：add 每调一次就往 args 里塞一个值，而「只看开着自动续费的」这类条件
// 里没有 $n（它比的是一个布尔列）。用 add 传一个 nil 进去，会让 args 比占位符多出一个，
// SQL 执行时报「参数个数不匹配」——或者更糟，在参数刚好对上时静默用错值。
func (w *whereClause) addRaw(clause string) { w.conds = append(w.conds, clause) }

func (w *whereClause) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// countRows 跑一条 COUNT 并把筛选条件接上。
//
// 计数与取页**必须用同一份 w.args**（在 pageClause 追加 LIMIT/OFFSET 之前）：两份 args 各自
// 追加过参数的话，两边的占位符序号会在某个筛选字段被加进来那天悄悄错开。
func (r *PostgresRepository) countRows(ctx context.Context, from string, w whereClause) (int, error) {
	var total int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) `+from+w.sql(), w.args...).Scan(&total)
	return total, err
}

// pageClause 拼出 LIMIT / OFFSET 片段并把两个参数追加到 args 后面。
//
// 占位符号必须接在筛选条件后面，所以只能由这里算——写死 $5/$6 会在筛选条件多一个的时候
// 静默拿到错误的数（或者直接报参数越界）。
func pageClause(args []any, page, pageSize int) ([]any, string) {
	if pageSize <= 0 {
		pageSize = dto.DefaultPageSize
	}
	full := append(append([]any{}, args...), pageSize, api.PageOffset(page, pageSize))
	return full, fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
}
