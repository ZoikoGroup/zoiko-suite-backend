# Policy Service — Progress & Phase Plan

Status: **All 3 original batches (A, B, C) plus a follow-up Batch D built,
tested, and verified — including a real Docker image build and a real
second live service (`governance-decision-log-svc`) for cross-service
proof.** This supersedes the earlier 15-phase speculative plan: the build
was task-approved and scoped into 3 sequential batches, each its own
branch off `main` in `ZoikoGroup/zoiko-suite-backend`; Batch D followed
from a spec-compliance self-review (see "so what we need to do to make it
100% aligned" in conversation). Full citations, exact schema, exact
endpoint contracts, and exact code patterns to mirror are in
`context.md` §13. **policy-svc's v1 scope, including its evidence
obligation, is now functionally complete** — see `context.md` §19 for the
closing summary and what's genuinely left for a human to decide (Kafka
wiring, Authorization Service integration, the 3 remaining policy types,
consuming `entity.created`) versus what's actually done.

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

## Batch D — Close the evidence-obligation gap (post-review, 2026-07-07)

A spec-compliance review against the literal text of `03-microservices.md`
§8.1 found one real, unclosed gap: `Evaluate` returned `rule_basis`/
`policy_version_id` in its HTTP response but **never persisted anything**
— the "preserve evaluation basis for governed decisions" evidence
obligation was not actually met, only structurally set up for later. This
batch closes it. **Done, built, tested, and verified against a real,
independently-running `governance-decision-log-svc` instance — see
`context.md` §19.**

- [x] New `internal/decisionlog` package — `Client` interface +
      `HTTPClient`, POSTs to `governance-decision-log-svc`'s
      `POST /v1/decisions` after every real `APPROVAL_THRESHOLD`
      evaluation (not on `404`/`501` paths — nothing was evaluated there)
- [x] Two contract mismatches discovered and resolved while wiring this,
      not assumed away:
  - governance-decision-log-svc requires `tenant_id`/`legal_entity_id`
    non-empty on every decision; policy-svc legitimately allows both nil
    (global policies). Resolved with a `"GLOBAL"` sentinel value,
    confirmed accepted by a real instance with no special-casing needed
    on the decision-log side.
  - governance-decision-log-svc requires `actor_id`; `Evaluate`'s
    original request shape had no actor field at all. Added
    **`evaluated_by_principal_id`** as a new required field on
    `POST /v1/policies/evaluate` — a breaking change to an
    already-shipped, already-Postman-tested endpoint. Updated all
    existing tests and this doc's earlier endpoint reference accordingly.
- [x] `decision_id` field added to the evaluate request — **shipped
      optional in this batch, made required a follow-up pass later the
      same day (see Batch E below) once the duplicate-evidence-on-retry
      gap this created was judged worth closing immediately rather than
      documenting and moving on.**
- [x] Call is synchronous (matches this codebase's actual convention for
      Kafka publishing, not a goroutine) but best-effort: failures are
      logged, never surfaced or blocking — verified live by killing
      `governance-decision-log-svc` mid-session and confirming `Evaluate`
      still returned `200`.
- [x] HTTP client timeout tightened from an initial 5s to **2s** after
      live testing showed a DNS-resolution failure alone (fully-down
      dependency) cost ~2.5s wall-clock — a real, measured latency risk
      against the docs' "must not become a bottleneck" requirement for
      Policy Service, not a hypothetical one.

**Verified against a real, second live service (2026-07-07 — DONE):**
stood up an actual `governance-decision-log-svc` instance (its own
Postgres database, same Docker network) — not a stub. Ran `Evaluate`
with a caller-supplied `decision_id` and real `tenant_id`/
`legal_entity_id`, then fetched that exact `decision_id` back from
`governance-decision-log-svc` directly and confirmed every field
(`actor_id`, `outcome`, `rule_basis`, `evaluation_context`,
`correlation_id`) matched. Repeated with no `tenant_id`/`legal_entity_id`
and confirmed both come back as `"GLOBAL"`. Then stopped
`governance-decision-log-svc` entirely and confirmed `Evaluate` still
returns `200` with the failure only logged.

### Postman impact (superseded by Batch E — see there for the current body shape)

