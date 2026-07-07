# Policy Service — Progress & Phase Plan

Status: **All 3 batches (A, B, C) built, tested, and verified — including
a real Docker image build and run.** This supersedes the earlier
15-phase speculative plan: the build was task-approved and scoped into
3 sequential batches, each its own branch off `main` in
`ZoikoGroup/zoiko-suite-backend`. Full citations, exact schema, exact
endpoint contracts, and exact code patterns to mirror are in
`context.md` §13. **policy-svc's v1 scope is now functionally complete**
— see `context.md` §18 for the closing summary and what's genuinely left
for a human to decide (Kafka wiring, Authorization Service integration,
the 3 remaining policy types) versus what's actually done.

**Verification actually performed (2026-07-07):** no Go toolchain exists
in the assistant's sandbox, so a Docker container (`golang:1.25-alpine`)
was used to run `go mod tidy`, `go vet ./...`, `go build`, and
`go test ./... -v` against a real throwaway Postgres 16 container.
Batch A: 19/19 tests pass; one real bug found and fixed (JSONB whitespace
comparison — `context.md` §15). Batch B: added on top, 27/27 tests pass
on the first run, no new bugs. Batch C: added event publishing (which
required extending `ActivateVersion`'s return signature to report
superseded versions and a `transitioned` flag — a real refactor across
store/handler/tests, not just new files), **29/29 tests pass**; then the
actual `Dockerfile` was built into a real image and run against Postgres
— full create→version→activate→supersede→evaluate round trip exercised
from inside that image, with all four event types confirmed actually
emitted in the logs, including `policy.rule.retired` correctly tied to
the same correlation ID as the activation call that caused it. See
`context.md` §16 (Batch A transcript), §17 (Batch B transcript), §18
(Batch C transcript).

**Synced with `origin/main` (2026-07-07):** pulled 13 commits from `main`
into `shashi-changes` before committing this work. Notable changes that
landed independently of policy-svc: `identity-context-svc` and
`tenant-entity-registry-svc` got real Kafka producers (no longer
log-only stubs — worth re-checking against `context.md`/`progress.md`'s
"nothing publishes events for real yet" framing before relying on it for
future services), `jurisdiction-rules-svc` got its admin endpoints wired
(previously commented out — the exact endpoints this service's Batch A
was modeled on), and `audit-event-store-svc` got a real server,
Dockerfile, and docker-compose entry. Merge was clean — no conflicts in
`.github/workflows/ci.yml` or `services/README.md` despite both being
touched on both sides. Re-ran the full test suite post-merge: still
clean, nothing in policy-svc affected.

Repo correction: remote is `ZoikoGroup/zoiko-suite-backend`. Services
that exist now: `identity-context-svc` (8080), `tenant-entity-registry-svc`
(8081), `jurisdiction-rules-svc` (8082), `governance-decision-log-svc`
(8083). `policy-svc` is assigned **port 8085** (not 8084 — reserved
defensively in case `configuration-feature-flag-svc` lands first).

Design approach: mirror `jurisdiction-rules-svc` directly rather than
inventing a new shape — `Policy` ↔ `Jurisdiction` (lightweight named
container), `PolicyVersion` ↔ `JurisdictionRule` (effective-dated,
state-machined versions). Same idempotent
`INSERT...ON CONFLICT DO NOTHING` dedup, same "handler owns the state
machine, store just enforces caller-supplied `allowedPriors`" split.

---

## Batch A — Policy / PolicyVersion CRUD

Branch off `main`. Mirrors `jurisdiction-rules-svc`'s `domain/types.go`,
`internal/store/pg_store.go` (`CreateJurisdiction`, `CreateRule`,
`TransitionRuleStatus`), and its migration directly (`context.md`
§13.1–13.3). **Code written, not yet compiled/tested** — see the caveat
at the top of this file.

- [x] `policies` table: `policy_id`, `policy_code`, `policy_name`,
      `policy_type` (VARCHAR, data-driven — `APPROVAL_THRESHOLD`,
      `SPEND_CONTROL`, `SOD_RULE`, `SIGNATORY_MATRIX`; new types are a
      data row, never a code change), `created_at`,
      `created_by_principal_id` — `deployments/migrations/000001_initial_schema.up.sql`
