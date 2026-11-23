# BIZ-08 Business Deadline — Certification Status

**Service host:** exception-escalation-svc (BIZ-08 is bolted onto this service, beside the exception cases, audit findings and BIZ-05 tasks; tables `deadlines`, `deadline_escalations`, `deadline_due_history`)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 10
**Status: NOT CERTIFIED.** The command and query set is implemented. Three defects were found and fixed in this audit, and the five events now go through the shared eventing outbox. The spec's calendar/timezone inputs, supervisor separation of duties and exception-on-source-conflict are open, and the spec says calendar/timezone ambiguity alone blocks certification.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 10)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateDeadline, Recalculate, AssignOwner, Complete, Waive, Cancel, Escalate, MirrorAuthoritativeDeadline | All implemented | `internal/handler/deadline_handler.go`, `/v1/deadlines/...` |
| Queries: GetDeadline, ListUpcoming, ListOverdue, ExplainCalculation, GetSourceDeadline | All implemented | same |
| Lifecycle Scheduled -> Upcoming -> Due -> Overdue -> Completed/Waived/Cancelled/Superseded | Stored: SCHEDULED, COMPLETED, WAIVED, CANCELLED, SUPERSEDED. Upcoming and Overdue are computed at read time (72 h due-soon window, a documented placeholder). **No distinct "Due" state.** Terminal states are only reachable from SCHEDULED; edges are enforced in the store, not by the database | `internal/store/deadline_store.go` |
| Authorization / SoD | `DEADLINE_CREATE` for create and mirror, `DEADLINE_MANAGE` for everything else. Fail-closed. **No owner-plus-supervisor separation**: any holder of `DEADLINE_MANAGE` can complete, waive or cancel any deadline in the entity, including their own | handler |
| Manual override needs reason/authority | Waive and Cancel already required a reason. **Recalculate did not: fixed in this audit** (below). The "authority" part is only the `DEADLINE_MANAGE` action | handler, store |
| Mirrored statutory/legal dates cannot be silently overridden | Enforced: Recalculate refuses a mirrored deadline; a re-mirror with a changed source version over a row a human already completed, waived or cancelled is refused with `DEADLINE_SOURCE_CONFLICT`; an untouched row is superseded cleanly. A partial unique index allows one live row per source | store, migration 000004 |
| Events (5) | `deadline.created`, `deadline.superseded`, `deadline.completed`, `deadline.due_soon` and `deadline.overdue` are written to the shared eventing outbox in the same transaction as the change (and, for due-soon and overdue, as the notified stamp), as `com.zoikosuite.business.deadline.created`, `superseded`, `completed`, `became-due-soon` and `became-overdue`, with payload schemas in `contracts/events` enforced by the store tests and extension catalogue entries (producer Exception Escalation Service). The catalogue's section 19 entry `business.deadline.due` (producer "Deadline Service", no deployed owner) overlaps the last two and is unreconciled | `internal/store/eventing_events.go`, `contracts/events` |
| Failure: calendar/timezone/rule ambiguity blocks certification | **Not implemented.** No timezone, business calendar, deadline rule/profile or escalation plan is stored or evaluated; `due_at` is supplied by the caller and `calc_rule` is free text | — |
| Failure: authoritative source conflict creates an exception | **Not implemented.** The conflict is refused (409) but no exception case is created, although this service owns exception cases | `MirrorAuthoritativeDeadline` |
| Evidence: source trigger/version, rule, calendar, timezone, calculation, overrides, escalation, completion | Source ref and version, rule and inputs (at creation), escalation rows (append-only), completion and waiver actors and times are recorded. **Overrides were not kept: fixed in this audit.** Calendar and timezone are not recorded | tables |

## Defects found and fixed in this audit

1. **Deadline reads had no authorization.** `GetDeadline`, `GetSourceDeadline` and `ExplainCalculation` checked no principal and no action: any caller in the tenant, with no identity header at all, could read any deadline, its source lock and how it was calculated. The list queries already required `DEADLINE_MANAGE`. The three reads now require a principal and `DEADLINE_MANAGE` on the deadline's own legal entity (fetch, then authorize, then return), fail closed (403 denied, 503 authz unavailable). No new action to register.
2. **Recalculate overwrote the due date with no reason and no history.** The previous date, its rule and the reason for the change were lost, which the spec's evidence row ("overrides") and manual-override row rule out. Recalculate now requires a `reason` (400 without one, and the store refuses too), and each change writes one row to the new append-only, tenant-isolated `deadline_due_history` (migration 000006) in the same transaction: previous and new due date, previous and new rule, reason, actor. `ExplainCalculation` returns the history.
3. **A replay of an already-mirrored source version conflicted after the row was completed.** The status check ran before the same-version check, so replaying the identical source version of a mirror that a human had completed returned `DEADLINE_SOURCE_CONFLICT` instead of the existing row. A same-version replay is now idempotent whatever has happened locally; only a changed source version over an acted-on row conflicts.

