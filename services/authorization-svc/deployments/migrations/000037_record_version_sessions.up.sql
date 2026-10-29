-- 000037_record_version_sessions.up.sql
--
-- Add optimistic-concurrency version (record_version) to PrivilegedSession,
-- BreakGlassSession and SupportSession aggregates. API-3 requires ETag/If-Match
-- on every aggregate mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE privileged_sessions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE break_glass_sessions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE support_sessions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE privileged_sessions
    ADD CONSTRAINT ps_version_positive CHECK (record_version > 0);

ALTER TABLE break_glass_sessions
    ADD CONSTRAINT bgs_version_positive CHECK (record_version > 0);

ALTER TABLE support_sessions
    ADD CONSTRAINT ss_version_positive CHECK (record_version > 0);