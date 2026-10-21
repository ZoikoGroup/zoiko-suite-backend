# Spec deviations — consolidation-svc (ACC-12, ACC-13)

Governing document: `ZoikoSuite_Accounting_Record_to_Report_Detailed_Service_Specifications.docx`,
§15 "ACC-12 — Elimination & Consolidation Adjustments" and §16 "ACC-13 —
Consolidation" (one pipeline), plus §1 "Consolidation is book-separated —
ACC-12 top-side/elimination adjustments post only to consolidation books and
never rewrite entity statutory ledgers", the §2 invariant "Consolidation-only
entries never contaminate entity statutory books", the authority table (ACC-12
"Must never own: Entity statutory ledgers"; ACC-13 "Must never own: Source
entity balances"), negative tests 23–25 of §27, prohibited pattern 12
"Consolidation adjustment posted to statutory entity book", and the IFRS 10
note ("effective-dated group/control relationships … rather than entity-ledger
mutation"). First survey of this service against the spec (2026-10-17). The
platform completion tracker's "Not Started" row is stale and was ignored.

Note: ACC-13's contract table names seven commands — StartConsolidation,
LockInputs, TranslateEntities, ApplyEliminations, **ApplyAdjustments,
CertifyConsolidation, SupersedeConsolidation** — while its wireframe shows
only the first four. All seven are surveyed here.

Overall: the ACC-12 adjustment lifecycle is largely built (create → approve →
post → reverse, with maker-checker, a status guard on every transition, and
reversal through ACC-04). ACC-13 is one synchronous call that sums live GL
journals per account, subtracts matched intercompany journals and writes
signed snapshots; none of the run's governance (input lock, translation,
ownership, review, certification, supersession) exists. Most importantly, the
adjustment posting path writes into the group entity's own ledger book (A1).
Roughly 55% of ACC-12 and 20% of ACC-13; about 35% overall.

**Book-separation boundary: VIOLATED in code.** `PostConsolidationAdjustment`
posts to general-ledger-svc as an ordinary entity journal — see A1. Today the
call fails before anything is written, because general-ledger-svc rejects the
request body (400), so nothing reaches a ledger through this path at present.
That is an accident of contract drift, not a control. Between 2026-09-07
(`b5a2b984`, posting path added) and 2026-09-30 (`ddfa4015`, GL started
requiring `transaction_currency`/`document_date`) the body was accepted, so
any environment that posted an adjustment in that window may hold
consolidation entries in a group entity's statutory ledger.

## A. Not implemented / weaker than the spec

- **A1. Consolidation adjustments post into the group entity's statutory
  ledger (authority-boundary violation).** Code path:
  `handler.PostConsolidationAdjustment` → `clients.PostConsolidationAdjustmentJournal`
  → `POST {GL}/v1/postings/events` with `legal_entity_id =
  group_legal_entity_id` and **no book identifier**.
  general-ledger-svc's `PostAccountingEventRequest` has no `book_id` field;
  the journal is created with `book_id` NULL and its ledger entries with
  `book_id = ''` — the same scope as every other journal of that legal
  entity. The "group legal entity" is a real legal entity (typically the
  parent company), so elimination and top-side lines land in its statutory
  general ledger, trial balance and close. The store-level guard
  `GroupEntityHasRun`, named for the negative path "Adjustment targets
  statutory book", does not separate books: it only checks that the entity was
  once named as a run's group, and a run row is written before any input is
  fetched, so starting a run (even one that fails) for any entity the caller
  is authorized on makes that entity a valid target. Knock-on effect:
  `FetchTrialBalance` reads every FINALIZED journal of an entity with no book
  filter, so when the parent is also one of the run's children (the normal
  case) a posted adjustment would come back as entity input and be counted
  twice.

  *Why it was not fixed here:* the fix needs a decision and work outside this
  service. There is no consolidation book on the platform:
  general-ledger-svc's envelope keeps `book_id` NotRequired because "REF-06
  Accounting Book / Ledger Basis" does not exist, so no book can be issued or
  validated. Options for whoever scopes it: (a) a governed consolidation book
  per group in ACC-04/05 (add `book_id` to `PostAccountingEventRequest`,
  require it for a consolidation source, exclude consolidation books from
  entity TB, close and `GET /v1/journals` by default); or (b) keep
  consolidation entries entirely inside this service as its own append-only
  consolidation journal, never posted to general-ledger-svc, applied by
  ApplyAdjustments (A8). Either way, until then `PostConsolidationAdjustment`
  should be disabled (fail closed) rather than repaired. **Do not "fix" the
  current 400 by adding `transaction_currency`/`document_date` to the request:
  that would switch the violation on.** Also needed: a test that an adjustment
  can never target a book that is an entity's statutory book (§27 test 23),
  and a check of every environment for journals with `source_event_id` equal
  to a `consolidation_adjustment_id` (the 2026-09 window above).
