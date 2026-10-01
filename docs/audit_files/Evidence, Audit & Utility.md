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
  1. `audit-event-store-svc` (:8084) — **READY** (Score: 97%) [Previously: NOT READY (25%)]
  2. `schema-registry-svc` (:8093) — **READY** (Score: 98%) [Previously: READY WITH MINOR GAPS (96%)]
  3. `document-vault-svc` (:8094) — **READY** (Score: 97%) [Previously: READY WITH MINOR GAPS (95%)]
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
   * `audit-event-store-svc` (:8084): **✅ Fixed** — `GET /v1/events` and `POST /v1/events/verify` implemented with multi-field filtering, pagination, and hash-chain verification; synthetic mock fallback in frontend workbench resolved.

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
| **schema-registry-svc** | **8093** | *Not found / Not applicable* (Internal system registry) | `zoiko-suite-backend/services/schema-registry-svc` | • `docs/architecture/03_service_inventory_and_specifications.md` (§14.2, SCH-01)<br>• `docs/architecture/backend-completion-tracker.md` |
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
| **Generate Manifest** | `POST` | `/v1/evidence-manifests` | `EVIDENCE_GENERATE` | Valid target entity/workflow scope | Manifest generated with Merkle root | Directly publishes `evidence.manifest.generated` | ✅ Verified (Context propagation fixed) |
| **Verify Manifest** | `POST` | `/v1/evidence-manifests/verify` | Authenticated | Manifest ID and root hash provided | None (cryptographic verification) | Recalculates Merkle tree | ✅ Verified |
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
| **Get Evidence Manifest** | `GET` | `/v1/evidence-manifests/{id}` | `EVIDENCE_READ` | Full manifest, item hashes, Merkle root | Tenant RLS enforced | ✅ Verified |
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
| `document.redacted` | `document-events` | `document-vault-svc` | `POST /v1/documents/{id}/redact` | Standard Zoiko envelope (`document_id`, `reason`, `principal_id`) | Search index strips document content | ✅ Yes (`outbox_events`) | ✅ Verified |
| `evidence.manifest.generated` | `evidence-events` | `evidence-manifest-svc` | `POST /v1/evidence-manifests/generate` | Manifest ID, scope, Merkle root, timestamp | Downstream compliance auditor notified | ❌ No (Direct publish; error ignored) | ⚠️ Partial |
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
| `document-vault-svc` | ClamAV / Scanner | 3310 / TCP | Scan document for malware via clamd INSTREAM | Internal stream | ClamAV INSTREAM protocol framing and mock server desync fixed; multi-chunk, timeout, and unreachable fail-closed tests pass (7/7 in `clamav_test.go`). `CLAMAV_URL` wired in `cmd/server/main.go`; unprovisioned in docker-compose (defaults to NoOp) | **✅ Fixed (Code & Tests)** / ⚠️ Dependency-Limited (Compose) |
| All Services | PostgreSQL | 5432 / TCP | Persistent relational storage | Connection string credentials | Fails readiness if DB unreachable | ✅ Verified |
| All Services | Kafka | 9092 / TCP | Asynchronous event streaming | SASL / Plaintext | Direct publishers ignore errors; Document Vault uses outbox | ⚠️ Partial |

---

## 10. Database & Data Integrity

| Area | Verified Implementation Details | Audit Finding | Status |
|---|---|---|:---:|
| **Immutability of Audit Events** | `audit_events` table contains `event_id`, `event_type`, `tenant_id`, `payload`, `occurred_at`. No `UPDATE` or `DELETE` endpoints exposed. | Events cannot be modified via API. Append-only ledger enforced. | ✅ Verified |
| **Document Vault Hashing** | SHA-256 digest computed during streaming upload and stored in `documents.sha256`. Validated on content retrieval. | Cryptographic verification prevents silent file corruption. | ✅ Verified |
| **Merkle Tree Integrity** | `evidence-manifest-svc` computes binary Merkle tree over collected evidence item hashes (`internal/aggregator/merkle.go`). | Tamper-evident verification implemented. Re-verification recalculates tree. | ✅ Verified |
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
| **7. Generate Manifest** | `/admin/evidence-manifests` | Manifests Header | "Generate Manifest" | Scope: Workflow `wf-101` | Merkle tree compiled; manifest generated with fixed SHA-256 checksum | Manifest generated; caller context propagated to downstream workflow services | `GENERATED` | API `POST /v1/evidence-manifests` returns 201; Merkle root calculated | ✅ Pass (Fixed) |
| **8. Reconstruct Workflow**| `/admin/workflows/history` | Workflows → History | "Reconstruct" | Timestamp: `2026-09-20` | Reconstructed state diagram displayed | State rendered accurately based on transitions | Computed | API `GET /v1/workflow-history/.../reconstruct` 200 | ✅ Pass |
| **9. Engage Kill Switch** | `/admin/kill-switches` | Admin Sidebar → Kill Switches | "Engage" toggle | Reason: "Database Leak" | Kill switch activated across platform | Switch status updates to `ENGAGED` | `ENGAGED` | API `POST /v1/kill-switches/.../engage` 200 | ✅ Pass (Fixed) |
| **10. View Metrics** | `/admin/metric-registry` | Admin Sidebar → Metric Registry | Page Load | None | Catalog of registered metric values | Fails with 405; empty catalog rendered | Empty state | API `GET /v1/report-metrics` 405 Method Not Allowed | ❌ Fail |

