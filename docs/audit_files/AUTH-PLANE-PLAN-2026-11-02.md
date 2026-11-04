# Auth plane completion plan: identity-context-svc → gateway-auth-svc → access-control-svc

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:executing-plans to run this plan
> task by task. Steps use checkbox (`- [ ]`) syntax. Run **one service per session**, in the order
> given. When a service is finished, stop and hand back for verification (see "Verifier protocol").

**Goal:** Take identity-context-svc, gateway-auth-svc and access-control-svc to the same finished
state as authorization-svc: every spec feature built and wired, tested on real Postgres, proven
live on the rebuilt container by a dependency E2E script, and operable from a console page in the
v2.0 layout.

**Architecture:** Each service is closed with the same seven-stage recipe used for
authorization-svc on 2 Nov 2026 (backend gaps → contract and docs → live container → dependency
E2E → console page → evidence → hand-back). Backend is Go (chi, pgx, kafka-go), one module per
service, importing `zoiko.io/identity-envelope`. Frontend is Next.js 16 server pages built from
the console kit in `components/admin/console`.

**Tech stack:** Go 1.25+, Postgres 16, Kafka, Redis, Traefik, Docker Compose (project
`deployments`); Next.js 16 / React / TypeScript; Python 3.14 for the E2E scripts.

**Spec sources (read before each service):**
- `docs/audit_files/testing_shashi_progress/domain1-domain2-features-and-requirements.md`
  (D1-1 identity-context, D1-5 gateway-auth, D1-9 access-control): the feature list per service.
- `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md` §2 (ground rules R1–R15), §5 (decisions —
  do not re-ask), §6 tasks IAM-1, IAM-2, IAM-6…IAM-12, ORG-1, API-3, API-4, §7 evidence block.
- `docs/audit_files/Auth-Service-audit-2026-10-14.md` (AU-01…AU-27).
- `docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md` sections 1/9, 5/9, 9/9 and
  everything from "gateway-auth-svc cross-check, 13 October" to the end.
- Spec text from `docs/architecture/original_doc/*.docx`, extracted per IMPLEMENTATION-PLAN §3:
  ZS-SVC-A-001 GOV-01 §4 (identity), ZS-SVC-M-001 INT-01 §4.1 (gateway), ZS-IAM-001 §3, §5, §9,
  §10.1, §13–§24, §27 scenarios A01–A30 (all three), ZS-SEC-001 §8–§9 (gateway), ZS-API-001.

**Reference implementation (copy its shape, do not rebuild the kit):**
- Backend finish: `services/authorization-svc/progress.md` section "2 Nov: IAM-13…" (proof style).
- E2E: `deployments/scripts/e2e-authorization-deps.py` (one check per dependency edge, writes
  undone, exit code = failures).
- Console: `app/admin/authorization/page.tsx`, `components/admin/authorization/console/*`
  (`register.tsx` `RegisterView`, `data.ts`, `overview.tsx`, `forms.tsx`), kit
  `components/admin/console/*`, envelope headers `lib/api/envelope.ts`, result shape
  `app/admin/authorization/state.ts`, step-up `app/admin/account/actions.ts`.

---

## Context

authorization-svc is done (85/85 rows, IAM-13, quantity limits, API-3, rebuilt console, 33/33
dependency E2E). The three remaining auth-plane services score well on their old audits but are
not finished against the full documents:

- **identity-context-svc**: subject lifecycle (create, invite, activate, password change/forgot/
  reset with maker-checker) is built in `internal/lifecycle` but **has no route** (only `SetStatus`
  is reachable) — AU-09 is dead code. No list endpoints exist (principals, sessions, support
  contexts), so no console register can be built. ORG-1 ops, IAM-9/10/12, session-cap eviction
  event, entitlement (IAM-2), ingress refresh (S1-B3), MFA metrics (IAM-6) are open. The console
  page is a 72 KB client-side developer playground; login sends no client credentials.
- **gateway-auth-svc**: `/verify`, revocation (AU-01), CARTA modes (AU-23, S5-D2), INT-01
  admission and the token-gated admin read surface exist. Open: per-service audience (IAM-1),
  edge-set `X-Commercial-Plan` / `X-Org-Unit-Id` (S3-D1), trusted-hop client IP (S5-D3), edge
  login throttling (AU-04), INT-01 minimum acceptance tests, stale docs (openapi and release
  certificate still say STEP_UP_MFA is not enforced), admin surface not enabled in compose, and an
  old-style console page.
- **access-control-svc**: the governance surface (templates, requests, reviews, groups, subject
  links) is built, but the console and `lib/api/access-control.ts` cover only roles and bundles.
  Open: `resource.action` aliases for 668 legacy names (S9-D1), production grants by migration,
  refused-escalations read, cursor paging, API-3 coverage beyond roles/bundles, and an uncommitted
  repair of migration 000025.

Outcome: each service gets a feature matrix in which every spec row maps to a route, a test and a
console screen (or an explicit BLOCKED reason), so the verifier can check completeness row by row.

---

## Global constraints

Everything in IMPLEMENTATION-PLAN §2 applies. The ones that bite most:

- **R1 scope:** only our services (the three here, plus authorization-svc, identity-envelope,
  schema-registry-svc, `deployments/`, the frontend). Teammates' services (commercial-account,
  notification is ours, org-structure is not, …) may be **called**, never edited.
- **R3 git:** branch `shashi-changes` in both repos. One commit per task per service. Stage files
  by explicit path, never `git add -A`/`.`. Never push. Never commit `*.exe`, `*.bin`.
  Messages `feat(<svc>): …` with a body; end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **R4 migrations:** next free number (`ls` first; identity-context has duplicate 000012/000013).
  New file only, with `.down.sql`. Tenant tables: `ENABLE` + `FORCE ROW LEVEL SECURITY`, policy on
  `NULLIF(current_setting('app.tenant_id', true), '')::uuid`. Every `GRANT` guarded or naming a role
  from `create-app-roles.sh`. Apply up/down/up on a scratch DB.
