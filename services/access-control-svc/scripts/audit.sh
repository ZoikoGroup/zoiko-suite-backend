#!/usr/bin/env bash
# Audit test — access-control-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# running stack rather than against stubs. Every check below either exercises a
# documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8137), postgres reachable as
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
B=${B:-http://localhost:8137}
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
ENTITY=22222222-2222-2222-2222-222222222222
ME=33333333-3333-3333-3333-333333333333

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hasf() { if echo "$2" | grep -qF "$3"; then ok "$1"; else bad "$1 (missing $3)"; fi; }
nohasf() { if echo "$2" | grep -qF "$3"; then bad "$1 (found $3)"; else ok "$1"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 25 "$@"; }
ecode() { python -c "import sys,json;print(json.load(sys.stdin).get('error_code',''))" 2>/dev/null | tr -d '\015'; }
jfield() { python -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null | tr -d '\015'; }
psqlq() { docker exec -i "$PGCONTAINER" psql -U postgres -d access_control -tAc "$1" 2>/dev/null | tr -d '\015'; }
authzq() { docker exec -i "$PGCONTAINER" psql -U postgres -d authorization_svc -tAc "$1" 2>/dev/null | tr -d '\015'; }
uuid() { python -c "import uuid;print(uuid.uuid4())" | tr -d '\015'; }

# The FULL §4 envelope. request_id and source_channel are mandatory on writes,
# not only idempotency-key — omitting one gets 400 envelope_incomplete before a
# handler runs, which would make the business-rule checks below silently
# measure the envelope instead of the rule they name.
wh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Legal-Entity-Id:$ENTITY -H Idempotency-Key:$3 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3 -H Content-Type:application/json"; }
# Reads need identity plus traceability; no idempotency key, no entity.
rh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3"; }

echo "==================================================================="
echo " AUDIT — access-control-svc"
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
OUT=$(gotest -count=1 ./...)
chk "unit packages failing" "$(echo "$OUT" | grep -cE '^FAIL')" "0"
VOUT=$(gotest -count=1 -v ./...)
echo "        unit tests passing: $(echo "$VOUT" | grep -cE '^--- PASS|^    --- PASS')"
chk "unit tests failing" "$(echo "$VOUT" | grep -cE '^--- FAIL|^    --- FAIL')" "0"
# A suite that SKIPPED is a suite that verified nothing. It reports ok either
# way, which is exactly why this is asserted rather than eyeballed.
chk "no unit test skipped" "$(echo "$VOUT" | grep -cE '^--- SKIP|^    --- SKIP')" "0"

# The store suite runs against embedded Postgres under a NOSUPERUSER
# NOBYPASSRLS role, so the row-level-security assertions are made against a
# database that is actually enforcing it. A superuser bypasses RLS
# unconditionally and would let every isolation test pass with every policy
# dropped.
IOUT=$(gotest -tags=integration -count=1 -v -timeout=300s ./internal/store/)
echo "        store tests passing: $(echo "$IOUT" | grep -cE '^--- PASS|^    --- PASS')"
chk "store tests failing" "$(echo "$IOUT" | grep -cE '^--- FAIL|^    --- FAIL')" "0"
chk "no store test skipped" "$(echo "$IOUT" | grep -cE '^--- SKIP|^    --- SKIP')" "0"
hasf "store suite runs as NOBYPASSRLS" "$(cat internal/store/tenant_isolation_test.go)" "NOBYPASSRLS"

echo
echo "-- 3. Live health --------------------------------------------------"
chk "/healthz 200"  "$(code "$B/healthz")" "200"
chk "/readyz 200"   "$(code "$B/readyz")"  "200"
chk "/metrics 200"  "$(code "$B/metrics")" "200"
# Readiness must name authorization-svc. It is a readiness dependency, not
# merely a runtime one: every write calls it twice and this service fails
# closed, so with it unreachable the pool can be healthy while 100% of writes
# answer 503.
READY=$(body "$B/readyz")
hasf "readiness reports authorization-svc" "$READY" "authorization-svc"
hasf "readiness reports database" "$READY" "database"
hasf "readiness reports READY" "$READY" "READY"

echo
echo "-- 4. Envelope contract (ZS-ARCH-SVC-001 §4) -----------------------"
C=$(uuid)
chk "read with no tenant    401" "$(code "$B/v1/role-definitions/" -H "X-Principal-Id:$ME")" "401"
chk "read with no actor     401" "$(code "$B/v1/role-definitions/" -H "X-Tenant-Id:$TEN")" "401"
# Both halves of actor_subject_id. identity-context-svc reads the bundle
# collection on its session-resolution hot path and identifies itself,
# correctly, as a WORKLOAD. It was refused 401, which its resolver reported
# fail-closed as "upstream dependency unavailable" — so every context
# resolution in the stack answered 503 and the cause looked like an outage.
chk "read as a workload     200" \
  "$(code "$B/v1/role-definitions/" -H "X-Tenant-Id:$TEN" -H "X-Workload-Id:identity-context-svc" -H "X-Request-Id:$C" -H "X-Source-Channel:system" -H "X-Correlation-ID:$C")" "200"
# Writes are gated by the middleware ahead of any handler.
chk "write with no idempotency key 400" \
  "$(code -X POST "$B/v1/role-definitions/" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Legal-Entity-Id:$ENTITY" -H "X-Request-Id:$C" -H "X-Source-Channel:system" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')" "400"
chk "write with no source channel 400" \
  "$(code -X POST "$B/v1/role-definitions/" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Legal-Entity-Id:$ENTITY" -H "X-Request-Id:$C" -H "Idempotency-Key:$C" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')" "400"
ENVBODY=$(body -X POST "$B/v1/role-definitions/" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Legal-Entity-Id:$ENTITY" -H "X-Request-Id:$C" -H "X-Source-Channel:system" -H "X-Correlation-ID:$C" -H "Content-Type:application/json" -d '{}')
hasf "envelope refusal names the field" "$ENVBODY" "idempotency_key"
hasf "envelope refusal is structured" "$ENVBODY" "violations"

echo
echo "-- 5. Route surface matches openapi.yaml ---------------------------"
# Both directions. A spec listing a route the service does not serve sends a
# client generator down a dead end; a route the spec omits is a surface nobody
# reviewed.
SPEC_OPS=$(Q openapi-methods openapi.yaml)
for o in \
  "GET /v1/role-definitions/" "POST /v1/role-definitions/" \
  "GET /v1/role-definitions/{role_definition_id}" "PATCH /v1/role-definitions/{role_definition_id}" \
  "GET /v1/role-definitions/{role_definition_id}/permission-bundles" \
  "POST /v1/role-definitions/{role_definition_id}/permission-bundles" \
  "GET /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}" \
  "PATCH /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}" \
  "DELETE /v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}" \
  "GET /v1/permission-bundles/" "GET /healthz" "GET /readyz" "GET /metrics"; do
  hasf "openapi documents $o" "$SPEC_OPS" "$o"
done
# Anchored. Unanchored, r\.(Get|Post)\( also matches inside
# r.Header.Get("X-Principal-Id") — "Heade" + "r.Get(" — and would report routes
# that do not exist.
CODE_ROUTES=$(grep -cE '^[[:space:]]*r\.(Get|Post|Patch|Delete)\("' internal/handler/handler.go)
chk "handler registers 10 routes" "$CODE_ROUTES" "10"
# Every error_code the handler can emit must appear in the spec, or a client
# branching on the code meets one the contract never mentioned.
SPEC_CODES=$(Q openapi-error-codes openapi.yaml)
MISSING=0
# handler.go, governance.go (writeError, g.refuse and the fail() mapping) and
# the Idempotency-Key middleware.
EMITTED=$( { grep -ohE 'writeError\(w, [^,]+, "[a-z_]+"' internal/handler/handler.go internal/handler/governance.go | grep -oE '"[a-z_]+"$';
  grep -ohE 'http\.Status[A-Za-z]+, "[a-z_]+"' internal/handler/governance.go | grep -oE '"[a-z_]+"$';
  grep -ohE 'code = [^,]+, "[a-z_]+"' internal/handler/governance.go | grep -oE '"[a-z_]+"$';
  grep -ohE 'writeErr\(w, [^,]+, "[A-Za-z_]+"' internal/idempotency/middleware.go | grep -oE '"[A-Za-z_]+"$'; } | tr -d '"' | sort -u)
for c in $EMITTED; do
  echo "$SPEC_CODES" | grep -qx "$c" || { MISSING=$((MISSING+1)); echo "        undocumented: $c"; }
done
chk "every emitted error_code is in openapi" "$MISSING" "0"

echo
echo "-- 6. Event contract matches asyncapi.yaml -------------------------"
ASPEC=$(Q asyncapi-events asyncapi.yaml)
PUB=$(cat internal/events/publisher.go)
MIG=$(cat deployments/migrations/000004_outbox_and_bundle_code_unique.up.sql)
PUB="$PUB $(cat internal/events/governance.go)"
MIG="$MIG $(cat deployments/migrations/000010_assignment_requests.up.sql)"
for e in role.created role.updated permission.bundle.updated iam.role.published iam.assignment.requested          iam.assignment.granted iam.assignment.revoked iam.access_review.started iam.access_review.completed; do
  hasf "asyncapi documents $e" "$ASPEC" "$e"
  hasf "publisher emits $e" "$PUB" "$e"
  # The outbox CHECK constraint is the only list a deployment can actually
  # violate — an event type the constraint omits fails the INSERT, and because
  # the enqueue shares the write's transaction it fails the WRITE.
  hasf "outbox constraint admits $e" "$MIG" "'$e'"
done
# The payload field the CONSUMER reads, which is a different thing from the
# fields this service happens to put there. identity-context-svc's
# handleRoleUpdated unmarshals {"role_id": ...} and drops the event when it is
# empty; this service emitted only role_definition_id, so every role.updated
# was answered with "names no role_id — cannot revoke".
ROLEUPD=$(sed -n '/^func RoleUpdated/,/^}/p' internal/events/publisher.go)
hasf "role.updated payload carries role_id" "$ROLEUPD" '"role_id"'
BUNDUPD=$(sed -n '/^func BundleUpdated/,/^}/p' internal/events/publisher.go)
hasf "permission.bundle.updated names its role" "$BUNDUPD" '"role_id"'
# A bundle change must also emit role.updated: that is the only name the
# session-revoking consumer dispatches on, so the bundle event alone revokes
# nothing.
hasf "a bundle change also enqueues role.updated" "$(cat internal/store/pg_store.go)" "events.RoleUpdated(forRole, actorID)"

echo
echo "-- 7. The authorization action name --------------------------------"
# THE defect this service shipped with. The handler asked for
# "ACCESS_ROLE_MANAGE", a name nothing in this estate provisions: the seed
# attaches ACCESS_CONTROL_FULL with permitted_actions ["ROLE_MANAGE"], the
# console tells the operator a 403 means "you hold no ROLE_MANAGE grant", and
# the live bundle grants ROLE_MANAGE. So every write was refused 403 while every
# read worked — which reads as an under-granted operator, not as a service
# asking for an action nobody defines.
ACTION=$(grep -oE 'ActionRoleManage = "[A-Z_]+"' internal/telemetry/domain.go | grep -oE '"[A-Z_]+"' | tr -d '"')
chk "service authorizes against ROLE_MANAGE" "$ACTION" "ROLE_MANAGE"
nohasf "no ACCESS_ROLE_MANAGE anywhere in the service" "$(grep -rh 'ACCESS_ROLE_MANAGE' --include=*.go . | grep -v '^//' | grep -v 'NOT "ACCESS_ROLE_MANAGE"')" "= \"ACCESS_ROLE_MANAGE\""
# And the estate actually grants it.
GRANTED=$(authzq "SELECT count(*) FROM permission_bundles WHERE permitted_actions::text LIKE '%\"ROLE_MANAGE\"%' AND active_flag")
if [ "${GRANTED:-0}" -ge 1 ]; then ok "authorization-svc has an active bundle granting ROLE_MANAGE ($GRANTED)"; else bad "no active bundle grants ROLE_MANAGE — every write will 403"; fi

echo
echo "-- 8. Write path, live ---------------------------------------------"
CODE="AUDIT_$(date +%s)_$RANDOM"
C1=$(uuid)
CREATE=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$C1") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$CODE\",\"role_name\":\"Audit role\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$C1\"}")
ROLE_ID=$(echo "$CREATE" | jfield role_definition_id)
if [ -n "$ROLE_ID" ]; then ok "create role 201 ($ROLE_ID)"; else bad "create role: $CREATE"; fi
chk "create role status code" \
  "$(code -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"${CODE}_B\",\"role_name\":\"Audit role B\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$(uuid)\"}")" "201"
