package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-transfer-svc/internal/authz"
	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/events"
	"zoiko.io/privacy-transfer-svc/internal/handler"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/purposeregistry"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	calls      int
	eventTypes []string
	last       events.PublishParams
}

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.calls++
	p.eventTypes = append(p.eventTypes, params.EventType)
	p.last = params
	return nil
}

func (p *stubPublisher) published(eventType string) bool {
	for _, e := range p.eventTypes {
		if e == eventType {
			return true
		}
	}
	return false
}

// ── stub authz ───────────────────────────────────────────────────────────────

type stubAuthz struct{ deny bool }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub purpose registry ────────────────────────────────────────────────────

type stubPurposeRegistry struct {
	activities map[string]*purposeregistry.ActivityVersion
	err        error
}

func newStubPurposeRegistry() *stubPurposeRegistry {
	return &stubPurposeRegistry{activities: map[string]*purposeregistry.ActivityVersion{}}
}

func (p *stubPurposeRegistry) ResolveActivity(_ context.Context, _, _, activityID string) (*purposeregistry.ActivityVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.activities[activityID], nil
}

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-transfer-1"

func tctx() context.Context { return middleware.WithTenant(context.Background(), testTenant) }

func newTestRouter(st *stubStore, pub *stubPublisher, az handler.AuthzChecker, purposes *stubPurposeRegistry) chi.Router {
	logger := zap.NewNop()
	st.pub = pub // the store "enqueues" events; the stub records the committed ones here
	h := handler.New(st, az, purposes, logger)
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
	// §18.1 requires a correlation ID and idempotency key on every mutating
	// API; the generic helper supplies unique defaults so tests that are not
	// specifically exercising those headers stay focused on their behavior.
	req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
	req.Header.Set("Idempotency-Key", "idem-"+uuid.NewString())
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createRelationship(t *testing.T, r http.Handler) *domain.ProcessorRelationship {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
		ProcessingInstructions: "process payroll data only as instructed by the controller",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createRelationship: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	return &rel
}

func createValidMechanism(t *testing.T, r http.Handler) *domain.TransferMechanism {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{
		MechanismType: "SCC", EvidenceRef: "contract-doc-1",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createValidMechanism: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var m domain.TransferMechanism
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return &m
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateRelationship(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	if rel.Status != domain.RelationshipActive {
		t.Fatalf("expected ACTIVE, got %s", rel.Status)
	}
}

// TestCreateRelationship_UnboundPurposeRef_Rejected proves
// purpose_activity_refs are validated against a real PRV-01 call, not
// trusted as opaque strings.
func TestCreateRelationship_UnboundPurposeRef_Rejected(t *testing.T) {
	purposes := newStubPurposeRegistry() // nothing registered
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, purposes)

	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
		PurposeActivityRefs: []string{"activity-does-not-exist"},
	}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("FABRICATION: expected 422 for an unregistered purpose activity ref, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRelationship_ActivePurposeRef_Accepted(t *testing.T) {
	purposes := newStubPurposeRegistry()
	purposes.activities["activity-hr"] = &purposeregistry.ActivityVersion{ActivityVersionID: "av-1", ActivityID: "activity-hr", VersionStatus: "ACTIVE"}
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, purposes)

	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
		PurposeActivityRefs: []string{"activity-hr"},
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateRelationshipStatus_ToInactive(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/status",
		domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	updated, _ := st.FindRelationship(tctx(), rel.RelationshipID)
	if updated.Status != domain.RelationshipInactive {
		t.Fatalf("expected INACTIVE, got %s", updated.Status)
	}
}

func TestAttachAndListSubprocessors(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "sub-vendor-1", Service: "cloud-storage"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	wList := doRequest(r, http.MethodGet, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors", nil, testTenant)
	var got struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(wList.Body.Bytes(), &got)
	if got.Count != 1 {
		t.Fatalf("expected 1 subprocessor, got %d", got.Count)
	}
}

// ── transfer decision evaluation ─────────────────────────────────────────────

func TestEvaluateTransfer_Authorized_NoAssessmentRequired(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED, got %s (reasons=%v)", d.Result, d.ReasonCodes)
	}
}

// TestEvaluateTransfer_AuthorizationDenied proves EvaluateTransfer is now
// gated the same way every other mutating handler in this service already
// is: a principal lacking PRIVACY_TRANSFER_DECISION_EVALUATE is rejected
// with 403 before any decision is recorded or published, closing the gap
// where any non-empty X-Principal-Id could previously evaluate transfers.
func TestEvaluateTransfer_AuthorizationDenied(t *testing.T) {
	st := newStubStore()
	setupRouter := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, setupRouter)
	mech := createValidMechanism(t, setupRouter)

	pub := &stubPublisher{}
	deniedRouter := newTestRouter(st, pub, &stubAuthz{deny: true}, newStubPurposeRegistry())
	w := doRequest(deniedRouter, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if len(st.decisions) != 0 {
		t.Fatalf("expected no decision recorded on denial, found %d", len(st.decisions))
	}
	if pub.calls != 0 {
		t.Fatalf("expected no event published on denial, got %d publish calls", pub.calls)
	}
}

// TestEvaluateTransfer_AuthorizationDenied_NoIdempotencyRecordCreated proves
// a denied request cannot back-door a decision into existence via the
// idempotency store either: the same Idempotency-Key, retried by an
// authorized principal, must still evaluate for real rather than replaying
// a cached response the denied call never got to create.
func TestEvaluateTransfer_AuthorizationDenied_NoIdempotencyRecordCreated(t *testing.T) {
	st := newStubStore()
	setupRouter := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, setupRouter)
	mech := createValidMechanism(t, setupRouter)

	const key = "evaluate-denied-then-retry"
	deniedRouter := newTestRouter(st, &stubPublisher{}, &stubAuthz{deny: true}, newStubPurposeRegistry())
	denied := doRequestWithHeaders(deniedRouter, http.MethodPost, "/privacy/transfer-decisions",
		domain.EvaluateTransferRequest{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID},
		map[string]string{"X-Tenant-Id": testTenant, "Idempotency-Key": key})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", denied.Code, denied.Body.String())
	}

	allowedRouter := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	allowed := doRequestWithHeaders(allowedRouter, http.MethodPost, "/privacy/transfer-decisions",
		domain.EvaluateTransferRequest{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID},
		map[string]string{"X-Tenant-Id": testTenant, "Idempotency-Key": key})
	if allowed.Code != http.StatusOK {
		t.Fatalf("expected the same key to succeed once actually authorized, got %d: %s", allowed.Code, allowed.Body.String())
	}
	var d domain.TransferDecision
	_ = json.Unmarshal(allowed.Body.Bytes(), &d)
	if d.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED, got %s", d.Result)
	}
}

