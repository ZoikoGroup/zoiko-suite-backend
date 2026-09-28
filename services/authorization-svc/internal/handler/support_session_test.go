package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// Tenant Support & Controlled Impersonation (ZS-IAM-001 §15, §21, Scenario A14).
// Tests cover purpose-bound support sessions, tenant consent, read-only guardrails,
// bulk export prohibition (Scenario A14), and operator attribution.

const (
	testSupportTenantID      = "33333333-3333-4333-8333-333333333333"
	testSupportAdminCaller   = "tenant-admin-1"
	testSupportOperator      = "support-engineer-42"
	testSupportLegalEntityID = "33333333-3333-4333-8333-cccccccccccc"
)

func TestSupportSession_Create_Success(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"support_operator_id": "` + testSupportOperator + `",
		"ticket_ref": "SUP-77881",
		"purpose": "Investigate invoice tax calculation divergence",
		"read_only": true,
		"allow_bulk_export": false,
		"allowed_actions": ["invoice.read", "tax.read"],
		"duration_seconds": 3600,
		"tenant_consent_obtained": true
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/support/sessions", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportAdminCaller)
	req.Header.Set("X-Correlation-ID", "corr-sup-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var res domain.SupportSession
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.TenantID != testSupportTenantID {
		t.Errorf("tenant_id = %s, want %s", res.TenantID, testSupportTenantID)
	}
	if res.SupportOperatorID != testSupportOperator {
		t.Errorf("support_operator_id = %s, want %s", res.SupportOperatorID, testSupportOperator)
	}
	if res.TicketRef != "SUP-77881" {
		t.Errorf("ticket_ref = %s, want SUP-77881", res.TicketRef)
	}
	if !res.ReadOnly {
		t.Errorf("read_only should default to true")
	}
	if res.AllowBulkExport {
		t.Errorf("allow_bulk_export should default to false (Scenario A14)")
	}
	if res.Status != domain.SupportSessionStatusActive {
		t.Errorf("status = %s, want ACTIVE", res.Status)
	}
}

func TestSupportSession_Create_ValidationFailures(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Missing ticket_ref
	bodyNoTicket := `{
		"support_operator_id": "` + testSupportOperator + `",
		"purpose": "Routine debugging"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/support/sessions", bytes.NewBufferString(bodyNoTicket))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportAdminCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ticket_ref: expected 400, got %d", w.Code)
	}

	// Missing purpose
	bodyNoPurpose := `{
		"support_operator_id": "` + testSupportOperator + `",
		"ticket_ref": "SUP-101"
	}`
	req = httptest.NewRequest(http.MethodPost, "/v1/support/sessions", bytes.NewBufferString(bodyNoPurpose))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportAdminCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing purpose: expected 400, got %d", w.Code)
	}
}

func TestSupportSession_Get_And_List(t *testing.T) {
	store := &stubStore{
		supportSession: &domain.SupportSession{
			SessionID:         "sup-uuid-1",
			TenantID:          testSupportTenantID,
			SupportOperatorID: testSupportOperator,
			TicketRef:         "SUP-991",
			Status:            domain.SupportSessionStatusActive,
		},
		listSupportSessions: []domain.SupportSession{
			{SessionID: "sup-uuid-1", TenantID: testSupportTenantID, SupportOperatorID: testSupportOperator, TicketRef: "SUP-991"},
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// GET by ID
	req := httptest.NewRequest(http.MethodGet, "/v1/support/sessions/sup-uuid-1", nil)
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportAdminCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// List
	reqList := httptest.NewRequest(http.MethodGet, "/v1/support/sessions?active_only=true", nil)
	reqList.Header.Set("X-Tenant-Id", testSupportTenantID)
	reqList.Header.Set("X-Principal-Id", testSupportAdminCaller)
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", wList.Code, wList.Body.String())
	}
}

func TestSupportSession_Revocation(t *testing.T) {
	now := time.Now().UTC()
	store := &stubStore{
		revokeSupportSession: &domain.SupportSession{
			SessionID: "sup-uuid-1",
			TenantID:  testSupportTenantID,
			Status:    domain.SupportSessionStatusRevoked,
			RevokedAt: &now,
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	req := httptest.NewRequest(http.MethodPost, "/v1/support/sessions/sup-uuid-1/revoke", nil)
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportAdminCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorize_SupportSession_DiagnosticRead_Success(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		supportSession: &domain.SupportSession{
			SessionID:         "sup-session-12",
			TenantID:          testSupportTenantID,
			SupportOperatorID: testSupportOperator,
			TicketRef:         "SUP-TICKET-44",
			ReadOnly:          true,
			AllowBulkExport:   false,
			AllowedActions:    []string{"invoice.read", "audit.read"},
			Status:            domain.SupportSessionStatusActive,
			ExpiresAt:         time.Now().UTC().Add(time.Hour),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testSupportOperator + `",
		"legal_entity_id": "` + testSupportLegalEntityID + `",
		"action_type": "invoice.read",
		"support_session_id": "sup-session-12"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportOperator)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outcome, basis := decodeDecision(t, w)
	if outcome != "GRANTED" {
		t.Fatalf("expected GRANTED, got %v (basis: %s)", outcome, basis)
	}
	expectedBasis := "support:session=sup-session-12:operator=" + testSupportOperator + ":ticket=SUP-TICKET-44"
	if basis != expectedBasis {
		t.Errorf("decision_basis = %v, want %s", basis, expectedBasis)
	}
}