# 201 vs 200. A client that cannot tell a created role from a returned one has
# no use for an idempotency key, and this service answered 201 to both.
chk "replay answers 200 not 201" \
  "$(code -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$C1") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$CODE\",\"role_name\":\"Audit role\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$C1\"}")" "200"
REPLAY_ID=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$C1") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$CODE\",\"role_name\":\"Audit role\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$C1\"}" | jfield role_definition_id)
chk "replay resolves to the original role" "$REPLAY_ID" "$ROLE_ID"
# A duplicate code is the caller's to resolve in a second. It used to arrive as
# 503 store_unavailable with a raw Postgres SQLSTATE in the body.
DUP=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$CODE\",\"role_name\":\"Dup\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$(uuid)\"}")
chk "duplicate role_code is a conflict" "$(echo "$DUP" | ecode)" "role_code_exists"
chk "duplicate role_code is 409" \
  "$(code -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$CODE\",\"role_name\":\"Dup\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$(uuid)\"}")" "409"
nohasf "conflict body leaks no SQLSTATE" "$DUP" "SQLSTATE"
# The refused create must not have left an orphan role in authorization-svc.
ORPHANS=$(authzq "SELECT count(*) FROM roles WHERE role_code = '$CODE'")
chk "a refused duplicate provisions no orphan" "$ORPHANS" "1"
# An unknown scope type is refused rather than stored.
chk "unknown role_scope_type 400" \
  "$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$(uuid)\",\"role_name\":\"x\",\"role_scope_type\":\"GALAXY\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "invalid_scope_type"

echo
echo "-- 9. Provisioning actually reached authorization-svc --------------"
# The whole point of this service. A role recorded here that was never
# provisioned there is a catalogue entry describing enforcement that does not
# exist.
# Two facts in one query, so a role that exists but is inactive cannot pass.
# Concatenating a boolean renders it as 'true'/'false', not the 't'/'f' psql
# prints for a bare boolean column — a difference that cost this check one
# false FAIL before it was written this way.
REMOTE=$(authzq "SELECT role_code || '|' || active_flag FROM roles WHERE role_id = '$ROLE_ID'")
chk "role exists and is active in authorization-svc" "$REMOTE" "$CODE|true"
BC=$(uuid)
BUNDLE=$(body -X POST "$B/v1/role-definitions/$ROLE_ID/permission-bundles" $(wh "$TEN" "$ME" "$BC") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"AUDIT_BUNDLE\",\"permitted_actions\":[\"supplier_invoice.create\"],\"correlation_id\":\"$BC\"}")
BUNDLE_ID=$(echo "$BUNDLE" | jfield bundle_id)
if [ -n "$BUNDLE_ID" ]; then ok "attach bundle 201"; else bad "attach bundle: $BUNDLE"; fi
chk "bundle exists in authorization-svc" \
  "$(authzq "SELECT bundle_code FROM permission_bundles WHERE role_id = '$ROLE_ID'")" "AUDIT_BUNDLE"
