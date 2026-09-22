package messaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// deadLetters 是死信计数器的进程级单例。
//
// 用 OnceValue 而不是给 rabbitClient 加个字段：指标名是全局的，一个进程里的多个
// client（测试、或者发布与消费各一个）应当落在同一个 instrument 上。取的是 otel 的
// 全局 meter，所以不配 OTLP 端点时它就是个空实现——dev 不启 collector 也不报错。
var deadLetters = sync.OnceValue(func() metric.Int64Counter {
	return observability.Counter(observability.DeadLetterName, "messages rejected into the dead-letter queue after exhausting retries")
})

// RabbitConfig describes the exchange and queue used by RabbitMQ.
type RabbitConfig struct {
	URL          string
	Exchange     string
	ExchangeType string
	Queue        string
	RoutingKey   string
	DLX          string
	DLQ          string
	RetryLimit   int
}

// 失败消息的去向，默认是非零的。
//
// RetryLimit 为 0 时第一次处理失败就走 Reject(false)，消息既不重试也不进
// 死信——从系统里直接消失。这是最不该有的默认行为：运维只能从「少了一条」
// 察觉，而那时已经没法补。配上死信拓扑之后，同样的失败至少留下一条可查、
// 可重放的记录。
//
// 想关掉重试就显式设 RABBITMQ_RETRY_LIMIT=0：那时队列仍然带着死信交换机，
// 被拒绝的消息是进 DLQ 而不是被丢弃。
const (
	defaultRetryLimit = 5
	defaultDLX        = "panda.events.dlx"
	defaultDLQ        = "panda.events.dlq"
)

