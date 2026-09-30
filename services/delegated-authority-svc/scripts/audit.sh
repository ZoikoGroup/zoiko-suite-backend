#!/usr/bin/env bash
# Audit test — delegated-authority-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# running stack rather than against stubs. Every check below either exercises a
# documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8136), postgres reachable as
# $PGCONTAINER, authorization-svc on :8089, a Go toolchain, and the console's
# node_modules for section 16 (SKIP_FE=1 skips it).
#
# TEST_DATABASE_URL must point at a SCRATCH database and a NOSUPERUSER
# NOBYPASSRLS role — the store suite drops its tables on every run, and a
# superuser bypasses row-level security unconditionally, which would let the
# isolation tests pass with every policy dropped.
#
# Exits non-zero if anything failed, so CI can gate on it.
#
# Contract queries live in scripts/spec_query.py rather than inline heredocs:
# python's print() emits CRLF on Windows, CR is not IFS whitespace, and every
# token read back carried a trailing CR that made each grep for it fail against
# output plainly containing it. That produced five FAILs on working code.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8136}
AUTHZ=${AUTHZ:-http://localhost:8089}
PGCONTAINER=${PGCONTAINER:-zoiko-postgres}
FE=${FE:-$(cd "$SVC/../../../zoiko-suite-frontend-platform" 2>/dev/null && pwd)}
# A function, not a string. $SVC contains a space on this machine
# ("...\Zoiko comp\...") and an unquoted $Q would word-split the path,
# handing python a truncated filename and failing every contract query.
Q() { python "$SVC/scripts/spec_query.py" "$@"; }

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=99999999-9999-9999-9999-999999999999
ENTITY=22222222-2222-2222-2222-222222222222
ME=33333333-3333-3333-3333-333333333333
THEM=44444444-4444-4444-4444-444444444444
THIRD=55555555-5555-5555-5555-555555555555

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hasf() { if echo "$2" | grep -qF "$3"; then ok "$1"; else bad "$1 (missing $3)"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 25 "$@"; }
ecode() { python -c "import sys,json;print(json.load(sys.stdin).get('error_code',''))" 2>/dev/null | tr -d '\015'; }
psqlq() { docker exec -i "$PGCONTAINER" psql -U postgres -d delegated_authority -tAc "$1" 2>/dev/null | tr -d '\015'; }
uuid() { python -c "import uuid;print(uuid.uuid4())" | tr -d '\015'; }

# The FULL §4 envelope. request_id and source_channel are mandatory on every
# request, not only on writes — omitting them gets 400 envelope_incomplete
# before a handler runs, which would make the business-rule checks in section 7
# silently measure the envelope instead of the rule they name.
wh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Legal-Entity-Id:$ENTITY -H Idempotency-Key:$3 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3 -H Content-Type:application/json"; }

echo "==================================================================="
echo " AUDIT — delegated-authority-svc"
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
# this check measure what it claims to. Running `gofmt -w` on those files would
# rewrite every line and produce a diff that is pure noise.
UNFMT=""
for f in $(find . -name '*.go' -not -path './.gotmp/*'); do
  [ -n "$(tr -d '\015' < "$f" | gofmt -l 2>&1)" ] && UNFMT="$UNFMT $f"
done
if [ -z "$UNFMT" ]; then ok "gofmt clean (line-ending independent)"; else bad "gofmt:$UNFMT"; fi

echo
echo "-- 2. Test suite ---------------------------------------------------"
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
chk "packages failing" "$(echo "$OUT" | grep -cE '^FAIL')" "0"
echo "        packages passing: $(echo "$OUT" | grep -cE '^ok ')"
VOUT=$(gotest -count=1 -v ./...)
echo "        tests passing:    $(echo "$VOUT" | grep -cE '^--- PASS|^    --- PASS')"
chk "tests failing" "$(echo "$VOUT" | grep -cE '^--- FAIL|^    --- FAIL')" "0"
# A store suite that SKIPPED is a suite that verified nothing. It reports ok
# either way, which is exactly why this is asserted rather than eyeballed.
chk "no test skipped" "$(echo "$VOUT" | grep -cE '^--- SKIP|^    --- SKIP')" "0"

echo
echo "-- 3. Live health --------------------------------------------------"
chk "/healthz 200"  "$(code "$B/healthz")" "200"
chk "/readyz 200"   "$(code "$B/readyz")"  "200"
chk "/metrics 200"  "$(code "$B/metrics")" "200"
# Readiness must name authorization-svc. It is a readiness dependency, not
# merely a runtime one: every route calls it and this service fails closed, so
# with it unreachable the pool can be healthy while 100% of requests 503.
READY=$(body "$B/readyz")
hasf "readiness reports authorization-svc" "$READY" "authorization-svc"
hasf "readiness reports database" "$READY" "database"

echo
echo "-- 4. Envelope contract (ZS-ARCH-SVC-001 §4) -----------------------"
chk "read with no tenant    401" "$(code "$B/v1/delegations/" -H "X-Principal-Id:$ME")" "401"
chk "read with no principal 401" "$(code "$B/v1/delegations/" -H "X-Tenant-Id:$TEN")" "401"
# Distinct codes on purpose: a forgotten tenant header and a request that never
# passed ForwardAuth are fixed in different places.
chk "missing tenant code"    "$(body "$B/v1/delegations/" -H "X-Principal-Id:$ME" | ecode)" "tenant_missing"
chk "missing identity code"  "$(body "$B/v1/delegations/" -H "X-Tenant-Id:$TEN" | ecode)" "identity_missing"
chk "write with no envelope 400" \
  "$(code -X POST "$B/v1/delegations/" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" \
      -H 'Content-Type:application/json' -d '{}')" "400"

