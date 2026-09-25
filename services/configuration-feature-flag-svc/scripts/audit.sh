#!/usr/bin/env bash
# Audit test — configuration-feature-flag-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# running stack rather than against stubs. Every check below either exercises a
# documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8086), postgres reachable as
# $PGCONTAINER, authorization-svc on :8089, kafka as $KAFKACONTAINER, a Go
# toolchain, and the console's node_modules for section 16 (SKIP_FE=1 skips it).
#
# Exits non-zero if anything failed, so CI can gate on it.
#
# Contract queries live in scripts/spec_query.py rather than inline heredocs:
# python's print() emits CRLF on Windows, CR is not IFS whitespace, and every
# token read back would carry a trailing CR that makes each grep for it fail
# against output plainly containing it.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8086}
AUTHZ=${AUTHZ:-http://localhost:8089}
PGCONTAINER=${PGCONTAINER:-zoiko-postgres}
KAFKACONTAINER=${KAFKACONTAINER:-zoiko-kafka}
KAFKABIN=${KAFKABIN:-/opt/kafka/bin}
DEPLOY=${DEPLOY:-$(cd "$SVC/../../deployments" 2>/dev/null && pwd)}
FE=${FE:-$(cd "$SVC/../../../zoiko-suite-frontend-platform" 2>/dev/null && pwd)}
# A function, not a string. $SVC contains a space on this machine
# ("...\Zoiko comp\...") and an unquoted $Q would word-split the path, handing
# python a truncated filename and failing every contract query.
Q() { python "$SVC/scripts/spec_query.py" "$@"; }

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=99999999-9999-9999-9999-999999999999
ME=33333333-3333-3333-3333-333333333333

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hasf() { if echo "$2" | grep -qF "$3"; then ok "$1"; else bad "$1 (missing $3)"; fi; }
nohasf() { if echo "$2" | grep -qF "$3"; then bad "$1 (found $3)"; else ok "$1"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 25 "$@"; }
ecode() { python -c "import sys,json;print(json.load(sys.stdin).get('error',''))" 2>/dev/null | tr -d '\015'; }
jfield() { python -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null | tr -d '\015'; }
psqlq() { docker exec -i "$PGCONTAINER" psql -U postgres -d configuration_feature_flag -tAc "$1" 2>/dev/null | tr -d '\015'; }
authzq() { docker exec -i "$PGCONTAINER" psql -U postgres -d authorization_svc -tAc "$1" 2>/dev/null | tr -d '\015'; }
uuid() { python -c "import uuid;print(uuid.uuid4())" | tr -d '\015'; }

# The FULL §4 envelope. request_id and source_channel are mandatory on writes,
# not only idempotency-key — omitting one gets 400 envelope_incomplete before a
# handler runs, which would make the business-rule checks below silently
# measure the envelope instead of the rule they name.
wh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H Idempotency-Key:$3 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3 -H Content-Type:application/json"; }
# Reads need identity plus traceability; no idempotency key.
rh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3"; }

echo "==================================================================="
echo " AUDIT — configuration-feature-flag-svc"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo
echo "-- 1. Static analysis ----------------------------------------------"
cd "$SVC" || exit 1
# GOTMPDIR moved off the default temp path: Windows Application Control
# intermittently blocks a freshly linked Go test binary, which is a scan race
# and not a test failure. Section 2 also retries.
export GOTMPDIR="$SVC/.gotmp"
mkdir -p "$GOTMPDIR"
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi
# Line endings, not formatting: this repo is checked out with CRLF on Windows
# and `gofmt -l` reports every such file as unformatted. Normalising first makes
# this check measure what it claims to.
UNFMT=""
for f in $(find . -name '*.go' -not -path './.gotmp/*'); do
  [ -n "$(tr -d '\015' < "$f" | gofmt -l 2>&1)" ] && UNFMT="$UNFMT $f"
done
if [ -z "$UNFMT" ]; then ok "gofmt clean (line-ending independent)"; else bad "gofmt:$UNFMT"; fi

echo
echo "-- 2. Test suites --------------------------------------------------"
gotest() {
  local out
  for _ in 1 2 3 4 5; do
    out=$(go test "$@" 2>&1)
    echo "$out" | grep -q "Application Control" || break
    sleep 2
  done
  echo "$out"
}
# The store suite needs a database, and it must NOT be the one the live service
# is using: two of its tests reach the store-unavailable path by dropping a
# table. They restore the schema in t.Cleanup now, but pointing them at a
# serving database is still how this service lost feature_flags under a live
# demo once.
export TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://postgres:postgres@localhost:5432/configuration_feature_flag_test?sslmode=disable}"
docker exec -i "$PGCONTAINER" psql -U postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='configuration_feature_flag_test'" 2>/dev/null | grep -q 1 \
  || docker exec -i "$PGCONTAINER" psql -U postgres -c "CREATE DATABASE configuration_feature_flag_test" >/dev/null 2>&1
case "$TEST_DATABASE_URL" in
  */configuration_feature_flag\?*|*/configuration_feature_flag)
    bad "TEST_DATABASE_URL points at the SERVING database — the suite drops tables in it" ;;
  *) ok "store suite runs against an isolated database" ;;
