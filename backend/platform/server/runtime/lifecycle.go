package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
)

// Options contains the infrastructure dependencies owned by a service runtime.
// Dependencies may be injected for tests or applications with custom adapters.
type Options struct {
	Database         database.Pool
	Cache            cache.Client
	Publisher        messaging.Publisher
	Consumer         messaging.Consumer
	Messaging        io.Closer
	MessagingCleanup io.Closer
	Registry         registry.Registry
	Observability    observability.Providers
	Instance         registry.Instance
	ConsumerHandler  messaging.Handler
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	OwnDatabase      bool
	OwnCache         bool
	OwnMessaging     bool
	OwnRegistry      bool
	OwnObservability bool
}

// Lifecycle coordinates startup and shutdown of platform dependencies.
type Lifecycle struct {
	database, cache                                                    any
	db                                                                 database.Pool
	redis                                                              cache.Client
	publisher                                                          messaging.Publisher
	consumer                                                           messaging.Consumer
	messaging                                                          io.Closer
	messagingCleanup                                                   io.Closer
	registry                                                           registry.Registry
	providers                                                          observability.Providers
	instance                                                           registry.Instance
	handler                                                            messaging.Handler
	startupTimeout, shutdownTimeout                                    time.Duration
	ownDatabase, ownCache, ownMessaging, ownRegistry, ownObservability bool
	consumerCancel                                                     context.CancelFunc
	consumerDone                                                       chan error
	consumerRunning                                                    bool
	started                                                            bool
	starting                                                           bool
	startupDone                                                        chan struct{}
	startupErr                                                         error
	registered                                                         bool
	stopped                                                            bool
	stopDone                                                           chan struct{}
	stopErr                                                            error
	mu                                                                 sync.Mutex
}

func New(options Options) *Lifecycle {
	return &Lifecycle{
		db: options.Database, redis: options.Cache, publisher: options.Publisher,
		consumer: options.Consumer, messaging: options.Messaging, messagingCleanup: options.MessagingCleanup, registry: options.Registry,
		providers: options.Observability, instance: options.Instance, handler: options.ConsumerHandler,
		startupTimeout: options.StartupTimeout, shutdownTimeout: options.ShutdownTimeout,
		ownDatabase: options.OwnDatabase, ownCache: options.OwnCache, ownMessaging: options.OwnMessaging,
		ownRegistry: options.OwnRegistry, ownObservability: options.OwnObservability,
	}
}

func (l *Lifecycle) startupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if l.startupTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, l.startupTimeout)
}

// BeforeStart initializes and verifies infrastructure in dependency order. If a
// later step fails, resources already started are rolled back immediately.
func (l *Lifecycle) BeforeStart(ctx context.Context) error {
	l.mu.Lock()
	if l.starting {
		done := l.startupDone
		l.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		l.mu.Lock()
		err := l.startupErr
		l.mu.Unlock()
		return err
	}
	if l.stopped {
		l.mu.Unlock()
		return errors.New("lifecycle is already stopped")
	}
	if l.started {
		l.mu.Unlock()
		return nil
	}
	if l.consumerRunning {
		l.mu.Unlock()
		return errors.New("lifecycle consumer is still stopping")
	}
	l.starting = true
	l.startupDone = make(chan struct{})
	done := l.startupDone
	l.mu.Unlock()

	ctx, cancel := l.startupContext(ctx)
	defer cancel()
	started := make([]func(context.Context) error, 0, 4)
	finish := func(err error) error {
		l.mu.Lock()
		l.starting = false
		l.started = err == nil
		l.startupErr = err
		close(done)
		l.mu.Unlock()
		return err
	}
	rollback := func(cause error) error {
		cleanupCtx := context.Background()
		if l.shutdownTimeout > 0 {
			var cancel context.CancelFunc
			cleanupCtx, cancel = context.WithTimeout(cleanupCtx, l.shutdownTimeout)
			defer cancel()
		}
		var errs []error
		for _, cleanup := range slices.Backward(started) {
			if err := cleanup(cleanupCtx); err != nil {
				errs = append(errs, err)
			}
		}
		return finish(errors.Join(append([]error{cause}, errs...)...))
	}
	if l.db != nil {
		if l.ownDatabase {
			started = append(started, func(context.Context) error { l.db.Close(); return nil })
		}
		if err := l.db.Ping(ctx); err != nil {
			return rollback(fmt.Errorf("database startup: %w", err))
		}
	}
	if l.redis != nil {
		if l.ownCache {
			started = append(started, func(context.Context) error { return l.redis.Close() })
		}
		if err := l.redis.Ping(ctx); err != nil {
			return rollback(fmt.Errorf("cache startup: %w", err))
		}
	}
	if l.registry != nil && l.instance.Service != "" {
		if err := l.registry.Register(ctx, l.instance); err != nil {
			return rollback(fmt.Errorf("registry startup: %w", err))
		}
		l.mu.Lock()
		l.registered = true
		l.mu.Unlock()
		started = append(started, func(c context.Context) error {
			err := l.registry.Unregister(c, l.instance)
			if err == nil {
				l.mu.Lock()
				l.registered = false
				l.mu.Unlock()
			}
			return err
		})
	}
	if l.consumer != nil && l.handler != nil {
		consumerCtx, consumerCancel := context.WithCancel(context.Background())
		l.mu.Lock()
		l.consumerCancel = consumerCancel
		l.consumerDone = make(chan error, 1)
		done := l.consumerDone
		l.consumerRunning = true
		l.mu.Unlock()
		go func() {
			err := l.consumer.Consume(consumerCtx, l.handler)
			done <- err
			l.mu.Lock()
			l.consumerRunning = false
			l.mu.Unlock()
		}()
		started = append(started, func(c context.Context) error {
			consumerCancel()
			select {
			case err := <-done:
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			case <-c.Done():
				return nil
			}
		})
	}
	return finish(nil)
}

