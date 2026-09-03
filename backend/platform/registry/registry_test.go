package registry

import (
	"encoding/json"
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
