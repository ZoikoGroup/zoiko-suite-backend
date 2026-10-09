package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-rights-svc/internal/authz"
	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/events"
	"zoiko.io/privacy-rights-svc/internal/handler"
	"zoiko.io/privacy-rights-svc/internal/middleware"
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

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-rights-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, logger)
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

func createRequest(t *testing.T, r http.Handler) *domain.RightsRequest {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-1", RightFamily: domain.RightAccess, Jurisdiction: "EU",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createRequest: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var req domain.RightsRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)
	return &req
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateRequest_Received(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	if req.Status != domain.StatusReceived {
		t.Fatalf("expected RECEIVED, got %s", req.Status)
	}
	if req.IdentityVerified {
		t.Fatalf("expected identity_verified false at intake")
	}
}

func TestCreateRequest_InvalidRightFamily_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", map[string]string{
		"subject_ref": "subject-1", "right_family": "NOT_REAL",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestIdentityVerification_FailedAttempt_DoesNotAdvanceStatus(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: false, Method: "GOVT_ID_MATCH", Note: "name mismatch"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	updated, err := st.FindRequest(context.Background(), req.RequestID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Status != domain.StatusReceived {
		t.Fatalf("FABRICATION: a FAILED identity check must not advance status, got %s", updated.Status)
	}
	if updated.IdentityVerified {
		t.Fatalf("FABRICATION: identity_verified must remain false after a failed attempt")
	}
}

func TestIdentityVerification_SuccessfulAttempt_Advances(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)

	updated, _ := st.FindRequest(context.Background(), req.RequestID)
	if updated.Status != domain.StatusIdentityVerified || !updated.IdentityVerified {
		t.Fatalf("expected IDENTITY_VERIFIED and identity_verified=true, got status=%s verified=%v", updated.Status, updated.IdentityVerified)
	}
}

func TestAttachDiscoveryManifest_AdvancesToInDiscovery(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "accounts-receivable-svc", ContentHash: "sha256:abc", CandidateCount: 3}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	updated, _ := st.FindRequest(context.Background(), req.RequestID)
	if updated.Status != domain.StatusInDiscovery {
		t.Fatalf("expected IN_DISCOVERY, got %s", updated.Status)
	}
}

// TestClose_Fulfilled_WithoutIdentityVerification_Blocked is the
// regression test for §15.2's DISCLOSURE GATE. A discovery manifest IS
// attached here — the point is to isolate the identity-verification half
// of the gate specifically, so this doesn't just get caught by the
// other (no-manifest) precondition instead.
func TestClose_Fulfilled_WithoutIdentityVerification_Blocked(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	// Deliberately no identity-verification call.

	// A manifest attempt before identity verification stays in RECEIVED
	// (the store's own status machine only advances IDENTITY_VERIFIED ->
	// IN_DISCOVERY), but the manifest ROW itself is still recorded either
	// way — enough to isolate the identity check specifically.
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "accounts-receivable-svc", ContentHash: "sha256:abc", CandidateCount: 3}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("DISCLOSURE GATE VIOLATION: expected 422 closing FULFILLED with no identity verification, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	// Check for §32 error code PRV-012: IDENTITY_ASSURANCE_INSUFFICIENT
	if got["error"] == "" || !bytes.Contains([]byte(got["error"]), []byte("PRV-012")) {
		t.Fatalf("expected the error to contain PRV-012 (IDENTITY_ASSURANCE_INSUFFICIENT), got %q", got["error"])
	}
}

