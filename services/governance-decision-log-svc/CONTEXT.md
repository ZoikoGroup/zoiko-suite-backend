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

**Governance dependencies**: none upstream for governance logic, but writes
are authorized via authorization-svc (action `GOVERNANCE_DECISION_RECORD`).
Reads are also authorized (`GOVERNANCE_DECISION_READ`). Replay requires
`GOVERNANCE_DECISION_REPLAY`. This service sits beneath
Policy/Authorization/Workflow for governance decisions, but requires
authorization-svc to confirm the caller may append to the ledger.

## Evidence obligations

Append-only, immutable. No update, no delete. Must remain queryable for
audit, regulator, and legal-discovery scenarios for as long as the
retention class below requires.

## Decisions recorded 11 Nov 2026 (D2-2.F11)

| Question (register) | Decision | Where it lives |
|---|---|---|
| Retention class (D13) | **EV-REGULATED** (Event Catalogue section 16: elevated integrity, access and retention controls; legal hold overrides disposition) and record class **COMPLIANCE_EVIDENCE**. The duration is resolved by retention-registry-svc per jurisdiction and legal hold, never hard-coded here | `domain.RetentionClass` / `RecordClass`: `retention_class` + `record_class` on every decision read, on every event envelope, and on the export bundle (set when read; not part of the hashed canonical form) |
| "audit" query dimension of Doc 03 section 8.7 (E3) | The **correlation id**: every decision made under one audited business trace | `GET /v1/decisions?correlation=<id>` (max 64), index `idx_governance_decisions_tenant_correlation` (000017) |
| Is `decision_id` the GCP section 16 / GOV-07 `evidence_id`? (D11) | **Yes.** The decision row is the GOV-07 ledger leaf (`chain_seq`, `curr_hash`, Merkle leaf); there is no second identifier | openapi.yaml description |
| Which GOV section owns GovernanceDecision (N4), and does GOV-07's hash chain apply here (D10)? | **GOV-07**, decided 23 Oct 2026 (implementation plan section 4: "GOV-07 Evidence ledger and verification: governance-decision-log-svc"); the chain is built (000016) | tracker Reference A3 |
| `X-Purpose-Context` on reads or appends (D-06) | **Reads**, not appends | `internal/envelope/contract.go` (`PurposeContext: RequiredOnRead`) |
| Event naming (D-07, group decision C3) | **Keep the Doc 03 wire names and publish the Event Catalogue canonical names alongside** (rule R9, deprecation window): `com.zoikosuite.governance.decision.recorded` / `.replayed` | `events.WithCanonicalAlias`; two outbox rows per event, same `payload_hash`, the alias carries `alias_of_event_id` |

## Idempotency

Two keys, both truthful (12 Oct 2026):
- **`Idempotency-Key`** (required on every POST): same key + same body →
  200 with the stored row; same key + different body → 409
  `IDEMPOTENCY_MISMATCH`. Keys live in `idempotency_keys` (000008), scoped
  per tenant. The key check runs **after** authorization, because a replay
  returns a stored decision.
- **`decision_id`** (caller-supplied, unique per tenant since 000013 — the
  primary key is `(tenant_id, decision_id)`): `INSERT ... ON CONFLICT
  (tenant_id, decision_id) DO NOTHING`, never SELECT-then-INSERT. A repeat
  answers 200 with the **stored** row when the content matches and 409
  `DECISION_ID_CONFLICT` when it does not. It never echoes an unstored body.
- Replays (`POST /v1/decisions/{id}/replay`) are idempotent on
  `(tenant, decision, Idempotency-Key)` (000014): a retry returns the first
  manifest without re-evaluating.

## Event publishing — transactional outbox

`store.RecordDecision` writes the decision, its idempotency key and the
complete `governance.decision.recorded` envelope (`events.DecisionRecorded`)
to `outbox` in **one transaction**. `internal/outbox` relays it: it claims
rows with `FOR UPDATE SKIP LOCKED` under the `app.outbox_relay` marker the
outbox RLS policy admits (000012), publishes the envelope byte for byte
(`payload` is BYTEA, so `payload_hash` stays verifiable), keys the message by
`decision_id`, and records failures in `attempts` / `last_error` for retry.
A broker outage delays events; it cannot lose them.

Envelope: `event_id`, `event_type`, `event_version`, `emitted_at`,
`schema_version` (1.3), `source_service`, `tenant_id`, `legal_entity_id`,
`actor_id`, `correlation_id`, `causation_id`, `workflow_instance_id`,
`aggregate_type`, `aggregate_id`, `payload_hash`, `payload`. The payload no
longer carries a `jurisdiction_context` filled with `rule_basis`; this service
holds no jurisdiction of its own.

Relay health is on `/metrics`: alert on `outbox_oldest_pending_seconds`
(also `outbox_pending_events`, `outbox_pruned_events_total`). Published rows
older than `OUTBOX_RETENTION` (default `168h`; `0` keeps them) are pruned;
unpublished rows never are. `Stop()` cancels a relay pass blocked on a dead
broker, and what it did not mark published is relayed again.

