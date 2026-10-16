package funcrt

import (
	"context"
	"testing"
	"time"
)

func TestRunJobSuccess(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})
	if err := RunJob(context.Background(), p, []byte(`{"echo":1}`)); err != nil {
		t.Fatal(err)
	}
}

func TestRunJobHandlerError(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})
	if err := RunJob(context.Background(), p, []byte(`{"error":"x"}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestRunJobNotReady(t *testing.T) {
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Concurrency: 1, Timeout: 300 * time.Millisecond})
	if err := RunJob(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("expected not-ready error")
	}
}
