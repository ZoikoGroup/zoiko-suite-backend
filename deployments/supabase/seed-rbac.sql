-- =============================================================================
-- RBAC seed for authorization-svc  —  DATA, NOT A MIGRATION
-- =============================================================================
--
-- THIS IS NOT A MIGRATION AND THE SCHEMA NEEDS NONE. Every table and column
-- these statements touch already exists in the Supabase project; the write path
-- failures are a code defect (services do not forward the canonical envelope to
-- authorization-svc) plus the absence of the rows below. This file supplies the
-- rows.
--
-- WHAT IT WILL AND WILL NOT FIX, before you spend time on it:
--
--   WILL   make POST /v1/authorize answer PERMITTED instead of
--          DENIED / no_grant for the test principal. That is verifiable
--          immediately with a direct curl, because a caller supplying the full
--          envelope by hand already gets a 200 from that endpoint.
--
--   WILL NOT change the 503s on POST /v1/decisions or POST /v1/tenants. Those
--          come from the calling service sending no envelope headers to
--          authorization-svc, getting a 401, and failing closed. That is a Go
--          change in 56 authz clients, not a database one. Seeding first is
--          still worth it: with the code fixed and no grants, the answer would
--          be DENIED rather than PERMITTED, so both halves are needed and this
--          is the half that can be done now.
--
-- Run it in the Supabase SQL editor. Safe to re-run: every statement is
-- idempotent, and the fixed UUIDs below make it so without needing RETURNING
-- plumbing between statements.
--
-- ── Why app.tenant_id must be set first ──────────────────────────────────────
--
-- authorization_svc.roles has RLS ENABLED and FORCED, and FORCE binds the table
-- owner too — so this runs under the policy even in the SQL editor as postgres.
-- The policy's WITH CHECK is:
--
--     tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
--
-- With app.tenant_id unset, current_setting(...) returns NULL, the comparison is
-- NULL, and the INSERT is REFUSED. Without the set_config below you get a
-- row-level security violation and no role.
--
-- permission_bundles and principal_role_assignments have RLS disabled entirely,
-- so they need no setting. That asymmetry is pre-existing and worth its own look
-- — those two tables carry who-can-do-what and are unprotected by policy.
-- =============================================================================

-- The tenant these grants belong to. Change this and the three ids below
-- together if you are seeding for a different tenant.
--
-- set_config(..., false) rather than SET LOCAL: the Supabase SQL editor may run
-- a script inside its own transaction, and a nested BEGIN/COMMIT there would
-- either warn or close the editor's transaction early. Session-scoped works
-- whether or not an outer transaction exists.
SELECT set_config('app.tenant_id', '11111111-1111-1111-1111-111111111111', false);

-- ── 1. The role ──────────────────────────────────────────────────────────────
-- Fixed role_id so bundles and assignments below can reference it without a
-- lookup. ON CONFLICT targets the (tenant_id, role_code) unique index, so a
-- re-run reactivates rather than erroring.
INSERT INTO authorization_svc.roles
    (role_id, tenant_id, role_code, role_name, role_scope_type,
     active_flag, created_by_principal_id)
VALUES
    ('a0000000-0000-4000-8000-000000000001',
     '11111111-1111-1111-1111-111111111111',
     'SERVICECTL_TEST_ADMIN',
     'servicectl test administrator',
     'TENANT',
     TRUE,
     'servicectl-seed')
ON CONFLICT (tenant_id, role_code) DO UPDATE
    SET active_flag = TRUE,
        role_name   = EXCLUDED.role_name;

-- ── 2. The permission bundle ─────────────────────────────────────────────────
-- permitted_actions is a JSONB array of the flat action_type vocabulary
-- authorization-svc evaluates against.
--
-- These are not invented. They are every action string the three services under
-- test actually send:
--   GOVERNANCE_DECISION_RECORD  governance-decision-log-svc, handler.go
--   the rest                    tenant-entity-registry-svc, rendered by its
--                               ActionType(resource, action) helper, which
--                               uppercases and replaces "." and "-" with "_"
--                               (e.g. entity.hierarchy + create ->
--                               ENTITY_HIERARCHY_CREATE)
-- identity-context-svc authorizes nothing on its login path, so it needs none.
INSERT INTO authorization_svc.permission_bundles
    (permission_bundle_id, role_id, bundle_code, permitted_actions, active_flag)
