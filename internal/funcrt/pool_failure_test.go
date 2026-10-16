package funcrt

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitRestarted waits until worker 0's process has exited at least n times and
// a new process has polled /next.
func waitRestarted(t *testing.T, p *Pool, n int64) {
	t.Helper()
	w := p.workers[0]
	waitFor(t, 5*time.Second, "bootstrap exit", func() bool { return w.exits.Load() >= n })
	waitFor(t, 5*time.Second, "Ready() after restart", p.Ready)
}

func TestPoolTimeoutKillsAndRecovers(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 300 * time.Millisecond})
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

	start := time.Now()
	r := await(t, invokeAsync(p, `{"sleep":"5s"}`), 3*time.Second, "sleeping invocation")
	if r.Status != 504 || errorType(t, r) != "Function.TimedOut" {
		t.Errorf("status = %d, payload %s; want 504 Function.TimedOut", r.Status, r.Payload)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("timed-out invocation returned after %v; want < 1s", elapsed)
	}

	// The runaway process must be killed (not left sleeping for 5s) and
	// replaced by a fresh one that can serve.
	waitRestarted(t, p, 1)
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Errorf("worker recovered after %v; the sleeping process was not killed", elapsed)
	}
	if r := invoke(t, p, `{"echo":1}`); r.Status != 200 || strings.TrimSpace(string(r.Payload)) != "1" {
		t.Errorf("after timeout: status = %d, payload %s; want 200, 1", r.Status, r.Payload)
	}
	if f := p.InFlight(); f != 0 {
		t.Errorf("InFlight() = %d; want 0", f)
	}
}

func TestPoolCrashReturns502(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

	start := time.Now()
	r := await(t, invokeAsync(p, `{"exit":1}`), 3*time.Second, "crashing invocation")
	if r.Status != 502 || errorType(t, r) != "Runtime.ExitError" {
		t.Errorf("status = %d, payload %s; want 502 Runtime.ExitError", r.Status, r.Payload)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Errorf("crash reported after %v; want immediately, not at the deadline", elapsed)
	}

	if r := invoke(t, p, `{"echo":1}`); r.Status != 200 || strings.TrimSpace(string(r.Payload)) != "1" {
		t.Errorf("after crash: status = %d, payload %s; want 200, 1", r.Status, r.Payload)
	}
}

func TestPoolInitFailureNotReady(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: time.Second, Env: []string{"ECHO_INIT_FAIL=1"}})
	w := p.workers[0]

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p.Ready() {
			t.Fatal("Ready() = true for a bootstrap that fails init")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Backoff 100ms, 200ms, 400ms, 800ms: starts at ~0, 0.1, 0.3, 0.7s. A fixed
	// 100ms delay would give ~10.
	if n := w.starts.Load(); n < 2 || n > 5 {
		t.Errorf("%d process starts in 1s; want 2..5 with exponential backoff", n)
	}
}

func TestPoolQueuedRequestTimesOut(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 300 * time.Millisecond})
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

	first := invokeAsync(p, `{"sleep":"5s"}`)
	waitFor(t, time.Second, "first invocation in flight", func() bool { return p.InFlight() == 1 })
	start := time.Now()
	second := await(t, invokeAsync(p, `{"echo":1}`), 3*time.Second, "queued invocation")
	if second.Status != 504 {
		t.Errorf("queued: status = %d, payload %s; want 504", second.Status, second.Payload)
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Errorf("queued invocation returned after %v; want ~300ms", elapsed)
	}
	if r := await(t, first, 3*time.Second, "first invocation"); r.Status != 504 {
		t.Errorf("first: status = %d; want 504", r.Status)
	}
}

