# obligations-svc: spec deviations

## R-OD-05: no clock pauses

R-001 leaves open which clock classes may pause and how (R-OD-05, Legal/Compliance). No pause
exists: a clock runs from its trigger to a terminal state, and a changed deadline is a
supersession. Waiting for a decision is not a reason to stop a statutory clock.

## R-OD-06: evidence classes per domain

The evidence a fulfilment needs is whatever evidence-requirements-svc has defined for the
obligation's `(domain_code, action_type)` (mapped from `obligation_type` in the handler; an
unmapped type is evaluated as `COMPLIANCE / OBLIGATION_FULFIL_<TYPE>`). With no requirement
defined (NO_REQUIREMENTS_DEFINED) a fulfilment with evidence refs is accepted and logged
(D-ER-5); without refs it is refused. Which classes each domain must require is R-OD-06.

## R-001 §10.1 route names

| R-001 | Here |
|---|---|
| `POST /v1/obligations/evaluate` | same |
| `POST /v1/obligations/{id}/satisfy` | same (fulfils every running clock; per clock: `…/clocks/{cid}/satisfy`) |
| clock states set by a free target | named commands `activate`, `satisfy`, `waive`, `not-applicable`, `recalculate`; `POST …/clocks/{cid}/status` is a deprecated alias that only activates (ST-3) |

## Waiver and exemption approvals live in workflow-svc

A waiver, a not-applicable decision and an evidence exemption are approved in workflow-svc
(workflow types `OBLIGATION_WAIVER`, `OBLIGATION_NOT_APPLICABLE`,
`OBLIGATION_EVIDENCE_EXEMPTION`, subject `OBLIGATION_CLOCK/<clock_id>`, subject fingerprint
`domain.AuthoritySubjectFingerprint(clock, action, reason_code, evidence_refs)`). This service
verifies the approval (GET, evidence, verify-release) and that no decider is the requester; it
does not hold a second approval store. The console KPI "awaiting review" therefore cannot be
counted here and `GET /v1/obligations/summary` returns it as `null` with a note.

## Escalation NOTIFY is an event

NOTIFY emits `obligation.escalated` (`notify: true`). Delivery is notification-svc's, which needs a
template and a trigger on that event (tracker D1-7). Until then the escalation is recorded and
visible but not delivered.

## Calendar recalculation identity

The calendar consumer calculates as `svc-obligations` with `X-Principal-Id` / `X-Tenant-Id`
headers (no request envelope exists). Under `IDENTITY_VERIFY=require` on jurisdiction-rules-svc this
path needs a workload credential (mTLS peer or token exchange); until then it works only while
jurisdiction-rules-svc accepts headers (`warn`).
