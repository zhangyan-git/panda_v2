package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// OrderFilter 是订单列表的筛选条件。零值 = 不筛。
//
// 时间用闭区间（>= from, < to），to 由调用方按「当天 23:59:59.999」或「下一天零点」给，
// 仓储不替它猜时区——用户界面上的「今天」是业务时区的今天，不是 UTC 的今天。
type OrderFilter struct {
	UserID   string
	OrderNo  string
	Status   string
	Source   string
	StoreID  string
	DeviceID string
	// StoreIDs 是**数据范围**，不是筛选栏里的一项。与 StoreID 的区别不是「一个还是多个」，
	// 而是「谁说了算」：StoreID 来自查询串，调用方自己选的；StoreIDs 是服务端从账号的
	// 授权点位展开出来的，调用方碰不到。
	//
	// 因此这里的 nil/空语义与上面那一排筛选值不同，而且是**故意的**：
	//   - nil（后台那条路，没有数据范围这回事）→ 不加条件，行为与从前一字不差；
	//   - 非 nil 空切片（账号一个点位都没授权）→ `= ANY('{}')` 恒假 → 命中零行。
	//
	// 写成 `coalesce(cardinality($n),0)=0 OR ...` 那种「空即不过滤」的惯用法是这里唯一
	// 会致命的错法：它会把「没有任何点位」读成「所有点位」。
	StoreIDs []string
	// 三个「有没有某类行」的筛选，零值 = 不筛。订单类型不存冗余列、由行派生（见迁移 001），
	// 所以每一个都是一条 EXISTS 子查询。后台的「咖啡订单 / 幸运杯套订单 / 会员订单」三个
	// 列表就是各自把其中一项钉成 true——一张单可以同时含三类行（V2 的合并订单），
	// 于是它会在多个列表里都出现，这是设计，不是重复数据。
	HasDrink      *bool
	HasAddon      *bool
	HasMembership *bool
	CreatedFrom   *time.Time
	CreatedTo     *time.Time
	Page          int
	PageSize      int
}

// lineExistsCondition 生成「这一单有没有某类行」的 EXISTS 条件。
//
// lineType 只允许传 model 里的常量：这个字符串是直接拼进 SQL 文本的，不能来自请求。
// 列表和 count 都要用同一份文本，所以只有这一个构造函数。
func lineExistsCondition(lineType string, present bool) string {
	condition := "EXISTS (SELECT 1 FROM order_lines l WHERE l.order_id = orders.id AND l.line_type = '" + lineType + "')"
	if !present {
		return "NOT " + condition
	}
	return condition
}

// checkUUIDFilter 校验一个要绑进 uuid 列的筛选值。空串 = 不筛；形状不对 = ErrInvalidUUIDFilter。
//
// 为什么不能只靠 SQL 那边：FindOrderByID 用的 `NULLIF(参数, 空串)::uuid` 解决的是
// 「空串不是 uuid，别拿它去撞类型检查」，而一个**非空但形状不对**的值（`?userId=abc`）照样
// 会让那条 cast 抛 22P02，调用方拿到 500。要在应用层拦，就拦在所有筛选值汇合的地方——列表
// 查询这一层，而不是三个后台入口各写一遍（漏掉一个就是一个 500）。
//
// 选择「拒绝」而不是「当成没筛」：后台这三个筛选框的语义是精确匹配，静默忽略等于把
// 「查这个人」变成「查所有人」，而调用方从响应里看不出区别。
func checkUUIDFilter(field, value string) error {
	if value == "" {
		return nil
	}
	if _, err := uuid.Parse(value); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidUUIDFilter, field)
	}
	return nil
}

