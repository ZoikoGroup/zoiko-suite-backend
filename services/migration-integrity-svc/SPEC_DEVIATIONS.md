# Spec deviations — migration-integrity-svc (ZS-MIG-001)

Governing document: ZS-MIG-001 (Global Migration, Opening Balance & Cutover
Standard). The service-ownership table gives this service "migration
certification evidence" under ZS-MIG-001 · ACC-17 · Doc06 Phase 6, with
general-ledger as its dependency.

**What the service actually is.** A stateless record checker with an audit
trail: `POST /v1/migrations/validate` takes a list of records (`ref` + string
fields) and a list of required field names, runs five checks (required
fields present, source identity presence, duplicate `ref`, numeric format of
four hard-coded field names, and — added 2026-11-02 — currency-code validity
of a fifth hard-coded field name), stores the job, its check results and one
audit entry per violation, and emits events. `POST
/v1/migrations/{id}/audit/{entryId}/remediate` flips `is_remediated`;
`DELETE /v1/migrations/{id}` archives. It never writes to a target system,
which is consistent with MIG-INV-06.

**Where ZS-MIG-001 work lives today.** The opening-balance flow is in
**financial-close-svc** (ACC-17), not here: `/v1/migration-batches` with
create, validate, approve, commit (posts the opening journal), certify and an
exceptions list; batches carry a source extract hash, expected row count and
debit/credit totals, a crosswalk of entries, quarantine on failure, and a
`migration-batch-tieout` control population consumed by financial-control-svc
(FIN-CTRL-038). migration-integrity-svc is not connected to any of it.

Overall: of the 13 commands/queries in ZS-MIG-001 §23 and its §4 object model,
this service implements none in the spec's terms; its three checks cover a
small part of §16 (data quality) and MIG-C04/C05-style accountability only in
spirit. Roughly 10% of what the standard assigns to a migration-certification
service. One scoring defect was fixed in this pass (B1).

## A. Not implemented / weaker than the spec

- **A1. None of the §23 command surface exists.** Not built: `POST
  /migration-programs`, `POST /source-extracts:register`, `POST
  /migration-batches:validate|execute|cancel`, `POST
  /quarantine-items/{id}:resolve`, `POST /reconciliation-runs`, `POST
  /migration-certificates:issue`, `POST /cutover-runs`, `POST
  /cutover-runs/{id}:activate-target`, `GET /migration-lineage/{canonical_id}`,
  `GET /migration-crosswalk`. Of these, batch validate/execute and a
  crosswalk partly exist in financial-close-svc (see above). Before building
  any of them a decision is needed on the split: either migration-integrity
  becomes the migration control plane (programs, extracts, batches,
  quarantine, reconciliation, certificates, cutover) and financial-close
  keeps only the opening-balance *posting* as a domain loader, or this
  service is narrowed to certificates/evidence over financial-close's
  batches. The ownership table suggests the second; the code supports
  neither yet.
- **A2. None of the §4 object model exists** (partly closed -- see D).
  MigrationProgram, SourceSystem, SourceDataset, SourceExtract (watermark,
  manifest hash, record count, amount controls), MappingRuleSet,
  IdentityCrosswalk, MigrationCandidate, QuarantineItem, MigrationRun,
  ReconciliationRun, MigrationCertificate, CutoverRun, StabilizationCase —
  none are modelled. A "job" has no extract identity, no source watermark,
  no mapping version and no batch identity (MIG-INV-02, -03, -05, -11; §17
  certificate minimum). MigrationProgram, SourceExtract and IdentityCrosswalk
  are now built (D1/D2, C2); MigrationCertificate and MigrationBatchRef
  (C1/C3) cover part of MigrationRun/MigrationCertificate's ground.
  SourceSystem, SourceDataset, MappingRuleSet, MigrationCandidate,
  QuarantineItem, ReconciliationRun, CutoverRun and StabilizationCase
  remain unmodelled -- the cutover-runbook pieces (CutoverRun,
  StabilizationCase, the `G0`-`G10` gates) deliberately so (D6).
