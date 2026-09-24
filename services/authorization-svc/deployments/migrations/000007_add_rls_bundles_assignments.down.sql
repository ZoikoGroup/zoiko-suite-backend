-- Migration: 000007_add_rls_bundles_assignments.down.sql
--
-- Drops the policy before disabling RLS, so the table is never left with a
-- policy that nothing enforces — an inert CREATE POLICY reads like
-- protection while providing none, and that is worse than no policy at all
-- for anyone auditing this schema later.
--
-- Reverting this migration puts permission_bundles and
-- principal_role_assignments back to having NO tenant isolation at the
-- database layer. The application predicate in PgStore remains, so tenant
-- scoping still holds for every query that carries it — but a query that
-- forgets one returns every tenant's rows again instead of none. Only run
-- this to reproduce the pre-000007 state deliberately (the negative control
-- in pg_store_test.go does exactly that).

BEGIN;

DROP POLICY IF EXISTS tenant_isolation_policy ON principal_role_assignments;
ALTER TABLE principal_role_assignments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE principal_role_assignments DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON permission_bundles;
ALTER TABLE permission_bundles NO FORCE ROW LEVEL SECURITY;
ALTER TABLE permission_bundles DISABLE ROW LEVEL SECURITY;

COMMIT;
