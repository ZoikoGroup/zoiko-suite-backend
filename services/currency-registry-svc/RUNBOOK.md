# RUNBOOK — currency-registry-svc

REF-02 Currency Registry. Port **8172**. Postgres database `currency_registry`.
Kafka topic `zoiko.currency-registry.events`. Depends on: Postgres (reads and
writes), authorization-svc (commands only), Kafka (event delivery only).

> Status: this runbook was written alongside the code and has **not** been
> exercised against a running stack (no Docker daemon / Postgres was available
> when the service was built). Treat the commands below as the intended
> procedures, and correct them the first time they are used for real.

## 1. What breaks, and what it looks like

| Symptom | Likely cause | Where to look |
|---|---|---|
| Every command answers `503 DEPENDENCY_UNAVAILABLE` "authorization-svc unavailable" | authorization-svc down/unreachable, or mTLS misconfigured | section 4.2 |
| Every command answers `403 FORBIDDEN` | The `CURRENCY_*` actions are not granted/seeded in authorization-svc | section 4.2 |
| Reads answer `503 DEPENDENCY_UNAVAILABLE` "store unavailable" | Postgres down/pool exhausted | `/readyz`, section 4.4 |
| Consumers do not hear about a restriction/retirement | Outbox relay stalled / Kafka down | section 4.3 |
| A planned ISO update "did nothing" | Import was QUARANTINED (201 with `status: QUARANTINED`) or replayed (200) | section 4.1 |
| `:validate` says `UNKNOWN_CURRENCY` for everything | Registry empty (fresh database: nothing is built in) | section 3, then 4.1 |
| `409 VERSION_CONFLICT` on a command | Stale `expected_version`: re-read, re-decide | section 5 |
| `403 SOD_DENIED` on activate | The actor applied the import that introduced/last changed that currency | section 4.5 |

## 2. First response

```sh
curl -s localhost:8172/healthz            # liveness: process up
curl -s localhost:8172/readyz             # readiness: database reachable (JSON per component)
curl -s localhost:8172/metrics | grep currency_registry_
docker logs currency-registry-svc --tail 100
```

`/readyz` checks Postgres only. authorization-svc is deliberately not a
readiness dependency: reads keep serving without it, commands fail closed.

## 3. Calling it by hand

Commands need `X-Tenant-Id`, `X-Principal-Id`, `Idempotency-Key`, a `reason`
and (on existing objects) `expected_version`. Behind the gateway the first two
are set for you; locally you supply them.

```sh
H='-H X-Tenant-Id:t1 -H X-Principal-Id:alice -H Content-Type:application/json'

# What does the registry think of a code?  (never guesses; unknown => supported:false)
curl -s "localhost:8172/v1/currencies:validate?code=AAA&operation=post"

# Read (retired currencies are still 200). as_of is valid time.
curl -s "localhost:8172/v1/currencies/AAA?as_of=2026-01-01T00:00:00Z"
curl -s  localhost:8172/v1/currencies/AAA/minor-unit-versions

# Import. manifest_hash = sha256("zoiko.currency-manifest.v1\n" + sorted lines),
#   line = alpha|numeric|<Go-quoted name>|minor_unit|<true|false>
# Easiest way to compute it is the Go helper domain.ManifestHash (or reproduce
# the algorithm in the producer; it is described in openapi.yaml).
curl -s $H -H "Idempotency-Key: $(uuidgen)" -X POST localhost:8172/v1/currency-imports -d @import.json

# Lifecycle (needs a different principal than the importer for :activate)
curl -s $H -H "X-Principal-Id: bob" -H "Idempotency-Key: $(uuidgen)" -X POST \
  "localhost:8172/v1/currencies/<currency_id>:activate" -d '{"expected_version":1,"reason":"FA-123","approver_id":"cfo"}'
```

## 4. Incidents

### 4.1 An import did not apply

`POST /v1/currency-imports` outcomes:

