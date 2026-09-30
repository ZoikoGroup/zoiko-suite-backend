#!/usr/bin/env bash
#
# Seed the minimum data needed to verify identity-context-svc end to end
# against the local compose stack — including through the console's login.
#
# Idempotent: safe to re-run. Touches three databases:
#
#   identity_context     a role assignment, so the admin principal resolves
#   authorization_svc    a role + permission bundle + two assignments, so the
#                        three privileged GOV-01 commands are GRANTED rather
#                        than DENIED
#
# It does NOT seed the password credential — `cmd/seed-local-admin` does that,
# and this script tells you to run it.
#
# WHY TWO ASSIGNMENTS IN authorization_svc. identity-context-svc authorizes its
# tenant-scoped commands against the TENANT id in the legal_entity position —
# "may this principal invalidate context for this tenant" — while ordinary
# entity-scoped reads use the real legal entity. Granting only one of the two
# produces a DENIED with basis `no_grant` that looks like the role never
# applied. Both are seeded; that is not a duplicate.
set -u

PG=${PG:-zoiko-postgres}
TENANT=${TENANT:-11111111-1111-1111-1111-111111111111}
ENTITY=${ENTITY:-22222222-2222-2222-2222-222222222222}
PRINCIPAL=${PRINCIPAL:-33333333-3333-3333-3333-333333333333}
ROLE_ID=aaaa0000-0000-0000-0000-00000000aaaa
BUNDLE_ID=bbbb0000-0000-0000-0000-00000000bbbb

psql_do() { docker exec "$PG" psql -U postgres -d "$1" -c "$2"; }

echo "== identity_context: role assignment for the admin principal =="
psql_do identity_context "
INSERT INTO principal_role_assignments
  (assignment_id, principal_id, tenant_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by)
VALUES
  ('99999999-9999-9999-9999-999999999999','$PRINCIPAL','$TENANT',
   '4924b083-b23d-4ce8-adc3-08691582bee6','$ENTITY',
   now() - interval '1 day', now() + interval '365 days','$PRINCIPAL')
ON CONFLICT (assignment_id) DO NOTHING;"

echo "== authorization_svc: role =="
psql_do authorization_svc "
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('$ROLE_ID','$TENANT','IDENTITY_CONTEXT_ADMIN','Identity Context Administrator','LEGAL_ENTITY',true,'$PRINCIPAL')
ON CONFLICT (role_id) DO NOTHING;"

echo "== authorization_svc: permission bundle =="
psql_do authorization_svc "
INSERT INTO permission_bundles (permission_bundle_id, role_id, bundle_code, permitted_actions, active_flag)
VALUES ('$BUNDLE_ID','$ROLE_ID','IDENTITY_CONTEXT_FULL',
  '[\"IDENTITY_CONTEXT_CACHE_REFRESH\",\"IDENTITY_CONTEXT_TENANT_INVALIDATE\",\"IDENTITY_SUPPORT_CONTEXT_ATTACH\",\"IDENTITY_SUPPORT_CONTEXT_REVOKE\",\"PRINCIPAL_STATUS_MANAGE\"]'::jsonb,
  true)
ON CONFLICT (permission_bundle_id) DO UPDATE
  SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;"

echo "== authorization_svc: assignment at ENTITY scope =="
psql_do authorization_svc "
INSERT INTO principal_role_assignments
  (principal_role_assignment_id, principal_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by)
VALUES ('cccc0000-0000-0000-0000-00000000cccc','$PRINCIPAL','$ROLE_ID','$ENTITY',
        now() - interval '1 day', now() + interval '365 days','$PRINCIPAL')
ON CONFLICT (principal_role_assignment_id) DO NOTHING;"

echo "== authorization_svc: assignment at TENANT scope (see header) =="
psql_do authorization_svc "
INSERT INTO principal_role_assignments
  (principal_role_assignment_id, principal_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by)
VALUES ('dddd0000-0000-0000-0000-00000000dddd','$PRINCIPAL','$ROLE_ID','$TENANT',
        now() - interval '1 day', now() + interval '365 days','$PRINCIPAL')
ON CONFLICT (principal_role_assignment_id) DO NOTHING;"

cat <<'NOTE'

Seed complete.

Still to do by hand, once, from services/identity-context-svc:

  go run ./cmd/seed-local-admin \
    -dsn "postgres://postgres:postgres@localhost:5432/identity_context?sslmode=disable"

That sets the password for admin@zoikosuite.com. authorization-svc caches
decisions for 5 seconds, so allow a moment before expecting a new grant to
take effect.
NOTE
