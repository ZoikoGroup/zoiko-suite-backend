# RUNBOOK — fiscal-calendar-svc

REF-04 Fiscal Calendar. Port **8173**. Postgres database `fiscal_calendar`.
Kafka topic `zoiko.fiscal-calendar.events`. Depends on: Postgres (reads and
writes), authorization-svc (commands only), accounting-period-svc / REF-05
(activation of non-first versions only), Kafka (event delivery only).

> Status: this runbook was written alongside the code. The SQL and the
> migration/rollback steps were exercised against a throwaway PostgreSQL 16
> container used for the tests; nothing else here has been run against a live
> stack (no compose, Kafka, authorization-svc or accounting-period-svc). Treat
> the commands below as the intended procedures, and correct them the first
> time they are used for real.

## 1. What breaks, and what it looks like

| Symptom | Likely cause | Where to look |
|---|---|---|
| Every command answers `503 DEPENDENCY_UNAVAILABLE` "authorization-svc unavailable" | authorization-svc down/unreachable, or mTLS misconfigured | section 4.2 |
| Every command answers `403 FORBIDDEN` | The `FISCAL_CALENDAR_*` actions are not granted/seeded in authorization-svc | section 4.2 |
| `:activate` answers `503` "period history ... unavailable ... failing closed" | accounting-period-svc unreachable, returned non-200, or `ACCOUNTING_PERIOD_URL` wrong | section 4.1 |
| `:activate` answers `409 TRANSITION_PLAN_REQUIRED` | The change takes effect on or before the end of a posted/closed period | section 4.1 |
| `:activate` answers `409 INVALID_TRANSITION` "effective interval ... overlaps" | Another ACTIVE/SUPERSEDED version of the same entity+scope covers those dates | section 4.3 |
| Reads answer `503 DEPENDENCY_UNAVAILABLE` "store unavailable" | Postgres down/pool exhausted | `/readyz`, section 4.5 |
| `:resolve` / `?as_of=` answer `404 NOT_FOUND` | No ACTIVE or SUPERSEDED version covers that (entity, scope, date) | section 4.3 |
| Consumers do not hear about an activation | Outbox relay stalled / Kafka down | section 4.4 |
| `409 VERSION_CONFLICT` | Stale `expected_version`: re-read, re-decide | section 5 |
| `403 SOD_DENIED` | The proposer tried to approve their own version or plan | section 5 |

## 2. First response

```sh
curl -s localhost:8173/healthz            # liveness: process up
curl -s localhost:8173/readyz             # readiness: database reachable (JSON per component)
curl -s localhost:8173/metrics | grep fiscal_calendar_
docker logs fiscal-calendar-svc --tail 100
```

`/readyz` checks Postgres only. authorization-svc and accounting-period-svc are
deliberately not readiness dependencies: reads keep serving without them,
commands fail closed.

## 3. Calling it by hand

Commands need `X-Tenant-Id`, `X-Principal-Id`, `X-Legal-Entity-Id`,
`Idempotency-Key`, a `reason` and (on existing objects) `expected_version`.
Behind the gateway the first three are set for you; locally you supply them.

```sh
H='-H X-Tenant-Id:t1 -H X-Legal-Entity-Id:ent-1 -H Content-Type:application/json'

# Create a calendar + first DRAFT version (calendar months, year starting 1 January)
curl -s $H -H X-Principal-Id:alice -H "Idempotency-Key: $(uuidgen)" -X POST localhost:8173/v1/fiscal-calendars -d '{
  "legal_entity_id":"ent-1","code":"MAIN","scope":"STATUTORY","reason":"initial calendar",
  "pattern":{"type":"CALENDAR_MONTHS"},"fiscal_year_start_month":1,"fiscal_year_start_day":1,"effective_from":"2026-01-01"}'

# A different principal approves, then activates (version from the previous response)
curl -s $H -H X-Principal-Id:bob -H "Idempotency-Key: $(uuidgen)" -X POST \
  "localhost:8173/v1/fiscal-calendar-versions/<version_id>:approve"  -d '{"expected_version":1,"reason":"reviewed"}'
curl -s $H -H X-Principal-Id:bob -H "Idempotency-Key: $(uuidgen)" -X POST \
  "localhost:8173/v1/fiscal-calendar-versions/<version_id>:activate" -d '{"expected_version":2,"reason":"go live"}'

# Queries need only the tenant
Q='-H X-Tenant-Id:t1'
curl -s $Q "localhost:8173/v1/fiscal-calendars/<calendar_id>?as_of=2026-06-30"
curl -s $Q "localhost:8173/v1/fiscal-calendars/<calendar_id>/versions"
curl -s $Q "localhost:8173/v1/fiscal-calendars:resolve?legal_entity_id=ent-1&scope=STATUTORY&date=2026-06-30"
curl -s $Q "localhost:8173/v1/fiscal-calendar-versions/<version_id>/periods-preview?fiscal_year=2026"
```

