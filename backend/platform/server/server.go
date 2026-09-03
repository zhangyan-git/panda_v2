package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-kratos/kratos/v2"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/health"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
)

// Run starts a service's Kratos HTTP and gRPC servers and blocks until shutdown.
// It remains the compatibility entry point for existing services.
func Run(service string) error {
	cfg, err := config.Load(service)
	if err != nil {
		return err
	}
	return RunWithOptions(cfg, runtime.Options{})
}

// RunWithOptions starts a service with injected runtime dependencies.
func RunWithOptions(cfg config.Config, options runtime.Options) error {
	ctx := context.Background()
	cleanup := make([]func(), 0, 6)
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	if options.Database == nil {
		var err error
		options.Database, err = database.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		options.OwnDatabase = true
		cleanup = append(cleanup, func() { options.Database.Close() })
	}
	if options.Cache == nil {
		var err error
		options.Cache, err = cache.New(ctx, cache.Options{Addr: cfg.RedisAddress, Password: cfg.RedisPassword, DB: cfg.RedisDB})
		if err != nil {
			return err
		}
		options.OwnCache = true
		cleanup = append(cleanup, func() { _ = options.Cache.Close() })
	}
	if options.Registry == nil {
		options.Registry = registry.New(cfg.RegistryEndpoint)
		options.OwnRegistry = true
		cleanup = append(cleanup, func() { _ = options.Registry.Close() })
	}
	if options.Publisher == nil || options.Consumer == nil {
		publisher, consumer, closer, err := messaging.NewRabbitMQ(messaging.RabbitConfigFromEnv())
		if err != nil {
			return err
		}
		if options.Publisher == nil {
			options.Publisher = publisher
		}
		if options.Consumer == nil {
			options.Consumer = consumer
		}
		if options.Messaging == nil {
			options.Messaging = closer
			options.OwnMessaging = true
		} else {
			options.MessagingCleanup = closer
			cleanup = append(cleanup, func() { _ = closer.Close() })
		}
	}
	if options.Observability.Tracer == nil || options.Observability.Meter == nil {
		injected := options.Observability
		providers, err := observability.Init(ctx, observability.ConfigFromEnv())
		if err != nil {
			return err
		}
		if injected.Tracer != nil {
			providers.Tracer = injected.Tracer
		}
		if injected.Meter != nil {
			providers.Meter = injected.Meter
		}
		options.Observability = providers
		options.OwnObservability = true
		cleanup = append(cleanup, func() { _ = providers.Shutdown(context.Background()) })
	}
	h := health.New()
	httpServer := khttp.NewServer(khttp.Address(cfg.HTTPAddress), khttp.Timeout(5*time.Second))
	registerHealthRoutes(httpServer, h, cfg)
	if options.HTTPRoutes != nil {
		options.HTTPRoutes(httpServer)
	}
	grpcServer := kgrpc.NewServer(kgrpc.Address(cfg.GRPCAddress))
	grpcEndpoint, err := grpcServer.Endpoint()
	if err != nil {
		return err
	}
	if options.Instance.Service == "" {
		options.Instance = registry.Instance{Service: cfg.ServiceName, ID: cfg.ServiceName + grpcEndpoint.String(), Address: grpcEndpoint.String(), Version: cfg.Version, Environment: cfg.Environment}
	}
	lifecycle := runtime.New(options)
	application := kratos.New(
		kratos.Name(cfg.ServiceName), kratos.Version(cfg.Version),
		kratos.Server(httpServer, grpcServer),
		kratos.BeforeStart(func(ctx context.Context) error {
			err := lifecycle.BeforeStart(ctx)
			// Lifecycle has taken responsibility for all startup cleanup after it
			// has been invoked, including rollback on failure.
			handedOff = true
			if err != nil {
				return err
			}
			h.SetReady(true)
			return nil
		}),
		kratos.AfterStop(func(ctx context.Context) error {
			h.Stop()
			return lifecycle.AfterStop(ctx)
		}),
	)
	err = application.Run()
	if err != nil {
		_ = lifecycle.AfterStop(context.Background())
	}
	return err
}

func registerHealthRoutes(s *khttp.Server, h *health.State, cfg config.Config) {
	s.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { writeHealth(w, h.Status().Live) })
	s.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { writeHealth(w, h.Status().Ready) })
	s.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": cfg.ServiceName, "version": cfg.Version, "environment": cfg.Environment})
	})
}

func writeHealth(w http.ResponseWriter, ok bool) {
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
