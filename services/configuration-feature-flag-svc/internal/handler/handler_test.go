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
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/authz"
	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/handler"
	svcmiddleware "zoiko.io/configuration-feature-flag-svc/internal/middleware"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
)

// ── stub store ────────────────────────────────────────────────────────────────

// stubStore implements handler.ConfigStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	configEntry        *domain.ConfigEntry
	configEntryCreated bool
	configEntryErr     error
	gotUpsertConfig    domain.UpsertConfigEntryParams

	findConfigResult *domain.ConfigEntry
	findConfigErr    error

	listConfigResult    []*domain.ConfigEntry
	listConfigErr       error
	gotListConfigFilter store.ListFilter

	flag          *domain.FeatureFlag
	flagCreated   bool
	flagErr       error
	gotUpsertFlag domain.UpsertFeatureFlagParams

	findFlagResult *domain.FeatureFlag
	findFlagErr    error

	listFlagResult    []*domain.FeatureFlag
	listFlagErr       error
	gotListFlagFilter store.ListFilter

	// AA-001 governed surface.
	gotCreateDefinition domain.CreateDefinitionParams
	definition          *domain.ConfigDefinition
	definitionErr       error

	getDefinitionResult *domain.ConfigDefinition
	getDefinitionErr    error

	gotPublishDefinition domain.PublishDefinitionParams
	publishVersion       *domain.ConfigDefinitionVersion
	publishErr           error

	gotResolve    domain.ResolveParams
	resolveResult *domain.ResolvedConfigSnapshot
	resolveErr    error

	gotOverride   domain.ActivateOverrideParams
	overrideEntry *domain.ConfigEntry
	overrideErr   error

	gotCreateChange domain.CreateChangeParams
	change          *domain.ConfigChange
	changeErr       error

	approveChangeID string
	gotApproval     domain.ChangeApproval
	approvedChange  *domain.ConfigChange
	approveErr      error

	activateChangeID string
	activatedChange  *domain.ConfigChange
	activateErr      error

	gotEmergency domain.CreateEmergencyChangeParams
	emergency    *domain.EmergencyChange
	emergencyErr error

	activateEmergencyID  string
	activatedEmergency   *domain.EmergencyChange
	activateEmergencyErr error

	gotAttestation domain.RecordAttestationParams
	attestation    *domain.RuntimeAttestation
	attestationErr error

	gotReleasePlan domain.CreateReleasePlanParams
	releasePlan    *domain.ReleasePlan
	releasePlanErr error

	gotEvaluate domain.EvaluateFlagParams
	evaluation  *domain.FlagEvaluation
	evaluateErr error
}

func (s *stubStore) CreateDefinition(_ context.Context, params domain.CreateDefinitionParams) (*domain.ConfigDefinition, error) {
	s.gotCreateDefinition = params
	return s.definition, s.definitionErr
}

func (s *stubStore) GetDefinition(_ context.Context, _ string) (*domain.ConfigDefinition, error) {
	return s.getDefinitionResult, s.getDefinitionErr
}

func (s *stubStore) PublishDefinition(_ context.Context, params domain.PublishDefinitionParams) (*domain.ConfigDefinitionVersion, error) {
	s.gotPublishDefinition = params
	return s.publishVersion, s.publishErr
}

func (s *stubStore) Resolve(_ context.Context, params domain.ResolveParams) (*domain.ResolvedConfigSnapshot, error) {
	s.gotResolve = params
	return s.resolveResult, s.resolveErr
}

func (s *stubStore) ActivateOverride(_ context.Context, params domain.ActivateOverrideParams) (*domain.ConfigEntry, error) {
	s.gotOverride = params
	return s.overrideEntry, s.overrideErr
}

func (s *stubStore) CreateChange(_ context.Context, params domain.CreateChangeParams) (*domain.ConfigChange, error) {
	s.gotCreateChange = params
	return s.change, s.changeErr
}

