package clients_test

// Tests for the authorization client, driven against a stub that replies with
// authorization-svc's ACTUAL response shape.
//
// This is the gap that let the original defect live: every handler test stubs
// the AuthZClient interface out, so nothing anywhere exercised the JSON
// parsing. The client decoded `{"allowed": bool}` — a field authorization-svc
// has never returned — so it always decoded to false and every check denied.
// A fail-closed authorization check that always fails closed is invisible: it
// looks exactly like a permission nobody granted.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
)

func authzServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/authorize" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAuthzClient(authzURL string) *clients.Clients {
	return clients.New(authzURL, "http://ledger.invalid", "http://ap.invalid", "http://ar.invalid", "http://vault.invalid", zap.NewNop())
}

// The shape authorization-svc actually returns.
func TestCheckAllowed_GrantedDecisionOutcome_IsAllowed(t *testing.T) {
	srv := authzServer(t, http.StatusOK, map[string]string{
		"decision_outcome": "GRANTED",
		"decision_basis":   "rbac:role=CONSOLE_DEMO_OPERATOR",
	})

	err := newAuthzClient(srv.URL).CheckAllowed(t.Context(), "principal-1", "le-1", "PERIOD_CLOSE_CONFIG")
	if err != nil {
		t.Fatalf("a GRANTED decision must be allowed, got %v. This is the defect that made every "+
			"write path in this service answer 403 for every principal: the client decoded an "+
			"`allowed` boolean that authorization-svc does not send.", err)
	}
}

func TestCheckAllowed_DeniedDecisionOutcome_IsDenied(t *testing.T) {
	srv := authzServer(t, http.StatusOK, map[string]string{
		"decision_outcome": "DENIED",
		"decision_basis":   "no matching grant",
	})

	err := newAuthzClient(srv.URL).CheckAllowed(t.Context(), "principal-1", "le-1", "PERIOD_CLOSE_CONFIG")
	if !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Fatalf("expected ErrAuthorizationDenied, got %v", err)
	}
}

// A reply this client cannot understand must never read as permission — which
// is the one thing the old parsing got right, by accident, while getting the
// grant case wrong.
func TestCheckAllowed_UnrecognisedBody_IsDenied(t *testing.T) {
	for _, body := range []any{
		map[string]string{},                    // no outcome at all
		map[string]bool{"allowed": true},       // the shape this client used to expect
		map[string]string{"outcome": "GRANT"},  // a plausible near-miss
		map[string]string{"decision": "ALLOW"}, // another
	} {
		srv := authzServer(t, http.StatusOK, body)
		err := newAuthzClient(srv.URL).CheckAllowed(t.Context(), "principal-1", "le-1", "PERIOD_CLOSE_CONFIG")
		if !errors.Is(err, domain.ErrAuthorizationDenied) {
			t.Fatalf("body %v: expected a denial, got %v", body, err)
		}
	}
}

func TestCheckAllowed_ServiceUnreachable_FailsClosed(t *testing.T) {
	// A port nothing is listening on.
	err := newAuthzClient("http://127.0.0.1:1").CheckAllowed(t.Context(), "principal-1", "le-1", "PERIOD_CLOSE_CONFIG")
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("expected ErrAuthzServiceUnavailable, got %v", err)
	}
}

func TestCheckAllowed_Non200_FailsClosed(t *testing.T) {
	srv := authzServer(t, http.StatusInternalServerError, nil)
	err := newAuthzClient(srv.URL).CheckAllowed(t.Context(), "principal-1", "le-1", "PERIOD_CLOSE_CONFIG")
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("expected ErrAuthzServiceUnavailable, got %v", err)
	}
}

// ── the document vault ───────────────────────────────────────────────────────

// document-vault-svc returns the created document object itself. This client
// used to decode a `{"document": {...}}` envelope that service does not send,
// so the id was always empty and every close ended in "document_id was missing
// in vault response" — the same class of mismatch as the authz one above, and
// equally invisible to a suite that stubs the dependency.
func TestUploadCloseEvidence_ReadsTheDocumentIDFromTheRealShape(t *testing.T) {
	var gotTenantHeader, gotPrincipalHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenantHeader = r.Header.Get("X-Tenant-Id")
		gotPrincipalHeader = r.Header.Get("X-Principal-Id")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"document_id":     "doc-abc-123",
			"tenant_id":       "tenant-1",
			"title":           "Close Evidence Trail Balance — 2026-01",
			"classification":  "CONFIDENTIAL",
			"current_version": 1,
			"status":          "ACTIVE",
		})
	}))
	defer srv.Close()

	c := clients.New("http://authz.invalid", "http://ledger.invalid", "http://ap.invalid", "http://ar.invalid", srv.URL, zap.NewNop())
	id, err := c.UploadCloseEvidence(t.Context(), "tenant-1", "le-1", "2026-01",
		map[string]float64{"1000-Cash": 10}, "principal-1")
	if err != nil {
		t.Fatalf("UploadCloseEvidence: %v", err)
	}
	if id != "doc-abc-123" {
		t.Fatalf("document id = %q, want doc-abc-123", id)
	}
	if gotTenantHeader != "tenant-1" {
		t.Fatalf("X-Tenant-Id = %q, want tenant-1", gotTenantHeader)
	}
	if gotPrincipalHeader != "principal-1" {
		t.Fatalf("X-Principal-Id = %q, want principal-1", gotPrincipalHeader)
	}
}

// A reply with no document id must be an error, not an empty id quietly stored
// as the period's evidence reference.
func TestUploadCloseEvidence_NoDocumentID_IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ACTIVE"})
	}))
	defer srv.Close()

	c := clients.New("http://authz.invalid", "http://ledger.invalid", "http://ap.invalid", "http://ar.invalid", srv.URL, zap.NewNop())
	if _, err := c.UploadCloseEvidence(t.Context(), "tenant-1", "le-1", "2026-01",
		map[string]float64{"1000-Cash": 10}, "principal-1"); err == nil {
		t.Fatal("expected an error when the vault returns no document_id")
	}
}
