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

### 4. §18.1 Idempotency Key Handling
All mutating APIs support the `Idempotency-Key` header:
- First execution stores the request hash, status, and response body in `rights_idempotency_keys`.
- Retried execution with the same key and identical payload returns the cached response with `Idempotency-Replay: true`.
- Retried execution with the same key but differing payload returns HTTP 409 Conflict with `PRV-020: IMMUTABLE_EVIDENCE_CONFLICT`.

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
