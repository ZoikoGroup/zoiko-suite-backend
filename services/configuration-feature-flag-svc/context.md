# Configuration & Feature Flag Service — Context

Compiled from `docs/architecture/03-microservices.md` (the only architecture
doc that mentions this service at all — confirmed by grepping
`01-backend.md`, `02-diagrams.md`, `04-data-model.md`, `05-security.md`,
`06-blueprint.md` for "feature flag"/"configuration service": no schema,
no API list, no event list exists anywhere else) plus the task-approved
concrete spec in §7 below, which fills every gap the architecture doc
leaves open. This file has no independent authority — if it ever disagrees
with the source docs, the docs win; where the docs are silent, §7 (the
approved build task) is authoritative.

## 1. What it is

**Service Class:** Governance-adjacent platform service (not a Governance
Control Plane engine itself — see §2).
**Tier:** 0 (every other service's config/flag reads are on their hot
path).
**Naming convention in this repo:** `-svc` suffix, matching every other
service (`policy-svc`, `governance-decision-log-svc`, etc.) →
`configuration-feature-flag-svc`.

**Purpose** (`03-microservices.md` §9.6, verbatim — this is the entire
doc entry):
> Owns runtime configuration, rollout controls, and environment-aware
> feature flags.

**Critical Constraint** (§9.6, verbatim):
> Configuration may tune service behavior, but must never be used to
> bypass governance doctrine.

This constraint is satisfied by scope, not by code in this service: this
service only stores and serves values — it never calls Policy,
Authorization, or Evidence Requirements services, and nothing here lets a
config value or flag directly disable a check in another service. That
guarantee has to be *re-verified whenever another service becomes a real
consumer* of this service's values — this service can prevent itself from
being a backdoor, but it cannot prevent a careless consumer from wiring a
flag as a bypass. Flag that risk in review whenever a real consumer shows
up.

## 2. How this differs from Policy Service (don't confuse the two)

