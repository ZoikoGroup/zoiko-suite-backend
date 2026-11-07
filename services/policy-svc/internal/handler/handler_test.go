package handler_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	svcenvelope "zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/handler"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
	"zoiko.io/policy-svc/internal/store"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

const (
	testAuthzScopeID = "00000000-0000-0000-0000-0000000000f2"
	testPrincipal    = "principal-policy-admin"
	testTenant       = "11111111-1111-1111-1111-111111111111"
	otherTenant      = "22222222-2222-2222-2222-222222222222"
	testEntity       = "33333333-3333-3333-3333-333333333333"
	otherEntity      = "44444444-4444-4444-4444-444444444444"
	policyID         = "aaaaaaaa-0000-0000-0000-000000000001"
	otherPolicyID    = "aaaaaaaa-0000-0000-0000-000000000002"
	versionID        = "bbbbbbbb-0000-0000-0000-000000000001"
	otherVersionID   = "bbbbbbbb-0000-0000-0000-000000000002"
	evidenceID       = "cccccccc-0000-0000-0000-000000000001"
)

var fixedNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func strPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

func digestOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ── stubs ────────────────────────────────────────────────────────────────────

// stubStore implements handler.PolicyStore in memory and records what the
// handler asked of it.
type stubStore struct {
	policy        *domain.Policy
	policyCreated bool
	policyErr     error
	findPolicyErr error

	version         *domain.PolicyVersion
	versionCreated  bool
	versionErr      error
	versionParams   domain.CreatePolicyVersionParams
	findVersion     *domain.PolicyVersion
	findVersionErr  error
	findVersionByID map[string]*domain.PolicyVersion
	history         []*domain.PolicyVersion
	historyErr      error
	transitions     []*domain.VersionTransition

	transitionResult *domain.TransitionResult
	transitionErr    error
	transitionCmd    domain.TransitionCommand

	evaluation        *domain.Evaluation
	findEvaluationErr error
	saveErr           error
	saved             *domain.Evaluation
	findEvalTenant    string

	events []store.OutboxEvent

	createPolicyCalls  int
	createPolicyParams domain.CreatePolicyParams
	createVersionCalls int
	transitionCalls    int
	saveCalls          int

	// control tests & attestations
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
	createDefinitionTenant  string
	createExecutionCalls    int
	createExecutionTester   string
	createExecutionTenant   string
	createAttestationCalls  int
	createAttestationSigner string
	createAttestationTenant string
}

func (s *stubStore) CreatePolicy(_ context.Context, p domain.CreatePolicyParams) (*domain.Policy, bool, error) {
	s.createPolicyCalls++
	s.createPolicyParams = p
	return s.policy, s.policyCreated, s.policyErr
}

func (s *stubStore) FindPolicyByID(_ context.Context, _ string) (*domain.Policy, error) {
	if s.findPolicyErr != nil {
		return nil, s.findPolicyErr
	}
	if s.policy == nil {
		return &domain.Policy{PolicyID: policyID, PolicyType: "APPROVAL_THRESHOLD"}, nil
	}
	return s.policy, nil
}

func (s *stubStore) CreatePolicyVersion(_ context.Context, p domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error) {
	s.createVersionCalls++
	s.versionParams = p
	return s.version, s.versionCreated, s.versionErr
}

func (s *stubStore) FindPolicyVersionByID(_ context.Context, id string) (*domain.PolicyVersion, error) {
	if v, ok := s.findVersionByID[id]; ok {
		return v, nil
	}
	if s.findVersion == nil && s.findVersionErr == nil {
		return nil, domain.ErrPolicyVersionNotFound
	}
	return s.findVersion, s.findVersionErr
}

func (s *stubStore) ListVersionHistory(_ context.Context, _ string) ([]*domain.PolicyVersion, error) {
	return s.history, s.historyErr
}

func (s *stubStore) ListTransitions(_ context.Context, _ string) ([]*domain.VersionTransition, error) {
	return s.transitions, nil
}

func (s *stubStore) Transition(_ context.Context, cmd domain.TransitionCommand) (*domain.TransitionResult, error) {
	s.transitionCalls++
	s.transitionCmd = cmd
	return s.transitionResult, s.transitionErr
}

func (s *stubStore) SaveEvaluation(_ context.Context, ev *domain.Evaluation) (*domain.Evaluation, bool, error) {
	s.saveCalls++
	if s.saveErr != nil {
		return nil, false, s.saveErr
	}
	cp := *ev
	cp.EvaluationID = evidenceID
	cp.LedgerStatus = domain.LedgerPending
	s.saved = &cp
	return &cp, true, nil
}

func (s *stubStore) FindEvaluationByDecision(_ context.Context, tenantID, _ string) (*domain.Evaluation, error) {
	s.findEvalTenant = tenantID
	if s.findEvaluationErr != nil {
		return nil, s.findEvaluationErr
	}
	if s.evaluation == nil {
		return nil, domain.ErrEvaluationNotFound
	}
	return s.evaluation, nil
}

func (s *stubStore) FindEvaluationByID(_ context.Context, tenantID, _ string) (*domain.Evaluation, error) {
	s.findEvalTenant = tenantID
	if s.evaluation == nil {
		return nil, domain.ErrEvaluationNotFound
	}
	return s.evaluation, nil
}