- **A2. Elimination ignores the run's scope.** `StartRun` fetches every
  eliminable (MATCHED or CLOSED, B6) intercompany entry in the **tenant** (no filter by the run's child
  entities, group or fiscal period) and, for each, subtracts every line of
  both the source and target journals from the group balances. Entries between
  entities outside this group, or from other periods, are subtracted from
  balances they never contributed to. Subtracting whole journals also removes
  any non-intercompany lines those journals carry. An entry whose journals
  cannot be fetched becomes a run exception, but the run still completes
  with a partially eliminated balance. Scope: filter to pairs whose both
  entities are in the run and whose period is the run's period (needs a
  period on the pair — intercompany-accounting-svc A4), eliminate the
  intercompany lines only, and record each elimination as an elimination row
  linked to the pair (so ExplainElimination is answerable).
- **A3. No currency translation (`TranslateEntities`, `ExplainTranslation`).**
  Child balances are summed in whatever currency each journal was posted in,
  and the result is labelled with the request's `target_currency`. A group
  with a GBP and a USD subsidiary produces a meaningless total. No FX
  translation policy, rate source, rate version, CTA or per-entity translated
  balance exists. Blocked on the platform FX-rate read capability (the same
  blocker as financial-control-svc A8 and ACC-10).
- **A4. Inputs are live journals, not certified snapshots, and are never
  locked (`LockInputs`).** §16 requires "certified entity TB/snapshots" and
  "Exact input snapshots … and output hashes"; negative path "Input TB changes
  after lock" must be blocked. `FetchTrialBalance` lists FINALIZED journals
  from general-ledger-svc at run time and adds up their lines. Nothing checks
  that the entity's period is closed or its TB certified (ACC-14/15/16), no
  input watermark or hash is recorded, and two runs a minute apart can differ
  silently. Missing-entity detection is only "the GL call failed". Scope:
  read ACC-15/16 snapshot ids per child, refuse uncertified inputs
  (`CONSOLIDATION_INPUT_UNCERTIFIED`), store snapshot id and hash per input.
- **A5. The elimination-bound check on proposals is tenant-wide.**
  `CreateEliminationProposal` compares an ELIMINATION proposal against the sum
  of **all** eliminable (MATCHED or CLOSED, B6) intercompany amounts in the tenant, regardless of group,
  period or entity pair, and sums per-line the larger of debit/credit (so a
  balanced two-line 100 proposal counts as 200). The negative path
  "Elimination exceeds matched reciprocal balance" is therefore not
  meaningfully enforced. Depends on intercompany-accounting-svc A1
  (`GetReciprocalBalances`) and A4 (period on the pair); the proposal also
  needs to name which pair or entity pair it eliminates.