func RabbitConfigFromEnv() RabbitConfig {
	limit, err := strconv.Atoi(os.Getenv("RABBITMQ_RETRY_LIMIT"))
	if err != nil {
		// 没配或者配坏了都落到默认值。配坏了退回 0 会让失败消息静默消失，
		// 而这是本文件最想避免的那个结果。
		limit = defaultRetryLimit
	}
	if limit < 0 {
		limit = 0
	}
	return RabbitConfig{
		URL: os.Getenv("RABBITMQ_URL"), Exchange: envOr("RABBITMQ_EXCHANGE", "panda.events"),
		ExchangeType: envOr("RABBITMQ_EXCHANGE_TYPE", "topic"), Queue: os.Getenv("RABBITMQ_QUEUE"),
		RoutingKey: os.Getenv("RABBITMQ_ROUTING_KEY"), DLX: envOr("RABBITMQ_DLX", defaultDLX),
		DLQ: envOr("RABBITMQ_DLQ", defaultDLQ), RetryLimit: limit,
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// routeKey picks the routing key for one message. The event type *is* the
// routing key: the exchange is a topic, so each event type fans out on its own
// and a consumer can subscribe to a subset without a second client. RoutingKey
// is only the fallback for a message published without a type.
func (c RabbitConfig) routeKey(eventType string) string {
	if eventType != "" {
		return eventType
	}
	return c.RoutingKey
}

// bindKeys are the patterns this service's queue subscribes to, split on commas.
// With no configured key the queue takes everything: a topic exchange delivers
// only what a binding matches, so an empty binding would strand every event.
//
// 「strand 了也不会静默」这句原先写在这里，是错的，实测推翻（2026-09-14，本地
// broker）：路由键没有任何绑定时，broker 确实回了 basic.return / NO_ROUTE，发布
// 也是 mandatory+confirm 的，但**退回被丢掉、Publish 报成功**。原因是确认和退回在
// 客户端是两条路：退回经 NotifyReturn 进 buffered channel，还要过一个
// dispatchReturns goroutine 才派发到等的人；确认直接叫醒 waitConfirmation，它的
// defer 先把待派发登记删了，派发时查到 0 个等待者就扔。5 次里 5 次如此，不是偶发。
//
// 实际语义因此是：**消息投到交易所即算成功，没人订阅就等于没人要，安静丢弃、不重试**
// —— 对扇出型事件这多半正是想要的（没有消费者时无限重试更糟），但要有别的意思就得
// 另外做。判断有没有人收，看交易所的绑定，别指望 Publish 回错。
//
// 多个键用逗号分隔（`RABBITMQ_ROUTING_KEY=order.completed,order.after_sale.applied`）：
// 一个服务往往同时关心几件**具体**的事，而 `order.*` 这种放开的写法会把整个域的每一条
// 都收进来——收进来只为 ack 掉。收窄到列出来的这几个，队列里剩下的每一条都是要处理的，
// 「队列积压」才重新等于「消费跟不上了」。
//
// 逗号而不是别的分隔符：它不出现在 AMQP 的 routing key 里（keys 以点分段，段内是
// 字母数字连字符下划线），所以切分没有歧义。
func (c RabbitConfig) bindKeys() []string {
	keys := make([]string, 0, 1)
	for _, key := range strings.Split(c.RoutingKey, ",") {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return []string{"#"}
	}
	return keys
}

// deadLetterKey 是死信投出去时**带**的那个 routing key，也是 DLQ 在 DLX 上的那一条
// 绑定。两处必须取同一个值：不一样的话死信投出去就没有家（topic 交换机只投有绑定匹配
// 的那一条），而这件事不会有任何报错——失败的消息只是不见了。
//
// 取的是配置原文，**空就是空**，不跟着 bindKeys 退到 `#`：`#` 在 topic 交换机上匹配
// 每一条（上面 bindKeys 的注释就是靠这个「全收」的），而一个「配了队列名、没配路由键」
// 的服务若往 DLX 上绑一把 `#`，它就把**别家**的死信也捞进自己的 DLQ——别人的失败消息
// 从此查不到。空绑定只认路由键为空的那一条，也就是只有它自己那台队列发出来的死信。
// 实测（2026-09-15，本地 broker）：空的 x-dead-letter-routing-key 是被当真的，死信带着
// 空 routing key 投出来，落到空绑定上——不是「没设」退回原始路由键。
//
// 原文而不是切分后的某一个键：队列参数一旦声明就不能改，而原文是稳定的、与键的条数
// 无关。那个 `*`（如果配的是通配）在这一串里是个普通字符——它在词的中间，topic 交换机
// 的通配只认整段，所以它既不会被当匹配到别处，也不会漏掉自己。
func (c RabbitConfig) deadLetterKey() string { return c.RoutingKey }

func (c RabbitConfig) validate() error {
	if c.URL == "" { // Noop is deliberately valid without any Rabbit settings.
		return nil
	}
	if c.Exchange == "" {
		return fmt.Errorf("messaging: RabbitMQ exchange is required")
	}
	if c.ExchangeType == "" {
		return fmt.Errorf("messaging: RabbitMQ exchange type is required")
	}
	if c.RetryLimit < 0 {
		return fmt.Errorf("messaging: RabbitMQ retry limit cannot be negative")
	}
	if c.DLQ != "" && c.DLX == "" {
		return fmt.Errorf("messaging: RabbitMQ DLX is required when DLQ is configured")
	}
	if c.RetryLimit > 0 && (c.DLX == "" || c.DLQ == "") {
		return fmt.Errorf("messaging: RabbitMQ DLX and DLQ are required when retries are enabled")
	}
	return nil
}

// 重连退避。第一次很快——多数断连是瞬时抖动，等半分钟毫无必要；上限压在
// 30s，比这更久恢复时间就没有意义了，而 broker 没起来时重试本身没有成本。
const (
	reconnectInitialDelay = 500 * time.Millisecond
	reconnectMaxDelay     = 30 * time.Second
)

// ErrUnavailable 表示「当前这一代连接不可用」：正在重连，或者刚刚断掉。
// 调用方该做的是等下一代，而不是把它当成配置错误。
var ErrUnavailable = errors.New("messaging: rabbitmq connection unavailable")

// NewRabbitMQ returns Noop when RABBITMQ_URL is not configured.
func NewRabbitMQ(cfg RabbitConfig) (Publisher, Consumer, io.Closer, error) {
	if cfg.URL == "" {
		return Noop{}, Noop{}, nopCloser{}, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, nil, err
	}
	client := &rabbitClient{
		cfg: cfg, closed: make(chan struct{}), ready: make(chan struct{}),
		pendingReturns: make(map[string][]chan amqp.Return),
	}
	// 初次连接和重连走同一条路径。重连能用上的东西，启动时就已经被验证过。
	conn, err := client.dial()
	if err != nil {
		return nil, nil, nil, err
	}
	if !client.markUp(conn) {
		_ = conn.conn.Close()
		return nil, nil, nil, fmt.Errorf("messaging: client closed during startup")
	}
	// 断线由 supervise 兜住，而不是靠调用方发现：生命周期只在启动和停机时
	// 看消费者，运行中它退出是静默的——消息堆在队列里没人取，/readyz 照样 200。
	go client.supervise()
	return client, client, client, nil
}

func declareTopology(ch *amqp.Channel, cfg RabbitConfig) error {
	if cfg.DLX != "" {
		if err := ch.ExchangeDeclare(cfg.DLX, "topic", true, false, false, false, nil); err != nil {
			return err
		}
	}
	keys := cfg.bindKeys()
	args := amqp.Table{}
	if cfg.DLX != "" {
		args["x-dead-letter-exchange"] = cfg.DLX
		args["x-dead-letter-routing-key"] = cfg.deadLetterKey()
	}
	if _, err := ch.QueueDeclare(cfg.Queue, true, false, false, false, args); err != nil {
		return err
	}
	for _, key := range keys {
		if err := ch.QueueBind(cfg.Queue, key, cfg.Exchange, false, nil); err != nil {
			return err
		}
	}
	if cfg.DLQ != "" {
		if _, err := ch.QueueDeclare(cfg.DLQ, true, false, false, false, nil); err != nil {
			return err
		}
		if err := ch.QueueBind(cfg.DLQ, cfg.deadLetterKey(), cfg.DLX, false, nil); err != nil {
			return err
		}
	}
	return nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// brokerConn 是「一代」连接：连接本身、发布 channel、消费 channel 一起换。
//
// 不拆开单独补某个 channel，是因为它们只在同一次断连里一起失效——连接没了
// channel 必然也没了。分开修只会多出「连接是新的、channel 还是旧的」这种
// 中间状态，而每一种中间状态都要单独判一遍。
type brokerConn struct {
	conn      *amqp.Connection
	pubCh     *amqp.Channel
	consumeCh *amqp.Channel
	closed    chan *amqp.Error
}

type rabbitClient struct {
	cfg RabbitConfig

	// mu 保护 cur 和 ready 这一对。两者的不变式是：cur != nil 当且仅当
	// ready 已关闭。所有读改写都走 markUp / markDown，别处不要单独动它们。
	mu    sync.Mutex
	cur   *brokerConn
	ready chan struct{}

	closeOnce sync.Once
	closeErr  error
	// returnMu 保护 pendingReturns。它跨连接代际存在：等确认的那个调用方
	// 自己会在退出时清理，和连接换不换无关。
	returnMu       sync.Mutex
	pendingReturns map[string][]chan amqp.Return
	// closed 表示客户端被显式关掉了。它和断线是两回事：断线要重连，关闭要停。
	closed chan struct{}
}

// dial 建一代新连接：拨号、开 channel、声明拓扑、装上回调。
func (r *rabbitClient) dial() (*brokerConn, error) {
	conn, err := amqp.Dial(r.cfg.URL)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*brokerConn, error) {
		_ = conn.Close()
		return nil, err
	}
	pubCh, err := conn.Channel()
	if err != nil {
		return fail(err)
	}
	if err := pubCh.Confirm(false); err != nil {
		return fail(fmt.Errorf("messaging: enable publisher confirms: %w", err))
	}
	if err := pubCh.ExchangeDeclare(r.cfg.Exchange, r.cfg.ExchangeType, true, false, false, false, nil); err != nil {
		return fail(err)
	}
	// returns 必须是每一代新建的，不能共用：amqp091 在 channel 关闭时会
	// **关掉**它收到的这个 channel，共用一个的话，下一代再登记上去，等到
	// 那一代关闭时就会 close 一个已经关过的 channel，直接 panic。
	// 换成新的之后，旧的那条被关掉，对应的 dispatchReturns 也就自然退出了。
	returns := make(chan amqp.Return, 16)
	pubCh.NotifyReturn(returns)
	go r.dispatchReturns(returns)
	consumeCh := pubCh
	if r.cfg.Queue != "" {
		consumeCh, err = conn.Channel()
		if err != nil {
			return fail(err)
		}
		if err := declareTopology(consumeCh, r.cfg); err != nil {
			_ = consumeCh.Close()
			return fail(err)
		}
	}
	// 缓冲 1：不缓冲的话，连接关闭时那个发送会卡住 amqp091 的读循环。
	return &brokerConn{conn: conn, pubCh: pubCh, consumeCh: consumeCh, closed: conn.NotifyClose(make(chan *amqp.Error, 1))}, nil
}

// markUp 换上新的一代并放行所有等待者，返回是否被采纳。返回 false 时调用方
// 负责关掉传进来的连接（客户端已经关了，或者已有可用连接）。
//
// 不在这里关，是为了让这段状态机能脱离真实连接被测到——它的正确性只跟
// cur/ready 这一对有关，跟 socket 无关。
func (r *rabbitClient) markUp(conn *brokerConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return false
	}
	select {
	case <-r.closed:
		return false
	default:
	}
	r.cur = conn
	close(r.ready)
	return true
}

// markDown 摘掉当前这一代：Publish 立刻失败（relay 会重试 outbox 行），
// Consume 挂到新的 ready 上等重连。
func (r *rabbitClient) markDown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.cur = nil
	r.ready = make(chan struct{})
}