func (s *stubStore) EnqueueEvent(_ context.Context, ev store.OutboxEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *stubStore) CreateControlTestDefinition(_ context.Context, params domain.CreateControlTestDefinitionParams) (*domain.ControlTestDefinition, bool, error) {
	s.createDefinitionCalls++
	s.createDefinitionTenant = params.TenantID
	if s.createDefinitionErr != nil {
		return nil, false, s.createDefinitionErr
	}
	return &domain.ControlTestDefinition{
		ControlTestDefinitionID: "dddddddd-0000-0000-0000-000000000001",
		TenantID:                &params.TenantID,
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
	s.createExecutionTenant = params.TenantID
	if s.definitionErr != nil {
		return nil, s.definitionErr
	}
	if s.createExecutionErr != nil {
		return nil, s.createExecutionErr
	}
	return &domain.ControlTestExecution{
		ControlTestExecutionID:  "eeeeeeee-0000-0000-0000-000000000001",
		TenantID:                &params.TenantID,
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
	s.createAttestationTenant = params.TenantID
	return &domain.Attestation{
		AttestationID:        "ffffffff-0000-0000-0000-000000000001",
		TenantID:             &params.TenantID,
		Statement:            params.Statement,
		SignerPrincipalID:    params.SignerPrincipalID,
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

// stubCandidates is the resolution cache stand-in.
type stubCandidates struct {
	candidates []*domain.ApplicablePolicyVersion
	err        error
	calls      int
	tenant     *string
	entity     *string
}

func (c *stubCandidates) Candidates(_ context.Context, _ string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error) {
	c.calls++
	c.tenant, c.entity = tenantID, legalEntityID
	return c.candidates, c.err
}

// stubAuthz records every question and answers with err (or per action).
type stubAuthz struct {
	err      error
	byAction map[string]error

	calls      int
	principal  string
	scope      string
	actionType string
	tenantID   string
	actions    []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType, tenantID string) error {
	a.calls++
	a.principal, a.scope, a.actionType, a.tenantID = principalID, legalEntityID, actionType, tenantID
	a.actions = append(a.actions, actionType)
	if err, ok := a.byAction[actionType]; ok {
		return err
	}
	return a.err
}

// ── wiring ───────────────────────────────────────────────────────────────────

type rig struct {
	store *stubStore
	cands *stubCandidates
	az    *stubAuthz
	r     chi.Router
}

type rigOption func(*handler.Options)

func warnEvaluate(o *handler.Options) { o.EvaluateAuthz = handler.ModeWarn }
func warnReads(o *handler.Options)    { o.ReadAuthz = handler.ModeWarn }

// newRig mounts the same middleware cmd/server/main.go puts in front of the
// routes (tenant context and the canonical envelope), so handlers see the
// tenant scope and envelope exactly as in production.
func newRig(s *stubStore, c *stubCandidates, az *stubAuthz, opts ...rigOption) *rig {
	if s == nil {
		s = &stubStore{}
	}
	if c == nil {
		c = &stubCandidates{}
	}
	if az == nil {
		az = &stubAuthz{}
	}
	o := handler.Options{PlatformScopeID: testAuthzScopeID, Now: func() time.Time { return fixedNow }}
	for _, f := range opts {
		f(&o)
	}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(s, c, az, o, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterControlTestRoutes(r, h)
	return &rig{store: s, cands: c, az: az, r: r}
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

var noPrincipal reqOpt = func(r *http.Request) { r.Header.Del("X-Principal-Id") }
var noTenant reqOpt = func(r *http.Request) { r.Header.Del("X-Tenant-Id") }

// do sends a request carrying the gateway-verified principal and tenant
// unless an option removes them.
func (g *rig) do(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Correlation-ID", "corr-1")
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	g.r.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a JSON object: %v: %s", err, w.Body.String())
	}
	return out
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("expected %d, got %d: %s", status, w.Code, w.Body.String())
	}
	if code == "" {
		// Success bodies may be arrays; return the object when there is one.
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	body := decode(t, w)
	if body["code"] != code {
		t.Fatalf("expected code %s, got %v: %s", code, body["code"], w.Body.String())
	}
	return body
}

// ── POST /v1/policies ────────────────────────────────────────────────────────

func TestCreatePolicy_TenantOwnedByDefault(t *testing.T) {
	g := newRig(&stubStore{policy: &domain.Policy{PolicyID: policyID}, policyCreated: true}, nil, nil)
	w := g.do(http.MethodPost, "/v1/policies", `{"policy_code":"AP-5K","policy_name":"AP limit","policy_type":"APPROVAL_THRESHOLD"}`)
	expect(t, w, http.StatusCreated, "")
	if got := g.store.createPolicyParams.TenantID; got == nil || *got != testTenant {
		t.Fatalf("a family must be owned by the caller's tenant by default, got %v", got)
	}
	if g.az.actionType != authz.ActionPolicyCreate || g.az.scope != testAuthzScopeID {
		t.Fatalf("expected POLICY_CREATE at platform scope, got %s at %s", g.az.actionType, g.az.scope)
	}
	if g.store.createPolicyParams.CreatedByPrincipalID != testPrincipal {
		t.Fatalf("creator must be the header principal")
	}
}

func TestCreatePolicy_GlobalScopeNeedsGlobalGrant(t *testing.T) {
	g := newRig(&stubStore{policy: &domain.Policy{PolicyID: policyID}, policyCreated: true}, nil,
		&stubAuthz{byAction: map[string]error{authz.ActionPolicyCreateGlobal: domain.ErrAuthorizationDenied}})
	w := g.do(http.MethodPost, "/v1/policies", `{"policy_code":"AP-5K","policy_name":"AP","policy_type":"APPROVAL_THRESHOLD","scope":"GLOBAL"}`)
	expect(t, w, http.StatusForbidden, "AUTHORIZATION_DENIED")
	if g.store.createPolicyCalls != 0 {
		t.Fatal("a refused global create must not reach the store")
	}
}

func TestCreatePolicy_GlobalScopeHasNoOwner(t *testing.T) {
	g := newRig(&stubStore{policy: &domain.Policy{PolicyID: policyID}, policyCreated: true}, nil, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"AP","policy_name":"AP","policy_type":"T","scope":"GLOBAL"}`), http.StatusCreated, "")
	if g.store.createPolicyParams.TenantID != nil {
		t.Fatal("a GLOBAL family must have no owning tenant")
	}
}

func TestCreatePolicy_ReplayAndConflict(t *testing.T) {
	g := newRig(&stubStore{policy: &domain.Policy{PolicyID: policyID}}, nil, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"A","policy_type":"T"}`), http.StatusOK, "")

	g = newRig(&stubStore{policyErr: domain.ErrConflict}, nil, nil)
	body := expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"B","policy_type":"T"}`), http.StatusConflict, "IDEMPOTENCY_MISMATCH")
	if body["code"] != "IDEMPOTENCY_MISMATCH" || body["detail"] != "policy_conflict" {
		t.Fatalf("the legacy error value must be kept for existing callers, got %v", body)
	}
}

func TestCreatePolicy_Validation(t *testing.T) {
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_name":"A","policy_type":"T"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"`+strings.Repeat("x", 129)+`","policy_name":"A","policy_type":"T"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_id":"not-a-uuid","policy_code":"A","policy_name":"A","policy_type":"T"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"A","policy_type":"T","scope":"WORLD"}`), http.StatusBadRequest, "INVALID_REQUEST")
	if g.store.createPolicyCalls != 0 {
		t.Fatal("invalid input must never reach the store")
	}
}

