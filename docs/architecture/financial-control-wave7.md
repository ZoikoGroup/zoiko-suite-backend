# financial-control-svc — Wave 7 (exception resolution, resubmission, supersede, housekeeping)

Migration `000004_exception_resolution`: `exception_transitions` gains `authority_ref`, `evidence_ref`,
`carry_to_period` (the table stays append-only); `control_exceptions` gains `sla_breach_notified_at`.

## Exception lifecycle — `POST /controls/v1/exceptions/{id}/transition`

Body `{"to_state","reason", …}`; `If-Match` mandatory. Edges are the §21 table already in the domain
(`ASSIGNED → INVESTIGATING → AWAITING_* → REMEDIATED → REPERFORMED → CLOSED`; waiver and carry-forward only from
`INVESTIGATING`; no `REMEDIATED → CLOSED` shortcut). `GET …/{id}/transitions` returns the history.

| Target | Extra fields | Action | Independence rule |
|---|---|---|---|
| INVESTIGATING, AWAITING_EVIDENCE, AWAITING_ADJUSTMENT, REMEDIATED | REMEDIATED needs `evidence_ref` | `FINCTRL_EXCEPTION_RESOLVE` | actor must be the assigned owner |
| REPERFORMED | `evidence_ref` (the rerun) | `FINCTRL_REPERFORM` | not the owner, not whoever recorded the remediation |
| CLOSED | | `FINCTRL_EXCEPTION_RESOLVE` | |
| WAIVED_UNDER_AUTHORITY | `authority_ref` | `FINCTRL_EXCEPTION_WAIVE` | not the owner |
| CARRIED_FORWARD_UNDER_AUTHORITY | `authority_ref`, `carry_to_period` (YYYY-MM) | `FINCTRL_EXCEPTION_WAIVE` | not the owner |

Independence is checked in the store under the row lock. Events: remediation_recorded, reperformed, closed,
waived, carried_forward.

## Run roll-up

When the last exception of a `FAIL` run becomes CLOSED / WAIVED / CARRIED_FORWARD, the run moves from
`EXCEPTION_REVIEW` to `READY_TO_CERTIFY` in the same transaction: `PASS_WITH_APPROVED_EXCEPTIONS` if any
exception was waived or carried forward, otherwise `PASS` (every finding fixed and reperformed).

`POST /controls/v1/runs/{id}/submit-for-certification {"reason"}` (`If-Match`) is the operator's way back
after a **rejected** certification: it requires every exception resolved, and returns certification to PENDING.
The certifier who rejected may decide again; anyone who produced the result may not.

## Supersede

Creating a run with `prior_run_id` for the **same entity and period** supersedes a `READY_TO_CERTIFY` or
`CERTIFIED` prior run in the same transaction (its certification becomes SUPERSEDED, `superseded_by_run_id`
is set, `control.run.superseded` is emitted). A different period is only linked.

## Housekeeping (every 10 minutes)

* a run left in SCHEDULED / PREPARING for 24 h becomes `EXPIRED` (`control.run.expired`);
* each unresolved exception past `due_at` gets exactly one `control.exception.sla_breached` event.

## Not covered

* Run-level `REMEDIATION` / `REPERFORMANCE` lifecycle states are not used: reperformance is recorded per
  exception with its evidence reference, not by re-executing the run inside the same run. A fresh execution is a
  new run with `prior_run_id`.
* Nothing yet consumes the events (alerts, dashboards, notifications).

New authz actions to grant: `FINCTRL_EXCEPTION_RESOLVE`, `FINCTRL_EXCEPTION_WAIVE`, `FINCTRL_REPERFORM`
(plus `FINCTRL_CERTIFY` from Wave 5).
