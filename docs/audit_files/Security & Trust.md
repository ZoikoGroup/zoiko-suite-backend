# Domain Audit: Security & Trust

**Domain:** Security & Trust (mTLS Infrastructure, SIEM Streaming, Zero-Trust Access Scoring, BYOK/HYOK Key Custody)
**Scope:** `mtls-management-svc` (:8140), `siem-integration-svc` (:8141), `carta-svc` (:8142), `key-management-svc` (:8143)
**Audit Methodology:** Read-only audit of source code, tests (including live `go build`/`go vet`/`go test` execution), database/persistence layer, Docker Compose configuration, frontend implementation (including live Playwright browser verification of the actual rendered UI), and available architecture documentation. No application source code was modified as part of this audit.

---

## 1. Service Inventory

| Service Name | Port | Backend Location | Frontend Location | Documentation Sources |
|---|:---:|---|---|---|
| **mtls-management-svc** | **8140** | `zoiko-suite-backend/services/mtls-management-svc` | `Zoiko-suite-frontend-platform/components/admin/security-trust/MtlsManagementPanel.tsx` (shared page: `app/admin/security/page.tsx`) | `docs/architecture/03-microservices.md:18`; `docs/architecture/05-security.md` (mTLS/SIEM mentions); `docs/architecture/master-register-findings-2026-08-27.md` §3.27; `docs/architecture/backend-completion-tracker.md` rows 88/8m; `deployments/docker-compose.yml` comments; `deployments/docker-compose.override.yml` |
| **siem-integration-svc** | **8141** | `zoiko-suite-backend/services/siem-integration-svc` | `Zoiko-suite-frontend-platform/components/admin/security-trust/SiemIntegrationPanel.tsx` | `docs/architecture/phase-6-trust-intelligence.md:19`; `docs/architecture/05-security.md` §13; `docs/architecture/master-register-findings-2026-08-27.md:595` (states no dedicated spec exists); `docs/architecture/known-gaps.md:1654-1689` |
| **carta-svc** | **8142** | `zoiko-suite-backend/services/carta-svc` | `Zoiko-suite-frontend-platform/components/admin/security-trust/CartaAssessmentPanel.tsx` (plus an orphaned, unused `CartaCapTablePanel.tsx` from an earlier, incorrect cap-table interpretation of "CARTA") | `docs/architecture/05-security.md` §5.4; `docs/architecture/phase-6-trust-intelligence.md`; `docs/architecture/known-gaps.md:20`; `docs/architecture/full-architecture-gap-analysis.md:7,46`; `docs/architecture/master-register-findings-2026-08-27.md:1313` |
| **key-management-svc** | **8143** | `zoiko-suite-backend/services/key-management-svc` | `Zoiko-suite-frontend-platform/components/admin/security-trust/KeyManagementPanel.tsx` | `docs/architecture/05-security.md` §8.6, §9.5, §9.6; `docs/architecture/phase-6-trust-intelligence.md:21`; `docs/architecture/full-architecture-gap-analysis.md:94,99,1293`; `docs/architecture/backend-completion-tracker.md` rows 8k/82/90 |

**Shared frontend API client:** `Zoiko-suite-frontend-platform/lib/api/security-trust.ts` — a complete, backend-field-accurate client for all four services. **Confirmed imported by zero files anywhere in the frontend repository** (see §10/§11). **Shared frontend page:** `app/admin/security/page.tsx`, rendering all four panels plus an AI Governance panel (AI Governance is out of scope for this domain audit).

**No dedicated functional specification document** (`ZS-ARCH-SVC-*` or `doc7` entry) was found for any of the four services. All four are documented only via general architecture-standard principles (Doc 05) and after-the-fact tracker/gap-analysis documents. This is recorded as **Documentation unavailable (partial)** per service below, not assumed to be a defect in itself.

**Environment note (applies throughout this file):** only `mtls-management-svc` is deployed and running in this environment (`deployments/docker-compose.yml`, confirmed healthy via `docker ps`). `siem-integration-svc`, `carta-svc`, and `key-management-svc` are defined only in `deployments/docker-compose.phase6.yml`, a separate compose stack that has never been started in this environment. Live, running-container verification was therefore possible only for `mtls-management-svc`; the other three are verified by source code, `go build`/`go vet`/`go test`, and static analysis only.

---

## 2. Segregation of Duties (SoD)

### 2.1 `mtls-management-svc`
* **Maker:** Any principal holding the relevant `MTLS_CERT_*`/`MTLS_POLICY_*` grant for a tenant/legal entity.
* **Checker/Approver:** **Not found / Not documented.** No maker-checker or dual-approval pattern exists anywhere in this service.
* **Executor:** The service itself (synchronous — issues/rotates/revokes immediately on a granted request).
* **Required Role/Permission:** `MTLS_CERT_PROVISION`, `MTLS_CERT_READ`, `MTLS_CERT_ROTATE`, `MTLS_CERT_REVOKE`, `MTLS_POLICY_WRITE`, `MTLS_POLICY_READ` — deliberately split by consequence (revoke/rotate are distinct privileges from provision).
* **Self-Approval Restrictions:** Not applicable — no approval step exists to self-approve.
* **Separation Rules:** Certificate routes authorize against the **certificate's own** `legal_entity_id` (never caller-supplied); policy routes authorize against the **tenant** (policies have no legal entity).
* **Actual Enforcement:** Every mutating/read route except the bootstrap path requires a real `authorization-svc` grant, fail-closed (503 on unavailability, 403 on denial).
* **SoD Gaps:** **A single authorized principal can unilaterally revoke a service's mTLS trust** with no second approver. Additionally, the **bootstrap self-provisioning path** accepts a caller-claimed `ServiceName` with no allowlist and, as currently wired, is non-functional (see §8/§11 — confirmed by 3 failing tests).

### 2.2 `siem-integration-svc`
* **Maker:** Internal platform services (gateway-auth-svc, mtls-management-svc, key-management-svc, authorization-svc, identity-context-svc) streaming security events; tenant admins creating exporters.
* **Checker/Approver:** **Not found / Not documented.** No approval workflow exists.
* **Executor:** The service itself (accepts and stores in memory; does not forward anywhere — see §9).
* **Required Role/Permission:** `SIEM_EXPORTER_CREATE`, `SIEM_EXPORTER_READ`, `SIEM_EVENT_READ`.
* **Self-Approval Restrictions:** Not applicable.
* **Separation Rules:** `GetExporter`/`ListEvents` authorize against the **resource's own** `legal_entity_id`, never a caller-supplied scope.
* **Actual Enforcement:** 3 of 5 routes are authz-gated, fail-closed. 2 routes (`POST /v1/siem/stream`, `GET /v1/siem/exporters`) are **deliberately tenant-only, no principal/authz check** — documented rationale: 5 internal callers send only `X-Tenant-ID`, never a principal, and one of the callers (gateway-auth-svc) is reporting an *authentication failure*, so by definition there is no authenticated principal to check.
* **SoD Gaps:** Documented, accepted residual risk on the 2 ungated routes — "a caller holding a tenant header can inject fabricated security events, and can enumerate exporter names and endpoint URLs" (named in code, not hidden). The stated correct fix (service-identity via mTLS) is not yet wired for this route.

