#!/usr/bin/env bash
# Audit test — evidence-requirements-svc only.
#
# Re-runnable proof, against the running stack, that this service does what
# its spec says: static checks, live health, the allow path, every refusal, the
# API-3 / API-4 contract, telemetry, and the OpenAPI contract. The dependency
# edges (document vault, manifest, Kafka, gateway, …) are proven by
# deployments/scripts/e2e-evidence-requirements-deps.py; this script proves the
# service's own behaviour.
#
# Requires: the service on $B (default http://127.0.0.1:8130), identity-context
# on $IDENTITY (default :8080) for a real envelope, the demo seeds, a Go
# toolchain, curl, and python on PATH (or PY=/path/to/python).
#
# Exits with the number of failed checks, so CI can gate on it.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://127.0.0.1:8130}
IDENTITY=${IDENTITY:-http://127.0.0.1:8080}
PY=${PY:-python}

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=99999999-9999-4999-8999-999999999999
PRIN=33333333-3333-3333-3333-333333333333
ENTITY=22222222-2222-2222-2222-222222222222
EMAIL=${EMAIL:-admin@zoikosuite.com}
PASSWORD=${PASSWORD:-Zoiko@Governance1}
STAMP=$(date +%s)

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }

uuid() { "$PY" -c "import uuid;print(uuid.uuid4())"; }
field() { "$PY" -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: d={}
v=d
for k in '$1'.split('.'):
    v=v.get(k,'') if isinstance(v,dict) else ''
print(v)"; }

# req METHOD PATH [BODY] [extra curl args…] — prints "<status> <body>" with the
# canonical envelope. R_TEN, R_PRIN and R_ENT override the identity headers for
# one call; set one to empty to send no such header at all.
req() { local m=$1 p=$2 body=${3:-}; shift 2; [ $# -gt 0 ] && shift
  local ten=${R_TEN-$TEN} prin=${R_PRIN-$PRIN} ent=${R_ENT-$ENTITY}
  local args=(-s -m 20 -o "$TMP/body" -w '%{http_code}' -X "$m" "$B$p"
    -H "X-Correlation-ID: audit-$STAMP" -H "X-Request-Id: audit-$(uuid)" -H "X-Source-Channel: api"
    -H "Content-Type: application/json")
  [ -n "$ten" ] && args+=(-H "X-Tenant-Id: $ten")
  [ -n "$prin" ] && args+=(-H "X-Principal-Id: $prin")
  [ -n "$ent" ] && args+=(-H "X-Legal-Entity-Id: $ent")
  [ -n "$body" ] && args+=(--data-binary "$body")
  local s; s=$(curl "${args[@]}" "$@"); printf '%s %s' "$s" "$(cat "$TMP/body")"; }
status() { echo "${1%% *}"; }
body() { echo "${1#* }"; }

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

echo "==================================================================="
echo " AUDIT — evidence-requirements-svc"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')   service: $B"
echo "==================================================================="

echo
echo "-- 1. Static analysis ----------------------------------------------"
cd "$SVC" || exit 1
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi
if [ -z "$(go vet -tags=integration ./internal/store/ 2>&1)" ]; then ok "go vet -tags=integration (store) clean"; else bad "go vet -tags=integration"; fi
unformatted=$(for f in $(find . -name '*.go'); do tr -d '\r' < "$f" | gofmt -l >/dev/null 2>&1 || true; [ -n "$(tr -d '\r' < "$f" | gofmt -l 2>&1)" ] && echo "$f"; done)
if [ -z "$unformatted" ]; then ok "gofmt clean"; else bad "gofmt: $unformatted"; fi
missing_down=$(for u in deployments/migrations/*.up.sql; do [ -f "${u%.up.sql}.down.sql" ] || echo "$u"; done)
if [ -z "$missing_down" ]; then ok "every migration has a .down.sql"; else bad "no .down.sql: $missing_down"; fi
if grep -q "/cursor" openapi.yaml; then bad "openapi.yaml still documents a /cursor route"; else ok "no /cursor routes in the contract"; fi
for f in asyncapi.yaml RUNBOOK.md RELEASE_CERTIFICATE.md SPEC_DEVIATIONS.md; do
  if [ -s "$f" ]; then ok "$f present"; else bad "$f missing"; fi
done
if grep -q "evidence.requirement.satisfied" asyncapi.yaml && grep -q "evidence.requirement.missing" asyncapi.yaml; then
  ok "asyncapi.yaml lists both events"; else bad "asyncapi.yaml does not list both events"; fi

echo
echo "-- 2. Live health ---------------------------------------------------"
chk "GET /healthz" "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$B/healthz")" 200
chk "GET /readyz" "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$B/readyz")" 200
metrics=$(curl -s -m 10 "$B/metrics")
for series in http_requests_total readiness_up evidence_evaluations_total evidence_owner_unavailable_total evidence_outbox_unpublished; do
  if echo "$metrics" | grep -q "^$series"; then ok "/metrics serves $series"; else bad "/metrics lacks $series"; fi
done

echo
echo "-- 3. Allow path (a real envelope) --------------------------------"
login=$(curl -s -m 20 -X POST "$IDENTITY/v1/authenticate" -H "Content-Type: application/json" \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"client_id\":\"zoiko-console\",\"client_secret\":\"local-dev-console-client-secret\"}")
access=$(echo "$login" | field access_token)
JWT=""
if [ -n "$access" ]; then
  JWT=$(curl -s -m 20 -X POST "$IDENTITY/v1/context/resolve" -H "Content-Type: application/json" \
    -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" -H "X-Request-Id: audit-$(uuid)" -H "Idempotency-Key: $(uuid)" \
    -H "X-Source-Channel: api" -H "X-Correlation-ID: audit-$STAMP" \
    -d "{\"bearer_token\":\"$access\",\"legal_entity_id\":\"$ENTITY\"}" | field envelope_jwt)
fi
if [ -n "$JWT" ]; then ok "envelope minted by identity-context"; else bad "no envelope (login or resolve failed)"; fi
r=$(req GET /v1/evidence/summary "" -H "Authorization: Bearer $JWT"); chk "summary with the verified envelope" "$(status "$r")" 200
r=$(req POST /v1/evidence/evaluate "{\"legal_entity_id\":\"$ENTITY\",\"domain_code\":\"AUDIT\",\"action_type\":\"AUDIT_$STAMP\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $(uuid)")
chk "evaluate an ungated action" "$(status "$r")" 200
chk "  …outcome" "$(body "$r" | field outcome)" NO_REQUIREMENTS_DEFINED

echo
echo "-- 4. Refusals ------------------------------------------------------"
r=$(R_TEN= req GET /v1/evidence/summary ""); chk "no tenant" "$(status "$r")" 401
r=$(R_PRIN= req GET /v1/evidence/summary ""); chk "no principal" "$(status "$r")" 401
if [ -n "$JWT" ]; then
  r=$(req GET /v1/evidence/summary "" -H "Authorization: Bearer ${JWT%????}AAAA"); chk "tampered envelope (warn mode too)" "$(status "$r")" 401
fi
r=$(req GET "/v1/evidence-requirements?tenant_id=$OTHER_TEN" ""); chk "another tenant named in the query" "$(status "$r")" 403
r=$(req GET "/v1/evidence-requirements?filter%5Bcolour%5D=red" ""); chk "unknown filter" "$(status "$r")" 400
r=$(req GET "/v1/evidence-requirements?legal_entity_id=$ENTITY" ""); chk "bare (pre-API-4) filter" "$(status "$r")" 400
r=$(req GET "/v1/evidence/evaluations?cursor=forged.cursor" ""); chk "tampered cursor" "$(status "$r")" 400
r=$(req GET "/v1/evidence/evaluations?page_size=201" ""); chk "page_size above 200" "$(status "$r")" 400
stranger=$(uuid)
r=$(R_ENT=$stranger req POST /v1/evidence/evaluate "{\"legal_entity_id\":\"$stranger\",\"domain_code\":\"AUDIT\",\"action_type\":\"X\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $(uuid)")
chk "a legal entity not in the tenant" "$(status "$r")" 422
r=$(req POST /v1/evidence/evaluate "{\"legal_entity_id\":\"$(uuid)\",\"domain_code\":\"AUDIT\",\"action_type\":\"X\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $(uuid)")
chk "body entity disagrees with X-Legal-Entity-Id" "$(status "$r")" 400
key=$(uuid)
req POST /v1/evidence/evaluate "{\"legal_entity_id\":\"$ENTITY\",\"domain_code\":\"AUDIT\",\"action_type\":\"AUDIT_$STAMP\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $key" >/dev/null
r=$(req POST /v1/evidence/evaluate "{\"legal_entity_id\":\"$ENTITY\",\"domain_code\":\"AUDIT\",\"action_type\":\"OTHER_$STAMP\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $key")
chk "same Idempotency-Key, different action" "$(status "$r")" 409
"$PY" -c "print('{\"legal_entity_id\":\"x\",\"pad\":\"' + 'a'*300000 + '\"}')" > "$TMP/big.json"
r=$(req POST /v1/evidence/evaluate "" -H "Idempotency-Key: $(uuid)" --data-binary "@$TMP/big.json"); chk "body over 256 KiB" "$(status "$r")" 413
r=$(req POST /v1/admin/evidence-requirements "{\"tenant_id\":\"$TEN\",\"domain_code\":\"AUDIT\",\"action_type\":\"AUDIT_$STAMP\",\"evidence_type\":\"APPROVAL_RECORD\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $(uuid)")
chk "create a requirement" "$(status "$r")" 201
rid=$(body "$r" | field evidence_requirement_id)
r=$(req POST "/v1/admin/evidence-requirements/$rid/end-date" '{"reason":"audit"}' -H "Idempotency-Key: $(uuid)"); chk "end-date without If-Match" "$(status "$r")" 428
r=$(req POST "/v1/admin/evidence-requirements/$rid/end-date" '{"reason":"audit"}' -H 'If-Match: "v7"' -H "Idempotency-Key: $(uuid)"); chk "end-date with a stale If-Match" "$(status "$r")" 412
r=$(req POST "/v1/admin/evidence-requirements/$rid/end-date" '{"reason":"audit"}' -H 'If-Match: "v1"' -H "Idempotency-Key: $(uuid)"); chk "end-date with the current If-Match" "$(status "$r")" 200
r=$(req POST "/v1/admin/evidence-requirements/$rid/end-date" '{"reason":"audit"}' -H 'If-Match: "v2"' -H "Idempotency-Key: $(uuid)"); chk "end-date again (already retired)" "$(status "$r")" 422
r=$(req POST /v1/admin/evidence-requirements "{\"tenant_id\":\"$OTHER_TEN\",\"domain_code\":\"AUDIT\",\"action_type\":\"X\",\"evidence_type\":\"APPROVAL_RECORD\",\"correlation_id\":\"c\"}" -H "Idempotency-Key: $(uuid)")
chk "create into another tenant" "$(status "$r")" 403

echo
echo "-- 5. Problem documents -------------------------------------------"
ctype=$(curl -s -m 10 -o /dev/null -w '%{content_type}' "$B/v1/evidence/summary" -H "X-Tenant-Id: $TEN" -H "X-Request-Id: audit-$(uuid)")
chk "a refusal is application/problem+json" "$ctype" "application/problem+json"

echo
echo "-- 6. Contract -----------------------------------------------------"
# The parity and validity tests read openapi.yaml against the router. Run
# them in a container when the host blocks Go test binaries (Windows
# Application Control): GO_TEST_IN_DOCKER=1.
if [ "${GO_TEST_IN_DOCKER:-0}" = "1" ]; then
  SRC_DIR=$(cd "$SVC/.." && { pwd -W 2>/dev/null || pwd; })
  out=$(MSYS_NO_PATHCONV=1 docker run --rm -e GOFLAGS=-mod=mod -e GOPROXY=off -e CGO_ENABLED=0 \
    -v "$SRC_DIR:/src" -v "${GOMODCACHE:-$HOME/go/pkg/mod}:/go/pkg/mod" \
    -w /src/evidence-requirements-svc golang:1.26-alpine go test -count=1 -run 'OpenAPI' ./internal/handler/ 2>&1)
else
  out=$(go test -count=1 -run 'OpenAPI' ./internal/handler/ 2>&1)
fi
if echo "$out" | grep -q "^ok"; then ok "openapi.yaml valid and matches the router"; else bad "OpenAPI contract tests: $(echo "$out" | tail -1)"; fi

echo
echo "==================================================================="
echo " $PASS passed, $FAIL failed"
echo "==================================================================="
exit "$FAIL"
