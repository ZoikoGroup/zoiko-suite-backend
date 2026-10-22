package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// POST /v1/legal-holds/{id}:resolve-targets (ZS-SVC-S-001 §9.2 / §5.6) against
// real Postgres: a transactional resolution that records an immutable snapshot
// and reports partial failures instead of hiding them.

const missingRecordID = "00000000-0000-0000-0000-000000000000"

func newHold(t *testing.T, s *store.PgStore, seed *domain.Record, activate bool) *domain.LegalHold {
	t.Helper()
	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-resolve", HoldReasonCode: "LITIGATION", RecordIDs: []string{seed.RecordID}, IssuedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	if activate {
		_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
		require.NoError(t, err)
	}
	return hold
}

func resolve(t *testing.T, s *store.PgStore, ctx context.Context, holdID string, ids ...string) (*domain.HoldResolution, error) {
	t.Helper()
	return s.ResolveLegalHoldTargets(ctx, domain.ResolveLegalHoldTargetsParams{HoldID: holdID, RecordIDs: ids, ResolvedByPrincipalID: "counsel-frank"})
}

func TestResolveTargets_ActiveHold_AppliesTargetsAndRecordsASnapshot(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	seed := createTestRecord(t, s, "res-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	a := createTestRecord(t, s, "res-a", string(domain.RecordClassLegalMatterRecord), "US-DE")
	b := createTestRecord(t, s, "res-b", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, true)
	clearOutbox(t, pool)

	got, err := resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID, b.RecordID)
	require.NoError(t, err)
	require.True(t, got.Complete)
	require.Empty(t, got.Unresolved)
	require.Equal(t, domain.LegalHoldActive, got.HoldStatus)
	require.Len(t, got.Resolved, 2)
	for _, r := range got.Resolved {
		assert.Equal(t, domain.TargetAdded, r.Outcome)
		assert.True(t, r.Applied, "an ACTIVE hold applies at once")
	}
	require.NotEmpty(t, got.ResolutionID)

	// The hold now blocks disposition of those records (DRC-I10).
	for _, rec := range []*domain.Record{a, b} {
		ev, err := s.EvaluateRetention(tenantCtx(), []string{rec.RecordID})
		require.NoError(t, err)
		assert.True(t, ev[0].LegalHold.Blocked)
		assert.Equal(t, []string{hold.HoldID}, ev[0].LegalHold.HoldIDs)
	}
	assert.Len(t, eventsOfType(t, pool, "legal_hold_target_added"), 2)
	assert.Empty(t, eventsOfType(t, pool, "legal_hold_partial_application"), "nothing unresolved, so no partial-application event")

	var snapshots int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM legal_hold_resolutions WHERE hold_id::text = $1`, hold.HoldID).Scan(&snapshots))
	assert.Equal(t, 1, snapshots)
}

func TestResolveTargets_DraftHold_QueuesTargetsUntilActivation(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	seed := createTestRecord(t, s, "res-d-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	a := createTestRecord(t, s, "res-d-a", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, false)

	got, err := resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID)
	require.NoError(t, err)
	require.Equal(t, domain.LegalHoldDraft, got.HoldStatus)
	require.Len(t, got.Resolved, 1)
	assert.False(t, got.Resolved[0].Applied, "a DRAFT hold does not yet preserve")
	ev, _ := s.EvaluateRetention(tenantCtx(), []string{a.RecordID})
	assert.False(t, ev[0].LegalHold.Blocked)

	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	ev, _ = s.EvaluateRetention(tenantCtx(), []string{a.RecordID})
	assert.True(t, ev[0].LegalHold.Blocked, "activation applies the resolved targets")
}

func TestResolveTargets_RepeatIsARefresh_NotADuplicate(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	seed := createTestRecord(t, s, "res-r-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	a := createTestRecord(t, s, "res-r-a", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, true)

	_, err := resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID)
	require.NoError(t, err)
	clearOutbox(t, pool)
	again, err := resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID, seed.RecordID)
	require.NoError(t, err)
	require.Len(t, again.Resolved, 2)
	for _, r := range again.Resolved {
		assert.Equal(t, domain.TargetAlreadyTarget, r.Outcome)
		assert.True(t, r.Applied)
	}
	assert.Empty(t, eventsOfType(t, pool, "legal_hold_target_added"), "no new target, no new event")

	var targets, snapshots int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM legal_hold_targets WHERE hold_id::text = $1`, hold.HoldID).Scan(&targets))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM legal_hold_resolutions WHERE hold_id::text = $1`, hold.HoldID).Scan(&snapshots))
	assert.Equal(t, 2, targets, "the seed target and one more; the repeat added none")
	assert.Equal(t, 2, snapshots, "each resolve call is its own snapshot")
}

func TestResolveTargets_PartialFailure_IsReported_NotHidden(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	seed := createTestRecord(t, s, "res-p-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	good := createTestRecord(t, s, "res-p-good", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, true)
	clearOutbox(t, pool)

	// Another tenant's record, a record that does not exist and a malformed id.
	foreign := func() *domain.Record {
		doc, v := newEventDoc(nil, "res-foreign")
		doc.TenantID = otherTenantID
		require.NoError(t, s.CreateDocument(otherTenantCtx(), doc, v, "c"))
		rec, _, err := s.DeclareRecordV2(otherTenantCtx(), domain.DeclareRecordV2Params{
			DocumentID: doc.DocumentID, DocumentVersionID: v.DocumentVersionID, LegalEntityID: doc.LegalEntityID,
			RecordClass: string(domain.RecordClassLegalMatterRecord), JurisdictionScope: "US-DE", DeclaredByPrincipalID: "p", DeclarationReason: "x", CorrelationID: "c"})
		require.NoError(t, err)
		return rec
	}()

	got, err := resolve(t, s, tenantCtx(), hold.HoldID, good.RecordID, missingRecordID, "not-a-uuid", foreign.RecordID)
	require.NoError(t, err, "a record that cannot be resolved does not fail the call")
	require.False(t, got.Complete)
	require.Len(t, got.Resolved, 1)
	assert.Equal(t, good.RecordID, got.Resolved[0].RecordID, "the resolvable record still became a target")
	require.Len(t, got.Unresolved, 3)
	var ids []string
	for _, u := range got.Unresolved {
		ids = append(ids, u.RecordID)
		assert.Equal(t, domain.UnresolvedRecordNotFound, u.Reason)
	}
	assert.ElementsMatch(t, []string{missingRecordID, "not-a-uuid", foreign.RecordID}, ids)

	// The other tenant's record was NOT swept into this tenant's hold.
	var cross int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM legal_hold_targets WHERE record_id::text = $1`, foreign.RecordID).Scan(&cross))
	assert.Zero(t, cross)

	partial := eventsOfType(t, pool, "legal_hold_partial_application")
	require.Len(t, partial, 1, "§9.3 legal_hold_partial_application")
	assert.Equal(t, hold.HoldID, partial[0].Payload["hold_id"])
	assert.ElementsMatch(t, []any{missingRecordID, "not-a-uuid", foreign.RecordID}, partial[0].Payload["unresolved_targets"])
	assert.Len(t, eventsOfType(t, pool, "legal_hold_target_added"), 1)

	var snap []byte
	var unresolved int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT snapshot, unresolved_count FROM legal_hold_resolutions WHERE resolution_id::text = $1`, got.ResolutionID).Scan(&snap, &unresolved))
	assert.Equal(t, 3, unresolved)
	assert.Contains(t, string(snap), good.RecordID)
}

func TestResolveTargets_RefusalsChangeNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	seed := createTestRecord(t, s, "res-x-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	a := createTestRecord(t, s, "res-x-a", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, true)

	_, err := resolve(t, s, tenantCtx(), hold.HoldID)
	require.ErrorIs(t, err, domain.ErrNoRecordIDsToResolve)
	_, err = resolve(t, s, tenantCtx(), missingRecordID, a.RecordID)
	require.ErrorIs(t, err, domain.ErrLegalHoldNotFound)
	_, err = resolve(t, s, otherTenantCtx(), hold.HoldID, a.RecordID)
	require.ErrorIs(t, err, domain.ErrLegalHoldNotFound, "another tenant's hold is indistinguishable from none")

	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "closed", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "director-dana"})
	require.NoError(t, err)
	_, err = resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID)
	require.ErrorIs(t, err, domain.ErrLegalHoldReleased, "a released hold takes no new targets")

	var snapshots int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM legal_hold_resolutions WHERE hold_id::text = $1`, hold.HoldID).Scan(&snapshots))
	assert.Zero(t, snapshots, "a refused call records no snapshot")
	ev, _ := s.EvaluateRetention(tenantCtx(), []string{a.RecordID})
	assert.False(t, ev[0].LegalHold.Blocked)
}

func TestResolveTargets_SnapshotsAreImmutable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	seed := createTestRecord(t, s, "res-i-seed", string(domain.RecordClassLegalMatterRecord), "US-DE")
	a := createTestRecord(t, s, "res-i-a", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold := newHold(t, s, seed, true)
	got, err := resolve(t, s, tenantCtx(), hold.HoldID, a.RecordID)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = pool.Exec(ctx, `UPDATE legal_hold_resolutions SET unresolved_count = 9 WHERE resolution_id::text = $1`, got.ResolutionID)
	require.Error(t, err, "a snapshot is never edited, even by a superuser")
	_, err = pool.Exec(ctx, `DELETE FROM legal_hold_resolutions WHERE resolution_id::text = $1`, got.ResolutionID)
	require.Error(t, err, "and never deleted")
	_ = time.Now
}
