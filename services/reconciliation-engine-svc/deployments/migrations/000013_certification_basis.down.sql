-- Reverting drops the basis, lineage reference and freshness columns; values in
-- them are lost. The sealed manifest (and its sha256) is untouched.
DROP INDEX IF EXISTS uq_certifications_basis;
ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_max_age_positive;
ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_basis_is_complete;
ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_lineage_ref_format;
ALTER TABLE certifications DROP CONSTRAINT IF EXISTS certifications_basis_format;
ALTER TABLE certifications
    DROP COLUMN IF EXISTS evidence_max_age_seconds, DROP COLUMN IF EXISTS evidence_oldest_at,
    DROP COLUMN IF EXISTS lineage_manifest_sha256, DROP COLUMN IF EXISTS lineage_manifest_id,
    DROP COLUMN IF EXISTS basis_sha256;
