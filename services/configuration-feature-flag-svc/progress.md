# Configuration & Feature Flag Service — Progress & Phase Plan

Status: **100% aligned to the approved 3-batch build task — all three
batches written, built, tested, manually verified via Postman, AND the
real Dockerfile image built and round-trip tested, 2026-07-08.** Docker
Desktop was not running when this was first written; once started, the
full verification loop was actually executed in this sandbox (no host Go
toolchain needed): `golang:1.25-alpine` container bind-mounting the source
for the automated/dev-mode checks, and the actual multi-stage `Dockerfile`
image separately built and run for the final packaging proof — see
"Docker image build — closed" below. The only thing outside this task's
scope that remains undone is adding this service to
`deployments/docker-compose.yml`'s unified stack (§7.7 in `context.md`),
which was never part of the original 3-batch spec.

**Verification actually performed (2026-07-08):**
1. `go mod tidy && go vet ./... && go build ./cmd/server` → clean, no
   errors. The hand-copied `go.sum` (copied from
   `governance-decision-log-svc` on the reasoning that the dependency set
   is identical) was correct as-is.
2. `go test ./... -v` with `TEST_DATABASE_URL` set → **36/36 pass** (24
   handler unit tests + 12 real-Postgres integration tests), first run,
   no fixes needed.
3. The migration (`000001_initial_schema.up.sql`) applied cleanly via
   `psql` against a fresh database — first real proof the `COALESCE`/UUID
   partial-unique-index syntax and the `rollout_percentage` `CHECK`
   constraint are valid SQL, not just plausible-looking SQL.
4. The compiled binary was run as a live server (container `cff-app`,
   port 8086 published to the host). `/healthz` and `/readyz` both `200`.
5. Live HTTP round trip via `curl`: `POST /v1/config` (first write) →
   `201`; `GET /v1/config/{key}` → `200`, same value; identical
   `POST /v1/config` retry → `200` (idempotent no-op, same `config_id`
   returned, not a new row) — confirms the upsert-with-value-equality
   design in §7.3 works against a real database, not just in the unit
   tests.

## Manual Postman verification (2026-07-08, same day — human-driven, not automated)

After the automated pass above, every endpoint was independently exercised
by hand through Postman against the same live server. This surfaced and
closed real gaps the automated suite alone would not have caught:

- **Environment mistake found and fixed**: the `go test` run above shares
  the same Postgres database as the live demo server. One integration
  test (`TestPgStore_FeatureFlag_ErrorsWrapErrStoreUnavailable`) correctly
  drops `feature_flags` to test the store-unavailable path, but has no
  teardown to restore it — fine in an isolated test DB, but since it was
  the last test in the file and this database is shared with the live
  demo, it left `feature_flags` missing until manually recreated
  mid-session. **Lesson for next time**: never point `TEST_DATABASE_URL`
  at the same database a live demo is running against.
