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
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-transfer-svc/internal/authz"
	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/events"
	"zoiko.io/privacy-transfer-svc/internal/handler"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/purposeregistry"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

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

func (p *stubPurposeRegistry) ResolveActivity(_ context.Context, activityID string) (*purposeregistry.ActivityVersion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.activities[activityID], nil
}

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-transfer-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, purposes *stubPurposeRegistry) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, purposes, logger)
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

func createRelationship(t *testing.T, r http.Handler) *domain.ProcessorRelationship {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "payroll-processing",
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
	updated, _ := st.FindRelationship(context.Background(), rel.RelationshipID)
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
		Jurisdictions: []string{"EU-GDPR", "UK-GDPR"},
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


