package registry_test

// ORG-04 service tests: ProposeOwnershipRelationship, Approve and Reject.
// The database-level guarantees (CHECKs, RLS, the guarded UPDATE) are proven
// against real Postgres in internal/store.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ── memStore: ORG-04 ────────────────────────────────────────────────────────

func (m *memStore) CreateOwnershipRelationship(_ context.Context, r *domain.OwnershipRelationship) error {
	if m.ownership == nil {
		m.ownership = map[string]*domain.OwnershipRelationship{}
	}
	c := *r
	m.ownership[r.RelationshipID] = &c
	return nil
}

func (m *memStore) GetOwnershipRelationship(_ context.Context, id string) (*domain.OwnershipRelationship, error) {
	r, ok := m.ownership[id]
	if !ok {
		return nil, nil
	}
	c := *r
	return &c, nil
}

func (m *memStore) DecideOwnershipRelationship(_ context.Context, id string, expected int64,
	status domain.OwnershipStatus, by, note, _ string) (*domain.OwnershipRelationship, error) {
	r, ok := m.ownership[id]
	if !ok {
		return nil, registry.ErrNotFound
	}
	if r.Status != domain.OwnershipProposed {
		return nil, registry.ErrInvalidTransition
	}
	if r.RecordVersion != expected {
		return nil, registry.ErrVersionConflict
	}
	now := time.Now().UTC()
	r.Status, r.DecidedByPrincipalID, r.DecidedAt = status, &by, &now
	r.DecisionNote = &note
	r.RecordVersion++
	c := *r
	return &c, nil
}

// ── fixtures ────────────────────────────────────────────────────────────────

func seedOperationalEntity(ms *memStore, tenantID string) string {
	id := uuid.New().String()
	ms.entities[id] = &domain.LegalEntity{
		LegalEntityID: id, TenantID: tenantID, EntityCode: "EC-" + id[:8],
		LegalName: "Entity " + id[:8], EntityStatus: domain.EntityStatusActive, RecordVersion: 1,
	}
	return id
}

func proposeReq(parent, child string) domain.ProposeOwnershipRequest {
	pct := 70.0
	return domain.ProposeOwnershipRequest{
		ParentLegalEntityID: parent, ChildLegalEntityID: child,
		RelationshipKind: domain.RelationshipOwnership, OwnershipPct: &pct,
		ValidFrom:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EvidenceRef: "filing-1", Reason: "acquisition",
	}
}

func ownershipSvc(t *testing.T) (*registry.Service, *memStore, string, string) {
	t.Helper()
	svc, ms := baseSvc(t)
	seedActiveTenant(ms, "tenant-001")
	return svc, ms, seedOperationalEntity(ms, "tenant-001"), seedOperationalEntity(ms, "tenant-001")
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestOwnership_ProposeFilesAProposedRelationshipByTheVerifiedCaller(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)

	r, err := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipProposed, r.Status)
	assert.Equal(t, "maker", r.ProposedByPrincipalID)
	assert.Equal(t, "tenant-001", r.TenantID, "the tenant is the verified one")
	assert.EqualValues(t, 1, r.RecordVersion)
	assert.Nil(t, r.DecidedByPrincipalID)
}

