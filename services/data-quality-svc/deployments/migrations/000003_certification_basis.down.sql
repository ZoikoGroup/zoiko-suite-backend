-- Reverting drops the basis, lineage reference and freshness columns; any values
-- recorded in them are lost. The certification summaries (sealed JSON) keep
-- their own copy, so the sealed evidence itself is not lost.
DROP INDEX IF EXISTS uq_dq_certifications_basis;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_basis_is_complete;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_lineage_ref_format;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_basis_format;
ALTER TABLE dq_certifications
    DROP COLUMN IF EXISTS evidence_max_age_seconds, DROP COLUMN IF EXISTS evidence_as_of,
    DROP COLUMN IF EXISTS lineage_manifest_sha256, DROP COLUMN IF EXISTS lineage_manifest_id,
    DROP COLUMN IF EXISTS basis_sha256;
ALTER TABLE dq_runs DROP COLUMN IF EXISTS evidence_as_of;
