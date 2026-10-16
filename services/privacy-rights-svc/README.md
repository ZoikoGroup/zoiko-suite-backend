# privacy-rights-svc (PRV-04)

**Default Port:** `8154`  
**Classification:** Independent Service (Self-Contained Case Evidence)  
**Specification:** `ZS-SVC-W-001` (§14, §15, §18, §18.1, §31, §32)  
**Role:** Data Rights, Complaint & Disclosure Control Service. Owns the privacy meaning and immutable legal evidence of data subject rights (DSR) requests, complaints, identity-assurance events, discovery manifests, and disclosure governance.

---

## Architecture & Responsibilities

`privacy-rights-svc` manages the end-to-end lifecycle and evidence collection for statutory data subject rights requests:
1. **Case Intake:** Records requests across 8 statutory and regulatory right families:
   - `ACCESS` (GDPR Art. 15)
   - `RECTIFICATION` (GDPR Art. 16)
   - `ERASURE` (GDPR Art. 17)
   - `RESTRICTION` (GDPR Art. 18)
   - `PORTABILITY` (GDPR Art. 20)
   - `OBJECTION_WITHDRAWAL` (GDPR Art. 21)
   - `AUTOMATED_DECISION_CHALLENGE` (GDPR Art. 22)
   - `COMPLAINT` (Formal dispute / supervisory referral)
2. **Identity Assurance Evidence:** Records append-only identity verification attempts (both successful and failed attempts are recorded for auditing).
3. **Discovery Manifests:** Records signed/hashed discovery manifests attached by domain service adapters (content hash, candidate count, domain, evidence ref).
4. **Disclosure Gate (§15.2):** Strictly enforces that closing a request as `FULFILLED` requires verified identity AND at least one discovery manifest.
5. **Response Package Versioning (I21):** Increments `response_package_version` upon approved response/fulfilment; post-approval changes require re-versioning.
6. **Workflow Orchestration Linkage:** Supports optional caller-supplied `wfc_process_ref` binding a case to long-running `workflow-svc` execution without taking on orchestration itself (§14.1).

---

## Architectural Invariants

### 1. §15.2 Disclosure Gate
> *"Finding a record is only discovery. Disclosure requires identity assurance, applicable-right determination, scope review, exemptions, third-party protection, redaction and approved response assembly."*

Closing a request with outcome `FULFILLED` requires:
- `identity_verified = true` (HTTP 422 `PRV-012: IDENTITY_ASSURANCE_INSUFFICIENT` if missing)
- At least one attached discovery manifest (HTTP 422 `PRV-013: THIRD_PARTY_REVIEW_REQUIRED` if missing)

Closing as `REJECTED` or `WITHDRAWN` does not require these preconditions, as the case did not reach disclosure/fulfilment.

### 2. Database Immutability & Append-Only Triggers
Enforced at the PostgreSQL level via trigger functions:
- `rights_requests_closed_immutable`: Once a case has `status = 'CLOSED'`, any subsequent `UPDATE` is aborted by trigger `reject_closed_request_mutation()`.
- `identity_verification_events_append_only`: Any `UPDATE` or `DELETE` on identity evidence is aborted by `reject_evidence_mutation()`.
- `discovery_manifests_append_only`: Any `UPDATE` or `DELETE` on discovery manifests is aborted by `reject_evidence_mutation()`.

### 3. Tenant Isolation & Row-Level Security (RLS)
PostgreSQL Row-Level Security (`FORCE ROW LEVEL SECURITY`) is applied to all tables:
- `rights_requests`
- `identity_verification_events`
- `discovery_manifests`
- `rights_idempotency_keys`

Tenant isolation is verified fail-closed via `tenant_isolation_policy` comparing `tenant_id` to `app.tenant_id`.

The verified `X-Tenant-Id` is **required on every request** to the rights routes (enforced at the transport boundary by `internal/middleware`); a missing, empty or whitespace-only value is rejected with HTTP 401 before any handler runs. Only the trimmed value is used as the transaction-local `app.tenant_id`.

