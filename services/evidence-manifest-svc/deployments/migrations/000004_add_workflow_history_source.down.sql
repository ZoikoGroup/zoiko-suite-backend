ALTER TABLE manifest_records DROP CONSTRAINT IF EXISTS manifest_records_source_type_check;
ALTER TABLE manifest_records
    ADD CONSTRAINT manifest_records_source_type_check
    CHECK (source_type IN ('GOVERNANCE_DECISION', 'ACCESS_DECISION', 'WORKFLOW_INSTANCE'));
