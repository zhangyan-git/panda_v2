package rpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const serviceToken = "0123456789abcdef0123456789abcdef"

type fakeMerchants struct {
	merchant *model.Merchant
	err      error
}

func (f fakeMerchants) GetByID(context.Context, string) (*model.Merchant, error) {
	return f.merchant, f.err
}

type fakeAccess struct {
	merchantID string
	brandErr   error
	storeErr   error
	namesErr   error
	scopeErr   error
	brandNames map[string]string
	storeNames map[string]string
	store      *model.Store
	// storeIDs is returned as-is by StoreIDsByScope, nil included: the RPC's
	// normalization to an empty slice is one of the things under test here.
	storeIDs []string
}

func (f fakeAccess) FindBrandMerchantID(context.Context, string) (string, error) {
	return f.merchantID, f.brandErr
}

func (f fakeAccess) FindStoreMerchantID(context.Context, string) (string, error) {
	return f.merchantID, f.storeErr
}

func (f fakeAccess) FindStore(context.Context, string) (*model.Store, error) {
	return f.store, f.storeErr
}

func (f fakeAccess) ScopeNames(context.Context, []string, []string) (map[string]string, map[string]string, error) {
	if f.namesErr != nil {
		return nil, nil, f.namesErr
	}
	return f.brandNames, f.storeNames, nil
}

func (f fakeAccess) StoreIDsByScope(context.Context, string, string, string) ([]string, error) {
	return f.storeIDs, f.scopeErr
}

// testServer runs the implementation behind the same interceptor production
// installs through auth.GRPCServerOption, so the gating under test is real.
func testServer(t *testing.T, merchants merchantReader, access ownershipReader) (merchantv1.MerchantServiceClient, *auth.Service) {
	t.Helper()
	jwtService, err := auth.NewService([]byte(strings.Repeat("test", 8)), "test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.UnaryServerInterceptor(jwtService, serviceToken)))
	merchantv1.RegisterMerchantServiceServer(server, NewMerchantService(merchants, access))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return merchantv1.NewMerchantServiceClient(conn), jwtService
}

func TestMerchantServiceRequiresServiceToken(t *testing.T) {
	client, jwtService := testServer(t,
		fakeMerchants{merchant: &model.Merchant{ID: "m1", Name: "商户", Status: "active"}},
		fakeAccess{merchantID: "m1"},
	)
	accessToken, err := jwtService.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(context.Context) error{
		"GetMerchant": func(ctx context.Context) error {
			_, err := client.GetMerchant(ctx, &merchantv1.GetMerchantRequest{Id: "m1"})
			return err
		},
		"GetBrandMerchant": func(ctx context.Context) error {
			_, err := client.GetBrandMerchant(ctx, &merchantv1.GetBrandMerchantRequest{BrandId: "b1"})
			return err
		},
		"GetStoreMerchant": func(ctx context.Context) error {
			_, err := client.GetStoreMerchant(ctx, &merchantv1.GetStoreMerchantRequest{StoreId: "s1"})
			return err
		},
		"GetStore": func(ctx context.Context) error {
			_, err := client.GetStore(ctx, &merchantv1.GetStoreRequest{StoreId: "s1"})
			return err
		},
		"ResolveScopeNames": func(ctx context.Context) error {
			_, err := client.ResolveScopeNames(ctx, &merchantv1.ResolveScopeNamesRequest{BrandIds: []string{"b1"}})
			return err
		},
		"ListStoreIDs": func(ctx context.Context) error {
			_, err := client.ListStoreIDs(ctx, &merchantv1.ListStoreIDsRequest{MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant})
			return err
		},
	}
	credentials := []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"no credentials", context.Background(), codes.Unauthenticated},
		{"user access token", auth.WithAccessToken(context.Background(), accessToken), codes.PermissionDenied},
		{"invalid service token", auth.WithServiceToken(context.Background(), "wrong-token-wrong-token-wrong-token"), codes.Unauthenticated},
		{"service token", auth.WithServiceToken(context.Background(), serviceToken), codes.OK},
	}
	for name, call := range calls {
		for _, credential := range credentials {
			t.Run(name+"/"+credential.name, func(t *testing.T) {
				if got := status.Code(call(credential.ctx)); got != credential.want {
					t.Fatalf("code=%v want %v", got, credential.want)
				}
			})
		}
	}
}

