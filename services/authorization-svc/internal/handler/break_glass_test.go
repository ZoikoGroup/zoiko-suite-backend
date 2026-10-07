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

// Break-Glass Emergency Access (ZS-IAM-001 §14, §21, Scenario A16, Scenario A17).
// Tests cover emergency session creation, declared incident validation, listing, revocation,
// and emergency elevation in /v1/authorize including mid-operation expiry.

const (
	testBGTenantID      = "22222222-2222-4222-8222-222222222222"
	testBGCaller        = "ops-lead-1"
	testBGElevatedUser  = "incident-responder-9"
	testBGLegalEntityID = "22222222-2222-4222-8222-bbbbbbbbbbb2"
)

func TestBreakGlass_Create_Success(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testBGElevatedUser + `",
		"incident_id": "INC-CRIT-991",
		"reason": "Payment gateway failover outage remediation",
		"requested_actions": ["payment.override", "period.reopen"],
		"duration_seconds": 1800
	}`

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/break-glass-sessions", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	req.Header.Set("X-Correlation-ID", "corr-bg-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var res domain.BreakGlassSession
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.TenantID != testBGTenantID {
		t.Errorf("tenant_id = %s, want %s", res.TenantID, testBGTenantID)
	}
	if res.PrincipalID != testBGElevatedUser {
		t.Errorf("principal_id = %s, want %s", res.PrincipalID, testBGElevatedUser)
	}
	if res.IncidentID != "INC-CRIT-991" {
		t.Errorf("incident_id = %s, want INC-CRIT-991", res.IncidentID)
	}
	if res.Status != domain.BreakGlassSessionStatusActive {
		t.Errorf("status = %s, want ACTIVE", res.Status)
	}
	if len(res.RequestedActions) != 2 {
		t.Errorf("requested_actions len = %d, want 2", len(res.RequestedActions))
	}
}

// Scenario A16: Break-glass used without declared incident is strictly prohibited.
func TestBreakGlass_Create_ScenarioA16_MissingIncidentID(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testBGElevatedUser + `",
		"reason": "Routine maintenance without declared incident",
		"requested_actions": ["payment.override"]
	}`

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/break-glass-sessions", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Must be rejected with 400 Bad Request
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing incident_id (Scenario A16), got %d: %s", w.Code, w.Body.String())
	}

	var res map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["field"] != "incident_id" {
		t.Errorf("expected error on field incident_id, got %v", res)
	}
}

