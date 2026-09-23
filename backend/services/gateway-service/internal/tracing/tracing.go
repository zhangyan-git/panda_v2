// Package tracing 给网关这一跳补上链路里缺的「起点」。
//
// 网关不是 kratos 服务：它手里没有 khttp.Server，所以用不了
// middleware/tracing.Server()，platform/server/runtime.HTTPRouter 也接不了普通的
// http.ServeMux（那边包的是 *khttp.Server）。这里按 kratos 的做法写了份小号：
// 先从入向请求头提取上下文，再起一个 SERVER span，最后把 span 注入到转发出去的
// 那一份请求头上。下游服务本来就认 traceparent，接上这一段整条链就通了。
package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName 是 span 的来源标识，与 kratos 用 "kratos" 是同一个意思。
const instrumentationName = "gateway-service"

// propagator 必须自带，不要换成 otel.GetTextMapPropagator()。
//
// otel 的全局 propagator 默认是一个空的 composite（otel/internal/global/propagator.go
// 里 noop 那一行），而本仓库没有任何地方调用过 otel.SetTextMapPropagator。
// 用它不会报任何错：Inject 什么都不做、Extract 原样返回 ctx，症状是下游永远收不到
// traceparent、链路在网关这里无声断开，而日志里看不到一点异常。kratos 出于同样的
// 理由也自带了一份（middleware/tracing/tracer.go 里 NewTracer 的默认值）。
// 这里与它对齐：TraceContext + Baggage（kratos 多的那个 Metadata{} 是它自己的
// 传输层头，网关用不上）。
var propagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

// Inject 把 ctx 里的 span 写进出向请求头；ctx 里没有 span 时是 no-op。
func Inject(ctx context.Context, header http.Header) {
	propagator.Inject(ctx, propagation.HeaderCarrier(header))
}

// Server 是入向中间件，作用等价于 kratos 的 tracing.Server()。
//
// provider 由调用方传进来、而不是在包内取全局：otel.Tracer() 是在调用那一刻向
// 全局 provider 要 tracer 的，取早了（observability.Init 之前）拿到的就是 noop，
// 之后也不会自己变回来——症状同样是「请求正常、Tempo 里却什么都没有」。
func Server(provider trace.TracerProvider, next http.Handler) http.Handler {
	tracer := provider.Tracer(instrumentationName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先 Extract 再 Start：入向的 traceparent 是这一跳的父。从公网直接打进来的
		// 请求提取不到东西，Start 会自己起一条新链——那条链的起点就是这里。
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, spanName(r), trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		span.SetAttributes(
			semconv.HTTPRequestMethodKey.String(r.Method),
			semconv.URLPathKey.String(r.URL.Path),
		)
		if r.URL.RawQuery != "" {
			span.SetAttributes(semconv.URLQueryKey.String(r.URL.RawQuery))
		}

		recorder := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		elapsed := time.Since(start)

		// 状态码取真正写给客户端的那个：网关自己会产 429/502/504，不记的话
		// Tempo 里这一跳永远看着是成功的，而它恰恰是链路的断点。
		status := recorder.Status()
		span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}

		logRequest(ctx, r, status, elapsed)
	})
}

// logRequest 是网关这一跳的访问日志，一次请求一行。
//
// 为什么要有这一行：面板要的是「一次请求沿途的日志」，而这个仓库的日志几乎都长在
// 出错路径上（Info 36 处、Warn 71 处、Error 70 处，而成功的请求一行都不留）。少了
// 它，成功的那条链路在面板里是空的，全链路日志退化成「全链路错误日志」——一次下单
// 从头到尾做成了，OpenSearch 里却查不到任何东西。
//
// 为什么放在这个中间件里、而不是另起一个：这一行必须带 trace_id，而 trace_id 来自
// span。只有这里同时握着 span 的 ctx 与最终状态码；另起一个中间件就得再包一层
// statusRecorder，两个包装类型对 Flush / Unwrap 的要求还得各写一遍。
//
// 探针不经过这里（/livez 与 /readyz 直接挂在 mux 上，见 cmd/main.go 的 newMux），
// 所以不会把 LB 的高频探测刷进日志。
func logRequest(ctx context.Context, r *http.Request, status int, elapsed time.Duration) {
	// 只记路径，不记 RawQuery：查询串里可能带凭据，而 observability 的
	// redactHandler 只认键名，认不出塞在串里的那一串。
	slog.InfoContext(ctx, "gateway-service: request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	)
}

// spanName 用「方法 + 路径」。
//
// kratos 那边用的是路由模板（/v1/admin/membership-plans/{id}），不带 id、基数低；
// 网关是按前缀 switch 转发的，手里没有模板，只能在路径和「只留方法」之间选。
// 这里选路径：现阶段要的就是「一眼看出这条 trace 是哪次请求」，路径带上 id 才有
// 这个作用。将来若 span 名基数成为问题，改成只留方法、把路径挪到 url.path 属性。
func spanName(r *http.Request) string {
	return r.Method + " " + r.URL.Path
}

// statusRecorder 记下真正写给客户端的状态码。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Status 返回最终状态码；处理器一个字都没写时按 200 算，与 net/http 的语义一致。
func (r *statusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Unwrap 让 http.ResponseController 能穿透这层包装。
//
// 不写它的话，上游的流式响应（Flush）和协议升级会静默降级：包装类型没实现
// http.Flusher，ReverseProxy 会认为客户端这一侧不支持流式，于是把整个响应
// 攒完再一次性发出去——接口本身不报错，只是不再边算边推。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush 直接转发给底下的 ResponseWriter。
//
// Unwrap 只对走 http.ResponseController 的代码有用，而 httputil.ReverseProxy
// 是直接做类型断言找 Flusher 的，穿透不过去，所以这里再补一个。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
