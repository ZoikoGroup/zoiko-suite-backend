# Release Certificate — currency-registry-svc (REF-02)

**Date:** 2026-10-07
**Status: NOT CERTIFIED FOR PRODUCTION.** This document records what was built
and what was verified. It deliberately does **not** claim the spec's Definition
of Done (spec 9.2) is met: several of its gates were not run, and several
spec items are not implemented. Read "Not verified" before relying on anything
here.

## What this service is

The Currency Registry (spec REF-02): global ISO 4217 reference data, versioned
append-only minor-unit metadata, and the platform support lifecycle
(KNOWN -> SUPPORTED/RESTRICTED -> RETIRED), plus a per-tenant enablement
overlay. It owns Currency, CurrencyLifecycle, MinorUnitMetadata and
PlatformSupportStatus; it does not own FX rates, functional-currency
assignment or amounts, performs no arithmetic and uses no floating point.
Port 8172, database `currency_registry`, topic
`zoiko.currency-registry.events`. Structure, conventions and the vendored
envelope / outbox / telemetry / mTLS / health packages mirror
delegated-authority-svc.

## Verification performed

Environment: Windows, Go 1.27.1. Unit and handler tests ran without external services.
The Postgres suite and the migrations were additionally run on 2026-10-07 against
PostgreSQL 16 (docker postgres:16-alpine): all 4 store tests pass (the RLS test as a
NOSUPERUSER NOBYPASSRLS schema-owner role), and all 4 down migrations apply cleanly.
**No Kafka, no authorization-svc and no docker-compose run were available.**

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l .` | clean |
| `go test ./... -count=1` | pass: **109 passed, 0 failed, 4 skipped** (the 4 skipped are the Postgres suite; see below). Of the 109, 37 are top-level handler tests (with subtests counted in the total), the rest are domain, events, envelope-policy, doctrine/contract guards, and the outbox/health tests copied from the template. |
| `-race` | **not run** (cgo is unavailable on this machine) |

What the passing tests actually cover (all against the in-memory transactional
store `internal/memstore`, which runs the real business rules in
`internal/service` through the real HTTP handlers and router):

- State machine: every (from, to) pair of the status vocabulary, illegal ones
  -> `INVALID_TRANSITION`; RETIRED terminal; unknown status.
- Import: valid import applies (currencies enter KNOWN, UUIDv7 ids, versions,
  minor-unit rows, one `CurrencyUpdated` each); manifest hash mismatch ->
  `SOURCE_UNVERIFIED` with nothing stored; 11 invalid-row cases (code
  lengths/charset, minor unit out of range / negative / fractional / omitted,
  empty name, duplicate alpha/numeric in file, numeric conflict with registered
  code, numeric held by another currency) each quarantine the **whole** import
  (the valid row beside it is not applied) and enqueue `CurrencyImportQuarantined`;
  replay by Idempotency-Key and by (source, version, hash) returns the original
  with no new currencies/versions/events; same (source, version) with different
  content -> `DUPLICATE_CANDIDATE`; same key with different body ->
  `CONTEXT_INVALID`; missing Idempotency-Key / reason -> 400; authz deny -> 403,
  authz down -> 503 with nothing written; backdated minor-unit change
  quarantined; unchanged row creates nothing.
- Minor-unit change inserts a second version, leaves the first row untouched,
  bumps the currency version; `as_of` before the change returns the old exponent,
  after returns the new one; version history shows the derived `valid_to`.
- Activate/restrict/retire: Idempotency-Key and `expected_version` required;
  stale version -> `VERSION_CONFLICT`; importer cannot activate ->
  `SOD_DENIED` (and SoD follows the *latest* importer); authz denial -> 403;
  replay returns the original with exactly one event/history row; unknown id /
  command -> 404.
- `validate`: unknown code -> `supported:false`, `minor_unit: null`; full
  KNOWN/SUPPORTED/RESTRICTED/RETIRED matrix for `post`; `read` stays true for a
  retired currency; bad requests. Retired currency stays readable via GET.
- Tenant overlay: cannot enable KNOWN/RESTRICTED/unknown/retired currency;
  enable/disable with overlay versioning; tenant A's overlay invisible to B;
  cross-tenant path access -> 403; `tenant_scoped` validate per tenant.
- Events: one per command with tenant, object id, object version, effective_at,
  recorded_at, actor, correlation; event write failure rolls back the state
  change (atomicity, in the in-memory store).
- Guards: no hardcoded currency/numeric codes from a probe list and no float
  types in non-test code; OpenAPI/AsyncAPI parse and agree with the code and the
  outbox CHECK constraint on event names; envelope policy requires no legal
  entity but requires Idempotency-Key on writes.

## Not verified (read this)

1. **Postgres: not run.** The migrations (tables, CHECKs, partial unique
   indexes, append-only / no-delete / immutable-code triggers, FORCE RLS
   policies) and `internal/store/pg_store.go` (SQL, advisory locks,
   `FOR UPDATE`, outbox claim) were **executed on PostgreSQL 16 on 2026-10-07**: the 4 store tests pass and the down migrations apply cleanly. This covers the paths those tests exercise (import, idempotent replay, SoD, outbox write, append-only triggers, tenant overlay RLS, outbox claim); it is not a full proof, and no load or concurrency test was run. The suite skips without `TEST_DATABASE_URL`.
2. **RLS** was verified for the tenant overlay under a non-superuser schema-owner role (FORCE RLS applies to the owner); the RLS test self-skips on a superuser. The "no RLS on global tables" decision is by design and not otherwise tested.
3. **No end-to-end run** in docker compose: the compose entry, `init-db.sh`
   registration and Dockerfile build were not exercised.
4. **No load, performance or soak test.** The SLOs in SLO.md are proposals.
5. **No Kafka run**: the relay's publisher path was only unit-tested with the
   template's stub writer; real delivery, topic creation and ordering are
   unverified.
6. **authorization-svc integration is unverified**: the actions
   `CURRENCY_IMPORT`, `CURRENCY_ACTIVATE`, `CURRENCY_RESTRICT`,
   `CURRENCY_RETIRE`, `CURRENCY_TENANT_ENABLE`, `CURRENCY_TENANT_DISABLE` have
   not been seeded or checked against the real service. Until seeded, every
   command is denied.
7. **Generated-client / contract-compatibility gates** (spec 9.2) were not run;
   OpenAPI/AsyncAPI are hand-written and only checked for parse + name
   agreement.
8. **Security/privacy/accessibility/data-governance gates, backup/restore and
   reconciliation procedures, alerts**: not done. No Prometheus alert rules were
   added. The RUNBOOK has never been exercised.
9. **Downstream consumers**: no ACC/AR/AP/BNK/TAX service was integrated or
   tested against `:validate`; "dependent services pass integration controls"
   (spec 9.2) is not met.
10. **Cross-tenant / stale-cache / duplicate-import / replay / SoD /
    source-verification tests** (spec 9.2): covered at handler level on the
    in-memory store only (see above); not against Postgres.

## Spec requirements that are NOT met

See SPEC_DEVIATIONS.md for the full, numbered list. The ones that most affect a
certification decision: import is synchronous (not async); no ISO source
authenticity or licensing verification; `SOURCE_UNVERIFIED` leaves no stored
evidence; no protected-field fingerprint or verified approval chain;
no backdated corrections or recorded-time queries; `as_of` status is current
status; tenant overlay has no history table.

## Contract surface

- `openapi.yaml`: 9 paths (imports, lifecycle commands, queries, validate,
  tenant overlay). Named commands use the colon form
  (`/v1/currencies/{id}:activate`).
- `asyncapi.yaml`: `CurrencyUpdated`, `CurrencySupportChanged`,
  `CurrencyRetired`, `CurrencyImportQuarantined` on
  `zoiko.currency-registry.events`, via the transactional outbox,
  at-least-once.
- Typed error body `{"code","message"}`; code set per spec 3 plus `NOT_FOUND`,
  `FORBIDDEN`.

## Artifacts added

- `services/currency-registry-svc/` (this directory): code, 4 migrations
  (up + down), Dockerfile, OpenAPI, AsyncAPI, RUNBOOK.md, SLO.md,
  SPEC_DEVIATIONS.md, this certificate.
- `deployments/docker-compose.yml`: migrations mount + `currency-registry-svc`
  service block (port 8172, DB `zoiko_app`).
- `deployments/init-db.sh`: database `currency_registry` created and migrations
  applied; added to the `zoiko_app` grant list.
- No other service's code was modified. No git commits were made.

## Re-proving

```sh
cd services/currency-registry-svc
go build ./... && go vet ./... && go test ./... -count=1
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL=<scratch db, non-superuser> go test ./internal/store -count=1 -v
```
