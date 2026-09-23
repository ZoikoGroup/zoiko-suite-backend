package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/store"
)

// requireTestDB connects to a real Postgres named by TEST_DATABASE_URL
// and replays every migration from a clean slate — same
// gating/discovery pattern used by every other service touched in this
// build.
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS commercial_object_links;
		DROP TABLE IF EXISTS consent_records;
		DROP TABLE IF EXISTS opportunity_stage_transitions;
		DROP TABLE IF EXISTS opportunities;
		DROP TABLE IF EXISTS interactions;
		DROP TABLE IF EXISTS relationships;
		DROP TABLE IF EXISTS counterparties;
	`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		require.NoError(t, err, "reading migration %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, "applying migration %s", filepath.Base(path))
	}
	return pool
}

// ctx uses context.Background() throughout — middleware.GetTenantID
// resolves an unset context to "default", which every call in a given
// test agrees on, so this is a real single-tenant scope, not an
// unscoped call.
func ctx() context.Context { return context.Background() }

func newRelationshipForTest(t *testing.T, s *store.PgStore) *domain.Relationship {
	t.Helper()
	rel, err := s.CreateRelationship(ctx(), domain.CreateRelationshipParams{
		TenantID: "default", LegalEntityID: "le-1", Source: "WEBSITE", Channel: "FORM", OwnerPrincipalID: "owner-1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	return rel
}

func TestPgStore_CreateRelationship_Unlinked(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	rel := newRelationshipForTest(t, s)
	require.Equal(t, domain.PartyLinkUnlinked, rel.PartyLinkStatus)
	require.Equal(t, domain.RelationshipProspect, rel.Status)
	require.Nil(t, rel.CounterpartyID)

	got, err := s.GetRelationship(ctx(), rel.RelationshipID)
	require.NoError(t, err)
	require.Equal(t, rel.RelationshipID, got.RelationshipID)
}

func TestPgStore_GetRelationship_UnknownRelationship_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.GetRelationship(ctx(), "rel-does-not-exist")
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
}

// TestPgStore_CreateRelationship_WithCandidate_ThenConfirmPartyLink
// proves the doc's own failure semantics: a duplicate-party match stays
// an unresolved CANDIDATE until ConfirmPartyLink explicitly confirms
// it — never auto-promoted.
func TestPgStore_CreateRelationship_WithCandidate_ThenConfirmPartyLink(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	rel, err := s.CreateRelationship(ctx(), domain.CreateRelationshipParams{
		TenantID: "default", LegalEntityID: "le-1", CandidateCounterpartyID: "cpty-candidate-1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.PartyLinkCandidate, rel.PartyLinkStatus)
	require.NotNil(t, rel.CandidateCounterpartyID)
	require.Equal(t, "cpty-candidate-1", *rel.CandidateCounterpartyID)
	require.Nil(t, rel.CounterpartyID, "a candidate match must never be auto-promoted to a confirmed link")

	confirmed, err := s.ConfirmPartyLink(ctx(), domain.ConfirmPartyLinkParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "reviewer-1", CounterpartyID: "cpty-candidate-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.PartyLinkConfirmed, confirmed.PartyLinkStatus)
	require.NotNil(t, confirmed.CounterpartyID)
	require.Equal(t, "cpty-candidate-1", *confirmed.CounterpartyID)

	// Confirming an already-confirmed link is refused.
	_, err = s.ConfirmPartyLink(ctx(), domain.ConfirmPartyLinkParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "reviewer-1", CounterpartyID: "cpty-candidate-1",
	})
	require.ErrorIs(t, err, domain.ErrPartyLinkNotCandidate)
}

func TestPgStore_ConfirmPartyLink_RejectsUnlinkedRelationship(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s) // no candidate supplied — UNLINKED
	_, err := s.ConfirmPartyLink(ctx(), domain.ConfirmPartyLinkParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "reviewer-1", CounterpartyID: "cpty-x",
	})
	require.ErrorIs(t, err, domain.ErrPartyLinkNotCandidate)
}

func TestPgStore_LogInteraction_ThenGetTimeline(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	_, err := s.LogInteraction(ctx(), domain.LogInteractionParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", InteractionType: "CALL", Channel: "PHONE", Notes: "intro call", LoggedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)
	_, err = s.LogInteraction(ctx(), domain.LogInteractionParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", InteractionType: "EMAIL", Notes: "follow-up", LoggedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)

	timeline, err := s.GetTimeline(ctx(), "default", rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, timeline, 2)
	require.Equal(t, "CALL", timeline[0].InteractionType)
	require.Equal(t, "EMAIL", timeline[1].InteractionType)
}

func TestPgStore_LogInteraction_UnknownRelationship_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.LogInteraction(ctx(), domain.LogInteractionParams{
		RelationshipID: "rel-does-not-exist", TenantID: "default", InteractionType: "CALL", LoggedByPrincipalID: "rep-1",
	})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
}

// TestPgStore_Interactions_AreAppendOnly is the negative-controlled
// proof of the append-only trigger.
func TestPgStore_Interactions_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	interaction, err := s.LogInteraction(ctx(), domain.LogInteractionParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", InteractionType: "CALL", LoggedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE interactions SET notes='tampered' WHERE interaction_id=$1`, interaction.InteractionID)
	require.Error(t, err, "expected the trigger to refuse mutating an interaction")

	_, err = pool.Exec(ctx(), `DELETE FROM interactions WHERE interaction_id=$1`, interaction.InteractionID)
	require.Error(t, err, "expected the trigger to refuse deleting an interaction")
}

