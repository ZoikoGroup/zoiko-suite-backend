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

`POST /v1/decisions` must be idempotent on `governance_decision_id` (or
`correlation_id`, tbd — see open question below). Follow the pattern
already proven in `services/audit-event-store-svc/internal/store/store.go`:
`INSERT ... ON CONFLICT (id) DO NOTHING`, never SELECT-then-INSERT.

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

## Open design question — NOT YET DECIDED

Doctrine says governance services "fail closed" (`03-microservices.md`
§22 Failure Mode Expectations), and evidence capture is stated as
non-negotiable. But Authorization Service is explicitly "extremely high
call volume, latency-sensitive" (`03-microservices.md` §8.3). If this
service is unreachable when Authorization Service tries to record a
decision, two options exist:

1. **Fail closed** — the action being authorized is denied/blocked until
   the decision can be durably logged. Strictly honors "every decision is
   itself evidence," but couples Authorization Service's hot path to this
   service's uptime.
2. **Fail safe via durable async write** — Authorization proceeds
   immediately; the decision write goes through a local outbox/queue
   guaranteed to eventually land here. Protects Authorization Service's
   latency SLA, but leaves a small window where an action could complete
   before its evidence exists.

**Do not pick one silently.** This is a cross-service contract decision
that affects Authorization Service too — confirm with whoever owns that
service's implementation before building either side.

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
the required spec block. Do not scaffold service code until the open
design question above is resolved and whoever is driving this work has
explicitly said to proceed.
