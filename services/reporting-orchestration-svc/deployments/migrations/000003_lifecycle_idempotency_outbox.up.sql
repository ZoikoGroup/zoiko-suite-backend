-- Migration: 000003_lifecycle_idempotency_outbox.up.sql
--
-- Brings reporting-orchestration-svc up to the platform standards:
--
--   1. Report definitions get the REP-05 lifecycle
--      (DRAFT -> VALIDATED -> ACTIVE <-> PAUSED -> DEPRECATED), a version
--      column for ETag / If-Match, and created_by for evidence.
--   2. report_definitions / report_runs get FORCE'd RLS with WITH CHECK and
--      the NULLIF empty-tenant guard (000001 enabled RLS but never forced it).
--   3. Audit exports get the AUD-10 VALIDATING state, a version column,
--      the package seal digest, and who/why for seal and revocation.
--   4. Export child tables (manifests, redactions, delivery receipts) get
--      their own tenant_id and RLS policy - defense in depth on top of the
--      parent lookup the store already performs.
--   5. idempotency_records: Idempotency-Key replay protection (INV-08).
--   6. outbox_events: transactional outbox (Reporting contract "Events").

-- ---------------------------------------------------------------------------
-- 1. Report definition lifecycle
-- ---------------------------------------------------------------------------
ALTER TABLE report_definitions ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
ALTER TABLE report_definitions ADD COLUMN IF NOT EXISTS created_by VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE report_definitions ALTER COLUMN status SET DEFAULT 'DRAFT';

-- ARCHIVED was the pre-REP-05 terminal state; DEPRECATED is the spec name.
UPDATE report_definitions SET status = 'DEPRECATED' WHERE status = 'ARCHIVED';

ALTER TABLE report_definitions DROP CONSTRAINT IF EXISTS report_definitions_status_valid;
ALTER TABLE report_definitions ADD CONSTRAINT report_definitions_status_valid
    CHECK (status IN ('DRAFT', 'VALIDATED', 'ACTIVE', 'PAUSED', 'DEPRECATED'));

ALTER TABLE report_runs ADD COLUMN IF NOT EXISTS requested_by VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE report_runs DROP CONSTRAINT IF EXISTS report_runs_period_order;
ALTER TABLE report_runs ADD CONSTRAINT report_runs_period_order
    CHECK (period_start IS NULL OR period_end IS NULL OR period_start <= period_end);

-- ---------------------------------------------------------------------------
-- 2. FORCE'd RLS on the report tables
-- ---------------------------------------------------------------------------
ALTER TABLE report_definitions FORCE ROW LEVEL SECURITY;
ALTER TABLE report_runs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS report_definitions_tenant_isolation ON report_definitions;
CREATE POLICY report_definitions_tenant_isolation ON report_definitions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS report_runs_tenant_isolation ON report_runs;
CREATE POLICY report_runs_tenant_isolation ON report_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ---------------------------------------------------------------------------
-- 3. Audit export lifecycle columns
-- ---------------------------------------------------------------------------
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS seal_digest TEXT;
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS sealed_by_principal_id VARCHAR(128);
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS revoked_by_principal_id VARCHAR(128);
ALTER TABLE audit_export_requests ADD COLUMN IF NOT EXISTS revocation_reason TEXT;

ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_status_valid;
ALTER TABLE audit_export_requests ADD CONSTRAINT audit_export_requests_status_valid CHECK (
    status IN ('REQUESTED', 'APPROVED', 'BUILDING', 'VALIDATING', 'SEALED', 'DELIVERED', 'REVOKED', 'FAILED')
);

-- A revocation is never recorded without who and why.
ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_revocation_reason;
ALTER TABLE audit_export_requests ADD CONSTRAINT audit_export_requests_revocation_reason CHECK (
    status <> 'REVOKED' OR (revocation_reason IS NOT NULL AND revocation_reason <> '' AND revoked_by_principal_id IS NOT NULL)
);

-- A sealed (or later) package always carries its seal digest.
ALTER TABLE audit_export_requests DROP CONSTRAINT IF EXISTS audit_export_requests_seal_digest;
ALTER TABLE audit_export_requests ADD CONSTRAINT audit_export_requests_seal_digest CHECK (
    sealed_at IS NULL OR seal_digest IS NOT NULL
);

