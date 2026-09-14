package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	rows, err := r.pool.Query(ctx, `SELECT `+orderColumns+`, `+orderRowFlagColumns+`
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
		// 复用 scanOrder 的列顺序，再顺手读三个标记：两边的列顺序必须一起改
		// （orderRowFlagColumns 是那次改动的唯一入口），所以追加的列只能跟在
		// orderColumns 后面，不能插进中间。
		order, err := scanOrderWith(rows, &row.HasDrinkLine, &row.HasAddonLine, &row.HasMembershipLine)
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
