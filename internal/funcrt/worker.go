package funcrt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const runtimeAPIPrefix = "/2018-06-01/runtime"

// Restart backoff: a bootstrap that exited (crash, timeout kill, init failure)
// is restarted after minRestartDelay, doubling on every further exit up to
// maxRestartDelay. A successful invocation resets it.
const (
	minRestartDelay = 100 * time.Millisecond
	maxRestartDelay = 10 * time.Second
)

// worker owns one Runtime API listener (fixed port, lives for the pool's
// lifetime) and the bootstrap process that talks to it. The process may be
// killed and restarted any number of times while the listener, port and
// identity stay the same.
type worker struct {
	pool  *Pool
	index int
	port  int
	ln    net.Listener

	// ready is true once the current process has polled /next. It is reset
	// when the process exits or reports /init/error.
	ready atomic.Bool
	// initFailed is set when the current process reported /init/error; such a
	// process never becomes ready. It is reset before each process start.
	initFailed atomic.Bool

	// starts and exits count bootstrap process starts and exits.
	starts, exits atomic.Int64

	mu         sync.Mutex
	current    *invocation // invocation handed out by /next and not yet answered
	currentGen int64       // process generation (starts) that took current
	deadline   *time.Timer // fires onDeadline for current
	closed     bool        // set at shutdown; no new invocation may become current
	proc       *os.Process // running bootstrap, nil between processes
	procGen    int64       // generation of proc
	backoff    time.Duration
}

func (w *worker) addr() string { return fmt.Sprintf("127.0.0.1:%d", w.port) }

func (w *worker) log() *slog.Logger {
	return slog.Default().With("component", "funcrt", "worker", w.index)
}

func (w *worker) listen() error {
	ln, err := net.Listen("tcp", w.addr())
	if err != nil {
		return fmt.Errorf("funcrt: worker %d: %w", w.index, err)
	}
	w.ln = ln
	return nil
}

func (w *worker) closeListener() { _ = w.ln.Close() }

// run serves the Runtime API and keeps a bootstrap process running until ctx
// is cancelled.
func (w *worker) run(ctx context.Context) {
	srv := &http.Server{
		Handler:           w.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(w.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.log().Error("runtime API server stopped", "err", err)
		}
	}()

	for ctx.Err() == nil {
		err := w.runProcess(ctx)
		w.onProcessExit(ctx, err)
		select {
		case <-ctx.Done():
		case <-time.After(w.nextBackoff()):
		}
	}

	_ = srv.Close()
	<-served
	w.mu.Lock()
	w.closed = true
	inv := w.clearCurrentLocked()
	w.mu.Unlock()
	if inv != nil {
		inv.complete(shutdownResult())
	}
}

func shutdownResult() Result {
	return Result{Status: http.StatusBadGateway, Payload: errorPayload("Runtime.Shutdown", "funcrt is shutting down")}
}

// nextBackoff returns the delay before the next restart and doubles the one
// after it, up to maxRestartDelay.
func (w *worker) nextBackoff() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := max(w.backoff, minRestartDelay)
	w.backoff = min(2*d, maxRestartDelay)
	return d
}

// resetBackoff makes the next restart delay minRestartDelay again.
func (w *worker) resetBackoff() {
	w.mu.Lock()
	w.backoff = 0
	w.mu.Unlock()
}

// runProcess starts one bootstrap process and blocks until it exits or ctx is
// cancelled (in which case its process group is killed). While it runs, the
// process can be killed through killProcess.
func (w *worker) runProcess(ctx context.Context) error {
	cfg := w.pool.cfg
	cmd := exec.Command(cfg.Bootstrap)
	cmd.Env = append(append(os.Environ(), cfg.Env...), "AWS_LAMBDA_RUNTIME_API="+w.addr())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Own process group so the whole tree can be killed (see killProcess).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	w.initFailed.Store(false)
	if err := cmd.Start(); err != nil {
		return err
	}
	gen := w.starts.Add(1)
	w.setProc(cmd.Process, gen)
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		w.setProc(nil, 0)
		exited <- err
	}()

	select {
	case err := <-exited:
		return err
	case <-ctx.Done():
		w.killProcess(0)
		<-exited
		return ctx.Err()
	}
}

func (w *worker) setProc(p *os.Process, gen int64) {
	w.mu.Lock()
	w.proc, w.procGen = p, gen
	w.mu.Unlock()
}