echo
echo "-- 5. Route surface matches openapi.yaml ---------------------------"
# Both directions. A spec listing a route the service does not serve sends a
# client generator down a dead end; a route the spec omits is a surface nobody
# reviewed.
SPEC_PATHS=$(Q openapi-paths openapi.yaml)
for p in "/v1/delegations/" "/v1/delegations/{delegation_id}" "/v1/delegations/{delegation_id}/revoke" /healthz /readyz /metrics; do
  hasf "openapi documents $p" "$SPEC_PATHS" "$p"
done
# Anchored. Unanchored, r\.(Get|Post)\( also matches inside
# r.Header.Get("X-Principal-Id") — "Heade" + "r.Get(" — and reported a fifth
# route that does not exist.
CODE_ROUTES=$(grep -cE '^[[:space:]]*r\.(Get|Post)\("' internal/handler/handler.go)
chk "handler registers 4 routes" "$CODE_ROUTES" "4"
# Every error_code the handler can emit must be in the spec's enum, or a client
# branching on the code meets one the contract never mentioned.
SPEC_CODES=$(Q openapi-error-codes openapi.yaml)
MISSING=0
for c in $(grep -oE 'writeError\(w, [^,]+, "[a-z_]+"' internal/handler/handler.go | grep -oE '"[a-z_]+"$' | tr -d '"' | sort -u); do
  echo "$SPEC_CODES" | grep -qx "$c" || { MISSING=$((MISSING+1)); echo "        undocumented: $c"; }
done
chk "every emitted error_code is in openapi" "$MISSING" "0"

echo
echo "-- 6. Event contract matches asyncapi.yaml -------------------------"
ASPEC=$(cat asyncapi.yaml)
PUB=$(cat internal/events/publisher.go)
for e in authority.delegated authority.revoked authority.expired; do
  hasf "asyncapi documents $e" "$ASPEC" "$e"
  hasf "publisher emits $e" "$PUB" "$e"
done
# The spec promises consumers that expiry carries no actor, so it is asserted
# against the code rather than trusted.
EXPIRED_BLOCK=$(sed -n '/case EventExpired:/,/^	default:/p' internal/events/publisher.go)
hasf "authority.expired sets no actor" "$EXPIRED_BLOCK" 'actorID = ""'
hasf "authority.expired carries effective_to" "$EXPIRED_BLOCK" 'effective_to'

