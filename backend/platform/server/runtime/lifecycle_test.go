package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/observability"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
)

type lifecycleRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *lifecycleRecorder) add(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *lifecycleRecorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type fakeDB struct {
	recorder *lifecycleRecorder
	pingErr  error
}

func (f *fakeDB) Ping(context.Context) error { f.recorder.add("db-ping"); return f.pingErr }
func (f *fakeDB) Close()                     { f.recorder.add("db-close") }

type fakeCache struct {
	recorder *lifecycleRecorder
	pingErr  error
	closeErr error
}

func (f *fakeCache) Ping(context.Context) error { f.recorder.add("cache-ping"); return f.pingErr }
func (f *fakeCache) Close() error               { f.recorder.add("cache-close"); return f.closeErr }

type fakeRegistry struct {
	recorder      *lifecycleRecorder
	registerErr   error
	unregisterErr error
	closeErr      error
}

func (f *fakeRegistry) Register(context.Context, registry.Instance) error {
	f.recorder.add("registry-register")
	return f.registerErr
}
func (f *fakeRegistry) Unregister(context.Context, registry.Instance) error {
	f.recorder.add("registry-unregister")
	return f.unregisterErr
}
func (f *fakeRegistry) Close() error { f.recorder.add("registry-close"); return f.closeErr }

type fakeCloser struct {
	recorder *lifecycleRecorder
	name     string
	err      error
}

func (f *fakeCloser) Close() error { f.recorder.add(f.name); return f.err }

type fakeConsumer struct {
	recorder *lifecycleRecorder
	started  chan struct{}
	returnCh chan error
	cancelCh chan struct{}
	block    bool
}

func (f *fakeConsumer) Consume(ctx context.Context, _ messaging.Handler) error {
	f.recorder.add("consumer-start")
	if f.started != nil {
		close(f.started)
	}
	if f.block {
		return <-f.returnCh
	}
	select {
	case err := <-f.returnCh:
		return err
	case <-ctx.Done():
		if f.cancelCh != nil {
			close(f.cancelCh)
		}
		return ctx.Err()
	}
}

type fakeWorker struct {
	recorder *lifecycleRecorder
	started  chan struct{}
	stopped  chan struct{}
	err      error
}

func (f *fakeWorker) Run(ctx context.Context) error {
	f.recorder.add("worker-start")
	close(f.started)
	if f.err != nil {
		return f.err
	}
	<-ctx.Done()
	f.recorder.add("worker-stop")
	close(f.stopped)
	return ctx.Err()
}

var (
	_ database.Pool        = (*fakeDB)(nil)
	_ cache.Client         = (*fakeCache)(nil)
	_ registry.Registry    = (*fakeRegistry)(nil)
	_ messaging.Consumer   = (*fakeConsumer)(nil)
	_ observability.Logger = observability.NopLogger{}
)

func testInstance() registry.Instance { return registry.Instance{Service: "svc", ID: "id"} }

func TestLifecycleSuccessfulOrderAndOwnership(t *testing.T) {
	r := &lifecycleRecorder{}
	consumer := &fakeConsumer{recorder: r, started: make(chan struct{}), returnCh: make(chan error, 1)}
	l := New(Options{
		Database: &fakeDB{recorder: r}, Cache: &fakeCache{recorder: r},
		Registry: &fakeRegistry{recorder: r}, Instance: testInstance(),
		Consumer: consumer, ConsumerHandler: func(context.Context, messaging.Envelope) error { return nil },
		MessagingCleanup: &fakeCloser{recorder: r, name: "messaging-cleanup"},
		Messaging:        &fakeCloser{recorder: r, name: "messaging-close"},
		OwnDatabase:      true, OwnCache: true, OwnRegistry: true, OwnMessaging: true,
		OwnObservability: true,
	})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-consumer.started
	if err := l.AfterStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"db-ping", "cache-ping", "registry-register", "consumer-start", "registry-unregister", "registry-close", "messaging-cleanup", "messaging-close", "cache-close", "db-close"}
	if got := r.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestLifecycleDoesNotCloseUnownedResources(t *testing.T) {
	r := &lifecycleRecorder{}
	l := New(Options{Database: &fakeDB{recorder: r}, Cache: &fakeCache{recorder: r}, Messaging: &fakeCloser{recorder: r, name: "messaging"}, Registry: &fakeRegistry{recorder: r}, Instance: testInstance()})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.AfterStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.get(); !reflect.DeepEqual(got, []string{"db-ping", "cache-ping", "registry-register", "registry-unregister"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestLifecycleStartupRollback(t *testing.T) {
	tests := []struct {
		name string
		make func(*lifecycleRecorder) (*Lifecycle, error)
		want []string
	}{
		{"database", func(r *lifecycleRecorder) (*Lifecycle, error) {
			err := errors.New("db down")
			return New(Options{Database: &fakeDB{recorder: r, pingErr: err}, OwnDatabase: true}), err
		}, []string{"db-ping", "db-close"}},
		{"cache", func(r *lifecycleRecorder) (*Lifecycle, error) {
			err := errors.New("cache down")
			return New(Options{Database: &fakeDB{recorder: r}, Cache: &fakeCache{recorder: r, pingErr: err}, OwnDatabase: true, OwnCache: true}), err
		}, []string{"db-ping", "cache-ping", "cache-close", "db-close"}},
		{"registry", func(r *lifecycleRecorder) (*Lifecycle, error) {
			err := errors.New("registry down")
			return New(Options{Database: &fakeDB{recorder: r}, Cache: &fakeCache{recorder: r}, Registry: &fakeRegistry{recorder: r, registerErr: err}, Instance: testInstance(), OwnDatabase: true, OwnCache: true}), err
		}, []string{"db-ping", "cache-ping", "registry-register", "cache-close", "db-close"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &lifecycleRecorder{}
			l, cause := tt.make(r)
			err := l.BeforeStart(context.Background())
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want %v", err, cause)
			}
			if got := r.get(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("events = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLifecycleBeforeStartRetry(t *testing.T) {
	r := &lifecycleRecorder{}
	cause := errors.New("temporary")
	db := &fakeDB{recorder: r, pingErr: cause}
	l := New(Options{Database: db, OwnDatabase: true})
	if err := l.BeforeStart(context.Background()); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	db.pingErr = nil
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.get(); !reflect.DeepEqual(got, []string{"db-ping", "db-close", "db-ping"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestLifecycleConsumerCancelAndTimeout(t *testing.T) {
	r := &lifecycleRecorder{}
	consumer := &fakeConsumer{recorder: r, started: make(chan struct{}), cancelCh: make(chan struct{}), returnCh: make(chan error)}
	l := New(Options{Consumer: consumer, ConsumerHandler: func(context.Context, messaging.Envelope) error { return nil }, ShutdownTimeout: time.Second})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-consumer.started
	if err := l.AfterStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consumer.cancelCh:
	case <-time.After(time.Second):
		t.Fatal("consumer was not canceled")
	}

	blocked := &fakeConsumer{recorder: r, started: make(chan struct{}), returnCh: make(chan error), block: true}
	l = New(Options{Consumer: blocked, ConsumerHandler: func(context.Context, messaging.Envelope) error { return nil }, ShutdownTimeout: 10 * time.Millisecond})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-blocked.started
	if err := l.AfterStop(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestLifecycleWorkersStartStopBeforeResourcesClose(t *testing.T) {
	r := &lifecycleRecorder{}
	worker := &fakeWorker{recorder: r, started: make(chan struct{}), stopped: make(chan struct{})}
	l := New(Options{
		Workers:          []Runner{worker},
		MessagingCleanup: &fakeCloser{recorder: r, name: "messaging-cleanup"},
		Messaging:        &fakeCloser{recorder: r, name: "messaging-close"},
		OwnMessaging:     true,
	})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-worker.started
	if err := l.AfterStop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-worker.stopped; ok {
		t.Fatal("worker stop channel should be closed")
	}
	got := r.get()
	workerStop, cleanup := indexOf(got, "worker-stop"), indexOf(got, "messaging-cleanup")
	if workerStop < 0 || cleanup < 0 || workerStop > cleanup {
		t.Fatalf("events = %v, worker must stop before messaging cleanup", got)
	}
}

func TestLifecycleWorkerErrorIsAggregatedOnStop(t *testing.T) {
	r := &lifecycleRecorder{}
	workerErr := errors.New("worker failed")
	worker := &fakeWorker{recorder: r, started: make(chan struct{}), stopped: make(chan struct{}), err: workerErr}
	l := New(Options{Workers: []Runner{worker}})
	if err := l.BeforeStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-worker.started
	if err := l.AfterStop(context.Background()); !errors.Is(err, workerErr) {
		t.Fatalf("error = %v, want %v", err, workerErr)
	}
}

func indexOf(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

func TestLifecycleErrorAggregationAndConcurrentAfterStop(t *testing.T) {
	r := &lifecycleRecorder{}
	cacheErr, messagingErr := errors.New("cache close"), errors.New("messaging close")
	l := New(Options{Cache: &fakeCache{recorder: r, closeErr: cacheErr}, Messaging: &fakeCloser{recorder: r, name: "messaging", err: messagingErr}, OwnCache: true, OwnMessaging: true})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.AfterStop(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, cacheErr) || !errors.Is(err, messagingErr) {
			t.Fatalf("error = %v", err)
		}
	}
	if got := r.get(); !reflect.DeepEqual(got, []string{"messaging", "cache-close"}) {
		t.Fatalf("events = %v", got)
	}
}
