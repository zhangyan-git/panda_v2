package casbin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
)

// 这两个假实现都被 Run 所在的协程写、被测试协程读，所以要各自加锁——
// 否则 -race 报的是测试自己的数据竞争，而掩盖了被测代码的结论。
type fakeReloader struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeReloader) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *fakeReloader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeClient 只够用来看「发了什么」和「推给订阅者什么」。
type fakeClient struct {
	mu        sync.Mutex
	published []published
	pubErr    error
	channels  []string
	messages  chan []byte
}

type published struct {
	channel string
	payload string
}

func newFakeClient() *fakeClient {
	return &fakeClient{messages: make(chan []byte, 8)}
}

func (f *fakeClient) Ping(context.Context) error { return nil }
func (f *fakeClient) Close() error               { return nil }

func (f *fakeClient) Publish(_ context.Context, channel string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, published{channel: channel, payload: string(payload)})
	return nil
}

func (f *fakeClient) Subscribe(_ context.Context, channel string) (cache.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = append(f.channels, channel)
	return &fakeSubscription{messages: f.messages}, nil
}

func (f *fakeClient) publishedMessages() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.published...)
}

func (f *fakeClient) subscribedChannels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.channels...)
}

type fakeSubscription struct{ messages chan []byte }

func (s *fakeSubscription) Channel() <-chan []byte { return s.messages }
func (s *fakeSubscription) Close() error           { return nil }

func TestReloadPublishesInstanceIDAfterLocalReload(t *testing.T) {
	reloader := &fakeReloader{}
	client := newFakeClient()
	b := NewBroadcaster(reloader, client, "instance-a")

	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if reloader.callCount() != 1 {
		t.Fatalf("reload calls = %d, want 1", reloader.callCount())
	}
	published := client.publishedMessages()
	if len(published) != 1 {
		t.Fatalf("published %d messages, want 1", len(published))
	}
	got := published[0]
	if got.channel != PolicyChangedChannel {
		t.Fatalf("channel = %q, want %q", got.channel, PolicyChangedChannel)
	}
	if got.payload != "instance-a" {
		t.Fatalf("payload = %q, want the instance id", got.payload)
	}
}

// 本地重载都失败时不能广播：别的副本会去读同一份（没变、或被改坏的）策略，
// 通知只会让它们白跑一趟，还会把「变更已生效」这件事说得比实际更肯定。
func TestReloadDoesNotPublishWhenLocalReloadFails(t *testing.T) {
	reloader := &fakeReloader{err: errors.New("boom")}
	client := newFakeClient()
	b := NewBroadcaster(reloader, client, "instance-a")

	if err := b.Reload(); err == nil {
		t.Fatal("expected error from failed local reload")
	}
	if published := client.publishedMessages(); len(published) != 0 {
		t.Fatalf("published %v, want nothing", published)
	}
}

// 广播失败要报出来：本实例已生效，其它副本还停在旧规则上，这正是调用方
// 应该重试的信号（与本地重载失败归到同一类错误）。
func TestReloadReportsBroadcastFailure(t *testing.T) {
	reloader := &fakeReloader{}
	client := newFakeClient()
	client.pubErr = errors.New("redis down")
	b := NewBroadcaster(reloader, client, "instance-a")

	err := b.Reload()
	if !errors.Is(err, ErrBroadcast) {
		t.Fatalf("error = %v, want ErrBroadcast", err)
	}
	if reloader.callCount() != 1 {
		t.Fatalf("reload calls = %d, want 1: 本地必须已经生效", reloader.callCount())
	}
}

// 自己发的那条不能再触发一次重载，否则每次变更都白白多读一遍全量策略。
func TestRunSkipsOwnBroadcast(t *testing.T) {
	reloader := &fakeReloader{}
	client := newFakeClient()
	b := NewBroadcaster(reloader, client, "instance-a")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, b)

	client.messages <- []byte("instance-a")
	client.messages <- []byte("instance-b")
	waitFor(t, func() bool { return reloader.callCount() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if reloader.callCount() != 1 {
		t.Fatalf("reload calls = %d, want 1 (only the remote instance's)", reloader.callCount())
	}
}

func TestRunSubscribesToPolicyChannel(t *testing.T) {
	client := newFakeClient()
	b := NewBroadcaster(&fakeReloader{}, client, "instance-a")

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(ctx, b)
	waitFor(t, func() bool { return len(client.subscribedChannels()) == 1 })
	cancel()
	<-done

	channels := client.subscribedChannels()
	if channels[0] != PolicyChangedChannel {
		t.Fatalf("subscribed to %q, want %q", channels[0], PolicyChangedChannel)
	}
}

// 没配 Redis 时 Reload 仍要成功（只在本实例生效），订阅循环则安静地等 ctx 结束。
func TestNoopClientKeepsReloadWorking(t *testing.T) {
	reloader := &fakeReloader{}
	b := NewBroadcaster(reloader, cache.Noop{}, "instance-a")

	if err := b.Reload(); err != nil {
		t.Fatalf("Reload with Noop cache = %v, want nil", err)
	}
	if reloader.callCount() != 1 {
		t.Fatalf("reload calls = %d, want 1", reloader.callCount())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(ctx, b)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// 广播只是快路径，会丢。没有广播时副本也必须自己纠回来——否则 Redis 抖动一次，
// 这个副本就永远停在旧策略上，而外部完全看不出来。
func TestRunResyncsWithoutAnyBroadcast(t *testing.T) {
	reloader := &fakeReloader{}
	client := newFakeClient()
	b := NewBroadcaster(reloader, client, "instance-a")
	b.resyncInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, b)

	// 一条广播都不发，只靠周期重载。
	waitFor(t, func() bool { return reloader.callCount() >= 2 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// 兜底重载失败不能让订阅循环退出——退出等于把这个副本永久钉在旧规则上，
// 恰恰是这条兜底要防的事。失败了下个周期还要再来。
func TestRunKeepsResyncingAfterReloadFailure(t *testing.T) {
	reloader := &fakeReloader{err: errors.New("postgres down")}
	client := newFakeClient()
	b := NewBroadcaster(reloader, client, "instance-a")
	b.resyncInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, b)

	waitFor(t, func() bool { return reloader.callCount() >= 3 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// 没配 Redis 时这条兜底才是主力：Noop 的订阅通道永远不产出消息，
// 所以副本之间根本没有任何广播可收，只有周期重载能让它们最终一致。
func TestNoopClientStillResyncsPeriodically(t *testing.T) {
	reloader := &fakeReloader{}
	b := NewBroadcaster(reloader, cache.Noop{}, "instance-a")
	b.resyncInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(ctx, b)

	waitFor(t, func() bool { return reloader.callCount() >= 2 })
	cancel()
	<-done
}

func runInBackground(ctx context.Context, b *Broadcaster) <-chan error {
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	return done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
