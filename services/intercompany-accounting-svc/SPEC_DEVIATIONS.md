# Spec deviations — intercompany-accounting-svc (ACC-11)

Governing document: `ZoikoSuite_Accounting_Record_to_Report_Detailed_Service_Specifications.docx`,
§14 "ACC-11 — Intercompany Accounting", plus the authority table in §2 (ACC-11
"owns Intercompany pair/match state. Must never own: Local entity source
accounting"), the invariant "Unmatched intercompany balances remain visible and
are not silently eliminated", the §23 event table, the §24 data model
(IntercompanyPair) and negative tests 21–22 of §27. First survey of this
service against the spec (2026-10-17). The platform completion tracker's "Not
Started" row for ACC-11 is stale and was ignored.

Overall: all five named commands exist (`CreateIntercompanyPair` = `POST
/v1/intercompany/entries`, `AcknowledgeCounterparty`, `MatchIntercompany`,
`DisputeIntercompany`, `ResolveMismatch`), with guarded state transitions,
transactional-outbox events for create/match/mismatch, idempotent create, and
three of the four negative paths (amount differs, counterparty journal missing,
group relationship lost). `GetIntercompanyPair` is built. What is missing is
mostly on the read side (no reciprocal balances, no real unmatched list, no
structured explanation) and in how much of a pair is captured (one amount, one
currency, no source-side verification). Roughly 60% of the named capabilities
are implemented.

**Non-ownership boundary: holds.** The only calls this service makes to other
services are reads: `GET /v1/journals/{id}` on general-ledger-svc
(`internal/ledger/client.go`), `GET /entities/{id}/hierarchies` on
tenant-entity-registry-svc (`internal/entityregistry/client.go`) and `POST
/v1/authorize` on authorization-svc. Its only writes are to its own
`intercompany_entries` table and the `eventing_outbox`. It never creates,
edits or reverses a journal or ledger entry, the same posture
reconciliation-engine-svc holds for DATA-07.

## A. Not implemented / weaker than the spec

- **A1. `GetReciprocalBalances` is not implemented. — Closed (B10).** `GET
  /v1/intercompany/reciprocal-balances?entity_a=&entity_b=&period=` returns
  the period's intercompany positions between the two entities, split by
  `match_status`: each direction's totals by currency (A's source side
  against B, and B's against A) and their net per currency, summed exactly
  (`domain.SumAmounts`, `big.Rat`; migration 000005's `fiscal_period` is
  required) and never combined across currencies. Authorized on **both**
  entities; CLOSED is included — it was a real settled position (B10).
  Consolidation's elimination bound (consolidation-svc A5) can now read this
  instead of fetching every entry and adding them up itself; moving it onto
  this endpoint is that service's change, not this one.
- **A2. `ListUnmatchedIntercompany` does not exist as such; the list endpoint
  that stands in for it is unscoped.** `GET /v1/intercompany/entries` has no
  status filter, no period filter and no pagination. When neither
  `source_legal_entity_id` nor `target_legal_entity_id` is given it **skips the
  authorization check entirely** and returns every entry in the tenant to any
  caller with an `X-Principal-Id` header. This conflicts with the spec's
  "Scoped read" control and "cross-entity isolation with authorized group
  scope" (NFR). It cannot be closed by simply requiring a filter:
  consolidation-svc's `FetchMatchedIntercompanyEntries` depends on the
  unfiltered call (it filters for MATCHED on its side). Scope: add `status`
  and period filters plus cursor pagination; require either an entity filter
  or a group-scope authorization (a group-level action checked on the group
  entity); move consolidation-svc onto the scoped form in the same change.
- **A3. `ExplainMismatch` is a free-text string. — Closed (B9).** Migration
  000007 adds `intercompany_match_attempts`: one append-only, tenant-scoped
  row per applied MatchIntercompany attempt, written in the `UpdateMatch`
  transaction. Each row records the rule that failed, expected against
  observed amount and currency per side, the journal ids examined, the
  tolerance applied, the pair's period, who made the attempt and when.
  `GET …/entries/{id}/explanation` serves the rows. `mismatch_reason` is
  still returned, but it is now read from the latest attempt instead of
  being overwritten on the pair, so earlier attempts are kept. The match
  response also names `failed_rule`.
- **A4. A pair carries one amount and one currency, and no period or
  dimensions. — Closed (B7).** Migration 000005 adds
  `counterparty_amount`/`counterparty_currency_code`, `fiscal_period` and
  `dimensions` to the pair. MatchIntercompany now compares the target journal
  to the counterparty side, currency first: a currency difference is recorded
  as a MISMATCH (`currency_mismatch`), and the amounts are never compared.
  General-ledger-svc already returned the journal's `currency_code`; this
  service never decoded it. Amounts are exact decimals end to end (`big.Rat`,
  never `float64`). A1 and A5 stay open; the period and currency columns they
  need are now there.
- **A5. The match compares the whole target journal, not its intercompany
  line.** `MatchEntry` takes the larger of the journal's total debits and
  total credits and compares that to the entry amount. A journal that also
  carries non-intercompany lines (tax, a third-party leg) will mismatch, and a
  journal of the right total but the wrong accounts will match. There is no
  intercompany-account resolution ("Server-resolved: intercompany accounts")
  and no matching-policy or tolerance version (fixed `0.001`). Needs an
  intercompany account designation from the chart of accounts (ACC-01/02);
  until then this is a known approximation.
- **A6. The source side was never verified. — Closed (B12).**
  `CreateIntercompanyPair` now fetches the named source journal from
  general-ledger-svc with the same `GetJournal` call `MatchEntry` uses and
  runs the same four checks on it that `MatchEntry` runs on the target: the
  journal exists, its legal entity is the pair's `source_legal_entity_id`,
  its status is `FINALIZED`, and its currency is a valid ISO 4217 code equal
  to the pair's `currency_code`. A journal that fails one is a real
  accounting fact, so the pair is created directly in MISMATCH, in the same
  transaction as its `intercompany_match_attempts` row (A3), recording
  `SOURCE_JOURNAL_MISSING`, `SOURCE_ENTITY_MISMATCH`,
  `SOURCE_JOURNAL_NOT_FINALIZED`, `SOURCE_CURRENCY_UNVERIFIABLE` or
  `SOURCE_CURRENCY_MISMATCH`; migration 000008 widens the rule CHECK for
  them. The answer is 201 with the pair (MISMATCH plus `mismatch_reason`),
  not a 400/422/503 that would make the caller retry an identically wrong
  request. A general-ledger-svc outage is still a 503 and creates nothing.
  The source amount is not compared at create: A6's scope is the four checks
  `MatchEntry` runs before its amount check, and amount verification is A5's,
  where the journal-total approximation lives. A replay for the same
  `source_journal_id` returns the original pair before the ledger is
  consulted, so it neither re-verifies nor adds an attempt.
- **A7. Events: two of the four named events were missing, and one had a
  misleading name. — Closed (B11).** Emitted: `intercompany-entry.created`
  (IntercompanyPairCreated), `intercompany-entry.mismatch-detected`
  (IntercompanyMismatchDetected), `intercompany-entry.matched`
  (IntercompanyMatched) and `intercompany-entry.dispute-resolved`
  (IntercompanyDisputeResolved), plus `intercompany-entry.dispute-raised`
  for the MISMATCH → DISPUTED transition. A successful match no longer
  publishes under `posted`: the envelope's canonical `type` is `matched`,
  and the pre-standard `event_type` remains `intercompany.entry.posted` as
  the legacy name for one release. Acknowledge emits no event: the
  governing spec's ACC-11 "Events produced" row names only PairCreated,
  Matched, MismatchDetected and DisputeResolved (and §23 repeats
  Matched/MismatchDetected), so no acknowledgement event is invented. All
  three new events are enqueued in the same guarded transaction as the
  state change; a failed enqueue rolls the change back.
- **A8. No segregation of duties on resolution.** §14: "One entity cannot
  certify both sides of material mismatch without compensating control."
  Dispute and resolve use different actions, but both are authorized on the
  **target** entity only, and nothing stops the principal who raised a dispute
  from resolving it, or a target-entity user from resolving a mismatch with no
  source-entity involvement. There is no materiality notion to scope it.
  Needs a decision on what "certify both sides" means here (resolver ≠
  disputer; or source and target each acknowledge; or a materiality threshold
  above which both are required) before it is built.
- **A9. No `Closed` state, and `RESOLVED` records no outcome. — Closed
  (B8).** Migration 000006 adds `CLOSED`, the terminal state of a MATCHED
  pair (`POST …/entries/{id}/close`, with a required reason). It also
  requires `outcome` (`ACCEPTED` / `CORRECTED` / `WRITTEN_OFF`) on
  ResolveMismatch and stores it as `resolution_outcome`. Every state change
  is now recorded in the append-only `intercompany_pair_transitions` table
  (`GET …/entries/{id}/transitions`). consolidation-svc now also eliminates
  CLOSED pairs, not MATCHED only (see B8) — it still does not read
  `resolution_outcome`.
- **A10. No `Idempotency-Key` or `expected_version` on the lifecycle
  commands.** The wireframe requires both on every command. Create is
  idempotent by `(tenant_id, source_journal_id)`; acknowledge, match, dispute
  and resolve are protected only by their status guards, so a retried dispute
  after a lost response gets 422 instead of its original result, and there is
  no version a client can send to say "only if nothing changed since I read
  it". There is no version column.
- **A11. Group relationship check is optional and as-of now. — Closed (B13).**
  `MatchIntercompany` now evaluates the relationship at the end of the pair's
  `fiscal_period` when it is a `YYYY-MM` period — the same "as of the end of
  that month" cut-off the control-population contract gives `period_id` — not
  at the time of the match call. A relationship that held through the period
  and has ended since still matches; one that began after the period, or was
  lost inside it, is a MISMATCH regardless of today's hierarchy.
  `domain.FiscalPeriodEnd` computes the instant. A pair with no period, or a
  period that is not `YYYY-MM` (A4/B7 allow a free reference and
  general-ledger-svc stores it verbatim), is still checked as of now, because
  there is no calendar end to invent. The client is wired by `cmd/server` in
  every deployment (the URL has a non-empty default, and a registry that
  cannot be reached is a 503, never a skip); the nil-client branch is
  reachable only from tests and now logs a warning rather than passing
  silently. B13 has the reasoning and tests.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. Match outcome writes are guarded on the current status** (changed in
  this pass). `UpdateMatch` used to overwrite unconditionally, so a dispute
  that landed between `MatchEntry`'s read and its write was overwritten with
  MATCHED or MISMATCH. It now updates only from UNMATCHED,
  AWAITING_COUNTERPARTY or MISMATCH (`domain.MatchableStatuses`) and returns
  `ErrInvalidPairTransition` (422) otherwise, the same compare-and-set the
  other three lifecycle methods already used. In the same change, the five
  mismatch branches of `MatchEntry` stopped discarding the store error: they
  used to answer 422 MISMATCH even when the write (and its event) had failed;
  a failed write is now a 503. Covered by a real-Postgres test and two handler
  tests.
- **B2. `UNMATCHED` is the spec's "Open".** Kept under its pre-existing name
  (migration 000003).
- **B3. `MatchIntercompany` may skip `AwaitingCounterparty`.** A pair can be
  matched straight from UNMATCHED; acknowledgement is optional. The spec draws
  the states as a sequence but does not say acknowledgement is mandatory.
- **B4. MISMATCH is re-matchable.** A caller can re-run the match with a
  corrected `target_journal_id` instead of going through dispute.
- **B5. Routes and action names are not the wireframe's.** Routes are REST
  (`/v1/intercompany/entries/...`) rather than `/internal/v1/acc11/...`;
  actions are `INTERCOMPANY_ENTRY_{CREATE,VIEW,MATCH,ACKNOWLEDGE,DISPUTE,RESOLVE}`
  rather than `intercompany.manage/match/resolve`. Same platform convention as
  the other ACC services.
- **B6. Control population.** `GET /v1/control-populations/{population}`
  serves financial-control-svc (not part of ACC-11's named surface).
- **B7. How the pair's two sides, period and dimensions are modelled (A4).**
  - *Which side is which.* `amount`/`currency_code` keep their meaning as the
    originating (source) side. `counterparty_amount`/`counterparty_currency_code`
    are what the target entity is expected to book.
    `counterparty_currency_code` defaults to `currency_code`. In a
    same-currency pair `counterparty_amount` defaults to `amount` and may not
    differ from it, because otherwise a caller could choose what the target
    journal has to match. A cross-currency pair must state
    `counterparty_amount`. There is no FX rate source, so none is derived.
  - *Backfill.* Existing pairs get counterparty = source. That is exactly
    what the old match compared the target journal against, so their
    behaviour does not change.
  - *Period.* `fiscal_period` uses general-ledger-svc's own rule for a period
    reference (1-20 characters, no control characters; no fiscal calendar
    service exists to check it against). It is optional: treasury-svc's
    BNK-09 pairing does not send one yet. It is NULL on every pre-000005 pair.
    The true period is the source journal's, and inventing one from
    `created_at` would misplace pairs at period boundaries. A1 must decide
    how to report NULL-period pairs; it must not drop them silently.
  - *Dimensions.* A flat string map like general-ledger-svc's line
    dimensions, bounded to 32 entries (names 1-64 characters, values up to
    256, no control characters), and stored as a JSONB object (CHECK).
  - *Currency at match time.* A journal whose currency is not a real ISO
    4217 code is recorded as a MISMATCH (`currency_unverifiable`) and never
    matched. That covers general-ledger-svc's `XXX`, which it backfilled onto
    journals that predate its currency column. New pairs must carry
    three-letter uppercase codes. Existing rows are not re-validated.
  - *Decimals and the wire format.* Amounts are `domain.Decimal`, exact
    decimal text built on the `ParseAmount`/`SumAmounts`/`CanonicalAmount`
    pattern from financial-statements-svc. They are bound as text cast to
    NUMERIC and read back as `::text`. A value NUMERIC(18,4) would round
    (more than 4 decimal places) or overflow (more than 14 integer digits) is
    refused with a 400 instead of being stored as a different number. On the
    wire `amount` and `counterparty_amount` stay **JSON numbers**, written
    from the exact digits. consolidation-svc's `clients.IntercompanyEntry`
    and treasury-svc's create request both use number types, so a string
    would break them. Quoted strings are accepted on input, so callers can
    move to strings before the output changes.
  - *Tolerance.* Still the fixed 0.001 (A5), now compared exactly.
- **B8. CLOSED, resolution outcomes and the transition history (A9).**
  - *Which states may close.* MATCHED only, as A9 scopes it. An open,
    mismatched or disputed pair still has an unsettled difference, and
    closing it would take that difference out of view (the invariant
    "Unmatched intercompany balances remain visible"). RESOLVED is left as
    it was: it is already terminal, and whether a resolved pair should also
    close is a decision for the consolidation change below. CLOSED is
    terminal, and nothing (match included) leaves it.
  - *Who may close.* `INTERCOMPANY_ENTRY_CLOSE` is checked on **both** the
    source and the target entity. A terminal close takes the pair out of
    each side's open set, so neither side alone may do it. The other
    commands check one entity (A8 is about who may certify both sides; that
    question is not settled here).
  - ***Follow-up required before closing pairs in production: closed.***
    consolidation-svc's `FetchMatchedIntercompanyEntries` (renamed
    `FetchEliminableIntercompanyEntries`) kept `match_status == "MATCHED"`
    only, so a CLOSED pair would have dropped out of elimination on any
    re-run for an already-consolidated period. It now keeps MATCHED or
    CLOSED (consolidation-svc's own B6), and its `Amount` field moved off
    `float64` onto exact decimal text in the same change (ZS-DATA-001 D06).
    `resolution_outcome` is still not read there — that remains open.
  - *Resolution outcome.* Required on every new resolution, by the handler
    (400 `invalid_outcome`), by the store, and by a CHECK constraint.
    `chk_resolved_has_outcome` is `NOT VALID`, so pairs resolved before
    000006 keep a NULL outcome rather than an invented one. A consumer must
    read NULL as "unknown", never as eliminable. The endpoint's input is
    exact upper-case codes; lower-case is refused rather than normalised.
  - *History.* `intercompany_pair_transitions` follows
    reconciliation-engine-svc's `reconciliation_run_transitions`: identity
    key, `from_state <> to_state`, ENABLE + FORCE RLS on tenant, and an
    UPDATE/DELETE trigger (`reject_immutable_row`) that rejects any rewrite.
    Each row carries the pair's `fiscal_period` at the time, the reason
    (mismatch reason, dispute reason, resolution note or close reason), the
    outcome on RESOLVED, the actor and the correlation id. Every lifecycle
    write now locks the row (`SELECT … FOR UPDATE`), checks the current
    status, updates it (keeping the WHERE-status guard as a second line of
    defence) and inserts the transition in one transaction. A repeated
    MISMATCH is not a state change and adds no row (each attempt is recorded
    by A3's `match_attempts`). The backfill writes one row per existing
    pair, for the state it is in, with actor `migration:000006`; earlier
    transitions were never recorded and are not reconstructed.
  - *UpdateMatch accepts only MATCHED or MISMATCH* as the target status. It
    used to write whatever status it was handed, which would now have been
    a way to reach CLOSED without the close guard.
  - *No event for close.* Dispute and resolve now emit `dispute-raised` and
    `dispute-resolved` (B11); close remains outside the spec's named event
    set.
- **B9. How match attempts are recorded (A3).**
  - *What counts as an attempt.* Every MatchIntercompany call that reaches
    an outcome and is applied, whether MATCHED or MISMATCH. These calls
    record nothing: a call refused because the pair moved on since it was
    read (422 `invalid_transition`), a call rejected before the ledger is
    consulted (400/403/404), and a dependency outage (503). Recording a
    refused attempt would make a stale result the "latest" and change the
    `mismatch_reason` of a pair it never applied to.
  - *Rules.* `COUNTERPARTY_JOURNAL_MISSING`, `TARGET_ENTITY_MISMATCH`,
    `TARGET_JOURNAL_NOT_FINALIZED`, `CURRENCY_UNVERIFIABLE`,
    `CURRENCY_MISMATCH`, `AMOUNT_MISMATCH`, `GROUP_RELATIONSHIP_LOST`
    (CHECK-constrained), in the order the match applies them. The first
    failing rule is the one recorded. The message keeps the sentence format
    `mismatch_reason` always had.
  - *Sides.* Each attempt stores the source and target side as journal id,
    expected amount and currency, and observed amount, currency, legal
    entity, status and fiscal period. The target's observed amount is the
    journal total the match compared (A5's larger-of-debits-and-credits),
    recorded even when an earlier rule failed. The source side's observed
    columns are NULL until the source journal is verified (A6), and they
    already exist for that change. Observed amounts are unconstrained
    `NUMERIC`: they are general-ledger-svc's figures and must not be rounded
    to fit the pair's NUMERIC(18,4). Expected amounts are the pair's own.
  - *Integrity.* Append-only through the same `reject_immutable_row` trigger
    as B8's transitions, FORCE RLS on tenant, and CHECKs that make a
    MISMATCH carry a rule and message and a MATCHED attempt carry neither.
  - *`mismatch_reason`.* Read from the latest attempt's message (NULL when
    the latest attempt matched, or when there has been none). A DISPUTED,
    RESOLVED or CLOSED pair keeps reporting the reason of the attempt that
    put it there. The pair's own `mismatch_reason` column is no longer
    written or read. It keeps its pre-000007 value only so a rollback has
    something to return to: 000007's down migration writes each pair's
    latest message back before dropping the attempts. The column can be
    dropped once no deployed binary reads it.
  - *Backfill.* A pair with a `mismatch_reason` gets one
    `LEGACY_UNSTRUCTURED` attempt carrying that sentence, attributed to
    `migration:000007` and timestamped at the migration. Only the last
    attempt's text ever survived the overwrite, and when it was made is
    unknown. Expected amounts come from the pair (after 000005), and the
    tolerance is the 0.001 the old match applied. A pair that never
    mismatched gets no attempt; none is invented.
  - *Events.* The `mismatch-detected` event payload still carries
    `mismatch_reason`. `failed_rule` is in the HTTP response and the
    explanation, and is not yet added to the event; A7/B11 did not change
    the mismatch event's payload.
- **B10. How reciprocal balances are aggregated (A1).**
  - *What is summed.* Each pair contributes its own (source) side,
    `amount`/`currency_code`, grouped by `(source_legal_entity_id,
    target_legal_entity_id, currency_code, match_status)` and summed in Go
    with `domain.SumAmounts` (`big.Rat`), never in SQL and never through
    float64. "Entity A to entity B" is the pairs whose source is entity_a
    (the originating side); the other direction is the pairs sourced from
    entity_b. Net is A's direction minus B's, per currency.
  - *Currencies are never combined.* A USD total and a GBP total are
    separate entries in every direction and in the net; a currency carried
    by one direction only gets a signed net of its own, and a zero net is
    stated rather than omitted. A cross-currency pair's counterparty side is
    what the target is expected to book (B7), not a second amount in the
    source side's currency: there is no FX rate source to translate it
    with, so it is not folded into any total.
  - *CLOSED counts.* It is the terminal state of a MATCHED pair — a real
    settled position, the same reasoning that keeps it eliminable in
    consolidation-svc (B8).
  - *The period is required and exact.* Only pairs carrying the requested
    `fiscal_period` are aggregated. A pair with a NULL period (pre-000005,
    or a caller that sends none) belongs to no period: it is left out of
    every period's answer rather than attributed to one, the same choice A4
    made when it refused to invent a period.
  - *Refused rather than rounded.* A total that `CanonicalAmount`'s
    12-place rendering would round is refused by `ExactDecimal` instead of
    returned, and an amount NUMERIC(18,4) would round cannot be stored in
    the first place (`ValidatePositiveAmount`, B7) — so the answer is the
    exact sum of the stored amounts or an error, never a different number. A
    sum may exceed one column's NUMERIC(18,4) range (it is not being
    stored); it is still returned in full.
  - *Answer shape.* One entry per `match_status` present, in the spec's own
    state-model order; an empty direction is `[]`, never null, and a pair
    with no positions in the period is an empty `by_match_status`. A store
    error is a 503, never an empty or partial answer.
  - *Route and controls.* `GET /v1/intercompany/reciprocal-balances` (the
    house REST form, B5). The spec's command table marks GetReciprocalBalances
    a QUERY — "Scoped read; correlation + auth context" — so there is no
    Idempotency-Key: the envelope's write rule (INV-08) does not apply to a
    read. The read is checked with `INTERCOMPANY_ENTRY_VIEW` on both
    entities, the same two-entity shape as `ClosePair` (B8).
- **B11. How A7's event set is emitted (A7).**
  - *Matched, not posted.* `UpdateMatch` now enqueues the canonical
    `com.zoikosuite.accounting.intercompany-entry.matched` with
    `LegacyType: intercompany.entry.posted`. While legacy fields are on, the
    envelope still carries the old `event_type`
    (`intercompany.entry.posted`), so a consumer filtering on the old name
    sees the same name; the canonical `type` is the corrected one. This is
    one event, not two: a second copy under the old canonical name would
    duplicate a fact. No in-repo consumer filtered on either name —
    consolidation-svc reads intercompany entries over HTTP, not this topic —
    and the legacy name is kept for one release regardless.
  - *Dispute and resolution.* `DisputeIntercompany` (MISMATCH → DISPUTED)
    enqueues `...dispute-raised`; `ResolveMismatch` (DISPUTED → RESOLVED)
    enqueues `...dispute-resolved`. Their legacy `event_type` values are
    `intercompany.entry.dispute-raised` and
    `intercompany.entry.dispute-resolved`. The payload carries the pair's
    state as committed, plus `dispute_reason` on a dispute and
    `resolution_note` and `resolution_outcome` on a resolution.
    `resolution_outcome` is the same ACCEPTED / CORRECTED / WRITTEN_OFF
    value stored by A9, so a consumer can act on the decision rather than
    merely on the fact that a resolution happened.
  - *No acknowledgement or close event.* `AcknowledgeCounterparty` and
    `ClosePair` emit none. The governing spec's ACC-11 "Events produced" row
    names IntercompanyPairCreated, IntercompanyMatched,
    IntercompanyMismatchDetected and IntercompanyDisputeResolved only; §23
    repeats Matched and MismatchDetected. Adding an acknowledgement fact
    would be inventing an event the spec does not name. Close is outside
    A7's scope and remains without one.
  - *Transaction.* The lifecycle helper loads the pair after its guarded
    UPDATE, under the same row lock and RLS-scoped transaction, and calls the
    same `enqueueEntryEvent` helper every other event in this service uses.
    If building or enqueuing the envelope fails, the transaction rolls back,
    so a committed state change and its event cannot diverge.
- **B12. How the source journal is verified at create (A6).**
  - *One verification path.* `CreateEntry` and `MatchEntry` both call
    `checkJournal`, which makes the same `ledger.Client.GetJournal` request
    and applies the checks in the same order: exists → entity → FINALIZED →
    currency. Currency has two refusals, `…_CURRENCY_UNVERIFIABLE` for a
    code that is not a real ISO 4217 currency (`XXX`, general-ledger-svc's
    value for journals predating its currency column) and
    `…_CURRENCY_MISMATCH` for a different valid one; both precede any amount
    comparison, which is why a 5,000 GBP source journal cannot back a 5,000
    USD pair.
  - *Side-specific rules.* The target's existing rule names are unchanged.
    The source gets its own five so an explanation cannot mistake which side
    failed; the target's `COUNTERPARTY_JOURNAL_MISSING` wording is not
    reused for the source.
  - *Created in MISMATCH, 201.* The create command succeeds in the sense
    that the pair exists; its state says the source journal did not verify.
    The caller is not asked to retry. The create-time attempt records the
    source journal's observed amount, currency, entity, status and period
    (when the journal was fetched) and both sides' expectations; the target
    journal id is NULL until a match names it. It is append-only like every
    other attempt.
  - *Still 503 for an outage.* A failed general-ledger-svc call, or an
    unreadable journal amount, creates nothing and answers 503
    `gl_query_failed`: unlike a wrong journal, a retry can succeed.
  - *No amount check.* A6's scope is the four checks `MatchEntry` runs
    before its amount check, not amount equality. The pair's `amount` is the
    source side the caller states. A5 already records that the match compares
    a journal total, not its intercompany line, so comparing the source
    journal's total here would reject journals A5 knows are approximated.
    Source amount verification belongs with A5.
  - *Events.* The create transaction enqueues `…created` and, because the
    pair is MISMATCH from birth, `…mismatch-detected`; both carry
    `match_status` MISMATCH and the reason. Both roll back with the pair.
  - *Replay.* `GetEntryBySourceJournal` resolves `(tenant_id,
    source_journal_id)` before `GetJournal`, so a retried POST gets 200 and
    the original pair even during a ledger outage, and
    `CreateEntryWithAttempt` writes no attempt when its insert conflicts. A
    concurrent create between the lookup and the insert is the same: the
    loser returns the original row.
  - *Source is checked once, at create.* `MatchIntercompany` still verifies
    the target only. A source journal edited after the pair was created is
    not re-checked by a match; that is outside A6. Match attempts therefore
    still leave the source's observed columns NULL, and the create-time
    attempt is the one carrying them.
  - *Schema.* Migration 000008 widens
    `intercompany_match_attempts_failed_rule_check`. Its down migration
    restores 000007's list as `NOT VALID`: source-rule rows are immutable
    and must survive a rollback, and a pre-A6 binary only writes rules the
    old list allows. Re-applying 000008 validates them again.
- **B13. How the group-relationship check is anchored to the pair's period (A11).**
  - *As-of instant.* `groupRelationshipAsOf` evaluates the check at the last
    nanosecond of the pair's `fiscal_period` month, in UTC
    (`domain.FiscalPeriodEnd`). That is `control-population-contract.md`'s
    `period_id` rule — "as of the end of that month", all records dated
    on/before the last day — applied to a timestamp rather than a date: the
    last instant is inclusive of anything dated on the month's last day, and a
    relationship end-dated anywhere inside the month (including midnight on
    the last day) is closed by then and fails. Both hierarchy lists are still
    fetched unfiltered (open and closed alike), and
    `entityregistry.SharesOpenGroupRelationship` already applies
    `effective_from`/`effective_to` against the supplied instant, so there is
    no second date algorithm here.
  - *Nil or non-calendar period.* A4/B7 made `fiscal_period` optional and a
    free 1-20 character reference; general-ledger-svc stores it verbatim
    (`TestWave4_FiscalPeriodIsNotAssumedToBeYYYYMM` there). Only exact
    `YYYY-MM` has a month end. A pair with no period, or with one such as
    `FY26-P09`, is checked as of now: the pre-A11 behaviour, kept because
    there is no defensible period instant to substitute and refusing the match
    would reject a period this service already accepted. That is the only case
    in which the call time is used.
  - *Optional client.* `cmd/server` is the only production entrypoint and
    always calls `WithEntityRegistry`; `TENANT_REGISTRY_URL` has a non-empty
    default and config's `env` treats empty as unset, so no deployment can
    omit the client. A configured client that cannot be reached fails the
    match with 503 `entity_registry_unavailable`; it never passes. The nil
    branch is reachable only by handler tests that exercise other rules
    without a registry; it now logs a warning instead of skipping silently.
    It was not turned into a fail-closed 503 because it cannot occur in a
    deployment and doing so would only change the test harness; the existing
    `NoEntityRegistryConfigured` test pins that test-only skip.
  - *Evidence.* The match attempt still records the pair's `fiscal_period`
    (A3), so the instant the relationship was evaluated at can be
    reconstructed from the explanation.
  - *Tests.* `domain.FiscalPeriodEnd` covers normal month ends, a leap
    February and non-calendar periods. Handler tests cover a relationship
    that ended after the pair's period and still matches (it failed under the
    old call-time check), one that began after the period and is MISMATCH
    although open today (it passed under the old check), and one that ended
    inside the period and is MISMATCH. The existing intact/lost tests seed no
    period, so they continue to cover the as-of-now fallback.
