package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	// §18.1 requires a correlation ID and idempotency key on every mutating
	// API; the generic helper supplies unique defaults so tests that are not
	// specifically exercising those headers stay focused on their behavior.
	if method != http.MethodGet {
		if _, ok := headers["Idempotency-Key"]; !ok {
			req.Header.Set("Idempotency-Key", "idem-"+uuid.NewString())
		}
		if _, ok := headers["X-Correlation-ID"]; !ok {
			req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
		}
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

// mutationCases enumerates every mutating route so the mandatory-header and
// tenant-scope tests exercise the whole §18.1 surface.
func mutationCases(reqID string) []struct {
	name string
	path string
	body interface{}
} {
	return []struct {
		name string
		path string
		body interface{}
	}{
		{"create", "/privacy/rights-requests", domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}},
		{"identity-verification", "/privacy/rights-requests/" + reqID + "/identity-verification", domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}},
		{"discovery-manifest", "/privacy/rights-requests/" + reqID + "/discovery-manifests", domain.AttachDiscoveryManifestRequest{Domain: "d", ContentHash: "sha256:x"}},
		{"wfc-process-ref", "/privacy/rights-requests/" + reqID + "/wfc-process-ref", domain.AttachWFCProcessRefRequest{WFCProcessRef: "wf-1"}},
		{"close", "/privacy/rights-requests/" + reqID + "/close", domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}},
	}
}

// TestMutations_RequireCorrelationIDAndIdempotencyKey walks every mutating
// route and proves both §18.1 headers are mandatory: omitting or blanking
// either yields 400 before any idempotency record, store mutation or event.
func TestMutations_RequireCorrelationIDAndIdempotencyKey(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	for _, tc := range mutationCases(req.RequestID) {
		t.Run(tc.name+"/missing-correlation-id", func(t *testing.T) {
			w := doRawRequest(r, http.MethodPost, tc.path, tc.body, map[string]string{
				"X-Tenant-Id":     testTenant,
				"Idempotency-Key": "idem-" + uuid.NewString(),
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 when X-Correlation-ID is missing, got %d: %s", w.Code, w.Body.String())
			}
		})
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
	}
}

// TestMutations_RequireTenantScope proves a missing, empty or whitespace-only
// X-Tenant-Id is refused fail-closed (401) before any handler runs, while a
// whitespace-padded id is accepted (trimmed) — mirroring the RLS boundary.
func TestMutations_RequireTenantScope(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	for _, tc := range mutationCases(req.RequestID) {
		cases := map[string]map[string]string{
			"missing":         {"Idempotency-Key": "k1", "X-Correlation-ID": "c1"},
			"empty":           {"X-Tenant-Id": "", "Idempotency-Key": "k2", "X-Correlation-ID": "c2"},
			"whitespace-only": {"X-Tenant-Id": "   ", "Idempotency-Key": "k3", "X-Correlation-ID": "c3"},
		}
		for name, headers := range cases {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				w := doRawRequest(r, http.MethodPost, tc.path, tc.body, headers)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("expected 401 for %s X-Tenant-Id, got %d: %s", name, w.Code, w.Body.String())
				}
			})
		}
	}

	// A whitespace-padded tenant id is trimmed and accepted.
	w := doRawRequest(r, http.MethodPost, "/privacy/rights-requests",
		domain.CreateRightsRequestRequest{SubjectRef: "s-padded", RightFamily: domain.RightAccess},
		map[string]string{"X-Tenant-Id": "  " + testTenant + "  ", "Idempotency-Key": "k4", "X-Correlation-ID": "c4"})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when tenant id is padded with whitespace, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateRequest_AuthorizationRunsBeforeIdempotencyReplay is the GAP-4
// regression: an identical replay of a previously-authorized Idempotency-Key
// must NOT bypass authorization. The first call succeeds; once the principal
// loses authorization, the same key+payload must yield 403, never a replayed
// 201.
func TestCreateRequest_AuthorizationRunsBeforeIdempotencyReplay(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{}
	r := newTestRouter(st, &stubPublisher{}, az)

	payload := domain.CreateRightsRequestRequest{
		SubjectRef: "subject-authz-first", RightFamily: domain.RightErasure, Jurisdiction: "GDPR",
	}

	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{
		"Idempotency-Key": "key-authz-first",
	})
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on authorized first request, got %d: %s", w1.Code, w1.Body.String())
	}

	az.deny = true
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{
		"Idempotency-Key": "key-authz-first",
	})
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on unauthorized replay (authorization must precede idempotency), got %d: %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replay") != "" {
		t.Fatalf("an unauthorized replay must not be served as an idempotent replay")
	}
	if len(st.requests) != 1 {
		t.Fatalf("expected exactly 1 request created across both calls, got %d", len(st.requests))
	}
}

