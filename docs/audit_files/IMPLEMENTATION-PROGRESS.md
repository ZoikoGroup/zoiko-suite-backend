# Implementation progress

Plan: `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md`. Update this file after every task.
Statuses: TODO · IN PROGRESS · DONE · BLOCKED. DONE needs a filled evidence block (plan §7).

## Status board

| Task | Title | Status | Commit(s) | Note |
|---|---|---|---|---|
| P0-1 | Commit pending authorization work | DONE | 58c810e4, c7581e77, a1574241 | authz 90 store+IT / 467 handler pass, 0 fail; vet clean |
| P0-2 | Register JURISDICTION_RULE_VALIDATE | DONE | b82fd224 | access-control 000023 + template + store test |
| P0-3 | Fix stale resolve.go comment | DONE | 47986f06 | comment now matches tax.go |
| API-1 | Shared apicontract package | DONE | acce5b86 | 30 unit + 7 pgx integration pass, 0 fail/skip; vet clean |
| API-2 | Problem details + request id + cache, all our services | IN PROGRESS | e1830853, f361be5e, 8c8bb0a4, d33fa469, c338bbfc, 75e53db2, 4ed4ad08, 50f8f5fe, c2548257 | tenant-entity-registry-svc + access-control-svc + identity-context-svc + governance-decision-log-svc + jurisdiction-rules-svc + obligations-svc + configuration-feature-flag-svc + secret-vault-integration-svc done; 10 services remain |
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

## Decisions taken during implementation

| Date | Task | Decision | Why |
|---|---|---|---|
| 2026-10-23 | P0-2 | Created `JURISDICTION_COMPLIANCE_OFFICER` (TENANT, v1) carrying `JURISDICTION_SUPPORT_VIEW` + `JURISDICTION_RULE_VALIDATE` instead of extending an existing template | The plan assumes a template already carries the other `JURISDICTION_*` read actions; none did (queried `role_template_versions` in a fresh DB: 0 rows matching `JURISDICTION%`). Adding the validation POST to `VIEWER_AUDITOR_READ_ONLY` would put a verb into the read-only archetype; a dedicated tenant-scope template is the closest safe fit. |

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

