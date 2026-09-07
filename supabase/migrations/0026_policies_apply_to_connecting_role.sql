-- 0026_policies_apply_to_connecting_role.sql
-- Platform-wide correction. Creates no tables.
--
-- ── The defect ───────────────────────────────────────────────────────────────
--
-- Every tenant policy in 0001-0025 is written `FOR ALL TO zoiko_backend`, and
-- the services do not connect as zoiko_backend. deployments/supabase creates a
-- role per service — app_identity_context, app_employee_master and so on — and
-- docker-compose.supabase.yml points each service's DB_USER at its own.
--
-- A policy names the roles it applies to. With FORCE ROW LEVEL SECURITY and no
-- policy applicable to the connecting role, that role reads zero rows and
-- writes nothing. Verified: as app_employee_master the register returned
-- `(0 rows)` and an INSERT failed with "new row violates row-level security
-- policy" — for a row in its own tenant, in its own schema.
--
-- ── Why the obvious fix is worse ─────────────────────────────────────────────
--
-- GRANT zoiko_backend TO app_employee_master makes the policy match. It also
-- hands over everything zoiko_backend holds, and every migration grants that
-- role USAGE and DML on its own schema — so zoiko_backend accumulates access to
-- all 25. Verified: with that membership, app_employee_master read
-- payroll_run.pay_slips and inserted a row into it. Tenant isolation survived;
-- SERVICE isolation did not, and "USAGE on its own schema only" is the property
-- the role-per-service design exists to provide.
--
-- ── The fix ──────────────────────────────────────────────────────────────────
--
-- Drop the role restriction. A policy with no TO clause applies to every role,
-- and isolation comes from the grants that were already there: a role holds
-- USAGE on one schema and DML on that schema's tables, so it cannot reach
-- another service's tables whatever the policy says.
--
-- Verified on the same probe: own schema readable, a write to another tenant
-- refused by the policy, and a read of another service's schema refused with
-- "permission denied for schema payroll_run" — at the grant layer, before RLS
-- is consulted at all.
--
-- Widening to PUBLIC does not expose anything to `anon`. Table privileges are
-- checked before row security, and no migration grants anon anything, so anon
-- is refused at the ACL layer and never reaches a policy.
--
-- ── Why this is generated rather than written out ────────────────────────────
--
-- 58 policies name zoiko_backend across 25 files, plus 4 that name it alongside
-- `authenticated`. Transcribing 62 DROP/CREATE pairs by hand invites exactly one
-- typo in exactly one USING clause, which would silently widen a tenant
-- boundary. Reading each policy's own expression out of pg_policies and putting
-- it back verbatim cannot drift from what was reviewed.
--
-- The 38 policies that name ONLY `authenticated` are left alone. Those are the
-- deliberate console-session reads — a recipient reading their own notification,
-- an employee reading the holiday calendar — and they are correctly restricted.

-- ── 1. Policies apply to the connecting role ─────────────────────────────────

DO $$
DECLARE
    p        record;
    stmt     text;
    changed  int := 0;
BEGIN
    FOR p IN
        SELECT schemaname, tablename, policyname, cmd, qual, with_check
          FROM pg_policies
         WHERE 'zoiko_backend' = ANY (roles)
         ORDER BY schemaname, tablename, policyname
    LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I',
                       p.policyname, p.schemaname, p.tablename);

        -- cmd comes from the catalogue as ALL | SELECT | INSERT | UPDATE |
        -- DELETE, so it is a keyword rather than anything caller-supplied.
        stmt := format('CREATE POLICY %I ON %I.%I FOR %s',
                       p.policyname, p.schemaname, p.tablename, p.cmd);

        -- qual is NULL for an INSERT-only policy and with_check is NULL for a
        -- SELECT or DELETE one. Emitting only what was there keeps each policy
        -- legal for its own command without a per-command branch.
        IF p.qual IS NOT NULL THEN
            stmt := stmt || format(' USING (%s)', p.qual);
        END IF;
        IF p.with_check IS NOT NULL THEN
            stmt := stmt || format(' WITH CHECK (%s)', p.with_check);
        END IF;

        EXECUTE stmt;
        changed := changed + 1;
    END LOOP;

    RAISE NOTICE 'policies rewritten to apply to the connecting role: %', changed;
END
$$;

-- ── 2. Every service role can call the identity helpers ──────────────────────
--
-- A policy expression is evaluated as the querying role, so a role that now
-- matches the policy must also be able to run app.current_tenant_id(). 0001
-- granted that to zoiko_backend, authenticated and anon only.
--
-- Guarded on the role existing: on a fresh project the migrations run before
-- deployments/supabase creates the roles, and a missing one must not abort the
-- batch. deployments/supabase performs the same grants, so a later run of it
-- covers roles created after this migration.

DO $$
DECLARE
    r       text;
    granted int := 0;
BEGIN
    FOREACH r IN ARRAY ARRAY[
        'app_identity_context',
        'app_tenant_entity_registry',
        'app_authorization',
        'app_governance_decision_log',
        'app_access_control',
        'app_employee_master',
        'app_leave_absence',
        'app_compensation',
        'app_payroll_run'
    ]
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            CONTINUE;
        END IF;

        EXECUTE format('GRANT USAGE ON SCHEMA app TO %I', r);
        EXECUTE format('GRANT EXECUTE ON FUNCTION app.current_tenant_id() TO %I', r);
        EXECUTE format('GRANT EXECUTE ON FUNCTION app.current_principal_id() TO %I', r);

        -- Undo the membership if it was granted as a workaround for the defect
        -- this migration fixes. It is what carried cross-service access, and it
        -- is no longer needed for the policy to apply.
        --
        -- Left INHERIT: it is harmless with no memberships, and a role that is
        -- NOINHERIT while holding one would need an explicit SET ROLE per
        -- session, which no service issues.
        EXECUTE format('REVOKE zoiko_backend FROM %I', r);

        granted := granted + 1;
    END LOOP;

    RAISE NOTICE 'service roles granted the identity helpers: %', granted;
END
$$;

-- ── 3. Verification ──────────────────────────────────────────────────────────
--
-- Fails the migration if any policy still carries a role restriction naming
-- zoiko_backend. Cheaper to find here than as an empty register in a service.

DO $$
DECLARE leftover int;
BEGIN
    SELECT count(*) INTO leftover
      FROM pg_policies
     WHERE 'zoiko_backend' = ANY (roles);

    IF leftover > 0 THEN
        RAISE EXCEPTION
            '% policies still restricted to zoiko_backend; the rewrite did not complete',
            leftover;
    END IF;
END
$$;
