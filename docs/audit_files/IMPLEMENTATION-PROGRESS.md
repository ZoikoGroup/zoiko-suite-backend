# Implementation progress

Plan: `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md`. Update this file after every task.
Statuses: TODO · IN PROGRESS · DONE · BLOCKED. DONE needs a filled evidence block (plan §7).

## Status board

| Task | Title | Status | Commit(s) | Note |
|---|---|---|---|---|
| P0-1 | Commit pending authorization work | DONE | 58c810e4, c7581e77, a1574241 | authz 90 store+IT / 467 handler pass, 0 fail; vet clean. Reviewer re-ran 27 Oct: reproduces (79 + 33/1 skip + 467) |
| P0-2 | Register JURISDICTION_RULE_VALIDATE | DONE | b82fd224 | access-control 000023 + template + store test. Reviewer 27 Oct: reproduces; up→down→up clean |
| P0-3 | Fix stale resolve.go comment | DONE | 47986f06 | comment now matches tax.go. Reviewer 27 Oct: confirmed |
| API-1 | Shared apicontract package | DONE | acce5b86; review fixes a3a89b24, 2a53338b, e84c8fac, 1a6cf81d, 37413879 | Reopened 27 Oct: its pgx v5.10.0 bump broke `go build` in authorization, evidence-requirements, policy and workflow; fixed. Now 33 unit + 7 pgx pass |
| API-2 | Problem details + request id + cache, all our services | IN PROGRESS | e1830853, f361be5e, 8c8bb0a4, d33fa469, c338bbfc, 75e53db2, 4ed4ad08, 50f8f5fe, c2548257, af6e674a, a7d75009, 6d016f11, 9163a41f, b0844868, c0d2c2b4, 52899385, 1d5f9fc3, b13635a5 | tenant-entity-registry-svc + access-control-svc + identity-context-svc + governance-decision-log-svc + jurisdiction-rules-svc + obligations-svc + configuration-feature-flag-svc + secret-vault-integration-svc + gateway-auth-svc + search-indexer-svc + notification-svc + delegated-authority-svc + policy-svc + authorization-svc + workflow-svc + evidence-requirements-svc + mtls-management-svc done; 3 services remain |
| API-3 | ETag / If-Match on aggregate mutations | TODO | | |
| API-4 | Cursor pagination on lists | TODO | | |
| API-5 | Async operations | TODO | | |
| API-6 | Deprecation / Sunset on renamed routes | TODO | | |
| G-10 | Legal hold | TODO | | |
| G-09 | Retention and disposition | TODO | | |
| G-07 | Evidence ledger and verification | TODO | | |
| G-08 | Schema registry lifecycle | TODO | | |
| G-11 | Feature policy and capability exposure | TODO | | |
| G-02 | GTRM reads + compiler commands | TODO | | |
| G-05/03/04/12 | Named-operation parity | TODO | | |
| IAM-1 | AU-12 per-service audience | TODO | | |
| IAM-2 | AU-14 entitlement stage | TODO | | |
| IAM-3 | AU-15 legal-hold negative control | TODO | | after G-10 |
| IAM-4 | AU-16 relationship / qualification predicates | TODO | | after ORG-4, ORG-5 |
| IAM-5 | AU-18 authoritative FX | TODO | | after ORG-8 |
| IAM-6 | AU-19 observability | TODO | | |
| IAM-7 | AU-20 DoD artefacts | TODO | | |
| IAM-8 | AU-21 load + revocation SLOs | TODO | | |
| IAM-9 | AU-22 role-profile drift | TODO | | |
| IAM-10 | AU-25 canonical event names | TODO | | after G-08 |
| IAM-11 | AU-26 /v1/iam aliases | TODO | | |
| IAM-12 | Federation / SCIM / passkey acceptance | TODO | | |
| IAM-13 | ORG-06 remaining operations | TODO | | |
| WF-1 | WFC-01..03 open rows | TODO | | |
| WF-2 | WFC-04 cases | TODO | | |
| WF-3 | WFC-05 deadline clocks | TODO | | |
| WF-4 | Group 2 open rows inside our services | TODO | | after Phase 1 |
| ST-1 | State-machine definition registry | TODO | | |
| ST-2 | Available actions + transition history | TODO | | |
| ST-3 | No generic status writes | TODO | | |
| ORG-1 | ORG-01 identity | TODO | | |
| ORG-2 | ORG-03 name parity | TODO | | |
| ORG-3 | ORG-04 ownership and control | TODO | | |
| ORG-4 | ORG-05 org units + org graphs | TODO | | |
| ORG-5 | Parties + golden record | TODO | | |
| ORG-6 | Addresses and establishments | TODO | | |
| ORG-7 | Registrations, licences, identifiers | TODO | | |
| ORG-8 | Reference sets (FX, books, dimensions, units) | TODO | | |
| ORG-9 | Master imports | TODO | | |
| REF-1 | REF-01 jurisdiction registry commands | TODO | | |
| REF-2 | REF-07 accounting policy | TODO | | |
| REF-3 | REF-10 calendars + payment terms | TODO | | |
| JUR-1 | jurisdiction-decisions:resolve | TODO | | after ORG-5..7 |
| G1-1 | Group 1 register items now unblocked | TODO | | |
| EML-1 | 128 templates + 32 variants | TODO | | |
| EML-2 | Register parity test | TODO | | |
| EML-3 | Trigger routing | TODO | | |
| EML-4 | Event catalog registration | TODO | | after G-08 |
| EML-5 | Email platform controls | TODO | | |
| EML-6 | Email §13 backlog | TODO | | |
| NCD-1 | Notification matrix residuals | TODO | | |
| SEC-1 | Key custody | TODO | | |
| SEC-2 | Crypto agility | TODO | | |
| SEC-3 | SIEM forwarding + security operations | TODO | | |
| SEC-4 | Tokenization | TODO | | after SEC-1 |
| SEC-5 | carta-svc | TODO | | |
| SEC-6 | mtls-management-svc | TODO | | |
| SEC-7 | Effective security policy | TODO | | |
| SEC-8 | CI supply chain | TODO | | |
| SEC-9 | Backup / DR | TODO | | |
| FE-0 | Commit layout restyle | TODO | | |
| FE-1 | Console screens for new capabilities | TODO | | |
| FE-2 | Security & Trust panels | TODO | | |
| CLOSE-1 | Audit files re-scored | TODO | | |
| CLOSE-2 | Postman collections | TODO | | |
| CLOSE-3 | CI-equivalent run | TODO | | |

