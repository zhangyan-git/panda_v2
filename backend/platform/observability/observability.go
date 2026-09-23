package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.27.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Logger is the small logging contract used by platform packages.
type Logger interface {
	Info(string, ...any)
	Error(string, ...any)
}

type NopLogger struct{}

func (NopLogger) Info(string, ...any)  {}
func (NopLogger) Error(string, ...any) {}

// Config controls observability initialization.
type Config struct {
	ServiceName  string
	OTELEndpoint string
	LogLevel     slog.Level
}

func ConfigFromEnv() Config {
	return Config{ServiceName: envOr("SERVICE_NAME", "panda-service"), OTELEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")}
}

// serviceName 兜底成 "panda-service"：SERVICE_NAME 没配时所有服务的 span 与日志会
// 顶同一个名字，跨服务时间线就退化成一条谁都认不出来的流水。server.New 会在
// SERVICE_NAME 为空时把服务自己的名字塞进 cfg，所以正常路径上走不到这个兜底。
func (c Config) serviceName() string {
	if name := strings.TrimSpace(c.ServiceName); name != "" {
		return name
	}
	return "panda-service"
}

func NewLogger(cfg Config) Logger {
	return &slogLogger{logger: slog.New(newHandler(cfg, nil))}
}

type slogLogger struct{ logger *slog.Logger }

func (l *slogLogger) Info(message string, args ...any) {
	l.logger.Info(message, filteredArgs(args...)...)
}
func (l *slogLogger) Error(message string, args ...any) {
	l.logger.Error(message, filteredArgs(args...)...)
}

func filteredArgs(args ...any) []any {
	result := make([]any, 0, len(args))
	for i := 0; i < len(args); i++ {
		if key, ok := args[i].(string); ok && i+1 < len(args) {
			if isSensitive(key) {
				result = append(result, key, redacted)
				i++
				continue
			}
		}
		result = append(result, args[i])
	}
	return result
}

// Providers contains tracer, meter and logger providers. Without an OTEL endpoint,
// the SDK noop providers are returned and no exporter or network activity occurs;
// 日志仍会以 JSON 落到 stdout，只是不发往采集端。
type Providers struct {
	Tracer   oteltrace.TracerProvider
	Meter    metric.MeterProvider
	Logger   *sdklog.LoggerProvider
	shutdown func(context.Context) error
}

func (p Providers) Shutdown(ctx context.Context) error {
	if p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

func Init(ctx context.Context, cfg Config) (Providers, error) {
	if strings.TrimSpace(cfg.OTELEndpoint) == "" {
		// 没有采集端也装默认 logger：两条路径的日志格式与脱敏保持一致，dev 下看到的
		// 就是生产上会落进 OpenSearch 的那一行，不会「本地一套、线上另一套」。
		installDefaultLogger(newHandler(cfg, nil))
		return Providers{Tracer: otel.GetTracerProvider(), Meter: otel.GetMeterProvider()}, nil
	}

	endpoint, err := normalizeEndpoint(cfg.OTELEndpoint)
	if err != nil {
		return Providers{}, err
	}
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return Providers{}, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return Providers{}, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpoint(endpoint), otlploggrpc.WithInsecure())
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		_ = metricExporter.Shutdown(ctx)
		return Providers{}, fmt.Errorf("create OTLP log exporter: %w", err)
	}

	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(cfg.serviceName()))
	tracerProvider := trace.NewTracerProvider(
		trace.WithSampler(trace.ParentBased(trace.AlwaysSample())),
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	installDefaultLogger(newHandler(cfg, logProvider))
	return Providers{Tracer: tracerProvider, Meter: meterProvider, Logger: logProvider, shutdown: func(ctx context.Context) error {
		// logProvider 一并关：批处理器还攒着没发的一批，不关就随进程一起丢掉。
		return errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx), logProvider.Shutdown(ctx))
	}}, nil
}

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Scheme != "" {
		if parsed.Host == "" || parsed.Path != "" && parsed.Path != "/" {
			return "", fmt.Errorf("invalid OTLP endpoint %q: expected host:port or URL without path", endpoint)
		}
		endpoint = parsed.Host
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	if endpoint == "" {
		return "", fmt.Errorf("OTLP endpoint must not be empty")
	}
	return endpoint, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

var _ = metric.Meter(nil)
