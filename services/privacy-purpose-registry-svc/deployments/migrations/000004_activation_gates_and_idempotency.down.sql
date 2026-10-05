-- 000004_activation_gates_and_idempotency.down.sql

DROP TABLE IF EXISTS purpose_registry_idempotency_keys;

ALTER TABLE processing_activity_versions
    DROP COLUMN IF EXISTS dpia_tia_status,
    DROP COLUMN IF EXISTS notice_consent_dependency;
