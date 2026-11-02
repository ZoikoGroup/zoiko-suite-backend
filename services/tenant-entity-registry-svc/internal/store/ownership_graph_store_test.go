package store_test

// ORG-04 wave 2 against a real Postgres: cycle and overlap rejection, the 100%
// ceiling, supersession, end, and the races the per-tenant lock exists for.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(days int) time.Time { return base.AddDate(0, 0, days) }

// proposed files a PROPOSED ownership relationship parent -> child.
func proposed(t *testing.T, f *orgFixture, parent, child string, pct float64, fromDay int) *domain.OwnershipRelationship {
	t.Helper()
	r := newRelationship(f, parent, child)
	r.OwnershipPct, r.VotingPct, r.ValidFrom = f64(pct), nil, at(fromDay)
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	return r
}

// activated files and approves one.
func activated(t *testing.T, f *orgFixture, parent, child string, pct float64, fromDay int) *domain.OwnershipRelationship {
	t.Helper()
	r := proposed(t, f, parent, child, pct, fromDay)
	out, err := f.s.DecideOwnershipRelationship(f.ctx, r.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
	require.NoError(t, err)
	return out
}

func approve(f *orgFixture, id string, version int64) error {
	_, err := f.s.DecideOwnershipRelationship(f.ctx, id, version, domain.OwnershipActive, "p-checker", "", "")
	return err
}

func entities(t *testing.T, f *orgFixture, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		out[i] = ownershipEntity(t, f)
	}
	return out
}

func TestOwnershipGraph_ProposalThatContradictsTheActiveStructureIsRefusedAtOnce(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 3)
	a, b, c := f.entityID, e[0], e[1]
	activated(t, f, a, b, 60, 0)
	activated(t, f, b, c, 60, 0)

	t.Run("a loop", func(t *testing.T) {
		r := newRelationship(f, c, a)
		err := f.s.CreateOwnershipRelationship(f.ctx, r)
		require.ErrorIs(t, err, registry.ErrOwnershipConflict)
		require.ErrorIs(t, err, domain.ErrOwnershipCycle)
		got, _ := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
		assert.Nil(t, got, "a refused proposal leaves nothing behind")
	})
	t.Run("an identical overlapping relationship", func(t *testing.T) {
		r := newRelationship(f, a, b)
		r.ValidFrom = at(30)
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, r), domain.ErrOwnershipOverlap)
	})
	t.Run("owners above 100 percent", func(t *testing.T) {
		r := newRelationship(f, e[2], c)
		r.OwnershipPct, r.VotingPct, r.ValidFrom = f64(41), nil, at(0)
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, r), domain.ErrOwnershipExceeds)
	})
	t.Run("exactly 100 percent is fine", func(t *testing.T) {
		r := newRelationship(f, e[2], c)
		r.OwnershipPct, r.VotingPct, r.ValidFrom = f64(40), nil, at(0)
		require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	})
}

// The structure can move between a proposal and its approval, so approval
// checks again.
func TestOwnershipGraph_ApprovalChecksTheStructureAsItIsNow(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	a, b := f.entityID, e[0]

	reverse := proposed(t, f, b, a, 50, 0) // fine: nothing is active yet
	activated(t, f, a, b, 50, 0)           // now A owns B

	err := approve(f, reverse.RelationshipID, 1)
	require.ErrorIs(t, err, domain.ErrOwnershipCycle)
	got, _ := f.s.GetOwnershipRelationship(f.ctx, reverse.RelationshipID)
	assert.Equal(t, domain.OwnershipProposed, got.Status, "a refused approval changes nothing")
	assert.EqualValues(t, 1, got.RecordVersion)
}

// Two proposals that are each fine alone and together a loop: A->B and B->A
// approved at the same moment. Without the per-tenant lock both pass their
// check and both commit.
func TestOwnershipGraph_ConcurrentApprovalsCannotFormACycle(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	a, b := f.entityID, e[0]
	ab := proposed(t, f, a, b, 50, 0)
	ba := proposed(t, f, b, a, 50, 0)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{ab.RelationshipID, ba.RelationshipID} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = approve(f, id, 1) }()
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, domain.ErrOwnershipCycle)
		}
	}
	assert.Equal(t, 1, wins, "exactly one of two opposite edges may become active")
}