### 2.3 `carta-svc`
* **Maker:** `gateway-auth-svc`, calling `/v1/carta/evaluate` mid-authentication on behalf of a subject being evaluated.
* **Checker/Approver:** **Not found / Not documented.** No approval step — this is a scoring/decision engine, not a workflow.
* **Executor:** `gateway-auth-svc` (the actual caller) enforces the resulting decision (blocks on `ISOLATE`/`DENY`; logs but does not yet block on `STEP_UP_MFA` — no downstream step-up challenge flow exists).
* **Required Role/Permission:** `CARTA_ASSESSMENT_READ` (reads only).
* **Self-Approval Restrictions:** Not applicable.
* **Separation Rules:** `GetAssessment`/`ListAssessments` authorize against the assessment's own `legal_entity_id`.
* **Actual Enforcement:** 2 of 3 routes gated, fail-closed. `POST /v1/carta/evaluate` is **deliberately ungated** — gateway-auth-svc calls it *before* any verified identity envelope exists, so requiring authorization here would be circular (the answer would be needed to ask the question).
* **SoD Gaps:** Documented, accepted residual risk on `/evaluate` — "a caller holding a tenant header can submit fabricated evaluation requests, which both pollutes the tenant's assessment history and lets an attacker probe the scoring function to learn where the ALLOW boundary sits." Same mTLS-identity fix named as the long-term remedy, not yet wired.

### 2.4 `key-management-svc`
* **Maker:** Any principal holding the relevant `KEY_*` grant for a legal entity.
* **Checker/Approver:** **Not found / Not documented.** No maker-checker pattern, despite the code's own comments repeatedly describing `DisableKey` as "the destructive route" that could cause "unrecoverable data loss" once real KMS operations exist.
* **Executor:** The service itself (synchronous metadata mutation only — see §9, no actual key material is ever handled).
* **Required Role/Permission:** `KEY_REGISTER`, `KEY_READ`, `KEY_ROTATE`, `KEY_DISABLE`.
* **Self-Approval Restrictions:** Not applicable.
* **Separation Rules:** `RotateKey`/`DisableKey` read the key first to authorize against its **own** `legal_entity_id`, never a caller-supplied scope (read-before-authorize pattern, deliberate to avoid authorizing against post-mutation state).
* **Actual Enforcement:** **5 of 5 routes** are authz-gated, fail-closed — the most complete SoD coverage of the four services in this domain.
* **SoD Gaps:** No dual-control/maker-checker exists for `DisableKey`, a single-principal unilateral action, despite the code's own comments flagging its severity.

---

## 3. Commands

### 3.1 `mtls-management-svc`