## 4. Incidents

### 4.1 An activation was refused

`POST /v1/fiscal-calendar-versions/{id}:activate` (APPROVED -> ACTIVE). Guards, in order:

| Response | Meaning | Action |
|---|---|---|
| `409 INVALID_TRANSITION` "cannot move ... from X to ACTIVE" | The version is not APPROVED | approve first (a different principal than the proposer) |
| `409 INVALID_TRANSITION` "effective interval ... overlaps" | section 4.3 | |
| `503 DEPENDENCY_UNAVAILABLE` "period history ... failing closed" | Not the first version of its calendar, and accounting-period-svc did not give a usable answer | check `ACCOUNTING_PERIOD_URL`, REF-05 `/healthz`, and that it serves `GET /v1/calendar-usage?calendar_id=` returning `{"latest_period_end":"YYYY-MM-DD"|null,"has_posted_or_closed_periods":bool}` with 200. A 404 from REF-05 counts as unavailable. Retry the same request (same Idempotency-Key) once it is back. |
| `409 TRANSITION_PLAN_REQUIRED` | REF-05 reports posted/closed periods through a date on or after this version's `effective_from` | create a plan: `POST /v1/fiscal-calendar-versions/{to}/transition-plan` with `from_version_id` (the version being superseded), `affects_posted_periods: true`, an `impact_assessment` and a `mapping` of old period keys to new ones; a DIFFERENT principal than the plan's and the version's proposer approves it with `POST /v1/calendar-transition-plans/{plan}:approve`; then activate again. The message names the latest posted/closed period end. |
| `200` with `Idempotent-Replay: true` | Same Idempotency-Key as an earlier activation: the original result, nothing re-checked | none |

If a plan exists but is still refused: it must be APPROVED, declare
`affects_posted_periods`, and have `from_version_id` equal to the version that
would be superseded (the calendar's open-ended ACTIVE version).

```sql
-- as the service role, with the tenant set (FORCE RLS: a bare query sees nothing)
BEGIN; SELECT set_config('app.tenant_id', '<tenant>', true);
SELECT plan_id, from_version_id, to_version_id, status, affects_posted_periods, proposed_by, decided_by
FROM calendar_transition_plans WHERE calendar_id = '<calendar_id>' ORDER BY created_at;
ROLLBACK;
```

### 4.2 Commands failing: authorization

- `503 DEPENDENCY_UNAVAILABLE` ("authorization-svc unavailable; failing
  closed"): check `AUTHZ_SERVICE_URL`, authorization-svc `/healthz`, and (if
  `AUTHZ_MTLS_ENABLED=true`) mtls-management-svc. Commands are refused until it
  returns; reads are unaffected. Unavailable outcomes are never cached; GRANTED
  and DENIED decisions are cached for 5 s.
- `403 FORBIDDEN`: the principal lacks the action. Actions asked of
  authorization-svc: `FISCAL_CALENDAR_PROPOSE` (create, propose-change, create
  plan), `FISCAL_CALENDAR_APPROVE` (approve version, approve/reject plan),
  `FISCAL_CALENDAR_ACTIVATE`. These must exist in the role catalogue (they are
  not seeded by this service). On a fresh stack *every* command is denied until
  they are seeded.
- Metric: `fiscal_calendar_authz_decisions_total{action,outcome}`.

### 4.3 Overlap, gaps and 404s on resolve

At most one ACTIVE-or-SUPERSEDED version may cover a date for a (tenant, legal
entity, scope), across calendars. Inspect what is in force:

```sql
BEGIN; SELECT set_config('app.tenant_id', '<tenant>', true);
SELECT v.version_id, c.code, v.version_no, v.status, v.effective_from, v.effective_to
FROM fiscal_calendar_versions v JOIN fiscal_calendars c USING (calendar_id)
WHERE v.legal_entity_id = '<entity>' AND v.scope = '<scope>' AND v.status IN ('ACTIVE','SUPERSEDED')
ORDER BY v.effective_from;
ROLLBACK;
```

- Overlap refusal: choose an `effective_from` after the existing version's
  start (it will be ended where yours begins) - or use a different `scope` if
  you genuinely need a parallel basis. A new version starting on or before an
  existing version's start cannot be activated; there is no withdraw (see
  SPEC_DEVIATIONS.md item 8), so propose a correct one that starts later.
