package funcrt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambda/messages"
)

var (
	// echoBootstrap is the path to the compiled testdata/echo handler.
	echoBootstrap string
	// silentBootstrap never talks to the Runtime API; tests drive the API
	// themselves while it sleeps.
	silentBootstrap string
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// Restart, timeout and init-failure paths log by design; keep test
	// output readable.
	slog.SetDefault(slog.New(slog.DiscardHandler))

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

	silentBootstrap = filepath.Join(dir, "silent")
	if err := os.WriteFile(silentBootstrap, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "writing silent bootstrap:", err)
		return 1
	}
	return m.Run()
}

// startPool starts a pool on a random free port range and stops it when the
// test ends. cfg.Bootstrap and cfg.BasePort are filled in if unset.
func startPool(t *testing.T, cfg PoolConfig) *Pool {
	t.Helper()
	p, _ := startPoolWithStop(t, cfg)
	return p
}

// startPoolWithStop is startPool that also returns a function stopping the
// pool and waiting for it to shut down (safe to call more than once).
func startPoolWithStop(t *testing.T, cfg PoolConfig) (*Pool, func()) {
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
		stop := func() {
			cancel()
			p.Wait()
		}
		t.Cleanup(stop)
		return p, stop
	}
	t.Fatalf("starting pool: %v", lastErr)
	return nil, nil
}

// waitFor polls cond until it is true, failing the test after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// invokeAsync runs Invoke with context.Background in a goroutine.
func invokeAsync(p *Pool, payload string) <-chan Result {
	ch := make(chan Result, 1)
	go func() { ch <- p.Invoke(context.Background(), []byte(payload)) }()
	return ch
}

// await returns the result from ch, failing the test after timeout.
func await(t *testing.T, ch <-chan Result, timeout time.Duration, what string) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(timeout):
		t.Fatalf("%s: no result after %v", what, timeout)
		return Result{}
	}
}

func runtimeURL(p *Pool, path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s%s", p.workers[0].port, runtimeAPIPrefix, path)
}

// pollNext plays the bootstrap: it long-polls /next and sends the request id
// (or "" on failure) to the returned channel.
func pollNext(p *Pool) <-chan string {
	ch := make(chan string, 1)
	go func() {
		resp, err := http.Get(runtimeURL(p, "/invocation/next"))
		if err != nil {
			ch <- ""
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		ch <- resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
	}()
	return ch
}

func awaitID(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		if id == "" {
			t.Fatal("/next failed")
		}
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("/next returned nothing after 5s")
		return ""
	}
}

func postResponse(t *testing.T, p *Pool, id, body string) int {
	t.Helper()
	resp, err := http.Post(runtimeURL(p, "/invocation/"+id+"/response"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("posting response: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func errorType(t *testing.T, r Result) string {
	t.Helper()
	var lerr messages.InvokeResponse_Error
	if err := json.Unmarshal(r.Payload, &lerr); err != nil {
		t.Fatalf("payload %s is not Lambda error JSON: %v", r.Payload, err)
	}
	return lerr.Type
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
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

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
	if res.Status != 500 {
		t.Fatalf("status = %d, payload %s; want 500 (R17)", res.Status, res.Payload)
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
	waitFor(t, 5*time.Second, "Ready() after Start", p.Ready)
}

func TestPoolInvokeHonoursContextWhileQueued(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 5 * time.Second})

	// Occupy the only worker.
	busy := make(chan Result, 1)
	go func() { busy <- invoke(t, p, `{"sleep":"1s"}`) }()
	waitFor(t, 5*time.Second, "InFlight() > 0", func() bool { return p.InFlight() > 0 })

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
	inv := newInvocation(context.Background(), []byte(`{}`), time.Now().Add(time.Second))
	inv.deadline = time.UnixMilli(1700000000123)
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

// A dispatched invocation whose bootstrap never answers must still return 504
// at its deadline, even when the caller's context never ends.
func TestPoolDispatchedNoAnswerTimesOut(t *testing.T) {
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: 300 * time.Millisecond})

	next := pollNext(p)
	start := time.Now()
	res := invokeAsync(p, `{"echo":1}`)
	awaitID(t, next)
	r := await(t, res, 3*time.Second, "unanswered invocation")
	if r.Status != 504 {
		t.Errorf("status = %d, payload %s; want 504", r.Status, r.Payload)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %v; want ~300ms", elapsed)
	}
	if f := p.InFlight(); f != 0 {
		t.Errorf("InFlight() = %d; want 0", f)
	}
}

// An invocation queued behind a busy worker gets 504 at its deadline.
func TestPoolQueuedDeadline(t *testing.T) {
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: 300 * time.Millisecond})
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

	first := invokeAsync(p, `{"sleep":"1s"}`)
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

