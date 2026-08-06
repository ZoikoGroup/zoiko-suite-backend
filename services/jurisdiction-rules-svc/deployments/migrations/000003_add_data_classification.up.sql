-- 000003_add_data_classification.up.sql
-- Adds data_classification to both owned tables per
-- docs/architecture/04-data-model.md §20 and the tier assignment in
-- docs/architecture/data_classification_audit.md §2.11:
--
--   jurisdictions      → PUBLIC   (names, region codes, country codes)
--   jurisdiction_rules → INTERNAL (rule domain settings, legislative metadata)
--
-- VARCHAR with a default, not an enum — same shape as the column added to
-- tenant-entity-registry-svc (000004) and identity-context-svc (000002).
-- The defaults are the correct tier for existing rows, so no backfill is needed.

ALTER TABLE jurisdictions
    ADD COLUMN IF NOT EXISTS data_classification VARCHAR(32) NOT NULL DEFAULT 'PUBLIC';

ALTER TABLE jurisdiction_rules
    ADD COLUMN IF NOT EXISTS data_classification VARCHAR(32) NOT NULL DEFAULT 'INTERNAL';
