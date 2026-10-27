# Release Certificate — fiscal-calendar-svc (REF-04)

**Date:** 2026-10-07
**Status: NOT CERTIFIED FOR PRODUCTION.** This document records what was built
and what was verified. It deliberately does **not** claim the spec's Definition
of Done (spec 9.2) is met: several of its gates were not run, and several
spec items are not implemented. Read "Not verified" before relying on anything
here.

## What this service is

The Fiscal Calendar (spec REF-04): entity-specific, effective-dated fiscal
calendar definitions (calendar months, or 52/53-week patterns such as 4-4-5,
with optional zero-length special periods), a controlled lifecycle
DRAFT -> APPROVED -> ACTIVE -> SUPERSEDED, calendar transition plans, a
deterministic period preview (the contract REF-05 consumes), as-of reads and a
resolve call. It owns FiscalCalendar, FiscalCalendarVersion and
CalendarTransitionPlan; it does **not** own period instances, period close
evidence, journal posting or tax filing calendars. It uses no floating point;
all dates are `DATE`. Port 8173, database `fiscal_calendar`, topic
`zoiko.fiscal-calendar.events`. Structure, conventions and the vendored
envelope / outbox / telemetry / mTLS / health packages mirror
currency-registry-svc. Calendar patterns, fiscal-year starts and the
book/basis `scope` are data; no country, jurisdiction or currency value is a
code constant, enum or switch.

## Verification performed

