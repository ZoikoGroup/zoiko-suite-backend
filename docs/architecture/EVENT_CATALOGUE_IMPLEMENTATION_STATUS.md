# ZS-EVENT-001 — Event Catalogue & Contract Standard: implementation status

Spec: `docs/architecture/ZoikoSuite_Enterprise_Domain_Event_Catalogue_Event_Contract_Standard_Detailed_Engineering_Wireframe.docx`
(v1.0, 24 Aug 2026). Rule for this work: **no new service**. Everything lands in
`services/eventing` (shared library), `services/schema-registry-svc`, and the
existing producer/consumer services.

Status: first-pass gap map from reading the spec against the repo. Nothing has been
changed yet. Items marked "verify" were inferred from file names/greps, not tests.

## Where each part of the spec lives

| Spec part | Home | State |
|---|---|---|
| §4 Canonical envelope (CloudEvents-aligned) | `services/eventing/envelope` | Implemented: UUIDv7 id, payloadhash, tenant/legal-entity/book, aggregate id+version, correlation/causation/request, residency, classification, schemaversion, traceparent, partition key. Emits legacy field names too (expand/contract). |
| §5.3 Opaque partition key | `envelope.PartitionKey` | Implemented (hash of tenant+aggregate). |
| §6 Transactional outbox, dispatcher | `services/eventing/outbox`, `kafkaout` | Implemented (SKIP LOCKED, attempt limit, quarantine, RequireAll acks, backlog stats, Requeue keeping event id). |
| §8 Consumer inbox / dedupe | `services/eventing/inbox` | Implemented (states, attempts, poison to quarantine, region). Replay namespace added in Wave 1. |
| §10 Replay / repair | `eventing/inbox`, `eventing/repair` | **Wave 1 done (consumer side):** replay rejected by default, `AllowReplay()` opt-in, isolated per-run namespace (EV-T11). Producer-side replay-run tooling still open. |
| §11 Quarantine | `eventing/inbox` (`CaseSQL`) | **Wave 1 done:** case record with owner, priority, SLA, failure class, disposition, evidence; `UnownedQuarantined` signal. |
| §12 Schema registry & compatibility | `services/schema-registry-svc` | **Wave 2 done:** enum, bounds, pattern, format and additionalProperties now compared (enum growth refused unless NONE); sticky producer ownership (E-02, EV-T23); kebab-case canonical names registrable. `$ref`/`allOf`/`anyOf` still not compared. |
| §12.1 / §21 AsyncAPI | `asyncapi.yaml` | Only 12 of ~127 services have one. |
| §17 Observability | `outbox.ReadStats`, `busops` (lag, partition health) | Partial: producer + lag signals exist; consumer dedupe rate, unknown-type, replay, residency-denied counters not confirmed. |
| §14 Residency / ACLs | `integration/gate*`, inbox `SetRegion` | Partial (verify): consumer region check exists; producer-ownership ACL (EV-T23) not found. |
| §15 External provider effects | `eventing/integration`, payment/tax adapter services | Partial (verify): attempt-record pattern appears in adapters; not audited against EV-T12/T13. |
| §19 Catalogue (196 event types) | `services/eventing/catalogue` | **Wave 0 done:** catalogue.json (16 families), loader, `Lint` (§21 semantic lint), tests. Ownership enforcement at publish time is still open (Wave 2). |
| §21 CI gates (semantic lint, golden payloads, PII scan) | `services/_contract`, `_certification` | Not mapped yet. |

## Biggest gaps (in priority order)

1. **Adoption.** Only ~18 services depend on `zoiko.io/eventing`. About 51 services still
   carry a hand-written `internal/outbox`, and ~97 have their own `internal/events/publisher.go`
   (the `eventing` README says these drifted: id only in header, `occurred_at` vs
   `emitted_at`, no SKIP LOCKED/attempt cap).
2. **Naming.** Spec type is `com.zoikosuite.<domain>.<aggregate>.<past-tense-fact>`.
   Services emit things like `kill_switch.engaged`, `workflow.started`, `invoice`,
   `ap.invoice.create`, `ar.cash.applied`. Only `com.zoikosuite.accounting.journal.posted`
   is used among the four flagship examples; `ar.customer-invoice.issued`,
   `tax.tax-return.accepted`, `banking.payment.settled` have **0** hits.
3. **No catalogue / ownership registry.** Nothing records "event type → owning producer →
   schema → classification → consumers", so E-02, EV-T23 and the §21 semantic lint
   cannot be enforced.
