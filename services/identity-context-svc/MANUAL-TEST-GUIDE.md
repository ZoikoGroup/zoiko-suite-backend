# identity-context-svc — Manual Testing Guide

**Port:** 8080 · **Base URL (local):** `http://localhost:8080`
**Written:** 23 September 2026, against the service **as it currently runs**, post-fix.
**For:** QA testers and frontend engineers calling this service directly.

Organised by the seven functional areas of the console. Every request and response below
was **executed against a live instance** — the values are copied from what actually came
back, so you can paste them straight into Postman or curl.

| # | Section | Endpoints |
|---|---|---|
| 1 | [Authenticate](#1-authenticate) | 1 |
| 2 | [Resolve Identity Context](#2-resolve-identity-context) | 1 |
| 3 | [Principal Lookup & Management](#3-principal-lookup--management) | 4 |
| 4 | [Session Management](#4-session-management) | 4 |
| 5 | [Refresh Tenant Context Cache](#5-refresh-tenant-context-cache) | 1 |
| 6 | [Support Contexts (break-glass)](#6-support-contexts-break-glass) | 4 |
| 7 | [Platform surface](#7-platform-surface) | 3 |

---

## 0. Before you start

### Bring up the stack

```bash
cd zoiko-suite-backend/deployments
docker compose up -d --build identity-svc tenant-svc authorization-svc
```

`--build` is **not optional** — images in this repo go stale and you will otherwise test a
binary from weeks ago. This starts six containers (postgres, redis, kafka, tenant registry,
authorization, and this service), not the full estate.

### Apply migration 000008 — skipping this wastes your morning

```bash
docker exec -i zoiko-postgres psql -U postgres -d identity_context -v ON_ERROR_STOP=1 \
  < ../services/identity-context-svc/deployments/migrations/000008_gov01_gap_closure.up.sql

docker exec zoiko-postgres psql -U postgres -d identity_context \
  -c "GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO zoiko_app;"
```

**Why this matters:** `init-db.sh` only applies migrations to a *brand-new* Postgres volume.
On any stack that has run before, `docker compose up` silently skips them. The service then
reports **healthy** and fails on your first real call. Confirm it worked:

```bash
docker exec zoiko-postgres psql -U postgres -d identity_context -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_name='idempotency_keys';"
# must print 1
```

### Seed a principal

```bash
cd ../services/identity-context-svc
go run ./cmd/seed-local-admin -dsn "postgres://postgres:postgres@localhost:5432/identity_context?sslmode=disable"
```

### Your test data

| Thing | Value |
|---|---|
| Email | `admin@zoikosuite.com` |
| Password | `Zoiko@Governance1` |
| Tenant ID | `11111111-1111-1111-1111-111111111111` |
| Principal ID (you) | `33333333-3333-3333-3333-333333333333` |
| Legal Entity ID | `22222222-2222-2222-2222-222222222222` |
| Second principal (support tests) | `44444444-4444-4444-4444-444444444444` |

### The header set

Nearly everything needs these. The gateway normally sets them; testing directly, you do.

| Header | When | Example |
|---|---|---|
| `X-Tenant-Id` | always, except §1 | `11111111-1111-1111-1111-111111111111` |
| `X-Principal-Id` | always, except §1 and §2 | `33333333-3333-3333-3333-333333333333` |
| `X-Legal-Entity-Id` | all writes | `22222222-2222-2222-2222-222222222222` |
| `X-Source-Channel` | all writes | `api` · `web` · `mobile` · `batch` · `system` |
| `X-Request-Id` | all writes | `qa-req-1` |
| `X-Correlation-ID` | all writes **+ the two session reads** | `qa-001` |
| `Idempotency-Key` | all writes | `qa-idem-1` |

**Two behaviours that changed recently and will surprise you:**

1. The two session reads now **require** `X-Correlation-ID`. Missing it is `400`.
2. `Idempotency-Key` now **actually works** — repeating a write replays the first answer
   instead of doing the work twice.

---

## 1. Authenticate

### `POST /v1/authenticate`

**What it does:** Checks an email and password and returns a short-lived token. This token is
**not** an identity envelope and grants nothing on its own — it only proves the person knew
the password. You exchange it in §2 for the real credential. Keeping the two steps apart is
what stops a password from being enough to act.

**Auth required:** **None.** This is the front door and is deliberately exempt from the header
set — requiring an identity to prove an identity would be circular.

**Testing inputs**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `tenant_id` | string (uuid) | Yes | `11111111-1111-1111-1111-111111111111` | In the **body**, not a header. Naming a tenant only picks a search scope |
| `email` | string | Yes | `admin@zoikosuite.com` | Case-insensitive, ACTIVE humans only |
| `password` | string | Yes | `Zoiko@Governance1` | |
| `correlation_id` | string | No | `qa-001` | Ties this login to the rest of your run in the logs |

```bash
curl -X POST http://localhost:8080/v1/authenticate \
  -H 'Content-Type: application/json' \
  -d '{
    "tenant_id": "11111111-1111-1111-1111-111111111111",
    "email": "admin@zoikosuite.com",
    "password": "Zoiko@Governance1",
    "correlation_id": "qa-001"
  }'
```

**Success — `200 OK`**

```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "expires_in": 300,
  "principal_id": "33333333-3333-3333-3333-333333333333",
  "tenant_id": "11111111-1111-1111-1111-111111111111",
  "mfa_required": false
}
```

**Failures to test**

| Scenario | Status | Response |
|---|---|---|
| Wrong password | 401 | `{"error":"invalid credentials"}` |
| Unknown email | 401 | `{"error":"invalid credentials"}` — **same answer on purpose**, so accounts cannot be enumerated |
| Missing any of the three required fields | 400 | `{"error":"tenant_id, email and password are all required"}` |
| Malformed JSON | 400 | `{"error":"invalid request body"}` |
| Repeated wrong passwords | 401 | Account locks out after a configured number of attempts |
| Database down | 503 | `{"error":"authentication unavailable"}` |

**Watch for**

- `expires_in` is **300 seconds**. Idle for six minutes and §2 will 401 — correct, not a bug.
- `mfa_required` is always `false`. No step-up factor exists in the estate.
- Safe to repeat. No `Idempotency-Key`, no duplicates.

---

## 2. Resolve Identity Context

### `POST /v1/context/resolve`

**What it does:** The heart of the service. Takes the token from §1 and returns the **signed
identity envelope** — the JWT every other service in the platform trusts. This is where
tenant status, legal-entity scope, roles, delegated authority and session trust are actually
checked, across six dimensions.

**Auth required:** The **bearer token in the request body**. No `X-Principal-Id` — this
endpoint *establishes* identity, so it cannot demand one first.

**Testing inputs — body**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `bearer_token` | string | Yes | the `access_token` from §1 | |
| `legal_entity_id` | string (uuid) | Yes | `22222222-2222-2222-2222-222222222222` | The entity the user is acting under |
| `correlation_id` | string | Yes | `qa-001` | |

**Testing inputs — optional headers**

| Header | Example value | What it does |
|---|---|---|
| `X-Support-Context-Id` | `sup-01M36XR0Q7JERF77CQNKCERA2X` | Declares you are acting under a break-glass grant. **Now verified** — see below |
| `X-Workload-Id` | `qa-workload-1` | Recorded on the decision evidence |
| `X-Causation-Id` | `qa-cause-1` | Recorded on the decision evidence |
| `X-Source-Channel` | `web` | Recorded on the decision evidence |

```bash
curl -X POST http://localhost:8080/v1/context/resolve \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' \
  -H 'X-Request-Id: qa-req-1' \
  -H 'X-Correlation-ID: qa-001' \
  -d '{
    "bearer_token": "PASTE_ACCESS_TOKEN_HERE",
    "legal_entity_id": "22222222-2222-2222-2222-222222222222",
    "correlation_id": "qa-001"
  }'
```

**Success — `200 OK`**

```json
{
  "envelope_jwt": "eyJhbGciOiJSUzI1NiIsImtpZCI6...",
  "evidence_id": "ev-01M36XPRYKBDJ4CV6DWVVFN8WR",
  "session_context_id": "01M36XPRYKBDJ4CV6DWQ2P5WSN",
  "expires_at": 1790160402
}
```

**Keep `session_context_id`** — §4 needs it. Yours will differ from the example.

**Failures to test**

| Scenario | Status | Error code |
|---|---|---|
| No token in body | 401 | `CONTEXT_UNRESOLVED` |
| Expired or invalid token | 401 | `CONTEXT_UNRESOLVED` |
| Principal suspended or disabled | 401 | `CONTEXT_UNRESOLVED` |
| Tenant not ACTIVE | 401 | `CONTEXT_UNRESOLVED` |
| Not authorised for that legal entity | 401 | `CONTEXT_UNRESOLVED` |
| Entity not servable from this region | 403 | `RESIDENCY_DENIED` |
| Trust posture blocked | 401 | `TRUST_POSTURE_BLOCKED` |
| `saml_assertion` supplied | 400 | `UNSUPPORTED` — deliberate 400, no SAML IdP exists |
| tenant-svc unreachable | 503 | `UPSTREAM_UNAVAILABLE` |
| **Fictional `X-Support-Context-Id`** | **401** | **`BREAK_GLASS_REQUIRED`** |
| **Expired `X-Support-Context-Id`** | **401** | **`BREAK_GLASS_EXPIRED`** |
| **Another principal's `X-Support-Context-Id`** | **401** | **`BREAK_GLASS_REQUIRED`** |

**⚠️ Test the support-context cases explicitly — this was a real security hole.** The service
used to accept *any* string there and record it as the grant the session was issued under:
expired, revoked, another tenant's, entirely invented — all accepted. Verified live:

```bash
curl -X POST http://localhost:8080/v1/context/resolve \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-6' -H 'X-Correlation-ID: qa-001' \
  -H 'X-Support-Context-Id: sc-completely-made-up' \
  -d '{"bearer_token":"...","legal_entity_id":"22222222-2222-2222-2222-222222222222","correlation_id":"qa-001"}'
```

```json
{"error":"support context not found","error_code":"BREAK_GLASS_REQUIRED"}
```

**Watch for**

- **Every call mints a new session.** Not idempotent, deliberately, and it carries no
  `Idempotency-Key`. Five calls, five sessions. Correct.
- **The tenant comes from the token, never the header.** Send a *different* `X-Tenant-Id` and
  you still get a session for the token's tenant. Not a bug — it is the central guarantee.
  Decode the returned JWT to confirm.
- **Side effects:** writes a session evidence row; publishes `identity.context.resolved`, or
  `identity.context.resolution_failed` on refusal.
- Response carries `Cache-Control: no-store` — the envelope is a credential.
- To see the evidence fields land:
  ```bash
  docker exec zoiko-postgres psql -U postgres -d identity_context \
    -c "SELECT source_channel, workload_id, causation_id FROM session_contexts ORDER BY issued_at DESC LIMIT 1;"
  ```

---

## 3. Principal Lookup & Management

Reading **your own** principal needs no grant. Reading **someone else's** requires
`IDENTITY_PRINCIPAL_READ`. That exemption is intentional: if reading your own roles needed a
grant, every principal on the platform would need one and the check would be noise.

### 3.1 `GET /v1/principals/{principalId}`

**What it does:** Returns the principal record — id, tenant, type, display name, status.

**Testing inputs**

| Input | Where | Required? | Example value |
|---|---|---|---|
| `principalId` | path | Yes | `33333333-3333-3333-3333-333333333333` |
| `X-Tenant-Id` | header | Yes | `11111111-1111-1111-1111-111111111111` |
| `X-Principal-Id` | header | Yes | `33333333-3333-3333-3333-333333333333` |

```bash
curl "http://localhost:8080/v1/principals/33333333-3333-3333-3333-333333333333" \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333'
```

### 3.2 `GET /v1/principals/{principalId}/roles`

**What it does:** Lists the principal's currently active role assignments.

Same inputs as 3.1. Returns `200` with an array (empty array is normal on a fresh stack).

### 3.3 `GET /v1/principals/{principalId}/delegations`

**What it does:** Lists delegated authorities currently held by the principal — authority
handed to them by someone else, within a time window.

Same inputs as 3.1.

### 3.4 `PUT /v1/principals/{principalId}/status`

**What it does:** Sets a principal ACTIVE, SUSPENDED or DISABLED.

**Auth required:** `IDENTITY_PRINCIPAL_STATUS_SET`. **No self-exemption** — changing your own
status still needs the grant, unlike the reads above.

**Testing inputs**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `status` | string | Yes | `SUSPENDED` | `ACTIVE` · `SUSPENDED` · `DISABLED` |
| `reason` | string | No | `QA manual test` | |

```bash
curl -X PUT "http://localhost:8080/v1/principals/44444444-4444-4444-4444-444444444444/status" \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-11' \
  -H 'Idempotency-Key: qa-idem-11' -H 'X-Correlation-ID: qa-001' \
  -d '{"status":"SUSPENDED","reason":"QA manual test"}'
```

**Success — `204 No Content`** (empty body)

**Failures to test — all four routes**

| Scenario | Status | Response |
|---|---|---|
| No `X-Tenant-Id` | 401 | `{"error":"X-Tenant-Id is required — ..."}` |
| No `X-Principal-Id` | 401 | `{"error":"X-Principal-Id is required — ..."}` |
| Unknown principal (reads) | 404 | `{"error":"principal not found"}` |
| Someone else's, no grant | 403 | `{"error":"not authorized to perform this action"}` |
| Invalid status value | 400 | `{"error":"invalid principal status"}` |
| Unknown principal (status update) | 500 | `{"error":"failed to update status"}` — known rough edge, see §8 |
| authorization-svc down | 503 | `{"error":"authorization service unavailable"}` |

**Watch for**

- These four do **not** require `X-Correlation-ID` — only the session reads in §4 do.
- **Suspending a principal does not kill their live sessions.** Invalidate them separately
  via §4. This catches people out.
- Status change publishes `identity.principal.status_changed`.

---

## 4. Session Management

### 4.1 `GET /v1/context/session/{sessionContextId}`

**What it does:** Returns the signed envelope for an existing session.

**Auth required:** tenant + principal + **`X-Correlation-ID`**. Your own session needs no
grant; someone else's requires `IDENTITY_SESSION_READ`.

**Testing inputs**

| Input | Where | Required? | Example value |
|---|---|---|---|
| `sessionContextId` | path | Yes | `01M36XPRYKBDJ4CV6DWQ2P5WSN` (from §2 — yours differs) |
| `X-Tenant-Id` | header | Yes | `11111111-1111-1111-1111-111111111111` |
| `X-Principal-Id` | header | Yes | `33333333-3333-3333-3333-333333333333` |
| `X-Correlation-ID` | header | **Yes** | `qa-001` |

```bash
curl "http://localhost:8080/v1/context/session/01M36XPRYKBDJ4CV6DWQ2P5WSN" \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Correlation-ID: qa-001'
```

**Success — `200 OK`** → `{"envelope_jwt":"eyJhbGciOiJSUzI1NiIs..."}`

**⚠️ This returns a working credential.** It once had no tenant check at all, so knowing a
session id was enough to get an envelope for that identity in any tenant. Test the
cross-tenant case deliberately — you must get `404`, never `403`.

### 4.2 `GET /v1/context/session/{sessionContextId}/explain`

**What it does:** Explains *why* a context resolved as it did, dimension by dimension.
Reconstructed from the recorded decision, never re-derived — this is the audit answer to
"was this session valid at 14:05?"

**Testing inputs:** same as 4.1, plus optionally `?as_of=2026-09-23T10:45:00Z` (RFC3339) to
reconstruct the decision's standing at an instant.

```bash
curl "http://localhost:8080/v1/context/session/01M36XPRYKBDJ4CV6DWQ2P5WSN/explain" \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Correlation-ID: qa-001'
```

**Success — `200 OK`** (real response, truncated)

```json
{
  "session_context_id": "01M36XPRYKBDJ4CV6DWQ2P5WSN",
  "decision_id": "01M36XPRYKBDJ4CV6DWQ2P5WSN",
  "evidence_id": "ev-01M36XPRYKBDJ4CV6DWVVFN8WR",
  "outcome": "RESOLVED",
  "principal_id": "33333333-3333-3333-3333-333333333333",
  "tenant_id": "11111111-1111-1111-1111-111111111111",
  "legal_entity_id": "22222222-2222-2222-2222-222222222222",
  "environment": "local",
  "ingress_source": "localhost",
  "dimensions": [
    {"dimension": 1, "name": "authenticated_principal",
     "result": "33333333-3333-3333-3333-333333333333",
     "source": "verified_idp_token",
     "detail": "principal resolved from the token's subject claim within its tenant"},
    {"dimension": 2, "name": "tenant",
     "result": "11111111-1111-1111-1111-111111111111",
     "source": "tenant_registry",
     "detail": "tenant lifecycle_state was ACTIVE at issue time"}
  ]
}
```

### 4.3 `POST /v1/context/session/{sessionContextId}/invalidate`

**What it does:** Logs one session out. The record is **kept and marked invalid**, never
deleted — it is evidence.

**Auth required:** tenant + principal. Your own session needs no grant; someone else's
requires `IDENTITY_SESSION_INVALIDATE`.

**Testing inputs**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `reason` | string | Yes | `ADMIN_REVOKE` | `LOGOUT` · `ADMIN_REVOKE` · `RISK_ESCALATION` · `DELEGATION_REVOKED` |

```bash
curl -X POST "http://localhost:8080/v1/context/session/01M36XPRYKBDJ4CV6DWQ2P5WSN/invalidate" \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-10' \
  -H 'Idempotency-Key: qa-idem-10' -H 'X-Correlation-ID: qa-001' \
  -d '{"reason":"ADMIN_REVOKE"}'
```

**Success — `204 No Content`**

**Watch for:** the actor recorded is the **gateway-verified** `X-Principal-Id`. It used to come
from an unverified `X-Actor-Principal-ID` header, so the audit trail named whoever the caller
claimed to be. Send that header now and it is ignored — try it.

### 4.4 `POST /v1/context/tenant/invalidate`

**What it does:** Logs out **every** live session in your tenant. The blast radius is the
whole tenant — use last in your test run.

**Auth required:** `IDENTITY_CONTEXT_TENANT_INVALIDATE` — a **separate** grant from 4.3 on
purpose. "Log one user out" and "log everyone out" are not the same decision. No
self-exemption applies.

**Testing inputs**

| Field | Type | Required? | Example value |
|---|---|---|---|
| `reason` | string | Yes | `ADMIN_REVOKE` |
| `justification` | string | Yes | `QA manual test of tenant-wide revocation` |
| `correlation_id` | string | No | `qa-001` |

**Success — `200 OK`** → `{"sessions_revoked":3,"evidence_id":"ev-01M36XS1..."}`

**Failures to test — all four session routes**

| Scenario | Status | Response |
|---|---|---|
| **No `X-Correlation-ID` (4.1, 4.2 only)** | **400** | `{"error":"X-Correlation-ID is required on GOV-01 queries — ...","error_code":"CONTEXT_UNRESOLVED"}` |
| No `X-Tenant-Id` | 401 | `{"error":"X-Tenant-Id is required — ..."}` |
| Another tenant's session | 404 | `{"error":"session not found or expired"}` — **404 not 403**, so ids cannot be probed |
| Unknown session id | 404 | same as above |
| Someone else's session, no grant | 403 | `{"error":"not authorized to perform this action"}` |
| Invalidated or expired session | 404 | `{"error":"session not found or expired"}` |
| Bad `reason` value | 400 | `{"error":"invalid invalidation reason"}` |
| Bad `as_of` format (4.2) | 400 | `{"error":"as_of must be an RFC3339 timestamp"}` |
| Missing `justification` (4.4) | 400 | `CONTEXT_UNRESOLVED` with the reason |
| **Partial failure (4.4)** | 500 | `{"error":"tenant invalidation incomplete","sessions_revoked":2}` — **those 2 really are revoked.** Do not blindly re-run |

**⚠️ The 400 on 4.1/4.2 is new.** These reads used to work without a correlation id. If the
frontend suddenly gets a 400 here, it is not sending the header — that is the bug, not this.

---

## 5. Refresh Tenant Context Cache

### `POST /v1/context/cache/refresh`

**What it does:** Drops this service's cached ingress→tenant routing hints for your tenant,
forcing a fresh read from the registry. Use it after changing hostname bindings.

**Auth required:** tenant + principal + `IDENTITY_CONTEXT_CACHE_REFRESH` grant.

**Testing inputs**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `reason` | string | Yes | `QA manual test` | |
| `ingress_identifiers` | string[] | No | `["tenant-a.zoiko.io"]` | Omit to refresh all of your tenant's |
| `correlation_id` | string | No | `qa-001` | |

```bash
curl -X POST http://localhost:8080/v1/context/cache/refresh \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-5' \
  -H 'Idempotency-Key: qa-idem-1' -H 'X-Correlation-ID: qa-001' \
  -d '{"reason":"QA manual test","correlation_id":"qa-001"}'
```

**Success — `200 OK`**

```json
{"bindings_refreshed":0,"evidence_id":"ev-01M36XQGDF59EHN04VRBNH9467"}
```

`bindings_refreshed: 0` is **normal** on a fresh local stack — no ingress bindings are seeded.
Not a failure.

**⚠️ This is the easiest endpoint for testing idempotency.** Verified live:

| Call | Result |
|---|---|
| First, key `qa-idem-1` | `200`, `evidence_id: ev-01M36XQGDF...` |
| Same key, **same body** | `200` + header **`X-Idempotent-Replay: true`**, and the **identical** `evidence_id` — proof it did not re-run |
| Same key, **changed `reason`** | `409` `{"error":"this Idempotency-Key was already used for a different request","error_code":"IDEMPOTENCY_MISMATCH"}` |

**Failures to test**

| Scenario | Status | Response |
|---|---|---|
| No grant | 403 | `{"error":"not authorized to perform this action"}` |
| Cache service unwired | 501 | `UPSTREAM_UNAVAILABLE` |
| Same key, different body | 409 | `IDEMPOTENCY_MISMATCH` |
| Concurrent duplicate still running | 409 | `IDEMPOTENCY_IN_FLIGHT` |
| Malformed JSON | 400 | `CONTEXT_UNRESOLVED` |

---

## 6. Support Contexts (break-glass)

Time-limited emergency elevations for support staff. The most security-sensitive area of the
service — and where the worst defect was found. Test these thoroughly.

### 6.1 `POST /v1/context/support` — grant an elevation

**What it does:** Creates a time-limited break-glass grant so a support engineer can act on a
tenant during an incident. Scoped, expiring, independently approved and fully evidenced.

**Auth required:** tenant + principal + `IDENTITY_SUPPORT_CONTEXT_ATTACH` grant.

**Testing inputs**

| Field | Type | Required? | Example value | Notes |
|---|---|---|---|---|
| `tenant_id` | string (uuid) | Yes | `11111111-1111-1111-1111-111111111111` | Must match your header tenant |
| `support_principal_id` | string (uuid) | Yes | `44444444-4444-4444-4444-444444444444` | Who receives the elevation |
| `approver_principal_id` | string (uuid) | Yes | `33333333-3333-3333-3333-333333333333` | **Must differ from `support_principal_id`** |
| `reason_code` | string | Yes | `INCIDENT_RESPONSE` | `INCIDENT_RESPONSE` · `CUSTOMER_TICKET` · `DATA_CORRECTION` · `AUDIT_REQUEST` |
| `justification` | string | Yes | `QA manual test of the break-glass flow` | Minimum length enforced |
| `ticket_ref` | string | Yes | `QA-1` | |
| `ttl_seconds` | int | No | `600` | Defaults to 1 hour. **Maximum 4 hours** |
| `subject_principal_id` | string (uuid) | No | `33333333-...` | Narrows the grant to one person's data. Omit for tenant-wide |
| `correlation_id` | string | No | `qa-001` | |

```bash
curl -X POST http://localhost:8080/v1/context/support \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 33333333-3333-3333-3333-333333333333' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-7' \
  -H 'Idempotency-Key: qa-idem-7' -H 'X-Correlation-ID: qa-001' \
  -d '{
    "tenant_id": "11111111-1111-1111-1111-111111111111",
    "support_principal_id": "44444444-4444-4444-4444-444444444444",
    "approver_principal_id": "33333333-3333-3333-3333-333333333333",
    "reason_code": "INCIDENT_RESPONSE",
    "justification": "QA manual test of the break-glass flow",
    "ticket_ref": "QA-1",
    "ttl_seconds": 600
  }'
```

**Success — `201 Created`**

```json
{
  "support_context_id": "sup-01M36XR0Q7JERF77CQNKCERA2X",
  "expires_at": "2026-09-23T10:52:23.079311092Z",
  "evidence_id": "ev-01M36XR0Q7JERF77CQNMSV61H8"
}
```

### 6.2 `GET /v1/context/support/{supportContextId}` — read a grant

**What it does:** Returns the grant — who, why, approved by whom, when it expires, whether it
was revoked or reviewed.

**Auth required:** tenant + principal + `IDENTITY_SUPPORT_CONTEXT_ATTACH`.

### 6.3 `DELETE /v1/context/support/{supportContextId}` — revoke early

**What it does:** Ends an elevation before its TTL expires.

**Auth required:** `IDENTITY_SUPPORT_CONTEXT_REVOKE` — deliberately a **weaker** grant than
attach, because whoever notices a problem must be able to stop it.

**Testing inputs**

| Field | Type | Required? | Example value |
|---|---|---|---|
| `reason` | string | No | `QA manual test — revoking early` |

**Success — `204 No Content`**

**Watch for:** revoking twice is **safe** — the second returns `204` with no second event, and
the **first** reason stands, because the reason is what an investigation reads. An empty body
is accepted; a revocation with no stated reason beats one that never happened.

### 6.4 `POST /v1/context/support/{supportContextId}/review` — record the review

**What it does:** Records that a human reviewed an expired elevation. This closes the
reconciliation loop that §1 of the spec requires: a grant that expires and is never checked
is a control that ran but was never verified.

**Auth required:** `IDENTITY_SUPPORT_CONTEXT_REVIEW` — its **own** grant, deliberately not the
revoke one. Ending a live session and signing off that it was legitimate are different duties.

**Testing inputs:** path id + the standard write headers. **No body required.**

```bash
curl -X POST "http://localhost:8080/v1/context/support/sup-01M36XR0Q7JERF77CQNKCERA2X/review" \
  -H 'X-Tenant-Id: 11111111-1111-1111-1111-111111111111' \
  -H 'X-Principal-Id: 55555555-5555-5555-5555-555555555555' \
  -H 'X-Legal-Entity-Id: 22222222-2222-2222-2222-222222222222' \
  -H 'X-Source-Channel: api' -H 'X-Request-Id: qa-12' \
  -H 'Idempotency-Key: qa-idem-12' -H 'X-Correlation-ID: qa-001'
```

**Success — `204 No Content`**

**Failures to test — all four**

| Scenario | Status | Response |
|---|---|---|
| **Self-approval** (`approver` == `support_principal`) | 400 | `SOD_CONFLICT` — **core control, test it** |
| **`ttl_seconds: 999999`** (above the 4h ceiling) | 400 | TTL ceiling refusal — **test it** |
| **Review by the grantee** | **403** | `SOD_CONFLICT` — "may not be reviewed by its grantee or its approver" |
| **Review by the approver** | **403** | `SOD_CONFLICT` |
| Justification too short | 400 | Validation message |
| Missing `tenant_id` | 400 | `CONTEXT_UNRESOLVED` |
| Not found / another tenant's | 404 | `BREAK_GLASS_REQUIRED` |
| No grant | 403 | `{"error":"not authorized to perform this action"}` |
| Segregation-of-duties conflict | 409 | `SOD_CONFLICT` |
| Support family unwired | 501 | `UPSTREAM_UNAVAILABLE` |
| Replay, same key + body | 201 | Same body, `X-Idempotent-Replay: true` — **no second grant** |
| Replay, same key + different body | 409 | `IDEMPOTENCY_MISMATCH` |

**⚠️ Three things to verify deliberately**

1. **Replay used to mint a SECOND live break-glass grant.** It no longer does:
   ```bash
   # call 6.1 twice with the same Idempotency-Key, then:
   docker exec zoiko-postgres psql -U postgres -d identity_context -tAc \
     "SELECT count(*) FROM support_contexts WHERE ticket_ref='QA-1';"
   # must be 1, not 2
   ```
2. **The grant belongs to the support principal, not to you.** Using it in
   `X-Support-Context-Id` on a §2 resolve for a *different* principal is refused `401
   BREAK_GLASS_REQUIRED`. Verified live — correct, not a bug.
3. **The review route is brand new.** Break-glass review previously had no route at all, so
   `reviewed_at` could never be set by anything. Test the self-review refusal explicitly.

**Watch for:** a background reconciler reports expired-but-unreviewed grants every 15 minutes
(`SUPPORT_REVIEW_INTERVAL_MINUTES`). Look for `SUPPORT CONTEXT AWAITING REVIEW` in
`docker logs identity-context-svc`.

---

## 7. Platform surface

No auth, no side effects, safe to call at any time.

### 7.1 `GET /health`

**What it does:** Readiness. Reports each dependency separately.

```bash
curl http://localhost:8080/health
```

```json
{"status":"healthy","checks":{"outbox":"ok","outbox_pending":"0","postgres":"ok","redis":"ok","tenant_registry":"ok"},"checked_at":"2026-09-23T10:40:58Z"}
```

**Watch for:** if `tenant_registry` is not `ok`, `tenant-svc` is down and **every §2 resolve
will 503**. Check this first when resolve starts failing. `outbox_pending` climbing means
governance events are piling up unpublished — the one number worth alerting on.

### 7.2 `GET /metrics`

**What it does:** Prometheus metrics. Useful ones: `identity_context_outbox_pending`,
`identity_context_resolution_failed` (labelled by reason).

**Watch for:** a Prometheus `CounterVec` exports **nothing at all** until a label combination
is first observed. A missing `resolution_failed` series means no failure has happened yet, not
that the metric is broken.

### 7.3 `GET /.well-known/jwks.json`

**What it does:** Publishes the public keys other services use to verify the envelopes from
§2. Returns `200` with a JWKS document.

---

## 8. Known limitations — do not raise bugs for these

### Working as intended, differs from older docs

| Thing | Old docs said | Reality |
|---|---|---|
| URL shape | `/internal/v1/gov01/resolveTenantContext` | `/v1/context/...`. The spec column is headed *"Illustrative contract surface"* — **ruled illustrative, not binding**, 23 Sep 2026 |
| `expected_version` on commands | "Idempotency-Key + expected_version where stateful" | **Ruled not applicable.** Nothing here has a lost-update to protect — sessions are append-only, cache/tenant invalidate are naturally idempotent, support contexts are guarded by append-only `revoked_at`/`reviewed_at` |
| `/v1/context/resolve` needing `Idempotency-Key` | it used to demand one | It no longer does. The spec classifies it a **query**, and it mints a new session per call — replay protection would be meaningless |
| `X-Actor-Principal-ID` | was honoured as the actor | **Ignored.** The actor is the gateway-verified `X-Principal-Id` only |

### Genuinely incomplete — known and tracked

| Thing | Status |
|---|---|
| **Entitlement context reference** | `session_contexts.entitlement_context_ref` is **always NULL**. The spec requires it; the service that would supply it — **COM-03 Entitlement**, owner of `EntitlementSnapshot` — **does not exist anywhere in the estate**. The column is a placeholder |
| `bindings_refreshed: 0` (§5) | Normal locally — no ingress bindings seeded |
| `ingress_binding_version` empty in evidence | Normal locally — same reason |
| `mfa_required` always `false` | No step-up factor exists in the estate |
| SAML | `saml_assertion` always `400 UNSUPPORTED`. No SAML IdP configured |
| Segregation-of-duties engine | `SOD_SERVICE_URL` is empty locally, so SoD checks are permissive in local/dev. Staging and production refuse to boot without it |
| Unknown principal on status update | Returns `500`, arguably should be `404`. Known rough edge |

### Two error body shapes reach you

Handle both — a client reading only one sees an empty string for the other.

```json
{"error": "...", "error_code": "CONTEXT_UNRESOLVED", "correlation_id": "qa-001"}
```
```json
{"error": "envelope_incomplete", "detail": "...", "violations": [ ... ]}
```

The first comes from the handlers, the second from the envelope middleware.

---

## 9. Suggested run order

**Setup** — §0 in full: compose up, **apply 000008 + GRANT**, seed, `GET /health`.

1. **§1** Authenticate → keep `access_token` (5-minute life)
2. **§2** Resolve → keep `session_context_id` and `evidence_id`
3. **§4.1** Get session → envelope returned
4. **§4.2** Explain → dimension breakdown
5. **§3.1–3.3** Read your own principal, roles, delegations (no grant needed)
6. **§5** Cache refresh with `Idempotency-Key: qa-idem-1` → note `evidence_id`
7. **§5** Repeat identically → `X-Idempotent-Replay: true`, **same** `evidence_id`
8. **§5** Same key, changed `reason` → `409 IDEMPOTENCY_MISMATCH`
9. **§6.1** Create support context → keep `support_context_id`
10. **§6.1** Repeat, same key → no second grant (count rows)
11. **§6.1** Self-approval → `400`; `ttl_seconds: 999999` → `400`
12. **§6.2** Read it back
13. **§6.4** Review as the grantee → **`403 SOD_CONFLICT`**
14. **§6.3** Revoke → `204`. Repeat → `204` again
15. **§2** Resolve with `X-Support-Context-Id: sc-made-up` → **`401 BREAK_GLASS_REQUIRED`**
16. **§2** Resolve with another principal's grant → **`401 BREAK_GLASS_REQUIRED`**
17. **§4.1** Get session with **no** `X-Correlation-ID` → **`400`**
18. **§4.1** Get session with a **different** `X-Tenant-Id` → `404`, never `403`
19. **§2** Resolve with a wrong `X-Tenant-Id` header → still succeeds, bound to the **token's**
    tenant. Decode the JWT to confirm
20. **§3.4** Suspend principal `4444...` → `204`
21. **§4.4** Tenant invalidate → revokes everything. **Do this last**
