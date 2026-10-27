# Release Certificate — accounting-period-svc (REF-05)

**Date:** 2026-10-07
**Status: NOT CERTIFIED FOR PRODUCTION.** This document records what was built
and what was verified. It deliberately does **not** claim the spec's Definition
of Done (spec 9.2) is met: several of its gates were not run, one critical
dependency (the ACC-14 verification endpoint) does not exist yet, and the
service is not wired into any deployment. Read "Not verified" before relying on
anything here.

## What this service is

The Accounting Period service (spec REF-05): period instances materialised from a
fiscal-calendar version, the authoritative period state machine
(OPEN -> SOFT_CLOSED -> HARD_CLOSED -> REOPEN_AUTHORIZED -> RECLOSED, and
RECLOSED -> REOPEN_AUTHORIZED), an append-only state history that records the
ACC-14 evidence behind every transition, and the **posting gate**
(`GET /v1/accounting-periods:resolve`) that posting services must consult
server-side. It replaces the fail-open of financial-close-svc
`GET /v1/close/periods/status` ("unregistered period => OPEN") once the GL
cutover (a later, separate step, not started) points at it. It owns
AccountingPeriod, AccountingPeriodState and PeriodStateHistory; it does not own
the calendar definition (REF-04), close workflow evidence (ACC-14) or postings.
Port 8174, database `accounting_period`, topic `zoiko.accounting-period.events`.
Structure and vendored envelope / outbox / telemetry / mTLS / health packages
mirror currency-registry-svc.

## Verification performed

