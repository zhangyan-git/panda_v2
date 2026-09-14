package client

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const testServiceToken = "test-only-merchant-token-32-bytes-long"

// The services consume the client only through these two ports; the gRPC rewrite
// must keep satisfying both.
var (
	_ service.MerchantAccessPort     = (*MerchantGRPCClient)(nil)
	_ service.MerchantResourceAccess = (*MerchantGRPCClient)(nil)
)

// fakeMerchantService records what the peer saw and replays a canned answer.
type fakeMerchantService struct {
	merchantv1.UnimplementedMerchantServiceServer

	merchant     *merchantv1.Merchant
	brandOwner   string
	storeOwner   string
	brandNames   map[string]string
	storeNames   map[string]string
	merchantErr  error
	resourceErr  error
	delay        time.Duration
	requests     []string
	tokensSeen   []string
	lastDeadline bool
}

func (f *fakeMerchantService) record(ctx context.Context, call string) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(auth.MetadataServiceToken)
	f.requests = append(f.requests, call)
	f.tokensSeen = append(f.tokensSeen, values...)
	_, f.lastDeadline = ctx.Deadline()
}

// wait 模拟慢依赖：只有调用方取消/超时才会提前返回。
func (f *fakeMerchantService) wait(ctx context.Context) error {
	if f.delay <= 0 {
		return nil
	}
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (f *fakeMerchantService) GetMerchant(ctx context.Context, req *merchantv1.GetMerchantRequest) (*merchantv1.GetMerchantResponse, error) {
	f.record(ctx, "GetMerchant:"+req.GetId())
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if f.merchantErr != nil {
		return nil, f.merchantErr
	}
	return &merchantv1.GetMerchantResponse{Merchant: f.merchant}, nil
}

func (f *fakeMerchantService) GetBrandMerchant(ctx context.Context, req *merchantv1.GetBrandMerchantRequest) (*merchantv1.GetBrandMerchantResponse, error) {
	f.record(ctx, "GetBrandMerchant:"+req.GetBrandId())
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if f.resourceErr != nil {
		return nil, f.resourceErr
	}
	return &merchantv1.GetBrandMerchantResponse{MerchantId: f.brandOwner}, nil
}

func (f *fakeMerchantService) GetStoreMerchant(ctx context.Context, req *merchantv1.GetStoreMerchantRequest) (*merchantv1.GetStoreMerchantResponse, error) {
	f.record(ctx, "GetStoreMerchant:"+req.GetStoreId())
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if f.resourceErr != nil {
		return nil, f.resourceErr
	}
	return &merchantv1.GetStoreMerchantResponse{MerchantId: f.storeOwner}, nil
}

func (f *fakeMerchantService) ResolveScopeNames(ctx context.Context, req *merchantv1.ResolveScopeNamesRequest) (*merchantv1.ResolveScopeNamesResponse, error) {
	f.record(ctx, "ResolveScopeNames")
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if f.resourceErr != nil {
		return nil, f.resourceErr
	}
	known := func(names map[string]string, ids []string) map[string]string {
		out := map[string]string{}
		for _, id := range ids {
			if name, ok := names[id]; ok {
				out[id] = name
			}
		}
		return out
	}
	return &merchantv1.ResolveScopeNamesResponse{
		BrandNames: known(f.brandNames, req.GetBrandIds()),
		StoreNames: known(f.storeNames, req.GetStoreIds()),
	}, nil
}

// merchantMethod 描述一个客户端方法与其对应的 RPC，让四个方法共享同一批断言。
type merchantMethod struct {
	name  string
	call  func(*MerchantGRPCClient, context.Context, string) (string, error)
	rpc   string
	value string
	// fail 注入错误响应，使该方法走失败分支
	fail func(*fakeMerchantService, error)
	// blank 让该方法拿到一个「缺失/空值」的响应
	blank func(*fakeMerchantService)
}

var merchantMethods = []merchantMethod{
	{
		name: "status", call: (*MerchantGRPCClient).FindStatus, rpc: "GetMerchant", value: "active",
		fail:  func(f *fakeMerchantService, err error) { f.merchant, f.merchantErr = nil, err },
		blank: func(f *fakeMerchantService) { f.merchant = &merchantv1.Merchant{} },
	},
	{
		name: "name", call: (*MerchantGRPCClient).FindName, rpc: "GetMerchant", value: "咖啡商户",
		fail:  func(f *fakeMerchantService, err error) { f.merchant, f.merchantErr = nil, err },
		blank: func(f *fakeMerchantService) { f.merchant = &merchantv1.Merchant{Status: "active"} },
	},
	{
		name: "brand owner", call: (*MerchantGRPCClient).FindBrandMerchantID, rpc: "GetBrandMerchant", value: "merchant-1",
		fail:  func(f *fakeMerchantService, err error) { f.brandOwner, f.resourceErr = "", err },
		blank: func(f *fakeMerchantService) { f.brandOwner = "" },
	},
	{
		name: "store owner", call: (*MerchantGRPCClient).FindStoreMerchantID, rpc: "GetStoreMerchant", value: "merchant-1",
		fail:  func(f *fakeMerchantService, err error) { f.storeOwner, f.resourceErr = "", err },
		blank: func(f *fakeMerchantService) { f.storeOwner = "" },
	},
}

func newFakeMerchantService() *fakeMerchantService {
	return &fakeMerchantService{
		merchant:   &merchantv1.Merchant{Id: "merchant-1", Name: "咖啡商户", Status: "active", StatusCode: merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE},
		brandOwner: "merchant-1",
		storeOwner: "merchant-1",
	}
}

// newTestClient 用 bufconn 起一个真实 gRPC 服务，不依赖数据库与网络端口。
func newTestClient(t *testing.T, fake *fakeMerchantService, timeout time.Duration) *MerchantGRPCClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	merchantv1.RegisterMerchantServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := NewMerchantGRPCClient(conn, testServiceToken, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestMerchantGRPCClientMethods(t *testing.T) {
	for _, method := range merchantMethods {
		t.Run(method.name, func(t *testing.T) {
			fake := newFakeMerchantService()
			client := newTestClient(t, fake, 5*time.Second)
			got, err := method.call(client, context.Background(), "resource-1")
			if err != nil || got != method.value {
				t.Fatalf("result = %q, %v; want %q", got, err, method.value)
			}
			if len(fake.requests) != 1 || fake.requests[0] != method.rpc+":resource-1" {
				t.Fatalf("requests = %v; want one %s call", fake.requests, method.rpc)
			}
			if len(fake.tokensSeen) != 1 || fake.tokensSeen[0] != testServiceToken {
				t.Fatalf("service credential = %v; want it injected exactly once", fake.tokensSeen)
			}
			if !fake.lastDeadline {
				t.Fatal("call was not bounded by the configured timeout")
			}
		})
	}
}

// 资源不存在必须还原成 pgx.ErrNoRows：调用方据此判定「范围不属于该商户」
func TestMerchantGRPCClientNotFoundMapsToNoRows(t *testing.T) {
	for _, method := range merchantMethods {
		t.Run(method.name, func(t *testing.T) {
			fake := newFakeMerchantService()
			method.fail(fake, status.Error(codes.NotFound, "资源不存在"))
			client := newTestClient(t, fake, 5*time.Second)
			value, err := method.call(client, context.Background(), "resource-1")
			if value != "" || !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("result = %q, %v; want pgx.ErrNoRows", value, err)
			}
		})
	}
}

// 依赖故障绝不能伪装成 pgx.ErrNoRows：否则范围校验会把「查不到」当成
// 「不属于本商户」，或把不可用当成已删除
func TestMerchantGRPCClientGenericFailureIsNotNoRows(t *testing.T) {
	for _, method := range merchantMethods {
		for _, code := range []codes.Code{codes.Internal, codes.Unavailable, codes.PermissionDenied, codes.Unknown, codes.DeadlineExceeded} {
			t.Run(method.name+"/"+code.String(), func(t *testing.T) {
				fake := newFakeMerchantService()
				method.fail(fake, status.Error(code, "dependency failed"))
				client := newTestClient(t, fake, 5*time.Second)
				value, err := method.call(client, context.Background(), "resource-1")
				if value != "" || err == nil {
					t.Fatalf("result = %q, %v; want an error and no value", value, err)
				}
				if errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("transport failure mapped to pgx.ErrNoRows: %v", err)
				}
				if errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("transport failure mapped to ErrInvalidResponse: %v", err)
				}
			})
		}
	}
}