esac

VOUT=$(gotest -count=1 -v ./...)
echo "        tests passing: $(echo "$VOUT" | grep -cE '^--- PASS|^    --- PASS')"
chk "tests failing" "$(echo "$VOUT" | grep -cE '^--- FAIL|^    --- FAIL')" "0"
# A suite that SKIPPED is a suite that verified nothing. It reports ok either
# way, which is exactly why this is asserted rather than eyeballed.
chk "no test skipped" "$(echo "$VOUT" | grep -cE '^--- SKIP|^    --- SKIP')" "0"
# The store suite runs against a NOSUPERUSER NOBYPASSRLS role, so the
# row-level-security assertions are made against a database that is actually
# enforcing it. A superuser bypasses RLS unconditionally and would let every
# isolation test pass with every policy dropped.
hasf "store suite runs as NOBYPASSRLS" "$(cat internal/store/pg_store_test.go)" "NOBYPASSRLS"

echo
echo "-- 3. Live health --------------------------------------------------"
chk "/healthz 200"  "$(code "$B/healthz")" "200"
chk "/readyz 200"   "$(code "$B/readyz")"  "200"
chk "/metrics 200"  "$(code "$B/metrics")" "200"
# Readiness must name authorization-svc. It is a readiness dependency, not
# merely a runtime one: every write calls it and this service fails closed, so
# with it unreachable the pool can be healthy while 100% of writes answer 503 —
# and every read keeps working, which is what makes that look like a broken
# console rather than a missing dependency.
READY=$(body "$B/readyz")
hasf "readiness reports authorization-svc" "$READY" "authorization-svc"
hasf "readiness reports database" "$READY" "database"
hasf "readiness reports READY" "$READY" "READY"

echo
echo "-- 4. Envelope contract (ZS-ARCH-SVC-001 §4) -----------------------"
C=$(uuid)
chk "read with no tenant  401" "$(code "$B/v1/flags" -H "X-Principal-Id:$ME")" "401"
chk "write with no idempotency key 400" \
  "$(code -X POST "$B/v1/config" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Request-Id:$C" -H "X-Source-Channel:system" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')" "400"
chk "write with no source channel 400" \
  "$(code -X POST "$B/v1/config" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Request-Id:$C" -H "Idempotency-Key:$C" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')" "400"
