# Spec deviations — migration-integrity-svc (ZS-MIG-001)

Governing document: ZS-MIG-001 (Global Migration, Opening Balance & Cutover
Standard). The service-ownership table gives this service "migration
certification evidence" under ZS-MIG-001 · ACC-17 · Doc06 Phase 6, with
general-ledger as its dependency.

**What the service actually is.** A stateless record checker with an audit
trail: `POST /v1/migrations/validate` takes a list of records (`ref` + string
fields) and a list of required field names, runs three checks (required
fields present, duplicate `ref`, numeric format of four hard-coded field
names), stores the job, its check results and one audit entry per violation,
and emits events. `POST /v1/migrations/{id}/audit/{entryId}/remediate` flips
`is_remediated`; `DELETE /v1/migrations/{id}` archives. It never writes to a
target system, which is consistent with MIG-INV-06.

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
- **A2. None of the §4 object model exists.** MigrationProgram, SourceSystem,
  SourceDataset, SourceExtract (watermark, manifest hash, record count, amount
  controls), MappingRuleSet, IdentityCrosswalk, MigrationCandidate,
  QuarantineItem, MigrationRun, ReconciliationRun, MigrationCertificate,
  CutoverRun, StabilizationCase — none are modelled. A "job" has no extract
  identity, no source watermark, no mapping version and no batch identity
  (MIG-INV-02, -03, -05, -11; §17 certificate minimum).
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
  `quantity`); there is no schema-version check (MIG-C03, MIG-N03), currency
  check (MIG-C12, MIG-N12), date/timezone check (MIG-C13, MIG-N13) or
  referential check (the `REFERENTIAL_INTEGRITY` and `RANGE_CHECK` types and
  `ORPHANED_REFERENCE` / `OUT_OF_RANGE` violations are declared but never
  produced). Rules are not versioned, so a stored result cannot say which
  rule set produced it (MIG-INV-05).
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
