// Package client contains small platform client constructors.
package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	kratosgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	platformregistry "github.com/panda-dev/panda-v2/backend/platform/registry"
	"google.golang.org/grpc"
)

// clientMiddleware 是每个出站 gRPC 调用都要挂的中间件。
//
// 服务端中间件负责 Extract，客户端这一半负责 Inject —— 只挂一边，跨服务的
// 调用会断成两个各自的根 span，看起来「有链路」其实接不起来。
//
// 这里不显式传 tracer provider：Dial 在依赖注入链更外层，拿不到 runtime 的
// Observability，而 otel 的全局 provider 是**可后置委派**的 —— observability.Init
// 配了 OTLP 端点时 SetTracerProvider 一调，这里早先建好的 tracer 会自动接上。
// 没配时全局就是空实现，这层中间件什么都不做。
var clientMiddleware = []middleware.Middleware{tracing.Client()}

// Dial connects to another internal service.
//
// serviceName is the discovery identity and address is the static host:port.
// Both are always supplied, and the registry adapter decides which one is used:
// once the adapter really implements Resolver and Watcher the call goes through
// discovery, otherwise it dials address directly. Switching discovery on is
// therefore a configuration change, not a code change at any call site.
//
// This is deliberately not "fall back to the static address on error": a
// registry that is configured but broken must fail loudly, not silently route
// around itself.
func Dial(ctx context.Context, serviceName, address string, timeout time.Duration, adapter platformregistry.Registry) (*grpc.ClientConn, error) {
	if supportsDiscovery(adapter) {
		if strings.TrimSpace(serviceName) == "" {
			return nil, errors.New("service name is required for discovery")
		}
		return DialDiscovery(ctx, "discovery:///"+serviceName, timeout, adapter)
	}
	if strings.TrimSpace(address) == "" {
		return nil, errors.New("static address is required when discovery is unavailable")
	}
	return kratosgrpc.DialInsecure(ctx,
		kratosgrpc.WithEndpoint(address),
		kratosgrpc.WithTimeout(timeout),
		kratosgrpc.WithMiddleware(clientMiddleware...),
	)
}

func supportsDiscovery(adapter platformregistry.Registry) bool {
	if _, ok := adapter.(platformregistry.Resolver); !ok {
		return false
	}
	_, ok := adapter.(platformregistry.Watcher)
	return ok
}

// DialDiscovery creates an insecure Kratos gRPC connection resolved through the
// platform registry. endpoint must use the discovery:///service-name form.
func DialDiscovery(ctx context.Context, endpoint string, timeout time.Duration, adapter platformregistry.Registry) (*grpc.ClientConn, error) {
	resolver, ok := adapter.(platformregistry.Resolver)
	if !ok {
		return nil, ErrDiscoveryUnavailable
	}
	watcher, ok := adapter.(platformregistry.Watcher)
	if !ok {
		return nil, ErrDiscoveryUnavailable
	}
	discovery := platformregistry.KratosDiscovery{Resolver: resolver, Watcher: watcher}
	return kratosgrpc.DialInsecure(ctx,
		kratosgrpc.WithEndpoint(endpoint),
		kratosgrpc.WithTimeout(timeout),
		kratosgrpc.WithMiddleware(clientMiddleware...),
		kratosgrpc.WithDiscovery(discovery),
	)
}

var ErrDiscoveryUnavailable = errors.New("registry adapter does not support discovery")