// A bootstrap that reports /init/error is killed and restarted, and never
// counts toward Ready() even if it goes on to poll /next.
func TestPoolInitErrorRestarts(t *testing.T) {
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: time.Second})
	w := p.workers[0]
	waitFor(t, 5*time.Second, "first process start", func() bool { return w.starts.Load() >= 1 })

	resp, err := http.Post(runtimeURL(p, "/init/error"), "application/json",
		strings.NewReader(`{"errorMessage":"bad config","errorType":"InitError"}`))
	if err != nil {
		t.Fatalf("posting init error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("/init/error status = %d; want 202", resp.StatusCode)
	}
	waitFor(t, 2*time.Second, "process killed after /init/error", func() bool { return w.exits.Load() >= 1 })
	waitFor(t, 2*time.Second, "process restarted", func() bool { return w.starts.Load() >= 2 })
	if p.Ready() {
		t.Error("Ready() = true after init error")
	}
}

func TestWorkerBackoff(t *testing.T) {
	w := &worker{}
	want := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 10 * time.Second, 10 * time.Second,
	}
	for i, d := range want {
		if got := w.nextBackoff(); got != d {
			t.Errorf("backoff %d = %v; want %v", i, got, d)
		}
	}
	w.resetBackoff()
	if got := w.nextBackoff(); got != 100*time.Millisecond {
		t.Errorf("after reset: backoff = %v; want 100ms", got)
	}
}

// A successful invocation resets the restart backoff.
func TestPoolBackoffResetsAfterSuccess(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})
	w := p.workers[0]
	for i := range 3 {
		if r := invoke(t, p, `{"exit":1}`); r.Status != 502 {
			t.Fatalf("crash %d: status = %d; want 502", i, r.Status)
		}
	}
	if r := invoke(t, p, `{"echo":1}`); r.Status != 200 {
		t.Fatalf("echo: status = %d; want 200", r.Status)
	}
	if got := w.nextBackoff(); got != 100*time.Millisecond {
		t.Errorf("backoff after success = %v; want 100ms", got)
	}
}

// A handled function error (/invocation/error) also resets the restart
// backoff: the runtime is healthy (ruling R9).
func TestPoolBackoffResetsAfterHandledError(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})
	w := p.workers[0]
	for i := range 3 {
		if r := invoke(t, p, `{"exit":1}`); r.Status != 502 {
			t.Fatalf("crash %d: status = %d; want 502", i, r.Status)
		}
	}
	if r := invoke(t, p, `{"error":"boom"}`); r.Status != 502 || errorType(t, r) == "Runtime.ExitError" {
		t.Fatalf("handled error: status = %d, payload %s; want 502 handler error", r.Status, r.Payload)
	}
	if got := w.nextBackoff(); got != 100*time.Millisecond {
		t.Errorf("backoff after handled error = %v; want 100ms", got)
	}
}

// A timeout kill resets the restart backoff: a timed-out copy is not
// crash-looping, so it is restarted after the minimum delay.
func TestPoolBackoffResetsAfterTimeoutKill(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 300 * time.Millisecond})
	w := p.workers[0]
	for i := range 3 {
		if r := invoke(t, p, `{"exit":1}`); r.Status != 502 {
			t.Fatalf("crash %d: status = %d; want 502", i, r.Status)
		}
	}
	// Backoff is now 800ms (100, 200, 400 consumed).
	waitFor(t, 5*time.Second, "Ready() after crashes", p.Ready)
	if r := invoke(t, p, `{"sleep":"5s"}`); r.Status != 504 {
		t.Fatalf("sleep: status = %d; want 504", r.Status)
	}
	start := time.Now()
	waitRestarted(t, p, 4)
	if elapsed := time.Since(start); elapsed >= 700*time.Millisecond {
		t.Errorf("restart after timeout kill took %v; want the minimum delay, not 800ms", elapsed)
	}
	// Reset to 0, then the restart consumed minRestartDelay: next is 200ms.
	if got := w.nextBackoff(); got != 200*time.Millisecond {
		t.Errorf("backoff after timeout kill and restart = %v; want 200ms", got)
	}
}
