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
	brandNames map[string]string
	storeNames map[string]string
}

func (f fakeAccess) FindBrandMerchantID(context.Context, string) (string, error) {
	return f.merchantID, f.brandErr
}

func (f fakeAccess) FindStoreMerchantID(context.Context, string) (string, error) {
	return f.merchantID, f.storeErr
}

func (f fakeAccess) ScopeNames(context.Context, []string, []string) (map[string]string, map[string]string, error) {
	if f.namesErr != nil {
		return nil, nil, f.namesErr
	}
	return f.brandNames, f.storeNames, nil
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
	accessToken, err := jwtService.SignAccessGrant(auth.Grant{Subject: "admin", UserID: "admin"})
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
		"ResolveScopeNames": func(ctx context.Context) error {
			_, err := client.ResolveScopeNames(ctx, &merchantv1.ResolveScopeNamesRequest{BrandIds: []string{"b1"}})
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
