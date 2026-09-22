package messaging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 投递失败的退避上限与位移上限，两个都是防「一秒一次投到天荒地老」的。
const (
	// defaultMaxRetryDelay 是同一个事件两次投递之间最长等多久。取值是一分钟级别：
	// 到了这个量级，「再试一次」和「人工看一眼」的性价比已经反过来了，而事件仍然
	// 留在表里、仍然会被投——只是不再占着轮询。
	defaultMaxRetryDelay = 5 * time.Minute
	// maxRetryShift 挡住位移溢出。attempts 由认领语句累加，没有上限，而 1 秒左移
	// 33 位就溢出了 int64 的纳秒。
	maxRetryShift = 20
)

// RelayConfig controls polling and lease behavior for an outbox relay.
type RelayConfig struct {
	Owner        string
	BatchSize    int
	Lease        time.Duration
	PollInterval time.Duration
	// RetryDelay 是第一次失败之后的等待，之后每次翻倍，封顶 MaxRetryDelay。
	RetryDelay time.Duration
	// MaxRetryDelay 留零时取 defaultMaxRetryDelay。
	MaxRetryDelay time.Duration
	OnError       func(error)
	// OnPublished and OnFailed report a single event's outcome, as opposed to
	// OnError which reports the batch-level error the relay loop retries on.
	// Both are optional and must not block: the relay calls them inline.
	OnPublished func(LeasedEnvelope)
	OnFailed    func(LeasedEnvelope, error)
}

func (c RelayConfig) withDefaults() RelayConfig {
	if c.Owner == "" {
		c.Owner = uuid.NewString()
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.Lease <= 0 {
		c.Lease = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = time.Second
	}
	if c.MaxRetryDelay <= 0 {
		c.MaxRetryDelay = defaultMaxRetryDelay
	}
	// 上限低于起点的话翻倍立刻被它压住，退避等于没有——把起点抬到上限上，
	// 让「上限」这个词在这里是自洽的。
	if c.MaxRetryDelay < c.RetryDelay {
		c.MaxRetryDelay = c.RetryDelay
	}
	return c
}

// retryDelay 是第 attempts 次失败之后该等多久：RetryDelay 起，每次翻倍，封顶
// MaxRetryDelay。
//
// 为什么要退避、而不是一直按固定间隔重投：投不出去的事件大多不是「等一会儿就好」
// 的那一类（载荷被 broker 拒、事件类型没有绑定、消息本身超限），固定间隔下它们会
// 被**每秒**重投一次，直到进程退出——每秒一次数据库写加一次 broker 往返，只为了
// 换来一条同样的错误。退避之后同一件事的代价降到几分钟一次，而事件还在队列里。
//
// 刻意**不设放弃线**：outbox 里的每一条都对应一件已经发生的事，把它标成「不再投」
// 等于把这件事从系统里删掉。慢，是可以接受的；丢，不行。
func (c RelayConfig) retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > maxRetryShift {
		attempts = maxRetryShift
	}
	delay := c.RetryDelay << (attempts - 1)
	if delay <= 0 || delay > c.MaxRetryDelay {
		return c.MaxRetryDelay
	}
	return delay
}

// Relay publishes claimed outbox events and records the result. It is safe to
// run multiple relays against the same durable outbox; the store owns claim
// exclusion and lease ownership.
type Relay struct {
	outbox    DurableOutbox
	publisher Publisher
	config    RelayConfig
}

func NewRelay(outbox DurableOutbox, publisher Publisher, config RelayConfig) (*Relay, error) {
	if outbox == nil {
		return nil, errors.New("messaging: outbox relay requires an outbox")
	}
	if publisher == nil {
		return nil, errors.New("messaging: outbox relay requires a publisher")
	}
	return &Relay{outbox: outbox, publisher: publisher, config: config.withDefaults()}, nil
}

// Run polls until ctx is canceled. Store or publish failures are recorded and
// retried; infrastructure failures do not silently terminate the relay.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if r.config.OnError != nil {
				r.config.OnError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce claims and processes one batch. It returns claim or bookkeeping
// errors, while still attempting every event in the claimed batch.
func (r *Relay) RunOnce(ctx context.Context) error {
	events, err := r.outbox.ClaimPending(ctx, r.config.BatchSize, r.config.Owner, r.config.Lease)
	if err != nil {
		return fmt.Errorf("messaging: claim outbox events: %w", err)
	}
	var errs []error
	for _, event := range events {
		if err := r.publisher.Publish(ctx, event.Envelope); err != nil {
			if r.config.OnFailed != nil {
				r.config.OnFailed(event, err)
			}
			next := time.Now().Add(r.config.retryDelay(event.Attempts))
			if markErr := r.outbox.MarkFailure(ctx, event.EventID, event.LeaseToken, err, next); markErr != nil {
				recoveryErr := r.releaseLease(ctx, event)
				errs = append(errs, fmt.Errorf("event %q publish: %w; mark failure: %v; lease recovery: %v", event.EventID, err, markErr, recoveryErr))
			} else {
				errs = append(errs, fmt.Errorf("event %q publish: %w", event.EventID, err))
			}
			continue
		}
		if err := r.outbox.MarkSuccess(ctx, event.EventID, event.LeaseToken); err != nil {
			recoveryErr := r.releaseLease(ctx, event)
			errs = append(errs, fmt.Errorf("event %q mark success: %w; lease recovery: %v", event.EventID, err, recoveryErr))
			continue
		}
		// 只有 MarkSuccess 也成了才算投递完成：bookkeeping 失败时事件会被重新
		// 投递，这时候把它计成「已发布」会把重复投递藏起来。
		if r.config.OnPublished != nil {
			r.config.OnPublished(event)
		}
	}
	return errors.Join(errs...)
}

func (r *Relay) releaseLease(ctx context.Context, event LeasedEnvelope) error {
	releaser, ok := r.outbox.(interface {
		Release(context.Context, string, string) error
	})
	if !ok {
		return errors.New("outbox does not support lease release")
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return releaser.Release(recoveryCtx, event.EventID, event.LeaseToken)
}