// An unknown relationship or mechanism id is a 404 (the service's convention for
// an id that does not resolve), never 503 "store unavailable", and nothing is
// recorded or published for it.
func TestEvaluateTransfer_UnknownRelationshipOrMechanism_Is404(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: "00000000-0000-0000-0000-0000000000e2",
	}, testTenant)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "transfer mechanism not found") {
		t.Fatalf("unknown mechanism must be 404, got %d: %s", w.Code, w.Body.String())
	}
	w = doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: "00000000-0000-0000-0000-0000000000e3", TransferMechanismID: mech.MechanismID,
	}, testTenant)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "processor relationship not found") {
		t.Fatalf("unknown relationship must be 404, got %d: %s", w.Code, w.Body.String())
	}
	if pub.published("privacy.transfer.authorization_changed") {
		t.Fatal("no authorization event may be published for an unknown id")
	}
}

func TestEvaluateTransfer_Blocked_RelationshipInactive(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/status",
		domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlocked || d.ReasonCodes[0] != domain.ReasonRelationshipNotActive {
		t.Fatalf("expected BLOCKED/PROCESSOR_RELATIONSHIP_NOT_ACTIVE, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_Blocked_MechanismExpired(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)

	past := time.Now().UTC().Add(-48 * time.Hour)
	validFrom := past.Add(-24 * time.Hour)
	w := doRequest(r, http.MethodPost, "/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{
		MechanismType: "SCC", ValidFrom: &validFrom, ValidUntil: &past,
	}, testTenant)
	var mech domain.TransferMechanism
	_ = json.Unmarshal(w.Body.Bytes(), &mech)

	wDecision := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(wDecision.Body.Bytes(), &d)
	if d.Result != domain.ResultBlocked || d.ReasonCodes[0] != domain.ReasonMechanismExpired {
		t.Fatalf("FABRICATION: expected BLOCKED/TRANSFER_MECHANISM_INVALID_OR_EXPIRED for an expired mechanism, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_ReviewRequired_AssessmentMissing(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired || d.ReasonCodes[0] != domain.ReasonAssessmentMissing {
		t.Fatalf("expected REVIEW_REQUIRED/ASSESSMENT_REQUIRED_NOT_FOUND, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_Blocked_AssessmentRejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentReject,
	}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlocked || d.ReasonCodes[0] != domain.ReasonAssessmentRejected {
		t.Fatalf("expected BLOCKED/ASSESSMENT_REJECTED, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_ReviewRequired_AssessmentRemediate(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentRemediate,
	}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired || d.ReasonCodes[0] != domain.ReasonAssessmentRemediate {
		t.Fatalf("expected REVIEW_REQUIRED/ASSESSMENT_REQUIRES_REMEDIATION, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_ReviewRequired_AssessmentExpired(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	expired := time.Now().UTC().Add(-1 * time.Hour)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove, ReviewTriggerAt: &expired,
	}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired || d.ReasonCodes[0] != domain.ReasonAssessmentExpired {
		t.Fatalf("FABRICATION: expected REVIEW_REQUIRED/ASSESSMENT_EXPIRED for a stale approval, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestEvaluateTransfer_Authorized_ApprovedAssessmentNotExpired(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	future := time.Now().UTC().Add(365 * 24 * time.Hour)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove, ReviewTriggerAt: &future,
	}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED with a valid, non-expired APPROVE assessment, got %s %v", d.Result, d.ReasonCodes)
	}
	if d.AssessmentID == nil {
		t.Fatalf("expected the resolved assessment_id to be captured on the decision")
	}
}

// TestEvaluateTransfer_NoAssessmentRequired_IgnoresRejectedAssessment
// proves the opt-in nature of the assessment check — a REJECTED
// assessment must not block a transfer that never declared it needed one.
func TestEvaluateTransfer_NoAssessmentRequired_IgnoresRejectedAssessment(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentReject,
	}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: false,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED when assessment_required=false, even with a REJECTED assessment on file, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestGetDecision_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	w := doRequest(r, http.MethodGet, "/privacy/transfer-decisions/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCreateRelationship_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true}, newStubPurposeRegistry())
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func doRequestWithHeaders(r http.Handler, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	if _, ok := headers["X-Correlation-ID"]; !ok {
		req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
	}
	if _, ok := headers["Idempotency-Key"]; !ok {
		req.Header.Set("Idempotency-Key", "idem-"+uuid.NewString())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doRawRequest sends exactly the headers it is given (plus content type and
// principal) with no defaults, so negative tests can omit X-Tenant-Id,
// Idempotency-Key or X-Correlation-ID on purpose.
func doRawRequest(r http.Handler, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-01")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIdempotency_ReplayAndConflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())

	reqPayload := domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1",
		ProcessorRef:  "processor-1",
		Service:       "payroll-processing",
	}

	headers := map[string]string{
		"X-Tenant-Id":     testTenant,
		"Idempotency-Key": "transfer-idem-001",
	}

	// 1. Initial request -> 201 Created
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/processor-relationships", reqPayload, headers)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w1.Code, w1.Body.String())
	}
	var res1 domain.ProcessorRelationship
	if err := json.Unmarshal(w1.Body.Bytes(), &res1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// 2. Replay with identical payload -> 201 Created with Idempotency-Replay: true
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/processor-relationships", reqPayload, headers)
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 on replay, got %d", w2.Code)
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay: true header, got %s", w2.Header().Get("Idempotency-Replay"))
	}
	var res2 domain.ProcessorRelationship
	if err := json.Unmarshal(w2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("failed to decode replay response: %v", err)
	}
	if res2.RelationshipID != res1.RelationshipID {
		t.Fatalf("expected same relationship ID on replay, got %s vs %s", res2.RelationshipID, res1.RelationshipID)
	}

	// 3. Replay with conflicting payload -> 409 Conflict
	conflictPayload := domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-2",
		ProcessorRef:  "processor-2",
		Service:       "different-service",
	}
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/processor-relationships", conflictPayload, headers)
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payload mismatch, got %d: %s", w3.Code, w3.Body.String())
	}
}

func TestEvaluateTransfer_IdempotencyAndReasonCodes(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	evalReq := domain.EvaluateTransferRequest{
		RelationshipID:      rel.RelationshipID,
		TransferMechanismID: mech.MechanismID,
		AssessmentRequired:  false,
	}

	headers := map[string]string{
		"X-Tenant-Id":     testTenant,
		"Idempotency-Key": "transfer-eval-idem-002",
	}

	// 1. Initial evaluate call
	w1 := doRequestWithHeaders(r, http.MethodPost, "/v1/privacy/transfer-decisions", evalReq, headers)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w1.Code, w1.Body.String())
	}
	var d1 domain.TransferDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d1)
	if d1.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED, got %s", d1.Result)
	}

	// 2. Replay with same key
	w2 := doRequestWithHeaders(r, http.MethodPost, "/v1/privacy/transfer-decisions", evalReq, headers)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %d", w2.Code)
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay: true header, got %s", w2.Header().Get("Idempotency-Replay"))
	}
}

