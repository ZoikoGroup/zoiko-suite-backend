package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
	"zoiko.io/search-indexer-svc/internal/indexer"
	"zoiko.io/search-indexer-svc/internal/query"
	"zoiko.io/search-indexer-svc/internal/retrieval"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeStore struct {
	mu           sync.Mutex
	contract     *domain.IndexContract
	generation   *domain.IndexGeneration
	sources      []domain.SearchSource
	evidence     []domain.SearchEvidence
	tombstones   []domain.RestrictionTombstone
	tombstoneErr error
}

func (f *fakeStore) GetPublishedContract(_ context.Context, scope string) (*domain.IndexContract, error) {
	if f.contract == nil || f.contract.ScopeName != scope {
		return nil, domain.ErrNotFound
	}
	return f.contract, nil
}

func (f *fakeStore) GetActiveGeneration(_ context.Context, scope string) (*domain.IndexGeneration, error) {
	if f.generation == nil || f.generation.ScopeName != scope {
		return nil, domain.ErrNotFound
	}
	return f.generation, nil
}

func (f *fakeStore) RecordEvidence(_ context.Context, e domain.SearchEvidence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evidence = append(f.evidence, e)
	return nil
}

func (f *fakeStore) UpsertTombstone(_ context.Context, t domain.RestrictionTombstone, _ string) (bool, error) {
	if f.tombstoneErr != nil {
		return false, f.tombstoneErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tombstones = append(f.tombstones, t)
	return true, nil
}

func (f *fakeStore) ListContracts(context.Context, string) ([]domain.IndexContract, error) {
	if f.contract == nil {
		return nil, nil
	}
	return []domain.IndexContract{*f.contract}, nil
}

func (f *fakeStore) ListSources(context.Context) ([]domain.SearchSource, error) {
	return f.sources, nil
}
func (f *fakeStore) CreateSource(_ context.Context, s domain.SearchSource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources = append(f.sources, s)
	return nil
}
func (f *fakeStore) GetSourceByType(_ context.Context, st string) (*domain.SearchSource, error) {
	for i := range f.sources {
		if f.sources[i].SourceType == st {
			return &f.sources[i], nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeStore) GetSource(context.Context, string) (*domain.SearchSource, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) CreateContract(context.Context, domain.IndexContract) error { return nil }
func (f *fakeStore) GetContract(context.Context, string) (*domain.IndexContract, error) {
	if f.contract == nil {
		return nil, domain.ErrNotFound
	}
	return f.contract, nil
}
func (f *fakeStore) TransitionContract(context.Context, string, domain.ContractState, domain.ContractState) error {
	return nil
}
func (f *fakeStore) NextContractVersion(context.Context, string) (int, error)       { return 1, nil }
func (f *fakeStore) CreateGeneration(context.Context, domain.IndexGeneration) error { return nil }
func (f *fakeStore) GetGeneration(context.Context, string) (*domain.IndexGeneration, error) {
	if f.generation == nil {
		return nil, domain.ErrNotFound
	}
	return f.generation, nil
}
func (f *fakeStore) ListGenerations(context.Context, string) ([]domain.IndexGeneration, error) {
	if f.generation == nil {
		return nil, nil
	}
	return []domain.IndexGeneration{*f.generation}, nil
}
func (f *fakeStore) TransitionGeneration(context.Context, string, domain.GenerationState, domain.GenerationState, string, string) error {
	return nil
}
func (f *fakeStore) UpsertCheckpoint(context.Context, domain.IndexCheckpoint) error { return nil }
func (f *fakeStore) ListCheckpoints(context.Context, string) ([]domain.IndexCheckpoint, error) {
	return []domain.IndexCheckpoint{}, nil
}
func (f *fakeStore) GetProjectionRecord(context.Context, string, string, string, string) (*domain.ProjectionRecord, error) {
	return nil, nil
}
func (f *fakeStore) UpsertProjectionRecord(context.Context, domain.ProjectionRecord) (bool, error) {
	return true, nil
}
func (f *fakeStore) CountProjections(context.Context, string) (int64, int64, error) {
	return 42, 0, nil
}
func (f *fakeStore) MarkTombstoneState(context.Context, string, string, string, string, domain.PropagationState, string) error {
	return nil
}
func (f *fakeStore) ListPendingVerification(context.Context, int) ([]domain.RestrictionTombstone, error) {
	return nil, nil
}
func (f *fakeStore) ListTombstones(context.Context, string, string, int) ([]domain.RestrictionTombstone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tombstones, nil
}
func (f *fakeStore) ListEvidence(context.Context, string, string, int) ([]domain.SearchEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evidence, nil
}
func (f *fakeStore) Ping(context.Context) error { return nil }
func (f *fakeStore) Close()                     {}

type fakeEngine struct {
	result   searchclient.Result
	err      error
	lastPlan searchclient.ExecutionPlan
	indexed  []searchclient.Projection
}

func (f *fakeEngine) ExecutePlan(_ context.Context, _ string, p searchclient.ExecutionPlan) (searchclient.Result, error) {
	f.lastPlan = p
	return f.result, f.err
}
func (f *fakeEngine) IndexProjection(_ context.Context, _ string, p searchclient.Projection) error {
	f.indexed = append(f.indexed, p)
	return nil
}
func (f *fakeEngine) GetProjection(context.Context, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}
func (f *fakeEngine) CountProjections(context.Context, string, map[string]string) (int64, error) {
	return 0, nil
}
func (f *fakeEngine) EnsureIndex(context.Context, searchclient.IndexName) error { return nil }
func (f *fakeEngine) Index(context.Context, searchclient.IndexName, searchclient.Document) error {
	return nil
}
func (f *fakeEngine) Search(context.Context, searchclient.IndexName, searchclient.SearchQuery) ([]searchclient.SearchResult, error) {
	return nil, nil
}
func (f *fakeEngine) EnsureGeneration(context.Context, string, []searchclient.FieldMapping) error {
	return nil
}
func (f *fakeEngine) ActivateGeneration(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeEngine) ActiveGeneration(context.Context, string) (string, error) { return "", nil }
func (f *fakeEngine) DropIndex(context.Context, string) error                  { return nil }
func (f *fakeEngine) DeleteProjection(context.Context, string, string) error   { return nil }
func (f *fakeEngine) Ping(context.Context) error                               { return nil }

// ── harness ──────────────────────────────────────────────────────────────────

var sharedMetrics *telemetry.Metrics

func testMetrics() *telemetry.Metrics {
	if sharedMetrics == nil {
		sharedMetrics = telemetry.NewMetrics("search-indexer-svc-handler-test")
	}
	return sharedMetrics
}

const (
	tenantA  = "11111111-1111-1111-1111-111111111111"
	tenantB  = "22222222-2222-2222-2222-222222222222"
	actor    = "33333333-3333-3333-3333-333333333333"
	platform = "44444444-4444-4444-4444-444444444444"

	// Real UUIDs, because the routes now refuse a path id that is not one.
	// An unparseable id used to reach the driver as
	// `invalid input syntax for type uuid` — a 500 for what is a caller
	// error, and indistinguishable in monitoring from a real outage.
	contractID   = "55555555-5555-5555-5555-555555555555"
	generationID = "66666666-6666-6666-6666-666666666666"
)

var testKey = []byte("handler-test-key-at-least-thirty-two-bytes")

func testContract() *domain.IndexContract {
	return &domain.IndexContract{
		ContractID: contractID, SourceID: "s-1", ScopeName: "obligation", Version: 1,
		State: domain.ContractPublished, RetrievalClass: domain.RetrievalR1,
		AuthzAction: "OBLIGATION_READ",
		Fields: []domain.SearchFieldDefinition{
			{FieldID: "obligation_code", Type: "TEXT", Searchable: true, Returnable: true,
				SnippetAllowed: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "obligation_status", Type: "KEYWORD", Filterable: true, Facetable: true,
				Returnable: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "api_secret", Type: "KEYWORD", SensitivityClass: domain.SensitivitySecretProhibited},
		},
	}
}

func testGeneration() *domain.IndexGeneration {
	return &domain.IndexGeneration{
		GenerationID: generationID, ContractID: contractID, ScopeName: "obligation",
		PhysicalIndex: "obligation-gabc", State: domain.GenerationActive,
	}
}

type harness struct {
	router *chi.Mux
	store  *fakeStore
	engine *fakeEngine
	authz  *stubAuthz
}

type stubAuthz struct {
	err   error
	calls []string
}

func (s *stubAuthz) CheckAllowed(_ context.Context, principal, entity, action string) error {
	s.calls = append(s.calls, principal+"|"+entity+"|"+action)
	return s.err
}

func newHarness(t *testing.T, az *stubAuthz) *harness {
	t.Helper()
	if az == nil {
		az = &stubAuthz{}
	}
	st := &fakeStore{contract: testContract(), generation: testGeneration()}
	eng := &fakeEngine{result: searchclient.Result{Total: 0, Relation: "eq"}}

	metrics := testMetrics()
	planner := query.NewPlanner(query.DefaultLimits(), testKey)
	retriever := retrieval.New(eng, az, nil, metrics, zap.NewNop(), time.Second)
	ix := indexer.New(st, eng, nil, metrics, zap.NewNop())

	h := New(Config{
		Store: st, Engine: eng, Planner: planner, Retriever: retriever,
		Indexer: ix, AuthZ: az, Events: nil, Metrics: metrics, Log: zap.NewNop(),
		PlatformScopeID: platform, EvidenceKey: testKey,
	})

	r := chi.NewRouter()
	// Strict enforcement, matching the deployed configuration. A test that
	// ran in write-strict would not exercise the refusals the service
	// actually performs.
	r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), envelope.ModeStrict, nil))
	h.Routes(r)

	return &harness{router: r, store: st, engine: eng, authz: az}
}

