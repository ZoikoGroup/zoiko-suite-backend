# period-backfill

One-off cutover tool for REF-05. It brings REF-04 (fiscal-calendar-svc) and
REF-05 (accounting-period-svc) in line with the fiscal periods that already
exist in financial-close-svc.

- Talks to the three services **only over their HTTP APIs**. It never touches
  another service's tables.
- **Dry run by default.** Without `--apply` it only reads and prints the plan.
- **Never forces.** Anything it cannot map cleanly is reported (unmapped,
  unmatched, overlap, gap, incompatible calendar, boundary drift) and left alone.
- Principals are IDs only. There is no credential in this tool, and none is logged
  or written to the report.

## What it does, per legal entity

1. Reads legacy periods: `GET {close-url}/v1/close/periods/?legal_entity_id=...`
   (needs `PERIOD_CLOSE_VIEW` for `--maker`).
2. Classifies each one. A period is **mapped** only if its name is `YYYY-MM`
   *and* its dates are exactly that calendar month. Everything else
   (`2024-Q1`, `2026-04` ending on the 29th, unparseable dates) is **unmapped**,
   with the reason, and is never touched.
3. Derives the fiscal-year span from the mapped periods and the
   `--fy-start-month/--fy-start-day` (or uses `--from-fy/--to-fy`).
4. Ensures a `CALENDAR_MONTHS` calendar is in force at the first fiscal-year
   start: resolve; if none, **create** (`--maker`, `FISCAL_CALENDAR_PROPOSE`),
   **approve** (`--checker`, `FISCAL_CALENDAR_APPROVE`), **activate**
   (`--checker`, `FISCAL_CALENDAR_ACTIVATE`). An existing calendar of another
   pattern or start date is reported as `calendar_incompatible` and not changed.
   A draft/approved calendar left by an interrupted run is continued, not duplicated.
5. For each fiscal year without periods, `POST /v1/accounting-periods:materialize`
   (`--maker`, `PERIOD_MATERIALIZE`).
6. Matches legacy to REF-05 **by exact start/end date** (REF-05 NORMAL periods of
   the same book scope). Names and keys are never compared: legacy `2026-07` is
   REF-05 `FY2026-P04` when the year starts in April.
7. Optional last stage `--mirror-closed`: for each mapped, non-OPEN legacy period
   that matched, `POST {close-url}/v1/close/periods/{id}:mirror-to-period-service`
   (empty body, `Idempotency-Key`). OPEN periods are never replayed; periods whose
   REF-05 state already reflects the close are skipped. A 409 (mirroring disabled)
   is recorded as an error and stops the stage.

Every command carries `X-Tenant-Id`, `X-Principal-Id`, `X-Legal-Entity-Id`,
`X-Correlation-ID` and a **deterministic** `Idempotency-Key` (hash of the logical
operation), so re-running replays rather than repeats. Re-running after a full
success performs no writes at all.

## Usage

```sh
cd tools/period-backfill && go build -o period-backfill.exe .

period-backfill --tenant <tenant-uuid> \
  --entity <legal-entity-uuid> [--entity ... | --entities-file entities.txt] \
  --maker <principal-uuid> --checker <different-principal-uuid> \
  [--fy-start-month 1 --fy-start-day 1] [--from-fy 2024 --to-fy 2026] \
  [--calendar-url http://localhost:8173] [--period-url http://localhost:8174] [--close-url http://localhost:8104] \
  [--scope PRIMARY] [--calendar-code CALENDAR-MONTHS] \
  [--apply] [--mirror-closed] [--mirror-principal <uuid>] \
  [--strict] [--report report.json]
```

| Flag | Meaning |
| --- | --- |
| `--maker`, `--checker` | Required, and must differ (checked before any request). The proposer of a calendar version cannot approve it. |
| `--fy-start-month/day` | Parameters, default 01-01. Never inferred from a country. Day is 1-28. |
| `--scope` | Calendar scope **and** REF-05 `book_scope` (accounting-period-svc resolves the calendar by the book scope, so they must be equal). Opaque tenant data. |
| `--apply` | Execute. Omitted = read-only plan. |
| `--mirror-closed` | Include the mirror stage (planned in a dry run, executed with `--apply`). |
| `--strict` | Also exit 1 on unmapped/unmatched periods or blocking anomalies. |
| `--report` | Write the JSON report. |

Exit codes: `0` ok; `1` any error occurred, or `--strict` and unmapped / unmatched
periods / blocking anomalies exist (gaps are informational); `2` usage (including
maker == checker).

## Recommended order

1. **Dry run**: `period-backfill ... --report plan.json`. Nothing is written.
2. **Review**: every `UNMAPPED`, `ANOMALY` and `ERROR` line. Fix data or flags; the
   tool will not paper over them. `--strict` is useful in CI/rehearsal.
3. **`--apply`** (without `--mirror-closed`). Re-run it; the second run must report no writes.
4. **`--mirror-closed --apply`**, only **after financial-close-svc runs with
   `PERIOD_SERVICE_MIRROR=on`**. Do a `--mirror-closed` dry run first to see which
   periods would be replayed.

## Required grants

`deployments/scripts/seed-demo-rbac.ps1` seeds a maker role (`REF05_PERIOD_MAKER`:
`FISCAL_CALENDAR_PROPOSE`, `PERIOD_MATERIALIZE`, ...) and a distinct checker role
(`REF05_PERIOD_CHECKER`: `FISCAL_CALENDAR_APPROVE`, `FISCAL_CALENDAR_ACTIVATE`, ...)
for the demo. Real tenants must grant the equivalent to the two principals.
`--maker` also needs `PERIOD_CLOSE_VIEW` on financial-close-svc.

## Tests

`go test ./...` uses stateful `httptest` fakes of all three services (no network,
no real services).
