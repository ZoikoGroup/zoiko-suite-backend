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

func strptr(s string) *string { return &s }

// ── Phase 3.6: Monetary Limit Evaluation ────────────────────────────────────

func TestAuthorityLimit_AmountWithinLimit_Allowed(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)

	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-001",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		LowerLimit:       "100.00",
		UpperLimit:       "50000.00",
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Amount is 25000.00 GBP (between 100 and 50000)
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     principalID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "payment.release",
		ResourceAttributes: map[string]string{
			"amount":   "25000.00",
			"currency": "GBP",
		},
		CorrelationID: "corr-within-limit",
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
		t.Errorf("expected decision ALLOW for amount within limit, got %s", resp.Decision)
	}
}

func TestAuthorityLimit_AmountAboveUpperLimit_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)

	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-001",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		LowerLimit:       "0.00",
		UpperLimit:       "50000.00",
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Amount is 75000.00 GBP (exceeds 50000.00 upper limit)
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     principalID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "payment.release",
		ResourceAttributes: map[string]string{
			"amount":   "75000.00",
			"currency": "GBP",
		},
		CorrelationID: "corr-above-limit",
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
		t.Errorf("expected decision DENY for amount above upper limit, got %s", resp.Decision)
	}
	if resp.Basis != "authority_limit:exceeded:upper_limit" {
		t.Errorf("expected basis authority_limit:exceeded:upper_limit, got %s", resp.Basis)
	}
}

func TestAuthorityLimit_BoundaryCondition_ExactlyAtLimit(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)

	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-001",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		LowerLimit:       "500.00",
		UpperLimit:       "50000.00",
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// 1. Exactly at upper limit: 50000.00 GBP -> ALLOWED (inclusive upper bound)
	body1 := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {"amount": "50000.00", "currency": "GBP"}
	}`
	req1 := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Tenant-Id", tenantID)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	var resp1 domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)
	if resp1.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected ALLOW when amount exactly equals upper limit, got %s", resp1.Decision)
	}

	// 2. Exactly at lower limit: 500.00 GBP -> ALLOWED (inclusive lower bound)
	body2 := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {"amount": "500.00", "currency": "GBP"}
	}`
	req2 := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Tenant-Id", tenantID)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	var resp2 domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected ALLOW when amount exactly equals lower limit, got %s", resp2.Decision)
	}
}

func TestAuthorityLimit_BelowLowerLimit_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)

	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-001",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		LowerLimit:       "1000.00", // Minimum 1000 GBP
		UpperLimit:       "50000.00",
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Amount is 200.00 GBP (< 1000.00 lower limit)
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {"amount": "200.00", "currency": "GBP"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected decision DENY for amount below lower limit, got %s", resp.Decision)
	}
	if resp.Basis != "authority_limit:below_lower_limit" {
		t.Errorf("expected basis authority_limit:below_lower_limit, got %s", resp.Basis)
	}
}

