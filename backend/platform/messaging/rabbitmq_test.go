package messaging

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// newTestClient 建一个没有真实连接的客户端，只用来测 cur/ready 这段状态机。
// 不变式是这一对的核心：cur != nil 当且仅当 ready 已关闭。
func newTestClient() *rabbitClient {
	return &rabbitClient{closed: make(chan struct{}), ready: make(chan struct{})}
}

// 初值必须是「不可用」。反过来的话，NewRabbitMQ 之前或重连期间的 Publish
// 会拿到一个 nil channel 而不是一个明确的错误。
func TestRabbitClientStartsUnavailable(t *testing.T) {
	c := newTestClient()
	if _, err := c.publishChannel(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("publishChannel before connect = %v, want ErrUnavailable", err)
	}
	if _, err := c.consumerChannel(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("consumerChannel before connect = %v, want ErrUnavailable", err)
	}
}

func TestRabbitClientMarkUpReleasesWaiters(t *testing.T) {
	c := newTestClient()
	ready := make(chan error, 1)
	go func() { ready <- c.waitReady(context.Background()) }()

	// 上线之前等待者必须还挂着——立刻返回说明它没在等。
	select {
	case err := <-ready:
		t.Fatalf("waitReady returned %v before markUp", err)
	case <-time.After(20 * time.Millisecond):
	}

	if !c.markUp(&brokerConn{}) {
		t.Fatal("markUp on a fresh client must be accepted")
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("waitReady = %v, want nil once connected", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady did not return after markUp")
	}
}

// 断线把状态退回不可用，并让新的等待者挂住。这是 Publish 在重连期间快速失败
// （而不是卡在 outbox 租约上）的依据。
func TestRabbitClientMarkDownWithdrawsAvailability(t *testing.T) {
	c := newTestClient()
	c.markUp(&brokerConn{})

	c.markDown()
	if _, err := c.publishChannel(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("publishChannel while down = %v, want ErrUnavailable", err)
	}

	ready := make(chan error, 1)
	go func() { ready <- c.waitReady(context.Background()) }()
	select {
	case err := <-ready:
		t.Fatalf("waitReady returned %v while down", err)
	case <-time.After(20 * time.Millisecond):
	}

	if !c.markUp(&brokerConn{}) {
		t.Fatal("markUp after markDown must be accepted")
	}
	if err := <-ready; err != nil {
		t.Fatalf("waitReady = %v, want nil after reconnect", err)
	}
}

// 两条重连路径赛跑时，第二条必须被拒——否则 close(r.ready) 会二次关闭而 panic。
func TestRabbitClientRejectsSecondGeneration(t *testing.T) {
	c := newTestClient()
	if !c.markUp(&brokerConn{}) {
		t.Fatal("first markUp must be accepted")
	}
	if c.markUp(&brokerConn{}) {
		t.Fatal("second markUp must be rejected while one is already up")
	}
}

// 关闭之后不能再被采纳：否则 supervise 会在停机路径上重新连一次 broker。
func TestRabbitClientMarkUpRejectedAfterClose(t *testing.T) {
	c := newTestClient()
	close(c.closed)
	if c.markUp(&brokerConn{}) {
		t.Fatal("markUp after close must be rejected")
	}
}

// 停机必须能叫醒挂在 waitReady 上的消费者，否则它会一直阻塞到 ctx 超时，
// 关停时间被生命周期兜底吞掉。
func TestRabbitClientDetachReleasesWaiters(t *testing.T) {
	c := newTestClient()
	ready := make(chan error, 1)
	go func() { ready <- c.waitReady(context.Background()) }()

	if cur := c.detach(); cur != nil {
		t.Fatalf("detach on an unwired client = %v, want nil", cur)
	}
	select {
	case err := <-ready:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("waitReady after close = %v, want ErrUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady did not return after the client was closed")
	}
}

// waitRetry 是重订阅路径上防忙等的那道闸：连接一直可用时它也必须等满退避，
// 否则「队列不存在」会变成每秒几千次的失败重试。
func TestRabbitClientWaitRetryHonorsDelay(t *testing.T) {
	c := newTestClient()
	c.markUp(&brokerConn{})

	start := time.Now()
	if err := c.waitRetry(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("waitRetry = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("waitRetry returned after %v, want at least the 50ms backoff", elapsed)
	}
}

func TestRabbitClientWaitRetryRespectsContext(t *testing.T) {
	// 刻意不 markUp：连接可用时 waitReady 会立刻返回，测不到 ctx 这条路径。
	c := newTestClient()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitRetry = %v, want context.Canceled", err)
	}
	if err := c.waitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady = %v, want context.Canceled", err)
	}
}

// 队列没配就不该进入消费循环：那是配置错误，退避重试只会把它藏起来。
func TestRabbitConsumeRequiresQueue(t *testing.T) {
	c := newTestClient()
	c.markUp(&brokerConn{})
	err := c.Consume(context.Background(), func(context.Context, Envelope) error { return nil })
	if err == nil {
		t.Fatal("Consume without a queue must fail immediately")
	}
}

