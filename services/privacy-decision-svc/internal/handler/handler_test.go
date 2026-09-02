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
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	decisions map[string]*domain.PrivacyDecision
}

func newStubStore() *stubStore {
	return &stubStore{decisions: map[string]*domain.PrivacyDecision{}}
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

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub purpose registry (real state, not canned responses) ────────────────

type stubPurposeRegistry struct {
	activities map[string]*purposeregistry.ActivityVersion
	purposes   map[string]*purposeregistry.PurposeVersion
	err        error
}

func newStubPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{activities: map[string]*purposeregistry.ActivityVersion{}, purposes: map[string]*purposeregistry.PurposeVersion{}}
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
	status string
	err    error
}

func (c *stubConsentRegistry) ResolveStatus(_ context.Context, _, _ string) (*consentregistry.ConsentResolution, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &consentregistry.ConsentResolution{Status: c.status}, nil
}

// ── stub hold registry ───────────────────────────────────────────────────────

type stubHoldRegistry struct {
	blocked bool
	err     error
}

func (h *stubHoldRegistry) Resolve(_ context.Context, _, _, _ string) (*retentionregistry.RetentionResolution, error) {
	if h.err != nil {
		return nil, h.err
	}
	return &retentionregistry.RetentionResolution{Blocked: h.blocked}, nil
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

func newTestRouter(st *stubStore, pub *stubPublisher, purposes *stubPurposeRegistry, consents *stubConsentRegistry, holds *stubHoldRegistry) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, purposes, consents, holds, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
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
	r := newTestRouter(st, pub, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
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
	if pub.calls != 1 {
		t.Errorf("expected privacy.decision.evaluated published once, got %d", pub.calls)
	}

	// Retrieve it back.
	wGet := doRequest(r, http.MethodGet, "/v1/privacy/decisions/"+d.DecisionID, nil, testTenant)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 on get, got %d: %s", wGet.Code, wGet.Body.String())
	}
}

