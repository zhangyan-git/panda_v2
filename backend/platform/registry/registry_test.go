package registry

import (
	"encoding/json"
	"net/url"
	"testing"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestSplitEndpoints(t *testing.T) {
	got := splitEndpoints(" http://one:2379, ,http://two:2379 ")
	want := []string{"http://one:2379", "http://two:2379"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("splitEndpoints() = %v, want %v", got, want)
	}
}

func TestDecodeEventDeleteUsesPreviousValue(t *testing.T) {
	instance := Instance{Service: "orders", ID: "one", Address: "grpc://127.0.0.1:9000"}
	value, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeEvent(&clientv3.Event{
		Type:   clientv3.EventTypeDelete,
		Kv:     &mvccpb.KeyValue{Key: []byte("/panda/services/orders/one")},
		PrevKv: &mvccpb.KeyValue{Value: value},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != instance {
		t.Fatalf("decodeEvent() = %+v, want %+v", got, instance)
	}
}

func TestServicePrefix(t *testing.T) {
	if got, want := servicePrefix("orders"), "/panda/services/orders/"; got != want {
		t.Fatalf("servicePrefix() = %q, want %q", got, want)
	}
}

func TestValidateInstanceRejectsInvalidPathSegments(t *testing.T) {
	tests := []Instance{
		{Service: "", ID: "one"},
		{Service: "orders/other", ID: "one"},
		{Service: "orders", ID: "../one"},
		{Service: "orders", ID: " one"},
	}
	for _, instance := range tests {
		if err := validateInstance(instance); err == nil {
			t.Errorf("validateInstance(%+v) = nil, want error", instance)
		}
	}
}

func TestDecodeEventRequiresPreviousValueForDelete(t *testing.T) {
	_, err := decodeEvent(&clientv3.Event{
		Type: clientv3.EventTypeDelete,
		Kv:   &mvccpb.KeyValue{Key: []byte("/panda/services/orders/one")},
	})
	if err == nil {
		t.Fatal("decodeEvent() = nil error, want previous value error")
	}
}

func TestDecodeEventRequiresKeyValue(t *testing.T) {
	_, err := decodeEvent(nil)
	if err == nil {
		t.Fatal("decodeEvent(nil) = nil error, want error")
	}
}

// Kratos hands back a full URL. Building the ID from the whole string yields
// "orders-grpc://127.0.0.1:9000", whose "//" fails the path-segment check and
// aborts startup the moment a real registry is configured. The ID must come
// from the host so it stays one etcd key segment.
func TestNewInstanceDerivesIDFromEndpointHost(t *testing.T) {
	endpoint, err := url.Parse("grpc://127.0.0.1:19081")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := NewInstance("orders", endpoint, "1.2.3", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if want := "orders-127.0.0.1:19081"; instance.ID != want {
		t.Fatalf("ID = %q, want %q", instance.ID, want)
	}
	if want := "grpc://127.0.0.1:19081"; instance.Address != want {
		t.Fatalf("Address = %q, want %q", instance.Address, want)
	}
	if instance.Version != "1.2.3" || instance.Environment != "dev" {
		t.Fatalf("instance = %+v", instance)
	}
	if got, want := instanceKey(instance), "/panda/services/orders/orders-127.0.0.1:19081"; got != want {
		t.Fatalf("instanceKey() = %q, want %q", got, want)
	}
}

// Replicas of one service differ only by port, so the ID has to keep the port
// or the second replica would overwrite the first one's key.
func TestNewInstanceSeparatesReplicas(t *testing.T) {
	first, err := url.Parse("grpc://10.0.0.1:9090")
	if err != nil {
		t.Fatal(err)
	}
	second, err := url.Parse("grpc://10.0.0.2:9090")
	if err != nil {
		t.Fatal(err)
	}
	one, err := NewInstance("orders", first, "1.2.3", "dev")
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewInstance("orders", second, "1.2.3", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID {
		t.Fatalf("both replicas registered as %q", one.ID)
	}
}

func TestNewInstanceRejectsEndpointWithoutHost(t *testing.T) {
	for _, endpoint := range []*url.URL{nil, {Scheme: "grpc"}} {
		if _, err := NewInstance("orders", endpoint, "1.2.3", "dev"); err == nil {
			t.Fatalf("NewInstance(%v) = nil error, want error", endpoint)
		}
	}
}

func TestNewInstanceRejectsServiceNameThatIsNotOneSegment(t *testing.T) {
	endpoint, err := url.Parse("grpc://127.0.0.1:19081")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewInstance("orders/v2", endpoint, "1.2.3", "dev"); err == nil {
		t.Fatal("NewInstance(orders/v2) = nil error, want error")
	}
}