Environment: Windows, Go 1.27.1. The Postgres suite and the migrations were run
on 2026-10-07 against **PostgreSQL 16.15** (docker `postgres:16-alpine`, a
fresh container, database `fiscal_calendar_test`) as a **NOSUPERUSER
NOBYPASSRLS role that owns the database and schema** (so FORCE RLS applied to
it). **No Kafka, no authorization-svc, no accounting-period-svc and no
docker-compose run were available.**

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l .` | clean |
| `go mod tidy` (offline) | no change to go.mod / go.sum |
| `go test ./... -count=1`, no `TEST_DATABASE_URL` | pass: **105 passed, 0 failed, 7 skipped** (the 7 skipped are the Postgres suite) |
| `go test ./... -count=1` with `TEST_DATABASE_URL` (as above) and `REQUIRE_DB_TESTS=1` | pass: **134 passed, 0 failed, 0 skipped** (counts include subtests; 83 top-level tests, of which 7 are the Postgres suite) |
| Postgres suite repeated 3 more times | pass each time (the concurrent-activation test did not flake in 4 runs total) |
| Down migrations 4,3,2,1 | apply cleanly; 0 tables and 0 calendar functions remain; re-applying up 1..4 afterwards succeeds; a second down is a clean no-op |
| `-race` | **not run** (cgo is unavailable on this machine) |
| Mutation spot-check | 5 deliberate breakages of `internal/service/commands.go` (drop proposer SoD, drop overlap check, drop the plan requirement, drop fail-closed, skip the REF-05 history check) were each caught by the handler tests (1, 3, 4, 1, 8 failing tests); the file was then restored |

### What the passing tests cover

Handler tests (`internal/handler`) drive the real router and handlers over the
real business rules in `internal/service`, against the in-memory
transactional store `internal/memstore`:

- Create: DRAFT calendar + DRAFT version, `FiscalCalendarCreated` with tenant,
  object id/version, effective_at, recorded_at, actor, correlation id; 12
  validation refusals (bad/unknown pattern, weeks not 52, bad month/day, missing
  or inverted dates, bad code/scope, entity mismatch, missing reason, unknown
  field, malformed date) store nothing; duplicate code -> `DUPLICATE_CANDIDATE`;
  missing tenant / actor / entity / Idempotency-Key.
- Lifecycle: DRAFT -> APPROVED -> ACTIVE with full status history; every illegal
  move -> `INVALID_TRANSITION`; proposer cannot approve -> `SOD_DENIED` with
  nothing changed; stale or missing `expected_version` -> `VERSION_CONFLICT` /
  `CONTEXT_INVALID` (body and `X-Expected-Version` forms).
- Idempotency: replay returns the original with exactly one event; same key with
  a different body is refused; a replay of an activation succeeds even when
  REF-05 is down (it re-checks nothing).
- As-of resolution across two versions (boundary days, open-ended, before any
  version -> 404), resolve by (entity, scope, date) including no-match 404s,
  draft/approved versions not resolving, supersession end-dating
  (`effective_to`, `superseded_by_version_id`), event sequence
  (Created, Activated, ChangeProposed, Superseded, Activated) with
  `previous_version_id` / `superseded_by_version_id`.
- Overlap: a second calendar for the same entity+scope overlapping an in-force
  version is refused; a new version starting before or on the predecessor's start
  is refused; a bounded calendar ending before the first begins, another scope
  and another entity are allowed.
- Immutability: no PATCH/PUT/DELETE surface on calendars or versions; protected
  fields smuggled into a lifecycle command are rejected; a correction is a new
  version and the approved one keeps its definition.
- Transition plans: change over posted history without an approved plan ->
  `TRANSITION_PLAN_REQUIRED`; a plan that does not declare posted-period impact,
  or is only PROPOSED, or was REJECTED, does not satisfy it; an approved plan
  does and is named in the event; plan SoD (plan proposer, version proposer);
  reject then re-plan; plan validation (from must have been in force, stale
  to-version, same from/to, empty impact, posted impact without mapping, unknown
  version); a change effective the day after the last posted period, or with
  nothing posted, needs no plan; posted-but-unknown-end counts as touching
  history.
- Fail closed: REF-05 error, nil answer, or no client configured -> 503
  `DEPENDENCY_UNAVAILABLE` for any non-first version with nothing changed and no
  internal text leaked; the first version never calls REF-05; recovers when the
  dependency returns.
- Events: a failed outbox write rolls back the whole command (no calendar, no
  idempotency record; the retry succeeds), including a half-done supersession.
- Tenant isolation (memstore): another tenant gets 404 on every read and
  command, can reuse code/scope/idempotency key independently, and does not see
  the other tenant's objects (non-collision of two tenants' in-force versions is covered by the Postgres suite only).
- Authorization: the right action per command (PROPOSE for create / propose /
  plan, APPROVE for approve / plan decisions, ACTIVATE for activate); denial ->
  403 `FORBIDDEN`; authorization-svc down -> 503, nothing changed.
- periods-preview contract: exact top-level and per-period key sets, DRAFT
  refused, byte-identical on repeat and across later status changes, bad or out
  of range `fiscal_year`, unknown version, a 53-week year with a special period.

Domain tests: pattern validation table (24 cases); `PreviewPeriods` golden JSON
for a 12-month year (April start, 2026), a 4-4-5 52-week year (FY2026), the
53-week year FY2029 under both `ADD_TO_LAST` and `ADD_TO_FIRST`, a leap-year
February, `END_YEAR` labelling, and special periods; a contiguity/abutment
property test (6 patterns x 6 start anchors x 101 fiscal years); byte-identical
determinism over 50 calls; full (from, to) version state-machine matrix; plan
content validation; interval maths; date JSON. Other: events envelope tests,
REF-05 client tests (against `httptest`), envelope-policy, outbox and health
tests (copied from the template), doctrine guards (no probed
country/currency/standard value in Go or SQL, no float types, no soft delete,
every table has FORCE RLS), OpenAPI/AsyncAPI parse + name agreement with the
code and the outbox CHECK constraint, and the exact `periods-preview` schema
keys.

Postgres suite (`internal/store`, 7 tests, all passing as above): full lifecycle
with supersession, idempotent replay, SoD, outbox rows and history rows counted
inside a tenant transaction; the exclusion constraint refusing an overlapping
activation even when the service check is bypassed, and a different scope not
conflicting; **4 concurrent overlapping activations -> exactly one wins**; ~25
direct-SQL attempts to break the immutability triggers and CHECKs (pattern,
effective dates, start anchor, proposer, approver, status backwards/skipping,
non-increasing version, deletes of calendars/versions/plans, history
update/delete, self-approval of a version and of a plan, start day 31, inverted
window, scope forged against the calendar, duplicate version_no, `effective_to`
on a non-superseding update, a decided plan frozen), with four of them additionally asserted
to fail for the intended reason; transition-plan flow with the REF-05 stub and
the one-live-plan unique index; **RLS** (tenant-b sees 0 rows in all 6 tables,
a bare no-tenant query sees 0, tenant-a sees its own, a cross-tenant INSERT is
refused by WITH CHECK, same keys independent per tenant); outbox claim publishes
once.

## Not verified (read this)

1. **No integration with accounting-period-svc (REF-05).** The
   `GET /v1/calendar-usage` contract is the brief's; the client is tested only
   against a stub. REF-05 has not been run against `periods-preview` either.
   Until REF-05 serves `calendar-usage`, non-first activations fail closed.
2. **authorization-svc integration is unverified**: the actions
   `FISCAL_CALENDAR_PROPOSE`, `FISCAL_CALENDAR_APPROVE` and
   `FISCAL_CALENDAR_ACTIVATE` have not been seeded or checked against the real
   service. Until seeded, every command is denied.
3. **No end-to-end run** in docker compose: the compose entry, `init-db.sh`
   registration (database `fiscal_calendar`) and the Dockerfile build were not
   created or exercised (they belong to the parent). Nothing outside
   `services/fiscal-calendar-svc` was touched.
4. **No Kafka run**: the relay's publisher path was only unit-tested with the
   template's stub writer; real delivery, topic creation and ordering are
   unverified.
5. **btree_gist** worked for a non-superuser database owner on
   postgres:16-alpine. It has not been tried on the platform's real Postgres
   image, role layout or managed offering.
6. **No load, performance or soak test.** The SLOs in SLO.md are proposals.
   Postgres concurrency was exercised only by the one 4-goroutine activation
   test.
7. **Generated-client / contract-compatibility gates** (spec 9.2) were not run;
   OpenAPI/AsyncAPI are hand-written and only checked for parse + name
   agreement.
8. **Security/privacy/accessibility/data-governance gates, backup/restore and
   reconciliation procedures, alerts**: not done. No Prometheus alert rules were
   added. The RUNBOOK has never been exercised against a live stack beyond the
   Postgres container used for the tests.
9. **Downstream consumers**: no ACC/REP/TAX service was integrated or tested
   against `periods-preview` or the events; "dependent services pass
   integration controls" (spec 9.2) is not met.
10. **Cross-tenant / replay / SoD tests** (spec 9.2) ran against both the
    in-memory store and Postgres as described; stale-cache, duplicate-import and
    source-verification tests are not applicable or not implemented (see
    SPEC_DEVIATIONS).

## Spec requirements that are NOT met

See SPEC_DEVIATIONS.md for the full, numbered list. The ones that most affect a
certification decision: the posted-history check is only as good as REF-05's
answer, is point-in-time, and does not cover a different calendar taking over
an entity/scope (items 1-3); ORG-03/REF-06 are not consulted (5); "controller"
approval is an authorization action, not a verified role (6); no cancel/withdraw
of versions (8); valid-time only, no recorded-time reconstruction (10).

## Contract surface

- `openapi.yaml`: 13 paths (create, resolve, get/as_of, list versions,
  propose-change, get version, approve, activate, periods-preview, create plan,
  get plan, approve plan, reject plan). Named commands use the colon form.
- `asyncapi.yaml`: `FiscalCalendarCreated`, `FiscalCalendarChangeProposed`,
  `FiscalCalendarVersionActivated`, `FiscalCalendarSuperseded` on
  `zoiko.fiscal-calendar.events`, via the transactional outbox, at-least-once.
- Typed error body `{"code","message"}`; code set per spec 3 plus `NOT_FOUND`,
  `FORBIDDEN` and `TRANSITION_PLAN_REQUIRED`.

## Artifacts added

- `services/fiscal-calendar-svc/` (this directory): code, 4 migrations
  (up + down), Dockerfile, OpenAPI, AsyncAPI, RUNBOOK.md, SLO.md,
  SPEC_DEVIATIONS.md, this certificate.
- Not added (for the parent): `deployments/docker-compose.yml` service block
  and migrations mount, `deployments/init-db.sh` database `fiscal_calendar`
  and grant, authorization-svc action seeding, Prometheus rules.
- No other service's code was modified. No git command that changes state was
  run. The test container `fiscal-cal-pgtest` was removed after the runs.

## Re-proving

```sh
cd services/fiscal-calendar-svc
go build ./... && go vet ./... && go test ./... -count=1
# Postgres suite: a scratch database owned by a NOSUPERUSER NOBYPASSRLS role that owns its schema
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='postgres://app_plain:plain@127.0.0.1:55433/fiscal_calendar_test?sslmode=disable' \
  go test ./internal/store -count=1 -v
```
