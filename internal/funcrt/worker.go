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

// restartDelay is the pause before a bootstrap process that exited is started
// again. Task 3 replaces it with exponential backoff.
const restartDelay = 100 * time.Millisecond

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
	// when the process exits.
	ready atomic.Bool

	mu      sync.Mutex
	current *invocation // invocation handed out by /next and not yet answered
	closed  bool        // set at shutdown; no new invocation may become current
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
		case <-time.After(restartDelay):
		}
	}

	_ = srv.Close()
	<-served
	w.mu.Lock()
	w.closed = true
	inv := w.current
	w.current = nil
	w.mu.Unlock()
	if inv != nil {
		inv.complete(shutdownResult())
	}
}

func shutdownResult() Result {
	return Result{Status: http.StatusBadGateway, Payload: errorPayload("Runtime.Shutdown", "funcrt is shutting down")}
}

// runProcess starts one bootstrap process and blocks until it exits or ctx is
// cancelled (in which case its process group is killed).
func (w *worker) runProcess(ctx context.Context) error {
	cfg := w.pool.cfg
	cmd := exec.Command(cfg.Bootstrap)
	cmd.Env = append(append(os.Environ(), cfg.Env...), "AWS_LAMBDA_RUNTIME_API="+w.addr())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Own process group so the whole tree can be killed (see killProcessGroup).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		return err
	case <-ctx.Done():
		killProcessGroup(cmd)
		<-exited
		return ctx.Err()
	}
}

func killProcessGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// onProcessExit is called after every bootstrap exit. Task 3 extends it to
// fail an in-flight invocation (502 Runtime.ExitError) and drive backoff.
func (w *worker) onProcessExit(ctx context.Context, err error) {
	w.ready.Store(false)
	if ctx.Err() == nil {
		w.log().Warn("bootstrap exited", "err", err)
	}
}

// onInitError is called when the bootstrap reports /init/error. Task 3 uses it
// to count init failures.
func (w *worker) onInitError(body []byte) {
	w.log().Error("bootstrap init error", "error", string(body))
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
	w.ready.Store(true)
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

// setCurrent makes inv the in-flight invocation, completing any invocation it
// replaces with 502. It returns false (leaving inv untouched) if the worker
// has shut down.
func (w *worker) setCurrent(inv *invocation) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	prev := w.current
	w.current = inv
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
	inv := w.current
	if inv == nil || (id != "" && inv.id != id) {
		return nil
	}
	w.current = nil
	return inv
}

func (w *worker) handleResponse(rw http.ResponseWriter, r *http.Request) {
	w.finish(rw, r, func(body []byte) Result {
		return Result{Status: http.StatusOK, Payload: body}
	})
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

// finish completes the in-flight invocation named by the {id} path value.
func (w *worker) finish(rw http.ResponseWriter, r *http.Request, result func(body []byte) Result) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatus(rw, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	id := r.PathValue("id")
	inv := w.takeCurrent(id)
	if inv == nil {
		writeStatus(rw, http.StatusBadRequest, "InvalidRequestID", "unknown or already answered request id "+id)
		return
	}
	inv.complete(result(body))
	writeStatus(rw, http.StatusAccepted, "", "")
}

func (w *worker) handleInitError(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.onInitError(body)
	writeStatus(rw, http.StatusAccepted, "", "")
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
