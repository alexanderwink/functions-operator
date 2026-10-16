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
	Timeout      time.Duration // execution timeout, counted from dispatch (and, separately, the queue-wait limit); <= 0 means DefaultTimeout
	BasePort     int           // worker i listens on 127.0.0.1:BasePort+i
	Env          []string      // complete bootstrap environment (see BootstrapEnv); not inherited from the process
	FunctionName string
}

// Result is the outcome of one invocation.
type Result struct {
	Status    int    // 200 ok, 500 handler error (/invocation/error), 502 crash/no response, 504 timeout
	Payload   []byte // response payload, or Lambda error JSON otherwise
	RequestID string // Lambda request id of the invocation (set by Invoke)
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
// Timeout bounds two phases separately (ruling R16). Queue wait: if no worker
// takes the invocation within Timeout of Invoke (or ctx is done first), it is
// never dispatched and Invoke returns 504 without killing anything.
// Execution: once dispatched, the invocation's deadline (also sent to the
// bootstrap as Lambda-Runtime-Deadline-Ms) is dispatch time + Timeout. At that
// deadline the worker answers 504 and kills and restarts the runaway process;
// Invoke itself also returns 504 then (or as soon as ctx is done) even if the
// worker never answers.
//
// Invoke alone maintains the Queued and InFlight gauges, so they cannot leak
// whatever happens to the invocation inside a worker.
func (p *Pool) Invoke(ctx context.Context, payload []byte) Result {
	// Cancelled on return, so an invocation nobody waits for any more is never
	// dispatched (see worker.take and requeue).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inv := newInvocation(ctx, payload, time.Now().Add(p.cfg.Timeout))
	res := p.invoke(ctx, inv)
	res.RequestID = inv.id
	return res
}

func (p *Pool) invoke(ctx context.Context, inv *invocation) Result {
	queueTimer := time.NewTimer(time.Until(inv.queueDeadline))
	defer queueTimer.Stop()

	p.queued.Add(1)
	select {
	case p.queue <- inv:
		p.queued.Add(-1)
	case <-queueTimer.C:
		p.queued.Add(-1)
		return timeoutResult("invocation timed out while queued")
	case <-ctx.Done():
		p.queued.Add(-1)
		return cancelledResult(ctx.Err())
	}

	// Backstop for the execution deadline the worker arms at dispatch.
	execTimer := time.NewTimer(p.cfg.Timeout)
	defer execTimer.Stop()
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	select {
	case res := <-inv.result:
		return res
	case <-execTimer.C:
		return timeoutResult("invocation timed out")
	case <-ctx.Done():
		return cancelledResult(ctx.Err())
	}
}

// requeue hands inv back to the queue after a worker took it but could not
// run it (its /next poll or the worker went away). It gets a fresh queue-wait
// limit, and gives up, completing inv with 504, when that passes or when the
// caller has gone away.
func (p *Pool) requeue(inv *invocation) {
	inv.queueDeadline = time.Now().Add(p.cfg.Timeout) // ordered before any take by the channel send
	go func() {
		timer := time.NewTimer(time.Until(inv.queueDeadline))
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
	id      string
	ctx     context.Context // done once Invoke has returned
	payload []byte
	// queueDeadline is when waiting for a worker gives up (Invoke + Timeout,
	// renewed on requeue). deadline is the execution deadline, set by the
	// worker at dispatch (setCurrent). Both are written only while no other
	// goroutine can read them (before the queue send that hands inv over).
	queueDeadline time.Time
	deadline      time.Time
	result        chan Result // buffered(1): completing never blocks, even if Invoke gave up

	once sync.Once
}

func newInvocation(ctx context.Context, payload []byte, queueDeadline time.Time) *invocation {
	return &invocation{
		id:            newRequestID(),
		ctx:           ctx,
		payload:       payload,
		queueDeadline: queueDeadline,
		result:        make(chan Result, 1),
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
