package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// authzServer answers POST /v1/authorize with body and records each request's
// path and decoded fields; any other route is a 404, as in authorization-svc.
func authzServer(t *testing.T, status int, body map[string]any) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	var calls []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/authorize" {
			http.NotFound(w, r)
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func granted() map[string]any {
	return map[string]any{"decision_outcome": "GRANTED", "decision_basis": "rbac:role=recon-certifier",
		"access_decision_id": "adl_42", "decision": "ALLOW", "policy_set_version": "psv-7", "reason_codes": []string{"RBAC_MATCH"}}
}

// CheckAllowed and Decide reach authorization-svc's real route; the old
// /v1/authorization/check does not exist there.
func TestCheckAllowed_UsesAuthorize(t *testing.T) {
	srv, calls := authzServer(t, http.StatusOK, granted())
	c := NewClient(srv.URL, zap.NewNop())
	if err := c.CheckAllowed(context.Background(), "alice", "ent-1", "RECONCILIATION_MANAGE"); err != nil {
		t.Fatalf("granted: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0]["principal_id"] != "alice" || (*calls)[0]["legal_entity_id"] != "ent-1" ||
		(*calls)[0]["action_type"] != "RECONCILIATION_MANAGE" {
		t.Fatalf("request: %v", *calls)
	}
}

func TestDecide_ReturnsTheRecordedDecision(t *testing.T) {
	srv, calls := authzServer(t, http.StatusOK, granted())
	c := NewClient(srv.URL, zap.NewNop())
	a, err := c.Decide(context.Background(), "carol", "ent-1", "RECONCILIATION_CERTIFY")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if a.AccessDecisionID != "adl_42" || a.DecisionBasis != "rbac:role=recon-certifier" || a.PolicySetVersion != "psv-7" ||
		a.PrincipalID != "carol" || a.LegalEntityID != "ent-1" || a.ActionType != "RECONCILIATION_CERTIFY" ||
		a.Source != "authorization-svc" || a.DecidedAt.IsZero() || len(a.ReasonCodes) != 1 {
		t.Fatalf("snapshot: %+v", a)
	}
	// Uncached: a second decision is asked again.
	if _, err := c.Decide(context.Background(), "carol", "ent-1", "RECONCILIATION_CERTIFY"); err != nil || len(*calls) != 2 {
		t.Fatalf("second decide: %v, calls %d", err, len(*calls))
	}
}

func TestDecide_FailsClosed(t *testing.T) {
	untraceable := granted()
	delete(untraceable, "access_decision_id")
	for name, tc := range map[string]struct {
		status int
		body   map[string]any
		want   error
	}{
		"denied":             {http.StatusOK, map[string]any{"decision_outcome": "DENIED", "access_decision_id": "adl_1"}, ErrAuthorizationDenied},
		"no access decision": {http.StatusOK, untraceable, ErrAuthorizationUnavailable},
		"server error":       {http.StatusServiceUnavailable, map[string]any{"error": "store_unavailable"}, ErrAuthorizationUnavailable},
	} {
		srv, _ := authzServer(t, tc.status, tc.body)
		a, err := NewClient(srv.URL, zap.NewNop()).Decide(context.Background(), "carol", "ent-1", "RECONCILIATION_CERTIFY")
		if a != nil || !errors.Is(err, tc.want) {
			t.Fatalf("%s: %+v, %v", name, a, err)
		}
	}
	// Unreachable.
	a, err := NewClient("http://127.0.0.1:1", zap.NewNop()).Decide(context.Background(), "carol", "ent-1", "RECONCILIATION_CERTIFY")
	if a != nil || !errors.Is(err, ErrAuthorizationUnavailable) {
		t.Fatalf("unreachable: %+v, %v", a, err)
	}
}