- **A3. No migration certificate.** The ownership table's one named
  responsibility. §17 requires the certificate to name source watermarks,
  extract hashes, mapping versions, accepted batches, target release/schema,
  reconciliation runs, unresolved approved exceptions, runbook version and
  named signatories, and §5 requires that whoever prepared the mapping cannot
  be the sole approver. Nothing here issues, signs or stores a certificate;
  financial-close-svc's batch `certify` records a certifier and reason only.
- **A4. No reconciliation of source to target.** The checks look at the
  submitted records only. Nothing proves loaded + quarantined + excluded =
  source population (MIG-C04, §17 completeness), compares financial totals
  (MIG-N28) or relationships (MIG-N29), or ties open AR/AP to control accounts
  (MIG-C16/C17, MIG-N07). Records carry no amount field the service
  understands except as a numeric-format check.
- **A5. Remediation has no evidence** (closed). `RemediateEntry` accepted
  `notes` and discarded them: they were not stored and not in the
  `migration.audit_entry_remediated` event. Remediation is now an evidenced
  decision: `remediation_reason` and `remediation_evidence_ref` are both
  required (400 when either is missing or blank) and are stored on the audit
  entry together with `remediated_by` and `remediated_at` (migration
  `000003_remediation_evidence`), and all four are included in the
  `migration.audit_entry_remediated` event payload. The store records them on
  the first remediation only: a replay still succeeds, emits nothing and
  returns the recorded decision unchanged, so the evidence cannot be
  rewritten after the fact. The discarded `notes` field was removed rather
  than left as a field the API pretends to accept (MIG-INV-12 "owner, reason
  code and disposition"; MIG-N27 "quarantine item manually edited without
  evidence → reject resolution").
- **A6. No quarantine severity or reason taxonomy** (closed; classification
  choices in B3). §16's MQ0–MQ4 replace INFO/WARNING/CRITICAL outright (no
  reader outside this service consumed the old values). Each audit entry now
  stores its own `severity`, a §16 `reason_family` and an `owner_id`;
  checks carry the MQ class of the violations they raise, or MQ4 when they
  find nothing. Reason family is stored beside `violation_type`, not derived
  from it: today each check happens to raise one family, but the family
  depends on the field (a type mismatch on a date would be TEMPORAL, on a
  currency REFERENCE), so it is assigned where the violation is raised. The
  validate request requires `exception_owner_id` (400 without it), and that
  owner is stamped on every entry (MIG-INV-12, MIG-C37). Dispositions as
  built: **MQ0** fails the job at any score and any bar, by extending the
  A11 acceptance rule rather than adding a separate block; the job stores
  `blocker_count`, which is also in the `integrity_validated` event; and only
  the entry's named owner may remediate it (403 for anyone else, even with
  `AUDIT_ENTRY_REMEDIATE`). **MQ2** is left to the acceptance threshold,
  which is the spec's "may proceed only if acceptance criteria permit".
  **MQ1/MQ3/MQ4** are labelled and owned, nothing more. Migration
  `000005_quarantine_taxonomy` backfills existing rows exactly from the
  check/violation type that produced them, enforces the vocabulary with
  CHECK constraints and reverses cleanly; it is tested at schema version 4
  with legacy rows, then up, down and up again.
  **Not built, deliberately:**
  (1) *"Stop affected batch/cutover"* beyond this service. Batches and
  cutover live in financial-close-svc (A1) and nothing there consumes this
  service's results, so a FAILED job with `blocker_count > 0` is as far as
  "stop" reaches; no cross-service halt was invented.
  (2) *MQ1 "formally approved, time-limited migration exception"* needs an
  exception object and a certificate for it to gate (A1–A3, MIG-N34);
  neither exists, so an unresolved MQ1 blocks nothing.
  (3) *Remediation does not re-accept a job.* Resolving every MQ0 records
  the decision but leaves the job FAILED: acceptance needs a fresh
  validation of corrected data, not a note overriding a failed check.
  (4) *Legacy rows.* Entries recorded before 000005 have no owner (none was
  ever captured, and inventing one would fabricate accountability), so their
  MQ0s fall back to the action check alone. Stored job statuses were not
  recomputed: a job accepted before this change keeps COMPLETED, with its
  `blocker_count` beside it.
  (5) *No materiality figure.* MQ1 versus MQ2 cannot be judged against an
  approved materiality amount (the service understands no amounts, A4/A7);
  B3 states the rule used instead.
