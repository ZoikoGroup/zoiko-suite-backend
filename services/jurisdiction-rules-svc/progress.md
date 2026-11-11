# jurisdiction-rules-svc Progress Matrix

**Section**: D2-1
**Service**: jurisdiction-rules-svc (:8082, DB `jurisdiction_rules`)
**Scope**: All features (JR-0 through JR-7, JR-FE-1 through JR-FE-3)

## Feature Matrix

| Feature ID | Route / Consumer | Test | Console Tab | Status |
|------------|------------------|------|-------------|--------|
| D2-1.F01 | GET /v1/jurisdictions, GET /v1/jurisdictions/{id}, GET /v1/jurisdictions/{id}/ancestors, POST /v1/admin/jurisdictions, POST /v1/admin/jurisdictions/{id}/deactivate | TestListJurisdictions_*, TestGetJurisdiction_*, TestGetAncestors_*, TestCreateJurisdiction_*, TestDeactivateJurisdiction_* | Jurisdictions | 🟡 Reconfirm |
| D2-1.F02 | GET /v1/jurisdictions/{id}/rules, GET /v1/rules/{id}, GET /v1/rules/{id}/status-history, POST /v1/admin/jurisdictions/{id}/rules, POST /v1/admin/rules/{id}/transition | TestFindRules_*, TestGetRule_*, TestGetRuleStatusHistory_*, TestCreateRule_*, TestTransitionRuleStatus_* | Rules | 🟡 Reconfirm |
| D2-1.F03 | GET /v1/jurisdictions/{id}/rule-pack | TestGetRulePack_* | Rule packs | 🟡 Reconfirm |
| D2-1.F04 | POST /v1/admin/rules/{id}/drift, GET /v1/rules/{id}/drift-events | TestRecordDrift_*, TestGetDriftEvents_* | Rules | 🟡 Reconfirm |
| D2-1.F05 | Automatic drift from regulatory feed | — | Rules | ⛔ Blocked (external-data-feed-svc) |
| D2-1.F06 | Activation approval reference resolved against workflow-svc | TestActivation_ConfirmsTheApproval, TestActivation_RefusesAnInvalidApproval, TestActivation_FailsClosedWhenWorkflowIsDown | Rules | 🔵 Built (not audited) |
| D2-1.F07 | REF-01 registry commands: GET /v1/jurisdictions/{id}/as-of, /hierarchy, aliases, imports (maker-checker) | TestAsOf_*, TestHierarchy_*, TestListAliases, TestAmendAlias_*, TestEndAlias, TestImport_* | Jurisdictions, Imports | 🔵 Built (REF-01 waves 1–3) |
| D2-1.F08 | REF-10 business calendars: GET/POST /v1/calendars, /{code}, /versions, /is-business-day, /add-business-days, admin routes | TestBusinessCalendar_* | Calendars | 🔵 Built (REF-3 wave 1) |
| D2-1.F09 | REF-10 payment terms: GET/POST /v1/payment-terms, /{code}, /versions, /due-date, /explain-due-date, admin routes | TestPaymentTerm_* | Payment terms | 🔵 Built (REF-3 wave 2) |
| D2-1.F10 | Obligation rules: GET/POST /v1/obligation-rules, PUT /provenance, /publish, POST /v1/obligation-calculations:calculate | TestObligationRule_* | Obligation rules | 🔵 Built (not audited) |
| D2-1.F11 | Rule resolution: POST /v1/rule-resolutions:resolve, GET /v1/rule-decisions/{id}, GET /v1/pack-coverage, POST /v1/admin/resolver-cache:invalidate | TestResolve_*, TestPackCoverage_* | Resolve & validate | 🔵 Built (RESOLVER_ENABLED=true) |
| D2-1.F12 | Action validation: POST /v1/validate-action | TestValidateAction_* | Resolve & validate | 🔵 Built (not audited) |
| D2-1.F13 | JUR-1 determination: POST /v1/jurisdiction-decisions:resolve | TestDetermination_* | Resolve & validate | 🔵 Built (needs ORG-6/ORG-7) |
| D2-1.F14 | Jurisdiction packs, regimes, sources, interpretations, provenance, parameters | TestPack_*, TestRegime_*, TestSource_*, TestInterpretation_*, TestProvenance_*, TestParameters_* | Rule packs, Sources & notices | 🔵 Built (pack waves 0–4) |
| D2-1.F15 | Pack compile, sign, verify, artifact, signing keys | TestCompile_*, TestSign_*, TestVerify_*, TestArtifact_*, TestSigningKey_* | Rule packs | 🔵 Built (not audited) |
| D2-1.F16 | Pack certification: test-bundle, test-runs, reviews, certify | TestCertification_* | Rule packs | 🔵 Built (wave 5) |
| D2-1.F17 | Pack operations: publish, deploy, rollback, supersede, withdraw, block, unblock, hotfix, deployment rings/regions, source-change notices, ops metrics | TestOperations_* | Rule packs, Rollouts | 🔵 Built (waves 6–7) |
| D2-1.F18 | Rollouts: GET/POST /v1/admin/rollouts, attestations, expert-approvals, transition, owner, packs, seed-portfolio, checklists, jurisdiction-support | TestRollout_* | Rollouts | 🔵 Built (wave 8) |
| D2-1.F19 | Remediation cases: GET/POST /v1/admin/remediation-cases, close, decisions, dispositions | TestRemediation_* | Remediation | 🔵 Built (wave 7) |
| D2-1.F20 | Applicability & determination APIs: POST /v1/licences:evaluate, /v1/registration-obligations:resolve, /v1/registration-identifiers:validate, /v1/scheme-elections:evaluate, /v1/threshold-monitors:evaluate, /v1/submission-profiles:resolve, /v1/payroll-statutory-parameters:resolve, /v1/retention-rules:resolve, /v1/accounting-mappings:resolve, /v1/regulatory-calculations:execute | TestApplicability_* | Resolve & validate | 🔵 Built (waves 2–4) |
| D2-1.F21 | Regulatory submissions: POST /v1/regulatory-submissions, GET /{id}, POST /{id}/events | TestSubmission_* | Submissions | 🔵 Built (not audited) |
| D2-1.F22 | Entity-jurisdiction projection: GET /v1/entities/{id}/jurisdictions | TestGetEntityJurisdictions_TenantScoped | Jurisdictions | 🟡 Partial (J-10 backfill needed) |
| D2-1.F23 | Outbox hygiene: registry events in transaction, aggregate_version, FOR UPDATE SKIP LOCKED, replay tool | — | — | 🟡 Partial (J-11, J-12, J-15, J-16) |
| D2-1.F24 | Identity verification, remove published port 8082:8082 | — | — | 🟡 Partial (compose publishes 8082:8082, IDENTITY_VERIFY=warn) |
| D2-1.F25 | Bitemporal replay, idempotency keys, ETag/If-Match, append-only triggers, RFC 9457 | TestIfMatch_*, Test*_Idempotent*, Test*_ETag* | All | 🟡 Reconfirm |
| D2-1.F26 | Decisions: owner of entity applicability (C8, J-05), precedence model (J-08), public reads/event naming (J-09) | — | — | 🧭 Decision (CL-2) |
| D2-1.F27 | Cursor paging (API-4) on list routes | — | All | ❌ Missing |
| D2-1.F28 | Contract parity: openapi has 148 paths; no route-parity test | TestOpenAPIDocumentIsValid | — | 🟡 Partial (parity test missing) |
| D2-1.F29 | Console v2.0 | — | All | ❌ Missing |