// Three owners at 50% each of one entity: the third cannot be admitted, however
// the approvals interleave.
func TestOwnershipGraph_ConcurrentApprovalsCannotExceed100Percent(t *testing.T) {
	f := newORGFixture(t)
	owners := entities(t, f, 3)
	child := f.entityID
	var ids []string
	for _, owner := range owners {
		ids = append(ids, proposed(t, f, owner, child, 50, 0).RelationshipID)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = approve(f, id, 1) }()
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, domain.ErrOwnershipExceeds)
		}
	}
	assert.Equal(t, 2, wins, "two 50% holders fit; the third does not")
}

func TestOwnershipSupersede_ApprovalRetiresTheOriginalAtTheReplacementStart(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	old := activated(t, f, f.entityID, e[0], 60, 0)

	n := newRelationship(f, f.entityID, e[0])
	n.OwnershipPct, n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = f64(80), nil, at(100), &old.RelationshipID
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, n), "the replacement may overlap the original it replaces")

	// Until approval the original stands, untouched.
	cur, _ := f.s.GetOwnershipRelationship(f.ctx, old.RelationshipID)
	assert.Equal(t, domain.OwnershipActive, cur.Status)
	assert.Nil(t, cur.ValidTo)

	out, err := f.s.DecideOwnershipRelationship(f.ctx, n.RelationshipID, 1, domain.OwnershipActive, "p-checker", "", "")
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipActive, out.Status)

	cur, _ = f.s.GetOwnershipRelationship(f.ctx, old.RelationshipID)
	assert.Equal(t, domain.OwnershipSuperseded, cur.Status)
	require.NotNil(t, cur.ValidTo)
	assert.True(t, cur.ValidTo.Equal(at(100)), "the original ends exactly where its replacement starts")
	assert.EqualValues(t, old.RecordVersion+1, cur.RecordVersion)
	assert.InDelta(t, 60, *cur.OwnershipPct, 0.0001, "the superseded terms are preserved, not overwritten")
}

func TestOwnershipSupersede_Refusals(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 2)
	old := activated(t, f, f.entityID, e[0], 60, 0)

	replacement := func(from int) *domain.OwnershipRelationship {
		n := newRelationship(f, f.entityID, e[0])
		n.OwnershipPct, n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = f64(70), nil, at(from), &old.RelationshipID
		return n
	}

	t.Run("only one replacement may be pending", func(t *testing.T) {
		require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, replacement(50)))
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, replacement(60)), registry.ErrConflict)
	})
	t.Run("the replacement must keep parent, child and kind", func(t *testing.T) {
		n := replacement(70)
		n.ChildLegalEntityID = e[1]
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, n), registry.ErrInvalidInput)
	})
	t.Run("an unknown original", func(t *testing.T) {
		n := replacement(70)
		missing := uuid.New().String()
		n.SupersedesRelationshipID = &missing
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, n), registry.ErrNotFound)
	})
	t.Run("a rejected relationship cannot be superseded", func(t *testing.T) {
		p := proposed(t, f, f.entityID, e[1], 10, 0)
		_, err := f.s.DecideOwnershipRelationship(f.ctx, p.RelationshipID, 1, domain.OwnershipRejected, "p-checker", "", "")
		require.NoError(t, err)
		n := newRelationship(f, f.entityID, e[1])
		n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = nil, at(10), &p.RelationshipID
		require.ErrorIs(t, f.s.CreateOwnershipRelationship(f.ctx, n), registry.ErrInvalidTransition)
	})
}

// The original is ended while its replacement waits: approving the replacement
// must not resurrect or double-count anything.
func TestOwnershipSupersede_ApprovalFailsIfTheOriginalMovedOn(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	old := activated(t, f, f.entityID, e[0], 60, 0)

	n := newRelationship(f, f.entityID, e[0])
	n.VotingPct, n.ValidFrom, n.SupersedesRelationshipID = nil, at(100), &old.RelationshipID
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, n))

	_, err := f.s.EndOwnershipRelationship(f.ctx, old.RelationshipID, old.RecordVersion, at(40), "p-ender", "sold", "agreement", "")
	require.NoError(t, err)

	require.Error(t, approve(f, n.RelationshipID, 1))
	got, _ := f.s.GetOwnershipRelationship(f.ctx, n.RelationshipID)
	assert.Equal(t, domain.OwnershipProposed, got.Status, "the failed approval rolled back entirely")
}

