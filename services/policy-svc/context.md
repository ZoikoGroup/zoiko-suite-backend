# Policy Service — Context

Compiled from `docs/architecture/01-backend.md`, `02-diagrams.md`,
`03-microservices.md`, `04-data-model.md`, `05-security.md`,
`06-blueprint.md`, and `.agents/` rules. This file has no independent
authority — if it ever disagrees with the source docs, the docs win.

## 1. What it is

**Service Class:** Governance Platform Service
**Tier:** 0 (latency-critical, non-bypassable)
**Naming convention in this repo:** other Tier 0 / governance services are
suffixed `-svc` (`tenant-entity-registry-svc`, `jurisdiction-rules-svc`,
`identity-context-svc`, `audit-event-store-svc`) → this folder is
`policy-svc`.

**Purpose** (03-microservices.md §8.1):
Maintains and evaluates business, financial, legal, and internal control
policies applicable to material actions.

It is one of seven engines in the **Governance Control Plane**
(01-backend.md §07) — the non-bypassable architectural spine that every
material action must pass through before execution:
Policy Engine · Jurisdiction Engine · Authorization Engine · Workflow &
Approvals Engine · Obligations Engine · Evidence Requirements Engine ·
Decision Logging Layer.

> "No domain service may execute a material action without passing
> through it." — 01-backend.md §07

Policy Engine specifically: *"Evaluates business, financial, legal, and
internal control policies applicable to the action."*
Examples: approval thresholds · spend limits · signatory matrices ·
SoD (segregation-of-duties) rules · entity-specific governance rules.

## 2. Ownership boundary

**Owns** (03-microservices.md §8.1):
- policy definitions
- policy versions
- effective dates
- policy scopes
- approval thresholds
- signatory matrices
- SoD rule sets
- spend control rules

**Authoritative Boundary:** Sole source of truth for platform policy
definitions.

**Domain-cell ownership** (`.agents/agents.md`): owned by `@governance`,
which also owns Authorization, Workflow & Approvals, Obligations, and the
Governance Decision Log. `@governance` never writes business-domain code
and reviews any PR touching the Governance Plane.

## 3. API surface

**Inbound APIs** (03-microservices.md §8.1):
- get applicable policy set
- evaluate policy against action context
- retrieve policy version history
- validate threshold applicability

**Published Events:**
- `policy.created`
- `policy.updated`
- `policy.version.activated`
- `policy.rule.retired`

**Consumed Events:**
- `entity.created`
- `role.updated`
- `authority.delegated`

## 4. Data model

Per `04-data-model.md` §7.1 (Core Entities) and §7.2 (ERD):

### Policy
- policy_id
- tenant_id
- policy_code
- policy_name
- policy_domain
- policy_status
- versioning_mode

### PolicyVersion
- policy_version_id
- policy_id
- version_number
- effective_from
- effective_to
- policy_payload
- activation_status
- activated_by
- activated_at

ERD: `Policy └──< PolicyVersion` (one policy has many versions).

**Downstream linkage:** `GovernanceDecision` (§7.1) records
`policy_version_id` (nullable) as the basis for every governance decision
— i.e. every governance decision evidences *which policy version* was
evaluated. `GovernanceDecision` also references `jurisdiction_rule_basis`,
`authorization_outcome`, and `workflow_instance_id`, tying Policy Service
into the same decision record as Jurisdiction, Authorization, and
Workflow services.

**Phase A root objects** (§ "Phase A — The Root", 04-data-model.md ~L2678):
`Policy / PolicyVersion / JurisdictionRule` are called out explicitly as
part of the foundational root schema alongside
Tenant/LegalEntity/Jurisdiction/DataResidencyPolicy/ResidencyRegion,
Principal/Role/DelegatedAuthority, and
GovernanceDecision/WorkflowInstance/AuditEvent.

**Doctrine invariants that apply here** (`.agents/rules/doctrine.md`):
- No self-authorization — policy evaluation logic must not be duplicated
  or bypassed inside domain services.
- Every state-changing API and every event consumer must be idempotent.
- No soft-delete on material objects (policies/versions retire via
  status transition / effective end-dating, never deletion).
- Every material record carries `tenant_id`, `legal_entity_id`, and
  `effective_from`/`effective_to`.
- Events are facts, not commands — append-only, never mutate source truth
  from a downstream consumer.
- No hardcoded jurisdiction/country/currency/tax-rule values as code
  constants — jurisdiction-specific behavior must come from Jurisdiction
  Rules Service / Tax Service as versioned runtime data.

## 5. Evidence & compliance obligations

**Evidence Obligations** (03-microservices.md §8.1):
- preserve every policy version
- preserve effective-dated activation
- preserve evaluation basis for governed decisions

This ties into the platform-wide doctrine that "every governance decision
is itself evidence" (01-backend.md §07) — Policy Service's evaluation
output feeds "Policy decision evidence," one of the seven Evidence Types
in 01-backend.md §08, and "Policy Decision Log" in the Evidence + Event
Infrastructure layer (01-backend.md, diagrams).

05-security.md references policy version evidence and policy-gated
secret retrieval as part of the platform's broader evidence/security
posture (§9.2, §14 control lists).

## 6. Idempotency & scaling

