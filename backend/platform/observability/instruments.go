package observability

import (
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// scopeName 是平台级 instrument 的 scope。服务自己的请求指标由 Kratos 中间件
// 按服务名建 scope，这里承载的是**跨服务同类**的运行计数（outbox、策略重载、
// 限流拒绝），放在一个 scope 下才好按指标名过滤。
const scopeName = "github.com/panda-dev/panda-v2/backend"

// 指标名。用 panda. 前缀与 Kratos 自带的 server_requests_* 区分开。
const (
	OutboxPublishedName = "panda.outbox.published"
	OutboxFailedName    = "panda.outbox.failed"
	PolicyReloadsName   = "panda.policy.reloads"
	RateLimitDeniedName = "panda.ratelimit.denied"
	// DeadLetterName 数的是「处理失败、重试次数用完、被拒进死信队列」的消息。
	//
	// 死信队列**没有消费者**：消息进去就是等着人捞。没有这个计数，一条消息走到那里
	// 只留下队列深度的一个增量，而队列深度没有基线——涨到 3 还是 300 都不触发任何东西。
	DeadLetterName = "panda.consumer.dead_letters"
)

// Counter 取一个平台计数器。
//
// 这里走 otel 的**全局** meter，而不是把 MeterProvider 一路当参数传下来。全局
// meter 是可后置委派的：在 SetMeterProvider 之前建出来的 instrument 会被登记
// 下来，等 provider 就位时自动接到真实实现上。observability.Init 正是那个
// SetMeterProvider（且只在配了 OTLP 端点时才调），所以组件完全可以先构造、
// 后初始化 —— 顺序不再是耦合点。
//
// 没有配端点时，全局 meter 没有委派对象，返回的是空实现的计数器：不上报、
// 不分配、不报错。这正是 dev 不启 collector 时希望的降级。
func Counter(name, description string) metric.Int64Counter {
	counter, err := otel.Meter(scopeName).Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		// 名字和描述是本包里的常量，OTel 只在它们非法时报错 —— 那是编程错误，
		// 让它在构建/启动时就炸出来，好过每个调用点都处理一遍相同的心跳。
		panic(fmt.Sprintf("observability: create counter %s: %v", name, err))
	}
	return counter
}