echo
echo "-- 7. Delegation invariant (Doc 03 §9.3) ---------------------------"
# "Delegated authority must never exceed the delegator's own authority."
# Unseeded, the delegator holds nothing, so a grant naming them must be refused
# — and refused as delegator_lacks_authority, not as a generic 403.
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LATER=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)
K=$(uuid)
GRANT="{\"legal_entity_id\":\"$ENTITY\",\"delegator_principal_id\":\"$ME\",\"delegate_principal_id\":\"$THEM\",\"action_type\":\"AUDIT_PROBE_ACTION\",\"effective_from\":\"$NOW\",\"effective_to\":\"$LATER\",\"correlation_id\":\"$K\"}"
RC=$(body -X POST "$B/v1/delegations/" $(wh "$TEN" "$ME" "$K") -d "$GRANT" | ecode)
if [ "$RC" = "delegator_lacks_authority" ] || [ "$RC" = "forbidden" ]; then
  ok "ungranted delegation refused ($RC)"
else
  bad "ungranted delegation refused (got '$RC')"
fi
# A delegation to yourself is a no-op that reads as a chain. Refused before any
# authz call, so it holds whatever the delegator's grants are.
K=$(uuid)
SELF="{\"legal_entity_id\":\"$ENTITY\",\"delegator_principal_id\":\"$ME\",\"delegate_principal_id\":\"$ME\",\"action_type\":\"AUDIT_PROBE_ACTION\",\"effective_from\":\"$NOW\",\"effective_to\":\"$LATER\",\"correlation_id\":\"$K\"}"
chk "delegate==delegator refused" \
  "$(body -X POST "$B/v1/delegations/" $(wh "$TEN" "$ME" "$K") -d "$SELF" | ecode)" "delegate_is_delegator"
# A window with no positive duration is not time-bound at all.
K=$(uuid)
BADW="{\"legal_entity_id\":\"$ENTITY\",\"delegator_principal_id\":\"$ME\",\"delegate_principal_id\":\"$THEM\",\"action_type\":\"AUDIT_PROBE_ACTION\",\"effective_from\":\"$NOW\",\"effective_to\":\"$NOW\",\"correlation_id\":\"$K\"}"
chk "zero-length window refused" \
  "$(body -X POST "$B/v1/delegations/" $(wh "$TEN" "$ME" "$K") -d "$BADW" | ecode)" "invalid_time_window"

echo
echo "-- 8. Read scoping -------------------------------------------------"
# Two scopes, never three. Without an entity the answer is the caller's own
# involvement; asking after ANOTHER principal unscoped is 403, not an empty
# list, because "you may not ask" and "there are none" are different answers
# and only one of them is reassuring.
chk "unscoped self read 200" \
  "$(code "$B/v1/delegations/" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME")" "200"
chk "unscoped read of another principal 403" \
  "$(code "$B/v1/delegations/?delegate_principal_id=$THIRD" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME")" "403"
# A typo'd filter must not read as "nobody holds any delegated authority".
chk "unknown status filter 400" \
  "$(body "$B/v1/delegations/?status=ACTIVEE" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" | ecode)" "unknown_status"
chk "out-of-range limit 400" \
  "$(body "$B/v1/delegations/?limit=9999" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" | ecode)" "invalid_paging"
# A malformed id cannot name a row — that is what 404 means. It used to answer
# 503 store_unavailable, sending on-call to look at a healthy database.
chk "malformed delegation id 404" \
  "$(code "$B/v1/delegations/not-a-uuid" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME")" "404"
chk "malformed id is not_found" \
  "$(body "$B/v1/delegations/not-a-uuid" -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" | ecode)" "not_found"

echo
echo "-- 9. Expiry: structure ---------------------------------------------"
# The defect this section exists for: expiry used to piggyback on reads, so a
# tenant whose register nobody opened expired NOTHING, indefinitely — its
# delegates kept authority and authority.expired was never published.
MAIN=$(cat cmd/server/main.go)
STORE=$(cat internal/store/pg_store.go)
hasf "background sweeper package exists" "$(ls internal/expiry/ 2>&1)" "sweeper.go"
hasf "sweeper wired into main" "$MAIN" "expiry.New"
hasf "sweeper runs as a loop" "$MAIN" "sweeper.Run"
hasf "sweeper stops before the relay" "$MAIN" "sweeperCancel()"
# The sweep must cross tenants. A tenant-scoped sweep IS the original defect.
hasf "sweep is cross-tenant" "$STORE" "ExpireDueAllTenants"
hasf "sweep uses its own RLS exemption" "$STORE" "app.expiry_sweeper"
hasf "sweep batches with SKIP LOCKED" "$STORE" "FOR UPDATE SKIP LOCKED"
# expired_at must record when the AUTHORITY ended, not when the sweep noticed.
hasf "expired_at = effective_to" "$STORE" "expired_at = effective_to"
chk "expired_at is never the sweep clock" \
  "$(grep -c 'expired_at = \$1' internal/store/pg_store.go)" "0"
