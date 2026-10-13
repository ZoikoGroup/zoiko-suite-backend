# Release Certificate — financial-statements-svc (REP-01)

**Date:** 2026-10-13
**Status: NOT CERTIFIED FOR PRODUCTION.** This records what was built and
verified. It does not claim the spec's Definition of Done is met: the
service has never been run against a real financial-close-svc, authorization-
svc or Kafka broker, is not wired into docker-compose, and no snapshot
producer exists anywhere in the platform to feed it real data. Read "Not
verified" before relying on anything here.

## What this service is

The Financial Statements service (REP-01, ZS-SVC-H-001 §4.1): produces a
Balance Sheet and Income Statement from a CERTIFIED ACC-16 Signed Financial
Snapshot, through a Draft → Validated → InReview → Approved → Certified →
Published lifecycle, with segregation-of-duties (a preparer may not approve
or certify their own work) and a supersession path for corrections. See
SPEC_DEVIATIONS.md for the full, honest list of what the spec asks for that
this version does not build (Cash Flow Statement, disclosures, comparative
periods, a real presentation-mapping registry, full ACC-18 lineage-graph
integration).

## Verification performed

Environment: Windows, Go 1.26. Unit and integration tests run without
external services except a locally-embedded Postgres.

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `gofmt -l .` | clean |
| `go test ./...` (no database) | pass — domain (12 tests), service (9 tests) |
| `go test -tags=integration ./internal/store/...` (embedded Postgres 16) | pass — 7 tests |

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
   snapshot it actually produced, has never been run.
2. **No end-to-end run against real authorization-svc, Kafka, or
   financial-close-svc.** The HTTP clients (`internal/clients`,
   `internal/authz`) have no test coverage of their own in this pass — they
   are exercised indirectly through the service-layer fakes, which assert
   the *contract* each client must satisfy, not the real HTTP client's wire
   behavior against a live or stubbed server.
3. **Not registered in docker-compose, `init-db.sh`, or anywhere outside this
   service's own directory.** No port reservation beyond this document's own
   claim of 8175 (verified free against the compose file and
   `services/README.md` at the time of writing, not reserved anywhere).
4. **No handler-layer (HTTP) tests.** Routing, request decoding and the
   `domain.Code` → HTTP status mapping in `internal/handler` are untested
   directly; they are simple enough (thin wrappers over the service layer)
   that this is a real gap, not a negligible one, for anyone auditing the
   HTTP contract specifically.
5. **No load, performance, soak or failover testing.** `SLO.md` states
   proposed targets; none were measured.
6. **`-race` was not run.**
7. **authorization-svc action strings are invented, not seeded anywhere**:
   `FINANCIAL_STATEMENT_SET_CREATE`, `_VIEW`, `_REFRESH`, `_VALIDATE`,
   `_REVIEW`, `_APPROVE`, `_CERTIFY`, `_PUBLISH`. Until seeded in a real
   authorization-svc deployment, every command is denied in production.
8. **No mTLS pilot wiring exercised** — `AuthzMTLSEnabled` exists and is
   wired (off by default) but never tested with a real mtls-management-svc.

## Spec requirements that are NOT met

See SPEC_DEVIATIONS.md (B-section additions and A-section gaps). Most
relevant to a certification decision: no Cash Flow Statement (A2); no
disclosures (A5); no comparative periods (A6); no real presentation-mapping
registry (A3); no real ACC-18 lineage-graph walk (A8); ACC-16's content
schema is this service's own invention, not a platform agreement (A1).

## Contract surface

- `POST /v1/statement-sets` — CreateStatementSet
- `GET /v1/statement-sets` — ListStatementSets (`legal_entity_id` required, `status` optional)
- `GET /v1/statement-sets/{id}` — GetStatementSet
- `GET /v1/statement-sets/{id}/facts` — GetStatementFacts
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
- Nothing outside this directory was changed except `services/_contract/rollout.sh`.
  No compose entry, no `init-db.sh` registration, no change to
  financial-close-svc or any other service. No git commits were made.

## Re-proving

```sh
cd services/financial-statements-svc
go build ./... && go vet ./... && go test ./... -count=1
go test -tags=integration -count=1 -timeout=300s ./internal/store/...
```
