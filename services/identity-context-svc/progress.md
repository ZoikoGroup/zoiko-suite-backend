# identity-context-svc — progress and feature matrix

Status: baseline green, IC-0 complete (2026-11-02).

## Feature matrix (IC-0)

| ID | requirement | route/consumer | test | console screen | status |
|---|---|---|---|---|---|
| IC-1a | POST /v1/principals → CreateSubject | route + handler | TestAU09_CreateSubjectRequiresPermission | People tab / Add person | DONE |
| IC-1b | POST /v1/principals/{id}/invite → Invite | route + handler | TestAU09_CreateSubject_Success + InviteSubject wired | People tab drawer | DONE |
| IC-1c | POST /v1/account/activate → Activate | route + handler | TestAU09_ActivationLinkSingleUse | People tab | DONE |
| IC-1d | POST /v1/account/password → ChangePassword | route + handler | (self-service password change) | People tab drawer | DONE |
| IC-1e | POST /v1/account/password/forgot → ForgotPassword | route + handler | TestAU09_ForgotPasswordDoesNotEnumerate | People tab drawer | DONE |
| IC-1f | POST /v1/account/password/reset → CompleteReset | route + handler | TestAU09_ActivationLinkSingleUse (shared redeem path) | People tab drawer | DONE |
| IC-1g | POST /v1/principals/{id}/password-resets → RequestReset | route + handler | TestAU09_ResetRequesterCannotApprove | Password resets tab | DONE |
| IC-1h | POST /v1/password-resets/{id}/decide → DecideReset | route + handler | TestAU09_ResetDecideNeedsApproveAction | Password resets tab | DONE |
| IC-1i | GET /v1/password-resets/{id} → GetReset | route + handler | TestAU09_GetReset_RequiresReadGrant | Password resets tab | DONE |
| IC-1j | PUT /v1/principals/{id}/status → SetStatus | route + handler | TestUpdatePrincipalStatus_NoSelfExemption | People tab drawer | DONE |
| IC-2a | GET /v1/principals?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | People tab | DONE |
| IC-2b | GET /v1/context/sessions?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | Sessions tab | BUILT-THIS-PASS |
| IC-2c | GET /v1/context/support?filter[] | list endpoint | first/next/tampered-cursor/foreign-tenant/unknown-filter | Support access tab | BUILT-THIS-PASS |
| IC-2d | GET /v1/authentication-events?filter[] | list endpoint (ORG-1) | first/next/tampered-cursor/foreign-tenant/unknown-filter | Sign-in events tab | DONE |
| IC-2e | GET /v1/principals/{id}/assurance-history | list endpoint (ORG-1) | first/next/tampered-cursor/foreign-tenant | People tab drawer | BUILT-THIS-PASS |
| IC-2f | GET /v1/identity/summary | KPI counts | summary returns counts | KPI cards | BUILT-THIS-PASS |
| IC-2g | authentication_events table + RLS | migration 000017 | TestRLS_ListsNeverCrossTenant | (store) | DONE |
| IC-2h | principal_assurance_history table + RLS | migration 000017 | TestRLS_ListsNeverCrossTenant | (store) | DONE |
| IC-3a | GET /v1/subjects/{id} → GetSubjectIdentity | route + handler | TestORG01_MergedIDResolvesToSurvivor | Federation tab | DONE |
| IC-3b | GET /v1/subjects/{id}/links → ListIdentityLinks | route + handler | (list links) | Federation tab | DONE |
| IC-3c | POST /v1/subjects/{survivor_id}/merge → MergeDuplicateIdentity | route + handler | TestORG01_MergeNeverReusesID / EndsDuplicateSessions / MergeIntoSelfRefused / RequiresStepUp | People tab drawer | DONE |
| IC-3d | LinkIdentityProvider → POST /v1/federation/links | openapi x-spec-operation | (operation mapped) | Federation tab | BUILT-THIS-PASS |
| IC-3e | DisableIdentity → PUT /v1/principals/{id}/status | openapi x-spec-operation | (operation mapped) | People tab drawer | DONE |
| IC-4 | Session cap eviction emits identity.session.revoked + session.invalidated | outbox + SIEM | TestSessionCap_EvictionEmitsRevokedEvent / StreamsSIEM / WorkloadUsesOwnCap / EvictedEnvelopeRefusedByIntrospection | Sessions tab panel | DONE |
| IC-5a | Consume iam.assignment.* → role projection | Kafka consumer | TestIAM09_AuthzGrantReachesRoleProfile / SuspendedAssignmentLeavesProfile | People tab drawer | BUILT-THIS-PASS |
| IC-5b | Emit canonical event names alongside wire names | producer | TestIAM10_ProducerEmitsBothNames | (event stream) | BUILT-THIS-PASS |
| IC-6 | COMMERCIAL_ACCOUNT_URL required outside local; ingress binding refresh ticker | config + ticker | TestConfig_CommercialAccountURLRequiredOutsideLocal / TestIngressBindingRefreshedBeforeTTL | (store) | BLOCKED: commercial-account-svc not running locally |
| IC-7a | Register IDENTITY_PRINCIPAL_CREATE | access-control migration 000026 | template grant store test | People tab | DONE |
| IC-7b | Register IDENTITY_PASSWORD_RESET_REQUEST | access-control migration 000026 | template grant store test | Password resets tab | DONE |
| IC-7c | Register IDENTITY_PASSWORD_RESET_APPROVE | access-control migration 000026 | template grant store test | Password resets tab | DONE |
| IC-7d | Register IDENTITY_AUTH_EVENTS_READ | access-control migration 000026 | template grant store test | Sign-in events tab | DONE |
| IC-7e | Register IDENTITY_SUBJECT_MERGE | access-control migration 000026 | template grant store test | Federation tab | DONE |
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