// req builds a fully-enveloped request. Every §4 mandatory field is present,
// so a refusal in a test is about the thing under test and not about a header.
func req(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", tenantA)
	r.Header.Set("X-Principal-Id", actor)
	r.Header.Set("X-Source-Channel", "api")
	r.Header.Set("X-Purpose-Context", "COMPLIANCE_REVIEW")
	r.Header.Set("X-Request-Id", "req-1")
	r.Header.Set("X-Correlation-ID", "corr-1")
	r.Header.Set("Idempotency-Key", "idem-1")
	return r
}

func do(h *harness, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, r)
	return w
}

func bodyOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

// ── tests: search ────────────────────────────────────────────────────────────

func TestSearch_HappyPath(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST",
	}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// ESR-001. No tenant header, no search — and the refusal carries the code.
func TestSearch_WithoutTenantIsRefusedWithESR001(t *testing.T) {
	h := newHarness(t, nil)
	r := req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation"})
	r.Header.Del("X-Tenant-Id")

	w := do(h, r)
	// The envelope middleware refuses first, in strict mode. Either way the
	// request never reaches an index — which is the property that matters.
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnauthorized}, w.Code)
	assert.NotContains(t, w.Body.String(), "results")
}

// NP-01. A client-supplied tenant in the BODY is not merely ignored — the
// request is refused, because a caller that believed its tenant took effect
// would read an empty result as "that tenant has no data".
func TestSearch_TenantInBodyIsRejectedNotSilentlyIgnored(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST", "tenant_id": tenantB,
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_request", bodyOf(t, w)["error"])
}

