package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	service := getenv("SERVICE_NAME", "gateway-service")
	endpoint := getenv("SERVICE_ENDPOINT", "discovery:///"+service)
	registryEndpoint := os.Getenv("REGISTRY_ENDPOINT")
	timeout := 5 * time.Second

	adapter := registry.New(registryEndpoint)
	defer func() { _ = adapter.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := client.DialDiscovery(ctx, endpoint, timeout, adapter)
	if err != nil {
		panic(fmt.Errorf("dial %s: %w", endpoint, err))
	}
	defer conn.Close()

	status, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		panic(fmt.Errorf("health check %s: %w", service, err))
	}
	fmt.Printf("%s status: %s\n", service, status.GetStatus().String())
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