- [x] `policy_versions` table: `policy_version_id`, `policy_id`,
      `tenant_id` (nullable — null = global), `legal_entity_id`
      (nullable), `rule_payload` (JSONB), `effective_from`,
      `effective_to`, `version_status` (`DRAFT`/`ACTIVE`/`SUPERSEDED`/
      `RETIRED`), `created_at`, `created_by_principal_id` — same migration
- [x] No `UPDATE`/`DELETE` on either table — enforced by only ever writing
      `INSERT` (create) or the single `version_status`-only `UPDATE` used
      by activation/supersede in `internal/store/pg_store.go`
- [x] Standard entrypoint wiring: config → zap → pgxpool fail-fast →
      store → handler → `/healthz` + `/readyz` → graceful shutdown —
      `cmd/server/main.go`, `internal/config/config.go`,
      `internal/health/health.go` (byte-for-byte structural copy of
      `jurisdiction-rules-svc`'s shape, service name/port swapped)
- [x] `POST /v1/policies` — `internal/handler/handler.go: CreatePolicy`
- [x] `POST /v1/policies/{policy_id}/versions` —
      `internal/handler/handler.go: CreatePolicyVersion`
- [x] `POST /v1/policies/{policy_id}/versions/{version_id}/activate` —
      `internal/handler/handler.go: ActivateVersion`, backed by
      `internal/store/pg_store.go: ActivateVersion` — supersedes the prior
      ACTIVE version and activates the target in one DB transaction
      (supersede-then-activate ordering so the partial unique index
      `idx_policy_versions_one_active_per_scope` is never violated
      mid-transaction), reusing the same generic
      `UPDATE ... WHERE version_status = ANY($allowedPriors)` primitive
      (`transitionVersionStatus`) that mirrors `TransitionRuleStatus`
- [x] `GET /v1/policies/{policy_id}/versions` —
      `internal/handler/handler.go: ListVersionHistory`
- [x] Unit tests (stub store, no DB) — `internal/handler/handler_test.go`:
      created/idempotent-replay/missing-field/conflict/store-unavailable
      for `CreatePolicy`; created/missing-field/policy-not-found for
      `CreatePolicyVersion`; success/missing-actor/policy-id-mismatch/
      invalid-transition for `ActivateVersion`; empty-array/not-found/
      newest-first for `ListVersionHistory`
- [x] Integration tests (`TEST_DATABASE_URL`-guarded, skip if unset) —
      `internal/store/pg_store_test.go`: `CreatePolicy` idempotency+409,
      `CreatePolicyVersion` idempotency+409+policy-not-found,
      `ActivateVersion` supersede+idempotent-retry+invalid-transition,
      `ListVersionHistory` newest-first-includes-superseded

**Verified against real Postgres (2026-07-07 — DONE):** created a policy,
created a version, activated it, created a second version and activated
that too — confirmed the first version is `SUPERSEDED` (not deleted) and
the history endpoint shows both, newest first. Also verified the
idempotent-retry path over real HTTP: POSTing the identical
`CreatePolicyVersion` request twice returns `201` then `200` (not a false
`409` — see the bug fix below). All 19 Go tests pass
(`go test ./... -v` inside a `golang:1.25-alpine` container against a
throwaway `postgres:16-alpine` container).

### Bug found and fixed during verification: JSONB whitespace comparison

`internal/store/pg_store.go`'s `CreatePolicyVersion` conflict check
originally used `bytes.Equal(v.RulePayload, params.RulePayload)` to
detect whether a dedup-key match had a genuinely differing payload (409)
vs. an identical idempotent retry (200). Postgres's JSONB type
re-serializes with its own whitespace convention (a space after every
`:` and `,`) — so a compact-JSON request body (e.g.
`{"threshold_amount":5000}`, the format Go's own `json.Marshal` produces)
read back from the DB as `{"threshold_amount": 5000}` would **never**
byte-match the original request, causing every legitimate idempotent
retry to incorrectly 409. Caught by
`TestPgStore_CreatePolicyVersion_IdempotencyConflictAndPolicyNotFound`
failing on first real run. Fixed by replacing the byte comparison with a
semantic one (`jsonEqual`: unmarshal both sides, `reflect.DeepEqual`) —
insensitive to whitespace, key order, and numeric formatting. Confirmed
fixed both by the retest (19/19 pass) and by a live curl repro.

### Required local verification (for you to re-confirm independently)

The above was run in the assistant's own Docker-based sandbox, not your
machine — re-run at least once locally / in CI before merging:

1. `cd services/policy-svc && go mod tidy` — regenerates `go.sum` locally
   (already regenerated once in the assistant's sandbox; diff it against
   what you get)
2. `go build ./...` and `go vet ./...`
3. `go test ./...` (unit tests, no DB needed)
4. Spin up Postgres, set `TEST_DATABASE_URL`, re-run `go test ./...` for
   `internal/store/pg_store_test.go`
5. `go run ./cmd/server` against real Postgres and re-walk the HTTP round
   trip by hand (or via Postman — see the endpoint reference already
   shared)

## Batch B — Evaluation (the core value of the service)

Branch off `main`, on top of Batch A once policy/version CRUD exists.
This is what `03-microservices.md` §8.1 means by "evaluate policy against
action context" and "validate threshold applicability." **Code written,
built, tested (27/27 pass), and verified live — see `context.md` §17.**

Scope narrowly — do **not** build a generic engine for all four policy
types in one pass. Implement real evaluation for exactly one type:
`APPROVAL_THRESHOLD`. A plain `switch` on `policy_type` is the right
amount of structure; no plugin/registry system for four total cases.

- [x] `GET /v1/policies?policy_type=X&tenant_id=Y&legal_entity_id=Z` —
      the "get applicable policy set" API — `ListApplicablePolicyVersions`
      in `internal/handler/handler.go`, backed by
      `internal/store/pg_store.go: FindApplicableVersions`. Returns every
      currently-ACTIVE version compatible with the given scope, ordered
      most-specific-scope first (exact tenant+entity match, then
      tenant-only, then global) — see `context.md` §15 item 5 for the
      precedence rule and its documented v1 limitation (tie-break when
      multiple distinct policies share a type at the same tier)
- [x] `POST /v1/policies/evaluate` — `Evaluate` in `handler.go`
  - [x] look up the applicable ACTIVE version for that type+scope (reuses
        `FindApplicableVersions`, takes the most specific match)
  - [x] **no applicable policy → `404`** — confirmed live: evaluating an
        unrelated `policy_type` with nothing active returns
        `{"error":"no_applicable_policy",...}`
  - [x] for `APPROVAL_THRESHOLD`: compare `action_context.amount` against
        `rule_payload.threshold_amount` — `amount > threshold` →
        `APPROVAL_REQUIRED`, `amount <= threshold` (including exactly
        equal) → `WITHIN_THRESHOLD` (a documented choice, not specified
        by the task — see `context.md` §15 item 6)
  - [x] response: `{"result": ..., "policy_version_id": "...",
        "rule_basis": "<policy_code>:<policy_version_id>"}` — the
        `rule_basis` format is a documented choice, not specified by the
        task (§15 item 6); feeding `governance-decision-log-svc`'s
        `POST /v1/decisions` is still a separate future task
  - [x] unimplemented `policy_type` (e.g. `SPEND_CONTROL`) → `501`, not a
        silent no-op or a crash
- [x] idempotency falls out naturally (pure read/compute, no side
      effects) — confirmed: no write path exists anywhere in this batch
- [x] **no caching** — not added; a direct Postgres read is used (this
      permanently supersedes the old speculative "Phase 9 — caching"
      plan; it's deferred indefinitely, not scheduled)

**Verified against real Postgres (2026-07-07 — DONE):** activated an
`APPROVAL_THRESHOLD` version with `threshold_amount:5000`,
`POST /v1/policies/evaluate` with amount `7500` → `APPROVAL_REQUIRED`,
amount `1000` → `WITHIN_THRESHOLD`, amount `5000` (exactly equal) →
`WITHIN_THRESHOLD`; returned `policy_version_id` matched the version
activated. Also verified tenant-scope precedence and isolation live: a
tenant with its own override sees both its override (first) and the
global fallback (second); a different tenant with no override sees only
the global fallback and never leaks the first tenant's data — see
`context.md` §17 for the full transcript.

## Batch C — Events, CI, Dockerfile, README

Branch off `main`, once policy CRUD and evaluation both exist and are
tested. **Done, built, tested, and verified via a real Docker image run
— see `context.md` §18.**

- [x] Event publishing — `internal/events/publisher.go`, mirrors
      `governance-decision-log-svc/internal/events/publisher.go` exactly
      (same `envelope` struct, same log-only stub `emit()`, no real Kafka
      writer yet — `// TODO: publish to Kafka topic` left in place):
  - [x] `policy.created` on policy creation (first insert only — verified
        not re-published on idempotent replay, `TestCreatePolicy_IdempotentReplay`)
  - [x] `policy.updated` on new version created (first insert only)
  - [x] `policy.version.activated` on activation (real transition only —
        verified not re-published on idempotent no-op retry,
        `TestActivateVersion_IdempotentNoOp_DoesNotRepublish`)
  - [x] `policy.rule.retired` on supersede — required extending
        `Store.ActivateVersion`'s return signature to
        `(*PolicyVersion, []*PolicyVersion, bool, error)` so the store can
        tell the handler *which* version(s) got superseded (via `RETURNING`
        on the supersede `UPDATE`) and *whether* a real transition happened
        at all (vs. idempotent no-op) — a genuine signature change across
        `Store` interface + `PgStore` + handler + all tests, not just new
        files. Confirmed live: activating a second version in the same
        scope correctly emits `policy.rule.retired` for the first, tied to
        the same correlation ID as the activating request.
- [x] CI — added `policy-svc` to `matrix.service` in
      `.github/workflows/ci.yml` and to the `TEST_DATABASE_URL`
      conditional. Confirmed the CI Postgres service container already
      creates a database named `testdb` shared across all matrix
      services (not `policy` — policy-svc's own `config.go` default of
      `policy` is only used outside CI; `TEST_DATABASE_URL` overrides it
      in tests regardless).
- [x] Dockerfile + `.dockerignore` — mirror
      `governance-decision-log-svc/Dockerfile` exactly (`golang:1.25-alpine`
      builder → `distroless/static-debian12:nonroot` runtime, static
      binary, `-trimpath -ldflags="-s -w"`); binary name `policy-svc`,
      `EXPOSE 8085`. **Actually built** (`docker build`) and **actually
      run** as a container against real Postgres — not just written.
- [x] Updated `services/README.md` — added the `policy-svc` row (port
      8085, one-line status).

**Verify:** build the Docker image and run the container against a real
Postgres — full create-policy → create-version → activate → evaluate
round trip from inside the container.

---

## Explicit non-goals (do not do these as part of this build)

- **Do not consume `entity.created`, `role.updated`, or
  `authority.delegated`** even though `03-microservices.md` §8.1 lists
  them as consumed events. Nothing in the codebase publishes those for
  real yet (all logged stubs in their respective services) — there's
  nothing to actually consume. Follow-up once the producers are real.
- **Do not wire calls to Authorization Service** for admin writes
  (create/activate) — it doesn't exist yet. Same posture
  `governance-decision-log-svc` shipped with; revisit when Authorization
  Service exists.
- **Do not add caching/Redis/sidecar evaluation** in v1 — explicitly
  deferred (Batch B).
- **Do not build evaluation logic for `SPEND_CONTROL`, `SOD_RULE`, or
  `SIGNATORY_MATRIX`** — only `APPROVAL_THRESHOLD` gets real logic in v1;
  the others are future `switch` cases, not part of this build.

## Blocking cross-service dependencies (tracked, not yet resolvable)

- **Authorization Service** — doesn't exist; deferred per non-goals
  above rather than blocking.
- **Access Control Service** / **Delegated Authority Service** — don't
  exist; block real consumption of `role.updated` / `authority.delegated`
  (deferred per non-goals above).
- **Kafka event backbone** — not wired anywhere in the repo yet; all
  event publishing across all services is a log-only stub. Policy
  Service's publisher follows the same stub pattern, not a gap specific
  to this service.