| Response | Meaning | Action |
|---|---|---|
| `201`, `status: APPLIED` | Done. `summary` says created / minor_unit_changed / attributes_changed / unchanged | none |
| `200` + `Idempotent-Replay: true` | Same Idempotency-Key, or same (source, version, hash) already imported: the ORIGINAL result | none (nothing was re-applied) |
| `201`, `status: QUARANTINED` | At least one row was invalid; **the whole file was not applied** | read `row_errors`; fix the source; resubmit with the corrected rows under a NEW source_version |
| `422 SOURCE_UNVERIFIED` | `manifest_hash` != hash recomputed from `rows`. Nothing stored | the producer's hash algorithm or the payload is wrong (e.g. row mutated after hashing) |
| `409 DUPLICATE_CANDIDATE` | This (source, version) was imported before with a different manifest | use a new source_version; do not reuse versions |

Evidence for any stored import:

```sh
curl -s "localhost:8172/v1/currency-imports?source_name=<s>&source_version=<v>"
```
```sql
SELECT import_id, status, row_count, quarantine_reason, row_errors, actor, created_at
FROM currency_imports ORDER BY created_at DESC LIMIT 20;   -- also holds the submitted `rows`
```

Quarantine is never "released" by this service; correction means a new import.
Common quarantine causes: alpha code not 3 uppercase letters; numeric code not 3
digits; `minor_unit` outside 0..6 or not an integer; duplicate codes in the
file; numeric code that differs from the registered one for that alpha code, or
is held by another currency; alpha code that belongs to a RETIRED currency; a
minor-unit change whose `effective_at` is not after the current version's
`valid_from`.

### 4.2 Commands failing: authorization

- `503 DEPENDENCY_UNAVAILABLE` ("authorization-svc unavailable; failing
  closed"): check `AUTHZ_SERVICE_URL`, authorization-svc `/healthz`, and (if
  `AUTHZ_MTLS_ENABLED=true`) mtls-management-svc. Commands are refused until it
  returns; reads are unaffected. Unavailable outcomes are never cached; GRANTED
  and DENIED decisions are cached for 5 s.
- `403 FORBIDDEN`: the principal lacks the action. Actions asked of
  authorization-svc: `CURRENCY_IMPORT`, `CURRENCY_ACTIVATE`, `CURRENCY_RESTRICT`,
  `CURRENCY_RETIRE`, `CURRENCY_TENANT_ENABLE`, `CURRENCY_TENANT_DISABLE`. These
  must exist in the role catalogue (they are not seeded by this service). On a
  fresh stack *every* command is denied until they are seeded.
- Metric: `currency_registry_authz_decisions_total{action,outcome}`.

### 4.3 Events not reaching consumers

Events are written to `currency_outbox` in the same transaction as the change;
the relay drains them every 250 ms.

```sql
SELECT count(*) AS pending, min(created_at) AS oldest FROM currency_outbox WHERE published_at IS NULL;
SELECT outbox_id, event_type, attempts, last_error FROM currency_outbox
WHERE published_at IS NULL ORDER BY created_at LIMIT 20;
```
Metrics: `currency_registry_outbox_pending`,
`currency_registry_outbox_oldest_age_seconds` (the one that matters; depth alone
cannot tell a burst from a stalled relay), `..._outbox_failures_total`.

A growing `attempts` with `last_error` mentioning Kafka means the broker (or
`KAFKA_BROKERS`) is the problem; nothing is lost: rows stay until published.
Delivery is at-least-once, so consumers may see duplicates after a crash
mid-publish. Do **not** hand-edit `published_at` unless you are certain the event
was delivered.

### 4.4 Readiness failing / store unavailable

`/readyz` returns the failing component. A dead pool is this service's problem;
check `DB_HOST/DB_NAME/DB_USER/DB_SSLMODE`, pool saturation (max 20), and
Postgres. The runtime role is `zoiko_app` (DML only); migrations run as the
owner (see section 6).

### 4.5 SoD denial on activate

`403 SOD_DENIED`: `currencies.last_import_actor` equals the caller. By design a
*different* actor must activate. "Last import actor" is the actor of the import
that introduced or most recently changed the currency (name, flag or minor
unit), so if an import changes a RESTRICTED currency, its importer cannot
re-activate it either. A currency that is already SUPPORTED stays SUPPORTED when
an import changes it; no re-activation happens.

### 4.6 A minor unit looks wrong in a downstream system

