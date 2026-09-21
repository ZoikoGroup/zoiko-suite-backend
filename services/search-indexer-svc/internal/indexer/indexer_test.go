package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/projection"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeStore models the parts of the control plane Apply touches, INCLUDING
// the compare-and-set semantics of UpsertProjectionRecord. Reimplementing the
// epoch/version rule here rather than stubbing it out is deliberate: the rule
// is the thing under test at this layer, and a stub that always applied would
// let a regression in the caller's ordering pass unnoticed. The SQL that
// enforces it for real is pinned separately in the store integration suite.
type fakeStore struct {
	mu        sync.Mutex
	ledger    map[string]domain.ProjectionRecord
	tombs     []domain.RestrictionTombstone
	states    map[string]domain.PropagationState
	sources   []domain.SearchSource
	upsertErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		ledger: map[string]domain.ProjectionRecord{},
		states: map[string]domain.PropagationState{},
	}
}

func ledgerKey(r domain.ProjectionRecord) string {
	return r.TenantID + "|" + r.ScopeName + "|" + r.SourceType + "|" + r.SourceID
}

func (f *fakeStore) UpsertProjectionRecord(_ context.Context, r domain.ProjectionRecord) (bool, error) {
	if f.upsertErr != nil {
		return false, f.upsertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := ledgerKey(r)
	existing, found := f.ledger[key]
	if found {
		// Exactly the SQL's WHERE clause: epoch first and strict, then
		// version at an equal epoch.
		newer := r.RestrictionEpoch > existing.RestrictionEpoch ||
			(r.RestrictionEpoch == existing.RestrictionEpoch && r.SourceVersion > existing.SourceVersion)
		if !newer {
			return false, nil
		}
	}
	r.IndexedAt = time.Now().UTC()
	f.ledger[key] = r
	return true, nil
}

func (f *fakeStore) GetProjectionRecord(_ context.Context, tenantID, scope, sourceType, sourceID string) (*domain.ProjectionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.ledger[tenantID+"|"+scope+"|"+sourceType+"|"+sourceID]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (f *fakeStore) UpsertTombstone(_ context.Context, t domain.RestrictionTombstone, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.tombs {
		if existing.TenantID == t.TenantID && existing.SourceType == t.SourceType &&
			existing.SourceID == t.SourceID && existing.Epoch >= t.Epoch {
			return false, domain.ErrStaleEpoch
		}
	}
	f.tombs = append(f.tombs, t)
	return true, nil
}

func (f *fakeStore) MarkTombstoneState(_ context.Context, tenantID, sourceType, sourceID, eventID string, s domain.PropagationState, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[tenantID+"|"+sourceType+"|"+sourceID+"|"+eventID] = s
	return nil
}

func (f *fakeStore) ListPendingVerification(context.Context, int) ([]domain.RestrictionTombstone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.RestrictionTombstone{}
	for _, t := range f.tombs {
		key := t.TenantID + "|" + t.SourceType + "|" + t.SourceID + "|" + t.SourceEventID
		if f.states[key] != domain.PropagationVerified {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) ListSources(context.Context) ([]domain.SearchSource, error) {
	return f.sources, nil
}

// Unused by these tests; present so *fakeStore satisfies store.Store.
func (f *fakeStore) CreateSource(context.Context, domain.SearchSource) error { return nil }
func (f *fakeStore) GetSource(context.Context, string) (*domain.SearchSource, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) GetSourceByType(context.Context, string) (*domain.SearchSource, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) CreateContract(context.Context, domain.IndexContract) error { return nil }
func (f *fakeStore) GetContract(context.Context, string) (*domain.IndexContract, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) GetPublishedContract(context.Context, string) (*domain.IndexContract, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) ListContracts(context.Context, string) ([]domain.IndexContract, error) {
	return nil, nil
}
func (f *fakeStore) TransitionContract(context.Context, string, domain.ContractState, domain.ContractState) error {
	return nil
}
func (f *fakeStore) NextContractVersion(context.Context, string) (int, error)       { return 1, nil }
func (f *fakeStore) CreateGeneration(context.Context, domain.IndexGeneration) error { return nil }
func (f *fakeStore) GetGeneration(context.Context, string) (*domain.IndexGeneration, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) ListGenerations(context.Context, string) ([]domain.IndexGeneration, error) {
	return nil, nil
}
func (f *fakeStore) GetActiveGeneration(context.Context, string) (*domain.IndexGeneration, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeStore) TransitionGeneration(context.Context, string, domain.GenerationState, domain.GenerationState, string, string) error {
	return nil
}
func (f *fakeStore) UpsertCheckpoint(context.Context, domain.IndexCheckpoint) error { return nil }
func (f *fakeStore) ListCheckpoints(context.Context, string) ([]domain.IndexCheckpoint, error) {
	return nil, nil
}
func (f *fakeStore) CountProjections(context.Context, string) (int64, int64, error) { return 0, 0, nil }
func (f *fakeStore) ListTombstones(context.Context, string, string, int) ([]domain.RestrictionTombstone, error) {
	return nil, nil
}
func (f *fakeStore) RecordEvidence(context.Context, domain.SearchEvidence) error { return nil }
func (f *fakeStore) ListEvidence(context.Context, string, string, int) ([]domain.SearchEvidence, error) {
	return nil, nil
}
func (f *fakeStore) Ping(context.Context) error { return nil }
func (f *fakeStore) Close()                     {}

type fakeEngine struct {
	mu    sync.Mutex
	docs  map[string]searchclient.Projection
	err   error
	calls int
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{docs: map[string]searchclient.Projection{}}
}

func (f *fakeEngine) IndexProjection(_ context.Context, index string, p searchclient.Projection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.docs[index+"/"+p.DocID] = p
	return nil
}

func (f *fakeEngine) GetProjection(_ context.Context, index, docID string) (map[string]any, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.docs[index+"/"+docID]
	if !ok {
		return nil, false, nil
	}
	return map[string]any{"tombstoned": p.Tombstoned, "source_id": p.SourceID}, true, nil
}

func (f *fakeEngine) CountProjections(context.Context, string, map[string]string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, p := range f.docs {
		if !p.Tombstoned {
			n++
		}
	}
	return n, nil
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
func (f *fakeEngine) ExecutePlan(context.Context, string, searchclient.ExecutionPlan) (searchclient.Result, error) {
	return searchclient.Result{}, nil
}
func (f *fakeEngine) Ping(context.Context) error { return nil }

// ── fixtures ─────────────────────────────────────────────────────────────────

var sharedMetrics *telemetry.Metrics

func testMetrics() *telemetry.Metrics {
	if sharedMetrics == nil {
		sharedMetrics = telemetry.NewMetrics("search-indexer-svc-indexer-test")
	}
	return sharedMetrics
}

func source() domain.SearchSource {
	return domain.SearchSource{
		SourceID: "s-1", OwnerService: "obligations-svc", SourceType: "obligation",
		ResidencyRegion: "EU", SensitivityCeiling: domain.SensitivityFinancial,
		EventTopic:            "zoiko.obligations.events",
		EventTypes:            []string{"obligation.created", "obligation.updated"},
		RestrictionEventTypes: []string{"obligation.deleted"},
	}
}

func contract() domain.IndexContract {
	return domain.IndexContract{
		ContractID: "c-1", SourceID: "s-1", ScopeName: "obligation", Version: 1,
		State: domain.ContractPublished, RetrievalClass: domain.RetrievalR1,
		AuthzAction: "OBLIGATION_READ",
		Fields: []domain.SearchFieldDefinition{
			{FieldID: "obligation_code", SourcePath: "obligation_code", Type: "TEXT",
				Searchable: true, Returnable: true, SensitivityClass: domain.SensitivityInternal},
		},
	}
}

// withLiveScope builds an Indexer whose registry is populated directly,
// bypassing Reload's store round-trip.
func withLiveScope(st *fakeStore, eng *fakeEngine) *Indexer {
	ix := New(st, eng, nil, testMetrics(), zap.NewNop())
	proj, err := projection.New(contract(), source())
	if err != nil {
		panic(err)
	}
	ix.bySourceType["obligation"] = &liveScope{
		projector: proj, generationID: "g-1", physicalIndex: "obligation-g1",
	}
	ix.topics["zoiko.obligations.events"] = true
	return ix
}

func event(t *testing.T, eventType, eventID, tenantID string, emitted time.Time, payload map[string]any) projection.Event {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return projection.Event{
		EventID: eventID, EventType: eventType, TenantID: tenantID,
		LegalEntityID: "entity-1", EmittedAt: emitted, Payload: b,
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestApply_IndexesARegisteredEvent(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1", "obligation_code": "GST-Q4"}))

	require.NoError(t, err)
	assert.Equal(t, OutcomeIndexed, outcome)
	assert.Len(t, eng.docs, 1)
}

// An event type no projector claims is skipped and COMMITTED. Treating it as
// an error would dead-letter every other domain's traffic on a shared topic.
func TestApply_UnhandledEventTypeIsSkippedNotFailed(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "invoice.posted", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"invoice_id": "inv-1"}))

	require.NoError(t, err)
	assert.Equal(t, OutcomeSkipped, outcome)
	assert.Empty(t, eng.docs)
}

// INV-02. An event with no trusted tenant is QUARANTINED — not retried, and
// not indexed under a guessed tenant.
func TestApply_EventWithNoTenantIsQuarantined(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))

	assert.Equal(t, OutcomeQuarantined, outcome)
	require.Error(t, err)
	assert.ErrorIs(t, err, projection.ErrNoTrustedTenant)
	assert.Empty(t, eng.docs, "nothing reaches the index without a trusted tenant")
	assert.Empty(t, st.ledger, "and nothing reaches the ledger either")
}

// §5.2's idempotency: the same event twice produces one indexed state, and
// the replay is reported as stale rather than as a second write.
func TestApply_ReplayOfTheSameVersionIsStale(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)
	emitted := time.Now().UTC()
	ev := event(t, "obligation.created", "e1", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST", "record_version": 1})

	first, err := ix.Apply(context.Background(), ev)
	require.NoError(t, err)
	assert.Equal(t, OutcomeIndexed, first)

	second, err := ix.Apply(context.Background(), ev)
	require.NoError(t, err)
	assert.Equal(t, OutcomeStale, second)
	assert.Equal(t, 1, eng.calls, "a stale replay must not reach the engine at all")
}

// A NEWER version does apply.
func TestApply_NewerVersionSupersedes(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)
	emitted := time.Now().UTC()

	_, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "OLD", "record_version": 1}))
	require.NoError(t, err)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.updated", "e2", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "NEW", "record_version": 2}))
	require.NoError(t, err)
	assert.Equal(t, OutcomeIndexed, outcome)

	doc := eng.docs["obligation-g1/tenant-a:obligation:ob-1"]
	assert.Equal(t, "NEW", doc.Fields["obligation_code"])
}

