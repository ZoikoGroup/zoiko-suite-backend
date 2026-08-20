-- Migration: 000002_force_rls.up.sql
--
-- Makes the tenant isolation policy from 000001 apply to the role that owns
-- these tables, and writes its WITH CHECK down explicitly.
--
-- 000001 declared a policy and enabled row-level security, which READS as
-- isolation without being it: Postgres exempts a table's OWNER from row-level
-- security unless the table is declared FORCE, and this service connects as the
-- role init-db.sh created every table as. The policy has therefore never been
-- consulted for a single query this service has ever made.
--
-- BE PRECISE ABOUT WHAT THIS DOES AND DOES NOT BUY. FORCE binds the owner; it
-- does NOT bind a SUPERUSER or a role holding BYPASSRLS, which are exempt from
-- the row security system altogether. DB_USER is `postgres` everywhere in
-- compose, and in this image that IS a superuser -- so on a local run the policy
-- still does not bite. Giving the services an ordinary role is a platform-wide
-- change and is not this migration's to make; what this does is remove the
-- reason the policy could never work, so that granting an ordinary role starts
-- enforcing it.
--
-- The control that actually isolates tenants today is therefore NOT this policy.
-- It is the explicit `tenant_id = $n` predicate the store carries on every
-- statement, over a tenant the handler took from the verified X-Tenant-Id and
-- nowhere else.
--
-- `current_setting('app.tenant_id', true)` -- missing_ok = true -- returns NULL
-- rather than raising when the setting is absent, so a query that forgot to set
-- the scope matches no rows instead of erroring. Same posture as
-- accounts-receivable-svc's 000003 and notification-svc's 000002.

ALTER TABLE fiscal_periods FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT and UPDATE when no
-- WITH CHECK is given. Stated explicitly because relying on it implicitly is how
-- a write path comes to be overlooked: what refuses an insert carrying another
-- tenant's id is a WITH CHECK somebody wrote down.
DROP POLICY IF EXISTS tenant_isolation_policy ON fiscal_periods;
CREATE POLICY tenant_isolation_policy ON fiscal_periods
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE close_evidences FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT and UPDATE when no
-- WITH CHECK is given. Stated explicitly because relying on it implicitly is how
-- a write path comes to be overlooked: what refuses an insert carrying another
-- tenant's id is a WITH CHECK somebody wrote down.
DROP POLICY IF EXISTS tenant_isolation_policy ON close_evidences;
CREATE POLICY tenant_isolation_policy ON close_evidences
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
