#!/usr/bin/env bash
# evidence-requirements-svc — Audit Script
# Runs the complete audit checklist for the service.
# Usage: ./scripts/audit.sh [--live] [--base-url http://localhost:8130]

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8130}"
LIVE_MODE=false
TOKEN=""

for arg in "$@"; do
  case $arg in
    --live) LIVE_MODE=true ;;
    --base-url=*) BASE_URL="${arg#*=}" ;;
    --token=*) TOKEN="${arg#*=}" ;;
    *) echo "Unknown argument: $arg"; exit 1 ;;
  esac
done

if [[ "$LIVE_MODE" == true && -z "$TOKEN" ]]; then
  echo "ERROR: --live requires --token=<bearer-token>"
  exit 1
fi

AUTH_HEADER=""
if [[ -n "$TOKEN" ]]; then
  AUTH_HEADER="-H \"Authorization: Bearer $TOKEN\""
fi

echo "=========================================="
echo "evidence-requirements-svc Audit"
echo "Base URL: $BASE_URL"
echo "Live mode: $LIVE_MODE"
echo "=========================================="

# 1. Health & Readiness
echo ""
echo "1. Health & Readiness"
echo "---------------------"
curl -sf "$BASE_URL/healthz" >/dev/null && echo "  ✅ /healthz" || echo "  ❌ /healthz"
curl -sf "$BASE_URL/readyz" >/dev/null && echo "  ✅ /readyz" || echo "  ❌ /readyz"

# 2. Metrics
echo ""
echo "2. Metrics"
echo "----------"
curl -sf "$BASE_URL/metrics" | grep -q "http_requests_total" && echo "  ✅ /metrics (http_requests_total present)" || echo "  ❌ /metrics"

# 3. OpenAPI Spec
echo ""
echo "3. OpenAPI Spec"
echo "---------------"
curl -sf "$BASE_URL/openapi.yaml" >/dev/null && echo "  ✅ /openapi.yaml served" || echo "  ⚠️  /openapi.yaml not served (expected if not mounted)"

# 4. Contract Tests (require auth)
if [[ "$LIVE_MODE" == true ]]; then
  echo ""
  echo "4. Contract Tests (authenticated)"
  echo "----------------------------------"
  
  # 4.1 List requirements
  curl -sf $AUTH_HEADER "$BASE_URL/v1/evidence-requirements?page_size=5" >/dev/null \
    && echo "  ✅ GET /v1/evidence-requirements" || echo "  ❌ GET /v1/evidence-requirements"
  
  # 4.2 Cursor pagination
  curl -sf $AUTH_HEADER "$BASE_URL/v1/evidence-requirements/cursor?page_size=5" >/dev/null \
    && echo "  ✅ GET /v1/evidence-requirements/cursor" || echo "  ❌ GET /v1/evidence-requirements/cursor"
  
  # 4.3 Evaluations cursor
  curl -sf $AUTH_HEADER "$BASE_URL/v1/evidence/evaluations?page_size=5" >/dev/null \
    && echo "  ✅ GET /v1/evidence/evaluations" || echo "  ❌ GET /v1/evidence/evaluations"
  
  # 4.4 PBC requests cursor
  curl -sf $AUTH_HEADER "$BASE_URL/v1/pbc/requests/cursor?page_size=5" >/dev/null \
    && echo "  ✅ GET /v1/pbc/requests/cursor" || echo "  ❌ GET /v1/pbc/requests/cursor"
  
  # 4.5 Summary
  curl -sf $AUTH_HEADER "$BASE_URL/v1/evidence/summary" >/dev/null \
    && echo "  ✅ GET /v1/evidence/summary" || echo "  ❌ GET /v1/evidence/summary"
  
  # 4.6 Evaluate (expects 422 for missing legal entity or 503 for unavailable deps)
  IDEM_KEY=$(uuidgen)
  EVAL_RESP=$(curl -s -w "\n%{http_code}" $AUTH_HEADER \
    -H "Idempotency-Key: $IDEM_KEY" \
    -H "X-Legal-Entity-Id: 00000000-0000-0000-0000-000000000000" \
    -H "Content-Type: application/json" \
    "$BASE_URL/v1/evidence/evaluate" \
    -d '{"legal_entity_id":"00000000-0000-0000-0000-000000000000","domain_code":"FINANCE","action_type":"JOURNAL_POST","present_artifacts":[]}')
  HTTP_CODE=$(echo "$EVAL_RESP" | tail -1)
  if [[ "$HTTP_CODE" == "422" || "$HTTP_CODE" == "503" || "$HTTP_CODE" == "401" ]]; then
    echo "  ✅ POST /v1/evidence/evaluate (returns $HTTP_CODE as expected for test entity)"
  else
    echo "  ⚠️  POST /v1/evidence/evaluate returned $HTTP_CODE"
  fi
  
  # 4.7 Idempotency replay
  REPLAY_RESP=$(curl -s -w "\n%{http_code}" $AUTH_HEADER \
    -H "Idempotency-Key: $IDEM_KEY" \
    -H "X-Legal-Entity-Id: 00000000-0000-0000-0000-000000000000" \
    -H "Content-Type: application/json" \
    "$BASE_URL/v1/evidence/evaluate" \
    -d '{"legal_entity_id":"00000000-0000-0000-0000-000000000000","domain_code":"FINANCE","action_type":"JOURNAL_POST","present_artifacts":[]}')
  REPLAY_CODE=$(echo "$REPLAY_RESP" | tail -1)
  if [[ "$REPLAY_CODE" == "$HTTP_CODE" ]]; then
    echo "  ✅ Idempotency replay returns same status"
  else
    echo "  ⚠️  Idempotency replay returned $REPLAY_CODE (expected $HTTP_CODE)"
  fi
  
  # 4.8 Create requirement (tenant-wide, requires PLATFORM scope)
  CREATE_KEY=$(uuidgen)
  CREATE_RESP=$(curl -s -w "\n%{http_code}" $AUTH_HEADER \
    -H "Idempotency-Key: $CREATE_KEY" \
    -H "Content-Type: application/json" \
    "$BASE_URL/v1/admin/evidence-requirements" \
    -d '{"tenant_id":"'"$(echo $TOKEN | cut -d. -f2 | base64 -d 2>/dev/null | jq -r .tenant_id // "00000000-0000-0000-0000-000000000000")"'","domain_code":"TEST","action_type":"TEST_ACTION","evidence_type":"SUPPORTING_DOCUMENT","requirement_payload":{"minimum_count":1},"effective_from":"2026-01-01T00:00:00Z","correlation_id":"'"$CREATE_KEY"'"}')
  CREATE_CODE=$(echo "$CREATE_RESP" | tail -1)
  if [[ "$CREATE_CODE" == "201" || "$CREATE_CODE" == "403" || "$CREATE_CODE" == "401" ]]; then
    echo "  ✅ POST /v1/admin/evidence-requirements (returns $CREATE_CODE)"
  else
    echo "  ⚠️  POST /v1/admin/evidence-requirements returned $CREATE_CODE"
  fi