- **A7. Fixed, unversioned checks.** Required fields come from each request;
  the numeric fields are hard-coded (`amount`, `salary`, `balance`,
  `quantity`); there is no schema-version check (MIG-C03, MIG-N03),
  date/timezone check (MIG-C13, MIG-N13), or range check (the `RANGE_CHECK`
  type and `OUT_OF_RANGE` violation are still declared but never produced).
  Rules are not versioned, so a stored result cannot say which rule set
  produced it (MIG-INV-05).
  - *Currency check — closed 2026-11-02 (MIG-C12, MIG-N12).* A fifth check,
    "Currency Code Validity", validates a record's `currency_code` field (a
    hard-coded field name, same convention as the numeric-fields check)
    against a static ISO 4217 alphabetic code list
    (`domain.validISO4217Codes`). An unrecognized code is quarantined, never
    silently substituted (MIG-N12): `ORPHANED_REFERENCE`/MQ1/REFERENCE —
    the `REFERENTIAL_INTEGRITY` check type and `ORPHANED_REFERENCE`
    violation this item named as "declared but never produced" are now
    produced by this check, not a new type. A blank or absent
    `currency_code` is left to the required-fields check, not this one.
    *Not closed:* the rest of MIG-C12 ("minor units/functional currency
    valid") — this service holds no minor-unit table per code and no
    functional-currency context, only the alphabetic code list itself — and
    the schema-version, date/timezone and general referential/range checks
    A7 also names, none of which this pass touches. *Tests:*
    `internal/domain/types_test.go`
    (`TestIntegrity_CurrencyCodeValidity`); the pre-existing
    `TestIntegrity_EntriesCarrySeverityFamilyAndOwner` fixture extended with
    an invalid-currency record so every check still fails in that fixture;
    `internal/handler/handler_test.go`'s check-count assertion updated from
    4 to 5.
- **A8. Empty `ref` is accepted** (closed). A blank `ref` is now its own
  violation, `MISSING_SOURCE_IDENTITY`, reported by a dedicated "Source
  Identity Presence" check before duplicate detection runs (MIG-INV-02 every
  object preserves source identity, MIG-INV-14). Blank refs are excluded from
  duplicate detection, so several unidentified records are each reported for
  what they are instead of as "duplicates" of one another; a blank ref also
  no longer perturbs duplicate detection for the refs that do have an
  identity. Each blank-ref record counts as invalid for the count and score,
  like any other violation.
- **A9. Reads are not authorized per legal entity** (closed). `GET
  /v1/migrations` and `GET /v1/migrations/{id}` checked only the tenant
  (RLS), so any principal in the tenant could read every legal entity's
  migration jobs; writes were already authorized per legal entity. Both
  reads now run the same per-entity check the writes do, against a new
  `MIGRATION_JOB_VIEW` action: by id the job is fetched and authorized at its
  own entity; the list is authorized at the requested entity and requires
  `legal_entity_id` (400 without it, rather than a silently filtered list
  that would read as "there is nothing there"). Reads fail closed: 401
  without a principal, 403 on denial, 503 when authorization-svc is
  unreachable. `MIGRATION_JOB_VIEW` is a new action name — until reader
  roles are granted it, reads fail closed with 403.