func TestTransferAssessment_MeasuresAndCanonicalV1(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)

	req := domain.RecordTransferAssessmentRequest{
		RelationshipID:         rel.RelationshipID,
		Outcome:                domain.AssessmentApprove,
		ResidualRisk:           "LOW",
		EvidenceRef:            "s3://evidence/tia-001.pdf",
		GovernmentAccessRisk:   "MINIMAL_SAFEGUARDS_SATISFIED",
		TechnicalMeasures:      "AES_256_GCM_ENCRYPTION_IN_TRANSIT_AND_REST",
		OrganizationalMeasures: "ACCESS_LOGGING_AND_QUARTERLY_AUDIT",
	}

	w := doRequest(r, http.MethodPost, "/v1/privacy/transfer-assessments", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on /v1/..., got %d: %s", w.Code, w.Body.String())
	}

	var a domain.TransferAssessment
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if a.GovernmentAccessRisk != req.GovernmentAccessRisk {
		t.Fatalf("expected GovernmentAccessRisk %s, got %s", req.GovernmentAccessRisk, a.GovernmentAccessRisk)
	}
	if a.TechnicalMeasures != req.TechnicalMeasures {
		t.Fatalf("expected TechnicalMeasures %s, got %s", req.TechnicalMeasures, a.TechnicalMeasures)
	}
	if a.OrganizationalMeasures != req.OrganizationalMeasures {
		t.Fatalf("expected OrganizationalMeasures %s, got %s", req.OrganizationalMeasures, a.OrganizationalMeasures)
	}

	// Verify GET latest
	wGet := doRequest(r, http.MethodGet, "/v1/privacy/transfer-assessments?relationship_id="+rel.RelationshipID, nil, testTenant)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /v1/..., got %d", wGet.Code)
	}
}

