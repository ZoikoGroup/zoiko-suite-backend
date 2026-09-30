-- 000004_response_version_and_idempotency.down.sql
-- Revert 000004_response_version_and_idempotency.up.sql

DROP TABLE IF EXISTS rights_idempotency_keys;

ALTER TABLE rights_requests
    DROP COLUMN IF EXISTS response_package_version;