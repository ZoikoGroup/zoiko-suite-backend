-- Migration: 000005_quarantine_taxonomy.down.sql
--
-- Reverts the §16 quarantine taxonomy. Check severities return to the old
-- INFO/WARNING/CRITICAL label each check type used to carry. Lost: each
-- entry's severity, reason family and owner, and each job's blocker count.

ALTER TABLE migration_jobs
    DROP COLUMN IF EXISTS blocker_count;

ALTER TABLE migration_integrity_checks
    DROP CONSTRAINT IF EXISTS migration_integrity_checks_severity_valid;
UPDATE migration_integrity_checks SET severity = CASE
    WHEN records_failed = 0                                         THEN 'INFO'
    WHEN check_type IN ('SCHEMA_VALIDATION', 'SOURCE_IDENTITY')     THEN 'CRITICAL'
    WHEN check_type IN ('DUPLICATE_DETECTION', 'FORMAT_CHECK')      THEN 'WARNING'
    WHEN severity = 'MQ0'                                           THEN 'CRITICAL'
    WHEN severity IN ('MQ1', 'MQ2')                                 THEN 'WARNING'
    ELSE 'INFO'
END;
ALTER TABLE migration_integrity_checks
    ALTER COLUMN severity TYPE VARCHAR(32),
    ALTER COLUMN severity SET DEFAULT 'INFO';

DROP INDEX IF EXISTS idx_migration_audit_open_severity;
ALTER TABLE migration_audit_entries
    DROP CONSTRAINT IF EXISTS migration_audit_entries_severity_valid,
    DROP CONSTRAINT IF EXISTS migration_audit_entries_reason_family_valid,
    DROP COLUMN IF EXISTS severity,
    DROP COLUMN IF EXISTS reason_family,
    DROP COLUMN IF EXISTS owner_id;
