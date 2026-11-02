-- Migration: 000006_lifecycle_control_totals_idempotency.down.sql
--
-- Reverses 000006. Lost on the way down: who submitted each run, job
-- versions, archive actor/time, control totals, waiver dispositions and the
-- idempotency replay store. A WAIVED entry is returned to "not remediated"
-- (is_remediated stays FALSE), which is what it was before 000006 existed.

DROP POLICY IF EXISTS migration_jobs_isolation ON migration_jobs;
DROP POLICY IF EXISTS migration_integrity_checks_isolation ON migration_integrity_checks;
DROP POLICY IF EXISTS migration_audit_entries_isolation ON migration_audit_entries;
CREATE POLICY migration_jobs_isolation ON migration_jobs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_integrity_checks_isolation ON migration_integrity_checks
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_audit_entries_isolation ON migration_audit_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
ALTER TABLE migration_jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_integrity_checks NO FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_audit_entries NO FORCE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS migration_idempotency_keys;

DROP TRIGGER IF EXISTS migration_jobs_reject_delete ON migration_jobs;
DROP TRIGGER IF EXISTS migration_integrity_checks_reject_delete ON migration_integrity_checks;
DROP TRIGGER IF EXISTS migration_audit_entries_reject_delete ON migration_audit_entries;
DROP FUNCTION IF EXISTS migration_evidence_reject_delete();

DROP TRIGGER IF EXISTS migration_audit_entries_freeze_decided ON migration_audit_entries;
DROP FUNCTION IF EXISTS migration_audit_entries_freeze_decided();

DROP INDEX IF EXISTS idx_migration_audit_open;
ALTER TABLE migration_audit_entries
    DROP CONSTRAINT IF EXISTS migration_audit_entries_waiver_has_expiry,
    DROP CONSTRAINT IF EXISTS migration_audit_entries_status_valid,
    DROP COLUMN IF EXISTS waiver_expires_at,
    DROP COLUMN IF EXISTS status;

ALTER TABLE migration_jobs
    DROP CONSTRAINT IF EXISTS migration_jobs_archive_consistent,
    DROP CONSTRAINT IF EXISTS migration_jobs_version_positive,
    DROP CONSTRAINT IF EXISTS migration_jobs_status_valid,
    DROP COLUMN IF EXISTS control_totals_matched,
    DROP COLUMN IF EXISTS target_control_hash,
    DROP COLUMN IF EXISTS source_control_hash,
    DROP COLUMN IF EXISTS target_amount_total,
    DROP COLUMN IF EXISTS source_amount_total,
    DROP COLUMN IF EXISTS target_record_count,
    DROP COLUMN IF EXISTS source_record_count,
    DROP COLUMN IF EXISTS control_amount_field,
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS requested_by;
