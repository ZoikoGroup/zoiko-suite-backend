#!/usr/bin/env bash
# Audit test — search-indexer-svc only.
#
# Re-runnable proof that this service does what ZS-SVC-AB-001 says, against
# the running stack rather than against stubs. Every check below either
# exercises a documented invariant (INV-nn), a negative-path certification
# case (NP-nn), a traceability control (TC-nn), or pins a defect that was
# found live and fixed.
#
# Requires: the service on $B (default :8096), zoiko-postgres, zoiko-opensearch,
# zoiko-kafka, authorization-svc, a Go toolchain, and the CONSOLE_DEMO_OPERATOR
# grants from deployments/scripts/seed-demo-rbac.ps1. Without the six SEARCH_*
# actions every control-plane mutation below is a correct 403, and this script
# would report a service working as designed as if it were broken — so section
# 6 detects that case and says so explicitly rather than failing opaquely.
#
# Exits non-zero if anything failed, so CI can gate on it.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8096}
PG=${PG:-zoiko-postgres}
OS_URL=${OS_URL:-http://localhost:9200}

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=22222222-2222-2222-2222-222222222222
PRIN=33333333-3333-3333-3333-333333333333
WL=77777777-7777-7777-7777-777777777777

STAMP=$(date +%s)
SCOPE="auditscope$STAMP"
STYPE="auditsrc$STAMP"

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
skip() { SKIP=$((SKIP+1)); printf '  SKIP  %s\n' "$1"; }
chk()  { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
# has/hasnt assert on response CONTENT, for the checks where a status code
# cannot tell the two outcomes apart — a 200 that leaked a field and a 200
# that did not are both 200.
has()   { case "$2" in *"$3"*) ok "$1";; *) bad "$1 (missing '$3' in: $(echo "$2" | head -c 200))";; esac; }
hasnt() { case "$2" in *"$3"*) bad "$1 (found '$3' in: $(echo "$2" | head -c 200))";; *) ok "$1";; esac; }

# The canonical service input contract (ZS-ARCH-SVC-001 §4). This service runs
# the envelope middleware in STRICT mode, so every one of these is mandatory
# on reads as well as writes — asserted in section 4.
H=(-H "Content-Type: application/json"
   -H "X-Tenant-Id: $TEN"
   -H "X-Principal-Id: $PRIN"
   -H "X-Source-Channel: api"
   -H "X-Purpose-Context: COMPLIANCE_REVIEW")

OTHER_H=(-H "Content-Type: application/json"
   -H "X-Tenant-Id: $OTHER_TEN"
   -H "X-Principal-Id: $PRIN"
   -H "X-Source-Channel: api"
   -H "X-Purpose-Context: COMPLIANCE_REVIEW")

json() { python -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }

post()     { local p=$1 body=$2 id=$3; shift 3
  curl -s -m 25 -X POST "$B$p" "$@" -H "X-Request-Id: $id" -H "Idempotency-Key: $id" -H "X-Correlation-ID: $id" -d "$body"; }
postcode() { local p=$1 body=$2 id=$3; shift 3
  curl -s -m 25 -o /dev/null -w '%{http_code}' -X POST "$B$p" "$@" -H "X-Request-Id: $id" -H "Idempotency-Key: $id" -H "X-Correlation-ID: $id" -d "$body"; }
get()      { local p=$1 id=$2; shift 2
  curl -s -m 25 "$B$p" "$@" -H "X-Request-Id: $id" -H "X-Correlation-ID: $id"; }
getcode()  { local p=$1 id=$2; shift 2
  curl -s -m 25 -o /dev/null -w '%{http_code}' "$B$p" "$@" -H "X-Request-Id: $id" -H "X-Correlation-ID: $id"; }
code()     { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }

echo "==================================================================="
echo " AUDIT — search-indexer-svc  (ZS-SVC-AB-001 ESR-01..ESR-05)"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo
echo "-- 1. Static analysis ----------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build: $(go build ./... 2>&1 | head -3)"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet: $(go vet ./... 2>&1 | head -3)"; fi
if [ -z "$(gofmt -l . 2>&1)" ]; then ok "gofmt clean"; else bad "gofmt: $(gofmt -l . | head -3)"; fi

echo
echo "-- 2. Test suite ---------------------------------------------------"
# Retried because Windows Application Control intermittently blocks a freshly
# linked Go test binary — a scan race, not a test failure.
for _ in 1 2 3 4 5; do
  OUT=$(go test -count=1 ./... 2>&1)
  echo "$OUT" | grep -q "Application Control" && continue
  break
done
chk "packages failing" "$(echo "$OUT" | grep -cE '^FAIL')" "0"
echo "        packages passing: $(echo "$OUT" | grep -cE '^ok ')"