// supervise 在连接断掉后就地重连，直到客户端被关闭。
//
// 没有它，broker 重启一次就是永久停摆：发布侧每条都失败（outbox 一直重试，
// 至少还吵），消费侧那个 goroutine 直接返回、没有任何人重启它，而生命周期
// 只在启动和停机时看它——运行中退出是完全静默的。
func (r *rabbitClient) supervise() {
	for {
		r.mu.Lock()
		cur := r.cur
		r.mu.Unlock()
		if cur != nil {
			select {
			case <-cur.closed:
			case <-r.closed:
				return
			}
			r.markDown()
			slog.Warn("messaging: rabbitmq connection lost; reconnecting")
		}
		if !r.reconnect() {
			return
		}
	}
}

// reconnect 按退避重试直到连上或客户端被关闭，返回是否连上了。
func (r *rabbitClient) reconnect() bool {
	delay := reconnectInitialDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-r.closed:
			timer.Stop()
			return false
		}
		conn, err := r.dial()
		if err != nil {
			slog.Error("messaging: rabbitmq reconnect failed", "error", err, "retry_in", delay)
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}
		if !r.markUp(conn) {
			_ = conn.conn.Close()
			return false
		}
		slog.Info("messaging: rabbitmq reconnected")
		return true
	}
}

