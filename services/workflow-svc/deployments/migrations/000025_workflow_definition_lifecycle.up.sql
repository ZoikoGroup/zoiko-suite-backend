-- 000025_workflow_definition_lifecycle.up.sql
-- WFC-01: Workflow Definition Publication Lifecycle (WF-S22, R-001 §3.2–3.4, R-INV-01/02/24, NP-21/22/36)
-- Adds definition status states, validation gates, maker-checker, checksum, effective window

CREATE TYPE definition_status AS ENUM (
    'DRAFT',
    'VALIDATION_PENDING',
    'VALIDATED',
    'APPROVAL_PENDING',
    'PUBLISHED',
    'REJECTED',
    'SUSPENDED',
    'SUPERSEDED',
    'RETIRED'
);

-- Add publication lifecycle columns to workflow_definitions
ALTER TABLE workflow_definitions
    ADD COLUMN definition_status definition_status NOT NULL DEFAULT 'DRAFT',
    ADD COLUMN checksum              VARCHAR(64),                     -- sha256 of canonical stages+settings
    ADD COLUMN effective_from        TIMESTAMPTZ,
    ADD COLUMN effective_to          TIMESTAMPTZ,
    ADD COLUMN validated_by          TEXT,
    ADD COLUMN validated_at          TIMESTAMPTZ,
    ADD COLUMN approved_by           TEXT,
    ADD COLUMN approved_at           TIMESTAMPTZ,
    ADD COLUMN published_by          TEXT,
    ADD COLUMN published_at          TIMESTAMPTZ,
    ADD COLUMN emergency_allowed     BOOLEAN NOT NULL DEFAULT false;   -- WF-S25

-- Map existing rows from is_active to definition_status
UPDATE workflow_definitions
SET definition_status = CASE WHEN is_active THEN 'PUBLISHED' ELSE 'RETIRED' END;

-- Indexes for common queries
CREATE INDEX idx_workflow_definitions_status ON workflow_definitions (tenant_id, definition_status);
CREATE INDEX idx_workflow_definitions_effective ON workflow_definitions (effective_from, effective_to);

-- Trigger to prevent any UPDATE on a PUBLISHED definition except status and effective_to
CREATE OR REPLACE FUNCTION enforce_published_immutable()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.definition_status = 'PUBLISHED' THEN
        -- Allow status change and effective_to change only
        IF NEW.definition_status <> OLD.definition_status THEN
            IF NEW.definition_status NOT IN ('SUSPENDED', 'SUPERSEDED', 'RETIRED') THEN
                RAISE EXCEPTION 'PUBLISHED definition can only transition to SUSPENDED, SUPERSEDED, or RETIRED';
            END IF;
        END IF;
        IF NEW.effective_to IS NOT DISTINCT FROM OLD.effective_to THEN
            -- OK
        ELSIF NEW.effective_to < OLD.effective_from THEN
            RAISE EXCEPTION 'effective_to must be after effective_from';
        ELSE
            -- OK
        END IF;
        
        -- Check that no other columns changed
        IF NEW.workflow_type <> OLD.workflow_type
            OR NEW.version <> OLD.version
            OR NEW.stages_json <> OLD.stages_json
            OR NEW.name IS NOT DISTINCT FROM OLD.name THEN -- Actually allow name/description?
            OR NEW.description <> OLD.description
            OR NEW.checksum <> OLD.checksum
            OR NEW.effective_from <> OLD.effective_from
            OR NEW.expiry_hours IS NOT DISTINCT FROM OLD.expiry_hours
            OR NEW.escalation_target IS NOT DISTINCT FROM OLD.escalation_target
            OR NEW.emergency_allowed <> OLD.emergency_allowed
        THEN
            RAISE EXCEPTION 'PUBLISHED definition is immutable; only status and effective_to may change';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workflow_definitions_published_immutable
    BEFORE UPDATE ON workflow_definitions
    FOR EACH ROW EXECUTE FUNCTION enforce_published_immutable();

-- Update the unique index to use definition_status instead of is_active
DROP INDEX IF EXISTS idx_workflow_definitions_active_version;
CREATE UNIQUE INDEX idx_workflow_definitions_active_version
    ON workflow_definitions (tenant_id, workflow_type, version)
    WHERE definition_status IN ('PUBLISHED', 'SUSPENDED');

COMMENT ON COLUMN workflow_definitions.definition_status IS 'Definition lifecycle: DRAFT → VALIDATION_PENDING → VALIDATED → APPROVAL_PENDING → PUBLISHED / REJECTED / SUSPENDED / SUPERSEDED / RETIRED';
COMMENT ON COLUMN workflow_definitions.checksum IS 'SHA256 of canonical stages + settings for integrity verification';
COMMENT ON COLUMN workflow_definitions.effective_from IS 'When this definition becomes available for new instances';
COMMENT ON COLUMN workflow_definitions.effective_to IS 'When this definition is no longer available (after supersede/retire)';
COMMENT ON COLUMN workflow_definitions.validated_by IS 'Principal who validated the definition';
COMMENT ON COLUMN workflow_definitions.approved_by IS 'Principal who approved the definition (maker ≠ checker)';
COMMENT ON COLUMN workflow_definitions.published_by IS 'Principal who published the definition';
COMMENT ON COLUMN workflow_definitions.emergency_allowed IS 'Whether emergency approval may bypass timing/quorum for this definition (WF-S25)';