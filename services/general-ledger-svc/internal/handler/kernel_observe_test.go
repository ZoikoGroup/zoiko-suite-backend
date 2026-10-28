package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

func TestOutcomeSeparatesAnAccountingRefusalFromAnOutageAndARepeat(t *testing.T) {
	for _, c := range []struct {
		command string
		status  int
		want    string
	}{
		{"post_event", 201, kernel.OutcomeOK},
		{"post_event", 200, kernel.OutcomeExisting}, // an idempotent repeat is a duplicate-prevention hit
		{"register_event", 200, kernel.OutcomeExisting},
		{"reconciliation_run", 201, kernel.OutcomeOK},
		{"period_seal", 200, kernel.OutcomeOK}, // not an idempotent-create command: 200 is plain success
		{"post_event", 403, kernel.OutcomeRefused},
		{"post_event", 409, kernel.OutcomeRefused},
		{"post_event", 422, kernel.OutcomeRefused},
		{"post_event", 503, kernel.OutcomeDependency}, // a dependency that is down is not an accounting failure
		{"post_event", 500, kernel.OutcomeError},
	} {
		if got := outcomeOf(c.command, c.status); got != c.want {
			t.Errorf("%s %d: %s, want %s", c.command, c.status, got, c.want)
		}
	}
}

type captureObs struct {
	kernel.NopObserver
	command, outcome string
	d                time.Duration
}

func (c *captureObs) Command(n, o string, d time.Duration) { c.command, c.outcome, c.d = n, o, d }

func TestTheObserveMiddlewareRecordsTheCommandItWraps(t *testing.T) {
	obs := &captureObs{}
	h := (&KernelHandler{}).WithObserver(obs)
	srv := h.observe("post_event")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) }))
	srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	if obs.command != "post_event" || obs.outcome != kernel.OutcomeRefused {
		t.Fatalf("%+v", obs)
	}
	// with no observer wired the middleware is harmless
	(&KernelHandler{}).observe("x")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