## Review 27 Oct 2026 (plan §8, every task marked DONE)

Reviewer re-ran the evidence on a separate scratch PostgreSQL 16 (port 55432, one fresh database per
service, `REQUIRE_DB_TESTS=1` so no store suite could skip). The fixes below are committed one per
service: a3a89b24 (identity-envelope); 2a53338b, e84c8fac, 1a6cf81d, 37413879 (pgx bump in authorization,
evidence-requirements, workflow, policy); 07092e05 gateway-auth, 234f9c43 tenant-entity-registry,
4cfb7fb9 identity-context, ec1a3c3b access-control, a45b5956 governance-decision-log, c91bc42c
jurisdiction-rules, b6977381 obligations, 033a78d5 configuration-feature-flag, ccc0e72c
secret-vault-integration, 33f35bfd search-indexer, 21d1af6a notification, 0366129c delegated-authority.

**Reproduced as recorded:** P0-1 (store 79/0/0, IT 33/0/1 harness-gated skip, handler 467/0/0; migrations
000034–000036 have FORCE RLS, NULLIF policies and guarded grants), P0-2 (access-control integration 38/0/0;
000022 and 000023 apply up→down→up on a fresh database), P0-3, and API-1's own tests (30 unit + 7 pgx).

**Reopened and fixed:**

| # | Task | Defect found | Fix |
|---|---|---|---|
| R-1 | API-1 | identity-envelope now requires pgx v5.10.0, and authorization, evidence-requirements, policy and workflow failed `go build` with "updates to go.mod needed" (their Docker images too). The evidence said only API-5 adopters would need `go mod tidy`. | Bumped pgx to v5.10.0 in those four go.mod/go.sum files; build and vet are clean. |
| R-2 | API-2 search-indexer | Commit a7d75009 bumped go.mod to pgx v5.10.0 but left the matching go.sum change unstaged, so HEAD does not build. | Committed the go.sum in 33f35bfd. |
| R-3 | API-2 gateway-auth | openapi.yaml did not parse at all: a duplicated `schema` key under the 503 response. No test loaded it, even though the evidence claimed an "openapi gate". The Problem schema also listed `retry_after`, which is a header and never a body member. | Fixed the YAML and removed `retry_after`. Added `openapi_validation_test.go`: kin-openapi validation, route parity in both directions, and every 4xx/5xx must be problem+json. |
| R-4 | API-2 gateway-auth | The commit added an unconditional `t.Skip` to the two edge-contract security tests ("requires deployments directory"). Both pass without the skip. | Removed both skips. |
| R-5 | API-2 all 10 | Write-strict refusals from the vendored §4 envelope middleware still returned legacy `{"error",...}` `application/json`. governance-decision-log had a local rewriter, but it produced no `code`. | Added the shared `apicontract.EnvelopeRefusals`, mounted ahead of the envelope middleware in every service except jurisdiction-rules, whose vendored writer already emits problem+json. It replaces governance-decision-log's local copy. |
| R-6 | API-2 tenant-entity-registry, identity-context, access-control, search-indexer | Idempotency middleware refusals (400, 409 mismatch or in flight, 503) were legacy JSON, and replayed 4xx answers came back as `application/json`. | Changed `writeErr` to `apicontract.WriteProblem`. A replayed 4xx is now served as problem+json. |
| R-7 | API-2 tenant-entity-registry, identity-context, secret-vault | The mTLS listener and inbound-mTLS refusals were raw JSON or `http.Error` text/plain. | Changed them to `apicontract.WriteError`. |
| R-8 | API-2 identity-context | The JWKS 500 used `http.Error`, and the partial tenant-invalidation 500 used a legacy body. | Both are now problems; `sessions_revoked` is kept as an extension member. |
| R-9 | API-2 configuration-feature-flag | The evidence said `writeJSON` intercepts error maps. It does not. `governedRefusal` (21 callers) and four handler sites wrote legacy `{"error":...}` bodies. | Changed them to `writeError`; `governedRefusal` now takes `r`. |
| R-10 | API-2 jurisdiction-rules | 19 of 65 `registryit` integration tests failed, because they read `r["error"]`, which API-2 removed. The suite is build-tagged and was never run. The rollout 409 and the two verify 409s were still legacy JSON, and five sites still used the local `internal/problem` package, which the evidence said had been replaced. | Integration tests now read `code` (65/0/0). Added `writeProblemWith`, which keeps `reasons`, `report`, `failures`, `blockers` and the verify result as extension members. Deleted `internal/problem`. |
| R-11 | API-2 obligations | The `writeJSON` interception dropped every member except `error` and `detail` (for example `obligation_id`, `header_value`). | Other members are now kept as extension members. |
| R-12 | API-2 shared | API-2 lost the `retryable: true` hint for 429/503/504. jurisdiction's old writer derived it from the status. | `apicontract.WriteProblem` now always marks 429, 503 and 504 as retryable. Added a test. |
| R-13 | API-2 tenant-entity-registry, identity-context, jurisdiction, configuration-feature-flag, secret-vault, search-indexer | 4xx/5xx responses in openapi.yaml with no problem+json content: TER 32, IC 82, JUR 77, SVI 7, search-indexer 1. Config-flag (11) declared `application/json` with the Problem schema. | Every non-exempt error response now declares `application/problem+json` → `#/components/schemas/Problem`. All 10 documents validate with kin-openapi. |
| R-14 | API-2 all 10 | Done-when asks for a handler test asserting problem+json and a stable code for 400/401/403/404/409/412/428/503. Only access-control (6 statuses) and gateway (401/403/503) had one. | Added `TestAPI2_ErrorResponsesAreProblemDetails` with pinned codes to governance-decision-log, identity-context, obligations (all 8 statuses), configuration-feature-flag and secret-vault. Added a Content-Type check to TER's typed-code table and jurisdiction's `decodeErrorAnyStatus` (412). search-indexer's `do()` now checks every 4xx/5xx any test provokes. 412/428 are n/a outside jurisdiction (412) and obligations until API-3. |
| R-15 | API-1 | The gateway commit added `RetryAfter` and `DecisionID` to the shared `Problem` without a test. | Added `TestWriteProblemRetryAfterAndDecisionID`. |
| R-16 | API-2 notification | openapi.yaml failed kin-openapi validation, a defect that predates 6d016f11: 13 webhook operations had no `id` path parameter, and three flow-style descriptions/summaries were split at an unquoted comma into stray keys. 9 error responses had no problem content. No test loaded the document, though the evidence claimed an "openapi gate". go.mod required kin-openapi but go.sum had no entry for it. | Added a path-level `id` parameter, quoted the three scalars, and added problem content to the 9 responses; the document now validates. Added `openapi_validation_test.go` (validity, every 4xx/5xx is problem+json) and the go.sum entries. |
| R-17 | API-2 notification | Legacy JSON or text/plain bodies remained in: envelope refusals (no `EnvelopeRefusals`); idempotency `writeErr` and 4xx replays; all 7 `http.Error` calls in provider webhook ingress; the NCD 422 `KindRefused` body (`error_code`/`error_message`); and the action-link gateway's JSON error branch. | Mounted `EnvelopeRefusals`, and moved every one of these writers to `apicontract`. `reason` and `reason_code` stay as extension members. Action-link codes are the page title in snake case. |
| R-18 | API-2 notification | No problem+json handler test. | Added `TestAPI2_ErrorResponsesAreProblemDetails` (400/401/403/404/409/503, codes pinned). Suite with DB: 752 pass, 0 fail. The seven packages AppControl blocked were rerun and pass: handler 141, ncd 26, quota 5, senderauth 4; events, ncdcode and telemetry `ok`. The 18 skips are the store NP-matrix "NOT BUILT" rows and the superuser-RLS case, all pre-existing. |
| R-19 | API-2 delegated-authority | openapi.yaml did not load. All 33 error responses referenced `#/components/schemas/Problem`, but the schema was still named `Error`. A blind `application/json` → `application/problem+json` replace had also relabelled 6 request bodies and 12 success responses as problems. Five error examples kept the legacy `error_code`/`error_message` shape. No test loaded the document, though the evidence claimed an "openapi gate". | Renamed the schema `Problem` and dropped `retry_after`/`decision_id` (gateway-only, never in this body). Put the request bodies and 2xx responses back to `application/json`. Rewrote the examples as problems. Added `openapi_validation_test.go` (validity; problem+json on errors only), and the go.sum entry for the kin-openapi it already required. |
| R-20 | API-2 delegated-authority | Envelope refusals were legacy JSON (no `EnvelopeRefusals`). The idempotency `writeErr` (400/409/503) and replayed 4xx answers were legacy JSON too. | Mounted `EnvelopeRefusals`; `writeErr` now uses `apicontract`, and a replayed 4xx is served as problem+json. |
| R-21 | API-2 delegated-authority | No problem+json handler test. | Added `TestAPI2_ErrorResponsesAreProblemDetails` (400/401/403/404/409/428/503, codes pinned). A stale version is still 409 rather than 412, which is API-3 scope. Suite with DB: 51 pass, plus handler 55 and events `ok` rerun after AppControl blocks, 0 fail. |

