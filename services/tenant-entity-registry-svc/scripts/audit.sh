#!/usr/bin/env bash
#
# ORG-02 / ORG-03 completion audit for tenant-entity-registry-svc.
#
# Re-runs the proof rather than restating it. Every check below is executed
# against a RUNNING service and a REAL database; nothing here reads the source
# and concludes that a thing must work.
#
# Modelled on identity-context-svc/scripts/audit.sh, which exists for the same
# reason: a release certificate listing successes certifies nothing unless the
# reader can re-derive them.
#
# Usage:
#   scripts/audit.sh                      # against http://localhost:8081
#   BASE_URL=... DB=... scripts/audit.sh
#
# Requires: curl, jq, docker (for the database checks).

set -uo pipefail

BASE_URL="${BASE_URL:-http://localhost:8081}"
PG_CONTAINER="${PG_CONTAINER:-zoiko-postgres}"
DB="${DB:-tenant_entity_registry}"

PASS=0
FAIL=0
SKIP=0

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
amber() { printf '\033[33m%s\033[0m' "$1"; }

# ok <description> <condition-result>
ok() {
  if [ "$2" = "0" ]; then
    printf '  %s %s\n' "$(green PASS)" "$1"; PASS=$((PASS + 1))
  else
    printf '  %s %s\n' "$(red FAIL)" "$1"; FAIL=$((FAIL + 1))
  fi
}

skip() { printf '  %s %s\n' "$(amber SKIP)" "$1"; SKIP=$((SKIP + 1)); }

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

psql_q() { docker exec "$PG_CONTAINER" psql -U postgres -d "$DB" -tAc "$1" 2>/dev/null; }

# status <method> <path> [body] [extra-header...]
status() {
  local method="$1" path="$2" body="${3:-}"; shift 3 2>/dev/null || shift 2
  if [ -n "$body" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$method" "$BASE_URL$path" \
      -H 'Content-Type: application/json' "$@" -d "$body"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$method" "$BASE_URL$path" "$@"
  fi
}

printf '\033[1mORG-02 / ORG-03 audit — tenant-entity-registry-svc\033[0m\n'
printf 'Target: %s   Database: %s\n' "$BASE_URL" "$DB"

# ---------------------------------------------------------------------------
section '1. Service is up'
# ---------------------------------------------------------------------------

health=$(status GET /healthz)
ok "/healthz answers 200 (got $health)" "$([ "$health" = "200" ] && echo 0 || echo 1)"
if [ "$health" != "200" ]; then
  printf '\n%s service is not reachable; the rest of this audit would report false failures.\n' "$(red ABORT)"
  exit 1
fi

ready=$(status GET /readyz)
ok "/readyz answers 200 (got $ready)" "$([ "$ready" = "200" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '2. Schema — migration 000006'
# ---------------------------------------------------------------------------

for table in legal_entity_profile_versions tenant_lifecycle_history \
             tenant_host_bindings entity_registry_conflicts event_outbox; do
  n=$(psql_q "SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename='$table';")
  ok "table $table exists" "$([ "$n" = "1" ] && echo 0 || echo 1)"
done

for col in "tenants record_version" "legal_entities record_version"; do
  set -- $col
  n=$(psql_q "SELECT count(*) FROM information_schema.columns WHERE table_name='$1' AND column_name='$2';")
  ok "$1.$2 exists" "$([ "$n" = "1" ] && echo 0 || echo 1)"
done

# ---------------------------------------------------------------------------
section '3. Row-level security (tracker Priority 3, row 56)'
# ---------------------------------------------------------------------------

# ENABLE exempts the table owner from its own policies; FORCE does not.
unforced=$(psql_q "
  SELECT count(*) FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname='public' AND c.relkind='r'
     AND c.relname NOT IN ('residency_regions','tenant_host_bindings','schema_migrations')
     AND NOT c.relforcerowsecurity;")
ok "every tenant-scoped table has FORCE row-level security (unforced: ${unforced:-?})" \
   "$([ "$unforced" = "0" ] && echo 0 || echo 1)"

# The two exemptions are deliberate and documented in migration 000006.
hb=$(psql_q "SELECT relrowsecurity FROM pg_class WHERE relname='tenant_host_bindings';")
ok "tenant_host_bindings is deliberately exempt (read before a tenant is known)" \
   "$([ "$hb" = "f" ] && echo 0 || echo 1)"

# Every policy must have a WITH CHECK half, or an UPDATE can move a row to
# another tenant's id even though USING made it visible.
nocheck=$(psql_q "SELECT count(*) FROM pg_policies WHERE schemaname='public' AND with_check IS NULL;")
ok "every policy declares WITH CHECK (missing: ${nocheck:-?})" \
   "$([ "$nocheck" = "0" ] && echo 0 || echo 1)"

# The runtime role must not be able to bypass any of it.
bypass=$(psql_q "SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname='zoiko_app';")
if [ -z "$bypass" ]; then
  skip "runtime role zoiko_app not found in this database"
else
  ok "runtime role zoiko_app cannot bypass RLS" "$([ "$bypass" = "f" ] && echo 0 || echo 1)"
fi

# ---------------------------------------------------------------------------
section '4. ORG-02 §4.2 — named commands are routed'
# ---------------------------------------------------------------------------

# These assert ROUTING, not the write.
#
# Status code alone cannot tell "this route does not exist" from "this route
# exists and correctly refused": every tenant-scoped READ answers 404 when the
# request carries no verified tenant, because a caller who names no tenant must
# not learn whether a resource exists. An earlier version of this script read
# those 404s as missing routes and reported four false failures.
#
# What separates them is the RESPONDER. chi's unrouted fallback writes
# text/plain "404 page not found"; every handler here writes the JSON error
# envelope. The two never share a content type.
probe_route() {
  local desc="$1" method="$2" path="$3" body="${4:-}"
  local code ctype
  code=$(status "$method" "$path" "$body")
  if [ -n "$body" ]; then
    ctype=$(curl -s -o /dev/null -w '%{content_type}' -X "$method" "$BASE_URL$path" \
      -H 'Content-Type: application/json' -d "$body")
  else
    ctype=$(curl -s -o /dev/null -w '%{content_type}' -X "$method" "$BASE_URL$path")
  fi
  case "$ctype" in
    application/json*) ok "$desc — routed (handler answered $code)" 0 ;;
    *)                 ok "$desc — NOT ROUTED ($code, $ctype)" 1 ;;
  esac
}

