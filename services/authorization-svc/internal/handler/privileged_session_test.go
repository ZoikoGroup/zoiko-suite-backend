package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// Privileged Access Management (JIT Elevation - ZS-IAM-001 §13 & §21).
// Tests cover session creation, listing, revocation, and PAM elevation in /v1/authorize.

const (
	testPAMTenantID      = "11111111-1111-4111-8111-111111111111"
	testPAMCaller        = "admin-security-1"
	testPAMElevatedUser  = "operator-ops-9"
	testPAMLegalEntityID = "11111111-1111-4111-8111-aaaaaaaaaaa1"
)

func TestPrivilegedSession_Create_Success(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"requested_actions": ["emergency.override", "tax.reconcile"],
		"ticket_ref": "INC-88992",
		"reason": "Emergency tax reconciliation for month end",
		"duration_seconds": 1800
	}`

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	req.Header.Set("X-Correlation-ID", "corr-pam-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var res domain.PrivilegedSession
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.TenantID != testPAMTenantID {
		t.Errorf("tenant_id = %s, want %s", res.TenantID, testPAMTenantID)
	}
	if res.PrincipalID != testPAMElevatedUser {
		t.Errorf("principal_id = %s, want %s", res.PrincipalID, testPAMElevatedUser)
	}
	if res.TicketRef != "INC-88992" {
		t.Errorf("ticket_ref = %s, want INC-88992", res.TicketRef)
	}
	if res.Status != domain.PrivilegedSessionStatusActive {
		t.Errorf("status = %s, want ACTIVE", res.Status)
	}
	if len(res.RequestedActions) != 2 {
		t.Errorf("requested_actions len = %d, want 2", len(res.RequestedActions))
	}
}

func TestPrivilegedSession_Create_ValidationFailures(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Missing ticket_ref
	bodyNoTicket := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"requested_actions": ["emergency.override"],
		"reason": "Emergency maintenance"
	}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(bodyNoTicket))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ticket_ref: expected 400, got %d", w.Code)
	}

	// Missing reason
	bodyNoReason := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"requested_actions": ["emergency.override"],
		"ticket_ref": "INC-1234"
	}`
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(bodyNoReason))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: expected 400, got %d", w.Code)
	}

	// Missing requested_actions
	bodyNoActions := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"ticket_ref": "INC-1234",
		"reason": "Emergency maintenance"
	}`
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(bodyNoActions))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing requested_actions: expected 400, got %d", w.Code)
	}

	// Foreign tenant mismatch
	bodyMismatch := `{
		"tenant_id": "22222222-2222-4222-8222-222222222222",
		"ticket_ref": "INC-1234",
		"reason": "Emergency maintenance",
		"requested_actions": ["emergency.override"]
	}`
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(bodyMismatch))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign tenant: expected 403, got %d", w.Code)
	}
}

func TestPrivilegedSession_List(t *testing.T) {
	store := &stubStore{
		listPrivilegedSessions: []domain.PrivilegedSession{
			{
				SessionID:        "session-1",
				TenantID:         testPAMTenantID,
				PrincipalID:      testPAMElevatedUser,
				RequestedActions: []string{"action.one"},
				TicketRef:        "INC-1",
				Reason:           "Maintenance",
				Status:           domain.PrivilegedSessionStatusActive,
			},
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/privileged-sessions?principal_id="+testPAMElevatedUser+"&active_only=true", nil)
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		Sessions []domain.PrivilegedSession `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(res.Sessions))
	}
	if res.Sessions[0].SessionID != "session-1" {
		t.Errorf("session_id = %s, want session-1", res.Sessions[0].SessionID)
	}
}