# Two local bundles sharing a code on one role pointed at ONE remote grant:
# creating the second replaced the first's actions there, and detaching either
# retired the bundle both described.
chk "duplicate bundle_code is 409" \
  "$(code -X POST "$B/v1/role-definitions/$ROLE_ID/permission-bundles" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"AUDIT_BUNDLE\",\"permitted_actions\":[\"supplier_invoice.edit\"],\"correlation_id\":\"$(uuid)\"}")" "409"
chk "duplicate bundle_code error code" \
  "$(body -X POST "$B/v1/role-definitions/$ROLE_ID/permission-bundles" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"AUDIT_BUNDLE\",\"permitted_actions\":[\"supplier_invoice.edit\"],\"correlation_id\":\"$(uuid)\"}" | ecode)" "bundle_code_exists"
# Retirement propagates BEFORE it is recorded. Retiring here without telling
# authorization-svc left the role fully live while this register displayed
# RETIRED — a record that disagrees with the enforcement it describes.
chk "retire role 200" \
  "$(code -X PATCH "$B/v1/role-definitions/$ROLE_ID" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"status\":\"RETIRED\",\"correlation_id\":\"$(uuid)\"}")" "200"
chk "retirement cleared active_flag in authorization-svc" \
  "$(authzq "SELECT active_flag FROM roles WHERE role_id = '$ROLE_ID'")" "f"
chk "register agrees the role is retired" \
  "$(psqlq "SELECT status FROM role_definitions WHERE role_definition_id = '$ROLE_ID'")" "RETIRED"
# Reactivation restores exactly what was suspended: assignments are never
# removed, so this is reversible.
chk "reactivate 200" \
  "$(code -X PATCH "$B/v1/role-definitions/$ROLE_ID" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"status\":\"ACTIVE\",\"correlation_id\":\"$(uuid)\"}")" "200"
chk "reactivation restored active_flag" \
  "$(authzq "SELECT active_flag FROM roles WHERE role_id = '$ROLE_ID'")" "t"

echo
echo "-- 9b. Pre-provisioning guards, live (6 Oct 2026) -----------------"
# Until 6 Oct the SoD client sent a body authorization-svc refuses (400, no
# candidate_actions) and every error became 403 sod_conflict, so NO role or
# bundle could be created; the protected guard read a route nothing serves and
# skipped itself. These checks prove both guards decide, and decide correctly.
#
# Until 6 Oct the dev database carried no SoD rules (the §10.1 baseline was
# unseeded) and this section installed a tenant fixture rule. authorization-svc
# 000019 now seeds the baseline globally, so the checks below run against the
# real Payment Preparer / Payment Releaser rule, in the §5 names the permission
# taxonomy (000008) registers.
chk "the §10.1 baseline is seeded globally in authorization-svc"   "$(authzq "SELECT count(*) FROM sod_rules WHERE tenant_id IS NULL AND active_flag AND sod_rule_id::text LIKE '00000000-0000-0000-0101-%'")" "9"
authzq "DELETE FROM sod_rules WHERE action_a='AUDIT_SOD_A' AND action_b='AUDIT_SOD_B'" >/dev/null
GCODE="AUDIT_G_$(date +%s)_$RANDOM"
GR=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"$GCODE\",\"role_name\":\"Guard role\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$(uuid)\"}")
GROLE=$(echo "$GR" | jfield role_definition_id)
if [ -n "$GROLE" ]; then ok "role create is not SoD-refused"; else bad "role create: $GR"; fi
gbundle() { # $1 corr, $2 code, $3 actions-json
  body -X POST "$B/v1/role-definitions/$GROLE/permission-bundles" $(wh "$TEN" "$ME" "$1") \
    -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"$2\",\"permitted_actions\":$3,\"correlation_id\":\"$1\"}"
}
GA=$(uuid); GB=$(uuid); GS=$(uuid); GP=$(uuid); GP2=$(uuid)
chk "first half of a conflicting pair is accepted" "$(gbundle "$GA" G_A '["payment.prepare"]' | jfield bundle_code)" "G_A"
GBODY=$(gbundle "$GB" G_B '["payment.release"]')
chk "the other half, in a SECOND bundle of the same role, is refused" "$(echo "$GBODY" | ecode)" "sod_conflict"
hasf "the refusal names the pair" "$GBODY" "payment.release conflicts with payment.prepare"
chk "both halves in one bundle are refused" "$(gbundle "$GS" G_S '["payment.prepare","payment.release"]' | ecode)" "sod_conflict"
chk "a refused bundle never reaches authorization-svc" \
  "$(authzq "SELECT count(*) FROM permission_bundles WHERE role_id = '$GROLE'")" "1"
chk "iam.role.manage is protected" "$(gbundle "$GP" G_P '["supplier_invoice.match","iam.role.manage"]' | ecode)" "protected_action"
chk "PLATFORM_ADMIN is protected" "$(gbundle "$GP2" G_P2 '["PLATFORM_ADMIN"]' | ecode)" "protected_action"
chk "refusals are recorded as evidence" \
  "$(psqlq "SELECT string_agg(refusal_reason, ',' ORDER BY refusal_reason) FROM refused_escalations WHERE correlation_id IN ('$GB','$GS','$GP','$GP2')")" \
  "protected_action,protected_action,sod_conflict,sod_conflict"
# A caller holding ROLE_MANAGE but not authorization-svc's iam.role.manage is
# refused THERE. That 403 used to leave this service as 503
# authz_admin_unavailable: an outage page for a missing grant.
RMO=77777777-0000-0000-0000-000000000001; RMP=77777777-0000-0000-0000-0000000000aa
authzq "INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, created_by_principal_id)
          VALUES ('$RMO','$TEN','AUDIT_ROLE_MANAGE_ONLY','Role manage only','LEGAL_ENTITY','audit') ON CONFLICT DO NOTHING;
        INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions)
          SELECT '$RMO','RM_ONLY','[\"ROLE_MANAGE\"]' WHERE NOT EXISTS (SELECT 1 FROM permission_bundles WHERE role_id='$RMO');
        INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
          SELECT '$RMP','$RMO','$ENTITY', now() - interval '1 minute', 'audit'
          WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments WHERE principal_id='$RMP')" >/dev/null
sleep 6 # authorization-svc's grant cache
GF=$(uuid)
GFB=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$RMP" "$GF") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUDIT_FORBID_$RANDOM\",\"role_name\":\"x\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$GF\"}")
chk "authorization-svc's refusal is 403 provisioning_forbidden, not an outage" "$(echo "$GFB" | ecode)" "provisioning_forbidden"
hasf "and keeps authorization-svc's reason" "$GFB" "iam.role.manage is required"
chk "and is recorded" "$(psqlq "SELECT refusal_reason FROM refused_escalations WHERE correlation_id='$GF'")" "provisioning_forbidden"

echo
echo "-- 9c. Governance surface, live (6 Oct 2026 gap closure) -----------"
# The rows the 23 Sep audit recorded as unbuilt: §5 taxonomy, §9 system role
# templates and the 21 §9.1 archetypes, §9/§21 governed assignment requests,
# §9/§24 access review attestation, and Idempotency-Key binding. Every check
# runs against the live service and authorization-svc.
APPROVER=66666666-6666-6666-6666-666666666666
RUNID=$(date +%s)_$RANDOM

