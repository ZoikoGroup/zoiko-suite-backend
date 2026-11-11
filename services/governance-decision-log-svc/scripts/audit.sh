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
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz" "$(code $B/readyz)" "200"
RD=$(curl -s -m 20 $B/readyz)
if echo "$RD" | grep -q '"status":"ok"'; then ok "readyz reports the database pool ok"; else bad "readyz body: $RD"; fi

# Headers of a well-formed read (purpose of access, D-06) and write.
# R0/W0 carry no principal: curl sends a repeated header twice and the
# service reads the first, so the principal is added per call, never overridden.
R0=(-H "X-Tenant-Id: $TEN" -H "X-Legal-Entity-Id: $ENT" -H "X-Request-Id: au-$RUN"
    -H "X-Correlation-ID: au-$RUN" -H "X-Source-Channel: api" -H "X-Purpose-Context: GOVERNANCE_AUDIT")
W0=(-H "X-Tenant-Id: $TEN" -H "X-Legal-Entity-Id: $ENT" -H "X-Request-Id: au-$RUN"
    -H "X-Correlation-ID: au-$RUN" -H "X-Source-Channel: api" -H "Content-Type: application/json")
RH=("${R0[@]}" -H "X-Principal-Id: $PRIN")
WH=("${W0[@]}" -H "X-Principal-Id: $PRIN")
STRANGER=e2e00000-0000-4000-8000-0000000000aa