func TestCreatePolicy_ContextRequired(t *testing.T) {
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies", `{}`, noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{}`, noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{}`, header("X-Tenant-Id", "GLOBAL")), http.StatusBadRequest, "CONTEXT_UNRESOLVED")
}

func TestCreatePolicy_AuthzUnavailableFailsClosed(t *testing.T) {
	g := newRig(nil, nil, &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable})
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"A","policy_type":"T"}`), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	if g.store.createPolicyCalls != 0 {
		t.Fatal("no decision, no write")
	}
}

// ── POST /v1/policies/{id}/versions ──────────────────────────────────────────

const versionBody = `{"rule_payload":{"threshold_amount":5000},"effective_from":"2026-11-01T00:00:00Z"}`

func versionsPath(p string) string { return "/v1/policies/" + p + "/versions" }

func TestCreatePolicyVersion_CreatedAsDraftWithRequestKey(t *testing.T) {
	g := newRig(&stubStore{version: &domain.PolicyVersion{PolicyVersionID: versionID, RowVersion: 1}, versionCreated: true}, nil, nil)
	w := g.do(http.MethodPost, versionsPath(policyID), versionBody, header("Idempotency-Key", "key-1"))
	expect(t, w, http.StatusCreated, "")
	if w.Header().Get("ETag") != `"v1"` {
		t.Fatalf("a version response carries its row_version as ETag, got %q", w.Header().Get("ETag"))
	}
	p := g.store.versionParams
	if p.RequestKey != "key-1" || p.CreatedByPrincipalID != testPrincipal {
		t.Fatalf("expected request key and header creator, got %+v", p)
	}
	if p.TenantID != nil {
		t.Fatal("an omitted tenant_id on a platform family is a global version")
	}
	if g.az.actionType != authz.ActionPolicyVersionCreateGlobal {
		t.Fatalf("a global version needs the _GLOBAL grant, got %s", g.az.actionType)
	}
}

func TestCreatePolicyVersion_TenantOwnedFamilyDefaultsToOwnerScope(t *testing.T) {
	owner := testTenant
	g := newRig(&stubStore{
		policy:  &domain.Policy{PolicyID: policyID, PolicyType: "APPROVAL_THRESHOLD", TenantID: &owner},
		version: &domain.PolicyVersion{PolicyVersionID: versionID}, versionCreated: true,
	}, nil, nil)
	expect(t, g.do(http.MethodPost, versionsPath(policyID), versionBody), http.StatusCreated, "")
	if got := g.store.versionParams.TenantID; got == nil || *got != testTenant {
		t.Fatalf("a tenant-owned family's version must be in its owner's scope, got %v", got)
	}
	if g.az.actionType != authz.ActionPolicyVersionCreate {
		t.Fatalf("expected the tenant-scoped grant, got %s", g.az.actionType)
	}
}

func TestCreatePolicyVersion_NegativePaths(t *testing.T) {
	cases := []struct {
		name, body, code string
		status           int
		opts             []reqOpt
	}{
		{"foreign tenant", `{"tenant_id":"` + otherTenant + `","rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z"}`, "CONTEXT_UNRESOLVED", http.StatusForbidden, nil},
		{"global naming an entity", `{"legal_entity_id":"` + testEntity + `","rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"inverted window", `{"rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z","effective_to":"2026-10-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"equal window", `{"rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z","effective_to":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"missing threshold", `{"rule_payload":{},"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"negative threshold", `{"rule_payload":{"threshold_amount":-1},"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"payload not an object", `{"rule_payload":[1],"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
		{"entity header mismatch", `{"tenant_id":"` + testTenant + `","legal_entity_id":"` + testEntity + `","rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, []reqOpt{header("X-Legal-Entity-Id", otherEntity)}},
		{"bad version id", `{"policy_version_id":"v-1","rule_payload":{"threshold_amount":1},"effective_from":"2026-11-01T00:00:00Z"}`, "INVALID_REQUEST", http.StatusBadRequest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(nil, nil, nil)
			expect(t, g.do(http.MethodPost, versionsPath(policyID), tc.body, tc.opts...), tc.status, tc.code)
			if g.store.createVersionCalls != 0 {
				t.Fatal("refused input must not reach the store")
			}
		})
	}
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodPost, versionsPath("not-a-uuid"), versionBody), http.StatusBadRequest, "INVALID_REQUEST")
}