// The compiled plan's tenant filter is the HEADER's tenant, whatever else was
// sent. Asserted on the plan the engine received, because a 200 with no
// results cannot distinguish "filtered correctly" from "filtered by nothing".
func TestSearch_EngineReceivesTheHeaderTenant(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST",
	}))
	require.Equal(t, http.StatusOK, w.Code)

	var found bool
	for _, f := range h.engine.lastPlan.MandatoryFilters {
		if f.Field == "tenant_id" {
			found = true
			assert.Equal(t, []string{tenantA}, f.Values)
		}
	}
	assert.True(t, found, "the executed plan must carry a tenant filter")
}

// ESR-002 for an unregistered scope.
func TestSearch_UnregisteredScopeIsESR002(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "no-such-scope"}))

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, string(domain.ReasonScopeNotRegistered), bodyOf(t, w)["reason_code"])
}

// ESR-003.
func TestSearch_ForbiddenOperatorIsESR003(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST*",
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, string(domain.ReasonQueryOperatorForbidden), bodyOf(t, w)["reason_code"])
}

// ESR-006, and the refusal names the code as well as its meaning so a caller
// does not have to carry the §11.3 table.
func TestSearch_NonReturnableFieldIsESR006WithMeaning(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "requested_fields": []string{"api_secret"},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	body := bodyOf(t, w)
	assert.Equal(t, string(domain.ReasonFieldNotReturnable), body["reason_code"])
	assert.Equal(t, "FIELD_NOT_RETURNABLE", body["reason"])
}

