-- 000005_record_version.down.sql
--
-- Revert record_version additions for mutable aggregates.

ALTER TABLE restriction_tombstones
    DROP CONSTRAINT IF EXISTS rt_version_positive;
ALTER TABLE restriction_tombstones
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE index_generations
    DROP CONSTRAINT IF EXISTS ig_version_positive;
ALTER TABLE index_generations
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE index_contracts
    DROP CONSTRAINT IF EXISTS ic_version_positive;
ALTER TABLE index_contracts
    DROP COLUMN IF EXISTS record_version;