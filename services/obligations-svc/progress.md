# obligations-svc — Progress

**Status (23 Nov 2026):** Doc 03 §8.5 surface plus R-001 WFC-05 deadline clocks, rebuilt and tested
at unit and store level. Not yet run live (image, dev DB migration, dependency E2E, console): see
"Open" below. Plan and step table: `docs/audit_files/DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md`
§D2-4. Contracts: `openapi.yaml` (kin-openapi valid, router parity tested) and `asyncapi.yaml`
(two-way contract test).

This file replaces the 18–19 Nov version, which cited tests that never existed (`TestOB1_*`,
`TestWF3_*` before 23 Nov) and claimed S0–S6.

## Suite (23 Nov 2026, commit after `3b3de75d`)

`go build ./...` and `go vet ./...` clean. `go test -count=1 -p 1 ./...` with
`TEST_DATABASE_URL` on a scratch database (`obligations_test_ob`): **129 pass, 0 fail, 0 skip**.
The store suite resets the whole `public` schema before each test and runs the store as a
NOSUPERUSER, NOBYPASSRLS role that does not own the tables (`rlsPool`).

## Owns (Doc 03 §8.5) and where

| Concern | Where | Tests |
|---|---|---|
| Obligations (entity- and jurisdiction-bound, fail closed) | `POST/GET /v1/obligations`, `GET/PATCH …/{id}` | `TestCreateObligation_*`, `TestPgStore_CreateObligation_IdempotencyAnd409`, `TestPatchObligation_NeedsIfMatchAndAuthorizesAtTheEntity` |
| Status OPEN → IN_PROGRESS / OVERDUE / CLOSED | `POST …/{id}/status`; overdue sweep | `TestUpdateObligationStatus_*`, `TestPgStore_UpdateObligationStatus_*`, `TestPgStore_OverdueSweepFindsTenantsUnderRLS` |
| Workflow link | `POST/DELETE …/{id}/workflow` | handler suite |
| Filing requirements and their lifecycle | `…/filing-requirements[/{fr}[/status|/transitions]]` | `TestPgStore_FilingLifecycleAndHistory`, `TestPgStore_FilingRequirement_CreateAndList`, `TestFilingStatus_ValidationAndRoute` |
| Definitions, versions, entity mappings | `/v1/obligation-definitions…` | `TestPgStore_DefinitionsMappingsAndPinnedObligations`, `TestDefinitions_CreateAndRaiseFromDefinition` |
| Applicability decisions (append-only, UNASSESSED never stored, exactly one decider) | `…/applicability-decisions`, `…/applicability` | `TestCreateApplicabilityDecision_*`, `TestPgStore_ApplicabilityIdempotencyKey`, `TestOB2_*` |
| **WFC-05 deadline clocks** | see below | `TestWFC05_*`, `TestWF3_*` |

## WFC-05 deadline clocks (R-001 §7, §9, §10)

