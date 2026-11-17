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
| §12.1 / §21 AsyncAPI | `services/eventing/asyncapi` | **Generated** from the catalogue: 21 AsyncAPI 3.1.0 documents, drift-tested. Per-service `asyncapi.yaml` files (12) are separate and not yet reconciled; event `data` schemas still to be registered. |
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

## Decisions taken (owner answers) and what followed

1. **Catalogue gaps resolved by adding, not renaming.** The 32 types the Wave 3 audit
   found are in `catalogue.json` with `origin: "extension"` (producer = the emitting
   service, consumers "not yet declared" until registered per §23). The uncatalogued
   baseline is empty, so any new unlisted type fails CI. Spec entries are 196; total 231
   with the three period events below.
2. **accounting-period-svc migrated to `eventing`.** It is the real "Period Service" of
   §19. Events now go to `eventing_outbox` (migration 000005, `outbox.SchemaSQL` + the
   relay-aware RLS policy) through the shared envelope and relay (RequireAll acks). Its
   PascalCase names (`PeriodHardClosed`) stay on the wire as the legacy `event_type`
   (expand/contract); canonical types are `com.zoikosuite.accounting.period.{opened,
   soft-closed, hard-closed, reopened, reclosed, command-rejected}`. The legacy
   `accounting_period_outbox` is no longer written; the legacy relay keeps draining
   what it holds (tested). Needs `EVENT_RESIDENCY_REGION`. Dockerfile and compose now
   build from `services/`.
   - Not preserved: the legacy envelope's `event_version` ("1.0") member, and event ids
     are UUIDv7 instead of `evt-<uuid>`. Neither is read by any consumer found in the repo,
     but verify before production.
   - `asyncapi.yaml` of the service still documents the legacy names; regenerate it from
     the catalogue (AsyncAPI wave).
3. **Wave 2 platform controls added to `eventing`:** `policy.ProducerACL` (E-02, EV-T23),
   `policy.Residency` + `Inbox.SetDeliveryPolicy` (EV-T10, with audit evidence),
   `Inbox.SetObserver` (§17 consumer signals).

## Finding: two producers emit the same period facts (E-02 violation)

`financial-close-svc` builds `com.zoikosuite.accounting.period.<fact>` at runtime and
emits `soft-closed` and `reopened`, the same facts accounting-period-svc now emits under
the same canonical names. By E-02 / EV-T23 exactly one service may own each. Until the
owners decide which, `policy.ProducerACL` cannot be enabled for the `accounting.period.*`
family (it refuses overlapping grants by design). Suggested split: accounting-period-svc
owns the lifecycle facts (`opened`, `soft-closed`, `hard-closed`, `reopened`, `reclosed`,
`command-rejected`); financial-close-svc keeps close-evidence facts (`closed`,
`close-review-started`, `reopen-requested`, `reopen-rejected`) and stops emitting the
lifecycle ones. That is a Finance-owner decision.

## Still open

- Wire `ProducerACL` into producers (blocked on the finding above for the period family).
- Producer-side replay-run tooling (consumer side done).
- PII/secret gate: `envelope.New` now refuses secret material in `data` (EV-T09). A broader PII scan (names, emails, tax ids in free text) is not attempted: it needs the payload schemas and data classification.
- Payload schemas: published for the six period events and the eight `general-ledger-svc` journal facts (`contracts/events/`, each verified against real emissions on Postgres, including the kernel's richer `journal.posted`). Journal `created`, `validated`, `rejected` and `withdrawn` were emitted but uncatalogued (the audit cannot see types built at runtime); they are now catalogue extensions. Original note on the period events (`accounting-period-svc/contracts/events/*.schema.json`, derived from the real producer and verified by a test that validates every emitted event against them). The other ~217 catalogue types have none yet; derive each from its producer the same way. Registering the files in schema-registry-svc is a deploy step not done here (they pass the registry's own shape validation).
- AsyncAPI: documents are now generated from the catalogue (21 domains, drift-tested). Still open: registering per-event payload schemas so `data` stops being an open object, golden payloads, PII/secret scan (§21 gates).
- Waves 4-7 of §25 (operating finance, compliance, data/integrations, hardening).

