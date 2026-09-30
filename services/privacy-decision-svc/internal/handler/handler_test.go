package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/consentregistry"
	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/events"
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	decisions   map[string]*domain.PrivacyDecision
	idempotency map[string]*domain.IdempotencyRecord
}

func newStubStore() *stubStore {
	return &stubStore{
		decisions:   map[string]*domain.PrivacyDecision{},
		idempotency: map[string]*domain.IdempotencyRecord{},
	}
}

func (s *stubStore) RecordDecision(_ context.Context, tenantID string, d *domain.PrivacyDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	d.DecidedAt = time.Now().UTC()
	if tenantID != "" {
		d.TenantID = &tenantID
	}
	cp := *d
	s.decisions[d.DecisionID] = &cp
	return nil
}

func (s *stubStore) FindDecision(_ context.Context, decisionID string) (*domain.PrivacyDecision, error) {
	d, ok := s.decisions[decisionID]
	if !ok {
		return nil, domain.ErrDecisionNotFound
	}
	return d, nil
}

func (s *stubStore) GetIdempotency(_ context.Context, tenantID, key string) (*domain.IdempotencyRecord, error) {
	rec, ok := s.idempotency[tenantID+":"+key]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (s *stubStore) SaveIdempotency(_ context.Context, rec domain.IdempotencyRecord) error {
	cp := rec
	s.idempotency[rec.TenantID+":"+rec.Key] = &cp
	return nil
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub purpose registry ───────────────────────────────────────────────────

type stubPurposeRegistry struct {
	activities map[string]*purposeregistry.ActivityVersion
	purposes   map[string]*purposeregistry.PurposeVersion
	err        error
}

func newStubPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{
		activities: map[string]*purposeregistry.ActivityVersion{},
		purposes:   map[string]*purposeregistry.PurposeVersion{},
	}
}

func (p *stubPurposeRegistry) ResolveActivity(_ context.Context, activityID string) (*purposeregistry.ActivityVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.activities[activityID], nil
}

func (p *stubPurposeRegistry) ResolvePurpose(_ context.Context, purposeID string) (*purposeregistry.PurposeVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.purposes[purposeID], nil
}

// ── stub consent registry ────────────────────────────────────────────────────

type stubConsentRegistry struct {
	status           string
	consentReceiptID string
	noticeVersionID  string
	err              error
}

func (c *stubConsentRegistry) ResolveStatus(_ context.Context, _, _ string) (*consentregistry.ConsentResolution, error) {
	if c.err != nil {
		return nil, c.err
	}
	res := &consentregistry.ConsentResolution{Status: c.status}
	if c.consentReceiptID != "" || c.noticeVersionID != "" {
		res.LatestReceipt = &struct {
			ConsentReceiptID string `json:"consent_receipt_id"`
			NoticeVersionID  string `json:"notice_version_id,omitempty"`
		}{
			ConsentReceiptID: c.consentReceiptID,
			NoticeVersionID:  c.noticeVersionID,
		}
	}
	return res, nil
}

// ── stub hold registry ───────────────────────────────────────────────────────

type stubHoldRegistry struct {
	blocked bool
	holdID  string
	err     error
}

func (h *stubHoldRegistry) Resolve(_ context.Context, _, _, _ string) (*retentionregistry.RetentionResolution, error) {
	if h.err != nil {
		return nil, h.err
	}
	res := &retentionregistry.RetentionResolution{Blocked: h.blocked}
	if h.blocked && h.holdID != "" {
		res.MatchedHold = &retentionregistry.LegalHold{LegalHoldID: h.holdID}
	}
	return res, nil
}

// ── stub transfer registry ───────────────────────────────────────────────────

type stubTransferRegistry struct {
	decision *transferregistry.TransferDecision
	err      error
}

func (t *stubTransferRegistry) EvaluateTransfer(_ context.Context, _, _ string, req *transferregistry.EvaluateTransferRequest) (*transferregistry.TransferDecision, error) {
	if t.err != nil {
		return nil, t.err
	}
	if t.decision != nil {
		return t.decision, nil
	}
	return &transferregistry.TransferDecision{
		DecisionID:          "td-12345",
		Result:              "AUTHORIZED",
		TransferMechanismID: req.TransferMechanismID,
		RelationshipID:      req.RelationshipID,
	}, nil
}

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-decision-1"
const testActivity = "activity-hr-analytics"
const testPurpose = "purpose-hr-analytics"

func activeActivity() *purposeregistry.ActivityVersion {
	return &purposeregistry.ActivityVersion{
		ActivityVersionID: "av-1", ActivityID: testActivity, PurposeIDs: []string{testPurpose}, VersionStatus: "ACTIVE",
	}
}

func publishedPurpose() *purposeregistry.PurposeVersion {
	return &purposeregistry.PurposeVersion{PurposeVersionID: "pv-1", PurposeID: testPurpose, VersionStatus: "PUBLISHED"}
}

func newTestRouter(
	st *stubStore,
	pub *stubPublisher,
	purposes *stubPurposeRegistry,
	consents *stubConsentRegistry,
	holds *stubHoldRegistry,
	transfers *stubTransferRegistry,
) chi.Router {
	logger := zap.NewNop()
	if transfers == nil {
		transfers = &stubTransferRegistry{}
	}
	h := handler.New(st, pub, purposes, consents, holds, transfers, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestWithHeaders(r, method, path, body, tenantID, nil)
}

func doRequestWithHeaders(r http.Handler, method, path string, body interface{}, tenantID string, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestEvaluate_Permit_NoOptionalChecks(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// Canonical route POST /privacy/decisions
	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on /privacy/decisions, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultPermit {
		t.Fatalf("expected PERMIT, got %s (reasons=%v)", d.Result, d.ReasonCodes)
	}
	if d.ActivityVersionID == nil || *d.ActivityVersionID != "av-1" {
		t.Fatalf("expected resolved activity_version_id captured, got %v", d.ActivityVersionID)
	}
	if d.PurposeVersionID == nil || *d.PurposeVersionID != "pv-1" {
		t.Fatalf("expected resolved purpose_version_id captured, got %v", d.PurposeVersionID)
	}
	if d.InputFingerprint == "" {
		t.Fatal("expected input_fingerprint to be populated")
	}
	if pub.calls != 1 {
		t.Errorf("expected privacy.decision.evaluated published once, got %d", pub.calls)
	}

	// Canonical route GET /privacy/decisions/{id}
	wGet := doRequest(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, testTenant)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 on get, got %d: %s", wGet.Code, wGet.Body.String())
	}

	// Backwards-compatible route /v1/privacy/decisions/{id}
	wGetV1 := doRequest(r, http.MethodGet, "/v1/privacy/decisions/"+d.DecisionID, nil, testTenant)
	if wGetV1.Code != http.StatusOK {
		t.Fatalf("expected 200 on get /v1/..., got %d: %s", wGetV1.Code, wGetV1.Body.String())
	}
}

func TestEvaluate_IdempotencyKey_ReplayAndConflict(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	reqPayload := domain.EvaluateDecisionRequest{
		SubjectRef: "subject-idem", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}

	idemKey := "idem-key-decision-001"
	headers := map[string]string{"Idempotency-Key": idemKey}

	// 1. Initial request
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, testTenant, headers)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 on initial request, got %d: %s", w1.Code, w1.Body.String())
	}
	var d1 domain.PrivacyDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d1)

	// 2. Replay with identical payload -> 200 OK with Idempotency-Replay: true header
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, testTenant, headers)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d", w2.Code)
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay: true header, got %s", w2.Header().Get("Idempotency-Replay"))
	}
	var d2 domain.PrivacyDecision
	_ = json.Unmarshal(w2.Body.Bytes(), &d2)
	if d1.DecisionID != d2.DecisionID {
		t.Fatalf("expected identical decision_id %s, got %s", d1.DecisionID, d2.DecisionID)
	}

	// 3. Request with same Idempotency-Key but conflicting payload -> 409 Conflict
	conflictingPayload := domain.EvaluateDecisionRequest{
		SubjectRef: "subject-DIFFERENT", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationAccess,
	}
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", conflictingPayload, testTenant, headers)
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payload mismatch, got %d: %s", w3.Code, w3.Body.String())
	}
}

