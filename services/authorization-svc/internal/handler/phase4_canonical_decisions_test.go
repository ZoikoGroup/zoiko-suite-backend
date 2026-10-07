package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// ── Scenario A06: Dynamic SoD — Supplier Bank Change Proximity ──────────────
// ZS-IAM-001 §10.2 & §27 Acceptance Scenario A06:
// "Payment releaser also changed supplier bank account: Cooling-window conflict.
// Expected result: DENY or require independent high-assurance control per policy."
func TestScenarioA06_PaymentReleaserChangedSupplierBank_CoolingWindowConflict_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		releaserID  = "usr-releaser-001"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release", "payment.view"},
		rbacBasis:   "rbac:role=TREASURY_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// 1. Verify via canonical API: POST /internal/authorization/decisions
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     releaserID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "payment_instruction",
		ResourceID:    "pmt-9988",
		Action:        "payment.release",
		ResourceAttributes: map[string]string{
			"amount":                               "150000.00",
			"currency":                             "USD",
			"supplier_bank_changed_by":             releaserID, // Releaser changed the supplier bank details
			"supplier_bank_cooling_window_active": "true",     // within cooling window
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 120,
			Assurance:       "PHISHING_RESISTANT",
			Risk:            "LOW",
		},
		CorrelationID: "corr-a06-test",
	}

	body, err := json.Marshal(canonicalReq)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", releaserID)
	req.Header.Set("X-Correlation-ID", "corr-a06-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from canonical decisions, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Verify Scenario A06: Expected result is DENY due to cooling-window conflict
	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("Scenario A06: expected decision DENY, got %s", resp.Decision)
	}
	if resp.Basis != "sod:cooling_window_conflict" {
		t.Errorf("Scenario A06: expected basis sod:cooling_window_conflict, got %s", resp.Basis)
	}

	hasCoolingReason := false
	for _, code := range resp.ReasonCodes {
		if code == "COOLING_WINDOW_CONFLICT" || code == "SOD_DYNAMIC_CONFLICT" {
			hasCoolingReason = true
			break
		}
	}
	if !hasCoolingReason {
		t.Errorf("Scenario A06: expected reason codes to contain cooling window conflict, got %v", resp.ReasonCodes)
	}

	hasNegativeControl := false
	for _, nc := range resp.NegativeControls {
		if nc == "dynamic_sod:cooling_window_conflict" {
			hasNegativeControl = true
			break
		}
	}
	if !hasNegativeControl {
		t.Errorf("Scenario A06: expected negative controls to contain dynamic_sod:cooling_window_conflict, got %v", resp.NegativeControls)
	}

	// Also verify that the payment releaser is denied payment.release in available actions
	for _, action := range resp.AvailableActions {
		if action == "payment.release" {
			t.Errorf("Scenario A06: payment.release must NOT be in available_actions when cooling-window conflict exists")
		}
	}

	// 2. Also verify via legacy /v1/authorize endpoint
	v1Body := `{
		"principal_id": "` + releaserID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action_type": "payment.release",
		"tenant_id": "` + tenantID + `",
		"attributes": {
			"supplier_bank_changed_by": "` + releaserID + `",
			"supplier_bank_cooling_window_active": "true"
		}
	}`
	v1Req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(v1Body))
	v1Req.Header.Set("X-Tenant-Id", tenantID)
	v1Req.Header.Set("X-Principal-Id", releaserID)
	v1W := httptest.NewRecorder()
	r.ServeHTTP(v1W, v1Req)

	if v1W.Code != http.StatusOK {
		t.Fatalf("v1/authorize expected 200, got %d: %s", v1W.Code, v1W.Body.String())
	}
	var v1Got map[string]string
	_ = json.Unmarshal(v1W.Body.Bytes(), &v1Got)
	if v1Got["decision_outcome"] != "DENIED" {
		t.Errorf("Scenario A06 in /v1/authorize: expected outcome DENIED, got %s", v1Got["decision_outcome"])
	}
	if v1Got["decision_basis"] != "sod:cooling_window_conflict" {
		t.Errorf("Scenario A06 in /v1/authorize: expected basis sod:cooling_window_conflict, got %s", v1Got["decision_basis"])
	}
}

// TestScenarioA06_IndependentReleaser_NotBlocked verifies that when an independent
// releaser (who did NOT change the supplier bank details) attempts release, it is ALLOWED.
func TestScenarioA06_IndependentReleaser_NotBlocked(t *testing.T) {
	const (
		tenantID         = "11111111-1111-4111-8111-111111111111"
		legalEntity      = "22222222-2222-4222-8222-222222222222"
		independentRelID = "usr-independent-releaser-002"
		bankChangerID    = "usr-supplier-admin-003"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release", "payment.view"},
		rbacBasis:   "rbac:role=TREASURY_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     independentRelID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "payment_instruction",
		ResourceID:    "pmt-9988",
		Action:        "payment.release",
		ResourceAttributes: map[string]string{
			"amount":                               "150000.00",
			"currency":                             "USD",
			"supplier_bank_changed_by":             bankChangerID, // Changed by someone else!
			"supplier_bank_cooling_window_active": "true",
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 60,
			Assurance:       "PHISHING_RESISTANT",
			Risk:            "LOW",
		},
		CorrelationID: "corr-a06-allowed",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected decision ALLOW for independent releaser, got %s", resp.Decision)
	}
	if len(resp.NegativeControls) != 0 {
		t.Errorf("expected no negative controls for independent releaser, got %v", resp.NegativeControls)
	}
}

// ── Scenario A26: Stage 7 Assurance / Step-Up Handling ───────────────────────
// ZS-IAM-001 §7 Stage 7, §8.2 & §27 Acceptance Scenario A26:
// "Session authentication too old for period reopen: Role/authority valid.
// Expected result: STEP_UP; no reopen until recent auth satisfied."
func TestScenarioA26_SessionAuthnTooOldForPeriodReopen_StepUpRequired(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		bookID      = "book-uk-primary"
		controller  = "usr-gl-controller-001"
	)

	store := &stubStore{
		rbacActions: []string{"gl.period.reopen", "gl.period.view"},
		rbacBasis:   "rbac:role=FINANCIAL_CONTROLLER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Authn age = 900 seconds (15 minutes), which is > 300 seconds threshold
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     controller,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		BookID:        bookID,
		ResourceType:  "accounting_period",
		ResourceID:    "period-2026-07",
		Action:        "gl.period.reopen",
		ResourceAttributes: map[string]string{
			"period_name": "July 2026",
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 900, // TOO OLD! (Max allowed is 300s)
			Assurance:       "PHISHING_RESISTANT",
			Risk:            "HIGH",
		},
		CorrelationID: "corr-a26-test",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Scenario A26 Assertions:
	// 1. Decision must be STEP_UP (not ALLOW or DENY)
	if resp.Decision != domain.CanonicalDecisionStepUp {
		t.Fatalf("Scenario A26: expected decision STEP_UP, got %s", resp.Decision)
	}

	// 2. Obligations must include REQUIRE_RECENT_AUTHN
	hasRecentAuthnObligation := false
	for _, ob := range resp.Obligations {
		if ob == "REQUIRE_RECENT_AUTHN" {
			hasRecentAuthnObligation = true
			break
		}
	}
	if !hasRecentAuthnObligation {
		t.Errorf("Scenario A26: expected obligation REQUIRE_RECENT_AUTHN, got %v", resp.Obligations)
	}

	// 3. Reason codes must include STEP_UP_REQUIRED
	hasStepUpReason := false
	for _, code := range resp.ReasonCodes {
		if code == "STEP_UP_REQUIRED" {
			hasStepUpReason = true
			break
		}
	}
	if !hasStepUpReason {
		t.Errorf("Scenario A26: expected reason code STEP_UP_REQUIRED, got %v", resp.ReasonCodes)
	}

	// 4. Step-up metadata must detail required assurance & max authn age
	if resp.StepUp == nil {
		t.Fatalf("Scenario A26: expected step_up object in response, got nil")
	}
	if resp.StepUp.MaxAuthnAgeSeconds != 300 {
		t.Errorf("Scenario A26: expected MaxAuthnAgeSeconds 300, got %d", resp.StepUp.MaxAuthnAgeSeconds)
	}
	if resp.StepUp.RequiredAssurance != "PHISHING_RESISTANT" {
		t.Errorf("Scenario A26: expected RequiredAssurance PHISHING_RESISTANT, got %s", resp.StepUp.RequiredAssurance)
	}

	// 5. gl.period.reopen must not be in available_actions without step-up
	for _, action := range resp.AvailableActions {
		if action == "gl.period.reopen" {
			t.Errorf("Scenario A26: gl.period.reopen must NOT be in available_actions when STEP_UP is pending")
		}
	}
}

// TestScenarioA26_RecentAuthentication_Allowed verifies that when the user has recent
// authentication (authn_age_seconds <= 300), period reopen is ALLOWED.
func TestScenarioA26_RecentAuthentication_Allowed(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		controller  = "usr-gl-controller-001"
	)

	store := &stubStore{
		rbacActions: []string{"gl.period.reopen", "gl.period.view"},
		rbacBasis:   "rbac:role=FINANCIAL_CONTROLLER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Authn age = 45 seconds (RECENT! < 300s)
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     controller,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "accounting_period",
		ResourceID:    "period-2026-07",
		Action:        "gl.period.reopen",
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 45, // FRESH AUTH!
			Assurance:       "PHISHING_RESISTANT",
			Risk:            "LOW",
		},
		CorrelationID: "corr-a26-fresh",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected decision ALLOW with fresh authentication, got %s", resp.Decision)
	}
	if resp.StepUp != nil {
		t.Errorf("expected no step_up requirement with fresh authentication, got %+v", resp.StepUp)
	}
}

// ── Canonical Authorization Decision API Contract (§8.1 & §8.2) ─────────────
func TestCanonicalDecisionContract_Permit(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		user        = "usr-ap-specialist"
	)

	store := &stubStore{
		rbacActions: []string{"invoice.process", "invoice.view"},
		rbacBasis:   "rbac:role=AP_SPECIALIST",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     user,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "invoice",
		ResourceID:    "inv-001",
		Action:        "invoice.process",
		ResourceAttributes: map[string]string{
			"amount":   "500.00",
			"currency": "EUR",
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 100,
			Assurance:       "PHISHING_RESISTANT",
			Risk:            "LOW",
		},
		CorrelationID: "corr-permit-contract",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if resp.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected decision ALLOW, got %s", resp.Decision)
	}
	if resp.DecisionID == "" {
		t.Errorf("expected non-empty decision_id")
	}
	if resp.PolicySetVersion != domain.DefaultPolicySetVersion && resp.PolicySetVersion != "2026.08.24.4" {
		t.Errorf("expected policy_set_version 2026.08.24.4, got %s", resp.PolicySetVersion)
	}
	if len(resp.MatchedGrants) == 0 {
		t.Errorf("expected non-empty matched_grants")
	}
	if len(resp.ReasonCodes) == 0 {
		t.Errorf("expected non-empty reason_codes")
	}
}

func TestCanonicalDecisionContract_MissingFields(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubPublisher{}, &stubValidator{})

	// 1. Missing subject_id
	body1 := `{"action":"invoice.view","legal_entity_id":"22222222-2222-4222-8222-222222222222","tenant_id":"11111111-1111-4111-8111-111111111111"}`
	req1 := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body1))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing subject_id, got %d", w1.Code)
	}

	// 2. Missing action
	body2 := `{"subject_id":"user-1","legal_entity_id":"22222222-2222-4222-8222-222222222222","tenant_id":"11111111-1111-4111-8111-111111111111"}`
	req2 := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body2))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing action, got %d", w2.Code)
	}

	// 3. Missing legal_entity_id
	body3 := `{"subject_id":"user-1","action":"invoice.view","tenant_id":"11111111-1111-4111-8111-111111111111"}`
	req3 := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body3))
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing legal_entity_id, got %d", w3.Code)
	}
}

// ── Available Actions API (ZS-IAM-001 §21, ZS-STATE-001) ────────────────────
func TestAvailableActions_FiltersDynamicSoDAndFlagsStepUp(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-treasury-001"
	)

	// Principal holds payment.view, payment.release, and gl.period.reopen
	store := &stubStore{
		rbacActions: []string{"payment.view", "payment.release", "gl.period.reopen"},
		rbacBasis:   "rbac:role=TREASURY_MANAGER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Calling GET /v1/payment_instruction/pmt-100/available-actions
	// with supplier_bank_changed_by=usr-treasury-001 and cooling_window_active=true
	// and authn_age_seconds=600 (too old for period reopen)
	url := "/v1/payment_instruction/pmt-100/available-actions?" +
		"supplier_bank_changed_by=" + principalID +
		"&supplier_bank_cooling_window_active=true" +
		"&authn_age_seconds=600"

	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Legal-Entity-Id", legalEntity)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.AvailableActionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// 1. payment.view must be in AvailableActions
	hasPaymentView := false
	for _, act := range resp.AvailableActions {
		if act == "payment.view" {
			hasPaymentView = true
			break
		}
	}
	if !hasPaymentView {
		t.Errorf("expected payment.view in available_actions, got %v", resp.AvailableActions)
	}

	// 2. payment.release must be in DeniedActions (due to cooling-window conflict)
	hasDeniedRelease := false
	for _, da := range resp.DeniedActions {
		if da.Action == "payment.release" && da.ReasonCode == "SOD_DYNAMIC_CONFLICT" {
			hasDeniedRelease = true
			break
		}
	}
	if !hasDeniedRelease {
		t.Errorf("expected payment.release in denied_actions with SOD_DYNAMIC_CONFLICT, got %v", resp.DeniedActions)
	}

	// 3. gl.period.reopen must be in StepUpActions (due to authn_age_seconds > 300)
	hasStepUpReopen := false
	for _, sa := range resp.StepUpActions {
		if sa.Action == "gl.period.reopen" {
			hasStepUpReopen = true
			break
		}
	}
	if !hasStepUpReopen {
		t.Errorf("expected gl.period.reopen in step_up_actions, got %v", resp.StepUpActions)
	}
}

// ── Dynamic SoD: Requestor Self-Approval (§10.2) ─────────────────────────────
func TestDynamicSoD_RequestorSelfApproval_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		requestorID = "usr-employee-123"
	)

	store := &stubStore{
		rbacActions: []string{"expense.approve", "expense.view"},
		rbacBasis:   "rbac:role=EXPENSE_APPROVER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     requestorID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "expense_claim",
		ResourceID:    "exp-5544",
		Action:        "expense.approve",
		ResourceAttributes: map[string]string{
			"requestor_id": requestorID, // Self-approval attempt!
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 60,
			Assurance:       "PHISHING_RESISTANT",
		},
		CorrelationID: "corr-self-approve",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY for requestor self-approval, got %s", resp.Decision)
	}
	if resp.Basis != "sod:requestor_self_approval" {
		t.Errorf("expected basis sod:requestor_self_approval, got %s", resp.Basis)
	}
}

// ── Dynamic SoD: Own Access Elevation (§10.2) ────────────────────────────────
func TestDynamicSoD_OwnAccessElevation_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		adminID     = "usr-admin-001"
	)

	store := &stubStore{
		rbacActions: []string{"iam.access_assign", "iam.role.view"},
		rbacBasis:   "rbac:role=IAM_ADMIN",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     adminID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "role_assignment",
		ResourceID:    "assign-new",
		Action:        "iam.access_assign",
		ResourceAttributes: map[string]string{
			"target_subject_id": adminID, // Trying to elevate own access!
		},
		Environment: domain.EnvironmentContext{
			AuthnAgeSeconds: 60,
			Assurance:       "PHISHING_RESISTANT",
		},
		CorrelationID: "corr-elevation-deny",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY for own access elevation, got %s", resp.Decision)
	}
	if resp.Basis != "sod:own_access_elevation" {
		t.Errorf("expected basis sod:own_access_elevation, got %s", resp.Basis)
	}
}

// ── Static SoD Conflict via Canonical API ───────────────────────────────────
func TestCanonicalDecision_StaticSoD_Conflict(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		user        = "usr-cash-manager"
	)

	store := &stubStore{
		rbacActions:       []string{"payment.create", "payment.release"},
		rbacBasis:         "rbac:role=CASH_MANAGER",
		sodHasConflict:    true,
		sodConflictAction: "payment.create",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     user,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "payment.release",
		CorrelationID: "corr-sod-conflict",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY on static SoD conflict, got %s", resp.Decision)
	}
	if resp.Basis != "sod:conflict_with=payment.create" {
		t.Errorf("expected basis sod:conflict_with=payment.create, got %s", resp.Basis)
	}
}

// ── ABAC Denial via Canonical API ───────────────────────────────────────────
func TestCanonicalDecision_ABAC_Denial(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		user        = "usr-wire-operator"
	)

	val := "10000"
	store := &stubStore{
		rbacActions: []string{"wire.transfer"},
		rbacBasis:   "rbac:role=WIRE_OPERATOR",
		abacRules: []domain.ABACRule{
			{
				ABACRuleID:     "rule-amount-cap",
				RuleCode:       "WIRE_AMOUNT_LIMIT",
				ActionType:     "wire.transfer",
				Effect:         domain.EffectForbid,
				AttributeKey:   "amount",
				Operator:       "gt",
				AttributeValue: &val,
				ActiveFlag:     true,
			},
		},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Amount is 50000, which exceeds 10000 limit -> forbidden by ABAC
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     user,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "wire.transfer",
		ResourceAttributes: map[string]string{
			"amount": "50000",
		},
		CorrelationID: "corr-abac-denial",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY for ABAC violation, got %s", resp.Decision)
	}
	if resp.Basis != "abac:forbidden=WIRE_AMOUNT_LIMIT" {
		t.Errorf("expected basis abac:forbidden=WIRE_AMOUNT_LIMIT, got %s", resp.Basis)
	}
}

// ── Principal Status Suspended via Canonical API ────────────────────────────
func TestCanonicalDecision_PrincipalStatus_Suspended(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		user        = "usr-suspended"
	)

	store := &stubStore{
		principalStatus: "SUSPENDED",
		rbacActions:     []string{"invoice.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     user,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "invoice.view",
		CorrelationID: "corr-suspended",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY for suspended principal, got %s", resp.Decision)
	}
	if resp.Basis != "principal_status:SUSPENDED" {
		t.Errorf("expected basis principal_status:SUSPENDED, got %s", resp.Basis)
	}
}

// ── Dynamic SoD — Prior Rejected Reviewer (§10.2 Pattern 5) ─────────────────
func TestDynamicSoD_PriorRejectedReviewer_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		reviewerID  = "usr-reviewer-99"
	)

	store := &stubStore{
		rbacActions: []string{"invoice.approve", "invoice.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// 1. Canonical decision endpoint
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     reviewerID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "invoice.approve",
		ResourceAttributes: map[string]string{
			"prior_rejected_by": reviewerID,
			"is_resubmission":   "true",
		},
		CorrelationID: "corr-prior-rejected",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY for prior rejected reviewer, got %s", resp.Decision)
	}
	if resp.Basis != "sod:prior_rejected_reviewer" {
		t.Errorf("expected basis sod:prior_rejected_reviewer, got %s", resp.Basis)
	}

	// 2. /v1/authorize endpoint
	authReqBody := `{
		"principal_id": "` + reviewerID + `",
		"legal_entity_id": "` + legalEntity + `",
		"tenant_id": "` + tenantID + `",
		"action_type": "invoice.approve",
		"attributes": {
			"prior_rejected_by": "` + reviewerID + `",
			"is_resubmission": "true"
		}
	}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(authReqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Tenant-Id", tenantID)

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	var authResp struct {
		DecisionOutcome string `json:"decision_outcome"`
		DecisionBasis   string `json:"decision_basis"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &authResp)

	if authResp.DecisionOutcome != "DENIED" {
		t.Errorf("expected DENIED on /v1/authorize, got %s", authResp.DecisionOutcome)
	}
	if authResp.DecisionBasis != "sod:prior_rejected_reviewer" {
		t.Errorf("expected basis sod:prior_rejected_reviewer, got %s", authResp.DecisionBasis)
	}
}

// ── Dynamic SoD — Related-Party Conflict (§10.2 Pattern 6) ──────────────────
func TestDynamicSoD_RelatedPartyConflict_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		officerID   = "usr-procurement-officer"
	)

	store := &stubStore{
		rbacActions: []string{"contract.award", "contract.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     officerID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "contract.award",
		ResourceAttributes: map[string]string{
			"is_related_party": "true",
		},
		CorrelationID: "corr-related-party",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY for related-party conflict, got %s", resp.Decision)
	}
	if resp.Basis != "sod:related_party_conflict" {
		t.Errorf("expected basis sod:related_party_conflict, got %s", resp.Basis)
	}
}

// ── Lifecycle State Guards (ZS-STATE-001) ───────────────────────────────────
func TestCanonicalDecision_ResourceLifecycleState_TerminalStatus_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		approverID  = "usr-approver-77"
	)

	store := &stubStore{
		rbacActions: []string{"invoice.approve", "invoice.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     approverID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "invoice.approve",
		ResourceAttributes: map[string]string{
			"status": "VOIDED",
		},
		CorrelationID: "corr-lifecycle-terminal",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY for terminal VOIDED resource status, got %s", resp.Decision)
	}
	if resp.Basis != "state:terminal_status=VOIDED" {
		t.Errorf("expected basis state:terminal_status=VOIDED, got %s", resp.Basis)
	}
}

func TestAvailableActions_ResourceLifecycleState_TerminalStatus_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		userID      = "usr-ops-10"
	)

	store := &stubStore{
		rbacActions: []string{"payment.view", "payment.release"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	url := "/v1/payment_instruction/pmt-terminal-1/available-actions?" +
		"principal_id=" + userID +
		"&tenant_id=" + tenantID +
		"&legal_entity_id=" + legalEntity +
		"&status=CANCELLED"

	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.AvailableActionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if len(resp.AvailableActions) != 1 || resp.AvailableActions[0] != "payment.view" {
		t.Errorf("expected only payment.view to be available on CANCELLED resource, got %v", resp.AvailableActions)
	}

	foundDeniedRelease := false
	for _, da := range resp.DeniedActions {
		if da.Action == "payment.release" && da.ReasonCode == "RESOURCE_STATE_CONFLICT" {
			foundDeniedRelease = true
			break
		}
	}
	if !foundDeniedRelease {
		t.Errorf("expected payment.release to be denied with RESOURCE_STATE_CONFLICT, got %v", resp.DeniedActions)
	}
}

// ── GET /v1/me/capabilities (ZS-IAM-001 §21) ─────────────────────────────────
func TestCapabilities_ActivePrincipal_ReturnsModulesAndFeatures(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		userID      = "usr-finance-lead"
	)

	store := &stubStore{
		rbacActions: []string{"payment.view", "payment.release", "journal.post", "invoice.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	req := httptest.NewRequest(http.MethodGet, "/v1/me/capabilities", nil)
	req.Header.Set("X-Principal-Id", userID)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Legal-Entity-Id", legalEntity)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/me/capabilities, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.CapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if resp.PrincipalID != userID {
		t.Errorf("expected principal %s, got %s", userID, resp.PrincipalID)
	}
	if len(resp.Permissions) != 4 {
		t.Errorf("expected 4 permissions, got %d (%v)", len(resp.Permissions), resp.Permissions)
	}
	if !resp.Features["can_release_payments"] {
		t.Errorf("expected can_release_payments feature to be true")
	}
	if !resp.Features["can_post_journals"] {
		t.Errorf("expected can_post_journals feature to be true")
	}
}

func TestCapabilities_SuspendedPrincipal_Returns403(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		userID      = "usr-suspended-1"
	)

	store := &stubStore{
		principalStatus: "SUSPENDED",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	req := httptest.NewRequest(http.MethodGet, "/v1/me/capabilities", nil)
	req.Header.Set("X-Principal-Id", userID)
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for suspended user on /v1/me/capabilities, got %d: %s", w.Code, w.Body.String())
	}
}

// ── ValidateSoD Candidate Reporting with DynamicRestricted ───────────────────
func TestValidateSoD_ReportsDynamicRestrictedActions(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		callerID    = "usr-admin-1"
		subjectID   = "usr-user-1"
	)

	store := &stubStore{
		rbacActions: []string{"invoice.view"},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	const legalEntity = "22222222-2222-4222-8222-222222222222"

	reqBody := `{
		"principal_id": "` + subjectID + `",
		"legal_entity_id": "` + legalEntity + `",
		"candidate_actions": ["payment.release", "invoice.approve", "general.view"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sod/validate", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", callerID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ConflictFree      bool          `json:"conflict_free"`
		DynamicRestricted []string      `json:"dynamic_restricted"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if !resp.ConflictFree {
		t.Errorf("expected conflict_free true")
	}

	// payment.release and invoice.approve should be in dynamic_restricted
	hasRelease := false
	hasApprove := false
	for _, act := range resp.DynamicRestricted {
		if act == "payment.release" {
			hasRelease = true
		}
		if act == "invoice.approve" {
			hasApprove = true
		}
	}
	if !hasRelease || !hasApprove {
		t.Errorf("expected dynamic_restricted to contain payment.release and invoice.approve, got %v", resp.DynamicRestricted)
	}
}

