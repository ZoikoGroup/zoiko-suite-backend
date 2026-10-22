//go:build integration

// §16 quarantine taxonomy (SPEC_DEVIATIONS A6): severity, reason family and
// owner persist on each audit entry, the job's MQ0 blocker count persists,
// the schema refuses anything outside the spec's vocabulary, and migration
// 000005 backfills legacy rows exactly and reverses cleanly.
package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

func TestPgStore_QuarantineClassificationRoundTrips(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	tenantID := uuid.NewString()
	now := time.Now().UTC()

	job := &domain.MigrationJob{
		ID: uuid.NewString(), LegalEntityID: "le-uk-1", MigrationName: "taxonomy round trip",
		SourceSystem: "LEGACY_ERP", TargetService: "general-ledger-svc",
		TotalRecordsCount: 10, ValidRecordsCount: 9, InvalidRecordsCount: 1,
		IntegrityScore: 90, IntegrityScoreThreshold: 0, BlockerCount: 1,
		Status: domain.JobStatusFailed, StartedAt: &now, CompletedAt: &now,
	}
	checks := []domain.IntegrityCheck{{CheckName: "Source Identity Presence", CheckType: domain.CheckSourceIdentity,
		RecordsChecked: 10, RecordsPassed: 9, RecordsFailed: 1, Severity: domain.SeverityMQ0}}
	entries := []domain.AuditEntry{{RecordRef: "", ViolationType: domain.ViolationMissingSourceIdentity,
		Severity: domain.SeverityMQ0, ReasonFamily: domain.ReasonIdentity, OwnerID: "owner-9"}}
	require.NoError(t, s.CreateJob(ctx, tenantID, job, checks, entries, "principal-1", "corr-1"))

	read, err := s.GetJobByID(ctx, tenantID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, read.BlockerCount)
	require.Len(t, read.IntegrityChecks, 1)
	assert.Equal(t, domain.SeverityMQ0, read.IntegrityChecks[0].Severity)
	require.Len(t, read.AuditEntries, 1)
	e := read.AuditEntries[0]
	assert.Equal(t, domain.SeverityMQ0, e.Severity)
	assert.Equal(t, domain.ReasonIdentity, e.ReasonFamily)
	assert.Equal(t, "owner-9", e.OwnerID)

	jobs, err := s.ListJobs(ctx, tenantID, "le-uk-1", "")
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, 1, jobs[0].BlockerCount, "list must carry the blocker count: it explains a FAILED job without its entries")

	rem, err := s.RemediateEntry(ctx, tenantID, job.ID, e.ID, "identity restored", "ticket-1", "owner-9", "corr-2")
	require.NoError(t, err)
	assert.Equal(t, domain.SeverityMQ0, rem.Severity)
	assert.Equal(t, domain.ReasonIdentity, rem.ReasonFamily)
	assert.Equal(t, "owner-9", rem.OwnerID)

	// The validated event tells a consumer why the job failed.
	rows := outboxRows(t, tenantID, job.ID)
	require.NotEmpty(t, rows)
	assert.Contains(t, string(rows[0].payload), `"blocker_count":1`)
}

// The vocabulary is enforced by the schema, not only by the Go types: an
// entry outside it is refused and the whole job rolls back with it.
func TestPgStore_RefusesUnknownSeverityOrFamily(t *testing.T) {
	s := newStore()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		entry domain.AuditEntry
	}{
		{"legacy severity label", domain.AuditEntry{RecordRef: "r", ViolationType: domain.ViolationDuplicate,
			Severity: "CRITICAL", ReasonFamily: domain.ReasonIdentity}},
		{"unclassified severity", domain.AuditEntry{RecordRef: "r", ViolationType: domain.ViolationDuplicate,
			ReasonFamily: domain.ReasonIdentity}},
		{"unknown reason family", domain.AuditEntry{RecordRef: "r", ViolationType: domain.ViolationDuplicate,
			Severity: domain.SeverityMQ1, ReasonFamily: "MISC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := uuid.NewString()
			now := time.Now().UTC()
			job := &domain.MigrationJob{ID: uuid.NewString(), LegalEntityID: "le-uk-1", MigrationName: "bad",
				SourceSystem: "s", TargetService: "t", Status: domain.JobStatusFailed, StartedAt: &now, CompletedAt: &now}
			err := s.CreateJob(ctx, tenantID, job, nil, []domain.AuditEntry{tc.entry}, "p", "c")
			require.Error(t, err)
			var n int
			require.NoError(t, superPool.QueryRow(ctx, `SELECT count(*) FROM migration_jobs WHERE id = $1`, job.ID).Scan(&n))
			assert.Zero(t, n, "the job must roll back with its refused entry")
		})
	}
}

