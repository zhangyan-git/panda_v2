package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

// scopeFixture 是一组「两个商户、各带品牌与门店」的夹具。两个商户都在，是因为
// 这一组用例里最要紧的一条是**跨商户不能互相看见**：只有一个商户的话，
// 少了 merchant_id 锚点的查询会照样通过。
type scopeFixture struct {
	merchantA string
	merchantB string
	brandA1   string
	brandA2   string
	brandB1   string
	storesA1  []string // brandA1 下两家
	storeA2   string   // brandA2 下一家
	storeB1   string   // 商户 B 的点位
}

func seedScopeFixture(t *testing.T, pool *pgxpool.Pool) scopeFixture {
	t.Helper()
	ctx := context.Background()
	prefix := fixturePrefix(t)
	f := scopeFixture{
		merchantA: uuid.NewString(),
		merchantB: uuid.NewString(),
		brandA1:   uuid.NewString(),
		brandA2:   uuid.NewString(),
		brandB1:   uuid.NewString(),
		storesA1:  []string{uuid.NewString(), uuid.NewString()},
		storeA2:   uuid.NewString(),
		storeB1:   uuid.NewString(),
	}

	for _, m := range []string{f.merchantA, f.merchantB} {
		if _, err := pool.Exec(ctx, `INSERT INTO merchants (id, name, status) VALUES ($1, $2, 'active')`,
			m, prefix+"merchant"); err != nil {
			t.Fatalf("插入商户夹具: %v", err)
		}
	}
	for brand, merchant := range map[string]string{
		f.brandA1: f.merchantA,
		f.brandA2: f.merchantA,
		f.brandB1: f.merchantB,
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO brands (id, merchant_id, name, status, audit_status)
			VALUES ($1, $2, $3, 'active', 'pending')`, brand, merchant, prefix+brand[:8]); err != nil {
			t.Fatalf("插入品牌夹具: %v", err)
		}
	}
	insert := func(store, merchant, brand string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO stores (id, merchant_id, brand_id, name, status, audit_status)
			VALUES ($1, $2, $3, $4, 'active', 'pending')`, store, merchant, brand, prefix+store[:8]); err != nil {
			t.Fatalf("插入门店夹具: %v", err)
		}
	}
	for _, s := range f.storesA1 {
		insert(s, f.merchantA, f.brandA1)
	}
	insert(f.storeA2, f.merchantA, f.brandA2)
	insert(f.storeB1, f.merchantB, f.brandB1)

	// deleteMerchants 按依赖倒序清门店、品牌、商户，这里不再重复那两条 DELETE。
	t.Cleanup(func() { deleteMerchants(t, pool, []string{f.merchantA, f.merchantB}) })
	return f
}

// findIDs 按筛选条件取一页 id，用来比对集合而不依赖顺序。
func findIDs(t *testing.T, repo StoreRepository, f StoreFilter, limit, offset int) []string {
	t.Helper()
	rows, err := repo.FindPage(context.Background(), f, limit, offset)
	if err != nil {
		t.Fatalf("FindPage 出错: %v", err)
	}
	out := make([]string, len(rows))
	for i, s := range rows {
		out[i] = s.ID
	}
	return out
}

// TestStoreFilterStoreIDsDistinguishesNilFromEmpty 是本刀最要紧的一条语义测试。
//
// nil 与空切片在 Go 里只差一个长度，落到 SQL 上是 NULL 与 '{}'——
// 前者「不过滤」，后者「一条都不给」。把它们写成同一个分支（例如照搬
// coalesce(cardinality($2),0)=0 那个惯用法），没被授权任何点位的账号就会看到全平台，
// 而错误表现是一片安静：列表照常返回，只是多了别人的行。
func TestStoreFilterStoreIDsDistinguishesNilFromEmpty(t *testing.T) {
	pool := integrationPool(t)
	fx := seedScopeFixture(t, pool)
	ctx := context.Background()
	repo := NewStoreRepository(pool, nil)

	// nil：后台那条路。数据范围不参与，商户 A 的三家点位全部可见。
	nilFilter := StoreFilter{MerchantID: fx.merchantA}
	got := findIDs(t, repo, nilFilter, 100, 0)
	if len(got) != 3 {
		t.Fatalf("StoreIDs 为 nil 时应返回该商户全部 3 家点位, got %d (%v)", len(got), got)
	}
	if n, err := repo.Count(ctx, nilFilter); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 3 {
		t.Fatalf("nil 时 Count 应为 3, got %d", n)
	}

	// 非 nil 空切片：一个点位都没授权。列表与 total 都必须归零，
	// total 单独断言是因为「先取全量再在内存里过滤」正好只错在这一点上。
	emptyFilter := StoreFilter{MerchantID: fx.merchantA, StoreIDs: []string{}}
	if got := findIDs(t, repo, emptyFilter, 100, 0); len(got) != 0 {
		t.Fatalf("空切片应命中零行, got %d (%v)", len(got), got)
	}
	if n, err := repo.Count(ctx, emptyFilter); err != nil {
		t.Fatalf("Count 出错: %v", err)
	} else if n != 0 {
		t.Fatalf("空切片时 Count 应为 0, got %d", n)
	}

	// 单点范围：只有那一家。同一商户的其他点位必须被挡在外面。
	oneFilter := StoreFilter{MerchantID: fx.merchantA, StoreIDs: []string{fx.storeA2}}
	got = findIDs(t, repo, oneFilter, 100, 0)
	if len(got) != 1 || got[0] != fx.storeA2 {
		t.Fatalf("单点范围应只返回 %s, got %v", fx.storeA2, got)
	}

	// 范围外的点位：即便商户对、id 对，也要落空——这是「越界返回 404」的库层依据。
	foreign := StoreFilter{MerchantID: fx.merchantA, StoreIDs: []string{fx.storeB1}}
	if got := findIDs(t, repo, foreign, 100, 0); len(got) != 0 {
		t.Fatalf("另一个商户的点位不应被查出来, got %v", got)
	}
}

