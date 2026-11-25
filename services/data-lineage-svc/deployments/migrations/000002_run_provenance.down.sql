-- Reverts 000002. Drops the run/config provenance and version pins; any values
-- recorded in these columns are lost, which is the only way back to the
-- 000001 shape.
DROP INDEX IF EXISTS idx_lineage_activities_run;
DROP TRIGGER IF EXISTS trigger_lineage_activity_immutability ON lineage_activities;
DROP FUNCTION IF EXISTS enforce_lineage_activity_immutability();
ALTER TABLE lineage_edges DROP CONSTRAINT IF EXISTS lineage_edges_version_pins_not_mutable;
ALTER TABLE lineage_edges DROP COLUMN IF EXISTS derived_version, DROP COLUMN IF EXISTS source_version;
ALTER TABLE lineage_activities DROP CONSTRAINT IF EXISTS lineage_activities_config_digest_format;
ALTER TABLE lineage_activities
    DROP COLUMN IF EXISTS region, DROP COLUMN IF EXISTS environment, DROP COLUMN IF EXISTS config_digest,
    DROP COLUMN IF EXISTS build_version, DROP COLUMN IF EXISTS run_id;
