# Spec deviations — financial-control-svc (ZS-CONTROL-001)

Governing documents: ZS-CONTROL-001 (Global Financial Reconciliation & Control
Framework) and `docs/architecture/control-population-contract.md`, which this
service **defines** for every source service. Earlier detail lives in
`docs/architecture/financial-control-closure.md` (blocker register for the 25
blocked catalogue controls, GL caller fields, event-envelope decision) and
`financial-control-wave5.md` / `-wave7.md` / `-wave8.md`. This file does not
repeat the per-control blocker register; it records the gaps in the service's
own command, state and evidence model.

Overall: the §25 command/query surface, the §7 orthogonal state model, the §26
event set, population freeze/hash, deterministic matching, exception lifecycle,
maker-checker certification, close gate, monitoring and evidence export are
built. Roughly 80% of the named capabilities are implemented; the gaps below are
mostly about what happens *after* certification and about governance of the
policies a run pins.

## A. Not implemented / weaker than the spec

- **A1. A certification cannot be revoked.** `CertRevoked` and the
  `CERTIFIED → REVOKED` edge exist in `internal/domain/state.go`, but no command
  reaches them. Today the only way out of CERTIFIED is a new run with
  `prior_run_id` for the same entity and period, which supersedes it. That
  covers "replace with a better result" but not "this result is wrong and
  nothing replaces it yet".

  *What the specification does say* (checked against the source text):
  ZS-CONTROL-001 §7 names Revoked as a certification state (it is not a
  lifecycle state); §27 "Reopen/supersede certified run — restricted command,
  reason, evidence, prior certification retained"; §25 requires a reason for
  reopen and idempotency, `If-Match` and re-authorization on material commands;
  Invariant 2 "corrections create a new run, superseding certification or
  documented reopen workflow"; scenario 12 "certified run later found erroneous
  — original certification retained; controlled superseding/reopen workflow
  required". ZS-SVC-Z-001 (the data-quality certification analogue) adds "new
  material evidence may revoke certification prospectively; prior
  certification history remains", runbook RB-05 "revoke affected certification
  prospectively, preserve prior evidence, assess blast radius", NP-47
  "certification revoked with no reason/evidence" must fail safe, and an event
  carrying prior certification id, reason and effective time. Not relevant
  here: scenario 11 ("certification authority revoked before sign-off") is the
  *certifier's IAM authority* being withdrawn; `CertifyRun` already re-checks
  authorization when it is called.

  *Why it was not built* (2026-10-16): the specification does not say
  (a) which **lifecycle** state a run takes when its certification is revoked
  — §7 has no lifecycle Revoked, the only edge out of lifecycle CERTIFIED is
  SUPERSEDED, and the `control_runs` CHECK forbids lifecycle CERTIFIED with any
  other certification state, so a choice has to be made and the constraint
  migrated; (b) what happens **downstream** (A11); (c) **who** may revoke —
  "restricted command" names no role. Once those are decided, the command is
  small: `POST /controls/v1/runs/{id}/revoke-certification` with mandatory
  reason and evidence reference (NP-47), `If-Match`, `Idempotency-Key`, its own
  action (not `FINCTRL_CERTIFY`), the original certification transition kept,
  event `control.run.certification_revoked` (prior state, reason, effective
  time), and a test that the close gate reports closed for that control
  afterwards (the gate reads the latest run's states).
- **A2. No ControlCertification record (§6, Invariant 9).** Certifying changes
  the run's states and appends a transition row (actor, reason, time). Invariant
  9 requires the certified result to identify the certifier, *certification
  authority*, time, run version, *unresolved exceptions* and *impact
  assessment*. Not stored: which grant/role authorized the certifier at that
  moment (authority snapshot), the list of waived/carried-forward exceptions
  accepted by this certification, assessed exposure, and a link to an evidence
  package sealed for this certification (the package is sealed at execution).
  The evidence export reconstructs part of this from history. Scope: an
  append-only `control_certifications` table written in the certify
  transaction, included in `GET …/evidence` and the export.
- **A3. Certification has no idempotency key.** §25: idempotency is mandatory
  for run creation, execution, reperformance and certification. Create and
  execute require `Idempotency-Key` (`command_idempotency` table); `POST
  …/certification`, `POST …/submit-for-certification` and exception
  transitions (including REPERFORMED) rely on `If-Match` only. A client that
  retries a successful CERTIFY after a lost response gets 412 instead of the
  original result. Scope: reuse `command_idempotency` for those commands.
- **A4. Tolerance and materiality approval is self-declared.**
  `CreateTolerancePolicy` / `CreateMaterialityPolicy` take `approved_by` as a
  free-text body field; the only check is `approved_by != caller`. No second
  authenticated actor ever approves, so one principal can create and "approve"
  a wider tolerance by typing a colleague's id. §11 ("policy change requires
  governed configuration change") and §27 ("Finance Control Policy authority")
  require a real approval; rule versions already do it properly
  (`…/rule-versions/{version}/approve`). Scope: create policies in a pending
  state, add an approve endpoint with its own action, and make run creation
  resolve only approved versions. Also missing: any read endpoint for
  materiality policies (create only).
- **A5. No manual match (§10).** Matching is fully automatic; there is no
  command to pair two records manually with permission, reason and evidence.
  Consequences: an unmatched pair that a human can explain can only be
  resolved through remediation + reperformance, waiver or carry-forward;
  reconciliation-intelligence-svc's suggestions have no command here to act
  through; and the §28 manual-match-rate metric is reported as unavailable
  (`internal/domain/monitoring.go`). Scope: `POST /controls/v1/runs/{id}/manual-matches`
  with the §10/§27 rules — explicit permission, reason, evidence reference,
  amounts must agree within the run's pinned tolerance (scenario 06), blocked
  where an own-transaction rule applies.
- **A6. Run-level reperformance is not implemented.** The `REMEDIATION` and
  `REPERFORMANCE` lifecycle states exist but nothing enters them, and §25's
  `POST /runs/{id}:reperform` has no equivalent. Reperformance is recorded per
  exception (`REPERFORMED` with an `evidence_ref`) and a fresh execution is a
  new run with `prior_run_id` (documented in `financial-control-wave7.md`).
  Whether a run-level reperform is still needed is a design decision; if not,
  the two unused states should be removed or documented as reserved.
- **A7. Late data after freeze/certification is not detected (Invariant 14,
  scenarios 05 and 21).** A frozen population is immutable and later source
  data is picked up by a new run — but nothing notices that a source's
  watermark has moved after a run was CERTIFIED, so a certified population can
  go stale silently. `LATE_DATA` exists only as a category an `EXCEPTION_SCAN`
  rule may emit. The §7 attention signals derived today are CONTROL_FAILED,
  CONTROL_STALE (run never started), RESULT_INDETERMINATE, OVERDUE, SLA_BREACH
  and MATERIAL; `DATA_LATE` and `RECURRENT` are not derived. Scope: a sweeper
  that re-reads the watermark (page 1, `limit=1`) for the latest certified run
  of each control/entity/period, and on change opens a `LATE_DATA` exception
  or attention signal and, per policy, reports the close gate closed.
- **A8. No cross-currency matching (§10, ZS-SVC-Z-001 INV-10).** Records in
  different currencies on one reference raise `CURRENCY_MISMATCH`; nothing
  normalizes through an approved FX basis. Blocked on an FX-rate read capability
  (closure register, FX row).
- **A9. Suspense and clearing (§20, FIN-CTRL-031) are not implemented.**
  Blocked: no suspense designation on the chart of accounts and no item-level
  clearing (closure register).
- **A10. 25 catalogue controls are blocked or partial.** See
  `financial-control-closure.md` §7 and `GET /controls/v1/catalogue/status`;
  not repeated here.
- **A11. Downstream effect of a revoked (or superseded) certification is
  unspecified.** A certification is consumed elsewhere: financial-close-svc's
  readiness check calls `GET /controls/v1/close-gate` before a period is
  closed, and reporting is released from that closed period (ZS-CONTROL-001
  §22, Invariants 10 and 17). Neither ZS-CONTROL-001 nor ZS-SVC-Z-001 says
  what happens when a certification that a period was **already** closed on is
  revoked: whether the period must be reopened, whether released reports are
  marked or withdrawn, or whether consumers are only notified. ZS-SVC-Z-001
  RB-05 says only "assess blast radius", and its revocation is "prospective".
  The close gate in this service would report the gate closed for future
  readiness checks, but nothing tells financial-close-svc or reporting that a
  past decision rested on a revoked certification. This needs an owner's
  decision (Finance Controls / close governance) before A1 is built, and
  probably a consumer of `control.run.certification_revoked` in
  financial-close-svc. The same question applies today to a certified run that
  is superseded by a failing one after its period closed.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. `declared_totals` is required on every page of every population**
  (changed in this pass). The contract said "REQUIRED for Wave 2 sources" while
  the client treated the field as optional, so a source that omitted it gave no
  completeness proof and the run could still pass. The client now fails the
  run INDETERMINATE when any page lacks `declared_totals` or when two pages
  disagree. All eleven implemented sources already send it on every page
  (checked by code inspection), so no conforming source is affected. The
  contract document was updated to match.
- **B2. Contract limits now written down.** The client always enforced a
  population name of 1–64 characters starting with a letter or digit, and at
  most 8 params per spec; the contract document now states both.
- **B3. Close gate key is `period_id = PeriodName`.** financial-close-svc's
  period name is used as the run's `period_id`; there is no fiscal-calendar
  service to resolve periods by id (closure §4).
- **B4. Maker-checker is unconditional.** Invariant 12 applies "where
  maker-checker policy applies"; this service applies it to every
  certification (the certifier cannot have created or acted on the run) and
  every waiver (not by the exception owner). There is no per-control policy to
  relax it.
- **B5. ZS-EVENT-001 CloudEvents naming is not used.** Events use the
  platform's shared envelope with ZS-EVENT attributes as extra fields; a
  platform-wide decision is needed (closure §5).