1. `GET /v1/currencies/<code>/minor-unit-versions` for the full history with
   evidence (`source_version`, `import_id`, `evidence_ref`).
2. Compare with the consumer's persisted `currency_version` /
   `minor_unit_valid_from` (from its earlier `:validate` response). A change
   never alters earlier versions; historical amounts must be interpreted with
   the version in force at their own effective time (`?as_of=`).
3. There is no UPDATE path. A wrong value is corrected by a new import with a
   later `effective_at` (backdated correction is **not** supported; see
   SPEC_DEVIATIONS.md item 6).

## 5. The refusal vocabulary

| code | HTTP | meaning |
|---|---|---|
| `CONTEXT_INVALID` | 400 / 401 / 422 | malformed request, missing Idempotency-Key / reason / expected_version / tenant / actor; or semantically invalid context |
| `VERSION_CONFLICT` | 409 | `expected_version` is stale |
| `DUPLICATE_CANDIDATE` | 409 | (source, version) already imported with a different manifest; concurrent duplicate insert |
| `INVALID_TRANSITION` | 409 | illegal lifecycle move (including self-transitions, anything from RETIRED, KNOWN -> RETIRED) |
| `SOURCE_UNVERIFIED` | 422 | manifest hash mismatch |
| `SOD_DENIED` | 403 | importer may not activate |
| `REFERENCE_RETIRED` | 409 | tenant tried to enable a retired currency |
| `DEPENDENCY_UNAVAILABLE` | 503 | authorization-svc or the store is unavailable (commands fail closed) |
| `NOT_FOUND` | 404 | unknown currency/code/import (never existed, or not yet at `as_of`) |
| `FORBIDDEN` | 403 | authorization-svc denied the action |

## 6. Migrations and rollback

Applied by `deployments/init-db.sh` on a fresh volume only (globbed from
`deployments/migrations/*.up.sql`, sorted). On an existing stack apply new
migrations by hand as the DB owner. `*.down.sql` files are the manual rollback
path and are never run automatically.

| migration | contents |
|---|---|
| 000001 | global tables: `currencies`, `currency_imports`, `currency_minor_unit_versions`, `currency_status_history` (no tenant_id / no RLS by design: see the header comment) |
| 000002 | triggers: append-only (minor-unit versions, status history, imports), no DELETE on `currencies`, immutable codes, RETIRED terminal |
| 000003 | `tenant_currency_support` and `currency_idempotency` (tenant_id + FORCE RLS) |
| 000004 | `currency_outbox` (FORCE RLS; relay admitted via `app.outbox_relay`) |

Rolling back 000001-000004 **destroys the registry's history** (the tables are
dropped). Never run it on a database whose currency history has been used by
downstream financial records.

## 7. Configuration

| env | default | purpose |
|---|---|---|
| `PORT` | 8172 | HTTP |
| `DB_HOST/PORT/NAME/USER/PASSWORD/SSLMODE` | localhost/5432/currency_registry/postgres/""/require | Postgres |
| `KAFKA_BROKERS` | localhost:9092 | brokers |
| `KAFKA_EVENTS_TOPIC` | zoiko.currency-registry.events | topic |
| `AUTHZ_SERVICE_URL` | http://authorization-svc:8089 | authorization-svc |
| `AUTHZ_MTLS_ENABLED` / `AUTHZ_MTLS_URL` / `MTLS_MANAGEMENT_SERVICE_URL` | false / https://authorization-svc:8449 / http://mtls-management-svc:8140 | material-path mTLS |
| `ZS_ENVELOPE_ENFORCEMENT` | write-strict | canonical input envelope mode |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | http://otel-collector:4318 | tracing |
| `TEST_DATABASE_URL` | unset | enables the Postgres test suite (and overrides the DSN) |

## 8. Re-proving the service

```sh
cd services/currency-registry-svc
go build ./... && go vet ./... && go test ./... -count=1
# Postgres suite (needs a NON-superuser role for the RLS test, else that test skips itself):
TEST_DATABASE_URL=postgres://user:pass@localhost:5432/currency_registry_test?sslmode=disable \
  REQUIRE_DB_TESTS=1 go test ./internal/store -count=1 -v
```
The Postgres suite drops and recreates this service's tables in the target
database; point it at a scratch database.
