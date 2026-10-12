package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/projection"
)

type fakeReplayer struct{ events []projection.Event }

func (f fakeReplayer) Replay(_ context.Context, _ string, fn func(projection.Event) error) (int, error) {
	for _, e := range f.events {
		if err := fn(e); err != nil {
			return 0, err
		}
	}
	return len(f.events), nil
}

// A registry with v1 ACTIVE and v2 BUILDING, each with its own contract.
func migratingStore() (*fakeStore, domain.IndexContract, domain.IndexContract) {
	st := newFakeStore()
	st.sources = []domain.SearchSource{source()}
	v1 := contract()
	v1.State = domain.ContractRetired
	v2 := contract()
	v2.ContractID, v2.Version, v2.State = "c-2", 2, domain.ContractPublished
	v2.Fields = append(v2.Fields, domain.SearchFieldDefinition{FieldID: "obligation_status", SourcePath: "obligation_status",
		Type: "KEYWORD", Returnable: true, SensitivityClass: domain.SensitivityInternal})
	st.contracts = []domain.IndexContract{v1, v2}
	st.generations = []domain.IndexGeneration{
		{GenerationID: "g-1", ContractID: "c-1", ScopeName: "obligation", PhysicalIndex: "obligation-g1", State: domain.GenerationActive},
		{GenerationID: "g-2", ContractID: "c-2", ScopeName: "obligation", PhysicalIndex: "obligation-g2", State: domain.GenerationBuilding,
			BackfillState: domain.BackfillPending},
	}
	return st, v1, v2
}

func docIn(eng *fakeEngine, index, sourceID string) (searchclient.Projection, bool) {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	p, ok := eng.docs[index+"/"+searchclient.ProjectionDocID("tenant-a", "obligation", sourceID)]
	return p, ok
}

// Each generation is projected with ITS OWN contract, and a live event is
// written into the active generation and the candidate alike. Before, the
// published v2 contract was projected into v1's index.
func TestReload_EachGenerationUsesItsOwnContractAndLiveEventsReachCandidates(t *testing.T) {
	st, _, _ := migratingStore()
	eng := newFakeEngine()
	ix := New(st, eng, nil, testMetrics(), zap.NewNop())
	require.NoError(t, ix.Reload(context.Background()))

	out, err := ix.Apply(context.Background(), event(t, "obligation.created", "e1", "tenant-a", time.Now(),
		map[string]any{"obligation_id": "ob-1", "obligation_code": "GST", "obligation_status": "OPEN"}))
	require.NoError(t, err)
	assert.Equal(t, OutcomeIndexed, out)

	v1doc, ok := docIn(eng, "obligation-g1", "ob-1")
	require.True(t, ok)
	assert.NotContains(t, v1doc.Fields, "obligation_status", "v1's index never receives a v2-only field")
	v2doc, ok := docIn(eng, "obligation-g2", "ob-1")
	require.True(t, ok, "the candidate is written in parallel")
	assert.Equal(t, "OPEN", v2doc.Fields["obligation_status"])
}