# §5 taxonomy.
PDEFS=$(body "$B/v1/permission-definitions" $(rh "$TEN" "$ME" "$(uuid)"))
# 65 §5 names (000008) plus iam.assignment.approve_privileged (000013), the
# security-approval authority §9's risk-based approval needs.
chk "permission taxonomy lists the §5 names" "$(echo "$PDEFS" | python -c "import sys,json;print(sum(1 for d in json.load(sys.stdin) if d['naming']=='TAXONOMY'))" | tr -d '\015')" "66"
hasf "payment.release is CRITICAL in the taxonomy" "$(body "$B/v1/permission-definitions?search=payment.release" $(rh "$TEN" "$ME" "$(uuid)"))" '"risk_tier":"CRITICAL"'
TXR=$(gbundle "$(uuid)" G_T '["payment.relase"]')
chk "a misspelt action is refused (unknown_permission)" "$(echo "$TXR" | ecode)" "unknown_permission"
hasf "the refusal names the action" "$TXR" "payment.relase"
chk "a taxonomy refusal never reaches authorization-svc" \
  "$(authzq "SELECT count(*) FROM permission_bundles WHERE role_id = '$GROLE' AND bundle_code='G_T'")" "0"

# §9 templates.
TPL=$(body "$B/v1/role-templates/" $(rh "$TEN" "$ME" "$(uuid)"))
chk "the 21 §9.1 archetypes are seeded" "$(echo "$TPL" | python -c "import sys,json;print(sum(1 for t in json.load(sys.stdin) if t['is_archetype']))" | tr -d '\015')" "21"
chk "template versions are append-only (even for a superuser)" \
  "$(psqlq "UPDATE role_template_versions SET change_note='tamper' WHERE template_code='TREASURY_RELEASER'" >/dev/null 2>&1; psqlq "SELECT change_note FROM role_template_versions WHERE template_code='TREASURY_RELEASER' AND template_version=1")" "Initial §9.1 archetype"
PREP=$(body -X POST "$B/v1/role-templates/TREASURY_PREPARER/instantiate" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUD_TP_$RUNID\",\"correlation_id\":\"$(uuid)\"}")
PREP_ROLE=$(echo "$PREP" | python -c "import sys,json;print(json.load(sys.stdin)['role']['role_definition_id'])" 2>/dev/null | tr -d '\015')
PREP_BUNDLE=$(echo "$PREP" | python -c "import sys,json;print(json.load(sys.stdin)['bundle']['bundle_id'])" 2>/dev/null | tr -d '\015')
if [ -n "$PREP_ROLE" ]; then ok "TREASURY_PREPARER instantiated"; else bad "instantiate: $PREP"; fi
chk "the template role is provisioned in authorization-svc" "$(authzq "SELECT role_code FROM roles WHERE role_id='$PREP_ROLE'")" "AUD_TP_$RUNID"
chk "with exactly the template version's actions" \
  "$(authzq "SELECT permitted_actions::text FROM permission_bundles WHERE role_id='$PREP_ROLE' AND bundle_code='TEMPLATE'")" '["payment.create", "payment.prepare", "payment.submit"]'
chk "provenance is recorded" "$(psqlq "SELECT template_code||'/'||template_version FROM role_definitions WHERE role_definition_id='$PREP_ROLE'")" "TREASURY_PREPARER/1"
chk "a template-managed bundle cannot be edited (409)" \
  "$(body -X PATCH "$B/v1/role-definitions/$PREP_ROLE/permission-bundles/$PREP_BUNDLE" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"permitted_actions\":[\"payment.release\"],\"correlation_id\":\"$(uuid)\"}" | ecode)" "template_managed_bundle"
chk "an upgrade to the same version is refused" \
  "$(body -X POST "$B/v1/role-templates/TREASURY_PREPARER/upgrade" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_definition_id\":\"$PREP_ROLE\",\"template_version\":1,\"correlation_id\":\"$(uuid)\"}" | ecode)" "template_version_not_newer"
REL=$(body -X POST "$B/v1/role-templates/TREASURY_RELEASER/instantiate" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUD_TR_$RUNID\",\"correlation_id\":\"$(uuid)\"}")
REL_ROLE=$(echo "$REL" | python -c "import sys,json;print(json.load(sys.stdin)['role']['role_definition_id'])" 2>/dev/null | tr -d '\015')
if [ -n "$REL_ROLE" ]; then ok "TREASURY_RELEASER instantiated"; else bad "instantiate releaser: $REL"; fi
chk "iam.role.published was enqueued for the instantiation" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='iam.role.published' AND aggregate_key='$PREP_ROLE'")" "1"

# §9/§21 assignment requests. Subjects are fresh principals per run.
T1=$(uuid); T2=$(uuid)
areq() { # $1 caller $2 target $3 role $4 corr
  body -X POST "$B/v1/iam/access-assignments/" $(wh "$TEN" "$1" "$4") \
    -d "{\"legal_entity_id\":\"$ENTITY\",\"target_principal_id\":\"$2\",\"role_definition_id\":\"$3\",\"justification\":\"audit $RUNID\",\"correlation_id\":\"$4\"}"
}
A1C=$(uuid)
A1=$(areq "$ME" "$T1" "$PREP_ROLE" "$A1C")
A1ID=$(echo "$A1" | jfield request_id)
chk "a STANDARD request is provisioned on submission" "$(echo "$A1" | jfield status)" "PROVISIONED"
chk "authorization-svc holds the assignment under the request id" \
  "$(authzq "SELECT principal_id FROM principal_role_assignments WHERE principal_role_assignment_id='$A1ID' AND effective_to IS NULL")" "$T1"
chk "iam.assignment.granted was enqueued" "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='iam.assignment.granted' AND aggregate_key='$A1ID'")" "1"
chk "a replay of the request answers 200" "$(code -X POST "$B/v1/iam/access-assignments/" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"target_principal_id\":\"$T1\",\"role_definition_id\":\"$PREP_ROLE\",\"justification\":\"audit $RUNID\",\"correlation_id\":\"$A1C\"}")" "200"
# The §10.1 baseline against what the subject already HOLDS: T1 now prepares
# payments, so T1 may not also be given release.
SODA=$(areq "$ME" "$T1" "$REL_ROLE" "$(uuid)")
chk "releasing for a principal who prepares is refused (held-based SoD)" "$(echo "$SODA" | ecode)" "sod_conflict"
hasf "the refusal names the held conflict" "$SODA" "payment.release conflicts with payment."
A2=$(areq "$ME" "$T2" "$REL_ROLE" "$(uuid)")
A2ID=$(echo "$A2" | jfield request_id)
chk "a CRITICAL request waits for approval" "$(echo "$A2" | jfield status)" "PENDING_APPROVAL"
chk "and is not provisioned yet" "$(authzq "SELECT count(*) FROM principal_role_assignments WHERE principal_role_assignment_id='$A2ID'")" "0"
adec() { # $1 caller $2 id $3 verb
  body -X POST "$B/v1/iam/access-assignments/$2:$3" $(wh "$TEN" "$1" "$(uuid)") \
    -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"audit $RUNID\",\"correlation_id\":\"$(uuid)\"}"
}
chk "the requester cannot approve their own request" "$(adec "$ME" "$A2ID" approve | ecode)" "self_approval"
chk "the subject cannot approve their own access" "$(adec "$T2" "$A2ID" approve | ecode)" "forbidden"
chk "an independent approver provisions it" "$(adec "$APPROVER" "$A2ID" approve | jfield status)" "PROVISIONED"
chk "provisioned AS the approver" "$(authzq "SELECT assigned_by FROM principal_role_assignments WHERE principal_role_assignment_id='$A2ID'")" "$APPROVER"
chk "the database refuses a self-approval row outright" \
  "$(psqlq "UPDATE assignment_requests SET decided_by_principal_id = target_principal_id WHERE request_id='$A2ID'" >/dev/null 2>&1; psqlq "SELECT decided_by_principal_id FROM assignment_requests WHERE request_id='$A2ID'")" "$APPROVER"
