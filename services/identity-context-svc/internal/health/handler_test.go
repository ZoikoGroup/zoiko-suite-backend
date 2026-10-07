package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/health"
)

// The readiness probe had no tests at all, which is how the Content-Type bug
// below survived: the header was set AFTER WriteHeader, which net/http ignores,
// so every health response had been served as text/plain since the handler was
// written. Nothing noticed because nothing parsed it.

type fakeOutbox struct {
	pending    int
	dead       int
	pendingErr error
}

func (f *fakeOutbox) PendingCount(context.Context) (int, error) {
	return f.pending, f.pendingErr
}

func (f *fakeOutbox) DeadLetterCount(context.Context) (int, error) {
	return f.dead, nil
}

type fakeUpstream struct {
	name string
	err  error
}

func (f *fakeUpstream) Name() string               { return f.name }
func (f *fakeUpstream) Ping(context.Context) error { return f.err }

// newProbeHandler builds a handler with no Redis or Postgres, so the probe's
// own branching is testable without live infrastructure.
//
// Both are always set in cmd/server; the nil guards in the handler exist so
// the parts WITH branches — backlog thresholds, upstream aggregation, which
// conditions degrade and which merely report — can be exercised here.
func newProbeHandler(ob health.OutboxProbe, threshold int, ups ...health.UpstreamProbe) http.Handler {
	h := health.NewHandler(nil, nil).WithOutbox(ob, threshold)
	for _, u := range ups {
		h = h.WithUpstream(u)
	}
	return h
}

func probe(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
		"the probe must return parseable JSON")
	return w.Code, body
}

// ── Content type ─────────────────────────────────────────────────────────────

// TestProbeServesJSON is a regression guard for a bug this package carried
// from the start: Content-Type was set AFTER WriteHeader, which net/http
// silently ignores, so every health response was served as
// "text/plain; charset=utf-8". Nothing noticed because nothing parsed it.
func TestProbeServesJSON(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{}, 100)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

// ── Outbox depth ─────────────────────────────────────────────────────────────

func TestOutboxBacklogIsReportedButNotFatalBelowThreshold(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{pending: 50}, 100)

	code, body := probe(t, h)

	// A backlog is NOT unhealthy on its own — that is the whole point of the
	// outbox, and a broker restart must not take this service out of the load
	// balancer.
	assert.Equal(t, http.StatusOK, code)
	checks := body["checks"].(map[string]any)
	assert.Equal(t, "50", checks["outbox_pending"])
	assert.Equal(t, "ok", checks["outbox"])
}

func TestOutboxBacklogPastThresholdIsDegraded(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{pending: 5000}, 100)

	code, body := probe(t, h)

	// A count far past the threshold means the relay is not running, and every
	// governance event this service produced is sitting in a table.
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "degraded", body["status"])
	checks := body["checks"].(map[string]any)
	assert.Contains(t, checks["outbox"], "relay may not be running")
}

// TestDeadLettersAreReportedButNeverDegrade.
//
// Dead letters need an operator, not a restart. Taking the pod out of rotation
// would not fix one of them, and would turn a delivery problem into an
// availability one.
func TestDeadLettersAreReportedButNeverDegrade(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{pending: 3, dead: 7}, 100)

	code, body := probe(t, h)

	assert.Equal(t, http.StatusOK, code)
	checks := body["checks"].(map[string]any)
	assert.Equal(t, "7", checks["outbox_dead_letter"])
}

func TestUnreadableOutboxIsReportedButNotFatal(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{pendingErr: errors.New("query failed")}, 100)

	code, body := probe(t, h)

	// Postgres is already checked separately; a failure here with a healthy
	// Postgres is a query problem, not an availability one.
	assert.Equal(t, http.StatusOK, code)
	checks := body["checks"].(map[string]any)
	assert.Contains(t, checks["outbox"], "unreadable")
}

// ── Upstreams ────────────────────────────────────────────────────────────────

func TestUnreachableUpstreamDegradesTheProbe(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{}, 100,
		&fakeUpstream{name: "tenant_registry", err: errors.New("connection refused")})

	code, body := probe(t, h)

	// The registry is a fail-closed dependency of Dimension 2, so a registry
	// outage means every resolution 503s. The probe should say what the
	// request path already knows rather than keeping the pod in rotation.
	assert.Equal(t, http.StatusServiceUnavailable, code)
	checks := body["checks"].(map[string]any)
	assert.Contains(t, checks["tenant_registry"], "unreachable")
}

func TestHealthyUpstreamIsReportedOk(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{}, 100, &fakeUpstream{name: "tenant_registry"})

	code, body := probe(t, h)

	assert.Equal(t, http.StatusOK, code)
	checks := body["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["tenant_registry"])
}

func TestEveryUpstreamIsCheckedNotJustTheFirst(t *testing.T) {
	h := newProbeHandler(&fakeOutbox{}, 100,
		&fakeUpstream{name: "tenant_registry"},
		&fakeUpstream{name: "access_control", err: errors.New("timeout")})

	code, body := probe(t, h)

	assert.Equal(t, http.StatusServiceUnavailable, code)
	checks := body["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["tenant_registry"])
	assert.Contains(t, checks["access_control"], "unreachable")
}
