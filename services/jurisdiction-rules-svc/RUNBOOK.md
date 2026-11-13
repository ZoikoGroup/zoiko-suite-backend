# jurisdiction-rules-svc — Runbook

Service D2-1 (Governance Platform): jurisdictions, effective-dated rules, jurisdiction packs
(registry, compiler, certification, release), runtime resolution with decision evidence,
calendars, payment terms, rollouts, remediation. Port 8082, database `jurisdiction_rules`,
compose service `jurisdiction-svc` (container `jurisdiction-rules-svc`). Topic out:
`zoiko.jurisdiction.events`; topics in: `zoiko.entity.events`, and the regulatory feed when
configured. Contracts: `openapi.yaml`, `asyncapi.yaml`. Deviations: `SPEC_DEVIATIONS.md`.

## Dependencies

| Dependency | Used for | When it is down |
|---|---|---|
| Postgres (`jurisdiction_rules`) | everything | `/readyz` 503; every route 503 `store_unavailable` (fail closed) |
| authorization-svc | every admin command and every authorized read | 503 `authz_unavailable`; nothing is written |
| identity-context-svc (JWKS, introspection) | the identity gate; J-01 admin writes need a verified envelope | admin writes 401/503; reads follow `IDENTITY_VERIFY` |
| workflow-svc | rule activation confirms its `X-Approval-Reference` (J-02) | activation 503, fail closed |
| Kafka | outbox relay, entity consumer, regulatory feed consumer | writes still commit (outbox); events wait; consumers resume from their offsets |
| tenant-entity-registry-svc | J-10 backfill only | backfill 503 `registry_unavailable`; nothing else is affected |

## Configuration (beyond DB, Kafka, authz)

| Variable | Default | Meaning |
|---|---|---|
| `IDENTITY_VERIFY` | `require` outside local | gate mode for every route; compose runs `warn` because out-of-domain readers send no envelope yet |
| `IDENTITY_VERIFY_ADMIN` | `require` outside local/dev | J-01: writes under `/v1/admin/` and `/v1/operations/` need a verified envelope or mTLS peer; `off` is refused outside local/dev |
| `CURSOR_SIGNING_KEY_HEX` | random per process in local/dev | API-4 cursor key, at least 32 bytes; **required outside local/dev** (and the same on every replica) |
| `TENANT_REGISTRY_URL` | unset | enables `POST /v1/admin/entity-applicability:backfill` (J-10) |
| `KAFKA_REGULATORY_FEED_TOPIC` | unset | enables the regulatory feed consumer (JR-6). Leave unset until external-data-feed-svc publishes `regulatory.source.changed` |
| `RULE_EFFECTIVITY_INTERVAL_SECONDS` | 60 | how often rules crossing their effective start/end are announced (5–3600) |
| `RESOLVER_ENABLED`, `RESOLVER_RING`, `RESOLVER_REGION` | compose: enabled | the runtime resolver; production and staging must name ring and region |
| `PACK_SIGNING_KEY_REF`, `PACK_SIGNING_KEY_FILE` | unset | pack signing; without them `…/sign` answers 503. Production key custody waits on SEC-1 (⛔) |
| `WORKFLOW_SERVICE_URL` | required outside local/dev | approval confirmation for activations |

## Deploy

1. Apply migrations before the new binary: `python deployments/migrate.py up --db jurisdiction_rules`
   (000032 tenant RLS, 000033 outbox requeues + UPDATE grant, 000034 aggregate row versions,
   000035 effectivity watermark, 000036 async operations, 000037 schema convergence).
   000037 is a no-op on a database built from the migrations directory. On a database first
   migrated before 2 Nov 2026 it restores the rule provenance and parameter columns, the
   provenance guard trigger and the foreign keys that 000005 never applied there. Without it,
   rule provenance and every pack compile answer 503 (`column "regime_id" does not exist`).
   If it fails on a foreign key, a legacy row has no parent: fix that row and rerun.