func TestCreatePolicyVersion_IdempotencyMismatch409(t *testing.T) {
	g := newRig(&stubStore{versionErr: domain.ErrIdempotencyMismatch}, nil, nil)
	body := expect(t, g.do(http.MethodPost, versionsPath(policyID), versionBody, header("Idempotency-Key", "k")), http.StatusConflict, "IDEMPOTENCY_MISMATCH")
	if body["code"] != "IDEMPOTENCY_MISMATCH" || body["detail"] != "this Idempotency-Key or policy_version_id was already used with different content" {
		t.Fatalf("legacy error value lost: %v", body)
	}
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// ifm names the row_version lifecycleStore's version carries; every
// lifecycle command needs one (428 without).
var ifm = header("If-Match", `"v3"`)

func lifecyclePath(cmd string) string {
	return "/v1/policies/" + policyID + "/versions/" + versionID + "/" + cmd
}

func lifecycleStore(v *domain.PolicyVersion) *stubStore {
	if v == nil {
		v = &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, TenantID: strPtr(testTenant), LegalEntityID: strPtr(testEntity), VersionStatus: domain.StatusApproved, RowVersion: 3, CreatedByPrincipalID: "author"}
	}
	next := *v
	next.VersionStatus = domain.StatusActive
	next.RowVersion = v.RowVersion + 1
	return &stubStore{findVersion: v, transitionResult: &domain.TransitionResult{Version: &next, Transitioned: true}}
}

// Audit row "Activate with no body" — the documented optional body used to
// fail with 400 invalid_json (io.EOF).
func TestActivate_EmptyBodyIsAccepted(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, nil)
	w := g.do(http.MethodPost, lifecyclePath("activate"), "", ifm)
	expect(t, w, http.StatusOK, "")
	if g.store.transitionCmd.Command != domain.CommandActivate || g.store.transitionCmd.ActorID != testPrincipal {
		t.Fatalf("expected ACTIVATE by the header principal, got %+v", g.store.transitionCmd)
	}
	if w.Header().Get("ETag") != `"v4"` {
		t.Fatalf("expected the new row_version as ETag, got %q", w.Header().Get("ETag"))
	}
}

func TestActivate_BodyActorIsIgnored(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), `{"activated_by_principal_id":"someone-else"}`, ifm), http.StatusOK, "")
	if g.store.transitionCmd.ActorID != testPrincipal {
		t.Fatalf("the actor must be the header principal, got %q", g.store.transitionCmd.ActorID)
	}
}

func TestActivate_AuthorizedAgainstStoredScope(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), http.StatusOK, "")
	if g.az.actionType != authz.ActionPolicyVersionActivate || g.az.scope != testEntity {
		t.Fatalf("expected POLICY_VERSION_ACTIVATE on the version's own entity, got %s on %s", g.az.actionType, g.az.scope)
	}

	global := &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, VersionStatus: domain.StatusApproved, RowVersion: 3}
	g = newRig(lifecycleStore(global), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), http.StatusOK, "")
	if g.az.actionType != authz.ActionPolicyVersionActivateGlobal || g.az.scope != testAuthzScopeID {
		t.Fatalf("a global version needs the _GLOBAL grant at platform scope, got %s on %s", g.az.actionType, g.az.scope)
	}
}

// Audit row "Maker-checker / SoD on activation" and GOV-05 "production
// activation requires approved artifact".
func TestLifecycle_StoreRefusalsMapToStableClasses(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		detail string
	}{
		{domain.ErrApprovalRequired, http.StatusConflict, "APPROVAL_REQUIRED", "submit the version for review and have an independent approver approve it before activation"},
		{domain.ErrSoDConflict, http.StatusForbidden, "SOD_CONFLICT", "the maker cannot be the checker: the author cannot approve or activate, and the suspender cannot review the suspension"},
		{domain.ErrPolicyConflicted, http.StatusConflict, "POLICY_CONFLICTED", "conflicting policies"},
		{domain.ErrConcurrencyConflict, http.StatusConflict, "CONCURRENCY_CONFLICT", "concurrency conflict"},
		{domain.ErrInvalidTransition, http.StatusConflict, "CONCURRENCY_CONFLICT", "the version's current status does not allow this command"},
		{domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "store_unavailable"},
	}
	for _, tc := range cases {
		s := lifecycleStore(nil)
		s.transitionErr = tc.err
		g := newRig(s, nil, nil)
		body := expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), tc.status, tc.code)
		if body["code"] != tc.code || body["detail"] != tc.detail {
			t.Errorf("%v: expected legacy error %s, got %v", tc.err, tc.detail, body)
		}
	}
}

func TestLifecycle_ExpectedVersionFromBodyOrIfMatch(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("approve"), "", header("If-Match", `"v3"`)), http.StatusOK, "")
	if ev := g.store.transitionCmd.ExpectedVersion; ev == nil || *ev != 3 {
		t.Fatalf("If-Match must become expected_version 3, got %v", ev)
	}

	g = newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("approve"), `{"expected_version":3,"approval_reference":"WF-77"}`, header("If-Match", `"v3"`)), http.StatusOK, "")
	if ev := g.store.transitionCmd.ExpectedVersion; ev == nil || *ev != 3 || g.store.transitionCmd.ApprovalReference != "WF-77" {
		t.Fatalf("expected expected_version 3 and the approval reference, got %+v", g.store.transitionCmd)
	}
	if g.az.actionType != authz.ActionPolicyVersionApprove {
		t.Fatalf("approval needs POLICY_VERSION_APPROVE, got %s", g.az.actionType)
	}

	g = newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("approve"), `{"expected_version":2}`, header("If-Match", `"v3"`)), http.StatusPreconditionFailed, "stale_version")
	expect(t, g.do(http.MethodPost, lifecyclePath("approve"), "", header("If-Match", "abc")), http.StatusBadRequest, "invalid_if_match")
}

