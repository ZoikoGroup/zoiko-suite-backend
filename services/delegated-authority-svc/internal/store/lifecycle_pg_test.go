package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/store"
)

// These run as the NOSUPERUSER NOBYPASSRLS role requireTestDB switches to, so
// every RLS policy, CHECK, exclusion constraint and trigger is the database's
// answer — not a superuser's bypass of it.

func execTenant(t *testing.T, pool *pgxpool.Pool, tenant, sql string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant)
	require.NoError(t, err)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func countHistory(t *testing.T, pool *pgxpool.Pool, tenant, delegationID string) []string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant)
	require.NoError(t, err)
	rows, err := tx.Query(ctx, `SELECT transition FROM delegation_history WHERE delegation_id = $1 ORDER BY version`, delegationID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func proposal(id string, from, to time.Time) *domain.DelegationGrant {
	g := grant(id, "corr-"+id, testEntityA, principalSelf, principalOther, from, to)
	g.Status = domain.DelegationStatusProposed
	g.ApprovedByPrincipalID, g.ApprovedAt, g.ApprovalMethod = nil, nil, nil
	g.CreatedByPrincipalID = principalThird
	return g
}

// The exclusion constraint is the database's last word on overlap, and the
// window is half-open: back-to-back grants are not an overlap.
func TestOverlapIsRefusedByTheDatabaseAndAdjacencyIsNot(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC().Truncate(time.Second)

	_, err := s.CreateDelegation(ctx, grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour)))
	require.NoError(t, err)
	_, err = s.CreateDelegation(ctx, grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now.Add(time.Hour), now.Add(48*time.Hour)))
	require.ErrorIs(t, err, domain.ErrOverlapConflict, "the database must refuse an overlapping ACTIVE grant (23P01 → 409)")
	_, err = s.CreateDelegation(ctx, grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now.Add(24*time.Hour), now.Add(48*time.Hour)))
	require.NoError(t, err, "a grant starting where another ends is not an overlap ([from, to))")
}

func TestLifecycleProposeActivateSuspendResumeRevoke(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	id := uuid.NewString()

	_, err := s.CreateDelegation(ctx, proposal(id, now, now.Add(24*time.Hour)))
	require.NoError(t, err)
	require.Empty(t, outboxEvents(t, pool, testTenantA, id), "a proposal announces nothing")

	_, err = s.Transition(ctx, id, store.Activate, domain.TransitionInput{ExpectedVersion: 1, ActorPrincipalID: principalSelf, ApprovalMethod: domain.ApprovalDelegator})
	require.NoError(t, err)
	_, err = s.Transition(ctx, id, store.Suspend, domain.TransitionInput{ExpectedVersion: 1, ActorPrincipalID: principalSelf, Reason: "x"})
	require.ErrorIs(t, err, domain.ErrVersionMismatch, "a stale version is refused")
	_, err = s.Transition(ctx, id, store.Suspend, domain.TransitionInput{ActorPrincipalID: principalSelf, Reason: "x"})
	require.ErrorIs(t, err, domain.ErrVersionRequired)
	sus, err := s.Transition(ctx, id, store.Suspend, domain.TransitionInput{ExpectedVersion: 2, ActorPrincipalID: principalSelf, Reason: "investigation"})
	require.NoError(t, err)
	require.Equal(t, domain.DelegationStatusSuspended, sus.Status)
	_, err = s.Transition(ctx, id, store.Resume, domain.TransitionInput{ExpectedVersion: 3, ActorPrincipalID: principalSelf})
	require.NoError(t, err)
	rev, err := s.Transition(ctx, id, store.Revoke, domain.TransitionInput{ExpectedVersion: 4, ActorPrincipalID: principalOther, Reason: "no longer needed"})
	require.NoError(t, err)
	require.Equal(t, "no longer needed", *rev.RevocationReason)

	require.Equal(t, []string{"PROPOSED", "ACTIVATED", "SUSPENDED", "RESUMED", "REVOKED"}, countHistory(t, pool, testTenantA, id))
	require.Equal(t, []string{"authority.delegated", "authority.suspended", "authority.resumed", "authority.revoked"},
		outboxEvents(t, pool, testTenantA, id))
}