func TestMerchantServiceResults(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)
	t.Run("merchant carries name status and status code", func(t *testing.T) {
		client, _ := testServer(t, fakeMerchants{merchant: &model.Merchant{ID: "m1", Name: "商户", Status: "suspended"}}, fakeAccess{})
		resp, err := client.GetMerchant(serviceCtx, &merchantv1.GetMerchantRequest{Id: "m1"})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetMerchant(); got.GetName() != "商户" || got.GetStatus() != "suspended" || got.GetStatusCode() != merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED {
			t.Fatalf("merchant=%v", got)
		}
	})
	for _, tt := range []struct {
		name string
		call func(merchantv1.MerchantServiceClient) (string, error)
	}{
		{"brand", func(c merchantv1.MerchantServiceClient) (string, error) {
			resp, err := c.GetBrandMerchant(serviceCtx, &merchantv1.GetBrandMerchantRequest{BrandId: "b1"})
			return resp.GetMerchantId(), err
		}},
		{"store", func(c merchantv1.MerchantServiceClient) (string, error) {
			resp, err := c.GetStoreMerchant(serviceCtx, &merchantv1.GetStoreMerchantRequest{StoreId: "s1"})
			return resp.GetMerchantId(), err
		}},
	} {
		t.Run(tt.name+" ownership", func(t *testing.T) {
			client, _ := testServer(t, fakeMerchants{}, fakeAccess{merchantID: "m9"})
			id, err := tt.call(client)
			if err != nil || id != "m9" {
				t.Fatalf("id=%q err=%v", id, err)
			}
		})
	}
}