`ENV=production|staging` with no `KAFKA_BROKERS` refuses to start. Locally,
`KAFKA_BROKERS=` runs without a relay and events wait in the outbox.

Contracts: `schemas/governance.decision.recorded.v1.json` and
`schemas/governance.decision.replayed.v1.json`, registered in
schema-registry-svc under both names by
`deployments/scripts/register-governance-decision-log-schemas.py` (idempotent);
`TestR9_EmittedEnvelopesMatchTheRegisteredSchemas` keeps them true to the
producer.

Every event is queued twice in the same transaction (decision C3, rule R9):
under its wire name and under its Event Catalogue canonical name
(`com.zoikosuite.governance.decision.recorded` / `.replayed`). The alias has
the same payload and `payload_hash`, its own `event_id`, and
`alias_of_event_id` naming the wire event. Envelope `schema_version` is 1.3
(adds `retention_class`, `alias_of_event_id`).

## Read authorization

Every GET is authorized on `GOVERNANCE_DECISION_READ` on the legal entity the
data belongs to (the decision's, or the list's `entity` filter), with the
platform scope (`AUTHZ_PLATFORM_SCOPE_ID`) as the tenant-wide reader tried
second. An unfiltered list needs the platform grant. An authz outage is a 503,
never a reason to fall back.
`DECISION_LOG_READ_AUTHZ=warn` logs instead of refusing (a migration path
only). The code default and the compose default are `enforce`.

The three service readers (D2-2.F07, 11 Nov 2026) call the :8450 mTLS
listener with their own workload certificates (`identity-envelope/mtlsclient`,
each with its own bootstrap token) and are in `IDENTITY_PEERS`; every read
carries the full input envelope with a purpose:

| Reader | Principal it vouches for | Purpose |
|---|---|---|
| contract-lifecycle-svc (activation verifies its decision) | `svc-contract-lifecycle` (READ, authorization-svc 000042) | `contract_activation_verification` |
| decision-support-svc (precedent sample) | `svc-decision-support` (READ, authorization-svc 000042) | `decision_support_precedent` |
| evidence-manifest-svc (manifest snapshot) | the signed-in user (their own READ grant) | `evidence_manifest_assembly` |

decision-support-svc and evidence-manifest-svc also decode the list as a page
(`records`); evidence-manifest-svc follows `next_cursor` to the end of the
range and fails closed rather than snapshotting a partial range.

## Workload identity hardening (11 Nov 2026)

- **Rotation.** The certificate from mtls-management-svc is held by
  `mtls.RotatingIdentity`: the :8450 listener and the authorization-svc
  client read it per handshake, and it is re-provisioned at two thirds of its
  lifetime (retries back off from 1 to 15 minutes; the log escalates inside
  the last day). It used to be frozen at boot, so day 91 broke every peer
  write and every authorization check.
- **Revocation.** `mtls.RevocationChecker` fetches mtls-management-svc's
  CA-signed CRL (`MTLS_CRL_URL`, every `MTLS_CRL_REFRESH_SECONDS`, default 30),
  refuses a CRL not signed by the listener's CA or older than the one held,
  and refuses at the handshake any client certificate on it, and every client
  certificate while no current CRL is held. `MTLS_REVOCATION_CHECK=off` is
  accepted only with `ENV=local`.
- **Scheduled sealing.** `internal/sealer` seals every tenant's unsealed
  decisions each `EVIDENCE_SEAL_INTERVAL` (default `1h`; `0` = off) as
  `EVIDENCE_SEAL_PRINCIPAL_ID` (`svc-governance-decision-log`). It lists work
  across tenants under the read-only `app.evidence_sealer` marker (000017)
  and seals each tenant through the ordinary tenant-scoped path: chain
  re-verified first (a broken chain is refused and counted as
  `gdl_evidence_seal_total{outcome="chain_broken"}`, alert
  GovernanceLedgerChainBroken), advisory lock, Idempotency-Key derived from
  the head seen, so replicas on the same tick make one package.

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
- `IDENTITY_VERIFY` — compose runs `require` (`DECISION_LOG_IDENTITY_VERIFY`):
  the console presents its signed envelope; policy-svc's and workflow-svc's
  relays call :8450 with their certificates (identity-envelope/mtlsclient).
- `MTLS_ENABLED` / `MTLS_PORT` (8450) / `MTLS_BOOTSTRAP_TOKEN_PATH` — the
  inbound mTLS listener for peer workloads (X5); `IDENTITY_PEERS` (default
  `policy-svc,workflow-svc`) names the certificates that may vouch for
  X-Principal-Id; `AUTHZ_MTLS_ENABLED` / `AUTHZ_MTLS_URL` send authorization
  checks to authorization-svc's :8449 with the same workload identity.

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