- A `404` from `:resolve` means a date no version covers (before the first
  version, after a bounded version's `effective_to`, or the version is only
  DRAFT/APPROVED). The service never falls back to a nearest calendar.
- The database exclusion constraint `fcv_one_in_force_per_interval` is the
  backstop behind the service check. If it ever fires in the logs (raised as
  `INVALID_TRANSITION`) two activations raced past the advisory lock: treat it
  as a defect.

### 4.4 Events not reaching consumers

Events are written to `fiscal_calendar_outbox` in the same transaction as the
change; the relay drains them every 250 ms.

```sql
-- the relay role/policy admits app.outbox_relay; as an operator set it explicitly
BEGIN; SELECT set_config('app.outbox_relay', 'true', true);
SELECT count(*) AS pending, min(created_at) AS oldest FROM fiscal_calendar_outbox WHERE published_at IS NULL;
SELECT outbox_id, event_type, attempts, last_error FROM fiscal_calendar_outbox
WHERE published_at IS NULL ORDER BY created_at LIMIT 20;
ROLLBACK;
```
Metrics: `fiscal_calendar_outbox_pending`,
`fiscal_calendar_outbox_oldest_age_seconds` (the one that matters; depth alone
cannot tell a burst from a stalled relay), `..._outbox_failures_total`.

A growing `attempts` with `last_error` mentioning Kafka means the broker (or
`KAFKA_BROKERS`) is the problem; nothing is lost: rows stay until published.
Delivery is at-least-once, so consumers may see duplicates after a crash
mid-publish. Do **not** hand-edit `published_at` unless you are certain the event
was delivered.

### 4.5 Readiness failing / store unavailable

`/readyz` returns the failing component. A dead pool is this service's problem;
check `DB_HOST/DB_NAME/DB_USER/DB_SSLMODE`, pool saturation (max 20), and
Postgres. The runtime role is `zoiko_app` (DML only); migrations run as the
owner (see section 6).

### 4.6 A period boundary looks wrong in a downstream system

1. `GET /v1/fiscal-calendars/{id}/versions` for the full history, with who
   proposed/approved/activated each version and why.
2. Compare with the consumer's persisted `version_id` / `version_no`.
   `GET /v1/fiscal-calendar-versions/{version_id}/periods-preview?fiscal_year=`
   is deterministic and must reproduce the consumer's period boundaries for
   that version. A superseded version keeps its definition and stays readable.
3. There is no UPDATE path. A wrong definition is corrected by a new version
   (and, if it touches posted/closed history, a transition plan); the old
   version is never edited. A bad definition that was never activated simply
   stays APPROVED/DRAFT (there is no withdraw).

## 5. The refusal vocabulary

