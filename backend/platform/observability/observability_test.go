package observability

import (
	"context"
	"testing"
)

func TestFilteredArgs(t *testing.T) {
	got := filteredArgs("request_id", "r1", "token", "secret", "password=", "pw")
	if got[3] != "[REDACTED]" || got[5] != "[REDACTED]" {
		t.Fatalf("filtered args = %#v", got)
	}
}

func TestInitWithoutEndpoint(t *testing.T) {
	providers, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if providers.Tracer == nil || providers.Meter == nil {
		t.Fatal("expected noop providers")
	}
	if err := providers.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
