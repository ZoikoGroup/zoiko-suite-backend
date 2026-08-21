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
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
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

	// call counters + captured actor, so the authorization tests can prove a
	// refused request never reached the store and that the recorded actor is
	// the header principal rather than anything in the body.
	createPolicyCalls  int
	createVersionCalls int
	activateCalls      int
	activateActor      string

	// Chunk 10: control tests & attestations.
	definition              *domain.ControlTestDefinition
	definitionErr           error
	executions              []*domain.ControlTestExecution
	effectiveness           *domain.ControlEffectiveness
	attestation             *domain.Attestation
	attestationErr          error
	revoked                 *domain.Attestation
	revokeErr               error
	createDefinitionErr     error
	createExecutionErr      error
	createDefinitionCalls   int
	createExecutionCalls    int
	createExecutionTester   string
	createAttestationCalls  int
	createAttestationSigner string
}

func (s *stubStore) CreatePolicy(_ context.Context, _ domain.CreatePolicyParams) (*domain.Policy, bool, error) {
	s.createPolicyCalls++
	return s.policy, s.policyCreated, s.policyErr
}

func (s *stubStore) CreatePolicyVersion(_ context.Context, _ domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error) {
	s.createVersionCalls++
	return s.version, s.versionCreated, s.versionErr
}

func (s *stubStore) FindPolicyVersionByID(_ context.Context, _ string) (*domain.PolicyVersion, error) {
	return s.findVersion, s.findVersionErr
}

func (s *stubStore) ActivateVersion(_ context.Context, _ string, actorID string) (*domain.PolicyVersion, []*domain.PolicyVersion, bool, error) {
	s.activateCalls++
	s.activateActor = actorID
	return s.activated, s.superseded, s.transitioned, s.activateErr
}

func (s *stubStore) ListVersionHistory(_ context.Context, _ string) ([]*domain.PolicyVersion, error) {
	return s.history, s.historyErr
}

func (s *stubStore) FindApplicableVersions(_ context.Context, _ string, _, _ *string) ([]*domain.ApplicablePolicyVersion, error) {
	return s.applicable, s.applicableErr
}

// ── Chunk 10: control tests & attestations ──────────────────────────────────

func (s *stubStore) CreateControlTestDefinition(_ context.Context, params domain.CreateControlTestDefinitionParams) (*domain.ControlTestDefinition, bool, error) {
	s.createDefinitionCalls++
	if s.createDefinitionErr != nil {
		return nil, false, s.createDefinitionErr
	}
	return &domain.ControlTestDefinition{
		ControlTestDefinitionID: "ctd-1",
		ControlRef:              params.ControlRef,
		TestCode:                params.TestCode,
		Title:                   params.Title,
		Methodology:             params.Methodology,
		DesignStatus:            "DESIGNED",
		CreatedByPrincipalID:    params.CreatedByPrincipalID,
	}, true, nil
}

func (s *stubStore) FindControlTestDefinitionByID(_ context.Context, _ string) (*domain.ControlTestDefinition, error) {
	return s.definition, s.definitionErr
}

func (s *stubStore) CreateControlTestExecution(_ context.Context, params domain.CreateControlTestExecutionParams) (*domain.ControlTestExecution, error) {
	s.createExecutionCalls++
	s.createExecutionTester = params.TesterPrincipalID
	// Mirrors PgStore.CreateControlTestExecution: it validates the parent
	// definition exists before inserting.
	if s.definitionErr != nil {
		return nil, s.definitionErr
	}
	if s.createExecutionErr != nil {
		return nil, s.createExecutionErr
	}
	return &domain.ControlTestExecution{
		ControlTestExecutionID:  "cte-1",
		ControlTestDefinitionID: params.ControlTestDefinitionID,
		PeriodStart:             params.PeriodStart,
		PeriodEnd:               params.PeriodEnd,
		TesterPrincipalID:       params.TesterPrincipalID,
		Result:                  params.Result,
	}, nil
}

func (s *stubStore) ListControlTestExecutions(_ context.Context, _ string) ([]*domain.ControlTestExecution, error) {
	return s.executions, nil
}

