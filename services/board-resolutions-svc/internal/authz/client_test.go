// These tests drive a REAL http server rather than a stubbed interface.
//
// The fixture below is the literal shape authorization-svc sends:
// {"decision_outcome":"GRANTED"|"DENIED"}, always over a 200. There is no
// "allowed" boolean. financial-close-svc's client decoded exactly that
// non-existent field, so the value was always Go's zero value false and EVERY
// authorization check denied — a fail-closed check that always fails closed is
// indistinguishable from a permission nobody granted, from either side. Only a
// test that speaks real HTTP catches it.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
	return NewClient(srv.URL).CheckAllowed(context.Background(), "principal-1", "le-us", "RESOLUTION_PASS")
}

func TestCheckAllowed_Granted(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"decision_outcome":"GRANTED"}`)
	if err := check(t, srv); err != nil {
		t.Fatalf("a GRANTED decision was refused: %v", err)
	}
}

func TestCheckAllowed_Denied(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"decision_outcome":"DENIED"}`)
	if err := check(t, srv); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("a DENIED decision returned %v, want ErrAuthorizationDenied", err)
	}
}

// The whole point of the fixture: a response with no decision_outcome must not
// be read as a grant.
func TestCheckAllowed_AbsentDecisionField_FailsClosed(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"allowed":true}`)
	if err := check(t, srv); err == nil {
		t.Fatal("a response with no decision_outcome was treated as a grant")
	}
}

func TestCheckAllowed_Non200_FailsClosed(t *testing.T) {
	srv := newServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	if err := check(t, srv); !errors.Is(err, ErrAuthzServiceUnavailable) {
		t.Fatalf("a 500 returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

func TestCheckAllowed_MalformedBody_FailsClosed(t *testing.T) {
	srv := newServer(t, http.StatusOK, `not json`)
	if err := check(t, srv); !errors.Is(err, ErrAuthzServiceUnavailable) {
		t.Fatalf("a malformed body returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

func TestCheckAllowed_Unreachable_FailsClosed(t *testing.T) {
	err := NewClient("http://127.0.0.1:1").CheckAllowed(context.Background(), "p", "le", "A")
	if !errors.Is(err, ErrAuthzServiceUnavailable) {
		t.Fatalf("an unreachable service returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

// The request half of the contract: authorization-svc reads these three
// snake_case fields, and an empty legal_entity_id is rejected by it outright.
func TestCheckAllowed_SendsTheFieldNamesAuthorizationServiceReads(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"action_type":     "RESOLUTION_PASS",
	} {
		if got[field] != want {
			t.Errorf("request field %q is %v, want %q", field, got[field], want)
		}
	}
}
