package auth

import (
	"context"
	"crypto/subtle"
	"strings"

	"github.com/go-kratos/kratos/v2/middleware"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Metadata keys for internal gRPC calls. The authorization key intentionally
// mirrors the HTTP header name so a gateway-forwarded token needs no renaming.
const (
	MetadataAuthorization = "authorization"
	MetadataServiceToken  = "x-service-token"
)

// ServiceIdentity marks a context authenticated by the shared service token
// rather than an end-user access token. It carries no user and grants no
// permissions of its own.
type ServiceIdentity struct{}

type serviceIdentityContextKey struct{}

// ServiceFromContext reports whether ctx was authenticated as a service caller.
func ServiceFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(serviceIdentityContextKey{}).(ServiceIdentity)
	return ok
}

// RequireService fails unless ctx carries a service identity. Internal RPCs
// that act on infrastructure rather than on a user call this so that a valid
// end-user token cannot reach them.
func RequireService(ctx context.Context) error {
	if !ServiceFromContext(ctx) {
		return status.Error(codes.PermissionDenied, "service credentials required")
	}
	return nil
}

// RequireUser returns the end-user identity of ctx, failing when the call was
// authenticated as a service. RPCs answering about "the caller" call this.
func RequireUser(ctx context.Context) (Identity, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok || identity.UserID == "" {
		return Identity{}, status.Error(codes.Unauthenticated, "user access token required")
	}
	return identity, nil
}

// UnaryServerInterceptor authenticates internal gRPC calls.
//
// A caller must present exactly one credential: the shared service token for
// service-to-service RPCs, or a valid access token for RPCs performed on behalf
// of an end user. Presenting both is rejected rather than silently preferring
// one, so a misconfigured client cannot have its intent guessed. Presenting
// neither, or a token that fails verification, is unauthenticated.
//
// The interceptor only establishes identity. Whether a given RPC accepts a
// service caller or a user caller is decided by the handler.
func UnaryServerInterceptor(service *Service, serviceToken string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info != nil && unauthenticatedMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing credentials")
		}
		presented := firstValue(md, MetadataServiceToken)
		authorization := firstValue(md, MetadataAuthorization)
		if presented != "" && authorization != "" {
			return nil, status.Error(codes.Unauthenticated, "ambiguous credentials")
		}
		switch {
		case presented != "":
			if serviceToken == "" || !secretEqual(presented, serviceToken) {
				return nil, status.Error(codes.Unauthenticated, "invalid service token")
			}
			return handler(context.WithValue(ctx, serviceIdentityContextKey{}, ServiceIdentity{}), req)
		case authorization != "":
			if service == nil {
				return nil, status.Error(codes.Unauthenticated, "token verification unavailable")
			}
			token, ok := BearerToken(authorization)
			if !ok {
				return nil, status.Error(codes.Unauthenticated, "malformed authorization metadata")
			}
			claims, err := service.Parse(token)
			if err != nil || claims.TokenType != AccessTokenType {
				return nil, status.Error(codes.Unauthenticated, "invalid access token")
			}
			identity := Identity{
				// Who the caller is — and only that. The token's authorization
				// claims (roles, permissions, the super flag) are deliberately NOT
				// copied: they were signed at login and never change, so an RPC
				// authorizing on them would keep honoring a revoked grant or a
				// demoted administrator until the token expired. Callers that need
				// grants ask the identity service for them.
				//
				// Realm IS copied: it is part of "who the caller is", and an RPC
				// that admits only platform administrators (GetAdminAccess) has no
				// other way to tell a C-end token from an administrator's.
				Subject: claims.Subject, UserID: claims.UserID, Tenant: claims.Tenant,
				Realm: claims.Realm,
			}
			if claims.Scope != nil {
				identity.Scope = claims.Scope.clone()
			}
			return handler(WithIdentity(ctx, identity), req)
		default:
			return nil, status.Error(codes.Unauthenticated, "missing credentials")
		}
	}
}

// UnaryMiddleware adapts UnaryServerInterceptor into a Kratos middleware.
//
// The raw grpc.ChainUnaryInterceptor option cannot be combined with Kratos: the
// transport builds its own chained interceptor from middleware and rejects a
// second one. Adapting keeps a single implementation of the authentication
// rules for both worlds.
func UnaryMiddleware(interceptor grpc.UnaryServerInterceptor) middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			return interceptor(ctx, req, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
				return next(ctx, req)
			})
		}
	}
}

// GRPCServerOption installs internal-call authentication on a Kratos gRPC
// server. Services pass the result through runtime.Options.GRPCServerOptions.
//
// It registers through Use rather than through kgrpc.Middleware on purpose.
// Kratos' matcher.Use assigns the default middleware list instead of appending to
// it, so two ServerOptions that both call Middleware leave only the last one
// installed — and GRPCServerOptions are applied after the runtime's own, which
// would silently drop the instrumentation. Use adds to the matcher's prefix
// table, which is combined with those defaults rather than replacing them, and
// keeps the runtime middleware outermost so a rejected call is still traced.
func GRPCServerOption(service *Service, serviceToken string) kgrpc.ServerOption {
	middleware := UnaryMiddleware(UnaryServerInterceptor(service, serviceToken))
	return func(s *kgrpc.Server) {
		s.Use("/*", middleware)
	}
}

// WithServiceToken returns a context carrying the shared service token for an
// outgoing internal call.
func WithServiceToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, MetadataServiceToken, token)
}

// WithAccessToken returns a context carrying an end-user access token for an
// outgoing internal call made on that user's behalf.
func WithAccessToken(ctx context.Context, accessToken string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, MetadataAuthorization, "Bearer "+accessToken)
}

// AccessTokenFromMetadata returns the Bearer access token of an incoming call.
// It exists so a service can forward the caller's own token onward without ever
// inventing or trusting a caller-supplied identity field.
func AccessTokenFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	return BearerToken(firstValue(md, MetadataAuthorization))
}

// unauthenticatedMethod lists the framework methods that must stay reachable
// without credentials. Kratos registers the gRPC health service and server
// reflection on every server it builds; requiring a token for them would break
// load balancer health checks and service discovery in exactly the situation
// those probes exist to report on.
func unauthenticatedMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

func firstValue(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// secretEqual compares secrets without leaking length or content through timing.
func secretEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
