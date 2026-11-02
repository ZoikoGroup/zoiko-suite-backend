-- Migration: 000006_lifecycle_control_totals_idempotency.up.sql
--
-- Five related controls, each traced to ZS-MIG-001 (see ROUTES.md):
--
--  1. Who submitted the run (requested_by) and an optimistic-concurrency
--     version for ETag / If-Match on every state change of a job.
--  2. Source-to-target control totals (§17 completeness/accuracy,
--     MIG-INV-15, MIG-N01, MIG-N28, MIG-N29): the manifest's declared record
--     count, amount total and optional content hash beside the totals the
--     service computed from the submitted records. Amounts are NUMERIC, never
--     floating point.
--  3. Archive is a soft delete: archived_at / archived_by record who retired
--     the run; nothing is ever physically deleted (§24 "immutable issue
--     history", MIG-INV-21, MIG-INV-23). DELETE on the evidence tables is
--     refused by trigger, which also neutralises the old ON DELETE CASCADE.
--  4. Quarantine items get an explicit disposition state machine
--     (QUARANTINED -> REMEDIATED | WAIVED) and a waiver expiry (§16 MQ1
--     "formally approved, time-limited migration exception"). A decided item
--     can never be changed again (trigger).
--  5. Idempotency-Key replay store (INV-08, MIG-INV-24).
--
-- RLS on the three evidence tables is hardened to FORCE + WITH CHECK, so the
-- table owner is confined too and a row can never be written into another
-- tenant.

-- ── 1 + 2 + 3: jobs ──────────────────────────────────────────────────────────
ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS requested_by            VARCHAR(256),
    ADD COLUMN IF NOT EXISTS version                 INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS archived_at             TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS archived_by             VARCHAR(256),
    ADD COLUMN IF NOT EXISTS control_amount_field    VARCHAR(128),
    ADD COLUMN IF NOT EXISTS source_record_count     INT,
    ADD COLUMN IF NOT EXISTS target_record_count     INT,
    ADD COLUMN IF NOT EXISTS source_amount_total     NUMERIC(28,6),
    ADD COLUMN IF NOT EXISTS target_amount_total     NUMERIC(28,6),
    ADD COLUMN IF NOT EXISTS source_control_hash     CHAR(64),
    ADD COLUMN IF NOT EXISTS target_control_hash     CHAR(64),
    ADD COLUMN IF NOT EXISTS control_totals_matched  BOOLEAN;

ALTER TABLE migration_jobs DROP CONSTRAINT IF EXISTS migration_jobs_status_valid;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_status_valid
    CHECK (status IN ('PENDING', 'VALIDATING', 'COMPLETED', 'FAILED', 'ARCHIVED'));
ALTER TABLE migration_jobs DROP CONSTRAINT IF EXISTS migration_jobs_version_positive;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_version_positive CHECK (version >= 1);
ALTER TABLE migration_jobs DROP CONSTRAINT IF EXISTS migration_jobs_archive_consistent;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_archive_consistent
    CHECK (archived_at IS NULL OR status = 'ARCHIVED');

-- ── 4: quarantine disposition ────────────────────────────────────────────────
ALTER TABLE migration_audit_entries
    ADD COLUMN IF NOT EXISTS status            VARCHAR(16),
    ADD COLUMN IF NOT EXISTS waiver_expires_at TIMESTAMPTZ;

UPDATE migration_audit_entries
   SET status = CASE WHEN is_remediated THEN 'REMEDIATED' ELSE 'QUARANTINED' END
 WHERE status IS NULL;

ALTER TABLE migration_audit_entries
    ALTER COLUMN status SET DEFAULT 'QUARANTINED',
    ALTER COLUMN status SET NOT NULL;
ALTER TABLE migration_audit_entries DROP CONSTRAINT IF EXISTS migration_audit_entries_status_valid;
ALTER TABLE migration_audit_entries ADD CONSTRAINT migration_audit_entries_status_valid
    CHECK (status IN ('QUARANTINED', 'REMEDIATED', 'WAIVED'));
ALTER TABLE migration_audit_entries DROP CONSTRAINT IF EXISTS migration_audit_entries_waiver_has_expiry;
ALTER TABLE migration_audit_entries ADD CONSTRAINT migration_audit_entries_waiver_has_expiry
    CHECK (status <> 'WAIVED' OR waiver_expires_at IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_migration_audit_open
    ON migration_audit_entries(tenant_id, job_id)
    WHERE status = 'QUARANTINED';

-- A decided quarantine item is history: its disposition and evidence can
-- never be rewritten (MIG-N27, §24).
CREATE OR REPLACE FUNCTION migration_audit_entries_freeze_decided()
RETURNS trigger AS $$
BEGIN
    IF OLD.status <> 'QUARANTINED' THEN
        RAISE EXCEPTION 'migration audit entry % is already %, its decision is immutable', OLD.id, OLD.status
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS migration_audit_entries_freeze_decided ON migration_audit_entries;
CREATE TRIGGER migration_audit_entries_freeze_decided
    BEFORE UPDATE ON migration_audit_entries
    FOR EACH ROW EXECUTE FUNCTION migration_audit_entries_freeze_decided();

-- ── 3: no physical deletion of migration evidence ────────────────────────────
CREATE OR REPLACE FUNCTION migration_evidence_reject_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'migration evidence in % is never deleted; archive the job instead', TG_TABLE_NAME
        USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS migration_jobs_reject_delete ON migration_jobs;
CREATE TRIGGER migration_jobs_reject_delete
    BEFORE DELETE ON migration_jobs
    FOR EACH ROW EXECUTE FUNCTION migration_evidence_reject_delete();
DROP TRIGGER IF EXISTS migration_integrity_checks_reject_delete ON migration_integrity_checks;
CREATE TRIGGER migration_integrity_checks_reject_delete
    BEFORE DELETE ON migration_integrity_checks
    FOR EACH ROW EXECUTE FUNCTION migration_evidence_reject_delete();
DROP TRIGGER IF EXISTS migration_audit_entries_reject_delete ON migration_audit_entries;
CREATE TRIGGER migration_audit_entries_reject_delete
    BEFORE DELETE ON migration_audit_entries
    FOR EACH ROW EXECUTE FUNCTION migration_evidence_reject_delete();

-- ── 5: idempotency replay store ──────────────────────────────────────────────
-- One row per (tenant, principal, key). request_sha256 detects a key reused
-- for a different request; response_* is replayed verbatim (TEXT, not JSONB,
-- so the bytes are returned exactly as first sent).
CREATE TABLE IF NOT EXISTS migration_idempotency_keys (
    tenant_id        VARCHAR(64)  NOT NULL,
    principal_id     VARCHAR(256) NOT NULL,
    idempotency_key  VARCHAR(256) NOT NULL,
    operation        VARCHAR(64)  NOT NULL,
    request_sha256   CHAR(64)     NOT NULL,
    response_status  INT          NOT NULL DEFAULT 0,
    response_body    TEXT         NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, principal_id, idempotency_key)
);

ALTER TABLE migration_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_idempotency_keys_isolation ON migration_idempotency_keys;
CREATE POLICY migration_idempotency_keys_isolation ON migration_idempotency_keys FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── RLS hardening on the evidence tables ─────────────────────────────────────
ALTER TABLE migration_jobs FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_integrity_checks FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_audit_entries FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS migration_jobs_isolation ON migration_jobs;
DROP POLICY IF EXISTS migration_integrity_checks_isolation ON migration_integrity_checks;
DROP POLICY IF EXISTS migration_audit_entries_isolation ON migration_audit_entries;
CREATE POLICY migration_jobs_isolation ON migration_jobs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_integrity_checks_isolation ON migration_integrity_checks FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY migration_audit_entries_isolation ON migration_audit_entries FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
