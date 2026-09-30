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
     AND c.relname NOT IN ('residency_regions','tenant_host_bindings','tenant_onboarding_keys','schema_migrations')
     AND NOT c.relforcerowsecurity;")
ok "every tenant-scoped table has FORCE row-level security (unforced: ${unforced:-?})" \
   "$([ "$unforced" = "0" ] && echo 0 || echo 1)"

# The exemptions are deliberate: residency_regions and tenant_host_bindings in
# 000006, tenant_onboarding_keys in 000008 (read before a tenant is known —
# a retried onboarding does not know the tenant id it is asking about).
hb=$(psql_q "SELECT relrowsecurity FROM pg_class WHERE relname='tenant_host_bindings';")
ok "tenant_host_bindings is deliberately exempt (read before a tenant is known)" \
   "$([ "$hb" = "f" ] && echo 0 || echo 1)"
ok_keys=$(psql_q "SELECT relrowsecurity FROM pg_class WHERE relname='tenant_onboarding_keys';")
ok "tenant_onboarding_keys is deliberately exempt (000008: read before a tenant is known)" \
   "$([ "$ok_keys" = "f" ] && echo 0 || echo 1)"

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
section '11. Schema — migrations 000007–000010 (the 24 and 28 Sep gap closures)'
# ---------------------------------------------------------------------------

for t in approval_requests tenant_onboarding_keys entity_merge_records idempotency_keys; do
  n=$(psql_q "SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename='$t';")
  ok "table $t exists" "$([ "$n" = "1" ] && echo 0 || echo 1)"
done
for t in approval_requests entity_merge_records idempotency_keys; do
  f=$(psql_q "SELECT relforcerowsecurity FROM pg_class WHERE relname='$t';")
  ok "$t carries FORCE row-level security" "$([ "$f" = "t" ] && echo 0 || echo 1)"
done

# Maker-checker in the database, not only the service: the decider can never
# be the requester, whatever calls the store.
sod=$(psql_q "SELECT count(*) FROM pg_constraint WHERE conname='ar_no_self_decision';")
ok "constraint ar_no_self_decision is present" "$([ "$sod" = "1" ] && echo 0 || echo 1)"

