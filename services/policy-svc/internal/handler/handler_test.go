package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/decisionlog"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/handler"
)

// ── stub store ────────────────────────────────────────────────────────────────

// stubStore implements handler.PolicyStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	policy         *domain.Policy
	policyCreated  bool
	policyErr      error
	version        *domain.PolicyVersion
	versionCreated bool
	versionErr     error
	findVersion    *domain.PolicyVersion
	findVersionErr error
	activated      *domain.PolicyVersion
	superseded     []*domain.PolicyVersion
	transitioned   bool
	activateErr    error
	history        []*domain.PolicyVersion
	historyErr     error
	applicable     []*domain.ApplicablePolicyVersion
	applicableErr  error
}

func (s *stubStore) CreatePolicy(_ context.Context, _ domain.CreatePolicyParams) (*domain.Policy, bool, error) {
	return s.policy, s.policyCreated, s.policyErr
}

func (s *stubStore) CreatePolicyVersion(_ context.Context, _ domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error) {
	return s.version, s.versionCreated, s.versionErr
}

func (s *stubStore) FindPolicyVersionByID(_ context.Context, _ string) (*domain.PolicyVersion, error) {
	return s.findVersion, s.findVersionErr
}

func (s *stubStore) ActivateVersion(_ context.Context, _ string, _ string) (*domain.PolicyVersion, []*domain.PolicyVersion, bool, error) {
	return s.activated, s.superseded, s.transitioned, s.activateErr
}

func (s *stubStore) ListVersionHistory(_ context.Context, _ string) ([]*domain.PolicyVersion, error) {
	return s.history, s.historyErr
}

