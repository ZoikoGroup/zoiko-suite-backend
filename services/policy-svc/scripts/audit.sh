#!/usr/bin/env bash
# Audit — policy-svc (Governance Platform, Doc 03 §8.1 / GOV-05 / V-001 PDC-05),
# against the code and a live local stack.
#
#   B=http://127.0.0.1:8085 PY=<python> bash services/policy-svc/scripts/audit.sh
#
# Needs: go on PATH (host go test works with GOCACHE/GOTMPDIR on D:, see
# RUNBOOK.md), zoiko-postgres (a scratch database is created and dropped for
# the store suite), and the compose stack with policy-svc, identity-context-svc,
# authorization-svc, access-control-svc, governance-decision-log-svc and
# workflow-svc up (POLICY_IDENTITY_VERIFY=enforce). Live calls carry a signed
# envelope from a demo console login, because bare identity headers are 401.
# SKIP_SUITES=1 skips section 2, SKIP_E2E=1 skips section 9.
set -u
SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
ROOT="$(cd "$SVC/../.." && pwd)"
B=${B:-http://127.0.0.1:8085}
ID=${ID:-http://127.0.0.1:8080}
TEN=11111111-1111-1111-1111-111111111111
ENT=22222222-2222-2222-2222-222222222222
ADMIN=33333333-3333-3333-3333-333333333333
PY=${PY:-python}
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hdr() { # [w] — identity headers plus the signed envelope when one was issued
  local id; id=$(date +%s%N)$RANDOM
  H=(-H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $ADMIN" -H "X-Legal-Entity-Id: $ENT"
     -H "X-Correlation-ID: audit-$id" -H "X-Request-Id: audit-$id" -H "X-Source-Channel: api")
  [ -n "${JWT:-}" ] && H+=(-H "Authorization: Bearer $JWT")
  [ "${1:-}" = w ] && H+=(-H "Idempotency-Key: audit-$id")
  return 0
}
code() { curl -s -m 20 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 20 "$@"; }
gotest() { (cd "$SVC" && GOFLAGS=-mod=mod GOPROXY=off go test "$@"); }

echo "==================================================================="
echo " AUDIT — policy-svc   $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo; echo "-- 1. Static analysis --------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./..."; else bad "go build ./..."; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./..."; else bad "go vet ./..."; fi

if [ "${SKIP_SUITES:-}" != 1 ]; then
  echo; echo "-- 2. Suites, store against Postgres 16 as NOSUPERUSER NOBYPASSRLS --"
  DB="policy_audit_$(date +%s)"
  docker exec zoiko-postgres psql -U postgres -qc "CREATE DATABASE $DB;" >/dev/null 2>&1
  OUT=$(REQUIRE_DB_TESTS=1 TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/$DB?sslmode=disable" \
        gotest -count=1 -p 1 -json ./... 2>&1)
  docker exec zoiko-postgres psql -U postgres -qc "DROP DATABASE IF EXISTS $DB;" >/dev/null 2>&1
  T=$(echo "$OUT" | $PY -c "
import sys,json,collections
c=collections.Counter()
for l in sys.stdin:
    try: e=json.loads(l)
    except ValueError: continue
    if e.get('Test') and e.get('Action') in ('pass','fail','skip'): c[e['Action']]+=1
print(c['pass'], c['fail'], c['skip'])")
  set -- $T
  chk "tests failing" "${2:-?}" "0"
  chk "tests skipped (REQUIRE_DB_TESTS=1)" "${3:-?}" "0"
  echo "        tests passing: ${1:-?}"
fi

echo; echo "-- 3. Live service ------------------------------------------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz" "$(code $B/readyz)" "200"

echo; echo "-- 4. Identity (POL-3: POLICY_IDENTITY_VERIFY=enforce) -------------"
JWT=$($PY - "$ID" "$TEN" "$ENT" "$ADMIN" <<'EOF'
import json, sys, urllib.request, uuid
idp, ten, ent, admin = sys.argv[1:]
def post(url, body, h):
    r = urllib.request.Request(url, json.dumps(body).encode(), {"Content-Type": "application/json", **h}, method="POST")
    with urllib.request.urlopen(r, timeout=20) as x:
        return json.load(x)
t = post(f"{idp}/v1/authenticate", {"tenant_id": ten, "email": "admin@zoikosuite.com", "password": "Zoiko@Governance1",
         "client_id": "zoiko-console", "client_secret": "local-dev-console-client-secret"}, {})
e = post(f"{idp}/v1/context/resolve", {"bearer_token": t["access_token"], "legal_entity_id": ent},
         {"X-Tenant-Id": ten, "X-Principal-Id": admin, "X-Legal-Entity-Id": ent, "X-Request-Id": str(uuid.uuid4()),
          "Idempotency-Key": str(uuid.uuid4()), "X-Correlation-ID": f"audit-{uuid.uuid4()}", "X-Source-Channel": "api"})
print(e["envelope_jwt"])
EOF
)
if [ -n "$JWT" ]; then ok "console envelope issued by identity-context-svc"; else bad "console envelope"; fi
SAVED=$JWT; JWT=""
hdr; chk "bare identity headers are refused" "$(code "${H[@]}" $B/v1/policy-types)" "401"
JWT="${SAVED%????}AAAA"; [ "$JWT" = "$SAVED" ] && JWT="${SAVED%????}BBBB"
hdr; chk "a tampered envelope is refused" "$(code "${H[@]}" $B/v1/policy-types)" "401"
JWT=$SAVED
hdr; chk "the verified envelope is accepted" "$(code "${H[@]}" $B/v1/policy-types)" "200"

echo; echo "-- 5. Contract surface (registers and reads answer 200) ------------"
for p in /v1/policy-types /v1/policy-decision-keys /v1/policies/summary /v1/policy-families /v1/policy-versions \
         /v1/policy-evaluations /v1/control-test-definitions /v1/attestations; do
  hdr; chk "GET $p" "$(code "${H[@]}" "$B$p")" "200"
done

echo; echo "-- 6. Negative paths ----------------------------------------------"
hdr
V=$(body "${H[@]}" "$B/v1/policy-versions?page_size=50" | $PY -c "
import sys,json
items=json.load(sys.stdin).get('items',[])
v=next((i for i in items if i.get('version_status')=='DRAFT'), items[0] if items else None)
print(f\"{v['policy_id']} {v['policy_version_id']} {v.get('row_version',1)}\" if v else '')" 2>/dev/null)
if [ -n "$V" ]; then
  set -- $V
  hdr w; chk "lifecycle command without If-Match is 428" "$(code -X POST "${H[@]}" "$B/v1/policies/$1/versions/$2/submit-review" -d '{}')" "428"
  hdr w; chk "lifecycle command with a stale If-Match is 412" "$(code -X POST "${H[@]}" -H 'If-Match: "99999"' "$B/v1/policies/$1/versions/$2/submit-review" -d '{}')" "412"
  hdr w; chk "lifecycle command with a wildcard If-Match is 400" "$(code -X POST "${H[@]}" -H 'If-Match: *' "$B/v1/policies/$1/versions/$2/submit-review" -d '{}')" "400"
else bad "a policy version to probe (run deployments/scripts/e2e-policy-deps.py once)"; fi
hdr; chk "forged paging cursor is 400 (API-4)" "$(code "${H[@]}" "$B/v1/policy-versions?cursor=forged")" "400"
hdr; chk "malformed policy id is 400" "$(code "${H[@]}" "$B/v1/policies/not-a-uuid")" "400"
hdr; chk "evaluate without decision_id is 400" "$(code -X POST "${H[@]}" $B/v1/policies/evaluate \
  -d "{\"policy_type\":\"APPROVAL_THRESHOLD\",\"action_context\":{\"amount\":1}}")" "400"

echo; echo "-- 7. Telemetry ---------------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
for fam in http_requests_total http_request_duration_seconds outbox_pending outbox_oldest_pending_seconds \
           policy_evidence_pending policy_evidence_failed policy_decisions_total readiness_up; do
  if echo "$M" | grep -q "^# TYPE $fam "; then ok "$fam"; else bad "$fam missing"; fi
done

echo; echo "-- 8. Database -----------------------------------------------------"
Q() { docker exec zoiko-postgres psql -U postgres -d policy -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }
chk "runtime role app_policy is neither superuser nor BYPASSRLS" "$(Q "select rolsuper::text||rolbypassrls::text from pg_roles where rolname='app_policy'")" "falsefalse"
for t in policies policy_versions policy_version_transitions policy_evaluations outbox policy_evidence_outbox \
         policy_activation_schedule control_test_executions attestations policy_test_cases policy_test_runs policy_test_waivers; do
  chk "FORCE RLS on $t" "$(Q "select relforcerowsecurity from pg_class where relname='$t' and relkind='r'")" "t"
done
P=$(Q "select count(*) from outbox where published_at is null")
if [ -n "$P" ] && [ "$P" -lt 100 ]; then ok "outbox unpublished = $P"; else bad "outbox unpublished = ${P:-unreadable}"; fi
F=$(Q "select count(*) from policy_evidence_outbox where failed_at is not null and delivered_at is null")
if [ -n "$F" ]; then ok "policy_evidence_outbox FAILED = $F (re-drive: POST /v1/policy-evaluations/{id}/redeliver)"; else bad "policy_evidence_outbox unreadable"; fi

echo; echo "-- 9. Dependency E2E -----------------------------------------------"
if [ "${SKIP_E2E:-}" != 1 ]; then
  S=$($PY "$ROOT/deployments/scripts/e2e-policy-deps.py" 2>&1 | tail -1)
  case "$S" in *" 0 failed"*) ok "e2e-policy-deps.py: $S";; *) bad "e2e-policy-deps.py: $S";; esac
fi

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