Environment: Windows, Go 1.27.1. Unit and handler tests run without external
services. The Postgres suite and the migrations were run on 2026-10-07 against
PostgreSQL 16 (docker `postgres:16-alpine`) as a `NOSUPERUSER NOBYPASSRLS` role
that owns the schema (FORCE RLS applies to the owner). **No Kafka, no
authorization-svc, no fiscal-calendar-svc, no financial-close-svc and no
docker-compose run were available.**

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l .` | clean |
| `go test ./... -count=1` (no database) | pass: **154 passed, 0 failed, 6 skipped** (the 6 skipped are the Postgres suite) |
| `go test ./... -count=1 -v` with `TEST_DATABASE_URL` as the plain role, `REQUIRE_DB_TESTS=1` | pass: **160 passed (75 top-level + 85 subtests), 0 failed, 0 skipped**; the Postgres suite alone also run with `-count=5` without a failure |
| Down migrations 000004..000001 | apply cleanly, leave zero tables and zero functions; up migrations re-apply afterwards |
| `-race` | **not run** (cgo is unavailable on this machine) |

What the tests cover (handler tests drive the real router, handlers and
business rules over the in-memory transactional store `internal/memstore` with
stub CalendarClient / ProvenanceVerifier / authz; the Postgres suite drives the
real service over the real store):

- State machine: every (state x command) pair, 5 x 4 = 20, through the HTTP API:
  legal pairs land in the documented state with version + 1, one history row and
  one event; all others are `INVALID_TRANSITION` and write nothing. Domain table
  test; no SOFT_CLOSED -> OPEN; second reopen cycle.
- Provenance: missing refs (4 variants) -> `CONTEXT_INVALID` and a control event
  (negative path 30: direct hard-close without evidence; negative path 31:
  reopen without a workflow, event payload checked); verifier failing,
  unreachable or returning an untyped error -> rejected, state unchanged, control
  event, and the same command succeeds once ACC-14 recovers; success records
  both refs, fingerprint, actor and reason in history and the workflow ref in
  the event; the verifier is called with exactly period key / entity / command /
  refs. The HTTP verifier is tested against `httptest` servers for 14 outcomes (plus an unreachable server)
  (404, 403, 500, 502, 429, non-JSON, PENDING, REJECTED, wrong period / entity /
  command / snapshot / workflow, empty object, unreachable) - all fail closed.
- SoD (hard-close and reopen by the soft-close requester -> `SOD_DENIED` + control
  event), version conflict, missing `expected_version`, idempotent replay (same
  key returns the original, no ACC-14 call, no new history/event; different body
  -> 422), authz deny / unavailable, workflow-decision uniqueness, reopen
  time-bound/scoped validation (past, now, over max window, missing scope, scope
  wider than the period), outbox atomicity (failed event write rolls back the
  state change and the materialisation).
- Materialise: creates OPEN periods + history + one `PeriodOpened` each;
  idempotent (replay, re-run); version resolution via REF-04; 9 invalid-calendar
  variants (gap, overlap, duplicate key, bad dates, unknown kind, wrong calendar /
  year / entity, empty) store nothing; calendar down / unknown version; SPECIAL
  period overlap allowed; a second calendar version creates separate periods and
  leaves the first untouched; a changed preview for an existing version reports
  `boundary_drift` and moves nothing.
- Gate: no period -> 404 `PERIOD_NOT_FOUND` with `posting_allowed:false` and
  `Cache-Control: no-store` (negative path: never OPEN); input validation; the
  posting rule per state (OPEN, SOFT_CLOSED with and without the exception flag,
  HARD_CLOSED, REOPEN_AUTHORIZED, RECLOSED); reopen only inside scope and before
  `expires_at` with an injectable clock (one tick before / at expiry); expired
  reopen still reclosable; overlapping calendar versions with different states ->
  `RULE_AMBIGUOUS`; SPECIAL/NORMAL overlap needs `kind`; entity-wide vs book
  periods; commit visible to the next read. Pure `PostingDecision` and
  `CloseStatus` tables.
- Compat `status-by-key`: mapping for all states, `period_name` alias, unknown
  key / entity -> 404 (not OPEN), expired reopen -> CLOSED, most-restrictive
  across several periods. `calendar-usage`: null / latest end / not-OPEN rule /
  version filter.
- Tenant isolation (handler level): another tenant gets 404 / empty / not-found on
  every read and command; same Idempotency-Key in two tenants independent.
- Guards: no hardcoded currency/numeric codes from a probe list and no float types
  in non-test code; gate/compat paths never map absence to OPEN; migrations keep
  their immutability triggers and two FORCE RLS tables; OpenAPI / AsyncAPI parse
  and agree with the code and the outbox CHECK constraint on event names;
  envelope policy.
- **Postgres (6 tests):** full lifecycle + gate + replay + SoD + version conflict
  + invalid transition + reopen window expiry against real rows, with the control
  event surviving the rolled-back command; immutability and guard triggers
  (boundary / key / calendar version / scope updates, illegal and unbumped state
  changes, DELETE, TRUNCATE on both tables, history UPDATE/DELETE/TRUNCATE,
  evidence CHECK, unique instance); tenant isolation under FORCE RLS on all four
  tables (bare query without tenant sees zero rows, cross-tenant WITH CHECK
  violation refused); **two concurrent hard-closes: exactly one wins and the loser
  gets `VERSION_CONFLICT`, while a concurrent gate reader only ever observes
  consistent (state, version) pairs, versions never go backwards, and the read
  after the winner returned shows HARD_CLOSED**; 4 racing materialisations create
  each period exactly once; outbox claim publishes once.

## Not verified (read this)

1. **ACC-14 does not exist.** The verification endpoint is built in the cutover
   step. Against a real financial-close-svc today every state command is refused
   (`SOURCE_UNVERIFIED`). The HTTP verifier was tested only against stubs written
   from the same contract, so a mismatch in field names or semantics with the
   eventual implementation would not have been caught.
2. **No end-to-end run** in docker compose: compose entry, `init-db.sh`, the
   Dockerfile build and the Kafka relay were not exercised; the service is not
   registered anywhere.
3. **fiscal-calendar-svc (REF-04)** was not run against; the preview / resolve
   contract is as specified by the parent and tested against stubs. The default
   REF-04 port 8173 in config is a guess from that service's own default.
4. **authorization-svc**: the actions `PERIOD_STATE_COMMAND` and
   `PERIOD_MATERIALIZE` are not seeded or checked against the real service.
   Until seeded, every command is denied.
5. **No load, performance, soak or failover test.** The gate and command SLOs in
   SLO.md are proposals; the gate has been run against one local Postgres only.
   No replica / pooler behaviour was tested (the design requires the gate to read
   the primary).
6. **Spec 9.2 gates not run:** generated-client / contract-compatibility checks,
   security / privacy / accessibility / data-governance gates, backup / restore
   and reconciliation rehearsals, alerts (none added), and "dependent services
   pass integration controls" (the GL does not call this service yet).
7. **`-race`** was not run.
8. **Soft-close exception is only asserted by the caller** (SPEC_DEVIATIONS 2):
   as it stands the gate's SOFT_CLOSED restriction can be bypassed by any caller
   that sets the flag.

## Spec requirements that are NOT met

See SPEC_DEVIATIONS.md (30 numbered items). Most relevant to a certification
decision: the ACC-14 endpoint is missing (A1); the soft-close exception is
unverified (A2); "posted periods" in `calendar-usage` is "not OPEN" (A3); no
protected-field fingerprint issued by ACC-14 (A4); control event is best effort
(A5); no book / entity validation (A7); no as-of query (A8); no mTLS on the new
upstream calls (A11).

## Contract surface

- `openapi.yaml`: 8 paths (materialise, the four colon commands, gate `:resolve`,
  compat `:status-by-key`, list, get, state-history, `calendar-usage`).
- `asyncapi.yaml`: `PeriodOpened`, `PeriodSoftClosed`, `PeriodHardClosed`,
  `PeriodReopened`, `PeriodReclosed` and the control event
  `PeriodCommandRejected` on `zoiko.accounting-period.events`, via the
  transactional outbox, at-least-once.
- Typed error body `{"code","message"}`; the gate's failure bodies also carry
  `posting_allowed:false`. Code set per spec 3 plus `NOT_FOUND`, `FORBIDDEN`,
  `PERIOD_NOT_FOUND`.

## Artifacts added

- `services/accounting-period-svc/` (this directory): code, 4 migrations
  (up + down), Dockerfile, OpenAPI, AsyncAPI, RUNBOOK.md, SLO.md,
  SPEC_DEVIATIONS.md, this certificate.
- Nothing outside this directory was changed: no compose entry, no `init-db.sh`
  registration, no change to general-ledger-svc, financial-close-svc or
  fiscal-calendar-svc. No git commits were made.

## Re-proving

```sh
cd services/accounting-period-svc
go build ./... && go vet ./... && go test ./... -count=1
# Postgres: scratch DB owned by a plain role
#   CREATE ROLE app_plain LOGIN PASSWORD 'plain' NOSUPERUSER NOBYPASSRLS;
#   ALTER DATABASE accounting_period_test OWNER TO app_plain;
#   DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION app_plain;
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='postgres://app_plain:plain@127.0.0.1:55434/accounting_period_test?sslmode=disable' \
  go test ./internal/store -count=1 -v
```