VALUES
    ('b0000000-0000-4000-8000-000000000001',
     'a0000000-0000-4000-8000-000000000001',
     'SERVICECTL_TEST_BUNDLE',
     '[
        "GOVERNANCE_DECISION_RECORD",
        "TENANT_PROVISION",
        "TENANT_LIFECYCLE_TRANSITION",
        "ENTITY_CREATE",
        "ENTITY_UPDATE",
        "ENTITY_STATUS_TRANSITION",
        "ENTITY_HIERARCHY_CREATE",
        "ENTITY_HIERARCHY_END_DATE",
        "ENTITY_JURISDICTION_ASSIGN",
        "ENTITY_JURISDICTION_END_DATE",
        "WORKSPACE_CREATE",
        "WORKSPACE_UPDATE",
        "WORKSPACE_STATUS_TRANSITION",
        "RESIDENCY_POLICY_CREATE"
      ]'::jsonb,
     TRUE)
ON CONFLICT (role_id, bundle_code) DO UPDATE
    SET permitted_actions = EXCLUDED.permitted_actions,
        active_flag       = TRUE;

-- ── 3. The assignment ────────────────────────────────────────────────────────
-- legal_entity_id IS NULL ON PURPOSE. The evaluation query is:
--
--     WHERE pra.principal_id = $1
--       AND (pra.legal_entity_id = $2 OR pra.legal_entity_id IS NULL)
--
-- so a NULL means tenant-wide and matches every scope the callers pass —
-- including governance-decision-log-svc's platform scope
-- (00000000-0000-0000-0000-00000000f001) when a request carries no
-- X-Legal-Entity-Id. Naming one entity here would grant only that entity and
-- leave the platform-scope path denied, which is a confusing half-fix.
--
-- The column was NOT NULL in 000001 and made nullable by
-- 000003_nullable_legal_entity_for_tenant_scope, so this depends on that
-- migration being applied. It is.
INSERT INTO authorization_svc.principal_role_assignments
    (principal_role_assignment_id, principal_id, role_id, legal_entity_id,
     effective_from, effective_to, assigned_by)
VALUES
    ('c0000000-0000-4000-8000-000000000001',
     '33333333-3333-3333-3333-333333333333',
     'a0000000-0000-4000-8000-000000000001',
     NULL,
     NOW() - INTERVAL '1 minute',   -- effective_from <= NOW() is required
     NULL,                          -- no expiry
     'servicectl-seed')
ON CONFLICT (principal_role_assignment_id) DO UPDATE
    SET effective_from = EXCLUDED.effective_from,
        effective_to   = NULL,
        legal_entity_id = NULL;

-- =============================================================================
-- VERIFICATION — this reproduces the service's own evaluation query
-- =============================================================================
-- Same joins and predicates as authorization-svc's PgStore, so if this returns
-- a row the service will grant. If it returns nothing, the service will answer
-- DENIED / no_grant and the seed did not take.
--
-- roles is read here too, so app.platform_scope stands in for the escape hatch
-- the service's withPlatformScope helper sets on this exact read.
SELECT set_config('app.platform_scope', 'true', false);

SELECT r.role_code,
       pb.permitted_actions,
       pra.legal_entity_id,
       pra.effective_from,
       pra.effective_to
FROM authorization_svc.principal_role_assignments pra
JOIN authorization_svc.roles r
     ON r.role_id = pra.role_id AND r.active_flag
JOIN authorization_svc.permission_bundles pb
     ON pb.role_id = r.role_id AND pb.active_flag
WHERE pra.principal_id = '33333333-3333-3333-3333-333333333333'
  AND (pra.legal_entity_id = '22222222-2222-2222-2222-222222222222'
       OR pra.legal_entity_id IS NULL)
  AND pra.effective_from <= NOW()
  AND (pra.effective_to IS NULL OR pra.effective_to > NOW());
