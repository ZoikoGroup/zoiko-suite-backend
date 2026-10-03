-- 000002_ai04_governance_layer.up.sql
-- Adds the ZS-SVC-N-001 AI-04 (Anomaly / Exception Detection) governed
-- advisory workflow alongside this service's existing rule-based
-- anomaly_records/anomaly_detection_rules tables, which are untouched.
-- The two surfaces are independent: anomaly_records remains the
-- Phase 6 rule-engine feature, while the tables below add a governed
-- baseline/model registry, drift gating, and an immutable evidence
-- trail with a forward-only review lifecycle.

-- Anomaly Models: seller-managed baseline/model registration per
-- (domain, model provider, model version). Mutable in place.
-- RunDetection refuses outright, before any signal exists, when an
-- invocation's own observed drift exceeds max_drift_bp.
CREATE TABLE IF NOT EXISTS anomaly_models (
    model_id              TEXT         PRIMARY KEY
        CHECK (model_id ~ '^amd_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id             VARCHAR(64)  NOT NULL,
    domain_name           VARCHAR(128) NOT NULL,
    model_provider        VARCHAR(128) NOT NULL,
    model_version         VARCHAR(128) NOT NULL,
    review_threshold_bp   INT          NOT NULL CHECK (review_threshold_bp BETWEEN 0 AND 10000),
    max_drift_bp          INT          NOT NULL CHECK (max_drift_bp BETWEEN 0 AND 10000),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by            VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, domain_name, model_provider, model_version)
);

-- Detection Runs: one per RunDetection call, immutable provenance.
CREATE TABLE IF NOT EXISTS detection_runs (
    run_id             TEXT         PRIMARY KEY
        CHECK (run_id ~ '^dtr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id          VARCHAR(64)  NOT NULL,
    model_id           TEXT         NOT NULL REFERENCES anomaly_models(model_id),
    domain_name        VARCHAR(128) NOT NULL,
    model_provider     VARCHAR(128) NOT NULL,
    model_version      VARCHAR(128) NOT NULL,
    observed_drift_bp  INT          NOT NULL CHECK (observed_drift_bp BETWEEN 0 AND 10000),
    business_context   TEXT         NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by         VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_detection_runs_model ON detection_runs(tenant_id, model_id);

-- Anomaly Signals: one row per detected signal, governed and evidenced
-- (distinct from the legacy anomaly_records rule-engine table).
-- model_provider/model_version are frozen from the run at creation —
-- "feature/model version is retained with each signal." Forward-only
-- lifecycle: Detected -> ReviewPending -> (Confirmed|Dismissed|
-- Escalated), then fully terminal. A dismissed signal keeps every
-- evidentiary column, never deleted, never altered.
CREATE TABLE IF NOT EXISTS anomaly_signals (
    signal_id          TEXT         PRIMARY KEY
        CHECK (signal_id ~ '^ans_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id          VARCHAR(64)  NOT NULL,
    run_id             TEXT         NOT NULL REFERENCES detection_runs(run_id),
    domain_name        VARCHAR(128) NOT NULL,
    model_provider     VARCHAR(128) NOT NULL,
    model_version      VARCHAR(128) NOT NULL,
    source_entity_ref  VARCHAR(256) NOT NULL,
    severity           VARCHAR(16)  NOT NULL CHECK (severity IN ('LOW','MEDIUM','HIGH','CRITICAL')),
    anomaly_score_bp   INT          NOT NULL CHECK (anomaly_score_bp BETWEEN 0 AND 10000),
    features           JSONB        NOT NULL DEFAULT '{}'::jsonb,
    content_hash       CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    status             VARCHAR(16)  NOT NULL DEFAULT 'Detected'
        CHECK (status IN ('Detected','ReviewPending','Confirmed','Dismissed','Escalated')),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_anomaly_signals_run ON anomaly_signals(tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_anomaly_signals_status ON anomaly_signals(tenant_id, status);

-- Review Dispositions: append-only, exactly one row per signal — the
-- terminal disposition. Pure evidence: no other table is ever written
-- to as a side effect of a disposition, no matter the outcome.
CREATE TABLE IF NOT EXISTS review_dispositions (
    disposition_id   TEXT         PRIMARY KEY
        CHECK (disposition_id ~ '^rvd_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id        VARCHAR(64)  NOT NULL,
    signal_id        TEXT         NOT NULL UNIQUE REFERENCES anomaly_signals(signal_id),
    outcome          VARCHAR(16)  NOT NULL CHECK (outcome IN ('Confirmed','Dismissed','Escalated')),
    notes            TEXT         NOT NULL,
    actor            VARCHAR(128) NOT NULL,
    decided_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Idempotency Keys: did not previously exist in this service. Used by
-- the new AI-04 commands only; the legacy Detect/UpdateStatus/CreateRule
-- commands are unchanged.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       VARCHAR(64)  NOT NULL,
    owner_scope     VARCHAR(32)  NOT NULL,
    principal_id    VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    request_sha256  VARCHAR(64)  NOT NULL,
    resource_id     VARCHAR(128) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE anomaly_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE anomaly_models FORCE ROW LEVEL SECURITY;
CREATE POLICY anomaly_models_tenant_isolation ON anomaly_models
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE detection_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE detection_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY detection_runs_tenant_isolation ON detection_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE anomaly_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE anomaly_signals FORCE ROW LEVEL SECURITY;
CREATE POLICY anomaly_signals_tenant_isolation ON anomaly_signals
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE review_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_dispositions FORCE ROW LEVEL SECURITY;
CREATE POLICY review_dispositions_tenant_isolation ON review_dispositions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Harden the pre-existing tables with the same tenant-isolation
-- guarantee the rest of this platform relies on (ENABLE alone still
-- lets the table owner bypass RLS; FORCE closes that).
ALTER TABLE anomaly_detection_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE anomaly_records FORCE ROW LEVEL SECURITY;

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION reject_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows cannot be deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_anomaly_models_no_delete
    BEFORE DELETE ON anomaly_models
    FOR EACH ROW EXECUTE FUNCTION reject_delete();

CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_detection_runs_immutable
    BEFORE UPDATE OR DELETE ON detection_runs
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_review_dispositions_immutable
    BEFORE UPDATE OR DELETE ON review_dispositions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

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

CREATE TRIGGER trigger_signal_lifecycle
    BEFORE UPDATE OR DELETE ON anomaly_signals
    FOR EACH ROW EXECUTE FUNCTION enforce_signal_lifecycle();
