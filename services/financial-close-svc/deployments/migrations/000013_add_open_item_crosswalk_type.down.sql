ALTER TABLE migration_crosswalk_entries DROP CONSTRAINT IF EXISTS chk_crosswalk_source_reference_type;
ALTER TABLE migration_crosswalk_entries
    DROP COLUMN IF EXISTS source_reference_type,
    DROP COLUMN IF EXISTS party_id;
