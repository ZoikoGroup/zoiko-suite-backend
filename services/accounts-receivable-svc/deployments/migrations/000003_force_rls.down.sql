-- Migration: 000003_force_rls.down.sql
--
-- Returns the table to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.
ALTER TABLE customer_invoices NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON customer_invoices;
CREATE POLICY tenant_isolation_policy ON customer_invoices
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
