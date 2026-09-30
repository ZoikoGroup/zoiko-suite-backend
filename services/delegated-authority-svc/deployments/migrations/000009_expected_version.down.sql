-- Down migration for 000009_expected_version.up.sql

ALTER TABLE delegation_grants
    DROP COLUMN IF EXISTS version;

DROP INDEX IF EXISTS idx_delegation_grants_version;