// AfterStop closes resources in reverse dependency order. Shutdown gets its own
// timeout so a stuck dependency cannot block graceful termination indefinitely.
func (l *Lifecycle) AfterStop(ctx context.Context) error {
	for {
		l.mu.Lock()
		if l.starting {
			done := l.startupDone
			l.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if l.stopDone != nil {
			done := l.stopDone
			l.mu.Unlock()
			select {
			case <-done:
				l.mu.Lock()
				err := l.stopErr
				l.mu.Unlock()
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		l.stopDone = make(chan struct{})
		l.stopped = true
		l.mu.Unlock()
		break
	}
	if l.shutdownTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.shutdownTimeout)
		defer cancel()
	}
	var errs []error
	l.mu.Lock()
	consumerCancel, consumerDone := l.consumerCancel, l.consumerDone
	l.consumerCancel, l.consumerDone = nil, nil
	l.mu.Unlock()
	if consumerCancel != nil {
		consumerCancel()
	}
	if consumerDone != nil {
		select {
		case err := <-consumerDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				errs = append(errs, err)
			}
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}

	// Unregister independently of consumer termination. A stuck consumer must
	// not leave a stale service instance in the registry.
	if l.registry != nil && l.instance.Service != "" {
		l.mu.Lock()
		registered := l.registered
		l.mu.Unlock()
		if registered {
			unregisterCtx := ctx
			if unregisterCtx.Err() != nil {
				if l.shutdownTimeout > 0 {
					var cancel context.CancelFunc
					unregisterCtx, cancel = context.WithTimeout(context.Background(), l.shutdownTimeout)
					defer cancel()
				} else {
					unregisterCtx = context.Background()
				}
			}
			if err := l.registry.Unregister(unregisterCtx, l.instance); err != nil {
				errs = append(errs, err)
			} else {
				l.mu.Lock()
				l.registered = false
				l.mu.Unlock()
			}
		}
	}
	if l.registry != nil && l.ownRegistry {
		if err := l.registry.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	// Close messaging after the consumer exits. If it exceeds the shutdown
	// bound, Close is the explicit force-close operation provided by io.Closer.
	// This prevents a stuck consumer from becoming a permanent resource leak.
	if l.messagingCleanup != nil {
		if err := closeMessaging(ctx, l.messagingCleanup); err != nil {
			errs = append(errs, err)
		}
	}
	if l.messaging != nil && l.ownMessaging {
		if err := closeMessaging(ctx, l.messaging); err != nil {
			errs = append(errs, err)
		}
	}
	if l.redis != nil && l.ownCache {
		if err := l.redis.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if l.db != nil && l.ownDatabase {
		l.db.Close()
	}
	if l.ownObservability {
		if err := l.providers.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	result := errors.Join(errs...)
	l.mu.Lock()
	l.stopErr = result
	close(l.stopDone)
	l.mu.Unlock()
	return result
}

func closeMessaging(ctx context.Context, closer io.Closer) error {
	if force, ok := closer.(interface{ CloseContext(context.Context) error }); ok {
		return force.CloseContext(ctx)
	}
	return closer.Close()
}
