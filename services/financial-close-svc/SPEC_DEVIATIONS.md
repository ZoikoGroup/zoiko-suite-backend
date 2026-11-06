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

Update (2026-10-24, A13 pass): closes A13, a severe defect found while
tracing A4. Every financial-close-svc posting to general-ledger-svc was
refused. Migration 000022 is added; the scope is now migrations
000001–000022. Verification is under A13.

Update (2026-10-26, A4 pass): closes A4. general-ledger-svc migration 000018
adds `journal_headers.close_journal_designation` and the separate
`GL_CLOSE_REVIEW_POSTING` action (registered by access-control-svc migration
000019); this service now sends the designation on ACC-07/08 recognition and
termination and the existing SOFT_CLOSE override only during SOFT_CLOSE.
A5 remains open and now records the new GL column a future DB-level guard must
consult. Verification is under A4; the full plain and integration-tagged
suites were re-run against a fresh disposable `postgres:16` container.

Update (2026-11-03, A11 pass): closes A11's accrual (B10), prepayment (B11)
and allocation-rule (B12) lifecycle slices, and corrects a real bug found
while doing so — `transitionWithClaim` never refreshed the caller's struct
after a genuinely new, successful transition, only on replay (see the
correction note under B11/B12). Every claim-chain test in this pass was run
with `TEST_DATABASE_URL` pointed at a real, disposable Postgres instance so
it could not silently skip; the full `internal/store` `-tags=integration`
suite was re-run the same way afterward.

Update (2026-11-05, ACC-06 governance pass): narrows A1. The exception
lifecycle, certification/rejection, maker-checker and the three missing
queries are implemented by migration 000025; the asynchronous
Planned/Running/Reperformed state model remains open. The plain and
integration-tagged suites were re-run against a fresh embedded Postgres 16
(migrations 000001–000025 applied from empty); every package passed and the
store integration tests ran rather than skipping.

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

### A1. ACC-06 has no control-run lifecycle, certification or maker-checker — partially closed 2026-11-05

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

**Update (2026-11-05, ACC-06 governance pass).** The exception, certification,
maker-checker and query gaps are closed; the asynchronous state model is not.

- Migration 000025 adds `control_exceptions` (one companion row per run,
  OPEN → ASSIGNED → RESOLVED, resolution note mandatory) and the
  certification columns on `subledger_control_runs` (`CERTIFIED`/`FAILED`,
  certifier, timestamp and note). The run's own finding stays append-only:
  the 000004 trigger now permits only certification-column updates, and still
  refuses finding changes and deletes.
- `AssignException`, `RecordResolution`, `CertifyControl` and `RejectControl`
  are implemented. Certification refuses the run's author and the exception's
  resolver (maker-checker), refuses `CERTIFIED` while its exception is
  unresolved, and refuses a second certification. `FAILED` is the explicit
  reject path and is allowed without resolving the exception.
- `ListOpenExceptions`, `GetCloseBlockingControls` and `ExplainVariance` are
  implemented. The blocking query and close readiness share one
  classification, so they cannot disagree about whether a run blocks.
- `RemoveCloseRequirement` now refuses (422
  `control_has_unresolved_exception`) to remove a `SUBLEDGER_CONTROL`
  requirement whose latest run carries an OPEN/ASSIGNED exception, closing
  finding 2 above. The AR/AP baseline remains unremovable, and
  `SUBLEDGER_CONTROL_GATE_MODE=off` still exists (finding 3).
- Still open: `StartControlRun`/`ReperformControl` and the full
  Planned/Running/Reperformed state model are not implemented; re-running a
  control appends a new run rather than transitioning a planned one. A2
  (period/cutoff population assertion) is unchanged.

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

### A4. Designated close journals cannot actually post in SOFT_CLOSE or CLOSE_REVIEW — Closed (2026-10-26)

The kernel posting table quoted in `domain/period_state.go` gives CLOSE_REVIEW
as "Only designated close journals/workflows permitted", and
`CloseJournalsAllowed` lets ACC-07/08 recognition and termination through
SOFT_CLOSE and CLOSE_REVIEW. general-ledger-svc used to refuse both states for
these journals: its period check answered `RESTRICTED` with
`soft_close_override_required` and treated `CLOSE_JOURNALS_ONLY` as hard
closed, and this service's GL client sent neither an override nor a
close-journal designation. An accrual recognized during close review was
therefore refused by GL (503 `journal_posting_failed`) — fail-safe, but the
designated close-journal path the state machine promises did not exist end to
end.

Resolution (general-ledger-svc change, plus the client wiring here):

- GL now distinguishes `CLOSE_JOURNALS_ONLY` (`ErrCloseJournalsOnly`) from
  `CLOSED` (`ErrPeriodHardClosed`). A hard-closed period still refuses every
  journal, designated or not.