// TestCreateResponse_IncludesCanonicalClassificationAndIdentityState proves
// the §18 create-response critical fields are present additively (the
// pre-existing right_family/identity_verified fields are unchanged).
func TestCreateResponse_IncludesCanonicalClassificationAndIdentityState(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", domain.CreateRightsRequestRequest{
		SubjectRef: "subject-classify", RightFamily: domain.RightAccess, Jurisdiction: "EU",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		RightFamily      string `json:"right_family"`
		IdentityVerified bool   `json:"identity_verified"`
		IdentityState    string `json:"identity_state"`
		Classification   *struct {
			RightFamily  string `json:"right_family"`
			Jurisdiction string `json:"jurisdiction"`
		} `json:"classification"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if res.Classification == nil {
		t.Fatalf("expected §18 classification field on create response")
	}
	if res.Classification.RightFamily != res.RightFamily || res.Classification.Jurisdiction != "EU" {
		t.Fatalf("classification mismatch: %+v vs right_family=%s", res.Classification, res.RightFamily)
	}
	if res.IdentityState != "UNVERIFIED" {
		t.Fatalf("expected identity_state UNVERIFIED at intake, got %q", res.IdentityState)
	}
}

func TestIdentityVerificationResponse_IdentityStateBecomesVerified(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID_MATCH"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Request struct {
			IdentityState string `json:"identity_state"`
		} `json:"request"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Request.IdentityState != "VERIFIED" {
		t.Fatalf("expected identity_state VERIFIED after successful verification, got %q", res.Request.IdentityState)
	}
}

// TestDiscoveryManifestResponse_IncludesCanonicalHash proves the §18
// discovery-manifest "hash" field is exposed additively on both the attach
// response and the list response (content_hash is unchanged).
func TestDiscoveryManifestResponse_IncludesCanonicalHash(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req := createRequest(t, r)

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests",
		domain.AttachDiscoveryManifestRequest{Domain: "accounts-receivable-svc", ContentHash: "sha256:abc", CandidateCount: 3}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Manifest struct {
			ContentHash string `json:"content_hash"`
			Hash        string `json:"hash"`
		} `json:"manifest"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Manifest.Hash == "" || res.Manifest.Hash != res.Manifest.ContentHash {
		t.Fatalf("expected §18 hash == content_hash, got hash=%q content_hash=%q", res.Manifest.Hash, res.Manifest.ContentHash)
	}

	lw := doRequest(r, http.MethodGet, "/privacy/rights-requests/"+req.RequestID+"/discovery-manifests", nil, testTenant)
	var list struct {
		Data []struct {
			Hash string `json:"hash"`
		} `json:"data"`
	}
	_ = json.Unmarshal(lw.Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].Hash != "sha256:abc" {
		t.Fatalf("expected listed manifest hash sha256:abc, got %+v", list.Data)
	}
}

// TestNotFound_ReturnsPlainMessageWithoutMisusedCode proves request-not-found
// no longer borrows PRV-001 (PURPOSE_NOT_REGISTERED); §32 declares no
// not-found code, so a plain structured error is returned consistently.
func TestNotFound_ReturnsPlainMessageWithoutMisusedCode(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})

	w := doRequest(r, http.MethodGet, "/privacy/rights-requests/missing-1", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "PRV-") {
		t.Fatalf("not-found must not carry a misused §32 code, got %s", body)
	}

	mw := doRequest(r, http.MethodPost, "/privacy/rights-requests/missing-2/wfc-process-ref",
		domain.AttachWFCProcessRefRequest{WFCProcessRef: "wf-1"}, testTenant)
	if mw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on missing request, got %d: %s", mw.Code, mw.Body.String())
	}
	if body := mw.Body.String(); strings.Contains(body, "PRV-") {
		t.Fatalf("not-found must not carry a misused §32 code, got %s", body)
	}
}

// TestValidation_ReturnsPlainMessageWithoutMisusedCode proves input
// validation no longer borrows PRV-005 (POLICY_UNAVAILABLE); §32 declares no
// input-validation code, so a plain structured 400 is returned.
func TestValidation_ReturnsPlainMessageWithoutMisusedCode(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})

	w := doRequest(r, http.MethodPost, "/privacy/rights-requests", map[string]string{
		"subject_ref": "s", "right_family": "NOT_REAL",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "PRV-") {
		t.Fatalf("input validation must not carry a misused §32 code, got %s", body)
	}
}

// TestNoUndeclaredEventsPublished is the GAP-8 regression: §19 declares no
// rights-intake or rights-closure event, so the service must publish nothing
// for those transitions rather than emit undeclared event names.
func TestNoUndeclaredEventsPublished(t *testing.T) {
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{})
	req := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+req.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, testTenant)
	if pub.calls != 0 {
		t.Fatalf("no §19-declared event exists for rights intake/closure; expected 0 publishes, got %d", pub.calls)
	}
}
