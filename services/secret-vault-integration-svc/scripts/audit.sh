#!/usr/bin/env bash
# Audit test — secret-vault-integration-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# running stack rather than against stubs. Every check below either exercises
# a documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8087), zoiko-postgres, a Go toolchain,
# and the CONSOLE_DEMO_OPERATOR grants from
# deployments/scripts/seed-demo-rbac.ps1 (this service's six SECRET_* actions
# live in its VAULT_FULL bundle — without them every mutation below is a
# correct 403 and this script reports a service that is working as designed as
# if it were broken).
#
# Exits non-zero if anything failed, so CI can gate on it.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8087}
PG=${PG:-zoiko-postgres}
DB=${DB:-secret_vault_integration}

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=22222222-2222-2222-2222-222222222222
PRIN=33333333-3333-3333-3333-333333333333
WL=77777777-7777-7777-7777-777777777777
INTRUDER=99999999-9999-9999-9999-999999999999

STAMP=$(date +%s)
SPATH="zoiko/audit/$STAMP/db-password"

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }

# The canonical service input contract (ZS-ARCH-SVC-001 §4). Every one of these
# headers is mandatory here: the envelope middleware refuses the request before
# it reaches a handler otherwise, which is itself asserted in section 7.
H=(-H "Content-Type: application/json"
   -H "X-Tenant-Id: $TEN"
   -H "X-Principal-Id: $PRIN"
   -H "X-Source-Channel: api"
   -H "X-Purpose-Context: OPERATIONS")

# A brokering workload authenticates as a workload, not a human subject.
WH=(-H "Content-Type: application/json"
    -H "X-Tenant-Id: $TEN"
    -H "X-Workload-Id: $WL"
    -H "X-Source-Channel: api"
    -H "X-Purpose-Context: OPERATIONS")

json() { python -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }

# post/get take a request id and use it for all three trace headers, so a
# failure in this script is greppable in the service log by one value.
post() { local p=$1 body=$2 id=$3; shift 3
  curl -s -m 25 -X POST "$B$p" "$@" -H "X-Request-Id: $id" -H "Idempotency-Key: $id" -H "X-Correlation-ID: $id" -d "$body"; }
postcode() { local p=$1 body=$2 id=$3; shift 3
  curl -s -m 25 -o /dev/null -w '%{http_code}' -X POST "$B$p" "$@" -H "X-Request-Id: $id" -H "Idempotency-Key: $id" -H "X-Correlation-ID: $id" -d "$body"; }
get() { local p=$1 id=$2; shift 2
  curl -s -m 25 "$B$p" "$@" -H "X-Request-Id: $id" -H "X-Correlation-ID: $id"; }
getcode() { local p=$1 id=$2; shift 2
  curl -s -m 25 -o /dev/null -w '%{http_code}' "$B$p" "$@" -H "X-Request-Id: $id" -H "X-Correlation-ID: $id"; }

echo "==================================================================="
echo " AUDIT — secret-vault-integration-svc"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo
echo "-- 1. Static analysis ----------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi

echo
echo "-- 2. Test suite ---------------------------------------------------"
# Retried because Windows Application Control intermittently blocks a freshly
# linked Go test binary — a scan race, not a test failure.
for _ in 1 2 3 4 5; do
  OUT=$(go test -count=1 ./... 2>&1)
  echo "$OUT" | grep -q "Application Control" && continue
  break
done
chk "packages failing" "$(echo "$OUT" | grep -cE '^FAIL')" "0"
echo "        packages passing: $(echo "$OUT" | grep -cE '^ok ')"

