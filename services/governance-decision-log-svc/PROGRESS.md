# Governance Decision Log Service — Progress Log

Status tracker for this service. Update this file whenever meaningful
work happens (spec changes, decisions made, code milestones). Keep
entries dated and short — this is a log, not a design doc (see
CONTEXT.md for the spec/context).

## Current status

**Phase:** Phase 1 (write path) merged to `main` (PR #14). Phase 2
(query surface) and Phase 3 (close the loop — events, CI, Dockerfile,
README) both built, verified, and pushed to `shashi-changes` (mirrors
the Phase 1 flow — commits landed on the personal working branch
rather than per-phase feature branches). PR into `main` not yet opened
(to be opened manually).

## Roadmap — three phases, three branches, three PRs into `main`

Each phase branches off `main` fresh (not off the previous phase's
branch) — phase N+1 doesn't start until phase N's PR has merged.
Every phase's "done" bar includes a real-Postgres (and, for phase 3,
real-Docker) end-to-end check — `go build` passing is never sufficient
on its own. Full technical detail for each phase is in CONTEXT.md
("Build plan — three phases, three branches, three PRs into main").

| Phase | Branch (suggested) | Scope | Status |
| --- | --- | --- | --- |
| 1 | `feat/governance-decision-log-svc-write-path` | New service scaffold (cmd/config/handler/store/domain/health), migration, `POST /v1/decisions`, idempotent on `decision_id`, real `PgStore` wired into `main.go` | **Merged to `main`** (PR #14) |
| 2 | `feat/governance-decision-log-svc-query-surface` (actually landed on `shashi-changes`) | `GET /v1/decisions/{id}` + `GET /v1/decisions` with all 5 filters (actor, entity, action, rule_basis, time range), composable; handler unit tests + real Postgres integration tests | **Pushed, PR pending** |
| 3 | `feat/governance-decision-log-svc-close-loop` (actually landed on `shashi-changes`) | Publish `governance.decision.recorded` (stub-Kafka convention), add service to CI matrix + `TEST_DATABASE_URL` condition, Dockerfile, `services/README.md` entry | **Pushed, PR pending** |

## Log

### 2026-07-06 — Spec drafted
- Assembled initial service spec into `CONTEXT.md` from
  `docs/architecture/01-backend.md`, `03-microservices.md` §8.7, and
  `04-data-model.md` §7.1–7.3.
- Flagged an open cross-service design question: whether writes to this
  service should be fail-closed or fail-safe-async from the caller's
  perspective.

### 2026-07-06 — Spec finalized against concrete build briefs
- Received concrete, code-referencing task briefs for all three phases
  (write path, query surface, close-the-loop). Reconciled them against
  the draft spec and the canonical `04-data-model.md` entity:
  - Adopted the simplified MVP schema (`decision_id`, `tenant_id`,
    `legal_entity_id`, `actor_id`, `action_type`, `outcome`,
    `rule_basis`, `evaluation_context` JSONB, `correlation_id`,
    `decided_at`) as a deliberate deviation from the full
    `GovernanceDecision` entity — recorded the field mapping in
    CONTEXT.md so it isn't mistaken for an oversight later.
  - Resolved the idempotency-key question: `decision_id`, mirroring
    `audit-event-store-svc`'s `event_id` / `ON CONFLICT DO NOTHING`
    pattern exactly.
  - Resolved the fail-closed-vs-async open question as **out of scope
    for this service** — it's a decision for Authorization Service (the
    caller), not for the Decision Log Service's own contract.
  - Finalized config (`PORT=8083`, confirmed unused — 8080/8081/8082
    are already taken by the other three services), pgxpool Tier 0
    sizing, and the event-envelope approach (mirror existing publishers'
    envelope shape exactly, carry §19's required fields inside
    `payload` rather than inventing new envelope fields).
  - Confirmed `services/audit-event-store-svc` has no Dockerfile yet as
    of this writing — Phase 3 will need to write one from scratch
    unless a teammate ships one first (re-check at that time).
  - Read-API auth model question (who may call `GET /v1/decisions`) is
    still open but not blocking — none of the three briefs require an
    answer yet since no Authorization Service integration exists to
    enforce it against. Revisit before this service is exposed outside
    trusted internal callers.
- No code written yet — this was an analysis/recording pass only.

### 2026-07-06 — Phase 1 (write path) built and verified
- Scaffolded full service: `cmd/server/main.go`, `internal/{config,domain,store,handler,health}`,
  migration `000001_initial_schema.{up,down}.sql`, `go.mod`/`go.sum`.
