-- 000016_record_version.down.sql
--
-- Revert record_version additions for Principal, SessionContext and SupportContext.

ALTER TABLE support_contexts
    DROP CONSTRAINT IF EXISTS support_context_version_positive;
ALTER TABLE support_contexts
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE session_contexts
    DROP CONSTRAINT IF EXISTS session_context_version_positive;
ALTER TABLE session_contexts
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE principals
    DROP CONSTRAINT IF EXISTS principal_version_positive;
ALTER TABLE principals
    DROP COLUMN IF EXISTS record_version;