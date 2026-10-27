# obligations-svc — Progress

**Status (21 Oct 2026):** Doc 03 §8.5 scope complete. Group 2 completion pass;
the contract is `openapi.yaml` (validated with kin-openapi, route parity tested),
the collection is `postman/postman/ZoikoSuite_Obligations.postman_collection.json`.

## Owns (Doc 03 §8.5)

| Concern | Where |
|---|---|
| Obligation definitions | `obligation_definitions` + immutable `obligation_definition_versions` (000012); `/v1/obligation-definitions` |
| Obligation state and due dates | `obligations` (OPEN → IN_PROGRESS / OVERDUE / CLOSED; CLOSED terminal); `PATCH /v1/obligations/{id}` for due date, severity, responsible function |
| Obligation-to-entity mappings | `obligation_entity_mappings` (interval, one open mapping per definition and entity); an obligation raised from a definition must name a mapped entity and is pinned to the definition version |
| Obligation-to-workflow mappings | `workflow_id` (000007); `POST/DELETE /v1/obligations/{id}/workflow` |
| Source-origin references | `obligation_source_type/id`, `source_reference` (required, Atomic Linking) |
| Filing requirements | create, list, get; lifecycle PENDING → IN_PREPARATION → SUBMITTED → ACCEPTED / REJECTED, WITHDRAWN exit, append-only history |
| Applicability decisions | versioned, append-only (doc7 §E2); UNASSESSED is never stored |

## Controls

- **Entity- and jurisdiction-bound, fail-closed.** The legal entity is confirmed
  against tenant-entity-registry-svc (`GET /v1/entities/{id}/status`, in the
  caller's tenant; 422 otherwise, 503 if unreachable). The jurisdiction is
  confirmed against jurisdiction-rules-svc.
- **Identity.** `zoiko.io/identity-envelope` gate: the signed envelope is verified
  (JWKS) and its session must be live (introspection, AU-01); X-Principal-Id /
  X-Tenant-Id are then set from the verified claims. `IDENTITY_VERIFY` defaults
  to `require` outside local/dev; compose runs `warn` until every caller forwards
  the envelope. Outbound calls (authorization, jurisdiction, registry) forward it.
- **Authorization** per action, at the entity acted on, fail-closed:
  OBLIGATION_CREATE / _READ / _UPDATE / _STATUS_UPDATE, FILING_REQUIREMENT_CREATE /
  _READ / _STATUS_UPDATE, APPLICABILITY_DECISION_RECORD / _READ,
  OBLIGATION_ENTITY_MAPPING_MANAGE; the tenant-wide catalogue at PLATFORM scope
  (OBLIGATION_DEFINITION_MANAGE / _READ).
- **Tenant isolation.** FORCE RLS with NULLIF on every table; the runtime role
  is NOBYPASSRLS and not the owner. The store suite runs as such a role.
- **Idempotency.** Obligation create on `(tenant, obligation_code)` and on
  `Idempotency-Key` + body hash; applicability decisions on `Idempotency-Key`
  (same body replays 200, a changed body 409 IDEMPOTENCY_MISMATCH); filing
  requirements on their natural key; status and filing moves are no-ops when
  repeated.
- **Concurrency.** Row locks on every transition; If-Match / ETag on status,
  attributes (mandatory, 428 without), filing status and definition versions.
- **Errors.** RFC 9457 problem+json, legacy `error` kept, §16 `error_class`.
- **Pagination.** Default 50, max 200; keyset cursor (`X-Next-Cursor`, `Link`).

## Events

Transactional outbox (`event_outbox`), relayed by `internal/outbox`:
claimed `FOR UPDATE SKIP LOCKED`, exponential backoff (2^n s, cap 300 s),
parked only after 200 consecutive failures (~16 h); gauges
`outbox_pending_events`, `outbox_failed_events` (alert on > 0),
`outbox_oldest_pending_seconds`. Kafka key `<tenant>/<aggregate>`.
Topic `zoiko.obligations.events`: `obligation.created`, `obligation.updated`
(status, attribute, workflow link and filing changes, with `changed_fields`),
`obligation.overdue`, `obligation.closed`, `obligation.definition.changed`,
`obligation.entity_mapping.changed`. Parked events are requeued with
`PgStore.RequeueFailedOutbox` (or `UPDATE event_outbox SET status='PENDING',
attempts=0, failed_at=NULL, next_attempt_at=NOW() WHERE status='FAILED'`).

## Overdue

An in-process sweep (every minute) lists tenants with due OPEN / IN_PROGRESS
obligations under the SELECT-only `overdue_sweep_select` policy and transitions
each per tenant (events through the outbox). Before 000012 it found no tenants
under the runtime role and never ran.

## Not in scope here

- R-001 WFC-05 (clock classes, computed deadlines, DUE_SOON / FULFILLED /
  WAIVED): roadmap by decision of 21 Oct 2026; the 16 rows stay unscored.
- ComplianceStatus / ExceptionCase / EscalationRecord (Doc 04 §13.1): no spec
  section assigns them to this service.
- Single owner of "obligations" (C8): obligation-tracking-svc and
  filing-tracker-svc, outside this domain, still keep their own tables and
  obligation-tracking-svc emits the same event names.

## Deploy

1. Apply migration **000012** before the new binary (`deployments/migrate.py up`).
2. Set `TENANT_REGISTRY_URL` (required outside local/dev) and the `IDENTITY_*`
   variables (see `idenv.NewGateFromEnv`).
3. Register the new actions in access-control-svc (000021) and grant them.
4. Build from the repository root: `docker build -f services/obligations-svc/Dockerfile .`
