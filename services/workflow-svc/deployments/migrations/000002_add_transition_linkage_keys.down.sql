ALTER TABLE workflow_transitions
    DROP COLUMN IF EXISTS correlation_id,
    DROP COLUMN IF EXISTS causation_id;
