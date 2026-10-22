-- Migration: 000005_quarantine_taxonomy.down.sql
--
-- Reverts the §16 quarantine taxonomy. Check severities return to the old
-- INFO/WARNING/CRITICAL label each check type used to carry. Lost: each
-- entry's severity, reason family and owner, and each job's blocker count.
--
-- Refused while any audit entry names an owner. Severity, reason family and
-- blocker count are derivable again from check_type/violation_type, as the
-- up migration's own backfill derives them, but owner_id is the exception
-- owner an operator named at validation time (MIG-INV-12, MIG-C37): once
-- dropped, who was accountable for a real blocker cannot be reconstructed.
-- Rows backfilled from before 000005 carry no owner by design, so an
-- environment that has applied 000005 but run no validation since can still
-- roll back. The column check keeps this file re-runnable once the column
-- is gone, like every IF EXISTS below.

DO $$
DECLARE
    owned BOOLEAN := FALSE;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = current_schema()
                 AND table_name = 'migration_audit_entries'
                 AND column_name = 'owner_id') THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM migration_audit_entries WHERE owner_id IS NOT NULL)'
            INTO owned;
    END IF;
    IF owned THEN
        RAISE EXCEPTION 'cannot roll back 000005: audit entries with a named owner exist';
    END IF;
END $$;

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