## Quick Reference — Endpoints & How to Run (added 2026-07-08)

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/decisions` | Record a governance decision (idempotent on `Idempotency-Key` and `decision_id`) — called by Policy/Workflow services after each evaluation |
| `GET` | `/v1/decisions/{decision_id}` | Retrieve one decision by ID |
| `GET` | `/v1/decisions?actor=&entity=&action=&rule_basis=&from=&to=&limit=&offset=` | List/query decisions — all filters optional, compose with AND semantics |
| `POST` | `/v1/decisions/{decision_id}/replay` | Re-evaluate against the exact policy version (APPROVAL_THRESHOLD only) |
| `GET` | `/v1/decisions/{decision_id}/replay-manifests` | Replay history of one decision |
| `GET` | `/v1/decisions/summary?days=\|from=&to=&entity=` | KPIs, breakdowns, daily trend, facets (console) |
| `GET` | `/v1/decisions/{decision_id}/evidence` | One decision's chain link (re-computed), sealing package, events |
| `GET` | `/v1/replay-manifests?mismatches=&limit=&offset=` | Tenant-wide replay history |
| `GET` | `/v1/evidence/chain/verify` | Re-compute every link and every sealed package (G-07) |
| `GET` | `/v1/evidence/chain/export` | Evidence bundle download (offline re-verifiable) |
| `GET` | `/v1/evidence/packages` | Sealed packages, newest first |
| `POST` | `/v1/evidence/packages/seal` | Seal the chain (`GOVERNANCE_EVIDENCE_SEAL`, Idempotency-Key) |
| `GET` | `/v1/evidence/packages/{package_id}` | One package |
| `GET` | `/v1/evidence/packages/{package_id}/verify` | Re-compute one package |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness probe (DB connectivity) |

The full contract, status codes and required headers are in `openapi.yaml`.

### Evidence ledger (G-07, 000016)

The database links every decision on insert (BEFORE INSERT trigger
`gdl_chain_link`, per-tenant advisory lock): `chain_seq` 1..n in storage order,
`prev_hash` (32 zero bytes for the first), `curr_hash = sha256(prev_hash ||
gdl_canonical(row))`, where the canonical form is the row's jsonb text (v1).
No caller can supply hashes. A sealed package (`evidence_packages`,
append-only) records the RFC 6962 Merkle root over `curr_hash` of decisions
1..to_seq and `package_hash = sha256(prev_package_hash || root || head ||
uint64be(to_seq))`, so re-verification detects even a full rewrite of the
chain. Changing `gdl_canonical` breaks every link: a new canonical form is a
new version, never an edit.

The register fields (title, summary, domain, priority, tags,
related_obligation_id) are optional; `domain` is classified from `action_type`
by `decision_domain_rules` when omitted, and the status category of an outcome
comes from `decision_outcome_categories` (unmapped → OTHER). A follow-up (the
resolution of a PENDING/ESCALATED decision) is a new decision naming the
earlier one in `causation_id` — decisions are never edited.

Local dependency E2E: `deployments/scripts/e2e-governance-decision-log-deps.py`
(needs authorization, identity, tenant, policy-svc, kafka up and
`seed-demo-rbac.ps1` run). Service audit: `scripts/audit.sh`.

### Running the server

**Option A — native Go** (requires Go 1.25+ installed locally)
```powershell
cd services/governance-decision-log-svc
$env:DB_HOST="localhost"; $env:DB_PORT="5432"; $env:DB_NAME="governance_decision_log"; $env:DB_USER="postgres"; $env:DB_PASSWORD="secretpassword"; $env:DB_SSLMODE="disable"; $env:PORT="8083"
go run ./cmd/server
```

**Option B — Docker only, no local Go needed**

1. Network + Postgres:
```powershell
docker network create gdl-net
docker run -d --name gov-decision-log-pg --network gdl-net -e POSTGRES_PASSWORD=secretpassword -e POSTGRES_DB=governance_decision_log -p 55434:5432 postgres:16-alpine
```
2. Apply every migration, in order:
```powershell
Get-ChildItem deployments\migrations\*.up.sql | Sort-Object Name | ForEach-Object { Get-Content $_ -Raw | docker exec -i gov-decision-log-pg psql -v ON_ERROR_STOP=1 -U postgres -d governance_decision_log }
```
3. Build and run (from the `services/governance-decision-log-svc` directory):
```powershell
docker run -d --name gov-decision-log-app --network gdl-net -v "${PWD}:/src" -w /src -p 8083:8083 `
  -e DB_HOST=gov-decision-log-pg -e DB_PORT=5432 -e DB_NAME=governance_decision_log -e DB_USER=postgres -e DB_PASSWORD=secretpassword -e DB_SSLMODE=disable -e PORT=8083 `
  golang:1.25-alpine sh -c "go build -o /tmp/svc ./cmd/server && exec /tmp/svc"
```
4. Confirm: `curl http://localhost:8083/healthz` → `{"status":"ok"}`

**Tear down when done:**
```powershell
docker rm -f gov-decision-log-app gov-decision-log-pg
docker network rm gdl-net
```
