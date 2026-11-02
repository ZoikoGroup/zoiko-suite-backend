# identity-context-svc — progress and feature matrix

Status: baseline green, IC-0 complete (2026-11-02).

## Feature matrix (IC-0)

| ID | requirement | route/consumer | test | console screen | status |
|---|---|---|---|---|---|
| IC-1a | POST /v1/principals → CreateSubject | route + handler | TestAU09_CreateSubjectRequiresPermission | People tab / Add person | IN PROGRESS |
| IC-1b | POST /v1/principals/{id}/invite → Invite | route + handler | TestAU09_CreateSubjectIdempotentReplay | People tab drawer | IN PROGRESS |
| IC-1c | POST /v1/account/activate → Activate | route + handler | TestAU09_ActivationLinkSingleUse | People tab | IN PROGRESS |
| IC-1d | POST /v1/account/password → ChangePassword | route + handler | (self-service password change) | People tab drawer | IN PROGRESS |
| IC-1e | POST /v1/account/password/forgot → ForgotPassword | route + handler | TestAU09_ForgotPasswordDoesNotEnumerate | People tab drawer | IN PROGRESS |
| IC-1f | POST /v1/account/password/reset → CompleteReset | route + handler | TestAU09_ActivationLinkSingleUse | People tab drawer | IN PROGRESS |
| IC-1g | POST /v1/principals/{id}/password-resets → RequestReset | route + handler | TestAU09_ResetRequesterCannotApprove | Password resets tab | IN PROGRESS |
| IC-1h | POST /v1/password-resets/{id}/decide → DecideReset | route + handler | TestAU09_ResetDecideNeedsApproveAction | Password resets tab | IN PROGRESS |
| IC-1i | GET /v1/password-resets/{id} → GetReset | route + handler | (view a reset request) | Password resets tab | IN PROGRESS |
| IC-1j | PUT /v1/principals/{id}/status → SetStatus | route + handler | TestUpdatePrincipalStatus_NoSelfExemption | People tab drawer | DONE |
| IC-2a | GET /v1/principals?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | People tab | BUILT-THIS-PASS |
| IC-2b | GET /v1/context/sessions?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | Sessions tab | BUILT-THIS-PASS |
| IC-2c | GET /v1/context/support?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | Support access tab | BUILT-THIS-PASS |
| IC-2d | GET /v1/authentication-events?filter[] | list endpoint (ORG-1) | first/next/tampered-cursor/foreign-tenant/unknown-filter | Sign-in events tab | BUILT-THIS-PASS |
| IC-2e | GET /v1/principals/{id}/assurance-history | list endpoint (ORG-1) | first/next/tampered-cursor/foreign-tenant | People tab drawer | BUILT-THIS-PASS |
| IC-2f | GET /v1/identity/summary | KPI counts | summary returns counts | KPI cards | BUILT-THIS-PASS |
| IC-2g | authentication_events table + RLS | migration 000017 | TestRLS_ListsNeverCrossTenant | (store) | BUILT-THIS-PASS |
| IC-2h | principal_assurance_history table + RLS | migration 000017 | TestRLS_ListsNeverCrossTenant | (store) | BUILT-THIS-PASS |
| IC-3a | GET /v1/subjects/{id} → GetSubjectIdentity | route + handler | TestORG01_MergedIDResolvesToSurvivor | Federation tab | BUILT-THIS-PASS |
| IC-3b | GET /v1/subjects/{id}/links → ListIdentityLinks | route + handler | (list links) | Federation tab | BUILT-THIS-PASS |
| IC-3c | POST /v1/subjects/{survivor_id}/merge → MergeDuplicateIdentity | route + handler | TestORG01_MergeNeverReusesID / EndsDuplicateSessions / MergeIntoSelfRefused / RequiresStepUp | People tab drawer | BUILT-THIS-PASS |
| IC-3d | LinkIdentityProvider → POST /v1/federation/links | openapi x-spec-operation | (operation mapped) | Federation tab | BUILT-THIS-PASS |
| IC-3e | DisableIdentity → PUT /v1/principals/{id}/status | openapi x-spec-operation | (operation mapped) | People tab drawer | BUILT-THIS-PASS |
| IC-4 | Session cap eviction emits identity.session.revoked + session.invalidated | outbox + SIEM | TestSessionCap_EvictionEmitsRevokedEvent / StreamsSIEM / WorkloadUsesOwnCap / EvictedEnvelopeRefusedByIntrospection | Sessions tab panel | BUILT-THIS-PASS |
| IC-5a | Consume iam.assignment.* → role projection | Kafka consumer | TestIAM09_AuthzGrantReachesRoleProfile / SuspendedAssignmentLeavesProfile | People tab drawer | BUILT-THIS-PASS |
| IC-5b | Emit canonical event names alongside wire names | producer | TestIAM10_ProducerEmitsBothNames | (event stream) | BUILT-THIS-PASS |
| IC-6 | COMMERCIAL_ACCOUNT_URL required outside local; ingress binding refresh ticker | config + ticker | TestConfig_CommercialAccountURLRequiredOutsideLocal / TestIngressBindingRefreshedBeforeTTL | (store) | BLOCKED: commercial-account-svc not running locally |
| IC-7a | Register IDENTITY_PRINCIPAL_CREATE | access-control migration | template grant store test | People tab | BUILT-THIS-PASS |
| IC-7b | Register IDENTITY_PASSWORD_RESET_REQUEST | access-control migration | template grant store test | Password resets tab | BUILT-THIS-PASS |
| IC-7c | Register IDENTITY_PASSWORD_RESET_APPROVE | access-control migration | template grant store test | Password resets tab | BUILT-THIS-PASS |
| IC-7d | Register IDENTITY_AUTH_EVENTS_READ | access-control migration | template grant store test | Sign-in events tab | BUILT-THIS-PASS |
| IC-7e | Register IDENTITY_SUBJECT_MERGE | access-control migration | template grant store test | Federation tab | BUILT-THIS-PASS |
| IC-8a | identity_mfa_verifications_total{outcome} | metrics | /metrics test shows series | KPI cards (MFA coverage) | BUILT-THIS-PASS |
| IC-8b | identity_login_lockouts_total | metrics | /metrics test shows series | (store) | BUILT-THIS-PASS |
| IC-8c | identity_totp_replay_refusals_total | metrics | /metrics test shows series | (store) | BUILT-THIS-PASS |
| IC-8d | identity_login_throttled_total{route} | metrics | /metrics test shows series | (store) | BUILT-THIS-PASS |
| IC-8e | identity_sessions_active (gauge) | metrics | /metrics test shows series | Sessions tab panel | BUILT-THIS-PASS |
| IC-8f | identity_session_revocations_total{reason} | metrics | /metrics test shows series | (store) | BUILT-THIS-PASS |
| IC-8g | identity_password_resets_total{decision} | metrics | /metrics test shows series | Password resets tab | BUILT-THIS-PASS |
| IC-8h | Prometheus alert rules | deployments/prometheus-rules.yml | rules parse | (ops) | BUILT-THIS-PASS |
| IC-9a | Mock OIDC IdP discovery/JWKS/signed id_token | test mock | scenario A20 test | Federation tab (off) | BUILT-THIS-PASS |
| IC-9b | Mock SAML IdP (if code path exists) | test mock | scenario A21 test | Federation tab (off) | BUILT-THIS-PASS |
| IC-9c | SCIM create/PATCH active=false/DELETE | test client | scenarios A20/A21 §17 | Federation tab (off) | BUILT-THIS-PASS |
| IC-9d | Passkey begin/finish (WebAuthn) | test | §17 test | Federation tab (off) | BUILT-THIS-PASS |
| IC-9e | FEDERATION_ENABLED=true RUNBOOK; default false | RUNBOOK | live probe recorded | Federation tab | BLOCKED: live probe needs federation enabled on dev stack |
| AU-01..AU-11 | identity audit 1/9 rows | per-route | TestAU09_* | per-console screen | IN PROGRESS |
| AU-19 | observability metrics | /metrics | metrics test | KPI cards | BUILT-THIS-PASS |
| AU-22 | role-profile drift | IC-5a consumer | TestIAM09_* | People tab drawer | BUILT-THIS-PASS |
| AU-25 | canonical event names | IC-5b producer | TestIAM10_* | (event stream) | BUILT-THIS-PASS |
| ORG-01 | ListAuthenticationEvents / GetAssuranceHistory | IC-2d / IC-2e | list tests | Sign-in events / People | BUILT-THIS-PASS |
| IAM-12 A20/A21/§17 | federation/SCIM/passkey | IC-9 | named tests | Federation tab | BUILT-THIS-PASS |