func (s *stubStore) ResolveControlEffectiveness(_ context.Context, controlRef string) (*domain.ControlEffectiveness, error) {
	if s.effectiveness != nil {
		return s.effectiveness, nil
	}
	return &domain.ControlEffectiveness{ControlRef: controlRef, DesignStatus: "NOT_TESTED", OperatingEffectiveness: "NO_EXECUTIONS_RECORDED"}, nil
}

func (s *stubStore) CreateAttestation(_ context.Context, params domain.CreateAttestationParams) (*domain.Attestation, error) {
	s.createAttestationCalls++
	s.createAttestationSigner = params.SignerPrincipalID
	return &domain.Attestation{
		AttestationID:        "att-1",
		Statement:            params.Statement,
		StatementVersion:     params.StatementVersion,
		SubjectRef:           params.SubjectRef,
		SignerPrincipalID:    params.SignerPrincipalID,
		SignerRole:           params.SignerRole,
		AttestationStatus:    "ACTIVE",
		CreatedByPrincipalID: params.CreatedByPrincipalID,
	}, nil
}

func (s *stubStore) FindAttestationByID(_ context.Context, _ string) (*domain.Attestation, error) {
	return s.attestation, s.attestationErr
}

func (s *stubStore) RevokeAttestation(_ context.Context, _, _ string) (*domain.Attestation, error) {
	return s.revoked, s.revokeErr
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
	return newTestRouterWithAuthz(s, p, d, &stubAuthz{})
}

// newTestRouterWithAuthz is the same wiring with an explicit authz client —
// used by the tests that exercise the 403 and fail-closed 503 paths.
// newTestRouterWithAuthz mounts TenantContext, which cmd/server/main.go mounts
// in front of these same routes. It used to be omitted here, so every handler
// under test saw an empty tenant scope — and the store's lookup, which widens
// itself to every tenant when the scope is empty, was exercised in exactly the
// configuration the tests were meant to rule out.
func newTestRouterWithAuthz(s *stubStore, p *stubPublisher, d *stubDecisionLog, az *stubAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, d, az, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newControlTestRouter wires a router with control-test/attestation routes
// included — the base newTestRouter* helpers above only register
// handler.RegisterRoutes.
func newControlTestRouter(s *stubStore, az *stubAuthz) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubDecisionLog{}, az, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterControlTestRoutes(r, h)
	return r
}

// testAuthzScopeID stands in for config.AuthZPlatformScopeID — the synthetic
// legal entity a policy that is not entity-scoped is authorized against.
const testAuthzScopeID = "00000000-0000-0000-0000-0000000000f2"

// testPrincipal is what the gateway ForwardAuth middleware would set in
// X-Principal-Id after verifying the caller identity envelope.
const testPrincipal = "principal-policy-admin"

// stubAuthz records what it was asked and answers with err.
type stubAuthz struct {
	err error

	calls      int
	principal  string
	scope      string
	actionType string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls++
	a.principal, a.scope, a.actionType = principalID, legalEntityID, actionType
	return a.err
}

// testTenant is the caller's verified tenant scope, as the gateway would set it
// in X-Tenant-Id. It is a UUID because policy_versions.tenant_id is a uuid
// column, and a non-uuid scope can only ever see global versions.
const testTenant = "11111111-1111-1111-1111-111111111111"

// otherTenant is a tenant the caller has no scope in.
const otherTenant = "22222222-2222-2222-2222-222222222222"

// authed stamps the gateway-verified identity headers onto a request. Every
// mutating route requires the principal, and every tenant-scoped route requires
// the tenant — only the principal used to be stamped, so the scope-related
// behaviour of every one of these tests was the no-scope fallback path.
func authed(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Tenant-Id", testTenant)
	return req
}

