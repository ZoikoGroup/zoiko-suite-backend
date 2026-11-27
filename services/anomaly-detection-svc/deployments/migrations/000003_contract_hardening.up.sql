-- 000003_contract_hardening.up.sql
--
-- Brings both surfaces of anomaly-detection-svc up to the platform API
-- contract (Global API Command & Integration Contract §9 idempotency,
-- §10 ETag/If-Match, transactional outbox) and closes AI-04 spec gaps:
--
--   1. version columns for optimistic concurrency (ETag / If-Match)
--   2. rule-engine records: status/severity CHECKs, detected_by (SoD),
--      widened anomaly_score so a large z-score cannot overflow
--   3. anomaly_models: status, drift_policy, updated_at/updated_by
--   4. detection_runs: frozen thresholds/policy version/publisher, feature
--      set version, drift_breached flag
--   5. anomaly_signals: feature_set_version, version, updated_at
--   6. review_dispositions: reason_code and evidence_refs
--   7. idempotency_keys rebuilt on the §9.1 storage key
--      (tenant + principal + route template + key)
--   8. anomaly_outbox: transactional outbox for AI.AnomalyDetected /
--      AI.AnomalyDispositioned

-- ── 1/2. Rule-engine tables ─────────────────────────────────────────────────

ALTER TABLE anomaly_detection_rules
    ADD COLUMN IF NOT EXISTS version    INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS created_by VARCHAR(128) NOT NULL DEFAULT '';

ALTER TABLE anomaly_records
    ADD COLUMN IF NOT EXISTS version     INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS detected_by VARCHAR(128) NOT NULL DEFAULT '';

ALTER TABLE anomaly_records ALTER COLUMN anomaly_score TYPE NUMERIC(14,2);

ALTER TABLE anomaly_records
    ADD CONSTRAINT anomaly_records_status_check
        CHECK (status IN ('OPEN','UNDER_INVESTIGATION','CONFIRMED_ANOMALY','FALSE_POSITIVE','RESOLVED')),
    ADD CONSTRAINT anomaly_records_severity_check
        CHECK (severity IN ('LOW','MEDIUM','HIGH','CRITICAL'));

CREATE INDEX IF NOT EXISTS idx_anomaly_records_tenant_detected
    ON anomaly_records (tenant_id, detected_at DESC, anomaly_id DESC);
CREATE INDEX IF NOT EXISTS idx_anomaly_rules_tenant_created
    ON anomaly_detection_rules (tenant_id, created_at DESC, rule_id DESC);

-- Explicit WITH CHECK so an INSERT/UPDATE can never write a row for another
-- tenant (USING alone already implies this for FOR ALL, but the platform
-- convention is to spell it out).
DROP POLICY IF EXISTS tenant_isolation_policy ON anomaly_detection_rules;
CREATE POLICY tenant_isolation_policy ON anomaly_detection_rules
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS tenant_isolation_policy ON anomaly_records;
CREATE POLICY tenant_isolation_policy ON anomaly_records
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── 3. Anomaly models ───────────────────────────────────────────────────────

ALTER TABLE anomaly_models
    ADD COLUMN IF NOT EXISTS version      INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS status       VARCHAR(16)  NOT NULL DEFAULT 'Active',
    ADD COLUMN IF NOT EXISTS drift_policy VARCHAR(16)  NOT NULL DEFAULT 'BLOCK',
    ADD COLUMN IF NOT EXISTS updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS updated_by   VARCHAR(128) NOT NULL DEFAULT '';

UPDATE anomaly_models SET updated_by = created_by WHERE updated_by = '';

ALTER TABLE anomaly_models
    ADD CONSTRAINT anomaly_models_status_check CHECK (status IN ('Active','Disabled')),
    ADD CONSTRAINT anomaly_models_drift_policy_check CHECK (drift_policy IN ('BLOCK','ESCALATE','DISABLE'));

CREATE INDEX IF NOT EXISTS idx_anomaly_models_tenant_created
    ON anomaly_models (tenant_id, created_at DESC, model_id DESC);

-- ── 4. Detection runs ───────────────────────────────────────────────────────