| code | HTTP | meaning |
|---|---|---|
| `CONTEXT_INVALID` | 400 / 401 / 422 | malformed request, missing Idempotency-Key / reason / expected_version / tenant / actor / legal entity; invalid pattern or dates; legal-entity context does not match the object; Idempotency-Key reused for a different request |
| `VERSION_CONFLICT` | 409 | `expected_version` is stale (also: a concurrent proposal took the version number) |
| `DUPLICATE_CANDIDATE` | 409 | calendar code already exists for the entity; a live transition plan already exists for the version |
| `INVALID_TRANSITION` | 409 | illegal lifecycle move; effective-interval overlap; preview of a DRAFT; plan created from a version never in force |
| `TRANSITION_PLAN_REQUIRED` | 409 | change touches posted/closed history without an approved plan |
| `SOD_DENIED` | 403 | proposer may not approve their own version / plan |
| `DEPENDENCY_UNAVAILABLE` | 503 | authorization-svc, accounting-period-svc (activation) or the store is unavailable (commands fail closed) |
| `NOT_FOUND` | 404 | unknown id (or another tenant's); no version in force for resolve / as_of |
| `FORBIDDEN` | 403 | authorization-svc denied the action |
| `RULE_AMBIGUOUS` | 409 | two in-force versions for one date: should be impossible; treat as a defect |

## 6. Migrations and rollback

Applied by `deployments/init-db.sh` on a fresh volume only (globbed from
`deployments/migrations/*.up.sql`, sorted) once the parent registers the
database. On an existing stack apply new migrations by hand as the DB owner.
`*.down.sql` files are the manual rollback path and are never run
automatically. 000001 runs `CREATE EXTENSION IF NOT EXISTS btree_gist`
(trusted extension on PostgreSQL 13+; a superuser must pre-create it if the
migration role cannot).

| migration | contents |
|---|---|
| 000001 | `fiscal_calendars`, `fiscal_calendar_versions` (with the exclusion constraint), `calendar_transition_plans`, `fiscal_calendar_status_history`; all tenant_id + FORCE RLS |
| 000002 | triggers: no delete anywhere, append-only history, calendar identity immutable, version definition immutable + forward-only status + `effective_to` set once on supersession, plan content immutable + decision once |
| 000003 | `fiscal_calendar_idempotency` (tenant_id + FORCE RLS) |
| 000004 | `fiscal_calendar_outbox` (FORCE RLS; relay admitted via `app.outbox_relay`) |

Rolling back **destroys the calendar history** (the tables are dropped). Never
run it on a database whose calendars have been used by period or accounting
records.

## 7. Configuration

| env | default | purpose |
|---|---|---|
| `PORT` | 8173 | HTTP |
| `DB_HOST/PORT/NAME/USER/PASSWORD/SSLMODE` | localhost/5432/fiscal_calendar/postgres/""/require | Postgres |
| `KAFKA_BROKERS` | localhost:9092 | brokers |
| `KAFKA_EVENTS_TOPIC` | zoiko.fiscal-calendar.events | topic |
| `AUTHZ_SERVICE_URL` | http://authorization-svc:8089 | authorization-svc |
| `ACCOUNTING_PERIOD_URL` | http://accounting-period-svc:8174 | REF-05 `calendar-usage` (the port is a guess; set it) |
| `AUTHZ_MTLS_ENABLED` / `AUTHZ_MTLS_URL` / `MTLS_MANAGEMENT_SERVICE_URL` | false / https://authorization-svc:8449 / http://mtls-management-svc:8140 | material-path mTLS (authorization-svc calls only; the REF-05 client does not use mTLS) |
| `ZS_ENVELOPE_ENFORCEMENT` | write-strict | canonical input envelope mode |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | http://otel-collector:4318 | tracing |
| `TEST_DATABASE_URL` | unset | enables the Postgres test suite (and overrides the DSN) |

## 8. Re-proving the service

```sh
cd services/fiscal-calendar-svc
go build ./... && go vet ./... && go test ./... -count=1
# Postgres suite: needs a NOSUPERUSER NOBYPASSRLS role that owns the database and schema
# (FORCE RLS binds the owner; a superuser makes the RLS test skip itself).
docker run -d --name fiscal-cal-pgtest -e POSTGRES_PASSWORD=pgtest -e POSTGRES_DB=fiscal_calendar_test -p 127.0.0.1:55433:5432 postgres:16-alpine
docker exec fiscal-cal-pgtest psql -U postgres -d fiscal_calendar_test \
  -c "CREATE ROLE app_plain LOGIN PASSWORD 'plain' NOSUPERUSER NOBYPASSRLS" \
  -c "ALTER DATABASE fiscal_calendar_test OWNER TO app_plain" \
  -c "DROP SCHEMA public CASCADE" -c "CREATE SCHEMA public AUTHORIZATION app_plain"
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='postgres://app_plain:plain@127.0.0.1:55433/fiscal_calendar_test?sslmode=disable' \
  go test ./internal/store -count=1 -v
docker rm -f fiscal-cal-pgtest
```
The Postgres suite drops and recreates this service's tables in the target
database; point it at a scratch database. Counting rows in a FORCE-RLS table
needs `set_config('app.tenant_id', ...)` inside a transaction first; a bare
`SELECT count(*)` returns 0.