// FindOrderByID 读一张订单的头部信息（不带行、不带流水）。
//
// 给「这个订单是不是他的」这类判定用：判定不需要把整单读出来，读全量再丢掉
// 只是把大订单的 IO 花在了一次布尔比较上。
func (r *PostgresRepository) FindOrderByID(ctx context.Context, id string) (*model.Order, error) {
	order, err := scanOrder(r.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE id = NULLIF($1,'')::uuid`, id))
	if err != nil {
		return nil, mapPGError(err)
	}
	return order, nil
}

// FindOrderByNo 按订单号读一张订单的头部信息。
//
// 与 FindOrderByID 是同一份投影的两个键：orderColumns 多一列或换顺序，两个函数都要跟着动，
// 放在一起才看得见这件事。
//
// 为什么要有这个键：发起支付那条路走的是**订单号**。它是这一单对外的标识——写进
// payments.order_no、印在渠道账单上、支付结果事件里回传的也是它——而调用方（小程序）手上
// 只有它。用内部 UUID 做那条路的路由，等于要求客户端先拿到一个它不需要知道的标识。
//
// order_no 上有唯一索引（orders_order_no_key），所以这是一次索引查找，不是扫描。
func (r *PostgresRepository) FindOrderByNo(ctx context.Context, orderNo string) (*model.Order, error) {
	order, err := scanOrder(r.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE order_no = $1`, orderNo))
	if err != nil {
		return nil, mapPGError(err)
	}
	return order, nil
}

// OrderDetail 是一张订单的全量：头 + 行 + 出资 + 状态流水 + 售后记录。
type OrderDetail struct {
	Order        *model.Order
	Lines        []*model.OrderLine
	PaymentLines []*model.OrderPaymentLine
	Transitions  []*model.OrderStateTransition
	// 这一单的售后申请，新的在前。用户与客服都在订单详情里看售后记录，所以不再单开一个
	// 用户侧列表接口——列表要的东西这里都有。
	AfterSales []*model.OrderAfterSale
}

// GetOrderDetail 读一张订单的全量。
//
// 五条查询不加事务：都是单表读，快照不一致（比如读到刚支付完的订单头、但流水还是旧的）
// 的窗口只有毫秒级，而详情页展示的是「此刻的样子」，不是一份需要严格一致的对账单。
// 真正需要一致的是写路径，那些在 service 的事务里。
func (r *PostgresRepository) GetOrderDetail(ctx context.Context, id string) (*OrderDetail, error) {
	order, err := r.FindOrderByID(ctx, id)
	if err != nil {
		return nil, err
	}
	detail := &OrderDetail{Order: order}

	lineRows, err := r.pool.Query(ctx,
		`SELECT `+orderLineColumns+` FROM order_lines WHERE order_id = $1 ORDER BY line_no`, order.ID)
	if err != nil {
		return nil, err
	}
	for lineRows.Next() {
		line, err := scanOrderLine(lineRows)
		if err != nil {
			lineRows.Close()
			return nil, err
		}
		detail.Lines = append(detail.Lines, line)
	}
	if err := lineRows.Err(); err != nil {
		lineRows.Close()
		return nil, err
	}
	lineRows.Close()

	paymentRows, err := r.pool.Query(ctx,
		`SELECT `+paymentLineColumns+` FROM order_payment_lines WHERE order_id = $1 ORDER BY line_no`, order.ID)
	if err != nil {
		return nil, err
	}
	for paymentRows.Next() {
		line, err := scanPaymentLine(paymentRows)
		if err != nil {
			paymentRows.Close()
			return nil, err
		}
		detail.PaymentLines = append(detail.PaymentLines, line)
	}
	if err := paymentRows.Err(); err != nil {
		paymentRows.Close()
		return nil, err
	}
	paymentRows.Close()

	// 状态流水只取订单本体的。付款失败与取消会同时写下 order 与 payment_line 两种聚合的流水，
	// 订单详情要讲的是「这一单怎么走到今天的」，把支付行的流水也混进来会把叙事打散；
	// 要查支付行自己的变迁，按 (aggregate_type, aggregate_id) 单独查。
	transitionRows, err := r.pool.Query(ctx,
		`SELECT `+transitionColumns+` FROM order_state_transitions
		 WHERE aggregate_type='order' AND aggregate_id=$1 ORDER BY created_at, id`, order.ID)
	if err != nil {
		return nil, err
	}
	for transitionRows.Next() {
		transition, err := scanTransition(transitionRows)
		if err != nil {
			return nil, err
		}
		detail.Transitions = append(detail.Transitions, transition)
	}
	if err := transitionRows.Err(); err != nil {
		return nil, err
	}
	transitionRows.Close()

	// 售后单不带派生列（福卡承诺/行摘要）：那是审核要看的，用户在自己的订单详情里看到的
	// 是「我申请过几次、到哪一步了」。要审核视图的人走 admin 的售后列表。
	afterSaleRows, err := r.pool.Query(ctx,
		`SELECT `+afterSaleColumns+` FROM order_after_sales a
		 WHERE a.order_id=$1 ORDER BY a.created_at DESC, a.id`, order.ID)
	if err != nil {
		return nil, err
	}
	defer afterSaleRows.Close()
	for afterSaleRows.Next() {
		item, err := scanAfterSale(afterSaleRows)
		if err != nil {
			return nil, err
		}
		detail.AfterSales = append(detail.AfterSales, item)
	}
	if err := afterSaleRows.Err(); err != nil {
		return nil, err
	}
	return detail, nil
}

