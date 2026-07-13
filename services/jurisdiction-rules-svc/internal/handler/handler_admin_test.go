package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ── mock authz client ────────────────────────────────────────────────────────

// mockAuthZ returns whatever error is configured — nil means permit.
// Used to exercise the 403 (ErrUnauthorized) and 503 (ErrAuthZUnavailable)
// paths without a real Authorization Service.
type mockAuthZ struct {
	err error
}

func (m mockAuthZ) Authorize(_ context.Context, _, _, _ string) error {
	return m.err
}

func nopLogger() *zap.Logger {
	return zap.NewNop()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("failed to encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return executeRequest(h, req)
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	return body
}

// ── CreateJurisdiction ───────────────────────────────────────────────────────

func TestCreateJurisdiction_201_Created(t *testing.T) {
	store := &stubStore{
		createdJurisdiction:    &domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB"},
		jurisdictionWasCreated: true,
	}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions", map[string]any{
		"jurisdiction_code": "GB",
		"jurisdiction_name": "United Kingdom",
		"jurisdiction_type": "COUNTRY",
		"authority_type":    "FEDERAL",
		"effective_from":    time.Now().UTC().Format(time.RFC3339),
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateJurisdiction_200_IdempotentReplay(t *testing.T) {
	store := &stubStore{
		createdJurisdiction:    &domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB"},
		jurisdictionWasCreated: false,
	}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions", map[string]any{"jurisdiction_code": "GB"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent replay, got %d", rr.Code)
	}
}

func TestCreateJurisdiction_409_Conflict(t *testing.T) {
	store := &stubStore{createJurisdictionErr: domain.ErrConflict}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions", map[string]any{"jurisdiction_code": "GB"})

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "conflict" {
		t.Fatalf("expected error=conflict, got %q", got)
	}
}

func TestCreateJurisdiction_400_MalformedBody(t *testing.T) {
	store := &stubStore{}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions", bytes.NewBufferString("{not json"))
	rr := executeRequest(h, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestCreateJurisdiction_403_AuthzDenied(t *testing.T) {
	store := &stubStore{}
	h := newTestRouterWithAuthz(store, mockAuthZ{err: authz.ErrUnauthorized})

	rr := postJSON(t, h, "/v1/admin/jurisdictions", map[string]any{"jurisdiction_code": "GB"})

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "unauthorized" {
		t.Fatalf("expected error=unauthorized, got %q", got)
	}
}

func TestCreateJurisdiction_503_AuthzUnavailable(t *testing.T) {
	store := &stubStore{}
	h := newTestRouterWithAuthz(store, mockAuthZ{err: authz.ErrAuthZUnavailable})

	rr := postJSON(t, h, "/v1/admin/jurisdictions", map[string]any{"jurisdiction_code": "GB"})

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "authz_unavailable" {
		t.Fatalf("expected error=authz_unavailable, got %q", got)
	}
}

// ── DeactivateJurisdiction ───────────────────────────────────────────────────

func TestDeactivateJurisdiction_200_OK(t *testing.T) {
	store := &stubStore{deactivatedJurisdiction: &domain.Jurisdiction{JurisdictionID: "j-1", ActiveFlag: false}}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/deactivate", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeactivateJurisdiction_404_NotFound(t *testing.T) {
	store := &stubStore{deactivateErr: domain.ErrJurisdictionNotFound}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions/unknown/deactivate", nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// ── CreateRule ───────────────────────────────────────────────────────────────

func TestCreateRule_201_Created(t *testing.T) {
	store := &stubStore{
		createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT"},
		ruleWasCreated: true,
	}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", map[string]any{
		"rule_domain":    "PAYROLL",
		"rule_code":      "MIN-WAGE",
		"rule_name":      "Minimum Wage",
		"effective_from": time.Now().UTC().Format(time.RFC3339),
		"rule_payload":   json.RawMessage(`{"applies_to_entity_types":["COMPANY"]}`),
		"rule_status":    "DRAFT",
	})

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateRule_409_Conflict(t *testing.T) {
	store := &stubStore{createRuleErr: domain.ErrConflict}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", map[string]any{"rule_code": "MIN-WAGE"})

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
}

// ── TransitionRuleStatus ─────────────────────────────────────────────────────

func TestTransitionRuleStatus_200_OK(t *testing.T) {
	store := &stubStore{transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"}}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestTransitionRuleStatus_400_UnrecognizedTargetStatus(t *testing.T) {
	store := &stubStore{}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	// "DRAFT" is not a key in ruleStatusAllowedPriors — nothing transitions back to DRAFT.
	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "DRAFT"})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_status" {
		t.Fatalf("expected error=invalid_status, got %q", got)
	}
}

func TestTransitionRuleStatus_409_InvalidTransition(t *testing.T) {
	store := &stubStore{transitionErr: domain.ErrInvalidTransition}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "RETIRED"})

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_transition" {
		t.Fatalf("expected error=invalid_transition, got %q", got)
	}
}

func TestTransitionRuleStatus_404_RuleNotFound(t *testing.T) {
	store := &stubStore{transitionErr: domain.ErrRuleNotFound}
	h := newTestRouterWithAuthz(store, authz.NewStubAuthZClient(nopLogger()))

	rr := postJSON(t, h, "/v1/admin/rules/unknown/transition", map[string]any{"new_status": "ACTIVE"})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "rule_not_found" {
		t.Fatalf("expected error=rule_not_found, got %q", got)
	}
}
