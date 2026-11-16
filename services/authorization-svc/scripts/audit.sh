#!/usr/bin/env bash
# Audit — authorization-svc (IAM / AuthZ), against the code and a live local stack.
#
#   B=http://127.0.0.1:8089 bash services/authorization-svc/scripts/audit.sh
#
# Needs: go on PATH (build/vet), Docker (the test suites run in a throwaway
# golang container because Windows Application Control blocks host test
# binaries; promtool runs from prom/prometheus), the compose stack with
# authorization-svc up (deployments/scripts/e2e-authorization-scenarios.py
# --seed-only run once), and zoiko-postgres for the database checks.
# Set SKIP_SUITES=1 to skip sections 2-3 (the slow ones).
set -u
SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
ROOT="$(cd "$SVC/../.." && pwd)"
B=${B:-http://127.0.0.1:8089}
TEN=11111111-1111-1111-1111-111111111111
ENT=22222222-2222-2222-2222-222222222222
ADMIN=33333333-3333-3333-3333-333333333333
CHECKER=a1000000-0000-4000-8000-000000000001
PY=${PY:-python}
MODCACHE=${MODCACHE:-$(go env GOMODCACHE 2>/dev/null)}
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hdr() { # principal [write]
  local id; id=$(date +%s%N)$RANDOM
  H=(-H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $1" -H "X-Legal-Entity-Id: $ENT"
     -H "X-Correlation-ID: audit-$id" -H "X-Request-Id: audit-$id" -H "X-Source-Channel: api")
  [ "${2:-}" = w ] && H+=(-H "Idempotency-Key: audit-$id")
  return 0
}
code() { curl -s -m 20 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 20 "$@"; }
gotest() { # args...: run go test in a throwaway container
  MSYS_NO_PATHCONV=1 docker run --rm --memory=1400m -e GOFLAGS=-mod=mod -e GOPROXY=off -e CGO_ENABLED=0 \
    -e "AUTHZ_IT_DSN=${AUTHZ_IT_DSN:-}" -e "TEST_DATABASE_URL=${TEST_DATABASE_URL:-}" -e "REQUIRE_DB_TESTS=${REQUIRE_DB_TESTS:-}" \
    -v "$(cd "$SVC/.." && pwd -W 2>/dev/null || cd "$SVC/.." && pwd):/src" -v "$MODCACHE:/go/pkg/mod" \
    -w /src/authorization-svc golang:1.26-alpine go test "$@"
}

echo "==================================================================="
echo " AUDIT — authorization-svc (IAM / AuthZ)   $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo; echo "-- 1. Static analysis --------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./..."; else bad "go build ./..."; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./..."; else bad "go vet ./..."; fi

if [ "${SKIP_SUITES:-}" != 1 ]; then
  echo; echo "-- 2. Unit suites (container) -------------------------------------"
  OUT=$(gotest -p 2 -count=1 ./... 2>&1)
  chk "packages failing" "$(echo "$OUT" | grep -cE '^(FAIL|--- FAIL)')" "0"
  echo "        packages passing: $(echo "$OUT" | grep -cE '^ok ')"

  echo; echo "-- 3. Store integration vs Postgres 16 (NOBYPASSRLS + owner) -------"
  export AUTHZ_IT_DSN=${AUTHZ_IT_DSN:-postgres://app_authorization:postgres@host.docker.internal:5432/authorization_it?sslmode=disable}
  export TEST_DATABASE_URL=${TEST_DATABASE_URL:-postgres://postgres:postgres@host.docker.internal:5432/authorization_test?sslmode=disable}
  export REQUIRE_DB_TESTS=1
  SOUT=$(gotest -count=1 -p 1 -v ./internal/store/ ./internal/idempotency/ 2>&1)
  chk "integration tests failing" "$(echo "$SOUT" | grep -cE -- '--- FAIL')" "0"
  echo "        passing: $(echo "$SOUT" | grep -cE -- '--- PASS')   skipped (by design): $(echo "$SOUT" | grep -cE -- '--- SKIP')"
fi

echo; echo "-- 4. Live service ------------------------------------------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz" "$(code $B/readyz)" "200"

echo; echo "-- 5. Contract surface (openapi.yaml: 86 paths, 101 operations) ----"
# Every documented GET without a path parameter answers 200 for the admin.
for p in /v1/admin/roles /v1/admin/role-assignments /v1/admin/delegated-authorities /v1/iam/delegations \
         /v1/admin/sod-rules /v1/admin/sod-exceptions /v1/admin/abac-rules /v1/admin/authority-limits \
         /v1/admin/workload-bindings /v1/admin/privileged-sessions /admin/v1/privileged-sessions \
         /v1/admin/break-glass-sessions /admin/v1/break-glass-sessions /v1/support/sessions \
         /v1/access-decisions "/v1/sod/conflicting-permissions?action_type=zt.payment.prepare" \
         "/v1/me/capabilities?legal_entity_id=$ENT"; do
  hdr $ADMIN; chk "GET $p" "$(code "${H[@]}" "$B$p")" "200"
done
hdr $ADMIN; chk "POST /v1/authorize answers" "$(code -X POST "${H[@]}" $B/v1/authorize \
  -d "{\"principal_id\":\"$ADMIN\",\"legal_entity_id\":\"$ENT\",\"action_type\":\"iam.role.read\",\"tenant_id\":\"$TEN\"}")" "200"

echo; echo "-- 6. Negative paths ----------------------------------------------"
hdr $ADMIN w
A=$(body "${H[@]}" "$B/v1/admin/role-assignments?principal_id=$CHECKER" | $PY -c "import sys,json;r=json.load(sys.stdin);print(r[0]['principal_role_assignment_id'] if r else '')" 2>/dev/null)
if [ -n "$A" ]; then
  hdr $ADMIN w; chk "assignment revoke without If-Match is 428 (AZ-1)" "$(code -X POST "${H[@]}" "$B/v1/admin/role-assignments/$A/revoke" -d '{"reason_code":"AUDIT"}')" "428"
  hdr $ADMIN w; chk "assignment revoke with a stale If-Match is 412" "$(code -X POST "${H[@]}" -H 'If-Match: "v99999"' "$B/v1/admin/role-assignments/$A/revoke" -d '{"reason_code":"AUDIT"}')" "412"
else bad "a checker assignment to probe (run e2e-authorization-scenarios.py --seed-only)"; fi
hdr $ADMIN w; chk "binding deactivate without If-Match is 428 (AZ-1)" "$(code -X POST "${H[@]}" "$B/v1/admin/workload-bindings/zt-invoice-batch-worker/deactivate" -d '{"reason_code":"AUDIT"}')" "428"
hdr $ADMIN w; chk "ABAC rule with an unknown operator is 400" "$(code -X POST "${H[@]}" $B/v1/admin/abac-rules \
  -d "{\"rule_code\":\"AUDIT_X\",\"action_type\":\"zt.x\",\"effect\":\"FORBID\",\"attribute_key\":\"k\",\"operator\":\"approximately\",\"attribute_value\":\"1\",\"tenant_id\":\"$TEN\",\"reason_code\":\"AUDIT\"}")" "400"
hdr $ADMIN; chk "cross-tenant entity on /v1/authorize is 403" "$(code -X POST "${H[@]}" $B/v1/authorize \
  -d "{\"principal_id\":\"$ADMIN\",\"legal_entity_id\":\"$(cat /proc/sys/kernel/random/uuid 2>/dev/null || $PY -c 'import uuid;print(uuid.uuid4())')\",\"action_type\":\"iam.role.read\",\"tenant_id\":\"$TEN\"}")" "403"
hdr $ADMIN; chk "forged paging cursor is 400 (API-4)" "$(code "${H[@]}" "$B/v1/admin/roles?cursor=forged")" "400"
KEY="audit-idem-$(date +%s%N)"; RB="{\"tenant_id\":\"$TEN\",\"role_code\":\"AUDIT_$(date +%s)\",\"role_name\":\"audit\",\"role_scope_type\":\"TENANT\",\"created_by_principal_id\":\"$ADMIN\"}"
hdr $ADMIN; R1=$(code -X POST "${H[@]}" -H "Idempotency-Key: $KEY" $B/v1/admin/roles -d "$RB")
hdr $ADMIN; R2=$(curl -s -m 20 -o /dev/null -D - -X POST "${H[@]}" -H "Idempotency-Key: $KEY" $B/v1/admin/roles -d "$RB" | grep -ci '^X-Idempotent-Replay: true')
hdr $ADMIN; R3=$(code -X POST "${H[@]}" -H "Idempotency-Key: $KEY" $B/v1/admin/roles -d "${RB/audit/different}")
chk "idempotent create, replay, mismatch" "$R1/$R2/$R3" "201/1/409"

echo; echo "-- 7. §26 telemetry ------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
for fam in authorization_decisions_total authorization_decisions_by_action_total authorization_evaluation_seconds_bucket \
           authorization_decision_record_failures_total authorization_cache_hits_total authorization_cache_misses_total \
           authorization_cache_invalidations_total authorization_cache_entries authorization_watermark_age_seconds \
           authorization_outbox_pending authorization_outbox_oldest_pending_seconds authorization_outbox_stuck \
           authorization_sessions authorization_sessions_expiring_soon authorization_projection_lag_seconds_count; do
  if echo "$M" | grep -q "^$fam"; then ok "$fam"; else bad "$fam missing"; fi
done

echo; echo "-- 8. Alert rules and dashboard -----------------------------------"
R=$(MSYS_NO_PATHCONV=1 docker run --rm --entrypoint promtool -v "$(cd "$ROOT/deployments" && pwd -W 2>/dev/null || pwd):/d:ro" \
    prom/prometheus:v2.55.1 check rules /d/prometheus-rules.yml 2>&1)
if echo "$R" | grep -q SUCCESS; then ok "prometheus-rules.yml valid ($(echo "$R" | grep -oE '[0-9]+ rules'))"; else bad "promtool: $R"; fi
chk "authorization-svc alert group" "$(grep -c '^      - alert: Authorization' "$ROOT/deployments/prometheus-rules.yml")" "8"
if $PY -c "import json,sys;json.load(open(sys.argv[1],encoding='utf-8'))" "$ROOT/deployments/grafana/provisioning/dashboards/authorization-svc.json" 2>/dev/null; then
  ok "grafana dashboard authorization-svc.json parses"; else bad "grafana dashboard"; fi

echo; echo "-- 9. Outbox drain -------------------------------------------------"
P=$(docker exec zoiko-postgres psql -U postgres -d authorization_svc -tAc "select count(*) from outbox_events where published_at is null" 2>/dev/null | tr -d '[:space:]')
if [ -n "$P" ] && [ "$P" -lt 100 ]; then ok "outbox_events unpublished = $P"; else bad "outbox_events unpublished = ${P:-unreadable}"; fi

echo; echo "-- 10. Scenario suites ---------------------------------------------"
if [ "${SKIP_E2E:-}" != 1 ]; then
  S=$($PY "$ROOT/deployments/scripts/e2e-authorization-scenarios.py" 2>&1 | tail -1)
  case "$S" in *passed) N=${S%%/*}; T=${S#*/}; T=${T%% *}; chk "persona scenarios ($S)" "$N" "$T";; *) bad "persona scenarios: $S";; esac
fi

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
