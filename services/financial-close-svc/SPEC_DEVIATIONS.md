# Spec deviations — financial-close-svc (ZS-SVC-B-001)

Governing document: ZS-SVC-B-001, *Accounting & Record-to-Report Detailed
Service Specifications* (ACC-01 through ACC-18), sections 9–13 (ACC-06 to
ACC-10), 17 (ACC-14), 20 (ACC-17), 21 (ACC-18) and the shared §2 invariants,
§22 command/error contract and §23 event baseline. The extracted working copy
is `scratch/accounting_r2r_spec.txt`. This is the first deviation register
for these capabilities; `SPEC_DEVIATIONS_REF05.md` covers only the REF-05
period-mirror cutover and is unchanged.

Scope: ACC-06, -07, -08, -09, -10, -14, -17 and -18 against the real handlers
(`internal/handler/handler.go`, `period_close.go`, `period_mirror.go`), the
store, the GL/subledger clients and migrations 000001–000021. ACC-16 is in
this service too but outside this survey; where a finding applies to it as
well, that is noted.

Cross-reference: the brief for this survey described the ACC-17 ↔
migration-integrity-svc boundary as risk-register item R15. In
`docs/ZoikoSuite Domain Ownership.md`, R15 is "Chart of accounts entity
missing". The boundary itself is recorded in
`services/migration-integrity-svc/SPEC_DEVIATIONS.md` A1/A3/A4.

Verification (2026-10-24): `go build ./...`, `go vet ./...`,
`go vet -tags=integration ./...`, `go test ./... -count=1` and
`go test -tags=integration ./... -count=1` were all run against a fresh,
disposable `postgres:16-alpine` container (database `financial_close_test`,
migrations 000001–000021 applied from empty by the suites). All packages
passed. 45 store tests ran against the database, none skipped. The only
skips are the REF-05 mirror handler tests, which already carry a deliberate
`t.Skip` ("REF-05 mirror rebase pending") and were not touched. Migration
000021 was also checked by hand: `psql --single-transaction -v
ON_ERROR_STOP=1` applied 000001–000021 to an empty database, ran 000021 down
and up again, and then attempted each new guard (results under B1, B2 and
B4). No shared development database was used.

## Verified controls

Checked against code and schema, not comments:

- **ACC-14 state machine and reopen governance.** OPEN → SOFT_CLOSE →
  CLOSE_REVIEW → HARD_CLOSED → AUTHORIZED_REOPEN → RECLOSED. Each transition
  re-reads the period `FOR UPDATE` and writes an append-only
  `period_state_transitions` row in the same transaction. The one-step reopen
  is retired (410). A reopen needs a request plus a different principal's
  approval; that is checked in the handler, the store and the
  `period_reopen_requests_independent_decision` CHECK. The reopen window is
  bounded (30 days). A reclose accepts only control runs and bank
  reconciliations performed after the reopen. Readiness fails closed on any
  dependency error. The close's reliance manifest is hashed and HMAC-signed
  with `CLOSE_SIGNING_KEY`. The period is now also guarded at the data layer
  (B4).
- **Close gate.** The AR and AP subledger controls are a baseline that cannot
  be removed. Waiving a bank account needs its own permission
  (`PERIOD_CLOSE_EXCLUSION_APPROVE`) and a reason. The latest run of every
  required control must be `MATCHED`, and an `EXCEPTION` blocks the close.
- **Posting refusal in general-ledger-svc.** GL calls
  `GET /v1/close/periods/status` before every journal create, post and
  reverse, and fails closed when this service is unreachable. A period nobody
  registered answers OPEN; that is deliberate and documented in
  `GetPeriodStatus`. A5 covers what this does not do.
- **ACC-07/08 replay safety.** Recognition is unique per (schedule, period),
  and GL dedups on the `schedule:period` correlation id. A reversal is unique
  per recognition instance. Termination requires an explicit final-balance
  treatment. An amendment cannot drop below the number of periods already
  recognized.
