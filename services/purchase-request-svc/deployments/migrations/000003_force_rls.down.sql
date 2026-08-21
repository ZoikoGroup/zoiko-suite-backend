-- Migration: 000003_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE purchase_requests NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_requests;
CREATE POLICY tenant_isolation_policy ON purchase_requests
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)::UUID);
