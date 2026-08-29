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
	if got["error"] == "" || !bytes.Contains([]byte(got["error"]), []byte("identity")) {
		t.Fatalf("expected the error to name identity verification specifically, got %q", got["error"])
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