chk "revoke ends the assignment" "$(adec "$ME" "$A1ID" revoke | jfield status)" "REVOKED"
chk "in authorization-svc too" "$(authzq "SELECT effective_to IS NOT NULL FROM principal_role_assignments WHERE principal_role_assignment_id='$A1ID'")" "t"
chk "iam.assignment.revoked names the subject, not the revoker" \
  "$(psqlq "SELECT payload->'payload'->>'principal_id' FROM event_outbox WHERE event_type='iam.assignment.revoked' AND aggregate_key='$A1ID'")" "$T1"

# §9/§24 access review. The CRITICAL releaser assignment (T2) is reviewed by
# the approver.
RC=$(body -X POST "$B/v1/access-review-campaigns/" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"campaign_name\":\"audit $RUNID\",\"review_type\":\"PRIVILEGED\",\"default_reviewer_principal_id\":\"$APPROVER\",\"role_definition_ids\":[\"$REL_ROLE\"],\"due_at\":\"2030-01-01T00:00:00Z\",\"correlation_id\":\"$(uuid)\"}")
CID=$(echo "$RC" | jfield campaign_id)
ITEM=$(echo "$RC" | python -c "import sys,json;print(json.load(sys.stdin)['items'][0]['item_id'])" 2>/dev/null | tr -d '\015')
chk "the campaign snapshots the live assignment" "$(echo "$RC" | python -c "import sys,json;i=json.load(sys.stdin)['items'];print(len(i), i[0]['target_principal_id'], i[0]['risk_tier'])" 2>/dev/null | tr -d '\015')" "1 $T2 CRITICAL"
chk "iam.access_review.started was enqueued" "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='iam.access_review.started' AND aggregate_key='$CID'")" "1"
rdec() { # $1 caller $2 decision
  body -X POST "$B/v1/access-review-campaigns/$CID/items/$ITEM/decide" $(wh "$TEN" "$1" "$(uuid)") \
    -d "{\"legal_entity_id\":\"$ENTITY\",\"decision\":\"$2\",\"reason\":\"audit $RUNID\",\"correlation_id\":\"$(uuid)\"}"
}
chk "the subject cannot attest their own access" "$(rdec "$T2" KEEP | ecode)" "self_attestation"
chk "only the assigned reviewer decides" "$(rdec "$ME" KEEP | ecode)" "not_item_reviewer"
chk "an undecided CRITICAL item blocks completion" \
  "$(body -X POST "$B/v1/access-review-campaigns/$CID/complete" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "unresolved_high_risk"
chk "REVOKE by the reviewer is applied" "$(rdec "$APPROVER" REVOKE | jfield revocation_applied)" "True"
chk "and revokes the assignment in authorization-svc" "$(authzq "SELECT effective_to IS NOT NULL FROM principal_role_assignments WHERE principal_role_assignment_id='$A2ID'")" "t"
chk "and closes the request that created it" "$(psqlq "SELECT status FROM assignment_requests WHERE request_id='$A2ID'")" "REVOKED"
chk "the campaign now completes" \
  "$(body -X POST "$B/v1/access-review-campaigns/$CID/complete" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"correlation_id\":\"$(uuid)\"}" | jfield status)" "COMPLETED"
chk "iam.access_review.completed was enqueued" "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='iam.access_review.completed' AND aggregate_key='$CID'")" "1"
chk "my access reviews lists the reviewer's item" \
  "$(body "$B/v1/iam/access-reviews" $(rh "$TEN" "$APPROVER" "$(uuid)") | python -c "import sys,json;print(any(i['item_id']=='$ITEM' for i in json.load(sys.stdin)))" | tr -d '\015')" "True"


echo
echo "-- 9d. Re-audit fixes, live (7 Oct 2026) ---------------------------"
# Each check pins one gap the 7 Oct re-audit found in the 6 Oct build:
# entity binding, approval authority by risk, end dates, effective-dated
# revocation and its expiry sweep, toxic combinations across a tenant-wide and
# an entity role, dormancy / inactive-subject flags, and the paged list.
OTHER_ENTITY=44444444-4444-4444-4444-444444444444
# The ROLE_MANAGE-only principal also manages OTHER_ENTITY, so the entity
# binding itself is reached (begin's ROLE_MANAGE check passes on the body's entity).
authzq "INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
          SELECT '$RMP','$RMO','$OTHER_ENTITY', now() - interval '1 minute', 'audit'
          WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments WHERE principal_id='$RMP' AND legal_entity_id='$OTHER_ENTITY')" >/dev/null
sleep 6 # authorization-svc's grant cache
areq_end() { # $1 caller $2 target $3 role $4 corr $5 effective_to
  body -X POST "$B/v1/iam/access-assignments/" $(wh "$TEN" "$1" "$4") \
    -d "{\"legal_entity_id\":\"$ENTITY\",\"target_principal_id\":\"$2\",\"role_definition_id\":\"$3\",\"justification\":\"audit $RUNID\",\"effective_to\":\"$5\",\"correlation_id\":\"$4\"}"
}
isoin() { python -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=$1)).strftime('%Y-%m-%dT%H:%M:%SZ'))" | tr -d '\015'; }

# F4 approval authority by risk: CRITICAL needs a security approver.
T3=$(uuid)
A3=$(areq "$ME" "$T3" "$REL_ROLE" "$(uuid)")
A3ID=$(echo "$A3" | jfield request_id)
hasf "a CRITICAL request names the security approver it needs" "$A3" "iam.assignment.approve_privileged"
chk "a ROLE_MANAGE-only approver is refused for a CRITICAL grant" "$(adec "$RMP" "$A3ID" approve | ecode)" "security_approval_required"
chk "and nothing is provisioned" "$(authzq "SELECT count(*) FROM principal_role_assignments WHERE principal_role_assignment_id='$A3ID'")" "0"
chk "the refusal is evidence" "$(psqlq "SELECT count(*) FROM refused_escalations WHERE refusal_reason='security_approval_required' AND principal_id='$RMP'" | awk '{print ($1>0)?"yes":"no"}')" "yes"
chk "a security approver provisions it" "$(adec "$APPROVER" "$A3ID" approve | jfield status)" "PROVISIONED"

# F1 a command's entity is the record's entity.
T4=$(uuid)
A4=$(areq "$ME" "$T4" "$REL_ROLE" "$(uuid)")
A4ID=$(echo "$A4" | jfield request_id)
chk "a manager of another entity cannot decide this entity's request" \
  "$(body -X POST "$B/v1/iam/access-assignments/$A4ID:reject" $(wh "$TEN" "$RMP" "$(uuid)") \
     -d "{\"legal_entity_id\":\"$OTHER_ENTITY\",\"reason\":\"audit $RUNID\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "entity_mismatch"
chk "and the request is untouched" "$(psqlq "SELECT status FROM assignment_requests WHERE request_id='$A4ID'")" "PENDING_APPROVAL"
chk "cleanup: the requester cancels it" "$(body -X POST "$B/v1/iam/access-assignments/$A4ID:cancel" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"audit\",\"correlation_id\":\"$(uuid)\"}" | jfield status)" "CANCELLED"

