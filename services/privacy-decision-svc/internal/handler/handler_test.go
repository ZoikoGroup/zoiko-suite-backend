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
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	decisions   map[string]*domain.PrivacyDecision
	idempotency map[string]*domain.IdempotencyRecord
	failCommit  bool
	commitCalls int
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
	d.ContextHash = d.InputFingerprint
	d.EvidenceRef = "prv-dec:" + d.DecisionID
	cp := *d
	s.decisions[tenantID+":"+d.DecisionID] = &cp
	return nil
}

func (s *stubStore) FindDecision(_ context.Context, tenantID, decisionID string) (*domain.PrivacyDecision, error) {
	d, ok := s.decisions[tenantID+":"+decisionID]
	if !ok {
		return nil, domain.ErrDecisionNotFound
	}
	return d, nil
}

func (s *stubStore) GetIdempotency(_ context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	rec, ok := s.idempotency[tenantID+":"+principalID+":"+key]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

// CommitDecision mirrors the real store's all-or-nothing contract: if
// failCommit is set nothing is persisted.
func (s *stubStore) CommitDecision(ctx context.Context, claim domain.IdempotencyRecord, d *domain.PrivacyDecision, render store.RenderFunc) (*domain.IdempotencyRecord, error) {
	s.commitCalls++
	if s.failCommit {
		return nil, domain.ErrStoreUnavailable
	}
	k := claim.TenantID + ":" + claim.PrincipalID + ":" + claim.Key
	if rec, ok := s.idempotency[k]; ok {
		return rec, nil
	}
	if err := s.RecordDecision(ctx, claim.TenantID, d); err != nil {
		return nil, err
	}
	code, body, err := render(d)
	if err != nil {
		delete(s.decisions, claim.TenantID+":"+d.DecisionID)
		return nil, err
	}
	rec := claim
	rec.ResponseCode, rec.ResponseBody = code, body
	s.idempotency[k] = &rec
	return nil, nil
}

// ── stub purpose registry ───────────────────────────────────────────────────

type stubPurposeRegistry struct {
	activities map[string]*purposeregistry.ActivityVersion
	purposes   map[string]*purposeregistry.PurposeVersion
	versions   map[string]*purposeregistry.ActivityVersion
	err        error
}

func newStubPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{
		activities: map[string]*purposeregistry.ActivityVersion{},
		purposes:   map[string]*purposeregistry.PurposeVersion{},
		versions:   map[string]*purposeregistry.ActivityVersion{},
	}
}

func (p *stubPurposeRegistry) ResolveActivity(_ context.Context, _, _, activityID string) (*purposeregistry.ActivityVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.activities[activityID], nil
}

func (p *stubPurposeRegistry) ResolveActivityVersion(_ context.Context, _, _, activityID, versionID string) (*purposeregistry.ActivityVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	if v, ok := p.versions[activityID+"@"+versionID]; ok {
		return v, nil
	}
	return nil, nil
}

func (p *stubPurposeRegistry) ResolvePurpose(_ context.Context, _, _, purposeID string) (*purposeregistry.PurposeVersion, error) {
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
	expiresAt        *time.Time
	err              error
}