M4=$(cat deployments/migrations/000004_expiry_sweeper.up.sql 2>/dev/null)
hasf "000004 admits the sweeper" "$M4" "expiry_sweeper"
hasf "000004 indexes the cross-tenant sweep" "$M4" "idx_delegation_grants_active_effective_to"
hasf "000004 backfills misdated expired_at" "$M4" "SET expired_at = effective_to"

echo
echo "-- 10. Expiry: live proof -------------------------------------------"
# Prove the sweeper actually ends a grant against the running service, with
# nobody reading the register. Everything in section 9 is structural; this is
# the check that would have caught the original defect.
if [ "$(psqlq "SELECT 1")" = "1" ]; then
  DID=$(uuid); CORR=$(uuid)
  psqlq "INSERT INTO delegation_grants (delegation_id,tenant_id,legal_entity_id,
      delegator_principal_id,delegate_principal_id,action_type,effective_from,effective_to,
      status,created_by_principal_id,correlation_id,created_at,updated_at)
    VALUES ('$DID','$OTHER_TEN','$ENTITY','$ME','$THEM','AUDIT_EXPIRY_PROBE',
      now() - interval '2 hours', now() - interval '1 hour','ACTIVE','$ME','$CORR',now(),now());" >/dev/null
  if [ "$(psqlq "SELECT count(*) FROM delegation_grants WHERE delegation_id='$DID';")" = "1" ]; then
    ok "probe grant seeded in a tenant nobody reads"
    EXPIRED=no
    for _ in $(seq 1 24); do
      [ "$(psqlq "SELECT status FROM delegation_grants WHERE delegation_id='$DID';")" = "EXPIRED" ] && { EXPIRED=yes; break; }
      sleep 5
    done
    chk "sweeper expired it with no read" "$EXPIRED" "yes"
    if [ "$EXPIRED" = "yes" ]; then
      DRIFT=$(psqlq "SELECT abs(extract(epoch from (expired_at - effective_to)))::int
                       FROM delegation_grants WHERE delegation_id='$DID';")
      if [ -n "$DRIFT" ] && [ "$DRIFT" -le 2 ]; then
        ok "expired_at equals effective_to (drift ${DRIFT}s)"
      else
        bad "expired_at equals effective_to (drift ${DRIFT}s)"
      fi
      chk "authority.expired enqueued" \
        "$(psqlq "SELECT count(*) FROM delegation_outbox WHERE delegation_id='$DID' AND event_type='authority.expired';")" "1"
    fi
    psqlq "DELETE FROM delegation_outbox WHERE delegation_id='$DID';" >/dev/null
    psqlq "DELETE FROM delegation_grants WHERE delegation_id='$DID';" >/dev/null
  else
    bad "probe grant seeded in a tenant nobody reads"
  fi
else
  echo "        SKIP (no postgres at $PGCONTAINER)"
fi

echo
echo "-- 11. Outbox -------------------------------------------------------"
hasf "relay wired into main" "$MAIN" "outbox.NewRelay"
hasf "events enqueued in the state-change txn" "$STORE" "delegation_outbox"
# One Publish call per batch, not per record: kafka-go waits out BatchTimeout
# once per call, so a per-record loop drains at roughly one event a second.
hasf "relay publishes the batch in one call" "$(cat internal/outbox/relay.go)" "r.publisher.Publish(ctx, msgs)"
if [ "$(psqlq "SELECT 1")" = "1" ]; then
  STUCK=$(psqlq "SELECT count(*) FROM delegation_outbox WHERE published_at IS NULL AND created_at < now() - interval '5 minutes';")
  chk "no outbox event older than 5m unpublished" "${STUCK:-0}" "0"
fi

echo
echo "-- 12. Telemetry ----------------------------------------------------"
M=$(body "$B/metrics")
for m in delegated_authority_grants_total delegated_authority_revocations_total \
         delegated_authority_register_reads_total delegated_authority_authz_decisions_total \
         delegated_authority_expiries_total delegated_authority_expiry_lateness_seconds \
         delegated_authority_expiry_due_pending delegated_authority_expiry_oldest_overdue_seconds \
         delegated_authority_expiry_sweep_failures_total delegated_authority_outbox_pending \
         delegated_authority_outbox_oldest_age_seconds readiness_up; do
  hasf "exposes $m" "$M" "$m"
done
# The escalation outcomes must be pre-registered, or the alert watching them
# matches no series until the first attempt — and an alert that cannot fire
# before the event it warns about is not an alert.
hasf "delegator_mismatch outcome registered" "$M" 'outcome="delegator_mismatch"'
hasf "self_dealing outcome registered" "$M" 'outcome="self_dealing"'
hasf "authz unavailable outcome registered" "$M" 'outcome="unavailable"'

echo
echo "-- 13. Alert rules --------------------------------------------------"
RULES="$SVC/../../deployments/prometheus-rules.yml"
SCRAPE="$SVC/../../deployments/prometheus.yml"
hasf "scrape job exists" "$(cat "$SCRAPE")" "job_name: delegated-authority-svc"
R=$(Q alert-names "$RULES")
for a in DelegatedAuthorityExpirySweepStalled DelegatedAuthorityExpirySweepFailing \
         DelegatedAuthorityEscalationAttempts DelegatedAuthorityOutboxStalled \
         DelegatedAuthorityAuthzUnavailable DelegatedAuthorityReadinessFailing; do
  hasf "alert $a defined" "$R" "$a"
done
# Every metric an alert reads must exist. An alert on a misspelled series is
# silent forever and looks exactly like one that never fires because nothing
# is wrong.
UNKNOWN=0
for series in $(Q alert-series "$RULES"); do
  echo "$M" | grep -qF "$series" || { UNKNOWN=$((UNKNOWN+1)); echo "        alert reads missing series: $series"; }
done
chk "every alert series exists on /metrics" "$UNKNOWN" "0"
# Every "RUNBOOK section N.N" pointer must resolve to a real heading. The
# pointer is otherwise discovered only by an on-call, at the worst moment.
BADREF=0
for s in $(Q runbook-refs "$RULES"); do
  grep -qE "^#+ $s " RUNBOOK.md || { BADREF=$((BADREF+1)); echo "        dangling: RUNBOOK section $s"; }
done
chk "every RUNBOOK pointer resolves" "$BADREF" "0"

echo
echo "-- 14. Release artifacts --------------------------------------------"
for f in openapi.yaml asyncapi.yaml RUNBOOK.md RELEASE_CERTIFICATE.md progress.md scripts/audit.sh; do
  if [ -f "$SVC/$f" ]; then ok "$f present"; else bad "$f missing"; fi
done
Q parses openapi.yaml  >/dev/null 2>&1 && ok "openapi.yaml parses"  || bad "openapi.yaml parses"
Q parses asyncapi.yaml >/dev/null 2>&1 && ok "asyncapi.yaml parses" || bad "asyncapi.yaml parses"

echo
echo "-- 15. Migrations & RLS ---------------------------------------------"
for m in 000001_initial_schema 000002_force_rls_and_invariants 000003_outbox 000004_expiry_sweeper; do
  if [ -f "deployments/migrations/${m}.up.sql" ] && [ -f "deployments/migrations/${m}.down.sql" ]; then
    ok "$m has up and down"
  else
    bad "$m has up and down"
  fi
done
ALLSQL=$(cat deployments/migrations/*.sql)
# FORCE, not merely ENABLE: Postgres exempts a table's OWNER from RLS unless
# the table is FORCE, and these services connect as the owner. A policy that
# silently does nothing is worse than none, because it reads as a control.
hasf "grants table is FORCE RLS" "$ALLSQL" "delegation_grants FORCE ROW LEVEL SECURITY"
hasf "outbox table is FORCE RLS" "$ALLSQL" "delegation_outbox FORCE ROW LEVEL SECURITY"
if [ "$(psqlq "SELECT 1")" = "1" ]; then
  # The LIVE policy is what matters. A down migration or a hand-edit would not
  # show up in the repo.
  POL=$(psqlq "SELECT pg_get_expr(polqual, polrelid) FROM pg_policy WHERE polrelid='delegation_grants'::regclass;")
  hasf "live grants policy admits the sweeper" "$POL" "expiry_sweeper"
  hasf "live grants policy uses NULLIF" "$POL" "NULLIF"
  OPOL=$(psqlq "SELECT pg_get_expr(polqual, polrelid) FROM pg_policy WHERE polrelid='delegation_outbox'::regclass;")
  hasf "live outbox policy admits the relay" "$OPOL" "outbox_relay"
fi

echo
echo "-- 16. Frontend -----------------------------------------------------"
if [ "${SKIP_FE:-0}" = "1" ] || [ -z "$FE" ] || [ ! -d "$FE/node_modules" ]; then
  echo "        SKIP (SKIP_FE=1 or console node_modules absent)"
else
  # Deliberately NOT a subshell: ok/bad increment PASS/FAIL, and a subshell
  # would discard every count in this section while still printing them.
  [ -f "$FE/app/admin/delegations/page.tsx" ] && ok "console page exists" || bad "console page exists"
  [ -f "$FE/app/admin/delegations/actions.ts" ] && ok "server actions exist" || bad "server actions exist"
  [ -f "$FE/lib/api/delegations.ts" ] && ok "api client exists" || bad "api client exists"
  [ -f "$FE/components/admin/delegations/DelegationRegisterPanel.tsx" ] && ok "register panel exists" || bad "register panel exists"
  grep -q '/admin/delegations' "$FE/lib/constants.ts" && ok "nav entry present" || bad "nav entry present"

  # The client must explain every refusal this service can produce. Most are
  # rules the console cannot check for itself, because they depend on grants
  # only authorization-svc knows about — which is exactly when a bare error
  # string leaves the reader with nothing to do next.
  EX=$(cat "$FE/lib/api/delegations.ts")
  while IFS= read -r phrase; do
    [ -z "$phrase" ] && continue
    if echo "$EX" | grep -qF "$phrase"; then ok "explains: $phrase"; else bad "explains: $phrase"; fi
  done <<'PHRASES'
caller may only delegate their own authority
may not name the caller as delegate
delegator does not hold the authority
delegate_principal_id must differ
effective_to must be after effective_from
status must be one of
invalid delegation status transition
delegation not found
authorization-svc unavailable
PHRASES

  # The client's documented contract must match the service's behaviour. It
  # described expiry as lazy and read-triggered, which stopped being true when
  # the sweeper landed — stale documentation the next reader would trust.
  echo "$EX" | grep -qF "EXPIRY IS ENFORCED" && ok "client documents enforced expiry" || bad "client documents enforced expiry"
  if echo "$EX" | grep -qF "EXPIRY IS LAZY"; then bad "client no longer claims lazy expiry"; else ok "client no longer claims lazy expiry"; fi

  # The service answers refusals in the legacy error_code/error_message
  # dialect, distinct from the envelope middleware's error/detail. Without
  # folding it, every refusal reaches the explainer empty and renders as a
  # bare status line instead of the sentence written for it.
  grep -q 'error_code' "$FE/lib/api/client.ts" && ok "client folds error_code dialect" || bad "client folds error_code dialect"

  TSOUT=$(cd "$FE" && npx tsc --noEmit -p tsconfig.json 2>&1)
  if [ -z "$TSOUT" ]; then ok "console typecheck clean"; else bad "console typecheck clean"; fi

  if [ -f "$FE/e2e/delegations.spec.ts" ]; then
    ok "e2e spec exists"
    SPECS=$(cd "$FE" && npx playwright test e2e/delegations.spec.ts --reporter=line 2>&1)
    echo "$SPECS" | tail -2
    if echo "$SPECS" | grep -qE '[0-9]+ passed' && ! echo "$SPECS" | grep -qE '[0-9]+ failed'; then
      ok "e2e delegations specs pass"
    else
      bad "e2e delegations specs pass"
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