# F2 / F3 end dates, effective-dated revocation, and the expiry sweep.
chk "an end date in the past is refused" "$(areq_end "$ME" "$(uuid)" "$PREP_ROLE" "$(uuid)" "2020-01-01T00:00:00Z" | ecode)" "invalid_effective_to"
T5=$(uuid); T6=$(uuid)
A5END=$(isoin 20)
A5=$(areq_end "$ME" "$T5" "$PREP_ROLE" "$(uuid)" "$A5END")
A5ID=$(echo "$A5" | jfield request_id)
chk "an assignment with an end date is provisioned" "$(echo "$A5" | jfield status)" "PROVISIONED"
chk "authorization-svc holds the end date" "$(authzq "SELECT effective_to = '$A5END'::timestamptz FROM principal_role_assignments WHERE principal_role_assignment_id='$A5ID'")" "t"
hasf "iam.assignment.granted carries effective_to" "$(psqlq "SELECT payload::text FROM event_outbox WHERE event_type='iam.assignment.granted' AND aggregate_key='$A5ID'")" '"effective_to"'
A6=$(areq "$ME" "$T6" "$PREP_ROLE" "$(uuid)")
A6ID=$(echo "$A6" | jfield request_id)
chk "a revoke in the past is refused" "$(body -X POST "$B/v1/iam/access-assignments/$A6ID:revoke" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"audit\",\"effective_at\":\"2020-01-01T00:00:00Z\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "invalid_effective_at"
A6END=$(isoin 20)
SCH=$(body -X POST "$B/v1/iam/access-assignments/$A6ID:revoke" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"moves team\",\"effective_at\":\"$A6END\",\"correlation_id\":\"$(uuid)\"}")
chk "an effective-dated revoke leaves it PROVISIONED until the instant" "$(echo "$SCH" | jfield status)" "PROVISIONED"
chk "and schedules the end in authorization-svc" "$(authzq "SELECT effective_to = '$A6END'::timestamptz FROM principal_role_assignments WHERE principal_role_assignment_id='$A6ID'")" "t"
chk "and announces nothing yet" "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='iam.assignment.revoked' AND aggregate_key='$A6ID'")" "0"
chk "a later schedule cannot extend it" "$(body -X POST "$B/v1/iam/access-assignments/$A6ID:revoke" $(wh "$TEN" "$ME" "$(uuid)") -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"x\",\"effective_at\":\"$(isoin 86400)\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "ends_sooner"
echo "        waiting 55s: both ends (20s) pass, then the expiry sweep (every 30s) runs"
sleep 55
chk "the grant-time end closes as EXPIRED" "$(psqlq "SELECT status FROM assignment_requests WHERE request_id='$A5ID'")" "EXPIRED"
chk "the scheduled end closes as REVOKED" "$(psqlq "SELECT status FROM assignment_requests WHERE request_id='$A6ID'")" "REVOKED"
chk "each enqueued iam.assignment.revoked naming the subject" \
  "$(psqlq "SELECT string_agg(payload->'payload'->>'principal_id', ',' ORDER BY aggregate_key) FROM event_outbox WHERE event_type='iam.assignment.revoked' AND aggregate_key IN ('$A5ID','$A6ID')")" \
  "$( [ "$A5ID" \< "$A6ID" ] && echo "$T5,$T6" || echo "$T6,$T5")"
chk "the scheduled one is attributed to who scheduled it" "$(psqlq "SELECT payload->>'actor_id' FROM event_outbox WHERE event_type='iam.assignment.revoked' AND aggregate_key='$A6ID'")" "$ME"
chk "authorization-svc no longer grants either" "$(authzq "SELECT count(*) FROM principal_role_assignments WHERE principal_role_assignment_id IN ('$A5ID','$A6ID') AND (effective_to IS NULL OR effective_to > now())")" "0"
hasf "the sweep is counted" "$(curl -s -m 10 "$B/metrics")" "access_control_assignments_expired_total"

# F5 toxic combination across a tenant-wide role and an entity role. T7 holds
# a tenant-wide role X; is then granted the entity-scoped releaser (clean: X
# grants nothing conflicting); then X gains payment.prepare (§24 "newly
# introduced SoD conflicts after role/policy changes"). Grouped by
# (principal, entity) alone, the two never met.
XC=$(uuid)
XR=$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$XC") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUD_TW_$RUNID\",\"role_name\":\"Tenant-wide\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$XC\"}")
XROLE=$(echo "$XR" | jfield role_definition_id)
XB1=$(uuid)
chk "tenant-wide role X gets a harmless bundle" "$(body -X POST "$B/v1/role-definitions/$XROLE/permission-bundles" $(wh "$TEN" "$ME" "$XB1") -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"X1\",\"permitted_actions\":[\"supplier_invoice.create\"],\"correlation_id\":\"$XB1\"}" | jfield bundle_code)" "X1"
T7=$(uuid)
A7=$(areq "$ME" "$T7" "$XROLE" "$(uuid)"); A7ID=$(echo "$A7" | jfield request_id)
chk "X is assigned tenant-wide" "$(authzq "SELECT legal_entity_id IS NULL FROM principal_role_assignments WHERE principal_role_assignment_id='$A7ID'")" "t"
A8=$(areq "$ME" "$T7" "$REL_ROLE" "$(uuid)"); A8ID=$(echo "$A8" | jfield request_id)
chk "the entity releaser is granted (nothing conflicting held yet)" "$(adec "$APPROVER" "$A8ID" approve | jfield status)" "PROVISIONED"
XB2=$(uuid)
chk "X then gains payment.prepare" "$(body -X POST "$B/v1/role-definitions/$XROLE/permission-bundles" $(wh "$TEN" "$ME" "$XB2") -d "{\"legal_entity_id\":\"$ENTITY\",\"bundle_code\":\"X2\",\"permitted_actions\":[\"payment.prepare\"],\"correlation_id\":\"$XB2\"}" | jfield bundle_code)" "X2"
# F6 fixtures: T7's releaser grant is backdated past a 1-day window (dormant,
# never used); T8 holds X and is suspended.
T8=$(uuid)
A9=$(areq "$ME" "$T8" "$XROLE" "$(uuid)"); A9ID=$(echo "$A9" | jfield request_id)
authzq "UPDATE principal_role_assignments SET effective_from = now() - interval '3 days' WHERE principal_role_assignment_id='$A8ID';
        INSERT INTO principal_status_projection (principal_id, tenant_id, status, source_service)
          VALUES ('$T8', '$TEN', 'SUSPENDED', 'audit') ON CONFLICT (tenant_id, principal_id) DO UPDATE SET status='SUSPENDED'" >/dev/null
