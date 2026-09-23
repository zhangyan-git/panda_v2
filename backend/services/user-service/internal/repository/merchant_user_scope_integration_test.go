package repository

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// 这一组用例针对的是「范围目标是一组」之后真正落在 SQL 上的那几句话：写入侧的
// $3::text[]::uuid[] 转换、读回侧的 scope_ids::text[]、以及回收时那条同时摘元素与
// 判空档位的 UPDATE。它们各有各的现成假仓储测不到的地方——
//
//   - 假仓储收的是 []string，看不到 text[]→uuid[] 这一跳；转换写错时只在真库上炸。
//   - 假仓储是 map，看不到「空数组与 NULL 在列上是两回事」；而那个区别在展开处读法相反。
//   - ResetScopeByTarget 的假实现是我们自己照语义写的一遍，与那条 SQL 的 CASE 无关；
//     一条语句里 array_remove 出现两次、判空靠 cardinality，只有真跑才知道对不对。
//
// 与 pagination_integration_test 同一套做法：不建库、不跑迁移，夹具用唯一前缀隔离，
// 结束后按 id 精确删除。没有 USER_DATABASE_URL / TEST_DATABASE_URL 时整组跳过——
// 也就是说 `go test ./...` 全绿并不代表这些跑过。
func scopeRepoFixture(t *testing.T) (*pgxpool.Pool, MerchantUserRepository, string, []string) {
	t.Helper()
	pool := integrationPool(t)
	ids := make([]string, 0, 4)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM merchant_users WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Errorf("清理商户账号夹具: %v", err)
		}
	})
	return pool, NewMerchantUserRepository(pool, nil), "it-scope-" + uuid.NewString()[:8] + "-", ids
}

// newScopeAccount 建一个账号并把它记进清理列表。
func newScopeAccount(t *testing.T, repo MerchantUserRepository, ids *[]string, username, scopeType string, scopeIDs []string) string {
	t.Helper()
	id := uuid.NewString()
	*ids = append(*ids, id)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.Create(context.Background(), &model.MerchantUser{
		ID: id, MerchantID: uuid.NewString(), Username: username, PasswordHash: "x",
		Name: username, Status: "active", ScopeType: scopeType, ScopeIDs: scopeIDs,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("创建账号 %s: %v", username, err)
	}
	return id
}

func mustFind(t *testing.T, repo MerchantUserRepository, id string) *model.MerchantUser {
	t.Helper()
	u, err := repo.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", id, err)
	}
	return u
}

func sortedCopy(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// TestMerchantUserScopeRoundTripsAsASet 守住写入与读回是同一组目标。
//
// 落库与读回走的是两条不同的转换（写 $3::text[]::uuid[]，读 scope_ids::text[]），
// 只有真库能证明它们在两边是同一个集合——尤其是元素顺序：数组列的读回顺序就是
// 存储顺序，而 ScopeNames 要与它同序等长，顺序在这里错位会让名字贴到别人的 id 上。
func TestMerchantUserScopeRoundTripsAsASet(t *testing.T) {
	_, repo, prefix, ids := scopeRepoFixture(t)

	t.Run("brand tier keeps every target", func(t *testing.T) {
		brands := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		id := newScopeAccount(t, repo, &ids, prefix+"brand", "brand", brands)

		got := mustFind(t, repo, id)
		if got.ScopeType != "brand" || !reflect.DeepEqual(got.ScopeIDs, brands) {
			t.Fatalf("读回的范围=%v/%v want brand/%v", got.ScopeType, got.ScopeIDs, brands)
		}
		// 假仓储在造数据时会把 ScopeNames 一起填上；真仓储这一列是占位空数组，
		// 名字由 service 层经 gRPC 解析后填。这一条钉住「仓储不编造名称」。
		if len(got.ScopeNames) != 0 {
			t.Fatalf("仓储不该填名称, got %v", got.ScopeNames)
		}
	})

	t.Run("store tier keeps every target", func(t *testing.T) {
		stores := []string{uuid.NewString(), uuid.NewString()}
		id := newScopeAccount(t, repo, &ids, prefix+"store", "store", stores)

		if got := mustFind(t, repo, id); !reflect.DeepEqual(got.ScopeIDs, stores) {
			t.Fatalf("读回的范围=%v want %v", got.ScopeIDs, stores)
		}
	})
}

// TestMerchantUserEmptyScopeIsAnEmptyArrayNotNull 是本刀的语义要点落在列上的样子。
//
// scope_ids 是 NOT NULL，空集合与 NULL 在展开处读法相反（一个是「一项目标都没有」，
// 一个是「不过滤」）。只要有一处把 nil 写成了 NULL，那个区别就会在某条路径上被抹平，
// 而这里之后的每一层都看不见它——所以直接查列，不看读回的值。
func TestMerchantUserEmptyScopeIsAnEmptyArrayNotNull(t *testing.T) {
	pool, repo, prefix, ids := scopeRepoFixture(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name      string
		scopeType string
		scopeIDs  []string
	}{
		// nil 是调用方最可能递进来的形状（merchant 档没有目标可填）。
		{"merchant tier with nil targets", "merchant", nil},
		{"merchant tier with an empty slice", "merchant", []string{}},
		// 品牌档但一个目标都没有：写入口按说挡掉了，真写进来也必须落成空数组，
		// 而不是 NULL——后者会让这个账号在展开处变成「不过滤」。
		{"brand tier with nil targets", "brand", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := newScopeAccount(t, repo, &ids, prefix+uuid.NewString()[:8], tt.scopeType, tt.scopeIDs)

			var isNull bool
			var card int
			if err := pool.QueryRow(ctx,
				`SELECT scope_ids IS NULL, cardinality(scope_ids) FROM merchant_users WHERE id = $1`,
				id).Scan(&isNull, &card); err != nil {
				t.Fatal(err)
			}
			if isNull {
				t.Fatal("scope_ids 落成了 NULL：列是 NOT NULL，而 NULL 在展开处读作「不过滤」")
			}
			if card != 0 {
				t.Fatalf("目标数应为 0, got %d", card)
			}
			// 读回侧也要收口：nil 一旦流到边界上就是「不过滤」。
			if got := mustFind(t, repo, id); got.ScopeIDs == nil || len(got.ScopeIDs) != 0 {
				t.Fatalf("读回应是空切片而不是 nil, got %#v", got.ScopeIDs)
			}
		})
	}
}