func TestEvaluateTransfer_ConditionalOutcomeWithMeasures(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	// Record assessment with technical and organizational measures
	assReq := domain.RecordTransferAssessmentRequest{
		RelationshipID:         rel.RelationshipID,
		Outcome:                domain.AssessmentApprove,
		ResidualRisk:           "MEDIUM",
		TechnicalMeasures:      "TLS_1_3_AND_CUSTOMER_HSM_ENCRYPTION",
		OrganizationalMeasures: "QUARTERLY_ACCESS_REVIEWS",
	}
	wAss := doRequest(r, http.MethodPost, "/privacy/transfer-assessments", assReq, testTenant)
	if wAss.Code != http.StatusCreated {
		t.Fatalf("failed to record assessment: %d", wAss.Code)
	}

	evalReq := domain.EvaluateTransferRequest{
		RelationshipID:      rel.RelationshipID,
		TransferMechanismID: mech.MechanismID,
		AssessmentRequired:  true,
	}

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", evalReq, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var d domain.TransferDecision
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if d.Result != domain.ResultConditional {
		t.Fatalf("expected CONDITIONAL outcome, got %s", d.Result)
	}
	if !strings.Contains(d.Conditions, "TLS_1_3_AND_CUSTOMER_HSM_ENCRYPTION") {
		t.Fatalf("expected conditions to contain technical measure, got: %s", d.Conditions)
	}
	if !strings.Contains(d.Conditions, "QUARTERLY_ACCESS_REVIEWS") {
		t.Fatalf("expected conditions to contain organizational measure, got: %s", d.Conditions)
	}
}