- **R5 permissions:** every new action a handler checks is registered in access-control
  (`permission_definitions`) **and** carried by a role template, risk tier HIGH/CRITICAL when
  destructive or administrative.
- **R6 every new/changed route:** envelope gate kept; authorize at the object acted on;
  `Idempotency-Key` on mutations; `If-Match` on mutations of an existing aggregate (428 missing,
  412 stale) and `ETag` on its GET; RFC 9457 problems; events through the outbox in the same
  transaction; in `openapi.yaml`, validated by kin-openapi, covered by the route-parity test.
- **R7 wiring:** constructed in `cmd/server/main.go` and reached by a route/consumer/ticker. Grep
  the callers.
- **R8:** no `go f(ctx)` with the request context.
- **R10 tests:** real Postgres, store suites run (0 skip). Every spec negative path / minimum
  acceptance line gets a test named after it (`TestINT01_…`, `TestAU09_…`, `TestORG01_…`).
  Never point tests at the dev stack's `:5432` databases except the documented scratch DBs.
- **R14:** no stubs, no invented answers. Missing external dependency → interface + real local
  implementation; only the production adapter is BLOCKED.
- **Console:** v2.0 layout only (breadcrumb + title + gold primary action; 5 KPI cards; 3 panels:
  pipeline / ordered list / bar list with callout; tabbed register with search, filters, pager,
  drawer; "Required controls" footer). Live data only; an unreadable source shows as
  "unavailable", never as zero. URL-driven state (`?tab ?q ?page ?id ?pane`). Maker never sees the
  checker's buttons. Every command: reason dialog + step-up retry. No raw JSON as the only view.
  Sidebar labels unchanged.

**Windows/host facts (from memory, verified 2 Nov):**
- Use `127.0.0.1`, never `localhost`.
- Python: `C:/Users/kaman/AppData/Local/Python/bin/python3.14.exe`.
- Go tests: `mkdir .gotmp && GOTMPDIR="$(pwd -W)/.gotmp" go test ./...` in the service dir, then
  delete `.gotmp`. If App Control blocks an exe, rerun; fallback `go test -c -o ./x_test.exe ./pkg`.
- `gofmt` check: `tr -d '\r' < f | gofmt -l` (working copies are CRLF).
- Docker VM ~2 GB: `docker compose -p deployments build <svc>` one at a time; confirm a fresh
  image timestamp; then `up -d --no-deps --no-build --force-recreate <svc>`.
- Migrations on the dev stack: `deployments/migrate.py status|up --db <name>` — migrate **before**
  recreating the container.
