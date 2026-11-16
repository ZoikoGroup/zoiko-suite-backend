package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/middleware"
)

// appPool returns a pool for the store under test. When
// TEST_DATABASE_APP_URL is set it connects as that (non-superuser,
// NOBYPASSRLS) role, so row-level security is actually enforced; otherwise it
// falls back to the migration pool, which may bypass RLS.
func appPool(t *testing.T, migrationPool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_APP_URL")
	if dsn == "" {
		return migrationPool
	}
	p, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// BIZ-06 implementation gate: tenant isolation. Tenant B must not be able to
// read or change tenant A's relationship, interactions, opportunities,
// consent records or commercial links, and nothing B attempts may change A's
// data.
func TestPgStore_CRM_TenantIsolation(t *testing.T) {
	migrationPool := requireTestDB(t)
	pool := appPool(t, migrationPool)
	s := newStore(pool)

	const tenantA, tenantB = "tenant-a", "tenant-b"
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	rel, err := s.CreateRelationship(ctxA, domain.CreateRelationshipParams{TenantID: tenantA, LegalEntityID: "le-shared", Source: "WEB", OwnerPrincipalID: "owner-a", CreatedByPrincipalID: "rep-a"})
	require.NoError(t, err)
	_, err = s.LogInteraction(ctxA, domain.LogInteractionParams{RelationshipID: rel.RelationshipID, TenantID: tenantA, InteractionType: "CALL", Notes: "a-private-notes", LoggedByPrincipalID: "rep-a"})
	require.NoError(t, err)
	opp, err := s.CreateOpportunity(ctxA, domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: tenantA, LegalEntityID: "le-shared", Name: "A deal", Value: 1000, CreatedByPrincipalID: "rep-a"})
	require.NoError(t, err)
	_, err = s.RecordConsent(ctxA, domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: tenantA, ConsentType: "EMAIL", ConsentStatus: domain.ConsentGranted, RecordedByPrincipalID: "rep-a"})
	require.NoError(t, err)
	_, err = s.LinkCommercialObject(ctxA, domain.LinkCommercialObjectParams{RelationshipID: rel.RelationshipID, TenantID: tenantA, LinkedObjectType: "QUOTE", LinkedObjectID: "q-a", LinkedByPrincipalID: "rep-a"})
	require.NoError(t, err)

	// Reads: B sees nothing of A's, even using the same legal-entity id.
	_, err = s.GetRelationship(ctxB, rel.RelationshipID)
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.GetOpportunity(ctxB, opp.OpportunityID)
	require.ErrorIs(t, err, domain.ErrOpportunityNotFound)
	timeline, err := s.GetTimeline(ctxB, tenantB, rel.RelationshipID)
	require.NoError(t, err)
	require.Empty(t, timeline)
	consents, err := s.GetConsentContext(ctxB, tenantB, rel.RelationshipID)
	require.NoError(t, err)
	require.Empty(t, consents)
	links, err := s.GetLinkedFinancialObjects(ctxB, tenantB, rel.RelationshipID)
	require.NoError(t, err)
	require.Empty(t, links)
	pipeline, err := s.ListPipeline(ctxB, domain.ListPipelineParams{TenantID: tenantB, LegalEntityID: "le-shared"})
	require.NoError(t, err)
	require.Empty(t, pipeline)

	// Writes: B cannot touch A's rows.
	_, err = s.LogInteraction(ctxB, domain.LogInteractionParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, InteractionType: "CALL", LoggedByPrincipalID: "attacker"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.CreateOpportunity(ctxB, domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, LegalEntityID: "le-shared", Name: "injected", CreatedByPrincipalID: "attacker"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.UpdateStage(ctxB, domain.UpdateStageParams{OpportunityID: opp.OpportunityID, TenantID: tenantB, ActorPrincipalID: "attacker", Stage: domain.StageQualified})
	require.ErrorIs(t, err, domain.ErrOpportunityNotFound)
	_, err = s.CloseOpportunity(ctxB, domain.CloseOpportunityParams{OpportunityID: opp.OpportunityID, TenantID: tenantB, ActorPrincipalID: "attacker", Won: true})
	require.ErrorIs(t, err, domain.ErrOpportunityNotFound)
	_, err = s.ActivateRelationship(ctxB, domain.ActivateRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, ActorPrincipalID: "attacker"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.CloseRelationship(ctxB, domain.CloseRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, ActorPrincipalID: "attacker", ClosureReason: "x"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.RecordConsent(ctxB, domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, ConsentType: "EMAIL", ConsentStatus: domain.ConsentWithdrawn, RecordedByPrincipalID: "attacker"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
	_, err = s.LinkCommercialObject(ctxB, domain.LinkCommercialObjectParams{RelationshipID: rel.RelationshipID, TenantID: tenantB, LinkedObjectType: "QUOTE", LinkedObjectID: "q-b", LinkedByPrincipalID: "attacker"})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)

	// A's data is unchanged by everything B attempted.
	got, err := s.GetRelationship(ctxA, rel.RelationshipID)
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipProspect, got.Status)
	gotOpp, err := s.GetOpportunity(ctxA, opp.OpportunityID)
	require.NoError(t, err)
	require.Equal(t, domain.StageLead, gotOpp.Stage)
	require.Equal(t, domain.OpportunityOpen, gotOpp.Status)
	aTimeline, err := s.GetTimeline(ctxA, tenantA, rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, aTimeline, 1)
	aConsents, err := s.GetConsentContext(ctxA, tenantA, rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, aConsents, 1)
	require.Equal(t, domain.ConsentGranted, aConsents[0].ConsentStatus)
	aLinks, err := s.GetLinkedFinancialObjects(ctxA, tenantA, rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, aLinks, 1)

	// RLS itself, bypassing the store's own tenant_id predicates: with B's
	// tenant context and no WHERE tenant_id, A's rows must still be invisible.
	// Only meaningful for a non-superuser / NOBYPASSRLS role; otherwise it is
	// skipped rather than reported as a pass.
	ctx := context.Background()
	var bypass bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass))
	if bypass {
		t.Log("connecting role bypasses RLS; skipping the raw RLS probe (set TEST_DATABASE_APP_URL to a NOSUPERUSER NOBYPASSRLS role)")
		return
	}
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB)
	require.NoError(t, err)
	for _, table := range []string{"relationships", "interactions", "opportunities", "opportunity_stage_transitions", "consent_records", "commercial_object_links"} {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n), table)
		require.Zero(t, n, "RLS leak: tenant B context sees %d rows in %s", n, table)
	}
}