func TestMerchantGRPCClientInvalidResponses(t *testing.T) {
	for _, method := range merchantMethods {
		t.Run(method.name+"/empty value", func(t *testing.T) {
			fake := newFakeMerchantService()
			method.blank(fake)
			client := newTestClient(t, fake, 5*time.Second)
			value, err := method.call(client, context.Background(), "resource-1")
			if value != "" || !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("result = %q, %v; want ErrInvalidResponse", value, err)
			}
		})
		t.Run(method.name+"/missing merchant", func(t *testing.T) {
			fake := newFakeMerchantService()
			fake.merchant, fake.brandOwner, fake.storeOwner = nil, "", ""
			client := newTestClient(t, fake, 5*time.Second)
			value, err := method.call(client, context.Background(), "resource-1")
			if value != "" || !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("result = %q, %v; want ErrInvalidResponse", value, err)
			}
		})
	}
}

func TestMerchantGRPCClientStatusValues(t *testing.T) {
	fake := newFakeMerchantService()
	client := newTestClient(t, fake, 5*time.Second)
	for _, tc := range []struct {
		merchant *merchantv1.Merchant
		want     string
		invalid  bool
	}{
		{merchant: &merchantv1.Merchant{Status: "active"}, want: "active"},
		{merchant: &merchantv1.Merchant{Status: "pending"}, want: "pending"},
		{merchant: &merchantv1.Merchant{Status: "suspended"}, want: "suspended"},
		// 对端只填枚举时退回 status_code，避免把有效商户判成非法响应
		{merchant: &merchantv1.Merchant{StatusCode: merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE}, want: "active"},
		{merchant: &merchantv1.Merchant{StatusCode: merchantv1.MerchantStatus_MERCHANT_STATUS_PENDING}, want: "pending"},
		{merchant: &merchantv1.Merchant{StatusCode: merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED}, want: "suspended"},
		{merchant: &merchantv1.Merchant{Status: "unknown"}, invalid: true},
		{merchant: &merchantv1.Merchant{Status: "ACTIVE"}, invalid: true},
		{merchant: &merchantv1.Merchant{Status: " active"}, invalid: true},
		{merchant: &merchantv1.Merchant{Status: "active "}, invalid: true},
		{merchant: &merchantv1.Merchant{Status: "deleted"}, invalid: true},
		{merchant: &merchantv1.Merchant{}, invalid: true},
		{merchant: &merchantv1.Merchant{Status: "active", StatusCode: merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED}, want: "active"},
	} {
		name := tc.merchant.GetStatus()
		if name == "" {
			name = tc.merchant.GetStatusCode().String()
		}
		t.Run(name, func(t *testing.T) {
			fake.merchant = tc.merchant
			value, err := client.FindStatus(context.Background(), "merchant-1")
			if tc.invalid {
				if value != "" || !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("result = %q, %v; want ErrInvalidResponse", value, err)
				}
				return
			}
			if err != nil || value != tc.want {
				t.Fatalf("result = %q, %v; want %q", value, err, tc.want)
			}
		})
	}
}

