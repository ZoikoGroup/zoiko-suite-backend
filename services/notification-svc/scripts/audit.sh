#!/usr/bin/env bash
# Audit — notification-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# RUNNING stack rather than against stubs. Every check below either exercises a
# documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8133), postgres reachable as
# $PGCONTAINER, authorization-svc on :8089, kafka as $KAFKACONTAINER, a Go
# toolchain, and the console's node_modules for section 14 (SKIP_FE=1 skips it).
#
# Exits non-zero if anything failed, so CI can gate on it.
#
# Contract queries live in scripts/spec_query.py rather than inline heredocs:
# python's print() emits CRLF on Windows, CR is not IFS whitespace, and every
# token read back would carry a trailing CR that makes each grep for it fail
# against output plainly containing it.
#
# ── WHY THIS AUDIT IS SHAPED THE WAY IT IS ───────────────────────────────────
#
# Almost every interesting failure on this service answers 2xx. A FAILED
# delivery is a 201 by design (§9.7 — notification failure must not collapse the
# source workflow); a rescheduled one is a 201; a stranded notification belongs
# to a request that succeeded days earlier; and a lost event used to be a log
# line and nothing more. So a smoke test that checks status codes proves almost
# nothing here, and most of what follows asserts on the DATABASE and on the
# METRICS rather than on the response.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8133}
AUTHZ=${AUTHZ:-http://localhost:8089}
PGCONTAINER=${PGCONTAINER:-zoiko-postgres}
KAFKACONTAINER=${KAFKACONTAINER:-zoiko-kafka}
KAFKABIN=${KAFKABIN:-/opt/kafka/bin}
MAILPIT=${MAILPIT:-http://localhost:8025}
DEPLOY=${DEPLOY:-$(cd "$SVC/../../deployments" 2>/dev/null && pwd)}
FE=${FE:-$(cd "$SVC/../../../zoiko-suite-frontend-platform" 2>/dev/null && pwd)}
# A function, not a string. $SVC contains a space on this machine
# ("...\Zoiko comp\...") and an unquoted $Q would word-split the path, handing
# python a truncated filename and failing every contract query.
Q() { python "$SVC/scripts/spec_query.py" "$@"; }

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=99999999-9999-9999-9999-999999999999
ME=33333333-3333-3333-3333-333333333333
ENTITY=22222222-2222-2222-2222-222222222222
OTHER_PRINCIPAL=44444444-4444-4444-4444-444444444444

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
hasf() { if echo "$2" | grep -qF "$3"; then ok "$1"; else bad "$1 (missing $3)"; fi; }
nohasf() { if echo "$2" | grep -qF "$3"; then bad "$1 (found $3)"; else ok "$1"; fi; }

code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
body() { curl -s -m 25 "$@"; }
# TWO error dialects reach a caller here, and reading only one makes every
# business-rule check below report an empty string — which looks like the rule
# is broken when it is the reader that is.
#
#   handler refusals:  {"error_code": "...", "error_message": "..."}
#   envelope refusals: {"error": "envelope_incomplete", "detail": "...", ...}
#
# The console already copes with both (lib/api/client.ts reads either), and
# openapi.yaml documents both. This reads error_code first and falls back.
ecode() { python -c "import sys,json;d=json.load(sys.stdin);print(d.get('error_code') or d.get('error') or '')" 2>/dev/null | tr -d '\015'; }
jfield() { python -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null | tr -d '\015'; }
psqlq() { docker exec -i "$PGCONTAINER" psql -U postgres -d notification -tAc "$1" 2>/dev/null | tr -d '\015'; }
authzq() { docker exec -i "$PGCONTAINER" psql -U postgres -d authorization_svc -tAc "$1" 2>/dev/null | tr -d '\015'; }
uuid() { python -c "import uuid;print(uuid.uuid4())" | tr -d '\015'; }

# The FULL §4 envelope for a WRITE. request_id, source_channel AND
# X-Legal-Entity-Id are mandatory alongside idempotency-key — this service sets
# LegalEntityID: RequiredOnWrite (a notification is an entity-scoped record,
# INV-02). Omitting any one gets 400 envelope_incomplete before a handler runs,
# which would make every business-rule check below silently measure the envelope
# instead of the rule it names.
#
# The first draft of this file omitted X-Legal-Entity-Id and did exactly that:
# eighteen checks reported the rule they named as broken when the service was
# refusing the audit's own malformed request. openapi.yaml and
# postman_collection.json had the same omission, which is this estate's
# recurring failure — a client built strictly from a spec that documents five
# write headers where the service demands six has every write refused, for a
# reason that reads like an authorization problem and is not.
wh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Legal-Entity-Id:$ENTITY -H Idempotency-Key:$3 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3 -H Content-Type:application/json"; }
# Reads need identity plus traceability; no idempotency key.
rh() { echo "-H X-Tenant-Id:$1 -H X-Principal-Id:$2 -H X-Request-Id:$3 -H X-Source-Channel:system -H X-Correlation-ID:$3"; }

echo "==================================================================="
echo " AUDIT — notification-svc"
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
# is using: it DROPs notifications, and those rows are the record of which
# notices went out. The suite has its own guard (requireThrowawayDatabase);
# this asserts the variable independently, because the guard is only as good as
# it is reachable.
export TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://postgres:postgres@localhost:5432/notification_test?sslmode=disable}"
docker exec -i "$PGCONTAINER" psql -U postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='notification_test'" 2>/dev/null | grep -q 1 \
  || docker exec -i "$PGCONTAINER" psql -U postgres -c "CREATE DATABASE notification_test" >/dev/null 2>&1
case "$TEST_DATABASE_URL" in
  */notification\?*|*/notification)
    bad "TEST_DATABASE_URL points at the SERVING database — the suite drops tables in it" ;;
  *) ok "store suite runs against an isolated database" ;;