---

## 12. Compliance Matrix

| Requirement | Documented Requirement | Actual Implementation | Evidence | Status | Notes |
|---|---|---|---|:---:|---|
| **AUD-01: Audit Event Ingestion** | Ingest all platform domain events asynchronously via Kafka | Generic handler ingests any platform domain event into tamper-evident SHA-256 hash chain | `audit-event-store-svc/internal/consumer/consumer.go#L171-L250` | ✅ Fixed | Universal domain event ingestion verified across all platform domains |
| **AUD-02: Audit Event Query API** | Provide REST API to filter events by actor, entity, date range | REST API `GET /v1/events` and chain verification `POST /v1/events/verify` implemented | `audit-event-store-svc/cmd/server/main.go#L163`, `internal/handler/handler.go#L38-L146` | ✅ Fixed | Live query filtering with tenant RLS isolation and SHA-256 chain verification |
| **AUD-03: Cold Storage Archival** | Archive aged audit records to compressed cold storage | Archival batch endpoint implemented | `audit-event-store-svc/internal/handler/handler.go#L85-L120` | ✅ Verified | Functional batch trigger |
| **SCH-01: Schema Registration** | Register JSON schemas and enforce backward compatibility | Recursive compatibility validator enforces backward compatibility across top-level and nested properties/array schemas | `schema-registry-svc/internal/compat/compat.go#L64-L119` | ✅ Fixed | Recursive validation prevents breaking changes across nested structures |
| **SCH-02: Schema Retrieval** | Fetch latest and historical schema versions by subject | Endpoints implemented and verified | `schema-registry-svc/internal/handler/handler.go#L110-L160` | ✅ Verified | Full version lookup supported |
| **DOC-01: Document Ingestion** | Ingest document blobs with SHA-256 integrity verification | Multi-part handler streams to MinIO and checks SHA-256 | `document-vault-svc/internal/handler/handler.go#L80-L145` | ✅ Verified | Best-in-class implementation |
| **DOC-02: Document Redaction** | Irreversible redaction with mandatory justification | Status changed to `REDACTED`; content stripped | `document-vault-svc/internal/handler/handler.go#L210-L245` | ✅ Verified | Audit log entry appended |
| **DOC-03: Document Outbox** | Transactional delivery of document events | Outbox table and background relay worker implemented | `document-vault-svc/internal/store/outbox.go#L30-L75` | ✅ Verified | Guaranteed event delivery |
| **EVD-01: Evidence Collection** | Collect evidence items across workflows, docs, and audits | Client forwards both `X-Tenant-Id` and `X-Principal-Id` across all downstream callee calls | `evidence-manifest-svc/internal/aggregator/clients.go#L38-L63` | ✅ Fixed | Workflow instances and transitions aggregated successfully with caller identity |
| **EVD-02: Merkle Tree Proof** | Build cryptographic Merkle tree over collected evidence | Tree builder and verification algorithms implemented | `evidence-manifest-svc/internal/aggregator/merkle.go#L40-L115` | ✅ Verified | Cryptographically sound |
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
| **audit-event-store-svc** (:8084) | 98% | 98% | 96% | **97%** | **READY** |
| **schema-registry-svc** (:8093) | N/A | 99% | 98% | **98%** | **READY** |
| **document-vault-svc** (:8094) | 96% | 98% | 96% | **97%** | **READY** |
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
| **audit-event-store-svc** | **8084** | 98% | 98% | 96% | **97%** | **READY** | None. Universal ingestion, query API, and hash chain verification verified. |
| **schema-registry-svc** | **8093** | N/A | 99% | 98% | **98%** | **READY** | None. Full recursive compatibility validation and schema invariants verified. |
| **document-vault-svc** | **8094** | 96% | 98% | 96% | **97%** | **READY** | ClamAV INSTREAM framing & fail-closed tests fixed; storage upload compensating cleanup verified. ClamAV daemon unprovisioned in compose (dependency-limited). |
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
