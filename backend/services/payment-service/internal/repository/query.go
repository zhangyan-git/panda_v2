package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 这个文件是**后台只读列表与详情**的读路径。
//
// 与 payment_query.go 的分工：那个文件是**业务**读路径（回调与发起支付要的那几行），
// 这个文件只服务后台页面——带 JOIN 的富行、分页、按条件筛。
//
// 两者共用同一份 xxxColumns（postgres.go / payment_query.go 里的那几个），靠 qualify 拼
// 前缀。不各抄一遍的理由与 membership-service 的同名文件逐字相同：抄一遍的后果是
// 「列表页和详情页上同一条记录字段不一样」，而那种 bug 只有对着两个页面对数才会被发现。

// querier 让同一段读逻辑既能用连接池也能用事务。
//
// 它比其它服务里同名的那个接口多了一层用处：那边的读要么在写事务里、要么直接用池，
// 这里多出第三种——**只读快照事务**（见 GetPaymentDetail）。本服务的 pool 与 pgx.Tx
// 都满足它。
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// 下面四个是从 membership-service 的 repository/query.go 里复制过来的同名函数。
//
// 复制而不是抽到 platform：它们依赖 dto.DefaultPageSize 与 api.PageOffset，而 dto 是
// **每个服务各自的一份**（json tag 是各服务的对外契约）。抽到 platform 就得让 platform
// 认识某个服务的 dto，那是 platform 不该知道的事。

