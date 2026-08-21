-- Migration: 000002_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE purchase_orders NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_orders;
CREATE POLICY tenant_isolation_policy ON purchase_orders
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)::UUID);

ALTER TABLE purchase_order_amendments NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_amendments;
CREATE POLICY tenant_isolation_policy ON purchase_order_amendments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)::UUID);