| Rule | How | Tests |
|---|---|---|
| Classes HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY | 000016 CHECK; SOFT_INTERNAL → INTERNAL_SLA, EXTENSION → HARD_LEGAL (old value kept for the down) | `TestMigration000016_UpDownUp_KeepsOld000014Rows` |
| Due date only from jurisdiction-rules (R-INV-15/16) | `POST …/clocks` calls `POST /v1/obligation-calculations:calculate`; any outcome but DUE_DATE_CALCULATED is 422 naming it; outage 503; no due-date field exists | `TestWF3_StartClock_OnlyACalculatedAnswerMakesAClock`, `TestWF3_StartClock_ACallerDueDateIsRefused` |
| Provenance and trigger evidence stored verbatim (NP-12) | rule, version, digest, calendar code/version/id, time zone, convention, anchor/unadjusted/local due date, extension, pack, decision id; trigger event id/type/time, business time, source object, ingestion latency | `TestWFC05_StartClock_ProvenanceRoundTripsAndKeyIsIdempotent` |
| due_at immutable; history never deleted (R-INV-17) | trigger `obligation_clocks_immutable_provenance`; delete and update refused on clocks, transitions, milestones, escalation events | `TestWFC05_DueAtAndProvenanceAreImmutable_AndNothingIsDeleted` |
| Computed state (R-INV-26, NP-16) | `domain.EffectiveState`; every read resolves at now | `TestWFC05_EffectiveState_FrozenClockReadsOverdueWithoutSweep`, `TestWF3_GetClock_ReadsOverdueWithoutASweep`, `TestWFC05_FrozenClockOverdueOnRead_SweepPersistsOnce` |
| Sweep persists once (NP-37) | row lock + state/version predicate + unique (clock_id, row_version) transitions | `TestWFC05_ConcurrentSweepsPersistOneMove` |
| Fulfilled after overdue keeps the overdue period (NP-38) | `overdue_since` set once, never cleared (trigger) | `TestWFC05_SatisfyAfterOverdue_KeepsTheOverduePeriod` |
| Fulfilment needs evidence (R-INV-14, NP-17) | evidence-requirements `POST /v1/evidence/evaluate` (MISSING 409, outage 503, NO_REQUIREMENTS_DEFINED accepted with refs and logged); DB CHECK | `TestWF3_SatisfyClock_NeedsAcceptedEvidence`, `TestWFC05_FulfilledWithoutEvidenceIsRefusedByTheDatabase` |
| Waiver / N-A with authority (NP-18) | `OBLIGATION_CLOCK_WAIVE` / `_NOT_APPLICABLE` grant; APPROVED `OBLIGATION_WAIVER` / `OBLIGATION_NOT_APPLICABLE` workflow about this clock, released for the subject fingerprint (verify-release), decided by another principal; N/A links a NOT_APPLICABLE decision; DB CHECK approver ≠ requester | `TestWF3_WaiveClock_AuthorityAndASecondPerson`, `TestWF3_NotApplicable_NeedsANotApplicableDecisionOfThisObligation`, `TestWFC05_WaiverNeedsADistinctApprover_AndNALinksADecision` |
| Supersession (R-INV-17, NP-14, NP-32) | `POST …/recalculate`; calendar consumer on `business-calendar.updated` / `jurisdiction.calendar.changed` (replay-safe) | `TestWFC05_Supersede_KeepsTheOldClockAndLinksBothWays`, `TestWF3_Recalculate_SupersedesOnlyWhenTheAnswerChanges`, `TestWFC05_CalendarCorrection_*` |
| Escalation never changes the clock (R-INV-19) | milestones fire once; RAISE_PRIORITY, ADD_OWNER, OPEN_CASE (outbox WORKFLOW_CASE → workflow-svc `POST /v1/cases`), NOTIFY, REQUIRE_APPROVAL; versioned escalation policies | `TestWFC05_MilestoneFiresOnce_EscalationNeverChangesTheClock`, `TestWFC05_EscalationPolicyVersionsAreImmutable`, `TestWFC05_WorkerTick_*` |
| DST (R-INV-18) | the instant is jurisdiction-rules' answer (`ResolveLocalInstant`, JR-DST-1), stored with its time zone | jurisdiction `TestResolveLocalInstant_Rules`; `TestWFC05_DueDayStart_UsesTheClockZone` |
| ST-2 / ST-3 | `…/available-actions`, `…/transitions`; `…/status` is a deprecated alias (`Deprecation: true`) that only activates | `TestWF3_AvailableActions_FollowStateAndGrants`, `TestWF3_ClockStatusRoute_IsADeprecatedAlias` |
| Tenant isolation | FORCE RLS with WITH CHECK on every clock table; system workers read across tenants only under `app.clock_sweep` | `TestWFC05_RLS_AnotherTenantCannotSeeTheClock` |
| Events (R-001 §10.3 envelope) | `asyncapi.yaml` | `TestAsyncAPI_MatchesTheCode` |
| Metrics and alerts | `obligation_clocks_overdue`, `obligation_clock_sweep_lag_seconds`, `obligation_escalation_failures_total`, …; `deployments/prometheus-rules.yml` group `obligations` (promtool SUCCESS) | — |

Grants: access-control 000062 registers the six clock actions (OBLIGATION_MANAGER v2,
OBLIGATION_AUTHORITY v1, COMPLIANCE_CATALOGUE_ADMINISTRATOR v2); authorization 000044 provisions
`svc-obligations` (OBLIGATION_CALCULATION_CALCULATE, CASE_OPEN) in every tenant
(`TestOBS8_ObligationsServiceIdentityProvisionedWithATenantsFirstRole`).

## Mutation check (23 Nov 2026)

17 mutants, all killed: computed OVERDUE ×2, provenance required ×2, waiver authority ×3,
supersession keeps the old clock ×2, satisfy needs evidence ×2, applicability deciders ×2,
sweep once (all three guard layers removed; a single layer removed survives because the other two
still hold), milestone fires once (app guards removed; the DB trigger holds). Runner:
`D:\zoikocomp\.cache\tools\mutate.py` with the specs `ob_mutants*.json` there.

## Decision D-OB-1 (user, 23 Nov 2026)

ComplianceStatus, ExceptionCase and EscalationRecord are not owned here (Doc 03 §13.7, §13.9,
§18). 000015 drops their tables; the 11 routes are gone (404). An overdue clock opens a workflow
case and emits `obligation.*` events their owners can consume.

## Open

- OB-S16 identity `require`: callers still send headers in compose (`warn`).
- OB-S18/S19 live: image rebuild, `migrate.py up --db obligations` (000015, 000016), rewritten
  `e2e-obligations-deps.py`, `scripts/audit.sh`.
- The escalation case relay needs workflow-svc's `POST /v1/cases` with `source_type` /
  `source_id` / `milestone` idempotency (WF-S2, WF-S7) and its mTLS peer listener (WF-S16).
- NOTIFY needs a notification-svc template and trigger for `obligation.escalated` (D1-7).
- OB-FE-1…3 console; C8 single owner (cross-team); R-OD-05 (clock pauses: none built),
  R-OD-06 (evidence classes per domain) on the decision sheet.
