# workflow-svc — Progress

## Status (21 Oct 2026): Doc 03 §8.4 complete; governed under the runtime role

The Governance Platform audit (14 Oct: 26 ✅ / 23 ⚠️ / 12 ❌) was re-run on
21 Oct and every local gap closed. The first finding was a defect the earlier
audits could not see because their tests ran as the superuser: after 000017
put FORCE RLS on the stage, quorum and transition tables, the stage reads still
used the bare pool, so **as `zoiko_app` an instance had no stages, no approver
could approve, and a same-key retry answered 409**. Every store read and write
now runs under the caller's tenant, and the store suite proves the approval
chain, escalation, timers, re-check and append-only log as a NOBYPASSRLS role
(`internal/store/completion_rls_test.go`, mutants killed).

What changed (migration 000020):

- **Decisions.** Named approver, delegate (authorization-svc answers "does the
  delegate hold WORKFLOW_APPROVE *from this approver*" and returns the
  delegation id), ROLE stage holder, or the escalation target of an ESCALATED
  instance. Nobody decides two stages of one instance; the initiator never
  decides; a service account or system principal cannot decide
  (`approver_not_human`); REJECT needs a rationale. `workflow_type`,
  `approver_role` and `stage_order` go to authorization-svc as attributes.
- **Quorum.** Votes on one stage are serialised (stage `FOR UPDATE`), every
  vote is recorded, tallies are per review cycle, ESCALATED stages can be
  voted on (concurrent final votes produced two `workflow.completed` in 14 of
  15 runs before).
- **Commands.** Idempotency-Key with a request hash on every command (replay
  200, different body 409); the old `(tenant, correlation_id)` unique index is
  gone (a second workflow in one business flow was a 503).
- **Evidence.** Each transition records its stage, review cycle, access
  decision, authority basis, delegation, definition version and ledger
  decision id; `workflow_transitions` and quorum decisions are append-only by
  trigger. The ledger record is written to the outbox in the decision's own
  transaction and delivered to governance-decision-log-svc as the service
  principal `svc-workflow` (`WORKFLOW_LEDGER_PRINCIPAL_ID`; a 409 parks it,
  anything else retries with backoff). Responses carry `decision_id`.
- **Lifecycle.** Escalation keeps the stage actionable; `escalate-to` is routed
  (it had a handler and no route); request-changes reopens a REJECTED stage for
  a new review cycle (`workflow.reopened`); re-invalidating is an idempotent
  200; `next-approver` on a terminal workflow is 404 `no_current_stage`.
- **Definitions.** A workflow started from a definition follows its route
  (SINGLE / QUORUM / ROLE stages, stage deadlines, expiry, escalation target).
- **Timers** (`internal/timers`). Past `due_at` → EXPIRED
  (`workflow.expired`); a PENDING stage past its deadline → escalated to the
  target, or ESCALATED. Listed across tenants under the SELECT-only
  `app.timer_sweep` policy; every transition runs under the instance's tenant.
- **Consumed events** (`internal/consumer`). policy.updated /
  policy.version.*, authority.*, authorization.denied flag the PENDING
  instances whose current approver they concern; the timer pass re-authorizes
  those approvers and escalates when one may no longer approve. Claimed once
  in `workflow_consumed_events`, committed after apply.
- **Outbox.** One relay per destination (Kafka, decision_log), exponential
  backoff (cap 300 s), parked after 200 failures (was 10 at 1.5 s); Kafka key
  `<tenant>/<workflow_instance_id>`; the envelope carries causation, plane,
  payload hash, review cycle, access decision.
- **Edge.** identity-envelope gate (compose `warn` until the console forwards
  the envelope); reads need WORKFLOW_READ unless the caller is the initiator or
  a current approver (`WORKFLOW_READ_AUTHZ`); X-Tenant-Id and legal entity must
  be UUIDs; body legal entity must match X-Legal-Entity-Id and exist in the
  tenant (tenant-entity-registry-svc); RFC 9457 errors with §16 classes; the
  service no longer invents a correlation id.
- `openapi.yaml` (kin-openapi valid, route parity test) for the approval
  surface; `postman/postman/ZoikoSuite_Workflow.postman_collection.json`.
- Image built from source at the repository root (identity-envelope replace).

Grants: access-control 000021 registers WORKFLOW_READ / CANCEL / ESCALATE /
ESCALATE_TO / INVALIDATE / ADMINISTER / DEFINE and the WORKFLOW_APPROVER,
WORKFLOW_REQUESTER and WORKFLOW_ADMINISTRATOR templates (holds H-1, H-2);
`deployments/supabase/seed-rbac.sql` grants `svc-workflow` (and `svc-policy`)
GOVERNANCE_DECISION_RECORD and nothing else.

Still open, outside this service: relationship independence (R-INV-09) needs
an org-structure source (H-6); automatic invalidation needs the subject owners
to call `/invalidate` (H-5); invoice-approval-svc does not start workflows
correctly (N-3); the gateway route and published port (H-4).

## History: v1 built and verified end-to-end (2026-07-09)

*The v1 notes below are kept as written; the consumed-events and single-action
decisions in them are superseded by the 21 Oct section above.*

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