// TestStoreFindPageRespectsScopeAcrossPages 把范围与分页放在一起：范围必须先在
// 数据层限定，再分页（§5.2.3:368）。先取全量再在内存里过滤，第一页就会凑不满。
func TestStoreFindPageRespectsScopeAcrossPages(t *testing.T) {
	pool := integrationPool(t)
	fx := seedScopeFixture(t, pool)
	repo := NewStoreRepository(pool, nil)

	f := StoreFilter{MerchantID: fx.merchantA, StoreIDs: fx.storesA1}
	got := pageAll(t, 1, func(limit, offset int) ([]string, error) {
		rows, err := repo.FindPage(context.Background(), f, limit, offset)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(rows))
		for i, s := range rows {
			out[i] = s.ID
		}
		return out, nil
	})
	if len(got) != 2 {
		t.Fatalf("逐页翻出 2 条, got %d (%v)", len(got), got)
	}
	total, err := repo.Count(context.Background(), f)
	if err != nil {
		t.Fatalf("Count 出错: %v", err)
	}
	if int64(len(got)) != total {
		t.Fatalf("Count(%d) 与翻页行数(%d) 不一致", total, len(got))
	}
}

// TestFindIDsByScopeExpandsEachLevel 覆盖三档展开与展开时的锚点。
func TestFindIDsByScopeExpandsEachLevel(t *testing.T) {
	pool := integrationPool(t)
	fx := seedScopeFixture(t, pool)
	ctx := context.Background()
	repo := NewStoreRepository(pool, nil)

	t.Run("merchant level is every store of the tenant", func(t *testing.T) {
		ids, err := repo.FindIDsByScope(ctx, fx.merchantA, auth.ScopeTypeMerchant, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3 {
			t.Fatalf("expect 3 stores, got %v", ids)
		}
		// 这个结果会被原样当代入传给下游的 ANY()，nil 在那里编码成 NULL 并被读成
		// 「不过滤」。所以空集时也必须回非 nil 切片。
		if ids == nil {
			t.Fatal("ids is nil; it must be an empty slice")
		}
	})

	t.Run("brand level is the stores under that brand", func(t *testing.T) {
		ids, err := repo.FindIDsByScope(ctx, fx.merchantA, auth.ScopeTypeBrand, fx.brandA1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 {
			t.Fatalf("expect 2 stores, got %v", ids)
		}
		for _, id := range ids {
			if id == fx.storeA2 || id == fx.storeB1 {
				t.Fatalf("品牌档串到了别的品牌: %v", ids)
			}
		}
	})

	t.Run("store level is that single point", func(t *testing.T) {
		ids, err := repo.FindIDsByScope(ctx, fx.merchantA, auth.ScopeTypeStore, fx.storeA2)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != fx.storeA2 {
			t.Fatalf("expect [%s], got %v", fx.storeA2, ids)
		}
	})

	t.Run("another merchant's brand expands to nothing", func(t *testing.T) {
		// scope_id 是调用方给来的值引用，锚点就在这里：少了 merchant_id，
		// 商户 A 的账号只要拿到商户 B 的品牌 id，就能把可见点位扩到 B 家去。
		ids, err := repo.FindIDsByScope(ctx, fx.merchantA, auth.ScopeTypeBrand, fx.brandB1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("跨商户的品牌必须展开为空集, got %v", ids)
		}
		if ids == nil {
			t.Fatal("跨商户的空结果也必须是空切片，不能是 nil")
		}
	})

	t.Run("an unrecognized level is an error, not an empty set", func(t *testing.T) {
		// 空集是一个正常答案。认不出的档位混进同一个答案里，调用方会把一次配置
		// 错误读成「这个账号名下没有点位」并照常放行。
		if _, err := repo.FindIDsByScope(ctx, fx.merchantA, "region", "r1"); !errors.Is(err, ErrScopeTypeUnknown) {
			t.Fatalf("err=%v want ErrScopeTypeUnknown", err)
		}
	})

	t.Run("a scope id that is not a uuid yields an empty set", func(t *testing.T) {
		// 两侧都转 text 再比，非 uuid 的输入落成一次不匹配，而不是 uuid 列上的 22P02。
		// 22P02 会让一次「范围引用已失效」表现为 500，而正确的表现是看不见任何点位。
		ids, err := repo.FindIDsByScope(ctx, fx.merchantA, auth.ScopeTypeStore, "not-a-uuid")
		if err != nil {
			t.Fatalf("非 uuid 的 scope_id 不应报错: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("expect empty, got %v", ids)
		}
	})
}