- Seeds: `powershell.exe -File deployments/scripts/seed-demo-rbac.ps1` and `seed-demo-registry.ps1`.
- Frontend render check: `next dev` needs `dangerouslyDisableSandbox`, `ZOIKO_API_TIMEOUT_MS=15000`;
  kill orphaned `.next\dev\build\` node workers after a crash.

## Review Focus

The inputs most likely to break a real user that no single task's happy path covers — each has a
pinned test in the owning task:

1. **A revoked or disabled session still working somewhere.** Disable a principal / invalidate a
   session / merge a duplicate → gateway `/verify` and every console read refuse within the
   revocation TTL (≤ 60 s). Tests in IC-3, IC-4, and the IC-10 / GW-6 E2E scripts.
2. **Self-approval paths.** Password-reset requester approving their own reset; support-context
   requester approving; access request requester approving; review owner deciding their own item.
   Tests in IC-1, AC-4; console hides the button (IC-FE-3, AC-FE-2).
3. **Enumeration and brute force on pre-session routes.** Forgot-password and activate must answer
   identically for unknown and known accounts and be rate-limited per real client IP. Tests IC-1,
   GW-2.
4. **Forged context headers through the edge.** `X-Tenant-Id`, `X-Commercial-Plan`,
   `X-Org-Unit-Id`, `X-Authn-Age-Seconds`, `X-Forwarded-For` from an untrusted peer are replaced or
   dropped. Tests GW-1, GW-2, GW-3, the edge-contract test and the GW-6 E2E.
5. **Paging across tenants and tampered cursors.** A cursor minted for tenant A or another filter
   is refused (400), and RLS (as the app role, not superuser) never returns tenant B rows. Tests
   IC-2, AC-3.

---

## The per-service recipe (stages S0–S6)

Every service runs these stages; the task lists below fill them in.

- **S0 Baseline + feature matrix.** Clean working tree for the service; full suite green with
  counts recorded; create/extend `services/<svc>/progress.md` with a **feature matrix**: one row
  per spec requirement (named operation, control, event, negative path, minimum-acceptance test,
  D1-x feature, AU row), columns `ID | requirement | route/consumer | test | console screen |
  status (DONE/BUILT-THIS-PASS/BLOCKED: reason)`. Every later task updates its rows.
- **S1 Backend gaps** (TDD: failing test → code → green → commit), with migrations and R5
  permission registrations.
- **S2 Contract and docs:** `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`
  (re-certified), `SPEC_DEVIATIONS.md`, `scripts/audit.sh`, Postman collection.
- **S3 Live container:** build image, migrate DB, force-recreate, run `scripts/audit.sh` live.
- **S4 Dependency E2E:** `deployments/scripts/e2e-<svc>-deps.py`, modelled on
  `e2e-authorization-deps.py`; all checks PASS on the live stack.
- **S5 Console:** API client additions + page rebuilt in v2.0 layout; `npx tsc --noEmit` and
  `npx eslint <touched files>` clean; every tab rendered against the live service (headless Chrome
  screenshot) including one empty and one error state.
- **S6 Evidence + hand-back:** IMPLEMENTATION-PLAN §7 evidence block per task in the service's
  `progress.md`; update `IMPLEMENTATION-PROGRESS.md` status rows; mutation check (break two guards
  on a scratch copy, a test must fail); then stop and ask for verification.

---

## Service 1 — identity-context-svc (:8080, DB `identity_context`)

### IC-0 · Baseline and feature matrix (S0)
- [ ] Review the uncommitted diffs in `internal/store/pg_gov01.go` and
  `scripts/seed-local-verification.sh`; commit with a reason or revert. Delete the untracked
  `status_check.bin` and the `*.exe` files in the service root (not tracked; `.gitignore` covers them).
- [ ] Run the full suite on a scratch Postgres (`TEST_DATABASE_URL`); record pass/fail/skip per
  package. The D1-1 list names 4 failing tests (`TestUpdatePrincipalStatus_NoSelfExemption` — sends
  no `If-Match`; `TestAuthorizeForwardsTheEnvelopeLegalEntityNotTheTenant`;
  `TestAuthorizePrefersTheParsedEnvelopeOnContext`; `TestSupportApprovalRoute`). If still failing,
  find the root cause (superpowers:systematic-debugging) and fix code or test — never weaken an
  assertion that pins a control.
- [ ] Write `services/identity-context-svc/progress.md` with the feature matrix from: GOV-01 §4
  (the 44 rows in the Identity audit 1/9), D1-1 features, AU-01…AU-11, AU-19, AU-22, AU-25, ORG-01
  operations, IAM-12 scenarios A20/A21/§17.
- **Done when:** suite green with counts in progress.md; matrix committed.

### IC-1 · Subject lifecycle routes (AU-09) — wire `internal/lifecycle`
- **Files:** new `internal/context/handler_lifecycle.go` (+ `_test.go`); `cmd/server/main.go`
  (pass `lifecycleSvc` to the handler with a `WithLifecycle` option, like `WithStatusChanger`);
  `openapi.yaml`; access-control migration (see IC-7).
- **Routes → service method → action:**
  - `POST /v1/principals` → `CreateSubject` → `IDENTITY_PRINCIPAL_CREATE` (Idempotency-Key)
  - `POST /v1/principals/{id}/invite` → `Invite` → `IDENTITY_PRINCIPAL_CREATE`
  - `POST /v1/account/activate` → `Activate` (pre-session; login throttle applies)
  - `POST /v1/account/password` → `ChangePassword` (caller's own envelope; step-up not required)
  - `POST /v1/account/password/forgot` → `ForgotPassword` (always `202`, same body for unknown email)
  - `POST /v1/account/password/reset` → `CompleteReset` (pre-session; throttled)
  - `POST /v1/principals/{id}/password-resets` → `RequestReset` → `IDENTITY_PASSWORD_RESET_REQUEST`
  - `POST /v1/password-resets/{id}/decide` → `DecideReset` → `IDENTITY_PASSWORD_RESET_APPROVE`
  - `GET /v1/password-resets/{id}` → `GetReset` → `IDENTITY_PRINCIPAL_READ`
- Map `lifecycle.Err*` to problems: `ErrInvalid` 400, `ErrNotFound` 404, `ErrConflict` 409,
  `ErrForbidden` 403, `ErrStepUp` 401 `step_up_required`, `ErrBadCredential` 401,
  `ErrLinkInvalid` 400 `link_invalid`.
- **Tests:** `TestAU09_CreateSubjectRequiresPermission`, `TestAU09_CreateSubjectIdempotentReplay`,
  `TestAU09_ForgotPasswordDoesNotEnumerate` (byte-identical 202 for known/unknown),
  `TestAU09_ActivationLinkSingleUse`, `TestAU09_ResetRequesterCannotApprove`,
  `TestAU09_ResetDecideNeedsApproveAction`, `TestAU09_PreSessionRoutesThrottled` (429 +
  `Retry-After`), `TestAU09_WorkloadCreatedActiveWithoutPassword`.
- **Done when:** every lifecycle method has a caller from `main.go` (grep), tests pass, openapi
  parity passes.

### IC-2 · Console read models (list endpoints, API-4 cursor paging)
- **Files:** `internal/context/handler_lists.go` (+ test), `internal/store/pg_lists.go`
  (+ store test), migration **000017_identity_read_models** (`authentication_events`,
  `principal_assurance_history`, both append-only, RLS+FORCE, tenant-scoped).
- **Routes** (all `GET`, cursor via `identity-envelope/apicontract/cursor.go`; `page_size` default
  50, max 200; unknown `filter[...]` → 400):
  - `/v1/principals?filter[status]&filter[principal_type]&q` → `IDENTITY_PRINCIPAL_READ`
  - `/v1/context/sessions?filter[principal_id]&filter[status]` → `IDENTITY_SESSION_READ`
  - `/v1/context/support?filter[status]` → `IDENTITY_SUPPORT_CONTEXT_REVIEW`
  - `/v1/authentication-events?filter[principal_id]&filter[outcome]&filter[since]` (ORG-1
    `ListAuthenticationEvents`) → `IDENTITY_AUTH_EVENTS_READ` (new)
  - `/v1/principals/{id}/assurance-history` (ORG-1 `GetAssuranceHistory`) → `IDENTITY_PRINCIPAL_READ`
  - `/v1/identity/summary` → counts for the console KPIs (principals by status, active sessions,
    MFA coverage, support contexts pending approval, failed sign-ins last 24 h) →
    `IDENTITY_PRINCIPAL_READ`
- **Writers:** authenticator, MFA service and OIDC login append `authentication_events` in the
  same transaction as their existing decision rows; MFA activate/revoke/reset and status change
  append `principal_assurance_history`.
- **Tests per list:** first page, next page, tampered cursor 400, cursor from another tenant 400,
  unknown filter 400, RLS isolation as the NOBYPASSRLS app role (`TestRLS_ListsNeverCrossTenant`).
- **Done when:** all six routes routed, registered (IC-7), documented, tested.

### IC-3 · ORG-1 identity operations
- **Files:** `internal/context/handler_subjects.go` (+ test), store, migration
  **000018_principal_aliases** (`principal_aliases(alias_id PK, survivor_id, merged_at, merged_by,
  reason)`, append-only).
- **Routes:**
  - `GET /v1/subjects/{id}` (`GetSubjectIdentity`; an alias id resolves to the survivor, with
    `merged_into`)
  - `GET /v1/subjects/{id}/links` (`ListIdentityLinks`, from the federation links store)
  - `POST /v1/subjects/{survivor_id}/merge` body `{duplicate_id, reason}` (`MergeDuplicateIdentity`)
    → `IDENTITY_SUBJECT_MERGE` (CRITICAL), step-up required, `If-Match` on the survivor. Ends every
    session of the duplicate, disables it, writes the alias, emits `identity.subject.merged` via
    outbox. authorization-svc consumes it and re-points the duplicate's assignments to the survivor
    (consumer in `services/authorization-svc/internal/events/lifecycle_consumer.go`).
  - openapi `x-spec-operation` on existing routes: `LinkIdentityProvider` → `POST
    /v1/federation/links`, `DisableIdentity` → `PUT /v1/principals/{id}/status`.
- **Tests:** `TestORG01_MergeNeverReusesID`, `TestORG01_MergeEndsDuplicateSessions`,
  `TestORG01_MergedIDResolvesToSurvivor`, `TestORG01_MergeIntoSelfRefused`,
  `TestORG01_MergeRequiresStepUp`; authorization-svc `TestMergedSubjectAssignmentsRepointed`.

### IC-4 · Session cap eviction event (D1-1 PARTIAL)
- Eviction of the oldest session by the cap writes `identity.session.revoked` **and** legacy
  `session.invalidated` through the outbox (reason `SESSION_CAP_EVICTED`) and streams a SIEM event.
- Service accounts and API clients use their own cap `SESSION_CAP_WORKLOAD` (default 20), not the
  human cap (default 5).
- **Tests:** `TestSessionCap_EvictionEmitsRevokedEvent`, `TestSessionCap_EvictionStreamsSIEM`,
  `TestSessionCap_WorkloadUsesOwnCap`, `TestSessionCap_EvictedEnvelopeRefusedByIntrospection`.

### IC-5 · IAM-9 role-profile drift + IAM-10 canonical event names
- Consumer also subscribes to `zoiko.authorization.events` and applies `iam.assignment.*`
  (granted/approved/revoked/suspended/resumed/expired) to the role projection
  (`internal/store/pg_role_projection.go`), idempotent, commit-after-apply.
- Producer emits IAM §23 names alongside the current wire names (R9): grep the producers first;
  at minimum `identity.session.revoked` next to `session.invalidated`, and `identity.subject.*`
  next to `principal.status.changed`. Add both to `asyncapi.yaml`; register in schema-registry-svc
  if G-08 routes exist, otherwise list under follow-ups.
- **Tests:** `TestIAM09_AuthzGrantReachesRoleProfile`, `TestIAM09_SuspendedAssignmentLeavesProfile`,
  `TestIAM10_ProducerEmitsBothNames`.

### IC-6 · IAM-2 entitlement + S1-B3 ingress-binding refresh
- `COMMERCIAL_ACCOUNT_URL`: set in compose and k8s; config refuses to start without it outside
  `ENV=local` (`TestConfig_CommercialAccountURLRequiredOutsideLocal`). Live: a resolve shows a real
  entitlement status. If commercial-account-svc does not run locally, record that live half BLOCKED.
- Ingress bindings refreshed from tenant-entity-registry-svc before their TTL (ticker at TTL/2 with
  `context.WithoutCancel` + timeout), so a binding never reads `STALE` while the registry is up.
  Test `TestIngressBindingRefreshedBeforeTTL`.

### IC-7 · Permissions and grants by migration (R5, S1-B2)
- access-control migration (next free number, expected **000026_register_identity_actions**):
  register `IDENTITY_PRINCIPAL_CREATE`, `IDENTITY_PASSWORD_RESET_REQUEST`,
  `IDENTITY_PASSWORD_RESET_APPROVE`, `IDENTITY_AUTH_EVENTS_READ`, `IDENTITY_SUBJECT_MERGE` (and any
  other action IC-1…IC-3 check that `GET /v1/permission-definitions` does not list), and add them
  plus the six existing `IDENTITY_*` actions to the identity-administrator role template(s).
- Store test: a principal instantiated from that template is granted each action.
- Remove the equivalent grants from `seed-demo-rbac.ps1` only after the migration covers them.

### IC-8 · IAM-6 observability
- Metrics: `identity_mfa_verifications_total{outcome}`, `identity_login_lockouts_total`,
  `identity_totp_replay_refusals_total`, `identity_login_throttled_total{route}`,
  `identity_sessions_active` (gauge), `identity_session_revocations_total{reason}`,
  `identity_password_resets_total{decision}`.
- Alert rules in `deployments/prometheus-rules.yml`: lockout burst, replay burst, throttle spike,
  step-up failure ratio.
- **Done when:** a `/metrics` test shows each series; the rules file parses (promtool or YAML).

### IC-9 · IAM-12 federation, SCIM and passkey acceptance
- Tests with an `httptest` mock OIDC IdP (discovery, JWKS, signed id_token), a mock SAML IdP if the
  code path exists, and a SCIM client: create → principal exists; PATCH active=false → disabled and
  sessions ended; DELETE → deprovisioned. Passkey begin/finish with the WebAuthn library already in
  `go.mod`. Scenarios A20, A21, §17 named in test names.
- RUNBOOK: how to set `FEDERATION_ENABLED=true`; default stays false.
- Live probe with federation enabled on the dev stack, recorded in progress.md.

### IC-10 · Contract, docs, live, E2E (S2–S4)
- [ ] openapi (all new routes, kin-openapi + parity), asyncapi (new events), RUNBOOK,
  RELEASE_CERTIFICATE (re-certify), SPEC_DEVIATIONS, `scripts/audit.sh` run live.
- [ ] Re-score AU-01…AU-11 in `Auth-Service-audit-2026-10-14.md` (new dated section).
- [ ] Build image, migrate `identity_context` (000017, 000018), force-recreate.
- [ ] `deployments/scripts/e2e-identity-context-deps.py`. One check per edge:
  health of every dependency; tenant-entity-registry (resolve binding, entity tenant);
  access-control (role created → role_profile); authorization-svc (admin-route authz, and Kafka
  `iam.assignment.*` → envelope role_profile, IC-5); delegated-authority (Kafka `authority.*` →
  envelope delegations); gateway-auth (JWKS over mTLS; invalidate → `/verify` 401 within TTL, lag
  measured = IAM-8 revocation number); notification-svc (invite link sent); siem (session revoked
  streamed); Redis (session cache); Kafka outbox (`principal.status.changed` +
  `identity.subject.disabled`); OIDC discovery, code + PKCE, refresh rotation and reuse detection;
  TOTP enrol → step-up → gateway `X-Assurance-Level: MFA`; lifecycle create → invite → activate →
  login; password reset request → refused self-approve → approve by second admin; merge.
  Undo writes where possible; exit code = failures.

### IC-FE-1 · Console login with a confidential client (R-1)
- `app/api/auth/login/route.ts`: send `client_id` / `client_secret` from server-only env
  `IDENTITY_CONSOLE_CLIENT_ID` / `IDENTITY_CONSOLE_CLIENT_SECRET` (never `NEXT_PUBLIC_*`).
- Compose: add the console client to `IDP_CLIENT_SECRETS_FILE`, `IDP_CLIENT_CHANNELS="<id>=web"`,
  then `IDP_CLIENT_AUTH=required`.
- Verify live: login → resolve → gateway answers `X-Source-Channel-Basis: verified`.

### IC-FE-2 · API client
- `lib/api/identity.ts`: typed functions for every IC-1…IC-3 route and the existing support
  approve/review, MFA reset, status change (with `ifMatch`), all through `lib/api/envelope.ts`
  (bearer, Idempotency-Key, If-Match). `explainIdentityError` covers the new problem codes.

### IC-FE-3 · `/admin/identity` rebuilt in v2.0
- Replace the client-side playground `app/admin/identity/page.tsx` with a server page shaped like
  `app/admin/authorization/page.tsx`; add `actions.ts`, `state.ts` (reuse the authorization result
  shape), and `components/admin/identity/console/` (`data.ts`, `overview.tsx`, `principals.tsx`,
  `sessions.tsx`, `support.tsx`, `resets.tsx`, `events.tsx`, `federation.tsx`, `forms.tsx`,
  `diagnostics.tsx`). Reuse `RegisterView` from the authorization console (move it to
  `components/admin/console` if a second page needs it — keep one copy).
- **Header:** "Identity Context", gold primary "Add person" (CreateSubject), secondary "Check a
  session".
- **KPIs:** Active principals · Active sessions · MFA coverage (%) · Support access awaiting
  approval · Failed sign-ins (24 h). All from `/v1/identity/summary`.
- **Panels:** session lifecycle pipeline (Signed in → Context resolved → Stepped up → Ended);
  assurance ladder ordered list (Password, TOTP, Passkey, Federated) with counts; MFA coverage by
  role bar list with an amber callout for administrators without a second factor.
- **Tabs:** People · Sessions · Support access · Password resets · Sign-in events · Federation
  (honest "federation is not enabled" when off) · Diagnostics (resolve / explain / JWKS tools moved
  from the old page).
- **Drawer commands:** change status (reason, If-Match), reset MFA (step-up retry), invite, request
  password reset, approve/reject reset (hidden from the requester), end session, approve support
  access (only the named approver), merge duplicate (CRITICAL, step-up).
- **Footer:** "Required controls: approval by a second person for resets and support access ·
  step-up for privileged changes · a reason on every change · sessions end on disable".

---

## Service 2 — gateway-auth-svc (:8092, no database)

### GW-0 · Baseline and feature matrix (S0)
- Suite green, counts recorded. Create `services/gateway-auth-svc/progress.md` with the matrix from
  INT-01 §4.1 (ownership, runtime operations, the five queries, the request state machine, the four
  events `api_request_denied` / `api_rate_limited` / `api_route_version_unavailable` /
  `api_abuse_threshold_exceeded`, idempotency, failure semantics, the eight minimum acceptance
  tests), ZS-SEC-001 §8 ingress + edge rows, GOV-01 ingress duties, D1-5, AU-01/04/12/23/27.
- Fix the stale text: `openapi.yaml` (~line 97) and `RELEASE_CERTIFICATE.md` (~line 150) still say
  STEP_UP_MFA is not enforced; `internal/telemetry/telemetry.go:111` comment likewise. The code
  answers 401 `step_up_required` (`internal/handler/handler.go:375`).

### GW-1 · INT-01 minimum acceptance tests (implement what is missing)
- **File:** `internal/handler/int01_acceptance_test.go`. One test per spec line:
  `TestINT01_ForgedTenantHeader`, `TestINT01_ValidTokenDownstreamDenied` (gateway admits; the 403 is
  the service's, distinguishable from a gateway refusal), `TestINT01_VersionUnsupported`,
  `TestINT01_RequestSizeAbuse`, `TestINT01_RateLimitRace` (N goroutines never exceed burst),
  `TestINT01_RouteConfigRollback` (reloading the previous admission policy restores the previous
  supported versions), `TestINT01_ResidencyDenied`, `TestINT01_DownstreamTimeoutDistinguishable`.
- **Residency:** if no residency check exists, add one: the tenant context's
  `data_residency_policy_id` versus the route's region (from GTRM routing headers); a mismatch on a
  material request → 403 `residency_denied`, event `api_request_denied`. Fail closed when either
  side is unknown on a write.

### GW-2 · S5-D3 trusted hop and AU-04 edge login throttling
- Config `GATEWAY_TRUSTED_PROXY_CIDRS` + `GATEWAY_TRUSTED_PROXY_DEPTH`. Client IP = the right-most
  `X-Forwarded-For` hop not in a trusted CIDR; an untrusted peer's XFF is ignored. CARTA and the IP
  limiter key on it.
- A stricter per-IP bucket (`GATEWAY_LOGIN_RATE`, `GATEWAY_LOGIN_BURST`) on `/v1/authenticate`,
  `/v1/authenticate/step-up`, `/v1/account/*`, `/oauth2/login*`, `/oauth2/token`; 429 +
  `Retry-After`, event `api_rate_limited`. Route identity-context's public auth routes through
  Traefik (compose labels and GTRM routes).
- **Tests:** `TestClientIP_UntrustedPeerXFFIgnored`, `TestClientIP_TrustedChainTakesFirstUntrusted`,
  `TestAU04_LoginRoutesUseStricterBucket`, `TestAU04_ThrottleKeyedOnRealClientIP`. Live: 25 rapid
  failed logins through Traefik → 429 before the 25th.

### GW-3 · S3-D1 edge-set `X-Commercial-Plan` and `X-Org-Unit-Id`
- Plan from commercial-account-svc (read-only; find its plan route by grepping
  `services/commercial-account-svc` — do not edit it), cached per tenant with a short TTL; org unit
  from the envelope claim when present, otherwise not set.
- Both stripped from clients: GTRM `gtrm-edge-strip`, compose and GTRM `authResponseHeaders`;
  edge-contract test updated.
- **Tests:** `TestS3D1_ForgedPlanHeaderReplaced`, `TestS3D1_PlanUnavailableOmitsHeaderNeverGuesses`,
  `TestS3D1_OrgUnitFromEnvelopeOnly`.

### GW-4 · Admin read surface for the console
- Compose: `GATEWAY_ADMIN_ADDR=:8192` bound to `127.0.0.1:8192` on the host only (host 9092 is Kafka), token file from a
  compose secret (≥ 24 chars). Never published through Traefik.
- New reads in `internal/admin/admin.go`: `GET /internal/v1/gateway/decisions?outcome&tenant_id&limit`
  (recent window, all outcomes), `GET /internal/v1/gateway/usage` (all tenants on this replica),
  `GET /internal/v1/gateway/policy` (stripped headers, injected headers, CARTA mode, revocation TTL
  and fail mode, limiter settings, supported versions, body caps). Each answer states its scope
  ("this replica since start").
- **Tests** in `admin_test.go`: token required (401), each read's shape, `limit` bounds.

### GW-5 · IAM-1 per-service audience (largest; do last)
- Gateway: after verifying, exchange the session envelope for one whose audience is the target
  service, with `idenv.ForwardTo` / `Exchanger` (`services/identity-envelope/forward.go`) against
  identity-context's token endpoint (`GrantTokenExchange` already supported). Route → audience map
  from config `GATEWAY_AUDIENCE_ROUTES`, generated by `deployments/scripts/gen-gateway-routes.mjs`.
  Cache per (session, audience) until `exp - 30 s`. Unknown route on a protected path fails closed.
- identity-envelope already supports this — do not add a new setting:
  `NewGateFromEnv` (`services/identity-envelope/config.go:84`) sets `Audience` from
  `IDENTITY_AUDIENCE` (default = the service name) and accepts the legacy `zoiko-internal` while
  `IDENTITY_ACCEPT_LEGACY_AUDIENCE=true` (default). Rollout: gateway exchanges first; once the
  E2E shows every gated service receiving its own audience, set
  `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` per service in compose/k8s. Grep each of our gated
  services' `main.go` for `NewGateFromEnv` to list them; any service with a hand-rolled gate
  (policy-svc `identity_gate.go`) must read the same two settings.
- **Tests:** `TestIAM01_EnvelopeForPolicyRefusedByWorkflow` (identity-envelope),
  `TestVerify_ExchangesForRouteAudience`, `TestVerify_UnknownRouteFailsClosed`,
  `TestVerify_ExchangeFailureIs503NotPassThrough`. Live probe: policy-svc envelope at workflow-svc →
  401 in strict mode.

### GW-6 · Contract, docs, live, E2E (S2–S4)
- openapi, RUNBOOK, RELEASE_CERTIFICATE (re-certify), SPEC_DEVIATIONS (S5-D1 WAF/DDoS/bot at the
  GCP edge stays BLOCKED on the platform decision; document the intended Cloud Armor placement),
  `scripts/audit.sh` live.
- Rebuild image, force-recreate (no migration).
- `deployments/scripts/e2e-gateway-auth-deps.py`, through Traefik where the edge matters:
  identity-context (JWKS over mTLS; invalidate → 401 within TTL; step-up → `X-Assurance-Level: MFA`,
  real `X-Authn-Age-Seconds`); tenant-entity-registry (host ↔ tenant binding, mismatch 403, entity
  outside tenant 403); carta (STEP_UP_MFA → 401; carta stopped + write → 503, read → 200); siem
  (denial streamed); commercial-account (plan header, or recorded BLOCKED); Traefik (every forged
  header replaced or dropped end to end); authorization-svc (age header reaches its step-up);
  admin surface (a denied request's decision retrievable by id); throttle (429); audience (GW-5).

### GW-FE-1 · API client (server only)
- `lib/api/gateway-auth.ts`: admin reads via `GATEWAY_ADMIN_URL` + token read from
  `GATEWAY_ADMIN_TOKEN_FILE` on the server (`import "server-only"`); the token never reaches the
  browser.

### GW-FE-2 · `/admin/gateway` rebuilt in v2.0
- **KPIs:** Admitted · Denied · Rate-limited · Step-up required · Risk engine unavailable (this
  replica since start, labelled so).
- **Panels:** request pipeline (Received → Validating → Admitted / Denied / Rate-limited →
  Forwarded); trusted-header policy ordered list (stripped vs set by the edge, from
  `/policy`); denial reasons bar list with an amber callout when the risk engine was unavailable.
- **Tabs:** Denied requests (drawer = `GetGatewayDecision`) · Tenant usage · Rate limits (lookup by
  client/tenant) · Versions & admission policy · Verify my session (existing `GatewayVerifyForm`).
- No write commands (route and policy publication stay governed config); say so in the page.

> **Deviation (2026-11-04), GW-FE-1 and GW-FE-2:** the console does not read the admin surface.
> Decisions, usage and limiter state are held in memory per replica, so behind a load balancer every
> page load would show a different replica, and a decision-by-id lookup would miss. Fleet traffic
> belongs in Grafana (`gateway_*`) and denials in SIEM; GW-4's admin surface stays as a RUNBOOK tool.
> `/admin/gateway` is instead a v2.0 diagnostic page: probe and session KPIs, the /verify stage
> pipeline, header order, denial reasons, Verify my session, and signing keys. Matrix rows whose
> screen was a per-replica tab now give that reason, which the verifier walk below accepts. Details:
> `services/gateway-auth-svc/progress.md` (Decisions taken, GW-FE-1, GW-FE-2).

---

## Service 3 — access-control-svc (:8137, DB `access_control`)

### AC-0 · Baseline and feature matrix (S0)
- The working tree has an in-place repair of `000025_record_version.up/down.sql` (wrong table
  names; wrapped in a transaction with `IF NOT EXISTS`). Keep it — the original never applied
  cleanly — and record the R4 deviation in `progress.md` and `SPEC_DEVIATIONS.md`. Apply it up/down/up
  on a scratch DB, then commit.
- Suite with `-tags=integration` green, counts recorded.
- Extend `services/access-control-svc/progress.md` with the matrix from ZS-IAM-001 §5, §9 (objects),
  §10.1, §19, §21, §24, scenarios touching assignments/reviews, and D1-9.

### AC-1 · S9-D1 `resource.action` aliases for legacy names
- Migration (next free, expected **000027_permission_aliases**): `permission_aliases(legacy_action
  PK, canonical_action → permission_definitions)`, seeded by a generator
  `services/access-control-svc/scripts/gen_aliases.py` (deterministic rule
  `<DOMAIN>_<OBJECT>_<VERB>` → `<object>.<verb>`, domain-qualified on collision; collisions and
  unmapped names listed in progress.md for review). Callers' checks are never renamed.
- Provisioning to authorization-svc writes both names into `permitted_actions`, so a bundle written
  with either form satisfies a check made with the other. `GET /v1/permission-definitions` returns
  `aliases`.
- **Tests:** `TestS9D1_CanonicalGrantSatisfiesLegacyCheck`, `TestS9D1_LegacyGrantSatisfiesCanonicalCheck`,
  `TestS9D1_UnknownActionStillRefused`, `TestS9D1_AliasCannotPointAtUnregisteredAction`.

### AC-2 · Production grants by migration
- access-control migration (expected **000028_production_role_templates**): templates carrying the
  notification actions (`TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS`,
  `NOTIFICATION_RESOLVE_OUTCOME`), workflow actions, and register + template `iam.workload.manage`
  (authorization-svc follow-up).
- authorization-svc migration (next free, expected **000041_service_identity_grants**): platform
  role assignments for `svc-access-control` (at least `iam.assignment.revoke` for review REVOKE)
  and `svc-policy`, guarded and idempotent.
- **Tests:** store test per template; authorization IT `TestSvcAccessControlMayRevoke` → PERMIT.

### AC-3 · Console read gaps + API-4 cursor paging
- `GET /v1/iam/refused-escalations?filter[since]&filter[actor]` (from the 000006 table) →
  `iam.audit.read` (or the existing read action the store's tests use).
- `GET /v1/iam/summary` (KPI counts: active roles, requests awaiting approval, open review items,
  overdue review items, refused escalations last 30 days).
- Cursor paging on every list: role-definitions, permission-bundles, permission-definitions,
  role-templates, access-assignments, access-review-campaigns, iam/access-reviews, groups,
  group assignments, subject-links, refused-escalations. Keep offset paging only where documented
  as a small static list.
- **Tests per list:** first, next, tampered cursor 400, foreign-tenant cursor 400, unknown filter
  400; RLS as the app role.

### AC-4 · API-3 completion
- For every aggregate in 000025 (roles, bundles, protected permissions, assignment requests,
  groups, group assignments, group members, review campaigns, review items) and subject links:
  GET returns `ETag`; every mutation (approve, reject, cancel, revoke, decide, reassign, complete,
  member add/remove, group-assignment commands, template instantiate/upgrade) has three tests —
  missing `If-Match` 428, stale 412, current 2xx.
- Self-approval tests: `TestAC_RequesterCannotApproveOwnRequest`,
  `TestAC_ReviewerCannotDecideOwnAccess`, `TestAC_CriticalRoleNeedsSecurityApprover`.

### AC-5 · Contract, docs, live, E2E (S2–S4)
- openapi, asyncapi, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, `scripts/audit.sh` live.
- Build image; `migrate.py up --db access_control` (and authorization for 000041); force-recreate.
- The service already mounts the identity-envelope gate (`cmd/server/main.go:376`,
  `IDENTITY_VERIFY`). Once AC-FE-1 sends the bearer on every call, set `IDENTITY_VERIFY=require`
  for access-control-svc in compose and prove a header-only call gets 401 in the E2E.
- `deployments/scripts/e2e-access-control-deps.py`: authorization-svc (role create provisions;
  bundle provisions; request → approve → `/v1/authorize` PERMIT; review REVOKE as
  `svc-access-control` over mTLS → DENY; alias grant satisfies legacy check); SoD (conflicting
  request refused via `/v1/sod/validate`); identity-context (role change ends sessions: envelope
  refused after revoke); Kafka HR events (employment change via a subject link opens an
  event-triggered review); outbox (`role.created` invalidates authz cache); mtls-management (:8449
  admin call); expiry sweeper (effective-dated revoke lands); groups (group assignment fans out to
  one request per member).

### AC-FE-1 · API client
- `lib/api/access-control.ts`: functions for permission definitions (with aliases), role
  templates (+ instantiate, upgrade), access assignments (+ approve/reject/cancel/revoke), review
  campaigns (+ decide, reassign, complete), my review items, groups (+ members, group
  assignments + commands), subject links, refused escalations, summary — all through
  `lib/api/envelope.ts`, sending `If-Match` from `record_version`.

### AC-FE-2 · `/admin/access-control` rebuilt in v2.0
- **Header:** "Roles & Access", gold primary "Request access".
- **KPIs:** Active roles · Requests awaiting approval · Open review items · Overdue review items ·
  Refused escalations (30 d).
- **Panels:** access request pipeline (Requested → SoD checked → Approved (security approver for
  CRITICAL) → Provisioned); role archetypes ordered list (the 21 §9.1 templates, version chip);
  permission registry by risk tier bar list with an amber callout for legacy names without an alias.
- **Tabs:** Roles · Permission bundles · Permission registry · Role templates · Access requests ·
  Access reviews · My reviews · Groups · Subject links · Refused escalations.
- Requester never sees approve/reject; reviewer never decides their own access; every command has
  the reason dialog and step-up retry.

---

## Verifier protocol (Claude re-runs this after each service; nothing is taken on the summary)

1. `git log` / `git show --stat` for the service's commits: only R1 paths, explicit staging, no
   binaries, one commit per task.
2. Build, `go vet`, full suite on a scratch Postgres; counts must match `progress.md`; store suites
   0 skip.
3. **Feature matrix walk:** every row has a route or consumer, a named test, and a console screen —
   or a BLOCKED reason that names the external dependency. Spot-read the spec for 10 random rows.
4. Grep callers from `cmd/server/main.go` for every new handler, store method, consumer and ticker
   (R7).
5. kin-openapi validation and route-parity tests pass; every new route is documented.
6. Each new migration: number, `.down.sql`, RLS + FORCE with `NULLIF`, guarded grants; up/down/up
   on scratch.
7. Every new action registered in access-control and carried by a template (R5).
8. Live: image rebuilt (fresh timestamp), migrations applied, `e2e-<svc>-deps.py` all PASS, and
   `scripts/audit.sh` green.
9. Mutation check: break two guards on a scratch copy (e.g. self-approval check, cursor tenant
   binding); a test must fail for each.
10. Frontend: `tsc --noEmit` and `eslint` on touched files clean; each tab rendered against the live
    service with screenshots; one empty and one error state shown honestly; no invented numbers.

Any failure reopens the task.

## BLOCKED (build the interface + local implementation, mark only the production part)

- External IdP vendor (D-4): federation runs against the mock IdP in tests and an operator-set IdP
  live.
- Production edge WAF/DDoS/bot controls (S5-D1): platform decision; documented placement only.
- Security sign-offs: TOTP as interim factor (D-1), session maximums (D-2), revocation SLO by tier
  (D-5), sender-constrained tokens per client class (AU-27, D-7).
- commercial-account-svc availability for live entitlement / plan checks (IC-6, GW-3): code and
  tests done; live half recorded BLOCKED if the service is not running.
- Traefik `serversTransport` client certificates (S4-B1) and per-service mTLS enrolment (M-01):
  infrastructure.

## Execution handoff

First step after approval: copy this file to
`docs/audit_files/AUTH-PLANE-PLAN-2026-11-02.md` in the backend repo and commit it
(`docs: add auth-plane completion plan`), so the executing model reads it from the repo.

Run one service per session with another model, in order: **identity-context-svc → gateway-auth-svc
→ access-control-svc** (the gateway's audience exchange needs identity's token endpoint; access-control's
grants cover the identity actions). Give that model this file plus IMPLEMENTATION-PLAN §2/§5/§7.
At the end of each service, return here for the verifier protocol before starting the next.
