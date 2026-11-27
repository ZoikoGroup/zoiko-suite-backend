-- Migration 000002: recommendation lifecycle, human disposition history,
-- Idempotency-Key replay records and the transactional outbox.
--
-- Spec anchors:
--   * ZS-SVC-X-001 AIG-04 §7.3 output disposition state machine
--     (REVIEW_REQUIRED -> ACCEPTED | REJECTED | SUPERSEDED) and NP-40
--     ("Review deadline expires -> Escalate/expire; never auto-accept").
--   * ZS-SVC-X-001 §3.1 OutputDisposition entity: "execution_id, reviewer,
--     required_role, disposition, reason, timestamp, downstream_action_ref
--     ... append-only disposition history."
--   * Data/Search/Analytics/AI spec §2: "Every material AI output retains
--     model/provider version, prompt/template version, retrieval/source
--     references" and "Every analytical/search/AI output declares ...
--     assurance state".
--   * Data/Search/Analytics/AI spec §3 Idempotency: "stable request/run IDs
--     and replay-safe writes".

BEGIN;

-- ── 1. recommendations: lifecycle, provenance and explainability columns ──

ALTER TABLE recommendations
    ADD COLUMN IF NOT EXISTS status                          VARCHAR(20)  NOT NULL DEFAULT 'REVIEW_REQUIRED',
    ADD COLUMN IF NOT EXISTS version                         INTEGER      NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS oversight_class                 VARCHAR(4)   NOT NULL DEFAULT 'O2',
    ADD COLUMN IF NOT EXISTS recommendation_source           VARCHAR(32)  NOT NULL DEFAULT 'PRECEDENT_ENGINE',
    ADD COLUMN IF NOT EXISTS assurance_state                 VARCHAR(20)  NOT NULL DEFAULT 'ADVISORY',
    ADD COLUMN IF NOT EXISTS model_id                        VARCHAR(255) NOT NULL DEFAULT 'decision-support/precedent-frequency',
    ADD COLUMN IF NOT EXISTS model_version                   VARCHAR(64)  NOT NULL DEFAULT '1.0.0',
    ADD COLUMN IF NOT EXISTS prompt_version                  VARCHAR(64),
    ADD COLUMN IF NOT EXISTS history_status                  VARCHAR(20)  NOT NULL DEFAULT 'UNAVAILABLE',
    ADD COLUMN IF NOT EXISTS outcome_distribution            JSONB        NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS evidence_refs                   JSONB        NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS expires_at                      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS decided_by_principal_id         VARCHAR(255),
    ADD COLUMN IF NOT EXISTS decided_at                      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS decision_reason                 TEXT,
    ADD COLUMN IF NOT EXISTS downstream_action_ref           VARCHAR(255),
    ADD COLUMN IF NOT EXISTS approval_reference              VARCHAR(255),
    ADD COLUMN IF NOT EXISTS superseded_by_recommendation_id UUID,
    ADD COLUMN IF NOT EXISTS supersedes_recommendation_id    UUID,
    ADD COLUMN IF NOT EXISTS updated_at                      TIMESTAMPTZ;

UPDATE recommendations SET expires_at = created_at + INTERVAL '72 hours' WHERE expires_at IS NULL;
UPDATE recommendations SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE recommendations ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE recommendations ALTER COLUMN updated_at SET NOT NULL;

ALTER TABLE recommendations DROP CONSTRAINT IF EXISTS recommendations_status_check;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_status_check
    CHECK (status IN ('REVIEW_REQUIRED', 'ACCEPTED', 'REJECTED', 'EXPIRED', 'SUPERSEDED'));

ALTER TABLE recommendations DROP CONSTRAINT IF EXISTS recommendations_oversight_class_check;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_oversight_class_check
    CHECK (oversight_class IN ('O1', 'O2', 'O3'));

-- A decided recommendation always names who decided and when.
ALTER TABLE recommendations DROP CONSTRAINT IF EXISTS recommendations_decision_recorded_check;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_decision_recorded_check
    CHECK (status = 'REVIEW_REQUIRED' OR (decided_by_principal_id IS NOT NULL AND decided_at IS NOT NULL));

