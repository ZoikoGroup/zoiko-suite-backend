-- ZS-SVC-D-001 AP-02: let the expiry sweeper find due requisitions across tenants.
--
-- ExpireDue discovers requisitions whose expires_at has passed. It is a system
-- job with no request tenant, and purchase_requests is FORCE ROW LEVEL SECURITY
-- with a tenant_isolation_policy that shows a row only to its own tenant. Under
-- an ordinary NOSUPERUSER NOBYPASSRLS role (the role create-app-roles.sh
-- provisions) that discovery query sees ZERO rows, so no requisition would ever
-- expire -- silently, because as a superuser it works.
--
-- The fix is deliberately narrow: an additional policy for SELECT only, granted
-- to a transaction that sets app.queue_worker = 'requisition-expiry'. It lets
-- the sweeper READ across tenants to find work. It grants no write: each expiry
-- is then applied through the ordinary tenant-scoped transition under the owning
-- tenant's app.tenant_id, so tenant_isolation_policy still governs every UPDATE.
DROP POLICY IF EXISTS expiry_worker_read_policy ON purchase_requests;
CREATE POLICY expiry_worker_read_policy ON purchase_requests
    FOR SELECT
    USING (current_setting('app.queue_worker', true) = 'requisition-expiry');