// NP-11 / NP-48, the headline case. A replayed create arriving AFTER a
// deletion must not resurrect the document.
func TestApply_ReplayAfterDeletionCannotResurrect(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)
	emitted := time.Now().UTC()

	_, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST", "record_version": 1}))
	require.NoError(t, err)

	_, err = ix.Apply(context.Background(), event(t, "obligation.deleted", "e2", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST", "record_version": 1}))
	require.NoError(t, err)
	assert.True(t, eng.docs["obligation-g1/tenant-a:obligation:ob-1"].Tombstoned)

	// The same create, redelivered after the deletion — a higher record
	// version, even, so ordinary version comparison would let it through. The
	// epoch comparison is what refuses it.
	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1-replay", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST", "record_version": 99}))
	require.NoError(t, err)

	assert.Equal(t, OutcomeStale, outcome, "a non-restriction event cannot outrank a tombstone")
	assert.True(t, eng.docs["obligation-g1/tenant-a:obligation:ob-1"].Tombstoned,
		"the document must still be tombstoned after the replay")
}

// A restriction writes a tombstone row and marks it APPLIED — never VERIFIED,
// because nothing has tested invisibility yet (§2.2).
func TestApply_RestrictionIsAppliedButNotVerified(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)

	_, err := ix.Apply(context.Background(), event(t, "obligation.deleted", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1"}))
	require.NoError(t, err)

	require.Len(t, st.tombs, 1)
	assert.Equal(t, domain.PropagationApplied, st.states["tenant-a|obligation|ob-1|e1"])
	assert.NotEqual(t, domain.PropagationVerified, st.states["tenant-a|obligation|ob-1|e1"])
}

// The ledger is written BEFORE the engine. A ledger failure must leave the
// index untouched, because the ledger's compare-and-set is the concurrency
// control — writing the engine first would change what is searchable before
// anything checked whether it should.
func TestApply_LedgerFailureLeavesTheIndexUntouched(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	st.upsertErr = errors.New("database unavailable")
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))

	assert.Equal(t, OutcomeError, outcome)
	require.Error(t, err)
	assert.Zero(t, eng.calls, "the engine must not be written when the ledger refused")
}

