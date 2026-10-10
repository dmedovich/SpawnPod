package telemetry

import (
	"context"
	"errors"
	"google.golang.org/grpc"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadinessTracksServingDependenciesAndDrain(t *testing.T) {
	s := &State{}
	check := func(want int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if w.Code != want {
			t.Fatalf("status %d, want %d", w.Code, want)
		}
	}
	check(503)
	s.API(nil)
	s.Serving(true)
	check(200)
	s.API(errors.New("API unavailable"))
	check(503)
	s.API(nil)
	s.checked = time.Now().Add(-21 * time.Second)
	check(503)
	s.API(nil)
	s.Drain()
	check(503)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal("dependency outage affected liveness")
	}
}
func TestRPCMetricsNeverUseUntrustedMethodOrErrorLabels(t *testing.T) {
	s := &State{}
	for _, method := range []string{"/plugin.v1alpha.BackendPlugin/Create", "/secret-repository/token"} {
		_, _ = s.Unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { return nil, errors.New("secret error text") })
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	b := w.Body.String()
	for _, secret := range []string{"secret-repository", "token", "secret error text"} {
		if strings.Contains(b, secret) {
			t.Fatalf("metric leaked %q", secret)
		}
	}
	if !strings.Contains(b, `spawnpod_rpc_failures_total{method="Create"} 1`) || !strings.Contains(b, `spawnpod_rpc_active{method="Create"} 0`) {
		t.Fatal(b)
	}
}
