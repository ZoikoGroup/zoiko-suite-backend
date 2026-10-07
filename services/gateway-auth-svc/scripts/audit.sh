#!/usr/bin/env bash
# Audit test — gateway-auth-svc only.
#
# Re-runnable proof that this service does what its spec says, against the
# running stack rather than against stubs. Every check below either exercises a
# documented behaviour or pins a defect that was found live and fixed.
#
# Requires: the service on $B (default :8092), identity-context-svc on :8080
# with access-control-svc and tenant-entity-registry-svc reachable (the envelope
# this service verifies can only be minted by the real two-hop flow), a Go
# toolchain, and Prometheus on :9090 for the alert-rule section.
#
# Exits non-zero if anything failed, so CI can gate on it.

SVC="${SVC:-$(cd "$(dirname "$0")/.." && pwd)}"
B=${B:-http://localhost:8092}
IDENTITY=${IDENTITY:-http://localhost:8080}

TEN=11111111-1111-1111-1111-111111111111
OTHER_TEN=99999999-9999-9999-9999-999999999999
PRIN=33333333-3333-3333-3333-333333333333
ENTITY=22222222-2222-2222-2222-222222222222
EMAIL=${EMAIL:-admin@zoikosuite.com}
PASSWORD=${PASSWORD:-Zoiko@Governance1}

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }

json() { python -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }
code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
# hdr NAME -- reads one response header from /verify, case-insensitively.
vhdr() { local name=$1; shift
  curl -s -m 25 -D- -o /dev/null "$B/verify" "$@" \
    | tr -d '\r' | awk -v n="$(echo "$name" | tr 'A-Z' 'a-z')" \
        'BEGIN{IGNORECASE=1} tolower($1)==n":"{$1=""; sub(/^ /,""); print}'; }

echo "==================================================================="
echo " AUDIT — gateway-auth-svc"
echo " $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "==================================================================="

echo
echo "-- 1. Static analysis ----------------------------------------------"
cd "$SVC" || exit 1
# GOTMPDIR is moved off the default temp path: Windows Application Control
# intermittently blocks a freshly linked Go test binary, which is a scan race
# and not a test failure. Building inside the service directory makes it much
# rarer but does not eliminate it, so section 2 also retries.
export GOTMPDIR="$SVC/.gotmp"
mkdir -p "$GOTMPDIR"
if [ -z "$(go build ./... 2>&1)" ]; then ok "go build ./... clean"; else bad "go build"; fi
if [ -z "$(go vet ./... 2>&1)" ]; then ok "go vet ./... clean"; else bad "go vet"; fi

echo
echo "-- 2. Test suite ---------------------------------------------------"
# Retried on top of the relocated GOTMPDIR, because moving the build
# directory reduces the Application Control scan race but does not eliminate
# it: a freshly linked test binary is occasionally blocked wherever it is
# written. Without the retry this section fails intermittently on a service
# whose tests are fine, which is worse than no check at all.
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

echo
echo "-- 3. Live health ---------------------------------------------------"
chk "GET /healthz" "$(code $B/healthz)" "200"
chk "GET /readyz"  "$(code $B/readyz)"  "200"
# The image's own HEALTHCHECK must run readyz, not healthz. Liveness answers 200
# whenever the process is up, so with an unreachable JWKS endpoint the container
# reported healthy while refusing every request in the estate.
DH=$(docker inspect --format '{{.State.Health.Status}}' gateway-auth-svc 2>/dev/null)
chk "container HEALTHCHECK" "$DH" "healthy"
HCMD=$(docker inspect --format '{{json .Config.Healthcheck.Test}}' gateway-auth-svc 2>/dev/null)
case "$HCMD" in
  *readyz*) ok "  ... and it probes /readyz, not /healthz" ;;
  *)        bad "  ... HEALTHCHECK probes $HCMD — a dead JWKS reads as healthy" ;;
esac