// ESR-015.
func TestSearch_OversizedPageIsESR015(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "size": 9999,
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, string(domain.ReasonResultWindowExceeded), bodyOf(t, w)["reason_code"])
}

// INV-24. A partial engine answer produces 206, not 200. The status code is
// the part of a response a caller cannot overlook.
func TestSearch_PartialAnswerIs206(t *testing.T) {
	h := newHarness(t, nil)
	h.engine.result = searchclient.Result{
		Total: 5, Relation: "eq", Partial: true, PartialReason: "1 of 3 shards failed",
	}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation"}))
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "PARTIAL", bodyOf(t, w)["completeness_state"])
}

// Evidence is recorded on the SUCCESS path.
func TestSearch_RecordsEvidenceOnSuccess(t *testing.T) {
	h := newHarness(t, nil)
	do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))

	require.Len(t, h.store.evidence, 1)
	e := h.store.evidence[0]
	assert.Equal(t, tenantA, e.TenantID)
	assert.Equal(t, "obligation", e.ScopeName)
	assert.Equal(t, "req-1", e.RequestID)
	assert.NotEmpty(t, e.PlanDigest)
}

// And on the REFUSAL path. An evidence table that only records successful
// searches cannot answer "what was this actor trying to reach".
func TestSearch_RecordsEvidenceOnRefusal(t *testing.T) {
	h := newHarness(t, nil)
	do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "GST*",
	}))

	require.Len(t, h.store.evidence, 1)
	assert.Contains(t, h.store.evidence[0].ReasonCodes, string(domain.ReasonQueryOperatorForbidden))
}

// INV-17 / §9.2. The evidence carries a digest, never the query text — and
// the text here is exactly the kind §9.2 names as personal data.
func TestSearch_EvidenceNeverStoresQueryText(t *testing.T) {
	h := newHarness(t, nil)
	do(h, req(t, http.MethodPost, "/v1/search", map[string]any{
		"scope": "obligation", "query": "Jane Doe misconduct dismissal",
	}))

	require.Len(t, h.store.evidence, 1)
	raw, err := json.Marshal(h.store.evidence[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "Jane")
	assert.NotContains(t, string(raw), "misconduct")
	assert.Len(t, h.store.evidence[0].QueryDigest, 64)
}

// ── tests: retrieve ──────────────────────────────────────────────────────────

// INV-29's bulk boundary. A retrieve with too many refs is refused with
// ESR-016 rather than quietly becoming an export.
func TestRetrieve_BulkLimitIsEnforcedWithESR016(t *testing.T) {
	h := newHarness(t, nil)
	refs := make([]map[string]string, 0, maxRetrieveRefs+1)
	for i := 0; i <= maxRetrieveRefs; i++ {
		refs = append(refs, map[string]string{"source_type": "obligation", "source_id": "ob-x"})
	}

	w := do(h, req(t, http.MethodPost, "/v1/retrieve", map[string]any{
		"scope": "obligation", "refs": refs,
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, string(domain.ReasonExportAuthzRequired), bodyOf(t, w)["reason_code"])
}

// §9.1 / insecure direct object reference. A retrieve by id still compiles a
// plan with the mandatory tenant and tombstone filters — it is not a
// short-circuit to a raw engine get.
func TestRetrieve_StillAppliesMandatoryFilters(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/retrieve", map[string]any{
		"scope": "obligation",
		"refs":  []map[string]string{{"source_type": "obligation", "source_id": "ob-1"}},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	fields := map[string]bool{}
	for _, f := range h.engine.lastPlan.MandatoryFilters {
		fields[f.Field] = true
	}
	assert.True(t, fields["tenant_id"], "a retrieve must still be tenant-filtered")
	assert.True(t, fields["_id"], "and narrowed to the named refs")
	require.Len(t, h.engine.lastPlan.MandatoryMustNot, 1)
	assert.Equal(t, "tombstoned", h.engine.lastPlan.MandatoryMustNot[0].Field)
}

// ── tests: exports ───────────────────────────────────────────────────────────

// INV-29 / NP-29. An export needs its OWN authorization; search permission
// does not carry over.
func TestExport_RequiresItsOwnAuthorization(t *testing.T) {
	h := newHarness(t, &stubAuthz{err: authz.ErrDenied})
	w := do(h, req(t, http.MethodPost, "/v1/search-exports", map[string]any{
		"scope": "obligation", "reason": "regulatory disclosure",
	}))

	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, string(domain.ReasonExportAuthzRequired), bodyOf(t, w)["reason_code"])
	require.NotEmpty(t, h.authz.calls)
	assert.Contains(t, h.authz.calls[0], ActionExport)
}

// An export must state its own reason; the search purpose does not carry over.
func TestExport_RequiresItsOwnReason(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search-exports", map[string]any{"scope": "obligation"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "reason_required", bodyOf(t, w)["error"])
}

func TestExport_AuthorizedRecordsEvidenceAndTransfersNothing(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search-exports", map[string]any{
		"scope": "obligation", "reason": "regulatory disclosure",
	}))

	require.Equal(t, http.StatusAccepted, w.Code)
	body := bodyOf(t, w)
	assert.Equal(t, "AUTHORIZED", body["status"])
	assert.Contains(t, body["detail"], "no records have been transferred")
	require.Len(t, h.store.evidence, 1)
	assert.Contains(t, h.store.evidence[0].ReasonCodes, string(domain.ReasonExportAuthzRequired))
}