`POST /v1/policies/evaluate` now requires `evaluated_by_principal_id` in
the body — existing saved requests need updating or they'll fail `400
missing_field`. Example (incomplete as of Batch E — `decision_id` is also
required now):

```json
{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":7500},"evaluated_by_principal_id":"admin-1"}
```

## Batch E — Close the retry-duplicate-decision gap (same day, follow-up)

Prompted by a direct "so what we need to do to make it 100% aligned"
follow-up. §19/Batch D above left one loose end: `decision_id` was
optional, so a client-side retry of `Evaluate` with none supplied could
record a duplicate decision in `governance-decision-log-svc`. Re-checked
against doctrine's idempotency requirement applied to the evidence write
specifically (not just `Evaluate`'s returned result) and judged: this
needed no business input, only an implementation decision, so it was
fixed immediately instead of added to the standing gap list. **Done,
tested, and verified live — see `context.md` §20.**

- [x] `decision_id` changed from optional to **required** on
      `POST /v1/policies/evaluate` — second breaking change to this
      endpoint today (first was `evaluated_by_principal_id` in Batch D)
- [x] Removed the `uuid.New()` fallback generation in
      `evaluateApprovalThreshold` — `google/uuid` no longer imported in
      `internal/handler`
- [x] 35/35 tests pass (34 from Batch D + `TestEvaluate_MissingDecisionID`;
      `TestEvaluate_ApprovalRequired` updated to assert the *supplied*
      `decision_id` is forwarded verbatim rather than asserting one gets
      generated)

**Verified live (2026-07-07 — DONE):** `POST /v1/policies/evaluate` with
`decision_id` omitted → `400 {"error":"missing_field","field":"decision_id"}`.
Called `Evaluate` **twice** with the identical `decision_id` against the
real `governance-decision-log-svc` instance, then queried
`GET /v1/decisions?actor=admin-1` and counted matches — **exactly one**
decision record after two calls. This is the actual guarantee, proven,
not just documented.

### Postman impact (current — supersedes the Batch D example above)

`POST /v1/policies/evaluate` requires **both** `evaluated_by_principal_id`
**and** `decision_id`. Current full example:

```json
{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":7500},"evaluated_by_principal_id":"admin-1","decision_id":"some-unique-id-you-control"}
```

Use a stable, caller-generated ID (e.g. your own request/correlation ID)
so retries of the same logical evaluation don't create duplicate evidence
records in `governance-decision-log-svc`.

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
        `POST /v1/decisions` **was** a separate future task when this was
        written — **done in Batch D below**, don't trust this line alone
  - [x] unimplemented `policy_type` (e.g. `SPEND_CONTROL`) → `501`, not a
        silent no-op or a crash
- [x] idempotency: **stale as originally written here — corrected by
      Batch D below.** At the time this batch shipped, "no write path
      exists anywhere in this batch" was true. As of Batch D, `Evaluate`
      does write (best-effort) to `governance-decision-log-svc` on every
      real evaluation; that write is idempotent only when the caller
      supplies `decision_id`. See Batch D's section for the full story.
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
  them as consumed events.
  - **`entity.created` — status changed since this was first written.**
    `tenant-entity-registry-svc` now has a real Kafka producer for it
    (confirmed after pulling `origin/main` — see "Synced with
    `origin/main`" note above). Consuming it is technically unblocked,
    but nothing in the docs specifies what policy-svc should *do* with
    an `entity.created` event — building a consumer with no defined
    business behavior would be infrastructure with no purpose. **Needs
    an answer from you** (e.g. "validate `legal_entity_id` references,"
    "invalidate a future cache entry," or "nothing, skip it") before
    this is buildable, not more unilateral engineering.
  - **`role.updated` / `authority.delegated` — still fully blocked.**
    Access Control Service and Delegated Authority Service don't exist
    at all yet. No amount of decision-making unblocks this; it needs
    those services to exist first.
- **Do not wire calls to Authorization Service** for admin writes
  (create/activate) — it doesn't exist yet. Same posture
  `governance-decision-log-svc` shipped with; revisit when Authorization
  Service exists.
- **Do not add caching/Redis/sidecar evaluation** in v1 — explicitly
  deferred (Batch B). Technically buildable now (no external blocker),
  but the spec explicitly allows deferring it ("may be cached... not
  required"), unlike the evidence obligation, which the spec does not
  allow deferring and which Batches D/E closed. Needs a "yes, build it"
  from you, not an assumption that it's wanted.
- **Do not build evaluation logic for `SPEND_CONTROL`, `SOD_RULE`, or
  `SIGNATORY_MATRIX`** — only `APPROVAL_THRESHOLD` gets real logic in v1;
  the others are future `switch` cases, not part of this build. **Cannot
  be built without you supplying the actual business rules** — the docs
  name these three policy types but give zero formulas or logic for any
  of them, unlike `APPROVAL_THRESHOLD`'s explicit "compare against a
  threshold" instruction.

## Blocking cross-service dependencies (tracked, not yet resolvable)

- **Authorization Service** — doesn't exist; deferred per non-goals
  above rather than blocking.
- **Access Control Service** / **Delegated Authority Service** — don't
  exist; block real consumption of `role.updated` / `authority.delegated`
  (deferred per non-goals above) — genuinely blocking, not a decision
  policy-svc can make on its own.
- **Kafka event backbone — status changed since this was first written.**
  It's real now for `identity-context-svc` and `tenant-entity-registry-svc`
  (real `kafka.Writer` producers, confirmed after the `origin/main` pull).
  Policy Service's own publisher (`internal/events/publisher.go`) is
  still a log-only stub — that part hasn't changed and isn't blocked by
  anything except priority; wiring a real `kafka.Writer` there would
  follow the same pattern those two services now use.

## Remaining gaps against a strict reading of `03-microservices.md` §8.1

As of Batch E, this is the complete, current list — nothing here has
regressed and nothing here has been quietly fixed since the last review
(see `context.md` §19/§20 for what *was* closed):

1. Consuming `entity.created` — needs you to specify intended behavior (see above)
2. Consuming `role.updated`/`authority.delegated` — blocked on services that don't exist
3. Caching — needs a "yes, build it" from you (not required by spec)
4. Evaluation logic for `SPEND_CONTROL`/`SOD_RULE`/`SIGNATORY_MATRIX` — needs business rules from you
5. `policies` table has no `tenant_id`/`legal_entity_id` — needs a decision on whether to reverse the Batch A design precedent (mirrors `jurisdictions`)
6. No literal separate "validate threshold applicability" endpoint (folded into `Evaluate`) — low value to fix, recommend leaving as-is unless you disagree
