-- Migration: 000002_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE spend_policies NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON spend_policies;
CREATE POLICY tenant_isolation_policy ON spend_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE spend_consumptions NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON spend_consumptions;
CREATE POLICY tenant_isolation_policy ON spend_consumptions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
