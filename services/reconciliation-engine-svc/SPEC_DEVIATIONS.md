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
(B21–B24). Rules are immutable, digest-sealed rule versions, approved by
someone other than their author and in effect from their effective date; a
run pins the version in effect (B26), and a reperformance re-reads the run's
populations through the same contract, freezing the re-read as a new snapshot
generation beside the immutable original (B30). Still missing: rounding and
business-day tolerance and reconciling items (A5), maker-checker beyond
accepting risk and approving rules (A9), recurrence and
qualitative materiality (A4 remainder),
and two questions the specs leave open: how absolute and percentage
tolerance combine (A15) and what defines a PARTIAL result (A16). Defects
fixed: B1–B4; Stage 1 closed A1, A10, A13 and A14 (B7–B12); Stage 2
closed A2 and A3 and most of A5 (B13–B17); Stage 3 closed A6 (B18–B20) and A7
and most of A4 (B21–B25); Stage 4 closed A11 (B26–B27); B30–B31 close A8;
B32–B33 close A12's manifest part and B34–B35 its revocation part.

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
- **A8. Closed** — `Reperform` re-reads the run's populations through the same
  control-population path `StartRun` uses and re-tests the run against them,
  freezing the re-read as a new snapshot generation beside the immutable
  original; see B30–B31.
- **A9. Certification: Closed** — certifying is its own action and a
  preparer cannot certify; see B28. Accepting risk (B22) and approving a rule
  version (B26) were already separated. Define, StartRun, Match (including
  manual matches) and Reperform still share `RECONCILIATION_MANAGE`, by
  design: they are preparation steps, not a certifying authority, so no
  independence requirement applies between them — one principal may define,
  run and match. ZS-CONTROL-001 Invariant 12 is about the preparer certifying
  their own work, which B28 refuses, and DATA-07's "rule publication
  separated from … approval" is B26. RaiseException, assign and supersede
  also stay on MANAGE. Not built: a policy switch for "where maker-checker
  policy applies" — it applies to every certification here — and dynamic SoD
  revalidated against authorization-svc beyond the action check.
- **A10. Closed** — definitions and runs carry `legal_entity_id`; see B9.
- **A11. Closed** — rule versions with independent approval and effective
  dating, and definition reads; see B26.
