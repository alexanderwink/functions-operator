package funcrt

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
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