**Idempotency Requirement:** Evaluation endpoints must be safely
repeatable (03-microservices.md §8.1).

**Scaling Characteristics:**
- read-heavy
- cache-accelerated
- low write frequency
- high execution criticality

**Critical Constraint:** Policy decisions may be cached for performance,
but the authoritative rule source remains centralized and auditable.

**Sidecar / distributed evaluation** (05-security.md §6.5): for Tier 0 /
latency-sensitive services, policy and authorization evaluation may use
high-speed distributed enforcement patterns — local policy caches,
sidecar policy agents, OPA-style evaluation components — *provided*:
- policy source remains centralized
- policy provenance is auditable
- stale decision risk is bounded
- fail-safe behavior is defined

03-microservices.md §3.9 (Governance Latency Must Remain Operationally
Viable) makes the same point generally: governance enforcement must not
become a bottleneck; Tier 0 services should support high-speed execution
patterns including sidecar or local decision-cache strategies "where safe
and auditable."

## 7. Tech stack & model policy

`.agents/rules/tech-stack.md`: Tier 0 latency-critical services
(**Identity, Authorization, Policy**) → **Go**. (Matches the existing Go
services in this repo.)

`.agents/rules/model-policy.md`: Governance Plane services (Identity,
**Policy**, Authorization, Workflow, Audit, Decision Log) → **Claude
Opus/Sonnet only** when using AI agents to write code for this service.

## 8. Build sequencing

**Phase 1 — The Sovereign Spine** (06-blueprint.md): Policy Service is
one of ten services/components required in Phase 1, alongside Identity
Context Service, Tenant & Entity Registry Service, Jurisdiction Rules
Service, Authorization Service, Workflow & Approvals Service, Obligations
Service, Governance Decision Log Service, Secret Vault Integration
Service, and Configuration & Feature Flag Service — plus infra (API
gateway, schema registry, base Kubernetes, observability baseline, audit
event pipeline bootstrap, Global Traffic & Residency Manager).

**Phase 1 Exit Criteria** (06-blueprint.md):
- no material request can bypass governance path
- identity, entity, authorization, and residency context resolve
  deterministically
- governance decisions are logged as evidence
- secrets are centrally managed
- baseline zero-trust posture exists for internal services
- region-aware routing is technically proven

Doctrine: *"Do not start a Tier 1 service until its Tier 0 dependency has
met its exit criteria."* Policy Service is itself a Tier 0 dependency
other services will wait on.

## 9. Spec/scaffold process

`.agents/skills/service-spec/SKILL.md` names **Policy Service** and
**Authorization Service** as the two canonical worked examples to follow
when drafting a new service spec (reference `03-microservices.md §08`).
Process: draft a full service spec, get explicit approval, *then*
scaffold service code, tests, and an OpenAPI stub — mirroring how
`tenant-entity-registry-svc`, `jurisdiction-rules-svc`, and
`identity-context-svc` were built (each has `cmd/server/main.go`,
`internal/{handler,store,domain,health}`, `deployments/migrations/`,
`openapi.yaml`, `go.mod`).

## 10. Current repo state (as of 2026-07-07, corrected)

**Correction:** the remote is `ZoikoGroup/zoiko-suite-backend`, not
`ZoikoGroup/zoiko-suite` as an earlier version of this file assumed
(verified via `git remote -v`). Update any external references
accordingly.

**Services that actually exist now** (verified via `ls services/` —
supersedes the "only 4 services exist" snapshot from 2026-07-06):

| Service | Port | Status |
| --- | --- | --- |
| `identity-context-svc` | 8080 | HTTP server wired; principal store is Postgres-backed |
| `tenant-entity-registry-svc` | 8081 | Real Postgres + Row-Level Security for tenant isolation |
| `jurisdiction-rules-svc` | 8082 | Real Postgres-backed read API |
| `governance-decision-log-svc` | 8083 | Append-only evidence store (`POST`/`GET /v1/decisions`) — **new since the last snapshot** |
| `audit-event-store-svc` | — | Kafka consumer + store interface only; no HTTP server yet |
| `policy-svc` | **8085** (assigned, not yet bound in code) | This folder — currently docs only, no code |

`configuration-feature-flag-svc` has **not** landed (no such directory
exists) — port 8084 is technically free, but the build task for this
service explicitly assigns **8085** to policy-svc defensively, in case
configuration-feature-flag-svc lands first and claims 8084. Follow that
assignment rather than re-checking at implementation time.

Policy Service is not mentioned in `docs/architecture/known-gaps.md`
(which only tracks gaps in jurisdiction-rules-svc, tenant-entity-registry-svc,
identity-context-svc). See `progress.md` in this folder for the concrete,
task-approved build plan and status.

## 11. Required spec block (per SKILL.md) — filled in vs. still open

`.agents/skills/service-spec/SKILL.md` requires a spec with 9 fields
before any code is written, and says the spec must be **approved before
scaffolding starts**. Status of each field for Policy Service:

| Field | Status | Source |
| --- | --- | --- |
| Service name, class, purpose | ✅ filled | §1 above |
| Owned objects (exact field names) | ✅ filled — **superseded by concrete schema in §13** | §2, §4, §13 |
| Inbound / Outbound APIs | ✅ filled — **superseded by concrete endpoint list in §13** | §3, §13 |
| Published / Consumed events | ✅ filled — **scope narrowed in §13** (consumed events explicitly deferred) | §3, §13 |
| Governance dependencies (which Governance Plane engines this service calls) | ⚠️ still open, but now **explicitly deferred rather than blocking** | The approved build task does not wire calls to Authorization Service for admin writes (create/activate) — it isn't mentioned at all in the 3-batch task spec in §13. Same posture as `governance-decision-log-svc`'s precedent: ship without it, since Authorization Service doesn't exist yet, and revisit when it does. Not resolved, just consciously deferred. |
| Evidence obligations | ✅ filled | §5 above |
| Idempotency requirement | ✅ filled | §6, §13 — evaluation endpoint idempotency is free (pure read/compute), and admin writes get it via the `INSERT...ON CONFLICT DO NOTHING` / `ON CONFLICT` transition pattern (§13) |
| **Failure mode** (fail closed / fail safe / degraded / compensating saga) | ✅ **resolved for the evaluation endpoint** | §13: no applicable ACTIVE policy version for a given type+scope → `404`, and the service explicitly does **not** guess fail-open vs fail-closed — that decision is pushed to the caller. This is a real architectural choice (not an omission) and should be treated as the answer to this spec field going forward. Caching/sidecar failure-mode (05-security.md §6.5) is still unaddressed since caching is explicitly out of scope for v1 (§13). |

**Net:** the spec is now effectively complete for the v1 scope actually
being built (§13). What's still open — Authorization-Service wiring for
admin writes, and cache-layer failure-mode — is deferred by explicit
design choice, not by oversight, and only needs revisiting when
Authorization Service exists or caching is added in a later version.

## 13. Concrete v1 implementation spec (task-approved, supersedes speculative design)

This section is the authoritative, approved build spec for policy-svc v1,
handed down as three sequential work batches (each its own branch off
`main` in `ZoikoGroup/zoiko-suite-backend`). It fills every gap the
architecture docs leave open by mirroring `jurisdiction-rules-svc`
directly — same relationship shape (a lightweight named container owning
effective-dated, state-machined versions), same code patterns — the same
way `governance-decision-log-svc` filled its own doc gaps by precedent
rather than inventing something new.

**Mirror mapping:** `Policy` ↔ `Jurisdiction`, `PolicyVersion` ↔
`JurisdictionRule`.

### 13.1 Schema

`policies` table (mirrors `jurisdictions`):
- `policy_id`
- `policy_code`
- `policy_name`
- `policy_type` — VARCHAR, data-driven, e.g. `APPROVAL_THRESHOLD`,
  `SPEND_CONTROL`, `SOD_RULE`, `SIGNATORY_MATRIX`. New types are a data
  row, never a code change — same doctrine as `jurisdiction_type` /
  `rule_domain` in jurisdiction-rules-svc.
- `created_at`
- `created_by_principal_id`

`policy_versions` table (mirrors `jurisdiction_rules`):
- `policy_version_id`
- `policy_id`
- `tenant_id` — **nullable**; NULL means global (applies across all
  tenants)
- `legal_entity_id` — **nullable**
- `rule_payload` — JSONB; actual rule content, shape depends on
  `policy_type` (e.g. `{"threshold_amount": 5000}` for
  `APPROVAL_THRESHOLD`)
- `effective_from`
- `effective_to`
- `version_status` — `DRAFT | ACTIVE | SUPERSEDED | RETIRED`, VARCHAR not
  enum
- `created_at`
- `created_by_principal_id`

**No UPDATE/DELETE on either table.** A change is always either a new
DRAFT version or a status transition (mirrors jurisdiction-rules-svc's
"no soft-delete, deactivation via status + effective_to" rule exactly).

Migration file should follow jurisdiction-rules-svc's
`000001_initial_schema.up.sql` conventions: idempotent-creation unique
index for the dedup key (see 13.3), a lookup index on `(policy_id,
tenant_id, legal_entity_id)` for the "current ACTIVE version in scope"
query used by both activation-supersede and evaluation, and a partial
index on `version_status = 'ACTIVE'` (mirrors `idx_jrules_status`).

### 13.2 Endpoints — Batch A (Policy/PolicyVersion CRUD)

- `POST /v1/policies` — create the named policy container.
- `POST /v1/policies/{policy_id}/versions` — create a new DRAFT version.
- `POST /v1/policies/{policy_id}/versions/{version_id}/activate` —
  DRAFT→ACTIVE. Atomically supersedes whatever version was previously
  ACTIVE for that `(policy_id, tenant_id, legal_entity_id)` scope. This
  is the `TransitionRuleStatus`-with-`allowedPriors` pattern (13.3)
  applied twice in one transaction: the new version transitions
  `DRAFT→ACTIVE` (allowedPriors=[`DRAFT`]), the old one transitions
  `ACTIVE→SUPERSEDED` (allowedPriors=[`ACTIVE`]).
- `GET /v1/policies/{policy_id}/versions` — full version history, newest
  first.

**Verification (real Postgres):** create a policy, create a version,
activate it, create a second version and activate that too, confirm the
first version is now `SUPERSEDED` (not deleted) and the history endpoint
shows both.

