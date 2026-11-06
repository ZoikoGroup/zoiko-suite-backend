# Domain 1 + Domain 2: completion tracker, plan and audit record

One file for all 26 units in scope: every feature with its status and the tasks that finish it (first half), then the rules, decisions, task specifications and open audit registers behind them (Reference), then the route and console-coverage inventories and a platform explainer (Appendices A–C).

**Date:** 6 November 2026
**Basis:** backend branch `shashi-changes` at `9be6c41d` (30 commits ahead of and 1 behind `origin/main` as last fetched on 4 Nov). Frontend branch `shashi-changes`.
**Scope:** all 26 units of rule R1. That is the 16 core services (Domain 1, Identity, Scope & Foundation, D1-1…D1-9; Domain 2, Governance Platform, D2-1…D2-7) and the 10 supporting units (D2-8 schema-registry-svc, D2-9 retention-registry-svc, D2-10 the shared libraries and the GTRM compiler, D1-10…D1-13 the Security & Trust services). Finishing every session (0–25) and the definition of done means Domain 1 and Domain 2 are complete except production. Teammates' services (org-structure, commercial-account, document-vault, evidence-manifest, esignature, schema-registry callers, …) appear only as dependencies.

**Replaces twelve files** (all removed from `docs/audit_files` on 6 Nov; read any of them with `git show 9be6c41d:"docs/audit_files/<path>"`):
- merged into the Reference chapter: `IMPLEMENTATION-PLAN-2026-10-23.md`, `AUTH-PLANE-PLAN-2026-11-02.md`, `Auth-Service-audit-2026-10-14.md`, `Identity, Scope & Foundation-audit-2026-09-23.md`, `Governance Platform-audit-2026-09-28.md`, `IMPLEMENTATION-PROGRESS.md` (the "Sources merged" table in the Reference says what each kept and dropped);
- merged into Appendix C: `testing_shashi_progress/agenda_&_functionality_of_zs.md`;
- `testing_shashi_progress/DOMAIN1-DOMAIN2-AUDIT-AND-PLAN-REFERENCE.md` (6 Nov, never committed; all of it is in the Reference chapter);
- `testing_shashi_progress/domain1-domain2-features-and-requirements.md` (2 Nov status, replaced by the feature tables);
- `EVIDENCE-REQUIREMENTS-PLAN-2026-11-04.md` (executed 5 Nov; its leftovers are ER-W3…ER-W8);
- `testing_shashi_progress/group1-identity-foundation-testing.md` and `group2-governance-platform-testing.md` (16–21 Oct; open items carried into "Carried over from the removed group testing files");
- `Legal-Corporate-Commercial-and-Tax-Compliance-audit-2026-10-12.md` (Domains 6 and 7, outside this file's scope).

**Sources used for every status:**
- The Reference chapter of this file (rules, decisions, task specifications, scores and open registers from the six merged files).
- Each service's `progress.md` and `SPEC_DEVIATIONS.md`.
- `git log 2a87f299..HEAD` per service (128 commits since the 2 Nov baseline, 49 of them by 07Yashjais merged from main).
- The routes registered in each service's router, extracted from the code on 6 Nov: 824, not counting health, readiness and metrics. Each one is mapped to its feature row in Appendix A.
- A build, `go vet` and unit-test sweep of all 16 modules on 6 Nov (results in the dashboard).
- The frontend repo on 6 Nov: which console pages use the v2.0 kit, and which backend routes each `lib/api` client calls (Appendix B).

**Reading references in this file:** the short names point to Parts of the Reference chapter below.
- "IMPLEMENTATION-PLAN §n" → Part A (same § numbers; §6 tasks in A5).
- "AUTH-PLANE plan" → Part B.
- "Auth-Service audit" and AU rows → Part C.
- "Identity audit" → Part D.
- "Governance audit" → Part E.
- "IMPLEMENTATION-PROGRESS" → Part F.

Text not carried into the Reference (closed passes, the old status board, evidence blocks) is in git: `git show 9be6c41d:"docs/audit_files/<file name>"`.

## How to use this file

**Implementing model:**
1. Read "Do first", the rules under "Execution order", the section of the service you are given, and the Reference Parts it points to.
2. Work one service per session, in the order under "Execution order". Do the backend tasks in the order listed in the service section, then its "Console (frontend)" tasks, following "Frontend rules for every console". A service is finished only when its console drives it end to end in the browser test.
3. Tick a task (`- [x]`) when its "Done when" holds, and change the feature rows it closes to 🔵 with the commit id and test names in the Evidence column. Never set ✅ yourself.
4. Finish with the service's hand-back (S0–S6), then stop and hand back for verification.

**Verifier (Claude):** run the verifier protocol ("Verifier protocol", before the Reference) for the service handed back, including VC-1 for its "Reconfirm" rows. Rows that pass become ✅ and the dashboard is updated. Any failure reopens the task.

## Status legend

| Emoji | Meaning |
|---|---|
| ✅ | Done: built, tested, and confirmed by the latest audit or verifier pass (evidence cited) |
| 🟡 | Partial: built, but part of the behaviour, a dependency wiring or the proof is missing |
| 🔵 | Built, not verified: routes and code exist, but no audit or verifier pass has checked them |
| ❌ | Missing: not built |
| ⛔ | Blocked: waits on infrastructure, a vendor or a service that does not exist or does not run yet (named in the row) |
| 🧭 | Decision: waits on a decision by a named owner (decision id in the row) |
| 🏁 | Service level only: every stage S0–S6 passed the verifier protocol, and every row is confirmed. None today |

**Confirmation rule (6 Nov):** every row that was ✅ is now 🟡, and its Evidence starts with "Reconfirm (was ✅)". Those rows need only the confirmation check VC-1 (in "Verifier protocol"), not new work. The dashboard counts them separately from rows that are really partial. A row becomes ✅ again only after VC-1 passes

Stages (from the AUTH-PLANE plan): **S0** baseline + feature matrix · **S1** backend gaps · **S2** contract and docs · **S3** live container · **S4** dependency E2E · **S5** console v2.0 · **S6** evidence + hand-back.

---

# Dashboard

| # | Service | Overall | Features | ✅ confirmed | 🟡 reconfirm | 🟡 partial | 🔵 | ❌ | ⛔ | 🧭 | Suite 6 Nov (build · vet · unit) | Stages | Console page · API calls | Next task |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| D1-1 | identity-context-svc :8080 | 🟡 | 34 | 0 | 19 | 11 | 0 | 1 | 1 | 2 | ✔ · ✔ · ✔ 26 pkgs | S0–S6 verified 3 Nov; reopened 4 Nov (K-1…K-7, S1-14) | v2.0 · 43/79 | IC-11 |
| D1-2 | tenant-entity-registry-svc :8081 | 🟡 | 27 | 0 | 11 | 3 | 3 | 9 | 1 | 0 | ✔ · ✔ · ✔ 13 pkgs | none (no `progress.md`) | legacy · 42/79 | TER-0 |
| D1-3 | configuration-feature-flag-svc :8086 | 🟡 | 19 | 0 | 8 | 5 | 1 | 5 | 0 | 0 | ✔ · ✔ · ✔ 6 pkgs | none | legacy · 6/26 | CFG-0 |
| D1-4 | secret-vault-integration-svc :8087 | 🟡 | 17 | 0 | 7 | 3 | 1 | 3 | 3 | 0 | ✔ · ✔ · ✔ 9 pkgs | none | legacy · 11/16 | SV-0 |
| D1-5 | gateway-auth-svc :8092 | 🟡 | 20 | 0 | 8 | 7 | 0 | 2 | 1 | 2 | ✔ · ✔ · ✔ 9 pkgs | S0 ✔, S1 partial, S5 ✔; S2–S4, S6 open (verifier: hold) | v2.0 · 0/0 | FE-H, GW-6a |
| D1-6 | search-indexer-svc :8096 | 🟡 | 16 | 0 | 9 | 2 | 0 | 3 | 1 | 1 | ✔ · ✔ · ✔ 13 pkgs | none | legacy · 16/20 | SI-0 |
| D1-7 | notification-svc :8133 | 🟡 | 24 | 0 | 9 | 4 | 4 | 7 | 0 | 0 | ✔ · ✔ · ✔ 25 pkgs | none | legacy · 6/91 | NT-0 |
| D1-8 | delegated-authority-svc :8136 | 🟡 | 12 | 0 | 7 | 1 | 0 | 4 | 0 | 0 | ✔ · ✔ · ✔ 7 pkgs | none | legacy · 4/9 | DA-0 |
| D1-9 | access-control-svc :8137 | 🟡 | 18 | 0 | 9 | 2 | 1 | 6 | 0 | 0 | ✔ · ✔ · ✔ 7 pkgs (integration suite not run) | none (AC-0…AC-FE-3 not started) | legacy · 3/50 | AC-0 |
| D1-10 | mtls-management-svc :8140 | 🟡 | 13 | 0 | 3 | 4 | 1 | 4 | 0 | 1 | ✔ · ✔ · ✔ 5 pkgs | none | shared `/admin/security`, dead-wired · 7/9 | MT-0 |
| D1-11 | siem-integration-svc :8141 | 🟡 | 11 | 0 | 0 | 2 | 0 | 8 | 1 | 0 | ✔ · ✔ · ✔ 1 pkg | none (phase6 compose only) | shared, dead-wired · 3/5 | SG-0 |
| D1-12 | carta-svc :8142 | 🟡 | 8 | 0 | 2 | 1 | 0 | 4 | 1 | 0 | ✔ · ✔ · ✔ 2 pkgs | none (phase6 compose only) | shared, dead-wired · 2/3 | CA-0 |
| D1-13 | key-management-svc :8143 | 🟡 | 11 | 0 | 1 | 0 | 0 | 9 | 1 | 0 | ✔ · ✔ · ✔ 3 pkgs | none (phase6 compose only) | shared, dead-wired · 5/5 | KM-0 |
| D2-1 | jurisdiction-rules-svc :8082 | 🟡 | 29 | 0 | 5 | 4 | 16 | 2 | 1 | 1 | ✔ · ✔ · ✔ 11 pkgs | none (no `progress.md`) | legacy · 12/148 | JR-0 |
| D2-2 | governance-decision-log-svc :8083 | 🟡 | 13 | 0 | 4 | 4 | 0 | 4 | 0 | 1 | ✔ · ✔ · ✔ 6 pkgs | none | legacy · 2/4 | GDL-0 |
| D2-3 | policy-svc :8085 | 🟡 | 14 | 0 | 7 | 2 | 1 | 3 | 0 | 1 | ✔ · ✔ · ✔ 12 pkgs | none | legacy · 7/39 | POL-0 |
| D2-4 | obligations-svc :8088 | 🟡 | 14 | 0 | 4 | 5 | 2 | 3 | 0 | 0 | ✔ · ✔ · ✔ 6 pkgs | none | legacy · 7/24 | OB-0 |
| D2-5 | authorization-svc :8089 | 🟡 | 23 | 0 | 13 | 4 | 1 | 4 | 0 | 1 | ✔ · ✔ · ✔ 14 pkgs | S0–S6 verified 2 Nov (main merged since); reconfirm | v2.0 · 64/80 | AZ-3 |
| D2-6 | workflow-svc :8090 | 🟡 | 23 | 0 | 8 | 7 | 2 | 4 | 1 | 1 | ✔ · ✔ · ✔ 17 pkgs (3 re-run after an App Control block) | none | legacy · 19/76 | WF-0 |
| D2-7 | evidence-requirements-svc :8130 | 🟡 | 15 | 0 | 9 | 4 | 0 | 1 | 0 | 1 | ✔ · ✔ · ✔ 7 pkgs | S0–S6 verified 5 Nov; reconfirm | v2.0 · 18/22 | ER-W4 |
| D2-8 | schema-registry-svc :8093 | 🟡 | 10 | 0 | 2 | 1 | 0 | 6 | 0 | 1 | ✔ · ✔ · ✔ 5 pkgs | none (no `progress.md`) | legacy · 5/5 | SR-0 |
| D2-9 | retention-registry-svc :8148 | 🟡 | 11 | 0 | 4 | 1 | 1 | 4 | 0 | 1 | ✔ · ✔ · ✔ 4 pkgs | none (no openapi, no `progress.md`) | legacy · 7/7 | RR-0 |
| D2-10 | libraries + GTRM compiler | 🟡 | 10 | 0 | 2 | 2 | 2 | 3 | 0 | 1 | ✔ · ✔ · ✔ 23 pkgs (gtrm root: stray `go.mod`) | none | — (no UI) | LIB-1 |
| | **Total (23 sections, 26 units)** | | **392** | **0** | **151** | **79** | **36** | **99** | **12** | **15** | **26 / 26 modules green** | 0 🏁 (2 awaiting reconfirmation) | **4 v2.0 · 289/797** | |

"Unit" means `go test -count=1 ./...` without the `integration` tag. Store integration suites need a scratch Postgres and were not re-run on 6 Nov; each service's S0 task runs them.

---

# Do first (before any service session)

1. **Look at, then remove, the stray files** in the working tree. They are untracked and none is referenced by the code:
   - `services/configuration-feature-flag-svc/internal/handler/handler.go.bak`, `handler.go.tmp`, `handler.go.tmp2` (leftovers of the 2 Nov build repair);
   - `services/notification-svc/handlercheck.bin`;
   - `services/workflow-svc/tmp/` (`kilo_test`, `workflow_test.dat`) and `services/workflow-svc/workflow_handler_test.bin`.
2. **Decide the `deployments/gtrm` module change:** `deployments/gtrm/compiler/go.mod` is modified and `deployments/gtrm/go.mod` is untracked. Review the diff; commit it with a reason or revert it.
3. **Merge `origin/main`** into `shashi-changes` (fetch first; it was 1 commit behind on 4 Nov) and re-run the sweep. Do this again before each service session.
4. **Run CI** on the branch and read the result. No CI run exists for the 30 local commits.
5. **Make the access-control integration suite runnable** (it blocks AC-0 and ER-W3): embedded Postgres refuses to run `initdb` as uid 1000 in `golang:1.26-alpine`. Run it as a named non-root user, or point `TEST_DATABASE_URL` at a `postgres:16` container.
---

# Domain 1 — Identity, Scope & Foundation

These services answer "who are you, which organisation are you in, and what is switched on?"

## D1-1 · identity-context-svc (:8080, DB `identity_context`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-01 §4; ZS-IAM-001 §3, §13, §17, §23; AU-01…AU-11 (Part C) |
| Latest score | 41/44 (93.2%, 95.5% weighted), re-audit 4 Nov. NP4 dropped to ⚠️ (K-1) |
| Open gap register | Identity audit → "identity-context-svc gap register, 4 November 2026" (14 gaps + 5 proof items) |
| Stages | S0–S6 completed 3 Nov (verifier protocol passed). Reopened by the 4 Nov re-audit |
| Console | `/admin/identity` v2.0 (3 Nov) |
| Feature matrix | `services/identity-context-svc/progress.md` (IC-0 matrix) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-1.F01 | Resolve context into a signed envelope: `POST /v1/context/resolve` | 🟡 | Reconfirm (was ✅): Identity audit 1/9; suite 828/0/0 on 4 Nov | VC-1 |
| D1-1.F02 | Effective context and explain: `GET /v1/context/session/{id}`, `GET …/explain` | 🟡 | Reconfirm (was ✅): Identity audit 1/9 | VC-1 |
| D1-1.F03 | Cache refresh, tenant invalidation, session invalidation: `POST /v1/context/cache/refresh`, `/v1/context/tenant/invalidate`, `/v1/context/session/{id}/invalidate` | 🟡 | Reconfirm (was ✅): Identity audit 1/9 | VC-1 |
| D1-1.F04 | Session register (API-4): `GET /v1/context/sessions` | 🟡 | Reconfirm (was ✅): progress.md IC-2b; verifier 3 Nov | VC-1 |
| D1-1.F05 | Break-glass support context: `POST/GET /v1/context/support`, `GET/DELETE …/{id}`, `…/approve`, `…/review` (named approver, 4 h max, reconciler) | 🟡 | Reconfirm (was ✅): Identity audit 1/9; progress.md IC-2c | VC-1 |
| D1-1.F06 | Password login with a confidential client: `POST /v1/authenticate`; compose `IDP_CLIENT_AUTH=required` | 🟡 | Compose requires the client. The console sends it only when `IDENTITY_CONSOLE_CLIENT_ID/_SECRET` are set; `.env.example` leaves them empty (K-5, S1-B1) | IC-FE-4 |
| D1-1.F07 | OIDC provider: discovery, `/.well-known/jwks.json`, `/oauth2/authorize` (code + PKCE), `/oauth2/token` (refresh rotation, token exchange), `/oauth2/revoke`, `/oauth2/introspect`, `/oauth2/userinfo`, `/oauth2/login`, `/oauth2/login/mfa`, `/oauth2/logout`, `/v1/logout`, `/v1/context/session/introspect` | 🟡 | Reconfirm (was ✅): AU-01…AU-11 re-score, 3 Nov | VC-1 |
| D1-1.F08 | TOTP MFA: `/v1/mfa/totp/enroll`, `/activate`, `/revoke`, `GET /v1/mfa/factors`, `POST /v1/authenticate/step-up`, admin reset `POST /v1/principals/{id}/mfa/reset` | 🟡 | Reconfirm (was ✅): AU re-score 3 Nov; MFA UI 3 Nov (N-9) | Security sign-off: D1-1.F31; VC-1 |
| D1-1.F09 | Passkeys (AU-11): `/v1/mfa/passkeys`, `/register/begin`, `/register/finish`, `/revoke`, `/v1/authenticate/step-up/passkey/*`, `/oauth2/login/passkey/*` | 🟡 | Reconfirm (was ✅): `TestAU11_*`; live E2E 61/61 (3 Nov) | Real-browser ceremony still owed: IC-18; VC-1 |
| D1-1.F10 | Subject lifecycle (AU-09): `POST /v1/principals`, `…/{id}/invite`, `/v1/account/activate`, `/v1/account/password`, `/v1/account/password/forgot`, `/v1/account/password/reset` | 🟡 | Reconfirm (was ✅): `TestAU09_*`; progress.md IC-1 | VC-1 |
| D1-1.F11 | Password resets with maker-checker: `POST /v1/principals/{id}/password-resets`, `GET /v1/password-resets`, `GET …/{id}`, `POST …/{id}/decide` | 🟡 | K-7 / S1-13: the metric counts refusals, a store outage answers 409, the expiry write rolls back, intermediate approvals emit no event, an undelivered link strands the request | IC-13 |
| D1-1.F12 | Principal admin: `GET /v1/principals` (API-4), `GET /v1/principals/{id}`, `PUT /v1/principals/{id}/status`, `GET …/{id}/roles`, `GET …/{id}/delegations` | 🟡 | Reconfirm (was ✅): Identity audit; `TestUpdatePrincipalStatus_NoSelfExemption` | Generic status PUT → named commands: ST-3 (cross-cutting); VC-1 |
| D1-1.F13 | Console read models: `GET /v1/authentication-events`, `/v1/principals/{id}/assurance-history`, `/v1/identity/summary` (migration 000017, RLS) | 🟡 | Reconfirm (was ✅): progress.md IC-2; `TestRLS_ListsNeverCrossTenant`; `TestIC2_PasswordResetsRegisterPagesAndNamesTheRequester` | VC-1 |
| D1-1.F14 | ORG-1 subject identity and merge with step-up: `GET /v1/subjects/{id}`, `GET …/links`, `POST /v1/subjects/{survivorID}/merge` | 🟡 | Reconfirm (was ✅): `TestORG01_*`; authorization-svc re-points merged assignments (0174312a) | VC-1 |
| D1-1.F15 | Main's ORG-01 routes: `GET /v1/identities/{id}`, `…/assurance-history`, `…/authentication-events`, `…/links`, `POST …/merge` | 🟡 | S1-14: `POST /v1/identities/{id}/merge` asks for no step-up and writes only `identity.subject.merged`, so authorization-svc still reads the duplicate as ACTIVE | IC-14 |
| D1-1.F16 | Federation (OIDC): `/v1/federation/connections` (+ approve/reject versions, suspend, resume, revoke, rotate-metadata, keys/refresh, logins), `/v1/federation/links`, `POST /v1/authenticate/federated`, `GET /oauth2/login/federate/{idpID}` | 🟡 | Reconfirm (was ✅): progress.md IC-9e/IC-9f; `TestAdminRoutesAuthorizeAtTheLegalEntityNotTheTenant`; live probe 3 Nov | VC-1 |
| D1-1.F17 | SCIM v2 and provisioning admin: `/scim/v2/{tenant}/{connection}/Users*`, `ServiceProviderConfig`; on `/v1/federation/connections/{id}`: `…/mapping` (GET/POST), `/v1/federation/connections/{id}/mapping/versions/{v}/approve`, `/v1/federation/connections/{id}/mapping/versions/{v}/reject`, `/v1/federation/connections/{id}/scim-token`, `/v1/federation/connections/{id}/scim-token/revoke`, `/v1/federation/connections/{id}/provisioning/status`, `/v1/federation/connections/{id}/provisioning/events`, `/v1/federation/connections/{id}/provisioning/exceptions`, `/v1/federation/connections/{id}/provisioning/exceptions/{id}/resolve`, `/v1/federation/connections/{id}/provisioning/reconcile`, `/v1/federation/connections/{id}/provisioning/deprovision-latency` | 🟡 | Built and tested with a SCIM client (IC-9c). Live SCIM is off locally and the service principal holds no operator grant (S1-B5) | IC-15 |
| D1-1.F18 | SAML federation | 🧭 | S1-D1: build SAML, or record OIDC-only in SPEC_DEVIATIONS (Security / platform owners) | Decision |
| D1-1.F19 | Role projection from `iam.assignment.*`, `authority.*`, `role.updated`, `entity.updated` (Kafka consumer) | 🟡 | K-1 / S1-9: a revocation, expiry or suspension older than the 6-minute horizon is skipped whole, so the role stays framed. K-2 / S1-10: a suspension consumed before its grant is lost | IC-11, IC-12 |
| D1-1.F20 | Session cap with eviction event (human cap 5, workload cap 20) | 🟡 | Reconfirm (was ✅): `TestSessionCap_*`; progress.md IC-4 | VC-1 |
| D1-1.F21 | `identity.session.revoked` for bulk evictions, with tenant and real actor | 🟡 | K-6 / S1-12: bulk evictions emit nothing; emitted events carry no tenant; the single-session event is published after commit | IC-16 |
| D1-1.F22 | Event contract (`asyncapi.yaml`) | 🟡 | K-3 / S1-11: five emitted events undeclared; the contract test checks a hand-written list; nine constants have no emitter | IC-17 |
| D1-1.F23 | Canonical IAM §23 event names next to wire names (IAM-10) | 🟡 | Reconfirm (was ✅): `TestIAM10_ProducerEmitsBothNames` | Schema registration waits for G-08; VC-1 |
| D1-1.F24 | Observability (IAM-6): MFA, lockout, replay, throttle, session and reset metrics + alert rules | 🟡 | Reconfirm (was ✅): progress.md IC-8; verifier 3 Nov | VC-1 |
| D1-1.F25 | Entitlement context reference (IAM-2, S1-C1) | ⛔ | Client and config done (`TestConfig_CommercialAccountURLRequiredOutsideLocal`). commercial-account-svc does not run locally; compose sets no `COMMERCIAL_ACCOUNT_URL` | Run commercial-account-svc, set the URL, prove a resolve records the reference |
| D1-1.F26 | Ingress-binding refresh before TTL (S1-B3) | 🟡 | Reconfirm (was ✅): `TestIngressBindingRefreshedBeforeTTL`; refresher in `cmd/server/main.go` | VC-1 |
| D1-1.F27 | Identity actions registered and carried by templates (R5) | 🟡 | Reconfirm (was ✅): access-control 000026 and 000027 | VC-1 |
| D1-1.F28 | Idempotency-Key (principal-bound), outbox, RFC 9457 (API-2), ETag/If-Match on Principal and SupportContext (API-3) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-2/API-3 | VC-1 |
| D1-1.F29 | Console never signs people in from its own credential list | 🟡 | K-4 / S1-B4: the console falls back to a local list when identity-context refuses or is down | IC-FE-4 |
| D1-1.F30 | Console `/admin/identity` v2.0 (People, Sessions, Support access, Password resets, Sign-in events, Federation, Diagnostics) | 🟡 | Page verified 3 Nov (IC-FE-3). The client calls 43 of 79 screen-relevant routes: no public account pages, no federation admin, no support-access review (Appendix B) | IC-FE-5, IC-FE-6 |
| D1-1.F31 | Security sign-offs: TOTP as interim factor; passkey attestation `none`; passkey-first login | 🧭 | S1-D2, S1-D3 (Security / product) | Decision |
| D1-1.F32 | IAM-7 DoD artefacts: re-certify `RELEASE_CERTIFICATE.md` and refresh `scripts/audit.sh` after the 4 Nov fixes | 🟡 | Re-certified 3 Nov; the K-1…K-7 fixes will change behaviour | IC-18 |
| D1-1.F33 | IAM-8 load and revocation SLOs (AU-21) | ❌ | IMPLEMENTATION-PROGRESS: TODO | IAM-8 |
| D1-1.F34 | Unique migration numbers | 🟡 | Two files each for 000012, 000013, 000016, 000017 and 000018 in `deployments/migrations` (ours and main's after the 3–4 Nov merges). The suite applies them by file name; a version-keyed runner would refuse or skip one | IC-19 |

**Tasks**

- [ ] **IC-11 · S1-9 (K-1) projection survives late revocations.** Apply the revocation horizon to session eviction only. Always write the projection (`EndRoleAssignment`, `SuspendRoleAssignment`, tombstone). Add a resync from authorization-svc's assignments and a dead-letter record for an event given up after its retries.
  - Files: `internal/events/consumer.go` (~371-377), `internal/store/pg_role_projection.go`, new resync job wired in `cmd/server/main.go`.
  - Tests: `TestIC11_LateRevokeClosesProjectionWithoutEviction`, `TestIC11_LateSuspendHoldsProjection`, `TestIC11_ReplayFromEarliestFramesNoRevokedRole`, `TestIC11_GivenUpEventIsDeadLettered`.
  - Done when: NP4 is re-scored ✅ (an entry in Part G) with these tests as evidence.
- [ ] **IC-12 · S1-10 (K-2) suspension before grant.** Record a suspension that has no row yet (keyed tenant + assignment, like the tombstone); `UpsertRoleAssignment` applies it on insert.
  - Tests: `TestIC12_SuspendThenGrantNotFramed`, `TestIC12_SuspendGrantResumeFramed`.
- [ ] **IC-13 · S1-13 (K-7) password-reset decisions.** Count the metric after commit; map store outages to 503; commit the expiry before refusing; emit an event for every decision; answer 502/503 (or allow re-issue) when link delivery fails.
  - Tests: one per point, `TestIC13_*`.
- [ ] **IC-14 · S1-14 merge route parity.** `POST /v1/identities/{id}/merge` requires `steppedUpActor`; `lifecycle.MergeDuplicate` also writes `principal.status.changed` and `identity.subject.disabled`. Agree the change with the author of main's ORG-01 waves (07Yashjais) first, because it changes `handler_merge_test.go` and `lifecycle/merge_test.go`.
  - Tests: `TestIC14_IdentitiesMergeNeedsStepUp`, `TestIC14_MergeEmitsStatusEvents`; authorization-svc projection reads the duplicate as DISABLED.
- [ ] **IC-15 · S1-B5 live SCIM.** Grant the operator role to `IDENTITY_SERVICE_PRINCIPAL_ID` by migration (access-control), set `FEDERATION_PROVISIONING_ENABLED=true` in compose, run SCIM create / disable / delete live.
- [ ] **IC-16 · S1-12 (K-6) per-session revoke events.** Emit one `identity.session.revoked` per ended session through the outbox, in the invalidation's transaction, with `tenant_id` and the real actor (or causing event).
  - Tests: `TestIC16_StatusChangeEndingTwoSessionsEmitsTwoEvents`, `TestIC16_EventCarriesTenantAndActor`.
- [ ] **IC-17 · S1-11 (K-3) event contract.** Declare the five undeclared events in `asyncapi.yaml`; derive the contract test's list from the producers; delete or wire the nine unused constants.
  - Mutation check: drop one declaration → the contract test fails.
- [ ] **IC-18 · Proof owed.** Rebuild the image after `15a9c581`; run `e2e-identity-context-deps.py` with suspend and resume steps; rotate signing keys with two published keys (AU-05); a passkey ceremony in a real browser; re-run mutants; re-certify `RELEASE_CERTIFICATE.md`; refresh `scripts/audit.sh`.
- [ ] **IC-19 · Migration numbering.** Renumber the later file of each duplicated pair (000012, 000013, 000016, 000017, 000018) to the next free numbers, as new files with their `.down.sql`, and add a test that fails on a duplicated prefix (also run it for every service in the sweep). Check `deployments/migrate.py` status on the dev DB before and after, so applied migrations are not re-run. Agree the renumbering with the author of main's files (07Yashjais).
- [ ] **IC-FE-4 · Console sign-in (frontend repo).** Remove the local credential fallback outside development and never after a 401 / `invalid_client` (S1-B4). Document the console client in the env setup and refuse to start without it outside development (S1-B1). Then re-score "source channel".
- [ ] **IAM-8 · Load and revocation SLOs** (shared with authorization-svc, see D2-5): measure revocation lag identity → gateway `/verify`.

**Console (frontend)** — `app/admin/identity` (v2.0, 3 Nov) · clients `lib/api/identity.ts`, `lib/api/identity-console.ts` call 43 of 79 screen-relevant routes (Appendix B). Follow "Frontend rules for every console" below.

The page exists. What is missing:
- the public account pages;
- the full federation admin;
- support-access review;
- subject links.

| Part | Content |
|---|---|
| Public pages (new, outside `/admin`) | `/activate?token=` → `POST /v1/account/activate` (D1-1.F10). `/forgot-password` → `POST /v1/account/password/forgot`, always the same message (no enumeration). `/reset-password?token=` → `POST /v1/account/password/reset`. Account › Change password → `POST /v1/account/password`. Login: a "Sign in with <IdP>" button when federation is on → `GET /oauth2/login/federate/{idpID}` (D1-1.F16) |
| Federation tab (extend) | Connections register + drawer: `GET …/{id}`, versions approve/reject (maker ≠ checker), suspend, resume, revoke, rotate-metadata, keys/refresh, logins. Sub-tabs: Links (`/v1/federation/links*`), Mapping (versions approve/reject), SCIM (issue token, shown once and never stored; revoke), Provisioning (status, events, exceptions with resolve, reconcile, deprovision latency) (D1-1.F16, F17) |
| Support access drawer | Add the Review command `POST /v1/context/support/{id}/review` (D1-1.F05) |
| People drawer | Subject links `GET /v1/subjects/{id}/links` (D1-1.F14). `/v1/identities/*` gets no screen until IC-14 makes its merge equal to `/v1/subjects` merge; record that in `progress.md` |

**Tasks**
- [ ] **IC-FE-5 · API client + screens** for every route above, through `lib/api/envelope.ts`. Unit tests for the mappers (`vitest`).
- [ ] **IC-FE-6 · UI E2E** `tests/e2e/console/identity.spec.ts`. Cover:
  - invite → activate (public page) → login;
  - forgot-password identical answers;
  - password reset maker-checker (the requester never sees Approve);
  - support access review;
  - a federation connection version approved by a second admin;
  - SCIM token shown once;
  - one empty state and one "unavailable" state.

**Blocked and decisions:** S1-C1 (commercial-account-svc), S1-D1 (SAML), S1-D2 / S1-D3 (Security sign-offs).

**Hand-back:** S1 tasks above → S2 asyncapi, RUNBOOK, re-certify → S3 rebuild + `scripts/audit.sh` live → S4 `deployments/scripts/e2e-identity-context-deps.py` → S5 console (IC-FE-4) → S6 evidence in `progress.md`, re-score NP4 and source channel.

---

## D1-2 · tenant-entity-registry-svc (:8081, DB `tenant_entity_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | ORG-02, ORG-03; ZS-SVC-I-001 ORG-04/05/07/08/09; ZS-SVC-U-001 MDM-01…05; GOV-02 reads |
| Latest score | 47/48 (97.9%, 99.0% weighted), 19 Oct |
| New since the audit | ORG-2 (`90822800`), ORG-04 waves 1–3 (`a5a94683`, `69a99711`, `f5d4dc72`), ORG-5 waves 1–2 (`50b0db9e`, `ab2311c2`), all by 07Yashjais on main, merged 3–4 Nov. Not audited |
| Stages | S0 not started: there is no `progress.md` |
| Console | `/admin/tenants` legacy (not the v2.0 kit) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-2.F01 | Tenant create (maker-checker, onboarding key) and lifecycle: `POST /v1/tenants`, `GET /v1/tenants/{id}`, `POST …/lifecycle`, `POST /v1/tenants/{id}/commands/{command}` (activate, suspend, resume, terminate, retry/abandon provisioning) | 🟡 | Reconfirm (was ✅): Identity audit ORG-02 rows (19 Oct) | VC-1 |
| D1-2.F02 | Tenant reads and settings: `GET /v1/resolve-tenant`, `GET/POST /v1/tenants/{id}/defaults`, `POST …/home-region`, `GET …/residency-region`, `GET/POST …/host-bindings`, `GET …/lifecycle-history` | 🟡 | Reconfirm (was ✅): Identity audit ORG-02 rows | VC-1 |
| D1-2.F03 | Legal entity create and lifecycle: `POST /v1/entities`, `GET/PATCH /v1/entities/{id}`, `GET/POST …/status`, `POST …/activation`, `…/verification`, `…/legal-name`, `…/registered-office`, `…/profile-amendments` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows | VC-1 |
| D1-2.F04 | Entity reads: `GET /v1/entities/by-registry-number`, `…/{id}/versions`, `…/{id}/as-of`, `GET /v1/tenants/{id}/entities` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows | VC-1 |
| D1-2.F05 | Non-destructive merge/unmerge and registry-conflict quarantine: `POST /v1/entities/{id}/merge`, `…/unmerge`, `GET …/merge-records`, `GET /v1/registry-conflicts`, `POST …/{id}/resolution` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows | VC-1 |
| D1-2.F06 | Approval requests (maker-checker): `GET /v1/approval-requests`, `GET …/{id}`, `POST …/approve`, `POST …/reject` | 🟡 | Reconfirm (was ✅): Identity audit ORG-02/03 rows | VC-1 |
| D1-2.F07 | Purpose-scoped tax-identity bundles: `GET/POST /v1/entities/{id}/tax-identity-bundles`, `GET /v1/tax-identity-bundles/{id}`, `POST …/status` | 🟡 | Reconfirm (was ✅): Identity audit (RESTRICTED bundle authorized at the target entity) | VC-1 |
| D1-2.F08 | Hierarchies and jurisdiction assignments: `POST /v1/entity-hierarchies`, `DELETE …/{id}`, `GET /v1/entities/{id}/hierarchies`, `GET/POST …/jurisdictions`, `DELETE /v1/entity-jurisdictions/{id}` | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-2.F09 | Residency policies and regions: `POST /v1/residency-policies`, `GET …/{id}`, `GET /v1/residency-regions`, `GET …/{id}` | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-2.F10 | Workspaces: `POST /v1/workspaces`, `GET/PATCH …/{id}`, `POST …/status`, `GET /v1/tenants/{id}/workspaces` | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-2.F11 | Entity status read for other services: `GET /v1/entities/{id}/status` | 🟡 | Reconfirm (was ✅): Used by workflow, obligations, evidence-requirements | VC-1 |
| D1-2.F12 | ORG-2 operation-id parity (`CreateLegalEntity`, `DeactivateLegalEntity` on existing routes) | 🔵 | `90822800`; no audit | TER-1 |
| D1-2.F13 | ORG-3 (ORG-04) ownership and control: `POST /v1/ownership-relationships`, `GET …/{id}`, `POST …/approval`, `…/rejection`, `…/supersession`, `…/end`, `GET /v1/ownership-graph`, `GET /v1/entities/{id}/parents`, `…/group`, `…/control-chain` | 🔵 | `a5a94683`, `69a99711`, `f5d4dc72` (cycle and overlap rejection, outbox events); no audit | TER-1 |
| D1-2.F14 | ORG-5 (ORG-07 + MDM-01/05) parties and golden record: `GET/POST /v1/parties`, `GET …/{id}`, `POST …/facts:propose`, `…/identifiers`, `…/roles`, `…/roles/{id}/removal`, `GET …/merge-history`, `GET /v1/identifiers:find`, `POST /v1/resolution:match`, `POST /v1/resolution/{case}:merge`, `…:split`, `GET /v1/resolution-cases`, `GET …/{id}`, `POST …/decision` | 🔵 | `50b0db9e`, `ab2311c2`; `PARTY_*` actions registered (access-control 000044, 000045); no audit | TER-1 |
| D1-2.F15 | ORG-6 (ORG-08 + MDM-03) addresses and establishments | ❌ | Not built | ORG-6 |
| D1-2.F16 | ORG-7 (ORG-09 + MDM-03) registrations, licences, identifiers (`POST /v1/identifiers/{id}:verify`, expiry sweep) | ❌ | Not built | ORG-7 |
| D1-2.F17 | ORG-8 reference sets: `GET /v1/reference/{set}/{code}?as_of=`, FX (REF-03), books (REF-06), dimensions (REF-08), units (REF-09) | ❌ | Not built | ORG-8 |
| D1-2.F18 | ORG-9 master imports (`POST /v1/master-imports`, API-5 async) | ❌ | Not built | ORG-9 |
| D1-2.F19 | G-02 GOV-02 runtime reads (+ `deployments/gtrm` compiler commands) | ❌ | Not built | G-02 |
| D1-2.F20 | `fiscal_calendar_id` checked against fiscal-calendar-svc (S2-C1, G1-1) | ❌ | Format-checked only. fiscal-calendar-svc (a teammate's; `/v1/fiscal-calendar-versions/…`) exists and can be called read-only | TER-6 |
| D1-2.F21 | Consumers persist `object_id` / `object_version` from events (§9.2 gate 6, S2-C2) | 🟡 | Events carry both; consumers are other teams' services | Cross-team follow-up |
| D1-2.F22 | Restore drill with measured RPO/RTO (§9.2 gate 7, S2-C3) | ❌ | Not run | TER-4 |
| D1-2.F23 | Envelope verification enforced (compose `TENANT_REGISTRY_IDENTITY_VERIFY` default `warn`) | 🟡 | `deployments/docker-compose.yml` | X5 (cross-cutting) |
| D1-2.F24 | Ingress preserves the client `Host` header (NP3) | 🟡 | A rewriting proxy silently disables NP3; not checked on the GCP path | TER-5 |
| D1-2.F25 | Cursor paging on lists (API-4) | ❌ | Offset lists | TER-2 |
| D1-2.F26 | Address / LEI verification provider | ⛔ | Vendor not chosen | Decision |
| D1-2.F27 | Console `/admin/tenants` v2.0 with the 16 contract changes in `SPEC_DEVIATIONS.md` | ❌ | Legacy page | TER-FE-1…3 |

**Tasks**

- [ ] **TER-0 · Baseline and feature matrix (S0).** Create `services/tenant-entity-registry-svc/progress.md` with a matrix row per ORG-02, ORG-03, ORG-04, ORG-05, ORG-07 and MDM requirement. Run the full suite on a scratch Postgres (0 skip) and record counts.
- [ ] **TER-1 · Verify the teammate waves (ORG-2, ORG-04, ORG-5).** For each route in D1-2.F12–F14: openapi entry and parity, authorization at the object, Idempotency-Key, If-Match/ETag, outbox event in the same transaction, RLS + FORCE on new tables, R5 registration. Fix what fails; add `TestORG04_*` / `TestORG07_*` tests for the spec negative paths (cycle, overlap, self-approval, merge lineage, split).
  - Done when: each row is ✅ in `progress.md` with a test name.
- [ ] **ORG-6 · Addresses and establishments** (IMPLEMENTATION-PLAN §6 ORG-6). Local deterministic verifier; external provider BLOCKED. Move registered office onto the model, keeping the route.
- [ ] **ORG-7 · Registrations, licences, identifiers** (§6 ORG-7). Conflicting higher-authority source opens a registry conflict, never overwrites.
- [ ] **ORG-8 · Reference sets** (§6 ORG-8): FX, books, dimensions, units, `/v1/reference/{set}/{code}`. Exact decimal / rational arithmetic.
- [ ] **ORG-9 · Master imports** (§6 ORG-9), as an API-5 async operation.
- [ ] **G-02 · GOV-02 runtime reads** (§6 G-02).
- [ ] **TER-2 · API-4 cursor paging** on every list (shared cursor `identity-envelope/apicontract/cursor.go`). Tests per list: first, next, tampered 400, foreign-tenant 400, unknown filter 400.
- [ ] **TER-3 · Contract and docs (S2):** openapi, asyncapi (ownership and party events), RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman.
- [ ] **TER-4 · Restore drill:** restore the DB from backup on a scratch instance; record RPO/RTO in `progress.md`.
- [ ] **TER-6 · Fiscal-calendar check (S2-C1):** validate `fiscal_calendar_id` on entity create and amend against fiscal-calendar-svc (read-only client, fail closed on 503, typed problem on unknown id). Tests `TestS2C1_UnknownFiscalCalendarRefused`, `TestS2C1_RegistryDownFailsClosed`.
- [ ] **TER-5 · Live (S3/S4):** rebuild, migrate, `scripts/audit.sh`; new `deployments/scripts/e2e-tenant-entity-registry-deps.py` (identity-context host binding, authorization-svc decisions, jurisdiction-rules consumer, Kafka events, Host header through Traefik).

**Console (frontend)** — `app/admin/tenants` (legacy) · clients `lib/api/tenants.ts`, `lib/api/tenants-org.ts` call 42 of 79 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Platform › Tenants & Entities"; gold primary "New legal entity"; secondary "Resolve host", "New tenant" |
| KPIs (from new `GET /v1/registry/summary`) | Active tenants · Active legal entities · Approvals awaiting decision · Open registry conflicts · Open party resolution cases |
| Panels | Pipeline: entity lifecycle DRAFT → VERIFIED → ACTIVE → INACTIVE / DISSOLVED. Ordered list: control chain of the selected group (`…/control-chain`). Bar list: entities per residency region, with an amber callout for entities without a residency policy |
| Tabs | Tenants (F01, F02) · Legal entities (F03, F04, F11) · Approvals (F06) · Conflicts & merges (F05) · Ownership (F13) · Parties (F14) · Resolution cases (F14) · Tax identity (F07) · Hierarchies & jurisdictions (F08) · Residency (F09) · Workspaces (F10) |
| Drawer commands | Tenant commands (activate, suspend, resume, terminate initiate/complete, retry/abandon provisioning, home region, locale). Entity verify (evidence), activate, legal name, registered office, profile amendment, merge / unmerge. Approval approve / reject (hidden from the requester). Ownership propose / approve / reject / supersede / end. Party create (match-first: candidates shown before create), propose fact, link identifier, add / remove role. Resolution decide / merge / split. Tax bundle read with a purpose dialog |
| Footer | Required controls: maker-checker on termination, home region and legal name · evidence before VERIFIED · a purpose on every sensitive identifier read · merges never destroy records |

**Tasks**
- [ ] **TER-FE-1 · Summary route + API client.** Backend `GET /v1/registry/summary`: authorized, tested, in openapi. Client functions for every route in Appendix B D1-2.
- [ ] **TER-FE-2 · Page v2.0**, as specified above.
- [ ] **TER-FE-3 · UI E2E** `tests/e2e/console/tenants.spec.ts`. Cover:
  - entity DRAFT → VERIFIED → ACTIVE;
  - a legal-name change approved by a second user;
  - an ownership relationship proposed and approved;
  - a party created with the match shown first;
  - a tax-bundle read without a purpose refused;
  - empty and unavailable states.

**Blocked and decisions:** address/LEI provider; consumers persisting object versions (cross-team).

**Hand-back:** `deployments/scripts/e2e-tenant-entity-registry-deps.py` all PASS; matrix complete.

---

## D1-3 · configuration-feature-flag-svc (:8086, DB `configuration`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-AA-001 (CFG-01…03); GOV-11 |
| Latest score | 47/49 (96%, 98% weighted) |
| Since 2 Nov | Build restored, 000014 repaired, GetChange/GetEmergencyChange added, authorize before If-Match (`159ee017`, 283 pass on Postgres 16) |
| Stages | `progress.md` last updated 22 Sep; no feature matrix in the S0 format |
| Console | `/admin/settings` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-3.F01 | Config definitions registry: `POST /v1/config/definitions`, `GET …/{key}` (type, owner, scopes, fallback, safety class, regions) | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F02 | Immutable publish with `approval_reference` for S2/S3: `POST /v1/config/definitions/{key}/publish` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F03 | Overrides on five layers with allowlist and fixed precedence: `PUT /v1/config/overrides/{scope}` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F04 | Resolve with lineage and residency: `POST /v1/config/resolve` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F05 | Immutable snapshots: `GET /v1/config/snapshots/{id}` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F06 | Snapshot signing (decided S3-D2) | ❌ | Not built | CFG-3 |
| D1-3.F07 | Feature flags: `GET/POST /v1/flags`, `GET …/{key}`, `POST …/evaluate`, `…/release-plans`, `…/kill-switch`, `…/retire`, `…/removal` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F08 | Change sets C1/C2/C3: `POST /v1/config/changes`, `GET …/{id}`, `POST …/approve`, `…/activate`, `…/rollback` | 🟡 | Reconfirm (was ✅): Identity audit 3/9; `TestActivate_IfMatch`, `TestGetChange_ReturnsETag` (`159ee017`) | VC-1 |
| D1-3.F09 | Emergency changes: `POST /v1/emergency-changes`, `GET …/{id}`, `POST …/activate`, `…/retrospective` | 🟡 | S3-B1: anyone with the action may break glass; no live break-glass session is required | CFG-2 (with G-11) |
| D1-3.F10 | Runtime attestation and drift: `POST /v1/runtime/attest` | 🟡 | S3-B2: `X-Workload-Id` not taken from a verified identity | CFG-4 |
| D1-3.F11 | Plan eligibility on release plans (INV-02) | 🟡 | Trusts `X-Commercial-Plan`; the edge does not set it yet (gateway GW-8) | After GW-8 |
| D1-3.F12 | `secret://<env>/<path>` references resolved through the secret broker (S3-B3) | 🟡 | Nothing resolves them | CFG-5 |
| D1-3.F13 | 8 events through the transactional outbox | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | Kafka proof: X3; VC-1 |
| D1-3.F14 | Legacy key-value config: `GET/POST /v1/config`, `GET /v1/config/{key}` | 🔵 | Routed; not part of the CFG rows scored | CFG-1: confirm they go through the governed path or deprecate them (API-6) |
| D1-3.F15 | ETag/If-Match (API-3) | 🟡 | Present on definitions, change activate and emergency activate only. Missing on change approve/rollback, release plans, kill switch, flag retire/removal, overrides | CFG-1 |
| D1-3.F16 | G-11 feature policy and capability exposure | ❌ | Not built | G-11 |
| D1-3.F17 | Dev DB migrated and `scripts/audit.sh` live (S3-B4) | ❌ | Not run since 159ee017 | CFG-6 |
| D1-3.F18 | Cursor paging (API-4) | ❌ | — | CFG-1 |
| D1-3.F19 | Console page v2.0 (definitions, overrides, changes, emergency changes, flags, snapshots) | ❌ | `/admin/settings` legacy | CFG-FE-1…3 |

**Tasks**

- [ ] **CFG-0 · Baseline (S0):** feature matrix in `progress.md`; suite on scratch Postgres with counts; delete the untracked `internal/handler/handler.go.bak`, `.tmp`, `.tmp2`.
- [ ] **CFG-1 · API-3 and API-4 completion:** `If-Match` (428/412/2xx tests each) on change approve/rollback, release plans, kill switch, flag retire/removal, overrides; ETag on every GET; cursor paging on lists. Decide the legacy `/v1/config` routes: route them through the governed path or mark them deprecated (Deprecation/Sunset).
- [ ] **CFG-2 · S3-B1 emergency grant:** a dedicated emergency action in access-control (R5) and a required live break-glass context from authorization-svc on `/v1/emergency-changes`. Tests: `TestCFG2_EmergencyWithoutBreakGlassRefused`.
- [ ] **CFG-3 · S3-D2 snapshot signing:** sign snapshots (local key until SEC-1, then key-management-svc); verify on read. Tests: tampered snapshot fails verification.
- [ ] **CFG-4 · S3-B2 attestation identity:** take the workload id from the mTLS client certificate; refuse a header-only id when mTLS is required.
- [ ] **CFG-5 · S3-B3 `secret://` resolution** through `POST /v1/secrets/broker` (secret-vault-integration-svc), never logged.
- [ ] **G-11 · Feature policy** (IMPLEMENTATION-PLAN §6 G-11).
- [ ] **CFG-6 · Live:** migrate the dev DB, rebuild, `scripts/audit.sh` live; new `deployments/scripts/e2e-configuration-feature-flag-deps.py`.

**Console (frontend)** — `app/admin/settings` (legacy; keep the route and sidebar label) · clients `lib/api/configuration.ts`, `lib/api/config.ts` call 6 of 26 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Platform › Configuration & Feature Flags"; gold primary "Propose change"; secondary "Resolve a key", "New flag" |
| KPIs (from new `GET /v1/config/summary`) | Published definitions · Changes awaiting approval · Emergency changes active (retrospective due) · Flags with kill switch on · Attestation drift |
| Panels | Pipeline: Proposed → Validated → Approved → Activated → Verified. Ordered list: override precedence (environment → service → tenant → org unit → user), with the override count per layer. Bar list: rollout % per release plan, with an amber callout for kill switches on and emergency changes without a retrospective |
| Tabs | Definitions (F01, F02) · Overrides (F03) · Resolve & snapshots (F04, F05) · Change sets (F08) · Emergency changes (F09) · Feature flags (F07, F11) · Attestations (F10) · Legacy keys (F14, read-only, deprecation notice) |
| Drawer commands | Publish (approval reference for S2/S3). Set override. Change propose / approve (the proposer never sees Approve) / activate (If-Match, expected before-hash) / rollback. Emergency activate (needs a break-glass session after CFG-2) + retrospective. Flag release plan / kill switch / retire / removal |
| Footer | Required controls: the proposer never approves · an approval reference for S2/S3 keys · emergency changes revert at expiry and need a retrospective · a reason on every change |

**Tasks**
- [ ] **CFG-FE-1 · Summary route + API client** for every route in Appendix B D1-3.
- [ ] **CFG-FE-2 · Page v2.0.**
- [ ] **CFG-FE-3 · UI E2E** `tests/e2e/console/configuration.spec.ts`. Cover:
  - a change proposed by one user and approved by a second;
  - activation with a stale If-Match shows the reload prompt;
  - a kill switch toggled and reverted;
  - an emergency change needs a retrospective;
  - empty and unavailable states.

**Blocked and decisions:** commercial plan header waits for GW-8 (S3-D1).

---

## D1-4 · secret-vault-integration-svc (:8087, DB `secret_vault`)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §3.1, §9, §13, §14 (tokenization) |
| Latest score | 18/21 (86%, 93% weighted) |
| Stages | `progress.md` last updated 21 Sep |
| Console | `/admin/secrets` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-4.F01 | Secret policies and versions: `GET/POST /v1/secret-policies`, `GET/POST …/{id}/versions`, `POST …/versions/{v}/activate` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F02 | Material seeding, manual rotation, rotation sweeper: `POST …/{id}/material`, `POST …/{id}/rotate`, sweeper ticker | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F03 | Broker with signed lease token (`ltk:v3`): `POST /v1/secrets/broker` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F04 | Leases: `GET /v1/secrets/leases`, `GET …/{id}`, `POST …/verify`, `POST …/revoke`; audit read `GET /v1/secrets/audit` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F05 | Sign and use with a lease: `POST /v1/secrets/sign`, `POST /v1/secrets/use` | 🔵 | Routed; not named in the 2 Nov status | SV-1 |
| D1-4.F06 | Break-glass emergency retrieval: `POST …/{id}/emergency-retrieval` (exception for the path, approver ≠ retriever, evidence before release) | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F07 | Shared-secret exception register: `GET/POST /v1/shared-secret-exceptions`, `POST …/{id}/revoke` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F08 | Inbound mTLS with identity check; KMS envelope encryption (Vault Transit / Cloud KMS); production refuses to boot without them | 🟡 | Reconfirm (was ✅): Identity audit 4/9 (code); local encrypted-file backend until infrastructure exists | Infra: D1-4.F12; VC-1 |
| D1-4.F09 | Workload identity bound to environment, region and audience (O-1) | 🟡 | Checks built but off | SV-2 (after SEC-6) |
| D1-4.F10 | Non-exportable workload credentials (O-2) | 🟡 | The issuer generates the mTLS private key and sends it | SV-2 (after SEC-6 CSR signing) |
| D1-4.F11 | Dynamic per-lease credentials (O-3, decided S4-D1) | ⛔ | Needs Vault secrets engines | Vault infrastructure |
| D1-4.F12 | Production key custody: Vault Transit or Cloud KMS key; one-time migration with `VAULT_MIGRATE_LEGACY_KEY_FILE` | ⛔ | Infrastructure | Platform |
| D1-4.F13 | Tokenization, masking, pseudonymization (SEC-4) | ❌ | Not built; after SEC-1 | SEC-4 |
| D1-4.F14 | ETag/If-Match on SecretPolicyVersion, SecretLease, SharedSecretException (API-3) | 🟡 | IMPLEMENTATION-PROGRESS: "implemented but tests need update"; suite green on 6 Nov | SV-1 |
| D1-4.F15 | Gitleaks findings (6 repo-wide) rotated or allowlisted | ❌ | Open | SV-3 |
| D1-4.F16 | Gateway client certificate for the edge → service hop (S4-B1) | ⛔ | Traefik `serversTransport` certificates (infrastructure) | Platform |
| D1-4.F17 | Cursor paging (API-4); console v2.0 | ❌ | — | SV-1, SV-FE-1…3 |

**Tasks**

- [ ] **SV-0 · Baseline (S0):** matrix in `progress.md`; suite with counts.
- [ ] **SV-1 · Verify and finish:** sign/use routes (authorization, lease binding, evidence, openapi); API-3 tests 428/412/2xx per aggregate; API-4 on leases, policies, exceptions, audit.
- [ ] **SV-2 · O-1/O-2 (after SEC-6 adds `spiffe://zoiko/<env>/<service>` URI SANs and CSR signing):** set `MTLS_REQUIRED_URI_PREFIX`, `MTLS_MAX_CLIENT_CERT_LIFETIME=24h`, mandatory in production.
- [ ] **SEC-4 · Tokenization** (after SEC-1).
- [ ] **SV-3 · Gitleaks:** rotate or allowlist each finding with a reason.
- [ ] **SV-4 · Live:** rebuild, `scripts/audit.sh`, `deployments/scripts/e2e-secret-vault-integration-deps.py` (broker → verify → revoke; emergency retrieval with a second approver; rotation sweep).

**Console (frontend)** — `app/admin/secrets` (legacy) · client `lib/api/secret-vault.ts` calls 11 of 16 screen-relevant routes (Appendix B; broker, use and sign are workload APIs with no screen). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Security › Secrets & Leases"; gold primary "New secret policy"; secondary "Register exception" |
| KPIs (from new `GET /v1/secrets/summary`) | Active policies · Active leases · Rotations due in 7 days · Emergency retrievals (30 d) · Shared-secret exceptions |
| Panels | Pipeline: lease Issued → Verified → Used → Expired / Revoked. Ordered list: rotation queue (next due first). Bar list: policies by backend and rotation compliance, with an amber callout for overdue rotation and for the local (non-KMS) backend |
| Tabs | Policies & versions (F01, F02) · Leases (F04) · Emergency retrievals (F06) · Shared-secret exceptions (F07) · Audit (F04) |
| Drawer commands | New version / activate (If-Match). Rotate now. Seed material (write-only field, never echoed). Revoke lease. Emergency retrieval: the path must have an exception; approver ≠ retriever; reason required. The material is shown once, masked until revealed, never cached, and cleared when the drawer closes. Exception create / revoke |
| Footer | Required controls: material is never displayed except one emergency retrieval · approver ≠ retriever · an exception for every shared path · every read audited |

**Tasks**
- [ ] **SV-FE-1 · Summary route + API client** for every route in Appendix B D1-4.
- [ ] **SV-FE-2 · Page v2.0.**
- [ ] **SV-FE-3 · UI E2E** `tests/e2e/console/secrets.spec.ts`. Cover:
  - policy → version → activate;
  - rotate;
  - lease revoke;
  - emergency retrieval refused for the same user and allowed with a second approver, material cleared on close;
  - empty and unavailable states.

**Blocked:** Vault / Cloud KMS, dynamic credentials, Traefik client certificates.

---

## D1-5 · gateway-auth-svc (:8092, no database)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-M-001 INT-01 §4.1; Security §8; GOV-01 ingress; AU-01/04/12/23/27 |
| Latest score | 22/22 Security §8 + GOV-01 ingress rows (not re-scored) |
| Status | AUTH-PLANE GW-0…GW-6 ran 4 Nov; verifier result **hold**: findings G-1…G-4 (Identity audit → "gateway-auth-svc status, 4 November 2026") |
| Suite | 9 packages ok (4 Nov and 6 Nov) |
| Console | `/admin/gateway` v2.0 diagnostic page (deviation recorded 4 Nov) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-5.F01 | ForwardAuth `/verify`: envelope against identity-context JWKS, issuer and audience pinned, incomplete claims refused | 🟡 | Reconfirm (was ✅): Identity audit gateway rows (22/22) | VC-1 |
| D1-5.F02 | Hostname ↔ tenant binding (NP-2), tenant-context resolution, fail closed | 🟡 | Reconfirm (was ✅): Identity audit gateway rows | VC-1 |
| D1-5.F03 | Sets 8 server-resolved headers, strips forged identity headers (split policy) | 🟡 | Reconfirm (was ✅): Identity audit gateway rows | VC-1 |
| D1-5.F04 | CARTA risk check on every request; modes fail closed on writes, open on reads (S5-D2, AU-23); SIEM denial stream; `X-Auth-Denial-Reason` | 🟡 | Reconfirm (was ✅): AU-23; Identity audit | VC-1 |
| D1-5.F05 | Assurance headers: `X-Assurance-Level`, `X-Authn-Age-Seconds`, verified source channel, support-context id | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-5.F06 | mTLS to identity-context and tenant-entity-registry (SEC-INV-05) | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-5.F07 | INT-01 minimum acceptance tests and residency gate | 🟡 | Reconfirm (was ✅): `TestINT01_*` (8); `d3cfc988`, `aebdf04e` | VC-1 |
| D1-5.F08 | Trusted-hop client IP (S5-D3): `GATEWAY_TRUSTED_PROXY_CIDRS`, `_DEPTH` | 🟡 | `TestClientIP_*` pass; not proven live; production CIDRs are platform's decision | GW-7 |
| D1-5.F09 | Edge login throttling (AU-04) on `/v1/authenticate*`, `/v1/account/*`, `/oauth2/login*`, `/oauth2/token` | 🟡 | `TestAU04_*` pass; the live probe (25 failed logins → 429) was not run; routing identity-context's auth routes through Traefik is unverified | GW-7 |
| D1-5.F10 | Edge-set `X-Commercial-Plan` / `X-Org-Unit-Id` (S3-D1) | 🟡 | `internal/commercial` client with `TestClient_*`; the S3D1 tests named in progress.md do not exist; live half needs commercial-account-svc | GW-8 |
| D1-5.F11 | Admin read surface on `:8192` (token file): `GET /internal/v1/gateway/decisions/{id}`, `/denied`, `/usage/{tenantID}`, `/rate-limit/{scope}/{key}`, `/routes/versions` | 🟡 | The three new reads (all-outcome decisions, all-tenant usage, policy) were not built; compose mounts the `gateway_admin_token` **volume** where the code expects a token **file** | GW-9 |
| D1-5.F12 | IAM-1 / AU-12 per-service audience: `/verify` exchanges the envelope for the route's audience; unknown route fails closed | ❌ | G-1: the exchanger is only wired into the gateway's own carta/commercial calls; no route → audience map; the four named tests do not exist; the exchange client is registered nowhere | GW-5 |
| D1-5.F13 | Dependency E2E `deployments/scripts/e2e-gateway-auth-deps.py` | 🟡 | G-2: raises `TypeError` at check 5; wrong OAuth client; wrong commercial URL; three checks pass without testing; exits 1 not the failure count | GW-6a |
| D1-5.F14 | Contract and docs: SPEC_DEVIATIONS (S5-D1 placement, AU-27, S5-B2), re-certified RELEASE_CERTIFICATE, live `scripts/audit.sh` | ❌ | G-3: SPEC_DEVIATIONS does not exist; no re-certification; no live run | GW-6b |
| D1-5.F15 | Feature matrix in `progress.md` matches reality | 🟡 | G-4: rows INT01-07, S3-D1, AU-12, GW-5, S5-B2 claim tests or probes that do not exist | GW-6b |
| D1-5.F16 | Production internet-edge controls: DDoS, WAF, bot/abuse, TLS policy, request-size limits (S5-D1) | 🧭 | Platform decision (e.g. Cloud Armor in front of the GCP LB) | Decision |
| D1-5.F17 | Console → service calls go through the edge, or every service verifies the envelope (S5-B2, R-3) | 🟡 | About 100 services still trust `X-Principal-Id` on direct calls | X5 (cross-cutting) |
| D1-5.F18 | Per-service mTLS enrolment (M-01) and Traefik `serversTransport` client certificates | ⛔ | Infrastructure (SEC-6) | Platform |
| D1-5.F19 | Console `/admin/gateway` v2.0 diagnostic page | 🟡 | Reconfirm (was ✅): progress.md GW-FE-2; deviation note in AUTH-PLANE plan | VC-1 |
| D1-5.F20 | Sender-constrained tokens per client class (AU-27) | 🧭 | Security decision (D-7) | Decision |

**Tasks** (order from the 4 Nov status: G-2 → G-1 → G-3 → G-4)

- [ ] **GW-6a · Rewrite the E2E (G-2)** on the pattern of `e2e-authorization-deps.py`: password login with the confidential client → resolve → envelope; `skip()` never counts as a pass; exit code = failures; one check per edge: identity-context JWKS over mTLS, invalidate → `/verify` 401 within TTL, step-up → `X-Assurance-Level: MFA`; registry binding and mismatch 403; CARTA STEP_UP_MFA → 401, carta stopped → write 503 / read 200; SIEM denial streamed; Traefik replaces every forged header; authorization-svc receives the age header; throttle 429; audience (after GW-5). Undo writes.
- [ ] **GW-5 · IAM-1 per-service audience (G-1)**, as specified in Part B4 (GW-5): `GATEWAY_AUDIENCE_ROUTES` generated by `deployments/scripts/gen-gateway-routes.mjs`; cache per (session, audience) until `exp − 30 s`; unknown protected route fails closed; exchange failure → 503, never pass-through; count failures in a metric. Register the gateway's OAuth client in identity-context (`deployments/identity/oidc-clients.local.json`, `OIDC_EXCHANGE_AUDIENCES`), secret from a compose secret.
  - Tests: `TestVerify_ExchangesForRouteAudience`, `TestVerify_UnknownRouteFailsClosed`, `TestVerify_ExchangeFailureIs503NotPassThrough`, identity-envelope `TestIAM01_EnvelopeForPolicyRefusedByWorkflow`.
  - Rollout: once the E2E shows each gated service receives its own audience, set `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` per service.
- [ ] **GW-7 · Live proof for S5-D3 and AU-04:** route identity-context's public auth routes through Traefik; 25 rapid failed logins → 429 before the 25th; an untrusted peer's `X-Forwarded-For` ignored. Record output in `progress.md`.
- [ ] **GW-8 · S3-D1 tests:** `TestS3D1_ForgedPlanHeaderReplaced`, `TestS3D1_PlanUnavailableOmitsHeaderNeverGuesses`, `TestS3D1_OrgUnitFromEnvelopeOnly`; GTRM `gtrm-edge-strip` and compose `authResponseHeaders` updated; live half BLOCKED until commercial-account-svc runs.
- [ ] **GW-9 · Admin surface:** fix the compose token mount (an init container writes a ≥24-char token file into the volume, `chown 65532`, `chmod 0400`; mount the volume as a directory; `GATEWAY_ADMIN_TOKEN_FILE=/run/secrets/gateway_admin/token`). Build or formally drop the three reads in `RUNBOOK.md` (the console does not use them).
- [ ] **GW-6b · Contract and docs (G-3, G-4):** `SPEC_DEVIATIONS.md` (S5-D1 placement, AU-27, S5-B2, GW-FE deviation); re-certify `RELEASE_CERTIFICATE.md`; correct every matrix row to its real status; rebuild image; `scripts/audit.sh` live; run the new E2E and keep its output.

**Console (frontend)** — `app/admin/gateway` (v2.0 diagnostic page, 4 Nov; the per-replica admin surface is deliberately not read, see D1-5.F11). No API client gaps.

**Tasks**
- [ ] **GW-FE-3 · UI E2E** `tests/e2e/console/gateway.spec.ts` against the live gateway after GW-5…GW-9. Cover:
  - "Verify my session" shows the assurance level and authn age;
  - after a step-up the page shows MFA;
  - signing keys listed;
  - with the gateway stopped, the page shows "unavailable".

**Blocked and decisions:** S5-D1, AU-27, M-01 / S4-B1, commercial-account-svc live.

**Hand-back:** then continue the auth plane with access-control-svc (D1-9).

---

## D1-6 · search-indexer-svc (:8096, DB `search_indexer`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-AB-001 (ESR-001…ESR-020); RAG contract §10.2 |
| Latest score | 47/49 (96%, 98% weighted) |
| Stages | `progress.md` last updated 30 Sep |
| Console | `/admin/search` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-6.F01 | Governed lexical search: `POST /v1/search` (mandatory filters, query safety, field policy, bounded window, signed cursor, freshness) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F02 | Semantic / hybrid search: `POST /v1/search/semantic` and the six vector-index controls | 🟡 | Verified with a stub; answers ESR-019 because no embedding provider is configured | SI-1 (blocked on OD-10) |
| D1-6.F03 | Retrieve with per-candidate re-authorization: `POST /v1/retrieve`; classes R0, R1, R3 (export) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F04 | R2 source hydration | 🟡 | Wired for `obligation` only | SI-2 (OD-05) |
| D1-6.F05 | Index contracts: `GET/POST /v1/index-contracts`, `GET …/{id}`, `POST …/{id}/state` (immutable after publication) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F06 | Index generations: `GET/POST /v1/index-generations`, `POST …/{id}/state`, `GET/POST …/{id}/retrieval-evaluations` (backfill, ledger validation, activate, rebuild, recall@k certification) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F07 | Restrictions and erasure with epochs: `GET/POST /v1/restrictions` | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F08 | Checkpoints and lag: `GET /v1/checkpoints`; STALE/UNKNOWN freshness gates (ESR-012); restriction-failure exclusion (ESR-018) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F09 | Search exports: `POST /v1/search-exports` (purpose, separate permission, evidence); facet privacy; snippet policy | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F10 | Search evidence, sources and scopes: `GET /v1/search-evidence`, `GET/POST /v1/search-sources`, `GET /v1/scopes` | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F11 | 8 events, 20 reason codes, Idempotency-Key, ETag/If-Match on contracts and generations | 🟡 | Reconfirm (was ✅): Identity audit 6/9; IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D1-6.F12 | Replay API in each owning domain service, so rebuilds go past Kafka retention (S6-C2) | ⛔ | Owning services are other teams' | Cross-team |
| D1-6.F13 | Grant-dependent `scripts/audit.sh` sections 7–12 and 16 against a real, migrated authorization-svc | ❌ | Not run | SI-3 |
| D1-6.F14 | RAG contract (§10.2) | 🧭 | Mostly owned by AI governance | Decision |
| D1-6.F15 | Cursor paging on list routes (API-4): checkpoints, contracts, generations, restrictions, sources, evidence | ❌ | `/v1/search` already uses a signed cursor; the admin lists do not | SI-3 |
| D1-6.F16 | Console v2.0 (Search, Contracts, Generations, Restrictions, Exports, Evidence) | ❌ | Legacy page | SI-FE-1…3 |

**Tasks**

- [ ] **SI-0 · Baseline (S0):** matrix in `progress.md` from ESR-001…ESR-020; suite with counts.
- [ ] **SI-1 · Embedding provider (OD-10):** when chosen, set `EMBEDDING_PROVIDER_URL`, certify recall with a retrieval evaluation, record it. Until then the row stays 🟡 with ESR-019.
- [ ] **SI-2 · R2 scopes (OD-05):** set `SOURCE_SERVICE_URL_<TYPE>` per decided scope; a test per scope that hydration re-authorizes.
- [ ] **SI-3 · API-4 and live:** cursor paging on admin lists; rebuild; run `scripts/audit.sh` sections 7–12 and 16 against migrated authorization-svc; new `deployments/scripts/e2e-search-indexer-deps.py`.

**Console (frontend)** — `app/admin/search` (legacy) · clients `lib/api/search.ts`, `lib/api/search-indexer.ts` call 16 of 20 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Platform › Search & Index"; gold primary "New index contract"; secondary "Run a search" |
| KPIs (from `GET /v1/checkpoints` and lists, or a new `GET /v1/search/summary`) | Active generations · Scopes STALE or UNKNOWN · Max lag (s) · Active restrictions · Exports (30 d) |
| Panels | Pipeline: generation Building → Backfilled → Validated → Active → Retired. Ordered list: retrieval classes R0–R3 and what each returns. Bar list: freshness per scope, with an amber callout for STALE/UNKNOWN and for semantic search unavailable (ESR-019) |
| Tabs | Search (F01, F02, F03) · Index contracts (F05) · Generations (F06) · Restrictions (F07) · Freshness (F08) · Exports (F09) · Evidence & sources (F10) |
| Drawer commands | Contract state. Generation state (activate needs a passing retrieval evaluation). Run evaluation. Add restriction. Request export (purpose + its own permission) |
| Footer | Required controls: every result re-authorized · restricted records never return · exports need a purpose and their own permission |

**Tasks**
- [ ] **SI-FE-1 · API client** for every route in Appendix B D1-6 (+ summary if built).
- [ ] **SI-FE-2 · Page v2.0.**
- [ ] **SI-FE-3 · UI E2E** `tests/e2e/console/search.spec.ts`. Cover:
  - a search with mandatory filters;
  - a restricted record disappears;
  - an export without a purpose is refused;
  - semantic search shows its honest "unavailable" answer;
  - empty and unavailable states.

**Blocked and decisions:** OD-10 embedding provider; OD-05 R2 scopes; S6-C2 replay APIs; RAG contract.

---

## D1-7 · notification-svc (:8133, DB `notification`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-Y-001 NCD-01…NCD-05; ZS-COMMS-EMAIL-001 |
| Latest score | 35/37 (95%, 97% weighted); 110-row NCD matrix 94% weighted |
| Since 2 Nov | `560b6212` (4 Nov): record_version wired, 000034 repaired, authorize before If-Match. The 25 failing tests now pass (sweep 6 Nov: 25 packages ok) |
| Stages | `progress.md` last updated 6 Oct |
| Console | `/admin/notifications` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-7.F01 | NCD-01 communication intents: `POST /v1/communication-intents`, `GET …/{id}`, `POST …/versions`, `…/activate` (second principal), `…/retire`, `GET …/{id}/templates`, `GET /v1/intents/{id}/effective` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F02 | NCD-01 templates: `POST /v1/templates`, `GET /v1/templates/{id}`, `POST …/validate` (15 checks), `…/approve`, `…/reject` (approver ≠ author), `…/publish`, `POST /v1/templates/{id}/retire`; render preview `POST /v1/render-previews` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F03 | NCD-02 recipient resolution: `POST /v1/recipient-resolution`; endpoint exceptions `POST /v1/recipient-endpoint-exceptions`; `POST /v1/revalidate`; open exceptions queue `GET /v1/exceptions` (`?open=false` for closed) | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F04 | NCD-02 channel decision: `POST /v1/channel-decision` | 🟡 | The privacy/marketing permission is supplied by the caller (G-1) | NT-2 |
| D1-7.F05 | Suppressions and preferences: `GET/POST /v1/suppressions`, `POST …/{id}/lift`, `POST /v1/preferences`, `GET /v1/preferences/me` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F06 | NCD-03 delivery: `POST /v1/communications`, `GET …/{id}`, `POST …/prepare`, `…/dispatch`, `…/cancel`, `…/resend`, `…/misdelivery`, `…/attempts/{id}/resolve`; provider bindings `GET /v1/provider-bindings`, `POST …/{id}/circuit`; quotas, priority, storm dedupe | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F07 | Governed bulk sends: `POST /v1/bulk-sends`, `POST …/{id}/dispatch`; approvals `GET /v1/approvals/{id}`, `POST …/approve`, `…/reject` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F08 | NCD-04 evidence and provider events: `GET /v1/evidence/{id}`, `POST /v1/provider-events/{binding}`, `GET /v1/reputation`, `POST /v1/streams/{stream}` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F09 | NCD-05 regulated notices: `POST /v1/regulated-notices`, `GET …/{id}`, `POST …/manual-evidence`; acknowledgments `POST /v1/acknowledgments` (bound to content hash); deadline escalation | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F10 | Record declaration to the records service: `POST /v1/regulated-notices/{id}/record-declaration` | 🟡 | Arrives by callback; legal hold sits with retention-registry-svc (S7-D3) | NT-3 |
| D1-7.F11 | Legacy free-text send path: `GET/POST /v1/notifications`, `POST …/events/ingest`, `GET …/{id}`, `…/attempts`, `…/delivery-status`, `…/evidence`, `POST …/read`, `…/cancel`, `…/resend`, `…/resolve-delivery-outcome`, `GET …/unread-count`, `GET …/templates`, `GET …/intents/{id}`, `GET/POST …/suppression`, `POST …/unsubscribe` (RFC 8058), `POST …/webhooks/{provider}` | 🟡 | Still open, so INV-01 is not met | NT-5 (G-6) |
| D1-7.F12 | Channels | 🟡 | EMAIL and IN_APP only; SMS and push missing | ⛔ OD-03 / OD-04 |
| D1-7.F13 | Email templates: 128 templates + 32 variants, register parity, trigger routing (EML-1…EML-6) | ❌ | Not built | EML-1…EML-6 |
| D1-7.F14 | ETag/If-Match on Notification, Template, TemplateVersion (API-3) | 🟡 | Reconfirm (was ✅): `560b6212`; sweep 6 Nov green | VC-1 |
| D1-7.F15 | Document templates: `POST /v1/document-templates`, `GET …/{id}`, `…/compare`, `…/locales`, `…/published`, `POST …/retire`, `…/versions`, `POST …/versions/{v}/validate`, `…/preview`, `…/approve`, `…/publish` | 🔵 | Routed in `internal/handler/handler.go`; not scored by the NCD audit | NT-1 |
| D1-7.F16 | Outbound webhooks (INT-04): `/v1/webhook-subscriptions` (+ submit, approve, reject, suspend, resume, revoke, rotate-signing-key, health, log, deliveries), `GET /v1/webhook-deliveries/{id}`, `POST …/replay`, `GET /v1/webhook-dead-letters` | 🔵 | `7f563f9f` (ZS-SVC-M-001 Wave 5, 07Yashjais); not audited in this domain | NT-1 |
| D1-7.F17 | Action links: `GET /v1/notifications/actions/{token}`, `POST …/execute` | 🔵 | `internal/actionlink/gateway.go`; signed, expiring links and S1+ sign-in (NP-33, S7-D2) not verified | NT-1, NT-6 |
| D1-7.F18 | Secrets (provider, callback, webhook, unsubscribe, DKIM) leased from secret-vault-integration-svc (G-3) | ❌ | Environment variables today | NT-4 |
| D1-7.F19 | Actions `TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS`, `NOTIFICATION_RESOLVE_OUTCOME` granted by migration (G-9) | ❌ | Only the demo seed grants them | AC-2 (D1-9) |
| D1-7.F20 | Deadline clocks from obligations-svc (G-5) | ❌ | Caller-supplied deadlines | After WF-3 (D2-4) |
| D1-7.F21 | MDM party source for external contacts (G-4) | 🔵 | ORG-5 parties now exist in tenant-entity-registry (unverified, D1-2.F14) | NT-7 |
| D1-7.F22 | Estate control so only notification-svc reaches provider credentials (NP-01, TC-01) | ❌ | Not built | NT-4 |
| D1-7.F23 | Cursor paging (API-4) on suppressions, notifications, webhook lists, exceptions | ❌ | — | NT-1 |
| D1-7.F24 | Console v2.0 (intents, template approval, suppressions, preferences, regulated notices, evidence, exceptions, reputation, webhooks) and a recipient acknowledgment page (G-7, G-8) | ❌ | Legacy page; send form still on `/v1/notifications` | NT-FE-1…3 |

**Tasks**

- [ ] **NT-0 · Baseline (S0):** matrix in `progress.md` (NCD 110 rows + D1-7); delete the untracked `handlercheck.bin`; suite with counts on scratch Postgres.
- [ ] **NT-1 · Verify the unscored surfaces** (document templates, outbound webhooks, action links): authorization at the object, maker-checker where a template is approved, Idempotency-Key, If-Match/ETag, openapi parity, events; add API-4 to lists. Tests named `TestNT1_*`.
- [ ] **NT-2 · G-1 channel decision:** register the service's purposes in privacy-purpose-registry-svc; call `POST /v1/privacy/decisions` (privacy-decision-svc) instead of trusting the caller; fail closed on marketing when privacy is down. Take the legal basis for regulated notices from policy-svc (S7-C2, G1-1).
- [ ] **NT-3 · G-2 records:** declare regulated-notice evidence packages as records; legal hold through retention-registry-svc (S7-D3).
- [ ] **NT-4 · G-3 secrets + NP-01:** lease every provider/callback/webhook/unsubscribe/DKIM secret from the broker; no secret in env outside local.
- [ ] **NT-5 · G-6 / INV-01 close the legacy path:** a product decision per workflow on who is notified by which template; move callers to `POST /v1/communications`; mark `/v1/notifications` send and `/events/ingest` deprecated (API-6), then close them.
- [ ] **NT-6 · Secure links (NP-33, S7-D2):** signed, expiring links; S1+ content needs a signed-in session. Tests: expired link 410, tampered link 400, S1 content without session 401.
- [ ] **NT-7 · G-4 external contacts** from tenant-entity-registry parties (after TER-1 verifies ORG-5).
- [ ] **EML-1…EML-6 · Email catalogue** (IMPLEMENTATION-PLAN §6). EML-4 after G-08.
- [ ] **NCD-1 · Close the 7 partial and 3 unmet NCD matrix rows** where their dependency now exists.
- [ ] **NT-8 · Live:** rebuild, `scripts/audit.sh`, new `deployments/scripts/e2e-notification-deps.py` (identity endpoints, privacy decision, secret broker, Kafka trigger → communication, provider sandbox, bounce ingestion).

**Console (frontend)** — `app/admin/notifications` (legacy) · client `lib/api/notifications.ts` calls 6 of 91 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. This is the largest console gap in Domain 1.

| Part | Content |
|---|---|
| Header | "Communications › Notifications"; gold primary "New communication"; secondary "New intent", "New template" |
| KPIs (from new `GET /v1/communications/summary`) | Communications today · Delivery UNKNOWN awaiting reconcile · Templates awaiting approval · Regulated notices awaiting acknowledgement · Open exceptions |
| Panels | Pipeline: Prepared → Dispatched → Delivered / Bounced / Unknown → Reconciled. Ordered list: channel fallback order with each provider's circuit state chip. Bar list: deliverability per channel, with an amber callout for open circuits and complaint spikes |
| Tabs | Communications (F06) · Intents (F01) · Templates (F02) · Document templates (F15) · Recipients & preferences (F03, F05) · Suppressions (F05) · Bulk sends & approvals (F07) · Regulated notices (F09, F10) · Evidence & reputation (F08) · Exceptions (F03) · Webhooks (F16) · Legacy notifications (F11, read-only, deprecation notice) |
| Drawer commands | Communication prepare / dispatch / cancel / resend (reason) / misdelivery / resolve attempt. Intent version / activate (second principal) / retire. Template validate / approve / reject (approver ≠ author) / publish / retire. Document template version validate / preview / approve / publish / retire. Suppression add / lift. Bulk send approve (≠ requester) / dispatch. Notice manual evidence / record declaration. Webhook subscription submit / approve (≠ submitter) / suspend / resume / revoke / rotate signing key; delivery replay; provider circuit |
| Other pages | Account › Notification preferences (`GET /v1/preferences/me`, `POST /v1/preferences`). Public acknowledgment page `/ack/[token]` → `POST /v1/acknowledgments` (signed link, NT-6). The bell keeps the legacy read until NT-5 |
| Footer | Required controls: no free-text sends (intent + template only) · approver ≠ author · a reason on every resend · suppressions lifted only through governance |

**Tasks**
- [ ] **NT-FE-1 · Summary route + API client** for every route in Appendix B D1-7.
- [ ] **NT-FE-2 · Page v2.0** + preferences page + acknowledgment page; the send form uses `POST /v1/communications`.
- [ ] **NT-FE-3 · UI E2E** `tests/e2e/console/notifications.spec.ts`. Cover:
  - intent → template validate → approve by a second user → publish;
  - communication prepare → dispatch → delivered evidence;
  - a suppression blocks a send;
  - a regulated notice acknowledged through the public page;
  - a webhook subscription approved by a second user;
  - empty and unavailable states.

**Blocked and decisions:** SMS/push providers (OD-03, OD-04); sender DNS; per-workflow notification decision (G-6).

---

## D1-8 · delegated-authority-svc (:8136, DB `delegated_authority`)

**Facts**

| Item | Value |
|---|---|
| Spec | ORG-06 (delegation half) |
| Latest score | 23/23 (100%) |
| Stages | `progress.md` last updated 6 Oct |
| Console | `/admin/delegations` legacy (last changed 18 Aug) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-8.F01 | Create a delegation: `POST /v1/delegations` (delegator bound to caller and must hold the action; SoD via `/v1/sod/validate`; self-dealing refused; on-behalf grant PROPOSED until an independent approver activates) | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F02 | Lifecycle: `POST /v1/delegations/{id}/activate`, `…/revoke`, `…/extend`, `…/suspend`, `…/resume`; cross-tenant expiry sweeper | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F03 | Reads: `GET /v1/delegations`, `GET …/{id}`, effective delegations as of a date, `GET /v1/delegations/explain` (ExplainDelegationChain) | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F04 | AuthorityLimit (monetary / quantity ceiling) on a delegation; overlap prevention (EXCLUDE); Idempotency-Key; `409 correlation_reused` | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F05 | Delegator's own quantity limits enforced in authorization-svc (S8-D1) | 🟡 | Reconfirm (was ✅): authorization-svc 000040, 2 Nov (`3fb48d6b`) | VC-1 |
| D1-8.F06 | Evidence rows for refused escalations; `evidence_ref` on every event; events `authority.delegated/.revoked/.expired` projected into authorization-svc and identity-context-svc | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F07 | If-Match / `expected_version` (428 / 412) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D1-8.F08 | Envelope verification enforced (compose `DELEGATED_AUTHORITY_IDENTITY_VERIFY` default `warn`) | 🟡 | Compose | X5 |
| D1-8.F09 | Cursor paging on `GET /v1/delegations` (API-4) | ❌ | — | DA-1 |
| D1-8.F10 | `/v1/iam/delegations` alias (IAM-11 / AU-26) | ❌ | Not built | DA-1 |
| D1-8.F11 | Dependency E2E | ❌ | No `e2e-delegated-authority-deps.py` | DA-2 |
| D1-8.F12 | Console v2.0 (Delegations, Proposed for approval, Expiring, Explain chain) | ❌ | Legacy page | DA-FE-1…3 |

**Tasks**

- [ ] **DA-0 · Baseline (S0):** matrix in `progress.md`; suite with counts.
- [ ] **DA-1 · API-4 + IAM-11:** cursor paging on the list; `/v1/iam/delegations` alias with parity test.
- [ ] **DA-2 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-delegated-authority-deps.py` (authorization SoD validate; `authority.delegated` reaches authorization-svc and the identity envelope; revoke removes it within TTL; expiry sweep).

**Console (frontend)** — `app/admin/delegations` (legacy, 18 Aug) · client `lib/api/delegations.ts` calls 4 of 9 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Access › Delegated Authority"; gold primary "Delegate authority"; secondary "Explain a chain" |
| KPIs (from new `GET /v1/delegations/summary`) | Active · Proposed awaiting approval · Expiring in 7 days · Suspended · On-behalf grants |
| Panels | Pipeline: Proposed → Active → Suspended → Expired / Revoked. Ordered list: the explain chain of the selected delegation (delegator → delegate). Bar list: delegations by action with limit utilisation, with an amber callout for expiring or at-limit delegations |
| Tabs | Active · Proposed · Suspended · Expired & revoked · Explain (F01–F04) |
| Drawer commands | Activate (independent approver; hidden from the delegator), extend, suspend, resume, revoke. Each takes a reason, If-Match and step-up retry |
| Footer | Required controls: the delegator must hold the action · no self-dealing · on-behalf grants need an independent approver · limits never exceed the delegator's own |

**Tasks**
- [ ] **DA-FE-1 · Summary route + API client** for every route in Appendix B D1-8.
- [ ] **DA-FE-2 · Page v2.0.**
- [ ] **DA-FE-3 · UI E2E** `tests/e2e/console/delegations.spec.ts`. Cover:
  - delegate → the delegate's action is permitted;
  - an on-behalf grant activated by a second user;
  - revoke ends it;
  - self-delegation refused;
  - empty and unavailable states.

---

## D1-9 · access-control-svc (:8137, DB `access_control`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-IAM-001 §5, §9, §10.1, §19, §21, §24; Authorization Standard |
| Latest score | 10/10 (100%) |
| Status | AUTH-PLANE Service 3 (AC-0…AC-FE-2) **not started**. Migrations since: 000025 repair (`a374c56b`), 000026/000027 identity and federation actions, 000037…000045 teammates' actions, 000046 evidence-request templates, 000047 evidence-manifest actions (`9be6c41d`) |
| Stages | `progress.md` last updated 12 Oct |
| Console | `/admin/access-control` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-9.F01 | Role definitions: `GET/POST /v1/role-definitions`, `GET/PATCH …/{id}`; provisioned into authorization-svc | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F02 | Permission bundles: `GET /v1/permission-bundles`, `GET/POST /v1/role-definitions/{id}/permission-bundles`, `GET/PATCH/DELETE …/{bundle_id}` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F03 | Permission registry: `GET /v1/permission-definitions` (65 §5 names + legacy names; unknown actions refused) | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F04 | Registered actions for other services: 000026, 000027, 000037–000045 | 🟡 | Reconfirm (was ✅): `TestRegisteredPermissionDefinitions` | VC-1 |
| D1-9.F05 | Evidence-manifest actions and templates (000047): `EVIDENCE_MANIFEST_GENERATE`/`_READ`, template `EVIDENCE_MANIFEST_MANAGER` v1, `EVIDENCE_REQUEST_MANAGER` v2, `OBLIGATION_MANAGER` v3 | 🟡 | `TestR5_EvidenceManifestActionsAreInTemplates` written; the integration suite failed to start (embedded Postgres `initdb` refuses user id 1000); not applied to the dev DB | AC-0 |
| D1-9.F06 | System role templates (append-only) and the 21 §9.1 archetypes: `GET /v1/role-templates`, `GET …/{code}`, `GET …/versions`, `POST …/instantiate`, `POST …/upgrade` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F07 | Assignment requests: `GET/POST /v1/iam/access-assignments`, `GET …/{id}`, `POST …/{id}/approve|reject|cancel|revoke` (risk-tiered, SoD re-check, security approver for CRITICAL, effective-dated revoke, expiry sweep) | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F08 | Groups: `GET/POST /v1/iam/groups`, `GET …/{id}`, `POST …/members`, `POST …/members/{id}/remove` (and `:remove`), `GET/POST …/assignments`, `GET /v1/iam/group-assignments/{id}`, `POST …/revoke|sync` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F09 | Access reviews: `GET/POST /v1/access-review-campaigns`, `GET …/{id}`, `POST …/complete`, `POST …/items/{id}/decide`, `…/reassign`, `GET /v1/iam/access-reviews`; HR-event triggered via subject links `GET/POST /v1/iam/subject-links` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F10 | Federation role eligibility: `GET /v1/iam/federation/role-eligibility` | 🔵 | Routed; added with the federation work (3 Nov); not audited | AC-0 |
| D1-9.F11 | Sessions end on role or assignment change; Idempotency-Key; outbox; refused-escalation evidence | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F12 | `resource.action` aliases for legacy names (S9-D1) | ❌ | Decided, not built | AC-1 |
| D1-9.F13 | Production grants by migration (notification actions, workflow actions incl. `WORKFLOW_CANCEL/ESCALATE/INVALIDATE/ADMINISTER`, a template with `WORKFLOW_APPROVE`, `iam.workload.manage`; service identities `svc-access-control`, `svc-policy`, `GOVERNANCE_DECISION_RECORD` for ledger writers) | ❌ | Only `seed-demo-rbac.ps1` grants most of them | AC-2 |
| D1-9.F14 | Refused-escalations read and console summary: `GET /v1/iam/refused-escalations`, `GET /v1/iam/summary` | ❌ | Not built | AC-3 |
| D1-9.F15 | Cursor paging (API-4) on every list | ❌ | Offset lists | AC-3 |
| D1-9.F16 | ETag/If-Match beyond roles and bundles (assignment requests, groups, members, campaigns, items, subject links, template instantiate/upgrade) | 🟡 | Roles and bundles only | AC-4 |
| D1-9.F17 | Contract, docs, live, E2E; `IDENTITY_VERIFY=require` once the console sends the bearer | ❌ | No `e2e-access-control-deps.py` | AC-5 |
| D1-9.F18 | Console `/admin/access-control` v2.0 (Roles, Bundles, Registry, Templates, Requests, Reviews, My reviews, Groups, Subject links, Refused escalations) | ❌ | Legacy page covers roles and bundles only | AC-FE-1…3 |

**Tasks** (as specified in Part B5, plus AC-0 additions)

- [ ] **AC-0 · Baseline (S0).** Run the store integration suite with `-tags=integration`. Embedded Postgres refuses to run `initdb` as uid 1000 in `golang:1.26-alpine`: run the container as a named non-root user (`adduser -D pg && su pg -c …`) or use a `postgres:16` service with `TEST_DATABASE_URL`. Record counts (0 skip). Apply 000047 up/down/up on scratch, then `deployments/migrate.py up --db access_control` on the dev stack. Verify the federation role-eligibility route. Extend `progress.md` with the matrix.
- [ ] **AC-1 · S9-D1 aliases.** Tests `TestS9D1_CanonicalGrantSatisfiesLegacyCheck`, `TestS9D1_LegacyGrantSatisfiesCanonicalCheck`, `TestS9D1_UnknownActionStillRefused`, `TestS9D1_AliasCannotPointAtUnregisteredAction`.
- [ ] **AC-2 · Production grants by migration** (access-control templates + authorization-svc service-identity assignments). Remove matching grants from `seed-demo-rbac.ps1` only after the migration covers them.
- [ ] **AC-3 · Refused escalations, summary, API-4** on every list. Tests per list: first, next, tampered 400, foreign-tenant 400, unknown filter 400; RLS as the app role.
- [ ] **AC-4 · API-3 completion** + self-approval tests `TestAC_RequesterCannotApproveOwnRequest`, `TestAC_ReviewerCannotDecideOwnAccess`, `TestAC_CriticalRoleNeedsSecurityApprover`.
- [ ] **AC-5 · Contract, docs, live, E2E:** `deployments/scripts/e2e-access-control-deps.py` with the edges listed in the AUTH-PLANE plan.

**Console (frontend)** — `app/admin/access-control` (legacy) · client `lib/api/access-control.ts` calls 3 of 50 routes (Appendix B). Full specification: Part B5, AC-FE-1 and AC-FE-2 (header "Roles & Access", KPIs, panels, ten tabs, requester and reviewer rules). Follow "Frontend rules for every console" below.

**Tasks**
- [ ] **AC-FE-1 · API client** for every route in Appendix B D1-9 + `/v1/iam/summary` and `/v1/iam/refused-escalations` (AC-3).
- [ ] **AC-FE-2 · Page v2.0** as in the AUTH-PLANE plan.
- [ ] **AC-FE-3 · UI E2E** `tests/e2e/console/access-control.spec.ts`. Cover:
  - access request → approved by a second user → `/v1/authorize` permits;
  - CRITICAL role needs the security approver;
  - a review REVOKE removes access;
  - a group assignment fans out;
  - the requester never sees Approve;
  - empty and unavailable states.

**Hand-back:** completes the auth plane. Then re-run the 10-step verifier protocol.

---

# Domain 2 — Governance Platform

These services answer "is this allowed, under which rules, who must approve, and what proof is needed?"

## D2-1 · jurisdiction-rules-svc (:8082, DB `jurisdiction_rules`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.2; ZS-JUR-001; ZS-SVC-V-001 PDC-02; REF-01, REF-10; Jurisdiction Pack Standard (`docs/architecture/jurisdiction-pack-wave*.md`) |
| Latest score | 73 met / 4 partial / 2 failed of 79 (92.4%, 94.9% weighted), 19 Oct |
| New since the audit | ZS-JUR-001 gap closing and jurisdiction-pack waves (30–31 Oct), REF-01 waves 1–3 (`a2d65f51`, `74db6105`, `645b1f35`), REF-3 waves 1–2 (`1302a620`, `dd86aade`), all by 07Yashjais. Not audited |
| Stages | S0 not started: there is no `progress.md` |
| Console | `/admin/jurisdictions` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-1.F01 | Jurisdiction registry: `GET /v1/jurisdictions`, `GET …/{id}` (fail-closed check; 503 = cannot answer), `GET …/{id}/ancestors`, `POST /v1/admin/jurisdictions`, `POST …/{id}/deactivate` | 🟡 | Reconfirm (was ✅): Governance audit, jurisdiction-rules rows (19 Oct) | VC-1 |
| D2-1.F02 | Effective-dated rules: `GET /v1/jurisdictions/{id}/rules`, `GET /v1/rules/{id}`, `GET …/status-history`, `POST /v1/admin/jurisdictions/{id}/rules`, `POST /v1/admin/rules/{id}/transition` (creator ≠ activator) | 🟡 | Reconfirm (was ✅): Governance audit (19 Oct) | VC-1 |
| D2-1.F03 | Rule pack with `pack_digest`: `GET /v1/jurisdictions/{id}/rule-pack` | 🟡 | Reconfirm (was ✅): Governance audit (19 Oct) | VC-1 |
| D2-1.F04 | Manual legal drift: `POST /v1/admin/rules/{id}/drift`, `GET /v1/rules/{id}/drift-events`, event `legal.drift.detected` | 🟡 | Reconfirm (was ✅): Governance audit (19 Oct) | VC-1 |
| D2-1.F05 | Automatic drift from a regulatory feed (J-03, J-04) | ⛔ | external-data-feed-svc publishes no regulatory-change event | Cross-team, then JR-6 |
| D2-1.F06 | Activation approval reference resolved against workflow-svc (J-02) | 🔵 | `internal/approval/client.go`, `WithApprovalVerifier` (`internal/handler/entity.go`); not audited | JR-1 |
| D2-1.F07 | REF-01 registry commands: versioned jurisdictions `GET …/{id}/as-of`, `…/hierarchy`, aliases `GET/POST …/{id}/aliases`, `POST …/aliases/{id}/end`, `GET /v1/jurisdiction-code-mappings`, `GET /v1/jurisdiction-resolutions`, maker-checker imports `GET/POST /v1/admin/jurisdiction-imports`, `GET …/{batch}`, `POST …/approve`, `…/reject`, hierarchy change events | 🔵 | REF-01 waves 1–3; registered actions 000037, 000038 | JR-1 |
| D2-1.F08 | REF-10 business calendars: `GET /v1/calendars`, `GET …/{code}`, `…/versions`, `…/versions/{v}`, `…/is-business-day`, `…/add-business-days`; `POST /v1/admin/calendars`, `…/{code}/versions`, `…/versions/{v}/publish`, `PUT …/sources`, `POST …/{code}/imports` (holiday import, observed holidays) | 🔵 | REF-3 wave 1 (`1302a620`) | JR-1 |
| D2-1.F09 | REF-10 payment terms: `GET /v1/payment-terms/{code}`, `…/versions`, `…/versions/{v}`, `…/due-date`, `…/explain-due-date`; `POST /v1/admin/payment-terms`, `…/{code}/versions`, `…/versions/{v}/approve|supersede|retire` | 🔵 | REF-3 wave 2 (`dd86aade`); registered actions 000041 | JR-1 |
| D2-1.F10 | Regulatory calendars and obligation rules: `GET /v1/obligation-rules`, `GET …/{id}`, `POST /v1/admin/obligation-rules`, `PUT …/{id}/provenance`, `POST …/{id}/publish`, `POST /v1/obligation-calculations:calculate` | 🔵 | Built before 2 Nov; not audited | JR-1 |
| D2-1.F11 | Rule resolution with decision evidence: `POST /v1/rule-resolutions:resolve`, `GET /v1/rule-decisions/{id}`, `GET /v1/pack-coverage`, `POST /v1/admin/resolver-cache:invalidate` | 🔵 | Compose now sets `RESOLVER_ENABLED=true` by default (J-07 decision executed); not audited | JR-1 |
| D2-1.F12 | Action validation: `POST /v1/validate-action` (`JURISDICTION_RULE_VALIDATE`) | 🔵 | `internal/handler/validate.go`, `validate_test.go`; not audited | JR-1 |
| D2-1.F13 | JUR-1 determination: `POST /v1/jurisdiction-decisions:resolve` | 🔵 | `internal/handler/determination.go`. Its inputs (ORG-6 establishments, ORG-7 registrations) do not exist yet | JR-1, then complete after ORG-6/ORG-7 |
| D2-1.F14 | Jurisdiction packs: `GET /v1/packs`, `GET …/{ref}`, `…/versions`, `…/versions/{v}`; `POST /v1/admin/packs`, `…/versions`, `…/submit-review`; regimes `GET /v1/regimes`, `GET …/{regime}`, `POST /v1/admin/regimes`; sources `GET /v1/sources`, `GET /v1/sources/{id}`, `POST /v1/admin/sources`, `POST /v1/admin/sources/{id}/review`, `POST /v1/admin/sources/{id}/supersede`; interpretations `GET /v1/interpretations/{id}`, `POST /v1/admin/interpretations`, `…/approve`; rule provenance and parameters `GET /v1/rules/{id}/provenance`, `PUT /v1/admin/rules/{id}/provenance`, `GET /v1/rules/{id}/parameters`, `PUT /v1/admin/rules/{id}/parameters` | 🔵 | Jurisdiction-pack waves 0–4 (docs in `docs/architecture/`); not audited | JR-1 |
| D2-1.F15 | Pack compile, sign, verify, artifact; signing keys: `POST …/versions/{v}/compile`, `…/sign`, `POST /v1/packs/{ref}/versions/{v}/verify`, `GET …/artifact`, `GET /v1/pack-signing-keys`, `POST /v1/admin/pack-signing-keys`, `…/{key}/retire|revoke` | 🔵 | Built before 2 Nov; not audited | JR-1 |
| D2-1.F16 | Pack certification: `POST …/versions/{v}/test-bundle`, `…/test-runs`, `…/reviews`, `…/certify`; `GET …/certification`, `…/reviews`, `…/test-bundle`, `…/test-runs` | 🔵 | Wave 5; not audited | JR-1 |
| D2-1.F17 | Pack operations: `POST /v1/admin/packs/{ref}/versions/{v}/publish`, `/v1/admin/packs/{ref}/versions/{v}/deploy`, `/v1/admin/packs/{ref}/versions/{v}/rollback`, `/v1/admin/packs/{ref}/versions/{v}/supersede`, `/v1/admin/packs/{ref}/versions/{v}/withdraw`, `/v1/admin/packs/{ref}/versions/{v}/block`, `/v1/admin/packs/{ref}/versions/{v}/unblock`, `/v1/admin/packs/{ref}/versions/{v}/hotfix`, `…/hotfix/retrospective`, `GET /v1/packs/{ref}/versions/{v}/deployments`, `GET /v1/packs/{ref}/versions/{v}/hotfix`; deployment rings and regions `GET/POST /v1/deployment-rings`, `/v1/deployment-regions` (admin create); source-change notices `GET /v1/source-change-notices`, `GET …/{id}`, `POST /v1/admin/source-change-notices`, `…/review`, `…/close`; §29 operations feed `GET /v1/ops/metrics` | 🔵 | Waves 6–7; not audited | JR-1 |
| D2-1.F18 | Rollouts: `GET/POST /v1/admin/rollouts`, `GET …/{id}`, `POST …/attestations`, `…/expert-approvals`, `…/transition`, `PUT …/owner`, `PUT …/packs`, `POST /v1/admin/rollouts:seed-portfolio`, `GET /v1/rollout-checklists`, `GET /v1/jurisdiction-support/{code}` | 🔵 | Wave 8; not audited | JR-1 |
| D2-1.F19 | Remediation cases: `GET/POST /v1/admin/remediation-cases`, `GET …/{id}`, `POST …/close`, `GET …/decisions`, `POST …/dispositions` | 🔵 | Wave 7 remediation; not audited | JR-1 |
| D2-1.F20 | Applicability and determination APIs: `POST /v1/licences:evaluate`, `/v1/registration-obligations:resolve`, `/v1/registration-identifiers:validate`, `/v1/scheme-elections:evaluate`, `/v1/threshold-monitors:evaluate`, `/v1/submission-profiles:resolve`, `/v1/payroll-statutory-parameters:resolve`, `/v1/retention-rules:resolve`, `/v1/accounting-mappings:resolve`, `/v1/regulatory-calculations:execute` | 🔵 | Waves 2–4; not audited | JR-1 |
| D2-1.F21 | Regulatory submissions: `POST /v1/regulatory-submissions`, `GET …/{id}`, `POST …/{id}/events` | 🔵 | Not audited | JR-1 |
| D2-1.F22 | Entity-jurisdiction projection from tenant-registry events: `GET /v1/entities/{id}/jurisdictions` (consumer) | 🟡 | No backfill for entities whose events left Kafka retention (J-10) | JR-2 |
| D2-1.F23 | Outbox hygiene: registry events written inside the transaction (J-11), `aggregate_version` on registry events (J-12), `FOR UPDATE SKIP LOCKED` in the worker (J-15), replay tool for parked events (J-16) | 🟡 | Parked events exist (`internal/events/publisher.go`); no replay tool; `SKIP LOCKED` not found in the code; J-11/J-12 not re-checked | JR-3 |
| D2-1.F24 | Identity verification instead of trusting `X-Principal-Id`; published port `8082:8082` removed (J-01, X5) | 🟡 | Compose still publishes `8082:8082`; `IDENTITY_VERIFY` default `warn` | JR-4 |
| D2-1.F25 | Bitemporal replay, idempotency keys, ETag/If-Match, append-only triggers, RFC 9457 | 🟡 | Reconfirm (was ✅): Governance audit; IMPLEMENTATION-PROGRESS API-2/API-3 | VC-1 |
| D2-1.F26 | Decisions: owner of entity applicability (C8, J-05), one precedence model (J-08), public reads / `rule_payload` content / event naming (J-09) | 🧭 | Governance audit register | Decision |
| D2-1.F27 | Cursor paging (API-4) on the many list routes | ❌ | — | JR-5 |
| D2-1.F28 | Contract parity: openapi has 148 paths; no route-parity test | 🟡 | `openapi_validation_test.go` validates the file; it does not compare it with the router | JR-5 |
| D2-1.F29 | Console v2.0 (Jurisdictions, Rules, Packs, Calendars, Payment terms, Imports, Rollouts, Remediation) | ❌ | Legacy page | JR-FE-1…3 |

**Tasks**

- [ ] **JR-0 · Baseline (S0):** create `services/jurisdiction-rules-svc/progress.md` with a matrix (ZS-JUR-001, Doc 03 §8.2, REF-01, REF-10, the pack standard waves). Full suite on scratch Postgres (0 skip). Apply missing local migrations with `deployments/migrate.py up --db jurisdiction_rules` (E-1).
- [ ] **JR-1 · Audit and finish the unaudited surface (F06–F21).** For each route group: authorization at the object, maker-checker where a version is approved, Idempotency-Key on mutations, If-Match/ETag, outbox in the same transaction, RLS + FORCE, R5 registrations, openapi entry. Spec negative paths get `TestJUR_*` / `TestREF01_*` / `TestREF10_*` tests. Record results in the matrix; fix what fails.
- [ ] **JR-2 · J-10 backfill job** for entities whose events left Kafka retention (reads tenant-entity-registry). Test: an entity with no event gets its projection.
- [ ] **JR-3 · Outbox hygiene:** J-11, J-12, J-15 (`FOR UPDATE SKIP LOCKED`, two-worker test), J-16 (parked-event replay command, with authorization and evidence).
- [ ] **JR-4 · J-01:** remove the published `8082:8082` port from compose; verify the envelope (`IDENTITY_VERIFY=require` once callers send it).
- [ ] **JR-5 · API-4 and contract parity:** cursor paging on lists; add a router ↔ openapi parity test (pattern: `evidence-requirements-svc/internal/handler/openapi_parity_test.go`); extend the Postman collection (E-4).
- [ ] **JR-6 · Drift consumer (J-03, J-04)** once external-data-feed-svc publishes a regulatory-change event with `external_feed_reference`: move matching rules to `UNDER_REVIEW` and record drift.
- [ ] **JR-7 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-jurisdiction-rules-deps.py` (tenant-registry consumer, workflow approval reference, authorization decisions, Kafka events, pack sign/verify round trip).

**Console (frontend)** — `app/admin/jurisdictions` (legacy) · client `lib/api/jurisdictions.ts` calls 12 of 148 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. This is the largest console gap overall; split the drawer forms per tab file.

| Part | Content |
|---|---|
| Header | "Governance › Jurisdictions & Rule Packs"; gold primary "New rule"; secondary "Validate an action", "New import" |
| KPIs (from new `GET /v1/jurisdiction-summary`) | Active jurisdictions · Rules in DRAFT or review · Packs published · Rollouts in progress · Open drift events + remediation cases |
| Panels | Pipeline: pack Draft → In review → Certified → Signed → Published → Deployed. Ordered list: ancestor chain of the selected jurisdiction. Bar list: pack coverage by jurisdiction (`/v1/pack-coverage`), with an amber callout for open drift events and source-change notices |
| Tabs | Jurisdictions (F01, F07) · Rules (F02, F04, F14) · Rule packs (F03, F14, F15, F16, F17) · Rollouts (F18) · Calendars (F08) · Payment terms (F09) · Obligation rules (F10) · Imports (F07) · Sources & notices (F14, F17) · Remediation (F19) · Resolve & validate (F11, F12, F13, F20) · Submissions (F21) |
| Drawer commands | Rule transition (creator ≠ activator; approval reference), drift record, provenance and parameters. Import approve / reject (≠ proposer). Alias add / end. Calendar version / sources / publish / holiday import. Payment term version approve / supersede / retire. Pack submit-review / test bundle / test run / review / certify / compile / sign / verify / publish / deploy / rollback / hotfix (+ retrospective) / block / unblock / withdraw. Rollout owner / packs / attest / expert approval / transition. Remediation disposition / close. Signing key register / retire / revoke |
| Footer | Required controls: the creator never activates · imports and payment terms are maker-checker · only signed packs deploy · evidence and a reason on every change |

**Tasks**
- [ ] **JR-FE-1 · Summary route + API client** for every route in Appendix B D2-1. Split the client by area if it passes ~800 lines (`lib/api/jurisdictions/*.ts`).
- [ ] **JR-FE-2 · Page v2.0.**
- [ ] **JR-FE-3 · UI E2E** `tests/e2e/console/jurisdictions.spec.ts`. Cover:
  - a rule created by one user and activated by another;
  - an import approved by a second user;
  - business-day arithmetic shown from a published calendar;
  - payment-term due date with explain;
  - pack sign → verify → publish;
  - validate-action diagnostic;
  - empty and unavailable states.

**Blocked and decisions:** external-data-feed-svc event; C8/J-05, J-08, J-09.

---

## D2-2 · governance-decision-log-svc (:8083, DB `governance_decision_log`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.7; GOV-07 |
| Latest score | 41 met / 6 partial / 0 failed of 47 (87.2%, 93.6% weighted) |
| Stages | `progress.md` (no feature matrix in the S0 format) |
| Console | `/admin/governance` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-2.F01 | Append a decision: `POST /v1/decisions` (tenant-owned, immutable, idempotent on `(tenant_id, decision_id)`) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-2.F02 | Query and get: `GET /v1/decisions`, `GET /v1/decisions/{id}` (ETag on reads) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-2.F03 | Replay: `POST /v1/decisions/{id}/replay`; `GET …/{id}/replay-manifests` (tenant-scoped) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-2.F04 | Outbox and relay for `governance.decision.recorded` / `.replayed` | 🟡 | Coded and retry-tested; not proven against a real broker | X3 |
| D2-2.F05 | `jurisdiction_context` on decisions; column-width and size guards; RFC 9457 | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-2.F06 | Read authorization | 🟡 | Compose `DECISION_LOG_READ_AUTHZ: "warn"` (hard-coded) | GDL-1 |
| D2-2.F07 | Callers forward the envelope and `X-Principal-Id` (contract-lifecycle, decision-support, evidence-manifest) (D-04) | 🟡 | Other teams' services | Cross-team |
| D2-2.F08 | Identity verification through ForwardAuth and mTLS workload identity (D-05, X5) | 🟡 | `IDENTITY_VERIFY` default `warn` | X5 |
| D2-2.F09 | GOV-07 evidence ledger and verification (hash chain, seal, verify, export) | ❌ | Not built | G-07 |
| D2-2.F10 | workflow-svc ledger relay proven live (`WORKFLOW_LEDGER_PRINCIPAL_ID` granted `GOVERNANCE_DECISION_RECORD`) | ❌ | Coded 21 Oct; never run against a real decision log | GDL-2 |
| D2-2.F11 | Open questions: "audit" query dimension; GOV-07 hash chain; `decision_id` = §16 `evidence_id`; retention class; which GOV section owns GovernanceDecision; `X-Purpose-Context` on reads or appends (D-06); event naming (D-07, C3) | 🧭 | Governance audit register | Decision |
| D2-2.F12 | Cursor paging on `GET /v1/decisions` (API-4) | ❌ | — | GDL-1 |
| D2-2.F13 | Dependency E2E and console v2.0 | ❌ | No `e2e-governance-decision-log-deps.py`; legacy page | GDL-3, GDL-FE-1…3 |

**Tasks**

- [ ] **GDL-0 · Baseline (S0):** matrix in `progress.md`; suite with counts.
- [ ] **GDL-1 · Read authorization enforce + API-4:** grant every reader `GOVERNANCE_DECISION_READ` by migration (AC-2); make compose read `DECISION_LOG_READ_AUTHZ` from an env default and set `enforce`; cursor paging. Tests: reader without the grant → 403 in enforce mode.
- [ ] **G-07 · Evidence ledger** (IMPLEMENTATION-PLAN §6 G-07): per-tenant hash chain, sealed packages, verify, export; tamper test.
- [ ] **GDL-2 · Live ledger relay:** set `WORKFLOW_LEDGER_PRINCIPAL_ID`, grant it, run workflow → decision log once on the stack, record the decision id.
- [ ] **GDL-3 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-governance-decision-log-deps.py` (policy relay as `svc-policy`, workflow relay, authorization read checks, Kafka event).

**Console (frontend)** — `app/admin/governance` (legacy) · client `lib/api/governance.ts` calls 2 of 4 screen-relevant routes (Appendix B; the append is service-to-service). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Governance › Decision Log"; gold primary "Replay a decision"; secondary "Export evidence" (after G-07) |
| KPIs (from new `GET /v1/decisions/summary`) | Decisions (24 h) · Source services · Replays (30 d) · Replay mismatches · Evidence chain status (after G-07) |
| Panels | Pipeline: Recorded → Relayed → Replayed → Verified. Ordered list: top decision sources (policy, workflow, authorization, evidence). Bar list: decisions by type and outcome, with an amber callout for replay mismatches and for read authorization still in `warn` |
| Tabs | Decisions (F01, F02) · Replays (F03) · Evidence chain (G-07, when built) |
| Drawer commands | Replay (reason) and view manifest. After G-07: verify integrity, seal package, export bundle |
| Footer | Required controls: decisions are immutable · reads are authorized · every replay records a manifest |

**Tasks**
- [ ] **GDL-FE-1 · Summary route + API client** for every route in Appendix B D2-2 (and the G-07 routes once built).
- [ ] **GDL-FE-2 · Page v2.0.**
- [ ] **GDL-FE-3 · UI E2E** `tests/e2e/console/decision-log.spec.ts`. Cover:
  - a decision written by policy-svc appears;
  - replay shows its manifest;
  - a reader without the grant is refused once enforce is on;
  - empty and unavailable states.

**Blocked and decisions:** the open questions above; callers' envelope forwarding (D-04).

---

## D2-3 · policy-svc (:8085, DB `policy`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.1; GOV-05; ZS-SVC-V-001; REF-07 |
| Latest score | 54 met / 2 partial / 0 failed of 56 (96.4%, 98.2% weighted) |
| New since the audit | `openapi.yaml` + parity test (28 Oct); REF-2 waves 1–2 (`b1761456`, `196d2a71`, 07Yashjais). REF-2 not audited |
| Stages | `progress.md` last updated 14 Oct |
| Console | `/admin/policies` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-3.F01 | Policies and versions: `GET/POST /v1/policies`, `GET …/{id}`, `GET/POST …/{id}/versions`, `GET /v1/policy-versions/{id}`, `GET …/transitions`, `GET /v1/policy-versions/compare` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-3.F02 | GOV-05 lifecycle: `POST …/versions/{v}/submit-review`, `…/approve` (maker-checker + own-object SoD), `…/activate`, `…/schedule`, `…/suspend`, `…/review-suspension` (suspender cannot review), `…/retire`; If-Match mandatory (428) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-3.F03 | Evaluate, resolve, explain, evidence: `POST /v1/policies/evaluate`, `GET /v1/policies/resolve`, `GET /v1/policies/explain`, `GET /v1/policy-evaluations/{id}` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-3.F04 | Evidence relayed into governance-decision-log-svc as `svc-policy` (durable outbox) | 🟡 | Reconfirm (was ✅): Governance audit | Production grant: AC-2; VC-1 |
| D2-3.F05 | 8 event types; `jurisdiction_context` and `residency_region` on events | 🟡 | Reconfirm (was ✅): Governance audit | Kafka proof: X3; VC-1 |
| D2-3.F06 | Control tests and attestations: `POST /v1/control-test-definitions`, `GET …/{id}`, `GET/POST …/{id}/executions`, `GET /v1/controls/{ref}/effectiveness`, `POST /v1/attestations`, `GET …/{id}`, `POST …/{id}/revoke` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-3.F07 | Identity verification (JWKS envelope, or mTLS on `:8449`) | 🟡 | Compose `POLICY_IDENTITY_VERIFY` default `warn` (P-12); needs governance-decision-log to forward the envelope on replay | POL-3 |
| D2-3.F08 | Evaluation for SPEND_CONTROL, SIGNATORY_MATRIX, SOD_RULE (P-01) | 🧭 | No business rules define them | Business input, then POL-2 |
| D2-3.F09 | `openapi.yaml` with router parity | 🟡 | Reconfirm (was ✅): Published 28 Oct (`e2a6c285`); `TestOpenAPI_MatchesTheRouter` (`internal/handler/api2_problem_test.go`); extended by REF-2 | VC-1 |
| D2-3.F10 | REF-07 accounting policy: `GET/POST /v1/accounting-policies`, `GET …/resolve`, `GET …/explain`, `GET …/versions/{id}`, `GET …/versions/{id}/impact`, `POST …/versions/{id}/submit-review|approve|activate|supersede|emergency-suspend|review-suspension` | 🔵 | REF-2 waves 1–2; registered actions 000039, 000040 | POL-1 |
| D2-3.F11 | Callers report a policy-svc 401/403 as 401/403, not 503 (P-13: expense-claim-svc, payment-authorization-svc) | 🟡 | Other teams' services | Cross-team |
| D2-3.F12 | V-001 PDC-05 certification suites, simulation, waivers | ❌ | Roadmap | POL-4 |
| D2-3.F13 | Cursor paging (API-4) on `GET /v1/policies`, `GET /v1/accounting-policies`, executions | ❌ | — | POL-1 |
| D2-3.F14 | Dependency E2E and console v2.0 | ❌ | No `e2e-policy-deps.py`; legacy page | POL-5, POL-FE-1…3 |

**Tasks**

- [ ] **POL-0 · Baseline (S0):** matrix in `progress.md` (GOV-05, Doc 03 §8.1, REF-07); suite with counts.
- [ ] **POL-1 · Verify REF-2 and the contract:** accounting-policy routes (maker-checker, emergency suspension needs a live break-glass session, supersession, If-Match, events, RLS, R5); keep `TestOpenAPI_MatchesTheRouter` green; API-4 on lists. Tests `TestREF07_*` for each spec negative path.
- [ ] **POL-2 · Three policy types** once P-01 business input exists: evaluator and payload validation per type.
- [ ] **POL-3 · P-12 enforce identity** after governance-decision-log forwards the envelope on replay.
- [ ] **POL-4 · PDC-05** certification suites, simulation, waivers (V-001).
- [ ] **POL-5 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-policy-deps.py` (authorization SoD on approve, decision-log relay as `svc-policy`, break-glass on emergency suspension, Kafka events).

**Console (frontend)** — `app/admin/policies` (legacy) · client `lib/api/policies.ts` calls 7 of 39 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Governance › Policies"; gold primary "Draft policy"; secondary "Evaluate", "Draft accounting policy" |
| KPIs (from new `GET /v1/policies/summary`) | Active policies · Versions in review · Suspended awaiting review · Evaluations (24 h) · Control tests failing |
| Panels | Pipeline: Draft → In review → Approved → Scheduled / Active → Suspended → Retired. Ordered list: resolution precedence for the selected scope (`/v1/policies/explain`). Bar list: control effectiveness, with an amber callout for failing tests and revoked attestations |
| Tabs | Policies (F01, F02) · Versions & compare (F01) · Accounting policies (F10) · Evaluate & explain (F03) · Control tests (F06) · Attestations (F06) |
| Drawer commands | Version submit-review / approve (≠ author; SoD) / activate / schedule / suspend / review-suspension (≠ suspender) / retire, all with If-Match. Accounting submit-review / approve / activate / supersede / emergency-suspend (break-glass) / review-suspension. Control test execution. Attestation create / revoke |
| Footer | Required controls: maker-checker on every version · the suspender never reviews · emergency suspension needs a break-glass session · If-Match on every change |

**Tasks**
- [ ] **POL-FE-1 · Summary route + API client** for every route in Appendix B D2-3.
- [ ] **POL-FE-2 · Page v2.0.**
- [ ] **POL-FE-3 · UI E2E** `tests/e2e/console/policies.spec.ts`. Cover:
  - a version approved by a second user and activated;
  - the suspender cannot review;
  - an accounting policy emergency-suspended only under break-glass;
  - compare two versions;
  - empty and unavailable states.

**Blocked and decisions:** P-01 business rules; P-13 callers (cross-team).

---

## D2-4 · obligations-svc (:8088, DB `obligations`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.5; R-001 WFC-05 (in scope by the 23 Oct decision) |
| Latest score | 41 met / 22 partial / 2 failed of 65 (63.1%, 80.0% weighted); WFC-05 unscored |
| Stages | `progress.md` (84 lines, 21 Oct); no S0 matrix |
| Console | `/admin/obligations` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-4.F01 | Obligations: `POST /v1/obligations` (dedup on tenant + code, jurisdiction validated fail-closed, entity-bound), `GET /v1/obligations`, `GET …/{id}`, `PATCH …/{id}` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-4.F02 | Status transitions OPEN / IN_PROGRESS / OVERDUE / CLOSED: `POST …/{id}/status`; overdue-detection scheduler | 🟡 | Reconfirm (was ✅): Governance audit | Replaced by WFC-05 states: WF-3; VC-1 |
| D2-4.F03 | Link / unlink a workflow instance: `POST/DELETE …/{id}/workflow` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-4.F04 | Filing requirements: `GET/POST …/{id}/filing-requirements`, `GET …/{fr}`, `POST …/{fr}/status`, `GET …/{fr}/transitions` | 🔵 | Built after the audit | OB-1 |
| D2-4.F05 | Obligation definitions: `GET/POST /v1/obligation-definitions`, `GET …/{id}`, `GET/POST …/{id}/versions`, `POST …/{id}/retire`; entity mappings `GET/POST …/{id}/entity-mappings`, `POST …/{m}/end` | 🔵 | Built after the audit | OB-1 (and WF-3) |
| D2-4.F06 | Applicability decisions (append-only, versioned, UNASSESSED distinct): `GET/POST …/{id}/applicability-decisions`, `GET …/{id}/applicability` | 🟡 | Duplicates appended; value not validated; "exactly one of human or system" not enforced | OB-2 |
| D2-4.F07 | Transactional outbox and relay (fixed 19 Oct) | 🟡 | Not proven against a real broker | X3 |
| D2-4.F08 | Idempotency-Key, If-Match, RLS, object-level read authorization, `X-Legal-Entity-Id` reconciliation, full envelope on outbound authz | 🟡 | Reconfirm (was ✅): Governance audit; IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D2-4.F09 | Legal entity checked against tenant-entity-registry (`GET /v1/entities/{id}/status`) | 🟡 | Header and body reconciled and authorized, not checked against the registry | OB-3 |
| D2-4.F10 | Single owner of obligation data (C8: obligations-svc decided owner) | 🟡 | obligation-tracking-svc and filing-tracker-svc also hold obligations and emit the same event names | Cross-team |
| D2-4.F11 | WFC-05 deadline clocks: clock classes, computed deadlines with calendar provenance (jurisdiction-rules calendars), warning schedule, states PLANNED/ACTIVE/DUE_SOON/DUE/OVERDUE/FULFILLED/WAIVED/NOT_APPLICABLE/SUPERSEDED, satisfaction evidence, waiver/N-A with authority, supersession on calendar correction, escalation policies, DST rule, `evaluate`/`satisfy` APIs, clock events | ❌ | Not built | WF-3 |
| D2-4.F12 | The 22 partial rows (definition/instance split; `obligation.updated` scope; event and channel naming; partition key; publish-failure metric; caller id collision; dead envelope resolver; invalid status → 400; Location header; openapi and Postman; safe store tests; URL escaping; compose `depends_on`; stale progress.md) | 🟡 | Governance audit, obligations section | OB-4 (WF-4) |
| D2-4.F13 | Cursor paging (API-4) | ❌ | — | OB-4 |
| D2-4.F14 | Dependency E2E and console v2.0 | ❌ | No `e2e-obligations-deps.py`; legacy page | OB-5, OB-FE-1…3 |

**Tasks**

- [ ] **OB-0 · Baseline (S0):** matrix in `progress.md` (Doc 03 §8.5, the 65 audit rows, WFC-05 §7); suite with counts.
- [ ] **OB-1 · Audit the new definition, mapping and filing routes**; fix and test (`TestOB1_*`).
- [ ] **OB-2 · Applicability decisions:** idempotent on key, CHECK on the value, exactly one decider (human or system). Tests per rule.
- [ ] **OB-3 · Registry check** of the legal entity (fail closed on 503).
- [ ] **WF-3 · WFC-05 deadline clocks** (IMPLEMENTATION-PLAN §6 WF-3), using jurisdiction-rules business calendars (D2-1.F08). Done when: frozen-clock OVERDUE test, calendar correction supersedes, waiver without authority 403, DST test.
- [ ] **OB-4 · The 22 partial rows + API-4** (WF-4), each re-scored with evidence.
- [ ] **OB-5 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-obligations-deps.py` (jurisdiction check, registry check, workflow link, evidence-requirements gate, notification on escalation, Kafka clock events).

**Console (frontend)** — `app/admin/obligations` (legacy) · client `lib/api/obligations.ts` calls 7 of 24 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. The Deadline clocks tab follows WF-3.

| Part | Content |
|---|---|
| Header | "Compliance › Obligations"; gold primary "New obligation"; secondary "Check applicability" |
| KPIs (from new `GET /v1/obligations/summary`) | Open · Due soon (7 d) · Overdue · Filing requirements pending · Unassessed applicability |
| Panels | Pipeline: Planned → Active → Due soon → Due → Overdue → Fulfilled (WF-3). Ordered list: escalation ladder of the selected clock. Bar list: obligations by jurisdiction and clock class, with an amber callout for overdue HARD_LEGAL clocks |
| Tabs | Obligations (F01, F02, F03) · Deadline clocks (F11) · Definitions & mappings (F05) · Filing requirements (F04) · Applicability (F06) |
| Drawer commands | Status transition. Link / unlink workflow. Filing status transition. Applicability decision (UNASSESSED distinct; exactly one decider). Definition version / retire. Mapping end. After WF-3: satisfy, waive and N/A (authority + reason + evidence) |
| Footer | Required controls: deadlines come from versioned rules and calendars · waivers need authority and evidence · OVERDUE is never suppressed |

**Tasks**
- [ ] **OB-FE-1 · Summary route + API client** for every route in Appendix B D2-4 (and the WF-3 routes).
- [ ] **OB-FE-2 · Page v2.0.**
- [ ] **OB-FE-3 · UI E2E** `tests/e2e/console/obligations.spec.ts`. Cover:
  - obligation created with a computed deadline and calendar provenance;
  - waive without authority refused;
  - linked workflow shown;
  - applicability decision recorded once;
  - empty and unavailable states.

**Blocked and decisions:** C8 overlap removal (cross-team).

---

## D2-5 · authorization-svc (:8089, DB `authorization`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.3; GOV-03/04/12; ZS-IAM-001; ORG-06 (role half) |
| Latest score | 85/85 (100%), 14 Oct; 2 Nov pass added IAM-13, quantity limits, API-3 |
| Stages | S0–S6 verified 2 Nov: suites 818 pass / 0 fail / 1 by-design skip; 4 mutants killed; dependency E2E 33/33; console rebuilt Reconfirm in VC-1 |
| Since 2 Nov | Merge of main (`15a9c581`) brought 07Yashjais's ORG-06 waves 4–5 (`2dc16f5b`, `8efb2a89`): suspension history and expiry events, next to our IAM-13. IC-3 re-points merged subjects (`0174312a`). Not re-verified after the merge |
| Console | `/admin/authorization` v2.0 |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-5.F01 | `POST /v1/authorize`: six layers in order (status, RBAC, delegation ∩ delegator's grants, static SoD, own-object SoD, ABAC); fails closed | 🟡 | Reconfirm (was ✅): Governance audit 85/85 | VC-1 |
| D2-5.F02 | SoD: `POST /v1/sod/validate`, `/v1/sod/evaluate`, `GET /v1/sod/conflicting-permissions`; rules `GET/POST /v1/admin/sod-rules`, `…/approve|reject|retire|reactivate`; exceptions `GET/POST /v1/admin/sod-exceptions`, `…/approve|reject|revoke` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F03 | Roles and bundles: `GET/POST /v1/admin/roles`, `…/retire|reactivate`, `GET/POST …/{id}/permission-bundles`, `POST /v1/admin/permission-bundles/{id}/retire|reactivate` | 🟡 | Reconfirm (was ✅): Governance audit; API-3 `3c010d3e` | VC-1 |
| D2-5.F04 | Role assignments (IAM-13): `GET/POST /v1/admin/role-assignments`, `GET …/as-of`, `POST …/{id}/approve|reject|revoke|suspend|resume`, `GET …/{id}/suspensions` | 🟡 | Reconfirm (was ✅): progress.md "2 Nov: IAM-13…"; E2E 33/33 | Re-verify after the merge: D2-5.F17; VC-1 |
| D2-5.F05 | ABAC rules with maker-checker: `GET/POST /v1/admin/abac-rules`, `…/approve|reject|retire|reactivate` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F06 | Authority limits, monetary and quantity (S8-D1): `GET/POST /v1/admin/authority-limits`, `…/approve|change|reject|retire` | 🟡 | Reconfirm (was ✅): 000040; mutant "quantity read as money" killed | VC-1 |
| D2-5.F07 | Workload bindings: `GET/POST /v1/admin/workload-bindings`, `…/deactivate|reactivate` | 🟡 | Reconfirm (was ✅): Governance audit; 000039 | VC-1 |
| D2-5.F08 | Delegated access and projection: `POST /v1/delegated-access/evaluate`, `GET/POST /v1/admin/delegated-authorities`, `…/revoke` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F09 | Privileged (JIT), break-glass and support sessions: `/v1/admin/privileged-sessions*`, `/v1/admin/break-glass-sessions*`, `/v1/support/sessions*` (+ `/admin/v1/*` aliases): request, approve with step-up for both parties, reject, review, revoke, activity; e.g. `/v1/admin/privileged-sessions/{id}/reject`, `/v1/admin/break-glass-sessions/{id}/reject`, `/admin/v1/privileged-sessions/{id}/reject`, `/admin/v1/break-glass-sessions/{id}/reject`; maximums 8 h / 2 h / 4 h | 🟡 | Reconfirm (was ✅): Governance audit | Sign-off: D2-5.F16; VC-1 |
| D2-5.F10 | Access decision log: `GET /v1/access-decisions`, `GET …/{id}` (`matched_grants`); `POST /internal/authorization/decisions` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F11 | Available actions and capabilities: `GET /v1/{resource_type}/{resource_id}/available-actions`, `GET /v1/me/capabilities`; entity scope `POST /v1/entity-scope/validate` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F12 | GOV-03 commands and cache: `POST /internal/v1/gov03/commands/invalidateAuthorizationCache`, `…/recomputeSubjectEffectiveAccess`, `POST /v1/admin/authorization-cache/invalidate`, `POST /v1/admin/subjects/{id}/effective-access/recompute` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-5.F13 | Envelope verification (JWKS) on every non-evaluation route; mTLS workload gate; admin delegates by verified mTLS CN | 🟡 | Built. Compose runs `AUTHZ_IDENTITY_ASSERTION: "warn"` and `AUTHZ_EVALUATION_MTLS: "permit"` (F-1, M-02) | AZ-2 |
| D2-5.F14 | Event consumers (principal status, assignments, delegations, `identity.subject.merged`) with cache invalidation | 🟡 | Reconfirm (was ✅): Governance audit; `0174312a` | VC-1 |
| D2-5.F15 | If-Match on assignments and workload bindings is honoured but not required | 🟡 | Console and access-control's revoke call send none (progress.md 2 Nov) | AZ-1 |
| D2-5.F16 | Security sign-offs: TOTP interim factor (SO-1), session maximums (SO-2) | 🧭 | Security | Decision |
| D2-5.F17 | Main's ORG-06 waves 4–5 merged next to IAM-13 (suspension history, assignment expiry events, session handling of suspended assignments) | 🔵 | `2dc16f5b`, `8efb2a89`, merge `15a9c581`; no verifier pass since | AZ-3 |
| D2-5.F18 | Callers: no tenantless `/v1/authorize`; preparer sent on own-object actions (X-1, X-2); `X-Authn-Age-Seconds` forwarded, then `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up` (X-3) | 🟡 | Other services | Cross-team, then AZ-2 |
| D2-5.F19 | `/v1/iam/*` aliases (IAM-11 / AU-26): `/v1/iam/access-assignments`, `/v1/iam/delegations` as aliases of the admin routes | ❌ | access-control-svc owns `/v1/iam/access-assignments` as its own resource; aliasing not decided | AZ-4 |
| D2-5.F20 | IAM-7 DoD artefacts: `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`, `scripts/audit.sh` | ❌ | None of the four exists in `services/authorization-svc/` (6 Nov) | AZ-5 |
| D2-5.F21 | IAM-8 load and revocation SLOs (`/v1/authorize` p50/p95/p99, revocation lag vs IAM §29) | ❌ | Not measured | IAM-8 |
| D2-5.F22 | Cursor paging (API-4) on admin lists and access decisions | ❌ | — | AZ-1 |
| D2-5.F23 | Console `/admin/authorization` v2.0 | 🟡 | Page verified 2 Nov. The client calls 64 of 80 screen-relevant routes: no suspend/resume, as-of, session review/activity, reactivate or binding commands (Appendix B) | AZ-FE-1, AZ-FE-2 |

**Tasks**

- [ ] **AZ-1 · Require If-Match and add API-4:** after the console (frontend) and access-control-svc's revoke send `If-Match`, make it mandatory (428) on assignments and bindings; cursor paging on lists.
- [ ] **AZ-2 · Enforce identity and mTLS:** console forwards `Authorization: Bearer <envelope>` on every call (F-1), then `AUTHZ_IDENTITY_ASSERTION=require`; every PEP gets a client certificate (M-02), then `AUTHZ_EVALUATION_MTLS=require`; after X-3, `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up`; after X-1, `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=true` (default `false` in `internal/config/config.go`; the warn log lists the tenantless callers). Prove each in the E2E.
- [ ] **AZ-3 · Re-verify after the merge:** full suites (both DSNs, NOBYPASSRLS role); re-run the 4 mutants; check that main's ORG-06 suspension/expiry code and IAM-13 do not double-emit events or keep two suspension models; `e2e-authorization-deps.py` on a fresh image.
- [ ] **AZ-4 · IAM-11 aliases:** decide with access-control which route is canonical; implement aliases with parity tests (and Deprecation headers on the non-canonical one).
- [ ] **AZ-5 · IAM-7 artefacts:** create `asyncapi.yaml` (every produced and consumed event), `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`, `scripts/audit.sh` (pattern: `identity-context-svc`); asyncapi parses; `audit.sh` runs live.
- [ ] **IAM-8 · Load and revocation SLOs:** k6 or Go load test of `/v1/authorize`; measure revocation propagation; record against IAM §29.

**Console (frontend)** — `app/admin/authorization` (v2.0, 2 Nov) · client `lib/api/authorization.ts` calls 64 of 80 screen-relevant routes (Appendix B). The page exists. What is missing is the IAM-13 and session commands.

**Tasks**
- [ ] **AZ-FE-1 · Missing commands:**
  - Assignments tab: suspend / resume, suspension history, an "as of" date picker (`/as-of`).
  - Sessions tabs: review and activity for privileged, break-glass and support sessions; support session revoke.
  - Reactivate on roles, bundles, SoD rules and ABAC rules.
  - Workload binding deactivate / reactivate.
  - Every command sends If-Match from `version` (needed before AZ-1 makes it mandatory) and the bearer envelope (F-1).
- [ ] **AZ-FE-2 · UI E2E** `tests/e2e/console/authorization.spec.ts`. Cover:
  - assignment suspended → access denied → resumed (not by the assignee);
  - as-of view at two instants;
  - a break-glass session approved by a second user with step-up;
  - stale If-Match shows the reload prompt;
  - empty and unavailable states.

---

## D2-6 · workflow-svc (:8090, DB `workflow`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.4; GOV-06; R-001 WFC-01…WFC-05 (WFC-04 here, WFC-05 in obligations); ZS-STATE §11–§14 |
| Latest score | 36 met / 25 partial / 0 failed of 61 (59.0%, 79.5% weighted) |
| Stages | `progress.md` (161 lines, 21 Oct); no S0 matrix. Untracked `tmp/` and `workflow_handler_test.bin` in the service folder |
| Console | `/admin/workflows` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-6.F01 | Create an instance: `POST /v1/workflows` (expands the definition's stages, checks entity status in the registry, refuses a foreign tenant) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F02 | Reads: `GET /v1/workflows/{id}`, `…/next-approver`, `GET /v1/workflows/pending`, `GET …/{id}/evidence` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F03 | Actions: `POST …/{id}/actions` (approve/reject), `…/request-changes` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F04 | Quorum votes: `POST …/{id}/quorum-votes` | 🟡 | Quorum races open (L-2) | WF-1 |
| D2-6.F05 | Escalate, escalate-to, cancel, invalidate, verify-release: `POST …/{id}/escalate`, `…/escalate-to`, `…/cancel`, `…/invalidate`, `…/verify-release` | 🟡 | Reconfirm (was ✅): Governance audit | Grants: AC-2; VC-1 |
| D2-6.F06 | Timers: expiry, deadline, escalation, approver re-check; durable recovery of stuck instances | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F07 | Delegation checked through authorization-svc (`delegation_id` recorded); Idempotency-Key replay and mismatch | 🟡 | Reconfirm (was ✅): Governance audit | Handler tests: WF-5; VC-1 |
| D2-6.F08 | Decision-log writes through a dedicated outbox destination; `decision_id` returned; decisionlog client (`9d9bac43`) | 🟡 | No live ledger proof | GDL-2 (D2-2) |
| D2-6.F09 | Lineage on decisions and transitions: `access_decision_id`, `authority_basis`, `delegation_id` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F10 | Consumed events (policy, delegated-authority, authorization) trigger approver re-checks | 🟡 | Tenantless global `policy.*` events are skipped | WF-6 |
| D2-6.F11 | Workflow definitions: `GET/POST /v1/workflow-definitions`, `GET …/{id}`, `POST …/supersede|activate|suspend|retire` | 🟡 | Instances not pinned to a definition version; definitions table has no RLS | WF-1 |
| D2-6.F12 | Identity-envelope gate (signed envelope overrides gateway headers) | 🟡 | Compose `WORKFLOW_IDENTITY_VERIFY` default `warn` | X5 |
| D2-6.F13 | ETag/If-Match on 11 mutations (7 instance, 4 definition) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D2-6.F14 | Audit engagement module: `POST /v1/audit/engagements`, `GET …/{id}`, acceptance (`submit-acceptance`, `acceptance-decision`), `activate`, `amend-scope`, `withdraw`, `mark-fieldwork-complete`, `enter-completion-review`, `mark-report-ready`, `close`, `GET …/completion-gates`; plans `GET/POST …/plan`, `POST /v1/audit/plans/{id}/approve|materiality`, `GET/POST …/risks`; risks `POST /v1/audit/risks/{id}/assess|mark-significant|response|assertions`; workpapers `POST …/workpapers`, `GET /v1/audit/workpapers/{id}`, `POST …/procedures|results|conclusions|evidence-links|cross-references|addenda|mark-prepared|lock`; reviews `POST …/reviews`, `…/reviews/quality`, `GET …/reviews/release-gates`, `POST /v1/audit/review-scopes/{id}/reviewers|notes|sign-off`, `POST /v1/audit/review-notes/{id}/respond|resolve`, `POST /v1/audit/sign-offs/{id}/withdraw`, `POST /v1/audit/quality-reviews/{id}/complete` | 🔵 | Built; not in the Group 2 audit | WF-7 |
| D2-6.F15 | Forms module: `POST /v1/forms`, `GET …/{id}`, `POST …/publish|retire|drafts`, `GET …/pending-submissions`; submissions `GET /v1/form-submissions/{id}`, `POST …/drafts|validate|submit|supersede|route`, `GET …/validation-result`, `GET …/version` | 🔵 | Built; not in the Group 2 audit | WF-7 |
| D2-6.F16 | WFC-04 cases (WF-2) | ❌ | Not built | WF-2 |
| D2-6.F17 | ZS-STATE state-machine registry (ST-1), available actions + transition history (ST-2), no generic status writes (ST-3) | ❌ | Not built | ST-1, ST-2, ST-3 |
| D2-6.F18 | The 25 partial rows: quorum stage lock and `row_version` (L-2); RLS on stages, transitions, quorum tables, outbox, definitions (L-8); per-type approve authority (Q-6); split state model, common columns, event naming (Q-3) | 🟡 | Governance audit, workflow sections (14, 19, 21 Oct) | WF-1 |
| D2-6.F19 | Business-object owners call `/invalidate` or emit change events (H-5); invoice-approval-svc calls a path that does not exist (N-3) | 🟡 | Other teams' services | Cross-team |
| D2-6.F20 | Reporting-line and team data for approver independence (H-6) | ⛔ | Needs org-structure data (ORG-05, teammate's org-structure-svc) | Cross-team |
| D2-6.F21 | Decisions Q-1…Q-6 (WFC scope, binding API surface C4, event catalogue, consumed-event behaviour, service-account vs human approver, approve action codes) | 🧭 | Governance audit register | Decision |
| D2-6.F22 | Cursor paging (API-4) on pending, definitions, form lists | ❌ | — | WF-5 |
| D2-6.F23 | Root-context Docker image build, dependency E2E, console v2.0 | ❌ | Not run; no `e2e-workflow-deps.py`; legacy page | WF-8, WF-FE-1…3 |

**Tasks**

- [ ] **WF-0 · Baseline (S0):** remove the untracked `tmp/` and `workflow_handler_test.bin`; matrix in `progress.md` (Doc 03 §8.4, GOV-06, WFC-01…04, ZS-STATE, the 61 audit rows, audit and forms modules); suite with counts.
- [ ] **WF-1 · Close WFC-01…03 open rows** (IMPLEMENTATION-PLAN §6 WF-1): quorum races, RLS on six tables, definition version pinning, state model, common columns, per-type approve authority. Each row re-scored with evidence.
- [ ] **WF-5 · Handler tests and API-4:** ROLE stages, definition expansion, escalation-target approval, registry 404/503, Idempotency-Key replay/mismatch, identity-gate override; cursor paging on lists.
- [ ] **WF-6 · Global `policy.*` events** re-check every tenant (bounded fan-out, test).
- [ ] **WF-7 · Audit the audit-engagement and forms modules:** authorization at the object (engagement team membership, reviewer ≠ preparer, sign-off independence), If-Match, Idempotency-Key, events, RLS, R5, openapi + parity. Tests `TestAUD_*`, `TestFORM_*`.
- [ ] **WF-2 · WFC-04 cases** (§6 WF-2).
- [ ] **ST-1 / ST-2 / ST-3 · State machines** (§6). ST-3 covers identity-context's `PUT /v1/principals/{id}/status` too.
- [ ] **WF-8 · Live:** build the image from the root context; `scripts/audit.sh`; new `deployments/scripts/e2e-workflow-deps.py` (registry status, authorization approve and delegation, decision-log relay, policy event re-check, invalidate from a business object).

**Console (frontend)** — `app/admin/workflows` (legacy) · clients `lib/api/workflow.ts`, `lib/api/workflow-history.ts` call 19 of 76 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. Audit engagements and forms are large: give each its own tab file and drawer forms.

| Part | Content |
|---|---|
| Header | "Governance › Workflows & Approvals"; gold primary "Start workflow"; secondary "My pending approvals" |
| KPIs (from new `GET /v1/workflows/summary`) | Pending my approval · Active instances · Escalated · Expiring in 24 h · Active definitions |
| Panels | Pipeline: Created → In approval → Approved / Rejected → Released. Ordered list: stages of the selected definition, with the approver rule chip. Bar list: instances by workflow type, with an amber callout for escalations and expiring instances |
| Tabs | Pending approvals (F02) · Instances (F01–F05) · Definitions (F11) · Audit engagements (F14) · Forms (F15) · Cases (WF-2, when built) |
| Drawer commands | Approve / reject (reason, step-up; never shown to the requester), request changes, quorum vote, escalate / escalate-to, cancel, invalidate, verify-release. Definition supersede / activate / suspend / retire. Engagement lifecycle (acceptance, activate, amend scope, fieldwork complete, completion review, report ready, close, withdraw), plan / materiality / risks / assertions, workpapers (procedures, results, conclusions, evidence links, cross-references, addenda, mark prepared, lock; preparer ≠ reviewer), review notes, sign-offs, quality reviews. Form publish / retire / drafts; submission validate / submit / route / supersede |
| Footer | Required controls: approver independence · one vote per approver · every decision recorded in the decision log · a reason on every action |

**Tasks**
- [ ] **WF-FE-1 · Summary route + API client** for every route in Appendix B D2-6.
- [ ] **WF-FE-2 · Page v2.0.**
- [ ] **WF-FE-3 · UI E2E** `tests/e2e/console/workflows.spec.ts`. Cover:
  - start → approved by a different user → decision id shown;
  - the requester never sees Approve;
  - quorum vote counted once;
  - an engagement workpaper prepared by one user and reviewed by another;
  - a form submitted and routed;
  - empty and unavailable states.

**Blocked and decisions:** H-6 org data; Q-1…Q-6; H-5 / N-3 (cross-team).

---

## D2-7 · evidence-requirements-svc (:8130, DB `evidence_requirements`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.6 |
| Latest score | 39 met / 1 partial / 3 failed of 43 on 2 Nov; 5 Nov completion pass closed the in-service rows (Governance audit → "evidence-requirements-svc completion, 5 November 2026") |
| Stages | S0–S6 verified 5 Nov: `scripts/audit.sh` 42/42, dependency E2E 84/84 (3 owner-blocked skips); PBC object authorization added and verified (`0e11a571`) Reconfirm in VC-1 |
| Console | `/admin/evidence` v2.0 (5 Nov) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-7.F01 | Evaluate: `POST /v1/evidence/evaluate` (SATISFIED / MISSING / NO_REQUIREMENTS_DEFINED; fails closed on a malformed rule); authorization `EVIDENCE_EVALUATE` | 🟡 | Reconfirm (was ✅): progress.md ER matrix; E2E 84/84 | Compose still `EVIDENCE_EVALUATE_AUTHZ` default `warn` (D-ER-6): ER-W8; VC-1 |
| D2-7.F02 | Evaluation reads: `GET /v1/evidence/evaluations` (API-4), `GET …/{id}`, `GET /v1/evidence/summary` | 🟡 | Reconfirm (was ✅): ER-3 | VC-1 |
| D2-7.F03 | Requirement catalogue: `GET /v1/evidence-requirements` (API-4), `GET …/{id}` (ETag), `POST /v1/admin/evidence-requirements` (idempotent), `POST …/{id}/end-date` (If-Match) | 🟡 | Reconfirm (was ✅): ER-3, ER-4 | VC-1 |
| D2-7.F04 | Supporting-artifact rules (subtype, minimum count); distinct artifacts by canonical reference | 🟡 | Reconfirm (was ✅): `TestER02_SameManifestTwiceCountsOnce` | VC-1 |
| D2-7.F05 | Append-only decision ledger; outbox `evidence.requirement.satisfied` / `.missing` proven on Kafka | 🟡 | Reconfirm (was ✅): 5 Nov E2E §9; migration 000011 | Schemas not registered: ER-W5; VC-1 |
| D2-7.F06 | PBC requests (14 routes): `GET/POST /v1/pbc/requests`, `GET …/{id}`, `POST …/send|clarify|extend-due-date|reassign|mark-satisfied|close|responses|view|notes`, `POST /v1/pbc/responses/{id}/acknowledge|scan-result` | 🟡 | Reconfirm (was ✅): ER-4; If-Match on manager commands | VC-1 |
| D2-7.F07 | PBC object authorization (manager or assignee; view by assignee only; AUDIT_ONLY notes by manager; acknowledge needs MANAGE) (D-ER-29) | 🟡 | Reconfirm (was ✅): `TestPBCAuthz_*` (5 tests); 2 mutants caught | E2E section and docs: ER-W6, ER-W7; VC-1 |
| D2-7.F08 | Document verification against document-vault-svc (envelope forwarded) | 🟡 | Reconfirm (was ✅): `internal/documentvault/client.go`; live E2E §6 used a fixture row | Real upload through the vault: ER-W6 (needs ER-W4); VC-1 |
| D2-7.F09 | Signature verification against esignature-integration-svc | 🟡 | `internal/esignature/client.go`; live SATISFIED not shown: the service runs only in the phase7 stack | ⛔ phase7 stack |
| D2-7.F10 | EVIDENCE_MANIFEST references checked against evidence-manifest-svc | 🟡 | Code done; a real manifest cannot count until 000047 is applied (D1-9.F05) | ER-W3, ER-W6 |
| D2-7.F11 | Callers log NO_REQUIREMENTS_DEFINED (7 services, D-ER-5) | 🟡 | Reconfirm (was ✅): `TestEvaluate_NoRequirementsDefinedPassesWithAWarning` in each caller (`03f61a87`) | VC-1 |
| D2-7.F12 | general-ledger gates every finalization path | 🟡 | Only `PostJournal` is gated; `PostApprovedJournal`, `PostAccountingEvent`→`commitJournal`, `ReprocessFailedPosting` are not | Cross-team |
| D2-7.F13 | Callers forward the bearer envelope (identity-envelope `idenv.Forward`) | ❌ | No caller does | Cross-team, then ER-W8 |
| D2-7.F14 | Decisions: tier (D-ER-1), scan-result ownership (D-ER-2), which document/signature statuses count (D-ER-3), GOV-07 linkage | 🧭 | SPEC_DEVIATIONS | Decision |
| D2-7.F15 | Console `/admin/evidence` v2.0 | 🟡 | Manager side verified 5 Nov (ER-FE-2). The assignee side (view, respond, notes) and response acknowledge are not in the console (Appendix B) | ER-FE-3, ER-FE-4 |

**Tasks** (W3–W7 of the 5 Nov plan that were not run)

- [ ] **ER-W3 · 000047 proof:** covered by AC-0 (store suite, up/down/up, apply to the dev DB). Then a real manifest counts towards SATISFIED.
- [ ] **ER-W4 · Compose fixes:** document-vault `DB_USER: "${DB_USER_DOCUMENT_VAULT:-app_document_vault}"` with the password from `create-app-roles.sh`; `CLAMAV_URL`; a `clamav/clamav:1.4.1` service with `profiles: ["scan"]`, `mem_limit` and a clamd healthcheck; gateway admin token init container (same as GW-9). Check with `docker compose -p deployments config`.
- [ ] **ER-W5 · Register event schemas:** `services/evidence-requirements-svc/schemas/evidence.requirement.{satisfied,missing}.v1.json` from `asyncapi.yaml` 1.1.0; `deployments/scripts/register-evidence-requirements-schemas.py` (fixed Idempotency-Key per event + version; the principal that holds schema-publish).
- [ ] **ER-W6 · E2E:** remove the manifest and upload skips (real vault upload with clamd; real manifest); add a PBC authorization section (outsider 403 on GET/view/notes/acknowledge; assignee GET + view; AUDIT_ONLY by assignee 403, by manager 201; outsider view leaves the ETag unchanged); add a schema-registry section. Only the esignature skip may remain.
- [ ] **ER-W7 · Docs:** `progress.md`, `context.md` §4 and §9 rewritten from the code, `SPEC_DEVIATIONS.md` (D-ER-27 amended, D-ER-29, D-ER-5 caller logging, D-ER-24), `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`; re-score Governance audit rows 9/10/11/21 with file:line.
- [ ] **ER-W8 · Enforce:** once callers send envelopes (F13), set `EVIDENCE_EVALUATE_AUTHZ` and `EVIDENCE_IDENTITY_VERIFY` to enforce/require in compose and prove a header-only call is refused.

**Console (frontend)** — `app/admin/evidence` (v2.0, 5 Nov) · client `lib/api/evidence.ts` calls 18 of 22 routes (Appendix B). The manager side exists. The assignee side is missing.

**Tasks**
- [ ] **ER-FE-3 · Assignee side:**
  - "My evidence requests": the register filtered to requests assigned to the signed-in principal.
  - Opening a request records the view (`POST …/view`; assignee only).
  - Respond: upload through document-vault-svc, then `POST …/responses`.
  - SHARED notes (`POST …/notes`).
  - Manager: Acknowledge on a response (`POST /v1/pbc/responses/{id}/acknowledge`).
- [ ] **ER-FE-4 · UI E2E** `tests/e2e/console/evidence.spec.ts`. Cover:
  - manager creates and sends;
  - the assignee views, responds and notes;
  - the manager acknowledges and marks satisfied;
  - an outsider cannot open the request;
  - evaluate shows SATISFIED;
  - empty and unavailable states.

**Blocked and decisions:** esignature live (phase7 stack); GL ungated paths and bearer forwarding (cross-team); D-ER-1/2/3.

---

# Supporting units in scope (rule R1): governance supporting and Security & Trust

These ten units are ours under R1 (Reference Part A1) and are needed for Domain 1 and 2 to be finished: the schema registry, retention and legal hold, the shared libraries and the GTRM compiler, and the four Security & Trust services that the auth plane depends on. Sources:
- `Evidence, Audit & Utility.md` (teammate's audit, 21 Oct): schema-registry-svc, retention-registry-svc and search-client sections.
- `Security & Trust.md` (teammate's audit, 12–13 Oct, latest scores §14.7).
- Reference Part A5: G-02, G-08, G-09, G-10, SEC-1…SEC-9.
- The code and the 6 Nov build/vet/test sweep.

Both teammate audits score only what each service's own documents promise. The spec sections the plan places in these units (GOV-08, GOV-09, GOV-10, Security Standard §10–§26) were never scored, so they appear below as ❌ rows.

## D2-8 · schema-registry-svc (:8093, DB `schema_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-08 (ZS-SVC-A-001); Event Catalogue standard; plan task G-08 |
| Latest score | 21 Oct (Evidence, Audit & Utility §2): 12 ✅, 0 partial, 0 missing on its own documented contract; 11 ❓; 3 external blockers. GOV-08 lifecycle not scored |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (5 packages) |
| Docs | `openapi.yaml` only: no `progress.md`, README, RUNBOOK, RELEASE_CERTIFICATE or SPEC_DEVIATIONS |
| Console | `/admin/schemas` legacy; client `lib/api/schemas.ts` calls 5 of 5 routes |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-8.F01 | Register a schema version: `POST /v1/schemas/{eventName}/versions` (compatibility check, idempotency, version assignment, immutability) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §2 (21 Oct) | VC-1 |
| D2-8.F02 | Reads: `GET /v1/schemas`, `GET /v1/schemas/{eventName}/versions`, `…/versions/latest`, `…/versions/{version}` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §2 | API-4 on the list: SR-2; VC-1 |
| D2-8.F03 | GOV-08 lifecycle: RegisterDraftSchema → RunCompatibilityCheck → ApproveSchema (maker-checker) → PublishSchema → DeprecateSchema → RetireSchema | ❌ | Not built | G-08 |
| D2-8.F04 | GOV-08 queries: GetActiveSchema, CompareSchemas, GetCompatibilityReport | ❌ | Not built | G-08 |
| D2-8.F05 | Consumer registration and ListConsumers | ❌ | Not built | G-08 |
| D2-8.F06 | Schemas other tasks need: IAM-10 canonical identity events, ER-W5 evidence-requirements events, EML-4 email parent events | ❌ | None registered | SR-3 (after G-08) |
| D2-8.F07 | 11 points to clarify: read permission, replay-key binding, replay marker, SchemaDependencyMap, NONE rollout approval, JSON Schema keywords compared, artifact fields, runtime payload validation, 1 MiB body limit owner, Event Catalogue compatibility rules, required artifacts; plus tenant visibility of schemas | 🧭 | Evidence, Audit & Utility §2; "Carried over" table | CL-2 |
| D2-8.F08 | Live authorization: the actions it checks are registered in access-control and granted; envelope `require` | 🟡 | 21 Oct audit listed this as external; authorization-svc now runs | SR-1, EST-1 |
| D2-8.F09 | Service documents: `progress.md` matrix, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman | ❌ | Missing | SR-0, SR-4 |
| D2-8.F10 | Dependency E2E and console v2.0 | ❌ | No `e2e-schema-registry-deps.py`; legacy page | SR-5, SR-FE-1…3 |

**Tasks**

- [ ] **SR-0 · Baseline (S0):** `progress.md` with a matrix covering GOV-08 rows, the 12 documented rows and the 11 ❓. Integration suite on scratch Postgres (0 skip).
- [ ] **SR-1 · Grants:** every action the handlers check is registered in access-control and carried by a template (R5). Store test.
- [ ] **G-08 · Lifecycle, queries and consumers** (Reference Part A5 G-08). Keep the existing `/{eventName}/versions` routes working. Tests:
  - `TestG08_IncompatiblePublishWithoutApprovalRefused`;
  - `TestG08_RetiredSchemaCannotBeRegisteredAgainst`;
  - `TestG08_ApproverIsNotTheAuthor`;
  - `TestG08_ConsumerListedAfterRegistration`.
- [ ] **SR-2 · API-3 / API-4:** If-Match on the lifecycle commands; cursor paging on lists.
- [ ] **SR-3 · Register the platform's schemas:** identity canonical names (IAM-10), evidence-requirements (ER-W5), and later EML-4. One script per producer, idempotent.
- [ ] **SR-4 · Contract and docs (S2):** openapi with a parity test, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman.
- [ ] **SR-5 · Live (S3/S4):** rebuild, `scripts/audit.sh`, and a new `deployments/scripts/e2e-schema-registry-deps.py`. It covers:
  - the authorization decision;
  - publishing an incompatible schema refused;
  - a consumer listing;
  - a producer's event validated against `versions/latest`.

**Console (frontend)** — `app/admin/schemas` (legacy) · client `lib/api/schemas.ts`. Rebuild in v2.0 following "Frontend rules for every console".

| Part | Content |
|---|---|
| Header | "Governance › Schemas & Contracts"; gold primary "Register draft"; secondary "Compare versions" |
| KPIs (new `GET /v1/schemas/summary`) | Published event types · Drafts awaiting approval · Deprecated · Incompatible submissions (30 d) · Registered consumers |
| Panels | Pipeline: Draft → Checked → Approved → Published → Deprecated → Retired. Ordered list: versions of the selected event. Bar list: consumers per event, with an amber callout for consumers on a deprecated version |
| Tabs | Event types (F01, F02) · Drafts & approvals (F03) · Compatibility (F04) · Consumers (F05) |
| Drawer commands | Run check, approve (≠ author), publish, deprecate, retire, register consumer |
| Footer | Required controls: no publish without a passing check or an approval · published versions are immutable · retired schemas accept no registrations |

- [ ] **SR-FE-1 · Summary route + API client** for every G-08 route.
- [ ] **SR-FE-2 · Page v2.0.**
- [ ] **SR-FE-3 · UI E2E** `tests/e2e/console/schemas.spec.ts`. Cover:
  - draft → check → approve by a second user → publish;
  - an incompatible draft is blocked;
  - empty and unavailable states.

**Blocked and decisions:** the 11 ❓ items and schema visibility (CL-2).

## D2-9 · retention-registry-svc (:8148, DB `retention_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-09 retention and disposition, GOV-10 legal hold (ZS-SVC-A-001); ZS-SVC-S-001; ZS-DATA-GOV-001; plan tasks G-09, G-10 |
| Latest score | 21 Oct (Evidence, Audit & Utility §7): 11 ✅ on its own documented contract; 9 ❓; 2 external blockers. GOV-09 and GOV-10 not scored |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (4 packages) |
| Docs | none: no `openapi.yaml`, `progress.md`, README or RUNBOOK |
| Console | `/admin/retention` legacy; client `lib/api/retention.ts` calls 7 of 7 routes |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-9.F01 | Retention policies: `POST /v1/retention-policies`, `GET /v1/retention-policies` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Becomes G-09 rules; VC-1 |
| D2-9.F02 | Resolve retention: `GET /v1/retention/resolve` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | VC-1 |
| D2-9.F03 | Legal holds: `POST /v1/legal-holds`, `GET /v1/legal-holds`, `GET /v1/legal-holds/{id}`, `POST /v1/legal-holds/{id}/release` (append-only release) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Extended by G-10; VC-1 |
| D2-9.F04 | Outbox delivery, forced RLS, body tenant validation | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Kafka proof: X3; VC-1 |
| D2-9.F05 | GOV-10 legal hold, complete:<br>• scope by subjects, record classes, date range, custodians;<br>• amend scope; add/remove custodian;<br>• release with maker-checker (the issuer cannot release);<br>• reconcile coverage;<br>• queries EvaluateHoldCoverage, ListHeldRecords, GetPropagationStatus;<br>• events `legal_hold.*` | ❌ | Not built | G-10 |
| D2-9.F06 | GOV-09 retention and disposition:<br>• versioned rules with the period from jurisdiction-rules `POST /v1/retention-rules:resolve`;<br>• schedule, approve (maker-checker), execute (API-5), reperform, suspend;<br>• eligibility that always checks holds;<br>• signed disposition certificate | ❌ | Not built | G-09 |
| D2-9.F07 | Hold consumers: identity-context GOV-10 projection; authorization denies destructive actions on held subjects (IAM-3); document-vault (cross-team) | 🟡 | identity-context consumes holds; IAM-3 not built; document-vault is a teammate's | G-10, EST-8, CL-4 |
| D2-9.F08 | 9 points to clarify: read grant for one hold, release-check order, min/max retention relationship, correlation-id precedence, publisher failure behaviour, read authorization on lists, ZS-DATA-GOV-001 schedule fields, hold scope/version/review-date semantics, error body | 🧭 | Evidence, Audit & Utility §7 | CL-2 |
| D2-9.F09 | Contract and docs: `openapi.yaml` with parity test, `asyncapi.yaml`, `progress.md`, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman | ❌ | None exist | RR-0, RR-3 |
| D2-9.F10 | API-3 If-Match and API-4 cursor paging | 🔵 | Audit says "pagination" matched; cursor and If-Match not verified | RR-2 |
| D2-9.F11 | Dependency E2E and console v2.0 | ❌ | No `e2e-retention-registry-deps.py`; legacy page | RR-4, RR-FE-1…3 |

**Tasks**

- [ ] **RR-0 · Baseline (S0):** `progress.md` matrix (GOV-09, GOV-10, the 11 documented rows, the 9 ❓); integration suite (0 skip).
- [ ] **G-10 · Legal hold** first (Reference Part A5 G-10; IAM-3 and G-09 depend on it). Tests:
  - `TestG10_IssuerCannotRelease`;
  - `TestG10_DispositionOverHeldSubjectRefused`;
  - `TestG10_CoverageEvaluated`;
  - `TestG10_PropagationStatusListsAcks`.
  identity-context's projection test must consume the new event.
- [ ] **G-09 · Retention and disposition** (Reference Part A5 G-09). Tests:
  - `TestG09_EligibilityFalseUnderHold`;
  - `TestG09_ApproverIsNotScheduler`;
  - `TestG09_CertificateVerifies`;
  - `TestG09_ExecutionIsAsync` (API-5).
- [ ] **RR-1 · Grants (R5)** for every new action; template `RECORDS_MANAGER` (and a legal-hold officer template).
- [ ] **RR-2 · API-3 / API-4** on policies, holds and dispositions.
- [ ] **RR-3 · Contract and docs (S2):** write `openapi.yaml` + parity test, `asyncapi.yaml`, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman.
- [ ] **RR-4 · Live (S3/S4):** rebuild, `scripts/audit.sh`, and a new `deployments/scripts/e2e-retention-registry-deps.py`. It covers:
  - jurisdiction-rules retention resolve;
  - authorization;
  - a hold event consumed by identity-context, and by authorization once IAM-3 is built;
  - a disposition refused under a hold;
  - Kafka events.

**Console (frontend)** — `app/admin/retention` (legacy) · client `lib/api/retention.ts`. Rebuild in v2.0.

| Part | Content |
|---|---|
| Header | "Governance › Retention & Legal Hold"; gold primary "Issue legal hold"; secondary "Resolve retention" |
| KPIs (new `GET /v1/retention/summary`) | Active holds · Custodians under hold · Dispositions due (30 d) · Dispositions awaiting approval · Holds not acknowledged by a consumer |
| Panels | Pipeline: Scheduled → Approved → Executing → Certified (disposition). Ordered list: the selected hold's custodians and scope. Bar list: records held per record class, with an amber callout for unacknowledged propagation |
| Tabs | Legal holds (F03, F05) · Retention rules (F01, F06) · Dispositions (F06) · Resolve (F02) · Propagation (F07) |
| Drawer commands | Amend scope, add/remove custodian, release (never shown to the issuer), schedule / approve (≠ scheduler) / execute / suspend disposition, download certificate |
| Footer | Required controls: the issuer never releases · no disposition over a held subject · disposition approval by a second person · a certificate for every disposition |

- [ ] **RR-FE-1 · Summary route + API client** for every G-09/G-10 route.
- [ ] **RR-FE-2 · Page v2.0.**
- [ ] **RR-FE-3 · UI E2E** `tests/e2e/console/retention.spec.ts`. Cover:
  - a hold issued and released by a second user;
  - a disposition refused under a hold;
  - a disposition approved and certified;
  - empty and unavailable states.

**Blocked and decisions:** the 9 ❓ items (CL-2); document-vault as a hold consumer (cross-team, CL-4).

## D2-10 · Shared libraries and the GTRM compiler (identity-envelope, search-client, _contract, deployments/gtrm)

**Facts**

| Item | Value |
|---|---|
| Spec | identity-envelope: GOV-01 §4 envelope, ZS-API-001 contract helpers. search-client: ZS-SVC-AB-001 (via search-indexer). gtrm: GOV-02 and `docs/architecture/global-traffic-residency-manager-decision.md` |
| Latest score | search-client 21 Oct (Evidence, Audit & Utility §10): 13 ✅, 8 ❓, 3 external. The others are not audited as units |
| Suite 6 Nov | identity-envelope ✔ (2 pkgs); search-client ✔ (1); `_contract` ✔ (19); `deployments/gtrm/compiler` ✔ (1). `deployments/gtrm` fails `vet`/`test` with "matched no packages": the untracked `deployments/gtrm/go.mod` ("Do first" item 2) |
| Console | none (libraries) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-10.F01 | identity-envelope gate (`NewGateFromEnv`), forwarder and token exchanger, apicontract (problem details, cursor, ETag helpers) | 🟡 | Reconfirm (was ✅): Used by verified services (authorization 2 Nov, evidence-requirements 5 Nov); suite green 6 Nov | VC-1 |
| D2-10.F02 | Strict per-service audience (`IDENTITY_ACCEPT_LEGACY_AUDIENCE=false`) | ❌ | Waits for GW-5 | EST-2 |
| D2-10.F03 | identity-envelope and `_contract` docs (README, what each package guarantees, versioning) | ❌ | No README | LIB-1 |
| D2-10.F04 | search-client: governed client (tenant requirements, strict mappings, mandatory filters, paging, projections, vector/hybrid, Ping) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §10 (21 Oct) | VC-1 |
| D2-10.F05 | search-client against a real OpenSearch: integration tests on a local OpenSearch container | 🟡 | Listed as external (cluster); no local run recorded | LIB-2 |
| D2-10.F06 | search-client: 8 ❓ (restriction epoch, legacy client scope, page-size cap, DropIndex guards, environment defaults, cursor tie-break, refresh policy, query budgets) | 🧭 | Evidence, Audit & Utility §10 | CL-2 |
| D2-10.F07 | `_contract` shared contract and test kit (19 packages) | 🔵 | Suite green 6 Nov; never audited | LIB-3 |
| D2-10.F08 | GTRM compiler: routing-map validation gates and `--check` | 🔵 | `deployments/gtrm/EVIDENCE.md`; compiler tests green 6 Nov | LIB-4 |
| D2-10.F09 | GTRM compiler commands (G-02): `activate-policy`, `schedule-change`, `failover initiate|complete|abort`, `mark-region-unavailable` with provenance and evidence | ❌ | Not built | G-02 |
| D2-10.F10 | Stray `deployments/gtrm/go.mod` (empty module) and the modified `deployments/gtrm/compiler/go.mod` | 🟡 | `git status` 6 Nov | "Do first" item 2, then LIB-4 |

**Tasks**

- [ ] **LIB-1 · Docs:** READMEs for identity-envelope and `_contract` (packages, guarantees, settings such as `IDENTITY_VERIFY` and `IDENTITY_AUDIENCE`, upgrade rules). A `CHANGELOG.md` per library, because every service imports them.
- [ ] **LIB-2 · search-client live:** a compose `opensearch` service under a profile; integration tests tagged `integration` run against it.
- [ ] **LIB-3 · Audit `_contract`:** list what it provides and who imports it; remove dead packages; record the result in the matrix.
- [ ] **LIB-4 · GTRM gates:** every compiler gate has a test; `--check` passes on Windows.
- [ ] **G-02 · Compiler commands** (Reference Part A5 G-02) together with the registry reads that tenant-entity-registry gets in its session. Test `TestG02_FailoverOutsideResidencyRejected`.

## D1-10 · mtls-management-svc (:8140)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard (ZS-SEC-001) §8.6 / §9 mTLS and workload identity; `03-microservices.md`; plan task SEC-6 |
| Latest score | `Security & Trust.md` §14.7 (13 Oct): FE 25%, BE 92%, integration 85%, overall 67%, Not Ready. Open items §14.6 |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (5 packages) |
| Deploy | main compose (and phase6). Certificates and policies are held in memory; revocations persist |
| Console | shared `/admin/security` page; its panels call routes that do not exist; `lib/api/security-trust.ts` is imported by no file |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-10.F01 | Certificates: `POST /v1/mtls/certificates`, `GET /v1/mtls/certificates`, `GET /v1/mtls/certificates/{id}`, `POST /v1/mtls/certificates/{id}/rotate`, `DELETE /v1/mtls/certificates/{id}` (revoke); real CA | 🟡 | Reconfirm (was ✅): Security & Trust §7, §14 | VC-1 |
| D1-10.F02 | CRL: `GET /v1/mtls/crl` (revocations persist) | 🟡 | Reconfirm (was ✅): Security & Trust §14.2 (MT-1) | VC-1 |
| D1-10.F03 | Bootstrap self-provisioning | 🟡 | Reconfirm (was ✅): Security & Trust §13.1 (resolved 12 Oct) | Tenant metadata from the token only: MT-3; VC-1 |
| D1-10.F04 | CSR signing: `POST /v1/mtls/certificates/csr` (the private key never leaves the workload) | 🔵 | Route exists; not covered by the 13 Oct pass | SEC-6 |
| D1-10.F05 | URI SAN `spiffe://zoiko/<env>/<service>` (S4-C1) | ❌ | Not built | SEC-6 |
| D1-10.F06 | Persistence of certificates and policies (PER-1) | ❌ | In memory | SEC-6 |
| D1-10.F07 | Automated rotation; `EXPIRED` status set (ROT-1) | ❌ | `auto_rotate` stored, never read | MT-1 |
| D1-10.F08 | mTLS policies: `GET /v1/mtls/policies`, `POST /v1/mtls/policies` | 🟡 | `CreatePolicy` error handling flagged (§12) | MT-2 |
| D1-10.F09 | `MTLS_*` actions and `iam.workload.manage` registered and templated (X-RBAC) | 🟡 | §14.6 X-RBAC | MT-2 |
| D1-10.F10 | `/readyz` (RDY-1) | ❌ | 404 | MT-2 |
| D1-10.F11 | Maker-checker on RevokeCert (MC-1) | 🧭 | §11.7, §14.6 | CL-2 |
| D1-10.F12 | Every PEP presents a client certificate so authorization can run `require` (M-02) | 🟡 | §14.6 M-02 | EST-1 |
| D1-10.F13 | Contract, docs, E2E | 🟡 | `openapi.yaml` exists; no `progress.md`, RUNBOOK or E2E | MT-4 |

**Tasks**

- [ ] **MT-0 · Baseline (S0):** `progress.md` matrix from the Security & Trust audit §7, §13, §14 and SEC-6.
- [ ] **SEC-6 · CSR signing, URI SAN, Postgres persistence** (Reference Part A5 SEC-6; new DB, role and DSN under R4). Tests:
  - `TestSEC6_CSRIssuedKeyNeverLeavesWorkload`;
  - `TestSEC6_URISANPresent`;
  - `TestSEC6_RevocationSurvivesRestart`.
- [ ] **MT-1 · Automated rotation:** a ticker (`context.WithoutCancel` + timeout) rotates certificates with `auto_rotate` before `not_after − margin`, and marks expired ones `EXPIRED`; metrics and events. Test with a frozen clock.
- [ ] **MT-2 · Fixes:**
  - `CreatePolicy` validation and problem codes;
  - register `MTLS_*` and `iam.workload.manage` in access-control (R5);
  - add `/readyz`.
- [ ] **MT-3 · Bootstrap tenant (BS-1):** take the tenant and legal entity from the bootstrap token's binding, never from caller headers.
- [ ] **MT-4 · Contract, docs, live:**
  - openapi parity, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS;
  - a new `deployments/scripts/e2e-mtls-management-deps.py`: issue by CSR, a peer refuses a revoked certificate via the CRL, rotation, authorization decisions.

**Console (frontend):** the shared Security page.

`/admin/security` (legacy): its four panels fetch `/api/v1/security/*` routes that do not exist, and the backend-accurate client `lib/api/security-trust.ts` is imported by no file. This session builds the v2.0 shell. Each Security & Trust session adds its own tabs.

| Part | Content |
|---|---|
| Header | "Security › Security & Trust"; gold primary "Issue certificate"; secondary "Evaluate access risk" |
| KPIs (one summary per service, read in parallel) | Active certificates · Expiring in 14 days · Keys active · Open security incidents · Risk decisions DENY/ISOLATE (24 h) |
| Panels | Pipeline: certificate Requested → Issued → Rotated → Revoked/Expired. Ordered list: the trust chain (root → intermediate → workload). Bar list: certificates expiring per service, with an amber callout for certificates without auto-rotation |
| Tabs (this session) | Certificates (F01–F05) · mTLS policies (F08) · Revocations / CRL (F02) |
| Footer | Required controls: private keys never leave the workload · revocations distributed by CRL · every command has a reason and is audited |

- [ ] **ST-FE-1 · API client:** make `lib/api/security-trust.ts` server-only and route it through `lib/api/envelope.ts`. Delete the dead `/api/v1/security/*` fetches and the orphaned `CartaCapTablePanel.tsx`.
- [ ] **ST-FE-2 · Page v2.0 shell + mTLS tabs.**
- [ ] **ST-FE-3 · UI E2E** `tests/e2e/console/security.spec.ts` (mTLS part). Cover:
  - issue, rotate and revoke;
  - the CRL shows the revocation;
  - empty and unavailable states.

**Blocked and decisions:** MC-1 (CL-2); production CA hierarchy and Traefik `serversTransport` certificates (production backlog).

## D1-11 · siem-integration-svc (:8141)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §16, §22, §25; `phase-6-trust-intelligence.md`; plan task SEC-3 |
| Latest score | `Security & Trust.md` §12: BE 55%, integration 20%, overall 27%, Not Ready. Its named purpose (forwarding to a SIEM) does not exist |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (1 package) |
| Deploy | `docker-compose.phase6.yml` only; in-memory store |
| Console | Security page (dead-wired, see D1-10) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-11.F01 | Exporters: `GET /v1/siem/exporters`, `POST /v1/siem/exporters`, `GET /v1/siem/exporters/{id}` | 🟡 | In memory; the third-party `AuthToken` is stored in plaintext | SEC-3, SG-1 |
| D1-11.F02 | Events: `POST /v1/siem/stream` (ingest), `GET /v1/siem/events` | 🟡 | In memory; two ingest routes deliberately ungated (accepted residual) | SEC-3 |
| D1-11.F03 | Postgres store + outbox; a restart loses no event | ❌ | In memory | SEC-3 |
| D1-11.F04 | Schema validation and redaction (§16 "must not contain") | ❌ | Not built | SEC-3 |
| D1-11.F05 | Forwarders: RFC 5424 syslog over TLS and HTTPS JSON (HEC/Elastic), retry, backoff, dead-letter queue | ❌ | No outbound client | SEC-3 |
| D1-11.F06 | Security exceptions: `POST /security/exceptions` (time-bound, approved) | ❌ | Not built | SEC-3 |
| D1-11.F07 | Incident containment: `POST /security/incidents/{id}:contain` (end sessions, revoke certificates, suspend keys; approval required) | ❌ | Not built | SEC-3 |
| D1-11.F08 | Evidence by correlation: `GET /security/evidence/{correlation_id}` | ❌ | Not built | SEC-3 |
| D1-11.F09 | Exporter credentials leased from secret-vault, never stored in plaintext | ❌ | Plaintext | SG-1 |
| D1-11.F10 | Running in the main compose with healthchecks; every producer's async SIEM client reaches it | ❌ | phase6 only | SG-2 |
| D1-11.F11 | Vendor-specific SIEM configuration (Splunk, Datadog, Elastic, Sentinel tenancy) | ⛔ | Vendor choice and accounts | Production backlog |

**Tasks**

- [ ] **SG-0 · Baseline (S0):** `progress.md` matrix (Security & Trust audit rows, SEC-3).
- [ ] **SEC-3 · siem-integration-svc** (Reference Part A5 SEC-3; new DB under R4). Tests:
  - `TestSEC3_RestartLosesNoEvent`;
  - `TestSEC3_SecretRedactedBeforeStorage`;
  - `TestSEC3_ForwardRetriesUntilSyslogSinkReceives` (local syslog sink container);
  - `TestSEC3_ContainmentNeedsApproval`.
- [ ] **SG-1 · Credentials:** exporter secrets leased from secret-vault (`secret://`), never stored or logged.
- [ ] **SG-2 · Deploy and wire:** add it to the main compose with a healthcheck and a local syslog sink under a profile. Point every service's `SIEM_URL` at it. Gate the ingest routes with mTLS workload identity, which closes the accepted residual.
- [ ] **SG-3 · Contract, docs, live:** openapi + parity, asyncapi, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, and `deployments/scripts/e2e-siem-integration-deps.py`. Cover:
  - a gateway denial reaches the sink;
  - containment ends a session and revokes a certificate;
  - evidence by correlation id.
- [ ] **ST-FE-4 · Security page tabs:** SIEM exporters, Events, Exceptions, Incidents (containment with approval), Evidence lookup. Extend `security.spec.ts`.

**Blocked and decisions:** vendor configuration (production backlog).

## D1-12 · carta-svc (:8142)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §5.4 (zero-trust scoring); plan task SEC-5 |
| Latest score | `Security & Trust.md` §12: BE 85%, integration 55%, overall 48%, Not Ready |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (2 packages) |
| Deploy | phase6 compose only; in-memory assessments. gateway-auth-svc calls it on every request |
| Console | Security page (dead-wired) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-12.F01 | Evaluate: `POST /v1/carta/evaluate` (ALLOW / STEP_UP_MFA / ISOLATE / DENY), deterministic, consumed by gateway-auth-svc; gateway enforces STEP_UP_MFA (AU-23) | 🟡 | Reconfirm (was ✅): Security & Trust §7; Auth-Service audit AU-23 | VC-1 |
| D1-12.F02 | Assessment reads: `GET /v1/carta/assessments`, `GET /v1/carta/assessments/{id}` | 🟡 | Reconfirm (was ✅): Security & Trust §4.3 | VC-1 |
| D1-12.F03 | Real signals for `device_trust_level` and `is_known_location`, replacing the placeholders | ❌ | §11.2 | SEC-5 |
| D1-12.F04 | Postgres persistence for assessments | ❌ | In memory | SEC-5 |
| D1-12.F05 | Maker-checker where the audit asks for it (policy and threshold changes) | ❌ | §11.3 | SEC-5 |
| D1-12.F06 | The ungated evaluate route accepts tenant-header-only callers (accepted residual) | 🟡 | §11.3 #4 | CA-1 |
| D1-12.F07 | Running in the main compose (gateway E2E "carta stopped → 503 write / 200 read" needs it) | ❌ | phase6 only | CA-1 |
| D1-12.F08 | Commercial geo-IP / device-fingerprint provider | ⛔ | No provider chosen | Production backlog |

**Tasks**

- [ ] **CA-0 · Baseline (S0):** `progress.md` matrix.
- [ ] **SEC-5 · carta-svc** (Reference Part A5 SEC-5). Real local signals:
  - known location from identity-context `authentication_events` (prior successful sign-ins from the network/country);
  - device trust from the passkey / registered-factor state.
  Then Postgres persistence (R4) and maker-checker on policy changes. Tests: `TestSEC5_EverySignalFromARealSource`, `TestSEC5_AssessmentSurvivesRestart`.
- [ ] **CA-1 · Deploy and gate:** add it to the main compose with a healthcheck; require mTLS workload identity (gateway's certificate) on `/v1/carta/evaluate`.
- [ ] **CA-2 · Contract, docs, live:** openapi parity, RUNBOOK, RELEASE_CERTIFICATE, and `deployments/scripts/e2e-carta-deps.py`. Cover:
  - an unknown location → STEP_UP_MFA → gateway 401 `step_up_required`;
  - carta stopped → write 503 / read 200.
- [ ] **ST-FE-5 · Security page tab:** Risk assessments (list, drawer with signals and decision; evaluate diagnostic). Extend `security.spec.ts`.

## D1-13 · key-management-svc (:8143)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §8.6, §9.5, §10–§12, §23, §25, §26; plan tasks SEC-1, SEC-2, SEC-7 |
| Latest score | `Security & Trust.md` §12: BE 55%, integration 20%, overall 27%, Not Ready (metadata only, no custody) |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (3 packages) |
| Deploy | phase6 compose only; in memory |
| Console | Security page (dead-wired) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-13.F01 | Key metadata: `POST /v1/keys`, `GET /v1/keys`, `GET /v1/keys/{id}`, `POST /v1/keys/{id}/rotate`, `POST /v1/keys/{id}/disable` (all authorized) | 🟡 | Reconfirm (was ✅): Security & Trust §7 | Replaced by real custody in SEC-1; VC-1 |
| D1-13.F02 | Real custody: generate, BYOK import (wrapped), encrypt / decrypt / sign APIs, local AES-256-GCM KEK whose own key comes from secret-vault | ❌ | Metadata only | SEC-1 |
| D1-13.F03 | Postgres persistence with every §11.1 KeyRecord field | ❌ | In memory | SEC-1 |
| D1-13.F04 | Lifecycle: scheduled and on-demand rotation, re-encryption (API-5), suspend (encrypt refused, decrypt allowed), revoke/destroy with dual control and dependency assessment, archive, compromise | ❌ | Not built | SEC-1 |
| D1-13.F05 | Maker-checker on disable/destroy | ❌ | §11.3 | SEC-1 |
| D1-13.F06 | HYOK external key reference with an availability probe; fails closed | ❌ | Not built | SEC-1 |
| D1-13.F07 | §26 key events through an outbox | ❌ | Not built | SEC-1 |
| D1-13.F08 | Crypto agility: algorithm registry (Preferred … Prohibited), `GET /security/crypto-inventory`, a versioned encryption envelope used by key-management and secret-vault | ❌ | Not built | SEC-2 |
| D1-13.F09 | Effective security policy: `GET /security/policies/effective` | ❌ | Not built | SEC-7 |
| D1-13.F10 | Running in the main compose; secret-vault (SEC-4 tokenization) and configuration-feature-flag (CFG-3 snapshot signing) use it | ❌ | phase6 only | KM-1 |
| D1-13.F11 | Cloud KMS and Vault Transit adapters | ⛔ | Infrastructure | Production backlog |

**Tasks**

- [ ] **KM-0 · Baseline (S0):** `progress.md` matrix.
- [ ] **SEC-1 · Real custody** (Reference Part A5 SEC-1; new DB under R4). Tests:
  - `TestSEC1_SuspendedKeyRefusesEncryptStillDecrypts`;
  - `TestSEC1_DestroyNeedsSecondApprover`;
  - `TestSEC1_ReencryptionResumable`;
  - `TestSEC1_UnavailableHYOKFailsClosed`.
- [ ] **SEC-2 · Crypto agility** (Part A5 SEC-2). Test: `TestSEC2_ProhibitedAlgorithmRefusedOldCiphertextDecrypts`.
- [ ] **SEC-7 · Effective policy route** (Part A5 SEC-7), authorized diagnostic use only.
- [ ] **KM-1 · Deploy and integrate:**
  - add it to the main compose;
  - move configuration-feature-flag's snapshot signing (CFG-3) and secret-vault tokenization (SEC-4) onto it;
  - register the actions (R5).
- [ ] **KM-2 · Contract, docs, live:** openapi parity, asyncapi, RUNBOOK, RELEASE_CERTIFICATE, and `deployments/scripts/e2e-key-management-deps.py`. Cover:
  - generate → encrypt → rotate → old ciphertext decrypts;
  - suspend;
  - destroy with a second approver;
  - re-encryption operation polled to completion.
- [ ] **ST-FE-6 · Security page tabs:** Keys (lifecycle commands with dual control), Crypto inventory, Algorithms, Effective policy. Finish `security.spec.ts`. Cover:
  - a destroy refused for the same user;
  - a prohibited algorithm refused;
  - empty and unavailable states.

**Blocked:** cloud KMS / Vault Transit adapters (production backlog).

---

# Cross-cutting work (both domains)

| ID | Item | Status | What is needed | Touches |
|---|---|---|---|---|
| X5 | Signed identity everywhere: ForwardAuth on every Traefik route, no published service ports, every service verifies the envelope (`IDENTITY_VERIFY` from `warn` to `require`) | 🟡 | Per service: confirm the gate is mounted (grep `NewGateFromEnv` / `internal/identity`), send the bearer from the console and callers, flip compose to `require`, prove a header-only call gets 401 in the service's E2E | All 16 |
| R-3 | About 100 estate services still trust `X-Principal-Id` on direct calls | 🟡 | Mount the identity-envelope gate service by service (pattern: authorization-svc `internal/identity`, workflow-svc gate). Our 16 first | All |
| IAM-1 | Per-service audience (AU-12) | ❌ | GW-5, then `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` per service | Gateway + all gated services |
| X3 | Real Kafka proof for every outbox | 🟡 | evidence-requirements proven on Kafka (5 Nov); identity proven 3 Nov. Each service's E2E must read its own event from the broker | All |
| API-3 | ETag / If-Match on every aggregate mutation | 🟡 | Remaining: configuration-feature-flag (CFG-1), access-control beyond roles/bundles (AC-4), secret-vault tests (SV-1), authorization makes it mandatory (AZ-1) | 4 services |
| API-4 | Cursor pagination on every list | 🟡 | Done in evidence-requirements and identity-context. Every other service has a task (TER-2, CFG-1, SV-1, SI-3, NT-1, DA-1, AC-3, JR-5, GDL-1, POL-1, OB-4, AZ-1, WF-5) | 14 services |
| API-5 | Async operations | ❌ | Needed first by ORG-9 master imports | tenant-entity-registry |
| API-6 | Deprecation / Sunset on renamed routes | ❌ | Needed by ST-3, NT-5, CFG-1, AZ-4 | 4 services |
| R5 | Production grants by migration, not `seed-demo-rbac.ps1` | 🟡 | AC-2 (notification, workflow, `svc-access-control`, `svc-policy`, ledger writers, `GOVERNANCE_DECISION_READ` readers) | access-control, authorization |
| G-07 | Evidence ledger and verification | ❌ | In governance-decision-log-svc | D2-2 |
| G-08 | Schema registry lifecycle (schema-registry-svc) | ❌ | Then register IAM-10, EML-4, evidence-requirements schemas | schema-registry-svc |
| G-09 / G-10 | Retention and disposition; legal hold (retention-registry-svc) | ❌ | G-10 first (IAM-3 and G-09 depend on it) | retention-registry-svc |
| G-05/03/04/12 | Named-operation parity | ❌ | `x-spec-operation` on existing routes in the GOV documents' services | Several |
| IAM-3/4/5 | Legal-hold negative control (after G-10); relationship predicates (after ORG-4/ORG-5); authoritative FX (after ORG-8) | ❌ | IMPLEMENTATION-PLAN §6 | authorization-svc |
| SEC-1…SEC-9 | Key custody, crypto agility, SIEM, tokenization, CARTA, mTLS management, effective security policy, CI supply chain, backup/DR | ❌ | IMPLEMENTATION-PLAN §6 Phase 7 (key-management, siem, carta, mtls-management are Security & Trust services) | Security & Trust |
| CI | No CI run on the recent commits of `shashi-changes` | 🟡 | Push and read the Actions run; fix red jobs | Both repos |
| MAIN | `shashi-changes` vs `origin/main` | 🟡 | Merge `origin/main` before each service session; re-run that service's suites after the merge | Both repos |
| FE-1 | Console screens for every capability (v2.0 kit) | 🟡 | v2.0 pages: identity, gateway, authorization, evidence (three of them still miss commands). Twelve pages to rebuild. Clients call 260 of 763 screen-relevant routes; Appendix B lists every gap. Each service section has its own "Console (frontend)" plan and `*-FE-*` tasks | Frontend |
| FE-H | Console UI E2E harness (Playwright against the live stack) | ❌ | Built once, before the first service's FE tasks: see "Frontend rules for every console" | Frontend |
| CLOSE-1…3 | Rows re-scored (Part G), Postman collections, CI-equivalent run | ❌ | After each service's S6 | Docs |

Every row of this table is a task in a session: G-08 → session 17 · G-09/G-10 → 18 · G-02 → 11 and 19 · SEC-6 → 20 · SEC-5 → 21 · SEC-3 → 22 · SEC-1/SEC-2/SEC-7 → 23 · SEC-4 → 14 · X5, R-3, IAM-1, X3, API-5, API-6, R5, G-05/03/04/12, IAM-3/4/5, SEC-8, SEC-9, CI → 24 · CLOSE-1…3, MAIN, FE-1 sign-off → 25.

---

# Carried over from the removed group testing files (6 Nov)

`testing_shashi_progress/group1-identity-foundation-testing.md` (16 Oct) and `group2-governance-platform-testing.md` (21 Oct) were removed on 6 Nov. Their findings were superseded by the 4–5 Nov audit sections and this tracker. The open items below were in them and nowhere else; each is now owned here.

| Service | Item | Status | Owner / where it goes |
|---|---|---|---|
| D1-3 configuration-feature-flag | Idempotency of the legacy `POST /v1/config`: value-equality upsert today; a caller `Idempotency-Key` would be a breaking change | 🧭 | Decide in CFG-1 together with deprecating the legacy routes |
| D1-4 secret-vault-integration | Secret class scoping: tenant / legal entity vs global (foundation for §9.6 sensitive key separation) | 🧭 | Security |
| D1-5 gateway-auth | Ratify JWKS cache TTL (default 5 min), SIEM queue sizing (256 slots, 4 workers, 3 s timeout) and the drop-with-counter policy | 🧭 | Security / platform ops; record in RUNBOOK (GW-6b) |
| D1-6 search-indexer | OD-13 bulk export delivery (size limit, format, mechanism); numeric latency and freshness SLOs for alerts | 🧭 | Product / ops; SI-3 wires the alerts once decided |
| D1-7 notification | Delivery retry policy (maximum attempts, backoff curve, dead letter) to ratify; per-jurisdiction unsubscribe rules through policy-svc | 🧭 | Product / legal; then NT tasks |
| D2-1 jurisdiction-rules | Production pack signing key (`PACK_SIGNING_KEY_REF` / `PACK_SIGNING_KEY_FILE`); production `RESOLVER_RING` / `RESOLVER_REGION` | ⛔ | Platform ops (key custody waits for SEC-1); JR-7 documents both in RUNBOOK |
| D2-2 governance-decision-log | Which services consume `zoiko.governance.events` (downstream audit and replay topology) | 🧭 | Platform architecture; add to the open questions in D2-2.F11 |
| D2-3 policy | Business sign-off on the ACTIVE tie-break for the same type and scope (`effective_from DESC`) | 🧭 | Business owner |
| D2-5 authorization | Confirm the break-glass exclusion list (payment, posting, filing actions; ZS-IAM-001 §14) with product | 🧭 | Product / Security |
| schema-registry-svc (ours under R1; G-08) | No README; tenant visibility of schemas (platform-wide reads for every verified principal, or scoped enumeration) | 🧭 | Decide in G-08; write the README then |

---

# Frontend rules for every console (stage S5)

A service is finished end to end only when its console drives every screen-relevant route against the live service, and a browser test proves it.

**Where things go** (frontend repo `zoiko-suite-frontend-platform`, branch `shashi-changes`):
- `app/admin/<page>/page.tsx` is a server page, with `actions.ts` (server actions) and `state.ts` (result shape: reuse `app/admin/authorization/state.ts`). Keep the existing route and sidebar label.
- Tab and drawer components go in `components/admin/<service>/console/`: `data.ts` (loaders), `overview.tsx` (KPIs + panels), one file per tab, `forms.tsx`.
- The API client is `lib/api/<service>.ts`, server-only (`import "server-only"`).

**Build only from the kit:**
- Use `components/admin/console` (KpiCard/KpiGrid; Panels: Pipeline, OrderedModel, BarList, PanelCallout; Register*; DetailDrawer; DrawerFacts; ControlsFooter; FilterSelect) and `components/ui` (StatCard, Badge, Button `variant="accent"` for the gold action).
- Move `RegisterView` from `components/admin/authorization/console/register.tsx` into the kit the first time a second page needs it. Keep one copy.
- Reference pages: `app/admin/authorization`, `app/admin/evidence`.

**Layout (v2.0):**
- Breadcrumb, title, one-line description, secondary outline buttons and one gold primary action.
- 5 KPI cards.
- 3 panels: a pipeline, an ordered list, and a bar list with an amber callout.
- A tabbed register with search, filters, pager and a right-side detail drawer.
- A "Required controls" footer.
- State lives in the URL: `?tab ?q ?filter[...] ?cursor ?id ?pane`.

**Data rules:**
- Live data only.
- KPIs come from the service's summary route (the `*-FE-1` task adds it when missing, with authorization, tests and openapi).
- Lists use API-4 cursors.
- A source that cannot be read shows "unavailable", never zero or an empty list. No invented numbers.
- No raw JSON as the only view; diagnostics may add a collapsible JSON panel.

**Command rules:**
- Every call goes through `lib/api/envelope.ts`: the bearer envelope, `X-Request-Id`, `Idempotency-Key` on mutations, and `If-Match` from the record's version or ETag.
- Every command opens a reason dialog and retries once after step-up on `401 step_up_required`.
- `412` shows "changed by someone else — reload".
- Problem codes become sentences in an `explain<Service>Error`.
- The maker never sees the checker's buttons (decided from the signed-in principal). The backend still enforces it.
- Secrets and tokens shown once are never cached or logged, and are cleared when the drawer closes.
- Server-only environment variables only; never `NEXT_PUBLIC_*` for a secret.

**Checks for every FE task:**
- `npx tsc --noEmit`, `npx eslint <touched files>`, and `npx vitest run <new tests>` (mappers and `explain*Error`).
- Render with `next build && next start` (port 3100). `next dev` OOMs inside the sandbox and leaves orphaned `.next\dev\build` workers; stop them after a crash. Set `ZOIKO_API_TIMEOUT_MS=15000`.

**FE-H · Console UI E2E harness (build once, first FE session):**
- `playwright.console.config.ts`:
  - `testDir: tests/e2e/console`, `channel: "msedge"`, `workers: 1`;
  - `webServer`: `npx next start -p 3100`, with the service URLs from `lib/api/config.ts` pointed at the live dev stack on `127.0.0.1`.
- `tests/e2e/console/fixtures.ts`:
  - a real login through identity-context with the console client;
  - two users (maker and checker) from the demo seeds;
  - a step-up helper (TOTP from the seeded factor);
  - an `unavailable()` helper that points one service URL at a closed port for the error-state test.
- Run: `npx playwright test -c playwright.console.config.ts tests/e2e/console/<service>.spec.ts`. The existing `playwright.config.ts` (assistant suite) stays as it is.
- Each service's spec, named in its `*-FE-*` task:
  - opens every tab;
  - filters, pages and opens the drawer;
  - runs one round trip per command family, checking the result in the register;
  - checks the maker/checker rule in the UI;
  - checks one empty state and one unavailable state;
  - undoes its writes;
  - saves a screenshot per tab under `test-results/console/<service>/`.

---

# Execution order (one service per session)

The implementing model runs one service per session, in this order, and stops at that service's hand-back. A session covers the service's backend tasks and its console tasks (`*-FE-*`), so each service comes back finished end to end. Claude then runs the verifier protocol below before the next service starts.

| Session | Unit (prompt SERVICE) | Section | Tasks |
|---|---|---|---|
| 0 | do-first | — | "Do first" list |
| 1 | gateway-auth-svc | D1-5 | FE-H first, then GW-6a, GW-5, GW-7, GW-8, GW-9, GW-6b, GW-FE-3 |
| 2 | identity-context-svc | D1-1 | IC-11…IC-19, IC-FE-4…IC-FE-6 |
| 3 | access-control-svc | D1-9 | AC-0…AC-FE-3 (AC-0 also closes ER-W3) |
| 4 | evidence-requirements-svc | D2-7 | ER-W4…ER-W8, ER-FE-3, ER-FE-4 |
| 5 | authorization-svc | D2-5 | AZ-3 first, then AZ-1, AZ-2, AZ-4, AZ-5, IAM-8, AZ-FE-1, AZ-FE-2 |
| 6 | obligations-svc | D2-4 | OB-0…OB-FE-3 with WF-3 |
| 7 | workflow-svc | D2-6 | WF-0, WF-1, WF-5, WF-6, WF-7, WF-2, ST-1…ST-3, WF-8, WF-FE-1…3 |
| 8 | governance-decision-log-svc | D2-2 | GDL-0…GDL-FE-3 with G-07 |
| 9 | jurisdiction-rules-svc | D2-1 | JR-0…JR-FE-3 (split with SCOPE if needed) |
| 10 | policy-svc | D2-3 | POL-0…POL-FE-3 |
| 11 | tenant-entity-registry-svc | D1-2 | TER-0, TER-1, ORG-6…ORG-9, G-02 (registry reads), TER-2…TER-6, TER-FE-1…3; then JUR-1 inputs (D2-1.F13) |
| 12 | configuration-feature-flag-svc | D1-3 | CFG-0…CFG-FE-3 with G-11 |
| 13 | notification-svc | D1-7 | NT-0…NT-8, EML-1…EML-6, NCD-1, NT-FE-1…3 (split with SCOPE if needed) |
| 14 | secret-vault-integration-svc | D1-4 | SV-0…SV-FE-3, SEC-4 |
| 15 | search-indexer-svc | D1-6 | SI-0…SI-FE-3 |
| 16 | delegated-authority-svc | D1-8 | DA-0…DA-FE-3 |
| 17 | schema-registry-svc | D2-8 | SR-0, SR-1, G-08, SR-2…SR-5, SR-FE-1…3 |
| 18 | retention-registry-svc | D2-9 | RR-0, G-10, G-09, RR-1…RR-4, RR-FE-1…3 |
| 19 | libraries-and-gtrm | D2-10 | LIB-1…LIB-4, G-02 (compiler commands) |
| 20 | mtls-management-svc | D1-10 | MT-0, SEC-6, MT-1…MT-4, ST-FE-1…3 (builds the shared Security page) |
| 21 | carta-svc | D1-12 | CA-0, SEC-5, CA-1, CA-2, ST-FE-5 |
| 22 | siem-integration-svc | D1-11 | SG-0, SEC-3, SG-1…SG-3, ST-FE-4 |
| 23 | key-management-svc | D1-13 | KM-0, SEC-1, SEC-2, SEC-7, KM-1, KM-2, ST-FE-6 |
| 24 | estate-switch-over | Session 24 | EST-1…EST-13 |
| 25 | closure-gate | Session 25 | CL-1…CL-8 and the definition of done |

Sessions 11–14 move work onto key-management (CFG-3, SEC-4) only in session 23 (KM-1); until then they use the local key as their tasks say.

**Rules for the implementing model (from IMPLEMENTATION-PLAN §2 and the AUTH-PLANE plan):**
- R1: edit only our 16 services, `identity-envelope`, `schema-registry-svc`, `deployments/` and the frontend. Teammates' services (org-structure, commercial-account, document-vault, evidence-manifest, the 7 evidence-gate callers' business logic, …) are called, never edited, except where a task here names the exact file.
- R3: branch `shashi-changes` in both repos; one commit per task; stage files by explicit path; never commit `*.exe` / `*.bin`; never push.
- R4: migrations take the next free number, ship a `.down.sql`, use `ENABLE` + `FORCE ROW LEVEL SECURITY` with `NULLIF(current_setting('app.tenant_id', true), '')`, guard every `GRANT`, and are applied up/down/up on a scratch DB.
- R5: every action a handler checks is registered in access-control and carried by a role template.
- R6: every new or changed route keeps the envelope gate, authorizes at the object, takes `Idempotency-Key` on mutations, `If-Match` on mutations of an existing aggregate (428/412) and `ETag` on its GET, answers RFC 9457 problems, writes events through the outbox in the same transaction, and is in `openapi.yaml` with a parity test.
- R7: every new handler, store method, consumer and ticker is reached from `cmd/server/main.go`.
- R10: real Postgres for store suites (0 skip); every spec negative path has a test named after its id.
- R14: no stubs and no invented answers. A missing external dependency gets an interface plus a real local implementation; only the production adapter is marked ⛔.
- Status in this file: the implementer may change a row to 🔵 (built) and tick a task. Only the verifier changes a row to ✅.

**Environment facts:**
- Use `127.0.0.1`, never `localhost`. Python: `C:/Users/kaman/AppData/Local/Python/bin/python3.14.exe -I`.
- Host Go tests: `GOTMPDIR=<scratch dir> GOTOOLCHAIN=local go test -count=1 ./...`. Windows Application Control sometimes blocks a test binary ("An Application Control policy has blocked this file"); re-run the blocked package, or use Docker `golang:1.26-alpine` with `-v <services>:/src -v C:/Users/kaman/go/pkg/mod:/go/pkg/mod -e GOFLAGS=-mod=mod -e GOPROXY=off -e CGO_ENABLED=0` and `MSYS_NO_PATHCONV=1`; revert any `go.mod` the `-mod=mod` run rewrites.
- Working copies are CRLF: check `gofmt` with `tr -d '\r' < f | gofmt -l`.
- Dev stack: compose project `deployments`; `deployments/migrate.py up --db <name>` before recreating a container; build one image at a time (Docker VM ~2 GB); start only the containers a task needs and stop them afterwards.

---

# Verifier protocol (Claude runs this at each hand-back; nothing is taken on the summary)

1. `git log` / `git show --stat` for the service's commits: only R1 paths, explicit staging, no binaries, one commit per task.
2. Build, `go vet`, full suite on a scratch Postgres; counts match `progress.md`; store suites 0 skip.
3. **Feature matrix walk:** every row in this file and in `progress.md` has a route or consumer, a named test, and a console screen, or a ⛔/🧭 reason naming the external dependency or decision owner. Spot-read the spec for 10 random rows.
4. Grep the callers from `cmd/server/main.go` for every new handler, store method, consumer and ticker (R7).
5. kin-openapi validation and router-parity tests pass; every new route is documented.
6. Each new migration: number, `.down.sql`, RLS + FORCE with `NULLIF`, guarded grants; up/down/up on scratch.
7. Every new action is registered in access-control and carried by a template (R5).
8. Live: image rebuilt (fresh timestamp), migrations applied, `deployments/scripts/e2e-<svc>-deps.py` all PASS (a skip never counts as a pass), `scripts/audit.sh` green.
9. Mutation check: break two guards on a scratch copy (for example a self-approval check and a cursor tenant binding); a test must fail for each.
10. Frontend:
    - `npx tsc --noEmit`, `npx eslint <touched files>` and the new `vitest` tests are clean;
    - Appendix B for the service is empty, or each remaining route has a recorded reason;
    - `npx playwright test -c playwright.console.config.ts tests/e2e/console/<service>.spec.ts` passes against the live stack, with a screenshot per tab, one empty state and one "unavailable" state;
    - no invented numbers;
    - the maker never sees the checker's buttons.

11. Plan §8 extras: re-run the named-operation coverage scan over the 15 specs and compare it with the 23 Oct baseline (no missing operation except BLOCKED ones); reopen any task whose evidence is missing, does not reproduce, or describes code with no caller.

- [ ] **VC-1 · Confirmation check (verifier).** Every row whose Evidence starts with "Reconfirm (was ✅)" was marked done before 6 Nov and is now 🟡 until it is confirmed again. For each one:
  - re-run the cited test or suite;
  - re-read the code at the cited place, on the current branch;
  - for claims about live behaviour, re-run the service's dependency E2E.

  A pass turns the row ✅, with the date and evidence. A failure keeps it 🟡 or makes it ❌, with the finding and a new task. Run it for a service at its hand-back. It can also run earlier, for a service whose session has not started yet.

Any failure reopens the task. After a pass, the verifier changes the rows to ✅, updates the dashboard, and adds one dated entry to Part G (re-score log).

---

# Session 24 · Estate switch-over (all 26 units)

Run this once every service session (1–23) has handed back. It turns the per-service work into one estate that enforces everything at the same time. Each task flips a setting only after the E2E has proved that the callers are ready.

- [ ] **EST-1 · Signed identity everywhere (X5, R-3, F-1, M-02).**
  - Every one of our units moves from `IDENTITY_VERIFY=warn` to `require`. Change the compose defaults in `deployments/docker-compose.yml`; today it is `warn` for tenant-registry, jurisdiction, decision-log, config, secret-vault, obligations, workflow, search-indexer, notification, delegated-authority, access-control and evidence-requirements.
  - Then:
    - `POLICY_IDENTITY_VERIFY=enforce`;
    - `AUTHZ_IDENTITY_ASSERTION=require`;
    - `AUTHZ_EVALUATION_MTLS=require` (after every PEP presents a certificate);
    - `DECISION_LOG_READ_AUTHZ=enforce`;
    - `EVIDENCE_EVALUATE_AUTHZ=enforce`;
    - `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=true`;
    - `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up`.
  - Done when a header-only call to every unit answers 401 in `e2e-all.py` (EST-10) and every console Playwright spec still passes.
- [ ] **EST-2 · Strict audience (IAM-1):** after GW-5, set `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` for every gated unit. Prove that an envelope minted for one service is refused by another (`TestIAM01_EnvelopeForPolicyRefusedByWorkflow` plus a live probe).
- [ ] **EST-3 · No published service ports:** only Traefik (and documented admin ports bound to 127.0.0.1) is reachable from the host. Remove `8082:8082` and every other published port of our units from compose (J-01). Run the E2Es through Traefik.
- [ ] **EST-4 · Kafka proof matrix (X3):** add a table to this file listing every event our units produce or consume (producer, topic, consumers). Each edge needs a check in some E2E that reads the record from the broker. Done when there is no edge without a check.
- [ ] **EST-5 · Async operations (API-5)** in the five places: search-indexer rebuild/reindex, jurisdiction-rules compile and test runs, retention-registry disposition (G-09), tenant-entity-registry master imports (ORG-9), key-management re-encryption (SEC-1). Every unit must match the shared semantics: 202 + `Location: /v1/operations/{id}`, polling, cancel before the point of no return, and the same key giving the same operation id. One test file per unit.
- [ ] **EST-6 · Deprecation (API-6):** every renamed or replaced route answers with `Deprecation` and `Sunset` headers, and a test proves it. This covers ST-3 status writes, NT-5 legacy notifications, CFG-1 legacy config and AZ-4 aliases.
- [ ] **EST-7 · Named-operation parity (G-05/03/04/12):** in policy-svc and authorization-svc, every GOV-03/04/05/12 operation maps to a route (`x-spec-operation`) or gets built (Reference Part A5).
- [ ] **EST-8 · IAM-3, IAM-4, IAM-5 in authorization-svc:**
  - IAM-3: legal-hold deny, needs G-10.
  - IAM-4: relationship and qualification predicates from parties, org units and jurisdiction rules; needs TER-1 and org-structure-svc (a teammate's, read-only).
  - IAM-5: authoritative FX from REF-03 rate sets; needs ORG-8.
  - Tests for scenarios A23, A24, A25, and a grep showing no literal FX rate.
- [ ] **EST-9 · Grants audit (R5):** a test lists every action string that any of our units checks (grep the handlers' action constants) and fails if one is missing from access-control's registry or from every template. Strip `deployments/scripts/seed-demo-rbac.ps1` down to demo users and demo data. Production grants come only from migrations.
- [ ] **EST-10 · One stack, one runner:**
  - all 26 units are in `deployments/docker-compose.yml` with healthchecks (fold `docker-compose.phase6.yml` in);
  - `docker compose -p deployments config` is clean;
  - a fresh volume migrates from zero with `migrate.py up --all`;
  - a new `deployments/scripts/e2e-all.py` runs every `e2e-*-deps.py` and the console Playwright suite, and prints one table.
- [ ] **EST-11 · Observability parity:** every unit has `/metrics`, `/healthz`, `/readyz`, request and outbox metrics, and alert rules in `deployments/prometheus-rules.yml`, plus a RUNBOOK section per alert. Run `promtool check rules`.
- [ ] **EST-12 · CI supply chain (SEC-8):** per unit, govulncheck, an SBOM, and cosign sign and verify. The workflow parses, actionlint is clean, and CI is green on `shashi-changes`.
- [ ] **EST-13 · Backup and restore on the dev stack (SEC-9):**
  - `deployments/scripts/backup.sh` and `restore.sh`, plus the runbook;
  - a recorded drill: back up every DB of our units, restore it to scratch, and run the verification queries.
  - The production drill is in the production backlog.

---

# Session 25 · Closure gate: Domain 1 and Domain 2 complete (except production)

- [ ] **CL-1 · Spec coverage inventory.**
  - Extract the 15 specs (command in Reference Part A2, list in the §3 table).
  - For every section that governs one of our 26 units, list each named command, query, event, state transition, control and negative path.
  - Map each one to a route or consumer, a test and a feature row.
  - Write the result as **Appendix D · Spec coverage inventory** in this file, with columns spec | section | item | unit | route/consumer | test | row | status.
  - Every item without a match becomes a new feature row and task in its unit's section; implement it before CL-8.
  - Done when every item maps, or is ⛔ with a production-only reason, or is out of scope with the rule that excludes it.
- [ ] **CL-2 · Decision sheet.**
  - Collect every 🧭 row, the "Carried over" table, the ❓ items in the audits (Parts D and E) and the teammate audits' "needs clarification" lists.
  - Write them as one table in Reference Part F, with columns question | options | recommendation | owner.
  - The user (or the named owner) decides; record each answer with its date.
  - Then implement each decision as a task in its unit; the row moves to 🔵.
  - Done when no 🧭 is left.
- [ ] **CL-3 · Confirmation sweep (VC-1 for everything).** Every "Reconfirm (was ✅)" row in all 23 sections is confirmed or reopened. Done when no "Reconfirm" row is left.
- [ ] **CL-4 · Cross-team handover.**
  - For each row that depends on a teammate's service, write the request: what to change, why, and the evidence. Examples:
    - general-ledger's ungated posting paths;
    - callers forwarding the envelope;
    - the C8 obligations overlap;
    - external-data-feed drift events;
    - document-vault as a hold consumer;
    - policy-svc callers' 401/403 mapping;
    - invoice-approval's missing path;
    - business-object owners calling `/invalidate`;
    - object versions persisted by consumers.
  - Record in Reference Part F the date and to whom each was sent.
  - Our side must already fail closed and say so in SPEC_DEVIATIONS.
  - Done when every cross-team item is either handed over or closed.
- [ ] **CL-5 · Close-out (CLOSE-2, CLOSE-3):**
  - Postman collections updated for every unit;
  - a CI-equivalent run for every unit: `go mod verify`, build, vet and `go test -race ./...` in `golang:1.25` against `postgres:16-alpine`, with counts recorded.
- [ ] **CL-6 · Full acceptance on a fresh stack:**
  - fresh volumes, migrations from zero, grants only from migrations;
  - `e2e-all.py` passes with no FAIL and no SKIP except documented production-only ones;
  - every console Playwright spec passes;
  - screenshots are archived under `test-results/console/`.
- [ ] **CL-7 · Merge:**
  - open a PR from `shashi-changes` to `main` in both repos;
  - CI green;
  - review comments resolved;
  - merge (after the user approves);
  - re-run the build/vet/unit sweep and `e2e-all.py` on `main`.
- [ ] **CL-8 · Final re-score and sign-off:**
  - the verifier re-scores every unit;
  - writes one Part G entry per unit with its final score;
  - sets the dashboard (every row ✅ or production-only ⛔);
  - ticks the definition of done below.

## Definition of done: Domain 1 and Domain 2, everything except production

All of these must hold. The verifier checks each one in CL-8.

1. Every feature row of all 23 sections (16 services, 6 supporting services, the libraries) is ✅ after the verifier's check, or ⛔ with a reason that is either listed in the production backlog below or handed over to the owning team under CL-4. No 🟡, 🔵, ❌ or 🧭 is left.
2. Appendix D (CL-1) maps every spec item of our 26 units.
3. Appendix B is empty for every unit, and every unit's console spec passes against the live stack.
4. Every unit has completed S0–S6: a `progress.md` matrix, a green suite (0 skip), openapi/asyncapi with parity tests, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman, a dependency E2E that passes, and a mutation check.
5. EST-1…EST-13 are done: require mode, strict audience, no published ports, Kafka matrix, API-3…API-6, R5 grants by migration only, one compose, observability, CI supply chain, and a backup/restore drill on dev.
6. Every decision is taken and implemented (CL-2), and every cross-team item is handed over (CL-4).
7. Merged to `main`, with CI green and the post-merge sweep and `e2e-all.py` green (CL-7).

## Production backlog (outside this file's "done"; the next project phase)

These need production infrastructure, vendors or sign-offs. The code for each already has an interface plus a real local implementation, so only the production adapter or setup is missing.

| Area | Item | Unit |
|---|---|---|
| Edge | DDoS, WAF, bot/abuse and TLS policy at the GCP load balancer (Cloud Armor; S5-D1); production trusted-proxy CIDRs | gateway-auth-svc |
| Identity | External IdP vendor (D-4); SAML if decided; real-browser passkey attestation policy; TOTP sign-off (SO-1); session maximums (SO-2); sender-constrained tokens (AU-27) | identity-context-svc, authorization-svc |
| Keys and secrets | Cloud KMS or Vault Transit; one-time migration `VAULT_MIGRATE_LEGACY_KEY_FILE`; dynamic credentials (O-3); HYOK customer keys | secret-vault, key-management |
| Workload identity | Production CA hierarchy; per-service mTLS enrolment (M-01); Traefik `serversTransport` client certificates (S4-B1) | mtls-management, gateway |
| SIEM | Vendor tenancy and configuration (Splunk / Datadog / Elastic / Sentinel) | siem-integration-svc |
| Communications | Email provider, sender DNS (SPF, DKIM, DMARC), warm-up; SMS and push providers (OD-03, OD-04) | notification-svc |
| Search | Embedding provider and model (OD-10); production OpenSearch | search-indexer-svc, search-client |
| Risk signals | Commercial geo-IP / device-fingerprint provider | carta-svc |
| Reference data | Address and LEI verification provider | tenant-entity-registry-svc |
| Signing | Production pack signing key (`PACK_SIGNING_KEY_REF`); production `RESOLVER_RING` / `RESOLVER_REGION` | jurisdiction-rules-svc |
| Platform | Kubernetes manifests and GCP deploy; production restore drill with measured RPO/RTO (S2-C3); load and revocation SLOs measured on production-like capacity (IAM-8) | all |

---

# Reference: rules, plan and audit record

Background the tasks above rely on, merged from six files on 6 Nov. Status lives in the feature tables above, not here. Every block below is copied word for word and starts with a `Source:` line naming the original file and line range; removed text is in git (`git show 9be6c41d:"docs/audit_files/<file name>"`).

## Where to find things in the Reference

| Need | Go to |
|---|---|
| Rules every change must follow (R1–R15) | Part A1, then Part B1 |
| Decisions already taken (do not re-ask) | Part A4, Part E3, Part F1 |
| The specification of a plan task (ORG-6, WF-3, G-07, EML-1, SEC-4, API-5, …) | Part A5 |
| Evidence block format for `progress.md` | Part A6 |
| How the reviewer checks a hand-back | "Verifier protocol" (before this chapter) |
| Gateway GW-5 / GW-6 and access-control AC-0 … AC-FE-2 specifications | Part B4, Part B5 |
| AU-01 … AU-27 (auth plane rows) | Part C |
| Domain 1 scores and open gaps (S2-… to S9-…, S1-9 … S1-14, K-1 … K-7, G-1 … G-4) | Part D |
| Domain 2 scores, row movements and open gaps (J-…, D-…, P-…, L-…, Q-…, H-…, the obligations rows) | Part E |
| Cross-team follow-ups and dormant email triggers | Part F1 |
| Where a re-audit result goes | Part G |

## Sources merged into this file

| Source file (removed 6 Nov) | Kept here | Removed, and why |
|---|---|---|
| `IMPLEMENTATION-PLAN-2026-10-23.md` (1,015 lines) | §2 rules, §3 environment, §4 placement, §5 decisions, §6 every open task, §7 evidence block, §8 reviewer checks (Part A) | §1 "how to work" (replaced by the tracker's "How to use"). Finished tasks: P0-1…P0-3 (27 Oct); API-1 and API-2 (DONE); IAM-12 and IAM-13 (2–3 Nov); ORG-1 (3 Nov) |
| `AUTH-PLANE-PLAN-2026-11-02.md` (622 lines) | Global constraints, review focus, S0–S6 recipe, gateway status + GW-5, GW-6, GW-FE-2 deviation, all of access-control AC-0…AC-FE-2, BLOCKED (Part B) | 2 Nov context. identity-context IC-0…IC-FE-3 (done, verified 3 Nov). GW-0…GW-4 and GW-FE-1 (done or on hold; their state is in Part D6). The verifier protocol (it lives in the tracker). Execution handoff |
| `Auth-Service-audit-2026-10-14.md` (216 lines) | Scope, sources, method, gaps A–E by owner, the 3 Nov re-score of AU-01…AU-11 (Part C) | "Path to done end to end" (replaced by the tracker's execution order). The 14 Oct live probes (history) |
| `Identity, Scope & Foundation-audit-2026-09-23.md` (3,489 lines) | Method and legend. Scores (19 Oct after fixes; identity 4 Nov). The 7 Oct "every open gap" register and every change to it (13 Oct, 14 Oct, 19 Oct, 21 Oct). identity-context's 4 Nov register and findings K-1…K-7. Gateway status 4 Nov (Part D) | The nine original per-service row tables (23–28 Sep), replaced by the 7 Oct register and later passes. Group 1 overall (28 Sep). Findings R-1…R-7 (7 Oct), F-1…F-8 (19 Oct) and N-1…N-9 (3 Nov), all closed. The 8 Oct, 13 Oct, 14 Oct and 19 Oct closure narratives and their test logs. "Claims checked" lists |
| `Governance Platform-audit-2026-09-28.md` (3,882 lines) | Method and legend. The 19 Oct current scores and row movements. Group decisions. The open registers of jurisdiction-rules, governance-decision-log, policy and workflow. The rows of obligations not ✅. The authorization sign-off register. The workflow 21 Oct pass. The identity items (4 Nov). Evidence-requirements completion (5 Nov) (Part E) | 28 Sep scores, group-wide findings and top gaps (replaced by 19 Oct). The seven original per-service row tables (the 19 Oct section calls them history), except obligations' open rows. The authorization register and closure narrative (closed: 85/85). Policy per-row results. Deploy checklists, verification logs and the obligations outbox regression narrative (fixed 19 Oct) |
| `IMPLEMENTATION-PROGRESS.md` (702 lines) | Decisions taken during implementation, cross-team follow-ups, dormant email triggers (Part F) | The status board (replaced by the tracker dashboard and its cross-cutting table). The 27 and 28 Oct reviews. The evidence blocks P0-1…API-4 (history; each service's `progress.md` and git keep them) |

## How to keep the Reference current

- Status changes go in the feature tables, not here.
- A verifier pass that re-scores rows adds one dated entry to Part G. Do not append whole new audit passes.
- When a register item in Part D or E closes, mark it closed in place (date and commit) instead of deleting it.

## Part A · Implementation plan: rules, placement, decisions, open task specs

### A1 · Ground rules R1–R15 (plan §2)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 28–137 (“2. Ground rules (non-negotiable)”)

#### 2. Ground rules (non-negotiable)

**R1 · Scope: only our services.** You may change:

| Group | Services |
|---|---|
| Identity, Scope & Foundation | identity-context-svc, tenant-entity-registry-svc, configuration-feature-flag-svc, secret-vault-integration-svc, gateway-auth-svc, search-indexer-svc, notification-svc, delegated-authority-svc, access-control-svc |
| Governance Platform | jurisdiction-rules-svc, governance-decision-log-svc, policy-svc, obligations-svc, authorization-svc, workflow-svc, evidence-requirements-svc |
| Governance supporting | schema-registry-svc, retention-registry-svc, `services/identity-envelope`, `services/search-client`, `services/_contract`, `deployments/gtrm` |
| Security & Trust | mtls-management-svc, siem-integration-svc, carta-svc, key-management-svc |
| Platform files | `deployments/` (compose, k8s, init-db, migrate.py, prometheus rules), `.github/workflows/`, `docs/` |

Every other service belongs to a teammate. Examples are currency-registry, fiscal-calendar,
accounting-period, payee-banking-identity, counterparty-management, org-structure,
exception-escalation, obligation-tracking, document-vault, privacy-*, commercial-account,
capability-registry, kill-switch-registry, treasury, general-ledger, evidence-manifest and
external-data-feed. **Do not edit them.** You may *call* their existing APIs. If a task needs a
change inside one, add it to "Cross-team follow-ups" in the progress file and carry on.

**R2 · No new microservices.** Where each spec section goes is decided in §4.

**R3 · Git.**
- Make one commit per task, or per service when a task spans several services.
- Stage files by explicit path. Never use `git add -A` or `git add .`, because other people's
  edits sit in this checkout.
- Never push, and never commit binaries (`*.exe`, `server`, `healthcheck`).
- Write commit messages in the repo's style (`feat(<svc>): …`, with a body saying what was wrong
  and what changed).

**R4 · Migrations.**
- Use the next free number in that service's `deployments/migrations/`. Some services have
  duplicate numbers, so `ls` the folder first.
- Always add a new file; never edit a migration that already exists. Every `.up.sql` needs a
  `.down.sql`.
- Tenant tables need `ENABLE` + `FORCE ROW LEVEL SECURITY`. Policies use
  `NULLIF(current_setting('app.tenant_id', true), '')::uuid`; a bare `::uuid` cast breaks on
  pooled connections.
- Every `GRANT` must be guarded (`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '…') THEN … END IF; END $$;`)
  or name a role that `deployments/scripts/create-app-roles.sh` creates. An unguarded grant
  aborts fresh-volume init for all 85 databases.
- A service that gets its first database (the Security & Trust four) also needs a database and
  role in `deployments/init-db.sh` and `create-app-roles.sh`, and a compose entry for its DSN.

**R5 · Permissions.** Every action a handler checks must be registered in access-control-svc:
- a new migration (000023 onward) with
  `INSERT INTO permission_definitions (action_name, naming, risk_tier, description)`, following
  the pattern in `000021_register_governance_platform_actions.up.sql`;
- a role template that carries the action;
- risk tier HIGH or CRITICAL for anything destructive or administrative.

An unregistered action can never be granted, so the endpoint answers 403 forever. That is how
`POST /v1/validate-action` broke.

**R6 · Every new or changed route:**
- keeps the identity-envelope gate already mounted in that service;
- checks authorization at the object it acts on (its entity), not at the caller's own entity;
- takes `Idempotency-Key` on mutations (ZS-API-001 §9). The replay key is tenant + principal +
  route template + key → request hash. Same key with a different body is 409;
- requires `If-Match` on mutations of an existing aggregate (428 when missing, 412 when stale)
  and returns `ETag` on reads of it;
- returns RFC 9457 problem details (task API-1);
- writes its events through the service's transactional outbox, in the same DB transaction as
  the state change;
- appears in `openapi.yaml`, which must be validated with a real parser (kin-openapi), and passes
  the service's route-parity test. Add that test where it is missing.

**R7 · Wiring.** New code must be constructed in `cmd/server/main.go` (or code it calls) and
reached by a route, consumer or ticker. Grep the callers. Code with no caller is not done.

**R8 · Background work.** Never run `go f(ctx)` with the request context, because it is cancelled
when the handler returns. Use `context.WithoutCancel` plus a timeout, or the outbox.

**R9 · Event names.** Use the wire names producers actually emit (grep the producers). Spec names
sometimes differ; where they do, emit both during a deprecation window and register both in
schema-registry.

**R10 · Tests.**
- Run against real Postgres (§3). Store and integration suites must **run, not skip**: record
  pass/fail/skip per package.
- Every negative-path or "minimum acceptance" line in a spec section you implement gets a test
  named after it (e.g. `TestORG04_RejectsOwnershipCycle`).
- Never point a test at the dev stack's Postgres on :5432. authorization-svc's store tests drop
  their database.

**R11 · Windows host.**
- Application Control randomly blocks `*.test.exe`. Compile with
  `go test -c -o <name>.exe ./internal/<pkg>` and run the renamed binary from the package folder,
  or run the suite in `docker run --rm -v "<repo>:/src" -w /src/services/<svc> golang:1.25-alpine go test ./...`.
- Do not write files with Python on Windows (it writes CRLF). `*.sh` must stay LF; check with
  `git ls-files --eol`.

**R12 · Docker.** Start only the target service and its `depends_on`
(`docker compose -f deployments/docker-compose.yml up -d <svc>`), never the whole file.
Rebuild with `--build`, because a plain `up` runs weeks-old images.

**R13 · Disk.** C: is almost full. If a build fails for space, run `go clean -cache`. Do not
prune Docker images or volumes.

**R14 · No stubs, no fake answers.** If a capability needs something that does not exist here
(cloud KMS, a SIEM vendor, an external IdP, an address-verification provider, sender DNS), build
the provider interface plus a real local implementation: a local KEK, a syslog/HTTPS sink,
deterministic validation rules. Mark only the production adapter BLOCKED. An endpoint that
invents an answer is worse than no endpoint.

**R15 · Spec deviations.** Where placement or behaviour differs from a spec (e.g. master data
living in tenant-entity-registry-svc instead of MDM-01…05 services), say so in that service's
`SPEC_DEVIATIONS.md`. Create the file if it is missing.

---

### A2 · Environment (plan §3)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 138–185 (“3. Environment”)

#### 3. Environment

- **Backend:** `zoiko-suite-backend/` (Go, chi, pgx, kafka-go). Each service is its own module.
  The 16 Group 1/2 services import `zoiko.io/identity-envelope` through `replace ../identity-envelope`,
  so their Dockerfiles build from the repo root.
- **Frontend:** `zoiko-suite-frontend-platform/` (Next.js 16, which differs from older versions;
  read `node_modules/next/dist/docs/` before writing FE code).
- **Scratch test database:**
  ```bash
  docker run -d --name plan-pg -p 55432:5432 -e POSTGRES_PASSWORD=postgres postgres:16-alpine
  # one database per service:
  docker exec plan-pg psql -U postgres -c "CREATE DATABASE tenant_registry_test"
  export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:55432/tenant_registry_test?sslmode=disable"
  ```
  RLS assertions must run as an app role, not the superuser. Copy configuration-feature-flag-svc's
  `appRolePool` test pattern.
- **Dev stack migrations:** `python deployments/migrate.py status`, then `up --db <name>`.
- **Spec text:** the `.docx` files are in `docs/architecture/original_doc/`. Extract them once into
  the git-ignored `scratch/` folder:
  ```bash
  mkdir -p scratch/specs
  for f in docs/architecture/original_doc/*.docx; do b=$(basename "$f" .docx); unzip -p "$f" word/document.xml | sed -e 's#</w:p>#\n#g' -e 's#<w:tab/># #g' -e 's#<[^>]*>##g' -e 's/&amp;/\&/g' -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&quot;/"/g' -e "s/&apos;/'/g" > "scratch/specs/$b.txt"; done
  ```
- **Audit history:** `docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md`
  (Group 1), `Governance Platform-audit-2026-09-28.md` (Group 2), `Auth-Service-audit-2026-10-14.md`
  (IAM) and `Security & Trust.md`. The latest state of each service is at the end of each file.

**Spec index**

| ID | File (in `docs/architecture/original_doc/`) | Governs |
|---|---|---|
| ZS-SVC-A-001 | `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx` | GOV-01…12, §16 envelope and error classes, §17 events |
| ZS-SVC-I-001 | `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx` | ORG-01…10 (§4.x), REF-01…10 |
| ZS-SVC-U-001 | `ZS-SVC-U-001_Enterprise_Master_Data_…docx` | MDM-01…05 (§9–18), API list near line 640 of the text |
| ZS-SVC-R-001 | `ZS-SVC-R-001_Workflow_Approval_Case_Obligation_Control_…docx` | WFC-01…05 |
| ZS-SVC-V-001 | `ZS-SVC-V-001_Policy_Rules_Decision_Jurisdictional_Applicability_Control_…docx` | PDC-01…05 |
| ZS-SVC-Y-001 | `ZS-SVC-Y-001_Notification_Communication_Delivery_Control_…docx` | NCD-01…05 |
| ZS-SVC-AA-001 | `ZS-SVC-AA-001_Platform_Configuration_Feature_Flag_…docx` | CFG-01…05 |
| ZS-SVC-AB-001 | `ZS-SVC-AB-001_Enterprise_Search_Indexing_…docx` | ESR-01…05 |
| ZS-IAM-001 | `ZoikoSuite_Global_Authorization_RBAC_ABAC_Segregation_of_Duties_Standard_…docx` | §1–32, scenarios A01…A30 |
| ZS-SEC-001 | `ZoikoSuite_Security_Privacy_Residency_Cryptographic_Architecture_Standard_…docx` | §1–30 |
| ZS-JUR-001 | `ZoikoSuite_Global_Jurisdiction_Pack_Regulatory_Rule_Architecture_Standard_…docx` | §1–30 |
| ZS-STATE-001 | `ZoikoSuite_Business_Object_State_Machine_Workflow_Approval_Catalogue.docx` | §1–24 |
| ZS-API-001 | `ZoikoSuite_Global_API_Command_Integration_Contract_Standard_…docx` | §1–30 |
| ZS-COMMS-EMAIL-001 | `ZoikoSuite_Email_Communications_System.docx` | Sections 00–15: 128 templates (§10/§11), 32 variants (§12), backlog (§13) |

---

### A3 · Where each missing section is built (plan §4)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 186–208 (“4. Where each missing section is built (decided 23 Oct 2026)”)

#### 4. Where each missing section is built (decided 23 Oct 2026)

| Spec section | Built in |
|---|---|
| GOV-02 runtime reads | tenant-entity-registry-svc (reads) + `deployments/gtrm` compiler commands (stays a compiler: decision doc §15) |
| GOV-07 Evidence ledger and verification | governance-decision-log-svc |
| GOV-08 Schema and contract registry lifecycle | schema-registry-svc |
| GOV-09 Retention and disposition, GOV-10 Legal hold | retention-registry-svc (legal-hold authority; document-vault becomes a consumer, cross-team) |
| GOV-11 Feature policy and capability exposure | configuration-feature-flag-svc |
| WFC-04 Cases, ZS-STATE §11 state-machine registry | workflow-svc |
| WFC-05 Obligation, deadline and escalation clocks | obligations-svc |
| ORG-01 identity gaps | identity-context-svc |
| ORG-04, ORG-05, ORG-07, ORG-08, ORG-09; MDM-01…05; REF-03, REF-06, REF-08, REF-09; `/v1/reference/{set}/{code}`; master imports | tenant-entity-registry-svc (new internal modules `ownership`, `orgunit`, `party`, `address`, `registration`, `reference`, `imports`) |
| REF-01 jurisdiction registry commands, REF-10 business calendars and payment terms, ZS-JUR `jurisdiction-decisions:resolve` | jurisdiction-rules-svc |
| REF-07 Accounting policy | policy-svc (policy type `ACCOUNTING_POLICY`) |
| ORG-06 remaining operations | authorization-svc (role assignments) |
| Email templates and trigger routing | notification-svc |
| Key custody, BYOK/HYOK, crypto inventory and algorithm registry, effective security policy | key-management-svc |
| SIEM forwarding, security exceptions, incident containment, security evidence | siem-integration-svc |
| Tokenization, masking, pseudonymization | secret-vault-integration-svc |

---

### A4 · Decisions already taken (plan §5)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 209–236 (“5. Decisions already taken (do not re-ask)”)

#### 5. Decisions already taken (do not re-ask)

| Topic | Decision |
|---|---|
| Scope | Ours plus existing services only; no new microservices; teammates' services untouched |
| Email | All 128 templates + 32 state variants, plus trigger routing; triggers with no producer stay dormant and listed |
| Security | Service code + CI supply chain + backup/DR scripts and runbook |
| WFC-05 (old C1 hold) | In scope now: obligations-svc gets the full deadline-clock model |
| C8 single owner of obligations | obligations-svc is the owner; obligation-tracking-svc overlap is a cross-team follow-up |
| C3 event naming | Keep current wire names; add spec-canonical names alongside where they differ (R9) |
| X9 RFC 9457 | Required everywhere in our services (API-1/API-2) |
| P-09 `expected_version` | Mandatory, as `If-Match`, for every mutation of an existing aggregate (API-3) |
| P-03 policies and jurisdiction | Policies carry `jurisdiction_context` (V-001 PDC-02 applicability) |
| S3-D1 `X-Commercial-Plan`, `X-Org-Unit-Id` | Edge-set server context, stripped from clients; plan from commercial-account-svc |
| S3-D2 config snapshot signing | Sign with a key-management-svc key (local key until SEC-1 lands) |
| S4-D1 dynamic credentials | Broker returns per-lease credentials; Vault engine adapter BLOCKED until infra |
| S5-D2 CARTA unreachable | Fail closed on writes, open on reads (decided 21 Oct) |
| S7-D1 notification operating values | Ratify the values already in code |
| S7-D2 secure links | Signed, expiring capability link; S1+ content also needs a signed-in session |
| S7-D3 legal-hold authority | retention-registry-svc |
| S8-D1 revocation window | 5 s accepted; quantitative authority limits live in authorization-svc `authority_limits` |
| S9-D1 legacy action names | Add `resource.action` aliases in access-control; never rename callers' checks |
| GTRM | Stays a compiler (approved decision doc); runtime commands are compiler commands with provenance |
| Embedding provider (S6-D1), SIEM vendor, cloud KMS, external IdP vendor, address/LEI verification provider, sender DNS | BLOCKED on external choice; build interface + local implementation |
| P-01 SPEND_CONTROL / SIGNATORY_MATRIX / SOD_RULE payload semantics | BLOCKED on business input |

---

### A5 · Open task specifications (plan §6; done tasks removed)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 237–240 (“6. Tasks”)

#### 6. Tasks

Each task gives: **Where** · **Spec** · **Build** · **Done when**. Every task is also bound by §2.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 267–268 (“Phase 1: API contract (ZS-API-001) in all our services”)

#### Phase 1: API contract (ZS-API-001) in all our services

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 302–335 (“**API-3 · ETag and If-Match on every aggregate mutation**”)

**API-3 · ETag and If-Match on every aggregate mutation**
- **Build:** list the mutable aggregates per service in the progress file first (tenant, legal
  entity, flag, config change, secret policy, template, delegation, role, bundle, rule, pack,
  calendar, policy version, obligation, filing requirement, authority limit, SoD/ABAC rule,
  workflow definition, evidence requirement, schema, hold, rule set, key record, …).
- **Done when:** for each aggregate, GET returns `ETag`, and each mutation has three tests:
  missing `If-Match` → 428, stale → 412, correct → 2xx.

**API-4 · Cursor pagination on every list endpoint**
- **Build:** `filter[...]`, `sort` with `id` tie-break, `page_size`, `next_cursor`. Offset paging
  stays only on small static reference lists, and that must be documented in openapi.
- **Done when:** each list endpoint has tests for its first page, next page, tampered cursor (400)
  and unknown filter (400).

**API-5 · Async operations**
- **Where:**
  - search-indexer: rebuild and reindex;
  - jurisdiction-rules: pack compile and test-runs;
  - retention-registry: disposition execution (G-09);
  - tenant-entity-registry: master imports (ORG-9);
  - key-management: re-encryption (SEC-1).
- **Spec:** ZS-API-001 §18.
- **Build:**
  - `Prefer: respond-async` (or an always-async operation) → 202 + `Location: /v1/operations/{id}`;
  - `GET /v1/operations/{id}`;
  - cancel only before the point of no return;
  - an idempotency retry returns the same operation id.
- **Done when:** each service has tests for 202 + Location, polling to a terminal state, the same
  key giving the same operation id, and cancelling after the point of no return → 409.

**API-6 · Deprecation of renamed routes**
- **Build:** use the API-1 helper wherever a route is renamed (e.g. ST-3).
- **Done when:** the old route returns `Deprecation` and `Sunset` headers, as shown by a test.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 336–447 (“Phase 2: governance control plane (ZS-SVC-A-001)”)

#### Phase 2: governance control plane (ZS-SVC-A-001)

**G-10 · Legal hold** (do this first: IAM-3 and G-09 depend on it)
- **Where:** retention-registry-svc.
- **Spec:** GOV-10 (§13 of the GOV doc, including its negative paths), ZS-DATA-GOV legal hold.
- **Build:**
  - commands: `IssueLegalHold` (matter, scope by subject ids / record classes / date range /
    custodians), `AmendHoldScope`, `AddCustodian`, `RemoveCustodian`, `ReleaseHold` (maker-checker:
    the issuer cannot release), `ReconcileHoldCoverage`;
  - queries: `EvaluateHoldCoverage`, `GetHold`, `ListHeldRecords`, `GetPropagationStatus` (which
    consuming services have acknowledged the hold);
  - events `legal_hold.issued`, `.amended` and `.released` through the outbox. Check the wire name
    identity-context-svc's existing GOV-10 projection consumes, and keep it working.
- **Done when:**
  - every op is routed, authorized and registered (R5);
  - tests cover each spec negative path, including that release by the issuer is refused and that
    a disposition over a held subject is refused (with G-09);
  - identity-context's projection test consumes the new event.

**G-09 · Retention and disposition**
- **Where:** retention-registry-svc.
- **Spec:** GOV-09.
- **Build:**
  - `PublishRetentionRule`: versioned and effective-dated, keyed by record class + jurisdiction,
    with the period taken from jurisdiction-rules `POST /v1/retention-rules:resolve`;
  - `ScheduleDisposition`;
  - `ApproveDisposition`: maker-checker;
  - `ExecuteDisposition`: an API-5 async operation. Emit `retention.disposition.instructed` to the
    owning service, record its confirmation, and issue a signed disposition certificate;
  - `ReperformDisposition`, `SuspendDisposition`;
  - queries `ResolveRetention`, `GetDispositionEligibility` (always checks G-10 holds),
    `ExplainRetentionDecision`, `ListDueDisposition`.
- **Done when:**
  - eligibility under an active hold is false and execution is refused;
  - the certificate verifies;
  - the approver cannot be the scheduler;
  - all of the above have tests.

**G-07 · Evidence ledger and verification**
- **Where:** governance-decision-log-svc.
- **Spec:** GOV-07.
- **Build:**
  - commands `AppendEvidence`, `SealEvidencePackage`, `VerifyEvidenceIntegrity`,
    `SupersedeEvidenceReference`, `ExportEvidencePackage`;
  - queries `GetEvidence`, `GetEvidenceChain`, `VerifyPackage`, `SearchBySubject` (privileged, with
    purpose required);
  - per-tenant hash chain (`prev_hash`);
  - a sealed package is a manifest of evidence ids + digests + Merkle root, signed (local key until
    SEC-1, then key-management);
  - export is a JSON bundle with a detached signature;
  - append-only triggers on the new tables.
- **Done when:**
  - tampering with one stored row makes `VerifyEvidenceIntegrity` fail (test);
  - a sealed package cannot be changed;
  - `SearchBySubject` without the privileged action → 403;
  - an exported bundle verifies offline (test).

**G-08 · Schema and contract registry lifecycle**
- **Where:** schema-registry-svc.
- **Spec:** GOV-08.
- **Build:**
  - `RegisterDraftSchema` → `RunCompatibilityCheck` (the existing `internal/compat`) →
    `ApproveSchema` (maker-checker) → `PublishSchema` → `DeprecateSchema` → `RetireSchema`;
  - queries `GetActiveSchema`, `CompareSchemas`, `ListConsumers` (with consumer registration),
    `GetCompatibilityReport`;
  - keep the existing `/{eventName}/versions` routes working.
- **Done when:**
  - publishing an incompatible schema without approval is refused;
  - a retired schema cannot be registered against;
  - the existing route tests still pass.

**G-11 · Feature policy and capability exposure**
- **Where:** configuration-feature-flag-svc.
- **Spec:** GOV-11; AA-001 CFG-03.
- **Build:**
  - commands `CreateFeaturePolicy`, `ApproveFeaturePolicy`, `AssignReleaseRing`,
    `EnableTenantFeature`, `DisableTenantFeature`, `EmergencyDisableFeature` (needs a live
    break-glass or emergency session from authorization-svc; closes S3-B1);
  - queries `EvaluateFeaturePolicy`, `ListEffectiveFeatures`, `ExplainFeatureDecision`;
  - a decision = entitlement (commercial-account-svc, read-only) ∧ release ring ∧ tenant override
    ∧ kill state (kill-switch-registry-svc `GET /check`, read-only).
- **Done when:**
  - a feature outside the tenant's entitlement evaluates false with reason `FEATURE_DISABLED`;
  - emergency disable without an emergency session → 403;
  - explain lists each input.

**G-02 · GTRM reads and compiler commands**
- **Where:** tenant-entity-registry-svc (reads), `deployments/gtrm/compiler` (commands).
- **Spec:** GOV-02; `docs/architecture/global-traffic-residency-manager-decision.md`.
- **Build:**
  - reads `ExplainRoutingDecision`, `GetTenantResidencyTopology` and `GetFailoverReadiness`, built
    from the compiled routing map + regions catalog the registry loads, with `ResolveTenantRegion`
    as the existing `ResolveRegion`;
  - compiler subcommands `activate-policy`, `schedule-change`, `failover initiate|complete|abort`
    and `mark-region-unavailable`. Each edits `routing-map.yaml` through the same validation gates,
    stamps provenance and writes an evidence record;
  - no runtime service.
- **Done when:**
  - every compiler gate test passes;
  - failover to a region not allowed by the tenant's residency is rejected;
  - `--check` passes on Windows.

**G-05 / G-03 / G-04 / G-12 · Named-operation parity**
- **Where:** policy-svc, authorization-svc.
- **Build:** for each named operation in GOV-03/04/05/12 with no route of its own:
  - **If a route already does it:** add `operationId` + `x-spec-operation: <Name>` in openapi.
  - **If not:** implement it. Expected ones: `EvaluateStaticConflict`, `EvaluateDynamicConflict`,
    `ExplainSoDDecision`, `ApproveCompensatingControl`, `RevokeCompensatingControl`,
    `EvaluateMakerChecker`, `ListEligibleCheckers`, `GetBreakGlassStatus`.
- **Done when:** every GOV-03/04/05/12 operation maps to a route in openapi, and new ones have
  tests.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 448–514 (“Phase 3: IAM (ZS-IAM-001, `Auth-Service-audit-2026-10-14.md`)”)

#### Phase 3: IAM (ZS-IAM-001, `Auth-Service-audit-2026-10-14.md`)

**IAM-1 · AU-12 per-service audience**
- **Build:** the gateway exchanges the session envelope for one whose audience is the target
  service (`idenv.Exchanger` / `ForwardTo` already exist), and each gate expects its own audience.
- **Done when:** an envelope minted for policy-svc is refused by workflow-svc (test + live probe).

**IAM-2 · AU-14 entitlement stage**
- **Build:**
  - authorization pipeline stage 3 asks commercial-account-svc (COM-03, cached) before candidate
    permissions, with deny reason `FEATURE_DISABLED`;
  - identity-context already calls `WithEntitlementResolver` when `COMMERCIAL_ACCOUNT_URL` is
    set (S1-C1). Set it in compose and k8s, and make it required outside `ENV=local`;
  - feed the config-flag plan input from commercial-account-svc (S3-C1).
- **Done when:** scenario A04 has a test and an unentitled feature is denied.

**IAM-3 · AU-15 legal-hold negative control** (after G-10)
- **Build:** authorization consumes hold events into a projection. Destructive actions on a held
  subject → deny `LEGAL_HOLD`.
- **Done when:** scenario A23 has a test.

**IAM-4 · AU-16 relationship and qualification predicates** (after ORG-4, ORG-5)
- **Build:** a PIP for engagement/matter membership and jurisdiction qualification, with attributes
  read from authoritative services (tenant-registry parties and org units, jurisdiction-rules),
  not from the caller.
- **Done when:** scenarios A24 and A25 have tests.

**IAM-5 · AU-18 authoritative FX** (after ORG-8)
- **Build:** authority limits convert with REF-03 rate sets (tenant-entity-registry `reference`)
  as of the decision time, and record the rate id in evidence. Remove every hard-coded rate.
- **Done when:** grep finds no literal rates, and a test pins the conversion to the rate set.

**IAM-6 · AU-19 observability**
- **Build:**
  - permit/deny/step-up counters, cache hit/miss, revocation-lag histogram and privileged-session
    gauges in authorization-svc;
  - MFA, lockout and replay metrics in identity-context;
  - alert rules in `deployments/prometheus-rules.yml`.
- **Done when:** a `/metrics` test shows the series, and promtool (or a YAML parse) validates the
  rules.

**IAM-7 · AU-20 Definition-of-Done artefacts**
- **Build:**
  - authorization-svc gets `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md` and
    `scripts/audit.sh`;
  - refresh identity-context's and gateway-auth's certificates and `audit.sh`.
- **Done when:** the files exist, asyncapi parses, and `audit.sh` runs against a live container.

**IAM-8 · AU-21 performance and revocation SLOs**
- **Build:** a load test of `/v1/authorize` (k6 or Go) and a revocation-propagation measurement.
- **Done when:** p50, p95 and p99 and the revocation lag are recorded in the progress file and
  compared with the IAM §29 targets.

**IAM-9 · AU-22 role-profile drift**
- **Build:** identity-context also consumes authorization-svc's role-assignment events (admin API,
  expiry sweeper, HR-exit revocations).
- **Done when:** a test shows a grant made directly in authz reaches the envelope `role_profile`.

**IAM-10 · AU-25 canonical event names**
- **Build:** emit the IAM §23 names (`identity.subject.*`, `identity.session.revoked`, …) alongside
  the current wire names, and register both in schema-registry (G-08).
- **Done when:** producer tests assert both names.

**IAM-11 · AU-26 `/v1/iam/*` aliases**
- **Build:** `/v1/iam/access-assignments` and `/v1/iam/delegations` as aliases of the admin routes.
- **Done when:** parity and alias tests pass.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 528–614 (“Phase 4: workflow, obligations and state machines”)

#### Phase 4: workflow, obligations and state machines

**WF-1 · WFC-01…03: close workflow-svc's open rows**
- **Spec:** R-001 WFC-01/02/03; Governance audit "workflow-svc" sections (14, 19 and 21 Oct).
- **Build:** close the 25 partial rows: quorum races, RLS on six tables, the state model, common
  columns, per-type approve authority, and handler tests for the new paths.
- **Done when:** each row is re-scored in the audit file with evidence.

**WF-2 · WFC-04 cases**
- **Where:** workflow-svc.
- **Spec:** R-001 §6.
- **Build:**
  - case families from §6.2;
  - lifecycle OPEN → TRIAGED → IN_PROGRESS → WAITING_INTERNAL / WAITING_EXTERNAL → RESOLVED →
    CLOSED, plus REOPENED;
  - additive, versioned notes;
  - merge (`canonical_case_id` + aliases) and split (lineage kept);
  - confidentiality / legal-privilege labels enforced by authorization (restricted membership),
    not by the UI;
  - a closure gate: every mandatory child work item satisfied, waived or transferred;
  - links to domain records without mutating them;
  - `case.*` events;
  - obligations escalation can open a case.
- **Done when:** tests cover every §6.3 transition, refused closure with open mandatory work,
  merge/split lineage, and a restricted case being unreadable without membership.

**WF-3 · WFC-05 deadline clocks**
- **Where:** obligations-svc.
- **Spec:** R-001 §7.
- **Build:**
  - versioned obligation definitions (trigger, scope, due-date formula, evidence and satisfaction
    rules);
  - instances from trigger events (`event_id`, `event_time`, source version);
  - clock classes HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY;
  - computation evidence: rule version, timezone, `calendar_version_id` from jurisdiction-rules
    calendars, business-day convention, offsets;
  - warning schedule;
  - lifecycle PLANNED, ACTIVE, DUE_SOON, DUE, OVERDUE, FULFILLED, WAIVED, NOT_APPLICABLE,
    SUPERSEDED;
  - satisfaction evidence; waiver and N/A with authority + reason + evidence;
  - supersession when a calendar is corrected (the old due date stays reconstructable);
  - escalation policies (priority, owners, a WF-2 case, a notification);
  - a deterministic DST rule;
  - also close the two ❌ rows: idempotent applicability decisions, and the single owner (C8).
- **Done when:**
  - a scheduler delay cannot suppress OVERDUE (test with a frozen clock);
  - a calendar correction produces a superseding clock and keeps the old one;
  - a waiver without authority → 403;
  - a DST-boundary test passes.

**WF-4 · Group 2 open rows that are inside our services**
- **Build:** after Phase 1 (which closes RFC 9457, Location and pagination rows), close the
  remaining ⚠️ rows in evidence-requirements (18), obligations (22), governance-decision-log (6),
  jurisdiction-rules (4) and policy (2), using each service's section of the Governance audit.
  Rows that need a teammate's service go to cross-team follow-ups.
- **Done when:** each row is re-scored in the audit file with evidence.

**ST-1 · ZS-STATE §11 state-machine definition registry**
- **Where:** workflow-svc.
- **Build:**
  - `StateMachineDefinition` (object_type, semver, states, terminal states, transitions with
    guards and required evidence);
  - immutable once published; migration between versions is an explicit command;
  - routes to publish, get and list definitions.
- **Done when:** a published definition cannot change, and a new version coexists with old
  objects (tests).

**ST-2 · Available actions and transition history on every lifecycle object in our services**
- **Spec:** ZS-STATE §12, §14.
- **Build:**
  - for each lifecycle object (list it in the progress file first):
    `GET …/{id}/available-actions` (server-evaluated for the caller, informative) and
    `GET …/{id}/transitions` (append-only: from/to, command, actor, versions before/after, reason,
    evidence refs, correlation);
  - record `state_machine_version` on each transition row;
  - publish each object's definition in ST-1.
- **Done when:** each object has the two routes and their tests, and the transition tables are
  append-only (trigger test).

**ST-3 · No generic status writes**
- **Build:**
  - replace identity-context `PUT /principals/{id}/status` with named commands
    (`POST /principals/{id}:suspend`, `:reactivate`, `:disable`), keeping the PUT with
    Deprecation/Sunset (API-6);
  - grep the other services for generic status PUT/PATCH and fix them the same way.
- **Done when:** no PUT/PATCH writes a status field in our services, except deprecated aliases.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 615–619 (“Phase 5: organization and reference data (ZS-SVC-I-001, ZS-SVC-U-001)”)

#### Phase 5: organization and reference data (ZS-SVC-I-001, ZS-SVC-U-001)

All in tenant-entity-registry-svc unless stated. Write the `SPEC_DEVIATIONS.md` entry first
(R15): MDM-01…05 implemented as modules of this service.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 629–765 (“**ORG-2 · ORG-03 name parity**”)

**ORG-2 · ORG-03 name parity**
- **Build:** add `operationId`s for `CreateLegalEntity` and `DeactivateLegalEntity` on the existing
  routes.

**ORG-3 · ORG-04 group ownership and control**
- **Build:**
  - `ProposeOwnershipRelationship`, `ApproveOwnershipRelationship` (the proposer cannot approve),
    `SupersedeRelationship`, `EndRelationship`;
  - queries `GetOwnershipGraph`, `GetGroupAsOf`, `ListEntityParents`, `ExplainControlChain`;
  - effective-dated, with cycle and overlap detection and evidence refs;
  - events `OwnershipRelationshipActivated`, `OwnershipRelationshipSuperseded`,
    `ControlRelationshipChanged`.
- **Done when:** the §4.4 minimum acceptance passes, each part with a test: cycle rejected,
  contradictory overlap rejected, group reconstructed as of a date, evidence preserved.

**ORG-4 · ORG-05 org units + MDM-02 org graphs**
- **Build:**
  - `CreateOrgUnit`, `ReparentOrgUnit`, `ChangeManager`, `ActivateOrgUnit`, `DeactivateOrgUnit`;
  - queries `GetOrgUnit`, `ListHierarchy`, `GetHierarchyAsOf`, `FindUnitsByManager`;
  - `GET /v1/org-graphs/{type}?as_of=`, where the hierarchy types (legal ownership, management,
    operational, reporting) are never mixed (MDM §11.2).
- **Done when:** reparenting into a cycle is rejected, and as-of returns the past tree (tests).

**ORG-5 · ORG-07 + MDM-01 + MDM-05 parties and golden record**
- **Build:**
  - `POST /v1/parties`, idempotent on `create_request_id`. It runs an exact-identifier and
    candidate match first, returns `REVIEW_REQUIRED` on ambiguity, and never creates a duplicate;
  - `GET /v1/parties/{id}?as_of=`, `POST /v1/parties/{id}/facts:propose`, party roles add/remove,
    external identifiers link/find, search;
  - `POST /v1/resolution:match`, `POST /v1/resolution/{case}:merge`,
    `POST /v1/resolution/{case}:split` (survivor id, alias/redirect lineage, no id reuse,
    downstream remediation events), `ExplainMergeHistory`;
  - stewardship cases;
  - a party of kind LEGAL_ENTITY links to the ORG-03 `legal_entity_id` (legal entities stay owned
    by ORG-03).
- **Done when:** MDM §10.1 create-flow steps 1–7 and §18.2 merge steps 1–6 each have a test, and
  merged ids still resolve.

**ORG-6 · ORG-08 + MDM-03 addresses and establishments**
- **Build:**
  - `CreateAddress` (structured, with a normalized comparison form; the source value is kept),
    `VerifyAddress` (pluggable verifier: local deterministic rules, external provider BLOCKED),
    `AmendAddress`, `CreateEstablishment`, `ChangeEstablishmentStatus`, `LinkAddressToParty`;
  - queries `GetAddress`, `GetEstablishment`, `ListEntityEstablishments`, `GetEstablishmentsAsOf`;
  - move the existing registered-office route onto this model, keeping the route.
- **Done when:** tests cover each operation, and the registered office still works.

**ORG-7 · ORG-09 + MDM-03 registrations, licences and identifiers**
- **Build:**
  - `CreateRegistration`, `VerifyRegistration`, `RenewRegistration`, `SuspendRegistration`,
    `ExpireRegistration`, `RevokeRegistration`;
  - queries `GetRegistration`, `FindByNumber` (extend the existing by-registry-number),
    `ListExpiringRegistrations`, `GetRegistrationHistory`;
  - `POST /v1/identifiers/{id}:verify`, with LEI alignment (MDM §13.3);
  - when a higher-authority source disagrees, open a conflict case (reuse registry-conflicts);
  - an expiry sweep emits events.
- **Done when:** the §14.1 pipeline steps are tested, and a conflicting higher-authority source
  opens a conflict instead of overwriting.

**ORG-8 · Reference sets (MDM-04, REF-03/06/08/09)**
- **Where:** tenant-entity-registry `reference` module.
- **Build:**
  - `GET /v1/reference/{set}/{code}?as_of=`, with versioned, effective-dated reference releases;
  - **REF-03 FX:** `IngestFXObservation`, `ApproveFXRateSet`, `SupersedeRateObservation`,
    `CloseRateSet`, `GetRateSet`, `ListRatesBySource`, `ExplainDerivedCrossRate`,
    `GetHistoricalRate`. Exact decimals and source-proven; currency codes are validated read-only
    against currency-registry-svc;
  - **REF-06 books:** `CreateAccountingBook`, `ActivateBook`, `AmendBookBasis`, `SuspendPosting`,
    `RetireBook`, `GetAccountingBook`, `ListEntityBooks`, `GetBookAsOf`, `ExplainBookBasis`;
  - **REF-08 dimensions:** `CreateDimensionType`, `CreateDimensionValue`, `ReparentDimensionValue`,
    `ActivateDimensionValue`, `DeactivateDimensionValue`, `MapSourceObject`, `GetDimensionValue`,
    `ResolveDimension`, `GetHierarchyAsOf`, `ListPostingEligibleValues`;
  - **REF-09 units:** `CreateUnit`, `ApproveConversion`, `SupersedeConversion`, `RetireUnit`,
    `GetUnit`, `ConvertQuantity` (exact rational arithmetic), `GetConversionVersion`,
    `ListUnitsByDimension`.
- **Done when:**
  - every listed operation is routed and tested;
  - cross-rate explain shows its source rates;
  - converting with a superseded conversion uses the version effective at the given time.

**ORG-9 · Master imports**
- **Build:** `POST /v1/master-imports` as an API-5 async operation: upload → validate → preview →
  approve → import → reconcile, with per-record errors kept and no partial silent import.
- **Done when:** a bad row blocks approval, reconciliation counts match, and a retry with the same
  key gives the same operation.

**REF-1 · REF-01 jurisdiction registry commands**
- **Where:** jurisdiction-rules-svc.
- **Build:** `ImportJurisdictionUpdate`, `AddInternalJurisdiction`, `AmendJurisdictionAlias`,
  `RetireJurisdiction`, `GetJurisdictionAsOf`, `MapExternalJurisdictionCode`.
- **Done when:** a retired jurisdiction is still readable as of the past and refused for new rules
  (tests).

**REF-2 · REF-07 accounting policy**
- **Where:** policy-svc, policy type `ACCOUNTING_POLICY`.
- **Build:** `DraftAccountingPolicy`, `ApprovePolicyVersion`, `ActivatePolicyVersion`,
  `SupersedePolicyVersion`, `EmergencySuspendPolicy`, `ResolveAccountingPolicy` and
  `ListApplicablePolicies` (by entity, book, jurisdiction and date).
- **Done when:** resolution as of a date returns the version effective then, and emergency suspend
  needs an emergency session (tests).

**REF-3 · REF-10 business calendars and payment terms**
- **Where:** jurisdiction-rules-svc.
- **Build:**
  - extend calendars with `ImportHolidayCalendar`, `IsBusinessDay` and `AddBusinessDays`;
  - payment terms: `ApprovePaymentTerm`, `SupersedePaymentTerm`, `RetirePaymentTerm`,
    `GetPaymentTerm` and `ExplainDueDate` (deterministic, calendar-versioned).
- **Done when:** due dates across holidays and month ends match fixed golden cases.

**JUR-1 · `POST /v1/jurisdiction-decisions:resolve`** (after ORG-5, ORG-6, ORG-7)
- **Where:** jurisdiction-rules-svc.
- **Spec:** ZS-JUR §9, §25.
- **Build:**
  - inputs: legal entity (ORG-03), establishments (ORG-6), registrations (ORG-7), counterparty party
    (ORG-5), plus transaction facts in the request;
  - output: regime and jurisdiction, with an evidence chain;
  - update the `resolve.go` scope note;
  - the external regulatory feed stays BLOCKED (external-data-feed-svc is a teammate's).
- **Done when:** missing facts give a 422 naming the fact, never a guess, and golden cases pass.

**G1-1 · Group 1 register items that are now unblocked**
- **Spec:** Identity audit "07/10 re-audit gaps" B, C and D. Verify each item is still open before
  fixing it.
- **Build:**
  - S7-C1: notification resolves external contacts and cross-tenant relationships from ORG-5
    parties;
  - S2-C1: fiscal-calendar id checked against fiscal-calendar-svc (read-only);
  - S1-B3: ingress-binding refresh from the registry;
  - S3-B3: `secret://` references resolved through the secret-vault broker;
  - S7-B1: privacy-decision-svc PRV-03 call;
  - S7-B2: notice package declared in document-vault `POST /v1/records`;
  - S7-B3: provider secrets leased from secret-vault;
  - S7-C2: legal basis from policy-svc, deadlines from WF-3;
  - S4-C1 and S4-C2: mtls-management URI SAN and CSR signing (with SEC-6);
  - S9-1 and S9-B, if still open.
- **Done when:** each item is re-scored in the Identity audit file with evidence.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 766–818 (“Phase 6: communications (ZS-SVC-Y-001, ZS-COMMS-EMAIL-001)”)

#### Phase 6: communications (ZS-SVC-Y-001, ZS-COMMS-EMAIL-001)

**EML-1 · All 128 templates + 32 variants**
- **Where:** notification-svc catalog. Follow the `DefaultSeedDefinitions` pattern in
  `internal/ledger/catalog.go`; split it into one file per family A–H.
- **Spec:** Email doc §10 (master register), §11 (content), §12 (variants), Appendix B (footer
  modules, prohibited patterns).
- **Build:** for each template, copy the subject, preheader, body, CTA, reference line and footer
  class exactly; set class (T0/S0/…), sender stream and reply mode from its header; set
  `RequiredVariables` from every `{{…}}`. Write both HTML and plain text, HTML-escaped.
- **Done when:** EML-2 passes with 128/128 and 32/32.

**EML-2 · Register parity test**
- **Build:**
  - extract the §10/§11/§12 register once into `internal/ledger/testdata/email_register.json`
    (id, class, trigger, recipient, reply, variables);
  - test catalog ↔ register both ways;
  - lint every template: no prohibited pattern, all variables declared, plain-text twin present,
    no S2/S3 variables in subjects.
- **Done when:** the test passes, and removing any template makes it fail.

**EML-3 · Trigger routing**
- **Build:**
  - a consumer for the 129 trigger events → intent and template selection (variants by §12
    conditions) → recipient resolution (NCD-02) → send through the NCD plane;
  - map spec trigger names to wire names (R9);
  - list every trigger with no producer in the progress file as dormant, naming the service that
    should produce it.
- **Done when:** each live trigger has an end-to-end test from event to rendered communication
  record, and a duplicate event delivers once.

**EML-4 · Event catalog** (after G-08)
- **Build:** register the 128 parent events in schema-registry with payload schemas, owner and
  consumers (email §13 P0-02).

**EML-5 · Email platform controls**
- **Spec:** sections 05–09.
- **Build:** one test per control:
  - message-class and preference precedence (§05);
  - identity, tenant and rendering contract (§06);
  - link controls: signed, expiring, no PII in the URL (§07);
  - sender, DMARC alignment and reply architecture config (§08);
  - accessibility and localization: alt text, `lang`, dark-mode-safe colours, governed locale
    fallback (§09).
- **Done when:** every control has a test. Implement any control that is missing.

**EML-6 · Email §13 backlog**
- **Build:** go through every P0/P1 item notification owns and either implement it or mark it
  BLOCKED with the external need (sender DNS, warm-up).

**NCD-1 · Notification 110-row matrix**
- **Build:** close the 7 partial and 3 unmet rows wherever their dependency now exists.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 819–919 (“Phase 7: security (ZS-SEC-001)”)

#### Phase 7: security (ZS-SEC-001)

**SEC-1 · key-management-svc real custody**
- **Spec:** §10, §11, §12, §25, §26.
- **Build:**
  - Postgres persistence (R4: new DB, role, compose DSN);
  - `KeyRecord` with every §11.1 field;
  - a provider interface with a local AES-256-GCM KEK (the KEK itself from secret-vault). The Cloud
    KMS and Vault Transit adapters are BLOCKED on infrastructure;
  - lifecycle: generate, BYOK import (wrapped), activate (purpose, region, tenant scope,
    algorithm), use (encrypt/decrypt/sign APIs, authorized and logged), scheduled and on-demand
    rotation, re-encryption as an API-5 operation, suspend, revoke/destroy with dual control and a
    dependency assessment, archive, compromise;
  - HYOK: an external key reference with an availability probe, failing closed;
  - routes `POST /security/keys/{id}:rotate`, `:suspend`, `:revoke`;
  - §26 events;
  - maker-checker on disable (Security & Trust audit finding).
- **Done when:**
  - a suspended key refuses encrypt but still decrypts;
  - destroy needs a second approver;
  - re-encryption is resumable;
  - an unavailable HYOK key fails closed;
  - all of the above have tests.

**SEC-2 · Crypto agility (§23)**
- **Where:** key-management-svc.
- **Build:**
  - an algorithm registry with states Preferred / Approved / Transitional / Deprecated /
    Prohibited / Migration target;
  - `GET /security/crypto-inventory` covering keys, mtls certificates (mtls-management), envelope
    signing keys (identity-context JWKS), algorithms and libraries;
  - a versioned encryption envelope (algorithm id + key version in the ciphertext header), used by
    key-management and secret-vault.
- **Done when:** a Prohibited algorithm is refused for new keys, and old ciphertext still decrypts.

**SEC-3 · siem-integration-svc**
- **Spec:** §16, §22, §25.
- **Build:**
  - Postgres store + outbox;
  - an ingestion API with schema validation and redaction (the §16 "must not contain" list);
  - forwarders: RFC 5424 syslog over TLS, and HTTPS JSON (HEC- or Elastic-compatible), with retry,
    backoff and a dead-letter queue. Vendor-specific config is BLOCKED;
  - `POST /security/exceptions` (time-bound, approved);
  - `POST /security/incidents/{id}:contain`: runs an approved action set (end sessions via
    identity-context, revoke certificates via mtls-management, suspend keys via key-management);
  - `GET /security/evidence/{correlation_id}`.
- **Done when:**
  - a restart loses no event;
  - a secret in a payload is redacted before storage;
  - forwarding retries until a local syslog sink receives the event;
  - containment needs approval;
  - all of the above have tests.

**SEC-4 · Tokenization (§14)**
- **Where:** secret-vault-integration-svc.
- **Build:**
  - tokenize, detokenize and mask, each with a purpose and authorization check;
  - a token vault encrypted with SEC-1 keys;
  - deterministic tokens only for approved joinable fields;
  - pseudonymization mapping stored separately and restricted by purpose.
- **Done when:** detokenize without a permitted purpose → 403, masking never returns the raw value,
  and an audit event is written for each detokenize.

**SEC-5 · carta-svc**
- **Build:**
  - replace the two placeholder scoring inputs named in `Security & Trust.md` §11 with real
    signals;
  - add Postgres persistence;
  - add maker-checker where the audit asks for it.
- **Done when:** every scoring input comes from a real source (test), and the audit rows are
  re-scored.

**SEC-6 · mtls-management-svc**
- **Build:** a CSR-signing endpoint (the private key never leaves the workload; closes S4-C2), URI
  SAN `spiffe://zoiko/<env>/<service>` (closes S4-C1), Postgres persistence for certificates and
  revocations, and the remaining items in `Security & Trust.md` §14.
- **Done when:** issuance by CSR works end to end, and revocation survives a restart.

**SEC-7 · `GET /security/policies/effective`**
- **Where:** key-management-svc.
- **Build:** returns the effective crypto, residency and security policy for authorized diagnostic
  use (§25).

**SEC-8 · CI supply chain (§19)**
- **Where:** `.github/workflows/ci.yml`.
- **Build:** a govulncheck step per service; an SBOM per image (syft, SPDX JSON) uploaded as an
  artifact; image signing (cosign keyless with GitHub OIDC) plus a verify step.
- **Done when:** the workflow YAML parses, actionlint is clean, and the PR checks stay green
  (ask the user to paste the run if logs are not accessible).

**SEC-9 · Backup and disaster recovery (§18)**
- **Build:**
  - `deployments/scripts/backup.sh`: `pg_dump -Fc` per DB, a sha256 manifest, and encryption
    (age or gpg) with a key separate from app credentials;
  - `deployments/scripts/restore.sh`: restores into a scratch instance and runs verification
    queries;
  - `docs/architecture/runbooks/backup-dr.md`: RPO/RTO per data class, restore authorization, key
    recovery, ransomware steps.
- **Done when:** a recorded drill backs up the dev stack, restores to scratch, and verification
  passes. Paste the output into the progress file.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 920–960 (“Phase 8: frontend (`zoiko-suite-frontend-platform`, branch `domains-1-2-fe`)”)

#### Phase 8: frontend (`zoiko-suite-frontend-platform`, branch `domains-1-2-fe`)

**FE-0 · Commit the 23 Oct layout restyle**
- **Build:** stage only these files:
  - `app/admin/page.tsx`, `app/globals.css`;
  - `components/admin/layout/{AdminShell,GlobalSearch,NotificationBell,Sidebar,Topbar,UserMenu}.tsx`;
  - `components/admin/overview/KpiCardGrid.tsx`;
  - `components/admin/shared/PageHeader.tsx`, `components/admin/shared/Breadcrumbs.tsx`;
  - `components/ui/{Badge,Button,Card,StatCard}.tsx`, `components/ui/index.ts`;
  - `lib/constants.ts`.
- Leave the authorization console files (`app/admin/authorization/*`, `app/admin/account/`,
  `components/admin/authorization/kit/`, `lib/api/*`, `lib/authz-inbox.ts`, `lib/directory.ts`,
  `package*.json`) to their owner.
- **Done when:** `npx eslint` on those files is clean. `tsc` errors may remain only in the
  authorization console files.

**FE-1 · Console screens for every new backend capability**
- **Screens:**
  - tenant registry tabs: ownership graph, org units, parties and merges, addresses and
    establishments, registrations, reference data, imports;
  - evidence ledger (decision log);
  - schema lifecycle;
  - retention and legal holds;
  - feature policies;
  - cases;
  - deadline clocks;
  - key management;
  - security operations (SIEM, exceptions, incidents);
  - tokenization admin;
  - email template catalog browser.
- **Rules:**
  - real API calls through the existing `lib/api` clients;
  - plain-English rendering, never raw JSON as the only view;
  - honest empty and error states ("could not be read" is not "none");
  - sidebar labels unchanged except for new entries.
- **Done when:** each screen loads against a live service (screenshot or Playwright evidence).

**FE-2 · Security & Trust panels**
- **Build:** wire the four panels to the real client `lib/api/security-trust.ts`, which nothing
  imports today.

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 961–977 (“Phase 9: close-out”)

#### Phase 9: close-out

**CLOSE-1 · Audit files**
- **Build:** append an "Implementation pass" section to each audit file, re-scoring only the rows
  you touched, and update the `SPEC_DEVIATIONS.md` files.

**CLOSE-2 · Postman collections**
- **Build:** update each touched service's collection for the envelope, `Idempotency-Key` and
  `If-Match`.

**CLOSE-3 · CI-equivalent run**
- **Build:** for every touched service, run `go mod verify`, build, vet and
  `go test -race ./...` in `golang:1.25` against `postgres:16-alpine`.
- **Done when:** every service is green, with counts recorded.

---

### A6 · Evidence block (plan §7)

> Source: `IMPLEMENTATION-PLAN-2026-10-23.md`, lines 978–996 (“7. Evidence block (copy into the progress file for every task)”)

#### 7. Evidence block (copy into the progress file for every task)

```
### <TASK-ID> — <title>
Status: TODO | IN PROGRESS | DONE | BLOCKED
Commits: <hash> <subject>
Migrations: <svc>/<NNNNNN_name> (up+down), applied fresh: yes/no, applied via migrate.py: yes/no
Permissions registered: access-control <migration> — <ACTION, …>
Routes: <METHOD path> → <handler>  (openapi validated with: <command>, result)
Wiring: <grep -n … cmd/server/main.go output>
Tests: <command> → <pkg>: <pass>/<fail>/<skip>   (store suites must show 0 skip)
Negative-path tests: <TestNames> ↔ <spec § line>
Live probe (if run): <request> → <status>
Spec rows closed: <doc §…>
Blocked / limits: <what, and what it needs>
```

---

(Plan §8, "How the reviewer checks your work", is folded into the Verifier protocol above.)

---

## Part B · Auth-plane plan: what is still open (gateway GW-5/GW-6, access-control AC-0…AC-FE-2)

### B1 · Global constraints

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 74–123 (“Global constraints”)

#### Global constraints

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

### B2 · Review focus

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 124–146 (“Review Focus”)

#### Review Focus

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

### B3 · Per-service recipe S0–S6

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 147–171 (“The per-service recipe (stages S0–S6)”)

#### The per-service recipe (stages S0–S6)

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

### B4 · gateway-auth-svc: status, GW-5, GW-6, console deviation

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 362–376 (“Service 2 — gateway-auth-svc (:8092, no database)”)

#### Service 2 — gateway-auth-svc (:8092, no database)

> **Status 4 Nov 2026 (verifier check):**
> - **Hold:** GW-0 and GW-1, and the console revision (GW-FE-1 removed, GW-FE-2 diagnostic page).
> - **Built but partial:** GW-2 has no live proof. GW-3 is missing its `TestS3D1_*` tests. GW-4
>   is missing the three new reads.
> - **Not built as specified:** GW-5. Only the gateway's own calls are exchanged; `/verify`
>   does not exchange for the route audience, and its four tests are absent.
> - **Open:** GW-6. The E2E script crashes, there is no SPEC_DEVIATIONS, no re-certification and
>   no live run.
>
> Suite: 9 packages ok, 0 fail. Findings G-1 … G-4 and the order to resume are in
> `Identity, Scope & Foundation-audit-2026-09-23.md` → "gateway-auth-svc status, 4 November
> 2026". access-control-svc (Service 3) should wait until GW-5 and GW-6 are closed.

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 430–461 (“GW-5 · IAM-1 per-service audience (largest; do last)”)

#### GW-5 · IAM-1 per-service audience (largest; do last)
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

#### GW-6 · Contract, docs, live, E2E (S2–S4)
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

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 467–487 (“GW-FE-2 · `/admin/gateway` rebuilt in v2.0”)

#### GW-FE-2 · `/admin/gateway` rebuilt in v2.0
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

### B5 · access-control-svc: AC-0 … AC-FE-2

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 488–577 (“Service 3 — access-control-svc (:8137, DB `access_control`)”)

#### Service 3 — access-control-svc (:8137, DB `access_control`)

##### AC-0 · Baseline and feature matrix (S0)
- The working tree has an in-place repair of `000025_record_version.up/down.sql` (wrong table
  names; wrapped in a transaction with `IF NOT EXISTS`). Keep it — the original never applied
  cleanly — and record the R4 deviation in `progress.md` and `SPEC_DEVIATIONS.md`. Apply it up/down/up
  on a scratch DB, then commit.
- Suite with `-tags=integration` green, counts recorded.
- Extend `services/access-control-svc/progress.md` with the matrix from ZS-IAM-001 §5, §9 (objects),
  §10.1, §19, §21, §24, scenarios touching assignments/reviews, and D1-9.

##### AC-1 · S9-D1 `resource.action` aliases for legacy names
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

##### AC-2 · Production grants by migration
- access-control migration (expected **000028_production_role_templates**): templates carrying the
  notification actions (`TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS`,
  `NOTIFICATION_RESOLVE_OUTCOME`), workflow actions, and register + template `iam.workload.manage`
  (authorization-svc follow-up).
- authorization-svc migration (next free, expected **000041_service_identity_grants**): platform
  role assignments for `svc-access-control` (at least `iam.assignment.revoke` for review REVOKE)
  and `svc-policy`, guarded and idempotent.
- **Tests:** store test per template; authorization IT `TestSvcAccessControlMayRevoke` → PERMIT.

##### AC-3 · Console read gaps + API-4 cursor paging
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

##### AC-4 · API-3 completion
- For every aggregate in 000025 (roles, bundles, protected permissions, assignment requests,
  groups, group assignments, group members, review campaigns, review items) and subject links:
  GET returns `ETag`; every mutation (approve, reject, cancel, revoke, decide, reassign, complete,
  member add/remove, group-assignment commands, template instantiate/upgrade) has three tests —
  missing `If-Match` 428, stale 412, current 2xx.
- Self-approval tests: `TestAC_RequesterCannotApproveOwnRequest`,
  `TestAC_ReviewerCannotDecideOwnAccess`, `TestAC_CriticalRoleNeedsSecurityApprover`.

##### AC-5 · Contract, docs, live, E2E (S2–S4)
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

##### AC-FE-1 · API client
- `lib/api/access-control.ts`: functions for permission definitions (with aliases), role
  templates (+ instantiate, upgrade), access assignments (+ approve/reject/cancel/revoke), review
  campaigns (+ decide, reassign, complete), my review items, groups (+ members, group
  assignments + commands), subject links, refused escalations, summary — all through
  `lib/api/envelope.ts`, sending `If-Match` from `record_version`.

##### AC-FE-2 · `/admin/access-control` rebuilt in v2.0
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

### B6 · BLOCKED items

> Source: `AUTH-PLANE-PLAN-2026-11-02.md`, lines 601–612 (“BLOCKED (build the interface + local implementation, mark only the production part)”)

#### BLOCKED (build the interface + local implementation, mark only the production part)

- External IdP vendor (D-4): federation runs against the mock IdP in tests and an operator-set IdP
  live.
- Production edge WAF/DDoS/bot controls (S5-D1): platform decision; documented placement only.
- Security sign-offs: TOTP as interim factor (D-1), session maximums (D-2), revocation SLO by tier
  (D-5), sender-constrained tokens per client class (AU-27, D-7).
- commercial-account-svc availability for live entitlement / plan checks (IC-6, GW-3): code and
  tests done; live half recorded BLOCKED if the service is not running.
- Traefik `serversTransport` client certificates (S4-B1) and per-service mTLS enrolment (M-01):
  infrastructure.

---

## Part C · Auth service audit (AU-01 … AU-27)

### C1 · Scope, gaps by owner, decisions, deployment state (14 Oct, with the 19 Oct note)

> Source: `Auth-Service-audit-2026-10-14.md`, lines 1–135 (“Auth service — full documentation and behaviour audit (14 October 2026)”)

#### Auth service — full documentation and behaviour audit (14 October 2026)

> **Update, 19 Oct 2026.** The code written for AU-01, AU-04 to AU-11 and AU-27 landed on 15 Oct
> but was not constructed in identity-context-svc's `cmd/server` until 19 Oct. It is wired and
> verified live now: AU-04, AU-06 (identity side), AU-10 closed; AU-05 closed in code; AU-01 half
> closed (sessions end and a session-status endpoint exists, but the gateway and
> authorization-svc do not call it yet); AU-07, AU-08, AU-09 and AU-11 still open (storage only, no
> endpoint). See `Identity, Scope & Foundation-audit-2026-09-23.md`, "Fixes, 19 October 2026".
> The rows below are as found on 14 Oct.

**Scope.** The auth service as one plane:
- **identity-context-svc:** login, MFA, session envelope (GOV-01, Doc 03 §9.1);
- **gateway-auth-svc:** edge authentication (INT-01, Doc 05 §10.1);
- **authorization-svc:** decisions, IAM administration and sessions (GOV-03/04/12, Doc 03 §8.3);
- their wiring with access-control-svc and the callers.

**Baseline.** The code includes the uncommitted 14 Oct auth-plane pass (TOTP step-up, `auth_time`, envelope gate, session caps).

**Sources read for this audit:**
- ZS-IAM-001 (all 32 sections + appendices);
- Governance Control Plane GOV-01/03/04/12, §16–24;
- Doc 05 Security Architecture §3, §5, §6, §10, §11;
- ZS-SEC-001 §8, §9, §10, §11, §13, §16, §17;
- ZS-API-001 §5, §6, §12, §24–27, §30;
- ZS-EVENT-001 (IAM / security families, channels, producer authorisation);
- Doc 03 §8.3 and §9.1;
- ZoikoSuite Domain Ownership (7 Oct), risks R10, R22, R25, R26;
- `backend-completion-tracker.md` rows 79, 81, 82i, 84c.

**Method.**
1. Every requirement was checked against the code by reading it, not against earlier scores.
2. The main gaps were then proven on running binaries (identity-context, gateway-auth, authorization in `require` mode; scratch Postgres with NOBYPASSRLS roles).

Live evidence is marked **L**; code evidence **C**.

##### Verdict

The 85 scored authorization rows pass, and the 14 Oct live E2E passed 41/41. **But the auth
service is not done end to end.**

A fresh reading of every source document finds:
- **12 High gaps.** Revocation, break-glass approval, authority-limit administration, login
  throttling, key management, the login protocol, federation, provisioning, the subject
  lifecycle, the PEP workload gate, the unattributed callers, and every other service still
  trusting `X-Principal-Id`.
- **15 Medium gaps** and **6 Low gaps.**
- **8 sign-off / decision items** and a **front-end track**.

The live behaviour probes matched the documents in **0 of 12** checks. The one apparent
match was a false positive (the input contract answered before routing); re-probed, it is a
gap.

Earlier scores measured what the 85 rows ask. They do not cover authentication protocol,
identity lifecycle, key management or the edge, which the documents also require.

##### A. Gaps inside the three auth services

| ID | Sev | Gap | Doc | Evidence | To make it done |
|---|---|---|---|---|---|
| AU-01 | **High** | **Logout / revocation is not enforced.** After `POST /v1/context/session/{id}/invalidate` (204), the same envelope still opens authorization-svc admin routes (200) and passes gateway `/verify` (200) until it expires (≤ 300 s). Only identity-context's own MFA routes refuse it. | IAM §3 "Revocation … must not wait for cache/token expiry", A11; Doc 05 §5.3 | **L** | Revocation check on the envelope's `session_context_id` at the gateway and the authz gate: a short-TTL revocation set fed by `session.invalidated` / `principal.status.changed`, or an introspection call with a cache. Measure revocation lag (§26). |
| AU-02 | **High** | **Break-glass goes ACTIVE with no independent approval.** It is issued 201 ACTIVE with `approved_by` empty, and there is no Requested → Approved state. | GOV-12 (state model, ApproveBreakGlass, "requester cannot approve own"); Doc 05 §5.5 "require elevated approval"; GOV §1 "independently approved". IAM §14 asks only for notification, so the documents conflict. | **L** | Add the approval state and route (independent approver, own step-up), as JIT already has. Quorum by risk class is GOV §23's open decision. |
| AU-03 | **High** | **Approval and signing authority limits cannot be administered.** The limits are evaluated on every approval decision, but nothing creates or changes them: `CreateAuthorityLimit` has no caller and `POST /v1/admin/authority-limits` is 404. A limit can only be set by direct DB insert, and `iam.authority_limit.changed` can never fire. | IAM §12, §21–22, §23; §8 "no direct runtime mutation"; GOV anti-pattern 12 | **L C** | Governed command API (create / change / retire with maker-checker, reason and version) emitting `iam.authority_limit.changed`; access-control registry action for it. |
| AU-04 | **High** | **No source-level login throttling.** 25 failed logins against 25 accounts from one client: all 401, never 429. Per-account lockout bounds guessing on one account, not password spraying. The edge's global limit (1000 req/s) keys on the immediate peer (`depth: 0`), which behind a load balancer is the balancer. Identity routes are not behind the edge at all (S5-B2). | Doc 05 §10.1; ZS-SEC-001 §8; ZS-API-001 §27; IAM §26 | **L C** | Per-IP and per-tenant rate limits on `/v1/authenticate` and `/v1/authenticate/step-up`, keyed on the real client IP (trusted-proxy depth), plus 429 + `Retry-After`. Route identity through the edge. |
| AU-05 | **High** | **Signing and encryption keys are not managed.** The envelope signing key is a PEM file with a single `kid`, and the JWKS publishes one key, so there is no rotation without breaking live envelopes. The HS256 login-token secret, the MFA key and the client-secret file come from env / files, not KMS or vault. The code's own TODOs say so. | ZS-SEC-001 §10–13 (KMS / HSM, rotation, JIT secrets); Doc 05 §9 | **C** | KMS-backed signing (or an HSM key) with two published keys during rotation; secrets from secret-vault-integration-svc; a documented rotation runbook. |
| AU-06 | **High** | **The login is a password-for-token exchange, not an OIDC flow.** There is no authorization code + PKCE and no authorization-server metadata (`/.well-known/openid-configuration` 404). The confidential-client login added on 14 Oct (client_id + secret + username/password → token) is the resource-owner password grant shape, which §18 prohibits. | IAM §17–18 (code + PKCE; ROPC prohibited; AS metadata); ZS-API-001 §24 | **L C** | A real OIDC provider (first-party or external IdP) with hosted login, code + PKCE, AS metadata and refresh-token rotation. The console then logs in by redirect, and IC's `/v1/authenticate` becomes internal or is retired. **Decision:** build or buy the IdP. |
| AU-07 | **High** | **No enterprise federation.** There is no OIDC or SAML SSO (`SAMLAssertion` returns `ErrSAMLUnsupported`). | IAM §3, §17 (Wave 4); Doc 05 §5.2 | **C** | Federation through the IdP chosen in AU-06, with an issuer / audience / nonce / PKCE profile and controlled claim-to-subject mapping. |
| AU-08 | **High** | **No provisioning.** There is no SCIM 2.0, no just-in-time (JIT) user creation and no group-to-role mapping. Deprovisioning relies on manual status changes; scenarios A20 / A21 are not testable. | IAM §17 (Wave 4), A20, A21, §23 `identity.scim_sync.completed` | **C** | SCIM endpoint (users / groups), `externalId` binding, governed group mappings, termination-priority deprovisioning. |
| AU-09 | **High** | **No subject lifecycle.** Nothing creates a subject or binds an authentication identity, invites a user, or sets, changes or resets a password; there is no password policy. Principals exist only through seed tools. `identity.subject.created` / `identity.subject.disabled` are never produced. | IAM §2, §22, §23; Doc 05 §5.3; ZS-SEC-001 §10.1 | **L C** | Subject + authentication-identity admin API; invite / activation; self-service change and governed reset (separately controlled recovery, IAM §3); password policy including a breached-password check. |
| AU-10 | Med | **No session continuation.** The login token lives 300 s and the envelope 300 s, with no refresh token. A console must re-prompt for the password or hold it. | IAM §18 (refresh rotation); Doc 05 §5.3 bounded TTL | **C** | Comes with AU-06: refresh tokens with rotation and reuse detection, bound to the session record (so AU-01 revocation covers them). |
| AU-11 | Med | **Only TOTP.** There are no passkeys / WebAuthn, the preferred phishing-resistant factor. | IAM §3, §13 | **C** | WebAuthn factor in the same `principal_credentials` model; assurance level `PHISHING_RESISTANT`. |
| AU-12 | Med | **One audience for everything.** Every envelope has `aud=zoiko-internal`, so a token captured at one service is valid at all of them. | IAM §18 "narrow resource audience"; ZS-API-001 §24 | **L** | Per-service or per-resource-family audiences, or token exchange at the gateway. |
| AU-13 | Med | **Privileged activity is not evidence-linked.** Decision evidence has no structured privileged / break-glass / support session ID (break-glass appears only inside the basis text), no `BREAK_GLASS` tag or obligation, and support sessions produce no summary of resources accessed. "Commands executed under a session" cannot be listed for review. | IAM §13–15, §20; GOV-12 evidence; ZS-SEC-001 §17 | **C** | Session-ID columns on `access_decision_log`, a `BREAK_GLASS` reason code / obligation, and a per-session activity read used by the review routes. |
| AU-14 | Med | **No product-entitlement stage.** Pipeline stage 3 (scenario A04) is absent: authz never asks commercial-account-svc / capability-registry. | IAM §7 stage 3, A04; GOV-03 dependencies | **C** | Entitlement check (cached, COM-03) before candidate permissions, with a `FEATURE_DISABLED` / deny reason. |
| AU-15 | Med | **No legal-hold negative control.** A delete is not refused under hold (A23). | IAM §7 stage 6, A23 | **C** | Hold projection (GOV-10 events) as a negative control on destructive actions. |
| AU-16 | Med | **No relationship or qualification predicates.** There is no engagement / matter membership (A24) and no jurisdiction qualification (A25). Subject attributes are whatever the calling service sends: there is no attribute source (PIP) owned by an authoritative service. | IAM §6.2, §8 PIP, A24, A25 | **C** | A PIP for subject attributes (HR / qualifications) and relationship providers (audit engagements, legal matters). |
| AU-17 | Med | **Policy is not published under governance.** SoD rules, ABAC rules, roles and bundles take effect the moment they are created; there is no draft → approve → publish lifecycle. They are versioned and evented, but not independently approved. | IAM §8 PAP "no direct runtime mutation without controlled publication"; GOV §1 | **C** | Approval step (maker-checker) before activation for policy artefacts, as policy-svc's GOV-05 lifecycle does. |
| AU-18 | Med | **Approval limits use hard-coded FX rates** (e.g. 1 USD = 0.78 GBP in source), not an authoritative FX basis. | IAM §12, A09; §31 "authoritative FX basis" | **C** | Rates from currency-registry-svc with as-of evidence; §31 decision. |
| AU-19 | Med | **authorization-svc observability.** It has HTTP metrics only: no permit / deny / step-up rates, cache health, revocation lag or privileged-session gauges, and no alert rules of its own. identity-context has no MFA metrics or alerts (lockouts, replays, step-up failures). | IAM §26 | **C** | Domain metrics + alert rules (deny spikes, self-approval attempts, break-glass active, MFA lockout / replay bursts). |
| AU-20 | Med | **authorization-svc lacks its DoD artefacts.** It has no OpenAPI, AsyncAPI, runbook, release certificate or audit script. identity-context and gateway-auth certificates and `audit.sh` (17 Sep) predate the 14 Oct changes. | GOV DoR #3, DoD #1, #11, #12; IAM §29 | **C** | Publish contracts (OpenAPI 3.1, AsyncAPI), runbook, re-certified release certificates, updated audit scripts. |
| AU-21 | Med | **Performance and revocation SLOs are unmeasured.** There is no load test of `/v1/authorize` after the 14 Oct changes and no measured revocation propagation. The ownership plan's R22 (hot synchronous path) is unchanged: there is no PEP-side cache or sidecar. | IAM §29 Load / Revocation; Doc 05 §6.5; Doc 03 §3.9; R22 | **C** | Load test against the NFR C0 budget; revocation-lag measurement; decide on a PEP sidecar / local decision cache. |
| AU-22 | Med | **Role-profile projection drift (R10, tracker 79).** identity-context projects role assignments only from access-control-svc's events. Grants and revocations made directly in authz (admin API, expiry sweeper, HR-exit revocations) never reach it, so envelope `role_profile` can be stale. | Domain Ownership R10; tracker 79 | **C** | Consume `iam.assignment.*` from `zoiko.authorization.events` (or make authz the single writer and drop the duplicate). |
| AU-23 | Low | **CARTA.** A `STEP_UP_MFA` answer hard-blocks instead of sending the user to the step-up that now exists, and CARTA fails open when unreachable (an S5-D2 decision). | Doc 05 §3.11, §5.4 | **C** | Map `STEP_UP_MFA` → `401 step_up_required`; decide the fail mode. |
| AU-24 | Low | **Error and request-ID format.** Errors are not RFC 9457 `application/problem+json`, and there is no `Zoiko-Request-Id` (`X-Request-Id` is used instead). | ZS-API-001 §6, §12 (B-02) | **L** | Platform-wide X9 change. |
| AU-25 | Low | **Event names and channels drift.** <br>• ZS-IAM-001 §23 names `identity.subject.*` / `identity.session.revoked`, but identity emits `principal.status.changed` / `session.invalidated`. <br>• ZS-EVENT-001 wants `zoikosuite.<domain>.<stream>.v1` and `security.privileged-session.*`, but authz uses `zoiko.authorization.events` and `security.privileged_session.*`. <br>• There is no producer or consumer ACL on the broker. | IAM §23; EVENT §5.2, §14 | **C** | Resolve the IAM-vs-EVENT naming conflict, then alias or rename; broker ACLs. |
| AU-26 | Low | **API paths drift from §21.** The routes live at `/v1/admin/role-assignments` and `/v1/admin/delegated-authorities`, not `/v1/iam/access-assignments` and `/v1/iam/delegations`. authz also keeps a delegation write API that duplicates delegated-authority-svc (tracker 81). | IAM §21; tracker 81 | **C** | Aliases or the documented paths; retire the duplicate writer once the console has moved. |
| AU-27 | Low | **Bearer envelopes are not sender-constrained.** There is no DPoP or mTLS binding, so a stolen envelope replays for ≤ 300 s. | IAM §18; §31 decision | **C** | Decide per client class (§31). |

##### B. Gaps that need other services

| ID | Sev | Gap | Evidence | Needs |
|---|---|---|---|---|
| X-R3 | **High** | **Every other service still takes `X-Principal-Id` / `X-Tenant-Id` on trust** when called directly. Only authorization-svc (and identity's MFA routes) verify the envelope. ZS-SEC-001 §9: forwarded context must be "signed … and revalidated by the receiving service". | **C** (grep of the estate) | Each service adopts authz's `internal/identity` gate (a shared module would be better). |
| X-82i | **High** | **About 86 `/v1/authorize` callers send no envelope.** Their decisions are logged without tenant or principal attribution (invisible in the audit read), and `ZS_ENVELOPE_ENFORCEMENT` cannot go `strict`. | tracker 82i | Each caller forwards the envelope (policy-svc's client is the reference). |
| X-M02 | **High** | **The decision endpoint answers anyone.** With no certificate and no headers: 200 (permit mode). Production defaults to `require`, but no PEP holds a client certificate yet, so require mode cannot go live. | **L** | Each PEP provisions its certificate and is added to `AUTHZ_PEP_ALLOWLIST`. |
| X-3 | Med | **PEPs do not forward `X-Authn-Age-Seconds`**, so `AUTHZ_STEP_UP_UNKNOWN_AGE=permit` and reopen-class step-up (A26) does not bite. | **C** | Callers forward the gateway's age header. |
| X-1 / X-2 | Med | **Tenantless callers, and no preparer on own-object actions** (REQUIRE_APPROVAL fallback). | register | Calling services. |
| X-84c | Low | **Always-grant `Authorize` stubs still exist in 7 services** (migration-integrity, tax-authority-interface, reporting-orchestration, hris-connector, external-data-feed, esignature-integration, banking-connector). They are dead code (0 callers) and a latent always-allow. | **C** | Delete them. |

##### C. Front end (zoiko-suite-frontend-platform; not on this machine, unverified)

1. Send `Authorization: Bearer <envelope>` on every authorization-svc call; then compose →
   `AUTHZ_IDENTITY_ASSERTION=require`.
2. MFA UI:
   - enrolment (QR from `otpauth_uri`) and activation;
   - on `401 step_up_required`: prompt → step-up → re-resolve → retry;
   - admin reset page.
3. Send the client credentials at login (then `IDP_CLIENT_AUTH=required`). This is superseded
   by AU-06 if a real OIDC flow is adopted.
4. Route console → service calls through the edge (S5-B2).
5. Render server `available-actions` and denial / step-up states (IAM §29).

##### D. Decisions and sign-offs (no code can close these)

| ID | Decision | Owner |
|---|---|---|
| D-1 | TOTP as the interim high-assurance factor; the assurance taxonomy (IAM §31) | Security |
| D-2 | Session maximums 8 h / 2 h / 4 h (IAM §31) | Security |
| D-3 | Break-glass approval and quorum by risk class (GOV §23; the IAM §14 vs GOV-12 / Doc 05 conflict) | Security + Finance / Control + Ops |
| D-4 | Build or buy the IdP / OIDC provider (AU-06 to AU-08) | Architecture + Security |
| D-5 | Revocation SLO by risk tier (IAM §31) | Security |
| D-6 | Authoritative FX basis for authority limits (IAM §31) | Finance / Control |
| D-7 | Token format and sender-constrained tokens per client class (IAM §31) | Security |
| D-8 | Platform items: RFC 9457 (X9); WAF / DDoS at the GCP edge; real client IP behind the load balancer; delete the stale `docker-compose.override.yml`; `init-db.sh` role ordering | Platform |

##### E. Deployment state

- **Real databases are behind the code.**
  - identity_context: 000012, 000013;
  - authorization_svc: 000033 (+ earlier ones where missing);
  - access_control: 000020.
- **Staging / production:** `MFA_SECRET_KEY` (fatal without it); `AUTHZ_IDENTITY_JWKS_URL`
  (https :8449 — mTLS key fetch unexercised live while the override turns authz mTLS off).
- **Enrolment:** session issuers and approvers must enrol TOTP before they need to.
- **Commit state:** all 14 Oct work is uncommitted.

### C2 · Re-score of AU-01 … AU-11 (3 Nov)

> Source: `Auth-Service-audit-2026-10-14.md`, lines 195–216 (“Re-score of AU-01 to AU-11, 3 November 2026 (identity-context-svc)”)

#### Re-score of AU-01 to AU-11, 3 November 2026 (identity-context-svc)

Scored against the running stack, not against earlier write-ups: identity-context-svc image rebuilt
3 Nov 16:34, `deployments/scripts/e2e-identity-context-deps.py` 61/61 PASS, `scripts/audit.sh`
live 37/38 (the one failure is section 2, host `go test` binaries blocked by Windows Application
Control; the same suite in a golang:1.26 container is green), plus a live probe with
`FEDERATION_ENABLED=true`. AU-02 and AU-03 are authorization-svc rows and are not re-scored here.

| Row | Was (14 Oct) | Now | Evidence |
|---|---|---|---|
| AU-01 | Open: a revoked envelope still worked | **Closed** | E2E: authorization-svc refuses the invalidated envelope 5.1 s after `POST .../invalidate` (session introspection) |
| AU-04 | Open: 25 failed logins, never 429 | **Closed (identity side)** | Per-IP and per-tenant budgets on `/v1/authenticate`, step-up, pre-session routes and the hosted login; `TestAU09_PreSessionRoutesThrottled`, `TestHostedLoginIsThrottledAfterRepeatedFailures`, `TestAU11_HostedLoginRefusesAWrongPasskeyAndThrottles`. Edge throttling is gateway-auth-svc GW-2 |
| AU-05 | Open: one key, secrets in env | **Closed in code** | Keyring + KMS interface (19 Oct). Live JWKS publishes one key (`local-dev-key-1`); a rotation with two published keys has not been exercised live |
| AU-06 | Open: password-for-token, no OIDC | **Closed** | Discovery, code + PKCE S256 only, unregistered client refused without redirect (audit §7b); `/v1/authenticate` still open locally for the console |
| AU-07 | Open: no federation | **Closed for OIDC** | Live with `FEDERATION_ENABLED=true`: connections and links read 200 for the admin, 403 without a grant, a forged id_token 401 with the uniform answer. The probe found every admin route 503 (tenant id passed as the legal entity) and the six FEDERATION_* actions unregistered; both fixed (access-control 000027). SAML is a recorded deviation pending D-4 |
| AU-08 | Open: no SCIM | **Closed in code** | SCIM create/PATCH/DELETE with termination-priority deprovisioning (`internal/federation`, store suite). Not exercised live: provisioning is off on the local stack (`/scim/v2/*` 404) |
| AU-09 | Open: no subject lifecycle | **Closed** | E2E: workload created ACTIVE without a password, person PENDING_ACTIVATION, idempotent replay, maker-checker reset (requester refused), the new Password resets register, forgot-password identical for known and unknown accounts; reset decisions now carry a reason |
| AU-10 | Open: no refresh | **Closed** | Refresh rotation with reuse detection ending the login session (`TestRefreshRotatesAndReuseEndsTheSession`) |
| AU-11 | Open: TOTP only | **Closed** | Passkeys (go-webauthn v0.17.4): registration, hosted-login second step, step-up to `PHISHING_RESISTANT`, revoke, administrator reset. E2E with a software P-256 authenticator: registration verified, an assertion from another origin refused, step-up envelope `assurance_level: PHISHING_RESISTANT`, a spent ceremony refused, sign-in event at `PASSKEY`, enrolled and step-up events in the outbox. A browser platform authenticator has not been exercised (headless Chrome has none) |

Still open from this list: AU-05 live rotation, AU-08 live SCIM, SAML (D-4), and the console's
passkey ceremony in a real browser.

---

## Part D · Domain 1 audit: Identity, Scope & Foundation (current state)

### D1 · Method and legend

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 1–15 (“Group 1 — Identity, Scope & Foundation: documentation-compliance audit”)

#### Group 1 — Identity, Scope & Foundation: documentation-compliance audit

**Date:** 23 September 2026 (identity-context-svc re-audited 28 September 2026; tenant-entity-registry-svc re-audited live 29 September 2026; configuration-feature-flag-svc remediated 29 September 2026; secret-vault-integration-svc remediated 29 September 2026; gateway-auth-svc remediated and re-audited 29 September 2026; search-indexer-svc remediated, re-audited live and remediated again 30 September 2026; notification-svc remediated and verified live 30 September 2026, re-audited and six further defects fixed 5 October 2026, in-service gaps closed and full matrix scored 6 October 2026; delegated-authority-svc independently re-audited in two passes and seven defects fixed 6 October 2026; access-control-svc re-audited, two regressions and five gaps fixed 6 October 2026, and its unbuilt §5/§9/§24 scope built the same day; all nine re-audited 7 October 2026, six findings, fixed 8 October: see "Group 1 re-audit, 7 October 2026"; every gap open then, grouped by what it waits on, is in "07/10 re-audit gaps" after it; **all nine re-audited again 19 October 2026: eight new findings (F-1 … F-8), none fixed yet, including a delegated-authority-svc create that fails on every call and an image identity-context-svc can no longer build: see "Group 1 re-audit, 19 October 2026" at the end**; **identity-context-svc re-audited 4 November 2026: seven findings (K-1 … K-7), none fixed yet, NP4 ✅ → ⚠️, and every open identity-context-svc gap in one register: see "identity-context-svc re-audit, 4 November 2026" and "identity-context-svc gap register, 4 November 2026" at the end**; **gateway-auth-svc AUTH-PLANE pass checked 4 November 2026: GW-0/GW-1 hold, GW-2…GW-4 partial, GW-5 not built as specified, GW-6 open, no score change: see "gateway-auth-svc status, 4 November 2026" at the end**)
**Scope:** all nine services of Group 1, audited one at a time.
**Source of truth:** the original `.docx` specifications in `docs/architecture/`, **not** the
per-service `openapi.yaml` / `asyncapi.yaml` artefacts. This is deliberate: auditing code
against a contract written alongside it cannot catch a case where both encode the same wrong
assumption.

**Method (per service):** extract the documented contract (endpoints, schemas, status codes,
auth, business rules, side effects, events) → extract the implemented contract from the code →
diff the two → report. Anything ambiguous in the documents is flagged **needs clarification
(❓)** rather than scored as a pass or a fail.

**Legend:** ✅ match · ⚠️ partial · ❌ missing · ❓ needs clarification

### D2 · Scores

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2967–2981 (“Scores after the fixes”)

#### Scores after the fixes

| # | Service | 19 Oct re-audit | **After fixes** | What changed |
|---|---|---|---|---|
| 1 | identity-context-svc | 41/44: 93.2% (95.5%) | **42/44: 95.5% (96.6%)** | NP4 ⚠️ → ✅ (F-3) |
| 2 | tenant-entity-registry-svc | 45/48: 93.8% (96.9%) | **47/48: 97.9% (99.0%)** | Rows 22 and 34 ⚠️ → ✅ (F-2) |
| 3 | configuration-feature-flag-svc | 47/49: 96% (98%) | **47/49: 96% (98%)** | No change |
| 4 | secret-vault-integration-svc | 18/21: 86% (93%) | **18/21: 86% (93%)** | No change |
| 5 | gateway-auth-svc | 22/22: 100% | **22/22: 100%** | No change |
| 6 | search-indexer-svc | 47/49: 96% (98%) | **47/49: 96% (98%)** | F-6 fixed; both affected rows also carry F-4/R-7 history, so the semantic rows stay ⚠️ |
| 7 | notification-svc | 35/37: 95% (97%) | **35/37: 95% (97%)** | No change |
| 8 | delegated-authority-svc | 21/23: 91.3% (93.5%) | **23/23: 100%** | DelegateAuthority ❌ → ✅; idempotency scope ⚠️ → ✅ (F-1, 000013) |
| 9 | access-control-svc | 10/10: 100% | **10/10: 100%** | No change |
| | **Group** | 286/303: 94.4% (96.9%) | **291/303: 96.0%** (296.5 weighted, **97.9%**) | |

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 3176–3188 (“Score”)

#### Score

| Service | Before (19 Oct, after fixes) | **4 Nov** | What changed |
|---|---|---|---|
| identity-context-svc | 42/44: 95.5% (96.6% weighted) | **41/44: 93.2% (95.5%)** | NP4 ✅ → ⚠️ (K-1) |
| **Group 1** | 291/303: 96.0% (97.9%) | **290/303: 95.7% (296.0 weighted, 97.7%)** | No other service was re-scored |

The other two rows that fall short of full are unchanged:
- **Entitlement context reference (❌).** `COMMERCIAL_ACCOUNT_URL` is still unset in compose,
  and commercial-account-svc does not run locally.
- **Source channel (⚠️).** Compose now requires the console's client. The console sends it only
  when its client environment variables are set, and its `.env.example` leaves them empty (K-5).

### D3 · Open gap register for the nine services (7 Oct; identity-context rows replaced by D5)

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2327–2462 (“07/10 re-audit gaps: every gap open on 7 October 2026”)

#### 07/10 re-audit gaps: every gap open on 7 October 2026

This register lists every gap still open in Group 1 after the 7 October re-audit. It brings
together the six new findings (R-1 … R-7) and every gap the earlier passes left open or
dependent on another service. **Nothing here is fixed yet.** Each gap is placed under what it
waits on. When a dependency is finished, take the rows under it, fix them, and re-check them
the way the "Re-check" column says.

Gap ids are `S<service>-<n>`, so S1 is identity-context-svc and S9 is access-control-svc.
Where a gap came from the 7 Oct re-audit, its R-number is given too.

> **19 Oct:** eight new group A gaps (S8-1, S2-1, S1-5 … S1-8, S6-3, S0-1), S1-C1 moved to
> group B, and S5-B1 closed. See "Register changes, 19 October" at the end of this document.
>
> **4 Nov:** the identity-context-svc (S1) rows are replaced by "identity-context-svc gap
> register, 4 November 2026" at the end: S1-B2 and S1-B3 closed, and S1-9 … S1-13, S1-B4,
> S1-B5 and S1-D1 … S1-D3 added.

**Summary**

| Group | What it waits on | Gaps |
|---|---|---|
| A | Nothing. Fixable now inside the service | 17 |
| B | A service that already exists (connect or change it) | 18 |
| C | A service or infrastructure that does not exist yet | 17 |
| D | A decision by the spec or service owners | 14 |
| | **Total** | **66** |

##### A · Fixable now, inside the service (no dependency)

The first seven are the 7 Oct findings, ranked by risk. Fix these first.

| Id | Service | Gap | Spec | Fix | Re-check |
|---|---|---|---|---|---|
| S1-1 (R-2) | identity-context-svc | Break-glass approver is a request-body field. It never consents and need not exist; caller = approver passes the local code | GOV-01 §4 AttachSupportContext; §1 "independently approved"; §2 inv. 8 | Make approval a separate call by the approver (tenant-entity-registry `approval_requests` pattern); grant only after it. Refuse caller = approver locally | Attach naming a nonexistent approver → no grant; an approval by the requester → 403 `SOD_CONFLICT`; a real approver's approval → grant |
| S3-1 (R-4) | configuration-feature-flag-svc | Proposer can approve and activate their own C2/C3 change; `approved_at` is taken from the body | AA-001 §8.1 (C2 WFC approval, C3 segregated approval), §10.1 | Refuse approver = `created_by_principal_id` for C2/C3; set `approved_at` on the server | Store test: maker approves own C3 → refused; backdated `approved_at` ignored |
| S3-2 (R-5) | configuration-feature-flag-svc | `ApproveChange` has no status guard, so a VERIFIED change goes back to APPROVED or PROPOSED | AA-001 §8 | `WHERE status = 'PROPOSED'`; anything else → 409 | Approve/reject a VERIFIED change → 409, status unchanged |
| S7-1 (R-6) | notification-svc | NP-11 "controlled exception" is three unchecked strings (`exception_ref`, `verification_ref`, `reviewer_principal_id`); the endpoint is then marked verified | Y-001 §5.2, NP-11, INV-15 | Resolve `exception_ref` against `ncd_exceptions` (open, right kind, approved by the named reviewer, reviewer ≠ caller) | Made-up refs → refused `NCD-007`; a real approved exception → accepted. Rescore NP-11 and INV-15 |
| S1-2 (R-3) | identity-context-svc | Kafka consumer auto-commits (`ReadMessage`) and claims the event id before acting. A failed session eviction drops the revocation for good | GOV-01 NP4; GOV inv. 10 (at-least-once) | `FetchMessage` + commit after success; release the Redis claim when the side effect fails | Fault-inject `EvictAllForPrincipal` once → the event is redelivered and the sessions end |
| S1-3 (R-1) | identity-context-svc | `POST /v1/context/resolve` needs no correlation; a header-only correlation is dropped, and sessions are stored with `correlation_id = ""` | GOV-01 §4 "correlation required"; inv. 9 | `requireCorrelation` on resolve; take the header when the body has none | No correlation → 400; header only → stored |
| S6-1 (R-7) | search-indexer-svc | Dockerfile (5 Oct merge) copies gitignored prebuilt binaries; `docker build` fails from a clean checkout | §9.2-style DoD (buildable, reproducible) | Restore the two-stage build | `docker build -f services/search-indexer-svc/Dockerfile .` succeeds on a clean checkout |
| S1-4 | identity-context-svc | `iam.assignment.granted` (keyed by request id) and review-driven `iam.assignment.revoked` (keyed by assignment id) can reorder, so a late grant re-opens a revoked projection | GOV-01 NP4 | Key both by the assignment id in access-control-svc. In the projection, never insert over a recorded revocation (tombstone) | Deliver revoke then grant → role not framed |
| S6-2 | search-client | `EnsureIndex` treats any error containing "404" as "index missing" | — (robustness) | Check the response status, not the error text | Unit test with a non-404 error |
| S3-3 | configuration-feature-flag-svc | Store suite (68 tests) passes with every test skipped when no DB is set | AA-001 §14 certification | Fail when `REQUIRE_DB_TESTS=1` and no DB (CI sets it) | CI shows the store tests run |
| S7-2 | notification-svc | NP-34: no post-upload attachment comparison | Y-001 NP-34 | Compare the provider's stored attachment hash with the pinned one once a provider transmits attachments | Matrix row NP-34 → Met |
| S7-3 | notification-svc | NP-57: the stranded-attempt path has no test | NP-57 | Add the test | Matrix row → Met |
| S7-4 | notification-svc | INV-29: projections cannot write, true by construction but untested | INV-29 | Add the test | Matrix row → Met |
| S7-5 | notification-svc | TC-03: no fuzz test | TC-03 | Fuzz the template validator and renderer | Matrix row → Met |
| S7-6 | notification-svc | TC-18: no DLP check | TC-18 | DLP scan on rendered content | Matrix row → Met |
| S9-1 | access-control-svc | `DORMANT` counts a decision granted through several roles as use of each | Authz Std §24 | Attribute use to the role that granted the decision (needs the granting role in the decision log, see S9-B) | Two roles, one used → only the used one is not dormant |
| S5-1 | deployments/gtrm | `gtrm-compiler --check` reports drift on a Windows checkout (CRLF working copy) | — (tooling) | Normalise line endings before comparing, or add `.gitattributes eol=lf` for the artefact | `--check` OK on Windows |

##### B · Closable now: connect to, or change, a service that already exists

| Id | Service | Gap | Spec | Depends on (exists) | What closing it takes |
|---|---|---|---|---|---|
| S1-B1 | identity-context-svc | Source channel: web vs mobile cannot be known here | GOV-01 §4 server-resolved source channel | **gateway-auth-svc** | Edge stamps `X-Source-Channel` and strips the client's copy |
| S1-B2 | identity-context-svc | No service grants the six `IDENTITY_*` actions; protected routes answer 403 against a real authorization-svc | GOV-01 §4 authorization | **authorization-svc** | Seed the actions into operator roles by migration |
| S1-B3 | identity-context-svc | Ingress bindings are never refreshed from the tenant registry; every binding reads `STALE` past the TTL | GOV-01 §4 cache state | **tenant-entity-registry-svc** | Push or poll binding changes (event or scheduled refresh) |
| S5-B1 | gateway-auth-svc | SEC-INV-05: the gateway's mTLS client is ready, but the peers have no mTLS listener | Security Std §3.1 SEC-INV-05, §9 | **identity-context-svc, tenant-entity-registry-svc** | `:8449` listener with `RequireAndVerifyClientCert` in both, then `*_MTLS_ENABLED=true` |
| S5-B2 | gateway-auth-svc | Console → service calls bypass ForwardAuth; services trust the `X-*-Id` headers the console server sends | Security §8, SEC-INV-01/04 | **zoiko-suite-frontend-platform** + gateway | Route console server calls through the edge, or sign them; then audit as its own item |
| S5-B3 | all services | The 8 §4 pass-through fields (`X-Purpose-Context`, `X-Approval-Reference`, `X-Evidence-Refs`, …) are caller assertions; each owning service must validate them | GOV §16 envelope | **each owning service** | Every service that acts on one confirms it (e.g. the approval exists and names this action) |
| S3-B1 | configuration-feature-flag-svc | Emergency changes are not restricted to designated actors, and need no IAM break-glass session | AA-001 §8.4, §10.1 | **authorization-svc** (+ identity-context break-glass) | Dedicated emergency grant; require a live support/break-glass context on `/emergency-changes` |
| S3-B2 | configuration-feature-flag-svc | Runtime attestation reads `X-Workload-Id`, which the edge strips; only service-to-service calls can carry it, and nothing sets it from a verified identity | AA-001 §10.1 `/runtime/attest` (workload identity) | **mTLS (mtls-management-svc)** | Take the workload from the client certificate, as secret-vault-integration-svc does |
| S3-B3 | configuration-feature-flag-svc | `secret://<env>/<path>` references are resolved by nothing | AA-001 §7.2, INV-10 | **secret-vault-integration-svc** | Resolve references through the broker |
| S3-B4 | configuration-feature-flag-svc | The dev `configuration_feature_flag` DB is at `000003` (code needs `000013`); `audit.sh` (25 routes) was never re-run live | — (deployment) | **migrations runner** | `migrate.py up`, then run `audit.sh` |
| S4-B1 | secret-vault-integration-svc | The gateway has no client certificate for the gateway → service hop (O-1 forwarder) | Security §9 | **gateway-auth-svc** + mtls-management-svc | Provision it; list it in `MTLS_TRUSTED_FORWARDERS` |
| S6-B1 | search-indexer-svc | authorization-svc's DB is behind its code (`pra.book_id`), so `/v1/authorize` answers 503; the grant-dependent `audit.sh` sections 7–12 and 16 never ran | AB-001 §11 | **authorization-svc** | Apply its migrations, re-seed, re-run `audit.sh` |
| S7-B1 (G-1) | notification-svc | Privacy/marketing permission is caller-supplied (one of the 2 partial API rows) | Y-001 §5.3, NP-17, NP-47, INV-30 | **privacy-decision-svc** (PRV-03) + privacy-purpose-registry-svc (PRV-01) | Register purposes, then call `POST /v1/privacy/decisions` in the channel decision |
| S7-B2 (G-2) | notification-svc | No record declaration; the evidence package waits on a callback (the other partial API row) | Y-001 §8.1, NP-58, TC-15, TC-20 | **document-vault-svc** (DRC-02/03) | Declare the notice package with `POST /v1/records` |
| S7-B3 (G-3) | notification-svc | Provider, callback, webhook, unsubscribe and DKIM secrets are environment variables | INV-26 | **secret-vault-integration-svc** | Lease them at start-up and on rotation |
| S7-B4 (G-9) | notification-svc | `TEMPLATE_MANAGE/APPROVE`, `NOTIFICATION_SUPPRESS`, `NOTIFICATION_RESOLVE_OUTCOME` are granted only in the dev seed | — | **authorization-svc** | Grant them by migration (after S6-B1's migration fix) |
| S7-B5 (G-8, G-7 part) | notification-svc | Console covers only the legacy register; the send form uses the free-text legacy path; regulated-notice acknowledgment is API-only | Y-001 §8.3, §10.1, INV-01 | **zoiko-suite-frontend-platform** | Screens for the plane, a recipient acknowledgment page, and the send form moved to `POST /v1/communications` |
| S9-B | access-control-svc | A REVOKE review decision provisions as the reviewer, who needs `iam.assignment.revoke`, or the item stays open (`403 provisioning_forbidden`); DORMANT attribution needs the granting role (S9-1) | Authz Std §24 | **authorization-svc** | A service identity this service can revoke as; the granting role recorded in the decision log |

##### C · Blocked on a service or infrastructure that does not exist yet

| Id | Service | Gap | Spec | Blocked on | Then |
|---|---|---|---|---|---|
| S1-C1 | identity-context-svc | Entitlement context reference never resolved (records `UPSTREAM_NOT_CONFIGURED`) (❌) | GOV-01 §4 server-resolved context | **COM-03 Entitlement** (Commercial spec §4.3) | One `WithEntitlementResolver` call |
| S3-C1 | configuration-feature-flag-svc | INV-02: plan eligibility needs the commercial entitlement (⚠️) | AA-001 INV-02 | **COM (commercial account service)** + the edge to set `X-Commercial-Plan` | Edge sets it from COM; see S3-D1 |
| S2-C1 | tenant-entity-registry-svc | Item 28: fiscal-calendar reference is format-checked only (⚠️) | ORG-03 §4.3 | **REF-04 Fiscal Calendar** | Resolve the id against REF-04 |
| S2-C2 | tenant-entity-registry-svc | §9.2 gate 6: dependents must persist the pinned `object_id`/`object_version` | ORG §9.2 | **each consuming service** | Check each consumer stores them |
| S2-C3 | tenant-entity-registry-svc | §9.2 gate 7: RPO/RTO must be measured on production | ORG §9.2 | **production database / operations** | Run the restore drill in production |
| S4-C1 (O-1) | secret-vault-integration-svc | Workload identity not bound to environment/region/audience; inbound cert checks built but off (⚠️) | Security §9 | **mtls-management-svc**: URI SAN `spiffe://zoiko/<env>/<service>` from the issuer's config | Turn on `MTLS_REQUIRED_URI_PREFIX` + `MTLS_MAX_CLIENT_CERT_LIFETIME=24h`, mandatory in production |
| S4-C2 (O-2) | secret-vault-integration-svc | The mTLS private key is generated by the issuer and sent over the wire (⚠️) | Security §9 | **mtls-management-svc**: CSR signing endpoint | Generate the key in process, send only a CSR |
| S4-C3 (O-3) | secret-vault-integration-svc | Static material only; no dynamic credentials (⚠️) | Security §13 | **HashiCorp Vault** with a secrets engine + delivery decision (S4-D1) | Per-policy `STATIC`/`DYNAMIC` mode wired to the engine's lease API |
| S4-C4 | secret-vault-integration-svc | Production refuses to boot until KMS (Transit or Cloud KMS), server/client certificates and shared-secret exceptions exist | Security §13, SEC-INV-07 | **Infrastructure** | Provision; migrate material with `VAULT_MIGRATE_LEGACY_KEY_FILE` |
| S6-C1 | search-indexer-svc | Semantic search and the §10.1 vector controls are built but answer ESR-019 with no provider (2 ⚠️) | AB-001 §10.1, §11.1 | **An embedding provider** (decision OD-10, S6-D1) | Set `EMBEDDING_PROVIDER_URL`; certify recall |
| S6-C2 | search-indexer-svc | Rebuilds are bounded by Kafka retention | AB-001 INV-21/22 | **Replay from owning domain services** | Domain replay API for records older than retention |
| S7-C1 (G-4) | notification-svc | No external-party contacts or authorized cross-tenant relationships; every cross-tenant recipient refused | Y-001 §5.2, NP-12, INV-16, TC-05 | **MDM** (no owner) | Resolve contacts and relationships from MDM |
| S7-C2 (G-5) | notification-svc | Legal basis and deadlines are caller-supplied | Y-001 §8.1, §8.2 | **PDC** (none) and **WFC** deadline clocks (workflow-svc has WFC-01/02 only) | Take basis from PDC and clocks from WFC |
| S7-C3 (G-6, G-7 rest) | notification-svc | No service calls notification-svc; the legacy free-text path stays open for old callers. INV-01 not met | INV-01 | **Callers in each domain** (needs per-workflow product decisions) | Move callers to `POST /v1/communications`, then close the legacy path |
| S7-C4 | notification-svc | NP-01/TC-01: nothing stops another service calling an email provider directly | NP-01, TC-01 | **Estate control** (network policy or secret brokering via S7-B3) | Only notification-svc can reach provider credentials |
| S9-C1 | access-control-svc | Group subjects: assignments to groups (⚠️, the one partial row) | Authz Std §9 | **A group-membership source** (no service holds groups) | Add group subjects once membership exists |
| S9-C2 | access-control-svc | Event-triggered reviews start only on request; no consumer of manager-change or contractor-end events | Authz Std §24 | **HR / workforce events** (none published) | Consume them and open reviews automatically |

##### D · Waiting on a decision

| Id | Service | Question | Who decides | Affects |
|---|---|---|---|---|
| S3-D1 | configuration-feature-flag-svc / gateway | Should `X-Commercial-Plan` and `X-Org-Unit-Id` be edge-set server context (stripped from clients) or caller assertions? Today any client can assert a plan, so NP-08 is not met end to end | Platform / spec owners | S3-C1, INV-02, INV-08, NP-08 |
| S3-D2 | configuration-feature-flag-svc | OD-03: snapshot and version signing | AA-001 owners | §10.1 "digest/signature" |
| S4-D1 | secret-vault-integration-svc | Dynamic-credential delivery: Vault Agent sidecar, or broker-returned per-lease credentials | Security owners | S4-C3 |
| S5-D1 | gateway-auth-svc | Where do the §8 Internet-edge controls (DDoS, WAF, rate limiting, bot/abuse, TLS, request-size limits) live in production? The GCP runbook names no Cloud Armor (6 ❓) | Platform / operations | 6 unscored ❓ rows |
| S5-D2 | gateway-auth-svc | Should an unreachable CARTA still let requests through (❓)? | Security owners | 1 ❓ row |
| S5-D3 | gateway-auth-svc | Real client IP for CARTA behind the GCP load balancer (trusted-hop design) | Security / platform | CARTA signal quality |
| S6-D1 | search-indexer-svc | OD-10: which embedding provider and model | AB-001 owners | S6-C1 |
| S6-D2 | search-indexer-svc | OD-05: which scopes are R2 (source-hydrated); each needs `SOURCE_SERVICE_URL_<TYPE>` | AB-001 owners | R2 coverage |
| S6-D3 | search-indexer-svc | RAG contract §10.2: mostly assigned to AIG, not ESR (❓) | AIG / AB-001 owners | 1 ❓ row |
| S7-D1 (G-15) | notification-svc | Ratify OD-06/13/15 values taken in code: E4 rule, 30-min UNKNOWN deadline, quotas, bulk threshold, reputation thresholds, marketing quiet window | Y-001 owners | Recorded decisions |
| S7-D2 | notification-svc | NP-33 / OD-05: secure-link audience and authentication model; OD-03/04 SMS and push providers (G-11) | Y-001 owners | NP-33, channels |
| S7-D3 | notification-svc / estate | Which legal-hold source is authoritative: document-vault-svc or retention-registry-svc (both expose `/v1/legal-holds`)? | Data-governance owners | S7-B2 |
| S8-D1 | delegated-authority-svc / authorization-svc | Is a 5 s cross-replica revocation window (`AUTHZ_CACHE_TTL_SECONDS`) acceptable? Where does a principal's quantitative (non-monetary) authority limit live? | authorization-svc owners | 2 ❓ |
| S9-D1 | access-control-svc | The 668 legacy action names do not follow §5 `resource.action`; renaming is each owning service's change | Each service owner | §5 taxonomy |

##### Re-check order once dependencies land

1. **Now, no dependency:** group A, starting with S1-1, S3-1/S3-2, S7-1, S1-2, S1-3, S6-1.
2. **authorization-svc migrated and seeded** → S6-B1, then S1-B2, S7-B4, S9-B, S3-B1. Re-run
   search-indexer `audit.sh` sections 7–12 and 16 and the notification live check against
   real grants.
3. **gateway-auth-svc edge changes** (S1-B1, and S3-D1 once decided) → re-score
   identity-context source channel and configuration INV-02 / NP-08.
4. **mTLS listeners + mtls-management-svc URI SAN and CSR** → S5-B1, S4-C1, S4-C2, S3-B2,
   S4-B1.
5. **PRV and DRC wired** (S7-B1, S7-B2) → notification's two partial API rows to ✅.
6. **COM-03, REF-04, MDM, PDC, an embedding provider** as they are built → S1-C1, S3-C1,
   S2-C1, S7-C1, S7-C2, S6-C1.

After each step, re-score only the rows that step touched. Then update the "Scores" table in
"Group 1 re-audit, 7 October 2026" and the summary table at the top of this file.

---

### D4 · Changes to the register since 7 Oct

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2519–2532 (“Not fixable in gateway-auth-svc (what each one needs)”)

#### Not fixable in gateway-auth-svc (what each one needs)

| ID | Issue | Needs |
|---|---|---|
| R-1 (S1-B1 remainder) | A **human** login never gets a verified channel. `/v1/authenticate` issues IdP tokens with no `azp`, so identity-context-svc's `IDP_CLIENT_CHANNELS` mapping has nothing to match. Service accounts and API clients get a server-derived channel; humans stay `asserted`. | identity-context-svc: an authenticated client at login (an OIDC client or external IdP that sets `azp`), then `IDP_CLIENT_CHANNELS="<console-client>=web,…"`. |
| R-2 (A-03 remainder) | The gateway can state assurance but not **how recently** the user authenticated: there is no `auth_time` claim, and `mfa_verified` is always false (no second factor exists). authorization-svc's step-up therefore stays at `AUTHZ_STEP_UP_UNKNOWN_AGE=permit` in compose. | identity-context-svc: an `auth_time` claim carried from the login, then the gateway forwards `X-Authn-Age-Seconds` and compose switches to `step_up`. An MFA factor for `MFA`. |
| R-3 (A-01) | Backends still trust `X-Principal-Id` / `X-Tenant-Id`. The edge makes those authoritative for traffic through Traefik, but a call that reaches a service directly (console server calls, S5-B2) is not covered. The signed envelope already travels in `Authorization` to every backend. | Each consuming service (first authorization-svc) verifies the bearer against identity-context-svc's JWKS instead of trusting the headers. Service-to-service admin calls (e.g. access-control-svc → authorization-svc) must forward the user's bearer first. Cross-service change with a rollout mode. |
| S5-B2 | Console → service calls bypass ForwardAuth. | Frontend: route through the edge, or R-3. |
| S4-B1 | A client certificate for the edge → service hop. | Traefik `serversTransport` certificates from mtls-management-svc (infrastructure); gateway-auth-svc is not in the data path. |
| S3-D1 | Should `X-Commercial-Plan` / `X-Org-Unit-Id` be edge-set or caller assertions? | Spec decision; then the edge sets them from COM. |
| S5-D1–D3 | WAF/DDoS/rate limits; CARTA fail-open; real client IP behind the GCP LB. | Platform / security decisions. |
| M-01 (via mtls-management-svc) | Any bootstrap-token holder can mint a certificate for any CN, which weakens the `MTLS_ALLOWED_CLIENTS` allow-lists. | mtls-management-svc: per-service enrolment. |

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2609–2618 (“Still open after this pass (what each needs)”)

#### Still open after this pass (what each needs)

| ID | Issue | Needs |
|---|---|---|
| R-1 console | The console does not send `client_id` / `client_secret` yet, so compose stays `IDP_CLIENT_AUTH=optional`. | Frontend (console server tier); then set `required`. |
| R-3 estate | Roughly 100 other services still take `X-Principal-Id` on trust when called directly. | Each consumer verifies the envelope (authorization-svc's `internal/identity` is the pattern). |
| MFA UI | Nothing in the console calls the MFA routes. | Frontend: enrolment QR, step-up prompt on `401 step_up_required`, admin reset page. |
| Assurance | TOTP is not phishing-resistant. | Security approval of TOTP as the interim factor (ZS-IAM-001 §31 "session assurance taxonomy"); WebAuthn later. |
| S5-B2, S4-B1, S3-D1, S5-D1–D3 | Unchanged from the 13 Oct table above. | As listed there. |

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2840–2870 (“Also noted (not scored)”)

#### Also noted (not scored)

- **Session concurrency cap** (`cbdb4aa1`, migration 000015). It works as described. Revoking
  the oldest session records the reason but emits no `identity.session.revoked` and no SIEM
  event, and the default cap of 5 applies to service-account principals too.
- **notification-svc: three handler tests skip silently** unless the store suite has already
  built the schema in the same database, and `go test ./...` runs `internal/handler` first:
  - `TestNCDHTTP_ProviderEventsNeedSignature`;
  - `TestNCDHTTP_UnsubscribeHandlingCannotBeDisabled`;
  - `TestSend_OverAQuotaIsRefusedWith429AndRetryAfter`.

  Re-run after the store suite, all three pass. In CI, where packages run in parallel against
  one database, they can skip without anyone noticing.
- **gofmt.** 44 committed Go files are not gofmt-formatted: identity-context 17,
  secret-vault-integration 10, gateway-auth 4, access-control 4, tenant-entity-registry 3,
  search-indexer 2, notification 2, delegated-authority 2. This was counted from the committed
  content, so Windows line endings do not inflate it. CI has no gofmt gate.
- **DoD artefacts are stale.** identity-context-svc's `RELEASE_CERTIFICATE.md` and
  `scripts/audit.sh` date from 18 Sep. The 16 Oct testing record says `audit.sh` has stale
  expectations: its support call names the caller as the approver.
- **S1-C1 (entitlement) is no longer blocked on a missing service.** commercial-account-svc
  serves COM-03 (`GET /v1/commercial/entitlements/effective`), and identity-context-svc has a
  matching client (8 Oct) that is used only when `COMMERCIAL_ACCOUNT_URL` is set. Compose does
  not set it. The row stays ❌ until it is wired and verified, but it moves from register
  group C to group B.
- **The shared `zoiko.io/identity-envelope` module is used only by identity-context-svc.** The
  other eight services still accept `X-Principal-Id` / `X-Tenant-Id` on a direct call
  (register R-3, estate).
- **S1-B2 is unchanged.** The `IDENTITY_*` actions are registered in access-control-svc's
  registry (000020), but only the local seed script grants them.

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2883–2905 (“Register changes, 19 October”)

#### Register changes, 19 October

**New gaps, all in group A (fixable now inside the service):**

| Id | Service | Gap | Fix | Re-check |
|---|---|---|---|---|
| S8-1 (F-1) | delegated-authority-svc | Create fails on every call; 000012 loosened the replay key | Give the VALUES list 25 placeholders. Take `idempotency_key` back out of the unique key (a new migration), and leave key-based replay to the HTTP replay store | Store suite 24/24. Same correlation_id with a new key → replay, not 409. A fresh correlation_id with the same key → replay from the middleware |
| S2-1 (F-2) | tenant-entity-registry-svc | Entity commands, sensitive reads and their approvals are authorized at the caller's envelope entity | Authorize at the target entity, read from the row before the check. Keep the platform scope for platform decisions | Grant at E1 only: update E2 → 403, update E1 → 200, sensitive read of E2 → 403 |
| S1-5 (F-3) | identity-context-svc | A status change emits no event and ends no session | In the status transaction, enqueue `principal.status.changed` (what authorization-svc consumes) as well as `identity.subject.*`, and run `EvictAllForPrincipal` on DISABLED / SUSPENDED. Fix `asyncapi.yaml` | Disable → sessions ended, authorization-svc's projection reads DISABLED, a decision for the principal is denied |
| S1-6 (F-4) | identity-context-svc | The image cannot be built | Build context `services/` (or the repo root) with both modules copied, as search-indexer-svc does | `docker build` from a clean checkout succeeds |
| S1-7 (F-5) | identity-context-svc | The auth plane is compiled but not constructed | Build the OIDC provider, lifecycle service, login throttle, DPoP verifier and keyring from `cfg.AuthPlane` in `main.go`; add tests for `internal/oidc` and `internal/lifecycle`; then re-score AU-01 … AU-11 | Discovery answers 200; a code + PKCE login works end to end; a password spray gets 429 |
| S6-3 (F-6) | search-indexer-svc | Caller identity reaches the embedding provider | Restore trace-only headers and pin them with a test | The provider sees no `X-Principal-Id` or `X-Tenant-Id` |
| S1-8 (F-7) | identity-context-svc | Verifier refusals untested | Restore the negative tests | Both mutants killed |
| S0-1 (F-8) | repository | Six binaries committed | Remove them and ignore `services/*/*.exe` | `git ls-files '*.exe'` lists nothing under Group 1 |

**Closed since the 7 Oct register:**
- S5-B1 / SEC-INV-05 (13 Oct): gateway-auth-svc row → ✅.
- The eight remaining group A rows (19 Oct, uncommitted): tests pass in this run.
- S1-B1 backend (14 Oct). The console part stays open, so the source-channel row stays ⚠️
  until the console sends its client credentials and `IDP_CLIENT_AUTH` is `required`.

**Moved:** S1-C1 from group C to group B (see "Also noted").

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 2982–3000 (“Register changes”)

#### Register changes

Closed: **S8-1** (F-1), **S2-1** (F-2), **S1-5** (F-3), **S1-6** (F-4), **S1-7** (F-5),
**S6-3** (F-6), **S1-8** (F-7), **S0-1** (F-8). The rows closed earlier on 19 Oct (Group A)
remain closed. **S1-C1** stays open (commercial-account-svc is wired only when
`COMMERCIAL_ACCOUNT_URL` is set), and **S1-B1**'s console half stays open.

#### Residual notes

- CI has not run on `d7eaef57`; the last green Group 1 jobs were on `02a125c9`. F-1 would have
  caught a future placeholder drift, so a CI run is still the release gate.
- Group 2 and other groups still track binaries (`evidence-requirements-svc/handler.test.exe`,
  `governance-decision-log-svc/server.exe`, `privacy-rights-svc/{healthcheck,server}.exe`,
  `tools/schemacheck/schemacheck.exe`); the F-8 fix was scoped to Group 1.
- F-5 makes the AU-01 … AU-11 routes real, but those rows belong to the Auth-Service audit and
  were not re-scored here.
- No live compose acceptance run was repeated after these fixes. Everything above is code,
  unit/integration-suite and image-build evidence.

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 3001–3034 (“Cross-domain completion pass affecting Group 1, 21 October 2026 (uncommitted)”)

#### Cross-domain completion pass affecting Group 1, 21 October 2026 (uncommitted)

Group 2's workflow-svc completion pass changes how two Group 1 services are reached. No
Group 1 score changes; this note records the new call paths and the deployment fixes they
required.

- **tenant-entity-registry-svc** is now called synchronously by workflow-svc
  (`GET /v1/entities/{id}/status`) before a workflow instance is created, and by
  obligations-svc and evidence-requirements-svc on their write paths. The read uses the
  caller tenant and no additional authorization action; the response fields were checked
  against the client struct. Compose uses the `tenant-entity-registry-svc` container name;
  K8s uses `tenant-svc.zoiko-identity.svc.cluster.local:8081`.
- **identity-context-svc** serves the `identity-envelope` gate now mounted by workflow-svc.
  A presented signed envelope is verified against JWKS, its session is introspected, and
  its claims override `X-Principal-Id` / `X-Tenant-Id` before the workflow envelope parser
  and tenant middleware read them. This also makes `idenv.Forward` meaningful for outbound
  authorization, tenant-registry and ledger calls. Compose sets `IDENTITY_VERIFY=warn`
  while the console does not forward the envelope on every workflow call; K8s points at
  `identity-svc.zoiko-identity.svc.cluster.local:8080`.
- **K8s deployment gaps corrected.** The workflow, obligations and evidence-requirements
  manifests were missing in-cluster DNS for `tenant-svc`/`identity-svc`; obligations and
  evidence also lacked the identity URLs, and evidence-requirements-svc had no
  NetworkPolicy at all under a namespace default-deny (so it could not reach Postgres or
  Kafka). The policies now allow the cross-namespace G1 calls and workflow's ledger/
  document-vault egress. policy-svc and authorization-svc likewise now name the in-cluster
  identity-svc JWKS URL instead of the compose-only short name, with matching egress rules.
  Modified YAML parses; the manifests were not applied to a live cluster in this pass.
- **Verified on this tree.** Main code is unchanged, but every Group 1 suite was rerun after
  the wiring changes against real PostgreSQL 16: **2,314 pass / 2 skip / 0 fail** across the
  nine services (identity 515, tenant-registry 269, configuration 175, secret-vault 154,
  gateway-auth 64, search-indexer 258, notification 634, delegated-authority 109,
  access-control 136). Docker Desktop is not installed, so image builds and compose/live
  acceptance remain unrun; CI remains the release gate.

### D5 · identity-context-svc: gap register and findings (4 Nov)

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 3352–3431 (“identity-context-svc gap register, 4 November 2026: every open gap”)

#### identity-context-svc gap register, 4 November 2026: every open gap

This register replaces the identity-context-svc (S1) rows in "07/10 re-audit gaps" and in the
19 Oct register changes. It lists every gap still open for the service, whatever owns the fix.
Groups as before: **A** fixable now inside the service; **B** needs a change in a service or
client that exists; **C** needs something that does not exist yet; **D** waits on a decision.
A last table lists proof still owed where the code is done.

**Summary**

| Group | What it waits on | Gaps |
|---|---|---|
| A | Nothing: fixable now inside the service | 6 |
| B | A service or client that exists | 5 |
| C | Something that does not exist yet | 0 |
| D | A decision | 3 |
| | **Total** | **14** (plus 5 proof items) |

##### A · Fixable now, inside identity-context-svc

| Id | Gap | Spec | Fix | Re-check |
|---|---|---|---|---|
| S1-9 (K-1) | A revocation, expiry or suspension older than the horizon is skipped whole, so the role projection keeps framing it. A projection change given up after its retries is lost for good | GOV-01 §4 NP4; invariant 10 | Apply the horizon to the eviction only. Always write the projection (`EndRoleAssignment`, `SuspendRoleAssignment`), then skip the eviction when the event is too old. Add a projection resync from authorization-svc's assignments, and a dead-letter record for an event given up on | Revoke and suspend 10 minutes old → projection closed / held, no eviction. Replay from the earliest offset → no revoked assignment is framed. Rescore NP4 |
| S1-10 (K-2) | A suspension consumed before its grant leaves no trace; the late grant frames the suspended role | GOV-01 §4 NP4 | Record the suspension without a row (keyed by tenant + assignment, like the tombstone), and have `UpsertRoleAssignment` apply it on insert | Suspend, then grant → not framed. Suspend, grant, resume → framed |
| S1-11 (K-3) | Five emitted events are not in `asyncapi.yaml`; the contract test checks a hand-written list that omits them; nine event constants have no emitter | Event Catalogue; GOV-01 §17 | Declare the five, add them to `producedEventTypes`, and delete (or wire) the nine unused constants | Contract test fails when an emitted name is undeclared (mutant: drop one declaration) |
| S1-12 (K-6) | Bulk session evictions emit no `identity.session.revoked`. The events that are emitted carry no tenant and name the subject as the actor, and the single-session one is published after its commit | ZS-IAM-001 §23; invariant 10 | Emit per session through the outbox in the invalidation's transaction, with the tenant and the real actor (or the event that caused it) | A status change ending 2 sessions → 2 `identity.session.revoked` with `tenant_id` set |
| S1-13 (K-7) | Password-reset decisions: the metric counts refusals; an outage answers 409; the expiry write rolls back; intermediate approvals leave no event; an undelivered link strands the request | IAM §3; plan Console rule | Count after commit. Map store outages to 503. Commit the expiry before refusing. Emit an event for every decision. Answer 502/503 (or allow a re-issue) when delivery fails | One test per point |
| S1-14 (PR #249 merge, `abc2c226`) | Two merge routes, two strengths. `POST /v1/identities/{id}/merge` (main's ORG-01 merge: `MergeIdentity` → `lifecycle.MergeDuplicate`) asks for no step-up, and it writes only `identity.subject.merged`. The duplicate is disabled without `principal.status.changed` / `identity.subject.disabled`, so authorization-svc's status projection keeps it ACTIVE. `POST /v1/subjects/{id}/merge` requires a step-up and writes all three. Deferred from the merge because closing it changes main's tested behaviour (`handler_merge_test.go`; `lifecycle/merge_test.go` asserts the merge writes only `identity.subject.merged`) | IAM §3 (identity recovery is sensitive); F-3 (a disable must be emitted) | In a follow-up PR agreed with the author of main's ORG-01 waves: require `steppedUpActor` on `MergeIdentity`; add the two status events to `MergeDuplicate`'s build function; update both tests | `/v1/identities` merge without a recent second factor → 401 `step_up_required`. A merge writes `identity.subject.merged`, `principal.status.changed` and `identity.subject.disabled`. authorization-svc's projection reads the duplicate as DISABLED |

##### B · Needs a change in a service or client that exists

| Id | Gap | Spec | Depends on | What closing it takes |
|---|---|---|---|---|
| S1-B4 (K-4) | The console signs people in from its own credential list when identity-context-svc refuses, fails or is down | GOV-01 §4 (authenticated subject); 28 Sep ruling | **zoiko-suite-frontend-platform** | No fallback outside local development, and never after a 401 or `invalid_client`. Bootstrap the production administrator through identity-context-svc |
| S1-B1 / R-1 console (K-5) | The console sends its login client only when `IDENTITY_CONSOLE_CLIENT_ID` / `_SECRET` are set, and the example env leaves them empty. The source-channel row stays ⚠️ | GOV-01 §4 server-resolved source channel | **zoiko-suite-frontend-platform** | Document the local client in the console's env setup, and refuse to start the console server without it outside development. Then rescore source channel |
| S1-C1 | Entitlement context reference is never resolved (❌). The client and config are done; compose does not set `COMMERCIAL_ACCOUNT_URL` | GOV-01 §4 server-resolved context | **commercial-account-svc** (COM-03) | Run it on the stack, set the URL, and verify a resolve records the entitlement reference |
| R-3 estate | Services still take `X-Principal-Id` / `X-Tenant-Id` on trust when called directly. 20 of 122 service folders (identity-context-svc included) reference `zoiko.io/identity-envelope`, and a reference is not proof of a mounted gate | Security §8; A-01 | **each consuming service** | Mount the envelope gate (authorization-svc's `internal/identity`, workflow-svc's gate) service by service |
| S1-B5 | Live SCIM provisioning is off locally, and the service principal holds no operator grant | AU-08; INT-10 | **access-control-svc** grant + deployment config | `FEDERATION_PROVISIONING_ENABLED=true` and an operator grant for `IDENTITY_SERVICE_PRINCIPAL_ID`, then a live SCIM create / disable |

##### D · Waiting on a decision

| Id | Question | Who decides | Affects |
|---|---|---|---|
| S1-D1 (D-4) | SAML federation: build it, or record OIDC only (SPEC_DEVIATIONS) | Security / platform owners | INT-10 SAML |
| S1-D2 (SO-1) | Approve TOTP as the interim high-assurance factor, now that passkeys exist, and approve passkey attestation `none` with no authenticator allow-list (SPEC_DEVIATIONS) | Security | ZS-IAM-001 §13, §31; AU-11 |
| S1-D3 | Is a passkey-first (usernameless) login required, or are passkeys a second factor only? | Security / product | AU-11 scope |

##### Proof still owed (code done)

| Item | What is missing |
|---|---|
| Assignment suspension, live | The merge's suspend and resume handling has not run against Kafka. Re-run `e2e-identity-context-deps.py` on an image built after `15a9c581`, with a suspend and a resume step |
| AU-05 key rotation, live | A rotation exercised with two published keys (JWKS publishes one locally) |
| Passkey in a real browser | A ceremony with a platform authenticator (the live proof used a software P-256 authenticator) |
| S1-C1, live | As in group B, once commercial-account-svc runs |
| Mutants | Not re-run since 3 Nov (3/3 killed then) |

##### Closed since the 7 Oct register

| Id | Closed by |
|---|---|
| S1-1 … S1-4 | 8 Oct gap closure |
| S1-5 … S1-8 | 19 Oct fixes |
| S1-B2 (no service grants the `IDENTITY_*` actions) | access-control 000026 role templates (IC-7) and 000027 (`FEDERATION_*`, N-4) |
| S1-B3 (ingress bindings never refreshed) | IC-6 refresher (`cmd/server/main.go:738-745`) |
| R-2 (`auth_time`, MFA) | 14 Oct auth plane closure; passkeys 3 Nov (N-1) |
| MFA UI | 3 Nov (N-9) |

##### Where to start

1. **S1-9 (K-1).** It is the only finding that rescored a row, and the fix is local. Then
   **S1-10 (K-2)**: the same projection, the same tombstone pattern.
2. **S1-B4 (K-4) and S1-B1 (K-5)**, together in the console: remove the fallback and configure
   the client.
3. **S1-11 (K-3)** and **S1-12 (K-6)**: contract and evidence.
4. **S1-13 (K-7).**
5. Run the live E2E on a post-merge image, then rescore NP4 and source channel.

---

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 3189–3341 (“Findings, ranked by risk”)

#### Findings, ranked by risk

##### K-1 · identity-context-svc: a late revocation or suspension is skipped whole, so the projection keeps framing the role (auth)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| NP4 · cache invalidation removes stale privilege promptly | GOV-01 §4 NP4; invariant 10 (at-least-once) | `Consumer.Handle` drops `iam.assignment.revoked`, `.expired` and `.suspended` before dispatch when they are older than the revocation horizon: the envelope TTL + 1 min, so 6 min by default (`internal/events/consumer.go:371-377`). `iam.assignment.granted` is not gated | ⚠️ (was ✅) | **Confirmed in a throwaway consumer test.** At the default horizon, three 10-minute-old events for one assignment were handled. The grant was projected. The revoke and the suspend were skipped: no `EndRoleAssignment`, no tombstone, no `SuspendRoleAssignment`. The horizon was written on 28 Sep (R1) for session eviction only. The projection writes were added later, inside the same handlers: the close and tombstone (S1-4, 8 Oct) and the suspension (merge, 3 Nov). The resolver frames roles from this projection (`internal/context/resolver.go:399`). So every later envelope's `role_profile` names the revoked or suspended role and its permission bundles. This lasts until the assignment's own end date, which is `infinity` for an open-ended grant |

**When it happens.**
- **Consumer lag over 6 minutes.** A pod is down, the broker is down, or one event retries
  through its budget (25 attempts, about 10 minutes) and holds the partition behind it.
- **The topics are replayed** (a consumer-group reset or a new environment). Every historic
  grant is projected and every historic revocation is skipped.
- **An event runs out of retries.** The consumer commits past it, logged at ERROR. A projection
  change lost that way stays lost: there is no resync from authorization-svc and no dead-letter
  record.

**Impact.** authorization-svc decides from its own assignments, so `/v1/authorize` is not
affected. The stale role reaches every relying party that reads `role_profile`, and the console.

##### K-2 · identity-context-svc: a suspension that arrives before its grant is lost (auth, latent)

`SuspendRoleAssignment` (`internal/store/pg_role_suspension.go:18-30`) is an `UPDATE`. When the
projection holds no row, it changes nothing and records nothing. A revocation in the same case
leaves a tombstone (S1-4); a suspension leaves no trace.

**Confirmed on Postgres 16 (throwaway store test).** Suspend `a-9`, then project its grant:
`FindActiveRoleAssignments` frames `a-9`.

authorization-svc keys the grant, suspend and resume events by the assignment id on one topic,
so they normally arrive in order. The gap opens in two cases:
- the first grant was not projected, because the principal did not exist here yet
  (`ErrUnknownPrincipal`);
- the row is created later by a grant from the other producer, access-control-svc, on another
  topic.

The merge's order test (`TestPgStore_SuspensionIsOrderIndependentAndIdempotent`) starts from a
projected row. It proves that suspend and resume commute; it does not cover a suspension that
arrives before the grant.

##### K-3 · identity-context-svc: five emitted events are not in asyncapi.yaml, and the contract test cannot see them (contract)

**Undeclared events.** These go to the outbox and are not declared in `asyncapi.yaml`:
- the lifecycle service writes `identity.subject.created`, `identity.password.changed`,
  `identity.password_reset.requested` and `identity.password_reset.decided`
  (`internal/lifecycle/service.go:167`, `:358`, `:416`, `:453`);
- every session invalidation also writes the legacy `session.invalidated` beside
  `identity.session.revoked` (`internal/events/publisher.go:281`).

**Why the test passes.** `TestEveryProducedEventIsPublishedInTheContract` checks a hand-written
list, `producedEventTypes` (`internal/events/contract_test.go:78`), and that list omits all five.

**Unused constants.** Nine `Event*` constants in `publisher.go` have no emitter. A reader of the
package can take them for live events:
- `identity.subject.invited`;
- `identity.idp.registered`, `identity.idp.status_changed`;
- `identity.federation.login`;
- `identity.group_role_mapping.proposed`, `.decided`;
- `identity.group.membership_changed`;
- `identity.scim_connection.created`, `identity.scim_sync.completed`.

##### K-4 · zoiko-suite-frontend-platform: the console signs people in without identity-context-svc (auth, cross-service)

**The path.** `app/api/auth/login/route.ts:66-126` creates a console-only session from
`lib/auth.ts` `findUserByCredentials` in any of these cases:
- `/v1/authenticate` is unreachable or fails;
- it answers 401 for an account the console's own list knows;
- authentication succeeds but resolve fails.

**Who can sign in that way.** The demo accounts. In production, the `ZOIKO_ADMIN_EMAIL` /
`ZOIKO_ADMIN_PASSWORD` account, or every demo account when `ALLOW_DEMO_CREDENTIALS` is set.

**What it skips.** The session carries no envelope and passed none of identity-context-svc's
controls: lockout, login throttle, client authentication, MFA, the session register and
sign-in events.

**What it contradicts.** The 28 Sep ruling says `/v1/authenticate` is the console's only login
path until an external identity provider is connected.

**Reach (code, not driven).** The session holds the configured account's `principalId` and
`tenantId`. The console's `envelopeHeaders` (`lib/api/envelope.ts:125-133`) sends those as
`X-Principal-Id` / `X-Tenant-Id`, with no `Authorization` header, because the session has no
envelope. The calls are accepted wherever headers are trusted:
- by every service that does not verify the envelope (register R-3);
- by authorization-svc in compose, which runs `AUTHZ_IDENTITY_ASSERTION: "warn"`.

A backend in `require` mode (authorization-svc's default outside `ENV=local`) refuses them.

##### K-5 · console: its login client is not configured by default (auth, R-1 remainder)

Compose requires a login client (`IDP_CLIENT_AUTH: "required"`, `deployments/docker-compose.yml:675`).
The console sends `client_id` / `client_secret` only when `IDENTITY_CONSOLE_CLIENT_ID` and
`IDENTITY_CONSOLE_CLIENT_SECRET` are set. Its `.env.example` leaves both empty, and the checkout
has no local env file. A console started from the example therefore gets 401 `invalid_client`
from identity-context-svc, and then falls back to a console-only session (K-4). The client is
registered on the service side (`deployments/identity/idp-clients.local.json`, `zoiko-console`).
This is why the source-channel row stays ⚠️.

##### K-6 · identity-context-svc: bulk session revocations emit no session event, and the ones that do carry no tenant (evidence)

**Bulk evictions are silent.** `DurableCache.EvictAllForPrincipal`, `EvictAllForEntity` and
`EvictAllForTenant` (`internal/session/durable.go`) record each invalidation in
`session_contexts`, but emit no `identity.session.revoked`. Only two paths emit it: the
single-session route (`internal/context/resolver.go:768`) and the concurrency cap
(`cmd/server/revocation.go`). These ends of a session therefore leave no session event and no
per-session SIEM signal:
- a status change;
- an assignment revocation or suspension;
- a role change or an entity change;
- the end of a delegation.

**The events that are emitted** have three defects:
- `tenant_id` is empty;
- `actor_id` is the subject, not whoever revoked the session (`internal/events/publisher.go:278-281`);
- the single-session route publishes from a detached goroutine after the invalidation commits,
  not in its transaction.

**Exposure is low.** `identity-envelope`'s revocation cache holds a live answer for 5 s, so
relying parties are not left holding an ended session.

##### K-7 · identity-context-svc: password-reset decisions, five smaller defects (evidence / availability)

- **The metric counts refusals.** `identity_password_resets_total{decision}` is incremented
  before the store decides (`internal/lifecycle/service.go:458-460`). A decision that is then
  refused (already decided, expired, not pending) is still counted.
- **A database outage reads as a conflict.** Every store failure, including an outage, comes back
  as `ErrConflict`, so the route answers 409, never 503 (`service.go:470-471`).
- **The expiry write is rolled back.** `DecideResetRequest` sets `EXPIRED` and then returns an
  error in the same transaction (`internal/store/pg_lifecycle.go:397-399`). The row stays
  `PENDING_APPROVAL`; reads show `EXPIRED` only because they compute it (N-7).
- **Intermediate approvals leave no event.** With `PASSWORD_RESET_APPROVALS` of 2 or more, no
  event is written until the last decision (`pg_lifecycle.go:427-429`), and that event names
  only the last approver. Earlier approvers and their reasons never reach the event stream. The
  default is 1.
- **A failed delivery strands the reset.** If delivering an approved link fails, the call still
  answers 200 `ISSUED` and logs a WARN (`service.go:479-481`). The link is stored only as a
  hash, so it cannot be re-sent, and the request waits out its TTL.

##### Also noted (not scored)

- **Duplicate migration numbers.** 000012, 000013 and 000016 each have two files.
  `deployments/migrate.py` and the Kubernetes Postgres init track migrations by filename and apply
  them in filename order, so both work today. A runner keyed on version numbers (golang-migrate)
  would refuse the directory. The merge message records the decision to leave them.
- **A disable whose session end fails still answers 200.** `lifecycle.SetStatus` commits the
  status and its events, then ends the sessions outside the transaction. A failure there is
  logged and not retried (`internal/lifecycle/service.go:523-526`). The exposure is bounded:
  resolve refuses a principal that is not ACTIVE, so live envelopes run out within the envelope
  TTL (300 s).
- **The local compose stack.** On 4 Nov the `identity-context-svc` container was restarting in a
  loop because the Redis container was stopped. That is the Tier 0 fail-fast working as designed,
  not a defect.

### D6 · gateway-auth-svc: AUTH-PLANE status (4 Nov)

> Source: `Identity, Scope & Foundation-audit-2026-09-23.md`, lines 3432–3489 (“gateway-auth-svc status, 4 November 2026: AUTH-PLANE GW-0 … GW-6”)

#### gateway-auth-svc status, 4 November 2026: AUTH-PLANE GW-0 … GW-6

**Why.** Ten commits on 4 Nov (`b2efeee0` … `f9673bc1`) ran the gateway tasks of
`AUTH-PLANE-PLAN-2026-11-02.md`. This section records which of them hold up when the code is read,
and what is still open. Each claim here was checked against the code or a test run on 4 Nov.
Claims taken only from `services/gateway-auth-svc/progress.md` are marked as such.

**Score: no change.** The Security §8 + GOV-01 ingress rows were not re-scored; they stay at 22/22.
The AUTH-PLANE tasks add scope beyond those rows (INT-01 minimum acceptance tests, AU-04, S3-D1,
S5-D3, AU-12/IAM-1).

**Test evidence** (`shashi-changes` at `33ee341d`):

| Check | Result |
|---|---|
| `go build ./...` / `go vet ./...` (host, Go 1.26.7) | clean |
| `go test -count=1 ./...` (host, `GOTMPDIR`) | **9 packages ok, 0 fail**: admin, admission, commercial, config, handler, jwks, mtls, problem, siem. 154 test functions in the module |
| Not run | image rebuild and live container, `scripts/audit.sh`, `e2e-gateway-auth-deps.py`, mutants |

##### Per task

| Task | Commit(s) | 4 Nov status | What was checked |
|---|---|---|---|
| GW-0 baseline, matrix, stale STEP_UP_MFA text | `b2efeee0` | ✅ DONE | Matrix in `progress.md`. The stale text is fixed in openapi, `RELEASE_CERTIFICATE.md`, telemetry and handler comments |
| GW-1 INT-01 minimum acceptance tests + residency gate | `d3cfc988`, `aebdf04e` | ✅ DONE (tests) | All eight `TestINT01_*` exist and pass. The first commit's residency gate lacked its registry reads; `aebdf04e` added them |
| GW-2 trusted hop (S5-D3) + login throttle (AU-04) | `c7e7dac4` | ⚠️ built, not proven live | `TestClientIP_*` (3) and `TestAU04_*` (3) pass. The live probe the matrix cites ("25 failed logins → 429 before the 25th") was not run. Routing identity-context's public auth routes through Traefik is unverified |
| GW-3 edge-set `X-Commercial-Plan` / `X-Org-Unit-Id` (S3-D1) | `06afd752` | ⚠️ partial | `internal/commercial` client with six `TestClient_*` tests. The matrix names `TestS3D1_*`; **no such test exists**. Replacing a forged plan header at the edge has no named test. Live half BLOCKED: commercial-account-svc does not run locally |
| GW-4 admin read surface | `fb2a30c0` | ⚠️ partial | Compose sets `GATEWAY_ADMIN_ADDR=:8192` and a secret token file. The three new reads the plan asked for (decisions of every outcome, usage for all tenants, policy) were **not built**. `admin.go` still serves decision by id, denied, usage per tenant, rate limit and versions. Since the console decision below, the surface is a RUNBOOK tool. Not exercised live |
| GW-5 per-service audience (AU-12 / IAM-1) | `370381e8` | ❌ not built as specified | See finding G-1 |
| GW-6 contract, docs, live, E2E | `9bc132f4` (script only) | ❌ open | See findings G-2 and G-3 |
| GW-FE-1 console admin client | removed | ✅ decision | Removed. Decisions, usage and limiter state are per replica (deviation note in the AUTH-PLANE plan) |
| GW-FE-2 `/admin/gateway` v2.0 | frontend; `f9673bc1` docs | ✅ DONE, per `progress.md` | Diagnostic page. The render check (down path and mocked up path) and eslint are recorded in `progress.md`; not re-run here |

##### Findings

| # | Finding | Evidence | What closes it |
|---|---|---|---|
| G-1 | **GW-5 exchanges for the gateway's own calls, not for the target service.** The plan's GW-5: `/verify` exchanges the session envelope for one whose audience is the route's service (`GATEWAY_AUDIENCE_ROUTES`), with an unknown route failing closed. The commit instead wires `idenv.Exchanger` into the gateway's outbound carta-svc and commercial-account-svc calls (`internal/carta/client.go:153`, `internal/commercial/client.go:91,125`). `/verify` never uses it (`handler.go:115,134` only stores it). There is no route → audience map. The four tests the matrix names (`TestVerify_ExchangesForRouteAudience`, `TestVerify_UnknownRouteFailsClosed`, `TestVerify_ExchangeFailureIs503NotPassThrough`, `TestIAM01_EnvelopeForPolicyRefusedByWorkflow`) **exist nowhere** in `services/`. The commit touched no test file. Also, compose sends client `gateway-auth-svc` with secret `changeme-in-production`, a client registered nowhere. The OIDC client registry (`deployments/identity/oidc-clients.local.json`, mounted as `OIDC_CLIENTS_FILE`) holds only `local-dev-cli`, `idp-clients.local.json` holds only `zoiko-console`, and compose sets no `OIDC_EXCHANGE_AUDIENCES`. Every live exchange would therefore fail, and the code then forwards the original envelope silently | `git show --stat 370381e8`; `grep -rn "func TestVerify_ExchangesForRouteAudience"` finds nothing | Build GW-5 as planned, with its four tests. Register the gateway client in identity-context, or read the secret from a compose secret. Count exchange failures in a metric, not a silent pass-through. IAM-1 stays open until then |
| G-2 | **`e2e-gateway-auth-deps.py` cannot produce a valid run.** (a) `call("GET", url, envelope(jwt), headers={...})` passes `headers` twice, so the script raises `TypeError` at check 5 (and again at checks 12 and 13). (b) `login()` asks `/oauth2/token` for `client_credentials` as `zoiko-console`, which is not an OAuth client with that grant (`oidc-clients.local.json` lists only `local-dev-cli`). The checks that `X-Principal-Id` equals the demo admin could not hold for a client token anyway. (c) `COMMERCIAL` points at `:8080`, which is identity-context. (d) Three checks pass without testing anything: `check(..., True, "skipped")` for the admin surface and audience exchange, and `True` when the forged-header request is not 200. (e) It exits 1, not the failure count its docstring promises. (f) There is no revocation (AU-01), residency, CARTA step-up or Traefik header-replacement check, and no write is undone | Read of the script at `9bc132f4` | Rewrite on the pattern of `e2e-authorization-deps.py`: password login with the confidential client → context resolve → envelope; `skip()` never counted as a pass; one check per edge listed in GW-6; run it live and keep the output |
| G-3 | **GW-6 deliverables are missing or uncertified.** `progress.md` has no GW-6 section. `SPEC_DEVIATIONS.md` does not exist, yet the matrix cites it for SEC8-E1 and AU-27. `RELEASE_CERTIFICATE.md` was edited only to fix the STEP_UP_MFA text, not re-certified. The image was not rebuilt and `scripts/audit.sh` was not run against this tree | `ls services/gateway-auth-svc` | Do GW-6: SPEC_DEVIATIONS (S5-D1 placement, AU-27, S5-B2), re-certification, image rebuild, live `audit.sh`, live E2E |
| G-4 | **The feature matrix claims evidence that does not exist.** These rows are marked DONE but name tests that do not exist (G-1, GW-3) or a live probe that was not run (AU-04): INT01-07 (GW-4 reads), S3-D1, AU-12, GW-5 and S5-B2 | as above | Correct the rows to their real status when G-1 … G-3 are worked |

##### Register changes

| Id | 4 Nov |
|---|---|
| S5-D3 (real client IP behind the GCP LB) | **Built.** Trusted-hop design with `GATEWAY_TRUSTED_PROXY_CIDRS` / `_DEPTH` (defaults RFC 1918, depth 1). The decision on production CIDRs stays with platform |
| AU-04 edge login throttling | **Built, not proven live** (G-4) |
| S3-D1 edge-set plan / org unit | **Partial.** Client built; edge tests and the live half are open (GW-3 row) |
| AU-12 / IAM-1 per-service audience | **Open** (G-1) |
| S5-B2, S4-B1, S5-D1, AU-27 | Unchanged |

##### Where to start

1. G-2: make the E2E honest, then run it. It will show which of the rest is real.
2. G-1: GW-5 as planned. IAM-1 depends on it.
3. G-3: GW-6 deliverables and the live run.
4. G-4: correct the matrix, then hand back for the AUTH-PLANE verifier protocol.

---

## Part E · Domain 2 audit: Governance Platform (current state)

### E1 · Method, scoring and legend

> Source: `Governance Platform-audit-2026-09-28.md`, lines 1–47 (“Group 2 — Governance Platform: documentation-compliance audit”)

#### Group 2 — Governance Platform: documentation-compliance audit

**Date:** 28 September 2026
**Scope:** all seven services of Group 2, "Governance Platform — the non-bypassable spine", each
audited on its own: jurisdiction-rules-svc, governance-decision-log-svc, policy-svc,
obligations-svc, authorization-svc, workflow-svc, evidence-requirements-svc.
**Source of truth:** the original specifications in `docs/architecture/`: `03-microservices.md`
§08 (§8.1–§8.7, one section per service), `04-data-model.md`, and the `.docx` standards listed
below. The per-service `openapi.yaml`, `context.md` and `progress.md` files were treated as
**claims to verify**, not as the contract. The one exception is jurisdiction-rules-svc's
`openapi.yaml`, which was also diffed as a published contract. The reason is the same as in the
Group 1 audit: checking code against a contract written alongside it cannot catch a case where
both encode the same wrong assumption.

**Specifications used across the group:**

| Document | ID | Used for |
|---|---|---|
| `03-microservices.md` §08 Governance Platform Service Specifications | Doc 03 | Primary per-service spec (purpose, owns, inbound APIs, events, critical constraints) |
| `04-data-model.md` §7.1–7.3, §13 | Doc 04 | Entity attributes, effective-dating, compliance model |
| `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx` | ZS-SVC-A-001 | §2 invariants, GOV-03 / 04 / 05 / 06 / 07 / 12, §16 envelope and error classes, §17 events, §18 data model |
| `ZS-SVC-V-001_Policy_Rules_Decision_Jurisdictional_Applicability_Control_…docx` | V-001 | PDC-01…05 (policy registry, jurisdiction applicability, evaluation, evidence, simulation) |
| `ZS-SVC-R-001_Workflow_Approval_Case_Obligation_Control_…docx` | R-001 | WFC-01…05 (definitions, orchestration, approval/SoD, case, obligation/deadline) |
| `ZoikoSuite_Global_Authorization_RBAC_ABAC_Segregation_of_Duties_Standard_…docx` | ZS-IAM-001 | Authorization pipeline, SoD, delegation, caching, evidence |
| `ZoikoSuite_Global_Jurisdiction_Pack_Regulatory_Rule_Architecture_Standard_…docx` | ZS-JUR-001 | Jurisdiction packs, rule immutability, precedence |
| `ZoikoSuite_Global_Data_Governance_Evidence_Lineage_Retention_Legal_Hold_Standard_…docx` | ZS-DATA-GOV-001 | Immutability, retention, legal hold |
| `ZoikoSuite_Enterprise_Domain_Event_Catalogue_Event_Contract_Standard_…docx` | Event Catalogue | Event naming, envelope, transactional outbox |
| `ZoikoSuite_Global_API_Command_Integration_Contract_Standard_…docx` | API standard | Status codes, idempotency, RFC 9457 errors, pagination, If-Match |

**Method (per service):**
1. Extract the documented contract: endpoints, schemas, status codes, auth, business rules,
   side effects and events.
2. Extract the implemented contract from the code: router, handlers, store, migrations,
   publishers and consumers, envelope policy, compose and gateway wiring.
3. Diff the two, then report.

The audit is **static and read-only**: no containers were started, no code was changed, and
no tests were run. The authorization-svc store tests would have wiped their target database.
The two authorization-svc privilege-escalation paths were checked twice in the code, once by
the service auditor and once again independently. Anything ambiguous or contradictory in the
documents is flagged **needs clarification (❓)** and left out of the score, not counted as a
pass or a fail.

**Scoring:** *Full match* = ✅ ÷ scored items. *Weighted* = (✅ + ½ ⚠️) ÷ scored items. Items
marked ❓ are not scored.

**Legend:** ✅ match · ⚠️ partial · ❌ missing · ❓ needs clarification

### E2 · Current scores and row movements (19 Oct re-audit)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3490–3596 (“Group 2 — re-audit, 19 October 2026”)

#### Group 2 — re-audit, 19 October 2026

**Basis:** code at `d7eaef57` plus the uncommitted obligations-svc outbox fix (10 files; see
*Obligations outbox regression* below). All eight services — the seven Group 2 services and
schema-registry-svc — were built, vetted and fully tested against real Postgres 16 on 19 October.
Every scored row was re-checked against the current code, the 12–19 October closure records and
the per-service live acceptance runs (dates cited below). No row was carried forward without
evidence. The per-service tables above are left intact as history; this section is the current
state.

**Result: 343 ✅ / 76 ⚠️ / 17 ❌ over 436 scored rows = 78.7% full, 87.4% weighted**
(28 September: 165 / 144 / 109 of 418 = 39.5% / 56.7%). 17 rows remain ❓ (unchanged; the 16
WFC-05 rows stay unscored pending the C1 scope decision).

| Service | ✅ | ⚠️ | ❌ | ❓ | Scored | Full | Weighted | Last live |
|---|---|---|---|---|---|---|---|---|
| jurisdiction-rules-svc | 73 | 4 | 2 | 3 | 79 | 92.4% | 94.9% | 17 Oct GREEN |
| governance-decision-log-svc | 41 | 6 | 0 | 5 | 47 | 87.2% | 93.6% | 17 Oct GREEN |
| policy-svc | 54 | 2 | 0 | 1 | 56 | 96.4% | 98.2% | 17 Oct E2E green |
| obligations-svc | 41 | 22 | 2 | 1 (+16 WFC-05) | 65 | 63.1% | 80.0% | 17 Oct E2E green |
| authorization-svc | 85 | 0 | 0 | 0 | 85 | 100% | 100% | 17 Oct 46/46 |
| workflow-svc | 29 | 24 | 8 | 2 | 61 | 47.5% | 67.2% | 19 Oct 35/35 |
| evidence-requirements-svc | 20 | 18 | 5 | 5 | 43 | 46.5% | 67.4% | 19 Oct 42/42 |
| **Group 2 total** | **343** | **76** | **17** | **17** | **436** | **78.7%** | **87.4%** | |
| schema-registry-svc (supporting, unscored) | — | — | — | — | — | — | — | 19 Oct 20/20 |

##### Row movements since the 28 September audit

**jurisdiction-rules-svc — 73 ✅ / 4 ⚠️ / 2 ❌ (unchanged since the 12 October re-score).**
39 → 73 ✅, 23 → 4 ⚠️, 17 → 2 ❌ across Records #311–#313 (precedence, rule versions and
immutability, ETag/If-Match, bitemporal replay, idempotency keys, deactivation idempotence,
append-only triggers, calendar, consumer). Remaining ❌: `POST /v1/validate-action` (decision on
the action_type → rule mapping) and the external regulatory feed (external-data-feed-svc, on
hold). Remaining ⚠️: X5 identity headers, applicability mappings (C8/backfill), drift detection,
resolver evidence (`RESOLVER_ENABLED` decision).

**governance-decision-log-svc — 41 ✅ / 6 ⚠️ / 0 ❌ (unchanged since 12 October).**
16 → 41 ✅, 23 → 6 ⚠️, 8 → 0 ❌ (repaired 000007, tenant-scoped manifests, outbox + relay with
acknowledged envelopes and decision_id keys, idempotency on stored rows, replay idempotency and
events, column-width guards, RFC 9457 handler errors, broker guard, size bound). The 6 ⚠️ all
wait on other services or ensure-compliance choices: A10 (policy-svc global writes), A7/A11
(read-authz still `warn`, callers' envelopes), A1 (raw tenant header, X5), I3 (shared envelope
refusals not RFC 9457, X9), C3 event naming. Kafka relay needs a broker.

**policy-svc — 54 ✅ / 2 ⚠️ / 0 ❌ (closed 14 October).**
15 → 54 ✅, 15 → 2 ⚠️, 25 → 0 ❌ (GOV-05 lifecycle, maker-checker, optimistic concurrency,
scheduled-activation guard, evidence relay into the real ledger, 8 event types, SoD). Remaining
⚠️: no `openapi.yaml` artifact, Kafka relay without a broker.

**authorization-svc — 85 ✅ / 0 ⚠️ / 0 ❌ (closed 14 October).**
29 → 85 ✅ over the 68 original rows plus 17 session-control rows. The 12 Oct mTLS workload gate
and the 14 Oct auth-plane closure (identity envelope via JWKS, step-up before JIT/break-glass,
session maximums, admin authorization, admin delegation, `require` mode) leave nothing failed.
Two organisational sign-offs remain (SO-1 TOTP, SO-2 session maximums); deployment notes are in
*authorization-svc — auth plane closure (14 October 2026)*.

**obligations-svc — 29 → 41 ✅, 27 → 22 ⚠️, 9 → 2 ❌ (17–19 October).**
The 17 October pass (live E2E green) plus the 19 October outbox fix move thirteen row verdicts
(twelve to ✅, one to ⚠️):
workflow mappings ❌→✅ (000007, link/unlink, live 200/409); filing-requirement create ❌→✅
(000005 dedup, live same-id replay); `Idempotency-Key` semantics ❌→✅ (000009; live replay 200
same id, mismatch 409 IDEMPOTENCY_KEY_CONFLICT); principal on applicability reads ❌→✅ (live
401); `X-Legal-Entity-Id` reconciled ❌→✅ (live 400 `legal_entity_mismatch`);
transactional outbox ❌→✅ (see below); idempotent obligation create ⚠️→✅; overdue detection
⚠️→✅ (in-process scheduler, live OVERDUE transition); applicability isolation ⚠️→✅ (000006
tenant_id + FORCE RLS + `withTenantTx`); read authorization ⚠️→✅ (object-level
`ActionObligationRead`); outbound authz envelope ⚠️→✅ (full envelope + `ForwardTo`);
If-Match ⚠️→✅ (000008; live 412 stale / 200 correct). Legal entity exists ❌→⚠️ (header/body
reconciled and authz-checked; still no registry existence check).
Remaining ❌: single owner of obligation truth (three services own "obligations" and two emit
the same event names — C8), idempotent applicability decision (duplicates still appended).
Remaining ⚠️ (22): definition/instance split, entity mappings, `obligation.updated` scope,
event/channel naming, partition key, alertable outbox states, caller-supplied id collision,
raw identity headers (X5), dead resolver, invalid status 409-vs-400, filing_status lifecycle,
applicability value/exactly-one validation, RFC 9457, Location header, pagination, stale
progress.md, missing Postman/openapi, store-test safety, outbound URL construction, compose
depends_on. Section B (WFC-05) unchanged and unscored.

**workflow-svc — 26 → 29 ✅, 23 → 24 ⚠️, 12 → 8 ❌ (19 October).**
ListPendingApprovals ❌→✅ (`GET /v1/workflows/pending`, live 200), GetApprovalEvidence ❌→✅
(`GET /v1/workflows/{id}/evidence`, live transition chain), RequestChanges ❌→✅
(`POST …/request-changes`: eligible only from REJECTED/ESCALATED, live reopen with the true
from-state and the calling actor). Versioned definitions ❌→⚠️ (definition CRUD + lifecycle —
`supersede` implemented atomically, activate/suspend/retire, get/list — all live-verified;
instances are still not pinned to a definition version and the table still lacks RLS).
Escalate/cancel authorization had already closed on 14 October. The 19 October pass also fixed
the commit-blocking compile break, the missing `000008_add_escalation_target` migration, 24-file
gofmt drift, a cross-tenant definition read leak and the platform-scope sentinel
(`AUTHORIZE` of definition ops). Remaining ❌ (8): Expire, delegation, deadline timers, durable
stuck-instance recovery, Idempotency-Key semantics (RLS-hidden stage comparison), decision-log
writes (H-3 + wire shape), evidence lineage/authority snapshot, consumed events (Q-4). Remaining
⚠️ (24) include quorum races, escalation dead end, RLS on 6 tables, per-type approve authority
(H-1/H-2), gateway identity (X5), state model, common columns.

**evidence-requirements-svc — 16 → 20 ✅, 18 ⚠️, 9 → 5 ❌ (19 October).**
Outbox publication ❌→✅ (claim fixed — explicit 6-column contract, safe getters, complete
payload; live: events claimed, attempts/errors recorded with Kafka down), `idempotency_key`
mandatory/mismatch ❌→✅ (payload fingerprints on all four write paths → 409; live-proven),
idempotent requirement create ❌→✅, malformed write ids ⚠️→✅ (22P02 → 400 `invalid_identifier`;
the legitimate unfiltered PBC list now 200 — it was 503). Legal entity validated by the server
❌→⚠️ (envelope authoritative, mismatch 400; no registry check). Remaining ❌ (5): real
document-vault verification path, distinct-artifact double-count, gate coverage for
GL-post/close-lock/vat-gst/corporate-tax, and the two caller-side gate gaps (envelope,
allow-list fail-open). Also fixed this pass: PBC authorization now entity-scoped, scan-result
gated, concurrent response retries serialized, empty-key collision. Open decisions: evaluate
authorization action, `openapi.yaml`, scan ownership, tier, `NO_REQUIREMENTS_DEFINED` semantics.

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3642–3669 (“Group arithmetic”)

#### Group arithmetic

436 scored = the 418-row baseline + 17 authorization session-control rows + 1 policy-svc row.
Weighted credit uses half for ⚠️: (343 + 76/2) / 436 = 381 / 436 = **87.4%**.
28 September baseline: (165 + 144/2) / 418 = 237 / 418 = 56.7%.

#### Remaining open items (group-wide)

- Cross-service and platform: X1 caller envelopes (evidence callers), X5 gateway-signed identity
  everywhere, X9 RFC 9457 error classes, X3 real Kafka brokers for end-to-end relay proof
  (all outboxes are wired and retry-test-proven), C3 event-catalogue naming, C4 API surface,
  C1 WFC-05 scope, C8 obligation-source ownership.
- 17 ❌ split: jurisdiction 2 (validate-action, regulatory feed), obligations 2 (single owner,
  applicability dedup), workflow 8 (expire, delegation, timers, recovery, idempotency,
  decision-log writes, lineage, consumed events), evidence 5 (document-vault verification,
  distinct artifacts, gate coverage, 2 caller-side).
- Decisions: workflow H-1/H-2 grants (access-control-svc), evidence evaluate action, required
  WFC-05 scope; obligations decision D-1 ("outbox: wire or delete") is now **resolved — wired**.

#### Caveats

- CI was not run on this exact working tree; the suites above ran on the Windows host against
  local Postgres 16, with the Docker race suite as the CI equivalent for the services that
  carried one this week.
- Live acceptance dates precede the obligations outbox fix (17 Oct); the relay is covered by
  unit and store tests but not a broker.
- Mutation testing was not re-run in this pass; it was run per fix in the 12–14 October passes.

### E3 · Group-level decisions

> Source: `Governance Platform-audit-2026-09-28.md`, lines 195–214 (“Needs clarification — group-level decisions”)

#### Needs clarification — group-level decisions

These questions came up in more than one service. Each answer moves a set of ❓ rows, and in
several cases turns a block of ❌ rows into "future scope". They are listed in order of how
many rows they unblock.

| # | Question | Affects | Why it matters |
|---|---|---|---|
| C1 | **Which spec binds each service?** Doc 03 §8.x (the v1 service specs), or the multi-service packs V-001 (PDC-01…05), R-001 (WFC-01…05) and ZS-JUR-001? | policy-svc (~12 ❌ rows), workflow-svc (~12 ❌ rows), obligations-svc (16 unscored WFC-05 rows; score 65% → 54% if binding), jurisdiction-rules-svc (rows on approval, bitemporal replay, versions, precedence and outcomes) | Decides whether the lifecycle, versioning, deadline and precedence rows are defects or roadmap. |
| C2 | **GOV-xx ownership.** GOV-04 (SoD exceptions and compensating controls), GOV-07 (Evidence Ledger), GOV-12 (maker-checker and break-glass). The decision log and the evidence-requirements gate have no GOV number. | authorization-svc, governance-decision-log-svc, evidence-requirements-svc | GOV-04's exception lifecycle and all of GOV-12 have no implementation anywhere in the estate. GOV-07 is probably owned by evidence-manifest-svc, document-vault-svc or audit-event-store-svc in Group 3. |
| C3 | **Which event naming is the wire contract?** Doc 03 dotted names, GOV §17 PascalCase, V-001 / R-001 names, or the Event Catalogue's `com.zoikosuite.<domain>.<aggregate>.<fact>` | All 7 | Every service currently follows Doc 03, and workflow-history-svc consumes those names. |
| C4 | **Are GOV-xx and V-001 / R-001 paths binding?** (`/internal/v1/gov05/commands/*`, `/v1/packages/*`, `/v1/approvals/{id}/decisions`, `/internal/authorization/decisions`) | policy, workflow, authorization | **Precedent:** the Group 1 audit ruled GOV-01's "Illustrative contract surface" column illustrative, not binding (23 Sep 2026). The same column heading appears in GOV-03 / 05 / 06. |
| C5 | **Is "evaluate" a material write?** The envelope treats `POST …/evaluate` as one and demands `Idempotency-Key` and `X-Legal-Entity-Id`. authorization-svc exempts `/v1/authorize`. | policy-svc, evidence-requirements-svc | Decides whether to change the services' envelope policy or fix every caller. |
| C6 | **Must evidence writes block the decision?** Doc 03 §3.9 and §8.1 say evidence failure must not block evaluation; V-001 §14.1 step 7 and GOV §1 say no material success without evidence. | policy-svc, governance-decision-log-svc, workflow-svc | policy-svc chose best-effort, which is also what makes X2 silent. |
| C7 | **Who owns role and permission-bundle master data?** authorization-svc (Doc 03 §8.3 "RBAC mappings") or access-control-svc (§9.4 "role catalogues")? Tracker row 79 is Not Started. | authorization-svc | Decides where gap 1 has to be fixed. |
| C8 | **Who owns obligations, filing calendars and entity-jurisdiction mappings?** obligations-svc vs obligation-tracking-svc vs filing-tracker-svc; jurisdiction-rules-svc (Doc 03 §8.2 "Owns") vs tenant-entity-registry-svc and obligations-svc (where the code puts them) | obligations-svc, jurisdiction-rules-svc | |
| C9 | **Does `NO_REQUIREMENTS_DEFINED` permit or block?** | evidence-requirements-svc and its callers | The catalog starts empty and every caller permits, so once X1 is fixed every gated action would pass with no evidence. |

---

### E4 · jurisdiction-rules-svc open register (12 Oct)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 2628–2697 (“jurisdiction-rules-svc — open gap register (12 October 2026)”)

#### jurisdiction-rules-svc — open gap register (12 October 2026)

**Current score:** 73 ✅ / 4 ⚠️ / 2 ❌ of 79 scored = **92.4% full, 94.9% weighted**
(verified re-audit plus Record #313; the 28 Sep baseline was 49% / 63.9%).

**How to use this register:** pick an item up when its *Waits on* is cleared, apply the fix,
run the *Verify* scenario, then re-score the listed audit row. "Row" is the row number in the
82-row table of the *Re-audit, 12 October 2026* section.

**Dependency status, as of 12 Oct:**
- **On hold — the user owns them:** obligations-svc and workflow-svc.
- **On hold — the user's instruction:** every other cross-service gap.
- **Complete:** tenant-entity-registry-svc (consumed, Record #312).

##### A. Blocked on another service (on hold)

| ID | Row | Gap | Waits on | Fix in this service once unblocked | Verify |
|---|---|---|---|---|---|
| J-01 | 30 | `X-Principal-Id` is trusted as sent. Host port `8082:8082` is published, and the local Traefik route has no gateway-auth, so anyone who can reach the port can act as any principal (X5). | **gateway-auth-svc** (a signed identity assertion on forwarded requests), or inbound mTLS via **mtls-management-svc**. Shared infra: remove or loopback-bind the compose port (#306) and add ForwardAuth to the Traefik route. | Verify the assertion (or the client certificate) in `requirePrincipal` (`internal/handler/handler.go`) before trusting the header, mandatory outside `ENV=local`. Same pattern as secret-vault-integration-svc `inboundmtls`. | A POST straight to :8082 with a forged `X-Principal-Id` and no assertion or certificate → 401; the same request through the gateway → normal. |
| J-02 | 36 (residual) | `X-Approval-Reference` on DRAFT→ACTIVE is recorded but never checked. Four-eyes is enforced (creator ≠ activator), but any string passes as "approval". | **workflow-svc** (user-owned): an API to read an approval by reference (status, subject, approver). | In `TransitionRuleStatus`, resolve the reference. It must be APPROVED, for this `jurisdiction_rule_id`, by someone other than the creator; otherwise 403 `approval_not_valid`. Fail closed (503) if workflow-svc is unreachable. | A made-up reference → 403; an approval for a different rule → 403; a valid approval → 200, and history `change_reason` holds the reference. |
| J-03 | 68 | No consumer for external regulatory feed changes. | **external-data-feed-svc**. Today it publishes only generic `external.feed.event.ingested` / `external.feed.subscribed` on `zoiko.external.data.feed.events`, with no regulatory payload. It needs an agreed event, for example a regulatory source change carrying `external_feed_reference`, authority and a change summary. | A consumer in `internal/consumer` (same pattern as `entity.go`: FetchMessage, commit after apply, `consumed_events` dedupe) that matches `jurisdiction_rules.external_feed_reference` and records drift (`UNDER_REVIEW`) through `RecordDriftWithQuerier`, publishing `legal.drift.detected`. | Publish a feed change for a reference held by an ACTIVE rule → the rule goes `UNDER_REVIEW`, a drift event row exists, and `legal.drift.detected` is in the outbox. A redelivery causes no second drift row. |
| J-04 | 54 | Legal drift is recorded only by hand (`POST …/drift`); it is never *detected*. | Same as J-03. | Comes with J-03. | Same as J-03. |
| J-05 | 53 | Entity applicability mappings are a projection; ownership is undecided (C8). | Decision C8 with **tenant-entity-registry-svc** and **obligations-svc** (user-owned). | If this service becomes owner: move the assignment write API here and make the registry consume. If not: re-score row 53 as a deliberate projection (✅ against Doc 03 with a doc note). | n/a, decision first. |

##### B. Needs a decision (no dependency, just a choice)

| ID | Row | Gap | Decision needed | Proposed answer | Verify once built |
|---|---|---|---|---|---|
| J-06 | 4 | `POST /v1/validate-action` is documented in `openapi.yaml` and Doc 03 §8.2 but not built; clients built from openapi get 404. | How an `action_type` (e.g. `tax.calculate`) maps to a rule. | `action_type` = `<rule_domain>.<rule_code>`. Resolve with `ResolveRulePack` (`required_codes=[rule_code]`, `domain=rule_domain`, `effective_at`). RESOLVED → 200 `{permitted:true, rule, resolved_from, pack_digest}`; CONFLICTED or INSUFFICIENT → 409 `{permitted:false, detail, …}`; unknown jurisdiction → 404. Authorize as `JURISDICTION_RULE_VALIDATE`. | RESOLVED, CONFLICTED, INSUFFICIENT and unknown-jurisdiction cases. Route-parity check: `/v1/validate-action` no longer "documented, not routed". |
| J-07 | 56 | The rule basis of governed actions is recorded only by the ZS-JUR resolver (`rule_decision_evidence`), and `RESOLVER_ENABLED` is off by default and on the stack. The base rule pack returns `pack_digest` but records nothing. | Turn the resolver on in compose (it needs `RESOLVER_RING` / `RESOLVER_REGION`), or have this service record evidence for rule-pack calls. | Enable the resolver on the stack. Callers of `/rule-pack` store `pack_digest` with their action. | `/v1/rule-resolutions:resolve` answers and writes a `rule_decision_evidence` row; row 56 ✅. |
| J-08 | — | Two precedence models: registry `precedence` (higher wins; used by the ZS-JUR resolver; set via provenance) and `precedence_level` (lower wins; used by `/rule-pack`; set on create). They can disagree for the same rule. | Pick one model. | Keep `precedence_level` for `/rule-pack` and document `precedence` as the resolver's, or migrate one into the other. | One precedence per rule; both resolution paths agree in a test. |
| J-09 | 35, 51, 65 (❓) + group C1/C8 | The open questions from 28 Sep: read endpoints public vs the INTERNAL tier; `rule_payload` content (metadata only vs rates); event and topic naming (Doc 03 vs Event Catalogue); which spec binds (C1); `X-Tenant-Id` on tenantless writes; whether rules from deactivated ancestors are inherited. | Product / architecture owner. | — | Re-score the ❓ rows once answered. |

##### C. Fixable now in this service (no dependency), lower severity

| ID | Gap | Fix | Verify |
|---|---|---|---|
| J-10 | Entity mappings exist only for events still inside Kafka retention: locally, 75 of 80 entity events had already expired. Older entities have no mapping. | A one-off backfill command or admin endpoint that pages tenant-entity-registry-svc's entities and jurisdiction assignments (read API) and upserts through `ApplyEntityApplicability` with synthetic event ids `backfill:<assignment_id>:<version>` (version-guarded, so a later live event wins). | Seed an entity in the registry, delete the topic's retained events, run the backfill → `/v1/entities/{id}/jurisdictions` returns it; re-running is a no-op. |
| J-11 | Registry (ZS-JUR) events (`PublishRegistryEvent`) are written to the outbox **after** the registry write commits, through the pool, not inside the transaction. A crash in between loses the event. | Thread the registry store's `tx` into `emitToOutbox` (pass a `Querier`) for each registry write. | Fault-inject an error after commit → no event is lost; after a rollback → no event. |
| J-12 | Registry events carry no `aggregate_version` (base events do since #313). | Pass the aggregate's version where the registry tables have one. | Event test asserts the field. |
| J-13 | `If-Match` covers rule transition and drift, not jurisdiction deactivate. Jurisdictions have `row_version` since 000020. | Optional `If-Match` on `POST /v1/admin/jurisdictions/{id}/deactivate`, checked against `jurisdictions.row_version`. | Stale If-Match → 412; current → 200. |
| J-14 | `TestPgStore_PackDigestPinsTheAnswer` failed once in a combined store+handler run and could not be reproduced in 10 runs. It now asserts the pack is non-empty first. | If it recurs, read the assertion message. Suspect the clock or timing of `at` against the history row's `known_from`. | 20 consecutive full-suite runs green. |
| J-15 | The outbox worker reads with no `FOR UPDATE SKIP LOCKED`, so two replicas can publish the same event twice. At-least-once delivery; consumers dedupe on `event_id`. | Claim rows with `FOR UPDATE SKIP LOCKED` in a transaction, like the tenant-entity-registry relay. | Two workers on one outbox → each event published once. |
| J-16 | Parked outbox events (after 10 failed publishes, Record #313) have no replay tool, only a metric and a log line. | Admin endpoint or script to list and requeue parked events (reset `publish_attempts`). | Park one, requeue it, it publishes. |
| J-17 | Authorization is per action and target, but authorization-svc only *records* `resource_id`; it does not evaluate per-resource policy. | Cross-service in authorization-svc (on hold), noted for completeness. | — |

##### D. Environment, deploy and process

| ID | Item | Action |
|---|---|---|
| E-1 | **The local database is missing this service's migrations 000005–000017** (all registry siblings `000005_jurisdiction_pack_registries` … `000009_pack_release_operations`, and 000010–000017 including `000016_fix_rule_status_history_trigger`). **Every rule INSERT fails locally** (`known_from` NOT NULL). 000018–000020 were applied by hand (`method='manual'`). | Run the tracked migrations runner (`migrate.py status` then `up`). Python is not installed on this workstation. Then re-run the live checks. |
| E-2 | Deploy order: apply **000018** (idempotency keys; without it every `/v1/admin/*` write is 503 because the middleware fails closed), **000019** (entity projection), **000020** (row version and immutability; it repairs `rule_version = 0` rows before its guard). | Apply before rolling out the new image. |
| E-3 | Shared infra, not edited here: `deployments/scripts/create-app-roles.sh` re-grants UPDATE/DELETE on all tables (the DB triggers carry append-only and immutability); compose publishes `8082:8082` (J-01). | Platform owner. |
| E-4 | Doc 03 §8.2 was edited (two published events added). The Postman collection does not cover the new routes (`/v1/entities/{id}/jurisdictions`, `known_at`, `If-Match`). | Extend `postman/postman/ZoikoSuite_JurisdictionRules.postman_collection.json`. |
| E-5 | All of the above work is **uncommitted** on `shashi-changes`: 19 modified files plus new migrations 000018–000020, `internal/consumer/`, `internal/handler/{entity,idempotency,occ}.go`, `internal/store/{entity,idempotency}_store.go`, and their tests. | Review, then commit. |
| E-6 | Running tests on this workstation: Windows Application Control intermittently blocks Go test binaries. The integration suite runs reliably only as `go test -c -tags integration -o <scratch>/registryit.test.exe` and then executing the binary. Store tests need `EMBEDDED_PG_STORE_TESTS=1`. | — |

##### Re-check order when resuming

1. **E-1 and E-2:** migrate the local DB, then re-run the live compose checks.
2. **J-06:** once the decision is made, build validate-action (closes row 4).
3. **J-10, J-11, J-13, J-15, J-16:** in-service, can be done any time.
4. **J-02:** after workflow-svc.
5. **J-01:** after gateway-auth-svc / mtls-management-svc, plus the infra change.
6. **J-03 / J-04:** after external-data-feed-svc publishes a regulatory change event.
7. **J-05, J-07, J-08, J-09:** decisions. Then re-run the full 82-row re-audit and replace
   this register.

---

### E5 · governance-decision-log-svc open register (12 Oct)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 2997–3068 (“governance-decision-log-svc — open gap register (12 October 2026)”)

#### governance-decision-log-svc — open gap register (12 October 2026)

**Current score:**
- Original 47 scored rows: **41 ✅ / 6 ⚠️ / 0 ❌ = 87.2% full, 93.6% weighted**. The 28 Sep
  baseline was 34% / 58.5%; the true start on 12 Oct, re-checked live, was 40.4% / 66.0%.
- All 56 rows, including re-audit findings R-1…R-9: **49 ✅ / 6 ⚠️ / 1 ❌ = 87.5% full, 92.9%
  weighted**.

**Status: every open item is ON HOLD** (user instruction, 12 Oct). Nothing below can be closed
inside this service. Each item waits on another service, or on a decision.

**How to use this register:** when an item's *Waits on* is done, apply *Then here* if anything is
listed, run *Verify*, and re-score the row. Row ids are the ones used in this service's
*Gap closure*, *Re-audit* and *Held-gap pass* sections.

##### Progress so far (12 October 2026, all uncommitted)

| Pass | Score (47 rows) | What closed |
|---|---|---|
| 28 Sep audit | 34.0% / 58.5% | — |
| Live re-check at the start of 12 Oct | 40.4% / 66.0% | The 9 Oct commit 4820b5e1 was found to have a dead outbox, a migration (000007) that could not apply, and 4 regressions |
| Gap closure | 70.2% / 85.1% | A3, I1, V2, V3, V5, D4, D7, D12, E10, A13, N2, I6, I7, I4, N3 (outbox relay rebuilt; tenant-scoped key; idempotency truthful; `openapi.yaml`) |
| Gap pass 2 | 85.1% / 92.6% | V4 (jurisdiction context, 000015), I5 (replay event), E4/E5/D1/D2/D5 (Doc 03 §8.7 and Doc 04 §7.1 corrected) |
| Re-audit | 85.1% / 92.6% (56 rows: 71.4% / 83.0%) | Found R-1…R-9 |
| Re-audit fixes | (56 rows: 85.7% / 92.0%) | R-1, R-2, R-3, R-4, R-6, R-7, R-8, R-9 |
| Held-gap pass | **87.2% / 93.6%** (56 rows: **87.5% / 92.9%**) | I3 (envelope refusals rendered as RFC 9457) |

##### A. Blocked on another service (on hold)

| ID | Row | Gap | Waits on | Then here | Verify |
|---|---|---|---|---|---|
| D-01 | R-5 | **No workflow-svc decision reaches the ledger.** Its client sends 4 of the 8 envelope headers, and `RecordDecisionParams` has no JSON tags: no `decision_id` and no `rule_basis`. Every approve, reject, escalate or cancel decision is refused (400) and logged as "best-effort" | **workflow-svc** (`internal/decisionlog/client.go`): send this service's wire shape (`openapi.yaml`) and the full envelope (`svcenvelope.ForwardTo`, Idempotency-Key, X-Legal-Entity-Id, X-Purpose-Context) | Nothing | A workflow approval → 201 here; the decision is readable by `workflow_instance_id`; its event is on `zoiko.governance.events` |
| D-02 | A10 | ✅ **14 Oct: policy-svc side delivered and verified live** (tenant in the header only, `PLATFORM` entity, full envelope incl. `X-Purpose-Context`, writer = `svc-policy`; ledger rows observed for tenant-scoped and global evaluations). Re-score A10 in this service's next pass. Was: **policy-svc's global-scope writes are refused.** It sends body `tenant_id: "GLOBAL"` (and `legal_entity_id: "GLOBAL"`) with header `X-Tenant-Id: <platform UUID>`, which gets 403 `TENANT_SCOPE_MISMATCH` (and would get 400 on the entity). It also forwards the *end user* as the writer, so that user would need `GOVERNANCE_DECISION_RECORD` | **policy-svc** (`internal/decisionlog/client.go`): put the header tenant in the body (or omit it), and send a UUID or `PLATFORM` entity. Plus a **decision**: end user or workload identity as the writer | Nothing | A global-policy Evaluate → decision recorded (201), actor as decided |
| D-03 | A7 | **Read authorization is only logged on the stack.** Compose runs `DECISION_LOG_READ_AUTHZ=warn` because two readers send no principal | **contract-lifecycle-svc** (`internal/governancelog/client.go`) and **decision-support-svc** (`internal/clients/governancelog.go`) forward `X-Principal-Id`; **access-control-svc** grants `GOVERNANCE_DECISION_READ` (entity or platform scope) to each reader's principal, evidence-manifest-svc's included | Set `DECISION_LOG_READ_AUTHZ=enforce` in compose (the code default) | Contract activation still passes; a reader without a grant → 403; no "read admitted" warnings in the logs |
| D-04 | A11 | **The read callers are out of envelope contract.** contract-lifecycle sends X-Tenant-Id only; decision-support has no request id or source channel; evidence-manifest has no request id or source channel. They pass only because write-strict admits incomplete reads | **contract-lifecycle-svc**, **decision-support-svc**, **evidence-manifest-svc**: forward the envelope with `svcenvelope.ForwardTo` | Optionally `ZS_ENVELOPE_ENFORCEMENT=strict` | No `X-Envelope-Contract: violated` on their reads |
| D-05 | A1 | **`X-Tenant-Id` / `X-Principal-Id` are trusted as sent (X5).** Host port 8083 is published, the local Traefik route has no gateway-auth, and GTRM has no route for this service | **gateway-auth-svc** (ForwardAuth on the route; GTRM entry) and **mtls-management-svc** (workload identity for service-to-service calls) | Optionally verify the workload identity on writes | A direct call with a forged principal is refused at the edge |

##### B. Needs a decision

| ID | Row | Gap | Decision needed | Proposed answer | Then |
|---|---|---|---|---|---|
| D-06 | A9 | `X-Purpose-Context` is required on every append. Its own rationale (`services/_contract/rollout.sh` SENSITIVE list: "governed sensitive access") means *reads*, and GCP §16 scopes purpose to privileged, destructive or emergency commands. Neither covers an evidence append | Should purpose be required on appends, on reads, or both? | Required on **reads**, not appends. This needs a `RequiredOnRead` level in **services/_contract** (the shared generator), and the readers (D-03/D-04) to send purpose first | Regenerate `internal/envelope/contract.go` with `rollout.sh` |
| D-07 | V1 | Event names follow Doc 03 (`governance.decision.recorded` / `.replayed`), not the Event Catalogue pattern `com.zoikosuite.governance.decision.recorded` | **Group decision C3**: which naming is the wire contract | Keep Doc 03 (every service and workflow-history-svc use it) unless C3 says otherwise | Rename both event types in `internal/events` and in Doc 03 §8.7 |

##### C. Unscored (❓, carried from the 28 Sep audit)

| Row | Question |
|---|---|
| E3 | What the "audit" query dimension in Doc 03 §8.7 means |
| D10 | Whether GOV-07's canonical hash / digest chain applies to this service (C2) |
| D11 | Whether `decision_id` is the §16 `evidence_id` |
| D13 | Which retention or record class applies to governance decisions (ZS-DATA-GOV-001) |
| N4 | Which GOV-xx section owns GovernanceDecision (C2) |

##### D. Deploy checklist for the uncommitted work

1. Apply migrations **000007** (repaired and re-runnable; it fixes deployments where the old version failed or partly applied) and **000012–000015** *before* the new binary. Reads select `stored_at` and `jurisdiction_context`; inserts use `ON CONFLICT (tenant_id, decision_id)`.
2. The idempotency hash changed (it now covers principal, correlation, jurisdiction and legal entity). A retry, sent across the deploy, of a write first made by the old binary answers 409.
3. Compose keeps `DECISION_LOG_READ_AUTHZ=warn` until D-03 is done.
4. `OUTBOX_RETENTION` defaults to 168h. Alert on `outbox_oldest_pending_seconds`.
5. Housekeeping: `server.exe` (30 MB) has been tracked in git since 4820b5e1; remove it.

##### Suggested order to resume

1. **D-02 policy-svc** and **D-01 workflow-svc**: the ledger starts receiving the two main decision streams.
2. **D-04, then D-03**: read callers forward the envelope and principal, grants are seeded, and compose switches to `enforce`.
3. **D-05**: once gateway-auth-svc and mtls-management-svc are complete.
4. **D-06 and D-07**: whenever the decisions are made.
5. Re-run the full re-audit and replace this register. Expected after D-01…D-05: 56 rows at 54 ✅ / 2 ⚠️ / 0 ❌ (96.4% / 98.2%); after D-06/D-07 as well: 100%.

---

### E6 · policy-svc open register (14 Oct, rounds 1 and 2)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3162–3189 (“Open register”)

#### Open register

| ID | Rows | Gap | Waits on | Then |
|---|---|---|---|---|
| P-01 | A2, F4 | SPEND_CONTROL, SIGNATORY_MATRIX and SOD_RULE have no defined payload or evaluation semantics | Business input | Add an evaluator per type; `validateRulePayload` per type |
| P-02 | B5 | X5: identity headers trusted as sent | gateway-auth-svc ForwardAuth (held, as for every service) | Nothing here |
| P-03 | E4 | `jurisdiction_context` / `residency_region` on events | Decision: whether policies carry jurisdiction (V-001 PDC-02 applicability) | Add to versions and envelopes |
| P-04 | A5, A10, F1, F2 | Doc drift | Approval of DC-1…DC-4 below | Re-score to ✅ |
| P-05 | B2, B3 | Gates run `warn` in compose | **access-control-svc** registers `POLICY_EVALUATE`, `POLICY_READ`, `POLICY_CREATE_GLOBAL`, `POLICY_VERSION_{APPROVE,SUSPEND,RETIRE}[_GLOBAL]`; **expense-claim-svc** and **payment-authorization-svc** forward the envelope (they send `X-Tenant-Id` only and have been answered 401 by Evaluate since 9 Oct in enforce); **governance-decision-log-svc** replay principal gets `POLICY_READ` | Set `POLICY_EVALUATE_AUTHZ` / `POLICY_READ_AUTHZ` to `enforce` |
| P-06 | D1 | The ledger writer needs `GOVERNANCE_DECISION_RECORD` | **authorization-svc** grant to `svc-policy` (platform scope and the entities it records for) | Until then evidence waits, durable, in `policy_evidence_outbox` |
| P-07 | D1 | `X-Purpose-Context: policy_evaluation_evidence` is sent because the decision log requires it on appends | decision-log D-06 | Drop it if D-06 moves purpose to reads |
| P-08 | B8 | GOV-05 negative path #3 says suspension is "evidenced **and reviewed**": it is evidenced; no review workflow exists | GOV-12 has no owning service | Route suspensions to the GOV-12 owner |
| P-09 | C18 | `expected_version` is optional; GCP §16 says mandatory for "protected" updates without defining which | Decision | Make it mandatory for the chosen commands |
| P-10 | — | V-001 PDC-05 (certification suites, simulation, waivers), decision models, authority sources | Roadmap (C1) | — |
| P-11 | B9 | A pre-000007 control-test execution or attestation has no tenant and is now readable by no one (fail closed) | Data owner, if any such rows exist | Assign `tenant_id` |

#### Proposed doc corrections (DOC DRIFT — not applied)

- **DC-1, Doc 03 §8.1 Inbound APIs:** add the REST route map (in `services/policy-svc/progress.md`)
  as the physical form of the GOV-05 operations, including the control-test and attestation routes
  (doc7 §E3 / §E6).
- **DC-2, Doc 04 §7.1:** a physical-name note: `policy_domain` → `policy_type`, `policy_payload` →
  `rule_payload`, `activation_status` → `version_status` (values: the GOV-05 state model).
- **DC-3, Doc 03 §8.1 Published Events:** add `policy.version.submitted`, `.approved`, `.scheduled`,
  `.suspended` and `policy.applicability.conflicted` (subject to C3).
- **DC-4, GCP §16:** add `POLICY_CONFLICTED` (V-001 §5 applicability CONFLICTED) to the stable
  classes, or name the existing class conflicts should use.

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3238–3255 (“Register after round 2”)

#### Register after round 2

| ID | Status |
|---|---|
| P-01 (A2, F4) | **Open — business input.** No spec defines payloads or evaluation for SPEND_CONTROL, SIGNATORY_MATRIX or SOD_RULE (Doc 03 gives spend evaluation to the Spend Controls Service §12.12, and SoD evaluation sits with authorization-svc). |
| P-02 (B5) | Closed in code. Residual = P-12. |
| P-03 (E4) | Closed. |
| P-04 | Closed (DC-1…DC-4). |
| P-05 | Closed (registry, callers, seed, compose `enforce`). Production grants remain an IAM administrator's act. |
| P-06 | Closed for the demo stack (seed script); production: grant `svc-policy` the same. |
| P-07 | **Held** on decision-log D-06 (`X-Purpose-Context` on appends). |
| P-08 | Closed inside GOV-05 (suspension review). GOV-12 still has no owning service for maker-checker in general. |
| P-09 | Closed (mandatory). |
| P-10 | Roadmap (V-001 PDC-05). |
| P-11 | Data: any pre-000007 control-test execution or attestation needs a `tenant_id` (none exists in the scratch or test data). |
| **P-12 (new)** | Compose runs `POLICY_IDENTITY_VERIFY=warn` because governance-decision-log-svc's replay reads policy versions without the signed envelope (its generated `ForwardTo` carries no `Authorization`) and no peer calls over mTLS yet. When it forwards the envelope (or calls `:8449`), set `enforce`. |
| P-13 (new, not scored) | expense-claim-svc and payment-authorization-svc answer a policy-svc 401/403 as 503 `policy-svc unavailable` (status logged). Worth a distinct error there. |

### E7 · obligations-svc: rows not ✅ at the 28 Sep audit (obligations has no later register; many of these closed on 19 Oct, see the row movements in E2; current status is the tracker's D2-4)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 1118–1369 (“4/7 — obligations-svc (:8088) vs Doc 03 §8.5 and Doc 04 §13 (R-001 WFC-05 recorded, unscored)”) — only rows marked ⚠️ / ❌ / ❓

#### Section A — Ownership and binding (Doc 03 §8.5)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Owns obligation **definitions** | 03 §8.5 Owns | 000001:24 (a single `obligations` table) | ⚠️ | One flat record, with no split between definition and instance and no versioned definition. |
| Obligation-to-entity **mappings** | 03 §8.5 | 000001:28 (one `legal_entity_id`) | ⚠️ | No mapping table; an obligation binds to exactly one entity. |
| Obligation-to-workflow mappings | 03 §8.5 | none | ❌ | No column, table or API links an obligation to a workflow instance. |
| **Legal entity exists and belongs to the caller's tenant** | 03 §8.5 entity-bound; known-gaps.md:879-894 (OPEN) | handler.go:311 (authz only) | ❌ | A principal with a grant on entity X can write X-scoped obligations into **any tenant** just by changing `X-Tenant-Id`. |
| Single owner of obligation truth | 03 §3.3; 04 §2.1; tracker row 78 (Not Started) | separate `obligations` tables in obligations-svc, obligation-tracking-svc and filing-tracker-svc; obligation-tracking-svc handler.go:121 and 220 emit **`obligation.created` / `obligation.updated`** | ❌ | §12.3 specifies `contract.obligation.*`. Two producers share one event type on different topics, which invites consumer confusion (C8). |

#### Section A — Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Publishes `obligation.updated` | 03 §8.5 | handler.go:555-561; publisher.go:103-109 | ⚠️ | Fires only on status transitions. There is no attribute-update API, so "updated" never covers data changes. |
| **Overdue detection** (the due date passing) | 03 §8.5 (owns due dates and publishes overdue) | none; progress.md says "No built-in scheduler" | ⚠️ | A past-due obligation stays OPEN **forever** unless an external caller POSTs OVERDUE, and no such caller exists in the estate. See B7 for the stricter WFC-05 rule. |
| Event type and channel naming | Event Catalogue §5.1-5.2 | publisher.go:86 etc.; config.go:92 `zoiko.obligations.events` | ⚠️ | Estate-wide cosmetic drift; the wire names match 03 §8.5 (C3). |
| Partition key = hash(tenant, aggregate) | Event Catalogue §5.3 | publisher.go:163, key = obligation_id | ⚠️ | Per-aggregate ordering still holds. Minor. |
| Transactional outbox | Event Catalogue §6 (L389-392); R-001 R-INV-23 | handler.go:348-372, 548-580 (published after commit, straight to the broker) | ❌ | A Kafka failure, or the request context being cancelled after commit, loses the event permanently, leaving only a log line. Group-wide X3. |
| Alertable failure states | 03 §3.8 | telemetry.go:64-76 (HTTP and readiness metrics only) | ⚠️ | No publish-failure counter, so event loss cannot be alerted on. |

#### Section A — Idempotency

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Idempotent obligation create | 03 §3.7; API standard L381 | pg_store.go:225-298 | ⚠️ | Dedup is on (tenant, code), but the conflict check compares only entity, jurisdiction, type and due_date (L287-290). A replay with a different severity, source_reference or responsible_function silently returns 200 with the *old* record. |
| Idempotent filing-requirement create | 03 §3.7 (names "filing submission") | pg_store.go:514-553 | ❌ | No dedup: a retry creates a duplicate. A reused caller-supplied id hits PK 23505 and gets 503. |
| Idempotent applicability decision | 03 §3.7 | applicability_store.go:67-99 | ❌ | Same as above; duplicate decisions are appended. |
| `Idempotency-Key` semantics (same key replays; changed body → 409) | API standard L375-385; envelope INV-08 | envelope/policy.go:220-227 checks presence only; the value is read nowhere | ❌ | Required on writes, then ignored. Group-wide X4. |
| Caller-supplied obligation_id collision | API standard 409 | pg_store.go:212-234 (the ON CONFLICT target is (tenant, code) only) | ⚠️ | A PK clash hits 23505 and gets 503. The 503-versus-201 difference reveals whether that id exists in another tenant. |

#### Section A — Tenant, auth and envelope

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tenant isolation: applicability_decisions | R-INV-22; known-gaps.md:896-918 (OPEN) | applicability_store.go:68, 105, 139 (tenant-scoped parent lookup), then straight to the pool at L87, 115, 152; 000004:26-28 | ⚠️ | The cross-tenant read leak in known-gaps is now closed at the app level by the parent pre-check, so that entry is partly stale. There is still no tenant_id column and no RLS. |
| **Principal required on applicability reads** | API standard 401; R-001 §11.1 | applicability_handler.go:210-243, 251-277 (no `requirePrincipal`); the envelope admits GETs (middleware.go:102-108) | ❌ | `facts_used` and the decider's identity can be read without authentication, given only a tenant header. |
| Identity headers are verified | 05-security; gateway doctrine | tenant.go:37-46 and handler.go:69-76 trust the raw headers; the local Traefik route has no auth (all-services.yml:12-16, 344-351) | ⚠️ | Production relies on GTRM ForwardAuth; direct access to port 8088 (compose:1077-1078) bypasses it. Group-wide X5. |
| Read authorization (object-level entity scope) | R-001 §11.1 | handler.go:391-478, 687-707 (no authz) | ⚠️ | Any principal in the tenant can read every entity's obligations. |
| Authz call carries the caller's envelope | tracker row 82i | authz/client.go:94-101 (Content-Type and correlation only) | ⚠️ | Admitted since the 82i fix, but decisions are logged with a NULL tenant: unattributed, and invisible in `GET /v1/access-decisions` (X6). |
| `X-Legal-Entity-Id` reconciled with the body's `legal_entity_id` | canonical input contract §4 (INV-02); contract.go:17 | none | ❌ | The header is required but never compared, and authz uses the body value. |
| Envelope tenant and entity resolution | envelope/resolver.go (vendored) | `NewResolver` (resolver.go:99) is never called | ⚠️ | Dead code: tenant and entity are never resolved against the registry. |

#### Section A — State, data model and applicability

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Invalid status value → 400 | the handler's own contract (handler.go:508); API standard 400/422 | pg_store.go:447-456 → `ErrInvalidTransition` → 409 (handler.go:171-172) | ⚠️ | `"FOO"` gets 409 invalid_transition. |
| filing_status lifecycle | 04 §13.1 (field listed) | pg_store.go:530 hard-codes PENDING; there is no update route | ⚠️ | The status can never change. |
| ComplianceStatus / ExceptionCase / EscalationRecord | 04 §13.1-13.2 | not built (progress.md: "Deliberate v1 scope") | ❓ | §8.5 does not assign their ownership. |
| The applicability decision value is validated | 000002:21-42 comment; doc7 §E2 | applicability_handler.go:68-91 checks non-empty only; no CHECK constraint | ⚠️ | Any string is stored, including `UNASSESSED`, which the design says must never be stored. |
| Each decision is attributed to exactly one of human or system | 000002:60-61 "exactly one" | the CHECK at L72-74 is at-least-one; the handler allows both | ⚠️ | |

#### Section A — API standard, NFRs and artefacts

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Error body is RFC 9457 problem+json | API standard L260, 471-489 | handler.go:709-716 `{"error":...}` | ⚠️ | Estate-wide drift (X9). |
| 201 includes a Location header | API standard L443 | handler.go:379, 674; applicability_handler.go:202 | ⚠️ | No Location header. |
| Pagination (cursor; default 50, max 200) | API standard L556-570 | handler.go:131-160 uses offset with 100/500; pg_store.go:561 and applicability_store.go:109 are unbounded | ⚠️ | |
| If-Match optimistic concurrency on the mutable aggregate | API standard L69, 359 | none; no row_version column | ⚠️ | The row lock prevents lost updates but not stale-intent writes. |
| progress.md is accurate | claims | progress.md:40-56, 70-75 | ⚠️ | **Stale.** It says there is no authz wiring (there is: authz/client.go) and no compose entry (there is: compose:1070), and it omits tenant scoping, applicability and the envelope. |
| Postman collection | none documented | none in `postman/` | ⚠️ | |
| Store tests are safe | known pattern: store tests wipe their DB | pg_store_test.go:22-24 (silent skip), L44 `DROP TABLE ... CASCADE` | ⚠️ | Destroys whatever database TEST_DATABASE_URL points at. |
| Outbound URL built from body input | security hygiene | jurisdiction/client.go:62 uses `Sprintf` without escaping | ⚠️ | Bounded, because the UUID column rejects non-UUIDs later. |
| Compose declares runtime dependencies | compose convention | compose:1096-1102 (postgres, kafka and authz only) | ⚠️ | jurisdiction-svc is missing, so every create gets 503 until it is up. |

#### Section B — R-001 WFC-05 (Obligation, Deadline & Escalation) — ❓ unscored

| Item | Documented | Implemented | Status | If binding |
|---|---|---|---|---|
| B1 Clock class HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY (R-INV-15) | R-001 §7.1 L466; §2 L164 | none (`severity_level` is free text) | ❓ | ❌ |
| B2 Deadline computed with rule/calendar version, timezone and business-day provenance | R-INV-16; §7.1 L470; §7.3 | due_date is supplied by the caller (handler.go:215) | ❓ | ❌ |
| B3 Recalculation supersedes; history can be reconstructed | R-INV-17; §7.1 Supersession | none | ❓ | ❌ |
| B4 Lifecycle PLANNED / ACTIVE / DUE_SOON / DUE / OVERDUE / FULFILLED / WAIVED / NOT_APPLICABLE / SUPERSEDED | §7.2 L482-512 | OPEN / IN_PROGRESS / OVERDUE / CLOSED (pg_store.go:399-403) | ❓ | ❌ |
| B5 FULFILLED requires an evidence ref or an approved exemption reason | R-INV-14 | CLOSED needs only a status string (handler.go:482-484) | ❓ | ❌ |
| B6 WAIVED / NOT_APPLICABLE need authority plus evidence | §7.1 L476; §7.2 | applicability decisions exist (source rule, facts, actor) but are not linked to obligation state | ❓ | ⚠️ |
| B7 OVERDUE is computed; scheduler delay cannot suppress it; durable timers | §7.2 L497; "Scheduler is source of truth" L55-57; NP-37 | none (progress.md) | ❓ | ❌ |
| B8 Warning and escalation schedule; DUE_SOON; escalation ≠ fulfilment | §7.1 L472; §7.4; R-INV-19 | none | ❓ | ❌ |
| B9 `POST /v1/obligations/evaluate`, `POST /v1/obligations/{id}/satisfy` | §10.1 L644-649 | not routed (handler.go:183-192) | ❓ | ❌ |
| B10 Events obligation_created, deadline_clock_started, obligation_due_soon, obligation_due, obligation_overdue, obligation_fulfilled, obligation_waived | §10.2 L667-668 | obligation.created / updated / overdue / closed only | ❓ | ❌ |
| B11 Trigger evidence (event_id + event_time + source version) | §7.1 L468; NP-12 | source_reference text only | ❓ | ❌ |
| B12 Business state and outbox written atomically | R-INV-23 | see the Section A outbox row | ❓ | ❌ |
| B13 Common columns: row_version, correlation_id, plane, data_class, residency_region, updated_by | §9.1 L590 | none of these (000001) | ❓ | ❌ |
| B14 Envelope minimum: plane, causation_id, subject version, payload hash, idempotency ref | §10.3 L670 | partial (publisher.go:27-51) | ❓ | ⚠️ |
| B15 Satisfied-after-overdue keeps the overdue period | NP-38 | the status is overwritten and there is no history table; only closed_at is kept | ❓ | ⚠️ |
| B16 Overdue-obligation metric by clock class, jurisdiction and owner | §12 L734 | none | ❓ | ❌ |

### E8 · authorization-svc: sign-off and dependency register (14 Oct)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3457–3472 (“Sign-off and dependency register after this pass”)

#### Sign-off and dependency register after this pass

| ID | Item | Owner |
|---|---|---|
| SO-1 | Approve TOTP as the interim "tenant/security-approved high-assurance" factor (ZS-IAM-001 §13; §31 assurance taxonomy). A WebAuthn factor follows. | Security |
| SO-2 | Approve the session maximums 8 h / 2 h / 4 h (§31 "Numeric JIT and break-glass session limits require Security approval"). | Security |
| F-1 | The console forwards `Authorization: Bearer <envelope>` on every authorization-svc call; then compose → `AUTHZ_IDENTITY_ASSERTION=require`. | Frontend |
| F-2 | MFA UI: enrol (QR), activate, step-up prompt on `401 step_up_required` → re-resolve → retry; admin reset page. | Frontend |
| F-3 | The console's server tier logs in with `client_id` / `client_secret`; then `IDP_CLIENT_AUTH=required`. | Frontend |
| M-02 | PEP client certificates, then `AUTHZ_EVALUATION_MTLS=require`. | Every PEP |
| X-1 / X-2 | Tenantless `/v1/authorize` callers; the preparer on own-object actions. | Calling services |
| X-3 | PEPs forward the gateway's `X-Authn-Age-Seconds` to `/v1/authorize`; then `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up`. | Calling services |
| R-3 | Every other service verifies the envelope instead of trusting `X-Principal-Id`. | Each service |
| B-02 | RFC 9457 problem+json (X9). | Platform |
| P-1 | Delete the stale `docker-compose.override.yml`, which turns authorization-svc's mTLS off. Until then the https JWKS fetch with this service's own certificate is unexercised live. | User / Platform |

### E9 · workflow-svc: per-row results and register (14 Oct) and the 21 Oct pass

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3278–3417 (“workflow-svc — re-audit and open gap register (14 October 2026)”)

#### workflow-svc — re-audit and open gap register (14 October 2026)

Every row of section 6/7 was re-checked against the code as of 14 Oct, with gap #1 (lifecycle
authorization) already fixed in the working tree (uncommitted). Code was read row by row. Every
suspected defect below marked **proven** was reproduced against a throwaway Postgres 16 (the real
migrations; NOBYPASSRLS role where RLS matters) or the real HTTP clients against a fake server.
The temporary probe tests were deleted afterwards.

**Result:** 26 ✅ / 23 ⚠️ / 12 ❌ / 2 ❓ over **61** scored rows, **42.6% full, 61.5% weighted**
(28 Sep: 21 / 61, 34% / 49.2%. The 12 Oct code-only re-score, 32 / 23 / 6 (52% / 71%), over-counted:
the outbox, idempotency, delegation, concurrency and decision-log "fixes" it credited each have a
hole that only a live run shows).

##### Per-row results

| # | Row | 28 Sep | 14 Oct | Why |
|---|---|---|---|---|
| 1 | Create approval workflow | ✅ | ✅ | |
| 2 | Resolve next approver | ⚠️ | ⚠️ | A terminal or ESCALATED workflow is still 404 `workflow_not_found`, same as a missing id; only the current stage is returned |
| 3 | Submit approval action | ✅ | ✅ | |
| 4 | Escalate endpoint | ✅ | ✅ | |
| 5 | Cancel workflow | ✅ | ✅ | |
| 6 | GetWorkflow | ✅ | ✅ | Transitions still not exposed (row 8) |
| 7 | ListPendingApprovals | ❌ | ❌ | No route |
| 8 | GetApprovalEvidence | ❌ | ❌ | No route; `workflow_transitions` is write-only through the API |
| 9 | RequestChanges | ❌ | ❌ | APPROVE / REJECT only |
| 10 | Expire | ❌ | ❌ | `due_at` / `next_action_at` added (000015) but never written; no timer, no EXPIRED state |
| 11 | Subject binding and invalidation | ❌ | ⚠️ | Subject type/id/version/fingerprint (optional), `/invalidate`, `/verify-release`, `workflow.approval.invalidated`. Binding is optional and nothing invalidates automatically (H-5) |
| 12 | Path and command shape | ❓ | ❓ | C4 |
| 13 | Versioned definitions, instance pinned | ❌ | ❌ | `workflow_definitions` (000016) and four store methods exist, but no route calls them, CreateWorkflow never sets `workflow_definition_id`, and the table has no RLS |
| 14 | Route server-resolved | ❌ | ⚠️ | Each named approver is now checked for `WORKFLOW_APPROVE` at create, but the initiator still chooses the approvers |
| 15 | Initiator not an approver (create) | ✅ | ✅ | SINGLE and QUORUM |
| 16 | No self-approval (decision) | ✅ | ✅ | Still no `self_approval_blocked` event |
| 17 | Approver authority fail-closed | ✅ | ✅ | |
| 18 | Authz call carries tenant and envelope | ❌ | ✅ | `X-Tenant-Id`, legal entity, principal, request id, correlation, causation, source channel and Idempotency-Key are forwarded; authorization-svc resolves the entity owner |
| 19 | Underlying approval authority | ⚠️ | ⚠️ | One `WORKFLOW_APPROVE` for every workflow type; registered (access-control 000008) but carried by no role template (H-2) |
| 20 | Independence beyond requester ≠ approver | ⚠️ | ⚠️ | The initiator now goes as `resource_owner_principal_id`; R-INV-09 relationship independence has no data source (H-6) |
| 21 | Eligibility re-checked at decision | ✅ | ✅ | |
| 22 | Authorization on escalate / cancel | ❌ | ✅ | Gap #1, 14 Oct (uncommitted): `WORKFLOW_CANCEL` / `WORKFLOW_ESCALATE` for participants, `WORKFLOW_ADMINISTER` otherwise, `WORKFLOW_INVALIDATE` on `/invalidate`. Denies everyone until H-1 |
| 23 | Delegation paths | ❌ | ❌ | **Proven:** the client calls `POST /v1/authorize/delegation`, which authorization-svc does not have; the 404 is read as "unavailable", so every caller who is not the assigned approver (delegate or stranger) gets **503**, never 200 or 403 |
| 24 | Quorum / parallel approvals | ❌ | ⚠️ | N-of-M quorum stages exist (000012), but see row 32; no parallel stages |
| 25 | Duplicate approver across stages | ❌ | ✅ | Handler check plus unique index (000014) |
| 26 | Required source inputs | ❌ | ⚠️ | Subject ref / version / fingerprint, optional; no requested transition, amount, risk or jurisdiction |
| 27 | Deadlines, SLA timers, timer recovery | ❌ | ❌ | Columns only; main.go starts no job besides the outbox relay |
| 28 | Escalation semantics | ⚠️ | ⚠️ | **Proven still a dead end:** escalate sets `current_stage = 0`, so FindCurrentStage answers not-found and the stage approver gets `wrong_approver`, although the store and the edge table now say ESCALATED can be approved. No target or owner change |
| 29 | A timeout never counts as approval | ✅ | ✅ | Trivially |
| 30 | Durable state, stuck-instance recovery | ❌ | ❌ | |
| 31 | Duplicate approval idempotent | ✅ | ✅ | |
| 32 | Concurrency control | ❌ | ⚠️ | SINGLE approve, cancel, escalate and invalidate use `row_version`. **Proven:** the quorum path updates stage and instance with no version predicate and never bumps `row_version`; concurrent final votes produced two `workflow.completed` in **14 of 15** runs, and a cancel that read first can overwrite APPROVED. No client `expected_version` |
| 33 | Idempotency-Key honoured | ❌ | ❌ | Key stored (000013), but **proven:** (a) a same-key, same-body retry under the runtime role (NOBYPASSRLS) gets **409 `idempotency_mismatch`**: the body comparison reloads stages with `context.Background()`, RLS hides them, and "can't read" counts as "differs" (passes only as superuser); (b) 000003's `(tenant_id, correlation_id)` unique index is still there, so a second start in the same saga under a new key is **503** (23505); (c) quorum pools compare sorted to caller order. Action endpoints ignore the key |
| 34 | correlation_id mandatory | ⚠️ | ⚠️ | main.go still generates it before the envelope runs |
| 35 | Every transition preserved | ✅ | ✅ | `from_state` is hard-coded `PENDING` on approve/reject, wrong after ESCALATED |
| 36 | Transition log append-only | ⚠️ | ⚠️ | **Proven:** an app-grant role can UPDATE and DELETE `workflow_transitions`; no trigger or REVOKE |
| 37 | Approver identity and outcome | ✅ | ✅ | |
| 38 | Rationale where required | ⚠️ | ⚠️ | User rationale only on the mutable stage row (and the dead ledger call); transition text synthetic; REJECT never requires one |
| 39 | Evidence / decision-log write; evidence_id | ❌ | ❌ | **Proven:** `RecordDecisionParams` has no JSON tags, so the body has Go-cased keys and no `decision_id` or `rule_basis`, and no `X-Purpose-Context`, `Idempotency-Key`, `X-Request-Id`, `X-Source-Channel` or `X-Legal-Entity-Id`: every append is refused (decision-log D-01). Called after commit and only logged on failure; quorum votes never call it; no `evidence_id` in responses. Last hop H-3 |
| 40 | Evidence lineage, authority snapshot | ❌ | ❌ | `/v1/authorize` returns `access_decision_id`; workflow-svc discards it. No route version |
| 41 | Published event names (Doc 03) | ✅ | ✅ | Plus `workflow.approval.invalidated`, not in Doc 03 (C3) |
| 42 | Event names per GOV-06 / R-001 | ❓ | ❓ | C3 |
| 43 | Outbox, publish after commit | ❌ | ✅ | `outbox_events` written in the business transaction (000011); relay with `FOR UPDATE SKIP LOCKED`, stable `X-Event-ID`, 10-attempt cap |
| 44 | Publish context | ⚠️ | ✅ | The relay runs on its own context |
| 45 | Cancellation event | ⚠️ | ⚠️ | Still `workflow.completed` with CANCELLED (C3) |
| 46 | Event envelope minimum | ⚠️ | ⚠️ | Unchanged: event_id header only; no causation, process version, plane, payload hash or evidence refs; jurisdiction empty |
| 47 | Per-aggregate ordering | ⚠️ | ⚠️ | Still no message key (LeastBytes) |
| 48 | Consumed events | ❌ | ❌ | No consumer. All three now have producers (`authority.delegated` delegated-authority-svc, `authorization.denied` authorization-svc, `policy.updated` policy-svc); what to do with them is still unspecified (Q-4) |
| 49 | Tenant required | ✅ | ✅ | |
| 50 | Client tenant not authoritative | ✅ | ✅ | |
| 51 | RLS with NULLIF | ✅ | ✅ | `workflow_instances` only |
| 52 | Isolation for stages / transitions | ⚠️ | ⚠️ | Still no RLS on stages, transitions, quorum tables, outbox or definitions; reads use the pool. Row 33(a) is a live consequence |
| 53 | Legal entity server-validated | ⚠️ | ⚠️ | Body `legal_entity_id` not compared with `X-Legal-Entity-Id`; only the approver checks at create make authorization-svc look up its owner |
| 54 | Malformed id → 400/404 | ⚠️ | ⚠️ | Path ids validated on most routes (not `/quorum-votes`); a non-UUID tenant header or legal entity still reaches the cast → 503 |
| 55 | Stable error classes | ⚠️ | ⚠️ | Mixed `writeError` and ad-hoc maps |
| 56 | Actor from verified identity | ✅ | ✅ | |
| 57 | Gateway authentication | ⚠️ | ⚠️ | Traefik route still stripPrefix only; compose still publishes 8090 (H-4) |
| 58 | Instance state model | ⚠️ | ⚠️ | INVALIDATED added; ESCALATED undocumented; no InProgress / Expired |
| 59 | Workflow vs approval state | ⚠️ | ⚠️ | One column |
| 60 | Approval does not execute the business transition | ✅ | ✅ | |
| 61 | Common columns | ❌ | ⚠️ | `row_version`, `updated_at`, `plane`, `data_class`, `residency_region` added (000012); `created_by` is never written (always `''`) |
| 62 | Observability | ✅ | ✅ | |
| 63 | Health / readiness | ✅ | ✅ | |

##### New defects (not separate rows in the 28 Sep audit)

| ID | Defect | Proof |
|---|---|---|
| N-1 | Invalidating an already-INVALIDATED workflow is 409 (`INVALIDATED -> INVALIDATED` edge), so a plain retry never reaches the store's idempotent path | Handler probe: 409 `invalid_transition` |
| N-2 | `POST /v1/workflows/{id}/verify-release` has no authorization call (any tenant principal can probe an approval's subject fingerprint) | Code: handler.go VerifyRelease |
| N-3 | invoice-approval-svc (caller, not this service) posts to `/v1/workflows/instances`, which does not exist, with a body workflow-svc does not accept and only `X-Tenant-Id`; on any failure it makes up a random workflow id, so its approvals never go through workflow-svc | invoice-approval-svc clients.go StartWorkflowInstance |
| N-4 | Duplicate migration versions (two `000010_*`, two `000012_*`, no `000008`). Harmless under migrate.py (keyed by filename, sorted), fatal under golang-migrate | `ls deployments/migrations` |

##### A. Blocked on another service (on hold)

| ID | Rows | What waits | Service | Why it depends on that service |
|---|---|---|---|---|
| H-1 | 22 | Granting cancel / escalate / invalidate to anyone | **access-control-svc** | Must register `WORKFLOW_CANCEL`, `WORKFLOW_ESCALATE`, `WORKFLOW_INVALIDATE`, `WORKFLOW_ADMINISTER` (HIGH) in `permission_definitions` (same shape as its 000019). A permission bundle may only name registered actions (000009 raises otherwise), so until then no role can hold them and the three endpoints answer 403 to everyone |
| H-2 | 19 | Approvals working on a fresh deployment | **access-control-svc** | `WORKFLOW_APPROVE` is registered but no role template carries it, so every approval is denied until someone grants it by hand. Per-workflow-type action codes are also a decision (Q-6) |
| H-3 | 39 | Decisions reaching the ledger | **authorization-svc** (grant) + **governance-decision-log-svc** (contract) | The ledger checks `GOVERNANCE_DECISION_RECORD` against the `X-Principal-Id` it receives; workflow-svc sends the human actor, who will not hold it. policy-svc solved the same thing with its own service principal (`svc-policy`, P-06), which needs the grant in authorization-svc. The decision log also requires `X-Purpose-Context` on appends (its D-06). The wire shape, headers and a durable outbox are local and not held |
| H-4 | 57 | Trusting `X-Tenant-Id` / `X-Principal-Id` | **gateway-auth-svc** / shared deployments | The traefik route and the published port live in `deployments/`, not in this service; the forward-auth middleware is the gateway's, held for every service (as policy-svc P-02) |
| H-5 | 11 | Automatic material-change invalidation | **The business-object owners** (invoice-approval-svc first, then AP / AR / PO) | workflow-svc does not own the subject, so it only learns of a material change if the owner calls `/invalidate` or emits a change event it consumes (Q-4). invoice-approval-svc does not even start workflows correctly yet (N-3) |
| H-6 | 20 | R-INV-09 relationship independence (same reporting line, same team) | **org-structure / HR source** (e.g. orgstructure-svc), via authorization-svc | Needs manager / team relationship data that workflow-svc does not hold; authorization-svc's own-object layer covers only "preparer = approver" |
| H-7 | 17, 18 | Calling authorization-svc once its evaluation gate is set to `require` | **mtls-management-svc** + authorization-svc config | Compose runs `AUTHZ_EVALUATION_MTLS=permit` with an empty allow-list, so the plaintext call works today. Under `require`, workflow-svc needs a client certificate and a place on `AUTHZ_PEP_ALLOWLIST`. Not a gap yet; deploy note |

##### B. Needs a decision (no service dependency)

| ID | Rows | Question |
|---|---|---|
| Q-1 (C1) | 9, 10, 13, 14, 26, 27, 30, 58, 59 | Is workflow-svc WFC-02 + WFC-03, or only the Doc 03 §8.4 v1? Decides whether RequestChanges, Expire, definitions-driven routes, source inputs, timers and the split state model are defects or roadmap |
| Q-2 (C4) | 12 | Which API surface binds (Doc 03 capabilities, R-001 §10.1 paths, GOV-06 `/internal/v1/gov06/*`) |
| Q-3 (C3) | 41, 42, 45 | Event catalogue (dotted, PascalCase or snake_case), incl. a separate cancellation event and `workflow.approval.invalidated` |
| Q-4 | 48, 11 | What workflow-svc does on `authorization.denied`, `policy.updated`, `authority.delegated` (and on a subject-changed event). The producers exist now; only the behaviour is missing |
| Q-5 | 15, 16 | R-INV-08: how to tell a service account from a human approver |
| Q-6 | 19 | One `WORKFLOW_APPROVE`, or an action code per workflow type |

##### C. Fixable now in this service (no dependency)

| ID | Rows | Fix |
|---|---|---|
| L-1 | 33 | Replay comparison inside the tenant context; drop or narrow 000003's correlation unique index now that the key exists; compare quorum pools as sets |
| L-2 | 32, 24 | Quorum path: lock the stage (`FOR UPDATE`) or check `row_version` on stage and instance, bump `row_version`; honour `If-Match` / `expected_version` |
| L-3 | 23 | Delegation through authorization-svc's existing `POST /v1/delegated-access/evaluate` (its `basis` names the delegator; `held_directly` stops the delegator's own grant passing as the delegate's). A stranger then gets 403 again, not 503 |
| L-4 | 28, 2 | Keep `current_stage` on escalate so the stage stays actionable; escalation target / owner per Q-1 |
| L-5 | 39, 40, 38 | Ledger wire shape and full envelope through an in-transaction outbox (as policy-svc); record quorum votes; store `access_decision_id`; return `evidence_id`; rationale required on REJECT and kept on the transition |
| L-6 | 36 | Append-only trigger (or REVOKE UPDATE, DELETE) on `workflow_transitions` and quorum decisions |
| L-7 | 7, 8 | ListPendingApprovals and GetApprovalEvidence (transitions + decisions) reads |
| L-8 | 52, 13 | RLS on stages, transitions, quorum tables, outbox and definitions (outbox relay needs a policy that lets it read across tenants) |
| L-9 | 53, 54, 55, 34 | Body vs `X-Legal-Entity-Id`; UUID checks on every route and header; one error helper; stop generating correlation ids |
| L-10 | 47, 46, 35, 61 | Kafka key = aggregate id; envelope fields; true `from_state`; write `created_by` |
| L-11 | N-1, N-2 | Already-INVALIDATED skips the edge check; authorize VerifyRelease |

##### Suggested order

1. L-1, L-2, L-3 (data integrity: retries, races, delegation, all proven live).
2. L-5 + H-3 (evidence: nothing reaches the ledger today).
3. H-1, H-2 in access-control-svc (so the lifecycle and approve permissions can be granted).
4. L-4, L-6, L-8, L-11; then the cosmetic L-9 / L-10.
5. Q-1 … Q-6, then the scope-dependent rows.

---

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3670–3732 (“Workflow completion pass, 21 October 2026 (uncommitted)”)

#### Workflow completion pass, 21 October 2026 (uncommitted)

This pass addresses the implementation gap behind workflow-svc's eight open ❌ rows: Expire,
delegation, deadline timers, durable stuck-instance recovery, Idempotency-Key semantics,
decision-log writes, evidence lineage/authority snapshot and consumed events. It re-scores the workflow rows below; the full suite and a real-Postgres run cover this
tree, so row status is now verified.

##### What changed

- **Deadline/escalation/recheck timers.** `internal/timers` sweeps instances that are due,
  have a pending `next_action_at`, or carry `approver_recheck_at`, under
  `app.timer_sweep`. Migration `000020_completion` adds `review_cycle`, `escalated_at`,
  `approver_recheck_at`, the due/next-action indexes and the `workflow_timer_sweep_select`
  RLS policy. Transitions re-enter the ordinary tenant-scoped lifecycle methods.
- **Expire and durable recovery.** The sweeper's deadline path calls the store's expiry
  transition (`PENDING`/`ESCALATED` → `EXPIRED`, `workflow.expired`); `EscalateToWorkflow`
  replaces the escalation dead end, and a failed timer action remains due for the next
  pass.
- **Delegation.** `authz.CheckDelegation` asks authorization-svc for `WORKFLOW_APPROVE`
  with the named approver as delegator and returns `delegation_id`; the stage records that
  authority instead of matching the approver as a substring of the decision basis.
- **Idempotency.** Decision, lifecycle and escalate paths take `Idempotency-Key` plus
  request hash; replay returns the recorded command, mismatch returns 409. Stage
  comparison no longer depends on an RLS-hidden read.
- **Decision-log writes.** The decision transaction writes the governance ledger record to
  the outbox with destination `decision_log`; `internal/decisionlog.LedgerDeliverer` posts
  it with the configured ledger principal. A second relay drains that destination, so Kafka
  and the ledger no longer share one delivery loop; a committed action response now returns
  the ledger id as `decision_id` (row 39's evidence id).
- **Lineage and authority.** Decisions, lifecycle commands and outbox events carry
  `access_decision_id`, `authority_basis` and `delegation_id`; `000020` stores them on the
  decision and transition rows. `CheckAllowedWithAuthority` gives cancel, escalate,
  escalate-to, invalidate and request-changes the same snapshot approvals already had.
- **Consumed events.** `internal/consumer` consumes policy, delegated-authority and
  authorization events, claims each event id in `workflow_consumed_events`, and flags the
  affected approvers/tenant for re-check. A failed apply releases the claim for redelivery.
- **Definition routing.** `CreateWorkflow` now expands a supplied definition's stages
  before validation (previously it validated the definition and then ignored its stages),
  takes `expiry_hours` as the instance due date and the definition's escalation target as
  the default.
- **Cross-domain identity.** workflow-svc now mounts `identity-envelope`'s gate:
  when a signed envelope is present, its claims override the gateway headers, the session
  is introspected, and `idenv.Forward` passes the verified identity to authorization-svc,
  tenant-svc and the ledger. Compose sets `IDENTITY_VERIFY=warn` locally; K8s sets the
  in-cluster `identity-svc` JWKS/introspection URLs.
- **Caller bundle correctness.** `CreateWorkflow` already refuses a foreign tenant; the
  definition expansion now happens after that refusal so a definition cannot be used to
  inject another tenant's route. Registry status responses were checked against
  `GET /v1/entities/{id}/status` (`entity_id`, `tenant_id`, `entity_status`).
- **Deployment wiring.** Compose: root-context image build, `TENANT_ENTITY_REGISTRY_SERVICE_URL`,
  `WORKFLOW_LEDGER_PRINCIPAL_ID`, tenant-svc health dependency, identity URLs. K8s:
  governance ledger, tenant registry, ledger principal and identity URLs; NetworkPolicy
  egress now permits workflow → tenant-svc:8081, identity-svc:8080, governance-svc:8083
  and document-vault-svc:8094; obligations/evidence policies and envs were corrected for
  the same G1 dependencies, and evidence-requirements-svc received the missing policy
  (its namespace default-deny previously blocked Postgres and Kafka too). policy-svc and
  authorization-svc also received the in-cluster identity-svc JWKS URL and the matching
  egress rule, instead of the compose-only `identity-context-svc` short name.
- **Dockerfile.** `services/workflow-svc/Dockerfile` now builds from the repository root so
  the `replace zoiko.io/identity-envelope => ../identity-envelope` directive resolves;
  `Dockerfile.dockerignore` keeps the root context to the two modules. This is the same
  F-4 pattern used by identity-context-svc and search-indexer-svc.

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3763–3779 (“Remaining open items after this pass”)

#### Remaining open items after this pass

- **9 FAIL remain:** evidence-requirements 5 (real document-vault verification, distinct
  artifact double-count, gate coverage for GL-post/close-lock/vat-gst/corporate-tax, caller
  envelope, caller allow-list), obligations 2 (single owner of obligation truth and idempotent
  applicability decisions), jurisdiction 2 (`validate-action`, regulatory feed). None was touched.
- **Workflow warning:** tenantless global `policy.*` events are skipped; a global policy
  change does not re-check every tenant. `WORKFLOW_LEDGER_PRINCIPAL_ID` is required outside
  `ENV=local` and defaults empty; local runs leave the ledger relay retrying.
- **Test coverage still thin on new paths:** ROLE stages, definition expansion,
  escalation-target approval, registry 404/503, Idempotency-Key replay/mismatch at the
  handler layer, and identity-gate override do not have dedicated handler tests (the store
  and consumer paths are covered).
- X3 (real brokers), X5 (signed identity everywhere), X9 (RFC 9457), C1, C3, C4, C8 remain.

---

### E10 · identity-context-svc items in this audit (4 Nov)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3780–3820 (“identity-context-svc items in this audit, status at 4 November 2026”)

#### identity-context-svc items in this audit, status at 4 November 2026

identity-context-svc is a Group 1 service. This audit tracks the parts of it that authorization-svc
depends on: the 14 Oct sign-off and dependency register, and N-05. The service was re-audited on 4 Nov.
The findings (K-1 … K-7) and its full gap register are in
`Identity, Scope & Foundation-audit-2026-09-23.md`, in the sections "identity-context-svc re-audit,
4 November 2026" and "identity-context-svc gap register, 4 November 2026".

**No Group 2 score changes.** K-1 and K-2 leave a revoked or suspended role in the envelope's
`role_profile`. authorization-svc decides from its own assignments, not from that claim, so its
85 rows stand.

##### The 14 Oct sign-off and dependency register: identity-context rows

| ID | 14 Oct item | 4 Nov status |
|---|---|---|
| F-2 | MFA UI: enrolment, step-up prompt, admin reset | **Closed 3 Nov.** The console has a Sign-in security page (passkey and authenticator app) and offers a passkey in the step-up dialog (Identity audit N-9) |
| F-3 | The console logs in with its client; then `IDP_CLIENT_AUTH=required` | **Half done.** Compose has been `required` since 3 Nov (N-5). The console sends its client only when `IDENTITY_CONSOLE_CLIENT_ID` / `_SECRET` are set, and its `.env.example` leaves them empty (K-5). If identity-context-svc then refuses the login, the console signs the person in from its own credential list (K-4) |
| F-1 | The console forwards the envelope on every authorization-svc call; then compose → `AUTHZ_IDENTITY_ASSERTION=require` | **Open.** Compose is still `warn`. Until it is `require`, a console-only session from K-4, which sends `X-Principal-Id` / `X-Tenant-Id` and no envelope, is admitted by authorization-svc in compose |
| SO-1 | Security approves TOTP as the interim high-assurance factor; WebAuthn follows | **Open, wider.** WebAuthn passkeys exist since 3 Nov: a passkey step-up gives `assurance_level: PHISHING_RESISTANT`. The approval now also covers passkey attestation `none`, with no authenticator allow-list (Identity register S1-D2) |
| P-1 | Delete the stale `docker-compose.override.yml` | **Closed.** `feec479b` (3 Nov) removed it |
| R-3 | Every other service verifies the envelope | **Open.** 20 of 122 service folders reference `zoiko.io/identity-envelope`, and a reference is not proof of a mounted gate |
| X-3 | PEPs forward `X-Authn-Age-Seconds`; then `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up` | **Open.** Compose is still `permit`. identity-context-svc already puts `auth_time` on every envelope |

##### N-05: principal status ordering

**Holds.** identity-context-svc sends `principal.status.changed` (with `identity.subject.*`) from
the status transaction, stamped with envelope `emitted_at` and no payload `status_changed_at`.
authorization-svc orders by `emitted_at` when the payload has no timestamp
(`internal/events/lifecycle_consumer.go:557-566`).

##### New since 14 Oct, for authorization-svc to know

- **Suspension in the role projection.** identity-context-svc consumes authorization-svc's
  `iam.assignment.suspended` / `.resumed` (000037 / 000041), ends the subject's sessions, and
  holds the role out of the envelope. The two defects K-1 and K-2 are on the identity side. No
  authorization-svc change is needed, except that a projection resync (the fix for S1-9) would
  read authorization-svc's assignments.
- **Live proof.** The suspension handling has not run against a real broker. The 3 Nov live E2E
  used an image built before the merge.

### E11 · evidence-requirements-svc completion (5 Nov)

> Source: `Governance Platform-audit-2026-09-28.md`, lines 3821–3882 (“evidence-requirements-svc completion, 5 November 2026”)

#### evidence-requirements-svc completion, 5 November 2026

Plan: `EVIDENCE-REQUIREMENTS-PLAN-2026-11-04.md` (ER-0 … ER-9, ER-FE-1/2). A first execution the same
day reported every task done; the verifier protocol found most of it was not (named tests that did
not exist, an end-date that failed on Postgres, an If-Match path that could never match, a SQL
injection in list `sort`, no image, no E2E run, no console). This section scores the re-done pass.
Evidence: `services/evidence-requirements-svc/progress.md` "Verification evidence"; live
`scripts/audit.sh` 42/42 and `deployments/scripts/e2e-evidence-requirements-deps.py --with-outage`
84/84 (3 owner-blocked skips, never counted) on the rebuilt image; suites handler 72, store 24 /
35 (integration), events 5, manifest 3, esignature 3, registry 1, telemetry 1 — 0 fail, 0 skip.

**Score: 20 → 39 ✅, 18 → 1 ⚠️, 5 → 3 ❌, 5 ❓ of 43 — 90.7% full, 91.9% weighted.**

Rows that moved (file:line at 5 Nov):

| Row | 19 Oct | 5 Nov | Evidence |
|---|---|---|---|
| Owns document requirements, and verifies document evidence | ❌ | ✅ | `handler.go:516` checkArtifacts → document-vault with the envelope forwarded; live E2E §6: a vault document SATISFIED, a random id MISSING with the reason, another entity's document MISSING, vault stopped → 503 and the ledger count unchanged |
| Sufficiency counts distinct artifacts | ❌ | ✅ | canonical key `artifactKey`; `TestER02_SameManifestTwiceCountsOnce`, `TestEvaluate_TheSameDocumentInAnotherCaseCountsOnce`; live E2E §6 (one document twice vs minimum 2 → MISSING); mutation check |
| Owns signature requirements | ⚠️ | ✅ | `internal/esignature/client.go` (existence in tenant + entity); `TestER02_SignatureUnknownIsUnmet`, `TestER02_SignatureServiceDownIs503NoRow`, `TestVerifyEnvelope*`; live: owner unreachable → 503 `esignature_unavailable`, no row. A real envelope could not be shown live (phase7 stack) |
| Non-document artifacts | ⚠️ | ✅ | an empty reference never counts (`TestER02_EmptyReferenceNeverCounts`); EVIDENCE_MANIFEST verified (`TestER02_Manifest*`); unconfigured owner = 503 (`TestER02_UnconfiguredOwnerNeverCounts`) |
| Event envelope (causation, jurisdiction, governing refs) | ⚠️ | ✅ | `handler.go:429`, `outbox/worker.go:114-122`; asyncapi.yaml 1.1.0 rewritten from a real record; live E2E §9 |
| tenant_id resolved by GOV-01, never trusted from an arbitrary header | ⚠️ | ✅ | compose route through `gateway-auth@file` (D-ER-28); live E2E §3: forged X-Tenant-Id / X-Principal-Id next to an envelope are replaced, the ledger row carries the verified ones |
| Server-resolved context (tenant and entity operable) | ⚠️ | ✅ | `handler.go:236` registry check; live E2E §5 (foreign / random entity 422, no ledger row) |
| A missing tenant gets 401 | ⚠️ | ✅ | `handler.go:1140`; `TestEvaluate_MissingTenant_Returns401`; audit.sh §4 |
| Workload identity accepted | ⚠️ | ✅ | `handler.go:1159`; `TestEvaluate_AVerifiedPeerWorkloadIsTheCaller` |
| Stable error classes | ⚠️ | ✅ | RFC 9457 via apicontract; `TestAPI2_ErrorResponsesAreProblemDetails`; owner-specific 503 codes (`handler.go:1083`) |
| Effective interval validity | ⚠️ | ✅ | `handler.go:868`, `handler.go:1015` |
| Requirement payload validated when authored | ⚠️ | ✅ | `handler.go:939`; `TestCreateRequirement_PayloadMustMatchTheRuleShape` |
| Evaluation ledger is append-only / immutable | ⚠️ | ✅ | migrations 000007 (UPDATE/DELETE trigger), 000009 (TRUNCATE); `TestPgStore_EvaluationLedgerIsAppendOnly` |
| Decision is reproducible (governing versions retained) | ⚠️ | ✅ | `handler.go:410` requirement_ids stored and published as governing_refs; console drawer "Tested against" |
| Legal entity is validated by the server | ⚠️ | ✅ | registry (`TENANT_REGISTRY_URL`, required outside local); `TestEvaluate_LegalEntityMustExistInTheTenant`; live E2E §5 |
| API endpoints specified in the primary spec | ⚠️ | ✅ | openapi.yaml 1.4.0; `TestOpenAPIMatchesTheRouter`, `TestOpenAPIDocumentIsValid` |
| context.md §4 list semantics | ⚠️ | ✅ | context.md §4 / §9 / §12 refreshed 5 Nov |
| context and progress claims vs the estate | ⚠️ | ✅ | progress.md rewritten; every test it names resolves (`grep -rn "func <Name>("`) |
| Postman collection is usable | ⚠️ | ✅ | regenerated from openapi.yaml: full envelope, Idempotency-Key, If-Match |

Still ⚠️ (1): **Read contracts are strongly scoped** — the lists and the requirement / evaluation reads are
authorized at the entity, but `GET /v1/pbc/requests/{id}` (`pbc.go:145`) still checks the tenant only.

Still ❌ (3), all on the callers' side and owned by their teams: no gate in general-ledger post,
financial-close lock, vat-gst or corporate-tax; board-resolutions, corporate-actions and
filing-preparation send an incomplete envelope; corporate-actions and filing-preparation pass any
outcome other than MISSING (fail open).

Defects found live in this pass, not visible to unit tests: the outbox refused every event under the
real runtime role (000008's `FOR ALL USING (false)`; fixed by 000011, `TestOutbox_AppRoleAppendsButCannotRead`);
compose connected as a role with no CONNECT on the database; the ER-5 Traefik route sat on an entrypoint
that redirects before routing and had no ForwardAuth; access-control 000046 claimed to register six actions
000008 already registered, and its down migration deleted them (rewritten; `TestR5_EvidenceActionsRegistered`,
`TestR5_EvidenceTemplateGrantsEveryAction`, access-control store suite 47/47).

Blocked outside this service: evidence-manifest-svc authorizes every read on `EVIDENCE_MANIFEST_READ`,
which access-control does not register, so no manifest can count yet (references fail closed);
esignature-integration-svc runs only in the phase7 stack; document-vault refuses uploads without clamd,
which compose does not run; document-vault and gateway-auth-svc each have a compose defect (DB role;
admin-token volume mounted where a file is expected).

| Service | ✅ | ⚠️ | ❌ | ❓ | Scored | Full | Weighted | Last live |
|---|---|---|---|---|---|---|---|---|
| evidence-requirements-svc | 39 | 1 | 3 | 5 | 43 | 90.7% | 91.9% | 5 Nov: audit 42/42, E2E 84/84 |
| **Group 2 total** | **362** | **59** | **15** | **17** | **436** | **83.0%** | **89.8%** | |

---

## Part F · Implementation decisions, cross-team follow-ups, dormant triggers

### F1 · From IMPLEMENTATION-PROGRESS

> Source: `IMPLEMENTATION-PROGRESS.md`, lines 250–274 (“Decisions taken during implementation”)

#### Decisions taken during implementation

| Date | Task | Decision | Why |
|---|---|---|---|
| 2026-10-23 | P0-2 | Created `JURISDICTION_COMPLIANCE_OFFICER` (TENANT, v1) carrying `JURISDICTION_SUPPORT_VIEW` + `JURISDICTION_RULE_VALIDATE` instead of extending an existing template | The plan assumes a template already carries the other `JURISDICTION_*` read actions; none did (queried `role_template_versions` in a fresh DB: 0 rows matching `JURISDICTION%`). Adding the validation POST to `VIEWER_AUDITOR_READ_ONLY` would put a verb into the read-only archetype; a dedicated tenant-scope template is the closest safe fit. |
| 2026-10-27 | API-2 | Envelope refusals are rewritten by the shared `apicontract.EnvelopeRefusals` rather than by editing each vendored `internal/envelope/middleware.go` | The vendored copy comes from `services/_contract`, and a rollout would overwrite local edits. Changing the generator would touch every teammate's service (R1). |
| 2026-10-28 | API-2 | policy-svc keeps `code` = the §16 class and adds the specific refusal as `reason_code` | `code` was already the class before API-2 (`{"error": specific, "code": class}`), and callers and tests match on it. Swapping it to the specific code would break them, and dropping the specific code lost information. |
| 2026-10-28 | API-2 | A writer that names no code uses `apicontract.CodeForStatus(status)` | The S&T four never had codes. A status-derived code is stable and spec-conformant. A blanket `internal_error` on a 403 was not. |
| 2026-10-28 | API-2 | workflow `verify-release` 409 stays `application/json` | It is the release decision (`can_release: false`), like jurisdiction's validate-action 409. Recorded in workflow-svc `SPEC_DEVIATIONS.md`. |
| 2026-10-27 | API-2 | Exempt from problem+json: `/healthz`, `/readyz`, `/health` probe bodies; identity-context `/oauth2/*`, `/scim/v2/*`, `/.well-known/*`; jurisdiction `POST /v1/validate-action` 409 | Protocol-mandated formats (RFC 6749/6750, RFC 7644) and a 409 that is a decision result, not an error report. Recorded in each service's `SPEC_DEVIATIONS.md` (R15). |

#### Cross-team follow-ups (changes needed in services we do not own)

| Service | Change needed | Needed by task |
|---|---|---|
| document-vault-svc | Consume `legal_hold.*` from retention-registry instead of owning holds | G-10 |
| obligation-tracking-svc | Overlaps obligations-svc (single owner C8) | WF-3 |
| external-data-feed-svc | Regulatory source feed for jurisdiction packs | JUR-1 |
| corporate-actions, filing-preparation, general-ledger, financial-close, vat-gst, corporate-tax | Forward the envelope and call evidence-requirements before governed actions | WF-4 |

#### Dormant email triggers (no producer yet)

| Trigger | Template | Service that should produce it |
|---|---|---|

---

## Part G · Re-score log

One dated entry per verifier pass that re-scores rows: service, rows re-scored (old → new), new score, evidence (commit, test, E2E output). Newest last.

| Date | Service | Rows re-scored | New score | Evidence |
|---|---|---|---|---|
| 6 Nov 2026 | (all) | None: file built from the six sources | Latest scores are in D2 (Domain 1) and E2 (Domain 2; later passes in E9 and E11) | The `Source:` lines in this file |

---

# Appendix A · Route inventory (6 Nov 2026)

Every route registered in each service's router, extracted from the code, with the feature row that covers it. Health, readiness and metrics endpoints are left out. A route added later must get a row here and in its service's feature table.

## D1-9 · access-control-svc (50 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/access-review-campaigns` | D1-9.F09 |
| POST | `/v1/access-review-campaigns` | D1-9.F09 |
| GET | `/v1/access-review-campaigns/{campaign_id}` | D1-9.F09 |
| POST | `/v1/access-review-campaigns/{campaign_id}/complete` | D1-9.F09 |
| POST | `/v1/access-review-campaigns/{campaign_id}/items/{item_id}/decide` | D1-9.F09 |
| POST | `/v1/access-review-campaigns/{campaign_id}/items/{item_id}/reassign` | D1-9.F09 |
| GET | `/v1/iam/access-assignments` | D1-9.F07 |
| POST | `/v1/iam/access-assignments` | D1-9.F07 |
| GET | `/v1/iam/access-assignments/{request_id}` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}/approve` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}/reject` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}/cancel` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}/revoke` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}:approve` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}:reject` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}:cancel` | D1-9.F07 |
| POST | `/v1/iam/access-assignments/{request_id}:revoke` | D1-9.F07 |
| GET | `/v1/iam/access-reviews` | D1-9.F09 |
| GET | `/v1/iam/federation/role-eligibility` | D1-9.F10 |
| GET | `/v1/iam/group-assignments/{group_assignment_id}` | D1-9.F08 |
| POST | `/v1/iam/group-assignments/{group_assignment_id}/revoke` | D1-9.F08 |
| POST | `/v1/iam/group-assignments/{group_assignment_id}/sync` | D1-9.F08 |
| POST | `/v1/iam/group-assignments/{group_assignment_id}:revoke` | D1-9.F08 |
| POST | `/v1/iam/group-assignments/{group_assignment_id}:sync` | D1-9.F08 |
| GET | `/v1/iam/groups` | D1-9.F08 |
| POST | `/v1/iam/groups` | D1-9.F08 |
| GET | `/v1/iam/groups/{group_id}` | D1-9.F08 |
| GET | `/v1/iam/groups/{group_id}/assignments` | D1-9.F08 |
| POST | `/v1/iam/groups/{group_id}/assignments` | D1-9.F08 |
| POST | `/v1/iam/groups/{group_id}/members` | D1-9.F08 |
| POST | `/v1/iam/groups/{group_id}/members/{principal_id}/remove` | D1-9.F08 |
| POST | `/v1/iam/groups/{group_id}/members/{principal_id}:remove` | D1-9.F08 |
| GET | `/v1/iam/subject-links` | D1-9.F09 |
| POST | `/v1/iam/subject-links` | D1-9.F09 |
| GET | `/v1/permission-bundles` | D1-9.F02 |
| GET | `/v1/permission-definitions` | D1-9.F03 |
| GET | `/v1/role-definitions` | D1-9.F01 |
| POST | `/v1/role-definitions` | D1-9.F01 |
| GET | `/v1/role-definitions/{role_definition_id}` | D1-9.F01 |
| PATCH | `/v1/role-definitions/{role_definition_id}` | D1-9.F01 |
| GET | `/v1/role-definitions/{role_definition_id}/permission-bundles` | D1-9.F02 |
| POST | `/v1/role-definitions/{role_definition_id}/permission-bundles` | D1-9.F02 |
| DELETE | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` | D1-9.F02 |
| GET | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` | D1-9.F02 |
| PATCH | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` | D1-9.F02 |
| GET | `/v1/role-templates` | D1-9.F06 |
| GET | `/v1/role-templates/{template_code}` | D1-9.F06 |
| POST | `/v1/role-templates/{template_code}/instantiate` | D1-9.F06 |
| POST | `/v1/role-templates/{template_code}/upgrade` | D1-9.F06 |
| GET | `/v1/role-templates/{template_code}/versions` | D1-9.F06 |

## D2-5 · authorization-svc (98 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/admin/v1/break-glass-sessions` | D2-5.F09 |
| POST | `/admin/v1/break-glass-sessions` | D2-5.F09 |
| GET | `/admin/v1/break-glass-sessions/{session_id}` | D2-5.F09 |
| GET | `/admin/v1/break-glass-sessions/{session_id}/activity` | D2-5.F09 |
| POST | `/admin/v1/break-glass-sessions/{session_id}/approve` | D2-5.F09 |
| POST | `/admin/v1/break-glass-sessions/{session_id}/reject` | D2-5.F09 |
| POST | `/admin/v1/break-glass-sessions/{session_id}/review` | D2-5.F09 |
| POST | `/admin/v1/break-glass-sessions/{session_id}/revoke` | D2-5.F09 |
| GET | `/admin/v1/privileged-sessions` | D2-5.F09 |
| POST | `/admin/v1/privileged-sessions` | D2-5.F09 |
| GET | `/admin/v1/privileged-sessions/{session_id}/activity` | D2-5.F09 |
| POST | `/admin/v1/privileged-sessions/{session_id}/approve` | D2-5.F09 |
| POST | `/admin/v1/privileged-sessions/{session_id}/reject` | D2-5.F09 |
| POST | `/admin/v1/privileged-sessions/{session_id}/review` | D2-5.F09 |
| POST | `/admin/v1/privileged-sessions/{session_id}/revoke` | D2-5.F09 |
| POST | `/internal/authorization/decisions` | D2-5.F10 |
| POST | `/internal/v1/gov03/commands/invalidateAuthorizationCache` | D2-5.F12 |
| POST | `/internal/v1/gov03/commands/recomputeSubjectEffectiveAccess` | D2-5.F12 |
| GET | `/v1/access-decisions` | D2-5.F10 |
| GET | `/v1/access-decisions/{access_decision_id}` | D2-5.F10 |
| GET | `/v1/admin/abac-rules` | D2-5.F05 |
| POST | `/v1/admin/abac-rules` | D2-5.F05 |
| POST | `/v1/admin/abac-rules/{abac_rule_id}/approve` | D2-5.F05 |
| POST | `/v1/admin/abac-rules/{abac_rule_id}/reactivate` | D2-5.F05 |
| POST | `/v1/admin/abac-rules/{abac_rule_id}/reject` | D2-5.F05 |
| POST | `/v1/admin/abac-rules/{abac_rule_id}/retire` | D2-5.F05 |
| GET | `/v1/admin/authority-limits` | D2-5.F06 |
| POST | `/v1/admin/authority-limits` | D2-5.F06 |
| POST | `/v1/admin/authority-limits/{authority_limit_id}/approve` | D2-5.F06 |
| POST | `/v1/admin/authority-limits/{authority_limit_id}/change` | D2-5.F06 |
| POST | `/v1/admin/authority-limits/{authority_limit_id}/reject` | D2-5.F06 |
| POST | `/v1/admin/authority-limits/{authority_limit_id}/retire` | D2-5.F06 |
| POST | `/v1/admin/authorization-cache/invalidate` | D2-5.F12 |
| GET | `/v1/admin/break-glass-sessions` | D2-5.F09 |
| POST | `/v1/admin/break-glass-sessions` | D2-5.F09 |
| GET | `/v1/admin/break-glass-sessions/{session_id}` | D2-5.F09 |
| GET | `/v1/admin/break-glass-sessions/{session_id}/activity` | D2-5.F09 |
| POST | `/v1/admin/break-glass-sessions/{session_id}/approve` | D2-5.F09 |
| POST | `/v1/admin/break-glass-sessions/{session_id}/reject` | D2-5.F09 |
| POST | `/v1/admin/break-glass-sessions/{session_id}/review` | D2-5.F09 |
| POST | `/v1/admin/break-glass-sessions/{session_id}/revoke` | D2-5.F09 |
| GET | `/v1/admin/delegated-authorities` | D2-5.F08 |
| POST | `/v1/admin/delegated-authorities` | D2-5.F08 |
| POST | `/v1/admin/delegated-authorities/{delegation_id}/revoke` | D2-5.F08 |
| POST | `/v1/admin/permission-bundles/{permission_bundle_id}/reactivate` | D2-5.F03 |
| POST | `/v1/admin/permission-bundles/{permission_bundle_id}/retire` | D2-5.F03 |
| GET | `/v1/admin/privileged-sessions` | D2-5.F09 |
| POST | `/v1/admin/privileged-sessions` | D2-5.F09 |
| GET | `/v1/admin/privileged-sessions/{session_id}/activity` | D2-5.F09 |
| POST | `/v1/admin/privileged-sessions/{session_id}/approve` | D2-5.F09 |
| POST | `/v1/admin/privileged-sessions/{session_id}/reject` | D2-5.F09 |
| POST | `/v1/admin/privileged-sessions/{session_id}/review` | D2-5.F09 |
| POST | `/v1/admin/privileged-sessions/{session_id}/revoke` | D2-5.F09 |
| GET | `/v1/admin/role-assignments` | D2-5.F04 |
| POST | `/v1/admin/role-assignments` | D2-5.F04 |
| GET | `/v1/admin/role-assignments/as-of` | D2-5.F04 |
| POST | `/v1/admin/role-assignments/{assignment_id}/approve` | D2-5.F04 |
| POST | `/v1/admin/role-assignments/{assignment_id}/reject` | D2-5.F04 |
| POST | `/v1/admin/role-assignments/{assignment_id}/resume` | D2-5.F04 |
| POST | `/v1/admin/role-assignments/{assignment_id}/revoke` | D2-5.F04 |
| POST | `/v1/admin/role-assignments/{assignment_id}/suspend` | D2-5.F04 |
| GET | `/v1/admin/role-assignments/{assignment_id}/suspensions` | D2-5.F04 |
| GET | `/v1/admin/roles` | D2-5.F03 |
| POST | `/v1/admin/roles` | D2-5.F03 |
| GET | `/v1/admin/roles/{role_id}/permission-bundles` | D2-5.F03 |
| POST | `/v1/admin/roles/{role_id}/permission-bundles` | D2-5.F03 |
| POST | `/v1/admin/roles/{role_id}/reactivate` | D2-5.F03 |
| POST | `/v1/admin/roles/{role_id}/retire` | D2-5.F03 |
| GET | `/v1/admin/sod-exceptions` | D2-5.F02 |
| POST | `/v1/admin/sod-exceptions` | D2-5.F02 |
| POST | `/v1/admin/sod-exceptions/{sod_exception_id}/approve` | D2-5.F02 |
| POST | `/v1/admin/sod-exceptions/{sod_exception_id}/reject` | D2-5.F02 |
| POST | `/v1/admin/sod-exceptions/{sod_exception_id}/revoke` | D2-5.F02 |
| GET | `/v1/admin/sod-rules` | D2-5.F02 |
| POST | `/v1/admin/sod-rules` | D2-5.F02 |
| POST | `/v1/admin/sod-rules/{sod_rule_id}/approve` | D2-5.F02 |
| POST | `/v1/admin/sod-rules/{sod_rule_id}/reactivate` | D2-5.F02 |
| POST | `/v1/admin/sod-rules/{sod_rule_id}/reject` | D2-5.F02 |
| POST | `/v1/admin/sod-rules/{sod_rule_id}/retire` | D2-5.F02 |
| POST | `/v1/admin/subjects/{principal_id}/effective-access/recompute` | D2-5.F12 |
| GET | `/v1/admin/workload-bindings` | D2-5.F07 |
| POST | `/v1/admin/workload-bindings` | D2-5.F07 |
| POST | `/v1/admin/workload-bindings/{workload_id}/deactivate` | D2-5.F07 |
| POST | `/v1/admin/workload-bindings/{workload_id}/reactivate` | D2-5.F07 |
| POST | `/v1/authorize` | D2-5.F01 |
| POST | `/v1/delegated-access/evaluate` | D2-5.F08 |
| POST | `/v1/entity-scope/validate` | D2-5.F11 |
| GET | `/v1/me/capabilities` | D2-5.F11 |
| GET | `/v1/sod/conflicting-permissions` | D2-5.F02 |
| POST | `/v1/sod/evaluate` | D2-5.F02 |
| POST | `/v1/sod/validate` | D2-5.F02 |
| GET | `/v1/support/sessions` | D2-5.F09 |
| POST | `/v1/support/sessions` | D2-5.F09 |
| GET | `/v1/support/sessions/{session_id}` | D2-5.F09 |
| GET | `/v1/support/sessions/{session_id}/activity` | D2-5.F09 |
| POST | `/v1/support/sessions/{session_id}/revoke` | D2-5.F09 |
| GET | `/v1/{resource_type}/{resource_id}/available-actions` | D2-5.F11 |
| GET | `/v1/{resource}/{id}/available-actions` | D2-5.F11 |

## D1-12 · carta-svc (3 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/carta/assessments` | D1-12.F02 |
| GET | `/v1/carta/assessments/{id}` | D1-12.F02 |
| POST | `/v1/carta/evaluate` | D1-12.F01 |

## D1-3 · configuration-feature-flag-svc (27 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/config` | D1-3.F14 |
| POST | `/v1/config` | D1-3.F14 |
| POST | `/v1/config/changes` | D1-3.F08 |
| GET | `/v1/config/changes/{change_id}` | D1-3.F08 |
| POST | `/v1/config/changes/{change_id}/activate` | D1-3.F08 |
| POST | `/v1/config/changes/{change_id}/approve` | D1-3.F08 |
| POST | `/v1/config/changes/{change_id}/rollback` | D1-3.F08 |
| POST | `/v1/config/definitions` | D1-3.F01 |
| GET | `/v1/config/definitions/{key}` | D1-3.F01 |
| POST | `/v1/config/definitions/{key}/publish` | D1-3.F02 |
| PUT | `/v1/config/overrides/{scope}` | D1-3.F03 |
| POST | `/v1/config/resolve` | D1-3.F04 |
| GET | `/v1/config/snapshots/{snapshot_id}` | D1-3.F05 |
| GET | `/v1/config/{key}` | D1-3.F14 |
| POST | `/v1/emergency-changes` | D1-3.F09 |
| GET | `/v1/emergency-changes/{emergency_change_id}` | D1-3.F09 |
| POST | `/v1/emergency-changes/{emergency_change_id}/activate` | D1-3.F09 |
| POST | `/v1/emergency-changes/{emergency_change_id}/retrospective` | D1-3.F09 |
| GET | `/v1/flags` | D1-3.F07 |
| POST | `/v1/flags` | D1-3.F07 |
| GET | `/v1/flags/{key}` | D1-3.F07 |
| POST | `/v1/flags/{key}/evaluate` | D1-3.F07 |
| POST | `/v1/flags/{key}/kill-switch` | D1-3.F07 |
| POST | `/v1/flags/{key}/release-plans` | D1-3.F07 |
| POST | `/v1/flags/{key}/removal` | D1-3.F07 |
| POST | `/v1/flags/{key}/retire` | D1-3.F07 |
| POST | `/v1/runtime/attest` | D1-3.F10 |

## D1-8 · delegated-authority-svc (9 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/delegations` | D1-8.F03 |
| POST | `/v1/delegations` | D1-8.F01 |
| GET | `/v1/delegations/explain` | D1-8.F03 |
| GET | `/v1/delegations/{delegation_id}` | D1-8.F03 |
| POST | `/v1/delegations/{delegation_id}/activate` | D1-8.F02 |
| POST | `/v1/delegations/{delegation_id}/extend` | D1-8.F02 |
| POST | `/v1/delegations/{delegation_id}/resume` | D1-8.F02 |
| POST | `/v1/delegations/{delegation_id}/revoke` | D1-8.F02 |
| POST | `/v1/delegations/{delegation_id}/suspend` | D1-8.F02 |

## D2-7 · evidence-requirements-svc (22 routes)

| Method | Path | Feature |
|---|---|---|
| POST | `/v1/admin/evidence-requirements` | D2-7.F03 |
| POST | `/v1/admin/evidence-requirements/{evidence_requirement_id}/end-date` | D2-7.F03 |
| GET | `/v1/evidence-requirements` | D2-7.F03 |
| GET | `/v1/evidence-requirements/{evidence_requirement_id}` | D2-7.F03 |
| POST | `/v1/evidence/evaluate` | D2-7.F01 |
| GET | `/v1/evidence/evaluations` | D2-7.F02 |
| GET | `/v1/evidence/evaluations/{evaluation_id}` | D2-7.F02 |
| GET | `/v1/evidence/summary` | D2-7.F02 |
| GET | `/v1/pbc/requests` | D2-7.F06 |
| POST | `/v1/pbc/requests` | D2-7.F06 |
| GET | `/v1/pbc/requests/{request_id}` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/clarify` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/close` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/extend-due-date` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/mark-satisfied` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/notes` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/reassign` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/responses` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/send` | D2-7.F06 |
| POST | `/v1/pbc/requests/{request_id}/view` | D2-7.F06 |
| POST | `/v1/pbc/responses/{response_id}/acknowledge` | D2-7.F06 |
| POST | `/v1/pbc/responses/{response_id}/scan-result` | D2-7.F06 |

## D1-5 · gateway-auth-svc (6 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/internal/v1/gateway/decisions/{id}` | D1-5.F11 |
| GET | `/internal/v1/gateway/denied` | D1-5.F11 |
| GET | `/internal/v1/gateway/rate-limit/{scope}/{key}` | D1-5.F11 |
| GET | `/internal/v1/gateway/routes/versions` | D1-5.F11 |
| GET | `/internal/v1/gateway/usage/{tenantID}` | D1-5.F11 |
| ANY | `/verify` | D1-5.F01 |

## D2-2 · governance-decision-log-svc (5 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/decisions` | D2-2.F02 |
| POST | `/v1/decisions` | D2-2.F01 |
| GET | `/v1/decisions/{decision_id}` | D2-2.F02 |
| POST | `/v1/decisions/{decision_id}/replay` | D2-2.F03 |
| GET | `/v1/decisions/{decision_id}/replay-manifests` | D2-2.F03 |

## D1-1 · identity-context-svc (105 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/.well-known/jwks.json` | D1-1.F07 |
| GET | `/.well-known/oauth-authorization-server` | D1-1.F07 |
| GET | `/.well-known/openid-configuration` | D1-1.F07 |
| GET | `/oauth2/authorize` | D1-1.F07 |
| POST | `/oauth2/authorize` | D1-1.F07 |
| POST | `/oauth2/introspect` | D1-1.F07 |
| POST | `/oauth2/login` | D1-1.F07 |
| GET | `/oauth2/login/federate/{idpID}` | D1-1.F16 |
| POST | `/oauth2/login/mfa` | D1-1.F07 |
| POST | `/oauth2/login/passkey/begin` | D1-1.F09 |
| POST | `/oauth2/login/passkey/finish` | D1-1.F09 |
| GET | `/oauth2/logout` | D1-1.F07 |
| POST | `/oauth2/logout` | D1-1.F07 |
| POST | `/oauth2/revoke` | D1-1.F07 |
| POST | `/oauth2/token` | D1-1.F07 |
| GET | `/oauth2/userinfo` | D1-1.F07 |
| POST | `/oauth2/userinfo` | D1-1.F07 |
| GET | `/scim/v2/{tenantID}/{connectionID}/ServiceProviderConfig` | D1-1.F17 |
| GET | `/scim/v2/{tenantID}/{connectionID}/Users` | D1-1.F17 |
| POST | `/scim/v2/{tenantID}/{connectionID}/Users` | D1-1.F17 |
| DELETE | `/scim/v2/{tenantID}/{connectionID}/Users/{id}` | D1-1.F17 |
| GET | `/scim/v2/{tenantID}/{connectionID}/Users/{id}` | D1-1.F17 |
| PATCH | `/scim/v2/{tenantID}/{connectionID}/Users/{id}` | D1-1.F17 |
| PUT | `/scim/v2/{tenantID}/{connectionID}/Users/{id}` | D1-1.F17 |
| POST | `/v1/account/activate` | D1-1.F10 |
| POST | `/v1/account/password` | D1-1.F10 |
| POST | `/v1/account/password/forgot` | D1-1.F10 |
| POST | `/v1/account/password/reset` | D1-1.F10 |
| POST | `/v1/authenticate` | D1-1.F06 |
| POST | `/v1/authenticate/federated` | D1-1.F16 |
| POST | `/v1/authenticate/step-up` | D1-1.F08 |
| POST | `/v1/authenticate/step-up/passkey/begin` | D1-1.F09 |
| POST | `/v1/authenticate/step-up/passkey/finish` | D1-1.F09 |
| GET | `/v1/authentication-events` | D1-1.F13 |
| POST | `/v1/context/cache/refresh` | D1-1.F03 |
| POST | `/v1/context/resolve` | D1-1.F01 |
| POST | `/v1/context/session/introspect` | D1-1.F07 |
| GET | `/v1/context/session/{sessionContextID}` | D1-1.F02 |
| GET | `/v1/context/session/{sessionContextID}/explain` | D1-1.F02 |
| POST | `/v1/context/session/{sessionContextID}/invalidate` | D1-1.F03 |
| GET | `/v1/context/sessions` | D1-1.F04 |
| GET | `/v1/context/support` | D1-1.F05 |
| POST | `/v1/context/support` | D1-1.F05 |
| DELETE | `/v1/context/support/{supportContextID}` | D1-1.F05 |
| GET | `/v1/context/support/{supportContextID}` | D1-1.F05 |
| POST | `/v1/context/support/{supportContextID}/approve` | D1-1.F05 |
| POST | `/v1/context/support/{supportContextID}/review` | D1-1.F05 |
| POST | `/v1/context/tenant/invalidate` | D1-1.F03 |
| GET | `/v1/federation/connections` | D1-1.F16 |
| POST | `/v1/federation/connections` | D1-1.F16 |
| GET | `/v1/federation/connections/{connectionID}` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/keys/refresh` | D1-1.F16 |
| GET | `/v1/federation/connections/{connectionID}/logins` | D1-1.F16 |
| GET | `/v1/federation/connections/{connectionID}/mapping` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/mapping` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/mapping/versions/{version}/approve` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/mapping/versions/{version}/reject` | D1-1.F17 |
| GET | `/v1/federation/connections/{connectionID}/provisioning/deprovision-latency` | D1-1.F17 |
| GET | `/v1/federation/connections/{connectionID}/provisioning/events` | D1-1.F17 |
| GET | `/v1/federation/connections/{connectionID}/provisioning/exceptions` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/provisioning/exceptions/{exceptionID}/resolve` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/provisioning/reconcile` | D1-1.F17 |
| GET | `/v1/federation/connections/{connectionID}/provisioning/status` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/resume` | D1-1.F16 |
| POST | `/v1/federation/connections/{connectionID}/revoke` | D1-1.F16 |
| POST | `/v1/federation/connections/{connectionID}/rotate-metadata` | D1-1.F16 |
| POST | `/v1/federation/connections/{connectionID}/scim-token` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/scim-token/revoke` | D1-1.F17 |
| POST | `/v1/federation/connections/{connectionID}/suspend` | D1-1.F16 |
| POST | `/v1/federation/connections/{connectionID}/versions/{version}/approve` | D1-1.F16 |
| POST | `/v1/federation/connections/{connectionID}/versions/{version}/reject` | D1-1.F16 |
| GET | `/v1/federation/links` | D1-1.F16 |
| POST | `/v1/federation/links` | D1-1.F16 |
| GET | `/v1/federation/links/{linkID}` | D1-1.F16 |
| GET | `/v1/identities/{principalID}` | D1-1.F15 |
| GET | `/v1/identities/{principalID}/assurance-history` | D1-1.F15 |
| GET | `/v1/identities/{principalID}/authentication-events` | D1-1.F15 |
| GET | `/v1/identities/{principalID}/links` | D1-1.F15 |
| POST | `/v1/identities/{principalID}/merge` | D1-1.F15 |
| GET | `/v1/identity/summary` | D1-1.F13 |
| POST | `/v1/logout` | D1-1.F07 |
| GET | `/v1/mfa/factors` | D1-1.F08 |
| GET | `/v1/mfa/passkeys` | D1-1.F09 |
| POST | `/v1/mfa/passkeys/register/begin` | D1-1.F09 |
| POST | `/v1/mfa/passkeys/register/finish` | D1-1.F09 |
| POST | `/v1/mfa/passkeys/revoke` | D1-1.F08 |
| POST | `/v1/mfa/totp/activate` | D1-1.F08 |
| POST | `/v1/mfa/totp/enroll` | D1-1.F08 |
| POST | `/v1/mfa/totp/revoke` | D1-1.F08 |
| GET | `/v1/password-resets` | D1-1.F11 |
| GET | `/v1/password-resets/{resetID}` | D1-1.F11 |
| POST | `/v1/password-resets/{resetID}/decide` | D1-1.F11 |
| GET | `/v1/principals` | D1-1.F12 |
| POST | `/v1/principals` | D1-1.F10 |
| GET | `/v1/principals/{principalID}` | D1-1.F12 |
| GET | `/v1/principals/{principalID}/assurance-history` | D1-1.F13 |
| GET | `/v1/principals/{principalID}/delegations` | D1-1.F12 |
| POST | `/v1/principals/{principalID}/invite` | D1-1.F10 |
| POST | `/v1/principals/{principalID}/mfa/reset` | D1-1.F08 |
| POST | `/v1/principals/{principalID}/password-resets` | D1-1.F11 |
| GET | `/v1/principals/{principalID}/roles` | D1-1.F12 |
| PUT | `/v1/principals/{principalID}/status` | D1-1.F12 |
| GET | `/v1/subjects/{principalID}` | D1-1.F14 |
| GET | `/v1/subjects/{principalID}/links` | D1-1.F14 |
| POST | `/v1/subjects/{survivorID}/merge` | D1-1.F14 |

## D2-1 · jurisdiction-rules-svc (148 routes)

| Method | Path | Feature |
|---|---|---|
| POST | `/v1/accounting-mappings:resolve` | D2-1.F20 |
| POST | `/v1/admin/calendars` | D2-1.F08 |
| POST | `/v1/admin/calendars/{calendar_code}/imports` | D2-1.F08 |
| POST | `/v1/admin/calendars/{calendar_code}/versions` | D2-1.F08 |
| POST | `/v1/admin/calendars/{calendar_code}/versions/{version}/publish` | D2-1.F08 |
| PUT | `/v1/admin/calendars/{calendar_code}/versions/{version}/sources` | D2-1.F08 |
| POST | `/v1/admin/deployment-regions` | D2-1.F17 |
| POST | `/v1/admin/deployment-rings` | D2-1.F17 |
| POST | `/v1/admin/interpretations` | D2-1.F14 |
| POST | `/v1/admin/interpretations/{interpretation_id}/approve` | D2-1.F14 |
| GET | `/v1/admin/jurisdiction-imports` | D2-1.F07 |
| POST | `/v1/admin/jurisdiction-imports` | D2-1.F07 |
| GET | `/v1/admin/jurisdiction-imports/{batch_id}` | D2-1.F07 |
| POST | `/v1/admin/jurisdiction-imports/{batch_id}/approve` | D2-1.F07 |
| POST | `/v1/admin/jurisdiction-imports/{batch_id}/reject` | D2-1.F07 |
| POST | `/v1/admin/jurisdictions` | D2-1.F01 |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/aliases` | D2-1.F07 |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/aliases/{alias_id}/end` | D2-1.F07 |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/deactivate` | D2-1.F01 |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/rules` | D2-1.F02 |
| POST | `/v1/admin/obligation-rules` | D2-1.F10 |
| PUT | `/v1/admin/obligation-rules/{obligation_rule_id}/provenance` | D2-1.F10 |
| POST | `/v1/admin/obligation-rules/{obligation_rule_id}/publish` | D2-1.F10 |
| POST | `/v1/admin/pack-signing-keys` | D2-1.F15 |
| POST | `/v1/admin/pack-signing-keys/{key_ref}/retire` | D2-1.F15 |
| POST | `/v1/admin/pack-signing-keys/{key_ref}/revoke` | D2-1.F15 |
| POST | `/v1/admin/packs` | D2-1.F14 |
| POST | `/v1/admin/packs/{pack_ref}/versions` | D2-1.F14 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/block` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/certify` | D2-1.F16 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/compile` | D2-1.F15 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/deploy` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/hotfix` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/hotfix/retrospective` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/publish` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/reviews` | D2-1.F16 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/rollback` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/sign` | D2-1.F15 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/submit-review` | D2-1.F14 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/supersede` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/test-bundle` | D2-1.F16 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/test-runs` | D2-1.F16 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/unblock` | D2-1.F17 |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/withdraw` | D2-1.F17 |
| POST | `/v1/admin/payment-terms` | D2-1.F09 |
| POST | `/v1/admin/payment-terms/{term_code}/versions` | D2-1.F09 |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/approve` | D2-1.F09 |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/retire` | D2-1.F09 |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/supersede` | D2-1.F09 |
| POST | `/v1/admin/regimes` | D2-1.F14 |
| GET | `/v1/admin/remediation-cases` | D2-1.F19 |
| POST | `/v1/admin/remediation-cases` | D2-1.F19 |
| GET | `/v1/admin/remediation-cases/{case_id}` | D2-1.F19 |
| POST | `/v1/admin/remediation-cases/{case_id}/close` | D2-1.F19 |
| GET | `/v1/admin/remediation-cases/{case_id}/decisions` | D2-1.F19 |
| POST | `/v1/admin/remediation-cases/{case_id}/dispositions` | D2-1.F19 |
| POST | `/v1/admin/resolver-cache:invalidate` | D2-1.F11 |
| GET | `/v1/admin/rollouts` | D2-1.F18 |
| POST | `/v1/admin/rollouts` | D2-1.F18 |
| GET | `/v1/admin/rollouts/{rollout_id}` | D2-1.F18 |
| POST | `/v1/admin/rollouts/{rollout_id}/attestations` | D2-1.F18 |
| POST | `/v1/admin/rollouts/{rollout_id}/expert-approvals` | D2-1.F18 |
| PUT | `/v1/admin/rollouts/{rollout_id}/owner` | D2-1.F18 |
| PUT | `/v1/admin/rollouts/{rollout_id}/packs` | D2-1.F18 |
| POST | `/v1/admin/rollouts/{rollout_id}/transition` | D2-1.F18 |
| POST | `/v1/admin/rollouts:seed-portfolio` | D2-1.F18 |
| POST | `/v1/admin/rules/{jurisdiction_rule_id}/drift` | D2-1.F04 |
| PUT | `/v1/admin/rules/{jurisdiction_rule_id}/parameters` | D2-1.F14 |
| PUT | `/v1/admin/rules/{jurisdiction_rule_id}/provenance` | D2-1.F14 |
| POST | `/v1/admin/rules/{jurisdiction_rule_id}/transition` | D2-1.F02 |
| POST | `/v1/admin/source-change-notices` | D2-1.F17 |
| POST | `/v1/admin/source-change-notices/{notice_id}/close` | D2-1.F17 |
| POST | `/v1/admin/source-change-notices/{notice_id}/review` | D2-1.F17 |
| POST | `/v1/admin/sources` | D2-1.F14 |
| POST | `/v1/admin/sources/{source_id}/review` | D2-1.F14 |
| POST | `/v1/admin/sources/{source_id}/supersede` | D2-1.F14 |
| GET | `/v1/calendars` | D2-1.F08 |
| GET | `/v1/calendars/{calendar_code}` | D2-1.F08 |
| GET | `/v1/calendars/{calendar_code}/add-business-days` | D2-1.F08 |
| GET | `/v1/calendars/{calendar_code}/is-business-day` | D2-1.F08 |
| GET | `/v1/calendars/{calendar_code}/versions` | D2-1.F08 |
| GET | `/v1/calendars/{calendar_code}/versions/{version}` | D2-1.F08 |
| GET | `/v1/deployment-regions` | D2-1.F17 |
| GET | `/v1/deployment-rings` | D2-1.F17 |
| GET | `/v1/entities/{legal_entity_id}/jurisdictions` | D2-1.F22 |
| GET | `/v1/interpretations/{interpretation_id}` | D2-1.F14 |
| GET | `/v1/jurisdiction-code-mappings` | D2-1.F07 |
| POST | `/v1/jurisdiction-decisions:resolve` | D2-1.F13 |
| GET | `/v1/jurisdiction-resolutions` | D2-1.F07 |
| GET | `/v1/jurisdiction-support/{jurisdiction_code}` | D2-1.F18 |
| GET | `/v1/jurisdictions` | D2-1.F01 |
| GET | `/v1/jurisdictions/{jurisdiction_id}` | D2-1.F01 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/aliases` | D2-1.F07 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/ancestors` | D2-1.F01 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/as-of` | D2-1.F07 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/hierarchy` | D2-1.F07 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/rule-pack` | D2-1.F03 |
| GET | `/v1/jurisdictions/{jurisdiction_id}/rules` | D2-1.F02 |
| POST | `/v1/licences:evaluate` | D2-1.F20 |
| POST | `/v1/obligation-calculations:calculate` | D2-1.F10 |
| GET | `/v1/obligation-rules` | D2-1.F10 |
| GET | `/v1/obligation-rules/{obligation_rule_id}` | D2-1.F10 |
| GET | `/v1/ops/metrics` | D2-1.F17 |
| GET | `/v1/pack-coverage` | D2-1.F11 |
| GET | `/v1/pack-signing-keys` | D2-1.F15 |
| GET | `/v1/packs` | D2-1.F14 |
| GET | `/v1/packs/{pack_ref}` | D2-1.F14 |
| GET | `/v1/packs/{pack_ref}/versions` | D2-1.F14 |
| GET | `/v1/packs/{pack_ref}/versions/{version}` | D2-1.F14 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/artifact` | D2-1.F15 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/certification` | D2-1.F16 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/deployments` | D2-1.F17 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/hotfix` | D2-1.F17 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/reviews` | D2-1.F16 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/test-bundle` | D2-1.F16 |
| GET | `/v1/packs/{pack_ref}/versions/{version}/test-runs` | D2-1.F16 |
| POST | `/v1/packs/{pack_ref}/versions/{version}/verify` | D2-1.F15 |
| GET | `/v1/payment-terms/{term_code}` | D2-1.F09 |
| GET | `/v1/payment-terms/{term_code}/due-date` | D2-1.F09 |
| GET | `/v1/payment-terms/{term_code}/explain-due-date` | D2-1.F09 |
| GET | `/v1/payment-terms/{term_code}/versions` | D2-1.F09 |
| GET | `/v1/payment-terms/{term_code}/versions/{version}` | D2-1.F09 |
| POST | `/v1/payroll-statutory-parameters:resolve` | D2-1.F20 |
| GET | `/v1/regimes` | D2-1.F14 |
| GET | `/v1/regimes/{regime}` | D2-1.F14 |
| POST | `/v1/registration-identifiers:validate` | D2-1.F20 |
| POST | `/v1/registration-obligations:resolve` | D2-1.F20 |
| POST | `/v1/regulatory-calculations:execute` | D2-1.F20 |
| POST | `/v1/regulatory-submissions` | D2-1.F21 |
| GET | `/v1/regulatory-submissions/{submission_id}` | D2-1.F21 |
| POST | `/v1/regulatory-submissions/{submission_id}/events` | D2-1.F21 |
| POST | `/v1/retention-rules:resolve` | D2-1.F20 |
| GET | `/v1/rollout-checklists` | D2-1.F18 |
| GET | `/v1/rule-decisions/{decision_id}` | D2-1.F11 |
| POST | `/v1/rule-resolutions:resolve` | D2-1.F11 |
| GET | `/v1/rules/{jurisdiction_rule_id}` | D2-1.F02 |
| GET | `/v1/rules/{jurisdiction_rule_id}/drift-events` | D2-1.F04 |
| GET | `/v1/rules/{jurisdiction_rule_id}/parameters` | D2-1.F14 |
| GET | `/v1/rules/{jurisdiction_rule_id}/provenance` | D2-1.F14 |
| GET | `/v1/rules/{jurisdiction_rule_id}/status-history` | D2-1.F02 |
| POST | `/v1/scheme-elections:evaluate` | D2-1.F20 |
| GET | `/v1/source-change-notices` | D2-1.F17 |
| GET | `/v1/source-change-notices/{notice_id}` | D2-1.F17 |
| GET | `/v1/sources` | D2-1.F14 |
| GET | `/v1/sources/{source_id}` | D2-1.F14 |
| POST | `/v1/submission-profiles:resolve` | D2-1.F20 |
| POST | `/v1/threshold-monitors:evaluate` | D2-1.F20 |
| POST | `/v1/validate-action` | D2-1.F12 |

## D1-13 · key-management-svc (5 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/keys` | D1-13.F01 |
| POST | `/v1/keys` | D1-13.F01 |
| GET | `/v1/keys/{id}` | D1-13.F01 |
| POST | `/v1/keys/{id}/disable` | D1-13.F01 |
| POST | `/v1/keys/{id}/rotate` | D1-13.F01 |

## D1-10 · mtls-management-svc (9 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/mtls/certificates` | D1-10.F01 |
| POST | `/v1/mtls/certificates` | D1-10.F01 |
| POST | `/v1/mtls/certificates/csr` | D1-10.F04 |
| DELETE | `/v1/mtls/certificates/{id}` | D1-10.F01 |
| GET | `/v1/mtls/certificates/{id}` | D1-10.F01 |
| POST | `/v1/mtls/certificates/{id}/rotate` | D1-10.F01 |
| GET | `/v1/mtls/crl` | D1-10.F02 |
| GET | `/v1/mtls/policies` | D1-10.F08 |
| POST | `/v1/mtls/policies` | D1-10.F08 |

## D1-7 · notification-svc (97 routes)

| Method | Path | Feature |
|---|---|---|
| POST | `/v1/acknowledgments` | D1-7.F09 |
| GET | `/v1/approvals/{id}` | D1-7.F07 |
| POST | `/v1/approvals/{id}/approve` | D1-7.F07 |
| POST | `/v1/approvals/{id}/reject` | D1-7.F07 |
| POST | `/v1/bulk-sends` | D1-7.F07 |
| POST | `/v1/bulk-sends/{id}/dispatch` | D1-7.F07 |
| POST | `/v1/channel-decision` | D1-7.F04 |
| POST | `/v1/communication-intents` | D1-7.F01 |
| GET | `/v1/communication-intents/{id}` | D1-7.F01 |
| POST | `/v1/communication-intents/{id}/activate` | D1-7.F01 |
| POST | `/v1/communication-intents/{id}/retire` | D1-7.F01 |
| GET | `/v1/communication-intents/{id}/templates` | D1-7.F01 |
| POST | `/v1/communication-intents/{id}/versions` | D1-7.F01 |
| POST | `/v1/communications` | D1-7.F06 |
| GET | `/v1/communications/{id}` | D1-7.F06 |
| POST | `/v1/communications/{id}/attempts/{attempt_id}/resolve` | D1-7.F06 |
| POST | `/v1/communications/{id}/cancel` | D1-7.F06 |
| POST | `/v1/communications/{id}/dispatch` | D1-7.F06 |
| POST | `/v1/communications/{id}/misdelivery` | D1-7.F06 |
| POST | `/v1/communications/{id}/prepare` | D1-7.F06 |
| POST | `/v1/communications/{id}/resend` | D1-7.F06 |
| POST | `/v1/document-templates` | D1-7.F15 |
| POST | `/v1/document-templates/versions/{versionID}/approve` | D1-7.F15 |
| POST | `/v1/document-templates/versions/{versionID}/preview` | D1-7.F15 |
| POST | `/v1/document-templates/versions/{versionID}/publish` | D1-7.F15 |
| POST | `/v1/document-templates/versions/{versionID}/validate` | D1-7.F15 |
| GET | `/v1/document-templates/{templateID}` | D1-7.F15 |
| GET | `/v1/document-templates/{templateID}/compare` | D1-7.F15 |
| GET | `/v1/document-templates/{templateID}/locales` | D1-7.F15 |
| GET | `/v1/document-templates/{templateID}/published` | D1-7.F15 |
| POST | `/v1/document-templates/{templateID}/retire` | D1-7.F15 |
| POST | `/v1/document-templates/{templateID}/versions` | D1-7.F15 |
| GET | `/v1/evidence/{id}` | D1-7.F08 |
| GET | `/v1/exceptions` | D1-7.F03 |
| GET | `/v1/intents/{id}/effective` | D1-7.F01 |
| GET | `/v1/notifications` | D1-7.F11 |
| POST | `/v1/notifications` | D1-7.F11 |
| GET | `/v1/notifications/actions/{token}` | D1-7.F17 |
| POST | `/v1/notifications/actions/{token}/execute` | D1-7.F17 |
| POST | `/v1/notifications/events/ingest` | D1-7.F11 |
| GET | `/v1/notifications/intents/{id}` | D1-7.F11 |
| GET | `/v1/notifications/suppression` | D1-7.F11 |
| POST | `/v1/notifications/suppression` | D1-7.F11 |
| GET | `/v1/notifications/templates` | D1-7.F11 |
| GET | `/v1/notifications/unread-count` | D1-7.F11 |
| POST | `/v1/notifications/unsubscribe` | D1-7.F11 |
| POST | `/v1/notifications/webhooks/{provider}` | D1-7.F11 |
| GET | `/v1/notifications/{id}` | D1-7.F11 |
| GET | `/v1/notifications/{id}/attempts` | D1-7.F11 |
| POST | `/v1/notifications/{id}/cancel` | D1-7.F11 |
| GET | `/v1/notifications/{id}/delivery-status` | D1-7.F11 |
| GET | `/v1/notifications/{id}/evidence` | D1-7.F11 |
| POST | `/v1/notifications/{id}/read` | D1-7.F11 |
| POST | `/v1/notifications/{id}/resend` | D1-7.F11 |
| POST | `/v1/notifications/{id}/resolve-delivery-outcome` | D1-7.F11 |
| POST | `/v1/preferences` | D1-7.F05 |
| GET | `/v1/preferences/me` | D1-7.F05 |
| GET | `/v1/provider-bindings` | D1-7.F06 |
| POST | `/v1/provider-bindings/{id}/circuit` | D1-7.F06 |
| POST | `/v1/provider-events/{binding}` | D1-7.F08 |
| POST | `/v1/recipient-endpoint-exceptions` | D1-7.F03 |
| POST | `/v1/recipient-resolution` | D1-7.F03 |
| POST | `/v1/regulated-notices` | D1-7.F09 |
| GET | `/v1/regulated-notices/{id}` | D1-7.F09 |
| POST | `/v1/regulated-notices/{id}/manual-evidence` | D1-7.F09 |
| POST | `/v1/regulated-notices/{id}/record-declaration` | D1-7.F10 |
| POST | `/v1/render-previews` | D1-7.F02 |
| GET | `/v1/reputation` | D1-7.F08 |
| POST | `/v1/revalidate` | D1-7.F03 |
| POST | `/v1/streams/{stream}` | D1-7.F08 |
| GET | `/v1/suppressions` | D1-7.F05 |
| POST | `/v1/suppressions` | D1-7.F05 |
| POST | `/v1/suppressions/{id}/lift` | D1-7.F05 |
| POST | `/v1/templates` | D1-7.F02 |
| GET | `/v1/templates/{id}` | D1-7.F02 |
| POST | `/v1/templates/{id}/approve` | D1-7.F02 |
| POST | `/v1/templates/{id}/publish` | D1-7.F02 |
| POST | `/v1/templates/{id}/reject` | D1-7.F02 |
| POST | `/v1/templates/{id}/retire` | D1-7.F02 |
| POST | `/v1/templates/{id}/validate` | D1-7.F02 |
| GET | `/v1/webhook-dead-letters` | D1-7.F16 |
| GET | `/v1/webhook-deliveries/{id}` | D1-7.F16 |
| POST | `/v1/webhook-deliveries/{id}/replay` | D1-7.F16 |
| GET | `/v1/webhook-subscriptions` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions` | D1-7.F16 |
| GET | `/v1/webhook-subscriptions/{id}` | D1-7.F16 |
| PATCH | `/v1/webhook-subscriptions/{id}` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/approve` | D1-7.F16 |
| GET | `/v1/webhook-subscriptions/{id}/deliveries` | D1-7.F16 |
| GET | `/v1/webhook-subscriptions/{id}/health` | D1-7.F16 |
| GET | `/v1/webhook-subscriptions/{id}/log` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/reject` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/resume` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/revoke` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/rotate-signing-key` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/submit` | D1-7.F16 |
| POST | `/v1/webhook-subscriptions/{id}/suspend` | D1-7.F16 |

## D2-4 · obligations-svc (24 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/obligation-definitions` | D2-4.F05 |
| POST | `/v1/obligation-definitions` | D2-4.F05 |
| GET | `/v1/obligation-definitions/{definition_id}` | D2-4.F05 |
| GET | `/v1/obligation-definitions/{definition_id}/entity-mappings` | D2-4.F05 |
| POST | `/v1/obligation-definitions/{definition_id}/entity-mappings` | D2-4.F05 |
| POST | `/v1/obligation-definitions/{definition_id}/entity-mappings/{mapping_id}/end` | D2-4.F05 |
| POST | `/v1/obligation-definitions/{definition_id}/retire` | D2-4.F05 |
| GET | `/v1/obligation-definitions/{definition_id}/versions` | D2-4.F05 |
| POST | `/v1/obligation-definitions/{definition_id}/versions` | D2-4.F05 |
| GET | `/v1/obligations` | D2-4.F01 |
| POST | `/v1/obligations` | D2-4.F01 |
| GET | `/v1/obligations/{obligation_id}` | D2-4.F01 |
| PATCH | `/v1/obligations/{obligation_id}` | D2-4.F01 |
| GET | `/v1/obligations/{obligation_id}/applicability` | D2-4.F06 |
| GET | `/v1/obligations/{obligation_id}/applicability-decisions` | D2-4.F06 |
| POST | `/v1/obligations/{obligation_id}/applicability-decisions` | D2-4.F06 |
| GET | `/v1/obligations/{obligation_id}/filing-requirements` | D2-4.F04 |
| POST | `/v1/obligations/{obligation_id}/filing-requirements` | D2-4.F04 |
| GET | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}` | D2-4.F04 |
| POST | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/status` | D2-4.F04 |
| GET | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/transitions` | D2-4.F04 |
| POST | `/v1/obligations/{obligation_id}/status` | D2-4.F02 |
| DELETE | `/v1/obligations/{obligation_id}/workflow` | D2-4.F03 |
| POST | `/v1/obligations/{obligation_id}/workflow` | D2-4.F03 |

## D2-3 · policy-svc (39 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/accounting-policies` | D2-3.F10 |
| POST | `/v1/accounting-policies` | D2-3.F10 |
| GET | `/v1/accounting-policies/explain` | D2-3.F10 |
| GET | `/v1/accounting-policies/resolve` | D2-3.F10 |
| GET | `/v1/accounting-policies/versions/{version_id}` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/submit-review` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/approve` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/activate` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/supersede` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/emergency-suspend` | D2-3.F10 |
| POST | `/v1/accounting-policies/versions/{version_id}/review-suspension` | D2-3.F10 |
| GET | `/v1/accounting-policies/versions/{version_id}/impact` | D2-3.F10 |
| POST | `/v1/attestations` | D2-3.F06 |
| GET | `/v1/attestations/{id}` | D2-3.F06 |
| POST | `/v1/attestations/{id}/revoke` | D2-3.F06 |
| POST | `/v1/control-test-definitions` | D2-3.F06 |
| GET | `/v1/control-test-definitions/{id}` | D2-3.F06 |
| GET | `/v1/control-test-definitions/{id}/executions` | D2-3.F06 |
| POST | `/v1/control-test-definitions/{id}/executions` | D2-3.F06 |
| GET | `/v1/controls/{control_ref}/effectiveness` | D2-3.F06 |
| GET | `/v1/policies` | D2-3.F01 |
| POST | `/v1/policies` | D2-3.F01 |
| POST | `/v1/policies/evaluate` | D2-3.F03 |
| GET | `/v1/policies/explain` | D2-3.F03 |
| GET | `/v1/policies/resolve` | D2-3.F03 |
| GET | `/v1/policies/{policy_id}` | D2-3.F01 |
| GET | `/v1/policies/{policy_id}/versions` | D2-3.F01 |
| POST | `/v1/policies/{policy_id}/versions` | D2-3.F01 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/activate` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/approve` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/retire` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/review-suspension` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/schedule` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/submit-review` | D2-3.F02 |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/suspend` | D2-3.F02 |
| GET | `/v1/policy-evaluations/{evidence_id}` | D2-3.F03 |
| GET | `/v1/policy-versions/compare` | D2-3.F01 |
| GET | `/v1/policy-versions/{version_id}` | D2-3.F01 |
| GET | `/v1/policy-versions/{version_id}/transitions` | D2-3.F01 |

## D2-9 · retention-registry-svc (7 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/legal-holds` | D2-9.F03 |
| POST | `/v1/legal-holds` | D2-9.F03 |
| GET | `/v1/legal-holds/{id}` | D2-9.F03 |
| POST | `/v1/legal-holds/{id}/release` | D2-9.F03 |
| GET | `/v1/retention-policies` | D2-9.F01 |
| POST | `/v1/retention-policies` | D2-9.F01 |
| GET | `/v1/retention/resolve` | D2-9.F02 |

## D2-8 · schema-registry-svc (5 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/schemas` | D2-8.F02 |
| GET | `/v1/schemas/{eventName}/versions` | D2-8.F02 |
| POST | `/v1/schemas/{eventName}/versions` | D2-8.F01 |
| GET | `/v1/schemas/{eventName}/versions/latest` | D2-8.F02 |
| GET | `/v1/schemas/{eventName}/versions/{version}` | D2-8.F02 |

## D1-6 · search-indexer-svc (20 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/checkpoints` | D1-6.F08 |
| GET | `/v1/index-contracts` | D1-6.F05 |
| POST | `/v1/index-contracts` | D1-6.F05 |
| GET | `/v1/index-contracts/{contractID}` | D1-6.F05 |
| POST | `/v1/index-contracts/{contractID}/state` | D1-6.F05 |
| GET | `/v1/index-generations` | D1-6.F06 |
| POST | `/v1/index-generations` | D1-6.F06 |
| GET | `/v1/index-generations/{generationID}/retrieval-evaluations` | D1-6.F06 |
| POST | `/v1/index-generations/{generationID}/retrieval-evaluations` | D1-6.F06 |
| POST | `/v1/index-generations/{generationID}/state` | D1-6.F06 |
| GET | `/v1/restrictions` | D1-6.F07 |
| POST | `/v1/restrictions` | D1-6.F07 |
| POST | `/v1/retrieve` | D1-6.F03 |
| GET | `/v1/scopes` | D1-6.F10 |
| POST | `/v1/search` | D1-6.F01 |
| GET | `/v1/search-evidence` | D1-6.F10 |
| POST | `/v1/search-exports` | D1-6.F09 |
| GET | `/v1/search-sources` | D1-6.F10 |
| POST | `/v1/search-sources` | D1-6.F10 |
| POST | `/v1/search/semantic` | D1-6.F02 |

## D1-4 · secret-vault-integration-svc (19 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/secret-policies` | D1-4.F01 |
| POST | `/v1/secret-policies` | D1-4.F01 |
| POST | `/v1/secret-policies/{secret_policy_id}/emergency-retrieval` | D1-4.F06 |
| POST | `/v1/secret-policies/{secret_policy_id}/material` | D1-4.F02 |
| POST | `/v1/secret-policies/{secret_policy_id}/rotate` | D1-4.F02 |
| GET | `/v1/secret-policies/{secret_policy_id}/versions` | D1-4.F01 |
| POST | `/v1/secret-policies/{secret_policy_id}/versions` | D1-4.F01 |
| POST | `/v1/secret-policies/{secret_policy_id}/versions/{version_id}/activate` | D1-4.F01 |
| GET | `/v1/secrets/audit` | D1-4.F04 |
| POST | `/v1/secrets/broker` | D1-4.F03 |
| GET | `/v1/secrets/leases` | D1-4.F04 |
| GET | `/v1/secrets/leases/{lease_id}` | D1-4.F04 |
| POST | `/v1/secrets/leases/{lease_id}/revoke` | D1-4.F04 |
| POST | `/v1/secrets/leases/{lease_id}/verify` | D1-4.F04 |
| POST | `/v1/secrets/sign` | D1-4.F05 |
| POST | `/v1/secrets/use` | D1-4.F05 |
| GET | `/v1/shared-secret-exceptions` | D1-4.F07 |
| POST | `/v1/shared-secret-exceptions` | D1-4.F07 |
| POST | `/v1/shared-secret-exceptions/{exception_id}/revoke` | D1-4.F07 |

## D1-11 · siem-integration-svc (5 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/siem/events` | D1-11.F02 |
| GET | `/v1/siem/exporters` | D1-11.F01 |
| POST | `/v1/siem/exporters` | D1-11.F01 |
| GET | `/v1/siem/exporters/{id}` | D1-11.F01 |
| POST | `/v1/siem/stream` | D1-11.F02 |

## D1-2 · tenant-entity-registry-svc (79 routes)

| Method | Path | Feature |
|---|---|---|
| GET | `/v1/approval-requests` | D1-2.F06 |
| GET | `/v1/approval-requests/{approvalRequestID}` | D1-2.F06 |
| POST | `/v1/approval-requests/{approvalRequestID}/approve` | D1-2.F06 |
| POST | `/v1/approval-requests/{approvalRequestID}/reject` | D1-2.F06 |
| GET | `/v1/entities/by-registry-number` | D1-2.F04 |
| POST | `/v1/entities/{entityID}/activation` | D1-2.F03 |
| GET | `/v1/entities/{entityID}/as-of` | D1-2.F04 |
| GET | `/v1/entities/{entityID}/control-chain` | D1-2.F13 |
| GET | `/v1/entities/{entityID}/group` | D1-2.F13 |
| POST | `/v1/entities/{entityID}/legal-name` | D1-2.F03 |
| POST | `/v1/entities/{entityID}/merge` | D1-2.F05 |
| GET | `/v1/entities/{entityID}/merge-records` | D1-2.F05 |
| GET | `/v1/entities/{entityID}/parents` | D1-2.F13 |
| POST | `/v1/entities/{entityID}/profile-amendments` | D1-2.F03 |
| POST | `/v1/entities/{entityID}/registered-office` | D1-2.F03 |
| POST | `/v1/entities/{entityID}/unmerge` | D1-2.F05 |
| POST | `/v1/entities/{entityID}/verification` | D1-2.F03 |
| GET | `/v1/entities/{entityID}/versions` | D1-2.F04 |
| GET | `/v1/identifiers:find` | D1-2.F14 |
| GET | `/v1/ownership-graph` | D1-2.F13 |
| POST | `/v1/ownership-relationships` | D1-2.F13 |
| GET | `/v1/ownership-relationships/{relationshipID}` | D1-2.F13 |
| POST | `/v1/ownership-relationships/{relationshipID}/approval` | D1-2.F13 |
| POST | `/v1/ownership-relationships/{relationshipID}/end` | D1-2.F13 |
| POST | `/v1/ownership-relationships/{relationshipID}/rejection` | D1-2.F13 |
| POST | `/v1/ownership-relationships/{relationshipID}/supersession` | D1-2.F13 |
| GET | `/v1/parties` | D1-2.F14 |
| POST | `/v1/parties` | D1-2.F14 |
| GET | `/v1/parties/{partyID}` | D1-2.F14 |
| POST | `/v1/parties/{partyID}/facts:propose` | D1-2.F14 |
| POST | `/v1/parties/{partyID}/identifiers` | D1-2.F14 |
| GET | `/v1/parties/{partyID}/merge-history` | D1-2.F14 |
| POST | `/v1/parties/{partyID}/roles` | D1-2.F14 |
| POST | `/v1/parties/{partyID}/roles/{roleID}/removal` | D1-2.F14 |
| GET | `/v1/registry-conflicts` | D1-2.F05 |
| POST | `/v1/registry-conflicts/{conflictID}/resolution` | D1-2.F05 |
| GET | `/v1/resolution-cases` | D1-2.F14 |
| GET | `/v1/resolution-cases/{caseID}` | D1-2.F14 |
| POST | `/v1/resolution-cases/{caseID}/decision` | D1-2.F14 |
| POST | `/v1/resolution/{caseID}:merge` | D1-2.F14 |
| POST | `/v1/resolution/{caseID}:split` | D1-2.F14 |
| POST | `/v1/resolution:match` | D1-2.F14 |
| GET | `/v1/resolve-tenant` | D1-2.F02 |
| POST | `/v1/tenants/{tenantID}/commands/{command}` | D1-2.F01 |
| GET | `/v1/tenants/{tenantID}/defaults` | D1-2.F02 |
| POST | `/v1/tenants/{tenantID}/defaults` | D1-2.F02 |
| POST | `/v1/tenants/{tenantID}/home-region` | D1-2.F02 |
| GET | `/v1/tenants/{tenantID}/host-bindings` | D1-2.F02 |
| POST | `/v1/tenants/{tenantID}/host-bindings` | D1-2.F02 |
| GET | `/v1/tenants/{tenantID}/lifecycle-history` | D1-2.F02 |
| POST | `/v1/entities` | D1-2.F03 |
| GET | `/v1/entities/{entityID}` | D1-2.F03 |
| PATCH | `/v1/entities/{entityID}` | D1-2.F03 |
| GET | `/v1/entities/{entityID}/hierarchies` | D1-2.F08 |
| GET | `/v1/entities/{entityID}/jurisdictions` | D1-2.F08 |
| POST | `/v1/entities/{entityID}/jurisdictions` | D1-2.F08 |
| GET | `/v1/entities/{entityID}/status` | D1-2.F11 |
| POST | `/v1/entities/{entityID}/status` | D1-2.F11 |
| GET | `/v1/entities/{entityID}/tax-identity-bundles` | D1-2.F07 |
| POST | `/v1/entities/{entityID}/tax-identity-bundles` | D1-2.F07 |
| POST | `/v1/entity-hierarchies` | D1-2.F08 |
| DELETE | `/v1/entity-hierarchies/{hierarchyID}` | D1-2.F08 |
| DELETE | `/v1/entity-jurisdictions/{assignmentID}` | D1-2.F08 |
| POST | `/v1/residency-policies` | D1-2.F09 |
| GET | `/v1/residency-policies/{policyID}` | D1-2.F09 |
| GET | `/v1/residency-regions` | D1-2.F09 |
| GET | `/v1/residency-regions/{regionID}` | D1-2.F09 |
| GET | `/v1/tax-identity-bundles/{bundleID}` | D1-2.F07 |
| POST | `/v1/tax-identity-bundles/{bundleID}/status` | D1-2.F07 |
| POST | `/v1/tenants` | D1-2.F01 |
| GET | `/v1/tenants/{tenantID}` | D1-2.F01 |
| GET | `/v1/tenants/{tenantID}/entities` | D1-2.F04 |
| POST | `/v1/tenants/{tenantID}/lifecycle` | D1-2.F01 |
| GET | `/v1/tenants/{tenantID}/residency-region` | D1-2.F02 |
| GET | `/v1/tenants/{tenantID}/workspaces` | D1-2.F10 |
| POST | `/v1/workspaces` | D1-2.F10 |
| GET | `/v1/workspaces/{workspaceID}` | D1-2.F10 |
| PATCH | `/v1/workspaces/{workspaceID}` | D1-2.F10 |
| POST | `/v1/workspaces/{workspaceID}/status` | D1-2.F10 |

## D2-6 · workflow-svc (76 routes)

| Method | Path | Feature |
|---|---|---|
| POST | `/v1/audit/engagements` | D2-6.F14 |
| GET | `/v1/audit/engagements/{engagement_id}` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/acceptance-decision` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/activate` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/amend-scope` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/close` | D2-6.F14 |
| GET | `/v1/audit/engagements/{engagement_id}/completion-gates` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/enter-completion-review` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/mark-fieldwork-complete` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/mark-report-ready` | D2-6.F14 |
| GET | `/v1/audit/engagements/{engagement_id}/plan` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/plan` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/reviews` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/reviews/quality` | D2-6.F14 |
| GET | `/v1/audit/engagements/{engagement_id}/reviews/release-gates` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/submit-acceptance` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/withdraw` | D2-6.F14 |
| POST | `/v1/audit/engagements/{engagement_id}/workpapers` | D2-6.F14 |
| POST | `/v1/audit/plans/{plan_id}/approve` | D2-6.F14 |
| POST | `/v1/audit/plans/{plan_id}/materiality` | D2-6.F14 |
| GET | `/v1/audit/plans/{plan_id}/risks` | D2-6.F14 |
| POST | `/v1/audit/plans/{plan_id}/risks` | D2-6.F14 |
| POST | `/v1/audit/quality-reviews/{quality_review_id}/complete` | D2-6.F14 |
| POST | `/v1/audit/review-notes/{review_note_id}/resolve` | D2-6.F14 |
| POST | `/v1/audit/review-notes/{review_note_id}/respond` | D2-6.F14 |
| POST | `/v1/audit/review-scopes/{review_scope_id}/notes` | D2-6.F14 |
| POST | `/v1/audit/review-scopes/{review_scope_id}/reviewers` | D2-6.F14 |
| POST | `/v1/audit/review-scopes/{review_scope_id}/sign-off` | D2-6.F14 |
| POST | `/v1/audit/risks/{risk_id}/assertions` | D2-6.F14 |
| POST | `/v1/audit/risks/{risk_id}/assess` | D2-6.F14 |
| POST | `/v1/audit/risks/{risk_id}/mark-significant` | D2-6.F14 |
| POST | `/v1/audit/risks/{risk_id}/response` | D2-6.F14 |
| POST | `/v1/audit/sign-offs/{sign_off_id}/withdraw` | D2-6.F14 |
| GET | `/v1/audit/workpapers/{workpaper_id}` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/addenda` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/conclusions` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/cross-references` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/evidence-links` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/lock` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/mark-prepared` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/procedures` | D2-6.F14 |
| POST | `/v1/audit/workpapers/{workpaper_id}/results` | D2-6.F14 |
| GET | `/v1/form-submissions/{submission_id}` | D2-6.F15 |
| POST | `/v1/form-submissions/{submission_id}/drafts` | D2-6.F15 |
| POST | `/v1/form-submissions/{submission_id}/route` | D2-6.F15 |
| POST | `/v1/form-submissions/{submission_id}/submit` | D2-6.F15 |
| POST | `/v1/form-submissions/{submission_id}/supersede` | D2-6.F15 |
| POST | `/v1/form-submissions/{submission_id}/validate` | D2-6.F15 |
| GET | `/v1/form-submissions/{submission_id}/validation-result` | D2-6.F15 |
| GET | `/v1/form-submissions/{submission_id}/version` | D2-6.F15 |
| POST | `/v1/forms` | D2-6.F15 |
| GET | `/v1/forms/{form_id}` | D2-6.F15 |
| POST | `/v1/forms/{form_id}/drafts` | D2-6.F15 |
| GET | `/v1/forms/{form_id}/pending-submissions` | D2-6.F15 |
| POST | `/v1/forms/{form_id}/publish` | D2-6.F15 |
| POST | `/v1/forms/{form_id}/retire` | D2-6.F15 |
| GET | `/v1/workflow-definitions` | D2-6.F11 |
| POST | `/v1/workflow-definitions` | D2-6.F11 |
| GET | `/v1/workflow-definitions/{workflow_definition_id}` | D2-6.F11 |
| POST | `/v1/workflow-definitions/{workflow_definition_id}/activate` | D2-6.F11 |
| POST | `/v1/workflow-definitions/{workflow_definition_id}/retire` | D2-6.F11 |
| POST | `/v1/workflow-definitions/{workflow_definition_id}/supersede` | D2-6.F11 |
| POST | `/v1/workflow-definitions/{workflow_definition_id}/suspend` | D2-6.F11 |
| POST | `/v1/workflows` | D2-6.F01 |
| GET | `/v1/workflows/pending` | D2-6.F02 |
| GET | `/v1/workflows/{workflow_instance_id}` | D2-6.F02 |
| POST | `/v1/workflows/{workflow_instance_id}/actions` | D2-6.F03 |
| POST | `/v1/workflows/{workflow_instance_id}/cancel` | D2-6.F05 |
| POST | `/v1/workflows/{workflow_instance_id}/escalate` | D2-6.F05 |
| POST | `/v1/workflows/{workflow_instance_id}/escalate-to` | D2-6.F05 |
| GET | `/v1/workflows/{workflow_instance_id}/evidence` | D2-6.F02 |
| POST | `/v1/workflows/{workflow_instance_id}/invalidate` | D2-6.F05 |
| GET | `/v1/workflows/{workflow_instance_id}/next-approver` | D2-6.F02 |
| POST | `/v1/workflows/{workflow_instance_id}/quorum-votes` | D2-6.F04 |
| POST | `/v1/workflows/{workflow_instance_id}/request-changes` | D2-6.F03 |
| POST | `/v1/workflows/{workflow_instance_id}/verify-release` | D2-6.F05 |

---

# Appendix B · Console API coverage (6 Nov 2026)

For each service: the screen-relevant routes its console API client (`lib/api/*.ts` in the frontend repo) does not call yet. Left out by design: protocol endpoints (OIDC, SCIM, JWKS), `/admin/v1` aliases, `/internal/*` service-to-service routes, workload APIs (secret broker/use/sign, runtime attest), provider callbacks, and the decision-log append. Each service's FE task must call every route listed here, or record in `progress.md` why a route has no screen.

## D1-1 · identity-context-svc: client calls 43 of 79 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/account/activate` |
| POST | `/v1/account/password` |
| POST | `/v1/account/password/forgot` |
| POST | `/v1/account/password/reset` |
| POST | `/v1/authenticate/federated` |
| POST | `/v1/context/support/{supportContextID}/review` |
| GET | `/v1/federation/connections/{connectionID}` |
| POST | `/v1/federation/connections/{connectionID}/keys/refresh` |
| GET | `/v1/federation/connections/{connectionID}/logins` |
| GET | `/v1/federation/connections/{connectionID}/mapping` |
| POST | `/v1/federation/connections/{connectionID}/mapping` |
| POST | `/v1/federation/connections/{connectionID}/mapping/versions/{version}/approve` |
| POST | `/v1/federation/connections/{connectionID}/mapping/versions/{version}/reject` |
| GET | `/v1/federation/connections/{connectionID}/provisioning/deprovision-latency` |
| GET | `/v1/federation/connections/{connectionID}/provisioning/events` |
| GET | `/v1/federation/connections/{connectionID}/provisioning/exceptions` |
| POST | `/v1/federation/connections/{connectionID}/provisioning/exceptions/{exceptionID}/resolve` |
| POST | `/v1/federation/connections/{connectionID}/provisioning/reconcile` |
| GET | `/v1/federation/connections/{connectionID}/provisioning/status` |
| POST | `/v1/federation/connections/{connectionID}/resume` |
| POST | `/v1/federation/connections/{connectionID}/revoke` |
| POST | `/v1/federation/connections/{connectionID}/rotate-metadata` |
| POST | `/v1/federation/connections/{connectionID}/scim-token` |
| POST | `/v1/federation/connections/{connectionID}/scim-token/revoke` |
| POST | `/v1/federation/connections/{connectionID}/suspend` |
| POST | `/v1/federation/connections/{connectionID}/versions/{version}/approve` |
| POST | `/v1/federation/connections/{connectionID}/versions/{version}/reject` |
| GET | `/v1/federation/links` |
| POST | `/v1/federation/links` |
| GET | `/v1/federation/links/{linkID}` |
| GET | `/v1/identities/{principalID}` |
| GET | `/v1/identities/{principalID}/assurance-history` |
| GET | `/v1/identities/{principalID}/authentication-events` |
| GET | `/v1/identities/{principalID}/links` |
| POST | `/v1/identities/{principalID}/merge` |
| GET | `/v1/subjects/{principalID}/links` |

## D1-2 · tenant-entity-registry-svc: client calls 42 of 79 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/approval-requests` |
| GET | `/v1/approval-requests/{approvalRequestID}` |
| POST | `/v1/approval-requests/{approvalRequestID}/approve` |
| POST | `/v1/approval-requests/{approvalRequestID}/reject` |
| POST | `/v1/entities/{entityID}/activation` |
| GET | `/v1/entities/{entityID}/control-chain` |
| GET | `/v1/entities/{entityID}/group` |
| POST | `/v1/entities/{entityID}/merge` |
| GET | `/v1/entities/{entityID}/merge-records` |
| GET | `/v1/entities/{entityID}/parents` |
| POST | `/v1/entities/{entityID}/unmerge` |
| POST | `/v1/entities/{entityID}/verification` |
| GET | `/v1/identifiers:find` |
| GET | `/v1/ownership-graph` |
| POST | `/v1/ownership-relationships` |
| GET | `/v1/ownership-relationships/{relationshipID}` |
| POST | `/v1/ownership-relationships/{relationshipID}/approval` |
| POST | `/v1/ownership-relationships/{relationshipID}/end` |
| POST | `/v1/ownership-relationships/{relationshipID}/rejection` |
| POST | `/v1/ownership-relationships/{relationshipID}/supersession` |
| GET | `/v1/parties` |
| POST | `/v1/parties` |
| GET | `/v1/parties/{partyID}` |
| POST | `/v1/parties/{partyID}/facts:propose` |
| POST | `/v1/parties/{partyID}/identifiers` |
| GET | `/v1/parties/{partyID}/merge-history` |
| POST | `/v1/parties/{partyID}/roles` |
| POST | `/v1/parties/{partyID}/roles/{roleID}/removal` |
| GET | `/v1/resolution-cases` |
| GET | `/v1/resolution-cases/{caseID}` |
| POST | `/v1/resolution-cases/{caseID}/decision` |
| POST | `/v1/resolution/{caseID}:merge` |
| POST | `/v1/resolution/{caseID}:split` |
| POST | `/v1/resolution:match` |
| GET | `/v1/resolve-tenant` |
| POST | `/v1/tenants/{tenantID}/home-region` |
| POST | `/v1/workspaces/{workspaceID}/status` |

## D1-3 · configuration-feature-flag-svc: client calls 6 of 26 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/config/changes` |
| GET | `/v1/config/changes/{change_id}` |
| POST | `/v1/config/changes/{change_id}/activate` |
| POST | `/v1/config/changes/{change_id}/approve` |
| POST | `/v1/config/changes/{change_id}/rollback` |
| POST | `/v1/config/definitions` |
| GET | `/v1/config/definitions/{key}` |
| POST | `/v1/config/definitions/{key}/publish` |
| PUT | `/v1/config/overrides/{scope}` |
| POST | `/v1/config/resolve` |
| GET | `/v1/config/snapshots/{snapshot_id}` |
| POST | `/v1/emergency-changes` |
| GET | `/v1/emergency-changes/{emergency_change_id}` |
| POST | `/v1/emergency-changes/{emergency_change_id}/activate` |
| POST | `/v1/emergency-changes/{emergency_change_id}/retrospective` |
| POST | `/v1/flags/{key}/evaluate` |
| POST | `/v1/flags/{key}/kill-switch` |
| POST | `/v1/flags/{key}/release-plans` |
| POST | `/v1/flags/{key}/removal` |
| POST | `/v1/flags/{key}/retire` |

## D1-4 · secret-vault-integration-svc: client calls 11 of 16 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/secret-policies/{secret_policy_id}/emergency-retrieval` |
| POST | `/v1/secrets/leases/{lease_id}/verify` |
| GET | `/v1/shared-secret-exceptions` |
| POST | `/v1/shared-secret-exceptions` |
| POST | `/v1/shared-secret-exceptions/{exception_id}/revoke` |

## D1-5 · gateway-auth-svc: client calls 0 of 0 screen-relevant routes

Every screen-relevant route is called.

## D1-6 · search-indexer-svc: client calls 16 of 20 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/index-generations/{generationID}/retrieval-evaluations` |
| POST | `/v1/index-generations/{generationID}/retrieval-evaluations` |
| POST | `/v1/retrieve` |
| POST | `/v1/search/semantic` |

## D1-7 · notification-svc: client calls 6 of 91 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/acknowledgments` |
| GET | `/v1/approvals/{id}` |
| POST | `/v1/approvals/{id}/approve` |
| POST | `/v1/approvals/{id}/reject` |
| POST | `/v1/bulk-sends` |
| POST | `/v1/bulk-sends/{id}/dispatch` |
| POST | `/v1/channel-decision` |
| POST | `/v1/communication-intents` |
| GET | `/v1/communication-intents/{id}` |
| POST | `/v1/communication-intents/{id}/activate` |
| POST | `/v1/communication-intents/{id}/retire` |
| GET | `/v1/communication-intents/{id}/templates` |
| POST | `/v1/communication-intents/{id}/versions` |
| POST | `/v1/communications` |
| GET | `/v1/communications/{id}` |
| POST | `/v1/communications/{id}/attempts/{attempt_id}/resolve` |
| POST | `/v1/communications/{id}/cancel` |
| POST | `/v1/communications/{id}/dispatch` |
| POST | `/v1/communications/{id}/misdelivery` |
| POST | `/v1/communications/{id}/prepare` |
| POST | `/v1/communications/{id}/resend` |
| POST | `/v1/document-templates` |
| POST | `/v1/document-templates/versions/{versionID}/approve` |
| POST | `/v1/document-templates/versions/{versionID}/preview` |
| POST | `/v1/document-templates/versions/{versionID}/publish` |
| POST | `/v1/document-templates/versions/{versionID}/validate` |
| GET | `/v1/document-templates/{templateID}` |
| GET | `/v1/document-templates/{templateID}/compare` |
| GET | `/v1/document-templates/{templateID}/locales` |
| GET | `/v1/document-templates/{templateID}/published` |
| POST | `/v1/document-templates/{templateID}/retire` |
| POST | `/v1/document-templates/{templateID}/versions` |
| GET | `/v1/evidence/{id}` |
| GET | `/v1/exceptions` |
| GET | `/v1/intents/{id}/effective` |
| GET | `/v1/notifications/intents/{id}` |
| GET | `/v1/notifications/suppression` |
| POST | `/v1/notifications/suppression` |
| GET | `/v1/notifications/{id}/attempts` |
| POST | `/v1/notifications/{id}/cancel` |
| GET | `/v1/notifications/{id}/delivery-status` |
| GET | `/v1/notifications/{id}/evidence` |
| POST | `/v1/notifications/{id}/resend` |
| POST | `/v1/notifications/{id}/resolve-delivery-outcome` |
| POST | `/v1/preferences` |
| GET | `/v1/preferences/me` |
| GET | `/v1/provider-bindings` |
| POST | `/v1/provider-bindings/{id}/circuit` |
| POST | `/v1/recipient-endpoint-exceptions` |
| POST | `/v1/recipient-resolution` |
| POST | `/v1/regulated-notices` |
| GET | `/v1/regulated-notices/{id}` |
| POST | `/v1/regulated-notices/{id}/manual-evidence` |
| POST | `/v1/regulated-notices/{id}/record-declaration` |
| POST | `/v1/render-previews` |
| GET | `/v1/reputation` |
| POST | `/v1/revalidate` |
| POST | `/v1/streams/{stream}` |
| GET | `/v1/suppressions` |
| POST | `/v1/suppressions` |
| POST | `/v1/suppressions/{id}/lift` |
| POST | `/v1/templates` |
| GET | `/v1/templates/{id}` |
| POST | `/v1/templates/{id}/approve` |
| POST | `/v1/templates/{id}/publish` |
| POST | `/v1/templates/{id}/reject` |
| POST | `/v1/templates/{id}/retire` |
| POST | `/v1/templates/{id}/validate` |
| GET | `/v1/webhook-dead-letters` |
| GET | `/v1/webhook-deliveries/{id}` |
| POST | `/v1/webhook-deliveries/{id}/replay` |
| GET | `/v1/webhook-subscriptions` |
| POST | `/v1/webhook-subscriptions` |
| GET | `/v1/webhook-subscriptions/{id}` |
| PATCH | `/v1/webhook-subscriptions/{id}` |
| POST | `/v1/webhook-subscriptions/{id}/approve` |
| GET | `/v1/webhook-subscriptions/{id}/deliveries` |
| GET | `/v1/webhook-subscriptions/{id}/health` |
| GET | `/v1/webhook-subscriptions/{id}/log` |
| POST | `/v1/webhook-subscriptions/{id}/reject` |
| POST | `/v1/webhook-subscriptions/{id}/resume` |
| POST | `/v1/webhook-subscriptions/{id}/revoke` |
| POST | `/v1/webhook-subscriptions/{id}/rotate-signing-key` |
| POST | `/v1/webhook-subscriptions/{id}/submit` |
| POST | `/v1/webhook-subscriptions/{id}/suspend` |

## D1-8 · delegated-authority-svc: client calls 4 of 9 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/delegations/explain` |
| POST | `/v1/delegations/{delegation_id}/activate` |
| POST | `/v1/delegations/{delegation_id}/extend` |
| POST | `/v1/delegations/{delegation_id}/resume` |
| POST | `/v1/delegations/{delegation_id}/suspend` |

## D1-9 · access-control-svc: client calls 3 of 50 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/access-review-campaigns` |
| POST | `/v1/access-review-campaigns` |
| GET | `/v1/access-review-campaigns/{campaign_id}` |
| POST | `/v1/access-review-campaigns/{campaign_id}/complete` |
| POST | `/v1/access-review-campaigns/{campaign_id}/items/{item_id}/decide` |
| POST | `/v1/access-review-campaigns/{campaign_id}/items/{item_id}/reassign` |
| GET | `/v1/iam/access-assignments` |
| POST | `/v1/iam/access-assignments` |
| GET | `/v1/iam/access-assignments/{request_id}` |
| POST | `/v1/iam/access-assignments/{request_id}/approve` |
| POST | `/v1/iam/access-assignments/{request_id}/reject` |
| POST | `/v1/iam/access-assignments/{request_id}/cancel` |
| POST | `/v1/iam/access-assignments/{request_id}/revoke` |
| POST | `/v1/iam/access-assignments/{request_id}:approve` |
| POST | `/v1/iam/access-assignments/{request_id}:reject` |
| POST | `/v1/iam/access-assignments/{request_id}:cancel` |
| POST | `/v1/iam/access-assignments/{request_id}:revoke` |
| GET | `/v1/iam/access-reviews` |
| GET | `/v1/iam/federation/role-eligibility` |
| GET | `/v1/iam/group-assignments/{group_assignment_id}` |
| POST | `/v1/iam/group-assignments/{group_assignment_id}/revoke` |
| POST | `/v1/iam/group-assignments/{group_assignment_id}/sync` |
| POST | `/v1/iam/group-assignments/{group_assignment_id}:revoke` |
| POST | `/v1/iam/group-assignments/{group_assignment_id}:sync` |
| GET | `/v1/iam/groups` |
| POST | `/v1/iam/groups` |
| GET | `/v1/iam/groups/{group_id}` |
| GET | `/v1/iam/groups/{group_id}/assignments` |
| POST | `/v1/iam/groups/{group_id}/assignments` |
| POST | `/v1/iam/groups/{group_id}/members` |
| POST | `/v1/iam/groups/{group_id}/members/{principal_id}/remove` |
| POST | `/v1/iam/groups/{group_id}/members/{principal_id}:remove` |
| GET | `/v1/iam/subject-links` |
| POST | `/v1/iam/subject-links` |
| GET | `/v1/permission-definitions` |
| GET | `/v1/role-definitions/{role_definition_id}` |
| PATCH | `/v1/role-definitions/{role_definition_id}` |
| GET | `/v1/role-definitions/{role_definition_id}/permission-bundles` |
| POST | `/v1/role-definitions/{role_definition_id}/permission-bundles` |
| DELETE | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` |
| GET | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` |
| PATCH | `/v1/role-definitions/{role_definition_id}/permission-bundles/{bundle_id}` |
| GET | `/v1/role-templates` |
| GET | `/v1/role-templates/{template_code}` |
| POST | `/v1/role-templates/{template_code}/instantiate` |
| POST | `/v1/role-templates/{template_code}/upgrade` |
| GET | `/v1/role-templates/{template_code}/versions` |

## D2-1 · jurisdiction-rules-svc: client calls 12 of 148 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/accounting-mappings:resolve` |
| POST | `/v1/admin/calendars` |
| POST | `/v1/admin/calendars/{calendar_code}/imports` |
| POST | `/v1/admin/calendars/{calendar_code}/versions` |
| POST | `/v1/admin/calendars/{calendar_code}/versions/{version}/publish` |
| PUT | `/v1/admin/calendars/{calendar_code}/versions/{version}/sources` |
| POST | `/v1/admin/deployment-regions` |
| POST | `/v1/admin/deployment-rings` |
| POST | `/v1/admin/interpretations` |
| POST | `/v1/admin/interpretations/{interpretation_id}/approve` |
| GET | `/v1/admin/jurisdiction-imports` |
| POST | `/v1/admin/jurisdiction-imports` |
| GET | `/v1/admin/jurisdiction-imports/{batch_id}` |
| POST | `/v1/admin/jurisdiction-imports/{batch_id}/approve` |
| POST | `/v1/admin/jurisdiction-imports/{batch_id}/reject` |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/aliases` |
| POST | `/v1/admin/jurisdictions/{jurisdiction_id}/aliases/{alias_id}/end` |
| POST | `/v1/admin/obligation-rules` |
| PUT | `/v1/admin/obligation-rules/{obligation_rule_id}/provenance` |
| POST | `/v1/admin/obligation-rules/{obligation_rule_id}/publish` |
| POST | `/v1/admin/pack-signing-keys` |
| POST | `/v1/admin/pack-signing-keys/{key_ref}/retire` |
| POST | `/v1/admin/pack-signing-keys/{key_ref}/revoke` |
| POST | `/v1/admin/packs` |
| POST | `/v1/admin/packs/{pack_ref}/versions` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/block` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/certify` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/compile` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/deploy` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/hotfix` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/hotfix/retrospective` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/publish` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/reviews` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/rollback` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/sign` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/submit-review` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/supersede` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/test-bundle` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/test-runs` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/unblock` |
| POST | `/v1/admin/packs/{pack_ref}/versions/{version}/withdraw` |
| POST | `/v1/admin/payment-terms` |
| POST | `/v1/admin/payment-terms/{term_code}/versions` |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/approve` |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/retire` |
| POST | `/v1/admin/payment-terms/{term_code}/versions/{version}/supersede` |
| POST | `/v1/admin/regimes` |
| GET | `/v1/admin/remediation-cases` |
| POST | `/v1/admin/remediation-cases` |
| GET | `/v1/admin/remediation-cases/{case_id}` |
| POST | `/v1/admin/remediation-cases/{case_id}/close` |
| GET | `/v1/admin/remediation-cases/{case_id}/decisions` |
| POST | `/v1/admin/remediation-cases/{case_id}/dispositions` |
| POST | `/v1/admin/resolver-cache:invalidate` |
| GET | `/v1/admin/rollouts` |
| POST | `/v1/admin/rollouts` |
| GET | `/v1/admin/rollouts/{rollout_id}` |
| POST | `/v1/admin/rollouts/{rollout_id}/attestations` |
| POST | `/v1/admin/rollouts/{rollout_id}/expert-approvals` |
| PUT | `/v1/admin/rollouts/{rollout_id}/owner` |
| PUT | `/v1/admin/rollouts/{rollout_id}/packs` |
| POST | `/v1/admin/rollouts/{rollout_id}/transition` |
| POST | `/v1/admin/rollouts:seed-portfolio` |
| PUT | `/v1/admin/rules/{jurisdiction_rule_id}/parameters` |
| PUT | `/v1/admin/rules/{jurisdiction_rule_id}/provenance` |
| POST | `/v1/admin/source-change-notices` |
| POST | `/v1/admin/source-change-notices/{notice_id}/close` |
| POST | `/v1/admin/source-change-notices/{notice_id}/review` |
| POST | `/v1/admin/sources` |
| POST | `/v1/admin/sources/{source_id}/review` |
| POST | `/v1/admin/sources/{source_id}/supersede` |
| GET | `/v1/calendars` |
| GET | `/v1/calendars/{calendar_code}` |
| GET | `/v1/calendars/{calendar_code}/add-business-days` |
| GET | `/v1/calendars/{calendar_code}/is-business-day` |
| GET | `/v1/calendars/{calendar_code}/versions` |
| GET | `/v1/calendars/{calendar_code}/versions/{version}` |
| GET | `/v1/deployment-regions` |
| GET | `/v1/deployment-rings` |
| GET | `/v1/entities/{legal_entity_id}/jurisdictions` |
| GET | `/v1/interpretations/{interpretation_id}` |
| GET | `/v1/jurisdiction-code-mappings` |
| POST | `/v1/jurisdiction-decisions:resolve` |
| GET | `/v1/jurisdiction-resolutions` |
| GET | `/v1/jurisdiction-support/{jurisdiction_code}` |
| GET | `/v1/jurisdictions/{jurisdiction_id}/aliases` |
| GET | `/v1/jurisdictions/{jurisdiction_id}/as-of` |
| GET | `/v1/jurisdictions/{jurisdiction_id}/hierarchy` |
| POST | `/v1/licences:evaluate` |
| POST | `/v1/obligation-calculations:calculate` |
| GET | `/v1/obligation-rules` |
| GET | `/v1/obligation-rules/{obligation_rule_id}` |
| GET | `/v1/ops/metrics` |
| GET | `/v1/pack-coverage` |
| GET | `/v1/pack-signing-keys` |
| GET | `/v1/packs` |
| GET | `/v1/packs/{pack_ref}` |
| GET | `/v1/packs/{pack_ref}/versions` |
| GET | `/v1/packs/{pack_ref}/versions/{version}` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/artifact` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/certification` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/deployments` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/hotfix` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/reviews` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/test-bundle` |
| GET | `/v1/packs/{pack_ref}/versions/{version}/test-runs` |
| POST | `/v1/packs/{pack_ref}/versions/{version}/verify` |
| GET | `/v1/payment-terms/{term_code}` |
| GET | `/v1/payment-terms/{term_code}/due-date` |
| GET | `/v1/payment-terms/{term_code}/explain-due-date` |
| GET | `/v1/payment-terms/{term_code}/versions` |
| GET | `/v1/payment-terms/{term_code}/versions/{version}` |
| POST | `/v1/payroll-statutory-parameters:resolve` |
| GET | `/v1/regimes` |
| GET | `/v1/regimes/{regime}` |
| POST | `/v1/registration-identifiers:validate` |
| POST | `/v1/registration-obligations:resolve` |
| POST | `/v1/regulatory-calculations:execute` |
| POST | `/v1/regulatory-submissions` |
| GET | `/v1/regulatory-submissions/{submission_id}` |
| POST | `/v1/regulatory-submissions/{submission_id}/events` |
| POST | `/v1/retention-rules:resolve` |
| GET | `/v1/rollout-checklists` |
| GET | `/v1/rule-decisions/{decision_id}` |
| POST | `/v1/rule-resolutions:resolve` |
| GET | `/v1/rules/{jurisdiction_rule_id}/parameters` |
| GET | `/v1/rules/{jurisdiction_rule_id}/provenance` |
| GET | `/v1/rules/{jurisdiction_rule_id}/status-history` |
| POST | `/v1/scheme-elections:evaluate` |
| GET | `/v1/source-change-notices` |
| GET | `/v1/source-change-notices/{notice_id}` |
| GET | `/v1/sources` |
| GET | `/v1/sources/{source_id}` |
| POST | `/v1/submission-profiles:resolve` |
| POST | `/v1/threshold-monitors:evaluate` |
| POST | `/v1/validate-action` |

## D2-2 · governance-decision-log-svc: client calls 2 of 4 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/decisions/{decision_id}/replay` |
| GET | `/v1/decisions/{decision_id}/replay-manifests` |

## D2-3 · policy-svc: client calls 7 of 39 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/accounting-policies` |
| POST | `/v1/accounting-policies` |
| GET | `/v1/accounting-policies/explain` |
| GET | `/v1/accounting-policies/resolve` |
| GET | `/v1/accounting-policies/versions/{version_id}` |
| POST | `/v1/accounting-policies/versions/{version_id}/submit-review` |
| POST | `/v1/accounting-policies/versions/{version_id}/approve` |
| POST | `/v1/accounting-policies/versions/{version_id}/activate` |
| POST | `/v1/accounting-policies/versions/{version_id}/supersede` |
| POST | `/v1/accounting-policies/versions/{version_id}/emergency-suspend` |
| POST | `/v1/accounting-policies/versions/{version_id}/review-suspension` |
| GET | `/v1/accounting-policies/versions/{version_id}/impact` |
| POST | `/v1/attestations` |
| GET | `/v1/attestations/{id}` |
| POST | `/v1/attestations/{id}/revoke` |
| POST | `/v1/control-test-definitions` |
| GET | `/v1/control-test-definitions/{id}` |
| GET | `/v1/control-test-definitions/{id}/executions` |
| POST | `/v1/control-test-definitions/{id}/executions` |
| GET | `/v1/controls/{control_ref}/effectiveness` |
| GET | `/v1/policies/explain` |
| GET | `/v1/policies/resolve` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/approve` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/retire` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/review-suspension` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/schedule` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/submit-review` |
| POST | `/v1/policies/{policy_id}/versions/{version_id}/suspend` |
| GET | `/v1/policy-evaluations/{evidence_id}` |
| GET | `/v1/policy-versions/compare` |
| GET | `/v1/policy-versions/{version_id}` |
| GET | `/v1/policy-versions/{version_id}/transitions` |

## D2-4 · obligations-svc: client calls 7 of 24 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/obligation-definitions` |
| POST | `/v1/obligation-definitions` |
| GET | `/v1/obligation-definitions/{definition_id}` |
| GET | `/v1/obligation-definitions/{definition_id}/entity-mappings` |
| POST | `/v1/obligation-definitions/{definition_id}/entity-mappings` |
| POST | `/v1/obligation-definitions/{definition_id}/entity-mappings/{mapping_id}/end` |
| POST | `/v1/obligation-definitions/{definition_id}/retire` |
| GET | `/v1/obligation-definitions/{definition_id}/versions` |
| POST | `/v1/obligation-definitions/{definition_id}/versions` |
| GET | `/v1/obligations/{obligation_id}/applicability` |
| GET | `/v1/obligations/{obligation_id}/applicability-decisions` |
| POST | `/v1/obligations/{obligation_id}/applicability-decisions` |
| GET | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}` |
| POST | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/status` |
| GET | `/v1/obligations/{obligation_id}/filing-requirements/{filing_requirement_id}/transitions` |
| DELETE | `/v1/obligations/{obligation_id}/workflow` |
| POST | `/v1/obligations/{obligation_id}/workflow` |

## D2-5 · authorization-svc: client calls 64 of 80 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/admin/abac-rules/{abac_rule_id}/reactivate` |
| GET | `/v1/admin/break-glass-sessions/{session_id}/activity` |
| POST | `/v1/admin/break-glass-sessions/{session_id}/review` |
| POST | `/v1/admin/permission-bundles/{permission_bundle_id}/reactivate` |
| GET | `/v1/admin/privileged-sessions/{session_id}/activity` |
| POST | `/v1/admin/privileged-sessions/{session_id}/review` |
| GET | `/v1/admin/role-assignments/as-of` |
| POST | `/v1/admin/role-assignments/{assignment_id}/resume` |
| POST | `/v1/admin/role-assignments/{assignment_id}/suspend` |
| GET | `/v1/admin/role-assignments/{assignment_id}/suspensions` |
| POST | `/v1/admin/roles/{role_id}/reactivate` |
| POST | `/v1/admin/sod-rules/{sod_rule_id}/reactivate` |
| POST | `/v1/admin/workload-bindings/{workload_id}/deactivate` |
| POST | `/v1/admin/workload-bindings/{workload_id}/reactivate` |
| GET | `/v1/support/sessions/{session_id}/activity` |
| POST | `/v1/support/sessions/{session_id}/revoke` |

## D2-6 · workflow-svc: client calls 19 of 76 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/audit/engagements` |
| GET | `/v1/audit/engagements/{engagement_id}` |
| POST | `/v1/audit/engagements/{engagement_id}/acceptance-decision` |
| POST | `/v1/audit/engagements/{engagement_id}/activate` |
| POST | `/v1/audit/engagements/{engagement_id}/amend-scope` |
| POST | `/v1/audit/engagements/{engagement_id}/close` |
| GET | `/v1/audit/engagements/{engagement_id}/completion-gates` |
| POST | `/v1/audit/engagements/{engagement_id}/enter-completion-review` |
| POST | `/v1/audit/engagements/{engagement_id}/mark-fieldwork-complete` |
| POST | `/v1/audit/engagements/{engagement_id}/mark-report-ready` |
| GET | `/v1/audit/engagements/{engagement_id}/plan` |
| POST | `/v1/audit/engagements/{engagement_id}/plan` |
| POST | `/v1/audit/engagements/{engagement_id}/reviews` |
| POST | `/v1/audit/engagements/{engagement_id}/reviews/quality` |
| GET | `/v1/audit/engagements/{engagement_id}/reviews/release-gates` |
| POST | `/v1/audit/engagements/{engagement_id}/submit-acceptance` |
| POST | `/v1/audit/engagements/{engagement_id}/withdraw` |
| POST | `/v1/audit/engagements/{engagement_id}/workpapers` |
| POST | `/v1/audit/plans/{plan_id}/approve` |
| POST | `/v1/audit/plans/{plan_id}/materiality` |
| GET | `/v1/audit/plans/{plan_id}/risks` |
| POST | `/v1/audit/plans/{plan_id}/risks` |
| POST | `/v1/audit/quality-reviews/{quality_review_id}/complete` |
| POST | `/v1/audit/review-notes/{review_note_id}/resolve` |
| POST | `/v1/audit/review-notes/{review_note_id}/respond` |
| POST | `/v1/audit/review-scopes/{review_scope_id}/notes` |
| POST | `/v1/audit/review-scopes/{review_scope_id}/reviewers` |
| POST | `/v1/audit/review-scopes/{review_scope_id}/sign-off` |
| POST | `/v1/audit/risks/{risk_id}/assertions` |
| POST | `/v1/audit/risks/{risk_id}/assess` |
| POST | `/v1/audit/risks/{risk_id}/mark-significant` |
| POST | `/v1/audit/risks/{risk_id}/response` |
| POST | `/v1/audit/sign-offs/{sign_off_id}/withdraw` |
| GET | `/v1/audit/workpapers/{workpaper_id}` |
| POST | `/v1/audit/workpapers/{workpaper_id}/addenda` |
| POST | `/v1/audit/workpapers/{workpaper_id}/conclusions` |
| POST | `/v1/audit/workpapers/{workpaper_id}/cross-references` |
| POST | `/v1/audit/workpapers/{workpaper_id}/evidence-links` |
| POST | `/v1/audit/workpapers/{workpaper_id}/lock` |
| POST | `/v1/audit/workpapers/{workpaper_id}/mark-prepared` |
| POST | `/v1/audit/workpapers/{workpaper_id}/procedures` |
| POST | `/v1/audit/workpapers/{workpaper_id}/results` |
| GET | `/v1/form-submissions/{submission_id}` |
| POST | `/v1/form-submissions/{submission_id}/drafts` |
| POST | `/v1/form-submissions/{submission_id}/route` |
| POST | `/v1/form-submissions/{submission_id}/submit` |
| POST | `/v1/form-submissions/{submission_id}/supersede` |
| POST | `/v1/form-submissions/{submission_id}/validate` |
| GET | `/v1/form-submissions/{submission_id}/validation-result` |
| GET | `/v1/form-submissions/{submission_id}/version` |
| POST | `/v1/forms` |
| GET | `/v1/forms/{form_id}` |
| POST | `/v1/forms/{form_id}/drafts` |
| GET | `/v1/forms/{form_id}/pending-submissions` |
| POST | `/v1/forms/{form_id}/publish` |
| POST | `/v1/forms/{form_id}/retire` |
| GET | `/v1/workflows/{workflow_instance_id}/next-approver` |

## D2-7 · evidence-requirements-svc: client calls 18 of 22 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/pbc/requests/{request_id}/notes` |
| POST | `/v1/pbc/requests/{request_id}/responses` |
| POST | `/v1/pbc/requests/{request_id}/view` |
| POST | `/v1/pbc/responses/{response_id}/acknowledge` |

## D2-8 · schema-registry-svc: client calls 5 of 5 screen-relevant routes

Every screen-relevant route is called.

## D2-9 · retention-registry-svc: client calls 7 of 7 screen-relevant routes

Every screen-relevant route is called.

## D1-10 · mtls-management-svc: client calls 7 of 9 screen-relevant routes

| Method | Path not called yet |
|---|---|
| POST | `/v1/mtls/certificates/csr` |
| GET | `/v1/mtls/crl` |

## D1-11 · siem-integration-svc: client calls 3 of 5 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/siem/exporters/{id}` |
| POST | `/v1/siem/stream` |

## D1-12 · carta-svc: client calls 2 of 3 screen-relevant routes

| Method | Path not called yet |
|---|---|
| GET | `/v1/carta/assessments/{id}` |

## D1-13 · key-management-svc: client calls 5 of 5 screen-relevant routes

Every screen-relevant route is called.

---

# Appendix C · ZoikoSuite explainer: agenda, user journey and what every service does

> Source: `testing_shashi_progress/agenda_&_functionality_of_zs.md` (27 Oct 2026, removed 6 Nov). Kept: sections 1–14, 16 and 17. Removed: "How to use this document" and section 15 (corrections to explanations given in one chat session).

## 1. The motto: what ZoikoSuite is

**Primary objective** (from `docs/architecture/01-backend.md`):

> Ensure every financial, workforce, legal, tax, and compliance action executes inside a
> governed, evidential, jurisdiction-aware control layer.

**Category it claims:** *Governed Business Operations Intelligence.*

**It is not an ERP.** The architecture doc makes this point directly. A normal enterprise system
works like this:

```
Input → Transaction Processing → Storage → Reporting
```

ZoikoSuite works like this:

```
Intent → Policy Validation → Jurisdiction Validation → Execution → Evidence Capture → Intelligence
```

**The core rule:** governance is not a later step, a report, or an audit layer added on top. It is
the *execution boundary*. No material business action completes until the platform has answered:

1. Who is acting, and under what delegated authority?
2. Under which tenant, legal entity and jurisdiction is it happening?
3. Which policy, approval path and control thresholds apply?
4. Which legal, tax, payroll and compliance effects does it trigger?
5. What evidence must be recorded at the moment it executes?

**One-line summary:** governance first, evidence created automatically, audit becomes easy.

---

## 2. Why it is being built

- **Typical software adds governance later.** Most enterprise tools record the transaction first
  and check compliance afterwards (audits, reviews). By then, money may already have moved.
  ZoikoSuite checks before the action and records proof during the action.
- **Audit readiness becomes a by-product.** Evidence is produced as work happens, so audits do not
  need a separate effort to rebuild what happened.
- **Multi-country, multi-company businesses need one control layer.** Finance, HR, payroll, tax,
  legal and compliance rules vary by country. ZoikoSuite applies them in one place instead of
  across separate tools.
- **Lower compliance risk.** No action proceeds without policy, approval, jurisdiction check and
  evidence.
- **Safe AI.** AI may recommend, classify, detect and forecast. It may never bypass policy or
  approval.
- **Build order is foundation first.** Identity, entity, governance, evidence and security are
  built before business domains are scaled (`docs/architecture/06-blueprint.md`: "No
  business-domain expansion may outrun identity, entity, governance, evidence, and security
  foundations").

---

## 3. Who uses it: an organisation, not a single person

ZoikoSuite is built for **organisations**, not for solo use.

- **Multi-tenant:** a *tenant* is one customer organisation. Many organisations share the
  platform, and each one's data is isolated from the others.
- **Multi-entity:** one tenant can hold many *legal entities* (e.g. Acme India, Acme Germany).
- **Multi-jurisdiction:** each entity is tied to a country or region whose rules apply.
- **Role-based:** many people in one organisation use it together, each with their own role.
- **Deployment options:** shared SaaS, or a dedicated database or deployment for regulated
  enterprise customers.

**User groups** (taken from the architecture docs and service names):

| Who | What they do in ZoikoSuite |
|---|---|
| Employees and managers | Submit and approve requests: leave, expenses, purchases |
| Finance teams and controllers | Ledger, month-end close, payments, reconciliation |
| Procurement teams | Purchase requests, purchase orders, supplier onboarding |
| Suppliers | Are paid, checked, onboarded; their bank details are verified |
| HR and payroll teams | Employee records, contracts, payroll runs, benefits |
| Tax teams | Tax calculation, VAT/GST, filings |
| Legal and compliance officers | Contracts, obligations, policies, privacy |
| Treasury | Cash, banking, payment runs |
| Board and executives | Board resolutions, high-value approvals, delegated authority |
| Auditors and regulators | Read evidence trails and decision logs |
| Platform administrators | Run the admin console: roles, flags, secrets, tenants |
| Integration partners | Banks, HR systems, e-signature, tax authorities, via connector services |

---

## 4. The ten design principles (the "agendas")

From `docs/architecture/01-backend.md` §02:

1. **Governance before execution.** Every material operation passes policy, authorization,
   approval and jurisdiction checks first. There is no bypass path.
2. **Evidence by default.** The platform produces unalterable evidence as work happens.
3. **Entity-aware everywhere.** Every action, record, rule and event belongs to a legal entity.
4. **Jurisdiction is first-class.** Country rules drive tax, payroll, employment law, filings,
   retention and approvals at runtime. Jurisdiction is not just a settings field.
5. **Modular capability, unified control.** Business modules can be switched on separately.
   Governance, identity, evidence and observability are always on.
6. **API-first, UI-second.** The UI is only a client of governed services, never a privileged
   shortcut.
7. **AI is policy-constrained.** AI may recommend; it may never override policy or approval.
8. **Multi-tenant, isolatable.** Shared infrastructure, with isolation strong enough for
   regulated customers.
9. **Source truth stays intact.** Analytics and reports never change the original records.
10. **No silent state change.** Every material change must be attributable, evidenced and
    reproducible. Silent overwrites are forbidden.

---

## 5. The seven planes (layers of the architecture)

From `docs/architecture/01-backend.md` §03. Each plane has one job and none is optional.

| Plane | Job | Example services |
|---|---|---|
| Experience | Where users and systems interact: web app, admin console, APIs, notifications | `zoiko-suite-frontend-platform`, notification-svc |
| Domain Execution | Runs the business: finance, HR, payroll, tax, legal, commercial | general-ledger-svc, payroll-run-svc, contract-lifecycle-svc |
| Governance | The non-bypassable control layer: policy, authorization, approvals, jurisdiction | policy-svc, authorization-svc, workflow-svc, jurisdiction-rules-svc |
| Evidence | Audit trails, decision records, document history | governance-decision-log-svc, audit-event-store-svc, evidence-manifest-svc |
| Intelligence | Forecasts, anomaly detection, risk scores; never changes source records | forecasting-svc, anomaly-detection-svc, compliance-risk-scoring-svc |
| Data | Databases, event streams (Kafka), document stores, search | Postgres per service, Kafka, search-indexer-svc |
| Security and Infrastructure | Identity, access, encryption, secrets, networking, deployment | identity-context-svc, gateway-auth-svc, secret-vault-integration-svc, mtls-management-svc |

**Service classes** (`docs/architecture/03-microservices.md`):

| Class | Purpose |
|---|---|
| Governance Platform Services | Shared, non-bypassable control services (the "core moat") |
| Core Domain Services | Finance, payroll, HR, legal, tax, compliance, commercial work |
| Evidence & Audit Services | Audit records, document lineage, workflow history, evidence packs |
| Platform Foundation Services | Identity, tenant/entity context, secrets, configuration, notifications, search |
| Intelligence & Reporting Services | Risk scoring, anomaly detection, forecasting, reporting |
| Integration & Extensibility Services | Safe connections to banks, ERPs, HR systems, filing portals, e-signature |

---

## 6. The canonical runtime control flow

From `docs/architecture/01-backend.md` §04. This is the model for every material action
(payroll release, contract signing, tax filing, board resolution, payment, termination, …).

| Stage | What happens |
|---|---|
| 1. Request intake | A request arrives from UI, API, schedule or event. Every entry path is governed. |
| 2. Identity resolution | Who is acting, which tenant, entity, jurisdiction, role, delegated authority, session trust. |
| 3. Governance evaluation | Policy thresholds, segregation of duties (SoD), approval matrix, jurisdiction obligations, required documents. |
| 4. Execution authorization | Allowed → continue. Not allowed → denied, escalated, or turned into an approval workflow. Silent failure is forbidden. |
| 5. Transaction and event commit | The domain service saves the change and publishes an append-only event. Events are facts, not commands. |
| 6. Evidence generation | Actor, action, entity, jurisdiction, rule applied, approvals, documents, timestamps and provenance are recorded. |
| 7. Intelligence update | Analytics, forecasting and risk services read the events and update insights without changing source records. |

---

## 7. Governance first vs audit first: one worked example

**Scenario.** A clerk wants to pay a supplier 500,000. The company rule says: above 100,000, two
approvers are needed. (The 100,000 / two-approver rule is an illustration, not real policy data
from the repo. Rules like it are stored in policy-svc as `APPROVAL_THRESHOLD` policies.)

### ZoikoSuite: governance first, audit later

1. The clerk submits the payment in the console.
2. **Identity:** the platform confirms who the clerk is, their legal entity and their delegated
   authority (identity-context-svc).
3. **Policy:** policy-svc evaluates the threshold rule: above 100,000 needs two approvers.
4. **Jurisdiction:** the country's rules are applied, e.g. withholding tax or a filing duty
   (jurisdiction-rules-svc).
5. **Authorization:** authorization-svc checks segregation of duties. The clerk cannot approve
   their own payment.
6. **Result:** the payment is **not** released. It becomes an approval workflow waiting for a
   second approver (workflow-svc). No money has moved.
7. The second approver approves. Only now does the payment execute.
8. **Evidence:** who requested, who approved, which rule applied, the amount and the time are
   recorded in the same step (governance-decision-log-svc, audit-event-store-svc).
9. **Later, the auditor** finds a complete record and does not need to rebuild anything.

### Typical outside system: audit first, governance second

1. The clerk pays the invoice directly.
2. Money leaves the bank.
3. Months later, an auditor reviews a sample of payments.
4. The auditor finds that the 500,000 payment had only one approver, against policy.
5. **Result:** the breach has already happened. Money is hard to recall. The company now has an
   audit finding, remediation work, and possibly penalties.

### The difference

| | ZoikoSuite (governance first) | Typical system (audit first) |
|---|---|---|
| When the rule is checked | Before the action | Months after the action |
| Who enforces it | The system, with no bypass | People reviewing afterwards |
| Cost of a mistake | Prevented | Only reported |
| Evidence | Created automatically with every action | Collected later, often incomplete |
| Coverage | 100% of actions | A sample |

---

## 8. The user journey through the system

This is what happens, in order, when a person uses the admin console. File paths point to the
code that does each step.

### Phase A: log in (once per 8 hours)

1. The user opens the console. `proxy.ts` (frontend) looks for the session cookie
   `zoiko_session`. With no cookie, the user is redirected to `/login?redirect=<page>`.
2. The user enters email and password. `app/api/auth/login/route.ts` sends them to
   **identity-context-svc**, which authenticates the user and then resolves their context
   (`/v1/context/resolve`). It returns a **signed IdentityContextEnvelope** (a JWT) that carries
   principal, tenant and legal entity.
3. The console stores the session in the `zoiko_session` cookie for 8 hours
   (`SESSION_MAX_AGE_SECONDS` in `lib/auth.ts`). The envelope itself expires in minutes.
4. **Fallback:** if identity-context-svc is down, or does not know the account, the console falls
   back to local demo accounts in `lib/auth.ts`. That session has **no envelope**. The login
   response says which path was used (`identityProvider`: `identity-context-svc` or
   `local-credentials`).

### Phase B: open the admin page

5. The user opens `/admin`. `app/admin/layout.tsx` reads the cookie for name, email and role.
6. **Only the login cookie is checked here.** authorization-svc is **not** called just to show
   the page.
7. In development, `proxy.ts` can also start the backend services the page needs (on-demand
   launcher, `lib/servicectl`).

### Phase C: open a service page or perform an action

8. The page fetches data **from the server side** (Server Component or Server Action), never
   from the browser. Two reasons: the Go services have no CORS, and backend hostnames and
   identity headers must stay hidden from the browser.
9. The request goes to the **single gateway port** (Traefik, `localhost:8000`):
   `http://localhost:8000/<service-name>/<service's own path>`,
   e.g. `/purchase-order-svc/v1/purchase-orders`. With `ZOIKO_USE_GATEWAY=false`, the console
   calls each service's own port instead.
10. **Gateway check:**
    - **Production:** Traefik's ForwardAuth sends every request first to **gateway-auth-svc**
      (port 8092). It verifies the signed envelope against identity-context-svc's public keys
      (JWKS), checks tenant and entity against tenant-entity-registry-svc, consults carta-svc
      for risk, and injects trusted headers: `X-Principal-Id`, `X-Tenant-Id`,
      `X-Legal-Entity-Id`. Forged or missing identity → the request is refused.
    - **Local development:** the gateway routes in
      `deployments/traefik-dynamic/all-services.yml` carry **no** auth check ("LOCAL DEVELOPMENT
      ONLY … must never be deployed"). The console sends the identity headers itself.
11. **The service receives the request** and reads who the user is from those headers. The IDs
    are UUIDs. A non-UUID value fails inside the Postgres driver and shows up as **503**, not 400.
12. **Read (viewing data):** the database's row-level security (RLS) returns only rows for the
    user's tenant. A read without `X-Tenant-Id` returns 404 or an empty list instead of failing
    loudly.
13. **Write (create, approve, pay):** before saving, the service calls **authorization-svc**
    `POST /v1/authorize`: "may this principal do this action, on this entity, now?" If
    authorization-svc is unreachable, the service refuses (fails closed).
14. **Further governance checks** depending on the action: policy-svc (thresholds, approval
    rules), jurisdiction-rules-svc (country rules), workflow-svc (multi-step approval),
    evidence-requirements-svc (proof needed before completion). The order is decided by each
    service's code; it is not one fixed chain.
15. **Commit:** the service saves the change and writes an event through its transactional outbox
    (same database transaction), which publishes it to Kafka.
16. **Evidence:** governance-decision-log-svc stores the decision and why it was allowed.
    audit-event-store-svc stores the stream of events (what happened, in order).
17. **Notification:** notification-svc sends messages for events that have a notification
    configured, e.g. "Payment P-101 needs your approval". Not every action sends one.

### Flowchart

```mermaid
flowchart TD
    A[User opens console] --> B{Cookie zoiko_session?}
    B -- No --> L[/login/]
    L --> C[Email + password sent to identity-context-svc]
    C --> C2{identity-context-svc accepts?}
    C2 -- Yes --> C3[Signed envelope stored in session cookie]
    C2 -- No / down --> C4[Local demo account fallback, no envelope]
    C3 --> D
    C4 --> D
    B -- Yes --> D[/admin loads: reads name, role from cookie/]
    D --> F[User opens a service page or clicks an action]
    F --> G[Server Component / Server Action calls backend]
    G --> H[Gateway :8000 /service-name/path]
    H --> I{Production?}
    I -- Yes --> J[gateway-auth-svc verifies envelope, checks tenant, injects identity headers]
    I -- No, local dev --> K[Routed straight through, console supplies headers]
    J --> M[Target service]
    K --> M
    M --> N{Read or write?}
    N -- Read --> R[Query filtered by RLS on X-Tenant-Id]
    R --> Z[Response to console]
    N -- Write --> P[authorization-svc POST /v1/authorize]
    P --> Q{Allowed?}
    Q -- No --> X[Refused, fail closed]
    Q -- Yes --> S[policy-svc / jurisdiction-rules-svc / workflow-svc / evidence-requirements-svc as needed]
    S --> T[Save + event via outbox to Kafka]
    T --> U[governance-decision-log-svc + audit-event-store-svc record it]
    U --> V[notification-svc tells people if configured]
    V --> Z
```

### The three checks in one line each

- **Login cookie:** are you logged in?
- **Gateway + gateway-auth-svc:** is this request genuine, and who sent it?
- **Service + authorization-svc:** are you allowed to change this?

### Common misunderstandings (corrected during the session)

| Misunderstanding | What the code actually does |
|---|---|
| "`/admin` is checked by authorization-svc" | `/admin` checks only the session cookie. authorization-svc is called by backend services before writes. |
| "The gateway decides read / write / no-view" | The gateway (gateway-auth-svc) only proves the request is genuine and attaches identity headers. Read scoping is done by database RLS; write permission by authorization-svc. |
| "`zoiko_session` is a token the user types" | It is a cookie the console sets after login. It holds the session and, when available, the signed envelope. |
| "Policy and jurisdiction always run after authorization in a fixed order" | Each service decides its own order. Some check jurisdiction before authorization. |
| "Every action sends a notification" | Only events that have a configured notification do. |

---

## 9. The two record keepers: decision log vs audit event store

Both are part of the Evidence plane, but they answer different questions.

| | governance-decision-log-svc (port 8083) | audit-event-store-svc (port 8084) |
|---|---|---|
| Question it answers | **Why** was this allowed? | **What** happened, and in what order? |
| What it stores | Governance decisions: verdict, rule applied, approvers, time | Every event published on Kafka |
| How data arrives | Services call `POST /v1/decisions` | It consumes Kafka topics (bad messages go to `<topic>.dlq`) |
| Can it be changed? | No, append-only and tenant-owned | Append-only history |
| Does it decide anything? | No. Policy, authorization and workflow decide; it only records | No |

**Example.** Payment P-101 for 500,000 needs two approvers.

- 10:01, Anna approves → audit-event-store-svc records `payment.approval.recorded` by Anna.
- 10:02, Ben approves; the rule is now satisfied →
  - governance-decision-log-svc records: *"P-101 APPROVED. Rule DUAL-APPROVAL-100K.
    Approvers: Anna, Ben. 10:02."*
  - audit-event-store-svc records `payment.approved` at 10:02.
- 10:03, payment sent → audit-event-store-svc records `payment.released`.

"Why was P-101 paid?" → the decision log. "What happened to P-101, step by step?" → the audit
event store. (Event names above are illustrations.)

---

## 10. Who sets the rules, and obligations vs workflow

### Rules live in policy-svc

policy-svc (port 8085) stores policies and policy versions, including `APPROVAL_THRESHOLD`
rules, and evaluates them at `POST /v1/policies/evaluate`. A person with the right permission
creates or changes a rule through the policy-svc API or the admin console. Services then call it
to decide whether, for example, one approval is enough.

### Obligations vs workflow

| | obligations-svc (port 8088) | workflow-svc (port 8090) |
|---|---|---|
| What it is | A list of duties that must be done by a deadline | The approval engine that routes an item through sign-off steps |
| What it tracks | The duty, its state (open, done, overdue), due date, legal entity, linked workflow | Who has signed off so far, and who is next |
| Rule | Jurisdiction-bound; validated against jurisdiction-rules-svc, fail closed | Extends authorization, never replaces it: each approver must still be allowed |
| Example | "File the VAT return for Acme Germany by 31 January" | "Payment goes Clerk → Manager → Director" |

**Together:** the VAT return is an obligation. Filing it needs approval, so a workflow runs before
the obligation is marked done.

Note: obligation-tracking-svc (port 8121, a teammate's service) overlaps with obligations-svc.
The 23 Oct plan decided obligations-svc is the owner of obligations.

---

## 11. How to reach any service (development)

- **Through the gateway:** `http://localhost:8000/<service-name>/<path>`
  (port set by `GATEWAY_PORT`; prefixes are the full service name, generated into
  `deployments/traefik-dynamic/all-services.yml`; index in `deployments/traefik-dynamic/ROUTES.md`).
- **Direct:** `http://localhost:<port>/<path>`. Useful when the gateway itself is suspect.
- **Health:** `/healthz` or `/health`, depending on the service.
- **A wrong prefix** gives a Traefik 404 that looks exactly like the service being down.
- **Start only what you need:** `docker compose -f deployments/docker-compose.yml up -d --build <svc>`.
  Do not start the whole ~90-service file.
- **Identity headers** on every call: `X-Principal-Id`, `X-Tenant-Id`, `X-Legal-Entity-Id`
  (UUIDs). In production gateway-auth-svc sets them; local routes trust what the caller sends.

---

## 12. Domain 1: Identity, Scope & Foundation (9 services)

Source: this file, Reference Part D (Group 1) and each
service's spec. These services answer "who are you, which organisation are you in, and what is
switched on?"

### 12.1 identity-context-svc · port 8080
- **Does:** resolves the authenticated principal, tenant, active legal entity, role profile,
  delegated authority and session trust for every request. Issues and verifies the signed
  identity tokens (envelopes). Has MFA, OIDC and federation packages.
- **Access:** `localhost:8000/identity-context-svc/...`; e.g. `/v1/context/resolve`,
  `/v1/context/session/{id}`.
- **Used when:** at login, and whenever any request needs to know who the user is.
- **Example:** Ravi logs in. The service confirms: Ravi, tenant Acme, entity Acme India, role
  "AP Clerk", and hands back a signed envelope.

### 12.2 tenant-entity-registry-svc · port 8081
- **Does:** master register of tenants, legal entities, entity hierarchies, jurisdiction
  assignments, data-residency policies and residency regions. Uses Postgres row-level security
  for tenant isolation.
- **Access:** `localhost:8000/tenant-entity-registry-svc/...`
- **Used when:** a company or legal entity is created or changed; when gateway-auth-svc checks
  that a tenant may transact and that the entity in the token belongs to it.
- **Example:** Acme adds "Acme Germany" under Acme Group and assigns it the Germany jurisdiction.

### 12.3 configuration-feature-flag-svc · port 8086
- **Does:** versioned runtime configuration and environment-aware feature flags.
- **Access:** `localhost:8000/configuration-feature-flag-svc/v1/config`, `/v1/flags`
- **Used when:** a feature is turned on or off, or a setting changes.
- **Example:** the "new invoice screen" flag is on for the test tenant only.

### 12.4 secret-vault-integration-svc · port 8087
- **Does:** policy-gated broker for secrets. Workloads never hold long-lived credentials; they
  ask this service, which grants a short lease and supports rotation. Version 1 uses a local
  encrypted-file backend; a real Vault/KMS client is pending.
- **Access:** `localhost:8000/secret-vault-integration-svc/v1/secrets/broker`
- **Used when:** a service needs a database or API secret at runtime.
- **Example:** the payment service asks for its bank API key and gets it for one hour only.

### 12.5 gateway-auth-svc · port 8092
- **Does:** Traefik's ForwardAuth target. Every gated request is sent here first; a 2xx lets it
  through, anything else is returned to the caller. Verifies the signed envelope against
  identity-context-svc's JWKS, resolves tenant context via tenant-entity-registry-svc, consults
  carta-svc (risk) and siem-integration-svc (security events), and strips forged identity
  headers.
- **Access:** called by Traefik, not by users. Direct: `localhost:8092`.
- **Used when:** on every production request, before it reaches any service.
- **Example:** a request that sets its own `X-Tenant-Id` to another company's ID is stopped here.

### 12.6 search-indexer-svc · port 8096
- **Does:** builds and maintains the enterprise search index (spec ESR-01…05). Before showing a
  result it confirms through authorization-svc that the user may see it.
- **Access:** `localhost:8000/search-indexer-svc/...`
- **Used when:** data changes (index update) and when someone searches.
- **Example:** a new supplier record becomes searchable, but only for users allowed to see it.

### 12.7 notification-svc · port 8133
- **Does:** notification and communication delivery (spec NCD-01…05). Tracks real delivery
  states (`CREATED`, `SUBMITTED_TO_PROVIDER`, `PROVIDER_ACCEPTED`, …); it never reports a
  generic "sent". Checks authorization-svc and fails closed.
- **Access:** `localhost:8000/notification-svc/...`
- **Used when:** an event needs to tell a person something.
- **Example:** "Payment P-101 needs your approval" sent to Ben.

### 12.8 delegated-authority-svc · port 8136
- **Does:** time-bound, scope-bound delegated authority chains. authorization-svc intersects a
  delegation with the delegator's live grants, so a delegate can never exceed the delegator.
- **Access:** `localhost:8000/delegated-authority-svc/...`
- **Used when:** someone is away and another person must approve for them.
- **Example:** Priya delegates her approvals to Ben for three days, up to 50,000 each.

### 12.9 access-control-svc · port 8137
- **Does:** role catalogues and permission bundles, and provisions them into authorization-svc.
  Runs access reviews (periodic checks of who holds which role). Every action a handler checks
  must be registered here as a permission definition, or it can never be granted.
- **Access:** `localhost:8000/access-control-svc/...`
- **Used when:** admins create roles, assign roles, or run access reviews.
- **Example:** create the role "Payment Approver" with the payment-approve permission and assign
  it to Ben for Acme Germany.

---

## 13. Domain 2: Governance Platform, "the non-bypassable spine" (7 services)

Source: this file, Reference Part E (Group 2) and each service's
spec. These services answer "is this allowed, under which rules, who must approve, and what proof
is needed?"

### 13.1 jurisdiction-rules-svc · port 8082
- **Does:** the authoritative registry of jurisdictions and their effective-dated rules.
  `/rule-pack` resolves a jurisdiction's whole parent chain to one winning rule per rule code and
  names where it came from. `GET /v1/jurisdictions/{id}` is the fail-closed check other services
  make before saving a `jurisdiction_id`: **503 means "cannot answer", never "no"**. Also tracks
  legal drift (`legal.drift.detected`). Nothing is deleted: rules are end-dated, so past dates
  still answer correctly. Reads are public; writes need authorization-svc.
- **Access:** `localhost:8000/jurisdiction-rules-svc/v1/jurisdictions`, `/v1/rules`,
  `POST /v1/validate-action`, `POST /v1/regulatory-calculations:execute`
- **Used when:** an action depends on a country's rules, or a jurisdiction ID is saved.
- **Example:** a German invoice is checked against the German VAT rule valid on the invoice date.

### 13.2 governance-decision-log-svc · port 8083
- **Does:** the append-only ledger of governance decisions. Every decision is tenant-owned,
  immutable once written, and queryable by actor and other fields. It records decisions; it does
  not make them. A "Tier 0, platform survival" service.
- **Access:** `POST` / `GET localhost:8000/governance-decision-log-svc/v1/decisions`
- **Used when:** policy, authorization or workflow reaches a decision.
- **Example:** "Payment P-101 APPROVED under rule X by Anna and Ben at 10:02."

### 13.3 policy-svc · port 8085
- **Does:** stores policies and policy versions, and evaluates them (e.g. `APPROVAL_THRESHOLD`).
  Policies carry a `jurisdiction_context`.
- **Access:** `localhost:8000/policy-svc/v1/policies`, `POST /v1/policies/evaluate`
- **Used when:** an action may need approval or must respect a limit.
- **Example:** "Above 100,000 needs two approvers" is evaluated for a 500,000 payment.

### 13.4 obligations-svc · port 8088
- **Does:** owns obligation definitions, obligation state, due dates, and links from an
  obligation to its entity and its workflow. Jurisdiction-bound, with fail-closed validation
  against jurisdiction-rules-svc.
- **Access:** `localhost:8000/obligations-svc/v1/obligations`
- **Used when:** a legal or filing duty is created, changes state, or falls due.
- **Example:** "VAT return for Acme Germany, due 31 January", open until filed.

### 13.5 authorization-svc · port 8089
- **Does:** the runtime access-decision engine (policy decision point). `POST /v1/authorize`
  evaluates six layers in order, and every layer after the first can only **remove** what an
  earlier one allowed. Layer 0: principal status (a suspended or disabled user can do nothing).
  Layer 1: role-based access (roles → permission bundles → effective-dated, entity-scoped
  assignments). Layer 2: delegated authority, intersected with the delegator's live grants.
  Layer 3: static segregation of duties (conflicting roles). Layer 4: own-object segregation of
  duties (you cannot approve your own request). Layer 5: attribute-based rules (ABAC). Also holds
  authority limits and break-glass approvals.
- **Access:** `POST localhost:8000/authorization-svc/v1/authorize`
- **Used when:** before every material write, called by the service doing the write.
- **Example:** clerk Ravi tries to approve his own payment → DENY (segregation of duties).

### 13.6 workflow-svc · port 8090
- **Does:** approval workflows that extend authorization and never replace it. Routes an item
  through approval steps and records each one.
- **Access:** `localhost:8000/workflow-svc/...`
- **Used when:** an item needs more than one sign-off.
- **Example:** a payment goes Clerk → Manager → Director; each step waits for the right person.

### 13.7 evidence-requirements-svc · port 8130
- **Does:** decides what evidence must exist before an action may complete, and records each
  decision in an append-only ledger.
- **Access:** `localhost:8000/evidence-requirements-svc/v1/...`
- **Used when:** an action is about to complete and proof is required first.
- **Example:** "The payment cannot be released until the invoice PDF is attached."

### 13.8 One payment through both domains

1. **identity-context-svc:** who is Ravi? (Domain 1)
2. **gateway-auth-svc:** is the request genuine? (Domain 1)
3. **jurisdiction-rules-svc:** which German rule applies? (Domain 2)
4. **policy-svc:** two approvers needed. (Domain 2)
5. **authorization-svc:** may Ravi do this? ALLOW or DENY. (Domain 2)
6. **workflow-svc:** route to the second approver. (Domain 2)
7. **evidence-requirements-svc:** is the invoice attached? (Domain 2)
8. **governance-decision-log-svc:** record the decision. (Domain 2)
9. **notification-svc:** tell the next approver. (Domain 1)

---

## 14. Catalogue of all 121 services

The `services/` folder has 126 folders. Five are not services:

- `eventing`, `identity-envelope`, `search-client`: shared Go libraries every service imports
  (event publishing, the signed identity envelope and the API contract helpers, search client).
- `_certification`: tool that runs the cited tests and writes a release certificate.
- `_contract`: shared contract artefacts.

**How to read the table.** "Kid example" explains the service as you would to a five-year-old,
with the software term in brackets. Port is from `deployments/traefik-dynamic/ROUTES.md`; "—"
means the service is not in the gateway route index. Src: **S** = read from the service's own
docs or code, **N** = inferred from the name.

### 14.1 Front door, identity and security

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| identity-context-svc | 8080 | Resolves who you are, your tenant, entity, roles; issues signed envelopes | The school register with your name and class (identity provider) | Login, every request | S |
| gateway-auth-svc | 8092 | Verifies every request's signed envelope before Traefik forwards it | The gate guard checking your ticket (ForwardAuth) | Every production request | S |
| authorization-svc | 8089 | Decides "may this person do this, here, now?" | The teacher saying yes or no to a hall pass (policy decision point) | Before every write | S |
| access-control-svc | 8137 | Defines roles and permission bundles, loads them into authorization-svc, runs access reviews | The list of which kids may use which room (RBAC catalogue) | Role created or assigned | S |
| delegated-authority-svc | 8136 | Time- and scope-limited delegation of authority | "Priya signs for me this week" (delegation chain) | Someone is away | S |
| tenant-entity-registry-svc | 8081 | Tenants, legal entities, hierarchies, jurisdictions, data residency | The school board's list of schools (master registry) | Company created or checked | S |
| configuration-feature-flag-svc | 8086 | Versioned settings and feature flags | A light switch for a new feature (feature flag) | Feature turned on/off | S |
| secret-vault-integration-svc | 8087 | Leases secrets to allowed workloads, rotation-aware | A locker that gives a key for one hour (secret broker) | A service needs a password | S |
| mtls-management-svc | 8140 | Internal certificate authority for service-to-service mTLS, incl. revocation | Every school bus carries a registered pass (certificate authority) | Services connect securely | S |
| key-management-svc | — | Key records and metadata; today it does not encrypt or decrypt itself (key custody is planned work, SEC-1) | A logbook of which key opens which lock (key registry) | Keys created, rotated, listed | S |
| carta-svc | 8142 | Continuous Adaptive Risk and Trust Assessment: answers `ALLOW`, `STEP_UP_MFA`, `ISOLATE` or `DENY` for a request | A guard who asks for a second ID when something looks odd (risk-based access) | gateway-auth-svc checks a request | S |
| siem-integration-svc | 8141* | Sends security events to a security monitoring system (SIEM) | A camera feed to the guard room (security event forwarding) | Security-relevant event happens | N |
| kill-switch-registry-svc | 8147 | Privileged, approval-controlled, logged emergency stops scoped by plane, domain, provider or tenant | A fire alarm that stops the lift (kill switch) | Something must be stopped fast | S |

\* 8141 is from `deployments/docker-compose.yml`, not the gateway index.

### 14.2 Governance and rules

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| policy-svc | 8085 | Stores policies and evaluates them (e.g. approval thresholds) | "Over 100, two teachers must sign" (rules engine) | An action needs a rule check | S |
| jurisdiction-rules-svc | 8082 | Jurisdictions and their effective-dated rules | Each country's rule book (rule registry) | Country rules matter | S |
| workflow-svc | 8090 | Multi-step approval workflows | A form passed from teacher to principal (workflow engine) | Several sign-offs needed | S |
| obligations-svc | 8088 | Duties with due dates, tied to entity and jurisdiction | A homework list with deadlines (obligation register) | A duty is created or due | S |
| evidence-requirements-svc | 8130 | Decides what proof must exist before an action completes | "Bring your signed slip before the trip" (evidence gate) | An action is about to finish | S |
| governance-decision-log-svc | 8083 | Append-only record of governance decisions | A diary of who said yes to what (decision ledger) | A decision is made | S |
| schema-registry-svc | 8093 | Canonical registry of event message formats | A form template every message must follow (schema registry) | Services publish or read events | S |
| retention-registry-svc | 8148 | How long records are kept; planned owner of legal holds (GOV-09/10) | "Keep tax papers 7 years" (retention policy) | Records reviewed for keep/delete | N |
| capability-registry-svc | 8145 | Capability, module and release registry: whether a capability exists, is released and is available | The shop menu showing which dishes are ready (capability catalogue) | A module is checked or released | S |
| commercial-account-svc | 8144 | The verified customer that buys ZoikoSuite, its users, subscriptions and renewals | A gym membership card that renews (subscription/account) | A customer signs up or renews | S |
| source-authority-svc | 8150 | Says which connected system's value to trust for a field, without guessing when sources disagree | "For your address, trust the school office" (source of truth) | Two systems disagree | S |

### 14.3 Evidence, audit and records

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| audit-event-store-svc | 8084 | Consumes Kafka events and keeps the history; dead-letters bad messages | A CCTV log of who did what, when (event store) | Any event is published | S |
| evidence-manifest-svc | 8095 | Assembles evidence sets from decision log, authorization and workflow for audits; fails whole, never partial | A folder labelled "Audit file for P-101" (evidence bundle) | An auditor or regulator asks | S |
| document-vault-svc | 8094 | Secure document storage with authorization checks | A fireproof safe for papers (document store) | A document is stored or fetched | S |
| workflow-history-svc | 8097 | History of approval steps | A diary of who signed which page (audit trail) | Someone asks how an approval went | N |
| comments-collaboration-svc | — | Comments and collaboration on business records (BIZ-09) | Sticky notes on a worksheet (comments) | Someone comments on an item | S |
| migration-integrity-svc | — | Checks records being moved in from old systems (required fields, duplicates, number formats) and keeps an audit trail; customer data cutover, not database migrations | Checking every box before moving house (data migration validation) | A customer migrates data in | S |
| global-search-svc | — | Search across the platform | Google for your own company (search) | Someone searches | N |

### 14.4 Reference data

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| currency-registry-svc | — | ISO 4217 currencies, minor units, support lifecycle (REF-02) | A chart saying 1 dollar = 100 cents (reference data) | Money is entered or shown | S |
| fiscal-calendar-svc | — | Entity-specific financial calendars (REF-04) | A calendar where the year starts in April (fiscal calendar) | Financial periods are set | S |
| accounting-period-svc | — | Period instances, period state and the posting gate (REF-05) | A locked diary for last month (period close) | A month is opened or closed | S |
| org-structure-svc | 8116 | Organisation units and reporting lines | A family tree of managers (org chart) | Reporting lines change | N |
| counterparty-management-svc | 8124 | Records of business partners | An address book of companies (party master) | A partner is added | N |

### 14.5 Finance: record to report

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| general-ledger-svc | 8098 | The main accounting book | The shop's master notebook (ledger) | Any money posting | N |
| accounts-payable-svc | 8099 | Money the company owes suppliers | "I owe Ram 500" (payables) | Supplier bill entered | N |
| accounts-receivable-svc | 8101 | Money customers owe; includes the customer financial profile (AR-01) | "Customer X owes us 300" (receivables) | Customer invoiced | S |
| financial-close-svc | 8104 | Steps to close a month or year | The end-of-month checklist (close management) | Period close | N |
| consolidation-svc | 8106 | Adds many companies into one group result | Adding all branches into one total (consolidation) | Group reports | N |
| intercompany-accounting-svc | 8105 | Transactions between companies in one group (ACC-11) | One sibling lending pocket money to another (intercompany) | Group companies trade | S |
| financial-statements-svc | — | Profit and loss, balance sheet (REP-01) | A money report card (financial statements) | Reports produced | S |
| financial-control-svc | — | Verifies financial truth with control checks; never posts or edits accounting | A checker who counts the till but never touches the money (control testing) | Controls are run | S |
| project-accounting-svc | — | Money per project | A box per project showing its spend (project costing) | Project costs posted | N |
| asset-management-svc | — | Company assets and their value | A list of desks and their age (fixed assets) | Assets bought or sold | N |
| inventory-management-svc | — | Stock levels | Counting items on the shelf (inventory) | Goods in or out | N |
| corporate-actions-svc | 8123 | Shareholder events such as dividends or share changes | Sharing a cake among owners (corporate actions) | A share event happens | N |
| reporting-orchestration-svc | — | Runs and schedules reports; reads archives from audit-event-store-svc | A printer queue that prints at set times (report scheduler) | A report is requested | S |

### 14.6 Banking, payments and treasury

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| treasury-svc | 8103 | Cash position and planning | The family piggy-bank plan (cash management) | Cash checked or planned | N |
| banking-connector-svc | — | Connection to external banks | A phone line to the bank (bank integration) | Bank data fetched or sent | N |
| bank-reconciliation-svc | 8102 | Matches bank statements to company records | Ticking each bank line in your notebook (reconciliation) | Statement loaded | N |
| reconciliation-engine-svc | — | General matching of two sets of records | Matching socks from two piles (matching engine) | Records must be matched | N |
| reconciliation-intelligence-svc | — | Reads bank reconciliation runs and their unmatched lines, and helps resolve them | "These three lines are probably the same payment" (match suggestions) | Matching leaves gaps | S |
| payable-open-item-svc | — | Authoritative unpaid supplier and expense liabilities, due dates, holds, credits (AP-08) | The "to pay" pile (open items) | Payments are planned | S |
| payment-proposal-svc | — | Proposes which items to pay | Picking which bills fit this week (payment proposal) | Payment run prepared | N |
| payment-authorization-svc | — | Approves payments before they go out | A manager's signature on a cheque (payment approval) | A payment needs approval | N |
| payment-run-svc | — | Executes a batch of payments; sends settlement postings to accounting | Paying all bills together on Friday (payment batch) | Payment run executes | S |
| payment-initiation-adapter-svc | — | Sends payment instructions to the bank | A courier taking the cheque to the bank (payment adapter) | A payment is sent | N |
| payment-status-svc | — | Tracks whether a payment succeeded | A parcel tracking page (status tracking) | Someone checks a payment | N |
| payee-banking-identity-svc | — | Verifies payee bank details | Checking the address before posting a letter (bank account verification) | Bank details added or changed | N |

### 14.7 Procurement and accounts payable

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| purchase-request-svc | 8100 | Staff ask to buy something | "I need five markers" (purchase requisition) | Someone needs something | N |
| purchase-order-svc | 8129 | Formal order to a supplier; checks authorization before writes | The order slip sent to the shop (purchase order) | A request is approved | S |
| procurement-workflow-svc | 8134 | Approval path for buying | Request goes to boss, then finance (procurement workflow) | A purchase is requested | N |
| goods-service-receipt-svc | — | Records what was received; sends goods-received-not-invoiced postings to accounting | Ticking items off the delivery note (goods receipt) | Goods arrive | S |
| invoice-approval-svc | 8107 | Checks and approves supplier invoices | A teacher checking a receipt before paying (invoice approval) | An invoice arrives | N |
| expense-claim-svc | — | Employee expense claims; sends accounting postings | Handing in your bus ticket to get money back (expense claim) | An expense is claimed | S |
| spend-controls-svc | 8131 | Spending limits | A pocket-money limit (spend control) | Someone tries to spend | N |
| supplier-financial-profile-svc | — | Supplier payment terms and financial details | A card with the shopkeeper's terms (supplier profile) | Supplier set up | N |
| supplier-recovery-svc | — | Recovers supplier overpayments, duplicates and credits via refund or offset (AP-12) | Getting back extra change you were given (recovery) | A supplier owes money back | S |
| vendor-due-diligence-svc | 8135 | Background checks on suppliers | Reading a shop's reviews first (due diligence) | A new supplier is added | N |

### 14.8 Workforce: HR and payroll

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| employee-master-svc | 8108 | The authoritative employee record (WFP-01) | A student profile card (employee master) | Someone joins or changes | S |
| employment-contracts-svc | 8109 | Employment contracts | The signed appointment letter (contract record) | A contract is created | N |
| compensation-svc | 8111 | Salary and pay structure | The pay-slip template (compensation) | Pay is set or changed | N |
| benefits-svc | 8112 | Employee benefits such as insurance | A free-lunch card (benefits) | Benefits enrolment | N |
| leave-absence-svc | 8115 | Leave and absence requests | Asking for days off on a calendar (leave management) | Someone requests leave | N |
| payroll-run-svc | 8110 | Calculates and runs payroll; dispatches postings to accounting | Handing out pocket money on Friday (payroll run) | Payroll cycle | S |
| payroll-tax-svc | 8113 | Tax on salaries | Taking the tax slice out of pay (payroll tax) | Payroll runs | N |
| payroll-exceptions-svc | 8114 | Flags unusual payroll cases | Noticing one name paid twice (exception handling) | Pay looks wrong | N |
| performance-review-svc | 8139 | Performance reviews | A report card for work (appraisal) | Review is due | N |
| offboarding-severance-svc | 8117 | Leaving process and final pay | Returning your ID card and getting final pay (offboarding) | Someone leaves | N |
| workforce-compliance-svc | 8118 | Checks workforce rules are followed | Checking everyone has the right permit (workforce compliance) | Staff rules reviewed | N |
| hris-connector-svc | — | Connects to external HR systems | A bridge between two class registers (HRIS integration) | HR data syncs | N |

### 14.9 Tax

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| tax-rules-svc | 8125 | Tax rules | The tax chart (tax rule store) | Tax is calculated | N |
| tax-determination-svc | 8126 | Works out which tax applies to a transaction | "This sale needs 18%" (tax engine) | A sale or purchase | N |
| vat-gst-svc | 8127 | VAT and GST handling | Adding sales tax to a bill (indirect tax) | A VAT/GST transaction | N |
| withholding-tax-svc | — | Tax held back from payments | Keeping a slice of a freelancer's fee for tax (withholding) | A supplier is paid | N |
| corporate-tax-svc | 8128 | The company's income tax | The school's yearly tax bill (corporate tax) | Year-end tax | N |
| filing-preparation-svc | — | Prepares tax and statutory filings | Filling in a form before sending it (filing prep) | A filing is due | N |
| filing-tracker-svc | — | Filing deadlines and status | A homework tracker for tax forms (filing tracker) | A filing is due | N |
| tax-authority-interface-svc | — | Live adapter that submits to tax authorities (INT-08) | A post box to the tax office (government API integration) | A filing is submitted | S |

### 14.10 Legal, corporate and compliance

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| contract-lifecycle-svc | 8119 | Contracts from draft to expiry | A letter from first draft to signed copy (CLM) | A contract changes stage | N |
| clause-template-svc | 8120 | Library of standard contract clauses | Ready-made sentences for letters (template library) | A contract is drafted | N |
| board-resolutions-svc | 8122 | Board decisions | Minutes of a meeting (board resolution) | The board decides | N |
| esignature-integration-svc | — | Sends documents for electronic signature | Getting a signature by email (e-signature integration) | A document needs signing | N |
| obligation-tracking-svc | 8121 | Tracks obligations; overlaps obligations-svc (teammate's service) | Ticking off homework (obligation tracking) | A duty is due | N |
| compliance-status-svc | — | Shows whether rules are being met | A traffic light per rule (compliance dashboard) | Compliance is checked | N |
| compliance-risk-scoring-svc | — | Gives areas a risk score | "Playground risk: high" (risk scoring) | Risks are ranked | N |
| exception-escalation-svc | — | Escalates unresolved problems to the right person | "Tell the principal if not fixed in two days" (escalation) | A task is overdue | N |

### 14.11 Privacy

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| privacy-consent-svc | — | Records who consented to what | A signed permission slip (consent record) | Consent given or withdrawn | S |
| privacy-purpose-registry-svc | — | Approved purposes for using personal data | Labelled boxes: "Payroll", "Marketing" (purpose registry) | A purpose is chosen | S |
| privacy-decision-svc | — | Decides whether personal data may be used; reads the consent registry | "May we email this person? Only if they said yes" (privacy decision) | Personal data is used | S |
| privacy-rights-svc | — | Handles requests like "delete my data" or "give me a copy" | A customer asks for their file (data subject request) | A person exercises a right | S |
| privacy-transfer-svc | — | Rules for moving personal data across borders | "Can this file leave the country?" (cross-border transfer) | Data is sent abroad | S |

### 14.12 Data and integration

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| data-ingestion-svc | — | Data intake (DATA-01) and controlled bulk import (INT-06) | Pouring water into a jug (ETL / import) | A data file is imported | S |
| data-quality-svc | — | Finds wrong or missing data | Spotting a missing name on the list (data quality) | Data loaded or reviewed | N |
| data-lineage-svc | — | Records that one item was derived from another (DATA-03) | "This total came from these three bills" (lineage) | Someone asks where data came from | S |
| external-data-feed-svc | — | Receives outside data feeds | A newspaper delivered each morning (data feed) | A feed arrives | N |
| connectivity-api-bridge-svc | — | Connector management for external systems (ZS-SVC-M-001) | A plug adapter for foreign sockets (integration connector) | An external system is connected | S |
| document-extraction-svc | — | Reads text and numbers from documents | Copying numbers from a bill into a sheet (OCR / extraction) | A scanned document is uploaded | N |

### 14.13 Intelligence, analytics and AI

| Service | Port | What it does | Kid example | Used when | Src |
|---|---|---|---|---|---|
| analytical-data-platform-svc | — | Governed analytical snapshots and datasets; never owns source records (DATA-04) | A photo of the shelf for counting later (data warehouse) | Analysis runs | S |
| semantic-model-svc | — | Governed metrics, dimensions and joins over analytical data (DATA-05) | A dictionary for report words (semantic layer) | Reports use business terms | S |
| metric-registry-svc | 8149 | Defined, versioned, traceable executive metrics (REP-01) | "Revenue = sales minus refunds" (metric definition) | Metrics are defined or shown | S |
| forecasting-svc | — | Predicts future numbers | Guessing next month's sales (forecasting) | A forecast is requested | N |
| anomaly-detection-svc | — | Spots unusual activity | Noticing one spend that is far too big (anomaly detection) | Data is scanned | N |
| decision-support-svc | 8138 | Governed recommendations at approval and oversight points | A coach suggesting the next move (recommendation) | An approver needs advice | S |
| classification-svc | — | Suggests transaction, document and category classifications with evidence and confidence; never holds final authority (AI-02) | Suggesting which box a toy goes in, a grown-up confirms (ML classification) | A transaction or document needs a category | S |
| assistant-rag-svc | — | Answers permitted questions from authorized sources; uses approved tools through normal commands (AI-03) | A helper who reads the handbook before answering (RAG assistant) | A user asks the assistant | S |
| ai-governance-svc | 8146 | Controls how AI models and prompts are used, with human oversight | A supervisor for a robot helper (AI governance) | An AI feature runs | N |

---

## 16. Key files to read

| Topic | File |
|---|---|
| Product doctrine, principles, planes, control flow | `docs/architecture/01-backend.md` |
| Service classes and ownership of objects | `docs/architecture/03-microservices.md` |
| Security architecture | `docs/architecture/05-security.md` |
| Build order and teams | `docs/architecture/06-blueprint.md` |
| Original specifications (`.docx`) | `docs/architecture/original_doc/` |
| Service list with ports (partial) | `services/README.md` |
| Gateway route index | `deployments/traefik-dynamic/ROUTES.md` |
| Domain 1 audit | this file, Reference Part D |
| Domain 2 audit | this file, Reference Part E |
| Feature status and tasks | this file (feature tables) |
| Implementation rules, decisions and task specs | this file, Reference Parts A, B and F |
| Console login | `zoiko-suite-frontend-platform/app/api/auth/login/route.ts` |
| Session and identity | `zoiko-suite-frontend-platform/lib/auth.ts` |
| Login redirect and service launcher | `zoiko-suite-frontend-platform/proxy.ts` |
| How the console reaches the backend | `zoiko-suite-frontend-platform/README.md` |

---

## 17. Glossary

| Term | Meaning |
|---|---|
| Tenant | One customer organisation using ZoikoSuite |
| Legal entity | A registered company inside a tenant (e.g. Acme Germany) |
| Jurisdiction | The country or region whose laws apply to an entity or action |
| Principal | The user or workload making a request |
| Identity envelope | Signed token (JWT) from identity-context-svc carrying principal, tenant and entity |
| Gateway / Traefik | The single entry port that routes `/service-name/...` to each service |
| ForwardAuth | Traefik feature that asks gateway-auth-svc to approve each request first |
| JWKS | Public keys used to verify signed tokens |
| RLS (row-level security) | Postgres feature that returns only the current tenant's rows |
| RBAC / ABAC | Role-based / attribute-based access control |
| SoD (segregation of duties) | One person may not both request and approve the same thing |
| Delegated authority | Temporary permission to act for someone else, within limits |
| Fail closed | If a check cannot be answered, the action is refused rather than allowed |
| Append-only | Records can be added but never changed or deleted |
| Event / Kafka | A published fact ("payment.approved") delivered to other services through Kafka |
| Transactional outbox | Events are saved in the same DB transaction as the change, then published, so none are lost |
| DLQ (dead-letter queue) | Where messages that cannot be processed are put aside |
| Idempotency key | A request ID that makes a retried request safe to repeat |
| RFC 9457 problem details | Standard JSON error format (`application/problem+json`) with a stable `code` |
| Material action | A business action with legal, financial or compliance effect (pay, approve, file, terminate) |
| Evidence | The recorded proof of who did what, under which rule, and when |
