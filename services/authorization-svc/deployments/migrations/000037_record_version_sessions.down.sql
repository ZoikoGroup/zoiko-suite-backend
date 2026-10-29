-- 000037_record_version_sessions.down.sql
--
-- Revert record_version additions for session aggregates.

ALTER TABLE support_sessions
    DROP CONSTRAINT IF EXISTS ss_version_positive;
ALTER TABLE support_sessions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE break_glass_sessions
    DROP CONSTRAINT IF EXISTS bgs_version_positive;
ALTER TABLE break_glass_sessions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE privileged_sessions
    DROP CONSTRAINT IF EXISTS ps_version_positive;
ALTER TABLE privileged_sessions
    DROP COLUMN IF EXISTS record_version;