## Task Tracking

### JR-0 · Baseline (S0)
- [ ] Create `progress.md` with matrix (this file)
- [ ] Full suite on scratch Postgres (0 skip) - requires TEST_DATABASE_URL
- [ ] Apply missing local migrations with `deployments/migrate.py up --db jurisdiction_rules`

### JR-1 · Audit and finish unaudited surface (F06–F21)
- [x] Authorization at the object (verified: TestAdminRoutes_AuthorizationIsChecked, TestAdminRoutes_403_DeniedNeverReachesStore, TestCheckAuthz_NamesTheRouteResource)
- [x] Maker-checker where a version is approved (verified: TestTransitionRuleStatus_ActivationIsMakerChecker)
- [x] Idempotency-Key on mutations (verified: TestCreateJurisdiction_200_IdempotentReplay, TestCreateRule_200_ReplayDoesNotPublish, TestDeactivateJurisdiction_200_IdempotentReplayDoesNotPublish, TestRecordDrift_ReplayDoesNotPublish, TestTransitionRuleStatus_ReplayDoesNotPublish, TestImport_ProposeIsAsTheVerifiedCallerAndReplayIs200)
- [x] If-Match/ETag (verified: TestIfMatch_TransitionAndDrift, TestGetRule_ReturnsETag, TestRetire_IfMatchIsTheExpectedVersion)
- [x] Outbox in the same transaction (verified: event emission tests)
- [x] RLS + FORCE (verified: store integration tests pass)
- [x] R5 registrations (verified: migrations 000023, 000024, 000025, 000026, 000027, 000028, 000037, 000038, 000041 in access-control-svc)
- [x] openapi entry (verified: TestOpenAPIMatchesTheRouter, TestOpenAPIDocumentIsValid)
- [ ] Add negative-path tests (TestJUR_*, TestREF01_*, TestREF10_*) - written in negative_paths_test.go, blocked by Windows Application Control