func (s *stubStore) FindApplicableVersions(_ context.Context, _ string, _, _ *string) ([]*domain.ApplicablePolicyVersion, error) {
	return s.applicable, s.applicableErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

// stubPublisher implements handler.EventPublisher for unit testing.
// Records every call so tests can assert on publish behaviour; never
// errors unless a test explicitly wants to exercise the failure path.
type stubPublisher struct {
	createdCalls   int
	updatedCalls   int
	activatedCalls int
	retiredCalls   int
}

func (p *stubPublisher) PublishPolicyCreated(_ context.Context, _ domain.Policy, _ string) error {
	p.createdCalls++
	return nil
}

func (p *stubPublisher) PublishPolicyUpdated(_ context.Context, _ domain.PolicyVersion, _ string) error {
	p.updatedCalls++
	return nil
}

func (p *stubPublisher) PublishVersionActivated(_ context.Context, _ domain.PolicyVersion, _ string) error {
	p.activatedCalls++
	return nil
}

func (p *stubPublisher) PublishRuleRetired(_ context.Context, _ domain.PolicyVersion, _ string) error {
	p.retiredCalls++
	return nil
}

// ── stub decision log client ─────────────────────────────────────────────────

// stubDecisionLog implements handler.decisionlog.Client (via the
// decisionlog.Client interface) for unit testing. Records every call so
// tests can assert on recording behaviour.
type stubDecisionLog struct {
	calls int
	last  decisionlog.RecordDecisionParams
	err   error
}

func (d *stubDecisionLog) RecordDecision(_ context.Context, params decisionlog.RecordDecisionParams) error {
	d.calls++
	d.last = params
	return d.err
}

func newTestRouter(s *stubStore) chi.Router {
	return newTestRouterFull(s, &stubPublisher{}, &stubDecisionLog{})
}

func newTestRouterWithPublisher(s *stubStore, p *stubPublisher) chi.Router {
	return newTestRouterFull(s, p, &stubDecisionLog{})
}

func newTestRouterFull(s *stubStore, p *stubPublisher, d *stubDecisionLog) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, d, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// ── CreatePolicy ─────────────────────────────────────────────────────────────

func TestCreatePolicy_Created(t *testing.T) {
	store := &stubStore{
		policy: &domain.Policy{
			PolicyID:             "p-1",
			PolicyCode:           "APPROVAL_5K",
			PolicyName:           "5K Approval Threshold",
			PolicyType:           "APPROVAL_THRESHOLD",
			CreatedByPrincipalID: "admin-1",
		},
		policyCreated: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	body := `{"policy_code":"APPROVAL_5K","policy_name":"5K Approval Threshold","policy_type":"APPROVAL_THRESHOLD","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.createdCalls != 1 {
		t.Errorf("expected policy.created published once, got %d", pub.createdCalls)
	}
	var got domain.Policy
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.PolicyCode != "APPROVAL_5K" {
		t.Errorf("expected policy_code APPROVAL_5K, got %s", got.PolicyCode)
	}
}

func TestCreatePolicy_IdempotentReplay(t *testing.T) {
	store := &stubStore{
		policy:        &domain.Policy{PolicyID: "p-1", PolicyCode: "APPROVAL_5K"},
		policyCreated: false,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	body := `{"policy_code":"APPROVAL_5K","policy_name":"5K Approval Threshold","policy_type":"APPROVAL_THRESHOLD","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d", w.Code)
	}
	if pub.createdCalls != 0 {
		t.Errorf("expected policy.created NOT published on idempotent replay, got %d calls", pub.createdCalls)
	}
}

func TestCreatePolicy_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"policy_code":"APPROVAL_5K"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreatePolicy_Conflict(t *testing.T) {
	store := &stubStore{policyErr: domain.ErrConflict}
	r := newTestRouter(store)

	body := `{"policy_code":"APPROVAL_5K","policy_name":"5K Approval Threshold","policy_type":"APPROVAL_THRESHOLD","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestCreatePolicy_StoreUnavailable(t *testing.T) {
	store := &stubStore{policyErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	body := `{"policy_code":"APPROVAL_5K","policy_name":"5K Approval Threshold","policy_type":"APPROVAL_THRESHOLD","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── CreatePolicyVersion ──────────────────────────────────────────────────────

func TestCreatePolicyVersion_Created(t *testing.T) {
	store := &stubStore{
		version: &domain.PolicyVersion{
			PolicyVersionID: "pv-1",
			PolicyID:        "p-1",
			VersionStatus:   "DRAFT",
			RulePayload:     []byte(`{"threshold_amount":5000}`),
		},
		versionCreated: true,
	}
	r := newTestRouter(store)

	body := `{"rule_payload":{"threshold_amount":5000},"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePolicyVersion_MissingEffectiveFrom(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreatePolicyVersion_PolicyNotFound(t *testing.T) {
	store := &stubStore{versionErr: domain.ErrPolicyNotFound}
	r := newTestRouter(store)

	body := `{"effective_from":"2026-01-01T00:00:00Z","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/missing/versions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ActivateVersion ──────────────────────────────────────────────────────────

func TestActivateVersion_Success(t *testing.T) {
	store := &stubStore{
		findVersion:  &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "DRAFT"},
		activated:    &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "ACTIVE"},
		superseded:   []*domain.PolicyVersion{{PolicyVersionID: "pv-0", PolicyID: "p-1", VersionStatus: "SUPERSEDED"}},
		transitioned: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.PolicyVersion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.VersionStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", got.VersionStatus)
	}
	if pub.activatedCalls != 1 {
		t.Errorf("expected policy.version.activated published once, got %d", pub.activatedCalls)
	}
	if pub.retiredCalls != 1 {
		t.Errorf("expected policy.rule.retired published once (for the superseded version), got %d", pub.retiredCalls)
	}
}

func TestActivateVersion_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{
		findVersion:  &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "ACTIVE"},
		activated:    &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "ACTIVE"},
		transitioned: false, // store signals this was a no-op, not a real transition
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pub.activatedCalls != 0 || pub.retiredCalls != 0 {
		t.Errorf("expected no events published on idempotent no-op, got activated=%d retired=%d",
			pub.activatedCalls, pub.retiredCalls)
	}
}

func TestActivateVersion_MissingActor(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestActivateVersion_PolicyIDMismatch(t *testing.T) {
	// version_id resolves, but belongs to a different policy_id than the path.
	store := &stubStore{
		findVersion: &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-OTHER", VersionStatus: "DRAFT"},
	}
	r := newTestRouter(store)

	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on policy_id mismatch, got %d", w.Code)
	}
}

func TestActivateVersion_InvalidTransition(t *testing.T) {
	store := &stubStore{
		findVersion: &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "RETIRED"},
		activateErr: domain.ErrInvalidTransition,
	}
	r := newTestRouter(store)

	body := `{"activated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ── ListVersionHistory ───────────────────────────────────────────────────────

func TestListVersionHistory_EmptyReturnsArray(t *testing.T) {
	store := &stubStore{history: nil}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/policies/p-1/versions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListVersionHistory_NotFound(t *testing.T) {
	store := &stubStore{historyErr: domain.ErrPolicyNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/policies/missing/versions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListVersionHistory_NewestFirst(t *testing.T) {
	now := time.Now().UTC()
	store := &stubStore{
		history: []*domain.PolicyVersion{
			{PolicyVersionID: "pv-2", EffectiveFrom: now, VersionStatus: "ACTIVE"},
			{PolicyVersionID: "pv-1", EffectiveFrom: now.Add(-time.Hour), VersionStatus: "SUPERSEDED"},
		},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/policies/p-1/versions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got []domain.PolicyVersion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if len(got) != 2 || got[0].PolicyVersionID != "pv-2" {
		t.Fatalf("expected pv-2 first (newest), got %+v", got)
	}
}

// ── ListApplicablePolicyVersions ─────────────────────────────────────────────

func TestListApplicablePolicyVersions_MissingPolicyType(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodGet, "/v1/policies", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestListApplicablePolicyVersions_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{applicable: nil})

	req := httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListApplicablePolicyVersions_Success(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{PolicyVersionID: "pv-1", VersionStatus: "ACTIVE"},
				PolicyCode:    "APPROVAL_5K",
			},
		},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD&tenant_id=t-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got []domain.ApplicablePolicyVersion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if len(got) != 1 || got[0].PolicyCode != "APPROVAL_5K" {
		t.Fatalf("expected 1 result with policy_code APPROVAL_5K, got %+v", got)
	}
}

func TestListApplicablePolicyVersions_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{applicableErr: domain.ErrStoreUnavailable})

	req := httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── Evaluate ─────────────────────────────────────────────────────────────────

func TestEvaluate_ApprovalRequired(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{
					PolicyVersionID: "pv-1",
					VersionStatus:   "ACTIVE",
					RulePayload:     []byte(`{"threshold_amount":5000}`),
				},
				PolicyCode: "APPROVAL_5K",
			},
		},
	}
	decisionLog := &stubDecisionLog{}
	r := newTestRouterFull(store, &stubPublisher{}, decisionLog)

	tenantID := "t-1"
	body := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"t-1","action_context":{"amount":7500},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Result          string `json:"result"`
		PolicyVersionID string `json:"policy_version_id"`
		RuleBasis       string `json:"rule_basis"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Result != "APPROVAL_REQUIRED" {
		t.Errorf("expected APPROVAL_REQUIRED, got %s", got.Result)
	}
	if got.PolicyVersionID != "pv-1" {
		t.Errorf("expected policy_version_id pv-1, got %s", got.PolicyVersionID)
	}
	if got.RuleBasis != "APPROVAL_5K:pv-1" {
		t.Errorf("expected rule_basis APPROVAL_5K:pv-1, got %s", got.RuleBasis)
	}

	// The evidence obligation: every real evaluation must be recorded.
	if decisionLog.calls != 1 {
		t.Fatalf("expected RecordDecision called once, got %d", decisionLog.calls)
	}
	if decisionLog.last.ActorID != "admin-1" {
		t.Errorf("expected ActorID admin-1, got %s", decisionLog.last.ActorID)
	}
	if decisionLog.last.Outcome != "APPROVAL_REQUIRED" {
		t.Errorf("expected Outcome APPROVAL_REQUIRED, got %s", decisionLog.last.Outcome)
	}
	if decisionLog.last.RuleBasis != "APPROVAL_5K:pv-1" {
		t.Errorf("expected RuleBasis APPROVAL_5K:pv-1, got %s", decisionLog.last.RuleBasis)
	}
	if decisionLog.last.TenantID == nil || *decisionLog.last.TenantID != tenantID {
		t.Errorf("expected TenantID t-1 forwarded, got %v", decisionLog.last.TenantID)
	}
	if decisionLog.last.DecisionID == "" {
		t.Errorf("expected a generated DecisionID when none supplied")
	}
}