func (s *stubStore) ApproveChange(_ context.Context, changeID string, approval domain.ChangeApproval, _ string) (*domain.ConfigChange, error) {
	s.approveChangeID = changeID
	s.gotApproval = approval
	return s.approvedChange, s.approveErr
}

func (s *stubStore) ActivateChange(_ context.Context, changeID, _, _ string) (*domain.ConfigChange, error) {
	s.activateChangeID = changeID
	return s.activatedChange, s.activateErr
}

func (s *stubStore) CreateEmergencyChange(_ context.Context, params domain.CreateEmergencyChangeParams) (*domain.EmergencyChange, error) {
	s.gotEmergency = params
	return s.emergency, s.emergencyErr
}

func (s *stubStore) ActivateEmergencyChange(_ context.Context, emergencyChangeID, _, _ string) (*domain.EmergencyChange, error) {
	s.activateEmergencyID = emergencyChangeID
	return s.activatedEmergency, s.activateEmergencyErr
}

func (s *stubStore) RecordAttestation(_ context.Context, params domain.RecordAttestationParams) (*domain.RuntimeAttestation, error) {
	s.gotAttestation = params
	return s.attestation, s.attestationErr
}

func (s *stubStore) CreateReleasePlan(_ context.Context, params domain.CreateReleasePlanParams) (*domain.ReleasePlan, error) {
	s.gotReleasePlan = params
	return s.releasePlan, s.releasePlanErr
}

func (s *stubStore) EvaluateFlag(_ context.Context, params domain.EvaluateFlagParams) (*domain.FlagEvaluation, error) {
	s.gotEvaluate = params
	return s.evaluation, s.evaluateErr
}

func (s *stubStore) UpsertConfigEntry(_ context.Context, params domain.UpsertConfigEntryParams) (*domain.ConfigEntry, bool, error) {
	s.gotUpsertConfig = params
	return s.configEntry, s.configEntryCreated, s.configEntryErr
}

func (s *stubStore) FindCurrentConfigEntry(_ context.Context, _, _ string, _ *string) (*domain.ConfigEntry, error) {
	return s.findConfigResult, s.findConfigErr
}

func (s *stubStore) ListCurrentConfigEntries(_ context.Context, filter store.ListFilter) ([]*domain.ConfigEntry, error) {
	s.gotListConfigFilter = filter
	return s.listConfigResult, s.listConfigErr
}

func (s *stubStore) UpsertFeatureFlag(_ context.Context, params domain.UpsertFeatureFlagParams) (*domain.FeatureFlag, bool, error) {
	s.gotUpsertFlag = params
	return s.flag, s.flagCreated, s.flagErr
}

func (s *stubStore) FindCurrentFeatureFlag(_ context.Context, _, _ string, _ *string) (*domain.FeatureFlag, error) {
	return s.findFlagResult, s.findFlagErr
}

func (s *stubStore) ListCurrentFeatureFlags(_ context.Context, filter store.ListFilter) ([]*domain.FeatureFlag, error) {
	s.gotListFlagFilter = filter
	return s.listFlagResult, s.listFlagErr
}

// newTestRouter mounts TenantContext, which cmd/server/main.go mounts in front
// of these same routes. This service had no tenant middleware at all until the
// scope was closed, so nothing here ever exercised a scoped request.
func newTestRouter(s *stubStore) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, testAuthz(), testAuthzScopeID, testMetrics(), zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// testMetrics gives every router its own registry.
//
// prometheus.MustRegister panics on a duplicate collector name, so the default
// registry cannot be shared across tests — and a test that did share one would
// be asserting on whatever ran before it.
func testMetrics() *telemetry.Domain {
	return telemetry.NewDomainWith(telemetry.NewRegistry(), "configuration-feature-flag-svc")
}

// ── POST /v1/config ──────────────────────────────────────────────────────────

