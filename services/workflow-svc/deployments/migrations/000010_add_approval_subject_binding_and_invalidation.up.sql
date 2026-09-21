-- 000010_add_approval_subject_binding_and_invalidation.up.sql
-- Approval Subject Binding & Invalidation per ZS-STATE-001 §6.1 and Invariant I-06/I-07.
--
-- Adds canonical subject identity, version, and fingerprint binding to workflow_instances,
-- and support for the INVALIDATED state on material object changes.

ALTER TABLE workflow_instances
    ADD COLUMN subject_type                 VARCHAR(64),
    ADD COLUMN subject_id                   TEXT,
    ADD COLUMN subject_version              INT,
    ADD COLUMN subject_fingerprint          VARCHAR(71),
    ADD COLUMN invalidated_at               TIMESTAMPTZ,
    ADD COLUMN invalidation_reason_code     VARCHAR(64),
    ADD COLUMN invalidation_narrative       TEXT,
    ADD COLUMN invalidation_evidence_refs   TEXT[];

-- Subject fingerprint format constraint: must be 'sha256:<64 hex digits>' (71 chars total) when present.
ALTER TABLE workflow_instances
    ADD CONSTRAINT workflow_subject_fingerprint_format
    CHECK (subject_fingerprint IS NULL OR (subject_fingerprint LIKE 'sha256:%' AND length(subject_fingerprint) = 71));

-- Subject version must be non-negative when present.
ALTER TABLE workflow_instances
    ADD CONSTRAINT workflow_subject_version_positive
    CHECK (subject_version IS NULL OR subject_version >= 0);

-- Partial index for looking up active or past workflows for a specific business object.
CREATE INDEX idx_workflow_instances_subject
    ON workflow_instances (tenant_id, subject_type, subject_id)
    WHERE subject_type IS NOT NULL AND subject_id IS NOT NULL;
