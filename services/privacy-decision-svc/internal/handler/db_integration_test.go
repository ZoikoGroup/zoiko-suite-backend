package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/testdb"
)

func newRealDBTestRouter(t *testing.T, db *testdb.DB, purposes *stubPurposeRegistry, consents *stubConsentRegistry, holds *stubHoldRegistry, transfers *stubTransferRegistry) chi.Router {
	t.Helper()
	logger := zap.NewNop()
	pgStore := store.NewPgStore(db.App, logger)
	if transfers == nil {
		transfers = &stubTransferRegistry{}
	}
	h := handler.New(pgStore, purposes, consents, holds, transfers, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func TestHandler_RealDB_EvaluateAndGetDecision_RLS(t *testing.T) {
	db := testdb.Setup(t)
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newRealDBTestRouter(t, db, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	tenantA := "00000000-0000-0000-0000-000000000001"
	tenantB := "00000000-0000-0000-0000-000000000002"
	idemKey := "idem-real-db-" + uuid.New().String()

	reqPayload := domain.EvaluateDecisionRequest{
		SubjectRef:           "user-real-db",
		ProcessingActivityID: testActivity,
		PurposeID:            testPurpose,
		ProposedOperation:    domain.OperationUse,
		SubjectContext:       validSubjectContext(),
	}

	headersA := map[string]string{
		"Idempotency-Key":  idemKey,
		"X-Correlation-ID": "corr-db-001",
		"X-Principal-Id":   "actor-01",
	}

	// 1. POST /privacy/decisions under Tenant A
	wPost := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, tenantA, headersA)
	if wPost.Code != http.StatusOK {
		t.Fatalf("expected 200 on EvaluateDecision, got %d: %s", wPost.Code, wPost.Body.String())
	}

	var d domain.PrivacyDecision
	if err := json.Unmarshal(wPost.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}

	// Verify §18 response contract fields
	if d.DecisionID == "" {
		t.Errorf("expected decision_id to be populated")
	}
	if d.Result != domain.ResultPermit {
		t.Errorf("expected PERMIT, got %s", d.Result)
	}
	if d.ContextHash == "" || d.InputFingerprint == "" {
		t.Errorf("expected context_hash & input_fingerprint to be populated")
	}
	if d.EvidenceRef == "" {
		t.Errorf("expected evidence_ref to be populated")
	}
	if len(d.PolicyRefs) == 0 {
		t.Errorf("expected policy_refs to be populated, got empty")
	}
	if d.TenantID == nil || *d.TenantID != tenantA {
		t.Errorf("expected tenant_id %s, got %v", tenantA, d.TenantID)
	}

	// 2. Idempotent replay under Tenant A returns cached decision
	wReplay := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, tenantA, headersA)
	if wReplay.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d", wReplay.Code)
	}
	if wReplay.Header().Get("Idempotency-Replay") != "true" {
		t.Errorf("expected Idempotency-Replay: true header, got %s", wReplay.Header().Get("Idempotency-Replay"))
	}
	var dReplay domain.PrivacyDecision
	_ = json.Unmarshal(wReplay.Body.Bytes(), &dReplay)
	if dReplay.DecisionID != d.DecisionID {
		t.Errorf("replay decision_id mismatch: %s vs %s", dReplay.DecisionID, d.DecisionID)
	}

	// 3. Replay with conflicting payload under Tenant A returns 409 PRV-020
	conflictPayload := reqPayload
	conflictPayload.SubjectRef = "different-user"
	wConflict := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", conflictPayload, tenantA, headersA)
	if wConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", wConflict.Code)
	}
	var errConflict map[string]string
	_ = json.Unmarshal(wConflict.Body.Bytes(), &errConflict)
	if errConflict["error_code"] != domain.ErrCodeImmutableEvidenceConflict {
		t.Errorf("expected PRV-020, got %s", errConflict["error_code"])
	}

	// 4. GET /privacy/decisions/{id} under Tenant A succeeds
	wGetA := doRequestWithHeaders(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, tenantA, map[string]string{
		"X-Principal-Id": "actor-01",
	})
	if wGetA.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET decision for tenant A, got %d: %s", wGetA.Code, wGetA.Body.String())
	}

	// 5. GET /privacy/decisions/{id} under Tenant B returns 404 (RLS prevented cross-tenant read)
	wGetB := doRequestWithHeaders(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, tenantB, map[string]string{
		"X-Principal-Id": "actor-02",
	})
	if wGetB.Code != http.StatusNotFound {
		t.Fatalf("CRITICAL SECURITY VIOLATION: Tenant B could read Tenant A's decision over HTTP! Status=%d body=%s", wGetB.Code, wGetB.Body.String())
	}
	var errCross map[string]string
	_ = json.Unmarshal(wGetB.Body.Bytes(), &errCross)
	if errCross["error_code"] != domain.ErrCodeNotFound {
		t.Errorf("expected NOT_FOUND, got %s", errCross["error_code"])
	}
}

func TestHandler_RealDB_TypedErrors(t *testing.T) {
	db := testdb.Setup(t)
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newRealDBTestRouter(t, db, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)
	tenantID := "00000000-0000-0000-0000-000000000001"

	// 1. Missing principal -> 401 AUTHENTICATION_FAILED
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "user", ProcessingActivityID: testActivity, PurposeID: testPurpose, ProposedOperation: domain.OperationUse,
	}, tenantID, map[string]string{"X-Principal-Id": ""})
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w1.Code)
	}
	var e1 map[string]string
	_ = json.Unmarshal(w1.Body.Bytes(), &e1)
	if e1["error_code"] != domain.ErrCodeAuthenticationFailed {
		t.Errorf("expected AUTHENTICATION_FAILED, got %s", e1["error_code"])
	}

	// 2. Missing tenant -> 400 TENANT_SCOPE_REQUIRED
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "user", ProcessingActivityID: testActivity, PurposeID: testPurpose, ProposedOperation: domain.OperationUse,
	}, "", nil)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w2.Code)
	}
	var e2 map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &e2)
	if e2["error_code"] != domain.ErrCodeTenantScopeRequired {
		t.Errorf("expected TENANT_SCOPE_REQUIRED, got %s", e2["error_code"])
	}

	// 3. Missing correlation -> 400 CORRELATION_ID_REQUIRED
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "user", ProcessingActivityID: testActivity, PurposeID: testPurpose, ProposedOperation: domain.OperationUse,
	}, tenantID, map[string]string{"X-Correlation-ID": ""})
	if w3.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w3.Code)
	}
	var e3 map[string]string
	_ = json.Unmarshal(w3.Body.Bytes(), &e3)
	if e3["error_code"] != domain.ErrCodeCorrelationRequired {
		t.Errorf("expected CORRELATION_ID_REQUIRED, got %s", e3["error_code"])
	}

	// 4. Missing idempotency key -> 400 IDEMPOTENCY_KEY_REQUIRED
	w4 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "user", ProcessingActivityID: testActivity, PurposeID: testPurpose, ProposedOperation: domain.OperationUse,
	}, tenantID, map[string]string{"Idempotency-Key": ""})
	if w4.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w4.Code)
	}
	var e4 map[string]string
	_ = json.Unmarshal(w4.Body.Bytes(), &e4)
	if e4["error_code"] != domain.ErrCodeIdempotencyRequired {
		t.Errorf("expected IDEMPOTENCY_KEY_REQUIRED, got %s", e4["error_code"])
	}
}