// killProcess sends SIGKILL to the running bootstrap's process group. With a
// non-zero gen it only kills that process generation, so a late kill cannot
// hit a newer process.
func (w *worker) killProcess(gen int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.proc == nil || (gen != 0 && w.procGen != gen) {
		return
	}
	_ = syscall.Kill(-w.proc.Pid, syscall.SIGKILL)
}

// onProcessExit is called after every bootstrap exit. An invocation still in
// flight fails at once with 502 Runtime.ExitError rather than at its deadline.
// At shutdown the final sweep in run answers it instead.
func (w *worker) onProcessExit(ctx context.Context, err error) {
	wasReady := w.ready.Swap(false)
	w.exits.Add(1)
	if ctx.Err() != nil {
		return
	}
	if inv := w.takeCurrent(""); inv != nil {
		inv.complete(exitResult(err))
	}
	switch {
	case w.initFailed.Load():
		w.log().Error("bootstrap init failed (reported /init/error); restarting with backoff", "err", err)
	case !wasReady:
		w.log().Error("bootstrap exited before polling /next (init failure); restarting with backoff", "err", err)
	default:
		w.log().Warn("bootstrap exited", "err", err)
	}
}

func exitResult(err error) Result {
	msg := "Runtime exited without providing a reason"
	if err != nil {
		msg = "Runtime exited with error: " + err.Error()
	}
	return Result{Status: http.StatusBadGateway, Payload: errorPayload("Runtime.ExitError", msg)}
}

// onInitError handles /init/error: the process can never become ready, so it
// is killed and restarted with backoff.
func (w *worker) onInitError(body []byte) {
	w.initFailed.Store(true)
	w.ready.Store(false)
	w.log().Error("bootstrap init error", "error", string(body))
	w.killProcess(w.starts.Load())
}

// onDeadline fires when inv passes its deadline while in flight: it answers
// 504 and kills the process that is running it; run then restarts it.
func (w *worker) onDeadline(inv *invocation) {
	w.mu.Lock()
	if w.current != inv {
		w.mu.Unlock()
		return
	}
	gen := w.currentGen
	w.clearCurrentLocked()
	w.mu.Unlock()
	inv.complete(timeoutResult("invocation timed out"))
	w.log().Warn("invocation timed out; killing bootstrap", "requestId", inv.id)
	w.killProcess(gen)
}

func (w *worker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+runtimeAPIPrefix+"/invocation/next", w.handleNext)
	mux.HandleFunc("POST "+runtimeAPIPrefix+"/invocation/{id}/response", w.handleResponse)
	mux.HandleFunc("POST "+runtimeAPIPrefix+"/invocation/{id}/error", w.handleError)
	mux.HandleFunc("POST "+runtimeAPIPrefix+"/init/error", w.handleInitError)
	return mux
}

// handleNext long-polls the shared queue and hands the next invocation to the
// bootstrap.
func (w *worker) handleNext(rw http.ResponseWriter, r *http.Request) {
	if !w.markReady() {
		writeStatus(rw, http.StatusForbidden, "Runtime.InitError", "runtime reported an init error; it will be restarted")
		return
	}
	// A /next while an invocation is outstanding means the runtime will never
	// answer it (or a stale poll from a killed process raced a new one).
	w.failCurrent(noResponseResult())

	inv, ok := w.take(r.Context())
	if !ok {
		return
	}
	if !w.setCurrent(inv) {
		w.pool.requeue(inv)
		return
	}

	h := rw.Header()
	for k, v := range w.nextHeaders(inv) {
		h[k] = v
	}
	h.Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(inv.payload)
}

// markReady records that the current process polled /next. It returns false,
// leaving the worker not ready, if the process reported /init/error.
// initFailed is re-checked after storing ready so that a concurrent
// onInitError (which sets initFailed, then clears ready) always wins.
func (w *worker) markReady() bool {
	if w.initFailed.Load() {
		return false
	}
	w.ready.Store(true)
	if w.initFailed.Load() {
		w.ready.Store(false)
		return false
	}
	return true
}

