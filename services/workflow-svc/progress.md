# workflow-svc — Progress

## Status: v1 built and verified end-to-end (2026-07-09)

Implements Workflow & Approvals Service per `docs/architecture/03-microservices.md`
§8.4 — the last unbuilt Phase 1 Sovereign Spine service. It couldn't be built
before today because the docs are explicit: "approval workflows extend
authorization, they do not replace it" — this service has a real, load-bearing
dependency on authorization-svc, which only just merged.

## What's built

- `WorkflowInstance` / `WorkflowStage` / `WorkflowTransition` — ordered
  multi-stage approval chains, entity-bound, append-only transition log.
- `POST /v1/workflows`, `GET /v1/workflows/{id}`,
  `GET /v1/workflows/{id}/next-approver`, `POST /v1/workflows/{id}/actions`,
  `POST /v1/workflows/{id}/escalate`, `POST /v1/workflows/{id}/cancel`.
- Every approval action is checked against authorization-svc's
  `POST /v1/authorize` before being applied — fail-closed, same pattern as
  every other synchronous cross-service call in this platform.
- Idempotency (explicit doctrine requirement — "duplicate approval submission
  must not create double-state transition"): a replayed action is recognized
  by looking up the actor's *own* stage directly, not just "the current
  stage" — so a retry that arrives after the workflow has already advanced
  past that actor's stage is still correctly recognized as a no-op, not
  rejected as a wrong-approver error.
- Kafka producer: `workflow.started`, `approval.granted`, `approval.rejected`,
  `workflow.escalated`, `workflow.completed`.

## Deliberate v1 scope decisions

- **No rule engine for resolving approvers.** The spec's "resolve next
  approver" capability is implemented as a query over a chain the *caller*
  supplies at creation time. Nothing in the architecture docs specifies how
  approvers should be derived from workflow_type/amount/role — inventing that
  business logic isn't this service's call to make.
- **No consumed events in v1**, despite the spec listing `authorization.denied`,
  `policy.updated`, `authority.delegated`:
  - `authorization.denied` is now genuinely published by authorization-svc,
    but no behavior is specified anywhere for what workflow-svc should *do*
    with it — building a consumer with nothing to do would be dead
    infrastructure (same reasoning policy-svc used to defer `entity.created`).
  - `policy.updated` is published by policy-svc, same gap: no specified
    reaction.
  - `authority.delegated` isn't published by anything yet — authorization-svc
    owns `DelegatedAuthority` but doesn't emit an event on creation. Genuinely
    blocked, not a decision this service can make.
- **Single platform-wide `action_type`** (`WORKFLOW_APPROVE`) checked against
  authorization-svc for every approval, regardless of `workflow_type`. No
  per-workflow-type action codes are specified in the docs.
- **One stage per approver per workflow assumed.** If the same principal is
  listed as the approver for two stages in one chain, idempotency lookups by
  approver could resolve ambiguously. Revisit if that scenario is a real
  requirement — the docs don't mention it.

## Verified end-to-end (not just unit tests)

Booted the real image against the live compose stack (real Postgres, real
Kafka, real authorization-svc):

1. Granted `WORKFLOW_APPROVE` to two principals via authorization-svc's real
   admin API (role + permission bundle + role assignment).
2. Created a 2-stage workflow over HTTP.
3. Confirmed a principal with **no** authorization-svc grant is rejected with
   403 before the workflow's own approver logic is ever reached (proves the
   "extends authorization" integration is real, not a stub).
4. Approved stage 1, then replayed the identical request — confirmed it's a
   no-op (didn't double-advance to stage 3, which doesn't exist).
5. Approved the final stage — confirmed `workflow_status` transitioned to
   `APPROVED`, `current_stage` reset to 0, `completed_at` stamped.
6. Independently consumed the real `zoiko.workflow.events` Kafka topic and
   confirmed `workflow.started`, `approval.granted`, and `workflow.completed`
   all actually landed on the wire — not just that the write call returned
   no error.

## Bug found and fixed during this build

`SubmitAction`'s SQL had the same Postgres type-inference conflict found
earlier in obligations-svc: a single bound parameter (`$1`) used both as a
plain assignment and inside a `CASE`/`IN` comparison in the same query
confuses Postgres's type inference (`inconsistent types deduced for
parameter $1`, SQLSTATE 42P08). Fixed with an explicit `::VARCHAR` cast in
both occurrences.