// GCP §16: reason_code is required for privileged or destructive commands.
func TestSuspendAndRetire_RequireReasonCode(t *testing.T) {
	for _, cmd := range []string{"suspend", "retire"} {
		g := newRig(lifecycleStore(nil), nil, nil)
		expect(t, g.do(http.MethodPost, lifecyclePath(cmd), "", ifm), http.StatusBadRequest, "INVALID_REQUEST")
		if g.store.transitionCalls != 0 {
			t.Fatalf("%s without reason_code must not reach the store", cmd)
		}
		expect(t, g.do(http.MethodPost, lifecyclePath(cmd), `{"reason_code":"DEFECT","reason":"wrong threshold"}`, ifm), http.StatusOK, "")
		if g.store.transitionCmd.Reason != "DEFECT: wrong threshold" {
			t.Fatalf("expected the recorded reason, got %q", g.store.transitionCmd.Reason)
		}
	}
	g := newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("suspend"), `{"reason_code":"X"}`, ifm), http.StatusOK, "")
	if g.az.actionType != authz.ActionPolicyVersionSuspend {
		t.Fatalf("suspension is privileged: expected POLICY_VERSION_SUSPEND, got %s", g.az.actionType)
	}
}

func TestLifecycle_VersionOfAnotherPolicyIs404(t *testing.T) {
	v := &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: otherPolicyID, VersionStatus: domain.StatusApproved}
	g := newRig(lifecycleStore(v), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), http.StatusNotFound, "NOT_FOUND")
	if g.store.transitionCalls != 0 {
		t.Fatal("mismatched pairing must not transition anything")
	}
}

func TestLifecycle_DeniedNeverReachesStore(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, g.do(http.MethodPost, lifecyclePath("activate"), "", ifm), http.StatusForbidden, "AUTHORIZATION_DENIED")
	if g.store.transitionCalls != 0 {
		t.Fatal("a denied command must not reach the store")
	}
	expect(t, g.do(http.MethodPost, "/v1/policies/x/versions/"+versionID+"/activate", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions/x/activate", ""), http.StatusBadRequest, "INVALID_REQUEST")
}

// ── evaluate ─────────────────────────────────────────────────────────────────

const thresholdPayload = `{"threshold_amount": 5000}`

func activeThreshold(id, pid string, tenant, entity *string, payload string) *domain.ApplicablePolicyVersion {
	activated := fixedNow.Add(-24 * time.Hour)
	return &domain.ApplicablePolicyVersion{
		PolicyVersion: domain.PolicyVersion{
			PolicyVersionID: id, PolicyID: pid, TenantID: tenant, LegalEntityID: entity,
			RulePayload: json.RawMessage(payload), ArtifactDigest: digestOf(payload),
			EffectiveFrom: fixedNow.Add(-48 * time.Hour), VersionStatus: domain.StatusActive,
			ActivatedAt: &activated, VersionNumber: 1,
		},
		PolicyCode: "AP-" + id[:4],
	}
}

func evalBody(extra string) string {
	b := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + testTenant + `","decision_id":"dec-1","action_context":{"amount":7500}`
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

func evalRig(az *stubAuthz, opts ...rigOption) *rig {
	return newRig(&stubStore{}, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{
		activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload),
	}}, az, opts...)
}

// Audit top gap #1: the actor comes from the verified header, never the
// body, and Evaluate is authorized.
func TestEvaluate_ActorFromHeaderAndAuthorized(t *testing.T) {
	g := evalRig(nil)
	w := g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(`"evaluated_by_principal_id":"forged"`))
	body := expect(t, w, http.StatusOK, "")
	if g.store.saved.RequestedByPrincipalID != testPrincipal {
		t.Fatalf("evidence must record the header principal, got %q", g.store.saved.RequestedByPrincipalID)
	}
	if g.az.actionType != authz.ActionPolicyEvaluate || g.az.principal != testPrincipal {
		t.Fatalf("expected POLICY_EVALUATE for the header principal, got %s for %s", g.az.actionType, g.az.principal)
	}
	if body["result"] != "APPROVAL_REQUIRED" || body["evidence_id"] != evidenceID || body["ledger_status"] != "PENDING" {
		t.Fatalf("unexpected response: %v", body)
	}
}

func TestEvaluate_EnforceModeRefusesWithoutPrincipal(t *testing.T) {
	g := evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(""), noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	if g.store.saveCalls != 0 || g.cands.calls != 0 {
		t.Fatal("an unidentified evaluation must not resolve or record anything")
	}
}

func TestEvaluate_DeniedNeverEvaluates(t *testing.T) {
	g := evalRig(&stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusForbidden, "AUTHORIZATION_DENIED")
	if g.store.saveCalls != 0 || g.cands.calls != 0 {
		t.Fatal("a denied evaluation must not resolve or record anything")
	}
}

func TestEvaluate_WarnModeAdmitsButRecordsNoForgedActor(t *testing.T) {
	g := evalRig(&stubAuthz{err: domain.ErrAuthorizationDenied}, warnEvaluate)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(`"evaluated_by_principal_id":"forged"`), noPrincipal), http.StatusOK, "")
	if g.store.saved.RequestedByPrincipalID != "" {
		t.Fatalf("with no verified principal the requester is unknown, never the body's: got %q", g.store.saved.RequestedByPrincipalID)
	}
}

func TestEvaluate_TenantAndEntityAreServerResolved(t *testing.T) {
	g := evalRig(nil)
	body := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + otherTenant + `","decision_id":"d","action_context":{"amount":1}}`
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", body), http.StatusForbidden, "CONTEXT_UNRESOLVED")

	g = evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(`"legal_entity_id":"`+testEntity+`"`), header("X-Legal-Entity-Id", otherEntity)),
		http.StatusBadRequest, "INVALID_REQUEST")

	// The header entity scopes the evaluation when the body omits it.
	g = evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(""), header("X-Legal-Entity-Id", testEntity)), http.StatusOK, "")
	if g.cands.entity == nil || *g.cands.entity != testEntity || g.az.scope != testEntity {
		t.Fatalf("expected the header entity to scope resolution and authorization, got %v / %s", g.cands.entity, g.az.scope)
	}
}

