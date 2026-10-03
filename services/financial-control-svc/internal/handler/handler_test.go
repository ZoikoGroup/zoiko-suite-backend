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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/handler"
	svcmiddleware "zoiko.io/financial-control-svc/internal/middleware"
)

// fakeStore implements handler.Store; only what a test sets is exercised.
type fakeStore struct {
	run       *domain.ControlRun
	created   bool
	runErr    error
	def       *domain.ControlDefinition
	approveEr error
	calls     []string
	w1        wave1State
	w5        wave5State
	w7        wave7State
}

func (f *fakeStore) CreateDefinition(_ context.Context, _, _, _ string, req domain.CreateControlDefinitionRequest) (*domain.ControlDefinition, error) {
	f.calls = append(f.calls, "CreateDefinition")
	return &domain.ControlDefinition{ControlDefinitionID: "d1", ControlCode: req.ControlCode}, nil
}
func (f *fakeStore) GetDefinition(context.Context, string, string) (*domain.ControlDefinition, error) {
	f.calls = append(f.calls, "GetDefinition")
	if f.def == nil {
		return nil, domain.ErrNotFound
	}
	return f.def, nil
}
func (f *fakeStore) ListDefinitions(context.Context, string, int) ([]domain.ControlDefinition, error) {
	return nil, nil
}
func (f *fakeStore) CreateRuleVersion(context.Context, string, string, string, domain.CreateRuleVersionRequest) (*domain.ControlRuleVersion, error) {
	return &domain.ControlRuleVersion{RuleVersion: 2}, nil
}
func (f *fakeStore) ApproveRuleVersion(context.Context, string, string, int, string) (*domain.ControlRuleVersion, error) {
	f.calls = append(f.calls, "ApproveRuleVersion")
	return &domain.ControlRuleVersion{RuleVersion: 1}, f.approveEr
}
func (f *fakeStore) ListRuleVersions(context.Context, string, string) ([]domain.ControlRuleVersion, error) {
	return nil, nil
}
func (f *fakeStore) CreateTolerancePolicy(context.Context, string, string, domain.CreateTolerancePolicyRequest) (*domain.TolerancePolicy, error) {
	f.calls = append(f.calls, "CreateTolerancePolicy")
	return &domain.TolerancePolicy{ToleranceVersion: 1}, nil
}
func (f *fakeStore) ListTolerancePolicies(context.Context, string, string, string) ([]domain.TolerancePolicy, error) {
	return nil, nil
}
func (f *fakeStore) CreateMaterialityPolicy(context.Context, string, string, domain.CreateMaterialityPolicyRequest) (*domain.MaterialityPolicy, error) {
	return &domain.MaterialityPolicy{MaterialityVersion: 1}, nil
}
func (f *fakeStore) CreateRun(_ context.Context, _, _, _, _ string, _ domain.CreateRunRequest) (*domain.ControlRun, bool, error) {
	f.calls = append(f.calls, "CreateRun")
	return f.run, f.created, f.runErr
}
func (f *fakeStore) GetRun(context.Context, string, string) (*domain.ControlRun, error) {
	f.calls = append(f.calls, "GetRun")
	if f.run == nil {
		return nil, domain.ErrNotFound
	}
	c := *f.run
	return &c, nil
}
func (f *fakeStore) ListRuns(context.Context, string, domain.ListRunsFilter) ([]domain.ControlRun, error) {
	return nil, nil
}
func (f *fakeStore) ListTransitions(context.Context, string, string) ([]domain.Transition, error) {
	return nil, nil
}

type fakeAuthz struct {
	err     error
	actions []string
	entity  []string
}

func (a *fakeAuthz) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.actions = append(a.actions, action)
	a.entity = append(a.entity, entity)
	return a.err
}

func newServer(s *fakeStore, a *fakeAuthz) http.Handler {
	return newServerExec(s, a, &fakeExecutor{})
}

func newServerExec(s *fakeStore, a *fakeAuthz, ex *fakeExecutor) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, a, ex, zap.NewNop()))
	return r
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

