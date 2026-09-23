package repository

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// 这个文件是**调用日志**：写一次调用、读一页调用。没有别的。
//
// 它是本库里唯一一张只增表（见 migrations/partner），所以只有两个动词，而且都在这里：
// RecordCallLog 在请求那条路上被调用（每个请求一次，包括所有拒绝路径），ListCallLogs 给后台
// 查「某个合作方某段时间调了什么」。
//
// **没有 UPDATE、没有 DELETE**：这张表存在的全部意义就是「合作方说『我发了』而我们能拿出
// 当时那一次的原样」。一条能被改写的日志证明不了任何事。

// CallLogRow 是列表里的一行：日志本身加上合作方名字。
//
// 名字要 JOIN 出来而不是让前端再查一次：日志行上只有 partner_id，而列表页要显示的是名字
// ——一个全是 UUID 的列表在排查时读不下去。
type CallLogRow struct {
	Log *model.CallLog
	// PartnerName 在认不出调用方时是空串（那一行的 partner_id 是全零 UUID，JOIN 不到东西）。
	PartnerName string
}

// callLogFrom 是列表与计数的同一个 FROM。
//
// LEFT JOIN 而不是 JOIN：认不出调用方的那些行（密钥查不到、头都没带）写的 partner_id 是全零
// UUID，它在 partner_accounts 里没有对应行。用 JOIN 的话这些行会从列表里**消失**——而它们
// 恰恰是最该被看见的一类（有人在试）。这也是日志表不建外键的原因
// （见 migrations/partner 里 partner_call_logs 的建表说明）。
const callLogFrom = ` FROM partner_call_logs l
	LEFT JOIN partner_accounts p ON p.id = l.partner_id`

// callLogColumns 是 partner_call_logs 的读取列（已带 l. 前缀，见下面那条 JOIN）。
const callLogColumns = `l.id, l.partner_id::text, l.api_key_id::text, l.api_key_mask, l.method,
	l.path, l.query, l.request_ip, l.request_body, l.response_body, l.status_code,
	l.duration_ms, l.error_code, l.created_at`

// RecordCallLog 写下一次调用，并把「这把钥匙被用过」记在密钥行上。
//
// # 一次事务里的两件事
//
// 插入日志与更新 last_used_at / call_count 在同一个事务里。它们回答的是同一个问题的两半
// （「刚才发生了什么」与「这把钥匙还活着吗」），分成两次写就会出现「日志有了但计数没动」——
// 而那种偏差没有任何地方能发现。
//
// # 为什么认不出调用方时不更新计数
//
// 更新要有 id 才做（APIKeyID 是全零 UUID 时跳过）。否则会把一次「带着不存在的密钥的探测」
// 记到一把真实密钥头上——那把密钥的 last_used_at 会因此一直很新，运营永远清不掉一把没人
// 在用的钥匙。
//
// # 为什么验签没过的不算「用过」
//
// 条件是**这一行没有 error_code**（也就是这次调用走到了 handler）。不这么判的话，任何一个人
// 拿着一个泄露的 api_key（公开标识，抓包就有）反复发垃圾请求，就能把一把早已停用/淘汰的钥匙
// 的 last_used_at 一直顶到最新，而 error_code 那一列根本不会有人去看。判定「最后使用时间」
// 应当用「真的签对了」的调用，与 partner_api_keys.last_used_at 那条列注释
// （「验签通过之后才更新」）是同一件事。
func (r *PostgresRepository) RecordCallLog(ctx context.Context, entry model.CallLog) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `INSERT INTO partner_call_logs
		(partner_id, api_key_id, api_key_mask, method, path, query, request_ip,
		 request_body, response_body, status_code, duration_ms, error_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		entry.PartnerID, entry.APIKeyID, entry.APIKeyMask, entry.Method, entry.Path,
		entry.Query, entry.RequestIP, entry.RequestBody, entry.ResponseBody,
		entry.StatusCode, entry.DurationMS, entry.ErrorCode); err != nil {
		return err
	}

	if entry.APIKeyID != "" && entry.APIKeyID != model.UnknownPartnerID && entry.ErrorCode == "" {
		// 不做 `WHERE status = 'enabled'`：那把钥匙在被停用的前一瞬间可能刚通过验签，这一次
		// 调用是真的发生了。用它自己的 id 更新即可，停用与否不影响「它被用过」这个事实。
		if _, err := tx.Exec(ctx, `UPDATE partner_api_keys
			SET last_used_at = NOW(), call_count = call_count + 1
			WHERE id = $1`, entry.APIKeyID); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// ListCallLogs 读一页调用日志，最新的在前。
//
// 排序用 `l.id DESC` 而不是 created_at：id 是 IDENTITY，天然单调，用它排序既走主键索引又
// 不会在同一毫秒的几行之间给出不稳定的顺序——而「同一秒里这几条谁先谁后」正是排查一次并发
// 调用时要看的。created_at 那个索引留给保留期清理（见 migrations/partner 文件头的已知缺口）。
//
// 全部筛选条件都带 `l.` 前缀（见 callLogFrom 的 JOIN）：两张表都有 id、created_at，不限定
// 别名的报错是运行时的「列指代不明」。
func (r *PostgresRepository) ListCallLogs(ctx context.Context, q dto.CallLogQuery) ([]*CallLogRow, int, error) {
	w := whereClause{}
	if q.PartnerID != "" {
		w.add(`l.partner_id = $%d`, q.PartnerID)
	}
	if q.APIKeyID != "" {
		w.add(`l.api_key_id = $%d`, q.APIKeyID)
	}
	if q.ErrorCode != "" {
		w.add(`l.error_code = $%d`, q.ErrorCode)
	}
	if q.StatusCode > 0 {
		w.add(`l.status_code = $%d`, q.StatusCode)
	}
	if q.From != nil {
		w.add(`l.created_at >= $%d`, *q.From)
	}
	if q.To != nil {
		// 闭区间上界。用 `<` 加一秒是不行的（会漏掉那一秒内的亚秒部分），而 `<=` 对
		// timestamptz 是精确比较，前端传的是它自己那边的整秒——差一点点只影响边界那一条。
		w.add(`l.created_at <= $%d`, *q.To)
	}

	total, err := countRows(ctx, r.pool, callLogFrom, w)
	if err != nil {
		return nil, 0, err
	}
	args, page := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+callLogColumns+`, COALESCE(p.name, '')`+
		callLogFrom+w.sql()+` ORDER BY l.id DESC`+page, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*CallLogRow, 0, q.PageSize)
	for rows.Next() {
		item := &CallLogRow{Log: &model.CallLog{}}
		log := item.Log
		if err := rows.Scan(&log.ID, &log.PartnerID, &log.APIKeyID, &log.APIKeyMask,
			&log.Method, &log.Path, &log.Query, &log.RequestIP, &log.RequestBody,
			&log.ResponseBody, &log.StatusCode, &log.DurationMS, &log.ErrorCode,
			&log.CreatedAt, &item.PartnerName); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