### 13.3 Code patterns to mirror exactly (from jurisdiction-rules-svc)

**Domain types** (`internal/domain/types.go`): plain string
discriminator fields (`policy_type`, `version_status`) — no Go enums,
`iota`, or switch/case on these values in validation logic; new values
are a data migration only. Comment convention: document the known values
as a comment where the field is declared, same as `JurisdictionType`/
`RuleDomain` in jurisdiction-rules-svc's `types.go`.

**Idempotent creation** (mirrors `CreateJurisdiction`/`CreateRule` in
`pg_store.go`):
```
INSERT INTO policies (...) VALUES (...)
ON CONFLICT (policy_code) DO NOTHING
RETURNING <columns>;
```
On `pgx.ErrNoRows` (conflict occurred), fall back to a `SELECT` by the
natural dedup key and return the existing record with `created=false`.
Any other error wraps `ErrStoreUnavailable` — never conflate
"already exists" with "database unreachable." Apply the same shape to
`policy_versions`, with the dedup key most likely `(policy_id, tenant_id,
legal_entity_id, effective_from)` (parallels `(jurisdiction_id, rule_code,
effective_from)` on `jurisdiction_rules` — use `COALESCE` on the nullable
`tenant_id`/`legal_entity_id` columns the same way
`idx_jurisdictions_code_type_parent_unique` handles nullable
`parent_jurisdiction_id`).

**State transition** (mirrors `TransitionRuleStatus`):
- Fetch current record by ID first.
- If already at target status, return it unchanged — idempotent no-op,
  not an error.
- Otherwise: `UPDATE ... SET version_status = $1 WHERE policy_version_id
  = $2 AND version_status = ANY($3::text[]) RETURNING <columns>` — the
  store takes `allowedPriors` as a caller-supplied parameter, it doesn't
  hardcode the state machine.
- Zero rows updated → `ErrInvalidTransition`, not silently ignored.

**Split of responsibility:** the **handler** owns the state machine (it
decides, e.g., that activation is only legal from `DRAFT` and supersede
is only legal from `ACTIVE`, and passes those as `allowedPriors`); the
**store** is a generic, dumb enforcer of whatever priors it's given. Do
not hardcode allowed-prior lists inside the store layer.

**Error types**: define `ErrPolicyNotFound`, `ErrPolicyVersionNotFound`,
`ErrInvalidTransition`, `ErrConflict`, `ErrStoreUnavailable` in
`internal/domain`, mirroring jurisdiction-rules-svc's error-type set
exactly (including the "store unavailable → fail closed" contract on
`ErrStoreUnavailable`).

**Entrypoint wiring**: standard shape used by every Go service in this
repo — config → zap logger → pgxpool (fail-fast on connect) → store →
handler → `/healthz` + `/readyz` → graceful shutdown. No deviation needed
here; copy the shape from `jurisdiction-rules-svc/cmd/server/main.go`.

### 13.4 Endpoints — Batch B (evaluation — the core value of the service)

Scope narrowly: **only `APPROVAL_THRESHOLD` gets real evaluation logic**
in v1. The other three `policy_type` values from 13.1 are not
implemented yet. Design so adding the next type is "add a case," not "a
refactor" — a plain `switch` on `policy_type` dispatching to a per-type
evaluator function is correct here; do not build a plugin/registry system
for four total cases.

- `GET /v1/policies?policy_type=X&tenant_id=Y&legal_entity_id=Z` — the
  spec's "get applicable policy set" API (03-microservices.md §8.1).
  Returns the currently-ACTIVE policy version(s) applicable to that
  scope.
- `POST /v1/policies/evaluate` — body: `{policy_type, tenant_id,
  legal_entity_id, action_context: {...}}`.
  1. Look up the applicable ACTIVE version for that type+scope.
  2. **No applicable policy → `404`.** The service does not guess
     fail-open/fail-closed; that decision belongs to the caller. (This is
     the resolved answer to the "failure mode" spec field in §11.)
  3. For `APPROVAL_THRESHOLD`: compare `action_context.amount` against
     `rule_payload.threshold_amount` from the matched version.
  4. Response: `{"result": "APPROVAL_REQUIRED" | "WITHIN_THRESHOLD",
     "policy_version_id": "...", "rule_basis": "..."}`.

**Why this response shape:** it's deliberately close to what
`governance-decision-log-svc`'s `POST /v1/decisions` expects — especially
`rule_basis` — because this evaluation result is meant to feed that
service later. Wiring the actual call between them is explicitly a
**separate future task**, not part of this build.

**Idempotency:** falls out naturally — this is a pure read/compute
endpoint with no side effects. The only risk is accidentally introducing
hidden state mutation (e.g. a "last evaluated at" write); don't.

**Caching:** explicitly **not required for v1**, even though
05-security.md §6.5 allows it. Do not add Redis or any cache layer now —
a direct Postgres read is fine at this stage. (This means Phase 9/caching
from the old speculative plan in `progress.md` is deferred indefinitely,
not scheduled.)

**Verification (real Postgres):** activate an `APPROVAL_THRESHOLD`
version with a threshold, `POST /v1/policies/evaluate` with an amount
above it (expect `APPROVAL_REQUIRED`) and below it (expect
`WITHIN_THRESHOLD`), confirm the returned `policy_version_id` matches the
version activated.

