# Spec deviations — reconciliation-engine-svc (DATA-07, ZS-SVC-Z-001 DQC-03, ZS-CONTROL-001)

Governing documents. The service-ownership table names **DATA-07** (ZS-SVC-N-001,
Data, Search, Analytics & AI specifications, "DATA-07 — Reconciliation
Engine") and **ZS-SVC-Z-001 DQC-03** (Reconciliation, Control Total & Balance
Certification). DATA-07 depends on ZS-CONTROL-001, whose matching (§10),
tolerance (§11), exception (§21) and invariant (§4) rules therefore apply too.

DATA-07 names: ownership of ReconciliationDefinition, ReconciliationRun,
PopulationSnapshot, MatchResult, ReconciliationException, Certification;
explicit non-ownership of "editing ledger/subledger/source records; inventing
corrective journals"; commands StartRun, Match, RaiseException, Reperform,
Certify, SupersedeRun; lifecycle Planned → Running → ExceptionsOpen →
Reperformed → Certified / Failed; events CONTROL.ReconciliationCompleted,
CONTROL.ExceptionRaised, CONTROL.Certified; three acceptance tests.

Overall: every named command and read surface exists and the non-ownership
line holds (the service writes only its own tables). Populations are now read
from their owning services through the control population contract, with
watermarks, declared totals and hashes pinned on the frozen snapshot (B7), and
definitions and runs carry a legal entity (B9). Matching honours the declared
composite key, compares currency and dates, and checks per-currency balance
for the population and every declared dimension (B13–B15). A run carries
three state dimensions — lifecycle, result and certification — with an
append-only history, and a run that cannot prove its populations is recorded
Failed / INDETERMINATE (B18–B19). Exceptions are owned, classified,
severity-graded against a per-currency materiality, due by an SLA and follow
a governed lifecycle; accepted risk is a separate, authorized path that keeps
the result VARIANCE, and unresolved exposure is assessed in aggregate
(B21–B24). Still missing: rounding and business-day tolerance and reconciling
items (A5), maker-checker beyond accepting risk (A9), recurrence and
qualitative materiality (A4 remainder), and any defined meaning for a PARTIAL
result (A16). Defects fixed: B1–B4; Stage 1 closed A1, A10, A13 and A14
(B7–B12); Stage 2 closed A2 and A3 and most of A5 (B13–B17); Stage 3 closed
A6 (B18–B20) and A7 and most of A4 (B21–B25).

## A. Not implemented / weaker than the spec

- **A1. Closed** — populations are read from their owning services; see B7.
- **A2. Closed** — currency is compared and never summed across; see B14.
- **A3. Closed** — composite-key matching and dimensional balance; see B13.
- **A4. Mostly closed** — per-currency materiality grades every exception and
  the aggregate is assessed (B23). Still missing: *recurrence* ("repeated
  small items may be material by pattern", §11; §21 "Recurrence") needs
  exception history across runs of a definition, and *qualitative* triggers
  ("quantitative and qualitative criteria") need a criteria list nobody has
  defined; neither raises severity today. Close impact (scenario 25
  "reassesses close impact") belongs to the close service, which can read
  the run's assessment.
- **A5. Rounding and business-day tolerance are missing** (absolute,
  percentage and calendar-day date tolerance exist: B15). §11 also names a
  *rounding tolerance* "bound to known rounding rules, currencies and
  tax/accounting calculations", and allows date tolerance in *business* days.
  Both need inputs this service does not have — the applicable rounding rule
  and a business-day calendar — which the platform keeps in the jurisdiction
  and tax services; they are not to be hardcoded here. §10's "expected clearing
  date" for a timing difference — a reconciling item that is expected to
  clear rather than an error — is also not modelled: a DATE_OUTSIDE_TOLERANCE
  exception is an ordinary exception with the TIMING category, due by the SLA
  rather than by its clearing date.
- **A6. Closed** — three state dimensions, transition history, reachable
  Failed / INDETERMINATE; see B18–B19.
- **A7. Closed** — governed exception lifecycle, owner, classification,
  severity, SLA, aging and an authorized accepted-risk path; see B21–B22.
  Not built: ZS-CONTROL-001's *carried forward under authority* (it is not in
  ZS-SVC-Z-001's exception vocabulary, which this service follows, and needs
  a next-period run to carry into) and an escalation chain beyond the owner
  role (§21 "Owner / reviewer — authorized owner and escalation chain").