// scoped stamps only the tenant scope, for reads that need no principal.
func scoped(req *http.Request) *http.Request {
	req.Header.Set("X-Tenant-Id", testTenant)
	return req
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePolicyVersion_MissingEffectiveFrom(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/missing/versions", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body)))
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

// ── GetPolicyVersionByID ─────────────────────────────────────────────────────

func TestGetPolicyVersionByID_Found(t *testing.T) {
	store := &stubStore{
		findVersion: &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "SUPERSEDED"},
	}
	r := newTestRouter(store)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, scoped(httptest.NewRequest(http.MethodGet, "/v1/policy-versions/pv-1", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.PolicyVersion
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	// The whole point of this endpoint: it returns a version regardless of
	// whether it's still ACTIVE — a replay needs the EXACT version used at
	// decision time, not "whatever is current now".
	if got.VersionStatus != "SUPERSEDED" {
		t.Errorf("expected a SUPERSEDED version to still be fetchable by ID, got status %s", got.VersionStatus)
	}
}

func TestGetPolicyVersionByID_NotFound(t *testing.T) {
	store := &stubStore{findVersionErr: domain.ErrPolicyVersionNotFound}
	r := newTestRouter(store)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, scoped(httptest.NewRequest(http.MethodGet, "/v1/policy-versions/does-not-exist", nil)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body)))
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

// TestActivateVersion_ActorComesFromHeaderNotBody replaces an earlier test
// that asserted a 400 when activated_by_principal_id was missing from the
// body. That field is now ignored: a caller must not be able to choose what
// the audit trail records about them, so the actor is read from the
// gateway-verified header and an empty body is perfectly valid.
func TestActivateVersion_ActorComesFromHeaderNotBody(t *testing.T) {
	store := &stubStore{
		findVersion:  &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "DRAFT"},
		activated:    &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "ACTIVE"},
		transitioned: true,
	}
	r := newTestRouter(store)

	// Body names someone else entirely; the header is what must be recorded.
	body := `{"activated_by_principal_id":"someone-else"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.activateActor != testPrincipal {
		t.Errorf("store recorded actor %q, want the header principal %q", store.activateActor, testPrincipal)
	}
}

// ── authentication and authorization ─────────────────────────────────────────

// mutatingRoutes is every route that changes policy state, so a route added
// later cannot quietly skip the gate.
var mutatingRoutes = []struct {
	name string
	path string
	body string
}{
	{"create policy", "/v1/policies", `{"policy_code":"C","policy_name":"N","policy_type":"APPROVAL_THRESHOLD"}`},
	{"create version", "/v1/policies/p-1/versions", `{"effective_from":"2025-01-01T00:00:00Z","rule_payload":{"threshold_amount":100}}`},
	{"activate version", "/v1/policies/p-1/versions/pv-1/activate", `{}`},
}

func okStore() *stubStore {
	return &stubStore{
		policy:         &domain.Policy{PolicyID: "p-1", PolicyCode: "C"},
		policyCreated:  true,
		version:        &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "DRAFT"},
		versionCreated: true,
		findVersion:    &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "DRAFT"},
		activated:      &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "ACTIVE"},
		transitioned:   true,
	}
}

// TestMutatingRoutes_401_WithoutPrincipal — policy-svc took the acting
// principal from the request body, so the audit columns recorded whatever the
// caller typed and an unauthenticated caller could write as anyone.
func TestMutatingRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range mutatingRoutes {
		t.Run(route.name, func(t *testing.T) {
			r := newTestRouter(okStore())
			// Deliberately NOT wrapped in authed().
			req := httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a principal, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestMutatingRoutes_403_Denied — a denial must stop the write.
func TestMutatingRoutes_403_Denied(t *testing.T) {
	for _, route := range mutatingRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := okStore()
			az := &stubAuthz{err: domain.ErrAuthorizationDenied}
			r := newTestRouterWithAuthz(store, &stubPublisher{}, &stubDecisionLog{}, az)

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
			if az.calls != 1 {
				t.Errorf("expected exactly 1 authorization check, got %d", az.calls)
			}
			if az.principal != testPrincipal {
				t.Errorf("authz saw principal %q, want %q", az.principal, testPrincipal)
			}
			if store.createPolicyCalls > 0 || store.createVersionCalls > 0 || store.activateCalls > 0 {
				t.Error("FAIL: a denied request still reached the store")
			}
		})
	}
}

// TestMutatingRoutes_503_AuthzUnavailableFailsClosed — an unreachable
// authorization service must block the mutation, not wave it through.
func TestMutatingRoutes_503_AuthzUnavailableFailsClosed(t *testing.T) {
	for _, route := range mutatingRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := okStore()
			az := &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}
			r := newTestRouterWithAuthz(store, &stubPublisher{}, &stubDecisionLog{}, az)

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
			}
			if store.createPolicyCalls > 0 || store.createVersionCalls > 0 || store.activateCalls > 0 {
				t.Error("FAIL: a request reached the store without an authorization decision")
			}
		})
	}
}

// TestActivateVersion_AuthorizedAgainstStoredScope — activation must be
// checked against the entity the version actually binds, read from the stored
// record, not from anything the caller supplied.
func TestActivateVersion_AuthorizedAgainstStoredScope(t *testing.T) {
	entity := "legal-entity-77"
	store := okStore()
	store.findVersion = &domain.PolicyVersion{
		PolicyVersionID: "pv-1", PolicyID: "p-1", VersionStatus: "DRAFT", LegalEntityID: &entity,
	}
	az := &stubAuthz{}
	r := newTestRouterWithAuthz(store, &stubPublisher{}, &stubDecisionLog{}, az)

	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if az.scope != entity {
		t.Errorf("authz scope = %q, want the version's legal entity %q", az.scope, entity)
	}
	if az.actionType != "POLICY_VERSION_ACTIVATE" {
		t.Errorf("action_type = %q, want POLICY_VERSION_ACTIVATE", az.actionType)
	}
}

// TestCreatePolicy_UsesPlatformScopeWhenUnscoped — a Policy header row has no
// legal entity, and authorization-svc rejects an empty legal_entity_id.
func TestCreatePolicy_UsesPlatformScopeWhenUnscoped(t *testing.T) {
	az := &stubAuthz{}
	r := newTestRouterWithAuthz(okStore(), &stubPublisher{}, &stubDecisionLog{}, az)

	body := `{"policy_code":"C","policy_name":"N","policy_type":"APPROVAL_THRESHOLD"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if az.scope != testAuthzScopeID {
		t.Errorf("authz scope = %q, want the platform scope %q", az.scope, testAuthzScopeID)
	}
	if az.scope == "" {
		t.Error("an empty legal_entity_id would be rejected by authorization-svc with 400")
	}
}

// TestEvaluate_NotGated — Evaluate is a query other services call on the hot
// path; it changes no policy state, so requiring a POLICY_* grant on every
// caller would be wrong. It stays open deliberately.
func TestEvaluate_NotGated(t *testing.T) {
	az := &stubAuthz{err: domain.ErrAuthorizationDenied}
	store := &stubStore{applicable: []*domain.ApplicablePolicyVersion{}}
	r := newTestRouterWithAuthz(store, &stubPublisher{}, &stubDecisionLog{}, az)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_amount":100,"evaluated_by_principal_id":"caller-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("Evaluate must not be gated, got %d", w.Code)
	}
	if az.calls != 0 {
		t.Errorf("Evaluate should not call authorization-svc, got %d calls", az.calls)
	}
}

