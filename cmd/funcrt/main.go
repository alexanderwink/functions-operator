// Command funcrt runs N copies of an unmodified AWS Lambda bootstrap in one
// pod, either as an HTTP service or as a one-shot cron job.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexanderwink/functions-operator/internal/funcrt"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	os.Exit(run())
}

func run() int {
	cfg, err := funcrt.LoadConfig(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool := funcrt.NewPool(funcrt.PoolConfig{
		Bootstrap:    cfg.Bootstrap,
		Concurrency:  cfg.Concurrency,
		Timeout:      cfg.Timeout,
		BasePort:     cfg.BasePort,
		Env:          funcrt.BootstrapEnv(os.Environ()),
		FunctionName: cfg.FunctionName,
	})
	// In service mode the pool outlives the signal: on SIGTERM the server stops
	// accepting and drains in-flight requests first, and only then are the
	// bootstrap copies stopped. Sharing ctx would kill the copies immediately and
	// answer every in-flight request with 502 during rollouts and scale-in.
	poolCtx, stopPool := context.WithCancel(context.Background())
	if cfg.Mode == funcrt.ModeJob {
		poolCtx, stopPool = context.WithCancel(ctx)
	}
	if err := pool.Start(poolCtx); err != nil {
		stopPool()
		slog.Error("starting pool", "error", err)
		return 1
	}
	defer func() {
		stopPool()
		pool.Wait()
	}()

	switch cfg.Mode {
	case funcrt.ModeJob:
		payload := []byte(cfg.Payload)
		if len(payload) == 0 {
			payload = funcrt.ScheduledEvent(time.Now(), eventID())
		}
		err := funcrt.RunJob(ctx, pool, payload)
		stopPool()
		if err != nil {
			slog.Error("job failed", "error", err)
			return 1
		}
		slog.Info("job succeeded")
		return 0
	default:
		if err := funcrt.NewServer(pool, cfg.Port, cfg.AdminPort).Run(ctx); err != nil {
			slog.Error("server", "error", err)
			return 1
		}
		return 0
	}
}

func eventID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