// ListOrderLines 只读一张订单的行，别的什么都不读。
//
// 存在它的理由：发起支付那条路上要的是「这一单有哪些类型的行」（分账规则的 biz_type 由
// 行推出来，见 service 的 settlementBizType），而发起支付是用户点「支付」的热路径。
// 复用 GetOrderDetail 会把订单头、出资行、状态流水、售后记录一起读出来——五条查询换一个
// 字段，其中三条与这次调用毫无关系。
//
// 行的形状与 GetOrderDetail 里那份**一模一样**（同一个 orderLineColumns、同一个
// scanOrderLine、同一个 ORDER BY line_no）：两个方法读的是同一批数据，投影或排序分了叉，
// 表现是「详情页看到的行与分账算出来的不是一回事」，而那种错很难从结果上看出来。
func (r *PostgresRepository) ListOrderLines(ctx context.Context, orderID string) ([]*model.OrderLine, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+orderLineColumns+` FROM order_lines WHERE order_id = $1 ORDER BY line_no`, orderID)
	if err != nil {
		return nil, mapPGError(err)
	}
	defer rows.Close()

	var lines []*model.OrderLine
	for rows.Next() {
		line, err := scanOrderLine(rows)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// OrderRow 是列表里的一行：订单头部，外加三个从行派生出来的标记。
//
// 标记为什么由这里给而不是在应用层从行里推：列表根本没读行（见下），而后台要按
// 「咖啡订单 / 幸运杯套订单 / 会员订单」分类展示、列表里还要能看出「这单还含什么」。
// 用三条 EXISTS 顺手算出来，比为了三个布尔值把每一行都读出来划算。
type OrderRow struct {
	Order             *model.Order
	HasDrinkLine      bool
	HasAddonLine      bool
	HasMembershipLine bool
	// 取杯号也由列表顺手带出来（列表不读行，见下）。没有饮品行或还没付成功就是 nil。
	PickupCode *string
}

// orderRowFlagColumns 是列表 SELECT 里跟在 orderColumns 后面的那三列派生标记。
//
// 顺序与 scanOrderWith 的附加参数顺序一一对应（drink / addon / membership），
// 而且两边都由 lineExistsCondition 生成：列和扫描目标错位是静默的——布尔值会串位，
// 谁也没报错，只是「会员订单」列表里混进了咖啡单。
var orderRowFlagColumns = strings.Join([]string{
	lineExistsCondition(model.LineTypeDrink, true),
	lineExistsCondition(model.LineTypeAddon, true),
	lineExistsCondition(model.LineTypeMembership, true),
}, ", ")

// orderRowPickupColumn 是列表 SELECT 里最后那一列：这一单的取杯号。
//
// 用标量子查询而不是把 order_lines JOIN 进来，理由与上面三个 EXISTS 相同（见 ListOrders 的
// 注释）：饮品行一单可以有好几行，JOIN 会让订单在结果里重复，叠上 LIMIT 就跳着翻页。
// 走 order_lines_order_idx (order_id, line_no)，取 line_no 最小的那行——一单的饮品行共用
// 一个取杯号，真要出现不一致也该是稳定地取第一行而不是随机一行。
const orderRowPickupColumn = `(SELECT l.pickup_code FROM order_lines l
	WHERE l.order_id = orders.id AND l.pickup_code IS NOT NULL
	ORDER BY l.line_no LIMIT 1)`

// ListOrders 按筛选条件分页读订单头部（不含行）。
//
// 列表页不取行：一页 20 单、每单最多 3 行，一次 JOIN 出 60 行再在应用里拼装，比多 60 倍的
// 传输量换来的只是省一次查询。需要行的时候（详情、导出）走 GetOrderDetail。
func (r *PostgresRepository) ListOrders(ctx context.Context, f OrderFilter) ([]*OrderRow, int, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	// 三个 uuid 列的筛选值先验形状：不验的话它们会被直接绑进 uuid 列，PG 抛 22P02，
	// 一次「筛选框里少打一个字符」变成 500。逐个写而不是遍历一张表：同一个请求里有两个
	// 字段都写错时，报哪一个应当是确定的。
	if err := checkUUIDFilter("userId", f.UserID); err != nil {
		return nil, 0, err
	}
	if err := checkUUIDFilter("storeId", f.StoreID); err != nil {
		return nil, 0, err
	}
	if err := checkUUIDFilter("deviceId", f.DeviceID); err != nil {
		return nil, 0, err
	}

	// 参数按顺序追加，$n 跟着 len(args) 走：手写编号最容易在加条件时错位。
	var args []any
	conditions := make([]string, 0, 8)
	bind := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.UserID != "" {
		conditions = append(conditions, "user_id = "+bind(f.UserID))
	}
	if f.OrderNo != "" {
		conditions = append(conditions, "order_no = "+bind(f.OrderNo))
	}
	if f.Status != "" {
		conditions = append(conditions, "status = "+bind(f.Status))
	}
	if f.Source != "" {
		conditions = append(conditions, "source = "+bind(f.Source))
	}
	if f.StoreID != "" {
		conditions = append(conditions, "store_id = "+bind(f.StoreID))
	}
	if f.DeviceID != "" {
		conditions = append(conditions, "device_id = "+bind(f.DeviceID))
	}
	// 数据范围：只有 nil 才是不筛（见 OrderFilter.StoreIDs）。这一条与上面那几条不同，
	// 它没有对应的「空串 = 不筛」写法，因为空切片必须是一条恒假的条件而不是缺席，
	// 而且它落在 where 里就意味着下面的 count 与列表用的是同一段条件——「先取全量再在
	// 内存里筛」那种写法会让 total 与列表对不上，在这里被一并挡掉。
	if f.StoreIDs != nil {
		conditions = append(conditions, "store_id::text = ANY("+bind(f.StoreIDs)+"::text[])")
	}
	if f.CreatedFrom != nil {
		conditions = append(conditions, "created_at >= "+bind(*f.CreatedFrom))
	}
	if f.CreatedTo != nil {
		conditions = append(conditions, "created_at < "+bind(*f.CreatedTo))
	}
	// 用 EXISTS 而不是把订单行 JOIN 进来：加购行一单可以有好几行，JOIN 进主表会让这一单
	// 在结果里出现好几次（再叠上 LIMIT，翻页就跳着走）；EXISTS 还能在拿到第一条时就停，
	// 且不影响下面的 count。
	if f.HasDrink != nil {
		conditions = append(conditions, lineExistsCondition(model.LineTypeDrink, *f.HasDrink))
	}
	if f.HasAddon != nil {
		conditions = append(conditions, lineExistsCondition(model.LineTypeAddon, *f.HasAddon))
	}
	if f.HasMembership != nil {
		conditions = append(conditions, lineExistsCondition(model.LineTypeMembership, *f.HasMembership))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM orders`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		// 一条都没有时不必再查一次：分页参数可能超出范围，结果必然是空。
		return nil, 0, nil
	}

	pageArgs := append(append([]any{}, args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.pool.Query(ctx, `SELECT `+orderColumns+`, `+orderRowFlagColumns+`, `+orderRowPickupColumn+`
		FROM orders`+where+
		fmt.Sprintf(" ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	orders := make([]*OrderRow, 0, f.PageSize)
	for rows.Next() {
		row := &OrderRow{}
		// 复用 scanOrder 的列顺序，再顺手读三个标记与取杯号：两边的列顺序必须一起改
		// （orderRowFlagColumns 与 orderRowPickupColumn 是那次改动的唯一入口），所以追加的
		// 列只能跟在 orderColumns 后面，不能插进中间。
		order, err := scanOrderWith(rows, &row.HasDrinkLine, &row.HasAddonLine,
			&row.HasMembershipLine, &row.PickupCode)
		if err != nil {
			return nil, 0, err
		}
		row.Order = order
		orders = append(orders, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return orders, total, nil
}