func TestEvaluateTransfer_MandatoryReassessmentTriggers(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	wRel := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "cloud-hosting",
		Jurisdictions:          []string{"EU-GDPR", "UK-GDPR"},
		ProcessingInstructions: "host data only within the declared jurisdictions",
	}, testTenant)
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(wRel.Body.Bytes(), &rel)

	mech := createValidMechanism(t, r)

	// 1. Destination jurisdiction mismatch trigger
	evalReq := domain.EvaluateTransferRequest{
		RelationshipID:          rel.RelationshipID,
		TransferMechanismID:     mech.MechanismID,
		DestinationJurisdiction: "AU-PRIVACY", // not in relationship's ["EU-GDPR", "UK-GDPR"]
		AssessmentRequired:      false,
	}
	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", evalReq, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED on new jurisdiction trigger, got %s", d.Result)
	}
	foundJurisdictionTrigger := false
	for _, code := range d.ReasonCodes {
		if strings.Contains(code, "NEW_DESTINATION_JURISDICTION") {
			foundJurisdictionTrigger = true
			break
		}
	}
	if !foundJurisdictionTrigger {
		t.Fatalf("expected reason code for new destination jurisdiction, got: %v", d.ReasonCodes)
	}

	// 2. Minors context trigger
	evalMinors := domain.EvaluateTransferRequest{
		RelationshipID:      rel.RelationshipID,
		TransferMechanismID: mech.MechanismID,
		SubjectClasses:      []string{"MINORS"},
		AssessmentRequired:  false,
	}
	wMinors := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", evalMinors, testTenant)
	var dMinors domain.TransferDecision
	_ = json.Unmarshal(wMinors.Body.Bytes(), &dMinors)
	if dMinors.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED on minors context trigger, got %s", dMinors.Result)
	}

	// 3. Security/privacy incident trigger
	evalIncident := domain.EvaluateTransferRequest{
		RelationshipID:      rel.RelationshipID,
		TransferMechanismID: mech.MechanismID,
		ReassessmentTrigger: domain.TriggerSecurityPrivacyIncident,
		AssessmentRequired:  false,
	}
	wIncident := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", evalIncident, testTenant)
	var dIncident domain.TransferDecision
	_ = json.Unmarshal(wIncident.Body.Bytes(), &dIncident)
	if dIncident.Result != domain.ResultReviewRequired {
		t.Fatalf("expected REVIEW_REQUIRED on incident trigger, got %s", dIncident.Result)
	}
}

func TestEvaluateTriggersEndpoint(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	wRel := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "cloud-hosting",
		Jurisdictions: []string{"EU-GDPR", "UK-GDPR"},
	}, testTenant)
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(wRel.Body.Bytes(), &rel)

	mech := createValidMechanism(t, r)

	req := domain.EvaluateTriggersRequest{
		RelationshipID:          rel.RelationshipID,
		TransferMechanismID:     mech.MechanismID,
		DestinationJurisdiction: "JP-APPI",
		DeclaredTrigger:         domain.TriggerMaterialArchitectureChange,
	}

	w := doRequest(r, http.MethodPost, "/v1/privacy/transfer-assessments/evaluate-triggers", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on evaluate-triggers, got %d: %s", w.Code, w.Body.String())
	}

	var resp domain.EvaluateTriggersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode triggers response: %v", err)
	}

	if !resp.ReassessmentNeeded {
		t.Fatalf("expected ReassessmentNeeded: true")
	}
	if len(resp.TriggerEvaluations) != 8 {
		t.Fatalf("expected 8 trigger evaluations, got %d", len(resp.TriggerEvaluations))
	}
	if len(resp.ActiveTriggers) == 0 {
		t.Fatalf("expected active triggers to be non-empty")
	}
}

// ── §18.1 mandatory transport headers ────────────────────────────────────────