echo
echo "-- 3. Store integration vs real Postgres 16 ------------------------"
# A scratch database, never the live one: the last test in the store suite
# drops all four tables on purpose to prove the error path, with no teardown.
# Pointing it at $DB leaves the running service with no schema.
docker exec "$PG" psql -U postgres -c "DROP DATABASE IF EXISTS svi_audit_scratch;" >/dev/null 2>&1
docker exec "$PG" psql -U postgres -c "CREATE DATABASE svi_audit_scratch;" >/dev/null 2>&1
SOUT=$(TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/svi_audit_scratch?sslmode=disable" go test -count=1 -v ./internal/store/ 2>&1)
chk "store tests failing" "$(echo "$SOUT" | grep -cE '^--- FAIL')" "0"
chk "store tests skipped" "$(echo "$SOUT" | grep -cE '^--- SKIP')" "0"
echo "        store tests passing: $(echo "$SOUT" | grep -cE '^--- PASS')"

echo
echo "-- 4. Silent-skip guard --------------------------------------------"
# The store suite is TEST_DATABASE_URL-gated. Without this guard an unset
# variable skips every test in it while `go test ./...` still prints ok — a
# verification that verified nothing.
G=$(REQUIRE_DB_TESTS=1 go test -count=1 ./internal/store/ 2>&1 | grep -c "claims to verify")
if [ "$G" -gt 0 ]; then ok "store suite fails loudly when REQUIRE_DB_TESTS is set"; else bad "silent-skip guard"; fi

echo
echo "-- 5. Live health ---------------------------------------------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz"  "$(code $B/readyz)"  "200"
# The image's own HEALTHCHECK runs the compiled healthcheck binary against
# /readyz, so a dead pool cannot read as healthy.
DH=$(docker inspect --format '{{.State.Health.Status}}' secret-vault-integration-svc 2>/dev/null)
chk "container HEALTHCHECK" "$DH" "healthy"

echo
echo "-- 6. Policy administration -----------------------------------------"
R=$(post /v1/secret-policies \
  "{\"secret_class\":\"INTEGRATION_TOKEN\",\"secret_path\":\"$SPATH\",\"created_by_principal_id\":\"$PRIN\"}" \
  "au-$STAMP-1" "${H[@]}")
PID=$(echo "$R" | json secret_policy_id)
if [ -n "$PID" ]; then ok "CreateSecretPolicy         POST /v1/secret-policies"; else bad "CreateSecretPolicy: $R"; fi

# Idempotent on secret_path: a replay returns the same row, not a second one.
PID2=$(post /v1/secret-policies \
  "{\"secret_class\":\"INTEGRATION_TOKEN\",\"secret_path\":\"$SPATH\",\"created_by_principal_id\":\"$PRIN\"}" \
  "au-$STAMP-1b" "${H[@]}" | json secret_policy_id)
chk "  ... idempotent on secret_path" "$PID2" "$PID"

# Same path, different class is a genuine conflict, not a replay.
chk "  ... 409 on same path, different class" \
  "$(postcode /v1/secret-policies "{\"secret_class\":\"DATABASE_CREDENTIAL\",\"secret_path\":\"$SPATH\",\"created_by_principal_id\":\"$PRIN\"}" "au-$STAMP-1c" "${H[@]}")" "409"

R=$(post "/v1/secret-policies/$PID/versions" \
  "{\"tenant_id\":\"$TEN\",\"allowed_workload_ids\":[\"$WL\"],\"max_lease_duration_seconds\":300,\"effective_from\":\"2026-01-01T00:00:00Z\",\"created_by_principal_id\":\"$PRIN\"}" \
  "au-$STAMP-2" "${H[@]}")
VID=$(echo "$R" | json secret_policy_version_id)
if [ -n "$VID" ]; then ok "CreateSecretPolicyVersion  POST .../versions (DRAFT)"; else bad "CreateSecretPolicyVersion: $R"; fi

# A DRAFT version must be invisible to the broker — activation is what
# publishes it. Asserted before activating, so the order proves it.
chk "  ... DRAFT is not brokerable yet" \
  "$(postcode /v1/secrets/broker "{\"secret_path\":\"$SPATH\",\"requested_by_principal_id\":\"$WL\",\"tenant_id\":\"$TEN\",\"request_id\":\"au-$STAMP-draft\"}" "au-$STAMP-draft" "${WH[@]}")" "404"

AR=$(post "/v1/secret-policies/$PID/versions/$VID/activate" "{\"activated_by_principal_id\":\"$PRIN\"}" "au-$STAMP-3" "${H[@]}")
chk "ActivateVersion            POST .../activate" "$(echo "$AR" | json version_status)" "ACTIVE"
chk "  ... reports it transitioned" "$(echo "$AR" | json transitioned)" "True"
# A repeat is a no-op, and must say so rather than looking like a second write.
chk "  ... replay reports no transition" \
  "$(post "/v1/secret-policies/$PID/versions/$VID/activate" "{\"activated_by_principal_id\":\"$PRIN\"}" "au-$STAMP-3b" "${H[@]}" | json transitioned)" "False"

chk "PutSecretMaterial          POST .../material" \
  "$(postcode "/v1/secret-policies/$PID/material" "{\"material_base64\":\"$(printf 'audit-secret-%s' "$STAMP" | base64)\",\"written_by_principal_id\":\"$PRIN\"}" "au-$STAMP-4" "${H[@]}")" "200"

chk "ListApplicable             GET  /v1/secret-policies?secret_class=" \
  "$(getcode "/v1/secret-policies?secret_class=INTEGRATION_TOKEN&tenant_id=$TEN" "au-$STAMP-5" "${H[@]}")" "200"
chk "  ... 400 without secret_class" \
  "$(getcode "/v1/secret-policies?tenant_id=$TEN" "au-$STAMP-5b" "${H[@]}")" "400"
chk "ListVersionHistory         GET  .../versions" \
  "$(getcode "/v1/secret-policies/$PID/versions" "au-$STAMP-6" "${H[@]}")" "200"

echo
echo "-- 7. Brokering (the core value) ------------------------------------"
BR=$(post /v1/secrets/broker \
  "{\"secret_path\":\"$SPATH\",\"requested_by_principal_id\":\"$WL\",\"tenant_id\":\"$TEN\",\"request_id\":\"au-$STAMP-7\"}" \
  "au-$STAMP-7" "${WH[@]}")
LID=$(echo "$BR" | json lease_id)
TOK=$(echo "$BR" | json lease_token)
if [ -n "$LID" ] && [ -n "$TOK" ]; then ok "Broker grant              POST /v1/secrets/broker"; else bad "Broker grant: $BR"; fi

# Idempotent on request_id: the durable lease is reused, the token re-minted.
LID2=$(post /v1/secrets/broker \
  "{\"secret_path\":\"$SPATH\",\"requested_by_principal_id\":\"$WL\",\"tenant_id\":\"$TEN\",\"request_id\":\"au-$STAMP-7\"}" \
  "au-$STAMP-7b" "${WH[@]}" | json lease_id)
chk "  ... idempotent on request_id (same lease)" "$LID2" "$LID"

chk "  ... 403 for an unauthorized workload" \
  "$(postcode /v1/secrets/broker "{\"secret_path\":\"$SPATH\",\"requested_by_principal_id\":\"$INTRUDER\",\"tenant_id\":\"$TEN\",\"request_id\":\"au-$STAMP-8\"}" "au-$STAMP-8" \
     -H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Workload-Id: $INTRUDER" -H "X-Source-Channel: api" -H "X-Purpose-Context: OPERATIONS")" "403"
chk "  ... 404 for an unregistered secret_path" \
  "$(postcode /v1/secrets/broker "{\"secret_path\":\"zoiko/nope/$STAMP\",\"requested_by_principal_id\":\"$WL\",\"tenant_id\":\"$TEN\",\"request_id\":\"au-$STAMP-9\"}" "au-$STAMP-9" "${WH[@]}")" "404"

chk "GetLease                   GET  /v1/secrets/leases/{id}" "$(getcode "/v1/secrets/leases/$LID" "au-$STAMP-10" "${H[@]}")" "200"
chk "ListLeases                 GET  /v1/secrets/leases" "$(getcode "/v1/secrets/leases?tenant_id=$TEN&limit=5" "au-$STAMP-11" "${H[@]}")" "200"

echo
echo "-- 8. Tenant isolation ----------------------------------------------"
# An id-addressed route must answer 404, not 403: 403 confirms the row exists,
# which is enough to enumerate another tenant's leases one guess at a time.
chk "another tenant's lease reads as 404" \
  "$(getcode "/v1/secrets/leases/$LID" "au-$STAMP-12" -H "X-Tenant-Id: $OTHER_TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api")" "404"
# Refused, and -- the part that matters -- NOT revoked. The status code is not
# pinned to one value here because two different correct refusals are reachable
# depending on how the caller is provisioned: 403 when the principal holds no
# role in the target tenant at all (the demo seed's case), 404 when it holds one
# but the lease belongs to someone else. Asserting either alone would fail a
# correct service on the other. What must never happen is a 2xx.
XREV=$(postcode "/v1/secrets/leases/$LID/revoke" "{\"revoked_by_principal_id\":\"$PRIN\"}" "au-$STAMP-13" \
  -H "Content-Type: application/json" -H "X-Tenant-Id: $OTHER_TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api" -H "X-Purpose-Context: OPERATIONS")
case "$XREV" in
  2*) bad "another tenant revoked the lease ($XREV)" ;;
  *)  ok  "another tenant cannot revoke it ($XREV)" ;;
