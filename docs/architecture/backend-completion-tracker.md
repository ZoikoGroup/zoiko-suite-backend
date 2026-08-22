# Backend Completion Tracker

Single, consolidated list of everything confirmed **not built / not complete** against
`docs/original_doc/` (the 7-document spec: Doc 01–06 series + Doc 07 commercial standard),
verified against the real codebase on 2026-08-21 (not just re-stated from older tracking
docs — see "Verification method" per item).

This supersedes nothing — `known-gaps.md`, `doc7-implementation-backlog.md`, and
`full-architecture-gap-analysis.md` remain the detailed narrative record of *how* each
finding was discovered. This file exists to be worked top-to-bottom, one row at a time.

## Working rule

**Exactly one row at a time.** For each row:
1. Move it to `In Progress`.
2. Implement the fix for that row only.
3. Run the full verification loop: `gofmt -w .` → `go build ./...` → `go vet ./...` →
   `go test ./... -count=1` on every service touched.
4. Write/extend a test that specifically proves the fix (not just that nothing broke).
5. Commit + push to `satyaprakash-changes` (no PR — per standing instruction, one
   consolidated PR happens at the end of the whole tracker, not per row).
6. Mark the row `Done`, with the commit hash and a one-line note on what was verified.
7. Only then move to the next row.

Do not start a second row before the current one is `Done`. Do not batch multiple rows
into one commit.

