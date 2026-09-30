package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/embedding"
	"zoiko.io/search-indexer-svc/internal/projection"
)

type fakeLag struct {
	lag SourceLag
	err error
}

func (f fakeLag) TopicLag(context.Context, string) (SourceLag, error) { return f.lag, f.err }

// liveScopeFor builds an Indexer over an arbitrary contract and source.
func liveScopeFor(t *testing.T, st *fakeStore, eng *fakeEngine, c domain.IndexContract, src domain.SearchSource) *Indexer {
	t.Helper()
	ix := New(st, eng, nil, testMetrics(), zap.NewNop())
	proj, err := projection.New(c, src)
	require.NoError(t, err)
	ix.bySourceType[src.SourceType] = &liveScope{projector: proj, generationID: "g-1", physicalIndex: "obligation-g1"}
	return ix
}

func sourceWithMaxLag(seconds int) domain.SearchSource {
	s := source()
	s.MaxLagSeconds = seconds
	return s
}

func lastCheckpoint(t *testing.T, st *fakeStore) domain.IndexCheckpoint {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	require.NotEmpty(t, st.checkpoints, "the sweep wrote no checkpoint")
	return st.checkpoints[len(st.checkpoints)-1]
}

// The defect the 25 Sep "fix" left: lag was time.Since(time.Now()), so a
// consumer hours behind read CURRENT. Measured at the broker, it is STALE.
func TestCheckpoint_ConsumerHoursBehindIsStale(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, contract(), sourceWithMaxLag(300))
	ix.SetLagProbe(fakeLag{lag: SourceLag{Partitions: 1, Backlog: 40, Watermark: 1000,
		OldestPending: time.Now().Add(-2 * time.Hour)}})

	ix.RecordCheckpoints(context.Background())

	cp := lastCheckpoint(t, st)
	assert.Equal(t, domain.FreshnessStale, cp.Freshness)
	assert.Greater(t, cp.LagMS, int64(time.Hour/time.Millisecond))
	assert.Equal(t, int64(1000), cp.Watermark, "the watermark is the broker position, not the wall clock")
}

// Past half the bound is LAGGING — surfaced, not yet a block.
func TestCheckpoint_PastHalfTheBoundIsLagging(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, contract(), sourceWithMaxLag(300))
	ix.SetLagProbe(fakeLag{lag: SourceLag{Backlog: 3, OldestPending: time.Now().Add(-200 * time.Second)}})

	ix.RecordCheckpoints(context.Background())
	assert.Equal(t, domain.FreshnessLagging, lastCheckpoint(t, st).Freshness)
}

// An idle source with nothing pending is CURRENT however old its last event.
func TestCheckpoint_CaughtUpIsCurrent(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, contract(), sourceWithMaxLag(300))
	ix.SetLagProbe(fakeLag{lag: SourceLag{Partitions: 1, Backlog: 0, Watermark: 7}})

	ix.RecordCheckpoints(context.Background())
	cp := lastCheckpoint(t, st)
	assert.Equal(t, domain.FreshnessCurrent, cp.Freshness)
	assert.Zero(t, cp.LagMS)
}

// §2.2: what cannot be measured is UNKNOWN, never CURRENT.
func TestCheckpoint_UnmeasurableIsUnknown(t *testing.T) {
	for name, probe := range map[string]LagProbe{
		"no probe":     nil,
		"probe failed": fakeLag{err: errors.New("broker unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			st, eng := newFakeStore(), newFakeEngine()
			ix := liveScopeFor(t, st, eng, contract(), sourceWithMaxLag(300))
			if probe != nil {
				ix.SetLagProbe(probe)
			}
			ix.RecordCheckpoints(context.Background())
			assert.Equal(t, domain.FreshnessUnknown, lastCheckpoint(t, st).Freshness)
		})
	}
}

// A population gap never upgrades a STALE scope to LAGGING: the worse wins.
func TestCheckpoint_PopulationGapDoesNotMaskStale(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	eng.docs["obligation-g1/x"] = searchclient.Projection{DocID: "x"} // engine 1, ledger 0
	ix := liveScopeFor(t, st, eng, contract(), sourceWithMaxLag(60))
	ix.SetLagProbe(fakeLag{lag: SourceLag{Backlog: 1, OldestPending: time.Now().Add(-10 * time.Minute)}})

	ix.RecordCheckpoints(context.Background())
	assert.Equal(t, domain.FreshnessStale, lastCheckpoint(t, st).Freshness)
}