func TestActivateVersion_PolicyIDMismatch(t *testing.T) {
	// version_id resolves, but belongs to a different policy_id than the path.
	store := &stubStore{
		findVersion: &domain.PolicyVersion{PolicyVersionID: "pv-1", PolicyID: "p-OTHER", VersionStatus: "DRAFT"},
	}
	r := newTestRouter(store)

	body := `{"activated_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body)))
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
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions/pv-1/activate", bytes.NewBufferString(body)))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies/p-1/versions", nil))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies/missing/versions", nil))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies/p-1/versions", nil))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD", nil))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD&tenant_id="+testTenant, nil))
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

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD", nil))
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

	tenantID := testTenant
	body := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + testTenant + `","action_context":{"amount":7500},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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
	// Recorded outcome must be in the decision log's vocabulary
	// (GRANTED/DENIED/ESCALATED), not this service's own result vocabulary.
	// ESCALATED, not DENIED: over-threshold routes to an approver rather than
	// refusing the action. The response above keeps APPROVAL_REQUIRED — the two
	// vocabularies are deliberately different, so both are asserted here.
	if decisionLog.last.Outcome != "ESCALATED" {
		t.Errorf("expected recorded Outcome ESCALATED, got %s", decisionLog.last.Outcome)
	}
	if decisionLog.last.RuleBasis != "APPROVAL_5K:pv-1" {
		t.Errorf("expected RuleBasis APPROVAL_5K:pv-1, got %s", decisionLog.last.RuleBasis)
	}
	if decisionLog.last.TenantID == nil || *decisionLog.last.TenantID != tenantID {
		t.Errorf("expected TenantID %s forwarded, got %v", tenantID, decisionLog.last.TenantID)
	}
	if decisionLog.last.DecisionID != "dec-1" {
		t.Errorf("expected DecisionID dec-1 forwarded as-is, got %s", decisionLog.last.DecisionID)
	}
}