// The database enforces the rules even against a writer that skips the
// handler: approval segregation, a revocation reason, suspension evidence and
// an append-only history.
func TestDatabaseEnforcesEvidenceRules(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	id := uuid.NewString()
	_, err := s.CreateDelegation(ctx, proposal(id, now, now.Add(24*time.Hour)))
	require.NoError(t, err)

	require.Error(t, execTenant(t, pool, testTenantA, `UPDATE delegation_grants SET status = 'ACTIVE',
		approved_by_principal_id = created_by_principal_id, approved_at = now(), approval_method = 'ADMINISTRATOR_APPROVAL'
		WHERE delegation_id = $1`, id), "the maker may not approve their own proposal")
	require.Error(t, execTenant(t, pool, testTenantA, `UPDATE delegation_grants SET status = 'ACTIVE' WHERE delegation_id = $1`, id),
		"ACTIVE without approval evidence is refused")
	require.Error(t, execTenant(t, pool, testTenantA, `UPDATE delegation_grants SET status = 'REVOKED',
		revoked_at = now(), revoked_by_principal_id = 'x' WHERE delegation_id = $1`, id), "a revocation states its reason")
	require.Error(t, execTenant(t, pool, testTenantA, `UPDATE delegation_history SET reason = 'rewritten' WHERE delegation_id = $1`, id),
		"history is append-only")
	require.Error(t, execTenant(t, pool, testTenantA, `DELETE FROM delegation_history WHERE delegation_id = $1`, id),
		"history is append-only")
}

func TestActivateAndExtendRefuseALapsedWindow(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	id := uuid.NewString()
	_, err := s.CreateDelegation(ctx, proposal(id, now.Add(-2*time.Hour), now.Add(-time.Hour)))
	require.NoError(t, err)
	_, err = s.Transition(ctx, id, store.Activate, domain.TransitionInput{ExpectedVersion: 1, ActorPrincipalID: principalSelf, ApprovalMethod: domain.ApprovalDelegator})
	require.ErrorIs(t, err, domain.ErrLapsed, "no grace extension: a lapsed proposal is not activated")

	// And the sweep expires proposals and suspensions, not only ACTIVE grants.
	expired, err := s.ExpireDue(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, []string{"PROPOSED", "EXPIRED"}, countHistory(t, pool, testTenantA, id))
}

// Extend keeps identity and evidence, and the overlap pre-check excludes the
// grant itself — the 5 Oct re-audit probe.
func TestExtendPassesItsOwnOverlapCheck(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	g := grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, g)
	require.NoError(t, err)

	newTo := now.Add(48 * time.Hour).Truncate(time.Microsecond) // Postgres precision
	require.ErrorIs(t, s.CheckOverlap(ctx, testTenantA, testEntityA, principalOther, "PO_ISSUE", now, newTo, uuid.NewString(), ""),
		domain.ErrOverlapConflict, "without the self-exclusion the grant overlaps itself")
	require.NoError(t, s.CheckOverlap(ctx, testTenantA, testEntityA, principalOther, "PO_ISSUE", now, newTo, "", g.DelegationID))

	ext, err := s.Transition(ctx, g.DelegationID, store.Extend, domain.TransitionInput{ExpectedVersion: 1, ActorPrincipalID: principalSelf,
		Reason: "leave extended", NewEffectiveTo: newTo})
	require.NoError(t, err)
	require.True(t, ext.EffectiveTo.Equal(newTo))
	require.Equal(t, []string{"authority.delegated", "authority.extended"}, outboxEvents(t, pool, testTenantA, g.DelegationID))
}

// As of an instant before a revocation, the grant was effective; now it is not.
func TestListEffectiveAsOfReconstructsHistory(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	g := grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now.Add(-time.Hour), now.Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, g)
	require.NoError(t, err)
	before := time.Now().UTC()
	time.Sleep(20 * time.Millisecond)
	_, err = s.Transition(ctx, g.DelegationID, store.Revoke, domain.TransitionInput{ExpectedVersion: 1, ActorPrincipalID: principalOther, Reason: "x"})
	require.NoError(t, err)

	then, err := s.ListEffectiveAsOf(ctx, domain.ListDelegationsFilter{LegalEntityID: testEntityA}, before)
	require.NoError(t, err)
	require.Len(t, then, 1)
	require.Equal(t, domain.DelegationStatusActive, then[0].Status, "as it was then, not as it is now")
	nowList, err := s.ListEffectiveAsOf(ctx, domain.ListDelegationsFilter{LegalEntityID: testEntityA}, time.Now().UTC())
	require.NoError(t, err)
	require.Empty(t, nowList)
	other, err := s.ListEffectiveAsOf(tenantCtx(testTenantB), domain.ListDelegationsFilter{}, before)
	require.NoError(t, err)
	require.Empty(t, other, "another tenant's history is invisible")
}

