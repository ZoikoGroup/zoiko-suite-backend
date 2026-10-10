-- 000004_versioning_revisions_outbox_idempotency.up.sql
-- AP-01 hardening (ZS-SVC-D-001):
--   * version column on the profile (bumped by every change, used for
--     expected_version / STALE_VERSION),
--   * the spec's remaining profile attributes (procurement category refs,
--     tax/withholding classification refs, AP account policy, risk/control
--     flags),
--   * supplier_profile_revisions: append-only full-snapshot history written
--     in the same transaction as every change (as-of reads, history, and the
--     "who last changed payee-related fields" lookup),
--   * outbox_events: transactional outbox (events are written in the same tx
--     as the state change; a relay publishes them to Kafka),
--   * supplier_profile_idempotency: Idempotency-Key results,
--   * a one-live-profile-per-(tenant, legal entity, supplier) unique index,
--   * tightened RLS: no "tenant_id IS NULL" escape hatch.

ALTER TABLE supplier_financial_profiles
    ADD COLUMN version                   INT    NOT NULL DEFAULT 1,
    ADD COLUMN procurement_category_refs TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN tax_classification_refs   TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN ap_account_policy         TEXT,
    ADD COLUMN risk_control_flags        TEXT[] NOT NULL DEFAULT '{}';

-- A supplier has at most one live (non-retired) profile per legal entity.
CREATE UNIQUE INDEX uq_supplier_financial_profiles_live
    ON supplier_financial_profiles (tenant_id, legal_entity_id, supplier_ref)
    WHERE status <> 'RETIRED';

CREATE TABLE supplier_profile_revisions (
    revision_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID        NOT NULL,
    profile_id            UUID        NOT NULL REFERENCES supplier_financial_profiles(profile_id),
    version               INT         NOT NULL,           -- profile version this revision produced
    change_type           VARCHAR(48) NOT NULL,
    -- Full profile state AFTER the change (what an as-of read returns) and
    -- BEFORE it (NULL for the creation revision).
    snapshot              JSONB       NOT NULL,
    prior_snapshot        JSONB,
    actor_principal_id    TEXT        NOT NULL,           -- maker / proposer
    approver_principal_id TEXT,                           -- checker, for approved high-risk changes
    reason                TEXT,
    payee_related         BOOLEAN     NOT NULL DEFAULT FALSE,
    effective_from        TIMESTAMPTZ NOT NULL,           -- effective until the next revision's effective_from
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (profile_id, version)
);
CREATE INDEX idx_supplier_profile_revisions_asof ON supplier_profile_revisions (profile_id, effective_from DESC);
CREATE INDEX idx_supplier_profile_revisions_payee ON supplier_profile_revisions (profile_id, version DESC) WHERE payee_related;

CREATE TRIGGER supplier_profile_revisions_append_only
    BEFORE UPDATE OR DELETE ON supplier_profile_revisions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- Baseline revision (version 1) for profiles that predate this migration: the
-- earliest state that can honestly be reconstructed is the current one.
INSERT INTO supplier_profile_revisions
    (tenant_id, profile_id, version, change_type, snapshot, prior_snapshot, actor_principal_id, reason, payee_related, effective_from)
SELECT tenant_id, profile_id, 1, 'BASELINE',
       jsonb_build_object(
           'profile_id', profile_id, 'tenant_id', tenant_id, 'legal_entity_id', legal_entity_id,
           'supplier_ref', supplier_ref, 'status', status, 'payee_reference', payee_reference,
           'category', category, 'invoice_channel', invoice_channel,
           'payment_method_preference', payment_method_preference, 'tax_withholding_ref', tax_withholding_ref,
           'hold_reason', hold_reason, 'version', 1, 'created_at', created_at,
           'created_by_principal_id', created_by_principal_id, 'updated_at', updated_at),
       NULL, created_by_principal_id, 'baseline at AP-01 versioning migration', FALSE, updated_at
FROM supplier_financial_profiles
WHERE tenant_id IS NOT NULL;

CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64)  NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NULL,
    legal_entity_id  TEXT NOT NULL,
    actor_id         VARCHAR(255) NULL,
    correlation_id   VARCHAR(255) NULL,
    headers          JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ NULL,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT NULL
);
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);
-- outbox_events is an internal queue polled across all tenants by the relay:
-- no RLS and no append-only trigger (the relay updates published_at etc.).

CREATE TABLE supplier_profile_idempotency (
    tenant_id        TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    operation        TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    status_code      INT  NOT NULL,
    response         JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- Tighten RLS: rows are tenant-scoped; a missing tenant context sees nothing.
-- (Legacy rows with a NULL tenant_id become unreachable through the service.)
DROP POLICY IF EXISTS tenant_isolation_policy ON supplier_financial_profiles;
CREATE POLICY tenant_isolation_policy ON supplier_financial_profiles FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON payment_terms_periods;
CREATE POLICY tenant_isolation_policy ON payment_terms_periods FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON high_risk_change_requests;
CREATE POLICY tenant_isolation_policy ON high_risk_change_requests FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON profile_change_events;
CREATE POLICY tenant_isolation_policy ON profile_change_events FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE supplier_profile_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE supplier_profile_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON supplier_profile_revisions FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE supplier_profile_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE supplier_profile_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON supplier_profile_idempotency FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
