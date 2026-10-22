DROP POLICY IF EXISTS workflow_definitions_tenant_isolation ON workflow_definitions;
ALTER TABLE workflow_definitions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workflow_transitions_tenant_isolation ON workflow_transitions;
ALTER TABLE workflow_transitions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workflow_stage_quorum_decisions_tenant_isolation ON workflow_stage_quorum_decisions;
ALTER TABLE workflow_stage_quorum_decisions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workflow_stage_quorum_approvers_tenant_isolation ON workflow_stage_quorum_approvers;
ALTER TABLE workflow_stage_quorum_approvers DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS workflow_stages_tenant_isolation ON workflow_stages;
ALTER TABLE workflow_stages DISABLE ROW LEVEL SECURITY;