- **A6. Run state model and governance commands are absent.** Spec: "Draft →
  CollectingInputs → Locked → Processing → Review → Certified/Superseded".
  Built: RUNNING → COMPLETED / FAILED, all inside one HTTP request.
  `CertifyConsolidation` and `SupersedeConsolidation` do not exist, so there is
  no review step, no maker-checker ("Entity preparers cannot certify group run
  alone"), and none of the certification blockers in §16 (uncertified/missing
  inputs, unresolved elimination blockers, ownership ambiguity, "Elimination
  run incomplete but certification attempted"). A run with elimination
  exceptions is still COMPLETED. Events ConsolidationInputsLocked,
  ConsolidationCertified and ConsolidationSuperseded are therefore never
  emitted (started, completed and exception-detected are). This is the
  largest piece of ACC-13 work and should be scoped as its own task; it
  depends on A3/A4/A7.
- **A7. No ownership, NCI or effective-dated group structure.** The caller
  supplies `child_legal_entity_ids` on each run; there is no consolidation
  method, ownership percentage, NCI, or as-of group membership, so "Ownership
  changes mid-period" and "Ownership effective date ambiguous" (§27 test 26)
  cannot be detected. The spec lists the ownership/NCI data source as an open
  decision ("Group Finance + Organization domain", §29), so this is blocked
  on that decision; tenant-entity-registry-svc's hierarchies are the obvious
  first source for membership.
- **A8. `ApplyAdjustments` is not implemented.** No run reads
  `consolidation_adjustments`. A POSTED adjustment only affects a run
  indirectly, if it reached the GL (A1) and the group entity happens to be one
  of the children. Once A1 is decided, a run must apply POSTED, unreversed
  adjustments for its group/period explicitly and record which ones.
- **A9. ACC-12's adjustment events are emitted. — Closed (B7).**
  ConsolidationAdjustmentCreated, ConsolidationAdjustmentApproved,
  ConsolidationAdjustmentPosted and ConsolidationAdjustmentReversed are
  enqueued to the transactional outbox in the transaction that makes the
  adjustment state change, with the adjustment itself as the aggregate and
  the exact spec name preserved as the legacy `event_type`. Downstream
  consumers (ACC-15/16/18, reporting) can now see adjustments.
- **A10. `ExplainElimination` and `GetAdjustmentHistory` are not
  implemented.** History is only the latest set of approve/post/reverse
  columns on the adjustment row; there is no transition log. There is no
  elimination record at all for the automatic eliminations a run performs
  (they exist only as arithmetic on the snapshot), so "why was this amount
  eliminated" has no answer. `ListEliminations` is approximated by
  `GET /v1/consolidation/adjustments` (all types, no type filter). Scope:
  `consolidation_adjustment_transitions` table; per-run `eliminations` rows
  from A2.
- **A11. `GetGroupBalance` and `DrillEntityContribution` are partial.**
  Group balances are readable per run (`/runs/{id}/snapshots`), not by group
  and period ("the certified balance for group G, period P"). Contributions
  (`/runs/{id}/contributions`) are pre-elimination gross amounts per child; no
  translated amount, elimination or adjustment is attributed back to an entity.
- **A12. Snapshot signature is keyed with the tenant id.** Each snapshot's
  HMAC-SHA256 uses the tenant id as the key. Tenant ids are not secret, so
  anyone can recompute a "valid" signature for altered figures; it provides
  no integrity evidence. The payload also formats the balance with `%f`.
  Scope: a managed signing key (the platform's open "Snapshot signature/key
  profile" decision, §29) or drop the claim and rely on ACC-16.
- **A13. List endpoints skip authorization when unfiltered.** `GET
  /v1/consolidation/runs` and `GET /v1/consolidation/adjustments` check
  authorization only when `group_legal_entity_id` is supplied; without it they
  return every run or adjustment in the tenant to any caller with an
  `X-Principal-Id`. No other service calls these endpoints (checked across
  the repository), so requiring the filter is safe, but it is an API contract
  change and is left for an explicit decision.
- **A14. No `Idempotency-Key`/`expected_version`, and `StartRun` is not
  idempotent.** A retried `POST /v1/consolidation/runs` creates a second run.
  Adjustment commands rely on status guards only (same as
  intercompany-accounting-svc A10). If the final `CompleteRun` write fails,
  the handler logs it and still answers 201 COMPLETED while the row stays
  RUNNING.
- **A15. No `Draft` adjustment state.** `CreateEliminationProposal` lands
  directly in PENDING_APPROVAL (documented in migration 000003; no Submit
  command is named). `Superseded` is represented as REVERSED with
  `superseded_by_adjustment_id` set, not a separate state.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. A superseding adjustment must actually replace the reversed one**
  (changed in this pass). After a snapshot exists, reversal requires naming a
  superseding adjustment (negative path "Reverse after snapshot without
  supersession"). Previously any existing adjustment id satisfied that,
  including the adjustment being reversed, one for another group or period, or
  one already reversed. It must now be a different, unreversed adjustment for
  the same group and fiscal period (422 `invalid_supersession` otherwise),
  checked before the GL reversal call. Covered by a table test of all four
  cases.
- **B2. Maker-checker is unconditional for approval.** The creator can never
  approve (§15 limits this to "protected top-side adjustment"; there is no
  notion of protected, so every adjustment is treated as one). Posting and
  reversal have no SoD rule.
- **B3. Reversal goes through ACC-04's `CreateReversalPosting`.** Correct for
  history preservation, but it inherits A1: the reversal is also an entity
  ledger journal.
- **B4. Routes and action names are not the wireframe's.** REST routes under
  `/v1/consolidation/...` instead of `/internal/v1/acc12|acc13/...`; actions
  `CONSOLIDATION_RUN_*` / `CONSOLIDATION_ADJUSTMENT_*` instead of
  `consolidation.adjust.manage/approve/post` and
  `consolidation.run/review/certify`. Same platform convention as the other
  ACC services.
- **B5. Control population.** `GET /v1/control-populations/balance-contributions`
  serves financial-control-svc (not part of the named ACC-12/13 surface).
- **B6. CLOSED intercompany pairs are eliminable, and their amounts are
  exact decimals** (changed in this pass). This is the consolidation-side
  blocker that intercompany-accounting-svc's A9 flagged in its own **B8**.
  - *The coupling.* intercompany-accounting-svc added `CLOSED`, a terminal
    state reachable only from `MATCHED`, for "the period this pair belongs to
    has been consolidated". `FetchMatchedIntercompanyEntries` kept
    `match_status == "MATCHED"` only, so a closed pair dropped out. A
    re-run for an already-consolidated period (a restatement, an audit
    adjustment) would then have treated the pair as never matched and left
    it uneliminated, overstating group balances. The function is now
    `FetchEliminableIntercompanyEntries` and keeps MATCHED **or** CLOSED.
    UNMATCHED, AWAITING_COUNTERPARTY, MISMATCH, DISPUTED and RESOLVED are
    still never eliminated. RESOLVED stays excluded until this service reads
    intercompany-accounting-svc's `resolution_outcome`. Both callers
    (`StartRun` elimination and the `CreateEliminationProposal` bound) use
    the new set. `TestFetchEliminableIntercompanyEntries_MatchedAndClosedOnly`
    covers it against an HTTP double with one pair in every status.
  - *Decimal amounts.* `clients.IntercompanyEntry.Amount` was `float64`
    (ZS-DATA-001 D06). It is now `domain.Decimal`: exact decimal text using
    financial-statements-svc's `ParseAmount`/`SumAmounts`/`CanonicalAmount`
    pattern (`internal/domain/decimal.go`), the same type
    intercompany-accounting-svc uses. It decodes a JSON number as well as a
    quoted string, because intercompany-accounting-svc still writes `amount`
    as a JSON number literal of the exact digits. A plain `string` field
    would refuse that. A malformed amount fails the fetch rather than
    counting as zero. The matched-balance bound in `CreateEliminationProposal`
    is summed as `big.Rat`. The proposal's own line amounts are still
    `float64` in this service's domain; each is read as its shortest decimal
    form before the exact comparison. A5's per-line semantics are unchanged.
  - *Still float64, not part of this change:* general-ledger journal lines
    and trial balances (`FetchTrialBalance`, `FetchJournalLines`), the
    consolidated balances and snapshots built from them, and
    `ConsolidationAdjustmentLine` amounts. Elimination itself never read
    `IntercompanyEntry.Amount`; it reverses the journals' real posted lines.
- **B7. Adjustment event shape (A9).** Each lifecycle command emits one event
  in the transaction that already made the change:
  `com.zoikosuite.accounting.consolidation-adjustment.created|approved|posted|reversed`,
  with `ConsolidationAdjustmentCreated|Approved|Posted|Reversed` as the
  legacy `event_type` (the spec's "Events produced" names, verbatim). The
  aggregate is the adjustment itself (`subject =
  urn:zoikosuite:consolidation-adjustment:<id>`) and `legalentityid` is the
  group legal entity, mirroring the run events' envelope shape. The payload
  carries the adjustment id, tenant, group, period, status, the real
  transition timestamp and actor; Posted adds the consolidation-book journal
  id, and Reversed adds the reason and any superseding adjustment id. A
  rejected transition (wrong status, unknown id) never reaches the event:
  the outbox cannot report a change the transaction rolled back.
