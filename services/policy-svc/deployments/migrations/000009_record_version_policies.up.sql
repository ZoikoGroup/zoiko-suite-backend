-- 000009_record_version_policies.up.sql
--
-- Add optimistic-concurrency version (record_version) to Policy aggregate.
-- API-3 requires ETag/If-Match on every aggregate mutation; GET must return ETag,
-- and mutations require If-Match.

ALTER TABLE policies
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE policies
    ADD CONSTRAINT policy_version_positive CHECK (record_version > 0);