// ── Phase 3.7 & Scenario A09: Currency-Basis Handling ───────────────────────
// ZS-IAM-001 §12, §27 Acceptance Scenario A09:
// "Authority limit exceeded after FX conversion: Local currency comparison exceeds limit.
// Expected result: DENY / route higher authority using deterministic FX basis."
func TestScenarioA09_AuthorityLimitExceededAfterFXConversion_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-treasury-releaser"
	)

	// Upper limit is £50,000 GBP
	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-gbp-50k",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		LowerLimit:       "0.00",
		UpperLimit:       "50000.00", // £50,000 GBP
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=TREASURY_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Transaction requested in USD: $100,000 USD
	// Using standard FX rate: 1 USD = 0.78 GBP -> Converted amount = £78,000 GBP
	// £78,000 GBP > £50,000 GBP limit -> Exceeded after FX conversion!
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     principalID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "payment_instruction",
		ResourceID:    "pmt-fx-001",
		Action:        "payment.release",
		ResourceAttributes: map[string]string{
			"amount":   "100000.00",
			"currency": "USD",
		},
		CorrelationID: "corr-a09-fx-test",
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

	// Scenario A09 Assertions:
	// 1. Must be DENY
	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("Scenario A09: expected decision DENY, got %s", resp.Decision)
	}

	// 2. Basis must clearly report exceeding limit after FX conversion
	if resp.Basis != "authority_limit:exceeded:after_fx_conversion" {
		t.Errorf("Scenario A09: expected basis authority_limit:exceeded:after_fx_conversion, got %s", resp.Basis)
	}

	// 3. Reason codes must include AUTHORITY_LIMIT_EXCEEDED
	hasLimitExceededReason := false
	for _, code := range resp.ReasonCodes {
		if code == "AUTHORITY_LIMIT_EXCEEDED" {
			hasLimitExceededReason = true
			break
		}
	}
	if !hasLimitExceededReason {
		t.Errorf("Scenario A09: expected reason codes to contain AUTHORITY_LIMIT_EXCEEDED, got %v", resp.ReasonCodes)
	}

	// 4. Also verify through /v1/authorize
	v1Body := `{
		"principal_id": "` + principalID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action_type": "payment.release",
		"tenant_id": "` + tenantID + `",
		"attributes": {
			"amount": "100000.00",
			"currency": "USD"
		}
	}`
	v1Req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(v1Body))
	v1Req.Header.Set("X-Tenant-Id", tenantID)
	v1W := httptest.NewRecorder()
	r.ServeHTTP(v1W, v1Req)

	var v1Got map[string]string
	_ = json.Unmarshal(v1W.Body.Bytes(), &v1Got)
	if v1Got["decision_outcome"] != "DENIED" {
		t.Errorf("Scenario A09 in /v1/authorize: expected DENIED, got %s", v1Got["decision_outcome"])
	}
	if v1Got["decision_basis"] != "authority_limit:exceeded:after_fx_conversion" {
		t.Errorf("Scenario A09 in /v1/authorize: expected basis authority_limit:exceeded:after_fx_conversion, got %s", v1Got["decision_basis"])
	}
}

func TestCurrencyMismatch_NoFXBasis_FailsClosed(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)

	limit := domain.AuthorityLimit{
		AuthorityLimitID: "lim-001",
		TenantID:         tenantID,
		PrincipalID:      strptr(principalID),
		AuthorityType:    "payment_release",
		LegalEntityID:    strptr(legalEntity),
		Currency:         "GBP",
		UpperLimit:       "50000.00",
		EffectiveFrom:    time.Now().Add(-1 * time.Hour),
	}

	store := &stubStore{
		rbacActions:     []string{"payment.release"},
		rbacBasis:       "rbac:role=PAYMENT_RELEASER",
		authorityLimits: []domain.AuthorityLimit{limit},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Unknown/unsupported currency "XYZ" with no explicit fx_rate provided -> fail closed!
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {"amount": "1000.00", "currency": "XYZ"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected fail-closed DENY when currency has no FX basis, got %s", resp.Decision)
	}
	if resp.Basis != "authority_limit:currency_conversion_missing" {
		t.Errorf("expected basis authority_limit:currency_conversion_missing, got %s", resp.Basis)
	}
}

// ── Phase 3.8: Quorum / Dual Approval ───────────────────────────────────────

func TestQuorum_DualApproval_Satisfied_Allowed(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release"},
		rbacBasis:   "rbac:role=PAYMENT_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// 2 distinct independent approvers ("approver-1", "approver-2"), preparer is "maker-0"
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {
			"dual_control_required": "true",
			"preparer_id": "maker-0",
			"approved_by": "approver-1, approver-2"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected ALLOW when quorum is satisfied, got %s (basis=%s)", resp.Decision, resp.Basis)
	}
}

func TestQuorum_DualApproval_InsufficientApprovers_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release"},
		rbacBasis:   "rbac:role=PAYMENT_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Only 1 approver ("approver-1"), but dual control requires 2
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {
			"dual_control_required": "true",
			"approved_by": "approver-1"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY when quorum is insufficient, got %s", resp.Decision)
	}
	if resp.Basis != "quorum:insufficient_approvals:have=1:required=2" {
		t.Errorf("expected basis quorum:insufficient_approvals:have=1:required=2, got %s", resp.Basis)
	}
}

func TestQuorum_DuplicateApprover_CannotSatisfyQuorum(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release"},
		rbacBasis:   "rbac:role=PAYMENT_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Same approver listed twice ("approver-1, approver-1")
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {
			"dual_control_required": "true",
			"approved_by": "approver-1, approver-1"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY when duplicate approver tries to satisfy dual control, got %s", resp.Decision)
	}
	if resp.Basis != "quorum:insufficient_approvals:have=1:required=2" {
		t.Errorf("expected basis quorum:insufficient_approvals:have=1:required=2, got %s", resp.Basis)
	}
}

func TestQuorum_PreparerCannotCountTowardApprovalQuorum(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.release"},
		rbacBasis:   "rbac:role=PAYMENT_RELEASER",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Preparer "maker-0" is listed in approved_by alongside "approver-1".
	// Preparer cannot count as an independent checker!
	body := `{
		"subject_id": "` + principalID + `",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action": "payment.release",
		"resource_attributes": {
			"dual_control_required": "true",
			"preparer_id": "maker-0",
			"approved_by": "maker-0, approver-1"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("expected DENY when preparer tries to satisfy approval quorum, got %s", resp.Decision)
	}
	if resp.Basis != "quorum:insufficient_approvals:have=1:required=2" {
		t.Errorf("expected basis have=1:required=2, got %s", resp.Basis)
	}
}

