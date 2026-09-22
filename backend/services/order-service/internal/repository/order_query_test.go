package repository

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 这一份验的是后台列表那三个 uuid 筛选框的形状校验：写错一个字符要拿到一条「请求不合法」，
// 而不是一个 500。
//
// 走真的 dev 库（夹具与跳过规则见 after_sale_integration_test.go 的 afterSaleIntegrationPool）：
// 要断言的不只是「返回了哪个错误」，还有「这个错误不是 PG 抛的 22P02」——后者正是原来那条
// 500 的模样，只有真的把值交给 PG 才能证明它没被交出去。

// assertRejectedAsFilter 断言一个筛选值被应用层挡下，而不是被 PG 的类型检查挡下。
func assertRejectedAsFilter(t *testing.T, where string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：非 uuid 的筛选值被当成了一次合法查询", where)
	}
	if !errors.Is(err, ErrInvalidUUIDFilter) {
		t.Fatalf("%s：得到 %v，期望 ErrInvalidUUIDFilter", where, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("%s：筛选值还是进了 SQL（PG %s：%s）——它会以 500 的形式回到调用方",
			where, pgErr.Code, pgErr.Message)
	}
}

// TestListOrdersRejectsMalformedUUIDFilters 覆盖订单列表的三个 uuid 列筛选。
//
// 这条用例在 PG 之前就该返回：真让 `?userId=abc` 进到 `user_id = $1` 里，PG 抛 22P02
// （invalid input syntax for type uuid），controller 那边没有任何 case 认它，于是回 500
// ——运维看到的是「服务器错误」，而真正要改的只是筛选框里那个字符。
func TestListOrdersRejectsMalformedUUIDFilters(t *testing.T) {
	repo := NewPostgresRepository(afterSaleIntegrationPool(t), nil)
	ctx, _ := ctxWithTraceID(t, "")

	cases := []struct {
		field  string
		filter OrderFilter
	}{
		{"userId", OrderFilter{UserID: "abc", Page: 1, PageSize: 20}},
		{"userId（半截 uuid）", OrderFilter{UserID: "2f1c9a3e-0000", Page: 1, PageSize: 20}},
		{"storeId", OrderFilter{StoreID: "not-a-uuid", Page: 1, PageSize: 20}},
		{"deviceId", OrderFilter{DeviceID: "12345", Page: 1, PageSize: 20}},
	}
	for _, tc := range cases {
		_, _, err := repo.ListOrders(ctx, tc.filter)
		assertRejectedAsFilter(t, "ListOrders "+tc.field, err)
	}
}

// TestListAfterSalesRejectsMalformedUUIDFilters 是售后单列表那一个（userId 是它唯一的
// uuid 列筛选）。
func TestListAfterSalesRejectsMalformedUUIDFilters(t *testing.T) {
	repo := NewPostgresRepository(afterSaleIntegrationPool(t), nil)
	ctx, _ := ctxWithTraceID(t, "")

	_, _, err := repo.ListAfterSales(ctx, AfterSaleFilter{UserID: "abc", Page: 1, PageSize: 20})
	assertRejectedAsFilter(t, "ListAfterSales userId", err)
}

// TestListFiltersAcceptEmptyAndWellFormedUUIDs 是上一条的反面：**空串仍然是「不筛」**，
// 一个形状正确的 uuid 仍然是「精确匹配」。
//
// 少了它，把校验写成「只要不是 uuid 就报错」也能让上面那些用例通过——而那样一来，后台打开
// 列表页（不带任何筛选）就会直接 400。
func TestListFiltersAcceptEmptyAndWellFormedUUIDs(t *testing.T) {
	repo := NewPostgresRepository(afterSaleIntegrationPool(t), nil)
	ctx, _ := ctxWithTraceID(t, "")

	// 空值 = 不筛：三个字段全空，应当正常返回（这一页多半是空的，不是这里要验的）。
	if _, _, err := repo.ListOrders(ctx, OrderFilter{Page: 1, PageSize: 20}); err != nil {
		t.Fatalf("不带筛选的订单列表不该报错：%v", err)
	}
	// 形状正确的 uuid = 一次精确匹配，只是查不到东西。
	missing := "00000000-0000-0000-0000-000000000000"
	if _, _, err := repo.ListOrders(ctx, OrderFilter{
		UserID: missing, StoreID: missing, DeviceID: missing, Page: 1, PageSize: 20,
	}); err != nil {
		t.Fatalf("形状正确的 uuid 筛选不该报错：%v", err)
	}
	if _, _, err := repo.ListAfterSales(ctx, AfterSaleFilter{Page: 1, PageSize: 20}); err != nil {
		t.Fatalf("不带筛选的售后单列表不该报错：%v", err)
	}
	if _, _, err := repo.ListAfterSales(ctx, AfterSaleFilter{
		UserID: missing, Page: 1, PageSize: 20,
	}); err != nil {
		t.Fatalf("形状正确的 uuid 筛选不该报错：%v", err)
	}
}