ENVBODY=$(body -X POST "$B/v1/config" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Request-Id:$C" -H "X-Source-Channel:system" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')
hasf "envelope refusal names the field" "$ENVBODY" "idempotency_key"
hasf "envelope refusal is structured" "$ENVBODY" "violations"

echo
echo "-- 5. Route surface matches openapi.yaml ---------------------------"
# Both directions. A spec listing a route the service does not serve sends a
# client generator down a dead end; a route the spec omits is a surface nobody
# reviewed.
SPEC_OPS=$(Q openapi-methods openapi.yaml)
for o in \
  "POST /v1/config" "GET /v1/config" "GET /v1/config/{key}" \
  "POST /v1/flags" "GET /v1/flags" "GET /v1/flags/{key}" \
  "POST /v1/config/definitions" "GET /v1/config/definitions/{key}" \
  "POST /v1/config/definitions/{key}/publish" "POST /v1/config/resolve" \
  "PUT /v1/config/overrides/{scope}" "POST /v1/config/changes" \
  "POST /v1/config/changes/{change_id}/approve" "POST /v1/config/changes/{change_id}/activate" \
  "POST /v1/emergency-changes" "POST /v1/emergency-changes/{emergency_change_id}/activate" \
  "POST /v1/runtime/attest" "POST /v1/flags/{key}/release-plans" \
  "POST /v1/flags/{key}/evaluate" \
  "GET /healthz" "GET /readyz" "GET /metrics"; do
  hasf "openapi documents $o" "$SPEC_OPS" "$o"
done
# Anchored. Unanchored, r\.(Get|Post)\( also matches inside
# r.Header.Get("X-Principal-Id") — "Heade" + "r.Get(" — and would report routes
# that do not exist. Put included since ?v1/config/overrides/{scope} is a PUT.
CODE_ROUTES=$(grep -cE '^[[:space:]]*r\.(Get|Post|Patch|Put|Delete)\("' internal/handler/handler.go)
chk "handler registers 19 routes" "$CODE_ROUTES" "19"
# Every error code the handler can emit must appear in the spec, or a client
# branching on the code meets one the contract never mentioned. Two sources:
# literal "error": "<code>" payloads, and the governedCodeStatus map whose keys
# are the AA-001 refusal codes the governed routes emit dynamically.
SPEC_CODES=$(Q openapi-error-codes openapi.yaml)
MISSING=0
for c in $(grep -oE '"error":[[:space:]]*"[a-z_]+"' internal/handler/handler.go | grep -oE '"[a-z_]+"$' | tr -d '"' | sort -u); do
  echo "$SPEC_CODES" | grep -qx "$c" || { MISSING=$((MISSING+1)); echo "        undocumented: $c"; }
done
for c in $(grep -oE '"[a-z0-9_]+":[[:space:]]*http\.Status' internal/handler/handler.go \
           | grep -oE '"[a-z0-9_]+"' | tr -d '"' | sort -u); do
  echo "$SPEC_CODES" | grep -qx "$c" || { MISSING=$((MISSING+1)); echo "        undocumented: $c"; }
done
chk "every emitted error code is in openapi" "$MISSING" "0"

echo
echo "-- 6. Event contract matches asyncapi.yaml -------------------------"
ASPEC=$(Q asyncapi-events asyncapi.yaml)
PUB=$(cat internal/events/publisher.go)
# The outbox CHECK constraint was introduced on event_outbox in 000003 and
# widened to all ten event types in 000008, so the constraint that must admit
# an event lives in whichever file carries the current CHECK — read them all.
MIG=$(cat deployments/migrations/000003_outbox.up.sql deployments/migrations/000008_release_plans.up.sql)
for e in config.updated feature_flag.updated config.snapshot.published \
         config.version.published config.override.activated flag.release.activated \
         flag.kill_switch.activated config.change.verified config.drift.detected \
         config.emergency.expired; do
  hasf "asyncapi documents $e" "$ASPEC" "$e"
  hasf "publisher builds $e" "$PUB" "$e"
  # The outbox CHECK constraint is the only list a deployment can actually
  # violate — an event type the constraint omits fails the INSERT, and because
  # the enqueue shares the write's transaction it fails the WRITE.
  hasf "outbox constraint admits $e" "$MIG" "'$e'"
done
# The partition key must be the aggregate, not the correlation id. Keying on the
# correlation id put two changes to the same entry on different partitions
# whenever they arrived on different requests, so a consumer replaying them
# could apply an older value after a newer one and serve it permanently.
CFGEV=$(sed -n '/^func ConfigUpdated/,/^}/p' internal/events/publisher.go)
hasf "config.updated is keyed on config_id" "$CFGEV" "entry.ConfigID"
hasf "config.updated states global scope" "$CFGEV" "scope_is_global"
FLAGEV=$(sed -n '/^func FeatureFlagUpdated/,/^}/p' internal/events/publisher.go)
hasf "feature_flag.updated is keyed on flag_id" "$FLAGEV" "flag.FlagID"
# Nothing may publish from the handler. That is the defect the outbox removed:
# a Kafka write after the commit, with the error logged and discarded.
# Strip comments first: the handler's own doc comment says "no event publisher
# here any more", which a naive grep for the word reads as the thing it denies.
nohasf "handler publishes nothing directly" "$(grep -v '^[[:space:]]*//' internal/handler/handler.go)" "publisher"

echo
echo "-- 7. The authorization action names -------------------------------"
# An action name nothing in the estate provisions refuses every write 403 while
# every read works — which reads as an under-granted operator rather than as a
# service asking for a name nobody defines. access-control-svc shipped that way.
for a in CONFIGURATION_WRITE CONFIGURATION_GLOBAL_WRITE FEATURE_FLAG_WRITE FEATURE_FLAG_GLOBAL_WRITE; do
  hasf "service defines $a" "$(cat internal/telemetry/domain.go)" "\"$a\""
  GRANTED=$(authzq "SELECT count(*) FROM permission_bundles WHERE permitted_actions::text LIKE '%\"$a\"%' AND active_flag")
  if [ "${GRANTED:-0}" -ge 1 ]; then ok "estate grants $a ($GRANTED bundle)"; else bad "no active bundle grants $a — those writes will 403"; fi
done
# Writing the environment-wide default must be a DIFFERENT grant from writing
# one organisation's value. Both paths used to authorize the same action, so a
# principal provisioned for one organisation could change what every other one
# reads — and RLS cannot catch it, because a global row genuinely belongs to no
# tenant and the policy's WITH CHECK admits a NULL tenant_id unconditionally.
HND=$(cat internal/handler/handler.go)
hasf "config write picks its action from the scope" "$HND" "ActionConfigGlobalWrite"
hasf "flag write picks its action from the scope" "$HND" "ActionFlagGlobalWrite"

echo
echo "-- 8. Write path, live ---------------------------------------------"
KEY="audit.key.$(date +%s).$RANDOM"
C1=$(uuid)
CREATE=$(body -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$C1") \
  -d "{\"key\":\"$KEY\",\"value\":100,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}")
CFG_ID=$(echo "$CREATE" | jfield config_id)
if [ -n "$CFG_ID" ]; then ok "create config 201 ($CFG_ID)"; else bad "create config: $CREATE"; fi
chk "create config status code" \
  "$(code -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"${KEY}_b\",\"value\":1,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}")" "201"
# 201 vs 200. The whole point of an append-only store is that a caller can tell
# a recorded change from a no-op, and collapsing them into "saved" loses it.
chk "unchanged value answers 200 not 201" \
  "$(code -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"value\":100,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}")" "200"
