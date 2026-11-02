-- 000011_job_version_requester_decisions.up.sql
--
-- 1. requested_by: the principal who ran the analysis. The Global Authorization
--    / SoD standard denies requestor self-approval ("requestor_id == subject_id
--    AND approval required -> DENY"), so APPROVED / EXECUTED decisions by the
--    requester are refused. Jobs analysed before this migration have '' and
--    cannot be checked (documented limitation).
-- 2. version: the job aggregate version for ETag / If-Match (Global API
--    Contract Standard §10). Every decision and the archive increment it.
-- 3. archived_at / archived_by: who archived the job and when (soft delete).
-- 4. decided_by / decided_at on each suggestion (ZS-SVC-Z-001 INV-18: manual
--    overrides require actor and reason).
ALTER TABLE reconciliation_jobs ADD COLUMN IF NOT EXISTS requested_by VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE reconciliation_jobs ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
ALTER TABLE reconciliation_jobs ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
ALTER TABLE reconciliation_jobs ADD COLUMN IF NOT EXISTS archived_by VARCHAR(128);
ALTER TABLE reconciliation_jobs DROP CONSTRAINT IF EXISTS reconciliation_jobs_version_positive;
ALTER TABLE reconciliation_jobs ADD CONSTRAINT reconciliation_jobs_version_positive CHECK (version >= 1);
ALTER TABLE reconciliation_jobs DROP CONSTRAINT IF EXISTS reconciliation_jobs_archive_consistent;
ALTER TABLE reconciliation_jobs ADD CONSTRAINT reconciliation_jobs_archive_consistent
    CHECK (archived_at IS NULL OR status = 'ARCHIVED');  -- jobs archived before 000011 keep NULL

ALTER TABLE reconciliation_unmatched_items ADD COLUMN IF NOT EXISTS decided_by VARCHAR(128);
ALTER TABLE reconciliation_unmatched_items ADD COLUMN IF NOT EXISTS decided_at TIMESTAMPTZ;