- **A8. Reperform does nothing useful.** `Reperform` re-runs auto-matching on
  items with neither a match nor an exception — after the first `Match` there
  are none, so it only changes the status to `Reperformed`. Because
  populations are frozen in the run and remediation happens in the owning
  domain, a meaningful reperformance is a new run over new populations,
  linked to this one. Scope: define reperformance as "start a linked run and
  supersede on certification", or remove the command.
- **A9. No maker-checker or separated permissions** (except accepting risk,
  which has its own action and independence rule — B22). Every other command
  uses one action, `RECONCILIATION_MANAGE`, so the same principal can define the
  rules, start the run, manually match, and certify it (ZS-CONTROL-001
  Invariant 12, §27; DATA-07 "configuration/rule publication separated from
  evaluation/release approval"). Scope: separate actions (define, run, manual
  match, certify) and refuse a certifier who started the run or made a manual
  match in it, under the run's row lock.
- **A10. Closed** — definitions and runs carry `legal_entity_id`; see B9.
- **A11. Definitions are upserted in place, without history or approval.**
  Re-posting a name (within its legal entity) overwrites its rules, now
  including its populations and amount scale (the version increments on a rule change — B4 —
  but the previous rules are not kept, so a run's `definition_version` can be
  matched only against the copy frozen on the run itself). No approval step,
  no effective dating (ZS-SVC-Z-001 INV-01/22, ZS-CONTROL-001 Invariant 4).
  There is also no read endpoint for definitions.
