# BIZ-05 Task / Case Management — Certification Status

**Service host:** exception-escalation-svc (BIZ-05 is bolted onto this service; no separate service)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 7
**Status: NOT CERTIFIED.** Core contract implemented; two defects fixed in this audit; several spec items open.

A certificate listing only successes certifies nothing, so the last sections say what was **not** verified.

## Contract coverage (spec section 7)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateTask, Assign, Start, Block, Escalate, Complete, Reopen, CloseCase, Cancel (+ CreateCase) | All routed | `internal/handler/task_handler.go` |
| Queries: GetTask, ListQueue, GetCase, GetSLAState, GetLinkedObjectStatus, GetHistory | All routed | same |
| Lifecycle New -> Assigned -> In Progress -> Blocked/Escalated -> Completed -> Closed; Reopened governed | State edges enforced in the store; the database CHECK constrains status *values* only, not edges | `internal/store/task_store.go`, migration 000003 |
| Append-only history | `task_transitions` rows written in the same transaction as each change; update/delete refused by trigger | migration 000003 |
| Events (6) | `task.created`, `task.assigned`, `task.escalated`, `task.completed`, `task.reopened`, `case.closed` | handler |
| Failure: SLA clock failure never silently closes/escalates | SLA state is computed at read time; nothing auto-transitions | `GetSLAState` |
| Failure: linked-domain failure surfaced distinctly | `GetLinkedObjectStatus` is a passthrough that reports `tracked: false`; no live lookup | `GetLinkedObjectStatus` |
| Authorization / SoD: assignee/team + supervisor | **Not implemented** — any holder of `TASK_MANAGE` can assign/start/block/escalate any task in the entity; `TASK_CLOSE` gates complete and case close; no assignee check | handler |
| Sensitive case purpose restrictions | **Not implemented** | — |
| Evidence: required evidence / completion criteria | **Not enforced** — `required_evidence` is stored but `CompleteTask` does not require it | `CompleteTask` |
| Dependencies: BIZ-08 deadlines, BIZ-09 collaboration | Not linked | — |
| API contract | **None** | — |

## Changes made in this audit

- **Task creation now validates its case.** `CreateTask` accepted any `case_id`: a task could join a nonexistent case, a CLOSED case (and then never be closed by the case-close cascade), a case of a different legal entity, or an id belonging to another tenant. It now requires an existing, OPEN case of the same legal entity and tenant (row lock `FOR SHARE`, so a concurrent `CloseCase` cannot race it). Errors: `case not found` (404), `case is already closed` (409), `task legal entity does not match the case legal entity` (422).
- **Reopening a closed task clears `closed_by`.** `closed_at` was cleared but `closed_by` was left, so a reopened task still named its previous closer.
- **Stale tests repaired.** Before this audit the service's store tests already failed on the unmodified code (tasks, cases and deadlines "not found"): the helper `ctx()` built a tenant-less context, but a recent tenant-isolation fix made the store read the tenant from the context. `ctx()` now carries tenant `default`. `.github/workflows/ci.yml` does not reference this service at all (not even in its build matrix), which is why nobody saw it.
- New tests: case must exist / be open / match entity; reopen-from-closed clears closed fields; cross-tenant isolation for cases and tasks, with a raw RLS probe for non-superuser roles. Mutation check: the first two fail without the fix.

## Implementation gate

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Does not own linked-object state (`tracked:false`); nothing posts to another domain | Partial (by inspection) |
| Tenant isolation | Every task/case query filters `tenant_id` and RLS is forced; `TestPgStore_Task_TenantIsolation` passes as superuser and as a `NOSUPERUSER NOBYPASSRLS` role | Covered, including RLS |
| Classification / retention | Nothing task- or case-specific found | **Missing** |
| Negative paths | Wrong-state transitions, double close, append-only history, case validation | Mostly covered |
| Evidence lineage | Transition history, SLA deadline, linked object, completion notes recorded; no evidence attachments | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./...                    all packages pass against a throwaway postgres:16-alpine as the
                                 postgres superuser (store suite 37 s, 41 tests)
new BIZ-05 tests                 3/3 pass as a NOSUPERUSER NOBYPASSRLS role
```

## NOT verified

- The rest of the store suite (including BIZ-08 deadlines and findings) was only run as the superuser.
- HTTP behavior of the new case-validation errors was not exercised through the handler; only the store behavior is tested.
- No real authorization-svc, Kafka or docker-compose stack.
- Events are published **after** the database commit and a failure is only logged (not via a transactional outbox), so an event can be lost on a crash between commit and publish. Not changed here.

## Open items

1. Assignee/supervisor separation of duties and sensitive-purpose restrictions: policy decisions.
2. Enforce `required_evidence` at completion: needs a decision on what counts as evidence (text vs attachment).
3. Add this service to CI (build, vet and database tests) so these tests stop being invisible.
4. OpenAPI contract and event contract for BIZ-05.
5. Database trigger for task status edges (the CHECK allows any value-to-value change by direct SQL).
6. Move task and case events to the transactional outbox.
7. Classification / retention policy for tasks and cases.