// ── Phase 3.9 & Scenario A10: Execution-Time Revalidation ────────────────────
// ZS-IAM-001 §12, §27 Acceptance Scenario A10:
// "Approval facts mutate after approval: Object fingerprint changes.
// Expected result: Invalidate approval; require reapproval."
func TestScenarioA10_ApprovalFactsMutateAfterApproval_ObjectFingerprintChanged_Denied(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.execute", "payment.release"},
		rbacBasis:   "rbac:role=PAYMENT_EXECUTOR",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Approved with hash "hash-v1-amount-50k-to-supplier-A",
	// but before execution, current fact hash is "hash-v2-amount-50k-to-supplier-B" (mutated facts!)
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     principalID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		ResourceType:  "payment_instruction",
		ResourceID:    "pmt-0099",
		Action:        "payment.execute",
		ResourceAttributes: map[string]string{
			"execution_revalidation": "true",
			"approved_fact_hash":     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"current_fact_hash":      "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", // Changed!
		},
		CorrelationID: "corr-a10-test",
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

	// Scenario A10 Assertions:
	// 1. Must be DENY
	if resp.Decision != domain.CanonicalDecisionDeny {
		t.Errorf("Scenario A10: expected decision DENY, got %s", resp.Decision)
	}

	// 2. Basis must indicate fact hash mismatch
	if resp.Basis != "revalidation:fact_hash_mismatch" {
		t.Errorf("Scenario A10: expected basis revalidation:fact_hash_mismatch, got %s", resp.Basis)
	}

	// 3. Reason codes must include FACT_HASH_MUTATION_DETECTED and REAPPROVAL_REQUIRED
	hasMutationReason := false
	for _, code := range resp.ReasonCodes {
		if code == "FACT_HASH_MUTATION_DETECTED" || code == "REAPPROVAL_REQUIRED" {
			hasMutationReason = true
			break
		}
	}
	if !hasMutationReason {
		t.Errorf("Scenario A10: expected reason codes to indicate fact mutation, got %v", resp.ReasonCodes)
	}

	// 4. Also verify in /v1/authorize
	v1Body := `{
		"principal_id": "` + principalID + `",
		"legal_entity_id": "` + legalEntity + `",
		"action_type": "payment.execute",
		"tenant_id": "` + tenantID + `",
		"attributes": {
			"execution_revalidation": "true",
			"approved_fact_hash": "hash-v1",
			"current_fact_hash": "hash-v2"
		}
	}`
	v1Req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(v1Body))
	v1Req.Header.Set("X-Tenant-Id", tenantID)
	v1W := httptest.NewRecorder()
	r.ServeHTTP(v1W, v1Req)

	var v1Got map[string]string
	_ = json.Unmarshal(v1W.Body.Bytes(), &v1Got)
	if v1Got["decision_outcome"] != "DENIED" {
		t.Errorf("Scenario A10 in /v1/authorize: expected DENIED, got %s", v1Got["decision_outcome"])
	}
	if v1Got["decision_basis"] != "revalidation:fact_hash_mismatch" {
		t.Errorf("Scenario A10 in /v1/authorize: expected basis revalidation:fact_hash_mismatch, got %s", v1Got["decision_basis"])
	}
}

func TestScenarioA10_ApprovalFactsUnchanged_Allowed(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-executor"
	)

	store := &stubStore{
		rbacActions: []string{"payment.execute"},
		rbacBasis:   "rbac:role=PAYMENT_EXECUTOR",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	// Same fact hash: facts have NOT changed
	canonicalReq := domain.CanonicalDecisionRequest{
		SubjectID:     principalID,
		PrincipalType: "HUMAN",
		TenantID:      tenantID,
		LegalEntityID: legalEntity,
		Action:        "payment.execute",
		ResourceAttributes: map[string]string{
			"execution_revalidation": "true",
			"approved_fact_hash":     "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"current_fact_hash":      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		CorrelationID: "corr-a10-unchanged",
	}

	body, _ := json.Marshal(canonicalReq)
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp domain.CanonicalDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Decision != domain.CanonicalDecisionAllow {
		t.Errorf("expected ALLOW when approval facts are unchanged, got %s (basis=%s)", resp.Decision, resp.Basis)
	}
}
