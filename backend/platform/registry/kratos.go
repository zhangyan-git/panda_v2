package registry

import (
	"context"

	kratosregistry "github.com/go-kratos/kratos/v2/registry"
)

// KratosRegistrar adapts this package's Registry to Kratos' registrar API.
type KratosRegistrar struct {
	Registry Registry
}

func (r KratosRegistrar) Register(ctx context.Context, instance *kratosregistry.ServiceInstance) error {
	return r.Registry.Register(ctx, fromKratosInstance(instance))
}

func (r KratosRegistrar) Deregister(ctx context.Context, instance *kratosregistry.ServiceInstance) error {
	return r.Registry.Unregister(ctx, fromKratosInstance(instance))
}

// KratosDiscovery adapts an Etcd resolver to Kratos' discovery API.
type KratosDiscovery struct {
	Resolver Resolver
	Watcher  Watcher
}

func (d KratosDiscovery) GetService(ctx context.Context, name string) ([]*kratosregistry.ServiceInstance, error) {
	instances, err := d.Resolver.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	result := make([]*kratosregistry.ServiceInstance, 0, len(instances))
	for _, instance := range instances {
		result = append(result, toKratosInstance(instance))
	}
	return result, nil
}

func (d KratosDiscovery) Watch(ctx context.Context, name string) (kratosregistry.Watcher, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	events, err := d.Watcher.Watch(watchCtx, name)
	if err != nil {
		cancel()
		return nil, err
	}
	return &kratosWatcher{ctx: watchCtx, cancel: cancel, events: events, instances: make(map[string]*kratosregistry.ServiceInstance)}, nil
}

type kratosWatcher struct {
	ctx       context.Context
	cancel    context.CancelFunc
	events    <-chan Event
	instances map[string]*kratosregistry.ServiceInstance
}

func (w *kratosWatcher) Next() ([]*kratosregistry.ServiceInstance, error) {
	select {
	case <-w.ctx.Done():
		return nil, w.ctx.Err()
	case event, ok := <-w.events:
		if !ok {
			return nil, context.Canceled
		}
		if event.Err != nil {
			return nil, event.Err
		}
		instance := toKratosInstance(event.Instance)
		if event.Type == "delete" {
			delete(w.instances, instance.ID)
		} else {
			w.instances[instance.ID] = instance
		}
		result := make([]*kratosregistry.ServiceInstance, 0, len(w.instances))
		for _, current := range w.instances {
			result = append(result, current)
		}
		return result, nil
	}
}

func (w *kratosWatcher) Stop() error {
	w.cancel()
	return nil
}

func fromKratosInstance(instance *kratosregistry.ServiceInstance) Instance {
	address := ""
	if len(instance.Endpoints) > 0 {
		address = instance.Endpoints[0]
	}
	return Instance{Service: instance.Name, ID: instance.ID, Address: address, Version: instance.Version}
}

func toKratosInstance(instance Instance) *kratosregistry.ServiceInstance {
	return &kratosregistry.ServiceInstance{
		ID: instance.ID, Name: instance.Service, Version: instance.Version,
		Endpoints: []string{instance.Address},
		Metadata:  map[string]string{"environment": instance.Environment},
	}
}
