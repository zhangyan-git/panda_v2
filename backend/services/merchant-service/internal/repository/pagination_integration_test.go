package repository

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 这些用例只连调用方给的库，不建库、不跑迁移；夹具用唯一前缀隔离，
// 结束后按依赖倒序删干净。没有 DATABASE_URL 时整组跳过。
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("MERCHANT_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set MERCHANT_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("merchant test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("merchant test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixturePrefix 每个用例一个唯一前缀，用来把夹具和库里已有数据隔开——
// name 是 ILIKE 模糊匹配，写死的前缀会捞到别人的行。
func fixturePrefix(t *testing.T) string {
	t.Helper()
	return "it-pg-" + uuid.NewString()[:8] + "-"
}

// pageAll 按 pageSize 翻完所有页，返回全量 id 序列。
//
// 逐个翻页而不是一次性拿：要验证的正是「翻页本身会不会重复或漏行」。
// offset 超过总数时 FindPage 返回空切片，就是终止条件。
func pageAll(t *testing.T, pageSize int, fetch func(limit, offset int) ([]string, error)) []string {
	t.Helper()
	var ids []string
	for page := 1; ; page++ {
		rows, err := fetch(pageSize, (page-1)*pageSize)
		if err != nil {
			t.Fatalf("第 %d 页查询失败: %v", page, err)
		}
		if len(rows) == 0 {
			return ids
		}
		ids = append(ids, rows...)
		if page > 1000 {
			t.Fatal("翻页没有终止，可能是 OFFSET 算错")
		}
	}
}

// TestMerchantCountMatchesPages 验证 Count 与逐页翻出的行数一致。
//
// 两个查询的条件由 merchantWhere 共用，但「共用」这件事本身没有编译期保证——
// 真出现偏差，症状是分页器上写着 5 条、翻到第 3 页却空了，且不报错。
// 夹具刻意共用同一个 created_at：批量导入的商户时间戳撞在一起很常见，
// 少了 ORDER BY 的 id 决胜位，同一行会在相邻两页各出现一次、另一行一次都不出现。
func TestMerchantCountMatchesPages(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewMerchantRepository(pool, nil)
	prefix := fixturePrefix(t)

	// 5 条商户：3 条 active、2 条 suspended，created_at 全部相同。
	ids := make([]string, 0, 5)
	statuses := []string{"active", "active", "active", "suspended", "suspended"}
	for i, status := range statuses {
		id := uuid.NewString()
		ids = append(ids, id)
		name := fmt.Sprintf("%s%d", prefix, i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO merchants (id, name, status, created_at, updated_at)
			VALUES ($1, $2, $3, TIMESTAMPTZ '2020-01-01 00:00:00+00', TIMESTAMPTZ '2020-01-01 00:00:00+00')`,
			id, name, status); err != nil {
			t.Fatalf("插入商户夹具: %v", err)
		}
	}
	t.Cleanup(func() { deleteMerchants(t, pool, ids) })

	total, err := repo.Count(ctx, prefix, "")
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if total != 5 {
		t.Fatalf("Count 应为 5, got %d", total)
	}

	// pageSize=3 会切出 3+2 两页，正是决胜位最容易出问题的边界。
	got := pageAll(t, 3,
		func(limit, offset int) ([]string, error) {
			rows, err := repo.FindPage(ctx, prefix, "", limit, offset)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(rows))
			for i, m := range rows {
				out[i] = m.ID
			}
			return out, nil
		})

	if len(got) != 5 {
		t.Fatalf("逐页翻出 5 条, got %d (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("商户 %s 在两页里重复出现: %v", id, got)
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("商户 %s 一页都没翻到: %v", id, got)
		}
	}

	// 带 status 过滤时，Count 与翻页结果必须仍然一致。
	filtered, err := repo.Count(ctx, prefix, "active")
	if err != nil {
		t.Fatalf("带筛选的 Count 出错: %v", err)
	}
	if filtered != 3 {
		t.Fatalf("active 商户应为 3, got %d", filtered)
	}
	pageOne, err := repo.FindPage(ctx, prefix, "active", 100, 0)
	if err != nil {
		t.Fatalf("带筛选的 FindPage 出错: %v", err)
	}
	if int64(len(pageOne)) != filtered {
		t.Fatalf("Count(%d) 与同一组筛选下 FindPage 的行数(%d) 不一致", filtered, len(pageOne))
	}
}

// TestStoreFindPageAllFilters 覆盖 storeConds 的占位符编号。
//
// storeConds 里 brandId 是在 buildFilter 之外单独 append 的，因为它不在那四个
// 通用条件里。这正是最容易数错编号的地方：buildFilter 按 len(args) 生成 $N，
// 一旦品牌条件插队，LIMIT/OFFSET 的编号就会错位——症状是查询直接报错，
// 或者更糟：参数按错位顺序绑定，筛出的是另一批门店。
// 所以这里把五个筛选字段一次全填上，逼出编号路径。
func TestStoreFindPageAllFilters(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	storeRepo := NewStoreRepository(pool, nil)
	prefix := fixturePrefix(t)

	merchantID, brandID, otherBrandID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, status) VALUES ($1, $2, 'active')`,
		merchantID, prefix+"merchant"); err != nil {
		t.Fatalf("插入商户夹具: %v", err)
	}
	for _, b := range []string{brandID, otherBrandID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO brands (id, merchant_id, name, status, audit_status)
			VALUES ($1, $2, $3, 'active', 'pending')`, b, merchantID, prefix+b[:8]); err != nil {
			t.Fatalf("插入品牌夹具: %v", err)
		}
	}

	// 3 家门店挂在 brandID 下（created_at 相同），另 1 家挂 otherBrandID——
	// 后者用来确认 brandId 真的参与了筛选，而不是被当成未绑定的参数忽略掉。
	storeIDs := make([]string, 0, 4)
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		storeIDs = append(storeIDs, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO stores (id, merchant_id, brand_id, name, status, audit_status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'active', 'pending', TIMESTAMPTZ '2020-01-01 00:00:00+00', TIMESTAMPTZ '2020-01-01 00:00:00+00')`,
			id, merchantID, brandID, fmt.Sprintf("%sstore-%d", prefix, i)); err != nil {
			t.Fatalf("插入门店夹具: %v", err)
		}
	}
	decoyID := uuid.NewString()
	storeIDs = append(storeIDs, decoyID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO stores (id, merchant_id, brand_id, name, status, audit_status)
		VALUES ($1, $2, $3, $4, 'active', 'pending')`,
		decoyID, merchantID, otherBrandID, prefix+"decoy"); err != nil {
		t.Fatalf("插入对照门店夹具: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM stores WHERE id = ANY($1::uuid[])`, storeIDs); err != nil {
			t.Errorf("清理门店夹具: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `DELETE FROM brands WHERE merchant_id = $1`, merchantID); err != nil {
			t.Errorf("清理品牌夹具: %v", err)
		}
		deleteMerchants(t, pool, []string{merchantID})
	})

	// 五个筛选字段全填：merchantId + brandId + name(前缀) + status + auditStatus。
	f := StoreFilter{
		MerchantID:  merchantID,
		BrandID:     brandID,
		Name:        prefix,
		Status:      "active",
		AuditStatus: "pending",
	}
	got := pageAll(t, 2,
		func(limit, offset int) ([]string, error) {
			rows, err := storeRepo.FindPage(ctx, f, limit, offset)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(rows))
			for i, s := range rows {
				out[i] = s.ID
			}
			return out, nil
		})

	if len(got) != 3 {
		t.Fatalf("brandId 应把对照门店排除在外，期望 3 家, got %d (%v)", len(got), got)
	}
	for _, id := range got {
		if id == decoyID {
			t.Fatalf("brandId 未生效：另一品牌下的门店被查了出来")
		}
	}

	total, err := storeRepo.Count(ctx, f)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if total != 3 {
		t.Fatalf("Count 应为 3, got %d", total)
	}

	// 前缀唯一，换个名称应该一条都匹配不到——确认 name 条件也真的在生效，
	// 而不是五个条件里只用了前几个。
	if n, err := storeRepo.Count(ctx, StoreFilter{MerchantID: merchantID, Name: prefix + "nope"}); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 0 {
		t.Fatalf("不存在的名称前缀应匹配 0 条, got %d", n)
	}
}