TC=$(body -X POST "$B/v1/access-review-campaigns/" $(wh "$TEN" "$ME" "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"campaign_name\":\"audit toxic $RUNID\",\"review_type\":\"PERIODIC\",\"default_reviewer_principal_id\":\"$APPROVER\",\"role_definition_ids\":[\"$XROLE\",\"$REL_ROLE\"],\"dormancy_days\":1,\"due_at\":\"2030-01-01T00:00:00Z\",\"correlation_id\":\"$(uuid)\"}")
flags_of() { echo "$TC" | python -c "import sys,json;print(','.join(sorted(f for i in json.load(sys.stdin)['items'] if i['authz_assignment_id']=='$1' for f in i['flags'])))" 2>/dev/null | tr -d '\015'; }
chk "the tenant-wide half of the combination is flagged" "$(flags_of "$A7ID")" "SOD_CONFLICT"
chk "the entity half is flagged (and, unused for the window, DORMANT)" "$(flags_of "$A8ID")" "DORMANT,SOD_CONFLICT"
chk "a suspended subject's assignment is SUBJECT_INACTIVE" "$(flags_of "$A9ID")" "SUBJECT_INACTIVE"
chk "and raised to HIGH so the campaign cannot close over it" \
  "$(echo "$TC" | python -c "import sys,json;print([i['risk_tier']+'/'+i['subject_status'] for i in json.load(sys.stdin)['items'] if i['authz_assignment_id']=='$A9ID'][0])" 2>/dev/null | tr -d '\015')" "HIGH/SUSPENDED"
chk "the campaign records its dormancy window" "$(echo "$TC" | jfield dormancy_days)" "1"
TCID=$(echo "$TC" | jfield campaign_id)
chk "completing through another entity is refused" \
  "$(body -X POST "$B/v1/access-review-campaigns/$TCID/complete" $(wh "$TEN" "$RMP" "$(uuid)") -d "{\"legal_entity_id\":\"$OTHER_ENTITY\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "entity_mismatch"

# Paged list with usage (authorization-svc).
PG=$(curl -s -m 10 "$AUTHZ/v1/admin/role-assignments?role_id=$REL_ROLE&include_usage=true&limit=1&offset=0" -H "X-Principal-Id: $ME" -H "X-Tenant-Id: $TEN")
chk "authorization-svc pages the assignment list" "$(echo "$PG" | python -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null | tr -d '\015')" "1"
hasf "and reports the subject's status with each row" "$PG" '"principal_status"'
authzq "DELETE FROM principal_status_projection WHERE principal_id='$T8' AND source_service='audit'" >/dev/null
# Idempotency-Key: bound to the request, honoured on retry.
IK=$(uuid); IC=$(uuid)
IBODY="{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUD_IK_$RUNID\",\"role_name\":\"Idem\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$IC\"}"
chk "first write with a key creates (201)" "$(code -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$IK") -d "$IBODY")" "201"
IREP=$(curl -s -m 25 -D - -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$IK") -d "$IBODY")
hasf "the same key and body replays as 200" "$IREP" "HTTP/1.1 200"
hasf "and says it is a replay" "$IREP" "X-Idempotent-Replay: true"
chk "the same key with a different body is refused" \
  "$(body -X POST "$B/v1/role-definitions/" $(wh "$TEN" "$ME" "$IK") -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"AUD_IK2_$RUNID\",\"role_name\":\"Idem\",\"role_scope_type\":\"TENANT\",\"correlation_id\":\"$(uuid)\"}" | ecode)" "IDEMPOTENCY_MISMATCH"
chk "only one role was created" "$(psqlq "SELECT count(*) FROM role_definitions WHERE role_code LIKE 'AUD_IK%_$RUNID'")" "1"
echo
echo "-- 10. Read scoping and malformed ids ------------------------------"
R=$(uuid)
chk "unknown-but-valid id 404" "$(code "$B/v1/role-definitions/$(uuid)" $(rh "$TEN" "$ME" "$R"))" "404"
# A malformed id cannot name a row — that is what 404 means. It used to answer
# 503 store_unavailable, sending on-call to look at a healthy database, AND it
# leaked the database's own error dialect to any caller who probed it.
chk "malformed role id 404" "$(code "$B/v1/role-definitions/not-a-uuid" $(rh "$TEN" "$ME" "$R"))" "404"
chk "malformed role id is not_found" "$(body "$B/v1/role-definitions/not-a-uuid" $(rh "$TEN" "$ME" "$R") | ecode)" "not_found"
nohasf "malformed id leaks no SQLSTATE" "$(body "$B/v1/role-definitions/not-a-uuid" $(rh "$TEN" "$ME" "$R"))" "SQLSTATE"
chk "malformed bundle id 404" "$(code "$B/v1/role-definitions/$ROLE_ID/permission-bundles/nope" $(rh "$TEN" "$ME" "$R"))" "404"
# Another tenant's role reads as absent, not as forbidden — "you may not see it"
# and "it does not exist" must not be distinguishable here.
chk "cross-tenant read 404" "$(code "$B/v1/role-definitions/$ROLE_ID" $(rh "$OTHER_TEN" "$ME" "$R"))" "404"
# A typo'd filter must not read as "nothing is defined".
chk "unknown status filter 400" "$(body "$B/v1/role-definitions/?status=ACTIVEE" $(rh "$TEN" "$ME" "$R") | ecode)" "invalid_status"
chk "unknown scope filter 400" "$(body "$B/v1/role-definitions/?scope_type=GALAXY" $(rh "$TEN" "$ME" "$R") | ecode)" "invalid_scope_type"
chk "negative limit 400" "$(body "$B/v1/role-definitions/?limit=-1" $(rh "$TEN" "$ME" "$R") | ecode)" "invalid_limit"
chk "bad active_flag 400" "$(body "$B/v1/permission-bundles/?active_flag=maybe" $(rh "$TEN" "$ME" "$R") | ecode)" "invalid_active_flag"
# The flat catalogue: every role's bundles in one tenant-scoped read.
FLAT=$(body "$B/v1/permission-bundles/" $(rh "$TEN" "$ME" "$R"))
hasf "flat bundle catalogue returns this tenant's bundles" "$FLAT" "AUDIT_BUNDLE"
chk "flat catalogue is empty for another tenant" "$(body "$B/v1/permission-bundles/" $(rh "$OTHER_TEN" "$ME" "$R"))" "[]"

echo
echo "-- 11. Outbox, end to end ------------------------------------------"
# The event and the state change are one commit. Delivery is a separate,
# retryable problem — but it must actually happen.
chk "role.created was enqueued" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='role.created' AND aggregate_key='$ROLE_ID'")" "1"
chk "role.updated was enqueued for the retire+reactivate" \
  "$(psqlq "SELECT count(*) >= 2 FROM event_outbox WHERE event_type='role.updated' AND aggregate_key='$ROLE_ID'")" "t"
# The field identity-context-svc reads, in the row that was actually written.
chk "the enqueued role.updated carries role_id" \
  "$(psqlq "SELECT payload->'payload'->>'role_id' FROM event_outbox WHERE event_type='role.updated' AND aggregate_key='$ROLE_ID' ORDER BY outbox_id DESC LIMIT 1")" "$ROLE_ID"
# A bundle write emits BOTH events — the bundle one says what changed, the role
# one is the only name the session-revoking consumer dispatches on.
chk "the bundle write also enqueued a role.updated" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type='permission.bundle.updated' AND aggregate_key='$BUNDLE_ID'")" "1"
# The relay drained them. A backlog that never clears is a consumer that is
# never told anything, with no 5xx and no latency to show for it.
sleep 2
chk "the relay published everything" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND created_at < now() - interval '5 seconds'")" "0"
chk "no outbox row is stuck retrying" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND attempts > 3")" "0"
# And it reached Kafka, with the field the consumer reads.
KOUT=$(docker exec -i "$KAFKACONTAINER" bash -lc "$KAFKABIN/kafka-console-consumer.sh --bootstrap-server localhost:9094 --topic zoiko.access-control.events --from-beginning --timeout-ms 15000" 2>/dev/null)
hasf "role.created reached Kafka" "$KOUT" "\"role_id\": \"$ROLE_ID\""
hasf "the Kafka envelope names this service" "$KOUT" '"source_service": "access-control-svc"'
# authorization-svc consumes these and invalidates its cached grant sources. A
# stale cache there can serve a grant this register has already withdrawn.
# AUTHZ_LOG names a log file when authorization-svc runs outside docker.
if [ -n "${AUTHZ_LOG:-}" ]; then ALOG=$(tail -400 "$AUTHZ_LOG"); else ALOG=$(docker logs authorization-svc 2>&1 | tail -200); fi
hasf "authorization-svc consumed a grant-graph event" "$ALOG" "grant-graph event: cached grants invalidated"

echo
echo "-- 12. Telemetry ---------------------------------------------------"
M=$(body "$B/metrics")
for s in access_control_role_writes_total access_control_bundle_writes_total \
         access_control_authz_decisions_total access_control_authz_admin_calls_total \
         access_control_outbox_pending access_control_outbox_published_total \
         access_control_outbox_failures_total access_control_outbox_oldest_age_seconds \
         readiness_up; do
  hasf "exports $s" "$M" "$s"
