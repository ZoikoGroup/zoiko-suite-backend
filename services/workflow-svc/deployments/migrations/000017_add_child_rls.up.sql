-- 000017_add_child_rls.up.sql
-- Close the backstop gap left by 000004: only workflow_instances had RLS.
-- Child tables have no tenant_id of their own, so each policy derives the
-- tenant from its owning instance. When app.tenant_id is unset the
-- current_setting expression is NULL and the table matches no rows.

ALTER TABLE workflow_stages ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_stages FORCE ROW LEVEL SECURITY;
CREATE POLICY workflow_stages_tenant_isolation ON workflow_stages
    FOR ALL
    USING (EXISTS (
        SELECT 1 FROM workflow_instances wi
        WHERE wi.workflow_instance_id = workflow_stages.workflow_instance_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM workflow_instances wi
        WHERE wi.workflow_instance_id = workflow_stages.workflow_instance_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));

ALTER TABLE workflow_stage_quorum_approvers ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_stage_quorum_approvers FORCE ROW LEVEL SECURITY;
CREATE POLICY workflow_stage_quorum_approvers_tenant_isolation ON workflow_stage_quorum_approvers
    FOR ALL
    USING (EXISTS (
        SELECT 1 FROM workflow_stages ws
        JOIN workflow_instances wi ON wi.workflow_instance_id = ws.workflow_instance_id
        WHERE ws.workflow_stage_id = workflow_stage_quorum_approvers.workflow_stage_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM workflow_stages ws
        JOIN workflow_instances wi ON wi.workflow_instance_id = ws.workflow_instance_id
        WHERE ws.workflow_stage_id = workflow_stage_quorum_approvers.workflow_stage_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));

ALTER TABLE workflow_stage_quorum_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_stage_quorum_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY workflow_stage_quorum_decisions_tenant_isolation ON workflow_stage_quorum_decisions
    FOR ALL
    USING (EXISTS (
        SELECT 1 FROM workflow_stages ws
        JOIN workflow_instances wi ON wi.workflow_instance_id = ws.workflow_instance_id
        WHERE ws.workflow_stage_id = workflow_stage_quorum_decisions.workflow_stage_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM workflow_stages ws
        JOIN workflow_instances wi ON wi.workflow_instance_id = ws.workflow_instance_id
        WHERE ws.workflow_stage_id = workflow_stage_quorum_decisions.workflow_stage_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));

ALTER TABLE workflow_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY workflow_transitions_tenant_isolation ON workflow_transitions
    FOR ALL
    USING (EXISTS (
        SELECT 1 FROM workflow_instances wi
        WHERE wi.workflow_instance_id = workflow_transitions.workflow_instance_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM workflow_instances wi
        WHERE wi.workflow_instance_id = workflow_transitions.workflow_instance_id
          AND wi.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    ));

ALTER TABLE workflow_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY workflow_definitions_tenant_isolation ON workflow_definitions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