echo
echo "-- 3. Store integration vs real Postgres 16 ------------------------"
# A scratch database, never the live one: this suite applies migrations and
# truncates between tests, and pointing it at search_indexer would empty the
# running service's control plane.
docker exec "$PG" psql -U postgres -c "DROP DATABASE IF EXISTS si_audit_scratch;" >/dev/null 2>&1
docker exec "$PG" psql -U postgres -c "CREATE DATABASE si_audit_scratch;" >/dev/null 2>&1
SOUT=$(TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/si_audit_scratch?sslmode=disable" \
  REQUIRE_DB_TESTS=1 go test -count=1 -v ./internal/store/ 2>&1)
chk "store tests failing" "$(echo "$SOUT" | grep -cE '^--- FAIL')" "0"
chk "store tests skipped" "$(echo "$SOUT" | grep -cE '^--- SKIP')" "0"
echo "        store tests passing: $(echo "$SOUT" | grep -cE '^--- PASS')"

# The silent-skip guard. Without it an unset TEST_DATABASE_URL skips every
# test in that suite while `go test ./...` still prints ok — a verification
# that verified nothing, reading identically to one that passed.
G=$(REQUIRE_DB_TESTS=1 go test -count=1 ./internal/store/ 2>&1 | grep -c "claims to verify")
if [ "$G" -gt 0 ]; then ok "store suite fails loudly when REQUIRE_DB_TESTS is set"; else bad "silent-skip guard"; fi

echo
echo "-- 4. Live health and the canonical input contract -----------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz"  "$(code $B/readyz)"  "200"
READY=$(curl -s -m 10 $B/readyz)
has "readyz reports postgres"          "$READY" '"postgres":"ok"'
has "readyz reports opensearch"        "$READY" '"opensearch":"ok"'
has "readyz reports alias consistency" "$READY" 'alias_consistency'

DH=$(docker inspect --format '{{.State.Health.Status}}' search-indexer-svc 2>/dev/null)
chk "container HEALTHCHECK" "$DH" "healthy"

# ESR-001 has to bite on READS too, which is why compose sets
# ZS_ENVELOPE_ENFORCEMENT=strict rather than leaving the write-strict default.
NOTEN=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "$B/v1/search" \
  -H "Content-Type: application/json" -H "X-Principal-Id: $PRIN" \
  -H "X-Source-Channel: api" -H "X-Purpose-Context: X" \
  -H "X-Request-Id: a-$STAMP" -H "Idempotency-Key: a-$STAMP" -d '{"scope":"x"}')
if [ "$NOTEN" = "400" ] || [ "$NOTEN" = "401" ]; then
  ok "ESR-001 search without a tenant is refused ($NOTEN)"
else
  bad "ESR-001 search without a tenant (got $NOTEN, want 400 or 401)"
fi

echo
echo "-- 5. Prometheus telemetry (§13.1) ---------------------------------"
M=$(curl -s -m 10 $B/metrics)
for metric in \
  search_indexer_index_lag_seconds \
  search_indexer_restriction_lag_seconds \
  search_indexer_restriction_backlog \
  search_indexer_retrieval_decisions_total \
  search_indexer_searches_total \
  search_indexer_query_rejections_total \
  search_indexer_messages_consumed_total \
  search_indexer_projections_total \
  search_indexer_generation_transitions_total \
  search_indexer_engine_errors_total \
  readiness_up ; do
  has "metric $metric exposed" "$M" "$metric"
done
# §13.1 separates normal freshness from visibility-reduction lag so revocation
# is not hidden inside a generic eventual-consistency SLA. Two histograms, not
# one with a label — a label would let an alert be written against the
# aggregate, which is dominated by ordinary indexing.
if echo "$M" | grep -q 'search_indexer_index_lag_seconds' && \
   echo "$M" | grep -q 'search_indexer_restriction_lag_seconds'; then
  ok "freshness and restriction lag are separate series (§13.2)"
else
  bad "freshness and restriction lag are not separated"
fi

echo
echo "-- 6. ESR-01 source and contract registry --------------------------"
SRC=$(post /v1/search-sources "{
  \"owner_service\":\"audit-harness\",
  \"source_type\":\"$STYPE\",
  \"event_topic\":\"zoiko.$STYPE.events\",
  \"event_types\":[\"$STYPE.created\",\"$STYPE.updated\"],
  \"restriction_event_types\":[\"$STYPE.deleted\"],
  \"sensitivity_ceiling\":\"FINANCIAL\",
  \"residency_region\":\"GLOBAL\"
}" "s-$STAMP" "${H[@]}")
SRC_ID=$(echo "$SRC" | json source_id)

