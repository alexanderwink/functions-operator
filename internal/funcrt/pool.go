// Package funcrt implements the AWS Lambda Runtime API for a pool of
// unmodified Lambda "bootstrap" binaries running inside one pod. It replaces
// the AWS Lambda Runtime Interface Emulator.
package funcrt

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults applied by NewPool for zero-valued config fields.
const (
	DefaultConcurrency = 1
	DefaultTimeout     = 3 * time.Second // AWS Lambda's default function timeout
)

// PoolConfig configures a Pool.
type PoolConfig struct {
	Bootstrap    string        // path to bootstrap binary
	Concurrency  int           // number of bootstrap copies; < 1 means DefaultConcurrency
	Timeout      time.Duration // per-invocation timeout, counted from Invoke; <= 0 means DefaultTimeout
	BasePort     int           // worker i listens on 127.0.0.1:BasePort+i
	Env          []string      // extra env for bootstrap (user env + AWS_LAMBDA_*)
	FunctionName string
}

// Result is the outcome of one invocation.
type Result struct {
	Status  int    // 200 ok, 502 handler error/crash, 504 timeout
	Payload []byte // response payload, or Lambda error JSON for 502/504
}

// Pool runs Concurrency bootstrap processes and dispatches invocations to
// whichever one polls /next first. All methods are safe for concurrent use.
type Pool struct {
	cfg     PoolConfig
	queue   chan *invocation // unbuffered: a send succeeds only when a worker takes it
	workers []*worker

	inFlight atomic.Int64
	queued   atomic.Int64

	started atomic.Bool
	wg      sync.WaitGroup
}

// NewPool returns a Pool for cfg. It does not start anything.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	p := &Pool{cfg: cfg, queue: make(chan *invocation)}
	for i := range cfg.Concurrency {
		p.workers = append(p.workers, &worker{pool: p, index: i, port: cfg.BasePort + i})
	}
	return p
}

// Start binds every worker's Runtime API listener and launches the bootstrap
// processes. It returns once all listeners are bound (an error means none are
// left open); processes start in the background. Cancelling ctx stops the
// listeners and kills the processes; Wait blocks until that has finished.
func (p *Pool) Start(ctx context.Context) error {
	if !p.started.CompareAndSwap(false, true) {
		return fmt.Errorf("funcrt: pool already started")
	}
	for i, w := range p.workers {
		if err := w.listen(); err != nil {
			for _, prev := range p.workers[:i] {
				prev.closeListener()
			}
			return err
		}
	}
	for _, w := range p.workers {
		p.wg.Go(func() { w.run(ctx) })
	}
	return nil
}

// Wait blocks until all workers have shut down after the Start context is
// cancelled. It returns immediately if the pool was never started.
func (p *Pool) Wait() { p.wg.Wait() }

// Invoke runs payload on the next idle worker and waits for the result.
//
// The invocation deadline is now+Timeout, so time spent queued counts against
// the timeout. If the deadline passes or ctx is done before a worker takes the
// invocation, it is never dispatched and Invoke returns 504 promptly. Once
// dispatched, Invoke returns 504 at the deadline (or as soon as ctx is done)
// even if the worker never answers. At the same deadline the worker answers
// 504 itself and kills and restarts the runaway process.
//
// Invoke alone maintains the Queued and InFlight gauges, so they cannot leak
// whatever happens to the invocation inside a worker.
func (p *Pool) Invoke(ctx context.Context, payload []byte) Result {
	deadline := time.Now().Add(p.cfg.Timeout)
	inv := newInvocation(ctx, payload, deadline)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	p.queued.Add(1)
	select {
	case p.queue <- inv:
		p.queued.Add(-1)
	case <-timer.C:
		p.queued.Add(-1)
		return timeoutResult("invocation timed out while queued")
	case <-ctx.Done():
		p.queued.Add(-1)
		return cancelledResult(ctx.Err())
	}

	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	select {
	case res := <-inv.result:
		return res
	case <-timer.C:
		return timeoutResult("invocation timed out")
	case <-ctx.Done():
		return cancelledResult(ctx.Err())
	}
}

// requeue hands inv back to the queue after a worker took it but could not
// run it (its /next poll or the worker went away). It gives up, completing inv
// with 504, at the deadline or when the caller's ctx is done.
func (p *Pool) requeue(inv *invocation) {
	go func() {
		timer := time.NewTimer(time.Until(inv.deadline))
		defer timer.Stop()
		select {
		case p.queue <- inv:
		case <-timer.C:
			inv.complete(timeoutResult("invocation timed out while queued"))
		case <-inv.ctx.Done():
			inv.complete(cancelledResult(inv.ctx.Err()))
		}
	}()
}

// Ready reports whether at least one worker's current bootstrap process has
// polled /next without reporting /init/error.
func (p *Pool) Ready() bool {
	for _, w := range p.workers {
		if w.ready.Load() {
			return true
		}
	}
	return false
}

// InFlight is the number of dispatched invocations whose Invoke is still waiting.
func (p *Pool) InFlight() int { return int(p.inFlight.Load()) }

// Queued is the number of invocations waiting for an idle worker.
func (p *Pool) Queued() int { return int(p.queued.Load()) }

// functionARN is the value of Lambda-Runtime-Invoked-Function-Arn.
func (p *Pool) functionARN() string {
	return "arn:aws:lambda:local:000000000000:function:" + p.cfg.FunctionName
}

// invocation is one Invoke call travelling from the queue to a worker.
type invocation struct {
	id       string
	ctx      context.Context
	payload  []byte
	deadline time.Time
	result   chan Result // buffered(1): completing never blocks, even if Invoke gave up

	once sync.Once
}

func newInvocation(ctx context.Context, payload []byte, deadline time.Time) *invocation {
	return &invocation{
		id:       newRequestID(),
		ctx:      ctx,
		payload:  payload,
		deadline: deadline,
		result:   make(chan Result, 1),
	}
}

// complete delivers res to the waiting Invoke. Only the first call has effect.
func (inv *invocation) complete(res Result) {
	inv.once.Do(func() { inv.result <- res })
}

// newRequestID returns a random RFC 4122 version 4 UUID.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// errorPayload renders a Lambda-style error document.
func errorPayload(errorType, message string) []byte {
	b, _ := json.Marshal(struct {
		ErrorMessage string `json:"errorMessage"`
		ErrorType    string `json:"errorType"`
	}{message, errorType})
	return b
}

func timeoutResult(message string) Result {
	return Result{Status: http.StatusGatewayTimeout, Payload: errorPayload("Function.TimedOut", message)}
}

func cancelledResult(err error) Result {
	return timeoutResult("invocation abandoned: " + err.Error())
}