NOOP_ID=$(body -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"value\":100,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}" | jfield config_id)
chk "the no-op returns the existing row" "$NOOP_ID" "$CFG_ID"
# A real change answers 201 with a NEW id, and the predecessor is end-dated
# rather than deleted.
CHANGED_ID=$(body -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"value\":250,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}" | jfield config_id)
if [ -n "$CHANGED_ID" ] && [ "$CHANGED_ID" != "$CFG_ID" ]; then ok "a changed value records a new version"; else bad "a changed value must record a new version"; fi
chk "the predecessor is end-dated, not deleted" \
  "$(psqlq "SELECT effective_to IS NOT NULL FROM config_entries WHERE config_id = '$CFG_ID'")" "t"
chk "exactly one row is effective for the scope" \
  "$(psqlq "SELECT count(*) FROM config_entries WHERE key='$KEY' AND environment='audit' AND effective_to IS NULL")" "1"
# Validation
chk "missing field 400" \
  "$(body -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"value\":1,\"environment\":\"audit\",\"created_by_principal_id\":\"$ME\"}" | ecode)" "missing_field"
chk "rollout out of range 400" \
  "$(body -X POST "$B/v1/flags" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"enabled\":true,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"rollout_percentage\":150,\"created_by_principal_id\":\"$ME\"}" | ecode)" "invalid_field"
