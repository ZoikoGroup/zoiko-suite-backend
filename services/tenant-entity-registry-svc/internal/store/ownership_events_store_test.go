package store_test

// ORG-04 wave 3 against a real Postgres: every state change writes its event in
// the same transaction, and the group structure can be rebuilt for any past date
// from what was actually stored.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// withOwnershipEvents is what the service attaches to every ORG-04 write.
func withOwnershipEvents(f *orgFixture) context.Context {
	return registry.WithEvent(f.ctx, events.OwnershipRecord)
}

// ownershipEvents returns the ORG-04 envelopes written for the fixture's
// tenant, oldest first.
func ownershipEvents(t *testing.T, f *orgFixture) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT payload FROM event_outbox
		 WHERE tenant_id = $1 AND (event_type LIKE 'ownership.relationship.%' OR event_type = 'control.relationship.changed')
		 ORDER BY created_at`, f.tenantID)
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		out = append(out, decodeRaw(t, raw))
	}
	return out
}

func eventTypes(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["event_type"].(string))
	}
	return out
}

func TestOwnershipEvents_EachStateChangeWritesItsEventInTheSameTransaction(t *testing.T) {
	f := newORGFixture(t)
	ctx := withOwnershipEvents(f)
	e := entities(t, f, 2)

	r := newRelationship(f, f.entityID, e[0])
	r.VotingPct = nil
	r.ValidFrom = at(0)
	require.NoError(t, f.s.CreateOwnershipRelationship(ctx, r))
	_, err := f.s.DecideOwnershipRelationship(ctx, r.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "corr-approve")
	require.NoError(t, err)
	_, err = f.s.EndOwnershipRelationship(ctx, r.RelationshipID, 2, at(60), "p-ender", "sold", "disposal-1", "corr-end")
	require.NoError(t, err)

	rejected := newRelationship(f, f.entityID, e[1])
	rejected.VotingPct = nil
	rejected.ValidFrom = at(0)
	require.NoError(t, f.s.CreateOwnershipRelationship(ctx, rejected))
	_, err = f.s.DecideOwnershipRelationship(ctx, rejected.RelationshipID, 1, domain.OwnershipRejected, "p-checker", "no filing", "")
	require.NoError(t, err)

	evs := ownershipEvents(t, f)
	assert.Equal(t, []string{
		"ownership.relationship.proposed",
		"ownership.relationship.activated",
		"ownership.relationship.ended",
		"ownership.relationship.proposed",
		"ownership.relationship.rejected",
	}, eventTypes(evs))

	// Each carries the version the write left behind: the value GET returns.
	for i, want := range []float64{1, 2, 3, 1, 2} {
		assert.EqualValues(t, want, evs[i]["object_version"], "event %d", i)
	}
	activated := evs[1]
	assert.Equal(t, r.RelationshipID, activated["object_id"])
	assert.Equal(t, "p-checker", activated["actor_id"])
	assert.Equal(t, "corr-approve", activated["correlation_id"])
	assert.Equal(t, "filing-1", activated["evidence_ref"])
	assert.NotEmpty(t, activated["recorded_at"])
	ended := evs[2]
	assert.Equal(t, "p-ender", ended["actor_id"])
	assert.Equal(t, "disposal-1", ended["evidence_ref"])
}

func TestOwnershipEvents_ControlRelationshipsEmitControlRelationshipChanged(t *testing.T) {
	f := newORGFixture(t)
	ctx := withOwnershipEvents(f)
	e := entities(t, f, 1)
	r := newRelationship(f, f.entityID, e[0])
	board := domain.ControlBoard
	r.RelationshipKind, r.OwnershipPct, r.VotingPct, r.ControlType = domain.RelationshipControl, nil, nil, &board
	require.NoError(t, f.s.CreateOwnershipRelationship(ctx, r))
	_, err := f.s.DecideOwnershipRelationship(ctx, r.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"ownership.relationship.proposed", "control.relationship.changed"}, eventTypes(ownershipEvents(t, f)))
	payload := ownershipEvents(t, f)[1]["payload"].(map[string]any)
	assert.Equal(t, "ACTIVATED", payload["change_type"])
	assert.Equal(t, "BOARD", payload["control_type"])
}

// A supersession retires one relationship and activates another: two events,
// one transaction, each pinned to its own relationship and version.
func TestOwnershipEvents_SupersessionWritesBothEventsAtomically(t *testing.T) {
	f := newORGFixture(t)
	ctx := withOwnershipEvents(f)
	e := entities(t, f, 1)
	old := activated(t, f, f.entityID, e[0], 60, 0) // wave-2 helper: no event hook attached

	n := newRelationship(f, f.entityID, e[0])
	n.OwnershipPct, n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = f64(80), nil, at(100), &old.RelationshipID
	n.EvidenceRef = "filing-2"
	require.NoError(t, f.s.CreateOwnershipRelationship(ctx, n))
	_, err := f.s.DecideOwnershipRelationship(ctx, n.RelationshipID, 1, domain.OwnershipActive, "p-approver-2", "", "corr-2")
	require.NoError(t, err)

	evs := ownershipEvents(t, f)
	// The activation and the retirement commit together, so they share a
	// timestamp and their relative order is not defined; the proposal is first.
	require.Len(t, evs, 3)
	assert.Equal(t, "ownership.relationship.proposed", eventTypes(evs)[0])
	assert.ElementsMatch(t, []string{"ownership.relationship.activated", "ownership.relationship.superseded"}, eventTypes(evs)[1:])

	superseded := eventOfType(t, evs, "ownership.relationship.superseded")
	assert.Equal(t, old.RelationshipID, superseded["object_id"])
	assert.EqualValues(t, old.RecordVersion+1, superseded["object_version"])
	assert.Equal(t, "p-approver-2", superseded["actor_id"], "attributed to whoever approved the replacement")
	assert.Equal(t, "filing-2", superseded["evidence_ref"])
	assert.Equal(t, n.RelationshipID, superseded["payload"].(map[string]any)["replaced_by_relationship_id"])
	assert.Equal(t, n.ValidFrom.UTC().Format("2006-01-02T15:04:05Z"), superseded["effective_at"], "stops where the replacement starts")
	assert.Equal(t, old.RelationshipID, eventOfType(t, evs, "ownership.relationship.activated")["payload"].(map[string]any)["supersedes_relationship_id"])
}

// The event is part of the fact: if it cannot be produced, the fact is not
// committed, and a refused write produces no event.
func TestOwnershipEvents_AreAtomicWithTheFact(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)

	t.Run("an event that cannot be rendered aborts the proposal", func(t *testing.T) {
		broken := registry.WithEvent(f.ctx, func(any) (*outbox.Record, error) { return nil, errors.New("render failed") })
		r := newRelationship(f, f.entityID, e[0])
		require.Error(t, f.s.CreateOwnershipRelationship(broken, r))
		got, _ := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
		assert.Nil(t, got, "no relationship without its event")
	})
	t.Run("an event that cannot be rendered aborts the approval", func(t *testing.T) {
		r := proposed(t, f, f.entityID, e[0], 60, 0)
		broken := registry.WithEvent(f.ctx, func(any) (*outbox.Record, error) { return nil, errors.New("render failed") })
		_, err := f.s.DecideOwnershipRelationship(broken, r.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
		require.Error(t, err)
		got, _ := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
		assert.Equal(t, domain.OwnershipProposed, got.Status)
		assert.EqualValues(t, 1, got.RecordVersion)
	})
	t.Run("a refused approval emits nothing", func(t *testing.T) {
		ctx := withOwnershipEvents(f)
		// Proposed while nothing is active, so it is accepted; then the opposite
		// edge becomes active, and approving this one would close a loop.
		reverse := proposed(t, f, f.entityID, e[0], 50, 5)
		activated(t, f, e[0], f.entityID, 50, 0)
		n := len(ownershipEvents(t, f))
		_, err := f.s.DecideOwnershipRelationship(ctx, reverse.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
		require.ErrorIs(t, err, domain.ErrOwnershipCycle)
		assert.Equal(t, n, len(ownershipEvents(t, f)), "a rolled-back approval must leave no event")
	})
}

// The structure on a past date is rebuilt from what was stored, and agrees with
// what the events said at the time.
func TestOwnershipReads_RebuildThePastFromStoredRows(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 2)
	parent, child, other := f.entityID, e[0], e[1]

	original := activated(t, f, parent, child, 60, 0)
	// Replace it from day 100 with 80%.
	n := newRelationship(f, parent, child)
	n.OwnershipPct, n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = f64(80), nil, at(100), &original.RelationshipID
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, n))
	_, err := f.s.DecideOwnershipRelationship(f.ctx, n.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
	require.NoError(t, err)
	// A second holding, ended on day 150.
	side := activated(t, f, other, child, 20, 40)
	_, err = f.s.EndOwnershipRelationship(f.ctx, side.RelationshipID, side.RecordVersion, at(150), "p-ender", "sold", "ev", "")
	require.NoError(t, err)
	// A proposal that never took effect, and a rejected one.
	proposed(t, f, parent, other, 10, 0)

	read := func(day int) map[string]float64 {
		rels, err := f.s.ListOwnershipInForceAt(f.ctx, at(day))
		require.NoError(t, err)
		got := map[string]float64{}
		for _, r := range domain.GraphAsOf(rels, at(day), "").Relationships {
			got[r.ParentLegalEntityID] += *r.OwnershipPct
		}
		return got
	}

	assert.Equal(t, map[string]float64{parent: 60}, read(10), "before the second holding: the original terms")
	assert.Equal(t, map[string]float64{parent: 60, other: 20}, read(50))
	assert.Equal(t, map[string]float64{parent: 80, other: 20}, read(120), "after the supersession: the new terms")
	assert.Equal(t, map[string]float64{parent: 80}, read(200), "after the second holding ended")
	assert.Empty(t, read(-1), "before anything")
	assert.Empty(t, mapToList(read(10), other), "the pending proposal never counted")

	// Control chain at two dates, from the same rows.
	rels, _ := f.s.ListOwnershipInForceAt(f.ctx, at(10))
	chain := domain.ExplainControlChain(rels, at(10), child)
	require.Len(t, chain.Chains, 1)
	assert.InDelta(t, 60, chain.Chains[0].EffectiveOwnershipPct, 0.0001)
	rels, _ = f.s.ListOwnershipInForceAt(f.ctx, at(200))
	chain = domain.ExplainControlChain(rels, at(200), child)
	assert.InDelta(t, 80, chain.Chains[0].EffectiveOwnershipPct, 0.0001)

	// The superseded original is still retrievable, with its own terms.
	old, err := f.s.GetOwnershipRelationship(f.ctx, original.RelationshipID)
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipSuperseded, old.Status)
	assert.InDelta(t, 60, *old.OwnershipPct, 0.0001)
}

func mapToList(m map[string]float64, key string) []float64 {
	if v, ok := m[key]; ok {
		return []float64{v}
	}
	return nil
}

func TestOwnershipReads_AreTenantScoped(t *testing.T) {
	a := newORGFixture(t)
	e := entities(t, a, 1)
	activated(t, a, a.entityID, e[0], 60, 0)
	b := secondTenant(t, a)

	rels, err := b.s.ListOwnershipInForceAt(b.ctx, at(10))
	require.NoError(t, err)
	assert.Empty(t, rels, "tenant B sees nothing of tenant A's structure")
	rels, err = a.s.ListOwnershipInForceAt(a.ctx, at(10))
	require.NoError(t, err)
	assert.Len(t, rels, 1)
}

func decodeRaw(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	env := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env
}

func eventOfType(t *testing.T, evs []map[string]any, eventType string) map[string]any {
	t.Helper()
	for _, e := range evs {
		if e["event_type"] == eventType {
			return e
		}
	}
	t.Fatalf("no %s event among %v", eventType, eventTypes(evs))
	return nil
}
