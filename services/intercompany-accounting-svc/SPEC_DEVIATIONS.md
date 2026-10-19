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

- **A1. `GetReciprocalBalances` is not implemented.** No endpoint aggregates
  intercompany positions by entity pair (A owes B / B owes A, net, by currency,
  by status). Consolidation therefore cannot ask "what is the matched reciprocal
  balance between X and Y for this period", and has to fetch every entry and
  add them up itself (see consolidation-svc A5). Scope: `GET
  /v1/intercompany/reciprocal-balances?entity_a=&entity_b=&period=` returning
  per-direction and net totals split by `match_status`, authorized on both
  entities. This needs a period on the pair first (A4).
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
- **A3. `ExplainMismatch` is a free-text string.** `mismatch_reason` is a
  formatted sentence (`amount_mismatch: intercompany amount … != journal amount
  …`). There is no structured explanation (expected vs observed amount and
  currency per side, which rule failed, the journal ids examined, the
  tolerance applied), and the reason is overwritten on each match attempt, so
  earlier attempts are lost. Scope: a `match_attempts` child table written in
  the `UpdateMatch` transaction, served by `GET …/entries/{id}/explanation`.
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
- **A6. The source side is never verified.** `CreateIntercompanyPair` accepts
  any `source_journal_id` without asking general-ledger-svc whether it exists,
  is FINALIZED, belongs to `source_legal_entity_id`, or matches the amount.
  Only the target side is checked, at match time. The negative path "One side
  missing" is therefore enforced for the counterparty only. Scope: the same
  four checks `MatchEntry` runs on the target, run on the source at create
  time (reject, or create directly in MISMATCH with a reason).
- **A7. Events: two of the four named events are missing, and one has a
  misleading name.** Emitted: `intercompany-entry.created`
  (IntercompanyPairCreated), `intercompany-entry.mismatch-detected`
  (IntercompanyMismatchDetected) and `intercompany-entry.posted` for a
  successful match. IntercompanyMatched is published under the name
  **`posted`**, which suggests a ledger posting this service never makes.
  `IntercompanyDisputeResolved` is not emitted at all, and acknowledge and
  dispute emit nothing either, so a consumer (ACC-12/13, close) cannot react
  to a resolution. Scope: enqueue `matched` (keeping `posted` as the legacy
  type for one release), `dispute-raised` and `dispute-resolved` inside the
  existing guarded transactions.
- **A8. No segregation of duties on resolution.** §14: "One entity cannot
  certify both sides of material mismatch without compensating control."
  Dispute and resolve use different actions, but both are authorized on the
  **target** entity only, and nothing stops the principal who raised a dispute
  from resolving it, or a target-entity user from resolving a mismatch with no
  source-entity involvement. There is no materiality notion to scope it.
  Needs a decision on what "certify both sides" means here (resolver ≠
  disputer; or source and target each acknowledge; or a materiality threshold
  above which both are required) before it is built.
- **A9. No `Closed` state, and `RESOLVED` records no outcome.** The state
  model ends "Resolved/Closed"; only RESOLVED exists, reachable only from
  DISPUTED. A MATCHED pair has no terminal close (e.g. after the period is
  consolidated), and a resolution does not say whether the mismatch was
  accepted, corrected, or written off, so consolidation cannot tell whether a
  RESOLVED pair may be eliminated (it currently eliminates MATCHED only).
- **A10. No `Idempotency-Key` or `expected_version` on the lifecycle
  commands.** The wireframe requires both on every command. Create is
  idempotent by `(tenant_id, source_journal_id)`; acknowledge, match, dispute
  and resolve are protected only by their status guards, so a retried dispute
  after a lost response gets 422 instead of its original result, and there is
  no version a client can send to say "only if nothing changed since I read
  it". There is no version column.
- **A11. Group relationship check is optional and as-of now.** The "entity
  loses group relationship mid-period" check runs only when an entity-registry
  client is configured (it is in `cmd/server`), and it checks the relationship
  at the time of the match call, not over the pair's period. Belongs with A4.

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