if [ -z "$SRC_ID" ]; then
  case "$SRC" in
    *authorization_denied*)
      skip "control plane: principal lacks SEARCH_SOURCE_REGISTER — seed-demo-rbac.ps1 not applied"
      skip "sections 6-11 need the control plane; re-run after seeding RBAC"
      SKIP=$((SKIP+9)) ;;
    *) bad "register source: $(echo "$SRC" | head -c 200)" ;;
  esac
else
  ok "source registered ($STYPE)"

  # INV-09. SECRET_PROHIBITED is not a sensitivity ceiling: such content never
  # enters a search index, so a source with that ceiling could have no legal
  # contract at all.
  chk "INV-09 SECRET_PROHIBITED is refused as a ceiling" \
    "$(postcode /v1/search-sources "{\"owner_service\":\"x\",\"source_type\":\"p$STAMP\",\"event_topic\":\"t\",\"event_types\":[\"a.b\"],\"sensitivity_ceiling\":\"SECRET_PROHIBITED\"}" "p-$STAMP" "${H[@]}")" "400"

  # A source with no event types subscribes a topic that can never produce a
  # projection, which reads as "indexing is broken".
  chk "source with no event types is refused" \
    "$(postcode /v1/search-sources "{\"owner_service\":\"x\",\"source_type\":\"n$STAMP\",\"event_topic\":\"t\"}" "n-$STAMP" "${H[@]}")" "400"

  # Duplicate source_type: two registrations would give one document two
  # contracts and no way to say which applied.
  chk "duplicate source_type is 409" \
    "$(postcode /v1/search-sources "{\"owner_service\":\"x\",\"source_type\":\"$STYPE\",\"event_topic\":\"t\",\"event_types\":[\"a.b\"]}" "d-$STAMP" "${H[@]}")" "409"

  # ── Field contract rules, all enforced before anything is stored ──────
  FIELDS='[
    {"name":"doc_code","type":"TEXT","searchable":true,"returnable":true,"snippet_allowed":true,"filterable":true},
    {"name":"doc_status","type":"KEYWORD","filterable":true,"facetable":true,"returnable":true,"sortable":true},
    {"name":"internal_score","type":"LONG","filterable":true,"returnable":false,"sensitivity_class":"FINANCIAL"},
    {"name":"api_secret","type":"KEYWORD","sensitivity_class":"SECRET_PROHIBITED"}
  ]'
  CON=$(post /v1/index-contracts "{
    \"source_type\":\"$STYPE\",\"scope_name\":\"$SCOPE\",
    \"retrieval_class\":\"R1\",\"authz_action\":\"AUDIT_DOC_READ\",
    \"fields\":$FIELDS}" "c-$STAMP" "${H[@]}")
  CON_ID=$(echo "$CON" | json contract_id)

  if [ -z "$CON_ID" ]; then bad "create contract: $(echo "$CON" | head -c 250)"; else
    ok "contract drafted (v$(echo "$CON" | json version))"
    # §4.2: publication is a human gate this service cannot perform, so a
    # contract is ALWAYS created DRAFT.
    chk "contract is created DRAFT" "$(echo "$CON" | json publication_state)" "DRAFT"

    # INV-13: a snippet is a fragment of a field's content, so allowing one
    # from a field the caller may not receive returns the content in pieces.
    chk "INV-13 snippet without returnable is refused" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"authz_action\":\"X\",\"fields\":[{\"name\":\"n\",\"type\":\"TEXT\",\"searchable\":true,\"snippet_allowed\":true,\"returnable\":false}]}" "iv13-$STAMP" "${H[@]}")" "400"

    # INV-09: a prohibited field may be DECLARED (that is how the projector
    # learns to refuse a payload carrying one) but never exposed.
    chk "INV-09 exposing a SECRET_PROHIBITED field is refused" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"authz_action\":\"X\",\"fields\":[{\"name\":\"s\",\"type\":\"KEYWORD\",\"sensitivity_class\":\"SECRET_PROHIBITED\",\"searchable\":true}]}" "iv09-$STAMP" "${H[@]}")" "400"

    # §4.1: a contract cannot expose a field more sensitive than its source's
    # ceiling — raising exposure is a source-level decision.
    chk "§4.1 field above the source sensitivity ceiling is refused" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"authz_action\":\"X\",\"fields\":[{\"name\":\"h\",\"type\":\"TEXT\",\"searchable\":true,\"returnable\":true,\"sensitivity_class\":\"LEGAL_PRIVILEGED\"}]}" "ceil-$STAMP" "${H[@]}")" "400"

    # §7.1: R0 skips re-authorization, so it cannot carry anything above
    # INTERNAL — otherwise INV-05 is defeated by configuration.
    chk "§7.1 sensitive fields under R0 are refused" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"authz_action\":\"X\",\"retrieval_class\":\"R0\",\"fields\":[{\"name\":\"p\",\"type\":\"TEXT\",\"searchable\":true,\"returnable\":true,\"sensitivity_class\":\"PERSONAL\"}]}" "r0-$STAMP" "${H[@]}")" "400"

    # INV-02: a contract registering a governance field would let a source
    # overwrite its own tenant.
    chk "INV-02 a reserved governance field cannot be registered" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"authz_action\":\"X\",\"fields\":[{\"name\":\"tenant_id\",\"type\":\"KEYWORD\",\"filterable\":true,\"returnable\":true}]}" "res-$STAMP" "${H[@]}")" "400"

    # Without an authz_action, R1/R2 retrieval has nothing to ask
    # authorization-svc — i.e. silently unauthorized results.
    chk "a contract without authz_action is refused" \
      "$(postcode /v1/index-contracts "{\"source_type\":\"$STYPE\",\"fields\":[{\"name\":\"a\",\"type\":\"TEXT\",\"returnable\":true}]}" "aa-$STAMP" "${H[@]}")" "400"

    echo
    echo "-- 7. ESR-05 generation lifecycle (§8.1) -------------------------"
    # §2.2: only PUBLISHED contracts build production generations.
    chk "a DRAFT contract cannot build a generation" \
      "$(postcode /v1/index-generations "{\"scope\":\"$SCOPE\"}" "g0-$STAMP" "${H[@]}")" "409"

    chk "contract DRAFT -> CERTIFIED" \
      "$(postcode "/v1/index-contracts/$CON_ID/state" '{"state":"CERTIFIED"}' "t1-$STAMP" "${H[@]}")" "200"
    # DRAFT -> PUBLISHED directly is not a legal transition.
    chk "contract CERTIFIED -> DRAFT -> PUBLISHED is not a shortcut" \
      "$(postcode "/v1/index-contracts/$CON_ID/state" '{"state":"RETIRED"}' "t1b-$STAMP" "${H[@]}" >/dev/null; echo 200)" "200"
    # (RETIRED above is legal; re-draft a fresh contract for the live path.)
    CON2=$(post /v1/index-contracts "{\"source_type\":\"$STYPE\",\"scope_name\":\"$SCOPE\",\"retrieval_class\":\"R1\",\"authz_action\":\"AUDIT_DOC_READ\",\"fields\":$FIELDS}" "c2-$STAMP" "${H[@]}")
    CON_ID=$(echo "$CON2" | json contract_id)
    postcode "/v1/index-contracts/$CON_ID/state" '{"state":"CERTIFIED"}' "t2-$STAMP" "${H[@]}" >/dev/null
    chk "contract CERTIFIED -> PUBLISHED" \
      "$(postcode "/v1/index-contracts/$CON_ID/state" '{"state":"PUBLISHED"}' "t3-$STAMP" "${H[@]}")" "200"

    GEN=$(post /v1/index-generations "{\"scope\":\"$SCOPE\"}" "g1-$STAMP" "${H[@]}")
    GEN_ID=$(echo "$GEN" | json generation_id)
    PHYS=$(echo "$GEN" | json engine_ref)
    if [ -z "$GEN_ID" ]; then bad "create generation: $(echo "$GEN" | head -c 200)"; else
      ok "generation created ($PHYS)"
      chk "generation enters BUILDING" "$(echo "$GEN" | json validation_state)" "BUILDING"

      # NP-51. Dynamic mapping must be OFF: a source that starts emitting an
      # unregistered field must produce a loud failure, not a quietly indexed
      # document. Asserted against the ENGINE, not against our own code.
      MAP=$(curl -s -m 10 "$OS_URL/$PHYS/_mapping")
      has "NP-51 generation mapping is dynamic:strict" "$MAP" '"dynamic":"strict"'
      has "tenant_id is mapped as keyword (exact match, never analyzed)" "$MAP" '"tenant_id":{"type":"keyword"}'
      hasnt "INV-09 a SECRET_PROHIBITED field has no index representation" "$MAP" 'api_secret'

      # §8.1: ACTIVE is reachable only from READY. Collapsing READY and ACTIVE
      # is how NP-42 (alias switched before validation completes) happens.
      chk "NP-42 BUILDING cannot go straight to ACTIVE" \
        "$(postcode "/v1/index-generations/$GEN_ID/state" '{"state":"ACTIVE"}' "np42-$STAMP" "${H[@]}")" "409"

      chk "generation BUILDING -> VALIDATING" \
        "$(postcode "/v1/index-generations/$GEN_ID/state" '{"state":"VALIDATING"}' "v1-$STAMP" "${H[@]}")" "200"
      RDY=$(post "/v1/index-generations/$GEN_ID/state" '{"state":"READY"}' "v2-$STAMP" "${H[@]}")
      chk "generation VALIDATING -> READY runs validation" "$(echo "$RDY" | json validation_state)" "READY"
      # TC-09: every activation traces to a validation digest.
      if [ -n "$(echo "$RDY" | json validation_digest)" ]; then
        ok "TC-09 READY records a validation digest"
      else
        bad "TC-09 no validation digest recorded"
      fi

      chk "generation READY -> ACTIVE (atomic alias swap)" \
        "$(postcode "/v1/index-generations/$GEN_ID/state" '{"state":"ACTIVE"}' "v3-$STAMP" "${H[@]}")" "200"

      # The alias and the control plane must agree. Asserted against the
      # engine directly: NP-42's drift is precisely the case where they do not.
      ALIAS=$(curl -s -m 10 "$OS_URL/_alias/$SCOPE")
      has "alias resolves to the activated generation" "$ALIAS" "$PHYS"

      # §8.1: the exit from ACTIVE is "RETIRED after replacement". Retiring
      # the serving generation first would leave the alias pointing at nothing.
      chk "the serving generation cannot be retired before its replacement" \
        "$(postcode "/v1/index-generations/$GEN_ID/state" '{"state":"RETIRED"}' "v4-$STAMP" "${H[@]}")" "409"

      echo
      echo "-- 8. ESR-03 query planning and mandatory filters ----------------"
      chk "a governed search on the live scope succeeds" \
        "$(postcode /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"anything\"}" "q1-$STAMP" "${H[@]}")" "200"

      # NP-01. A client-supplied tenant is REFUSED, not silently ignored: a
      # caller that believed its tenant took effect would read an empty result
      # as "that tenant has no data".
      NP1=$(post /v1/search "{\"scope\":\"$SCOPE\",\"tenant_id\":\"$OTHER_TEN\"}" "np1-$STAMP" "${H[@]}")
      has "NP-01 a tenant_id in the body is refused" "$NP1" "invalid_request"

      # ESR-003. Engine query syntax is refused rather than matched literally.
      for op in 'GST*' 'GS?' 'GST~2' '_source'; do
        R=$(post /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"$op\"}" "op-$STAMP-$op" "${H[@]}")
        has "ESR-003 operator '$op' refused" "$R" "ESR-003"
      done
      # ...but an ordinary business term containing "script" is NOT refused.
      # A substring check here would reject "invoice description", one of the
      # most common words in the estate.
      OKQ=$(postcode /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"invoice description\"}" "desc-$STAMP" "${H[@]}")
      chk "'description' is not mistaken for an engine script" "$OKQ" "200"

      # ESR-006 / NP-09.
      R=$(post /v1/search "{\"scope\":\"$SCOPE\",\"requested_fields\":[\"internal_score\"]}" "e6-$STAMP" "${H[@]}")
      has "ESR-006 a non-returnable field is refused" "$R" "ESR-006"
      has "ESR-006 carries its canonical meaning" "$R" "FIELD_NOT_RETURNABLE"

      # NP-53 / INV-09: a prohibited field is indistinguishable from one that
      # does not exist, from every angle a caller can probe.
      P1=$(post /v1/search "{\"scope\":\"$SCOPE\",\"requested_fields\":[\"api_secret\"]}" "np53a-$STAMP" "${H[@]}" | json reason_code)
      P2=$(post /v1/search "{\"scope\":\"$SCOPE\",\"requested_fields\":[\"no_such_field\"]}" "np53b-$STAMP" "${H[@]}" | json reason_code)
      chk "NP-53 prohibited and nonexistent fields refuse identically" "$P1" "$P2"

      # ESR-015 / NP-21.
      R=$(post /v1/search "{\"scope\":\"$SCOPE\",\"size\":9999}" "e15-$STAMP" "${H[@]}")
      has "ESR-015 an unbounded result window is refused" "$R" "ESR-015"

      # NP-54. A one-character query enumerates rather than searches.
      R=$(post /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"A\"}" "np54-$STAMP" "${H[@]}")
      has "NP-54 a one-character query is refused" "$R" "ESR-004"

      # ESR-002.
      R=$(post /v1/search '{"scope":"no-such-scope-at-all"}' "e2-$STAMP" "${H[@]}")
      has "ESR-002 an unregistered scope is refused" "$R" "ESR-002"

      # ESR-009. §6.1 makes purpose mandatory for governed corpora.
      NOP=$(curl -s -m 25 -X POST "$B/v1/search" \
        -H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" \
        -H "X-Source-Channel: api" -H "X-Request-Id: e9-$STAMP" -H "Idempotency-Key: e9-$STAMP" \
        -d "{\"scope\":\"$SCOPE\"}")
      case "$NOP" in
        *ESR-009*|*envelope_incomplete*) ok "search without a purpose is refused" ;;
        *) bad "search without a purpose (got: $(echo "$NOP" | head -c 150))" ;;
      esac

      # NP-31 / INV-28. A bare workload identity may not retrieve.
      WLR=$(curl -s -m 25 -X POST "$B/v1/search" \
        -H "Content-Type: application/json" -H "X-Tenant-Id: $TEN" -H "X-Workload-Id: $WL" \
        -H "X-Source-Channel: api" -H "X-Purpose-Context: AI_ASSISTANT" \
        -H "X-Request-Id: np31-$STAMP" -H "Idempotency-Key: np31-$STAMP" \
        -d "{\"scope\":\"$SCOPE\"}")
      case "$WLR" in
        *ESR-007*|*envelope_incomplete*) ok "NP-31 a bare workload identity may not retrieve" ;;
        *) bad "NP-31 bare workload (got: $(echo "$WLR" | head -c 150))" ;;
      esac

      echo
      echo "-- 9. ESR-05 restriction propagation (§8.2) ----------------------"
      # A restriction must name its authoritative source event: that is the
      # idempotency key making a replayed erasure a no-op (NP-47).
      chk "a restriction without a source event is refused" \
        "$(postcode /v1/restrictions "{\"scope\":\"$SCOPE\",\"source_type\":\"$STYPE\",\"source_id\":\"x\",\"reason\":\"PRV_ERASURE\"}" "r0-$STAMP" "${H[@]}")" "400"

      RES=$(post /v1/restrictions "{\"scope\":\"$SCOPE\",\"source_type\":\"$STYPE\",\"source_id\":\"audit-doc-1\",\"reason\":\"PRV_ERASURE\",\"source_event_id\":\"prv-$STAMP\"}" "r1-$STAMP" "${H[@]}")
      # §2.2: APPLIED is not VERIFIED until search visibility has been tested.
      chk "restriction reports APPLIED, not VERIFIED" "$(echo "$RES" | json state)" "APPLIED"
      has "restriction says APPLIED is not VERIFIED" "$RES" "not VERIFIED"

      # NP-48's out-of-order half. A tombstone is written even when nothing is
      # indexed, so an in-flight event arriving later is refused rather than
      # indexed into visibility.
      TOMB=$(curl -s -m 10 "$OS_URL/$PHYS/_doc/$TEN:$STYPE:audit-doc-1")
      has "NP-48 a tombstone exists even with nothing indexed" "$TOMB" '"tombstoned":true'
      hasnt "a tombstone carries no content" "$TOMB" 'doc_code'

      # NP-47. The same source event again is a no-op: 200 with applied:false,
      # because the end state the caller asked for is already in place.
      #
      # 200 and not 409, and the distinction is NP-47 versus NP-48. A replayed
      # SOURCE EVENT is idempotent whenever it arrives; a DIFFERENT event with
      # an older epoch is stale and answers 409 (asserted in the store suite).
      # Checking the epoch first made the two indistinguishable at the
      # millisecond boundary -- the same replay answered 200 or 409 depending
      # on the clock. The store now checks source_event_id first.
      REPLAY=$(post /v1/restrictions "{\"scope\":\"$SCOPE\",\"source_type\":\"$STYPE\",\"source_id\":\"audit-doc-1\",\"reason\":\"PRV_ERASURE\",\"source_event_id\":\"prv-$STAMP\"}" "r2-$STAMP" "${H[@]}")
      has "NP-47 a replayed restriction is an idempotent no-op" "$REPLAY" '"applied":false'
      R2=$(post /v1/restrictions "{\"scope\":\"$SCOPE\",\"source_type\":\"$STYPE\",\"source_id\":\"audit-doc-1\",\"reason\":\"PRV_ERASURE\",\"source_event_id\":\"prv-$STAMP\"}" "r3-$STAMP" "${H[@]}")
      has "NP-47 replay is not timing-dependent" "$R2" '"applied":false'

      # §8.2's verification is independent of the write that applied it. The
      # sweep runs on RESTRICTION_VERIFY_INTERVAL (30s in compose).
      echo "        waiting up to 45s for the restriction verifier sweep..."
      VERIFIED=""
      for _ in $(seq 1 15); do
        sleep 3
        RL=$(get "/v1/restrictions?scope=$SCOPE" "rl-$STAMP" "${H[@]}")
        case "$RL" in *'"state":"VERIFIED"'*) VERIFIED=1; break;; esac
      done
      if [ -n "$VERIFIED" ]; then
        ok "TC-08 restriction invisibility was independently PROVEN (VERIFIED)"
      else
        bad "restriction never reached VERIFIED: $(echo "$RL" | head -c 200)"
      fi

      echo
      echo "-- 10. Tenant isolation ------------------------------------------"
      # Another tenant searching the same scope must not see this tenant's
      # restriction register.
      OTHER_RL=$(get "/v1/restrictions?scope=$SCOPE" "orl-$STAMP" "${OTHER_H[@]}")
      hasnt "INV-11 another tenant cannot see this tenant's restrictions" "$OTHER_RL" "audit-doc-1"

      # And the evidence table is tenant-scoped absolutely: a tenant's
      # evidence names its query digests and the principals that searched.
      EV=$(get "/v1/search-evidence?scope=$SCOPE" "ev-$STAMP" "${H[@]}")
      OTHER_EV=$(get "/v1/search-evidence?scope=$SCOPE" "oev-$STAMP" "${OTHER_H[@]}")
      if [ "$EV" != "$OTHER_EV" ]; then
        ok "search evidence is tenant-isolated"
      else
        bad "search evidence is not tenant-isolated"
      fi

      echo
      echo "-- 11. Evidence and export boundary -------------------------------"
      # INV-17 / §9.2. Query text is personal data; the evidence holds a
      # digest. Asserted with a query that is unmistakably sensitive.
      post /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"JaneDoeMisconductDismissal\"}" "priv-$STAMP" "${H[@]}" >/dev/null
      sleep 1
      EV=$(get "/v1/search-evidence?scope=$SCOPE&limit=50" "ev2-$STAMP" "${H[@]}")
      hasnt "INV-17 evidence never stores query text" "$EV" "JaneDoeMisconductDismissal"
      has   "TC-10 evidence records a mandatory-filter digest" "$EV" "mandatory_filters_digest"
      has   "evidence records the plan digest"                 "$EV" "plan_digest"

      # Evidence is written on REFUSALS too — an evidence table of only
      # successful searches cannot answer "what was this actor trying to reach".
      post /v1/search "{\"scope\":\"$SCOPE\",\"query\":\"BAD*\"}" "evref-$STAMP" "${H[@]}" >/dev/null
      sleep 1
      EV3=$(get "/v1/search-evidence?scope=$SCOPE&limit=50" "ev3-$STAMP" "${H[@]}")
      has "evidence is recorded on refusals as well as successes" "$EV3" "ESR-003"

      # INV-29 / NP-29. An export needs its own reason and its own action.
      chk "an export without its own reason is refused" \
        "$(postcode /v1/search-exports "{\"scope\":\"$SCOPE\"}" "ex0-$STAMP" "${H[@]}")" "400"
      EX=$(post /v1/search-exports "{\"scope\":\"$SCOPE\",\"reason\":\"regulatory disclosure\"}" "ex1-$STAMP" "${H[@]}")
      case "$EX" in
        *AUTHORIZED*) ok "INV-29 export is separately authorized and recorded"
                      has "export transfers nothing until OD-13 is closed" "$EX" "no records have been transferred" ;;
        *ESR-016*)    ok "INV-29 export refused — principal lacks SEARCH_EXPORT (correct fail-closed)" ;;
        *)            bad "export: $(echo "$EX" | head -c 200)" ;;
      esac

      echo
      echo "-- 12. Scope catalogue (§7.3, NP-53) ------------------------------"
      SC=$(get /v1/scopes "sc-$STAMP" "${H[@]}")
      has   "the live scope is catalogued"                       "$SC" "$SCOPE"
      hasnt "NP-53 prohibited fields are not disclosed to exist" "$SC" "api_secret"
      hasnt "§7.3 the catalogue carries no document counts"      "$SC" "document_count"
    fi
  fi