- GL migration `000018_add_close_journal_designation` adds a nullable
  `journal_headers.close_journal_designation` with a closed allowlist:
  `ACCRUAL_RECOGNITION` (ACC-07), `PREPAYMENT_RECOGNITION` and
  `PREPAYMENT_TERMINATION` (ACC-08). Every value must agree with the journal's
  ACC-03 type (all three are `ACCRUAL`); the database CHECK repeats the rule.
  The designation is journal-level evidence, not a caller-trusted permission.
- A separate `GL_CLOSE_REVIEW_POSTING` action (registered by access-control-svc
  migration `000019`) is checked whenever a designation is actually used in
  CLOSE_REVIEW, at create, validate-commit, post, approved-post, adopt and
  reprocess. The designation alone does not grant access. Registration makes
  the action authorable; it does not grant it to anyone. Deployments must grant
  it to the principal that posts close journals — this service uses the human
  request principal, and the same is true for the existing
  `GL_SOFT_CLOSE_POSTING_OVERRIDE` grant.
- This service's GL client now sends the matching designation on ACC-07
  accrual recognition, ACC-08 prepayment recognition and ACC-08 prepayment
  termination. During `SOFT_CLOSE` only, it also sends the existing
  `override_soft_close=true` + non-empty `soft_close_override_reason` pair, so
  GL's `GL_SOFT_CLOSE_POSTING_OVERRIDE` path is satisfied. CLOSE_REVIEW sends
  the designation and no override: the two mechanisms remain distinct.
- An ordinary, non-designated posting during CLOSE_REVIEW is still refused
  with HTTP 412 `period_locked` and the same `period_locked` error code as
  before. The internal sentinel changed; the external refusal did not.
- ACC-07 reversal through `ReverseGLJournal` still sends neither a designation
  nor an override. Reversal is outside the recognition/termination scope this
  A4 pass covers and remains a separate narrow path if the close policy is
  ever extended to it.

Evidence: `general-ledger-svc/internal/handler/close_review_posting_test.go`
(ordinary refusal, designated create/post, missing action, invalid and
type-mismatched designations, hard-closed refusal),
`internal/handler/posting_engine_test.go` (ordinary ACC-04 event refusal),
`internal/close/client_test.go`
(policy mapping), `internal/store/close_journal_designation_store_test.go`
(round trip, outbox evidence, DB CHECK, guarded down migration), and this
service's `internal/handler/close_journal_meta_test.go` plus
`internal/clients/gl_journal_contract_test.go` (ACC-07/08 request shape and
SOFT_CLOSE override only in SOFT_CLOSE). Both services' plain and
integration-tagged suites are green against a fresh disposable Postgres
container.

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

A4's new `journal_headers.close_journal_designation` column (GL migration
`000018`) is part of the row a future DB-level guard must consult. A guard that
knows only the period state would either refuse a legitimate designated close
journal in CLOSE_REVIEW or be too permissive; a guard that consults the
designation must still refuse every journal in CLOSED. The migration's down
migration refuses while any row carries a designation. This is a note for the
future A5 fix, not a fix: GL still has no DB-level period guard.

This service's side is now data-layer enforced (B4). Closing the GL side
needs GL to check period state transactionally, for example a period-version
token re-checked at commit, or a local period mirror read under a row lock.
That is a general-ledger-svc change.

### A6. ACC-14 does not separate the close certifier from the preparers — partially closed 2026-11-06

§17:

> "Segregation of duties | Requester cannot unilaterally reopen hard-closed
> material book; close certifier independent from preparers where required."

The first clause is enforced (see Verified controls). The second clause's
independence is now enforced too, unconditionally rather than behind a
materiality switch — the same interpretation B1 already made for every other
maker-checker gate in this service (see B1's own note on why: applying it to
every item avoids building a materiality model this spec never defines).

`HardClosePeriod` and `ReclosePeriod` now also require the certifier to differ
from the preparer — the principal who most recently performed the transition
into the state being closed from (`EnterCloseReview` for a hard close, the
reopen approval into `AUTHORIZED_REOPEN` for a reclose). `GetLatestTransitionPrincipal`
reads this from `period_state_transitions`, the same append-evidence table
`GetCloseHistory` already serves from. A period with no such transition on
record (constructed directly rather than through the tracked lifecycle, e.g.
a pre-existing or test-seeded row) has no identifiable preparer and the check
is a no-op for it — in production every period reaches CLOSE_REVIEW or
AUTHORIZED_REOPEN only through the tracked transition, so this is not a real
bypass, only a graceful default for untracked history.

Still open: Reopen scope is always the whole period — `RequestReopen` takes
no book or module scope, and §17's evidence list includes "reopen scope".
That needs a scoping model this service does not have yet (which books or
modules a reopen covers), left for a separate pass.