esac
STILL=$(get "/v1/secrets/leases/$LID" "au-$STAMP-14" "${H[@]}" | json status)
chk "  ... and the lease is still live" "$STILL" "GRANTED"

# The enumeration property, stated directly: a refusal must not reveal whether
# the lease exists. A caller probing id by id learns nothing as long as a real
# lease it cannot reach and an id that was never issued answer identically.
GHOST=$(postcode "/v1/secrets/leases/44444444-0000-4000-8000-00000000dead/revoke" "{\"revoked_by_principal_id\":\"$PRIN\"}" "au-$STAMP-13b" \
  -H "Content-Type: application/json" -H "X-Tenant-Id: $OTHER_TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api" -H "X-Purpose-Context: OPERATIONS")
chk "  ... a real foreign lease is indistinguishable from a nonexistent one" "$XREV" "$GHOST"

# Cross-tenant authorization must not be cacheable. The decision cache was
# keyed without the tenant, so an answer taken for one tenant was served to
# whichever tenant asked next -- a principal authorized in its own tenant got a
# window of another tenant's permissions. Asked back to back, inside the cache
# TTL, on purpose: that is the only window in which the defect is visible.
SELF=$(postcode "/v1/secrets/leases/44444444-0000-4000-8000-00000000dead/revoke" "{\"revoked_by_principal_id\":\"$PRIN\"}" "au-$STAMP-13c" "${H[@]}")
chk "in-tenant revoke is authorized (primes the decision cache)" "$SELF" "404"
XCACHE=$(postcode "/v1/secrets/leases/44444444-0000-4000-8000-00000000dead/revoke" "{\"revoked_by_principal_id\":\"$PRIN\"}" "au-$STAMP-13d" \
  -H "Content-Type: application/json" -H "X-Tenant-Id: $OTHER_TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api" -H "X-Purpose-Context: OPERATIONS")
