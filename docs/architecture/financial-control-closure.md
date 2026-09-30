# ZS-CONTROL-001: closure pass, blocker register and operating steps

Scope of this pass: finish everything that is safely implementable inside the current architecture and record,
exactly, what is not. No new document, no new service, no invented business rule.

## 1. General-ledger event callers (ZS-ACC-KERNEL-001 mandatory fields)

`POST /v1/postings/events` now requires `transaction_currency` and `document_date` (`posting_date` optional). Every
caller was traced to its own data. **Only one posting could be fixed without inventing a value.**

| Service | Posting | Currency source | Business-date source | Result |
|---|---|---|---|---|
| asset-management-svc | asset-event apply | `asset_events.currency` (caller-supplied; apply refuses with 422 if absent) | `asset_events.effective_date` | **Fixed** |
| asset-management-svc | depreciation run emit | none: no currency on runs, lines, schedules or assets | none: `fiscal_period` is a name only | **Not fixed** |
| consolidation-svc | elimination adjustment | none on the adjustment; `target_currency` is an untranslated run label and an adjustment has no run link | none | **Not fixed** |
| inventory-management-svc | valuation emit, write-down | none: no inventory table carries a currency | none: run and write-down carry only `fiscal_period` | **Not fixed** |
| project-accounting-svc | recognition run emit | `financial profile.currency` exists | none: run has only processing timestamps | **Not fixed** |
| treasury-svc | cross-entity transfer journal | `TreasuryTransfer.CurrencyCode` exists | none: no value/execution date on the transfer | **Not fixed** |

Until the missing capabilities exist, the four unfixed callers are refused by the GL with 400 (before this pass they
failed with 503, so nothing that worked has been broken). What each needs, and where it belongs:

* **A business date** on: depreciation runs, consolidation adjustments, valuation runs and write-downs, recognition
  runs, treasury transfers. Options named by the investigations: an explicit date on the request stored with the
  document (needs a migration in each service), or a fiscal-calendar capability returning a period-end date from
  `fiscal_period` and legal entity (no Fiscal Calendar service exists; financial-close-svc's period check returns only
  open/closed).
* **A currency** on: depreciation (asset or book), consolidation adjustments, inventory valuation and write-downs.
  The legal entity's functional currency belongs to REF-02 (Currency Registry), which does not exist;
  `default_currency_code` from tenant-entity-registry is a tenant default, not a functional currency, and was not used.
* Deriving a period end from a `YYYY-MM` string, or using `time.Now()`, would be a guess or a processing date; both
  were refused. Treasury needs an explicit decision if "processing date captured once at submission" is acceptable.

## 2. Reversal events

`ReverseJournal` now writes the reversing journal's ledger entries atomically but emits only `journal.reversed`
(payload: original journal id and `reversing_journal_id`). Emitting `journal.posted` for the reversing journal was
investigated and **not** done: ZS-ACC-KERNEL-001 defines `JournalReversed` as the event for "reversal posts" and never
requires a second posted fact; ZS-EVENT-001 test EV-T19 says the original posted event remains and the reversal
emits new facts; no consumer of either event exists in the repository; and two existing contract tests
(`outbox_atomicity_test.go`, `control_population_wave4_integration_test.go`) assert the absence. A future consumer that
must apply reversals without a fetch should get an enriched `journal.reversed`, not a `journal.posted`.

## 3. Authorization actions