func TestPgStore_CreateOpportunity_LandsLeadWithHistory(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Acme deal", Value: 50000, CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)
	require.Equal(t, domain.StageLead, opp.Stage)
	require.Equal(t, domain.OpportunityOpen, opp.Status)
	require.Equal(t, "USD", opp.Currency)

	got, err := s.GetOpportunity(ctx(), opp.OpportunityID)
	require.NoError(t, err)
	require.Equal(t, opp.OpportunityID, got.OpportunityID)
}

func TestPgStore_CreateOpportunity_UnknownRelationship_ReturnsNotFound(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	_, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		RelationshipID: "rel-does-not-exist", TenantID: "default", LegalEntityID: "le-1", Name: "X", CreatedByPrincipalID: "rep-1",
	})
	require.ErrorIs(t, err, domain.ErrRelationshipNotFound)
}

func TestPgStore_UpdateStage_ValidatesStageAndRecordsTransition(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Acme deal", CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)

	_, err = s.UpdateStage(ctx(), domain.UpdateStageParams{OpportunityID: opp.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Stage: "NOT_A_REAL_STAGE"})
	require.ErrorIs(t, err, domain.ErrInvalidStage)

	updated, err := s.UpdateStage(ctx(), domain.UpdateStageParams{OpportunityID: opp.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Stage: domain.StageQualified, Reason: "budget confirmed"})
	require.NoError(t, err)
	require.Equal(t, domain.StageQualified, updated.Stage)

	var transitionCount int
	err = pool.QueryRow(ctx(), `SELECT count(*) FROM opportunity_stage_transitions WHERE opportunity_id=$1`, opp.OpportunityID).Scan(&transitionCount)
	require.NoError(t, err)
	require.Equal(t, 2, transitionCount, "expected the creation transition plus this UpdateStage transition")
}