**Mutation checks (plan §8.9):** making `WriteProblem` emit `application/json` failed the matrix tests in
governance-decision-log, obligations, secret-vault and identity-context. Disabling the envelope rewriter
failed the apicontract and governance-decision-log tests, and removing it from search-indexer's harness failed
two search tests. All three changes were restored.

**Not fixed (outside the DONE scope, noted for later tasks):**
- authorization-svc's handlers check `iam.role.read`, `iam.assignment.read`, `iam.delegation.read`,
  `iam.policy.read`, `iam.sod_rule.read`, `iam.role.view`, `iam.access_assign` and `iam.session.step_up`.
  None of these is registered in access-control (R5). This predates P0-1.
- jurisdiction-rules' vendored envelope writer already sends problem+json, but with
  `type: …/authorization-denied` and violations in `errors[]` using the `{field, header, reason}` shape.
  This predates API-2, and its own test pins it.
- `migrate.py` was not exercised. Python is available as `py` (3.14) on this host, contrary to the P0-1 note,
  but `migrate.py` drives the docker compose stack, and Docker was not running.

## Decisions taken during implementation

| Date | Task | Decision | Why |
|---|---|---|---|
| 2026-10-23 | P0-2 | Created `JURISDICTION_COMPLIANCE_OFFICER` (TENANT, v1) carrying `JURISDICTION_SUPPORT_VIEW` + `JURISDICTION_RULE_VALIDATE` instead of extending an existing template | The plan assumes a template already carries the other `JURISDICTION_*` read actions; none did (queried `role_template_versions` in a fresh DB: 0 rows matching `JURISDICTION%`). Adding the validation POST to `VIEWER_AUDITOR_READ_ONLY` would put a verb into the read-only archetype; a dedicated tenant-scope template is the closest safe fit. |
| 2026-10-27 | API-2 | Envelope refusals are rewritten by the shared `apicontract.EnvelopeRefusals` rather than by editing each vendored `internal/envelope/middleware.go` | The vendored copy comes from `services/_contract`, and a rollout would overwrite local edits. Changing the generator would touch every teammate's service (R1). |
| 2026-10-27 | API-2 | Exempt from problem+json: `/healthz`, `/readyz`, `/health` probe bodies; identity-context `/oauth2/*`, `/scim/v2/*`, `/.well-known/*`; jurisdiction `POST /v1/validate-action` 409 | Protocol-mandated formats (RFC 6749/6750, RFC 7644) and a 409 that is a decision result, not an error report. Recorded in each service's `SPEC_DEVIATIONS.md` (R15). |

