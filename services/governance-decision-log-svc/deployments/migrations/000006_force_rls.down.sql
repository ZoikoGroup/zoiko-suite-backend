-- Migration: 000006_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE governance_decisions NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON governance_decisions;
CREATE POLICY tenant_isolation_policy ON governance_decisions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
