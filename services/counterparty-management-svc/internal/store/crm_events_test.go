package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/store"
)

// Each of the five BIZ-06 events is written to the eventing outbox by the same
// transaction as the change it announces. The payload schemas under
// contracts/events are enforced on every row by verifyEventContracts.
func TestPgStore_CRM_EventsWrittenToOutbox(t *testing.T) {
	pool := requireTestDB(t)
	s := newStore(pool)

	rel, err := s.CreateRelationship(ctx(), domain.CreateRelationshipParams{
		CorrelationID: "corr-1", TenantID: "default", LegalEntityID: "le-1", Source: "WEB", Channel: "FORM",
		OwnerPrincipalID: "owner-1", CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)
	_, err = s.LogInteraction(ctx(), domain.LogInteractionParams{
		CorrelationID: "corr-1", RelationshipID: rel.RelationshipID, TenantID: "default", InteractionType: "CALL",
		Channel: "PHONE", Notes: "secret-notes-must-not-leak", LoggedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)
	opp, err := s.CreateOpportunity(ctx(), domain.CreateOpportunityParams{
		CorrelationID: "corr-1", RelationshipID: rel.RelationshipID, TenantID: "default", LegalEntityID: "le-1",
		Name: "Acme deal", Value: 50000, CreatedByPrincipalID: "rep-1",
	})
	require.NoError(t, err)
	_, err = s.UpdateStage(ctx(), domain.UpdateStageParams{
		CorrelationID: "corr-1", OpportunityID: opp.OpportunityID, TenantID: "default", ActorPrincipalID: "rep-1",
		Stage: domain.StageQualified, Reason: "budget confirmed",
	})
	require.NoError(t, err)
	_, err = s.CloseRelationship(ctx(), domain.CloseRelationshipParams{
		CorrelationID: "corr-1", RelationshipID: rel.RelationshipID, TenantID: "default", ActorPrincipalID: "owner-1", ClosureReason: "lost",
	})
	require.NoError(t, err)

	rows, err := pool.Query(context.Background(), `SELECT event_type, convert_from(payload, 'UTF8') FROM eventing_outbox`)
	require.NoError(t, err)
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var typ, payload string
		require.NoError(t, rows.Scan(&typ, &payload))
		counts[typ]++
		require.NotContains(t, payload, "secret-notes-must-not-leak", "free-text notes must never be carried in an event")
	}
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]int{
		"com.zoikosuite.business.relationship.created":      1,
		"com.zoikosuite.business.interaction.logged":        1,
		"com.zoikosuite.business.opportunity.created":       1,
		"com.zoikosuite.business.opportunity.stage-changed": 1,
		"com.zoikosuite.business.relationship.closed":       1,
	}, counts)
}

// A failed or refused write must leave neither state nor event: with no event
// region configured the envelope is refused, and the relationship row must be
// rolled back with it (the outbox is transactional, not best effort).
func TestPgStore_CRM_NoEventRegion_RollsBackTheWrite(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool) // deliberately no WithEventRegion

	_, err := s.CreateRelationship(ctx(), domain.CreateRelationshipParams{
		TenantID: "default", LegalEntityID: "le-1", CreatedByPrincipalID: "rep-1",
	})
	require.Error(t, err)

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM relationships`).Scan(&n))
	require.Zero(t, n, "the relationship insert must roll back when its event cannot be built")
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&n))
	require.Zero(t, n, "no event may be written for a rolled-back change")
}