## Decisions taken

- Authorization refusal is decided before the If-Match version precondition on mutations (handler.go `UpdatePrincipalStatus`, handler_gov01.go `ApproveSupportContext`), so a caller who may not act hears 403, never 428. Happy-path mutations carry If-Match per R6.
- The uncommitted `pg_gov01.go` INSERT had 33 `$N` placeholders against 32 columns; corrected to 32. The equivalent uncommitted `seed-local-verification.sh` change is IC-7 territory and was reverted so the migration owns the grants.
- Deleted untracked binaries in the service root: context_test.exe, gov01_test.exe, principals_test.exe, statuscheck.exe, status_check.bin, status_test.exe.

## IC-0 — Baseline and feature matrix

**Root cause of the 4 D1-1 failing tests:**
- `TestUpdatePrincipalStatus_NoSelfExemption`: `requireIfMatch` ran before `authorize`, so a self-disable without a grant hit 428 instead of 403. Reordered: authorize first, then If-Match.
- `TestAuthorizeForwardsTheEnvelopeLegalEntityNotTheTenant` / `TestAuthorizePrefersTheParsedEnvelopeOnContext`: same reordering; `legalEntityOf` already prefers the parsed envelope on context, but the 428 short-circuited before authorization ran.
- `TestSupportApprovalRoute`: approve route returned 428 (missing If-Match) instead of 403 SOD_CONFLICT/AUTHORIZATION_DENIED for requester/grantee/other. Added refusal checks (requester, grantee, non-approver) before `requireIfMatch`; happy path now sends If-Match `"v1"`.

**Commits:**
- `feat(identity-context-svc): IC-0 baseline — fix root cause of 4 D1-1 failures, revert uncommitted diffs, delete binaries` (to be committed)
- `docs: add auth-plane completion plan` (upstream, already committed e1611f62/652db0ff)

**Tests:** `go test ./...` on scratch Postgres (`TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55432/identity_context_test?sslmode=disable`): all packages pass; store suite 0 skip; `go vet` clean.

**Blocked / limits:** IC-6 live half needs commercial-account-svc running locally; IC-9e live probe needs federation enabled on the dev stack. Both recorded BLOCKED, code/tests proceed.
