-- 000003_contract_hardening.down.sql

DROP TABLE IF EXISTS anomaly_outbox;

DROP TABLE IF EXISTS idempotency_keys;
CREATE TABLE idempotency_keys (
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
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

DROP INDEX IF EXISTS idx_review_dispositions_tenant_decided;
ALTER TABLE review_dispositions DROP COLUMN IF EXISTS evidence_refs, DROP COLUMN IF EXISTS reason_code;

DROP INDEX IF EXISTS idx_anomaly_signals_tenant_created;
ALTER TABLE anomaly_signals
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS feature_set_version;

ALTER TABLE detection_runs
    DROP COLUMN IF EXISTS drift_breached,
    DROP COLUMN IF EXISTS drift_policy,
    DROP COLUMN IF EXISTS max_drift_bp,
    DROP COLUMN IF EXISTS review_threshold_bp,
    DROP COLUMN IF EXISTS feature_set_version,
    DROP COLUMN IF EXISTS model_published_by,
    DROP COLUMN IF EXISTS model_policy_version;

DROP INDEX IF EXISTS idx_anomaly_models_tenant_created;
ALTER TABLE anomaly_models
    DROP CONSTRAINT IF EXISTS anomaly_models_drift_policy_check,
    DROP CONSTRAINT IF EXISTS anomaly_models_status_check,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS drift_policy,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS version;

DROP INDEX IF EXISTS idx_anomaly_rules_tenant_created;
DROP INDEX IF EXISTS idx_anomaly_records_tenant_detected;
ALTER TABLE anomaly_records
    DROP CONSTRAINT IF EXISTS anomaly_records_severity_check,
    DROP CONSTRAINT IF EXISTS anomaly_records_status_check,
    DROP COLUMN IF EXISTS detected_by,
    DROP COLUMN IF EXISTS version;
ALTER TABLE anomaly_detection_rules
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS version;
