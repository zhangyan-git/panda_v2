package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
)

// Options contains the infrastructure dependencies owned by a service runtime.
// Dependencies may be injected for tests or applications with custom adapters.
type Runner interface {
	Run(context.Context) error
}

type Options struct {
	Database         database.Pool
	Cache            cache.Client
	Publisher        messaging.Publisher
	Consumer         messaging.Consumer
	Workers          []Runner
	Messaging        io.Closer
	MessagingCleanup io.Closer
	Registry         registry.Registry
	Observability    observability.Providers
	Instance         registry.Instance
	ConsumerHandler  messaging.Handler
	HTTPRoutes       func(*khttp.Server)
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	OwnDatabase      bool
	OwnCache         bool
	OwnMessaging     bool
	OwnRegistry      bool
	OwnObservability bool
}

// Lifecycle coordinates startup and shutdown of platform dependencies.
type workerState struct {
	cancel  context.CancelFunc
	done    chan error
	started chan struct{}
}

type Lifecycle struct {
	database, cache                                                    any
	db                                                                 database.Pool
	redis                                                              cache.Client
	publisher                                                          messaging.Publisher
	consumer                                                           messaging.Consumer
	workers                                                            []Runner
	messaging                                                          io.Closer
	messagingCleanup                                                   io.Closer
	registry                                                           registry.Registry
	providers                                                          observability.Providers
	instance                                                           registry.Instance
	handler                                                            messaging.Handler
	httpRoutes                                                         func(*khttp.Server)
	startupTimeout, shutdownTimeout                                    time.Duration
	ownDatabase, ownCache, ownMessaging, ownRegistry, ownObservability bool
	consumerCancel                                                     context.CancelFunc
	consumerDone                                                       chan error
	consumerRunning                                                    bool
	workerStates                                                       []workerState
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
		consumer: options.Consumer, workers: append([]Runner(nil), options.Workers...), messaging: options.Messaging, messagingCleanup: options.MessagingCleanup, registry: options.Registry,
		providers: options.Observability, instance: options.Instance, handler: options.ConsumerHandler, httpRoutes: options.HTTPRoutes,
		startupTimeout: options.StartupTimeout, shutdownTimeout: options.ShutdownTimeout,
		ownDatabase: options.OwnDatabase, ownCache: options.OwnCache, ownMessaging: options.OwnMessaging,
		ownRegistry: options.OwnRegistry, ownObservability: options.OwnObservability,
	}
}

func (l *Lifecycle) startupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if l.startupTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, l.startupTimeout)
}

// BeforeStart initializes and verifies infrastructure in dependency order. If a
// later step fails, resources already started are rolled back immediately.
func (l *Lifecycle) BeforeStart(ctx context.Context) error {
	ctx = normalizeContext(ctx)
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
	started := make([]func(context.Context) error, 0, 8)
	if l.ownMessaging {
		started = append(started, func(ctx context.Context) error { return closeMessaging(ctx, l.messaging) })
	}
	if l.ownObservability {
		started = append(started, func(ctx context.Context) error { return l.providers.Shutdown(ctx) })
	}
	if l.ownRegistry {
		started = append(started, func(context.Context) error { return l.registry.Close() })
	}
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
		l.mu.Lock()
		l.consumerCancel, l.consumerDone = nil, nil
		l.workerStates = nil
		l.mu.Unlock()
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
		consumerStarted := make(chan struct{})
		l.mu.Lock()
		l.consumerCancel = consumerCancel
		l.consumerDone = make(chan error, 1)
		done := l.consumerDone
		l.consumerRunning = true
		l.mu.Unlock()
		go func() {
			close(consumerStarted)
			err := recoverRun(func() error { return l.consumer.Consume(consumerCtx, l.handler) })
			done <- err
			l.mu.Lock()
			l.consumerRunning = false
			l.mu.Unlock()
		}()
		select {
		case err := <-done:
			return rollback(fmt.Errorf("consumer startup: %w", err))
		case <-consumerStarted:
		}
		started = append(started, func(c context.Context) error {
			consumerCancel()
			select {
			case err := <-done:
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			case <-c.Done():
				return c.Err()
			}
		})
		select {
		case err := <-done:
			return rollback(fmt.Errorf("consumer startup: %w", err))
		case <-consumerStarted:
		}
	}
	for _, worker := range l.workers {
		if worker == nil {
			continue
		}
		workerCtx, workerCancel := context.WithCancel(context.Background())
		workerDone := make(chan error, 1)
		workerStarted := make(chan struct{})
		l.mu.Lock()
		l.workerStates = append(l.workerStates, workerState{cancel: workerCancel, done: workerDone})
		l.mu.Unlock()
		go func(worker Runner, ctx context.Context, done chan error, started chan struct{}) {
			close(started)
			done <- recoverRun(func() error { return worker.Run(ctx) })
		}(worker, workerCtx, workerDone, workerStarted)
		select {
		case <-workerStarted:
		case <-workerDone:
			return rollback(errors.New("worker exited during startup"))
		}
		started = append(started, func(c context.Context) error {
			workerCancel()
			select {
			case err := <-workerDone:
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
	ctx = normalizeContext(ctx)
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
		started := l.started
		startupErr := l.startupErr
		l.mu.Unlock()
		if !started && startupErr != nil {
			l.mu.Lock()
			l.stopErr = nil
			close(l.stopDone)
			l.mu.Unlock()
			return nil
		}
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
	workers := append([]workerState(nil), l.workerStates...)
	l.workerStates = nil
	l.mu.Unlock()
	for _, worker := range workers {
		worker.cancel()
	}
	for _, worker := range workers {
		select {
		case err := <-worker.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				errs = append(errs, err)
			}
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
		}
	}
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

func recoverRun(run func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v\n%s", recovered, debug.Stack())
		}
	}()
	return run()
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func closeMessaging(ctx context.Context, closer io.Closer) error {
	if force, ok := closer.(interface{ CloseContext(context.Context) error }); ok {
		return force.CloseContext(ctx)
	}
	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
