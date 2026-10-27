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
- **A2. No ControlCertification record (§6, Invariant 9). — Closed (B7).**
  Every CERTIFY decision now writes one append-only `control_certifications`
  row (migration 000007) inside `CertifyRun`'s own transaction, under the
  run's row lock. It holds §6's canonical fields: `run_id`, certifier,
  authority snapshot, result, unresolved exceptions, assessed exposure,
  timestamp and evidence package. It also holds Invariant 9's run version
  and the certification transition it records. `GET …/evidence` and the
  evidence export serve the record as a `certification` field, read directly
  from the table instead of being inferred from the transition history.
- **A3. Certification has no idempotency key. — Closed (B8).** `POST
  …/certification`, `POST …/submit-for-certification` and `POST
  /controls/v1/exceptions/{id}/transition` (every target state, including
  REPERFORMED) now require `Idempotency-Key` (400 `missing_idempotency_key`),
  in addition to `If-Match`. Each claims the key in `command_idempotency`
  exactly as `BeginExecution` does. A repeat of the key for the same command
  on the same target returns the original result (`Idempotent-Replay: true`,
  the target as it now stands) instead of a 409/412. The same key used for
  another target or decision is a 409. Migration 000008 adds the
  `exception_id` an exception transition needs, and widens `command`.
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
- **A7. Late data after certification is not detected (Invariant 14,
  scenarios 05 and 21). — Closed (B9).** A late-data sweep now runs on the
  service's housekeeping schedule. For the latest CERTIFIED run of each
  control, entity and period, it re-reads each side's source watermark (page
  1, `limit=1`, through the same `source` client request execution builds
  populations with) and compares it with the watermark the run froze. When
  it has moved, it opens one `LATE_DATA` exception on that run, with reason
  `LATE_DATA_AFTER_CERTIFICATION`, through the same insert, transition and
  `control.exception.opened` event execution uses. The close gate then
  reports the control as `BLOCKING` with that reason, at the same decision
  point (`EvaluateGate`) as every other reason. It stays blocked until the
  exception is resolved under authority or a new run supersedes the
  certified one. Nothing is mutated: the run row, its history, its frozen
  population, its evidence and its `control_certifications` row are never
  written. The integration test compares their `row_to_json` before and
  after.
  **Not built:**
  1. There is no configurable per-control or per-population late-data
     policy (§9 "Late-data rule"). Every moved watermark on a latest
     certified run blocks the gate until it is resolved.
  2. Late data on a run that has frozen its population but is not yet
     certified (READY_TO_CERTIFY, EXCEPTION_REVIEW) is not detected, so a
     certifier can still certify a population the source has already moved
     past.
  3. Detection runs at the sweep's cadence (every 10 minutes, each run at
     most every 30), not on a source event, so a gate read in between does
     not see it yet.
  4. A source the sweep cannot read is recorded as `UNVERIFIED` in
     `control_watermark_checks` and logged. It does not affect the gate, and
     no endpoint exposes it.
  5. The `DATA_LATE` and `RECURRENT` attention signals are still not derived.
     The exception was chosen as the shape (B9), and `GET
     …/monitoring/metrics`' late-data count already includes these
     exceptions.
  6. As in A11, nothing subscribes to the event, so a period closed before
     the late data arrived is not flagged anywhere.
  7. Scenario 21's consolidation population belongs to consolidation-svc.
     This covers this service's populations only.
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
- **B7. The ControlCertification record (A2).**
  - *Authority snapshot.* authorization-svc's `/v1/authorize` already
    returns `access_decision_id` (the key of its append-only
    AccessDecisionLog, which holds the matched grants), `decision_basis`
    (e.g. `rbac:role=…`), `decision`, `policy_set_version` and
    `reason_codes`; this service used to keep only the outcome. A new
    `authz.Client.Decide` returns them, and `CertifyRun` now authorizes
    through it (`authorizeWithAuthority`). `Decide` always asks live,
    bypassing the 5-second decision cache, so the recorded authority is the
    decision taken for this certification and never one reused from
    another request. A GRANTED answer with no `access_decision_id` cannot be
    traced and is refused as `authorization_unavailable` (503). The store
    also refuses a CERTIFY whose snapshot is missing, untraceable, or for
    another principal, and a CHECK requires `access_decision_id` and
    `decision_outcome = GRANTED`. The snapshot is stored as authorization-svc
    gave it, plus the principal, entity, action and the time it was
    received. Every other command still uses the cached `CheckAllowed`;
    only certification records its authority.
  - *Unresolved exceptions.* Every exception of the run not CLOSED at
    certification: in practice the WAIVED_UNDER_AUTHORITY and
    CARRIED_FORWARD_UNDER_AUTHORITY ones a PASS_WITH_APPROVED_EXCEPTIONS
    result accepts. Each is recorded with state, category, reason code,
    severity, exact exposure and currency, plus the `authority_ref` and
    `carry_to_period` of the exception transition that put it there.
  - *Assessed exposure.* `domain.AssessExposure`, which is the existing
    `Summarise` (exact `big.Rat` totals per currency and severity) applied
    to those exceptions, against the materiality policy the **run pinned
    at creation**, not the entity's current one. A run that pinned none
    gets totals with `aggregate_material: null` (not assessable). No FX is
    applied; other currencies are listed as unassessed.
  - *Evidence package.* A reference to the package sealed at execution
    (the latest, if reperformed), by `evidence_package_id` (foreign key) and
    `evidence_digest`. No new package is sealed for the certification. A
    run with no sealed package cannot be certified (422).
  - *Scope.* Only CERTIFY writes a record; a REJECT is a transition, not a
    certification. One row per run (UNIQUE), immutable
    (`reject_control_mutation`), FORCE RLS. `certification_transition_id`
    is the same CERTIFICATION → CERTIFIED row a revocation (B6) references.
    No backfill: runs certified before 000007 had no authority recorded,
    none is invented, and their `certification` is absent or null. The
    `control.run.certified` event payload is unchanged.
  - *Reads.* `GET …/evidence` adds `certification` to the package fields
    (omitted when there is none). The export adds `certification` (null when
    none) inside the bundle's manifest digest. `run_transitions` stays in
    the export as history, but is no longer the only place a certification
    can be seen.
