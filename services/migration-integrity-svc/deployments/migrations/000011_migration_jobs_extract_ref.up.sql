-- Migration: 000011_migration_jobs_extract_ref.up.sql
--
-- Ties /v1/migrations/validate to a registered, immutable SourceExtract
-- instead of accepting raw records with no frozen record of what was
-- submitted. Additive and nullable: a caller that still posts /validate
-- without source_extract_id (the pre-existing contract) is unaffected --
-- see handler.ValidateMigration's backward-compatibility handling.
ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS source_extract_id UUID REFERENCES migration_source_extracts(id),
    ADD COLUMN IF NOT EXISTS source_extract_version INT;

CREATE INDEX IF NOT EXISTS idx_migration_jobs_source_extract ON migration_jobs(tenant_id, source_extract_id);