func TestEvaluate_Block_ActivityNotActive(t *testing.T) {
	purposes := newStubPurposeRegistry()
	inactive := activeActivity()
	inactive.VersionStatus = "SUSPENDED"
	purposes.activities[testActivity] = inactive
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("expected BLOCK, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV002ProcessingActivityInactive {
		t.Fatalf("expected reason PRV-002: PROCESSING_ACTIVITY_INACTIVE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Block_PurposeNotBoundToActivity_Safeguard1(t *testing.T) {
	purposes := newStubPurposeRegistry()
	activity := activeActivity()
	activity.PurposeIDs = []string{"some-other-purpose"} // testPurpose not bound
	purposes.activities[testActivity] = activity
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("Safeguard 1: expected BLOCK for purpose not bound to activity, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV009PurposeIncompatible {
		t.Fatalf("expected reason PRV-009: PURPOSE_INCOMPATIBLE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Block_PurposeNotPublished(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	// purposes.purposes left empty

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock || d.ReasonCodes[0] != domain.PRV001PurposeNotRegistered {
		t.Fatalf("expected BLOCK/PRV-001: PURPOSE_NOT_REGISTERED, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluate_ReviewRequired_DpiaAssessmentRequired(t *testing.T) {
	purposes := newStubPurposeRegistry()
	act := activeActivity()
	act.DpiaTiaStatus = "REVIEW_REQUIRED" // requires privacy review
	purposes.activities[testActivity] = act
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED when activity has REVIEW_REQUIRED DPIA status, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV016AssessmentRequired {
		t.Fatalf("expected PRV-016: ASSESSMENT_REQUIRED, got %v", d.ReasonCodes)
	}
}

// Safeguard 4 ("no model-training purpose inherited from telemetry unless
// separately registered and permitted") is enforced structurally by the
// existing purpose-binding check (Step 1b), not by a fabricated
// purpose-ID naming heuristic: TRAIN_MODEL against a purpose that IS
// registered and bound to the activity is permitted, exactly like any
// other operation would be. This test asserts that a TRAIN_MODEL request
// against an activity that has NOT registered the purpose is BLOCKED by
// that same real, pre-existing check — not by inventing a rule about the
// purpose_id's name/substrings.
func TestEvaluate_Safeguard4_ModelTraining_BlockedWhenPurposeNotBoundToActivity(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity() // registers testPurpose only
	purposes.purposes["purpose-unbound"] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: "purpose-unbound",
		ProposedOperation: domain.OperationTrainModel,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("Safeguard 4: expected BLOCK for model training on an unregistered purpose, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV009PurposeIncompatible {
		t.Fatalf("expected PRV-009: PURPOSE_INCOMPATIBLE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Safeguard6_Anonymize_BlockedWithoutApprovedControl(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// Proposed operation is ANONYMIZE without DeidentificationControlRef
	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationAnonymize,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("Safeguard 6: expected BLOCK for anonymize without approved control, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV010DataCategoryRestricted {
		t.Fatalf("expected PRV-010: DATA_CATEGORY_RESTRICTED, got %v", d.ReasonCodes)
	}
}

// A MINOR subject is a condition this service can detect from
// caller-declared context but cannot itself adjudicate (what field
// minimization or redaction applies is PDC's job, and no PDC exists in
// this codebase — see the domain package doc comment). The fail-closed
// answer per §26's own doctrine is REVIEW_REQUIRED, not a fabricated
// RESTRICT constraint invented by this service.
func TestEvaluate_ReviewRequired_MinorSubject(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "child-user-01", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext: &domain.SubjectContext{
			SubjectRef:   "child-user-01",
			SubjectClass: "MINOR",
			AgeBand:      "MINOR",
			Residency:    "GB",
		},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED for minor-subject processing, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV010DataCategoryRestricted {
		t.Fatalf("expected PRV-010: DATA_CATEGORY_RESTRICTED, got %v", d.ReasonCodes)
	}
}

// Same reasoning as the minor-subject case: sensitive/special-category
// data is detected, but the exact minimization treatment is PDC's job.
func TestEvaluate_ReviewRequired_SensitiveDataCategory(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-patient-01", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		DataContext: &domain.DataContext{
			DataCategories:   []string{"HEALTH"},
			SensitivityFlags: []string{"SPECIAL_CATEGORY", "HEALTH"},
			Classification:   "RESTRICTED",
		},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED for special category data, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV010DataCategoryRestricted {
		t.Fatalf("expected PRV-010: DATA_CATEGORY_RESTRICTED, got %v", d.ReasonCodes)
	}
}

// A real CONDITIONAL transfer authorization (from privacy-transfer-svc's
// own evaluation) is the one legitimate source of RESTRICT constraint
// content — verified by case 4 of TestEvaluate_TransferCheck_PRV05Integration
// below.

func TestEvaluate_ConsentCheck_Granted_CapturesNoticeVersion(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	consentStub := &stubConsentRegistry{
		status:           "GRANTED",
		consentReceiptID: "receipt-999",
		noticeVersionID:  "notice-ver-888",
	}
	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, consentStub, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultPermit {
		t.Fatalf("expected PERMIT, got %s %v", d.Result, d.ReasonCodes)
	}
	if d.ConsentReceiptID == nil || *d.ConsentReceiptID != "receipt-999" {
		t.Fatalf("expected consent_receipt_id receipt-999, got %v", d.ConsentReceiptID)
	}
	if d.NoticeVersionID == nil || *d.NoticeVersionID != "notice-ver-888" {
		t.Fatalf("expected notice_version_id notice-ver-888, got %v", d.NoticeVersionID)
	}
}

func TestEvaluate_ConsentCheck_NotGranted_Blocks(t *testing.T) {
	for _, tc := range []struct {
		status       string
		expectedCode string
	}{
		{"DENIED", domain.PRV006ConsentRequiredMissing},
		{"WITHDRAWN", domain.PRV007ConsentWithdrawn},
		{"NOT_REQUESTED", domain.PRV006ConsentRequiredMissing},
	} {
		t.Run(tc.status, func(t *testing.T) {
			purposes := newStubPurposeRegistry()
			purposes.activities[testActivity] = activeActivity()
			purposes.purposes[testPurpose] = publishedPurpose()

			r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{status: tc.status}, &stubHoldRegistry{}, nil)

			w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
				SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
				ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
			}, testTenant)
			var d domain.PrivacyDecision
			_ = json.Unmarshal(w.Body.Bytes(), &d)
			if d.Result != domain.ResultBlock {
				t.Fatalf("expected BLOCK for consent status %s, got %s", tc.status, d.Result)
			}
			if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != tc.expectedCode {
				t.Fatalf("expected reason %s, got %v", tc.expectedCode, d.ReasonCodes)
			}
		})
	}
}

func TestEvaluate_TransferCheck_PRV05Integration(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	// 1. Export without transfer check/mechanism -> BLOCKED (PRV-015)
	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)
	w1 := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
	}, testTenant)
	var d1 domain.PrivacyDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d1)
	if d1.Result != domain.ResultBlock || d1.ReasonCodes[0] != domain.PRV015TransferNotAuthorized {
		t.Fatalf("expected BLOCK/PRV-015 for unconfigured export, got %s %v", d1.Result, d1.ReasonCodes)
	}

	// 2. Transfer check authorized via PRV-05 -> PERMIT
	transferStub := &stubTransferRegistry{
		decision: &transferregistry.TransferDecision{
			DecisionID: "td-authorized-99",
			Result:     "AUTHORIZED",
		},
	}
	r2 := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferStub)
	w2 := doRequest(r2, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		TransferCheck: &domain.TransferCheckRequest{
			RelationshipID:          "rel-001",
			TransferMechanismID:     "mech-scc-001",
			DestinationJurisdiction: "US",
		},
	}, testTenant)
	var d2 domain.PrivacyDecision
	_ = json.Unmarshal(w2.Body.Bytes(), &d2)
	if d2.Result != domain.ResultPermit {
		t.Fatalf("expected PERMIT with authorized transfer, got %s %v", d2.Result, d2.ReasonCodes)
	}
	if d2.TransferDecisionID == nil || *d2.TransferDecisionID != "td-authorized-99" {
		t.Fatalf("expected transfer_decision_id td-authorized-99, got %v", d2.TransferDecisionID)
	}

	// 3. Transfer check blocked via PRV-05 -> BLOCK (PRV-015)
	transferBlockedStub := &stubTransferRegistry{
		decision: &transferregistry.TransferDecision{
			DecisionID: "td-blocked-99",
			Result:     "BLOCKED",
		},
	}
	r3 := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferBlockedStub)
	w3 := doRequest(r3, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		TransferCheck: &domain.TransferCheckRequest{
			RelationshipID:      "rel-001",
			TransferMechanismID: "mech-scc-001",
		},
	}, testTenant)
	var d3 domain.PrivacyDecision
	_ = json.Unmarshal(w3.Body.Bytes(), &d3)
	if d3.Result != domain.ResultBlock || d3.ReasonCodes[0] != domain.PRV015TransferNotAuthorized {
		t.Fatalf("expected BLOCK/PRV-015 when transfer blocked, got %s %v", d3.Result, d3.ReasonCodes)
	}

	// 4. Transfer check conditional via PRV-05 -> RESTRICT, carrying the
	// transfer service's own constraint content (not invented locally).
	transferConditionalStub := &stubTransferRegistry{
		decision: &transferregistry.TransferDecision{
			DecisionID: "td-conditional-99",
			Result:     "CONDITIONAL",
		},
	}
	r4 := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferConditionalStub)
	w4 := doRequest(r4, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		TransferCheck: &domain.TransferCheckRequest{
			RelationshipID:          "rel-001",
			TransferMechanismID:     "mech-scc-001",
			DestinationJurisdiction: "US",
		},
	}, testTenant)
	var d4 domain.PrivacyDecision
	_ = json.Unmarshal(w4.Body.Bytes(), &d4)
	if d4.Result != domain.ResultRestrict || d4.ReasonCodes[0] != domain.PRV011MinimizationRequired {
		t.Fatalf("expected RESTRICT/PRV-011 with conditional transfer, got %s %v", d4.Result, d4.ReasonCodes)
	}
	if len(d4.Constraints) != 1 || d4.Constraints[0].Type != "RECIPIENT_LIMITATION" {
		t.Fatalf("expected one RECIPIENT_LIMITATION constraint from transfer service, got %v", d4.Constraints)
	}
	if d4.TransferDecisionID == nil || *d4.TransferDecisionID != "td-conditional-99" {
		t.Fatalf("expected transfer_decision_id td-conditional-99, got %v", d4.TransferDecisionID)
	}
}

