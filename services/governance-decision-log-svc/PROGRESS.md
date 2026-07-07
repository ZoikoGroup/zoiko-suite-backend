# Governance Decision Log Service — Progress Log

Status tracker for this service. Update this file whenever meaningful
work happens (spec changes, decisions made, code milestones). Keep
entries dated and short — this is a log, not a design doc (see
CONTEXT.md for the spec/context).

## Current status

**Phase:** Phase 1 (write path) built and verified. Not yet committed
to a branch. Phase 2 (query surface) not started.

## Roadmap — three phases, three branches, three PRs into `main`

Each phase branches off `main` fresh (not off the previous phase's
branch) — phase N+1 doesn't start until phase N's PR has merged.
Every phase's "done" bar includes a real-Postgres (and, for phase 3,
real-Docker) end-to-end check — `go build` passing is never sufficient
on its own. Full technical detail for each phase is in CONTEXT.md
("Build plan — three phases, three branches, three PRs into main").

| Phase | Branch (suggested) | Scope | Status |
| --- | --- | --- | --- |
| 1 | `feat/governance-decision-log-svc-write-path` | New service scaffold (cmd/config/handler/store/domain/health), migration, `POST /v1/decisions`, idempotent on `decision_id`, real `PgStore` wired into `main.go` | **Built + verified** (not committed yet) |
| 2 | `feat/governance-decision-log-svc-query-surface` | `GET /v1/decisions/{id}` + `GET /v1/decisions` with all 5 filters (actor, entity, action, rule_basis, time range), composable; handler unit tests + real Postgres integration tests | Not started |
| 3 | `feat/governance-decision-log-svc-close-loop` | Publish `governance.decision.recorded` (stub-Kafka convention), add service to CI matrix + `TEST_DATABASE_URL` condition, Dockerfile, `services/README.md` entry | Blocked on Phase 1 + 2 merge |

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

## Next steps

- [x] Scaffold Phase 1 (write path).
- [x] Verify Phase 1 against a real Postgres container (boot, POST,
      confirm row, re-POST same `decision_id`, confirm no duplicate).
- [ ] Commit Phase 1 to `feat/governance-decision-log-svc-write-path` and open a PR into `main`.
- [ ] Scaffold Phase 2 (query surface) on a fresh branch off updated `main`.
- [ ] Scaffold Phase 3 (events, CI, Dockerfile, README) on a fresh
      branch off updated `main`; verify via a real Docker container
      against real Postgres.
- [ ] Before Authorization Service integration begins: revisit the
      fail-closed-vs-async question and the read-API auth model
      question, both currently deferred.