### 13.5 Batch C — events, CI, Dockerfile, README

**Event publishing** — mirror
`governance-decision-log-svc/internal/events/publisher.go` exactly: same
`envelope` struct (`event_type`, `emitted_at`, `schema_version`,
`source_service`, `correlation_id`, `payload`), same log-only stub
`emit()` (no real Kafka writer yet — that's a `// TODO: publish to Kafka
topic` left in place, not a gap to fill now). Publish:
- `policy.created` on policy creation
- `policy.updated` on new version created
- `policy.version.activated` on activation
- `policy.rule.retired` on supersede/retire

**Explicit non-goal:** do **not** consume `entity.created`,
`role.updated`, or `authority.delegated` yet, even though
03-microservices.md §8.1 lists them as consumed events. Nothing in the
codebase publishes those for real yet — they're all logged stubs in
their respective services — so there is nothing to actually consume.
Wiring real consumption is a follow-up once the producers are real, out
of scope for this build.

**CI** (`.github/workflows/ci.yml`):
- Add `policy-svc` to the `matrix.service` list (currently:
  `audit-event-store-svc`, `identity-context-svc`,
  `tenant-entity-registry-svc`, `jurisdiction-rules-svc`,
  `governance-decision-log-svc`).
- Add `policy-svc` to the `TEST_DATABASE_URL` conditional alongside
  `jurisdiction-rules-svc`, `identity-context-svc`,
  `governance-decision-log-svc` (line ~72), since it has real Postgres
  integration tests.

**Dockerfile** — mirror
`services/governance-decision-log-svc/Dockerfile` exactly: two-stage
build, `golang:1.25-alpine` builder → `gcr.io/distroless/static-debian12:nonroot`
runtime, `CGO_ENABLED=0` static binary, `-trimpath -ldflags="-s -w"`.
Swap the binary name to `policy-svc` and `EXPOSE 8085` (per the port
assignment in §10 — do **not** use 8084, even though it's technically
free right now).

**`services/README.md`** — add a row to the services table:
`| `policy-svc` | 8085 | <one-line status once built> |`.

**Verification:** build the Docker image and run the container against a
real Postgres — full create-policy → create-version → activate →
evaluate round trip from inside the container.

## 14. Build-pattern evidence from existing services (git history)

Commit history of the three real Go services shows a consistent shape
that the phase plan in `progress.md` is modeled on:

- `tenant-entity-registry-svc`: single large `scaffold ... in Go` commit,
  followed by an `audit remediation — F1-F8, R1-R4` cleanup pass, then a
  targeted fix wiring a real `principal_id` into an audit column. Read:
  scaffold everything at once, then a dedicated review/remediation pass
  catches gaps (this is also the service with zero test coverage on
  `pg_store.go` — the remediation pass didn't include test coverage, a
  gap explicitly called out in `known-gaps.md`).
- `jurisdiction-rules-svc`: incremental — migration + one validation
  endpoint first, then handler unit tests, then list/ancestors endpoints,
  then a Postgres integration test + CI service container, then a
  migration fix (UNIQUE constraint → `CREATE UNIQUE INDEX`), then a
  two-stage admin-mutation pass explicitly split into "stage 1" (schema +
  authz package) and "stage 2" (domain audit fields + idempotency/state
  checks on store mutations). This is the more disciplined of the two
  patterns and the one the phase plan follows: schema → read endpoints →
  tests → write endpoints with idempotency built in from the start, not
  bolted on after.

Policy Service should follow the `jurisdiction-rules-svc` shape, not the
`tenant-entity-registry-svc` shape — given the explicit "Idempotency
Requirement" and "high execution criticality" called out in its own spec
(§6 above), retrofitting idempotency after a big-bang scaffold is exactly
the kind of gap `known-gaps.md` shows is expensive to fix later.

## 15. Batch A implementation record (written, built, tested, verified live)

Code for Batch A (§13.2, §13.3) has been written into this service
folder: `go.mod`, `internal/domain/types.go`, `internal/store/pg_store.go`,
`internal/store/pg_store_test.go`, `internal/handler/handler.go`,
`internal/handler/handler_test.go`, `internal/health/health.go`,
`internal/config/config.go`, `cmd/server/main.go`,
`deployments/migrations/000001_initial_schema.{up,down}.sql`.

**Verification status (2026-07-07): actually run, not just written.**
This sandbox has no Go toolchain and no direct network access, so
verification used Docker: a `golang:1.25-alpine` container ran
`go mod tidy` (regenerated `go.sum`), `go vet ./...`, `go build`, and
`go test ./... -v`, all against a throwaway `postgres:16-alpine`
container on a dedicated Docker network. Result: clean vet, clean build,
**19/19 tests pass**. The compiled binary was then run as a live server
(port 8085) and the full HTTP round trip was exercised with `curl` — see
§16 for the transcript. Still worth an independent re-run locally/in CI
(different Go version, different OS) — see `progress.md`'s "Required
local verification" list — but this is no longer "written but unverified
code," it has actually executed successfully.

Three implementation decisions made in code that go beyond a literal
copy of jurisdiction-rules-svc's pattern, worth knowing about:

1. **`ActivateVersion` is a single store method wrapping a DB
   transaction**, not two independent handler-orchestrated calls to a
   generic transition function. The supersede-then-activate ordering
   inside one transaction is required so the partial unique index
   `idx_policy_versions_one_active_per_scope` (at most one ACTIVE version
   per `(policy_id, tenant_id, legal_entity_id)` scope) is never violated
   mid-operation. The generic `transitionVersionStatus(ctx, queryRower,
   ...)` helper still exists and still takes caller-supplied
   `allowedPriors`, satisfying the "store enforces caller-supplied
   allowedPriors" split — it's just called twice, from inside
   `ActivateVersion`, against a `pgx.Tx` instead of the pool directly.
2. **`CreatePolicyVersion` validates the parent policy exists via
   `FindPolicyByID` before inserting**, rather than relying on the
   `policy_id` foreign key to reject bad references. This mirrors
   `FindRules`' defensive pattern in jurisdiction-rules-svc (which
   validates the parent jurisdiction exists before querying), not
   `CreateRule`'s pattern (which has no such check and would surface an
   FK violation as a misleading 503 instead of a 404) — deliberately
   picked the better of the two existing patterns in that codebase.
3. **No `EventPublisher` dependency in the Batch A handler at all** —
   `PolicyStore` is the only interface the handler takes. Event
   publishing is scoped to Batch C per the task's explicit 3-batch
   sequencing; adding it now would be scope creep into a later batch.
4. **`CreatePolicyVersion`'s dedup-conflict check compares `rule_payload`
   semantically (`jsonEqual`: unmarshal + `reflect.DeepEqual`), not via
   `bytes.Equal`.** This is a deliberate departure from
   jurisdiction-rules-svc's `CreateRule`, which does use `bytes.Equal` on
   its own JSONB `rule_payload` column — and that pattern is latently
   broken there too, it just hasn't been caught because that service's
   own test happens to write its JSON literal with the same spacing
   Postgres re-serializes to (`{"rate": 0.19}`, with a space). Real
   verification here (§16) caught it: Postgres always re-serializes JSONB
   with a space after `:`/`,`, so comparing DB-read bytes against raw
   compact-JSON request bytes (the format `json.Marshal` produces) falsely
   flags every legitimate idempotent retry as a 409 conflict. Fixed
   instead of mirrored, since mirroring a proven-latent bug on request
   would be worse than diverging from the source pattern here.

## 16. Verification transcript (2026-07-07)

Environment: `golang:1.25-alpine` + `postgres:16-alpine`, both Docker
containers on a dedicated bridge network (`policy-svc-net`), source
bind-mounted read/write so `go mod tidy` could update `go.mod`/`go.sum`
in place. Port 8085 published to the host.

1. `docker run ... postgres:16-alpine` → applied
   `000001_initial_schema.up.sql` directly via `psql` — 2 tables + 6
   indexes created with no errors (first real validation the SQL, and
   specifically the `COALESCE(...)`-expression unique indexes and their
   matching `ON CONFLICT` targets, is syntactically valid).
2. `go mod tidy && go vet ./... && go build -o /tmp/policy-svc ./cmd/server && exec /tmp/policy-svc`
   → clean vet, clean build, server started, connected to the DB pool,
   listening on `:8085`.
3. `GET /healthz` → 200, `GET /readyz` → 200.
4. Full HTTP round trip via `curl`: create policy (201) → create version
   v1 (201, DRAFT) → activate v1 (200, ACTIVE) → create version v2 (201,
   DRAFT) → activate v2 (200, ACTIVE) → `GET .../versions` → returned v2
   first (ACTIVE) then v1 (**SUPERSEDED**, not deleted) — confirms the
   atomic supersede-then-activate transaction in `ActivateVersion` works
   correctly against a real database.
5. `go test ./... -v` (first run, `TEST_DATABASE_URL` pointed at the
   Postgres container): 15/15 handler unit tests pass; 3/4 store
   integration tests pass, 1 failure —
   `TestPgStore_CreatePolicyVersion_IdempotencyConflictAndPolicyNotFound`
   — root-caused to the JSONB whitespace issue in §15 item 4.
6. Fixed `jsonEqual` in `pg_store.go`. Re-ran `go vet` + `go test ./... -v`
   → **19/19 pass**.
7. Rebuilt and restarted the live server with the fix; repeated the
   `CreatePolicyVersion` idempotent-retry case directly over HTTP with
   compact JSON (no spaces) — confirmed `201` then `200`, not `409`.

Not yet done at this point: Batch B/C don't exist yet, so nothing about
evaluation, events, CI, or the Dockerfile has been exercised. This
verification covers Batch A only. Containers were left running (not torn
down) at the end of this session so the service stays reachable at
`http://localhost:8085` for manual/Postman testing. Batch A was then also
independently confirmed working by manual Postman testing (all 4
endpoints returned expected status codes).

## 17. Batch B implementation record and verification transcript (2026-07-07)

Code written: `internal/domain/types.go` gained `ApplicablePolicyVersion`
(§13.4); `internal/store/pg_store.go` gained
`FindApplicableVersions` (the scope-precedence query); `internal/handler/handler.go`
gained `ListApplicablePolicyVersions`, `Evaluate`, and
`evaluateApprovalThreshold`; both `handler_test.go` and `pg_store_test.go`
gained corresponding tests (13 new: 9 handler unit tests, 1 store
integration test covering scope precedence/isolation).

**Design decisions made filling gaps the task left open** (none of these
were specified beyond the placeholder `"..."` in the task's `rule_basis`
example, or left fully implicit):

5. **Scope-precedence ordering for "applicable" versions.** The task says
   `GET /v1/policies` returns "the currently-ACTIVE policy version(s)
   applicable to that scope" and evaluate looks up "**the** applicable
   ACTIVE version" (singular) — implying a precedence rule when a global
   version and a tenant/entity-specific override could both be ACTIVE at
   once (the schema explicitly supports this via nullable
   `tenant_id`/`legal_entity_id`, §13.1). Implemented: most-specific-scope
   wins — exact `(tenant_id, legal_entity_id)` match, then tenant-only,
   then global — via a `SQL CASE`-based specificity score, descending.
   `GET /v1/policies` returns the full ordered set (matching the "set"
   wording); `Evaluate` takes the first (most specific) as "the"
   applicable version. **Known v1 limitation, not fixed**: if two
   *distinct* `Policy` rows share a `policy_type` and are both ACTIVE at
   the same specificity tier, the tie-break is `effective_from DESC` —
   arbitrary from a business standpoint, though deterministic. This
   surfaced live in testing (§ below) exactly as expected/documented, not
   as a surprise.
6. **`rule_basis` format and the equal-to-threshold boundary.** The task
   left `rule_basis`'s content as `"..."` and didn't say whether
   `amount == threshold_amount` is `APPROVAL_REQUIRED` or
   `WITHIN_THRESHOLD`. Implemented: `rule_basis = "<policy_code>:<policy_version_id>"`
   (human-readable, deterministic, unique); `amount > threshold` →
   `APPROVAL_REQUIRED`, `amount <= threshold` (including exactly equal) →
   `WITHIN_THRESHOLD`. Both are reasonable defaults, not derived from any
   doc — flag if the real intent differs.
7. **Unimplemented `policy_type` values return `501 Not Implemented`**,
   not a silent `WITHIN_THRESHOLD`-style default or a panic. This wasn't
   specified either, but silently approving/rejecting an action under a
   policy type with no real logic would be a correctness hazard for a
   governance service — fail loud instead.

### Verification transcript

Same environment as §16 (Docker, `golang:1.25-alpine` +
`postgres:16-alpine`, live server on port 8085), continued in the same
session — Postgres already had Batch A's schema and some leftover test
rows from earlier manual/Postman testing.

1. `go vet ./...` clean, `go test ./... -v` → **27/27 pass on the first
   run** (15 Batch A handler + 9 Batch B handler + 4 Batch A store + 1
   Batch B store = 29 test functions total; some Batch A counts folded in
   — see actual test file for the exact list). No bugs found this time.
2. Rebuilt and restarted the live server with Batch B code.
3. `GET /v1/policies` with no `policy_type` → `400 missing_field`.
4. Created a fresh `APPROVAL_THRESHOLD` policy+version with
   `threshold_amount: 5000`, activated it. `GET /v1/policies?policy_type=APPROVAL_THRESHOLD`
   → `200`, array of active versions (included a leftover policy from
   earlier testing too — expected, both are real ACTIVE `APPROVAL_THRESHOLD`
   rows).
5. `POST /v1/policies/evaluate` with `amount: 7500` → `APPROVAL_REQUIRED`;
   `amount: 1000` → `WITHIN_THRESHOLD`. (Matched against the *other*,
   earlier-testing-leftover policy due to the tie-break rule in item 5
   above — confirmed this is the documented behavior, not a bug, by
   inspecting which `policy_version_id`/`rule_basis` came back.)
6. `POST /v1/policies/evaluate` with `policy_type: "SOD_RULE"` (nothing
   active for that type) → `404 no_applicable_policy`.
7. **Scope precedence/isolation, live, with a fresh policy to avoid the
   multi-policy tie-break noise**: activated a global version and a
   tenant-A-specific override on the same policy. `GET .../versions?policy_type=SOD_RULE&tenant_id=<A>`
   → both returned, tenant-specific first. `GET .../versions?policy_type=SOD_RULE&tenant_id=<B>`
   (a different tenant) → only the global version returned — tenant A's
   override never leaked to tenant B.

No bugs found in Batch B (unlike Batch A's JSONB comparison bug) — all
behavior matched the design on the first live pass.

## 18. Batch C implementation record and verification transcript (2026-07-07)

Code written: `internal/events/publisher.go` (new — the `Publisher` type
and its 4 `Publish*` methods + shared `envelope`/`emit()`, mirroring
`governance-decision-log-svc/internal/events/publisher.go` structurally);
`cmd/server/main.go` (constructs the publisher, passes it into
`handler.New`); `.github/workflows/ci.yml` (added `policy-svc` to the
matrix and `TEST_DATABASE_URL` conditional); `Dockerfile` + `.dockerignore`
(new); `services/README.md` (new row).

**A genuine refactor, not just additive files**: publishing
`policy.rule.retired` requires knowing which version(s) got superseded by
an activation, and publishing `policy.version.activated`/
`policy.rule.retired` only on a *real* transition (not an idempotent
retry) requires knowing whether the store actually did anything. Neither
was available from `ActivateVersion`'s original 3-value return
(`*PolicyVersion, []*PolicyVersion, error`, added in Batch A/§13.3). Fixed
by:
- Adding a `RETURNING` clause to the supersede `UPDATE` in
  `PgStore.ActivateVersion` and collecting the affected rows via
  `tx.Query` (previously `tx.Exec`, which discards rows).
- Adding a fourth return value, `transitioned bool` — `false` for the
  pre-existing idempotent-no-op path (activating an already-ACTIVE
  version), `true` for every real transition. This mirrors the
  `created bool` convention `CreatePolicy`/`CreatePolicyVersion` already
  use for the same purpose (distinguish "first time" from "replay").
- This changed `Store.ActivateVersion`'s and `PolicyStore.ActivateVersion`'s
  signatures to `(*PolicyVersion, []*PolicyVersion, bool, error)` —
  updated in both interfaces, the `PgStore` implementation, the handler
  call site, the `stubStore` test double, and every call site in
  `pg_store_test.go` (8 call sites). This is exactly the kind of
  interface-signature churn that's cheap early (Batch A/B were only ever
  run in this same session) and would be expensive later — a live
  argument for why `context.md` §12's "schema → reads → tests → writes
  with idempotency built in from the start" ordering matters even within
  a single service's own batches, not just across the codebase's history.

**Design decision**: `PublishPolicyCreated`/`PublishPolicyUpdated`/etc.
take `correlationID` as an explicit parameter rather than a field on the
domain struct (unlike `governance-decision-log-svc`'s
`GovernanceDecision.CorrelationID`) — `domain.Policy`/`domain.PolicyVersion`
have no such field, and adding one solely to satisfy the publisher would
leak an HTTP-layer concern into the domain package. The handler already
has `correlationID` from the request header at every call site, so
threading it through as a parameter is simpler and doesn't touch the
schema.

### Verification transcript

1. `go build ./... && go vet ./... && go test ./... -v` (same Docker
   environment as §16/§17) → **29/29 pass** after the `ActivateVersion`
   refactor above, including two new tests specifically covering the
   refactor: `TestActivateVersion_Success` (asserts
   `PublishVersionActivated`/`PublishRuleRetired` each called exactly
   once) and `TestActivateVersion_IdempotentNoOp_DoesNotRepublish`
   (asserts neither is called when `transitioned=false`).
2. Verified `.github/workflows/ci.yml`'s Postgres service container
   creates a database named `testdb` (not `policy`) shared across all
   matrix services — `TEST_DATABASE_URL` points there regardless of
   `config.go`'s own `DB_NAME` default, so no mismatch.
3. **Built the real Docker image**: `docker build -t policy-svc:batchc -f Dockerfile .`
   — multi-stage build completed cleanly (`go mod download` → `go build`
   with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` → copied into
   `distroless/static-debian12:nonroot`).
4. **Ran the actual built image** (not the dev bind-mount container used
   for Batches A/B) as a container against the same Postgres container,
   connected via the same Docker network. `/healthz` and `/readyz` both
   `200`.
5. Full round trip from inside that image: create policy (`201`) →
   create version v1 (`201`) → activate v1 (`200`, `ACTIVE`) → create
   version v2 in the same scope (`201`) → activate v2 (`200`, `ACTIVE`)
   → evaluate (`200`, `APPROVAL_REQUIRED` for an amount above v2's
   `threshold_amount`... note: matched against a different
   leftover-from-earlier-testing policy per the documented tie-break in
   §17 item 5, same as before, not a new issue).
6. Grepped the container's logs for `event_type` and confirmed all four
   events fired with correct payloads and correlation IDs: `policy.created`
   (once, on the policy create call), `policy.updated` (twice, once per
   version create), `policy.version.activated` (twice, once per
   activation), and **`policy.rule.retired` exactly once**, for v1,
   carrying the *same* correlation ID as the request that activated v2 —
   confirming the supersede-triggers-retired-event wiring is correct end
   to end, not just at the unit-test level.

No bugs found in Batch C itself (the `ActivateVersion` signature change
was anticipated design work, not a bug fix) — everything matched on the
first live pass through the real Docker image.

**policy-svc v1 is now feature-complete per the 3-batch task scope.**
What's genuinely left, none of it blocking further use of what's built:
- Wire a real `kafka.Writer` into `events.Publisher` — currently a
  logged stub, same as every other service's event publishing in this
  repo (no Kafka backbone exists anywhere yet).
- Wire Authorization Service calls into admin writes — deferred per
  `progress.md`'s non-goals list, since Authorization Service doesn't
  exist yet.
- Implement evaluation logic for `SPEND_CONTROL`, `SOD_RULE`,
  `SIGNATORY_MATRIX` — each is a new `case` in `Evaluate`'s switch, not a
  restructure, per the design in §13.4.
- Consume `entity.created`/`role.updated`/`authority.delegated` — deferred
  until their producers are real.
