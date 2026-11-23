//go:build integration

// Proof that MigrationProgram aggregates its batches' live status, and that
// a SourceExtract is genuinely immutable: the database itself refuses an
// UPDATE/DELETE/TRUNCATE against migration_source_extracts (migration
// 000010's triggers), and a "correction" is a new row with the next
// version number, never a rewrite of the old one (MIG-INV-03).
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

func TestPgStore_Program_CreateAndAggregateBatches(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	tenantID := uuid.NewString()

	program, err := s.CreateProgram(ctx, tenantID, &domain.MigrationProgram{
		ProgramID: "ACME-ONB-2026", SourceEstate: "LEGACY_ERP", TargetScope: "general-ledger-svc",
		OwnerPrincipalID: "owner-1", CutoverStrategy: "PHASED", CreatedBy: "principal-1",
	}, "corr-1")
	require.NoError(t, err)
	assert.Equal(t, domain.ProgramOpen, program.Status)

	// A program with no batches yet summarizes as empty, not an error.
	summary, err := s.GetProgramBatchSummary(ctx, tenantID, program.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, summary.TotalBatches)

	// Promote two jobs' batch refs under this program.
	job1 := createJob(t, s, tenantID, 0)
	job2 := createJob(t, s, tenantID, 0)
	ref1, err := s.CreateBatchRef(ctx, tenantID, &domain.MigrationBatchRef{
		LegalEntityID: "le-uk-1", ProgramID: program.ID, JobID: job1.ID, BatchCode: "B1", GLBookID: "book-1",
	}, "principal-1", "corr-2")
	require.NoError(t, err)
	_, err = s.CreateBatchRef(ctx, tenantID, &domain.MigrationBatchRef{
		LegalEntityID: "le-uk-1", ProgramID: program.ID, JobID: job2.ID, BatchCode: "B2", GLBookID: "book-1",
	}, "principal-1", "corr-3")
	require.NoError(t, err)

	summary, err = s.GetProgramBatchSummary(ctx, tenantID, program.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.TotalBatches)
	assert.Equal(t, 2, summary.StatusCounts[string(domain.BatchRefPending)])
	assert.Equal(t, 0, summary.ReconciledCount)

	// Advance one batch to RECONCILED and re-check the summary reflects it live.
	_, err = s.UpdateBatchRef(ctx, tenantID, ref1.ID, domain.BatchRefReconciled, "gl-batch-1", "gl-journal-1", "JRN-1", "MATCHED", "", "principal-1", "corr-4")
	require.NoError(t, err)
	summary, err = s.GetProgramBatchSummary(ctx, tenantID, program.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.StatusCounts[string(domain.BatchRefReconciled)])
	assert.Equal(t, 1, summary.StatusCounts[string(domain.BatchRefPending)])
	assert.Equal(t, 1, summary.ReconciledCount)

	// Program status transition: OPEN -> IN_PROGRESS -> COMPLETE, forward only.
	next, err := domain.NextProgramStatus(program.Status, domain.ProgramInProgress)
	require.NoError(t, err)
	updated, err := s.UpdateProgramStatus(ctx, tenantID, program.ID, next, "principal-1", "corr-5")
	require.NoError(t, err)
	assert.Equal(t, domain.ProgramInProgress, updated.Status)

	_, err = domain.NextProgramStatus(updated.Status, domain.ProgramOpen)
	assert.Error(t, err, "a program may not move backward from IN_PROGRESS to OPEN")

	_, err = domain.NextProgramStatus(domain.ProgramOpen, domain.ProgramComplete)
	assert.Error(t, err, "a program may not skip IN_PROGRESS straight to COMPLETE")
}

