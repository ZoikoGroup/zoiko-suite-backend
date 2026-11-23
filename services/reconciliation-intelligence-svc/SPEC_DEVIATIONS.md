# Spec deviations — reconciliation-intelligence-svc (advisory reconciliation suggestions)

**No dedicated specification exists for this service.** The service-ownership
table describes it as "ReconciliationSuggestion (advisory), consuming
bank-reconciliation" (Doc03 §15.4); `docs/architecture/04-data-model.md` §16
defines `ReconciliationSuggestion` (`reconciliation_suggestion_id`,
`legal_entity_id`, `source_record_a`, `source_record_b`, `confidence_score`,
`suggestion_status`, `generated_at`) with the rules "intelligence outputs are
explainable artifacts", "must preserve model reference and score basis", "does
not mutate source truth". The binding rules on what it may do come from
ZS-CONTROL-001 (§2 "AI may classify/prioritize; it cannot certify, waive, post
or clear a material financial exception", §10 "AI-assisted suggestion: may
propose candidate matches; deterministic/human-authorized action establishes
the match", Invariant 18, scenario 27) and ZS-SVC-Z-001 INV-28.

What the code claims (`internal/domain/types.go`): a heuristic, not a model;
every output is `RECOMMENDED`; a human decides through `ApplyResolution`;
tolerances come from financial-control-svc's governed policy registry and the
service refuses to analyze without them.

Overall: the advisory claim holds — the service writes only its own tables
and never posts, waives or clears anything — and the tolerance gate is real.
It now consumes bank-reconciliation (A1), detects duplicate entries within
a run (A2), and proposes the book side of an UNMATCHED line: a FINALIZED
general-ledger-svc journal, as an advisory AUTO_MATCH or DATE_SKEW (A2a,
2026-10-26) — or, when no journal matches, a NORMALIZED banking-connector-svc
canonical transaction (A2b part 1, 2026-10-28). Two defects were fixed in
this pass (B1, B2); a later pass closed A7 and the decimal half of A4; a
third closed A1, A3 and A6; a fourth closed A8 and A9; a fifth closed A5; a
sixth closed A2a; a seventh closed A2b part 1; an eighth closed A2b part 4
(general-ledger-svc's QueryLedger keyset cursor, A15, and this service's
internal/ledger client following it); a ninth closed A2b part 2
(two-journal matches, requiring bank-reconciliation-svc's own A2 to gain
many-to-one support first); the latest closes A2b part 3 (2026-11-01),
requiring bank-reconciliation-svc to gain a lookup-by-journal-id capability
first — A2b is now closed in full.

## A. Not implemented / weaker than the spec

- **A1. It does not consume bank-reconciliation** (closed). Transactions used
  to be pushed in the `POST /v1/reconciliations/analyze` body by the caller,
  so suggestions were about whatever the caller sent. The body is now
  `{legal_entity_id, job_name, bank_reconciliation_run_id}` (unknown fields,
  including the old `transactions_a`/`transactions_b`, are 400).
  `internal/bankrec` reads the run (`GET /v1/reconciliation-runs/{id}`), then
  its UNMATCHED lines (`GET /v1/statement-lines?bank_account_id=&
  statement_date=&status=UNMATCHED`) — the same `(bank_account_id,
  statement_date)` scope bank-reconciliation-svc's `FreezePopulation`
  selects. `GET …/population` is not used: it returns the population's
  header, not its lines. The job stores `bank_reconciliation_run_id`; every
  suggestion stores `bank_reconciliation_run_id` and the real
  `statement_line_id` (migration 000003), and the resolution event carries
  both. The read fails closed: a run of another legal entity is 403, a
  superseded run 409, and a line outside the run's scope, not UNMATCHED,
  duplicated, or with an amount that cannot be held exactly is 502 — as is
  bank-reconciliation-svc refusing the request (a 4xx is reported as this
  request's defect, not an outage). Read-only by construction: the client
  has no write method, and bank-reconciliation-svc's own propose/confirm/
  reject-match flow remains the only way a match is established.
  *Limits:* bank-reconciliation-svc's list route has no cursor and caps a
  page at 1000, so a run with 1000+ UNMATCHED lines is refused (422) rather
  than analyzed partially; lifting that needs a paged population route
  there. Suggestions are about the run's *current* UNMATCHED lines, not the
  frozen population snapshot, because no route exposes the snapshot's lines.
  Pre-000003 jobs keep NULL in the new columns — there is no source record
  to backfill.
- **A2. It never proposes a candidate match** (narrowed — DUPLICATE_ENTRY
  done; the rest is A2a). Of the three declared-but-never-produced types,
  `DUPLICATE_ENTRY` is now produced: it needs only the UNMATCHED population
  A1 already reads. Within one run, two lines are probable duplicates when
  they have exactly the same amount (sign included, `big.Rat`), the same
  `currency_code`, and bank references that are equal after upper-casing
  and removing whitespace, or where one is the other plus one trailing
  sequence suffix (`-1`, `_2`, `/3`, `.4`, `#5` or `(2)`; 1-2 digits). Two
  *different* suffixes on one stem (`PAY-1` / `PAY-2`) are not a match:
  that is how distinct recurring payments are numbered. An empty reference
  never matches. Same bank account and statement date need no check — a
  run is exactly one of each — so an entry duplicated across two statement
  dates is in two runs and is not detected. In a group of duplicates one
  line is the anchor (the unsuffixed original if there is one, else the
  lowest `statement_line_id`); each other line gets one `DUPLICATE_ENTRY`
  suggestion (`CONFIRM_DUPLICATE`, confidence label 85 for identical
  references, 70 for suffix-related) naming both lines — the second in
  `counterpart_statement_line_id` (migration 000004).
  *Suppress, not coexist:* duplicate detection runs first, and a copy gets
  *only* the `DUPLICATE_ENTRY` suggestion — no write-off or review of a copy,
  which would be the wrong action and could contradict the duplicate
  finding. The anchor still gets the normal missing-reference suggestion,
  judged on its own amount and noting its copies: once the copy is removed
  it is one real entry that is still unmatched. So every line remains the
  subject of exactly one suggestion (A3's invariant holds).
  Advisory only: removing a copy is done in bank-reconciliation-svc, by a
  human. `AMOUNT_MISMATCH` / `TIMING_ADJUSTMENT` are produced only on pre-A1
  jobs.
- **A2a. No candidate match against a counterpart record: `AUTO_MATCH` and
  `DATE_SKEW` are never produced** (closed 2026-10-26 for the ledger side; the
  canonical-transaction side is A2b). *Finding, as recorded:* Both mean proposing a counterpart for an
  UNMATCHED statement line, and that counterpart is never another
  bank-reconciliation-svc record. bank-reconciliation-svc's own match
  verification (`MatchStatementLine` / `RunAutomaticMatching` →
  `verifyJournalMatches` / `verifyCanonicalMatch`) accepts exactly two kinds:
  (a) a general-ledger-svc journal that is FINALIZED, of the same legal
  entity, and moves the line's `gl_cash_account_code` by the line's amount
  in the right direction; or (b) a banking-connector-svc canonical
  transaction that is NORMALIZED, of the same tenant and currency, and of
  the same amount. This service reads neither. *Needed:* a read client to
  general-ledger-svc and/or banking-connector-svc that can list candidates
  scoped by the line's cash account (or bank account) and a date window
  around the statement date. Neither offers such a route today:
  general-ledger-svc's `GET /v1/journals` filters only by `legal_entity_id`,
  `fiscal_period`, `status` and `limit` (no account, no date window), and
  banking-connector-svc exposes canonical transactions only by id
  (`GET /v1/banking/canonical-transactions/{id}`) — bank-reconciliation-svc
  itself only ever verifies a caller-named candidate by id. So A2a needs a
  candidate-search route in one of those services (or a bounded
  per-fiscal-period read filtered here), plus `gl_cash_account_code`, which
  bank-reconciliation-svc already returns on a statement line but
  `internal/bankrec` does not yet decode. A candidate then becomes an
  `AUTO_MATCH` suggestion (same amount, dated within a governed date
  tolerance) or `DATE_SKEW` (same amount, dated beyond it), naming the journal or
  transaction id — still advisory: the match is proposed and confirmed in
  bank-reconciliation-svc's own maker-checker flow, by humans.

  *Resolution (2026-10-26).* The ledger side is done.

  - **Why the ledger side.** A run's lines are the bank side. BNK-05's own
    purpose is reconciling bank activity "to authoritative
    ledger/subledger/payment/receipt populations", so the counterpart that
    makes a line reconciled is a book entry. A canonical transaction is
    BNK-04's normalized form of a bank statement line, the same bank fact
    again.
  - **The read.** general-ledger-svc's ACC-05 `QueryLedger` gains a
    transaction-date window and a journal-status filter, plus an index
    (its B14).
    - `internal/bankrec` now decodes `gl_cash_account_code`.
    - A new read-only `internal/ledger` client reads one cash account's
      FINALIZED entries in `[statement date − horizon, statement date +
      horizon]`.
    - A run is one bank account and one statement date, so this is one read
      per run, not per line.
  - **The match rule.** `domain/candidates.go` applies bank-reconciliation-
    svc's own acceptance rule (`verifyJournalMatches`). Entries are grouped
    per journal, and the journal's net movement on the line's cash account
    (debits minus credits) must equal the line's signed amount exactly, as
    `big.Rat`. It must also be of the line's tenant and legal entity, which
    the read is scoped to and the client re-checks.
    - The journal's entries must also be in the line's currency, which
      bank-reconciliation-svc does not check for journals.
    - Direction follows from the sign: money in is a debit to cash.
  - **Outcomes.**
    - *One journal within the governed date tolerance:* `AUTO_MATCH`, naming
      it in `counterpart_journal_id`, confidence label 90.
    - *None within the tolerance, one beyond it inside the horizon:*
      `DATE_SKEW` with `MANUAL_REVIEW`, naming it, confidence label 65.
    - *More than one candidate, or one journal qualifying for two lines:*
      `AMBIGUOUS`. Nothing is named, and the candidates are listed in the
      rationale. One journal can be matched to one line only, so naming it
      for two would invite a human to confirm both.
    - *No candidate:* `NO_CANDIDATE`.
    - *A line with no `gl_cash_account_code`:* `NO_CASH_ACCOUNT`.
  - **How results are recorded.**
    - Every searched line stores its outcome (`candidate_search_outcome`)
      and the tolerance version it was judged by (`date_tolerance_version`),
      so "searched, nothing found" is a recorded result.
    - A proposal replaces the missing-reference suggestion. Every other
      outcome is noted on it, so each line is still the subject of exactly
      one suggestion (A3's invariant).
    - An AMBIGUOUS line is never recommended for write-off.
    - A DUPLICATE_ENTRY copy is not searched; the entry it duplicates is.
  - **The tolerance is governed.** AUTO_MATCH against DATE_SKEW is decided by
    a financial-control-svc TolerancePolicy, metric
    `RECONCILIATION_CANDIDATE_DATE_MATCH`. That is the same versioned
    registry the write-off tolerance comes from: its `date_tolerance_days`
    with `date_basis`.
    - No policy is 424, as for the write-off tolerance.
    - A `BUSINESS` basis is also 424. No business-day calendar exists, and
      financial-control-svc's own matcher counts calendar days too.
    - **Deployment prerequisite:** analyses for a legal entity return 424
      until an operator approves that policy for it.
  - **Fail closed.** If the read fails, nothing is analyzed, because "could
    not look" must never become "no candidate":
    - general-ledger-svc unreachable: 503;
    - a row outside the requested tenant, entity, account or window, or an
      amount that cannot be held exactly: 502;
    - a full page of 1000 entries (there is no cursor, general-ledger-svc
      A15): 422.
  - **Still advisory.** Every suggestion is `RECOMMENDED`. Nothing here
    writes to bank-reconciliation-svc or general-ledger-svc; both test fakes
    fail on any non-GET. `ValidateExecutionRef` already refuses a ledger
    journal on an EXECUTED `AUTO_MATCH`: that match is made in
    bank-reconciliation-svc's propose/confirm flow, by humans.
  - **Storage.** Migration 000008 adds `counterpart_journal_id`,
    `candidate_search_outcome` and `date_tolerance_version`, with CHECKs:
    - a journal is named exactly when the outcome is
      WITHIN_DATE_TOLERANCE or DATE_SKEW;
    - AUTO_MATCH requires WITHIN_DATE_TOLERANCE and a journal;
    - an outcome has a positive version.

    The resolution event carries the journal and outcome. The down
    migration refuses while any row carries a result. It turns
    `row_security` off first, so a role subject to RLS fails the guard
    loudly instead of seeing no rows and dropping the data.

  *Tests:*
  - `internal/domain/candidates_test.go`: AUTO_MATCH; DATE_SKEW; NO_CANDIDATE
    as an explicit result; a same-amount journal of another legal entity,
    currency or tenant never proposed; direction and per-journal netting;
    ambiguity, including one journal for two lines; within beats skew;
    no-cash-account; duplicate copies not searched.
  - `internal/handler/candidate_match_test.go`, through the real analyze
    route:
    - proposals, with one ledger read for a three-line run and its exact
      query parameters;
    - a currency mismatch is not proposed;
    - another entity's journal in the response is 502, unreachable is 503,
      and a full page is 422;
    - an ungoverned or BUSINESS date tolerance is 424;
    - an approved AUTO_MATCH executed with a journal reference is refused
      by the execution-reference rule.
  - `internal/ledger/client_test.go`, `internal/financialcontrol/date_tolerance_test.go`,
    and the `gl_cash_account_code` decode test in `internal/bankrec`.
  - `internal/store/candidate_match_test.go` on real Postgres: round trip,
    the CHECKs refusing inconsistent rows, and the down migration refusing
    while a result exists and succeeding once none does.
- **A2b. No candidate match against a banking-connector-svc canonical
  transaction, and the ledger search has known limits** (part 1 closed
  2026-10-28; part 4 closed 2026-10-30; part 2 closed 2026-11-01; part 3
  closed 2026-11-01 — A2b is now closed in full).
  1. *The canonical side* (closed 2026-10-28). banking-connector-svc serves
     BNK-04 `ListTransactions` (`GET /v1/banking/canonical-transactions`, its
     A20).
     - **When.** Only on a line the journal search (A2a, including its
       cross-line exclusivity pass) leaves `NO_CANDIDATE`. A line with a
       journal proposal, a journal `AMBIGUOUS`, or `NO_CASH_ACCOUNT` keeps
       that result and is never also searched here, so a line never carries
       both kinds of candidate. `domain.CanonicalSearchLines` names those
       lines; the handler reads banking-connector-svc only when there is at
       least one, so a run whose lines all have a journal result never calls
       it.
     - **The read.** A new read-only `internal/banking` client, built like
       `internal/ledger`: one `GET` per (bank account, currency, statement
       date) — normally one per run — with `legal_entity_id`,
       `bank_account_id` (the line's own, from bank-reconciliation-svc; both
       services key it to treasury-svc's BNK-01 account), `currency`, the
       same `[statement date ± horizon]` window as the ledger read, and
       `limit=5000` (the route's cap). Every row is re-checked against the
       tenant, entity, bank account, currency and window asked for, must be
       NORMALIZED, have a UUID id, and carry its amount as a decimal *string*
       held exactly (a bare JSON number is refused as a contract
       regression). A non-empty `next_cursor` is refused as
       `ErrWindowTooLarge`: cursor-following is not implemented, and a
       partial page cannot prove there is no candidate.
     - **The match rule.** bank-reconciliation-svc's `verifyCanonicalMatch`
       accepts a NORMALIZED transaction of the same tenant and currency
       whose amount equals the line's, compared signed, in cents. Both
       services sign an amount the same way — banking-connector-svc copies
       the raw posted amount (opening + Σ lines = closing), and a
       bank-reconciliation-svc line is positive for money in — so the rule
       here is a direct signed equality, as `big.Rat` (stricter than cents).
       It also requires the line's legal entity and bank account, which that
       check does not.
     - **Outcomes.** As for journals, under the same governed date
       tolerance: one within it is `AUTO_MATCH`, one only beyond it inside
       the horizon is `DATE_SKEW` / `MANUAL_REVIEW`, more than one — or one
       transaction claimed by two lines — is `AMBIGUOUS`, none is
       `NO_CANDIDATE` (now stating that both services were searched). The
       outcome values are reused; the rule names the source:
       `CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE`,
       `CANDIDATE_TRANSACTION_DATE_SKEW`, `CANDIDATE_TRANSACTION_AMBIGUOUS`.
       The rationale names banking-connector-svc, the transaction and the
       bank account, and says no journal matched first. Cross-line
       exclusivity is per record kind: a journal and a transaction are never
       treated as the same record, even with equal ids.
     - **Storage.** Migration 000009 adds `counterpart_transaction_id` and
       replaces 000008's counterpart CHECKs: exactly one of journal or
       transaction is named exactly when the outcome is
       WITHIN_DATE_TOLERANCE or DATE_SKEW, AUTO_MATCH names one, and a named
       journal or transaction must sit under its own kind's rule. The
       resolution event carries `counterpart_transaction_id`. The down
       migration refuses while any row carries a canonical result.
     - **Fail closed.** If the read is needed and fails, nothing is
       analyzed: unreachable 503; a `next_cursor` 422; a refusal (including
       403 while the principal lacks `bank.transaction.read`) or an
       inconsistent row 502. The domain refuses too: a `NO_CANDIDATE` line
       whose bank account was not read is an error, never "no candidate".
     - **Deployment prerequisite.** This service's principal needs a role
       bundle granting `bank.transaction.read` (access-control-svc migration
       000022 registers the permission) on each legal entity it analyzes.
       Until it is granted, analysis of any run with a `NO_CANDIDATE` line
       is 502. `BANKING_CONNECTOR_SERVICE_URL` defaults to
       `http://banking-connector-svc:8145`.
     - **Not searched.** A `NO_CASH_ACCOUNT` line is not searched for a
       canonical transaction, although bank-reconciliation-svc could verify
       one for it. Widening the gate is a reviewer's call.

     *Tests:* `internal/banking/client_test.go` (exact query, envelope,
     scope/entity/account/currency/status/window/UTC rows refused, a
     `next_cursor` page refused, an amount float64 cannot distinguish held
     exactly, 400/403/503/unreachable); `internal/domain/canonical_candidates_test.go`
     (AUTO_MATCH, DATE_SKEW, beyond horizon, NO_CANDIDATE naming both,
     NO_CANDIDATE-only gating against journal AUTO_MATCH / DATE_SKEW /
     AMBIGUOUS and NO_CASH_ACCOUNT, an unread account refused, signed and
     exact amounts, out-of-scope rows, AMBIGUOUS, cross-line exclusivity
     across both kinds); `internal/handler/canonical_candidate_match_test.go`
     through the real analyze route (proposal on the wire, one read with its
     exact parameters, no read when not needed, 503/422/502);
     `internal/store/candidate_transaction_test.go` on real Postgres (round
     trip, the CHECKs, the down migration).
  2. *Multiple journals — closed 2026-11-01.* A journal used to be a
     candidate only if its own net movement equaled the line; one payment
     booked as two journals was never found. Closing this for real required
     a change in bank-reconciliation-svc first: its match model only ever
     accepted one journal per line (`matched_journal_id` is a scalar
     column), so detecting a qualifying pair here would have had nothing to
     confirm it against. bank-reconciliation-svc's own A2 now records "no
     many-to-one, one-to-many or split matches" as partially closed on the
     journal path (migration 000015): `ProposeMatch`/`ConfirmMatch` accept
     `journal_ids` (2 or more, currently capped at 10), verify each one
     independently and their combined net together, and record the
     allocation as explicit evidence — satisfying ZS-CONTROL-001 section
     10's "many-to-one / one-to-many ... with explicit allocation", which
     this service's own governing-document citations already name.
     - **The search.** `domain.searchJournalPairs` tries every distinct
       pair of journals on the line's cash account — bounded to exactly
       two, the case the gap names, not an open-ended combination search —
       but only when the existing single-journal search (including its
       cross-line exclusivity pass) finds nothing at all. A single matching
       journal is always preferred over a pair. Each journal in a
       candidate pair must individually be real, correctly scoped and
       single-dated (the same per-journal checks as the single-journal
       search), and both must be within the search horizon; their combined
       net, not either alone, must equal the line's signed amount exactly.
     - **Outcomes.** Reused, as for A2a/A2b part 1: one qualifying pair
       within the governed date tolerance (the later of the two journals'
       days from the statement date decides) is `AUTO_MATCH`; one only
       beyond it but inside the horizon is `DATE_SKEW`/`MANUAL_REVIEW`;
       more than one qualifying pair, or a journal shared with another
       line's single or paired candidate, is `AMBIGUOUS`; none is
       `NO_CANDIDATE` (now meaning both single journals and pairs were
       tried). New rule names:
       `CANDIDATE_MULTI_JOURNAL_WITHIN_DATE_TOLERANCE`,
       `CANDIDATE_MULTI_JOURNAL_DATE_SKEW`,
       `CANDIDATE_MULTI_JOURNAL_AMBIGUOUS`.
     - **Cross-line exclusivity.** Extended, not reimplemented: the same
       `proposedBy` journal-id map used for single-journal exclusivity now
       also registers both halves of an accepted pair, so a journal shared
       between a pair and a single match (or two pairs) demotes every line
       that claimed it to `AMBIGUOUS`, exactly as for two lines sharing one
       single journal.
     - **Confidence — a judgment call (B-style interpretation).** A
       two-journal proposal is scored below its single-journal counterpart
       at the same date outcome (80 vs. 90 for AUTO_MATCH, 55 vs. 65 for
       DATE_SKEW): it depends on two records being exactly right, not one.
       Not spec-mandated; a reviewer may want these raised, lowered, or
       equalized.
     - **Storage.** Migration 000010 adds `counterpart_journal_ids`
       (`UUID[]`, exactly 2 elements when set) alongside the existing
       `counterpart_journal_id`/`counterpart_transaction_id`, replacing
       000009's counterpart CHECKs with a three-way version: exactly one of
       the three counterpart shapes is named exactly when the outcome is
       `WITHIN_DATE_TOLERANCE` or `DATE_SKEW`, `AUTO_MATCH` names exactly
       one, and each named shape must sit under its own kind's rule. The
       down migration refuses while any row carries a multi-journal result.
     - **Still not closed on the canonical-transaction path.** This
       increment is the journal path only — the same path A2b part 1 and
       the rest of A2a already use, and reconciliation-intelligence-svc's
       own search order tries it first. banking-connector-svc's
       `ListTransactions`/the canonical candidate search (A2b part 1) is
       untouched: a canonical transaction is still matched one at a time.
       bank-reconciliation-svc's own `journal_id`/`journal_ids` match path
       is marked deprecated there in favor of `transaction_id` for a new
       *single* match, so a reviewer may want many-to-one moved to (or
       duplicated onto) the canonical-transaction path instead; this was a
       deliberate scope decision, not an oversight.
     - *Tests:* `internal/domain/multi_journal_candidates_test.go`
       (AUTO_MATCH and DATE_SKEW via a pair; a single journal preferred
       over a qualifying pair; multiple qualifying pairs are AMBIGUOUS; a
       pair sharing a journal with another line's single match makes both
       AMBIGUOUS; no single journal or pair still reaches NO_CANDIDATE);
       `internal/store/candidate_multi_journal_test.go` on real Postgres
       (round trip including the `UUID[]` column, every CHECK refusing an
       inconsistent row — wrong counterpart combination, wrong rule, wrong
       array length — and the down migration refusing while a result
       exists, succeeding once none does). bank-reconciliation-svc's own
       side is tested separately — see its SPEC_DEVIATIONS.md A2.
  3. *Already-matched journals — closed 2026-11-01.* bank-reconciliation-svc
     now offers the lookup this service needed: `GET /v1/statement-lines`
     takes `journal_ids` (ANDs with its existing filters, exactly like
     `bank_account_id`/`statement_date`/`status`; the expected caller sends
     it alone) and returns every line, any status, that names one of the
     given ids as its matched or proposed counterpart — scalar or, since A2b
     part 2, recorded in `statement_line_matched_journals`. It is the only
     service that knows a journal is claimed, because the claim is recorded
     there, not on the journal itself.
     (`services/bank-reconciliation-svc/internal/store/pg_store.go`'s
     `ListStatementLines`, `internal/domain/types.go`'s
     `ListStatementLinesFilter.JournalIDs`/`MaxJournalLookupIDs` (200),
     `internal/handler/handler.go`'s `parseJournalIDsParam`.)

     This service's `internal/bankrec.Client.LookupClaimedJournalIDs` calls
     it once per candidate search, batched at 200 ids, after the ledger read
     and before the journal/pair search run: every distinct journal id the
     ledger returned is checked in one pass, not one call per line. A
     claimed id is excluded from `searchCandidates` (the single-journal
     search) and from `searchJournalPairs`'s pool (A2b part 2) alike — it
     can never be proposed alone or as half of a pair. The lookup fails
     CLOSED like every other upstream read here: a failed batch fails the
     whole analyze call (502), never a partially-checked result. A journal
     absent from the claimed set is simply unclaimed as of this read, not
     guaranteed available — bank-reconciliation-svc's own verification at
     propose/confirm time remains the real, authoritative check; this only
     stops the suggestion from being produced in the first place.

     *Tests:* bank-reconciliation-svc —
     `internal/handler/handler_test.go` (`TestListStatementLines_JournalIDsFilter_*`:
     distinct valid ids wired through, malformed/duplicate/empty ids
     refused 400, no param leaves the filter nil);
     `internal/store/journal_lookup_pg_smoke_test.go` on real Postgres
     (scalar matched, scalar proposed, a multi-journal allocation found by
     either of its two journal ids with both returned via
     `ProposedJournalIDs`, no match is `[]` not an error, the filter ANDs
     with an existing status filter rather than overriding it, and more
     than `MaxJournalLookupIDs` ids is refused).
     This service — `internal/bankrec/client_test.go`
     (`TestLookupClaimedJournalIDs_*`: empty input makes no call, scalar and
     array ids are all extracted including one the caller never asked
     about, more than 200 ids are batched into separate requests, and a
     failed batch fails the whole lookup);
     `internal/domain/already_matched_journal_test.go` (a claimed journal is
     excluded from the single-journal search, excluding it leaves a
     genuinely unclaimed candidate usable, a claimed journal is excluded
     from the pair pool entirely, and a nil map — every caller that predates
     this change — excludes nothing);
     `internal/handler/already_matched_journal_test.go` through the real
     analyze route with a fake bank-reconciliation-svc that answers
     `journal_ids` (`TestAnalyze_AlreadyMatchedJournal_IsExcludedFromProposal`,
     `TestAnalyze_UnclaimedJournal_IsStillProposed`).
  4. *No paging — closed 2026-10-30.* general-ledger-svc's A15 added a
     keyset cursor to `QueryLedger` (`after_entry_seq` in,
     `{"entries": [...], "next_cursor": ...}` out, replacing the old bare
     array). `internal/ledger.ListCashAccountEntries` now follows
     `next_cursor` to completion instead of refusing any page of exactly
     `maxEntries` (1000) rows — that count is no longer ambiguous between
     "exactly this many" and "truncated," so a cash account with exactly
     1000 matching entries in the window is read in full, not refused.
     `ErrWindowTooLarge` still exists, now bounded by `maxPages` (20): a
     window needing more than 20 pages (20,000 rows) is still refused
     rather than followed forever, so a pathological or adversarial
     upstream cannot turn this into an unbounded loop — "could not read it
     all" must still never become "no candidate."
     *Tests:* `internal/ledger/client_test.go`
     (`TestListCashAccountEntries_FollowsCursorAcrossPages`,
     `TestListCashAccountEntries_RefusesBeyondMaxPages`);
     `internal/handler/candidate_match_test.go`
     (`TestAnalyze_LedgerUnavailableOrTruncated_FailsClosed`'s two
     subtests: exactly one page's worth of rows now succeeds, and a window
     past `maxPages` is still 422) through the real analyze route and a
     fake general-ledger-svc that implements the real cursor contract
     (`internal/handler/fakes_test.go`'s `newFakeLedgerServer`).
     *At the time:* parts 2 and 3 below were unaffected by this — paging
     finds every row in the window, but a multi-journal payment or an
     already-matched journal was still not specially handled. Both are now
     closed (2026-11-01, above).
- **A3. One-directional** (closed — the framing no longer applies). Only side
  A was walked: a side-B item with no side-A counterpart was never reported,
  and `total_processed_count` and the rate counted side A only. Since A1 there
  are no sides: bank-reconciliation-svc already ran the two-sided match, and
  its UNMATCHED lines are one population. Every line is examined once and
  yields exactly one suggestion, so nothing can be invisible, and
  `total_processed_count` is the number of lines read. A run-sourced job's
  `matched_count` and `reconciliation_rate` are NULL (omitted from JSON)
  rather than a constant 0 / 0%: every line examined is unmatched by
  definition, and match state is bank-reconciliation-svc's to report.
  `source_system_a` is `BANK_STATEMENTS` and `source_system_b` is NULL.
  Pre-A1 jobs keep the counters they were stored with.
- **A4. Money has no currency** (the D06 decimal half is fixed). *Fixed:*
  amounts, discrepancies and tolerances were `float64`; they are now exact
  decimal strings compared as `big.Rat` (`internal/domain/decimal.go`, same
  convention as financial-statements-svc). The API takes and returns
  amounts as strings — a JSON number is 400 — and an amount that would not
  be stored exactly by the `NUMERIC(18,4)` columns (more than 4 decimal
  places, or 15+ integer digits) is 400 rather than silently rounded by
  Postgres. The tolerance client parses financial-control-svc's decimal
  string straight to `big.Rat` (it used to go through `ParseFloat`) and
  fails closed on a negative tolerance. A discrepancy is now exact
  `|a − b|`; the old float path rounded it to 2 places, so a sub-cent
  difference used to count as a match and now produces a suggestion.
  *Still open:* items carry no currency, so the governed tolerance (which
  has a currency in financial-control-svc) is compared with amounts of
  unknown currency, and the tolerance client ignores the policy's currency.
  Since A1 the source line does have one (bank-reconciliation-svc's
  `currency_code`); it is read but not yet stored or compared.
- **A5. `WRITE_OFF` and `TIMING_ADJUSTMENT` are accounting actions, and
  `EXECUTED` has no reference.** — Closed (B6). The service recommends them,
  which is allowed (advisory), but marking a suggestion `EXECUTED` recorded no
  reference to the authoritative command that did it, only free text in
  `resolution_notes`. ZS-CONTROL-001 requires the RemediationAction to carry
  an "authoritative command reference", and its evidence standard says
  "Journal/reversal/adjustment/credit/debit note IDs; control does not embed
  authoritative correction as free text". `POST …/apply` now takes a
  structured `authoritative_ref`, `{"kind":"GL_JOURNAL","journal_id":"<uuid>"}`:
  the general-ledger-svc journal that posted the correction. Marking a
  `WRITE_OFF` or `TIMING_ADJUSTMENT` `EXECUTED` without one is 400, and the
  suggestion stays `APPROVED`. A note is not accepted in its place. A
  reference that is malformed, or sent with any decision other than
  `EXECUTED`, is also 400. Migration 000007 stores it in two columns, and the
  database refuses an `EXECUTED` accounting action without one. The reference
  is returned on the suggestion and carried on the
  `reconciliation.resolution_recommended` event. `EXECUTED` is kept: the
  doc's other option, dropping it and letting the owning domain's event close
  the suggestion, is a larger change than this gap needs.
  *Still open:* the reference is caller-asserted. Its shape is checked, but
  general-ledger-svc is not asked whether the journal exists, is posted, or
  belongs to the suggestion's entity (B6). Suggestions `EXECUTED` before
  000007 carry no reference, and none is reconstructed.
- **A6. Reads are not authorized** (closed). `GET /v1/reconciliations` and
  `GET /v1/reconciliations/{id}` checked only the tenant (RLS); any principal
  of the tenant could read every legal entity's jobs and items. Both now
  require `X-Principal-Id` (401) and a `RECONCILIATION_READ` grant from
  authorization-svc on the legal entity (403 denied, 503 unreachable), the
  same fail-closed check the writes use. `GET /{id}` checks the job's own
  entity, after loading it, as the write routes do. The list now requires
  `legal_entity_id` (400 without it): "every entity in the tenant" is not
  something one decision can grant. Writes are unchanged — analyze checks
  `RECONCILIATION_ANALYZE` on the request's entity and now also refuses a
  run belonging to another entity. *Open, outside this service:* none of
  this service's actions (`RECONCILIATION_READ`, `_ANALYZE`,
  `_APPLY_RESOLUTION`, `_ARCHIVE`) is registered in access-control-svc's
  permission taxonomy (migration 000008), which validates every bundle
  write, so no permission bundle can grant them yet.
- **A7. Silent in-memory fallback in production** (closed). If Postgres was
  unreachable or failed its ping at startup, `cmd/server/main.go` logged a
  warning and served from `MemoryStore`: jobs and decisions were lost on
  restart and no events were emitted. It now exits non-zero with
  `db unreachable at startup` (or `failed to create db pool`), the same
  posture as general-ledger-svc, financial-statements-svc and
  bank-reconciliation-svc. `MemoryStore` remains only as a handler-test
  double.
- **A8. No model/score provenance.** — Closed (B4). The data model requires
  "model reference and score basis". There is no model to reference: the
  score comes from fixed rules. Those rules now have names. Each suggestion
  records `heuristic_rule`, one of `MISSING_REFERENCE_WRITE_OFF`,
  `MISSING_REFERENCE_MANUAL_REVIEW`, `DUPLICATE_EXACT` or
  `DUPLICATE_SUFFIXED`, matching the four confidence labels. The prose
  rationale in `resolution_notes` remains, but it is no longer the only
  record of which rule fired. Each suggestion also records
  `tolerance_version`: the financial-control-svc `TolerancePolicy` version
  the client returned and the analysis actually used, which used to be
  discarded. Both columns are added by migration 000005 and are NULL only on
  suggestions written before it. The tolerance *value* is not copied — the
  version plus the `RECONCILIATION_MISSING_REFERENCE_WRITEOFF` metric name
  identify the exact append-only policy row. Duplicate suggestions record the
  same version even though their rule does not compare amounts against the
  tolerance (B4).
- **A9. Field names differ from the data model.** — Closed (B5). The
  canonical Data Model / ERD Pack (`docs/architecture/04-data-model.md` §16,
  the document that calls itself "the canonical truth model") names the
  entity `ReconciliationSuggestion` with `reconciliation_suggestion_id`,
  `legal_entity_id`, `source_record_a`, `source_record_b`,
  `confidence_score`, `suggestion_status` and `generated_at`. The Go type is
  now `ReconciliationSuggestion`; `transaction_ref_a|b` are
  `source_record_a|b`, `resolution_status` is `suggestion_status`, and
  `legal_entity_id` is on the suggestion row — migration 000006 adds it,
  backfills it from the job, and makes it NOT NULL. The Data/AI spec's
  DATA-07 "Reconciliation Engine" section names different entities
  altogether (`ReconciliationDefinition`, `ReconciliationRun`,
  `PopulationSnapshot`, `MatchResult`, `ReconciliationException`,
  `Certification`) and does not name `ReconciliationSuggestion`; the
  canonical data model is therefore the source used here. For one release
  the item JSON still emits the old keys and the apply route still accepts
  `resolution_status`, both derived from the canonical fields (B5).
  `resolution_notes` and the `unmatched_items` collection key keep their
  names: A9 did not name them, and the collection key is the existing wire
  shape.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. Missing-reference rule judges the amount's magnitude** (fixed in this
  pass). The rule compared the *signed* amount with the small-amount
  tolerance, so any credit — e.g. −50,000.00 — counted as "below tolerance"
  and got a 90-confidence `WRITE_OFF` recommendation. It now compares
  `|amount|`, and the item's `discrepancy_amount` is the magnitude.
- **B2. A human decision follows a fixed path** (fixed). `ApplyResolution`
  stored any string as `resolution_status` and let any status overwrite any
  other (REJECTED → EXECUTED, EXECUTED → REJECTED, arbitrary text). Now: the
  requested status must be `APPROVED`, `REJECTED` or `EXECUTED` (else 400);
  allowed moves are `RECOMMENDED → APPROVED | REJECTED` and
  `APPROVED → EXECUTED`; anything else is 409 and the earlier decision
  stands. The item row is locked (`FOR UPDATE`) while the move is checked.
  The exact graph is this pass's reading of the service's own comments
  ("it proposes, and a human decides") — no spec defines it. In particular,
  an APPROVED suggestion cannot later be REJECTED; if that is wanted, add the
  edge.
- **B3. Empty status still means APPROVED.** Kept from the original handler
  so existing callers that send no status are unaffected.
- **B4. Tolerance provenance is recorded per suggestion, not on the job.**
  A8 described the gap as "no ... tolerance version stored on the job". The
  version is stored on each suggestion instead, because the suggestion is the
  intelligence artifact whose score basis the data model requires preserving,
  and every suggestion in one job shares the single version fetched for that
  analysis. Duplicate suggestions record it too, although their rule does not
  compare amounts against the tolerance: the version identifies the policy in
  force for the analysis they belong to. A job with no suggestions records no
  version, which is honest — it produced no intelligence artifact.
- **B5. A9's rename is dual-named on the wire for one release.** The
  canonical response emits `suggestion_status`, `source_record_a|b` and
  `legal_entity_id`, and also emits `resolution_status` and
  `transaction_ref_a|b`, derived from the same fields so they cannot
  disagree. `POST …/apply` accepts either status key; sending both with
  different values is 400. The internal "resolution" names A9 did not cover
  (`ApplyResolution`, `resolution_notes`, the
  `reconciliation.resolution_recommended` event and its legacy
  `resolution_status` payload key) are unchanged; the event payload now also
  carries `suggestion_status`. Remove the aliases after one release. The only
  in-repo consumer of the old names is the Postman collection, whose apply
  request still sends `resolution_status`, so it exercises the alias rather
  than a hard break.
- **B7. Candidate-search decisions (A2a) a reviewer may reverse.**
  - *The search horizon is a service setting.* `CANDIDATE_SEARCH_HORIZON_DAYS`
    (1–366, default 31, refused at startup otherwise) bounds how far the
    ledger read reaches for a DATE_SKEW candidate. It is a read bound, not a
    match criterion: AUTO_MATCH against DATE_SKEW is the governed tolerance,
    and the window read is never narrower than it. It could instead be a
    second governed policy field.
  - *What AUTO_MATCH is recorded as.* An in-tolerance proposal is
    `MISSING_REFERENCE` / `AUTO_MATCH`: the line's discrepancy is its
    missing link to a book entry, and the discrepancy amount is 0.
    `DATE_SKEW` gets `MANUAL_REVIEW`, never `TIMING_ADJUSTMENT`, because a
    timing adjustment is a posting (A5) and here the question is only
    whether the dates are close enough to match.
  - *Exact amount.* Matching requires the exact amount, as
    bank-reconciliation-svc's own verification does. No amount tolerance
    applies to a proposal.
  - *Currency.* It is required to match, which is stricter than
    bank-reconciliation-svc's journal check.
- **B6. A5's reference is a ledger journal id, asserted by the caller, and
  required only for accounting actions.**
  1. *One shape, `journal_id`.* Both `WRITE_OFF` and `TIMING_ADJUSTMENT` of a
     bank reconciling item are postings to the entity's own books.
     general-ledger-svc is the only service that posts them, and it returns
     the same identifier for every journal it posts: the UUID `journal_id`.
     That covers its ADJUSTMENT, ACCRUAL, REVERSAL and STANDARD journals, so
     a reversal is a journal id as well. Nothing that owns a bank line issues
     an `adjustment_id`. bank-reconciliation-svc has no write-off or
     adjustment command. consolidation-svc's `adjustment_id` is for
     consolidation-only adjustments, which never touch an entity's books, and
     accounts-receivable-svc's is for receivables. The reference still
     carries `kind` (only `GL_JOURNAL` today), so another owning domain's
     command can be added without changing what is stored.
  2. *Which recommendations need one.* `WRITE_OFF` and `TIMING_ADJUSTMENT`
     must have one. `MANUAL_REVIEW` may have one, because a review can end in
     a journal or in nothing to post. `CONFIRM_DUPLICATE` and `AUTO_MATCH`
     refuse one, because they are carried out in bank-reconciliation-svc, not
     the ledger. bank-reconciliation-svc has no command that removes a
     duplicate line, so those two still reach `EXECUTED` with no reference;
     there is nothing yet for them to name.
  3. *Asserted, not verified.* Nothing else in the apply request is checked
     against another service: the status is checked against the allowed
     decisions, and the note is free text. The only outside call is the
     authorization check. The reference follows the same rule. It must be
     whole, of a known kind, and a UUID, which is canonicalized. Whether it
     is required depends on the suggestion's recommendation, which the store
     reads from the row it already locks. So a decision adds no query and no
     outside call, on a path every decision takes. Verifying it would need a
     call to general-ledger-svc for each `EXECUTED` decision, made
     before the row is locked. That call would return the journal's status
     and legal entity.
  4. *Database.* Two CHECKs in migration 000007 enforce this:
     - `reconciliation_suggestion_authoritative_ref_check`: a reference is
       whole or absent, is `GL_JOURNAL`, is recorded only on `EXECUTED`, and
       only on `WRITE_OFF`, `TIMING_ADJUSTMENT` or `MANUAL_REVIEW`.
     - `reconciliation_suggestion_accounting_execution_check`: an `EXECUTED`
       `WRITE_OFF` or `TIMING_ADJUSTMENT` has a reference. It is `NOT VALID`,
       so rows `EXECUTED` before 000007 stay as they are, and every new write
       is held to it.

     The down migration refuses, with "cannot roll back 000007: recorded
     authoritative references exist", while any reference is recorded.