func TestOwnership_ProposeRefusals(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	ctx := as("maker", "tenant-001")

	t.Run("invalid request", func(t *testing.T) {
		req := proposeReq(parent, child)
		req.EvidenceRef = ""
		_, err := svc.ProposeOwnershipRelationship(ctx, req)
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("self ownership", func(t *testing.T) {
		_, err := svc.ProposeOwnershipRelationship(ctx, proposeReq(parent, parent))
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("unknown entity", func(t *testing.T) {
		_, err := svc.ProposeOwnershipRelationship(ctx, proposeReq(parent, uuid.New().String()))
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("draft entity is not operational", func(t *testing.T) {
		ms.entities[child].EntityStatus = domain.EntityStatusDraft
		defer func() { ms.entities[child].EntityStatus = domain.EntityStatusActive }()
		_, err := svc.ProposeOwnershipRelationship(ctx, proposeReq(parent, child))
		require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	})
	t.Run("not authorized", func(t *testing.T) {
		denied := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})
		_, err := denied.ProposeOwnershipRelationship(ctx, proposeReq(parent, child))
		require.ErrorIs(t, err, registry.ErrUnauthorized)
	})
	t.Run("no verified principal", func(t *testing.T) {
		_, err := svc.ProposeOwnershipRelationship(domain.WithTenant(context.Background(), "tenant-001"), proposeReq(parent, child))
		require.Error(t, err)
	})
	assert.Empty(t, ms.ownership, "no refusal may leave a relationship behind")
}

func TestOwnership_ProposerCannotApproveOrRejectTheirOwnProposal(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	r, err := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))
	require.NoError(t, err)

	body := domain.OwnershipDecisionRequest{ExpectedVersion: 1}
	_, err = svc.ApproveOwnershipRelationship(as("maker", "tenant-001"), r.RelationshipID, body)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	_, err = svc.RejectOwnershipRelationship(as("maker", "tenant-001"), r.RelationshipID, body)
	require.ErrorIs(t, err, registry.ErrSelfApproval)

	assert.Equal(t, domain.OwnershipProposed, ms.ownership[r.RelationshipID].Status)
}

func TestOwnership_AnotherPrincipalApprovesAndTheDeciderIsTheVerifiedCaller(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	r, err := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))
	require.NoError(t, err)

	out, err := svc.ApproveOwnershipRelationship(as("checker", "tenant-001"), r.RelationshipID,
		domain.OwnershipDecisionRequest{ExpectedVersion: 1, DecisionNote: "filing verified"})
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipActive, out.Status)
	require.NotNil(t, out.DecidedByPrincipalID)
	assert.Equal(t, "checker", *out.DecidedByPrincipalID)
	assert.EqualValues(t, 2, out.RecordVersion)
}

func TestOwnership_RejectLeavesTheRelationshipRejected(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	r, _ := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))

	out, err := svc.RejectOwnershipRelationship(as("checker", "tenant-001"), r.RelationshipID,
		domain.OwnershipDecisionRequest{ExpectedVersion: 1, DecisionNote: "no filing"})
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipRejected, out.Status)
}

func TestOwnership_DecisionRefusals(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	r, _ := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))
	checker := as("checker", "tenant-001")

	t.Run("stale version", func(t *testing.T) {
		_, err := svc.ApproveOwnershipRelationship(checker, r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 7})
		require.ErrorIs(t, err, registry.ErrVersionConflict)
	})
	t.Run("missing version", func(t *testing.T) {
		strict := newSvc(t, ms, permitAllAuthZ{}, acceptAllJurisd{})
		strict.ConfigureConcurrency(false)
		_, err := strict.ApproveOwnershipRelationship(checker, r.RelationshipID, domain.OwnershipDecisionRequest{})
		require.ErrorIs(t, err, registry.ErrVersionRequired)
	})
	t.Run("unknown relationship", func(t *testing.T) {
		_, err := svc.ApproveOwnershipRelationship(checker, uuid.New().String(), domain.OwnershipDecisionRequest{ExpectedVersion: 1})
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("not authorized to approve", func(t *testing.T) {
		denied := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})
		_, err := denied.ApproveOwnershipRelationship(checker, r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 1})
		require.ErrorIs(t, err, registry.ErrUnauthorized)
	})
	t.Run("entity merged since the proposal", func(t *testing.T) {
		merged := parent
		ms.entities[child].MergedIntoLegalEntityID = &merged
		defer func() { ms.entities[child].MergedIntoLegalEntityID = nil }()
		_, err := svc.ApproveOwnershipRelationship(checker, r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 1})
		require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	})
	t.Run("already decided", func(t *testing.T) {
		_, err := svc.ApproveOwnershipRelationship(checker, r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 1})
		require.NoError(t, err)
		_, err = svc.ApproveOwnershipRelationship(as("another", "tenant-001"), r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 2})
		require.ErrorIs(t, err, registry.ErrInvalidTransition)
	})
}