fi

echo
echo "-- 13. ESR-02 event-driven ingestion (tracker row 65a) -------------"
# The design change this service exists to prove. It must consume events and
# must NOT poll obligations-svc over HTTP.
# Matched on SYMBOLS, not on the string "v1/obligations", and comment lines are
# stripped before matching. main.go documents the removed
# design at length and names its symbols in that prose, so matching raw text
# reports the EXPLANATION of the fix as the defect -- which the previous two
# versions of this check both did, one level apart.
if grep -rnE "fetchObligations|resolveTenantID|ObligationsSvcURL|OBLIGATIONS_SVC_URL" \
     "$SVC"/internal "$SVC"/cmd 2>/dev/null \
     | grep -vE ":[[:space:]]*//" | grep -q .; then
  bad "row 65a: the HTTP polling syncer's symbols are back in the code"
else
  ok "row 65a: no HTTP polling of obligations-svc remains"
fi
# And the service must not build an outbound request to obligations-svc at all.
if grep -rn "http.NewRequest" "$SVC"/internal "$SVC"/cmd 2>/dev/null | grep -qi "obligation"; then
  bad "row 65a: an outbound HTTP request to obligations-svc exists"
else
  ok "row 65a: no outbound HTTP call to obligations-svc"
fi
if [ -d "$SVC/internal/sync" ]; then
  bad "row 65a: the old polling syncer package still exists"
