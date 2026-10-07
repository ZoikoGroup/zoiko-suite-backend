package payeeidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

// TestGetActiveDestination_ForwardsPrincipalHeader pins the fix for a real
// bug: payee-banking-identity-svc's GetActivePayeeDestination unconditionally
// requires X-Principal-Id (401 without it), but this client never sent it —
// every call 401ed, every payee resolved as "no ORG-10 coverage" (the
// handler's non-200 path maps to ErrPayeeDestinationServiceUnavailable, not
// ErrNoActiveDestination, so this would NOT have been silently mistaken for
// a legitimate absence either — it was a hard failure, logged as a warning
// and swallowed at the one call site that logs it at all), which silently
// neutered the destination-pinning re-verification gate this client exists
// to feed.
func TestGetActiveDestination_ForwardsPrincipalHeader(t *testing.T) {
	var gotPrincipal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal = r.Header.Get("X-Principal-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Destination{DestinationID: "dest-1", LegalEntityID: "le-1", Status: "ACTIVE"})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())
	dest, err := c.GetActiveDestination(context.Background(), "tenant-1", "principal-1", "le-1", "payee-1")
	if err != nil {
		t.Fatalf("GetActiveDestination: %v", err)
	}
	if gotPrincipal != "principal-1" {
		t.Fatalf("expected X-Principal-Id to be forwarded as %q, got %q", "principal-1", gotPrincipal)
	}
	if dest.DestinationID != "dest-1" {
		t.Fatalf("unexpected destination: %+v", dest)
	}
}

func TestGetActiveDestination_ForwardsTenantHeader(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Destination{DestinationID: "dest-1", LegalEntityID: "le-1", Status: "ACTIVE"})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())
	if _, err := c.GetActiveDestination(context.Background(), "tenant-1", "principal-1", "le-1", "payee-1"); err != nil {
		t.Fatalf("GetActiveDestination: %v", err)
	}
	if gotTenant != "tenant-1" {
		t.Fatalf("expected X-Tenant-Id to be forwarded as %q, got %q", "tenant-1", gotTenant)
	}
}

// TestGetActiveDestination_Unauthorized_FailsClosed documents what the
// pre-fix behavior actually produced downstream: a real server enforcing
// X-Principal-Id returns 401 for a request missing it, which this client
// maps to ErrPayeeDestinationServiceUnavailable — not
// ErrNoActiveDestination. The bug this file pins was never "every payee
// looks uncovered"; it was "every lookup fails," silently downgraded to a
// warning log at the one caller that's best-effort.
func TestGetActiveDestination_Unauthorized_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Principal-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Destination{DestinationID: "dest-1", LegalEntityID: "le-1", Status: "ACTIVE"})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())
	_, err := c.GetActiveDestination(context.Background(), "tenant-1", "", "le-1", "payee-1")
	if err != domain.ErrPayeeDestinationServiceUnavailable {
		t.Fatalf("expected ErrPayeeDestinationServiceUnavailable when X-Principal-Id is missing and the server 401s, got %v", err)
	}
}
