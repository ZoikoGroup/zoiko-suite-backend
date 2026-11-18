# obligations-svc — Progress

**Status (18 Nov 2026):** Doc 03 §8.5 scope complete + WF-3 deadline clocks in progress.
Plan: `docs/audit_files/DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md` §D2-4.
The contract is `openapi.yaml` (validated with kin-openapi, route parity tested),
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
| **WF-3: Deadline clocks** | `obligation_clocks` (PLANNED/ACTIVE/DUE_SOON/DUE/OVERDUE/FULFILLED/WAIVED/NOT_APPLICABLE/SUPERSEDED); computed deadlines with jurisdiction-rules calendar provenance; satisfaction/waiver/N-A APIs |

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
  **WF-3 adds:** OBLIGATION_CLOCK_SATISFY, OBLIGATION_CLOCK_WAIVE, OBLIGATION_CLOCK_NOT_APPLICABLE.
- **Tenant isolation.** FORCE RLS with NULLIF on every table; the runtime role
  is NOBYPASSRLS and not the owner. The store suite runs as such a role.
- **Idempotency.** Obligation create on `(tenant, obligation_code)` and on
  `Idempotency-Key` + body hash; applicability decisions on `Idempotency-Key`
  (same body replays 200, a changed body 409 IDEMPOTENCY_MISMATCH); filing
  requirements on their natural key; status and filing moves are no-ops when
  repeated. **WF-3 adds:** clock satisfaction/waiver/N-A on `Idempotency-Key`.
- **Concurrency.** Row locks on every transition; If-Match / ETag on status,
  attributes (mandatory, 428 without), filing status and definition versions.
  **WF-3 adds:** clock transitions use If-Match on clock version.
- **Errors.** RFC 9457 problem+json, legacy `error` kept, §16 `error_class`.
- **Pagination.** Default 50, max 200; keyset cursor (`X-Next-Cursor`, `Link`).
  **WF-3 adds:** API-4 cursor paging on clocks list.

## Events

Transactional outbox (`event_outbox`), relayed by `internal/outbox`:
claimed `FOR UPDATE SKIP LOCKED`, exponential backoff (2^n s, cap 300 s),
parked only after 200 consecutive failures (~16 h); gauges
`outbox_pending_events`, `outbox_failed_events` (alert on > 0),
`outbox_oldest_pending_seconds`. Kafka key `<tenant>/<aggregate>`.
Topic `zoiko.obligations.events`: `obligation.created`, `obligation.updated`
(status, attribute, workflow link and filing changes, with `changed_fields`),
`obligation.overdue`, `obligation.closed`, `obligation.definition.changed`,
`obligation.entity_mapping.changed`.
**WF-3 adds:** `obligation.clock.planned`, `.activated`, `.due_soon`, `.due`,
`.overdue`, `.fulfilled`, `.waived`, `.superseded`. Parked events are requeued with
`PgStore.RequeueFailedOutbox` (or `UPDATE event_outbox SET status='PENDING',
attempts=0, failed_at=NULL, next_attempt_at=NOW() WHERE status='FAILED'`).

## Overdue

An in-process sweep (every minute) lists tenants with due OPEN / IN_PROGRESS
obligations under the SELECT-only `overdue_sweep_select` policy and transitions
each per tenant (events through the outbox). Before 000012 it found no tenants
under the runtime role and never ran.

**WF-3 replaces with clock-based sweep:** the clock worker evaluates every
ACTIVE clock against its computed deadline, transitions through DUE_SOON → DUE
→ OVERDUE, and publishes the corresponding clock events.

## Not in scope here

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

## Feature Matrix (OB-0 Baseline)

| ID | Requirement | Route / consumer | Test | Status |
|---|---|---|---|---|
| D2-4.F01 | Obligations CRUD | `POST /v1/obligations`, `GET /v1/obligations`, `GET …/{id}`, `PATCH …/{id}` | TestOB1_Create, TestOB1_Get, TestOB1_List, TestOB1_Update | DONE |
| D2-4.F02 | Status transitions | `POST …/{id}/status`; overdue sweep | TestOB1_StatusTransitions | DONE |
| D2-4.F03 | Link / unlink workflow | `POST/DELETE …/{id}/workflow` | TestOB1_WorkflowLink | DONE |
| D2-4.F04 | Filing requirements | `GET/POST …/{id}/filing-requirements`, `GET …/{fr}`, `POST …/{fr}/status`, `GET …/{fr}/transitions` | TestOB1_FilingReqs | DONE |
| D2-4.F05 | Obligation definitions | `GET/POST /v1/obligation-definitions`, `GET …/{id}`, `GET/POST …/{id}/versions`, `POST …/{id}/retire`; entity mappings `GET/POST …/{id}/entity-mappings`, `POST …/{m}/end` | TestOB1_Definitions, TestOB1_Mappings | DONE |
| D2-4.F06 | Applicability decisions | `GET/POST …/{id}/applicability-decisions`, `GET …/{id}/applicability` | TestOB2_* (idempotent, CHECK, exactly one decider) | IN PROGRESS |
| D2-4.F07 | Transactional outbox | `event_outbox`, relay, Kafka | TestOB1_Outbox | DONE |
| D2-4.F08 | Idempotency-Key, If-Match, RLS | All mutations | TestOB1_Idempotency, TestAPI3_* | DONE |
| D2-4.F09 | Registry check of legal entity | `GET /v1/entities/{id}/status` | TestOB3_RegistryCheckFailClosed | IN PROGRESS |
| D2-4.F10 | Single owner of obligation data (C8) | Cross-team | — | BLOCKED |
| D2-4.F11 | **WF-3: Deadline clocks** | Clock classes, computed deadlines, states, satisfy/waive/N-A, escalation, events | TestWF3_* | IN PROGRESS |
| D2-4.F12 | 22 partial rows + API-4 | Re-scored with evidence | OB-4 | PENDING |
| D2-4.F13 | Cursor paging (API-4) | All list endpoints | TestOB1_API4Paging | IN PROGRESS |
| D2-4.F14 | Dependency E2E + console v2.0 | `e2e-obligations-deps.py`, console | OB-5, OB-FE | PENDING |
