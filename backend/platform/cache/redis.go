package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Client is the lifecycle contract used by platform components.
type Client interface {
	Ping(context.Context) error
	Close() error
	// Publish 往频道投一条消息。Noop 接受并丢弃：调用方不必区分「没配 Redis」
	// 和「配了但这条没人收」，两种情况在语义上都是「本进程之外没有别的订阅者」。
	Publish(ctx context.Context, channel string, payload []byte) error
	// Subscribe 订阅一个频道，Close 之前持续投递。返回即代表订阅已经建立，
	// 不是「正在建立」——见 Redis.Subscribe 里的说明。
	Subscribe(ctx context.Context, channel string) (Subscription, error)
}

// Subscription 是一条频道订阅。Channel 只产出消息负载（频道名调用方本来就知道），
// 订阅关闭后该 channel 关闭。
type Subscription interface {
	Channel() <-chan []byte
	Close() error
}

// Noop keeps services runnable when REDIS_ADDR is not configured.
type Noop struct{}

func (Noop) Ping(context.Context) error { return nil }
func (Noop) Close() error               { return nil }

// Publish 无事可做：没有 Redis 也就没有跨进程的订阅者。
func (Noop) Publish(context.Context, string, []byte) error { return nil }

// Subscribe 返回一个永不产出的订阅。调用方的订阅循环因此可以照常阻塞在
// select 上，等 ctx 结束——不必为「没配 Redis」写一条分支。
func (Noop) Subscribe(context.Context, string) (Subscription, error) {
	return &noopSubscription{messages: make(chan []byte)}, nil
}

type noopSubscription struct{ messages chan []byte }

func (s *noopSubscription) Channel() <-chan []byte { return s.messages }
func (s *noopSubscription) Close() error           { return nil }

// NewFromEnv creates a Redis client using REDIS_ADDR, REDIS_PASSWORD, and
// REDIS_DB. An empty REDIS_ADDR returns a no-op adapter.
func NewFromEnv(ctx context.Context) (Client, error) {
	value := os.Getenv("REDIS_DB")
	db := 0
	if value != "" {
		var err error
		db, err = strconv.Atoi(value)
		if err != nil {
			return nil, errors.New("invalid REDIS_DB")
		}
	}
	return New(ctx, Options{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       db,
	})
}

type Options struct {
	Addr     string
	Password string
	DB       int
}

// New creates a Redis 7 compatible go-redis v9 client. It does not ping during
// construction; callers can explicitly Ping as part of startup.
func New(_ context.Context, options Options) (Client, error) {
	if options.Addr == "" {
		return Noop{}, nil
	}
	if options.DB < 0 {
		return nil, errors.New("redis database must not be negative")
	}
	return &Redis{client: redis.NewClient(&redis.Options{
		Addr:     options.Addr,
		Password: options.Password,
		DB:       options.DB,
	})}, nil
}

// Redis adapts go-redis to the platform lifecycle contract.
type Redis struct{ client *redis.Client }

func (r *Redis) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("redis client is nil")
	}
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

func (r *Redis) Publish(ctx context.Context, channel string, payload []byte) error {
	if r == nil || r.client == nil {
		return errors.New("redis client is nil")
	}
	return r.client.Publish(ctx, channel, payload).Err()
}

func (r *Redis) Subscribe(ctx context.Context, channel string) (Subscription, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("redis client is nil")
	}
	sub := r.client.Subscribe(ctx, channel)
	// Receive 会一直等到订阅确认才返回。不等它，紧随其后发出的那条消息就可能
	// 落在「订阅登记完成」之前而丢掉，而调用方以为已经订上了——这种丢法只在
	// 启动瞬间出现，事后极难复现。
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, fmt.Errorf("redis subscribe %s: %w", channel, err)
	}
	return newRedisSubscription(sub), nil
}

// redisSubscription 把 go-redis 的消息类型转成本包的 []byte，免得平台接口
// 漏出 go-redis 的类型。转发协程只为这一件事存在。
type redisSubscription struct {
	sub      *redis.PubSub
	messages chan []byte
	done     chan struct{}
	once     sync.Once
}

func newRedisSubscription(sub *redis.PubSub) *redisSubscription {
	s := &redisSubscription{sub: sub, messages: make(chan []byte, 16), done: make(chan struct{})}
	go s.forward()
	return s
}

// forward 转投消息。go-redis 的 Channel 只在 Close 时关闭，断线重连由它自己
// 在内部完成，所以这里的 for range 不会因为网络抖动提前结束。
func (s *redisSubscription) forward() {
	defer close(s.messages)
	for msg := range s.sub.Channel() {
		select {
		case s.messages <- []byte(msg.Payload):
		case <-s.done:
			return
		}
	}
}

func (s *redisSubscription) Channel() <-chan []byte { return s.messages }

// Close 幂等：done 只关一次，go-redis 的 PubSub.Close 本身也允许重复调用。
func (s *redisSubscription) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.sub.Close()
}

// ScriptRunner 暴露 Redis 的 EVAL，给「读-改-写必须原子」的组件用
// （platform/ratelimit 的滑动窗口就是）。它不属于「缓存」这件事，所以不进 Client：
// Noop 无法实现 EVAL，硬塞进去只会逼出一个假装成功的实现。
type ScriptRunner interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

func (r *Redis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("redis client is nil")
	}
	return r.client.Eval(ctx, script, keys, args...).Result()
}

var (
	_ Client       = (*Redis)(nil)
	_ ScriptRunner = (*Redis)(nil)
)