## Status values
`Not Started` / `In Progress` / `Done` / `Blocked` (needs a decision this tracker can't make)

---

## Priority 1 — Tier-0 governance services with zero row-level security — ✅ COMPLETE

The Doc 03 §06 "must exist before broad functional expansion" services that had a
`tenant_id` column and **no `CREATE POLICY` / `ENABLE ROW LEVEL SECURITY` at all** — verified
by grepping every migration file, not inferred. Pattern to copy: `governance-decision-log-svc`'s
`000002_add_rls.up.sql` + `000006_force_rls.up.sql`.

**Closed 2026-08-22.** 7 real services fixed (row 4 was a false positive — genuinely
platform-wide reference data). All 7 verified against a real Postgres 16 as a purpose-created
`NOSUPERUSER NOBYPASSRLS` role, each with a negative control.

### What this tier actually taught us — read before starting Priority 2

Four lessons that cost real time here and will recur:

1. **A superuser bypasses RLS unconditionally, `FORCE` included.** `TEST_DATABASE_URL` points
   at `postgres`. An isolation test over that connection proves only that the application
   predicate works — it never touches the policy. Every row here needed a purpose-created
   ordinary role. The first version of row 6's test passed for exactly this wrong reason.
2. **Always run a negative control.** Remove the migration; the test must fail. Row 8 also
   showed the *reverse* control matters: an over-restrictive policy is a real failure mode
   too (it hid global defaults), and only a test that checks for it will catch it.
3. **Three services needed a deliberate exemption, and getting it wrong is an outage, not a
   tightening.** audit-event-store-svc (Kafka writer, global hash chain — a naive policy
   stops the evidence store accepting events *silently*), authorization-svc
   (`FindGrantedActions` is the core `/v1/authorize` path — scoping it breaks all
   authorization platform-wide), secret-vault-integration-svc (cross-tenant admin actions).
   Check for a legitimate cross-tenant caller *before* writing the policy, not after.
4. **Test harnesses that name migrations individually silently skip new ones.** Rows 7 and 8
   both had this: the migration under test would not have been applied at all. Check the
   suite globs `*.up.sql` before trusting a green run.

| # | Service | Spec ref | Status | Notes |
|---|---|---|---|---|
| 1 | identity-context-svc | Doc 03 §06, Doc 04 §2.2 | Done | `045ae84`. Also found & fixed a real cross-tenant hole: GET/PUT /v1/principals/{id} routes had no X-Tenant-Id check at all. RLS (ENABLE+FORCE+WITH CHECK) added on all 4 tables; 4 isolation tests + 2 handler tests live-verified against real Postgres 16. |
| 2 | secret-vault-integration-svc | Doc 03 §06, Doc 04 §2.2 | Done | `6343434`. Real bug: ListVersionHistory had zero tenant scoping. RLS added with a documented `app.platform_scope` bypass for the 2 genuinely cross-tenant admin actions (ActivateVersion, Rotate). 9 tests live-verified against real Postgres 16, including 2 proving the bypass itself works. |
| 3 | policy-svc | Doc 03 §06, Doc 04 §2.2 | Done | `14602c6`. Real bug: ListVersionHistory had zero tenant scoping (same shape as row 2's fix). RLS on policy_versions only (other 4 tables are platform-wide, no tenant_id). No platform-scope bypass needed — ActivateVersion here is genuinely tenant-scoped. 7 tests live-verified against real Postgres 16. |
| 4 | jurisdiction-rules-svc | Doc 03 §06 | **Not applicable** | False positive in the original audit — the only "tenant_id" hit in its migrations is a comment ("...reference data, not per-tenant data. No tenant_id column."), not a real column. This service is genuinely platform-wide reference data (matches Doc 03's own design — jurisdiction-rules-svc *is* the jurisdiction concept). No RLS is possible or correct here; fabricating a tenant boundary would violate the "never fabricate a signal with nothing real to populate it" doctrine. Removed from the count of 8 — real Tier-0 count is 7. |
| 5 | authorization-svc | Doc 03 §06, Doc 04 §2.2 | Done | `de2dfc9`. Real, severe bug found beyond the RLS gap: all 7 `/v1/admin/*` write routes had NO authentication at all — tenant_id and actor attribution came straight from the request body. Fixed all 7 (tenant verification, actor from X-Principal-Id, delegator-must-be-caller). RLS added on `roles`/`sod_rules` only (the only 2 tables with a real tenant_id). Two reads (`FindRoleByID`, `FindGrantedActions`) needed a deliberate platform-scope bypass — `FindGrantedActions` is the core `/v1/authorize` path called on nearly every request platform-wide; scoping it by tenant would have silently broken all authorization. 37 tests live-verified against real Postgres 16. |
| 6 | workflow-svc | Doc 03 §06, Doc 04 §2.2 | Done | `9a5f748`. Two real bugs beyond RLS: (1) `FindWorkflowByID` — the choke point all Store methods route through — fell back to an UNSCOPED lookup when X-Tenant-Id was omitted (document-vault-svc's "filter that disables itself" shape); (2) `initiated_by`/`actor_principal_id` came from the request body on every route, making the existing SoD checks self-declared rather than load-bearing. RLS on `workflow_instances`. 31 tests live-verified against real Postgres 16 — including a purpose-created NOSUPERUSER NOBYPASSRLS role for the no-tenant probe (a superuser bypasses RLS unconditionally, so the first version of that test passed for the wrong reason) plus an explicit negative-control run with the migration removed. |
| 7 | audit-event-store-svc | Doc 03 §06, Doc 04 §2.2 | Done | `f02b270`. Different shape from rows 1–6: a naive tenant-only FORCE policy here would have taken the platform's append-only evidence store **offline** — no tenant-context plumbing exists (Kafka consumer, no X-Tenant-Id), the hash chain is deliberately global (Doc 04 §15.4), and `sequence_number` is UNIQUE, so the chain-tip SELECT would match zero rows and WITH CHECK would reject every insert, silently (DLQ absorbs a consumer's error). Proven by negative control, not assumed. RLS added with an explicit `app.platform_scope` exemption set only by `PgStore.Store`. Also replaced the test suite's hand-maintained `const schema` copy with `applyMigrations()` reading the real files — the copy would have made this very migration untested. 4 integration tests live-verified against real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role. |
| 8 | configuration-feature-flag-svc | Doc 03 §06, Doc 04 §2.2 | Done | `7058337`. Simplest row — the app layer was already correct (all 6 handlers call `requireTenant`, every store method already takes the tenant, list routes already refuse a foreign `?tenant_id=`), so this was purely the DB backstop with no application bug alongside. RLS on both tables, keeping NULL-tenant_id global defaults readable by everyone — a plain `tenant_id = app.tenant_id` policy would hide every global default and turn applicable config into "not found". No platform-scope bypass needed (no legitimate cross-tenant caller exists). Also fixed the suite's setup naming only `000001`, which would have left this migration untested. 16/16 tests pass against real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role, with negative controls in **both** directions (missing policy → leak; over-restrictive policy → global defaults hidden). |

**Verification method per row**: add a `TestPgStore_RLS_TenantIsolation`-style test (same
pattern as tenant-entity-registry-svc's) that creates two tenants and proves a query scoped
to tenant A cannot see tenant B's rows, against a real Postgres instance.

⚠️ **Two ways an RLS test passes for the wrong reason** — both hit during row 6, both worth
checking before marking any row Done:

1. **Connected as a superuser.** `TEST_DATABASE_URL` normally points at `postgres`, and a
   SUPERUSER bypasses row-level security *unconditionally* — `FORCE` does not change this.
   A test asserting isolation while connected as the superuser proves only that the
   application-level `WHERE tenant_id = $n` predicate works, and nothing at all about the
   policy the row adds. For any assertion that the *policy itself* closes a gap (e.g. a
   missing-tenant fallback the app predicate deliberately leaves open), connect as a
   purpose-created `NOSUPERUSER NOBYPASSRLS` role — see workflow-svc's `appRolePool` helper
   for the pattern. This mirrors the platform's real runtime role (`zoiko_app`).
2. **The app predicate already covered it.** If the test would pass with the migration
   deleted, it is testing the handler/store code, not the RLS policy. Run the negative
   control explicitly: temporarily remove the `_add_rls.up.sql` file, confirm the test
   fails, restore it, confirm it passes.

## Priority 2 — Remaining non-Tier-0 services with zero row-level security

Same defect, lower severity (not on the governance critical path), still a real gap.
Re-verified individually (2026-08-21) — 6 of these use `NNN_init.sql` naming rather than
golang-migrate's `NNNNNN_name.up.sql`, which the original audit's glob pattern would have
missed if re-run naively; checked their actual file contents directly instead.

| # | Service | Status | Notes |
|---|---|---|---|
| 9 | ai-governance-svc | Not Started | |
| 10 | banking-connector-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |
| 11 | commercial-account-svc | Not Started | |
| 12 | connectivity-api-bridge-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |
| 13 | esignature-integration-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |
| 14 | evidence-manifest-svc | Not Started | |
| 15 | external-data-feed-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |
| 16 | hris-connector-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |
| 17 | kill-switch-registry-svc | Not Started | |
| 18 | retention-registry-svc | Not Started | |
| 19 | source-authority-svc | **Not applicable** | False positive — its only "tenant_id" mention is a comment comparing its real column (`entity_ref`, free-text, no tenant dimension) to kill-switch-registry-svc's design. Genuinely platform-wide reference data; no fix needed. |
| 20 | tax-authority-interface-svc | Not Started | Verified real: `tenant_id VARCHAR(64) NOT NULL` in `001_init.sql` |

## Priority 3 — RLS enabled but not FORCEd (defense-in-depth only)

40 services (list below) have `ENABLE ROW LEVEL SECURITY` + a policy, but not `FORCE ROW LEVEL
SECURITY`. **Lower urgency than Priority 1/2** — the platform's `zoiko_app` runtime role is a
non-owner, so RLS already applies to normal traffic; FORCE only matters if something connects
as the table owner (a future regression, manual psql access, etc). Worth doing, but after 1–2.

| # | Service | Status |
|---|---|---|
| 21 | access-control-svc | Not Started |
| 22 | anomaly-detection-svc | Not Started |
| 23 | benefits-svc | Not Started |
| 24 | clause-template-svc | Not Started |
| 25 | compensation-svc | Not Started |
| 26 | compliance-risk-scoring-svc | Not Started |
| 27 | compliance-status-svc | Not Started |
| 28 | consolidation-svc | Not Started |
| 29 | contract-lifecycle-svc | Not Started |
| 30 | corporate-actions-svc | Not Started |
| 31 | corporate-tax-svc | Not Started |
| 32 | counterparty-management-svc | Not Started |
| 33 | decision-support-svc | Not Started |
| 34 | employee-master-svc | Not Started |
| 35 | employment-contracts-svc | Not Started |
| 36 | exception-escalation-svc | Not Started |
| 37 | filing-preparation-svc | Not Started |
| 38 | filing-tracker-svc | Not Started |
| 39 | forecasting-svc | Not Started |
| 40 | intercompany-accounting-svc | Not Started |
| 41 | invoice-approval-svc | Not Started |
| 42 | leave-absence-svc | Not Started |
| 43 | migration-integrity-svc | Not Started |
| 44 | obligation-tracking-svc | Not Started |
| 45 | offboarding-severance-svc | Not Started |
| 46 | org-structure-svc | Not Started |
| 47 | payroll-exceptions-svc | Not Started |
| 48 | payroll-run-svc | Not Started |
| 49 | payroll-tax-svc | Not Started |
| 50 | performance-review-svc | Not Started |
| 51 | procurement-workflow-svc | Not Started |
| 52 | reconciliation-intelligence-svc | Not Started |
| 53 | reporting-orchestration-svc | Not Started |
| 54 | tax-determination-svc | Not Started |
| 55 | tax-rules-svc | Not Started |
| 56 | tenant-entity-registry-svc | Not Started |
| 57 | treasury-svc | Not Started |
| 58 | vat-gst-svc | Not Started |
| 59 | withholding-tax-svc | Not Started |
| 60 | workflow-history-svc | Not Started |
| 61 | workforce-compliance-svc | Not Started |

## Priority 4 — Structural / cross-cutting

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 62 | Shared Go module enforcing the event envelope shape — today every service hand-copies its own struct; nothing stops the next new service from drifting | Doc 03 §19 | Not Started | |
| 63 | Transactional outbox rollout beyond the 1-service pilot (`commercial-account-svc`) — every other service can still silently drop an event on a crash between DB commit and Kafka publish | Doc 03 §17.3, Doc 07 §L1–L2 | Not Started | Verified: `grep -rl "internal/outbox" services/*/internal` returns only commercial-account-svc |
| 64 | General-purpose saga / compensating-transaction coordinator — only one flow (procurement-workflow-svc) got a one-off fix; no reusable pattern for the next multi-service flow | Doc 03 §17.8 | Not Started | |

## Priority 5 — Governance plane completeness

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 65 | No single enforced governance pipeline sequence — each service calls whichever governance engines it individually decided on | Doc 01 §07, Doc 03 | Not Started | |
| 66 | jurisdiction-rules-svc has no compliance calendar entity; `jurisdiction.calendar.changed` is declared but unemittable | Doc 03 §8.2 | Not Started | |
| 67 | authorization-svc has no platform-scoped, non-entity resource concept — services fake a synthetic `legal_entity_id` as a workaround | Doc 03 (spec silence, not a violation) | Not Started | |
| 68 | A DENIED governance decision doesn't auto-convert to an approval workflow except where explicitly wired per-service | Doc 02 Diagram 2 | Not Started | |
| 68a | **audit-event-store-svc has no query API at all.** Doc 03 §14.1 requires its records to be "immutable and **queryable** by actor, entity, action, workflow, or time range". The service has exactly one store method (`Store`) and only `/healthz` + `/readyz` routes — evidence goes in and cannot be got out. Found while doing row 7. The RLS policy added there is deliberately shaped so this API inherits tenant scoping by default when built. | Doc 03 §14.1 | Not Started | Real missing feature, not just missing RLS. Note the evidence is genuinely durable and hash-chained — it is only unreadable, which makes this a retrieval gap rather than a data-loss one. |

## Priority 6 — Data model (Doc 04)

| # | Item | Status | Notes |
|---|---|---|---|
| 69 | Missing entity: `UltimateBeneficialOwner` (no table anywhere) | Not Started | Verified: zero grep hits |
| 70 | Missing entity: `FiscalCalendar` (dangling FK column exists, no table) | Not Started | Verified: zero grep hits |
| 71 | Missing entity: `TaxLogicSnapshot` (dangling FK in 2 services) | Not Started | Verified: zero grep hits |
| 72 | Missing entity: `GrossToNetCalculationLog` | Not Started | Verified: zero grep hits |
| 73 | Missing entity: `NexusRecord` | Not Started | Verified: zero grep hits |
| 74 | Missing entity: chart-of-accounts (`Account`) in general-ledger-svc | Not Started | general-ledger-svc's own migration comment admits this |
| 75 | Missing entity: `SchemaDependencyMap` (+ `compatibility_mode`) | Not Started | Verified: zero grep hits |
| 76 | Missing entity: standalone `VendorProfile` (only scattered FK-shaped columns) | Not Started | Verified: zero grep hits |
| 77 | Document Vault missing `virus_scan_status` and `digital_signature_id` (Doc 04 §15.5 requires both) | Not Started | Verified: zero grep hits in document-vault-svc migrations |
| 78 | Obligation tracking duplicated across 3 services with non-identical schemas — violates §2.1 single-owner doctrine | Doc 04 §2.1 | Not Started | Verified: obligations-svc, obligation-tracking-svc, and filing-tracker-svc each have their OWN separate `obligations`/`filing_requirements` table |
| 79 | Identity/role assignment duplicated across authorization-svc and identity-context-svc | Not Started | |
| 80 | No field-level encryption/classification tagging on tax ID / bank reference / payroll columns anywhere outside document-vault-svc | Doc 04 §2.8, §20 | Not Started | |
| 81 | authorization-svc owns its own `delegated_authorities` table, duplicating delegated-authority-svc's ownership of the same concept (Doc 03 §9.3 names Delegated Authority Service as the authoritative owner — a separate service) | Doc 04 §2.1 | Not Started | Found while fixing row 5's auth gap (2026-08-21). Not addressed there — this is a cross-service consolidation decision, same class as item 78, not a quick fix |
| 82 | authorization-svc's `permission_bundles`, `principal_role_assignments`, `delegated_authorities`, and `access_decision_log` carry no `tenant_id` column at all — only `legal_entity_id` (and `delegated_authorities` not always that). RLS was only possible on `roles`/`sod_rules`, the 2 tables that actually have the column | Doc 04 §2.2 | Not Started | Found during row 5. Fabricating a `tenant_id` column on tables that were never given one is a data-model change, not an RLS migration — deliberately not done in that row |

## Priority 7 — Security (Doc 05) — capability exists, incomplete

| # | Item | Status | Notes |
|---|---|---|---|
| 81 | secret-vault-integration-svc's broker never returns real secret material — no service can bootstrap a runtime credential through it | Blocked | Needs a vault-side API design decision, not just more wiring — see `known-gaps.md` |
| 82 | key-management-svc is metadata CRUD only — never actually used to encrypt/decrypt anything | Not Started | |
| 83 | No confidential computing / TEE anywhere (spec calls for it on payroll/tax calculation) | Not Started | |
| 84 | No PAM / break-glass / just-in-time elevation anywhere | Not Started | |

## Priority 8 — Testing

| # | Item | Status | Notes |
|---|---|---|---|
| 85 | Most services' store layers are tested only against stubs, not real Postgres — only jurisdiction-rules-svc, identity-context-svc, tenant-entity-registry-svc, vendor-due-diligence-svc have real integration coverage | Not Started | |
| 86 | No contract tests, load/performance tests, or DR/restore tests anywhere | Not Started | |

---

## Explicitly NOT on this tracker (not backend-engineering work)

- Numeric SLOs, ZoikoSuite's own merchant/tax/processor billing setup, Doc 07 §27 sign-off by
  named function owners, per-service safe-degraded-mode definitions, source-authority-svc's
  real precedence data for actual connected systems — all blocked on a human/business decision,
  not code (see `full-architecture-gap-analysis.md`).
- CI security scanning, IaC coverage, staging/QA environments — devops, not backend service code.
- Frontend console wiring (identity-context-svc / workflow-svc have no console page) — a
  different teammate's lane per current team split.

## Change log

- 2026-08-21 — Tracker created. Priorities 1–3 (RLS) counts verified fresh against all 153
  migration files in `services/*/deployments/migrations/`, not copied from prior docs. Priority
  4 item 63's "1 service only" outbox claim re-verified by grep. Priority 6 items 69–77's "zero
  hits" re-verified by grep — all still genuinely absent as of this date.
