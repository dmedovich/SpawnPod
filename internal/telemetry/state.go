// Package telemetry exposes bounded process metrics and dependency readiness.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
)

type rpcMetric struct {
	count, failures, active int64
	seconds                 float64
}
type State struct {
	mu                            sync.Mutex
	serving, draining, api        bool
	checked                       time.Time
	reconciles, reconcileFailures int64
	lastReconcile                 time.Time
	rpc                           map[string]*rpcMetric
}

func (s *State) Serving(value bool) { s.mu.Lock(); s.serving = value; s.mu.Unlock() }
func (s *State) Drain()             { s.mu.Lock(); s.draining = true; s.mu.Unlock() }
func (s *State) API(err error) {
	s.mu.Lock()
	s.api = err == nil
	s.checked = time.Now()
	s.mu.Unlock()
}
func (s *State) Reconciled(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciles++
	if err != nil {
		s.reconcileFailures++
	} else {
		s.lastReconcile = time.Now()
	}
}
func (s *State) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serving && !s.draining && s.api && time.Since(s.checked) < 20*time.Second
}
func methodName(method string) string {
	// Never turn arbitrary client method names into metric labels.
	parts := strings.Split(method, "/")
	if len(parts) == 3 && parts[1] == "plugin.v1alpha.BackendPlugin" {
		switch parts[2] {
		case "Capabilities", "Create", "Start", "Exec", "CopyIn", "CopyOut", "Remove":
			return parts[2]
		}
	}
	return "other"
}
func (s *State) observe(method string) func(error) {
	method = methodName(method)
	s.mu.Lock()
	if s.rpc == nil {
		s.rpc = make(map[string]*rpcMetric)
	}
	m := s.rpc[method]
	if m == nil {
		m = &rpcMetric{}
		s.rpc[method] = m
	}
	m.active++
	s.mu.Unlock()
	start := time.Now()
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		m.active--
		m.count++
		m.seconds += time.Since(start).Seconds()
		if err != nil {
			m.failures++
		}
	}
}
func (s *State) Unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	finish := s.observe(info.FullMethod)
	response, err := handler(ctx, req)
	finish(err)
	return response, err
}
func (s *State) Stream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	finish := s.observe(info.FullMethod)
	err := handler(srv, stream)
	finish(err)
	return err
}
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.mu.Lock()
		defer s.mu.Unlock()
		ready := 0
		if s.serving && !s.draining && s.api && time.Since(s.checked) < 20*time.Second {
			ready = 1
		}
		fmt.Fprintf(w, "# TYPE spawnpod_ready gauge\nspawnpod_ready %d\n", ready)
		fmt.Fprintf(w, "# TYPE spawnpod_reconciliations_total counter\nspawnpod_reconciliations_total %d\n", s.reconciles)
		fmt.Fprintf(w, "# TYPE spawnpod_reconciliation_failures_total counter\nspawnpod_reconciliation_failures_total %d\n", s.reconcileFailures)
		last := int64(0)
		if !s.lastReconcile.IsZero() {
			last = s.lastReconcile.Unix()
		}
		fmt.Fprintf(w, "# TYPE spawnpod_reconciliation_last_success_timestamp_seconds gauge\nspawnpod_reconciliation_last_success_timestamp_seconds %d\n", last)
		fmt.Fprintln(w, "# TYPE spawnpod_rpc_total counter\n# TYPE spawnpod_rpc_failures_total counter\n# TYPE spawnpod_rpc_active gauge\n# TYPE spawnpod_rpc_duration_seconds summary")
		for method, m := range s.rpc {
			fmt.Fprintf(w, "spawnpod_rpc_total{method=%q} %d\nspawnpod_rpc_failures_total{method=%q} %d\nspawnpod_rpc_active{method=%q} %d\nspawnpod_rpc_duration_seconds_sum{method=%q} %g\nspawnpod_rpc_duration_seconds_count{method=%q} %d\n", method, m.count, method, m.failures, method, m.active, method, m.seconds, method, m.count)
		}
	})
	return mux
}
