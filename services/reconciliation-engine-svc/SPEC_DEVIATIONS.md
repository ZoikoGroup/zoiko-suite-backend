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
definitions and runs carry a legal entity (B9). What a run proves about those
populations is still weaker than the specs require: matching ignores the
declared key fields, currency and dimensions (A2, A3), tolerance is absolute
only (A5), and there is no result dimension or exception governance (A6, A7).
Defects fixed: B1–B4; Stage 1 closed A1, A10, A13 and A14 (B7–B12).

## A. Not implemented / weaker than the spec

- **A1. Closed** — populations are read from their owning services; see B7.
- **A2. Currency is stored but not used.** Since B7, every item carries the
  currency its source delivered and every snapshot has per-currency control
  totals. Matching, tolerance and the certification total still ignore it:
  two items in different currencies with the same reference and amount match,
  and `total_amount_minor_units` (snapshot) and `total_asserted_minor_units`
  (certification) still add different currencies together (ZS-SVC-Z-001
  INV-10, NP-22). Scope: a currency-mismatch exception, tolerance per currency,
  no cross-currency total anywhere.
- **A3. `match_key_fields` is stored but ignored.** Matching pairs items on
  `ref_id` only; the definition's declared key fields and item `dimensions`
  are never used. A reader of a definition is told something about the rule
  that is not true. Composite-key matching (ZS-CONTROL-001 §10 "composite
  deterministic match") and dimensional balance (DQC-03 "no aggregate-only
  pass when a material sub-dimension is out of balance", INV-08) are absent.
- **A4. Materiality is stored but never used.** It is copied onto runs and the
  certification manifest, and nothing reads it: no severity, no aggregate
  assessment (ZS-CONTROL-001 §11 aggregation, scenario 25).
- **A5. Only absolute tolerance.** No percentage, date or rounding tolerance
  (§11); `occurred_at` is never compared, so timing differences are invisible
  and there is no expected-clearing-date reconciling item (§10).
- **A6. No result dimension.** DQC-03 defines MATCH / VARIANCE /
  INDETERMINATE / PARTIAL / SUPERSEDED; ZS-CONTROL-001 §7 separates lifecycle,
  result, exception and certification. Here a single `status` carries
  everything, there is no INDETERMINATE or PARTIAL outcome, and the `Failed`
  status in the DATA-07 lifecycle is never set by any code path.
- **A7. Exceptions have no governance fields or lifecycle.** An exception has
  free-text `reason`, `amount`, `Open/Resolved`. Missing per ZS-CONTROL-001
  Invariant 7 and §21: owner, severity, category/assertion, due date/SLA,
  aging, root cause. The only way to resolve one is a manual match that
  consumes its item; there is no waiver/accepted-risk path (DQC-05 "accepted
  risk never changes a FAIL into PASS"). An item that is genuinely unmatched
  because the source is wrong can therefore never be closed in its run: the
  run can only be superseded.
- **A8. Reperform does nothing useful.** `Reperform` re-runs auto-matching on
  items with neither a match nor an exception — after the first `Match` there
  are none, so it only changes the status to `Reperformed`. Because
  populations are frozen in the run and remediation happens in the owning
  domain, a meaningful reperformance is a new run over new populations,
  linked to this one. Scope: define reperformance as "start a linked run and
  supersede on certification", or remove the command.
- **A9. No maker-checker or separated permissions.** Every command uses one
  action, `RECONCILIATION_MANAGE`, so the same principal can define the
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
  pinned tolerance/materiality and the definition version — not the
  population hashes, the manual matches with their reasons, or the certifier's
  authority (ZS-SVC-Z-001 INV-21, DQC-05 "certification binds exact
  evidence"; ZS-CONTROL-001 Invariant 9). No revocation.
- **A13. Closed** — supersession is constrained and records its actor; see B10.
- **A14. Closed** — population totals are overflow-checked; see B11.

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

  *Interim failure posture.* A population that cannot be proven complete
  starts **no run**: the request answers 502 (source unreachable) or 422
  (anything else) with `"result": "INDETERMINATE"` and the reason, and nothing
  is frozen. A durable Failed/INDETERMINATE run needs the result dimension
  (A6). The read runs on a context detached from the request (3-minute bound;
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
