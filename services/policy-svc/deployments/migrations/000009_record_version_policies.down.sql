-- 000009_record_version_policies.down.sql
--
-- Revert record_version additions for Policy.

ALTER TABLE policies
    DROP CONSTRAINT IF EXISTS policy_version_positive;
ALTER TABLE policies
    DROP COLUMN IF EXISTS record_version;