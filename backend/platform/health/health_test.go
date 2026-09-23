package health

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// flakyCheck 的结论可以被测试随时改写，用来模拟依赖挂掉又恢复。
type flakyCheck struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (c *flakyCheck) check(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.err
}

func (c *flakyCheck) set(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *flakyCheck) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// 启动没走完就不该收流量——这一层和依赖无关，是 ready 自己的职责。
func TestReadyIsFalseBeforeStartupCompletes(t *testing.T) {
	s := New()
	if s.Status().Ready {
		t.Fatal("a freshly constructed State must not report ready")
	}
	s.SetReady(true)
	if !s.Status().Ready {
		t.Fatal("ready after SetReady(true)")
	}
}

// 核心性质：依赖不可达时，即使启动早已完成，Ready 也必须变 false。
// 少了这一层，运行中挂掉的 Postgres 不会让副本被摘走。
func TestReadyTracksDependencyReachability(t *testing.T) {
	s := New()
	s.SetReady(true)

	if changed := s.SetDependenciesReachable(false); !changed {
		t.Fatal("the first write must report a change")
	}
	if s.Status().Ready {
		t.Fatal("Ready must be false while a dependency is unreachable")
	}
	if !s.Status().Live {
		t.Fatal("a dependency outage must not make the process look dead: 重启解决不了依赖挂掉")
	}

	s.SetDependenciesReachable(true)
	if !s.Status().Ready {
		t.Fatal("Ready must recover when the dependency comes back")
	}
}

// 依赖正常时再写一次同样的值不算变化，调用方据此只在翻转时打日志。
func TestSetDependenciesReachableReportsOnlyTransitions(t *testing.T) {
	s := New() // deps 初值为 true

	if !s.SetDependenciesReachable(false) {
		t.Fatal("true -> false must report a change")
	}
	if s.SetDependenciesReachable(false) {
		t.Fatal("false -> false must not report a change")
	}
	if !s.SetDependenciesReachable(true) {
		t.Fatal("false -> true must report a change")
	}
	if s.SetDependenciesReachable(true) {
		t.Fatal("true -> true must not report a change")
	}
}

// 依赖挂掉不能让它自己的探测循环退出——退出等于这个副本永远停在
// 「依赖是好的」这个结论上。而且 Run 提前返回会被生命周期判成
// 「worker exited during startup」直接拒绝启动。
func TestProbeKeepsRunningWhileDependencyIsDown(t *testing.T) {
	check := &flakyCheck{err: errors.New("postgres down")}
	s := New()
	p := NewProbe(s, 5*time.Millisecond, 20*time.Millisecond, Dependency{Name: "database", Check: check.check})

	// 先模拟「启动已完成」：要测的是运行中依赖挂掉，而不是启动期的判定。
	// 置位在起探针之前，Ready 会先变成 true，随后被探测结果压回 false。
	s.SetReady(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	waitFor(t, func() bool { return !s.Status().Ready })
	waitFor(t, func() bool { return check.count() >= 2 }) // 失败之后还在继续探

	select {
	case err := <-done:
		t.Fatalf("Run returned %v while the dependency was down; it must keep probing", err)
	default:
	}

	// 依赖恢复后探针要把就绪还回来。
	check.set(nil)
	waitFor(t, func() bool { return s.Status().Ready })

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// 启动时先探一次，而不是等第一个间隔。State 的 deps 初值是 true，
// 不先探一下的话，前一个间隔里探针会在依赖已经不可达时仍报就绪。
func TestProbeChecksImmediatelyOnStart(t *testing.T) {
	check := &flakyCheck{err: errors.New("postgres down")}
	s := New()
	// 间隔给得很长：如果只有 ticker，这段时间内什么都不会发生。
	p := NewProbe(s, time.Hour, 20*time.Millisecond, Dependency{Name: "database", Check: check.check})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	waitFor(t, func() bool { return check.count() >= 1 })
	waitFor(t, func() bool { return !s.Status().Ready })
}

// 检查函数自己卡住时，超时必须兜住：探测不能变成一个挂在那里的 goroutine，
// 更不能因为一次 Ping 没回来就永远不给结论。
func TestProbeBoundsEachCheck(t *testing.T) {
	blocked := make(chan struct{})
	s := New()
	p := NewProbe(s, 5*time.Millisecond, 20*time.Millisecond, Dependency{
		Name: "database",
		Check: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-blocked:
				return nil
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	waitFor(t, func() bool { return !s.Status().Ready })
	close(blocked)
}

// 没有依赖时探针无事可做，但不能因此判定为不就绪。
func TestProbeWithoutDependenciesStaysReady(t *testing.T) {
	s := New()
	s.SetReady(true)
	p := NewProbe(s, 5*time.Millisecond, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	time.Sleep(30 * time.Millisecond)
	if !s.Status().Ready {
		t.Fatal("a probe with nothing to check must not withdraw readiness")
	}
}

// Stop 是「这个副本判死」，live 与 ready 一起转 false。
//
// 两者必须一起转，因为两个探针喂的是编排器的两个不同决定：readyz 只管要不要继续往这台
// 发流量，livez 才管要不要把它回收重建。只转 ready 的副本不再接流量、却也永远不会被重启，
// 是一台占着资源不干活的僵尸。runtime 的 WorkerFailure 正是靠这一下让 worker 死掉的副本
// 被换掉。
func TestStopMarksTheInstanceDeadOnBothProbes(t *testing.T) {
	s := New()
	s.SetReady(true)
	if got := s.Status(); !got.Live || !got.Ready {
		t.Fatalf("Status() = %+v, want both live and ready", got)
	}
	s.Stop()
	if got := s.Status(); got.Live || got.Ready {
		t.Fatalf("Status() = %+v after Stop, want neither", got)
	}
}

// 判死之后，依赖探测的后续成功不能把它救回来。
//
// 这不是假想：探针自己就是一个 Worker，与死掉的那个各跑各的。别的 worker 崩掉、这个副本
// 已经判死之后，探针仍会按 15s 的节奏继续跑，Postgres 好好的于是每次都说「依赖可达」。
// 如果 Success 能翻回 ready，判死就会被自己进程里的另一根线程撤销——恢复路径当场失效。
func TestStopIsNotUndoneByALaterSuccessfulProbe(t *testing.T) {
	s := New()
	s.SetReady(true)
	s.Stop()
	// 与 Probe.observe 写的是同一个方法。
	s.SetDependenciesReachable(true)
	if got := s.Status(); got.Live || got.Ready {
		t.Fatalf("Status() = %+v, want an instance already stopped to stay dead", got)
	}
}
