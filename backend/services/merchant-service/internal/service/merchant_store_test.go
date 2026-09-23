package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// scopeRepoFake 记下查询拿到的过滤条件。这一层要验证的正是「范围变成了什么」，
// 所以除了记下来没有别的可断言的东西。
type scopeRepoFake struct {
	repository.StoreRepository
	seenFilter repository.StoreFilter
	stores     []*model.Store
	store      *model.Store
	err        error
}

func (f *scopeRepoFake) FindPage(_ context.Context, filter repository.StoreFilter, _, _ int) ([]*model.Store, error) {
	f.seenFilter = filter
	return f.stores, f.err
}

func (f *scopeRepoFake) Count(_ context.Context, filter repository.StoreFilter) (int64, error) {
	f.seenFilter = filter
	return int64(len(f.stores)), f.err
}

func (f *scopeRepoFake) FindByID(context.Context, string) (*model.Store, error) {
	return f.store, f.err
}

// List 与 Count 必须用同一组条件：只过滤了列表没过滤 total，分页控件会显示
// 一个比实际行数大的总数——而这正是「先取全量再在内存里过滤」被抓住的地方。
func TestMerchantStoreListPutsScopeOnBothQueries(t *testing.T) {
	repo := &scopeRepoFake{}
	svc := NewMerchantStoreService(repo)
	scope := auth.StoreScope{MerchantID: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIDs: []string{"b1"}, StoreIDs: []string{"s1", "s2"}}

	// 请求方带上的商户字段必须被范围覆盖，而不是与之并存或取胜。
	if _, _, err := svc.List(context.Background(), scope, repository.StoreFilter{MerchantID: "m2", Name: "x", StoreIDs: []string{"s9"}}, 1, 20); err != nil {
		t.Fatal(err)
	}
	want := repository.StoreFilter{MerchantID: "m1", Name: "x", StoreIDs: []string{"s1", "s2"}}
	if !reflect.DeepEqual(repo.seenFilter, want) {
		t.Fatalf("filter=%+v want %+v", repo.seenFilter, want)
	}
}

// nil 是唯一一个会让范围过滤器失效的取值：它到了 SQL 是 NULL，被读成「不过滤」。
// auth.WithStoreScope 已经在中间件里收过一次口，这里是第二道——直接调 service
// （例如将来的批处理或另一个入口）时它同样得成立。
func TestMerchantStoreListTreatsNilScopeAsNothingAuthorized(t *testing.T) {
	repo := &scopeRepoFake{}
	svc := NewMerchantStoreService(repo)
	if _, _, err := svc.List(context.Background(), auth.StoreScope{MerchantID: "m1"}, repository.StoreFilter{}, 1, 20); err != nil {
		t.Fatal(err)
	}
	if repo.seenFilter.StoreIDs == nil {
		t.Fatal("StoreIDs is nil — the query would return every store")
	}
	if len(repo.seenFilter.StoreIDs) != 0 {
		t.Fatalf("StoreIDs=%v want empty", repo.seenFilter.StoreIDs)
	}
}

func TestMerchantStoreGetRejectsStoresOutsideTheScope(t *testing.T) {
	scope := auth.StoreScope{MerchantID: "m1", ScopeType: auth.ScopeTypeStore, ScopeIDs: []string{"s1"}, StoreIDs: []string{"s1"}}

	repo := &scopeRepoFake{store: &model.Store{ID: "s1", MerchantID: "m1"}}
	if _, err := NewMerchantStoreService(repo).Get(context.Background(), scope, "s1"); err != nil {
		t.Fatalf("范围内的门店应当可读: %v", err)
	}

	// 同商户、也存在，但不在范围里。
	repo = &scopeRepoFake{store: &model.Store{ID: "s2", MerchantID: "m1"}}
	if _, err := NewMerchantStoreService(repo).Get(context.Background(), scope, "s2"); !errors.Is(err, ErrStoreOutOfScope) {
		t.Fatalf("err=%v want ErrStoreOutOfScope", err)
	}

	// 范围是 nil 时同上：没有范围不是没有边界。
	repo = &scopeRepoFake{store: &model.Store{ID: "s1", MerchantID: "m1"}}
	if _, err := NewMerchantStoreService(repo).Get(context.Background(), auth.StoreScope{MerchantID: "m1"}, "s1"); !errors.Is(err, ErrStoreOutOfScope) {
		t.Fatalf("err=%v want ErrStoreOutOfScope", err)
	}
}