| Command/Action | Endpoint | Required Role/Scope | Preconditions | State Changes | Side Effects |
|---|---|---|---|---|---|
| `ProvisionCert` | `POST /v1/mtls/certificates` | `MTLS_CERT_PROVISION` (legal-entity scope) **or** valid `X-Mtls-Bootstrap-Token` (service-identity scope) | Valid `legal_entity_id`/`service_name`/`common_name`; `rotation_days` defaults to 90 | New cert row, status `ACTIVE` | SIEM `certificate.issued` (Low) |
| `RotateCert` | `POST /v1/mtls/certificates/{id}/rotate` | `MTLS_CERT_ROTATE` (cert's own legal entity) | Cert exists in caller's tenant | New key/cert material replaces row in place; same ID | SIEM `certificate.rotated` (Low) |
| `RevokeCert` | `DELETE /v1/mtls/certificates/{id}` | `MTLS_CERT_REVOKE` (cert's own legal entity) | Cert exists in caller's tenant | Status → `REVOKED` | SIEM `certificate.revoked` (Medium) |
| `CreatePolicy` | `POST /v1/mtls/policies` | `MTLS_POLICY_WRITE` (tenant scope) | `policy_name` non-empty | New policy row | None found |

### 3.2 `siem-integration-svc`

| Command/Action | Endpoint | Required Role/Scope | Preconditions | State Changes | Side Effects |
|---|---|---|---|---|---|
| `CreateExporter` | `POST /v1/siem/exporters` | `SIEM_EXPORTER_CREATE` (legal-entity scope) | `legal_entity_id`/`name`/`platform`/`endpoint_url` required | New exporter row, status `ACTIVE` | None found |
| `StreamEvent` | `POST /v1/siem/stream` | **None** (tenant-only, deliberate) | `exporter_id`/`source_service`/`event_type`/`message` required; exporter must resolve for caller's tenant | New event row, status set to literal `"DELIVERED"` unconditionally | None — event is stored only, never actually forwarded (see §9) |

### 3.3 `carta-svc`

| Command/Action | Endpoint | Required Role/Scope | Preconditions | State Changes | Side Effects |
|---|---|---|---|---|---|
| `EvaluateAccess` | `POST /v1/carta/evaluate` | **None** (tenant-only, deliberate) | `legal_entity_id`/`subject_id`/`action_requested` required | New immutable assessment record | None found (no events published) |

### 3.4 `key-management-svc`

| Command/Action | Endpoint | Required Role/Scope | Preconditions | State Changes | Side Effects |
|---|---|---|---|---|---|
| `RegisterKey` | `POST /v1/keys` | `KEY_REGISTER` (legal-entity scope) | `legal_entity_id`/`key_alias`/`key_provider`; `external_key_arn` required for BYOK/HYOK | New key row, state `ENABLED` | None found |
| `RotateKey` | `POST /v1/keys/{id}/rotate` | `KEY_ROTATE` (key's own legal entity) | Key exists in caller's tenant | `key_version`/`rotation_count` incremented; `last_rotated_at` set — **metadata only, no actual cryptographic material is touched** | SIEM `key.rotated` (Medium) |
| `DisableKey` | `POST /v1/keys/{id}/disable` | `KEY_DISABLE` (key's own legal entity) | Key exists in caller's tenant | State → `DISABLED` | SIEM `key.disabled` (High) |

---

## 4. Reads

### 4.1 `mtls-management-svc`

| Read/Query | Endpoint | Required Permission/Header | Returned Information | Tenant/Security Restrictions |
|---|---|---|---|---|
| `GetCert` | `GET /v1/mtls/certificates/{id}` | `X-Tenant-ID` + `X-Principal-Id` + `MTLS_CERT_READ` (cert's own legal entity) | Certificate record (never includes private key PEM) | 404 on foreign-tenant/absent (never 403 — avoids confirming existence) |
| `ListCerts` | `GET /v1/mtls/certificates` | `X-Tenant-ID` + `X-Principal-Id` + `MTLS_CERT_READ`; `legal_entity_id` query param **mandatory** | `{"data":[...],"count":n}` | Tenant-scoped |
| `ListPolicies` | `GET /v1/mtls/policies` | `X-Tenant-ID` + `X-Principal-Id` + `MTLS_POLICY_READ` (tenant scope) | `{"data":[...],"count":n}` | Tenant-scoped |

### 4.2 `siem-integration-svc`

| Read/Query | Endpoint | Required Permission/Header | Returned Information | Tenant/Security Restrictions |
|---|---|---|---|---|
| `ListExporters` | `GET /v1/siem/exporters` | `X-Tenant-ID` only (deliberate, no principal) | `{"data":[...],"count":n}` (auth tokens never serialized) | Tenant-scoped |
| `GetExporter` | `GET /v1/siem/exporters/{id}` | `X-Tenant-ID` + `X-Principal-Id` + `SIEM_EXPORTER_READ` (exporter's own legal entity) | Exporter record (no `auth_token`) | 404 on foreign-tenant/absent |
| `ListEvents` | `GET /v1/siem/events` | `X-Tenant-ID` + `X-Principal-Id` + `SIEM_EVENT_READ`; `exporter_id` query param **mandatory** | `{"data":[...],"count":n}` | 404 (not 403) if exporter not found for tenant — deliberate, avoids confirming existence to a prober |

### 4.3 `carta-svc`

| Read/Query | Endpoint | Required Permission/Header | Returned Information | Tenant/Security Restrictions |
|---|---|---|---|---|
| `GetAssessment` | `GET /v1/carta/assessments/{id}` | `X-Tenant-ID` + `X-Principal-Id` + `CARTA_ASSESSMENT_READ` (assessment's own legal entity) | Full assessment record | 404 on foreign-tenant/absent |
| `ListAssessments` | `GET /v1/carta/assessments` | `X-Tenant-ID` + `X-Principal-Id` + `CARTA_ASSESSMENT_READ`; `legal_entity_id` query param **mandatory** | `{"data":[...],"count":n}`; optional `subject_id` narrows within scope | Tenant-scoped |

### 4.4 `key-management-svc`

| Read/Query | Endpoint | Required Permission/Header | Returned Information | Tenant/Security Restrictions |
|---|---|---|---|---|
| `GetKey` | `GET /v1/keys/{id}` | `X-Tenant-ID` + `X-Principal-Id` + `KEY_READ` (key's own legal entity) | Key metadata including `external_key_arn` reference (never raw key material — none exists) | 404 on foreign-tenant/absent |
| `ListKeys` | `GET /v1/keys` | `X-Tenant-ID` + `X-Principal-Id` + `KEY_READ`; `legal_entity_id` query param **mandatory** | `{"data":[...],"count":n}` | Tenant-scoped |

---

## 5. States & Transitions

### 5.1 `mtls-management-svc` — Certificate
* **Discovered States:** `ACTIVE`, `EXPIRED`, `REVOKED`, `PENDING` (declared in `domain/types.go`).
* **Actually Used:** Only `ACTIVE` (on issue/rotate) and `REVOKED` (on revoke). **`EXPIRED` and `PENDING` are dead states — never set by any code path.**
* **Valid Transitions:** `ACTIVE → REVOKED` (via `RevokeCert`). Rotation replaces material in place without a status transition (stays `ACTIVE`).
* **Who Can Perform Them:** Any principal with the relevant grant (no maker-checker).
* **Preconditions:** Cert must exist within caller's tenant.
* **Invalid/Blocked Transitions:** No automatic expiry sweep exists — a certificate past its `valid_to` date is never transitioned to `EXPIRED`; it simply remains `ACTIVE` in this service's own records indefinitely.

### 5.2 `siem-integration-svc` — Exporter / Event
* **Exporter States:** `ACTIVE`, `PAUSED`, `DISABLED` declared; **only `ACTIVE` is ever set** — no handler transitions an exporter to `PAUSED` or `DISABLED`.
* **Event Status:** `DELIVERED`, `FAILED` declared; **every event is unconditionally stamped `DELIVERED`** at write time regardless of whether anything was actually forwarded anywhere (nothing is — see §9). `FAILED` is never set.
* **Valid Transitions:** None implemented beyond initial creation.

### 5.3 `carta-svc` — Assessment
* **Discovered States:** No state machine — each evaluation produces one **immutable, append-only** `CartaAssessment` record. `decision` is one of `ALLOW`/`STEP_UP_MFA`/`ISOLATE`/`DENY`, computed once at creation and never transitioned.
* **Valid Transitions:** Not applicable (no update/delete exists on the store interface).

### 5.4 `key-management-svc` — Key
* **Discovered States:** `ENABLED`, `DISABLED`, `PENDING_ROTATION`, `REVOKED` (declared in `domain/types.go`).
* **Actually Used:** Only `ENABLED` (on register) and `DISABLED` (on disable). **`PENDING_ROTATION` and `REVOKED` are dead states — never set by any code path**; there is no distinct "revoke" action, only "disable."
* **Valid Transitions:** `ENABLED → DISABLED` (via `DisableKey`). Rotation does not change state, only increments version/rotation-count metadata.
* **Who Can Perform Them:** Any principal with `KEY_DISABLE` grant (no maker-checker).
* **Invalid/Blocked Transitions:** No scheduled/automatic rotation exists anywhere — rotation is manual-API-call-only.

---

## 6. Events

| Event Type | Producer | Consumer | Trigger | Payload/Purpose | Downstream Effect |
|---|---|---|---|---|---|
| `certificate.issued` | `mtls-management-svc` | `siem-integration-svc` (via internal SIEM client, async/fire-and-forget) | Successful `ProvisionCert` (both normal and bootstrap paths) | Cert ID, tenant, service name | Security telemetry record only — see §9, not actually forwarded onward |
| `certificate.rotated` | `mtls-management-svc` | `siem-integration-svc` | Successful `RotateCert` | Cert ID, tenant | Same as above |
| `certificate.revoked` | `mtls-management-svc` | `siem-integration-svc` | Successful `RevokeCert` | Cert ID, tenant | Same as above |
| `key.rotated` | `key-management-svc` | `siem-integration-svc` | Successful `RotateKey` | Key ID, tenant | Same as above |
| `key.disabled` | `key-management-svc` | `siem-integration-svc` | Successful `DisableKey` | Key ID, tenant | Same as above |

**No events are published by `siem-integration-svc` itself** — it is a terminus, not a producer; events it receives are stored in memory and **never forwarded to any downstream consumer or third-party SIEM platform** (see §9, §11).

**No events are published by `carta-svc`** — confirmed, no Kafka/event-bus usage found anywhere in this service.

**No Kafka is used anywhere in this domain.** All four services communicate exclusively via synchronous HTTP (to `authorization-svc` for grants, and the above SIEM events via an internal async-queue HTTP client to `siem-integration-svc`). The SIEM delivery mechanism is explicitly fire-and-forget/fail-open by design (bounded 256-slot queue, 4 workers, drop-on-full with a counter) — a slow/unreachable `siem-integration-svc` never blocks or fails the triggering request.

---

## 7. Compliance Matrix

| Documented Requirement | Actual Implementation | Status | Evidence/Notes | FE % | BE % | INT % | Overall % | Production Readiness |
|---|---|:---:|---|:---:|:---:|:---:|:---:|:---:|
| **mtls-management-svc:** Certificate issue/rotate/revoke (`03-microservices.md:18`) | Real ECDSA P-256 x509 via genuine CA; verified via `x509.Verify` in tests | ✅ Match | `services/mtls-management-svc/internal/ca/ca.go`; `handler_test.go::TestProvisionRotateRevoke` | — | 90% | — | — | — |
| **mtls-management-svc:** "Automated rotations" (`03-microservices.md:18`) | Not implemented — `AutoRotate` is a stored boolean, never read by any scheduler | ❌ Missing | No cron/ticker/background job found anywhere in the service | — | — | — | — | — |
| **mtls-management-svc:** Bootstrap self-provisioning ("the pilot," `master-register-findings-2026-08-27.md` §3.27) | **Confirmed broken** — 3 tests fail with `401 envelope_incomplete`; platform envelope middleware has no exemption for this path | ❌ Confirmed Defect | `go test ./...` output; corroborated by `deployments/docker-compose.override.yml`'s dated (2026-09-16), user-approved workaround that disables mTLS on `authorization-svc` entirely rather than fixing this path | — | — | 20% | — | — |
| **mtls-management-svc: Service Overall** | — | ⚠️ Partial | See §11/§12 | 5% | 80% | 60% | **48%** | **Not Ready** |
| **siem-integration-svc:** Forward to external SIEM platforms (Splunk/Datadog/Elastic/Sentinel) (`phase-6-trust-intelligence.md:19`) | **Not implemented** — zero outbound HTTP client to any third party anywhere in the service | ❌ Missing | Confirmed by full source read of `handler.go`/`store.go`/`main.go`; `AuthToken` field is collected and stored but never read back or dialed anywhere | — | — | 0% | — | — |
| **siem-integration-svc:** Internal event record-keeping | Solid in-memory CRUD, well-tenant-isolated, well-tested | ✅ Match (undocumented as such) | `go test ./...` clean | — | 80% | — | — | — |
| **siem-integration-svc: Service Overall** | — | ❌ Missing (core purpose) | See §11/§12 | 5% | 55% | 20% | **27%** | **Not Ready** |
| **carta-svc:** Zero-trust access scoring (ALLOW/STEP_UP_MFA/ISOLATE/DENY) (`05-security.md` §5.4) | Real, deterministic scoring algorithm; genuinely consumed by `gateway-auth-svc` in production code | ✅ Match | `internal/domain/types.go`; `gateway-auth-svc/internal/carta/client.go` | — | 90% | — | — | — |
| **carta-svc:** Real device-trust/location signals | 2 of 5 scoring inputs (`device_trust_level`, `is_known_location`) are hardcoded, honestly-labeled placeholders in the real caller — no device-fingerprinting/geo-IP service exists yet | ⚠️ Partial | `gateway-auth-svc/internal/carta/client.go` | — | — | 50% | — | — |
| **carta-svc: Service Overall** | — | ⚠️ Partial | See §11/§12 | 5% | 85% | 55% | **48%** | **Not Ready** |
| **key-management-svc:** BYOK/HYOK key custody (`05-security.md` §8.6, §9.5) | **Self-acknowledged metadata-only** — no key material, encryption, or decryption anywhere in the service, for either model | ❌ Missing, self-acknowledged | `docs/architecture/full-architecture-gap-analysis.md:94,99` explicitly names this as an open gap; corroborated independently by code/test reading | — | — | 0% | — | — |
| **key-management-svc:** Key lifecycle metadata (register/rotate/disable) | 5 of 5 routes fully authz-gated — strongest SoD coverage in the domain | ✅ Match | `authz_test.go` | — | 85% | — | — | — |
| **key-management-svc: Service Overall** | — | ❌ Missing (core purpose) | See §11/§12 | 5% | 55% | 20% | **27%** | **Not Ready** |

---

## 8. Security & Authorization

### 8.1 Authentication
All four services consume gateway-resolved identity headers: `X-Tenant-ID` (verified tenant), `X-Principal-Id` (authenticated actor). Mutating/read requests missing `X-Tenant-ID` are refused with `401` on every route across all four services — each service was found to have previously had a "default-tenant" fabrication bug (a missing header silently defaulting to a shared literal tenant string), **independently found, fixed, and regression-tested in all four services** (`tenant_scope_test.go` in each), per tracker rows 8k (key-management), 8m (mtls-management), and documented equivalents for siem-integration/carta.

### 8.2 Authorization
All four services call `authorization-svc` via `POST /v1/authorize`, failing closed (503) on any transport error or non-200, and refusing (403) on a non-`GRANTED` decision. All four implement a 5-second local decision cache (citing Doc 05 §6.5), caching only real `GRANTED`/`DENIED` outcomes, never "unavailable" outcomes — preserving fail-closed semantics across a transient outage.

**Gated-route coverage by service:**
| Service | Gated Routes | Deliberately Ungated | Rationale for Ungated Routes |
|---|:---:|:---:|---|
| mtls-management-svc | 6 of 6 (excl. bootstrap) | Bootstrap path (service-identity, not principal) | Chicken-and-egg: a service with no prior credential cannot present a principal |
| siem-integration-svc | 3 of 5 | `POST /stream`, `GET /exporters` | 5 internal callers send tenant-only headers; one reports an auth *failure*, so no principal can exist by definition |
| carta-svc | 2 of 3 | `POST /evaluate` | Called mid-authentication by gateway-auth-svc, before any verified identity exists — gating would be circular |
| key-management-svc | 5 of 5 | None | No backward-compatibility constraint applies; this service has no external callers today |

### 8.3 Tenant Isolation
All four services are tenant-scoped (not platform-wide). None has a database/RLS dimension — **all four persist exclusively in memory** (`sync.RWMutex`-guarded Go maps), confirmed by source reading and the absence of any `DATABASE_URL`/migrations directory for any of the four. Tenant isolation is therefore enforced **entirely in application code** (store-layer tenant-ID comparison on every read/write), not backstopped by a database policy. All four have dedicated, passing cross-tenant isolation regression tests.

### 8.4 Privileged Operations
The most consequential unilateral actions in this domain are `RevokeCert` (mtls-management-svc) and `DisableKey` (key-management-svc) — both can break service-to-service trust or encryption access outright. **Neither has a maker-checker/dual-approval control**; a single authorized principal can perform either unilaterally. This is flagged as a gap in §11, not assumed to be acceptable.

### 8.5 Fail-Closed Behavior
Confirmed fail-closed across all four services for authorization-svc unavailability (`503`, never silent success) and for denied decisions (`403`). `carta-svc`'s one real caller (`gateway-auth-svc`) fails **open** at the *client* level if `carta-svc` itself is unreachable — explicitly and correctly scoped as acceptable, since JWT authentication has already gated the request independently by that point.

### 8.6 Security Gaps (Domain-Wide)
1. **🔴 Critical — `mtls-management-svc` bootstrap self-provisioning is broken**, confirmed by 3 failing tests and a dated production workaround (`docker-compose.override.yml`). This is the one path where platform-wide mTLS adoption could begin, and it currently cannot. — **✅ RESOLVED as of 2026-10-12, see §13.1** (found already fixed in the working tree; re-verified by test execution).
2. **🟠 Medium — No certificate revocation distribution** (no CRL/OCSP) — a "revoked" certificate in `mtls-management-svc` remains cryptographically valid to any verifying peer.
3. **🟠 Medium — No maker-checker on `RevokeCert`/`DisableKey`**, the two most destructive actions in the domain.
4. **🟠 Medium — Live third-party credentials collected with no function:** `siem-integration-svc`'s `SIEMExporter.AuthToken` is stored in plaintext in memory and never used for anything, since no outbound egress exists.
5. **🟡 Accepted, documented residual risk:** 2 routes in `siem-integration-svc` and 1 in `carta-svc` accept tenant-header-only calls with no principal, pending a platform-wide mTLS service-identity rollout that itself is currently non-functional (see #1) — this dependency chain means the stated long-term fix for these residual risks is presently blocked by the mTLS bootstrap defect.

---

## 9. Integrations

| Source Service | Target Dependency | Protocol/Port | Purpose | Failure Mode | Status |
|---|---|---|---|---|:---:|
| `mtls-management-svc` | `authorization-svc` | HTTP / 8089 | RBAC grant evaluation | Fails closed, `503` | ⚠️ Live-confirmed `503` — `authorization-svc` itself is platform-wide broken (missing migrations, same `principal_role_assignments.book_id` issue documented throughout this engagement) |
| `mtls-management-svc` | `siem-integration-svc` | HTTP / 8141 (async client) | Security telemetry | Fail-open, fire-and-forget, bounded queue with drop-on-full | ⚠️ Implemented; target service never deployed in this environment, so live delivery was not verified |
| `siem-integration-svc` | `authorization-svc` | HTTP / 8089 | RBAC grant evaluation (3 of 5 routes) | Fails closed, `503` | ❌ Not verified — service never deployed in this environment |
| `siem-integration-svc` | External SIEM (Splunk/Datadog/Elastic/Sentinel) | — | **Documented but does not exist** | — | ❌ **Missing** — no outbound client found anywhere |
| `carta-svc` | `authorization-svc` | HTTP / 8089 | RBAC grant evaluation (2 of 3 routes) | Fails closed, `503` | ❌ Not verified — service never deployed in this environment |
| `gateway-auth-svc` (inbound caller) | `carta-svc` | HTTP / 8142 | Continuous zero-trust access scoring during authentication | Fails **open** at the client (acceptable — JWT auth already gates) | ✅ Verified in `gateway-auth-svc`'s own source — the one genuine, real integration in this domain |
| `key-management-svc` | `authorization-svc` | HTTP / 8089 | RBAC grant evaluation (5 of 5 routes) | Fails closed, `503` | ❌ Not verified — service never deployed in this environment |
| `key-management-svc` | `siem-integration-svc` | HTTP / 8141 (async client) | Security telemetry (key rotate/disable) | Fail-open, fire-and-forget | ❌ Not verified — service never deployed in this environment |
| `key-management-svc` | External KMS/HSM (AWS KMS/Azure Key Vault/GCP KMS/HashiCorp Vault) | — | **Documented but does not exist** | — | ❌ **Missing** — no SDK/client for any provider found anywhere |

**Database dependencies:** **None for any of the four services** — all persist exclusively in memory. This is a confirmed, deliberate, platform-documented design choice (`backend-completion-tracker.md:358`: "Four of these (mtls, siem, key-management, carta) have no Postgres at all"), not an oversight — but it does mean **all certificate, event, assessment, and key-metadata history is lost on every process restart** across the entire domain.

**Required headers (all four services):** `X-Tenant-ID` (mandatory on every route), `X-Principal-Id` (mandatory except the deliberately-ungated routes listed in §8.2), plus the full canonical envelope contract (`X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, `Idempotency-Key`, etc.) on material writes per `ZS-ARCH-SVC-001 v2.0` §4, enforced by shared middleware in each service.

---

## 10. UI Workflow

**Page URL:** `http://localhost:3000/admin/security`
**Navigation:** Console Navigation → (Security & Trust Governance page, shared across all four services plus AI Governance).

**Workflow performed:** logged into the real console via `/api/auth/login` with the documented demo credential; loaded `/admin/security` with the resulting session (confirmed via live Playwright browser automation, not inferred).

| Step | Expected Result | Actual Result |
|---|---|---|
| Page load | Live data for all 4 panels | `200`, page renders fully, title "Security & Trust | Zoiko Suite", no crash/error digest |
| `SecuritySummaryBar` KPI tiles | Real counts from each backend | **Fabricated hardcoded fallback numbers** ("12 Active" certs, "24 Verified" CARTA assessments, "8 Active" KMS keys) — confirmed in source (`SecuritySummaryBar.tsx` lines 75/76/78: `|| 12`, `|| 8`, `|| 24`) to activate specifically when the real fetch fails, which it always does (see below). **mTLS tile only: ✅ RESOLVED 2026-10-12, see §13.2 (New Gap 4)** — the `|| 12` fallback was removed; live re-check shows an honest "0 Active". The CARTA (`|| 24`) and KMS (`|| 8`) fallbacks are unchanged and remain in scope for those services' own tasks. |
| `MtlsManagementPanel` | List of real certificates from mtls-management-svc | Fetches `GET /api/v1/security/certificates` — **this Next.js route does not exist anywhere in the app** (`app/api/`); request 404s, silently caught, panel always shows "No active certificates found." — **✅ RESOLVED 2026-10-12, see §13.2** — the route now exists and calls the real backend client; confirmed live via Playwright. |
| `SiemIntegrationPanel` | List of real SIEM events | Fetches `GET /api/v1/security/events` — **does not exist**; same silent-empty pattern |
| `CartaAssessmentPanel` | List of real CARTA assessments | Fetches `GET /api/v1/security/assessments` — **does not exist**; same silent-empty pattern |
| `KeyManagementPanel` | List of real KMS keys | Fetches `GET /api/v1/security/keys` — **does not exist**; same silent-empty pattern |
| "Issue mTLS Cert" button | Calls `provisionMtlsCertificate` against the real backend | `onClick={() => alert("Issue Certificate dialog: connect to mtls-management-svc")}` — **inert placeholder, no backend call attempted at all** — **✅ RESOLVED 2026-10-12, see §13.2 (New Gap 3)** — now dispatches a real event that opens a modal and submits a real request reaching the real backend; confirmed live via Playwright (request observed reaching `mtls-management-svc`, correctly blocked only by the pre-existing `authorization-svc` outage, §13.3). |
| "Rotate Active Key" button | Calls `rotateKmsKey` against the real backend | `onClick={() => alert("Rotate KMS Key triggered")}` — **inert placeholder, no backend call attempted at all** |
| "Refresh Stream" button | Re-fetch live data | `onClick={() => window.location.reload()}` — a full page reload, not a targeted data refresh |

**Downstream proof:** The real, backend-field-accurate API client (`lib/api/security-trust.ts`, containing `listMtlsCertificates`, `provisionMtlsCertificate`, `rotateMtlsCertificate`, `listMtlsPolicies`, `listSiemEvents`, `listSiemExporters`, `createSiemExporter`, `listKmsKeys`, `registerKmsKey`, `rotateKmsKey`, `disableKmsKey`, `evaluateCartaAccess`, `listCartaAssessments`) was **imported by zero files anywhere in the frontend repository** at the time of the original audit — confirmed by a repository-wide search. It also did not compile: `mtlsManagement`, `siemIntegration`, `carta`, `keyManagement` were never added to `lib/api/config.ts`'s `ServiceName` union, producing 13 TypeScript errors (`npx tsc --noEmit`). **✅ RESOLVED as of 2026-10-12, see §13.1/§13.2** — `config.ts` now has all 4 service entries (found already fixed, uncommitted), the mTLS functions of this client are now genuinely called from the Next.js catch-all route, and `npx tsc --noEmit` shows 0 errors in this file. The SIEM/CARTA/KMS functions in this same file remain unwired (out of scope for this pass).

**Direct live backend verification (bypassing the broken frontend), against the one running service:** `curl http://localhost:8140/healthz` → `200 {"service":"mtls-management-svc","status":"ok"}`. `GET /v1/mtls/certificates` without `legal_entity_id` → `400` (correct validation). With full valid params → `503 {"error":"authorization service unavailable"}` (correct fail-closed behavior — `authorization-svc` is platform-wide broken in this environment). **Re-confirmed unchanged on 2026-10-12** — see §13.3 for the root cause now pinned down to an exact log line (`column pra.book_id does not exist`).

---

## 11. Gaps / Findings

### 11.1 Critical Gaps
1. **`mtls-management-svc` — Bootstrap self-provisioning confirmed broken.** 3 tests fail (`TestProvisionCert_BootstrapToken_SkipsAuthorization`, `TestProvisionCert_WrongBootstrapToken_FallsBackToDeniedAuthz`, `TestProvisionCert_NoBootstrapTokenHeader_FallsBackToDeniedAuthz`), all with `401 envelope_incomplete`. Root cause: the platform-wide canonical-envelope middleware (added after the bootstrap mechanism was built) has no exemption for this one call path. Corroborated by a dated, user-approved production workaround (`deployments/docker-compose.override.yml`, 2026-09-16) that disables mTLS on `authorization-svc` entirely rather than fixing this service. — **✅ RESOLVED as of 2026-10-12, see §13.1.**
2. **`siem-integration-svc` — Documented purpose does not exist in code.** Described in architecture documentation as forwarding to Splunk/Datadog/Elastic/Sentinel; the service is a pure in-memory internal record-keeper with zero outbound integration to any third party.
3. **`key-management-svc` — No actual key custody.** Self-acknowledged in the repository's own gap-analysis documentation as "metadata CRUD only — never actually used to encrypt/decrypt anything." BYOK and HYOK are stored identically as metadata with an external ARN reference; no cryptographic material or operation exists anywhere in the service.
4. **Entire domain frontend is non-functional.** All four panels fetch nonexistent Next.js API routes (`/api/v1/security/*`); the real backend-matching API client is unused dead code; the dashboard's summary KPIs are hardcoded fallback numbers presented as live telemetry; the primary write-action buttons are inert `alert()` stubs. — **`mtls-management-svc`'s portion of this finding is ✅ RESOLVED as of 2026-10-12, see §13.2** (its panel, its route, its KPI tile, and its "Issue mTLS Cert" button are now genuinely wired to the real backend). The `siem-integration-svc`, `carta-svc`, and `key-management-svc` panels, and the shared `SecurityActionHeader`'s other buttons, are unchanged and remain non-functional — out of scope for this pass.

### 11.2 Business-Rule Gaps
1. **No scheduled/automatic rotation** in either `mtls-management-svc` (despite `AutoRotate` being a stored, documented field) or `key-management-svc` — rotation is manual-API-call-only in both.
2. **`carta-svc`'s scoring inputs are partially synthetic.** 2 of 5 signals (`device_trust_level`, `is_known_location`) are honest, labeled placeholders in the real caller (`gateway-auth-svc`), pending a device-fingerprinting/geo-IP capability that does not exist elsewhere in the platform.
3. **`gateway-auth-svc` does not yet enforce `STEP_UP_MFA`** — it logs the decision but has no downstream challenge flow to act on it.

### 11.3 Security/SoD Gaps
1. No maker-checker/dual-approval exists anywhere in this domain for the two most destructive single-principal actions: `RevokeCert` and `DisableKey`.
2. No CRL/OCSP revocation distribution in `mtls-management-svc` — a revoked certificate remains cryptographically valid to any peer not separately querying this service.
3. `siem-integration-svc` stores a live third-party credential (`AuthToken`) in plaintext with no current functional use.
4. Documented, accepted residual exposure on the deliberately-ungated routes in `siem-integration-svc` (2 routes) and `carta-svc` (1 route) — tenant-header-only callers can inject fabricated data or enumerate resource identifiers, pending the (currently broken) mTLS service-identity rollout.

### 11.4 FE/BE Gaps
* **FE:** Complete, domain-wide disconnect between the real backend API client and the actually-rendered UI (see §10/§11.1 item 4). An orphaned legacy component (`CartaCapTablePanel.tsx`, from an earlier, incorrect "CARTA = cap table" interpretation) remains exported though unused.
* **BE:** Generally strong — all four services have passing, well-structured test suites (except mtls-management-svc's 3 bootstrap failures), consistent fail-closed authorization, and independently-fixed-and-regression-tested tenant isolation.

### 11.5 Integration Gaps
1. 3 of 4 services (`siem-integration-svc`, `carta-svc`, `key-management-svc`) have never been deployed in this environment (only defined in an unstarted `docker-compose.phase6.yml`), preventing any live runtime verification for them in this audit.
2. `authorization-svc` is platform-wide broken in this environment (missing migrations), causing every live RBAC check against the one running service (`mtls-management-svc`) to fail with `503` — a cross-service dependency, not owned by any Security & Trust service.
3. `siem-integration-svc` and `key-management-svc` have no real external integration to audit (see Critical Gaps #2/#3).

### 11.6 Documentation Drift
1. `03-microservices.md` claims `mtls-management-svc` performs "automated rotations" and maintains "trust stores" — neither exists in code.
2. `phase-6-trust-intelligence.md` describes `siem-integration-svc` as streaming to named external SIEM platforms — not implemented.
3. `phase-6-trust-intelligence.md`/`05-security.md` describe `key-management-svc` as integrating with named external KMS providers (AWS KMS, Azure Key Vault, GCP KMS, HashiCorp Vault) — no such integration exists; this is independently and explicitly acknowledged elsewhere in the repository's own gap-analysis documentation, so it is drift that the project itself has already flagged, not a new discovery.
4. `docker-compose.yml`'s comment on `gateway-auth-svc`'s mTLS renewal ("renewed at half-life") describes an intended *consumer* behavior that is not implemented by any client in the platform today (the feature "stays off because neither peer has an mTLS listener yet," per the comment itself).

### 11.7 Needs Clarification
1. Whether `mtls-management-svc`'s revocation is intended to be a database-flag-only design (verification delegated to each peer) or is an unaddressed gap — not stated either way in any available documentation.
2. Whether a maker-checker requirement was ever intended for `RevokeCert`/`DisableKey` — no documentation found either requiring or explicitly exempting these actions from dual-control.
3. No authoritative `ZS-ARCH-SVC-*`/`doc7` specification exists for any of the four services to resolve either question against.

---

## 12. Final Domain Summary

| Service | Port | FE % | BE % | INT % | Overall % | Readiness | Critical Remaining Gaps |
|---|---:|---:|---:|---:|---:|---|---|
| **mtls-management-svc** | 8140 | ~~5%~~ **15%** | ~~80%~~ **85%** | 60% | ~~48%~~ **55%** | **Not Ready** | Bootstrap self-provisioning: ✅ resolved 2026-10-12 (§13.1). `CreatePolicy` error handling, frontend dead-wiring, and fabricated mTLS KPI: ✅ fixed 2026-10-12 (§13.2). Remaining: no revocation distribution; no maker-checker on revoke; no automated rotation — all unchanged, see §13.3/§13.4. |
| **siem-integration-svc** | 8141 | 5% | 55% | 20% | **27%** | **Not Ready** | No real SIEM forwarding exists despite being the service's documented, named purpose; pure in-memory, lost on restart; never deployed in this environment; frontend dead-wired |
| **carta-svc** | 8142 | 5% | 85% | 55% | **48%** | **Not Ready** | Real, well-built, genuinely consumed by `gateway-auth-svc` — but 2 of 5 scoring inputs are placeholders and `STEP_UP_MFA` is not yet enforced downstream; never deployed in this environment; frontend dead-wired |
| **key-management-svc** | 8143 | 5% | 55% | 20% | **27%** | **Not Ready** | Self-acknowledged metadata-only — no actual key custody for BYOK/HYOK; no maker-checker on disable; never deployed in this environment; frontend dead-wired |
| **Security & Trust Total** | — | ~~5%~~ **8%** | ~~69%~~ **70%** | **39%** | ~~38%~~ **39%** | **Not Ready** | See domain-wide findings below. Total recomputed 2026-10-12 after the `mtls-management-svc` remediation pass (§13); `siem-integration-svc`/`carta-svc`/`key-management-svc` rows are unchanged from the original audit. |

### Domain-Wide Observations
* **Total services discovered/audited:** 4 of 4.
* **Readiness breakdown:** 0 Ready, 0 Ready with Minor Gaps, 4 Not Ready.
* **Common security issues:** platform-wide `authorization-svc` outage blocks live RBAC verification for whichever service is reachable; an identical historical "default-tenant fabrication" bug was independently found, fixed, and regression-tested across all four services (a genuine remediation success, not a current gap).
* **Common FE/BE/Integration gaps:** Frontend is uniformly and completely disconnected from the backend across the entire domain (one root cause — a mocked API surface never reconciled with the real, separately-built backend client — four identical symptoms). Backend code quality is consistently strong (passing tests, fail-closed authorization, tenant isolation) but each service's core differentiating promise is either broken (mtls bootstrap), entirely unbuilt (SIEM forwarding, key custody), or partially placeholder (CARTA's signal inputs).
* **SoD issues:** no maker-checker exists anywhere in this domain for the two most destructive actions (certificate revoke, key disable).
* **Integration/dependency issues:** `authorization-svc` migration gap (platform-wide, not domain-owned); 3 of 4 services have never been deployed in this environment at all.
* **Documentation gaps:** no dedicated functional specification exists for any of the four services; several documentation-vs-implementation mismatches found, two of which (SIEM forwarding, key custody) are already honestly acknowledged elsewhere in the repository's own architecture gap-analysis documents.
* **Cross-service positive finding:** all four services vendor an identical, correctly-engineered async/fail-open SIEM client package, ported platform-wide after an 850ms synchronous-blocking defect was originally found and fixed in `authorization-svc` — genuine evidence of cross-service remediation discipline, standing in contrast to the frontend finding above.

---

## 13. `mtls-management-svc` Remediation Pass — 2026-10-12

This section records the outcome of a dedicated "implement confirmed gaps + discover new gaps" pass against `mtls-management-svc` specifically, performed after the domain audit above. **No other service in this domain was modified.** The original findings above are left intact (per this engagement's no-silent-rewrite discipline) and annotated in place with pointers to this section; nothing here deletes or contradicts the original audit's findings — it records what has changed since.

### 13.1 Previously-Flagged Gap Re-Verification

**Original Gap (§11.1 #1):** Bootstrap self-provisioning confirmed broken — `TestProvisionCert_BootstrapToken_SkipsAuthorization`, `TestProvisionCert_WrongBootstrapToken_FallsBackToDeniedAuthz`, `TestProvisionCert_NoBootstrapTokenHeader_FallsBackToDeniedAuthz` all failing with `401 envelope_incomplete`; root cause identified as `internal/envelope/contract.go` having no `ExemptPaths` entry for `/v1/mtls/certificates`.

**Verification:** Re-ran all 3 named tests individually (`-run "TestProvisionCert_BootstrapToken_SkipsAuthorization|TestProvisionCert_WrongBootstrapToken_FallsBackToDeniedAuthz|TestProvisionCert_NoBootstrapTokenHeader_FallsBackToDeniedAuthz" -v`) — all **PASS**. Ran the full `internal/handler` suite — all **PASS**. Ran `go clean -testcache` then a full clean `go build ./...` / `go vet ./...` / `go test ./... -count=1 -v` across all 3 packages (`authz`, `handler`, `siem`) — all clean, zero failures.

**Root-cause check:** `git diff -- services/mtls-management-svc/internal/envelope/contract.go` shows `ExemptPaths: []string{"/v1/mtls/certificates"}` already present as an **uncommitted** working-tree change — not authored in this session, not present (by this audit's own evidence) at the time of the original domain audit. Something external to this engagement applied the exact fix between the domain audit and this pass.

**Current Status:** ✅ **Resolved** — found already fixed in the working tree, confirmed live by test execution. Not claimed as new authorship.

**Evidence:** `go test ./... -count=1 -v` (all packages `ok`); `git diff -- services/mtls-management-svc/internal/envelope/contract.go`.

### 13.2 Newly Discovered & Fixed Gaps

**New Gap 1 — `CreatePolicy` discarded its store error (service-owned defect).**
- **Discovery Evidence:** `internal/handler/handler.go`'s `CreatePolicy` executed `_ = h.store.CreatePolicy(r.Context(), tenantID, pol)`, discarding any error and unconditionally responding `201` — inconsistent with every other write handler in the file (e.g. `ProvisionCert` checks `h.ca.IssueLeaf`/`h.store.CreateCert`).
- **Remediation:** Changed to check the error and return `500` on failure, matching the file's established pattern.
- **Verification:** Added `TestCreatePolicy_StoreFailure_Returns500NotFalse201` (a `failingPolicyStore` test double embedding `*store.MemoryStore`, overriding only `CreatePolicy` to force an error) — **PASS**. Full suite re-run after the fix — all tests across all 3 packages **PASS**; `go build`/`go vet` clean.
- **Current Status:** ✅ Fixed and regression-tested. `MemoryStore.CreatePolicy` itself can never currently fail, so this has no live behavioral effect today — a defense-in-depth consistency fix, not a live-bug fix.

**New Gap 2 — `listMtlsPolicies` (frontend API client) could never succeed against the real backend.**
- **Discovery Evidence:** `lib/api/security-trust.ts`'s `listMtlsPolicies` sent `tenant_id` as a query parameter and never passed `identity`, so it never sent the `X-Tenant-ID`/`X-Principal-Id` headers the real `ListPolicies` handler (`handler.go:455-463`) strictly requires (`401` if absent) — this call would have failed every time it was ever invoked.
- **Remediation:** Changed `listMtlsPolicies` to accept and forward `identity`, exactly like the other 3 mTLS functions in the same file (`listMtlsCertificates`, `provisionMtlsCertificate`, `rotateMtlsCertificate`) already correctly do.
- **Verification:** `npx tsc --noEmit` — 0 errors in `security-trust.ts` (unchanged total elsewhere: 21, all pre-existing and unrelated). Confirmed via repo-wide search that this function has zero callers anywhere in the frontend, so the fix carries no risk of behavioral regression to any live caller.
- **Current Status:** ✅ Fixed. Still dead code (no panel wires it in) — this corrects a latent defect for its eventual first caller; it cannot be live-verified end-to-end until a UI caller exists, which was not invented.

**New Gap 3 — "Issue mTLS Cert" panel hardcoded an invalid `legal_entity_id` literal, defeating the real session identity.**
- **Discovery Evidence:** Live Playwright run (logged into the real console with the documented demo credential, clicked "Issue mTLS Cert", submitted the form) showed the request reaching the real backend but failing `503 authorization service unavailable`. Root-caused by directly `curl`-ing `mtls-management-svc` and `authorization-svc` with the exact request shape: `MtlsManagementPanel.tsx`'s `handleIssueCert` POSTed a hardcoded `legal_entity_id: "LE-001"` (not a UUID), which took precedence over the real session's `legal_entity_id` in the Next.js route's `(body.legal_entity_id as string) || identity.legalEntityId || "LE-001"` fallback chain, causing `authorization-svc` to reject the request with `400 invalid_scope` before it could ever return a real `GRANTED`/`DENIED` decision — permanently masking the real authorization outcome behind a malformed-input error.
- **Remediation:** Removed the hardcoded `legal_entity_id: "LE-001"` from the panel's request body, so the route correctly falls through to the logged-in session's real `legal_entity_id` (a genuine UUID).
- **Verification:** Re-ran the identical live Playwright flow after the fix — the request now reaches `authorization-svc` with a well-formed UUID and fails for the *correct*, already-documented reason (`store_unavailable`, see §13.3), not the masking `400`/`503` conflation. Confirmed directly via `docker logs authorization-svc`: `ERROR: column pra.book_id does not exist (SQLSTATE 42703)` in `FindGrantedActionsScoped` — the same platform-wide migration gap documented domain-wide and repository-wide throughout this engagement.
- **Current Status:** ✅ Fixed (service-owned frontend defect corrected). End-to-end success is still blocked by the pre-existing, out-of-scope `authorization-svc` migration gap (§13.3) — not by this service.

**New Gap 4 — `SecuritySummaryBar`'s mTLS KPI fabricated a fallback count of "12 Active" certificates.**
- **Discovery Evidence:** `SecuritySummaryBar.tsx:75` computed `activeCerts` as `certs.filter(...).length || certs.length || 12`. Since the real certificate list is currently always empty (§13.3), the KPI tile permanently displayed "12 Active" / "12 total managed certs" as live telemetry — matching the original domain audit's Critical Gap #4 finding verbatim.
- **Remediation:** Removed the `|| 12` fallback (and the matching `|| 12` in the "total managed certs" sub-label). The KMS (`|| 8`) and CARTA (`|| 24`) fallbacks in this same shared file were deliberately left untouched — they belong to `key-management-svc`'s and `carta-svc`'s own scope, not this one.
- **Verification:** Live Playwright re-check of `/admin/security` after the fix shows the mTLS KPI tile now reads **"0 Active" / "0 total managed certs (:8140)"** — the honest current count.
- **Current Status:** ✅ Fixed for the mTLS tile specifically.

**New Gap 5 — `ListCerts`/`ListPolicies` discarded their store errors (found during the second fresh scan, service-owned defect).**
- **Discovery Evidence:** A second fresh scan after the first round of fixes (re-reading the full handler file end-to-end) found the same error-discarding pattern as New Gap 1, in two more places: `ListCerts` (`certs, _ := h.store.ListCerts(...)`) and `ListPolicies` (`pols, _ := h.store.ListPolicies(...)`). A real store failure in either would have been silently converted into a false, empty `200` response — indistinguishable from "this tenant genuinely has zero certificates/policies," which is a worse failure mode than New Gap 1's (a misleading successful *read* rather than a misleading successful *write*).
- **Remediation:** Both now check the error and return `500` on failure, before falling back to an empty slice only when the store genuinely returned none.
- **Verification:** Added `TestListCerts_StoreFailure_Returns500NotFalseEmpty200` and `TestListPolicies_StoreFailure_Returns500NotFalseEmpty200` (a `failingListStore` double forcing both methods to error) — both **PASS**. Full suite re-run — all 21 tests across 3 packages **PASS**; `go build`/`go vet` clean.
- **Current Status:** ✅ Fixed and regression-tested. Same as New Gap 1: `MemoryStore`'s list methods can never currently fail, so this is a defense-in-depth consistency fix with no live behavioral effect today.

### 13.3 Confirmed Still-Present, Out-of-Scope Conditions (re-verified, not modified)
- `authorization-svc`'s migration gap is **still live**, re-confirmed in this exact pass by direct `curl` and `docker logs authorization-svc` (`ERROR: column pra.book_id does not exist (SQLSTATE 42703)`, `FindGrantedActionsScoped`, `pg_store.go:1493`). This blocks every real `GRANTED`/`DENIED` decision platform-wide, including for `mtls-management-svc`'s now-correctly-wired "Issue mTLS Cert" flow. **CROSS-SERVICE DEPENDENCY — not modified**, per this task's explicit instruction not to touch `authorization-svc`.
- Separately: the demo/test identity the console uses (`principal_id 33333333-3333-3333-3333-333333333333`, role "Console Demo Operator") holds **no permission bundle granting any mtls-management-svc action** anywhere in `authorization-svc`'s live data (confirmed by directly reading `permission_bundles`/`roles`). Even once the migration gap above is fixed, this specific demo principal would still receive `DENIED`, not `GRANTED`, for certificate provisioning. **ENVIRONMENT LIMITATION (missing RBAC seed data)** — not fabricated or worked around; creating a permission grant to force a passing demo was explicitly out of scope.
- CRL/OCSP absence, no automated rotation, no maker-checker on `RevokeCert` — all re-confirmed still present, unchanged from the original audit, and correctly left alone as NEEDS CLARIFICATION / DOCUMENTATION DRIFT rather than invented into new features.

### 13.4 Updated Score — `mtls-management-svc`

| | FE % | BE % | INT % | Overall % | Readiness |
|---|---:|---:|---:|---:|---|
| Before this pass | 5% | 80% | 60% | 48% | Not Ready |
| After this pass | 15% | 85% | 60% | **55%** | **Not Ready** |

**Rationale:**
- **BE 80%→85%:** bootstrap path confirmed working (already fixed, verified) + 3 discarded-store-error defects fixed and regression-tested (`CreatePolicy`, `ListCerts`, `ListPolicies` — the latter two found during the second fresh scan). All three are the same no-live-impact consistency-fix category, so they are counted together rather than compounding the score further. Remaining gaps (no CRL/OCSP, no scheduled rotation, no maker-checker on `RevokeCert`) are unchanged and keep this below 90%.
- **FE 5%→15%:** the mTLS panel's dead-wiring is genuinely resolved — "Issue mTLS Cert" opens a real modal, submits a real request that reaches the real backend through a real Next.js route (previously 404), and the KPI tile reports an honest count instead of a fabricated one. Still capped well below 50% because the panel's "Rotate" action and the rest of the Security & Trust dashboard (SIEM/CARTA/KMS panels, the shared `SecurityActionHeader`'s other buttons) remain exactly as broken as the original audit found them — explicitly out of scope here — and the one wired flow cannot be shown to succeed end-to-end in this environment (§13.3).
- **INT 60% unchanged:** the one live dependency (`authorization-svc`) is exactly as broken as before, for the same out-of-scope reason; the SIEM async client was already correctly implemented and is unchanged.

This section does not alter any finding for `siem-integration-svc`, `carta-svc`, or `key-management-svc` — those three services were not in scope for this pass, and their sections and scores elsewhere in this document are unchanged.