- `POST /v1/decisions`: 201 on first insert, 200 (no overwrite) on replay
  of the same `decision_id`, 400 on missing field/bad JSON, 503 on store
  failure. `GET` endpoints deferred to Phase 2 as planned; `store.FindByID`
  exists internally already for Phase 2 to reuse.
- 5 handler unit tests (mocked store) + 3 Postgres integration tests
  (env-guarded via `TEST_DATABASE_URL`) — all passing.
- No local Go toolchain on this machine — build/vet/test all run inside
  `golang:1.25` Docker containers instead (same approach CI uses).
- Verified live end-to-end: booted the real binary against a real
  `postgres:16-alpine` container, confirmed `/healthz` + `/readyz` return
  200, POSTed a decision (201), re-POSTed the same `decision_id` with a
  different payload (200, not 201), confirmed via direct SQL query exactly
  one row exists with the **original** outcome preserved (no overwrite).
  Test containers torn down after verification.
- Not yet committed/pushed — sitting as uncommitted files pending review.

### 2026-07-06 — Manual Postman smoke test against `gdl-test-server`
- Re-verified Phase 1 via Postman against the `gdl-test-server` container
  (`go run ./cmd/server`, source bind-mounted, backed by `gdl-test-postgres`):
  `GET /healthz` → 200, `POST /v1/decisions` → 201 first time / 200 on
  replay of the same `decision_id` (idempotency confirmed live, not just
  in tests).
- Hit a stale-process gotcha worth remembering: `/healthz` initially
  returned Go's default 404 because the container's `go run` process had
  been started before `/healthz` was added to `main.go` — `go run`
  compiles once at container startup and does not hot-reload on source
  changes. Fixed with `docker restart gdl-test-server`. **Any future code
  change to this service requires a container restart to take effect.**

### 2026-07-07 — Phase 2 (query surface) built and verified
- Branched `feat/governance-decision-log-svc-query-surface` off updated
  `main` (post Phase-1 merge, PR #14).
- Added `store.PgStore.List` (all 5 filters — `actor_id`, `legal_entity_id`,
  `action_type`, `rule_basis`, `decided_at` time range — compose with AND,
  default limit 50/max 200, newest-first), refactored `FindByID` to share
  a `decisionColumns`/`scanDecision` helper with `List`.
- Added `GET /v1/decisions/{decision_id}` (200/404/503) and
  `GET /v1/decisions?actor=&entity=&action=&rule_basis=&from=&to=&limit=&offset=`
  (200 with all filters composable, 400 on invalid `from`/`to`, 503 on
  store failure) to the handler, mirroring `jurisdiction-rules-svc`'s
  `ListJurisdictions`/`GetRules` filter-parsing pattern.
- 8 new handler unit tests (mocked store) + 8 new Postgres integration
  tests (one per filter in isolation, one composing three filters
  together, one asserting empty-not-error on no match) — all passing,
  run inside `golang:1.25` (still no local Go toolchain).
- Verified live end-to-end: booted the real binary against a real
  `postgres:16-alpine` container, POSTed two decisions, confirmed
  `GET /v1/decisions/{id}` returns 200 for a known id and 404 for an
  unknown one, confirmed unfiltered `GET /v1/decisions` returns all rows,
  confirmed single-filter and composed multi-filter (`entity` +
  `rule_basis`) queries narrow correctly with AND semantics, confirmed
  `from` time-range filtering, and confirmed `from=<garbage>` returns 400.
  Test containers torn down after verification.
- Committed to `feat/governance-decision-log-svc-query-surface`
  (`238a2b2`), separate from the manual Postman pass below.

### 2026-07-07 — Manual Postman smoke test of Phase 2, committed to `shashi-changes`
- Re-verified Phase 2 live via Postman against a fresh `gdl-test-server`
  + `gdl-test-postgres` pair: `POST /v1/decisions` → 201, `GET
  /v1/decisions/{decision_id}` → 200 for the created id, 404 for an
  unknown id, `GET /v1/decisions` (unfiltered and filtered) → 200 with
  correct results. Test containers torn down after verification.
- Cherry-picked the Phase 2 commit (`238a2b2`) from
  `feat/governance-decision-log-svc-query-surface` onto `shashi-changes`
  (now `2092b32`) to match Phase 1's actual landing branch, and pushed.
- PR into `main` not yet opened — title/description/URL handed off for
  manual creation (`gh` CLI is unauthenticated on this machine).

### 2026-07-07 — Phase 3 (close the loop) built and verified
- Added `internal/events/publisher.go`: `Publisher.PublishDecisionRecorded`
  emits `governance.decision.recorded` (stub — logs the full envelope,
  does not write to Kafka yet), mirroring `identity-context-svc`'s and
  `tenant-entity-registry-svc`'s envelope shape exactly. Payload includes
  `tenant_id`, `legal_entity_id`, `actor_id`, `jurisdiction_context`
  (populated from `rule_basis`, per CONTEXT.md), plus the remaining
  decision fields.
- Wired the publisher into `Handler` (new `EventPublisher` dependency)
  and `main.go`. `CreateDecision` now publishes only on a genuine first
  insert (`created == true`) — an idempotent replay must not re-emit the
  event. A publish failure is logged but does not fail the HTTP request
  (event delivery is a stubbed, non-blocking concern).
- Added `governance-decision-log-svc` to `.github/workflows/ci.yml`'s
  `matrix.service` list and to the `TEST_DATABASE_URL` conditional
  (alongside `jurisdiction-rules-svc` / `identity-context-svc`).
- Wrote a multi-stage `Dockerfile` + `.dockerignore`, mirroring the
  structure of `audit-event-store-svc`'s Dockerfile (found on its
  as-yet-unmerged `feat/audit-event-store-context-resolved` branch —
  still absent from `main` as of this writing): `golang:1.25-alpine`
  builder → `gcr.io/distroless/static-debian12:nonroot` runtime,
  statically linked, non-root, `EXPOSE 8083`.