- **A10. Source values are stored and echoed** (closed). The exposure half
  of this was A9 — the read API echoed raw values to any principal in the
  tenant — so what remains is an explicit decision, not a build. The value
  is stored and echoed raw, deliberately: it is the single offending field
  value, the evidence remediation acts on (A5, MIG-INV-12), not a record
  dump; masking the one value a remediator must act on would leave them
  with nothing, and no permission granularity finer than the per-entity
  check exists here to hang a narrower read on. The boundary is therefore
  the per-legal-entity read authorization: a caller authorized for the job
  sees the evidence, everyone else gets 403, and the list read returns job
  summaries carrying no audit entries at all. §25 governs migration logs;
  this is an evidence row behind the same authorization as the job it
  belongs to. If a whole record is ever captured, mask reads the way
  treasury masks account numbers (`BANK_ACCOUNT_VIEW_MASKED`).
- **A11. Integrity score is a ratio, not an acceptance result** (closed).
  The score is now judged against an explicit, configurable acceptance bar
  (`INTEGRITY_SCORE_THRESHOLD`, service default 95): a job scoring below it
  is `FAILED`, not `COMPLETED` — including the one-valid-in-a-thousand case
  that used to pass — and a run with every record invalid is `FAILED` at
  any bar. The bar actually applied is stored on the job row
  (`integrity_score_threshold`, migration `000004`) and returned by the
  reads, so an acceptance decision stays auditable after the configured
  default changes. "Score is high" as an acceptance criterion (§33
  anti-pattern) is now an explicit, stored bar.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. Violations are counted per record, not per distinct ref** (fixed in
  this pass). The valid/invalid counts and the integrity score were computed
  from a set of *refs*, so four records sharing one ref counted as one
  invalid record: a batch in which every record was a duplicate scored 75%.
  Every record of a duplicated ref now counts as invalid (which copy is
  authoritative cannot be known here, consistent with MIG-N05), and the
  duplicate check's passed/failed counts are per record. Side effect on job
  status: a batch whose records are all duplicates was `COMPLETED` and is now
  `FAILED` (the existing "every record invalid" rule). The existing handler
  test's sample encoded the old count (its "valid" record was one of the two
  duplicates); one genuinely valid record was added and the test now asserts
  exact counts (4 invalid, 1 valid, score 20).
- **B2. Numeric format is a plain signed decimal** (fixed). The check
  accepted any string of digits, `.` and `-`, so `1.2.3`, `-`, `.` and `1-2`
  passed. It now requires `-?(digits[.digits] | .digits)`. Thousands
  separators, `+`, and exponents are still refused, as before. A blank value
  is still not a format violation — presence is the required-fields check's
  job.
