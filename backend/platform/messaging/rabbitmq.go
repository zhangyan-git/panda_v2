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
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

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

// bindKey is the pattern this service's queue subscribes to. With no configured
// key the queue takes everything: a topic exchange delivers only what a binding
// matches, so an empty binding would strand every event. That failure is not
// quiet — publishes are mandatory and confirmed, so a stranded event comes back
// as a returned message and the relay retries it forever.
func (c RabbitConfig) bindKey() string {
	if c.RoutingKey != "" {
		return c.RoutingKey
	}
	return "#"
}

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
	key := cfg.bindKey()
	args := amqp.Table{}
	if cfg.DLX != "" {
		args["x-dead-letter-exchange"] = cfg.DLX
		args["x-dead-letter-routing-key"] = key
	}
	if _, err := ch.QueueDeclare(cfg.Queue, true, false, false, false, args); err != nil {
		return err
	}
	if err := ch.QueueBind(cfg.Queue, key, cfg.Exchange, false, nil); err != nil {
		return err
	}
	if cfg.DLQ != "" {
		if _, err := ch.QueueDeclare(cfg.DLQ, true, false, false, false, nil); err != nil {
			return err
		}
		if err := ch.QueueBind(cfg.DLQ, key, cfg.DLX, false, nil); err != nil {
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
			if err := handler(ctx, envelope); err == nil {
				if ackErr := d.Ack(false); ackErr != nil {
					return ackErr
				}
			} else if retryCount(d.Headers) < r.cfg.RetryLimit {
				if err := r.republishRetry(ctx, d); err != nil {
					return err
				}
				if err := d.Ack(false); err != nil {
					return err
				}
			} else if err := d.Reject(false); err != nil {
				return err
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