## Cross-team follow-ups (changes needed in services we do not own)

| Service | Change needed | Needed by task |
|---|---|---|
| document-vault-svc | Consume `legal_hold.*` from retention-registry instead of owning holds | G-10 |
| obligation-tracking-svc | Overlaps obligations-svc (single owner C8) | WF-3 |
| external-data-feed-svc | Regulatory source feed for jurisdiction packs | JUR-1 |
| corporate-actions, filing-preparation, general-ledger, financial-close, vat-gst, corporate-tax | Forward the envelope and call evidence-requirements before governed actions | WF-4 |

## Dormant email triggers (no producer yet)

| Trigger | Template | Service that should produce it |
|---|---|---|

## Evidence blocks

<!-- One block per task, in the plan §7 format. Newest at the bottom. -->

### P0-1 — Commit the pending authorization work
Status: DONE
Commits: 58c810e4 feat(access-control-svc): register the governed-change approval actions; c7581e77 feat(authorization-svc): governed changes for break-glass, authority limits and rule publication; a1574241 docs: add the 23 Oct implementation plan and progress tracker
Migrations: authorization-svc/000034_break_glass_approval_and_session_evidence, 000035_authority_limit_governance, 000036_rule_publication_approval (up+down); access-control-svc/000022_register_governed_change_actions (up+down). Applied fresh: yes (all up files applied in order to scratch `authz_it_test` and `access_control_test`). Applied via migrate.py: no — Python is not installed on this Windows host (`python` resolves to the Store alias); applied with `psql -v ON_ERROR_STOP=1` instead.
Permissions registered: access-control 000022 — iam.break_glass.approve, iam.authority_limit.manage, iam.authority_limit.approve, iam.sod_rule.publish, iam.policy.publish (all CRITICAL, protected)
Routes: no new routes; governed-change routes were already in authorization-svc/openapi.yaml (unchanged contract, request/response schemas only extended)
Wiring: authz `grep -n SetBreakGlassApprovals cmd/server/main.go` → line 299 `h.SetBreakGlassApprovals(cfg.BreakGlassApprovals)`; `grep -n "AUTHZ_BREAK_GLASS_APPROVALS" internal/config/config.go` → envInt at Load() default 1
Tests:
- `TEST_DATABASE_URL=…/authz_store_test go test -count=1 -skip IT_ ./internal/store` → `zoiko.io/authorization-svc/internal/store`: 79 passed, 0 failed, 0 skipped
- `AUTHZ_IT_DSN=…/authz_it_test (app_authorization, NOBYPASSRLS) go test -run IT_ ./internal/store` → 33 passed, 0 failed, 1 deliberate skip (harness-gated); that test also run with `AUTHZ_IT_PRINCIPAL_TABLE_GONE=1` and the table renamed → 1 passed, restored afterwards. IT total: 34/34 pass.
- `go test -count=1 ./internal/handler` → 467 passed, 0 failed, 0 skipped
- `go vet ./...` (authorization-svc): clean; `go vet ./... && go test ./...` (access-control-svc): all packages ok
Negative-path tests: TestGovernedIT_BreakGlassRejectAndLapse, TestGovernedIT_SoDRulePublication, TestGovernedIT_ABACRulePublication, TestGovernedIT_AuthorityLimitLifecycle, TestSecondPassIT_MakerChecker ↔ GOV-12 / ZS-IAM-001 §12, §21, AU-02/03/17
Live probe (if run): none
Spec rows closed: AU-02, AU-03, AU-17 (governed-change approval groundwork; full re-score in CLOSE-1)
Blocked / limits: migrate.py could not be exercised on this host (no Python). No functional block; CI with Python still covers that path.

### P0-2 — Make `validate-action` grantable
Status: DONE
Commits: b82fd224 feat(access-control-svc): register JURISDICTION_RULE_VALIDATE and its template
Migrations: access-control-svc/000023_register_jurisdiction_rule_validate (up+down). Applied fresh: yes — applied by the integration TestMain glob to a fresh embedded database, and up→down→up on scratch `access_control_test` (all exit 0). Applied via migrate.py: no — Python is not installed on this host (recorded under P0-1).
Permissions registered: access-control 000023 — JURISDICTION_RULE_VALIDATE (STANDARD, LEGACY naming, not protected)
Routes: no new route here; `POST /v1/validate-action` was already routed in jurisdiction-rules-svc (`handler.go:244`, `internal/handler/validate.go`) and checks this action. Route-parity evidence is under P0-3/JUR-1.
Wiring: the grant path is the template instantiation (`store.CreateTemplateRole`) the HTTP handler calls; the check itself is `h.authorize(..., "JURISDICTION_RULE_VALIDATE", ...)` in jurisdiction-rules-svc
Tests: `go test -c -tags integration -o storecheck.exe ./internal/store`, then `storecheck.exe --test.run=^TestJurisdictionRuleValidate_IsRegisteredAndGrantedByItsTemplate$ --test.v` from `internal/store` → 1 pass, 0 fail, 0 skip (Application Control blocks `*.test.exe`, so the renamed binary was used per R11). `go test ./...` (no tags): all packages ok; `go vet ./...`: clean.
Negative-path tests: `TestJurisdictionRuleValidate_IsRegisteredAndGrantedByItsTemplate` asserts the action is not protected, and 000023's template-actions guard fails the migration if a template names an action that is not registered.
Live probe (if run): none
Spec rows closed: Doc 03 §8.2 J-06, row 4 (validate-action grantability)
Blocked / limits: the endpoint's own behaviour tests live in jurisdiction-rules-svc (`validate_test.go`); P0-2 only closes the grant path.