-- ---------------------------------------------------------------------------
-- 4. Tenant column + RLS on the export child tables
-- ---------------------------------------------------------------------------
-- The append-only triggers from 000002 forbid UPDATE, so they are disabled
-- for the one-off backfill and re-enabled immediately afterwards.
ALTER TABLE export_manifests ADD COLUMN IF NOT EXISTS tenant_id VARCHAR(64);
ALTER TABLE redaction_decisions ADD COLUMN IF NOT EXISTS tenant_id VARCHAR(64);
ALTER TABLE delivery_receipts ADD COLUMN IF NOT EXISTS tenant_id VARCHAR(64);

ALTER TABLE export_manifests DISABLE TRIGGER trg_reject_export_manifests_update;
ALTER TABLE redaction_decisions DISABLE TRIGGER trg_reject_redaction_decisions_update;
ALTER TABLE delivery_receipts DISABLE TRIGGER trg_reject_delivery_receipts_update;

UPDATE export_manifests m SET tenant_id = e.tenant_id
    FROM audit_export_requests e WHERE m.export_id = e.export_id AND m.tenant_id IS NULL;
UPDATE redaction_decisions d SET tenant_id = e.tenant_id
    FROM audit_export_requests e WHERE d.export_id = e.export_id AND d.tenant_id IS NULL;
UPDATE delivery_receipts r SET tenant_id = e.tenant_id
    FROM audit_export_requests e WHERE r.export_id = e.export_id AND r.tenant_id IS NULL;

ALTER TABLE export_manifests ENABLE TRIGGER trg_reject_export_manifests_update;
ALTER TABLE redaction_decisions ENABLE TRIGGER trg_reject_redaction_decisions_update;
ALTER TABLE delivery_receipts ENABLE TRIGGER trg_reject_delivery_receipts_update;

ALTER TABLE export_manifests ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE redaction_decisions ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE delivery_receipts ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE export_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE export_manifests FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS export_manifests_tenant_isolation ON export_manifests;
CREATE POLICY export_manifests_tenant_isolation ON export_manifests
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE redaction_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE redaction_decisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS redaction_decisions_tenant_isolation ON redaction_decisions;
CREATE POLICY redaction_decisions_tenant_isolation ON redaction_decisions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE delivery_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_receipts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS delivery_receipts_tenant_isolation ON delivery_receipts;
CREATE POLICY delivery_receipts_tenant_isolation ON delivery_receipts
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE INDEX IF NOT EXISTS idx_audit_export_requests_tenant_created
    ON audit_export_requests (tenant_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 5. Idempotency-Key records
-- ---------------------------------------------------------------------------
-- One row per (tenant, key). The row is reserved IN_PROGRESS before the
-- command runs and COMPLETED with the response once it succeeds, so a retry
-- replays the recorded response instead of executing the command twice.
-- request_fingerprint (sha256 of method + path + body) detects a key being
-- reused for a different request.
CREATE TABLE IF NOT EXISTS idempotency_records (
    tenant_id           VARCHAR(64)  NOT NULL,
    idempotency_key     VARCHAR(255) NOT NULL,
    request_fingerprint CHAR(64)     NOT NULL,
    state               VARCHAR(16)  NOT NULL,
    response_status     INT,
    response_body       BYTEA,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, idempotency_key),
    CONSTRAINT idempotency_records_state_valid CHECK (state IN ('IN_PROGRESS', 'COMPLETED'))
);

ALTER TABLE idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_records_tenant_isolation ON idempotency_records;
CREATE POLICY idempotency_records_tenant_isolation ON idempotency_records
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ---------------------------------------------------------------------------
-- 6. Transactional outbox
-- ---------------------------------------------------------------------------
-- Written in the same transaction as the state change it describes; the
-- relay (internal/events/relay.go) publishes unpublished rows to Kafka.
-- Deliberately no RLS: the relay is cross-tenant infrastructure with no
-- request tenant (same decision as general-ledger-svc's outbox_events).
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        VARCHAR(64)  NOT NULL,
    legal_entity_id  VARCHAR(64)  NOT NULL DEFAULT '',
    actor_id         VARCHAR(255) NOT NULL DEFAULT '',
    correlation_id   VARCHAR(255) NOT NULL DEFAULT '',
    payload          JSONB        NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at     TIMESTAMPTZ,
    publish_attempts INT          NOT NULL DEFAULT 0,
    last_error       TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished
    ON outbox_events (created_at ASC) WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_outbox_events_aggregate
    ON outbox_events (tenant_id, aggregate_type, aggregate_id);