case "$XCACHE" in
  2*|404) bad "the in-tenant authorization decision leaked to another tenant ($XCACHE)" ;;
  *)      ok  "  ... and that decision does not leak to the next tenant ($XCACHE)" ;;
esac

echo
echo "-- 9. Malformed path parameters (regression) -------------------------"
# Every id here maps to a UUID column. A non-UUID used to reach the driver,
# whose generic error every handler translated to 503 store_unavailable — so a
# caller's own bad id read as an outage, and the client would retry, back off
# and eventually page someone. 400, and it must name the offending parameter.
for spec in \
  "GET:/v1/secrets/leases/not-a-uuid:lease_id" \
  "GET:/v1/secret-policies/not-a-uuid/versions:secret_policy_id"; do
  M=${spec%%:*}; rest=${spec#*:}; P=${rest%:*}; F=${rest##*:}
  chk "$M $P" "$(getcode "$P" "au-$STAMP-badid" "${H[@]}")" "400"
  FIELD=$(get "$P" "au-$STAMP-badid2" "${H[@]}" | json field)
  chk "  ... names the parameter" "$FIELD" "$F"
done
chk "POST .../{bad}/rotate" \
  "$(postcode "/v1/secret-policies/not-a-uuid/rotate" "{\"request_id\":\"au-$STAMP-badrot\",\"rotated_by_principal_id\":\"$PRIN\"}" "au-$STAMP-badrot" "${H[@]}")" "400"
# The other half: a well-formed id that matches no row is still 404, because
# it could have existed and the caller addressed it correctly.
chk "well-formed unknown lease id is still 404" \
  "$(getcode "/v1/secrets/leases/44444444-0000-4000-8000-00000000dead" "au-$STAMP-15" "${H[@]}")" "404"

echo
echo "-- 10. Rotation ------------------------------------------------------"
RR=$(post "/v1/secret-policies/$PID/rotate" "{\"request_id\":\"au-$STAMP-rot\",\"rotated_by_principal_id\":\"$PRIN\"}" "au-$STAMP-16" "${H[@]}")
chk "Rotate                     POST .../rotate" "$(echo "$RR" | json revoked_lease_count)" "1"
chk "  ... the granted lease is now REVOKED" "$(get "/v1/secrets/leases/$LID" "au-$STAMP-17" "${H[@]}" | json status)" "REVOKED"
# The replay must not rotate again, and must report the ORIGINAL count: 0 here
# reads as "this rotation revoked nothing", the opposite of what happened.
RP=$(post "/v1/secret-policies/$PID/rotate" "{\"request_id\":\"au-$STAMP-rot\",\"rotated_by_principal_id\":\"$PRIN\"}" "au-$STAMP-18" "${H[@]}")
chk "  ... replay is idempotent and reports the real count" "$(echo "$RP" | json revoked_lease_count)" "1"

echo
echo "-- 11. Audit evidence ------------------------------------------------"
AUD=$(get "/v1/secrets/audit?secret_path=$SPATH&limit=50" "au-$STAMP-19" "${H[@]}")
for ev in REQUESTED GRANTED DENIED REVOKED ROTATED; do
  N=$(echo "$AUD" | python -c "import sys,json;print(sum(1 for x in json.load(sys.stdin) if x.get('event_type')=='$ev'))" 2>/dev/null)
  if [ "${N:-0}" -ge 1 ]; then ok "audit surfaces $ev ($N)"; else bad "audit surfaces $ev (0)"; fi
done
# The regression this section exists for: ROTATED rows were written with
# tenant_id = NULL while ListAuditLog always filters on the caller's tenant,
# so a rotation appeared in nobody's audit trail — including the tenant that
# performed it, whose every lease it had just killed.
ROT_TEN=$(echo "$AUD" | python -c "
import sys,json
print(next((x.get('tenant_id') or '' for x in json.load(sys.stdin) if x.get('event_type')=='ROTATED'), ''))" 2>/dev/null)
chk "ROTATED entry is bound to the rotating tenant" "$ROT_TEN" "$TEN"
# And it must not leak to a tenant with no involvement: secret paths are
# platform-wide addresses, so a globally-readable rotation row would let any
# tenant enumerate everyone else's.
LEAK=$(get "/v1/secrets/audit?secret_path=$SPATH&event_type=ROTATED&limit=50" "au-$STAMP-20" \
  -H "X-Tenant-Id: $OTHER_TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api" \
  | python -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null)
chk "  ... and invisible to an unrelated tenant" "$LEAK" "0"

echo
echo "-- 12. Canonical input contract (ZS-ARCH-SVC-001 §4) ----------------"
# 401, not 400: a missing tenant or actor means the request never passed
# gateway verification, so telling the caller to fix its payload is wrong.
chk "no envelope headers at all" \
  "$(postcode /v1/secret-policies '{}' "au-$STAMP-21" -H "Content-Type: application/json")" "401"
# purpose_context is this service's conditional §4 field — every write here is
# governed sensitive access, so the reason for it is captured before material
# moves.
chk "write without X-Purpose-Context" \
  "$(postcode /v1/secret-policies "{\"secret_class\":\"INTEGRATION_TOKEN\",\"secret_path\":\"$SPATH-np\",\"created_by_principal_id\":\"$PRIN\"}" "au-$STAMP-22" \
     -H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" -H "X-Source-Channel: api")" "400"
chk "write without Idempotency-Key" \
  "$(curl -s -m 25 -o /dev/null -w '%{http_code}' -X POST "$B/v1/secret-policies" \
     -H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" \
     -H "X-Source-Channel: api" -H "X-Purpose-Context: OPERATIONS" \
     -H "X-Request-Id: au-$STAMP-23" -H "X-Correlation-ID: au-$STAMP-23" \
     -d "{\"secret_class\":\"INTEGRATION_TOKEN\",\"secret_path\":\"$SPATH-ni\",\"created_by_principal_id\":\"$PRIN\"}")" "400"
chk "health probes stay exempt" "$(code $B/healthz)" "200"

echo
echo "-- 13. Secrets are never stored here ---------------------------------"
# The whole doctrine of this service in one assertion: it holds policy, lease
# and audit metadata, and the material lives behind the vault backend. A
# plaintext secret appearing in any of its four tables is a design failure,
# not a bug to be patched.
HITS=$(docker exec "$PG" psql -U postgres -d "$DB" -tAc "
  SELECT count(*) FROM secret_access_audit_log WHERE outcome_detail LIKE '%audit-secret-%'
     OR secret_path LIKE '%audit-secret-%';" 2>/dev/null | tr -d '[:space:]')
chk "no secret material in the audit table" "${HITS:-1}" "0"
LEASE_HITS=$(docker exec "$PG" psql -U postgres -d "$DB" -tAc "
  SELECT count(*) FROM secret_leases WHERE request_id LIKE '%audit-secret-%';" 2>/dev/null | tr -d '[:space:]')
chk "no secret material in the lease table" "${LEASE_HITS:-1}" "0"
# The lease token is a handle, not the secret. If the material ever leaked
# into it, this is where it would show.
case "$TOK" in
  local-lease:*) ok "lease_token is an opaque handle, not the material" ;;
  *)             bad "lease_token has an unexpected shape: $TOK" ;;
esac

echo
echo "-- 14. Telemetry -----------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
DOMAIN=$(echo "$M" | grep -c '^secret_vault_')
if [ "$DOMAIN" -ge 20 ]; then ok "domain metric series emitted ($DOMAIN)"; else bad "domain metrics ($DOMAIN, want >= 20)"; fi
for fam in secret_vault_broker_decisions_total secret_vault_lease_revocations_total \
           secret_vault_rotations_total secret_vault_authz_decisions_total \
           secret_vault_backend_errors_total; do
  if echo "$M" | grep -q "^$fam"; then ok "  $fam"; else bad "  $fam missing"; fi
done
# The counters must reflect what this run actually did, not merely exist.
GRANTS=$(echo "$M" | grep '^secret_vault_broker_decisions_total{.*outcome="granted"' | awk '{print $2}')
if [ "${GRANTS%%.*}" -ge 1 ] 2>/dev/null; then ok "  broker grants counted ($GRANTS)"; else bad "  broker grants not counted ($GRANTS)"; fi
DENIED=$(echo "$M" | grep '^secret_vault_broker_decisions_total{.*outcome="denied"' | awk '{print $2}')
if [ "${DENIED%%.*}" -ge 1 ] 2>/dev/null; then ok "  broker denials counted ($DENIED)"; else bad "  broker denials not counted ($DENIED)"; fi
echo "$M" | grep -q '^readiness_up' && ok "  readiness_up gauge present" || bad "  readiness_up missing"

echo
echo "-- 15. Alert rules ---------------------------------------------------"
RULES=$(curl -s -m 20 "http://localhost:9090/api/v1/rules" 2>/dev/null)
if [ -n "$RULES" ]; then
  NR=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
print(sum(len(g['rules']) for g in d['data']['groups'] if 'secret-vault' in g['name']))" 2>/dev/null)
  NH=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
print(sum(1 for g in d['data']['groups'] if 'secret-vault' in g['name'] for r in g['rules'] if r.get('health')=='ok'))" 2>/dev/null)
  chk "secret-vault alert rules loaded" "$NR" "6"
  chk "  ... all evaluating (health ok)" "$NH" "6"
else
  echo "  SKIP  prometheus not reachable on :9090 (rules not asserted)"
fi

docker exec "$PG" psql -U postgres -c "DROP DATABASE IF EXISTS svi_audit_scratch;" >/dev/null 2>&1

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