Twelve `FINCTRL_*` actions are used in code and all twelve are in the seed bundle `FINCTRL_FULL` in
`deployments/scripts/seed-demo-rbac.ps1`; the eleven `*_CONTROL_POPULATION_READ` actions declared by the source
services are in bundle `FINCTRL_POPULATION_READ`. Names follow the flat `UPPER_SNAKE` vocabulary every service and seed
uses (ZS-IAM-001 Appendix A's dotted names are a documentation form). authorization-svc has no action catalogue: an
action is grantable once it appears in a role's permission bundle, so no authorization-svc change is needed and none
was made. No duplicate concept exists (`grep` finds no overlapping control/reconciliation actions).

**Operational step (not automated).** Real environments must grant these through their normal RBAC process, using
separate roles for the segregated duties: define/approve-rule/policy, run-create/execute, certify, waive, reperform,
evidence-export. The demo bundle deliberately grants everything to one role; the service still refuses a certifier
who created or took part in a run, and a waiver by the exception's own owner. Whoever executes a control also needs
the population-read action of every source that control reads.

## 4. Financial-close gate: enabling it

`FINCTRL_CLOSE_GATE_MODE` on financial-close-svc: unset/`off` = the gate is never consulted (default, and the compose
value); `enforce` = closing fails closed unless financial-control-svc reports the gate open; any other value is
treated as `enforce` and logged at startup. Nothing in this pass sets it to `enforce` in any environment.

Order of enablement: (1) seed control definitions and approve their rule versions in financial-control-svc, marking
the mandatory ones `close_gating`; (2) create runs whose `period_id` equals the close period's `PeriodName`;
(3) certify them; (4) with the mode `enforce` in a lower environment call the period readiness endpoint and confirm
`financial_controls` issues clear; (5) only then enable in production. Behaviour when enforced: service unreachable,
non-200 or bad body = 503 (close blocked); entity with no mandatory controls = blocked; any uncertified mandatory
control = 422 with the count. Covered by client tests (open, blocked, unconfigured, 500, bad JSON, refused,
timeout) and handler tests (off never calls; enforce open/blocked/unavailable/unconfigured; readiness matches lock).

## 5. Event envelope

The canonical envelope is the snake_case wrapper shared by the whole platform and parsed by audit-event-store-svc.
financial-control-svc uses it unchanged and adds ZS-EVENT-001 attributes as extra fields (see `financial-control-wave8.md`).
A regression test decodes the published message with a mirror of the audit-store's envelope. **Blocked platform-wide
decisions:** (a) adopting ZS-EVENT-001's CloudEvents naming (`specversion`, `com.zoikosuite.*` types) across all
producers and consumers together; (b) the values of `residency_region` and `classification` (governed by the
data-classification and residency standards); (c) registering `dataschema` URNs in schema-registry-svc;
(d) `causation_id`/`requestid` propagation.

## 6. Environment verification

Docker was available in this pass. The financial-control-svc image builds. A smoke test on an isolated network with a
throwaway Postgres 16 (migrations 000001-000005 applied): the service started, `/healthz`, `/readyz` and `/metrics`
returned 200, a request without identity returned 401, a request with identity while authorization-svc was
unreachable failed closed with 503, and a malformed entity id returned 400. **Not exercised:** Kafka, a real
authorization-svc, or any source service (the full compose stack was not started, and `docker compose config` fails on a
pre-existing undefined service unrelated to this work); the Docker images of the other modified services were not built.

## 7. Blocker register

The 25 blocked controls, by dependency. "Other document" = the capability belongs to a different architecture
document; "Decision" = a design or business decision is required first.

| Dependency | Controls | Missing capability | Other document | Decision needed | Deferrable |
|---|---|---|---|---|---|
| Payroll to GL / payments | 005, 006 | payroll posting path with account mapping and employer taxes; payroll-to-payment path and link key | ZS-SVC workforce/payroll, banking | account mapping, payment linkage | yes |
| Tax ledger | 007, 008, 009 | a tax ledger and tax payment/refund records | ZS-SVC-F (tax, e-invoicing) | ledger design | yes |
| Fixed assets | 010, 011 | currency and business date on depreciation; capitalisation link; dated per-asset population | ZS-SVC assets/inventory | date/currency source | yes |
| Inventory valuation | 012 | inventory value posted to the GL; currency and date on valuation | ZS-SVC assets/inventory | valuation currency policy | yes |
| Provider settlement | 004 | provider settlement population linked to payment instructions | ZS-SVC banking/treasury | linkage design | yes |
| FX | 025, 026 | revaluation posting with source event id and journal fields; treasury rates read endpoint with source and scope | ZS-SVC accounting/treasury | rate source and scope | yes |
| Unapplied cash | 029, 030 | AR and AP unapplied-cash / payment-application records | ZS-SVC-C, ZS-SVC-D | data model | yes |
| Consolidation and reporting | 016, 018, 040, 043 | elimination-to-entry links; statement mapping and output; related-party / counterparty-entity dimension; certified per-account TB | ZS-SVC reporting/consolidation | new consolidation architecture | yes |
| Imports and events | 033, 034 | generic import registry; consumer-side inbox so consumed can be compared with published | ZS-SVC-T, ZS-EVENT-001 | inbox convention | yes |
| E-invoicing | 035, 036 | an e-invoice service and acknowledgement store | ZS-SVC-F | new service | yes |
| Period close and suspense | 024, 031 | GL knowledge of when a period closed; suspense designation and item-level clearing | ZS-ACC-KERNEL-001 / financial-close | suspense rules (absent from the kernel spec) | yes |
| Migration identity, bank master | 039, 044 | shared identity for migrated open items; bank-account master mapping account, entity and cash GL account | ZS-MIG-001, ZS-SVC-E | identity and ownership rules | yes |

Partial controls and what they omit are stated in `GET /controls/v1/catalogue/status`.