// publishChannel 返回当前这一代的发布 channel。重连期间返回错误而不是阻塞：
// 调用方是 outbox relay，它本来就会重试，卡在这里只会把租约耗到超时。
func (r *rabbitClient) publishChannel() (*amqp.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return nil, ErrUnavailable
	}
	return r.cur.pubCh, nil
}

func (r *rabbitClient) consumerChannel() (*amqp.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return nil, ErrUnavailable
	}
	return r.cur.consumeCh, nil
}

// waitReady 等当前这一代可用。
func (r *rabbitClient) waitReady(ctx context.Context) error {
	r.mu.Lock()
	ready := r.ready
	r.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.closed:
		return ErrUnavailable
	}
}

// waitRetry 先退避一段再等连接可用。退避不能省：队列不存在这类配置错误会让
// 订阅立刻失败，不退避就是忙等。
func (r *rabbitClient) waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.closed:
		return ErrUnavailable
	}
	return r.waitReady(ctx)
}

// detach 关掉客户端并摘走当前连接，让 supervise 和所有等待者退出。
func (r *rabbitClient) detach() *brokerConn {
	close(r.closed)
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur
	r.cur = nil
	return cur
}

func (r *rabbitClient) Close() error {
	return r.close(func() error {
		cur := r.detach()
		if cur == nil {
			return nil
		}
		if cur.consumeCh != cur.pubCh {
			_ = cur.consumeCh.Close()
		}
		_ = cur.pubCh.Close()
		return cur.conn.Close()
	})
}

// CloseContext force-closes the connection when the context has a deadline.
// Closing the connection directly also closes its channels and consumers.
func (r *rabbitClient) CloseContext(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return r.Close()
	}
	return r.close(func() error {
		cur := r.detach()
		if cur == nil {
			// 正在重连，没有连接可关；detach 已经把 supervise 停掉了。
			return nil
		}
		return cur.conn.CloseDeadline(deadline)
	})
}

func (r *rabbitClient) close(closeConn func() error) error {
	r.closeOnce.Do(func() {
		r.closeErr = closeConn()
	})
	return r.closeErr
}

