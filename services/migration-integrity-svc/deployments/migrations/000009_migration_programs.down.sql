DROP INDEX IF EXISTS idx_migration_batch_refs_program;
ALTER TABLE migration_batch_refs DROP COLUMN IF EXISTS program_id;

DROP INDEX IF EXISTS idx_migration_programs_tenant;
DROP TABLE IF EXISTS migration_programs;