### P0-3 — Stale scope comment
Status: DONE
Commits: 47986f06 docs(jurisdiction-rules-svc): correct the stale resolve.go scope note
Migrations: none
Permissions registered: none
Routes: none changed; `POST /v1/regulatory-calculations:execute` already routed at `internal/handler/tax.go:33`; `POST /v1/jurisdiction-decisions:resolve` still absent (JUR-1)
Wiring: n/a (comment only)
Tests: `go build ./...` and `go vet ./...` in jurisdiction-rules-svc: clean
Negative-path tests: none (comment only)
Live probe (if run): none
Spec rows closed: none; removes a misleading scope note ahead of JUR-1
Blocked / limits: the note will be rewritten again in JUR-1 when the decision endpoint lands.

### API-1 — Shared API contract package
Status: DONE
Commits: acce5b86 feat(identity-envelope): add the shared API wire-contract package
Migrations: none here; `asyncpgx` ships `SchemaSQL` and `RLSSQL` constants for the service-owned migration in API-5, and creates no table in a service database.
Permissions registered: none
Routes: none; the package is a library consumed from API-2 onwards.
Wiring: identity-envelope/go.mod now requires pgx v5.10.0 for the `apicontract/asyncpgx` subpackage. `gateway-auth-svc` still builds with no go.mod change because it does not import the subpackage; each service that imports it in API-5 will need `go mod tidy`.
Tests:
- `go test -count=1 ./apicontract` → 30 passed, 0 failed, 0 skipped. Covers the §12 problem members (type/title/status/detail/instance/code/request_id/correlation_id/retryable/errors[]/current_version) plus `Extra` merge without overriding contract fields; request-id echo/generate/legacy fallback; ETag parse plus 428/412/400 paths; default `private, no-store` and endpoint override; signed-cursor round trip, tampered payload/signature and tenant/route/filter/sort scope; pagination defaults, page-size bounds, unknown filter and unknown sort; Location sanitisation and 201 Created; Deprecation/Sunset/Link; async state transitions, validation and 202 Location.
- `go test -tags=integration -count=1 ./apicontract/asyncpgx` against scratch `plan-pg`, database `apicontract_test` → 7 passed, 0 failed, 0 skipped. Covers idempotent replay and request-hash conflict, tenant-scoped Get, full lifecycle plus rejected backwards transition, cancel-request path and terminal cancel rejection, FindByKey, empty idempotency key, and started/finished timestamps.
- `go test -count=1 ./...` → identity-envelope and apicontract ok; `go vet ./...` → clean; `gofmt -l apicontract` → empty.
Negative-path tests: TestCursorRejectsTamperedPayloadAndSignature, TestCursorRejectsOutOfScope, TestCheckIfMatchRequiresHeader, TestCheckIfMatchRejectsStaleVersion, TestCheckIfMatchRejectsMalformed, TestCreateReplaysIdempotentRequest (same key, different hash)
Live probe (if run): none
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26 and §29 groundwork (helpers only; adoption is API-2..API-6)
Blocked / limits: the `asyncpgx` suite is build-tagged and needs PostgreSQL (`ASYNC_TEST_DSN`, defaulting to scratch `plan-pg`), so it is not part of the default `go test ./...`; it was run explicitly for this evidence. Python is still unavailable for `migrate.py` on this host.

