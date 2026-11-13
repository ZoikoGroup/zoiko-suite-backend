#!/usr/bin/env bash
# Audit — jurisdiction-rules-svc (D2-1) only. Static checks, every suite on
# scratch databases (0 skips), then live checks against the running service.
#
#   B=http://127.0.0.1:8082 bash scripts/audit.sh          # everything
#   SKIP_SUITES=1 bash scripts/audit.sh                     # live checks only
#
# Needs: the zoiko-postgres container; for the live part, jurisdiction-svc and
# identity-context-svc (the demo admin's signed envelope). Python on PATH as
# `python` (or PY=...).
SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://127.0.0.1:8082}
IDP=${IDP:-http://127.0.0.1:8080}
PY=${PY:-python}
TEN=11111111-1111-1111-1111-111111111111
PRIN=33333333-3333-3333-3333-333333333333
ENT=22222222-2222-2222-2222-222222222222
PG=zoiko-postgres
RUN=$(date +%s)$$
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk()  { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
sql()  { docker exec $PG psql -U postgres -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'; }

echo "==================================================================="
echo " AUDIT — jurisdiction-rules-svc (D2-1)"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

cd "$SVC" || exit 1
if [ -z "$SKIP_SUITES" ]; then
  echo
  echo "-- 1. Static analysis ---------------------------------------------"
  if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
  if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi
  if [ -z "$(go vet -tags=integration ./internal/registryit/ 2>&1)" ]; then ok "go vet -tags=integration clean"; else bad "go vet (integration)"; fi

  echo
  echo "-- 2. Unit suite ----------------------------------------------------"
  for i in 1 2 3 4 5; do
    OUT=$(go test -count=1 ./... 2>&1)
    echo "$OUT" | grep -q "Application Control" && continue
    break
  done
  chk "packages failing" "$(echo "$OUT" | grep -cE '^FAIL')" "0"
  echo "        packages passing: $(echo "$OUT" | grep -cE '^ok ')"

  echo
  echo "-- 3. Store suite vs real Postgres (0 skips) -----------------------"
  docker exec $PG psql -U postgres -q -c "DROP DATABASE IF EXISTS jur_audit_store WITH (FORCE)" -c "CREATE DATABASE jur_audit_store" >/dev/null 2>&1
  SOUT=$(TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/jur_audit_store?sslmode=disable" REQUIRE_DB_TESTS=1 go test -count=1 -p 1 -v ./internal/store/ 2>&1)
  chk "store tests failing" "$(echo "$SOUT" | grep -cE '^\s*--- FAIL')" "0"
  chk "store tests skipped" "$(echo "$SOUT" | grep -cE '^\s*--- SKIP')" "0"
  echo "        store tests passing: $(echo "$SOUT" | grep -cE '^\s*--- PASS')"

  echo
  echo "-- 4. Registry integration suite (every migration, 0 skips) --------"
  docker exec $PG psql -U postgres -q -c "DROP DATABASE IF EXISTS jur_audit_it WITH (FORCE)" -c "CREATE DATABASE jur_audit_it" >/dev/null 2>&1
  IOUT=$(JUR_IT_DSN="postgres://postgres:postgres@127.0.0.1:5432/jur_audit_it?sslmode=disable" go test -tags=integration -count=1 -timeout=1200s -v ./internal/registryit/ 2>&1)
  chk "registryit tests failing" "$(echo "$IOUT" | grep -cE '^\s*--- FAIL')" "0"
  chk "registryit tests skipped" "$(echo "$IOUT" | grep -cE '^\s*--- SKIP')" "0"
  echo "        registryit tests passing: $(echo "$IOUT" | grep -cE '^\s*--- PASS')"
  docker exec $PG psql -U postgres -q -c "DROP DATABASE IF EXISTS jur_audit_store WITH (FORCE)" -c "DROP DATABASE IF EXISTS jur_audit_it WITH (FORCE)" >/dev/null 2>&1

  echo
  echo "-- 5. Contracts ------------------------------------------------------"
  OUT=$(go test -count=1 ./internal/handler -run OpenAPI 2>&1)
  if echo "$OUT" | grep -q "^ok"; then ok "openapi.yaml valid and matches the router"; else bad "OpenAPI: ${OUT:0:200}"; fi
  OUT=$(go test -count=1 ./internal/events -run AsyncAPI 2>&1)
  if echo "$OUT" | grep -q "^ok"; then ok "asyncapi.yaml documents every event and only those"; else bad "AsyncAPI: ${OUT:0:200}"; fi
fi

echo
echo "-- 6. Database (dev) -------------------------------------------------"
NMIG=$(ls "$SVC"/deployments/migrations/*.up.sql | wc -l | tr -d ' ')
APPLIED=$(sql jurisdiction_rules "select count(*) from schema_migrations")
chk "every migration applied to the dev database" "$APPLIED" "$NMIG"
chk "RLS enabled and forced on the five tenant tables" "$(sql jurisdiction_rules "select count(*) from pg_class where relname in ('entity_jurisdiction_applicability','regulatory_submissions','regulatory_submission_events','rule_decision_evidence','async_operations') and relrowsecurity and relforcerowsecurity")" "5"
chk "the runtime role is not a superuser and cannot bypass RLS" "$(sql jurisdiction_rules "select count(*) from pg_roles where rolname='app_jurisdiction_rules' and not rolsuper and not rolbypassrls")" "1"
chk "row_version on the fourteen aggregates (000034)" "$(sql jurisdiction_rules "select count(*) from information_schema.columns where column_name='row_version' and table_name in ('regulatory_sources','interpretation_records','jurisdiction_pack_versions','pack_signing_keys','regulatory_calendar_versions','obligation_rules','payment_term_versions','jurisdiction_rollouts','remediation_cases','source_change_notices','jurisdiction_import_batches','jurisdiction_aliases','regulatory_submissions','pack_hotfixes')")" "14"

echo
echo "-- 7. Live service ---------------------------------------------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz" "$(code $B/readyz)" "200"
R0=(-H "X-Tenant-Id: $TEN" -H "X-Legal-Entity-Id: $ENT" -H "X-Request-Id: au-$RUN" -H "X-Correlation-ID: au-$RUN" -H "X-Source-Channel: api")
TOKEN=$(curl -s -m 20 -X POST $IDP/v1/authenticate -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"admin@zoikosuite.com\",\"password\":\"Zoiko@Governance1\",\"client_id\":\"zoiko-console\",\"client_secret\":\"local-dev-console-client-secret\"}" \
  | $PY -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null)
ENVJWT=$(curl -s -m 20 -X POST $IDP/v1/context/resolve "${R0[@]}" -H "X-Principal-Id: $PRIN" -H "Idempotency-Key: au-env-$RUN" -H 'Content-Type: application/json' \
  -d "{\"bearer_token\":\"$TOKEN\",\"legal_entity_id\":\"$ENT\"}" \
  | $PY -c "import sys,json; print(json.load(sys.stdin).get('envelope_jwt',''))" 2>/dev/null)
if [ -n "$ENVJWT" ]; then ok "signed envelope issued for the demo admin"; else bad "no envelope from identity-context-svc at $IDP"; fi
RH=("${R0[@]}" -H "X-Principal-Id: $PRIN" -H "Authorization: Bearer $ENVJWT")
WH=("${RH[@]}" -H "Content-Type: application/json")

chk "GET /v1/jurisdictions (legacy array)" "$(code "${RH[@]}" $B/v1/jurisdictions)" "200"
P=$(curl -s -m 20 "${RH[@]}" "$B/v1/jurisdictions?page_size=2")
if echo "$P" | grep -q '"items"' && echo "$P" | grep -q '"page_size":2'; then ok "API-4 paged list answers {items, page_size}"; else bad "paged list: ${P:0:160}"; fi
chk "API-4 tampered cursor → 400" "$(code "${RH[@]}" "$B/v1/jurisdictions?page_size=2&cursor=abc.def")" "400"
chk "API-4 unknown filter → 400" "$(code "${RH[@]}" "$B/v1/sources?filter%5Bcolour%5D=red")" "400"
chk "GET /v1/jurisdiction-summary" "$(code "${RH[@]}" $B/v1/jurisdiction-summary)" "200"
chk "GET /v1/admin/outbox/parked" "$(code "${RH[@]}" $B/v1/admin/outbox/parked)" "200"
chk "GET /v1/regimes" "$(code "${RH[@]}" $B/v1/regimes)" "200"
chk "GET /v1/pack-coverage" "$(code "${RH[@]}" $B/v1/pack-coverage)" "200"
chk "GET /v1/operations/{unknown} → 404" "$(code "${RH[@]}" $B/v1/operations/op_000000000000000000000000)" "404"

echo
echo "-- 8. Negative paths (live) ------------------------------------------"
J="{\"jurisdiction_code\":\"AU$RUN\",\"jurisdiction_name\":\"Audit probe\",\"jurisdiction_type\":\"COUNTRY\",\"authority_type\":\"NATIONAL\",\"effective_from\":\"2026-01-01T00:00:00Z\"}"
chk "J-01: admin write with identity headers only → 401" "$(code -X POST "${R0[@]}" -H "X-Principal-Id: $PRIN" -H 'Content-Type: application/json' -H "Idempotency-Key: h-$RUN" -d "$J" $B/v1/admin/jurisdictions)" "401"
chk "J-01: forged X-Identity-Basis is not a verification → 401" "$(code -X POST "${R0[@]}" -H "X-Principal-Id: $PRIN" -H "X-Identity-Basis: verified_envelope" -H 'Content-Type: application/json' -H "Idempotency-Key: f-$RUN" -d "$J" $B/v1/admin/jurisdictions)" "401"
chk "malformed body → 400" "$(code -X POST "${WH[@]}" -H "Idempotency-Key: m-$RUN" -d '{bad' $B/v1/admin/jurisdictions)" "400"
chk "unknown jurisdiction → 404 (REF-01 #22)" "$(code "${RH[@]}" $B/v1/jurisdictions/00000000-0000-4000-8000-000000000000)" "404"
chk "malformed If-Match → 400" "$(code -X POST "${WH[@]}" -H "Idempotency-Key: i-$RUN" -H 'If-Match: "x"' -d '{"reason":"r"}' $B/v1/admin/sources/00000000-0000-4000-8000-000000000000/review)" "400"

echo
echo "-- 9. Telemetry and outbox -------------------------------------------"
M=$(curl -s -m 20 $B/metrics)
for fam in http_requests_total outbox_pending_events; do
  if echo "$M" | grep -q "^$fam"; then ok "  metric $fam"; else bad "  metric $fam missing"; fi
done
PARKED=$(sql jurisdiction_rules "select count(*) from event_outbox where published_at is null and publish_attempts >= 10")
PENDING=$(sql jurisdiction_rules "select count(*) from event_outbox where published_at is null and publish_attempts < 10")
echo "        pending=$PENDING parked=$PARKED"
if [ -n "$PENDING" ] && [ "$PENDING" -lt 100 ]; then ok "outbox drains (pending $PENDING)"; else bad "outbox backlog ${PENDING:-unreadable}"; fi

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