4. **Replay safety** (E-17, EV-T11): no replay namespace in the inbox, no irreversible-consumer flag.
5. **Quarantine record** with owner/SLA/disposition (§11.1).
6. **Schema compatibility depth** (enum, bounds, additionalProperties).
7. **AsyncAPI** per event family.

## Proposed plan (existing services only)

- **Wave 0 – catalogue as data.** Add the §19 inventory as a versioned catalogue file under
  `services/eventing` (or registered through `schema-registry-svc`), with owner, aggregate,
  classification, retention class, consumers. Add a semantic-lint test (name pattern,
  past tense, owner present) that runs in CI.
- **Wave 1 – reliability gaps in `eventing`.** `replay_run_id` + irreversible-consumer
  rejection in `inbox`; quarantine record (§11.1); consumer-side metrics.
- **Wave 2 – registry.** Extend `compat` to enum/bounds/additionalProperties; producer
  ownership check at registration.
- **Wave 3+ – per spec §25.** Migrate producers to `eventing` and canonical names,
  accounting core first (journal, period, reconciliation), then AR/AP/banking, then
  tax/legal/audit. Each migration keeps legacy names (expand/contract).

## Decisions needed from the owner

- Catalogue home: static file in `eventing` vs rows in `schema-registry-svc`.
- Rename policy for existing event types: dual-emit old+new during migration, or a
  hard cut per service.
- Which wave to start with (suggest Wave 0 + Wave 1: no producer behaviour change).

## Wave 3 — accounting core: what the audit found

`services/eventing/catalogue` now scans every service for emitted event types
(`ScanServices`, `ScanPrefixes`) and reconciles them with the §19 catalogue.
`TestNoNewUncataloguedEventTypes` is a ratchet: a new emitted type that is not in
`catalogue.json` fails CI; `testdata/uncatalogued_baseline.txt` holds the 32 known
ones and may only shrink (`TestBaselineHasNoStaleEntries`).

Limits of the scan: it sees complete string literals (33) and runtime prefixes (18
`com.zoikosuite.<domain>.<aggregate>.` literals). Types assembled as prefix + verb
(most of the accounting services) show only as families, so the real emitted count is
higher than 33. 195 of 196 catalogued types are not seen in any literal; that is the
upper bound of the backlog, not a measured number.

Accounting-core services (general-ledger, financial-close, consolidation,
intercompany-accounting, financial-statements, reconciliation-intelligence) already
use `eventing` and canonical `com.zoikosuite.*` names. The gap is vocabulary, not
mechanism. Decisions needed from the domain owners (spec §20: Finance/Tax/Legal sign
off regulated semantics) before any producer is renamed:

| Emitted today | Catalogue (§19) | Note |
|---|---|---|
| `accounting.period.closed` / `reclosed` (financial-close-svc) | `accounting.period.hard-closed` | Same fact? "closed" is a close-evidence event, not obviously the hard close. Consumers read the canonical `type` first (inbox `Parse`), so a rename is consumer-visible. |
| `accounting.period.soft-closed` | `accounting.period.soft-closed` | Matches. |
| `accounting.journal.created` / `rejected` (general-ledger-svc) | `journal.proposed` / `approved` / `posted` | `created` and `rejected` are not catalogued. |
| `accounting.kernel.*` (account.scope-changed, event.rejected, reconciliation.break-raised) | `accounting-event.rejected`, `reconciliation-break.detected` | Different aggregates/verbs. |
| `accounting.consolidation-run.*`, `consolidation-adjustment.*`, `intercompany-entry.*` | `consolidation-run.completed`, `intercompany-mismatch.detected` | Run/adjustment/entry lifecycle events are richer than §19. |
| `banking.payment-execution-state.*` (settled, rejected, returned, ...) | `banking.payment.settled` / `rejected` / `returned` | State-named aggregate; the catalogue names the payment. |
| `banking.payee-destination.*`, `bank-connection.*`, `provider-callback.quarantined` | not catalogued | Banking-identity facts missing from §19. |
| accounting-period-svc | `accounting.period.*` | Does not use `eventing` yet (no outbox). |

Recommended resolution per row: either add the type to `catalogue.json` with owner and
consumers (when the fact is real and distinct), or rename with dual-emit through
`LegacyType` (when it is the same fact under a different name). Each removal from the
baseline is then a visible, reviewable step.
