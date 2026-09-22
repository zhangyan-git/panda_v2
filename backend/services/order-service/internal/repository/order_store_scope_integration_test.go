package repository

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orderStoreFixture 造一张挂到指定点位上的订单。只填列表查询会读到的那几列，其余走默认。
//
// 清理由它自己注册：跑完不留下任何一行（order_state_transitions 除外，那张表 append-only，
// 见 afterSaleIntegrationPool 的说明——这里不写流水，所以不会留）。
func orderStoreFixture(t *testing.T, pool *pgxpool.Pool, storeID string) string {
	t.Helper()
	orderID := uuid.NewString()
	_, err := pool.Exec(context.Background(), `INSERT INTO orders
		(id, order_no, user_id, source, store_id, status, original_amount, payable_amount)
		VALUES ($1,$2,$3,'miniapp',$4,'pending_payment',1000,1000)`,
		orderID, "SCOPE-"+orderID, uuid.NewString(), storeID)
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM orders WHERE id=$1`, orderID)
	})
	return orderID
}

// TestListOrdersStoreScopeIsNilVersusEmpty 钉住数据范围那一列的 nil/空语义。
//
// 两者必须给出相反的答案，而且这不是细节，是这一整套机制唯一会致命的地方：
//   - nil（后台那条路：没有数据范围这回事）→ 不过滤；
//   - 非 nil 空切片（账号一个点位都没授权）→ `= ANY('{}')` 恒假，命中零行。
//
// 把空切片写成「不过滤」的话，一个没被授权任何点位的账号登录后会看到全平台的订单，
// 而界面、日志、返回值都不会有任何异常——所以这条必须对着真 PG 验，不能只比字符串。
//
// 同时验 count 与列表用的是同一段 where：空范围那一条如果只筛了列表、没筛 count，会
// 得到 total=1、列表为空，翻页器上显示「共 1 条」却一条也翻不出来。
func TestListOrdersStoreScopeIsNilVersusEmpty(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, nil)
	ctx, _ := ctxWithTraceID(t, "")

	mine, other := uuid.NewString(), uuid.NewString()
	orderID := orderStoreFixture(t, pool, mine)
	orderNo := "SCOPE-" + orderID
	base := func(storeIDs []string) OrderFilter {
		// 用订单号把自己那一单框住：这个库是共用的 dev 库，不带订单号的话「nil 不过滤」
		// 这条断言只是在数别人的行。订单号是精确匹配，一样能证明有没有被范围筛掉。
		return OrderFilter{OrderNo: orderNo, StoreIDs: storeIDs, Page: 1, PageSize: 20}
	}

	cases := []struct {
		name     string
		storeIDs []string
		wantRows int
	}{
		// nil：后台那条路，行为与从前一字不差。
		{"nil 不过滤", nil, 1},
		// 空切片：一个点位都没授权的账号。
		{"空切片命中零行", []string{}, 0},
		// 范围内 / 范围外各一条，对称着写：只验「空切片是零行」的话，一个永远返回
		// 零行的实现也能过。
		{"范围内的点位", []string{mine}, 1},
		{"范围外的点位", []string{other}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := repo.ListOrders(ctx, base(tc.storeIDs))
			if err != nil {
				t.Fatalf("ListOrders = %v", err)
			}
			if len(rows) != tc.wantRows || total != tc.wantRows {
				t.Fatalf("rows=%d total=%d want %d —— count 与列表必须用同一段 where",
					len(rows), total, tc.wantRows)
			}
		})
	}
}

// 详情那条路的判定不在 SQL 里（它按 id 读整单，再在 service 里比范围），所以这里只钉住
// 「详情读得出来 store_id」这一件事：service 的越界判定就建立在这个字段上。
func TestFindOrderByIDCarriesTheStoreID(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, nil)
	ctx, _ := ctxWithTraceID(t, "")

	storeID := uuid.NewString()
	orderID := orderStoreFixture(t, pool, storeID)
	order, err := repo.FindOrderByID(ctx, orderID)
	if err != nil {
		t.Fatalf("FindOrderByID = %v", err)
	}
	if order.StoreID == nil || *order.StoreID != storeID {
		t.Fatalf("store_id=%v want %s", order.StoreID, storeID)
	}
}
