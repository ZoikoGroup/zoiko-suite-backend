-- 000007_record_version.down.sql
--
-- Revert record_version additions for mutable aggregates.

ALTER TABLE shared_secret_exceptions
    DROP CONSTRAINT IF EXISTS sse_version_positive;
ALTER TABLE shared_secret_exceptions
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE secret_leases
    DROP CONSTRAINT IF EXISTS sl_version_positive;
ALTER TABLE secret_leases
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE secret_policy_versions
    DROP CONSTRAINT IF EXISTS spv_version_positive;
ALTER TABLE secret_policy_versions
    DROP COLUMN IF EXISTS record_version;