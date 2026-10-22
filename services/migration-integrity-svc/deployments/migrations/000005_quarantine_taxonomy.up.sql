-- Migration: 000005_quarantine_taxonomy.up.sql
--
-- ZS-MIG-001 §16 quarantine taxonomy (SPEC_DEVIATIONS A6, B3).
--
-- Severity moves from a flat INFO/WARNING/CRITICAL label on each check to the
-- spec's MQ0-MQ4 classes, each with a required disposition: MQ0 stops the
-- run (the job is FAILED at any score), MQ1 must be resolved or excepted,
-- MQ2 is left to the acceptance threshold, MQ3/MQ4 are tracked. Each audit
-- entry gains its own severity, a §16 reason family (a separate dimension
-- from violation_type) and an owner (MIG-INV-12, MIG-C37). Each job stores
-- how many MQ0 blockers it had.
--
-- Backfill is exact, not a relabelling of the old scale: every violation
-- type and check type this service has ever produced came from exactly one
-- check, whose classification is fixed in domain.PerformIntegrityValidation.
-- Existing entries get no owner -- none was ever recorded, and inventing one
-- would fabricate accountability. Existing job statuses are NOT recomputed:
-- a stored acceptance decision is history, and blocker_count records what the
-- new rule would have said beside it.

-- ── Audit entries ─────────────────────────────────────────────────────────────
ALTER TABLE migration_audit_entries
    ADD COLUMN IF NOT EXISTS severity VARCHAR(8),
    ADD COLUMN IF NOT EXISTS reason_family VARCHAR(16),
    ADD COLUMN IF NOT EXISTS owner_id VARCHAR(256);

UPDATE migration_audit_entries SET
    severity = CASE violation_type
        WHEN 'MISSING_SOURCE_IDENTITY' THEN 'MQ0'
        WHEN 'DUPLICATE'               THEN 'MQ1'
        WHEN 'TYPE_MISMATCH'           THEN 'MQ1'
        ELSE 'MQ2' -- MISSING_REQUIRED; OUT_OF_RANGE/ORPHANED_REFERENCE were never produced
    END,
    reason_family = CASE violation_type
        WHEN 'MISSING_SOURCE_IDENTITY' THEN 'IDENTITY'
        WHEN 'DUPLICATE'               THEN 'IDENTITY'
        WHEN 'ORPHANED_REFERENCE'      THEN 'REFERENCE'
        ELSE 'SCHEMA'
    END
WHERE severity IS NULL OR reason_family IS NULL;

ALTER TABLE migration_audit_entries
    ALTER COLUMN severity SET NOT NULL,
    ALTER COLUMN reason_family SET NOT NULL;

ALTER TABLE migration_audit_entries
    DROP CONSTRAINT IF EXISTS migration_audit_entries_severity_valid,
    DROP CONSTRAINT IF EXISTS migration_audit_entries_reason_family_valid;
ALTER TABLE migration_audit_entries
    ADD CONSTRAINT migration_audit_entries_severity_valid
        CHECK (severity IN ('MQ0', 'MQ1', 'MQ2', 'MQ3', 'MQ4')),
    ADD CONSTRAINT migration_audit_entries_reason_family_valid
        CHECK (reason_family IN ('IDENTITY', 'REFERENCE', 'TEMPORAL', 'FINANCIAL', 'SUBLEDGER',
                                 'TAX', 'DOCUMENT', 'SCHEMA', 'SECURITY', 'SOURCE'));

-- Open quarantine items by severity (§26 quarantine_age_by_severity).
CREATE INDEX IF NOT EXISTS idx_migration_audit_open_severity
    ON migration_audit_entries(tenant_id, severity, created_at)
    WHERE is_remediated = FALSE;

-- ── Integrity checks ──────────────────────────────────────────────────────────
-- A check that failed carries the class of the violations it raises; a check
-- that found nothing is MQ4. The old label is the fallback only for check
-- types that were declared but never produced.
UPDATE migration_integrity_checks SET severity = CASE
    WHEN records_failed = 0                    THEN 'MQ4'
    WHEN check_type = 'SOURCE_IDENTITY'        THEN 'MQ0'
    WHEN check_type = 'DUPLICATE_DETECTION'    THEN 'MQ1'
    WHEN check_type = 'FORMAT_CHECK'           THEN 'MQ1'
    WHEN check_type = 'SCHEMA_VALIDATION'      THEN 'MQ2'
    WHEN severity = 'CRITICAL'                 THEN 'MQ0'
    WHEN severity = 'WARNING'                  THEN 'MQ2'
    ELSE 'MQ4'
END
WHERE severity NOT IN ('MQ0', 'MQ1', 'MQ2', 'MQ3', 'MQ4');

ALTER TABLE migration_integrity_checks
    ALTER COLUMN severity TYPE VARCHAR(8),
    ALTER COLUMN severity SET DEFAULT 'MQ4';
ALTER TABLE migration_integrity_checks
    DROP CONSTRAINT IF EXISTS migration_integrity_checks_severity_valid;
ALTER TABLE migration_integrity_checks
    ADD CONSTRAINT migration_integrity_checks_severity_valid
        CHECK (severity IN ('MQ0', 'MQ1', 'MQ2', 'MQ3', 'MQ4'));

-- ── Jobs ──────────────────────────────────────────────────────────────────────
ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS blocker_count INT NOT NULL DEFAULT 0;

UPDATE migration_jobs j SET blocker_count = b.n
FROM (
    SELECT job_id, COUNT(*) AS n
    FROM migration_audit_entries
    WHERE severity = 'MQ0'
    GROUP BY job_id
) b
WHERE j.id = b.job_id;
