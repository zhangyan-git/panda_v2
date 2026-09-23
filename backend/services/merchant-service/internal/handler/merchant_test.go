package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeMerchantAccess stands in for user-service's MerchantAccessService.
type fakeMerchantAccess struct {
	resp  *userv1.GetMerchantAccessResponse
	err   error
	calls int
}

func (f *fakeMerchantAccess) GetMerchantAccess(context.Context, *userv1.GetMerchantAccessRequest, ...grpc.CallOption) (*userv1.GetMerchantAccessResponse, error) {
	f.calls++
	return f.resp, f.err
}

// merchantStoreRepo records the filter it was handed, which is how these tests
// see what the boundary turned into by the time it reaches the query.
type merchantStoreRepo struct {
	repository.StoreRepository
	stores []*model.Store
	byID   *model.Store
	err    error

	seenFilter repository.StoreFilter
}

func (f *merchantStoreRepo) FindPage(_ context.Context, filter repository.StoreFilter, _, _ int) ([]*model.Store, error) {
	f.seenFilter = filter
	return f.stores, f.err
}

func (f *merchantStoreRepo) Count(_ context.Context, filter repository.StoreFilter) (int64, error) {
	f.seenFilter = filter
	return int64(len(f.stores)), f.err
}

func (f *merchantStoreRepo) FindByID(context.Context, string) (*model.Store, error) {
	return f.byID, f.err
}

// merchantAPI drives the real route table: the middleware chain, the mux path
// variables and the handlers are the ones production mounts.
type merchantAPI struct {
	server *khttp.Server
	jwt    *auth.Service
	repo   *merchantStoreRepo
	access *fakeMerchantAccess
}

func newMerchantAPI(t *testing.T, repo *merchantStoreRepo, access *fakeMerchantAccess) *merchantAPI {
	t.Helper()
	authorizer, err := NewMerchantAuthorizer(access, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jwtService := testJWT(t)
	server := khttp.NewServer()
	RegisterMerchant(runtime.NewHTTPRouter(server),
		NewMerchantStoreHandler(service.NewMerchantStoreService(repo)), jwtService, authorizer)
	return &merchantAPI{server: server, jwt: jwtService, repo: repo, access: access}
}

// token mints a merchant access token for the given account.
func (a *merchantAPI) token(t *testing.T, grant auth.Grant) string {
	t.Helper()
	if grant.Realm == "" {
		grant.Realm = auth.RealmMerchant
	}
	return testToken(t, a.jwt, grant)
}

// do issues one request with an already-minted token.
func (a *merchantAPI) do(t *testing.T, token, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", token)
	w := httptest.NewRecorder()
	a.server.ServeHTTP(w, r)
	return w
}

// call mints a token for the grant and issues one request with it.
func (a *merchantAPI) call(t *testing.T, grant auth.Grant, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return a.do(t, a.token(t, grant), method, path)
}

func TestMerchantStoreListIsBoundToTheResolvedScope(t *testing.T) {
	repo := &merchantStoreRepo{stores: []*model.Store{{ID: "s1", MerchantID: "m1", Name: "一店"}}}
	api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIds: []string{"b1"}, StoreIds: []string{"s1", "s2"},
	}})

	w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, http.MethodGet, "/v1/merchant/stores")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if repo.seenFilter.MerchantID != "m1" {
		t.Fatalf("merchant filter=%q want m1", repo.seenFilter.MerchantID)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1", "s2"}) {
		t.Fatalf("scope filter=%v want [s1 s2]", repo.seenFilter.StoreIDs)
	}
}

// The query string must not be able to name a merchant: the boundary comes from
// the identity service, and a request field that could widen it would make the
// whole scope mechanism optional.
func TestMerchantStoreListIgnoresMerchantIDFromTheQuery(t *testing.T) {
	repo := &merchantStoreRepo{}
	api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIds: []string{"s1"},
	}})

	w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"},
		http.MethodGet, "/v1/merchant/stores?merchantId=m2&name=x")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if repo.seenFilter.MerchantID != "m1" {
		t.Fatalf("merchant filter=%q want m1 — the query string widened the scope", repo.seenFilter.MerchantID)
	}
	if repo.seenFilter.Name != "x" {
		t.Fatalf("name filter=%q want x — a display filter should still apply", repo.seenFilter.Name)
	}
}

// An account whose scope covers nothing is a normal account, not an error, and
// the filter it produces must be the empty set rather than no filter at all.
func TestMerchantStoreListWithEmptyScopeFiltersEverything(t *testing.T) {
	repo := &merchantStoreRepo{}
	api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIds: []string{"b-empty"},
	}})

	if w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, http.MethodGet, "/v1/merchant/stores"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if repo.seenFilter.StoreIDs == nil {
		t.Fatal("StoreIDs is nil — downstream that is read as \"no filter\", not \"no stores\"")
	}
	if len(repo.seenFilter.StoreIDs) != 0 {
		t.Fatalf("StoreIDs=%v want empty", repo.seenFilter.StoreIDs)
	}
}

func TestMerchantStoreDetailHidesOutOfScopeStores(t *testing.T) {
	for _, tt := range []struct {
		name    string
		scope   []string
		store   *model.Store
		repoErr error
		want    int
	}{
		{
			name:  "inside the scope",
			scope: []string{"s1", "s2"},
			store: &model.Store{ID: "s1", MerchantID: "m1"},
			want:  http.StatusOK,
		},
		{
			// 403 would admit the id exists and merely belongs to someone else.
			name:  "another merchant's store",
			scope: []string{"s1", "s2"},
			store: &model.Store{ID: "s9", MerchantID: "m2"},
			want:  http.StatusNotFound,
		},
		{
			name:    "no such store",
			scope:   []string{"s1"},
			repoErr: pgx.ErrNoRows,
			want:    http.StatusNotFound,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &merchantStoreRepo{byID: tt.store, err: tt.repoErr}
			api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
				MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIds: []string{"b1"}, StoreIds: tt.scope,
			}})
			w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, http.MethodGet, "/v1/merchant/stores/x")
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
		})
	}
}