-- correlation_id is a trace identifier, not an idempotency key: one business
-- flow may legitimately ask for several recommendations. Replay protection
-- moves to the Idempotency-Key table below.
DROP INDEX IF EXISTS idx_recommendations_tenant_correlation;
CREATE INDEX IF NOT EXISTS idx_recommendations_tenant_correlation ON recommendations (tenant_id, correlation_id);

-- Keyset pagination order: created_at DESC, recommendation_id DESC.
CREATE INDEX IF NOT EXISTS idx_recommendations_tenant_page ON recommendations (tenant_id, created_at DESC, recommendation_id DESC);
CREATE INDEX IF NOT EXISTS idx_recommendations_tenant_status ON recommendations (tenant_id, status);

-- 000001 enabled RLS on recommendations but did not FORCE it, so the table
-- owner was exempt. FORCE makes the policy bind every non-superuser role.
ALTER TABLE recommendations ENABLE ROW LEVEL SECURITY;
ALTER TABLE recommendations FORCE ROW LEVEL SECURITY;

-- 000001's policy had no explicit WITH CHECK; recreate it so inserts and
-- updates are checked against the same tenant predicate, stated explicitly.
DROP POLICY IF EXISTS tenant_isolation_policy ON recommendations;
CREATE POLICY tenant_isolation_policy ON recommendations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── 2. recommendation_dispositions: append-only human decision history ──

CREATE TABLE IF NOT EXISTS recommendation_dispositions (
    disposition_id                UUID PRIMARY KEY,
    tenant_id                     VARCHAR(255) NOT NULL,
    recommendation_id             UUID         NOT NULL REFERENCES recommendations (recommendation_id),
    from_status                   VARCHAR(20),
    to_status                     VARCHAR(20)  NOT NULL,
    decided_by_principal_id       VARCHAR(255) NOT NULL,
    reason                        TEXT,
    downstream_action_ref         VARCHAR(255),
    approval_reference            VARCHAR(255),
    replacement_recommendation_id UUID,
    resulting_version             INTEGER      NOT NULL,
    correlation_id                VARCHAR(255) NOT NULL,
    request_id                    VARCHAR(255),
    decided_at                    TIMESTAMPTZ  NOT NULL
);

ALTER TABLE recommendation_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE recommendation_dispositions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS recommendation_dispositions_tenant_isolation ON recommendation_dispositions;
CREATE POLICY recommendation_dispositions_tenant_isolation ON recommendation_dispositions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_recommendation_dispositions_rec
    ON recommendation_dispositions (tenant_id, recommendation_id, decided_at);

-- Append-only: history rows are never edited or removed.
CREATE OR REPLACE FUNCTION recommendation_dispositions_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'recommendation_dispositions is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_recommendation_dispositions_append_only ON recommendation_dispositions;
CREATE TRIGGER trg_recommendation_dispositions_append_only
    BEFORE UPDATE OR DELETE ON recommendation_dispositions
    FOR EACH ROW EXECUTE FUNCTION recommendation_dispositions_append_only();

-- ── 3. idempotency_keys: Idempotency-Key replay records ──

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       VARCHAR(255) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    request_hash    VARCHAR(64)  NOT NULL,
    resource_id     UUID         NOT NULL,
    response_body   JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── 4. outbox: transactional outbox for Kafka events and governance log records ──

CREATE TABLE IF NOT EXISTS outbox (
    outbox_id     UUID PRIMARY KEY,
    tenant_id     VARCHAR(255) NOT NULL,
    destination   VARCHAR(32)  NOT NULL CHECK (destination IN ('KAFKA', 'GOVERNANCE_DECISION_LOG')),
    message_type  VARCHAR(128) NOT NULL,
    message_key   VARCHAR(255) NOT NULL,
    payload       JSONB        NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    attempts      INTEGER      NOT NULL DEFAULT 0,
    last_error    TEXT,
    delivered_at  TIMESTAMPTZ,
    parked_at     TIMESTAMPTZ
);

-- Tenant requests see only their own rows. The relay has no tenant: it sets
-- app.outbox_relay = 'on' inside its own transaction to drain every tenant.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS outbox_tenant_or_relay ON outbox;
CREATE POLICY outbox_tenant_or_relay ON outbox FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.outbox_relay', true) = 'on')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.outbox_relay', true) = 'on');

CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox (created_at)
    WHERE delivered_at IS NULL AND parked_at IS NULL;

COMMIT;
