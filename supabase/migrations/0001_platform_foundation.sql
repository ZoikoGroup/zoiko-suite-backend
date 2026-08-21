-- 0001_platform_foundation.sql
-- Zoiko Suite on Supabase — shared foundation.
--
-- Applied ONCE, before any service migration. Every per-service migration in
-- this directory depends on the roles, schemas and helper functions created
-- here and will fail without it.
--
-- ── Why this file exists ─────────────────────────────────────────────────────
--
-- On the docker-compose estate every service connects as the Postgres
-- SUPERUSER, and a superuser bypasses row-level security unconditionally. The
-- 119 RLS policies across the estate are therefore present, correctly written,
-- and never executed. Moving to Supabase is the opportunity to make them
-- load-bearing, but only if the backend stops connecting as a bypassing role —
-- so this file creates `zoiko_backend`, a plain NOSUPERUSER / NOBYPASSRLS role
-- that the Go services use instead.
--
-- Note the same trap exists on Supabase in a new costume: the `service_role`
-- key used by supabase-js has BYPASSRLS. Anything that connects with the
-- service-role key is exactly as unprotected as the old superuser. Backend
-- services must use the `zoiko_backend` connection string, not that key.

-- ── Roles ────────────────────────────────────────────────────────────────────

-- The role every Go service connects as. NOT a superuser and NOT BYPASSRLS,
-- which is the entire point: FORCE ROW LEVEL SECURITY on the service tables
-- then applies to it, and the tenant policies actually run.
--
-- Password is set here as a placeholder and MUST be rotated before this is
-- pointed at anything real:
--   ALTER ROLE zoiko_backend WITH PASSWORD '<from your secret manager>';
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_backend') THEN
        CREATE ROLE zoiko_backend LOGIN PASSWORD 'change_me_before_use'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END
$$;

-- ── app schema: helpers, not data ────────────────────────────────────────────

CREATE SCHEMA IF NOT EXISTS app;
COMMENT ON SCHEMA app IS
    'Platform helper functions. Owns no tables — service data lives in one schema per service.';

GRANT USAGE ON SCHEMA app TO zoiko_backend, authenticated, anon;

-- ── Identity helpers ─────────────────────────────────────────────────────────
--
-- Both helpers resolve identity from two sources, in order:
--
--   1. The PostgREST JWT claims (`request.jwt.claims`) — how a request arriving
--      through Supabase's own API surface carries identity.
--   2. `app.tenant_id` / `app.principal_id` set with set_config(...) on the
--      transaction — how the Go services already do it today, in withRLS /
--      withTenantTx.
--
-- Source 2 is a deliberate compatibility bridge. It means a service can be
-- pointed at Supabase WITHOUT rewriting its store layer, and the policies still
-- constrain it. Drop the fallback once every service authenticates through the
-- gateway with a real JWT.
--
-- STABLE, not IMMUTABLE: the value is fixed within a statement but varies
-- between transactions. Marking it IMMUTABLE would let the planner cache a
-- tenant across transactions, which would be a cross-tenant leak.

CREATE OR REPLACE FUNCTION app.current_tenant_id()
RETURNS TEXT
LANGUAGE sql
STABLE
SECURITY INVOKER
SET search_path = ''
AS $$
    SELECT COALESCE(
        NULLIF(current_setting('request.jwt.claims', true)::jsonb -> 'app_metadata' ->> 'tenant_id', ''),
        NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'tenant_id', ''),
        NULLIF(current_setting('app.tenant_id', true), '')
    );
$$;

COMMENT ON FUNCTION app.current_tenant_id() IS
    'Caller tenant, from the JWT claim or from set_config(''app.tenant_id''). NULL when neither is set — policies must treat NULL as "match nothing", never as "match all".';

CREATE OR REPLACE FUNCTION app.current_principal_id()
RETURNS TEXT
LANGUAGE sql
STABLE
SECURITY INVOKER
SET search_path = ''
AS $$
    SELECT COALESCE(
        NULLIF(current_setting('request.jwt.claims', true)::jsonb ->> 'sub', ''),
        NULLIF(current_setting('app.principal_id', true), '')
    );
$$;

COMMENT ON FUNCTION app.current_principal_id() IS
    'Verified caller principal. Use as a column DEFAULT for created_by/actor columns so attribution cannot be supplied by the request body.';

GRANT EXECUTE ON FUNCTION app.current_tenant_id() TO zoiko_backend, authenticated, anon;
GRANT EXECUTE ON FUNCTION app.current_principal_id() TO zoiko_backend, authenticated, anon;

-- ── Append-only enforcement ──────────────────────────────────────────────────
--
-- Several services own evidence tables that are documented as append-only —
-- governance decisions, replay manifests, purchase-order amendments, document
-- access logs. Withholding the UPDATE/DELETE grant is enough to stop an
-- ordinary role such as zoiko_backend, and that is how most of them are
-- protected.
--
-- It is NOT enough for a privileged one. A superuser bypasses privilege checks
-- and row-level security entirely, and Supabase's `service_role` key carries
-- BYPASSRLS — so on the tables where mutation would destroy evidence rather
-- than merely corrupt state, a BEFORE trigger is used as well. A trigger is not
-- bypassed by any role, which makes it the only control in this database that
-- still binds the service-role key.
--
-- SECURITY INVOKER (the default) is deliberate: this must run with the caller's
-- rights and simply refuse, never act on anyone's behalf.

CREATE OR REPLACE FUNCTION app.reject_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '%.% is append-only: % is not permitted',
        TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP;
END;
$$;

COMMENT ON FUNCTION app.reject_mutation() IS
    'Refuses UPDATE and DELETE on append-only evidence tables. A trigger is used because it binds even a BYPASSRLS role such as Supabase''s service_role, which no policy or grant in this database does.';

-- ── Convention notes for the per-service migrations ──────────────────────────
--
-- 1. ONE SCHEMA PER SERVICE. The estate has 63 databases today; Supabase is a
--    single database. Schema-per-service preserves the ownership boundary and
--    keeps the five colliding table names apart — `obligations`,
--    `filing_requirements`, `access_decision_log`, `delegated_authorities` and
--    `principal_role_assignments` are each defined by two different services
--    and would overwrite each other in a flat `public`.
--
-- 2. EVERY TENANT-SCOPED TABLE GETS *BOTH* `ENABLE` AND `FORCE` ROW LEVEL
--    SECURITY. Ten of the twenty finished services enable without forcing,
--    which does nothing at all when the connecting role owns the table.
--
-- 3. POLICIES FAIL CLOSED ON A MISSING TENANT. Written as
--       tenant_id = app.current_tenant_id()
--    which is NULL-safe by accident of SQL semantics: NULL = anything is NULL,
--    not true, so a connection that never set a tenant sees zero rows. Never
--    write `app.current_tenant_id() IS NULL OR tenant_id = ...` — that is the
--    document-vault defect, a filter that switches itself off when the header
--    is absent.
--
-- 4. EVERY POLICY CARRIES `WITH CHECK`, not just `USING`. USING governs which
--    rows are visible; WITH CHECK governs which rows may be written. A policy
--    with only USING lets a caller INSERT a row into another tenant that it
--    then cannot see — the write-side integrity gap already open in
--    obligations-svc.
