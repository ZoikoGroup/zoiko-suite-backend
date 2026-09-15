-- Migration: 000002_add_audit_exports.up.sql
--
-- AUD-10's export/redact/deliver half. audit_export_requests references an
-- archive in audit-event-store-svc by plain TEXT id, not a foreign key —
-- that is a different service's database; the reference is validated live,
-- via a real HTTP call, at BuildExportPackage time (see
-- internal/archivestore/client.go), never assumed correct at insert time.

CREATE TABLE IF NOT EXISTS audit_export_requests (
    export_id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  VARCHAR(64) NOT NULL,
    legal_entity_id             VARCHAR(64) NOT NULL,
    archive_id                  TEXT        NOT NULL,
    purpose                     TEXT        NOT NULL,
    status                      VARCHAR(32) NOT NULL DEFAULT 'REQUESTED',
    requested_by_principal_id  VARCHAR(128) NOT NULL,
    approved_by_principal_id    VARCHAR(128),
    delivered_to_principal_id  VARCHAR(128),
    failure_reason               TEXT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    approved_at                  TIMESTAMPTZ,
    sealed_at                    TIMESTAMPTZ,
    delivered_at                  TIMESTAMPTZ,
    revoked_at                    TIMESTAMPTZ,

    CONSTRAINT audit_export_requests_status_valid CHECK (
        status IN ('REQUESTED','APPROVED','BUILDING','SEALED','DELIVERED','REVOKED','FAILED')
    ),
    -- The DB-enforced half of AUD-10's approval-segregation control: the
    -- approver can never be recorded as the same principal who requested
    -- the export — defense in depth alongside the app-layer check in
    -- ApproveExport.
    CONSTRAINT audit_export_requests_no_self_approval CHECK (
        approved_by_principal_id IS NULL OR approved_by_principal_id <> requested_by_principal_id
    )
);

CREATE INDEX IF NOT EXISTS idx_audit_export_requests_tenant_status
    ON audit_export_requests (tenant_id, status);

CREATE TABLE IF NOT EXISTS export_manifests (
    manifest_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    export_id     UUID        NOT NULL REFERENCES audit_export_requests(export_id),
    artifact_name TEXT        NOT NULL,
    sha256        TEXT        NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_export_manifests_export ON export_manifests (export_id);

CREATE TABLE IF NOT EXISTS redaction_decisions (
    decision_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    export_id              UUID        NOT NULL REFERENCES audit_export_requests(export_id),
    field_or_scope         TEXT        NOT NULL,
    reason                 TEXT        NOT NULL,
    decided_by_principal_id VARCHAR(128) NOT NULL,
    decided_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_redaction_decisions_export ON redaction_decisions (export_id);

CREATE TABLE IF NOT EXISTS delivery_receipts (
    receipt_id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    export_id                  UUID        NOT NULL REFERENCES audit_export_requests(export_id),
    delivered_to_principal_id  VARCHAR(128) NOT NULL,
    delivery_channel            TEXT        NOT NULL,
    delivered_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_delivery_receipts_export ON delivery_receipts (export_id);

-- export_manifests / redaction_decisions / delivery_receipts are all
-- append-only evidentiary child tables — a package's own manifest, its
-- redaction decisions, and its delivery history are never edited or
-- deleted after the fact.
CREATE OR REPLACE FUNCTION reject_export_child_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_export_manifests_update BEFORE UPDATE ON export_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();
CREATE TRIGGER trg_reject_export_manifests_delete BEFORE DELETE ON export_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();

CREATE TRIGGER trg_reject_redaction_decisions_update BEFORE UPDATE ON redaction_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();
CREATE TRIGGER trg_reject_redaction_decisions_delete BEFORE DELETE ON redaction_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();

CREATE TRIGGER trg_reject_delivery_receipts_update BEFORE UPDATE ON delivery_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();
CREATE TRIGGER trg_reject_delivery_receipts_delete BEFORE DELETE ON delivery_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_export_child_mutation();

-- RLS, FORCE'd (unlike migration 000001's existing tables, which enable
-- but do not force — not this migration's place to change those). NULLIF
-- guards against app.tenant_id being set to the empty string, which must
-- never match a real tenant_id.
ALTER TABLE audit_export_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_export_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY audit_export_requests_tenant_isolation ON audit_export_requests
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- The three child tables carry no tenant_id of their own (they are pure
-- children of audit_export_requests, same shape as this service's existing
-- report_runs → report_definitions relationship) — tenant isolation is
-- enforced by joining through export_id, which is only ever looked up via
-- an already tenant-scoped parent read in the store layer.
