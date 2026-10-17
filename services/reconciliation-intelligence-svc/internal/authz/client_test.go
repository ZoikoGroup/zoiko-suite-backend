package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

// fakeAuthz stands in for authorization-svc: it records every request's
// headers and answers with decide(tenant), like the real service always
// returning 200 with the decision in the body.
type fakeAuthz struct {
	mu      sync.Mutex
	headers []http.Header
}

func (f *fakeAuthz) serve(t *testing.T, decide func(tenant string) string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": decide(r.Header.Get("X-Tenant-Id"))})
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, zap.NewNop())
}

func grant(string) string { return "GRANTED" }

// The canonical envelope is forwarded with the CALLER's values — the defect
// class fixed twice in financial-close-svc (7abd0f8a, d6c2cc38).
func TestEnvelopeForwarded_CallerValues(t *testing.T) {
	f := &fakeAuthz{}
	c := f.serve(t, grant)

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		RequestID: "req-caller", CorrelationID: "corr-caller", CausationID: "cause-caller",
		SourceChannel: svcenvelope.ChannelIntegration, SourceSystem: "erp-x",
	})
	if err := c.CheckAllowed(ctx, "tenant-1", "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	got := f.headers[0]
	want := map[string]string{
		"X-Tenant-Id":       "tenant-1",
		"X-Principal-Id":    "principal-1",
		"X-Legal-Entity-Id": "le-1",
		"X-Request-Id":      "req-caller",
		"X-Correlation-ID":  "corr-caller",
		"X-Causation-Id":    "cause-caller",
		"X-Source-Channel":  "integration",
		// An external channel obliges source_system.
		"X-Source-System": "erp-x",
	}
	for h, v := range want {
		if got.Get(h) != v {
			t.Errorf("%s = %q, want %q", h, got.Get(h), v)
		}
	}
	// /v1/authorize is not a material write in authorization-svc, and two
	// identical questions must log two decisions — no idempotency key.
	if k := got.Get("Idempotency-Key"); k != "" {
		t.Errorf("Idempotency-Key sent (%q); /v1/authorize must not be deduplicated", k)
	}
}

// With no envelope in context, ids are minted and the channel is "system" —
// the mandatory set is never sent short.
func TestEnvelopeForwarded_MintsWhenAbsent(t *testing.T) {
	f := &fakeAuthz{}
	c := f.serve(t, grant)
	if err := c.CheckAllowed(context.Background(), "tenant-1", "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("CheckAllowed: %v", err)
	}
	got := f.headers[0]
	for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Request-Id", "X-Correlation-ID"} {
		if got.Get(h) == "" {
			t.Errorf("%s not sent", h)
		}
	}
	if got.Get("X-Source-Channel") != "system" {
		t.Errorf("X-Source-Channel = %q, want system", got.Get("X-Source-Channel"))
	}
}

// No verified tenant: refuse without calling, never under a made-up tenant
// (b75655a2).
func TestTenantMissing_RefusedWithoutCall(t *testing.T) {
	f := &fakeAuthz{}
	c := f.serve(t, grant)
	if err := c.CheckAllowed(context.Background(), "", "principal-1", "le-1", "SOME_ACTION"); !errors.Is(err, ErrTenantMissing) {
		t.Fatalf("expected ErrTenantMissing, got %v", err)
	}
	if len(f.headers) != 0 {
		t.Fatalf("authorization-svc was called %d time(s) with no tenant", len(f.headers))
	}
}

// The decision cache is keyed by tenant: the same principal, entity and
// action in another tenant is a different question. Before the tenant was a
// parameter, tenant-b would have been answered from tenant-a's cached grant.
func TestCache_DoesNotCrossTenants(t *testing.T) {
	f := &fakeAuthz{}
	c := f.serve(t, func(tenant string) string {
		if tenant == "tenant-a" {
			return "GRANTED"
		}
		return "DENIED"
	})
	ctx := context.Background()

	if err := c.CheckAllowed(ctx, "tenant-a", "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("tenant-a: expected grant, got %v", err)
	}
	if err := c.CheckAllowed(ctx, "tenant-b", "principal-1", "le-1", "SOME_ACTION"); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("tenant-b: expected its own DENIED decision, got %v (served from tenant-a's cache?)", err)
	}
	// Same tenant again is a cache hit: still two live calls in total.
	if err := c.CheckAllowed(ctx, "tenant-a", "principal-1", "le-1", "SOME_ACTION"); err != nil {
		t.Fatalf("tenant-a repeat: %v", err)
	}
	if len(f.headers) != 2 {
		t.Fatalf("expected 2 live calls (one per tenant), got %d", len(f.headers))
	}
}