var auth = map[string]string{"X-Tenant-Id": "11111111-1111-1111-1111-111111111111", "X-Principal-Id": "alice"}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range auth {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

const runBody = `{"control_definition_id":"d1","legal_entity_id":"e1","trigger_type":"DAILY"}`

func sampleRun() *domain.ControlRun {
	return &domain.ControlRun{RunID: "r1", LegalEntityID: "e1", ControlDefinitionID: "d1", Version: 3,
		LifecycleState: domain.LifecycleScheduled, ResultState: domain.ResultNotEvaluated,
		CertificationState: domain.CertNotRequired, CreatedAt: time.Now()}
}

func TestMissingTenantOrPrincipalIsUnauthorized(t *testing.T) {
	h := newServer(&fakeStore{}, &fakeAuthz{})
	assert.Equal(t, 401, do(h, "GET", "/controls/v1/definitions/d1", "", map[string]string{"X-Principal-Id": "a"}).Code)
	assert.Equal(t, 401, do(h, "GET", "/controls/v1/definitions/d1", "", map[string]string{"X-Tenant-Id": "t"}).Code)
}

func TestAuthorizationDeniedIs403_AndStoreIsNeverTouched(t *testing.T) {
	s, a := &fakeStore{run: sampleRun(), created: true}, &fakeAuthz{err: domain.ErrAuthorizationDenied}
	rr := do(newServer(s, a), "POST", "/controls/v1/runs", runBody, with(map[string]string{"Idempotency-Key": "k"}))
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s.calls, "CreateRun", "a denied caller must not reach the store")
}

// Invariant/§25: an unreachable authorization-svc rejects the action.
func TestAuthorizationUnavailableFailsClosed(t *testing.T) {
	s, a := &fakeStore{run: sampleRun(), created: true}, &fakeAuthz{err: domain.ErrAuthorizationServiceUnavailable}
	rr := do(newServer(s, a), "POST", "/controls/v1/runs", runBody, with(map[string]string{"Idempotency-Key": "k"}))
	assert.Equal(t, 503, rr.Code)
	assert.Empty(t, s.calls)
}

func TestCreateRun_RequiresIdempotencyKey(t *testing.T) {
	s := &fakeStore{run: sampleRun(), created: true}
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/runs", runBody, auth)
	assert.Equal(t, 400, rr.Code)
	assert.Contains(t, rr.Body.String(), "missing_idempotency_key")
	assert.Empty(t, s.calls)
}

func TestCreateRun_CreatedThenReplay(t *testing.T) {
	s := &fakeStore{run: sampleRun(), created: true}
	h := newServer(s, &fakeAuthz{})
	rr := do(h, "POST", "/controls/v1/runs", runBody, with(map[string]string{"Idempotency-Key": "k"}))
	require.Equal(t, 201, rr.Code)
	assert.Equal(t, `"3"`, rr.Header().Get("ETag"))

	s.created = false
	rr = do(h, "POST", "/controls/v1/runs", runBody, with(map[string]string{"Idempotency-Key": "k"}))
	assert.Equal(t, 200, rr.Code, "replay returns the original run, not a second one")
	assert.Equal(t, "true", rr.Header().Get("Idempotent-Replay"))
}

func TestCreateRun_AuthorizedAgainstBodyEntity(t *testing.T) {
	a := &fakeAuthz{}
	do(newServer(&fakeStore{run: sampleRun(), created: true}, a), "POST", "/controls/v1/runs", runBody, with(map[string]string{"Idempotency-Key": "k"}))
	assert.Equal(t, []string{"FINCTRL_RUN_CREATE"}, a.actions)
	assert.Equal(t, []string{"e1"}, a.entity)
}

// Scenario 04: the caller can never supply tolerance VALUES for a run.
func TestCreateRun_RejectsCallerSuppliedToleranceValues(t *testing.T) {
	body := `{"control_definition_id":"d1","legal_entity_id":"e1","trigger_type":"DAILY","absolute_tolerance":"9999"}`
	s := &fakeStore{run: sampleRun(), created: true}
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/runs", body, with(map[string]string{"Idempotency-Key": "k"}))
	assert.Equal(t, 400, rr.Code, "unknown fields are refused, not silently ignored")
	assert.Empty(t, s.calls)
}

func TestCreateRun_ValidationAndErrorMapping(t *testing.T) {
	key := with(map[string]string{"Idempotency-Key": "k"})
	h := func(s *fakeStore) http.Handler { return newServer(s, &fakeAuthz{}) }

	assert.Equal(t, 400, do(h(&fakeStore{}), "POST", "/controls/v1/runs",
		`{"control_definition_id":"d1","legal_entity_id":"e1","trigger_type":"ON_DEMAND"}`, key).Code, "on-demand needs a reason")

	for err, want := range map[error]int{
		domain.ErrNotFound:          404,
		domain.ErrConflict:          409,
		domain.ErrInvalidArgument:   400,
		domain.ErrInvalidTransition: 422,
		context.DeadlineExceeded:    503,
	} {
		rr := do(h(&fakeStore{runErr: err}), "POST", "/controls/v1/runs", runBody, key)
		assert.Equal(t, want, rr.Code, "store error %v", err)
	}
}

