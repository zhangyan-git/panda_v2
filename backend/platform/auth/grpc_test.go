package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// stubTransporter stands in for the server transport Kratos installs on the
// context before it runs the middleware chain. The interceptor only reads
// Operation from it, so that is all this carries; a real server fills one in
// per call and the unit test below builds the same shape without a network.
type stubTransporter struct{ operation string }

func (stubTransporter) Kind() transport.Kind            { return transport.KindGRPC }
func (stubTransporter) Endpoint() string                { return "grpc://127.0.0.1:0" }
func (s stubTransporter) Operation() string             { return s.operation }
func (stubTransporter) RequestHeader() transport.Header { return nil }
func (stubTransporter) ReplyHeader() transport.Header   { return nil }

const testServiceToken = "0123456789abcdef0123456789abcdef"

// callInterceptor runs the interceptor over a context built from md and reports
// both the error and the context the handler saw.
func callInterceptor(t *testing.T, interceptor grpc.UnaryServerInterceptor, md metadata.MD, fullMethod string) (context.Context, error) {
	t.Helper()
	ctx := context.Background()
	if md != nil {
		ctx = metadata.NewIncomingContext(ctx, md)
	}
	var seen context.Context
	_, err := interceptor(ctx, "request", &grpc.UnaryServerInfo{FullMethod: fullMethod}, func(ctx context.Context, req any) (any, error) {
		seen = ctx
		if req != "request" {
			t.Fatalf("handler received %v, want the original request", req)
		}
		return "response", nil
	})
	return seen, err
}

func bearerMetadata(t *testing.T, claims Claims) metadata.MD {
	t.Helper()
	return metadata.Pairs(MetadataAuthorization, "Bearer "+signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims))
}