func TestEvaluate_MissingEvaluatedByPrincipalID(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestEvaluate_MissingDecisionID(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":5000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

// The GRANTED half of the outcome mapping. Paired with the ESCALATED assertion in
// TestEvaluate_ApprovalRequired_RecordsDecision: an under-threshold evaluation is
// a real authorization and must be recorded as one, or the audit trail cannot
// show that the spend was approved.
func TestEvaluate_WithinThreshold_RecordsGrantedOutcome(t *testing.T) {
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

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if decisionLog.calls != 1 {
		t.Fatalf("expected RecordDecision called once, got %d", decisionLog.calls)
	}
	if decisionLog.last.Outcome != "GRANTED" {
		t.Errorf("expected recorded Outcome GRANTED, got %s", decisionLog.last.Outcome)
	}

	// The API response keeps this service's own result vocabulary — callers switch
	// on it, so normalising the evidence write must not leak into the contract.
	var got struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Result != "WITHIN_THRESHOLD" {
		t.Errorf("expected response result WITHIN_THRESHOLD, got %s", got.Result)
	}
}

func TestEvaluate_MissingPolicyType(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"action_context":{"amount":1000}}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEvaluate_NoApplicablePolicy(t *testing.T) {
	decisionLog := &stubDecisionLog{}
	r := newTestRouterFull(&stubStore{applicable: nil}, &stubPublisher{}, decisionLog)

	body := `{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
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

	body := `{"policy_type":"SPEND_CONTROL","action_context":{"amount":1000},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
	if decisionLog.calls != 0 {
		t.Errorf("expected no decision recorded for an unimplemented policy_type, got %d calls", decisionLog.calls)
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

// TestGetPolicyVersionByID_NoTenantScope_Refused covers the store's
// self-disabling filter: with an empty tenant in context its lookup fell back to
// UNSCOPED, so a request that simply omitted X-Tenant-Id read any tenant's
// policy version by id.
func TestGetPolicyVersionByID_NoTenantScope_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: "pv-1"}})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/policy-versions/pv-1", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
	}
}

// ListVersionHistory had no tenant scoping at all until this fix — any
// authenticated caller could list every tenant's policy versions for a
// policy_id. This proves the missing-header case is now refused, same
// as its sibling GetPolicyVersionByID above.
func TestListVersionHistory_NoTenantScope_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{history: []*domain.PolicyVersion{{PolicyVersionID: "pv-1"}}})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/policies/p-1/versions", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListApplicablePolicyVersions_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodGet,
		"/v1/policies?policy_type=APPROVAL_THRESHOLD&tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 reading another tenant's policy set, got %d: %s", w.Code, w.Body.String())
	}
}

// A policy version decides what the platform enforces, so publishing one into
// another tenant is the most consequential write this service has.
func TestCreatePolicyVersion_ForeignTenantBody_Refused(t *testing.T) {
	store := &stubStore{policy: &domain.Policy{PolicyID: "p-1"}}
	r := newTestRouter(store)
	body := `{"tenant_id":"` + otherTenant + `","rule_payload":{"threshold_amount":5000},"effective_from":"2026-01-01T00:00:00Z"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 publishing into another tenant, got %d: %s", w.Code, w.Body.String())
	}
}

// A global version (no tenant_id) bound to one legal entity is an incoherent
// scope — and it used to be the way to reach global scope while authorizing only
// against an entity the caller already held.
func TestCreatePolicyVersion_GlobalScopeWithLegalEntity_Refused(t *testing.T) {
	store := &stubStore{policy: &domain.Policy{PolicyID: "p-1"}}
	r := newTestRouter(store)
	body := `{"legal_entity_id":"33333333-3333-3333-3333-333333333333","rule_payload":{"threshold_amount":5000},"effective_from":"2026-01-01T00:00:00Z"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/policies/p-1/versions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a global version naming a legal entity, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEvaluate_ForeignTenantBody_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	body := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + otherTenant + `","action_context":{"amount":7500},"evaluated_by_principal_id":"admin-1","decision_id":"dec-1"}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/policies/evaluate", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 evaluating against another tenant's policy set, got %d: %s", w.Code, w.Body.String())
	}
}
