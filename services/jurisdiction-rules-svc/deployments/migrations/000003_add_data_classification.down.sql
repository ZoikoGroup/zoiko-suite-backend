-- 000003_add_data_classification.down.sql
-- Reverts the data_classification columns.

ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS data_classification;
ALTER TABLE jurisdictions      DROP COLUMN IF EXISTS data_classification;
