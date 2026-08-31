package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-run-svc/internal/authz"
	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/events"
	"zoiko.io/payment-run-svc/internal/handler"
	"zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/paymentauthorization"
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

// ── stub payment-authorization-svc client ───────────────────────────────────

type stubAuth struct {
	auths        map[string]paymentauthorization.Authorization
	validity     map[string]bool
	consumeFails map[string]bool
	consumeCalls map[string]int
}

func newStubAuth() *stubAuth {
	return &stubAuth{
		auths: map[string]paymentauthorization.Authorization{}, validity: map[string]bool{},
		consumeFails: map[string]bool{}, consumeCalls: map[string]int{},
	}
}

func (a *stubAuth) add(id, tenantID, legalEntityID string, netAmount float64) {
	tid := tenantID
	a.auths[id] = paymentauthorization.Authorization{
		AuthorizationID: id, TenantID: &tid, LegalEntityID: legalEntityID, NetAmount: netAmount,
		Currency: "USD", Status: "APPROVED", PayeeRef: "payee-" + id,
	}
	a.validity[id] = true
}

func (a *stubAuth) GetApprovedAuthorization(_ context.Context, tenantID, legalEntityID, authorizationID string) (*paymentauthorization.Authorization, error) {
	auth, ok := a.auths[authorizationID]
	if !ok || (auth.TenantID != nil && *auth.TenantID != tenantID) || auth.LegalEntityID != legalEntityID || auth.Status != "APPROVED" {
		return nil, domain.ErrAuthorizationNotEligible
	}
	return &auth, nil
}

func (a *stubAuth) ValidateAuthorization(_ context.Context, _, authorizationID string) (bool, error) {
	return a.validity[authorizationID], nil
}

func (a *stubAuth) ConsumeAuthorization(_ context.Context, _, _, authorizationID string) error {
	a.consumeCalls[authorizationID]++
	if a.consumeFails[authorizationID] {
		return domain.ErrAuthorizationConsumeFailed
	}
	return nil
}

var _ paymentauthorization.Client = (*stubAuth)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap11-1"
const testLegalEntity = "le-ap11-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, auth *stubAuth) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, auth, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequestAs(r http.Handler, method, path string, body interface{}, tenantID, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, tenantID, "principal-operator")
}

func createRun(t *testing.T, r http.Handler, authIDs []string) *domain.PaymentRun {
	t.Helper()
	req := domain.CreateRunRequest{
		LegalEntityID: testLegalEntity, PayingBankAccountRef: "bank-acct-1", Currency: "USD",
		ValueDate: time.Now().UTC().Add(24 * time.Hour), PaymentMethod: "ACH", AuthorizationIDs: authIDs,
	}
	w := doRequest(r, http.MethodPost, "/ap11/runs/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createRun: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Run domain.PaymentRun `json:"run"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return &resp.Run
}

func validateAndLock(t *testing.T, r http.Handler, runID string) *httptest.ResponseRecorder {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap11/runs/"+runID+"/validate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	return doRequest(r, http.MethodPost, "/ap11/runs/"+runID+"/lock", nil, testTenant)
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateRun_Draft(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-1", testTenant, testLegalEntity, 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)

	run := createRun(t, r, []string{"auth-1"})
	if run.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", run.Status)
	}
}

// TestCreateRun_CrossTenantAuthorization_Rejected is negative-path #4.
func TestCreateRun_CrossTenantAuthorization_Rejected(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-2", "some-other-tenant", testLegalEntity, 200)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)

	w := doRequest(r, http.MethodPost, "/ap11/runs/", domain.CreateRunRequest{
		LegalEntityID: testLegalEntity, PayingBankAccountRef: "acct", Currency: "USD",
		ValueDate: time.Now().UTC(), PaymentMethod: "ACH", AuthorizationIDs: []string{"auth-2"},
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 cross-tenant rejected, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRun_AuthorizationAlreadyInAnotherRun_Rejected(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-3", testTenant, testLegalEntity, 300)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	createRun(t, r, []string{"auth-3"})

	w := doRequest(r, http.MethodPost, "/ap11/runs/", domain.CreateRunRequest{
		LegalEntityID: testLegalEntity, PayingBankAccountRef: "acct", Currency: "USD",
		ValueDate: time.Now().UTC(), PaymentMethod: "ACH", AuthorizationIDs: []string{"auth-3"},
	}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 already in another run, got %d: %s", w.Code, w.Body.String())
	}
}

func TestValidatePaymentRun_NoLongerValid_Blocked(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-4", testTenant, testLegalEntity, 100)
	auth.validity["auth-4"] = false
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-4"})

	w := doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/validate", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 no longer valid, got %d: %s", w.Code, w.Body.String())
	}
}

// TestLockPaymentRun_ConsumesAuthorizations is the first real caller of
// AP-10's ConsumeAuthorization anywhere in this codebase.
func TestLockPaymentRun_ConsumesAuthorizations(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-5", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-5"})

	w := validateAndLock(t, r, run.RunID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 locking, got %d: %s", w.Code, w.Body.String())
	}
	if auth.consumeCalls["auth-5"] != 1 {
		t.Fatalf("expected exactly one real Consume call, got %d", auth.consumeCalls["auth-5"])
	}
	var locked domain.PaymentRun
	_ = json.Unmarshal(w.Body.Bytes(), &locked)
	if locked.Status != domain.StatusLocked {
		t.Fatalf("expected LOCKED, got %s", locked.Status)
	}
}

