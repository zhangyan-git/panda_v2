// Package health 记录一个进程的存活与就绪状态，并提供周期性依赖探测。
//
// 两层状态是分开的，因为它们的失效方式不同：
//
//   - ready：启动流程走完了没有。一次性，成功之后基本不再变，停机时清掉。
//   - deps：依赖**现在**还可不可达。会来回变，需要有人在后台一直看。
//
// 合成的 Ready 才是 /readyz 的答案。如果只有 ready 这一层，运行中 Postgres
// 挂掉后探针仍然是 200，负载均衡会继续把请求送到一个每个请求都 500 的副本上。
package health

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// 探测的默认节奏。间隔远小于「运维发现不对劲」的时间，又远大于一次
// Ping 的成本；超时给得比间隔小一个量级，探测自己不能变成拖累。
const (
	defaultProbeInterval = 15 * time.Second
	defaultProbeTimeout  = 3 * time.Second
)

type Status struct {
	Live  bool
	Ready bool
}

// Check 检查一个依赖是否可用，返回 nil 表示可用。
type Check func(ctx context.Context) error

// Dependency 是一个被周期性检查的依赖。Name 只用于日志——探针的结论是
// 合起来的，负载均衡要的是「这一台能不能用」，不是「哪个依赖坏了」。
type Dependency struct {
	Name  string
	Check Check
}

type State struct {
	live  atomic.Bool
	ready atomic.Bool
	// deps 初始为 true：构造完到第一次探测之间有一个窗口，这期间报不就绪
	// 会让每一次滚动发布都先把副本摘一遍。启动流程本身会 ping 依赖并且
	// 失败即拒绝启动，所以「还没探过」时乐观是有依据的，不是赌。
	deps atomic.Bool
}

func New() *State {
	s := &State{}
	s.live.Store(true)
	s.deps.Store(true)
	return s
}

func (s *State) SetReady(v bool) { s.ready.Store(v) }

// SetDependenciesReachable 写入依赖探测结果，返回这次调用有没有改变状态。
// 返回值给调用方做「只在翻转时打日志」用——依赖挂着的时候每 15s 一条日志
// 会把「什么时候开始坏的」这条真正有用的信息刷没。
func (s *State) SetDependenciesReachable(v bool) bool {
	return s.deps.Swap(v) != v
}

func (s *State) Status() Status {
	// Ready 是两层的合成：启动没走完、或者依赖不可达，都不该收流量。
	return Status{Live: s.live.Load(), Ready: s.ready.Load() && s.deps.Load()}
}

func (s *State) Stop() {
	s.live.Store(false)
	s.ready.Store(false)
}

// Probe 实现 runtime.Runner：周期性检查依赖，把结果写进 State。
//
// 注册成 Worker 而不是自己起 goroutine，是为了让它跟着服务的生命周期走——
// 停机时 ctx 结束、循环退出，不需要另写一套停止逻辑。
type Probe struct {
	state    *State
	interval time.Duration
	timeout  time.Duration
	deps     []Dependency
}

// NewProbe 建一个依赖探测。interval 和 timeout 传 0 取默认值。
func NewProbe(state *State, interval, timeout time.Duration, deps ...Dependency) *Probe {
	if interval <= 0 {
		interval = defaultProbeInterval
	}
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	return &Probe{state: state, interval: interval, timeout: timeout, deps: deps}
}

// Run 先立刻探一次，再按间隔重复。
//
// 立刻探这一下不是多余的：State 的 deps 初值是 true，第一次 tick 之前
// 那 15s 里探针会给一个乐观的答案。启动阶段刚刚 ping 过依赖，这里再确认
// 一次，就让「报了就绪」和「依赖确实可达」之间没有空档。
func (p *Probe) Run(ctx context.Context) error {
	p.observe(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 与其它 Runner 一致：正常停机返回 ctx.Err()，由生命周期认出
			// context.Canceled 不算错误。
			return ctx.Err()
		case <-ticker.C:
			p.observe(ctx)
		}
	}
}

func (p *Probe) observe(ctx context.Context) {
	reachable, failed := p.checkAll(ctx)
	if !p.state.SetDependenciesReachable(reachable) {
		return
	}
	if reachable {
		slog.Info("dependencies are reachable again; readiness restored")
		return
	}
	slog.Error("dependency unreachable; readiness withdrawn", "dependency", failed)
}

// checkAll 逐个探，返回是否全部可达以及第一个失败的依赖名。
// 全部探完而不是短路：这是每 15s 一次的后台动作，不是请求路径，
// 一次把坏掉的都记下来比省那一次 Ping 有用。
func (p *Probe) checkAll(ctx context.Context) (bool, string) {
	reachable, failed := true, ""
	for _, dep := range p.deps {
		if dep.Check == nil {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
		err := dep.Check(checkCtx)
		cancel()
		if err != nil && reachable {
			reachable, failed = false, dep.Name
		}
	}
	return reachable, failed
}