## Blind spot of the emitted-type audit

`ScanServices` only sees complete string literals; most producers build the type from a
prefix plus a fact, so their types are invisible to the ratchet. Wave work on
general-ledger-svc found four emitted-but-uncatalogued journal facts that way. Each
producer migrated to a schema-verified family (period, journal) closes its own blind
spot; the other dynamic families (`kernel.*`, `account-mapping.*`, consolidation,
intercompany, treasury, banking reconciliation, statement sets) still need the same pass.

## general-ledger-svc kernel events: schemas from captured payloads

44 `accounting.kernel.*` facts are now catalogued (extensions) and have schemas in
`general-ledger-svc/contracts/events/`. Rather than hand-derive 44 shapes, the whole GL
test suite was run once against a Postgres with a capture trigger (about 5,700 emitted
events) and `eventing/schemainfer` inferred an observed contract per type. Checks:
- the 365 captured journal events all satisfy the hand-written journal schemas (0 failures);
- all 51 schemas pass schema-registry-svc's own shape validation;
- every kernel integration test now validates the events it emitted against these schemas
  (a deliberately broken schema made it fail, so the check can fail);
- a test requires a schema file for every literal `OutEvent` fact in the source. It found
  `allocation-rule.retired`, which no test emits; its schema was derived from the code.

Limits: four types had a single sample, so they state no required fields; `required` for
the others reflects what the suite exercises, not necessarily what a rarely-hit code path
omits. No `account-mapping.*` events were captured (no suite path emitted them): they remain
without schemas. Fixed along the way: the AsyncAPI generator collided messages for
sub-aggregate types (`kernel.migration-batch.approved` vs `kernel.approved`).

## Capture across 12 more services (196 schemas, 165 more catalogue extensions)

The capture method (trigger on `eventing_outbox`, run the service's integration suite with
`-tags=integration -count=1`, infer with `eventing/schemainfer`) was applied to every
service that uses `eventing`. Lessons: many suites hide their event tests behind
`-tags=integration`; Go's test cache returns `ok (cached)` without running anything; a few
suites `DROP SCHEMA public CASCADE`, so the capture objects live in their own schema; one
database per service avoids cross-talk.

Result: `contracts/events/` schemas in banking-connector, connectivity-api-bridge (103),
consolidation, financial-close, intercompany-accounting, payee-banking-identity,
payment-initiation-adapter, payment-status, reconciliation-intelligence,
tax-authority-interface, treasury, bank-reconciliation. 254 schema files in total, all
passing schema-registry-svc's validation. 165 more emitted types are catalogued as
extensions (catalogue 444 = 196 spec + 248 extensions). Not captured (no events written
to the database by their tests): data-ingestion, financial-statements, identity-context,
migration-integrity, notification.

New guard tests: no schema without a catalogue entry; one service per schema.

### Findings that need an owner decision

1. **Two services emit the same period facts with incompatible payloads.**
   `accounting.period.reopened` and `soft-closed`: accounting-period-svc sends about 30
   fields keyed by `period_key`; financial-close-svc sends 7 fields keyed by
   `fiscal_period_id`. A consumer of `type` cannot know which shape it gets. Frozen in
   `knownSchemaConflicts`; resolve by choosing the owner (see earlier finding).
2. **Two domains the spec does not define.** connectivity-api-bridge emits ~110 types under
   `integration.*` and `connectivity.*` (section 19 puts connector facts under `data.*`),
   treasury under `treasury.*`, others under `reconciliation.*` and `einvoice.*`. They are
   catalogued as extensions via `ExtensionDomains`; decide whether to adopt the domains into
   section 19 or migrate the producers.
