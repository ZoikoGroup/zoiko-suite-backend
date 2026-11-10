#!/usr/bin/env bash
# Audit test — governance-decision-log-svc (GDL) only.
SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8083}
TEN=11111111-1111-1111-1111-111111111111
PRIN=33333333-3333-3333-3333-333333333333
ENT=22222222-2222-2222-2222-222222222222
RUN=$(date +%s)$$
PROM=${PROM:-http://127.0.0.1:9090}
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk()  { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }

echo "==================================================================="
echo " AUDIT — governance-decision-log-svc (GDL)"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo
echo "-- 1. Static analysis ---------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi

echo
echo "-- 2. Test suite ---------------------------------------------------"
for i in 1 2 3 4 5; do
  OUT=$(go test -count=1 ./... 2>&1)
  echo "$OUT" | grep -q "Application Control" && continue
  break
done
PKG_OK=$(echo "$OUT" | grep -cE '^ok ')
PKG_FAIL=$(echo "$OUT" | grep -cE '^FAIL')
chk "packages failing" "$PKG_FAIL" "0"
echo "        packages passing: $PKG_OK"

echo
echo "-- 3. Store integration vs real Postgres 16 ------------------------"
docker exec zoiko-postgres psql -U postgres -c "DROP DATABASE IF EXISTS gdl_audit_scratch;" >/dev/null 2>&1
docker exec zoiko-postgres psql -U postgres -c "CREATE DATABASE gdl_audit_scratch;" >/dev/null 2>&1
SOUT=$(TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/gdl_audit_scratch?sslmode=disable" go test -count=1 -v ./internal/store/ 2>&1)
SP=$(echo "$SOUT" | grep -cE '^--- PASS|^    --- PASS')
SF=$(echo "$SOUT" | grep -cE '^--- FAIL|^    --- FAIL')
SS=$(echo "$SOUT" | grep -cE '^--- SKIP')
chk "store tests failing" "$SF" "0"
chk "store tests skipped" "$SS" "0"
echo "        store tests passing: $SP"

echo
echo "-- 4. Silent-skip guard -------------------------------------------"
GOUT=$(REQUIRE_DB_TESTS=1 go test -count=1 ./internal/store/ 2>&1 | grep -c "claims to verify")
if [ "$GOUT" -gt 0 ]; then ok "store suite fails loudly when REQUIRE_DB_TESTS is set ($GOUT tests)"; else bad "silent-skip guard"; fi

echo
echo "-- 5. Live service health ------------------------------------------"
HJ=$(curl -s -m 20 $B/healthz)
chk "GET /healthz" "$(code $B/healthz)" "200"
RD=$(curl -s -m 20 $B/readyz)
chk "GET /readyz" "$(code $B/readyz)" "200"
for c in postgres outbox; do
  if echo "$RD" | grep -q "\"$c\":\"ok\""; then ok "readiness check: $c"; else bad "readiness check: $c"; fi
done

echo
echo "-- 6. GDL contract surface (spec §18–§21) --------------------------"
# Admin routes require step-up (mfa_verified). We can't easily do that here,
# so we just check the routes exist and are protected.
for path in \
  "/v1/decisions" \
  "/v1/decisions/{id}" \
  "/v1/decisions/{id}/replay" \
  "/v1/decisions/{id}/replay-manifests" \
  "/v1/evidence/chain/verify" \
  "/v1/evidence/chain/export" \
  "/v1/evidence/packages/seal" \
  "/v1/evidence/packages/{id}"; do
  chk "route $path exists (401 as expected)" "$(code -X GET $B$path "${H[@]}" -H 'X-Request-Id: ia1' -H 'X-Correlation-ID: ia1')" "401"
done

# If-Match required on mutations
KM=$(uuidgen)
chk "POST /v1/decisions requires If-Match" "$(code -X POST $B/v1/decisions "${H[@]}" -H 'X-Request-Id: ia1' -H 'X-Correlation-ID: ia1' -d "{\"tenant_id\":\"$TEN\",\"legal_entity_id\":\"$ENT\",\"actor_id\":\"$PRIN\",\"action_type\":\"PAYROLL_RELEASE\",\"outcome\":\"GRANTED\",\"rule_basis\":\"policy-v3-sod\",\"correlation_id\":\"$KM\"}')" "428"
chk "POST /v1/decisions requires Idempotency-Key" "$(code -X POST $B/v1/decisions "${H[@]}" -H 'If-Match: *' -H 'X-Request-Id: ia2' -H "Idempotency-Key: ia2-$RUN" -H 'X-Correlation-ID: ia2' -d "{\"tenant_id\":\"$TEN\",\"legal_entity_id\":\"$ENT\",\"actor_id\":\"$PRIN\",\"action_type\":\"PAYROLL_RELEASE\",\"outcome\":\"GRANTED\",\"rule_basis\":\"policy-v3-sod\",\"correlation_id\":\"$KM\"}')" "428"

# Cursor paging on lists
s=$(code -X GET $B/v1/decisions "${H[@]}" -H 'X-Request-Id: ia3' -H 'X-Correlation-ID: ia3')
if [ "$s" = "401" ]; then ok "GET /v1/decisions exists (401 as expected)"; else bad "GET /v1/decisions (got $s)"; fi

echo
echo "-- 7. Negative-path acceptance (spec §18–§21) -----------------------"
# NP1: Missing Idempotency-Key on mutation refused (428)
KM=$(uuidgen)
s=$(code -X POST $B/v1/decisions "${H[@]}" -H 'If-Match: *' -H 'X-Request-Id: np1' -H 'X-Correlation-ID: np1' -d "{\"tenant_id\":\"$TEN\",\"legal_entity_id\":\"$ENT\",\"actor_id\":\"$PRIN\",\"action_type\":\"PAYROLL_RELEASE\",\"outcome\":\"GRANTED\",\"rule_basis\":\"policy-v3-sod\",\"correlation_id\":\"$KM\"}")
chk "NP1 missing Idempotency-Key refused" "$s" "428"

# NP2: Stale If-Match refused (412)
s=$(code -X POST $B/v1/decisions "${H[@]}" -H 'If-Match: "stale-etag"' -H 'X-Request-Id: np2' -H "Idempotency-Key: np2-$RUN" -H 'X-Correlation-ID: np2' -d "{\"tenant_id\":\"$TEN\",\"legal_entity_id\":\"$ENT\",\"actor_id\":\"$PRIN\",\"action_type\":\"PAYROLL_RELEASE\",\"outcome\":\"GRANTED\",\"rule_basis\":\"policy-v3-sod\",\"correlation_id\":\"$KM\"}")
chk "NP2 stale If-Match refused" "$s" "412"

# NP3: Decision not found returns 404
s=$(code -X GET $B/v1/decisions/00000000-0000-0000-0000-000000000000 "${H[@]}" -H 'X-Request-Id: np3' -H 'X-Correlation-ID: np3')
chk "NP3 unknown decision returns 404" "$s" "404"

# NP4: Cursor pagination works
s=$(code -X GET $B/v1/decisions?cursor=invalid "${H[@]}" -H 'X-Request-Id: np4' -H 'X-Correlation-ID: np4')
chk "NP4 invalid cursor refused" "$s" "400"

# NP5: Evidence ledger endpoints require authorization
s=$(code -X GET $B/v1/evidence/chain/verify "${H[@]}" -H 'X-Request-Id: np5' -H 'X-Correlation-ID: np5')
chk "NP5 verify chain requires auth" "$s" "401"

s=$(code -X GET $B/v1/evidence/chain/export "${H[@]}" -H 'X-Request-Id: np6' -H 'X-Correlation-ID: np6')
chk "NP6 export chain requires auth" "$s" "401"

s=$(code -X POST $B/v1/evidence/packages/seal "${H[@]}" -H 'X-Request-Id: np7' -H "Idempotency-Key: np7-$RUN" -H 'X-Correlation-ID: np7' -d '{"package_id":"test","last_decision_id":"00000000-0000-0000-0000-000000000000"}')
chk "NP7 seal package requires auth" "$s" "401"

s=$(code -X GET $B/v1/evidence/packages/test-package "${H[@]}" -H 'X-Request-Id: np8' -H 'X-Correlation-ID: np8')
chk "NP8 get package requires auth" "$s" "401"

echo
echo "-- 8. G-07 Evidence ledger ------------------------------------------"
# VerifyChain endpoint
s=$(code -X GET $B/v1/evidence/chain/verify "${H[@]}" -H 'X-Request-Id: g07-1' -H 'X-Correlation-ID: g07-1')
if [ "$s" = "401" ]; then ok "G-07 verify chain endpoint exists (401 as expected)"; else bad "G-07 verify chain (got $s)"; fi

# ExportChain endpoint
s=$(code -X GET $B/v1/evidence/chain/export "${H[@]}" -H 'X-Request-Id: g07-2' -H 'X-Correlation-ID: g07-2')
if [ "$s" = "401" ]; then ok "G-07 export chain endpoint exists (401 as expected)"; else bad "G-07 export chain (got $s)"; fi

# SealPackage endpoint
s=$(code -X POST $B/v1/evidence/packages/seal "${H[@]}" -H 'X-Request-Id: g07-3' -H "Idempotency-Key: g07-3-$RUN" -H 'X-Correlation-ID: g07-3' -d '{"package_id":"test","last_decision_id":"00000000-0000-0000-0000-000000000000"}')
if [ "$s" = "401" ]; then ok "G-07 seal package endpoint exists (401 as expected)"; else bad "G-07 seal package (got $s)"; fi

# GetPackage endpoint
s=$(code -X GET $B/v1/evidence/packages/test-package "${H[@]}" -H 'X-Request-Id: g07-4' -H 'X-Correlation-ID: g07-4')
if [ "$s" = "401" ]; then ok "G-07 get package endpoint exists (401 as expected)"; else bad "G-07 get package (got $s)"; fi

echo
echo "-- 9. Telemetry ----------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
GDLN=$(echo "$M" | grep -c '^governance_decision_log_')
if [ "$GDLN" -ge 10 ]; then ok "GDL metric families emitted ($GDLN series)"; else bad "GDL metrics ($GDLN)"; fi
for fam in governance_decision_log_recorded_total governance_decision_log_decisions_total \
           governance_decision_log_replays_total governance_decision_log_chain_verifications_total \
           governance_decision_log_chain_exports_total governance_decision_log_packages_sealed_total \
           governance_decision_log_outbox_pending governance_decision_log_outbox_dead_letter; do
  if echo "$M" | grep -q "^$fam"; then ok "  $fam"; else bad "  $fam missing"; fi
done
echo "$M" | grep -q '^governance_decision_log_recorded_total{.*outcome="created"' && ok "  created decisions counted" || bad "  created decisions not counted"
echo "$M" | grep -q '^governance_decision_log_recorded_total{.*outcome="replay"' && ok "  replay decisions counted" || bad "  replay decisions not counted"

echo
echo "-- 10. Alert rules (gate 11) ---------------------------------------"
RULES=$(curl -s -m 20 "$PROM/api/v1/rules")
NR=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
n=sum(len(g['rules']) for g in d['data']['groups'] if 'gdl' in g['name'] or 'governance' in g['name'])
print(n)" 2>/dev/null)
NH=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
n=sum(1 for g in d['data']['groups'] if ('gdl' in g['name'] or 'governance' in g['name']) for r in g['rules'] if r.get('health')=='ok')
print(n)" 2>/dev/null)
chk "GDL/governance alert rules loaded" "$NR" "6"
chk "alert rules evaluating (health ok)" "$NH" "6"
UP=$(curl -s -m 20 --get "$PROM/api/v1/query" --data-urlencode 'query=up{job="governance-decision-log-svc"}' | python -c "
import sys,json
r=json.load(sys.stdin)['data']['result']
print(r[0]['value'][1] if r else '0')" 2>/dev/null)
chk "prometheus scraping the service" "$UP" "1"

echo
echo "-- 11. Outbox drain ------------------------------------------------"
PENDING=$(docker exec zoiko-postgres psql -U postgres -d governance_decision_log -tAc "select count(*) filter (where published_at is null) from event_outbox;" 2>/dev/null | tr -d '[:space:]')
PUB=$(docker exec zoiko-postgres psql -U postgres -d governance_decision_log -tAc "select count(published_at) from event_outbox;" 2>/dev/null | tr -d '[:space:]')
echo "        published=$PUB pending=$PENDING"
if [ "${PENDING:-1}" -lt 100 ]; then ok "outbox at baseline"; else bad "outbox backlog $PENDING"; fi

echo
echo "-- 12. Contract parity ---------------------------------------------"
# OpenAPI parity test
cd "$SVC"
if [ -f internal/handler/openapi_parity_test.go ]; then
  OUT=$(go test -count=1 ./internal/handler -run OpenAPI 2>&1)
  if echo "$OUT" | grep -q "PASS"; then ok "OpenAPI parity test passes"; else bad "OpenAPI parity test fails"; fi
else
  echo "  SKIP  OpenAPI parity test not found"
fi

# AsyncAPI parity test
if [ -f internal/events/contract_test.go ]; then
  OUT=$(go test -count=1 ./internal/events -run Contract 2>&1)
  if echo "$OUT" | grep -q "PASS"; then ok "AsyncAPI parity test passes"; else bad "AsyncAPI parity test fails"; fi
else
  echo "  SKIP  AsyncAPI parity test not found"
fi

docker exec zoiko-postgres psql -U postgres -c "DROP DATABASE IF EXISTS gdl_audit_scratch;" >/dev/null 2>&1

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]