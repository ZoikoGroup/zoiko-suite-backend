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