// The merchant domain is read-only this round. A write verb must not fall
// through to a handler that ignores it.
func TestMerchantStoreRoutesAreReadOnly(t *testing.T) {
	api := newMerchantAPI(t, &merchantStoreRepo{}, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIds: []string{"s1"},
	}})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, method, "/v1/merchant/stores"); w.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d want 404", method, w.Code)
		}
	}
}

func TestMerchantRoutesRejectNonMerchantCallers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		grant auth.Grant
		want  int
	}{
		{
			// A platform administrator's token is not a merchant token, and its
			// subject has no merchant account row to resolve a scope from.
			name:  "platform token",
			grant: auth.Grant{Subject: "admin", UserID: "admin", Realm: auth.RealmPlatform},
			want:  http.StatusForbidden,
		},
		{
			// No tenant, so there is nothing to resolve and nothing to default to.
			name:  "merchant token without a tenant",
			grant: auth.Grant{Subject: "a1", UserID: "a1", Realm: auth.RealmMerchant},
			want:  http.StatusForbidden,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			access := &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant}}
			api := newMerchantAPI(t, &merchantStoreRepo{}, access)
			w := api.call(t, tt.grant, http.MethodGet, "/v1/merchant/stores")
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if access.calls != 0 {
				t.Fatal("the identity service was asked about a request that must be refused on the token alone")
			}
		})
	}
}

// A boundary that could not be fetched is never replaced by one that was
// guessed — not the token's, not an empty set, not everything.
func TestMerchantRoutesFailClosedWhenTheScopeCannotBeResolved(t *testing.T) {
	for _, tt := range []struct {
		name string
		resp *userv1.GetMerchantAccessResponse
		err  error
		want int
	}{
		{"disabled account", nil, status.Error(codes.PermissionDenied, "no"), http.StatusForbidden},
		{"expired token", nil, status.Error(codes.Unauthenticated, "no"), http.StatusUnauthorized},
		{"identity service down", nil, status.Error(codes.Unavailable, "down"), http.StatusServiceUnavailable},
		{"answer is about another merchant", &userv1.GetMerchantAccessResponse{MerchantId: "m2", ScopeType: auth.ScopeTypeMerchant}, nil, http.StatusServiceUnavailable},
		{"answer names no merchant", &userv1.GetMerchantAccessResponse{ScopeType: auth.ScopeTypeMerchant}, nil, http.StatusServiceUnavailable},
		{"answer carries an unknown scope", &userv1.GetMerchantAccessResponse{MerchantId: "m1", ScopeType: "region"}, nil, http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &merchantStoreRepo{stores: []*model.Store{{ID: "s1"}}}
			api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: tt.resp, err: tt.err})
			w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, http.MethodGet, "/v1/merchant/stores")
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if repo.seenFilter.StoreIDs != nil || repo.seenFilter.MerchantID != "" {
				t.Fatalf("the query ran with filter %+v for an undecided boundary", repo.seenFilter)
			}
		})
	}
}

// The scope is re-fetched on every request. A narrowed scope must apply to the
// very next call with the same, still-valid token — that is the whole reason it
// is not signed into the 24-hour access token.
func TestMerchantScopeIsResolvedFreshEveryRequest(t *testing.T) {
	repo := &merchantStoreRepo{}
	access := &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant, StoreIds: []string{"s1", "s2"},
	}}
	api := newMerchantAPI(t, repo, access)
	// 同一枚令牌打两次。令牌没过期、内容也没变，变的只有数据范围。
	token := api.token(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"})

	if w := api.do(t, token, http.MethodGet, "/v1/merchant/stores"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1", "s2"}) {
		t.Fatalf("first request scope=%v", repo.seenFilter.StoreIDs)
	}

	access.resp = &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeStore, ScopeIds: []string{"s1"}, StoreIds: []string{"s1"},
	}
	if w := api.do(t, token, http.MethodGet, "/v1/merchant/stores"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(repo.seenFilter.StoreIDs, []string{"s1"}) {
		t.Fatalf("scope=%v — the scope was reused instead of re-resolved", repo.seenFilter.StoreIDs)
	}
	if access.calls != 2 {
		t.Fatalf("resolver calls=%d want 2", access.calls)
	}
}

// 门店删除后账号范围内的那个 id 会失效，但账号本身仍然可用：范围里的其他点位照常
// 能看。这条同时盯住「范围为空」不等于「范围失效」这个区分。
func TestMerchantStoreDetailRequiresMembershipNotJustOwnership(t *testing.T) {
	repo := &merchantStoreRepo{byID: &model.Store{ID: "s1", MerchantID: "m1"}}
	api := newMerchantAPI(t, repo, &fakeMerchantAccess{resp: &userv1.GetMerchantAccessResponse{
		// 账号属于 m1，这家店也是 m1 的，但不在它的范围里。
		MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIds: []string{"b1"}, StoreIds: []string{"s2"},
	}})
	w := api.call(t, auth.Grant{Subject: "a1", UserID: "a1", Tenant: "m1"}, http.MethodGet, "/v1/merchant/stores/s1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 — 同商户但范围外也要看不见", w.Code)
	}
}
