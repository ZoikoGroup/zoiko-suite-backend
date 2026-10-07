package authz_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
)

// ── client selection ─────────────────────────────────────────────────────────

func TestNewClient_LocalEnvironment_UsesStub(t *testing.T) {
	log := zap.NewNop()
	client, err := authz.NewClient("local", "http://authorization-svc", log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected client, got nil")
	}
	if err := client.Authorize(context.Background(), "principal-1", "scope-1", "res", "act", nil); err != nil {
		t.Errorf("expected stub to permit, got %v", err)
	}
}

func TestNewClient_ProductionEnvironment_PlaceholderURL_ReturnsError(t *testing.T) {
	log := zap.NewNop()
	_, err := authz.NewClient("production", "http://authorization-svc", log)
	if err == nil {
		t.Fatal("expected error when starting in production with placeholder URL, got nil")
	}
}

// TestNewClient_ProductionEnvironment_SelfReferentialURL_ReturnsError covers
// the value docker-compose.yml actually shipped: AUTHZ_SERVICE_URL pointed
// back at this service, described in a comment as "mock stub authz points
// back". That was harmless only while Authorize was a no-op; it must not be
// mistaken for a wired authorization service.
func TestNewClient_ProductionEnvironment_SelfReferentialURL_ReturnsError(t *testing.T) {
	_, err := authz.NewClient("production", "http://jurisdiction-svc:8082", zap.NewNop())
	if err == nil {
		t.Fatal("expected error for the self-referential placeholder URL, got nil")
	}
}

func TestNewClient_ProductionEnvironment_ValidURL_ReturnsHTTPClient(t *testing.T) {
	log := zap.NewNop()
	client, err := authz.NewClient("production", "http://real-authz.zoiko.internal", log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected client, got nil")
	}
}

// ── HTTP client behaviour ────────────────────────────────────────────────────

// TestHTTPClient_Granted proves the client sends the contract
// authorization-svc actually implements and accepts a GRANTED decision.
func TestHTTPClient_Granted(t *testing.T) {
	var gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED","decision_basis":"rbac","access_decision_id":"d-1"}`))
	}))
	defer srv.Close()

	c := authz.NewHTTPAuthZClient(srv.URL, zap.NewNop())
	if err := c.Authorize(context.Background(), "principal-1", "scope-1", "jurisdiction_rule", "transition", nil); err != nil {
		t.Fatalf("expected permit, got %v", err)
	}

	if gotPath != "/v1/authorize" {
		t.Errorf("expected POST to /v1/authorize, got %q", gotPath)
	}
	if gotBody["principal_id"] != "principal-1" {
		t.Errorf("principal_id = %q, want principal-1", gotBody["principal_id"])
	}
	if gotBody["legal_entity_id"] != "scope-1" {
		t.Errorf("legal_entity_id = %q, want scope-1", gotBody["legal_entity_id"])
	}
	if gotBody["action_type"] != "JURISDICTION_RULE_TRANSITION" {
		t.Errorf("action_type = %q, want JURISDICTION_RULE_TRANSITION", gotBody["action_type"])
	}
}

// TestHTTPClient_ForwardsTheEnvelopeHeaders is the regression test for the
// defect this client just replaced: authorizeLive sent NO envelope headers
// at all, so authorization-svc's canonical-envelope middleware answered 401
// envelope_incomplete on every single admin write, and this client folded
// that into the indistinguishable ErrAuthZUnavailable — a missing header
// read as authorization-svc being down.
func TestHTTPClient_ForwardsTheEnvelopeHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_outcome":"GRANTED"}`))
	}))
	defer srv.Close()

	// A real inbound request carries a chi request id by the time this client
	// runs; context.Background() alone (as the other tests use) would not.
	ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-123")
	c := authz.NewHTTPAuthZClient(srv.URL, zap.NewNop())
	if err := c.Authorize(ctx, "principal-1", "scope-1", "jurisdiction", "create"); err != nil {
		t.Fatalf("expected permit, got %v", err)
	}

	for _, header := range []string{"X-Request-Id", "X-Source-Channel", "Idempotency-Key", "X-Legal-Entity-Id", "X-Principal-Id"} {
		if got.Get(header) == "" {
			t.Errorf("request is missing %s — authorization-svc will answer 401 envelope_incomplete", header)
		}
	}
	if got.Get("X-Legal-Entity-Id") != "scope-1" {
		t.Errorf("X-Legal-Entity-Id sent as %q, want %q", got.Get("X-Legal-Entity-Id"), "scope-1")
	}
	if got.Get("X-Principal-Id") != "principal-1" {
		t.Errorf("X-Principal-Id sent as %q, want %q", got.Get("X-Principal-Id"), "principal-1")
	}
}

// TestHTTPClient_Denied — authorization-svc answers DENIED with HTTP 200, so
// a client that only checked the status code would permit the action.
func TestHTTPClient_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision_outcome":"DENIED","decision_basis":"no_grant"}`))
	}))
	defer srv.Close()

	c := authz.NewHTTPAuthZClient(srv.URL, zap.NewNop())
	err := c.Authorize(context.Background(), "principal-1", "scope-1", "jurisdiction", "create", nil)
	if !errors.Is(err, authz.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for a DENIED decision, got %v", err)
	}
}

// TestHTTPClient_FailsClosed is the regression test for the defect this
// client replaced: Authorize was a TODO that returned nil, so every admin
// mutation was permitted without a decision. Each case below must deny.
func TestHTTPClient_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "500 from authorization-svc",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "400 missing field",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing_field","field":"legal_entity_id"}`))
			},
		},
		{
			name: "unreadable body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`not json`))
			},
		},
		{
			name: "empty decision outcome",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := authz.NewHTTPAuthZClient(srv.URL, zap.NewNop())
			err := c.Authorize(context.Background(), "principal-1", "scope-1", "jurisdiction", "create", nil)
			if err == nil {
				t.Fatal("FAIL: authorization was permitted without a positive decision")
			}
		})
	}
}

// TestHTTPClient_UnreachableFailsClosed covers the network-error path
// specifically — it must be ErrAuthZUnavailable (503), not a denial (403),
// so operators can tell an outage from a policy decision.
func TestHTTPClient_UnreachableFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := authz.NewHTTPAuthZClient(url, zap.NewNop())
	err := c.Authorize(context.Background(), "principal-1", "scope-1", "jurisdiction", "create", nil)
	if !errors.Is(err, authz.ErrAuthZUnavailable) {
		t.Fatalf("expected ErrAuthZUnavailable for an unreachable service, got %v", err)
	}
}

func TestActionType(t *testing.T) {
	cases := map[[2]string]string{
		{"jurisdiction", "create"}:            "JURISDICTION_CREATE",
		{"jurisdiction", "deactivate"}:        "JURISDICTION_DEACTIVATE",
		{"jurisdiction_rule", "create"}:       "JURISDICTION_RULE_CREATE",
		{"jurisdiction_rule", "transition"}:   "JURISDICTION_RULE_TRANSITION",
		{"jurisdiction_rule", "record_drift"}: "JURISDICTION_RULE_RECORD_DRIFT",
	}
	for in, want := range cases {
		if got := authz.ActionType(in[0], in[1]); got != want {
			t.Errorf("ActionType(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