probe_route 'POST /v1/tenants/{id}/commands/SuspendTenant' POST \
  "/v1/tenants/00000000-0000-0000-0000-000000000001/commands/SuspendTenant" '{"reason":"audit"}'
probe_route 'POST /v1/tenants/{id}/defaults' POST \
  "/v1/tenants/00000000-0000-0000-0000-000000000001/defaults" '{"primary_locale":"fr-FR","reason":"audit"}'
probe_route 'GET  /v1/tenants/{id}/lifecycle-history' GET \
  "/v1/tenants/00000000-0000-0000-0000-000000000001/lifecycle-history"
probe_route 'GET  /v1/tenants/{id}/defaults' GET \
  "/v1/tenants/00000000-0000-0000-0000-000000000001/defaults"
probe_route 'POST /v1/tenants/{id}/host-bindings' POST \
  "/v1/tenants/00000000-0000-0000-0000-000000000001/host-bindings" '{"hostname":"audit.example"}'

# An unknown command must be refused as a command, not routed as a tenant id.
unknown=$(status POST "/v1/tenants/00000000-0000-0000-0000-000000000001/commands/DeleteTenant" '{"reason":"x"}')
ok "unknown command DeleteTenant is refused (got $unknown)" \
   "$([ "$unknown" != "200" ] && [ "$unknown" != "204" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '5. ORG-02 §4.2 — ResolveTenantByHost (§8 NP2/NP3)'
# ---------------------------------------------------------------------------

# Deliberately unauthenticated: it is how a caller learns its tenant.
unknown_host=$(status GET "/v1/resolve-tenant?hostname=definitely-not-bound.invalid")
ok "an unknown hostname resolves to nothing, not to a default tenant (got $unknown_host)" \
   "$([ "$unknown_host" = "404" ] && echo 0 || echo 1)"

body=$(curl -s "$BASE_URL/v1/resolve-tenant?hostname=definitely-not-bound.invalid")
echo "$body" | grep -qi 'tenant_id' && has_tenant=1 || has_tenant=0
ok "the refusal body names no tenant" "$([ "$has_tenant" = "0" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '6. ORG-03 §4.3 — profile versions and as-of'
# ---------------------------------------------------------------------------

probe_route 'POST /v1/entities/{id}/profile-amendments' POST \
  "/v1/entities/00000000-0000-0000-0000-000000000001/profile-amendments" '{"change_reason":"AMENDMENT"}'
probe_route 'POST /v1/entities/{id}/legal-name' POST \
  "/v1/entities/00000000-0000-0000-0000-000000000001/legal-name" '{"legal_name":"Audit Ltd"}'
probe_route 'POST /v1/entities/{id}/registered-office' POST \
  "/v1/entities/00000000-0000-0000-0000-000000000001/registered-office" '{"registered_office":"{}"}'
