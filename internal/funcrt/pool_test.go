package funcrt

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambda/messages"
)

// echoBootstrap is the path to the compiled testdata/echo handler.
var echoBootstrap string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "funcrt-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdtemp:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	echoBootstrap = filepath.Join(dir, "bootstrap")
	build := exec.Command("go", "build", "-tags", "lambda.norpc", "-o", echoBootstrap, "./testdata/echo")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building testdata/echo:", err)
		return 1
	}
	return m.Run()
}

// startPool starts a pool on a random free port range and stops it when the
// test ends. cfg.Bootstrap and cfg.BasePort are filled in if unset.
func startPool(t *testing.T, cfg PoolConfig) *Pool {
	t.Helper()
	if cfg.Bootstrap == "" {
		cfg.Bootstrap = echoBootstrap
	}
	if cfg.FunctionName == "" {
		cfg.FunctionName = "echo"
	}
	var lastErr error
	for range 20 {
		cfg.BasePort = 20000 + rand.IntN(30000)
		p := NewPool(cfg)
		ctx, cancel := context.WithCancel(context.Background())
		if err := p.Start(ctx); err != nil {
			cancel()
			lastErr = err
			continue
		}
		t.Cleanup(func() {
			cancel()
			p.Wait()
		})
		return p
	}
	t.Fatalf("starting pool: %v", lastErr)
	return nil
}

func invoke(t *testing.T, p *Pool, payload string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Invoke(ctx, []byte(payload))
}

func TestPoolEcho(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})

	res := invoke(t, p, `{"echo":"hi"}`)
	if res.Status != 200 {
		t.Fatalf("status = %d, payload %s; want 200", res.Status, res.Payload)
	}
	if got := strings.TrimSpace(string(res.Payload)); got != `"hi"` {
		t.Fatalf("payload = %s; want %q", got, `"hi"`)
	}
}

func TestPoolQueuesBeyondConcurrency(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 2, Timeout: 5 * time.Second})

	// Warm up so process start-up time does not inflate the measurement.
	for !p.Ready() {
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]Result, 6)
	for i := range results {
		wg.Go(func() { results[i] = invoke(t, p, `{"sleep":"200ms"}`) })
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, r := range results {
		if r.Status != 200 {
			t.Errorf("invocation %d: status = %d, payload %s; want 200", i, r.Status, r.Payload)
		}
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("6 × 200ms on 2 workers took %v; want ≥ 600ms", elapsed)
	}
	if q, f := p.Queued(), p.InFlight(); q != 0 || f != 0 {
		t.Errorf("after completion Queued=%d InFlight=%d; want 0, 0", q, f)
	}
}

func TestPoolHandlerError(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})

	res := invoke(t, p, `{"error":"boom"}`)
	if res.Status != 502 {
		t.Fatalf("status = %d, payload %s; want 502", res.Status, res.Payload)
	}
	if !strings.Contains(string(res.Payload), "boom") {
		t.Fatalf("payload %s does not contain %q", res.Payload, "boom")
	}
	var lerr messages.InvokeResponse_Error
	if err := json.Unmarshal(res.Payload, &lerr); err != nil {
		t.Fatalf("payload %s is not Lambda error JSON: %v", res.Payload, err)
	}
	if lerr.Message != "boom" {
		t.Errorf("errorMessage = %q; want %q", lerr.Message, "boom")
	}

	res = invoke(t, p, `{"echo":1}`)
	if res.Status != 200 || strings.TrimSpace(string(res.Payload)) != "1" {
		t.Fatalf("after error: status = %d, payload %s; want 200, 1", res.Status, res.Payload)
	}
}

func TestPoolReady(t *testing.T) {
	p := NewPool(PoolConfig{Bootstrap: echoBootstrap, Concurrency: 1, Timeout: time.Second, BasePort: 1})
	if p.Ready() {
		t.Fatal("Ready() = true before Start")
	}

	p = startPool(t, PoolConfig{Concurrency: 1, Timeout: time.Second})
	deadline := time.Now().Add(5 * time.Second)
	for !p.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("Ready() still false 5s after Start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPoolInvokeHonoursContextWhileQueued(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})

	// Occupy the only worker.
	busy := make(chan Result, 1)
	go func() { busy <- invoke(t, p, `{"sleep":"1s"}`) }()
	for p.InFlight() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := p.Invoke(ctx, []byte(`{"echo":1}`))
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("cancelled Invoke returned after %v; want prompt return", elapsed)
	}
	if res.Status != 504 {
		t.Errorf("status = %d, payload %s; want 504", res.Status, res.Payload)
	}
	if q := p.Queued(); q != 0 {
		t.Errorf("Queued() = %d after cancellation; want 0", q)
	}
	if r := <-busy; r.Status != 200 {
		t.Errorf("busy invocation status = %d; want 200", r.Status)
	}
}

func TestPoolNextHeaders(t *testing.T) {
	// End-to-end tests prove the headers work with lambda.Start; this pins
	// their exact values.
	w := &worker{pool: NewPool(PoolConfig{FunctionName: "my-fn", Timeout: time.Second})}
	inv := newInvocation(context.Background(), []byte(`{}`), time.UnixMilli(1700000000123))
	h := w.nextHeaders(inv)
	if got := h.Get("Lambda-Runtime-Aws-Request-Id"); got != inv.id || got == "" {
		t.Errorf("request id = %q; want %q", got, inv.id)
	}
	if got := h.Get("Lambda-Runtime-Deadline-Ms"); got != "1700000000123" {
		t.Errorf("deadline = %q; want 1700000000123", got)
	}
	if got, want := h.Get("Lambda-Runtime-Invoked-Function-Arn"), "arn:aws:lambda:local:000000000000:function:my-fn"; got != want {
		t.Errorf("arn = %q; want %q", got, want)
	}
}