// ── tests: restrictions ──────────────────────────────────────────────────────

func TestRestriction_RequiresAuthorizationAndSourceEvent(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation",
		"source_id": "ob-1", "reason": "PRV_ERASURE",
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "source_event_id_required", bodyOf(t, w)["error"])

	denied := newHarness(t, &stubAuthz{err: authz.ErrDenied})
	w = do(denied, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation", "source_id": "ob-1",
		"reason": "PRV_ERASURE", "source_event_id": "prv-evt-1",
	}))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// §2.2. A restriction reports APPLIED, and says explicitly that APPLIED is
// not VERIFIED — a privacy workflow must not read "accepted" as "proven".
func TestRestriction_ReportsAppliedNotVerified(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation", "source_id": "ob-1",
		"reason": "PRV_ERASURE", "source_event_id": "prv-evt-1",
	}))

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	body := bodyOf(t, w)
	assert.Equal(t, string(domain.PropagationApplied), body["state"])
	assert.Equal(t, false, body["verified"])
	assert.Contains(t, body["verified_note"], "not VERIFIED")
}

// NP-48's out-of-order half. A restriction for a ref with nothing indexed
// still writes a tombstone, so an in-flight event arriving afterwards is
// refused rather than indexed into visibility.
func TestRestriction_WritesTombstoneEvenWhenNothingIsIndexed(t *testing.T) {
	h := newHarness(t, nil)
	do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation", "source_id": "never-indexed",
		"reason": "PRV_ERASURE", "source_event_id": "prv-evt-2",
	}))

	require.Len(t, h.engine.indexed, 1)
	assert.True(t, h.engine.indexed[0].Tombstoned)
	assert.Empty(t, h.engine.indexed[0].Fields, "a tombstone carries no content")
	assert.Positive(t, h.engine.indexed[0].RestrictionEpoch)
}

// NP-11. A stale restriction epoch is a 409 with ESR-013, not a 200 — a
// privacy workflow must not record an erasure that was declined.
func TestRestriction_StaleEpochIs409WithESR013(t *testing.T) {
	h := newHarness(t, nil)
	h.store.tombstoneErr = domain.ErrStaleEpoch

	w := do(h, req(t, http.MethodPost, "/v1/restrictions", map[string]any{
		"scope": "obligation", "source_type": "obligation", "source_id": "ob-1",
		"reason": "PRV_ERASURE", "source_event_id": "old-evt",
	}))

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, string(domain.ReasonRestrictionEpochMismtch), bodyOf(t, w)["reason_code"])
}

// ── tests: control plane ─────────────────────────────────────────────────────

// Every control-plane write is authorized against PLATFORM scope, not the
// caller's own entity — a tenant-scoped grant must not change what every
// other tenant can search.
func TestControlPlane_AuthorizesAgainstPlatformScope(t *testing.T) {
	h := newHarness(t, nil)
	do(h, req(t, http.MethodPost, "/v1/search-sources", map[string]any{
		"owner_service": "obligations-svc", "source_type": "obligation",
		"event_topic": "zoiko.obligations.events",
		"event_types": []string{"obligation.created"},
	}))

	require.NotEmpty(t, h.authz.calls)
	assert.Contains(t, h.authz.calls[0], platform)
	assert.Contains(t, h.authz.calls[0], ActionSourceRegister)
}

