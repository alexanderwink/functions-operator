package funcrt

import (
	"context"
	"fmt"
	"time"
)

// RunService runs funcrt in service mode until ctx is done (SIGTERM), then
// shuts down gracefully: the server keeps serving through the pre-stop window
// (PreStopDelay), drains in-flight requests for up to DrainTimeout(cfg.Timeout),
// and only then are the bootstrap copies stopped. It returns nil when the
// drain completes.
func RunService(ctx context.Context, cfg Config, env []string) error {
	return runService(ctx, cfg, env, PreStopDelay)
}

func runService(ctx context.Context, cfg Config, env []string, preStop time.Duration) error {
	pool := NewPool(PoolConfig{
		Bootstrap:    cfg.Bootstrap,
		Concurrency:  cfg.Concurrency,
		Timeout:      cfg.Timeout,
		BasePort:     cfg.BasePort,
		Env:          env,
		FunctionName: cfg.FunctionName,
	})
	// The pool outlives ctx: sharing it would kill the copies at SIGTERM and
	// answer every in-flight request with 502 during rollouts and scale-in.
	poolCtx, stopPool := context.WithCancel(context.Background())
	defer func() {
		stopPool()
		pool.Wait()
	}()
	if err := pool.Start(poolCtx); err != nil {
		return fmt.Errorf("starting pool: %w", err)
	}
	s := NewServer(pool, cfg.Port, cfg.AdminPort)
	s.preStop = preStop
	return s.Run(ctx)
}