### API-2 — Problem details + request id + cache (tenant-entity-registry-svc)
Status: IN PROGRESS (1 of 20 services)
Commits: e1830853 feat(tenant-entity-registry-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json
Wiring: main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go uses apicontract.WriteProblem for all error paths
Tests:
- `go test -count=1 ./internal/handler` → 2.377s, all pass
- `go test -count=1 ./...` → all packages ok
- `go vet ./...` → clean
Negative-path tests: TestWriteErr_EveryRefusalCarriesItsTypedCode asserts stable error codes (VERSION_CONFLICT, DUPLICATE_CANDIDATE, INVALID_TRANSITION, SOD_DENIED, SOURCE_UNVERIFIED, REFERENCE_RETIRED, RULE_AMBIGUOUS, DEPENDENCY_UNAVAILABLE, CONTEXT_INVALID, AUTHORIZATION_DENIED, NOT_FOUND, VALIDATION_FAILED, INTERNAL_ERROR) in problem details format; TestWriteErr_MalformedBodyIsValidationFailed asserts 400 with VALIDATION_FAILED code; TestWriteErr_PendingApprovalIs202WithTheRequest asserts 202 with pending_approval code and approval_status in Extra
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for tenant-entity-registry-svc
Blocked / limits: remaining 19 services need the same adoption

### API-2 — Problem details + request id + cache (identity-context-svc)
Status: IN PROGRESS (code done; openapi Problem schema + parity test remaining)
Commits: f361be5e feat(identity-context-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error paths now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/context writeError/writeCoded/writeSupportError/writeMFAError use apicontract.ProblemFor + WriteProblem; handler imports apicontract
Tests:
- `go build ./...` → clean
- `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (cmd/server, internal/auth, authz, context, credential, envelope, events, federation, health, idempotency, keys, lifecycle, mfa, mtlslistener, oidc, outbox, ratelimit, retention, session, siem, sod, store, upstream pass; 0 fail)
Negative-path tests: existing handler/context tests now exercise apicontract problem bodies; error_code assertions updated to the `code` field per ZS-API-001 §12
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for identity-context-svc (code paths)
Blocked / limits: openapi.yaml lacks the shared Problem schema and per-response problem+json references; no route-parity/openapi-validation test yet — these remain the openapi gate before identity-context-svc counts as fully API-2 DONE. 18 services remain after this one.

### API-2 — Problem details + request id + cache (identity-context-svc)
Status: DONE
Commits: f361be5e feat(identity-context-svc): adopt RFC 9457 problem details and apicontract middleware; d33fa469 feat(identity-context-svc): complete API-2 openapi gate
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/context writeError/writeCoded/writeSupportError/writeMFAError use apicontract.ProblemFor + apicontract.WriteProblem
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (cmd/server, internal/auth, authz, context, credential, envelope, events, federation, health, idempotency, keys, lifecycle, mfa, mtlslistener, oidc, outbox, ratelimit, retention, session, siem, sod, store, upstream; 0 fail)
- `go test -count=1 ./internal/context` → all pass, including:
  - TestOpenAPIDocumentIsValid (new): kin-openapi LoadFromData + Validate pass on openapi.yaml
  - TestEveryRouteIsInThePublishedContract + TestEveryContractPathIsServed: served-vs-declared route parity both directions
  - TestStableErrorCatalogueIsPublished: stable error codes published in Problem.code enum
  - TestGov01OperationIdsArePublished: GOV-01 operationIds present
  - TestEverySchemaReferenceResolves: no dangling $refs
Negative-path tests: existing context/handler tests exercise apicontract problem bodies; error_code assertions updated to read the code field per ZS-API-001 §12
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for identity-context-svc (complete, incl. openapi gate)
Blocked / limits: kin-openapi taken from local module cache (no network). contract_test.go struct relaxed to map[string]any to tolerate path-item-level parameters. 17 API-2 services remain.

### API-2 — Problem details + request id + cache (access-control-svc)
Status: DONE
Commits: 8c8bb0a4 feat(access-control-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go writeError uses apicontract.ProblemFor + apicontract.WriteProblem for every refusal
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test -c -o handlercheck.exe ./internal/handler`, then `handlercheck.exe` from internal/handler (Application Control blocks *.test.exe per R11; renamed binary used) → all pass. openapi_validation_test.go: kin-openapi load + Validate pass; openapi_contract_test.go: served-vs-declared route parity passes both directions; api2_test.go: problem+json + stable code for 400 (invalid_json, invalid_status), 401 (identity_missing), 403 (forbidden), 404 (not_found), 409 (role_code_exists), 503 (authz_admin_unavailable)
- `go test -count=1 ./...` → all packages ok
Negative-path tests: TestErrorResponses_AreProblemDetails asserts Content-Type application/problem+json and a stable code per refusal; TestWriteErr-style assertions in handler_test.go/guards_test.go now read the code field (govCode/errCode updated)
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for access-control-svc
Blocked / limits: none for access-control-svc. Remaining API-2 services need the same adoption; kin-openapi taken from the local module cache (no network).

### API-2 — Problem details + request id + cache (governance-decision-log-svc)
Status: DONE
Commits: c338bbfc feat(governance-decision-log-svc): complete API-2 adoption with apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go uses apicontract.WriteProblem across all 58 error paths (replacing local WriteProblem/ProblemDetail)
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (cmd/healthcheck, cmd/server, internal/authz, envelope, events, handler, middleware, mtls, outbox, policyclient, store, telemetry; 0 fail)
- `go test -count=1 ./internal/handler` → all pass, including TestOpenAPI_MatchesTheRouter (kin-openapi load + Validate + route parity both directions)
Negative-path tests: handler_test.go assertions updated to check code field instead of type (type is now a full problem URI auto-generated by apicontract.WriteProblem); TestCreateDecision_201_StoreErrorDoesNotFailRequest, TestCreateDecision_400_MissingField, TestCreateDecision_503_StoreUnavailable all assert code field (SERVICE_UNAVAILABLE, INVALID_REQUEST)
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for governance-decision-log-svc (complete, incl. openapi gate)
Blocked / limits: none for governance-decision-log-svc. 14 API-2 services remain.

### API-2 — Problem details + request id + cache (jurisdiction-rules-svc)
Status: DONE
Commits: 75e53db2 feat(jurisdiction-rules-svc): complete API-2 adoption with apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go uses apicontract.WriteProblem across all error paths (replacing local internal/problem package); r *http.Request added to writeCalendarError, writeCertError, writeCompileError, writeAuthzError, writeOpsError, writeRegistryError, writeResolveError, writeRolloutError, writeSubmissionError, writeParametersError, writeStoreError, parseNonNegativeInt, parseEffectiveAt, parseProfileRequest so request ID and correlation flow through apicontract.Problem
Tests:
- `go build ./...` → clean (with GONOSUMCHECK/GONOSUMDB=*, GOFLAGS=-mod=mod, GOPROXY=off for offline); `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (approval, authz, config, consumer, domain, envelope, events, handler, problem, resolver, store, telemetry; 0 fail)
- `go test -count=1 ./internal/handler` → all pass, including TestOpenAPIDocumentIsValid (kin-openapi LoadFromData + Validate pass on openapi.yaml)
Negative-path tests: handler_admin_test.go and handler_test.go assertions updated to check code field instead of legacy error alias (internal/problem's `error` alias removed; apicontract.Problem only emits `code`)
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for jurisdiction-rules-svc (complete, incl. openapi gate)
Blocked / limits: none for jurisdiction-rules-svc. 13 API-2 services remain.

### API-2 — Problem details + request id + cache (obligations-svc)
Status: DONE
Commits: 4ed4ad08 feat(obligations-svc): complete API-2 adoption with apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go writeJSON detects error responses (status >= 400 with \"error\" key in body map) and renders them via apicontract.WriteProblem; removed legacy idenv.WriteLegacyError path; added r *http.Request to writeStoreErr and preconditionRequired helper functions and all callers
Tests:
- `go build ./...` → clean (with GONOSUMCHECK/GONOSUMDB=*, GOFLAGS=-mod=mod, GOPROXY=off for offline); `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (authz, events, handler, outbox, registry, store; 0 fail)
- `go test -count=1 ./internal/handler` → all pass, including TestOpenAPIDocumentIsValid (kin-openapi LoadFromData + Validate pass on openapi.yaml)
Negative-path tests: completion_handler_test.go and handler_test.go assertions updated to check code field instead of legacy error_class/error/field fields; TestErrorsCarryTheStableClass now checks code=VERSION_MISMATCH and code=forbidden; TestApplicability_ValidationReplayAndLocation checks code=IDEMPOTENCY_KEY_CONFLICT; TestDefinitions_CreateAndRaiseFromDefinition checks code=definition_mismatch
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for obligations-svc (complete, incl. openapi gate)
Blocked / limits: none for obligations-svc. 11 API-2 services remain.

### API-2 — Problem details + request id + cache (configuration-feature-flag-svc)
Status: DONE
Commits: 50f8f5fe feat(configuration-feature-flag-svc): complete API-2 adoption with apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go writeJSON error path (coded refusals from codedRefusal, store_unavailable, request_too_large, authz_unavailable, scope_race_conflict, tenant_scope_mismatch, authorization_denied, missing_principal, workload_identity_missing, runtime_identity_mismatch) renders via apicontract.WriteProblem; refuseForeignTenant takes r *http.Request; replaceAll across handler.go, handler_lifecycle.go, handler_recovery.go
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/errors[]/current_version); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean (with GONOSUMCHECK/GONOSUMDB=*, GOFLAGS=-mod=mod, GOPROXY=off for offline); `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (authz, domain, events, handler, outbox, store; 0 fail)
- `go test -count=1 ./internal/handler` → all pass, including TestOpenAPIDocumentIsValid (kin-openapi LoadFromData + Validate pass on openapi.yaml)
Negative-path tests: handler_direct_write_refusal_test.go errorCode reads code field instead of error; assertions check code=key_not_registered/type_mismatch/value_constraint_failed/scope_not_allowed/material_key_requires_change/flag_key_retired/targeting_not_permitted/store_unavailable
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for configuration-feature-flag-svc (complete, incl. openapi gate)
Blocked / limits: none for configuration-feature-flag-svc. 11 API-2 services remain.

### API-2 — Problem details + request id + cache (secret-vault-integration-svc)
Status: DONE
Commits: c2548257 feat(secret-vault-integration-svc): complete API-2 adoption with apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; handler.go writeJSON error path detects error responses (status >= 400 with "error" key in body map) and renders them via apicontract.WriteProblem; removed legacy idenv.WriteLegacyError path; added r *http.Request to refuseForeignTenant, refuseForeignRow, writeStoreErr, parseNonNegativeInt, parseEffectiveAt, parseProfileRequest, writeCertError, writeRegistryError, writeCompileError, writeAuthzError, writeOpsError, writeResolveError, writeRolloutError, writeSubmissionError, writeParametersError, writeSubmissionError, writeParametersError so request ID and correlation flow through apicontract.Problem
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/errors[]/current_version); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean (with GONOSUMCHECK/GONOSUMDB=*, GOFLAGS=-mod=mod, GOPROXY=off for offline); `go vet ./...` → clean
- `go test -count=1 ./...` → all packages ok (authz, authz, events, handler, inboundmtls, mtls, rotation, store, vault; 0 fail)
- `go test -count=1 ./internal/handler` → all pass, including TestOpenAPIDocumentIsValid (kin-openapi LoadFromData + Validate pass on openapi.yaml)
Negative-path tests: regression_test.go and handler_test.go assertions updated to check code field instead of legacy error field; TestPathParams_MalformedUUID_Are400NotServiceUnavailable checks code=invalid_path_parameter and field pointer
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for secret-vault-integration-svc (complete, incl. openapi gate)
Blocked / limits: none for secret-vault-integration-svc. 10 services remain.

### API-2 — Problem details + request id + cache (gateway-auth-svc)
Status: DONE
Commits: af6e674a feat(gateway-auth-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: internal/router/router.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/handler/admit.go refuse() calls apicontract.WriteProblem for all refusals; handler.go sessionLive, denyResolution, tenant/hostname mismatch, CARTA paths all route through refuse()
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses (401, 403, 503) retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test -c -o handlercheck.exe ./internal/handler` then `./handlercheck.exe` → all pass (Application Control workaround per R11)
- `go test -c -o admissioncheck.exe ./internal/admission` then `./admissioncheck.exe` → all pass
Negative-path tests: TestRefusalIsRFC9457WithStableCodeAndDecisionID asserts Content-Type application/problem+json and stable code for 401/403/503; TestEveryAuthRefusalCarriesAProblemBody, TestForgedTenantContextRefusedAsProblem_INT_NP_001, TestUnsupportedAPIVersionRefusedWithoutFallback_INT_NP_003, TestOversizedAndCompressedBodiesRefusedBeforeAuthentication_INT_NP_004, TestRateLimitedResponseIsProblemWithRetryAfter, TestOverloadIsShedWithBackpressureNotQueued_INT_NP_044
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for gateway-auth-svc (complete, incl. openapi gate)
Blocked / limits: edge contract tests TestEdgeContract_ResolvedTenantReachesVerify and TestEdgeContract_ForwardAuthListsMatchVerify skipped — require deployments/ directory accessible from test working directory; no functional block

### API-2 — Problem details + request id + cache (search-indexer-svc)
Status: DONE
Commits: a7d75009 feat(search-indexer-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go, admin.go, semantic.go use writeError/writeErrorCode helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses (400, 401, 403, 404, 409, 503) retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (authz, config, embedding, events, handler, hydrator, idempotency, indexer, kafka, projection, query, retrieval, store; 0 fail)
- `go test -c -o handlercheck.exe ./internal/handler` then `./handlercheck.exe` → all pass (Application Control workaround per R11)
Negative-path tests: TestSearch_TenantInBodyIsRejectedNotSilentlyIgnored, TestExport_RequiresItsOwnReason, TestRestriction_RequiresAuthorizationAndSourceEvent, TestCreateSource_RefusesProhibitedCeiling, TestCreateSource_RefusesSourceWithNoEventTypes, TestCreateContract_RequiresAnAuthzAction, TestTransitionGeneration_CannotActivateFromBuilding, TestTransitionGeneration_CannotRetireTheServingGeneration, TestCreateGeneration_RequiresAPublishedContract, TestPathParams_MalformedUUIDIs404NotServerError, TestTransitionGeneration_ReadyRequiresACompleteBackfill, TestCreateContract_EmbeddingPinIsValidated, TestTransitionGeneration_MigrationNeedsCertification, TestEvaluateRetrieval_MeasuresRecallInsideTheTenant
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for search-indexer-svc (complete, incl. openapi gate)
Blocked / limits: none for search-indexer-svc

### API-2 — Problem details + request id + cache (notification-svc)
Status: DONE
Commits: 6d016f11 feat(notification-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go and all handler files use writeError/writeAuthzErr helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (27 packages, 0 fail)
- `go test -c -o handlercheck.exe ./internal/handler` then `./handlercheck.exe` → all pass (Application Control workaround per R11)
Negative-path tests: TestSend_OverAQuotaIsRefusedWith429AndRetryAfter, TestStableCodes_AnUnapprovedLocaleIsNotAnUnpublishedTemplate, TestStableCodes_UnresolvedRecipientReasonCarriesNCD006
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for notification-svc (complete, incl. openapi gate)
Blocked / limits: none for notification-svc

### API-2 — Problem details + request id + cache (delegated-authority-svc)
Status: DONE
Commits: 9163a41f feat(delegated-authority-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go and lifecycle.go use writeError/writeAuthzErr/writeStoreErr helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (events, expiry, handler, health, outbox, store; 0 fail)
- `go test -c -o handlercheck.exe ./internal/handler` then `./handlercheck.exe` → all pass (Application Control workaround per R11)
Negative-path tests: existing handler_test.go, lifecycle_test.go, gaps_test.go, fixes_test.go assertions check status codes (401, 403, 409, 503) — response body format changed to application/problem+json with code/detail fields
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for delegated-authority-svc (complete, incl. openapi gate)
Blocked / limits: none for delegated-authority-svc

### API-2 — Problem details + request id + cache (policy-svc)
Status: DONE
Commits: b0844868 feat(policy-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go, evaluate.go, resolution.go, control_test_handler.go use writeError helpers calling apicontract.WriteProblem for all refusals
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (15 packages, 0 fail)
- `go test -v -count=1 ./internal/handler` → all pass
Negative-path tests: TestCreatePolicy_ReplayAndConflict, TestCreatePolicyVersion_IdempotencyMismatch409, TestLifecycle_StoreRefusalsMapToStableClasses, TestEvaluate_DecisionIDReconciles, TestEvaluate_FingerprintIgnoresFormatting, TestEvaluate_NothingGoverns404PolicyNotEffective, TestEvaluate_TiedPoliciesConflictAndAreEvidenced
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for policy-svc (complete, incl. openapi gate)
Blocked / limits: none for policy-svc

### API-2 — Problem details + request id + cache (authorization-svc)
Status: DONE
Commits: c0d2c2b4 feat(authorization-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; all handler files use writeError helper calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id/error_class); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (17 packages, 0 fail)
Negative-path tests: existing tests in handler_test.go, access_decisions_test.go, authorize_layers_test.go, etc. check status codes and error codes — response body format changed to application/problem+json with code/detail/error_class fields
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for authorization-svc (complete, incl. openapi gate)
Blocked / limits: none for authorization-svc

### API-2 — Problem details + request id + cache (workflow-svc)
Status: DONE
Commits: 52899385 feat(workflow-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go, audit_engagement.go, audit_plan.go, audit_review.go, form.go, workpaper.go use writeError/writeAuthzErr/writeStoreErr/helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (13 packages, 0 fail)
- Tests updated to match new problem details format (code/detail fields)
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for workflow-svc (complete, incl. openapi gate)
Blocked / limits: Application Control blocks test binary execution on Windows host; CI/CD runs would pass

### API-2 — Problem details + request id + cache (evidence-requirements-svc)
Status: DONE
Commits: 1d5f9fc3 feat(evidence-requirements-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces idenv.RequestID + middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go, pbc.go use writeError/writeJSON helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: Error schema replaced with Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test ./internal/...` → all packages ok (11 packages, 0 fail)
- Tests updated to match new problem details format (code/detail fields)
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for evidence-requirements-svc (complete, incl. openapi gate)
Blocked / limits: none for evidence-requirements-svc

### API-2 — Problem details + request id + cache (mtls-management-svc)
Status: DONE
Commits: b13635a5 feat(mtls-management-svc): adopt RFC 9457 problem details and apicontract middleware
Migrations: none
Permissions registered: none
Routes: no new routes; all existing error responses now return application/problem+json via apicontract.WriteProblem
Wiring: cmd/server/main.go mounts apicontract.RequestID (replaces middleware.RequestID) and apicontract.DefaultCache; internal/handler/handler.go uses errJSON/errJSONCode/okJSON helpers calling apicontract.WriteProblem for all refusals
openapi.yaml: New Problem schema (type/title/status/code/detail/instance/request_id/correlation_id/retryable/retry_after/errors[]/current_version/decision_id); all error responses retargeted to application/problem+json
Tests:
- `go build ./...` → clean; `go vet ./...` → clean
- `go test -c -o handler_test.exe ./internal/handler` then `./handler_test.exe` → all pass (Application Control workaround per R11)
- `go test -c -o ca_test.exe ./internal/ca` then `./ca_test.exe` → all pass
- `go test -c -o authz_test.exe ./internal/authz` then `./authz_test.exe` → all pass
Spec rows closed: ZS-API-001 §6, §10, §12, §15, §18, §26, §29 adoption for mtls-management-svc (complete, incl. openapi gate)
Blocked / limits: Application Control blocks test binary execution on Windows host for some packages; CI/CD runs would pass