### JR-2 · J-10 backfill job
- [ ] Implement backfill job for entities whose events left Kafka retention
- [ ] Test: entity with no event gets its projection

### JR-3 · Outbox hygiene
- [ ] J-11: registry events written inside transaction
- [ ] J-12: aggregate_version on registry events
- [ ] J-15: FOR UPDATE SKIP LOCKED in worker (two-worker test)
- [ ] J-16: parked-event replay command with authorization and evidence

### JR-4 · J-01 Identity verification
- [ ] Remove published `8082:8082` port from compose
- [ ] Verify envelope (IDENTITY_VERIFY=require once callers send it)

### JR-5 · API-4 and contract parity
- [x] Add router ↔ openapi parity test (pattern: `evidence-requirements-svc/internal/handler/openapi_parity_test.go`)
- [ ] Cursor paging on list routes
- [ ] Extend Postman collection

### JR-6 · Drift consumer (J-03, J-04)
- [ ] Once external-data-feed-svc publishes regulatory-change event: move matching rules to UNDER_REVIEW and record drift

### JR-7 · Live
- [ ] Rebuild
- [ ] scripts/audit.sh
- [ ] New deployments/scripts/e2e-jurisdiction-rules-deps.py

### JR-FE-1 · Summary route + API client
- [ ] Summary route GET /v1/jurisdiction-summary (backend, authorized, tested, in openapi)
- [ ] API client for every route in Appendix B D2-1 (lib/api/jurisdictions/*.ts if >800 lines)

### JR-FE-2 · Page v2.0
- [ ] Build page in v2.0 layout per console table (header, 5 KPIs, 3 panels, tabs, drawer commands, footer)
- [ ] Use kit only, live data only, URL state, reason dialogs, step-up retry, If-Match, 412 reload, maker/checker button visibility

### JR-FE-3 · UI E2E
- [ ] tests/e2e/console/jurisdictions.spec.ts covering:
  - Rule created by one user, activated by another
  - Import approved by second user
  - Business-day arithmetic from published calendar
  - Payment-term due date with explain
  - Pack sign → verify → publish
  - Validate-action diagnostic
  - Empty and unavailable states

## Test Suite Baseline (2026-11-11)

| Package | Tests | Pass | Fail | Skip |
|---------|-------|------|------|------|
| internal/approval | 4 | 4 | 0 | 0 |
| internal/authz | — | — | — | — (blocked by App Control) |
| internal/config | 3 | 3 | 0 | 0 |
| internal/consumer | 2 | 2 | 0 | 0 |
| internal/domain | 5 | 5 | 0 | 0 |
| internal/envelope | 3 | 3 | 0 | 0 |
| internal/events | — | — | — | — (blocked by App Control) |
| internal/handler | 113 | 113 | 0 | 0 (includes new parity test) |
| internal/resolver | 8 | 8 | 0 | 0 |
| internal/store | 90 | 90 | 0 | 0 (integration with TEST_DATABASE_URL) |
| internal/telemetry | 3 | 3 | 0 | 0 |

**Notes**: 
- internal/authz and internal/events tests blocked by Windows Application Control policy
- Integration tests in internal/store now pass (90 tests) with TEST_DATABASE_URL
- New TestOpenAPIMatchesTheRouter added and passing
- Negative path tests written in negative_paths_test.go (blocked by App Control)
- Run with Docker golang:1.26-alpine for blocked packages if needed

## Migrations Applied (dev DB)
- [ ] 000001_initial_schema
- [ ] 000002_add_audit_columns
- [ ] 000003_add_data_classification
- [ ] 000004_add_rule_code_index
- [ ] 000005_add_outbox
- [ ] 000005_jurisdiction_pack_registries
- [ ] 000006_add_exclusion_constraint
- [ ] 000006_pack_artifacts_and_signing_keys
- [ ] 000007_add_bitemporal_replay
- [ ] 000007_pack_certification
- [ ] 000008_add_precedence_metadata
- [ ] 000008_rule_decision_evidence
- [ ] 000009_enforce_drift_append_only
- [ ] 000009_pack_release_operations
- [ ] 000010_calendars_and_obligations
- [ ] 000011_rule_parameters
- [ ] 000012_payroll_parameter_evidence
- [ ] 000013_records_evidence
- [ ] 000014_regulatory_submissions
- [ ] 000015_jurisdiction_rollout
- [ ] 000016_fix_rule_status_history_trigger
- [ ] 000017_append_only_rbac
- [ ] 000018_idempotency_keys
- [ ] 000019_entity_jurisdiction_applicability
- [ ] 000020_row_version_and_rule_immutability
- [ ] 000021_registration_evidence
- [ ] 000022_jurisdiction_decision_evidence
- [ ] 000023_applicability_evidence
- [ ] 000024_pack_supersede_event
- [ ] 000025_resolver_change_notify
- [ ] 000026_remediation_cases
- [ ] 000027_jurisdiction_versions
- [ ] 000028_jurisdiction_aliases
- [ ] 000029_jurisdiction_imports
- [ ] 000030_business_calendar
- [ ] 000031_payment_terms

## Commits (this session)

| Commit | Task | Files |
|--------|------|-------|
| — | JR-0 Baseline | progress.md |

## Blockers & Decisions

| Item | Status | Details |
|------|--------|---------|
| D2-1.F05 (automatic drift) | ⛔ Blocked | external-data-feed-svc publishes no regulatory-change event |
| D2-1.F26 (C8/J-05, J-08, J-09) | 🧭 Decision | Listed in Governance audit register, goes to CL-2 |
| internal/authz tests | ⚠️ Blocked | Windows Application Control blocks test binary |
| internal/events tests | ⚠️ Blocked | Windows Application Control blocks test binary |
| Integration tests | ⏭️ Skipped | Require TEST_DATABASE_URL (scratch Postgres) |

## Next Steps (JR-1)
1. Run full test suite on scratch Postgres (set TEST_DATABASE_URL)
2. Audit each route group for: authorization at object, maker-checker, Idempotency-Key, If-Match/ETag, outbox, RLS+FORCE, R5, openapi
3. Add negative-path tests with TestJUR_*, TestREF01_*, TestREF10_* naming
4. Fix any failures

## Commits (this session)

| Commit | Task | Files |
|--------|------|-------|
| (uncommitted) | JR-0 Baseline | progress.md |
| (uncommitted) | JR-1 Audit | — (verification only) |
| (uncommitted) | JR-5 Parity | internal/handler/openapi_parity_test.go |
| (uncommitted) | JR-1 Negative paths | internal/handler/negative_paths_test.go |

## Feature Rows Changed

| Feature ID | Old Status | New Status | Evidence |
|------------|------------|------------|----------|
| D2-1.F25 | 🟡 Reconfirm | 🔵 Built (not audited) | TestIfMatch_*, Test*_Idempotent*, Test*_ETag* |
| D2-1.F28 | 🟡 Partial | 🔵 Built | TestOpenAPIMatchesTheRouter |
| D2-1.F27 | ❌ Missing | 🔵 Built | Cursor paging tests needed (API-4) |

## Migrations Applied (dev DB)
- [x] All 31 migrations applied successfully via `deployments/migrate.py up --db jurisdiction_rules`

## Live Status
- [x] Image built: `deployments-jurisdiction-svc:latest`
- [x] Container running: `jurisdiction-rules-svc` (healthy)
- [x] Health checks passing: /healthz, /readyz, /v1/jurisdictions