### 4. §18.1 Idempotency Key Handling
All mutating APIs **require** the `Idempotency-Key` header; a missing or blank value is rejected with HTTP 400 before any side effect:
- First execution stores the request hash, status, and response body in `rights_idempotency_keys`.
- Retried execution with the same key and identical payload returns the cached response with `Idempotency-Replay: true`.
- Retried execution with the same key but differing payload returns HTTP 409 Conflict with `PRV-020: IMMUTABLE_EVIDENCE_CONFLICT`.
- Authorization is evaluated **before** the idempotency lookup, so a replayed key can never bypass the authorization decision.

### 5. §18.1 Request Envelope Headers
Every mutating API requires the following service-boundary headers, validated after authentication and before any idempotency lookup/persistence, database mutation or event publication:

| Header | Requirement | On violation |
|---|---|---|
| `X-Tenant-Id` | non-empty, non-whitespace tenant scope | HTTP 401 `{"error":"X-Tenant-Id header is required"}` |
| `Idempotency-Key` | non-empty, non-whitespace | HTTP 400 `{"error":"Idempotency-Key header is required"}` |
| `X-Correlation-ID` | non-empty, non-whitespace | HTTP 400 `{"error":"X-Correlation-ID header is required"}` |
| `X-Principal-Id` | non-empty actor context | HTTP 401 `{"error":"X-Principal-Id header is required"}` |

### 6. §19 Event Alignment
`ZS-SVC-W-001` §19 declares no rights-intake or rights-closure event. The service therefore publishes **no** domain events for `RECEIVED`/`CLOSED` transitions (previously emitted `privacy.rights_request.received` / `privacy.rights_request.closed`, which are not in the §19 catalogue, have been removed). The only rights-related §19 event, `privacy.rights.action_required` (payload: request, domain, action type/scope, deadline/WFC ref; consumers: domain adapter + WFC evidence collector), is not emitted by this service — its production is WFC/domain-orchestration territory per §14.1, not PRV-04's own privacy-meaning state.

### 7. §32 Code Usage Notes
`PRV-001..PRV-020` are used only for the conditions §32 actually names. §32 declares **no** request-not-found code and **no** input-validation code, so:
- a missing rights request returns HTTP 404 with a plain structured error (`{"error":"rights request not found"}`), never `PRV-001`;
- caller input errors return HTTP 400 with a plain structured error, never `PRV-005`.

The §15.2 disclosure gate continues to use `PRV-012`/`PRV-013`, double-close/immutability uses `PRV-020`, and tenant mismatch uses `PRV-003`.

### 8. §18 Canonical Response Fields
The §18 API table's critical response fields are exposed additively (existing fields are unchanged):
- **Create / request responses:** `classification` (`{right_family, jurisdiction}`, derived from the stored record) and `identity_state` (`VERIFIED` / `UNVERIFIED`, derived from `identity_verified`).
- **Discovery-manifest responses:** `hash` (the §18 name for the manifest `content_hash`).

---

## API Surface

Both canonical `/privacy/rights-requests` and versioned `/v1/privacy/rights-requests` routes are supported:

| Method | Route | Description | Auth / Gating |
|---|---|---|---|
| `POST` | `/privacy/rights-requests` | Create rights request / case intake | `PRIVACY_RIGHTS_REQUEST_CREATE` |
| `GET` | `/privacy/rights-requests` | List requests by subject (`?subject_ref=...`) | Tenant verified |
| `GET` | `/privacy/rights-requests/{id}` | Get request by ID | Tenant verified |
| `POST` | `/privacy/rights-requests/{id}/identity-verification` | Record identity assurance event | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/discovery-manifests` | Attach domain discovery manifest | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `GET` | `/privacy/rights-requests/{id}/discovery-manifests` | List discovery manifests | Tenant verified |
| `POST` | `/privacy/rights-requests/{id}/wfc-process-ref` | Attach workflow-svc process reference | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/close` | Close case (enforces §15.2 Disclosure Gate) | `PRIVACY_RIGHTS_REQUEST_CLOSE` |

---

## Health & Probes

- Liveness: `GET /healthz` -> `{"status":"ok"}` (HTTP 200)
- Readiness: `GET /readyz` -> `{"status":"ready"}` (HTTP 200, checks PostgreSQL connection pool)