The existing store tests for recalculation were updated to pass a reason.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Mirrored dates are locked against local recalculation (tested); nothing posts to another domain; no TAX/LEG lookup exists, `GetSourceDeadline` reports the recorded lock only. No test asserts that the service holds no tax or legal calculation | Partial |
| Tenant isolation | RLS forced on all three tables. `TestPgStore_Deadline_TenantIsolation` (added) runs as a `NOSUPERUSER NOBYPASSRLS` role: tenant B cannot read, list, explain, assign, complete, recalculate, waive, cancel or escalate tenant A's deadline; A's data is unchanged; a raw count under B's context sees 0 rows in all three tables. Mutation-checked: it fails with RLS removed from `deadlines` | Covered, including RLS |
| Classification / retention | Nothing deadline-specific found | **Missing** |
| Negative paths | Double complete/waive/cancel, escalate on a terminal deadline, recalculate a mirrored deadline, source conflict, missing source fields, read without principal or action, recalculate without a reason, append-only escalations and history | Covered |
| Evidence lineage | Source lock, rule, escalation and due-date history are stored; calendar, timezone and completion evidence beyond actor and time are not | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container (store suite 34 s)
new tests
  handler: TestDeadlineReads_RequireAuthorizationOnTheDeadlinesEntity   passes; fails with the check removed
  handler: TestRecalculate_RequiresAReason
  store:   TestPgStore_Recalculate_RequiresReasonAndKeepsHistory
  store:   TestPgStore_MirrorAuthoritativeDeadline_ReplayAfterCompletionIsNotAConflict   fails with the old ordering
  store:   TestPgStore_Deadline_TenantIsolation                         fails with RLS off
```

Date: 2026-11-17. The Postgres container was disposable and has been removed.

## Event migration (after the audit)

- The handler used to publish these events after the commit and only log a failure; for due-soon and overdue the notified timestamp was committed first, so a failed publish was never retried. They are now enqueued in the same transaction (migration 000007 equals `outbox.SchemaSQL`, checked by a test). A failed enqueue rolls the change and the stamp back (tested).
- `EVENT_RESIDENCY_REGION` is now required (compose sets it, default `local`), a relay delivers `eventing_outbox` and is not started when there is no database or no Kafka brokers, and the Docker build context moved to `services/` (Dockerfile, `Dockerfile.dockerignore`, `docker-compose.phase5.yml`). The service's other events (exception cases, findings, tasks) still use the direct publisher and are untouched.
- Verified: full suite passes against Postgres 16, a deliberately broken schema fails the store test, the image builds and the container refuses to start without the region. Not verified: a started container with a database, and delivery through a real Kafka broker.

## NOT verified

- No Docker image build or container run, and the docker-compose stack was not run.
- Real authorization-svc decisions for `DEADLINE_CREATE` and `DEADLINE_MANAGE` (tests use a stub), and that the actions are registered there.
- The rest of the store suite runs as the connecting superuser; only the new isolation test uses a non-superuser role.
- No event was delivered through Kafka.
- `ListUpcoming` and `ListOverdue` take row locks and stamp the "notified" timestamp on a read. Concurrent reads were not tested.
- Whether anything consumes the deadline events (BIZ-05 tasks, BIZ-10 notifications): no link was found.

## Open items to reach certification

1. ~~Events can be lost: move the five events to the shared eventing outbox.~~ Done (see "Event migration"). Still to do: reconcile the catalogue's unowned `business.deadline.due` with `became-due-soon` and `became-overdue`.
2. Store and evaluate the spec's required inputs: timezone, business calendar (REF-10), deadline rule/profile and escalation plan; record them as evidence. Without them due-date calculation is caller-supplied and the calendar/timezone ambiguity the spec says blocks certification cannot be ruled out.
3. On `DEADLINE_SOURCE_CONFLICT`, create an exception case in this service instead of only refusing.
4. Owner plus supervisor separation of duties: decide who may waive or cancel (not the owner), and add the check.
5. Add a distinct "Due" state if the spec's lifecycle is meant literally, and replace the fixed 72-hour window with a configured one.
6. Add an OpenAPI contract for `/v1/deadlines` (none exists) and a route-drift test.
7. Register and verify `DEADLINE_CREATE` and `DEADLINE_MANAGE` in authorization-svc; run the service in the compose stack.
8. Link to BIZ-05 tasks, BIZ-10 notifications and BIZ-09 comments, which the spec lists as dependencies.