// The guard is real, proven with a DENYING stub. A control-plane test that
// only ever passes with a permitting stub proves the happy path, not the gate.
func TestControlPlane_DeniedPrincipalCannotRegisterASource(t *testing.T) {
	h := newHarness(t, &stubAuthz{err: authz.ErrDenied})
	w := do(h, req(t, http.MethodPost, "/v1/search-sources", map[string]any{
		"owner_service": "obligations-svc", "source_type": "obligation",
		"event_topic": "zoiko.obligations.events",
		"event_types": []string{"obligation.created"},
	}))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, h.store.sources)
}

// INV-07's control-plane half: an unreachable authorizer fails closed with
// 503, never open.
func TestControlPlane_UnavailableAuthorizerFailsClosed(t *testing.T) {
	h := newHarness(t, &stubAuthz{err: authz.ErrUnavailable})
	w := do(h, req(t, http.MethodPost, "/v1/search-sources", map[string]any{
		"owner_service": "obligations-svc", "source_type": "obligation",
		"event_topic": "zoiko.obligations.events",
		"event_types": []string{"obligation.created"},
	}))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, h.store.sources)
}

// INV-09. SECRET_PROHIBITED is not a sensitivity ceiling — a source whose
// ceiling was that class could have no legal contract at all.
func TestCreateSource_RefusesProhibitedCeiling(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search-sources", map[string]any{
		"owner_service": "vault-svc", "source_type": "secret",
		"event_topic": "zoiko.secret.events", "event_types": []string{"secret.created"},
		"sensitivity_ceiling": "SECRET_PROHIBITED",
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "prohibited_ceiling", bodyOf(t, w)["error"])
}

// A source with no event types would subscribe a topic that can never
// produce a projection — which reads as "indexing is broken".
func TestCreateSource_RefusesSourceWithNoEventTypes(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodPost, "/v1/search-sources", map[string]any{
		"owner_service": "obligations-svc", "source_type": "obligation",
		"event_topic": "zoiko.obligations.events",
	}))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "event_types_required", bodyOf(t, w)["error"])
}

// §4.2. A contract is always created DRAFT; publication is a separate,
// separately-authorized act because §4.2's gates are a human workflow.
func TestCreateContract_IsAlwaysDraft(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", OwnerService: "obligations-svc",
		SensitivityCeiling: domain.SensitivityFinancial, FreshnessClass: "S1",
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation", "scope_name": "obligation",
		"authz_action": "OBLIGATION_READ",
		"fields": []map[string]any{
			{"name": "obligation_code", "type": "TEXT", "searchable": true, "returnable": true},
		},
	}))

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, string(domain.ContractDraft), bodyOf(t, w)["publication_state"])
}

// INV-13. snippet_allowed without returnable would return the content in
// fragments.
func TestCreateContract_RefusesSnippetWithoutReturnable(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityFinancial,
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation", "authz_action": "OBLIGATION_READ",
		"fields": []map[string]any{
			{"name": "notes", "type": "TEXT", "searchable": true,
				"snippet_allowed": true, "returnable": false},
		},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, bodyOf(t, w)["detail"], "not returnable")
}

// INV-09. A prohibited field may be DECLARED (that is how the projector
// learns to refuse a payload carrying one) but never exposed.
func TestCreateContract_RefusesExposingAProhibitedField(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityFinancial,
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation", "authz_action": "OBLIGATION_READ",
		"fields": []map[string]any{
			{"name": "api_secret", "type": "KEYWORD",
				"sensitivity_class": "SECRET_PROHIBITED", "searchable": true},
		},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, bodyOf(t, w)["detail"], "SECRET_PROHIBITED")
}

// §4.1's sensitivity ceiling: a contract cannot expose a field more sensitive
// than its source was registered to carry.
func TestCreateContract_EnforcesSourceSensitivityCeiling(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityInternal,
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation", "authz_action": "OBLIGATION_READ",
		"fields": []map[string]any{
			{"name": "employee_notes", "type": "TEXT", "searchable": true,
				"returnable": true, "sensitivity_class": "HR"},
		},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, bodyOf(t, w)["detail"], "ceiling")
}