func TestUpsertConfigEntry_Created(t *testing.T) {
	store := &stubStore{
		configEntry: &domain.ConfigEntry{
			ConfigID:    "cfg-1",
			Key:         "payroll.batch_size",
			Value:       []byte(`100`),
			Environment: "staging",
		},
		configEntryCreated: true,
	}
	r := newTestRouter(store)

	body := `{"key":"payroll.batch_size","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	// config.updated is no longer published from the handler at all: the store
	// enqueues it in the transaction that wrote the row. What this asserts is
	// the trigger — a real transition answers 201, and the store enqueues on
	// exactly that branch. See store.TestUpsert_EnqueuesOnlyOnRealTransition.
	var got domain.ConfigEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Key != "payroll.batch_size" {
		t.Errorf("expected key payroll.batch_size, got %s", got.Key)
	}
}

func TestUpsertConfigEntry_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{
		configEntry:        &domain.ConfigEntry{ConfigID: "cfg-1", Key: "k"},
		configEntryCreated: false,
	}
	r := newTestRouter(store)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent no-op, got %d: %s", w.Code, w.Body.String())
	}
	// 200, so the store took the no-op branch and enqueued nothing. Asserted
	// against a real database in store.TestUpsert_EnqueuesOnlyOnRealTransition.
}

func TestUpsertConfigEntry_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"key":"k"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_InvalidJSON(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(`{not json`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertConfigEntry_StoreUnavailable(t *testing.T) {
	store := &stubStore{configEntryErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// A failure to RECORD the event must now fail the write, which is the exact
// inversion this service needed. It used to publish after the commit and log
// the error, so a broker hiccup answered 201 with the change recorded and every
// consumer still reading the superseded value — valid data, so undetectable
// downstream. The enqueue shares the write's transaction, so the store reports
// it as a store failure and nothing is committed.
func TestUpsertConfigEntry_EnqueueFailureFailsTheWrite(t *testing.T) {
	store := &stubStore{configEntryErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	body := `{"key":"k","value":100,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the event cannot be recorded, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GET /v1/config/{key} ─────────────────────────────────────────────────────

func TestGetConfigEntry_Found(t *testing.T) {
	store := &stubStore{findConfigResult: &domain.ConfigEntry{ConfigID: "cfg-1", Key: "k", Value: []byte(`100`)}}
	r := newTestRouter(store)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConfigEntry_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetConfigEntry_NotFound(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrConfigEntryNotFound}
	r := newTestRouter(store)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/missing?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetConfigEntry_StoreUnavailable(t *testing.T) {
	store := &stubStore{findConfigErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/k?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/config ───────────────────────────────────────────────────────────

func TestListConfigEntries_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigResult: nil})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListConfigEntries_FiltersForwarded(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?environment=staging&tenant_id="+testTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if s.gotListConfigFilter.Environment != "staging" {
		t.Errorf("expected environment=staging forwarded, got %q", s.gotListConfigFilter.Environment)
	}
	if s.gotListConfigFilter.TenantID == nil || *s.gotListConfigFilter.TenantID != testTenant {
		t.Errorf("expected the verified tenant %s forwarded, got %v", testTenant, s.gotListConfigFilter.TenantID)
	}
}

func TestListConfigEntries_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{listConfigErr: domain.ErrStoreUnavailable})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── POST /v1/flags ───────────────────────────────────────────────────────────

func TestUpsertFeatureFlag_Created(t *testing.T) {
	store := &stubStore{
		flag:        &domain.FeatureFlag{FlagID: "flag-1", Key: "new_ui", Enabled: true, RolloutPercentage: 100},
		flagCreated: true,
	}
	r := newTestRouter(store)

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	// See the note on the config equivalent: the event is enqueued by the
	// store, on exactly the branch that answers 201.
	if store.gotUpsertFlag.RolloutPercentage != 100 {
		t.Errorf("expected rollout_percentage to default to 100, got %d", store.gotUpsertFlag.RolloutPercentage)
	}
}

func TestUpsertFeatureFlag_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{flag: &domain.FeatureFlag{FlagID: "flag-1"}, flagCreated: false}
	r := newTestRouter(store)

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// 200 — the no-op branch, which enqueues nothing.
}

func TestUpsertFeatureFlag_MissingEnabled(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"key":"new_ui","environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpsertFeatureFlag_ExplicitFalseIsNotMissing(t *testing.T) {
	store := &stubStore{flag: &domain.FeatureFlag{FlagID: "flag-1"}, flagCreated: true}
	r := newTestRouter(store)

	body := `{"key":"new_ui","enabled":false,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for explicit enabled=false, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotUpsertFlag.Enabled != false {
		t.Errorf("expected Enabled=false forwarded, got %v", store.gotUpsertFlag.Enabled)
	}
}

func TestUpsertFeatureFlag_RolloutPercentageOutOfRange(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","rollout_percentage":150,"created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_NegativeRolloutPercentageRejected(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"key":"new_ui","enabled":true,"environment":"staging","rollout_percentage":-1,"created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_StoreUnavailable(t *testing.T) {
	store := &stubStore{flagErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	body := `{"key":"new_ui","enabled":true,"environment":"staging","created_by_principal_id":"admin-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GET /v1/flags/{key} ──────────────────────────────────────────────────────

func TestGetFeatureFlag_Found(t *testing.T) {
	store := &stubStore{findFlagResult: &domain.FeatureFlag{FlagID: "flag-1", Key: "new_ui"}}
	r := newTestRouter(store)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetFeatureFlag_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/new_ui", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetFeatureFlag_NotFound(t *testing.T) {
	store := &stubStore{findFlagErr: domain.ErrFeatureFlagNotFound}
	r := newTestRouter(store)

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags/missing?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── GET /v1/flags ────────────────────────────────────────────────────────────

func TestListFeatureFlags_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{listFlagResult: nil})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListFeatureFlags_StoreUnavailable(t *testing.T) {
	r := newTestRouter(&stubStore{listFlagErr: domain.ErrStoreUnavailable})

	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── authorization test scaffolding ───────────────────────────────────────────

// testAuthzScopeID stands in for config.AuthZPlatformScopeID.
const testAuthzScopeID = "00000000-0000-0000-0000-0000000000f3"

// testPrincipal is what the gateway ForwardAuth middleware sets in
// X-Principal-Id after verifying the caller identity envelope.
const testPrincipal = "principal-test-admin"

// testTenant is the caller's verified tenant scope; tenant_id is a uuid column.
const testTenant = "11111111-1111-1111-1111-111111111111"

// otherTenant is a tenant the caller has no scope in.
const otherTenant = "22222222-2222-2222-2222-222222222222"

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

// testAuthz is the permit-all default used by every pre-existing test.
func testAuthz() *stubAuthz { return &stubAuthz{} }

// authed stamps the gateway-verified principal header onto a request.
// Every mutating route now requires it.
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

// ── authorization contract ───────────────────────────────────────────────────

// gatedRoutes is every route that mutates state, so a route added later
// cannot quietly skip the gate. The AA-001 governed surface is here as well:
// every one of those writes is a mutation, and each answers the gate tests at
// the exact same checkpoints. The POST reads (resolve, evaluate) are not here
// — they mutate nothing and authorize nothing.
var gatedRoutes = []struct {
	name   string
	method string
	path   string
	body   string
}{
	// VALID bodies, which these fixtures were not.
	//
	// They named config_key/flag_key/updated_by_principal_id — fields this
	// service has never accepted — and passed anyway, because the authorization
	// check used to run before the body was parsed, so the payload was never
	// looked at. The scope being written now chooses which action to authorize,
	// so the body is decoded first and these had to become real.
	{name: "upsert config", method: http.MethodPost, path: "/v1/config", body: `{"key":"k","value":"v","environment":"production","tenant_id":"` + testTenant + `","created_by_principal_id":"a"}`},
	{name: "upsert flag", method: http.MethodPost, path: "/v1/flags", body: `{"key":"f","environment":"production","enabled":true,"tenant_id":"` + testTenant + `","created_by_principal_id":"a"}`},

	// AA-001 governed surface.
	{name: "create definition", method: http.MethodPost, path: "/v1/config/definitions", body: `{"key":"k","owner":"payroll-svc","value_type":"INTEGER","safety_class":"S2","allowed_scopes":["ENVIRONMENT","TENANT"],"fallback_policy":"SAFE_DEFAULT","sensitivity":"INTERNAL"}`},
	{name: "publish definition", method: http.MethodPost, path: "/v1/config/definitions/k/publish", body: `{"lifecycle":"PUBLISHED"}`},
	{name: "activate override", method: http.MethodPut, path: "/v1/config/overrides/environment", body: `{"key":"k","environment":"production","value":5}`},
	{name: "create change", method: http.MethodPost, path: "/v1/config/changes", body: `{"change_class":"C2","environment":"production","tenant_id":"` + testTenant + `","parts":[{"kind":"config","key":"k","scope":{"environment":"production","tenant_id":"` + testTenant + `"},"new_value":10}]}`},
	{name: "approve change", method: http.MethodPost, path: "/v1/config/changes/change-1/approve", body: `{"approved":true}`},
	{name: "activate change", method: http.MethodPost, path: "/v1/config/changes/change-1/activate", body: `{}`},
	{name: "create emergency change", method: http.MethodPost, path: "/v1/emergency-changes", body: `{"key":"k","environment":"production","new_value":10,"reason":"incident","incident_id":"inc-1","expires_at":"2026-09-26T00:00:00Z"}`},
	{name: "activate emergency change", method: http.MethodPost, path: "/v1/emergency-changes/ec-1/activate", body: `{}`},
	{name: "record attestation", method: http.MethodPost, path: "/v1/runtime/attest", body: `{"runtime_id":"rt-1","attest_key":"ak-1","environment":"production","observed_digest":"d"}`},
	{name: "create release plan", method: http.MethodPost, path: "/v1/flags/new_ui/release-plans", body: `{"environment":"production","strategy":"ALL_OR_NOTHING"}`},
}

// TestGatedRoutes_401_WithoutPrincipal — this service shipped with no gate of
// any kind, so anything able to reach the port could write to it.
func TestGatedRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			az := &stubAuthz{}
			r := chi.NewRouter()
			// TenantContext, as main.go mounts it. The scope being written
			// decides which action is authorized — an environment-wide default
			// is a different grant from one organisation's value — so the
			// tenant has to be resolved before the authorization call, and a
			// router without this middleware refuses at 401 before authz is
			// ever consulted.
			r.Use(svcmiddleware.TenantContext())
			handler.RegisterRoutes(r, handler.New(store, az, testAuthzScopeID, testMetrics(), zap.NewNop()))

			// Deliberately NOT wrapped in authed().
			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(route.body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a principal, got %d: %s", w.Code, w.Body.String())
			}
			if az.calls != 0 {
				t.Error("authorization was consulted before the caller was even identified")
			}
		})
	}
}

