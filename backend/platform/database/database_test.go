package database

import (
	"context"
	"testing"
)

func TestNewWithoutURLReturnsNoop(t *testing.T) {
	pool, err := New(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.(Noop); !ok {
		t.Fatalf("pool type = %T, want Noop", pool)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool.Close()
}