// TestClose_Fulfilled_WithoutDiscoveryManifest_Blocked is the other half
// of the same gate: identity alone is not enough either.
func TestClose_Fulfilled_WithoutDiscoveryManifest_Blocked(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("DISCLOSURE GATE VIOLATION: expected 422 closing FULFILLED with no discovery manifest, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClose_Fulfilled_WithBothPreconditions_Succeeds(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "accounts-receivable-svc", ContentHash: "sha256:abc", CandidateCount: 3}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled, ResponseEvidenceHash: "sha256:response"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var closed domain.RightsRequest
	_ = json.Unmarshal(w.Body.Bytes(), &closed)
	if closed.Status != domain.StatusClosed || closed.Outcome == nil || *closed.Outcome != domain.OutcomeFulfilled {
		t.Fatalf("expected CLOSED/FULFILLED, got status=%s outcome=%v", closed.Status, closed.Outcome)
	}
}

// TestClose_Rejected_NoPreconditionsRequired proves REJECTED/WITHDRAWN
// carry no DISCLOSURE GATE precondition — a request can be rejected
// precisely because identity could never be verified.
func TestClose_Rejected_NoPreconditionsRequired(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeRejected, Reason: "identity could not be verified"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 closing REJECTED with no preconditions, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClose_AlreadyClosed_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on double-close, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAttachWFCProcessRef(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/wfc-process-ref",
		domain.AttachWFCProcessRefRequest{WFCProcessRef: "wf-instance-123"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	updated, _ := st.FindRequest(context.Background(), req.RequestID)
	if updated.WFCProcessRef == nil || *updated.WFCProcessRef != "wf-instance-123" {
		t.Fatalf("expected wfc_process_ref recorded, got %v", updated.WFCProcessRef)
	}
}

// TestAttachWFCProcessRef_OnClosedRequest_Conflict and its two siblings
// below prove the three "process" mutations (WFC ref, identity
// verification, discovery manifest) consistently reject a CLOSED request
// with 409/PRV-020, the same immutability-conflict code CloseRequest
// itself already uses for a double-close — rather than falling through to
// a generic 503/PRV-019 as if the backing store were merely unavailable.
func TestAttachWFCProcessRef_OnClosedRequest_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/wfc-process-ref",
		domain.AttachWFCProcessRefRequest{WFCProcessRef: "wf-instance-123"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 attaching WFC ref to a CLOSED request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordIdentityVerification_OnClosedRequest_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 recording identity verification on a CLOSED request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAttachDiscoveryManifest_OnClosedRequest_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "accounts-receivable-svc", ContentHash: "sha256:abc", CandidateCount: 3}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 attaching a discovery manifest to a CLOSED request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListRequestsBySubject(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	createRequest(t, r)
	createRequest(t, r)

	w := doRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=subject-1", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 2 {
		t.Fatalf("expected 2 requests for subject-1, got %d", got.Count)
	}
}

func TestCreateRequest_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true})
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-1", RightFamily: domain.RightAccess,
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestGetRequest_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodGet, "/privacy/rights-requests/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCreateRequest_IdempotencyKey_ReplayAndConflict(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})

	payload := domain.CreateRightsRequestRequest{
		SubjectRef:   "subject-idem-1",
		RightFamily:  domain.RightErasure,
		Jurisdiction: "GDPR",
	}

	// 1. Initial request with Idempotency-Key
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{
		"Idempotency-Key": "key-sar-12345",
	})
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on first request, got %d: %s", w1.Code, w1.Body.String())
	}
	var res1 domain.RightsRequest
	if err := json.Unmarshal(w1.Body.Bytes(), &res1); err != nil {
		t.Fatalf("failed to unmarshal first response: %v", err)
	}

	// 2. Idempotent replay with same key and identical payload -> 201 with Idempotency-Replay: true
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{
		"Idempotency-Key": "key-sar-12345",
	})
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 on replay, got %d: %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("expected Idempotency-Replay: true header, got %s", w2.Header().Get("Idempotency-Replay"))
	}
	var res2 domain.RightsRequest
	if err := json.Unmarshal(w2.Body.Bytes(), &res2); err != nil {
		t.Fatalf("failed to unmarshal replay response: %v", err)
	}
	if res1.RequestID != res2.RequestID {
		t.Fatalf("expected identical request_id %s, got %s", res1.RequestID, res2.RequestID)
	}

	// 3. Conflict: same key with different payload -> 409 Conflict
	payloadDiff := domain.CreateRightsRequestRequest{
		SubjectRef:   "subject-DIFFERENT",
		RightFamily:  domain.RightAccess,
		Jurisdiction: "CCPA",
	}
	w3 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payloadDiff, testTenant, map[string]string{
		"Idempotency-Key": "key-sar-12345",
	})
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on payload mismatch for same key, got %d: %s", w3.Code, w3.Body.String())
	}
}

func TestClose_ResponsePackageVersion_IncrementedOnFulfilled(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	// Verify identity
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)
	// Attach discovery manifest
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "crm-svc", ContentHash: "sha256:crm123", CandidateCount: 5}, testTenant)

	// Close as FULFILLED
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled, ResponseEvidenceHash: "sha256:pkg-v1"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var closed domain.RightsRequest
	_ = json.Unmarshal(w.Body.Bytes(), &closed)
	if closed.ResponsePackageVersion != 1 {
		t.Fatalf("expected ResponsePackageVersion=1 (I21), got %d", closed.ResponsePackageVersion)
	}
}

func TestV1Routes_Succeed(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodPost, "/v1/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-v1", RightFamily: domain.RightPortability, Jurisdiction: "EU",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 on /v1/ route, got %d: %s", w.Code, w.Body.String())
	}
	var req domain.RightsRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)

	// Fetch via /v1/
	w2 := doRequest(r, http.MethodGet, "/v1/privacy/rights-requests/"+req.RequestID, nil, testTenant)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 fetching via /v1/, got %d: %s", w2.Code, w2.Body.String())
	}
}