func TestOwnership_GetIsNotFoundForAnUnknownId(t *testing.T) {
	svc, _, _, _ := ownershipSvc(t)
	_, err := svc.GetOwnershipRelationship(as("anyone", "tenant-001"), uuid.New().String())
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func (m *memStore) EndOwnershipRelationship(_ context.Context, id string, expected int64, endDate time.Time,
	by, reason, evidence, _ string) (*domain.OwnershipRelationship, error) {
	r, ok := m.ownership[id]
	if !ok {
		return nil, registry.ErrNotFound
	}
	if r.Status != domain.OwnershipActive {
		return nil, registry.ErrInvalidTransition
	}
	if r.RecordVersion != expected {
		return nil, registry.ErrVersionConflict
	}
	now := time.Now().UTC()
	r.Status, r.ValidTo, r.EndedByPrincipalID, r.EndedAt = domain.OwnershipEnded, &endDate, &by, &now
	r.EndReason, r.EndEvidenceRef = &reason, &evidence
	r.RecordVersion++
	c := *r
	return &c, nil
}

// activeRelationship proposes and approves one, returning it as stored.
func activeRelationship(t *testing.T, svc *registry.Service, parent, child string) *domain.OwnershipRelationship {
	t.Helper()
	r, err := svc.ProposeOwnershipRelationship(as("maker", "tenant-001"), proposeReq(parent, child))
	require.NoError(t, err)
	out, err := svc.ApproveOwnershipRelationship(as("checker", "tenant-001"), r.RelationshipID, domain.OwnershipDecisionRequest{ExpectedVersion: 1})
	require.NoError(t, err)
	return out
}

func supersedeReq(version int64) domain.SupersedeOwnershipRequest {
	pct := 80.0
	return domain.SupersedeOwnershipRequest{
		ExpectedVersion: version, OwnershipPct: &pct,
		ValidFrom:   time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		EvidenceRef: "filing-2", Reason: "increased stake",
	}
}

func TestOwnership_SupersedeFilesALinkedProposalAndChangesNothingYet(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	old := activeRelationship(t, svc, parent, child)

	n, err := svc.SupersedeOwnershipRelationship(as("maker", "tenant-001"), old.RelationshipID, supersedeReq(old.RecordVersion))
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipProposed, n.Status)
	require.NotNil(t, n.SupersedesRelationshipID)
	assert.Equal(t, old.RelationshipID, *n.SupersedesRelationshipID)
	assert.Equal(t, parent, n.ParentLegalEntityID, "parent, child and kind come from the original")
	assert.Equal(t, child, n.ChildLegalEntityID)
	assert.InDelta(t, 80, *n.OwnershipPct, 0.0001)
	assert.Equal(t, domain.OwnershipActive, ms.ownership[old.RelationshipID].Status, "the original applies until approval")
}