# enabled is required and deliberately not defaulted: on an append-only record
# "left blank" and "off" are two different statements.
chk "omitted enabled is refused, not defaulted" \
  "$(body -X POST "$B/v1/flags" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"created_by_principal_id\":\"$ME\"}" | ecode)" "missing_field"
# Another tenant's scope is refused outright rather than written.
chk "foreign tenant_id 403" \
  "$(body -X POST "$B/v1/config" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"key\":\"$KEY\",\"value\":1,\"environment\":\"audit\",\"tenant_id\":\"$OTHER_TEN\",\"created_by_principal_id\":\"$ME\"}" | ecode)" "tenant_scope_mismatch"

echo
echo "-- 9. Feature flags, live ------------------------------------------"
FKEY="audit.flag.$(date +%s).$RANDOM"
FLAG=$(body -X POST "$B/v1/flags" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"key\":\"$FKEY\",\"enabled\":true,\"environment\":\"audit\",\"tenant_id\":\"$TEN\",\"rollout_percentage\":25,\"created_by_principal_id\":\"$ME\"}")
FLAG_ID=$(echo "$FLAG" | jfield flag_id)
if [ -n "$FLAG_ID" ]; then ok "create flag 201"; else bad "create flag: $FLAG"; fi
chk "flag read back at the same scope 200" \
  "$(code "$B/v1/flags/$FKEY?environment=audit&tenant_id=$TEN" $(rh "$TEN" "$ME" "$(uuid)"))" "200"
# The exact-tuple rule. A tenant miss does NOT fall back to the global default,
# so a 404 here says nothing about whether a global value exists — which is the
# single most misread response this service produces.
chk "a different scope is a 404, not a fallback" \
  "$(code "$B/v1/flags/$FKEY?environment=audit" $(rh "$TEN" "$ME" "$(uuid)"))" "404"
chk "404 names the resource" \
  "$(body "$B/v1/flags/$FKEY?environment=audit" $(rh "$TEN" "$ME" "$(uuid)") | ecode)" "feature_flag_not_found"
chk "missing environment 400" \
  "$(body "$B/v1/flags/$FKEY" $(rh "$TEN" "$ME" "$(uuid)") | ecode)" "missing_field"

echo
echo "-- 10. Read scoping ------------------------------------------------"
R=$(uuid)
LIST=$(body "$B/v1/config?environment=audit" $(rh "$TEN" "$ME" "$R"))
hasf "list returns this tenant's entries" "$LIST" "$KEY"
# An absent tenant filter used to mean "no filter", i.e. every tenant's
# configuration returned to any caller. It now means this tenant plus the
# globals that apply to it.
OTHERLIST=$(body "$B/v1/config?environment=audit" $(rh "$OTHER_TEN" "$ME" "$R"))
nohasf "another tenant cannot see it" "$OTHERLIST" "$KEY"
chk "naming a foreign tenant on a list is 403" \
  "$(body "$B/v1/config?tenant_id=$OTHER_TEN" $(rh "$TEN" "$ME" "$R") | ecode)" "tenant_scope_mismatch"
