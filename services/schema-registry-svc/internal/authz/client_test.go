// These tests drive a REAL http server rather than a stubbed interface.
//
// Every handler test in this service replaces this client with a double, so
// nothing exercised the parsing — and parsing a response shape the dependency
// does not actually send is the failure mode that cost financial-close-svc its
// entire write surface: a client decoding an "allowed" boolean authorization-svc
// has never sent, so the field was always absent, always false, and EVERY check
// denied. A fail-closed check that always fails closed is indistinguishable
// from a permission nobody granted, from both sides.
//
// The fixture below is the literal shape authorization-svc sends:
// {"decision_outcome":"GRANTED"|"DENIED"}, always over a 200.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/domain"
)

func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/authorize" {
			t.Errorf("client called %s, want /v1/authorize", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func check(t *testing.T, srv *httptest.Server) error {
	t.Helper()
	return NewHTTPClient(srv.URL, zap.NewNop()).
		CheckSchemaPublishAllowed(context.Background(), "principal-1", "le-us", "corr-1")
}

func TestCheckSchemaPublishAllowed_Granted(t *testing.T) {
	if err := check(t, newServer(t, http.StatusOK, `{"decision_outcome":"GRANTED"}`)); err != nil {
		t.Fatalf("a GRANTED decision was refused: %v", err)
	}
}

func TestCheckSchemaPublishAllowed_Denied(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `{"decision_outcome":"DENIED"}`))
	if !errors.Is(err, domain.ErrPublishDenied) {
		t.Fatalf("a DENIED decision returned %v, want ErrPublishDenied", err)
	}
}

// The shape that matters: a response with no decision_outcome must never be
// read as a grant. The zero value is what a renamed field or a changed
// envelope produces.
func TestCheckSchemaPublishAllowed_AbsentDecisionField_FailsClosed(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `{"allowed":true}`))
	if err == nil {
		t.Fatal("a response with no decision_outcome was treated as a grant")
	}
}

func TestCheckSchemaPublishAllowed_Non200_FailsClosed(t *testing.T) {
	err := check(t, newServer(t, http.StatusInternalServerError, `{"error":"boom"}`))
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
		t.Fatalf("a 500 returned %v, want ErrAuthorizationServiceUnavailable", err)
	}
}

// authorization-svc answers 400 for an empty legal_entity_id. That is exactly
// what this service used to send for a registration carrying no legal entity,
// and it arrives here as "unavailable" — which is why the handler substitutes
// the platform scope rather than passing an empty string down.
func TestCheckSchemaPublishAllowed_BadRequest_FailsClosedAsUnavailable(t *testing.T) {
	err := check(t, newServer(t, http.StatusBadRequest, `{"error":"invalid_scope"}`))
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
		t.Fatalf("a 400 returned %v, want ErrAuthorizationServiceUnavailable", err)
	}
}

func TestCheckSchemaPublishAllowed_MalformedBody_FailsClosed(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `not json`))
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
		t.Fatalf("a malformed body returned %v, want ErrAuthorizationServiceUnavailable", err)
	}
}

func TestCheckSchemaPublishAllowed_Unreachable_FailsClosed(t *testing.T) {
	err := NewHTTPClient("http://127.0.0.1:1", zap.NewNop()).
		CheckSchemaPublishAllowed(context.Background(), "p", "le", "corr")
	if !errors.Is(err, domain.ErrAuthorizationServiceUnavailable) {
		t.Fatalf("an unreachable service returned %v, want ErrAuthorizationServiceUnavailable", err)
	}
}

// The request half of the contract: authorization-svc reads these three
// snake_case fields, and the action type is what the RBAC bundle grants. A
// mismatch here means the seed grants a permission nothing ever asks for.
func TestCheckSchemaPublishAllowed_SendsTheFieldsAuthorizationServiceReads(t *testing.T) {
	var got map[string]any
	var gotCorrelation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCorrelation = r.Header.Get("X-Correlation-ID")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	if err := check(t, srv); err != nil {
		t.Fatalf("check: %v", err)
	}
	for field, want := range map[string]string{
		"principal_id":    "principal-1",
		"legal_entity_id": "le-us",
		"action_type":     "SCHEMA_PUBLISH",
	} {
		if got[field] != want {
			t.Errorf("request field %q is %v, want %q", field, got[field], want)
		}
	}
	if gotCorrelation != "corr-1" {
		t.Errorf("correlation id forwarded as %q, want corr-1 — the decision log is how a refusal is traced back", gotCorrelation)
	}
}
