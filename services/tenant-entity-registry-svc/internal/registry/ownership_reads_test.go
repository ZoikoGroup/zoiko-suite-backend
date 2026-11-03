package registry_test

// ORG-04 read surfaces at the service layer: authorization, masking of
// beneficial notes, and fail-closed behaviour. The graph arithmetic itself is
// proven in internal/domain, and the as-of reconstruction against real rows in
// internal/store.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// actionAuthZ answers per action: allow unless the action is listed.
type actionAuthZ struct {
	deny        map[string]bool
	unavailable map[string]bool
}

func (a actionAuthZ) Authorize(_ context.Context, _, _, _, action string) error {
	if a.unavailable[action] {
		return authz.ErrAuthZUnavailable
	}
	if a.deny[action] {
		return authz.ErrUnauthorized
	}
	return nil
}

const secretNote = "owned through the Zeta family trust"

// approvedWithNotes proposes (with a sensitive note) and approves one relationship.
func approvedWithNotes(t *testing.T, svc *registry.Service, parent, child string) *domain.OwnershipRelationship {
	t.Helper()
	req := proposeReq(parent, child)
	req.BeneficialNotes = secretNote
	r, err := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), req)
	require.NoError(t, err)
	out, err := svc.ApproveOwnershipRelationship(as("checker", "tenant-001"), r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 1})
	require.NoError(t, err)
	return out
}

var readAt = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func TestOwnershipReads_ReturnTheStructureAndTheNotesToAFullyPermittedCaller(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	approvedWithNotes(t, svc, parent, child)
	ctx := as("reader", "tenant-001")

	g, err := svc.GetOwnershipGraph(ctx, readAt, "")
	require.NoError(t, err)
	require.Len(t, g.Relationships, 1)
	require.NotNil(t, g.Relationships[0].BeneficialNotes)
	assert.Equal(t, secretNote, *g.Relationships[0].BeneficialNotes)

	p, err := svc.ListEntityParents(ctx, child, readAt)
	require.NoError(t, err)
	require.Len(t, p.Parents, 1)
	assert.Equal(t, parent, p.Parents[0].ParentLegalEntityID)

	grp, err := svc.GetGroupAsOf(ctx, child, readAt)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{parent, child}, grp.Members)
	assert.Equal(t, []string{parent}, grp.UltimateParents)

	ch, err := svc.ExplainControlChain(ctx, child, readAt)
	require.NoError(t, err)
	require.Len(t, ch.Chains, 1)
	assert.Equal(t, parent, ch.Chains[0].UltimateParentLegalEntityID)
	assert.InDelta(t, 70, ch.Chains[0].EffectiveOwnershipPct, 0.0001)
	assert.True(t, ch.Chains[0].Controls, "70% is a majority")
}

// The structure without the people behind it: a caller lacking the sensitive
// permission is not refused, the notes are simply not there.
func TestOwnershipReads_AreMaskedForACallerWithoutTheSensitivePermission(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	rel := approvedWithNotes(t, svc, parent, child)
	masked := newSvc(t, ms, actionAuthZ{deny: map[string]bool{"view_sensitive": true}}, acceptAllJurisd{})
	ctx := as("analyst", "tenant-001")

	g, err := masked.GetOwnershipGraph(ctx, readAt, "")
	require.NoError(t, err)
	require.Len(t, g.Relationships, 1)
	assert.Nil(t, g.Relationships[0].BeneficialNotes, "graph")
	assert.Equal(t, parent, g.Relationships[0].ParentLegalEntityID, "the structure is still there")

	p, err := masked.ListEntityParents(ctx, child, readAt)
	require.NoError(t, err)
	assert.Nil(t, p.Parents[0].BeneficialNotes, "parents")

	grp, err := masked.GetGroupAsOf(ctx, child, readAt)
	require.NoError(t, err)
	assert.Nil(t, grp.Relationships[0].BeneficialNotes, "group")

	one, err := masked.GetOwnershipRelationship(ctx, rel.RelationshipID)
	require.NoError(t, err)
	assert.Nil(t, one.BeneficialNotes, "single read")
	assert.Equal(t, "filing-1", one.EvidenceRef)

	// And masking did not damage the stored row for the next, permitted reader.
	again, err := svc.GetOwnershipRelationship(as("reader", "tenant-001"), rel.RelationshipID)
	require.NoError(t, err)
	require.NotNil(t, again.BeneficialNotes)
	assert.Equal(t, secretNote, *again.BeneficialNotes)
}

func TestOwnershipReads_ChainNeverCarriesNotesAtAll(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	approvedWithNotes(t, svc, parent, child)
	ch, err := svc.ExplainControlChain(as("reader", "tenant-001"), child, readAt)
	require.NoError(t, err)
	b, _ := jsonOf(ch)
	assert.NotContains(t, b, secretNote)
}

func TestOwnershipReads_RefusalsAndOutages(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	approvedWithNotes(t, svc, parent, child)
	ctx := as("reader", "tenant-001")

	t.Run("no view permission", func(t *testing.T) {
		denied := newSvc(t, ms, actionAuthZ{deny: map[string]bool{"view": true}}, acceptAllJurisd{})
		_, err := denied.GetOwnershipGraph(ctx, readAt, "")
		require.ErrorIs(t, err, registry.ErrUnauthorized)
		_, err = denied.ListEntityParents(ctx, child, readAt)
		require.ErrorIs(t, err, registry.ErrUnauthorized)
		_, err = denied.GetGroupAsOf(ctx, child, readAt)
		require.ErrorIs(t, err, registry.ErrUnauthorized)
		_, err = denied.ExplainControlChain(ctx, child, readAt)
		require.ErrorIs(t, err, registry.ErrUnauthorized)
	})
	t.Run("the sensitive check being down fails closed rather than leaking or masking silently", func(t *testing.T) {
		down := newSvc(t, ms, actionAuthZ{unavailable: map[string]bool{"view_sensitive": true}}, acceptAllJurisd{})
		_, err := down.GetOwnershipGraph(ctx, readAt, "")
		require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	})
	t.Run("no verified principal", func(t *testing.T) {
		_, err := svc.GetOwnershipGraph(domain.WithTenant(context.Background(), "tenant-001"), readAt, "")
		require.Error(t, err)
	})
	t.Run("unknown entity", func(t *testing.T) {
		_, err := svc.GetGroupAsOf(ctx, "00000000-0000-4000-8000-000000000000", readAt)
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("an unauthorized caller cannot probe which ids exist", func(t *testing.T) {
		denied := newSvc(t, ms, actionAuthZ{deny: map[string]bool{"view": true}}, acceptAllJurisd{})
		_, err := denied.GetOwnershipRelationship(ctx, "00000000-0000-4000-8000-000000000000")
		require.ErrorIs(t, err, registry.ErrUnauthorized, "not 404: that would confirm the id is absent")
	})
}

func TestOwnershipReads_ZeroAsOfMeansNow(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	approvedWithNotes(t, svc, parent, child) // valid from 2026-01-01
	p, err := svc.ListEntityParents(as("reader", "tenant-001"), child, time.Time{})
	require.NoError(t, err)
	assert.Len(t, p.Parents, 1)
	assert.WithinDuration(t, time.Now(), p.AsOf, 5*time.Second)
}

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