// A→B→C returns both links, in order. It used to return only B→C.
func TestExplainDelegationChainReturnsTheWholePath(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)
	now := time.Now().UTC()
	ab := grant(uuid.NewString(), uuid.NewString(), testEntityA, principalSelf, principalOther, now.Add(-time.Minute), now.Add(time.Hour))
	bc := grant(uuid.NewString(), uuid.NewString(), testEntityA, principalOther, principalThird, now.Add(-time.Minute), now.Add(time.Hour))
	bc.CreatedByPrincipalID, bc.ApprovedByPrincipalID = principalOther, ptr(principalOther)
	_, err := s.CreateDelegation(ctx, ab)
	require.NoError(t, err)
	_, err = s.CreateDelegation(ctx, bc)
	require.NoError(t, err)

	chain, err := s.ExplainDelegationChain(ctx, testTenantA, testEntityA, principalSelf, principalThird, "PO_ISSUE")
	require.NoError(t, err)
	require.Len(t, chain, 2)
	require.Equal(t, ab.DelegationID, chain[0].DelegationID)
	require.Equal(t, bc.DelegationID, chain[1].DelegationID)
	require.Equal(t, 2, chain[1].StepNumber)
}

func TestRefusedEscalationIsDurableAndTenantScoped(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	now := time.Now().UTC()
	require.NoError(t, s.RecordRefusedEscalation(tenantCtx(testTenantA), &domain.RefusedEscalation{
		LegalEntityID: testEntityA, CallerPrincipalID: principalThird, DelegatorPrincipalID: principalSelf,
		DelegatePrincipalID: principalThird, ActionType: "PO_ISSUE", EffectiveFrom: now, EffectiveTo: now.Add(time.Hour),
		RefusalReason: "delegator_exceeds_limit"}), "the runtime role can write the evidence")
	require.Error(t, s.RecordRefusedEscalation(tenantCtx(testTenantA), &domain.RefusedEscalation{
		LegalEntityID: testEntityA, CallerPrincipalID: "x", DelegatorPrincipalID: "y", DelegatePrincipalID: "z",
		ActionType: "PO_ISSUE", EffectiveFrom: now, EffectiveTo: now, RefusalReason: "made_up"}), "the vocabulary is closed")

	var n int
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", testTenantB)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM refused_escalations`).Scan(&n))
	require.Zero(t, n, "tenant B cannot see tenant A's refusals")
}

func TestIdempotencyKeyClaimReplayAndMismatch(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := context.Background()
	rec, err := s.ClaimIdempotencyKey(ctx, testTenantA, "POST /v1/delegations/x/revoke?expected_version=1", "k1", "fp-1")
	require.NoError(t, err)
	require.Nil(t, rec, "the first claim runs the command")
	rec, err = s.ClaimIdempotencyKey(ctx, testTenantA, "POST /v1/delegations/x/revoke?expected_version=1", "k1", "fp-1")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Zero(t, rec.ResponseStatus, "still in flight")
	require.NoError(t, s.CompleteIdempotencyKey(ctx, testTenantA, "POST /v1/delegations/x/revoke?expected_version=1", "k1", 200, []byte(`{"ok":true}`)))
	rec, err = s.ClaimIdempotencyKey(ctx, testTenantA, "POST /v1/delegations/x/revoke?expected_version=1", "k1", "fp-1")
	require.NoError(t, err)
	require.Equal(t, 200, rec.ResponseStatus)
	_, err = s.ClaimIdempotencyKey(ctx, testTenantA, "POST /v1/delegations/x/revoke?expected_version=1", "k1", "fp-other-principal")
	require.ErrorIs(t, err, store.ErrIdempotencyFingerprintMismatch)
}