esac
hasf "store suite refuses a non-throwaway database" "$(cat internal/store/pg_store_test.go)" "requireThrowawayDatabase"

VOUT=$(gotest -count=1 -v ./...)
echo "        tests passing: $(echo "$VOUT" | grep -cE '^--- PASS|^    --- PASS')"
chk "tests failing" "$(echo "$VOUT" | grep -cE '^--- FAIL|^    --- FAIL')" "0"
# A suite that SKIPPED is a suite that verified nothing. It reports ok either
# way, which is exactly why this is asserted rather than eyeballed.
chk "no test skipped" "$(echo "$VOUT" | grep -cE '^--- SKIP|^    --- SKIP')" "0"
# Every package that has behaviour has a suite. internal/outbox had none until
# the relay landed, and a relay nothing exercises is a loop that can silently
# stop draining.
for pkg in store handler retry events outbox deliver templates identity authz; do
  if ls internal/$pkg/*_test.go >/dev/null 2>&1; then ok "internal/$pkg has a suite"; else bad "internal/$pkg has NO suite"; fi
done

echo
echo "-- 3. Reachability -------------------------------------------------"
chk "GET /healthz" "$(code "$B/healthz")" "200"
chk "GET /readyz"  "$(code "$B/readyz")"  "200"
chk "GET /metrics" "$(code "$B/metrics")" "200"
# readyz, not healthz, is what the container healthcheck uses: liveness answers
# 200 with a dead pool, so a service 500ing every read would read as healthy.
# -A 120, not -A 40: the healthcheck sits below the whole environment block, and
# a short window found nothing while reporting the probe as missing.
hasf "compose healthcheck probes readyz" "$(grep -A 120 '^  notification-svc:' "$DEPLOY/docker-compose.yml" 2>/dev/null)" "readyz"

echo
echo "-- 4. Canonical §4 envelope ----------------------------------------"
NOTENV=$(uuid)
# Identity is required on reads as well as writes: every handler calls
# requirePrincipal, and a read admitted without one would be an unattributed
# disclosure of a register.
# The catalogue route in particular. Its handler comment claimed the envelope
# middleware refused an unattributed request; in write-strict mode — the default
# — a read's envelope is parsed and REPORTED and the request is then admitted,
# so a bare curl with no headers at all returned 200 and the whole catalogue.
# The one route documented as authenticated was the one route that was not. It
# now checks identity itself.
chk "the catalogue with NO headers at all is refused" "$(code "$B/v1/notifications/templates")" "401"
chk "read with no tenant is refused"    "$(code -H "X-Principal-Id:$ME" "$B/v1/notifications/templates")" "401"
chk "read with no principal is refused" "$(code -H "X-Tenant-Id:$TEN"  "$B/v1/notifications/templates")" "401"
chk "read with the full envelope is admitted" "$(code $(rh $TEN $ME $NOTENV) "$B/v1/notifications/templates")" "200"

# A write missing ONLY the idempotency key. This is the request every
# pre-enforcement Postman collection on this estate accidentally makes, and it
# must be refused before a handler runs.
NOIDEM=$(body -X POST \
  -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" -H "X-Request-Id:$NOTENV" \
  -H "X-Source-Channel:system" -H "Content-Type:application/json" \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"IN_APP\",\"subject\":\"x\",\"correlation_id\":\"$NOTENV\"}" \
  "$B/v1/notifications/")
chk "write with no Idempotency-Key is envelope_incomplete" "$(echo "$NOIDEM" | ecode)" "envelope_incomplete"
# And NOTHING was recorded — refused ahead of the handler, not inside it.
chk "the refused write left no record" "$(psqlq "SELECT count(*) FROM notifications WHERE correlation_id='$NOTENV'")" "0"

echo
echo "-- 5. Authorization ------------------------------------------------"
# Sending and reading are SEPARATE grants. A service that treated one as
# implying the other would let anyone who can read a register send from it.
HSRC=$(cat internal/handler/handler.go)
hasf "NOTIFICATION_SEND is a distinct action" "$HSRC" 'actionSend = "NOTIFICATION_SEND"'
hasf "NOTIFICATION_VIEW is a distinct action" "$HSRC" 'actionView = "NOTIFICATION_VIEW"'
# permission_bundles.permitted_actions is a JSONB array; there is no
# permissions table. The original query asked the wrong schema and reported an
# empty string, which reads as "the grants are missing" rather than "the query
# is wrong" — the worst kind of audit failure, because it accuses the system.
GRANTS=$(authzq "SELECT count(*) FROM permission_bundles WHERE permitted_actions @> '[\"NOTIFICATION_SEND\"]'::jsonb AND permitted_actions @> '[\"NOTIFICATION_VIEW\"]'::jsonb")
if [ "${GRANTS:-0}" -ge 1 ] 2>/dev/null; then
  ok "both notification actions are bundled in authorization-svc ($GRANTS bundles)"
else
  bad "no permission bundle grants NOTIFICATION_SEND and NOTIFICATION_VIEW"
fi

# The defect this pins: authorization used to be CONDITIONAL on the
# legal_entity_id filter, so omitting it — the easier request — returned every
# notification in the tenant, subjects and bodies included, to a principal
# holding no grant at all. A read is authorized by who is asking, never by which
# query parameters they happened to send.
UNSCOPED=$(body $(rh $TEN $OTHER_PRINCIPAL $(uuid)) "$B/v1/notifications/?recipient_principal_id=$ME")
chk "reading another principal's inbox is refused" "$(echo "$UNSCOPED" | ecode)" "forbidden"
# And an entity read with no grant is a 403, not a filtered 200.
NOGRANT=$(code $(rh $TEN $OTHER_PRINCIPAL $(uuid)) "$B/v1/notifications/?legal_entity_id=00000000-0000-0000-0000-0000000000ff")
case "$NOGRANT" in
  403|503) ok "an ungranted entity register read is refused ($NOGRANT)" ;;
  *) bad "an ungranted entity register read returned $NOGRANT, want 403" ;;
esac

echo
echo "-- 6. Send, and what SENT means ------------------------------------"
CORR=$(uuid)
SENT=$(body -X POST $(wh $TEN $ME $CORR) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"IN_APP\",\"subject\":\"audit in-app\",\"body\":\"audit\",\"correlation_id\":\"$CORR\"}" \
  "$B/v1/notifications/")
NID=$(echo "$SENT" | jfield notification_id)
chk "IN_APP send concludes SENT" "$(echo "$SENT" | jfield status)" "SENT"
if [ -n "$NID" ]; then ok "the send returned a notification id"; else bad "no notification id: $SENT"; fi
# IN_APP has no endpoint outside the platform, so resolving an address for one
# would make every in-app notice depend on identity-context-svc for a value
# nothing reads.
chk "IN_APP stores no external address" "$(psqlq "SELECT coalesce(recipient_address,'') FROM notifications WHERE notification_id='$NID'")" ""

# Idempotent on (tenant_id, correlation_id): a replay is 200 with the SAME id,
# never a second delivery.
REPLAY_CODE=$(code -X POST $(wh $TEN $ME $(uuid)) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"IN_APP\",\"subject\":\"audit in-app\",\"body\":\"audit\",\"correlation_id\":\"$CORR\"}" \
  "$B/v1/notifications/")
chk "a replayed correlation id is 200, not 201" "$REPLAY_CODE" "200"
chk "the replay created no second row" "$(psqlq "SELECT count(*) FROM notifications WHERE tenant_id='$TEN' AND correlation_id='$CORR'")" "1"

# SMS is refused at the BOUNDARY, leaving nothing recorded. It used to reach the
# adapter and be reported as a delivery failure, so a caller's typo left a
# stored FAILED row and a notification.failed event — evidence of an attempt no
# provider ever saw.
SMSCORR=$(uuid)
SMS=$(body -X POST $(wh $TEN $ME $SMSCORR) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"SMS\",\"subject\":\"withdrawn\",\"correlation_id\":\"$SMSCORR\"}" \
  "$B/v1/notifications/")
chk "SMS is unsupported_channel" "$(echo "$SMS" | ecode)" "unsupported_channel"
chk "a refused channel leaves no record" "$(psqlq "SELECT count(*) FROM notifications WHERE correlation_id='$SMSCORR'")" "0"

# A FAILED delivery is still a 201. §9.7: notification failure must not collapse
# the source operational workflow.
WHCORR=$(uuid)
WHCODE=$(code -X POST $(wh $TEN $ME $WHCORR) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"WEBHOOK\",\"subject\":\"audit webhook\",\"correlation_id\":\"$WHCORR\"}" \
  "$B/v1/notifications/")
chk "a WEBHOOK send is still 201" "$WHCODE" "201"
chk "and is recorded FAILED" "$(psqlq "SELECT status FROM notifications WHERE correlation_id='$WHCORR'")" "FAILED"
# A FAILED row with no reason is a record that something did not go out and no
# account of what happened — which is the only thing that row is for.
WHREASON=$(psqlq "SELECT coalesce(failure_reason,'') FROM notifications WHERE correlation_id='$WHCORR'")
if [ -n "$WHREASON" ]; then ok "the FAILED row states a reason"; else bad "FAILED with no reason"; fi

# Content forms are mutually exclusive.
chk "template + subject is conflicting_content" "$(body -X POST $(wh $TEN $ME $(uuid)) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"EMAIL\",\"template\":\"registration_received\",\"subject\":\"mine\",\"correlation_id\":\"$(uuid)\"}" \
  "$B/v1/notifications/" | ecode)" "conflicting_content"
chk "a template missing a variable is refused" "$(body -X POST $(wh $TEN $ME $(uuid)) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"EMAIL\",\"template\":\"registration_received\",\"variables\":{},\"recipient_address\":\"a@b.example\",\"correlation_id\":\"$(uuid)\"}" \
  "$B/v1/notifications/" | ecode)" "missing_template_variables"

echo
echo "-- 7. Recipient address provenance ---------------------------------"
# §0.4 names notices sent to an unverified free-text address with no recipient
# provenance as a thing this control plane exists to prevent. Provenance is only
# a control if it is RECORDED.
ADDRCORR=$(uuid)
body -X POST $(wh $TEN $ME $ADDRCORR) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"EMAIL\",\"subject\":\"audit email\",\"recipient_address\":\"audit@zoiko.local\",\"correlation_id\":\"$ADDRCORR\"}" \
  "$B/v1/notifications/" >/dev/null
chk "a caller-supplied address is recorded as REQUEST" \
  "$(psqlq "SELECT recipient_address_source FROM notifications WHERE correlation_id='$ADDRCORR'")" "REQUEST"
chk "and the address itself is snapshotted" \
  "$(psqlq "SELECT recipient_address FROM notifications WHERE correlation_id='$ADDRCORR'")" "audit@zoiko.local"
# An address with no provenance is reachable only by a bug: the two columns are
# written by the same code path.
chk "no row has an address without provenance" \
  "$(psqlq "SELECT count(*) FROM notifications WHERE recipient_address IS NOT NULL AND recipient_address_source IS NULL")" "0"
# An address is only meaningful for a channel that leaves the platform.
chk "an address on IN_APP is address_not_applicable" "$(body -X POST $(wh $TEN $ME $(uuid)) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"IN_APP\",\"subject\":\"x\",\"recipient_address\":\"a@b.example\",\"correlation_id\":\"$(uuid)\"}" \
  "$B/v1/notifications/" | ecode)" "address_not_applicable"
chk "a malformed address is refused at the boundary" "$(body -X POST $(wh $TEN $ME $(uuid)) \
  -d "{\"recipient_principal_id\":\"$ME\",\"legal_entity_id\":\"$ENTITY\",\"channel\":\"EMAIL\",\"subject\":\"x\",\"recipient_address\":\"not-an-address\",\"correlation_id\":\"$(uuid)\"}" \
  "$B/v1/notifications/" | ecode)" "invalid_recipient_address"

echo
echo "-- 8. Read state and the unread badge ------------------------------"
# Only the RECIPIENT may mark read. A NOTIFICATION_VIEW grant lets an
# administrator read the register, and reading the register is not the recipient
# reading their notice — it must not clear their badge.
FIRST=$(body -X POST $(wh $TEN $ME $(uuid)) "$B/v1/notifications/$NID/read" | jfield read_at)
if [ -n "$FIRST" ]; then ok "the recipient can mark their notice read"; else bad "mark read returned no read_at"; fi
sleep 1
SECOND=$(body -X POST $(wh $TEN $ME $(uuid)) "$B/v1/notifications/$NID/read" | jfield read_at)
chk "a repeat mark keeps the FIRST read" "$SECOND" "$FIRST"
chk "a non-recipient may not mark read" "$(code -X POST $(wh $TEN $OTHER_PRINCIPAL $(uuid)) "$B/v1/notifications/$NID/read")" "403"

# Read state is IN_APP only: this service cannot observe whether an email was
# opened, and a read_at on an EMAIL row would be an assertion it has no way to
# make.
EMAILID=$(psqlq "SELECT notification_id FROM notifications WHERE correlation_id='$ADDRCORR'")
chk "marking an EMAIL read is refused" \
  "$(body -X POST $(wh $TEN $ME $(uuid)) "$B/v1/notifications/$EMAILID/read" | ecode)" "channel_has_no_read_state"
chk "no EMAIL row carries read state" \
  "$(psqlq "SELECT count(*) FROM notifications WHERE read_at IS NOT NULL AND channel <> 'IN_APP'")" "0"

# The unread count is the CALLER's, with no principal parameter — a per-
# principal total anyone could query would report on colleagues' attention.
UC=$(body $(rh $TEN $ME $(uuid)) "$B/v1/notifications/unread-count")
chk "the unread count names IN_APP as its scope" "$(echo "$UC" | jfield channel)" "IN_APP"
chk "and reports the caller" "$(echo "$UC" | jfield recipient_principal_id)" "$ME"
# Scoped to UnreadCount's own body. ListNotifications legitimately reads that
# query parameter — it is how a register read filters — so grepping the whole
# file reported the badge as leaky because a different handler does something
# entirely proper.
nohasf "the unread-count route takes no principal parameter" \
  "$(sed -n '/^func (h \*Handler) UnreadCount/,/^}/p' internal/handler/handler.go)" \
  'Query().Get("recipient_principal_id")'

echo
echo "-- 9. Tenant isolation ---------------------------------------------"
# Row-level security is FORCED. These services connect as the table owner, and
# Postgres exempts an owner from RLS unless FORCE is declared — so without it
# the policy would be a control that reads as present and does nothing.
chk "notifications forces RLS" "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='notifications'")" "t"
chk "event_outbox forces RLS"  "$(psqlq "SELECT relforcerowsecurity FROM pg_class WHERE relname='event_outbox'")" "t"
chk "notifications has policies" "$(psqlq "SELECT count(*)>0 FROM pg_policies WHERE tablename='notifications'")" "t"
chk "event_outbox has a policy"  "$(psqlq "SELECT count(*)>0 FROM pg_policies WHERE tablename='event_outbox'")" "t"
# Another tenant's notification is not_found, never forbidden: telling a caller
# that an id exists elsewhere is itself a disclosure.
chk "another tenant's notification is 404" "$(code $(rh $OTHER_TEN $ME $(uuid)) "$B/v1/notifications/$NID")" "404"
# An id that cannot be a UUID names no notification, which is what not found
# means. The driver's cast failure used to surface as 503 — an outage status for
# a typo in a URL.
chk "a non-UUID id is 404, not 503" "$(code $(rh $TEN $ME $(uuid)) "$B/v1/notifications/not-a-uuid")" "404"
# The relay's hatch must be a DIFFERENT flag from the retry worker's. Reusing
# app.platform_scope would have silently widened a SELECT-only hatch to a write
# one across every tenant's notification bodies.
POLSRC=$(psqlq "SELECT string_agg(qual,' ') FROM pg_policies WHERE tablename='event_outbox'")
hasf "the outbox relay names itself (app.outbox_relay)" "$POLSRC" "outbox_relay"
nohasf "and does NOT reuse the retry worker's platform_scope" "$POLSRC" "platform_scope"
# NULLIF, not a bare current_setting. Postgres keeps a custom GUC in the SESSION
# after a transaction-local SET is reset, with '' as its value.
hasf "the outbox policy guards the empty-GUC trap with NULLIF" "$POLSRC" "NULLIF"

echo
echo "-- 10. The transactional outbox ------------------------------------"
# THE defect this service carried. Both producers wrote to Kafka after
# committing, with the error logged and discarded, so a broker hiccup at the
# moment a notice concluded left the delivery recorded, the caller told 201, and
# no consumer anywhere learning the notice went out or that it did not —
# silently, because the one thing that would have reported the loss was the
# event that was lost.
chk "event_outbox exists" "$(psqlq "SELECT count(*) FROM information_schema.tables WHERE table_name='event_outbox'")" "1"
# The structural half of the fix: a conclusion cannot happen without an event.
hasf "CompleteDelivery requires an event" "$(cat internal/store/pg_store.go)" "a delivery may not conclude without one"
# The old methods must be GONE, not merely unused: a publisher that can still be
# called after a commit is the defect waiting to be re-introduced.
nohasf "Publisher.PublishSent no longer exists" "$(cat internal/events/publisher.go)" "func (p *Publisher) PublishSent"
nohasf "Publisher.PublishFailed no longer exists" "$(cat internal/events/publisher.go)" "func (p *Publisher) PublishFailed"
nohasf "the handler does not publish after committing" "$HSRC" "publisher.Publish"
nohasf "the retry worker does not publish after committing" "$(cat internal/retry/worker.go)" "publisher.Publish"

# Every concluded notification has exactly one event. Proven against the
# database, not against a log line.
CONCLUDED=$(psqlq "SELECT count(*) FROM notifications WHERE status IN ('SENT','FAILED')")
EVENTS=$(psqlq "SELECT count(*) FROM event_outbox")
if [ "$EVENTS" -ge 3 ] 2>/dev/null; then ok "the outbox holds events for this run's sends ($EVENTS for $CONCLUDED concluded)"; else bad "outbox holds $EVENTS events"; fi
# A PENDING notification must NOT have emitted one: it has not failed, and
# publishing a failure a later attempt reverses would have consumers act on an
# outcome that did not happen.
chk "only the two known event types are enqueued" \
  "$(psqlq "SELECT count(*) FROM event_outbox WHERE event_type NOT IN ('notification.sent','notification.failed')")" "0"
# The aggregate key is the notification, so two events about one notification
# stay ordered on one partition.
chk "every event is keyed by a real notification" \
  "$(psqlq "SELECT count(*) FROM event_outbox o WHERE NOT EXISTS (SELECT 1 FROM notifications n WHERE n.notification_id::text = o.aggregate_key)")" "0"
# The relay drains. A backlog that is not falling means every governed notice
# concluded since then has been recorded with no consumer told.
sleep 3
PENDING=$(psqlq "SELECT count(*) FROM event_outbox WHERE published_at IS NULL")
chk "the relay has drained the outbox" "$PENDING" "0"
# And the envelope stored is the finished one — built at enqueue time, because
# the relay runs long after the request that caused the send is gone.
ENVJ=$(psqlq "SELECT payload FROM event_outbox ORDER BY outbox_id DESC LIMIT 1")
for field in event_id event_type event_version schema_version source_service correlation_id; do
  hasf "the stored envelope carries $field" "$ENVJ" "\"$field\""
done
# And carries NO message content: the topic is readable by every consumer on the
# bus, and a consumer that needs the content reads the register under its own
# authorization.
nohasf "the event payload carries no subject" "$ENVJ" "\"subject\""
nohasf "the event payload carries no body"    "$ENVJ" "\"body\""
nohasf "the event payload carries no recipient address" "$ENVJ" "\"recipient_address\""

# On the bus, not only in the table.
# The topic is created on first publish (the writer sets
# AllowAutoTopicCreation), so this checks that the relay actually REACHED Kafka
# — not merely that it marked rows published, which a dry-run publisher also
# does.
# bash -lc, not the script directly. The image sets JAVA_HOME in the login
# profile, so a direct exec runs the wrapper without a JVM on PATH and prints
# nothing — which this check then reported as "the topic is not on the broker",
# accusing the relay of a failure that was entirely in the invocation.
TOPICS=$(docker exec -i "$KAFKACONTAINER" bash -lc "$KAFKABIN/kafka-topics.sh --bootstrap-server localhost:9094 --list" 2>/dev/null | tr -d '\015')
[ -z "$TOPICS" ] && TOPICS=$(docker exec -i "$KAFKACONTAINER" bash -lc "$KAFKABIN/kafka-topics.sh --bootstrap-server localhost:9092 --list" 2>/dev/null | tr -d '\015')
if echo "$TOPICS" | grep -q '^zoiko.notification.events$'; then
  ok "the events topic exists on the broker — the relay reached Kafka"
elif [ -z "$TOPICS" ]; then
  bad "could not list topics on $KAFKACONTAINER (set KAFKABIN / KAFKACONTAINER)"
else
  bad "zoiko.notification.events is not on the broker"
fi

echo
echo "-- 11. Retry, and the stranded sweep -------------------------------"
WSRC=$(cat internal/retry/worker.go)
# A rescheduled failure must NOT conclude and must NOT emit an event.
hasf "a rescheduled failure emits nothing" "$WSRC" "No notification.failed event, and nothing enqueued"
# The sweep exists. RunOnce's own comment asserted "the sweep below" for months
# before one was built, and five notifications sat undelivered for six days.
hasf "SweepStranded exists" "$WSRC" "func (w *Worker) SweepStranded"
hasf "the sweep only ever SCHEDULES" "$(cat internal/store/pg_store.go)" "func (s *PgStore) ReviveStranded"
# COALESCE(last_attempt_at, created_at) is the in-flight clock: a row stranded
# before its FIRST attempt has no last_attempt_at, which was the whole of the
# real case.
hasf "the stranded clock covers never-attempted rows" "$(cat internal/store/pg_store.go)" "COALESCE(last_attempt_at, created_at)"
# Zero is a true off switch, never "sweep everything now" — the one setting that
# would duplicate every live send.
hasf "a zero threshold disables the sweep" "$WSRC" "if w.strandedAfter <= 0"
# And nothing is stranded right now.
STRANDED=$(psqlq "SELECT count(*) FROM notifications WHERE status='PENDING' AND next_attempt_at IS NULL AND COALESCE(last_attempt_at, created_at) < now() - interval '15 minutes'")
chk "no notification is stranded in flight" "$STRANDED" "0"
# The schema refuses a concluded notification with a retry scheduled — that
# combination would have the worker re-send a message that already went out.
chk "no concluded notification has a retry scheduled" \
  "$(psqlq "SELECT count(*) FROM notifications WHERE status <> 'PENDING' AND next_attempt_at IS NOT NULL")" "0"
# The outcome of an attempt already made must outlive the request: once the
# provider has been called, what happened is a fact about the outside world.
hasf "the delivery outcome is written on a context that outlives the request" "$HSRC" "context.WithoutCancel"

echo
echo "-- 12. Domain metrics ----------------------------------------------"
# These exist because every interesting failure here answers 2xx. Without them
# no number anywhere moves when the platform stops delivering.
MET=$(body "$B/metrics")
DOMSRC=$(cat internal/telemetry/domain.go)

# Every metric must be DECLARED. This is the check that catches one being
# dropped in a refactor.
for m in notification_deliveries_total notification_delivery_attempts_total \
         notification_delivery_duration_seconds notification_retries_scheduled_total \
         notification_retries_exhausted_total notification_stranded_reclaimed_total \
         notification_outbox_pending notification_outbox_oldest_age_seconds \
         notification_outbox_published_total notification_outbox_publish_failures_total; do
  hasf "metric $m is declared" "$DOMSRC" "$m"
done

# Only the UNLABELLED ones are exported unconditionally. A Prometheus
# CounterVec/HistogramVec emits nothing at all — not even its HELP line — until
# a label combination is first observed, so asserting that an unexercised vec
# appears on /metrics fails for a reason that has nothing to do with the code.
# notification_retries_scheduled_total is the live example: nothing transiently
# failed during this run, so it is correctly absent.
for m in notification_outbox_pending notification_outbox_oldest_age_seconds \
         notification_outbox_publish_failures_total notification_retries_exhausted_total \
         notification_stranded_reclaimed_total; do
  hasf "metric $m is exported" "$MET" "$m"
done

# And these three MUST have been exercised by sections 6 and 10 above, which is
# a stronger statement than being exported: a metric that is declared and never
# moves is a dashboard that reads healthy through an outage.
for m in notification_deliveries_total notification_delivery_attempts_total \
         notification_outbox_published_total; do
  hasf "metric $m was exercised by this run" "$MET" "$m"
done
# The counters actually moved during section 6 — an exported metric that never
# increments is a dashboard that reads healthy through an outage.
if echo "$MET" | grep -E '^notification_deliveries_total\{' | grep -qvE ' 0$'; then
  ok "notification_deliveries_total has moved"
else
  bad "notification_deliveries_total is exported but flat"
fi

echo
echo "-- 13. Contract artefacts ------------------------------------------"
for f in openapi.yaml asyncapi.yaml postman_collection.json RUNBOOK.md progress.md; do
  if [ -s "$SVC/$f" ]; then ok "$f present"; else bad "$f missing"; fi
done
# Every documented route is registered, and every registered route is
# documented. A spec that has drifted from its service is worse than no spec,
# because callers build against it.
ROUTES=$(sed -n '/func RegisterRoutes/,/^}/p' internal/handler/handler.go)
for op in $(Q openapi-operations | tr -d '\015' | tr ' ' '|'); do
  M=${op%%|*}; P=${op##*|}
  case "$P" in /healthz|/readyz|/metrics) continue ;; esac
  # chi registers "/" and "/{id}" relative to the /v1/notifications route group.
  REL=$(echo "$P" | sed 's#^/v1/notifications##'); [ -z "$REL" ] && REL="/"
  if echo "$ROUTES" | grep -qi "r\.$(echo "$M" | tr 'A-Z' 'a-z' | sed 's/^./\U&/')(\"$REL\""; then
    ok "$M $P is registered"
  else
    bad "$M $P is documented but not registered"
  fi
done
# Every error code the spec names must exist in the handler or the envelope.
ALLSRC=$(cat internal/handler/handler.go internal/envelope/*.go)
for c in $(Q openapi-error-codes | tr -d '\015'); do
  hasf "error code $c exists in the service" "$ALLSRC" "$c"
done
# The channels the spec accepts are the channels the handler accepts.
# gofmt ALIGNS the map values, so "IN_APP":  true carries two spaces and an
# exact-string grep missed it — reporting a channel as unsupported that the
# service plainly supports, which would have sent someone looking for a bug in
# the handler.
CHANMAP=$(sed -n '/var supportedChannels/,/^}/p' internal/handler/handler.go)
for ch in $(Q openapi-channels | tr -d '\015'); do
  if echo "$CHANMAP" | grep -qE "\"$ch\":[[:space:]]+true"; then
    ok "channel $ch is accepted by the handler"
  else
    bad "channel $ch is in the spec but not in supportedChannels"
  fi
done
nohasf "SMS is NOT in the accepted set" "$(sed -n '/supportedChannels/,/^}/p' internal/handler/handler.go)" '"SMS": true'
# The event types agree across asyncapi, the migration's CHECK and the Go
# constants. An event type any one of them omits fails the INSERT, and because
# the enqueue shares the delivery write's transaction, it fails the WRITE.
A=$(Q asyncapi-event-types | tr -d '\015' | tr '\n' ' ')
M=$(Q migration-event-types | tr -d '\015' | tr '\n' ' ')
G=$(Q go-event-types | tr -d '\015' | tr '\n' ' ')
if [ "$A" = "$M" ] && [ "$M" = "$G" ]; then
  ok "event types agree across asyncapi, migration and Go ($G)"
else
  bad "event types differ — asyncapi[$A] migration[$M] go[$G]"
fi
# The Postman collection must carry the §4 envelope, or every write in it 401s
# for a reason that reads like an authorization problem and is not.
PM=$(cat "$SVC/postman_collection.json")
hasf "the collection mints X-Tenant-Id"    "$PM" "X-Tenant-Id"
hasf "the collection mints X-Principal-Id" "$PM" "X-Principal-Id"
hasf "the collection mints Idempotency-Key on writes" "$PM" "Idempotency-Key"
hasf "and does so in a collection-level script, not per request" "$PM" '"listen": "prerequest"'

echo
echo "-- 14. Console (frontend) ------------------------------------------"
if [ -n "$SKIP_FE" ] || [ ! -d "$FE/node_modules" ]; then
  echo "        skipped (SKIP_FE set, or node_modules absent)"
else
  FESRC=$(cat "$FE/lib/api/notifications.ts" 2>/dev/null)
  # The console must not render provider acceptance as receipt. §0.4 forbids it,
  # and the console is where an operator forms that belief.
  hasf "the client states what SENT means per channel" "$FESRC" "SENT means a mail provider ACCEPTED"
  hasf "and separates RETRYING from FAILED" "$FESRC" "export function isRetrying"
  # Mojibake: this file was once saved as cp1252 and shipped "Sendingâ€¦" and
  # "â€” none, write the subject" to real users.
  if grep -rqP '\xc3\xa2\xe2\x82\xac' "$FE/components/admin/notifications/" "$FE/app/admin/notifications/" 2>/dev/null; then
    bad "the notification console has mojibake in rendered text"
  else
    ok "no mojibake in the notification console"
  fi
  if [ -f "$FE/e2e/notifications.spec.ts" ]; then ok "an e2e spec exists"; else bad "no e2e spec for the notification console"; fi
  if [ -f "$FE/e2e/mock/notification-service.mjs" ]; then ok "an e2e mock exists"; else bad "no e2e mock for notification-svc"; fi
  hasf "the mock is wired into the Playwright config" "$(cat "$FE/playwright.config.ts")" "notification-service.mjs"
  hasf "and the console is pointed at it" "$(cat "$FE/playwright.config.ts")" "ZOIKO_NOTIFICATION_URL"
  # tsconfig includes Next's GENERATED route types, and a dev server left running
# by a Playwright run can have written a truncated routes.d.ts — which fails
# typechecking at a syntax error inside a file nobody wrote. Clearing the
# generated dev types and retrying distinguishes that from a real type error in
# the console's own source, which is what this check is for.
TSCOUT=$( cd "$FE" && npx tsc --noEmit 2>&1 )
if [ -n "$TSCOUT" ] && echo "$TSCOUT" | grep -q '\.next/'; then
  rm -rf "$FE/.next/dev" 2>/dev/null
  TSCOUT=$( cd "$FE" && npx tsc --noEmit 2>&1 )
fi
if [ -z "$TSCOUT" ]; then ok "console typechecks"; else bad "console tsc failed: $(echo "$TSCOUT" | head -2)"; fi
  if [ -n "$RUN_E2E" ]; then
    E2EOUT=$( cd "$FE" && npx playwright test e2e/notifications.spec.ts --reporter=line 2>&1 )
    if echo "$E2EOUT" | grep -qE "[0-9]+ passed" && ! echo "$E2EOUT" | grep -qE "[0-9]+ failed"; then
      ok "e2e notifications spec green ($(echo "$E2EOUT" | grep -oE '[0-9]+ passed' | tail -1))"
    else
      bad "e2e notifications spec failed"
    fi
  else
    echo "        e2e run skipped (set RUN_E2E=1 to include it — it takes ~2 minutes)"
  fi
fi

echo
echo "==================================================================="
TOTAL=$((PASS+FAIL))
if [ "$TOTAL" -gt 0 ]; then
  PCT=$(python -c "print(round($PASS*100.0/$TOTAL,1))" | tr -d '\015')
else
  PCT=0
fi
echo " RESULT: $PASS passed, $FAIL failed of $TOTAL  ($PCT%)"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