func TestEvaluate_MissingEvaluatedByPrincipalID(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestEvaluate_DecisionLogFailure_StillReturns200(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{
					PolicyVersionID: "pv-1",
					RulePayload:     []byte(`{"threshold_amount":5000}`),
				},
				PolicyCode: "APPROVAL_5K",
			},
		},
	}
	decisionLog := &stubDecisionLog{err: fmt.Errorf("governance-decision-log-svc unreachable")}
	r := newTestRouterFull(store, &stubPublisher{}, decisionLog)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Best-effort: evaluation must still succeed even if evidence recording fails.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when decision-log call fails, got %d: %s", w.Code, w.Body.String())
	}
	if decisionLog.calls != 1 {
		t.Errorf("expected RecordDecision to have been attempted once, got %d", decisionLog.calls)
	}
}

func TestEvaluate_WithinThreshold(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{
					PolicyVersionID: "pv-1",
					VersionStatus:   "ACTIVE",
					RulePayload:     []byte(`{"threshold_amount":5000}`),
				},
				PolicyCode: "APPROVAL_5K",
			},
		},
	}
	r := newTestRouter(store)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Result != "WITHIN_THRESHOLD" {
		t.Errorf("expected WITHIN_THRESHOLD, got %s", got.Result)
	}
}

func TestEvaluate_AmountEqualsThreshold_IsWithinThreshold(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{
					PolicyVersionID: "pv-1",
					VersionStatus:   "ACTIVE",
					RulePayload:     []byte(`{"threshold_amount":5000}`),
				},
				PolicyCode: "APPROVAL_5K",
			},
		},
	}
	r := newTestRouter(store)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":5000},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Result != "WITHIN_THRESHOLD" {
		t.Errorf("expected WITHIN_THRESHOLD when amount == threshold, got %s", got.Result)
	}
}