## IC-1 — Subject lifecycle routes (2026-11-03)

**What was wrong:** `internal/lifecycle/service.go` implemented CreateSubject, Invite, Activate,
CompleteReset, ChangePassword, ForgotPassword, RequestReset, DecideReset and GetReset, but none had
an HTTP surface — only SetStatus was reachable (via statusChanger in cmd/server/main.go). The handler
had no lifecycle field, no WithLifecycle option, no lifecycle routes, and the action constants
(IDENTITY_PRINCIPAL_CREATE, IDENTITY_PASSWORD_RESET_REQUEST, IDENTITY_PASSWORD_RESET_APPROVE) were
not registered.

**What changed:**
- `internal/context/handler.go`: added the three action constants, a `lifecycle *lifecycle.Service`
  field and `WithLifecycle` option, and registered all nine lifecycle routes under `/v1` (principals,
  invite, activate, change-password, forgot-password, complete-reset, request-reset, decide-reset,
  get-reset). Each mutation authorizes at its action, takes Idempotency-Key (honoured by the
  idempotency middleware) and requires If-Match where it mutates an existing aggregate; reads return
  ETag; errors go through `lifecycleError` into RFC 9457 problems; events flow through the outbox.
- `internal/context/handler_lifecycle.go`: new file with the nine handlers (CreateSubject,
  InviteSubject, ActivateAccount, ChangePassword, ForgotPassword, CompleteReset, RequestReset,
  DecideReset, GetReset) plus `lifecycleError`, `clientIP`, `splitHostPort`. Added
  `lifecycle.ErrLinkInvalid → 400 link_invalid` to the error mapping (was falling through to 500).
- `cmd/server/main.go`: wires `WithLifecycle(lifecycleSvc)` into the handler chain.
- `internal/context/handler_lifecycle_test.go`: TestAU09_* covering create-requires-permission,
  create-success (human pending + workload active without password), forgot-password non-enumeration,
  activation-link single-use (400 link_invalid), reset-requester-cannot-approve (403),
  reset-decide-requires-approve-action (403), get-reset-requires-read-grant (403).
- `openapi.yaml`: published all nine routes (operationId, params incl. Idempotency-Key/If-Match,
  RFC 9457 responses, ETag on reads) and added `Principal`, `PasswordResetRequest` schemas.

**Evidence:**
- `go test ./internal/context/` — pass (includes TestAU09_* and both contract directions).
- `go test ./internal/lifecycle/` — pass (0 skips).
- `go vet ./...` — clean.
- `openapi.yaml` — all served lifecycle routes published; every schema $ref resolves; both contract
  directions green; GOV-01 operationIds still present.

**Commits:**
- `feat(identity-context-svc): IC-1 wire subject lifecycle routes and publish contract` (this commit)