// Two /next polls on one worker (e.g. a stale poll from a killed process and
// a fresh one) must not silently drop an invocation: the one replaced is
// completed with 502 Runtime.NoResponse, the other can still be answered.
func TestPoolConcurrentNextDoesNotOrphan(t *testing.T) {
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: 5 * time.Second})

	nextA, nextB := pollNext(p), pollNext(p)
	// Let both polls block in take before anything is queued, so each grabs
	// an invocation without seeing the other's.
	time.Sleep(200 * time.Millisecond)
	resA := invokeAsync(p, `{"echo":"a"}`)
	resB := invokeAsync(p, `{"echo":"b"}`)
	ids := []string{awaitID(t, nextA), awaitID(t, nextB)}

	// Exactly one invocation was replaced and must fail promptly.
	var replaced Result
	var survivor <-chan Result
	select {
	case replaced = <-resA:
		survivor = resB
	case replaced = <-resB:
		survivor = resA
	case <-time.After(time.Second):
		t.Fatal("neither invocation completed after being replaced")
	}
	if replaced.Status != 502 || errorType(t, replaced) != "Runtime.NoResponse" {
		t.Errorf("replaced: status = %d, payload %s; want 502 Runtime.NoResponse", replaced.Status, replaced.Payload)
	}

	accepted := 0
	for _, id := range ids {
		if postResponse(t, p, id, `"ok"`) == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("%d responses accepted; want exactly 1 (the current invocation)", accepted)
	}
	if r := await(t, survivor, time.Second, "surviving invocation"); r.Status != 200 || string(r.Payload) != `"ok"` {
		t.Errorf("survivor: status = %d, payload %s; want 200 \"ok\"", r.Status, r.Payload)
	}
	waitFor(t, time.Second, "InFlight() == 0", func() bool { return p.InFlight() == 0 })
}

// If take receives an invocation although its poll is already gone (select
// chooses randomly), it must hand the invocation back rather than drop it.
func TestWorkerTakeRequeuesWhenPollGone(t *testing.T) {
	p := NewPool(PoolConfig{Timeout: 5 * time.Second})
	w := p.workers[0]
	res := invokeAsync(p, `{"echo":1}`)
	waitFor(t, time.Second, "Queued() == 1", func() bool { return p.Queued() == 1 })

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if inv, ok := w.take(gone); ok {
			t.Fatalf("take on a cancelled poll returned invocation %s", inv.id)
		}
	}

	ctx, cancelTake := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelTake()
	inv, ok := w.take(ctx)
	if !ok {
		t.Fatal("invocation lost: not available to a live poll")
	}
	inv.complete(Result{Status: 200, Payload: []byte("1")})
	if r := await(t, res, time.Second, "requeued invocation"); r.Status != 200 {
		t.Errorf("status = %d; want 200", r.Status)
	}
}