- **ACC-09.** At most one run per (rule, period). Reprocess re-posts only the
  recorded result lines of a FAILED run. Supersession keeps exactly one
  current version (partial unique index). Approval checks that drivers sum to
  100% and that every recipient account is ACTIVE.
- **ACC-10.** Book amounts come from GL's trial balance, never from the
  caller. A missing rate or a non-ASSET/LIABILITY account is refused. Posting
  is idempotent on status. A reversal is a new run that negates the recorded
  items.
- **ACC-17 validation.** Validation quarantines a batch, with a persisted
  reason, on an imbalance, a control-total mismatch, an unknown or inactive
  target account, a suspense-named target, or an open AR/AP item that already
  exists in subledger history. Create is idempotent on the source system.
  Crosswalk rows are append-only.
- **ACC-18.** Lineage edges, verifications and quarantines are append-only,
  with unique keys. `GetLineageAsOf` is a stable point-in-time read. A
  quarantine needs a reason and is still counted. The Kafka consumer records
  edges for four source event types.
- **Outbox.** Period lifecycle events, the control-exception event and the
  snapshot-integrity event are enqueued in the same transaction as their
  write (migrations 000019/000020).

## A. Not implemented, weaker than the spec, or still open

### A1. ACC-06 has no control-run lifecycle, certification or maker-checker

§9 requires:

> "Commands | StartControlRun; AssignException; RecordResolution;
> ReperformControl; CertifyControl; RejectControl"

> "State model | Planned → Running → Exception/Passed → Reperformed → Certified
> or Failed."

> "Segregation of duties | Preparer cannot certify own material control where
> maker-checker required; close role cannot waive unresolved mandatory break."

`RunSubledgerControl` is a single synchronous comparison. It appends one
`subledger_control_runs` row with status `MATCHED` or `EXCEPTION`, and that
is the whole lifecycle. There is no Planned/Running/Reperformed/Certified/
Failed state, no exception entity, and none of the commands AssignException,
RecordResolution, CertifyControl or RejectControl. The queries
ListOpenExceptions, GetCloseBlockingControls and ExplainVariance are also
absent.

On the two questions this survey was asked:

- *Is the maker-checker a real check?* For ACC-06 there is nothing to check:
  no certify step exists, so maker-checker is neither implemented nor only
  documented. It is absent.
- *Is certifying with an unresolved material break blocked?* There is no
  certification to block. What the close does enforce is that the latest run
  of each required control is `MATCHED`. That is real, but weaker than the
  spec in three ways:
  1. A break is "resolved" by running the control again until it matches. No
     resolution, owner or approval is recorded.
  2. A checklist-added control (`SUBLEDGER_CONTROL` requirement) can be
     removed by a `PERIOD_CLOSE_CONFIG` holder while its latest run is an
     `EXCEPTION`. That is a close-adjacent role waiving an unresolved
     mandatory break. Only the AR/AP baseline is protected from this.
  3. `SUBLEDGER_CONTROL_GATE_MODE=off` disables the gate for the deployment.
     The reliance manifest records it, and an unrecognised value fails closed
     to `enforce`.

Building the ACC-06 lifecycle (with financial-control-svc's
exception/certification model as the obvious reference) is the largest open
item in this register.

### A2. ACC-06 compares live subledger totals to a period balance, with no population assertion

§9 requires:

> "Required source inputs | subledger totals/populations; GL control-account
> totals; entity/book/period; cutoff watermark."

> "Failure / degradation semantics | Matching totals alone insufficient if
> population assertion fails"

> "Evidence / lineage | Freeze population/watermark, control definition
> version, totals, matching evidence, exceptions and approver."

`GetAPSubledgerTotal`, `GetARSubledgerTotal`, `GetInventoryValueTotal` and
`GetAssetNetBookValueTotal` take no period and no cutoff. They return the
subledger as it stands now, which is then compared with the trial balance for
the requested period.