// TestGatedRoutes_403_Denied — a denial must stop the write.
func TestGatedRoutes_403_Denied(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			az := &stubAuthz{err: authz.ErrDenied}
			r := chi.NewRouter()
			// TenantContext, as main.go mounts it. The scope being written
			// decides which action is authorized — an environment-wide default
			// is a different grant from one organisation's value — so the
			// tenant has to be resolved before the authorization call, and a
			// router without this middleware refuses at 401 before authz is
			// ever consulted.
			r.Use(svcmiddleware.TenantContext())
			handler.RegisterRoutes(r, handler.New(store, az, testAuthzScopeID, testMetrics(), zap.NewNop()))

			req := authed(httptest.NewRequest(route.method, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
			if az.principal != testPrincipal {
				t.Errorf("authz saw principal %q, want %q", az.principal, testPrincipal)
			}
			if az.scope == "" {
				t.Error("an empty legal_entity_id would be rejected by authorization-svc with 400")
			}
		})
	}
}

// TestGatedRoutes_503_AuthzUnavailableFailsClosed — an unreachable
// authorization service must block the mutation, not wave it through.
func TestGatedRoutes_503_AuthzUnavailableFailsClosed(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			az := &stubAuthz{err: authz.ErrUnavailable}
			r := chi.NewRouter()
			// TenantContext, as main.go mounts it. The scope being written
			// decides which action is authorized — an environment-wide default
			// is a different grant from one organisation's value — so the
			// tenant has to be resolved before the authorization call, and a
			// router without this middleware refuses at 401 before authz is
			// ever consulted.
			r.Use(svcmiddleware.TenantContext())
			handler.RegisterRoutes(r, handler.New(store, az, testAuthzScopeID, testMetrics(), zap.NewNop()))

			req := authed(httptest.NewRequest(route.method, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────
//
// This service read no tenant header at all before these tests existed: a
// tenant_id in a body chose whose configuration to overwrite, and an ABSENT
// ?tenant_id= on a list route was documented as "entries across all tenants".

func TestListConfigEntries_NoTenantScope_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
	}
}

// The list used to be unfiltered when ?tenant_id= was omitted. It is now the
// caller's own tenant plus the global defaults that apply to it.
func TestListConfigEntries_ScopedToVerifiedTenantPlusGlobal(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?environment=staging", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotListConfigFilter.TenantID == nil || *s.gotListConfigFilter.TenantID != testTenant {
		t.Fatalf("expected the list scoped to the verified tenant %s, got %v", testTenant, s.gotListConfigFilter.TenantID)
	}
	if !s.gotListConfigFilter.IncludeGlobal {
		t.Fatal("expected global defaults included — they apply to this tenant too")
	}
}