func TestPrivilegedSession_Revoke(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions/ps-uuid-1/revoke", nil)
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var res domain.PrivilegedSession
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Status != domain.PrivilegedSessionStatusRevoked {
		t.Errorf("status = %s, want REVOKED", res.Status)
	}

	// Nonexistent session returns 404
	store.revokePrivilegedErr = domain.ErrPrivilegedSessionNotFound
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions/nonexistent/revoke", nil)
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	req.Header.Set("X-Principal-Id", testPAMCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestAuthorize_PAM_Elevation_Granted(t *testing.T) {
	sessionID := "ps-elevation-12345"
	ticketRef := "INC-99100"

	store := &stubStore{
		// No standing RBAC or delegation grants
		rbacActions: []string{},
		privilegedSession: &domain.PrivilegedSession{
			SessionID:        sessionID,
			TenantID:         testPAMTenantID,
			PrincipalID:      testPAMElevatedUser,
			RequestedActions: []string{"emergency.tax_override", "payroll.emergency_run"},
			TicketRef:        ticketRef,
			Reason:           "System outage emergency recovery",
			Status:           domain.PrivilegedSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(30 * time.Minute),
			CreatedAt:        time.Now().UTC(),
		},
	}

	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"legal_entity_id": "` + testPAMLegalEntityID + `",
		"action_type": "emergency.tax_override",
		"tenant_id": "` + testPAMTenantID + `",
		"privileged_session_id": "` + sessionID + `"
	}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outcome, basis := decodeDecision(t, w)
	if outcome != "GRANTED" {
		t.Fatalf("expected GRANTED via PAM, got %s (basis: %s)", outcome, basis)
	}
	expectedBasis := "pam:session=" + sessionID + ":ticket=" + ticketRef
	if basis != expectedBasis {
		t.Fatalf("basis = %q, want %q", basis, expectedBasis)
	}
}

func TestAuthorize_PAM_Expired_Denied(t *testing.T) {
	sessionID := "ps-expired-1"
	store := &stubStore{
		privilegedSession: &domain.PrivilegedSession{
			SessionID:        sessionID,
			TenantID:         testPAMTenantID,
			PrincipalID:      testPAMElevatedUser,
			RequestedActions: []string{"emergency.override"},
			TicketRef:        "INC-123",
			Status:           domain.PrivilegedSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(-5 * time.Minute), // expired 5 minutes ago!
		},
	}

	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"legal_entity_id": "` + testPAMLegalEntityID + `",
		"action_type": "emergency.override",
		"tenant_id": "` + testPAMTenantID + `",
		"privileged_session_id": "` + sessionID + `"
	}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("expired PAM session must be DENIED, got %s (basis: %s)", outcome, basis)
	}
	if basis != "no_grant" {
		t.Fatalf("basis = %q, want no_grant", basis)
	}
}

func TestAuthorize_PAM_PrincipalMismatch_Denied(t *testing.T) {
	sessionID := "ps-valid-other-principal"
	store := &stubStore{
		privilegedSession: &domain.PrivilegedSession{
			SessionID:        sessionID,
			TenantID:         testPAMTenantID,
			PrincipalID:      "other-operator",
			RequestedActions: []string{"emergency.override"},
			TicketRef:        "INC-123",
			Status:           domain.PrivilegedSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(time.Hour),
		},
	}

	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"legal_entity_id": "` + testPAMLegalEntityID + `",
		"action_type": "emergency.override",
		"tenant_id": "` + testPAMTenantID + `",
		"privileged_session_id": "` + sessionID + `"
	}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("principal mismatch PAM session must be DENIED, got %s (basis: %s)", outcome, basis)
	}
}

func TestAuthorize_PAM_SoD_Enforcement(t *testing.T) {
	// Privileged session elevates for "POST_PAYMENT", but principal already holds "APPROVE_PAYMENT"
	// Static SoD rule forbids holding both simultaneously!
	sessionID := "ps-sod-test"
	store := &stubStore{
		rbacActions: []string{"APPROVE_PAYMENT"},
		privilegedSession: &domain.PrivilegedSession{
			SessionID:        sessionID,
			TenantID:         testPAMTenantID,
			PrincipalID:      testPAMElevatedUser,
			RequestedActions: []string{"POST_PAYMENT"},
			TicketRef:        "INC-SOD-1",
			Status:           domain.PrivilegedSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(time.Hour),
		},
		sodConflictAction: "APPROVE_PAYMENT",
		sodHasConflict:    true,
	}

	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"legal_entity_id": "` + testPAMLegalEntityID + `",
		"action_type": "POST_PAYMENT",
		"tenant_id": "` + testPAMTenantID + `",
		"privileged_session_id": "` + sessionID + `"
	}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("SoD conflict under PAM must be DENIED, got %s (basis: %s)", outcome, basis)
	}
	if basis != "sod:conflict_with=APPROVE_PAYMENT" {
		t.Fatalf("basis = %q, want sod:conflict_with=APPROVE_PAYMENT", basis)
	}
}

func TestAuthorize_PAM_OwnObject_Enforcement(t *testing.T) {
	// Privileged session elevates for "INVOICE_APPROVE", but operator prepared the invoice!
	sessionID := "ps-own-obj"
	store := &stubStore{
		privilegedSession: &domain.PrivilegedSession{
			SessionID:        sessionID,
			TenantID:         testPAMTenantID,
			PrincipalID:      testPAMElevatedUser,
			RequestedActions: []string{"INVOICE_APPROVE"},
			TicketRef:        "INC-OWN-1",
			Status:           domain.PrivilegedSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(time.Hour),
		},
		ownObjectForbidden: true,
	}

	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body := `{
		"principal_id": "` + testPAMElevatedUser + `",
		"legal_entity_id": "` + testPAMLegalEntityID + `",
		"action_type": "INVOICE_APPROVE",
		"tenant_id": "` + testPAMTenantID + `",
		"resource_owner_principal_id": "` + testPAMElevatedUser + `",
		"privileged_session_id": "` + sessionID + `"
	}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testPAMTenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("own-object SoD under PAM must be DENIED, got %s (basis: %s)", outcome, basis)
	}
	if basis != "sod:own_object_forbidden" {
		t.Fatalf("basis = %q, want sod:own_object_forbidden", basis)
	}
}
