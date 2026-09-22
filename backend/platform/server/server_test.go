package server

import (
	"context"
	"errors"
	"testing"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"go.opentelemetry.io/otel"
)

// TestServerMiddlewareRecoversHandlerPanics 守的是两个包之间的那根线：panic 在这条链上
// 被翻成 runtime.ErrHandlerPanic，HTTP 那层才认得出该补一个 500（见 HTTPRouter.HandleFunc）。
//
// 少了这根线，一个 panic 会一路走到 net/http，按连接断开收场；而**只要有人吃掉了 panic
// 却没人补响应**，就是一个空 200 —— 调用方把崩溃读成成功。
//
// 链的顺序也在这里钉住：recovery 必须在最靠近处理器的那一端，否则 tracing 与 metrics
// 根本不会被执行到（它们不是 defer 写法），那次崩溃在监控里就是一团空白。
func TestServerMiddlewareRecoversHandlerPanics(t *testing.T) {
	cfg := config.Config{ServiceName: "test-service", HTTPTimeoutMS: 5000}
	// 与不带 OTLP 端点时 Init 返回的是同一对（全局 noop provider）。
	providers := observability.Providers{Tracer: otel.GetTracerProvider(), Meter: otel.GetMeterProvider()}
	middlewares, err := serverMiddleware(cfg, providers)
	if err != nil {
		t.Fatal(err)
	}
	chain := middleware.Chain(middlewares...)

	_, err = chain(func(context.Context, any) (any, error) { panic("boom") })(context.Background(), nil)
	if !errors.Is(err, runtime.ErrHandlerPanic) {
		t.Fatalf("panic 在这条链上交回的是 %v，want runtime.ErrHandlerPanic", err)
	}
}

// panic 的值不跟着错误走：它会原样成为回给调用方的状态消息，而 panic 的值常常就是
// 正在被处理的数据（一句 SQL、一个结构体）。现场在日志里。
func TestServerMiddlewareDoesNotLeakThePanicValue(t *testing.T) {
	cfg := config.Config{ServiceName: "test-service", HTTPTimeoutMS: 5000}
	providers := observability.Providers{Tracer: otel.GetTracerProvider(), Meter: otel.GetMeterProvider()}
	middlewares, err := serverMiddleware(cfg, providers)
	if err != nil {
		t.Fatal(err)
	}
	chain := middleware.Chain(middlewares...)

	_, err = chain(func(context.Context, any) (any, error) {
		panic("select * from users where password='hunter2'")
	})(context.Background(), nil)
	if err == nil {
		t.Fatal("panic 没有变成错误")
	}
	if got := err.Error(); got != runtime.ErrHandlerPanic.Error() {
		t.Fatalf("回给调用方的消息是 %q，它泄了 panic 的值", got)
	}
}