func TestEvaluate_LegalHoldCheck_Blocked(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{blocked: true, holdID: "hold-88"}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationDelete,
		LegalHoldCheck:    &domain.LegalHoldCheckRequest{RecordClass: "HR_EMPLOYEE_RECORD", EntityRef: "subject-1"},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock || d.ReasonCodes[0] != domain.PRV014RetentionOrHoldBlock {
		t.Fatalf("expected BLOCK/PRV-014: RETENTION_OR_HOLD_BLOCK, got %s %v", d.Result, d.ReasonCodes)
	}
	if d.LegalHoldID == nil || *d.LegalHoldID != "hold-88" {
		t.Fatalf("expected legal_hold_id hold-88 captured, got %v", d.LegalHoldID)
	}
}

func TestEvaluate_DependencyUnavailable_FailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.err = errors.New("connection refused")

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultIndeterminate {
		t.Fatalf("FAIL-OPEN: expected INDETERMINATE when the purpose registry is unreachable, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV019PrivacyContextIndeterminate {
		t.Fatalf("expected reason PRV-019: PRIVACY_CONTEXT_INDETERMINATE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_InvalidProposedOperation_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", map[string]string{
		"subject_ref": "subject-1", "processing_activity_id": testActivity, "purpose_id": testPurpose,
		"proposed_operation": "NOT_A_REAL_OPERATION",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid proposed_operation, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetDecision_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodGet, "/privacy/decisions/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
