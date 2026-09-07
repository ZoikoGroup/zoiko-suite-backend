-- 0027_tenant_registry_fail_closed.sql
-- tenant-entity-registry-svc → schema `tenant_entity_registry`. Creates no tables.
--
-- ── Why this schema is not covered by 0001-0026 ──────────────────────────────
--
-- tenant_entity_registry has no migration in this directory. It reached Supabase
-- by a different route: deployments/supabase applies each service's COMPOSE
-- migrations into a schema, and those were written for the database-per-service
-- estate where RLS was decorative — every service connected as the Postgres
-- superuser, so no policy ever executed.
--
-- The result is seven tables carrying the compose-era policy shape, in the
-- schema that is the root of all scope on this platform. Found by surveying the
-- live project rather than the migration set:
--
--   tenants, workspaces, legal_entities, entity_hierarchies,
--   entity_jurisdiction_assignments, tax_identity_bundles,
--   data_residency_policies
--
-- ── The defect: the cast raises where it should match nothing ────────────────
--
-- All seven policies compare `tenant_id = (current_setting('app.tenant_id', …))::uuid`.
-- The cast fails on any value that is not a UUID, and the empty string is not a
-- UUID — so a connection that installed no tenant, or a malformed one, gets
--
--     ERROR:  invalid input syntax for type uuid: ""
--
-- from every table in the schema, where the intended behaviour is zero rows.
-- Measured before this migration on a reproduction of the live shape: reads with
-- no tenant installed, and with a non-UUID tenant, both errored.
--
-- That is fail-LOUD, not fail-open, so it is not a leak. It is still wrong: a
-- service behaving correctly against a connection that has not yet installed a
-- tenant gets a 500 instead of an empty result, and the failure names a type
-- cast rather than the missing identity that caused it.
--
-- Comparing `tenant_id::text = app.current_tenant_id()` degrades to "matches
-- nothing" — the same correction 0004 made for accounts_payable, for the same
-- reason, and the shape every migration in this set uses.
--
-- `workspaces` additionally omitted missing_ok — `current_setting('app.tenant_id')`
-- with no second argument — which would raise on an unset setting even without
-- the cast. Removing the cast removes both failure modes at once.
--
-- ── What WITH CHECK does and does not fix here ────────────────────────────────
--
-- The seven policies carried USING and no WITH CHECK, and the obvious reading is
-- the one 0001 warns about: "with only USING, a caller can INSERT a row into
-- another tenant that it then cannot see."
--
-- That reading is WRONG for these policies, and the reproduction proved it. When
-- a FOR ALL policy omits WITH CHECK, Postgres applies the USING expression to
-- the write check as well — so a cross-tenant INSERT was already refused:
--
--     ERROR:  new row violates row-level security policy for table "workspaces"
--
-- The warning in 0001 holds where a permissive SELECT policy and a separate
-- INSERT policy exist side by side, not for a single FOR ALL policy. WITH CHECK
-- is written out below because being explicit is worth having when the next
-- reader asks the same question — not because it closes a hole.
--
-- ── What this migration does NOT do ──────────────────────────────────────────
--
-- FORCE is applied for consistency and for the day ownership changes, but on
-- Supabase today it is close to cosmetic: these tables are owned by `postgres`,
-- and `postgres` carries BYPASSRLS, which overrides FORCE. The load-bearing
-- change here is WITH CHECK, which applies to app_tenant_entity_registry — a
-- NOBYPASSRLS role that RLS already governed on the read side only.
--
-- It also leaves alone five tables in this project that have NO row security at
-- all, because none of them has a tenant_id column to write a policy against:
--
--   authorization_svc.principal_role_assignments   (principal_id, legal_entity_id)
--   authorization_svc.delegated_authorities        (legal_entity_id)
--   authorization_svc.permission_bundles           (no scope columns)
--   tenant_entity_registry.residency_regions       (no scope columns)
--   zoiko_platform.schema_migrations               (bookkeeping)
--
-- Giving the authorization_svc tables tenant isolation means ADDING a tenant
-- dimension, as 0018 had to for obligations. That is a schema change with live
-- data in the service every write on the platform consults, and it belongs in
-- its own migration with its own review. Their present exposure is bounded by
-- grants: only app_authorization can read them, and `authenticated` cannot.

-- ── Rewrite the seven policies ───────────────────────────────────────────────
--
-- Generated from the catalogue rather than transcribed. The seven differ from
-- each other only in table name, and reading each one back out of pg_policies
-- means the tenant column is discovered rather than assumed — a hand-written set
-- would invite one wrong column name in one USING clause, which is precisely a
-- silently widened tenant boundary.

DO $$
DECLARE
    p        record;
    rewrote  int := 0;
BEGIN
    FOR p IN
        SELECT c.relname AS tablename, pol.polname AS policyname
          FROM pg_policy pol
          JOIN pg_class c     ON c.oid = pol.polrelid
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'tenant_entity_registry'
           -- Only tables that actually carry a tenant column. Anything else in
           -- this schema is reference data and is not touched.
           AND EXISTS (
               SELECT 1 FROM pg_attribute a
                WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
                  AND a.attnum > 0 AND NOT a.attisdropped
           )
         ORDER BY c.relname
    LOOP
        EXECUTE format('DROP POLICY %I ON tenant_entity_registry.%I',
                       p.policyname, p.tablename);

        -- tenant_id::text against app.current_tenant_id():
        --   - NULL when no tenant is installed, and NULL is not true, so the
        --     connection sees nothing and can write nothing. Never rewrite as
        --     "app.current_tenant_id() IS NULL OR ..." — that is a filter which
        --     switches itself off exactly when identity is absent.
        --   - No cast on the setting, so a malformed value matches nothing
        --     rather than raising.
        EXECUTE format($fmt$
            CREATE POLICY %I ON tenant_entity_registry.%I
                FOR ALL
                USING      (tenant_id::text = app.current_tenant_id())
                WITH CHECK (tenant_id::text = app.current_tenant_id())
        $fmt$, p.policyname, p.tablename);

        EXECUTE format('ALTER TABLE tenant_entity_registry.%I ENABLE ROW LEVEL SECURITY', p.tablename);
        EXECUTE format('ALTER TABLE tenant_entity_registry.%I FORCE  ROW LEVEL SECURITY', p.tablename);

        rewrote := rewrote + 1;
    END LOOP;

    RAISE NOTICE 'tenant_entity_registry policies given a WITH CHECK and forced: %', rewrote;
END
$$;

-- ── Verification ─────────────────────────────────────────────────────────────
--
-- Fails the migration rather than leaving a half-applied state to be discovered
-- later as a write that should have been refused.

DO $$
DECLARE
    no_check   int;
    not_forced int;
BEGIN
    SELECT count(*) INTO no_check
      FROM pg_policies
     WHERE schemaname = 'tenant_entity_registry' AND with_check IS NULL;

    SELECT count(*) INTO not_forced
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'tenant_entity_registry' AND c.relkind = 'r'
       AND c.relrowsecurity AND NOT c.relforcerowsecurity;

    IF no_check > 0 THEN
        RAISE EXCEPTION '% tenant_entity_registry policies still have no WITH CHECK', no_check;
    END IF;
    IF not_forced > 0 THEN
        RAISE EXCEPTION '% tenant_entity_registry tables are enabled but not forced', not_forced;
    END IF;
END
$$;