// brokerURL 返回集成测试要用的 broker 地址。没配就跳过——与 bindingTestPool
// 用 TEST_DATABASE_URL 的方式一致：需要真实依赖的测试不该在没依赖时假装通过。
func brokerURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL is not set; skipping the RabbitMQ integration test")
	}
	return url
}

// cutConnection 模拟一次 broker 重启：从客户端这侧掐掉底层连接。
// 走的是和 broker 掉线完全相同的那条路径（NotifyClose → supervise 重连）。
func cutConnection(t *testing.T, c *rabbitClient) {
	t.Helper()
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur == nil {
		t.Fatal("no connection to cut")
	}
	_ = cur.conn.Close()
}

// 断线之后发布要自动恢复。修之前这里是永久停摆：连接没了就再也没人建。
func TestRabbitPublishRecoversAfterConnectionLoss(t *testing.T) {
	cfg := integrationConfig(t, "publish-recovery")
	client, _, closer, err := NewRabbitMQ(cfg)
	if err != nil {
		t.Fatalf("NewRabbitMQ: %v", err)
	}
	defer func() { _ = closer.Close() }()
	rc := client.(*rabbitClient)

	ctx := context.Background()
	if err := client.Publish(ctx, Envelope{EventType: "verify.event", Payload: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("initial publish: %v", err)
	}

	cutConnection(t, rc)

	// 重连是异步的，所以这里有窗口期：期间允许失败，但要看到它自己恢复。
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := client.Publish(ctx, Envelope{EventType: "verify.event", Payload: []byte(`{"n":2}`)})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("publishing never recovered after the connection was cut: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 消费侧断线后要自己重新订阅并继续收消息。修之前这个 goroutine 直接返回，
// 生命周期只会在停机时看它一眼——运行中退出完全静默。
func TestRabbitConsumeResubscribesAfterConnectionLoss(t *testing.T) {
	cfg := integrationConfig(t, "consume-recovery")
	client, consumer, closer, err := NewRabbitMQ(cfg)
	if err != nil {
		t.Fatalf("NewRabbitMQ: %v", err)
	}
	defer func() { _ = closer.Close() }()
	rc := client.(*rabbitClient)

	received := make(chan string, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- consumer.Consume(ctx, func(_ context.Context, e Envelope) error {
			received <- string(e.Payload)
			return nil
		})
	}()

	publishAndAwait := func(body string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if err := client.Publish(ctx, Envelope{EventType: "verify.event", Payload: []byte(body)}); err == nil {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("could not publish %q: %v", body, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		deadline = time.Now().Add(20 * time.Second)
		for {
			select {
			case got := <-received:
				if got == body {
					return
				}
			case <-time.After(20 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("never received %q after the connection was cut", body)
				}
			}
		}
	}

	publishAndAwait("before-cut")

	cutConnection(t, rc)

	// 消费循环必须还活着，并且重连后重新订阅上。
	publishAndAwait("after-cut")

	select {
	case err := <-done:
		t.Fatalf("Consume returned %v; it must survive a connection loss", err)
	default:
	}
}

// 持久化消息要能扛住 broker 重启。这条测不了「真的重启」（那要动共享容器），
// 但它能钉住我们这边的义务：投出去的消息带 Persistent。
func TestRabbitPublishMarksMessagesPersistent(t *testing.T) {
	cfg := integrationConfig(t, "persistence")
	client, _, closer, err := NewRabbitMQ(cfg)
	if err != nil {
		t.Fatalf("NewRabbitMQ: %v", err)
	}
	defer func() { _ = closer.Close() }()

	// 用另一个 channel 直接读投递属性：消费接口会把 DeliveryMode 丢掉，
	// 而它正是要断言的字段。
	rc := client.(*rabbitClient)
	ch, err := rc.cur.conn.Channel()
	if err != nil {
		t.Fatalf("open inspect channel: %v", err)
	}
	defer func() { _ = ch.Close() }()
	deliveries, err := ch.Consume(cfg.Queue, "", false, true, false, false, nil)
	if err != nil {
		t.Fatalf("consume for inspection: %v", err)
	}

	if err := client.Publish(context.Background(), Envelope{EventType: "verify.event", Payload: []byte(`{"persist":true}`)}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case d := <-deliveries:
		if d.DeliveryMode != amqp.Persistent {
			t.Fatalf("DeliveryMode = %d, want %d (Persistent)", d.DeliveryMode, amqp.Persistent)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("published message never arrived")
	}
}

// integrationConfig 建一套隔离的交换机/队列名，避免和 dev 栈里已有的拓扑撞车。
func integrationConfig(t *testing.T, label string) RabbitConfig {
	t.Helper()
	url := brokerURL(t)
	unique := label + "-" + time.Now().Format("150405.000000")
	return RabbitConfig{
		URL: url, Exchange: "verify." + unique, ExchangeType: "topic",
		Queue: "verify." + unique, RoutingKey: "verify.event",
		DLX: "verify." + unique + ".dlx", DLQ: "verify." + unique + ".dlq", RetryLimit: 3,
	}
}
