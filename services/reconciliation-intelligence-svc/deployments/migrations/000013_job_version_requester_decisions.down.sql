ALTER TABLE reconciliation_unmatched_items DROP COLUMN IF EXISTS decided_at;
ALTER TABLE reconciliation_unmatched_items DROP COLUMN IF EXISTS decided_by;
ALTER TABLE reconciliation_jobs DROP CONSTRAINT IF EXISTS reconciliation_jobs_archive_consistent;
ALTER TABLE reconciliation_jobs DROP CONSTRAINT IF EXISTS reconciliation_jobs_version_positive;
ALTER TABLE reconciliation_jobs DROP COLUMN IF EXISTS archived_by;
ALTER TABLE reconciliation_jobs DROP COLUMN IF EXISTS archived_at;
ALTER TABLE reconciliation_jobs DROP COLUMN IF EXISTS version;
ALTER TABLE reconciliation_jobs DROP COLUMN IF EXISTS requested_by;