Failure scenario: an October control run made in mid-November compares the
November AP subledger with the October AP control balance. It reports an
EXCEPTION that is not a break, or a MATCH that hides one.

Nothing freezes a population or a watermark, and a matching total is never
tested against a missing or duplicated item. That is negative path 1, "Totals
match but missing/duplicate items", and negative path 4, "Late item after
cutoff". The tolerance is a fixed 0.01 constant. It cannot be widened
mid-run, so negative path 2 holds trivially, but there is no materiality
policy behind it. A fix needs cutoff-scoped population endpoints in AP, AR,
assets and inventory first.

### A3. ACC-07/08 amendments mis-spread after recognition and bypass approval

§11 (ACC-08) requires:

> "Segregation of duties | Material manual schedule changes require
> independent approval."

> "Failure / degradation semantics | Schedule version change cannot rewrite
> recognized history; rounding residual handled by explicit policy."

and §10 (ACC-07) lists AmendFutureSchedule among the commands, with negative
path 2, "Accrual changed after some periods recognized".

`AmendFutureSchedule` and `ModifyPrepayment` overwrite `total_amount` and
`period_count` on an APPROVED or ACTIVE schedule, immediately. There is no
re-approval and no schedule version (no `ExplainScheduleVersion`). Each
period's amount is then recomputed from the new totals as
`total/period_count`, with the last period taking `total - base × (n-1)`.
That formula ignores what has already been recognized.

Failure scenario: 1,200 over 12 periods; 3 periods recognized at 100 each;
amended to 2,400 over 12. The remaining nine periods recognize 200 each, the
last 2,400 − 200×11 = 200. Total recognized is 300 + 1,800 = 2,100, so the
schedule never reaches its amended total.

Recognized history itself is not rewritten (the instances are append-only),
but the future spread is wrong and the change was never independently
approved. A fix needs schedule versions with a pending-amendment approval
step, and a remaining-balance spread. Left open rather than patched, because
the spread rule is an accounting-policy decision.

### A4. Designated close journals cannot actually post in SOFT_CLOSE or CLOSE_REVIEW

The kernel posting table quoted in `domain/period_state.go` gives CLOSE_REVIEW
as "Only designated close journals/workflows permitted", and
`CloseJournalsAllowed` lets ACC-07/08 recognition and termination through
SOFT_CLOSE and CLOSE_REVIEW. general-ledger-svc refuses both states for these
journals. Its period check answers `RESTRICTED` with
`soft_close_override_required` and treats `CLOSE_JOURNALS_ONLY` as hard
closed, and this service's GL client sends neither an override nor a
close-journal designation.

Failure scenario: an accrual recognized during close review is refused by GL
(503 `journal_posting_failed`). This is fail-safe, but the designated
close-journal path the state machine promises does not exist end to end.
Fixing it needs a journal designation that GL accepts in those states, which
is a general-ledger-svc change.

### A5. HardClosed is enforced at GL's API layer, not its data layer, and a posting can race the close

§2 invariant 6:

> "Hard-closed periods reject ordinary posting; reopen is explicit, scoped,
> approved and evidenced."

§17:

> "Failure / degradation semantics | Mandatory unresolved control, unposted
> material events or failed certification blocks close; ordinary postings
> denied in HardClosed."

and negative path 2, "Posting races close transition".

The ledger is general-ledger-svc's database. Its only period enforcement is
the synchronous status call before create/post/reverse, which fails closed.
There is no period guard in GL's schema, and no fence between that check and
GL's commit. A posting that read OPEN before SOFT_CLOSE could therefore
commit after it.