done
# Pre-created at zero. A series that has never been observed and one reading
# zero are indistinguishable to an alert expression, so a rule written to catch
# the FIRST occurrence of something stays silent through exactly the event it
# exists for.
hasf "conflict outcome series exists before any conflict" "$M" 'outcome="conflict"'
hasf "forbidden outcome series exists" "$M" 'outcome="forbidden"'
hasf "authz_admin_unavailable series exists" "$M" 'outcome="authz_admin_unavailable"'
hasf "admin call operations are labelled" "$M" 'operation="create_role"'
chk "readiness gauge is 1" "$(echo "$M" | grep -E '^readiness_up' | awk '{print $2}')" "1"

echo
echo "-- 13. Alert rules -------------------------------------------------"
if [ -f "$DEPLOY/prometheus-rules.yml" ]; then
  hasf "prometheus scrapes this service" "$(cat "$DEPLOY/prometheus.yml")" "access-control-svc:8137"
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
for f in openapi.yaml asyncapi.yaml RUNBOOK.md RELEASE_CERTIFICATE.md progress.md scripts/audit.sh; do
  if [ -f "$SVC/$f" ]; then ok "$f present"; else bad "$f missing"; fi
done

echo
echo "-- 15. Migrations & RLS --------------------------------------------"
# Read back from pg_policy, not from the migration file. A migration that was
# written correctly and never applied looks exactly like one that was applied.
chk "role_definitions FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='role_definitions'")" "t"
chk "permission_bundle_defs FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='permission_bundle_defs'")" "t"
chk "event_outbox FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='event_outbox'")" "t"
# A USING-only policy governs what is VISIBLE, not what may be WRITTEN — a
# caller could insert a row attributed to another tenant and then be unable to
# see it.
for t in role_definitions permission_bundle_defs event_outbox; do
  chk "$t policy has WITH CHECK" "$(psqlq "SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relname='$t' AND p.polwithcheck IS NOT NULL")" "1"
done
# The relay's one deliberate cross-tenant exemption, named rather than implied.
hasf "outbox policy admits the named relay" "$(psqlq "SELECT pg_get_expr(polqual,polrelid) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relname='event_outbox'")" "app.outbox_relay"
chk "status CHECK constraint present" "$(psqlq "SELECT count(*) FROM pg_constraint WHERE conname='role_definitions_status_check'")" "1"
chk "one bundle_code per role is enforced" "$(psqlq "SELECT count(*) FROM pg_indexes WHERE indexname='idx_permission_bundle_defs_role_code'")" "1"
chk "no duplicate bundle codes exist" "$(psqlq "SELECT count(*) FROM (SELECT 1 FROM permission_bundle_defs GROUP BY tenant_id, role_definition_id, bundle_code HAVING count(*)>1) x")" "0"
# 000007: the refusal evidence and the protected catalogue.
chk "refused_escalations FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='refused_escalations'")" "t"
chk "protected_permissions FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='protected_permissions'")" "t"
chk "refusals are append-only (no UPDATE/DELETE/ALL policy)" \
  "$(psqlq "SELECT count(*) FROM pg_policies WHERE tablename='refused_escalations' AND cmd IN ('ALL','UPDATE','DELETE')")" "0"
chk "the catalogue protects the iam.* admin actions" \
  "$(psqlq "SELECT count(*) FROM protected_permissions WHERE active_flag AND action_name LIKE 'iam.%'")" "12"
# 000008-000012: the governance tables.
for t in permission_definitions role_templates role_template_versions assignment_requests access_review_campaigns access_review_items idempotency_keys; do
  chk "$t FORCE RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='$t'")" "t"
done
chk "template versions carry the immutability trigger" "$(psqlq "SELECT count(*) FROM pg_trigger WHERE tgname='role_template_versions_no_update'")" "1"
chk "assignment_requests refuses self-decision rows" "$(psqlq "SELECT count(*) FROM pg_constraint WHERE conname='assignment_requests_independent_decision'")" "1"
chk "review items refuse self-attestation rows" "$(psqlq "SELECT count(*) FROM pg_constraint WHERE conname='access_review_items_no_self_attestation'")" "1"
# Every up has a down.
UPS=$(ls "$SVC/deployments/migrations"/*.up.sql | wc -l)
DOWNS=$(ls "$SVC/deployments/migrations"/*.down.sql | wc -l)
chk "every up migration has a down" "$UPS" "$DOWNS"

echo
echo "-- 16. Frontend ----------------------------------------------------"
if [ -n "$SKIP_FE" ] || [ -z "$FE" ] || [ ! -d "$FE/node_modules" ]; then
  echo "        skipped (SKIP_FE set or console node_modules absent)"
else
  CLIENT="$FE/lib/api/access-control.ts"
  if [ -f "$CLIENT" ]; then ok "console client present"; else bad "console client present"; fi
  EX=$(cat "$CLIENT")
  # The client must call the routes the service actually serves. Every route in
  # an earlier version of this file was wrong — /v1/roles and
  # /v1/roles/{id}/bundles, both 404 — and because no page imported it, nothing
  # surfaced that.
  hasf "client uses /v1/role-definitions" "$EX" "/v1/role-definitions"
  hasf "client uses the flat bundle read" "$EX" "/v1/permission-bundles/"
  nohasf "client does not call /v1/roles" "$EX" '"/v1/roles"'
  # The client must explain every refusal this service can produce. Most are
  # rules the console cannot check for itself, because they depend on grants
  # only authorization-svc knows about — which is exactly when a bare error
  # string leaves the reader with nothing to do next.
  while IFS= read -r phrase; do
    [ -z "$phrase" ] && continue
    if echo "$EX" | grep -qF "$phrase"; then ok "explains: $phrase"; else bad "explains: $phrase"; fi
  done <<'PHRASES'
role_code_exists
bundle_code_exists
authorization-svc admin API unavailable
authorization denied for this access control action
role definition not found
permission bundle not found
nothing_to_update
empty_actions
invalid_status
caller identity missing
PHRASES
  # The service answers refusals in the legacy error_code/error_message dialect,
  # distinct from the envelope middleware's error/detail. Without folding it,
  # every refusal reaches the explainer empty and renders as a bare status line.
  grep -q 'error_code' "$FE/lib/api/client.ts" && ok "client folds error_code dialect" || bad "client folds error_code dialect"
  # 201 vs 200 must survive to the caller, or the replay branch is unreachable.
  grep -q 'status: number' "$FE/lib/api/client.ts" && ok "write result carries the status" || bad "write result carries the status"
  grep -q 'result.status === 200' "$FE/app/admin/access-control/actions.ts" && ok "console distinguishes a replay from a create" || bad "console distinguishes a replay from a create"

  TSOUT=$(cd "$FE" && npx tsc --noEmit -p tsconfig.json 2>&1 | grep -v 'npm notice')
  if [ -z "$TSOUT" ]; then ok "console typecheck clean"; else bad "console typecheck clean"; fi

  if [ -f "$FE/e2e/access-control.spec.ts" ]; then
    ok "e2e spec exists"
    if [ -f "$FE/e2e/mock/access-control-service.mjs" ]; then ok "e2e mock exists"; else bad "e2e mock exists"; fi
    SPECS=$(cd "$FE" && npx playwright test e2e/access-control.spec.ts --reporter=line 2>&1)
    echo "$SPECS" | tail -2
    if echo "$SPECS" | grep -qE '[0-9]+ passed' && ! echo "$SPECS" | grep -qE '[0-9]+ failed'; then
      ok "e2e access-control specs pass"
    else
      bad "e2e access-control specs pass"
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
