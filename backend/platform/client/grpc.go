// Package client contains small platform client constructors.
package client

import (
	"context"
	"errors"
	"time"

	kratosgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	platformregistry "github.com/panda-dev/panda-v2/backend/platform/registry"
	"google.golang.org/grpc"
)

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
		kratosgrpc.WithDiscovery(discovery),
	)
}

var ErrDiscoveryUnavailable = errors.New("registry adapter does not support discovery")