- **B8. Idempotency of certification, resubmission and exception
  transitions (A3).**
  - *One mechanism.* The same `command_idempotency` table and rule as
    `BeginExecution` (and `RevokeCertification`, B6, which is unchanged): the
    key is looked up first, the command runs, and the key is claimed in the
    same transaction only once the command has succeeded, so a refused
    command (segregation, a stale version, an invalid transition) never
    consumes it. The three commands share `internal/store/idempotency.go`
    (`lookupCommandKey`/`recordCommandKey`). `BeginExecution` and
    `RevokeCertification` keep their own inline copies, which apply the same
    rule.
  - *What a key is bound to.* The run, and for an exception transition the
    exception too: migration 000008 adds `command_idempotency.exception_id`.
    Without it, one key retried against two exceptions of the same run would
    read as a replay of the first. The command name records what was decided
    (`CERTIFY` or `REJECT`, `SUBMIT_FOR_CERTIFICATION`,
    `EXCEPTION:<to_state>`), so a key cannot replay a different decision on
    the same target; that 409s instead. `command` is widened to
    VARCHAR(64) for `EXCEPTION:CARRIED_FORWARD_UNDER_AUTHORITY`. A key from
    another command (e.g. the run's `EXECUTE` key) is a 409.
  - *Replay.* Like `BeginExecution`, a replay returns the target as it now
    stands, not a stored copy of the original response. It is checked
    before the `If-Match` version, because the retry carries the version the
    client saw before its first attempt. The response carries
    `Idempotent-Replay: true` and the current ETag.
  - *Concurrency.* Unlike `BeginExecution`, these commands take the
    target's row lock **before** looking the key up. Two concurrent first
    attempts with one key are therefore serialised: one applies and the
    other replays it, rather than the second failing on the version
    (covered by a concurrent integration test).
  - *Order of checks in the handler.* Idempotency-Key, then If-Match (428),
    then the body, then authorization, matching `ExecuteRun` and
    `RevokeCertification`. Authorization is re-evaluated on a replay too
    (§25: on every material command). No caller outside this service uses
    these three endpoints, so making the header mandatory breaks no client
    in the repository.
  - *Rollback.* 000008's down migration deletes the claims the old schema
    cannot hold (exception transitions, commands longer than 32 characters).
    They are retry protection, not records.

- **B9. How late data after certification is detected (A7).**
  - *Trigger.* The service already had an in-process scheduler. `cmd/server`
    runs the housekeeping `Sweep` (run expiry, SLA breaches) on a 10-minute
    ticker under the relay context, and the late-data sweep is a second loop
    of the same shape: `engine.LateDataSweeper.Sweep(ctx, 100)` every 10
    minutes, re-checking a run at most every 30 minutes, least recently
    checked first. It is not computed lazily on close-gate reads, for three
    reasons:
    1. The gate would then make one or two source calls per mandatory
       control on every read, and a source outage would make the gate's
       answer depend on that source.
    2. A GET would have to write the exception.
    3. Invariant 14 wants the exception to exist when the late data is
       there, not only once someone asks the gate. Monitoring and exception
       queues see it too.

    It is a separate loop rather than another step in `Sweep` so that a slow
    source (each read is bounded at 10 seconds) cannot delay run expiry or
    SLA events. `Sweep` lives in `store`, which has no source client.
  - *Shape.* An exception, not an attention signal. `LATE_DATA` is already a
    `control_exceptions` category, the one `EXCEPTION_SCAN` rules emit. The
    exception lifecycle already carries owner, due date, authority, evidence
    and root cause, which is how a late-data finding gets resolved, and
    monitoring already counts the category. Attention signals are derived
    from stored fields on read, which a watermark re-check cannot be.
    `domain.LateDataException` builds it with:
    - severity HIGH, owner = the definition's owner role, due date from the
      HIGH SLA;
    - side = the side that moved, `record_ids` = the stale frozen
      `population_id`;
    - exposure 0 in `XXX` (there is no amount yet);
    - detail naming both watermarks.

    Its reason `LATE_DATA_AFTER_CERTIFICATION` is reserved (`validateScan`
    refuses it), so the gate cannot confuse it with a `LATE_DATA` finding the
    certifier already saw.
  - *Population read.* `source.HTTPFetcher.Watermark` calls the same `page`
    request `Fetch` loops over, now taking its limit, with `limit=1` and no
    cursor. The spec is resolved as `Execute` resolves it (definition spec,
    bound to the run's scope) and must give the frozen snapshot's `spec_ref`,
    or the check is `UNVERIFIED`. As at execution, the source authorizes the
    human caller (control-population-contract.md): the read forwards the
    principal who executed the run, the actor of its PREPARING transition.
    If that principal has since lost the source's read action, the source
    refuses and the check is `UNVERIFIED`. *A reviewer may prefer a service
    principal here; the contract has none today.*
  - *Recording.* `PgStore.RecordWatermarkCheck` runs in one transaction
    under the run's `FOR UPDATE` lock, so replicas sweeping at once open the
    exception once (covered by a concurrent test). It first re-checks that
    the run is still CERTIFIED/CERTIFIED and still the latest run of its
    control, entity and period. It opens an exception only when no
    unresolved late-data exception exists for that side and the observed
    watermark is not the one the last such exception was opened for. Once
    that one is resolved, the same watermark is not reported again, but a
    further move is. Migration 000009's `control_watermark_checks` (one
    mutable row per run and side, FORCE RLS) holds the last check (CURRENT,
    MOVED, or UNVERIFIED with its error) and that reported watermark. It is
    the sweep's state, not a record: the exception, its transition and its
    event are the record.
  - *Gate.* `PgStore.CloseGate` reads the latest run's oldest unresolved
    `LATE_DATA_AFTER_CERTIFICATION` exception into `GateInput`. When the
    run is CERTIFIED/CERTIFIED, `EvaluateGate` reports `BLOCKING` with that
    reason, plus `late_data_exception_id` and `late_data_detected_at`. A
    revoked certification still reports `CERTIFICATION_REVOKED`. Two things
    clear it:
    - resolving the exception (closure, waiver or carry-forward under
      authority, through the existing transition endpoint);
    - a new run, which becomes the latest. Naming the certified run as
      prior supersedes it, as before.
  - *Rollback.* 000009's down migration refuses while any check records a
    late-data exception it opened. That reported watermark is what stops a
    resolved one being reported again after a re-apply. Otherwise it drops
    the table. The same `DO $$ … RAISE EXCEPTION` guard as 000006/000007,
    with a table check so it can be re-run.