func (r *rabbitClient) Publish(ctx context.Context, e Envelope) error {
	if e.EventID == "" {
		e.EventID = uuid.NewString()
	}
	ch, err := r.publishChannel()
	if err != nil {
		return err
	}
	confirmation, err := ch.PublishWithDeferredConfirm(
		r.cfg.Exchange, r.cfg.routeKey(e.EventType), true, false, amqp.Publishing{
			ContentType: "application/json", MessageId: e.EventID, Type: e.EventType,
			Headers: amqp.Table{"event_version": e.EventVersion, "trace_id": e.TraceID}, Body: e.Payload,
			// 交换机、队列都是 durable 的，但消息自己不持久的话 broker 重启就没了，
			// 数据卷救不回来。这一行是「durable」这个词真正兑现的地方。
			DeliveryMode: amqp.Persistent,
		},
	)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return fmt.Errorf("messaging: publisher confirmation unavailable")
	}
	acked, err := r.waitConfirmation(ctx, confirmation, e.EventID)
	if err != nil {
		return err
	}
	if !acked {
		return fmt.Errorf("messaging: publisher confirmation rejected")
	}
	return nil
}

// Consume 订阅并一直消费，跨连接重建。
//
// 它只在 ctx 结束（或客户端被关掉）时返回。把内层错误交出去是不行的：
// 生命周期只在启动和停机时看这个 goroutine，运行中它退出没有任何人会发现
// ——消息堆在队列里没人取，而 /readyz 照样 200。
func (r *rabbitClient) Consume(ctx context.Context, handler Handler) error {
	if r.cfg.Queue == "" {
		return fmt.Errorf("messaging: RabbitMQ queue is required for consume")
	}
	for {
		err := r.consumeOnce(ctx, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// consumeOnce 只可能因为这一代连接不能再用了而返回。退避是必须的：
		// 队列不存在这类配置错误会让订阅立刻失败，不退避就是忙等。
		slog.Warn("messaging: consumer loop ended; resubscribing", "error", err)
		if err := r.waitRetry(ctx, reconnectInitialDelay); err != nil {
			return err
		}
	}
}

// consumeOnce 订阅并消费，直到这一代连接失效或 ctx 结束。它不负责重连——
// 重连是 supervise 的事，这里只等下一代。
func (r *rabbitClient) consumeOnce(ctx context.Context, handler Handler) error {
	ch, err := r.consumerChannel()
	if err != nil {
		return err
	}
	tag := "panda-consumer-" + uuid.NewString()
	deliveries, err := ch.Consume(r.cfg.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	// 用订阅时拿到的那个 channel 取消，而不是再去读一次当前连接：这中间可能
	// 已经换了一代，Cancel 到新 channel 上会留下一个没人管的旧消费者。
	defer func() { _ = ch.Cancel(tag, false) }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("%w: delivery channel closed", ErrUnavailable)
			}
			envelope := envelopeFromDelivery(d)
			attempts := retryCount(d.Headers)
			if err := handler(ctx, envelope); err == nil {
				if ackErr := d.Ack(false); ackErr != nil {
					return ackErr
				}
			} else if attempts < r.cfg.RetryLimit {
				// 处理失败的**值**此前是直接丢掉的——重投几次、为什么失败，日志里一个字
				// 都没有。重投有上限（RetryLimit），所以这不是无限循环，但「这条消息
				// 为什么被重投了五次」当时只能靠猜。
				slog.Warn("messaging: consumer handler failed; retrying",
					"event", envelope.EventID, "type", envelope.EventType,
					"attempt", attempts+1, "retry_limit", r.cfg.RetryLimit, "error", err)
				if err := r.republishRetry(ctx, d); err != nil {
					return err
				}
				if err := d.Ack(false); err != nil {
					return err
				}
			} else {
				// 这一条走到头了。它被拒进死信队列，而那个队列**没有消费者**：消息进去
				// 就停在那里，直到有人主动去捞。所以下面这条日志与这个计数是它唯一的
				// 痕迹，也是「该去 DLQ 里看看了」这件事唯一会被触发的时机。
				deadLetters().Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", envelope.EventType)))
				slog.Error("messaging: consumer handler failed; message dead-lettered",
					"event", envelope.EventID, "type", envelope.EventType,
					"attempts", attempts, "error", err)
				if err := d.Reject(false); err != nil {
					return err
				}
			}
		}
	}
}

func envelopeFromDelivery(d amqp.Delivery) Envelope {
	e := Envelope{EventID: d.MessageId, EventType: d.Type, Payload: d.Body}
	if v, ok := d.Headers["event_version"].(string); ok {
		e.EventVersion = v
	}
	if v, ok := d.Headers["trace_id"].(string); ok {
		e.TraceID = v
	}
	return e
}

