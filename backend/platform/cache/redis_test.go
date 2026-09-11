package cache

import (
	"context"
	"os"
	"testing"
	"time"
)

// Noop 必须满足整个 Client 契约：服务在 REDIS_ADDR 为空时照常启动，靠的就是它。
var _ Client = Noop{}

func TestNoopPublishIsSilent(t *testing.T) {
	if err := (Noop{}).Publish(context.Background(), "any", []byte("x")); err != nil {
		t.Fatalf("Noop.Publish = %v, want nil", err)
	}
}

func TestNoopSubscribeNeverDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := (Noop{}).Subscribe(ctx, "any")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	select {
	case msg, ok := <-sub.Channel():
		t.Fatalf("Noop subscription delivered %q (open=%v), want silence", msg, ok)
	case <-time.After(50 * time.Millisecond):
	}
	// Close 之后依旧安静：没有 Redis 就没有订阅，关掉一条本来就不投递的订阅不该 panic。
	if err := sub.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

func TestRedisNilClientFailsClosed(t *testing.T) {
	var r *Redis
	if err := r.Publish(context.Background(), "any", nil); err == nil {
		t.Fatal("expected error from nil redis client")
	}
	if _, err := r.Subscribe(context.Background(), "any"); err == nil {
		t.Fatal("expected error from nil redis client")
	}
	if _, err := r.Eval(context.Background(), "return 1", nil); err == nil {
		t.Fatal("expected error from nil redis client")
	}
}

// 真 Redis 上的收发往返。用 REDIS_TEST_ADDR 显式打开，未配置就跳过——
// 跳过不算通过，阶段 5 的验收会在真实 Redis 上跑一遍。
func TestRedisPublishSubscribeRoundTrip(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := New(ctx, Options{Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	channel := "panda:test:roundtrip"
	sub, err := client.Subscribe(ctx, channel)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	// Subscribe 返回即代表订阅已建立，所以这里不需要 sleep 等订阅生效。
	if err := client.Publish(ctx, channel, []byte("payload")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case msg := <-sub.Channel():
		if string(msg) != "payload" {
			t.Fatalf("got %q, want payload", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message within 5s")
	}

	// 关掉订阅后 channel 关闭，订阅循环据此退出。
	if err := sub.Close(); err != nil {
		t.Fatalf("close subscription: %v", err)
	}
	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Fatal("expected the message channel to close after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message channel stayed open after Close")
	}
}

func TestNewFromEnvRejectsInvalidDB(t *testing.T) {
	t.Setenv("REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("REDIS_DB", "invalid")
	if _, err := NewFromEnv(context.Background()); err == nil {
		t.Fatal("expected invalid REDIS_DB error")
	}
}

func TestNewFromEnvEmptyDBDefaultsToZero(t *testing.T) {
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_DB", "")
	client, err := NewFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(Noop); !ok {
		t.Fatalf("client type = %T, want Noop", client)
	}
}