# Never null. A client that has to handle both [] and null for "nothing" has two
# code paths where one belongs.
chk "an empty list is [] not null" "$(body "$B/v1/config?environment=nothing-here" $(rh "$TEN" "$ME" "$R"))" "[]"

echo
echo "-- 11. Outbox, end to end ------------------------------------------"
# The event and the state change are one commit. Delivery is a separate,
# retryable problem — but it must actually happen.
chk "config.updated was enqueued for the create" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='config.updated' AND aggregate_key='$CFG_ID'")" "1"
chk "the changed value enqueued its own event" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='config.updated' AND aggregate_key='$CHANGED_ID'")" "1"
# The no-op must NOT have enqueued anything: re-asserting a value already in
# force is not a new fact, and a consumer invalidating its cache on one would be
# doing it for a change that did not happen.
chk "the no-op enqueued nothing" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='config.updated' AND aggregate_key='$NOOP_ID' AND aggregate_key <> '$CFG_ID'")" "0"
chk "feature_flag.updated was enqueued" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='feature_flag.updated' AND aggregate_key='$FLAG_ID'")" "1"
# The outbox row belongs to the CALLER, not to the scope. A global write has no
# scope tenant, and a row owned by nobody is refused by the outbox's own policy.
chk "the outbox row is owned by the caller" \
  "$(psqlq "SELECT tenant_id FROM event_outbox WHERE aggregate_key='$CFG_ID'")" "$TEN"
# The relay drained them. A backlog that never clears is a set of consumers that
# is never told anything, with no 5xx and no latency to show for it.
sleep 2
chk "the relay published everything" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND created_at < now() - interval '5 seconds'")" "0"
chk "no outbox row is stuck retrying" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND attempts > 3")" "0"
# And it reached Kafka, carrying the fields a consumer reads.
KOUT=$(docker exec -i "$KAFKACONTAINER" bash -lc "$KAFKABIN/kafka-console-consumer.sh --bootstrap-server localhost:9094 --topic zoiko.configuration.events --from-beginning --timeout-ms 15000" 2>/dev/null)
# Matched loosely on purpose. The envelope is held in a JSONB column, so
# Postgres normalises it on the way through — keys reordered, ": " separators —
# and the bytes on the topic are NOT the bytes Go marshalled. Semantically
# identical and invisible to any JSON parser, but an exact-byte grep fails
# against a message plainly containing the field.
hasf "config.updated reached Kafka" "$KOUT" "$CFG_ID"
if echo "$KOUT" | grep -qE '"source_service": ?"configuration-feature-flag-svc"'; then
  ok "the Kafka envelope names this service"
else
  bad "the Kafka envelope names this service"
fi
hasf "the Kafka envelope carries scope_is_global" "$KOUT" '"scope_is_global"'

echo
echo "-- 12. Telemetry ---------------------------------------------------"
M=$(body "$B/metrics")
for s in configuration_config_writes_total configuration_flag_writes_total \
         configuration_authz_decisions_total configuration_global_scope_writes_total \
         configuration_governed_writes_total configuration_outbox_pending \
         configuration_outbox_published_total configuration_outbox_failures_total \
         configuration_outbox_oldest_age_seconds \
         readiness_up; do
  hasf "exports $s" "$M" "$s"
done
# Pre-created at zero. A series that has never been observed and one reading
# zero are indistinguishable to an alert expression, so a rule written to catch
# the FIRST occurrence of something stays silent through exactly the event it
# exists for.
hasf "no_change outcome series exists" "$M" 'outcome="no_change"'
hasf "forbidden outcome series exists" "$M" 'outcome="forbidden"'
hasf "authz_unavailable outcome series exists" "$M" 'outcome="authz_unavailable"'
hasf "the global-write action is labelled" "$M" 'action="CONFIGURATION_GLOBAL_WRITE"'
hasf "the governed-writes counter is pre-created with both labels" "$M" 'configuration_governed_writes_total'
chk "readiness gauge is 1" "$(echo "$M" | grep -E '^readiness_up' | awk '{print $2}')" "1"