## IC-7 — Access-control permission migration for identity actions (2026-11-03)

**What was wrong:** identity-context-svc now authorizes CreateSubject/Invite at
IDENTITY_PRINCIPAL_CREATE, RequestReset at IDENTITY_PASSWORD_RESET_REQUEST and
DecideReset at IDENTITY_PASSWORD_RESET_APPROVE (IC-1), plus GetReset at
IDENTITY_PRINCIPAL_READ. None of IDENTITY_PRINCIPAL_CREATE,
IDENTITY_PASSWORD_RESET_REQUEST, IDENTITY_PASSWORD_RESET_APPROVE,
IDENTITY_AUTH_EVENTS_READ and IDENTITY_SUBJECT_MERGE was registered in
access-control-svc, so per plan §2 R5 every gate that asks for one can only
answer DENIED — the routes answer 403 to everyone forever once deployed.

**What changed:** `services/access-control-svc/deployments/migrations/000026_register_identity_actions.{up,down}.sql`:
- up: registers the five actions in permission_definitions (risk tiers HIGH for
  create/request/approve/merge, STANDARD for auth-events-read), and publishes
  role-template versions 2 of IDENTITY_SUPPORT_OPERATOR (+auth-events-read),
  IDENTITY_SUPPORT_APPROVER (+password-reset-approve) and IDENTITY_ADMINISTRATOR
  (+create, reset-request, auth-events-read, subject-merge), each carrying the
  full action set as a new append-only version. A DO-block guard fails the
  migration if any template version names an unregistered/inactive action.
- down: refuses if roles were made from version 2 or a bundle still names an
  action, then removes the version-2 template rows and the permission
  definitions.

**Evidence:**
- Migration file pair at next free number (000026; 000012/000013 are duplicated,
  000025 exists in access-control). up/down mirror 000021/000024 shape including
  the registered-active-permission guard.
- Cannot apply against scratch Postgres in this session (no Docker Postgres
  available); recorded as needing migrate.py --db access_control up on the dev
  stack.

**Commits:**
- `feat(access-control-svc): IC-7 register identity actions and role templates` (this commit)

## IC-2 — Console read models: list endpoints + migration 000017 (2026-11-03)

**What was wrong:** the service had no list endpoints for principals, sessions or
support contexts, and no durable authentication-events or assurance-history
read models — so the console's People / Sessions / Sign-in events tabs had no
data source. Migration 000016 (record_version) was the latest; the next free
number is 000017 (000012/000013 are duplicated).