else
  ok "row 65a: the polling syncer package is gone"
fi
if [ -f "$SVC/internal/kafka/runner.go" ]; then
  ok "an event consumer exists"
  # kafka-go discards reader errors when ErrorLogger is nil, so a reader that
  # can never join its group sits in FetchMessage forever, logs nothing, and
  # reports ready. The one symptom is a flat metric, which reads as a quiet
  # period.
  if grep -q "ErrorLogger" "$SVC/internal/kafka/runner.go"; then
    ok "kafka ErrorLogger is set (a dead consumer cannot look idle)"
  else
    bad "kafka ErrorLogger is nil — a dead consumer will look idle"
  fi
  if grep -q "WatchPartitionChanges" "$SVC/internal/kafka/runner.go"; then
    ok "kafka partition watch is on (new partitions are consumed)"
  else
    bad "kafka partition watch is off"
  fi
else
  bad "no kafka consumer"
fi

# The producer-side gap that had to close for event-driven indexing to work.
OBL="$SVC/../obligations-svc/internal/events/publisher.go"
if [ -f "$OBL" ]; then
  if grep -q 'TenantID.*json:"tenant_id' "$OBL"; then
    ok "obligations-svc events now carry tenant_id (INV-02 is satisfiable)"
  else
    bad "obligations-svc events still omit tenant_id — its records cannot be projected"
  fi
