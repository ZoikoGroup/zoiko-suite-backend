# Runbook — financial-statements-svc

## What this service is

REP-01 (Financial Statements, ZS-SVC-H-001 §4.1). Turns a CERTIFIED ACC-16
Signed Financial Snapshot (financial-close-svc) into a Balance Sheet and
Income Statement, through a Draft → Validated → InReview → Approved →
Certified → Published lifecycle with segregation-of-duties enforcement.
Port 8175, database `financial_statements`, topic
`zoiko.financial-statements.events`.

## Dependencies

| Service | What for | Failure mode |
|---|---|---|
| Postgres | All state | Fails closed: `/readyz` fails, no writes possible. |
| financial-close-svc | `GET /v1/financial-snapshots/{id}` — the only source of truth for a statement set | `CreateStatementSet` / `:refresh-draft` return 503 `DEPENDENCY_UNAVAILABLE` if unreachable; 422 `SOURCE_UNVERIFIED` if the snapshot isn't CERTIFIED. |
| authorization-svc | Every command and read | Fails closed: 503 if unreachable, 403 if denied. |
| Kafka | Event delivery | Never blocks a request — events wait in `eventing_outbox` until the relay can deliver them. |

## Common operations

**Check the outbox backlog**: `outbox_backlog_events{service="financial-statements-svc"}` and
`outbox_oldest_backlog_age_seconds{service="financial-statements-svc"}` on `/metrics`. A growing
age means the relay stopped or Kafka is unreachable — check `outboxWriter`
connectivity, not the database.

**A statement set is stuck in DRAFT with validation errors**: `GET /v1/statement-sets/{id}` shows
`validation_errors`. The usual cause is the snapshot's accounts don't balance
(Assets ≠ Liabilities + Equity) — this is a real data problem upstream, not a
bug here; fix the source and `:refresh-draft` again.

**A command returns 409 VERSION_CONFLICT**: someone else changed the set
concurrently. Re-`GET` the set for its current version and retry with that
version.

**A command returns 403 SOD_DENIED**: the preparer (whoever last created or
refreshed the set) tried to approve or certify their own work. This is
correct behavior, not a bug — a different principal must perform that step.

**Correcting a CERTIFIED or PUBLISHED set**: never edit it — call
`POST /v1/statement-sets/{id}:supersede` with a new (also CERTIFIED)
snapshot. The old set is marked SUPERSEDED and kept forever, unchanged; a new
set starts its own DRAFT → ... → CERTIFIED cycle.

## Known gaps (see SPEC_DEVIATIONS.md for the full list)

- No Cash Flow Statement — only Balance Sheet and Income Statement.
- No snapshot producer exists anywhere in the platform yet — every snapshot
  used with this service today must be hand-crafted against
  `internal/domain.SnapshotContent`'s schema.
- No disclosures, no real ACC-18 lineage graph walk. Comparatives are
  two-period only, against an explicitly named CERTIFIED/PUBLISHED prior set
  (`GetComparatives` 422 `SOURCE_UNVERIFIED` means the prior isn't certified;
  `CONTEXT_INVALID` means entity/framework/currency/period don't line up).
