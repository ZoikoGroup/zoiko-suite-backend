package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
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
	return NewClient(srv.URL, zap.NewNop()).
		CheckAllowed(context.Background(), "principal-1", "le-us", "NOTIFICATION_SEND")
}

func TestCheckAllowed_Granted(t *testing.T) {
	if err := check(t, newServer(t, http.StatusOK, `{"decision_outcome":"GRANTED"}`)); err != nil {
		t.Fatalf("a GRANTED decision was refused: %v", err)
	}
}

func TestCheckAllowed_Denied(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `{"decision_outcome":"DENIED"}`))
	if !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Fatalf("a DENIED decision returned %v, want ErrAuthorizationDenied", err)
	}
}

// The shape that cost financial-close-svc its entire write surface: a client
// reading a field authorization-svc does not send. Absent → zero value →
// silently not a grant. This must be an unusable-answer error, not a decision.
func TestCheckAllowed_AbsentDecisionField_FailsClosedAsUnavailable(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `{"allowed":true}`))
	if err == nil {
		t.Fatal("a response with no decision_outcome was treated as a grant")
	}
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("returned %v, want ErrAuthzServiceUnavailable — an unparseable answer is not a denial", err)
	}
}

func TestCheckAllowed_Non200_FailsClosed(t *testing.T) {
	err := check(t, newServer(t, http.StatusInternalServerError, `{"error":"boom"}`))
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("a 500 returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

func TestCheckAllowed_MalformedBody_FailsClosed(t *testing.T) {
	err := check(t, newServer(t, http.StatusOK, `not json`))
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("a malformed body returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

func TestCheckAllowed_Unreachable_FailsClosed(t *testing.T) {
	err := NewClient("http://127.0.0.1:1", zap.NewNop()).
		CheckAllowed(context.Background(), "p", "le", "A")
	if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
		t.Fatalf("an unreachable service returned %v, want ErrAuthzServiceUnavailable", err)
	}
}

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
		"action_type":     "NOTIFICATION_SEND",
	} {
		if got[field] != want {
			t.Errorf("request field %q is %v, want %q", field, got[field], want)
		}
	}
}