func TestListConfigEntries_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config?tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's configuration, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListFeatureFlags_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/flags?tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's flags, got %d: %s", w.Code, w.Body.String())
	}
}

// Configuration is what other services read to decide how to behave, so a
// cross-tenant write here changes another tenant's runtime behaviour.
func TestUpsertConfigEntry_ForeignTenantBody_Refused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	body := `{"key":"k","value":"v","environment":"staging","tenant_id":"` + otherTenant + `","created_by_principal_id":"` + testPrincipal + `"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 writing into another tenant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpsertFeatureFlag_ForeignTenantBody_Refused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	body := `{"key":"new_ui","enabled":true,"environment":"staging","tenant_id":"` + otherTenant + `","created_by_principal_id":"` + testPrincipal + `"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 flipping another tenant's flag, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetConfigEntry_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodGet,
		"/v1/config/k?environment=staging&tenant_id="+otherTenant, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 reading another tenant's config entry, got %d: %s", w.Code, w.Body.String())
	}
}

// ── AA-001 governed surface ──────────────────────────────────────────────────

func TestCreateConfigDefinition_Success(t *testing.T) {
	s := &stubStore{definition: &domain.ConfigDefinition{DefinitionID: "def-1", Key: "k"}}
	r := newTestRouter(s)
	body := `{"key":"k","owner":"payroll-svc","value_type":"INTEGER","safety_class":"S2","allowed_scopes":["ENVIRONMENT","TENANT"],"fallback_policy":"SAFE_DEFAULT","sensitivity":"INTERNAL"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/definitions", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotCreateDefinition.ActorPrincipalID != testPrincipal {
		t.Errorf("expected the verified principal recorded as author, got %q", s.gotCreateDefinition.ActorPrincipalID)
	}
	if s.gotCreateDefinition.EffectiveModel != domain.EffectiveModelImmediate {
		t.Errorf("expected effective_model default IMMEDIATE, got %q", s.gotCreateDefinition.EffectiveModel)
	}
}

func TestCreateConfigDefinition_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/definitions", strings.NewReader(`{"key":"k"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestFindConfigDefinition_NotFound(t *testing.T) {
	s := &stubStore{getDefinitionErr: domain.ErrKeyNotRegistered}
	r := newTestRouter(s)
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/definitions/nope", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFindConfigDefinition_Found(t *testing.T) {
	s := &stubStore{getDefinitionResult: &domain.ConfigDefinition{DefinitionID: "def-1", Key: "k"}}
	r := newTestRouter(s)
	req := scoped(httptest.NewRequest(http.MethodGet, "/v1/config/definitions/k", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestPublishConfigDefinition_ResolvesKeyThenPublishes(t *testing.T) {
	s := &stubStore{
		getDefinitionResult: &domain.ConfigDefinition{DefinitionID: "def-1", Key: "k"},
		publishVersion:      &domain.ConfigDefinitionVersion{VersionID: "ver-1", Version: 1},
	}
	r := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/definitions/k/publish", strings.NewReader(`{"lifecycle":"PUBLISHED"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotPublishDefinition.DefinitionID != "def-1" {
		t.Errorf("expected publish to name the resolved definition, got %q", s.gotPublishDefinition.DefinitionID)
	}
}

func TestActivateOverride_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/tenant", strings.NewReader(`{"key":"k","environment":"production"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivateOverride_InvalidPathScope(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/customer", strings.NewReader(`{"key":"k","environment":"production","value":5}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown override scope, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivateOverride_ForeignTenantLayerRefused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	body := `{"key":"k","environment":"production","value":5,"scope_id":"` + otherTenant + `"}`
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/tenant", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant-layer override of another tenant, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotOverride.Key != "" {
		t.Error("the store must never be reached for a foreign tenant override")
	}
}

func TestActivateOverride_TenantLayerCallsStoreWithScope(t *testing.T) {
	s := &stubStore{overrideEntry: &domain.ConfigEntry{ConfigID: "cfg-1", Key: "k"}}
	r := newTestRouter(s)
	body := `{"key":"k","environment":"production","value":5,"scope_id":"` + testTenant + `"}`
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/tenant", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotOverride.Layer != domain.ScopeTenant {
		t.Errorf("expected layer %s, got %s", domain.ScopeTenant, s.gotOverride.Layer)
	}
	if s.gotOverride.ScopeID == nil || *s.gotOverride.ScopeID != testTenant {
		t.Errorf("expected the tenant override to name the claimed tenant, got %v", s.gotOverride.ScopeID)
	}
	if s.gotOverride.ActorPrincipalID != testPrincipal {
		t.Errorf("expected the verified principal as the actor, got %q", s.gotOverride.ActorPrincipalID)
	}
}

func TestActivateOverride_CodedRefusalMapped(t *testing.T) {
	s := &stubStore{overrideErr: domain.ErrScopeNotAllowed}
	r := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/environment", strings.NewReader(`{"key":"k","environment":"production","value":5}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 scope_not_allowed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResolveConfig_MissingEnvironment(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/config/resolve", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResolveConfig_CodedRefusalMapped(t *testing.T) {
	s := &stubStore{resolveErr: domain.ErrNoAttestedSnapshot}
	r := newTestRouter(s)
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/config/resolve", strings.NewReader(`{"environment":"production"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 no_attested_snapshot, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResolveConfig_ForwardsKeys(t *testing.T) {
	s := &stubStore{resolveResult: &domain.ResolvedConfigSnapshot{SnapshotID: "s-1"}}
	r := newTestRouter(s)
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/config/resolve", strings.NewReader(`{"environment":"production","keys":["a","b"]}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.gotResolve.Keys) != 2 || s.gotResolve.Keys[0] != "a" {
		t.Errorf("expected the key allowlist forwarded, got %v", s.gotResolve.Keys)
	}
}

func TestCreateChange_Created(t *testing.T) {
	s := &stubStore{change: &domain.ConfigChange{ChangeID: "c-1", Status: domain.ChangeStatusProposed}}
	r := newTestRouter(s)
	body := `{"change_class":"C1","environment":"production","parts":[{"kind":"config","key":"k","scope":{"environment":"production"},"new_value":10}]}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotCreateChange.CallerTenantID != testTenant {
		t.Errorf("expected the verified tenant forwarded as caller, got %q", s.gotCreateChange.CallerTenantID)
	}
	if s.gotCreateChange.ActorPrincipalID != testPrincipal {
		t.Errorf("expected the verified principal as the actor, got %q", s.gotCreateChange.ActorPrincipalID)
	}
}

func TestApproveChange_NotFound(t *testing.T) {
	s := &stubStore{approveErr: domain.ErrChangeNotFound}
	r := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes/c-1/approve", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 change_not_found, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveChange_ApproverIsVerifiedPrincipal(t *testing.T) {
	s := &stubStore{approvedChange: &domain.ConfigChange{ChangeID: "c-1", Status: domain.ChangeStatusApproved}}
	r := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes/c-1/approve", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !s.gotApproval.Approved {
		t.Error("expected approval to default to approved=true")
	}
	if s.gotApproval.ByPrincipalID != testPrincipal {
		t.Errorf("expected the verified principal as the approver, got %q", s.gotApproval.ByPrincipalID)
	}
}

func TestActivateChange_RequiresApproval(t *testing.T) {
	s := &stubStore{activateErr: domain.ErrChangeApprovalRequired}
	r := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes/c-1/activate", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 change_approval_required, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateEmergencyChange_NoExpiryRefusedBeforeStore(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	body := `{"key":"k","environment":"production","new_value":10,"reason":"incident","incident_id":"inc-1"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/emergency-changes", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing expiry, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotEmergency.Key != "" {
		t.Error("the store must not be reached for a missing expiry")
	}
}

func TestCreateEmergencyChange_StoreRefusesNoExpiry(t *testing.T) {
	s := &stubStore{emergencyErr: domain.ErrEmergencyChangeNoExpiry}
	r := newTestRouter(s)
	body := `{"key":"k","environment":"production","new_value":10,"reason":"incident","incident_id":"inc-1","expires_at":"2026-09-26T00:00:00Z"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/emergency-changes", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 emergency_change_no_expiry, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateEmergencyChange_ForeignTenantRefused(t *testing.T) {
	s := &stubStore{}
	r := newTestRouter(s)
	body := `{"key":"k","environment":"production","new_value":10,"reason":"incident","incident_id":"inc-1","expires_at":"2026-09-26T00:00:00Z","tenant_id":"` + otherTenant + `"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/emergency-changes", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordAttestation_Success(t *testing.T) {
	s := &stubStore{attestation: &domain.RuntimeAttestation{AttestationID: "att-1"}}
	r := newTestRouter(s)
	body := `{"runtime_id":"rt-1","attest_key":"ak-1","environment":"production","observed_digest":"abc"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/runtime/attest", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotAttestation.CallerTenantID != testTenant {
		t.Errorf("expected the verified tenant forwarded as caller, got %q", s.gotAttestation.CallerTenantID)
	}
}

func TestCreateReleasePlan_Success(t *testing.T) {
	s := &stubStore{releasePlan: &domain.ReleasePlan{ReleasePlanID: "rp-1"}}
	r := newTestRouter(s)
	body := `{"environment":"production","strategy":"ALL_OR_NOTHING"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/new_ui/release-plans", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotReleasePlan.FlagKey != "new_ui" {
		t.Errorf("expected the path flag key forwarded, got %q", s.gotReleasePlan.FlagKey)
	}
}

func TestEvaluateFlag_Success(t *testing.T) {
	s := &stubStore{evaluation: &domain.FlagEvaluation{Key: "new_ui", Enabled: true, Outcome: domain.OutcomeValue}}
	r := newTestRouter(s)
	body := `{"environment":"production","subject_key":"user-1","context":{"plan":"enterprise"}}`
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/flags/new_ui/evaluate", strings.NewReader(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotEvaluate.Key != "new_ui" || s.gotEvaluate.SubjectKey != "user-1" {
		t.Errorf("expected key and subject forwarded, got %q / %q", s.gotEvaluate.Key, s.gotEvaluate.SubjectKey)
	}
	if s.gotEvaluate.Context["plan"] != "enterprise" {
		t.Errorf("expected evaluation context forwarded, got %v", s.gotEvaluate.Context)
	}
}

func TestEvaluateFlag_MissingSubjectKey(t *testing.T) {
	r := newTestRouter(&stubStore{})
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/flags/new_ui/evaluate", strings.NewReader(`{"environment":"production"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
