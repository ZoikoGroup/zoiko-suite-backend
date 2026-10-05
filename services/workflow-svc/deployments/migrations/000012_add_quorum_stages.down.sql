DROP TABLE IF EXISTS workflow_stage_quorum_decisions;
DROP TABLE IF EXISTS workflow_stage_quorum_approvers;

ALTER TABLE workflow_stages DROP CONSTRAINT IF EXISTS workflow_stages_shape_matches_type;
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS required_approvals;
ALTER TABLE workflow_stages ALTER COLUMN approver_principal_id SET NOT NULL;
ALTER TABLE workflow_stages DROP COLUMN IF EXISTS stage_type;