// TestGetStoreCarriesUsability covers the extra question GetStore answers over
// GetStoreMerchant: a caller about to deploy a device needs the store's status,
// not just its owner, and both forms of each status travel together.
func TestGetStoreCarriesUsability(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)
	client, _ := testServer(t, fakeMerchants{}, fakeAccess{store: &model.Store{
		ID: "s1", MerchantID: "m1", BrandID: "b1", Name: "门店一",
		Status: "disabled", AuditStatus: "approved", Visible: true,
	}})
	resp, err := client.GetStore(serviceCtx, &merchantv1.GetStoreRequest{StoreId: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	store := resp.GetStore()
	if store.GetMerchantId() != "m1" || store.GetName() != "门店一" {
		t.Fatalf("store=%v", store)
	}
	if store.GetStatus() != "disabled" || store.GetStatusCode() != merchantv1.ResourceStatus_RESOURCE_STATUS_DISABLED {
		t.Fatalf("status=%q code=%v", store.GetStatus(), store.GetStatusCode())
	}
	if store.GetAuditStatus() != "approved" || store.GetAuditStatusCode() != merchantv1.AuditStatus_AUDIT_STATUS_APPROVED {
		t.Fatalf("audit status=%q code=%v", store.GetAuditStatus(), store.GetAuditStatusCode())
	}
	// 未采集的坐标必须是「没有」，不能落成 0：0 是一个真实坐标，读的人分不出
	// 「没测过」和「在赤道上」。
	if store.Longitude != nil || store.Latitude != nil {
		t.Fatalf("unset coordinates must stay unset: %v %v", store.Longitude, store.Latitude)
	}
}

// TestResolveScopeNamesAnswersListings covers the scope column of a merchant
// account listing. An id the merchant service does not know is absent from the
// maps — an account whose scope was deleted stays listable — while a storage
// failure is an error, and an opaque one.
func TestResolveScopeNamesAnswersListings(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)

	client, _ := testServer(t, fakeMerchants{}, fakeAccess{
		brandNames: map[string]string{"b1": "品牌一"},
		storeNames: map[string]string{"s1": "门店一"},
	})
	resp, err := client.ResolveScopeNames(serviceCtx, &merchantv1.ResolveScopeNamesRequest{
		BrandIds: []string{"b1", "deleted"}, StoreIds: []string{"s1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetBrandNames()["b1"] != "品牌一" || resp.GetStoreNames()["s1"] != "门店一" {
		t.Fatalf("resp=%v", resp)
	}
	if _, ok := resp.GetBrandNames()["deleted"]; ok {
		t.Fatalf("unknown id must be absent: %v", resp.GetBrandNames())
	}

	failing, _ := testServer(t, fakeMerchants{}, fakeAccess{namesErr: errors.New(`pq: relation "brands" does not exist`)})
	_, err = failing.ResolveScopeNames(serviceCtx, &merchantv1.ResolveScopeNamesRequest{BrandIds: []string{"b1"}})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code=%v want Internal", got)
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("storage error text leaked: %v", err)
	}
}

// recordingAccess notes whether the repository was consulted, so a validation
// test can assert it was not: an unexpanded scope must be refused, never
// answered from a default.
type recordingAccess struct {
	fakeAccess
	called *bool
}

func (r recordingAccess) StoreIDsByScope(context.Context, string, string, string) ([]string, error) {
	*r.called = true
	return r.storeIDs, r.scopeErr
}

// TestListStoreIDsExpandsScope covers the answer user-service turns into the
// boundary every downstream service filters on.
func TestListStoreIDsExpandsScope(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)

	t.Run("merchant scope carries the expanded set", func(t *testing.T) {
		client, _ := testServer(t, fakeMerchants{}, fakeAccess{storeIDs: []string{"s1", "s2"}})
		resp, err := client.ListStoreIDs(serviceCtx, &merchantv1.ListStoreIDsRequest{
			MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetStoreIds(); len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
			t.Fatalf("store_ids=%v", got)
		}
	})

	// An account authorized for nothing is a real answer, and it must not be
	// mistaken for a failure. Note what the wire does to it: proto3 does not
	// serialize an empty repeated field, so the empty slice the RPC builds is
	// decoded back into nil here. The distinction therefore cannot be preserved
	// by this RPC and is not meant to be — the load-bearing nil→empty
	// normalization is auth.WithStoreScope, applied in the middleware, so every
	// consumer reads a non-nil StoreIDs. This case pins "empty scope arrives as
	// an empty answer, not an error".
	t.Run("empty scope arrives as an empty answer", func(t *testing.T) {
		client, _ := testServer(t, fakeMerchants{}, fakeAccess{})
		resp, err := client.ListStoreIDs(serviceCtx, &merchantv1.ListStoreIDsRequest{
			MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeId: "b-empty",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetStoreIds()) != 0 {
			t.Fatalf("store_ids=%v", resp.GetStoreIds())
		}
	})

	t.Run("a storage failure stays opaque", func(t *testing.T) {
		client, _ := testServer(t, fakeMerchants{}, fakeAccess{scopeErr: errors.New(`pq: relation "stores" does not exist`)})
		_, err := client.ListStoreIDs(serviceCtx, &merchantv1.ListStoreIDsRequest{
			MerchantId: "m1", ScopeType: auth.ScopeTypeStore, ScopeId: "s1",
		})
		if got := status.Code(err); got != codes.Internal {
			t.Fatalf("code=%v want Internal", got)
		}
		if strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("storage error text leaked: %v", err)
		}
	})
}

// TestListStoreIDsRefusesUnexpandableScope pins the difference between "this
// account may see nothing" and "this request cannot be answered". Both reach the
// caller as a boundary, so a request that cannot be interpreted must fail loudly
// here instead of quietly becoming an empty set.
func TestListStoreIDsRefusesUnexpandableScope(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)
	for _, tt := range []struct {
		name    string
		request *merchantv1.ListStoreIDsRequest
	}{
		{"no merchant", &merchantv1.ListStoreIDsRequest{ScopeType: auth.ScopeTypeMerchant}},
		{"merchant scope carrying a scope id", &merchantv1.ListStoreIDsRequest{
			MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant, ScopeId: "b1",
		}},
		{"brand scope without a scope id", &merchantv1.ListStoreIDsRequest{MerchantId: "m1", ScopeType: auth.ScopeTypeBrand}},
		{"store scope without a scope id", &merchantv1.ListStoreIDsRequest{MerchantId: "m1", ScopeType: auth.ScopeTypeStore}},
		{"unknown scope type", &merchantv1.ListStoreIDsRequest{MerchantId: "m1", ScopeType: "region"}},
		{"missing scope type", &merchantv1.ListStoreIDsRequest{MerchantId: "m1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			client, _ := testServer(t, fakeMerchants{}, recordingAccess{called: &called})
			_, err := client.ListStoreIDs(serviceCtx, tt.request)
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code=%v want InvalidArgument", got)
			}
			if called {
				t.Fatal("repository was consulted for a scope that cannot be expanded")
			}
		})
	}
}