func TestUnaryServerInterceptorAcceptsServiceToken(t *testing.T) {
	interceptor := UnaryServerInterceptor(newTestService(t), testServiceToken)
	ctx, err := callInterceptor(t, interceptor, metadata.Pairs(MetadataServiceToken, testServiceToken), "/panda.user.v1.UserService/HasUsers")
	if err != nil {
		t.Fatalf("service token rejected: %v", err)
	}
	if !ServiceFromContext(ctx) {
		t.Fatal("ServiceFromContext = false for a service-token call")
	}
	if err := RequireService(ctx); err != nil {
		t.Fatalf("RequireService = %v, want nil", err)
	}
	// A service caller is not a user, so an RPC answering about "the caller" must
	// refuse it rather than fall through to an empty identity.
	if _, err := RequireUser(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("RequireUser = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptorAcceptsUserToken(t *testing.T) {
	claims := validTestClaims(AccessTokenType)
	claims.Roles = []string{"super_admin"}
	claims.Permissions = []string{"merchant.view"}
	claims.IsSuper = true
	interceptor := UnaryServerInterceptor(newTestService(t), testServiceToken)
	ctx, err := callInterceptor(t, interceptor, bearerMetadata(t, claims), "/panda.user.v1.AdminAccessService/GetAdminAccess")
	if err != nil {
		t.Fatalf("access token rejected: %v", err)
	}
	if ServiceFromContext(ctx) {
		t.Fatal("ServiceFromContext = true for a user-token call")
	}
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		t.Fatal("IdentityFromContext = false for a user-token call")
	}
	if identity.UserID != "user-1" || identity.Subject != "subject-1" {
		t.Fatalf("identity = %+v, want the token's subject and user", identity)
	}
	// No authorization claim survives this layer — not permissions, not roles,
	// not the super flag. They were signed at login and never change, so an RPC
	// authorizing on them would keep honoring revoked grants and demoted
	// administrators until the token expired, which is exactly what asking the
	// identity service for live grants exists to avoid.
	if len(identity.Permissions) != 0 {
		t.Fatalf("identity carried the token's permission snapshot: %v", identity.Permissions)
	}
	if len(identity.Roles) != 0 {
		t.Fatalf("identity carried the token's role snapshot: %v", identity.Roles)
	}
	if identity.IsSuper {
		t.Fatal("identity carried the token's super flag")
	}
	if _, err := RequireUser(ctx); err != nil {
		t.Fatalf("RequireUser = %v, want nil", err)
	}
}

func TestUnaryServerInterceptorRejectsAmbiguousCredentials(t *testing.T) {
	md := bearerMetadata(t, validTestClaims(AccessTokenType))
	md.Set(MetadataServiceToken, testServiceToken)
	interceptor := UnaryServerInterceptor(newTestService(t), testServiceToken)
	if _, err := callInterceptor(t, interceptor, md, "/panda.user.v1.UserService/HasUsers"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ambiguous credentials got %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptorRejectsBadCredentials(t *testing.T) {
	shortSecretClaims := validTestClaims(AccessTokenType)
	refreshClaims := validTestClaims(RefreshTokenType)
	tests := []struct {
		name  string
		md    metadata.MD
		token string
	}{
		{name: "no metadata", md: nil, token: testServiceToken},
		{name: "empty metadata", md: metadata.MD{}, token: testServiceToken},
		{name: "no service token configured", md: metadata.Pairs(MetadataServiceToken, testServiceToken), token: ""},
		{name: "wrong service token", md: metadata.Pairs(MetadataServiceToken, "not-the-service-token"), token: testServiceToken},
		{name: "malformed authorization", md: metadata.Pairs(MetadataAuthorization, "token-without-scheme"), token: testServiceToken},
		{name: "refresh token used as access token", md: bearerMetadata(t, refreshClaims), token: testServiceToken},
		{
			name: "token signed with another secret",
			md: metadata.Pairs(MetadataAuthorization,
				"Bearer "+signTestClaims(t, []byte("another-secret-another-secret-32"), jwt.SigningMethodHS256, shortSecretClaims)),
			token: testServiceToken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := UnaryServerInterceptor(newTestService(t), tt.token)
			if _, err := callInterceptor(t, interceptor, tt.md, "/panda.user.v1.UserService/HasUsers"); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("got %v, want Unauthenticated", err)
			}
		})
	}
}

func TestUnaryServerInterceptorRejectsUserTokenWhenVerifierMissing(t *testing.T) {
	interceptor := UnaryServerInterceptor(nil, testServiceToken)
	_, err := callInterceptor(t, interceptor, bearerMetadata(t, validTestClaims(AccessTokenType)), "/panda.user.v1.AdminAccessService/GetAdminAccess")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("nil verifier got %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptorSkipsHealthAndReflection(t *testing.T) {
	interceptor := UnaryServerInterceptor(newTestService(t), testServiceToken)
	for _, method := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		t.Run(method, func(t *testing.T) {
			ctx, err := callInterceptor(t, interceptor, metadata.MD{}, method)
			if err != nil {
				t.Fatalf("%s required credentials: %v", method, err)
			}
			// The bypass must not be a backdoor into the authenticated identity.
			if ServiceFromContext(ctx) {
				t.Fatal("bypassed probe was given a service identity")
			}
			if _, ok := IdentityFromContext(ctx); ok {
				t.Fatal("bypassed probe was given a user identity")
			}
		})
	}
}

func TestUnaryMiddlewareAdaptsInterceptor(t *testing.T) {
	var adapted middleware.Middleware = UnaryMiddleware(UnaryServerInterceptor(newTestService(t), testServiceToken))
	handler := adapted(func(ctx context.Context, req any) (any, error) {
		if !ServiceFromContext(ctx) {
			t.Fatal("middleware did not pass the authenticated context to the next handler")
		}
		return "handled:" + req.(string), nil
	})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataServiceToken, testServiceToken))
	got, err := handler(ctx, "req")
	if err != nil {
		t.Fatalf("handler = %v", err)
	}
	if got != "handled:req" {
		t.Fatalf("handler = %v, want the next handler's response", got)
	}
}

func TestUnaryMiddlewarePropagatesRejection(t *testing.T) {
	adapted := UnaryMiddleware(UnaryServerInterceptor(newTestService(t), testServiceToken))
	handler := adapted(func(context.Context, any) (any, error) {
		t.Fatal("next handler ran for an unauthenticated call")
		return nil, nil
	})
	got, err := handler(context.Background(), "req")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("handler = (%v, %v), want Unauthenticated", got, err)
	}
}

// TestUnaryMiddlewareReadsMethodFromTransport is the regression test for the
// adapter path, which is the only path the services actually run.
//
// UnaryMiddleware hands the interceptor a bare &grpc.UnaryServerInfo{} — Kratos
// consumed the real one before invoking the middleware chain — so a method read
// from info alone is the empty string, unauthenticatedMethod never matches, and
// the documented probe exemption silently never fires. This test builds the
// context exactly as Kratos' gRPC interceptor does and asserts both halves: a
// framework method passes without credentials, a business method still does not,
// and passing does not hand the caller an identity.
func TestUnaryMiddlewareReadsMethodFromTransport(t *testing.T) {
	adapted := UnaryMiddleware(UnaryServerInterceptor(newTestService(t), testServiceToken))
	call := func(method string, md metadata.MD) (context.Context, error) {
		var seen context.Context
		handler := adapted(func(ctx context.Context, req any) (any, error) {
			seen = ctx
			return "handled", nil
		})
		ctx := context.Background()
		if md != nil {
			ctx = metadata.NewIncomingContext(ctx, md)
		}
		ctx = transport.NewServerContext(ctx, stubTransporter{operation: method})
		_, err := handler(ctx, "req")
		return seen, err
	}

	for _, method := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		t.Run(method, func(t *testing.T) {
			ctx, err := call(method, metadata.MD{})
			if err != nil {
				t.Fatalf("%s required credentials through the adapter: %v", method, err)
			}
			// The bypass must not be a backdoor into the authenticated identity.
			if ServiceFromContext(ctx) {
				t.Fatal("bypassed probe was given a service identity")
			}
			if _, ok := IdentityFromContext(ctx); ok {
				t.Fatal("bypassed probe was given a user identity")
			}
		})
	}

	businessMethod := "/panda.user.v1.UserService/HasUsers"
	t.Run(businessMethod, func(t *testing.T) {
		// The transport must not be read as an excuse to skip verification.
		if _, err := call(businessMethod, metadata.MD{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("got %v, want Unauthenticated", err)
		}
		ctx, err := call(businessMethod, metadata.Pairs(MetadataServiceToken, testServiceToken))
		if err != nil {
			t.Fatalf("service token rejected through the adapter: %v", err)
		}
		if !ServiceFromContext(ctx) {
			t.Fatal("adapter did not pass the authenticated context to the next handler")
		}
	})
}

