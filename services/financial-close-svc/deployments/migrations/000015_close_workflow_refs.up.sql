-- Migration: 000015_close_workflow_refs.up.sql
--
-- REF-05 cutover, phase 1. accounting-period-svc (REF-05) will own authoritative
-- period state; before it changes state it calls back
--   GET /v1/close/workflow-refs/{ref}
-- on this service to verify the ACC-14 decision it was handed. This table is that
-- decision record: one row per close/reopen command financial-close-svc has
-- decided to present to REF-05.
--
-- A row is written BEFORE the command is sent to REF-05, in its own committed
-- transaction (REF-05's callback must be able to see it). Rows are permanent:
-- a ref whose REF-05 call failed stays, because deleting evidence of a decision
-- that was made is worse than keeping an unused one.
--
-- control_snapshot_ref is a lowercase sha256 hex digest, computed by
-- internal/periodmirror.SnapshotRef over the canonical JSON document
--   {"v":1,"fiscal_period_id":<uuid>,"command":<cmd>,
--    "evidence_document_id":<string>,
--    "readiness":{"is_ready":<bool>,"blocking_issues":[<sorted strings>]} | null}
-- i.e. the period's readiness snapshot as of the command + the close evidence
-- document id + the period id + the command. "readiness" is null for commands
-- that are not gated on readiness (AUTHORIZE_REOPEN).

CREATE TABLE close_workflow_refs (
    ref_id               UUID PRIMARY KEY,                       -- UUIDv7
    tenant_id            VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    fiscal_period_id     UUID NOT NULL REFERENCES fiscal_periods(fiscal_period_id),
    period_name          VARCHAR(50) NOT NULL,
    period_key           VARCHAR(255) NOT NULL,                  -- REF-05/REF-04 key, e.g. FY2026-P07
    command              VARCHAR(32) NOT NULL
        CHECK (command IN ('SOFT_CLOSE', 'HARD_CLOSE', 'AUTHORIZE_REOPEN', 'RECLOSE')),
    status               VARCHAR(16) NOT NULL
        CHECK (status IN ('APPROVED', 'REJECTED')),
    control_snapshot_ref TEXT NOT NULL
        CHECK (control_snapshot_ref ~ '^[0-9a-f]{64}$'),
    requested_by         VARCHAR(255) NOT NULL,
    reason               TEXT NOT NULL,
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION reject_close_workflow_ref_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'close_workflow_refs is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_close_workflow_ref_update
    BEFORE UPDATE ON close_workflow_refs
    FOR EACH ROW EXECUTE FUNCTION reject_close_workflow_ref_mutation();
CREATE TRIGGER trg_reject_close_workflow_ref_delete
    BEFORE DELETE ON close_workflow_refs
    FOR EACH ROW EXECUTE FUNCTION reject_close_workflow_ref_mutation();
CREATE TRIGGER trg_reject_close_workflow_ref_truncate
    BEFORE TRUNCATE ON close_workflow_refs
    FOR EACH STATEMENT EXECUTE FUNCTION reject_close_workflow_ref_mutation();

ALTER TABLE close_workflow_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE close_workflow_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON close_workflow_refs
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_close_workflow_refs_period ON close_workflow_refs (tenant_id, fiscal_period_id, created_at);
