#!/usr/bin/env bash
# Audit test — identity-context-svc (GOV-01) only.
SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8080}
TEN=11111111-1111-1111-1111-111111111111
PRIN=33333333-3333-3333-3333-333333333333
ENT=22222222-2222-2222-2222-222222222222
SUP=44444444-4444-4444-4444-444444444444
H=(-H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" -H "X-Legal-Entity-Id: $ENT" -H "X-Source-Channel: api")
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk()  { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }

echo "==================================================================="
echo " AUDIT — identity-context-svc (GOV-01)"
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
docker exec zoiko-postgres psql -U postgres -c "DROP DATABASE IF EXISTS identity_audit_scratch;" >/dev/null 2>&1
docker exec zoiko-postgres psql -U postgres -c "CREATE DATABASE identity_audit_scratch;" >/dev/null 2>&1
SOUT=$(TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/identity_audit_scratch?sslmode=disable" go test -count=1 -v ./internal/store/ 2>&1)
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
HJ=$(curl -s -m 20 $B/health)
chk "GET /health" "$(code $B/health)" "200"
for c in postgres redis outbox tenant_registry; do
  if echo "$HJ" | grep -q "\"$c\":\"ok\""; then ok "health check: $c"; else bad "health check: $c"; fi
done

echo
echo "-- 6. GOV-01 contract surface (spec section 4) ---------------------"
TOKEN=$(curl -s -m 20 -X POST $B/v1/authenticate -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"admin@zoikosuite.com\",\"password\":\"Zoiko@Governance1\"}" \
  | python -c "import sys,json;print(json.load(sys.stdin)['access_token'])")
R=$(curl -s -m 25 -X POST $B/v1/context/resolve "${H[@]}" -H "X-Request-Id: au1" -H "Idempotency-Key: au1" -H "X-Correlation-ID: au1" \
  -d "{\"bearer_token\":\"$TOKEN\",\"legal_entity_id\":\"$ENT\",\"correlation_id\":\"au1\"}")
SID=$(echo "$R" | python -c "import sys,json;print(json.load(sys.stdin).get('session_context_id',''))" 2>/dev/null)
if [ -n "$SID" ]; then ok "ResolveTenantContext        POST /v1/context/resolve"; else bad "ResolveTenantContext"; fi
if echo "$R" | grep -q envelope_jwt; then ok "  ... returns a signed envelope"; else bad "envelope_jwt absent"; fi
chk "GetEffectiveContext        GET  /v1/context/session/{id}" \
    "$(code $B/v1/context/session/$SID "${H[@]}" -H 'X-Request-Id: au2' -H 'X-Correlation-ID: au2')" "200"
chk "ExplainContextResolution   GET  .../explain" \
    "$(code $B/v1/context/session/$SID/explain "${H[@]}" -H 'X-Request-Id: au3' -H 'X-Correlation-ID: au3')" "200"
chk "RefreshTenantContextCache  POST /v1/context/cache/refresh" \
    "$(code -X POST $B/v1/context/cache/refresh "${H[@]}" -H 'X-Request-Id: au4' -H 'Idempotency-Key: au4' -H 'X-Correlation-ID: au4' -d '{"correlation_id":"au4"}')" "200"
chk "InvalidateTenantContext    POST /v1/context/tenant/invalidate" \
    "$(code -X POST $B/v1/context/tenant/invalidate "${H[@]}" -H 'X-Request-Id: au5' -H 'Idempotency-Key: au5' -H 'X-Correlation-ID: au5' -d '{"correlation_id":"au5","justification":"audit run: verifying the named command surface"}')" "200"
SR=$(curl -s -m 25 -X POST $B/v1/context/support "${H[@]}" -H "X-Request-Id: au6" -H "Idempotency-Key: au6" -H "X-Correlation-ID: au6" \
  -d "{\"tenant_id\":\"$TEN\",\"support_principal_id\":\"$SUP\",\"approver_principal_id\":\"$PRIN\",\"reason_code\":\"INCIDENT_RESPONSE\",\"justification\":\"audit run: exercising the privileged break-glass command\",\"ticket_ref\":\"AUDIT-X\",\"ttl_seconds\":300}")
SC=$(echo "$SR" | python -c "import sys,json;print(json.load(sys.stdin).get('support_context_id',''))" 2>/dev/null)
if [ -n "$SC" ]; then ok "AttachSupportContext       POST /v1/context/support"; else bad "AttachSupportContext: $SR"; fi

echo
echo "-- 7. Negative-path acceptance (spec section 4) --------------------"
docker exec zoiko-postgres psql -U postgres -d identity_context -c \
  "INSERT INTO tenant_ingress_bindings (ingress_identifier,tenant_id,environment,active_flag) VALUES ('audit-other.zoiko.local','88888888-8888-8888-8888-888888888888','local',true) ON CONFLICT (ingress_identifier) DO UPDATE SET tenant_id=EXCLUDED.tenant_id;" >/dev/null 2>&1
# The spec requires the spoofed header be "ignored/rejected". This service
# IGNORES it: the envelope is bound to the tenant in the verified token, never
# to the header. Asserting 401 tests only the "rejected" branch and fails a
# correct implementation, so decode the envelope and check what it is bound to.
NP1=$(curl -s -m 25 -X POST $B/v1/context/resolve -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 88888888-8888-8888-8888-888888888888' -H "X-Principal-Id: $PRIN" \
  -H "X-Legal-Entity-Id: $ENT" -H 'X-Source-Channel: api' \
  -H 'X-Request-Id: np1' -H 'Idempotency-Key: np1' -H 'X-Correlation-ID: np1' \
  -d "{\"bearer_token\":\"$TOKEN\",\"legal_entity_id\":\"$ENT\"}" \
  | python -c "
import sys,json,base64
try:
    d=json.load(sys.stdin)
except Exception:
    print('unparseable'); raise SystemExit
if 'envelope_jwt' not in d:
    print('refused'); raise SystemExit
p=d['envelope_jwt'].split('.')[1]; p+='='*(-len(p)%4)
print(json.loads(base64.urlsafe_b64decode(p))['principal']['tenant_id'])
" 2>/dev/null)
if [ "$NP1" = "$TEN" ] || [ "$NP1" = "refused" ]; then
  ok "NP1 spoofed tenant header ignored (envelope bound to the token tenant, not the header)"
else
  bad "NP1 spoofed tenant header honoured (envelope bound to $NP1)"
fi
chk "NP2 unknown ingress cannot select a tenant" \
    "$(code -X POST $B/v1/context/resolve "${H[@]}" -H 'X-Canonical-Ingress: audit-other.zoiko.local' -H 'X-Request-Id: np2' -H 'Idempotency-Key: np2' -H 'X-Correlation-ID: np2' -d "{\"bearer_token\":\"$TOKEN\",\"legal_entity_id\":\"$ENT\"}")" "401"
chk "NP3 self-approved break-glass refused" \
    "$(code -X POST $B/v1/context/support "${H[@]}" -H 'X-Request-Id: np3' -H 'Idempotency-Key: np3' -H 'X-Correlation-ID: np3' -d "{\"tenant_id\":\"$TEN\",\"support_principal_id\":\"$PRIN\",\"approver_principal_id\":\"$PRIN\",\"reason_code\":\"INCIDENT_RESPONSE\",\"justification\":\"audit run: self-approval must be refused\",\"ticket_ref\":\"AUDIT-Y\",\"ttl_seconds\":300}")" "400"
chk "NP3 excessive TTL refused" \
    "$(code -X POST $B/v1/context/support "${H[@]}" -H 'X-Request-Id: np3b' -H 'Idempotency-Key: np3b' -H 'X-Correlation-ID: np3b' -d "{\"tenant_id\":\"$TEN\",\"support_principal_id\":\"$SUP\",\"approver_principal_id\":\"$PRIN\",\"reason_code\":\"INCIDENT_RESPONSE\",\"justification\":\"audit run: TTL beyond the ceiling must be refused\",\"ticket_ref\":\"AUDIT-Z\",\"ttl_seconds\":999999}")" "400"
if [ -n "$SC" ]; then
  chk "NP4 revoke support context" "$(code -X DELETE $B/v1/context/support/$SC "${H[@]}" -H 'X-Request-Id: np4a' -H 'Idempotency-Key: np4a' -H 'X-Correlation-ID: np4a')" "204"
  chk "NP4 revoke is idempotent"   "$(code -X DELETE $B/v1/context/support/$SC "${H[@]}" -H 'X-Request-Id: np4b' -H 'Idempotency-Key: np4b' -H 'X-Correlation-ID: np4b')" "204"
fi
chk "envelope contract enforced (no headers)" \
    "$(code -X POST $B/v1/context/resolve -H 'Content-Type: application/json' -d '{}')" "401"

echo
echo "-- 8. Telemetry ----------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
GOVN=$(echo "$M" | grep -c '^identity_context')
if [ "$GOVN" -ge 9 ]; then ok "GOV-01 metric families emitted ($GOVN series)"; else bad "GOV-01 metrics ($GOVN)"; fi
echo "$M" | grep -q 'identity_context_outbox_pending' && ok "outbox depth gauge present" || bad "outbox gauge"

echo
echo "-- 9. Alert rules (gate 11) ----------------------------------------"
RULES=$(curl -s -m 20 "http://localhost:9090/api/v1/rules")
NR=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
n=sum(len(g['rules']) for g in d['data']['groups'] if 'gov-01' in g['name'])
print(n)" 2>/dev/null)
NH=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
n=sum(1 for g in d['data']['groups'] if 'gov-01' in g['name'] for r in g['rules'] if r.get('health')=='ok')
print(n)" 2>/dev/null)
chk "GOV-01 alert rules loaded" "$NR" "8"
chk "alert rules evaluating (health ok)" "$NH" "8"
UP=$(curl -s -m 20 --get "http://localhost:9090/api/v1/query" --data-urlencode 'query=up{job="identity-context-svc"}' | python -c "
import sys,json
r=json.load(sys.stdin)['data']['result']
print(r[0]['value'][1] if r else '0')" 2>/dev/null)
chk "prometheus scraping the service" "$UP" "1"

echo
echo "-- 10. Outbox drain ------------------------------------------------"
PENDING=$(docker exec zoiko-postgres psql -U postgres -d identity_context -tAc "select count(*) filter (where published_at is null) from event_outbox;" 2>/dev/null | tr -d '[:space:]')
PUB=$(docker exec zoiko-postgres psql -U postgres -d identity_context -tAc "select count(published_at) from event_outbox;" 2>/dev/null | tr -d '[:space:]')
echo "        published=$PUB pending=$PENDING"
if [ "${PENDING:-1}" -lt 100 ]; then ok "outbox at baseline"; else bad "outbox backlog $PENDING"; fi

docker exec zoiko-postgres psql -U postgres -d identity_context -c "DELETE FROM tenant_ingress_bindings WHERE ingress_identifier='audit-other.zoiko.local';" >/dev/null 2>&1
docker exec zoiko-postgres psql -U postgres -c "DROP DATABASE IF EXISTS identity_audit_scratch;" >/dev/null 2>&1

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
