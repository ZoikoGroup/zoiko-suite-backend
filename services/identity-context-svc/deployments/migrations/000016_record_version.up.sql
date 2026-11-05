-- 000016_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to Principal,
-- SessionContext and SupportContext aggregates. API-3 requires ETag/If-Match
-- on every aggregate mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE principals
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE session_contexts
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE support_contexts
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE principals
    ADD CONSTRAINT principal_version_positive CHECK (record_version > 0);

ALTER TABLE session_contexts
    ADD CONSTRAINT session_context_version_positive CHECK (record_version > 0);

ALTER TABLE support_contexts
    ADD CONSTRAINT support_context_version_positive CHECK (record_version > 0);