Evidence: `internal/handler/period_close_test.go`
(`TestHardClose_SamePrincipalAsCloseReviewPreparer_Refused`,
`TestReclose_SamePrincipalAsReopenApprover_Refused`, plus the existing
state-machine and kernel-seal tests updated to use independent principals),
`internal/store/certifier_independence_integration_test.go` (real-Postgres:
no prior transition, a tracked SOFT_CLOSE preparer, a tracked CLOSE_REVIEW
preparer, each independently addressable by `toState`).

### A7. ACC-09 posts on execution with no review, and its drivers are static percentages — partially closed 2026-11-06

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

Rule maker-checker is now enforced (B1). The run-certifier clause is now
enforced too: `ExecuteAllocation` refuses a principal who is also the rule's
`CreatedByPrincipalID` (403 `self_approval_forbidden`, reusing the same
shared sentinel and literal state model — the spec's own Run state is
Planned→Calculated→Posted/Failed with no separate Review state, so this is
enforced as a certifier/driver-owner identity check at execution rather than
by inventing a state the spec does not name). Applied unconditionally like
every other maker-checker gate in this service (B1's own interpretation),
not only for a "high-risk pool" the spec never defines how to identify.

`ExecuteAllocation` still moves Planned → Calculated → Posted inside one
request — there remains no standalone run-review step distinct from the
certifier check above. Drivers are fixed percentages on the rule, not a
driver population such as headcount or square metres, so "driver population
incomplete" cannot arise and cannot be detected. The source amount is read
from the trial balance with no watermark persisted on the run — GL's trial
balance endpoint does not expose one to capture.

Evidence: `internal/handler/handler_test.go`
(`TestExecuteAllocation_SamePrincipalAsRuleCreator_Refused`, plus the
existing execution tests updated to use an independent executor principal).

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
mint an aggregate with no natural key. B10 closes the accrual lifecycle slice
(Submit/Approve/Amend/Cancel); B11 closes the mirror prepayment slice
(Approve/Modify/Terminate); B12 closes the ACC-09 allocation rule slice
(Approve/Supersede); B13 closes ACC-10's Approve (Post already answers a
replay with the run's current state via its own status check, not a claim);
B14 closes ACC-17's Approve and Certify, and records why Validate is left
open — it has two possible terminal outcomes (VALIDATED or QUARANTINED)
that this house pattern does not yet model; B15 closes ACC-14's two simple
period transitions below (StartSoftClose, EnterCloseReview) and corrects a
stale claim this table made about HardClose/Reclose. The remaining
commands rely on natural
keys or status guards, so a retry after a lost response returns a 4xx
instead of the original result, or a duplicate append-only row:

| Command | Replay today |
|---|---|
| Accrual submit/approve/amend/cancel | original result (B10, claimed) |
| Accrual recognize / reverse | original result (natural key) |
| Prepayment approve/modify/terminate | original result (B11, claimed) |
| Prepayment recognize | original result (natural key) |
| Allocation approve/supersede | original result (B12, claimed) |
| Allocation execute / reprocess | original run (natural key / status) |
| FX approve | original result (B13, claimed); post: original (status) |
| Migration validate/approve/certify | validate: still 422 (A11 residual, see B14); approve/certify: original result (B14, claimed); commit: original (status), now also retries reconciliation |
| Period soft-close / close-review | original result (B15, claimed) |
| Period hard-close / reclose | 422 `period_already_locked` if already at the target state (corrected from this table's prior "200 with current state" claim, which did not match the code; not claimed this pass — see B15) |
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

### A13. Every journal this service posted was refused by general-ledger-svc — closed 2026-10-24

**Severity:** every ledger consequence this service owns was failing. This
covered ACC-07 accrual recognition and reversal, ACC-08 prepayment
recognition and termination settlement, ACC-09 allocation, ACC-10 FX
revaluation and ACC-17 opening balances. They failed in every period state,
OPEN included. Nothing was posted wrongly: each failure was fail-closed and
surfaced as 503 `journal_posting_failed`. Nothing was posted at all.

**Finding.** Commit d18abaa9 (2026-08-24, the §4 envelope rollout) made
general-ledger-svc enforce two contracts this service's GL client never met.

- *The envelope.* In its default `write-strict` mode GL's envelope middleware
  refuses a write (400 `envelope_incomplete`) unless it carries
  `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, `Idempotency-Key`
  and `X-Legal-Entity-Id`. `createGLJournal`, `transitionGLJournal` (validate
  and post) and `ReverseGLJournal` sent only tenant and principal.
  `compileTrialBalance`, in the same file, already sent the full set.
- *ACC-03's required journal inputs.* `CreateJournal` refuses a body without
  `journal_type`, `transaction_date`, `posting_date` and `currency_code` (400
  `missing_field`). ZS-SVC-B-001 ACC-03 makes these caller-supplied: "Required
  source inputs: entity/book; journal type; document/transaction/posting
  dates; currency; ...". Only the FX rate is server-resolved. GL enforcing
  them is correct; this service not sending them was the defect.

None of the five records that post journals carried a currency, so there was
nothing to send. The handler tests stub the GL client, so neither contract
was ever exercised.

**Resolution.**

- *One envelope helper.* `setLedgerEnvelope` sets the §4 headers on every GL
  write. `compileTrialBalance`'s inline copy became it.
- *Idempotency keys.* Each key is built from the write's business key, so a
  retried posting presents the key GL saw the first time:
  - create: `gl-journal-create:<correlation_id>`, where the correlation id is
    schedule:period, run or batch;
  - validate and post: `gl-journal-<action>:<journal_id>`;
  - reverse: `gl-journal-reverse:<correlation_id>`.
- *One body builder.* Every create body is built by
  `newGLCreateJournalRequest` from a `domain.GLJournalMeta`, so no path can
  leave ACC-03's inputs out again.
- *Journal type per path.* general-ledger-svc accepts STANDARD, ADJUSTMENT,
  ACCRUAL, REVERSAL, OPENING, CLOSING and RECLASS. The kernel standard's §8.1
  also names Allocation and Revaluation, which GL does not accept.

  | Path | `journal_type` |
  |---|---|
  | ACC-07 recognition | ACCRUAL |
  | ACC-08 recognition and termination settlement | ACCRUAL |
  | ACC-09 allocation | RECLASS (nearest to the kernel's Allocation) |
  | ACC-10 revaluation | ADJUSTMENT (nearest to the kernel's Revaluation) |
  | ACC-17 opening balances | OPENING |

- *Dates.* Every journal this service posts is a period-end consequence, so
  both dates are the period's last day. That is the registered period's
  `period_end` when the period is in the register, and otherwise the last day
  of a `YYYY-MM` name. Any other period name is refused with 422
  `posting_date_unresolvable`, never guessed.
- *Currency.* Migration 000022 adds `currency_code` to `accrual_schedules`,
  `prepayment_schedules`, `allocation_rules`, `fx_revaluation_runs` and
  `migration_batches`. For an FX run it is the book currency the adjustment
  is booked in, not an item's foreign currency.
  - All five create commands require it as a three-letter upper-case ISO 4217
    code, otherwise 400.
  - An allocation supersede keeps the rule's currency. An FX reversal
    inherits the reversed run's.
  - The column is sourced from a new service-local field because REF-02 (the
    Currency Registry) doesn't exist yet. Reconcile it against the registry
    once it ships. No functional currency exists on any legal entity
    anywhere in the platform, so there is nothing to derive it from today.
- *Data-layer guard.* Migration 000022's triggers refuse a new row without a
  currency, which also catches an internal insert that forgets to copy one.
  They also refuse changing a currency once set, because journals already
  posted in it would disagree with the record that produced them. A format
  CHECK refuses anything that is not three upper-case letters.

**Legacy rows.** A record created before migration 000022 has a NULL
currency, and the platform has nothing to backfill it from.

- Its postings are refused before any GL call, and before any state moves,
  with 422 `currency_code_missing`. They no longer reach GL as a 400 that
  surfaced as "GL unavailable".
- An ACC-09 rule acquires a currency through a governed supersede-and-approve:
  the supersede may set one only when the current version has none.
- The other four record types have no API to set one. The guard allows a
  legacy NULL to be set exactly once (an operator `UPDATE`) and never changed
  after that. A governed set-currency command for them is open.

**Not changed (noted).**

- `ErrJournalPostingFailed`'s text still reads "rejected the accrual
  recognition journal" on every path, including FX and migration.
- GL's `tenant_id` and `legal_entity_id` are uuid columns, so a non-UUID
  entity id is refused there as a store error (503). Real ids are
  registry-issued UUIDs, but this service does not validate the shape.

**Tests.**

- `internal/clients/gl_journal_contract_test.go`: create, validate, post and
  reverse each carry the envelope; the create body carries the ACC-03 fields;
  a retry presents the same Idempotency-Key.
- `internal/handler/acc_journal_shape_test.go`:
  - every path sends the right type, dates and currency;
  - a registered `period_end` wins over the calendar month;
  - an unresolvable period, or a legacy record without a currency, is
    refused before GL;
  - allocation leaves no run behind, and the migration batch stays APPROVED;
  - all five creates require a valid currency;
  - the supersede currency rules hold, and the FX reversal inherits;
  - the reversal names the legal entity.
- `internal/store/journal_currency_pg_smoke_test.go` (real Postgres):
  - the insert guard, including FX reversal and supersede without a copied
    currency;
  - the format CHECK;
  - immutability;
  - a legacy NULL can be set once, and its status transitions still work.
- `internal/handler/gl_journal_e2e_test.go` (integration tag, runs with
  `GL_E2E_URL`): this service's real handlers and GL client against a running
  general-ledger-svc binary. That means GL's real envelope middleware, handler
  and Postgres under RLS, connected as a `NOSUPERUSER NOBYPASSRLS` role.
  Authorization and close-period status were stubbed to grant and to report
  OPEN.
  - It replays the pre-fix request shape and gets 400 `envelope_incomplete`,
    then, with the envelope, 400 `missing_field`.
  - ACC-07 recognition and reversal, ACC-10 revaluation and ACC-17
    commit-and-reconcile each post FINALIZED journals carrying the expected
    type, posting date and currency. The ACC-17 case also exercises B6's
    read-back reconciliation.
  - Negative control: with the client's `journal_type` blanked, all three
    posting paths fail with 503 `journal_posting_failed`.
  - The run also found the B6 status defect (see B6).
- Existing fixtures: the existing fixtures now carry a currency. The ACC-09
  duplicate-version test now asserts `idx_allocation_rules_current_version`
  by name. Without that, the new currency guard would have made it pass for
  the wrong reason.

**Verification (2026-10-24).**

- *Database:* a fresh disposable `postgres:16-alpine` container, `a13-pg`.
- *Build and vet:* `go build ./...`, `go vet ./...` and
  `go vet -tags=integration ./...` pass.
- *Test suites:* `go test -count=1 -p 1 ./...` and
  `go test -tags=integration -count=1 -p 1 ./...` pass. The integration run
  had `GL_E2E_URL` set, and recorded 413 passes and 17 skips. The 17 skips are
  the REF-05 mirror tests' existing deliberate `t.Skip`.
- *Migrations, by hand:* migrations 000001–000022 applied from empty with
  `psql --single-transaction -v ON_ERROR_STOP=1`. 000022 down removed all
  five columns, and 000022 up applied again over the now-legacy rows.
- *Guards, by hand* (each case its own single-transaction script):

  | Case | Result |
  |---|---|
  | Insert with no currency, each of the five tables | refused |
  | Lower-case currency | refused by the format CHECK |
  | Changing a set currency | refused |
  | Changing a legacy row's currency after setting it once | refused |
  | Insert with `GBP` | accepted |
  | Legacy NULL row | accepted |
  | Legacy row status change that leaves currency alone | accepted |
  | Setting a legacy row's currency once | accepted |

- *general-ledger-svc:* its code is unchanged by this pass. Its migrations
  were applied the same way to the e2e database.

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
  requires: status FINALIZED, the batch's entity and period, and per-account
  debit and credit totals equal to the crosswalk, with no account on one side
  only.
  - *Correction (2026-10-24, A13 pass):* this pass first required status
    `POSTED`. general-ledger-svc has no such status: its posted state is
    `FINALIZED` (PENDING → VALIDATED → FINALIZED → REVERSED). Every correctly
    posted batch would have been quarantined as a mismatch. The stub GL
    reported `POSTED`, so the handler tests passed. A13's run against a real
    general-ledger-svc found it. The check now uses
    `domain.GLJournalStatusFinalized`; a REVERSED journal still does not
    reconcile.
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
- **B10. The accrual lifecycle claims an Idempotency-Key (closes A11's
  accrual row).** `SubmitAccrual`, `ApproveAccrual`, `AmendFutureSchedule`
  and `CancelFutureAccrual` now require `Idempotency-Key` and claim it in
  `command_idempotency` (the same table B3 added) in the same transaction as
  the guarded status UPDATE, via a new `transitionWithClaim` — `createWithClaim`
  applied to a command that moves an existing resource instead of minting
  one. A retry returns the schedule as it now stands (200) instead of 422
  `invalid_transition` on a transition that already happened; the same key
  reused for a different command or a different schedule is 409. The request
  hash folds in the schedule id (these commands are addressed by URL path,
  not body, so the id is the only thing distinguishing "the same request" for
  the bodyless three; Amend's hash also folds in the request body).
  - *A real bug found while doing this, not assumed:* `CancelFutureAccrual`'s
    handler-level pre-check (refuse if the schedule is COMPLETED or
    CANCELLED) read the schedule's status from the GET that preceded the
    idempotency claim lookup. On a retry of an already-succeeded cancel, that
    snapshot is CANCELLED, so the pre-check would have rejected a legitimate
    replay with 422 before the claim lookup ever ran. The store's
    `CancelAccrualSchedule` no longer trusts a caller-supplied `fromStatus` as
    its guard (the old signature took one, defeating the point of a fixed
    disallowed set — a stale `fromStatus` argument equal to the current status
    would let the UPDATE succeed from any status); it now refuses COMPLETED
    and CANCELLED unconditionally in the UPDATE's WHERE clause, and the
    handler's pre-check was removed so the claim lookup runs first.
  - *What did not change:* `AmendFutureSchedule`'s handler-level
    `would_drop_recognized_periods` check still reads the *current* count of
    recognized periods before the claim lookup. A retry made after more
    periods were recognized since the first (successful) amend can therefore
    be rejected even though the original call is replayable — a narrower,
    pre-existing-shape version of the same ordering issue, left open because
    fixing it needs the claim lookup to run before the recognized-period
    count is read, which only the store can do and `AmendAccrualSchedule`
    does not currently expose that count to its own guard.
  - *Tests:* `internal/handler/handler_test.go`
    (`TestSubmitAccrual_RetryUnderSameKeyReplays`,
    `TestSubmitAccrual_NoIdempotencyKey_Returns400`,
    `TestCancelFutureAccrual_RetryAfterSuccess_Replays`) and
    `internal/store/accrual_lifecycle_idempotency_integration_test.go`
    (replay for submit/amend/cancel, and same-key-different-schedule
    conflict), run against a real, disposable Postgres container. The
    existing `TestPgStore_TransitionsWithSetClause_ApplyAgainstRealDB` and
    `TestPgStore_SoD_SelfApprovalRefusedByDatabase_RealDB` were updated for
    the new signatures and now also prove a *second, newly-keyed* cancel
    attempt on an already-cancelled schedule is still refused (as distinct
    from a replay of the same key).
- **B11. The prepayment lifecycle claims an Idempotency-Key (closes A11's
  prepayment row), mirroring B10.** `ApprovePrepayment`, `ModifyPrepayment`
  and `TerminatePrepayment` now require `Idempotency-Key` and claim it via the
  same `transitionWithClaim`, targeting `targetPrepaymentSchedule`.
  - *Terminate's guard, not its journal path, is what changed.*
    `TerminatePrepaymentSchedule` no longer takes a caller-supplied
    `fromStatus`; like `CancelAccrualSchedule` it now refuses COMPLETED and
    TERMINATED unconditionally in the UPDATE's own WHERE clause. The
    handler's `COMPLETED/TERMINATED` pre-check (reading the GET's stale
    `sch.Status`) was removed for the same reason B10 removed
    `CancelFutureAccrual`'s: it would have rejected a legitimate replay
    before the claim lookup ran. The RECOGNIZE_REMAINING branch above the
    store call is unaffected and was not made claim-aware itself — it is
    already replay-safe without one: general-ledger-svc dedups the final
    settlement journal on the `schedule:TerminationPseudoPeriod` correlation
    id, and `CreatePrepaymentRecognition` dedups the evidence row on its own
    `UNIQUE(schedule_id, fiscal_period)` constraint, so a retried Terminate
    re-reads both instead of duplicating either, exactly as the handler's own
    pre-existing doc comment on that branch describes.
  - *Tests:* `internal/handler/handler_test.go`
    (`TestApprovePrepayment_RetryUnderSameKeyReplays`,
    `TestTerminatePrepayment_RetryAfterSuccess_Replays`) and
    `internal/store/prepayment_lifecycle_idempotency_integration_test.go`
    (replay for approve/modify/terminate, and a freshly-keyed terminate
    attempt on an already-terminated schedule still refused), run against a
    real, disposable Postgres container. The existing
    `TestPgStore_SoD_SelfApprovalRefusedByDatabase_RealDB` and
    `TestPgStore_TransitionsWithSetClause_ApplyAgainstRealDB` were updated
    for the new signatures.

- **Correction to B10/B11 found while adding B12 (`transitionWithClaim` never
  refreshed the caller's struct on a genuinely new, successful transition).**
  `transitionWithClaim` originally just called `createWithClaim`.
  `createWithClaim`'s own non-replay path never calls its `load` callback —
  correct for a create, whose caller-supplied struct already holds exactly
  what it is about to insert — but wrong for a transition, whose
  caller-supplied struct reflects the resource's state *before* the
  transition. The result: a genuinely new (non-replay) Submit, Approve,
  Amend or Cancel left the handler's in-memory schedule exactly as the GET
  had returned it — e.g. still `DRAFT` after a successful Submit — so a real
  client's first call to any of these got back a response whose status had
  not moved, even though the row in the database had. Only a *replay* (the
  second call under the same key) ever returned the correct, current state,
  because only the replay path calls `load`.
  - *Why the stub-backed handler tests never caught it.* Every stub
    implementation in `handler_test.go` explicitly copies the updated row
    back into the caller's struct on its own first-success path (`*sch =
    *s.schedules[...]`), independently of the claim/replay machinery. That
    was written to match the *intended* behavior, which happened to diverge
    from what `PgStore` actually did — a test double drifting from the real
    implementation it stands in for.
  - *How it surfaced.* Running the `openTestPool`-based integration suites
    (`acc_controls_hardening_pg_smoke_test.go`,
    `accrual_lifecycle_idempotency_integration_test.go`, and this pass's own
    new tests) against a real, disposable Postgres with `TEST_DATABASE_URL`
    exported — not the `tenant_isolation_test.go` `TestMain`, which starts
    its own embedded Postgres for a *different* set of tests and does not
    export `TEST_DATABASE_URL`, so every `openTestPool`-based test silently
    skips unless the variable is set from outside the test binary. The B10
    and B11 commits had only ever been verified with it unset, so this
    defect shipped in both without a real-database test ever having run
    against the new code paths.
  - *The fix.* `transitionWithClaim` is no longer a one-line call to
    `createWithClaim`; it has its own full attempt/retry loop that calls
    `load(tx, targetID)` once more, in the same transaction, immediately
    after a successful `perform` and claim insert — so both the replay path
    and the genuinely-new-success path now leave the caller's struct current.
    `createWithClaim` itself is untouched: the four aggregate creates (B3)
    never needed this and still don't.
  - *Verification.* Every B10/B11/B12 test was re-run with
    `TEST_DATABASE_URL` pointed at a real, disposable Postgres instance (not
    skipped), and the whole `internal/store` package's `-tags=integration`
    suite was run the same way afterward — all pass. This is now the
    standard this register holds every future `A11` pass to: a claim-chain
    test that only ever ran skipped is not verification.

- **B12. The ACC-09 allocation rule lifecycle claims an Idempotency-Key
  (closes A11's allocation row).** `ApproveAllocationRule` and
  `SupersedeAllocationRule` now require `Idempotency-Key`.
  - *Two different claim shapes, same table.* `ApproveAllocationRule` is a
    transition — it claims the EXISTING `rule_version_id` via
    `transitionWithClaim`, the same shape B10/B11 use. `SupersedeAllocationRule`
    is not: it mints a brand-new `rule_version_id` on every call (the handler
    generates a fresh UUID even on a retry), so it uses `createWithClaim`
    directly, exactly like `CreateAllocationRule` (B3) — the claim's target is
    the *new* version's id, and `requestHash` is computed from the request
    body plus the path's `rule_id` (never from the freshly-minted version id,
    which would make a retry's own fresh id fail to match its first attempt's
    hash). Both claim `targetAllocationRule` (`allocation_rule_version_id`);
    the `command` column disambiguates which of the two (or `CreateAllocationRule`)
    a given key was used for.
  - *Tests:* `internal/handler/handler_test.go`
    (`TestApproveAllocationRule_RetryUnderSameKeyReplays`,
    `TestSupersedeAllocationRule_RetryUnderSameKeyReplays`, the latter also
    proving a retry mints no second version and a genuinely new key against
    an already-superseded rule is refused) and
    `internal/store/allocation_rule_idempotency_integration_test.go` (replay
    for both commands, and the same already-superseded refusal), run against
    a real, disposable Postgres container per the verification note above.
- **B13. ACC-10's Approve claims an Idempotency-Key; Post is deliberately
  left as it was (closes A11's FX row).** `ApproveFXRevaluationRun` is a
  transition, claimed via `transitionWithClaim` exactly like B10/B11/B12's
  approve commands, targeting `targetFXRevaluationRun`. `PostRevaluation`
  is NOT given a claim: its own handler-level check — a run already POSTED
  returns its current state untouched, before any journal-posting work runs
  — already gives a retry the original result without a second journal,
  which is the same outcome a claim would produce, and predates this A11
  pass (it is the spec's own negative path, "Revaluation replay duplicates
  journal"). Adding `command_idempotency` on top would duplicate, not
  strengthen, a guard that already works correctly.
  - *Tests:* `internal/handler/handler_test.go`
    (`TestApproveRevaluation_RetryUnderSameKeyReplays`) and
    `internal/store/fx_revaluation_idempotency_integration_test.go` (replay
    for Approve), run against a real, disposable Postgres container per the
    verification note above.
- **B14. ACC-17's Approve and Certify claim an Idempotency-Key; Commit and
  Validate are deliberately not given one (closes A11's migration row for
  two of its three named commands).**
  - *Schema.* `command_idempotency` had no column for a migration batch.
    Migration `000023` adds `migration_batch_id` (a real FK to
    `migration_batches`), widens `chk_command_idempotency_one_target` to the
    five columns, and adds the matching lookup index — the same shape B3's
    four columns already have. The down migration refuses while any claim
    names a batch, same posture as every other `command_idempotency` change
    in this register.
  - *Approve and Certify.* Both are plain transitions —
    `ApproveMigrationBatch` (VALIDATED → APPROVED) and `CertifyMigrationBatch`
    (RECONCILED → CERTIFIED) — claimed via `transitionWithClaim` exactly like
    B10-B13's approve commands, targeting the new `targetMigrationBatch`.
    `loadMigrationBatch` is the tx-scoped counterpart of `GetMigrationBatch`
    (batch header plus crosswalk entries), used as the load callback so a
    replay — and, per the B10/B11/B12/B13 correction note, a genuinely new
    success too — carries the entries, not just the header.
  - *Commit is unaffected.* `CommitOpeningPosting`'s own
    RECONCILED/CERTIFIED/POSTED switch already answers a retry with the
    batch's current state before doing any journal-posting work, predating
    this pass (the spec's own negative path, "Commit repeated after
    timeout"). Adding a claim on top would duplicate, not strengthen, a
    guard that already works.
  - *Validate is NOT claimed — left open.* Unlike every other command this
    A11 pass closes, `ValidateOpeningBalances` does not have one fixed
    success transition: it ends in VALIDATED (`MarkMigrationBatchValidated`)
    or, on any of several validation failures, QUARANTINED
    (`QuarantineMigrationBatch`), and both are reached only after real
    external-dependency work (a chart-of-accounts read, and an AR/AP
    existence check per open-item crosswalk entry). `transitionWithClaim`
    and `createWithClaim` both assume one fixed target transition decided
    before the claim is recorded; Validate would need the claim lookup to
    gate entry to that external work BEFORE either branch runs, then record
    the claim against whichever of the two transitions actually happened —
    a different shape from every command this register has closed so far,
    and not one to retrofit without its own design pass. Today a retry of
    Validate still 422s regardless of which terminal state the first
    attempt reached, exactly as before this pass; the only change is that it
    is now a documented, scoped gap rather than an unexamined one.
  - *Tests:* `internal/handler/handler_test.go`
    (`TestApproveMigrationBatch_RetryUnderSameKeyReplays`,
    `TestCertifyMigrationBatch_RetryUnderSameKeyReplays`) and
    `internal/store/migration_batch_idempotency_integration_test.go` (replay
    for both commands, the latter also proving the crosswalk entries survive
    a replay), run against a real, disposable Postgres container per the
    verification note above. Migration `000023`'s down/up round-trip was
    also checked by hand against the same running instance, per this
    register's established convention for a schema change (A4, B4).
- **B15. ACC-14's two simple period transitions claim an Idempotency-Key;
  HardClose/Reclose/Reopen are deliberately not given one, and this table's
  prior claim about HardClose/Reclose was wrong (closes A11's period row
  for StartSoftClose/EnterCloseReview).**
  - *Schema.* Migration `000024` adds `fiscal_period_id` to
    `command_idempotency`, widens `chk_command_idempotency_one_target` to
    six columns, and adds the matching lookup index — same shape as B14's
    `000023` for migration batches.
  - *Why only two of the period commands.* `ApplyPeriodTransition` is the
    one function behind every period transition (soft-close, close-review,
    hard-close, reclose, and the reopen commands), but StartSoftClose and
    EnterCloseReview are the only callers that do nothing beyond that one
    guarded UPDATE. `HardClosePeriod`/`ReclosePeriod` (`closeWithEvidence`)
    run readiness checks, seal the period in the accounting kernel, and
    build and sign an evidence manifest before ever calling it; the reopen
    commands have their own request/approval object. Wrapping either in a
    claim is a separate, larger design question (where does the claim lookup
    sit relative to a kernel seal that already happened?), not a drop-in
    extension of this pass's pattern — so this closes only the two commands
    that fit the house shape.
  - *The new store methods.* `StartSoftCloseTransition` and
    `EnterCloseReviewTransition` each call a shared, unexported
    `periodTransitionWithClaim` — `transitionWithClaim` wrapping the
    existing `applyPeriodTransition` — with their own fixed `allowedFrom`
    and target state (`u.To` is set inside the method, not by the caller:
    the whole point of having two methods instead of one parameterised by
    target state). On a refusal (`ErrInvalidPeriodTransition`), the period
    as it actually stands is still returned, matching
    `ApplyPeriodTransition`'s own existing contract, so
    `writeTransitionErr`'s "the period is X, this needs Y" message needs no
    second read.
  - *The handler.* `simpleTransition` now takes the claimed store method as
    a function value (`apply`) instead of the `to` state string, since each
    of the two store methods already knows its own target; this avoids
    giving the `handler` package a dependency on the `store` package's
    exported command-name constants, which nothing else in this codebase
    does either.
  - *The stale claim corrected.* This table previously said "Period
    hard-close / reclose | 200 with current state". Reading
    `closeWithEvidence` while scoping this pass found that is not what the
    code does: a hard-close/reclose attempt against a period already at the
    target state answers 422 `period_already_locked`, not 200. The table
    now says what the code actually does; the behavior itself is unchanged
    by this pass.
  - *Tests:* `internal/handler/period_close_test.go`
    (`TestStartSoftClose_RetryUnderSameKeyReplays`,
    `TestEnterCloseReview_RetryUnderSameKeyReplays`, the former also proving
    a freshly-keyed soft-close attempt on an already-SOFT_CLOSE period is
    still refused) and
    `internal/store/period_transition_idempotency_integration_test.go`
    (replay for both commands; the refusal case also proves the current
    period is still returned on a real refusal, not just a replay), run
    against a real, disposable Postgres container per the verification note
    above. Migration `000024`'s down/up round-trip was also checked by hand
    against the same running instance.