3. **19 emitted names break the past-tense rule** (`batch.duplicate`, `outbound.pending`,
   `security.tenant-scope-mismatch`, ...). Recorded in `extensionNamingDebt`; shrink-only.
4. **financial-statements-svc store writes a column no migration creates**
   (`statement_facts.source_accounts`); its integration tests fail on a fresh database.
   Pre-existing and unrelated to this work, found because the capture ran those tests.

### Enforced in each producer's own suite

Every one of the 12 adopter services now validates what its tests emit against its published
`contracts/events` schemas, as general-ledger-svc already did. The shared check is
`eventing/contracttest` (`CheckOutbox`, `Verify` per test, `FinishSuite` per suite). Services
that drop the outbox between tests call it at cleanup (consolidation, financial-close,
intercompany-accounting, reconciliation-intelligence, payee-banking-identity, banking-connector,
connectivity-api-bridge, tax-authority-interface); services that share one pool call it after
the suite and fail if the suite emitted nothing (payment-status, payment-initiation-adapter,
treasury, bank-reconciliation). A deliberately broken schema failed the consolidation suite.
Found along the way: general-ledger-svc `insertAccountVersion` had 19 placeholders for 18
columns (every account create failed); fixed.

### Producer ownership (E-02, EV-T23) checked in the same place

`catalogue.ServiceName()` turns a catalogue producer written "<Name> Service" (plus Accounting
Kernel and Payment Initiation Adapter) into a deployed service; roles (AR, Legal, ...) and
capabilities several services emit (Period Service, Party Service, ...) stay unowned.
`policy.CatalogueACL()` is the lenient ACL that results, and `contracttest.CheckOutbox` now also
fails any event whose publisher (taken from `source`, `urn:zoikosuite:service:<service>`) is not
the type's owner, or that names no publisher. All 12 adopter suites pass with it. Still not
enforced at runtime in the producers: this is a test-time gate; wiring the ACL into each
service's `main` is open, and blocked for `accounting.period.*` until the owner decision.

### account-mapping.* now catalogued and schema'd

The four `accounting.account-mapping.*` facts general-ledger-svc emits through `enqueueMappingEvent`
(`created`, `activated`, `superseded`, `ambiguity-detected`) were invisible to the type audit
(the type is assembled at runtime) and had no capture. They are now catalogue extensions
(catalogue 448 = 196 spec + 252 extensions) with schemas in `general-ledger-svc/contracts/events/`,
validated against real emissions by every GL store test at cleanup. `ambiguity-detected` is not
emitted by any current test, so its schema is derived from the code only.

### The five "no samples" services, resolved

- **financial-statements-svc**: the earlier "writes a column no migration creates" finding was wrong.
  Migration 000003 adds `source_accounts`; the test harness only applied 000001 and 000002 (now all
  four). Ten `reporting.statement-set.*` facts are catalogued (extensions) with schemas in
  `contracts/events/`, validated per test.
- **migration-integrity-svc**: four `migration.*` facts (`job.integrity-validated`,
  `job.integrity-violations-detected`, `job.archived`, `audit-entry.remediated`) catalogued with
  schemas, validated after the whole suite (which fails if it emits nothing). `migration` is a new
  extension domain, one more open question next to `integration`, `connectivity`, etc.
- **data-ingestion-svc** emits no events.
- **identity-context-svc** and **notification-svc** write to their own outboxes, not `eventing`.
  Giving them canonical envelopes and schemas is a migration (as accounting-period-svc was), not a
  schema task; still open.

Catalogue is now 462 entries = 196 spec + 266 extensions.

### identity-context-svc migrated to the eventing outbox

Decision taken: events with no tenant (a resolution that failed before a tenant was known) are
scoped to a reserved `platform` tenant, so every event fits the section 4 envelope and none is lost.

