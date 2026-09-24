-- Migration: 000007_add_rls_bundles_assignments.up.sql
--
-- Closes tracker item 82 for the two tables it still covers:
-- permission_bundles and principal_role_assignments. These are the tables
-- that carry WHO-CAN-DO-WHAT — a bundle is the list of granted actions, an
-- assignment is the fact that a principal holds it — and until now both had
-- no policy and no ENABLE at all. `roles` and `sod_rules` were protected by
-- 000004, access_decision_log by 000005, delegated_authorities by 000006;
-- these two were the remaining hole, and they are the most sensitive of the
-- six.
--
-- ── Why no tenant_id column is added here ───────────────────────────────
--
-- 000004 declined to write these policies because neither table has a
-- tenant_id, and it was right that fabricating one is a data-model change
-- rather than an RLS migration. 000005 and 000006 solved their tables by
-- adding the column and backfilling.
--
-- These two do not need that, and are better off without it. Both carry
--
--     role_id UUID NOT NULL REFERENCES roles(role_id)
--
-- and roles.tenant_id is itself NOT NULL. The owning tenant is therefore
-- already a total function of the row — derivable, never absent. Adding a
-- copy of it would introduce two failure modes that the FK route does not
-- have: a nullable window during backfill, and the standing possibility of
-- a bundle whose tenant_id disagrees with its own role's. A denormalised
-- tenant on a mandatory-FK child is a cache, and a cache of the value the
-- policy depends on is the wrong thing to protect a policy with.
--
-- So the policy reads the tenant through the FK instead.
--
-- ── The platform-scope escape hatch is NOT optional here ────────────────
--
-- PgStore.FindGrantedActions — the core POST /v1/authorize path, called on
-- nearly every mutating request platform-wide — joins all three of roles,
-- permission_bundles and principal_role_assignments. When it is called with
-- no tenant scope it runs inside withPlatformScope, which sets
-- app.platform_scope = 'true'; roles' own policy from 000004 already honours
-- that flag.
--
-- If these two policies did not honour it too, that join would match zero
-- rows for every tenantless call, and the service would answer DENIED
-- `no_grant` instead of the truth — authorization silently failing closed
-- across the platform, with no error anywhere to find it by. That is the
-- audit-event-store-svc failure mode from Priority 1 (a naive policy taking
-- a Tier-0 service offline quietly), and it is the reason the OR clause
-- below exists. It is proven by test, not assumed: see
-- TestPgStore_RLS_PlatformScopeStillResolvesGrants.
--
-- The flag grants visibility, not access. handler.go compares every result
-- against its own verified X-Tenant-Id and refuses on mismatch, and nothing
-- but withPlatformScope sets it.
--
-- ── Reading `roles` from inside these policies ──────────────────────────
--
-- roles has RLS ENABLED and FORCED, so the EXISTS sub-selects below are
-- themselves subject to roles' policy. That composes correctly and is
-- deliberate: with app.tenant_id set, the sub-select can only see that
-- tenant's roles, so a foreign bundle fails the EXISTS on the inner policy
-- before the outer predicate is even considered. The two layers agree
-- rather than fighting.
--
-- current_setting(..., true) returns NULL rather than raising when the
-- setting is absent, and NULLIF guards the '' that withRLS installs for a
-- genuinely tenantless call — ''::uuid raises, and an error is not the same
-- as "matches nothing".

BEGIN;

-- ── permission_bundles ──────────────────────────────────────────────────

ALTER TABLE permission_bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE permission_bundles FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON permission_bundles
    FOR ALL
    USING (
        current_setting('app.platform_scope', true) = 'true'
        OR EXISTS (
            SELECT 1 FROM roles r
             WHERE r.role_id = permission_bundles.role_id
               AND r.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM roles r
             WHERE r.role_id = permission_bundles.role_id
               AND r.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- ── principal_role_assignments ──────────────────────────────────────────
--
-- Same shape. Note the WITH CHECK deliberately does NOT carry the
-- platform-scope hatch: a read may need to cross tenants to resolve a
-- grant, but nothing in this service has a legitimate reason to WRITE an
-- assignment outside the caller's verified tenant. Every admin write path
-- goes through handler.requireTenant first.

ALTER TABLE principal_role_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_role_assignments FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON principal_role_assignments
    FOR ALL
    USING (
        current_setting('app.platform_scope', true) = 'true'
        OR EXISTS (
            SELECT 1 FROM roles r
             WHERE r.role_id = principal_role_assignments.role_id
               AND r.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM roles r
             WHERE r.role_id = principal_role_assignments.role_id
               AND r.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- The policies read `roles`, so an ordinary app role needs SELECT on it.
-- It already has DML on every table in the schema via create-app-roles.sh;
-- this is stated explicitly so the dependency is visible if that script
-- is ever narrowed.
--
-- (No GRANT is issued here: migrations run as the owner, and narrowing the
-- app role's grants is a change to that script, not to this migration.)

COMMIT;