- **A12. Certification is thin.** The sealed manifest records counts, the
  pinned tolerance/materiality, the definition version and (since B22) the
  result and the assessment with every accepted risk — not the
  population hashes, the manual matches with their reasons, or the certifier's
  authority (ZS-SVC-Z-001 INV-21, DQC-05 "certification binds exact
  evidence"; ZS-CONTROL-001 Invariant 9). No revocation.
- **A13. Closed** — supersession is constrained and records its actor; see B10.
- **A14. Closed** — population totals are overflow-checked; see B11.
- **A15. How an absolute and a percentage tolerance combine is unspecified.**
  Text checked: ZS-CONTROL-001 §11 "Absolute tolerance — Value in relevant
  currency/unit; versioned by control and scope" and "Percentage tolerance —
  Permitted only where economically meaningful; base must be explicit";
  ZS-SVC-Z-001 DQC-01 "Explicit numeric or percentage tolerance, scale/rounding
  semantics and population definition" and INV-09. Each describes one kind of
  tolerance; none says whether a comparison that declares both passes when it
  is within *either* threshold (the common "5 cents or 0.1%") or only when
  within *both*. The two readings disagree on real items, and choosing one is a
  control-design decision for the definition owner, not an implementation
  detail. Until it is decided, a currency declares exactly one kind (B15);
  declaring both is refused. Once decided, the change is a `combine` field on
  the currency tolerance (`EITHER` / `BOTH`), versioned with the policy.
- **A16. What makes a run PARTIAL is unspecified.** Text checked:
  ZS-SVC-Z-001 DQC-03 result table "PARTIAL — Only defined subset matched;
  never presented as full population certification"; INV-26 "A partial
  reconciliation cannot be represented as full certification"; NP-26 "Partial
  population labeled full match — Must fail safe; preserve evidence; block or
  isolate affected certification"; the reconciliation state row "NOT_RUN /
  MATCH / VARIANCE / INDETERMINATE" (which omits PARTIAL). None says how a
  subset is *defined* — a run scope narrower than the definition's population,
  a declared exclusion list (ZS-CONTROL-001 names exclusions on a snapshot),
  or a source that declares itself incomplete — nor whether a PARTIAL result
  can carry a certification of its own subset ("cannot be represented as
  *full* certification" implies some narrower certification may exist). This
  service has no subset concept: every population is proven complete against
  the source's declared totals or the run fails (B7, B19). So PARTIAL is in
  the result vocabulary and the column's CHECK, but neither the domain nor the
  lifecycle trigger lets a run enter it, and a PARTIAL could never be ELIGIBLE
  or CERTIFIED (only a MATCH can). Deciding the subset model is a control-design
  question for the definition owner; once decided it is a definition field
  (the declared subset), a snapshot field (what was excluded and why) and an
  edge into PARTIAL.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. A manual match cannot hide an amount difference** (fixed in this
  pass). Before, any two items could be manually matched with a reason, which
  resolved their exceptions whatever the amounts — an operator could clear an
  out-of-tolerance pair, or pair two items from the same side, and certify.
  ZS-CONTROL-001 §10 ("cannot hide amount difference") and scenario 06
  ("narrative alone cannot force pass") forbid that. Now the two items must
  be on different sides and differ by no more than the run's pinned tolerance
  (`domain.ValidateManualMatch`); otherwise the request is refused and the
  exceptions stay open.
- **B2. An empty population is still a side** (fixed). The matcher used to
  take its sides from the items still unprocessed, so when one side had no
  items it found fewer than two sides and raised nothing; the run then moved
  to `Running` with zero exceptions and could be certified with nothing
  matched (ZS-SVC-Z-001 NP-27, "missing source coerced to zero"). Sides now
  come from the frozen snapshots.
- **B3. Exactly two populations** (fixed). DATA-07 says "two or more", but the
  matcher only ever compared the first two sides alphabetically; a third
  population was frozen, never matched or excepted, and the run could still
  certify (INV-26). `StartRun` now refuses anything but two populations, and
  the matcher refuses a run that does not have two. N-way reconciliation is
  not implemented. Runs created earlier with three or more sides will now
  fail `Match`/`Reperform` with an error instead of certifying partially.
- **B4. The version changes only when the rules change; a run cannot
  supersede itself** (fixed). `trigger_definition_versioning` (migration
  000001) bumped `version` on every update, so re-posting identical rules
  produced a new version that meant nothing. Migration 000003 makes the
  trigger bump `version` and `updated_at` only when `match_key_fields`,
  tolerance or materiality change; the trigger is the single owner of both
  columns. (A first attempt put this condition in the upsert's SQL; the
  trigger overrode it, which the integration test caught.) `SupersedeRun`
  with `new_run_id` equal to the run id is refused.
- **B5. Verification of B1–B4.** B1 and B3 have unit tests in
  `internal/domain`; the SQL behaviour (sides from snapshots, conditional
  versioning, the supersede guard, manual-match refusal) has integration tests
  in `internal/store/pg_store_test.go`, run against Postgres 16 with
  `TEST_DATABASE_URL`.
- **B6. Refused requests answer 400.** The new errors fall through the
  handler's existing default branch (400), like the service's other
  validation errors.
- **B7. Populations are read from their owners, not pushed (closes A1).**
  A definition now names its two populations — `{side, system, population,
  params, no_period}` — and `POST /runs` takes only `definition_id`, an
  optional `period_id` (YYYY-MM) and an optional `scope` map. StartRun reads
  both populations through `control-population-contract.md` for the
  definition's legal entity and that period, forwarding the caller's verified
  tenant/principal/entity so each source applies its own authorization. The
  client fails closed on exactly the conditions financial-control-svc's does:
  unregistered system, non-200, unreadable page, missing watermark or one that
  changes between pages, `declared_totals` missing on any page, differing
  between pages, or not tying to the received records (row count and exact
  per-currency decimal total), an invalid record, a cursor that does not
  advance, more than 200,000 records. Each snapshot pins the spec it was read
  with (`spec_ref`), the watermark, the declared row count and totals, its own
  per-currency control totals and a sha256 over every frozen record; each item
  keeps its source `record_id` and currency. Migration 000004's NOT VALID
  CHECKs make the table refuse a new snapshot without that evidence, or whose
  row count differs from the declared one.

  *Shared or duplicated?* Duplicated: `internal/source/client.go` is a copy of
  financial-control-svc's, adapted to this service's types, and the header
  says to keep the two in step. No service imports a shared Go module (no
  `go.mod` under `services/` references `zoiko.io/contract`);
  `services/_contract/rollout.sh` records the platform rule that shared code is
  vendored, not imported, and financial-close-svc already carries its own
  population client. Extracting would also have meant editing
  financial-control-svc, and Go forbids importing its `internal/` packages
  anyway. If a third consumer appears, the client is a candidate for
  `_contract/` plus a rollout step, done as its own change.

  *Failure posture.* A population that cannot be proven complete freezes
  nothing. Stage 1 started no run at all; since Stage 3a (B19) the attempt is
  recorded as a Failed / INDETERMINATE run, answered with 502 (source
  unreachable) or 422 (anything else). The read runs on a context detached from the request (3-minute bound;
  the server's write timeout is 4 minutes), so a client that disconnects does
  not abandon a run that was about to freeze. The Idempotency-Key is checked
  *before* any source is read, so a retry of a run that already started
  replays it instead of re-reading populations that may since have moved. The
  store re-reads the definition under `FOR SHARE` and refuses
  (`ErrDefinitionChanged`, 409) if its version moved while the populations
  were being read.

  *Divergences from financial-control-svc.* A repeated `record_id` in one
  population refuses the whole population here; financial-control-svc instead
  turns duplicates into DUPLICATE exceptions. This service has no exception
  category to put them in until A7, and refusing is the fail-closed option. The
  population hash also covers `attributes`, which financial-control-svc's
  omits, because matching here may key on them (A3).

  *API break.* `POST /definitions` now requires `legal_entity_id`,
  `populations` and `amount_scale`; `POST /runs` no longer accepts items. Both
  are deliberate: the old shapes are what A1 and A10 describe as the defect.
  `SOURCE_ENDPOINTS` (`name=url,…`) registers the readable systems; with it
  empty, every StartRun is refused.
- **B8. Amounts are integers at a declared, versioned scale; nothing is
  rounded.** The contract delivers decimal strings with a currency; this
  service stores int64 units. Converting needs a scale, and the platform
  forbids hardcoded currency exponents, so a definition declares
  `amount_scale` (0–12, required, no default). All amounts of its runs —
  items, tolerance, materiality, totals — are units of 10^-scale. The scale is
  a rule column (bumps the version) and is pinned on every run and snapshot,
  which is ZS-SVC-Z-001 INV-09 ("tolerance, rounding, scale … explicit and
  versioned") and TC-05 ("every monetary total stores currency, scale and
  rounding semantics"). The rounding semantics are "none": a source amount
  with more decimal places than the scale is refused (`ErrAmountNotRepresentable`),
  which is how NP-04 ("producer changes scale/precision silently") fails safe.
  Amounts are **signed**: the contract's GL and bank populations carry negative
  values, so 000004 drops 000001's `amount_minor_units >= 0` checks on items
  and exceptions.
- **B9. Legal entity on definitions and runs (closes A10).** A definition
  belongs to one legal entity (`UNIQUE (tenant_id, legal_entity_id, name)`, so
  the upsert can never move a definition between entities); a run copies its
  definition's entity. Triggers make `legal_entity_id` immutable on both, and
  a run's period, scope and amount scale as well. Every one of the twelve
  `h.authorize` call sites now passes the stored record's entity: run commands
  and reads load the run first; StartRun loads the definition; Define
  validates the requested entity as a UUID before authorizing against it.
  *Legacy rows:* rows written before 000004 have no entity, and none can be
  invented for them, so the new NOT NULL rules are NOT VALID constraints and
  the service refuses such records with 409 (`ErrLegacyRecord`) **before** any
  authorization call, never falling back to a broader scope. *Not changed:*
  the envelope policy keeps `X-Legal-Entity-Id` optional. `internal/envelope/contract.go`
  is generated by `services/_contract/rollout.sh` (outside this service), and
  authorization never reads the header: the entity comes from the record.
- **B10. Supersession is constrained and attributed (closes A13).** The
  replacement must use the same `definition_id` (`ErrSupersedeDifferentDefinition`)
  and be Certified or Failed itself (`ErrReplacementNotTerminal`), and a run
  can be the replacement of only one run (`ErrReplacementAlreadyUsed`, backed
  by a partial unique index). The actor and time are recorded
  (`superseded_by`, `superseded_at`), and a CHECK requires the actor on any
  newly superseded row. Both runs are locked in `run_id` order, so two
  crossing supersessions cannot deadlock. Note that "same definition" means
  same `definition_id`, not same version: a replacement run of a later
  version of the definition is allowed, since correcting the rules is a
  legitimate reason to supersede.
- **B11. Totals are overflow-checked (closes A14).** `freezePopulation`
  computes each snapshot's per-currency and overall totals with checked
  addition and refuses the run (`ErrAmountOverflow`) rather than wrapping;
  decimal-to-units conversion refuses an amount that does not fit an int64.
  The certification's `total_asserted_minor_units` is a Postgres `SUM` (numeric,
  so it cannot wrap, and scanning an out-of-range value into int64 errors). It
  still adds both sides and all currencies together, which is A2's problem.
- **B12. Status codes and verification of B7–B11.** New refusals answer 409
  when they conflict with stored state (`ErrDefinitionChanged`,
  `ErrLegacyRecord`, the three supersession errors), and 422/502 for
  populations as in B7; everything else keeps B6's 400. Tests: unit tests for
  the source client (ported from financial-control-svc's suite), scale
  conversion, overflow, the hash and request validation; handler tests that
  authorization is made against the stored entity and that a source failure
  starts no run; integration tests (Postgres 16, `TEST_DATABASE_URL`) for the
  pinned evidence, the definition-version race, the table-level evidence
  CHECK, overflow leaving no run behind, entity scoping and immutability,
  versioning of populations and scale, and every supersession rule. Migration
  000004 was round-tripped down and up.
- **B13. Composite-key matching and dimensional balance (closes A3).**
  `match_key_fields` is validated against what an item has — `ref_id`,
  `currency`, `occurred_at`, `amount_minor_units`, `dimensions.<name>` — and
  is now pinned on the run (before, a run did not pin its match key at all).
  Items pair on the composite key; an item lacking a key field is an
  exception (MISSING_KEY_FIELD) rather than pairing on an empty value.
  Pairing is deterministic (source record id order; for each side-A item the
  first same-key candidate that reconciles, otherwise the first candidate,
  and both become exceptions); before this change the order fell back to
  random item ids. After item matching, side totals are compared per
  currency for the whole population and for every value of every declared
  `balance_dimensions` attribute, and any bucket outside the currency's
  tolerance raises a BALANCE exception (no item; signed difference A − B;
  both totals and the threshold in `detail`), once per run and bucket however
  often matching re-runs. This is DQC-03 "no aggregate-only pass when a
  material sub-dimension is out of balance", INV-08, NP-21. Two reader notes:
  (a) every out-of-tolerance bucket is flagged — whether it is *material* is
  graded only once A4 exists, so this is the stricter reading; (b) a BALANCE
  exception cannot be resolved by a manual match (matching does not change
  frozen totals), so until A7's waiver path it can only be cleared by
  superseding the run. Items without the attribute form their own bucket.
- **B14. Currency is compared, never summed across (closes A2).** A
  key-matched pair in different currencies is a CURRENCY_MISMATCH exception
  for both items, whatever the amounts and tolerance; amounts in different
  currencies are never compared. ZS-CONTROL-001 §10 permits cross-currency
  matching only "using approved FX basis", and no FX basis is available to
  this service, so none is attempted. Snapshots stop writing the single
  `total_amount_minor_units` and certifications stop writing
  `total_asserted_minor_units` (both added currencies together; NOT VALID
  CHECKs keep new rows from writing them); certifications seal per-side,
  per-currency `control_totals`, and the manifest also pins the tolerance
  policy, match key and balance dimensions. Exceptions carry a currency.
  Manual matches (B1) now use the same comparison as automatic ones, so a
  manual match also cannot cross currencies or hide a timing difference.
- **B15. Per-currency tolerance policy (A2, A5).** `tolerance` replaces
  `tolerance_amount_minor_units`:
  `{"currencies": {"USD": {"absolute_minor_units": 5}, "EUR": {"percentage_bps": 10}},
  "percentage_base_side": "Ledger", "date_days": 3}`. Each currency declares
  one kind (A15). A percentage needs `percentage_base_side` naming one of the
  populations (§11 "base must be explicit"); it is applied to that side's
  amount (or sub-total, for balance) and floored, so it never rounds in the
  preparer's favour. A currency with no entry must match exactly: missing
  configuration can only make the check stricter. `date_days` is a
  calendar-day tolerance on `occurred_at` (DATE_OUTSIDE_TOLERANCE); absent, dates are not
  compared, and the absence is visible on the pinned policy rather than
  implied. The define endpoint now refuses unknown JSON fields, so a client
  still sending `tolerance_amount_minor_units` is told so instead of silently
  getting exact matching.
- **B16. Rules are pinned from run start, with evidence.** Migration 000005's
  lifecycle trigger fixes the tolerance policy, match key, balance
  dimensions, materiality and definition version from the moment a run is
  created, not only once it is certified (the doc-named "tolerance cannot be
  widened during a certified run" test now targets `tolerance_policy`). Every
  exception stores `detail`: the rule that fired, the observed values and the
  threshold applied (ZS-SVC-Z-001 TC-03). The 000005 down migration refuses to
  run while BALANCE exceptions exist (they have no 000004 shape and exceptions
  cannot be deleted), and backfills the strictest single tolerance (0) for
  rows that never had one.
- **B17. Verification of B13–B16.** Unit tests in `internal/domain` for key
  validation, composite keys, missing key fields, currency mismatch,
  absolute/percentage/date tolerance, flooring, overflow at the int64 limits,
  per-currency and per-dimension balance; a handler test for the refused
  legacy field; integration tests for the composite key, an unbalanced
  sub-dimension behind a balanced aggregate (and no duplicate on reperform),
  currency mismatch (including a refused manual match), percentage and date
  tolerance end to end, per-currency certification totals, rules pinned on an
  open run, and versioning of the new rule columns. Two existing tests changed
  expectations because the behaviour is now stricter: an empty side now also
  raises a population-level BALANCE exception. Migrations 000004–000005 were
  round-tripped, and the 000005 down refusal was exercised.
- **B18. Three state dimensions (closes A6).** Copied from
  financial-control-svc's state model, with this service's own vocabularies:
  *lifecycle* stays DATA-07's `status` (Planned → Running → ExceptionsOpen →
  Reperformed → Certified / Failed, plus Superseded) rather than ZS-CONTROL-001
  §7's longer list, because DATA-07 is this service's contract; *result* is
  DQC-03's `NOT_RUN / MATCH / VARIANCE / INDETERMINATE / PARTIAL /
  SUPERSEDED`; *certification* is ZS-SVC-Z-001's `UNCERTIFIED / ELIGIBLE /
  CERTIFIED / SUPERSEDED` (REVOKED is left out: revocation is A12, out of
  scope, and adding a state nothing can reach would only pretend). Rules
  (`internal/domain/state.go`, enforced again by migration 000006's CHECK and
  lifecycle trigger): a run is created Planned / NOT_RUN / UNCERTIFIED; no
  result before matching; an open run's result is MATCH with no open
  exception and VARIANCE otherwise, re-evaluated in the same transaction as
  every match, manual match, raised exception and reperformance; ELIGIBLE
  exactly while the result is MATCH; Certify requires ELIGIBLE (so a VARIANCE,
  INDETERMINATE or PARTIAL can never be certified — INV-05, INV-26; Stage 3b
  refines this for a VARIANCE that is only permitted, immaterial accepted
  risk, which may be certified *as a VARIANCE* — B22); Certified
  ⇔ CERTIFIED; ExceptionsOpen ⇒ VARIANCE; INDETERMINATE can only become
  SUPERSEDED (INV-05); a result is never reset to NOT_RUN; supersession sets
  the result (and a certification) to SUPERSEDED. Every change to any
  dimension appends a row to `reconciliation_run_transitions` (append-only,
  tenant RLS), read at `GET /runs/{id}/transitions`; superseding a run
  therefore keeps its earlier result in that history and, if certified, in
  its sealed certification, whose manifest now also records the result
  ("earlier evidence remains immutable"). Lifecycle edges are financial-control
  style: a closed table, one `advanceRun` that validates all three dimensions
  and writes the history. Two consequences a reader may want to reverse:
  `RaiseException` and `Certify` on a Planned run are refused
  (`ErrRunNotEvaluated`, 409) — before, `RaiseException` there tried
  Planned → ExceptionsOpen, which the trigger refused with a raw database error;
  and Reperform with exceptions still open stays Reperformed with a VARIANCE
  (A8 is unchanged). Existing rows were backfilled from their status and open
  exceptions, recorded as transitions by `migration:000006`; the migration
  refuses to run if a Failed run exists, because no code path could set
  Failed before it and any such row was written by hand.
- **B19. A run that cannot prove its populations is recorded Failed /
  INDETERMINATE.** Scenario 09 ("outage ⇒ Indeterminate/Failed, never Pass")
  and DATA-07's Failed state. When either population is unreachable,
  unregistered, inconsistent with its declared totals, too large, or contains a
  record or amount that cannot be frozen exactly (`source.PopulationError`),
  StartRun records a run pinned to the definition version it attempted, with
  no frozen population, lifecycle Failed, result INDETERMINATE, certification
  UNCERTIFIED, a stable `failure_code` (SOURCE_UNAVAILABLE,
  SOURCE_UNREGISTERED, SOURCE_INCONSISTENT, POPULATION_TOO_LARGE,
  POPULATION_INVALID) and the reason, and emits `CONTROL.ReconciliationFailed`
  (the name mirrors financial-control-svc's run-failed event; DATA-07 lists no
  failure event). The response keeps Stage 1's status codes — 502 for an
  unreachable source, 422 otherwise — now with the run in the body, so a
  client that treated those codes as failure still does. The Idempotency-Key
  is claimed by the failed run: a retry with the same key replays it (200 with
  its id, like every replay); a new attempt uses a new key and is a new run,
  which can supersede the failed one once it is Certified. Failure details are
  written once, on entering Failed, and kept through supersession. Not a run:
  an error in the request itself (a scope value the run does not carry — 400),
  a definition that changed during the read (409), or a failure to write the
  failed run. The population write after the read now has its own 30-second
  detached budget, so a read that used the whole 3-minute bound can still be
  recorded.
- **B20. Verification of B18–B19.** Unit tests in `internal/domain` for every
  lifecycle edge and cross-dimension invariant, the result and certification
  edges (INDETERMINATE → MATCH, reset to NOT_RUN and entering PARTIAL
  refused), and failure validation; `internal/source` for the failure-code
  classification and for a scope error not being a population failure;
  handler tests that a source failure records exactly one failed run with the
  right code and status and freezes nothing, and that a request error records
  nothing. Integration tests: a clean run through certification and
  supersession with its exact transition history; a VARIANCE refused for
  certification and re-evaluated to MATCH by a manual match; a failed run that
  is INDETERMINATE, pinned, unfrozen, closed to Match and Certify, replayed
  under its key and superseded by a certified run; RaiseException and Certify
  refused on a Planned run; direct SQL refused for coercing INDETERMINATE,
  certifying a VARIANCE, certifying outside the command, entering PARTIAL,
  resetting a result, rewriting a failure, creating a run in a later state and
  editing or deleting history; transition history tenant-isolated. Migration
  000006 was round-tripped on a database holding runs in every status, the
  backfill checked per status (Running with nothing open → MATCH / ELIGIBLE,
  Reperformed and ExceptionsOpen with an open exception → VARIANCE /
  UNCERTIFIED, Certified, Superseded, Planned), and both refusals exercised
  (down with live history; up with a hand-made Failed row).
- **B21. Governed exception lifecycle (closes A7).** States are ZS-SVC-Z-001's
  exception dimension — OPEN / INVESTIGATING / REMEDIATED / ACCEPTED_RISK /
  CLOSED (existing `Open` / `Resolved` rows were renamed OPEN / CLOSED). Edges
  (domain and trigger): OPEN → INVESTIGATING | ACCEPTED_RISK | CLOSED;
  INVESTIGATING → REMEDIATED | ACCEPTED_RISK | CLOSED; REMEDIATED →
  INVESTIGATING | CLOSED; ACCEPTED_RISK and CLOSED are final. CLOSED is
  reached only through a manual match (B1): §21 "clearing requires
  authoritative correction … not manual status-only closure". REMEDIATED
  records the correction made in the owning domain (`evidence_ref`, required)
  and leaves the exception unresolved, because this run's populations are
  frozen; a new run re-tests it (DQC-05 "Re-test"; A8). Working an exception
  (INVESTIGATING, REMEDIATED) is the assigned owner's job
  (`:assign` names the owner). Every exception is raised with an owner role
  (`exception_policy.owner_role`, pinned on the run — Invariant 7, never
  ownerless), a category and assertion from §21's lists (fixed per matching
  rule for automatic exceptions; named by the raiser for `RaiseException`), a
  severity and materiality (B23), an exposure, and `due_at` = raised + the
  pinned SLA for its severity (`exception_policy.sla_days`). Identity,
  classification, severity and due date are immutable (DQC "severity cannot
  be edited retroactively"); aging (`age_days`) and attention signals
  (OVERDUE, SLA_BREACH, MATERIAL, UNASSIGNED) are derived on read and never
  stored. A root cause (code + note) is required to remediate or accept a
  BLOCKER. Every move, assignment and manual-match closure is appended to
  `reconciliation_exception_transitions`. New endpoints: `GET
  /exceptions/{id}`, `GET /exceptions/{id}/transitions`, `POST
  /exceptions/{id}:assign`, `POST /exceptions/{id}:transition`, `GET
  /runs/{id}/assessment`. Two consequences: `RaiseException` now requires
  `category` and `assertion`; an item whose exception was accepted as risk can
  no longer be manually matched (acceptance is final).
- **B22. Accepted risk never turns VARIANCE into MATCH; certifying over it is
  opt-in and recorded.** ACCEPTED_RISK needs a named authority
  (`authority_ref`), the separate action `RECONCILIATION_ACCEPT_RISK`
  (scenario 24 "waived without authority — command rejected"), and an
  acceptor who is neither the exception's owner nor its raiser
  (financial-control-svc's waiver independence). An accepted exception stays
  unresolved, so the run's result stays VARIANCE (INV-19, NP-44); the run is
  no longer ExceptionsOpen (nothing is being worked) but it is not a MATCH.
  The run read (`assessment`) lists every accepted risk. Whether such a run
  can be certified is answered by DQC-05's open-blocker rule — "no unresolved
  BLOCKER exception unless certification profile explicitly permits and
  records governed risk acceptance" — read strictly: a definition's versioned
  `certification_profile.permit_accepted_risk` (default false, pinned on the
  run) must permit it, for any severity, not only BLOCKER; every unresolved
  exception must be accepted risk; and the accepted exposure must be
  IMMATERIAL in aggregate in every currency. A material or unassessable
  accepted risk is never certifiable here: accepting a material risk would
  need an authority tier above the acceptor (A9), so it needs a correction and
  a new run instead. When certified, the run is Certified / VARIANCE /
  CERTIFIED (never relabeled MATCH); the certification row stores
  `result_state` VARIANCE and the full assessment, and both are in the
  manifest hash (Invariant 9: "unresolved exceptions and impact assessment").
  This is the decision a reviewer is most likely to want to revisit: the
  reference service (financial-control-svc) certifies PASS_WITH_APPROVED_
  EXCEPTIONS over waivers without a profile switch; DQC-03 has no such result,
  and this service keeps VARIANCE and requires the switch.
- **B23. Materiality grades exceptions and is assessed in aggregate (A4).**
  `materiality` is per currency, in minor units at the amount scale
  (`{"currencies": {"USD": 100000}}`), replacing the currency-less
  `materiality_amount_minor_units` (which no new row writes); a currency
  without a threshold is UNASSESSED, never assumed immaterial. Severity is
  derived, never supplied, on DQC's scale, with financial-control-svc's bands:
  exposure ≥ threshold → BLOCKER (MATERIAL); ≥ threshold/10 → ERROR;
  otherwise WARNING; unassessed → ERROR. INFO is never derived. Exposure is the
  difference for an amount outside tolerance or a balance variance, and the
  whole item otherwise (|MinInt64| saturates). The aggregate (§11
  "individually and in aggregate", scenario 25) is per currency over
  unresolved exceptions: item exposures summed with a key-matched pair counted
  once, and a balance variance — itself an aggregate of those items —
  compared rather than added (the larger counts). It is computed on read from
  the exceptions (no stored copy to drift) and sealed into any certification.
  The bands and the aggregation basis are policy constants a reviewer may
  want on the definition instead.
- **B24. Governance is pinned, versioned and required.** `materiality`,
  `exception_policy` (owner role and SLA days for BLOCKER / ERROR / WARNING,
  1–365, all required) and `certification_profile` are definition rules:
  changing one bumps the version (B4), and each is copied onto the run and
  pinned from creation (trigger). Definitions and runs written before
  migration 000007 have none of them; such a definition cannot start a run and
  such a run cannot be matched or have exceptions raised (409, as for B9
  legacy records), because their exceptions could be neither owned nor graded.
  Migration 000007 renames exception statuses, adds the columns as NOT VALID
  requirements for new rows, and its down migration refuses while governed
  exception state, exception history or a certification over accepted risk
  exists.
- **B25. Verification of B21–B24.** Unit tests in `internal/domain` for every
  exception edge, the severity bands and the unassessed case, policy and
  request validation, root-cause and independence rules, the scenario-25
  aggregate with pair de-duplication, and the certification gate (no profile,
  material, unassessable, working, permitted-immaterial; VARIANCE certified
  only when the assessment allows); a handler test that accepting risk is
  checked against `RECONCILIATION_ACCEPT_RISK` on the run's entity and that a
  denial changes nothing. Integration tests: exceptions raised with
  classification, severity, materiality, exposure, owner role, SLA due date and
  derived attention (including a BLOCKER and an unassessed EUR item); the
  worked lifecycle with owner-only moves and evidence-required remediation
  that stays unresolved; accepted risk refused to the owner and the raiser,
  final, kept as VARIANCE, listed on the run, blocking certification without
  a profile and blocking a manual match; certification over permitted
  immaterial risk sealed as VARIANCE with every accepted exception; scenario
  25 blocking certification; a BLOCKER needing a root cause; manual-match
  closure recorded; direct SQL refused for rewriting severity or due date,
  undoing an acceptance, closing without a match, waiving without authority,
  making a VARIANCE eligible without the profile, widening a run's profile and
  rewriting exception history; and a profile change versioning the
  definition. Migration 000007 was round-tripped on a database holding
  exceptions, with the down refusal exercised and the status renames checked
  both ways.
