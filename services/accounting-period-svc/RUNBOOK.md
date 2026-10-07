# RUNBOOK — accounting-period-svc

REF-05 Accounting Period. Port **8174**. Postgres database `accounting_period`.
Kafka topic `zoiko.accounting-period.events`. Depends on: Postgres (everything),
authorization-svc (state commands and materialise only), fiscal-calendar-svc
REF-04 (materialise only), financial-close-svc ACC-14 (state commands only),
Kafka (event delivery only).

> Status: written alongside the code. The Postgres-facing parts were exercised on
> 2026-10-07 against a scratch PostgreSQL 16; **nothing here has been exercised
> against a running stack** (no compose, Kafka, authorization-svc, REF-04 or
> ACC-14). Treat the procedures as intended, and correct them on first real use.

## 0. Read this first: ACC-14 endpoint does not exist yet

State commands verify evidence with
`GET {FINANCIAL_CLOSE_URL}/v1/close/workflow-refs/{ref}` on financial-close-svc.
That endpoint is built in the later GL / close cutover step. Until then every
`request-soft-close`, `hard-close`, `authorize-reopen` and `reclose` answers
`422 SOURCE_UNVERIFIED` (404 from financial-close-svc) or `503
DEPENDENCY_UNAVAILABLE`. **That is correct fail-closed behaviour, not an
outage.** The gate, listings and materialise keep working.

Expected 200 body of that endpoint (all five must match the command, or the
command is refused):

```json
{"workflow_ref":"WF-1","period_key":"FY2026-P03","legal_entity_id":"<entity>",
 "command":"HARD_CLOSE","status":"APPROVED","control_snapshot_ref":"SNAP-1"}
```
`command` is one of `SOFT_CLOSE`, `HARD_CLOSE`, `AUTHORIZE_REOPEN`, `RECLOSE`.

## 1. What breaks, and what it looks like

| Symptom | Likely cause | Where to look |
|---|---|---|
| Every state command `422 SOURCE_UNVERIFIED` | ACC-14 endpoint missing (see 0) or ref not APPROVED / for another period, command or snapshot | message text; section 0 |
| State commands `503 DEPENDENCY_UNAVAILABLE` "ACC-14 ... unreachable" | financial-close-svc down / 5xx / throttled; `FINANCIAL_CLOSE_URL` wrong | section 4.2 |
| State commands `503` "authorization-svc unavailable" | authorization-svc down or mTLS misconfigured | section 4.1 |
| State commands `403 FORBIDDEN` | `PERIOD_STATE_COMMAND` / `PERIOD_MATERIALIZE` not granted or not seeded | section 4.1 |
| `403 SOD_DENIED` on hard-close / reopen | same actor requested the soft close | section 5 |
| Gate answers `404 PERIOD_NOT_FOUND` for a date you expect | year never materialised, other entity / book scope, tenant header wrong | section 4.3 |
| Gate answers `409 RULE_AMBIGUOUS` | two periods cover the date with different states (e.g. two calendar versions) | section 4.4 |
| Gate says not allowed in `REOPEN_AUTHORIZED` | reopen window expired (`REOPEN_WINDOW_EXPIRED`) or request outside reopen scope | section 4.5 |
| Materialise `422 CONTEXT_INVALID` "not contiguous" / "fiscal year" | REF-04 preview inconsistent | section 4.6 |
| Materialise `503` / `404` | fiscal-calendar-svc down / unknown version | section 4.6 |
| `409 VERSION_CONFLICT` | stale `expected_version`; re-read, re-decide | section 5 |
| Consumers do not hear about a close | outbox relay stalled / Kafka down | section 4.7 |
| Reads `503` "store unavailable" | Postgres down / pool exhausted | `/readyz` |

## 2. First response

```sh
curl -s localhost:8174/healthz     # liveness
curl -s localhost:8174/readyz      # readiness: Postgres only (by design)
curl -s localhost:8174/metrics | grep accounting_period_
```
`/readyz` checks Postgres only. The gate (the C0/C1 path) calls neither
authorization-svc, REF-04 nor ACC-14, so it must keep answering through their
outages.

## 3. Calling it by hand

Every call needs `X-Tenant-Id`; commands also `X-Principal-Id`, `Idempotency-Key`
and a `reason`. Behind the gateway the headers are injected.

```sh
# gate
curl -s -H 'X-Tenant-Id: t1' \
 'localhost:8174/v1/accounting-periods:resolve?legal_entity_id=E1&date=2026-03-10&purpose=post'
# materialise
curl -s -XPOST -H 'X-Tenant-Id: t1' -H 'X-Principal-Id: steward' -H 'Idempotency-Key: m-2026' \
 -d '{"legal_entity_id":"E1","calendar_id":"C1","fiscal_year":2026,"calendar_version_id":"V1","reason":"year start"}' \
 localhost:8174/v1/accounting-periods:materialize
# hard close
curl -s -XPOST -H 'X-Tenant-Id: t1' -H 'X-Principal-Id: approver' -H 'Idempotency-Key: hc-1' \
 -d '{"expected_version":2,"reason":"close","acc14_workflow_ref":"WF-1","control_snapshot_ref":"SNAP-1"}' \
 'localhost:8174/v1/accounting-periods/<period_id>:hard-close'
```

