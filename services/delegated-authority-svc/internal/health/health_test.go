package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/health"
)

type fakePool struct{ err error }

func (f fakePool) Ping(context.Context) error { return f.err }

var okPool = fakePool{}

func readinessBody(t *testing.T, h *health.Health) (int, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.Readiness(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("readiness body is not JSON: %v", err)
	}
	return rr.Code, body
}

// Liveness must answer 200 on a process that is up, whatever else is broken.
// A liveness probe that fails on a dependency outage gets the container killed
// and restarted, which fixes nothing and removes the one instance that could
// have said what was wrong.
func TestLiveness_IsUnconditional(t *testing.T) {
	h := health.New(okPool, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return errors.New("down") },
	})
	rr := httptest.NewRecorder()
	h.Liveness(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200 even with every dependency down", rr.Code)
	}
}

// The gap this fixed: EVERY endpoint on this service calls authorization-svc
// before doing anything and the service fails closed, so with authorization-svc
// unreachable a perfectly healthy pool meant 100% of requests answered 503
// while readiness reported ready and the load balancer kept sending traffic.
func TestReadiness_UnreachableDependencyIsNotReady(t *testing.T) {
	h := health.New(okPool, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return errors.New("connection refused") },
	})
	code, body := readinessBody(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503 when a hard dependency is unreachable", code)
	}
	if body["status"] != "NOT_READY" {
		t.Errorf("status = %v, want NOT_READY", body["status"])
	}
}

// Which component failed has to be in the answer, because the two verdicts have
// opposite remedies: a dead pool is this service's problem, an unreachable
// authorization-svc is somebody else's and restarting this one will not help.
func TestReadiness_NamesTheFailingComponent(t *testing.T) {
	h := health.New(okPool, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return errors.New("connection refused") },
	})
	_, body := readinessBody(t, h)
	components, ok := body["components"].(map[string]any)
	if !ok {
		t.Fatalf("readiness body has no components map: %v", body)
	}
	got, ok := components["authorization-svc"].(string)
	if !ok {
		t.Fatalf("authorization-svc is not reported at all: %v", components)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("authorization-svc = %q, want the underlying cause", got)
	}
	if _, ok := components["database"]; !ok {
		t.Error("database verdict missing; an operator cannot tell which half is down")
	}
}

// A reachable dependency must not be what fails the probe — otherwise the
// previous test passes for the wrong reason.
func TestReadiness_HealthyDependencyReportsOK(t *testing.T) {
	h := health.New(okPool, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return nil },
	})
	_, body := readinessBody(t, h)
	components := body["components"].(map[string]any)
	if components["authorization-svc"] != "ok" {
		t.Errorf("authorization-svc = %v, want ok", components["authorization-svc"])
	}
}

// The other half of the same fix: a healthy dependency must not mask a dead
// pool. Both verdicts are independent and both are reported.
func TestReadiness_DeadPoolIsNotReadyEvenWithEveryDependencyUp(t *testing.T) {
	h := health.New(fakePool{err: errors.New("pool closed")}, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return nil },
	})
	code, body := readinessBody(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503 with an unreachable database", code)
	}
	components := body["components"].(map[string]any)
	if got, _ := components["database"].(string); !strings.Contains(got, "pool closed") {
		t.Errorf("database = %q, want the underlying cause", got)
	}
}

// Everything up is the only combination that answers 200.
func TestReadiness_AllHealthyIsReady(t *testing.T) {
	h := health.New(okPool, zap.NewNop(), health.Dependency{
		Name:  "authorization-svc",
		Check: func(context.Context) error { return nil },
	})
	code, body := readinessBody(t, h)
	if code != http.StatusOK {
		t.Errorf("readiness = %d, want 200", code)
	}
	if body["status"] != "READY" {
		t.Errorf("status = %v, want READY", body["status"])
	}
}

// A probe with no dependencies declared still reports the database, so the
// variadic argument cannot silently turn the check off.
func TestReadiness_NoDependenciesStillChecksTheDatabase(t *testing.T) {
	h := health.New(fakePool{err: errors.New("down")}, zap.NewNop())
	code, _ := readinessBody(t, h)
	if code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503", code)
	}
}
