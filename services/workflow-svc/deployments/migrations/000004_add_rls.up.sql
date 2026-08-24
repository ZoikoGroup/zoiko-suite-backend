-- Migration: 000004_add_rls.up.sql
--
-- Only workflow_instances has a real tenant_id column (NOT NULL).
-- workflow_stages and workflow_transitions carry no tenant_id of their
-- own — every access to them already routes through FindWorkflowByID
-- first (see that method's own doc comment), so this migration's RLS
-- policy on workflow_instances is the backstop for the whole service in
-- one place, same as that method already claims for the application
-- layer.
--
-- current_setting(..., true) — missing_ok = true — returns NULL rather
-- than raising when app.tenant_id is unset, so a connection that forgot
-- to set it matches no rows instead of erroring.

ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_instances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON workflow_instances
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
