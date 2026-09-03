package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const defaultLeaseTTL int64 = 30

type Instance struct {
	Service     string `json:"service"`
	ID          string `json:"id"`
	Address     string `json:"address"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
}

type Registry interface {
	Register(context.Context, Instance) error
	Unregister(context.Context, Instance) error
	Close() error
}

// Resolver and Watcher are optional discovery interfaces for registry adapters.
type Resolver interface {
	Resolve(context.Context, string) ([]Instance, error)
}

type Event struct {
	// Type is one of "put" or "delete". Err is set when watching fails.
	Type     string
	Instance Instance
	Err      error
}

type Watcher interface {
	Watch(context.Context, string) (<-chan Event, error)
}

// New selects the configured registry adapter. An empty endpoint uses Noop.
func New(endpoint string) Registry {
	endpoints := splitEndpoints(endpoint)
	if len(endpoints) == 0 {
		return Noop{}
	}

	client, err := clientv3.New(clientv3.Config{Endpoints: endpoints})
	if err != nil {
		slog.Error("create etcd registry client", "error", err)
		return failed{err: err}
	}
	return &Etcd{client: client, kv: client, watcher: client}
}

// NewEtcd creates an etcd adapter from an existing client. It is useful for tests
// and for applications that manage the client lifecycle themselves.
func NewEtcd(client *clientv3.Client) *Etcd {
	if client == nil {
		return nil
	}
	return &Etcd{client: client, kv: client, watcher: client}
}

func splitEndpoints(endpoint string) []string {
	var endpoints []string
	for _, value := range strings.Split(endpoint, ",") {
		if value = strings.TrimSpace(value); value != "" {
			endpoints = append(endpoints, value)
		}
	}
	return endpoints
}

type Noop struct{}

func (Noop) Register(context.Context, Instance) error   { return nil }
func (Noop) Unregister(context.Context, Instance) error { return nil }
func (Noop) Close() error                               { return nil }

type failed struct{ err error }

func (f failed) Register(context.Context, Instance) error   { return f.err }
func (f failed) Unregister(context.Context, Instance) error { return f.err }
func (f failed) Close() error                               { return nil }

// Etcd stores only service registration metadata under a leased key.
type Etcd struct {
	client  *clientv3.Client
	kv      clientv3.KV
	watcher clientv3.Watcher

	mu       sync.Mutex
	opMu     sync.Mutex
	leaseID  clientv3.LeaseID
	leaseKey string
	cancel   context.CancelFunc
	keepDone chan struct{}
	closed   bool
}

func (e *Etcd) Resolve(ctx context.Context, service string) ([]Instance, error) {
	if err := validateService(service); err != nil {
		return nil, err
	}
	resp, err := e.kvClient().Get(ctx, servicePrefix(service), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("resolve service: %w", err)
	}
	instances := make([]Instance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var instance Instance
		if err := json.Unmarshal(kv.Value, &instance); err != nil {
			return nil, fmt.Errorf("decode service instance %q: %w", string(kv.Key), err)
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

// Watch emits the current service instances as individual put events, followed
// by changes from etcd. The returned channel is closed when ctx is canceled or
// the etcd watch terminates.
func (e *Etcd) Watch(ctx context.Context, service string) (<-chan Event, error) {
	if err := validateService(service); err != nil {
		return nil, err
	}
	instances, err := e.Resolve(ctx, service)
	if err != nil {
		return nil, err
	}
	out := make(chan Event, len(instances)+1)
	for _, instance := range instances {
		out <- Event{Type: "put", Instance: instance}
	}
	watchCtx, cancel := context.WithCancel(ctx)
	watchCh := e.watcherClient().Watch(watchCtx, servicePrefix(service), clientv3.WithPrefix(), clientv3.WithPrevKV())
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-watchCh:
				if !ok {
					return
				}
				if response.Err() != nil {
					sendEvent(ctx, out, Event{Err: response.Err()})
					return
				}
				for _, event := range response.Events {
					instance, decodeErr := decodeEvent(event)
					if decodeErr != nil {
						sendEvent(ctx, out, Event{Err: decodeErr})
						return
					}
					typeName := "put"
					if event.Type == clientv3.EventTypeDelete {
						typeName = "delete"
					}
					if !sendEvent(ctx, out, Event{Type: typeName, Instance: instance}) {
						return
					}
				}
			}
		}
	}()
	return out, nil
}

func sendEvent(ctx context.Context, out chan<- Event, event Event) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func decodeEvent(event *clientv3.Event) (Instance, error) {
	if event == nil || event.Kv == nil {
		return Instance{}, errors.New("registry event key is required")
	}
	var instance Instance
	kv := event.Kv
	if event.Type == clientv3.EventTypeDelete {
		if event.PrevKv == nil {
			return Instance{}, fmt.Errorf("decode service event %q: previous value is required", string(kv.Key))
		}
		kv = event.PrevKv
	}
	if err := json.Unmarshal(kv.Value, &instance); err != nil {
		return Instance{}, fmt.Errorf("decode service event %q: %w", string(event.Kv.Key), err)
	}
	return instance, nil
}

func (e *Etcd) kvClient() clientv3.KV {
	if e.kv != nil {
		return e.kv
	}
	return e.client
}

func (e *Etcd) watcherClient() clientv3.Watcher {
	if e.watcher != nil {
		return e.watcher
	}
	return e.client
}
func (e *Etcd) Register(ctx context.Context, instance Instance) error {
	if err := validateInstance(instance); err != nil {
		return err
	}
	value, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("marshal registry instance: %w", err)
	}

	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return errors.New("registry is closed")
	}

	lease, err := e.client.Grant(ctx, defaultLeaseTTL)
	if err != nil {
		return fmt.Errorf("grant registry lease: %w", err)
	}
	key := instanceKey(instance)
	if _, err = e.client.Put(ctx, key, string(value), clientv3.WithLease(lease.ID)); err != nil {
		_, _ = e.client.Revoke(context.Background(), lease.ID)
		return fmt.Errorf("register instance: %w", err)
	}

	keepCtx, cancel := context.WithCancel(context.Background())
	keepAlive, err := e.client.KeepAlive(keepCtx, lease.ID)
	if err != nil {
		cancel()
		_, _ = e.client.Revoke(context.Background(), lease.ID)
		return fmt.Errorf("keep registry lease alive: %w", err)
	}

	e.mu.Lock()
	oldLeaseID, oldCancel, oldDone := e.leaseID, e.cancel, e.keepDone
	e.leaseID, e.leaseKey, e.cancel = lease.ID, key, cancel
	e.keepDone = make(chan struct{})
	done := e.keepDone
	e.mu.Unlock()
	go e.consumeKeepAlive(keepAlive, done)

	if oldCancel != nil {
		oldCancel()
		<-oldDone
	}
	if oldLeaseID != 0 {
		_, _ = e.client.Revoke(context.Background(), oldLeaseID)
	}
	return nil
}

func (e *Etcd) consumeKeepAlive(keepAlive <-chan *clientv3.LeaseKeepAliveResponse, done chan struct{}) {
	defer close(done)
	for response := range keepAlive {
		if response == nil {
			slog.Warn("etcd registry lease keepalive stopped")
			return
		}
	}
}

func (e *Etcd) Unregister(ctx context.Context, instance Instance) error {
	if err := validateInstance(instance); err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()

	key := instanceKey(instance)
	value, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("marshal registry instance: %w", err)
	}
	e.mu.Lock()
	leaseID, leaseKey, cancel, done := e.leaseID, e.leaseKey, e.cancel, e.keepDone
	if leaseKey == key {
		e.leaseID, e.leaseKey, e.cancel, e.keepDone = 0, "", nil, nil
	}
	e.mu.Unlock()
	if cancel != nil && leaseKey == key {
		cancel()
		<-done
	}

	transaction := e.kvClient().Txn(ctx).If(clientv3.Compare(clientv3.Value(key), "=", string(value)))
	if leaseID != 0 && leaseKey == key {
		transaction = transaction.If(clientv3.Compare(clientv3.LeaseValue(key), "=", leaseID))
	}
	response, err := transaction.Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		return fmt.Errorf("unregister instance: %w", err)
	}
	if response.Succeeded && leaseID != 0 && leaseKey == key {
		if _, revokeErr := e.client.Revoke(context.Background(), leaseID); revokeErr != nil {
			return fmt.Errorf("unregister instance: %w", revokeErr)
		}
	}
	return nil
}

func (e *Etcd) Close() error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	leaseID, cancel, done := e.leaseID, e.cancel, e.keepDone
	e.leaseID, e.leaseKey, e.cancel, e.keepDone = 0, "", nil, nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if leaseID != 0 {
		_, _ = e.client.Revoke(context.Background(), leaseID)
	}
	return e.client.Close()
}

func validateService(service string) error {
	if err := validatePathSegment(service, "service"); err != nil {
		return fmt.Errorf("registry %w name", err)
	}
	return nil
}

func validateInstance(instance Instance) error {
	if err := validateService(instance.Service); err != nil {
		return err
	}
	if err := validatePathSegment(instance.ID, "instance"); err != nil {
		return fmt.Errorf("registry %w id", err)
	}
	return nil
}

func validatePathSegment(value, kind string) error {
	if value == "" {
		return fmt.Errorf("%s service and id are required", kind)
	}
	if value != strings.TrimSpace(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("%s contains an invalid path segment", kind)
	}
	return nil
}

func instanceKey(instance Instance) string {
	return path.Join("/panda/services", instance.Service, instance.ID)
}

func servicePrefix(service string) string {
	return path.Join("/panda/services", service) + "/"
}