// Shutting the pool down completes an in-flight invocation with 502.
func TestPoolShutdownCompletesInFlight(t *testing.T) {
	p, stop := startPoolWithStop(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: 5 * time.Second})

	next := pollNext(p)
	res := invokeAsync(p, `{"echo":1}`)
	awaitID(t, next)
	stop()
	r := await(t, res, time.Second, "in-flight invocation at shutdown")
	if r.Status != 502 || errorType(t, r) != "Runtime.Shutdown" {
		t.Errorf("status = %d, payload %s; want 502 Runtime.Shutdown", r.Status, r.Payload)
	}
}

// R16: queue time does not count against execution time. With one copy and
// Timeout T, two concurrent invocations each running 0.6×T both succeed: the
// second waits ~0.6×T in the queue (< T) and then gets a full T to run.
func TestPoolQueueWaitDoesNotCountAgainstExecution(t *testing.T) {
	const timeout = time.Second
	p := startPool(t, PoolConfig{Concurrency: 1, Timeout: timeout})
	waitFor(t, 5*time.Second, "Ready()", p.Ready)

	a := invokeAsync(p, `{"sleep":"600ms"}`)
	b := invokeAsync(p, `{"sleep":"600ms"}`)
	for i, ch := range []<-chan Result{a, b} {
		if r := await(t, ch, 5*time.Second, "invocation"); r.Status != 200 {
			t.Errorf("invocation %d: status = %d, payload %s; want 200", i, r.Status, r.Payload)
		}
	}
	if n := p.workers[0].exits.Load(); n != 0 {
		t.Errorf("%d bootstrap exits; want 0 (nothing should be killed)", n)
	}
}

// R16: Lambda-Runtime-Deadline-Ms is dispatch time + Timeout, not Invoke + Timeout.
func TestPoolDeadlineHeaderCountsFromDispatch(t *testing.T) {
	const timeout = 2 * time.Second
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: timeout})

	res := invokeAsync(p, `{"echo":1}`)
	waitFor(t, time.Second, "Queued() == 1", func() bool { return p.Queued() == 1 })
	time.Sleep(500 * time.Millisecond) // queued for 0.5s before any poll

	dispatched := time.Now()
	resp, err := http.Get(runtimeURL(p, "/invocation/next"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	ms, err := strconv.ParseInt(resp.Header.Get("Lambda-Runtime-Deadline-Ms"), 10, 64)
	if err != nil {
		t.Fatalf("deadline header: %v", err)
	}
	if d := time.UnixMilli(ms).Sub(dispatched); d < timeout-100*time.Millisecond || d > timeout+100*time.Millisecond {
		t.Errorf("deadline is dispatch + %v; want dispatch + %v", d, timeout)
	}
	if postResponse(t, p, resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), `1`) != http.StatusAccepted {
		t.Error("response not accepted")
	}
	if r := await(t, res, time.Second, "invocation"); r.Status != 200 {
		t.Errorf("status = %d, payload %s; want 200", r.Status, r.Payload)
	}
}

// R16: an invocation that exceeds the queue-wait limit gets 504 without any
// process being killed.
func TestPoolQueueTimeoutKillsNothing(t *testing.T) {
	p := startPool(t, PoolConfig{Bootstrap: silentBootstrap, Timeout: 300 * time.Millisecond})
	w := p.workers[0]
	waitFor(t, 5*time.Second, "process start", func() bool { return w.starts.Load() >= 1 })

	r := await(t, invokeAsync(p, `{"echo":1}`), 3*time.Second, "queued invocation")
	if r.Status != 504 || errorType(t, r) != "Function.TimedOut" {
		t.Errorf("status = %d, payload %s; want 504 Function.TimedOut", r.Status, r.Payload)
	}
	time.Sleep(100 * time.Millisecond)
	if n := w.exits.Load(); n != 0 {
		t.Errorf("%d bootstrap exits after a queue timeout; want 0", n)
	}
	if q := p.Queued(); q != 0 {
		t.Errorf("Queued() = %d; want 0", q)
	}
}