- **A12. Certification is thin. — Closed: manifest (B32), revocation
  (B34).** (ZS-SVC-Z-001 INV-21, DQC-05 "certification binds exact
  evidence"; ZS-CONTROL-001 Invariant 9.) The sealed manifest already recorded
  counts, the pinned tolerance and materiality, the rule version and its
  digest (B26), and the result and the assessment with every accepted risk
  (B22). Migration 000010 adds three things. Each is stored on the
  `certifications` row and inside the manifest hash, and is served by `GET
  …/runs/{id}/certification` and the `CONTROL.Certified` event:
  1. *Populations.* The content hash of every side of the generation that
     was certified. That is the run's latest generation (A8), named by
     snapshot id, with the spec, watermark, item count and declared totals
     it was frozen against.
  2. *Manual matches.* Every manual match of the run, with its reason, who
     made it, and the exceptions it resolved. Each also carries the
     structured evidence recorded when the match was made: both frozen items,
     their generation, and the comparison under the pinned tolerance.
  3. *Authority.* The certifier's authority: authorization-svc's own
     decision (`access_decision_id`, `decision_basis`, `policy_set_version`),
     asked live for this certification. A grant with no traceable decision
     fails closed (503), and nothing is certified.

  **Revocation (B34).** `POST …/runs/{id}:revoke-certification` moves a
  Certified run's certification CERTIFIED → REVOKED. The status stays
  Certified and the result is untouched. The request needs a reason and at
  least one evidence reference (NP-47), and its own action,
  `RECONCILIATION_REVOKE`. No principal recorded as certifying the run may
  revoke it. The sealed certification is retained untouched, with its
  manifest hash and everything 1–3 above bound into it. An append-only
  `reconciliation_certification_revocations` row names the certification
  and hash it revokes, and records the revoker's own authorization decision.
  `GET …/certification` serves the certification with its revocation beside
  it. This is financial-control-svc's revocation (its A1/B6), ported.
  ***Deployment prerequisite, outside this service:***
  `RECONCILIATION_REVOKE` is not registered in access-control-svc, so it
  cannot be granted yet (B34). Neither are `RECONCILIATION_CERTIFY`,
  `RECONCILIATION_ACCEPT_RISK` or `RECONCILIATION_APPROVE_RULES`.

  **Not built:**
  1. No backfill. A certification sealed, or a manual match made, before
     000010 carries none of this, and none is reconstructed.
  2. The manifest is still stored only as its SHA-256. The fields it covers
     are stored beside it, but nothing re-derives the hash to verify it.
  3. The certification is not signed. DQC-05 says "signed/immutable"; it is
     immutable (`reject_immutable_row`), not signed.
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
  CERTIFIED / SUPERSEDED` (REVOKED was left out here: revocation was A12,
  out of scope, and adding a state nothing can reach would only pretend;
  B34 adds it with the command that reaches it). Rules
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
  (A8 was open at that point; a reperformance now re-reads the populations and
  records what changed — B30). Existing rows were backfilled from their status and open
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
- **B26. Rule versions with independent approval and effective dating
  (closes A11).** Copied from financial-control-svc's control rule versions
  (`CreateRuleVersion` / `ApproveRuleVersion`). A definition row is now its
  identity only — tenant, legal entity, name — and is immutable (trigger); its
  rules are an append-only series in `reconciliation_rule_versions`, each a
  complete rule document (populations, match key, balance dimensions, amount
  scale, tolerance, materiality, exception policy, certification profile)
  sealed with the SHA-256 of its canonical encoding. Endpoints:
  `POST /definitions` creates the definition and its rule version 1 as a
  draft (a name already defined in the entity is now refused with 409 —
  rules are never overwritten in place); `GET /definitions/{id}` returns the
  definition with the rules *in effect*, their version, digest and effective
  date, and `latest_rule_version`; `POST /definitions/{id}/rule-versions`
  publishes a draft and must carry `based_on_version` = the latest version
  (DATA-07 "optimistic version checks on … rule … changes"; stale → 409) and a
  reason; `GET /definitions/{id}/rule-versions` lists the history;
  `POST /definitions/{id}/rule-versions/{version}/approve` approves a draft.
  Approval needs the separate action `RECONCILIATION_APPROVE_RULES`, checked on
  the definition's entity at the moment of approval (DATA-07 "rule
  publication separated from evaluation/release approval"; ZS-CONTROL-001
  Invariant 11), may not be given by the version's author (domain check and a
  CHECK constraint), happens once, and makes the version immutable (trigger;
  versions are never deleted). The version *in effect* is the approved one
  with the latest `effective_from` that is not in the future (the database's
  current date), and among equal dates the highest version; StartRun refuses a
  definition with none (409, before any source is read), pins that version's
  number as `definition_version` and its digest as `rules_sha256` (pinned by
  trigger; also in the certification manifest), and is refused if another
  version took effect while its populations were read (ErrDefinitionChanged,
  as before). Two deviations from the reference a reviewer may want to undo:
  (a) the reference picks the highest approved version among those
  effective, whatever its date; here the latest effective *date* governs, so
  a version can be scheduled ahead without displacing the one in force — and,
  so that no approved version is silently unreachable, a version dated before
  the version in effect is refused at creation and at approval (409); (b) the
  effective date is evaluated when a run starts, not against the run's
  period, as in the reference — the specs do not say which. Every load
  re-hashes the stored rules and refuses a version whose digest no longer
  matches (tamper evidence below the trigger). Definitions from before
  migration 000008 get **no** rule version: their rules were never approved
  and an approval cannot be invented, so they cannot run until a version is
  published (numbering continues above their old in-place version) and
  approved by someone else — which is also the way out for the pre-000007
  definitions B24 made unrunnable. Runs already started keep what they pinned.
  Version-change events: `CONTROL.RuleVersionPublished`,
  `CONTROL.RuleVersionApproved` (names mirror the reference; DATA-07 lists no
  definition events).
- **B27. Verification of B26.** Unit tests in `internal/domain` for the
  canonical digest (absent vs. empty collections, map order, a JSON round
  trip, a rule change), request validation and runnability; handler tests
  that approval is checked against `RECONCILIATION_APPROVE_RULES` on the
  definition's entity and a denial approves nothing, that a definition read
  is authorized on its entity, and that a definition with no approved rules
  starts nothing and reads no source. Integration tests: a draft-only
  definition cannot run; self-approval, double approval and approving a
  missing version are refused; a run pins the approved digest; a draft does
  not displace the version in effect and a stale `based_on_version` is
  refused; a future-dated version does not take effect early, a backdated one
  is refused, and a same-day one takes over once approved; history is kept
  with distinct digests and a run keeps the version it started under; a
  pre-000008 definition's numbering continues; direct SQL refused for editing
  an approved or draft version, self-approving, deleting a version, editing a
  definition row and changing a run's pinned digest; tampering below the
  trigger caught by the digest on read; rule versions tenant-isolated. Tests
  that relied on in-place redefinition now publish and approve versions; the
  redefinition test now asserts the refusal. Migration 000008 was applied over
  a 000007 database holding a definition with in-place rules (left untouched,
  given no version), round-tripped, and its down refusal exercised.
- **B28. A preparer cannot certify (closes A9's certification part).**
  ZS-CONTROL-001 Invariant 12 "A preparer cannot self-certify where
  maker-checker policy applies". Certify needs the separate action
  `RECONCILIATION_CERTIFY` (handler; `RECONCILIATION_MANAGE` is no longer
  enough), checked on the run's entity. Under the run's row lock, after the
  eligibility checks and before the idempotency claim, the store refuses
  (`ErrNotIndependentCertifier`, 403) a certifier who is any of the run's
  preparers: `started_by`, any `match_results.matched_by` (auto or manual —
  it is the principal who ran Match), and the actor of the run's LIFECYCLE
  transition out of Planned (the first Match) or into Reperformed. Every
  preparer counts, not only whoever acted last, so a trivial final step by
  someone else does not clear the original preparer. Raising an exception and
  accepting risk are findings about the work, not preparing it, so those
  actors may certify. No migration: every preparer was already recorded
  (000001 `started_by`, `matched_by`; 000006 transition `actor`). A8 did not
  replace Reperform with a linked run, so the preparer set is unchanged by it;
  were that ever done, the preparer query must follow it.
- **B29. Verification of B28.** A unit test in `internal/domain` that a
  preparer — including an earlier one, not only the last — is refused and
  another principal is not; a handler test that Certify is checked against
  `RECONCILIATION_CERTIFY` on the run's entity and that a denial certifies
  nothing. Integration test: on a run started by one principal, matched by
  a second and reperformed by a third, each of the three is refused and the
  run stays uncertified; a fourth principal certifies and is recorded as
  `sealed_by`. Tests that certified as the run's own operator now certify as
  a separate principal.
- **B30. Reperformance re-reads its populations (closes A8).** `Reperform` no
  longer re-runs matching over items the first `Match` already consumed. It
  re-reads BOTH of the run's populations through the same control-population
  path `StartRun` uses — `source.BuildPopulations` over the registered source
  systems, the run's own scope (`period_id`, entity, principal forwarded to the
  source) and the spec params the definition named, whose canonical identity
  (`BANK/bank-transactions`, `GL/account-postings?account_codes=1100`) is
  recorded as the snapshot's `spec_ref` — and freezes what it pulled as the
  run's next snapshot generation. Migration 000009 replaces
  `UNIQUE (run_id, side)` with `UNIQUE (run_id, side, generation)`: an earlier
  generation is immutable and stays beside the new one, so the history shows
  both what the run was first tested against and what it was re-tested
  against, and a re-read never edits a frozen population. Matching, the
  population and per-dimension balance checks, the `RaiseException` item fetch,
  manual matches and the certification's side control totals all read the
  LATEST generation only (`generation = (SELECT MAX(generation) …)`), so a
  reperformance cannot re-test old populations or add an old generation's
  totals to the ones being certified. The re-read runs under the rules the RUN
  pinned — `GetRuleVersion` at the run's `definition_version`, re-verified
  against the run's `rules_sha256` — so a definition edited since the run
  started cannot change what the run re-tests; the read and the persist are
  StartRun's two-phase shape (detached read context under
  `populationReadTimeout`, then the write under `persistTimeout`), and the run
  moves to Reperformed. A material difference against the generation the
  re-read supersedes — item content hash, declared row count or declared
  totals — is recorded in the Reperformed transition's reason, append-only
  history read at `GET /runs/{id}/transitions`, never silently absorbed:
  `reperformed against generation 2 (populations changed since generation 1:
  Ledger content hash f605c088→97f36497, declared row count 3→5, declared
  totals USD 60.00→106.00): 6 exception(s) being worked, 0 accepted as risk`.
  Unchanged content is named as such ("populations unchanged from generation
  1"), so "read again, same content" stays distinguishable from "not read
  again", and a moved watermark with unchanged content is mentioned on the
  same principle. No new exception kind was introduced: under A7/B21 an
  unclosable kind would block certification forever, and a re-read that found
  a difference is not by itself a finding about the data — the note is
  evidence, and the re-match's own ITEM and BALANCE exceptions are the
  findings. Three consequences a reader may want to reverse:
  (1) a re-read that cannot be proven complete and reproducible — unreachable,
  unregistered, inconsistent with its declared totals, too large, unreadable —
  freezes nothing and fails the command with StartRun's own classification
  (502 `SOURCE_UNAVAILABLE`, 422 otherwise, with `failure_code` in the body),
  leaving the run's state untouched and recording no attempt. Unlike StartRun
  (B19) there is no Failed run: nothing about the data was obtained, and a
  Failed run would be a durable claim about populations that were never read;
  (2) exceptions raised for an earlier generation stay Open and keep blocking
  certification — they close only through a manual match (B1/B21), and a
  reperformance is not a correction — while the re-match raises the current
  generation's own exceptions. A ref that now matches still leaves its earlier
  exception Open; one that now differs raises a fresh exception beside it
  (per-item uniqueness is `UNIQUE (item_id)`, so the new generation's item is
  a new exception). After a re-read of a broken run the run is therefore still
  uncertifiable until the earlier exceptions are matched or risk-accepted —
  the deliberate answer to "certify over what?";
  (3) the command is open only to Running and ExceptionsOpen: a Planned run has
  not been evaluated, and a Reperformed one already has a current generation,
  so re-reperforming after further remediation goes through `Match` first
  (which is what raises the new evidence). A legacy run (no pinned tolerance or
  digest, pre-000008) is refused rather than re-tested under rules it never
  pinned.
- **B31. Verification of B30.** Unit tests in `internal/store`
  (`matching_rules_test.go`) — a reperformance of an open run reports
  "populations unchanged from generation 1" and leaves it Reperformed with both
  generations present; a population changed between StartRun and Reperform
  freezes a second generation whose hash differs for the changed side and is
  identical for the untouched one, raises the new generation's ITEM exception,
  and the transition reason names the changed side only ("populations changed
  since generation 1: Ledger content hash …"). Handler test: a re-read that
  cannot be proven complete is refused 502/422 with `reperformCalls == 0`, so
  nothing is frozen and nothing is recorded. Migration 000009 was round-tripped
  on a database holding one generation per side (down drops the column and
  restores `UNIQUE (run_id, side)`, both snapshots become generation 1
  unchanged; up re-applies), and its refusal was exercised on a database
  holding two generations of a side: down refuses with "cannot remove snapshot
  generations while a run holds more than one generation of a side", leaving
  the schema and both generations untouched. Live check (this change;
  containers only — Postgres 16 in a container, the engine against it, a stub
  population source implementing the control-population contract over a mutable
  dataset, and a stub authorization service answering GRANTED): StartRun froze
  Bank (4 items, USD 100.00) and Ledger (3 items, USD 60.00) as generation 1
  and `:match` raised the Bank-only D ITEM exception and the USD balance
  exception (ExceptionsOpen). The Ledger dataset was then mutated (C 30.00 →
  31.00, D and E added, watermark gl-v1 → gl-v2) and `:reperform` returned
  Reperformed with the reason quoted in B30;
  `GET /runs/{id}/populations` showed four snapshots — Bank and Ledger,
  generations 1 and 2 — the Bank hash identical across generations and the
  Ledger hash changed; the re-match found A, B and D on generation 2 (D could
  only match against the re-read) and raised ITEM exceptions for C (both sides,
  the 1.00 difference) and E, while the earlier exceptions stayed Open, so
  `:certify` was still refused (409, Open exceptions remain) and a second
  `:reperform` was refused (409, not open). Every read the engine made used the
  identical path and query — `…/account-postings?account_codes=1100&…&period_id=2026-08`,
  the stub answering 400 for a read without `account_codes` — so the refetch
  carried the same spec, not a lookalike. A source answering 503 and one whose
  declared totals did not describe its records were both refused live (502
  `SOURCE_UNAVAILABLE`, 422 `SOURCE_INCONSISTENT`) with the run's snapshots,
  status and transition history unchanged, and after the source was restored
  the same run reperformed and reported "populations unchanged from generation
  1".
- **B32. What a certification binds (closes A12's manifest part).** The
  shape mirrors financial-control-svc's ControlCertification (its A2/B7),
  applied to this service's existing `certifications` record instead of a
  new table. Migration 000010 adds `authority_snapshot`, `populations` and
  `manual_matches` to `certifications`, and `evidence` to `match_results`.
  Both tables are already immutable (`reject_immutable_row`), so each column
  is written once, on insert. The new CHECKs are `NOT VALID`, so rows from
  before 000010 stay as they are, and every new row must satisfy them.
  - *Authority.* `authz.Client.Decide` calls authorization-svc's
    `/v1/authorize` live and returns its recorded decision:
    `access_decision_id` (the key of its append-only AccessDecisionLog),
    `decision`, `decision_basis`, `policy_set_version` and `reason_codes`.
    The client records the principal, entity, action and the time the
    answer arrived.
    - `Certify` now authorizes `RECONCILIATION_CERTIFY` on the run's entity
      through `Decide` rather than `CheckAllowed`, and hands the snapshot to
      the store.
    - A GRANTED answer with no `access_decision_id` cannot be traced, so it
      is refused as unavailable (503).
    - The store also refuses (`ErrAuthorityUntraceable`, 503) a snapshot
      that is missing, untraceable, not GRANTED or another principal's. It
      checks this before taking the run's lock or the idempotency key.
    - The table CHECK requires `access_decision_id`, `decision_outcome =
      GRANTED`, and `principal_id = sealed_by`.

    Every other command still uses `CheckAllowed`; only certification
    records its authority.
  - *A defect fixed on the way.* The authz client posted to
    `/v1/authorization/check`, a route authorization-svc does not register.
    Every check got a 404, which the client turned into a denial. That is
    the defect already fixed in data-quality-svc, data-lineage-svc,
    global-search-svc and semantic-model-svc. `CheckAllowed` and `Decide`
    now share one `/v1/authorize` call. A transport, status or decoding
    failure is now `ErrAuthorizationUnavailable` (503) instead of a 403
    denial. The client still caches nothing.
  - *Populations.* Every side of the run's latest snapshot generation: the
    generation whose control totals the certification already used, and the
    one matching ran against (A8). Each side is recorded by `snapshot_id`,
    `generation` and `content_hash`, with `source_system`, `spec_ref`,
    `source_watermark`, `item_count`, `declared_row_count` and
    `declared_totals`. Earlier generations are history, not what was
    certified, so they are not listed. A run with no frozen population
    cannot be certified.
  - *Manual matches: what was already recorded, and what was missing.* A
    manual match was already its own immutable `match_results` row:
    `manual = true`, the reason (required by a CHECK), `matched_by` and
    `matched_at`. Its exceptions name it in `resolved_by_match_id`, and B1
    already refused a pairing outside the pinned tolerance. So nothing was
    lost or overwritten, and the reason is prose by design: it is the
    operator's justification. intercompany-accounting-svc's A3 had a
    different problem: a machine-determined outcome kept only as an
    overwritten sentence. What was missing here was §10's "evidence" beside
    the reason: what the pairing was checked against.
  - *Manual matches: the evidence column.* `match_results.evidence` is now
    written in the transaction that applies the match, never reconstructed
    later (`domain.NewManualMatchEvidence`, required on manual rows by a
    CHECK). It holds:
    - both frozen items (id, side, source record, key, amount, currency,
      date) and their snapshot generation;
    - `key_overridden`;
    - the currency, the difference against the allowed amount, and the day
      difference against the allowed days when the policy has a date
      tolerance;
    - the pinned tolerance policy itself.

    The figures are computed exactly as `Compare` computes them, including
    the percentage base side. The certification lists every manual match of
    the run, of any generation, because an earlier generation's match closed
    exceptions the assessment counts. Each is listed with its reason,
    matcher, time, evidence (null for a pre-000010 match) and the exceptions
    it resolved. A run with none records an empty list.
  - *Manifest.* `certificationManifest` gains `populations`,
    `manual_matches`, `certifier` and `authority_snapshot`, so
    `manifest_sha256` now covers them. The `CONTROL.Certified` event payload
    is the certification row, so it carries all three.
  - *Rollback.* 000010's down refuses while any certification records an
    authority or any manual match records evidence. Dropping either would
    leave a manifest hash covering fields that no longer exist. Otherwise it
    drops the constraints and columns. This is the same posture as
    000007's and 000009's downs.
- **B33. Verification of B32.**
  - *Unit tests.*
    - `internal/domain`: the evidence builder's figures, including the
      percentage of the base side, the day difference and no date figures
      without a date tolerance; and `Traceable`.
    - `internal/authz`: both calls reach `/v1/authorize`; `Decide` returns
      the recorded decision and asks again each time; a denial, a grant with
      no access decision, a 503 and an unreachable server all fail closed.
    - `internal/handler`: Certify hands the store the certifier's own
      decision for `RECONCILIATION_CERTIFY` on the run's entity, and an
      untraceable grant answers 503 and certifies nothing.
  - *Integration tests* (`internal/store/certification_evidence_test.go`):
    - A manual match of two differently keyed items, 1003 against 1000 under
      a 5-unit tolerance, records its evidence (difference 3, allowed 5, key
      overridden, generation 1) on `GET …/matches`.
    - The certification lists both generation-1 snapshots by id and hash,
      the manual match with its reason, matcher, evidence and both resolved
      exceptions, and the certifier's decision. `GetCertification` reads the
      same back.
    - A direct insert whose authority lacks its decision id, or names
      another principal, breaks `certifications_evidence_check`.
    - After a reperformance, the certified hashes are generation 2's and
      differ from generation 1's.
    - A missing, untraceable, denied or other principal's authority is
      refused, and the run stays uncertified.
    - 000010's down round-trips on an empty database and refuses once a
      manual match has recorded evidence, leaving it intact.
  - *Existing tests* now pass the certifier's grant to `Certify`.
  - *Suite.* The full suite ran against a fresh, disposable Postgres 16
    container and passed.
- **B34. How a certification is revoked (closes A12's revocation part).**
  The shape is financial-control-svc's (c4ca5347, its A1/B6, and the
  rollback guard of 9bec6e69), applied to this service's state model and
  A12 record.
  - *State.* `CertRevoked` joins the certification dimension, with the edge
    CERTIFIED → {REVOKED, SUPERSEDED}. REVOKED has no outgoing edge, so a
    second revocation is refused.
    - `domain.ValidateCertificationRevocation` admits a revocation only on a
      Certified run whose certification is CERTIFIED. A superseded run's
      certification (SUPERSEDED) cannot be revoked.
    - `ValidateLifecycleTransition` lets a Certified run's certification be
      REVOKED without a status move. A run never *enters* Certified as
      REVOKED, and REVOKED needs status Certified or Superseded.
    - A revoked run can still be superseded (`SupersedeRun` is unchanged).
      It keeps REVOKED, because only CERTIFIED becomes SUPERSEDED.
    - The status (lifecycle) does not move, as in financial-control-svc's
      orthogonal model: the run was certified, and that history does not
      change.
  - *Command.* `POST /v1/reconciliation/runs/{runID}:revoke-certification`,
    with body `{"reason": …, "evidence_refs": […]}`. This service's
    `{runID}:verb` form is used, as for `:certify` and `:supersede`.
    - Both fields are mandatory (NP-47; §27 "reason, evidence"). The reason
      is 1–2000 characters. There are 1–20 evidence references of 1–500
      characters each, the same bound as an exception's `evidence_ref`.
      Unknown fields are refused, so a caller who sends the singular
      `evidence_ref` gets a 400 rather than a revocation without evidence.
    - Authority is the new action `RECONCILIATION_REVOKE` on the run's
      entity. It is distinct from `RECONCILIATION_CERTIFY`: the power to
      seal a run is not the power to withdraw a seal. The handler asks
      authorization-svc through `Decide`, as `Certify` does (B32), so the
      decision is recorded. An untraceable grant is refused with 503
      (`ErrRevokerAuthorityUntraceable`) and nothing is revoked.
    - The answer is 201 with the revocation record.
  - *Store* (`store.RevokeCertification`, one transaction, under the run's
    `FOR UPDATE` lock):
    1. Peek at the idempotency key. A retry of a completed revocation
       replays (200, `Idempotent-Replayed`) instead of being refused as
       already REVOKED. The same key with another body is 409.
    2. Validate the revocation.
    3. Load the sealed certification, and every CERTIFICATION → CERTIFIED
       transition of the run.
    4. Check independence (`domain.CheckRevokerIndependence`, the same
       shape as `CheckCertifierIndependence`). The revoker must not be any
       recorded certifier: the certification's `sealed_by`, the run's
       `certified_by`, and every actor of those transitions, not only the
       last.
    5. Claim the key. A refused revocation never consumes it.
    6. Insert the revocation row.
    7. Move the run through `advanceRun`, the one mechanism every state
       change uses. It validates the dimensions and appends one
       `CERTIFICATION: CERTIFIED → REVOKED` row to
       `reconciliation_run_transitions`, exactly as Certify's and
       Reperform's moves are recorded.
    8. Emit `CONTROL.CertificationRevoked`, whose payload is the revocation
       record: the prior certification id, the reason, the evidence and the
       effective time (`revoked_at`). These are DQC-05's
       `dqc.certification.revoked` fields.

    Prospective (RB-05): nothing already decided on the certification is
    undone.
  - *What a revocation's evidence is.* A12 made "evidence" in this service
    mean what a certification binds: populations, manual matches and
    authority. A revocation therefore records three things:
    - the caller's `evidence_refs`: the new material evidence (DQC-05);
    - exactly what it revokes: `certification_id`, the certification's
      `manifest_sha256` as `revoked_manifest_sha256`, and the CERTIFIED
      transition, with `certified_by` and `certified_at`;
    - the revoker's authority: authorization-svc's decision, as the
      certification records its certifier's.

    This last item is the one addition to financial-control-svc's shape,
    whose revocation does not record an authority decision.
  - *Prior certification retained* (§27, DQC-05). The `certifications` row
    is never written: it is immutable by trigger, and the store only reads
    it. The test compares the stored row before and after, byte for byte.
    Its populations, manual matches, authority and manifest hash survive the
    revocation, and so does the CERTIFIED transition. `GET
    …/runs/{id}/certification` serves the certification unchanged, with
    `revocation` beside it, so a reader cannot mistake it for one still in
    force. The run's own `certified_by` and `certified_at` are unchanged.
  - *Database* (migration 000011):
    - The `certification_state` vocabulary gains REVOKED.
    - 000007's `state_dimensions_check` splits "Certified ⇔ CERTIFIED" in
      three: Certified ⇒ CERTIFIED or REVOKED; CERTIFIED ⇒ Certified;
      REVOKED ⇒ Certified or Superseded.
    - The lifecycle trigger admits CERTIFIED → REVOKED. A Certified run
      may then change its certification state only, CERTIFIED → REVOKED,
      and nothing else (the row is compared whole), and only once its
      revocation record exists in the transaction.
    - A deferred constraint trigger refuses, at commit, a revocation record
      whose run is not REVOKED. So the record and the state exist only
      together.
    - `reconciliation_certification_revocations` has one row per run and
      per certification. It is append-only (`reject_immutable_row`), with
      RLS forced. Its named CHECKs mirror financial-control-svc's:
      - `independence_check`: `revoked_by <> certified_by`;
      - `reason_check`: a non-blank reason;
      - `evidence_refs_check`: at least one evidence reference, none NULL
        or blank;
      - `authority_check`: a GRANTED decision with an access decision id,
        of `revoked_by`;
      - `prospective_check`: `revoked_at >= certified_at`.
  - *Rollback.* 000011's down follows 9bec6e69's pattern. It refuses while
    any revocation is recorded, or any run is REVOKED. Otherwise it restores
    the 000006/000007 definitions exactly. Dropping the table would lose who
    revoked what, why and by what authority, with no way to reconstruct
    it. Rolling the CHECK back would require re-certifying a revoked run,
    which undoes the revocation.
  - ***Deployment prerequisite, outside this service.*** access-control-svc
    validates every grantable action against its `permission_definitions`
    registry (its 000008), and new actions are registered there by
    migration. financial-control-svc's `FINCTRL_REVOKE` needed exactly that
    (access-control-svc 000017). The same applies here, and more widely:
    - the registry holds `RECONCILIATION_MANAGE` and `RECONCILIATION_READ`
      only;
    - it does not hold `RECONCILIATION_REVOKE`, nor this service's existing
      `RECONCILIATION_CERTIFY`, `RECONCILIATION_ACCEPT_RISK` or
      `RECONCILIATION_APPROVE_RULES`;
    - `deployments/scripts/seed-demo-rbac.ps1` grants no `RECONCILIATION_*`
      action.

    Until they are registered and granted, revocation, certification, risk
    acceptance and rule approval are all denied (403, fail-closed). Both
    files are outside this service and were not changed here.
  - *Not changed.* Nothing in this service consumes certification state
    downstream, so there is no gate to update (financial-control-svc's
    close gate has no counterpart here). A REVOKED run that is still
    Certified remains a terminal run for `SupersedeRun`'s replacement check.
- **B35. Verification of B34.**
  - *Unit tests.*
    - `internal/domain`:
      - the new edges (CERTIFIED → REVOKED; nothing out of REVOKED;
        ELIGIBLE and SUPERSEDED cannot become REVOKED);
      - the lifecycle rules (Certified stays Certified when revoked; a
        revoked run supersedes; REVOKED cannot be entered from Reperformed
        or held by an open run);
      - `ValidateCertificationRevocation`;
      - `CheckRevokerIndependence` refuses an earlier certifier, not only
        the last;
      - the request's reason and evidence bounds.
    - `internal/handler`:
      - the revoke route authorizes `RECONCILIATION_REVOKE` on the run's
        entity and hands the store that decision;
      - only CERTIFY is 403; an untraceable grant is 503; the store's
        independence refusal is 403;
      - an empty body, missing reason, missing or blank evidence, or an
        unknown field is 400 before anything is authorized.
  - *Integration tests* (`internal/store/revocation_test.go`, app role, so
    RLS applies):
    - A revocation records the certification id and manifest hash it
      revokes, the certifier, the reason, the evidence and the revoker's
      decision. The run is Certified / MATCH / REVOKED, with the same
      certifier and certification time.
    - The history is unchanged, plus one appended CERTIFICATION move.
    - The certifications row is byte-identical, and the served
      certification is identical apart from its new `revocation`.
    - The event carries the prior certification, reason, evidence and
      effective time.
    - A retry replays; the same key with another request is refused; a
      second revocation is refused; another tenant sees neither the run nor
      the record.
    - The sealer and an earlier recorded certifier are both refused, and
      neither refusal consumes its key.
    - An open run, an untraceable, denied or other principal's authority,
      or a missing reason or evidence is refused, and the run stays
      CERTIFIED.
    - A revoked run supersedes and stays REVOKED, still served with its
      revocation.
    - The database refuses each of these: REVOKED with no record; a record
      with no revocation (at commit); a revocation that also changes
      `certified_by`; each named CHECK; UPDATE and DELETE of a record;
      re-certifying a revoked run.
    - 000011's down round-trips with a certified, unrevoked run, and
      refuses once a revocation exists, leaving it intact.
  - *Rollback guard, verified by hand.* On a fresh, disposable Postgres 16
    container, every up file was applied with `psql --single-transaction -v
    ON_ERROR_STOP=1 -f`, the semantics a migration runner uses.
    - With no revocation, the down applied the same way exited 0. It
      dropped the table and restored the four-state CHECK, and the up
      re-applied cleanly.
    - With a run that was revoked and then superseded, the down exited 3:
      "cannot roll back 000011: recorded certification revocations exist",
      raised at line 18.
    - After that refusal, the revocation row, the REVOKED run, the
      five-state CHECK, the trigger's revocation branch and both new
      functions were all still present. Nothing was partially applied.
  - *Suite.* The full suite (`go test -count=1 -p 1 ./...`, with
    `TEST_DATABASE_URL` set) ran against a fresh, disposable Postgres 16
    container. All 131 tests passed, and none was skipped.