func (c *stubConsentRegistry) ResolveStatus(_ context.Context, _, _, _, _ string) (*consentregistry.ConsentResolution, error) {
	if c.err != nil {
		return nil, c.err
	}
	res := &consentregistry.ConsentResolution{Status: c.status}
	if c.consentReceiptID != "" || c.noticeVersionID != "" {
		res.LatestReceipt = &consentregistry.Receipt{
			ConsentReceiptID: c.consentReceiptID,
			NoticeVersionID:  c.noticeVersionID,
			ExpiresAt:        c.expiresAt,
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

func (h *stubHoldRegistry) Resolve(_ context.Context, _, _, _, _ string) (*retentionregistry.RetentionResolution, error) {
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
		ActivityVersionID: "av-1", ActivityID: testActivity, PurposeIDs: []string{testPurpose}, VersionStatus: "ACTIVE", Jurisdictions: []string{"GB", "EU"},
		SubjectClasses: []string{"CUSTOMER", "EMPLOYEE"}, DataCategories: []string{"CONTACT_INFO", "IDENTIFIERS"},
		NoticeConsentDependency: "NOT_REQUIRED", DpiaTiaStatus: "NOT_REQUIRED",
	}
}

func publishedPurpose() *purposeregistry.PurposeVersion {
	return &purposeregistry.PurposeVersion{
		PurposeVersionID: "pv-1", PurposeID: testPurpose, VersionStatus: "PUBLISHED", LawfulBasisRefs: []string{"CONSENT", "LEGITIMATE_INTEREST"},
	}
}

func validSubjectContext() *domain.SubjectContext {
	return &domain.SubjectContext{
		SubjectRef:   "subject-1",
		SubjectClass: "CUSTOMER",
		Jurisdiction: "GB",
		Residency:    "GB",
	}
}

func newTestRouter(
	st *stubStore,
	purposes *stubPurposeRegistry,
	consents *stubConsentRegistry,
	holds *stubHoldRegistry,
	transfers *stubTransferRegistry,
) chi.Router {
	logger := zap.NewNop()
	if transfers == nil {
		transfers = &stubTransferRegistry{}
	}
	h := handler.New(st, purposes, consents, holds, transfers, logger)
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
	req.Header.Set("X-Correlation-ID", "corr-01")
	req.Header.Set("Idempotency-Key", "idem-"+uuid.New().String())
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
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
	r := newTestRouter(st, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// Canonical route POST /privacy/decisions
	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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
	if d.InputFingerprint == "" || d.ContextHash == "" {
		t.Fatal("expected input_fingerprint and context_hash to be populated")
	}
	if d.EvidenceRef == "" {
		t.Fatal("expected evidence_ref to be populated")
	}
	if len(d.PolicyRefs) == 0 {
		t.Fatal("expected policy_refs to be populated from purpose lawful basis")
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
	r := newTestRouter(st, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	reqPayload := domain.EvaluateDecisionRequest{
		SubjectRef: "subject-idem", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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
		SubjectContext:    validSubjectContext(),
	}
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", conflictingPayload, testTenant, headers)
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payload mismatch, got %d: %s", w3.Code, w3.Body.String())
	}
	var errResp map[string]string
	_ = json.Unmarshal(w3.Body.Bytes(), &errResp)
	if errResp["error_code"] != domain.ErrCodeImmutableEvidenceConflict {
		t.Fatalf("expected error_code PRV-020, got %s", errResp["error_code"])
	}
}

func TestEvaluate_Block_ActivityNotActive(t *testing.T) {
	purposes := newStubPurposeRegistry()
	inactive := activeActivity()
	inactive.VersionStatus = "SUSPENDED"
	purposes.activities[testActivity] = inactive
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

func TestEvaluate_PurposeBinding_UnboundPurposeBlockedForAnyOperation(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes["purpose-unbound"] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: "purpose-unbound",
		ProposedOperation: domain.OperationTrainModel,
		SubjectContext:    validSubjectContext(),
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("expected BLOCK for any operation on a purpose not registered for the activity, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.PRV009PurposeIncompatible {
		t.Fatalf("expected PRV-009: PURPOSE_INCOMPATIBLE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Safeguard6_Anonymize_BlockedWithoutApprovedControl(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationAnonymize,
		SubjectContext:    validSubjectContext(),
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

func TestEvaluate_ReviewRequired_MinorSubject(t *testing.T) {
	purposes := newStubPurposeRegistry()
	act := activeActivity()
	act.SubjectClasses = append(act.SubjectClasses, "MINOR")
	purposes.activities[testActivity] = act
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "child-user-01", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext: &domain.SubjectContext{
			SubjectRef:   "child-user-01",
			SubjectClass: "MINOR",
			AgeBand:      "MINOR",
			Residency:    "GB",
			Jurisdiction: "GB",
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

func TestEvaluate_ReviewRequired_SensitiveDataCategory(t *testing.T) {
	purposes := newStubPurposeRegistry()
	act := activeActivity()
	act.DataCategories = []string{"HEALTH"}
	purposes.activities[testActivity] = act
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-patient-01", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

func TestEvaluate_ConsentCheck_Granted_CapturesNoticeVersion(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	consentStub := &stubConsentRegistry{
		status:           "GRANTED",
		consentReceiptID: "receipt-999",
		noticeVersionID:  "notice-ver-888",
	}
	r := newTestRouter(newStubStore(), purposes, consentStub, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
		SubjectContext: validSubjectContext(),
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
		{"EXPIRED", domain.PRV006ConsentRequiredMissing},
		{"INVALIDATED", domain.PRV006ConsentRequiredMissing},
		{"NOT_REQUESTED", domain.PRV006ConsentRequiredMissing},
	} {
		t.Run(tc.status, func(t *testing.T) {
			purposes := newStubPurposeRegistry()
			purposes.activities[testActivity] = activeActivity()
			purposes.purposes[testPurpose] = publishedPurpose()

			r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{status: tc.status}, &stubHoldRegistry{}, nil)

			w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
				SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
				ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
				SubjectContext: validSubjectContext(),
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

func TestEvaluate_EmployeeConsent_ImbalanceReviewRequired(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{status: "GRANTED"}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "emp-101", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		ConsentCheck:      &domain.ConsentCheckRequest{Required: true},
		SubjectContext: &domain.SubjectContext{
			SubjectRef:   "emp-101",
			SubjectClass: "EMPLOYEE",
			Jurisdiction: "GB",
		},
	}, testTenant)

	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired {
		t.Fatalf("N10: expected REVIEW_REQUIRED for employee consent imbalance, got %s", d.Result)
	}
	if len(d.ReasonCodes) == 0 || d.ReasonCodes[0] != domain.PRV010DataCategoryRestricted {
		t.Fatalf("expected PRV-010, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Jurisdiction_UnresolvedFailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// Missing subject context -> INDETERMINATE with PRV-004
	w1 := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d1 domain.PrivacyDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d1)
	if d1.Result != domain.ResultIndeterminate || d1.ReasonCodes[0] != domain.PRV004JurisdictionUnresolved {
		t.Fatalf("N12: expected INDETERMINATE/PRV-004 for missing jurisdiction, got %s %v", d1.Result, d1.ReasonCodes)
	}

	// UNKNOWN jurisdiction -> INDETERMINATE with PRV-004
	w2 := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    &domain.SubjectContext{SubjectRef: "subject-1", Jurisdiction: "UNKNOWN"},
	}, testTenant)
	var d2 domain.PrivacyDecision
	_ = json.Unmarshal(w2.Body.Bytes(), &d2)
	if d2.Result != domain.ResultIndeterminate || d2.ReasonCodes[0] != domain.PRV004JurisdictionUnresolved {
		t.Fatalf("N12: expected INDETERMINATE/PRV-004 for UNKNOWN jurisdiction, got %s %v", d2.Result, d2.ReasonCodes)
	}

	// Mismatched jurisdiction against activity registered jurisdictions (GB, EU vs US)
	w3 := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    &domain.SubjectContext{SubjectRef: "subject-1", Jurisdiction: "US"},
	}, testTenant)
	var d3 domain.PrivacyDecision
	_ = json.Unmarshal(w3.Body.Bytes(), &d3)
	if d3.Result != domain.ResultBlock || d3.ReasonCodes[0] != domain.PRV004JurisdictionUnresolved {
		t.Fatalf("expected BLOCK/PRV-004 for mismatched jurisdiction, got %s %v", d3.Result, d3.ReasonCodes)
	}
}

func TestEvaluate_Policy_UnresolvedLawfulBasisFailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	// Purpose has empty LawfulBasisRefs
	emptyPurpose := publishedPurpose()
	emptyPurpose.LawfulBasisRefs = []string{}
	purposes.purposes[testPurpose] = emptyPurpose

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultIndeterminate || d.ReasonCodes[0] != domain.PRV005PolicyUnavailable {
		t.Fatalf("expected INDETERMINATE/PRV-005 when lawful basis is missing, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluate_TransferCheck_PRV05Integration(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	// 1. Export without transfer check/mechanism -> BLOCKED (PRV-015)
	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)
	w1 := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		SubjectContext:    validSubjectContext(),
	}, testTenant)
	var d1 domain.PrivacyDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d1)
	if d1.Result != domain.ResultBlock || d1.ReasonCodes[0] != domain.PRV015TransferNotAuthorized {
		t.Fatalf("expected BLOCK/PRV-015 for unconfigured export, got %s %v", d1.Result, d1.ReasonCodes)
	}

	// 1b. Foreign disclosure without transfer check -> BLOCKED (PRV-015)
	w1b := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationDisclose,
		SubjectContext:    validSubjectContext(), // GB
		RecipientContext: &domain.RecipientContext{
			DestinationJurisdiction: "US", // foreign jurisdiction!
		},
	}, testTenant)
	var d1b domain.PrivacyDecision
	_ = json.Unmarshal(w1b.Body.Bytes(), &d1b)
	if d1b.Result != domain.ResultBlock || d1b.ReasonCodes[0] != domain.PRV015TransferNotAuthorized {
		t.Fatalf("expected BLOCK/PRV-015 for foreign disclosure without transfer check, got %s %v", d1b.Result, d1b.ReasonCodes)
	}

	// 2. Transfer check authorized via PRV-05 -> PERMIT
	transferStub := &stubTransferRegistry{
		decision: &transferregistry.TransferDecision{
			DecisionID: "td-authorized-99",
			Result:     "AUTHORIZED",
		},
	}
	r2 := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferStub)
	w2 := doRequest(r2, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		SubjectContext:    validSubjectContext(),
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
	r3 := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferBlockedStub)
	w3 := doRequest(r3, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		SubjectContext:    validSubjectContext(),
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

	// 4. Transfer check conditional via PRV-05 -> RESTRICT
	transferConditionalStub := &stubTransferRegistry{
		decision: &transferregistry.TransferDecision{
			DecisionID: "td-conditional-99",
			Result:     "CONDITIONAL",
		},
	}
	r4 := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, transferConditionalStub)
	w4 := doRequest(r4, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationExport,
		SubjectContext:    validSubjectContext(),
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
}

func TestEvaluate_LegalHoldCheck_Blocked(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{blocked: true, holdID: "hold-88"}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationDelete,
		SubjectContext:    validSubjectContext(),
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

func TestEvaluate_DeletionWithoutHoldCheck_FailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// DELETE with no legal_hold_check: hold state unknown -> fail closed (I19), never PERMIT
	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationDelete,
		SubjectContext:    validSubjectContext(),
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultIndeterminate || d.ReasonCodes[0] != domain.PRV014RetentionOrHoldBlock {
		t.Fatalf("PRV-I19: expected INDETERMINATE/PRV-014 when deleting without legal hold information, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluate_DependencyUnavailable_FailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.err = errors.New("connection refused")

	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
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

func TestEvaluate_SecurityHeadersEnforced(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()
	r := newTestRouter(newStubStore(), purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	reqPayload := domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
	}

	// 1. Missing X-Principal-Id -> 401 AUTHENTICATION_FAILED
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, testTenant, map[string]string{"X-Principal-Id": ""})
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id, got %d", w1.Code)
	}
	var err1 map[string]string
	_ = json.Unmarshal(w1.Body.Bytes(), &err1)
	if err1["error_code"] != domain.ErrCodeAuthenticationFailed {
		t.Fatalf("expected error_code AUTHENTICATION_FAILED, got %s", err1["error_code"])
	}

	// 2. Missing X-Tenant-Id -> 400 TENANT_SCOPE_REQUIRED
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, "", nil)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without X-Tenant-Id, got %d", w2.Code)
	}
	var err2 map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &err2)
	if err2["error_code"] != domain.ErrCodeTenantScopeRequired {
		t.Fatalf("expected error_code TENANT_SCOPE_REQUIRED, got %s", err2["error_code"])
	}

	// 3. Tenant mismatch -> 403 TENANT_SCOPE_MISMATCH
	mismatchReq := reqPayload
	mismatchReq.TenantID = "other-tenant"
	w3 := doRequest(r, http.MethodPost, "/privacy/decisions", mismatchReq, testTenant)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on tenant mismatch, got %d", w3.Code)
	}
	var err3 map[string]string
	_ = json.Unmarshal(w3.Body.Bytes(), &err3)
	if err3["error_code"] != domain.ErrCodeTenantScopeMismatch {
		t.Fatalf("expected error_code TENANT_SCOPE_MISMATCH, got %s", err3["error_code"])
	}

	// 4. Missing X-Correlation-ID -> 400 CORRELATION_ID_REQUIRED
	w4 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, testTenant, map[string]string{"X-Correlation-ID": ""})
	if w4.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without X-Correlation-ID, got %d", w4.Code)
	}
	var err4 map[string]string
	_ = json.Unmarshal(w4.Body.Bytes(), &err4)
	if err4["error_code"] != domain.ErrCodeCorrelationRequired {
		t.Fatalf("expected error_code CORRELATION_ID_REQUIRED, got %s", err4["error_code"])
	}

	// 5. Missing Idempotency-Key -> 400 IDEMPOTENCY_KEY_REQUIRED
	w5 := doRequestWithHeaders(r, http.MethodPost, "/privacy/decisions", reqPayload, testTenant, map[string]string{"Idempotency-Key": ""})
	if w5.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without Idempotency-Key, got %d", w5.Code)
	}
	var err5 map[string]string
	_ = json.Unmarshal(w5.Body.Bytes(), &err5)
	if err5["error_code"] != domain.ErrCodeIdempotencyRequired {
		t.Fatalf("expected error_code IDEMPOTENCY_KEY_REQUIRED, got %s", err5["error_code"])
	}
}

