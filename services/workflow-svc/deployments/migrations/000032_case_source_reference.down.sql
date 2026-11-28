-- Reverts 000032: drops the source reference of cases (the cases stay).

DROP INDEX IF EXISTS cases_source_milestone_unique;
ALTER TABLE cases DROP CONSTRAINT IF EXISTS cases_source_complete;
ALTER TABLE cases
    DROP COLUMN IF EXISTS milestone,
    DROP COLUMN IF EXISTS source_id,
    DROP COLUMN IF EXISTS source_type;
