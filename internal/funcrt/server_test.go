package funcrt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServer runs a Server over a started echo pool on random ports.
func startServer(t *testing.T, cfg PoolConfig) (base, admin string, p *Pool) {
	t.Helper()
	p = startPool(t, cfg)
	var s *Server
	var err error
	for range 20 {
		port, ap := 20000+rand.IntN(30000), 20000+rand.IntN(30000)
		s = NewServer(p, port, ap)
		s.preStop = 0
		if err = s.listen(); err == nil {
			base, admin = fmt.Sprintf("http://127.0.0.1:%d", port), fmt.Sprintf("http://127.0.0.1:%d", ap)
			break
		}
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
	return base, admin, p
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerEndToEnd(t *testing.T) {
	base, admin, p := startServer(t, PoolConfig{})
	waitFor(t, 10*time.Second, "ready", func() bool {
		c, _ := get(t, admin+"/readyz")
		return c == 200 && p.Ready()
	})
	if c, _ := get(t, admin+"/healthz"); c != 200 {
		t.Errorf("healthz = %d", c)
	}
	code, body := get(t, base+"/p")
	if code != 200 || !strings.Contains(body, "/p") {
		t.Fatalf("got %d %q", code, body)
	}
	_, m := get(t, admin+"/metrics")
	for _, want := range []string{`funcrt_invocations_total{status="200"} 1`, "funcrt_inflight", "funcrt_queued", "funcrt_invocation_duration_seconds_count 1"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestServerBodyTooLarge(t *testing.T) {
	base, _, _ := startServer(t, PoolConfig{})
	resp, err := http.Post(base+"/", "application/octet-stream", io.LimitReader(zeroReader{}, MaxBodyBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// On shutdown the server must drain in-flight requests while the pool is still
// running (cmd/funcrt stops the pool only after Run returns).
func TestServerShutdownDrainsInFlight(t *testing.T) {
	p := startPool(t, PoolConfig{Timeout: 10 * time.Second})
	var s *Server
	var base string
	for range 20 {
		port := 20000 + rand.IntN(30000)
		s = NewServer(p, port, 20000+rand.IntN(30000))
		s.preStop = 0
		if err := s.listen(); err == nil {
			base = fmt.Sprintf("http://127.0.0.1:%d", port)
			break
		}
		s = nil
	}
	if s == nil {
		t.Fatal("listen failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx) }()
	waitFor(t, 10*time.Second, "ready", p.Ready)

	type result struct {
		code int
		body string
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/sleep/1s")
		if err != nil {
			res <- result{body: err.Error()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		res <- result{resp.StatusCode, string(b)}
	}()
	waitFor(t, 5*time.Second, "request in flight", func() bool { return p.inFlight.Load() == 1 })
	cancel()

	r := <-res
	if r.code != 200 || r.body != "/sleep/1s" {
		t.Fatalf("in-flight request during shutdown: %d %q", r.code, r.body)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// R17: a handler error (/invocation/error) is a generic 500 to the client; the
// error document is logged at warn with the request id.
func TestServerHandlerErrorIs500Generic(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	base, _, p := startServer(t, PoolConfig{})
	waitFor(t, 10*time.Second, "ready", p.Ready)
	resp, err := http.Get(base + "/error/secret-detail")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 500 || resp.Header.Get("Content-Type") != "application/json" || string(b) != `{"message":"Internal Server Error"}` {
		t.Errorf("got %d %q %q; want 500 application/json generic body", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	var entry struct {
		Level, Msg, RequestID, Error string
	}
	line := logs.String()
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &entry); err != nil {
		t.Fatalf("log %q: %v", line, err)
	}
	if entry.Level != "WARN" || entry.RequestID == "" || !strings.Contains(entry.Error, "secret-detail") {
		t.Errorf("log entry = %+v; want WARN with requestId and the error document", entry)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServerShutdownTiming(t *testing.T) {
	s := NewServer(NewPool(PoolConfig{Timeout: 45 * time.Second}), 1, 2)
	if s.preStop != 5*time.Second || s.drain != 47*time.Second {
		t.Errorf("preStop=%v drain=%v; want 5s and Timeout+2s = 47s", s.preStop, s.drain)
	}
}

// startService runs runService on random ports with the echo bootstrap.
func startService(t *testing.T, timeout, preStop time.Duration) (base string, cancel func(), done <-chan error) {
	t.Helper()
	for range 20 {
		cfg := Config{
			Mode: ModeService, Bootstrap: echoBootstrap, Concurrency: 1, Timeout: timeout,
			Port: 20000 + rand.IntN(30000), AdminPort: 20000 + rand.IntN(30000), BasePort: 20000 + rand.IntN(30000),
			FunctionName: "echo",
		}
		ctx, c := context.WithCancel(context.Background())
		ch := make(chan error, 1)
		go func() { ch <- runService(ctx, cfg, nil, preStop); close(ch) }()
		base = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
		admin := fmt.Sprintf("http://127.0.0.1:%d/readyz", cfg.AdminPort)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-ch:
				c()
				t.Logf("runService exited early (port clash?): %v", err)
				goto retry
			default:
			}
			if resp, err := http.Get(admin); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					t.Cleanup(func() { c(); <-ch })
					return base, c, ch
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		c()
		t.Fatal("service not ready")
	retry:
	}
	t.Fatal("could not start service")
	return "", nil, nil
}

type httpResult struct {
	code int
	body string
	err  error
}

func getAsync(url string) <-chan httpResult {
	ch := make(chan httpResult, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			ch <- httpResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		ch <- httpResult{code: resp.StatusCode, body: string(b)}
	}()
	return ch
}

// Service mode keeps serving through the pre-stop window after SIGTERM, then
// drains an in-flight request for up to Timeout+2s (not a fixed grace) and
// exits cleanly; the bootstrap is stopped only afterwards.
func TestRunServiceGracefulShutdown(t *testing.T) {
	base, cancel, done := startService(t, 5*time.Second, 500*time.Millisecond)

	long := getAsync(base + "/sleep/3s")
	time.Sleep(200 * time.Millisecond) // let it reach the bootstrap
	shutdownAt := time.Now()
	cancel()

	// Still served during the pre-stop window (it queues behind the long one).
	during := getAsync(base + "/during")
	if r := <-long; r.err != nil || r.code != 200 || r.body != "/sleep/3s" {
		t.Fatalf("in-flight request across shutdown: %+v", r)
	}
	if time.Since(shutdownAt) < 2*time.Second {
		t.Errorf("in-flight request finished %v after shutdown began; expected ~2.8s", time.Since(shutdownAt))
	}
	if r := <-during; r.err != nil || r.code != 200 || r.body != "/during" {
		t.Errorf("request during pre-stop window: %+v", r)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runService after a completed drain = %v; want nil (exit 0)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runService did not return after drain")
	}
}