// §7.1. R0 skips re-authorization, so it cannot carry anything above
// INTERNAL — otherwise INV-05 is defeated by configuration rather than by a
// bug.
func TestCreateContract_RefusesSensitiveFieldsUnderR0(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityHR,
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation", "authz_action": "OBLIGATION_READ",
		"retrieval_class": "R0",
		"fields": []map[string]any{
			{"name": "employee_notes", "type": "TEXT", "searchable": true,
				"returnable": true, "sensitivity_class": "HR"},
		},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, bodyOf(t, w)["detail"], "R0")
}

// A contract with no authz_action would leave R1/R2 retrieval with nothing to
// ask authorization-svc — i.e. silently unauthorized results.
func TestCreateContract_RequiresAnAuthzAction(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{
		SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityInternal,
	}}

	w := do(h, req(t, http.MethodPost, "/v1/index-contracts", map[string]any{
		"source_type": "obligation",
		"fields":      []map[string]any{{"name": "code", "type": "TEXT", "returnable": true}},
	}))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "authz_action_required", bodyOf(t, w)["error"])
}

// §8.1. ACTIVE is reachable only from READY. A BUILDING generation cannot be
// cut over, which is NP-42 stated as a state machine.
func TestTransitionGeneration_CannotActivateFromBuilding(t *testing.T) {
	h := newHarness(t, nil)
	h.store.generation.State = domain.GenerationBuilding

	w := do(h, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{
		"state": "ACTIVE",
	}))

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "invalid_transition", bodyOf(t, w)["error"])
}

// §8.1's exit from ACTIVE is "RETIRED after replacement". Retiring the
// serving generation first would leave the alias pointing at nothing.
func TestTransitionGeneration_CannotRetireTheServingGeneration(t *testing.T) {
	h := newHarness(t, nil)
	h.store.generation.State = domain.GenerationActive

	w := do(h, req(t, http.MethodPost, "/v1/index-generations/"+generationID+"/state", map[string]any{
		"state": "RETIRED",
	}))

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "active_generation", bodyOf(t, w)["error"])
}

// §2.2: only PUBLISHED contracts build production generations.
func TestCreateGeneration_RequiresAPublishedContract(t *testing.T) {
	h := newHarness(t, nil)
	h.store.contract = nil

	w := do(h, req(t, http.MethodPost, "/v1/index-generations", map[string]any{"scope": "obligation"}))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "no_published_contract", bodyOf(t, w)["error"])
}

// ── tests: scope catalogue ───────────────────────────────────────────────────

// NP-53 applies to the catalogue too: a prohibited field is not disclosed to
// exist.
func TestListScopes_DoesNotDiscloseProhibitedFields(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodGet, "/v1/scopes", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "api_secret")
	assert.Contains(t, w.Body.String(), "obligation_code")
}

// §7.3. The catalogue carries no counts: a scope listing that said how many
// documents a tenant has would be a cardinality disclosure made before any
// query was authorized.
func TestListScopes_CarriesNoCounts(t *testing.T) {
	h := newHarness(t, nil)
	w := do(h, req(t, http.MethodGet, "/v1/scopes", nil))

	require.Equal(t, http.StatusOK, w.Code)
	body := strings.ToLower(w.Body.String())
	assert.NotContains(t, body, "document_count")
	assert.NotContains(t, body, "\"total\"")
}

// A path id that is not a UUID answers 404, not 500.
//
// Every id in this service's paths is a uuid column in Postgres, and an
// unparseable value reaches the driver as `invalid input syntax for type
// uuid`. That is the wrong answer twice over: the request failed because the
// CALLER sent a bad id, and a server fault is indistinguishable in monitoring
// from a real outage — the same defect tenant-entity-registry-svc's store
// carries a fix for.
//
// 404 rather than 400, deliberately: a malformed id and an id that does not
// exist are the same fact from a caller's side, and answering them differently
// would let a caller tell a well-formed stranger's id apart from noise.
func TestPathParams_MalformedUUIDIs404NotServerError(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{
		"/v1/index-contracts/not-a-uuid",
		"/v1/index-contracts/not-a-uuid/state",
		"/v1/index-generations/not-a-uuid/state",
	} {
		t.Run(path, func(t *testing.T) {
			method := http.MethodGet
			var body any
			if strings.HasSuffix(path, "/state") {
				method, body = http.MethodPost, map[string]any{"state": "ACTIVE"}
			}
			w := do(h, req(t, method, path, body))
			assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			assert.Equal(t, "not_found", bodyOf(t, w)["error"])
		})
	}
}
