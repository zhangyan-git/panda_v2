package observability

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// newHandler 组装进程默认的 slog handler，一条记录同时给两个去处：
//
//   - stdout 的 JSON —— dev 下终端与 docker logs 照旧能看，格式与有没有采集端无关；
//   - OTLP —— 由 collector 落 OpenSearch。只有配了端点才有这一路。
//
// 两路外面套同一层脱敏，所以终端里看到的行与 OpenSearch 里查到的行逐字相同：接了
// 采集端不会多泄一份，也不会出现「终端里是 [REDACTED]、采集端里是明文」这种最难查的差。
func newHandler(cfg Config, logProvider *sdklog.LoggerProvider) slog.Handler {
	handlers := []slog.Handler{
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}),
	}
	if logProvider != nil {
		handlers = append(handlers, otelslog.NewHandler(cfg.serviceName(), otelslog.WithLoggerProvider(logProvider)))
	}
	return redactHandler{next: fanoutHandler{handlers: handlers}}
}

// installDefaultLogger 换掉进程默认 logger。
//
// 各服务写日志用的是标准库 slog（`slog.InfoContext(ctx, ...)` 这类），所以换掉
// slog 的 default 就等于一次改完全部调用点，不用逐个服务改。
//
// 带 ctx 的那些调用会**自动**带上 trace_id/span_id：otelslog 把 ctx 原样交给
// sdk/log 的 Logger.Emit，而后者自己 `trace.SpanContextFromContext(ctx)` 取当前
// span。所以「一行日志能不能顺着链路查到」取决于调用点有没有把 ctx 传下去，与这
// 里无关——写新代码时要传 ctx。
func installDefaultLogger(handler slog.Handler) {
	slog.SetDefault(slog.New(handler))
}

// fanoutHandler 把一条记录原样交给每个下游 handler。
//
// 下游各自判 Enabled：stdout 那一路按 LogLevel 过滤，OTLP 那一路恒真。所以调高
// LogLevel 只让终端变安静，采集端仍拿全量——终端上被过滤掉的那条，OpenSearch 里还在。
type fanoutHandler struct{ handlers []slog.Handler }

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, next := range h.handlers {
		if next.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, next := range h.handlers {
		if !next.Enabled(ctx, record.Level) {
			continue
		}
		// 每条一路给一份 Clone：LogRecord 会被下游持有（批处理器要攒够一批才发），
		// 几路共用一个 Record 的话，后跑的那路可能看见前一路改过的内容。
		errs = append(errs, next.Handle(ctx, record.Clone()))
	}
	return errors.Join(errs...)
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	wrapped := make([]slog.Handler, 0, len(h.handlers))
	for _, next := range h.handlers {
		wrapped = append(wrapped, next.WithAttrs(attrs))
	}
	return fanoutHandler{handlers: wrapped}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	wrapped := make([]slog.Handler, 0, len(h.handlers))
	for _, next := range h.handlers {
		wrapped = append(wrapped, next.WithGroup(name))
	}
	return fanoutHandler{handlers: wrapped}
}

// redactHandler 把键命中 sensitive 的属性值换成 [REDACTED]。
//
// 只看键名，不看值——认不出一个自由文本里是不是密钥，也不去猜；值里带密钥的调用点
// 自己要把它放进名为 token/password/... 的键里。WithAttrs 那一路也要过一遍：写在
// `slog.With("token", v)` 上的属性根本不经过 Handle，只在 WithAttrs 里出现一次。
type redactHandler struct{ next slog.Handler }

func (h redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactHandler) Handle(ctx context.Context, record slog.Record) error {
	out := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		out.AddAttrs(redactAttr(attr))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		masked = append(masked, redactAttr(attr))
	}
	return redactHandler{next: h.next.WithAttrs(masked)}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	if isSensitive(attr.Key) {
		return slog.String(attr.Key, redacted)
	}
	if attr.Value.Kind() == slog.KindGroup {
		group := attr
		group.Value = slog.GroupValue(redactAttrs(attr.Value.Group())...)
		return group
	}
	return attr
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	masked := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		masked = append(masked, redactAttr(attr))
	}
	return masked
}

// redacted 是替换值，sensitive 是判定用的键名表；两处都在这里，改一处就够。
const redacted = "[REDACTED]"

var sensitive = map[string]struct{}{"password": {}, "passwd": {}, "secret": {}, "token": {}, "access_token": {}, "refresh_token": {}, "authorization": {}, "cookie": {}, "api_key": {}}

// isSensitive 认键名，尾部的 "=" 先去掉：老式调用写成 `log.Info("token=", v)`，
// 键名里带着等号。
func isSensitive(key string) bool {
	_, secret := sensitive[strings.ToLower(strings.TrimSuffix(key, "="))]
	return secret
}
