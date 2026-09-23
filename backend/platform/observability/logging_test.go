package observability

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	otelog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// memoryExporter 收下送来的每一条日志记录，测试里当采集端用。
type memoryExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *memoryExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (e *memoryExporter) Shutdown(context.Context) error   { return nil }
func (e *memoryExporter) ForceFlush(context.Context) error { return nil }

func (e *memoryExporter) snapshot() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

func newTestLogger(t *testing.T, service string) (*slog.Logger, *memoryExporter) {
	t.Helper()
	exporter := &memoryExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return slog.New(newHandler(Config{ServiceName: service}, provider)), exporter
}

// attributesOf 把一条记录的属性拍平成 键→字符串，分组递归进去——slog.Group 落成
// log 的 MapValue，只走顶层会漏掉藏在组里的密钥。
func attributesOf(record sdklog.Record) map[string]string {
	flat := map[string]string{}
	var walk func(attrs []otelog.KeyValue)
	walk = func(attrs []otelog.KeyValue) {
		for _, kv := range attrs {
			if kv.Value.Kind() == otelog.KindMap {
				walk(kv.Value.AsMap())
				continue
			}
			flat[kv.Key] = kv.Value.AsString()
		}
	}
	record.WalkAttributes(func(kv otelog.KeyValue) bool {
		walk([]otelog.KeyValue{kv})
		return true
	})
	return flat
}

// 带 ctx 打的那条日志要能被 Tempo 里那条链路认出来：trace_id 与 span_id 都得是
// 当前 span 的。跨服务排查「一行日志属于哪一次请求」全指望它。
func TestContextLoggerCarriesSpanIdentity(t *testing.T) {
	logger, exporter := newTestLogger(t, "order-service")
	tracer := sdktrace.NewTracerProvider().Tracer("test")

	ctx, span := tracer.Start(context.Background(), "create-order")
	defer span.End()
	logger.InfoContext(ctx, "订单已创建", "order_no", "SO20260923001")

	records := exporter.snapshot()
	if len(records) != 1 {
		t.Fatalf("采集端收到 %d 条，想要 1 条", len(records))
	}
	want := span.SpanContext()
	if got := records[0].TraceID(); got != want.TraceID() {
		t.Errorf("trace_id = %s, 想要 %s", got, want.TraceID())
	}
	if got := records[0].SpanID(); got != want.SpanID() {
		t.Errorf("span_id = %s, 想要 %s", got, want.SpanID())
	}
	if got := records[0].Body().AsString(); got != "订单已创建" {
		t.Errorf("body = %q", got)
	}
	if got := attributesOf(records[0])["order_no"]; got != "SO20260923001" {
		t.Errorf("order_no = %q, 业务属性没跟着走", got)
	}
}

// ctx 里没有 span 时（后台 worker、启动阶段）也要照发，只是不带 trace_id——
// 发不出去比没有关联信息更糟。
func TestLoggerWithoutSpanStillExports(t *testing.T) {
	logger, exporter := newTestLogger(t, "order-service")

	logger.InfoContext(context.Background(), "定时任务开始")

	records := exporter.snapshot()
	if len(records) != 1 {
		t.Fatalf("采集端收到 %d 条，想要 1 条", len(records))
	}
	if got := records[0].TraceID(); got.IsValid() {
		t.Errorf("没有 span 时不该有 trace_id，得到 %s", got)
	}
}

// 密钥属性在进入任何一路 handler 之前就该被换掉：终端与采集端看到的必须是同一行，
// 否则「本地能看、线上是明文」这种差最难查。
func TestHandlerRedactsSensitiveKeys(t *testing.T) {
	cases := []struct {
		name      string
		secretKey string
		log       func(*slog.Logger, context.Context)
	}{
		{"内联属性", "token", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "调用渠道", "token", "s3cr3t", "order_no", "SO1")
		}},
		{"With 属性", "authorization", func(l *slog.Logger, ctx context.Context) {
			l.With("authorization", "Bearer s3cr3t").InfoContext(ctx, "调用渠道")
		}},
		// 键名照原样留着（带等号），只换值：改键名会让调用点与日志对不上。
		{"老式等号键", "password=", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "调用渠道", "password=", "s3cr3t")
		}},
		{"分组里的属性", "api_key", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "调用渠道", slog.Group("channel", "api_key", "s3cr3t"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, exporter := newTestLogger(t, "payment-service")
			tc.log(logger, context.Background())

			records := exporter.snapshot()
			if len(records) != 1 {
				t.Fatalf("采集端收到 %d 条，想要 1 条", len(records))
			}
			attrs := attributesOf(records[0])
			got, ok := attrs[tc.secretKey]
			if !ok {
				t.Fatalf("属性 %s 没收到，断言等于没跑；收到的是 %v", tc.secretKey, attrs)
			}
			if got != redacted {
				t.Errorf("%s = %q, 没有脱敏", tc.secretKey, got)
			}
		})
	}
}

func TestRedactAttrLeavesOrdinaryKeysAlone(t *testing.T) {
	got := redactAttr(slog.String("order_no", "SO1"))
	if got.Value.String() != "SO1" {
		t.Errorf("普通属性被改动了：%v", got.Value)
	}
}