// TestBrandCountMatchesPages 同 TestMerchantCountMatchesPages，覆盖品牌的
// b.sort 排序：夹具的 sort 全部相同，决胜位只能靠 b.id。
func TestBrandCountMatchesPages(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewBrandRepository(pool, nil)
	prefix := fixturePrefix(t)

	merchantID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, status) VALUES ($1, $2, 'active')`,
		merchantID, prefix+"merchant"); err != nil {
		t.Fatalf("插入商户夹具: %v", err)
	}
	brandIDs := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id := uuid.NewString()
		brandIDs = append(brandIDs, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO brands (id, merchant_id, name, status, audit_status, sort, created_at, updated_at)
			VALUES ($1, $2, $3, 'active', 'pending', 0, TIMESTAMPTZ '2020-01-01 00:00:00+00', TIMESTAMPTZ '2020-01-01 00:00:00+00')`,
			id, merchantID, fmt.Sprintf("%s%d", prefix, i)); err != nil {
			t.Fatalf("插入品牌夹具: %v", err)
		}
	}
	// brands.merchant_id 是 ON DELETE CASCADE，删商户会带走品牌。
	t.Cleanup(func() { deleteMerchants(t, pool, []string{merchantID}) })

	f := BrandFilter{MerchantID: merchantID, Name: prefix}
	total, err := repo.Count(ctx, f)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if total != 4 {
		t.Fatalf("Count 应为 4, got %d", total)
	}

	got := pageAll(t, 1,
		func(limit, offset int) ([]string, error) {
			rows, err := repo.FindPage(ctx, f, limit, offset)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(rows))
			for i, b := range rows {
				out[i] = b.ID
			}
			return out, nil
		})

	if len(got) != 4 {
		t.Fatalf("逐页翻出 4 条, got %d (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("品牌 %s 在两页里重复出现: %v", id, got)
		}
		seen[id] = true
	}
}

// deleteMerchants 按依赖倒序清理夹具：门店和品牌都挂在商户上，
// 直接删商户会因为 stores.brand_id 的 ON DELETE RESTRICT 报错。
func deleteMerchants(t *testing.T, pool *pgxpool.Pool, ids []string) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM stores WHERE merchant_id = ANY($1::uuid[])`, ids); err != nil {
		t.Errorf("清理门店夹具: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM brands WHERE merchant_id = ANY($1::uuid[])`, ids); err != nil {
		t.Errorf("清理品牌夹具: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM merchants WHERE id = ANY($1::uuid[])`, ids); err != nil {
		t.Errorf("清理商户夹具: %v", err)
	}
}