// TestPgStore_OpportunityStageTransitions_AreAppendOnly is the
// negative-controlled proof of the "versioned pipeline" mechanism — a
// raw UPDATE or DELETE against opportunity_stage_transitions is refused.
func TestPgStore_OpportunityStageTransitions_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Acme deal", CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)

	var transitionID string
	err = pool.QueryRow(ctx(), `SELECT transition_id FROM opportunity_stage_transitions WHERE opportunity_id=$1`, opp.OpportunityID).Scan(&transitionID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE opportunity_stage_transitions SET reason='tampered' WHERE transition_id=$1`, transitionID)
	require.Error(t, err, "expected the trigger to refuse mutating a stage transition")

	_, err = pool.Exec(ctx(), `DELETE FROM opportunity_stage_transitions WHERE transition_id=$1`, transitionID)
	require.Error(t, err, "expected the trigger to refuse deleting a stage transition")
}

func TestPgStore_UpdateStage_RejectsWhenOpportunityAlreadyClosed(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Acme deal", CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)

	// Directly close the opportunity to set up this test's own
	// precondition — CloseOpportunity itself is Wave 2's command.
	_, err = pool.Exec(ctx(), `UPDATE opportunities SET status='CLOSED' WHERE opportunity_id=$1`, opp.OpportunityID)
	require.NoError(t, err)

	_, err = s.UpdateStage(ctx(), domain.UpdateStageParams{OpportunityID: opp.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Stage: domain.StageProposal})
	require.ErrorIs(t, err, domain.ErrOpportunityInvalidState)
}

func TestPgStore_CloseOpportunity_WonAndLost(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	won, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Won deal", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	closedWon, err := s.CloseOpportunity(ctx(), domain.CloseOpportunityParams{OpportunityID: won.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Won: true, Reason: "signed"})
	require.NoError(t, err)
	require.Equal(t, domain.StageClosedWon, closedWon.Stage)
	require.Equal(t, domain.OpportunityClosed, closedWon.Status)
	require.NotNil(t, closedWon.ClosedAt)

	lost, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Lost deal", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	closedLost, err := s.CloseOpportunity(ctx(), domain.CloseOpportunityParams{OpportunityID: lost.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Won: false, Reason: "went with competitor"})
	require.NoError(t, err)
	require.Equal(t, domain.StageClosedLost, closedLost.Stage)

	// Cannot close an already-closed opportunity.
	_, err = s.CloseOpportunity(ctx(), domain.CloseOpportunityParams{OpportunityID: won.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Won: true})
	require.ErrorIs(t, err, domain.ErrOpportunityInvalidState)
}

func TestPgStore_ListPipeline_OnlyOpenForLegalEntity(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	open1, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-pipeline", Name: "Open 1", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	_, err = s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-pipeline", Name: "Open 2 (other entity closed)", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	closed, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-pipeline", Name: "Will be closed", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	_, err = s.CloseOpportunity(ctx(), domain.CloseOpportunityParams{OpportunityID: closed.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1", Won: true})
	require.NoError(t, err)

	pipeline, err := s.ListPipeline(ctx(), domain.ListPipelineParams{TenantID: "default", LegalEntityID: "le-pipeline"})
	require.NoError(t, err)
	require.Len(t, pipeline, 2, "expected only the two still-OPEN opportunities")
	ids := map[string]bool{}
	for _, o := range pipeline {
		ids[o.OpportunityID] = true
	}
	require.True(t, ids[open1.OpportunityID])
	require.False(t, ids[closed.OpportunityID])
}

func TestPgStore_RelationshipLifecycle_ActivateMarkDormantClose(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	require.Equal(t, domain.RelationshipProspect, rel.Status)

	// MarkDormant before Activate is refused — PROSPECT is not ACTIVE.
	_, err := s.MarkDormant(ctx(), domain.MarkDormantParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrRelationshipInvalidState)

	active, err := s.ActivateRelationship(ctx(), domain.ActivateRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1"})
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipActive, active.Status)

	// Activating again (already ACTIVE) is refused.
	_, err = s.ActivateRelationship(ctx(), domain.ActivateRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1"})
	require.ErrorIs(t, err, domain.ErrRelationshipInvalidState)

	dormant, err := s.MarkDormant(ctx(), domain.MarkDormantParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1", Reason: "no response in 90 days"})
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipDormant, dormant.Status)

	closed, err := s.CloseRelationship(ctx(), domain.CloseRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1", ClosureReason: "lost interest"})
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)

	// Closing an already-CLOSED relationship is refused.
	_, err = s.CloseRelationship(ctx(), domain.CloseRelationshipParams{RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1", ClosureReason: "again"})
	require.ErrorIs(t, err, domain.ErrRelationshipInvalidState)
}

// TestPgStore_MergeCRMProfile_ReparentsChildrenThenRejectsDoubleMerge
// proves the forward-link mechanism and its negative control, mirroring
// document-vault-svc's SupersedeClassification.
func TestPgStore_MergeCRMProfile_ReparentsChildrenThenRejectsDoubleMerge(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	source := newRelationshipForTest(t, s)
	target := newRelationshipForTest(t, s)

	interaction, err := s.LogInteraction(ctx(), domain.LogInteractionParams{RelationshipID: source.RelationshipID, TenantID: "default", InteractionType: "CALL", LoggedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{RelationshipID: source.RelationshipID, TenantID: "default", LegalEntityID: "le-1", Name: "Deal", CreatedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	_, err = s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: source.RelationshipID, TenantID: "default", ConsentType: "EMAIL", ConsentStatus: domain.ConsentGranted, RecordedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	link, err := s.LinkCommercialObject(ctx(), domain.LinkCommercialObjectParams{RelationshipID: source.RelationshipID, TenantID: "default", LinkedObjectType: "QUOTE", LinkedObjectID: "q-1", LinkedByPrincipalID: "rep-1"})
	require.NoError(t, err)

	merged, err := s.MergeCRMProfile(ctx(), domain.MergeCRMProfileParams{SourceRelationshipID: source.RelationshipID, TargetRelationshipID: target.RelationshipID, TenantID: "default", ActorPrincipalID: "supervisor-1"})
	require.NoError(t, err)
	require.Equal(t, domain.RelationshipMerged, merged.Status)
	require.NotNil(t, merged.MergedIntoRelationshipID)
	require.Equal(t, target.RelationshipID, *merged.MergedIntoRelationshipID)

	// Every child row reparented onto target.
	timeline, err := s.GetTimeline(ctx(), "default", target.RelationshipID)
	require.NoError(t, err)
	require.Len(t, timeline, 1)
	require.Equal(t, interaction.InteractionID, timeline[0].InteractionID)

	targetOpp, err := s.GetOpportunity(ctx(), opp.OpportunityID)
	require.NoError(t, err)
	require.Equal(t, target.RelationshipID, targetOpp.RelationshipID)

	consents, err := s.GetConsentContext(ctx(), "default", target.RelationshipID)
	require.NoError(t, err)
	require.Len(t, consents, 1)

	links, err := s.GetLinkedFinancialObjects(ctx(), "default", target.RelationshipID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, link.LinkID, links[0].LinkID)

	// A second merge attempt on the already-merged source is refused
	// with the precise reason, not a generic state error.
	another := newRelationshipForTest(t, s)
	_, err = s.MergeCRMProfile(ctx(), domain.MergeCRMProfileParams{SourceRelationshipID: source.RelationshipID, TargetRelationshipID: another.RelationshipID, TenantID: "default", ActorPrincipalID: "supervisor-1"})
	require.ErrorIs(t, err, domain.ErrRelationshipAlreadyMerged)
}

func TestPgStore_MergeCRMProfile_RejectsMergingIntoSelf(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	_, err := s.MergeCRMProfile(ctx(), domain.MergeCRMProfileParams{SourceRelationshipID: rel.RelationshipID, TargetRelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "supervisor-1"})
	require.ErrorIs(t, err, domain.ErrCannotMergeIntoSelf)
}

func TestPgStore_LinkCommercialObject_ThenGetLinkedFinancialObjects(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	_, err := s.LinkCommercialObject(ctx(), domain.LinkCommercialObjectParams{RelationshipID: rel.RelationshipID, TenantID: "default", LinkedObjectType: "QUOTE", LinkedObjectID: "q-1", LinkedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	_, err = s.LinkCommercialObject(ctx(), domain.LinkCommercialObjectParams{RelationshipID: rel.RelationshipID, TenantID: "default", LinkedObjectType: "CONTRACT", LinkedObjectID: "c-1", LinkedByPrincipalID: "rep-1"})
	require.NoError(t, err)

	links, err := s.GetLinkedFinancialObjects(ctx(), "default", rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, links, 2)
}

// TestPgStore_CommercialObjectLinks_AreAppendOnly is the
// negative-controlled proof of the append-only trigger.
func TestPgStore_CommercialObjectLinks_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	link, err := s.LinkCommercialObject(ctx(), domain.LinkCommercialObjectParams{RelationshipID: rel.RelationshipID, TenantID: "default", LinkedObjectType: "QUOTE", LinkedObjectID: "q-1", LinkedByPrincipalID: "rep-1"})
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE commercial_object_links SET linked_object_id='tampered' WHERE link_id=$1`, link.LinkID)
	require.Error(t, err, "expected the trigger to refuse mutating a commercial object link")

	_, err = pool.Exec(ctx(), `DELETE FROM commercial_object_links WHERE link_id=$1`, link.LinkID)
	require.Error(t, err, "expected the trigger to refuse deleting a commercial object link")
}