probe_route 'GET  /v1/entities/{id}/versions' GET \
  "/v1/entities/00000000-0000-0000-0000-000000000001/versions"
probe_route 'GET  /v1/entities/{id}/as-of' GET \
  "/v1/entities/00000000-0000-0000-0000-000000000001/as-of?as_of=2026-01-01"

# by-registry-number is a STATIC segment that must not be swallowed by the
# {entityID} parameter route next to it.
# A static segment beside a parameter route is the classic chi trap: if
# /v1/entities/{entityID} matched first this would be read as an entity id, and
# the tell is the responder, not the status -- an unscoped read 404s either way.
regct=$(curl -s -o /dev/null -w '%{content_type}' "$BASE_URL/v1/entities/by-registry-number?registration_number=AUDIT")
case "$regct" in
  application/json*) ok "GET /v1/entities/by-registry-number is not shadowed by /v1/entities/{id}" 0 ;;
  *)                 ok "GET /v1/entities/by-registry-number is shadowed ($regct)" 1 ;;
esac

# A malformed as_of must be a 400, not a 500 or a silent default to now.
badasof=$(status GET "/v1/entities/00000000-0000-0000-0000-000000000001/as-of?as_of=not-a-date")
ok "a malformed as_of is rejected as bad input (got $badasof)" \
   "$([ "$badasof" = "400" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '7. §8 NP5 — registry conflict quarantine'
# ---------------------------------------------------------------------------

probe_route 'GET  /v1/registry-conflicts' GET "/v1/registry-conflicts"
probe_route 'POST /v1/registry-conflicts/{id}/resolution' POST \
  "/v1/registry-conflicts/00000000-0000-0000-0000-000000000001/resolution" \
  '{"status":"DISMISSED","resolution_note":"audit"}'

# The CHECK that stops an OPEN conflict claiming to have been resolved.
erc=$(psql_q "SELECT count(*) FROM pg_constraint WHERE conname='erc_resolution_complete';")
ok "constraint erc_resolution_complete is present" "$([ "$erc" = "1" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '8. §4.2 maker-checker is enforced by the database, not only the service'
# ---------------------------------------------------------------------------

tlh=$(psql_q "SELECT count(*) FROM pg_constraint WHERE conname='tlh_no_self_approval';")
ok "constraint tlh_no_self_approval is present" "$([ "$tlh" = "1" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '9. §9.2 — transactional outbox'
# ---------------------------------------------------------------------------

relay=$(psql_q "SELECT count(*) FROM pg_policies WHERE tablename='event_outbox' AND qual LIKE '%outbox_relay%';")
ok "event_outbox policy carries the named relay capability" \
   "$([ "$relay" = "1" ] && echo 0 || echo 1)"

idx=$(psql_q "SELECT count(*) FROM pg_indexes WHERE tablename='event_outbox' AND indexname='idx_event_outbox_pending';")
ok "the relay's claim index exists" "$([ "$idx" = "1" ] && echo 0 || echo 1)"

stuck=$(psql_q "SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND attempts > 8;")
if [ "${stuck:-0}" = "0" ]; then
  ok "no event is stuck in the outbox past 8 attempts" 0
else
  ok "no event is stuck in the outbox past 8 attempts (stuck: $stuck)" 1
fi

# ---------------------------------------------------------------------------
section '10. Contracts'
# ---------------------------------------------------------------------------

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -f "$HERE/openapi.yaml" ]; then
  paths=$(grep -cE '^  /v1/' "$HERE/openapi.yaml")
  ok "openapi.yaml declares $paths /v1 paths" "$([ "$paths" -ge 30 ] && echo 0 || echo 1)"
else
  ok 'openapi.yaml is present' 1
fi

if [ -f "$HERE/asyncapi.yaml" ]; then
  msgs=$(grep -cE '^    [A-Z][A-Za-z]+:$' "$HERE/asyncapi.yaml")
  ok "asyncapi.yaml declares $msgs messages" "$([ "$msgs" -ge 15 ] && echo 0 || echo 1)"
else
  ok 'asyncapi.yaml exists' 1
fi

# The parity between these files and the code is asserted by Go tests, which is
# where it belongs — this only checks the files are present and non-trivial.

# ---------------------------------------------------------------------------
printf '\n\033[1mResult:\033[0m %s passed, %s failed, %s skipped\n' \
  "$(green "$PASS")" "$([ "$FAIL" -gt 0 ] && red "$FAIL" || echo "$FAIL")" "$SKIP"

[ "$FAIL" -eq 0 ] || exit 1