// qualify 给一份列清单里的每一项加上表别名前缀。
//
// **前提：清单里每一项都是「列名」或「列名::类型」**——本包所有的 xxxColumns 都是这个形状，
// 没有函数调用、没有嵌套逗号。往里加一个带括号的表达式会让它拼出一句语法错误的 SQL，
// 那会在第一次跑这条查询时当场炸掉，不会静默出错。
func qualify(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// pageClause 拼出 LIMIT / OFFSET 片段并把两个参数追加到 args 后面。
//
// 占位符序号必须接在筛选条件后面，所以只能由这里算——写死 $5/$6 会在筛选条件多一个的时候
// 静默拿到错误的数（或者直接报参数越界）。
func pageClause(args []any, page, pageSize int) ([]any, string) {
	if pageSize <= 0 {
		pageSize = dto.DefaultPageSize
	}
	full := append(append([]any{}, args...), pageSize, api.PageOffset(page, pageSize))
	return full, fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
}

// whereClause 把 (条件, 值) 累积器拼成 WHERE 子句。占位符序号与 args 的下标同步。
type whereClause struct {
	conds []string
	args  []any
}

// add 拼一条条件。**值可以是多个**：模板里的 %d 依次拿到从当前位置起的连续占位符序号，
// 所以同一个值要用三处（「一个关键词搜三列」）时不用把序号自己数一遍。
//
// 序号必须由这里算，不能由调用方写死：写死的 $3 会在前面多出一个筛选条件时静默指向别的值，
// 而那种错在 SQL 上完全合法，跑出来的是完全不同的结果。
func (w *whereClause) add(clause string, values ...any) {
	start := len(w.args) + 1
	w.args = append(w.args, values...)
	args := make([]any, 0, len(values))
	for i := range values {
		args = append(args, start+i)
	}
	w.conds = append(w.conds, fmt.Sprintf(clause, args...))
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
// 追加过参数的话，两边的占位符序号会在某个筛选字段被加进来那天悄悄错开。
func countRows(ctx context.Context, q querier, from string, w whereClause) (int, error) {
	var total int
	err := q.QueryRow(ctx, `SELECT COUNT(*) `+from+w.sql(), w.args...).Scan(&total)
	return total, err
}

// escapeLike 把用户输入里的 ILIKE 元字符转义掉。
//
// 不转的话，运维在单号框里打进一个 `_`（旧单号里真的有下划线）会当场变成单字符通配，
// 搜出所有单号长度对得上的行；打进 `%` 则是整个列表。这种「搜了但搜出来的是别的」比
// 搜不到更难察觉。
//
// 不需要在 SQL 里写 ESCAPE：PostgreSQL 的 LIKE / ILIKE 默认转义符就是反斜杠。
// 所以反斜杠本身也要转。
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// ============================================================
// 支付单
// ============================================================

// paymentListFrom 是列表与详情的同一个 FROM。
//
// 从前它 LEFT JOIN 两张配置表把渠道名与方式名带出来。那两张表删掉之后，这一份变回一条
// 单表查询——而**渠道名与方式名并没有消失**，它们由 service 层拿 catalog 补（见
// AdminQueryService.decorate）：名字属于「这份代码认识哪些支付方式」，不属于「这笔钱发生
// 了什么」，所以它不该出现在一条读钱的 SQL 里。
//
// 这也是它作为常量留在这里的唯一理由：列表与详情共用同一个 FROM，计数与取页也共用，
// 两边永远对得上。
const paymentListFrom = ` FROM payments p`

// PaymentListRow 是一张支付单加上从**代码里的目录**补出来的两个名字。
//
// 四个字段里前两个是 payments 自己的列（provider / payment_method，值就是渠道名与方式
// code），后两个是 service 层按目录补的中文名。名字留在这一层的形状里而不是让页面自己去
// 查目录，是因为列表与详情两处都要显示它们，而「按 code 取名字」只有一份实现。
type PaymentListRow struct {
	Payment     *model.Payment
	ChannelCode string
	ChannelName string
	MethodCode  string
	MethodName  string
}

func scanPaymentListRow(row scanner) (*PaymentListRow, error) {
	item := &PaymentListRow{Payment: &model.Payment{}}
	p := item.Payment
	err := row.Scan(&p.ID, &p.PaymentNo, &p.LegacyID, &p.OrderNo, &p.UserID, &p.Amount,
		&p.Provider, &p.PaymentMethod, &p.Status, &p.Subject, &p.Attach,
		&p.ProviderTransactionID, &p.FailureCode, &p.FailureMessage, &p.RequestID,
		&p.ExpiresAt, &p.PaidAt, &p.ClosedAt, &p.CreatedAt, &p.UpdatedAt,
		&p.AccountEntryID, &p.AccountFundedAt)
	if err != nil {
		return nil, err
	}
	// 两个 code 直接取 payments 自己的列，名字留给 service 补（见上面那段）。
	item.ChannelCode = p.Provider
	item.MethodCode = p.PaymentMethod
	return item, nil
}

// ListPayments 读一页支付单，按创建时间倒序。
//
// 默认排序（不带任何筛选）是这条查询的主用途——「最近发生了什么」。payment/006 的
// payments_admin_list_idx (created_at DESC, id DESC) 就是照它建的。
//
// 单号两个模糊条件吃不到索引（见 dto.PaymentQuery 的取舍说明），status / payment_method
// 也吃不到；「先筛后排序」在这张还只有几十万行的表上是可接受的。
//
// 计数与取页分两条查询、各自拿自己的快照：这样翻页时 total 可能与 items 差一行（正好在
// 两条查询之间新落了一张单）。后台列表上这不是问题——其余几个域的后台列表也是
// 这个形状。**详情不一样**，那里的一致性对用户可见，所以走只读快照事务（见 GetPaymentDetail）。
func (r *PostgresRepository) ListPayments(ctx context.Context, q dto.PaymentQuery) ([]*PaymentListRow, int, error) {
	w := whereClause{}
	if q.PaymentNo != "" {
		w.add(`p.payment_no ILIKE $%d`, "%"+escapeLike(q.PaymentNo)+"%")
	}
	if q.OrderNo != "" {
		w.add(`p.order_no ILIKE $%d`, "%"+escapeLike(q.OrderNo)+"%")
	}
	if q.UserID != "" {
		w.add(`p.user_id = $%d`, q.UserID)
	}
	if q.Status != "" {
		w.add(`p.status = $%d`, q.Status)
	}
	if q.MethodCode != "" {
		w.add(`p.payment_method = $%d`, q.MethodCode)
	}
	if q.CreatedFrom != nil {
		w.add(`p.created_at >= $%d`, *q.CreatedFrom)
	}
	if q.CreatedTo != nil {
		w.add(`p.created_at <= $%d`, *q.CreatedTo)
	}

	total, err := countRows(ctx, r.pool, paymentListFrom, w)
	if err != nil {
		return nil, 0, err
	}

	page, limit := pageClause(w.args, q.Page, q.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+qualify("p", paymentColumns)+paymentListFrom+w.sql()+` ORDER BY p.created_at DESC, p.id DESC`+limit, page...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]*PaymentListRow, 0, q.PageSize)
	for rows.Next() {
		item, err := scanPaymentListRow(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// PaymentDetailRows 是详情页六块数据。全在同一个只读快照里读出来。
type PaymentDetailRows struct {
	Payment       *PaymentListRow
	Fundings      []*model.PaymentFunding
	Transactions  []*model.PaymentTransaction
	Transitions   []*model.PaymentStateTransition
	ProviderCalls []*model.PaymentProviderCall
	Notifications []*model.PaymentNotification
}

// GetPaymentDetail 按支付单号一次读回详情页要的六块。
//
// # 为什么这一个是事务，而上面的列表不是
//
// 六条查询之间落进来一次渠道回调时，页面会出现**一个从未存在过的中间态**：支付单已经
// succeeded 而它的出资行还停在 reserved，或者 succeeded 却没有对应的记账流水。这张页面的
// 全部意义就是「如实反映这笔钱在某一刻的状态」，而排查的人正是照着它判断「到底扣没扣」——
// 显示一个不存在的组合比少显示一块更有害。
//
// 重复读 + 只读：六条查询看到同一个快照（这是 RepeatableRead 给的），只读则让「这里绝不可能
// 写」成为数据库层的保证，而不只是注释里的承诺。事务里一条写语句都没有，所以结尾是 Rollback
// 而不是 Commit——读事务没有东西可提交。
//
// # 子表的取法与排序
//
// 都按各自的索引走：出资行 payment_fundings_payment_id_line_no_key、流水
// payment_transactions_payment_idx、流转 payment_state_transitions_aggregate_idx、
// 渠道调用 payment_provider_calls_payment_idx、通知 payment_notifications_payment_idx。
//
// 状态流转**只取 payment 这一层**（aggregate_type='payment'）。表里还有 funding / refund /
// agreement / charge / reconciliation 五种 aggregate_type，但今天全仓只有 payment 那一种有
// 代码在写（实测 dev 库 162 行全是 payment）——把 funding 那几条混进来会让「支付单的状态流转」
// 这条时间线上多出几个不是支付单自己的节点。等哪天开始写它们，再决定要不要展示。
func (r *PostgresRepository) GetPaymentDetail(ctx context.Context, paymentNo string) (*PaymentDetailRows, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	detail := &PaymentDetailRows{}

	// 支付单本身先读：底下五块都要用它的 id / 单号做键。
	detail.Payment, err = scanPaymentListRow(tx.QueryRow(ctx,
		`SELECT `+qualify("p", paymentColumns)+
			paymentListFrom+` WHERE p.payment_no = $1`, paymentNo))
	if err != nil {
		return nil, mapPGError(err)
	}
	paymentID := detail.Payment.Payment.ID

	if detail.Fundings, err = loadFundings(ctx, tx, paymentID); err != nil {
		return nil, err
	}
	if detail.Transactions, err = r.loadTransactions(ctx, tx, paymentNo); err != nil {
		return nil, err
	}
	if detail.Transitions, err = r.loadTransitions(ctx, tx, paymentID); err != nil {
		return nil, err
	}
	if detail.ProviderCalls, err = r.loadProviderCalls(ctx, tx, paymentNo); err != nil {
		return nil, err
	}
	if detail.Notifications, err = r.loadNotifications(ctx, tx, paymentNo); err != nil {
		return nil, err
	}
	return detail, nil
}

// loadTransactions 读一张支付单的全部记账流水，按发生时间正序。
//
// 正序：这是一本账，要从头读才知道钱是怎么动的。倒序会把「先 in 后 out 的冲正」摆成
// 「先冲正后入账」，看上去像凭空少了一笔钱。
func (r *PostgresRepository) loadTransactions(ctx context.Context, q querier, paymentNo string) ([]*model.PaymentTransaction, error) {
	rows, err := q.Query(ctx, `SELECT `+transactionColumns+`
		FROM payment_transactions WHERE payment_no = $1 ORDER BY occurred_at, id`, paymentNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, scanTransaction)
}

func (r *PostgresRepository) loadTransitions(ctx context.Context, q querier, paymentID string) ([]*model.PaymentStateTransition, error) {
	rows, err := q.Query(ctx, `SELECT `+transitionColumns+`
		FROM payment_state_transitions
		WHERE aggregate_type = $1 AND aggregate_id = $2
		ORDER BY created_at, id`, model.AggregatePayment, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, scanTransition)
}

// loadProviderCalls 读一张支付单的**全部**出网调用，含失败与超时。
//
// 不过滤 result：失败与超时的那几条正是排查时最该看的（「渠道到底回了什么」），
// 过滤掉就只剩成功的人为太平。
func (r *PostgresRepository) loadProviderCalls(ctx context.Context, q querier, paymentNo string) ([]*model.PaymentProviderCall, error) {
	rows, err := q.Query(ctx, `SELECT `+providerCallColumns+`
		FROM payment_provider_calls WHERE payment_no = $1 ORDER BY created_at DESC, id`, paymentNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, scanProviderCall)
}

// loadNotifications 读一张支付单的**全部**回调通知，含被忽略与处理失败的。
//
// 同样不过滤 status：ignored（重放、早已处理过的通知）与 failed 都是排查时要看的东西。
func (r *PostgresRepository) loadNotifications(ctx context.Context, q querier, paymentNo string) ([]*model.PaymentNotification, error) {
	rows, err := q.Query(ctx, `SELECT `+notificationColumns+`
		FROM payment_notifications WHERE payment_no = $1 ORDER BY received_at, id`, paymentNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, scanNotification)
}

// collect 把一次查询的行扫成切片。
//
// 抽出来只是为了不让上面四个函数各抄一遍同样的五行循环——它们除了列清单与扫描函数以外
// 完全一样。**返回空切片而不是 nil**：nil 在 JSON 里是 null，而前端那两个页签拿到的
// 应该是空数组（`null.length` 会当场炸）。
func collect[T any](rows pgx.Rows, scan func(scanner) (T, error)) ([]T, error) {
	items := make([]T, 0, 4)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ============================================================
// 列清单与扫描（只给上面的读路径用）
// ============================================================

// 下面三张表今天没有任何读路径，所以之前没有列清单。新增的这三份与 model 的字段顺序
// 严格一一对应（UUID 列照旧 ::text），改一个字段要同时改列清单与扫描函数。

const transactionColumns = `id::text, legacy_id, kind, payment_no, refund_no, funding_line_no,
	line_type, direction, amount, provider, provider_transaction_id,
	account_entry_id::text, occurred_at, created_at`

const providerCallColumns = `id::text, provider, operation, payment_no,
	refund_no, agreement_no, request_id, trace_id, attempt_no, request_summary,
	response_summary, http_status, provider_code, provider_message, result, duration_ms,
	created_at`

const notificationColumns = `id::text, provider, notification_id, event_type,
	payment_no, refund_no, body, body_sha256, headers, signature_verified, status,
	failure_reason, received_at, processed_at`

func scanTransaction(row scanner) (*model.PaymentTransaction, error) {
	item := &model.PaymentTransaction{}
	err := row.Scan(&item.ID, &item.LegacyID, &item.Kind, &item.PaymentNo, &item.RefundNo,
		&item.FundingLineNo, &item.LineType, &item.Direction, &item.Amount, &item.Provider,
		&item.ProviderTransactionID, &item.AccountEntryID, &item.OccurredAt, &item.CreatedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}

func scanTransition(row scanner) (*model.PaymentStateTransition, error) {
	item := &model.PaymentStateTransition{}
	err := row.Scan(&item.ID, &item.AggregateType, &item.AggregateID, &item.FromStatus,
		&item.ToStatus, &item.Reason, &item.RequestID, &item.ActorType, &item.ActorID,
		&item.Metadata, &item.CreatedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}

func scanProviderCall(row scanner) (*model.PaymentProviderCall, error) {
	item := &model.PaymentProviderCall{}
	err := row.Scan(&item.ID, &item.Provider, &item.Operation,
		&item.PaymentNo, &item.RefundNo, &item.AgreementNo, &item.RequestID, &item.TraceID,
		&item.AttemptNo, &item.RequestSummary, &item.ResponseSummary, &item.HTTPStatus,
		&item.ProviderCode, &item.ProviderMessage, &item.Result, &item.DurationMS,
		&item.CreatedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}

// scanNotification 把 body 原样扫成 []byte。
//
// **不在这一层转 string**：这里是表镜像，body 是 bytea。转成 string 是「怎么发出去」的
// 问题，归 dto（那里写明了不转的话会以 base64 出网）。
func scanNotification(row scanner) (*model.PaymentNotification, error) {
	item := &model.PaymentNotification{}
	err := row.Scan(&item.ID, &item.Provider, &item.NotificationID,
		&item.EventType, &item.PaymentNo, &item.RefundNo, &item.Body, &item.BodySHA256,
		&item.Headers, &item.SignatureVerified, &item.Status, &item.FailureReason,
		&item.ReceivedAt, &item.ProcessedAt)
	if err != nil {
		return nil, err
	}
	return item, nil
}
