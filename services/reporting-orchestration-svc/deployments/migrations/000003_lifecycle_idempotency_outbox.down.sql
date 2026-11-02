-- Migration: 000003_lifecycle_idempotency_outbox.down.sql
--
-- Reverses 000003. Run 000004's down first. Data mapping on the way down:
-- statuses that did not exist before 000003 are folded into the nearest
-- pre-000003 value rather than deleted, so no row is lost:
--   report_definitions  DEPRECATED -> ARCHIVED, DRAFT/VALIDATED -> PAUSED
--   audit_export_requests VALIDATING -> BUILDING
-- idempotency_records and outbox_events are dropped (unpublished outbox
-- events are lost with them - drain the relay before rolling back).

-- 6 / 5. outbox and idempotency
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_records;

DROP INDEX IF EXISTS idx_audit_export_requests_tenant_created;

-- 4. export child tables: drop the per-row tenant column and its policies
DROP POLICY IF EXISTS delivery_receipts_tenant_isolation ON delivery_receipts;
ALTER TABLE delivery_receipts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE delivery_receipts DISABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_receipts DROP COLUMN IF EXISTS tenant_id;

DROP POLICY IF EXISTS redaction_decisions_tenant_isolation ON redaction_decisions;
ALTER TABLE redaction_decisions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE redaction_decisions DISABLE ROW LEVEL SECURITY;
ALTER TABLE redaction_decisions DROP COLUMN IF EXISTS tenant_id;

DROP POLICY IF EXISTS export_manifests_tenant_isolation ON export_manifests;
ALTER TABLE export_manifests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE export_manifests DISABLE ROW LEVEL SECURITY;
ALTER TABLE export_manifests DROP COLUMN IF EXISTS tenant_id;

-- 3. audit export lifecycle columns and checks
ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_seal_digest;
ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_revocation_reason;
ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_status_valid;
UPDATE audit_export_requests SET status = 'BUILDING' WHERE status = 'VALIDATING';
ALTER TABLE audit_export_requests ADD CONSTRAINT audit_export_requests_status_valid CHECK (
    status IN ('REQUESTED','APPROVED','BUILDING','SEALED','DELIVERED','REVOKED','FAILED')
);
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS revocation_reason;
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS revoked_by_principal_id;
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS sealed_by_principal_id;
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS seal_digest;
ALTER TABLE audit_export_requests DROP COLUMN IF EXISTS version;

-- 2. report tables: back to 000001's policies (ENABLEd, not FORCEd)
DROP POLICY IF EXISTS report_runs_tenant_isolation ON report_runs;
CREATE POLICY report_runs_tenant_isolation ON report_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
DROP POLICY IF EXISTS report_definitions_tenant_isolation ON report_definitions;
CREATE POLICY report_definitions_tenant_isolation ON report_definitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
ALTER TABLE report_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE report_definitions NO FORCE ROW LEVEL SECURITY;

-- 1. report definition lifecycle
ALTER TABLE report_runs DROP CONSTRAINT IF EXISTS report_runs_period_order;
ALTER TABLE report_runs DROP COLUMN IF EXISTS requested_by;
ALTER TABLE report_definitions DROP CONSTRAINT IF EXISTS report_definitions_status_valid;
UPDATE report_definitions SET status = 'ARCHIVED' WHERE status = 'DEPRECATED';
UPDATE report_definitions SET status = 'PAUSED' WHERE status IN ('DRAFT', 'VALIDATED');
ALTER TABLE report_definitions ALTER COLUMN status SET DEFAULT 'ACTIVE';
ALTER TABLE report_definitions DROP COLUMN IF EXISTS created_by;
ALTER TABLE report_definitions DROP COLUMN IF EXISTS version;