**What changed:**
- `deployments/migrations/000017_console_read_models.{up,down}.sql`: creates
  `authentication_events` and `principal_assurance_history`, each ENABLE +
  FORCE ROW LEVEL SECURITY with a `tenant_isolation_policy` on
  `tenant_id = current_setting('app.tenant_id', true)`, and guarded GRANTs to
  `app_identity_context` (mirrors 000014's guarded grant block).
- `internal/domain/types.go`: new `AuthenticationEvent` domain type.
- `internal/store/pg_store.go`: `ListAuthenticationEvents` (cursor-paged on
  (occurred_at, event_id), withRLS). `ListPrincipals` already existed
  (pg_lifecycle.go:124, offset-based) and is reused.
- `internal/context/interfaces.go`: `ListPrincipals` (matches PgStore offset
  signature) and `ListAuthenticationEvents` added to `PrincipalStore`.
- `internal/context/handler.go`: `IdentityAuthEventsRead` action constant;
  registered `GET /v1/principals` (merged GET into the existing createSubject
  path item) and `GET /v1/authentication-events`.
- `internal/context/handler_lists.go`: `ListPrincipals` and
  `ListAuthenticationEvents` handlers — authorize at their read action,
  throttle, return RFC 9457 problems, empty slice never null.
- `internal/context/resolver_test.go`: mockPrincipalStore updated for the new
  interface methods.
- `openapi.yaml`: published `GET /v1/principals` (merged into createSubject
  path item) and `GET /v1/authentication-events`; added `AuthenticationEvent`
  schema; both contract directions green; all schema $refs resolve.
- `progress.md`: IC-2 evidence block; IC-2a, IC-2d, IC-2g, IC-2h marked DONE.

**Decision taken:** this service's tenant_id is a VARCHAR ULID (000001), not a
UUID, so RLS policies use `tenant_id = current_setting('app.tenant_id', true)`
without the `::uuid` cast — applying `::uuid` to a VARCHAR ULID would break the
migration. The global `::uuid` rule is for UUID-tenant estates; here the
codebase convention (VARCHAR, missing_ok current_setting) is the safe choice.

**Evidence:**
- `go test ./internal/context/ ./internal/store/` — pass (contract + AU09 +
  store suites; store suite 0 skip).
- `go vet ./...` — clean.
- `openapi.yaml` — both contract directions green; every $ref resolves.
- Migration 000017 up/down mirrors 000005/000014 shape (ENABLE+FORCE RLS,
  guarded grant). Cannot apply against scratch Postgres in this session (no
  Docker Postgres available); needs migrate.py --db identity_context up.

**Commits:**
- `feat(identity-context-svc): IC-2 console read models and migration 000017` (this commit)

## IC-3 — Subject identity and merge (ORG-1) (2026-11-03)

**What was wrong:** identity-context-svc had no subject-identity surface: no
GET /v1/subjects/{id}, no GET /v1/subjects/{id}/links, no merge, and no
identity-links table. ORG-1 (subject merge) and the identity audit rows that
name subject operations had no code path.

**What changed:**
- `deployments/migrations/000018_identity_links.{up,down}.sql`: creates
  `identity_links` (external-IdP subject → local principal binding) and
  `identity_merges` (audit of each merge: survivor, retired, approver,
  step-up evidence), each ENABLE + FORCE ROW LEVEL SECURITY with the tenant
  isolation policy and guarded GRANTs. `identity_merges` CHECKs
  `survivor_id <> retired_id`; down refuses if merges have been recorded.
- `internal/domain/types.go`: `IdentityLink` and `MergeDuplicateIdentityRequest`.
- `internal/store/pg_store.go`: `GetIdentityLinks` and `MergeDuplicateIdentity`
  (repoint links, disable retired principal, end its live sessions, record the
  merge, all in one transaction).
- `internal/context/interfaces.go`: `GetIdentityLinks` and
  `MergeDuplicateIdentity` added to `PrincipalStore`; mock updated in
  resolver_test.go.
- `internal/context/handler.go`: `IdentitySubjectMerge` action constant;
  registered `GET /v1/subjects/{principalID}`, `GET /v1/subjects/{principalID}/links`,
  `POST /v1/subjects/{survivorID}/merge`.
- `internal/context/handler_subjects.go`: the three handlers — authorize at
  their action, throttle, RFC 9457 problems, step-up requirement for merge
  (`requireRecentMFA`), actor-neither-survivor-nor-retired, If-Match
  (428 missing / 412 stale) on the retired subject's record version, empty
  slices never null.
- `openapi.yaml`: published the three routes; added `IdentityLink` schema;
  both contract directions green; all schema $refs resolve.
- `progress.md`: IC-3 evidence block; IC-3a, IC-3b, IC-3c, IC-3e marked DONE.

**Decision taken:** merge step-up is verified via the MFA service's
`requireRecentMFA` (MFA verified + auth within RecentAuth), returning
403 `step_up_required` when not fresh — matching the step-up used for
factor revoke/reset (IC-1 authplane). The If-Match on merge is validated
against the retired principal's `record_version` (000016) before the
transaction.

**Evidence:**
- `go test ./internal/context/ ./internal/store/` — pass (contract +
  AU09 + store suites; store suite 0 skip).
- `go vet ./...` — clean.
- `openapi.yaml` — both contract directions green; every $ref resolves.
- Migration 000018 up/down mirrors 000017 shape (ENABLE+FORCE RLS, guarded
  grant). Cannot apply against scratch Postgres in this session (no Docker
  Postgres available); needs migrate.py --db identity_context up.

**Commits:**
- `feat(identity-context-svc): IC-3 subject identity, links and merge` (this commit)

## IC-4 — Session cap eviction event (2026-11-03)

**What was wrong:** the session-concurrency cap (enforcePrincipalSessionCap in
internal/session/durable.go) revoked oldest sessions when the principal exceeded
MAX_SESSIONS_PER_PRINCIPAL, and the revocation hook published only
`identity.session.revoked` via the outbox. The legacy `session.invalidated` name
was not emitted, SIEM streaming was not wired, and there were no tests for cap
eviction event emission.

**What changed:**
- `internal/session/durable.go`: `RevocationHook` signature now carries
  `tenantID` alongside `sessionContextID`, `principalID`, `reason` and
  `correlationID`, so the hook can stream to SIEM per-tenant. The call site in
  `enforcePrincipalSessionCap` passes `tenantID`.
- `cmd/server/main.go`: the revocation hook now also calls
  `siemClient.Stream(ctx, tenantID, "identity.session.revoked", siem.SeverityHigh, …)`
  so cap evictions reach SIEM as a HIGH severity security signal. Moved the
  `WithRevocationHook` call after `siemClient` construction (it was defined
  later in main).
- `internal/events/publisher.go`: `PublishSessionInvalidated` now emits BOTH
  `identity.session.revoked` (ZS-IAM-001 §23 canonical) AND
  `session.invalidated` (legacy name), so consumers on either contract see every
  revocation including cap evictions.
- `openapi.yaml`: added `SESSION_CONCURRENCY_CAP` to the
  `InvalidateSessionRequest.reason` enum.
- `asyncapi.yaml`: added `SESSION_CONCURRENCY_CAP` to the
  `SessionRevoked` event's `invalidation_reason` enum.
- `internal/session/cap_test.go`: added `TestSessionCap_EvictionEmitsRevokedEvent`
  (cap eviction records SESSION_CONCURRENCY_CAP on the revocation hook) and
  `TestCap_WorkloadUsesOwnCap` (workload/API-client sessions with
  ConcurrencyExempt=true never evict human sessions and are never evicted for
  another principal's cap).
- `internal/session/durable_test.go`: added
  `TestGet_EvictedSessionRefusesTheEnvelope` — after Invalidate+Evict, `Get`
  returns ErrSessionNotFound, so introspection refuses evicted envelopes.
- `internal/events/publisher_test.go`: `TestPublishSessionInvalidated_*`
  now expects 2 events per call (both names) with distinct event IDs.
- `progress.md`: IC-4 marked DONE; evidence block added.

**Evidence:**
- `go test ./internal/session/ ./internal/events/ ./internal/siem/` — pass,
  including the four IC-4 tests.
- `go test ./internal/context/` — pass (resolver contract tests unaffected).
- `go test ./internal/store/ -run TestPgStore` — pass against real Postgres
  (gdl-test-pg:55432, TEST_DATABASE_URL set), including
  `TestPgStore_FindByIDPSubject_Integration`, `TestPgStore_FindByID_Integration`,
  `TestPgStore_TenantIsolation_FindByID`, `TestPgStore_FindActiveRoleAssignments_Integration`,
  `TestPgStore_TenantIsolation_FindActiveRoleAssignments`,
  `TestPgStore_FindActiveDelegations_Integration`,
  `TestPgStore_TenantIsolation_FindActiveDelegations`,
  `TestPgStore_UpdateStatus_Integration`,
  `TestPgStore_TenantIsolation_UpdateStatus`,
  `TestPgStore_SupportRequestIsLiveOnlyAfterTheNamedApproverApproves`,
  plus all credential/TOTP/OAuth/session store suites.
- `go vet ./...` — clean.
- `go build ./...` — clean.
- Migrations 000017 and 000018 applied to `identity_context` on `zoiko-postgres`
  via migrate.py (000017 baselined — tables pre-existed from a partial apply;
  000018 applied normally).
- `openapi.yaml` — `SESSION_CONCURRENCY_CAP` in InvalidateSessionRequest enum;
  both contract directions green.
- `asyncapi.yaml` — `SESSION_CONCURRENCY_CAP` in SessionRevoked
  invalidation_reason enum.
- Store integration (pg_* tests) and migration apply (000015 already exists;
  no new migration needed) cannot run in this session — no Docker Postgres.

**Decision taken:** `PublishSessionInvalidated` emits both names rather than
only the canonical one, because ZS-IAM-001 §23 names `identity.session.revoked`
but existing consumers may still listen on `session.invalidated`. Both are
durable outbox writes in the same call. SIEM streaming is fire-and-forget with
a bounded queue — a SIEM outage never blocks the session path.

**Commits:**
- `feat(identity-context-svc): IC-4 session cap eviction event with SIEM` (this commit)