## 4. Procedures

### 4.1 authorization-svc
Seed actions `PERIOD_STATE_COMMAND` and `PERIOD_MATERIALIZE` for the intended
principals (not done by this change). `403 FORBIDDEN` = denied; `503` = down.
Authorization is evaluated against the period's legal entity.

### 4.2 ACC-14 unreachable
Fail closed by design: no state change, a `PeriodCommandRejected` event is
recorded. Restore financial-close-svc and retry the same command with the same
`Idempotency-Key` (a refused command stores nothing, so the retry runs fresh).

### 4.3 "No period" from the gate
This is the control working: **never** work around it by assuming OPEN. Check
the period exists:
`GET /v1/accounting-periods?legal_entity_id=E1` (tenant header required), then
`:materialize` the missing fiscal year. Remember book/module scope: a request
presenting `book_scope=B1` matches entity-wide periods and `B1` periods only.

### 4.4 RULE_AMBIGUOUS
List periods for the entity and find overlapping ones:
`GET /v1/accounting-periods?legal_entity_id=E1`. Typical causes: a second
fiscal-calendar version materialised over dates whose first-version period is
already closed; a SPECIAL period overlapping a NORMAL one (callers should pass
`kind=`); an entity-wide period and a book period in different states. Resolution
is an operational decision (there is no automatic precedence); nothing can be
deleted — close or reopen the periods through their commands so the states agree.

### 4.5 Reopen windows
`authorize-reopen` needs `reopen_scope` and a future `expires_at` within
`REOPEN_MAX_WINDOW` (default 72h). After `expires_at` the gate refuses posting
immediately, but the stored state stays `REOPEN_AUTHORIZED` until a `reclose`
command is run (it is allowed after expiry). There is no sweeper and no expiry event.

### 4.6 Materialise
Uses REF-04 `periods-preview` and (without a pinned version) `:resolve`. It never
moves an existing boundary; `boundary_drift` in the response lists periods whose
stored boundaries differ from REF-04's current preview. Investigate drift with
the REF-04 owners; it is not auto-corrected and cannot be (database trigger).

### 4.7 Outbox
```sql
-- as the service role, inside a tx with set_config('app.outbox_relay','true',true)
SELECT count(*), min(created_at) FROM accounting_period_outbox WHERE published_at IS NULL;
```
Metrics: `accounting_period_outbox_pending`, `..._oldest_age_seconds`,
`..._failures_total`. Delivery is at-least-once; the relay resumes on restart.

### 4.8 Control events
Refused close/reopen attempts appear as `PeriodCommandRejected` with
`rejection_code` (`CONTEXT_INVALID` = missing ACC-14 refs, `SOURCE_UNVERIFIED`,
`DEPENDENCY_UNAVAILABLE`, `SOD_DENIED`). Wire them to the security monitoring
feed; no alert rules were added in this change.

## 5. Concurrency and SoD
Two commands racing on one period: exactly one wins; the other gets
`409 VERSION_CONFLICT` (proved in the Postgres suite). SoD needs a second person:
whoever requested the soft close cannot hard-close or authorize a reopen.

## 6. Backup / restore
Standard Postgres backup of database `accounting_period`. `period_state_history`
is the audit record and is append-only; restore must keep it. **No backup or
restore procedure has been rehearsed.** After a restore, reconcile that every
non-OPEN `accounting_periods.version - 1` equals the count of non-MATERIALIZE
history rows for that period.

## 7. Configuration

| Env | Default | Meaning |
|---|---|---|
| `PORT` | 8174 | listen port |
| `DB_HOST/PORT/NAME/USER/PASSWORD/SSLMODE` | localhost / 5432 / accounting_period / postgres / "" / require | database |
| `KAFKA_BROKERS`, `KAFKA_EVENTS_TOPIC` | localhost:9092, zoiko.accounting-period.events | events |
| `AUTHZ_SERVICE_URL` | http://authorization-svc:8089 | authorization-svc |
| `FISCAL_CALENDAR_URL` | http://fiscal-calendar-svc:8173 | REF-04 (port is a guess) |
| `FINANCIAL_CLOSE_URL` | http://financial-close-svc:8104 | ACC-14 evidence |
| `REOPEN_MAX_WINDOW` | 72h | max distance of a reopen `expires_at` |
| `AUTHZ_MTLS_*`, `MTLS_MANAGEMENT_SERVICE_URL` | off | as in the template service |
