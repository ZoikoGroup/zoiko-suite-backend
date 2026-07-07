# Governance Decision Log Service — Agent Context

Read this file before writing any code for this service. It is the
self-contained brief: purpose, contract, doctrine constraints, and open
questions. Source-of-truth docs are linked throughout — if this file and
a linked doc ever disagree, the doc wins and this file should be corrected.

## Ownership

Owned by the `@governance` domain cell (`.agents/agents.md`). That cell
owns Policy, Authorization, Workflow & Approvals, Obligations, and this
service — and never writes business-domain code.

**Model policy** (`.agents/rules/model-policy.md`): this is a Governance
Plane service → Claude Opus/Sonnet only. No Gemini draft pass.

## Service class & tier

Governance Platform Service, **Tier 0 — Platform Survival**
(`docs/architecture/03-microservices.md` §6, §23). Must exist before
broad functional expansion — it is one of the Phase 1 "Sovereign Spine"
services (`docs/architecture/06-blueprint.md` §07 Phase 1).

## Purpose

Captures every governance evaluation as immutable evidence. Doctrine,
verbatim (`docs/architecture/01-backend.md` §07):

> Every governance decision is itself evidence. This is a non-negotiable
> architectural rule.

This service does not make governance decisions — Policy, Authorization,
and Workflow & Approvals do that. This service is the durable,
queryable record of decisions already made.

## Owned data — `GovernanceDecision`

Exact fields, from `docs/architecture/04-data-model.md` §7.1:

- `governance_decision_id`
- `tenant_id`
- `legal_entity_id`
- `principal_id`
- `action_type`
- `action_subject_type`
- `action_subject_id`
- `policy_version_id` (nullable)
- `jurisdiction_rule_basis`
- `authorization_outcome`
- `workflow_instance_id` (nullable)
- `correlation_id`
- `decision_timestamp`

Relations (`04-data-model.md` §7.2 ERD): `GovernanceDecision` →
`Tenant`, `LegalEntity`, `Principal`, `PolicyVersion`, `WorkflowInstance`.

Modeling rule (§7.3): **governance decisions must store basis, not just
outcome.** A record that only says "denied" without the policy version,
jurisdiction rule basis, and correlation id is not compliant.

### FINALIZED — MVP schema (2026-07-06)

The actual build brief simplifies the canonical entity above. This is a
deliberate MVP decision, not an oversight — record it here so nobody
"fixes" it back to the full entity without noticing it was intentional.

Table columns (single append-only table, no UPDATE/DELETE ever):

| Column | Notes |
| --- | --- |
| `decision_id` | caller-supplied, the dedup key (maps to `governance_decision_id`) |
| `tenant_id` | required |
| `legal_entity_id` | required |
| `actor_id` | maps to `principal_id` |
| `action_type` | required |
| `outcome` | maps to `authorization_outcome` |
| `rule_basis` | maps to `jurisdiction_rule_basis` |
| `evaluation_context` | JSONB catch-all — use this to carry `policy_version_id`, `action_subject_type`, `action_subject_id`, `workflow_instance_id` if/when a caller has them, rather than adding columns now |
| `correlation_id` | required |
| `decided_at` | maps to `decision_timestamp` |

If a real need for first-class `policy_version_id` / `workflow_instance_id`
columns (e.g. to filter/join on them efficiently) shows up later, promote
them out of `evaluation_context` in a follow-up migration — don't block
MVP on it now.

## API surface

**Inbound**
- `POST /v1/decisions` — record a governance decision. Callers: Policy
  Service, Authorization Service, Workflow & Approvals Service, after
  each evaluation.
- `GET /v1/decisions/{id}` — retrieve one decision.
- `GET /v1/decisions?actor=&entity=&action=&rule_basis=&from=&to=` —
  must be queryable by actor, entity, action, rule basis, and time range
  (explicit critical constraint, `03-microservices.md` §8.7).

**Outbound / published events**
- `governance.decision.recorded`

**Consumed events**: none expected. This service is a sink — Policy /
Authorization / Workflow call it directly (sync or via event), it does
not subscribe back to them.

**Governance dependencies**: none upstream. This service sits beneath
Policy/Authorization/Workflow, not behind them — it must never itself
require a governance check to accept a write (that would be circular
and could deadlock the evidence path).

## Evidence obligations

Append-only, immutable. No update, no delete. Must remain queryable
indefinitely (or per an explicit retention policy, if one is ever
defined) for audit, regulator, and legal-discovery scenarios.

## Idempotency