- **Postman client-side gotcha identified**: a trailing newline character
  (rendered as `↵` in Postman's UI) in a pasted URL or query-param value
  breaks exact-match filters and route matching, producing a misleading
  empty list or a raw Go `404 page not found` (not this service's own
  JSON error shape) — not a service bug, but worth knowing before
  suspecting one.

**Full matrix manually confirmed correct, live, via Postman:**

| Category | `/v1/config` | `/v1/flags` |
|---|---|---|
| Create (first write) | `201` ✓ | `201` ✓ |
| Read by key | `200` ✓ | `200` ✓ |
| List | `200`, array ✓ | `200`, array ✓ |
| Idempotent no-op (identical value) | `200`, same ID ✓ | `200`, same ID ✓ |
| Real version change | `201`, new ID; old row confirmed end-dated-not-deleted via direct `psql` query ✓ | `201`, new ID; old row confirmed end-dated-not-deleted via direct `psql` query ✓ |
| Field validation | `400 missing_field` ✓ | `400 invalid_field` (rollout_percentage=150) ✓ |
| Not found | (same code path as flags) | `404 feature_flag_not_found` ✓ |

**Docker image build — closed (2026-07-08, same day, follow-up):** the gap
above ("actual multi-stage image never built") was found during a
spec-compliance re-check prompted by "is this 100% aligned to the doc" and
closed immediately since Docker was already running. `docker build` on the
real `Dockerfile` succeeded cleanly (`golang:1.25-alpine` build stage →
`distroless/static-debian12:nonroot` runtime). Ran the built image as a
container (`configuration-feature-flag-svc:verify`, a temporary throwaway
tag) against the same `cff-pg` Postgres, on a separate host port (`18087`)
so it didn't disturb the already-running manual-testing container
(`cff-app`, port `8086`). `/healthz` and `/readyz` both `200`. Full
round trip from inside the real image: `POST /v1/config` (`201`) →
`GET` (`200`, value=1) → `POST` new version (`201`, new `config_id`) →
`GET` (`200`, value=2, new `config_id`) — exactly Batch 3's own
verification requirement, now genuinely met. The throwaway container was
removed after; `cff-app`/`cff-pg` (your manual-testing containers) were
untouched throughout.

**Still not done, and not part of this build's scope**: not added to
`deployments/docker-compose.yml`'s unified stack (§7.7 in `context.md`).

Full citations, exact schema, exact endpoint contracts, and the
idempotency design decision are in `context.md` §7.

## Port correction (found while building Batch 3 — read this before anything else)

The task spec assigned port **8084**, assuming it was free. It is not: a
real `deployments/docker-compose.yml` (merged into `main` after the task
was written) assigns 8084 to `audit-event-store-svc` for real — verified
by reading that file directly, not inferred. **Corrected to port 8086.**
See `context.md` §7.7 for the full account — this is the same class of
mistake `policy-svc`'s own defensive port reservation was written to
avoid, just from the other direction.

## Batch 1 — Schema + config read/write path

- [x] `config_entries` table: `config_id`, `key`, `value` (JSONB),
      `environment`, `tenant_id` (nullable), `effective_from`,
      `effective_to` (nullable), `created_by_principal_id`, `created_at` —
      `deployments/migrations/000001_initial_schema.up.sql`
- [x] No `UPDATE`/`DELETE` — enforced by only ever writing `INSERT` plus a
      single `effective_to`-only `UPDATE` used by the upsert transaction
- [x] Partial unique index — at most one currently-effective row per
      `(key, environment, tenant_id)` scope
- [x] Standard entrypoint wiring — `cmd/server/main.go`,
      `internal/config/config.go` (port default **8086** — see correction
      above; DB name default `configuration_feature_flag`),
      `internal/health/health.go` (structural copy of
      `governance-decision-log-svc`'s)
- [x] `POST /v1/config` — upsert-with-value-equality-check (§7.3 in
      context.md), transactional end-date-and-insert
      (`internal/handler/handler.go: UpsertConfigEntry`, backed by
      `internal/store/pg_store.go: PgStore.UpsertConfigEntry`)
- [x] `GET /v1/config/{key}?environment=X&tenant_id=Y` — exact-tuple
      lookup, 404 if none currently effective (`GetConfigEntry`)
- [x] `GET /v1/config?environment=X&tenant_id=Y` — list, both filters
      optional (`ListConfigEntries`)
- [x] Handler unit tests (stub store, no DB) —
      `internal/handler/handler_test.go`: 201 first write / 200
      idempotent-same-value / 400 missing field / 400 invalid JSON / 503
      store unavailable for `POST`; 200 found / 404 not found / 400
      missing environment / 503 for the single-key `GET`; 200 empty-array
      / filters-forwarded / 503 for the list `GET`; publish-failure
      doesn't fail the request
- [x] Postgres integration tests (`TEST_DATABASE_URL`-guarded) —
      `internal/store/pg_store_test.go`: first-write + idempotent-same-
      value no-op (exactly 1 row after a retry); a genuinely new value
      end-dates the old row **without deleting it** and `GET` returns the
      new one (the one test that actually proves the versioning claim,
      not just asserts it in a comment); tenant-scope isolation (a
      tenant-specific override and the global default coexist as
      independent currently-effective rows); list filtering by
      environment/tenant; `ErrStoreUnavailable` wrapping on all three
      methods when the table is dropped out from under the store

**Verified live (2026-07-08):** `go build`/`go vet` clean; all config
unit + integration tests pass (part of the 36/36 run — see Status above);
`POST → GET → identical-retry` round trip confirmed by hand via `curl`
against a live server and real Postgres.

## Batch 2 — Feature flags (identical pattern, on top of Batch 1)

- [x] `feature_flags` table: `flag_id`, `key`, `enabled` (boolean),
      `environment`, `tenant_id` (nullable), `rollout_percentage`
      (integer, default 100, `CHECK` 0–100), `effective_from`,
      `effective_to` (nullable), `created_by_principal_id`, `created_at`
      — same migration file as Batch 1 (both tables ship together since
      they're built in the same pass here, unlike the original 3-batch
      task framing which sequenced them separately — see design decision
      4 below)
- [x] `POST /v1/flags` — same upsert-with-equality-check pattern,
      comparing `(enabled, rollout_percentage)` instead of a JSON value;
      400 if `rollout_percentage` given outside 0–100 (validated in the
      handler; the DB `CHECK` constraint is a second, independent
      backstop — see the dedicated test for it)
- [x] `GET /v1/flags/{key}?environment=X&tenant_id=Y`
- [x] `GET /v1/flags?environment=X&tenant_id=Y`
- [x] Handler unit tests — same coverage shape as config's, plus a
      dedicated test proving `enabled: false` is treated as a real value,
      not a missing field (using `*bool` in the request struct)
- [x] Postgres integration tests — same versioning proof as config's
      (`rollout_percentage` change end-dates the old row); a case proving
      the DB `CHECK` constraint itself rejects an out-of-range value
      (independent of handler-side validation, in case a future caller of
      the store package skips the handler)

**Verified live (2026-07-08):** all feature-flag unit + integration tests
pass, including the CHECK-constraint and explicit-`false` cases (part of
the 36/36 run). Also manually verified end-to-end through Postman later
the same day: create, read, list, idempotent no-op, a real
`rollout_percentage` change (old row confirmed end-dated-not-deleted via
direct `psql` query), the `400` out-of-range case, and `404` not-found —
see the "Manual Postman verification" section below for the full matrix.

## Batch 3 — Events, CI, Dockerfile, README

- [x] `internal/events/publisher.go` — mirrors
      `governance-decision-log-svc`'s exactly (same envelope, same
      log-only stub). Publishes `config.updated` / `feature_flag.updated`
      only on a real transition (`created=true`), never on the
      idempotent-no-op path — verified by dedicated handler tests
- [x] CI — added `configuration-feature-flag-svc` to `matrix.service` and
      the `TEST_DATABASE_URL` conditional in `.github/workflows/ci.yml`
- [x] Dockerfile + `.dockerignore` — mirror
      `governance-decision-log-svc/Dockerfile` exactly; binary name
      `configuration-feature-flag-svc`, `EXPOSE 8086` (corrected from the
      task's original 8084 — see correction note above). **Actually
      built** (`docker build`) and **actually run** as a container against
      real Postgres, full round trip confirmed — not just written. See
      "Docker image build — closed" below.
- [x] `services/README.md` — added the service row (port 8086)

**Fully verified live (2026-07-08):** event publishing confirmed via
handler tests (publish-only-on-real-transition). CI config and
`services/README.md` are file edits, not independently runnable here.
**The actual `Dockerfile` has since been built and run** — see "Docker
image build — closed" below for the full transcript. Batch 3 is now
proven to the same standard as `policy-svc`'s was
(`policy-svc/context.md` §18).

**Not done, flagged as a follow-up, not part of this task**: this service
has not been added to `deployments/docker-compose.yml`'s unified 5-service
stack. Adding a 6th service there is reasonable but wasn't part of this
build's scope — see `context.md` §7.7.

## Design decisions made, not specified by the task (see context.md §7 for full reasoning)

1. **Idempotency key** — resolved as upsert-with-value-equality (§7.3),
   not a caller-supplied ID, since none was specified and none fits this
   request shape naturally. Flag if a caller-supplied version ID was
   actually wanted instead — that's a breaking-change addition later, not
   a quick fix.
2. **`value` column type** — JSONB, the task explicitly left this as "your
   call."
3. **No fallback from tenant-specific to global on the single-key `GET`**
   — exact-tuple match only, per a literal reading of the task's wording.
   `policy-svc`-style scope precedence was deliberately not added since
   nothing asked for it — flag if that's wrong.
4. **Both tables shipped in one migration / one batch pass** instead of
   sequencing config and flags as two separate migrations across two
   separate sessions, since both schemas were fully specified up front
   this time (unlike `policy-svc`, which was genuinely built incrementally
   across batches with review in between).
5. **Port corrected from 8084 to 8086** — see the correction note above
   and `context.md` §7.7. Not a "your call" design choice like 1–4; a
   genuine stale-assumption catch.

## What's explicitly NOT built (and why)

- **No consumers wired up anywhere else in the platform.** This service
  stores and serves values; nothing currently reads from it. That's
  expected for a v1 — the same posture `governance-decision-log-svc` had
  before `policy-svc`'s Batch D wired a real caller into it.
- **No real Kafka writer** — `internal/events/publisher.go` is a logged
  stub, same as every other service's event publishing in this repo.
- **No caching layer** — direct Postgres reads, consistent with every
  other service's v1 scope in this repo.
- **Not added to `deployments/docker-compose.yml`** — see Batch 3 above.

## Required local verification (for you to run independently)

This was written in a sandbox with no Go toolchain and no running Docker
daemon — nothing below has actually been executed, unlike every batch of
`policy-svc` through Batch E:

1. `cd services/configuration-feature-flag-svc && go mod tidy` — verify
   `go.sum` (copied from `governance-decision-log-svc` since the
   dependency set — chi, pgx/v5, zap — is identical; should already be
   correct, but re-verify)
2. `go build ./...` and `go vet ./...`
3. `go test ./...` (unit tests, no DB needed)
4. Spin up Postgres, set `TEST_DATABASE_URL`, re-run `go test ./...` for
   `internal/store/pg_store_test.go`
5. `go run ./cmd/server` against real Postgres and manually walk: create a
   config value, read it back, create a new version of the same key,
   confirm the read now returns the new value; repeat for a feature flag
6. Build the Docker image and run it against real Postgres to confirm the
   same round trip works from inside the container
