package funcrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MaxBodyBytes is the largest request body accepted (Lambda's 6 MB payload limit).
const MaxBodyBytes = 6 << 20

// Shutdown timing. After SIGTERM the server keeps serving for PreStopDelay so
// that the pod's endpoint removal reaches the interceptor and kube-proxy
// before the listener closes, then drains in-flight requests for up to the
// function timeout plus DrainSlack. The pod's terminationGracePeriodSeconds
// covers both (see internal/resources).
const (
	PreStopDelay = 5 * time.Second
	DrainSlack   = 2 * time.Second
)

// DrainTimeout is how long shutdown waits for in-flight requests: the longest
// an invocation can run (timeout) plus DrainSlack.
func DrainTimeout(timeout time.Duration) time.Duration { return timeout + DrainSlack }

// Server exposes a Pool over HTTP: invocations on one port (every request is
// translated to an API Gateway v2 event) and health/metrics on an admin port.
type Server struct {
	pool      *Pool
	port      int
	adminPort int

	invocations *prometheus.CounterVec
	duration    prometheus.Histogram
	registry    *prometheus.Registry

	main, admin     *http.Server
	mainLn, adminLn net.Listener

	preStop time.Duration // serve this long after ctx is done before shutting down
	drain   time.Duration // then wait this long for in-flight requests
}

// NewServer returns a Server for p. port serves invocations; adminPort serves
// /healthz, /readyz and /metrics.
func NewServer(p *Pool, port, adminPort int) *Server {
	s := &Server{
		pool: p, port: port, adminPort: adminPort, registry: prometheus.NewRegistry(),
		preStop: PreStopDelay, drain: DrainTimeout(p.cfg.Timeout),
	}
	s.invocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "funcrt_invocations_total", Help: "Invocations by HTTP status returned.",
	}, []string{"status"})
	s.duration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "funcrt_invocation_duration_seconds", Help: "Invocation latency including queueing.",
		Buckets: prometheus.DefBuckets,
	})
	s.registry.MustRegister(s.invocations, s.duration,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "funcrt_inflight", Help: "Invocations running on a bootstrap."},
			func() float64 { return float64(p.InFlight()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "funcrt_queued", Help: "Invocations waiting for an idle bootstrap."},
			func() float64 { return float64(p.Queued()) }),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if p.Ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))

	s.main = &http.Server{Handler: http.HandlerFunc(s.handleInvoke), ReadHeaderTimeout: 10 * time.Second}
	s.admin = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// Run listens on both ports and serves until ctx is done. It then keeps
// serving for the pre-stop window, stops accepting and drains in-flight
// requests (see PreStopDelay, DrainTimeout). It returns nil when the drain
// completes.
func (s *Server) Run(ctx context.Context) error {
	if err := s.listen(); err != nil {
		return err
	}
	return s.serve(ctx)
}

func (s *Server) listen() error {
	var err error
	if s.mainLn, err = net.Listen("tcp", ":"+strconv.Itoa(s.port)); err != nil {
		return fmt.Errorf("funcrt: listen on port %d: %w", s.port, err)
	}
	if s.adminLn, err = net.Listen("tcp", ":"+strconv.Itoa(s.adminPort)); err != nil {
		_ = s.mainLn.Close()
		return fmt.Errorf("funcrt: listen on admin port %d: %w", s.adminPort, err)
	}
	return nil
}

func (s *Server) serve(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- s.main.Serve(s.mainLn) }()
	go func() { errc <- s.admin.Serve(s.adminLn) }()

	var serveErr error
	select {
	case <-ctx.Done():
		slog.Default().Info("shutdown requested; serving through the pre-stop window",
			"component", "funcrt", "preStop", s.preStop.String(), "drain", s.drain.String())
		select {
		case <-time.After(s.preStop):
		case serveErr = <-errc:
		}
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), s.drain)
	defer cancel()
	err := errors.Join(s.main.Shutdown(sctx), s.admin.Shutdown(sctx))
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, err)
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	ev, err := ToEvent(r, newRequestID(), start)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
		}
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		http.Error(w, "encoding event", http.StatusInternalServerError)
		return
	}
	res := s.pool.Invoke(r.Context(), payload)
	s.invocations.WithLabelValues(strconv.Itoa(res.Status)).Inc()
	s.duration.Observe(time.Since(start).Seconds())
	WriteResponse(w, res)
}