func TestGetRun_AuthorizesAgainstRunsOwnEntity_AndSetsETag(t *testing.T) {
	a := &fakeAuthz{}
	// The client tries to assert a different entity in the header; the run's own entity decides.
	rr := do(newServer(&fakeStore{run: sampleRun()}, a), "GET", "/controls/v1/runs/r1", "", with(map[string]string{"X-Legal-Entity-Id": "spoofed"}))
	require.Equal(t, 200, rr.Code)
	assert.Equal(t, []string{"e1"}, a.entity)
	assert.Equal(t, `"3"`, rr.Header().Get("ETag"))
}

func TestGetRun_NotFoundAnd403BeforeData(t *testing.T) {
	assert.Equal(t, 404, do(newServer(&fakeStore{}, &fakeAuthz{}), "GET", "/controls/v1/runs/nope", "", auth).Code)
	rr := do(newServer(&fakeStore{run: sampleRun()}, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "GET", "/controls/v1/runs/r1", "", auth)
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, rr.Body.String(), "lifecycle_state", "no run data leaks to a denied caller")
}

// §7: derived attention signals never overwrite authoritative state.
func TestGetRun_AttentionSignalsAreDerivedNotStored(t *testing.T) {
	r := sampleRun()
	r.LifecycleState, r.ResultState = domain.LifecycleFailed, domain.ResultIndeterminate
	rr := do(newServer(&fakeStore{run: r}, &fakeAuthz{}), "GET", "/controls/v1/runs/r1", "", auth)
	var got domain.ControlRun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.ElementsMatch(t, []string{"CONTROL_FAILED", "RESULT_INDETERMINATE"}, got.Attention)
	assert.Equal(t, domain.LifecycleFailed, got.LifecycleState)
	assert.Equal(t, domain.ResultIndeterminate, got.ResultState)
}

func TestListRuns_RequiresEntityAndValidCursor(t *testing.T) {
	h := newServer(&fakeStore{}, &fakeAuthz{})
	assert.Equal(t, 400, do(h, "GET", "/controls/v1/runs", "", auth).Code)
	assert.Equal(t, 400, do(h, "GET", "/controls/v1/runs?legal_entity_id=e1&cursor=!!!", "", auth).Code)
	assert.Equal(t, 200, do(h, "GET", "/controls/v1/runs?legal_entity_id=e1", "", auth).Code)
}

func TestApproveRuleVersion_SelfApprovalIs403(t *testing.T) {
	s := &fakeStore{approveEr: domain.ErrSelfApproval}
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/definitions/d1/rule-versions/1/approve", "", auth)
	assert.Equal(t, 403, rr.Code)
	assert.Contains(t, rr.Body.String(), "self_approval")
	assert.Equal(t, 400, do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/definitions/d1/rule-versions/zero/approve", "", auth).Code)
}

func TestApproveRuleVersion_ChecksApprovalAuthority(t *testing.T) {
	a := &fakeAuthz{}
	do(newServer(&fakeStore{}, a), "POST", "/controls/v1/definitions/d1/rule-versions/1/approve", "", auth)
	assert.Equal(t, []string{"FINCTRL_APPROVE_RULE"}, a.actions, "authority re-evaluated at approval time (Invariant 11)")
}

func TestCreateTolerance_SelfApprovalRefusedBeforeStore(t *testing.T) {
	body := `{"legal_entity_id":"e1","metric":"FIN-CTRL-001","absolute_tolerance":"0.01","currency":"USD",
		"rationale":"r","effective_from":"2026-09-01","approved_by":"alice"}`
	s := &fakeStore{}
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/tolerance-policies", body, auth)
	assert.Equal(t, 403, rr.Code)
	assert.Empty(t, s.calls)

	body = strings.Replace(body, `"approved_by":"alice"`, `"approved_by":"bob"`, 1)
	assert.Equal(t, 201, do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/tolerance-policies", body, auth).Code)
}

func TestCreateDefinition_ValidatesInvariantOne(t *testing.T) {
	s := &fakeStore{}
	assert.Equal(t, 400, do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/definitions", `{"control_code":"FIN-CTRL-001"}`, auth).Code)
	assert.Empty(t, s.calls)
	good := `{"control_code":"FIN-CTRL-001","name":"n","domain":"AR","control_type":"BALANCE","assertions":["COMPLETENESS"],
		"risk_tier":"STANDARD","frequency":"DAILY","owner_role":"OWNER","source_spec":{"a":1},"target_spec":{"b":1},
		"evidence_policy":{"c":1},"initial_logic":{"kind":"MATCH"},"initial_effective_from":"2026-09-01"}`
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/definitions", good, auth)
	assert.Equal(t, 201, rr.Code, rr.Body.String())
}