// NP-51. A strict-mapping rejection is a contract violation: quarantined, not
// retried. Retrying would produce the same rejection forever and block the
// partition behind one document.
func TestApply_StrictMappingRejectionIsQuarantinedNotRetried(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	eng.err = searchclient.ErrStrictMappingRejected
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))

	assert.Equal(t, OutcomeQuarantined, outcome)
	require.Error(t, err)
}

// A transient engine failure IS retryable, and is reported as such so the
// Kafka runner retries and then dead-letters rather than dropping it.
func TestApply_TransientEngineFailureIsRetryable(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	eng.err = errors.New("connection reset")
	ix := withLiveScope(st, eng)

	outcome, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a",
		time.Now().UTC(), map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))

	assert.Equal(t, OutcomeError, outcome)
	require.Error(t, err)
}

// §8.2's verification proves invisibility independently of the write that
// applied it. A tombstoned document verifies; a still-visible one does not
// and is marked FAILED rather than quietly passed.
func TestVerifyRestrictions_VerifiesInvisibleAndFailsVisible(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)
	emitted := time.Now().UTC()

	_, err := ix.Apply(context.Background(), event(t, "obligation.deleted", "e-gone", "tenant-a", emitted,
		map[string]any{"obligation_id": "ob-gone"}))
	require.NoError(t, err)

	ix.VerifyRestrictions(context.Background())
	assert.Equal(t, domain.PropagationVerified, st.states["tenant-a|obligation|ob-gone|e-gone"])

	// Now a tombstone whose document is, contrary to the tombstone, still
	// visible in the index — the over-disclosure §8.2 escalates on.
	st.tombs = append(st.tombs, domain.RestrictionTombstone{
		TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-visible", Reason: "erasure", Epoch: emitted.UnixMilli(),
		SourceEventID: "e-visible", EffectiveAt: emitted, State: domain.PropagationApplied,
	})
	eng.docs["obligation-g1/tenant-a:obligation:ob-visible"] = searchclient.Projection{
		DocID: "tenant-a:obligation:ob-visible", SourceID: "ob-visible", Tombstoned: false,
	}

	ix.VerifyRestrictions(context.Background())
	assert.Equal(t, domain.PropagationFailed, st.states["tenant-a|obligation|ob-visible|e-visible"],
		"content still discoverable after propagation must be FAILED, never VERIFIED")
}

