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
// 结束后按 id 精确删除。没有 USER_DATABASE_URL / TEST_DATABASE_URL 时整组跳过。
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("USER_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set USER_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("user test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("user test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
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

// assertDistinct 断言翻页结果恰好覆盖 want 里的每一条、且没有重复。
func assertDistinct(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("逐页翻出 %d 条, 期望 %d 条 (%v)", len(got), len(want), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("id %s 在两页里重复出现: %v", id, got)
		}
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("id %s 一页都没翻到: %v", id, got)
		}
	}
}

// TestMerchantUserCountScopedToMerchant 验证 Count 与 FindPage 对 merchantID
// 的处理一致。
//
// 这是这批改动里唯一带筛选条件的 count：FindPage 和 Count 都收 merchantID，
// 少传一次就会把别的商户的账号算进来——症状是分页器上的总条数比实际能翻到的
// 多，而且不报错。所以夹具刻意放两个商户的账号。
// created_at 全部相同：批量建号时时间戳撞在一起很常见，少了 ORDER BY 的
// id 决胜位，同一行会在相邻两页各出现一次、另一行一次都不出现。
func TestMerchantUserCountScopedToMerchant(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewMerchantUserRepository(pool, nil)
	prefix := "it-pg-" + uuid.NewString()[:8] + "-"

	merchantA, merchantB := uuid.NewString(), uuid.NewString()
	idsA := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id := uuid.NewString()
		idsA = append(idsA, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO merchant_users (id, merchant_id, username, password_hash, name, created_at, updated_at)
			VALUES ($1, $2, $3, 'x', $4, TIMESTAMPTZ '2020-01-01 00:00:00+00', TIMESTAMPTZ '2020-01-01 00:00:00+00')`,
			id, merchantA, fmt.Sprintf("%sa%d", prefix, i), prefix+"A"); err != nil {
			t.Fatalf("插入商户账号夹具: %v", err)
		}
	}
	idsB := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		id := uuid.NewString()
		idsB = append(idsB, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO merchant_users (id, merchant_id, username, password_hash, name)
			VALUES ($1, $2, $3, 'x', $4)`,
			id, merchantB, fmt.Sprintf("%sb%d", prefix, i), prefix+"B"); err != nil {
			t.Fatalf("插入商户账号夹具: %v", err)
		}
	}
	allIDs := append(append([]string{}, idsA...), idsB...)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM merchant_users WHERE id = ANY($1::uuid[])`, allIDs); err != nil {
			t.Errorf("清理商户账号夹具: %v", err)
		}
	})

	total, err := repo.Count(ctx, merchantA)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if total != 4 {
		t.Fatalf("商户 A 的账号数应为 4, got %d", total)
	}

	// pageSize=3 切出 3+1 两页，是决胜位最容易出问题的边界。
	got := pageAll(t, 3, func(limit, offset int) ([]string, error) {
		rows, err := repo.FindPage(ctx, merchantA, limit, offset)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(rows))
		for i, u := range rows {
			out[i] = u.ID
		}
		return out, nil
	})
	assertDistinct(t, got, idsA)
	for _, id := range got {
		for _, other := range idsB {
			if id == other {
				t.Fatal("FindPage 返回了别的商户的账号")
			}
		}
	}

	// 同一个 FindPage/Count 组合换一个商户，两侧都必须跟着换。
	if n, err := repo.Count(ctx, merchantB); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 2 {
		t.Fatalf("商户 B 的账号数应为 2, got %d", n)
	}
	if n, err := repo.Count(ctx, uuid.NewString()); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 0 {
		t.Fatalf("不存在的商户应返回 0, got %d", n)
	}
}

// TestAdminUserCountMatchesPages 验证平台管理员列表的 Count 与逐页行数一致。
//
// admin_users 的 FindPage/Count 都不带筛选，所以这里防的不是「条件漏传」，
// 而是「总数与实际能翻到的行数对不上」——夹具共用同一个 created_at，
// 逼出 ORDER BY created_at, id 里的 id 决胜位。
func TestAdminUserCountMatchesPages(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repo := NewAdminUserRepository(pool, nil)
	prefix := "it-pg-" + uuid.NewString()[:8] + "-"

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO admin_users (id, username, password_hash, name, email, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, $4, TIMESTAMPTZ '2020-01-01 00:00:00+00', TIMESTAMPTZ '2020-01-01 00:00:00+00')`,
			id, fmt.Sprintf("%s%d", prefix, i), prefix+"管理员", fmt.Sprintf("%s%d@test.local", prefix, i)); err != nil {
			t.Fatalf("插入管理员夹具: %v", err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM admin_users WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Errorf("清理管理员夹具: %v", err)
		}
	})

	total, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	// Count 不带筛选，库里本来就有种子管理员，所以只断言「至少包含夹具的 3 条」。
	if total < 3 {
		t.Fatalf("总数至少应为 3, got %d", total)
	}

	// 夹具之外还有种子数据，翻页拿不全，所以这里只断言夹具三条都能被翻到、
	// 且整轮翻页没有重复——重复正是决胜位缺失的症状。
	got := pageAll(t, 2, func(limit, offset int) ([]string, error) {
		rows, err := repo.FindPage(ctx, limit, offset)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(rows))
		for i, u := range rows {
			out[i] = u.ID
		}
		return out, nil
	})
	if int64(len(got)) != total {
		t.Fatalf("逐页翻出 %d 条, Count 说有 %d 条", len(got), total)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("id %s 在两页里重复出现", id)
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("夹具管理员 %s 一页都没翻到", id)
		}
	}
}