func TestOwnershipEnd(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	r := activated(t, f, f.entityID, e[0], 60, 0)

	t.Run("refusals change nothing", func(t *testing.T) {
		_, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, 9, at(10), "p-ender", "r", "e", "")
		require.ErrorIs(t, err, registry.ErrVersionConflict)
		_, err = f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, r.RecordVersion, at(0), "p-ender", "r", "e", "")
		require.ErrorIs(t, err, registry.ErrInvalidInput, "not after valid_from")
		_, err = f.s.EndOwnershipRelationship(f.ctx, uuid.New().String(), 1, at(10), "p-ender", "r", "e", "")
		require.ErrorIs(t, err, registry.ErrNotFound)
		got, _ := f.s.GetOwnershipRelationship(f.ctx, r.RelationshipID)
		assert.Equal(t, domain.OwnershipActive, got.Status)
	})
	t.Run("ends and records who, when, why and on what evidence", func(t *testing.T) {
		out, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, r.RecordVersion, at(50), "p-ender", "stake sold", "disposal-1", "corr-end")
		require.NoError(t, err)
		assert.Equal(t, domain.OwnershipEnded, out.Status)
		assert.True(t, out.ValidTo.Equal(at(50)))
		assert.Equal(t, "p-ender", *out.EndedByPrincipalID)
		assert.Equal(t, "stake sold", *out.EndReason)
		assert.Equal(t, "disposal-1", *out.EndEvidenceRef)
		assert.NotNil(t, out.EndedAt)
		assert.EqualValues(t, r.RecordVersion+1, out.RecordVersion)
	})
	t.Run("an ended relationship cannot be ended again", func(t *testing.T) {
		_, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, r.RecordVersion+1, at(60), "p-ender", "r", "e", "")
		require.ErrorIs(t, err, registry.ErrInvalidTransition)
	})
	t.Run("ending frees the identity for a later relationship", func(t *testing.T) {
		next := proposed(t, f, f.entityID, e[0], 60, 50)
		require.NoError(t, approve(f, next.RelationshipID, 1))
	})
}

func TestOwnershipEnd_AnEndCanOnlyBringTheEndForward(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	r := newRelationship(f, f.entityID, e[0])
	r.VotingPct, r.ValidFrom = nil, at(0)
	to := at(100)
	r.ValidTo = &to
	require.NoError(t, f.s.CreateOwnershipRelationship(f.ctx, r))
	require.NoError(t, approve(f, r.RelationshipID, 1))

	_, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, 2, at(150), "p-ender", "r", "e", "")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "after the existing valid_to")
	out, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, 2, at(60), "p-ender", "r", "e", "")
	require.NoError(t, err)
	assert.True(t, out.ValidTo.Equal(at(60)))
}

// The table itself refuses an ENDED row with no evidence and any other row
// carrying end fields, so a writer that skips the service cannot fake or hide
// an end.
func TestOwnershipEnd_TheTableEnforcesTheEvidence(t *testing.T) {
	f := newORGFixture(t)
	e := entities(t, f, 1)
	r := activated(t, f, f.entityID, e[0], 60, 0)
	ctx := context.Background()

	_, err := f.pool.Exec(ctx, `UPDATE ownership_relationships SET status = 'ENDED' WHERE relationship_id = $1`, r.RelationshipID)
	require.Error(t, err, "ENDED with no who, why or evidence")
	assert.Contains(t, err.Error(), "or_end_recorded")

	_, err = f.pool.Exec(ctx, `UPDATE ownership_relationships SET end_reason = 'x' WHERE relationship_id = $1`, r.RelationshipID)
	require.Error(t, err, "an ACTIVE row carrying end fields")

	_, err = f.pool.Exec(ctx, `UPDATE ownership_relationships SET supersedes_relationship_id = relationship_id WHERE relationship_id = $1`, r.RelationshipID)
	require.Error(t, err, "a relationship cannot supersede itself")
}

// 000015 refuses to drop history it would destroy, and reverses cleanly when
// there is none.
func TestOwnership_Migration000015DownIsRefusedWhileHistoryExists(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	e := entities(t, f, 1)
	r := activated(t, f, f.entityID, e[0], 60, 0)
	_, err := f.s.EndOwnershipRelationship(f.ctx, r.RelationshipID, r.RecordVersion, at(10), "p-ender", "sold", "agreement", "")
	require.NoError(t, err)

	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}

	_, err = f.pool.Exec(ctx, read("000015_ownership_supersede_end.down.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "000015 down refused")

	_, err = f.pool.Exec(ctx, `DELETE FROM ownership_relationships`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, read("000015_ownership_supersede_end.down.sql"))
	require.NoError(t, err, "with no history the down migration applies")
	_, err = f.pool.Exec(ctx, read("000015_ownership_supersede_end.up.sql"))
	require.NoError(t, err, "and the up migration applies again")
}