// The ledger decides each record's state in the backfill.
func TestBackfill_LedgerIsTheAuthority(t *testing.T) {
	st, _, _ := migratingStore()
	eng := newFakeEngine()
	ix := New(st, eng, nil, testMetrics(), zap.NewNop())
	require.NoError(t, ix.Reload(context.Background()))

	t0 := time.Now().Add(-time.Hour)
	older := event(t, "obligation.created", "e-old", "tenant-a", t0, map[string]any{"obligation_id": "ob-1", "obligation_code": "OLD"})
	current := event(t, "obligation.created", "e-new", "tenant-a", t0.Add(time.Minute), map[string]any{"obligation_id": "ob-1", "obligation_code": "NEW"})
	erased := event(t, "obligation.created", "e-2", "tenant-a", t0, map[string]any{"obligation_id": "ob-2", "obligation_code": "ERASED"})
	unknown := event(t, "obligation.created", "e-3", "tenant-a", t0, map[string]any{"obligation_id": "ob-3", "obligation_code": "NEVER"})

	st.ledger[ledgerKey(domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1"})] =
		domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1",
			SourceVersion: t0.Add(time.Minute).UnixMilli()}
	// ob-2 was erased through POST /v1/restrictions: tombstoned in the ledger,
	// and no restriction EVENT exists in the topic.
	st.ledger[ledgerKey(domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-2"})] =
		domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-2",
			SourceVersion: t0.UnixMilli(), RestrictionEpoch: 99, Tombstoned: true}

	ix.SetReplayer(fakeReplayer{events: []projection.Event{older, current, erased, unknown}})
	var target *liveScope
	for _, c := range ix.candidates["obligation"] {
		target = c
	}
	require.NotNil(t, target)
	ix.backfill(context.Background(), target)

	doc, ok := docIn(eng, "obligation-g2", "ob-1")
	require.True(t, ok)
	assert.Equal(t, "NEW", doc.Fields["obligation_code"], "only the version the ledger holds is written")

	doc, ok = docIn(eng, "obligation-g2", "ob-2")
	require.True(t, ok)
	assert.True(t, doc.Tombstoned, "an erased record stays erased in the rebuild, even with no restriction event to replay")
	assert.Empty(t, doc.Fields)

	_, ok = docIn(eng, "obligation-g2", "ob-3")
	assert.False(t, ok, "a record the ledger never accepted is not invented by the replay")

	assert.Equal(t, domain.BackfillComplete, st.backfill["g-2"])
	assert.Contains(t, st.backfillNotes["g-2"], "written=1 tombstones=1")
}

// Create-only: the replay can never overwrite a newer live write.
func TestBackfill_NeverOverwritesALiveWrite(t *testing.T) {
	st, _, _ := migratingStore()
	eng := newFakeEngine()
	ix := New(st, eng, nil, testMetrics(), zap.NewNop())
	require.NoError(t, ix.Reload(context.Background()))

	t0 := time.Now().Add(-time.Hour)
	st.ledger[ledgerKey(domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1"})] =
		domain.ProjectionRecord{TenantID: "tenant-a", ScopeName: "obligation", SourceType: "obligation", SourceID: "ob-1",
			SourceVersion: t0.UnixMilli()}
	eng.docs["obligation-g2/"+searchclient.ProjectionDocID("tenant-a", "obligation", "ob-1")] =
		searchclient.Projection{SourceID: "ob-1", Fields: map[string]any{"obligation_code": "LIVE-NEWER"}}

	ix.SetReplayer(fakeReplayer{events: []projection.Event{
		event(t, "obligation.created", "e-old", "tenant-a", t0, map[string]any{"obligation_id": "ob-1", "obligation_code": "REPLAYED"}),
	}})
	ix.backfill(context.Background(), ix.candidates["obligation"][0])

	doc, _ := docIn(eng, "obligation-g2", "ob-1")
	assert.Equal(t, "LIVE-NEWER", doc.Fields["obligation_code"])
}

func TestBackfill_NoReplayerFails(t *testing.T) {
	st, _, _ := migratingStore()
	ix := New(st, newFakeEngine(), nil, testMetrics(), zap.NewNop())
	require.NoError(t, ix.Reload(context.Background()))
	ix.backfill(context.Background(), ix.candidates["obligation"][0])
	assert.Equal(t, domain.BackfillFailed, st.backfill["g-2"])
}

// Lag corners: nothing to measure, or a backlog with no readable age, is
// UNKNOWN — not CURRENT.
func TestCheckpoint_UnmeasurableCornersAreUnknown(t *testing.T) {
	st, eng := newFakeStore(), newFakeEngine()
	src := sourceWithMaxLag(300)
	ix := liveScopeFor(t, st, eng, contract(), src)
	ix.SetLagProbe(fakeLag{lag: SourceLag{Backlog: 5}})
	ix.RecordCheckpoints(context.Background())
	assert.Equal(t, domain.FreshnessUnknown, lastCheckpoint(t, st).Freshness)

	st2 := newFakeStore()
	src.EventTopic = ""
	ix2 := liveScopeFor(t, st2, newFakeEngine(), contract(), src)
	ix2.SetLagProbe(fakeLag{})
	ix2.RecordCheckpoints(context.Background())
	assert.Equal(t, domain.FreshnessUnknown, lastCheckpoint(t, st2).Freshness)
}
