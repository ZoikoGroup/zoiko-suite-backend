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
But it does not do what the data model and the ownership table say it is for:
it does not consume bank-reconciliation and it never proposes a candidate
match. Roughly 40% of what its own description implies is implemented. Two
defects were fixed in this pass (B1, B2); a later pass closed A7 and the
decimal half of A4.

## A. Not implemented / weaker than the spec

- **A1. It does not consume bank-reconciliation.** Transactions are pushed in
  the `POST /v1/reconciliations/analyze` body by the caller; nothing reads
  bank-reconciliation-svc's runs or unmatched items. Suggestions are therefore
  about whatever the caller sent, with no link to an authoritative
  reconciliation run or record. Scope: read the unmatched items of a
  bank-reconciliation run (or a financial-control-svc run) by id, and store
  the source run id and record ids on each suggestion.
- **A2. It never proposes a candidate match.** Matching is exact `ref_id`
  equality; `RecommendationAutoMatch` and the discrepancy types `DATE_SKEW`
  and `DUPLICATE_ENTRY` are declared and never produced. The one thing
  ZS-CONTROL-001 §10 says AI assistance is for — proposing that two records
  with *different* references are the same — is absent. Duplicate refs on
  side B collapse silently in a map (last one wins).
- **A3. One-directional.** Only side A is walked: a side-B item with no
  side-A counterpart is never reported, and `total_processed_count` and the
  reconciliation rate count side A only.
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
  Belongs with A1, once bank-reconciliation data gives the amounts a
  currency.
- **A5. `WRITE_OFF` and `TIMING_ADJUSTMENT` are accounting actions, and
  `EXECUTED` has no reference.** The service recommends them, which is
  allowed (advisory), but marking a suggestion `EXECUTED` records no
  reference to the authoritative command that did it (journal id, adjustment
  id). ZS-CONTROL-001 §21/§24 require remediation to reference the owning
  domain's command, never free text. Scope: require an `authoritative_ref` on
  `EXECUTED`, or drop `EXECUTED` and let the owning domain's event close the
  suggestion.
- **A6. Reads are not authorized.** `GET /v1/reconciliations` and `GET
  /v1/reconciliations/{id}` check only the tenant (RLS); any principal of the
  tenant can read every legal entity's jobs and items. Writes are authorized
  per entity.
- **A7. Silent in-memory fallback in production** (closed). If Postgres was
  unreachable or failed its ping at startup, `cmd/server/main.go` logged a
  warning and served from `MemoryStore`: jobs and decisions were lost on
  restart and no events were emitted. It now exits non-zero with
  `db unreachable at startup` (or `failed to create db pool`), the same
  posture as general-ledger-svc, financial-statements-svc and
  bank-reconciliation-svc. `MemoryStore` remains only as a handler-test
  double.
- **A8. No model/score provenance.** The data model requires "model
  reference and score basis"; the score is one of three fixed numbers and the
  rule that fired is only in free-text notes. There is no rule/heuristic
  version or tolerance version stored on the job (the tolerance version
  fetched from financial-control-svc is discarded).
- **A9. Field names differ from the data model.** `UnmatchedItem` /
  `resolution_status` / `transaction_ref_a|b` rather than
  `ReconciliationSuggestion` / `suggestion_status` / `source_record_a|b`; no
  `legal_entity_id` on the item row (it is on the job).

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
