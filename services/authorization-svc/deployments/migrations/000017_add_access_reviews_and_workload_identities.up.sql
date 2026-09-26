-- Migration: 000017_add_access_reviews_and_workload_identities.up.sql
--
-- Adds access_reviews for continuous governance and certification campaigns (ZS-IAM-001 §21, §22, §24)
-- Adds workload_bindings for machine identities, audience binding, and tenant trust flows (ZS-IAM-001 §16, Scenarios A18, A19).

BEGIN;

-- ── 1. access_reviews table ───────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS access_reviews (
    review_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    campaign_id           UUID NOT NULL,
    campaign_name         VARCHAR(128) NOT NULL,
    reviewer_principal_id TEXT NOT NULL,
    target_principal_id   TEXT NOT NULL,
    role_id               VARCHAR(64) NOT NULL,
    legal_entity_id       UUID NOT NULL,
    book_id               VARCHAR(64),
    org_unit_id           VARCHAR(64),
    review_type           VARCHAR(32) NOT NULL, -- PERIODIC, EVENT_TRIGGERED, PRIVILEGED
    status                VARCHAR(32) NOT NULL DEFAULT 'OPEN', -- OPEN, COMPLETED, ESCALATED, EXPIRED
    decision              VARCHAR(32), -- KEEP, REVOKE, MODIFY, ESCALATE
    decision_reason       TEXT,
    decided_at            TIMESTAMPTZ,
    decided_by            TEXT,
    due_at                TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE access_reviews IS
    'Access review attestations and certification campaigns (ZS-IAM-001 §21, §22, §24).';

CREATE INDEX IF NOT EXISTS idx_access_reviews_reviewer
    ON access_reviews (reviewer_principal_id, tenant_id, status);

CREATE INDEX IF NOT EXISTS idx_access_reviews_tenant_status
    ON access_reviews (tenant_id, status);

ALTER TABLE access_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_reviews FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON access_reviews
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ── 2. workload_bindings table ────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS workload_bindings (
    workload_id      TEXT NOT NULL,
    tenant_id        UUID NOT NULL,
    allowed_audience VARCHAR(128) NOT NULL,
    allowed_actions  TEXT[] NOT NULL DEFAULT '{}',
    active_flag      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workload_id, tenant_id)
);

COMMENT ON TABLE workload_bindings IS
    'Workload identity trusted bindings, allowed audience, and actions (ZS-IAM-001 §16, Scenarios A18, A19).';

CREATE INDEX IF NOT EXISTS idx_workload_bindings_lookup
    ON workload_bindings (workload_id, tenant_id) WHERE active_flag;

ALTER TABLE workload_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE workload_bindings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON workload_bindings
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