func TestPgStore_RecordConsent_ThenGetConsentContext_ReturnsLatestPerType(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)

	_, err := s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: "default", ConsentType: "EMAIL_MARKETING", ConsentStatus: domain.ConsentGranted, Basis: "opt-in form", RecordedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	_, err = s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: "default", ConsentType: "SMS", ConsentStatus: domain.ConsentGranted, RecordedByPrincipalID: "rep-1"})
	require.NoError(t, err)
	// A later, WITHDRAWN decision for EMAIL_MARKETING must be what
	// GetConsentContext reports — not the earlier GRANTED one.
	withdrawn, err := s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: "default", ConsentType: "EMAIL_MARKETING", ConsentStatus: domain.ConsentWithdrawn, Basis: "unsubscribed", RecordedByPrincipalID: "rep-1"})
	require.NoError(t, err)

	context, err := s.GetConsentContext(ctx(), "default", rel.RelationshipID)
	require.NoError(t, err)
	require.Len(t, context, 2, "expected one entry per distinct consent_type")
	byType := map[string]domain.ConsentRecord{}
	for _, c := range context {
		byType[c.ConsentType] = c
	}
	require.Equal(t, domain.ConsentWithdrawn, byType["EMAIL_MARKETING"].ConsentStatus)
	require.Equal(t, withdrawn.ConsentID, byType["EMAIL_MARKETING"].ConsentID)
	require.Equal(t, domain.ConsentGranted, byType["SMS"].ConsentStatus)
}

func TestPgStore_RecordConsent_InvalidStatus_Rejected(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	_, err := s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: "default", ConsentType: "EMAIL", ConsentStatus: "NOT_A_REAL_STATUS", RecordedByPrincipalID: "rep-1"})
	require.ErrorIs(t, err, domain.ErrInvalidConsentStatus)
}

// TestPgStore_ConsentRecords_AreAppendOnly is the negative-controlled
// proof of the append-only trigger.
func TestPgStore_ConsentRecords_AreAppendOnly(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	rel := newRelationshipForTest(t, s)
	consent, err := s.RecordConsent(ctx(), domain.RecordConsentParams{RelationshipID: rel.RelationshipID, TenantID: "default", ConsentType: "EMAIL", ConsentStatus: domain.ConsentGranted, RecordedByPrincipalID: "rep-1"})
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE consent_records SET consent_status='WITHDRAWN' WHERE consent_id=$1`, consent.ConsentID)
	require.Error(t, err, "expected the trigger to refuse mutating a consent record")

	_, err = pool.Exec(ctx(), `DELETE FROM consent_records WHERE consent_id=$1`, consent.ConsentID)
	require.Error(t, err, "expected the trigger to refuse deleting a consent record")
}