- **B3. Quarantine classification rule** (A6). Fixed per check, because
  without amounts or a materiality figure the service cannot classify per
  record: missing source identity → MQ0 / IDENTITY (a record with no
  identity cannot be counted in "loaded + quarantined + excluded = source
  population", so it threatens financial completeness); duplicate ref → MQ1
  / IDENTITY (collision, possible duplicate economic effect, MIG-N05/N09/N10);
  unparseable `amount`/`salary`/`balance`/`quantity` → MQ1 / SCHEMA (a
  financial value that cannot load, of unmeasurable size, is not presumed
  immaterial); missing caller-declared required field → MQ2 / SCHEMA (field
  of unknown meaning; the threshold decides). Nothing produces MQ3, and MQ4
  marks a clean check. Two consequences a reviewer may want to reverse: a
  single blank `ref` now fails a run that would otherwise pass, and the
  owner-only rule for MQ0 means a delegate cannot resolve one on the
  owner's behalf.

## C. Wiring this service into the platform as a real control plane

Until this pass, `migration-integrity-svc` did exactly one thing --
`POST /validate`'s six field-level checks -- and nothing in the platform
called it, nor did it call anything else. It had no concept of a migration
program, batch, identity crosswalk or certificate, despite being governed by
the full `ZS-MIG-001` doctrine. This pass adds the three objects the spec's
§4 Canonical Migration Object Model and §3 Doctrine describe above the
field-level engine, and wires a real call path into
`general-ledger-svc`, which already has the one real, working posting/
cutover lifecycle (`internal/kernel/migration.go` there: Stage -> Preview ->
Approve -> Post -> Reconcile, posted as a real OPENING journal through the
ordinary chart-of-accounts/dimension rules).

- **C1. `MigrationBatchRef`** (`migration_batch_refs`, migration `000006`).
  A thin reference row tying one validation job (`migration_jobs.id`) to the
  real batch general-ledger-svc executes (`gl_batch_id`, `gl_journal_id`,
  `gl_journal_number`, all copied back from GL's own responses, never minted
  here). It is deliberately NOT a copy of general-ledger-svc's
  `MigrationBatch`: it holds no lines, no account mapping, no posting state
  machine -- those stay in general-ledger-svc, which owns financial
  integrity for the posting itself. This is the "program/tracking layer" the
  task asked for, not a second "posting layer".

- **C2. `IdentityCrosswalk`** (`migration_identity_crosswalk`, migration
  `000007`). A permanent `(tenant, source_system, source_type, source_id) ->
  canonical_id` mapping with match evidence (`match_method`,
  `match_evidence`, `matched_by`), kept forever rather than scoped to one
  job, per MIG-INV-14 ("legacy and canonical identifiers remain mutually
  discoverable"). A later extract that reuses a legacy key reuses its
  existing canonical_id; a conflicting canonical_id for an already-mapped
  source identifier is refused (`store.ErrCrosswalkConflict`), not silently
  overwritten -- a legacy key changing identity is a fact that needs its own
  evidenced decision.

- **C3. `MigrationCertificate`** (`migration_certificates`, migration
  `000008`). Issued by `POST /v1/migrations/{id}/certificate` only after (a)
  the job carries no unresolved MQ0/MQ1 audit entries and (b)
  general-ledger-svc's own `GET /v1/migration-batches/{id}/evidence` is read
  **fresh, at issuance time** and reports `Reconciliation.Status == MATCHED`
  with `evidence_verified == true`. The certificate's `gl_journal_id` /
  `gl_journal_number` / `gl_evidence_hash` are copied from that read, not
  computed or asserted independently -- the claim is backed by GL's
  evidence, never this service's own say-so. A `CHECK` constraint
  (`gl_reconciliation_status = 'MATCHED'`) backstops this at the schema
  level.

- **C4. The actual call**: `POST /v1/migrations/{id}/promote`
  (`internal/handler/control_plane.go`) drives general-ledger-svc's
  *existing, unmodified* kernel migration endpoints as an HTTP client
  (`internal/glclient`) in the order Stage -> Validate -> Approve -> Post ->
  Reconcile, carrying the same tenant/principal/legal-entity/correlation
  envelope headers and an `Idempotency-Key` derived from the batch ref's own
  id (so a retried promotion step replays general-ledger-svc's own
  idempotent response instead of creating a second batch or journal).
  Preconditions enforced before any GL call is made: the job has no
  unresolved MQ0/MQ1 blockers, and every line's `source_ref` already has a
  resolved crosswalk entry. The preparer (who stages in GL) and the
  promoting principal (who approves) are required to differ, matching
  general-ledger-svc's own self-approval rule (`ErrSelfApproval`) rather
  than discovering it as an opaque 4xx mid-sequence.

- **C5. Authorization actions added**: `MIGRATION_BATCH_PROMOTE`,
  `MIGRATION_BATCH_VIEW`, `MIGRATION_CROSSWALK_WRITE`,
  `MIGRATION_CROSSWALK_VIEW`, `MIGRATION_CERTIFICATE_ISSUE`,
  `MIGRATION_CERTIFICATE_VIEW` -- checked the same fail-closed way as the
  existing `MIGRATION_JOB_*` actions (`internal/authz.Client.CheckAllowed`
  against authorization-svc, scoped per legal entity except the crosswalk
  actions, which are tenant-scoped because a crosswalk entry is not
  attached to one legal entity).

- **C6. `financial-close-svc`'s separate `MigrationBatch` (capability
  `ACC-17`) is redundant with general-ledger-svc's and should be retired —
  recommended, not done here.** `financial-close-svc` has its own
  `domain.MigrationBatch` and its own migration-batch lifecycle, entirely
  independent of `general-ledger-svc/internal/kernel/migration.go`. Both
  claim to be the migration-batch posting authority for opening balances;
  only one (general-ledger-svc's) actually posts through the real
  chart-of-accounts/dimension/period-gate rules and freezes real reconciled
  evidence. This service was wired to general-ledger-svc's version
  specifically because it is the one with working Stage/Preview/Approve/
  Post/Reconcile semantics against the real ledger. `financial-close-svc`'s
  version was deliberately left untouched for this task (out of scope,
  explicitly) -- but its existence as a second, competing migration-batch
  concept is a real architectural redundancy, and retiring it (or
  re-pointing `ACC-17` at general-ledger-svc's batches) is recommended
  future cleanup, not something this pass should decide unilaterally.

- **C7. What is intentionally NOT done here.** This service's generic
  `POST /validate` (field-level record checks: schema, referential,
  identity, duplicate, range, format) and `POST /{id}/promote`'s
  GL-shaped batch content (account codes, debit/credit, cutover window) are
  two different inputs with no automatic mapping between them --
  `ValidateMigrationRequest.Records` are opaque `{ref, fields}` pairs with
  no notion of an account code or a ledger side. Deriving GL posting lines
  from validated records automatically (rather than the caller supplying
  both, as `PromoteBatchRequest` does) would require a mapping/transform
  layer this service does not have and this task did not add; see the
  worked example below for how the two are joined by hand via the
  crosswalk's `source_ref`.

## D. `MigrationProgram` and `SourceExtract` (§4 object model, continued)

A later pass on top of C1-C7 above. §4 still names two objects neither this
service nor anything else in the platform modelled: `MigrationProgram` (the
top-level "this is one migration project" tracking object) and
`SourceExtract` (a frozen, versioned landing of source data, MIG-INV-03:
"Source extracts are immutable after landing; corrections create new
extract versions"). This pass adds exactly those two, following C1-C7's own
conventions (authz action naming, tenant/per-entity scoping, migration
numbering, outbox events via `enqueueMigrationEvent`), and nothing else --
the rest of A/B/C above is unchanged and still accurate.

- **D1. `MigrationProgram`** (`migration_programs`, migration `000009`).
  `tenant, program_id, source estate, target scope, owner, status, cutover
  strategy` per §4, with a `status` lifecycle (`OPEN -> IN_PROGRESS ->
  COMPLETE`, linear and forward-only -- `domain.NextProgramStatus`, the same
  guarded-lifecycle shape `accounting-period-svc` uses for its own periods)
  set only by an explicit `POST /v1/migrations/programs/{id}/status`, never
  inferred from its batches' state. `migration_batch_refs` gained a nullable
  `program_id` FK in the same migration (additive: every batch ref promoted
  before this migration, and any `/promote` call that still omits
  `program_id`, keeps exactly its pre-existing "no program" semantics --
  `PromoteBatchRequest.ProgramID` is optional). `GET
  /v1/migrations/programs/{id}` returns the program plus a live
  `ProgramBatchSummary` (total batches, a count per promotion status,
  reconciled count) computed by `GROUP BY status` over
  `migration_batch_refs` at read time, never a stored counter that could
  drift from the batches it describes.

- **D2. `SourceExtract`** (`migration_source_extracts`, migration `000010`).
  `extract_id, source watermark, generated_at, files/objects, manifest
  hash, record count, amount controls` per §4. Append-only: there is no
  update endpoint anywhere in this service for it, and the database itself
  refuses `UPDATE`/`DELETE`/`TRUNCATE` on the table outright (the same
  `BEFORE ... RAISE EXCEPTION` trigger pattern `accounting-period-svc` uses
  for its own append-only `period_state_history`,
  `deployments/migrations/000002_immutability.up.sql` there --
  `migration_source_extract_forbid_mutation()` here). A "correction" is a
  new row: `POST /v1/migrations/extracts` with an already-used `extract_id`
  registers `version = previous max + 1` (`store.CreateExtract`, guarded by
  `pg_advisory_xact_lock` on the `(tenant, extract_id)` pair so two
  concurrent corrections can never race to the same version number); the
  original row is never touched. `GET /v1/migrations/extracts?extract_id=`
  lists every version of one extract in order, which is the visible proof
  that a correction is a new row, not a rewrite.

- **D3. `/validate` now optionally binds to an extract** (`migration_jobs`
  gained nullable `source_extract_id`/`source_extract_version`, migration
  `000011`). `ValidateMigrationRequest.SourceExtractID` is optional and
  backward compatible: a caller that omits it validates the submitted
  records directly, exactly as this endpoint behaved before `SourceExtract`
  existed (nothing that calls `/validate` today needs to change). When
  provided, the handler requires the extract's `legal_entity_id` to match
  the request's and its `record_count` to equal `len(Records)` before
  validating -- refusing with 409 otherwise -- so a retry cannot silently
  validate a different population under an already-registered extract's
  name. This is deliberately a consistency check on the extract's own
  declared header, not a re-fetch of the extract's actual file contents:
  this service holds no file/object store client, matching C7's own note
  that this pass does not add a mapping/transform layer it does not have.

- **D4. Authorization actions added**: `MIGRATION_PROGRAM_WRITE`,
  `MIGRATION_PROGRAM_VIEW` (tenant-scoped, like
  `MIGRATION_CROSSWALK_*` -- a program is not attached to one legal entity),
  `MIGRATION_EXTRACT_REGISTER`, `MIGRATION_EXTRACT_VIEW` (per-legal-entity,
  like the job/batch-ref actions -- an extract names exactly one
  `legal_entity_id`).

- **D5. Proven end to end** (manual proof against the same minimal compose
  slice C1-C7 used, `deployments/migration-e2e/`; not re-committed as a
  script): create a program -> register a `SourceExtract` v1 ->
  `/validate` against it (record-count mismatch refused with 409 when
  tried) -> resolve the crosswalk -> `/promote` (now carrying `program_id`)
  drives general-ledger-svc's real Stage -> Validate -> Approve -> Post ->
  Reconcile to `RECONCILED`/`MATCHED` -> `/certificate` issues, backed by
  GL's fresh evidence read -> `GET /programs/{id}` reports
  `total_batches: 1, status_counts: {RECONCILED: 1}` -> a direct `UPDATE`/
  `DELETE` against `migration_source_extracts` for that extract, run as the
  Postgres superuser itself (not through this service's API at all), was
  refused by the trigger with `MIG-INV-03` in the error text -> the correct
  path, registering `extract_id` again, created v2 while v1's
  `manifest_hash` stayed exactly `sha256:aaa111` -- confirming the refused
  mutation attempts left the original row untouched. The program's status
  was then walked `OPEN -> IN_PROGRESS -> COMPLETE` through `POST
  .../status`, with `IN_PROGRESS -> OPEN` and `OPEN -> COMPLETE` both
  refused with 409 as illegal transitions.

- **D6. Cutover-runbook gates (`G0`-`G10`) remain deliberately out of
  scope.** Per the Engineering Build Blueprint, the gated cutover-runbook
  capability (`CutoverRun`, `StabilizationCase`, and the `G0`-`G10`
  go/no-go gate sequence §4/§23 also name) is Phase 6 "trust maturity" —
  not a blocker for the program/extract/batch/certificate chain this pass
  and C1-C7 together now support end to end. `MigrationProgram` and
  `SourceExtract` are built because batches and certificates needed them to
  mean anything concrete (a batch belonging to no program, an extract with
  no frozen identity); the cutover runbook is a separate, later capability
  this task does not build and was not asked to.
