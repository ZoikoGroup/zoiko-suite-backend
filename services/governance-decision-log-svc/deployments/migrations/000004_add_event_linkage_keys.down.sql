DROP INDEX IF EXISTS idx_governance_decisions_workflow_instance;
ALTER TABLE governance_decisions
    DROP COLUMN IF EXISTS workflow_instance_id,
    DROP COLUMN IF EXISTS causation_id;
