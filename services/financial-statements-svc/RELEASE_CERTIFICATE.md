# Release Certificate — financial-statements-svc (REP-01)

**Date:** 2026-10-13 (updated same day after closing docker-compose/init-db.sh
wiring, a demo authz seed, and handler-layer tests — see the changelog note
at the bottom of this section)
**Status: NOT CERTIFIED FOR PRODUCTION.** This records what was built and
verified. It does not claim the spec's Definition of Done is met: the
service has never been run against a real financial-close-svc, authorization-
svc or Kafka broker (only wired to be able to), and no snapshot producer
exists anywhere in the platform to feed it real data. Read "Not verified"
before relying on anything here.

**What changed since the first pass** (same day): registered in
`deployments/docker-compose.yml` (build, env, healthcheck, depends_on) and
`deployments/init-db.sh` (database creation, migrations, least-privilege
grant); added a REF-01 maker/checker section to
`deployments/scripts/seed-demo-rbac.ps1`; added 13 handler-layer HTTP tests.
None of this required Docker to actually run (`docker compose config` was
used to validate syntax only) — see item 2 below, still open.

## What this service is

The Financial Statements service (REP-01, ZS-SVC-H-001 §4.1): produces a
Balance Sheet and Income Statement from a CERTIFIED ACC-16 Signed Financial
Snapshot, through a Draft → Validated → InReview → Approved → Certified →
Published lifecycle, with segregation-of-duties (a preparer may not approve
or certify their own work) and a supersession path for corrections. See
SPEC_DEVIATIONS.md for the full, honest list of what the spec asks for that
this version does not build (Cash Flow Statement, disclosures, N-period
comparatives, a real presentation-mapping registry, full ACC-18 lineage-graph
integration).

## Verification performed

