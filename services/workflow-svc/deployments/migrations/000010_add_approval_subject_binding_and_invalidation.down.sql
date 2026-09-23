-- 000010_add_approval_subject_binding_and_invalidation.down.sql

DROP INDEX IF EXISTS idx_workflow_instances_subject;

ALTER TABLE workflow_instances
    DROP CONSTRAINT IF EXISTS workflow_subject_fingerprint_format,
    DROP CONSTRAINT IF EXISTS workflow_subject_version_positive,
    DROP COLUMN IF EXISTS subject_type,
    DROP COLUMN IF EXISTS subject_id,
    DROP COLUMN IF EXISTS subject_version,
    DROP COLUMN IF EXISTS subject_fingerprint,
    DROP COLUMN IF EXISTS invalidated_at,
    DROP COLUMN IF EXISTS invalidation_reason_code,
    DROP COLUMN IF EXISTS invalidation_narrative,
    DROP COLUMN IF EXISTS invalidation_evidence_refs;
