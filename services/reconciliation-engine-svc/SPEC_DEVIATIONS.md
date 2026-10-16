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
line holds (the service writes only its own tables). But the inputs a
reconciliation is supposed to prove things about — frozen, independently
identified populations with watermarks, currencies and versioned rules — are
much weaker than either spec requires, so a certification here proves less
than it claims. Roughly 50% of the named capabilities are implemented
faithfully. Four defects were fixed in this pass (B1–B4).

## A. Not implemented / weaker than the spec

- **A1. Populations are pushed by the caller, not drawn from their owners.**
  `POST /runs` takes the items of each side in the request body. There is no
  source watermark, query/spec version, source record id or declared control
  total, so a run cannot be reproduced and nothing proves the caller sent the
  whole population (ZS-SVC-Z-001 INV-25 "populations must be independently
  identifiable and reproducible", TC-04; ZS-CONTROL-001 §9 and Invariant 3).
  `content_hash` is a hash of whatever was posted. Scope: either pull
  populations through `control-population-contract.md` (the same client
  financial-control-svc uses, with watermark and `declared_totals`
  verification), or require the caller to supply source system, watermark and
  declared totals per side and verify them. This is the main gap.
- **A2. No currency anywhere.** Items, snapshots, tolerance and totals are
  bare `int64` minor units. Two sides in different currencies, or one side
  mixing currencies, are matched and summed as if they were the same unit
  (ZS-SVC-Z-001 INV-10, NP-22; TC-05 "every monetary total stores currency,
  scale and rounding"). Scope: currency on every item, per-currency snapshot
  totals, a currency-mismatch exception, tolerance per currency.
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
- **A10. No legal entity.** Definitions and runs carry no `legal_entity_id`;
  authorization is checked with the tenant id passed as the entity
  (`h.authorize(…, tenantID, …)`). Entity-scoped grants cannot be expressed.
- **A11. Definitions are upserted in place, without history or approval.**
  Re-posting a name overwrites its rules (the version now increments — B3 —
  but the previous rules are not kept, so a run's `definition_version` can be
  matched only against the copy frozen on the run itself). No approval step,
  no effective dating (ZS-SVC-Z-001 INV-01/22, ZS-CONTROL-001 Invariant 4).
  There is also no read endpoint for definitions.
- **A12. Certification is thin.** The sealed manifest records counts, the
  pinned tolerance/materiality and the definition version — not the
  population hashes, the manual matches with their reasons, or the certifier's
  authority (ZS-SVC-Z-001 INV-21, DQC-05 "certification binds exact
  evidence"; ZS-CONTROL-001 Invariant 9). No revocation.
- **A13. Supersession is unconstrained.** `SupersedeRun` accepts any existing
  run of the tenant as the replacement — a different definition, or a run that
  never completes — and does not record who superseded (`actor` is discarded).
  Only self-supersession is now refused (B4).
- **A14. `int64` totals can overflow silently** when a side's amounts are
  summed in `freezePopulation`; no overflow check.

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
- **B4. Redefining rules bumps the version; a run cannot supersede itself**
  (fixed). The upsert used to overwrite tolerance/materiality/keys without
  touching `version`, so every run and certification reported
  `definition_version: 1` whatever rules applied. The version now increments
  when the rules actually change (re-posting identical rules leaves it alone).
  `SupersedeRun` with `new_run_id` equal to the run id is refused.
- **B5. Verification of B2–B4.** The new rules in B1 and B3 are covered by
  unit tests in `internal/domain`. The SQL changes (sides from snapshots, the
  conditional version bump, the supersede guard) are covered by new
  integration tests in `internal/store/pg_store_test.go`, which need
  `TEST_DATABASE_URL` and were **not run** in this pass (skipped locally).
- **B6. Refused requests answer 400.** The new errors fall through the
  handler's existing default branch (400), like the service's other
  validation errors.
