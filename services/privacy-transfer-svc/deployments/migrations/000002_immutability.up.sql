-- 000002_immutability.up.sql
-- Enforces immutability at the database layer — GRANT/REVOKE cannot bind
-- the superuser this runtime connects as, so only a trigger actually
-- stops it (same reasoning as every other immutability migration in this
-- platform).
--
-- processor_relationships is the one legitimately mutable row here:
-- status may toggle ACTIVE/INACTIVE as a relationship starts or ends,
-- but every other field is a recorded legal fact that must not silently
-- change underneath a relationship's own identity.
CREATE OR REPLACE FUNCTION reject_relationship_content_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.controller_ref IS DISTINCT FROM OLD.controller_ref
       OR NEW.processor_ref IS DISTINCT FROM OLD.processor_ref
       OR NEW.service IS DISTINCT FROM OLD.service
       OR NEW.processing_instructions IS DISTINCT FROM OLD.processing_instructions
       OR NEW.purpose_activity_refs IS DISTINCT FROM OLD.purpose_activity_refs
       OR NEW.data_categories IS DISTINCT FROM OLD.data_categories
       OR NEW.subject_classes IS DISTINCT FROM OLD.subject_classes
       OR NEW.contract_evidence_ref IS DISTINCT FROM OLD.contract_evidence_ref
       OR NEW.jurisdictions IS DISTINCT FROM OLD.jurisdictions
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'processor_relationships content is immutable — only status may change (row %)', OLD.relationship_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER processor_relationships_content_immutable
    BEFORE UPDATE ON processor_relationships
    FOR EACH ROW EXECUTE FUNCTION reject_relationship_content_mutation();

-- subprocessors, transfer_mechanisms, transfer_assessments and
-- transfer_decisions are all pure append-only records — a correction
-- creates a new row, never edits an old one.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER subprocessors_append_only
    BEFORE UPDATE OR DELETE ON subprocessors
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER transfer_mechanisms_append_only
    BEFORE UPDATE OR DELETE ON transfer_mechanisms
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER transfer_assessments_append_only
    BEFORE UPDATE OR DELETE ON transfer_assessments
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER transfer_decisions_append_only
    BEFORE UPDATE OR DELETE ON transfer_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