func TestLockPaymentRun_ConsumeFails_RunMovesToException(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-6", testTenant, testLegalEntity, 100)
	auth.consumeFails["auth-6"] = true
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-6"})
	doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/validate", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/lock", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 consume failed, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID, nil, testTenant)
	var resp struct {
		Run domain.PaymentRun `json:"run"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Run.Status != domain.StatusException {
		t.Fatalf("expected EXCEPTION after failed consume, got %s", resp.Run.Status)
	}
}

// TestSubmitPaymentRun_ReplaySameKey_Idempotent is negative-path #1.
func TestSubmitPaymentRun_ReplaySameKey_Idempotent(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-7", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-7"})
	validateAndLock(t, r, run.RunID)

	w := doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-key-1"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 first submit, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-key-1"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent replay, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitPaymentRun_ReplayDifferentKey_Rejected(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-8", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-8"})
	validateAndLock(t, r, run.RunID)
	doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-key-a"}, testTenant)

	w := doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-key-b"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 different key rejected, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReconcilePaymentRunStatus_UpdatesInstructionAndRun(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-9", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-9"})
	validateAndLock(t, r, run.RunID)
	doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-9"}, testTenant)

	w := doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID+"/instructions", nil, testTenant)
	var listResp struct {
		Data []domain.RunInstruction `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	instructionID := listResp.Data[0].InstructionID

	w = doRequest(r, http.MethodPost, "/ap11/instructions/"+instructionID+"/reconcile",
		domain.ReconcileInstructionRequest{ExternalStatus: domain.InstructionSettled, ProviderEventRef: "provider-evt-1"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 reconciling, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID, nil, testTenant)
	var runResp struct {
		Run domain.PaymentRun `json:"run"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &runResp)
	if runResp.Run.Status != domain.StatusSettled {
		t.Fatalf("expected run SETTLED after all instructions settled, got %s", runResp.Run.Status)
	}
}

// TestReconcilePaymentRunStatus_ReplayedEvent_Idempotent is negative-path #2.
func TestReconcilePaymentRunStatus_ReplayedEvent_Idempotent(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-10", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-10"})
	validateAndLock(t, r, run.RunID)
	doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-10"}, testTenant)

	w := doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID+"/instructions", nil, testTenant)
	var listResp struct {
		Data []domain.RunInstruction `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	instructionID := listResp.Data[0].InstructionID

	reconcileReq := domain.ReconcileInstructionRequest{ExternalStatus: domain.InstructionAccepted, ProviderEventRef: "provider-evt-dup"}
	doRequest(r, http.MethodPost, "/ap11/instructions/"+instructionID+"/reconcile", reconcileReq, testTenant)

	w = doRequest(r, http.MethodPost, "/ap11/instructions/"+instructionID+"/reconcile", reconcileReq, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent replay, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Applied {
		t.Fatalf("expected applied=false for a replayed provider event")
	}
}

func TestCancelUnsubmittedRun(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-11", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-11"})

	w := doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/cancel", domain.CancelRunRequest{Reason: "duplicate run"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRetrySafeInstruction(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-12", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-12"})
	validateAndLock(t, r, run.RunID)
	doRequest(r, http.MethodPost, "/ap11/runs/"+run.RunID+"/submit", domain.SubmitRunRequest{IdempotencyKey: "idem-12"}, testTenant)

	w := doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID+"/instructions", nil, testTenant)
	var listResp struct {
		Data []domain.RunInstruction `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	instructionID := listResp.Data[0].InstructionID

	w = doRequest(r, http.MethodPost, "/ap11/instructions/"+instructionID+"/retry", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 retrying, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAvailableActions_Draft(t *testing.T) {
	auth := newStubAuth()
	auth.add("auth-13", testTenant, testLegalEntity, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, auth)
	run := createRun(t, r, []string{"auth-13"})

	w := doRequest(r, http.MethodGet, "/ap11/runs/"+run.RunID+"/available-actions", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		AvailableActions []string `json:"available_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, act := range resp.AvailableActions {
		found[act] = true
	}
	if !found["ValidatePaymentRun"] || !found["CancelUnsubmittedRun"] {
		t.Fatalf("expected DRAFT to allow validate/cancel, got %v", resp.AvailableActions)
	}
	if found["SubmitPaymentRun"] {
		t.Fatalf("did not expect SubmitPaymentRun available on DRAFT, got %v", resp.AvailableActions)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAuth())
	w := doRequest(r, http.MethodGet, "/ap11/runs/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