- Added a service list table to `services/README.md` (previously just a
  one-line header) covering all 5 existing services, not only this one.
- 3 new handler unit tests: publish fires exactly once on 201, does not
  fire on a 200 replay, and a publish error doesn't change the response
  status. All existing tests still pass (29 total: unit + Postgres
  integration).
- Verified live end-to-end against the **actual built Docker image**
  (not `go run`): `docker build` succeeded, container booted against a
  real `postgres:16-alpine` container, `/healthz` and `/readyz` returned
  200, `POST /v1/decisions` returned 201 and the container logs showed
  the `governance.decision.recorded` envelope emitted exactly once,
  `GET /v1/decisions/{id}` returned 200, and a replayed POST returned
  200 with no second "event emitted" log line. All test containers,
  network, and the built image removed after verification.
- Committed and pushed to `shashi-changes`.

### 2026-07-07 — Manual Postman session against `gdl-test-server` (post Phase 3)
- Spun up `gdl-test-postgres` + `gdl-test-server` (`go run ./cmd/server`,
  bind-mounted source) again for a live Postman session covering the
  query surface: `GET /healthz` → 200, `GET /v1/decisions?from=<garbage>`
  → 400 (invalid timestamp correctly rejected), `GET
  /v1/decisions?entity=entity-A&rule_basis=policy-v3-sod` → 200 with an
  empty result (correct — no decisions had been posted yet in this
  session). No `POST` was sent in this pass, so no
  `governance.decision.recorded` event fired; the Phase 3 event-emission
  behavior remains verified by the earlier real-Docker-image pass above,
  not by this session.
- Test containers left running at the user's request for further manual
  testing (not yet torn down as of this log entry).

## Next steps

- [x] Scaffold Phase 1 (write path).
- [x] Verify Phase 1 against a real Postgres container (boot, POST,
      confirm row, re-POST same `decision_id`, confirm no duplicate).
- [x] Commit Phase 1 to `feat/governance-decision-log-svc-write-path` and open a PR into `main`. (PR #14, merged)
- [x] Scaffold Phase 2 (query surface) on a fresh branch off updated `main`.
- [x] Verify Phase 2 against a real Postgres container (single lookup,
      unfiltered list, each filter individually, composed filters,
      invalid timestamp rejection).
- [x] Commit Phase 2 and push to `shashi-changes` (`2092b32`).
- [x] Build Phase 3 (events, CI, Dockerfile, README).
- [x] Verify Phase 3 via a real built Docker image against a real
      Postgres container (health, POST with event emitted once, GET,
      idempotent replay with no re-publish).
- [x] Commit Phase 3 and push to `shashi-changes`.
- [ ] Open a PR from `shashi-changes` into `main` covering Phase 2 + 3.
- [ ] Before Authorization Service integration begins: revisit the
      fail-closed-vs-async question and the read-API auth model
      question, both currently deferred.
