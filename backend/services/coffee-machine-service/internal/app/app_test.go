package app

import (
	"errors"
	"testing"
)

func TestRunWorkerReturnsNoProvider(t *testing.T) {
	if err := RunWorker("coffee-machine-service"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("RunWorker() error = %v, want %v", err, ErrNoProvider)
	}
}