func TestEvaluate_MissingPolicyType(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"action_context":{"amount":1000}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestEvaluate_MissingActionContextAmount(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{
				PolicyVersion: domain.PolicyVersion{
					PolicyVersionID: "pv-1",
					RulePayload:     []byte(`{"threshold_amount":5000}`),
				},
			},
		},
	}
	r := newTestRouter(store)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEvaluate_NoApplicablePolicy(t *testing.T) {
	decisionLog := &stubDecisionLog{}
	r := newTestRouterFull(&stubStore{applicable: nil}, &stubPublisher{}, decisionLog)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if decisionLog.calls != 0 {
		t.Errorf("expected no decision recorded when nothing was evaluated, got %d calls", decisionLog.calls)
	}
}

func TestEvaluate_PolicyTypeNotImplemented(t *testing.T) {
	store := &stubStore{
		applicable: []*domain.ApplicablePolicyVersion{
			{PolicyVersion: domain.PolicyVersion{PolicyVersionID: "pv-1"}},
		},
	}
	decisionLog := &stubDecisionLog{}
	r := newTestRouterFull(store, &stubPublisher{}, decisionLog)

	body := `{"policy_type":"SPEND_CONTROL","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
	if decisionLog.calls != 0 {
		t.Errorf("expected no decision recorded for an unimplemented policy_type, got %d calls", decisionLog.calls)
	}
}
