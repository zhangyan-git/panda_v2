package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/metrics"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/health"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
		options.Database, err = database.New(ctx, cfg.ServiceDatabaseURL)
		if err != nil {
			return err
		}
		options.OwnDatabase = true
		cleanup = append(cleanup, func() { options.Database.Close() })
	}
	// Cache 刻意不再自动创建。以前这里会在没注入时建一个，于是每个服务都被
	// 拖上一条 Redis 依赖：启动时 lifecycle 会 Ping 它，Ping 不通就拒绝启动。
	// merchant-service 根本不碰 Redis，却会因为一个它用不到的依赖在 Redis 的
	// 维护窗口里起不来——一个可选依赖变成了硬依赖。
	//
	// 需要 Redis 的服务自己建、自己注入（目前只有 user-service，它要用
	// cache.Client 广播策略变更）。没注入就是没有，lifecycle 全程判空。
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
	// Wire the outbox and the inbox here rather than in each service, so a
	// service cannot end up with an outbox and no relay (events pile up
	// unpublished) or a consumer and no inbox (every redelivery is applied
	// again). Supplying the store is the whole opt-in.
	if options.Outbox != nil && options.Publisher != nil {
		// Noop 发布器会接受每一次投递并回报成功，给它配 relay 等于把每条事件标成
		// 「已投递」再丢掉——outbox 反而被清空，比不投更糟。RABBITMQ_URL 为空时
		// NewRabbitMQ 返回的正是它，dev 不启 MQ 就会走到这里。这时宁可让事件堆在
		// 表里，等服务重启接上 broker 再投。
		if !messaging.Delivers(options.Publisher) {
			slog.Warn("outbox relay is not started because no publisher is configured; events accumulate in the outbox until one is")
		} else {
			published := observability.Counter(observability.OutboxPublishedName, "outbox events published to the broker")
			failed := observability.Counter(observability.OutboxFailedName, "outbox events the broker rejected")
			relay, err := messaging.NewRelay(options.Outbox, options.Publisher, messaging.RelayConfig{
				OnError: func(err error) { slog.Error("outbox relay", "error", err) },
				OnPublished: func(event messaging.LeasedEnvelope) {
					published.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", event.Envelope.EventType)))
				},
				OnFailed: func(event messaging.LeasedEnvelope, err error) {
					failed.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", event.Envelope.EventType)))
					slog.Warn("outbox relay publish failed", "event", event.EventID, "type", event.Envelope.EventType, "error", err)
				},
			})
			if err != nil {
				return err
			}
			options.Workers = append(options.Workers, relay)
		}
	}
	if options.ConsumerInbox != nil && options.Consumer != nil {
		options.Consumer = messaging.WithInbox(options.Consumer, options.ConsumerInbox)
	}
	if options.Observability.Tracer == nil || options.Observability.Meter == nil {
		injected := options.Observability
		obsConfig := observability.ConfigFromEnv()
		// 资源的 service.name 没配 SERVICE_NAME 时取自服务自己，而不是
		// ConfigFromEnv 那个 "panda-service" 兜底：三个服务的 span 顶同一个
		// service.name，跨服务链路就退化成一条谁都认不出来的时间线。
		if strings.TrimSpace(os.Getenv("SERVICE_NAME")) == "" {
			obsConfig.ServiceName = cfg.ServiceName
		}
		providers, err := observability.Init(ctx, obsConfig)
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
	instrumentation, err := serverMiddleware(cfg, options.Observability)
	if err != nil {
		return err
	}
	h := health.New()
	// A zero timeout would be worse than a missing one: kratos turns it into
	// context.WithTimeout(ctx, 0), so every request would be expired before the
	// handler ran. Load always fills it in; this catches a hand-built Config.
	if cfg.HTTPTimeoutMS <= 0 {
		return fmt.Errorf("HTTP timeout must be positive, got %dms", cfg.HTTPTimeoutMS)
	}
	// The timeout is a whole-request context deadline — kratos installs it in the
	// mux filter, so it covers reading the body too, not just the handler. That is
	// why it is configurable: one 10MB upload cannot finish inside the 5s that
	// suits every other route. See HTTPTimeoutMS in platform/config for the
	// per-service default and how it layers under the gateway.
	httpServer := khttp.NewServer(khttp.Address(cfg.HTTPAddress), khttp.Timeout(time.Duration(cfg.HTTPTimeoutMS)*time.Millisecond))
	// Routes go through the router, not the server: this repository registers
	// plain HandleFunc routes, which khttp.Middleware never reaches. See HTTPRouter.
	httpRouter := runtime.NewHTTPRouter(httpServer, instrumentation...)
	registerHealthRoutes(httpRouter, h, cfg)
	// /readyz 必须反映依赖的**当前**状态，而不是启动那一刻的快照。
	// 只在 BeforeStart 里置一次的话，运行中 Postgres 挂掉后探针仍然 200，
	// 负载均衡会继续把请求送到一个每个请求都 500 的副本上。
	//
	// 只把 Postgres 算进就绪。Redis 和 RabbitMQ 在这个仓库里是**可选**依赖——
	// 缺了是降级而不是失败（策略广播退化为只在本实例生效、事件堆在 outbox
	// 里等 broker 回来），这是仓库里明确的安全网。把它们也做成就绪条件，
	// 结果是「Redis 抖一下 → 所有副本被摘 → 全站不可用」，比降级运行糟得多。
	if options.Database != nil {
		db := options.Database
		options.Workers = append(options.Workers, health.NewProbe(h, 0, 0, health.Dependency{
			Name:  "database",
			Check: func(ctx context.Context) error { return db.Ping(ctx) },
		}))
	}
	if options.HTTPRoutes != nil {
		options.HTTPRoutes(httpRouter)
	}
	grpcServer := kgrpc.NewServer(append([]kgrpc.ServerOption{
		kgrpc.Address(cfg.GRPCAddress),
		kgrpc.Middleware(instrumentation...),
	}, options.GRPCServerOptions...)...)
	if options.GRPCRoutes != nil {
		options.GRPCRoutes(grpcServer)
	}
	grpcEndpoint, err := grpcServer.Endpoint()
	if err != nil {
		return err
	}
	if options.Instance.Service == "" {
		instance, err := registry.NewInstance(cfg.ServiceName, grpcEndpoint, cfg.Version, cfg.Environment)
		if err != nil {
			return err
		}
		options.Instance = instance
	}
	lifecycle := runtime.New(options)
	application := kratos.New(
		kratos.Name(cfg.ServiceName), kratos.Version(cfg.Version),
		kratos.Server(httpServer, grpcServer),
		kratos.BeforeStart(func(ctx context.Context) error {
			err := lifecycle.BeforeStart(ctx)
			if err != nil {
				return err
			}
			// Lifecycle has taken responsibility for all startup cleanup after it
			// has been invoked successfully.
			handedOff = true
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

// serverMiddleware 组装 HTTP 与 gRPC server 共用的观测中间件。
//
// 两个细节容易踩空：
//
//   - metrics.Server() 在没有拿到计数器或直方图时是**静默空操作**（内部两个
//     字段都 nil 就直接转发）。所以这里必须把两者都建出来，否则挂了等于没挂。
//   - tracer 显式用注入的 provider，而不是让中间件去取全局的。不配 OTLP 端点
//     时 Init 返回的就是全局的空实现，两个来源一致；但服务若注入了自己的
//     provider，只有显式传入才会走它。
func serverMiddleware(cfg config.Config, providers observability.Providers) ([]middleware.Middleware, error) {
	meter := providers.Meter.Meter(cfg.ServiceName)
	requests, err := metrics.DefaultRequestsCounter(meter, metrics.DefaultServerRequestsCounterName)
	if err != nil {
		return nil, fmt.Errorf("create server request counter: %w", err)
	}
	seconds, err := metrics.DefaultSecondsHistogram(meter, metrics.DefaultServerSecondsHistogramName)
	if err != nil {
		return nil, fmt.Errorf("create server request histogram: %w", err)
	}
	return []middleware.Middleware{
		tracing.Server(tracing.WithTracerProvider(providers.Tracer)),
		metrics.Server(metrics.WithRequests(requests), metrics.WithSeconds(seconds)),
	}, nil
}

func registerHealthRoutes(s *runtime.HTTPRouter, h *health.State, cfg config.Config) {
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
