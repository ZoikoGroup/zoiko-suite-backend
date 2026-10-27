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

What this service **verifies** before it will close a case as `FULFILLED` (checked atomically, under a row lock, in the same transaction as the close):
- a **verified identity-assurance event actually exists** for the case (`verified = true` row in `identity_verification_events`) — the denormalised `identity_verified` flag alone is not trusted (HTTP 422 `PRV-012: IDENTITY_ASSURANCE_INSUFFICIENT`);
- at least one discovery manifest is attached (HTTP 422 `PRV-013: THIRD_PARTY_REVIEW_REQUIRED`). A manifest only counts if it was accepted with a real SHA-256 digest (`content_hash` = 64 hex characters, optionally `sha256:`-prefixed) and a non-negative `candidate_count`; anything else is refused at attach time with 400 `VALIDATION_FAILED`.

Closing as `REJECTED` or `WITHDRAWN` carries no such precondition.

What this service **cannot verify** and therefore does **not** claim to enforce — applicable-right determination, scope review, exemptions, third-party protection, redaction and approved response assembly. §14.1 gives these to WFC/reviewers and domain adapters, and this service has no data model for them (inventing one would be inventing requirements). They are `CROSS-SERVICE / EXTERNAL DEPENDENCY` items: the gate here is a **necessary**, not a sufficient, condition for disclosure.

### 2. Database Immutability & Append-Only Triggers
Enforced at the PostgreSQL level via trigger functions:
- `rights_requests_closed_immutable`: Once a case has `status = 'CLOSED'`, any subsequent `UPDATE` is aborted by trigger `reject_closed_request_mutation()`.
- `identity_verification_events_append_only`: Any `UPDATE` or `DELETE` on identity evidence is aborted by `reject_evidence_mutation()`.
- `discovery_manifests_append_only`: Any `UPDATE` or `DELETE` on discovery manifests is aborted by `reject_evidence_mutation()`.

### 3. Tenant Isolation & Row-Level Security (RLS)
`FORCE ROW LEVEL SECURITY` is applied to `rights_requests`, `identity_verification_events`, `discovery_manifests` and `rights_idempotency_keys`. Since migration `000005` **no policy has a `tenant_id IS NULL` exception**: a row is visible and writable only when its `tenant_id` equals the transaction-local `app.tenant_id`, so a row with no tenant is visible to nobody (it used to be visible to, and writable by, every tenant).