2. Apply access-control 000052 (`--db access_control`): it registers the six new actions and
   publishes the nine role templates that carry every action this service checks. Grant roles
   from those templates; nobody can use the new routes before.
3. Set `CURSOR_SIGNING_KEY_HEX` (same value on every replica) outside local/dev.
4. Roll the image. `/readyz` must answer 200 before traffic.
5. After 000032: submissions written before it have no tenant and are no longer readable by
   any tenant (by design); decisions without a tenant stay visible to tenant-less callers only.

Rollback: the previous binary runs on the new schema, except that it reads tenant tables
without setting `app.tenant_id`, so with FORCE RLS it would see none of their rows. Revert
000032 (`down`) together with a binary rollback.

## Operations

### Parked outbox events (J-16)
An event the broker refused 10 times is parked; the relay moves on. Symptoms: metric
`outbox_parked_events_total` increases, `attention.parked_outbox_events` in the console summary.
1. `GET /v1/admin/outbox/parked` — read `last_error` (message too large, unknown topic, ACL).
2. Fix the cause (broker config, topic, ACL).
3. `POST /v1/admin/outbox/{outbox_id}/requeue` with `{"reason": "...", "evidence_ref": "INC-…"}`
   (JURISDICTION_OUTBOX_EVENT_REQUEUE). The requeue is recorded append-only and the next relay
   tick publishes the event. A second requeue of the same event while it is not parked is 409.

### Two replicas publishing
The relay claims rows `FOR UPDATE SKIP LOCKED` under a transaction advisory lock: only one
replica publishes per tick, and events of one aggregate keep their order. No action needed.

### Entity mappings missing (J-10)
An entity created before this service consumed `zoiko.entity.events` (or whose events
expired) has no jurisdiction mapping: `/v1/entities/{id}/jurisdictions` is empty and
determination answers no applicable jurisdiction.
Run `POST /v1/admin/entity-applicability:backfill` as a steward of that tenant
(`{"legal_entity_id": "…"}` for one entity, empty body for all). It is idempotent; a newer
live event always wins. 502 = the registry refused the caller; 503 = registry down (rerun).

### `503 event_not_recorded`
A write whose event could not be written to the outbox was rolled back (J-11). Check the
database (`event_outbox` constraints, disk). The client retries with the same
`Idempotency-Key`.

### `401 identity_not_verified`
An admin write arrived with identity headers only (J-01). The console must send its signed
envelope (`Authorization: Bearer`); a service must call over mTLS as a registered peer.

### `412 precondition_failed`
The caller's `If-Match` is older than the aggregate. Re-read (the 412's `ETag` is current) and
retry. Not an incident.

### Async compile / test runs (API-5)
`Prefer: respond-async` on `…/compile` or `…/test-runs` answers 202 + `Location`. An operation
stuck in `queued` or `running` after a restart: the background run died with its process.
Re-issue the command (a new `Idempotency-Key`); a queued one can be cancelled with
`POST /v1/operations/{id}/cancel`.

### Legal drift from the feed (JR-6)
When enabled, `regulatory.source.changed` moves ACTIVE rules citing the reference to
`UNDER_REVIEW` (never changes content). Work them from the console's attention list:
review the source, record the outcome with `POST /v1/admin/rules/{id}/drift` (`CURRENT`), or
author a superseding rule version.

### Rule effectivity events
`regulatory-rule.effective` / `.expired` are emitted by the watcher when a rule in force
crosses its start or end. The watermark is `rule_effectivity_watch`; deleting it makes the next
tick start from now (crossings in between are not announced).

### Resolver cache
Push invalidation (LISTEN/NOTIFY) drops caches on pack changes; the TTL is the backstop.
Force it with `POST /v1/admin/resolver-cache:invalidate` on a replica.

## Verification

- `bash scripts/audit.sh` — static, suites on scratch databases (0 skips), live checks.
- `python deployments/scripts/e2e-jurisdiction-rules-deps.py` — every dependency edge live.