func TestPgStore_Extract_RegisterAndVersion(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	tenantID := uuid.NewString()

	v1, err := s.CreateExtract(ctx, tenantID, &domain.SourceExtract{
		ExtractID: "AP-LEDGER-2026-01", LegalEntityID: "le-uk-1",
		SourceWatermark: "cursor-0001", GeneratedAt: time.Now().UTC(), ManifestHash: "sha256:aaa",
		RecordCount: 100, Files: []string{"s3://bucket/ap-2026-01/part-0.csv"},
		AmountControls: map[string]domain.ExtractAmountControl{"ap_total": {Total: "100000.00", Count: 100, Ref: "AP-CTRL-1"}},
	}, "principal-1", "corr-1")
	require.NoError(t, err)
	assert.Equal(t, 1, v1.Version)

	// A correction: same business extract_id, no caller-supplied version --
	// the store computes version = previous max + 1.
	v2, err := s.CreateExtract(ctx, tenantID, &domain.SourceExtract{
		ExtractID: "AP-LEDGER-2026-01", LegalEntityID: "le-uk-1",
		SourceWatermark: "cursor-0002", GeneratedAt: time.Now().UTC(), ManifestHash: "sha256:bbb",
		RecordCount: 101, Files: []string{"s3://bucket/ap-2026-01/part-0-corrected.csv"},
	}, "principal-1", "corr-2")
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version)
	assert.NotEqual(t, v1.ID, v2.ID, "a correction is a new row, not a rewrite of the old one")

	latest, err := s.GetLatestExtractByBusinessID(ctx, tenantID, "AP-LEDGER-2026-01")
	require.NoError(t, err)
	assert.Equal(t, v2.ID, latest.ID)
	assert.Equal(t, 2, latest.Version)

	versions, err := s.ListExtractVersions(ctx, tenantID, "AP-LEDGER-2026-01")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, "sha256:aaa", versions[0].ManifestHash, "the original version's manifest hash must survive unchanged")
	assert.Equal(t, 2, versions[1].Version)
	assert.Equal(t, "sha256:bbb", versions[1].ManifestHash)

	byID, err := s.GetExtractByID(ctx, tenantID, v1.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"s3://bucket/ap-2026-01/part-0.csv"}, byID.Files)
	assert.Equal(t, "100000.00", byID.AmountControls["ap_total"].Total)
}

// TestPgStore_Extract_ImmutableAtTheDatabase is the proof the task asked
// for: a direct UPDATE/DELETE against migration_source_extracts -- not
// going through this service's own (nonexistent) update endpoint, but
// straight SQL, as a bug or a fix-up script might attempt -- is refused by
// the database itself (migration 000010's triggers), not merely omitted
// from the application's API surface.
func TestPgStore_Extract_ImmutableAtTheDatabase(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	tenantID := uuid.NewString()

	e, err := s.CreateExtract(ctx, tenantID, &domain.SourceExtract{
		ExtractID: "IMMUTABLE-TEST", LegalEntityID: "le-uk-1",
		SourceWatermark: "cursor-0001", GeneratedAt: time.Now().UTC(), ManifestHash: "sha256:ccc",
		RecordCount: 5, Files: []string{"f1.csv"},
	}, "principal-1", "corr-1")
	require.NoError(t, err)

	// appPool runs as the RLS-respecting application role (see TestMain) --
	// the same role the running service uses, so this proves the service
	// itself cannot mutate an extract, not just that some privileged role
	// theoretically could be stopped. One connection is held for the whole
	// probe (set_config('app.tenant_id', ..., false) is session-scoped, and
	// a pool Exec can otherwise hand out a different connection each time).
	conn, err := appPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenantID)
	require.NoError(t, err)

	_, err = conn.Exec(ctx, `UPDATE migration_source_extracts SET manifest_hash = 'sha256:tampered' WHERE id = $1`, e.ID)
	require.Error(t, err, "an UPDATE against an immutable extract must be refused by the database")
	assert.Contains(t, err.Error(), "immutable", "the refusal must name why, not fail opaquely")

	_, err = conn.Exec(ctx, `DELETE FROM migration_source_extracts WHERE id = $1`, e.ID)
	require.Error(t, err, "a DELETE against an immutable extract must be refused by the database")

	_, err = conn.Exec(ctx, `TRUNCATE migration_source_extracts`)
	require.Error(t, err, "TRUNCATE bypasses row triggers and must be refused by its own statement-level trigger")

	// The correct path still works after every refusal above: register a
	// new version. The original row is completely unaffected.
	v2, err := s.CreateExtract(ctx, tenantID, &domain.SourceExtract{
		ExtractID: "IMMUTABLE-TEST", LegalEntityID: "le-uk-1",
		SourceWatermark: "cursor-0002", GeneratedAt: time.Now().UTC(), ManifestHash: "sha256:ddd",
		RecordCount: 6, Files: []string{"f1-corrected.csv"},
	}, "principal-1", "corr-2")
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version)

	original, err := s.GetExtractByID(ctx, tenantID, e.ID)
	require.NoError(t, err)
	assert.Equal(t, "sha256:ccc", original.ManifestHash, "the original row must be untouched by every refused mutation attempt")
}