- The verified `X-Tenant-Id` is required on every request: the `TenantContext` middleware refuses a missing or whitespace-only tenant with 401 `TENANT_SCOPE_REQUIRED` before any handler runs (after the principal check — see [§5](#5-181-request-envelope-headers)), each handler repeats the check, and the store itself refuses to open a transaction without one.
- A body `tenant_id` that differs from the verified header is `403 TENANT_SCOPE_MISMATCH`; a case is always stamped with the verified tenant.
- **Migration 000005 never deletes or rewrites a row.** It adds `CHECK (tenant_id IS NOT NULL) NOT VALID` (new NULL-tenant rows are refused) and sets `NOT NULL` only if no legacy NULL row exists. Legacy NULL-tenant rows are left in place (closed cases and evidence are immutable/append-only), are invisible to the application role, and raise a `WARNING` with the count — they need an operator-assigned owner: **BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING.** No tenant is guessed.
- There is no platform scope in this service's documentation, so none is offered: the platform-scope sentinel is just another tenant id here.

### 4. §18.1 Idempotency
All mutating APIs require `Idempotency-Key` (`X-Idempotency-Key` is accepted as an alias and honoured everywhere, including the replay lookup):
- **Scope:** (tenant, principal, key). A key never replays another principal's response.
- **Binding:** the stored hash covers method + path (the `/v1` alias prefix is normalised) + body. The same key on a different operation or payload is `409` `PRV-020`.
- **Order:** authentication → tenant → correlation/key presence → authorization → replay lookup → validation → execution. Authorization precedes the replay, so a revoked grant cannot replay a cached success. The first two steps are applied by the `TenantContext` middleware before any handler runs, in this order (principal first, then tenant), and are repeated by each handler.
- **Atomic:** the key claim, the business change and the stored replay response are written in **one transaction**. If any step fails nothing persists, so a retry re-executes cleanly; concurrent identical requests execute exactly once (the loser replays the winner's response).
- **Fail closed:** an idempotency lookup failure refuses the request (`503 STORE_UNAVAILABLE`) — it never proceeds without the lookup — and a failed save fails the whole operation instead of being ignored.
- Replay returns the original body with `Idempotency-Replay: true`. Refused attempts (4xx) are not stored.

### 5. §18.1 Request Envelope Headers
| Header | Requirement | On violation |
|---|---|---|
| `X-Principal-Id` | non-empty actor (every route, reads included) | 401 `AUTHENTICATION_FAILED` |
| `X-Tenant-Id` | non-empty, non-whitespace tenant scope (every route) | 401 `TENANT_SCOPE_REQUIRED` |
| `X-Correlation-ID` | mutating routes | 400 `CORRELATION_ID_REQUIRED` |
| `Idempotency-Key` | mutating routes | 400 `IDEMPOTENCY_KEY_REQUIRED` |

**Where the first two checks run.** `X-Principal-Id` and `X-Tenant-Id` are enforced by the `TenantContext` middleware, which wraps every API route and runs *before* the handler, and again by each handler. The middleware applies them in the documented order: a request with neither header is `401 AUTHENTICATION_FAILED` (principal first), one with a principal but no tenant is `401 TENANT_SCOPE_REQUIRED`, and the handler never runs without a verified tenant. `X-Correlation-ID`, when present, is also placed on the request context so the authorization call can carry it.

Body validation happens **after** authentication and authorization, so an unauthenticated caller gets 401 (and an unauthorized one 403) regardless of the body.

### 6. Events
`ZS-SVC-W-001` §19 declares no rights-intake or rights-closure event, so this service emits **none** (a guard test fails if event machinery is reintroduced). `PRIVACY.md` previously listed `privacy.rights_request.received` / `.closed`; that was stale and has been corrected. The only rights-related §19 event, `privacy.rights.action_required` (request, domain, action type/scope, deadline/WFC ref → domain adapter + WFC evidence collector), is **not** emitted: which service produces it is not stated by the specification (§14.1 gives orchestration to WFC) — NEEDS CLARIFICATION. If it is assigned to this service, emission must go through a transactional outbox (see privacy-purpose-registry-svc for the pattern). The `KAFKA_*` configuration keys are still parsed but unused.

### 7. Error contract
**Every error the service itself produces — from a handler or from the `TenantContext` middleware — is `{"error": <message>, "error_code": <code>}`.** §32 conditions keep their §32 code and long message; the rest use local symbolic codes.

**Responses generated by the framework or the HTTP server are outside that envelope.** They are not service code and no error code is invented for them:

| Response | Produced by | What the client gets |
|---|---|---|
| Unknown route | chi router | `404`, `text/plain`, body `404 page not found` |
| Known path, unsupported method | chi router | `405`, no body, with an `Allow` header |
| A panic inside a handler | chi `Recoverer` | `500`, no body (the panic is logged) |
| `/readyz` failure (no pool, or the database does not answer) | `health.ReadyzHandler` | `503`, `text/plain`, a JSON-looking body `{"status":"unavailable","reason":"…"}` |
| A request `net/http` rejects before routing (malformed request line, over-large headers) | Go HTTP server | `net/http`'s own plain-text response |


| Condition | Status | `error_code` |
|---|---|---|
| Missing principal / tenant | 401 | `AUTHENTICATION_FAILED` / `TENANT_SCOPE_REQUIRED` |
| Not authorized / authorization-svc unreachable | 403 / 503 | `AUTHORIZATION_DENIED` / `AUTHORIZATION_SERVICE_UNAVAILABLE` |
| Missing correlation id / idempotency key | 400 | `CORRELATION_ID_REQUIRED` / `IDEMPOTENCY_KEY_REQUIRED` |
| Invalid body or field (incl. non-SHA-256 `content_hash`) | 400 | `VALIDATION_FAILED` |
| Body `tenant_id` ≠ verified tenant | 403 | `TENANT_SCOPE_MISMATCH` |
| Case not found (or another tenant's) | 404 | `NOT_FOUND` |
| Closed case written / key reused for a different request | 409 | `PRV-020` |
| FULFILLED without verified identity | 422 | `PRV-012` |
| FULFILLED without a discovery manifest | 422 | `PRV-013` |
| Store or idempotency-lookup failure | 503 | `STORE_UNAVAILABLE` |

§32 has no not-found or validation code, so neither borrows a `PRV-` code.

### 8. §18 Canonical Response Fields
- **Create / request responses:** `classification` and `identity_state`.
- **Discovery-manifest responses:** `hash` (the §18 name for `content_hash`).

### 9. Authorization client

`internal/authz` calls `POST {AUTHZ_SERVICE_URL}/v1/authorize` with the body `{principal_id, legal_entity_id, action_type}` and treats anything other than HTTP 200 with `decision_outcome: GRANTED` as a refusal (`403` for a denial, `503` — fail closed — for an unreachable or non-200 authorization-svc). It sends the same identity and envelope headers as the sibling privacy services, so authorization-svc is not asked to admit an anonymous call and keeps working if it enforces its request envelope strictly:

| Header | Value |
|---|---|
| `X-Principal-Id` | the verified principal |
| `X-Legal-Entity-Id` | the verified tenant (the scope being checked) |
| `X-Tenant-Id` | the verified tenant from the request context (the scope argument if the context has none) |
| `X-Request-Id` | a fresh UUID per check |
| `X-Correlation-ID` | the caller's `X-Correlation-ID` when it sent one (mutating routes), else a fresh UUID |
| `X-Source-Channel` | `web` |
| `Idempotency-Key` | a fresh UUID per check — never the caller's own key, so a check is evaluated every time and an earlier decision cannot be replayed after a grant is revoked |

This changes no permission: the actions remain `PRIVACY_RIGHTS_REQUEST_CREATE`, `_PROCESS` and `_CLOSE`. Behaviour against a live authorization-svc has not been verified.

---

## API Surface

Both `/privacy/rights-requests` and `/v1/privacy/rights-requests` are mounted.

| Method | Route | Description | Grant (on the verified tenant) |
|---|---|---|---|
| `POST` | `/privacy/rights-requests` | Create case | `PRIVACY_RIGHTS_REQUEST_CREATE` |
| `GET` | `/privacy/rights-requests?subject_ref=` | List cases by subject | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `GET` | `/privacy/rights-requests/{id}` | Get case | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/identity-verification` | Record identity assurance | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/discovery-manifests` | Attach manifest | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `GET` | `/privacy/rights-requests/{id}/discovery-manifests` | List manifests | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/wfc-process-ref` | Attach workflow reference | `PRIVACY_RIGHTS_REQUEST_PROCESS` |
| `POST` | `/privacy/rights-requests/{id}/close` | Close (disclosure gate) | `PRIVACY_RIGHTS_REQUEST_CLOSE` |

Authorization is decided on the caller's verified tenant **before** any lookup, so 404-vs-403 cannot be used to probe case ids. **The specification names no read permission**, and none was invented: the three read routes reuse the existing `PRIVACY_RIGHTS_REQUEST_PROCESS` action (reading a case is part of processing it). Whether a dedicated read action is wanted is a NEEDS CLARIFICATION item; the grant itself lives in `authorization-svc` (external).

---

## External dependencies / blockers

- **authorization-svc** — grants for the three actions above must be provisioned there (not this service's). Not verified against a live authorization-svc.
- **WFC / reviewers / domain adapters** — applicable-right determination, scope review, exemptions, third-party protection, redaction, approved response assembly (§14.1, §15.2).
- **Operator tenant mapping** — legacy NULL-tenant rows (see §3).
- **Needs clarification** — read-route action; who emits `privacy.rights.action_required`; signed (asymmetric) discovery manifests.

---

## Running tests

```bash
go test ./...            # DB-backed tests SKIP without TEST_DATABASE_URL
go vet ./... && go build ./...
```
Real PostgreSQL (a throwaway database per test; the app connects as a `NOSUPERUSER NOBYPASSRLS` role):
```powershell
$env:TEST_DATABASE_URL="postgres://postgres:pgsuper@localhost:55432/postgres?sslmode=disable"
$env:REQUIRE_DB_TESTS="1"
go test -count=1 -v ./...
```
Latest run: **with** the database 66 top-level tests pass, 0 skipped (handler 50, store 8, authz 6, middleware 2); **without** it 58 pass and 8 are skipped (the store tests) — a run without a database proves nothing about RLS, atomicity or the migrations. Not performed: Docker rebuild, live compose stack, live authorization-svc, frontend.

---

## Health & Probes

- Liveness: `GET /healthz` -> `{"status":"ok"}` (HTTP 200)
- Readiness: `GET /readyz` -> `{"status":"ready"}` (HTTP 200, checks PostgreSQL connection pool)