// Audit row "Evaluation is safely repeatable": the same decision_id and input
// reconcile to the stored result; different input is refused.
func TestEvaluate_DecisionIDReconciles(t *testing.T) {
	g := evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusOK, "")
	stored := g.store.saved

	g2 := evalRig(nil)
	g2.store.evaluation = stored
	body := expect(t, g2.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusOK, "")
	if body["replayed"] != true || body["evidence_id"] != evidenceID {
		t.Fatalf("expected the stored result replayed, got %v", body)
	}
	if g2.store.saveCalls != 0 || g2.cands.calls != 0 {
		t.Fatal("a replay must not re-evaluate or record a second decision")
	}

	g3 := evalRig(nil)
	g3.store.evaluation = stored
	other := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + testTenant + `","decision_id":"dec-1","action_context":{"amount":1}}`
	expect(t, g3.do(http.MethodPost, "/v1/policies/evaluate", other), http.StatusConflict, "IDEMPOTENCY_MISMATCH")
}

func TestEvaluate_FingerprintIgnoresFormatting(t *testing.T) {
	g := evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"`+testTenant+`","decision_id":"d","action_context":{"amount":7500,"ref":"x"}}`), http.StatusOK, "")
	first := g.store.saved.InputFingerprint
	g = evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"decision_id":"d","action_context":{ "ref":"x", "amount":7500 },"tenant_id":"`+testTenant+`","policy_type":"APPROVAL_THRESHOLD"}`), http.StatusOK, "")
	if g.store.saved.InputFingerprint != first {
		t.Fatal("key order and whitespace must not change the input fingerprint")
	}
}

// Audit row "No applicable policy: fail closed with a stable error".
func TestEvaluate_NothingGoverns404PolicyNotEffective(t *testing.T) {
	g := newRig(nil, &stubCandidates{}, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusNotFound, "POLICY_NOT_EFFECTIVE")
	if g.store.saveCalls != 0 {
		t.Fatal("nothing evaluated, nothing recorded")
	}
}

// Audit top gap #6: a future-dated version does not decide before its time.
func TestEvaluate_FutureVersionDoesNotDecide(t *testing.T) {
	v := activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload)
	v.EffectiveFrom = fixedNow.Add(time.Hour)
	g := newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{v}}, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusNotFound, "POLICY_NOT_EFFECTIVE")
}

// Audit top gap #7: two policies at the same specificity are CONFLICTED,
// never latest-by-timestamp, and the conflict is evidenced.
func TestEvaluate_TiedPoliciesConflictAndAreEvidenced(t *testing.T) {
	a := activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload)
	b := activeThreshold(otherVersionID, otherPolicyID, strPtr(testTenant), nil, `{"threshold_amount": 100}`)
	later := fixedNow.Add(-time.Minute)
	b.ActivatedAt = &later
	g := newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{a, b}}, nil)
	body := expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusConflict, "POLICY_CONFLICTED")
	ids, _ := body["candidate_version_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("expected both candidates named, got %v", body["candidate_version_ids"])
	}
	if g.store.saveCalls != 0 {
		t.Fatal("a conflicted evaluation must not record a decision")
	}
	if len(g.store.events) != 1 || g.store.events[0].EventType != "policy.applicability.conflicted" {
		t.Fatalf("expected one policy.applicability.conflicted event, got %+v", g.store.events)
	}
}

func TestEvaluate_DigestMismatchFailsClosed(t *testing.T) {
	v := activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload)
	v.RulePayload = json.RawMessage(`{"threshold_amount": 999999}`)
	g := newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{v}}, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusInternalServerError, "EVIDENCE_INTEGRITY_FAILED")
	if g.store.saveCalls != 0 {
		t.Fatal("tampered content must not decide")
	}
}

// Audit top gap #5 / decision C6: no success without durable evidence.
func TestEvaluate_NoSuccessWithoutEvidence(t *testing.T) {
	g := evalRig(nil)
	g.store.saveErr = domain.ErrStoreUnavailable
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
}

func TestEvaluate_EvidenceContent(t *testing.T) {
	g := evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(""), header("Idempotency-Key", "ik")), http.StatusOK, "")
	ev := g.store.saved
	if ev.TenantID != testTenant || ev.Outcome != "ESCALATED" || ev.EvaluatorVersion == "" ||
		!strings.HasPrefix(ev.InputFingerprint, "sha256:") || ev.ArtifactDigest != digestOf(thresholdPayload) ||
		len(ev.SelectedRuleIDs) != 1 || ev.CorrelationID != "corr-1" || !ev.AsOf.Equal(fixedNow) || !ev.KnownAt.Equal(fixedNow) {
		t.Fatalf("incomplete evidence: %+v", ev)
	}
}

func TestEvaluate_ExactDecimalBoundaries(t *testing.T) {
	cases := map[string]string{
		`5000`:                     "WITHIN_THRESHOLD",
		`5000.00`:                  "WITHIN_THRESHOLD",
		`5000.0000000000000000001`: "APPROVAL_REQUIRED",
		`4999.99`:                  "WITHIN_THRESHOLD",
		`"7500"`:                   "",
	}
	for amount, want := range cases {
		g := evalRig(nil)
		body := `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"` + testTenant + `","decision_id":"d","action_context":{"amount":` + amount + `}}`
		if want == "" {
			// A string is not an exact decimal number: refused, never guessed.
			expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", body), http.StatusBadRequest, "INVALID_REQUEST")
			continue
		}
		got := expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", body), http.StatusOK, "")
		if got["result"] != want {
			t.Errorf("amount %s: expected %s, got %v", amount, want, got["result"])
		}
	}
}