ALTER TABLE detection_runs
    ADD COLUMN IF NOT EXISTS model_policy_version INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS model_published_by   VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS feature_set_version  VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS review_threshold_bp  INT          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_drift_bp         INT          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_policy         VARCHAR(16)  NOT NULL DEFAULT 'BLOCK',
    ADD COLUMN IF NOT EXISTS drift_breached       BOOLEAN      NOT NULL DEFAULT FALSE;

-- ── 5. Anomaly signals ──────────────────────────────────────────────────────

ALTER TABLE anomaly_signals
    ADD COLUMN IF NOT EXISTS feature_set_version VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS version             INT          NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_anomaly_signals_tenant_created
    ON anomaly_signals (tenant_id, created_at DESC, signal_id DESC);

-- The lifecycle trigger from 000002 also has to keep feature_set_version
-- immutable ("feature/model version is retained with each signal").
CREATE OR REPLACE FUNCTION enforce_signal_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'anomaly signals cannot be deleted';
    END IF;
    IF OLD.status IN ('Confirmed', 'Dismissed', 'Escalated') THEN
        RAISE EXCEPTION 'anomaly signal % is % and immutable', OLD.signal_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Detected' THEN
                IF NEW.status <> 'ReviewPending' THEN
                    RAISE EXCEPTION 'invalid anomaly signal transition from Detected to %', NEW.status;
                END IF;
            WHEN 'ReviewPending' THEN
                IF NEW.status NOT IN ('Confirmed', 'Dismissed', 'Escalated') THEN
                    RAISE EXCEPTION 'invalid anomaly signal transition from ReviewPending to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown anomaly signal status %', OLD.status;
        END CASE;
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.domain_name IS DISTINCT FROM OLD.domain_name
        OR NEW.model_provider IS DISTINCT FROM OLD.model_provider
        OR NEW.model_version IS DISTINCT FROM OLD.model_version
        OR NEW.feature_set_version IS DISTINCT FROM OLD.feature_set_version
        OR NEW.source_entity_ref IS DISTINCT FROM OLD.source_entity_ref
        OR NEW.severity IS DISTINCT FROM OLD.severity
        OR NEW.anomaly_score_bp IS DISTINCT FROM OLD.anomaly_score_bp
        OR NEW.features IS DISTINCT FROM OLD.features
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'anomaly signal % may only have its status change', OLD.signal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── 6. Review dispositions ──────────────────────────────────────────────────

ALTER TABLE review_dispositions
    ADD COLUMN IF NOT EXISTS reason_code   VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS evidence_refs JSONB       NOT NULL DEFAULT '[]'::jsonb;

CREATE INDEX IF NOT EXISTS idx_review_dispositions_tenant_decided
    ON review_dispositions (tenant_id, decided_at DESC, disposition_id DESC);

-- ── 7. Idempotency keys (§9.1 storage key) ──────────────────────────────────

DROP TABLE IF EXISTS idempotency_keys;
CREATE TABLE idempotency_keys (
    tenant_id       VARCHAR(64)  NOT NULL,
    principal_id    VARCHAR(128) NOT NULL,
    route_template  VARCHAR(160) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    request_sha256  CHAR(64)     NOT NULL,
    resource_type   VARCHAR(64)  NOT NULL,
    resource_id     VARCHAR(128) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, principal_id, route_template, idempotency_key)
);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── 8. Transactional outbox ─────────────────────────────────────────────────
--
-- Written in the same transaction as the state change it describes; the
-- relay (internal/outbox) publishes rows to Kafka and stamps published_at.
-- No RLS: the relay reads across tenants, and the table holds only events
-- that are published to the bus anyway.

CREATE TABLE IF NOT EXISTS anomaly_outbox (
    outbox_id       BIGSERIAL    PRIMARY KEY,
    event_id        TEXT         NOT NULL UNIQUE,
    event_type      TEXT         NOT NULL,
    aggregate_type  TEXT         NOT NULL,
    aggregate_id    TEXT         NOT NULL,
    tenant_id       TEXT         NOT NULL,
    legal_entity_id TEXT         NOT NULL DEFAULT '',
    actor_id        TEXT         NOT NULL DEFAULT '',
    correlation_id  TEXT         NOT NULL DEFAULT '',
    payload         JSONB        NOT NULL,
    occurred_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    available_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    attempts        INT          NOT NULL DEFAULT 0,
    last_error      TEXT,
    published_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_anomaly_outbox_ready
    ON anomaly_outbox (available_at, outbox_id)
    WHERE published_at IS NULL;
