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
	"github.com/go-kratos/kratos/v2/middleware/recovery"
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

const (
	// readHeaderTimeout 防慢速请求头：一个连上就不把请求头发完的客户端本来能一直占着
	// 连接，而**在途连接会让 Shutdown 一直等**（它等的是连接变空闲，不是等 ctx 到期）。
	// khttp 建出来的 http.Server 只设了 Handler 与 TLSConfig，这个字段默认是 0，也就是
	// 「永远等」。只限头不限体：上传接口的正文本来就慢，掐它的是请求自己的超时。
	readHeaderTimeout = 10 * time.Second

	// drainDelay 是「已经把自己摘掉」到「停止接受连接」之间的等待。
	//
	// 摘注册中心是立刻生效的（服务之间的 gRPC 按 etcd 找实例，watch 是推送），但平台
	// 外侧的 LB 按 /readyz 探，而探测有间隔。立刻停服务器的话，从摘掉到 LB 发现之间那几秒
	// 里 LB 仍把请求发过来，全部变成 502。等一个探测周期，让它先自己把这一台摘掉。
	// 取值要大于健康检查间隔（常见 2–5s），与网关的 drainDelay 同一个口径。
	drainDelay = 5 * time.Second

	// shutdownGrace 是停止预算里留给收尾的余量（写响应、记日志、提交那一次事务）。
	shutdownGrace = 5 * time.Second
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
	// 见 readHeaderTimeout：这个字段不设就是「不收完头也一直算在途」。
	httpServer.ReadHeaderTimeout = readHeaderTimeout
	// stopTimeout 是「停止」这件事的预算：一个请求的预算 + 一点收尾余量。
	//
	// **不设它不是「等一会儿」，是「永远等」**：kratos 只在 stopTimeout > 0 时才给停止
	// 上下文加 deadline（app.go:106-108）。没有 deadline 时两边的 force-close 分支都不
	// 会触发——khttp 的 Shutdown 一直等在途连接（哪怕只剩一个不发请求头的 socket），
	// gRPC 的 GracefulStop 一直等最后一条流——于是 application.Run() 不返回，AfterStop
	// 里那一串（停 worker、停消费者、注销、停 outbox relay、关连接池）一次都不跑，
	// 最后由编排器 SIGKILL 收场：注册中心里留下一个死实例，消费者被硬杀在半个消息上。
	//
	// 编排器的 terminationGracePeriod 要设得比它长，否则掐断的还是编排器自己。
	stopTimeout := time.Duration(cfg.HTTPTimeoutMS)*time.Millisecond + shutdownGrace
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
		// 每次 RPC 的整调用预算。**这一行不能省**：kratos 的 gRPC server 自带一个 1 秒的
		// 默认值（transport/grpc/server.go 的 NewServer），而它在 interceptor 里照样会套一层
		// context.WithTimeout（transport/grpc/interceptor.go）——不显式给，全仓每一个 gRPC
		// 处理器就都跑在一个没人选过的 1 秒里。
		//
		// 那个默认值会**静默吃掉调用方写下的预算**：deviceLookupTimeout 2 秒、
		// paymentCreateTimeout 5 秒、authorizationTimeout 5 秒，每一个都在代码里配了注释解释
		// 为什么是这么多，却一次都没生效过——下游还没答完，服务端先按 1 秒把自己掐断，再把它
		// 当成一次故障（Internal）报回调用方。退款那条路正是撞在这上面：渠道的应答稍慢于 1 秒，
		// 订单侧就收到一句「支付服务不可用」，而真相是「我们自己没等到结论」。
		//
		// 值与 HTTP 面共用同一个预算：在这一层两者管的是同一件事——一个处理单元最多能花多久。
		// 按服务给的那两个默认值（5 秒，merchant-service 90 秒）本来就是按「这个服务的处理单元
		// 要花多久」定的，与传输是 HTTP 还是 gRPC 无关。
		kgrpc.Timeout(time.Duration(cfg.HTTPTimeoutMS) * time.Millisecond),
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
		// 排空：先把不接收新流量的意思表达出去——本进程的 /readyz 立刻转 503，注册中心
		// 里那一条也删掉——然后等一个探测周期，最后才让 kratos 停服务器。
		//
		// 顺序是全部意义所在。kratos 只是在 beforeStop 之后 cancel 掉那个 ctx，服务器
		// 这才开始停；而**注销注册中心原本要等到 AfterStop**，也就是服务器停完之后。
		// 只把等待留在这里、不提前注销的话，那个周期里调用方仍在往一台已经不听端口的
		// 实例上发请求，正好是要避开的那段 502。
		kratos.BeforeStop(func(ctx context.Context) error {
			h.SetReady(false)
			if err := lifecycle.Drain(ctx); err != nil {
				// 摘不掉不是致命的：AfterStop 里还会再试一次；但这一轮排空就不成立了，
				// 得让人看见。
				slog.Error("drain: deregister failed; instance stays discoverable until shutdown", "error", err)
			}
			time.Sleep(drainDelay)
			return nil
		}),
		// 见 stopTimeout：这一行就是那个「有截止时间的停止」。
		kratos.StopTimeout(stopTimeout),
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
	// recovery 放在最后 = 最靠近处理器，这是有意的：panic 在这一层就被翻成一个错误，
	// 外面的 tracing 与 metrics 看到的是「一次 500」，而不是「一次什么都没记录的崩溃」。
	// 反过来的话（它挂在最外层）这两个中间件根本不会被执行到——它们是按处理器正常返回
	// 写的，不是按 defer 写的。
	//
	// 处理器 panic 在 gRPC 那条路是**直接把进程带走**（grpc-go 不 recover），在 HTTP
	// 那条路是断连；两边都收不到一个能看的错误。
	return []middleware.Middleware{
		tracing.Server(tracing.WithTracerProvider(providers.Tracer)),
		metrics.Server(metrics.WithRequests(requests), metrics.WithSeconds(seconds)),
		// 不给 handler：kratos 自己会把 panic 的值、请求和调用栈写进日志（它用的是
		// kratos 的 logger，落 stderr），这里只负责把错误值换成我们自己那一个——
		// 默认那个叫 UNKNOWN，HTTP 这层认不出它，而认不出就等于**空 200**（见
		// runtime.HTTPRouter.HandleFunc 的收尾）。
		recovery.Recovery(recovery.WithHandler(func(context.Context, any, any) error {
			return runtime.ErrHandlerPanic
		})),
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