func TestEvaluate_InputValidation(t *testing.T) {
	g := evalRig(nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"policy_type":"APPROVAL_THRESHOLD"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"decision_id":"d"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"policy_type":"APPROVAL_THRESHOLD","decision_id":"`+strings.Repeat("d", 65)+`"}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody(`"known_at":"2027-01-01T00:00:00Z"`)), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"policy_type":"APPROVAL_THRESHOLD","decision_id":"d","action_context":[1]}`), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", `{"policy_type":"APPROVAL_THRESHOLD","tenant_id":"`+testTenant+`","decision_id":"d","action_context":{}}`), http.StatusBadRequest, "INVALID_REQUEST")
	if g.store.saveCalls != 0 {
		t.Fatal("invalid input must not be recorded")
	}
}

func TestEvaluate_UnimplementedTypeIs501(t *testing.T) {
	v := activeThreshold(versionID, policyID, strPtr(testTenant), nil, `{"limit":1}`)
	g := newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{v}}, nil)
	body := `{"policy_type":"SPEND_CONTROL","tenant_id":"` + testTenant + `","decision_id":"d","action_context":{}}`
	expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", body), http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

// ── reads ────────────────────────────────────────────────────────────────────

func TestReads_EnforcedByDefault(t *testing.T) {
	paths := []string{
		"/v1/policies?policy_type=APPROVAL_THRESHOLD",
		"/v1/policies/resolve?policy_type=APPROVAL_THRESHOLD",
		"/v1/policies/explain?policy_type=APPROVAL_THRESHOLD",
		"/v1/policies/" + policyID,
		"/v1/policies/" + policyID + "/versions",
		"/v1/policy-versions/" + versionID + "/transitions",
		"/v1/policy-evaluations/" + evidenceID,
	}
	for _, p := range paths {
		g := newRig(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID}}, nil, nil)
		expect(t, g.do(http.MethodGet, p, "", noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
		g = newRig(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID}}, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
		expect(t, g.do(http.MethodGet, p, ""), http.StatusForbidden, "AUTHORIZATION_DENIED")
		if g.az.actionType != authz.ActionPolicyRead {
			t.Fatalf("%s: expected POLICY_READ, got %s", p, g.az.actionType)
		}
		g = newRig(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID}}, nil, nil)
		expect(t, g.do(http.MethodGet, p, "", noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	}
}

func TestReads_WarnModeAdmits(t *testing.T) {
	g := newRig(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID, RowVersion: 2}}, nil, &stubAuthz{err: domain.ErrAuthorizationDenied}, warnReads)
	w := g.do(http.MethodGet, "/v1/policy-versions/"+versionID, "", noPrincipal)
	expect(t, w, http.StatusOK, "")
	if w.Header().Get("ETag") != `"v2"` {
		t.Fatalf("expected ETag, got %q", w.Header().Get("ETag"))
	}
}

func TestGetPolicyVersion_AuthorizedOnItsOwnEntity(t *testing.T) {
	g := newRig(&stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID, LegalEntityID: strPtr(testEntity)}}, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policy-versions/"+versionID, ""), http.StatusOK, "")
	if g.az.scope != testEntity {
		t.Fatalf("expected the version's entity as scope, got %s", g.az.scope)
	}
	expect(t, g.do(http.MethodGet, "/v1/policy-versions/not-a-uuid", ""), http.StatusBadRequest, "INVALID_REQUEST")
}

func TestListApplicable_ForeignTenantRefusedAndSetIsResolved(t *testing.T) {
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policies?policy_type=T&tenant_id="+otherTenant, ""), http.StatusForbidden, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodGet, "/v1/policies", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policies?policy_type=T&as_of=yesterday", ""), http.StatusBadRequest, "INVALID_REQUEST")

	global := activeThreshold(versionID, policyID, nil, nil, thresholdPayload)
	tenantLevel := activeThreshold(otherVersionID, otherPolicyID, strPtr(testTenant), nil, thresholdPayload)
	future := activeThreshold("bbbbbbbb-0000-0000-0000-000000000003", "aaaaaaaa-0000-0000-0000-000000000003", strPtr(testTenant), strPtr(testEntity), thresholdPayload)
	future.EffectiveFrom = fixedNow.Add(time.Hour)
	g = newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{global, tenantLevel, future}}, nil)
	w := g.do(http.MethodGet, "/v1/policies?policy_type=APPROVAL_THRESHOLD&tenant_id="+testTenant+"&legal_entity_id="+testEntity, "")
	expect(t, w, http.StatusOK, "")
	var set []domain.ApplicablePolicyVersion
	_ = json.Unmarshal(w.Body.Bytes(), &set)
	if len(set) != 2 || set[0].PolicyVersionID != otherVersionID || set[1].PolicyVersionID != versionID {
		t.Fatalf("expected [tenant, global] without the future version, got %+v", set)
	}
}

func TestResolveAndExplain(t *testing.T) {
	v := activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload)
	g := newRig(nil, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{v}}, nil)
	body := expect(t, g.do(http.MethodGet, "/v1/policies/resolve?policy_type=APPROVAL_THRESHOLD&tenant_id="+testTenant, ""), http.StatusOK, "")
	if body["status"] != "RESOLVED" || body["trace"] != nil {
		t.Fatalf("expected RESOLVED without a trace, got %v", body)
	}
	body = expect(t, g.do(http.MethodGet, "/v1/policies/explain?policy_type=APPROVAL_THRESHOLD&tenant_id="+testTenant, ""), http.StatusOK, "")
	trace, _ := body["trace"].([]any)
	if len(trace) != 1 || trace[0].(map[string]any)["reason"] != "selected" {
		t.Fatalf("expected a trace naming the selected version, got %v", body["trace"])
	}
	// Historical request: as_of before the version existed resolves nothing.
	body = expect(t, g.do(http.MethodGet, "/v1/policies/resolve?policy_type=APPROVAL_THRESHOLD&tenant_id="+testTenant+"&as_of=2026-01-01T00:00:00Z", ""), http.StatusOK, "")
	if body["status"] != "NOT_APPLICABLE" {
		t.Fatalf("expected NOT_APPLICABLE as of January, got %v", body["status"])
	}
}

func TestCompareVersions(t *testing.T) {
	base := &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RulePayload: json.RawMessage(`{"threshold_amount":5000,"currency":"USD"}`), EffectiveFrom: fixedNow, VersionNumber: 1}
	target := &domain.PolicyVersion{PolicyVersionID: otherVersionID, PolicyID: policyID, RulePayload: json.RawMessage(`{"threshold_amount":7500,"note":"raised"}`), EffectiveFrom: fixedNow, VersionNumber: 2}
	g := newRig(&stubStore{findVersionByID: map[string]*domain.PolicyVersion{versionID: base, otherVersionID: target}}, nil, nil)
	body := expect(t, g.do(http.MethodGet, "/v1/policy-versions/compare?base="+versionID+"&target="+otherVersionID, ""), http.StatusOK, "")
	changes, _ := body["payload_changes"].([]any)
	got := map[string]string{}
	for _, c := range changes {
		m := c.(map[string]any)
		got[m["path"].(string)] = m["change"].(string)
	}
	if got["threshold_amount"] != "changed" || got["currency"] != "removed" || got["note"] != "added" || len(got) != 3 {
		t.Fatalf("unexpected payload diff: %v", got)
	}
	expect(t, g.do(http.MethodGet, "/v1/policy-versions/compare?base="+versionID, ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-versions/compare?base="+versionID+"&target=bbbbbbbb-0000-0000-0000-00000000000f", ""), http.StatusNotFound, "NOT_FOUND")
}

func TestGetEvaluation_ScopedToCallerTenant(t *testing.T) {
	g := newRig(&stubStore{evaluation: &domain.Evaluation{EvaluationID: evidenceID, TenantID: testTenant}}, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policy-evaluations/"+evidenceID, ""), http.StatusOK, "")
	if g.store.findEvalTenant != testTenant {
		t.Fatalf("evidence must be looked up in the caller's tenant, got %q", g.store.findEvalTenant)
	}
	g = newRig(nil, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policy-evaluations/"+evidenceID, ""), http.StatusNotFound, "NOT_FOUND")
}

func TestListVersionHistory(t *testing.T) {
	g := newRig(&stubStore{}, nil, nil)
	w := g.do(http.MethodGet, "/v1/policies/"+policyID+"/versions", "")
	expect(t, w, http.StatusOK, "")
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("an empty history is [], got %s", w.Body.String())
	}
	g = newRig(&stubStore{historyErr: domain.ErrPolicyNotFound}, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policies/"+policyID+"/versions", ""), http.StatusNotFound, "NOT_FOUND")
}

func TestCorrelationIDEchoed(t *testing.T) {
	g := newRig(nil, nil, nil)
	w := g.do(http.MethodGet, "/v1/policies/"+policyID+"/versions", "")
	if w.Header().Get("X-Correlation-ID") != "corr-1" {
		t.Fatalf("expected the correlation id echoed, got %q", w.Header().Get("X-Correlation-ID"))
	}
}

func TestRequestTooLarge413(t *testing.T) {
	g := newRig(nil, nil, nil)
	big := `{"policy_code":"` + strings.Repeat("x", 300<<10) + `"}`
	expect(t, g.do(http.MethodPost, "/v1/policies", big), http.StatusRequestEntityTooLarge, "INVALID_REQUEST")
}

// GOV-05 "Idempotency-Key + expected_version where stateful": a lifecycle
// command that does not name the version it decided on is refused before it
// reaches the store.
func TestLifecycle_ExpectedVersionIsMandatory(t *testing.T) {
	for _, cmd := range []string{"submit-review", "approve", "activate", "schedule", "suspend", "retire", "review-suspension"} {
		g := newRig(lifecycleStore(nil), nil, nil)
		w := g.do(http.MethodPost, lifecyclePath(cmd), `{"reason_code":"X","outcome":"UPHELD"}`)
		if w.Code != http.StatusPreconditionRequired {
			t.Errorf("%s without expected_version: want 428, got %d", cmd, w.Code)
		}
		if g.store.transitionCalls != 0 {
			t.Errorf("%s: a command without expected_version must not reach the store", cmd)
		}
	}
}

// GOV-05 negative path #3: a suspension is reviewed by an approver-class
// principal, with a reason and an outcome.
func TestReviewSuspension_Contract(t *testing.T) {
	g := newRig(lifecycleStore(nil), nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("review-suspension"), `{"outcome":"UPHELD"}`, ifm), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, lifecyclePath("review-suspension"), `{"reason_code":"CHECKED","outcome":"MAYBE"}`, ifm), http.StatusBadRequest, "INVALID_REQUEST")
	if g.store.transitionCalls != 0 {
		t.Fatal("an incomplete review must not reach the store")
	}
	expect(t, g.do(http.MethodPost, lifecyclePath("review-suspension"), `{"reason_code":"CHECKED","outcome":"overturned"}`, ifm), http.StatusOK, "")
	if c := g.store.transitionCmd; c.Command != domain.CommandReviewSuspension || c.Outcome != domain.SuspensionOverturned || c.Reason != "CHECKED" {
		t.Fatalf("unexpected command %+v", c)
	}
	if g.az.actionType != authz.ActionPolicyVersionApprove {
		t.Fatalf("reviewing a suspension is an approver's act, got %s", g.az.actionType)
	}

	s := lifecycleStore(nil)
	s.transitionErr = domain.ErrSoDConflict
	g = newRig(s, nil, nil)
	expect(t, g.do(http.MethodPost, lifecyclePath("review-suspension"), `{"reason_code":"X","outcome":"UPHELD"}`, ifm), http.StatusForbidden, "SOD_CONFLICT")
}