func TestMerchantGRPCClientRejectsIDs(t *testing.T) {
	fake := newFakeMerchantService()
	client := newTestClient(t, fake, 5*time.Second)
	for _, method := range merchantMethods {
		t.Run(method.name, func(t *testing.T) {
			for _, id := range []string{"", " ", "\t\n", ".", "..", "../other", "a/b", "a\\b", " id", "id ", "a\x00b", "a\nb", "a\x7fb", "\xff"} {
				if value, err := method.call(client, context.Background(), id); err == nil || value != "" {
					t.Errorf("ID %q returned %q, %v; want empty value and error", id, value, err)
				}
			}
		})
	}
	if len(fake.requests) != 0 {
		t.Fatalf("invalid IDs sent %d requests", len(fake.requests))
	}
}

// 范围名称走批量 RPC：一次调用解析一批品牌/门店，未知 id 只是不在结果里。
func TestMerchantGRPCClientResolvesScopeNames(t *testing.T) {
	fake := newFakeMerchantService()
	fake.brandNames = map[string]string{"b1": "一号品牌"}
	fake.storeNames = map[string]string{"s1": "一号门店"}
	client := newTestClient(t, fake, 5*time.Second)

	brandNames, storeNames, err := client.ScopeNames(context.Background(), []string{"b1", "deleted"}, []string{"s1"})
	if err != nil {
		t.Fatal(err)
	}
	if brandNames["b1"] != "一号品牌" || storeNames["s1"] != "一号门店" {
		t.Fatalf("brand=%v store=%v", brandNames, storeNames)
	}
	if _, ok := brandNames["deleted"]; ok {
		t.Fatalf("未知 id 不应出现在结果里: %v", brandNames)
	}
	if len(fake.requests) != 1 || fake.requests[0] != "ResolveScopeNames" {
		t.Fatalf("requests=%v", fake.requests)
	}

	// 非法 id 必须在发请求前就被拒绝，与其它四个方法一致。
	for _, ids := range [][]string{{".."}, {""}, {"a/b"}} {
		if _, _, err := client.ScopeNames(context.Background(), nil, ids); err == nil {
			t.Errorf("门店 id %q 应被拒绝", ids[0])
		}
	}
	if len(fake.requests) != 1 {
		t.Fatalf("非法 id 不应发出请求: %v", fake.requests)
	}

	fake.resourceErr = status.Error(codes.Internal, "merchant service error")
	if _, _, err := client.ScopeNames(context.Background(), []string{"b1"}, nil); status.Code(err) != codes.Internal {
		t.Fatalf("依赖失败必须原样上抛, got %v", err)
	}
}