// Scenario A14: Support user requests bulk export -> DENY unless separately explicit governed export authority.
func TestAuthorize_SupportSession_ScenarioA14_BulkExportDenied(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		supportSession: &domain.SupportSession{
			SessionID:         "sup-session-12",
			TenantID:          testSupportTenantID,
			SupportOperatorID: testSupportOperator,
			TicketRef:         "SUP-TICKET-44",
			ReadOnly:          true,
			AllowBulkExport:   false, // Bulk export disabled
			AllowedActions:    []string{"invoice.read"},
			Status:            domain.SupportSessionStatusActive,
			ExpiresAt:         time.Now().UTC().Add(time.Hour),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Support operator attempts bulk export
	body := `{
		"principal_id": "` + testSupportOperator + `",
		"legal_entity_id": "` + testSupportLegalEntityID + `",
		"action_type": "invoice.export",
		"support_session_id": "sup-session-12"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportOperator)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("expected DENIED for bulk export attempt (Scenario A14), got %v", outcome)
	}
	if basis != "support:bulk_export_prohibited" {
		t.Errorf("decision_basis = %v, want support:bulk_export_prohibited", basis)
	}
}

func TestAuthorize_SupportSession_ReadOnlyMutation_Denied(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		supportSession: &domain.SupportSession{
			SessionID:         "sup-session-12",
			TenantID:          testSupportTenantID,
			SupportOperatorID: testSupportOperator,
			TicketRef:         "SUP-TICKET-44",
			ReadOnly:          true,
			AllowBulkExport:   false,
			AllowedActions:    []string{"invoice.read"},
			Status:            domain.SupportSessionStatusActive,
			ExpiresAt:         time.Now().UTC().Add(time.Hour),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Support operator attempts mutation on read-only session
	body := `{
		"principal_id": "` + testSupportOperator + `",
		"legal_entity_id": "` + testSupportLegalEntityID + `",
		"action_type": "invoice.edit",
		"support_session_id": "sup-session-12"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", testSupportOperator)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("expected DENIED on read-only mutation, got %v", outcome)
	}
	if basis != "support:read_only_session" {
		t.Errorf("decision_basis = %v, want support:read_only_session", basis)
	}
}

func TestAuthorize_SupportSession_OperatorMismatch(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		supportSession: &domain.SupportSession{
			SessionID:         "sup-session-12",
			TenantID:          testSupportTenantID,
			SupportOperatorID: testSupportOperator,
			TicketRef:         "SUP-TICKET-44",
			Status:            domain.SupportSessionStatusActive,
			ExpiresAt:         time.Now().UTC().Add(time.Hour),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Wrong principal tries to use the support session
	body := `{
		"principal_id": "different-user",
		"legal_entity_id": "` + testSupportLegalEntityID + `",
		"action_type": "invoice.read",
		"support_session_id": "sup-session-12"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testSupportTenantID)
	req.Header.Set("X-Principal-Id", "different-user")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, _ := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("expected DENIED on operator mismatch, got %v", outcome)
	}
}
