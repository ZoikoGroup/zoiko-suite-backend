-- Migration: 000003_force_rls.up.sql
--
-- Makes the tenant isolation policy from 000001 apply to the role that owns the
-- table, and writes its WITH CHECK down explicitly.
--
-- 000001 declared a policy and enabled row-level security, which READS as
-- isolation without being it: Postgres exempts a table's OWNER from row-level
-- security unless the table is declared FORCE, and this service connects as the
-- role init-db.sh created every table as. The policy has therefore never been
-- consulted for a single query the service has ever made. Same defect, and same
-- fix, as notification-svc's and board-resolutions-svc's 000002.
--
-- BE PRECISE ABOUT WHAT THIS DOES AND DOES NOT BUY. FORCE binds the owner; it
-- does NOT bind a SUPERUSER or a role holding BYPASSRLS, which are exempt from
-- the row security system altogether. DB_USER defaults to `postgres` and
-- compose passes `postgres`, which in this image IS a superuser -- so on a
-- local run the policy still does not bite. Granting the services a
-- non-superuser role is a platform-wide change and is not this migration's to
-- make; what this migration does is remove the reason the policy could never
-- work, so that giving the service an ordinary role starts enforcing it.
--
-- The control that actually isolates tenants today is therefore NOT this
-- policy. It is the explicit `tenant_id = $n` predicate the store carries on
-- every statement, over a tenant the handler took from the verified
-- X-Tenant-Id and nowhere else. Before this pass that predicate was fed from a
-- ?tenant_id= query parameter and a body field, and the policy that should have
-- been the backstop was inert -- so there was no isolation at either layer.
ALTER TABLE customer_invoices FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT and UPDATE when no
-- WITH CHECK is given, which is the behaviour wanted here. Stated explicitly
-- because relying on it implicitly is how the write path came to be overlooked:
-- the service was inserting a body-supplied tenant_id, and the only thing that
-- would ever have refused it is a WITH CHECK nobody had written down.
DROP POLICY IF EXISTS tenant_isolation_policy ON customer_invoices;
CREATE POLICY tenant_isolation_policy ON customer_invoices
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id')::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id')::UUID);
