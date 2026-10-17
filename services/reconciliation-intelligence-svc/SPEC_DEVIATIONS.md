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
It now consumes bank-reconciliation (A1), but it still never proposes a
candidate match (A2), which is the other half of what the data model and the
ownership table say it is for. Two defects were fixed in this pass (B1, B2);
a later pass closed A7 and the decimal half of A4; a third closed A1, A3 and
A6.

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
- **A2. It never proposes a candidate match.** There is no matching at all
  now: since A1 the input is bank-reconciliation-svc's UNMATCHED lines, and
  each one gets a review/write-off suggestion on its own.
  `RecommendationAutoMatch` and the discrepancy types `DATE_SKEW` and
  `DUPLICATE_ENTRY` are declared and never produced, and `AMOUNT_MISMATCH` /
  `TIMING_ADJUSTMENT` are now produced only on pre-A1 jobs. The one thing
  ZS-CONTROL-001 §10 says AI assistance is for — proposing that a line and a
  ledger record with *different* references are the same — is absent; it
  needs a counterpart source (general-ledger journals or banking-connector
  canonical transactions) this service does not yet read.
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
  `EXECUTED` has no reference.** The service recommends them, which is
  allowed (advisory), but marking a suggestion `EXECUTED` records no
  reference to the authoritative command that did it (journal id, adjustment
  id). ZS-CONTROL-001 §21/§24 require remediation to reference the owning
  domain's command, never free text. Scope: require an `authoritative_ref` on
  `EXECUTED`, or drop `EXECUTED` and let the owning domain's event close the
  suggestion.
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
