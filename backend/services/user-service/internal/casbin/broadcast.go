package casbin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PolicyChangedChannel 是策略变更广播频道。消息体只有发布者的实例标识：
// 它表达的是「规则快照该重读了」，不是规则本身——收到的一方自己去库里读最新的，
// 也就不会出现「广播里的规则和库里的规则不一致」这种状态。
const PolicyChangedChannel = "panda:policy:changed"

// publishTimeout 限制广播的等待时间。Redis 不可达时不能让后台的一次角色保存
// 一直挂在这里，超时按失败处理，交给调用方决定要不要重试。
const publishTimeout = 3 * time.Second

// policyResyncInterval 是兜底重同步的间隔。广播是快路径，这个是慢路径：
// 广播丢了（Redis 抖动、重启、订阅断开重连的那几秒）不会有人补发，
// 而副本停在旧策略上没有外部迹象——只有下一次变更或重启才会纠正。
//
// 30s 的取舍：远小于「运维发现权限不对」的时间，又足够廉价——
// 一次重载就是一个小表全量读，副本数乘上每分钟两次，对 Postgres 不算事。
const policyResyncInterval = 30 * time.Second

// ErrBroadcast 表示本实例已经按新策略生效，但没能把这次变更告诉别的副本。
// 与 ErrRolePolicyReload 属于同一类问题——保存成功了，系统整体却可能不一致——
// 所以同样应该让调用方重试。
var ErrBroadcast = errors.New("策略已在本实例生效，但未能通知其它副本")

// Reloader 是 Broadcaster 需要的最小能力：重读策略快照。*Enforcer 是唯一的生产实现；
// 声明成接口是为了让「谁会被重载、谁会跳过自己」这段判断可以脱离数据库来测。
type Reloader interface {
	Reload() error
}

// Broadcaster 把一次策略变更扩散到所有副本：提交变更的副本先本地 Reload
// （保持原来「同一请求内同步生效」的语义），再广播自己的实例标识；其余副本
// 收到后各自 Reload。
//
// 广播只是快路径。它依赖 Redis，所以会丢：Redis 抖动或重启时那条消息就没了，
// 而重载失败的副本会一直停在旧策略上，没有任何外部迹象。所以订阅循环里还有
// 一条周期性的兜底重载——广播丢了，最迟一个间隔后自己纠回来。
//
// 广播的只是「该重读了」，不是鉴权结果。鉴权本身仍然每次实时查库
// （见 handler 的授权中间件），这条链路不给它加上任何缓存语义。
type Broadcaster struct {
	enforcer Reloader
	client   cache.Client
	instance string
	// resyncInterval 兜底重同步间隔，取自 policyResyncInterval。
	// 做成字段是为了让测试能用秒级间隔跑真实的订阅循环，而不是复刻一份逻辑。
	resyncInterval time.Duration
	// reloads 按触发方分别计数：local（本副本提交的变更）、remote（别的副本广播
	// 过来的）、periodic（兜底重同步）。local 和 remote 长期对不上，就说明有一个
	// 副本没收到广播，或者广播发不出去；periodic 的数就是这份怀疑的证据。
	reloads metric.Int64Counter
}

// NewBroadcaster 接受一个空的 client 并按 Noop 处理：REDIS_ADDR 未配置时，
// 服务照常启动，Reload 退化为「只在本实例生效」。
func NewBroadcaster(enforcer Reloader, client cache.Client, instanceID string) *Broadcaster {
	if client == nil {
		client = cache.Noop{}
	}
	// 从全局 meter 取，构造时机早于 observability.Init 也没关系——全局 meter 会把
	// instrument 登记下来，等 provider 就位再委派过去。
	return &Broadcaster{
		enforcer:       enforcer,
		client:         client,
		instance:       instanceID,
		resyncInterval: policyResyncInterval,
		reloads:        observability.Counter(observability.PolicyReloadsName, "casbin policy reloads by trigger"),
	}
}

// countReload 记一次重载。trigger 只有 local / remote 两个取值，基数固定。
func (b *Broadcaster) countReload(trigger string) {
	b.reloads.Add(context.Background(), 1, metric.WithAttributes(attribute.String("trigger", trigger)))
}

// Reload 实现 service.PolicyReloader：本地重载 + 广播给其它副本。
func (b *Broadcaster) Reload() error {
	if err := b.enforcer.Reload(); err != nil {
		return err
	}
	b.countReload("local")
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := b.client.Publish(ctx, PolicyChangedChannel, []byte(b.instance)); err != nil {
		return fmt.Errorf("%w: %w", ErrBroadcast, err)
	}
	return nil
}

// Run 实现 runtime.Runner：订阅广播，收到别的副本的变更就本地重载。
// 订阅随服务生命周期启停，ctx 结束时退出。
func (b *Broadcaster) Run(ctx context.Context) error {
	sub, err := b.client.Subscribe(ctx, PolicyChangedChannel)
	if err != nil {
		// 与 Redis 连不上时的其它启动检查一致：宁可起不来，也不要起一个
		// 从此收不到策略变更、却看起来正常的副本。
		return fmt.Errorf("subscribe policy changes: %w", err)
	}
	defer func() { _ = sub.Close() }()

	// 兜底重同步。放在 Subscribe 成功之后：订阅没建起来就直接返回错误，
	// 不该留下一个没人停的 ticker。
	ticker := time.NewTicker(b.resyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// 不区分「本副本刚改过」——重载是幂等的，判断谁改过反而要多存一份状态，
			// 而这份状态本身也会在广播丢失时失准。
			if err := b.enforcer.Reload(); err != nil {
				// 同远端重载失败：记下来继续听，不让循环退出——退出会让这个副本
				// 永远停在旧规则上，正是这条兜底要防的事。
				slog.Error("periodic policy resync", "error", err)
				continue
			}
			b.countReload("periodic")
		case payload, ok := <-sub.Channel():
			if !ok {
				// go-redis 只在 Close 时关闭消息通道，走到这里说明订阅没了。
				// 静默返回 nil 会让这个副本从此不再跟随策略变更，且没有任何迹象。
				return errors.New("policy change subscription closed")
			}
			if string(payload) == b.instance {
				continue // 自己发的：Reload 里已经重载过了
			}
			if err := b.enforcer.Reload(); err != nil {
				// 重载失败只影响本副本，变更本身在别处已经成功，所以记下来继续听，
				// 而不是让订阅循环退出——退出会让这个副本永远停在旧规则上。
				slog.Error("reload policy after remote change", "error", err, "from", string(payload))
				continue
			}
			b.countReload("remote")
			slog.Info("policy reloaded after remote change", "from", string(payload))
		}
	}
}