What changed (all behind the existing seams, so the ~45 call sites are untouched):
- `internal/outbox/eventing.go` converts the rendered legacy record into the canonical eventing
  envelope. `Store.EnqueueTx`/`Enqueue` and `PgStore.outboxEnqueueTx` write to `eventing_outbox`
  (migration 000020) in the same transaction as before. The envelope still emits the legacy
  `event_type`, `payload`, `source_service`, `tenant_id` ..., so authorization-svc, the gateway and
  the SIEM need no change. Dropped, as for the period events: the legacy `event_version` and the
  `evt-` id prefix (ids are now UUIDv7). A duplicate enqueue by a retrying caller is no longer
  absorbed by `ON CONFLICT (event_id)`.
- Canonical names: `identity.mfa_factor.enrolled` becomes `com.zoikosuite.identity.mfa-factor.enrolled`
  (underscores to hyphens; `session.invalidated` and `principal.status.changed` are mapped by hand).
  `identity` is a new extension domain (ZS-IAM-001 section 23 names it; section 19 of ZS-EVENT-001 does not).
- `main` requires `EVENT_RESIDENCY_REGION` (compose and the Kubernetes manifest set it), verifies the
  table, and runs eventing's relay beside the legacy relay, which only drains `event_outbox` now.
- The secret scan of `envelope.New` now runs on identity payloads. A payload holding a credential
  makes the enqueue (and so the business write) fail closed; the suite shows no current payload trips it.
- 30 types captured from the store suite (327 events) are catalogued (extensions, catalogue 492 =
  196 spec + 296 extensions) with schemas in `contracts/events/`, validated by every store test.
  All 30 names are past tense, so the lint needed no exceptions.

Not done:
- 40 names in the source were not captured (their tests use fake publishers, no database):
  `authentication.failed/throttled`, `login_session.*`, `password*`, `refresh_token.reuse_detected`,
  `step_up.*`, `subject.created/enabled/invited`, `idp.*`, `group*`, `scim_*`, `retention.*`,
  `context.cache_invalidated/tenant_invalidated`, `session.revoked/invalidated`,
  `support_context.revoked`, `token.exchanged`, ... (some, like `session.risk.changed`, are consumed,
  not emitted). They have neither catalogue entries nor schemas, and the emitted-type audit cannot
  see them. Capture them with a recording sink in their package tests.
- `PurgePublishedOutbox` only prunes the legacy `event_outbox`; published rows in `eventing_outbox`
  have no retention yet.
- notification-svc is still on its own outbox.

### counterparty-management-svc (BIZ-06 CRM) migrated to the eventing outbox

Five events, written to `eventing_outbox` (migration 000004) in the same transaction as the change
by the store, and no longer published directly by the handler:
`com.zoikosuite.business.relationship.created`, `relationship.closed`, `interaction.logged`,
`opportunity.created`, `opportunity.stage-changed` (legacy names `relationship.created`,
`opportunity.stage_changed`, ... are still emitted beside them). They are `extension` entries under
the existing `business` domain, producer `Counterparty Management Service`, with payload schemas in
`counterparty-management-svc/contracts/events` that the store tests enforce on every outbox row.
Free-text interaction notes are never put in an event.

Behaviour changes to know about:
- `EVENT_RESIDENCY_REGION` is now required (compose and phase5 set it, default `local`); the service
  refuses to start without it, and a write whose event cannot be built is rolled back.
- Delivery is now at-least-once through the relay; the old direct publish dropped the event with a
  warning when Kafka failed.
- The Docker build context moved to `services/` (Dockerfile, `Dockerfile.dockerignore`, both compose files).
- Not migrated: the existing counterparty events (`counterparty.*`) still use the old direct publisher,
  and `CloseOpportunity` still emits no event (it changes the stage to CLOSED_WON/LOST but never
  published `opportunity.stage_changed`; left as it was).

### document-vault-svc (BIZ-01 / BIZ-02): nine events moved to the eventing outbox

