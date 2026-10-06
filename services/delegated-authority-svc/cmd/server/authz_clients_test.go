package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
)

// fakeAuthz serves ONLY the routes authorization-svc really serves, with its
// real request and response shapes. The SoD client once posted to a route
// nothing served and decoded a field nothing sent; its unit tests used a stub
// client, so both survived until a live audit. These tests pin the contract.
type fakeAuthz struct {
	sodBody     map[string]any
	sodHeaders  http.Header
	sodReply    string
	authzBody   map[string]any
	authzReply  string
	authzStatus int
}

func (f *fakeAuthz) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sod/validate", func(w http.ResponseWriter, r *http.Request) {
		f.sodHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&f.sodBody)
		_, _ = w.Write([]byte(f.sodReply))
	})
	mux.HandleFunc("POST /v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.authzBody)
		if f.authzStatus != 0 {
			w.WriteHeader(f.authzStatus)
		}
		_, _ = w.Write([]byte(f.authzReply))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sodClient(t *testing.T, f *fakeAuthz) *httpSoDClient {
	return &httpSoDClient{baseURL: f.server(t).URL, client: &http.Client{Timeout: 5 * time.Second}, log: zap.NewNop()}
}

func TestSoDClient_SpeaksTheRealContract(t *testing.T) {
	f := &fakeAuthz{sodReply: `{"conflict_free":true,"conflicts":[]}`}
	if err := sodClient(t, f).CheckConflict(context.Background(), "t1", "le-1", "delegator", "delegate", "PAYMENT_APPROVE"); err != nil {
		t.Fatalf("a conflict-free answer must permit: %v", err)
	}
	if f.sodBody["principal_id"] != "delegate" {
		t.Errorf("the DELEGATE's holdings are what conflict; got principal_id=%v", f.sodBody["principal_id"])
	}
	if acts, _ := f.sodBody["candidate_actions"].([]any); len(acts) != 1 || acts[0] != "PAYMENT_APPROVE" {
		t.Errorf("candidate_actions = %v", f.sodBody["candidate_actions"])
	}
	if f.sodBody["legal_entity_id"] != "le-1" || f.sodBody["tenant_id"] != "t1" {
		t.Errorf("entity/tenant not sent: %v", f.sodBody)
	}
	for _, h := range []string{"X-Principal-Id", "X-Tenant-Id", "X-Request-Id", "X-Correlation-ID", "X-Source-Channel", "Idempotency-Key"} {
		if f.sodHeaders.Get(h) == "" {
			t.Errorf("envelope header %s missing — authorization-svc answers 400 without it", h)
		}
	}
}

func TestSoDClient_ConflictIsRefusedWithDetail(t *testing.T) {
	f := &fakeAuthz{sodReply: `{"conflict_free":false,"conflicts":[{"candidate_action":"PAYMENT_APPROVE","conflicts_with":"PAYMENT_RELEASE","source":"held"}]}`}
	err := sodClient(t, f).CheckConflict(context.Background(), "t1", "le-1", "a", "b", "PAYMENT_APPROVE")
	if !errors.Is(err, domain.ErrSODConflict) {
		t.Fatalf("want ErrSODConflict, got %v", err)
	}
}

// An answer that does not say conflict_free is unreadable, and unreadable
// refuses. Decoding it as "no conflict" is the fail-open this guards.
func TestSoDClient_UnreadableAnswerFailsClosed(t *testing.T) {
	for name, reply := range map[string]string{"old shape": `{"conflict":false}`, "empty": `{}`, "garbage": `not json`} {
		f := &fakeAuthz{sodReply: reply}
		err := sodClient(t, f).CheckConflict(context.Background(), "t1", "le-1", "a", "b", "X")
		if !errors.Is(err, domain.ErrAuthzServiceUnavailable) {
			t.Errorf("%s: want unavailable (fail closed), got %v", name, err)
		}
	}
}

func authzClient(t *testing.T, f *fakeAuthz) *httpAuthzClient {
	return &httpAuthzClient{baseURL: f.server(t).URL, client: &http.Client{Timeout: 5 * time.Second}, log: zap.NewNop(),
		cache: map[string]cachedDecision{}}
}

// ORG-06 negative case 11: the delegator is asked about the ceiling being
// delegated. The limit is not required: a delegator with none may narrow.
func TestAuthzClient_CheckAllowedAtLimitSendsTheCeiling(t *testing.T) {
	f := &fakeAuthz{authzReply: `{"decision_outcome":"GRANTED"}`}
	if err := authzClient(t, f).CheckAllowedAtLimit(context.Background(), "delegator", "le-1", "PAYMENT_APPROVE", "500.00", "USD"); err != nil {
		t.Fatal(err)
	}
	attrs, _ := f.authzBody["attributes"].(map[string]any)
	if attrs["amount"] != "500.00" || attrs["currency"] != "USD" || attrs["authority_limit_required"] != nil {
		t.Fatalf("attributes = %v", attrs)
	}
	f.authzReply = `{"decision_outcome":"DENIED"}`
	if err := authzClient(t, f).CheckAllowedAtLimit(context.Background(), "delegator", "le-1", "PAYMENT_APPROVE", "900.00", "USD"); !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Fatalf("a ceiling above the delegator's own must be denied, got %v", err)
	}
}

func TestAuthzClient_PlainCheckSendsNoAttributes(t *testing.T) {
	f := &fakeAuthz{authzReply: `{"decision_outcome":"GRANTED"}`}
	if err := authzClient(t, f).CheckAllowed(context.Background(), "p", "le-1", "DELEGATION_CREATE"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.authzBody["attributes"]; ok {
		t.Fatalf("a plain authorization must not carry limit attributes: %v", f.authzBody)
	}
}

// A delegator who holds the action only by delegation cannot pass it on:
// authorization-svc's basis "delegated:from=…" says so.
func TestAuthzClient_HeldInOwnRightReadsTheBasis(t *testing.T) {
	f := &fakeAuthz{authzReply: `{"decision_outcome":"GRANTED","decision_basis":"rbac:role=FIN"}`}
	if err := authzClient(t, f).CheckHeldInOwnRight(context.Background(), "p", "le-1", "PAYMENT_APPROVE"); err != nil {
		t.Fatalf("a role grant is held in own right: %v", err)
	}
	f.authzReply = `{"decision_outcome":"GRANTED","decision_basis":"delegated:from=cfo"}`
	if err := authzClient(t, f).CheckHeldInOwnRight(context.Background(), "p", "le-1", "PAYMENT_APPROVE"); !errors.Is(err, domain.ErrDelegatorAuthorityDelegated) {
		t.Fatalf("authority held by delegation must be refused, got %v", err)
	}
	// A CAPPED delegation asked about with no amount is denied on the
	// delegation's ceiling, a layer that runs only for delegation-based grants.
	f.authzReply = `{"decision_outcome":"DENIED","decision_basis":"delegation_limit:amount_required"}`
	if err := authzClient(t, f).CheckHeldInOwnRight(context.Background(), "p", "le-1", "PAYMENT_APPROVE"); !errors.Is(err, domain.ErrDelegatorAuthorityDelegated) {
		t.Fatalf("a capped delegation is still authority held by delegation, got %v", err)
	}
	f.authzReply = `{"decision_outcome":"DENIED","decision_basis":"no_grant"}`
	if err := authzClient(t, f).CheckHeldInOwnRight(context.Background(), "p", "le-1", "PAYMENT_APPROVE"); !errors.Is(err, domain.ErrAuthorizationDenied) {
		t.Fatalf("no grant at all is a denial, got %v", err)
	}
}