Environment: Windows, Go 1.26. Unit and integration tests run without
external services except a locally-embedded Postgres.

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l .` | clean |
| `go test ./...` (no database) | pass — domain (12 tests), service (9 tests), handler (13 tests) |
| `go test -tags=integration ./internal/store/...` (embedded Postgres 16) | pass — 7 tests |
| `go test -race ./...` | **not run** — cgo/gcc on this machine cannot build in 64-bit mode (`cc1.exe: sorry, unimplemented: 64-bit mode not compiled in`); a pre-existing environment limitation, not specific to this service |
| `docker compose config --quiet` over the edited `deployments/docker-compose.yml` | pass — confirms the new service block and migration volume mount are syntactically valid; does **not** confirm the container actually builds or runs |

What the tests cover:

- **Domain** (`internal/domain`): every legal and illegal state transition;
  `CanSupersede`; `SnapshotContent` validation (missing/duplicate/invalid
  fields); `BuildFacts` category→statement mapping; `ValidateTieOut` (balanced,
  imbalanced, and all-zero-category edge cases); exact-decimal parsing and
  canonicalization.
- **Service** (`internal/service`, against fakes — no DB, no HTTP): a
  snapshot is verified CERTIFIED and legal-entity-matched before
  `CreateStatementSet` succeeds; a mismatched legal entity is refused; the
  full happy-path lifecycle end to end; the preparer is refused both
  `Approve` and `Certify` on their own work; `ValidateStatements` refuses a
  set with no facts yet; `RequestChanges` returns a set to DRAFT;
  `SupersedeStatementSet` is refused outside CERTIFIED/PUBLISHED;
  authorization denial propagates as `CodeForbidden`.
- **Store** (`internal/store`, real Postgres via embedded-postgres): outbox
  event enqueued transactionally with `CreateSet`; the partial unique index
  refuses a second active set for the same (tenant, entity, period,
  framework); a full lifecycle enqueues exactly 7 outbox events across 6
  transitions plus the initial create; a stale `expectedVersion` is refused
  as `VERSION_CONFLICT`; RLS actually hides another tenant's row (not just
  "the column exists"); the store layer independently refuses a preparer's
  own approval even if the service layer's check were bypassed; `Supersede`
  correctly marks the old set, creates the new one, and the one-active-set
  constraint re-applies to the replacement.
- A schema-drift guard test (`TestEventingOutboxMigrationMatchesLibrary`)
  fails if migration 000002 ever drifts from `services/eventing/outbox`'s
  canonical schema.
- **Handler** (`internal/handler`, real HTTP via `httptest` + `chi`, against
  the same kind of fakes as the service-layer tests): missing-principal
  refuses with 401; a verified-CERTIFIED snapshot creates with 201; an
  unverified/missing snapshot answers 422 with `error_code:
  SOURCE_UNVERIFIED`; authorization denial answers 403; an unknown resource
  answers 404; an unknown colon-command answers 404; refresh-draft then
  validate round-trips through real JSON encode/decode; `:certify` requires
  a `reason` in the body (400 otherwise); the preparer approving their own
  work answers 403 with `error_code: SOD_DENIED`; an illegal transition
  answers 409; `:supersede` returns 201 with the new set naming what it
  supersedes; `ListStatementSets` requires `legal_entity_id` (422 otherwise)
  and genuinely filters by it.

A real bug was found and fixed during this verification: `Supersede`'s
original write order violated the `superseded_by_set_id` foreign key (it
tried to point the old row at a new row that didn't exist yet) while
simultaneously needing the old row to stop being "active" before the new row
could be inserted, to satisfy the one-active-set unique index. Fixed with a
three-statement order (mark old superseded with no forward pointer → insert
new → backfill old's pointer) — see `pg_store.go`'s `Supersede` for the
reasoning, and the still-passing `TestPgStore_Supersede_MarksOldAndCreatesNew`.

## Not verified (read this)

1. **No real snapshot producer exists anywhere in the platform.** ACC-16's
   `Content` field has no platform-agreed schema (SPEC_DEVIATIONS A1); every
   test here uses hand-crafted JSON matching `internal/domain.SnapshotContent`.
   A real integration test against a running financial-close-svc, with a
   snapshot it actually produced, has never been run. **Still open** — this
   is a cross-service architecture decision (who owns producing a real
   trial-balance snapshot in this schema), not something fixable inside
   this service alone.
2. **No end-to-end run against real authorization-svc, Kafka, or
   financial-close-svc.** The HTTP clients (`internal/clients`,
   `internal/authz`) have no test coverage of their own in this pass — they
   are exercised indirectly through the service-layer fakes, which assert
   the *contract* each client must satisfy, not the real HTTP client's wire
   behavior against a live or stubbed server. **Still open** — no Docker
   daemon is available in this environment to actually run the stack;
   `docker compose config` only validated YAML syntax, not a real boot.
3. ~~Not registered in docker-compose, `init-db.sh`...~~ **Resolved.** Added a
   `financial-statements-svc` block to `docker-compose.yml` (build, env,
   healthcheck, `depends_on` financial-close-svc/authorization-svc/kafka), a
   migration volume mount, and an `init-db.sh` entry (database creation +
   migrations + least-privilege `zoiko_app` grant, following
   accounting-period-svc's own pattern exactly). Port 8175 confirmed free at
   the time of writing. **Not yet run** — see item 2.
4. ~~No handler-layer (HTTP) tests.~~ **Resolved.** 13 tests added in
   `internal/handler/handler_test.go`, real HTTP via `httptest.NewRequest` +
   the real `chi` router, covering auth/authz failures, the full command
   dispatch including the colon-suffixed routes, JSON status-code mapping,
   and list filtering.
5. **No load, performance, soak or failover testing.** `SLO.md` states
   proposed targets; none were measured. **Still open** — needs a running
   stack.
6. **`-race` was not run.** **Still open** — attempted; this machine's cgo
   toolchain cannot build in 64-bit mode (`cc1.exe: sorry, unimplemented:
   64-bit mode not compiled in`). Environment limitation, not specific to
   this service (same gap accounting-period-svc's own certificate recorded).
7. ~~authorization-svc action strings are invented, not seeded anywhere~~
   **Partially resolved.** Added a REF-01 maker/checker section to
   `deployments/scripts/seed-demo-rbac.ps1` (roles `REF01_STATEMENT_MAKER`
   / `REF01_STATEMENT_CHECKER`, granting the 8
   `FINANCIAL_STATEMENT_SET_*` actions), parsed and syntax-checked but
   **never run** against a live authorization-svc (needs item 2). This
   seeds the demo/dev environment only — it is not a production provisioning
   mechanism (authorization-svc's roles and permission bundles are
   per-tenant data; there is still no platform-wide convention for rolling a
   new service's actions out to real tenants, the same gap
   accounting-period-svc's own certificate recorded and left open).
8. **No mTLS pilot wiring exercised** — `AuthzMTLSEnabled` exists and is
   wired (off by default) but never tested with a real mtls-management-svc.
   **Still open.**

## Spec requirements that are NOT met

See SPEC_DEVIATIONS.md (B-section additions and A-section gaps). Most
relevant to a certification decision: no Cash Flow Statement (A2); no
disclosures (A5); comparatives are two-period only (A6/A7); no real presentation-mapping
registry (A3); no real ACC-18 lineage-graph walk (A8); ACC-16's content
schema is this service's own invention, not a platform agreement (A1).

## Contract surface

- `POST /v1/statement-sets` — CreateStatementSet
- `GET /v1/statement-sets` — ListStatementSets (`legal_entity_id` required, `status` optional)
- `GET /v1/statement-sets/{id}` — GetStatementSet
- `GET /v1/statement-sets/{id}/facts` — GetStatementFacts
- `GET /v1/statement-sets/{id}/comparatives?prior_set_id=` — GetComparatives (two-period; prior must be CERTIFIED/PUBLISHED)
- `POST /v1/statement-sets/{id}:refresh-draft` / `:validate` / `:submit-for-review` /
  `:request-changes` / `:approve` / `:certify` / `:publish` / `:supersede`
- `GET /v1/statement-facts/{id}/explain` — ExplainReportedFact

No formal `openapi.yaml` / `asyncapi.yaml` was written in this pass — the
contract above and the domain event names in `internal/store/pg_store.go`'s
`enqueueEvent` call sites (`FinancialStatementSetCreated`,
`FinancialStatementDraftRefreshed`, `FinancialStatementsValidated`,
`FinancialStatementsSubmittedForReview`, `FinancialStatementsChangesRequested`,
`FinancialStatementsApproved`, `FinancialStatementsCertified`,
`FinancialStatementsPublished`, `FinancialStatementsSuperseded`) are the
authoritative surface until those are written.

## Artifacts added

- `services/financial-statements-svc/` (this directory): code, 2 migrations
  (up + down), Dockerfile + Dockerfile.dockerignore (not built or run — no
  Docker daemon available in this environment), RUNBOOK.md, SLO.md,
  SPEC_DEVIATIONS.md, this certificate.
- Vendored `internal/envelope/` via `services/_contract/rollout.sh
  financial-statements-svc` (added to that script's `ENTITY_SCOPED` and
  `ACCOUNTING` lists).
- `deployments/docker-compose.yml`: new `financial-statements-svc` service
  block, plus a migration volume mount on the `postgres` service.
- `deployments/init-db.sh`: `financial_statements:financial-statements` added
  to the service list (database + migrations) and to the `zoiko_app`
  least-privilege grant list.
- `deployments/scripts/seed-demo-rbac.ps1`: REF-01 maker/checker role
  section (`$REF01_ROLES`, `$REF01_SOD_PAIRS`), mirroring the existing
  REF-04/05 section.
- No change to financial-close-svc or any other service's own code.

## Re-proving

```sh
cd services/financial-statements-svc
go build ./... && go vet ./... && go test ./... -count=1
go test -tags=integration -count=1 -timeout=300s ./internal/store/...
```