Nine events are written to `eventing_outbox` (migration 000017) in the same transaction as the change, as
`com.zoikosuite.business.document.{record-declared,superseded,archived,disposition-requested,version-upload-quarantined}`
and `com.zoikosuite.business.classification.{proposed,confirmed,reclassified,superseded}`, with payload schemas in
`document-vault-svc/contracts/events` enforced by the store tests and extension catalogue entries (producer
`Document Vault Service`). The old event_type is kept as the envelope's legacy type.

Deliberately not moved: `document_created` and `document_version_committed` (named by ZS-SVC-S-001 section 9.3, so renaming
them needs a decision between two specs), and the DRC retention, legal-hold and preservation events.

Behaviour changes: `EVENT_RESIDENCY_REGION` is now required (compose sets it, default `local`), a write whose event
cannot be built is rolled back, a second relay delivers the eventing outbox (not started when no Kafka brokers are
configured), and the Docker build context moved to `services/`.

Also fixed: the older relay failed every poll on a row with a NULL `legal_entity_id` (legal holds, export packages),
which blocked every later event; it now scans a nullable value.

Open: the catalogue's section 19 entries `business.document.stored`, `versioned` and `classification-changed` (producer
"Document Service", no deployed owner) overlap in meaning with the new types and need reconciling.

### notification-svc (BIZ-03): four template events moved to the eventing outbox

`template.created`, `template.version_approved`, `template.published` and `template.retired` are written to
`eventing_outbox` (migration 000035) in the same transaction as the change, as
`com.zoikosuite.business.template.{created,version-approved,published,retired}`, with payload schemas in
`notification-svc/contracts/events` enforced by the store tests. The old event_type is kept as the envelope's legacy type.
Catalogue: `created`, `version-approved` and `retired` are new extension entries (producer `Notification Service`);
`published` was already a section 19 entry whose producer "Template Service" had no owner, so the producer mapping
now resolves "Template Service" to `notification-svc` (the service that hosts BIZ-03).

Not moved: `notification.*`, `communication.*` and `delivery.attempt.*` stay on the service's own `event_outbox`.

Behaviour changes: `EVENT_RESIDENCY_REGION` is now required (compose sets it, default `local`), a template write whose event
cannot be built is rolled back, and a second relay delivers the eventing outbox (not started when no Kafka brokers are
configured). The four event builders in `internal/events` were removed.

Open: `eventing_outbox` has no row-level security (the shared library's table is cross-tenant by design, its relay drains
every tenant), so the app role can read every tenant's envelopes; the service's legacy-RLS test excludes it explicitly.

### exception-escalation-svc (BIZ-08): five deadline events moved to the eventing outbox

`deadline.created`, `deadline.superseded`, `deadline.completed`, `deadline.due_soon` and `deadline.overdue` are written to
`eventing_outbox` (migration 000007) in the same transaction as the change, as
`com.zoikosuite.business.deadline.{created,superseded,completed,became-due-soon,became-overdue}`, with payload schemas in
`exception-escalation-svc/contracts/events` enforced by the store tests and extension catalogue entries (producer
`Exception Escalation Service`). The old event_type is kept as the envelope's legacy type. The two threshold events are named
with `became-` because the catalogue lint requires past-tense facts.

They were published by the handler after the commit with failures only logged; for due-soon and overdue the notified stamp was
committed first, so a failed publish was never retried. Not moved: the exception-case, finding and task events.

Behaviour changes: `EVENT_RESIDENCY_REGION` is now required (phase5 compose sets it, default `local`), a write whose event
cannot be built is rolled back, a relay delivers the eventing outbox (not started with no brokers), and the Docker build
context moved to `services/`.

Open: the catalogue's section 19 entry `business.deadline.due` (producer "Deadline Service", no deployed owner) overlaps
`became-due-soon` and `became-overdue` and needs reconciling.
