# Spec deviations — bank-reconciliation-svc (BNK-05)

Governing document: `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`,
§7 "BNK-05 — Bank Reconciliation", its statement of purpose ("Reconcile bank
statement activity and balances to authoritative ledger/subledger/payment/
receipt populations with reproducible matching, exceptions and close-gate
certification"), the §4 shared command/error/state contract, the §2 invariants
5, 6, 9, 10, 11, 12 and 19, the §3 authority row ("BNK-05 | Bank-to-book
match/exception/certificate state | Ledger edits, payment finality, tolerance
self-waiver"), the §5 entity table (`BankReconciliationRun`, `BankMatchGroup`,
`BankReconciliationCertificate`), the §7 events line, and the §12 negative-path
tests for BNK-05. First survey of this service against the spec (2026-10-22).

Citation check: every BNK-05 line cited in the review request was compared with
the docx text. The purpose, commands, queries, state model, permissions,
segregation-of-duties rule, events, failure/degradation semantics, evidence
line and the four minimum negative paths match the document exactly. The only
differences are presentational: the docx tables are flattened into
pipe-delimited text in the extracted copy, and the BNK-05 page itself does not
print the section number (§7 is the section that contains it). One nuance: the
BNK-05 page says "Result status (balanced/material exception) remains
orthogonal". It does not literally name a third, separate "certification"
dimension. The three-dimension check below is a design comparison with
`financial-control-svc`, not a quoted BNK-05 requirement.

Overall: the core of a reconciliation run is present — run/policy/certificate
tables, a frozen bank-side population with a hash, count and total, versioned
policies, a maker-checker manual-match path, line-level exact journal/canonical
verification, an append-only certificate, a financial-close period check and
an outbox event set. Roughly 55% of the named surface is present in a
defensible form. The material gaps are that reconciliation is **one-sided**
(no book/ledger/payment population and no balance reconciliation, A1), there is
no `BankMatchGroup` (A2), automatic matching is caller-sourced and has no
matching rules or matching tolerance (A3), half the named queries and four of
the eight lifecycle states are missing/unreachable (A4/A5), the legacy
`CompleteStatement` path is a weaker second certification path (A7), and late
data is not detected after freeze/certification (A8). The service is not yet a
complete BNK-05 control.

## What this pass changed

These are closed in this change and covered by the tests named below.

- **Complete certifier independence.** `CertifyRun` now gathers the run
  creator, the population freezer, every `matched_by_principal_id` in the
  frozen population and every retained `proposed_by_principal_id`, and applies
  `domain.CheckCertifierIndependence` (the `reconciliation-engine-svc`
  pattern) to the complete preparer set. Before, only matchers were checked, so
  a run creator, population freezer or manual-match proposer could certify.
  Tests: `TestCertifyRun_RunCreatorCannotCertify`,
  `TestCertifyRun_MatchProposerCannotCertify`, and the pre-existing
  `TestCertifyRun_RejectsSelfCertification`.
- **Every non-`MATCHED` included line is unresolved.** Certification now
  counts `status <> 'MATCHED'`, so `EXCEPTION` and `PENDING_CONFIRMATION`
  block when the threshold fails; previously only `UNMATCHED` was counted, and
  a run could certify with an exception or an unapproved proposal outstanding.
  Test: `TestCertifyRun_CountsExceptionAndPendingAsUnresolved`.
- **The unresolved-amount tolerance is pinned and enforced.** Migration
  `000014_run_unresolved_amount` adds `max_unresolved_amount` and
  `policy_currency` to `reconciliation_runs` and backfills them from each
  run's bound policy. `BindPolicy` pins them; `SupersedeRun` copies them;
  `CertifyRun` enforces the absolute unresolved exposure in the pinned
  currency. A run with unresolved lines in another currency is refused with
  `ErrUnresolvedExposureUnassessable` / HTTP 422
  `unresolved_exposure_unassessable` rather than assumed within tolerance.
  Before, the policy carried `max_unresolved_amount` but the run had nowhere
  to pin it and certification never applied it, so one enormous exception
  could pass a count/per-cent check. Tests:
  `TestCertifyRun_EnforcesMaxUnresolvedAmount` (blocked, within, other
  currency) and `TestCertifyRun_UnresolvedExposureUnassessable_Returns422`.
- **Policy widening is checked against every active bound run.** `CreatePolicy`
  now loops all active runs for the legal entity and refuses a new policy that
  widens count, per-cent **or amount** against any of them, and refuses a
  currency change while a run is bound. Before, it compared only the newest
  bound policy, and ignored the amount entirely. Tests:
  `TestCreatePolicy_WideningCheckedAgainstEveryActiveRun`,
  `TestCreatePolicy_WideningChecksUnresolvedAmount`, and the pre-existing
  `TestCreatePolicy_WideningBlockedWhenRunActive`.
- **Freeze and bind-policy are authorized.** `FreezePopulation` and
  `BindPolicy` now require `BANKREC_RUN` against the run's own legal entity.
  Before, any authenticated principal could freeze the population that would
  be certified or pin the thresholds. Tests:
  `TestFreezePopulation_AuthorizationDenied_Returns403`,
  `TestBindPolicy_AuthorizationDenied_Returns403`.
- **Concurrent certification is serialized.** `CertifyRun` takes `FOR UPDATE`
  on the run row before reading its status, so racing certifiers replay one
  certificate instead of one losing on the certificate unique index. Test:
  `TestCertifyRun_ConcurrentCallsProduceOneCertificate`.

Migration 000014 is additive; its down migration drops only the two new
columns. No existing row is rewritten except the backfill from its already-bound
policy.

## A. Not implemented / weaker than the spec

- **A1. Reconciliation is one-sided: there is no book/ledger population and no
  balance reconciliation.** `FreezePopulation` reads only `statement_lines`
  for the run's bank account and date. `verifyJournalMatches` verifies one
  caller-supplied journal at a time and only when a match is attempted; there
  is no ledger cash population, no payment/receipt population, no opening or
  closing balance, no book-side hash/count/total, and no balance
  reconciliation. The spec's purpose says "activity **and balances** to
  authoritative ledger/subledger/payment/receipt **populations**"; invariant 9
  says "Bank reconciliation certifies populations, not merely matching totals";
  the required inputs name ledger cash entries, payment/receipt facts and
  opening/closing balances. A bank-only population can certify while the book
  side is missing an equal and opposite item, because the book side was never
  enumerated. This is the largest gap and the reason the service cannot yet
  satisfy the "populations differ" half of negative path #1. *Why it was not
  fixed here:* it needs a population client/contract for ledger and payment
  facts, a two-sided snapshot schema and a balance-reconciliation rule. It is
  not a local patch. The proven shapes are `financial-control-svc`'s
  `control-population-contract.md` and `reconciliation-engine-svc`'s
  `population_snapshots` / sealed certification manifest.

- **A2. There is no `BankMatchGroup`; exact/partial/manual match state is
  collapsed into one line's columns.** The §5 entity table gives BNK-05
  authority over `BankMatchGroup` — "exact/partial/manual match and evidence".
  This service has no match-group table. A `statement_lines` row carries at
  most one `matched_journal_id` or `matched_transaction_id` (plus the
  proposal columns). Consequences:
  - no many-to-one, one-to-many or split matches;
  - no partial-match amount or residual;
  - no match identity to retrieve (`GetMatch` is absent, A4);
  - no group-level evidence or approval;
  - a rematch overwrites `matched_by_principal_id`/`matched_at`; the prior
    match survives only as an outbox event, not as queryable history.

  `UnmatchWithReason` deliberately leaves the old match columns in place, so a
  line can show a historical and a current match at the same time. The spec's
  "exact/partial/unmatched/exception states" are therefore not represented:
  the service has `UNMATCHED`, `PENDING_CONFIRMATION`, `MATCHED` and
  `EXCEPTION`, but no exact/partial distinction.

- **A3. `RunAutomaticMatching` is caller-sourced, and there is no matching
  tolerance.** The command requires the caller to supply each
  `(statement_line, journal|transaction)` candidate. The service verifies the
  candidate exactly — journal `FINALIZED`, same legal entity, posts to the
  line's `gl_cash_account_code`, exact amount — and then matches it. It does
  not generate candidates from the populations, score them, apply a
  date/reference/amount tolerance, apply FX/fees, or use a bank cut-off. The
  policy's count/per-cent/amount thresholds are certification materiality, not
  matching tolerance. §7's "matching rules/tolerances" and invariant 10's
  distinction between matching tolerance and materiality are therefore not
  met. *Why it was not fixed here:* candidate generation needs both
  populations (A1) and a rule/version model; the current exact per-candidate
  verification is a deliberate "never guess" posture, not the named command.

- **A4. Half the named query surface is missing.** `GetReconciliation` exists
  as `GET /v1/reconciliation-runs/{run_id}`. `GetPopulationSnapshot` exists
  only as metadata: `GET /v1/reconciliation-runs/{run_id}/population` returns
  the `reconciliation_populations` row and never the
  `reconciliation_population_lines`. There is no `GetMatch`,
  `ListUnmatchedItems`, `GetExceptions`, `GetCertificate` or
  `GetAvailableActions` route, and no store method for `GetMatch`,
  `GetExceptions`, `GetCertificate` or `GetAvailableActions`. `GetPolicy`
  exists in the store but has no route. A caller cannot retrieve a certificate
  after certification, inspect the population's line items, list the
  unmatched/exception queue, or ask which action is legal next.

- **A5. Four lifecycle states are unreachable, and result/certification are
  not separate dimensions.** `EXCEPTIONS_OPEN`, `REPERFORMED`,
  `READY_FOR_CERTIFICATION` and `FAILED` are defined in
  `domain/run_types.go`, but no handler or store path sets them;
  `AdvanceRunStatus` has no route and no production caller. The normal path is
  `DRAFT -> RUNNING -> CERTIFIED`, or `SUPERSEDED`. There is no orthogonal
  result status (`BALANCED` / `MATERIAL_EXCEPTION` / `INDETERMINATE`), and
  certification is only `status = CERTIFIED` plus a certificate row, not a
  separate certification dimension. The BNK-05 state line says "Result status
  (balanced/material exception) remains orthogonal"; the comparison with
  `financial-control-svc` shows the missing result and certification axes. A
  run that fails tolerance is simply refused and stays in its prior lifecycle
  state; it never records a material-exception result. `SupersedeRun` also
  accepts a `DRAFT`/`RUNNING` run, not only a `CERTIFIED` one, so
  `SUPERSEDED` is not tied to the certified state the spec's lifecycle
  implies.

- **A6. Manual-match evidence and approval are incomplete, and the
  single-actor path bypasses maker-checker.** `ProposeMatch` records the
  proposer, time and target; `ConfirmMatch` records the confirmer and time;
  `RejectProposedMatch` requires a reason. An **approved** manual match records
  no reason, no evidence reference and no approval beyond the confirmer's
  principal id. There is no `bank.recon.manual.approve` action: propose,
  confirm and reject all authorize `BANKREC_MATCH`, so one permission covers
  maker and checker (the principal-identity check blocks self-confirmation,
  but not a role that should not approve). The legacy
  `POST /v1/statement-lines/{id}/match` remains a single-actor match with the
  same `BANKREC_MATCH` action; a principal can bypass propose/confirm
  entirely. All manual paths are line-scoped, not run-scoped: they work on any
  line, including a line outside a frozen population or one in a certified
  run. Invariant 11 requires actor, reason, evidence and approval where
  required; this service retains actor and (on reject) reason only. *Why it
  was not fixed here:* it changes the request contract, the permission map and
  the match/evidence schema; it is a design item, not a local patch.

- **A7. `CompleteStatement` is a second, weaker certification path.**
  `POST /v1/bank-accounts/{bank_account_id}/statements/{statement_date}/complete`
  counts only `UNMATCHED` lines and then writes a legacy
  `reconciliation_certificates` row with `run_id IS NULL`. It has:
  - no certifier independence;
  - no frozen population, hash, count or total;
  - no policy/tolerance/amount check;
  - no `EXCEPTION` or `PENDING_CONFIRMATION` check;
  - no open-conflict check;
  - no financial-close period check;
  - a different authorization action, `BANKREC_COMPLETE_STATEMENT`, not
    `BANKREC_CERTIFY`.

  A statement whose lines are all `EXCEPTION` has `UNMATCHED = 0` and
  certifies with `matched_line_count = 0`. It can also write a legacy
  certificate for a statement already certified by a run, because the legacy
  unique index (`tenant_id, bank_account_id, statement_date WHERE run_id IS
  NULL`) and the run unique index are separate. This path alone fails
  negative paths #1 and #3. *Why it was not fixed here:* removing or
  forwarding a public compatibility route is a deployment/contract decision;
  the safe next step is to make it delegate to the run certification, or
  refuse when a run certificate exists.

- **A8. Late data after freeze/certification is not detected.** The frozen
  population is append-only, and `CertifyRun` evaluates membership in it
  rather than live `statement_lines`;
  `TestCertifyRun_EvaluatesFrozenPopulationNotLateData` proves a line added
  after freeze is ignored. That protects the certificate from silent mutation,
  which is good. But there is no late-data sweep, no re-read of a source
  watermark, no `LATE_DATA` exception, and no close-gate flag. Late lines
  simply sit outside the population. Separately, `UnmatchWithReason` can
  change a live line that belongs to a certified run's population; the
  certificate is not reopened and no late-data exception is raised. The
  unmatch emits `reconciliation.exception.raised`, but the run stays
  `CERTIFIED`. The spec's failure semantics say late data cannot mutate a
  certified population silently; the population row does not mutate, but the
  live population diverges silently. *Why it was not fixed here:* it needs a
  real source watermark to compare against; this service's
  `source_watermark` is only `max(created_at)` of its own rows, not BNK-03's
  import watermark (A12). `financial-control-svc` A7/B9 is the proven sweep
  shape.

- **A9. Open `evidence_conflicts` do not block certification.** The consumer
  for payment-status-svc's `PAYMENT_STATUS_CONFLICT_RAISED` creates
  `evidence_conflicts`, and raise/list/get/resolve exist. `CertifyRun` never
  reads the table. A run can certify with an open bank-vs-provider conflict on
  one of its lines. Invariant 19 requires a conflict path, and the BNK-05 SoD
  line says control exceptions need authorized remediation; the record exists,
  but it is not a gate.

- **A10. Read endpoints do not check `BANKREC_READ`.** Only
  `GET /v1/reconciliation-runs/period-status` authorizes `BANKREC_READ`.
  `GetStatementLine`, `ListStatementLines`, `GetRun`, `GetRunPopulation`,
  `GetCurrentPolicy`, `ListConflicts` and `GetConflict` are tenant-scoped
  only. Within a tenant, a principal with no banking read grant can read any
  entity's statement lines, runs, populations, policies and conflicts if it
  can reach the service. The demo seed documents this as a deliberate
  platform posture ("the same posture as general-ledger-svc"), but BNK-05's
  permission line names `bank.recon.read`, so it is a deviation.
  `payment-status-svc` closed the same gap with `payment.status.read`; that is
  the precedent.

- **A11. Policy governance is self-service, and policy inputs are not
  validated.** `CreatePolicy` and `BindPolicy` both authorize `BANKREC_RUN`;
  the same principal can create a permissive policy (when no active run is
  bound) and bind it to their own run. There is no independent approval, no
  `approved_by`, and no policy-approval endpoint. `effective_from` /
  `effective_to` are stored but never enforced: `GetCurrentPolicy` returns the
  highest version, and `BindPolicy` accepts any policy id for the tenant,
  including one not yet effective or expired. `BindPolicy` does not check that
  the policy's `legal_entity_id` matches the run's, so a run for entity A can
  pin entity B's thresholds. `CreatePolicy` does not validate that
  `max_unmatched_pct` is a 0..1 fraction: the code compares it to
  `unresolved/total` (0..1), but the column accepts up to 999.9999, and the
  existing tests use `5.0` as a "loose" value (500%). Negative values are also
  accepted; a negative `max_unmatched_count` makes `CertifyRun` skip the
  count/per-cent branch (`>= 0` guard), although the amount check still
  applies when a policy is bound. Invariant 10 and the authority row's
  "tolerance self-waiver" prohibition are not met. *Why it was not fixed
  here:* policy approval is a contract/RBAC change, and tightening validation
  would invalidate existing test fixtures; it needs a deliberate migration of
  policy values.

- **A12. Entity and source scope are not enforced consistently.** The
  statement `source_watermark` is this service's `max(created_at)`, not
  BNK-03's import watermark, and `statement_lines` is this service's own
  register: it stores no BNK-03 statement id, raw source hash/version,
  parser/mapping version or declared totals. Invariants 5 and 6 put raw
  statement evidence, source ids and the import watermark in BNK-03; BNK-05
  should consume that evidence, not re-own it. The only ingest uniqueness is
  `(tenant_id, correlation_id)`, so the same bank line with a new correlation
  id is accepted as a new line (negative path #1's duplicate-items case).
  Within the service, `FreezePopulation` selects lines by tenant, bank account
  and date but not `legal_entity_id`, so a population can include another
  entity's lines and is then labelled with the run's entity. `StartRun`'s
  idempotent retry returns an existing active run with the same
  account/date/correlation without checking its `legal_entity_id`, so a caller
  authorized for entity B can receive entity A's run. `BindPolicy` can bind
  another entity's policy (A11). The §4 context contract says legal entity is
  resolved server-side; these paths let the caller's entity and the resource's
  entity diverge. *Why it was not fixed here:* the account-to-entity invariant
  is documented but not constrained in the schema, so a filter would silently
  drop mis-scoped lines; the better fix is a fail-closed check plus a data
  constraint.

- **A13. The certificate and its events are thin.** `reconciliation_certificates`
  records `matched_line_count`, certifier/time, `run_id`, `population_id` and
  `population_version`. It does not record the population hash, line count,
  total, policy/tolerance version, unresolved count/exposure, manual-match
  reasons/approvals, or an authorization/authority snapshot. It references the
  population, so a join could recover some of it, but the certificate response
  and event do not carry it, and there is no `GetCertificate` (A4). The §7
  evidence line names "Population IDs/hashes/counts/totals, rules/tolerance
  version, matches, manual reasons/approvals, unresolved exposure,
  reperformance and certificate"; the §4 Evidence row names the request
  subject, approval/authorization fingerprint, evidence and versions. The
  `run.certified` event carries only `run_id`, `certificate_id`, tenant,
  account, date and matched count. `PeriodReconciliationStatus` likewise
  returns only latest-certified/latest-run digests, with no unresolved count,
  exposure or reason, so financial-close-svc cannot apply BNK-05 materiality
  from it. The `run.superseded` event also uses the old run's creator and
  correlation as its actor/correlation, not the principal who performed the
  supersede. This is materially thinner than `reconciliation-engine-svc`'s
  sealed `certificationManifest` (both populations by hash, declared
  counts/totals, tolerance, rules hash, manual matches, certifier authority).

- **A14. Concurrency and idempotency gaps remain outside certification.**
  `CertifyRun` now locks the run, but:
  - `StartRun` still does check-then-insert with no partial unique index on
    active runs, so two concurrent starts can create two active runs;
  - `SupersedeRun` does not lock the existing run or check for another active
    run, so two concurrent reperforms can create two DRAFTs and leave one
    unlinked; it can also supersede a certified run while a newer active run
    exists;
  - `FreezePopulation` does not lock the run; two concurrent freezes both read
    `population_id IS NULL`, one wins the `reconciliation_populations(run_id)`
    unique index, the other gets a raw unique violation (mapped to 503, not an
    idempotent replay);
  - there is no `If-Match`/ETag on mutable commands and no `Idempotency-Key`;
    repeated propose/confirm returns 409 rather than the original result.

  The §4 contract asks for expected-version semantics and durable idempotency
  for material commands. *Why it was not fixed here:* active-run uniqueness
  needs a partial index and a migration; Supersede/Freeze need locking and a
  retry design.

- **A15. N+1 / query shape.** `FreezePopulation` does one `SELECT` for all
  lines and then one `INSERT` per line inside the transaction.
  `CertifyRun` runs a run load, a preparer `UNION`, an unresolved count, a
  matched count and an exposure aggregate over the same population join; the
  counts can be one `COUNT(*) FILTER` query and the exposure can be folded in.
  `RunAutomaticMatching` loads the unmatched population once (good), but then
  does one external verification and one match transaction per candidate; that
  is inherent to per-candidate verification, not a hidden per-row read, but it
  is O(N) round trips and the NFR asks for close-window capacity.
  `PeriodReconciliationStatus` is two queries, which is fine. The freeze
  insert loop is the clearest candidate for `CopyFrom`/batch insert; the
  certification aggregates are the clearest candidate for one query.

- **A16. Amounts are `float64`, not exact.** `statement_lines.amount` is
  `NUMERIC(18,2)`, but the domain uses `float64` and `FreezePopulation`
  computes cents with `math.Round(amount * 100)`. Policy thresholds are
  `float64` too, and certification converts `max_unresolved_amount` to cents
  the same way. Values above the exact integer range (or repeated arithmetic)
  can lose a cent; the `financial-control-svc` and `reconciliation-engine-svc`
  patterns use exact decimal/rational arithmetic. The NFR asks for
  deterministic population reproduction and exact control totals.

## B. Interpretations and decisions a reviewer may want to reverse

- **B1. `SupersedeReconciliation` is folded into `ReperformReconciliation`.**
  `POST /v1/reconciliation-runs/{run_id}/reperform` marks the existing run
  `SUPERSEDED`, creates a new `DRAFT` for the same account/date, copies the
  pinned policy values, and emits `run.reperformed` + `run.superseded`. There
  is no separate `SupersedeReconciliation` route and no mandatory reason or
  evidence. The new run is not automatically refrozen or re-matched; the
  caller must call freeze and auto-match. The old certificate remains
  immutable and is not marked superseded; only the run's status and
  `superseded_by_run_id` show the replacement. A reviewer may want a distinct
  command, a reason field, and a certificate-supersession marker.

- **B2. A policy is optional, and the three thresholds are not a single
  conjunction.** A run with no bound policy uses the `reconciliation_runs`
  defaults (`max_unmatched_count = 0`, `max_unmatched_pct = 0`) and skips the
  amount check entirely. When a policy is bound, certification passes the
  count/per-cent test if **either** the count is within `max_unmatched_count`
  or the percentage is within `max_unmatched_pct`; the amount test is then
  applied separately and always, when the policy is bound. The three values
  are therefore not "all must pass". The amount test refuses any unresolved
  line in a currency other than the pinned policy currency (HTTP 422) rather
  than applying FX. These are deliberate fail-closed choices from this pass;
  a reviewer may prefer a required policy, a conjunction, or an FX basis.

- **B3. The permission map has no distinct manual-approve action, and the
  demo seed has not caught up with the run actions.** The spec names
  `bank.recon.read`, `bank.recon.run`, `bank.recon.match`,
  `bank.recon.manual.approve` and `bank.recon.certify`. The service uses
  `BANKREC_READ`, `BANKREC_RUN`, `BANKREC_MATCH` and `BANKREC_CERTIFY`;
  propose, confirm and reject all use `BANKREC_MATCH`; the legacy completion
  path uses `BANKREC_COMPLETE_STATEMENT`. The access-control taxonomy
  (`services/access-control-svc/deployments/migrations/000008_permission_taxonomy.up.sql`)
  registers `BANKREC_RUN`, `BANKREC_CERTIFY`, `BANKREC_READ` and
  `BANKREC_CONFLICT_RESOLVE`, but the demo seed
  (`deployments/scripts/seed-demo-rbac.ps1`, `BANKREC_FULL`) grants only
  `BANKREC_STATEMENT_INGEST`, `BANKREC_MATCH`, `BANKREC_FLAG_EXCEPTION` and
  `BANKREC_COMPLETE_STATEMENT`. In a demo tenant, the run, certify, read and
  conflict-resolve actions are not grantable through the seeded bundle; a
  tenant must define a custom role or the seed must be updated. This is a
  deployment prerequisite, not a service-code defect, but it makes the new
  run endpoints fail closed in the demo environment.

- **B4. Events use the platform envelope, not the spec's PascalCase names.**
  Every fact is emitted at least once: `run.started`, `matched`,
  `exception.raised`, `run.reperformed`, `run.certified`, `run.superseded`,
  plus `completed` and the evidence-conflict facts. The canonical type is
  `com.zoikosuite.banking.reconciliation.<fact>` with a legacy
  `reconciliation.*` type; the spec's `BankReconciliationStarted`,
  `BankMatchCreated`, `BankMatchExceptionRaised`,
  `BankReconciliationReperformed`, `BankReconciliationCertified` and
  `BankReconciliationSuperseded` are conceptual names. The legacy
  `CompleteStatement` path emits `reconciliation.completed`, not
  `reconciliation.run.certified`, so a consumer that watches only the run
  certification event will not see a legacy completion. A platform-wide event
  naming decision would settle the rest.

- **B5. Error codes are service-local, not the shared problem types.** The
  §4 contract lists `RECONCILIATION_UNRESOLVED` among the stable problem types.
  This service returns `material_residual_blocked`,
  `unresolved_exposure_unassessable`, `run_invalid_transition`,
  `population_not_frozen`, `self_certification_forbidden`, etc. A gateway or
  client mapping is needed if the shared taxonomy is mandatory.

- **B6. The close period is derived as `YYYY-MM` from the statement date, and
  an unregistered period is treated as open.** `CertifyRun` calls
  financial-close-svc with `period_name = statementDate.Format("2006-01")`
  and fails closed on a transport error or an unrecognized status. A 404
  ("period never registered") is treated as open, matching the existing
  general-ledger-svc and financial-control-svc clients. A reviewer may prefer
  to fail closed on 404 or to use a fiscal-calendar service; neither exists in
  this repository.

- **B7. `source_watermark` is a local placeholder.** Until BNK-03 integration
  (A12), the population's `source_watermark` is `max(created_at)` of the
  included statement lines. It records when this service ingested the rows,
  not when the source last changed. Any late-data sweep built on it would
  compare the wrong clock. This is documented as a decision so a future
  integration does not mistake it for a source watermark.

- **B8. Certificate immutability and supersession.** A certificate is
  append-only and never updated or deleted (trigger
  `reject_certificate_mutation`). Superseding a run does not mark its
  certificate; `PeriodReconciliationStatus` ignores superseded runs when
  choosing `LatestCertified`, so the close query is safe, but a direct
  certificate read (A4/A13) would show an apparently current certificate for a
  superseded run. A reviewer may want an explicit `SUPERSEDED` certificate
  state or a `superseded_by_certificate_id` reference.

## C. Duplication vs. genuine difference

### C1. Where this service duplicates a pattern another service solved better

- **Run/policy/certification lifecycle.** `financial-control-svc` models
  lifecycle, result and certification as three dimensions, with a separate
  `ControlCertification` record, late-data exceptions, revocation and a close
  gate. `reconciliation-engine-svc` seals a `certificationManifest` with both
  populations by content hash, declared counts/totals, tolerance, rules hash,
  manual matches and the certifier's authority snapshot. BNK-05 is a third,
  thinner variant: one lifecycle, no result dimension, a certificate with only
  a matched count, and no late-data sweep. The right move is to reuse one of
  those models (or the shared control-population contract), not to grow a
  fourth.
- **Population snapshots.** `reconciliation-engine-svc`'s
  `population_snapshots` are two-sided, generation-aware, and carry
  `declared_row_count`, `declared_totals`, `source_watermark`, `spec_ref` and a
  content hash. BNK-05's `reconciliation_populations` /
  `reconciliation_population_lines` freeze one side from its own table and
  compute a local hash. The local hash/count/total are a useful start, but they
  are not an independent completeness proof; the `reconciliation-engine-svc`
  shape is stronger and already tested.
- **Certification segregation of duties.** BNK-05 now copies
  `reconciliation-engine-svc`'s `CheckCertifierIndependence` and the
  complete-preparer set. This is a **good** duplication: the rule is identical
  (a preparer cannot self-certify), and the shared shape makes it auditable.
  It should stay aligned with the engine's implementation.
- **Legacy completion vs. run certification.** `CompleteStatement` duplicates
  the certification idea with weaker rules (A7). Unlike the SoD function, this
  duplication is not an improvement; it should delegate to run certification
  or be retired.
- **Read authorization.** `payment-status-svc` added a `payment.status.read`
  check to its read endpoints; BNK-05 kept tenant-only reads (A10). The
  tenant-scoping pattern is correctly duplicated from general-ledger-svc, but
  the read-authz pattern was not.
- **Event envelope/outbox.** The shared `envelope` + `eventing_outbox` +
  `inbox` pattern is correctly reused; this is not a deviation. The only
  BNK-05-specific issue is the event naming/payload thinness (B4/A13).

### C2. Where the service genuinely differs for good reason

- **Bank-specific exact verification.** `verifyJournalMatches` checks that the
  journal is `FINALIZED`, belongs to the same legal entity, posts to the
  line's `gl_cash_account_code`, and moves the exact amount in the right
  direction. `verifyCanonicalMatch` checks the BNK-03 canonical transaction's
  status, tenant, currency and exact amount. A generic control run cannot
  express the cash-account/direction check; this is a genuine BNK-05 control
  and should be preserved when the book-side population is added.
- **Line-level maker-checker manual matches.** `financial-control-svc` has no
  manual match at all (its A5). BNK-05's propose/confirm/reject path is a real
  bank-reconciliation capability; the gap is evidence/approval (A6), not the
  existence of the path.
- **Payment-status conflict join.** The consumer that turns
  `PAYMENT_STATUS_CONFLICT_RAISED` into `evidence_conflicts` is a genuine
  BNK-05/BNK-07 boundary join, not something `financial-control-svc` models.
  It should be extended to block certification (A9), not removed.
- **Financial-close period check.** Checking the period is open before
  certification is a genuine close-window integration and is stronger than a
  pure in-service control. Keep it; add the legacy path to it (A7).
- **Per-account, per-statement-date granularity.** A bank reconciliation is
  naturally scoped to one account and one statement date, not to a control's
  period/entity scope. The lighter run model is appropriate for that
  granularity. This does not justify the missing book side (A1): the same
  account/date can and should have a two-sided population.
- **Reperformance as supersede + new DRAFT.** Reusing the existing run row and
  preserving the old certificate is a reasonable BNK-05 correction model. The
  gap is that reperform does not refreeze/rematch and does not mark the old
  certificate (B1), not that it differs from a generic control run.

## D. Verification

- Before changes: `go test -count=1 ./...` passed.
- After changes: `gofmt` applied; `go build ./...` passed;
  `go test -count=1 ./...` passed.
- Integration suite: a fresh disposable `postgres:16-alpine` container
  (`bankrec-it-20261022`) on `127.0.0.1:55444`, database `bankrec_it`,
  `TEST_DATABASE_URL=host=127.0.0.1 port=55444 dbname=bankrec_it user=postgres
  password=postgres sslmode=disable`,
  `go test -tags=integration -count=1 -timeout=400s ./...` passed every
  package (`internal/store` 6.998s). The container was removed afterwards.
- The new governance tests were re-run against a second fresh database
  (`bankrec_it2`) to confirm they actually execute: all seven passed —
  `TestCertifyRun_RunCreatorCannotCertify`,
  `TestCertifyRun_MatchProposerCannotCertify`,
  `TestCertifyRun_CountsExceptionAndPendingAsUnresolved` (exception and
  pending subcases), `TestCertifyRun_EnforcesMaxUnresolvedAmount` (blocked,
  within, other-currency subcases),
  `TestCreatePolicy_WideningCheckedAgainstEveryActiveRun`,
  `TestCreatePolicy_WideningChecksUnresolvedAmount`, and
  `TestCertifyRun_ConcurrentCallsProduceOneCertificate`.
- The store integration harness applies every `*.up.sql` by glob and drops
  only a subset of tables; a second run against the same database fails on
  `evidence_conflicts` already existing. A fresh database per integration run
  is required. This is a test-harness limitation, not a service defect.
- Existing coverage for the pre-existing behavior remains: the frozen-
  population/late-data test (`TestCertifyRun_EvaluatesFrozenPopulationNotLateData`),
  the policy-widening test (`TestCreatePolicy_WideningBlockedWhenRunActive`),
  the run-level SoD test (`TestCertifyRun_RejectsSelfCertification`), the
  conflict store tests, and the tenant-isolation suite.
