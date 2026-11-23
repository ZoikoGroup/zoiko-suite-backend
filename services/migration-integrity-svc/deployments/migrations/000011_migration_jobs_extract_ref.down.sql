DROP INDEX IF EXISTS idx_migration_jobs_source_extract;
ALTER TABLE migration_jobs
    DROP COLUMN IF EXISTS source_extract_version,
    DROP COLUMN IF EXISTS source_extract_id;
