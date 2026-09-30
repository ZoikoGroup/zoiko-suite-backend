-- 000004_decision_dimensions_and_idempotency.down.sql
DROP TABLE IF EXISTS decision_idempotency_keys;

ALTER TABLE privacy_decisions
    DROP COLUMN IF EXISTS input_fingerprint,
    DROP COLUMN IF EXISTS notice_version_id,
    DROP COLUMN IF EXISTS constraints,
    DROP COLUMN IF EXISTS subject_context,
    DROP COLUMN IF EXISTS data_context,
    DROP COLUMN IF EXISTS secondary_purpose_id,
    DROP COLUMN IF EXISTS recipient_context,
    DROP COLUMN IF EXISTS transfer_decision_id;