// TestMerchantServiceNotFound pins the code user-service's client depends on to
// keep its previous pgx.ErrNoRows semantics.
func TestMerchantServiceNotFound(t *testing.T) {
	serviceCtx := auth.WithServiceToken(context.Background(), serviceToken)
	for _, tt := range []struct {
		name string
		call func(merchantv1.MerchantServiceClient) error
	}{
		{"merchant", func(c merchantv1.MerchantServiceClient) error {
			_, err := c.GetMerchant(serviceCtx, &merchantv1.GetMerchantRequest{Id: "missing"})
			return err
		}},
		{"brand", func(c merchantv1.MerchantServiceClient) error {
			_, err := c.GetBrandMerchant(serviceCtx, &merchantv1.GetBrandMerchantRequest{BrandId: "missing"})
			return err
		}},
		{"store", func(c merchantv1.MerchantServiceClient) error {
			_, err := c.GetStoreMerchant(serviceCtx, &merchantv1.GetStoreMerchantRequest{StoreId: "missing"})
			return err
		}},
		{"store detail", func(c merchantv1.MerchantServiceClient) error {
			_, err := c.GetStore(serviceCtx, &merchantv1.GetStoreRequest{StoreId: "missing"})
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := testServer(t,
				fakeMerchants{err: pgx.ErrNoRows},
				fakeAccess{brandErr: pgx.ErrNoRows, storeErr: pgx.ErrNoRows},
			)
			if got := status.Code(tt.call(client)); got != codes.NotFound {
				t.Fatalf("code=%v want NotFound", got)
			}
		})
	}
}

func TestMerchantServiceInternalErrorsStayOpaque(t *testing.T) {
	client, _ := testServer(t, fakeMerchants{err: errors.New("pq: relation \"merchants\" does not exist")}, fakeAccess{})
	_, err := client.GetMerchant(auth.WithServiceToken(context.Background(), serviceToken), &merchantv1.GetMerchantRequest{Id: "m1"})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code=%v want Internal", got)
	}
	if strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("storage error text leaked: %v", err)
	}
}

// TestMerchantServiceLeavesHTTPOnlyRPCsUnimplemented records that merchant CRUD
// and listing deliberately remain HTTP-only surfaces.
func TestMerchantServiceLeavesHTTPOnlyRPCsUnimplemented(t *testing.T) {
	client, _ := testServer(t, fakeMerchants{}, fakeAccess{})
	if _, err := client.ListMerchants(auth.WithServiceToken(context.Background(), serviceToken), &merchantv1.ListMerchantsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("code=%v want Unimplemented", status.Code(err))
	}
}