echo
echo "-- 13. Alert rules -------------------------------------------------"
if [ -f "$DEPLOY/prometheus-rules.yml" ]; then
  hasf "prometheus scrapes this service" "$(cat "$DEPLOY/prometheus.yml")" "configuration-feature-flag-svc:8086"
  NALERTS=$(Q alert-count "$DEPLOY/prometheus-rules.yml")
  if [ "${NALERTS:-0}" -ge 5 ]; then ok "alert group has $NALERTS rules"; else bad "alert group has only ${NALERTS:-0} rules"; fi
  # An alert whose expression names a series that does not exist never fires,
  # and looks identical to an alert that is simply not firing.
  for s in $(Q alert-series "$DEPLOY/prometheus-rules.yml"); do
    hasf "alert series exists on /metrics: $s" "$M" "$s"
  done
  # And an alert that points at a runbook section that does not exist sends
  # whoever is paged to a page that is not there.
  RB=$(cat "$SVC/RUNBOOK.md")
  for sec in $(Q alert-runbook-sections "$DEPLOY/prometheus-rules.yml"); do
    hasf "RUNBOOK has section $sec" "$RB" "### $sec"
  done
else
  bad "deployments/prometheus-rules.yml not found"
fi

echo
echo "-- 14. Release artifacts -------------------------------------------"
for f in openapi.yaml asyncapi.yaml RUNBOOK.md RELEASE_CERTIFICATE.md progress.md context.md scripts/audit.sh; do
  if [ -f "$SVC/$f" ]; then ok "$f present"; else bad "$f missing"; fi
done

echo
echo "-- 15. Migrations & RLS --------------------------------------------"
# Read back from pg_policy, not from the migration file. A migration that was
# written correctly and never applied looks exactly like one that was applied.
for t in config_entries feature_flags event_outbox; do
  chk "$t FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='$t'")" "t"
  # A USING-only policy governs what is VISIBLE, not what may be WRITTEN — a
  # caller could insert a row attributed to another tenant and then be unable to
  # see it.
  chk "$t policy has WITH CHECK" "$(psqlq "SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relname='$t' AND p.polwithcheck IS NOT NULL")" "1"
done
# NULLIF is not decoration. Postgres keeps a custom GUC in the SESSION after a
# transaction-local SET is reset, with '' as its value, so on any pooled
# connection that has already served one request the setting reads as the empty
# string rather than NULL — and ''::uuid raises, turning a scoped read into a
# 500.
hasf "config policy guards the empty GUC" "$(psqlq "SELECT pg_get_expr(polqual,polrelid) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relname='config_entries'")" "NULLIF"
# The relay's one deliberate cross-tenant exemption, named rather than implied.
hasf "outbox policy admits the named relay" "$(psqlq "SELECT pg_get_expr(polqual,polrelid) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relname='event_outbox'")" "app.outbox_relay"
# The concurrency backstop the upsert relies on, not just an optimisation.
for t in config_entries feature_flags; do
  chk "$t has the one-effective-row index" "$(psqlq "SELECT count(*) FROM pg_indexes WHERE indexname='idx_${t}_one_effective_per_scope'")" "1"
done
chk "rollout_percentage CHECK present" "$(psqlq "SELECT count(*) FROM pg_constraint WHERE conname='chk_feature_flags_rollout_percentage_range'")" "1"
chk "no scope has two effective config rows" \
  "$(psqlq "SELECT count(*) FROM (SELECT 1 FROM config_entries WHERE effective_to IS NULL GROUP BY key, environment, COALESCE(tenant_id,'00000000-0000-0000-0000-000000000000'::uuid) HAVING count(*)>1) x")" "0"
