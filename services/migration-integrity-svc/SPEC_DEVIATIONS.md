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
- **A6. No quarantine severity or reason taxonomy.** §16 defines MQ0–MQ4
  severities with required dispositions and reason families (IDENTITY,
  REFERENCE, TEMPORAL, FINANCIAL, SUBLEDGER, TAX, DOCUMENT, SCHEMA, SECURITY,
  SOURCE). The service has INFO/WARNING/CRITICAL per check and five violation
  types. There is no owner on an audit entry and no blocking semantics (an MQ0
  must stop the batch; nothing here stops anything).
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
- **A11. Integrity score is a ratio, not an acceptance result.** The 0–100
  score has no threshold, no PASS/FAIL, and no link to certification;
  "totals match" / "score is high" as an acceptance criterion is one of the
  §33 anti-patterns. A job is `FAILED` only when every record is invalid;
  with one valid record out of a thousand it is `COMPLETED`.

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
