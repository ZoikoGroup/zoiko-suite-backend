package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
)

func authzServer(t *testing.T, body string, calls *int32) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewHTTPClient(srv.URL, zap.NewNop())
}

func TestDecide_ReturnsTheRecordedDecision(t *testing.T) {
	var calls int32
	c := authzServer(t, `{"decision_outcome":"GRANTED","decision_basis":"rbac:role=CONTROL_CERTIFIER",
		"access_decision_id":"ad-1","decision":"PERMIT","policy_set_version":"cfg.9","reason_codes":["RBAC_GRANT"]}`, &calls)
	a, err := c.Decide(context.Background(), "p1", "e1", "FINCTRL_CERTIFY")
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessDecisionID != "ad-1" || a.DecisionBasis != "rbac:role=CONTROL_CERTIFIER" || a.PolicySetVersion != "cfg.9" ||
		a.PrincipalID != "p1" || a.LegalEntityID != "e1" || a.ActionType != "FINCTRL_CERTIFY" || a.DecidedAt.IsZero() {
		t.Fatalf("snapshot: %+v", a)
	}
}

// Decide never answers from the decision cache: a recorded authority must be the
// decision taken for this command, not one reused from another request.
func TestDecide_BypassesTheCache(t *testing.T) {
	var calls int32
	c := authzServer(t, `{"decision_outcome":"GRANTED","access_decision_id":"ad-1"}`, &calls)
	ctx := context.Background()
	if err := c.CheckAllowed(ctx, "p1", "e1", "FINCTRL_CERTIFY"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckAllowed(ctx, "p1", "e1", "FINCTRL_CERTIFY"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("CheckAllowed should have used its cache, server saw %d calls", calls)
	}
	if _, err := c.Decide(ctx, "p1", "e1", "FINCTRL_CERTIFY"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Decide must ask live, server saw %d calls", calls)
	}
}

func TestDecide_FailsClosed(t *testing.T) {
	var calls int32
	cases := map[string]struct {
		body string
		want error
	}{
		"denied":                   {`{"decision_outcome":"DENIED","access_decision_id":"ad-2"}`, domain.ErrAuthorizationDenied},
		"granted, no decision id":  {`{"decision_outcome":"GRANTED"}`, domain.ErrAuthorizationServiceUnavailable},
		"step-up is not a grant":   {`{"decision_outcome":"STEP_UP","access_decision_id":"ad-3"}`, domain.ErrAuthorizationDenied},
		"unreadable response body": {`not json`, domain.ErrAuthorizationServiceUnavailable},
	}
	for name, c := range cases {
		cl := authzServer(t, c.body, &calls)
		if _, err := cl.Decide(context.Background(), "p1", "e1", "FINCTRL_CERTIFY"); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}
