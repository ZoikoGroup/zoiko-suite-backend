DROP TRIGGER IF EXISTS transfer_decisions_append_only ON transfer_decisions;
DROP TRIGGER IF EXISTS transfer_assessments_append_only ON transfer_assessments;
DROP TRIGGER IF EXISTS transfer_mechanisms_append_only ON transfer_mechanisms;
DROP TRIGGER IF EXISTS subprocessors_append_only ON subprocessors;
DROP FUNCTION IF EXISTS reject_evidence_mutation();

DROP TRIGGER IF EXISTS processor_relationships_content_immutable ON processor_relationships;
DROP FUNCTION IF EXISTS reject_relationship_content_mutation();
