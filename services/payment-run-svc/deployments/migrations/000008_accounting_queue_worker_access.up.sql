-- ZS-SVC-D-001 AP-11: let the accounting dispatcher work across tenants.
--
-- accounting_posting_requests is FORCE ROW LEVEL SECURITY with a policy that
-- shows a row only to its own tenant (or a NULL-tenant row). The dispatcher is
-- a background worker with no request tenant, so under a NOSUPERUSER,
-- NOBYPASSRLS application role it would see only NULL-tenant rows and never
-- post anything.
--
-- Rather than dropping RLS from a table that holds financial payloads, the
-- policy gains one more condition: a transaction-local marker the dispatcher
-- sets (app.queue_worker = 'accounting-dispatcher'). Tenant-facing queries do
-- not set it and stay confined to their own tenant.
DROP POLICY IF EXISTS tenant_isolation ON accounting_posting_requests;
CREATE POLICY tenant_isolation ON accounting_posting_requests
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        OR current_setting('app.queue_worker', true) = 'accounting-dispatcher'
    );