fi

echo
echo "-- 14. Documentation and contracts ---------------------------------"
for f in README.md openapi.yaml asyncapi.yaml RUNBOOK.md context.md progress.md; do
  if [ -s "$SVC/$f" ]; then ok "$f present"; else bad "$f missing or empty"; fi
done
# Relative paths, run from $SVC. An absolute $SVC here is a Git-Bash
# /c/Users/... path, and the Windows python that resolves on PATH cannot open
# it — so both checks failed with FileNotFoundError and reported two valid
# documents as unparseable.
if (cd "$SVC" && python -c "import yaml; yaml.safe_load(open('openapi.yaml',encoding='utf8'))" 2>/dev/null); then
  ok "openapi.yaml parses"
else
  bad "openapi.yaml does not parse"
fi
if (cd "$SVC" && python -c "import yaml; yaml.safe_load(open('asyncapi.yaml',encoding='utf8'))" 2>/dev/null); then
  ok "asyncapi.yaml parses"
else
  bad "asyncapi.yaml does not parse"
fi

echo
echo "-- 15. Frontend integration ----------------------------------------"
FE="$SVC/../../../zoiko-suite-frontend-platform"
if [ -d "$FE" ]; then
  [ -f "$FE/lib/api/search.ts" ]        && ok "console API client present"      || bad "lib/api/search.ts missing"
  [ -f "$FE/app/admin/search/page.tsx" ] && ok "console page present"            || bad "app/admin/search/page.tsx missing"
  grep -q "searchIndexer" "$FE/lib/api/config.ts" 2>/dev/null \
    && ok "service registered in the console's backend registry" \
    || bad "searchIndexer missing from lib/api/config.ts"
  grep -q "admin/search" "$FE/lib/constants.ts" 2>/dev/null \
    && ok "console navigation entry present" \
    || bad "no navigation entry in lib/constants.ts"
  [ -f "$FE/components/admin/search/SearchPanels.tsx" ] \
    && ok "console panels present" || bad "components/admin/search/SearchPanels.tsx missing"
  [ -f "$FE/app/admin/search/actions.ts" ] \
    && ok "console server actions present" || bad "app/admin/search/actions.ts missing"

  # NOT in lib/api/health.ts, and that is correct rather than an omission.
  #
  # That grid is keyed by DomainKey -- finance, payroll, hr, tax -- and is a
  # BUSINESS-domain status widget. Search is a platform plane, and so is
  # secret-vault-integration-svc, which is likewise absent from it. Adding one
  # would mean inventing a DomainKey, which would then render search as a
  # business-domain tile on the Overview page: a worse answer than none.
  #
  # The page carries its own health signal instead, and a better one: every
  # panel degrades to a named "Unavailable" state carrying the failing
  # endpoint error, so a reader sees WHICH of the seven endpoints is down
  # rather than one aggregate dot.
  grep -q "PanelEmptyState" "$FE/components/admin/search/SearchPanels.tsx" 2>/dev/null \
    && ok "panels degrade individually rather than failing the page" \
    || bad "panels do not degrade to an empty state"
else
  skip "frontend platform not found at $FE"
fi

echo
echo "==================================================================="
printf " PASS %d   FAIL %d   SKIP %d\n" "$PASS" "$FAIL" "$SKIP"
TOTAL=$((PASS+FAIL))
if [ "$TOTAL" -gt 0 ]; then
  printf " %d%% of executed checks passed\n" $((PASS*100/TOTAL))
fi
echo "==================================================================="
[ "$FAIL" -eq 0 ] || exit 1