In practice the window is narrow. GL refuses ordinary postings from
SOFT_CLOSE onwards (A4's override rule), and hard close re-reads unposted
journals, the posting backlog and the trial balance at close time. But it is
not zero, and it is not data-layer enforcement.

This service's side is now data-layer enforced (B4). Closing the GL side
needs GL to check period state transactionally, for example a period-version
token re-checked at commit, or a local period mirror read under a row lock.
That is a general-ledger-svc change.

### A6. ACC-14 does not separate the close certifier from the preparers

§17:

> "Segregation of duties | Requester cannot unilaterally reopen hard-closed
> material book; close certifier independent from preparers where required."

The first clause is enforced (see Verified controls). The second is not.
`HardClosePeriod` and `ReclosePeriod` require `PERIOD_CLOSE_APPROVE` only, so
one principal holding both grants can soft-close, enter review and hard-close
the same period. There is no "where required" policy to switch independence
on. Reopen scope is also always the whole period: `RequestReopen` takes no
book or module scope, and §17's evidence list includes "reopen scope".

### A7. ACC-09 posts on execution with no review, and its drivers are static percentages

§12:

> "State model | Rule: Draft→Approved→Active→Superseded; Run:
> Planned→Calculated→Posted/Failed."

> "Segregation of duties | Rule author cannot approve material allocation
> where configured; run certifier separate from driver owner for high-risk
> pools."

> "Failure / degradation semantics | Driver population incomplete or allocated
> total not reconciling to source blocks posting."

> "Evidence / lineage | Persist source watermark, driver values, rule version,
> rounding and posting refs."

Rule maker-checker is now enforced (B1). `ExecuteAllocation`, however, moves
Planned → Calculated → Posted inside one request, with no run review or
certifier. Drivers are fixed percentages on the rule, not a driver
population such as headcount or square metres, so "driver population
incomplete" cannot arise and cannot be detected. The source amount is read
from the trial balance with no watermark persisted on the run.

### A8. ACC-10 rates and classification are caller-asserted

§13:

> "Required source inputs | entity/book; open monetary balances; valuation
> date; approved rate set."

> "Failure / degradation semantics | Missing/unsupported rate or ambiguous
> monetary classification blocks affected run; non-monetary items excluded
> unless policy explicitly permits."

> "Evidence / lineage | Item amount, source balance, rate source/version,
> calculation, accounts, approvals and posting refs."

The rate set is a map in the request body. There is no reference to an
approved rate set and no rate source/version, so negative path 3 ("Closing
rate amended after approval") is closed only because items are frozen at
start. Monetary classification is "account type is ASSET or LIABILITY",
which admits non-monetary assets such as inventory or PP&E. Foreign amounts
are caller-declared, not read from AR/AP/bank open balances. The commands and
queries ReprocessFailedRevaluation, GetItemCalculation, ExplainRateUsage and
ListExcludedItems are absent.

### A9. ACC-17: remaining gaps after reconciliation

§20:

> "Failure / degradation semantics | No production acceptance without
> source-to-target reconciliation; unexplained balancing plug prohibited."

> "Evidence / lineage | Source extract hashes, crosswalk, mappings,
> exceptions, opening postings, reconciliations and certificate."

> "Segregation of duties | Migration preparer cannot certify own material
> cutover; production posting still routed through ACC-04."

Source-to-target reconciliation and preparer/certifier separation are now
real (B6, B1). Still open:

- **Suspense detection is a substring check.** A "balancing plug" is
  detected only when a target account code contains "SUSPENSE". A plug booked
  to an ordinary-looking code passes, provided the batch balances and matches
  its declared control totals.
- **The source extract hash is not verified.** It is a caller-declared string
  stored as evidence.
- **Posting goes through ACC-03, not ACC-04.** The opening journal goes
  through GL's journal create/validate/post lifecycle, not an ACC-04 posting
  engine. No "control-account strategy" is resolved, and it was not verified
  whether GL's `direct_posting_restricted` accounts refuse opening AR/AP
  control lines.
- **No repeatable dry run.**
- **The ACC-17 events are not emitted**: `MigrationAccountingBatchCreated`,
  `OpeningPostingCommitted`, `MigrationReconciliationFailed` and
  `MigrationAccountingCertified` (A12).
- **Certification is not tied to a migration certificate.** It records a
  certifier and a reason only; see migration-integrity-svc A3.
- **Two concurrent creates for one source system** race on the batch's
  unique key, and the loser gets 503 instead of the existing batch.

### A10. ACC-18 lineage is one hop and has no report-fact trace

§21:

> "Purpose / bounded scope | Provide permission-aware immutable lineage from
> reported facts and snapshots back through consolidation, ledger, journals,
> accounting events and source evidence."

> "Queries / read contracts | TraceReportFact; TraceJournalToSource;
> GetLineageAsOf; VerifyLineageCompleteness"

> "Evidence / lineage | Store edge provenance, source IDs, event/journal/
> snapshot refs, version and verification state."

Every edge is `<capability record> → journal`. The capability records are
accrual recognition, prepayment recognition, allocation run, FX run,
migration batch, and the four Kafka-sourced asset/inventory/project types.
There are no report-fact, snapshot, consolidation, trial-balance, ledger-entry,
accounting-event or source-document nodes, so the chain cannot be traced end
to end. `TraceJournalToSource` returns one hop.

`TraceReportFact` does not exist. Edges carry no provenance, version or
verification state. GL manual journals and every other source domain have no
lineage. `LineageProjectionRebuilt`, `LineageGapDetected` and
`LineageVerificationFailed` are not emitted.

Permission-awareness is now real for what exists (B8). One remainder: a
trace with no edges answers 200 `[]` without an authorization check, because
there is no entity to authorize against. It reveals only that nothing is
recorded under that id in the tenant.

### A11. Command idempotency and expected_version coverage is partial

§22:

> "idempotency_key | Mandatory for I2–I4 accounting writes; request
> fingerprint retained."

> "expected_version | Required for mutable pre-posting state transitions."

Every command row of the §9–§21 wireframes carries "Idempotency-Key;
expected_version where stateful". B3 adds claims to the four creates that
mint an aggregate with no natural key. The remaining commands rely on natural
keys or status guards, so a retry after a lost response returns a 4xx
instead of the original result, or a duplicate append-only row:

| Command | Replay today |
|---|---|
| Accrual submit/approve/amend/cancel | 422 `invalid_transition` |
| Accrual recognize / reverse | original result (natural key) |
| Prepayment approve/modify/terminate | 422 |
| Prepayment recognize | original result (natural key) |
| Allocation approve/supersede | 422 |
| Allocation execute / reprocess | original run (natural key / status) |
| FX approve | 422; post: original (status) |
| Migration validate/approve/certify | 422; commit: original (status), now also retries reconciliation |
| Period soft-close / close-review | 409 `invalid_period_transition` |
| Period hard-close / reclose | 200 with current state |
| Reopen request | 409 `reopen_request_pending` |
| RunSubledgerControl | a second append-only run |
| VerifyTracePath | a second verification record |
| Mirror replay | header required but never stored |

`expected_version` is not implemented on any command. ACC-16's snapshot
creates have no claim either (outside this survey).

### A12. Most ACC-06..18 domain events are not emitted

§23 lists, among others:

> "ACC-06 | ControlExceptionRaised; ControlCertified"

> "ACC-07/08/09 | Recognition/AllocationDue; Recognition/AllocationPosted"

> "ACC-10 | RevaluationCalculated; RevaluationPosted"

> "ACC-17 | OpeningPostingCommitted; MigrationAccountingCertified"

> "ACC-18 | LineageGapDetected; LineageVerificationFailed"

The outbox carries the period lifecycle events, the subledger-control
exception and the snapshot integrity failure. No ACC-07/08/09/10/17/18
lifecycle event is emitted, so consumers named in §23 (close dashboard,
evidence, reporting) cannot react to them.

## B. Interpretations and additions (decisions a reviewer may reverse)

- **B1. Maker-checker on approval and certification is unconditional.**
  Changed in this pass. The approver may not be the creator for ACC-08
  schedules, ACC-09 rule versions, ACC-10 runs and ACC-17 batches, and may be
  neither the creator nor the submitter for ACC-07 accruals. The ACC-17
  certifier may not be the batch's preparer. It is checked in the handler
  (403 `self_approval_forbidden`, the code a self-approved reopen already
  returns), and migration 000021's `sod_*` CHECK constraints make the row
  refuse it as well. `mapPgError` maps a violation to `ErrSelfApproval`, so a
  race or a non-handler caller is not answered 503.
  - *Why this was needed:* before this, separation was by permission name
    only, and one principal holding both grants approved their own item.
  - *Interpretations:* the spec's wording is conditional ("where configured",
    "material"), and ACC-10's is "may be separated". Applying it to every
    item avoids a materiality model, as commercial-account-svc's B1 did.
    ACC-10 approval and ACC-17 approval (as distinct from certification) are
    the two places this goes beyond an explicit spec sentence.
  - *Deployment prerequisite:* the constraints are validating. A database
    with a historical self-approved row fails 000021 until that row is
    remediated.
  - *Manual check:* creator approval, submitter approval and
    preparer-certification were each refused with the named constraint, and
    an independent approval succeeded.
- **B2. One reversal per FX revaluation run.** Changed in this pass.
  `ReversePriorRevaluation` used to create a new reversal on every call, so
  two reversals could each be approved and posted, undoing the revaluation
  twice. `CreateFXReversalRun` now returns the existing reversal (200) and
  `fx_revaluation_runs_one_reversal` enforces the rule. A concurrent second
  attempt retries once and replays the winner. The prior run is the natural
  idempotency key, so this command takes no `Idempotency-Key` claim.
  - *Manual check:* a second reversal insert was refused by the index.
- **B3. command_idempotency for the four aggregate creates.** Changed in this
  pass. `CreateAccrual`, `CreatePrepayment` (claimed as
  `CreateRecognitionSchedule`, the spec's command name), `CreateAllocationRule`
  and `StartRevaluation` now require `Idempotency-Key` (400 without one).
  - *Mechanism:* the house table and rule (consolidation-svc 000008,
    financial-control-svc `BeginExecution`). The claim is per tenant, names
    the command and its one target through a typed foreign key, is recorded
    in the creating transaction, and is append-only. A replay returns the
    original resource (200). The same key with another command or another
    body is 409.
  - *Request fingerprint:* the sha256 of the decoded request re-encoded, so
    whitespace and key order do not turn a retry into a conflict.
  - *Concurrency:* concurrent first attempts are resolved on the claim's
    primary key; the store test runs eight in parallel and gets one row.
  - *What this is not:* the periodmirror package's `Idempotency-Key` is
    outbound only (sent to REF-05). This service had no inbound claim table
    before.
- **B4. fiscal_periods is guarded at the data layer.** Changed in this pass.
  A BEFORE UPDATE trigger admits only the six-state graph and keeps a
  period's identity and dates immutable. A DEFERRED constraint trigger checks
  at commit that entering AUTHORIZED_REOPEN is accompanied by an APPROVED
  request whose decision time and window equal the period's. It is deferred
  because `ApproveReopenRequest` moves the period before it marks the
  request, in one transaction.
  - *Why it matters:* this makes "recovery does not auto-open closed periods"
    hold against scripts and direct SQL, not only the API.
  - *Interpretation:* `ApplyPeriodTransition` takes a caller-chosen
    `allowedFrom`, and the guard now refuses jumps through it too. Three
    store-test fixtures that jumped OPEN → HARD_CLOSED now walk the graph
    (`advancePeriod`).
  - *Manual check:* HARD_CLOSED → OPEN and an AUTHORIZED_REOPEN with no
    request were refused; a reopen committed together with its approved
    request succeeded.
- **B5. Guarded status transitions bound their id to the wrong parameter.**
  Changed in this pass. This is an implementation correction, not policy.
  - *The bug:* the accrual, prepayment and migration-batch transition helpers
    appended `(toStatus, setClause values…, id, fromStatus)` while their SQL
    read the id from `$2` and the from-status from `$3`. Every transition
    that also sets columns therefore compared the id with a timestamp or
    principal and failed with SQLSTATE 22P02, which `mapPgError` reported as
    "fiscal period not found".
  - *Impact:* ACC-07 submit/approve/cancel, ACC-08 approve/terminate and
    ACC-17 validate/approve/post/reconcile/certify/quarantine all failed
    against a real database.
  - *Why it went unseen:* only stub-store tests exercised these paths, and
    the existing smoke tests insert rows already in their target status.
  - *Fix and test:* `transitionArgs` fixes the order; the new store tests
    walk every one of these transitions against Postgres.
- **B6. ACC-17 reconciliation reads the ledger back.** Changed in this pass.
  `CommitOpeningPosting` used to set RECONCILED straight after posting, with
  nothing compared; `ErrReconciliationMismatch` was declared but never used.
  It now reads the posted journal back from GL (`GetPostedJournal`) and
  requires: status POSTED, the batch's entity and period, and per-account
  debit and credit totals equal to the crosswalk, with no account on one side
  only.
  - *On a mismatch:* the batch moves POSTED → QUARANTINED with the first
    discrepancy as its reason, the response is 422
    `migration_reconciliation_failed` (§22's `MIGRATION_RECONCILIATION_FAILED`
    class), and certification stays impossible.
  - *If GL cannot be read:* the batch stays POSTED (503). Repeating the
    commit retries the reconciliation without posting again.
  - *Lineage:* the population now includes QUARANTINED batches that have a
    journal.
  - *Interpretation:* "source-to-target" here means crosswalk (already tied to
    the source's declared control totals at validation) against the posted
    journal. It is not checked against the trial balance, because the TB
    includes other activity in the period.
- **B7. Set-based reads and writes where this pass touched the path.** Changed
  in this pass.
  - *Lineage completeness:* `VerifyLineageCompleteness` and
    `RebuildLineageProjection` issued one edge query per posted journal of
    the entity's whole history. `ListPostedJournalLineage` classifies them
    all in one statement using two indexed `EXISTS` probes.
  - *Chart reads:* ACC-09 approval, ACC-10 calculation and ACC-17 validation
    made one GL HTTP call per driver, item or crosswalk entry; they now read
    the chart once per command (`chartIndex`). GL authorizes the listing
    exactly as the single-account read. The now-unused `GetAccountStatus` and
    `GetAccountType` client methods were removed.
  - *Inserts:* crosswalk entries, FX items and allocation drivers are written
    with one `unnest` insert instead of a round trip per row.
  - *Remaining:* the per-entry AR/AP open-item existence checks in
    `ValidateOpeningBalances`; AR and AP expose no bulk lookup.
- **B8. ACC-18 permission and rebuild corrections.** Changed in this pass.
  - *Trace and as-of:* `TraceJournalToSource` and `GetLineageAsOf` now
    require `LINEAGE_VIEW` on every legal entity in the result, not only the
    first edge's. A trace spanning an unauthorized entity is refused whole
    rather than silently filtered, because a filtered trace reads as complete
    (interpretation).
  - *VerifyTracePath:* counts only edges of the entity it is asked about.
    Before, a caller authorized for entity A could confirm, and record under
    A's name, a path in entity B. VerifyTracePath and QuarantineBrokenLineage
    now require the tenant scope at the edge, like every other handler.
  - *Rebuild:* `RebuildLineageProjection` re-verifies after recording edges
    and reports DEGRADED if gaps remain. It used to mark CURRENT
    unconditionally, which overwrote the DEGRADED a failed edge write had
    just recorded.
- **B9. Housekeeping.** The duplicated `/v1/close/workflow-refs/{ref}` route
  registration was removed. gofmt corrected three small pre-existing
  misalignments in code this pass edited.
