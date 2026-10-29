DROP INDEX IF EXISTS uq_consolidation_adjustments_source;
ALTER TABLE consolidation_adjustments DROP COLUMN IF EXISTS source_ref;