echo
echo "-- 4. Minting a real identity envelope ------------------------------"
# Not a stub. This service's whole job is verifying a signed envelope, so an
# audit that only ever exercised refusal paths would never prove the one path
# that matters works at all.
ACCESS=$(curl -s -m 25 -X POST "$IDENTITY/v1/authenticate" -H "Content-Type: application/json" \
  -d "{\"tenant_id\":\"$TEN\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | json access_token)
if [ -n "$ACCESS" ]; then ok "identity-context-svc issued an access token"; else bad "authenticate failed"; fi

RESOLVE=$(curl -s -m 25 -X POST "$IDENTITY/v1/context/resolve" -H "Content-Type: application/json" \
  -H "X-Tenant-Id: $TEN" -H "X-Principal-Id: $PRIN" -H "X-Legal-Entity-Id: $ENTITY" \
  -H "X-Source-Channel: api" -H "X-Request-Id: ga-$$" -H "X-Correlation-ID: ga-$$" -H "Idempotency-Key: ga-$$" \
  -d "{\"bearer_token\":\"$ACCESS\",\"legal_entity_id\":\"$ENTITY\"}")
ENVJWT=$(echo "$RESOLVE" | json envelope_jwt)
if [ -n "$ENVJWT" ]; then ok "  ... and resolved it into a signed envelope"; else bad "resolve failed: $(echo "$RESOLVE" | head -c 200)"; fi

AUTH=(-H "Authorization: Bearer $ENVJWT")

echo
echo "-- 5. The allow path -------------------------------------------------"
chk "POST-equivalent /verify with a real envelope" "$(code $B/verify "${AUTH[@]}")" "200"
chk "  ... forwards the verified principal" "$(vhdr X-Principal-Id "${AUTH[@]}")" "$PRIN"
chk "  ... forwards the verified tenant"    "$(vhdr X-Tenant-Id "${AUTH[@]}")" "$TEN"
chk "  ... forwards the legal entity"       "$(vhdr X-Legal-Entity-Id "${AUTH[@]}")" "$ENTITY"
# GOV-01: server-resolved, from the registry, not from the token or the caller.
JUR=$(vhdr X-Jurisdiction-Context "${AUTH[@]}")
if [ -n "$JUR" ]; then ok "  ... resolves jurisdiction context server-side ($JUR)"; else bad "no X-Jurisdiction-Context — GOV-01 resolution is not running"; fi
TZ=$(vhdr X-Timezone "${AUTH[@]}")
if [ -n "$TZ" ]; then ok "  ... resolves the operating timezone ($TZ)"; else bad "no X-Timezone"; fi
RES=$(vhdr X-Residency-Policy-Id "${AUTH[@]}")
if [ -n "$RES" ]; then ok "  ... resolves the residency policy"; else bad "no X-Residency-Policy-Id"; fi

# Provenance class S: "a client may request context but cannot override result."
# The caller asserts a different jurisdiction; the server-resolved value must win.
SPOOFED=$(vhdr X-Jurisdiction-Context "${AUTH[@]}" -H "X-Jurisdiction-Context: attacker-supplied")
chk "  ... a client-supplied context does not override it" "$SPOOFED" "$JUR"

echo
echo "-- 6. Refusals name themselves ---------------------------------------"
# One route, one status code for four different situations. Without the reason
# header they are indistinguishable to the caller and to the console.
chk "no credential at all" "$(code $B/verify)" "401"
chk "  ... reason" "$(vhdr X-Auth-Denial-Reason)" "no_token"
chk "a credential that does not verify" "$(code $B/verify -H 'Authorization: Bearer abc.def.ghi')" "401"
chk "  ... reason" "$(vhdr X-Auth-Denial-Reason -H 'Authorization: Bearer abc.def.ghi')" "invalid_token"
chk "a non-bearer scheme" "$(code $B/verify -H 'Authorization: Basic dXNlcjpwYXNz')" "401"
chk "  ... reason" "$(vhdr X-Auth-Denial-Reason -H 'Authorization: Basic dXNlcjpwYXNz')" "no_token"

echo
echo "-- 7. Bearer scheme is case-insensitive (RFC 7235 §4.2) --------------"
# A strict HasPrefix("Bearer ") refused "bearer <token>" — a spelling several
# HTTP clients emit by default — with the same 401 as a forged credential.
# Proven by the REASON, not the status: both spellings answer 401 for a garbage
# token, and only the reason says whether the scheme was recognised.
for scheme in Bearer bearer BEARER BeArEr; do
  chk "  $scheme <garbage> is read as a bearer credential" \
    "$(vhdr X-Auth-Denial-Reason -H "Authorization: $scheme abc.def.ghi")" "invalid_token"
done
chk "  a real envelope under a lowercase scheme is allowed" \
  "$(code $B/verify -H "authorization: bearer $ENVJWT")" "200"

echo
echo "-- 8. Token/hostname tenant binding (GTRM §6.2) ----------------------"
# A validly-signed token presented against a DIFFERENT tenant's hostname. The
# only refusal here that is evidence of an attack rather than a
# misconfiguration: by construction the token verified, so whoever sent it
# holds genuine credentials.
chk "a good token against another tenant's hostname" \
  "$(code $B/verify "${AUTH[@]}" -H "X-Zoiko-Resolved-Tenant-Id: $OTHER_TEN")" "403"
chk "  ... reason" \
  "$(vhdr X-Auth-Denial-Reason "${AUTH[@]}" -H "X-Zoiko-Resolved-Tenant-Id: $OTHER_TEN")" "tenant_hostname_mismatch"
# NOT attributed to the risk engine. This used to set X-Carta-Decision, sending
# anyone reading it to carta-svc's logs for a decision that was never made.
CARTA=$(vhdr X-Carta-Decision "${AUTH[@]}" -H "X-Zoiko-Resolved-Tenant-Id: $OTHER_TEN")
if [ -z "$CARTA" ]; then ok "  ... and is not reported as a CARTA decision"; else bad "  ... still reports X-Carta-Decision: $CARTA"; fi
# The matching case must not be a false positive.
chk "  ... the matching hostname still passes" \
  "$(code $B/verify "${AUTH[@]}" -H "X-Zoiko-Resolved-Tenant-Id: $TEN")" "200"
# Absence is not suspicious: it means the route is not on GTRM-resolved routing
# yet, so no comparison is made — an honest pass, not a fabricated one.
chk "  ... absence of the header is not treated as a mismatch" "$(code $B/verify "${AUTH[@]}")" "200"

echo
echo "-- 9. Probes stay ungated --------------------------------------------"
# /verify is the endpoint that ESTABLISHES tenant and actor from the token, so
# it cannot also demand them as envelope input — and the probes must answer
# without any credential at all or nothing can ever report this service's health.
chk "GET /healthz with no headers" "$(code $B/healthz)" "200"
chk "GET /readyz with no headers"  "$(code $B/readyz)"  "200"
chk "GET /metrics with no headers" "$(code $B/metrics)" "200"

echo
echo "-- 10. Telemetry -----------------------------------------------------"
# This service had NO metrics at all: no endpoint, no Prometheus dependency, no
# scrape job, no alert rules. It is the ForwardAuth target for every gated
# request in the estate — the first thing to fail and, until now, the least
# visible.
M=$(curl -s -m 20 $B/metrics)
DOMAIN=$(echo "$M" | grep -c '^gateway_auth_')
if [ "$DOMAIN" -ge 15 ]; then ok "domain metric series emitted ($DOMAIN)"; else bad "domain metrics ($DOMAIN, want >= 15)"; fi
for fam in gateway_auth_verify_decisions_total gateway_auth_jwks_errors_total \
           gateway_auth_tenant_context_total gateway_auth_carta_decisions_total; do
  if echo "$M" | grep -q "^$fam"; then ok "  $fam"; else bad "  $fam missing"; fi
done
echo "$M" | grep -q '^readiness_up' && ok "  readiness_up gauge present" || bad "  readiness_up missing"

# The counters must reflect what this run actually did.
ALLOWED=$(echo "$M" | grep '^gateway_auth_verify_decisions_total{.*outcome="allowed"' | awk '{print $2}')
if [ "${ALLOWED%%.*}" -ge 1 ] 2>/dev/null; then ok "  allows counted ($ALLOWED)"; else bad "  allows not counted ($ALLOWED)"; fi
MISMATCH=$(echo "$M" | grep '^gateway_auth_verify_decisions_total{.*outcome="tenant_hostname_mismatch"' | awk '{print $2}')
if [ "${MISMATCH%%.*}" -ge 1 ] 2>/dev/null; then ok "  spoofing attempts counted ($MISMATCH)"; else bad "  spoofing not counted ($MISMATCH)"; fi

# Every outcome label must EXIST as a series from startup, at zero. A series
# that does not exist and a series reading zero are indistinguishable to an
# alert — so a rule on "any spoofing attempt" would evaluate against nothing at
# all until the first one happened, staying silent through exactly what it was
# written for.
MISSING=0
for o in allowed no_token invalid_token incomplete_claims tenant_hostname_mismatch \
         carta_blocked tenant_context_denied tenant_context_unresolved; do
  echo "$M" | grep -q "outcome=\"$o\"" || { MISSING=$((MISSING+1)); echo "        missing series: $o"; }
done
chk "outcome series absent before first occurrence" "$MISSING" "0"

echo
echo "-- 11. Prometheus is actually scraping it ----------------------------"
# A /metrics endpoint nobody scrapes is not observability. This service had no
# scrape job, so even once the endpoint existed the alerts would have evaluated
# against no data.
TARGETS=$(curl -s -m 20 "http://localhost:9090/api/v1/targets" 2>/dev/null)
if [ -n "$TARGETS" ]; then
  UP=$(echo "$TARGETS" | python -c "
import sys,json
d=json.load(sys.stdin)
print(sum(1 for t in d['data']['activeTargets']
          if t['labels'].get('job')=='gateway-auth-svc' and t['health']=='up'))" 2>/dev/null)
  chk "scrape target up" "$UP" "1"
else
  echo "  SKIP  prometheus not reachable on :9090"
fi

echo
echo "-- 12. Alert rules ---------------------------------------------------"
RULES=$(curl -s -m 20 "http://localhost:9090/api/v1/rules" 2>/dev/null)
if [ -n "$RULES" ]; then
  NR=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
print(sum(len(g['rules']) for g in d['data']['groups'] if g['name']=='gateway-auth'))" 2>/dev/null)
  NH=$(echo "$RULES" | python -c "
import sys,json
d=json.load(sys.stdin)
print(sum(1 for g in d['data']['groups'] if g['name']=='gateway-auth'
          for r in g['rules'] if r.get('health')=='ok'))" 2>/dev/null)
  chk "gateway-auth alert rules loaded" "$NR" "6"
  chk "  ... all evaluating (health ok)" "$NH" "6"
else
  echo "  SKIP  prometheus not reachable on :9090 (rules not asserted)"
fi

echo
echo "-- 13. Published contract vs. the code -------------------------------"
# A contract document that has drifted is worse than none: a client generated
# from it fails at runtime against a service that is working correctly.
for f in openapi.yaml RUNBOOK.md; do
  if [ -f "$SVC/$f" ]; then ok "$f present"; else bad "$f missing"; fi
done
# No asyncapi.yaml, and that is correct rather than missing: this service
# publishes no events. It has no Kafka dependency at all — security signals go
# to siem-integration-svc over HTTP, which is that service's contract, not this
# one's. Asserted so the absence stays deliberate.
if grep -q "kafka" "$SVC/go.mod"; then
  bad "go.mod pulls in Kafka — if this service now publishes events it needs an asyncapi.yaml"
else
  ok "no event contract needed (no broker dependency)"
fi

ROUTE_DIFF=$(python - "$SVC" <<'PYEOF'
import re, sys, pathlib
try:
    import yaml
except ImportError:
    print("SKIP pyyaml not installed"); raise SystemExit
svc = pathlib.Path(sys.argv[1])
code = (svc / "internal/router/router.go").read_text(encoding="utf-8")
in_code = set()
# r.Get/r.Post/... and r.Handle, which is how /verify is mounted so it accepts
# every method ForwardAuth may replay.
for m, p in re.findall(r'r\.(Get|Post|Put|Patch|Delete)\("(/[^"]*)"', code):
    in_code.add(p)
for p in re.findall(r'r\.Handle\("(/[^"]*)"', code):
    in_code.add(p)
spec = yaml.safe_load((svc / "openapi.yaml").read_text(encoding="utf-8"))
in_spec = set(spec["paths"])
for r in sorted(in_code - in_spec): print("UNDOCUMENTED " + r)
for r in sorted(in_spec - in_code): print("PHANTOM " + r)
print("COUNT %d" % len(in_code))
PYEOF
)
echo "$ROUTE_DIFF" | grep -q "^SKIP" && echo "  SKIP  pyyaml not installed" || {
  chk "routes served but not documented" "$(echo "$ROUTE_DIFF" | grep -c '^UNDOCUMENTED')" "0"
  chk "routes documented but not served" "$(echo "$ROUTE_DIFF" | grep -c '^PHANTOM')" "0"
  echo "        routes documented: $(echo "$ROUTE_DIFF" | grep '^COUNT' | awk '{print $2}')"
}

# Runbook: each of the six alert rules names a section by number. A dangling
# pointer is discovered by an on-call at 3am.
RULES_FILE="$SVC/../../deployments/prometheus-rules.yml"
MISSING_SECTIONS=0
for n in 4.1 4.2 4.3 4.4 4.5 4.6; do
  grep -q "^### $n " "$SVC/RUNBOOK.md" || MISSING_SECTIONS=$((MISSING_SECTIONS+1))
done
chk "runbook sections the alerts reference" "$MISSING_SECTIONS" "0"
if [ -f "$RULES_FILE" ]; then
  REFS=$(awk '/^  - name: gateway-auth$/{f=1;next} /^  - name: /{f=0} f' "$RULES_FILE" \
    | grep -oE "RUNBOOK section [0-9]+\.[0-9]+" | awk '{print $3}' | sort -u)
  chk "  ... distinct sections referenced by these rules" "$(echo "$REFS" | grep -c .)" "6"
  DANGLING=0
  for ref in $REFS; do
    grep -q "^### $ref " "$SVC/RUNBOOK.md" || DANGLING=$((DANGLING+1))
  done
  chk "  ... references resolving to no runbook heading" "$DANGLING" "0"
fi

echo
echo "-- 14. Traefik forwards what this service produces -------------------"
# Every header set on the 200 path must appear in the ForwardAuth middleware's
# authResponseHeaders list, or Traefik drops it SILENTLY and the backend simply
# never sees it. Nothing else in the estate checks this, and the failure is
# invisible from both ends.
COMPOSE="$SVC/../../deployments/docker-compose.yml"
if [ -f "$COMPOSE" ]; then
  ARH=$(grep -o 'authResponseHeaders=[^"]*' "$COMPOSE" | head -1 | cut -d= -f2)
  if [ -n "$ARH" ]; then
    DROPPED=0
    for h in X-Principal-Id X-Tenant-Id X-Legal-Entity-Id X-Correlation-Id \
             X-Jurisdiction-Context X-Timezone X-Residency-Policy-Id X-Tenant-Context-Stale; do
      case ",$ARH," in
        *",$h,"*) ;;
        *) DROPPED=$((DROPPED+1)); echo "        not forwarded: $h" ;;
      esac
    done
    chk "headers set on success that Traefik would drop" "$DROPPED" "0"
  else
    echo "  SKIP  no authResponseHeaders found in compose"
  fi
else
  echo "  SKIP  compose file not found"
fi

echo
echo "-- 15. Console (the surface that reads this service) -----------------"
# gateway-auth-svc has no CRUD console of its own and correctly should not: it
# is a router callout with one machine endpoint. Its FRONTEND contract is the
# refusal signal — X-Tenant-Context, which the console reads to tell a GOV-01
# refusal apart from a backend service's own 403/503. That handling existed
# with no test coverage at all, so a change to this service's header names
# would have silently made the console misreport.
FE="${FE:-$SVC/../../../zoiko-suite-frontend-platform}"
if [ -n "$SKIP_FE" ]; then
  echo "  SKIP  SKIP_FE set"
elif [ ! -d "$FE/node_modules" ]; then
  echo "  SKIP  console not installed at $FE (run npm ci there)"
else
  FEOUT=$(cd "$FE" && npx playwright test e2e/gateway-auth.spec.ts --reporter=list 2>&1)
  FEPASS=$(echo "$FEOUT" | grep -cE "^\s+ok [0-9]+")
  FEFAIL=$(echo "$FEOUT" | grep -cE "^\s+x  [0-9]+")
  chk "console e2e failures" "$FEFAIL" "0"
  if [ "$FEPASS" -ge 4 ]; then ok "console e2e passing ($FEPASS)"; else bad "console e2e passing ($FEPASS, want >= 4)"; fi
fi

echo
echo "==================================================================="
printf ' RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
echo "==================================================================="
[ "$FAIL" -eq 0 ]