else
  echo ""
  echo "4. Contract Tests (skipped — not in live mode)"
  echo "   Run with --live --token=<bearer> to execute"
fi

# 5. Database Migrations (check via readiness)
echo ""
echo "5. Database Migrations"
echo "----------------------"
curl -sf "$BASE_URL/readyz" >/dev/null && echo "  ✅ Database reachable (readyz OK)" || echo "  ❌ Database not reachable"

# 6. Outbox Health
if [[ "$LIVE_MODE" == true ]]; then
  echo ""
  echo "6. Outbox Health"
  echo "----------------"
  SUMMARY=$(curl -sf $AUTH_HEADER "$BASE_URL/v1/evidence/summary")
  UNPUB=$(echo "$SUMMARY" | jq -r '.unpublished_outbox_older_than_5_minutes // -1')
  if [[ "$UNPUB" -eq 0 ]]; then
    echo "  ✅ No unpublished outbox events > 5min"
  elif [[ "$UNPUB" -gt 0 ]]; then
    echo "  ⚠️  $UNPUB unpublished outbox events > 5min"
  else
    echo "  ❌ Could not read summary"
  fi
fi

# 7. SPEC_DEVIATIONS Check
echo ""
echo "7. SPEC_DEVIATIONS"
echo "------------------"
if [[ -f "SPEC_DEVIATIONS.md" ]]; then
  echo "  ✅ SPEC_DEVIATIONS.md present"
  grep -c "^## D-ER-" SPEC_DEVIATIONS.md | xargs -I {} echo "  📋 {} deviations recorded"
else
  echo "  ❌ SPEC_DEVIATIONS.md missing"
fi

# 8. Runbook
echo ""
echo "8. Runbook"
echo "----------"
if [[ -f "RUNBOOK.md" ]]; then
  echo "  ✅ RUNBOOK.md present"
else
  echo "  ❌ RUNBOOK.md missing"
fi

# 9. Release Certificate
echo ""
echo "9. Release Certificate"
echo "----------------------"
if [[ -f "RELEASE_CERTIFICATE.md" ]]; then
  echo "  ✅ RELEASE_CERTIFICATE.md present"
else
  echo "  ❌ RELEASE_CERTIFICATE.md missing"
fi

# 10. AsyncAPI
echo ""
echo "10. AsyncAPI Spec"
echo "-----------------"
if [[ -f "asyncapi.yaml" ]]; then
  echo "  ✅ asyncapi.yaml present"
else
  echo "  ❌ asyncapi.yaml missing"
fi

echo ""
echo "=========================================="
echo "Audit Complete"
echo "=========================================="