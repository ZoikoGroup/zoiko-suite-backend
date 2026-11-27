package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// Child records used to be inserted for any finding_id (no FK), so a typo or
// another tenant's id produced orphan evidence. Each must now be refused with
// ErrFindingNotFound, and still accepted for a real finding.
func TestPgStore_FindingChildren_RequireExistingFinding(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)
	const unknown = "finding-does-not-exist"

	_, err := s.AccumulateMisstatement(ctx(), domain.AccumulateMisstatementParams{FindingID: unknown, TenantID: "default", Amount: 1, RecordedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrFindingNotFound)
	_, err = s.RecordMaterialityEvaluation(ctx(), domain.RecordMaterialityEvaluationParams{FindingID: unknown, TenantID: "default", EvaluatedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrFindingNotFound)
	_, err = s.RecordManagementResponse(ctx(), domain.RecordManagementResponseParams{FindingID: unknown, TenantID: "default", RespondedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrFindingNotFound)
	_, err = s.LinkRemediation(ctx(), domain.LinkRemediationParams{FindingID: unknown, TenantID: "default", EvidenceRef: "ev", RecordedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrFindingNotFound)
	_, err = s.RecordScopeLimitation(ctx(), domain.RecordScopeLimitationParams{FindingID: unknown, TenantID: "default", Description: "d", RecordedByPrincipalID: "p"})
	require.ErrorIs(t, err, domain.ErrFindingNotFound)

	c := newExceptionCaseForFindingTest(t, pool)
	f, err := s.CreateFinding(ctx(), domain.CreateFindingParams{
		ExceptionCaseID: c.ExceptionCaseID, TenantID: "default", LegalEntityID: c.LegalEntityID, EngagementID: uuid.New().String(),
		FindingType: domain.FindingTypeMisstatement, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)
	_, err = s.AccumulateMisstatement(ctx(), domain.AccumulateMisstatementParams{FindingID: f.FindingID, TenantID: "default", Amount: 1, RecordedByPrincipalID: "p"})
	require.NoError(t, err)
	l, err := s.RecordScopeLimitation(ctx(), domain.RecordScopeLimitationParams{FindingID: f.FindingID, TenantID: "default", Description: "d", RecordedByPrincipalID: "p"})
	require.NoError(t, err)
	got, err := s.GetScopeLimitation(ctx(), l.LimitationID)
	require.NoError(t, err)
	require.Equal(t, f.FindingID, got.FindingID)
	_, err = s.GetScopeLimitation(ctx(), "scopelimit-unknown")
	require.ErrorIs(t, err, domain.ErrScopeLimitationNotFound)
}
