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
	env := funcrt.BootstrapEnv(os.Environ())

	if cfg.Mode != funcrt.ModeJob {
		if err := funcrt.RunService(ctx, cfg, env); err != nil {
			slog.Error("server", "error", err)
			return 1
		}
		return 0
	}

	pool := funcrt.NewPool(funcrt.PoolConfig{
		Bootstrap:    cfg.Bootstrap,
		Concurrency:  cfg.Concurrency,
		Timeout:      cfg.Timeout,
		BasePort:     cfg.BasePort,
		Env:          env,
		FunctionName: cfg.FunctionName,
	})
	poolCtx, stopPool := context.WithCancel(ctx)
	if err := pool.Start(poolCtx); err != nil {
		stopPool()
		slog.Error("starting pool", "error", err)
		return 1
	}
	defer func() {
		stopPool()
		pool.Wait()
	}()
	payload := []byte(cfg.Payload)
	if len(payload) == 0 {
		payload = funcrt.ScheduledEvent(time.Now(), eventID())
	}
	if err := funcrt.RunJob(ctx, pool, payload); err != nil {
		slog.Error("job failed", "error", err)
		return 1
	}
	slog.Info("job succeeded")
	return 0
}

func eventID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
