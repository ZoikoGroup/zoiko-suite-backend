-- 000007_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in secret-vault-integration-svc. API-3 requires ETag/If-Match on every
-- aggregate mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE secret_policy_versions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE secret_leases
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE shared_secret_exceptions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE secret_policy_versions
    ADD CONSTRAINT spv_version_positive CHECK (record_version > 0);

ALTER TABLE secret_leases
    ADD CONSTRAINT sl_version_positive CHECK (record_version > 0);

ALTER TABLE shared_secret_exceptions
    ADD CONSTRAINT sse_version_positive CHECK (record_version > 0);