func TestMerchantGRPCClientTimeoutAndCancellation(t *testing.T) {
	for _, method := range merchantMethods {
		for _, mode := range []string{"timeout", "cancel"} {
			t.Run(method.name+"/"+mode, func(t *testing.T) {
				fake := newFakeMerchantService()
				fake.delay = 30 * time.Second
				client := newTestClient(t, fake, 50*time.Millisecond)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wantCode := codes.DeadlineExceeded
				if mode == "cancel" {
					wantCode = codes.Canceled
					cancel()
				}
				start := time.Now()
				value, err := method.call(client, ctx, "resource-1")
				if value != "" || status.Code(err) != wantCode {
					t.Fatalf("result = %q, %v; want %v", value, err, wantCode)
				}
				if errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("deadline mapped to pgx.ErrNoRows: %v", err)
				}
				if elapsed := time.Since(start); elapsed > 5*time.Second {
					t.Fatalf("call exceeded timeout bound: %s", elapsed)
				}
			})
		}
	}
}

func TestNewMerchantGRPCClientValidation(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 0, time.Nanosecond - 1} {
		t.Run("timeout/"+timeout.String(), func(t *testing.T) {
			if client, err := NewMerchantGRPCClient(nil, testServiceToken, timeout); err == nil || client != nil {
				t.Fatalf("accepted nil connection and timeout %s", timeout)
			}
		})
	}
	for _, token := range []string{"", " ", "\t"} {
		t.Run("token/"+token, func(t *testing.T) {
			if client, err := NewMerchantGRPCClient(nil, token, time.Second); err == nil || client != nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
	if client, err := NewMerchantGRPCClient(nil, testServiceToken, time.Second); err == nil || client != nil {
		t.Fatal("accepted nil connection")
	}
}
