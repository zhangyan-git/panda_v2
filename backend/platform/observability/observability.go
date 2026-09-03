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
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
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

func NewLogger(cfg Config) Logger {
	return &slogLogger{logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))}
}

type slogLogger struct{ logger *slog.Logger }

func (l *slogLogger) Info(message string, args ...any) {
	l.logger.Info(message, filteredArgs(args...)...)
}
func (l *slogLogger) Error(message string, args ...any) {
	l.logger.Error(message, filteredArgs(args...)...)
}

var sensitive = map[string]struct{}{"password": {}, "passwd": {}, "secret": {}, "token": {}, "access_token": {}, "refresh_token": {}, "authorization": {}, "cookie": {}, "api_key": {}}

func filteredArgs(args ...any) []any {
	result := make([]any, 0, len(args))
	for i := 0; i < len(args); i++ {
		if key, ok := args[i].(string); ok && i+1 < len(args) {
			if _, secret := sensitive[strings.ToLower(strings.TrimSuffix(key, "="))]; secret {
				result = append(result, key, "[REDACTED]")
				i++
				continue
			}
		}
		result = append(result, args[i])
	}
	return result
}

// Providers contains tracer and meter providers. Without an OTEL endpoint,
// the SDK noop providers are returned and no exporter or network activity occurs.
type Providers struct {
	Tracer   oteltrace.TracerProvider
	Meter    metric.MeterProvider
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

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "panda-service"
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName))
	tracerProvider := trace.NewTracerProvider(
		trace.WithSampler(trace.ParentBased(trace.AlwaysSample())),
		trace.WithBatcher(traceExporter),
		trace.WithResource(res),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	return Providers{Tracer: tracerProvider, Meter: meterProvider, shutdown: func(ctx context.Context) error {
		return errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
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