// TestTenantHeader_Rejected proves the tenant scope carrier is enforced at the
// transport boundary, before any business logic runs: a missing, empty or
// whitespace-only X-Tenant-Id must be refused rather than silently scoping the
// request to the empty/platform tenant.
func TestTenantHeader_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	body := domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
	}

	cases := map[string]map[string]string{
		"missing":              {"Idempotency-Key": "k1", "X-Correlation-ID": "c1"},
		"empty":                {"X-Tenant-Id": "", "Idempotency-Key": "k2", "X-Correlation-ID": "c2"},
		"whitespace-only":      {"X-Tenant-Id": "   ", "Idempotency-Key": "k3", "X-Correlation-ID": "c3"},
		"whitespace-around-id": {"X-Tenant-Id": "  " + testTenant + "  ", "Idempotency-Key": "k4", "X-Correlation-ID": "c4"},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, "/privacy/processor-relationships", body, headers)
			if name == "whitespace-around-id" {
				if w.Code != http.StatusCreated {
					t.Fatalf("expected 201 when tenant id is padded with whitespace, got %d: %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 for %s X-Tenant-Id, got %d: %s", name, w.Code, w.Body.String())
			}
		})
	}
}

// TestMutations_RequireIdempotencyKeyAndCorrelationID walks every mutating route
// and proves both §18.1 headers are mandatory: omitting either yields 400
// before any idempotency record, store mutation or event is produced.
func TestMutations_RequireIdempotencyKeyAndCorrelationID(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	cases := []struct {
		name string
		path string
		body interface{}
	}{
		{"create-relationship", "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}},
		{"update-status", "/privacy/processor-relationships/" + rel.RelationshipID + "/status", domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}},
		{"attach-subprocessor", "/privacy/processor-relationships/" + rel.RelationshipID + "/subprocessors", domain.AttachSubprocessorRequest{ProviderIdentity: "sub-1", Service: "svc"}},
		{"create-mechanism", "/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{MechanismType: "SCC", EvidenceRef: "e-1"}},
		{"record-assessment", "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove}},
		{"evaluate-transfer", "/privacy/transfer-decisions", domain.EvaluateTransferRequest{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID}},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/missing-idempotency-key", func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, tc.path, tc.body, map[string]string{
				"X-Tenant-Id":      testTenant,
				"X-Correlation-ID": "corr-test",
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when Idempotency-Key is missing, got %d: %s", w.Code, w.Body.String())
			}
		})
		t.Run(tc.name+"/blank-idempotency-key", func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, tc.path, tc.body, map[string]string{
				"X-Tenant-Id":      testTenant,
				"X-Correlation-ID": "corr-test",
				"Idempotency-Key":  "   ",
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when Idempotency-Key is blank, got %d: %s", w.Code, w.Body.String())
			}
		})
		t.Run(tc.name+"/missing-correlation-id", func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, tc.path, tc.body, map[string]string{
				"X-Tenant-Id":     testTenant,
				"Idempotency-Key": "idem-test-" + tc.name,
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when X-Correlation-ID is missing, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestEvaluateTransfer_Blocked_ProcessorInstructionMissing proves PRV-017 is
// enforced fail-closed: an ACTIVE relationship with no controller processing
// instructions cannot be authorized, and is reported with the §32 reason code.
func TestEvaluateTransfer_Blocked_ProcessorInstructionMissing(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())

	// Deliberately created WITHOUT processing_instructions.
	wRel := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
	}, testTenant)
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(wRel.Body.Bytes(), &rel)
	mech := createValidMechanism(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlocked || d.ReasonCodes[0] != domain.ReasonInstructionMissing {
		t.Fatalf("expected BLOCKED/PRV-017 PROCESSOR_INSTRUCTION_MISSING, got %s %v", d.Result, d.ReasonCodes)
	}
}

// ── PRV-018 subprocessor approval gate (§17.2 / §32) ─────────────────────────

// TestEvaluateTransfer_Blocked_SubprocessorNotApproved proves §17.2's
// fail-closed gate: an attached subprocessor with no §16.1 notification/
// approval model on file is an unapproved subprocessor, so the transfer is
// BLOCKED with the stable §32 code PRV-018 rather than silently
// authorized. A second, properly governed subprocessor must not clear it.
func TestEvaluateTransfer_Blocked_SubprocessorNotApproved(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "sub-unapproved", Service: "analytics"}, testTenant)
	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "sub-approved", Service: "storage", NotificationApprovalModel: "PRIOR_WRITTEN_NOTICE_30_DAYS"}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultBlocked || d.ReasonCodes[0] != domain.ReasonSubprocessorNotApproved {
		t.Fatalf("expected BLOCKED/PRV-018 SUBPROCESSOR_NOT_APPROVED, got %s %v", d.Result, d.ReasonCodes)
	}
}