# Every up has a down.
UPS=$(ls "$SVC/deployments/migrations"/*.up.sql | wc -l)
DOWNS=$(ls "$SVC/deployments/migrations"/*.down.sql | wc -l)
chk "every up migration has a down" "$UPS" "$DOWNS"

echo
echo "-- 16. Frontend ----------------------------------------------------"
if [ -n "$SKIP_FE" ] || [ -z "$FE" ] || [ ! -d "$FE/node_modules" ]; then
  echo "        skipped (SKIP_FE set or console node_modules absent)"
else
  CLIENT="$FE/lib/api/configuration.ts"
  if [ -f "$CLIENT" ]; then ok "console client present"; else bad "console client present"; fi
  EX=$(cat "$CLIENT")
  # The client must call the routes the service actually serves.
  hasf "client uses /v1/config" "$EX" "/v1/config"
  hasf "client uses /v1/flags" "$EX" "/v1/flags"
  # The client must explain every refusal this service can produce. Most are
  # rules the console cannot check for itself, which is exactly when a bare
  # error string leaves the reader with nothing to do next.
  while IFS= read -r phrase; do
    [ -z "$phrase" ] && continue
    if echo "$EX" | grep -qF "$phrase"; then ok "explains: $phrase"; else bad "explains: $phrase"; fi
  done <<'PHRASES'
rollout_percentage
missing_field
invalid_json
config_entry_not_found
feature_flag_not_found
scope_race_conflict
tenant_scope_mismatch
authorization_denied
authz_unavailable
store_unavailable
PHRASES
  # 201 vs 200 must survive to the caller, or the console cannot tell a recorded
  # change from a no-op — which is the one distinction this service exists for.
  grep -q 'status: number' "$FE/lib/api/client.ts" && ok "write result carries the status" || bad "write result carries the status"
  grep -q 'result.status === 201' "$FE/app/admin/settings/actions.ts" && ok "console distinguishes a change from a no-op" || bad "console distinguishes a change from a no-op"
  # A flag is not described by `enabled` alone: one enabled at 20% is on for a
  # fifth of people, and one DISABLED at 20% is off for everyone.
  grep -q 'explainFlag' "$CLIENT" && ok "console explains a flag in plain English" || bad "console explains a flag in plain English"

  TSOUT=$(cd "$FE" && npx tsc --noEmit -p tsconfig.json 2>&1 | grep -v 'npm notice')
  if [ -z "$TSOUT" ]; then ok "console typecheck clean"; else bad "console typecheck clean"; fi

  if [ -f "$FE/e2e/configuration.spec.ts" ]; then
    ok "e2e spec exists"
    if [ -f "$FE/e2e/mock/configuration-service.mjs" ]; then ok "e2e mock exists"; else bad "e2e mock exists"; fi
    SPECS=$(cd "$FE" && npx playwright test e2e/configuration.spec.ts --reporter=line 2>&1)
    echo "$SPECS" | tail -2
    if echo "$SPECS" | grep -qE '[0-9]+ passed' && ! echo "$SPECS" | grep -qE '[0-9]+ failed'; then
      ok "e2e configuration specs pass"
    else
      bad "e2e configuration specs pass"
    fi
  else
    bad "e2e spec exists"
  fi
fi

echo
echo "==================================================================="
TOTAL=$((PASS+FAIL))
if [ "$TOTAL" -gt 0 ]; then
  PCT=$(python -c "print(f'{100*$PASS/$TOTAL:.1f}')" | tr -d '\015')
else
  PCT=0
fi
echo " RESULT: $PASS passed, $FAIL failed  of $TOTAL  (${PCT}%)"
echo "==================================================================="
[ "$FAIL" -eq 0 ] || exit 1
