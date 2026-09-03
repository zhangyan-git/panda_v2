package cache

import (
	"context"
	"testing"
)

func TestNewFromEnvRejectsInvalidDB(t *testing.T) {
	t.Setenv("REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("REDIS_DB", "invalid")
	if _, err := NewFromEnv(context.Background()); err == nil {
		t.Fatal("expected invalid REDIS_DB error")
	}
}

func TestNewFromEnvEmptyDBDefaultsToZero(t *testing.T) {
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_DB", "")
	client, err := NewFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(Noop); !ok {
		t.Fatalf("client type = %T, want Noop", client)
	}
}