// TestMerchantUserUpdateScopeReplacesTheWholeSet 覆盖改范围这一刀：整组替换，
// 不是合并也不是只改第一个。
func TestMerchantUserUpdateScopeReplacesTheWholeSet(t *testing.T) {
	_, repo, prefix, ids := scopeRepoFixture(t)

	brandA, brandB, brandC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	id := newScopeAccount(t, repo, &ids, prefix+"update", "brand", []string{brandA})

	if err := repo.UpdateScope(context.Background(), id, "brand", []string{brandB, brandC}, true); err != nil {
		t.Fatalf("UpdateScope: %v", err)
	}
	got := mustFind(t, repo, id)
	if got.ScopeType != "brand" || !reflect.DeepEqual(sortedCopy(got.ScopeIDs), sortedCopy([]string{brandB, brandC})) {
		t.Fatalf("范围未整组替换: %v/%v", got.ScopeType, got.ScopeIDs)
	}
	if !got.IsAdmin {
		t.Fatal("is_admin 未跟着更新")
	}

	// 换档到商户级：目标必须被清空而不是留着（留着就是一条谁都不知道的旁路）。
	if err := repo.UpdateScope(context.Background(), id, "merchant", nil, true); err != nil {
		t.Fatalf("UpdateScope: %v", err)
	}
	if got := mustFind(t, repo, id); got.ScopeType != "merchant" || len(got.ScopeIDs) != 0 {
		t.Fatalf("商户档应清空目标: %v/%v", got.ScopeType, got.ScopeIDs)
	}
}

// TestResetScopeByTargetDropsOnlyTheNamedTarget 覆盖品牌/门店被删时那条 UPDATE。
//
// 三种结果同时钉住：只有一个目标的账号回落成商户档（产品口径）；还有别的目标的
// 账号保持原档、只少一个；同 id 但档位不同的账号不受影响（品牌 id 与门店 id 是两个
// 命名空间，只按 id 匹配会把另一档的账号一起回收）。
//
// 还有一条只有真库能验：那条 UPDATE 的 SET 里 array_remove 出现两次、判空靠
// cardinality。PostgreSQL 的 UPDATE ... SET 读的是**更新前**的行，所以两次求值看到的
// 是同一个数组；把它拆成两条语句就会读到已经改过的行，判断与实际结果错开。
func TestResetScopeByTargetDropsOnlyTheNamedTarget(t *testing.T) {
	_, repo, prefix, ids := scopeRepoFixture(t)
	ctx := context.Background()

	target, keep := uuid.NewString(), uuid.NewString()
	onlyID := newScopeAccount(t, repo, &ids, prefix+"only", "brand", []string{target})
	multiID := newScopeAccount(t, repo, &ids, prefix+"multi", "brand", []string{target, keep})
	// 同 id 的门店档：名字一样，档位不同，必须一动不动。
	otherTierID := newScopeAccount(t, repo, &ids, prefix+"tier", "store", []string{target})

	if err := repo.ResetScopeByTarget(ctx, "brand", target); err != nil {
		t.Fatalf("ResetScopeByTarget: %v", err)
	}

	if got := mustFind(t, repo, onlyID); got.ScopeType != "merchant" || len(got.ScopeIDs) != 0 {
		t.Fatalf("最后一个目标被删后应回落成商户档: %v/%v", got.ScopeType, got.ScopeIDs)
	}
	if got := mustFind(t, repo, multiID); got.ScopeType != "brand" || !reflect.DeepEqual(got.ScopeIDs, []string{keep}) {
		t.Fatalf("还有目标时只该摘掉被删的那个: %v/%v want brand/[%s]", got.ScopeType, got.ScopeIDs, keep)
	}
	if got := mustFind(t, repo, otherTierID); got.ScopeType != "store" || !reflect.DeepEqual(got.ScopeIDs, []string{target}) {
		t.Fatalf("另一档的同名 id 被误回收: %v/%v", got.ScopeType, got.ScopeIDs)
	}
}
