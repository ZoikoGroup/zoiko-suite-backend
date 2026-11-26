-- Reverts 000031: drops the coverage evidence columns (COVERED rows keep
-- their status but lose the workpaper they named).

ALTER TABLE risk_assessments DROP CONSTRAINT IF EXISTS risk_covered_has_evidence;
ALTER TABLE risk_assessments
    DROP COLUMN IF EXISTS covered_at,
    DROP COLUMN IF EXISTS covered_by_principal_id,
    DROP COLUMN IF EXISTS covered_by_workpaper_id;