func TestGetDecision_SecurityAndScoping(t *testing.T) {
	st := newStubStore()
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()
	r := newTestRouter(st, purposes, &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	// Create a decision for testTenant
	wCreate := doRequest(r, http.MethodPost, "/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
		SubjectContext:    validSubjectContext(),
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(wCreate.Body.Bytes(), &d)

	// 1. Unauthenticated GET -> 401
	wNoAuth := doRequestWithHeaders(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, testTenant, map[string]string{"X-Principal-Id": ""})
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Principal-Id on GET, got %d", wNoAuth.Code)
	}

	// 2. Missing Tenant on GET -> 400
	wNoTenant := doRequestWithHeaders(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, "", nil)
	if wNoTenant.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without X-Tenant-Id on GET, got %d", wNoTenant.Code)
	}

	// 3. Cross-Tenant GET with known ID -> 404 NOT_FOUND
	wCross := doRequest(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, "different-tenant")
	if wCross.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on cross-tenant GET, got %d", wCross.Code)
	}
	var errCross map[string]string
	_ = json.Unmarshal(wCross.Body.Bytes(), &errCross)
	if errCross["error_code"] != domain.ErrCodeNotFound {
		t.Fatalf("expected error_code NOT_FOUND, got %s", errCross["error_code"])
	}

	// 4. Same-Tenant GET -> 200 OK
	wSame := doRequest(r, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, testTenant)
	if wSame.Code != http.StatusOK {
		t.Fatalf("expected 200 on same-tenant GET, got %d", wSame.Code)
	}
}

func TestEvaluate_InvalidProposedOperation_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodPost, "/privacy/decisions", map[string]string{
		"subject_ref": "subject-1", "processing_activity_id": testActivity, "purpose_id": testPurpose,
		"proposed_operation": "NOT_A_REAL_OPERATION",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid proposed_operation, got %d: %s", w.Code, w.Body.String())
	}
	var errResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp["error_code"] != domain.ErrCodeValidationFailed {
		t.Fatalf("expected VALIDATION_FAILED, got %s", errResp["error_code"])
	}
}

func TestGetDecision_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{}, nil)

	w := doRequest(r, http.MethodGet, "/privacy/decisions/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var errResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp["error_code"] != domain.ErrCodeNotFound {
		t.Fatalf("expected error_code NOT_FOUND, got %s", errResp["error_code"])
	}
}