// NP-60. A tombstone for a scope with no active generation is FAILED, not
// VERIFIED: "there is nothing to check" is not the same claim as "we checked
// and it is gone", and an unproven state may not be relabelled as a pass.
func TestVerifyRestrictions_UnverifiableScopeIsFailedNotPassed(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := New(st, eng, nil, testMetrics(), zap.NewNop()) // no live scopes at all

	st.tombs = append(st.tombs, domain.RestrictionTombstone{
		TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation",
		SourceID: "ob-1", SourceEventID: "e1", EffectiveAt: time.Now().UTC(),
		State: domain.PropagationApplied,
	})

	ix.VerifyRestrictions(context.Background())
	assert.Equal(t, domain.PropagationFailed, st.states["tenant-a|obligation|ob-1|e1"])
}

// sourceTypeOf follows the estate's event-naming convention, which is what
// binds a topic's events to a projector without a per-event mapping table.
func TestSourceTypeOf(t *testing.T) {
	cases := map[string]string{
		"obligation.created":        "obligation",
		"purchase.request.approved": "purchase",
		"contract.signed":           "contract",
		"bare":                      "bare",
	}
	for eventType, want := range cases {
		assert.Equal(t, want, sourceTypeOf(projection.Event{EventType: eventType}), eventType)
	}
}