# The service runs IDENTITY_VERIFY=require in compose: identity headers alone
# are refused, so the audit signs in as the demo admin and presents the signed
# envelope, as the console does. In require mode a stranger's bare headers are
# refused as unverified (401) before any grant is consulted; the 403 grant path
# is proven by the dependency e2e through a peer workload.
IDP=${IDP:-http://127.0.0.1:8080}
MODE=warn
[ "$(code "${R0[@]}" -H "X-Principal-Id: $PRIN" $B/v1/decisions)" = "401" ] && MODE=require
echo "        identity mode: $MODE"
TOKEN=$(curl -s -m 20 -X POST $IDP/v1/authenticate -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"admin@zoikosuite.com\",\"password\":\"Zoiko@Governance1\",\"client_id\":\"zoiko-console\",\"client_secret\":\"local-dev-console-client-secret\"}" \
  | python -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null)
ENVJWT=$(curl -s -m 20 -X POST $IDP/v1/context/resolve "${W0[@]}" -H "X-Principal-Id: $PRIN" -H "Idempotency-Key: au-env-$RUN" \
  -d "{\"bearer_token\":\"$TOKEN\",\"legal_entity_id\":\"$ENT\"}" \
  | python -c "import sys,json; print(json.load(sys.stdin).get('envelope_jwt',''))" 2>/dev/null)
if [ -n "$ENVJWT" ]; then ok "signed envelope issued for the demo admin"; else bad "no envelope from identity-context-svc at $IDP"; fi
RH+=(-H "Authorization: Bearer $ENVJWT")
WH+=(-H "Authorization: Bearer $ENVJWT")
DENY=403
[ "$MODE" = "require" ] && DENY=401

echo
echo "-- 6. Contract surface: every route answers (openapi.yaml) ----------"
for path in /v1/decisions "/v1/decisions/summary" /v1/replay-manifests /v1/evidence/chain/verify /v1/evidence/packages; do
  chk "GET $path" "$(code "${RH[@]}" $B$path)" "200"
done
chk "GET /v1/evidence/chain/export (attachment)" "$(code "${RH[@]}" $B/v1/evidence/chain/export)" "200"
chk "GET /v1/decisions/{unknown} → 404" "$(code "${RH[@]}" $B/v1/decisions/audit-missing-$RUN)" "404"
chk "GET /v1/evidence/packages/{unknown} → 404" "$(code "${RH[@]}" $B/v1/evidence/packages/00000000-0000-4000-8000-000000000000)" "404"

echo
echo "-- 7. Negative paths ------------------------------------------------"
DEC="{\"decision_id\":\"audit-$RUN\",\"legal_entity_id\":\"$ENT\",\"action_type\":\"AUDIT_PROBE\",\"outcome\":\"GRANTED\",\"rule_basis\":\"AUDIT:probe\",\"correlation_id\":\"au-$RUN\"}"
chk "POST /v1/decisions without Idempotency-Key → 400" "$(code -X POST "${WH[@]}" -d "$DEC" $B/v1/decisions)" "400"
# The envelope contract refuses a write with no actor before the handler does:
# 400 envelope_incomplete or 401, depending on ZS_ENVELOPE_ENFORCEMENT.
s=$(code -X POST -H "X-Tenant-Id: $TEN" -H "Idempotency-Key: k-$RUN" -H 'Content-Type: application/json' -d "$DEC" $B/v1/decisions)
if [ "$s" = "400" ] || [ "$s" = "401" ]; then ok "POST /v1/decisions without a principal refused ($s)"; else bad "POST without a principal (got $s)"; fi
chk "POST /v1/decisions by an unverified or ungranted principal → $DENY" "$(code -X POST "${W0[@]}" -H "X-Principal-Id: $STRANGER" -H "Idempotency-Key: s-$RUN" -d "$DEC" $B/v1/decisions)" "$DENY"
chk "GET /v1/decisions by an unverified or ungranted principal → $DENY" "$(code "${R0[@]}" -H "X-Principal-Id: $STRANGER" $B/v1/decisions)" "$DENY"
chk "GET /v1/decisions?cursor=invalid → 400" "$(code "${RH[@]}" "$B/v1/decisions?cursor=invalid")" "400"
chk "GET /v1/decisions?status=MAYBE → 400" "$(code "${RH[@]}" "$B/v1/decisions?status=MAYBE")" "400"
chk "GET /v1/decisions/summary?days=0 → 400" "$(code "${RH[@]}" "$B/v1/decisions/summary?days=0")" "400"
chk "POST seal by an unverified or ungranted principal → $DENY" "$(code -X POST "${W0[@]}" -H "X-Principal-Id: $STRANGER" -H "Idempotency-Key: z-$RUN" -d '{}' $B/v1/evidence/packages/seal)" "$DENY"

echo
echo "-- 8. G-07 evidence ledger ------------------------------------------"
chk "record a probe decision → 201" "$(code -X POST "${WH[@]}" -H "Idempotency-Key: a-$RUN" -d "$DEC" $B/v1/decisions)" "201"
chk "repeat with the same key → 200" "$(code -X POST "${WH[@]}" -H "Idempotency-Key: a-$RUN" -d "$DEC" $B/v1/decisions)" "200"
V=$(curl -s -m 30 "${RH[@]}" $B/v1/evidence/chain/verify)
if echo "$V" | grep -q '"valid":true'; then ok "chain verifies (links + packages)"; else bad "chain verify: ${V:0:200}"; fi
EV=$(curl -s -m 20 "${RH[@]}" $B/v1/decisions/audit-$RUN/evidence)
if echo "$EV" | grep -q '"hash_valid":true' && echo "$EV" | grep -q '"prev_link_valid":true'; then ok "probe decision's own link re-computes"; else bad "evidence: ${EV:0:200}"; fi
SC=$(code -X POST "${WH[@]}" -H "Idempotency-Key: seal-$RUN" -d '{"label":"audit"}' $B/v1/evidence/packages/seal)
if [ "$SC" = "201" ]; then ok "seal through the head (201)"; else bad "seal (got $SC)"; fi
chk "seal again with nothing new → 409" "$(code -X POST "${WH[@]}" -H "Idempotency-Key: seal2-$RUN" -d '{}' $B/v1/evidence/packages/seal)" "409"

echo
echo "-- 9. Telemetry ----------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
for fam in outbox_pending_events outbox_oldest_pending_seconds outbox_pruned_events_total readiness_up http_requests_total http_request_duration_seconds_bucket; do
  if echo "$M" | grep -q "^$fam{.*service=\"governance-decision-log-svc\""; then ok "  $fam"; else bad "  $fam missing"; fi
done

echo
echo "-- 10. Alert rules --------------------------------------------------"
RULES_FILE="$SVC/../../deployments/prometheus-rules.yml"
NR=$(grep -c "alert: GovernanceLedger" "$RULES_FILE")
# 4 relay/readiness/write rules + 2 for scheduled sealing (chain broken,
# sealing failing), 11 Nov 2026.
chk "governance-decision-log alert rules defined" "$NR" "6"
if curl -s -m 5 "$PROM/-/ready" >/dev/null 2>&1; then
  UP=$(curl -s -m 20 --get "$PROM/api/v1/query" --data-urlencode 'query=up{job="governance-decision-log-svc"}' | python -c "
import sys,json
r=json.load(sys.stdin)['data']['result']
print(r[0]['value'][1] if r else '0')" 2>/dev/null)
  chk "prometheus scraping the service" "$UP" "1"
  LOADED=$(curl -s -m 20 "$PROM/api/v1/rules" | python -c "
import sys,json
g=[x for x in json.load(sys.stdin)['data']['groups'] if x['name']=='governance-decision-log']
print(sum(1 for r in (g[0]['rules'] if g else []) if r.get('health')=='ok'))" 2>/dev/null)
  chk "prometheus loaded the governance-decision-log rules (healthy)" "$LOADED" "6"
else
  echo "  SKIP  prometheus not running at $PROM (start the prometheus compose service to check scraping)"
fi

echo
echo "-- 11. Outbox drain ------------------------------------------------"
sleep 3
PENDING=$(docker exec zoiko-postgres psql -U postgres -d governance_decision_log -tAc "select count(*) from outbox where published_at is null;" 2>/dev/null | tr -d '[:space:]')
PUB=$(docker exec zoiko-postgres psql -U postgres -d governance_decision_log -tAc "select count(*) from outbox where published_at is not null;" 2>/dev/null | tr -d '[:space:]')
echo "        published=$PUB pending=$PENDING"
if [ -n "$PENDING" ] && [ "$PENDING" -lt 100 ]; then ok "outbox drained (pending $PENDING)"; else bad "outbox backlog ${PENDING:-unreadable}"; fi

echo
echo "-- 12. Contract parity ---------------------------------------------"
cd "$SVC"
OUT=$(go test -count=1 ./internal/handler -run OpenAPI 2>&1)
if echo "$OUT" | grep -q "^ok"; then ok "openapi.yaml matches the router"; else bad "OpenAPI parity: ${OUT:0:200}"; fi

docker exec zoiko-postgres psql -U postgres -c "DROP DATABASE IF EXISTS gdl_audit_scratch;" >/dev/null 2>&1

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
