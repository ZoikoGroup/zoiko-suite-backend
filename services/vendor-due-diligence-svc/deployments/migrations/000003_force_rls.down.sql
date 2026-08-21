-- Migration: 000003_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE vendor_dd_checks NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON vendor_dd_checks;
CREATE POLICY tenant_isolation_policy ON vendor_dd_checks
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE vendor_dd_evidence NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON vendor_dd_evidence;
CREATE POLICY tenant_isolation_policy ON vendor_dd_evidence
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
