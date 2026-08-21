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

## Priority 1 — Tier-0 governance services with zero row-level security

The 8 of 11 Doc 03 §06 "must exist before broad functional expansion" services that have a
`tenant_id` column and **no `CREATE POLICY` / `ENABLE ROW LEVEL SECURITY` at all** — verified
by grepping every migration file, not inferred. Pattern to copy: `governance-decision-log-svc`'s
`000002_add_rls.up.sql` + `000006_force_rls.up.sql`.

| # | Service | Spec ref | Status | Notes |
|---|---|---|---|---|
| 1 | identity-context-svc | Doc 03 §06, Doc 04 §2.2 | Done | `045ae84`. Also found & fixed a real cross-tenant hole: GET/PUT /v1/principals/{id} routes had no X-Tenant-Id check at all. RLS (ENABLE+FORCE+WITH CHECK) added on all 4 tables; 4 isolation tests + 2 handler tests live-verified against real Postgres 16. |
| 2 | secret-vault-integration-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 3 | policy-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 4 | jurisdiction-rules-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 5 | authorization-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 6 | workflow-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 7 | audit-event-store-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |
| 8 | configuration-feature-flag-svc | Doc 03 §06, Doc 04 §2.2 | Not Started | |

**Verification method per row**: add a `TestPgStore_RLS_TenantIsolation`-style test (same
pattern as tenant-entity-registry-svc's) that creates two tenants and proves a query scoped
to tenant A cannot see tenant B's rows, against a real Postgres instance.

## Priority 2 — Remaining non-Tier-0 services with zero row-level security

Same defect, lower severity (not on the governance critical path), still a real gap.

| # | Service | Status | Notes |
|---|---|---|---|
| 9 | ai-governance-svc | Not Started | |
| 10 | banking-connector-svc | Not Started | |
| 11 | commercial-account-svc | Not Started | |
| 12 | connectivity-api-bridge-svc | Not Started | |
| 13 | esignature-integration-svc | Not Started | |
| 14 | evidence-manifest-svc | Not Started | |
| 15 | external-data-feed-svc | Not Started | |
| 16 | hris-connector-svc | Not Started | |
| 17 | kill-switch-registry-svc | Not Started | |
| 18 | retention-registry-svc | Not Started | |
| 19 | source-authority-svc | Not Started | |
| 20 | tax-authority-interface-svc | Not Started | |

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
