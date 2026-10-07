package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// requireTestDB connects to a real Postgres named by TEST_DATABASE_URL and
// replays every migration from a clean slate — same gating/discovery
// pattern used by every other service touched in this build.
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
		DROP TABLE IF EXISTS deadline_escalations;
		DROP TABLE IF EXISTS deadlines;
		DROP TABLE IF EXISTS task_transitions;
		DROP TABLE IF EXISTS tasks;
		DROP TABLE IF EXISTS cases;
		DROP TABLE IF EXISTS finding_closure_assessments;
		DROP TABLE IF EXISTS remediation_evidence;
		DROP TABLE IF EXISTS management_responses;
		DROP TABLE IF EXISTS scope_limitations;
		DROP TABLE IF EXISTS control_deficiency_records;
		DROP TABLE IF EXISTS materiality_evaluations;
		DROP TABLE IF EXISTS misstatement_records;
		DROP TABLE IF EXISTS audit_findings;
		DROP TABLE IF EXISTS escalation_records;
		DROP TABLE IF EXISTS exception_cases;
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
// test agrees on, so this is a real single-tenant scope, not an unscoped
// call.
func ctx() context.Context { return context.Background() }

// newExceptionCaseForFindingTest inserts the exception_cases fixture row
// directly rather than via store.CreateException: that method's own setRLS
// helper issues `SET LOCAL app.tenant_id = $1`, a bind parameter inside a
// SET statement, which Postgres rejects outright (a pre-existing defect in
// this service, documented and deliberately NOT replicated into any AUD-08
// code — see finding_store.go's own findingSetRLS comment). Fixing that
// existing defect is outside AUD-08's scope, so the fixture works around it.
func newExceptionCaseForFindingTest(t *testing.T, pool *pgxpool.Pool) *domain.ExceptionCase {
	t.Helper()
	c := &domain.ExceptionCase{
		ExceptionCaseID: "excase-" + uuid.New().String(), TenantID: "default",
		LegalEntityID: uuid.New().String(), JurisdictionID: "US", ExceptionType: "AUDIT_FINDING",
		SeverityLevel: domain.SeverityHigh, LinkedObjectType: "JOURNAL_ENTRY", LinkedObjectID: uuid.New().String(),
		Description: "Revenue cutoff exception", CaseStatus: domain.CaseOpen, CreatedBy: "auditor-1",
	}
	_, err := pool.Exec(ctx(), `
		INSERT INTO exception_cases
			(exception_case_id, tenant_id, legal_entity_id, jurisdiction_id, exception_type,
			 severity_level, linked_object_type, linked_object_id, description, case_status, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now(),now())`,
		c.ExceptionCaseID, c.TenantID, c.LegalEntityID, c.JurisdictionID, c.ExceptionType,
		string(c.SeverityLevel), c.LinkedObjectType, c.LinkedObjectID, c.Description, string(c.CaseStatus), c.CreatedBy)
	require.NoError(t, err)
	return c
}

// TestPgStore_CloseFinding_RequiresRemediationEvidence is the real proof
// of AUD-NEG-026 "finding is closed with no closure evidence where
// required -> block closure."
func TestPgStore_CloseFinding_RequiresRemediationEvidence(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c := newExceptionCaseForFindingTest(t, pool)
	finding, err := s.CreateFinding(ctx(), domain.CreateFindingParams{
		ExceptionCaseID: c.ExceptionCaseID, TenantID: "default", LegalEntityID: c.LegalEntityID, EngagementID: uuid.New().String(),
		FindingType: domain.FindingTypeMisstatement, RequiresRemediationEvidence: true, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	_, err = s.CloseFinding(ctx(), domain.CloseFindingParams{FindingID: finding.FindingID, TenantID: "default", ClosedByPrincipalID: "partner-1", ClosureNotes: "done"})
	require.ErrorIs(t, err, domain.ErrClosureEvidenceRequired)

	_, err = s.LinkRemediation(ctx(), domain.LinkRemediationParams{FindingID: finding.FindingID, TenantID: "default", EvidenceRef: "doc-123", RecordedByPrincipalID: "auditor-1"})
	require.NoError(t, err)

	closed, err := s.CloseFinding(ctx(), domain.CloseFindingParams{FindingID: finding.FindingID, TenantID: "default", ClosedByPrincipalID: "partner-1", ClosureNotes: "done"})
	require.NoError(t, err)
	require.Equal(t, domain.FindingClosed, closed.Status)
}

// TestPgStore_AuditFinding_OriginalContentImmutable is the real proof of
// "original finding immutable": a raw UPDATE attempting to change
// finding_type (or any field other than status/reopened_count/closed_at)
// is refused by the trigger.
func TestPgStore_AuditFinding_OriginalContentImmutable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c := newExceptionCaseForFindingTest(t, pool)
	finding, err := s.CreateFinding(ctx(), domain.CreateFindingParams{
		ExceptionCaseID: c.ExceptionCaseID, TenantID: "default", LegalEntityID: c.LegalEntityID, EngagementID: uuid.New().String(),
		FindingType: domain.FindingTypeControlDeficiency, RequiresRemediationEvidence: false, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx(), `UPDATE audit_findings SET finding_type='TAMPERED' WHERE finding_id=$1`, finding.FindingID)
	require.Error(t, err, "expected the reject_finding_mutation trigger to refuse changing finding_type")

	_, err = pool.Exec(ctx(), `DELETE FROM audit_findings WHERE finding_id=$1`, finding.FindingID)
	require.Error(t, err, "expected audit findings to never be deletable")

	// A pure status transition (the legitimate path) is permitted.
	_, err = pool.Exec(ctx(), `UPDATE audit_findings SET status='EVALUATING' WHERE finding_id=$1`, finding.FindingID)
	require.NoError(t, err, "expected a pure status transition to be permitted")
}

// TestPgStore_ScopeLimitation_SurvivesFindingClosure is the real proof of
// AUD-NEG-029: closing a finding never touches scope_limitations, so an
// unresolved limitation remains visible via ListOpenScopeLimitations
// regardless of the parent finding's own status.
func TestPgStore_ScopeLimitation_SurvivesFindingClosure(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool)

	c := newExceptionCaseForFindingTest(t, pool)
	finding, err := s.CreateFinding(ctx(), domain.CreateFindingParams{
		ExceptionCaseID: c.ExceptionCaseID, TenantID: "default", LegalEntityID: c.LegalEntityID, EngagementID: uuid.New().String(),
		FindingType: domain.FindingTypeScopeLimitation, RequiresRemediationEvidence: false, CreatedByPrincipalID: "auditor-1",
	})
	require.NoError(t, err)

	_, err = s.RecordScopeLimitation(ctx(), domain.RecordScopeLimitationParams{FindingID: finding.FindingID, TenantID: "default", Description: "Component auditor documentation inaccessible", RecordedByPrincipalID: "auditor-1"})
	require.NoError(t, err)

	_, err = s.CloseFinding(ctx(), domain.CloseFindingParams{FindingID: finding.FindingID, TenantID: "default", ClosedByPrincipalID: "partner-1", ClosureNotes: "closed despite limitation"})
	require.NoError(t, err)

	open, err := s.ListOpenScopeLimitations(ctx(), "default", finding.FindingID)
	require.NoError(t, err)
	require.Len(t, open, 1, "expected the scope limitation to remain visible as open even after the finding closed")
}
