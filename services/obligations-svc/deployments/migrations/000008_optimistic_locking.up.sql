-- 000008_optimistic_locking.up.sql
-- Obligations Service — optimistic locking for obligation updates
--
-- Adds version column for expected_version/If-Match concurrency control
-- per platform standard §2.1 Canonical Service Input Contract.

BEGIN;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1;

COMMIT;