func TestBreakGlass_Create_ValidationFailures(t *testing.T) {
	store := &stubStore{}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Missing reason
	bodyNoReason := `{
		"principal_id": "` + testBGElevatedUser + `",
		"incident_id": "INC-001",
		"requested_actions": ["payment.override"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/break-glass-sessions", bytes.NewBufferString(bodyNoReason))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: expected 400, got %d", w.Code)
	}

	// Missing requested actions
	bodyNoActions := `{
		"principal_id": "` + testBGElevatedUser + `",
		"incident_id": "INC-001",
		"reason": "Emergency fix"
	}`
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/break-glass-sessions", bytes.NewBufferString(bodyNoActions))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing requested_actions: expected 400, got %d", w.Code)
	}
}

func TestBreakGlass_Get_And_List(t *testing.T) {
	store := &stubStore{
		breakGlassSession: &domain.BreakGlassSession{
			SessionID:   "bg-uuid-1",
			TenantID:    testBGTenantID,
			PrincipalID: testBGElevatedUser,
			IncidentID:  "INC-5544",
			Status:      domain.BreakGlassSessionStatusActive,
		},
		listBreakGlassSessions: []domain.BreakGlassSession{
			{SessionID: "bg-uuid-1", TenantID: testBGTenantID, PrincipalID: testBGElevatedUser, IncidentID: "INC-5544"},
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// GET by ID
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/break-glass-sessions/bg-uuid-1", nil)
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// List
	reqList := httptest.NewRequest(http.MethodGet, "/admin/v1/break-glass-sessions?principal_id="+testBGElevatedUser+"&active_only=true", nil)
	reqList.Header.Set("X-Tenant-Id", testBGTenantID)
	reqList.Header.Set("X-Principal-Id", testBGCaller)
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", wList.Code, wList.Body.String())
	}
}

func TestBreakGlass_Revocation(t *testing.T) {
	now := time.Now().UTC()
	store := &stubStore{
		revokeBreakGlassSession: &domain.BreakGlassSession{
			SessionID: "bg-uuid-1",
			TenantID:  testBGTenantID,
			Status:    domain.BreakGlassSessionStatusRevoked,
			RevokedAt: &now,
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/break-glass-sessions/bg-uuid-1/revoke", nil)
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGCaller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorize_BreakGlass_ElevatesActions(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{}, // No standing RBAC grants
		delegatedActions: []string{}, // No delegation
		breakGlassSession: &domain.BreakGlassSession{
			SessionID:        "bg-session-99",
			TenantID:         testBGTenantID,
			PrincipalID:      testBGElevatedUser,
			IncidentID:       "INC-EMERGENCY-1",
			RequestedActions: []string{"emergency.restore", "database.failover"},
			Status:           domain.BreakGlassSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(30 * time.Minute),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testBGElevatedUser + `",
		"legal_entity_id": "` + testBGLegalEntityID + `",
		"action_type": "emergency.restore",
		"break_glass_session_id": "bg-session-99"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGElevatedUser)
	req.Header.Set("X-Correlation-ID", "corr-bg-authz-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outcome, basis := decodeDecision(t, w)
	if outcome != "GRANTED" {
		t.Fatalf("expected GRANTED, got %v (basis: %s)", outcome, basis)
	}
	expectedBasis := "break_glass:session=bg-session-99:incident=INC-EMERGENCY-1"
	if basis != expectedBasis {
		t.Errorf("decision_basis = %v, want %s", basis, expectedBasis)
	}
}

// Scenario A17: Break-glass session expires mid-operation -> revalidate; deny/abort safely.
func TestAuthorize_BreakGlass_ScenarioA17_ExpiredMidOperation(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		breakGlassSession: &domain.BreakGlassSession{
			SessionID:        "bg-expired-1",
			TenantID:         testBGTenantID,
			PrincipalID:      testBGElevatedUser,
			IncidentID:       "INC-EXPIRED-9",
			RequestedActions: []string{"emergency.restore"},
			Status:           domain.BreakGlassSessionStatusExpired, // Expired mid-operation
			ExpiresAt:        time.Now().UTC().Add(-5 * time.Minute),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{
		"principal_id": "` + testBGElevatedUser + `",
		"legal_entity_id": "` + testBGLegalEntityID + `",
		"action_type": "emergency.restore",
		"break_glass_session_id": "bg-expired-1"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", testBGElevatedUser)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	outcome, basis := decodeDecision(t, w)
	// Must be DENIED because session expired
	if outcome != "DENIED" {
		t.Fatalf("expected DENIED for expired break-glass session (Scenario A17), got %v", outcome)
	}
	if basis != "no_grant" {
		t.Errorf("expected basis no_grant, got %v", basis)
	}
}

func TestAuthorize_BreakGlass_PrincipalMismatch(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{},
		delegatedActions: []string{},
		breakGlassSession: &domain.BreakGlassSession{
			SessionID:        "bg-session-userA",
			TenantID:         testBGTenantID,
			PrincipalID:      "authorized-user-A",
			IncidentID:       "INC-1",
			RequestedActions: []string{"emergency.restore"},
			Status:           domain.BreakGlassSessionStatusActive,
			ExpiresAt:        time.Now().UTC().Add(time.Hour),
		},
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Impostor user B attempts to use user A's break-glass session
	body := `{
		"principal_id": "impostor-user-B",
		"legal_entity_id": "` + testBGLegalEntityID + `",
		"action_type": "emergency.restore",
		"break_glass_session_id": "bg-session-userA"
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testBGTenantID)
	req.Header.Set("X-Principal-Id", "impostor-user-B")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	outcome, _ := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("expected DENIED on principal mismatch, got %v", outcome)
	}
}