func TestGRPCServerOptionBuilds(t *testing.T) {
	if option := GRPCServerOption(newTestService(t), testServiceToken); option == nil {
		t.Fatal("GRPCServerOption returned nil")
	}
}

// TestGRPCServerOptionComposesWithDefaultMiddleware is the regression test for
// the option's reason to exist. Kratos' matcher.Use assigns the default
// middleware list rather than appending, so an option built on kgrpc.Middleware
// silently uninstalls whatever the runtime installed first — observable only as
// missing traces and metrics, never as an error.
func TestGRPCServerOptionComposesWithDefaultMiddleware(t *testing.T) {
	var instrumented atomic.Int64
	runtimeMiddleware := func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			instrumented.Add(1)
			return next(ctx, req)
		}
	}

	srv := kgrpc.NewServer(
		kgrpc.Address("127.0.0.1:0"),
		kgrpc.Middleware(runtimeMiddleware),
		GRPCServerOption(newTestService(t), testServiceToken),
	)
	srv.Server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Echo",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			// Mirrors generated code: the handler must route through the
			// interceptor it is given, otherwise the server's middleware chain —
			// which is the thing under test — never runs.
			Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return in, nil
				}
				info := &grpc.UnaryServerInfo{FullMethod: "/test.Echo/Echo"}
				return interceptor(ctx, in, info, func(context.Context, any) (any, error) { return in, nil })
			},
		}},
	}, nil)
	// Endpoint() binds the listener, so asking for it before Start keeps the
	// address known without racing the serving goroutine for it. Start itself
	// blocks until the server stops, hence the goroutine.
	endpoint, err := srv.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = srv.Start(context.Background()) }()
	defer func() { _ = srv.Stop(context.Background()) }()
	conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Without credentials the call must still be refused, proving the option's own
	// middleware survived alongside the runtime's.
	call := func(ctx context.Context) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return conn.Invoke(ctx, "/test.Echo/Echo", &emptypb.Empty{}, &emptypb.Empty{})
	}
	if err := call(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated call: err=%v, want Unauthenticated", err)
	}
	if instrumented.Load() != 1 {
		t.Fatalf("runtime middleware ran %d times, want 1 (the auth option replaced the default list)", instrumented.Load())
	}

	if err := call(metadata.AppendToOutgoingContext(context.Background(), MetadataServiceToken, testServiceToken)); err != nil {
		t.Fatalf("authenticated call: %v", err)
	}
	if instrumented.Load() != 2 {
		t.Fatalf("runtime middleware ran %d times, want 2", instrumented.Load())
	}
}

// TestGRPCServerOptionAnswersHealthProbe is the same regression end to end,
// against a server assembled the way a service assembles one. It is the shape
// the bug was reported in: a readiness probe answered UNAUTHENTICATED by the
// gRPC listener while the HTTP /readyz next to it reports everything is fine, so
// the replica never becomes ready and nothing in the logs says why.
func TestGRPCServerOptionAnswersHealthProbe(t *testing.T) {
	srv := kgrpc.NewServer(
		kgrpc.Address("127.0.0.1:0"),
		GRPCServerOption(newTestService(t), testServiceToken),
	)
	endpoint, err := srv.Endpoint()
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	go func() { _ = srv.Start(context.Background()) }()
	defer func() { _ = srv.Stop(context.Background()) }()
	conn, err := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The default (all services) status is SERVING on a freshly started Kratos
	// server, so a probe that gets through answers OK rather than reporting on a
	// server it never reached.
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health probe was refused: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health status = %v, want SERVING", resp.GetStatus())
	}
}
