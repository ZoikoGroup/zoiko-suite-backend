package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"
)

// A peer request (policy-svc over mTLS) carries X-Workload-Id: policy-svc.
// Forwarded as is over THIS service's certificate, authorization-svc refused
// every such check as workload_identity_mismatch and the write became a 503
// (found by the X5 dependency e2e, 11 Nov). Over mTLS the hop names the
// certificate holder.
func TestMTLSCallNamesThisServiceAsTheWorkload(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Workload-Id")
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	ctx := idenv.WithIdentity(context.Background(), &idenv.Identity{Basis: idenv.BasisPeer, Workload: "policy-svc", PrincipalID: "svc-policy"})
	c := NewClientWithHTTPClient(srv.URL, zap.NewNop(), srv.Client()).WithWorkload("governance-decision-log-svc")
	if err := c.CheckAllowed(ctx, "svc-policy", "PLATFORM", "GOVERNANCE_DECISION_RECORD"); err != nil {
		t.Fatal(err)
	}
	if got != "governance-decision-log-svc" {
		t.Fatalf("X-Workload-Id on the authorization hop = %q, want this service", got)
	}
}
