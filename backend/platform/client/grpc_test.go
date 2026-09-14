package client

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	platformregistry "github.com/panda-dev/panda-v2/backend/platform/registry"
)

const testTimeout = 2 * time.Second

// discoveryRegistry stands in for the etcd adapter: it really implements both
// optional interfaces, which is the only thing Dial keys its decision on.
type discoveryRegistry struct{ platformregistry.Noop }

func (discoveryRegistry) Resolve(context.Context, string) ([]platformregistry.Instance, error) {
	return nil, nil
}

func (discoveryRegistry) Watch(context.Context, string) (<-chan platformregistry.Event, error) {
	return make(chan platformregistry.Event), nil
}

// resolverOnly implements half of discovery. Dial must not treat it as
// discovery-capable: a resolver with no watch would freeze the endpoint list at
// first resolution and never notice a replica coming or going.
type resolverOnly struct{ platformregistry.Noop }

func (resolverOnly) Resolve(context.Context, string) ([]platformregistry.Instance, error) {
	return nil, nil
}

func TestSupportsDiscoveryRequiresBothInterfaces(t *testing.T) {
	if supportsDiscovery(platformregistry.Noop{}) {
		t.Fatal("Noop reported as discovery-capable")
	}
	if supportsDiscovery(resolverOnly{}) {
		t.Fatal("a Resolver without Watcher reported as discovery-capable")
	}
	if !supportsDiscovery(discoveryRegistry{}) {
		t.Fatal("a Resolver+Watcher adapter reported as not discovery-capable")
	}
	if supportsDiscovery(nil) {
		t.Fatal("nil adapter reported as discovery-capable")
	}
}

func TestDialFallsBackToStaticAddress(t *testing.T) {
	// The address is deliberately unroutable: dialing is lazy, so a static
	// dial must still succeed. If it ever needs a live peer, that is a change
	// in behaviour worth failing on.
	conn, err := Dial(context.Background(), "merchant-service", "127.0.0.1:1", testTimeout, platformregistry.Noop{})
	if err != nil {
		t.Fatalf("Dial with a static address: %v", err)
	}
	defer func() { _ = conn.Close() }()
}

func TestDialRequiresStaticAddressWithoutDiscovery(t *testing.T) {
	for _, address := range []string{"", "   "} {
		if _, err := Dial(context.Background(), "merchant-service", address, testTimeout, platformregistry.Noop{}); err == nil {
			t.Fatalf("Dial accepted address %q with no discovery", address)
		}
	}
}

func TestDialRequiresServiceNameForDiscovery(t *testing.T) {
	_, err := Dial(context.Background(), "  ", "127.0.0.1:1", testTimeout, discoveryRegistry{})
	if err == nil || !strings.Contains(err.Error(), "service name") {
		t.Fatalf("Dial = %v, want a missing service name error", err)
	}
}

func TestDialUsesDiscoveryWhenSupported(t *testing.T) {
	// The static address is empty on purpose: the static branch would reject it,
	// so reaching a connection proves the discovery branch was taken.
	conn, err := Dial(context.Background(), "merchant-service", "", testTimeout, discoveryRegistry{})
	if err != nil {
		t.Fatalf("Dial with discovery: %v", err)
	}
	defer func() { _ = conn.Close() }()
}

func TestDialDiscoveryRejectsAdapterWithoutDiscovery(t *testing.T) {
	for name, adapter := range map[string]platformregistry.Registry{
		"noop":         platformregistry.Noop{},
		"resolverOnly": resolverOnly{},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DialDiscovery(context.Background(), "discovery:///merchant-service", testTimeout, adapter); !errors.Is(err, ErrDiscoveryUnavailable) {
				t.Fatalf("DialDiscovery = %v, want ErrDiscoveryUnavailable", err)
			}
		})
	}
}