func TestEvaluate_Block_ActivityNotActive(t *testing.T) {
	purposes := newStubPurposeRegistry()
	inactive := activeActivity()
	inactive.VersionStatus = "SUSPENDED"
	purposes.activities[testActivity] = inactive
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("expected BLOCK, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.ReasonActivityNotActive {
		t.Fatalf("expected reason ACTIVITY_NOT_ACTIVE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Block_ActivityNotFound(t *testing.T) {
	purposes := newStubPurposeRegistry() // nothing registered
	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: "unknown-activity", PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock || d.ReasonCodes[0] != domain.ReasonActivityNotActive {
		t.Fatalf("expected BLOCK/ACTIVITY_NOT_ACTIVE for an unregistered activity, got %s %v", d.Result, d.ReasonCodes)
	}
}

// TestEvaluate_Block_PurposeNotBoundToActivity is the regression test for
// PRV-C01 (purpose limitation): a purpose that is itself published but
// was never registered against THIS activity must still be blocked.
func TestEvaluate_Block_PurposeNotBoundToActivity(t *testing.T) {
	purposes := newStubPurposeRegistry()
	activity := activeActivity()
	activity.PurposeIDs = []string{"some-other-purpose"} // does NOT include testPurpose
	purposes.activities[testActivity] = activity
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock {
		t.Fatalf("FABRICATION: expected BLOCK for a purpose not bound to the activity, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.ReasonPurposeNotBoundToActivity {
		t.Fatalf("expected reason PURPOSE_NOT_BOUND_TO_ACTIVITY, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_Block_PurposeNotPublished(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	// purposes.purposes left empty — purpose does not resolve.

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock || d.ReasonCodes[0] != domain.ReasonPurposeNotPublished {
		t.Fatalf("expected BLOCK/PURPOSE_NOT_PUBLISHED, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluate_ConsentCheck_Granted_Permits(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{status: "GRANTED"}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultPermit {
		t.Fatalf("expected PERMIT when consent is GRANTED, got %s %v", d.Result, d.ReasonCodes)
	}
}

// TestEvaluate_ConsentCheck_NotGranted_Blocks is the regression test for
// PRV-C04: an opt-in consent check that resolves to anything but GRANTED
// must block, regardless of activity/purpose being otherwise fine.
func TestEvaluate_ConsentCheck_NotGranted_Blocks(t *testing.T) {
	for _, status := range []string{"DENIED", "WITHDRAWN", "NOT_REQUESTED"} {
		t.Run(status, func(t *testing.T) {
			purposes := newStubPurposeRegistry()
			purposes.activities[testActivity] = activeActivity()
			purposes.purposes[testPurpose] = publishedPurpose()

			r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{status: status}, &stubHoldRegistry{})

			w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
				SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
				ProposedOperation: domain.OperationUse, ConsentCheck: &domain.ConsentCheckRequest{Required: true},
			}, testTenant)
			var d domain.PrivacyDecision
			_ = json.Unmarshal(w.Body.Bytes(), &d)
			if d.Result != domain.ResultBlock {
				t.Fatalf("FABRICATION: expected BLOCK for consent status %s, got %s", status, d.Result)
			}
			if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.ReasonConsentNotGranted {
				t.Fatalf("expected reason CONSENT_NOT_GRANTED, got %v", d.ReasonCodes)
			}
		})
	}
}

// TestEvaluate_NoConsentCheckRequested_NotEvaluated proves consent
// checking is genuinely opt-in: an undeclared consent requirement must
// not be silently enforced (or silently ignored differently) — it's
// simply not part of the evaluation at all.
func TestEvaluate_NoConsentCheckRequested_NotEvaluated(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	// Consent registry would deny if asked — but it must never be asked.
	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{status: "DENIED"}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultPermit {
		t.Fatalf("expected PERMIT when no consent_check was requested (even though consent would deny), got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluate_LegalHoldCheck_Blocked(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities[testActivity] = activeActivity()
	purposes.purposes[testPurpose] = publishedPurpose()

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{blocked: true})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationDelete,
		LegalHoldCheck:    &domain.LegalHoldCheckRequest{RecordClass: "HR_EMPLOYEE_RECORD", EntityRef: "subject-1"},
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlock || d.ReasonCodes[0] != domain.ReasonLegalHoldBlocksUse {
		t.Fatalf("expected BLOCK/LEGAL_HOLD_BLOCKS_USE, got %s %v", d.Result, d.ReasonCodes)
	}
}

// TestEvaluate_DependencyUnavailable_FailsClosed proves §12.2's doctrine:
// "Material processing fails closed" — any unreachable dependency
// produces INDETERMINATE, never a silent PERMIT.
func TestEvaluate_DependencyUnavailable_FailsClosed(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.err = errors.New("connection refused")

	r := newTestRouter(newStubStore(), &stubPublisher{}, purposes, &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse,
	}, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultIndeterminate {
		t.Fatalf("FAIL-OPEN: expected INDETERMINATE when the purpose registry is unreachable, got %s", d.Result)
	}
	if len(d.ReasonCodes) != 1 || d.ReasonCodes[0] != domain.ReasonDependencyUnavailable {
		t.Fatalf("expected reason DEPENDENCY_UNAVAILABLE, got %v", d.ReasonCodes)
	}
}

func TestEvaluate_InvalidProposedOperation_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodPost, "/v1/privacy/decisions", map[string]string{
		"subject_ref": "subject-1", "processing_activity_id": testActivity, "purpose_id": testPurpose,
		"proposed_operation": "NOT_A_REAL_OPERATION",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid proposed_operation, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetDecision_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, newStubPurposeRegistry(), &stubConsentRegistry{}, &stubHoldRegistry{})

	w := doRequest(r, http.MethodGet, "/v1/privacy/decisions/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
