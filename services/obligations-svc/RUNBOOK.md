# obligations-svc — Runbook

## Dependencies (fail closed)

| Dependency | Used for | When down |
|---|---|---|
| PostgreSQL (`obligations`) | everything | not ready (`/readyz` 503) |
| authorization-svc | every action check | 503 `authorization_unavailable` |
| tenant-entity-registry-svc | the legal entity of every write | 503 |
| jurisdiction-rules-svc | jurisdiction check; clock due dates (`POST /v1/obligation-calculations:calculate`) | 503; no clock is started |
| evidence-requirements-svc | closing an obligation; fulfilling a clock | 503 `evidence_requirements_unavailable` |
| workflow-svc | waiver / N/A / exemption approvals; escalation cases | 503 `workflow_unavailable`; case requests stay queued |
| Kafka | events (outbox) and the calendar topic | events wait in `event_outbox` |

## Configuration

`EVIDENCE_REQUIREMENTS_URL`, `WORKFLOW_SERVICE_URL` (both required outside local/dev),
`KAFKA_CALENDAR_TOPIC` (`zoiko.jurisdiction.events`; empty disables calendar recalculation),
`CLOCK_SWEEP_INTERVAL_SECONDS` (default 60), `SERVICE_PRINCIPAL_ID` (`svc-obligations`).

## Deploy

1. `deployments/migrate.py up --db obligations` (000015 drops the three D-OB-1 tables; 000016 is the
   clock schema). Check `migrate.py status --db obligations` shows 0 pending.
2. access-control 000062 and authorization 000044 applied (actions, templates, `svc-obligations`).
3. Rebuild the image from host binaries or `docker compose build obligations-svc`, recreate.

## Operations

### A clock reads OVERDUE but no overdue event arrived
The state is computed at read time; the event waits for the sweep. Check
`obligation_clock_sweep_lag_seconds` and the log (`clock sweep`). Nothing is lost: the sweep
persists the move with `effective_at = due_at` whenever it runs.

### A deadline is wrong
Never edit `due_at` (the database refuses it). Correct the calendar or rule in
jurisdiction-rules-svc and publish; the calendar consumer supersedes the live clocks on the older
version. For one clock: `POST /v1/obligations/{id}/clocks/{cid}/recalculate` with a reason.

### Waiver refused with `approval_required`
The approval must be an APPROVED workflow of type `OBLIGATION_WAIVER` (or
`OBLIGATION_NOT_APPLICABLE` / `OBLIGATION_EVIDENCE_EXEMPTION`), subject type `OBLIGATION_CLOCK`,
subject id the clock id, subject fingerprint the one for the exact reason code and evidence refs
sent, and decided by someone other than the requester.

### Escalation cases not opening
`event_outbox` rows with `destination = 'WORKFLOW_CASE'` and `status = 'PENDING'` are retried with
backoff; `obligation_escalation_failures_total{action="OPEN_CASE"}` counts refusals. Check
workflow-svc and that `svc-obligations` holds `CASE_OPEN` in the tenant (authorization 000044).
Parked rows (`status = 'FAILED'`) are requeued with `PgStore.RequeueFailedOutbox`.

### Calendar corrections not applied
The consumer retries an event until every clock on the older version is recalculated; the log
says which clock failed. `consumed_events` holds applied event ids (a replay is skipped).

## Verification

`go test -count=1 -p 1 ./...` with `TEST_DATABASE_URL` on a database whose name contains `test`;
the store suite drops and recreates the `public` schema of that database.
