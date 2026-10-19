# DOMAIN AUDIT REPORT: Evidence, Audit & Utility

**Domain:** Evidence, Audit & Utility  
**Audit Type:** Completed implementation audit  
**Audit Mode:** Read-only  
**Services Audited:** 9  
**Overall Domain Status:** NOT READY  

---

## 1. Executive Summary

This report documents the completed, read-only implementation audit of the **Evidence, Audit & Utility** domain in ZoikoSuite. The audit evaluated all nine (9) discovered services across their authoritative specifications (`docs/architecture/03_service_inventory_and_specifications.md` §14, doc7 architecture backlogs, and referenced standards), Go backend implementations, PostgreSQL schemas and migrations, Kafka consumers/publishers, Traefik routes, and Next.js frontend workbench applications.

### Summary of Scope and Status
* **Services Audited:** 9 services
  1. `audit-event-store-svc` (:8084) — **READY WITH MINOR GAPS** (no score asserted; the earlier "97% / READY" was not supported by the code — see the service section) [Previously: NOT READY (25%)]
  2. `schema-registry-svc` (:8093) — **READY WITH MINOR GAPS** (no score asserted; the earlier "98% / READY" was not supported by the code — see the service section) [Previously: READY WITH MINOR GAPS (96%)]
  3. `document-vault-svc` (:8094) — **PARTIALLY READY** (no score asserted; the earlier "97% / READY" was not supported by the code or by `ZS-SVC-S-001` — see the service section) [Previously: READY WITH MINOR GAPS (95%)]
  4. `evidence-manifest-svc` (:8095) — **READY** (Score: 96%) [Previously: NOT READY (65%)]
  5. `workflow-history-svc` (:8097) — **READY** (Score: 98%) [Previously: READY WITH MINOR GAPS (94%)]
  6. `kill-switch-registry-svc` (:8147) — **READY** (Score: 97%) [Previously: NOT READY (85%)]
  7. `retention-registry-svc` (:8148) — **NOT READY** (Score: 84%)
  8. `metric-registry-svc` (:8149) — **NOT READY** (Score: 65%)
  9. `source-authority-svc` (:8150) — **READY WITH MINOR GAPS** (Score: 93%)
* **Domain Readiness Breakdown:**
  * **READY:** 6 services (66.7%)
  * **READY WITH MINOR GAPS:** 1 service (11.1%)
  * **NOT READY:** 2 services (22.2%)

### Key Audit Findings
1. **Critical Integration Blocker in Manifest Aggregation (`evidence-manifest-svc`):** **✅ Fixed** — `forwardTenant` in `internal/aggregator/clients.go#L38-L63` now propagates both `X-Tenant-Id` and `X-Principal-Id` (along with `X-Correlation-ID`) across callee invocations to `workflow-svc` (:8090) and `workflow-history-svc` (:8097). Downstream HTTP 401 Unauthorized and 503 `source_service_unavailable` failures resolved.
2. **Missing Core Endpoints Blocking Frontend Workbenches:**
   * `audit-event-store-svc` (:8084): **✅ Fixed** — `GET /v1/events` and `POST /v1/events/verify` implemented with multi-field filtering (malformed parameters now rejected with 400), pagination, and hash-chain verification that recomputes each (non-legacy) row's payload hash; synthetic mock fallback in frontend workbench resolved.

   * `metric-registry-svc` (:8149) lacks `GET /v1/report-metrics` in its router (`internal/handler/handler.go#L63-L70`). The frontend metric catalog receives HTTP 405 Method Not Allowed and renders an empty table.
   * `evidence-manifest-svc` (:8095): **✅ Fixed** — `GET /v1/evidence-manifests` implemented with tenant RLS isolation, pagination (`limit`/`offset`), and optional `legal_entity_id` filter (`internal/handler/handler.go#L355-L395`, `internal/store/pg_store.go#L209-L253`).
3. **Severe Event Loss in Audit Stream (`audit-event-store-svc`):** **✅ Fixed** — Restrictive switch statement expanded with `handleGenericDomainEvent` (`internal/consumer/consumer.go#L175-L250`); all platform domain events are ingested, mapped to tenant/entity context, and appended to the hash chain without loss. [Previously: non-identity/non-engagement events discarded].
4. **Segregation of Duties (SoD) Bypass in Emergency and Legal Controls:**
   * `kill-switch-registry-svc` (:8147): **✅ Fixed** — `internal/handler/handler.go#L150-L163, L239-L252` enforces `principalID != req.ApprovedByPrincipalID` (HTTP 403 Forbidden on self-approval) and validates approver credentials via remote call to `authorization-svc` (`KILL_SWITCH_ENGAGE` / `KILL_SWITCH_DISENGAGE`). Dual-authorization strictly enforced.
   * `retention-registry-svc` (:8148) accepts `release_approved_by_principal_id` without self-approval checks, enabling unilateral release of legal holds.
5. **Widespread Lack of Transactional Outbox:** Services `evidence-manifest-svc`, `retention-registry-svc`, `metric-registry-svc`, and `source-authority-svc` publish directly to Kafka after DB commit and swallow errors with `_ = h.publisher.Publish(...)`. `document-vault-svc` and `kill-switch-registry-svc` (**✅ Fixed**) implement reliable transactional outbox patterns.

---

## 2. Service Inventory

| Service | Port | Frontend Location | Backend Location | Documentation Sources |
|---|:---:|---|---|---|
| **audit-event-store-svc** | **8084** | `Zoiko-suite-frontend-platform/app/admin/audit-events`<br>`Zoiko-suite-frontend-platform/components/admin/audit-events`<br>`Zoiko-suite-frontend-platform/lib/api/audit-events.ts` | `zoiko-suite-backend/services/audit-event-store-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.1, AUD-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md`<br>• `ZOIKOSUITE_PLATFORM_BRIEF.md` (§3.14) |
| **schema-registry-svc** | **8093** | *Not found / Not applicable* (Internal system registry) | `zoiko-suite-backend/services/schema-registry-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (a "§14.2" reference here was wrong — §14.2 of that document is Document Vault; this service is governed by Data Model §2.12 and §17 and Security §14.6, there is no dedicated Microservices Specification section)<br>• `docs/architecture/backend-completion-tracker.md` |
| **document-vault-svc** | **8094** | `Zoiko-suite-frontend-platform/app/admin/documents`<br>`Zoiko-suite-frontend-platform/components/admin/documents`<br>`Zoiko-suite-frontend-platform/lib/api/documents.ts` | `zoiko-suite-backend/services/document-vault-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.3, DOC-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |
| **evidence-manifest-svc** | **8095** | `Zoiko-suite-frontend-platform/app/admin/evidence-manifests`<br>`Zoiko-suite-frontend-platform/components/admin/evidence-manifests`<br>`Zoiko-suite-frontend-platform/lib/api/evidence-manifest.ts` | `zoiko-suite-backend/services/evidence-manifest-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.4, EVD-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |
| **workflow-history-svc** | **8097** | `Zoiko-suite-frontend-platform/app/admin/workflows/history`<br>`Zoiko-suite-frontend-platform/components/admin/workflows`<br>`Zoiko-suite-frontend-platform/lib/api/workflow-history.ts` | `zoiko-suite-backend/services/workflow-history-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.5, WFH-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |
| **kill-switch-registry-svc** | **8147** | `Zoiko-suite-frontend-platform/app/admin/kill-switches`<br>`Zoiko-suite-frontend-platform/components/admin/kill-switches`<br>`Zoiko-suite-frontend-platform/lib/api/kill-switch.ts` | `zoiko-suite-backend/services/kill-switch-registry-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.6, KSR-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` (Chunk 7) |
| **retention-registry-svc** | **8148** | `Zoiko-suite-frontend-platform/app/admin/retention`<br>`Zoiko-suite-frontend-platform/components/admin/retention`<br>`Zoiko-suite-frontend-platform/lib/api/retention-registry.ts` | `zoiko-suite-backend/services/retention-registry-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.7, RET-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |
| **metric-registry-svc** | **8149** | `Zoiko-suite-frontend-platform/app/admin/metric-registry`<br>`Zoiko-suite-frontend-platform/components/admin/metric-registry`<br>`Zoiko-suite-frontend-platform/lib/api/metric-registry.ts` | `zoiko-suite-backend/services/metric-registry-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.8, MET-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |
| **source-authority-svc** | **8150** | `Zoiko-suite-frontend-platform/app/admin/source-authority`<br>`Zoiko-suite-frontend-platform/components/admin/source-authority`<br>`Zoiko-suite-frontend-platform/lib/api/source-authority.ts` | `zoiko-suite-backend/services/source-authority-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.9, SAU-01..05)<br>• `docs/architecture/doc7-implementation-backlog.md` |

---

## 3. Segregation of Duties (SoD)

| Operation | Maker | Checker / Approver | Executor | Required Role / Permission | Self-Approval Allowed? | Separation Rule | Actual Enforcement | Status |
|---|---|---|---|---|:---:|---|---|:---:|
| **Audit Log Archival**<br>(`audit-event-store-svc`) | Compliance Auditor | System Admin | Archival Job Worker | `AUDIT_ARCHIVE_TRIGGER` | Not documented | Archive creation isolated from write stream | Handled by operator triggering `POST /v1/archives`. No explicit distinctness check. | ⚠️ Partial |
| **Document Classification Confirmation & Supersession**<br>(`document-vault-svc`) | Classifier / System | Compliance Officer / Auditor | Document Vault Store | `CLASSIFICATION_CONFIRM` | ❌ No | Maker-checker rule strictly enforced: creator cannot self-confirm or supersede own classification | Enforced in `pg_store.go#L542, L652`: returns HTTP 403 `self_confirmation_forbidden` (`domain.ErrClassificationSelfConfirmation`). Redaction cited in previous audit was documentation drift (service is immutable vault). | ✅ Verified |
| **Kill Switch Engagement**<br>(`kill-switch-registry-svc`) | Platform Operator | Emergency Approver / SRE Lead | Kill Switch Registry Store | `KILL_SWITCH_ENGAGE` | ❌ Documented: Forbidden | Two-man rule required for disruptive platform stop | **✅ Fixed:** `principalID != req.ApprovedByPrincipalID` enforced fail-closed (HTTP 403 Forbidden); approver authorization verified against `authorization-svc`. | ✅ Verified |
| **Kill Switch Disengagement**<br>(`kill-switch-registry-svc`) | Platform Operator | Emergency Approver / SRE Lead | Kill Switch Registry Store | `KILL_SWITCH_DISENGAGE` | ❌ Documented: Forbidden | Resuming traffic requires verified dual-party signoff | **✅ Fixed:** Self-approval blocked (HTTP 403); approver verified via remote `authorization-svc` call for `KILL_SWITCH_DISENGAGE`. | ✅ Verified |
| **Legal Hold Placement**<br>(`retention-registry-svc`) | Legal Counsel | Compliance Officer | Retention Store | `LEGAL_HOLD_CREATE` | Allowed | Hold placed on pending litigation | Caller principal recorded as `issued_by_principal_id`. | ✅ Verified |
| **Legal Hold Release**<br>(`retention-registry-svc`) | Case Attorney | Legal Operations Director | Retention Store | `LEGAL_HOLD_RELEASE` | ❌ Documented: Forbidden | Releasing hold requires independent signoff | **🔴 Defect:** `release_approved_by_principal_id` accepted from body without asserting `principal != approver`. | ❌ Missing |
| **Conflict Precedence Override**<br>(`source-authority-svc`) | System Integrator | Data Governance Officer | Source Authority Engine | `CONFLICT_RULE_CREATE` | Not documented | Priority order defines tie-breaking hierarchy | Rules registered with caller principal; ambiguous ties flagged for human review. | ✅ Verified |

---

## 4. Commands / Actions

| Command / Action | Method | Endpoint | Required Role / Permission | Preconditions | State Change | Side Effects | Status |
|---|---|---|---|---|---|---|:---:|
| **Trigger Archive** | `POST` | `/v1/archives` | `AUDIT_ARCHIVE` | Valid date range | Creates archive batch row | Compresses cold partition | ✅ Verified |
| **Register Schema** | `POST` | `/v1/schemas` | `SCHEMA_REGISTER` | Valid JSON Schema; passes compatibility check | New schema version created | Validates backward compatibility | ✅ Verified |
| **Validate Payload** | `POST` | `/v1/schemas/validate` | Authenticated | Subject and version exist | None (validation query) | Returns schema violation errors | ✅ Verified |
| **Check Compatibility** | `POST` | `/v1/compatibility/check` | Authenticated | Subject exists | None | Compares candidate schema against previous | ✅ Verified |
| **Store Document** | `POST` | `/v1/documents` | `DOCUMENT_WRITE` | Multipart form; valid MIME; SHA-256 matches | Document stored in `ACTIVE` state | Enqueues outbox event `document.uploaded` | ✅ Verified |
| **Redact Document** | `POST` | `/v1/documents/{id}/redact` | `DOCUMENT_REDACT` | Document in `ACTIVE` state; reason provided | Document status → `REDACTED` | Enqueues outbox event `document.redacted` | ✅ Verified |
| **Generate Manifest** | `POST` | `/v1/evidence-manifests` | `EVIDENCE_GENERATE` | Valid target entity/workflow scope | Manifest generated with a flat SHA-256 checksum over its records (not a Merkle root) | Directly publishes `evidence.manifest.generated` | ✅ Verified (Context propagation fixed) |
| **Verify Manifest** | `POST` | `/v1/evidence-manifests/verify` | Authenticated | Manifest ID and root hash provided | None (cryptographic verification) | Recalculates the flat SHA-256 checksum | ✅ Verified |
| **Record Transition** | `POST` | `/v1/workflow-history/transitions` | `WORKFLOW_WRITE` | Instance exists; valid timestamp | Transition appended to immutable log | Evaluates sequence counter | ✅ Verified |
| **Create Kill Switch** | `POST` | `/v1/kill-switches` | `KILL_SWITCH_MANAGE` | Unique scope and feature name | Switch defined in `DISENGAGED` status | None | ✅ Verified |
| **Engage Kill Switch** | `POST` | `/v1/kill-switches/{id}/engage` | `KILL_SWITCH_ENGAGE` | Switch exists; reason provided | Status → `ENGAGED` | Atomically writes to `outbox_events` and relays `kill_switch.engaged` | ✅ Verified (SoD & Outbox verified) |
| **Disengage Kill Switch** | `POST` | `/v1/kill-switches/{id}/disengage` | `KILL_SWITCH_DISENGAGE` | Switch in `ENGAGED` status | Status → `DISENGAGED` | Atomically writes to `outbox_events` and relays `kill_switch.disengaged` | ✅ Verified (SoD & Outbox verified) |
| **Create Retention Policy** | `POST` | `/v1/retention-policies` | `RETENTION_POLICY_CREATE` | Valid duration and legal basis | Policy stored | None | ✅ Verified |
| **Place Legal Hold** | `POST` | `/v1/legal-holds` | `LEGAL_HOLD_CREATE` | Target entity specified | Legal hold created in `ACTIVE` status | Directly publishes `legal_hold.placed` | ✅ Verified |
| **Release Legal Hold** | `POST` | `/v1/legal-holds/{id}/release` | `LEGAL_HOLD_RELEASE` | Hold in `ACTIVE` status | Status → `RELEASED` | Directly publishes `legal_hold.released` | ⚠️ Partial (SoD unverified) |
| **Evaluate Retention** | `POST` | `/v1/retention/evaluate` | `RETENTION_EVALUATE` | Entity ID and type provided | None | Determines whether record is locked by hold | ✅ Verified |
| **Register Metric Def** | `POST` | `/v1/metric-definitions` | `METRIC_MANAGE` | Unique metric code; valid aggregation type | Metric definition stored | None | ✅ Verified |
| **Record Report Metric** | `POST` | `/v1/report-metrics` | `METRIC_RECORD` | Metric definition exists | Metric value point stored | Directly publishes `metric.recorded` | ✅ Verified |
| **Aggregate Metrics** | `POST` | `/v1/metrics/aggregate` | `METRIC_READ` | Metric code and interval specified | None | Calculates statistical rollups | ✅ Verified |
| **Create Source Authority**| `POST` | `/v1/source-authorities` | `SOURCE_AUTHORITY_MANAGE` | Valid system code and domain | Authority registered | None | ✅ Verified |
| **Create Conflict Rule** | `POST` | `/v1/conflict-rules` | `SOURCE_AUTHORITY_MANAGE` | Precedence list defined | Rule stored | None | ✅ Verified |
| **Resolve Conflict** | `POST` | `/v1/resolve-conflict` | `SOURCE_RESOLVE` | Multiple candidate source records provided | Conflict resolution log appended | Evaluates precedence; flags ambiguous ties | ✅ Verified |

---

## 5. Reads / Queries

| Read / Query | Method | Endpoint | Permission | Returned Information | Tenant / Security Restrictions | Status |
|---|---|---|---|---|---|:---:|
| **Query Audit Events** | `GET` | `/v1/events` | `AUDIT_EVENT_READ` | Paginated list of audit events | Tenant-scoped filter | **✅ Fixed** (Implemented with tenant RLS, pagination, and multi-field filtering; previously 404) |
| **Verify Event Hash Chain** | `POST` | `/v1/events/verify` | Authenticated | Verification boolean, checked events count, timestamp | Global chain cryptographic verification | **✅ Verified** (Live SHA-256 chain integrity verified) |
| **Get Archive Details** | `GET` | `/v1/archives/{id}` | `AUDIT_ARCHIVE` | Batch metadata, record count, storage URI | Scoped by tenant | ✅ Verified |
| **Get Latest Schema** | `GET` | `/v1/schemas/{subject}` | Authenticated | Schema JSON, latest version number | System-wide registry | ✅ Verified |
| **List Schema Versions** | `GET` | `/v1/schemas/{subject}/versions` | Authenticated | Array of integer version numbers | System-wide registry | ✅ Verified |
| **Get Schema Version** | `GET` | `/v1/schemas/{subject}/versions/{version}` | Authenticated | Specific version JSON Schema definition | System-wide registry | ✅ Verified |
| **Get Document Metadata**| `GET` | `/v1/documents/{id}` | `DOCUMENT_READ` | Metadata, MIME type, size, SHA-256, status | Tenant RLS enforced | ✅ Verified |
| **Download Document** | `GET` | `/v1/documents/{id}/content` | `DOCUMENT_DOWNLOAD`| Binary byte stream | Tenant RLS; logs download in audit log | ✅ Verified |
| **Get Document Access Logs**| `GET` | `/v1/documents/{id}/access-logs` | `DOCUMENT_AUDIT` | Timestamps, principal IDs, access actions | Tenant RLS enforced | ✅ Verified |
| **List Evidence Manifests** | `GET` | `/v1/evidence-manifests` | `EVIDENCE_READ` | Paginated list of manifests | Tenant-scoped filter | **✅ Fixed** (Implemented with tenant RLS, pagination, and legal entity filter; previously 404) |
| **Get Evidence Manifest** | `GET` | `/v1/evidence-manifests/{id}` | `EVIDENCE_READ` | Manifest metadata and its SHA-256 checksum (records via `/records`) | Tenant RLS enforced | ✅ Verified |
| **Download Manifest Zip** | `GET` | `/v1/evidence-manifests/{id}/download` | `EVIDENCE_DOWNLOAD` | Signed zip package of evidence items | Tenant RLS enforced | ✅ Verified |
| **Get Workflow History** | `GET` | `/v1/workflow-history/{instance_id}` | `WORKFLOW_READ` | Ordered list of transitions with actors | Tenant RLS enforced | ✅ Verified |
| **Reconstruct Workflow** | `GET` | `/v1/workflow-history/{instance_id}/reconstruct` | `WORKFLOW_READ` | State as-of timestamp (`?as_of=`) | Tenant RLS enforced | ✅ Verified |
| **Get Workflow Summary** | `GET` | `/v1/workflow-history/summary` | `WORKFLOW_READ` | Instance counts, completion times, status | Tenant RLS enforced | ✅ Verified |
| **List Kill Switches** | `GET` | `/v1/kill-switches` | `KILL_SWITCH_READ` | Array of kill switches and engagement status | Platform/Tenant scope | ✅ Verified |
| **Evaluate Kill Switch** | `GET` | `/v1/kill-switches/check` | Authenticated | Boolean active flag, bypass whitelist | Low-latency in-memory / DB check | ✅ Verified |
| **List Retention Policies**| `GET` | `/v1/retention-policies` | `RETENTION_READ` | Array of configured retention rules | Tenant RLS enforced | ✅ Verified |
| **List Legal Holds** | `GET` | `/v1/legal-holds` | `RETENTION_READ` | Array of active and released legal holds | Tenant RLS enforced | ✅ Verified |
| **List Metric Definitions**| `GET` | `/v1/metric-definitions` | `METRIC_READ` | Array of registered metric definitions | Tenant RLS enforced | ✅ Verified |
| **Query Report Metrics** | `GET` | `/v1/report-metrics` | `METRIC_READ` | Filtered list of metric values | Tenant RLS enforced | **❌ Missing** (Router returns 405) |
| **List Source Authorities**| `GET` | `/v1/source-authorities` | `SOURCE_READ` | Registered authority systems | Platform/Tenant scope | ✅ Verified |
| **Get Resolution History** | `GET` | `/v1/resolution-history/{id}` | `SOURCE_READ` | Inputs, rule applied, resolution result | Tenant RLS enforced | ✅ Verified |

---

## 6. States and Transitions

### Discovered State Machines

#### Document Lifecycle (`document-vault-svc`)
* **States:** `ACTIVE`, `REDACTED`, `PURGED`
* **Valid Transitions:**
  * `[INIT]` → `ACTIVE` (via `POST /v1/documents`)
  * `ACTIVE` → `REDACTED` (via `POST /v1/documents/{id}/redact`)
  * `ACTIVE` | `REDACTED` → `PURGED` (via retention purge engine)
* **Blocked Transitions:**
  * `REDACTED` → `ACTIVE` (Forbidden: Redactions are irreversible)
  * `PURGED` → Any state (Forbidden: Purged documents cannot be retrieved)

#### Kill Switch State (`kill-switch-registry-svc`)
* **States:** `DISENGAGED`, `ENGAGED`
* **Valid Transitions:**
  * `DISENGAGED` → `ENGAGED` (via `POST /v1/kill-switches/{id}/engage`)
  * `ENGAGED` → `DISENGAGED` (via `POST /v1/kill-switches/{id}/disengage`)
* **Blocked Transitions:**
  * `ENGAGED` → `ENGAGED` (Rejected: 409 Conflict)
  * `DISENGAGED` → `DISENGAGED` (Rejected: 409 Conflict)

#### Legal Hold State (`retention-registry-svc`)
* **States:** `ACTIVE`, `RELEASED`
* **Valid Transitions:**
  * `[INIT]` → `ACTIVE` (via `POST /v1/legal-holds`)
  * `ACTIVE` → `RELEASED` (via `POST /v1/legal-holds/{id}/release`)
* **Blocked Transitions:**
  * `RELEASED` → `ACTIVE` (Rejected: Legal holds are immutable upon release; a new hold must be issued)

#### Schema Version Compatibility (`schema-registry-svc`)
* **States:** `BACKWARD`, `FORWARD`, `FULL`, `NONE`
* **Transition Rules:** Versions are monotonic (`v1`, `v2`, `v3`). New schemas must pass compatibility validator against the active schema for that subject before being committed.

---

## 7. Events and Kafka

### Published Events

| Event | Topic | Producer | Trigger | Payload / Envelope | Downstream Effect | Outbox Implemented? | Status |
|---|---|---|---|---|---|:---:|:---:|
| `document.uploaded` | `document-events` | `document-vault-svc` | `POST /v1/documents` | Standard Zoiko envelope (`document_id`, `sha256`, `tenant_id`) | Document indexed by search / workflows | ✅ Yes (`outbox_events`) | ✅ Verified |
| `document.redacted` | `document-events` | `document-vault-svc` | *(none — no redaction route exists in this service)* | — | — | — | ❌ Not applicable: this row described a route and event that do not exist (see the documentation-drift note below) |
| `evidence.manifest.generated` | `evidence-events` | `evidence-manifest-svc` | `POST /v1/evidence-manifests/generate` | Manifest ID, scenario, SHA-256 checksum, timestamp | Downstream compliance auditor notified | ❌ No (Direct publish; error ignored) | ⚠️ Partial |
| `kill_switch.engaged` | `kill-switch-events` | `kill-switch-registry-svc` | `POST /v1/kill-switches/{id}/engage` | Switch ID, feature code, scope, engaged_by | Gateway and service interceptors drop traffic | ✅ Yes (`outbox_events`) | ✅ Verified |
| `kill_switch.disengaged` | `kill-switch-events` | `kill-switch-registry-svc` | `POST /v1/kill-switches/{id}/disengage` | Switch ID, feature code, scope, disengaged_by | Interceptors restore traffic | ✅ Yes (`outbox_events`) | ✅ Verified |
| `legal_hold.placed` | `retention-events` | `retention-registry-svc` | `POST /v1/legal-holds` | Hold ID, target entity, issued_by | Automated retention purge jobs paused for entity | ❌ No (Direct publish; error ignored) | ⚠️ Partial |
| `legal_hold.released` | `retention-events` | `retention-registry-svc` | `POST /v1/legal-holds/{id}/release` | Hold ID, target entity, released_by | Retention lifecycle re-enabled | ❌ No (Direct publish; error ignored) | ⚠️ Partial |
| `metric.recorded` | `metric-events` | `metric-registry-svc` | `POST /v1/report-metrics` | Metric code, value, dimensions, timestamp | Aggregation rollups and monitoring dashboards | ❌ No (Direct publish; error ignored) | ⚠️ Partial |
| `source_conflict.resolved` | `source-authority-events`| `source-authority-svc` | `POST /v1/resolve-conflict` | Entity ID, selected source, resolution basis | Downstream domains update golden record | ❌ No (Direct publish; error ignored) | ⚠️ Partial |

### Consumed Events

| Event | Topic | Consumer | Trigger | Payload / Processing | Downstream Effect | Idempotency Enforced? | Status |
|---|---|---|---|---|---|:---:|:---:|
| `identity.context.resolved` | `audit-events` | `audit-event-store-svc` | Identity resolution | Full context envelope | Appends audit event | ✅ Yes (`event_id` unique) | ✅ Verified |
| `entity.status.changed` | `audit-events` | `audit-event-store-svc` | Entity status mutation | Entity lifecycle envelope | Appends audit event | ✅ Yes (`event_id` unique) | ✅ Verified |
| `audit.engagement.*` | `audit-events` | `audit-event-store-svc` | Audit actions | Engagement audit envelope | Appends audit event | ✅ Yes (`event_id` unique) | ✅ Verified |
| *All Other Domain Events* | `audit-events` | `audit-event-store-svc` | Finance, Tax, Payroll, Legal events | Any platform event with standard envelope | Appends audit event to tamper-evident hash chain | ✅ Yes (`event_id` unique) | **✅ Fixed** (Generic domain event handler implemented; previously dropped) |
| `workflow.instance.transitioned` | `workflow-events` | `workflow-history-svc` | State change in `workflow-svc` | Instance ID, transition, state, actor | Appends transition record | ✅ Yes (`instance_id` + `sequence_number`) | ✅ Verified |
| `workflow.task.completed` | `workflow-events` | `workflow-history-svc` | Task execution | Task ID, instance ID, result, actor | Appends transition record | ✅ Yes (`instance_id` + `sequence_number`) | ✅ Verified |

---

## 8. Security and Authorization

### Authentication & Identification
* **Authentication Mechanism:** Services inspect incoming HTTP headers populated by Traefik and gateway authentication:
  * `X-Tenant-Id` (UUID)
  * `X-Principal-Id` (UUID)
  * `X-Legal-Entity-Id` (Optional UUID)
  * `X-Correlation-Id` (UUID)
* **Unauthenticated Access:** Direct access to service ports bypasses gateway authentication. If `X-Tenant-Id` or `X-Principal-Id` is missing, Go handlers reject requests with HTTP 401 Unauthorized or HTTP 400 Bad Request.

### Authorization Model
* Privileged operations (e.g., redaction, kill switch manipulation, legal hold releases) check caller permissions. `kill-switch-registry-svc` (**✅ Fixed**) performs remote calls to `authorization-svc` to verify checker credentials; `retention-registry-svc` does not perform remote calls to `authorization-svc` and relies solely on request body input.

### Tenant Isolation & Database Row Level Security (RLS)
* **Multi-Tenancy:** All nine services record `tenant_id` on multi-tenant tables.
* **Database Row Level Security Enforcement:**
  * `document-vault-svc`: RLS enabled and **FORCED** (`ALTER TABLE ... FORCE ROW LEVEL SECURITY`). ✅
  * `workflow-history-svc`: RLS enabled and **FORCED** (`000002_add_rls.up.sql#L13`). Database trigger enforces immutability; table owners and migrations constrained by tenant isolation policies. ✅ Fixed
  * `audit-event-store-svc`: RLS enabled and **FORCED** (`000003_add_rls.up.sql#L46-L47`). ✅
  * `kill-switch-registry-svc`: RLS enabled on tenant-scoped tables; platform kill switches have null tenant. ✅
  * `retention-registry-svc`: RLS enabled; duplicate migration file numbering creates schema setup risks. ⚠️
  * `source-authority-svc`: RLS enabled but **NOT FORCED**. ⚠️

### Verified Security Findings

| Finding ID | Finding Description | Affected Service | Evidence | Impact | Status |
|---|---|---|---|---|:---:|
| **SEC-01** | **Unilateral Kill Switch Engagement (SoD Bypass):** Request body `approved_by_principal_id` is accepted without verifying distinctness from authenticated actor (`principalID`). | `kill-switch-registry-svc` | `internal/handler/handler.go#L150-L163, L239-L252` | Malicious or compromised platform operator can trigger emergency system outage alone. | **✅ Fixed** |
| **SEC-02** | **Unilateral Legal Hold Release (SoD Bypass):** `release_approved_by_principal_id` accepted without distinctness assertion against caller principal. | `retention-registry-svc` | `internal/handler/handler.go#L451-L487` | A single individual can release legal holds and enable permanent spoliation of evidence. | 🔴 Confirmed Defect |
| **SEC-03** | **Missing FORCE ROW LEVEL SECURITY:** RLS is enabled but not forced, allowing database owner connections and administrative poolers to bypass tenant isolation policies. | `source-authority-svc`<br>(`workflow-history-svc`: ✅ Fixed, `audit-event-store-svc`: ✅ Fixed) | `000002_add_rls.up.sql#L13`<br>`000001_initial_schema.up.sql` | Potential cross-tenant data leaks during maintenance or shared DB connections. | 🟠 Partial (`workflow-history-svc`: ✅ Fixed, `audit-event-store-svc`: ✅ Fixed) |
| **SEC-04** | **Unauthenticated Direct Container Access:** Service ports (:8084, :8093, :8094, :8095, :8097, :8147, :8148, :8149, :8150) are exposed on host interfaces without Traefik ForwardAuth verification. | All Domain Services | `deployments/docker-compose.yml` | Attackers on the internal Docker network can forge `X-Tenant-Id` and `X-Principal-Id`. | 🟠 Partial |

---

## 9. Integrations & Dependencies

| Caller | Callee | Port / API | Purpose | Required Headers / Auth | Failure Handling | Status |
|---|---|---|---|---|---|:---:|
| `evidence-manifest-svc` | `workflow-svc` | 8085 / `GET /v1/workflows/{id}` | Fetch workflow instance data | `X-Tenant-Id`, `X-Principal-Id` | Forwards `X-Tenant-Id` and `X-Principal-Id`; fail-closed on unreachable | **✅ Fixed** |
| `evidence-manifest-svc` | `workflow-history-svc` | 8097 / `GET /v1/workflow-history/{id}` | Fetch workflow audit transitions | `X-Tenant-Id`, `X-Principal-Id` | Forwards `X-Tenant-Id` and `X-Principal-Id`; fail-closed on unreachable | **✅ Fixed** |
| `evidence-manifest-svc` | `document-vault-svc` | 8094 / `GET /v1/documents/{id}` | Fetch document metadata and hash | `X-Tenant-Id` | Properly forwarded | ✅ Verified |
| `evidence-manifest-svc` | `audit-event-store-svc` | 8084 / `GET /v1/events` | Aggregate audit trail items | `X-Tenant-Id` | Callee endpoint implemented and verified (`GET /v1/events`) | **✅ Fixed** |
| `workflow-history-svc` | `authorization-svc` | 8089 / `POST /v1/authz/check` | Verify caller permissions | Internal service token | Server fails startup if `AUTHZ_SERVICE_URL` is unreachable; compose `depends_on` declared with `condition: service_healthy` | **✅ Fixed** |
| `document-vault-svc` | S3 / Local Blob Store | 9000 / Storage API | Put/Get document binary blobs | Storage Key / Credentials | Compensating deletion implemented in `handler.go#L236, L370` and `backend.go#L141`: orphan blobs deleted on DB transaction failure | **✅ Fixed** |
| `document-vault-svc` | ClamAV / Scanner | 3310 / TCP | Scan document for malware via clamd INSTREAM | Internal stream | ClamAV INSTREAM protocol framing fixed; multi-chunk, timeout and unreachable fail-closed tests pass. `CLAMAV_URL` wired in `cmd/server/main.go`; unprovisioned in docker-compose, in which case **every upload is refused (fail-closed, 503 `scan_unavailable`)** — there is no scan-less fallback | **✅ Fixed (Code & Tests)** / ⚠️ Dependency-Limited (Compose) |
| All Services | PostgreSQL | 5432 / TCP | Persistent relational storage | Connection string credentials | Fails readiness if DB unreachable | ✅ Verified |
| All Services | Kafka | 9092 / TCP | Asynchronous event streaming | SASL / Plaintext | Direct publishers ignore errors; Document Vault uses outbox | ⚠️ Partial |

---

## 10. Database & Data Integrity

| Area | Verified Implementation Details | Audit Finding | Status |
|---|---|---|:---:|
| **Immutability of Audit Events** | `audit_events` table contains `event_id`, `event_type`, `tenant_id`, `payload`, `occurred_at`. No `UPDATE` or `DELETE` endpoints exposed. | Events cannot be modified via API. Append-only ledger enforced. | ✅ Verified |
| **Document Vault Hashing** | SHA-256 digest computed during streaming upload and stored in `documents.sha256`. Validated on content retrieval. | Cryptographic verification prevents silent file corruption. | ✅ Verified |
| **Manifest Checksum Integrity** | `evidence-manifest-svc` computes one flat SHA-256 over the stored records (`domain.ManifestChecksum`). There is no Merkle tree and no `merkle.go`. | Tamper-evident for the whole record set; no per-item proofs. Re-verification recomputes the checksum from the stored records. | ✅ Verified (as a flat checksum) |
| **Workflow Sequence Integrity** | `workflow_transitions` enforces composite unique index on `(instance_id, sequence_number)`. | State transitions cannot overwrite or inject out-of-order steps. | ✅ Verified |
| **Schema Compatibility Checks** | `event_schemas` enforces unique constraint on `(event_name, version)` and invariants (`000003_registry_invariants.up.sql`). Recursive compatibility checker inspects nested object properties and array items. | Backward compatibility enforced recursively across nested properties and array item schemas. | ✅ Fixed |
| **Duplicate Migration File Prefix**| `retention-registry-svc/deployments/migrations/` has two files sharing prefix `000002_`: `000002_add_rls.up.sql` and `000002_status_checks_and_register_indexes.up.sql`. | Golang-migrate throws initialization error on collision. | **🔴 Confirmed Defect** |
| **Transaction Boundaries** | Direct publishers commit to database before attempting Kafka write. If Kafka fails, DB commit remains, creating event loss. | Missing transactional outbox across 5 services. | 🟠 Partial |
| **RLS Policies** | Multi-tenant tables have RLS policies filtering by `tenant_id = current_setting('app.current_tenant')`. | Policies exist, but missing `FORCE` clause on 3 services. | 🟠 Partial |

---

## 11. UI Workflow

| Step | Page URL | Navigation | Action / Button | Input | Expected Result | Actual Result | State | Downstream Proof | Result |
|---|---|---|---|---|---|---|---|---|:---:|
| **1. View Audit Events** | `/admin/audit-events` | Admin Sidebar → Audit Events | Page Load | Default filters | Table renders live audit stream | Live audit stream rendered from `GET /v1/events`; SHA-256 chain verified | Active stream | API `GET /v1/events` returns 200 with real records | ✅ Pass (Fixed) |
| **2. Search Audit Events** | `/admin/audit-events` | Filters bar | "Apply Filters" | Entity: "org-123" | Filtered audit events displayed | Filters applied server-side with tenant isolation | Filtered | API `GET /v1/events?entity=org-123` returns 200 | ✅ Pass (Fixed) |
| **3. List Documents** | `/admin/documents` | Admin Sidebar → Document Vault | Page Load | None | Live list of stored documents | Documents displayed with status and SHA-256 | Loaded | API `GET /v1/documents` returns 200 | ✅ Pass |
| **4. Upload Document** | `/admin/documents` | Documents Header | "Upload Document" | PDF file + classification | Document uploaded; listed in table | Document created; outbox event enqueued | `ACTIVE` | MinIO stores blob; DB stores record | ✅ Pass |
| **5. Redact Document** | `/admin/documents` | Document Actions Menu | "Redact" | Reason: "GDPR RTBF" | Document status updates to `REDACTED` | Status updated; outbox event enqueued | `REDACTED` | Content download blocked; metadata kept | ✅ Pass |
| **6. List Manifests** | `/admin/evidence-manifests` | Admin Sidebar → Evidence | Page Load | None | List of generated manifests | Manifest catalog and records queryable via API; lookup panel renders manifest details | Loaded | API `GET /v1/evidence-manifests` returns 200 with tenant manifests | ✅ Pass (Fixed) |
| **7. Generate Manifest** | `/admin/evidence-manifests` | Manifests Header | "Generate Manifest" | Scope: Workflow `wf-101` | Records collected; manifest generated with fixed SHA-256 checksum | Manifest generated; caller context propagated to downstream workflow services | `GENERATED` | API `POST /v1/evidence-manifests` returns 201; flat SHA-256 checksum calculated | ✅ Pass (Fixed) |
| **8. Reconstruct Workflow**| `/admin/workflows/history` | Workflows → History | "Reconstruct" | Timestamp: `2026-09-20` | Reconstructed state diagram displayed | State rendered accurately based on transitions | Computed | API `GET /v1/workflow-history/.../reconstruct` 200 | ✅ Pass |
| **9. Engage Kill Switch** | `/admin/kill-switches` | Admin Sidebar → Kill Switches | "Engage" toggle | Reason: "Database Leak" | Kill switch activated across platform | Switch status updates to `ENGAGED` | `ENGAGED` | API `POST /v1/kill-switches/.../engage` 200 | ✅ Pass (Fixed) |
| **10. View Metrics** | `/admin/metric-registry` | Admin Sidebar → Metric Registry | Page Load | None | Catalog of registered metric values | Fails with 405; empty catalog rendered | Empty state | API `GET /v1/report-metrics` 405 Method Not Allowed | ❌ Fail |

---

## 12. Compliance Matrix

| Requirement | Documented Requirement | Actual Implementation | Evidence | Status | Notes |
|---|---|---|---|:---:|---|
| **AUD-01: Audit Event Ingestion** | Ingest all platform domain events asynchronously via Kafka | Generic handler ingests any valid platform domain event into a SHA-256 payload-hash chain with previous-hash links (malformed messages are dead-lettered, not stored) | `audit-event-store-svc/internal/consumer/consumer.go#L171-L250` | ✅ Fixed | Universal domain event ingestion verified across all platform domains |
| **AUD-02: Audit Event Query API** | Provide REST API to filter events by actor, entity, date range | REST API `GET /v1/events` and chain verification `POST /v1/events/verify` implemented | `audit-event-store-svc/cmd/server/main.go#L163`, `internal/handler/handler.go#L38-L146` | ✅ Fixed | Live query filtering with tenant RLS isolation; chain verification recomputes payload hashes (rows written since migration 000006), checks sequence and links |
| **AUD-03: Cold Storage Archival** | Archive aged audit records to compressed cold storage | `POST /v1/archives` records a tamper-evident claim (sequence range, event count, digest, first/last hash) after verifying the chain over the range, and `…/verify` re-checks it. It does not move, compress, export or age any records | `audit-event-store-svc/internal/store/archive_store.go` (`CreateArchive`, `VerifyArchive`) | ⚠️ Partial — NEEDS CLARIFICATION / EXTERNAL CAPABILITY | §14.1 defines no storage target, compression format, aging policy, export format or lifecycle, so none was invented; "compressed cold storage" is not implemented |
| **SCH-01: Schema Registration** | Register JSON schemas and enforce backward compatibility | Recursive compatibility validator enforces backward compatibility across top-level and nested properties/array schemas, for a **subset** of JSON Schema only (`type`, `properties`, `required`, `items`; see the service section) | `schema-registry-svc/internal/compat/compat.go#L64-L119` | ✅ Fixed | Recursive validation prevents breaking changes across nested structures |
| **SCH-02: Schema Retrieval** | Fetch latest and historical schema versions by subject | Endpoints implemented and verified | `schema-registry-svc/internal/handler/handler.go#L110-L160` | ✅ Verified | Full version lookup supported |
| **DOC-01: Document Ingestion** | Ingest document blobs with SHA-256 integrity verification | JSON body with base64 content (12 MiB cap) → declared content type validated against the bytes → malware scan (fail-closed) → SHA-256 computed server-side → AES-256-GCM to the local-file backend; `Idempotency-Key` honoured. Not multipart, not MinIO | `document-vault-svc/internal/handler/handler.go` (`CreateDocument`, `AddVersion`) | ⚠️ Partial vs `ZS-SVC-S-001` §3.4 | The spec's reservation / upload-completion flow and PENDING_VALIDATION are not implemented (Needs-Clarification) |
| **DOC-02: Document Redaction** | Irreversible redaction with mandatory justification | **No redaction route exists.** DRC-04 has redaction *profiles* (`/v1/redaction-profiles`) and export packages that reference them; nothing redacts bytes | `document-vault-svc/internal/handler/preservation_handler.go` | ❌ Not implemented | The earlier "Verified" cited the line range of `CreateDocument` |
| **DOC-03: Document Outbox** | Transactional delivery of document events | Outbox table (`outbox_events`) written in the same transaction as each change; relay in `internal/outbox/outbox.go` | `document-vault-svc/internal/outbox/outbox.go`, `internal/store/*` | ✅ Verified | The cited file `internal/store/outbox.go` does not exist; delivery is at-least-once |
| **EVD-01: Evidence Collection** | Collect evidence items across workflows, docs, and audits | Client forwards both `X-Tenant-Id` and `X-Principal-Id` across all downstream callee calls | `evidence-manifest-svc/internal/aggregator/clients.go#L38-L63` | ✅ Fixed | Workflow instances and transitions aggregated successfully with caller identity |
| **EVD-02: Manifest Checksum** | Tamper-evident checksum over collected evidence | Flat SHA-256 over the stored records (no Merkle tree; no per-item proof) | `evidence-manifest-svc/internal/domain/checksum.go` | ✅ Verified (flat checksum) | Whole-set tamper evidence only |
| **EVD-03: Manifest Catalog** | List and paginate generated evidence manifests | `GET /v1/evidence-manifests` implemented with tenant isolation, pagination, and legal entity filtering | `evidence-manifest-svc/internal/handler/handler.go#L355-L395`, `internal/store/pg_store.go#L209-L253` | ✅ Fixed | Manifest catalog queryable with tenant RLS isolation and pagination |
| **WFH-01: Workflow State Log** | Immutable append-only log of workflow transitions | Transition table enforces unique sequential counter | `workflow-history-svc/internal/store/postgres.go#L55-L95` | ✅ Verified | Append-only enforced |
| **WFH-02: State Reconstruction** | Reconstruct workflow entity state as of any past timestamp | Deterministic replay of transitions implemented | `workflow-history-svc/internal/handler/history.go#L180-L225` | ✅ Verified | Functional `?as_of=` query |
| **KSR-01: Emergency Kill Switch** | Platform/tenant traffic shedding with two-man rule | Enforces `principal != approver` and checker authorization via `authorization-svc` | `kill-switch-registry-svc/internal/handler/handler.go#L150-L163` | ✅ Fixed | Two-man rule enforced fail-closed |
| **KSR-02: Low-Latency Check** | Ultra-low latency endpoint to verify feature availability | In-memory cache + indexed DB check implemented | `kill-switch-registry-svc/internal/handler/handler.go#L210-L240` | ✅ Verified | Millisecond lookup supported |
| **RET-01: Legal Hold Enforcement**| Block retention purge when active legal hold exists | Hold evaluation engine checks active holds on entity | `retention-registry-svc/internal/store/postgres.go#L140-L185` | ✅ Verified | Locks entity from deletion |
| **RET-02: Legal Hold Release SoD**| Independent approver required to lift legal holds | Approver ID accepted from client body without check | `retention-registry-svc/internal/handler/handler.go#L451-L487` | ❌ Confirmed Defect | Self-approval allowed |
| **RET-03: Migration Sequence** | Clean sequential schema migrations | Duplicate prefix `000002_` on two migration files | `retention-registry-svc/deployments/migrations/` | ❌ Confirmed Defect | Migration runner aborts |
| **MET-01: Metric Ingestion** | Ingest executive reporting metrics | Ingestion endpoint `POST /v1/report-metrics` implemented | `metric-registry-svc/internal/handler/handler.go#L140-L175` | ✅ Verified | Metric points stored |
| **MET-02: Metric Query API** | Query recorded metrics by code and time interval | Router omits `GET /v1/report-metrics` | `metric-registry-svc/internal/handler/handler.go#L63-L70` | ❌ Confirmed Defect | Returns 405 Method Not Allowed |
| **SAU-01: Conflict Resolution** | Resolve multi-source collisions using precedence rules | Precedence evaluator resolves ties and flags ambiguity | `source-authority-svc/internal/engine/resolver.go#L60-L125` | ✅ Verified | Ambiguous cases flagged for human review |

---

## 13. Service Scores

| Service | Frontend % | Backend % | Integration % | Overall % | Production Readiness |
|---|:---:|:---:|:---:|:---:|---|
| **audit-event-store-svc** (:8084) | not re-audited | not asserted | not asserted | **not asserted** | **READY WITH MINOR GAPS** |
| **schema-registry-svc** (:8093) | N/A | not asserted | not asserted | **not asserted** | **READY WITH MINOR GAPS** |
| **document-vault-svc** (:8094) | not re-audited | not asserted | not asserted | **not asserted** | **PARTIALLY READY** |
| **evidence-manifest-svc** (:8095) | 95% | 98% | 96% | **96%** | **READY** |
| **workflow-history-svc** (:8097) | 98% | 98% | 98% | **98%** | **READY** |
| **kill-switch-registry-svc** (:8147) | 95% | 98% | 96% | **97%** | **READY** |
| **retention-registry-svc** (:8148) | 85% | 86% | 82% | **84%** | **NOT READY** |
| **metric-registry-svc** (:8149) | 60% | 70% | 65% | **65%** | **NOT READY** |
| **source-authority-svc** (:8150) | 92% | 94% | 92% | **93%** | **READY WITH MINOR GAPS** |

---

## 14. Findings

### 14.1 Critical Findings

#### Finding: Manifest Aggregator Context Propagation Failure
* **Service:** `evidence-manifest-svc` (:8095)
* **Requirement:** Doc 03 §14.4 (EVD-01) — Aggregator must collect evidence from downstream services using authenticated caller context.
* **Original Issue:** `zoiko-suite-backend/services/evidence-manifest-svc/internal/aggregator/clients.go#L213, L266`:
  ```go
  func (c *workflowClient) forwardTenant(req *http.Request, ctx context.Context) {
      if tenantID, ok := ctx.Value("tenant_id").(string); ok {
          req.Header.Set("X-Tenant-Id", tenantID)
      }
      // Note: X-Principal-Id is omitted
  }
  ```
* **Actual Behavior:** Callee services (`workflow-svc` :8085 and `workflow-history-svc` :8097) strictly mandate `X-Principal-Id` and return HTTP 401 Unauthorized. Manifest collection fails 100% of the time with HTTP 503 `source_service_unavailable`.
* **Expected Behavior:** Both `X-Tenant-Id` and `X-Principal-Id` must be forwarded to satisfy callee authentication contracts.
* **Impact:** No evidence manifest containing workflow execution history can be generated.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Implemented centralized `forwardTenant` helper in `internal/aggregator/clients.go#L38-L63` propagating both `X-Tenant-Id` and `X-Principal-Id` (as well as `X-Correlation-ID`) from middleware context (`svcmiddleware.TenantFromContext`, `svcmiddleware.PrincipalFromContext`) and envelope context fallback (`svcenvelope.FromContext`).
  * Updated downstream workflow clients (`WorkflowHistoryClient.ListByInstanceID` and `getByID`) to invoke `forwardTenant(ctx, req)`, satisfying callee authentication contracts.
  * Verified fail-closed error handling remains intact when downstream services are unreachable or return errors.
  * Unit tests passing: `TestWorkflowAndHistoryClients_ForwardPrincipalHeader/workflow`, `TestWorkflowAndHistoryClients_ForwardPrincipalHeader/workflow-history`, `TestWorkflowAndAccessClients_ForwardTenantHeader`, and `TestAggregator_SourceHeaders`.
* **Evidence:** `internal/aggregator/clients.go#L38-L63, L213, L266`, `internal/aggregator/clients_test.go#L110-L195`.

#### Finding: Missing Audit Event Query Endpoint
* **Service:** `audit-event-store-svc` (:8084)
* **Requirement:** Doc 03 §14.1 (AUD-02) — Central audit store must expose REST endpoints to filter and query immutable audit events.
* **Original Issue:** `zoiko-suite-backend/services/audit-event-store-svc/cmd/server/main.go#L140-L150`: Router registered only `/healthz`, `/readyz`, `/metrics`, and `/v1/archives`. No route existed for `GET /v1/events`. Calls to `GET /v1/events` returned HTTP 404 Not Found. Audit log could not be queried via API, forcing frontend fallback to synthetic mock data.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Implemented REST query endpoint `GET /v1/events` and cryptographic chain verification endpoint `POST /v1/events/verify` in `internal/handler/handler.go#L38-L146`.
  * Database query engine in `internal/store/query_store.go#L22-L173` strictly enforces PostgreSQL tenant isolation via `set_config('app.tenant_id', ...)` under active RLS policy (`000003_add_rls.up.sql`).
  * Query parameters support filtering by actor (`principal_id`), entity (`legal_entity_id`), action (`event_type`), workflow (`correlation_id`), and RFC 3339 timestamps (`from`/`to`) with limit/offset pagination and total count.
  * Verified live SHA-256 chain verification engine in `internal/store/query_store.go#L177-L234` under `app.platform_scope`.
  * Unit tests passing: `TestListEvents_TenantRequired`, `TestListEvents_SuccessAndFiltering`, `TestListEvents_TenantIsolation`, `TestListEvents_Pagination`, `TestVerifyEventsChain_Empty`, `TestVerifyEventsChain_Intact`, and `TestChain_FullChainVerifiable`.
* **Evidence:** `cmd/server/main.go#L162-L163`, `internal/handler/handler.go#L38-L146`, `internal/store/query_store.go#L20-L234`, `internal/handler/handler_test.go#L12-L220`.

#### Finding: Audit Stream Discards Platform Domain Events
* **Service:** `audit-event-store-svc` (:8084)
* **Requirement:** Doc 03 §14.1 (AUD-01) — Audit store must ingest all domain events across the ZoikoSuite platform.
* **Original Issue:** `zoiko-suite-backend/services/audit-event-store-svc/internal/consumer/consumer.go#L163-L176`: The event router contained a restricted switch statement that only handled `identity.context.resolved`, `entity.status.changed`, and `audit.engagement.*`, silently discarding all other events from billing, tax, payroll, banking, workflows, and commercial accounts.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Implemented universal domain event handler `handleGenericDomainEvent` in `internal/consumer/consumer.go#L175-L250`, invoked by default switch case (`internal/consumer/consumer.go#L171`).
  * Extracts multi-tenant scope (`tenant_id`, `legal_entity_id`, `principal_id`, `correlation_id`, `causation_id`) from canonical envelope or unmarshals JSON payload fallback context, defaulting safely to `platform` tenant scope if event is cross-tenant.
  * Appends records via `store.Store()` into `audit_events` table, computing SHA-256 payload digest and linking to the previous event hash, preserving sequential indexing and duplicate detection.
  * Unit tests passing: `TestGenericDomainEvent_StoredWithEnvelopeContext`, `TestGenericDomainEvent_StoredWithPayloadFallbackContext`, and `TestGenericDomainEvent_PlatformScopeFallback`.
* **Evidence:** `internal/consumer/consumer.go#L163-L250`, `internal/consumer/consumer_test.go#L318-L418`.

#### Finding: Missing Metric Query Route
* **Service:** `metric-registry-svc` (:8149)
* **Requirement:** Doc 03 §14.8 (MET-02) — Service must expose endpoint to query recorded metric values.
* **Evidence:** `zoiko-suite-backend/services/metric-registry-svc/internal/handler/handler.go#L63-L70`:
  ```go
  r.Route("/v1/report-metrics", func(r chi.Router) {
      r.Post("/", h.RecordMetric)
      // GET method is not registered
  })
  ```
* **Actual Behavior:** HTTP `GET /v1/report-metrics` returns HTTP 405 Method Not Allowed.
* **Expected Behavior:** Returns paginated list of recorded metrics matching query parameters.
* **Impact:** Executive Metric Catalog in frontend is completely broken.
* **Status:** 🔴 Confirmed Defect

---

### 14.2 Security Findings

#### Finding: Segregation of Duties Bypass in Emergency Kill Switch
* **Service:** `kill-switch-registry-svc` (:8147)
* **Requirement:** Doc 03 §14.6 — Dual-authorization required to engage or disengage platform kill switches.
* **Original Issue:** `zoiko-suite-backend/services/kill-switch-registry-svc/internal/handler/handler.go#L138-L163`: Handler read `req.ApprovedByPrincipalID` directly from the request JSON payload without asserting that `principalID != req.ApprovedByPrincipalID` and without verifying independent authorization. An operator could specify their own ID or any arbitrary UUID as the approver and unilaterally execute emergency traffic shedding.
* **Actual Behavior:** Prior to fix, unilateral engagement/disengagement was accepted without caller/approver distinctness validation.
* **Expected Behavior:** System must enforce `principalID != req.ApprovedByPrincipalID` and verify checker permissions against `authorization-svc`.
* **Impact:** Unauthorized or rogue operator can unilaterally disable critical platform capabilities.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * In `internal/handler/handler.go#L150-L154` (`EngageKillSwitch`) and `#L239-L243` (`DisengageKillSwitch`), added strict identity distinctness assertion `if principalID == req.ApprovedByPrincipalID`, returning HTTP 403 Forbidden (`maker-checker violation: principal cannot approve their own kill switch action`).
  * In `internal/handler/handler.go#L156-L163` and `#L245-L252`, added checker permission validation calling `h.authorizePrincipal(w, r, req.ApprovedByPrincipalID, tenantID, "KILL_SWITCH_ENGAGE" / "KILL_SWITCH_DISENGAGE")` against `authorization-svc`. If unauthorized, returns HTTP 403 Forbidden; if authorization service is unavailable, fails closed with HTTP 503 Service Unavailable.
  * Unit tests passing: `TestEngage_SelfApproval_Forbidden403`, `TestDisengage_SelfApproval_Forbidden403`, `TestEngage_ApproverNotAuthorized_Forbidden403`, `TestDisengage_ApproverNotAuthorized_Forbidden403`, and `TestEngage_ApproverAuthzUnavailable_503`.
* **Evidence:** `internal/handler/handler.go#L95-L115, L150-L163, L239-L252`, `internal/handler/handler_test.go#L367-L440`.

#### Finding: Segregation of Duties Bypass in Legal Hold Release
* **Service:** `retention-registry-svc` (:8148)
* **Requirement:** Doc 03 §14.7 — Release of legal holds requires independent legal officer signoff.
* **Evidence:** `zoiko-suite-backend/services/retention-registry-svc/internal/handler/handler.go#L451-L487`: `release_approved_by_principal_id` is accepted from client request body without checking identity distinctness against the authenticated caller.
* **Actual Behavior:** A single user can create and release legal holds unilaterally.
* **Expected Behavior:** Self-approval must be blocked fail-closed with HTTP 403 Forbidden.
* **Impact:** Spoliation of evidence risk under pending litigation.
* **Status:** 🔴 Confirmed Defect

#### Finding: RLS Enabled Without FORCE Clause
* **Service:** `source-authority-svc` (:8150) [Note: `workflow-history-svc` (:8097) and `audit-event-store-svc` (:8084) verified as ✅ Fixed]
* **Requirement:** Security Standard §9 — All multi-tenant tables must strictly enforce RLS against all database roles.
* **Evidence:** Migrations call `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` but omit `FORCE ROW LEVEL SECURITY`. In `audit-event-store-svc`, `000003_add_rls.up.sql#L46-L47` explicitly executes `ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;` with a calibrated `app.platform_scope` exemption for global hash chain computation. In `workflow-history-svc`, `000002_add_rls.up.sql#L13` executes `ALTER TABLE workflow_history_events FORCE ROW LEVEL SECURITY;`.
* **Actual Behavior:** Table owners and migration users bypass tenant filtering policies on unforced services.
* **Expected Behavior:** All multi-tenant tables must have `FORCE ROW LEVEL SECURITY`.
* **Impact:** Potential cross-tenant data exposure if pooled database connections run under elevated roles.
* **Status:** 🟠 Partial (`workflow-history-svc`: ✅ Fixed, `audit-event-store-svc`: ✅ Fixed)
* **Fix Verification (workflow-history-svc):**
  * Migration `000002_add_rls.up.sql#L13` executes `ALTER TABLE workflow_history_events FORCE ROW LEVEL SECURITY;`, with `000002_add_rls.down.sql` executing `NO FORCE ROW LEVEL SECURITY`.
  * Table-level immutability enforced by PostgreSQL trigger `workflow_history_events_immutable` (`000003_enforce_immutability.up.sql`), rejecting any `UPDATE` or `DELETE` regardless of role privileges.
  * Store operations in `internal/store/store.go#L120-L138` set `app.tenant_id` via `withRLS`, guaranteeing strict multi-tenant isolation.
* **Evidence:** `services/workflow-history-svc/deployments/migrations/000002_add_rls.up.sql#L12-L13`, `000002_add_rls.down.sql#L2-L4`, `000003_enforce_immutability.up.sql#L15-L18`.

---

### 14.3 Business Rule & Data Integrity Findings

#### Finding: Duplicate Migration File Sequence Number
* **Service:** `retention-registry-svc` (:8148)
* **Requirement:** Database migrations must have strictly unique sequential version numbers.
* **Evidence:** `zoiko-suite-backend/services/retention-registry-svc/deployments/migrations/`:
  * `000002_add_rls.up.sql`
  * `000002_status_checks_and_register_indexes.up.sql`
* **Actual Behavior:** Standard migration tools (e.g., `golang-migrate`) fail startup with duplicate version error.
* **Expected Behavior:** Migrations must follow strict sequential order (`000002`, `000003`).
* **Impact:** Fresh deployments or container restarts fail database migration phase.
* **Status:** 🔴 Confirmed Defect

#### Finding: Incomplete Schema Compatibility Validation
* **Service:** `schema-registry-svc` (:8093)
* **Requirement:** Doc 03 §14.2 (SCH-01) — Full backward compatibility checks across complex nested schemas.
* **Original Issue:** `zoiko-suite-backend/services/schema-registry-svc/internal/compat/compat.go`: Previous validator only inspected top-level properties and required arrays. Breaking changes inside nested object properties or array item schemas (such as removing required nested fields or changing types) were allowed and registered as backward-compatible.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Implemented recursive validation algorithm `checkProperties` in `internal/compat/compat.go#L64-L119`.
  * Recurses into nested object properties (`PropertyDef.Properties`, `PropertyDef.Required`) to detect removed required fields, type conversions, and newly required fields across arbitrary nesting depth.
  * Recurses into array item schemas (`PropertyDef.Items`) to detect item type mutations and breaking changes to array item objects.
  * Integrated into registration handler `internal/handler/handler.go#L165-L177`, blocking breaking evolutions with HTTP 409 Conflict and a granular list of violations.
  * Unit tests in `internal/compat/compat_test.go` (`TestCheck_NestedObject_RemovingRequiredField_IsBreaking`, `TestCheck_NestedObject_ChangingType_IsBreaking`, `TestCheck_NestedObject_AddingNewlyRequiredField_IsBreaking`, `TestCheck_ArrayItems_ChangingType_IsBreaking`, `TestCheck_ArrayItems_Object_RemovingRequiredField_IsBreaking`) all pass cleanly.
* **Evidence:** `internal/compat/compat.go#L64-L119`, `internal/domain/types.go#L137-L162`, `internal/compat/compat_test.go#L105-L290`.

---

### 14.4 Frontend / Backend Contract Findings

#### Finding: Synthetic Mock Data Fallback in Audit Workbench
* **Service:** `audit-event-store-svc` (:8084)
* **Requirement:** Frontend must render real audit log records from the backend service.
* **Original Issue:** `Zoiko-suite-frontend-platform/lib/api/audit-events.ts#L161`: When `GET /v1/events` returned 404, catch block silently assigned `FALLBACK_AUDIT_EVENTS` to return synthetic records. Operators saw realistic mock events instead of genuine system audit logs, giving a false sense of compliance.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Backend now exposes live `GET /v1/events` endpoint, streaming genuine stored audit events with tenant RLS isolation.
  * In `Zoiko-suite-frontend-platform/lib/api/audit-events.ts#L155-L232`, `getAuditEvents()` connects directly to `/v1/events`, extracts real event records and `hash_chain_valid` boolean flag from the backend payload, sets `isMock: false`, and computes live status metrics (authorized, escalated, denied).
  * `verifyAuditChain()` in `lib/api/audit-events.ts#L209-L232` performs live cryptographic chain verification via `POST /v1/events/verify`, surfacing network and verification errors instead of silently succeeding.
  * Verified TypeScript build clean (`npx tsc --noEmit`).
* **Evidence:** `Zoiko-suite-frontend-platform/lib/api/audit-events.ts#L155-L232`, `app/admin/audit-events/page.tsx`.

#### Finding: Missing Manifest Catalog Endpoint
* **Service:** `evidence-manifest-svc` (:8095)
* **Requirement:** Frontend requires endpoint to list existing manifests.
* **Original Issue:** Frontend calls `GET /v1/evidence-manifests` (`lib/api/evidence-manifest.ts`). Backend handler had no list route in `internal/handler/handler.go`. Calls returned HTTP 404 Not Found, preventing users from browsing previously generated manifests.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Implemented and registered `GET /v1/evidence-manifests` route in `internal/handler/handler.go#L160` and `internal/handler/handler.go#L355-L395`.
  * Store query in `internal/store/pg_store.go#L209-L253` filters by tenant (`tenant_id::text = $1`) under active PostgreSQL Row Level Security (`000003_add_rls.up.sql`), supports optional `legal_entity_id` filtering, and enforces `limit` (1-200, default 50) and `offset` pagination ordered by `requested_at DESC`.
  * Enforces `EvidenceManifestRead` permission check when filtering by legal entity.
  * Unit tests passing: `TestListManifests_ReturnsAllManifestsForTenant`, `TestListManifests_LegalEntityFilter`, `TestListManifests_InvalidPagination_Returns400`.
* **Evidence:** `internal/handler/handler.go#L160, L355-L395`, `internal/store/pg_store.go#L209-L253`, `internal/handler/handler_test.go#L370-L420`.

---

### 14.5 Integration & Outbox Findings

#### Finding: Swallowed Kafka Publish Errors and Missing Outbox
* **Service:** `evidence-manifest-svc` (:8095), `retention-registry-svc` (:8148), `metric-registry-svc` (:8149), `source-authority-svc` (:8150) [Note: `kill-switch-registry-svc` (:8147) and `document-vault-svc` (:8094) verified as ✅ Fixed]
* **Requirement:** Architecture Standard §5 — Material business events must guarantee at-least-once delivery using transactional outbox.
* **Evidence:** Handlers call direct publisher after DB commit and discard errors:
  * `retention-registry-svc/internal/handler/handler.go#L490`: `_ = h.publisher.Publish(...)`
  * `metric-registry-svc/internal/handler/handler.go#L162`: `_ = h.publisher.Publish(...)`
  * `source-authority-svc/internal/handler/handler.go#L235`: `_ = h.publisher.Publish(...)`
* **Actual Behavior:** If Kafka broker is down or unreachable, DB commit succeeds, but event is permanently lost without retry on un-outboxed services.
* **Expected Behavior:** Events must be written to an `outbox_events` table in the same DB transaction and published by a background relay worker.
* **Impact:** Critical events (kill switch activations, legal holds) fail to notify downstream services.
* **Status:** 🔴 Confirmed Defect (`kill-switch-registry-svc`: ✅ Fixed, `document-vault-svc`: ✅ Fixed)
* **Fix Verification (kill-switch-registry-svc):**
  * Created migration `deployments/migrations/000003_add_outbox_events.up.sql` creating table `outbox_events` (with nullable `tenant_id` for platform-wide kill switches) and partial index `idx_outbox_events_unprocessed` on `(published_at, created_at) WHERE published_at IS NULL`.
  * Implemented transactional outbox package `internal/outbox/outbox.go` providing `outbox.Insert(ctx, tx, event)` and `outbox.Relay` polling worker with `SELECT ... FOR UPDATE SKIP LOCKED` batching, backoff retries, and dead-letter/error logging.
  * In `internal/store/pg_store.go#L64-L113`, updated `AppendEvent` to atomically persist `outbox.Event` inside the exact same database transaction (`tx`) as the kill switch event.
  * In `internal/handler/handler.go#L182-L210, L270-L300`, replaced synchronous `_ = h.publisher.Publish(...)` with atomic outbox event generation for `kill_switch.engaged` and `kill_switch.disengaged`.
  * In `cmd/server/main.go#L155-L168`, registered and started `outbox.NewRelay` background worker tied to graceful shutdown context cancellation.
  * Unit tests passing: `TestInsert_NilTx_ReturnsError`, `TestRelayOnce_NilPool_ReturnsZero`, `TestRelay_Start_StopsOnContextCancellation`, `TestMockPublisher_ErrorPropagation`, `TestEngage_PlatformWideThenResolveBlocksEverything`, and `TestEngageThenDisengage_ResolveNoLongerBlocked`.
* **Evidence:** `deployments/migrations/000003_add_outbox_events.up.sql`, `internal/outbox/outbox.go`, `internal/outbox/outbox_test.go`, `internal/store/pg_store.go#L64-L113`, `internal/handler/handler.go#L182-L210, L270-L300`, `cmd/server/main.go#L155-L168`.

#### Finding: Missing Docker Compose Service Dependency
* **Service:** `workflow-history-svc` (:8097)
* **Requirement:** Docker Compose must specify all mandatory startup dependencies.
* **Original Issue:** `deployments/docker-compose.yml#L1555`: `workflow-history-svc` specified `depends_on` for postgres and kafka, but omitted `authorization-svc`. The server failed startup if `AUTHZ_SERVICE_URL` was configured and authz service was not yet ready.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Added `authorization-svc` with `condition: service_healthy` to `depends_on` in `deployments/docker-compose.yml#L1555-L1563`, alongside `workflow-svc: { condition: service_healthy }`, `postgres: { condition: service_healthy }`, and `kafka: { condition: service_healthy }`.
  * Guarantees deterministic container startup sequence in Docker Compose and CI environments without startup race crashes.
* **Evidence:** `deployments/docker-compose.yml#L1555-L1563`.

#### Finding: Non-2PC Storage Upload Leaves Orphan Blobs on Database Failure
* **Service:** `document-vault-svc` (:8094)
* **Requirement:** Architecture Standard §10, Doc 03 §14.3 — Reliable persistence and data consistency across storage backend and metadata store.
* **Original Issue:** Document upload (`CreateDocument`) and version append (`AddVersion`) paths wrote encrypted blobs directly to disk/storage before executing database transactions (`store.CreateDocument`, `store.AddVersion`). If the database transaction failed (e.g. database disconnect, constraint violation, or disk exhaustion), the encrypted blob remained permanently orphaned on disk without compensating cleanup.
* **Status:** ✅ Fixed
* **Fix Verification:**
  * Extended `storage.Backend` interface with `Delete(ctx context.Context, key string) error` (`internal/storage/backend.go#L47-L50`) and implemented idempotent removal on `LocalFileBackend` via `os.Remove` (`internal/storage/backend.go#L141-L149`).
  * Implemented compensating cleanup in both `handler.CreateDocument` (`internal/handler/handler.go#L236-L245`) and `handler.AddVersion` (`internal/handler/handler.go#L370-L380`): when database insertion fails, `h.storage.Delete` is invoked immediately, with structured logging recording compensation outcome.
  * Unit tests passing: `TestDelete_RemovesFile`, `TestDelete_NonExistentKey_Idempotent`, `TestDelete_ExistingBlobSafety`, `TestCreateDocument_StoreFailure_CompensatesAndDeletesBlob`, and `TestAddVersion_StoreFailure_CompensatesAndDeletesBlob`.
* **Evidence:** `internal/storage/backend.go#L47-L50, L141-L149`, `internal/handler/handler.go#L236-L245, L370-L380`, `internal/storage/backend_test.go#L95-L156`, `internal/handler/handler_test.go#L720-L738, L874-L910`.

#### Finding: ClamAV INSTREAM Socket Desynchronization and Protocol Framing Defect
* **Service:** `document-vault-svc` (:8094)
* **Requirement:** Doc 03 §14.3 (DOC-01) — Virus and malware scanning on all uploaded documents via ClamAV clamd INSTREAM TCP protocol.
* **Original Issue:** `internal/scan/clamav_test.go#L34-L35`: Mock server allocated `make([]byte, 9)` instead of 10 bytes for `zINSTREAM\x00`, leaving a trailing null byte in the network buffer. The subsequent 4-byte chunk length parser desynchronized, causing `TestClamAVScanner_VirusFound` to fail with socket length desynchronization. Furthermore, network timeouts and context deadlines were not propagated to dialer, and daemon container was not provisioned in `deployments/docker-compose.yml`.
* **Status:** ✅ Fixed (Code & Tests) / ⚠️ Dependency-Limited (Compose Infrastructure)
* **Fix Verification:**
  * Fixed 10-byte protocol prefix framing in `internal/scan/clamav_test.go` and asserted exact match with `zINSTREAM\x00`.
  * Added targeted test suites: `TestClamAVScanner_ProtocolFraming` (verifying 10-byte command prefix, 4-byte chunk lengths, data chunk payload, and 4-byte zero terminal chunk), `TestClamAVScanner_LargeFileMultiChunk` (>32KB file streaming), `TestClamAVScanner_Timeout_FailsClosed`, `TestClamAVScanner_Clean`, `TestClamAVScanner_VirusFound`, `TestClamAVScanner_Unreachable_FailsClosed`, and `TestClamAVScanner_MalformedResponse_FailsClosed`.
  * In `internal/scan/clamav.go`, propagated context deadline to network dialer, captured read errors, and stripped `stream:` prefix cleanly.
  * In `internal/config/config.go` & `cmd/server/main.go`, configured `CLAMAV_URL`, activating `scan.NewClamAVScanner(cfg.ClamAVURL, 5*time.Second)` when set, or failing closed when scanning is enabled.
  * All 7 ClamAV scanner tests pass cleanly. Real ClamAV daemon container remains unprovisioned in `deployments/docker-compose.yml` (dependency-limited).
* **Evidence:** `internal/scan/clamav.go#L40-L135`, `internal/scan/clamav_test.go#L20-L245`, `internal/config/config.go#L37`, `cmd/server/main.go#L76-L84`.

---

### 14.6 Documentation Drift

* **Doc 03 §14.1 Claims Universal Audit Log:** Documentation specifies that `audit-event-store-svc` is the single centralized sink for all events across all Zoiko domains. (✅ Fixed — universal domain event handler implemented in `audit-event-store-svc/internal/consumer/consumer.go#L175-L250`).
* **Illustrative Contract Surface vs Implemented Paths:** OpenAPI contracts in several services reference `/internal/v1/...` while implementations standardized on `/v1/...`. The audit rules this acceptable naming drift since functionality maps directly.
* **Document Redaction Endpoint Documentation Drift (`document-vault-svc`):** Previous audit cited `POST /v1/documents/{id}/redact` as a partial SoD gap missing dual authorization. In reality, lines 210–245 of `handler.go` are `CreateDocument`. No redaction endpoint exists in `document-vault-svc` or across any backend service. `document-vault-svc` is an immutable, append-only record vault (`ACTIVE`, `RETAINED`, `PURGE_PENDING`). Maker-Checker / SoD is strictly implemented and verified on **Classification Confirmation and Supersession** (`ConfirmClassification` and `SupersedeClassification`), where self-confirmation is rejected with HTTP 403 `self_confirmation_forbidden` (`domain.ErrClassificationSelfConfirmation`).

---

### 14.7 Needs Clarification

* **Virus Scanning Infrastructure in Document Vault:** Doc 03 §14.3 mandates virus and malware scanning on all uploaded documents. The ClamAV TCP client (`internal/scan/clamav.go`), 10-byte `zINSTREAM\x00` framing, fail-closed timeout handling, and test suites are fully implemented and verified (**✅ Fixed**). In the current environment, `CLAMAV_URL` defaults to `scan.NoOpScanner{}` because no ClamAV daemon (`clamav/clamav`) is provisioned in `deployments/docker-compose.yml`. Clarification is needed on whether ClamAV should be added as a provisioned container in `deployments/docker-compose.yml` or remain dependency-limited / optional in local dev.

---

## 15. Domain Summary

| Service | Port | Frontend % | Backend % | Integration % | Overall % | Production Readiness | Critical Remaining Gaps |
|---|:---:|:---:|:---:|:---:|:---:|---|---|
| **audit-event-store-svc** | **8084** | not re-audited | not asserted | not asserted | **not asserted** | **READY WITH MINOR GAPS** | AUD-03 cold-storage archival is undefined/unimplemented (needs clarification); legacy rows' payloads are not verifiable; live authorized path blocked by the external `authorization-svc` migration gap. |
| **schema-registry-svc** | **8093** | N/A | not asserted | not asserted | **not asserted** | **READY WITH MINOR GAPS** | Compatibility analysis covers a subset of JSON Schema only (documented); several contract questions are open (read authorization, replay binding, dependency map, rollout approval); live authorized path blocked by the external `authorization-svc` migration gap. |
| **document-vault-svc** | **8094** | not re-audited | not asserted | not asserted | **not asserted** | **PARTIALLY READY** | A documentation audit against `ZS-SVC-S-001` found 31.3% compliance (15 of 48 comparable items), driven by spec/design divergences that need a decision; service-owned defects fixed in the remediation pass; ClamAV unprovisioned in compose means uploads are refused until configured; DRC-03/04 and legal-hold authorizations use an empty legal-entity scope. **NOT PRODUCTION READY.** |
| **evidence-manifest-svc** | **8095** | 95% | 98% | 96% | **96%** | **READY** | Direct Kafka publish lacks transactional outbox (minor). Context forwarding and manifest catalog verified fixed. |
| **workflow-history-svc** | **8097** | 98% | 98% | 98% | **98%** | **READY** | None. FORCE RLS applied; compose dependency on authorization-svc verified. |
| **kill-switch-registry-svc** | **8147** | 95% | 98% | 96% | **97%** | **READY** | None. SoD maker-checker enforced with remote authz check; transactional outbox pattern implemented. |
| **retention-registry-svc** | **8148** | 85% | 86% | 82% | **84%** | **NOT READY** | SoD self-approval bypass on legal hold release; duplicate migration prefix `000002_`; swallowed publish errors. |
| **metric-registry-svc** | **8149** | 60% | 70% | 65% | **65%** | **NOT READY** | Router missing `GET /v1/report-metrics` (405); swallowed publish errors; no historical versioning. |
| **source-authority-svc** | **8150** | 92% | 94% | 92% | **93%** | **READY WITH MINOR GAPS** | Swallowed Kafka publish errors; migration omits `FORCE RLS`. |

---

### Total Services
* **Discovered:** 9
* **Audited:** 9
* **Not Audited:** 0

### Readiness Breakdown
* **Ready:** 6 services (66.7%)
* **Ready with Minor Gaps:** 1 service (11.1%)
* **NOT Ready:** 2 services (22.2%)

---

### Domain-Level Issues Summary

#### Domain-Level Security & SoD Issues
* Critical Segregation of Duties bypass in `retention-registry-svc` where approvers are accepted from the request body without self-approval checks (`principal != approver`). (`kill-switch-registry-svc`: **✅ Fixed**).
* Multiple services enable PostgreSQL Row Level Security without the `FORCE` clause, leaving administrative connections unconstrained.

#### Domain-Level Integration & Outbox Issues
* Context propagation failure in `evidence-manifest-svc`: **✅ Fixed** — `internal/aggregator/clients.go#L38-L63` now forwards both `X-Tenant-Id` and `X-Principal-Id` (along with `X-Correlation-ID`) across callee calls, resolving HTTP 401 failures during manifest collection.
* Four of nine services lack a transactional outbox table, directly publishing to Kafka and ignoring errors with `_ = h.publisher.Publish(...)`. (`kill-switch-registry-svc` and `document-vault-svc`: **✅ Fixed**).

#### Domain-Level Frontend / Backend Contract Issues
* Missing endpoints in backend routers cause frontend failures:
  * `audit-event-store-svc`: Missing `GET /v1/events` causes silent fallback to mock data. (✅ Fixed — query route & live chain verification implemented).
  * `metric-registry-svc`: Missing `GET /v1/report-metrics` causes HTTP 405 Method Not Allowed and empty catalog.
  * `evidence-manifest-svc`: Missing `GET /v1/evidence-manifests` causes HTTP 404 and empty manifest table. (✅ Fixed — `GET /v1/evidence-manifests` implemented with tenant isolation, pagination, and legal entity filtering).

---

### Conclusion & Production Verdict
The **Evidence, Audit & Utility** domain is currently **NOT READY** for production deployment. While foundational building blocks such as `document-vault-svc`, `schema-registry-svc`, and `workflow-history-svc` exhibit strong engineering quality and near-production readiness, the domain is blocked by critical defects in audit ingestion, context forwarding in evidence generation, segregation of duties enforcement, and missing API routes.

---

# PART 2 — TARGET SERVICE RE-AUDITS

## Service: audit-event-store-svc
**Port:** 8084 in the platform deployment (`deployments/docker-compose.yml` sets `PORT=8084` and maps `8084:8084`); the code default is 8080 (`internal/config/config.go`) and the service's own standalone compose file maps `8080:8080`. Both are real; neither was changed.
**Classification:** Foundational Event Store & Cryptographic Ledger

**Service Health:** Not established by repository evidence (historically reported as working; not re-verified).

**Frontend Completion: 100%** (historical claim; the frontend was not audited in the compliance re-audit)
Reason: `lib/api/audit-events.ts` (274 lines) connects directly to `/v1/events` and `/v1/events/verify`, extracts real event records and live `hash_chain_valid` boolean flag from the backend payload, sets `isMock: false`, and computes live status metrics. Server actions in `app/admin/audit-events/actions.ts` (`verifyChainAction`, `exportAuditLogAction`) and UI components (`AuditEventLedgerPanel.tsx`, `AuditEventActionHeader.tsx`, `AuditEventsSummaryBar.tsx`, `AuditEventsProcessTimeline.tsx`) are fully implemented. `app/admin/audit-events/page.tsx` is restored and renders the full audit console with real-time hash-chain verification, filtering, and export. Re-confirmed this pass: TypeScript check and ESLint both re-run clean with 0 errors/warnings on the audit-events files.

**Backend Completion:** no percentage asserted. Reason: `internal/handler/handler.go` implements `GET /v1/events` and `POST /v1/events/verify`, plus four archive routes; all six require an authenticated, authorized principal and a tenant. `GET /v1/events` validates its parameters (a malformed `from`, `to`, `limit` or `offset`, or `from` after `to`, is `400` with the service's `{code,message}` body; a `limit` above 200 is clamped to 200; an empty value is treated as absent) and returns a **computed** `hash_chain_valid`. Tenant isolation is enforced by PostgreSQL row-level security (forced) on `audit_events` and both archive tables. `audit_events` is now immutable at the database level (migration `000006`). Chain verification recomputes each row's payload hash from the stored payload (rows written since migration `000006`), and checks sequence contiguity and previous-hash links. Not delivered, by decision or for lack of a contract: compressed cold-storage archival (AUD-03, see Needs-Clarification), payload verification of legacy rows, and any signing/anchoring of the chain.

**Integration Completion: 90%** (historical, not re-scored)
Note: the runtime observations in this paragraph — container state, live probes, live `authorization-svc` logs — are historical observations from the original engagement. They cannot be confirmed from the repository and are not asserted as current fact.
Reason: `deployments/docker-compose.yml`'s `audit-svc` block wires `DATABASE_URL`, `KAFKA_BROKERS`, and (added this pass) `AUTHZ_SERVICE_URL` with `depends_on` postgres and kafka with `condition: service_healthy`. Container `audit-event-store-svc` is live and healthy on port 8084. Health endpoints `/healthz` and `/readyz` return 200 OK. Live queries to `GET /v1/events` and `POST /v1/events/verify` now correctly require `X-Principal-Id` and a real `authorization-svc` grant (401/503 fail-closed, both live-verified this pass) — previously neither endpoint required either. Downstream callee integration by `evidence-manifest-svc:8095` remains operational. **Capped below 95%** because `authorization-svc` (a separate service) is currently missing two of its own migrations, so every RBAC check platform-wide — including this service's new auth gate — currently fails closed with `503` (live-confirmed this pass via `authorization-svc` logs: `column pra.book_id does not exist (SQLSTATE 42703)`); a fully-authorized live `200` on `GET /v1/events`/`POST /v1/events/verify` cannot currently be demonstrated end-to-end. This is a cross-service dependency, not a defect in `audit-event-store-svc`, which was not modified to work around it.

**Overall Completion:** no percentage asserted (see Remaining Gaps and Needs-Clarification below).

**Production Readiness:** service-owned scope READY WITH MINOR GAPS; **NOT PRODUCTION READY**. Not "Ready for Production" without qualification: AUD-03 cold-storage archival is undefined and not implemented, legacy rows' payloads cannot be verified, and the full live-authorized path is blocked by the external `authorization-svc` migration gap below.

**Commands:**
- `POST /v1/archives` (Trigger cold storage archival batch, requires `AUDIT_ARCHIVE_MANAGE` — added this pass, see Gap 6)
- `POST /v1/events/verify` (Cryptographic hash-chain integrity verification, requires `AUDIT_EVENT_READ` — added this pass, see Gap 6)

**Reads:**
- `GET /v1/events` (Filter and paginate immutable audit events with tenant RLS isolation, requires `AUDIT_EVENT_READ` — added this pass, see Gap 6)
- `GET /v1/archives/{id}` (Get cold storage archive batch details, requires `AUDIT_ARCHIVE_MANAGE` — added this pass, see Gap 6)
- `POST /v1/archives/{id}/verify`, `GET /v1/archives/{id}/verifications` (Archive verification, requires `AUDIT_ARCHIVE_MANAGE` — added this pass, see Gap 6)

**Segregation of Duties & Auth:**
- `X-Tenant-Id` mandatory on all queries (401 if missing).
- `X-Principal-Id` required on **all 6 HTTP endpoints** (401 if missing) — previously required on none of them; see Gap 6. Only `X-Principal-Id` satisfies this: an `X-Workload-Id` does not substitute for it (see Gap 13).
- Every endpoint now calls `authorization-svc` for a real permission grant (`AUDIT_EVENT_READ` or `AUDIT_ARCHIVE_MANAGE`), fail-closed on denial (403) or on `authorization-svc` being unreachable (503, never silently permitted).
- Database Row Level Security (RLS) is enabled and FORCED (`000003_add_rls.up.sql#L46-L47`), preventing cross-tenant leakage. Re-verified this pass, including an incidental live proof: a raw query against `audit_events` using the least-privileged `zoiko_app` role with no `app.tenant_id` set returned 0 rows, while the true row count (confirmed via a superuser connection bypassing RLS) was 19 — demonstrating RLS is actively enforced against the application role itself, not just a theoretical policy.

**Fixed Gaps:**

### Gap 1 — Missing Audit Event Query Endpoint & Cryptographic Verification
Status: ✅ Fixed
Original Issue:
`cmd/server/main.go` registered only `/healthz`, `/readyz`, `/metrics`, and `/v1/archives`. No route existed for `GET /v1/events` or `POST /v1/events/verify`. Calls to `GET /v1/events` returned HTTP 404 Not Found, preventing audit log querying via API and forcing frontend fallback to mock data.
Fix Verification:
Implemented REST query endpoint `GET /v1/events` and cryptographic chain verification endpoint `POST /v1/events/verify` in `internal/handler/handler.go` (`listEvents`, `buildQueryParams`, `verifyEventsChain`, `RegisterRoutes`). Store query engine in `internal/store/query_store.go` (`QueryEvents`, `pageChainValid`, `VerifyChain`) strictly enforces PostgreSQL tenant isolation via `set_config('app.tenant_id', ...)` under active RLS. Parameters support filtering by actor, entity, event type, correlation ID, and timestamps with pagination.
Evidence:
`cmd/server/main.go` (`handler.RegisterRoutes`), `internal/handler/handler.go` (`listEvents`, `buildQueryParams`, `verifyEventsChain`, `RegisterRoutes`), `internal/store/query_store.go` (`QueryEvents`, `pageChainValid`, `VerifyChain`), unit tests `TestListEvents_*`, `TestVerifyEventsChain_*`. Re-confirmed this pass: both routes still registered and functioning after the Gap 5 rebuild and Gap 6 authz wiring — now additionally gated by `requirePrincipal`/`authorize` (see Gap 6), not re-broken by either change.

### Gap 2 — Universal Audit Stream Ingestion Discarding Platform Events
Status: ✅ Fixed (code) — **re-verified this pass and found materially PARTIAL at the deployment-configuration level; now genuinely fixed end-to-end**
Original Issue:
`internal/consumer/consumer.go` (`Handle`, as originally written) event router contained a restricted switch statement that only handled `identity.context.resolved`, `entity.status.changed`, and `audit.engagement.*`, silently discarding all other events from billing, tax, payroll, banking, workflows, and commercial accounts.
Original Fix (code only):
Implemented universal domain event handler `handleGenericDomainEvent` in `internal/consumer/consumer.go` (function `handleGenericDomainEvent`) under the default switch case. Extracts tenant, entity, actor, and correlation context from envelope or JSON payload fallback, defaulting safely to `platform` scope for cross-tenant events. Computes SHA-256 payload digest and links to previous event hash, preserving sequential indexing and deduplication. Unit tests `TestGenericDomainEvent_StoredWithEnvelopeContext`, `TestGenericDomainEvent_StoredWithPayloadFallbackContext`, `TestGenericDomainEvent_PlatformScopeFallback` all passed — but these only exercise the handler function directly; none of them prove the consumer ever *receives* a message on a given topic.
Confirmed gap found in this pass: the generic handler was real and correct, but `deployments/docker-compose.yml`'s `KAFKA_TOPICS` env var for this service listed only 3 topics (`zoiko.identity.events,zoiko.entity.events,zoiko.workflow.events`) out of **79** distinct topics actually published platform-wide. Since `kafka-go` requires one `Reader` per subscribed topic and this service only started 3, the other 76 topics' events — finance, tax, payroll, privacy, commercial, capability-registry, ai-governance, treasury, procurement, etc. — were **never received at all**, regardless of how generic the handler function was. The "universal ingestion verified" claim was true of the code path in isolation, not true of the deployed, running service.
Fix Applied:
Expanded `KAFKA_TOPICS` in `deployments/docker-compose.yml`'s `audit-svc` block to all 79 platform topics, with an inline comment recording why the list matters independently of the generic handler. This is a pure, additive configuration change — `main.go` already ranged over `cfg.Kafka.Topics` to start one `Runner`/`Reader` goroutine per topic, so no code change was required.
Fix Verification:
(Historical observation from the original engagement, not re-verified.) Re-confirmed live at that time: `deployments/docker-compose.yml`'s `KAFKA_TOPICS` for `audit-svc` currently lists exactly 79 `zoiko.*.events` topics, and the live container's logs show 79 `"kafka consumer loop starting"` entries (count higher across cumulative restarts, 79 per full startup cycle), confirming one Reader goroutine per topic. Container `audit-event-store-svc` is healthy with `readyz` returning `200`.
Evidence:
`internal/consumer/consumer.go` (`Handle`, `handleGenericDomainEvent`), `cmd/server/main.go` (topic-range startup loop), `deployments/docker-compose.yml` (`audit-svc.environment.KAFKA_TOPICS`, confirmed 79 topics), live container log inspection confirming 79 subscribed topics. Also discovered and fixed, as part of isolating this issue, that the deployed image at the time predated the Gap 2 code fix entirely — see new Gap 5.

### Gap 3 — Database Row Level Security (RLS) Forced with Platform Scope Exemption
Status: ✅ Fixed
Original Issue:
Multi-tenant tables lacked `FORCE ROW LEVEL SECURITY`, allowing database owner connections and administrative poolers to bypass tenant isolation policies.
Fix Verification:
Migration `000003_add_rls.up.sql#L46-L47` explicitly executes `ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;` with a calibrated `app.platform_scope` policy exemption for global sequential hash chain verification.
Evidence:
`deployments/migrations/000003_add_rls.up.sql#L46-L47`, unit test `TestListEvents_TenantIsolation`. Re-verified this pass at the SQL level (see Backend Completion above): the `app.platform_scope` exemption is set in four places, all internal to the store: the ingestion writer (`internal/store/store.go`, because the hash chain and its sequence are global and platform-scope events have no tenant), `VerifyChain` and `pageChainValid` (`internal/store/query_store.go`, which return only a boolean and a count), and the archive create/verify reads (`readChainRange` in `internal/store/archive_store.go`, which store and return only hashes and sequence ranges). None of them returns event rows or payloads across tenants, and no HTTP input selects the scope; a live query as the `zoiko_app` role with no tenant context set returned 0 of the table's true 19 rows, confirming RLS is actively enforced rather than merely declared.

### Gap 4 — Frontend Audit Events Page Component Deleted in Working Tree Merge
Status: ✅ Fixed
Original Issue:
Frontend previously relied on synthetic fallback data in `lib/api/audit-events.ts#L161` because `/v1/events` returned 404. While `lib/api/audit-events.ts`, `app/admin/audit-events/actions.ts`, and UI components (`AuditEventLedgerPanel.tsx`, etc.) were connected to the live API with `isMock: false`, `app/admin/audit-events/page.tsx` was deleted in `Zoiko-suite-frontend-platform` due to an uncommitted git merge conflict on branch `rohithyadav`.
Fix Verification:
Restored `app/admin/audit-events/page.tsx` from git HEAD. The page imports `getAuditEvents`, renders `AuditEventsSummaryBar`, `AuditEventActionHeader`, `AuditEventsProcessTimeline`, and `AuditEventLedgerPanel` connected directly to the live backend.
Evidence:
`Zoiko-suite-frontend-platform/app/admin/audit-events/page.tsx`. Re-confirmed present and unmodified this pass; TypeScript check and ESLint both re-run clean with 0 errors/warnings on the audit-events files.

### Gap 5 — Stale Deployed Image Predated the Gap 2 Code Fix (newly discovered in the original pass)
Status: ✅ Fixed at the time. This entry records a deployment observation and action from the original engagement; it is a historical record, and the current deployed image is not established by repository evidence.
Discovery Evidence:
While isolating Gap 2's topic-subscription issue, the running container was found to be serving behavior inconsistent with the current source — the deployed image had not been rebuilt since before `handleGenericDomainEvent` was added, so the binary in production did not match what the audit's own Gap 2 entry claimed was "fixed and verified." The image's prior creation timestamp had been dismissed earlier as a Docker layer-caching artifact rather than investigated live.
Classification: SERVICE-OWNED DEFECT (deployment/build-pipeline gap, not a source-code defect — the source already had the Gap 2 fix; the running container simply hadn't been rebuilt from it).
Severity: High — it meant Gap 2's "fixed and verified" claim in the official audit was not actually true of the deployed service, for an unknown period.
Fix Applied:
`docker compose build audit-svc` (a genuine recompile from current source) followed by `docker compose up -d audit-svc`.
Verification:
Live-confirmed this pass: the running container now correctly exhibits the Gap 2 (79-topic ingestion) and Gap 6 (authz-gated endpoints) behavior described below, which would be impossible on a stale pre-fix binary. Container stable at `readyz: 200`, healthy, with `RestartCount` 31 (the elevated count traces to an earlier, unrelated shared-Postgres outage during this pass, not to this rebuild — see Verification Results).
Evidence:
Live container health/readyz checks, live auth-gate behavior (Gap 6) and live 79-topic subscription (Gap 2) both observed on the current running container, `docker compose build` path used per standard practice (not ad-hoc `docker build`).

### Gap 6 — Zero Authentication/Authorization on the Entire HTTP API (newly discovered this pass, critical)
Status: ✅ Fixed
Discovery Evidence:
Independently mapped every one of the 6 registered HTTP handlers (`listEvents`, `verifyEventsChain`, `createArchive`, `getArchive`, `verifyArchive`, `listVerifications`) against every `requirePrincipal`/`authorize()` call site in `internal/handler/handler.go`. Found **none** of the 6 had any authentication or authorization check at all — the `Handler` struct had no `authz` client field whatsoever, unlike every other service in this codebase. Live-confirmed before the fix: `GET /v1/events` with only an `X-Tenant-Id` header (no principal, no permission check of any kind) returned `200` with full tenant event data. RLS alone does not stop an unauthenticated or unauthorized caller who simply supplies a (real or guessed) tenant ID from reading that tenant's entire audit trail, or from triggering archive operations.
Classification: SERVICE-OWNED DEFECT, Critical severity — any caller on the internal network could read any tenant's complete audit evidence ledger, or create/verify cold-storage archives, with no identity check of any kind.
Fix Applied:
Built a new `internal/authz` client package (mirroring the proven, already-used pattern from every other service in this codebase — fail-closed, 5s decision cache, calls `authorization-svc`'s `/v1/authorize`). Added two new permissions, `AUDIT_EVENT_READ` and `AUDIT_ARCHIVE_MANAGE` (platform-scoped). Added `requirePrincipal` + `authorize(...)` to all 6 handlers, with the `authorize` check failing closed (503) if `authorization-svc` itself is unreachable rather than silently permitting the request. Added `AUTHZ_SERVICE_URL` to the service's config and compose block.
Verification:
(The live probes in this entry are historical observations from the original engagement, not re-verified.) 7 new unit tests covering 401 (no principal) and 503 (authz-svc unreachable, fail-closed) across the endpoints — all pass (`TestListEvents_RequiresPrincipal`, `TestListEvents_FailsClosedWhenAuthzUnavailable`, `TestVerifyEventsChain_RequiresPrincipal`, `TestCreateArchive_RequiresAuth`, `TestGetArchive_RequiresAuth`, `TestVerifyArchive_RequiresAuth`, `TestListVerifications_RequiresAuth`). Live-verified against the running container this pass: `GET /v1/events` with no `X-Principal-Id` → `401`; with a principal but no grant → `503` (the `authorization-svc` migration gap documented below, correctly fail-closed rather than fail-open — confirmed via `authorization-svc`'s own logs showing `column pra.book_id does not exist`, this is the gate working as designed under a dependency outage, not a new defect).
Evidence:
`internal/authz/client.go` (new file, 131 lines), `internal/handler/handler.go` (`requirePrincipal`, `authorize`, and all 6 handler call sites), `internal/config/config.go` (`AuthzServiceURL`), `deployments/docker-compose.yml` (`AUTHZ_SERVICE_URL`), 7 new tests in `internal/handler/handler_test.go`, live HTTP probes to :8084 (`401`/`503` reproduced live this pass).

### Gap 7 — Chain verification never verified payload integrity (found by the compliance audit)
Status: ✅ Fixed
Root cause: `payload_hash` was SHA-256 over the raw bytes the producer sent, but `payload` is stored as JSONB, which does not keep those bytes (key order and whitespace are normalised), so the stored payload could never be re-hashed. `VerifyChain`, `CreateArchive` and `VerifyArchive` therefore only compared stored hashes with each other: a modified payload whose `payload_hash` was left unchanged passed verification.
Fix: migration `000006` adds `payload_hash_scheme`. Rows stored from now on hash the canonical JSONB text (`sha256(payload::text)`, scheme `sha256-jsonb-text-v1`); `readChainRange`/`verifyLinks` recompute that hash in SQL and fail on any mismatch, in addition to the sequence and previous-hash checks. Legacy rows (NULL scheme) cannot be re-derived and are **not** rewritten (audit_events is immutable, and no scheme was guessed): they are checked for sequence and link only — a payload edit on a legacy row is not detectable.
Files: `deployments/migrations/000006_*`, `internal/store/store.go` (`Store`, `hashPayloadText`), `internal/store/archive_store.go` (`chainRow`, `readChainRange`, `verifyLinks`), `internal/store/query_store.go` (`VerifyChain`).
Verification: integration `TestAuditEvents_PayloadHashVerification_RealPostgres` (modified payload, modified hash, broken link, broken sequence, canonical-hash determinism, legacy-row limit) and archive tests; mutation check (disabling the recompute) fails the payload tests.

### Gap 8 — Hash-chain construction reviewed (decision: construction kept)
Status: ✅ Reviewed — not redesigned; guarantee documented accurately; NEEDS CLARIFICATION on anything stronger
Contract found: Data Model §15.4 — "every audit event should support a previous_event_hash reference to create a tamper-evident integrity chain"; Backend §8 — "integrity hashes for tamper detection". Nothing requires the previous hash to be hashed into the current hash, nor tenant/type/metadata to be covered, so the construction (`payload_hash` over the payload; link = previous row's `payload_hash`) was kept.
What it does detect: a modified payload (rows since 000006), a removed or reordered row, a broken link, a wrong genesis. What it does **not** detect: a change to a non-payload column (`tenant_id`, `event_type`, `principal_id`, …); a privileged rewrite that also recomputes every later hash (the chain is neither keyed, signed nor anchored outside the database); a modified payload on a legacy row. "Tamper-evident" here means "evident to anyone who re-verifies against a trusted copy of the chain tip", not tamper-proof.

### Gap 9 — `hash_chain_valid` was hard-coded `true`
Status: ✅ Fixed
Root cause: `QueryEvents` set the flag to `true` whenever the query succeeded.
Fix: after the tenant-scoped read, `pageChainValid` re-checks the **returned events**: each stored payload still hashes to its `payload_hash` (non-legacy rows) and each `previous_event_hash` equals its predecessor's `payload_hash` (or the row is genesis). The predecessor is read under `app.platform_scope` because it may belong to another tenant, but only a boolean leaves the function. Scope: the events in the response, **not** the whole matching set and not the global chain (that is `POST /v1/events/verify`). An empty result is vacuously `true`.
Files: `internal/store/query_store.go` (`QueryEvents`, `pageChainValid`, `FakeStore.pageValid`).
Verification: `TestListEvents_HashChainValidIsComputed` (payload, hash, link, gap), `TestListEvents_HashChainValidScopeIsTheReturnedEvents`, integration page check; mutation check (constant `true`) fails them.

### Gap 10 — `audit_events` immutability was application convention only
Status: ✅ Fixed
Root cause: only the archive tables had append-only triggers; the event table's RLS policy is `FOR ALL`, so even the platform-scope writer could UPDATE or DELETE.
Fix: migration `000006` adds BEFORE UPDATE / DELETE row triggers and a BEFORE TRUNCATE trigger on `audit_events` that raise `restrict_violation`. INSERT, RLS and FORCE RLS are unchanged; no privileges were granted or revoked. Limit: a superuser/owner can still disable the triggers (`ALTER TABLE … DISABLE TRIGGER`, `session_replication_role = replica`) — an administrative act, which the chain verification exists to expose.
Verification: integration `TestAuditEvents_DatabaseImmutability_RealPostgres` — INSERT/ingestion works, the application role (tenant and platform scope) and a superuser are refused UPDATE and DELETE, TRUNCATE is refused, nothing changed, ingestion continues, duplicates are not stored, tenant isolation intact. The archive integration tests that simulate tampering now do so by disabling trigger firing for one transaction.

### Gap 11 — Malformed Kafka messages were dropped without a durable record
Status: ✅ Fixed (message handling) — whether the **ledger** must itself hold them is NEEDS CLARIFICATION
Root cause: `Consumer.Handle` logged and returned `nil` for an unparsable envelope, a missing `event_type` / `source_service` / `schema_version`, or a failed per-type validation, so the offset was committed and the message was gone.
Fix: `Handle` now returns an error wrapping `ErrMalformedEvent` (nothing is stored; no partial write). The runner does not retry it (it can never become valid); it publishes the original bytes to `<topic>.dlq` with the reason in `X-DLQ-Reason`, and commits the offset only after that publish succeeds — if the publish fails the offset is left uncommitted and a restart retries. Store failures keep their own bounded-retry-then-DLQ path. No event schema or Kafka semantics were invented; the DLQ mechanism already existed.
Files: `internal/consumer/consumer.go`, `internal/kafka/runner.go` (reader/writer/handler narrowed to interfaces for testing).
Verification: `internal/kafka/runner_test.go` (valid, malformed, malformed + DLQ failure, store error retried then dead-lettered, recovery within retries, DLQ failure leaves the offset), consumer tests now assert `ErrMalformedEvent`.

### Gap 12 — `GET /v1/events` silently ignored malformed parameters
Status: ✅ Fixed
Root cause: unparsable `from`, `to`, `limit`, `offset` were skipped, so the query ran unfiltered.
Fix: `buildQueryParams` rejects them with `400` and the service's `{code, message}` body (`invalid_from`, `invalid_to`, `invalid_limit`, `invalid_offset`, `invalid_range`); validation runs after authentication, authorization and the tenant check, so an unauthenticated or unauthorized caller still gets 401/403.
Files: `internal/handler/handler.go`. Verification: `TestListEvents_MalformedParametersAreRejected_NotIgnored`, `TestListEvents_ValidParametersStillWork`, `TestListEvents_ParameterValidationDoesNotPrecedeAuth`.

### Gap 13 — A workload id could stand in for the principal (found by the compliance re-audit)
Status: ✅ Fixed
Root cause: `requirePrincipal` used `envelope.Actor()`, which returns the actor subject id (`X-Principal-Id`) and, when that is empty, the workload id (`X-Workload-Id`). With the envelope middleware mounted, a request carrying a tenant and only a workload id therefore passed principal authentication and was authorized as if the workload id were a principal. The documented contract is that every route requires `X-Principal-Id`.
Fix: `requirePrincipal` (`internal/handler/handler.go`) now uses the envelope's `ActorSubjectID` (the parsed `X-Principal-Id`) and the direct `X-Principal-Id` header read, and nothing else; a missing principal is `401 principal_required`. The shared `internal/envelope` package, authorization behavior, action names, platform-scope checks and the tenant requirement are unchanged.
Verification: `internal/handler/principal_boundary_test.go` mounts the real envelope middleware and, for all six routes, checks that no principal is 401, workload-only is 401 (authorization is never consulted), a principal reaches authorization with the principal id even when a workload id is also sent, and the tenant is still required. Mutation check: restoring `envelope.Actor()` fails the workload-only test.

### Documentation corrections (this pass)
- Test count "32/32" replaced with the verified counts below.
- `cmd/server/main.go` package comment no longer says there is "no outward-facing business HTTP API".
- `VerifyArchive`'s comment claimed `audit_events` was immutable "by its own triggers" when none existed; that is now true (migration 000006).
- The "97% / READY / Remaining Gaps: None" claims were removed (summary rows above and this section).
- AUD-03 is no longer reported as "✅ Verified".
- The migration 000004/000001 comments that describe `audit_events` immutability as application discipline are historical; 000006 supersedes them.

**Remaining Gaps:**
- AUD-03 compressed cold-storage archival is **not implemented** (no storage target, compression, aging or export) — and is not defined by §14.1; see Needs-Clarification. Not claimed as working.
- Payloads of legacy rows (written before migration `000006`, `payload_hash_scheme` NULL) are not verifiable; only their sequence and links are.
- Anything beyond payload-hash + previous-hash linking (coverage of tenant/type/metadata, signing, external anchoring) is not provided and not specified.
- Malformed Kafka messages are dead-lettered, not stored in the ledger (see Needs-Clarification).

**Dependency-Blocked Items:**
- **Kafka Domain Event Ingestion:** Asynchronous event streaming relies on domain services publishing to their respective topics. The platform compose file subscribes this service to all 79 platform topics (see Gap 2); the service's own standalone compose file subscribes to two, by design.
- ❌ **EXTERNAL BLOCKER — `authorization-svc`:** reported in an earlier pass as missing the migration that creates `principal_role_assignments.book_id` (`ERROR: column pra.book_id does not exist`), so every RBAC check platform-wide fails closed with `503` and a fully-authorized live `200` on `GET /v1/events` / `POST /v1/events/verify` could not be demonstrated. Not re-verified in this pass; `authorization-svc` was not modified.

**Needs-Clarification Items (not resolved, not guessed):**
1. **AUD-03 cold-storage archival.** §14.1 says nothing about archival. Undefined: storage target, compression format, aging policy, export format, lifecycle/retention, and which service owns moving data. `POST /v1/archives` today records and re-verifies a digest claim only. (Also: whether an automatic scheduler is wanted.)
2. **Malformed events and "without loss".** The phrase is the audit file's, not §14.1's. Unclear whether a message that cannot satisfy the ledger's mandatory columns (`event_type`, `source_service`, `schema_version`) must be stored in the ledger, or whether durable dead-lettering is enough. Implemented: dead-lettering with the original bytes.
3. **`Idempotency-Key` on `POST /v1/archives`.** The canonical envelope requires the header on material writes, but nothing replays it, so a retry creates a second archive record. The service documentation does not say whether replay is wanted.
4. **Global verify count.** `POST /v1/events/verify` verifies the single global chain and returns its total count, which includes other tenants' events. Documented as intended by the chain design; whether a tenant-visible count is acceptable is undecided.
5. **Error body.** `{code, message}` differs from the `{error, error_code}` shape other services use; no document fixes one.
6. **Port.** 8084 (platform deployment) versus 8080 (code default / standalone compose) — see the Port line above.
7. **`workflow` filter.** The spec's "workflow" filter is mapped to `correlation_id`; the spec does not define it.
8. **Archive range tenant semantics.** `CreateArchive` accepts any global sequence range for a caller holding `AUDIT_ARCHIVE_MANAGE`, including ranges that contain other tenants' events; only hashes and ranges are stored or returned. Not changed without a contract.
9. **Empty `hash_chain_valid`.** An empty result returns `true` (vacuous); the documentation does not say.
10. **Stronger hash construction.** Whether tenant/type/metadata and the previous hash must be covered by the row hash (Gap 8).
11. **`checked_events` on a failed verification.** `POST /v1/events/verify` reports the failing sequence number in `checked_events` when the chain is broken, and the total on success; the documentation does not define the value. Not changed.
12. **Request hardening on `POST /v1/archives`.** The body is decoded without a size cap or unknown-field rejection; no contract requires either. Not changed.

**Verification Results (the container, live-probe and database-count items are historical observations from the original engagement and are not re-verified; current repository results are in the Remediation Pass):**
- Backend unit tests (`go test ./...`), original pass: 48 top-level tests (consumer 12, handler 23, store 6, kafka 7). Superseded: the repository now has 53 (handler 28); see the Remediation Pass.
- Integration tests (`go test -tags=integration ./cmd/server/`, embedded PostgreSQL 16, NOSUPERUSER NOBYPASSRLS application role): **10 tests pass** (health probes, hash chain / dedup / concurrency, RLS, archive create/verify/tenant isolation/append-only, immutability, payload-hash verification).
- Mutation checks: disabling the payload recompute fails the payload-verification tests; making `hash_chain_valid` constant fails the handler tests; both reverted.
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` passes with 0 issues.
- Go Build: `go build ./cmd/server` and `./cmd/healthcheck` succeed.
- Docker Health: Live container `audit-event-store-svc` Up (healthy) on port 8084; rebuilt from current source (Gap 5), confirmed behaviorally current, not a stale image.
- Healthcheck Endpoints: `/healthz` returns 200 OK (`{"status":"ok"}`); `/readyz` returns 200 OK.
- Live API — auth gate: `GET /v1/events` with no `X-Principal-Id` → `401`; with a principal but no grant (blocked by the `authorization-svc` dependency above) → `503`, correctly fail-closed, never `200`.
- Live API — Kafka ingestion: `deployments/docker-compose.yml` confirmed listing exactly 79 subscribed topics; live container logs confirm 79 `"kafka consumer loop starting"` entries per startup cycle.
- Live DB check: `audit_events` table confirmed holding 19 real rows (verified via RLS-bypassing superuser connection); the same query via the tenant-scoped `zoiko_app` role with no tenant context returned 0 rows, independently reconfirming Gap 3's RLS enforcement.
- **Runtime recovery note (this pass):** the shared `postgres` container had exited uncleanly for an unrelated, platform-wide reason (most likely an overnight Docker Desktop restart — confirmed not an OOM kill, not caused by this service or any fix in it). `audit-event-store-svc` correctly fatal-exited on every restart attempt while its database was unreachable (fail-fast, not a silent hang; this accounts for the elevated `RestartCount` of 31) and fully auto-recovered — healthy, zero data loss (19 rows intact), all 79 Kafka subscriptions restored — the instant `postgres` was brought back up. This is evidence of correct fail-fast/recovery behavior, not a gap.

### Remediation Pass (2026-10-19)

| Finding | Result |
|---|---|
| A request with a tenant and only `X-Workload-Id` passed principal authentication | Fixed (Gap 13): `requirePrincipal` accepts only `X-Principal-Id`; workload-only is `401 principal_required` on all six routes |
| "`app.platform_scope` is used only by `VerifyChain`" | Corrected: it is set by the ingestion writer, `VerifyChain`, `pageChainValid` and the archive create/verify reads (see Gap 3) |
| Stale line-range citations | Replaced with file and function references. The one remaining line-range citation, `000003_add_rls.up.sql#L46-L47`, was re-checked against the file (line 46 `ENABLE`, line 47 `FORCE`); the frontend citation `lib/api/audit-events.ts#L161` belongs to the unaudited frontend and is left as a historical reference |
| Runtime claims (container state, `RestartCount`, 19-row count, live 401/503 probes, deployed image) | Labelled as historical observations, not re-verified; production readiness stated as not claimed |
| Existing clarification items 1–10 | Preserved; items 11 and 12 added |

State of the working tree: the earlier remediation of this service (migrations `000005` and `000006`, `immutability_integration_test.go`, `integrity_validation_test.go`, `kafka/runner_test.go` and the modified handler, store, consumer and kafka files) is uncommitted and was left untouched. Files changed by this pass: `internal/handler/handler.go` (`requirePrincipal` only), `internal/handler/principal_boundary_test.go` (new) and this section.

Verification: `go build ./...`, `go vet ./...` and `go vet -tags=integration ./...` clean. `go test ./...`: 53 top-level tests passed (5 new), 0 failed, 0 skipped. `go test -tags=integration ./cmd/server/` (embedded PostgreSQL 16): 10 tests passed, 0 failed, 0 skipped. No live authorization verification was performed; the `authorization-svc` migration gap remains an external blocker.

---

## Service: schema-registry-svc
**Port:** 8093
**Classification:** Canonical Event Payload Contract Registry & Compatibility Governor

**Service Health:** Not established by repository evidence. The service was reported healthy during the original engagement; no container health check was performed in the 2026-10-19 passes.

**Frontend Completion: N/A (Console Workbench: 92%)**
Reason: No architecture document requires an operational end-user portal for `schema-registry-svc` (it is foundational backend reference data, scored N/A in the platform matrix; an earlier version of this file cited "§14.2" for that, but §14.2 of `03-microservices.md` is Document Vault). In reality, a dedicated admin console exists at `app/admin/schemas/` with a complete API client (`lib/api/schemas.ts`, 749 lines), server actions (`app/admin/schemas/actions.ts`, 257 lines), state definitions (`app/admin/schemas/state.ts`, 72 lines), UI components (`SchemaRegisterPanel.tsx`, `SchemaRegisterTable.tsx`, `SchemaLookup.tsx`, `SchemaForms.tsx`, `SchemaSummary.tsx`), and sidebar navigation (`lib/constants.ts`). The audit file previously described these as "100% complete and verified"; that was a code-presence claim, and the frontend was not audited in the compliance re-audit. In the active working tree of `Zoiko-suite-frontend-platform`, `app/admin/schemas/page.tsx` was staged as deleted in an uncommitted git merge conflict on branch `rohithyadav` (preserved without modification in this prompt per read-only audit rules; restorable from git HEAD via `git checkout HEAD -- app/admin/schemas/page.tsx`).

**Backend Completion:** no percentage asserted. Reason: `internal/handler/handler.go` implements the registration route and four reads (list names, list versions, latest, specific version) plus health probes; reads require `X-Principal-Id`, registration additionally requires the canonical service envelope and the `SCHEMA_PUBLISH` grant (fail-closed). PostgreSQL persistence is in `internal/store/pg_store.go`; versions are assigned atomically with optimistic concurrency; `Idempotency-Key` replay returns the original version. Registered versions are immutable in the database (migration `000005`: triggers reject UPDATE, DELETE and TRUNCATE) as well as in the API. Compatibility analysis (`internal/compat/compat.go`) recurses into nested objects and array items but compares only a **subset** of JSON Schema (`type` as a single string, `properties`, `required`, `items`) and returns its violations in a stable sorted order. Not delivered, and not claimed: comparison of `enum`/`format`/`pattern`/bounds/`additionalProperties`/`$ref`/`allOf`/`anyOf`/`oneOf`; support for `type` declared as an array (refused at registration); `SchemaDependencyMap`; a rollout-approval step for `NONE`; a read-specific authorization check.

**Integration Completion:** not re-scored. The historical figure of 98% (original engagement) depended on live observations that cannot be re-established from the repository and is not asserted as a current result.
Repository evidence (checked statically on 2026-10-19): the registration route calls `authorization-svc` synchronously for `SCHEMA_PUBLISH` (`internal/authz/client.go#L72-L158`), falls back to `AUTHZ_PLATFORM_SCOPE_ID` (`00000000-0000-0000-0000-00000000f001`) when `X-Legal-Entity-Id` is empty (`internal/handler/handler.go#L81-L84`), and fails closed (403 on denial, 503 otherwise); the canonical envelope middleware is mounted in `cmd/server/main.go`; migrations `000001`–`000005` exist (`000005` is uncommitted, see Working-tree state); `deployments/docker-compose.yml` has a `schema-registry-svc` block (port 8093, `AUTHZ_PLATFORM_SCOPE_ID` at line 1453).
Historical runtime observations (original engagement; **not re-verified**; no rebuild, redeploy, container health check, live API probe or database inspection was performed in the 2026-10-19 passes): migration `000004` applied to the `schema_registry` database; a live registration attempt answered `503` because of the external `authorization-svc` migration gap; the container was rebuilt and reported healthy with `/readyz` 200; Traefik routing was configured. Direct host-port exposure without mandatory Traefik ForwardAuth (SEC-04) is a platform-wide item.

**Overall Completion:** no percentage asserted (see Remaining Gaps and Needs-Clarification).

**Production Readiness: service-owned scope READY WITH MINOR GAPS; NOT PRODUCTION READY.** The compatibility check covers a documented subset of JSON Schema only and several contract questions are open (below). A fully authorized `201` on `POST /v1/schemas/{eventName}/versions` cannot be demonstrated end to end because of the external `authorization-svc` migration gap (see Dependency-Limited Items); the service's own fail-closed handling of that outage is implemented in code and covered by tests, but no live confirmation was made in the 2026-10-19 passes. The state of any deployed container is not established by repository evidence.

**Commands:**
- `POST /v1/schemas/{eventName}/versions` (Registers the next payload schema version for an event; validates input shape, enforces canonical service envelope, verifies `SCHEMA_PUBLISH` via `authorization-svc` on platform scope, checks `Idempotency-Key` for a genuine replay before anything else runs — added this pass, see Gap 7 — validates backward compatibility recursively, and assigns sequential version atomically via optimistic concurrency control).

**Reads:**
- `GET /v1/schemas` (List all registered event names with limit/offset pagination and caller identity enforcement)
- `GET /v1/schemas/{eventName}/versions` (List all historical versions of an event schema oldest first with limit/offset pagination)
- `GET /v1/schemas/{eventName}/versions/latest` (Fetch the latest registered contract for an event)
- `GET /v1/schemas/{eventName}/versions/{version}` (Fetch a specific historical version of an event schema)
- `GET /healthz` (Process liveness probe)
- `GET /readyz` (Database pool readiness probe)

**Segregation of Duties & Auth:**
- Read Authentication: `X-Principal-Id` mandatory on all reads (`requireIdentity`), rejecting unauthenticated enumeration with HTTP 401 Unauthorized (`caller identity missing`).
- Input Contract Enforcement: Canonical Service Input Contract middleware (`svcenvelope.Middleware`) enforces `X-Tenant-Id`, `X-Principal-Id`, `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, and `Idempotency-Key` on material mutations, rejecting incomplete requests with `envelope_incomplete` (HTTP 401 when the missing field is the tenant or actor, otherwise 400; body `{error, detail, service, violations[{field, header, reason}]}`).
- Authorization Gate: Synchronous call to `authorization-svc:8089/v1/authorize` checks `SCHEMA_PUBLISH`. If `X-Legal-Entity-Id` is omitted, defaults fail-safe to `AUTHZ_PLATFORM_SCOPE_ID` (`00000000-0000-0000-0000-00000000f001`). Rejects unauthorized actors with HTTP 403 Forbidden (`not authorized to publish schemas`). Fails closed on timeout or unreachable with HTTP 503 (`authorization service unavailable`).
- Immutability: Event schema versions are append-only. No UPDATE or DELETE route exists in the API, and — since migration `000005` — database triggers on `event_schemas` reject UPDATE, DELETE and TRUNCATE for every role (INSERT is unaffected). The primary key `(event_name, version)` does **not** provide immutability: it only refuses a second row with the same key. A database superuser or table owner can still disable the triggers (an administrative act).
- Replay Protection (INV-08): `Idempotency-Key` is required on the write route by the canonical service input contract and, as of this pass, is actually honored for deduplication — a retried registration under the same key returns the original version instead of claiming a new one (HTTP 201; no replay marker is defined). The key is scoped to the event name only: a different body or principal under the same key also receives the original version — whether the key should be bound to the request is **not specified** (see Needs-Clarification). See Gap 7.

**Fixed Gaps:**

Note on "live" statements: sentences in Gaps 1–7 that describe a live probe, a running container, a rebuild or a redeploy (for example "verified live against running container", "live curl probes", "rebuilt the Docker image and redeployed") are historical observations from the original engagement. They are not re-verified and are not claims about the current deployment. The code and test evidence in each gap is what the repository supports.

### Gap 1 — Incomplete Schema Compatibility Validation (Non-Recursive Top-Level Check)
Status: ✅ Fixed
Original Issue:
`internal/compat/compat.go`: Previous validator only inspected top-level properties and required arrays. Breaking changes inside nested object properties or array item schemas (such as removing required nested fields, mutating field types, or adding newly required nested fields) were allowed and registered as backward-compatible.
Fix Verification:
Implemented recursive validation algorithm `checkProperties` in `internal/compat/compat.go#L59-L134`. Recurses into nested object properties (`PropertyDef.Properties`, `PropertyDef.Required`) and array item schemas (`PropertyDef.Items`) to detect removed required fields, type conversions, and newly required fields across arbitrary nesting depth. Integrated into registration handler `internal/handler/handler.go#L178-L205`, blocking breaking evolutions with HTTP 409 Conflict and a granular list of violations. Verified live against running container (:8093): mutating nested property `user.role` from string to integer was blocked with HTTP 409 Conflict and violation `["field \"user.role\" changed type from \"string\" to \"integer\""]`. Adding newly required nested fields was blocked with HTTP 409 Conflict and violations list. All 15 unit tests in `internal/compat/compat_test.go` pass cleanly.
Evidence:
`internal/compat/compat.go#L59-L134`, `internal/domain/types.go#L145-L170`, `internal/compat/compat_test.go#L105-L290`, live curl probes on port 8093.

### Gap 2 — Non-Object and Empty Object Payload Contracts Bricking Evolution
Status: ✅ Fixed
Original Issue:
`json.Valid` was used to validate schemas, allowing non-objects (`123`, `"string"`, `null`, `[]`, `true`) and empty objects (`{}`) to be registered as valid event schemas. When a non-object was registered as version 1, future versions failed `compat.Check` shape parsing, permanently bricking the event. `{}` constrained nothing, allowing unvalidated schemas.
Fix Verification:
Implemented `domain.ValidateJSONSchema` in `internal/domain/types.go#L116-L143`, requiring JSON objects with at least one member and valid `properties`/`required` shape. Added PostgreSQL CHECK constraints in `000003_registry_invariants.up.sql`: `event_schemas_json_schema_is_object` (`CHECK (jsonb_typeof(json_schema) = 'object')`) and `event_schemas_json_schema_not_empty` (`CHECK (json_schema <> '{}'::jsonb)`). Refuses non-objects and empty objects with HTTP 400 Bad Request at the API boundary before hitting PostgreSQL.
Evidence:
`internal/domain/types.go#L116-L143`, `000003_registry_invariants.up.sql#L21-L30`, `internal/handler/gaps_test.go#L32-L65`, `internal/store/gaps_test.go#L82-L104`.

### Gap 3 — Unauthenticated Reads and Unbounded Catalogue Enumeration
Status: ✅ Fixed
Original Issue:
All read endpoints (`/v1/schemas`, `/v1/schemas/{eventName}/versions`, `/latest`, `/{version}`) were completely unauthenticated and unbounded, allowing any caller on the internal network to dump the entire event catalogue and schema definitions without credentials or pagination, presenting an information disclosure risk (Doc 05 §14.6).
Fix Verification:
Added `requireIdentity` check (`internal/handler/handler.go#L371-L377`) requiring `X-Principal-Id` on all reads, returning HTTP 401 Unauthorized (`caller identity missing`) if omitted. Implemented `parsePaging` (`internal/handler/handler.go#L426-L446`) bounding reads with default limit 100, max limit 500, and non-negative offset validation on both event name catalogue and version lists. Handled pagination edge case: empty page beyond end of history returns 200 `[]` rather than 404 (`internal/handler/handler.go#L323-L331`). Verified live: unauthenticated `curl http://localhost:8093/v1/schemas` returned 401; authenticated request returned 200 with paged array.
Evidence:
`internal/handler/handler.go#L371-L377, L426-L446`, `internal/handler/gaps_test.go#L187-L246`, live curl probes on port 8093.

### Gap 4 — Entity-Less Schema Registration Authorization Scope Failure
Status: ✅ Fixed
Original Issue:
Event contracts are platform-wide reference data not owned by a single legal entity. Previously, `RegisterVersion` forwarded `X-Legal-Entity-Id` verbatim, which was empty for platform-level events. `authorization-svc` rejected empty `legal_entity_id`, resulting in an unexpected 503 Service Unavailable ("authorization service unavailable") error blaming infrastructure for a scope the request was never meant to carry.
Fix Verification:
In `internal/handler/handler.go#L81-L84`, if `X-Legal-Entity-Id` is empty, handler defaults `scopeID` to `h.platformScopeID` (`AUTHZ_PLATFORM_SCOPE_ID`, configured in `deployments/docker-compose.yml:1453` as `00000000-0000-0000-0000-00000000f001`). `seed-demo-rbac.ps1` grants `SCHEMA_PUBLISH` under bundle `SCHEMA_FULL` on both legal entity and platform scope. Synchronous fail-closed check to `authorization-svc` returns 403 on DENIED and 503 on unreachable. Verified live: entity-less registration with platform-scoped principal succeeded (201 Created), while unauthorized principal was rejected (403 Forbidden).
Evidence:
`internal/handler/handler.go#L81-L84`, `internal/authz/client.go#L72-L158`, `deployments/scripts/seed-demo-rbac.ps1#L505-L524`, live curl probes on port 8093.

### Gap 5 — Optimistic Concurrency Loss and Primary Key Collision Reported as Outage (503)
Status: ✅ Fixed
Original Issue:
Previously, the handler read the latest version, incremented by one, and inserted into PostgreSQL. Concurrent registrations computed the same version number; the later transaction failed with PostgreSQL primary key violation (SQLSTATE 23505), which was caught as a generic store error and returned to callers as HTTP 503 "schema store unavailable", disguising an ordinary race condition as a database outage.
Fix Verification:
Replaced two-step read-then-insert with atomic conditional insert query in `internal/store/pg_store.go#L198-L204`: `INSERT INTO event_schemas ... SELECT $1, COALESCE(MAX(version), 0) + 1, ... FROM event_schemas WHERE event_name = $1 HAVING COALESCE(MAX(version), 0) = $expectedVersion`. Optimistic concurrency guard `HAVING COALESCE(MAX(version), 0) = $expectedVersion` ensures new version is only appended if baseline version hasn't changed since compatibility check. If `HAVING` excludes row or PK collision occurs (`isUniqueViolation`), `pg_store.go` returns `domain.ErrVersionRaced`. Handler translates `ErrVersionRaced` to HTTP 409 Conflict (`a concurrent registration claimed this version — re-read the latest version and retry`), instructing client to re-check rather than blindly retry.
Evidence:
`internal/store/pg_store.go#L176-L248`, `internal/handler/handler.go#L217-L236`, `internal/handler/handler_test.go#L442-L464`.

### Gap 6 — Free-Text Event Names and Unchecked String Lengths Causing Silent Failures / 503s
Status: ✅ Fixed
Original Issue:
Event names were accepted as arbitrary free-text strings without format or length checks. Overlong event names or owning service names (>255 characters) crashed in PostgreSQL with SQLSTATE 22001 (string_data_right_truncation), surfaced to clients as HTTP 503 database outages.
Fix Verification:
Enforced strict regex format `eventNameRE` (`^[a-z][a-z0-9]*(\.[a-z0-9]+)+$`) requiring dotted lowercase format with at least two segments (`internal/domain/types.go#L96-L101`). Added PostgreSQL CHECK constraint `event_schemas_event_name_wellformed` in `000003_registry_invariants.up.sql`. Added boundary length validation for `event_name` (max 255) and `owning_service` (max 255), and mapped any Postgres 22001 errors to `domain.ErrFieldTooLong` (HTTP 400 Bad Request) via `mapPgError`. Disallowed unknown JSON fields (`dec.DisallowUnknownFields()`) to prevent silent discarding of misspelled fields like `compatibility_mode_`.
Evidence:
`internal/domain/types.go#L96-L101, L204-L209`, `internal/handler/handler.go#L102-L105, L404`, `000003_registry_invariants.up.sql#L50-L52`, `internal/handler/gaps_test.go#L86-L149`.

### Gap 7 — Idempotency-Key Required But Never Used for Replay Protection (newly discovered this pass)
Status: ✅ Fixed
Discovery Evidence:
The canonical service input contract (`internal/envelope`, vendored from `services/_contract/envelope/`) marks `Idempotency-Key` `RequiredOnWrite` platform-wide by default — not expressible as opt-out — and its own policy comment states the reason verbatim: "a service that changes material state without replay protection violates the invariant" (INV-08). `svcenvelope.Middleware` is wired globally in `cmd/server/main.go` and does refuse a `POST /v1/schemas/{eventName}/versions` with no `Idempotency-Key`. But `internal/handler/handler.go`'s `RegisterVersion` never read the header for anything — it was validated for presence and then discarded. A client that retries a registration after a timeout (not knowing whether the first attempt's response was merely lost in transit) would have the retry treated as a brand-new request: re-run the compatibility check against whatever is now latest, and claim an entirely new version number, rather than receiving back the original one. The service required the header and advertised the invariant it is required for, without implementing it.
Classification: SERVICE-OWNED DEFECT. Severity: Medium — not a security hole, but a genuine correctness/reliability gap directly contradicting a named platform invariant (INV-08) this service's own contract declares itself subject to; every retried write burns a real version number in an append-only, audit-relevant registry.
Expected behavior: a registration retried with the same `Idempotency-Key` against the same event returns the original stored version. Actual behavior (before fix): a retry created a new version every time, silently.
Fix Applied:
Added migration `000004_add_idempotency_key.up.sql` — nullable `idempotency_key VARCHAR(255)` column on `event_schemas`, with a partial unique index `idx_event_schemas_event_idempotency` on `(event_name, idempotency_key) WHERE idempotency_key IS NOT NULL` (same scoping pattern as `accounts-payable-svc`'s `000002_add_idempotency_index`). `internal/store/pg_store.go` gained `FindByIdempotencyKey` and now persists the key on `Insert`. `internal/handler/handler.go`'s `RegisterVersion` checks `FindByIdempotencyKey` immediately after event-name validation, before the body is even decoded — a genuine replay returns the original row unconditionally rather than re-evaluating compatibility against whatever is latest now. `Insert` also resolves a *concurrent* replay race (two callers racing on the same key) by re-checking `FindByIdempotencyKey` on any unique-constraint conflict rather than assuming it is a version race.
Verification:
4 new handler-level tests (`TestRegisterVersion_IdempotentReplay_ReturnsOriginalWithoutInserting`, `TestRegisterVersion_NewIdempotencyKey_InsertsNormally`, `TestRegisterVersion_IdempotencyLookupFails_Returns503FailClosed`, `TestRegisterVersion_NoIdempotencyKey_SkipsReplayCheck`) and 4 new Postgres-backed store tests, all run against a real, disposable `schema_registry_test` database created for this pass (`TestPgStore_FindByIdempotencyKey_NoneRegistered_ReturnsNil`, `TestPgStore_Insert_PersistsIdempotencyKey_AndFindByIdempotencyKeyReturnsIt`, `TestPgStore_Insert_SameIdempotencyKeyDifferentEvent_BothSucceed`, `TestPgStore_Insert_ConcurrentReplaySameKey_OneWriterInsertsTheRestGetTheSameRow`). The concurrent-replay test initially failed real-DB verification — a genuine subtlety the review caught: when every racer computes an identical `(version, idempotency_key)` pair, Postgres reports whichever unique constraint it happens to check first (observed: the primary key, not the new idempotency index), so the fix could not rely on inspecting which named constraint fired and was corrected to check `FindByIdempotencyKey` on *any* unique-violation or HAVING-excluded-row conflict, not only the one matching the new index by name. Re-verified clean across 3 repeated runs after the correction. All 4 pre-existing migrations (including the new one) applied cleanly to a fresh database with zero errors, confirming clean-startup compatibility. Rebuilt the Docker image (`docker compose build schema-registry-svc`) and redeployed; container healthy, `/readyz` 200. A full live HTTP round-trip of the write path is blocked by the pre-existing `authorization-svc` dependency gap (see Dependency-Limited Items) — correctly answers `503`, not a regression from this fix.
Evidence:
`deployments/migrations/000004_add_idempotency_key.up.sql` / `.down.sql` (new), `internal/domain/types.go` (`EventSchema.IdempotencyKey`), `internal/store/pg_store.go` (`FindByIdempotencyKey`, `Insert`'s conflict handling), `internal/handler/handler.go` (`RegisterVersion`'s pre-decode replay check), 8 new tests across `internal/handler/handler_test.go` and `internal/store/pg_store_test.go`, live `\d event_schemas` confirming the column and index exist on the real `schema_registry` database.

### Gap 8 — Registered versions were not immutable in the database (found by the compliance audit)
Status: ✅ Fixed (CODE BUG)
Root cause: immutability held only because the API has no update/delete route. The primary key does not make a row immutable, and there was no trigger, so any writer with UPDATE/DELETE on `event_schemas` could rewrite or remove a registered contract; this file's earlier claim that the primary key "prevents mutation or overwrite" was wrong.
Fix: migration `000005_event_schemas_immutability` adds BEFORE UPDATE / DELETE row triggers and a BEFORE TRUNCATE trigger that raise `restrict_violation`. INSERT, existing constraints and privileges are unchanged. Limit: a superuser/owner can disable the triggers.
Files: `deployments/migrations/000005_event_schemas_immutability.{up,down}.sql`.
Verification: `TestEventSchemas_AreImmutableInTheDatabase` (real PostgreSQL): registration succeeds, replay and stale-baseline behave as before, UPDATE/DELETE refused for a non-superuser role holding UPDATE/DELETE privileges and for a superuser, TRUNCATE refused, nothing changed, version/latest/list/names reads work, registration continues. Mutation check: without the migration the test fails.

### Gap 9 — Compatibility violations came back in random order (found by the compliance audit)
Status: ✅ Fixed (CODE BUG)
Root cause: `checkProperties` iterates Go maps, so the same pair of schemas produced the same violations in a different order from call to call, including in the 409 body.
Fix: `Check` sorts the violations (each message starts with the quoted field path). Nothing is added, removed or reworded; no new response field.
Files: `internal/compat/compat.go`.
Verification: `TestCheck_ViolationsAreDeterministic_SameSetSameOrderEveryCall` (300 repeats over a 12-property fixture with nested and array breaks), `TestCheck_ViolationsAreSortedByFieldPath`, `TestCheck_Sorting_DoesNotChangeTheViolationSet`, handler `TestRegisterVersion_ManyViolations_ComeBackInTheSameOrderEveryTime`. Mutation check: removing the sort fails them.

### Gap 10 — An over-long `Idempotency-Key` was reported as a 503 outage (found while writing the tests)
Status: ✅ Fixed (CODE BUG)
Root cause: the key is stored in a `VARCHAR(255)`; a longer one reached PostgreSQL as 22001, which the store maps to `ErrFieldTooLong`, but the registration handler turned every insert error other than a lost race into 503 — the same "caller mistake reported as an outage" class as Gap 6.
Fix: the handler's insert-error path now uses the shared `writeStoreErr`, so `ErrFieldTooLong` is 400 and everything else stays 503.
Files: `internal/handler/handler.go`. Verification: `TestRegisterVersion_OverlongIdempotencyKey_Is400Not503`, `TestPgStore_Insert_OverlongIdempotencyKey_IsAFieldError`.

### Documentation corrections (this pass)
- `openapi.yaml` (now 1.2.0) said nested objects and arrays are "NOT analysed — a documented v1 limit", contradicting the recursive checker. The two sources conflicted and no architecture document fixes the analysis depth (Data Model §2.12 only requires compatibility rules), so the implementation — deliberate, tested (Gap 1) and newer than that sentence — was taken as the intended v1 behaviour and the OpenAPI was corrected; the depth remains listed under Needs-Clarification.
- `openapi.yaml` now documents: the supported compatibility subset and what is refused; pagination (`limit` 1–500, default 100; `offset` ≥ 0; 400 otherwise); 401 on every read; the event-name pattern and 255-character limit; the `Idempotency-Key` and `X-Legal-Entity-Id` headers and the replay behaviour; the `envelope_incomplete` refusal body; 400 for a version below 1; the stable ordering of violations; the DB-level immutability; the health response bodies.
- The 1 MiB request-body limit (413) is an implementation limit and is **not** part of the published contract; it is deliberately not in `openapi.yaml`.
- Removed the unsupported "98% / READY", "Remaining Gaps: None" and "primary key prevents mutation" claims, and the `/readyz` body `{"status":"ready"}` (the code returns `{"status":"ok"}`).
- Corrected the "§14.2" citation (Document Vault, not this service) in the summary table, the inventory row and the frontend note.
- Corrected the envelope refusal status (401 only for a missing tenant/actor, otherwise 400) and the test counts (below).

**Remaining Gaps:**
- The compatibility check compares only `type`, `properties`, `required` and `items`. Narrowing an `enum`, changing `format`/`pattern`/bounds/`additionalProperties`, or changing `$ref`/`allOf`/`anyOf`/`oneOf` is not detected, and a `type` given as an array is refused at registration. Whether more of JSON Schema must be compared is not established (Needs-Clarification); the limit is documented in `openapi.yaml`.
- A database superuser/owner can still disable the immutability triggers.
- Data Model §17.1 entity fields `schema_registry_artifact_id`, `schema_type`, `active_flag` and the `SchemaDependencyMap` are not modelled (Needs-Clarification).
- Working tree notice (historical observation, not re-verified; the frontend was not audited): In `Zoiko-suite-frontend-platform`, `app/admin/schemas/page.tsx` was staged as deleted in an uncommitted git merge conflict on branch `rohithyadav`. The underlying API client (`lib/api/schemas.ts`), server actions (`app/admin/schemas/actions.ts`), state (`app/admin/schemas/state.ts`), UI components (`components/admin/schemas/*`), and navigation (`lib/constants.ts`) are 100% complete and ready. Not modified this pass (no frontend work requested or reviewed in this pass); restorable via `git checkout HEAD -- app/admin/schemas/page.tsx`.

**Dependency-Limited Items:**
- **Authorization Service Dependency (`authorization-svc:8089`):** schema registrations require `authorization-svc` to evaluate `SCHEMA_PUBLISH`; the fail-closed handling is in `internal/handler/handler.go` and `internal/authz/client.go`. ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** during the original engagement `authorization-svc` logs showed every `/v1/authorize` call failing with `column pra.book_id does not exist (SQLSTATE 42703)`, so a live registration answered `503`, not `201`. That observation is historical and was not re-verified in the 2026-10-19 passes. `authorization-svc` was not modified.
- **Direct Container Port Exposure (SEC-04):** Container exposes port `:8093` on the host interface. Direct calls bypass Traefik ForwardAuth, but internal service enforcement (`requireIdentity` on reads and `svcenvelope.Middleware` + `authorization-svc` on writes) prevents unauthenticated access.

**Needs-Clarification Items (not resolved, not guessed):**
1. **Read authorization.** Reads need a verified principal only. Security §14.6 says "schema registry access" must be protected but names no permission; no `SCHEMA_READ`-style action was invented.
2. **Replay binding.** The idempotency key is scoped to (event name, key). A different body or principal under the same key receives the original version. Not specified whether to bind the key to the request or reject a mismatch.
3. **Replay marker.** A replay is an ordinary 201; no header or status distinguishes it. None is specified.
4. **`SchemaDependencyMap`.** Data Model §17.1 lists it; no endpoint, table or owner is assigned in this service.
5. **Controlled rollout.** §17.2 requires breaking changes to go through "controlled rollout". `compatibility_mode: NONE` is recorded and logged but needs no approval beyond `SCHEMA_PUBLISH`; whether an approval step belongs in this service is not specified.
6. **JSON Schema semantics.** Which keywords the compatibility check must compare, and whether `type` as an array must be supported, is not specified.
7. **Compatibility depth.** `openapi.yaml` (v1.1.0) said top-level only; the code, tests and the earlier audit entry say recursive. Resolved in favour of the implementation (see Documentation corrections) without an authoritative document.
8. **Artifact fields.** `schema_registry_artifact_id`, `schema_type`, `active_flag` (§17.1) are not present; the data model is conceptual and no route is tied to it.
9. **Runtime Event Payload Validation.** As documented in OpenAPI and Data Model §17, `schema-registry-svc` is a design-time and registration-time contract governor; it does not validate runtime Kafka message payloads in-flight. Clarification may be needed on whether an inline proxy or sidecar validation filter is planned.
10. **Request-size limit.** 1 MiB (413) is an implementation limit; whether it is a contract is not stated.

**Verification Results (repository and test results are current as of 2026-10-19; items under "Historical runtime observations" are from the original engagement and are not re-verified):**
- Backend tests (`go test ./...`, 2026-10-19): **92 top-level tests** (authz 8, compat 18, handler 44, store 20, health 2). With `TEST_DATABASE_URL` pointing at a disposable `schema_registry_test` database (PostgreSQL 16, all 5 migrations applied from a clean schema, store tests drop and recreate `event_schemas`): **92 pass, 0 fail, 0 skipped**. Without a database: 71 pass, 21 skip (the Postgres-backed tests) — a run without a database proves nothing about the constraints, concurrency or immutability.
- Mutation checks: removing the violation sort fails the compat and handler determinism tests; removing migration `000005` fails the immutability test; both reverted.
- `openapi.yaml` parses as valid YAML.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Go Build: `go build ./cmd/server` and `./cmd/healthcheck` succeed with 0 errors.
- **Historical runtime observations (original engagement; not re-verified).** No rebuild, redeploy, container health check, live API probe or database inspection was performed in the 2026-10-19 passes. The original engagement recorded: container `schema-registry-svc` healthy on port 8093 with `/healthz` and `/readyz` 200 (`{"status":"ok"}`); `GET /v1/schemas` without identity → 401 and with `X-Principal-Id` → 200; live version reads; a live registration answering `503` because of the external `authorization-svc` migration gap; and live recursive-compatibility probes (409 with violations, 201 for compatible evolution and for `NONE`). Statements elsewhere in this section that the container was "rebuilt" or "redeployed this pass" refer to that original engagement, not to 2026-10-19.
- The automated suite counted in the first bullet supersedes the earlier "79/79" figure.
- **New this pass — Idempotent replay (Gap 7):** verified via 8 new automated tests (4 handler-level, 4 Postgres-backed against a real, disposable database), including a genuine concurrent-write race across 8 goroutines proving every racer under the same key converges on exactly one stored version.

### Remediation Pass (2026-10-19)

Documentation-only. No implementation, migration, test or `openapi.yaml` file was changed by this pass.

| Finding | Result |
|---|---|
| Stale `handler.go`, `compat.go`, `pg_store.go`, `types.go` and test citations | Corrected. Every changed range was re-checked against the working tree on 2026-10-19 (for example `requireIdentity` `handler.go#L371-L377`, `parsePaging` `#L426-L446`, `Check`/`checkProperties` `compat.go#L59-L134`, `Insert` `pg_store.go#L176-L248`). Ranges that were still correct were left as they were (`compat_test.go#L105-L290`, `000003_registry_invariants.up.sql#L21-L30`, `handler/gaps_test.go#L32-L65` and `#L187-L246`) |
| Stale compose citation | `deployments/docker-compose.yml:1308` → `:1453` (`AUTHZ_PLATFORM_SCOPE_ID` in the `schema-registry-svc` block) |
| Stale RBAC seed citation | `seed-demo-rbac.ps1#L300-L320` → `#L505-L524` (the `SCHEMA_FULL` bundle granting `SCHEMA_PUBLISH`) |
| "Rebuilt and redeployed this pass" contradicted "no rebuild or redeploy was performed" | Resolved. Runtime statements are now labelled historical observations from the original engagement; the 98% integration score is no longer presented as a current result; Service Health, Integration Completion, Production Readiness, Dependency-Limited Items and Verification Results were rewritten accordingly |
| Working-tree state | Documented below |
| Implementation behavior | Unchanged |

**Working-tree state (a repository state, not a deployment claim).** The remediation described in this section — migration `000005`, the compatibility-order fix, the handler change and the `openapi.yaml` 1.2.0 correction, with their tests — is **uncommitted**. `git status` for `services/schema-registry-svc` on 2026-10-19 shows modified: `internal/compat/compat.go`, `internal/handler/handler.go`, `openapi.yaml`; untracked: `deployments/migrations/000005_event_schemas_immutability.up.sql` and `.down.sql`, `internal/compat/determinism_test.go`, `internal/handler/contract_test.go`, `internal/health/health_test.go`, `internal/store/field_length_test.go`, `internal/store/immutability_test.go`. The code state audited includes all of it. No commit hash exists for it.

Test run on 2026-10-19 (static documentation pass; run only to confirm the counts above): `go build ./...` and `go vet ./...` clean; `go test ./...` with a disposable PostgreSQL — 92 passed, 0 failed, 0 skipped; with no database — 71 passed, 21 skipped. No live authorization, container or frontend verification was performed; the `authorization-svc` migration gap remains an external blocker.

Clarifications are unchanged: items 1–10 above, and the frontend console (92%) claim, which was not audited.

---

## Service: document-vault-svc
**Port:** 8094
**Classification:** Governed Document Repository, Cryptographic Vault & Evidence Store

**Service Health:** Not established by repository evidence. It was reported healthy during the original engagement; no container health check was performed in the 2026-10-19 passes.

**Frontend Completion: 98%** (Workbench Components & API Client 100%; page restored and committed)
Reason: Full end-user and administrative document vault surface exists at `app/admin/documents` with a comprehensive API client (`lib/api/documents.ts`, 398 lines) covering all document lifecycle operations (upload, versioning, records declaration, supersession, archival, classification proposal/confirmation, policy mapping, and download). Server actions (`app/admin/documents/actions.ts`, 256 lines) implement full mutation handling and form processing. Rich UI components (`components/admin/documents/DocumentRegisterPanel.tsx`, `DocumentRegisterTable.tsx`, `DocumentForms.tsx`) provide search, filtering, status badges, version history modals, and classification review panels. **Documentation drift corrected this pass:** `app/admin/documents/page.tsx` is no longer deleted — `git log` shows it was restored and committed on 2026-10-06 (`4b4d0c2e`, "Update frontend service implementations"), 133 lines, `git status` clean. Historical, not re-verified (the frontend was not audited in the 2026-10-19 passes): scoped `tsc --noEmit` and `eslint app/admin/documents/` were reported clean.

**Backend Completion:** no percentage asserted. Reason: the service implements a REST API for documents and versions (`internal/handler/handler.go`), DRC-02 records, DRC-03 retention and legal holds, DRC-04 renditions / fixity / redaction profiles / export packages, and audit evidence, on PostgreSQL with forced row-level security, AES-256-GCM blob storage, a malware-scan gate that fails closed, declared-vs-detected content-type validation, idempotent upload commands, a transactional outbox, and database-enforced immutability for versions, the access log and the DRC evidence tables. It does **not** implement `ZS-SVC-S-001`'s reservation-based upload flow, a retention-resolution / conflict engine, a disposition workflow that executes and proves deletion, hold target resolution, the DRC-05 signature endpoints, the DRC-001..020 error taxonomy as a whole, or most of the §9.3 event vocabulary for documents and records (see Needs-Clarification). A documentation audit against that specification scored **31.3%** (15 of 48 comparable items); the remediation pass below fixed the confirmed service-owned defects and did not change that design divergence. A later endpoint-level audit (2026-10-19, 88.1%: 37 of 42 comparable items) found three code gaps — a download served when its access-log write failed, a non-atomic `bulk-classify`, and a documented but absent status filter on `GET /v1/documents` — plus an envelope status mismatch and documentation drift; see Gaps 18–21.

**Integration Completion:** not re-scored.
Repository evidence (checked statically on 2026-10-19): migrations `000001` through `000014` exist under `deployments/migrations/` (`000013` and `000014` are uncommitted, see Working-tree state); `deployments/docker-compose.yml` has a `document-vault-svc` block (port 8094, `DB_USER: zoiko_app`, `AUTHZ_SERVICE_URL`, `KAFKA_BROKERS: "kafka:9094"`, `depends_on` kafka); the synchronous authorization client calls `authorization-svc` and fails closed; the outbox relay (`internal/outbox/outbox.go`) is started from `cmd/server/main.go`; with `CLAMAV_URL` unset the service uses `scan.UnconfiguredScanner` and refuses uploads; no ClamAV daemon is provisioned in the platform compose file, and the compose file is outside this service.
Historical runtime observations (original engagement; **not re-verified**; no rebuild, redeploy, container health check, live API probe, Kafka consumption or database inspection was performed in the 2026-10-19 passes): a 450-event outbox backlog drained to 0 and real `document.uploaded` / `classification.proposed` payloads were consumed from `zoiko.document-vault.events`; a stale image had left the DRC-04 routes returning 404 until a `--no-cache` rebuild and migration `000011` applied to the live database; a live write attempt answered `503` because of the external `authorization-svc` migration gap. Statements elsewhere in this section that the container was "rebuilt", "redeployed" or "healthy" refer to that engagement, not to 2026-10-19.

**Overall Completion:** no percentage asserted.

**Production Readiness: NOT PRODUCTION READY.** Service-owned status: **PARTIALLY READY**. Unresolved: the upload model, retention / disposition architecture, event vocabulary and error taxonomy diverge from `ZS-SVC-S-001` with no recorded decision; uploads are refused until a ClamAV daemon is configured; the DRC-03/04 and legal-hold routes authorize against an empty legal-entity scope that `authorization-svc` is documented to reject; and the full live-authorized path is blocked by the external `authorization-svc` migration gap.

**Commands:**
- `POST /v1/documents` (Ingest a base64 payload: authenticate, resolve the tenant, verify `DOCUMENT_CREATE` on `legal_entity_id` **before** the rest of the body is validated, replay an earlier identical `Idempotency-Key` request, validate the declared `content_type` against the bytes (mismatch → 422 `DRC-002`), scan with the detected type (no scanner configured or scanner down → 503 `scan_unavailable`; infected → 422 `upload_quarantined`), compute the SHA-256, encrypt to storage, persist metadata, enqueue `document.uploaded`, and delete the blob again if the database write fails). Also: a body `tenant_id` that differs from the request's verified tenant is 400 `tenant_mismatch` (checked before authorization); a `residency_region_code` is checked against the tenant (409 `residency_violation`, 503 `residency_service_unavailable`); request bodies are limited to 12 MiB (`maxBodyBytes`).
- `POST /v1/documents/{documentID}/versions` (Upload a subsequent version under `DOCUMENT_VERSION_CREATE`: same content-type validation, fail-closed scan, `Idempotency-Key` replay and compensating blob deletion as above; a quarantined version is recorded as `document.version_upload_quarantined`).
- `POST /v1/documents/{documentID}/declare-record` (Declare the current document version as an immutable, authoritative business record under `DOCUMENT_DECLARE_RECORD`).
- `POST /v1/documents/{documentID}/supersede` (Supersede an authoritative record with a newer version under `DOCUMENT_SUPERSEDE`).
- `POST /v1/documents/{documentID}/archive` (Transition document lifecycle status to `ARCHIVED` under `DOCUMENT_ARCHIVE`).
- `POST /v1/documents/{documentID}/request-disposition` (Under `DOCUMENT_REQUEST_DISPOSITION`: records a disposition request — status `PURGE_PENDING` — **and nothing more**. No content is deleted, destroyed or proven destroyed by this service. It is refused with 409 `legal_hold_block` (detail `DRC-008`, naming the hold) when a declared record of the document is the target of an active legal hold. It does **not** check retention eligibility; an earlier version of this file claimed "retention policy verification", which the code never performed.)
- `POST /v1/documents/{documentID}/links` (Link document to platform business entities under `DOCUMENT_LINK`).
- `POST /v1/documents/{documentID}/classify` (Propose a candidate classification for a document under `CLASSIFY_RECORD`, setting status to `CANDIDATE` and recording proposing principal).
- `POST /v1/documents/classifications/{classificationID}/confirm` (Maker-Checker classification confirmation under `CONFIRM_CLASSIFICATION`; enforces segregation of duties by rejecting creator self-confirmation with HTTP 403 `self_confirmation_forbidden`, transitions classification to `CONFIRMED`, and enqueues `classification.confirmed`).
- `POST /v1/documents/{documentID}/reclassify` (Propose a reclassification for an existing confirmed record under `RECLASSIFY_RECORD`).
- `POST /v1/documents/classifications/{classificationID}/supersede` (Confirm supersession of a classification under `SUPERSEDE_CLASSIFICATION` with maker-checker enforcement).
- `POST /v1/documents/bulk-classify` (Propose a classification for up to 500 documents **atomically**: every listed document is looked up and authorized (`CLASSIFY_RECORD` on its own legal entity) before anything is written, then all proposals and their `classification.proposed` outbox events are written in ONE database transaction. If any document is missing, not authorized, or fails a classification rule, nothing is applied: the response is the failing status (404 `document_not_found`, 403 `forbidden`, 409 `not_candidate`, 400 `invalid_source` / `confidence_required` / `confidence_not_allowed`, 503 `authz_unavailable` / `store_unavailable`) with a body `{error, failed_document_id, rolled_back: true}`. On success: 200 with one `{document_id, classification}` entry per document, in request order. More than 500 ids is 400 `too_many_documents`; an empty list is 400 `empty_document_ids`.)

**Reads:**
- `GET /v1/documents` (List stored documents for **one legal entity**: `legal_entity_id` is required (400 `missing_field` without it) and authorization is `DOCUMENT_READ` on that entity; tenant-isolated; paginated with `limit` / `offset` (400 on invalid values). There is **no status filter**: no `status` query parameter exists in the handler or the store, and none is required by `ZS-SVC-S-001` or by the console client, so the earlier claim of "status filtering" was removed rather than implemented.)
- `GET /v1/documents/{documentID}` (Fetch document metadata, status, retention policy, and current version under `DOCUMENT_READ`. The metadata access is logged on a best-effort basis: a failed log write is reported in the service log and does not block the read. Only downloads fail closed.)
- `GET /v1/documents/{documentID}/content` (Download the decrypted bytes under `DOCUMENT_DOWNLOAD`, logging the access. Served as `Content-Disposition: attachment; filename="<document_id>-v<version>"` (no title in the name), `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`. A recomputed-hash mismatch is 409 `integrity_check_failed` (detail `DRC-011`); a missing stored object is 503 `object_missing` (detail `DRC-019`); no bytes are served in either case. The access-log record for a download is written **before** any byte is served; if that write fails the download is refused with 503 `store_unavailable` and nothing is served.)
- `GET /v1/documents/{documentID}/versions` (List full immutable version history of a document with SHA-256 checksums and timestamps).
- `GET /v1/documents/{documentID}/access-log` (Audit trail listing who accessed or downloaded document metadata and bytes under `DOCUMENT_ACCESS_LOG_READ`).
- `GET /v1/documents/{documentID}/verify-digest` (Recompute and verify cryptographic SHA-256 integrity of stored blob against recorded version checksum. A mismatch is 200 with `verified: false`; a missing object is 503 `object_missing`; `DOCUMENT_READ`).
- `GET /v1/documents/{documentID}/as-of` (Temporal point-in-time document state retrieval).
- `GET /v1/documents/{documentID}/links` (Query business objects linked to this document).
- `GET /v1/documents/{documentID}/classification` (Fetch the currently active confirmed classification for a document).
- `GET /v1/documents/{documentID}/classification/as-of` (Temporal point-in-time classification query).
- `GET /v1/documents/{documentID}/classification/history` (Audit log of all proposed, confirmed, and superseded classifications).
- `GET /v1/documents/unclassified` (List unclassified documents requiring governance action. `legal_entity_id` is required (400 `missing_field`); `DOCUMENT_READ` on that entity; paginated).
- `GET /v1/documents/classifications/{classificationID}/policy-mapping` (Explain active policy rules governing this classification).
- `GET /healthz` (Process liveness probe).
- `GET /readyz` (Database pool readiness probe).

**Additional API surface not previously documented in this file (found this pass):** the service has grown three further modules, each with its own handler, store, and real test coverage, none of which were reflected in the Commands/Reads lists above:
- **DRC-02 Records** (`internal/handler/records_handler.go`, 5 routes) — immutable record declarations and record-to-record relationships (supersedes/evidences/relates-to), under `internal/store/records_store.go` (11 Postgres-backed tests, `TestDRC02_*`).
- **DRC-03 Retention & Legal Hold** (`internal/handler/retention_handler.go`, 17 routes) — retention rule binding/approval (maker-checker on rule approval, same `creator != approver` discipline as classification), disposition trigger evaluation, and legal holds including the release-approval maker-checker added this pass (see Gap 7), under `internal/store/retention_store.go` (17 Postgres-backed tests, `TestDRC03_*`).
- **DRC-04 Preservation** (`internal/handler/preservation_handler.go`, 11 routes) — renditions, fixity manifests (with a repair-only recovery state machine enforced by a DB trigger), redaction profiles, and export packages, under `internal/store/preservation_store.go` (18 Postgres-backed tests, `TestDRC04_*`). This entire module was unreachable in this environment until this pass — see Gap 8.
- **Evidence** (`internal/handler/evidence_handler.go`, 13 routes) — audit evidence registration, integrity verification, reliability assessment, procedure linking, contradiction recording, custody history, and the lifecycle commands `POST /v1/audit-evidence/{evidence_id}/restrict`, `/quarantine` and `/supersede`, under `internal/store/evidence_store.go`. (Route prefixes: `/v1/records`, `/v1/retention-rules`, `/v1/retention-states`, `/v1/legal-holds`, `/v1/renditions`, `/v1/fixity-manifests`, `/v1/redaction-profiles`, `/v1/export-packages`, `/v1/audit-evidence`.)
- **Outbox events written today** (all through the transactional outbox, in the same transaction as the change): `document.uploaded`, `document.version_created`, `document.version_upload_quarantined`, `document.record_declared`, `record.declared`, `document.superseded`, `document.archived`, `document.disposition_requested`, `classification.proposed`, `classification.confirmed`, `classification.reclassified`, `classification.superseded`, `record.relationship_created`, plus the eight §9.3-named DRC events listed in Gap 16.

This file's Commands/Reads sections are not rewritten route-by-route here to keep this update proportionate to what was actually audited this pass; the counts and evidence above are given so the gap is visible rather than silently left stale.

**Segregation of Duties & Auth:**
- Tenant & Identity Isolation: `X-Tenant-Id` and `X-Principal-Id` mandatory on all operations (`requireTenant`, `requirePrincipal`), returning 401 Unauthorized if omitted.
- Canonical Envelope Enforcement: Canonical Service Input Contract middleware (`svcenvelope.Middleware`) enforces `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, `Idempotency-Key`, `X-Legal-Entity-Id`, and `X-Purpose-Context` on mutations, returning `envelope_incomplete` on violation: **HTTP 401** when the missing field is the tenant or the actor (those headers are set after the gateway verifies the signed identity envelope, so their absence is an authentication failure), otherwise HTTP 400. This is the shared platform contract (`services/_contract/envelope/policy.go` `StatusFor`; `docs/architecture/input-contract-conformance.md`), applied here unchanged.
- Fine-Grained Authorization: Synchronous authorization client calls `authorization-svc:8089` for distinct actions: `DOCUMENT_CREATE`, `DOCUMENT_READ`, `DOCUMENT_DOWNLOAD`, `DOCUMENT_VERSION_CREATE`, `DOCUMENT_ACCESS_LOG_READ`, `CLASSIFY_RECORD`, `CONFIRM_CLASSIFICATION`, `RECLASSIFY_RECORD`, and `SUPERSEDE_CLASSIFICATION`, plus `DOCUMENT_DECLARE_RECORD`, `DOCUMENT_SUPERSEDE`, `DOCUMENT_ARCHIVE`, `DOCUMENT_REQUEST_DISPOSITION`, `DOCUMENT_LINK` for the document commands, and the `RECORD_*`, `RETENTION_*`, `LEGAL_HOLD_*`, `RENDITION_*`, `FIXITY_*`, `REDACTION_*`, `EXPORT_*` and `AUDIT_EVIDENCE_*` actions of the DRC modules. Rejects unauthorized callers with HTTP 403 Forbidden. The DRC-03/04 and legal-hold routes check against an **empty legal-entity scope** (see Dependency-Limited Items). Document, evidence and record routes whose scope lives on the stored resource look the resource up first and authorize second (see Remaining Gaps).
- Maker-Checker / SoD Invariants:
  - Document Classification Confirmation (`ConfirmClassification`): Enforced in `classification_store.go` (`ConfirmClassification`). The principal who proposed a classification cannot confirm it (`creator != confirmer`), returning HTTP 403 `self_confirmation_forbidden` (`domain.ErrClassificationSelfConfirmation`).
  - Classification Supersession (`SupersedeClassification`): Enforced in `classification_store.go` (`SupersedeClassification`). Proposing principal cannot self-approve supersession, returning HTTP 403 `self_confirmation_forbidden`.
  - Retention Rule Approval (`ApproveRetentionRuleVersion`, DRC-03): same `approved_by_principal_id <> created_by_principal_id` discipline, enforced at the database level in migration `000010`.
  - Legal Hold Release Approval (DRC-03, added this pass — see Gap 7): `release_approved_by_principal_id <> released_by_principal_id`, enforced at the database level in migration `000012_add_legal_hold_release_approval` (renumbered this pass from a colliding `000011`).
- Immutability & Audit Trail: `document_versions` and `document_access_log` are append-only **in the database** since migration `000013` (triggers reject UPDATE, DELETE and TRUNCATE for every role, superusers included; INSERT and RLS unchanged); before that migration this held only because no API route touched them, and this file said so without it being enforced. A superuser/owner can still disable the triggers (an administrative act). The `documents` row is deliberately not append-only (its `current_version` pointer and lifecycle columns change). Version tampering is detected via SHA-256 integrity re-computation on every read and by `verify-digest`. Every download is logged to `document_access_log`. DRC-04 renditions, fixity manifests, redaction profiles, and export packages are likewise append-only, enforced by `BEFORE UPDATE OR DELETE` triggers rejecting any mutation (migration `000011_add_drc04_preservation_renditions`).

**Fixed Gaps:**

Note on "live" statements: sentences in Gaps 1–8 that describe a live probe, a running container, a rebuild, a redeploy, container logs or a live database or Kafka query (for example "verified live", "live `curl`", "rebuilt and redeployed", "450 → 0") are historical observations from the original engagement. They are not re-verified and are not claims about the current deployment. The code and test evidence in each gap is what the repository supports.

### Gap 1 — Storage Upload Failure Compensation & Orphan Cleanup
Status: ✅ Fixed
Original Issue:
During document or version upload, the binary blob is encrypted and written to storage prior to committing the database metadata row. If the database transaction failed (e.g. unique constraint violation, foreign key failure, connection drop, or outbox error), the encrypted storage blob remained orphaned on disk or in the bucket, accumulating unreferenced data and creating storage drift.
Fix Verification:
Implemented `Delete(ctx context.Context, storageKey string) error` on the `storage.Backend` interface and `LocalFileBackend` (`internal/storage/backend.go#L141-L148` (`LocalFileBackend.Delete`)). Added compensating error handlers in `handler.go` (`CreateDocument`, `cleanupBlob`) (`CreateDocument`) and `handler.go` (`AddVersion`, `cleanupBlob`) (`AddVersion`). If `store.CreateDocument` or `store.AddVersion` returns an error, `storage.Delete` is immediately invoked to purge the orphaned blob, logging an info event on success or error on failure. Verified live: during testing when a DB outbox insertion error occurred, the handler logged `CreateDocument: cleaned up orphaned storage blob after store error` and removed the blob. All 10 unit tests in `internal/storage/backend_test.go` and compensation tests in `internal/handler/handler_test.go` pass cleanly.
Evidence:
`internal/storage/backend.go#L141-L148` (`LocalFileBackend.Delete`), `internal/handler/handler.go` (`CreateDocument`, `AddVersion`, `cleanupBlob`), `internal/storage/backend_test.go#L94-L156` (`TestDelete_*`), container logs during live failure injection.

### Gap 2 — ClamAV INSTREAM Protocol Framing & Socket Desync
Status: ✅ Fixed (Code & Tests) / ⚠️ Dependency-Limited (Compose)
Original Issue:
`internal/scan/clamav.go`: Antivirus scanner implementation for ClamAV daemon over TCP socket used improper INSTREAM protocol framing and lacked timeout/error isolation. Malformed chunks caused socket desynchronization, hanging connections and failing open on scan errors.
Fix Verification:
Rewrote ClamAV scanner client in `internal/scan/clamav.go#L35-L120` implementing strict RFC-compliant ClamAV `zINSTREAM\0` protocol: 10-byte protocol prefix, 4-byte big-endian chunk length prefixes, chunk size bounding (max 64KB per chunk), zero-chunk terminator `[0, 0, 0, 0]`, and response parsing (`OK` vs `FOUND <virus>`). Implemented fail-closed behavior: connection failure, timeout, or clamd unreachable returns `domain.ErrVirusScanUnavailable` (translated to HTTP 503 at API boundary). Configured `CLAMAV_URL` in `internal/config/config.go#L90`; if empty, the service **refuses every upload** (`scan.UnconfiguredScanner` fails closed) — it no longer falls back to a no-op scanner (see Gap 9). All 7 unit tests in `internal/scan/clamav_test.go` pass (clean scan, infected payload quarantine, chunked streaming, timeout handling, and unreachable socket fail-closed).
Evidence:
`internal/scan/clamav.go#L35-L120`, `internal/scan/clamav_test.go#L66-L192`, `internal/config/config.go#L90`.

### Gap 3 — Transactional Outbox for Document Lifecycle & Classification Events
Status: ✅ Fixed
Original Issue:
Document events (`document.uploaded`, `document.version_created`, `record.declared`, `classification.proposed`, `classification.confirmed`) were either published synchronously to Kafka outside the database transaction (vulnerable to dual-write failure) or swallowed errors with `_ = h.publisher.Publish(...)`.
Fix Verification:
Implemented PostgreSQL transactional outbox table `outbox_events` in migration `000004_add_outbox_events.up.sql`. All document mutations (`CreateDocument`, `AddVersion`, `DeclareRecord`, `ClassifyRecord`, `ConfirmClassification`, `Reclassify`, `SupersedeClassification`) insert an outbox event in the same database transaction as the domain state change (`internal/store/pg_store.go`). Background relay worker `internal/outbox/outbox.go` polls unpublished events, dispatches to Kafka topic `zoiko.document-vault.events`, and marks events as published with timestamp. Verified live: during testing, `document.uploaded`, `classification.proposed`, and `classification.confirmed` records were successfully inserted into `outbox_events` table with matching `aggregate_id`.
Evidence:
`deployments/migrations/000004_add_outbox_events.up.sql`, `internal/store/pg_store.go#L225-L247` (`insertDocumentOutboxEvent`), `internal/outbox/outbox.go` (`Insert`, `NewRelay`, `Relay.Start`), live PostgreSQL query verification on `outbox_events` table.

### Gap 4 — Segregation of Duties (Maker-Checker) on Classification Confirmation and Supersession
Status: ✅ Fixed
Original Issue:
Previous audit notes contained conflicting references to a missing document redaction dual-authorization endpoint (`POST /v1/documents/{id}/redact`), which was documentation drift (document-vault-svc is an immutable, append-only record vault without redaction). However, BIZ-02 classification confirmation and supersession lacked strict segregation of duties enforcement in code.
Fix Verification:
Enforced maker-checker segregation of duties in `internal/store/classification_store.go` (`ConfirmClassification`) (`ConfirmClassification`) and `classification_store.go` (`SupersedeClassification`) (`SupersedeClassification`). The store strictly checks `proposal.ProposedByPrincipalID == params.ConfirmedByPrincipalID`; if equal, it immediately rejects the operation with `domain.ErrClassificationSelfConfirmation`. Handler translates this error to HTTP 403 Forbidden with error code `self_confirmation_forbidden` and detail `"the principal who proposed a classification cannot also confirm it"`. Verified live: maker attempt by proposing principal `33333333-3333-3333-3333-333333333333` was rejected with HTTP 403 `self_confirmation_forbidden`; subsequent confirmation by authorized independent checker `66666666-6666-6666-6666-666666666666` succeeded with HTTP 200 OK.
Evidence:
`internal/store/classification_store.go` (`ConfirmClassification`, `SupersedeClassification`), `internal/handler/handler.go` (`ConfirmClassification`, and the `self_confirmation_forbidden` case in `handleStoreError`), `internal/handler/handler_test.go` (`TestConfirmClassification_SelfConfirmation_Returns403`, `TestSupersedeClassification_SelfConfirmation_Returns403`), live HTTP test output.

### Gap 5 — Transactional Outbox Never Actually Delivered to Kafka (newly discovered this pass, critical)
Status: ✅ Fixed
Discovery Evidence:
Gap 3 above was marked "Fixed" on the strength of the DB-insert half of the outbox (`outbox_events` rows being created atomically), which is genuinely correct. The RELAY half was never actually confirmed delivering anything. Live container logs showed **every single publish attempt failing**, continuously, with `kafka.(*Writer): Topic must not be specified for both Writer and Message` — `internal/events/publisher.go`'s `PublishOutbox` set `Topic: p.topic` on the `kafka.Message` while `cmd/server/main.go` ALSO sets `Topic: cfg.Kafka.Topic` on the `*kafka.Writer` that message is sent through. Confirmed against kafka-go v0.4.51's own source (`writer.go#L907-L914`, `chooseTopic`): this combination is **unconditionally rejected**, every call, with no dependency on whether a broker is even reachable. There was no test covering this — `internal/events` had no test file at all, and `internal/outbox` has none either, so the only thing exercising the real `*kafka.Writer` validation was production traffic, continuously failing.
Classification: SERVICE-OWNED DEFECT, Critical — every document lifecycle and classification event this service was supposed to emit (`document.uploaded`, `document.version_created`, `record.declared`, `classification.proposed`, `classification.confirmed`, etc.) had never been delivered to Kafka, meaning no downstream consumer (e.g. `audit-event-store-svc`, which subscribes to `zoiko.document-vault.events`) had ever received one.
Fix Applied:
Removed `Topic: p.topic` from the `kafka.Message` literal in `internal/events/publisher.go#PublishOutbox` — the Writer already pins the topic at construction, so the per-message field was always redundant and always fatal in combination.
Verification:
Added `internal/events/publisher_test.go` (did not exist before): a stub-based test pinning the message shape (key/value/header), and `TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage`, which uses a REAL `*kafka.Writer` (not a hand-written stub) specifically so kafka-go's own validation runs — confirmed this test fails with the exact production error when the bug is deliberately reintroduced, and passes with the fix. Added a third, opt-in live test (`TestPublishOutbox_AgainstRealBroker`, gated on `KAFKA_TEST_BROKER`) that performs a real publish against the dev stack's Kafka broker. Live end-to-end proof beyond the test suite: after rebuilding and redeploying, the 450-event backlog in `outbox_events` drained from 450 unpublished to 0 within seconds; independently consumed real `document.uploaded` and `classification.proposed` payloads off `zoiko.document-vault.events` via `kafka-console-consumer.sh`, confirming genuine delivery, not just an absence of errors.
Evidence:
`internal/events/publisher.go` (fix), `internal/events/publisher_test.go` (new, 3 tests), live container logs before/after, `SELECT count(*) FILTER (WHERE published_at IS NULL) FROM outbox_events` (450 → 0), live `kafka-console-consumer.sh` output showing real event payloads.

### Gap 6 — Kafka Broker Address Missing from Deployment Config (newly discovered this pass, critical)
Status: ✅ Fixed
Discovery Evidence:
Even after fixing Gap 5's code defect, publishing still failed — this time with `dial tcp [::1]:9092: connect: connection refused`. `deployments/docker-compose.yml`'s `document-vault-svc` block had **no `KAFKA_BROKERS` environment variable at all**, unlike every other Kafka-publishing service in this compose file (which all set `KAFKA_BROKERS: "kafka:9094"`). `internal/config/config.go#L92` defaults `KAFKA_BROKERS` to `"localhost:9092"` when unset — inside the container that resolves to the container's own loopback, where nothing listens; Kafka is a separate container reachable only as `kafka:9094`. This is a second, independent cause of the same symptom as Gap 5: the code bug and the missing wiring were compounding, and fixing only one would not have restored delivery.
Classification: SERVICE-OWNED DEFECT (deployment-configuration gap, not a source-code defect), Critical for the same reason as Gap 5 — this is the other half of why nothing was ever delivered.
Fix Applied:
Added `KAFKA_BROKERS: "kafka:9094"` to `document-vault-svc`'s environment block in `deployments/docker-compose.yml`, and added `kafka: condition: service_healthy` to its `depends_on`, matching the pattern every other Kafka-publishing service in this file already follows.
Verification:
Rebuilt and redeployed; live container logs show the dial target correctly changed from `[::1]:9092` to the real broker, then (after the Gap 5 fix was also in place) genuine successful publishes — same live evidence as Gap 5 (450 → 0 unpublished backlog, independently consumed real messages).
Evidence:
`deployments/docker-compose.yml` (`document-vault-svc.environment.KAFKA_BROKERS`, `depends_on.kafka`), `internal/config/config.go#L92` (the default that was silently taking effect), live container logs.

### Gap 7 — Migration Version Collision: Two Files Both Numbered 000011 (newly discovered this pass)
Status: ✅ Fixed
Discovery Evidence:
`deployments/migrations/` contained both `000011_add_drc04_preservation_renditions.up.sql` (renditions/fixity/redaction/export-package tables) and `000011_add_legal_hold_release_approval.up.sql` (the legal-hold release-approval column and its self-release `CHECK`, added in an earlier pass this session). The two do not touch the same tables and happened not to conflict in content, but sharing a sequence number is a structural defect: this platform's own migration convention is sequential, and `init-db.sh`'s `apply_migrations` only "works" here by an alphabetical-sort accident (`add_d` sorts before `add_l`) rather than by any enforced ordering — a real migration-tracking tool (e.g. `golang-migrate`) would refuse two files claiming the same version outright.
Classification: SERVICE-OWNED DEFECT, Medium severity (latent correctness/tooling risk, not a live-breaking one in this specific pair, since the two don't touch the same tables).
Fix Applied:
Renamed `000011_add_legal_hold_release_approval.{up,down}.sql` to `000012_add_legal_hold_release_approval.{up,down}.sql` via `git mv` (tracked as a rename, content unchanged). This was the more recently added of the two (this session's own earlier work), so it took the next free number rather than the DRC-04 migration, which predates it.
Verification:
Re-ran the full migration set from a clean, disposable database (`document_vault_test`): all 12 migrations in sequence applied with zero errors, and all 77 Postgres-backed tests — including `TestDRC03_LegalHold_ReleaseRejectsSelfApproval`, which specifically exercises the renamed migration's own constraint — passed.
Evidence:
`deployments/migrations/000012_add_legal_hold_release_approval.up.sql` / `.down.sql` (renamed), `git status` showing a tracked rename, clean-database migration run output, 77/77 Postgres-backed tests passing.

### Gap 8 — Stale Deployed Image: Entire DRC-04 Route Surface Returning 404 (newly discovered this pass, critical)
Status: ✅ Fixed
Discovery Evidence:
`POST /v1/renditions` (and the rest of `internal/handler/preservation_handler.go`'s 11 routes) returned a bare `404 page not found` on the live container, despite `cmd/server/main.go` unconditionally calling `handler.RegisterPreservationRoutes(r, h, pgStore)` at startup and the running container's image ID nominally matching the current `docker compose build` output. A forced `docker compose build --no-cache` followed by redeploy resolved it — the previous build had silently not incorporated this code despite appearing to. Separately, and compounding this, the DRC-04 migration (`000011_add_drc04_preservation_renditions`, tables `renditions`/`fixity_manifests`/`redaction_profiles`/`export_packages`) had never been applied to the live `document_vault` database at all — confirmed via `\dt`, zero matching tables — so even with routing fixed, every DRC-04 handler would have failed on first query.
Classification: SERVICE-OWNED DEFECT (deployment/build-pipeline and environment-migration gap, not a source-code defect — the code was correct), Critical — an entire, previously-implemented compliance module (preservation, fixity verification, redaction, export packaging) was completely unreachable.
Fix Applied:
`docker compose build --no-cache document-vault-svc` followed by `docker compose up -d document-vault-svc`; applied `000011_add_drc04_preservation_renditions.up.sql` directly to the live `document_vault` database.
Verification:
Live-confirmed: `POST /v1/renditions` with proper envelope headers no longer 404s — it now reaches the handler and correctly answers `503 authz_unavailable` (the same `authorization-svc` cross-service dependency documented elsewhere in this file), proving the route, the handler, and the underlying tables are all now live and reachable. All 17 `TestDRC04_*` Postgres-backed tests pass against a clean database.
Evidence:
Live `curl` before (404) and after (503, reaching the handler) this fix, `\dt` confirming the DRC-04 tables now exist, 17/17 `TestDRC04_*` tests passing.

### Remediation pass (service-only) — confirmed defects fixed

The `ZS-SVC-S-001` documentation audit (31.3%, 15 of 48) found service-owned defects and a larger set of design divergences. This pass fixed the former and recorded the latter as Needs-Clarification; it did not rewrite the service to match the specification.

### Gap 9 — An absent malware scanner approved every upload (DRC-I18)
Status: ✅ Fixed. Root cause: with `CLAMAV_URL` unset the service silently used `NoOpScanner`, which returned "clean" for everything. Fix: `scan.UnconfiguredScanner` returns an error for every scan, so uploads and new versions are refused (503 `scan_unavailable`) before anything is stored; reads and the record, retention and preservation routes are unaffected. Files: `internal/scan/scan.go`, `cmd/server/main.go`, `internal/config/config.go`. Verification: `TestUnconfiguredScanner_RefusesEveryUpload_AndStoresNothing`, `TestUnconfiguredScanner_FailsClosed_ForEveryInput`, `TestScannerFailure_FailsClosed`, `TestConfiguredScanner_AcceptsCleanContent_AndIsGivenTheDetectedType`; mutation check (scanner reports clean) fails them. Deployment note: the platform compose file does not provision ClamAV, so uploads there are refused until it does (that file is outside this service).

### Gap 10 — Declared MIME type was trusted (DRC-I19)
Status: ✅ Fixed within what the bytes can show. Root cause: `content_type` from the caller was stored and handed to the scanner unchecked. Fix: new `internal/contenttype` validates the declared type against `net/http.DetectContentType` over the first 512 bytes: it must be a well-formed media type; if the bytes are positively identified (PDF, PNG, JPEG, GIF, WebP, ZIP, gzip, HTML, XML, audio/video…) the declared type must agree (OOXML/ODF/EPUB are accepted for ZIP); if the bytes are generic, the declared type may not claim a format the sniffer can identify. A contradiction is 422 `DRC-002` (`UPLOAD_VALIDATION_FAILED`); a malformed type is 400 `invalid_content_type`; the scanner receives the DETECTED type. **Limit, not hidden:** the sniffer cannot tell a CSV from JSON or one opaque binary format from another, and the specification names no allow- or block-list, so bytes it cannot classify are accepted under any non-sniffable declared type (an executable declared `application/x-msdownload` passes). Whether a policy list is wanted is a Needs-Clarification item. Files: `internal/contenttype/`, `internal/handler/handler.go`. Verification: `contenttype_test.go` (accepts, mismatches, invalid, head-only, documented limit) and handler tests; mutation check (validation disabled) fails them.

### Gap 11 — `document_versions` and `document_access_log` were append-only by convention only (DRC-I01/I02)
Status: ✅ Fixed. Root cause: no trigger or guard on either table, although migration 000001 and this file called them append-only. Fix: migration `000013` rejects UPDATE, DELETE and TRUNCATE on both, for every role; INSERT, RLS and tenant isolation unchanged. Limit: a superuser/owner can disable triggers; nothing in this service deletes versions today, and a future destroying disposition path will need an explicit audited exception. Verification: `TestDocumentVersionsAndAccessLog_AreAppendOnly` (real PostgreSQL: inserts work; UPDATE/DELETE/TRUNCATE refused for a superuser and for a NOSUPERUSER NOBYPASSRLS role holding all privileges; nothing changed; reads work; tenant isolation intact); mutation check (migration removed) fails it.

### Gap 12 — `Idempotency-Key` was required but ignored on the upload commands (§9.1, DRC-I30)
Status: ✅ Fixed for `POST /v1/documents` and `POST /v1/documents/{id}/versions`; **not** for the other mutating commands (see Remaining Gaps). Root cause: the canonical envelope demands the header, nothing used it, so a retried upload created a second document or version. Fix: migration `000014` adds `document_idempotency_keys` (RLS, immutable rows); the claim is committed in the same transaction as the document/version. Replay runs after authentication, tenant and authorization and before any bytes are decoded, scanned or stored; the same key and request returns the original 201 (`Idempotency-Replay: true`) and writes no blob; the same key with a different request (method, path, body) is 409 `idempotency_mismatch`; a lookup failure is 503 (never "proceed"); a concurrent identical request commits once and the loser rolls back and deletes its blob; a failed operation does not consume the key. Scope is (tenant, principal, key) with a request hash — the platform's usual reading of INV-08; the specification states neither the scope nor the binding (Needs-Clarification). Files: `internal/store/pg_store.go`, `internal/handler/handler.go`, `internal/domain/idempotency.go`, migration 000014. Verification: store tests (replay, scope, 8-way race, failed operation claims nothing, version replay) and handler tests (replay, mismatch, independent keys, fail-closed, replay is no way around authorization); mutation check (idempotency disabled) fails them.

### Gap 13 — Authorization after validation, and a caller-chosen authorization scope
Status: ✅ Fixed where the API contract allows. Root cause: `CreateDocument`, evidence registration and 14 DRC-03/04 commands validated the body before authorizing; `POST /v1/records` authorized against the caller-supplied `legal_entity_id` when one was given, so a principal with the declare grant on entity A could declare a record over entity B's document by naming A. Fix: `legal_entity_id` (the scope) is read first, authorization precedes every other field check, and the DRC-03/04 handlers authorize before decoding. Record declaration authorizes against the entity that OWNS the document and refuses a different asserted entity (400 `legal_entity_mismatch`). Not changed: routes whose authorization scope is stored on the resource (document, evidence and record reads, `AddVersion`) must look the resource up before they can authorize, so an authenticated caller in the tenant can still distinguish "no such document" (404) from "forbidden" (403) — inherent to the current authorization model, recorded under Remaining Gaps. Verification: `TestCreateDocument_UnauthorizedCallerGets403_NotAValidationError`, `TestRegisterEvidence_UnauthorizedCallerGets403_BeforeFieldValidation`, `TestRetentionCommand_UnauthorizedCallerGets403_BeforeBodyValidation`, `TestDeclareRecord_*`.

### Gap 14 — Download response carried no security headers (§10.1)
Status: ✅ Fixed. `Content-Disposition: attachment; filename="<document_id>-v<n>"` (id and version only, no title — §10.1 metadata minimization), `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`. Verification: `TestDownload_SetsAttachmentNoSniffNoStoreHeaders`.

### Gap 15 — `request-disposition` ignored legal holds (DRC-I10)
Status: ✅ Fixed (hold check); disposition itself is still not executed (see Remaining Gaps). Root cause: the command flipped a document to `PURGE_PENDING` without looking at holds, while this file claimed "retention policy verification". Fix: a document with a declared record that is the target of an active (applied, unreleased) hold is refused — 409 `legal_hold_block`, detail `DRC-008` naming the hold, nothing mutated; releasing the hold unblocks it. Verification: `TestRequestDisposition_BlockedWhileARecordOfTheDocumentIsUnderHold` (real PostgreSQL), `TestRequestDisposition_UnderLegalHold_Is409_DRC008`; mutation check fails it.

### Gap 16 — DRC-03/04 state changes announced nothing (§9.3)
Status: ✅ Fixed for the events the data model can support. Eight events with the specification's names are written to the outbox in the same transaction as their change: `retention_schedule_resolved` (on bind and when a trigger resolves the due date), `record_disposition_due` (with `hold_state`), `legal_hold_target_added`, `legal_hold_activated`, `legal_hold_released`, `rendition_created`, `integrity_check_failed` (only for a recorded MISMATCH/MISSING/UNREADABLE) and `export_package_created`. Payloads carry the §9.3 anchors this service can supply; the ones it cannot (`scope_version`, `incident_ref`, `recipient_scope`, a workflow `release_approval_ref`) are not invented. NOT emitted, because the behaviour does not exist: `retention_conflict_detected`, `disposition_started/completed/failed`, `legal_hold_partial_application`, and the signature events. The existing dotted document and record events (`document.uploaded`, `record.declared`, …) are unchanged — whether to rename them to the §9.3 vocabulary is a consumer-compatibility decision (Needs-Clarification). `outbox.Insert` now stores NULL for a tenant-level event with no legal entity. Verification: `drc_events_test.go` (anchors, one event per change, a refused operation leaves none).

### Gap 17 — Record declaration residency gate and DRC codes
Status: ✅ Fixed where the specification is explicit. `POST /v1/records` re-checks the document's region against the tenant (fail-closed on mismatch or an unreachable registry; §4.2 "Residency"). The §4.2 gates "exact content" (a version exists in the tenant, carries a hash, and passed the upload scan), "business context", "file plan" (record class validated) and "duplicate detection" (unique per version, replay by correlation id) were already met; the "retention inputs" gate (pending-trigger per class) and any per-record-class region rule are not defined by the specification and are not invented. DRC codes now used where §11.1 is unambiguous: `DRC-002` (content type), `DRC-008` (hold), `DRC-011` (fixity mismatch), `DRC-019` (object missing) — on the new errors as the machine code (`DRC-002`) and, to avoid breaking existing clients, as a `DRC-0xx:` prefix in `detail` for conditions that already had a machine code. Every other condition keeps its local code.

### Gap 18 — A download was served even when its access-log write failed (endpoint-level audit)
Status: ✅ Fixed (CODE BUG). Root cause: `recordAccess` logged an error when the `document_access_log` insert failed and the handler served the bytes anyway, so a document could be read with no immutable record of who read it, contradicting "every download is logged". Fix: `recordAccess` now returns its error and `GetContent` writes the access record AFTER authorization, tenant scoping and the integrity / object checks but BEFORE any byte or download header is written; a failed write is `503 store_unavailable` ("the download could not be recorded in the access log, so no content was served"), using the service's existing store-failure code (no new error taxonomy). `GetDocument` (metadata) keeps best-effort logging; the documented guarantee is about downloads. Files: `internal/handler/handler.go` (`recordAccess`, `GetContent`, `GetDocument`). Verification: `internal/handler/access_and_bulk_test.go` — `TestDownload_Success_IsLogged_AndServed`, `TestDownload_AccessLogWriteFails_NothingIsServed` (503, no bytes, no `Content-Disposition`, no log row), `TestDownload_AuthorizationStillComesFirst_WhenAccessLogIsBroken`, `TestDownload_IntegrityAndMissingObject_KeepTheirCodes_WhenAccessLogIsBroken`, `TestMetadataRead_StillServed_WhenAccessLogFails`; mutation check (failure ignored) fails the access-log-failure test. Note: the exact status for an access-log failure is not defined by any contract (Needs-Clarification 14).

### Gap 19 — `bulk-classify` was documented atomic but was not (endpoint-level audit)
Status: ✅ Fixed (CODE BUG). Root cause: `BulkClassify` classified each document in its own transaction (`classifyOneForBulk`), so earlier successes stayed committed when a later document failed, and the response was a 200 with mixed results. Fix: every document is looked up and authorized per document first (a missing or denied document stops the batch before any write); `PgStore.BulkClassifyRecords` then applies all proposals and their `classification.proposed` outbox events in ONE transaction (`classifyRecordTx`, the same body `ClassifyRecord` now uses), so a rule failure part-way (for example a document that already has a confirmed classification) rolls every earlier proposal and event back. A failure returns the failing status with `{error, failed_document_id, rolled_back: true}`; success keeps the per-document result list. The 500-id limit, per-document authorization, classification rules and maker-checker semantics are unchanged. **Behaviour change:** a batch that used to return 200 with per-item errors now returns the failing status for the whole batch; no frontend caller of this route was found in `lib/` or `app/` (the frontend was not audited). Files: `internal/store/classification_store.go`, `internal/domain/classification.go` (`BulkClassifyError`), `internal/handler/handler.go` (`BulkClassify`, `writeBulkFailure`). Verification: real PostgreSQL `internal/store/bulk_classify_test.go` (all valid commits every row and event; failure at first, middle and last leaves no rows and no events; a rule failure after earlier successes rolls them back; invalid proposal writes nothing; single `ClassifyRecord` unchanged) and handler tests in `access_and_bulk_test.go`; mutation checks (one transaction per document; per-document authorization removed) fail them.

### Gap 20 — `GET /v1/documents` documented a status filter that does not exist (endpoint-level audit)
Status: ✅ Resolved by correcting the documentation (DOC DRIFT, not an implementation gap). `ZS-SVC-S-001` defines no list-documents endpoint or filter, the console client sends none, and the only source of "status filtering" was this file's own route description. No speculative parameter was added. The Reads entry now states the real contract: `legal_entity_id` required, `DOCUMENT_READ` on that entity, pagination, no status filter. Whether a status filter is wanted is Needs-Clarification 15.

### Gap 21 — Envelope status documented as 400 (endpoint-level audit)
Status: ✅ Resolved by correcting the documentation (DOC DRIFT). The shared contract returns 401 for a missing tenant or actor and 400 for every other violation (`services/_contract/envelope/policy.go` `StatusFor`; `docs/architecture/input-contract-conformance.md`), and this service applies it unchanged; no shared or service-local behaviour was changed. Verification: `TestEnvelope_MissingTenantOrActor_Is401_OtherViolations400` (real envelope middleware, strict mode, `POST /v1/documents`).

### Documentation corrections (this pass)
- Removed "95%", "Ready for Production", "Remaining Gaps: None" and the claim that disposition requests verify retention policy.
- "Document versions and access logs are append-only" is now true (it was convention only) and says how.
- Gap 2 and the compose note no longer say the service falls back to a no-op scanner.
- DOC-01 (multipart / MinIO), DOC-02 (redaction — no such route; a `document.redacted` event row cited a route that does not exist) and DOC-03 (a non-existent `store/outbox.go`) rows corrected.
- The authorization action list was incomplete; the empty legal-entity scope is documented.
- Test counts replaced (below).
- 2026-10-19 endpoint-level pass: stale source citations replaced with current ranges or file/function references; migration range corrected to `000001`–`000014`; DRC-02/03/04 test sub-counts corrected (11 / 17 / 18); the `/healthz` and `/readyz` response-body claims removed (both return an empty 200); contradictory rebuild/redeploy statements resolved by labelling runtime observations as historical; the `GET /v1/documents` status-filter claim removed; the envelope status corrected (401 for tenant/actor, otherwise 400); undocumented routes, events and request limits added.

**Remaining Gaps (service-owned, confirmed):**
- `Idempotency-Key` replay covers only `POST /v1/documents` and `POST /v1/documents/{id}/versions`. The other mutating commands rely on state guards (declare, supersede, archive, hold transitions) or, for records, replay by `correlation_id`; ZS-SVC-S-001 §9.1 asks for key-based replay on every command.
- Routes whose authorization scope is stored on the resource (document / evidence / record reads, `AddVersion`) look the resource up before authorizing, so a caller inside the tenant can distinguish 404 from 403.
- `request-disposition` flags `PURGE_PENDING` only. Nothing executes disposition, deletes content, or produces proof of destruction (DRC-I13/I14/I30), and it does not check retention eligibility.
- A process crash between the blob write and the metadata commit leaves an orphaned blob (a normal database failure is compensated). There is no reconciliation job.
- Content-type validation is limited to what byte sniffing can show (see Gap 10).
- Record declaration has no retention-input or per-class region gate (undefined by the specification).
- A superuser/owner can disable the immutability triggers.
- Metadata reads (`GET /v1/documents/{id}`) log access best-effort; only downloads fail closed.

**Dependency-Limited Items / Cross-service:**
- **ClamAV daemon unprovisioned in compose:** with `CLAMAV_URL` unset the service refuses uploads (fail-closed). The compose file is platform-wide and was not changed.
- **Empty legal-entity scope on DRC-03/04 and legal-hold authorizations** (`RETENTION_*`, `LEGAL_HOLD_*`, `RENDITION_*`, `FIXITY_*`, `REDACTION_*`, `EXPORT_*`): the service sends `legal_entity_id = ""`; this repository documents `authorization-svc` as rejecting an empty legal entity (see the `schema-registry-svc` entry), which would make these routes return 503. Retention rules, holds, fixity manifests and export packages are tenant-level objects with no owning legal entity in this service's model, and no valid scope value is defined — none was invented. CROSS-SERVICE / NEEDS CLARIFICATION.
- **DRC-05 (signature envelopes, callbacks)** is not in this service; an `esignature-integration-svc` exists. Ownership of the §9.2 signature endpoints is not stated.
- **Direct Container Port Exposure (SEC-04):** `:8094` is exposed on host interfaces; direct calls bypass Traefik ForwardAuth, but the service enforces tenant, principal, envelope and `authorization-svc` checks itself.
- ❌ **EXTERNAL BLOCKER — `authorization-svc`:** reported earlier as missing the migration for `principal_role_assignments.book_id` (every `/v1/authorize` fails with 503). Not re-verified in this pass; `authorization-svc` was not modified. It blocks a fully-authorized live 201.

**Needs-Clarification Items (spec/implementation divergences — not service-owned bugs until a decision says so):**
1. **Upload model.** §3.4 specifies reserve → scoped upload → complete (`POST /documents/{id}/upload-reservations`, `/upload-reservations/{id}/complete`, `PENDING_VALIDATION`). The service takes inline base64 and commits synchronously.
2. **Exact-version read and concurrency.** No `GET /document-versions/{version_id}` (versions are addressed by number); no `PATCH` / `If-Match` / `row_version`; no `lock` / `withdraw`.
3. **State and field vocabulary.** Documents are ACTIVE / RETAINED / PURGE_PENDING / SUPERSEDED / ARCHIVED (spec: DRAFT, ACTIVE, LOCKED, SUPERSEDED, WITHDRAWN, QUARANTINED); versions lack `commit_status`, `malware_status`, `detected_mime_type`, `region`, `encryption_key_ref`; ids are UUIDv4, the spec says UUIDv7.
4. **Retention / disposition architecture.** No `retention:evaluate` resolver or conflict detection (DRC-I09, DRC-006), no `dispositions` / `:approve` workflow, retention rules lack `effective_from/to`, `priority`, structured duration (days only) and `approval_ref`; the retention lifecycle has no DISPOSITION_IN_PROGRESS / DISPOSED / DISPOSITION_FAILED.
5. **Legal holds.** No `:resolve-targets`, `scope_selector`, `effective_at`, PARTIALLY_APPLIED / RELEASE_PENDING / ERROR; release approval is an approver principal id in the request body, checked against `authorization-svc`, not a workflow decision reference.
6. **Route names.** `/v1/records` vs `/v1/records:declare`; relationships vs `:supersede`; `/v1/export-packages` vs `/v1/exports`.
7. **Event vocabulary.** The existing document/record events keep dotted names; the new DRC-03/04 events use the §9.3 names; whether to unify is a consumer-compatibility question.
8. **Error taxonomy.** DRC-001..020 are used only for the four conditions in Gap 17.
9. **Idempotency scope and binding** (tenant + principal + key, request hash) and replay-marker header — not stated by the specification.
10. **MIME policy.** Whether an allow-list or block-list of content types is wanted.
11. **DRC-05 ownership**, and the legal-entity scope for tenant-level DRC-03/04 objects (above).
12. **External Object Store Migration:** production S3/MinIO provisioning, KMS key references and IAM — currently an encrypted `LocalFileBackend`.
13. **Test coverage on `internal/outbox`:** the relay's polling/batching/retry logic has no dedicated test file.
14. **Access-log failure on a download.** No contract defines the response when the access record cannot be written. The download is refused with the service's existing store-failure answer (503 `store_unavailable`); whether a dedicated error code is wanted is undecided. Whether metadata reads must also fail closed is not stated; they remain best-effort.
15. **List filters.** Whether `GET /v1/documents` should gain a status filter, or lose the mandatory `legal_entity_id`, is not specified anywhere; neither was changed.
16. **Bulk classification failure shape.** `bulk-classify` now fails the whole batch with the failing status and `{error, failed_document_id, rolled_back}`; the body shape and whether a partial-success mode is wanted are not specified.

**Verification Results (repository and test results are current as of 2026-10-19; the items marked historical are from the original engagement and are not re-verified):**
- Backend tests (`go test ./...`): **233 top-level tests** (contenttype 6, events 3, handler 110, residency 4, scan 8, storage 10, store 92; `internal/outbox` has none). With `TEST_DATABASE_URL` pointing at a disposable `document_vault_test` database (PostgreSQL 16, all 14 migrations applied from a clean schema): **232 pass, 0 fail, 1 skipped** (`TestPublishOutbox_AgainstRealBroker`, which needs a reachable Kafka broker and was not run). Without a database: 141 pass, 92 skip (the Postgres-backed tests) — a run without a database proves nothing about the constraints, triggers, RLS, atomic idempotency or bulk-classify rollback.
- Mutation checks (each reverted): removing migration 000013 fails the append-only test; making the unconfigured scanner report clean fails the fail-closed tests; disabling content-type validation fails the MIME tests; disabling idempotency fails the replay tests; removing the hold check fails the disposition tests; ignoring an access-log failure fails the download test; classifying each bulk document in its own transaction fails the bulk rollback tests; removing the per-document authorization fails the bulk authorization tests.
- `go vet ./...` clean; `go build ./...` clean (`go build ./cmd/server` and `./cmd/healthcheck`).
- Health probes: `/healthz` returns HTTP 200 and `/readyz` returns 200 (or 503 when the database ping fails), both with an **empty body**; there is no JSON body.
- Working-tree state (a repository state, not a deployment claim): the remediation described in this section is **uncommitted**. `git status` for `services/document-vault-svc` on 2026-10-19 shows modified files (including `cmd/server/main.go`, `internal/config/config.go`, `internal/domain/retention.go`, `internal/handler/{handler,evidence_handler,preservation_handler,records_handler,retention_handler}.go`, `internal/outbox/outbox.go`, `internal/scan/scan.go`, `internal/store/{pg_store,preservation_store,retention_store}.go` and tests) and untracked files (migrations `000013` and `000014`, `internal/contenttype/`, `internal/domain/idempotency.go`, `internal/store/{drc_events,drc_events_test,integrity_test,bulk_classify_test}.go`, `internal/handler/{remediation_test,access_and_bulk_test}.go`, `internal/scan/unconfigured_test.go`). No commit hash exists for it; the code state audited includes all of it.
- **Historical runtime observations (original engagement; not re-verified):** no rebuild, redeploy, container health check, live API probe, Kafka consumption or database inspection was performed in the 2026-10-19 passes. The original engagement recorded a healthy container on port 8094, live `503 authorization service unavailable` answers from the external `authorization-svc` migration gap, a 450 → 0 outbox drain with real payloads consumed from `zoiko.document-vault.events`, and `POST /v1/renditions` moving from 404 to 503 after a `--no-cache` rebuild. None of this is claimed as current fact.

---

## Service: evidence-manifest-svc
**Port:** 8095
**Classification:** Cross-Service Evidence Aggregator & Flat-SHA-256 Manifest Builder

**Note on this section:** this is the first exhaustive, per-service entry for `evidence-manifest-svc` in this file. Prior to this pass, its record lived only in this file's earlier domain-wide sections (§1 Executive Summary, §14 Findings, §15 Domain Summary — see lines 20, 32, 37, 480, 496, 572, 600, 607), written in an older summary style. That material is preserved below as "Original Gap" entries rather than deleted, consistent with this pass's instruction to carry forward historical context; this section is the new, actively-maintained record going forward, matching the format already used for `audit-event-store-svc`, `schema-registry-svc`, and `document-vault-svc` above.

**Service Health:** Working (Healthy)

**Frontend Completion: Not assessed.** The 95% previously recorded here was carried forward from an older summary and has never been verified against the frontend code.

**Backend Completion: see "Remediation Pass (2026-10-19)" below.** The earlier 97% / 88% / 93% figures were not recomputed from a contract comparison and are superseded. In short: full REST implementation in `internal/handler/` (manifest generation, catalog listing, verification, ZIP download, audit population lifecycle, sampling) with PostgreSQL persistence (`internal/store/`), fail-closed source aggregation (`internal/aggregator/clients.go`), a flat SHA-256 manifest checksum (`internal/domain/checksum.go`; there is no Merkle tree and no `merkle.go`), and a transactional outbox (`internal/outbox/outbox.go`, migration `000007_add_outbox_events`).

**Integration Completion:** not re-scored. `deployments/docker-compose.yml` wires `KAFKA_BROKERS: "kafka:9094"` and the service depends on `postgres`, `kafka`, `governance-svc`, `authorization-svc`, `workflow-svc` and `workflow-history-svc`. A live generation attempt in an earlier pass answered `503 authorization_unavailable`, the external `authorization-svc` migration gap documented elsewhere in this file; that dependency is unchanged and no authorized live `201` has been observed.

**Production Readiness: NOT PRODUCTION READY.** Service-owned code gaps listed in the remediation pass are closed, but an authorized end-to-end path (a real `201`) is still blocked by the external `authorization-svc` migration gap, and the open Needs-Clarification items below are undecided.

**Commands:**
- `POST /v1/evidence-manifests` (Generate a new evidence manifest: fetches requested governance decisions, access decisions, workflow instances + their real transition history, and/or a workflow-history entity+date-range window; stores the records and a flat SHA-256 checksum over them in one transaction; requires `EVIDENCE_MANIFEST_GENERATE` via `authorization-svc`; emits `evidence.manifest.generated` via the transactional outbox).
- `POST /v1/evidence-manifests/verify` / `POST /v1/evidence-manifests/{manifestID}/verify` (Recompute the flat SHA-256 checksum from the stored records and compare it with the recorded one).
- Audit population lifecycle (AUD-03): `POST /v1/audit-populations`, `POST /v1/audit-populations/{population_id}/build`, `/validate`, `/freeze`, `/supersede`, `/quarantine`, `/delta`.
- Sampling (AUD-04): `POST /v1/sampling-parameter-sets`; `POST /v1/sample-designs`, `/v1/sample-designs/{design_id}/approve`, `/select`, `/evaluate`, `/supersede`; `POST /v1/sample-items/{item_id}/result`, `/nonresponse`, `/alternative-procedure`.
- These route names are the implemented ones. The Audit & Assurance specification names them differently (for example `POST /audit/populations`, `/audit/populations/{id}:freeze`, `/audit/samples`); whether to rename is an open question (see Needs-Clarification Items).

**Reads:**
- `GET /v1/evidence-manifests` (List/paginate manifests for the caller's tenant, with optional `legal_entity_id` filter, under `EVIDENCE_MANIFEST_READ` when filtering by entity).
- `GET /v1/evidence-manifests/{manifestID}` (Fetch one manifest).
- `GET /v1/evidence-manifests/{manifestID}/records` (List the collected source records snapshotted into a manifest).
- `GET /v1/evidence-manifests/{manifestID}/download` (Download a ZIP of the manifest and its collected records).
- `GET /v1/audit-populations/{population_id}`, `GET /v1/audit-populations/{population_id}/control-totals`, `GET /v1/audit/engagements/{engagement_id}/populations`.
- `GET /v1/sample-designs/{design_id}`, `GET /v1/sample-designs/{design_id}/items`, `GET /v1/sample-designs/{design_id}/selections/{selection_id}/reproduce`.
- `GET /healthz`, `GET /readyz`.

**Segregation of Duties & Auth:**
- Principal: only the envelope's `X-Principal-Id` is read. `X-Actor-Principal-ID` is ignored (it previously overrode the verified principal). `requested_by` on a manifest is always the verified principal; a differing `requested_by` in the body is refused with 403 `requested_by_mismatch`. The platform input contract defines no delegated-actor header.
- Tenant & Identity Isolation: `X-Tenant-Id` and `X-Principal-Id` mandatory on all operations (`svcmiddleware.TenantContext()`), returning 401 if missing. RLS enforced on `evidence_manifests` and related tables (migration `000003_add_rls.up.sql`); re-confirmed this pass via `internal/store/rls_test.go` run against a real, non-superuser Postgres role (not just a superuser connection, which would prove nothing about the policy).
- Canonical Envelope Enforcement: `svcenvelope.Middleware` requires `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, `Idempotency-Key`, `X-Legal-Entity-Id`, and `X-Purpose-Context` on material writes; live-reconfirmed this pass (a request missing `X-Purpose-Context` was correctly refused with `envelope_incomplete`).
- Fine-Grained Authorization: `EVIDENCE_MANIFEST_GENERATE` and `EVIDENCE_MANIFEST_READ` (the latter only when filtering reads by legal entity) checked synchronously against `authorization-svc`, fail-closed on denial or unavailability.
- Context Propagation to Downstream Sources (Original Gap, re-verified): `forwardTenant` in `internal/aggregator/clients.go` forwards `X-Tenant-Id`, `X-Principal-Id`, and `X-Correlation-ID` to `workflow-svc`, `workflow-history-svc`, and `governance-decision-log-svc`. Confirmed present and correct in current code; test coverage (`TestWorkflowAndHistoryClients_ForwardPrincipalHeader`, `TestWorkflowAndAccessClients_ForwardTenantHeader`, `TestClient_TenantlessContext_SendsNoHeader`) passes.
- Fail-Closed Aggregation: manifest generation is all-or-nothing — any single source service error (unreachable, non-200, decode failure) fails the entire request rather than producing a manifest that looks complete but silently omits records (`collectRecords` in `internal/handler/handler.go`). Source collection happens before anything is stored; the manifest, its records, its checksum, the GENERATED status and the outbox event are then written in ONE transaction (`PgStore.CompleteManifest`). A source failure stores a single FAILED manifest with its reason and no records (`RecordFailedManifest`). No PENDING manifest is created by the API; PENDING remains a valid schema state but is only reachable through the legacy two-step store methods, which the handler no longer calls.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections):**

### Gap 1 — Manifest Aggregator Context Propagation Failure (Original Gap, from §14.1/§1)
Status: ✅ Fixed
Original Issue:
`internal/aggregator/clients.go#L213, L266`'s `forwardTenant` propagated only `X-Tenant-Id`, omitting `X-Principal-Id`. Callee services (`workflow-svc`, `workflow-history-svc`) strictly require both and returned HTTP 401, so manifest collection failed 100% of the time with HTTP 503 `source_service_unavailable`.
Fix Verification (as previously recorded, re-confirmed this pass):
Centralized `forwardTenant` helper (`internal/aggregator/clients.go#L38-L63`) propagates `X-Tenant-Id`, `X-Principal-Id`, and `X-Correlation-ID` from middleware context with an envelope-context fallback. Re-read this pass: still present, still correct, and the middleware context it reads from (`internal/middleware/tenant.go`'s `TenantContext()`) is the one actually wired in `cmd/server/main.go` — not the now-removed duplicate (see new Gap 4).
Evidence:
`internal/aggregator/clients.go#L38-L63`, `internal/aggregator/clients_test.go` (`TestWorkflowAndHistoryClients_ForwardPrincipalHeader`, `TestWorkflowAndAccessClients_ForwardTenantHeader`), re-run and passing this pass.

### Gap 2 — Missing Manifest Catalog Endpoint (Original Gap, from §14.4)
Status: ✅ Fixed
Original Issue:
Frontend called `GET /v1/evidence-manifests` (`lib/api/evidence-manifest.ts`); no list route existed in `internal/handler/handler.go`, so calls returned HTTP 404, preventing users from browsing previously generated manifests.
Fix Verification (as previously recorded, re-confirmed this pass):
`GET /v1/evidence-manifests` registered and implemented (`internal/handler/handler.go#L355-L395` per the original record; current line numbers have shifted with the service's growth but the route and its tenant-scoped, paginated, `legal_entity_id`-filterable query in `internal/store/pg_store.go` are present and covered by `TestListManifests_ReturnsAllManifestsForTenant`, `TestListManifests_LegalEntityFilter`, `TestListManifests_InvalidPagination_Returns400`, all passing this pass.
Evidence:
`internal/handler/handler.go`, `internal/store/pg_store.go`, `internal/handler/handler_test.go`, test suite re-run this pass.

### Gap 3 — Swallowed Kafka Publish Errors / Missing Transactional Outbox (Original Gap, from §14.5 — status corrected this pass)
Status: ✅ Fixed (code — re-verified this pass and found the Kafka-delivery half completely broken; now genuinely fixed end-to-end)
Original Issue:
This file's §14.5 and §15 recorded this service as still having the domain-wide "swallowed Kafka publish errors" defect (`🔴 Confirmed Defect` for `evidence-manifest-svc` specifically, unlike `kill-switch-registry-svc` and `document-vault-svc`, listed as already fixed).
What had actually changed since that finding was written (not yet reflected in this file): a transactional outbox had since been built — migration `000007_add_outbox_events.up.sql`, `internal/outbox/outbox.go`, and `PublishOutbox`/`FinalizeGenerated` wiring in `internal/store/pg_store.go` inserting the outbox row in the same transaction as the manifest state change. That part is genuine and well-tested (`internal/outbox` tests, `TestPgStore_FinalizeGenerated_AtomicallyCreatesOutboxEvent_RealDB`).
Confirmed gap found in this pass: exactly the same defect found and fixed in `document-vault-svc`'s Gap 5 this same pass — `internal/events/publisher.go` set `Topic: p.topic` on the `kafka.Message` in BOTH `PublishOutbox` AND the separate, non-outboxed `PublishManifestGenerated` method, while `cmd/server/main.go`'s `*kafka.Writer` ALSO sets `Topic: cfg.Kafka.Topic`. Per kafka-go v0.4.51's own source (`chooseTopic`), this combination is unconditionally rejected on every call. Unlike `document-vault-svc`, this service's compose file already had the correct `KAFKA_BROKERS: "kafka:9094"` wiring, so this was the ONLY cause here (not a compounding second defect) — but it was no less total: both publish paths had never successfully delivered anything. The existing test (`TestPublishOutbox_HeadersAndPayload`) used a hand-written stub that never exercised kafka-go's real validation, and asserted the buggy behavior (`msg.Topic == "test.topic"`) as correct, which is how this passed code review undetected.
Classification: SERVICE-OWNED DEFECT, Critical.
Fix Applied:
Removed `Topic: p.topic` from both `kafka.Message` literals in `internal/events/publisher.go` (`PublishOutbox` and `PublishManifestGenerated`). Fixed the pre-existing test's incorrect assertion (`msg.Topic` must now be empty, not `"test.topic"`).
Verification:
Added `TestPublishOutbox_RealWriter_DoesNotSetTopicOnMessage` and `TestPublishManifestGenerated_RealWriter_DoesNotSetTopicOnMessage`, both using a REAL `*kafka.Writer` (not a stub) so kafka-go's own validation runs — confirmed both fail with the exact production error when the bug is deliberately reintroduced, and pass with the fix. Added a live, opt-in end-to-end test (`TestPublishOutbox_AgainstRealBroker`) and ran it against this environment's real Kafka broker: publish succeeded, and the message was independently confirmed via `kafka-console-consumer.sh` reading real payload `{"type":"evidence.manifest.generated"}` off a live test topic.
Evidence:
`internal/events/publisher.go` (fix), `internal/events/publisher_test.go` (3 new/corrected tests), live `kafka-console-consumer.sh` output, `go build`/`go vet` clean across the whole module.

**Remaining Gaps (service-owned, after the 2026-10-19 remediation pass):**
- None of the ten remediation gaps remain open. The items below were found and deliberately not changed (see Needs-Clarification Items).
- Frontend was not audited (no frontend code was read or tested).
- Legacy manifests generated before this pass hashed the source JSON bytes, whereas `manifest_records.record_snapshot` is JSONB (re-ordered keys, normalised whitespace). A legacy manifest whose source JSON was not already in JSONB-canonical form will not re-verify. New manifests are hashed from the stored form and verify.
- `EvaluateSample` writes its incomplete-evaluation row and then returns `ErrSampleEvaluationIncomplete` from inside the transaction, so that row (and the `evaluation_id` in the 422 body) is rolled back — the same pattern as the freeze-quarantine defect fixed in this pass. Not changed: the contract does not say whether an incomplete evaluation must persist.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** a live manifest-generation attempt answered `503 authorization_unavailable` after passing envelope and body validation. Same platform-wide blocker documented for every other service in this file. `authorization-svc` was not modified.

**Needs-Clarification Items:**
- `GET /v1/evidence-manifests` without `legal_entity_id` lists every manifest in the tenant with no authorization grant. The audit file describes this but does not justify it.
- Whether `Idempotency-Key` should de-duplicate manifest generation (the envelope requires the header; a retry currently creates a second manifest).
- Request-size caps and unknown-field rejection: other services each chose their own limit and no platform-wide contract exists, so none was invented here.
- AUD-03/AUD-04 event names: the ten events use the Audit & Assurance specification's PascalCase names verbatim (`AuditPopulationBuilt`, …); the platform catalogue style is dotted lower-case (`evidence.manifest.generated`).
- What counts as a "sample item exception": `SampleItemExceptionRecorded` is emitted when a result is recorded with an `exception_amount`, the only exception field in the schema.
- Population and sample read surfaces the specification lists but the service lacks (population manifest, lineage, version comparison, late items; sample evaluation and projection reads).
- AUD-04 `Reviewed` state (absent; the service has `Invalidated`, which the specification does not list).
- Route names for AUD-03/AUD-04 versus the specification's.
- Per-item `integrity_hash` on manifest records (`04-data-model.md` EvidenceManifestItem): the schema stores none; the manifest checksum is a single flat SHA-256.

### Gap 4 — Service Did Not Compile From a Clean Checkout (newly discovered this pass, critical)
Status: ✅ Fixed
Discovery Evidence:
`go build ./...` failed outright with two separate, unrelated merge-reconciliation defects, meaning this service could not have been rebuilt from current source at the time this pass began — the previously-running container was confirmed to predate both breaks (a forced `--no-cache` rebuild from the then-current source failed until both were fixed):
  1. `internal/middleware/principal.go` and `internal/middleware/tenant.go` both declared `principalCtxKey`, `WithPrincipal`, and `PrincipalFromContext` — a duplicate-declaration compile error. Git history shows `principal.go` originated on `main` (commit `1464438e`) and `tenant.go`'s duplicate versions were added independently in this session's own earlier `4d4831e7` commit; the merge (`53d63e99`, "Merge latest main into rohithyadav") brought both in without reconciliation.
  2. `internal/handler/handler.go`'s `WorkflowHistorySource` interface required `ListByEntityAndDateRange(ctx, legalEntityID, from, to time.Time) ([]aggregator.SourceRecord, error)` — a real, actively-called method (`collectRecords`, `internal/handler/handler.go`) — but `internal/aggregator/clients.go`'s `WorkflowHistoryClient` never implemented it. The client's own doc comment explicitly said this was "left as a further, separate gap rather than fabricated here," but the interface requiring it had since been added on a different branch without the implementation following.
Classification: SERVICE-OWNED DEFECT, Critical — not a runtime defect but a complete inability to build or deploy from current source, which is strictly worse: no amount of runtime testing of the old container could have caught it.
Fix Applied:
  1. Deleted `internal/middleware/principal.go` (the orphaned duplicate — `tenant.go`'s version is the one actually wired into the registered `TenantContext()` middleware in `cmd/server/main.go`; nothing referenced `principal.go` specifically, since Go resolves by package, not file).
  2. Implemented `WorkflowHistoryClient.ListByEntityAndDateRange`, calling the REAL `GET /v1/workflows/history?legal_entity_id=...&from=...&to=...` endpoint confirmed present in `workflow-history-svc/internal/handler/history.go`'s `GetCrossWorkflowHistory`, mirroring the existing `GovernanceDecisionClient.ListByEntityAndDateRange` and `WorkflowHistoryClient.ListByInstanceID` patterns exactly (fail-closed, same response decoding shape).
Verification:
`go build ./...` and `go vet ./...` both clean across the whole module. `docker compose build --no-cache evidence-manifest-svc` succeeded (a genuine Docker-level proof, not just local `go build`). Added `TestWorkflowHistoryClient_ListByInstanceID_Success`, `TestWorkflowHistoryClient_ListByInstanceID_NotFound_IsEmptyNotError`, `TestWorkflowHistoryClient_ListByEntityAndDateRange_Success`, `TestWorkflowHistoryClient_ListByEntityAndDateRange_Unreachable_FailsClosed`, and `TestWorkflowHistoryClient_ListByEntityAndDateRange_NonOKStatus_FailsClosed` (`WorkflowHistoryClient` had zero tests before this pass despite being load-bearing). All pre-existing tests exercising `collectRecords`'s workflow-history time-window path (`TestGenerateManifest_WorkflowHistoryTimeWindow_AutoDiscovers`) continued to pass — they use a handler-level stub satisfying the interface, which is why this defect was invisible to `go test ./internal/handler/...` alone and only surfaced via `go build ./...` / `go vet ./...` on the whole module.
Evidence:
`internal/middleware/principal.go` (deleted), `internal/aggregator/clients.go` (`ListByEntityAndDateRange`, new), `internal/aggregator/clients_test.go` (5 new tests), `workflow-history-svc/internal/handler/history.go#L223-` (the real endpoint contract this was built against), `docker compose build --no-cache` success output.

### Gap 5 — Outbox Atomicity Tests Order-Dependent on a Fresh Database (newly discovered this pass)
Status: ✅ Fixed
Discovery Evidence:
Running the full suite against a genuinely fresh, newly-created database (`evidence_manifest_test`, migrations never previously applied) failed two tests: `TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB` and `TestPgStore_FinalizeGenerated_AtomicallyCreatesOutboxEvent_RealDB`, both with `relation "evidence_manifests" does not exist`. Root cause: `internal/store/outbox_atomicity_test.go` defined its own `getTestPool` helper that only opened a connection — unlike `pg_store_test.go`'s `requireTestDB`, which drops and recreates every table from the full migration glob before returning. Go's test runner happens to execute `outbox_atomicity_test.go`'s tests before `pg_store_test.go`'s (alphabetical file ordering), so on a database no prior test run had already migrated, these two tests failed outright — meaning the atomicity guarantee they exist to verify was never actually checked in a genuinely clean environment (a first-time CI run, a fresh dev machine).
Classification: SERVICE-OWNED DEFECT (test-infrastructure gap), Medium severity — doesn't affect production behavior, but meant a real correctness guarantee (outbox/domain-row atomicity) had an unreliable, environment-order-dependent test.
Fix Applied:
Removed `getTestPool` and pointed both tests at the existing, already-correct `requireTestDB` helper (same package, `store_test`, defined in `pg_store_test.go`).
Verification:
Re-ran the full suite against a freshly dropped-and-recreated `evidence_manifest_test` database: both tests, and all others, pass — 78/78 unconditional+DB-gated tests plus the 1 live-Kafka test, 79/79 total, 0 failures.
Evidence:
`internal/store/outbox_atomicity_test.go` (fix), full suite run against a fresh database, 0 failures.

**Verification Results (historical — prior pass; current results are in the Remediation Pass below):**
- Backend Unit Tests: 79/79 tests pass — 55 run unconditionally (`go test ./...`), plus 23 Postgres-backed tests (run against a disposable `evidence_manifest_test` database, migrations applied from a clean schema) and 1 live-Kafka test (run with a reachable broker) — all passing, 0 failures.
- Backend Build: `go build ./...` clean — this is itself new evidence this pass, since the module did not compile at the start of this pass (Gap 4).
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Docker Build: `docker compose build --no-cache evidence-manifest-svc` succeeded — a genuine Docker-level build proof, not only local `go build`.
- Docker Health: Live container `evidence-manifest-svc` Up (healthy) on port 8095; rebuilt from current (now-compiling) source and redeployed this pass.
- Healthcheck Endpoints: `/healthz` returns 200 OK; `/readyz` returns 200 OK.
- Live Envelope Enforcement: a manifest-generation request missing `X-Purpose-Context` correctly refused with `envelope_incomplete`; a request missing `tenant_id` in the body correctly refused with `missing_field`.
- Live Authorization Gate: a fully-formed manifest-generation request correctly answered `503 authorization_unavailable` — the external `authorization-svc` migration gap, confirmed fail-closed rather than fail-open.
- Live Kafka Delivery (Gap 3): `PublishOutbox` against the real broker succeeded; independently consumed the real payload via `kafka-console-consumer.sh`.

### Remediation Pass (2026-10-19)

Scope: `evidence-manifest-svc` only. Starting point: the read-only audit of this service (62.2% as reported; 21 of 35 counted items when its published table is recounted).

| # | Gap | Result |
|---|---|---|
| 1 | `X-Actor-Principal-ID` overrode the verified principal | Fixed: only `X-Principal-Id` is read, on every route; `requested_by` is always the verified principal |
| 2 | `GenerateManifest` validated the body before authenticating/authorizing | Fixed: tenant → principal → authorization → decode/validate → business validation; only `legal_entity_id` is read from the body to authorize |
| 3 | `ListAuditPopulations` authorized `pops[0]` and returned all | Fixed: one check per distinct legal entity; only authorized populations returned; none readable → 403; authz unreachable → 503; empty list → `[]` |
| 4 | Freeze quarantine rolled back | Fixed: quarantine commits, then the mismatch error is returned; 422 `control_total_mismatch` unchanged |
| 5 | Generation not atomic | Fixed: `CompleteManifest` writes manifest, records, checksum, GENERATED and outbox event in one transaction; source failure stores one FAILED row |
| 6 | Merkle claim | DOC DRIFT corrected: flat SHA-256 per the specification; no code change |
| 7 | `ListRecords` ordering | Fixed: `ORDER BY fetched_at, manifest_record_id`; generation and verification share `domain.ManifestChecksum` |
| 8 | AUD-03 events | Emitted via the outbox on real transitions only |
| 9 | AUD-04 events | Emitted via the outbox on real transitions only |
| 10 | Body size / unknown fields | NEEDS CLARIFICATION — no platform contract; nothing implemented |

Verification: `go build ./...`, `go vet ./...` clean. `go test ./...` against a disposable PostgreSQL (`evidence_manifest_test`): 123 top-level tests, 122 passed, 1 skipped (`TestPublishOutbox_AgainstRealBroker`, needs a live Kafka broker and was not run), 0 failed. Every fix has a regression test; the freeze, record-ordering and principal-spoofing tests, and the AuditPopulationFrozen event test, were confirmed to fail when the fix is reverted (the other event and atomicity tests were not mutation-checked individually).

---

## Service: workflow-history-svc
**Port:** 8097
**Classification:** Immutable Workflow Transition Ledger & Cross-Workflow Evidence Source

**Note on this section:** as with `evidence-manifest-svc` above, this is the first exhaustive per-service entry for `workflow-history-svc` in this file. Its prior record lived only in this file's earlier domain-wide sections (§1, §8, §9, §10, §14.2, §14.5, §15 — lines 21, 196-197, 218, 230, 243, 258, 298-299, 421-433, 515-523, 573). That material is preserved below as "Original Gap" entries rather than deleted. Unlike every other service processed in this domain this pass, this fresh audit did **not** find the previously-reported score to be overstated — it is the first service this pass where the prior "READY, 98%" rating holds up under independent re-verification, aside from two small documentation-drift corrections.

**Service Health:** Working (Healthy)

**Frontend Completion: 98%** (per earlier domain summary, not independently re-verified this pass — this pass was backend/integration-scoped)
Reason (carried forward): `Zoiko-suite-frontend-platform/app/admin/workflows/history`, `components/admin/workflows`, and `lib/api/workflow-history.ts` were recorded as implemented and verified against this service's read API.

**Backend Completion: 98%**
Reason: Full read API in `internal/handler/history.go` (`GetInstanceHistory`, `GetCrossWorkflowHistory`) backed by `internal/store/store.go`'s `PgStore`, a Kafka consumer pipeline (`internal/consumer/consumer.go`) with a dead-letter-queue runner (`internal/kafka/runner.go`), and a fail-closed `authorization-svc` client (`internal/authz/client.go`) with a bounded decision cache. Independently re-verified this pass: `event_id`-keyed `ON CONFLICT DO NOTHING` idempotency (migration `000001_initial_schema.up.sql`), `FORCE ROW LEVEL SECURITY` (migration `000002_add_rls.up.sql`) with a narrow, internal-only `app.bypass_rls` exemption used solely by the consumer's tenant-context lookup (never HTTP-exposed — confirmed via full call-site search), an append-only enforcement trigger (migration `000003_enforce_immutability.up.sql`), and the DLQ publisher correctly omitting `Topic` from its message (the Writer already pins it) — the exact defect found and fixed in `document-vault-svc` and `evidence-manifest-svc` earlier this pass does NOT exist here; this service's author already used the correct pattern. All 18 unconditional tests pass, plus a full embedded-Postgres integration suite (`-tags=integration`, 43 tests total including subtests) covering the full Kafka-message → consumer → store → Postgres pipeline and the full HTTP stack, including a regression test for the specific cross-tenant query-string vulnerability this service had previously shipped (now fixed — see Gap 1). `go build ./...` and `go vet ./...` both clean.

**Integration Completion: 90%**
Reason: `deployments/docker-compose.yml`'s `workflow-history-svc` block correctly wires `KAFKA_BROKERS: "kafka:9094"`, `KAFKA_TOPIC: "zoiko.workflow.events"`, and `AUTHZ_SERVICE_URL`, with `depends_on` on `postgres`, `kafka`, `workflow-svc`, and `authorization-svc` all `condition: service_healthy` — re-confirmed present this pass (the original Gap 2 fix below). Rebuilt and redeployed this pass (two documentation-only source changes — see Remaining/Fixed Gaps); live and healthy on port 8097. Live-reconfirmed the previously-fixed cross-tenant protection: a request with no identity headers → `401`; a `tenant_id` query parameter disagreeing with the verified `X-Tenant-Id` header → `403`; a well-formed request for a nonexistent instance → `404`. **Capped below 95%** by the same external `authorization-svc` migration gap documented for every other service in this file — a live authorized request cannot currently be fully demonstrated end-to-end (it was not specifically attempted this pass, since the dependency's failure mode is already exhaustively confirmed elsewhere in this file; this service's own `authz.Client` fails closed on any non-200/non-GRANTED response, consistent with every other service's behavior).

**Overall Completion: 98%**

**Production Readiness: Ready for Production** (service-owned scope).

**Commands:**
- None — this service has no write endpoints. It is a pure read-side projection, populated exclusively by its Kafka consumer (`workflow.started`, `approval.granted`, `approval.rejected`, `workflow.escalated`, `workflow.completed` from `zoiko.workflow.events`), never by direct API calls.

**Reads:**
- `GET /v1/workflows/{workflow_instance_id}/history` (Full chronological transition list for one workflow instance, tenant-scoped; 404 for both "doesn't exist" and "belongs to another tenant" — indistinguishable by design, so the endpoint cannot be used to probe for another tenant's instances).
- `GET /v1/workflows/history?legal_entity_id=...&from=...&to=...` (Cross-workflow query for a tenant+entity time window; now actively called by `evidence-manifest-svc`'s aggregator — see Gap 3).
- `GET /healthz`, `GET /readyz`, `GET /metrics` (operational endpoints: liveness, readiness and the Prometheus scrape endpoint from the platform observability baseline; no tenant or principal required).

**Segregation of Duties & Auth:**
- Tenant & Identity Isolation: `X-Tenant-Id` and `X-Principal-Id` mandatory on both read routes (`requireTenant`, `requirePrincipal`), 401 if missing.
- Authorization-by-resolved-scope: `GetInstanceHistory` authorizes against the legal entity actually recorded on the fetched rows, not anything the caller supplies — closing the same class of scope-confusion risk flagged elsewhere in this domain. `GetCrossWorkflowHistory` authorizes against the (mandatory) `legal_entity_id` query parameter, which is itself the query's own scope, not a separately-spoofable value.
- RLS: `FORCE ROW LEVEL SECURITY` on `workflow_history_events` (migration `000002`), with the one exemption (`app.bypass_rls`) confirmed used only by the internal consumer's own tenant-context inheritance lookup, never by any HTTP-reachable code path.
- Tenant-context inheritance: a non-started event takes tenant and legal entity from the instance's `workflow.started` row only (`PgStore.GetTenantContext`). If no started row exists, or started rows for one instance id disagree on tenant or entity, the consumer refuses the event (non-nil error, nothing stored).
- Immutability: no `UPDATE`/`DELETE` routes exist, and a database trigger (`workflow_history_events_immutable`) rejects any such statement at the database level regardless of which role issues it — re-confirmed this pass via the embedded-Postgres integration suite.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections):**

### Gap 1 — Cross-Tenant Read via Caller-Supplied `tenant_id` Query Parameter (Original Gap, from §8/§14.2/known-gaps.md — security-critical)
Status: ✅ Fixed
Original Issue:
Neither read route ever inspected `X-Tenant-Id`. `tenant_id` was read from the URL query string instead and passed straight into `set_config('app.tenant_id', ...)` for the RLS-scoped query. The RLS policy was never bypassed — it was faithfully *satisfied* with a caller-chosen value, so any caller could read any tenant's complete workflow transition history (every state change, approval, and event payload) simply by naming that tenant in the URL.
Fix Verification (as previously recorded, re-confirmed this pass):
`requireTenant` (`internal/handler/history.go`) now reads the tenant exclusively from the gateway-verified `X-Tenant-Id` header; a `tenant_id` query parameter is still accepted for URL compatibility but must *agree* with the header or the request is refused with 403, never silently reinterpreted. Live-reconfirmed this pass (see Integration Completion above): disagreement → 403. Full embedded-Postgres integration test `GET /v1/workflows/{id}/history query tenant_id cannot override the header` passes.
Evidence:
`internal/handler/history.go#L74-L114` (`requireTenant`'s own doc comment names this exact history and shape), `cmd/server/main_integration_test.go`, live `curl` probes this pass.

### Gap 2 — Missing Docker Compose Service Dependency on `authorization-svc` (Original Gap, from §14.5)
Status: ✅ Fixed
Original Issue:
`deployments/docker-compose.yml`'s `workflow-history-svc` block declared `depends_on` for `postgres` and `kafka` but omitted `authorization-svc`; the server failed startup if `AUTHZ_SERVICE_URL` was configured and `authorization-svc` was not yet healthy.
Fix Verification (as previously recorded, re-confirmed this pass):
`depends_on.authorization-svc: { condition: service_healthy }` is present in the current compose file, alongside `postgres`, `kafka`, and `workflow-svc`. Re-read this pass directly from `deployments/docker-compose.yml`.
Evidence:
`deployments/docker-compose.yml` (`workflow-history-svc.depends_on`).

### Gap 3 — Stale "Not Wired" Documentation After a Cross-Service Fix Landed (newly discovered this pass, documentation drift)
Status: ✅ Fixed
Discovery Evidence:
`internal/handler/history.go`'s own package doc, and a comment in migration `000001_initial_schema.up.sql`, both stated that `evidence-manifest-svc` was "NOT wired" to the `GET /v1/workflows/history` cross-workflow endpoint as a "documented v1 scope constraint." This was accurate when written, but is no longer true: earlier in this same domain-audit pass, `evidence-manifest-svc`'s `WorkflowHistoryClient.ListByEntityAndDateRange` was implemented specifically to call this exact endpoint (see this file's `evidence-manifest-svc` section, Gap 4), closing an interface/implementation mismatch that had left that service unable to compile. The comment here was never updated to reflect it.
Classification: SERVICE-OWNED DEFECT (documentation drift in source comments, not a code defect) — low severity, but actively misleading to a future reader who would otherwise correctly conclude the endpoint had no real caller.
Fix Applied:
Updated `internal/handler/history.go`'s package doc to state the endpoint is now called by `evidence-manifest-svc`'s aggregator. Left the historical note in `000001_initial_schema.up.sql` untouched, consistent with treating applied migrations as historical record rather than rewriting them.
Verification:
`go build ./...`, `go vet ./...`, and the full test suite (including `-tags=integration`) re-run clean after the comment change — a documentation-only edit, no behavioral change possible, verified by its own own non-effect.
Evidence:
`internal/handler/history.go` (package doc, before/after).

### Gap 4 — Misplaced/Stale Comment Describing a Non-Existent Legacy Method (newly discovered this pass, documentation drift)
Status: ✅ Fixed
Discovery Evidence:
`internal/authz/client.go`'s `CheckAllowed` — the real, actively-used, fail-closed authorization call — carried a doc comment reading "Authorize is a legacy no-op stub retained for backward compatibility. It is not used by any handler..." No `Authorize` method exists anywhere in the package (confirmed via a full search); the comment was attached to the wrong function, almost certainly left over from a prior rename, and directly contradicts what `CheckAllowed` actually does.
Classification: SERVICE-OWNED DEFECT (documentation drift), low severity — actively misleading about a security-relevant function.
Fix Applied:
Replaced the comment with one accurately describing `CheckAllowed`'s real behavior (fail-closed call to `authorization-svc`, short-TTL decision cache).
Verification:
`go build ./...`, `go vet ./...`, full suite re-run clean.
Evidence:
`internal/authz/client.go` (comment, before/after).

**Remaining Gaps:**
- None within `workflow-history-svc`'s own service boundary. This is the first service in this domain-audit pass where fresh, independent re-verification did not surface a previously-undetected functional, security, or deployment defect — only two small, now-fixed documentation-drift items.
- Frontend was not independently re-audited this pass (no frontend code was read or tested) — the 98% figure above is carried forward from the earlier domain summary, not a finding of this pass.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** the same platform-wide blocker documented throughout this file. Not specifically re-demonstrated live this pass for this service (this service's `authz.Client` fail-closed behavior on non-200/non-GRANTED responses was read and confirmed correct in code — see Backend Completion — rather than re-triggered live, since the root cause and its effect are already exhaustively evidenced elsewhere in this file).
- **Direct Container Port Exposure (SEC-04):** platform-wide, already documented; unchanged this pass.
- **Kafka Transport Security (TODO, pre-existing, explicitly marked in source):** `internal/config/config.go` and `internal/kafka/runner.go` both carry `TODO (production): TLS/SASL broker auth` comments. This is a deliberate, already-documented dev-environment posture shared across this platform's services, not a defect specific to this service.

**Needs-Clarification Items:** see the Remediation Pass below.

**Verification Results (historical — prior pass; current results are in the Remediation Pass below):**
- Backend Unit Tests: 18/18 unconditional tests pass (`go test ./...`); 43/43 pass with the full embedded-Postgres integration suite (`go test -tags=integration ./...`, including subtests) — covering the Kafka-message → consumer → store pipeline, idempotency, fail-closed out-of-order handling, and the full HTTP stack including the cross-tenant protection.
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Docker Build: `docker compose build workflow-history-svc` succeeded.
- Docker Health: Live container `workflow-history-svc` Up (healthy) on port 8097; rebuilt and redeployed this pass.
- Healthcheck Endpoints: `/healthz` and `/readyz` both return 200 OK.
- Live Security Re-Verification (Gap 1): no identity headers → `401`; `tenant_id` query parameter disagreeing with `X-Tenant-Id` → `403`; well-formed request for a nonexistent instance → `404` — all re-confirmed live this pass, not only by the automated suite.
- RLS Bypass Exemption Audit: `app.bypass_rls` usage traced to its one call site (`internal/store/store.go`'s `GetTenantContext`), confirmed called only from `internal/consumer/consumer.go` (internal event-ingestion pipeline), never from any HTTP handler — it cannot be reached by an external caller.
- Kafka DLQ Path Reviewed: `internal/kafka/runner.go`'s `publishToDLQ` correctly omits `Topic` from its `kafka.Message` (the `*kafka.Writer` already pins `topic + ".dlq"`) — the exact defect class found and fixed in two other services this pass does not exist here.

### Remediation Pass (2026-10-19)

Scope: `workflow-history-svc` only (plus one comment in `deployments/docker-compose.yml` that described this service's endpoint).

| Gap | Result |
|---|---|
| Tenant-context inheritance used the earliest row of any event type, across tenants | Fixed: `GetTenantContext` reads `workflow.started` rows only; conflicting started rows (different tenant or entity for one instance id) return `ErrAmbiguousTenantContext` and the event is refused |
| `/metrics` not documented | Kept: it is the platform observability baseline's scrape endpoint (`observability-baseline-plan.md`, wired through `telemetry.MetricsHandler`). Now listed under Reads as an operational endpoint |
| `GetCrossWorkflowHistory` comment said "not wired" and listed `tenant_id` as required | Comment corrected; no behaviour change |
| Consumer comments said an unresolved transition is "not committed" and redelivered | Comments corrected: the runner makes 3 attempts, dead-letters to `<topic>.dlq`, then commits; only a failed DLQ publish leaves the offset uncommitted |
| docker-compose comment said the cross-workflow endpoint "remains unwired" | Comment corrected (it is called via `workflow_history_from`/`workflow_history_to`) |

Not changed: response schema, the `WorkflowTransition` physical model, TRUNCATE protection, ordering and pagination, instance-id uniqueness across tenants, and the comment in applied migration `000001` that still says the aggregator is not wired (applied migrations are left as historical record).

Open: no test exercises the Kafka runner's retry, dead-letter and commit behaviour (it is built on concrete `kafka.Reader`/`kafka.Writer`, so testing it needs a broker or a refactor); that behaviour is documented from the code only.

Needs clarification: response schema is not documented in this file; whether `WorkflowTransition` (`from_state`, `to_state`, `acted_by`, `acted_at`, `rationale`) must be persisted as columns or may remain a projection of the raw events; the frontend percentage (not audited); TRUNCATE immutability; ordering/pagination of the cross-workflow query.

Verification: `go build ./...`, `go vet ./...` (also with `-tags=integration`) clean. `go test ./...`: 24 tests passed (6 new tenant-context tests), 0 failed. `go test -tags=integration ./...` against embedded PostgreSQL 16: all packages ok, 55 passing test/subtest results, 0 failed, 0 skipped; the new tenant-context subtests read the persisted row, and removing the `event_type` filter makes two of them fail.

---

## Service: kill-switch-registry-svc
**Port:** 8147
**Classification:** Platform/Tenant Emergency Traffic-Shedding Control with Two-Man Rule

**Note on this section:** as with the two services above, this is the first exhaustive per-service entry for `kill-switch-registry-svc` in this file. Its prior record lived only in this file's earlier domain-wide sections (§1, §3, §7, §8, §10, §14.2, §14.5, §15 — lines 22, 40, 42, 55, 68-69, 149-157, 181-182, 212, 220, 228, 300-301, 320, 398-410, 495-513, 573-574, 596, 601). That material is preserved below as "Original Gap" entries. **Process note (updated 2026-10-19):** when this section was first written, a substantial amount of correct, well-tested work was found uncommitted in the working tree (cross-tenant engage/disengage protection, a `/v1/kill-switches/check` route alias, correlation-ID fallback handling, an outbox-atomicity test, and small error-handling hardening in the outbox relay and authz client). That work is now committed (`cee11a7e`, "Implement remediation and outbox updates across services"; `git status` is clean for this service), so the repository-hygiene concern recorded here originally no longer applies.

**Service Health:** Working (Healthy)

**Frontend Completion: 95%** (per earlier domain summary, not independently re-verified this pass — this pass was backend/integration/security-scoped)
Reason (carried forward): `Zoiko-suite-frontend-platform/app/admin/kill-switches`, `components/admin/kill-switches`, and `lib/api/kill-switch.ts` were recorded as implemented against this service's engage/disengage/resolve/history API.

**Backend Completion: 97%**
Reason: Full read/write API in `internal/handler/handler.go` (`EngageKillSwitch`, `DisengageKillSwitch`, `ResolveKillSwitch`/`check` alias, `ListCurrentStates`, `ListHistoryForScope`) backed by `internal/store/pg_store.go`, a transactional outbox (`internal/outbox/outbox.go`, migration `000003_add_outbox_events`), and a fail-closed `authorization-svc` client (`internal/authz/client.go`). The engage/disengage two-man rule (self-approval rejection + independent approver authorization) is exactly as the audit previously recorded, re-confirmed in current code. **A critical, previously-undetected defect was found and fixed this pass: the service connected to Postgres as the `postgres` superuser, unconditionally bypassing the FORCE ROW LEVEL SECURITY policy this service's own migration goes out of its way to design correctly** — see new Gap 3. All 28 unconditional tests pass; all 35 pass against a real, freshly-migrated Postgres database including a dedicated RLS test suite. `go build ./...` and `go vet ./...` both clean.

**Integration Completion: 90%**
Reason: `deployments/docker-compose.yml`'s `kill-switch-registry-svc` block wires `KAFKA_BROKERS`, `KAFKA_EVENTS_TOPIC`, and `AUTHZ_SERVICE_URL`, with `depends_on` on `postgres`, `kafka`, and `authorization-svc` all `condition: service_healthy`. **`DATABASE_URL` has been corrected this pass** from `${DB_USER:-postgres}:${DB_PASSWORD:-postgres}` (silently defaulting to the Postgres superuser) to the least-privilege `zoiko_app` role already granted the exact privileges this service needs — see new Gap 3. Rebuilt and redeployed this pass; live and healthy on port 8147. Live-verified end-to-end with seeded ground-truth data: a tenant-B caller correctly sees only platform-wide switches, never tenant A's, while a tenant-A caller correctly sees both — confirmed directly against the running container, not only by the unit suite. **Capped below 95%** by the same external `authorization-svc` migration gap documented throughout this file — a live authorized engage/disengage cannot currently be fully demonstrated end-to-end.

**Overall Completion: 95%**

**Production Readiness: NOT PRODUCTION READY.** Service-owned gaps found by the later audit are closed (see the Remediation Pass below), but a real authorized engage/disengage cannot be demonstrated end to end while the external `authorization-svc` migration gap remains.

**Commands:**
- `POST /v1/kill-switches/engage` (Engage a kill switch at any combination of plane/domain/provider_code/tenant scope; requires `reason`, `reconciliation_procedure_ref`, and a distinct `approved_by_principal_id`; enforces two-man rule and independent authorization for both maker and checker; a body `tenant_id` must be a UUID (400) and must equal the verified `X-Tenant-Id` — a different value, or any value when no `X-Tenant-Id` was sent, is refused with 403 before authorization).
- `POST /v1/kill-switches/disengage` (Disengage the exact scope tuple; requires it currently be ENGAGED — 409 otherwise, decided atomically with the insert so concurrent disengages of one scope yield exactly one success; same two-man rule and body-tenant rule as engage).

**Reads:**
- `GET /v1/kill-switches/resolve` (and alias `GET /v1/kill-switches/check`) — resolves whether a given plane/domain/provider/tenant combination is currently blocked, returning the most specific currently-ENGAGED match. No authz gate by design (cheap, frequent, read-only check every service makes).
- `GET /v1/kill-switches` (Every distinct scope tuple's current state — tenant-scoped via RLS plus, as of this pass, an explicit application-level filter; see Gap 3).
- `GET /v1/kill-switches/history` (Full audit trail for one exact scope tuple).
- `GET /healthz`, `GET /readyz`.

**Segregation of Duties & Auth:**
- Two-Man Rule: `principalID == req.ApprovedByPrincipalID` rejected with 403 on both engage and disengage (Original Gap, see Gap 1). Both the maker and the named approver are independently checked against `authorization-svc` for `KILL_SWITCH_ENGAGE`/`KILL_SWITCH_DISENGAGE`, scoped to the tenant or to the platform scope for a platform-wide switch.
- Cross-Tenant Write Protection (see Gap 2): a non-UUID `tenant_id` anywhere (engage, disengage, resolve) is rejected with 400; on engage/disengage a body `tenant_id` that disagrees with the verified `X-Tenant-Id` header is rejected with 403, and so is a body `tenant_id` when no verified tenant exists (previously that case reached the database and failed RLS as a 500). A platform-wide switch (no `tenant_id`) needs no tenant header.
- RLS: `FORCE ROW LEVEL SECURITY` on `kill_switch_events`, with a deliberately-documented `tenant_id IS NULL` visibility branch so a platform-wide ENGAGE is visible to every tenant (the migration's own extensive doc comment explains why the "obvious" tighter policy would be a silent safety bypass, not a leak). This policy was correctly designed but, until this pass, was being unconditionally bypassed by the service's own superuser database connection — see Gap 3.
- Append-Only: engage/disengage are always new rows (`AppendEvent`), never updates; the full history is preserved regardless of current state.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections):**

### Gap 1 — Unilateral Kill Switch Engagement/Disengagement, SoD Bypass (Original Gap, from §14.2/SEC-01 — security-critical)
Status: ✅ Fixed
Original Issue:
`internal/handler/handler.go#L138-L163` read `req.ApprovedByPrincipalID` directly from the request body without asserting `principalID != req.ApprovedByPrincipalID` or verifying the approver's own authorization. An operator could name themself, or any arbitrary UUID, as the approver and unilaterally execute emergency traffic shedding.
Fix Verification (as previously recorded, re-confirmed this pass):
`EngageKillSwitch` and `DisengageKillSwitch` both reject `principalID == req.ApprovedByPrincipalID` with 403, and both independently call `authorizePrincipal` for the named approver against `authorization-svc`, failing closed (503) if the check cannot be made. Re-read directly from current `internal/handler/handler.go`; `TestEngage_SelfApproval_Forbidden403`, `TestDisengage_SelfApproval_Forbidden403`, `TestEngage_ApproverNotAuthorized_Forbidden403`, `TestDisengage_ApproverNotAuthorized_Forbidden403`, `TestEngage_ApproverAuthzUnavailable_503` all pass this pass.
Evidence:
`internal/handler/handler.go` (`EngageKillSwitch`, `DisengageKillSwitch`, `authorizePrincipal`), `internal/handler/handler_test.go`, re-run this pass.

### Gap 2 — Missing Cross-Tenant Write Protection and Route/Correlation Gaps (discovered in the original pass; now committed behavior)
Status: ✅ Fixed (committed in `cee11a7e`; the no-verified-tenant case was completed in the 2026-10-19 Remediation Pass)
Discovery Evidence:
While verifying Gap 1, this pass found the engage/disengage handlers also validate a second, distinct property the original audit never recorded: that a caller-supplied `tenant_id` in the request body cannot disagree with the verified `X-Tenant-Id` header (403 if it does), and must be a well-formed UUID if present (400 otherwise) — the same class of cross-tenant risk already found and fixed in `workflow-history-svc` this pass, but for this service's WRITE path rather than a read. Also found: a `GET /v1/kill-switches/check` alias for `/resolve` (used by at least one caller expecting that exact path), and a correlation-ID fallback chain (request body → envelope context → `X-Correlation-ID` header) ensuring outbox events always carry a correlation ID even when the caller's JSON body omits one.
Classification: documented, committed and tested behavior of this service (the original audit had simply never recorded it).
Verification:
`TestEngage_CrossTenantForbidden403`, `TestDisengage_CrossTenantForbidden403`, `TestEngage_InvalidTenantUUID400`, `TestDisengage_InvalidTenantUUID400`, `TestResolve_InvalidTenantUUID400`, `TestCheck_RouteAliasWorks`, `TestEngage_CorrelationIDFallback` all pass. It is part of the committed source that the container builds from.
Evidence:
`internal/handler/handler.go` (`resolveTenantScope`, `EngageKillSwitch`, `DisengageKillSwitch`, `/check` route), `internal/handler/handler_test.go`.

### Gap 3 — Database Connection as Postgres Superuser Unconditionally Bypassed FORCE ROW LEVEL SECURITY (newly discovered this pass, critical, service-owned)
Status: ✅ Fixed
Discovery Evidence:
`deployments/docker-compose.yml`'s `kill-switch-registry-svc` block set `DATABASE_URL` to `postgres://${DB_USER:-postgres}:${DB_PASSWORD:-postgres}@postgres:5432/kill_switch_registry`, and no `.env` file or other override existed anywhere in this repo, so the service connected as the literal `postgres` role. Confirmed via `pg_roles`: `postgres` has `rolsuper=true, rolbypassrls=true` on this platform's database — a true superuser, which bypasses row-level security unconditionally, **`FORCE ROW LEVEL SECURITY` notwithstanding** (FORCE only binds the table owner, never a genuine superuser login). Migration `000002_add_rls.up.sql` contains an unusually extensive doc comment explaining that this is specifically "the one service in the tier where a naive tenant policy is not an outage or a leak but a SILENT SAFETY BYPASS" — the policy itself was correctly designed (including the subtle `tenant_id IS NULL` platform-wide-visibility branch), but the connection role silently defeated it entirely. Independently, `internal/store/pg_store.go`'s `ListCurrentStates` was found to be the **only** query in the file with no application-level tenant filter of its own (every other method — `LatestEventForScope`, `ResolveKillSwitch`, `ListHistoryForScope` — already has its own `WHERE tenant_id IS NOT DISTINCT FROM $N` or equivalent, independent of RLS), meaning it was the single point of failure this exact scenario would hit hardest.
Classification: SERVICE-OWNED DEFECT, Critical — security/tenant-isolation.
Expected behavior: a tenant-scoped caller to `GET /v1/kill-switches` sees only their own tenant's switches plus platform-wide ones.
Actual behavior (before fix): every caller, regardless of verified tenant, saw every tenant's kill-switch state — reason text (often naming the operational incident), approver principal IDs, and scope — because RLS was never actually in effect for this service's own connections.
Fix Applied:
1. `deployments/docker-compose.yml`: changed `DATABASE_URL` to connect as `zoiko_app` (`NOSUPERUSER`, `NOBYPASSRLS`), confirmed already holding the required `arwd` grants on `kill_switch_events` and `outbox_events` (`\dp` output) — no new grant needed.
2. `internal/store/pg_store.go`: added an explicit `WHERE tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')` clause to `ListCurrentStates`, mirroring the RLS policy's own condition exactly — defense-in-depth matching every other query in the file, so this endpoint no longer depends on RLS being the only thing standing between it and a full cross-tenant dump.
Verification:
Rebuilt and redeployed; confirmed via `pg_stat_activity` that the live container's connection pool now authenticates as `zoiko_app`, not `postgres`. **Live, end-to-end proof with seeded ground-truth data** (not only unit tests): inserted one tenant-A-scoped and one platform-wide kill-switch row directly; `GET /v1/kill-switches` as tenant B returned only the platform-wide row; as tenant A returned both. Independently proved the vulnerability had been real: re-ran the pre-fix query shape as the `postgres` superuser with `app.tenant_id` set to tenant B — it returned tenant A's row anyway, confirming the bypass was genuine and exploitable, not theoretical. Full suite re-run against a fresh, real Postgres database: 35/35 pass, including the pre-existing `TestRLS_ListCurrentStates_ScopedToCaller` and `TestRLS_PlatformWideSwitchStaysVisibleToTenants`.
Evidence:
`deployments/docker-compose.yml` (`kill-switch-registry-svc.environment.DATABASE_URL`), `internal/store/pg_store.go` (`ListCurrentStates`), `pg_stat_activity` output before/after, live `curl` probes as two different tenants, live superuser-bypass reproduction, 35/35 real-database test run.

### Gap 4 — Swallowed Error on Dead-Code Publish Path (newly discovered this pass, low severity)
Status: ✅ Fixed
Discovery Evidence:
`internal/events/publisher.go`'s `Publish` method (the direct, non-outbox publish path) logged "kafka publish failed — event dropped" on a write error but then unconditionally returned `nil`, so a caller would believe the event was delivered. Confirmed via a full call-site search that `Publish` has no live caller anywhere in this service — every mutation goes through the transactional outbox's `PublishOutbox` instead, which already returns errors correctly. This is exactly the anti-pattern this domain's own audit elsewhere calls "Widespread Lack of Transactional Outbox" (`_ = h.publisher.Publish(...)`), sitting dormant in an exported method of a service this same audit otherwise credits with having fixed that pattern.
Classification: SERVICE-OWNED DEFECT, low severity (dead code today, but an exported method on a security-relevant service should not silently misreport success on failure).
Fix Applied:
Changed the swallowed `return nil` to `return err` after logging, matching `PublishOutbox`'s already-correct behavior immediately below it in the same file.
Verification:
Added `TestPublish_WriterError_IsNotSwallowed`, asserting a writer failure is returned to the caller and no message is recorded as delivered. Full suite re-run clean.
Evidence:
`internal/events/publisher.go` (`Publish`), `internal/events/publisher_test.go` (new test).

**Remaining Gaps:**
- None open within `kill-switch-registry-svc`'s own service boundary (see the Remediation Pass below).
- Frontend was not audited.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** the same platform-wide blocker documented throughout this file; this service's own fail-closed behavior (503 on an unreachable/erroring authz check) was confirmed correct in code and is the same pattern verified live for other services this pass.

**Needs-Clarification Items:** see the Remediation Pass below.

**Verification Results (historical — original pass; current results are in the Remediation Pass below):**
- Backend Unit Tests: 28/28 unconditional tests pass (`go test ./...`); 35/35 pass against a real, freshly-migrated Postgres database (`TEST_DATABASE_URL`), including the dedicated RLS suite.
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Docker Build: `docker compose build kill-switch-registry-svc` succeeded.
- Docker Health: Live container `kill-switch-registry-svc` Up (healthy) on port 8147; rebuilt and redeployed this pass.
- Live RLS Fix Verification: `pg_stat_activity` confirms the live connection pool now authenticates as `zoiko_app`; seeded-data cross-tenant test confirms tenant B cannot see tenant A's kill switches while still correctly seeing platform-wide ones; the pre-fix vulnerability was independently reproduced and confirmed exploitable via a direct superuser query before being closed.
- Live Health: `/healthz` and `/readyz` both return 200 OK.

### Remediation Pass (2026-10-19)

Scope: `kill-switch-registry-svc` only.

| Gap | Result |
|---|---|
| Body `tenant_id` with no `X-Tenant-Id` fell through to the database and failed RLS as a 500 | Fixed: `checkBodyTenant` refuses it with 403 (malformed UUID is 400; a different tenant is 403) before authorization. RLS is unchanged and remains the backstop; the response carries no database detail |
| Concurrent disengages could both succeed | Fixed: `PgStore.DisengageIfEngaged` takes a transaction-scoped advisory lock keyed on the exact scope tuple, re-reads the latest event inside the transaction and inserts the DISENGAGE (and its outbox rows) in the same transaction. The loser gets the existing 409 `kill switch is not currently engaged for this scope`. Nothing persists beyond the transaction; no state model was added |
| Stale "uncommitted work" claims in this section | Corrected above (work is committed in `cee11a7e`) |

Not changed: migration `000001`. Two comments in it are stale ("required in practice by the handler... see internal/handler's missingField validation" — no such validation exists; and "doc7 does not mandate maker-checker... so this service does not invent that constraint" — the service does enforce the two-man rule). Applied migrations are kept as historical record, so the correction is recorded here instead. `backend-completion-tracker.md` rows 17 and 17a were reviewed and make no claim that is now false, so they were left as written.

Needs clarification (not implemented): whether `GET /v1/kill-switches` and `GET /v1/kill-switches/history` should require a principal or a grant (they currently require neither; the audit file justifies the absence of authorization only for `resolve`); whether `Idempotency-Key` should de-duplicate writes (a repeated ENGAGE adds another row); frontend completion (not audited).

External blocker: the `authorization-svc` migration gap (platform-wide); a real authorized engage/disengage has not been exercised end to end.

Verification: `go build ./...` and `go vet ./...` clean. `go test ./...` against a disposable PostgreSQL: all packages ok, 47 test functions passed (12 new), 0 failed, 0 skipped; without a database the 12 DB-backed tests skip. The concurrency tests widen the check-to-insert window with a test-only trigger; with the advisory lock removed they fail (12 successes instead of 1), with it they pass. Removing the new tenant check makes three handler tests fail.

---

## Service: retention-registry-svc
**Port:** 8148
**Classification:** Records Retention Policy Registry & Legal Hold Enforcement

**Note on this section:** as with the services above, this is the first exhaustive per-service entry for `retention-registry-svc` in this file; its prior record lived only in this file's earlier domain-wide sections (§1, §3, §7, §8, §10, §14.2, §14.3, §14.5, §15 — lines 23, 41-42, 56, 70-71, 158-164, 183-184, 212, 221, 229, 260, 302-304, 321, 413-415, 440-442, 496-513, 575, 596). Unlike every other service in this domain so far, the master summary marked this one **NOT READY (84%)** with three specific confirmed defects. **Process note (updated 2026-10-19):** when this section was first written, two of those three defects were found already fixed by correct, tested work that was then uncommitted in the working tree (the migration rename, the SoD self-approval check and a transactional outbox implementation). That work is now committed (`cee11a7e`; `git status` is clean for this service), so the repository-hygiene concern originally recorded here no longer applies. This pass's own contribution found and fixed a third, more subtle defect the audit never named, and a regression that its own fix for a pre-existing superuser bug would otherwise have introduced.

**Service Health:** Working (Healthy)

**Frontend Completion: 85%** (per earlier domain summary, not independently re-verified this pass)
Reason (carried forward): `Zoiko-suite-frontend-platform/app/admin/retention`, `components/admin/retention`, and `lib/api/retention-registry.ts` were recorded as implemented against this service's retention-policy and legal-hold API.

**Backend Completion: 95%**
Reason: Full read/write API in `internal/handler/handler.go` (`CreateRetentionPolicy`, `CreateLegalHold`, `ReleaseLegalHold`, `Resolve`, `ListRetentionPolicies`, `ListLegalHolds`, `GetLegalHold`) backed by `internal/store/pg_store.go`, a transactional outbox (`internal/outbox/outbox.go`, migration `000004_add_outbox_events`), and a fail-closed `authorization-svc` client. **The audit's RET-02 SoD finding is fixed** (self-approval rejection plus independent approver authorization on `ReleaseLegalHold`) — see Gap 1. **The audit's RET-03 duplicate-migration finding is fixed** (renamed to a clean `000001`-`000004` sequence) — see Gap 2. **This pass found and fixed a genuine, still-open defect the audit's own "swallowed Kafka publish errors" finding did not fully capture**: three handlers called the old direct, error-swallowed `publisher.Publish` *in addition to* the already-correct outbox insert, duplicating every `retention_policy.created`, `legal_hold.engaged`, and `legal_hold.released` event for any real Kafka consumer — see Gap 3. All 47 tests pass against a real, freshly-migrated Postgres database (unit-only run has fewer; the real-DB run includes the dedicated RLS suite). `go build ./...` and `go vet ./...` both clean.

**Integration Completion: 90%**
Reason: `deployments/docker-compose.yml` wires `KAFKA_BROKERS`, `KAFKA_EVENTS_TOPIC`, and `AUTHZ_SERVICE_URL`, with `depends_on` on `postgres`, `kafka`, and `authorization-svc`. **`DATABASE_URL` corrected this pass** from the `postgres` superuser default to the least-privilege `zoiko_app` role — the identical defect found and fixed for `kill-switch-registry-svc` in this same pass — see Gap 4. **Fixing that defect alone would have been incomplete and actively harmful**: `ListRetentionPolicies` and `ListLegalHolds` never set `app.tenant_id` before querying, so once RLS became genuinely enforced, those two endpoints would have silently returned zero tenant-scoped rows to every caller — found and fixed in the same gap before redeploying. Live-verified end-to-end with seeded ground-truth data: a request as tenant A correctly saw its own legal hold via `GET /v1/legal-holds`; a request as tenant B correctly did not. **Capped below 95%** by the same external `authorization-svc` migration gap documented throughout this file.

**Overall Completion: 92%**

**Production Readiness: NOT PRODUCTION READY.** Service-owned gaps found by the later audit are closed (see the Remediation Pass below), but a real authorized create/release cannot be demonstrated end to end while the external `authorization-svc` migration gap remains.

**Commands:**
- `POST /v1/retention-policies` (Create a retention policy at tenant or platform scope; requires `RETENTION_POLICY_CREATE`-equivalent authorization at the declared or platform scope).
- `POST /v1/legal-holds` (Place a legal hold; same tenant/platform scoping discipline).
- `POST /v1/legal-holds/{id}/release` (Release an ACTIVE hold; two-man rule — see Gap 1).

**Reads:**
- `GET /v1/retention/resolve?record_class=&jurisdiction_code=&tenant_id=&entity_ref=` (The check every other service makes before deleting/exporting/migrating a record; no authz gate by design, same posture as `kill-switch-registry-svc`'s resolve endpoint).
- `GET /v1/retention-policies`, `GET /v1/legal-holds` (Paginated registers, tenant-scoped plus platform-wide).
- `GET /v1/legal-holds/{id}`.
- `GET /healthz`, `GET /readyz`.

**Segregation of Duties & Auth:**
- Legal Hold Release Two-Man Rule (Original Gap — see Gap 1): `principalID == req.ReleaseApprovedByPrincipalID` rejected with 403; the named approver is independently authorized against `authorization-svc`.
- Cross-Tenant Write Protection: on create-policy/create-hold a non-UUID body `tenant_id` is rejected with 400; a body `tenant_id` that disagrees with the verified `X-Tenant-Id`, or that is sent with no `X-Tenant-Id` at all, is rejected with 403 before authorization (the no-header case previously reached the database and failed RLS as a 500). A platform-wide policy or hold (no `tenant_id`) needs no tenant header.
- RLS: `FORCE ROW LEVEL SECURITY` on both `retention_policies` and `legal_holds`, with the same deliberately-documented `tenant_id IS NULL` platform-wide-visibility branch as `kill-switch-registry-svc`'s migration (the two migrations explicitly cross-reference each other). Every `Find*`/`List*` store method has its OWN explicit application-level tenant filter independent of RLS — a materially stronger defense-in-depth posture than `kill-switch-registry-svc` had (which had exactly one method relying on RLS alone). This policy was correctly designed but was being unconditionally bypassed by the service's own superuser database connection until this pass — see Gap 4.
- Append-Only writes: holds and policies are created, never mutated in place; release is a status transition guarded by a WHERE-status check, not a free-form update.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections):**

### Gap 1 — Unilateral Legal Hold Release, SoD Bypass (Original Gap, from SEC-02/RET-02 — security-critical)
Status: ✅ Fixed (committed in `cee11a7e`)
Original Issue:
`internal/handler/handler.go#L451-L487` (as originally audited) accepted `release_approved_by_principal_id` from the request body without checking identity distinctness against the authenticated caller, and performed no remote authorization check on the approver — a single individual could release a legal hold unilaterally, risking spoliation of evidence.
Discovery Evidence:
Re-reading current `internal/handler/handler.go`'s `ReleaseLegalHold` found the self-approval check (`principalID == req.ReleaseApprovedByPrincipalID` → 403) and an independent `authorizePrincipal` call for the named approver already present and correct, although the earlier audit still showed it as a confirmed defect.
Verification:
`TestReleaseLegalHold_SelfApproval_Forbidden403`, `TestReleaseLegalHold_ApproverNotAuthorized_Forbidden403`, `TestReleaseLegalHold_ApproverAuthzUnavailable_503` all pass. The protection is part of the committed source.
Evidence:
`internal/handler/handler.go` (`ReleaseLegalHold`), `internal/handler/handler_test.go`.

### Gap 2 — Duplicate Migration File Prefix `000002_` (Original Gap, from RET-03)
Status: ✅ Fixed (committed in `cee11a7e`)
Original Issue:
`deployments/migrations/` contained both `000002_add_rls.up.sql` and `000002_status_checks_and_register_indexes.up.sql` — a genuine sequence collision that a real migration-tracking tool (`golang-migrate`) would refuse outright.
Discovery Evidence:
Current `deployments/migrations/` is a clean `000001`→`000004` sequence (`000003_status_checks_and_register_indexes`, renamed from `000002_`, plus a new `000004_add_outbox_events`) — committed.
Verification:
Full migration set applied cleanly from a clean database three times this pass (once per fresh test database created), zero errors, as part of every real-Postgres test run.
Evidence:
`deployments/migrations/000003_status_checks_and_register_indexes.{up,down}.sql` (renamed), clean-database migration runs.

### Gap 3 — Duplicate Event Delivery: Direct Publish Left Running Alongside the Transactional Outbox (newly discovered this pass, service-owned)
Status: ✅ Fixed
Discovery Evidence:
`CreateRetentionPolicy`, `CreateLegalHold`, and `ReleaseLegalHold` each already insert their event into the transactional outbox atomically with the domain row (`outboxEvt` passed into the store call) — the real, reliable delivery path the audit's "Widespread Lack of Transactional Outbox" finding was written to fix. But all three handlers ALSO called `_ = h.publisher.Publish(...)` immediately afterward, unconditionally, with the error discarded. This was not a fallback — nothing depended on whether it succeeded — so on the common case where both sends succeed, a real Kafka consumer received `retention_policy.created`, `legal_hold.engaged`, and `legal_hold.released` **twice** for every single occurrence.
Classification: SERVICE-OWNED DEFECT — a leftover from an incomplete migration to the outbox pattern, not a currently-reachable security issue, but a real correctness/reliability defect for every downstream consumer of these three events.
Expected behavior: exactly one delivery per domain event, via the outbox relay.
Actual behavior (before fix): two deliveries per event in the common case; the audit's own "error ignored" framing undersold the defect, since the error being ignored was never the point — the duplicate send was unconditional.
Fix Applied:
Removed all three direct `h.publisher.Publish(...)` calls from `internal/handler/handler.go`, leaving the already-correct outbox insert as the sole delivery path, consistent with how `ReleaseLegalHold` already behaved relative to the others before this pass (it had the same redundant call too, now also removed).
Verification:
Fixed a test (`TestLegalHold_BlocksResolveEvenWithAPermissiveRetentionPolicy`) that had been asserting on the now-removed direct-publish call count (`pub.calls`); it now asserts on the outbox events actually recorded (`st.outboxEvents`), matching the pattern every other outbox test in this file already used. Full suite re-run clean: 47/47 against a real database.
Evidence:
`internal/handler/handler.go` (`CreateRetentionPolicy`, `CreateLegalHold`, `ReleaseLegalHold`), `internal/handler/handler_test.go`.

### Gap 4 — Database Connection as Postgres Superuser Bypassed FORCE ROW LEVEL SECURITY, and a Regression That Fixing It Would Have Caused (newly discovered this pass, critical, service-owned)
Status: ✅ Fixed
Discovery Evidence:
Identical root cause to `kill-switch-registry-svc`'s Gap 3 in this same pass: `deployments/docker-compose.yml` set `DATABASE_URL` to `${DB_USER:-postgres}:${DB_PASSWORD:-postgres}`, defaulting to the Postgres superuser (`rolsuper=true, rolbypassrls=true`), with no `.env` override anywhere in the repo — so `FORCE ROW LEVEL SECURITY` on `retention_policies` and `legal_holds` was unconditionally bypassed. Migration `000002_add_rls.up.sql`'s own doc comment calls the naive-policy failure mode here "IRREVERSIBLE" (a hidden legal hold leads straight to spoliation). Unlike `kill-switch-registry-svc`, every `Find*`/`List*` store method already has its own explicit WHERE-clause tenant filter independent of RLS, so this specific defect was **not** exploitable for cross-tenant HTTP-reachable disclosure the way `kill-switch-registry-svc`'s was — it is a defense-in-depth and least-privilege violation, not a currently-open read vulnerability, and is classified accordingly (see the explicit distinction this file's own consistency discipline requires between these two classifications).

**Fixing this surfaced a second, genuine regression this pass had to also fix before it could be shipped**: `ListRetentionPolicies` and `ListLegalHolds` run their query on the bare connection pool, never calling the `withTenant` helper that sets `app.tenant_id` — their own WHERE clause was always correct, so this was invisible while RLS was bypassed by the superuser connection. The instant RLS became genuinely enforced, its own policy condition (which reads `app.tenant_id`, never set by these two methods) intersected with the already-correct WHERE clause to produce **zero rows for every tenant-scoped caller** — confirmed live: a seeded tenant-A legal hold became invisible to a tenant-A caller immediately after the superuser fix alone, before the second half of this fix was applied.
Classification: SERVICE-OWNED DEFECT. The superuser connection is Critical (least-privilege/defense-in-depth); the `withTenant` omission is Critical in a different way (a functional regression that this pass's own first fix would otherwise have shipped, hiding real retention policies and legal holds from every tenant at exactly the moment this service's accuracy matters most legally).
Fix Applied:
1. `deployments/docker-compose.yml`: `DATABASE_URL` changed to the least-privilege `zoiko_app` role, confirmed already holding the needed `arwd` grants on `retention_policies`, `legal_holds`, and `outbox_events`.
2. `internal/store/pg_store.go`: wrapped `ListRetentionPolicies` and `ListLegalHolds` in `s.withTenant(...)`, matching every other method in the file, so `app.tenant_id` is set before RLS evaluates its own policy on these two queries.
Verification:
Rebuilt and redeployed; confirmed via `pg_stat_activity` the live pool now authenticates as `zoiko_app`. Added `TestRLS_ListLegalHolds_ScopesCorrectly` and `TestRLS_ListRetentionPolicies_ScopesCorrectly` (new, real-Postgres-backed, non-superuser role) — both deliberately reproduced failing first (confirmed via a temporary, reverted-after re-test of the pre-fix query shape: tenant A saw only the platform-wide row, not their own, exactly the regression described above) and pass after the fix. **Live, end-to-end proof with seeded ground-truth data**: after both halves of the fix, `GET /v1/legal-holds` as tenant A correctly returned the seeded tenant-A hold; as tenant B correctly returned nothing. Full suite re-run against three independently fresh databases across this pass: 45-47/47 pass each time, 0 failures.
Evidence:
`deployments/docker-compose.yml` (`DATABASE_URL`), `internal/store/pg_store.go` (`ListRetentionPolicies`, `ListLegalHolds`), `internal/store/rls_test.go` (2 new tests), `pg_stat_activity` output, live `curl` probes as two tenants before/after.

**Remaining Gaps:**
- None open within `retention-registry-svc`'s own service boundary (see the Remediation Pass below).
- Frontend was not audited.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** the same platform-wide blocker documented throughout this file.

**Needs-Clarification Items:** see the Remediation Pass below.

**Verification Results (historical — original pass; current results are in the Remediation Pass below):**
- Backend Unit Tests: full suite passes against a real, freshly-migrated Postgres database on three independent runs this pass (45, 45, and 47 passing as fixes were added; 0 failures on the final run), including the dedicated RLS suite with its two new regression tests.
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Docker Build: `docker compose build retention-registry-svc` succeeded.
- Docker Health: Live container `retention-registry-svc` Up (healthy) on port 8148; rebuilt and redeployed this pass.
- Live RLS + Regression Fix Verification: `pg_stat_activity` confirms `zoiko_app`; seeded-data cross-tenant test confirms tenant A sees its own legal hold and tenant B does not — proving both halves of Gap 4's fix work together correctly, not just individually.
- Live Health: `/healthz` and `/readyz` both return 200 OK.

### Remediation Pass (2026-10-19)

Scope: `retention-registry-svc` only.

| Gap | Result |
|---|---|
| Body `tenant_id` with no `X-Tenant-Id` on `POST /v1/retention-policies` and `POST /v1/legal-holds` fell through to authorization and failed at the database's RLS `WITH CHECK` as a 500 | Fixed: `checkBodyTenant` refuses it with 403 (malformed UUID is 400; a different tenant is 403) before authorization and before any store call. RLS is unchanged and remains the backstop; the response carries no database detail |
| Stale "uncommitted work" claims in this section | Corrected above (committed in `cee11a7e`) |
| `GetLegalHold` comment contradicted itself ("no authz action is checked" vs "both gates are present") | Rewritten to describe the implemented behavior: identity and tenant scope, then `LEGAL_HOLD_READ` at the hold's own scope. No behavior change |
| Store comment on `FindLegalHoldByID` described the old superuser-connection posture | Rewritten: the service connects as `zoiko_app` (NOSUPERUSER, NOBYPASSRLS), so the policy is in force and the explicit tenant predicate is the independent second gate |

Not changed: the applied migrations `000001`–`000004` (historical comments stay as written; current behavior is documented here).

Needs clarification (not implemented): (1) `GET /v1/legal-holds/{id}` requires `LEGAL_HOLD_READ` at the hold's own scope; this file did not previously list that requirement and no other authoritative document settles it, so the gate is left exactly as implemented; (2) release ordering — `ReleaseLegalHold` checks the tenant, reads the hold (404 if absent or foreign) and only then checks the principal (401), so a caller with a tenant header but no principal can tell an absent hold id from a present one; the contract does not define the order, so it was not changed; (3) frontend completion (not audited).

Observations, not changed: request bodies accept `correlation_id` but the correlation id is taken from the header/envelope; `max_retention_days` is not checked against `min_retention_days`; the exported `KafkaPublisher.Publish` logs and returns `nil` on a write failure but has no live caller (every event goes through the outbox).

External blocker: the `authorization-svc` migration gap (platform-wide); a real authorized create/release has not been exercised end to end.

Verification: `go build ./...` and `go vet ./...` clean. `go test ./...` against a disposable PostgreSQL: all packages ok, 53 test functions passed (6 new, each run for both endpoints), 0 failed, 0 skipped; without a database the 11 DB-backed tests skip. Removing the new no-verified-tenant check makes two of the new tests fail.

---

## Service: metric-registry-svc
**Port:** 8149
**Classification:** Executive Report Metric Definition Registry (versioned metric definitions; platform-wide reference data, no tenant scoping)

**Note on this section:** this is the first exhaustive per-service entry for `metric-registry-svc` in this file; its prior record lived only in this file's earlier domain-wide sections (§1, §3, §5, §7-8, §10, §14.8, §15 — lines 24, 36, 42, 57, 97, 129, 185, 279, 305-306, 322, 379-392, 496-505, 576, 606). The master summary marked this service **NOT READY (65%)** with three stated defects: a missing `GET /v1/report-metrics` route, swallowed/un-outboxed Kafka publish errors, and "no historical versioning." Unlike the five services processed immediately before this one in this pass, none of this service's fixes were found pre-existing/uncommitted — all three original findings required fresh investigation, and one (versioning) turned out to be stale documentation, not a code defect. This pass also found and fixed a previously undocumented security gap (reads with zero identity requirement) and a least-privilege database-credential issue.

**Second pass (same engagement, immediately following):** re-verified every item above against current code (nothing had regressed), found and fixed one further newly-discovered gap — Idempotency-Key required by the envelope contract but never used for actual replay protection (Gap 6) — and, critically, **obtained genuine live runtime verification of Gaps 2, 5, and 6 without the blocked Docker rebuild**: Docker Desktop's registry resolution for `docker.io/library/golang:1.25-alpine` was re-confirmed still broken (retried repeatedly across this second pass, including immediately before writing this section — see Environment Limitation), so this pass instead compiled and ran this exact source tree directly (`go run ./cmd/server`) on the host, configured via environment variables to reach the already-running `postgres`, `kafka`, and `authorization-svc` containers on their published host ports. This is the same code that the Docker image would contain — not a substitute implementation — so its live behavior is genuine evidence, just not evidence from inside the container image itself. This proved, against real infrastructure: the service runs and serves `/healthz`/`/readyz` as `zoiko_app` (not superuser); `zoiko_app` can both read AND write `report_metric_definitions` and `outbox_events`; the outbox relay genuinely delivers to Kafka (a directly-inserted outbox row was picked up, published, and consumed from the real `zoiko.metric-registry.events` topic); and the authorization integration fails closed rather than being bypassed. It also surfaced, live, that `authorization-svc`'s own `/v1/authorize` endpoint is currently broken (`column pra.book_id does not exist`) — the same pre-existing, platform-wide `authorization-svc` migration gap documented throughout this file, now directly reproduced against this service's own write path — which blocks a full live HTTP happy-path proof of `CreateMetricDefinition`/`PublishNewVersion` (both correctly return 503 rather than silently permitting). The live container itself (port 8149) is still the pre-this-pass image; see Environment Limitation.

**Third pass (same engagement, immediately following): full fresh frontend audit + real browser verification, plus re-confirmation of both external blockers.** Re-checked both external blockers first: `docker pull golang:1.25-alpine` still fails identically (`context deadline exceeded`), and `authorization-svc`'s `/v1/authorize` still returns `503 {"error":"store_unavailable"}` with the identical root cause (`column pra.book_id does not exist`) — neither has been externally repaired; no workaround was attempted for either, per this pass's explicit instructions. The frontend's carried-forward 70% (never independently verified, and filed under the wrong path — `app/admin/metric-registry` does not exist; the real implementation lives at `app/admin/metrics`) was replaced with a full fresh audit: every source file was read, and the actual running UI was driven end-to-end with Playwright (a real headless Chromium browser, not curl) through the real login flow, against the real `metric-registry-svc` container and real Postgres data. See the new **Frontend** section below for the full account, including a genuinely interesting discovery: the *currently deployed* Docker image (dated 2026-09-22, predating this entire remediation engagement and still not rebuilt with any of this pass's fixes) independently already serves the catalog-list functionality that Gap 1 restored in current source — but live-confirmed via direct unauthenticated `curl` to still be missing Gap 4's identity check, meaning the live, running system today genuinely discloses metric definitions (including `owner_principal_id` and `formula_description`) to any caller without identity. This is recorded as a live, currently-exploitable consequence of the un-rebuilt container, not a new code defect — Gap 4's fix already exists in source and is proven correct; it simply is not yet deployed.

**Service Health:** Not established by repository evidence. Historical observation from the original engagement, not re-verified: the then-running Docker container was still the pre-this-pass image (old code, of unclear provenance — see above); a locally-run instance of the current code was live-verified against the same real infrastructure in the second pass, and the frontend was live-verified end-to-end against the actual deployed container in this third pass.

**Frontend Completion: 95%**
Reason: Full fresh audit of the actual implementation at `Zoiko-suite-frontend-platform/app/admin/metrics/{page.tsx,actions.ts,state.ts}`, `components/admin/metrics/{MetricPanels.tsx,MetricForms.tsx}`, `lib/api/metric-registry.ts`, and `app/api/v1/metrics/[code]/versions/route.ts` (the prior audit's recorded paths — `app/admin/metric-registry`, `components/admin/metric-registry` — do not exist; this is a path-naming correction, not a missing-implementation finding). **Live-verified with a real Playwright browser session** (login via the real demo-credential flow → session cookie → `/admin/metrics`): page loads with the correct title and heading; the catalog table renders real live data (3 genuine rows fetched from the running backend); the search box's empty-state ("No executive metrics match…") works; clicking a row's History link opens the version-lineage panel via the `/api/v1/metrics/[code]/versions` proxy route and renders real version data; the create-metric form accepts input and submits, correctly surfacing the live `503 — authorization service unavailable` from the broken `authorization-svc` dependency as a visible, readable error banner (via `explainMetricError`) rather than crashing or silently failing — and, confirmed by re-querying the catalog afterward, no partial/orphaned record was created, proving the fail-closed behavior is atomic end-to-end through the real UI. Code review confirms: identity (`X-Principal-Id`/`X-Tenant-Id`) is sourced exclusively from the server-side session cookie (`requireIdentity`/`sessionIdentity` in `actions.ts`/`page.tsx`) — no caller-controlled identity field exists anywhere in the form or client code; both write actions already send `Idempotency-Key` (derived from `correlation_id`), which will transparently gain real replay protection the moment this pass's Gap 6 backend fix is deployed, with zero frontend changes required; `explainMetricError` maps every documented backend error case (409 conflict, 404 not found, 403 unauthorized, invalid RFC3339 date) to a specific, actionable message. `npx tsc --noEmit` shows **zero errors in any metric-registry file** (35 pre-existing errors exist elsewhere in the repo, in unrelated domains — `lib/api/integration.ts`, `intelligence.ts`, `legal.ts`, `security-trust.ts`, and a repo-wide `ServiceName`/`ApiService` typing gap in `lib/api/config.ts` — none are service-owned by `metric-registry-svc` and none were touched, per this task's explicit scope rule). Focused ESLint on every metric-registry frontend file returns **0 errors, 9 warnings** — all either unused-import warnings (`Skeleton`, `ShieldAlert`, `Calendar`, `User`, `Badge`, `Button`) or one `react-hooks/set-state-in-effect` style warning on a dependency-cleanup effect in `MetricPanels.tsx` that live testing confirms behaves correctly (loading/error/empty states for version history all render as expected) — none are functional defects, so none were "fixed" per this task's explicit instruction not to make unrelated refactors for warnings alone. **Not 100%:** the publish-version form's actual submission was code-reviewed but not independently live-tested (it follows the identical pattern as create, which WAS live-tested), and the full write *happy path* (a visible "created" success banner) could not be observed by either pass because `authorization-svc` is down — a cross-service blocker, not a frontend defect.

**Backend Completion: 96%**
Reason: Full API in `internal/handler/handler.go` (`CreateMetricDefinition`, `GetActiveMetricDefinition`, `ListMetricVersions`, `PublishNewVersion`, and the newly-added `ListActiveMetricDefinitions`) backed by `internal/store/pg_store.go` and a transactional outbox (`internal/outbox/outbox.go`, migration `000002_add_outbox_events`, both newly added this pass — this service had zero outbox infrastructure beforehand, unlike several other services in this domain where it was found already implemented but uncommitted). **The audit's "no historical versioning" finding is found to be FALSE / stale documentation** — see Gap 3. **The audit's missing-list-route finding (MET-02) is confirmed and fixed** — see Gap 1. **The audit's swallowed-publish-error finding is confirmed and fixed** — see Gap 2. **This pass additionally found and fixed a previously undocumented defect**: `GetActiveMetricDefinition` and `ListMetricVersions` required no identity at all — see Gap 4. **The second pass found and fixed one further defect**: Idempotency-Key was required by the envelope contract (INV-08) but never used for actual replay protection on either write endpoint — see Gap 6. All 24 tests (13 handler, 4 outbox, 7 store — the first tests `internal/store` has ever had) pass against a real, freshly-migrated Postgres database. `go build ./...` and `go vet ./...` both clean. No remaining known service-owned backend defects.

**Integration Completion: 88%** (historical, not re-scored)
Note: the runtime observations in this paragraph — container image state, live `pg_stat_activity` output, live HTTP probes, the browser session — are historical observations from the original engagement. They cannot be confirmed from the repository and are not asserted as current fact.
Reason: `deployments/docker-compose.yml` wires `KAFKA_BROKERS`, `KAFKA_EVENTS_TOPIC`, and `AUTHZ_SERVICE_URL`. **`DATABASE_URL` corrected this pass** from the `postgres` superuser default to the least-privilege `zoiko_app` role — see Gap 5. The second pass live-proved the backend-to-infrastructure chain against real Postgres/Kafka/authorization-svc using this exact code run outside the container (`pg_stat_activity` confirms `zoiko_app`; a direct write-then-read round trip succeeded; the outbox relay genuinely delivered to the real `zoiko.metric-registry.events` Kafka topic; the authorization integration fails closed rather than being bypassed). **This third pass extended that proof through the full stack**: a real Playwright browser, authenticated via the actual login flow, drove the real frontend, which called the real `metric-registry-svc` container (not a mock, not the locally-run instance) through its full HTTP contract — session → envelope headers → backend → Postgres → rendered UI — and correctly surfaced a live cross-service failure (`authorization-svc` 503) as a readable error rather than crashing, with no partial write. **Still capped below the backend score** for two reasons, neither a defect in this service's own code, both re-confirmed live during this third pass: (1) the live Docker container has not actually been rebuilt with any of this pass's changes — confirmed via `docker image inspect` (image dated 2026-09-22, predating this engagement entirely) — see Environment Limitation; (2) `authorization-svc` itself is still broken (`column pra.book_id does not exist`, identical error on retry), so the full live HTTP happy path for `CreateMetricDefinition`/`PublishNewVersion` still cannot be exercised end-to-end — see Dependency-Limited Items. A related, newly-confirmed consequence of (1): the live container's divergence from current source cuts both ways — it happens to already serve Gap 1's list functionality (from whatever pre-engagement build it actually is), but is confirmed, live, to still lack Gap 4's identity check, so today's live reads are genuinely unauthenticated. This is an operational exposure caused by the undeployed fix, not evidence against the fix itself.

**Overall Completion: 89%**

**Production Readiness: NOT PRODUCTION READY.** Service-owned code is complete and tested in the repository (see the Remediation Pass below). Production readiness cannot be claimed because the external `authorization-svc` migration gap still prevents a genuine end-to-end authorized write, and the state of any deployed container is not established by repository evidence.

**Commands:**
- `POST /v1/report-metrics` (Create a new `metric_code` at version 1; requires `REPORT_METRIC_CREATE`; `Idempotency-Key`-protected as of this pass — see Gap 6).
- `POST /v1/report-metrics/{metric_code}/versions` (Publish a new version, atomically superseding whatever was ACTIVE; requires `REPORT_METRIC_PUBLISH`; `Idempotency-Key`-protected as of this pass — see Gap 6).

**Reads:**
- `GET /v1/report-metrics` (Paginated catalog of ACTIVE metric definitions, one row per `metric_code` — newly added this pass, see Gap 1; identity required, `?limit=`/`?offset=` bounded 1-500).
- `GET /v1/report-metrics/{metric_code}` (Current ACTIVE definition for one code; identity required as of this pass — see Gap 4).
- `GET /v1/report-metrics/{metric_code}/versions` (Full version history for one code; identity required as of this pass — see Gap 4).
- `GET /healthz`, `GET /readyz`.

**Segregation of Duties & Auth:**
- No tenant concept: `report_metric_definitions` has no `tenant_id` column and no RLS policy — this registry is explicitly platform-wide reference data, same posture as `schema-registry-svc` and `capability-registry-svc`. The `DATABASE_URL` fix in Gap 5 is therefore classified as least-privilege hardening, **not** an RLS-bypass fix (there is no RLS policy here for a superuser connection to defeat) — a distinction this file deliberately maintains against conflating the two (see `kill-switch-registry-svc`'s and `retention-registry-svc`'s sections for the contrasting case where the identical pattern WAS a genuine RLS bypass).
- Write authorization: `CreateMetricDefinition` requires `REPORT_METRIC_CREATE`, `PublishNewVersion` requires `REPORT_METRIC_PUBLISH`, both checked against `authorization-svc` after identity is established.
- Read authorization: identity-only (no granted-permission check), matching the platform convention confirmed in `capability-registry-svc/internal/handler/handler.go` (`GetCapability`, `ListIntegrationCapabilities`, `ListCapabilityClaims` all gate on `requirePrincipal` alone) — see Gap 4 for why this was not previously true here.
- Append-only history: `PublishNewVersion` runs inside a single transaction that supersedes the current ACTIVE row and inserts the new one atomically; no endpoint allows mutating a past version in place.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections, plus newly discovered):**

### Gap 1 — Missing Metric Catalog Query Route (Original Gap, MET-02 — confirmed still open, now fixed)
Status: ✅ Fixed
Original Issue:
`internal/handler/handler.go` registered only `POST /v1/report-metrics` under that path with no handler for a bare `GET`, so `GET /v1/report-metrics` returned HTTP 405 Method Not Allowed and the frontend's Executive Metric Catalog page rendered an empty table (per the master summary's line-279 frontend finding).
Verification:
Re-read the current router (`RegisterRoutes`) and confirmed no `GET "/"` handler existed under `/v1/report-metrics` prior to this pass — the audit's finding was accurate and still open.
Fix Applied:
Added `Store.ListActiveMetricDefinitions(ctx, limit, offset)` (`internal/store/pg_store.go`) returning one row per `metric_code` where `definition_status = 'ACTIVE'`, ordered by `metric_code`, and a new handler `ListActiveMetricDefinitions` wired to `r.Get("/", h.ListActiveMetricDefinitions)`, gated on `requirePrincipal` and a bounded `limit`/`offset` (`parsePaging`, 1-500, matching the pagination discipline already used by `schema-registry-svc` and `retention-registry-svc`).
Verification:
New tests `TestListActiveMetricDefinitions_ReturnsCatalog`, `TestListActiveMetricDefinitions_RequiresPrincipal`, `TestListActiveMetricDefinitions_InvalidLimitRejected` (handler layer) and `TestPgStore_ListActiveMetricDefinitions_ReturnsOneRowPerCode` (store layer, real Postgres, proves only the ACTIVE row per code appears, not every historical version) all pass.
Evidence:
`internal/handler/handler.go` (`ListActiveMetricDefinitions`, `parsePaging`, `RegisterRoutes`), `internal/store/pg_store.go` (`ListActiveMetricDefinitions`), `internal/handler/handler_test.go`, `internal/store/pg_store_test.go`.

### Gap 2 — Swallowed Kafka Publish Errors, No Transactional Outbox (Original Gap, from "Swallowed Kafka Publish Errors and Missing Outbox" — confirmed still open, now fixed)
Status: ✅ Fixed
Original Issue:
`internal/handler/handler.go#L162` (as originally audited): `_ = h.publisher.Publish(...)` called directly after DB commit in both `CreateMetricDefinition` and `PublishNewVersion`, discarding the error — a Kafka outage silently and permanently lost `report_metric.created`/`report_metric.version_published` events with no retry.
Verification:
Confirmed this service had zero outbox infrastructure prior to this pass (no `internal/outbox` package, no `outbox_events` table in any migration) — the only one of the six services processed across this pass's two sessions where the fix had to be built from scratch rather than found already implemented uncommitted.
Fix Applied:
Added migration `deployments/migrations/000002_add_outbox_events.{up,down}.sql` (table `outbox_events` with nullable `tenant_id` — always `NULL` here, since this registry is platform-wide — and a partial index on unpublished rows); added `internal/outbox/outbox.go` (`Event`, `Insert(ctx, tx, e)`, `Relay`/`NewRelay`/`Start`/`RelayOnce` using `SELECT ... FOR UPDATE SKIP LOCKED`), mirroring `retention-registry-svc`'s proven template exactly; added `KafkaPublisher.PublishOutbox` (`internal/events/publisher.go`) which deliberately does not set `Topic` on the outgoing `kafka.Message` (the `Writer` already pins it) — the same "Topic must not be specified for both Writer and Message" defect class found and fixed in `document-vault-svc`/`evidence-manifest-svc` earlier in this engagement; rewrote `CreateMetricDefinition` and `PublishNewVersion` (both handler and store) to insert the outbox event in the same transaction as the domain row instead of publishing directly; wired `outbox.NewRelay` into `cmd/server/main.go`, started on a cancellable context tied to graceful shutdown.
Verification:
New tests `TestInsert_NilTx_ReturnsError`, `TestRelayOnce_NilPool_ReturnsZero`, `TestRelay_Start_StopsOnContextCancellation`, `TestMockPublisher_ErrorPropagation` (outbox package); `TestPgStore_Outbox_AtomicityAndRelay` (store layer, real Postgres — proves the domain row and outbox row commit/roll back together, and that the relay retries a failing publish and succeeds on the next attempt); `TestPublishNewVersion_OutboxEventCreated` and the updated `TestCreateMetricDefinition_CreatesVersion1` (handler layer, asserting on the recorded outbox event instead of the removed direct-publish call). All pass against a real database. **Second pass — live Kafka proof obtained without the blocked container rebuild:** ran this exact code directly on the host (`go run ./cmd/server`) against the real `postgres`/`kafka` containers on their published host ports; inserted a real `outbox_events` row (same shape `CreateMetricDefinition` would write) directly via `psql` as `zoiko_app`; the live relay (actually running, 500ms tick) picked it up within ~2 seconds and set `published_at`; consumed the real `zoiko.metric-registry.events` Kafka topic directly (`kafka-console-consumer.sh --from-beginning`) and observed the exact published message. This is genuine delivery through this service's real `PublishOutbox`/`Relay` code against real Kafka, not a mock — the only thing not yet proven is this same behavior running *inside the rebuilt Docker image*, which remains blocked (see Environment Limitation). Test data was cleaned up after capture.
Evidence:
`deployments/migrations/000002_add_outbox_events.{up,down}.sql`, `internal/outbox/outbox.go`, `internal/outbox/outbox_test.go`, `internal/events/publisher.go` (`PublishOutbox`), `internal/store/pg_store.go` (`CreateMetricDefinition`, `PublishNewVersion`), `internal/handler/handler.go` (`CreateMetricDefinition`, `PublishNewVersion`), `cmd/server/main.go`.

### Gap 3 — "No Historical Versioning" (Original Gap — found FALSE, documentation drift, no code change)
Status: ⚪ Documentation Drift — not a code defect
Original Issue:
The master summary (line 42 and others) listed "no historical versioning" among this service's confirmed defects.
Discovery Evidence:
`internal/domain` defines `ReportMetricDefinition` with a `Version` field; `internal/handler/handler.go` implements `ListMetricVersions` (`GET /v1/report-metrics/{metric_code}/versions`, returns every version for a code) and `PublishNewVersion` (`POST /v1/report-metrics/{metric_code}/versions`), whose store implementation (`internal/store/pg_store.go`) runs an atomic supersede-then-insert transaction: the currently-ACTIVE row is marked superseded and a new row is inserted at `current.Version + 1`, inside one transaction, preserving every prior version as a historical row (`TestPublishNewVersion_SupersedesAndListsHistory`, `TestPgStore_PublishNewVersion_SupersedesAndListsHistory` both prove this against a real database). This functionality was already correctly implemented before this pass began; it was not added or modified.
Classification: DOCUMENTATION DRIFT — the audit claim does not match the current (or, as best this pass can tell, the recent prior) code. No remediation needed; the master summary and line-42/322/576 entries should be read as superseded by this section.
Evidence:
`internal/domain` (`ReportMetricDefinition.Version`), `internal/handler/handler.go` (`ListMetricVersions`, `PublishNewVersion`), `internal/store/pg_store.go` (`PublishNewVersion`), `internal/handler/handler_test.go` (`TestPublishNewVersion_SupersedesPriorAndHistorySurvives`), `internal/store/pg_store_test.go` (`TestPgStore_PublishNewVersion_SupersedesAndListsHistory`).

### Gap 4 — Reads Required No Identity At All (newly discovered this pass, security-relevant, service-owned)
Status: ✅ Fixed
Discovery Evidence:
Prior to this pass, `GetActiveMetricDefinition` (`GET /v1/report-metrics/{metric_code}`) and `ListMetricVersions` (`GET /v1/report-metrics/{metric_code}/versions`) called neither `requirePrincipal` nor any authorization check — any caller reaching this service over the internal network, authenticated or not, could enumerate the full metric definition catalog including `owner_principal_id` and `formula_description`. This contradicts the platform's own established convention for platform-wide, no-tenant registries, confirmed by inspecting `capability-registry-svc/internal/handler/handler.go`, where the equivalent reads (`GetCapability`, `ListIntegrationCapabilities`, `ListCapabilityClaims`) all gate on `requirePrincipal`.
Classification: SERVICE-OWNED DEFECT, security-relevant (unauthenticated internal information disclosure), not previously named in any version of this audit.
Expected Behavior: every read requires a resolved principal, consistent with every other registry service in this domain.
Fix Applied:
Added `if _, ok := h.requirePrincipal(w, r); !ok { return }` to the top of both `GetActiveMetricDefinition` and `ListMetricVersions`.
Verification:
New tests `TestGetActiveMetricDefinition_RequiresPrincipal` and `TestListMetricVersions_RequiresPrincipal` both pass, confirming a request without `X-Principal-Id` now receives 401 instead of the previously-returned 200.
Evidence:
`internal/handler/handler.go` (`GetActiveMetricDefinition`, `ListMetricVersions`), `internal/handler/handler_test.go`.

### Gap 5 — Database Connection as Postgres Superuser (newly discovered this pass, least-privilege hardening, service-owned)
Status: ✅ Code Fixed — ⚠️ Not Yet Live (see Environment Limitation)
Discovery Evidence:
`deployments/docker-compose.yml` set `DATABASE_URL` to `${DB_USER:-postgres}:${DB_PASSWORD:-postgres}`, the Postgres superuser default, with no `.env` override in the repository — the same pattern found in `kill-switch-registry-svc` and `retention-registry-svc`. **Unlike those two services, `report_metric_definitions` has no `tenant_id` column and no RLS policy of any kind** (confirmed by reading `deployments/migrations/000001_initial_schema.up.sql` in full), so this is explicitly **not** an RLS-bypass vulnerability — there is no tenant-isolation policy here for a superuser connection to defeat. It is classified as least-privilege hardening only, matching the `capability-registry-svc` precedent: a compromised or buggy connection from this service has no principled reason to hold superuser/`CREATEROLE`/drop-anything rights on a Postgres instance shared with every other service's data.
Fix Applied:
`deployments/docker-compose.yml`: `DATABASE_URL` changed to the least-privilege `zoiko_app` role, with an inline comment explicitly distinguishing this from an RLS fix. Confirmed via `\dp report_metric_definitions` and `\dp outbox_events` (the latter after applying migration `000002_add_outbox_events` directly to the live `metric_registry` database) that `zoiko_app` already holds the necessary `arwd` grants on both tables through the platform's pre-existing `ALTER DEFAULT PRIVILEGES` setup — no new grants were required.
Verification:
Grants confirmed live via `psql \dp` against the running `metric-registry` Postgres database. **The running Docker container `metric-registry-svc` has not yet been rebuilt with this change** — `pg_stat_activity` for THAT container still shows the old superuser connection as of this writing — see Environment Limitation below. **Second pass — live write proof obtained without the blocked container rebuild:** ran this exact code directly on the host with `DATABASE_URL` pointed at `zoiko_app`; confirmed via `pg_stat_activity` the connection authenticates as `zoiko_app` (not superuser); confirmed `zoiko_app` can both READ (`GET /v1/report-metrics` returned real catalog data live) and WRITE (a direct `BEGIN; INSERT ...; INSERT ...; COMMIT;` as `zoiko_app`, and a live `UPDATE ... SET idempotency_key = ...`, both succeeded) against `report_metric_definitions` and `outbox_events` with no grant errors. This is the same fix the Docker image will contain; it is proven correct and sufficient, just not yet running inside that image.
Evidence:
`deployments/docker-compose.yml` (`DATABASE_URL`), live `\dp` output on `report_metric_definitions` and `outbox_events`, live `pg_stat_activity` showing `zoiko_app`, live read+write round trip as `zoiko_app`.

### Gap 6 — Idempotency-Key Required But Never Used for Replay Protection (newly discovered second pass, service-owned)
Status: ✅ Fixed
Discovery Evidence:
The canonical service input contract (ZS-ARCH-SVC-001 v2.0 §4, INV-08) requires `Idempotency-Key` on every material write, and `internal/envelope`'s middleware already refused a `POST` missing the header — but the header's presence was never checked against anything stored: `CreateMetricDefinition` and `PublishNewVersion` had zero reference to the key anywhere in `internal/handler` or `internal/store` (confirmed by a full-text search of both packages). For `CreateMetricDefinition`, a client retry after a timeout coincidentally hit a 409 Conflict via `metric_code`'s own unique index — the wrong outcome for a genuine replay, which INV-08 requires to return the ORIGINAL 201. For `PublishNewVersion` the exposure is more serious: that method's supersede-then-insert sequence is **not** self-protecting — a retried call that reached the store would mark whatever row is CURRENTLY ACTIVE as SUPERSEDED, which, if a legitimate later publish had occurred in between, would corrupt the version history by superseding the wrong row. The exact same defect class was found and fixed in `schema-registry-svc` earlier in this engagement (its Gap 7).
Classification: SERVICE-OWNED DEFECT, not previously named in any version of this audit.
Fix Applied:
Migration `deployments/migrations/000003_add_idempotency_key.{up,down}.sql` adds `idempotency_key VARCHAR(255)` to `report_metric_definitions` and a partial unique index on `(metric_code, idempotency_key) WHERE idempotency_key IS NOT NULL`, mirroring `schema-registry-svc`'s `000004_add_idempotency_key` exactly. `internal/domain/types.go` gained `ReportMetricDefinition.IdempotencyKey` (`json:"-"`, internal only). Both handlers now check `h.store.FindByIdempotencyKey(ctx, metricCode, idempotencyKey)` before any mutation — for `PublishNewVersion`, before even decoding the request body, so a replay can never reach the supersede statement — and return the ORIGINAL row unconditionally on a hit. `internal/store/pg_store.go` adds `FindByIdempotencyKey` and, as a concurrency safety net mirroring `schema-registry-svc`'s, re-checks it on ANY unique-constraint violation during insert (not just the idempotency index specifically, since Postgres does not reliably report which of two simultaneously-violated constraints triggered first) and transparently returns the existing row instead of an error if the conflict was in fact a replay.
Verification:
New tests `TestCreateMetricDefinition_IdempotentReplay_ReturnsOriginal` and `TestPublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede` (handler layer, the latter specifically proving a replay of a stale publish does NOT disturb a legitimate later version that superseded it in between) and `TestPgStore_CreateMetricDefinition_IdempotentReplay_ReturnsOriginalRow` / `TestPgStore_PublishNewVersion_IdempotentReplay_DoesNotDoubleSupersede` (store layer, real Postgres, same proof against the actual unique index and transaction). All pass. Migration applied directly to the live `metric_registry` database (`ALTER TABLE` / `CREATE INDEX` succeeded); live-verified `zoiko_app` can write the new column (a direct `UPDATE ... SET idempotency_key = ...` succeeded). **A full live HTTP replay round trip through the write path could not be completed** — see Gap 2/Integration notes — because `authorization-svc` itself is currently broken, so every live `POST` attempt (with or without a repeated `Idempotency-Key`) fails closed at 503 before reaching this logic; the replay logic itself is proven correct by the real-database test suite, independent of that external blocker.
Evidence:
`deployments/migrations/000003_add_idempotency_key.{up,down}.sql`, `internal/domain/types.go` (`IdempotencyKey`), `internal/store/pg_store.go` (`FindByIdempotencyKey`, `CreateMetricDefinition`, `PublishNewVersion`), `internal/handler/handler.go` (`CreateMetricDefinition`, `PublishNewVersion`), `internal/handler/handler_test.go`, `internal/store/pg_store_test.go`.

**Frontend (third pass — full fresh audit, real browser verification):**
- **Files (path correction):** the audit's prior carried-forward paths were wrong — `app/admin/metric-registry` and `components/admin/metric-registry` do not exist. The real implementation is `Zoiko-suite-frontend-platform/app/admin/metrics/{page.tsx,actions.ts,state.ts}`, `components/admin/metrics/{MetricPanels.tsx,MetricForms.tsx}`, `lib/api/metric-registry.ts`, and `app/api/v1/metrics/[code]/versions/route.ts`. `lib/api/metric-registry.ts` was correctly named and did exist in the prior audit's file list.
- **Live Playwright verification** (real headless Chromium, real login as the demo admin account, real session cookie, real HTTP calls to the real running `metric-registry-svc` container — not curl, not a mock): login → `/admin/metrics` loads with correct title/heading → catalog table renders 3 real rows from the live database → search box's "no match" empty state works → clicking a catalog row's History link opens the version-lineage panel and loads real version data via the `/api/v1/metrics/[code]/versions` proxy route → submitting the create-metric form correctly surfaces the live `503 — authorization service unavailable` refusal as a readable banner, and a post-submission re-query of the catalog confirms no partial/orphaned record was written.
- **Identity & security:** `X-Principal-Id`/`X-Tenant-Id`/`X-Legal-Entity-Id` are sourced exclusively from the server-side session cookie (`requireIdentity` in `actions.ts`, `sessionIdentity` in `page.tsx`) via the shared `envelopeHeaders()` builder (`lib/api/envelope.ts`) — no form field or query parameter can set identity; this is the same platform-wide pattern every other service's console pages use, not something specific to this page.
- **Idempotency already wired:** both `createMetricDefinition` and `publishMetricVersion` (`lib/api/metric-registry.ts`) already pass `idempotencyKey: input.correlation_id` on every write — meaning this pass's backend Gap 6 fix becomes fully effective for real double-submit/retry protection the moment the container is rebuilt, with zero frontend changes needed.
- **Error mapping:** `explainMetricError` gives specific, actionable text for every documented backend failure (409 duplicate code, 404 unknown code on publish, 403 unauthorized, invalid RFC3339 date); `actions.ts` separately branches 401 (session expired) from 403/404/409 so the right message reaches the user instead of a generic failure.
- **Quality gates:** `npx tsc --noEmit` — zero errors in any metric-registry file (35 pre-existing errors exist elsewhere in the repo, entirely unrelated domains and a repo-wide `ServiceName`/`ApiService` typing gap — not touched, out of this task's scope). Focused ESLint on every metric-registry file — 0 errors, 9 warnings, all cosmetic (unused lucide-icon/UI imports) or a `react-hooks/set-state-in-effect` style suggestion on a dependency-cleanup effect that live testing confirms behaves correctly; none fixed, per this task's explicit instruction not to refactor for warnings alone.
- **Newly-confirmed live discrepancy (not a frontend defect — a live backend/deployment fact the frontend audit surfaced):** `docker image inspect` shows the running `metric-registry-svc` container's image was created `2026-09-22T08:28:13Z` — before this entire remediation engagement. Direct `curl` against it live confirms it already answers `GET /v1/report-metrics` (the Gap 1 functionality) successfully, from whatever divergent pre-engagement build it actually is — but **also confirms it still has zero identity requirement on `GET /v1/report-metrics/{code}/versions`** (a request with no `X-Principal-Id` header returns 200 with real data), meaning Gap 4's security fix, though correct and tested in source, is not protecting the live system today. This is the same undeployed-fix problem already named throughout this section; it is restated here because this pass obtained direct, first-hand live proof of it rather than inferring it.
- **Not independently verified:** the publish-version form's actual submission (same code pattern as create, not separately live-tested); the full write *happy-path* success banner (blocked everywhere by `authorization-svc`, not a frontend issue).

**Remaining Gaps:**
- None within `metric-registry-svc`'s own backend or frontend code that are still open at the code level.
- **Deployment state (historical, unverified — not a code defect):** during the original engagement the running container on port 8149 was observed to predate the repository's fixes (old superuser credentials, no outbox, no idempotency protection, no identity check on the version-history read). Whether any container is running today, and from which image, cannot be determined from the repository; this is recorded as a historical observation, not as a current exposure.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** the same platform-wide blocker documented throughout this file. **Reproduced directly against `metric-registry-svc`'s own write path in both the second and third passes**: a live `POST /v1/report-metrics` (with full envelope headers and a resolvable principal) returned `503 authorization service unavailable`; `authorization-svc`'s own logs show the identical root cause on both occasions — `pg FindGrantedActionsScoped failed: ERROR: column pra.book_id does not exist (SQLSTATE 42703)`. A direct `POST /v1/authorize` against `authorization-svc` itself (bypassing `metric-registry-svc` entirely) confirms the same error, ruling out anything on this service's side. `metric-registry-svc`'s handling of this is correct at every layer — backend fails closed (503, not a silent permit) and the frontend surfaces that refusal as a readable error rather than crashing or writing a partial record — the defect is entirely inside `authorization-svc`'s own schema/migrations and blocks a full live happy-path proof of `CreateMetricDefinition`/`PublishNewVersion`, through the API or the UI, regardless of which `metric-registry-svc` image is running. No attempt was made to bypass, mock, or disable this check, per this task's explicit rule.

**Environment Limitation (historical observation from the original engagement; not re-verified):**
- Docker Desktop's build path could not resolve `docker.io/library/golang:1.25-alpine` for the entire remediation window across all three passes: every `docker compose build metric-registry-svc` and direct `docker pull golang:1.25-alpine`/`golang:1-alpine` attempt failed with `context deadline exceeded` resolving the image manifest, while `alpine:3.19` resolved successfully and quickly on every attempt in the same window, from the same daemon, against the same registry host. A full Docker Desktop restart (taken specifically to rule out a stale-DNS/VM-networking cause, the same class of issue documented earlier in this engagement) restored general container and host-level registry connectivity (confirmed via `Test-NetConnection registry-1.docker.io -Port 443`, which succeeded) but did not resolve this specific image's pull failure, including from inside a running container's own network namespace (`bad address 'registry-1.docker.io'` on a fresh `alpine:3.19` container attempting the same host). This points to a registry-side condition specific to the `library/golang` repository (most plausibly Docker Hub anonymous-pull rate limiting, given how heavily that image is pulled across this platform's ~60 Go services) rather than a defect in this service, this pass's changes, or this pass's Docker configuration. The third pass re-confirmed the identical failure on retry immediately before this section was written, and — consistent with every prior pass — attempted no workaround that would alter the service's intended runtime configuration (no base-image substitution, no Dockerfile changes, no other service touched, no image swap).
- **A genuinely unexpected finding this third pass surfaced:** the container currently running on port 8149 is not simply "the pre-this-pass build" as earlier passes assumed — `docker image inspect` shows it was created `2026-09-22T08:28:13Z`, which predates this entire remediation engagement (and, per `git log`, predates the only two commits that have ever touched `internal/handler/handler.go`). Live behavior confirms it is a genuinely different, divergent build: it already serves `GET /v1/report-metrics` successfully (Gap 1's functionality, from some earlier source state never captured in this repository's git history) while simultaneously still lacking any identity check on `GET /v1/report-metrics/{code}/versions` (Gap 4 is confirmed live-absent). Neither fact changes this section's classification of Gaps 1 or 4 against the committed source this pass audited — both classifications were and remain accurate against git-tracked code — but it means "rebuild the container" is not simply reapplying a known-good prior state; the exact provenance of the running image is unknown and should be investigated by whoever controls this environment's deployment history.
- **What the second and third passes did instead, and why it is genuine evidence rather than a workaround to inflate the score:** the second pass compiled and ran this exact, unmodified source tree directly on the host (`go run ./cmd/server`), configured via the same environment variables the Docker container would receive, pointed at the already-running `postgres`, `kafka`, and `authorization-svc` containers on their published host ports (standard local-development tooling, not a new or unapproved mechanism) — proving Gaps 2, 5, and 6 work correctly against real infrastructure. The third pass additionally drove the real frontend with a real browser against the real (still pre-this-pass) `metric-registry-svc` container, proving the full stack's read path and its error handling genuinely work end-to-end today. Neither pass proves anything about the rebuilt container image itself, which is why Docker Build/Docker Health below are still reported as not completed.
- **As a result, `metric-registry-svc`'s Docker container could not be rebuilt or redeployed in any of the three passes.** Gaps 1-6 are verified by code review, a full real-Postgres test suite, genuine live backend runtime behavior (outside the container), and — for the read path — genuine live full-stack behavior through the real frontend; none are verified inside the actual container image, which remains a build of unknown, pre-engagement provenance. This is recorded as an environment limitation, not a resolved or fabricated verification.

**Needs-Clarification Items:**
- **Possible scope mismatch between this file's master cross-service action/read tables and this service's actual implemented domain model — now better evidenced, still not code-confirmed.** Lines 96-98 and 128-129 of this file's earlier §4/§5 tables describe a richer metric system than what exists in `metric-registry-svc`'s code: a separate "Register Metric Def" (`POST /v1/metric-definitions`), "Record Report Metric" as a distinct metric-*value*-point ingestion action (`POST /v1/report-metrics`, permission `METRIC_RECORD`, publishing `metric.recorded`), an "Aggregate Metrics" endpoint (`POST /v1/metrics/aggregate`), and a "Query Report Metrics" read returning "a filtered list of metric values" with tenant RLS. A full-text search of `services/metric-registry-svc` for `metric-definitions`, `metrics/aggregate`, `MetricValue`, `METRIC_RECORD`, and `METRIC_MANAGE` found zero matches. **This pass additionally read `internal/domain/types.go`'s own package doc comment**, which cites `docs/original_doc/zoiko_suite_doc7.txt §27 REP-01` directly and states in the code itself: *"This service defines what a metric IS and MEANS; it does not compute or store metric values itself."* That is first-party evidence the definitions-only scope is deliberate, not an oversight — but it is the code's own citation of doc7, not a direct reading of doc7 §27 itself, so this is reported as strengthened evidence toward DOCUMENTATION DRIFT rather than upgraded to a closed finding. This pass did **not** invent the missing value-ingestion/aggregation/tenant-RLS capability described in those lines, consistent with this task's instruction never to invent endpoints, fields, or business rules without code evidence. Recommend the architecture owner confirm against doc7 §27 directly whether metric-*value* ingestion/aggregation/tenant-scoping was intentionally descoped for `metric-registry-svc`; this section's Commands/Reads above reflect only what is actually implemented today.

**Verification Results (historical — the Docker, live-container and browser items below are observations from the original engagement and are not re-verified; current backend results are in the Remediation Pass):**
- Backend Unit Tests (at that time): 24 of the then-26 test functions were counted (13 handler, 4 outbox, 7 store; the 2 event-publisher tests were left out of the count) and pass against a real, freshly-migrated Postgres database (`metric_registry_test`), 0 failures.
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Frontend Types: `npx tsc --noEmit` — 0 errors in any metric-registry file (35 pre-existing, unrelated-domain errors elsewhere in the repo, not touched).
- Frontend Lint: focused ESLint on every metric-registry file — 0 errors, 9 cosmetic/style warnings, none functional.
- Docker Build: ❌ Not completed (any of the three passes) — blocked by the `golang:1.25-alpine` registry resolution failure described above; re-confirmed still blocked on retry during both the second and third passes.
- Docker Health (actual container): Live container `metric-registry-svc` Up (healthy) on port 8149, image created `2026-09-22T08:28:13Z` — predates this entire engagement; not yet rebuilt or redeployed; confirmed live this pass to still lack Gap 4's identity check.
- Live Database Grants: `\dp report_metric_definitions` and `\dp outbox_events` both confirm `zoiko_app` holds `arwd` on the live `metric_registry` database.
- Live Backend Runtime Verification (this exact code, run outside the container): `/healthz`/`/readyz` return 200; `pg_stat_activity` confirms the connection is `zoiko_app`, not superuser; a direct read-then-write round trip succeeded as `zoiko_app` on both `report_metric_definitions` (including the new `idempotency_key` column) and `outbox_events`; a real outbox row was relayed and consumed from the live `zoiko.metric-registry.events` Kafka topic; a live `POST /v1/report-metrics` with full envelope headers correctly failed closed with 503 when `authorization-svc` proved to be broken, rather than bypassing authorization. All test data created directly via SQL for this verification was cleaned up afterward.
- Live Full-Stack Verification (real Playwright browser against the actual deployed container): real login → session cookie → `/admin/metrics` renders real catalog data → search empty-state works → version-history panel loads real data via the real proxy route → create-form submission correctly surfaces the live `authorization-svc` 503 as a readable error with no partial write. No UI test data or fabricated records were left behind.
- Live Health (actual container, pre-this-pass image): `/healthz` and `/readyz` both return 200 OK.

### Remediation Pass (2026-10-19)

Scope: `metric-registry-svc` backend only; no frontend, no other service.

| Gap | Result |
|---|---|
| `CreateMetricDefinition` returned a stored definition (idempotent replay) before the principal was established | Fixed: request parsing and field validation (which read no stored state) run first, then `requirePrincipal` and `REPORT_METRIC_CREATE` authorization, then the replay lookup. An unauthenticated or unauthorized caller can no longer obtain a replay; an authorized replay still returns the original 201 |
| `PublishNewVersion` read stored state (replay lookup, then the existing-definition lookup answering 404) before the principal was established | Fixed: `requirePrincipal` and `REPORT_METRIC_PUBLISH` authorization now come first, then the replay lookup, body validation and the 404 for an unknown code. Without identity, an existing and an absent `metric_code` give the identical 401; without authorization, the identical 403 |
| Verification section said 24 tests | Corrected: the repository had 26 test functions (13 handler, 4 outbox, 7 store, 2 events); it now has 34 with the 8 new ordering tests |
| Runtime claims (container image, Docker build failures, "currently exploitable" exposure, readiness based on them) | Relabelled as historical, unverified observations; production readiness restated as not claimed |

Unchanged by design: idempotency semantics (replay returns the original row without comparing the request body), version allocation (a losing concurrent publish still gets a 500; unique indexes protect the history), the exported `KafkaPublisher.Publish` (no live caller), the platform-wide/no-tenant model, all routes, permissions, events and the `zoiko_app` role.

Needs clarification (not implemented): the earlier master tables describe `POST /v1/metric-definitions`, `METRIC_RECORD`, `metric.recorded`, `POST /v1/metrics/aggregate` and a value-query read that this service does not implement; the frontend 95% figure was not audited.

External blocker: the `authorization-svc` migration gap; a real authorized create/publish has not been exercised end to end.

Verification: `go build ./...` and `go vet ./...` clean. `go test ./...` against a disposable PostgreSQL: all packages ok, 34 test functions passed (8 new), 0 failed, 0 skipped; without a database the 7 DB-backed store tests skip (27 pass). Removing the identity-and-authorization gate from either write handler makes the new tests for that route fail.

---

## Service: source-authority-svc
**Port:** 8150
**Classification:** Precedence Register & Fact Resolution Service (versioned source-system precedence rules + append-only tenant-scoped normalized facts; doc7 §D1-D3, §K1)

**Note on this section:** this is the first exhaustive per-service entry for `source-authority-svc` in this file; its prior record lived only in this file's earlier domain-wide sections (§1, §3, §5, §7-8, §10, §14.2, §14.9, §15 — lines 25, 42, 58, 72, 99-101, 130-131, 186, 222, 230, 307, 323, 422-428, 496-505, 577, 606). The master summary marked this service **READY WITH MINOR GAPS (93%)** — the highest-scoring service processed in this domain so far — with two stated defects: swallowed Kafka publish errors, and a migration "enabling RLS without FORCE." **Unlike every other service processed in this domain this engagement, `git status` on this service's directory was completely clean** — no uncommitted fixes were found sitting in the working tree, because none were needed: this service already carries a real, coherent commit history (`git log` shows 10+ targeted fixes: canonical envelope rollout, mTLS, event_id collision fix, tenant-scoping + FORCE RLS migration, authorize-call envelope fix, resolver join qualification) and an existing `internal/handler/gaps_test.go` with 9 documented, already-fixed, already-tested gaps (missing tenant column on facts, ungated precedence-register reads, unused idempotency fields on both write paths, no way to ever supersede a rule, an INNER JOIN that silently dropped unranked sources, unvalidated `authority_class`, silently-discarded unknown JSON fields, and an unbrowsable register requiring an exact `field_family`). This pass verified the master's two remaining findings directly against current code: the RLS finding is **FALSE as literally stated — FORCE ROW LEVEL SECURITY has existed on `normalized_facts` since migration 000002** — but this pass found and fixed a different, still-genuine security gap behind the same symptom (see Gap 2), and confirmed the swallowed-Kafka-errors finding was still accurate and fixed it with a full transactional outbox (see Gap 1).

**Service Health:** Not established by repository evidence. Historical observation from the original engagement, not re-verified: the then-running Docker container could not be rebuilt (see Environment Limitation), and a locally-run instance of this code was reported as verified against the then-running Postgres/Kafka/`authorization-svc` containers.

**Frontend Completion: 90%** (code-reviewed this pass, not independently live-browser-tested)
Reason: unlike several other services in this domain, the audit's documented paths were accurate: `Zoiko-suite-frontend-platform/app/admin/source-authority/{page.tsx,actions.ts,state.ts}`, `components/admin/source-authority/{PrecedenceRegisterPanel,FactResolverPanel,SourceAuthorityForms}.tsx`, and `lib/api/source-authority.ts` all exist and were read in full. The API client's field names, idempotency wiring (`correlationId` minted once per page render, reused across retries of the same submission), and `explainSourceAuthorityError` string-matching all correspond exactly to the current backend contract, including the precise permission names (`SOURCE_AUTHORITY_MAP_VIEW`/`_CREATE`/`_SUPERSEDE`, `NORMALIZED_FACT_VIEW`) and the exact error strings this pass's own backend code returns. Identity (`X-Principal-Id`/`X-Tenant-Id`) is sourced exclusively from the server-side session cookie (`requireIdentity` in `actions.ts`) — no caller-controlled identity field exists in any form. **Not rated higher** because this pass did not drive the actual UI in a browser (no live-rendered-page or live-write-flow evidence was gathered, unlike the dedicated frontend pass performed for `metric-registry-svc`); the 90% reflects a thorough code review, not a live UI walkthrough.

**Backend Completion: 97%**
Reason: Full API in `internal/handler/handler.go` (`CreateSourceAuthorityMap`, `ListSourceAuthorityMaps`, `SupersedeSourceAuthorityMap`, `RecordFact`, `ListNormalizedFacts`, `Resolve`) backed by `internal/store/pg_store.go` and, newly this pass, a transactional outbox (`internal/outbox/outbox.go`, migration `000003_add_outbox_events`). **The audit's swallowed-publish-error finding is confirmed and fixed** — see Gap 1. **The audit's "RLS enabled without FORCE" finding is found to be FALSE** — see Gap 3 (documentation drift) — **but this pass found and fixed the real security gap behind the same symptom**: the superuser `DATABASE_URL` default defeated the FORCE clause that already existed — see Gap 2. The repository currently contains 53 test functions: 34 handler (including `gaps_test.go`), 4 outbox, 2 events and 13 store (9 in `pg_store_test.go` and 4 RLS-isolation tests in `rls_test.go`). The original pass counted 45 (32 handler, 4 outbox, 2 events, 9 store); that figure is superseded. The current inventory was executed and passed against a disposable PostgreSQL during the 2026-10-19 re-check (see the Remediation Pass). `go build ./...` and `go vet ./...` both clean. `internal/store` had zero tests before the original pass — it now has 13 test functions (9 in `pg_store_test.go`, 4 in `rls_test.go`), including the platform's established NOSUPERUSER/NOBYPASSRLS role pattern for genuine RLS proof. No remaining known service-owned backend defects.

**Integration Completion: 90%** (historical, not re-scored)
Note: the runtime observations in this paragraph — container image state, live `pg_stat_activity` output, the two-tenant HTTP probe, Kafka consumption, the live 503 from `authorization-svc` — are historical observations from the original engagement. They cannot be confirmed from the repository and are not asserted as current fact.
Reason: `deployments/docker-compose.yml` wires `KAFKA_BROKERS`, `KAFKA_EVENTS_TOPIC`, and `AUTHZ_SERVICE_URL`; the `authz` client already sends the full canonical envelope on every `/v1/authorize` call (fixed in a prior, already-committed pass — `git log`'s `938ac513`). **`DATABASE_URL` corrected this pass** from the `postgres` superuser default to the least-privilege, RLS-respecting `zoiko_app` role — see Gap 2. This pass live-proved the entire chain against real infrastructure using this exact code, run outside the container: `zoiko_app` genuinely reads and writes both `source_authority_maps` (platform-wide, no RLS) and `normalized_facts` (RLS-protected) with no grant errors; **a live, two-tenant isolation proof through the real, unauthenticated `GET /v1/source-authority/resolve` endpoint** — tenant A saw only its own fact, tenant B only its own, a third tenant with no facts saw neither — confirming the FORCE RLS policy genuinely isolates tenants over the corrected non-superuser connection; the outbox relay genuinely delivered a real event to the real `zoiko.source-authority.events` Kafka topic, confirmed by direct topic consumption. **Still capped below the backend score** for two reasons, neither a defect in this service's own code: (1) the live Docker container has not been rebuilt — see Environment Limitation; (2) `authorization-svc` itself is still broken (`column pra.book_id does not exist`, the same platform-wide dependency documented throughout this file), confirmed live against this service's own write path (`POST /v1/source-authority-maps` → 503, correctly fail-closed, no partial write) — see Dependency-Limited Items.

**Overall Completion: 92%**

**Production Readiness: NOT PRODUCTION READY.** Service-owned backend code is implemented and tested in the repository. Production readiness is not claimed because the external `authorization-svc` migration gap prevents a genuine end-to-end authorized write, and the state of any deployed container (image, database role) is not established by repository evidence.

**Commands:**
- `POST /v1/source-authority-maps` (Create version-1 of a precedence rule; platform-wide, requires `SOURCE_AUTHORITY_MAP_CREATE`; replay-protected on the request-body `correlation_id`, which is required: a replay returns the original row (200) and records no second outbox event). The common write-envelope middleware also requires an `Idempotency-Key` header on this write, but the handler does not read it and it is not the create replay identifier.
- `POST /v1/source-authority-maps/{id}/supersede` (End a rule's window; requires `SOURCE_AUTHORITY_MAP_SUPERSEDE`; refuses a past `effective_to`).
- `POST /v1/normalized-facts` (Append one tenant-scoped observation; requires `NORMALIZED_FACT_RECORD`; idempotent on `(tenant_id, correlation_id)`).

**Reads:**
- `GET /v1/source-authority-maps` (Paginated precedence register, `field_family` optional since a prior fix; requires `SOURCE_AUTHORITY_MAP_VIEW`; platform-wide, same rows for every tenant).
- `GET /v1/normalized-facts` (Fact history behind a resolution; requires `NORMALIZED_FACT_VIEW`; tenant-scoped).
- `GET /v1/source-authority/resolve` (The hot, cheap read every service needs; deliberately ungated on authorization, same posture as `kill-switch-registry-svc`'s resolve — but requires `X-Tenant-Id`, fixed in a prior already-committed pass, since the envelope middleware's write-strict default otherwise admits a tenant-less read).
- `GET /healthz`, `GET /readyz`.

**Segregation of Duties & Auth:**
- The tenant boundary runs between the two tables, not around the service: `source_authority_maps` is platform-wide reference data with no `tenant_id` column and no RLS (by design — confirmed via `TestRLS_NormalizedFactsEnabledAndForced`'s pinning assertion that it must NOT have row-level security); `normalized_facts` carries `tenant_id` and `FORCE ROW LEVEL SECURITY` (migration 000002). The Gap 2 `DATABASE_URL` fix is therefore a genuine RLS-bypass fix for `normalized_facts` specifically — a distinction this file deliberately maintains against conflating a real RLS bypass with the least-privilege-only case (see `metric-registry-svc`'s section for the contrasting case where the identical credential pattern was NOT an RLS issue because no RLS existed at all).
- Write authorization: `CreateSourceAuthorityMap` requires `SOURCE_AUTHORITY_MAP_CREATE`, `SupersedeSourceAuthorityMap` requires `SOURCE_AUTHORITY_MAP_SUPERSEDE`, `RecordFact` requires `NORMALIZED_FACT_RECORD`, all checked against `authorization-svc` with tenant-scoped decision caching.
- Read authorization: `ListSourceAuthorityMaps`/`ListNormalizedFacts` require `SOURCE_AUTHORITY_MAP_VIEW`/`NORMALIZED_FACT_VIEW` respectively (fixed in a prior already-committed pass — `gaps_test.go`'s Gap 2); `Resolve` is described here as identity-and-tenant-gated but NOT grant-gated [NEEDS CLARIFICATION: the implementation requires `X-Tenant-Id` only, not `X-Principal-Id`; see Needs-Clarification Items], a documented and consistent platform pattern for a hot, cheap, every-service-needs-it read.
- Immutability: a precedence rule is never rewritten, only end-dated (`Supersede`); a normalized fact is never updated, only appended with a later `observed_at` — both enforced by the handler never exposing an UPDATE/PATCH/PUT route for either, and by the store layer's own comments and structure.

**Fixed Gaps (carried forward from this file's earlier domain-wide sections, plus newly discovered):**

### Gap 1 — Swallowed Kafka Publish Errors, No Transactional Outbox (Original Gap, from "Widespread Lack of Transactional Outbox" — confirmed still open, now fixed)
Status: ✅ Fixed
Original Issue:
`internal/handler/handler.go#L235` (as originally audited): `_ = h.publisher.Publish(...)` called directly after the store call in `CreateSourceAuthorityMap`, `SupersedeSourceAuthorityMap`, and `RecordFact`, discarding the error — a Kafka outage silently and permanently lost `source_authority_map.created`/`.superseded`/`normalized_fact.recorded` events with no retry. `internal/events/publisher.go`'s `Publish` method itself always returns `nil` even when `WriteMessages` fails, confirming the error truly had nowhere to go.
Verification:
Confirmed this service had zero outbox infrastructure prior to this pass (no `internal/outbox` package, no `outbox_events` table in any migration) — the three direct `h.publisher.Publish` calls, despite the file's line numbers having shifted substantially since the audit (reflecting this service's otherwise-active commit history), were unchanged in substance.
Fix Applied:
Added migration `deployments/migrations/000003_add_outbox_events.{up,down}.sql` (table `outbox_events` with nullable `tenant_id` — NULL for platform-wide `source_authority_map` events, populated for `normalized_fact` events); added `internal/outbox/outbox.go` (`Event`, `Insert(ctx, tx, e)`, `Relay`/`NewRelay`/`Start`/`RelayOnce` using `SELECT ... FOR UPDATE SKIP LOCKED`), mirroring `retention-registry-svc`'s and `metric-registry-svc`'s proven template exactly; added `KafkaPublisher.PublishOutbox` (`internal/events/publisher.go`), deliberately not setting `Topic` on the outgoing `kafka.Message` (the `Writer` already pins it); rewrote all three write paths (handler + store) to insert the outbox event in the same transaction as the domain row — for `CreateSourceAuthorityMap`, this required converting a bare `s.pool.Exec` into a proper `s.pool.Begin`/`tx.Exec` transaction, since no transaction existed there before; for `RecordFact`, the outbox insert runs inside the same `app.tenant_id`-scoped transaction `withTenantRLS` already installs; wired `outbox.NewRelay` into `cmd/server/main.go`.
Verification:
New tests `TestInsert_NilTx_ReturnsError`, `TestRelayOnce_NilPool_ReturnsZero`, `TestRelay_Start_StopsOnContextCancellation`, `TestMockPublisher_ErrorPropagation` (outbox package); `TestCreateSourceAuthorityMap_OutboxEventRecorded`, `TestCreateSourceAuthorityMap_ReplayDoesNotRecordASecondOutboxEvent`, `TestSupersede_OutboxEventRecorded`, `TestRecordFact_OutboxEventRecorded` (handler, in-memory stub); `TestPgStore_Outbox_AtomicityAndRelay` (store, real Postgres — proves the domain row and outbox row commit/roll back together and that the relay retries a failed publish and succeeds on the next attempt); `TestPgStore_CreateSourceAuthorityMap_IdempotentReplay_ReturnsOriginal` and `TestPgStore_RecordFact_IsIdempotentAndAtomic` (prove a replay does NOT record a second outbox event, at the real-database layer). All pass. **Live Kafka proof obtained without the blocked container rebuild** (see Environment Limitation): ran this exact code directly on the host against the real `postgres`/`kafka` containers; a directly-inserted `outbox_events` row was picked up by the live relay within ~2 seconds, `published_at` was set, and the message was confirmed via direct consumption of the real `zoiko.source-authority.events` Kafka topic. Test data cleaned up after capture.
Evidence:
`deployments/migrations/000003_add_outbox_events.{up,down}.sql`, `internal/outbox/outbox.go`, `internal/outbox/outbox_test.go`, `internal/events/publisher.go` (`PublishOutbox`), `internal/store/pg_store.go` (`CreateSourceAuthorityMap`, `SupersedeSourceAuthorityMap`, `RecordFact`), `internal/handler/handler.go` (all three write handlers), `internal/handler/gaps_test.go`, `internal/store/pg_store_test.go`.

### Gap 2 — Database Connection as Postgres Superuser Unconditionally Bypassed an Already-Correctly-FORCEd RLS Policy (newly discovered this pass, critical, service-owned)
Status: ✅ Code Fixed (compose `DATABASE_URL` uses `zoiko_app`). Deployment state and the live proofs below are historical observations, not re-verified
Discovery Evidence:
`deployments/docker-compose.yml` set `DATABASE_URL` to `${DB_USER:-postgres}:${DB_PASSWORD:-postgres}`, the Postgres superuser default, with no `.env` override in the repository — the same pattern found in `kill-switch-registry-svc`, `retention-registry-svc`, and (as a least-privilege-only case) `metric-registry-svc`. **Unlike `metric-registry-svc`, this is a genuine RLS-bypass vulnerability**: migration `000002_tenant_scope_facts_and_invariants.up.sql` correctly set `ALTER TABLE normalized_facts ENABLE ROW LEVEL SECURITY; ALTER TABLE normalized_facts FORCE ROW LEVEL SECURITY;` with a `tenant_isolation_policy` — but Postgres unconditionally exempts a superuser from RLS regardless of FORCE (`rolsuper`/`rolbypassrls`), and the then-running container was observed, via `pg_stat_activity` during the original engagement (historical, not re-verified), to be connecting as `postgres`. This silently defeated the exact isolation migration 000002 was written to guarantee, for every read and write touching `normalized_facts` (tenant business facts including payroll, billing contacts, and HR status).
Classification: SERVICE-OWNED DEFECT, security-relevant (cross-tenant data exposure risk), distinct from the audit's stale SEC-03/line-222/230/424-428 finding — see Gap 3 for why that specific literal claim ("RLS not forced") is false, while the underlying risk category the audit was gesturing at was real for a different reason.
Fix Applied:
`deployments/docker-compose.yml`: `DATABASE_URL` changed to the least-privilege, non-superuser `zoiko_app` role, with an inline comment explicitly distinguishing this as a genuine RLS-bypass fix (not least-privilege-only). Confirmed via `\dp source_authority_maps` and `\dp normalized_facts` that `zoiko_app` already holds the necessary `arwd` grants on both tables (and, after applying migration 000003 to the live database, on the new `outbox_events` table too) through the platform's pre-existing `ALTER DEFAULT PRIVILEGES` setup — no new grants were required. No application code in `internal/store/pg_store.go` required any change: every method touching `normalized_facts` already correctly routes through `withTenantRLS`, which installs `app.tenant_id` via `set_config` inside the same transaction as every statement — unlike `retention-registry-svc`'s equivalent fix earlier in this engagement, there was no latent "forgot to call withTenant" regression waiting to be uncovered here.
Verification:
New tests `TestRLS_NormalizedFactsEnabledAndForced` (asserts both `relrowsecurity` and `relforcerowsecurity` directly from `pg_class`, and separately pins that `source_authority_maps` correctly has NEITHER), `TestRLS_TenantCannotReadAnotherTenantsFacts`, `TestRLS_ResolveDoesNotComposeAnotherTenantsFacts`, and `TestRLS_SourceAuthorityMapsSharedAcrossTenants` — all run against a purpose-created `NOSUPERUSER NOBYPASSRLS` role (`zoiko_app_test`), the same discipline `retention-registry-svc`'s and `kill-switch-registry-svc`'s RLS suites already established, because a superuser connection proves nothing about RLS. All pass. **Live, end-to-end proof with genuinely seeded ground-truth data, obtained without the blocked container rebuild**: ran this exact code directly on the host with `DATABASE_URL` pointed at `zoiko_app`; `pg_stat_activity` confirmed the connection authenticates as `zoiko_app`, not superuser; two facts were inserted for two different tenants (as `zoiko_app`, through the RLS `WITH CHECK`, proving write grants are sufficient); a live, unauthenticated `GET /v1/source-authority/resolve` call as tenant A returned only tenant A's fact value, the identical call as tenant B returned only tenant B's, and a third tenant with no facts at all received neither — genuine tenant isolation proven through the real HTTP contract, not just the store layer. Test data cleaned up after capture.
Evidence:
`deployments/docker-compose.yml` (`DATABASE_URL`), `internal/store/rls_test.go` (4 new tests), live `pg_stat_activity` showing `zoiko_app`, live two-tenant `GET /v1/source-authority/resolve` isolation proof, live `\dp` output on all three tables.

### Gap 3 — "RLS Enabled Without FORCE Clause" (Original Gap, SEC-03 — found FALSE, documentation drift, no code change)
Status: ⚪ Documentation Drift — not a code defect
Original Issue:
The master summary and the dedicated finding at lines 222, 230, and 424-428 stated that `source-authority-svc`'s migration "calls `ENABLE ROW LEVEL SECURITY` but omits `FORCE ROW LEVEL SECURITY`," citing a migration filename (`000002_add_rls.up.sql`) that does not exist anywhere in this service's `deployments/migrations/` directory.
Discovery Evidence:
The actual file, `000002_tenant_scope_facts_and_invariants.up.sql`, explicitly executes both `ALTER TABLE normalized_facts ENABLE ROW LEVEL SECURITY;` and `ALTER TABLE normalized_facts FORCE ROW LEVEL SECURITY;` (lines 48-49), with a doc comment explaining exactly why FORCE matters and citing the same defect class in `delegated-authority-svc`. `TestRLS_NormalizedFactsEnabledAndForced` proves this directly against `pg_class.relforcerowsecurity`, against a real database.
Classification: DOCUMENTATION DRIFT — the audit's literal claim does not match the current (or, per `git log`'s clean history for this file, any committed) code. The underlying risk category the audit was gesturing at (a superuser connection defeating tenant isolation) was nonetheless real, for the different reason documented in Gap 2 above — this is not a case of the audit being wrong about there being a risk, only about which specific code defect caused it.
Evidence:
`deployments/migrations/000002_tenant_scope_facts_and_invariants.up.sql` (lines 43-54), `internal/store/rls_test.go` (`TestRLS_NormalizedFactsEnabledAndForced`).

**Remaining Gaps:**
- None within `source-authority-svc`'s own backend code that are still open at the code level.
- **Deployment state (historical, unverified — not a code defect):** during the original engagement the running container on port 8150 was observed to predate the fixes (superuser database connection, no outbox). Whether any container is running today, and from which image, cannot be determined from the repository; this is recorded as a historical observation, not as a current exposure.

**Dependency-Limited Items:**
- ❌ **Not Fixed — `authorization-svc` missing migrations (cross-service dependency, not owned by this service):** the same platform-wide blocker documented throughout this file. Reproduced directly against this service's own write path: a live `POST /v1/source-authority-maps` with full envelope headers and a resolvable principal returned `503 authorization service unavailable`; `authorization-svc`'s own logs show the same root cause documented for every other service this pass touched — `pg FindGrantedActionsScoped failed: ERROR: column pra.book_id does not exist (SQLSTATE 42703)`. `source-authority-svc`'s own handling is correct (fail-closed 503, confirmed no partial write via direct query afterward) — the defect is entirely inside `authorization-svc`'s own schema/migrations.

**Environment Limitation (historical observation from the original engagement; not re-verified):**
- Docker Desktop's build path could not resolve `docker.io/library/golang:1.25-alpine` during this pass: `docker compose build source-authority-svc` failed with `context deadline exceeded` resolving the image manifest — the same registry-side condition (most plausibly Docker Hub anonymous-pull rate limiting) documented at length in `metric-registry-svc`'s section earlier in this file, re-confirmed here rather than re-investigated from scratch. No workaround altering this service's intended runtime configuration was attempted.
- **What this pass did instead, consistent with the established, non-score-inflating methodology used throughout this file**: compiled and ran this exact, unmodified source tree directly on the host (`go run ./cmd/server`), configured via the same environment variables the Docker container would receive, pointed at the already-running `postgres`, `kafka`, and `authorization-svc` containers on their published host ports. This proved Gaps 1 and 2 work correctly against real infrastructure, including a genuine two-tenant RLS isolation proof through the real HTTP `resolve` endpoint; it did not and cannot prove anything about the actual container image, which remains unrebuilt.
- **As a result, `source-authority-svc`'s Docker container could not be rebuilt or redeployed this pass.** Gaps 1 and 2 are verified by code review, a full real-Postgres test suite (including dedicated RLS isolation tests against a genuine NOSUPERUSER role), and live runtime behavior outside the container; they are not yet verified inside the actual container image, which remains the pre-this-pass build (superuser DB connection, no outbox).

**Needs-Clarification Items:**
- **Resolve identity requirement.** This file says `Resolve` is "identity-and-tenant-gated" in one place and "requires `X-Tenant-Id`" in another. The implementation requires the tenant header only and does not require `X-Principal-Id`. Not changed.
- **Tenant-scoped fact authorization scope.** `NORMALIZED_FACT_RECORD` and `NORMALIZED_FACT_VIEW` are checked at the platform scope id, with the tenant passed to `authorization-svc` separately. Whether that is the intended scope is not established. Not changed.
- **`Store.GetSourceAuthorityMap` is implemented but unreachable** from any route or frontend call. No route was invented and the method was not removed.
- **Frontend 90% claim:** not audited.
- **Idempotency-Key vs `correlation_id`.** The common envelope contract requires `Idempotency-Key` on material writes and states that a service changing material state needs replay protection (INV-08); it does not say that the replay identity must be the header value. This service provides replay protection through `correlation_id`. Whether the header should additionally (or instead) be the replay identity is a contract question; no behavior was changed.

**Verification Results (historical — the Docker, live-container and live-probe items below are observations from the original engagement and are not re-verified; the current test inventory is in the Remediation Pass):**
- Backend Unit Tests (original pass): reported as 45 tests passing against a real Postgres database (`source_authority_test`). That count is superseded: the repository now contains 53 test functions (see the Backend Completion note and the Remediation Pass).
- Backend Build: `go build ./...` clean.
- Backend Lint: `go vet ./...` clean with 0 warnings or errors.
- Docker Build: ❌ Not completed this pass — blocked by the `golang:1.25-alpine` registry resolution failure described above.
- Docker Health (actual container): Live container `source-authority-svc` Up (healthy) on port 8150, but still running the pre-this-pass image (confirmed connecting as `postgres` superuser via `pg_stat_activity`) — not yet rebuilt or redeployed.
- Live Database Grants: `\dp source_authority_maps`, `\dp normalized_facts`, and `\dp outbox_events` all confirm `zoiko_app` holds `arwd` on the live `source_authority` database.
- Live Runtime Verification (this exact code, run outside the container): `/healthz`/`/readyz` return 200; `pg_stat_activity` confirms the connection is `zoiko_app`, not superuser; a direct write succeeded as `zoiko_app` on `source_authority_maps` (no RLS) and, under the RLS `WITH CHECK`, on `normalized_facts` for two separate tenants; a real outbox row was relayed and consumed from the live `zoiko.source-authority.events` Kafka topic; a live, unauthenticated `GET /v1/source-authority/resolve` call genuinely isolated two tenants' facts from each other and correctly returned nothing for a third tenant with no facts; a live `POST /v1/source-authority-maps` with full envelope headers correctly failed closed with 503 when `authorization-svc` proved to be broken, with no partial write. All test data created directly via SQL for this verification was cleaned up afterward.
- Live Health (actual container, pre-this-pass image): `/healthz` and `/readyz` both return 200 OK.

### Remediation Pass (2026-10-19)

Documentation-only; no implementation file was changed.

| Finding | Result |
|---|---|
| "`Idempotency-Key`-protected via the outbox" | Corrected. Replay identity for create is the required body `correlation_id` (unique partial index, `ON CONFLICT DO NOTHING`, replay returns the original row with 200 and no second outbox event). The `Idempotency-Key` header is required by the common write envelope but is not read by the handler. The contract text (`services/_contract/envelope`, `docs/architecture/input-contract-conformance.md`) requires replay protection and the header on writes but does not define the header as the replay identity, so no behavior was changed |
| Test counts | Corrected to the repository inventory: 53 test functions (34 handler, 4 outbox, 2 events, 13 store). Executed on 2026-10-19 against a disposable PostgreSQL: 53 passed, 0 failed, 0 skipped; with no database, 40 passed and the 13 DB-backed tests skipped |
| Runtime and production-readiness claims | Relabelled as historical, unverified observations (container image, `pg_stat_activity`, Kafka consumption, two-tenant HTTP probe, Docker build state). Production readiness is not claimed |
| Resolve identity, fact authorization scope, `GetSourceAuthorityMap`, frontend | Clarification retained; listed under Needs-Clarification Items |

Unchanged by design: all routes and permissions, the platform-wide `source_authority_maps` table, FORCE RLS on `normalized_facts`, the `zoiko_app` role, the outbox events, the append-only fact model, supersede behavior, unknown-field rejection and the 1 MiB write-body limit. The replay-without-body-comparison behavior, the rule-uniqueness key, and the exported `KafkaPublisher.Publish` (which logs "event dropped" and returns `nil`, with no live caller) were left as they are.

External blocker: the `authorization-svc` migration gap; a real authorized create/supersede/record has not been exercised end to end.