cols=$(psql_q "SELECT count(*) FROM information_schema.columns WHERE
  (table_name='legal_entity_profile_versions' AND column_name IN ('lei','lei_source','lei_status'))
  OR (table_name='tenants' AND column_name IN ('external_customer_key','onboarding_request_ref','provisioning_failure_reason'))
  OR (table_name='legal_entities' AND column_name IN ('merged_into_legal_entity_id','verified_by_principal_id'))
  OR (table_name='tenant_lifecycle_history' AND column_name IN ('onboarding_request_ref','approval_request_id'));")
ok "LEI, onboarding, provisioning-failure, merge, verification and approval-chain columns exist (found $cols of 10)" \
   "$([ "$cols" = "10" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '12. Maker-checker, Draft/Verified/Active, merge — routed'
# ---------------------------------------------------------------------------

E0=00000000-0000-0000-0000-000000000001
probe_route 'GET  /v1/approval-requests' GET "/v1/approval-requests"
probe_route 'POST /v1/approval-requests/{id}/approve' POST "/v1/approval-requests/$E0/approve" '{}'
probe_route 'POST /v1/approval-requests/{id}/reject' POST "/v1/approval-requests/$E0/reject" '{}'
probe_route 'POST /v1/entities/{id}/verification' POST "/v1/entities/$E0/verification" '{}'
probe_route 'POST /v1/entities/{id}/activation' POST "/v1/entities/$E0/activation" '{}'
probe_route 'POST /v1/entities/{id}/merge' POST "/v1/entities/$E0/merge" '{}'
probe_route 'POST /v1/entities/{id}/unmerge' POST "/v1/entities/$E0/unmerge" '{}'
probe_route 'GET  /v1/entities/{id}/merge-records' GET "/v1/entities/$E0/merge-records"

# ---------------------------------------------------------------------------
section '13. §8 NP6 / §4.3 — profile history is coherent'
# ---------------------------------------------------------------------------

# Two open-ended versions starting at DIFFERENT instants mean as-of "now" and
# GetEntity can disagree about the entity's name. A same-instant correction
# (superseded in record time, same start) is legitimate and not counted.
# Before 28 Sep 2026 an amendment effective before an entity's first version
# produced exactly this.
overlap=$(psql_q "SELECT count(*) FROM (
  SELECT legal_entity_id FROM legal_entity_profile_versions
   WHERE effective_to IS NULL
   GROUP BY legal_entity_id HAVING count(DISTINCT effective_from) > 1) x;")
ok "no entity has two open-ended profile versions (entities affected: ${overlap:-?})" \
   "$([ "$overlap" = "0" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '14. Replay protection and NP3 on every route (live, no grants needed)'
# ---------------------------------------------------------------------------

# A refused write is still a terminal answer and is recorded, so replay is
# provable without any authorization grant: the second send with the same key
# must come back marked as a replay rather than evaluated again.
AUD_TENANT="${AUDIT_TENANT_ID:-11111111-1111-1111-1111-111111111111}"
AUD_KEY="audit-$(date +%s)-$RANDOM"
replay_hdr() {
  curl -s -D - -o /dev/null -X POST "$BASE_URL/v1/tenants/$AUD_TENANT/defaults" \
    -H 'Content-Type: application/json' -H "X-Tenant-Id: $AUD_TENANT" \
    -H 'X-Principal-Id: audit-probe' -H "X-Request-Id: $AUD_KEY-$1" \
    -H 'X-Correlation-ID: audit' -H 'X-Source-Channel: api' \
    -H "Idempotency-Key: $AUD_KEY" -d '{"primary_locale":"en-GB","reason":"audit replay probe"}' \
    | tr -d '\r' | grep -i '^x-idempotent-replay:' | awk '{print $2}'
}
first=$(replay_hdr 1); second=$(replay_hdr 2)
ok "a repeated Idempotency-Key is answered from the record (first: ${first:-none}, second: ${second:-none})" \
   "$([ -z "$first" ] && [ "$second" = "true" ] && echo 0 || echo 1)"

# NP3 beyond the command routes: a request on a bound host claiming a
# different tenant is refused on a plain read. Needs one bound host.
bound=$(psql_q "SELECT hostname||'|'||tenant_id FROM tenant_host_bindings LIMIT 1;")
if [ -z "$bound" ]; then
  skip "NP3 on GET /v1/tenants/{id} — no host binding exists to probe with"
else
  host="${bound%%|*}"; host_tenant="${bound##*|}"
  other=$(psql_q "SELECT tenant_id FROM tenants WHERE tenant_id <> '$host_tenant' LIMIT 1;")
  if [ -z "$other" ]; then
    skip "NP3 on GET /v1/tenants/{id} — needs a second tenant"
  else
    np3=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/v1/tenants/$other" \
      -H "Host: $host" -H "X-Tenant-Id: $other" -H 'X-Principal-Id: audit-probe' \
      -H 'X-Request-Id: audit-np3' -H 'X-Correlation-ID: audit' -H 'X-Source-Channel: api')
    ok "NP3: host bound to one tenant, request claiming another, plain read → 403 (got $np3)" \
       "$([ "$np3" = "403" ] && echo 0 || echo 1)"
  fi
fi

# ---------------------------------------------------------------------------
section '15. 28 Sep gap closure — typed errors, §7 events, home region, inputs'
# ---------------------------------------------------------------------------

# ORG §3 stable typed errors: a refusal carries error_code, not only prose.
ec=$(curl -s -X POST "$BASE_URL/v1/tenants/$AUD_TENANT/defaults" \
  -H 'Content-Type: application/json' -H "X-Tenant-Id: $AUD_TENANT" \
  -H 'X-Principal-Id: audit-probe' -H "X-Request-Id: audit-ec-$RANDOM" \
  -H 'X-Correlation-ID: audit' -H 'X-Source-Channel: api' \
  -H "Idempotency-Key: audit-ec-$(date +%s)-$RANDOM" -d '{"primary_locale":"en-GB","reason":"audit"}' \
  | grep -o '"error_code":"[A-Z_]*"' | cut -d'"' -f4)
ok "a refused write carries a stable error_code (got ${ec:-none})" "$([ -n "$ec" ] && echo 0 || echo 1)"
env_ec=$(curl -s -X POST "$BASE_URL/v1/tenants" -H 'Content-Type: application/json' -d '{}' \
  | grep -o '"error_code":"[A-Z_]*"' | cut -d'"' -f4)
ok "an envelope refusal is CONTEXT_INVALID (got ${env_ec:-none})" "$([ "$env_ec" = "CONTEXT_INVALID" ] && echo 0 || echo 1)"

# ORG §7: every event written since versioned envelopes shipped carries
# object_id / object_version / effective_at / recorded_at.
unversioned=$(psql_q "SELECT count(*) FROM event_outbox
  WHERE created_at >= (SELECT coalesce(min(created_at), now()) FROM event_outbox WHERE payload ? 'object_version')
    AND NOT (payload ? 'object_id' AND payload ? 'object_version' AND payload ? 'effective_at' AND payload ? 'recorded_at');")
ok "every event since the §7 envelope carries object id, version, effective and recorded time (missing: ${unversioned:-?})" \
   "$([ "$unversioned" = "0" ] && echo 0 || echo 1)"

# §9.2 gate 2: no event path bypasses the outbox — the direct publisher is gone,
# so nothing in the log may say a direct publish failed.
direct=$(docker logs tenant-entity-registry-svc 2>&1 | grep -c '"event publish failed"')
ok "no direct Kafka publish attempts in the service log (found: ${direct:-?})" "$([ "$direct" = "0" ] && echo 0 || echo 1)"

# Event delivery is observable.
m=$(curl -s "$BASE_URL/metrics" | grep -cE '^outbox_(pending|dead_letter)_events')
ok "outbox delivery gauges are exported (found $m of 2)" "$([ "$m" = "2" ] && echo 0 || echo 1)"

# §4.2 ChangeHomeRegion (000012) and provisioning inputs (000013).
probe_route 'POST /v1/tenants/{id}/home-region' POST "/v1/tenants/$E0/home-region" '{}'
hr=$(psql_q "SELECT count(*) FROM pg_constraint WHERE conname='tlh_home_region_evidenced';")
ok "constraint tlh_home_region_evidenced is present (no home-region change without evidence)" "$([ "$hr" = "1" ] && echo 0 || echo 1)"
pc=$(psql_q "SELECT count(*) FROM information_schema.columns WHERE
  (table_name='tenants' AND column_name IN ('primary_jurisdiction_id','subscription_id','home_region_decision_ref'))
  OR (table_name IN ('workspaces','entity_hierarchies','entity_jurisdiction_assignments') AND column_name='record_version');")
ok "provisioning-lineage, home-region and record_version columns exist (found $pc of 6)" "$([ "$pc" = "6" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
section '16. 29 Sep re-audit — malformed ids, §4.3 source inputs at creation'
# ---------------------------------------------------------------------------

# A malformed id is the caller's error. Every id route answered 500
# INTERNAL_ERROR to one until 29 Sep, which read as an outage in the SLO.
mid=$(curl -s -w ' %{http_code}' "$BASE_URL/v1/entities/not-a-uuid" \
  -H "X-Tenant-Id: $AUD_TENANT" -H 'X-Principal-Id: audit-probe' -H "X-Request-Id: audit-mid-$RANDOM" \
  -H 'X-Correlation-ID: audit' -H 'X-Source-Channel: api')
ok "a malformed id is a typed 400, not a 500 (got ${mid##* } $(echo "$mid" | grep -o '"error_code":"[A-Z_]*"' | cut -d'"' -f4))" \
   "$(echo "$mid" | grep -q '"error_code":"VALIDATION_FAILED"' && [ "${mid##* }" = "400" ] && echo 0 || echo 1)"

# §4.3 required inputs: the contract requires the registered address and the
# supporting evidence at creation, and documents the legal-form fields.
ce=$(awk '/^    CreateEntityRequest:/{f=1} f&&/^    UpdateEntityRequest:/{exit} f' openapi.yaml)
n=$(echo "$ce" | grep -cE '^ +(legal_form_code|legal_form_source|legal_form_local_text|registered_office|source_evidence_ref):')
req=$(echo "$ce" | tr -d '\n ' | grep -c 'registered_office,source_evidence_ref\]')
ok "CreateEntityRequest carries the §4.3 inputs and requires office + evidence (fields $n of 5, required $req of 1)" \
   "$([ "$n" = "5" ] && [ "$req" = "1" ] && echo 0 || echo 1)"

# ---------------------------------------------------------------------------
printf '\n\033[1mResult:\033[0m %s passed, %s failed, %s skipped\n' \
  "$(green "$PASS")" "$([ "$FAIL" -gt 0 ] && red "$FAIL" || echo "$FAIL")" "$SKIP"

[ "$FAIL" -eq 0 ] || exit 1