// take receives the next dispatchable invocation from the queue. Invocations
// whose deadline has passed or whose caller has gone away are completed with
// 504 and skipped. It returns false if ctx is done first.
func (w *worker) take(ctx context.Context) (*invocation, bool) {
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case inv := <-w.pool.queue:
			if ctx.Err() != nil {
				// select picked the queue although our poll is gone.
				w.pool.requeue(inv)
				return nil, false
			}
			if !time.Now().Before(inv.deadline) {
				inv.complete(timeoutResult("invocation timed out while queued"))
				continue
			}
			if err := inv.ctx.Err(); err != nil {
				inv.complete(cancelledResult(err))
				continue
			}
			return inv, true
		}
	}
}

func (w *worker) nextHeaders(inv *invocation) http.Header {
	h := http.Header{}
	h.Set("Lambda-Runtime-Aws-Request-Id", inv.id)
	h.Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(inv.deadline.UnixMilli(), 10))
	h.Set("Lambda-Runtime-Invoked-Function-Arn", w.pool.functionARN())
	return h
}

// setCurrent makes inv the in-flight invocation and arms its deadline timer
// (see onDeadline), completing any invocation it replaces with 502. It returns
// false (leaving inv untouched) if the worker has shut down.
func (w *worker) setCurrent(inv *invocation) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	prev := w.clearCurrentLocked()
	w.current, w.currentGen = inv, w.starts.Load()
	w.deadline = time.AfterFunc(time.Until(inv.deadline), func() { w.onDeadline(inv) })
	w.mu.Unlock()
	if prev != nil {
		prev.complete(noResponseResult())
	}
	return true
}

// failCurrent completes and clears the in-flight invocation, if any.
func (w *worker) failCurrent(res Result) {
	if inv := w.takeCurrent(""); inv != nil {
		inv.complete(res)
	}
}

func noResponseResult() Result {
	return Result{Status: http.StatusBadGateway, Payload: errorPayload("Runtime.NoResponse", "runtime requested another invocation without answering this one")}
}

// takeCurrent clears and returns the in-flight invocation if its id matches
// (any id when id is empty), or nil.
func (w *worker) takeCurrent(id string) *invocation {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil || (id != "" && w.current.id != id) {
		return nil
	}
	return w.clearCurrentLocked()
}

// clearCurrentLocked clears and returns the in-flight invocation (possibly
// nil) and disarms its deadline timer. w.mu must be held.
func (w *worker) clearCurrentLocked() *invocation {
	inv := w.current
	w.current, w.currentGen = nil, 0
	if w.deadline != nil {
		w.deadline.Stop()
		w.deadline = nil
	}
	return inv
}

func (w *worker) handleResponse(rw http.ResponseWriter, r *http.Request) {
	ok := w.finish(rw, r, func(body []byte) Result {
		return Result{Status: http.StatusOK, Payload: body}
	})
	if ok {
		w.resetBackoff()
	}
}

func (w *worker) handleError(rw http.ResponseWriter, r *http.Request) {
	w.finish(rw, r, func(body []byte) Result {
		if len(body) == 0 {
			errType := r.Header.Get("Lambda-Runtime-Function-Error-Type")
			if errType == "" {
				errType = "Unhandled"
			}
			body = errorPayload(errType, "function returned an error")
		}
		return Result{Status: http.StatusBadGateway, Payload: body}
	})
}

// finish completes the in-flight invocation named by the {id} path value. It
// reports whether that invocation was in flight.
func (w *worker) finish(rw http.ResponseWriter, r *http.Request, result func(body []byte) Result) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(rw, http.StatusBadRequest, "InvalidRequest", err.Error())
		return false
	}
	id := r.PathValue("id")
	inv := w.takeCurrent(id)
	if inv == nil {
		writeStatus(rw, http.StatusBadRequest, "InvalidRequestID", "unknown or already answered request id "+id)
		return false
	}
	inv.complete(result(body))
	writeStatus(rw, http.StatusAccepted, "", "")
	return true
}

// handleInitError acknowledges /init/error before killing the process, so
// the runtime sees the 202 rather than a reset connection.
func (w *worker) handleInitError(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	writeStatus(rw, http.StatusAccepted, "", "")
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
	w.onInitError(body)
}

// writeStatus writes a Runtime API status (errorType empty) or error document.
func writeStatus(rw http.ResponseWriter, code int, errorType, message string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	if errorType == "" {
		_, _ = rw.Write([]byte(`{"status":"OK"}`))
		return
	}
	_, _ = rw.Write(errorPayload(errorType, message))
}
