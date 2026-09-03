# Testing guide — tenant-entity-registry-svc

The authoritative registry every other ZoikoSuite service depends on. A `tenant_id`
from here scopes row-level security platform-wide, and a `legal_entity_id` from here
is the scope `authorization-svc` evaluates grants against. Nothing else on the
platform can be created until a tenant exists here, which is why this service is
worth testing harder than its size suggests.

## How much of this has actually been run

Sections 1–13 were **executed** on 2026-08-27 against the real service binary
(`cmd/server`) talking to a real PostgreSQL 16, and every "Expected output" block
below is pasted from that run rather than inferred from the handlers. Two
dependencies were the **development stubs** in that run, which the service selects
automatically from placeholder URLs and announces at startup:

```
"msg":"using STUB authorization client — wire real AuthZ before production"
"msg":"using STUB jurisdiction validator — wire real service before production"
```

So everything below is the behaviour with **authorization permitting everything**
and **every jurisdiction id accepted**. Section 14 covers what changes when those
are real, and is marked as inferred from source — do not trust its status codes
until someone has run them.

Section 15 (the console) was not exercised in a browser for this document.

Section 16 lists the defects this exercise found, including one that takes the
whole process down.

---

## Contents

| # | Section | Needs |
|---|---------|-------|
| 1 | [Unit and contract suites](#1-unit-and-contract-suites) | Go only |
| 2 | [Store tests against real Postgres](#2-store-tests-against-real-postgres) | Go only |
| 3 | [Standing the service up](#3-standing-the-service-up) | Docker **or** Go |
| 4 | [Health, readiness, metrics](#4-health-readiness-metrics) | running service |
| 5 | [The canonical envelope](#5-the-canonical-envelope) | running service |
| 6 | [Tenant provisioning and lifecycle](#6-tenant-provisioning-and-lifecycle) | running service |
| 7 | [Legal entities](#7-legal-entities) | running service |
| 8 | [The entity status state machine](#8-the-entity-status-state-machine) | running service |
| 9 | [Workspaces and billing classification](#9-workspaces-and-billing-classification) | running service |
| 10 | [Hierarchies — effective dating](#10-hierarchies--effective-dating) | running service |
| 11 | [Jurisdiction assignments](#11-jurisdiction-assignments) | running service |
| 12 | [Tax identity bundles](#12-tax-identity-bundles) | running service |
| 13 | [Residency policies, regions, tenant→region](#13-residency-policies-regions-tenantregion) | running service |
| 14 | [Fail-closed dependencies](#14-fail-closed-dependencies--inferred-not-run) | real authz + jurisdiction |
| 15 | [The admin console](#15-the-admin-console-admintenants) | console + service |
| 16 | [Defects this guide found](#16-defects-this-guide-found) | — |
| 17 | [Appendix: routes, enums, environment](#17-appendix-routes-enums-environment) | — |

---

## 1. Unit and contract suites

No database, no Docker, no network. First thing to run, first thing to fix.

```bash
cd services/tenant-entity-registry-svc
go build ./...
go vet ./...
go test ./...
```

**Expected output** — five packages with tests, nine without. `go build` and
`go vet` print nothing at all and exit 0:

```
?   	zoiko.io/tenant-entity-registry-svc/cmd/healthcheck	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/cmd/server	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/authz	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/classification	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/config	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/domain	[no test files]
ok  	zoiko.io/tenant-entity-registry-svc/internal/envelope	3.150s
ok  	zoiko.io/tenant-entity-registry-svc/internal/events	1.544s
ok  	zoiko.io/tenant-entity-registry-svc/internal/handler	2.712s
?   	zoiko.io/tenant-entity-registry-svc/internal/health	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/jurisdiction	[no test files]
?   	zoiko.io/tenant-entity-registry-svc/internal/middleware	[no test files]
ok  	zoiko.io/tenant-entity-registry-svc/internal/registry	3.110s
ok  	zoiko.io/tenant-entity-registry-svc/internal/store	3.761s
?   	zoiko.io/tenant-entity-registry-svc/internal/telemetry	[no test files]
```

**Expected counts.** 60 top-level tests pass, 200 assertions including subtests,
**9 skip**:

```bash
go test ./... -v 2>&1 | grep -c -- "--- PASS"     # 200
go test ./... -v 2>&1 | grep -- "--- SKIP"        # 9
```

```
--- SKIP: TestPgStore_CreateTenant_And_GetTenantByID (0.00s)
--- SKIP: TestPgStore_CreateEntity_And_GetEntityByID (0.00s)
--- SKIP: TestPgStore_RLS_TenantIsolation (0.00s)
--- SKIP: TestPgStore_UpdateWorkspace_ReclassifiesAndStampsActor (0.00s)
--- SKIP: TestPgStore_UpdateWorkspace_CrossTenantRefused (0.00s)
--- SKIP: TestPgStore_UpdateWorkspace_AbsentReturnsNil (0.00s)
--- SKIP: TestPgStore_TransitionWorkspaceStatus_ReturnsPreviousStatus (0.00s)
--- SKIP: TestPgStore_TransitionWorkspaceStatus_DisallowedPriorTouchesNothing (0.00s)
--- SKIP: TestPgStore_TransitionWorkspaceStatus_AbsentReturnsZero (0.00s)
```

**Those nine skips are the point of section 2.** A green `go test ./...` here has
not tested the database layer at all. `internal/store` reporting `ok` with every one
of its untagged tests skipped is exactly what a passing run looks like when nothing
was verified.

---

## 2. Store tests against real Postgres

There are two independent DB-backed suites and they are unskipped in **different
ways**. Running one does not run the other.

### 2a. The isolation suite — embedded Postgres, no setup

Build-tagged `integration`. It downloads and starts its own PostgreSQL 16, applies
all five migrations, and stops it again. Nothing to install; the first run fetches a
Postgres binary.

```bash
go test -tags=integration -count=1 -timeout=300s ./internal/store/
```

**Expected output:**

```
ok  	zoiko.io/tenant-entity-registry-svc/internal/store	25.194s
```

Roughly 25s — most of it is Postgres starting. Add `-v` to see the 14 isolation
tests, each of which creates two tenants and proves tenant B cannot read, update or
transition tenant A's rows.

**Why this suite matters more than the others.** Row-level security on these tables
is written correctly and **does not run**. The service connects as a Postgres
superuser, superusers bypass RLS unconditionally, and the tables were created
`ENABLE ROW LEVEL SECURITY` rather than `FORCE`, so the owner bypasses them too. The
only real isolation guarantee is the explicit `AND tenant_id = $N` in each query.
This suite is what pins those clauses in place.

### 2b. The store suite — bring your own Postgres

`internal/store/pg_store_test.go` skips unless `TEST_DATABASE_URL` is set. It
**drops and recreates** its tables on every run, so point it at a scratch database
and never at anything you care about.

```bash
export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:5432/ter_test?sslmode=disable"
go test -count=1 -v ./internal/store/
```

**Expected output** — the nine skips from section 1 now run and pass:

```
--- PASS: TestPgStore_CreateTenant_And_GetTenantByID (0.42s)
--- PASS: TestPgStore_CreateEntity_And_GetEntityByID (0.27s)
--- PASS: TestPgStore_RLS_TenantIsolation (0.30s)
--- PASS: TestPgStore_UpdateWorkspace_ReclassifiesAndStampsActor (0.30s)
--- PASS: TestPgStore_UpdateWorkspace_CrossTenantRefused (0.29s)
--- PASS: TestPgStore_UpdateWorkspace_AbsentReturnsNil (0.30s)
--- PASS: TestPgStore_TransitionWorkspaceStatus_ReturnsPreviousStatus (0.30s)
--- PASS: TestPgStore_TransitionWorkspaceStatus_DisallowedPriorTouchesNothing (0.30s)
--- PASS: TestPgStore_TransitionWorkspaceStatus_AbsentReturnsZero (0.27s)
PASS
ok  	zoiko.io/tenant-entity-registry-svc/internal/store	4.230s
```

If you still see `SKIP`, the variable did not reach the test process — `go test`
inherits the environment, so check the export rather than the test.

---

## 3. Standing the service up

### 3a. The normal path — Docker Compose

```powershell
cd zoiko-suite-backend
docker compose -f deployments/docker-compose.yml up -d postgres kafka authorization-svc tenant-svc
```

The container is named `tenant-entity-registry-svc` and publishes **:8081**. It
`depends_on` postgres, kafka and `authorization-svc` all being *healthy*, so if the
container never starts, check those three before reading any of this service's logs.

Then seed the two things the console needs, which answer two different questions:

```powershell
./deployments/scripts/seed-demo-registry.ps1   # does the demo tenant/entity exist?
./deployments/scripts/seed-demo-rbac.ps1       # may the demo principal act on it?
```

**`seed-demo-registry.ps1` is what fixes "This session's tenant is not in the
registry".** The console's demo identity names a fixed tenant
`11111111-1111-1111-1111-111111111111`, and nothing creates it — this service's own
`POST /v1/tenants` mints its own id and has no field to ask for a specific one, so a
fixed-id demo tenant is the one fixture the API cannot produce.

> **Two caveats on that seed, both real.**
>
> 1. `seed-demo-rbac.ps1` has **no bundle for this service**. It grants 22 other
>    services' actions and none of `TENANT_PROVISION`, `ENTITY_CREATE`,
>    `WORKSPACE_CREATE`, `ENTITY_STATUS_TRANSITION`… so against a *real*
>    `authorization-svc` every write in sections 6–13 is refused **403**, correctly.
>    Grant them yourself or expect the refusal.
> 2. `seed-demo-registry.sql` writes `residency_mode = 'SINGLE_REGION'` and
>    `conflict_resolution_mode = 'STRICTEST_WINS'`. Neither is a value this service's
>    `domain` package defines (§17). The columns are plain `VARCHAR` with no `CHECK`,
>    so it persists and reads back as-is.

### 3b. The no-Docker path — what this document actually used

Docker was not available, so every observed result below came from the real
`cmd/server` binary against a standalone PostgreSQL 16, with Kafka and both upstreams
switched off:

```bash
ENV=local PORT=18081 \
DB_HOST=localhost DB_PORT=5432 DB_NAME=ter_doc DB_USER=postgres DB_PASSWORD=postgres \
DB_SSLMODE=disable \
KAFKA_BROKERS= \
AUTHZ_SERVICE_URL=http://authorization-svc \
JURISDICTION_RULES_URL=http://jurisdiction-rules-svc \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
go run ./cmd/server
```

Three of those are load-bearing:

- **`KAFKA_BROKERS=`** — an explicitly empty value means "no event backbone".
  `envList` honours the empty string rather than substituting the default, which
  `env()` would. Event publishing then fails and is logged, never propagated, so
  writes still succeed.
- **`AUTHZ_SERVICE_URL=http://authorization-svc`** and
  **`JURISDICTION_RULES_URL=http://jurisdiction-rules-svc`** are recognised
  *placeholders*. They select the permit-everything stubs. `ENV=local` is required
  for that: `config.validate()` and `authz.NewClient` both refuse to start in
  `production` or `staging` with a placeholder URL, a blank `DB_PASSWORD`,
  `DB_SSLMODE=disable`, or an empty `AUTHZ_PLATFORM_SCOPE_ID`.
- **`OTEL_EXPORTER_OTLP_ENDPOINT`** pointing nowhere is harmless. You will see
  `traces export: … connection refused` in the log every few seconds; ignore it.

**Expected startup output** — five lines, in this order:

```json
{"level":"info","msg":"tenant-entity-registry-svc starting","port":18081,"db_host":"localhost","jurisdiction_rules_url":"http://jurisdiction-rules-svc","authz_url":"http://authorization-svc"}
{"level":"info","msg":"db pool connected"}
{"level":"warn","msg":"using STUB authorization client — wire real AuthZ before production","authz_service_url":"http://authorization-svc"}
{"level":"warn","msg":"using STUB jurisdiction validator — wire real service before production"}
{"level":"info","msg":"HTTP server listening","addr":":18081"}
```

**If `db pool connected` never appears, the process exits.** Postgres is a fail-fast
startup dependency; Kafka deliberately is not.

Every `curl` below assumes `BASE=http://localhost:18081`. On the compose stack use
`:8081`.

---

## 4. Health, readiness, metrics

Three unauthenticated routes, mounted outside `/v1` and exempt from the envelope
contract.

```bash
curl -i $BASE/healthz
curl -i $BASE/readyz
curl -s $BASE/metrics | head
```

**Expected output:**

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Correlation-Id: KEVIN/vnfnK0mJ90-000001

{"status":"ok","timestamp":"2026-08-27T10:14:56.9868942Z","service":"tenant-entity-registry-svc"}
```

`/readyz` returns an identical body — but only `/readyz` pings the database.
`/healthz` answers 200 whenever the process is alive, so **a dead database pool reads
as healthy on `/healthz` while every read returns 500.** Compose health-checks this
service on `/healthz`; several sibling services deliberately use `/readyz` instead.
When the pool is down:

```
HTTP/1.1 503 Service Unavailable
{"status":"db_unavailable","timestamp":"…","service":"tenant-entity-registry-svc"}
```

`/metrics` is Prometheus text. Useful assertion: after exercising the sections below,
every route appears with its **chi pattern**, not its expanded path — which is what
tells you the route table is wired correctly:

```
http_request_duration_seconds_count{method="GET",route="/v1/workspaces/{workspaceID}",service="tenant-entity-registry-svc"} 2
http_request_duration_seconds_count{method="GET",route="/v1/tenants/{tenantID}/workspaces",service="tenant-entity-registry-svc"} 1
```

Note the correlation header is echoed even when you did not send one — chi's request
id is substituted.

---

## 5. The canonical envelope

*(ZS-ARCH-SVC-001 §4)*

Every request passes `envelope.Middleware` before it reaches a handler. Default
enforcement is **`write-strict`** (`ZS_ENVELOPE_ENFORCEMENT`): material writes are
refused, reads are admitted. This is the single most common reason a request that
"should work" does not, so test it before anything else.

The five unconditionally mandatory headers, plus one more on writes:

| Header | Field | When |
|---|---|---|
| `X-Tenant-Id` | tenant_id | always |
| `X-Principal-Id` | actor_subject_id | always (or `X-Workload-Id`) |
| `X-Request-Id` | request_id | always |
| `X-Correlation-ID` | correlation_id | always |
| `X-Source-Channel` | source_channel | always — one of `api, import, integration, mobile, scheduled_job, system, web` |
| `Idempotency-Key` | idempotency_key | every non-GET/HEAD/OPTIONS |

### 5.1 A write with no envelope at all → 401

```bash
curl -i -X POST $BASE/v1/tenants -H 'Content-Type: application/json' \
  -d '{"tenant_code":"ACME","legal_name":"Acme Holdings Ltd"}'
```

**Expected — 401, not 400.** A missing tenant or actor means the request never passed
gateway verification, so it is an authentication failure. All five violations are
reported at once, deliberately, so adopting the envelope is not five round trips:

```
HTTP/1.1 401 Unauthorized
X-Envelope-Contract: violated
```

```json
{"error":"envelope_incomplete",
 "detail":"canonical input contract violated: actor_subject_id, idempotency_key, request_id, source_channel, tenant_id",
 "service":"tenant-entity-registry-svc",
 "violations":[
  {"field":"tenant_id","header":"X-Tenant-Id","reason":"mandatory: tenant authority and isolation boundary"},
  {"field":"actor_subject_id","header":"X-Principal-Id","reason":"mandatory: supply X-Principal-Id for a human subject or X-Workload-Id for a workload"},
  {"field":"request_id","header":"X-Request-Id","reason":"mandatory: request tracing"},
  {"field":"source_channel","header":"X-Source-Channel","reason":"mandatory: one of api, import, integration, mobile, scheduled_job, system, web"},
  {"field":"idempotency_key","header":"Idempotency-Key","reason":"mandatory for material state changes: duplicate/replay protection (INV-08)"}]}
```

### 5.2 Tenant and actor present, the rest missing → 400

Same shape, three violations, and the status drops to **400** because the two
authentication-class fields are now satisfied:

```json
{"error":"envelope_incomplete","detail":"canonical input contract violated: idempotency_key, request_id, source_channel", …}
```

### 5.3 An unrecognised channel → 400

`X-Source-Channel: browser`:

```json
{"field":"source_channel","header":"X-Source-Channel",
 "reason":"unrecognised channel \"browser\": expected one of api, import, integration, mobile, scheduled_job, system, web"}
```

An unknown channel is refused rather than coerced to `api`, because
`import`/`integration` are what make `X-Source-System` mandatory — silently mapping
an unknown value would erase that obligation.

### 5.4 A read with no envelope → 200

```bash
curl -i $BASE/v1/tenants/$TENANT          # no headers at all
```

**Expected: 200**, with `X-Envelope-Contract: violated` on the response and a
`canonical input contract violated` warning in the log. This is `write-strict` doing
its job. Set `ZS_ENVELOPE_ENFORCEMENT=strict` and the same call becomes 401.

> **A read with no `X-Tenant-Id` still returns 404, not data.** See §6.4 — the two
> refusals are independent and easy to confuse.

### 5.5 The full envelope

Every subsequent section assumes this preamble:

```bash
BASE=http://localhost:18081
TENANT=…                                     # filled in by section 6
PRINCIPAL=33333333-3333-3333-3333-333333333333

hdrs=(-H "X-Tenant-Id: $TENANT"
      -H "X-Principal-Id: $PRINCIPAL"
      -H "X-Request-Id: $(uuidgen)"
      -H "X-Correlation-ID: corr-1"
      -H "X-Source-Channel: api"
      -H "Idempotency-Key: $(uuidgen)"
      -H "Content-Type: application/json")
```

`X-Request-Id` and `X-Correlation-ID` are echoed back on every response, including
refusals — deliberately, so a caller debugging a 400 can still find the request in
the log that refused it.

### 5.6 The complete error vocabulary

Verified by driving each sentinel through the real router. This is the whole contract
between the service's error vocabulary and the API:

| Service error | HTTP | Body |
|---|---|---|
| `ErrNotFound` | **404** | `{"error":"not found","correlation_id":"corr-1"}` |
| `ErrUnauthenticated` | **401** | `{"error":"unauthenticated: no verified principal on request", …}` |
| `ErrUnauthorized` | **403** | `{"error":"forbidden", …}` |
| `ErrInvalidInput` | **400** | `{"error":"invalid input: <detail>", …}` |
| `ErrConflict` | **409** | `{"error":"conflict: resource already exists: <detail>", …}` |
| `ErrRegionUnresolved` | **409** | `{"error":"tenant's residency policy has no region assigned", …}` |
| `ErrInvalidTransition` | **422** | `{"error":"invalid status transition: <from> → <to>", …}` |
| `ErrServiceUnavailable` | **503** | `{"error":"upstream service unavailable — request rejected", …}` |
| anything else | **500** | `{"error":"internal server error", …}` — detail is logged, never returned |

Two more, from chi rather than the handler:

```
GET  /v1/nope              → 404  text/plain  "404 page not found"
POST /v1/residency-regions → 405  empty body
```

**401 and 403 are deliberately distinct.** 401 means no principal was identified at
all; 403 means a real principal was denied. An operator needs to tell "the gateway
did not forward an identity" from "this principal is not permitted".

### 5.7 Malformed bodies and query parameters

```bash
curl -X POST $BASE/v1/tenants "${hdrs[@]}" -d '{'
```

```json
{"error":"invalid request body","correlation_id":"corr-1"}     # 400
```

The two end-dating routes take `end_date` as a **required RFC3339 query parameter**:

```
DELETE /v1/entity-hierarchies/{id}                               → 400 {"error":"end_date query parameter is required (RFC3339)"}
DELETE /v1/entity-hierarchies/{id}?end_date=2026-12-31           → 400 {"error":"end_date must be RFC3339 format"}
DELETE /v1/entity-hierarchies/{id}?end_date=2026-12-31T00:00:00Z → 204
```

A date without a time is the mistake people actually make, and it is refused.

---

## 6. Tenant provisioning and lifecycle

### 6.1 The bootstrap paradox — read this first

`POST /v1/tenants` is the one call with no tenant yet: it is what *creates* one. The
service is built for that — `authorize()` falls back to `AUTHZ_PLATFORM_SCOPE_ID`
when no tenant is on the request.

**That fallback is unreachable.** The envelope middleware runs first and demands
`X-Tenant-Id` on every material write, including this one:

```bash
curl -X POST $BASE/v1/tenants -H 'X-Principal-Id: …' -H 'X-Source-Channel: api' … \
  -d '{"tenant_code":"DOCACME", …}'         # no X-Tenant-Id
```

```
HTTP/1.1 401 Unauthorized
{"error":"envelope_incomplete","detail":"canonical input contract violated: tenant_id", …}
```

So you must send *some* tenant to create a tenant, and whatever you send becomes the
authorization scope instead of the platform scope. Send the platform scope id itself
— that is where `TENANT_PROVISION` grants are supposed to live:

```bash
BOOT=00000000-0000-0000-0000-00000000f001    # AUTHZ_PLATFORM_SCOPE_ID
```

`AUTHZ_PLATFORM_SCOPE_ID` is therefore dead configuration on this path unless
enforcement is set to `observe`. See §16.2.

### 6.2 Provision a tenant

```bash
curl -X POST $BASE/v1/tenants \
  -H "X-Tenant-Id: $BOOT" -H "X-Principal-Id: $PRINCIPAL" \
  -H 'X-Request-Id: req-1' -H 'X-Correlation-ID: corr-1' \
  -H 'X-Source-Channel: api' -H 'Idempotency-Key: idem-1' \
  -H 'Content-Type: application/json' \
  -d '{"tenant_code":"DOCACME","legal_name":"Acme Holdings Ltd","trading_name":"Acme",
       "default_currency_code":"GBP","primary_timezone":"Europe/London","primary_locale":"en-GB"}'
```

**Expected — 201:**

```json
{"tenant_id":"01a042b8-4c87-7d62-8221-c6fb5eea179c",
 "tenant_code":"DOCACME","legal_name":"Acme Holdings Ltd","trading_name":"Acme",
 "status":"ACTIVE",
 "default_currency_code":"GBP","primary_timezone":"Europe/London","primary_locale":"en-GB",
 "default_data_residency_policy_id":"01a042b8-4c87-7d63-9817-2c8aa34d8e58",
 "lifecycle_state":"ONBOARDING",
 "created_at":"2026-08-27T10:16:09.6078773Z",
 "updated_at":"0001-01-01T00:00:00Z",
 "created_by_principal_id":"33333333-3333-3333-3333-333333333333",
 "updated_by_principal_id":""}
```

Four things to check in that response, all correct and all of which look wrong at a
glance:

1. **`status: ACTIVE` while `lifecycle_state: ONBOARDING`.** Separate fields, and only
   lifecycle has a state machine. Reporting either one as "the status" misrepresents
   the other.
2. **`updated_at` is the zero time and `updated_by_principal_id` is empty.** The 201
   body is the in-memory object, not the persisted row. Re-read the tenant and both
   are populated. Every create endpoint in this service behaves this way.
3. **A residency policy was created that you did not ask for**, and its id is
   returned. `default_data_residency_policy_id` in the *request* is accepted and
   ignored — a policy has a foreign key to `tenants`, so none can exist before the
   tenant does.
4. **`created_at` is UTC here but comes back with a local offset on read**
   (`2026-08-27T15:46:09.607877+05:30`). Same instant, rendered in the database
   session's timezone.

Save both ids:

```bash
TENANT=01a042b8-4c87-7d62-8221-c6fb5eea179c
POLICY=01a042b8-4c87-7d63-9817-2c8aa34d8e58
```

### 6.3 Duplicate tenant_code → 409

Repeat the exact call. `tenant_code` is `UNIQUE`:

```json
{"error":"store.CreateTenantWithDefaultResidencyPolicy: conflict: resource already exists: tenant_code DOCACME",
 "correlation_id":"corr-1"}
```

**The internal function name is in the response body.** Cosmetic, but it is an error
string a client will end up parsing — noted in §16.7.

### 6.4 Read it back, and the isolation probe

```bash
curl $BASE/v1/tenants/$TENANT -H "X-Tenant-Id: $TENANT" …
```

**200** — now with `updated_at` and `updated_by_principal_id` filled in.

Now the same URL with a **different** tenant header, and then with **none**:

| `X-Tenant-Id` | Expected |
|---|---|
| the tenant's own id | **200** + full body |
| any other id | **404** `{"error":"not found"}` |
| absent | **404** `{"error":"not found"}` |

**404 rather than 403 is deliberate.** A cross-tenant probe must not be able to
distinguish "exists but forbidden" from "does not exist", or the 403 itself confirms
the tenant exists. Note this is enforced in the service layer by comparing the path
tenant against the caller's verified tenant — *not* by RLS, which does not run (§2a).

### 6.5 The lifecycle state machine

`ONBOARDING → ACTIVE → {SUSPENDED, OFFBOARDING}`, `SUSPENDED → {ACTIVE, OFFBOARDING}`,
`OFFBOARDING` terminal.

```bash
curl -X POST $BASE/v1/tenants/$TENANT/lifecycle "${hdrs[@]}" -d '{"target_state":"ACTIVE"}'
```

| Call | Expected |
|---|---|
| `ONBOARDING → ACTIVE` | **204**, empty body |
| `ACTIVE → ONBOARDING` | **422** `invalid status transition: ACTIVE → ONBOARDING` |
| `→ BANANA` | **422** `invalid status transition: ACTIVE → BANANA` |
| on a tenant you cannot see | **404** |

**204 means re-read.** The transition succeeded and there is no body, so the caller
must fetch the tenant again to show the new state. Confirm with a `GET`:
`"lifecycle_state":"ACTIVE"`.

A nonsense target is refused as an invalid *transition* rather than invalid *input* —
the map lookup fails closed, which is the right outcome by a slightly odd route.

---

## 7. Legal entities

### 7.1 Create

`data_residency_policy_id` is **mandatory** — the data model does not permit an entity
without one. Use the policy that tenant provisioning created. `fiscal_calendar_id`
and `primary_jurisdiction_id` are `UUID NOT NULL` with no foreign key: the
jurisdictions live in another service's database and no fiscal calendar service
exists on this platform, so neither can be checked from here.

```bash
curl -X POST $BASE/v1/entities "${hdrs[@]}" -d "{
  \"tenant_id\":\"$TENANT\",
  \"entity_code\":\"DOCACME-UK\",\"legal_name\":\"Acme UK Limited\",
  \"entity_type\":\"SUBSIDIARY\",\"default_currency_code\":\"GBP\",
  \"fiscal_calendar_id\":\"99999999-9999-9999-9999-999999999999\",
  \"primary_jurisdiction_id\":\"88888888-8888-8888-8888-888888888888\",
  \"data_residency_policy_id\":\"$POLICY\"}"
```

**Expected — 201**, `entity_status: ACTIVE`, `tax_identity_bundle_id: null`:

```json
{"legal_entity_id":"01a042b8-db38-7ba3-b9fc-cc50ca479a45",
 "tenant_id":"01a042b8-4c87-7d62-8221-c6fb5eea179c",
 "entity_code":"DOCACME-UK","legal_name":"Acme UK Limited","trading_name":null,
 "registration_number":null,"tax_identity_bundle_id":null,
 "entity_type":"SUBSIDIARY","incorporation_date":null,
 "default_currency_code":"GBP",
 "fiscal_calendar_id":"99999999-9999-9999-9999-999999999999",
 "parent_legal_entity_id":null,"entity_status":"ACTIVE",
 "primary_jurisdiction_id":"88888888-8888-8888-8888-888888888888",
 "data_residency_policy_id":"01a042b8-4c87-7d63-9817-2c8aa34d8e58",
 "created_at":"2026-08-27T10:16:46.1367627Z","updated_at":"0001-01-01T00:00:00Z", …}
```

There is **no `tax_registration_number`** and there never will be. Tax identifier
values are owned by the Tax Service; this service stores only the structural header
(§12).

### 7.2 The rejection matrix

| Input | Observed | Should be |
|---|---|---|
| duplicate `entity_code` | **409** `conflict: … entity_code DOCACME-UK` | ✅ |
| `primary_jurisdiction_id: "not-a-uuid"` | **500** `internal server error` | 400 |
| `data_residency_policy_id` that does not exist | **500** `internal server error` | 400 or 404 |
| `tenant_id` naming *another* tenant | **500** `internal server error` | 403 or 404 |

The last three are unmapped driver errors — a malformed UUID and a foreign-key
violation both reach the pgx driver and surface as 500 (§16.4). The cross-tenant case
at least *fails*, but it fails by foreign key rather than by an authorization
decision, which is luck rather than design.

### 7.3 Read paths

```bash
curl $BASE/v1/tenants/$TENANT/entities "${hdrs[@]}"    # list
curl $BASE/v1/entities/$ENTITY        "${hdrs[@]}"     # get
curl $BASE/v1/entities/$ENTITY/status "${hdrs[@]}"     # lightweight probe
```

| Call | Expected |
|---|---|
| list, tenant has none | **200** `[]` — an empty **array**, not `null` |
| list, cross-tenant | **404** `{"error":"not found"}` |
| get, own tenant | **200** full entity |
| get, cross-tenant | **404** |
| status probe | **200** `{"entity_id":"…","tenant_id":"…","entity_status":"ACTIVE"}` |
| status probe, unknown id | **404** |

The status probe exists so consumers can poll live status without pulling the whole
entity. `[]`-not-`null` matters: this endpoint used to return `null` and callers
iterating the JSON array broke on it. **Only `entities` and `workspaces` are coalesced
this way** — see §16.5 for the four list endpoints that still return `null`.

### 7.4 PATCH — and the crash

Only three fields are patchable: `legal_name`, `trading_name`,
`default_currency_code`. Governance fields are not editable by design.

```bash
curl -X PATCH $BASE/v1/entities/$ENTITY "${hdrs[@]}" \
  -d '{"legal_name":"Acme UK Ltd","default_currency_code":"EUR"}'
```

**Expected — 200**, with `updated_at` advanced and `updated_by_principal_id` set to
the *verified* principal from `X-Principal-Id`. It is never read from the request
body — the field is `json:"-"` precisely so a client cannot inject one.

> ### ⛔ Do not run this against anything you need
>
> ```bash
> curl -X PATCH $BASE/v1/entities/cccccccc-0000-4000-8000-00000000000c "${hdrs[@]}" \
>   -d '{"legal_name":"Nope"}'
> ```
>
> **Observed: the connection is dropped and the service process exits.**
>
> ```
> before: GET /healthz → HTTP 200
>         PATCH        → HTTP 000  (connection refused)
> after:  GET /healthz → connection refused
> ```
>
> ```
> panic: runtime error: invalid memory address or nil pointer dereference
> goroutine 213 [running]:
> …internal/events.(*Publisher).PublishEntityUpdated(…, 0x0, …)
> 	internal/events/publisher.go:154
> created by …internal/registry.(*Service).UpdateEntity in goroutine 211
> 	internal/registry/service.go:533
> exit status 2
> ```
>
> Any id the caller's tenant cannot see does it — a nonexistent entity, or another
> tenant's. Reproduced twice. Full analysis in §16.1.

---

## 8. The entity status state machine

`ACTIVE → {DORMANT, SUSPENDED, DISSOLVED}`, `DORMANT → {ACTIVE, DISSOLVED}`,
`SUSPENDED → {ACTIVE, DISSOLVED}`, `DISSOLVED` terminal. No hard delete, no soft
delete — status transitions only.

The implementation is race-free by construction: a single
`UPDATE … WHERE entity_status = ANY($priors)`, no read-then-write, no
`SELECT FOR UPDATE`. Zero rows affected means either "not found" or "wrong prior
state", and the service cannot tell them apart without a second query — which is why
the whole right-hand column below is 422.

```bash
curl -X POST $BASE/v1/entities/$ENTITY/status "${hdrs[@]}" -d '{"new_status":"DORMANT"}'
```

| Step | Expected |
|---|---|
| `ACTIVE → DORMANT` | **204** |
| status probe | **200** `{…,"entity_status":"DORMANT"}` |
| `DORMANT → DORMANT` (replay) | **204** — idempotent no-op |
| `DORMANT → SUSPENDED` | **422** `entity … cannot transition to SUSPENDED from its current state` |
| `DORMANT → ACTIVE` | **204** |
| from another tenant | **422** — *not* 404 |
| id that does not exist | **422** — *not* 404 |
| `→ BANANA` | **422** |

Two notes worth carrying into any consumer of this endpoint:

- **The idempotent replay is real.** The target status is added to the allowed prior
  set, so re-applying the same transition is 204 rather than 422. That is what makes a
  retried request safe.
- **422 is overloaded.** A cross-tenant probe, a typo'd UUID and a genuinely illegal
  transition are indistinguishable from the response. Defensible — it leaks nothing —
  but "422" here does not imply the entity exists. Pre-check with
  `GET /v1/entities/{id}` if you need to tell them apart. Contrast with the *status
  probe*, which does return a clean 404.

---

## 9. Workspaces and billing classification

A workspace sits beneath a tenant and may optionally scope to one legal entity.
`billing_classification` is mandatory and refused fail-closed if unrecognised —
whether a workspace can ever produce a live Zoiko charge must never be inferred from
its name or its age.

Commercial: `COMMERCIAL_STANDALONE`, `COMMERCIAL_ZOIKO_ONE`, `LEGACY_MIGRATION`.
Non-billable: `PILOT_NON_BILLABLE`, `INTERNAL`, `DEMO`, `SANDBOX`, `QA_AUTOMATION`.

### 9.1 Create

```bash
curl -X POST $BASE/v1/workspaces "${hdrs[@]}" -d "{
  \"tenant_id\":\"$TENANT\",\"name\":\"Group Finance\",\"business_unit\":\"Treasury\",
  \"billing_classification\":\"COMMERCIAL_STANDALONE\",\"billing_source\":\"DIRECT\"}"
```

**201:**

```json
{"workspace_id":"01a042ba-a4a0-74fd-ad87-6d2ca09b105f",
 "tenant_id":"01a042b8-4c87-7d62-8221-c6fb5eea179c","legal_entity_id":null,
 "name":"Group Finance","business_unit":"Treasury",
 "billing_classification":"COMMERCIAL_STANDALONE","billing_source":"DIRECT",
 "commercial_account_id":null,"status":"ACTIVE", …}
```

`legal_entity_id: null` is legitimate — a workspace may hang directly off the tenant.
Omit the field entirely rather than sending `""`.

| Input | Expected |
|---|---|
| `billing_classification: "SANDBOX"` + `legal_entity_id` | **201**, `billing_source` defaults to `NONE` |
| `billing_classification: "PREMIUM"` | **400** `invalid input: unrecognized billing_classification "PREMIUM"` |
| `billing_classification` omitted | **400** `unrecognized billing_classification ""` |
| `billing_source: "INVOICE"` | **⚠ 201** — persisted unvalidated (§16.3) |

That last row is the one to check. `ValidBillingSources` exists and is enforced on
`PATCH` but **not** on `POST`, so an unrecognised source reaches the column on create
and is rejected on update — the opposite way round from what the code comments claim.

### 9.2 Read, patch, reclassify

| Call | Expected |
|---|---|
| `GET /v1/tenants/{t}/workspaces` | **200** array (`[]` when empty) |
| `GET /v1/workspaces/{id}` | **200** |
| `GET /v1/workspaces/{id}` cross-tenant | **404** |
| `PATCH` reclassify to `PILOT_NON_BILLABLE` | **200**, `updated_at` advanced |
| `PATCH` `billing_classification: "PREMIUM"` | **400** |
| `PATCH` `commercial_account_id: "acct-1"` | **400** `commercial_account_id must be a UUID` |
| `PATCH` an id that does not exist | **404** ✅ |
| `PATCH` cross-tenant | **404** ✅ |

**Reclassification is deliberately allowed.** It decides whether a workspace can ever
produce a live charge, so a workspace created under the wrong class has to be
correctable rather than wrong for life. Every change emits `workspace.updated` so
`commercial-account-svc` observes it rather than inferring it.

Contrast the last two rows with §7.4 — `PATCH /v1/workspaces` handles the missing row
correctly and returns 404. `PATCH /v1/entities` is the same situation with the nil
check missing.

### 9.3 Archive and restore

| Step | Expected |
|---|---|
| `→ ARCHIVED` | **204** |
| `→ ARCHIVED` again | **422** `workspace … cannot transition to ARCHIVED from its current state` |
| `→ ACTIVE` | **204** |
| `→ DELETED` | **400** `invalid input: unrecognized workspace status "DELETED"` |

Archiving is **reversible** — it hides a workspace from operational use and deletes
nothing, so an accidental archive must be recoverable. And `ACTIVE → ACTIVE` is
deliberately *not* a declared transition, so a repeated archive is a 422 rather than a
silent no-op that re-stamps `updated_by_principal_id`.

Note the two different refusals: an unknown *status string* is 400 (input), a known
status from the wrong *state* is 422 (transition). That distinction is correct here
and absent from entity status (§8).

---

## 10. Hierarchies — effective dating

Parent/child relationships between two legal entities, effective-dated.
`effective_to: null` means open. `DELETE` end-dates; it does not delete.

```bash
curl -X POST $BASE/v1/entity-hierarchies "${hdrs[@]}" -d "{
  \"tenant_id\":\"$TENANT\",
  \"parent_legal_entity_id\":\"$ENTITY\",\"child_legal_entity_id\":\"$ENTITY2\",
  \"relationship_type\":\"OWNERSHIP\",\"effective_from\":\"2026-01-01T00:00:00Z\"}"
```

| Call | Expected |
|---|---|
| create, valid | **201**, `effective_to: null` |
| `relationship_type: "FRIENDSHIP"` | **⚠ 201** — not validated (§16.3) |
| parent id that does not exist | **500** — FK violation, unmapped |
| list, none yet | **⚠ 200 `null`** — not `[]` |
| list, cross-tenant | **⚠ 200 `null`** — not 404 |
| `DELETE` with valid `end_date` | **204** |
| `DELETE` an id that does not exist | **⚠ 204** — silently (§16.6) |
| list after end-dating | **200**, row still present with `effective_to` set |

The end-dating result is the thing to actually verify — the row must still be there:

```json
[{"hierarchy_id":"01a042bb-56a6-7e62-818a-075c97f48ff3",
  "parent_legal_entity_id":"01a042b8-db38-7ba3-b9fc-cc50ca479a45",
  "child_legal_entity_id":"01a042bb-529c-7b00-9964-fa0c03e6697e",
  "relationship_type":"OWNERSHIP",
  "effective_from":"2026-01-01T05:30:00+05:30",
  "effective_to":"2026-12-31T05:30:00+05:30", …}]
```

`+05:30` is the database session timezone, not a data error — you sent
`2026-12-31T00:00:00Z` and that is the same instant.

**The list is not ordered.** After end-dating one of two rows, the still-open row came
back first. Do not depend on the order.

---

## 11. Jurisdiction assignments

Effective-dated, same end-date-never-delete pattern. `jurisdiction_id` is validated
**synchronously and fail-closed** against `jurisdiction-rules-svc` — a bad
jurisdiction reference would silently propagate tax, payroll and filing failures
across the platform.

**With the stub validator, every id is accepted.** The row below marked *(stub)*
changes against the real service — see §14.2.

```bash
curl -X POST $BASE/v1/entities/$ENTITY/jurisdictions "${hdrs[@]}" -d '{
  "jurisdiction_id":"88888888-8888-8888-8888-888888888888",
  "assignment_type":"PRIMARY","effective_from":"2026-01-01T00:00:00Z",
  "source_basis":"incorporation"}'
```

| Call | Expected |
|---|---|
| assign, valid | **201** |
| list, none yet | **⚠ 200 `null`** |
| list, populated | **200** array |
| `assignment_type: "SOMETIMES"` | **⚠ 201** — not validated (`PRIMARY`, `SECONDARY`, `TAX_ONLY`, `FILING_ONLY` are the declared values) |
| unknown `jurisdiction_id` *(stub)* | **201** — would be **400** against the real service |
| `DELETE …?end_date=…` | **204**, row retained with `effective_to` |

---

## 12. Tax identity bundles

**Structural header only.** `legal_entity_id`, `jurisdiction_id`, effective dates,
status, data classification. The actual tax registration number and all evidence
artifacts belong to the Tax Service, to keep regulated PII in one place. Do not add
identifier fields here.

```bash
curl -X POST $BASE/v1/entities/$ENTITY/tax-identity-bundles "${hdrs[@]}" -d '{
  "jurisdiction_id":"88888888-8888-8888-8888-888888888888",
  "effective_from":"2026-01-01T00:00:00Z","data_classification":"CONFIDENTIAL"}'
```

**201**, always `status: PENDING`:

```json
{"tax_identity_bundle_id":"01a042bb-6ae5-7c79-a68d-376c2e0df5d6",
 "tenant_id":"…","legal_entity_id":"…",
 "jurisdiction_id":"88888888-8888-8888-8888-888888888888",
 "status":"PENDING","effective_from":"2026-01-01T00:00:00Z","effective_to":null,
 "data_classification":"CONFIDENTIAL", …}
```

| Call | Expected |
|---|---|
| create, valid | **201**, `status: PENDING` |
| `data_classification: "TOP_SECRET"` | **400** `invalid input: invalid data classification "TOP_SECRET"` |
| `data_classification` omitted | **201** — optional, validated only when present |
| list, none yet | **⚠ 200 `null`** |
| get by id | **200** |
| `PENDING → ACTIVE` | **204** |
| `ACTIVE → PENDING` | **⚠ 204** — no state machine |
| `→ BANANA` | **⚠ 204** — and it persists |

That last row is worth seeing in full. After transitioning to `BANANA`:

```json
{"tax_identity_bundle_id":"01a042bb-6ae5-7c79-a68d-376c2e0df5d6",
 "status":"BANANA", …}
```

`TaxIdentityBundleStatus` declares `PENDING`, `ACTIVE`, `EXPIRED`, `SUPERSEDED` — and
unlike tenants, entities and workspaces, **nothing checks the value or the
transition**. The service passes the string to the store, which writes it to a
`VARCHAR(50)` with no constraint (§16.3).

---

## 13. Residency policies, regions, tenant→region

### 13.1 Regions are read-only

IaC-provisioned. There is no write endpoint, by design.

| Call | Expected |
|---|---|
| `GET /v1/residency-regions`, none seeded | **⚠ 200 `null`** |
| `GET /v1/residency-regions/{id}`, unknown | **404** |

On a fresh database this list is empty, which is why the tenant→region lookup below
cannot resolve. `seed-demo-registry.sql` inserts one (`demo-eu-west`).

### 13.2 Policies

| Call | Expected |
|---|---|
| create, valid | **201**, `active_flag: true`, `residency_region_id: null` if omitted |
| `residency_region_id` naming a region that does not exist | **500** — FK violation, unmapped |
| `residency_mode: "WHEREVER"`, `conflict_resolution_mode: "SHRUG"` | **⚠ 201** — neither validated |
| duplicate `policy_code` | **⚠ 201** — `policy_code` has no unique constraint |
| `GET` unknown id | **404** |

The duplicate case is interesting: the store maps SQLSTATE 23505 on this table to
`ErrConflict`, but migration 000001 never declares `policy_code` unique, so that
branch is unreachable.

Declared values, for reference: `residency_mode` ∈ `STRICT_REGION`,
`PREFERRED_REGION`, `FOLLOW_ENTITY`; `conflict_resolution_mode` ∈ `FAIL_CLOSED`,
`LOG_AND_PROCEED`, `ESCALATE`.

### 13.3 The tenant→region lookup, and the 409 you cannot clear

`GET /v1/tenants/{id}/residency-region` is the real lookup the Global Traffic &
Residency Manager's ingress uses. It walks
`Tenant.default_data_residency_policy_id → DataResidencyPolicy.residency_region_id →
ResidencyRegion.region_code`.

```bash
curl $BASE/v1/tenants/$TENANT/residency-region "${hdrs[@]}"
```

**Expected on a freshly provisioned tenant — 409, and this is a real state, not a
failure:**

```json
{"error":"tenant's residency policy has no region assigned","correlation_id":"corr-1"}
```

Provisioning always creates the default policy with **no region**, so the lookup is
409 from the moment the tenant exists.

**There is no API that can clear it.** Assigning a region needs either the tenant's
`default_data_residency_policy_id` repointed at a policy that has one, or that
policy's `residency_region_id` set — and the service exposes neither a `PATCH` on
residency policies nor any way to change a tenant's default policy. Creating a new
policy with a region does not help; the tenant still points at the original. Today
this is only resolvable by SQL, or by seeding the tenant with
`seed-demo-registry.sql`, which sets it up front (§16.9).

| Call | Expected |
|---|---|
| tenant exists, policy has no region | **409** `region unresolved` |
| tenant does not exist | **404** |
| cross-tenant | **404** |
| policy has a valid region | **200** `{"tenant_id":"…","region_code":"demo-eu-west","region_name":"Demo EU West"}` |

409 and 404 are deliberately distinct here: "unresolved" and "no such tenant" are
different operational problems.

---

## 14. Fail-closed dependencies — *inferred, not run*

Everything above ran against the permit-all stubs. This section is read from the
source and **has not been executed**; treat the status codes as claims to verify, not
as observations.

### 14.1 authorization-svc

Every mutation calls `authz.Authorize` before touching the store. `resource` and
`action` are flattened into the action vocabulary authorization-svc stores —
`("entity.hierarchy", "create")` → `ENTITY_HIERARCHY_CREATE`.

| Condition | Expected |
|---|---|
| decision `GRANTED` | the operation proceeds |
| decision anything else | **403** `{"error":"forbidden"}` |
| authorization-svc unreachable, slow, non-200, or unreadable body | **503** `upstream service unavailable — request rejected` |
| no `X-Principal-Id` on the request | **401** `unauthenticated: no verified principal on request` |

Both `GRANTED` and `DENIED` come back as **HTTP 200** — the status reflects that the
evaluation succeeded, not its outcome — so the body must always be read. Anything that
cannot be read as a decision fails closed.

**Two ways this bites in practice, both of which have already caused outages here:**

1. **Envelope forwarding.** authorization-svc enforces the same §4 contract on
   `POST /v1/authorize`. If the outbound call does not forward the caller's envelope,
   it answers 401 `envelope_incomplete`, this client reads any non-200 as
   "unavailable", and **every authorized write in the platform 503s while
   authorization-svc is healthy and answering correctly**. `Envelope.ApplyTo` exists
   for this; a header that `Parse` reads and `ApplyTo` omits silently disappears on
   every internal hop.
2. **Timeout.** Default 2s, overridable with `AUTHZ_HTTP_TIMEOUT_MS`.
   authorization-svc writes a decision-log row before it answers, so against a distant
   managed database the call has been measured at ~1.6s. The failure is
   `context canceled` at exactly 2.000s, surfaced as a 503, for a reason that has
   nothing to do with authorization. If you see 503s clustered at exactly two seconds,
   raise this before debugging anything else.

**Test it properly** by pointing `AUTHZ_SERVICE_URL` at a real `authorization-svc`
with **no** grants seeded for this service — which, per §3a, is the default state of
`seed-demo-rbac.ps1`. Every write in sections 6–13 should become 403 and every read
should keep working.

### 14.2 jurisdiction-rules-svc

`GET {base}/v1/jurisdictions/{id}`, 2s timeout, checked on entity creation,
jurisdiction assignment and tax bundle creation.

| Upstream | Expected |
|---|---|
| 200 | proceeds |
| 404 | **400** `invalid input: jurisdiction_id <id> not found in Jurisdiction Rules Service` |
| unreachable / any other status | **503** `upstream service unavailable` |

This is the state the console screenshot shows: `jurisdiction-rules-svc` was
unreachable, so the picker was unavailable *and* the write would have been refused 503
whatever UUID was typed. The banner in the UI says exactly that.

### 14.3 Kafka

Events are published in a goroutine and **failures are logged, never propagated** — a
write succeeds whether or not the event lands. Topic `zoiko.entity.events`.

Event names, from `internal/events`: `tenant.created`, `entity.created`,
`entity.updated`, `entity.status.changed`, `entity.hierarchy.changed`,
`entity.jurisdiction.changed`, `workspace.created`, `workspace.updated`,
`workspace.status.changed`.

Two known gaps in what those events carry:

- `entity.status.changed` sends an **empty `previous_status`** — the race-free
  single-UPDATE design never reads the prior state. `workspace.status.changed` does
  carry it, because its CTE has it in hand.
- `PublishEntityHierarchyChanged` on an end-date emits a **synthetic object** with only
  `hierarchy_id` and `effective_to` populated; every other field is zero.

---

## 15. The admin console (`/admin/tenants`)

Not exercised in a browser for this document. Two things are worth knowing before you
open it, both visible in the screenshots that prompted this guide.

**"This session's tenant is not in the registry — 404 not found."** The console's demo
identity is a fixed tenant id that nothing creates. Run `seed-demo-registry.ps1`
(§3a). This is not a console bug; it is the console correctly reporting that the
registry has never heard of the session's tenant.

**"jurisdiction-rules-svc could not be reached, so the picker is unavailable."** Start
`jurisdiction-svc` (:8082). Until then the entity form falls back to a free-text UUID
field, and — as the banner itself says — the write will be refused 503 whatever you
paste, because validation is fail-closed (§14.2).

The console talks to `:8081` directly (`ZOIKO_TENANT_REGISTRY_URL` in `.env.local`,
`ZOIKO_USE_GATEWAY=false`), builds the full §4 envelope for every call, and sends the
session identity as `X-Tenant-Id` / `X-Principal-Id`. Server actions cover
provisioning, lifecycle, entity create/update/status, workspace create, hierarchy
create/end-date, jurisdiction assign/end-date, and residency policy create — i.e.
sections 6–13 minus workspace patch/archive.

---

## 16. Defects this guide found

Ordered by how much they matter. Everything here was observed, not inferred.

### 16.1 `PATCH /v1/entities/{id}` on an invisible id crashes the process

**Severity: any authenticated caller can take the service down with one request.**

`store.UpdateEntity` returns `(nil, nil)` when no row matches — correct, and the same
shape as `UpdateWorkspace`. But `registry.Service.UpdateEntity` does not nil-check
before handing the pointer to a goroutine:

```go
e, err := s.store.UpdateEntity(ctx, legalEntityID, req)
if err != nil { … }
go s.events.PublishEntityUpdated(ctx, e, req.CorrelationID)   // e may be nil
```

`PublishEntityUpdated` dereferences `entity.TenantID` immediately. The panic is in a
goroutine, so chi's `Recoverer` cannot catch it and the process exits. Because this
service is Tier 0 — nothing else on the platform can resolve a tenant while it is down
— the blast radius is the whole estate.

Triggered by a nonexistent id **or** another tenant's id, so it is reachable by any
caller who can authenticate, without guessing a real id.

The fix is the two lines `UpdateWorkspace` already has:

```go
if e == nil { return nil, ErrNotFound }
```

`GetEntity`, `GetWorkspace`, `UpdateWorkspace` and `GetResidencyPolicy` all do this;
`UpdateEntity` is the one that does not.

### 16.2 `AUTHZ_PLATFORM_SCOPE_ID` is unreachable on the only path that uses it

The envelope middleware demands `X-Tenant-Id` on every material write, including
`POST /v1/tenants`. `Service.authorize` falls back to the platform scope only when the
context has no tenant — which can no longer happen. So tenant provisioning is
authorized against whatever tenant the caller put in the header, and grants seeded
against the platform scope are never consulted.

Under `ZS_ENVELOPE_ENFORCEMENT=observe` the fallback works, which means the
authorization scope for tenant creation silently depends on the enforcement mode.
`envelope.Policy` already has an `ExemptPaths` mechanism built for exactly this class
of bootstrap endpoint — `gateway-auth-svc`'s `/verify` is the documented precedent.

### 16.3 Validation gaps — values that persist unchecked

| Field | Endpoint | Declared values | Observed |
|---|---|---|---|
| `billing_source` | `POST /v1/workspaces` | `NONE`, `DIRECT`, `ZOIKO_ONE_BUNDLE` | `"INVOICE"` → **201** |
| `relationship_type` | `POST /v1/entity-hierarchies` | `OWNERSHIP`, `REPORTING`, `OPERATIONAL` | `"FRIENDSHIP"` → **201** |
| `assignment_type` | `POST …/jurisdictions` | `PRIMARY`, `SECONDARY`, `TAX_ONLY`, `FILING_ONLY` | `"SOMETIMES"` → **201** |
| `residency_mode` | `POST /v1/residency-policies` | `STRICT_REGION`, `PREFERRED_REGION`, `FOLLOW_ENTITY` | `"WHEREVER"` → **201** |
| `conflict_resolution_mode` | same | `FAIL_CLOSED`, `LOG_AND_PROCEED`, `ESCALATE` | `"SHRUG"` → **201** |
| `status` | `POST /v1/tax-identity-bundles/{id}/status` | `PENDING`, `ACTIVE`, `EXPIRED`, `SUPERSEDED` | `"BANANA"` → **204**, persisted |

All six columns are plain `VARCHAR` with no `CHECK`, so the value is stored and read
back as though it meant something. The `billing_source` one is notable because the
validation map **exists** and is applied on `PATCH` but not on `POST` — and the comment
on `ValidBillingSources` describes the create-path gap as though it had been fixed.

`conflict_resolution_mode` is the one with teeth: it decides what happens when
residency and jurisdiction obligations conflict, and `FAIL_CLOSED` is the safe default
that an unvalidated string silently opts out of. Note that `seed-demo-registry.sql`
already writes two undeclared values (§3a).

### 16.4 Foreign-key and type violations surface as 500

| Call | Observed | Should be |
|---|---|---|
| entity with a malformed `primary_jurisdiction_id` | 500 | 400 |
| entity with an unknown `data_residency_policy_id` | 500 | 400/404 |
| entity naming another tenant's `tenant_id` | 500 | 403/404 |
| hierarchy naming a nonexistent entity | 500 | 400/404 |
| policy naming a nonexistent region | 500 | 400/404 |

The store maps SQLSTATE 23505 (unique violation) to `ErrConflict` but nothing maps
23503 (foreign key) or 22P02 (invalid text representation). Each becomes an unhandled
error, logged in full and returned as `internal server error`. The cross-tenant entity
create is the one that matters — it currently fails *by accident*, because the policy
id happens to belong to another tenant, not because anything checked.

### 16.5 Four list endpoints return `null` instead of `[]`

`ListEntities` and `ListWorkspaces` coalesce nil to an empty slice, with a comment
recording that this endpoint returned `null` in production and broke callers iterating
the array. The same fix was never applied to:

- `GET /v1/entities/{id}/hierarchies`
- `GET /v1/entities/{id}/jurisdictions`
- `GET /v1/entities/{id}/tax-identity-bundles`
- `GET /v1/residency-regions`

`GET /v1/entities/{id}/hierarchies` **cross-tenant** also returns `200 null` rather
than the 404 the tenant-scoped reads give, because `ListHierarchies` has no
`assertTenantScope` call at all. It leaks nothing, but it is inconsistent.

### 16.6 `DELETE` on a nonexistent id returns 204

Both end-dating routes report success for an id that does not exist —
`store.EndDateHierarchy` never checks rows affected. A client retrying an end-date
against a typo'd id is told it worked.

### 16.7 Internal function names leak into error bodies

```json
{"error":"store.CreateTenantWithDefaultResidencyPolicy: conflict: resource already exists: tenant_code DOCACME"}
{"error":"store.CreateEntity: conflict: resource already exists: entity_code DOCACME-UK"}
```

`fmt.Errorf("store.X: %w", err)` wrapping reaches the client because `ErrConflict` and
`ErrInvalidInput` are rendered with `err.Error()`. Harmless today, but it is a string
clients will parse and a refactor will break.

### 16.8 `openapi.yaml` is missing six live routes

The spec documents 23 operations. The router registers **29**. Undocumented:

```
GET    /v1/tenants/{tenantID}/workspaces
POST   /v1/workspaces
GET    /v1/workspaces/{workspaceID}
PATCH  /v1/workspaces/{workspaceID}
POST   /v1/workspaces/{workspaceID}/status
GET    /v1/tenants/{tenantID}/residency-region
```

The entire workspace resource — including `billing_classification`, the field that
decides whether anything can be charged — is absent from the published contract.

### 16.9 A tenant's residency region cannot be set through the API

Covered in §13.3. Provisioning always creates a region-less default policy, and no
endpoint can assign a region to it or repoint the tenant at another policy. Every
freshly provisioned tenant is permanently 409 on `/residency-region` unless someone
runs SQL.

---

## 17. Appendix: routes, enums, environment

### Routes

29 under `/v1`, plus three unversioned. ✅ = exercised in this document.

| Method | Path | Success | |
|---|---|---|---|
| POST | `/v1/tenants` | 201 | ✅ |
| GET | `/v1/tenants/{tenantID}` | 200 | ✅ |
| POST | `/v1/tenants/{tenantID}/lifecycle` | 204 | ✅ |
| GET | `/v1/tenants/{tenantID}/residency-region` | 200 | ✅ |
| GET | `/v1/tenants/{tenantID}/entities` | 200 | ✅ |
| GET | `/v1/tenants/{tenantID}/workspaces` | 200 | ✅ |
| POST | `/v1/entities` | 201 | ✅ |
| GET | `/v1/entities/{entityID}` | 200 | ✅ |
| PATCH | `/v1/entities/{entityID}` | 200 | ✅ |
| GET | `/v1/entities/{entityID}/status` | 200 | ✅ |
| POST | `/v1/entities/{entityID}/status` | 204 | ✅ |
| POST | `/v1/workspaces` | 201 | ✅ |
| GET | `/v1/workspaces/{workspaceID}` | 200 | ✅ |
| PATCH | `/v1/workspaces/{workspaceID}` | 200 | ✅ |
| POST | `/v1/workspaces/{workspaceID}/status` | 204 | ✅ |
| POST | `/v1/entity-hierarchies` | 201 | ✅ |
| GET | `/v1/entities/{entityID}/hierarchies` | 200 | ✅ |
| DELETE | `/v1/entity-hierarchies/{hierarchyID}` | 204 | ✅ |
| POST | `/v1/entities/{entityID}/jurisdictions` | 201 | ✅ |
| GET | `/v1/entities/{entityID}/jurisdictions` | 200 | ✅ |
| DELETE | `/v1/entity-jurisdictions/{assignmentID}` | 204 | ✅ |
| POST | `/v1/residency-policies` | 201 | ✅ |
| GET | `/v1/residency-policies/{policyID}` | 200 | ✅ |
| GET | `/v1/residency-regions` | 200 | ✅ |
| GET | `/v1/residency-regions/{regionID}` | 200 | ✅ |
| POST | `/v1/entities/{entityID}/tax-identity-bundles` | 201 | ✅ |
| GET | `/v1/entities/{entityID}/tax-identity-bundles` | 200 | ✅ |
| GET | `/v1/tax-identity-bundles/{bundleID}` | 200 | ✅ |
| POST | `/v1/tax-identity-bundles/{bundleID}/status` | 204 | ✅ |
| GET | `/healthz` `/readyz` `/metrics` | 200 | ✅ |

### State machines

```
tenant lifecycle_state
  ONBOARDING  → ACTIVE
  ACTIVE      → SUSPENDED, OFFBOARDING
  SUSPENDED   → ACTIVE, OFFBOARDING
  OFFBOARDING → (terminal)

legal entity entity_status
  ACTIVE    → DORMANT, SUSPENDED, DISSOLVED
  DORMANT   → ACTIVE, DISSOLVED
  SUSPENDED → ACTIVE, DISSOLVED
  DISSOLVED → (terminal)

workspace status
  ACTIVE   ⇄ ARCHIVED          (reversible; ACTIVE→ACTIVE is NOT declared)

tax identity bundle status
  PENDING, ACTIVE, EXPIRED, SUPERSEDED   — declared, but NOT enforced (§16.3)
```

Separately, `tenant.status` is `ACTIVE | SUSPENDED | ARCHIVED` with **no state machine
and no endpoint**. Only `lifecycle_state` transitions.

### Enums

| Type | Values | Enforced? |
|---|---|---|
| `EntityType` | `SUBSIDIARY`, `BRANCH`, `HOLDING`, `OPERATIONAL` | not checked |
| `BillingClassification` | `COMMERCIAL_STANDALONE`, `COMMERCIAL_ZOIKO_ONE`, `LEGACY_MIGRATION`, `PILOT_NON_BILLABLE`, `INTERNAL`, `DEMO`, `SANDBOX`, `QA_AUTOMATION` | ✅ create + patch |
| `BillingSource` | `NONE`, `DIRECT`, `ZOIKO_ONE_BUNDLE` | patch only ⚠ |
| `HierarchyRelationshipType` | `OWNERSHIP`, `REPORTING`, `OPERATIONAL` | ⚠ no |
| `JurisdictionAssignmentType` | `PRIMARY`, `SECONDARY`, `TAX_ONLY`, `FILING_ONLY` | ⚠ no |
| `ResidencyMode` | `STRICT_REGION`, `PREFERRED_REGION`, `FOLLOW_ENTITY` | ⚠ no |
| `ConflictResolutionMode` | `FAIL_CLOSED`, `LOG_AND_PROCEED`, `ESCALATE` | ⚠ no |
| `Classification` | `PUBLIC`, `INTERNAL`, `CONFIDENTIAL`, `RESTRICTED` | ✅ when present |

### Environment variables

| Variable | Default | Notes |
|---|---|---|
| `ENV` | `local` | `production`/`staging` enable the startup safety checks |
| `PORT` | `8081` | |
| `DB_HOST` `DB_PORT` `DB_NAME` `DB_USER` `DB_PASSWORD` | `localhost` `5432` `tenant_entity_registry` `postgres` — | fail-fast at startup |
| `DB_SSLMODE` | `require` | `disable` refused in prod/staging |
| `DB_SCHEMA` | *(empty)* | a schema that does not exist is **not** a connect error — it fails on first query |
| `DB_OPTIONS` | *(empty)* | `default_query_exec_mode=exec statement_cache_capacity=0` behind a transaction pooler |
| `KAFKA_BROKERS` | `localhost:9092` | explicit empty = no backbone |
| `KAFKA_EVENTS_TOPIC` | `zoiko.entity.events` | |
| `AUTHZ_SERVICE_URL` | `http://authorization-svc` | placeholder → permit-all stub; fatal in prod/staging |
| `AUTHZ_PLATFORM_SCOPE_ID` | *(empty)* | required in prod/staging; see §16.2 |
| `AUTHZ_HTTP_TIMEOUT_MS` | `2000` | raise for a distant authorization-svc |
| `JURISDICTION_RULES_URL` | `http://jurisdiction-rules-svc` | placeholder → accept-all stub |
| `ZS_ENVELOPE_ENFORCEMENT` | `write-strict` | `strict` \| `write-strict` \| `observe`; an unrecognised value falls back to the default, never to `observe` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://otel-collector:4318` | unreachable is harmless |

### A test-run checklist

```
[ ] go build ./... && go vet ./...                → silent, exit 0
[ ] go test ./...                                 → 5 ok, 60 pass, 9 skip
[ ] go test -tags=integration ./internal/store/   → ok, ~25s
[ ] TEST_DATABASE_URL=… go test ./internal/store/ → the 9 skips now pass
[ ] /healthz, /readyz                             → 200 twice
[ ] write with no envelope                        → 401, five violations
[ ] read with no envelope                         → 200 + X-Envelope-Contract: violated
[ ] provision tenant                              → 201, ACTIVE + ONBOARDING
[ ] duplicate tenant_code                         → 409
[ ] cross-tenant GET                              → 404, never 403
[ ] lifecycle ONBOARDING→ACTIVE                   → 204, then re-read
[ ] create entity                                 → 201, entity_status ACTIVE
[ ] list entities, empty tenant                   → 200 []  (not null)
[ ] entity status ACTIVE→DORMANT→ACTIVE           → 204 each; replay 204; illegal 422
[ ] workspace bad classification                  → 400
[ ] archive → re-archive → restore                → 204, 422, 204
[ ] end-date hierarchy                            → 204, row retained with effective_to
[ ] tenant→region                                 → 409 region unresolved
[ ] PATCH entity, unknown id                      → ⛔ process dies (§16.1)
```
