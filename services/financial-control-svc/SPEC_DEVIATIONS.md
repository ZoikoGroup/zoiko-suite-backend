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

- **A1. A certification cannot be revoked. — Closed (B6).**
  `POST /controls/v1/runs/{run_id}/revoke-certification` moves a CERTIFIED
  run's certification dimension CERTIFIED → REVOKED. The lifecycle stays
  CERTIFIED and the result is untouched (§7: Revoked is a certification state,
  not a lifecycle state). The original certification is retained (§27):
  nothing in the run's history changes, and an append-only
  `control_certification_revocations` row references the certification
  transition it revokes. The request needs a mandatory reason and at least
  one evidence reference (§25, NP-47), plus `If-Match` and `Idempotency-Key`.
  It is authorized by its own action, `FINCTRL_REVOKE`, and the principal who
  certified the run cannot revoke it. It emits
  `control.run.certification_revoked` (prior state, prior certification,
  reason, evidence, effective time). The close gate reports the control as
  blocking with reason `CERTIFICATION_REVOKED`. The three questions this item
  left open were settled as follows: (a) the lifecycle stays CERTIFIED
  (migration 000006 relaxes the CHECK); (b) the downstream effect is
  prospective only (RB-05; see A11); (c) who may revoke: holders of
  `FINCTRL_REVOKE` other than the certifier. **Deployment prerequisite:**
  see B6, `FINCTRL_REVOKE` must be registered in access-control-svc first.
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
- **A11. Downstream effect of a revoked (or superseded) certification —
  narrowed: the decision is made, consumers are not yet wired.**
  ZS-SVC-Z-001 RB-05 answers the question this item asked: revocation is
  **prospective**. Its downstream effect is flagging and notification from
  the revocation onward. It does not reopen a period already closed on the
  certification, and it does not withdraw reports already released from that
  period. This service now does both of its parts (A1/B6): the close gate
  reports the control as `BLOCKING` with reason `CERTIFICATION_REVOKED`, the
  revocation time and its reason; and `control.run.certification_revoked`
  is emitted with the prior certification, reason, evidence and effective
  time. financial-close-svc already reads each gate item's `reason`, so a
  readiness check made after a revocation sees it without any change there.
  **Still open, and outside this service:** (1) nothing subscribes to
  `control.run.certification_revoked`. financial-close-svc only asks the
  gate when it closes a period, so a period closed *before* the revocation
  is not flagged anywhere until someone queries the gate for it again;
  (2) RB-05's "assess blast radius" has no query yet. Nothing lists the
  closed periods, or released reports, whose close rested on a now-revoked
  certification. Both are consumer-side work (financial-close-svc and
  reporting). The same applies to a certified run superseded by a failing
  one after its period closed: the gate reflects it, and
  `control.run.superseded` is emitted, but nothing acts on it.

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
- **B6. How certification revocation works (A1).**
  - *State.* Only the certification dimension moves, CERTIFIED → REVOKED.
    `domain.ValidateCertificationRevocation` admits it only on a lifecycle
    CERTIFIED run, and REVOKED has no outgoing edge, so a second revocation
    is refused (422). Migration 000006 replaces 000001's CHECK (lifecycle
    CERTIFIED ⇒ certification CERTIFIED) with
    `chk_certified_lifecycle_certification` (… IN CERTIFIED, REVOKED), and
    adds `chk_revoked_follows_certification` (REVOKED only on lifecycle
    CERTIFIED or SUPERSEDED). The down migration refuses while any revoked
    run exists. A revoked run can still be superseded by a later run that
    names it as `prior_run_id`. Its lifecycle then moves to SUPERSEDED and
    its certification stays REVOKED, so the history reads "revoked, then
    replaced".
  - *Shape.* Mirrors `CertifyRun`: `h.caller`, mandatory `If-Match`
    (`parseIfMatch`), `h.authorize` on the run's legal entity, and one
    store transaction holding the `FOR UPDATE` lock. It also requires
    `Idempotency-Key` and uses `command_idempotency` (command
    `REVOKE_CERTIFICATION`) the way `BeginExecution` does. A retry gets its
    original result (`Idempotent-Replay: true`) instead of a 422 on the
    now-revoked run, the key is claimed only after every check passes, and
    reusing a key for another run is a 409.
  - *Retained, additive only.* The run's certification transition row, and
    every other history row, is unchanged. The test compares their
    `row_to_json` text before and after. The new CERTIFICATION
    CERTIFIED → REVOKED transition records the revoker and reason. The new
    `control_certification_revocations` row records
    `certification_transition_id`, `certified_by`/`certified_at`, the reason,
    `evidence_refs`, `revoked_by`/`revoked_at` and the correlation id. It is
    one row per run (UNIQUE), immutable (`reject_control_mutation`), FORCE
    RLS, and CHECKed for a non-blank reason, at least one evidence reference,
    and `revoked_by <> certified_by`.
  - *Who.* `FINCTRL_REVOKE` is a separate grant from `FINCTRL_CERTIFY`
    (§27 "restricted command"). `domain.CheckRevokerIndependence` refuses
    whoever moved the run to CERTIFIED (403 `segregation_of_duties`), on the
    pattern of reconciliation-engine-svc's `CheckCertifierIndependence`. The
    run's preparers are not barred: revoking is the fail-safe direction.
  - ***Deployment prerequisite:*** access-control-svc validates every
    permission bundle against its `permission_definitions` registry
    (its migration 000008), and new actions are registered there by
    migration. Until `FINCTRL_REVOKE` is added there (and to the
    `FINCTRL_FULL` bundle in `deployments/scripts/seed-demo-rbac.ps1`), it
    cannot be granted, so every revocation is denied (fail-closed). Both
    files belong outside this service and were not changed here.
  - *Gate.* `EvaluateGate` reports a lifecycle CERTIFIED + certification
    REVOKED latest run as `BLOCKING` with reason `CERTIFICATION_REVOKED`.
    Before this it would have fallen through to `LATEST_RUN_CERTIFIED_PASS`.
    The item also carries `certification_revoked_at` and
    `revocation_reason`, both additive JSON fields.
