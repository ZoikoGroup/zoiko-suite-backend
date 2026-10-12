#!/usr/bin/env bash
# Live proof of Authorization Standard §9 / §19 for this service: a change to
# what a principal holds ends the sessions carrying the old grant.
#
#   1. a governed assignment (iam.assignment.granted) is projected into
#      identity-context-svc's principal_role_assignments
#   2. retiring the role (role.updated) ends the holder's sessions
#   3. revoking the assignment (iam.assignment.revoked) ends them again and
#      closes the projection
#
# Requires access-control-svc (:8137), authorization-svc (:8089),
# identity-context-svc consuming zoiko.access-control.events, Kafka and
# Postgres. Sessions are inserted directly: minting one through
# /v1/context/resolve needs the whole identity stack, and what is under test
# is the revocation, not the minting.

B=${B:-http://localhost:8137}
PGCONTAINER=${PGCONTAINER:-zoiko-postgres}
TEN=11111111-1111-1111-1111-111111111111
ENTITY=22222222-2222-2222-2222-222222222222
ME=33333333-3333-3333-3333-333333333333
WAIT=${WAIT:-30}

PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS  %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$1"; }
chk() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (got $2, want $3)"; fi; }
uuid() { python -c "import uuid;print(uuid.uuid4())" | tr -d '\015'; }
icq() { docker exec -i "$PGCONTAINER" psql -U postgres -d identity_context -tAc "$1" 2>/dev/null | tr -d '\015'; }
jfield() { python -c "import sys,json;print(json.load(sys.stdin).get('$1',''))" 2>/dev/null | tr -d '\015'; }
wh() { echo "-H X-Tenant-Id:$TEN -H X-Principal-Id:$ME -H X-Legal-Entity-Id:$ENTITY -H Idempotency-Key:$1 -H X-Request-Id:$1 -H X-Source-Channel:system -H X-Correlation-ID:$1 -H Content-Type:application/json"; }
waitfor() { # $1 sql, $2 want
  for _ in $(seq 1 "$WAIT"); do [ "$(icq "$1")" = "$2" ] && return 0; sleep 1; done; return 1
}
sessions() { # $1 principal: insert two live sessions
  for _ in 1 2; do
    icq "INSERT INTO session_contexts (session_context_id, principal_id, tenant_id, correlation_id, trust_posture, mfa_verified,
           risk_signal_source, envelope_jwt_jti, issued_at, expires_at, source_service, schema_version)
         VALUES ('$(uuid)', '$1', '$TEN', 'live-check', 'STANDARD', true, 'NONE', '$(uuid)', now(), now() + interval '1 hour',
           'live-check', '1.0')" >/dev/null
  done
}
live() { icq "SELECT count(*) FROM session_contexts WHERE principal_id='$1' AND invalidated_at IS NULL AND expires_at > now()"; }

echo "== live session revocation (access-control-svc -> identity-context-svc) =="
P=$(uuid)
icq "INSERT INTO principals (principal_id, tenant_id, principal_type, identity_provider_subject, email, display_name, status)
     VALUES ('$P', '$TEN', 'HUMAN', 'live-$P', 'live-$P@example.test', 'Live check', 'ACTIVE')" >/dev/null

ROLE=$(curl -s -X POST "$B/v1/role-templates/AP_PREPARER/instantiate" $(wh "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"role_code\":\"LIVE_APP_$(date +%s)\",\"correlation_id\":\"$(uuid)\"}" \
  | python -c "import sys,json;print(json.load(sys.stdin)['role']['role_definition_id'])" | tr -d '\015')
[ -n "$ROLE" ] && ok "AP_PREPARER role instantiated" || bad "instantiate"

A=$(curl -s -X POST "$B/v1/iam/access-assignments/" $(wh "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"target_principal_id\":\"$P\",\"role_definition_id\":\"$ROLE\",\"justification\":\"live check\",\"correlation_id\":\"$(uuid)\"}")
AID=$(echo "$A" | jfield request_id)
chk "assignment provisioned" "$(echo "$A" | jfield status)" "PROVISIONED"

if waitfor "SELECT count(*) FROM principal_role_assignments WHERE assignment_id='$AID' AND effective_to > now()" "1"; then
  ok "iam.assignment.granted projected into identity-context-svc"
else bad "assignment never projected"; fi

sessions "$P"
chk "the holder has 2 live sessions" "$(live "$P")" "2"
curl -s -o /dev/null -X PATCH "$B/v1/role-definitions/$ROLE" $(wh "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"status\":\"RETIRED\",\"correlation_id\":\"$(uuid)\"}"
if waitfor "SELECT count(*) FROM session_contexts WHERE principal_id='$P' AND invalidated_at IS NULL" "0"; then
  ok "retiring the role (role.updated) ended the holder's sessions"
else bad "role.updated did not end the sessions ($(live "$P") live)"; fi
chk "with reason ADMIN_REVOKE" "$(icq "SELECT DISTINCT invalidation_reason FROM session_contexts WHERE principal_id='$P'")" "ADMIN_REVOKE"

curl -s -o /dev/null -X PATCH "$B/v1/role-definitions/$ROLE" $(wh "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"status\":\"ACTIVE\",\"correlation_id\":\"$(uuid)\"}"
sleep 3
sessions "$P"
chk "fresh sessions after reactivation" "$(live "$P")" "2"
R=$(curl -s -X POST "$B/v1/iam/access-assignments/$AID:revoke" $(wh "$(uuid)") \
  -d "{\"legal_entity_id\":\"$ENTITY\",\"reason\":\"live check\",\"correlation_id\":\"$(uuid)\"}")
chk "assignment revoked" "$(echo "$R" | jfield status)" "REVOKED"
if waitfor "SELECT count(*) FROM session_contexts WHERE principal_id='$P' AND invalidated_at IS NULL" "0"; then
  ok "revoking the assignment (iam.assignment.revoked) ended the subject's sessions"
else bad "iam.assignment.revoked did not end the sessions ($(live "$P") live)"; fi
chk "and closed the projection" "$(icq "SELECT effective_to <= now() FROM principal_role_assignments WHERE assignment_id='$AID'")" "t"

echo
echo " RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