func TestOwnership_SupersedeRefusals(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	old := activeRelationship(t, svc, parent, child)
	maker := as("maker", "tenant-001")

	t.Run("stale version", func(t *testing.T) {
		_, err := svc.SupersedeOwnershipRelationship(maker, old.RelationshipID, supersedeReq(9))
		require.ErrorIs(t, err, registry.ErrVersionConflict)
	})
	t.Run("not after the original starts", func(t *testing.T) {
		req := supersedeReq(old.RecordVersion)
		req.ValidFrom = old.ValidFrom
		_, err := svc.SupersedeOwnershipRelationship(maker, old.RelationshipID, req)
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("no evidence", func(t *testing.T) {
		req := supersedeReq(old.RecordVersion)
		req.EvidenceRef = ""
		_, err := svc.SupersedeOwnershipRelationship(maker, old.RelationshipID, req)
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("unknown relationship", func(t *testing.T) {
		_, err := svc.SupersedeOwnershipRelationship(maker, uuid.New().String(), supersedeReq(1))
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("not authorized", func(t *testing.T) {
		denied := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})
		_, err := denied.SupersedeOwnershipRelationship(maker, old.RelationshipID, supersedeReq(old.RecordVersion))
		require.ErrorIs(t, err, registry.ErrUnauthorized)
	})
	t.Run("a PROPOSED relationship cannot be superseded", func(t *testing.T) {
		p, err := svc.ProposeOwnershipRelationship(maker, proposeReq(child, seedOperationalEntity(ms, "tenant-001")))
		require.NoError(t, err)
		_, err = svc.SupersedeOwnershipRelationship(maker, p.RelationshipID, supersedeReq(1))
		require.ErrorIs(t, err, registry.ErrInvalidTransition)
	})
}

func endReq(version int64) domain.EndOwnershipRequest {
	return domain.EndOwnershipRequest{
		ExpectedVersion: version, EndDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		EvidenceRef: "disposal-agreement", Reason: "stake sold",
	}
}

func TestOwnership_EndRecordsWhoWhenWhyAndEvidence(t *testing.T) {
	svc, _, parent, child := ownershipSvc(t)
	r := activeRelationship(t, svc, parent, child)

	out, err := svc.EndOwnershipRelationship(as("ender", "tenant-001"), r.RelationshipID, endReq(r.RecordVersion))
	require.NoError(t, err)
	assert.Equal(t, domain.OwnershipEnded, out.Status)
	assert.Equal(t, "ender", *out.EndedByPrincipalID)
	assert.Equal(t, "stake sold", *out.EndReason)
	assert.Equal(t, "disposal-agreement", *out.EndEvidenceRef)
	require.NotNil(t, out.ValidTo)
	assert.True(t, out.ValidTo.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)))
}

func TestOwnership_EndRefusals(t *testing.T) {
	svc, ms, parent, child := ownershipSvc(t)
	r := activeRelationship(t, svc, parent, child)
	ender := as("ender", "tenant-001")

	t.Run("no evidence", func(t *testing.T) {
		req := endReq(r.RecordVersion)
		req.EvidenceRef = " "
		_, err := svc.EndOwnershipRelationship(ender, r.RelationshipID, req)
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("no reason", func(t *testing.T) {
		req := endReq(r.RecordVersion)
		req.Reason = ""
		_, err := svc.EndOwnershipRelationship(ender, r.RelationshipID, req)
		require.ErrorIs(t, err, registry.ErrInvalidInput)
	})
	t.Run("stale version", func(t *testing.T) {
		_, err := svc.EndOwnershipRelationship(ender, r.RelationshipID, endReq(7))
		require.ErrorIs(t, err, registry.ErrVersionConflict)
	})
	t.Run("not authorized", func(t *testing.T) {
		denied := newSvc(t, ms, denyAllAuthZ{}, acceptAllJurisd{})
		_, err := denied.EndOwnershipRelationship(ender, r.RelationshipID, endReq(r.RecordVersion))
		require.ErrorIs(t, err, registry.ErrUnauthorized)
	})
	t.Run("unknown relationship", func(t *testing.T) {
		_, err := svc.EndOwnershipRelationship(ender, uuid.New().String(), endReq(1))
		require.ErrorIs(t, err, registry.ErrNotFound)
	})
	t.Run("already ended", func(t *testing.T) {
		_, err := svc.EndOwnershipRelationship(ender, r.RelationshipID, endReq(r.RecordVersion))
		require.NoError(t, err)
		_, err = svc.EndOwnershipRelationship(ender, r.RelationshipID, endReq(r.RecordVersion+1))
		require.ErrorIs(t, err, registry.ErrInvalidTransition)
	})
}

func (m *memStore) ListOwnershipInForceAt(_ context.Context, asOf time.Time) ([]domain.OwnershipRelationship, error) {
	var out []domain.OwnershipRelationship
	for _, r := range m.ownership {
		if domain.InForceAt(*r, asOf) {
			out = append(out, *r)
		}
	}
	return out, nil
}