// ── §10.1 embedding at index time ────────────────────────────────────────────

type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, _ domain.EmbeddingSpec, in []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = f.vec
	}
	return out, nil
}
func (f *fakeEmbedder) Configured() bool { return true }

func semanticContract() domain.IndexContract {
	c := contract()
	c.Embedding = &domain.EmbeddingSpec{Model: "m", ModelVersion: "1", Dimensions: 2,
		SourceFields: []string{"obligation_code"}, Preprocessing: embedding.ProfileNFKCWhitespaceV1, Similarity: "cosinesimil"}
	return c
}

func TestApply_SemanticScopeIndexesThePinnedVector(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, semanticContract(), source())
	emb := &fakeEmbedder{vec: []float32{0.1, 0.9}}
	ix.SetEmbedder(emb)

	out, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", time.Now(),
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST late filing"}))
	require.NoError(t, err)
	assert.Equal(t, OutcomeIndexed, out)

	doc := eng.docs["obligation-g1/"+searchclient.ProjectionDocID("tenant-a", "obligation", "ob-1")]
	assert.Equal(t, []float32{0.1, 0.9}, doc.Embedding)
	assert.Equal(t, "m@1", doc.EmbeddingModel)
}

// NP-35: a provider answering with another model blocks indexing — and blocks
// it BEFORE the ledger, so no ledger row claims a document the engine lacks.
func TestApply_ModelMismatchQuarantinesBeforeTheLedger(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, semanticContract(), source())
	ix.SetEmbedder(&fakeEmbedder{err: embedding.ErrModelMismatch})

	out, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", time.Now(),
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))
	require.Error(t, err)
	assert.Equal(t, OutcomeQuarantined, out)
	assert.Empty(t, st.ledger)
	assert.Empty(t, eng.docs)
}

// No provider is a transient error — retried, then dead-lettered — never a
// document silently indexed without its vector.
func TestApply_UnconfiguredProviderIsAnError(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, semanticContract(), source())

	out, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", time.Now(),
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST"}))
	require.Error(t, err)
	assert.Equal(t, OutcomeError, out)
	assert.Empty(t, st.ledger)
}

// A restriction never calls the provider and never carries a vector (NP-34).
func TestApply_RestrictionCarriesNoVector(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := liveScopeFor(t, st, eng, semanticContract(), source())
	emb := &fakeEmbedder{vec: []float32{1, 0}}
	ix.SetEmbedder(emb)

	_, err := ix.Apply(context.Background(), event(t, "obligation.deleted", "e2", "tenant-a", time.Now(),
		map[string]any{"obligation_id": "ob-1"}))
	require.NoError(t, err)
	assert.Zero(t, emb.calls)
	doc := eng.docs["obligation-g1/"+searchclient.ProjectionDocID("tenant-a", "obligation", "ob-1")]
	assert.Empty(t, doc.Embedding)
}

// NP-34: "verification checks both". A tombstoned document that still holds
// its vector is NOT invisible.
func TestVerify_TombstoneStillCarryingAVectorFails(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	ix := withLiveScope(st, eng)
	docID := searchclient.ProjectionDocID("tenant-a", "obligation", "ob-1")
	eng.raw = map[string]map[string]any{
		"obligation-g1/" + docID: {"tombstoned": true, "source_id": "ob-1", searchclient.EmbeddingVectorField: []any{0.1}},
	}
	st.tombs = []domain.RestrictionTombstone{{
		TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1",
		SourceEventID: "e2", EffectiveAt: time.Now(), State: domain.PropagationApplied,
	}}

	ix.VerifyRestrictions(context.Background())
	assert.Equal(t, domain.PropagationFailed, st.states["tenant-a|obligation|ob-1|e2"])
}

func TestSourceLag_ZeroWhenCaughtUp(t *testing.T) {
	now := time.Now()
	assert.Zero(t, SourceLag{Backlog: 0, OldestPending: now.Add(-time.Hour)}.Lag(now))
	assert.Equal(t, time.Minute, SourceLag{Backlog: 2, OldestPending: now.Add(-time.Minute)}.Lag(now))
}