Policy Service (`services/policy-svc`) is one of the seven Governance
Control Plane engines (`01-backend.md` §07) that every material action
must pass through. Configuration & Feature Flag Service is **not** one of
the seven engines and is not listed as part of the non-bypassable
governance path anywhere in the docs. It is infrastructure *other*
services may consult (e.g. "is the new payroll UI enabled for tenant X in
staging"), not a governance decision point. Do not let this service grow
governance-engine responsibilities — that is precisely what the Critical
Constraint above forbids.

## 3. Ownership boundary

**Owns:** runtime configuration values and environment-aware feature
flags, both versioned and effective-dated (see §7.1 for exact schema).

**Explicitly does not own:** authorization decisions, policy evaluation,
evidence requirements, or anything else that belongs to a Governance
Control Plane engine. A config value can *describe* a rollout percentage;
it cannot *grant* an authorization outcome.

## 4. Data model

No entities for this service exist anywhere in `04-data-model.md` — this
is the one architecture-doc gap larger than anything found while building
`policy-svc` (which at least had a speculative `Policy`/`PolicyVersion`
entity to mirror/supersede). The schema in §7.1 below is entirely the
approved build task's design, not derived from any doc.

## 5. Evidence & compliance obligations

Not stated anywhere in the architecture docs — no "Evidence Obligations"
section exists for this service (contrast with Policy Service's explicit
"preserve every policy version" language in `03-microservices.md` §8.1).
The append-only versioning design in §7.1 (never UPDATE/DELETE, always a
new row + end-dated predecessor) gives this service the same practical
evidentiary property as Policy Service's `PolicyVersion` history —
"what was config value X set to, and when, and by whom" is always
reconstructable — but this is a design choice made for this build, not a
documented requirement being satisfied.

## 6. Idempotency & scaling

**Idempotency Requirement:** not stated explicitly in `03-microservices.md`
§9.6 (unlike Policy Service's §8.1, which does state one). Doctrine's
blanket rule still applies ("every state-changing API must be
idempotent" — `.agents/rules/doctrine.md`), so this needed a design
decision — see §7.3.

**Scaling Characteristics:** not stated. Reasonable to assume read-heavy
(every service potentially reads config/flags on its hot path) and
low-write-frequency, same shape as Policy Service, but this is inferred
from the service's purpose, not documented.

## 7. Concrete v1 implementation spec (task-approved, fills every gap above)

This section is the authoritative, approved build spec for
`configuration-feature-flag-svc` v1, handed down as three sequential work
batches (mirroring how `policy-svc` and `governance-decision-log-svc` were
each built). Scaffolding pattern to mirror exactly:
`services/governance-decision-log-svc` (`cmd/server/main.go`: config → zap
logger → pgxpool with fail-fast `Ping` → store → handler → `/healthz` +
`/readyz` → graceful shutdown; `internal/{config,domain,handler,store,events,health}`).

### 7.1 Schema

Config values and feature flags are versioned and effective-dated, never
updated in place — the same "no soft-delete, a change is a new row"
doctrine invariant used by `jurisdiction-rules-svc`/`policy-svc`, applied
here on the task's own explicit instruction rather than by mirroring.

`config_entries` table:
- `config_id` — UUID, PK, server-generated (`gen_random_uuid()`)
- `key` — VARCHAR, the config key (e.g. `"payroll.release.batch_size"`)
- `value` — JSONB (chosen over TEXT: config values are often structured,
  and this matches `rule_payload`'s precedent in `policy-svc`)
- `environment` — VARCHAR, e.g. `"production"`, `"staging"`
- `tenant_id` — nullable UUID; NULL means global default for that
  environment
- `effective_from` — TIMESTAMPTZ, server-set to `NOW()` at write time
- `effective_to` — nullable TIMESTAMPTZ; NULL means currently effective
- `created_by_principal_id` — TEXT
- `created_at` — TIMESTAMPTZ

`feature_flags` table — identical shape, swapping `value` for:
- `flag_id` — UUID, PK
- `enabled` — BOOLEAN
- `rollout_percentage` — INTEGER, default 100, CHECK 0–100

**No UPDATE/DELETE on either table.** A "change" is always: end-date the
row currently effective for that `(key, environment, tenant_id)` scope
and insert a new one, in the same transaction (see §7.3).

Partial unique index on both tables — at most one currently-effective row
per `(key, environment, tenant_id)` scope, mirrors `policy-svc`'s
`idx_policy_versions_one_active_per_scope`:
```
CREATE UNIQUE INDEX ... ON config_entries (key, environment, COALESCE(tenant_id, <nil-sentinel>))
WHERE effective_to IS NULL;
```
This is the concurrency backstop referenced in §7.3 — it is what makes
the upsert safe under concurrent writers for the same scope, not just the
application-level transaction logic.

### 7.2 Endpoints

Config:
- `POST /v1/config` — upsert a value for `(key, environment, tenant_id)`.
  Body: `{key, value, environment, tenant_id?, created_by_principal_id}`.
- `GET /v1/config/{key}?environment=X&tenant_id=Y` — the row currently
  effective for that exact tuple. `environment` is required (400 if
  missing, mirrors `policy-svc` requiring `policy_type`); `tenant_id` is
  optional (omit for the global scope). **404 if none** — this service
  does not fall back from a tenant-specific miss to a global default;
  the spec's wording ("whichever row is currently effective **for that
  tuple**") reads as an exact match, not a precedence search like
  `policy-svc`'s `FindApplicableVersions`. Flag if inheritance-style
  fallback (tenant → global) is actually wanted — that would be a
  deliberate feature addition, not implied by the current wording.
- `GET /v1/config?environment=X&tenant_id=Y` — every currently-effective
  entry, filtered by the given optional parameters (both optional here,
  unlike the single-key lookup).

Feature flags — identical endpoint shape under `/v1/flags`:
- `POST /v1/flags` — body: `{key, enabled, environment, tenant_id?, rollout_percentage?, created_by_principal_id}`. `rollout_percentage` defaults to 100 if omitted; 400 if given outside 0–100.
- `GET /v1/flags/{key}?environment=X&tenant_id=Y`
- `GET /v1/flags?environment=X&tenant_id=Y`

### 7.3 Idempotency design (my call — the one gap the task spec left open)

Unlike `policy-svc`'s Batch A task (which explicitly said "same idempotent
`INSERT...ON CONFLICT DO NOTHING` dedup"), this task's spec never states
an idempotency key for `POST /v1/config`/`POST /v1/flags`. A naive
"always end-date current + insert new" implementation is **not**
idempotent: a retried POST would end-date an already-superseded row
(harmless no-op) but insert a *second* "new" row for the same logical
write, breaking the "exactly one currently-effective row per scope"
invariant enforced by the partial unique index in §7.1.

**Resolved as an upsert-with-value-equality check**, not a caller-supplied
dedup key (there is no natural caller-supplied ID in this task's request
shape the way `decision_id` exists for `governance-decision-log-svc`):

1. Within one transaction, `SELECT ... FOR UPDATE` the row currently
   effective for the target `(key, environment, tenant_id)` scope, if any
   — the row lock serializes concurrent writers for the same scope so two
   racing identical POSTs can't both decide "nothing to supersede."
2. If a current row exists and its value is unchanged (`value` for
   config — compared via the same `jsonEqual` semantic-JSON-equality
   approach as `policy-svc`'s `CreatePolicyVersion`, not `bytes.Equal`,
   for the same JSONB-whitespace-reserialization reason documented there;
   `enabled`+`rollout_percentage` for flags) → **no-op, return the
   existing row, `created=false`, HTTP `200`.** Setting something to the
   value it's already at is safe to repeat.
3. If a current row exists and the value differs → end-date it
   (`effective_to = NOW()`) and insert the new row, same transaction →
   `created=true`, HTTP `201`.
4. If no current row exists for that scope → insert the first row →
   `created=true`, HTTP `201`.

This is a genuine design decision, not implied by the task text — flag if
the real intent was a caller-supplied version/idempotency key instead
(that would be a breaking API change to add later, same as
`evaluated_by_principal_id`/`decision_id` were added to `policy-svc`'s
`Evaluate` after the fact).

### 7.4 Failure mode

Not stated in the task or the doc. Follows this repo's universal
convention: store unreachable → `503 store_unavailable` (fail closed —
never silently return a stale/default value on a store failure). No
applicable config/flag for a scope → `404`; per the same posture
`policy-svc`'s `Evaluate` takes on missing policies, **this service does
not guess a safe default** — a consumer treating a feature flag's `404`
as "enabled" instead of "disabled" would be a real safety bug on the
consumer's side, not something this service can prevent by design alone.

### 7.5 Events (Batch 3)

Mirror `governance-decision-log-svc/internal/events/publisher.go` exactly
— same `envelope` struct, same log-only stub `emit()` (`// TODO: publish
to Kafka topic`, no real writer anywhere in this repo yet). Publish:
- `config.updated` — only when `POST /v1/config` performs a *real*
  transition (`created=true`), never on the idempotent-no-op path.
- `feature_flag.updated` — same rule, for `POST /v1/flags`.

No consumed events — this service has no documented dependency on any
other service's events, and none are implied by the task.

### 7.6 CI / Dockerfile / README (Batch 3)

- Add `configuration-feature-flag-svc` to `.github/workflows/ci.yml`'s
  `matrix.service` list and its `TEST_DATABASE_URL` conditional, same
  pattern as every other Go service with real Postgres integration tests.
- Dockerfile: mirror `services/governance-decision-log-svc/Dockerfile`
  exactly (two-stage `golang:1.25-alpine` → `distroless/static-debian12:nonroot`
  build, `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`). Binary name
  `configuration-feature-flag-svc`, `EXPOSE 8086` (see §7.7 — **not** 8084
  as originally planned).
- `services/README.md` — add a row: port `8086`.

### 7.7 Port assignment — corrected during Batch 3

The original task spec assigned this service port `8084`, reasoning that
`8080`–`8083` were taken and `8085` was reserved by `policy-svc`. **That
assumption went stale between when the task was written and when Batch 3
was actually built**: a real `deployments/docker-compose.yml` and
`services/README.md` update landed on `main` in the interim (see the
"Synced with `origin/main`" notes in `policy-svc/progress.md`) that
assigns port `8084` to `audit-event-store-svc` for real — confirmed by
reading `docker-compose.yml` directly (`audit-svc` container, `PORT:
"8084"`, `ports: ["8084:8084"]`), not just inferred from a doc.

**Corrected assignment: `8086`.** Ports actually claimed as of this
writing: `identity-context-svc` (8080), `tenant-entity-registry-svc`
(8081), `jurisdiction-rules-svc` (8082), `governance-decision-log-svc`
(8083), `audit-event-store-svc` (8084), `policy-svc` (8085). This is the
same class of mistake `policy-svc`'s own port assignment was written
defensively to avoid — a reminder that "confirmed free" claims about
ports go stale fast in a repo with this much concurrent service work, and
should be re-checked against `docker-compose.yml` directly (not just
`services/README.md` or another service's docs) immediately before
picking one, not just at task-authoring time.

**Not done as part of this batch**: this service has not been added to
`deployments/docker-compose.yml`'s unified stack. That file orchestrates
5 services today; adding a 6th is a reasonable follow-up but is scope
creep beyond what this build task asked for — flagged in `progress.md`,
not fixed here.

## 8. Tech stack & model policy

`.agents/rules/tech-stack.md` doesn't name this service explicitly the
way it names Identity/Authorization/Policy as Go-mandated Tier 0 services
— but every existing service in this repo is Go, and the task spec
mandates Go explicitly, so there's no ambiguity to resolve here.

## 9. Build sequencing

Named as one of the ten Phase 1 "Sovereign Spine" components
(`06-blueprint.md`). Not on the non-bypassable governance path (§2 above),
so unlike Policy/Authorization Service it does not block other Tier 0
services from meeting their own exit criteria — it can be built in
parallel with anything else in Phase 1.

## 10. Implementation record

See `progress.md` in this folder for the batch-by-batch build log and
current status, including the honest verification status (this sandbox
has no Go toolchain or running Docker daemon — see `progress.md` for
exactly what that means for how much to trust this code before it's run
for real).

## 11. Quick Reference — Endpoints & How to Run (added 2026-07-08)

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/config` | Create/update a config value for `(key, environment, tenant_id)` — upsert-with-value-equality, idempotent |
| `GET` | `/v1/config/{key}?environment=X&tenant_id=Y` | Read the currently-effective value for an exact scope |
| `GET` | `/v1/config?environment=X&tenant_id=Y` | List currently-effective config entries (both filters optional) |
| `POST` | `/v1/flags` | Create/update a feature flag for `(key, environment, tenant_id)` — same upsert pattern |
| `GET` | `/v1/flags/{key}?environment=X&tenant_id=Y` | Read the currently-effective flag state for an exact scope |
| `GET` | `/v1/flags?environment=X&tenant_id=Y` | List currently-effective flags |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness probe (DB connectivity) |

### Running the server

**Option A — native Go** (requires Go 1.25+ installed locally)
```powershell
cd services/configuration-feature-flag-svc
$env:DB_HOST="localhost"; $env:DB_PORT="5432"; $env:DB_NAME="configuration_feature_flag"; $env:DB_USER="postgres"; $env:DB_PASSWORD="secretpassword"; $env:DB_SSLMODE="disable"; $env:PORT="8086"
go run ./cmd/server
```

**Option B — Docker only, no local Go needed** (this is the exact method used to build and manually verify this service via Postman)

1. Network + Postgres:
```powershell
docker network create cff-net
docker run -d --name cff-pg --network cff-net -e POSTGRES_PASSWORD=secretpassword -e POSTGRES_DB=configuration_feature_flag -p 55432:5432 postgres:16-alpine
```
2. Apply the migration (creates both `config_entries` and `feature_flags`):
```powershell
Get-Content deployments\migrations\000001_initial_schema.up.sql | docker exec -i cff-pg psql -U postgres -d configuration_feature_flag
```
3. Build and run (from the `services/configuration-feature-flag-svc` directory):
```powershell
docker run -d --name cff-app --network cff-net -v "${PWD}:/src" -w /src -p 8086:8086 `
  -e DB_HOST=cff-pg -e DB_PORT=5432 -e DB_NAME=configuration_feature_flag -e DB_USER=postgres -e DB_PASSWORD=secretpassword -e DB_SSLMODE=disable -e PORT=8086 `
  golang:1.25-alpine sh -c "go build -o /tmp/svc ./cmd/server && exec /tmp/svc"
```
4. Confirm: `curl http://localhost:8086/healthz` → `{"status":"ok"}`

A ready-to-import Postman collection covering every endpoint above (including the versioning and idempotency proof requests) is at `postman_collection.json` in this same folder.

**Tear down when done:**
```powershell
docker rm -f cff-app cff-pg
docker network rm cff-net
```

**Important — do not point `TEST_DATABASE_URL` at this same database while it's serving a live demo.** One integration test intentionally drops `feature_flags` to test the store-unavailable path with no teardown to restore it — harmless against an isolated/ephemeral test database, but it will break a live demo sharing the same Postgres instance (this happened once already during this build — see `progress.md`'s "Manual Postman verification" section for the full account and the fix).