FINALIZED: dedup key is `decision_id` (caller-supplied). Mirror
`services/audit-event-store-svc/internal/store/store.go`'s `Store` method
exactly: `INSERT ... ON CONFLICT (decision_id) DO NOTHING`, never
SELECT-then-INSERT (TOCTOU race under concurrent delivery — see that
file's doc comment for why).

## Event publishing — FINALIZED

After a successful `POST /v1/decisions` write, publish
`governance.decision.recorded`.

Mirror the existing publisher convention exactly — see
`services/identity-context-svc/internal/events/publisher.go` and
`services/tenant-entity-registry-svc/internal/events/publisher.go`. Both:
- wrap payloads in the same `envelope` struct shape (`EventType`,
  `EmittedAt`, `SchemaVersion`, `SourceService`, `CorrelationID`, `Payload`)
- have a `// producer *kafka.Writer — TODO: inject kafka.Writer before
  Phase 1 exit criteria` comment on the `Publisher` struct
- log the fully-marshaled envelope at `Info` level instead of writing to
  Kafka (`p.log.Info("event emitted (stub — wire Kafka writer)", ...)`)

**Important nuance**: neither existing envelope struct hoists
`tenant_id`/`legal_entity_id`/jurisdiction context to the top level —
those live inside the `payload` map, and neither existing publisher
emits jurisdiction context at all today. Don't invent new top-level
envelope fields to satisfy `03-microservices.md` §19 (event name,
version, timestamp, tenant ID, legal entity ID, jurisdiction context,
actor ID, correlation ID, source service, payload schema version) —
instead make sure the `payload` map for `governance.decision.recorded`
includes `tenant_id`, `legal_entity_id`, `actor_id` (from `actor_id`
column), and jurisdiction context (populate from `rule_basis` — it's
the closest thing this schema has to a jurisdiction reference). Schema
version goes in the envelope's existing `SchemaVersion` field, same as
today's services.

## Doctrine constraints that apply here

From `.agents/rules/doctrine.md` (always-on):
- No soft-delete — not applicable here in the usual sense since this
  object is never mutated at all after creation, but worth stating
  explicitly in the schema/migration comments.
- Every material record carries `tenant_id` + `legal_entity_id` — already
  satisfied by the `GovernanceDecision` field list above.
- Events are facts, not commands. Append-only.
- No hardcoded jurisdiction/tax/currency values — N/A for this service;
  it stores `jurisdiction_rule_basis` as an opaque reference, it does not
  interpret jurisdiction logic itself.

## Open design question — deferred, not this service's problem to solve

Doctrine says governance services "fail closed" (`03-microservices.md`
§22 Failure Mode Expectations), and evidence capture is stated as
non-negotiable. But Authorization Service is explicitly "extremely high
call volume, latency-sensitive" (`03-microservices.md` §8.3). If this
service is unreachable when Authorization Service tries to record a
decision, two options exist:

1. **Fail closed** — the action being authorized is denied/blocked until
   the decision can be durably logged.
2. **Fail safe via durable async write** — Authorization proceeds
   immediately via a local outbox/queue that guarantees eventual delivery
   here.

**Resolution for this service's build (2026-07-06): out of scope here.**
This is a decision for whoever calls this service (Authorization Service,
Policy Service, Workflow & Approvals Service), not for the Decision Log
Service itself. This service's contract is simple and doesn't change
either way: accept an idempotent `POST /v1/decisions`, store it durably,
make it queryable. Revisit this note when Authorization Service's actual
calling code is built — that's where the fail-closed-vs-async choice gets
made.

## Config — FINALIZED

Env vars (mirror `services/jurisdiction-rules-svc/internal/config/config.go`
exactly — same `env()`/`envInt()` helpers, same `DBConfig.DSN()` shape):

- `PORT` (default `8083` — confirmed free: `identity-context-svc`=8080,
  `tenant-entity-registry-svc`=8081, `jurisdiction-rules-svc`=8082,
  `audit-event-store-svc` has no HTTP server yet, so 8083 is next in
  sequence and unused)
- `DB_HOST`, `DB_PORT` (5432), `DB_NAME` (default `governance_decision_log`),
  `DB_USER`, `DB_PASSWORD`, `DB_SSLMODE`
- `ENV` (default `local`)

pgxpool settings: same Tier 0 sizing as the other three services —
`MaxConns=20, MinConns=2, MaxConnLifetime=30m, MaxConnIdleTime=5m,
HealthCheckPeriod=1m` — plus a fail-fast `Ping()` at startup (see both
`main.go` files under "reference files" below).

## What does NOT exist yet

No code for this service exists in the repo as of this writing. No
migration, no `go.mod`, no `cmd/`. Nothing in `.github/workflows/ci.yml`
references it. When scaffolding begins, follow the same layered pattern
used by the three existing Tier 0 services (`identity-context-svc`,
`tenant-entity-registry-svc`, `jurisdiction-rules-svc`):

```
cmd/server/main.go   — wiring
internal/handler/    — HTTP
internal/<domain>/   — orchestration / service layer
internal/store/      — Postgres persistence (pgx/v5, no ORM)
internal/domain/     — canonical types matching 04-data-model.md verbatim
internal/config/     — env-driven config, no hardcoded secrets
internal/health/     — liveness/readiness probes
```

Language: Go, consistent with the other Tier 0 services.

Per `.agents/skills/service-spec/SKILL.md`: this CONTEXT.md file *is*
the required spec block.

## Reference files — read these before writing the equivalent file here

| What | Mirror this file |
| --- | --- |
| `cmd/server/main.go` wiring order | `services/jurisdiction-rules-svc/cmd/server/main.go` and `services/tenant-entity-registry-svc/cmd/server/main.go` |
| `internal/config/config.go` | `services/jurisdiction-rules-svc/internal/config/config.go` |
| `internal/events/publisher.go` | `services/identity-context-svc/internal/events/publisher.go` and `services/tenant-entity-registry-svc/internal/events/publisher.go` (see "Event publishing — FINALIZED" above) |
| `internal/handler/*_test.go` (mocked-store unit tests) | `services/jurisdiction-rules-svc/internal/handler/handler_test.go` — note the explicit 404-vs-503 distinction tests, that pattern applies here too (not-found vs store-unavailable must never share a status code) |
| `internal/store/pg_store_test.go` (env-guarded integration tests) | `services/identity-context-svc/internal/store/pg_store_test.go` and `services/jurisdiction-rules-svc/internal/store/pg_store_test.go` — `TEST_DATABASE_URL` env-guard, drop+reapply migration in `openTestPool`, insert real rows, assert against real SQL |
| append-only idempotent insert | `services/audit-event-store-svc/internal/store/store.go`'s `Store` method — `INSERT ... ON CONFLICT (decision_id) DO NOTHING`, exact pattern to copy |
| append-only evidence-in-a-transaction pattern (if ever needed) | `services/identity-context-svc/internal/store/pg_store.go`'s `UpdateStatus` — not directly needed here since this service has no UPDATE, but useful if a future write needs both a state change and a decision-log insert atomically |

## Build plan — three phases, three branches, three PRs into main

Each phase branches off `main` (not off the previous phase's branch) —
i.e. phase N+1 doesn't start until phase N has merged into `main`, so
`main` always reflects what phase N+1 builds on. See PROGRESS.md for
live status.

**Phase 1 — write path** (new service scaffold + `POST /v1/decisions`)
- Full service scaffold per the layered pattern above
- Migration: single append-only table per "FINALIZED — MVP schema"
- `POST /v1/decisions`, idempotent, real `PgStore` wired into `main.go`
  (a `FakeStore` for unit tests is fine, but production wiring must be
  real Postgres — no stub as the primary path, unlike how
  `tenant-entity-registry-svc` currently stubs its authz/jurisdiction
  clients)
- Verify: `go build` passing is not sufficient — boot the service
  against a real Postgres container, POST a decision, confirm the row
  exists, POST the same `decision_id` again, confirm no duplicate row.

**Phase 2 — query surface** (on top of phase 1, once merged)
- `GET /v1/decisions/{id}` — 404 if not found
- `GET /v1/decisions?actor=&entity=&action=&rule_basis=&from=&to=` — all
  five filters, and they must compose (e.g. actor + time range together)
- Handler unit tests (mocked store) + real Postgres integration tests
  proving all five filters work individually and in combination
- Verify against a real Postgres container, not just mocks

**Phase 3 — close the loop** (on top of phase 1 + 2, once both merged)
- Publish `governance.decision.recorded` after a successful POST (see
  "Event publishing — FINALIZED" above)
- Add `governance-decision-log-svc` to `.github/workflows/ci.yml`'s
  `matrix.service` list AND to the `TEST_DATABASE_URL` conditional
  (currently only `jurisdiction-rules-svc` / `identity-context-svc`)
- Dockerfile: re-check `services/audit-event-store-svc/` first — if a
  teammate has added one by then, mirror its structure; as of
  2026-07-06 it does not exist yet, so absent that, write a standard
  multi-stage Go Dockerfile (build stage matching this service's
  `go.mod` Go version, non-root runtime user, minimal base image e.g.
  `gcr.io/distroless/static` or `alpine`)
- Add an entry to `services/README.md` (currently just a one-line
  `# zoiko-suite` header with no service list — check again before
  editing in case that's changed)
- Verify: build the Docker image, run the container against a real
  Postgres, confirm `/healthz` responds and a real POST + GET
  round-trip works from inside the container
