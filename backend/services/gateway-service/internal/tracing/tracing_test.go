package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestProvider 把 span 同步收进内存，测完直接查这一跳发了什么。
func newTestProvider() (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	return sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)), exporter
}

// TestServerInjectsTraceparentIntoOutbound 是这一整个包存在的理由。
//
// 它复现的是网关转发的那一步：中间件起 span，proxy.Rewrite 把 span 注入出向头。
// 注入若坏了不会报任何错——下游只是收不到 traceparent，链路在网关这一跳断开，
// 服务端日志里一片正常。所以这条断言必须钉死。
func TestServerInjectsTraceparentIntoOutbound(t *testing.T) {
	provider, _ := newTestProvider()

	var serverSpan trace.SpanContext
	var outbound string
	downstream := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		serverSpan = trace.SpanContextFromContext(r.Context())
		// 与 proxy.Rewrite 里那行是同一件事：把当前 span 写进出向请求头。
		headers := http.Header{}
		Inject(r.Context(), headers)
		outbound = headers.Get("traceparent")
	})

	Server(provider, downstream).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/admin/orders", nil))

	if outbound == "" {
		t.Fatal("出向请求头里没有 traceparent：下游会把这当成一条新链路的起点，跨服务的日志再也串不起来")
	}
	if !serverSpan.IsValid() {
		t.Fatal("处理器里取不到有效的 span context")
	}
	// traceparent 的格式是 00-<32位 trace id>-<16位 span id>-<flags>。
	if want := serverSpan.TraceID().String(); !strings.Contains(outbound, want) {
		t.Fatalf("traceparent %q 里没有这一跳的 trace id %s", outbound, want)
	}
	if want := serverSpan.SpanID().String(); !strings.Contains(outbound, want) {
		t.Fatalf("traceparent %q 里没有这一跳的 span id %s", outbound, want)
	}
}

// TestServerContinuesInboundTrace 钉住提取那一步：带着 traceparent 进来的请求，
// 这一跳必须挂在那条链上，而不是自己另起一条。
func TestServerContinuesInboundTrace(t *testing.T) {
	provider, exporter := newTestProvider()

	inboundTraceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	inboundSpanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	propagation.TraceContext{}.Inject(
		trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    inboundTraceID,
			SpanID:     inboundSpanID,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})),
		propagation.HeaderCarrier(headers),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/orders", nil)
	req.Header.Set("traceparent", headers.Get("traceparent"))
	Server(provider, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("期望 1 条 span，实际 %d 条", len(spans))
	}
	span := spans[0]
	if span.SpanContext.TraceID() != inboundTraceID {
		t.Errorf("trace id = %s，期望沿用入向的 %s", span.SpanContext.TraceID(), inboundTraceID)
	}
	if span.Parent.SpanID() != inboundSpanID {
		t.Errorf("父 span id = %s，期望入向的 %s", span.Parent.SpanID(), inboundSpanID)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Errorf("span kind = %v，期望 SERVER", span.SpanKind)
	}
	if want := "GET /v1/admin/orders"; span.Name != want {
		t.Errorf("span 名 = %q，期望 %q", span.Name, want)
	}
}

// TestServerRecordsStatus 钉住「网关这一跳是怎么失败的」。
//
// 429/502/504 都是网关自己产的，不记状态码的话 Tempo 里这一跳永远是成功的，
// 而它恰恰是链路断掉的那一跳。
func TestServerRecordsStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantError bool
	}{
		{name: "上游正常", status: http.StatusOK, wantError: false},
		{name: "客户端错误不改 span 状态", status: http.StatusNotFound, wantError: false},
		{name: "上游挂了", status: http.StatusBadGateway, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, exporter := newTestProvider()
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})
			Server(provider, handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/admin/orders", nil))

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("期望 1 条 span，实际 %d 条", len(spans))
			}
			value, ok := attrValue(spans[0].Attributes, "http.response.status_code")
			if !ok {
				t.Fatal("span 上没有 http.response.status_code")
			}
			if got := value.AsInt64(); got != int64(tt.status) {
				t.Errorf("http.response.status_code = %d，期望 %d", got, tt.status)
			}
			if got := spans[0].Status.Code.String(); (got == "Error") != tt.wantError {
				t.Errorf("span 状态 = %s，期望 Error=%v", got, tt.wantError)
			}
		})
	}
}

// TestServerTreatsSilentHandlerAsOK 覆盖「处理器一个字都没写」的情况：net/http
// 会替它回 200，span 上的状态码也该是 200，不能是 0。
func TestServerTreatsSilentHandlerAsOK(t *testing.T) {
	provider, exporter := newTestProvider()
	Server(provider, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livez", nil))

	value, ok := attrValue(exporter.GetSpans()[0].Attributes, "http.response.status_code")
	if !ok {
		t.Fatal("span 上没有 http.response.status_code")
	}
	if got := value.AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d，期望 %d", got, http.StatusOK)
	}
}

// TestServerLogsRequestWithTraceID 钉住访问日志与链路的绑定。
//
// 面板里能不能看见一次成功的业务流，全看这条日志；而它有没有用，全看它带没带上
// trace_id——不带的话它就只是又一行没人看的流水，搜一次请求搜不出来。这条断言
// 把「日志里的 trace id == 这一跳 span 的 trace id」钉死。
func TestServerLogsRequestWithTraceID(t *testing.T) {
	capture := &captureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	provider, exporter := newTestProvider()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	Server(provider, handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/admin/orders", nil))

	if len(capture.records) != 1 {
		t.Fatalf("期望 1 条访问日志，实际 %d 条", len(capture.records))
	}
	if want := "gateway-service: request"; capture.records[0].Message != want {
		t.Errorf("日志消息 = %q，期望 %q", capture.records[0].Message, want)
	}
	spanContext := exporter.GetSpans()[0].SpanContext
	if got := capture.spans[0].TraceID(); got != spanContext.TraceID() {
		t.Errorf("日志里的 trace id = %s，期望这一跳的 %s——不一致的话面板搜不到这条日志", got, spanContext.TraceID())
	}

	attrs := map[string]any{}
	capture.records[0].Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	if got := attrs["status"]; got != int64(http.StatusTeapot) {
		t.Errorf("日志 status = %v，期望 %d", got, http.StatusTeapot)
	}
	if got := attrs["path"]; got != "/v1/admin/orders" {
		t.Errorf("日志 path = %v，期望 /v1/admin/orders", got)
	}
}

// captureHandler 把记录连同处理它时拿到的 ctx 一起收下来——ctx 才是这条测试的
// 观测对象（trace id 在里面）。
type captureHandler struct {
	records []slog.Record
	spans   []trace.SpanContext
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.records = append(h.records, record)
	h.spans = append(h.spans, trace.SpanContextFromContext(ctx))
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func attrValue(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
