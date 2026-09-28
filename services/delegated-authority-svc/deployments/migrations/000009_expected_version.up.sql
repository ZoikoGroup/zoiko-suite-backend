-- Expected_version for protected changes: add version column for optimistic locking
-- Per the audit gap: "No expected_version for protected changes" - clients should
-- provide expected_version on revoke/extend to prevent lost updates.

ALTER TABLE delegation_grants
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

-- Add index for version queries
CREATE INDEX IF NOT EXISTS idx_delegation_grants_version
    ON delegation_grants (delegation_id, version);