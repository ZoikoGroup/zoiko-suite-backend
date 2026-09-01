-- Adds WORKFLOW_HISTORY as a valid source_type: workflow-history-svc's own
-- "v1 Known Gap" comment named this service directly as never having been
-- wired to its real transition-history endpoint. Every WorkflowInstanceID
-- requested now also pulls that instance's full transition history, one
-- ManifestRecord per real transition event.
ALTER TABLE manifest_records DROP CONSTRAINT IF EXISTS manifest_records_source_type_check;
ALTER TABLE manifest_records
    ADD CONSTRAINT manifest_records_source_type_check
    CHECK (source_type IN ('GOVERNANCE_DECISION', 'ACCESS_DECISION', 'WORKFLOW_INSTANCE', 'WORKFLOW_HISTORY'));