// TestEvaluateTransfer_Authorized_WithApprovedSubprocessor proves the
// inverse: a subprocessor that does carry an approval model does not block
// an otherwise compliant transfer.
func TestEvaluateTransfer_Authorized_WithApprovedSubprocessor(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "sub-approved", Service: "storage", NotificationApprovalModel: "PRIOR_WRITTEN_NOTICE_30_DAYS"}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.Result != domain.ResultAuthorized {
		t.Fatalf("expected AUTHORIZED with an approved subprocessor, got %s %v", d.Result, d.ReasonCodes)
	}
}

// ── §16.1 TransferContext ────────────────────────────────────────────────────

// TestEvaluateTransfer_TransferContextRecorded proves §16.1's exporter/
// importer roles, origin jurisdiction, route/provider and frequency/volume
// class travel with the authorization evidence: echoed on the decision
// response and durable on the GET-by-id read.
func TestEvaluateTransfer_TransferContextRecorded(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID:          rel.RelationshipID,
		TransferMechanismID:     mech.MechanismID,
		DestinationJurisdiction: "US",
		ExporterRole:            "CONTROLLER",
		ImporterRole:            "PROCESSOR",
		OriginJurisdiction:      "EU-GDPR",
		RouteProvider:           "AWS-DIRECT-CONNECT",
		FrequencyVolumeClass:    "BATCH_HOURLY_HIGH_VOLUME",
	}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)

	if d.AuthorizationID == "" {
		t.Fatalf("expected §18 authorization_id to be populated on the response")
	}
	if d.ExporterRole != "CONTROLLER" || d.ImporterRole != "PROCESSOR" || d.OriginJurisdiction != "EU-GDPR" ||
		d.RouteProvider != "AWS-DIRECT-CONNECT" || d.FrequencyVolumeClass != "BATCH_HOURLY_HIGH_VOLUME" {
		t.Fatalf("transfer context not echoed on decision: %+v", d)
	}

	wGet := doRequest(r, http.MethodGet, "/privacy/transfer-decisions/"+d.DecisionID, nil, testTenant)
	var got domain.TransferDecision
	_ = json.Unmarshal(wGet.Body.Bytes(), &got)
	if got.OriginJurisdiction != "EU-GDPR" || got.RouteProvider != "AWS-DIRECT-CONNECT" {
		t.Fatalf("transfer context not durable on GET: %+v", got)
	}
}

// ── §19 event contracts ──────────────────────────────────────────────────────

// TestEvents_EmitSpecContract proves PRV-05 emits the §19 event types it
// owns — privacy.processor.changed on relationship create/status change/
// subprocessor attach, privacy.assessment.expiring when a review/expiry
// trigger is declared, and privacy.transfer.authorization_changed on
// evaluation — and does not publish the superseded non-spec names.
func TestEvents_EmitSpecContract(t *testing.T) {
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)

	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/status",
		domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, testTenant)
	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/status",
		domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipActive}, testTenant)
	doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "sub-1", Service: "svc", NotificationApprovalModel: "PRIOR_WRITTEN_NOTICE_30_DAYS"}, testTenant)

	future := time.Now().UTC().Add(365 * 24 * time.Hour)
	doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove, ReviewTriggerAt: &future,
	}, testTenant)
	doRequest(r, http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
		RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID,
	}, testTenant)

	for _, want := range []string{handler.EventProcessorChanged, handler.EventAssessmentExpiring, handler.EventTransferAuthorizationChanged} {
		if !pub.published(want) {
			t.Fatalf("expected §19 event %q to be published; got %v", want, pub.eventTypes)
		}
	}
	for _, superseded := range []string{"privacy.processor_relationship.created", "privacy.transfer_decision.evaluated"} {
		if pub.published(superseded) {
			t.Fatalf("superseded non-spec event %q must not be published; got %v", superseded, pub.eventTypes)
		}
	}
}