// Migration 000005 against rows written under 000004: severities and
// families are backfilled exactly from what produced them, no owner is
// invented, blocker counts are derived, stored statuses are left alone, and
// the down migration restores the old labels.
func TestMigration000005_BackfillsLegacyRowsAndReverses(t *testing.T) {
	ctx := context.Background()
	dbName := "mig_000005_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := superPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = superPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)") })

	cfg := superPool.Config().ConnConfig
	// No statement cache: the test runs DDL that changes column types between
	// queries, which would invalidate cached prepared statements.
	poolCfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName))
	require.NoError(t, err)
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	defer pool.Close()

	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	sort.Strings(files)
	exec := func(path string) {
		t.Helper()
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(b))
		require.NoError(t, err, path)
	}
	var up5 string
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "000005_") {
			up5 = f
			continue
		}
		if filepath.Base(f) > "000005" {
			t.Fatalf("migration %s after 000005: extend this test to stop at 000004", f)
		}
		exec(f)
	}
	require.NotEmpty(t, up5)

	// A pre-000005 job: COMPLETED with a blank-ref entry and a duplicate.
	jobID := uuid.NewString()
	// Several statements in one call cannot take parameters; jobID is a
	// generated UUID, so substituting it is safe.
	_, err = pool.Exec(ctx, strings.ReplaceAll(`
		INSERT INTO migration_jobs (id, tenant_id, legal_entity_id, migration_name, source_system, target_service, status)
		VALUES ('$1', 't', 'le', 'legacy', 's', 't', 'COMPLETED');
		INSERT INTO migration_integrity_checks (tenant_id, job_id, check_name, check_type, records_failed, severity) VALUES
		  ('t', '$1', 'identity', 'SOURCE_IDENTITY', 1, 'CRITICAL'),
		  ('t', '$1', 'dup',      'DUPLICATE_DETECTION', 2, 'WARNING'),
		  ('t', '$1', 'schema',   'SCHEMA_VALIDATION', 0, 'INFO'),
		  ('t', '$1', 'format',   'FORMAT_CHECK', 1, 'WARNING');
		INSERT INTO migration_audit_entries (tenant_id, job_id, record_ref, violation_type) VALUES
		  ('t', '$1', '',  'MISSING_SOURCE_IDENTITY'),
		  ('t', '$1', 'D', 'DUPLICATE'),
		  ('t', '$1', 'M', 'MISSING_REQUIRED'),
		  ('t', '$1', 'T', 'TYPE_MISMATCH');`, "$1", jobID))
	require.NoError(t, err)

	exec(up5)

	entries := map[string][3]*string{}
	rows, err := pool.Query(ctx, `SELECT violation_type, severity, reason_family, owner_id FROM migration_audit_entries WHERE job_id = $1`, jobID)
	require.NoError(t, err)
	for rows.Next() {
		var vt string
		var sev, fam, owner *string
		require.NoError(t, rows.Scan(&vt, &sev, &fam, &owner))
		entries[vt] = [3]*string{sev, fam, owner}
	}
	require.NoError(t, rows.Err())
	rows.Close()
	for vt, want := range map[string][2]string{
		"MISSING_SOURCE_IDENTITY": {"MQ0", "IDENTITY"},
		"DUPLICATE":               {"MQ1", "IDENTITY"},
		"TYPE_MISMATCH":           {"MQ1", "SCHEMA"},
		"MISSING_REQUIRED":        {"MQ2", "SCHEMA"},
	} {
		got := entries[vt]
		require.NotNil(t, got[0], vt)
		assert.Equal(t, want[0], *got[0], vt+" severity")
		assert.Equal(t, want[1], *got[1], vt+" reason family")
		assert.Nil(t, got[2], vt+": no owner was ever recorded, none may be invented")
	}

	checkSev := func() map[string]string {
		m := map[string]string{}
		rows, err := pool.Query(ctx, `SELECT check_type, severity FROM migration_integrity_checks WHERE job_id = $1`, jobID)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var ct, sev string
			require.NoError(t, rows.Scan(&ct, &sev))
			m[ct] = sev
		}
		require.NoError(t, rows.Err())
		return m
	}
	assert.Equal(t, map[string]string{"SOURCE_IDENTITY": "MQ0", "DUPLICATE_DETECTION": "MQ1",
		"SCHEMA_VALIDATION": "MQ4", "FORMAT_CHECK": "MQ1"}, checkSev())

	var blockers int
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT blocker_count, status FROM migration_jobs WHERE id = $1`, jobID).Scan(&blockers, &status))
	assert.Equal(t, 1, blockers)
	assert.Equal(t, "COMPLETED", status, "a stored acceptance decision is history and is not recomputed")

	// Down restores the old labels and drops the new columns.
	exec(strings.Replace(up5, ".up.sql", ".down.sql", 1))
	assert.Equal(t, map[string]string{"SOURCE_IDENTITY": "CRITICAL", "DUPLICATE_DETECTION": "WARNING",
		"SCHEMA_VALIDATION": "INFO", "FORMAT_CHECK": "WARNING"}, checkSev())
	var cols int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'migration_audit_entries' AND column_name IN ('severity', 'reason_family', 'owner_id'))
		   OR (table_name = 'migration_jobs' AND column_name = 'blocker_count')`).Scan(&cols))
	assert.Zero(t, cols)

	// And up again: the migration is re-runnable after a rollback.
	exec(up5)
	assert.Equal(t, "MQ0", checkSev()["SOURCE_IDENTITY"])

	// Once an entry names an owner, the down refuses: the owner cannot be
	// reconstructed after the column is dropped. Nothing is dropped or
	// relabelled.
	_, err = pool.Exec(ctx, `INSERT INTO migration_audit_entries
		(tenant_id, job_id, record_ref, violation_type, severity, reason_family, owner_id)
		VALUES ('t', $1, '', 'MISSING_SOURCE_IDENTITY', 'MQ0', 'IDENTITY', 'owner-1')`, jobID)
	require.NoError(t, err)
	b, err := os.ReadFile(strings.Replace(up5, ".up.sql", ".down.sql", 1))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(b))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit entries with a named owner exist")
	assert.Equal(t, "MQ0", checkSev()["SOURCE_IDENTITY"], "a refused rollback must leave the taxonomy in place")
	var owner string
	require.NoError(t, pool.QueryRow(ctx, `SELECT owner_id FROM migration_audit_entries WHERE owner_id IS NOT NULL`).Scan(&owner))
	assert.Equal(t, "owner-1", owner)
}
