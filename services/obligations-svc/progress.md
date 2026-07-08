# obligations-svc — Progress

## Status: v1 built and verified end-to-end (2026-07-08)

Built from scratch per `docs/architecture/03-microservices.md` §8.5 and
`docs/architecture/04-data-model.md` §13.1.

## What's implemented

- `Obligation` CRUD: create (idempotent on `obligation_code`), get, list
  (filterable by `legal_entity_id`, `jurisdiction_id`, `obligation_type`,
  `status`, `due_before`/`due_after`)
- `FilingRequirement` CRUD scoped under a parent obligation: create, list
- Obligation status state machine: `OPEN` → `IN_PROGRESS`/`OVERDUE`/`CLOSED`,
  `IN_PROGRESS` → `OVERDUE`/`CLOSED`, `OVERDUE` → `CLOSED`. `CLOSED` is
  terminal. Idempotent on repeat requests for the current status.
- Critical constraint enforced: every obligation is entity-bound
  (`legal_entity_id`) and jurisdiction-bound (`jurisdiction_id`) — the
  latter is validated synchronously against the real jurisdiction-rules-svc
  on creation, fail-closed (an unreachable jurisdiction-rules-svc rejects
  the write with 503, never silently accepts an unvalidated ID).
- Critical enhancement "Atomic Linking" enforced: `source_reference` is a
  required field, both at the API validation layer and the DB `NOT NULL`
  constraint.
- Real Kafka producer: `obligation.created`, `obligation.updated`,
  `obligation.overdue`, `obligation.closed` — same pattern as
  identity-context-svc/tenant-entity-registry-svc/policy-svc.

## Deliberate v1 scope decisions (not oversights)

- **`ComplianceStatus`, `ExceptionCase`, `EscalationRecord` not built.**
  These are mentioned in 04-data-model.md §13.1 and the ERD ties them to
  `LegalEntity` as siblings of `Obligation`, not children of it — they read
  as a different service's entities (likely Exception & Escalation
  Service / Compliance Status, per the §5.2 catalogue), not core to
  obligations-svc. Revisit if a future spec section assigns them here
  explicitly.
- **No built-in scheduler for the `OPEN`/`IN_PROGRESS` → `OVERDUE`
  transition.** `obligation.overdue` is only emitted when something calls
  `POST /v1/obligations/{id}/status` with `"OVERDUE"`. Detecting *which*
  obligations have passed their `due_date` is left to an external caller
  (cron job, orchestrator) via `GET /v1/obligations?status=OPEN&due_before=<now>`.
  Building a scheduler is out of scope for this service.
- **No Authorization Service wiring on admin writes** — it doesn't exist
  yet. Same posture policy-svc and governance-decision-log-svc shipped
  with; revisit when Authorization Service exists.
- **No consumed events in v1.** This is a producer-only service for now —
  simpler to reason about, matching the pattern of some other single-purpose
  spine services early in their lifecycle.

## Verified (real infra, not mocks)

- `go build`/`go vet`/`go test` clean
- 7 store-layer integration tests against a real PostgreSQL instance
  (idempotency + 409 conflict, not-found, filtered listing, legal state
  transitions including the terminal-CLOSED and illegal-skip cases,
  filing requirement CRUD)
- Booted the real Docker image against the live platform compose stack
  (real Postgres, real Kafka, real jurisdiction-rules-svc):
  - Fail-closed jurisdiction validation confirmed against both a
    nonexistent jurisdiction (404) and a real one (201 success)
  - `obligation.created` event independently consumed off the real
    `zoiko.obligations.events` Kafka topic — not just a successful
    `WriteMessages` call
  - Legal and illegal status transitions confirmed via live HTTP calls
    (200 and 409 respectively)
  - Filing requirement creation and listing confirmed via live HTTP calls

## Not yet done

- Not wired into `deployments/docker-compose.yml` — that file currently
  has an unmerged, in-flight set of changes on another branch
  (`fix/docker-platform-gaps`) that this would conflict with if added
  here. Wiring obligations-svc into the compose stack should happen as a
  small follow-up once that branch merges.