func (r *rabbitClient) republishRetry(ctx context.Context, d amqp.Delivery) error {
	headers := amqp.Table{}
	maps.Copy(headers, d.Headers)
	headers["x-retry-count"] = retryCount(d.Headers) + 1
	ch, err := r.publishChannel()
	if err != nil {
		return err
	}
	confirmation, err := ch.PublishWithDeferredConfirm(
		// Retries must reuse the original routing key, which the delivery's type
		// carries — re-deriving it from config could move a retry onto a
		// different queue than the first attempt.
		r.cfg.Exchange, r.cfg.routeKey(d.Type), true, false, amqp.Publishing{
			ContentType: d.ContentType, MessageId: d.MessageId, Type: d.Type, Headers: headers, Body: d.Body,
			// 重投的这份是原消息的延续，持久性和首发一致：否则重试一次就把
			// 消息降级成不持久的了。
			DeliveryMode: amqp.Persistent,
		},
	)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return fmt.Errorf("messaging: retry publisher confirmation unavailable")
	}
	acked, err := r.waitConfirmation(ctx, confirmation, d.MessageId)
	if err != nil {
		return err
	}
	if !acked {
		return fmt.Errorf("messaging: retry publisher confirmation rejected")
	}
	return nil
}

// dispatchReturns 跟着一代连接走：returns 被 amqp091 关掉时这个循环就结束。
func (r *rabbitClient) dispatchReturns(returns <-chan amqp.Return) {
	for returned := range returns {
		r.returnMu.Lock()
		waiters := r.pendingReturns[returned.MessageId]
		if len(waiters) > 0 {
			waiter := waiters[0]
			if len(waiters) == 1 {
				delete(r.pendingReturns, returned.MessageId)
			} else {
				r.pendingReturns[returned.MessageId] = waiters[1:]
			}
			r.returnMu.Unlock()
			waiter <- returned
			close(waiter)
			continue
		}
		r.returnMu.Unlock()
	}
}

func (r *rabbitClient) waitConfirmation(ctx context.Context, confirmation *amqp.DeferredConfirmation, messageID string) (bool, error) {
	type confirmationResult struct {
		acked bool
		err   error
	}
	result := make(chan confirmationResult, 1)
	returnedCh := make(chan amqp.Return, 1)
	r.returnMu.Lock()
	r.pendingReturns[messageID] = append(r.pendingReturns[messageID], returnedCh)
	r.returnMu.Unlock()
	defer func() {
		r.returnMu.Lock()
		waiters := r.pendingReturns[messageID]
		for i, waiter := range waiters {
			if waiter == returnedCh {
				r.pendingReturns[messageID] = append(waiters[:i], waiters[i+1:]...)
				if len(r.pendingReturns[messageID]) == 0 {
					delete(r.pendingReturns, messageID)
				}
				break
			}
		}
		r.returnMu.Unlock()
	}()
	go func() {
		acked, err := confirmation.WaitContext(ctx)
		result <- confirmationResult{acked: acked, err: err}
	}()
	for {
		select {
		case outcome := <-result:
			return outcome.acked, outcome.err
		case returned := <-returnedCh:
			// 正常不会走到这条：退回要过 dispatchReturns 才送到这里，而确认通常先到、
			// 上面那个 defer 已经把登记清掉，派发时就找不到等待者了。留着是因为这不
			// 是**保证**——broker 回确认慢、退回先派发时这里仍然接得住。详见 bindKeys
			// 的注释和 2026-09-14 的实测。
			return false, fmt.Errorf("messaging: message returned: %s", returned.ReplyText)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func retryCount(headers amqp.Table) int {
	if value, ok := retryHeaderValue(headers["x-retry-count"]); ok {
		return value
	}
	if deaths, ok := headers["x-death"].([]any); ok {
		count := 0
		for _, death := range deaths {
			if table, ok := death.(amqp.Table); ok {
				if value, ok := retryHeaderValue(table["count"]); ok {
					count += value
				}
			}
		}
		return count
	}
	return 0
}

func retryHeaderValue(value any) (int, bool) {
	const maxInt = int(^uint(0) >> 1)
	switch value := value.(type) {
	case int:
		if value < 0 {
			return 0, false
		}
		return value, true
	case int8:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int16:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int32:
		if value < 0 {
			return 0, false
		}
		return int(value), true
	case int64:
		if value < 0 || value > int64(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint:
		if value > uint(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		if uint64(value) > uint64(maxInt) {
			return 0, false
		}
		return int(value), true
	case uint64:
		if value > uint64(maxInt) {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}
