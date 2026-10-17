# Group 2 — Governance Platform: Testing Progress

**Scope:** 7 services (jurisdiction-rules-svc, governance-decision-log-svc, policy-svc, authorization-svc, obligations-svc, workflow-svc, schema-registry-svc)

**Last Updated:** 2026-10-17

---

# jurisdiction-rules-svc (Port 8082) — Service 10 · Group 2 (1/7) — Tier 0 Jurisdiction Pack & Rule Engine

**Group:** 2 — Governance Platform
**Status:** ✅ **ALL TESTS PASSING** (236 pass + 65 registryit-integration + 34 store-integration · 0 skip · 0 fail) · **ZS-QA-001 live audit: GREEN** (service 10 of 17; first of Group 2)
**Last Verified:** 2026-10-17 — static checks, integration suites (embedded Postgres 16 + real Postgres 16), race suite in Docker, and a full live round trip (real binary + real Postgres, scratch DB `juris_live`) including the tenant-entity-registry contract proof

---

## 🔬 ZS-QA-001 Formal Verification (2026-10-17)

**Live acceptance result:** ✅ **mod verify/build/vet/gofmt clean; unit + integration suites green; full live E2E green** (against Doc 03 §8.2, its own `openapi.yaml`, ZS-JUR-001, V-001 PDC-02, and the Governance Platform audit 2026-09-28 §1/7).

Findings in this pass:

| Finding | Root Cause | Fix / Status |
|---------|-----------|--------------|
| **A — genuine gofmt drift in 9 files (line-ending independent)** | `internal/envelope/contract.go` (generated, 4-space indent), `internal/envelope/reason.go`, `internal/domain/types.go`, and 6 test files (`consumer/entity_test.go`, `envelope/middleware_test.go`, `events/publisher_test.go`, `handler/handler_test.go`, `problem/problem_test.go`, `telemetry/telemetry_test.go`). All other `gofmt -l` entries were CRLF-only artifacts of the Windows host. | ✅ `gofmt -w` on all 9 → **0 genuine drift**; build/vet re-verified clean |
| **B — `openapi.yaml` documents `POST /v1/validate-action` but no route exists** | The spec (Rules tag, §8.2 "validate-action" API) kept the endpoint; no handler was ever registered — live request returns chi's plain-text `404 page not found`. Route parity is otherwise **108/108** (every code route is documented), so this is a spec→code drift only. | ⚠️ Unresolved — needs a decision: implement the endpoint or remove it from the spec (Architecture Decision #5) |
| **C — no functional defects observed** | — | Live E2E first-pass green: idempotent create/replay, maker-checker activation, optimistic concurrency, exclusion-constraint overlap guard, bitemporal rule-pack resolution, fail-closed reads for inactive jurisdictions, transactional outbox, contract-compatible wire format |

**Verification evidence:**

- Static: `go mod verify` clean, `go build ./...` clean, `go vet ./...` clean, `gofmt -l` clean (line-ending independent)
- Unit + store suite: **236 pass / 0 skip / 0 fail** (`go test ./...` with `TEST_DATABASE_URL` → real Postgres 16); 11 test-bearing packages (authz, config, consumer, domain, envelope, events, handler, problem, resolver, store, telemetry)
- Integration (tag-gated): `go test -tags integration ./internal/registryit/` → **65 pass / 0 fail** (embedded Postgres 16 harness, self-applies migrations `000001–000020` filename-sorted); `go test -tags integration ./internal/store/` → **34 pass / 0 fail** (real Postgres; CI store-integration stage)
- Race suite (CI-equivalent, Docker `golang:1.26` + real Postgres 16 on `gdl-race-net`): all packages ok, **0 data races** (CI runs the same command without integration tags)
- Contract: `openapi.yaml` present (OpenAPI **3.1.0**, version 1.1.0) — `openapi-spec-validator` **OK**; route parity 108/108 code routes documented (one spec-only phantom path — finding B); RFC 9457 `application/problem+json` error bodies
- Wiring: envelope middleware (`ServicePolicy`, `LegalEntityID: NotRequired`) → IdempotencyMiddleware → resolver (config-gated) → authz client (mTLS variant available, stub only in `ENV=local`) → entity consumer (`zoiko.entity.events`, projects `entity.created` / `entity.jurisdiction.changed`) → outbox worker → telemetry/health
- **Live E2E (real binary + real Postgres 16, DB `juris_live`, 2026-10-17):**

```text
GET  /healthz, /readyz                    -> 200 / 200
GET  /v1/jurisdictions                    -> 200  []
POST /v1/admin/jurisdictions              -> 201  (replay same key+body -> 201 stored response, same id)
POST (same Idempotency-Key, other body)   -> 409  "already used for a different request"
GET  /v1/jurisdictions/{id}               -> 200  full row, effective_to=null
TER contract proof: decode as TER Ref     -> HTTP 200  id=<uuid> code=JUR-LIVE-01 active=true effective_to=<nil>
POST .../jurisdictions/{id}/rules         -> 201  DRAFT row_version=1
POST .../transition ACTIVE (approver)     -> 200  (X-Approval-Reference required; ETag "2")
POST .../transition ACTIVE (no ref)       -> 400  missing_approval_reference
POST .../transition ACTIVE (as author)    -> 403  self_approval_forbidden (maker-checker)
POST .../transition DRAFT->SUPERSEDED     -> 409  invalid_transition (state machine ACTIVE<-DRAFT, SUPERSEDED<-ACTIVE)
POST .../transition ACTIVE->SUPERSEDED    -> 200
POST .../transition stale If-Match "999"  -> 412  precondition_failed
GET  .../rules, .../rule-pack             -> 200  pack = ancestor chain, one winning rule per (domain,code)
GET  /v1/jurisdictions/{unknown}          -> 404  jurisdiction_not_found
POST .../deactivate; GET after            -> 200; 404 (inactive fails closed for TER)
POST write without Idempotency-Key        -> 400  envelope_incomplete
POST write with bad X-Source-Channel      -> 400  envelope_incomplete (source_channel)
GET  read without X-Tenant-Id             -> 200 + X-Envelope-Contract: violated (write-strict default; see posture note)
GET  /v1/entities/{id}/jurisdictions      -> 200  [] (projection needs Kafka)
GET  /v1/jurisdiction-support/JUR-LIVE-01 -> 200  supported=false, NO_ROLLOUT
POST /v1/rule-resolutions:resolve         -> 404  (RESOLVER_ENABLED=false -> resolver routes not registered; startup log says so)
POST /v1/validate-action                  -> 404  page not found (phantom spec path — finding B)
DB (event_outbox, KAFKA_BROKERS set)      -> jurisdiction.created + jurisdiction.rule.updated; unpublished, attempts=0, last_error null
DB (rule_status_history)                  -> DRAFT@author / ACTIVE@approver+APR-LIVE-001/002 / SUPERSEDED@author (fully attributed)
DB (final)                                -> R-LIVE-101 SUPERSEDED rv=3; R-LIVE-102 ACTIVE rv=2; idempotency_keys=10
```

### Closes the group1-noted items for this service

| group1 item (`group1-identity-foundation-testing.md`) | Status |
|---|---|
| **Contract for `/v1/jurisdictions/{id}` must match `jurisdiction.Ref`** (architecture decision, ~line 223) | ✅ **Proven live**: `GET /v1/jurisdictions/{id}` decoded with tenant-entity-registry's exact `Ref` struct (`jurisdiction_id`, `jurisdiction_code`, `active_flag`, `effective_to`) → HTTP 200; 404 for unknown/inactive, 503 contract for store outages (callers fail closed) |
| **"Ship jurisdiction-rules-svc + OpenAPI spec"** (blocker, ~line 235) | ✅ Service shipped on **:8082** (compose `jurisdiction-svc`, Traefik `PathPrefix /jurisdiction` with `gateway-auth` forward-auth); ✅ `openapi.yaml` present, OpenAPI 3.1.0, validates clean, route parity 108/108 (1 spec-only phantom path — finding B) |

---

## ✅ Completed Fixes

| Area | Issue | Resolution |
|------|-------|------------|
| **Build** | No build errors | Clean build |
| **Tests** | All pass (self-contained; no OIDC_ISSUER needed) | 236/236 + integration 65/65 (registryit) + 34/34 (store, tagged) · 0 skip |
| **AuthZ Client** | Missing `ForwardTo` on outbound call to authorization-svc | Added `svcenvelope.ForwardTo(ctx, req)` before `c.http.Do(req)` (prior pass; regression-tested) |
| **Envelope Contract** | Already rolled out | `internal/envelope/contract.go` (generated by rollout script; `LegalEntityID: NotRequired`) |
| **Events/Outbox** | Event publishing + outbox relay | Working; transactional outbox persists in `event_outbox` when `KAFKA_BROKERS` is configured (verified live) |
| **Rule overlap** | Check-then-act race allowed duplicate rules for same code/period | Exclusion constraint (migration `000006_add_exclusion_constraint`); retry + `409 rule_overlap` |
| **Maker-checker** | Activation could be self-approved | `X-Approval-Reference` required for ACTIVE; independent approver enforced (`403 self_approval_forbidden`); approver + reference recorded in `rule_status_history` (verified live) |
| **Optimistic concurrency** | — | `If-Match`/ETag (`row_version`) on transitions; stale version → `412 precondition_failed` (verified live) |
| **gofmt drift** | 9 files with genuine (line-ending independent) drift | `gofmt -w`; remaining `gofmt -l` entries are CRLF-only host artifacts |

---

## ⚠️ Partial / Pending (Within This Repo)

| Item | Status | Details |
|------|--------|---------|
| **Phantom spec path** | ⚠️ Finding B | `POST /v1/validate-action` in `openapi.yaml` only; live 404. Implement or remove (decision #5) |
| **Pack signing** | ⚠️ Config | `PACK_SIGNING_KEY_REF`/`PACK_SIGNING_KEY_FILE` unset → sign command answers 503; dev default (startup log confirms) |
| **Rule resolver** | ⚠️ Config | `RESOLVER_ENABLED` default false; resolver routes (`/v1/rule-resolutions:resolve` etc.) are not even registered → 404; enabled with `RESOLVER_RING`/`RESOLVER_REGION` in production |
| **Envelope posture** | ℹ️ By design | Compose does not set `ZS_ENVELOPE_ENFORCEMENT` for this service → default **write-strict**: writes are enforced (Idempotency-Key mandatory), reads missing fields are admitted with `X-Envelope-Contract: violated`. Promote to `strict` once all callers send the full envelope in production |
| **Kafka relay (empty brokers)** | ℹ️ By design | With empty `KAFKA_BROKERS` the publisher is a no-op and no outbox rows persist; with brokers configured rows are written and relayed (verified live). Compose always configures brokers |
| **Application Control Policy** | ⚠️ Infra | Windows blocks test binaries intermittently; tests pass when they run — infra issue |

---

## ❌ External Dependencies (Not In This Repo)

These are **separate services/repos** owned by other teams. This service is wired to call them; they return stub/`UPSTREAM_NOT_CONFIGURED` until counterparties exist.

| Dependency | Owner Repo | Config Field | Why Not Done Here | What's Wired |
|------------|------------|--------------|-------------------|--------------|
| **Authorization Service** | `authorization-svc` | `AuthZServiceURL` | In scope (Group 2) but separate service | `authz.NewClient` with `ForwardTo` ✅ — fails fast in staging/prod if URL empty or placeholder |
| **Tenant Entity Registry** | `tenant-entity-registry-svc` | `KAFKA_ENTITY_TOPIC` | In scope (Group 1, port 8081) | Kafka consumer projects `entity.created` / `entity.jurisdiction.changed` ✅ (contract wire format proven live, both directions) |
| **Kafka/Event Backbone** | Platform infra | `KAFKA_BROKERS` | Infrastructure, not code | Producer + outbox worker wired; transactional outbox verified live |
| **MTLS Management** | `mtls-management-svc` | `MTLSManagementServiceURL` | Optional mTLS client identity | Wired if `AuthzMTLSEnabled=true` |

---

## 📋 Architecture Decisions Requiring Human Input

| # | Decision | Impact |
|---|----------|--------|
| 1 | **Jurisdiction Rules Service contract** — wire format for `/v1/jurisdictions/{id}` | ✅ Closed: `domain.Jurisdiction` matches `tenant-entity-registry-svc`'s `Ref` (proven live decode 2026-10-17) |
| 2 | **Stub vs Real Service boundary** — when to enforce real AuthZ URL | Config already enforces for staging/prod; `ENV=local` allows placeholder |
| 3 | **Pack signing key** — provision Ed25519 key for pack certification | `PACK_SIGNING_KEY_REF`/`PACK_SIGNING_KEY_FILE` needed for `sign` command |
| 4 | **Rule resolver ring/region** — enable `/v1/rule-resolutions:resolve` | `RESOLVER_ENABLED=true` + `RESOLVER_RING`/`RESOLVER_REGION` in production |
| 5 | **`/v1/validate-action`** — spec documents it, code does not serve it | Implement the endpoint (§8.2 validate-action API) or remove it from `openapi.yaml` |

---

## 🔧 What Would Unblock Full Closure

| Blocker | Action Required | By Whom |
|---------|-----------------|---------|
| Authorization Service | Deploy `authorization-svc` (Group 2) | Group 2 team |
| Tenant Entity Registry | Deploy `tenant-entity-registry-svc` (port 8081) | Group 1 team (already tested) |
| Kafka brokers | Provision event backbone | Platform infra |
| Pack signing key | Provision Ed25519 signing key | Security team |
| Phantom spec path | Decide on `/v1/validate-action` (decision #5) | Engineering + QA |

---

## 📊 Test Evidence

```bash
$ go mod verify && go build ./... && go vet ./...
# clean

$ TEST_DATABASE_URL=postgres://postgres:secretpassword@localhost:55432/testdb?sslmode=disable \
  go test ./...
ok  zoiko.io/jurisdiction-rules-svc/internal/{authz,config,consumer,domain,envelope,events,handler,problem,resolver,store,telemetry}
# 236 pass / 0 skip / 0 fail

$ go test -tags integration -v ./internal/registryit/
# 65 pass / 0 fail (embedded Postgres 16; harness self-applies migrations 000001–000020, filename-sorted)

$ go test -tags integration ./internal/store/
# 34 pass / 0 fail (real Postgres 16; CI store-integration stage)

# CI-equivalent race verification (Docker: golang:1.26 + real Postgres 16 on gdl-race-net):
$ docker run --rm --network gdl-race-net -e TEST_DATABASE_URL=... -v <svc>:/src -w /src \
  golang:1.26 sh -c "go test -race -count=1 -timeout=900s ./..."
# all packages ok, 0 data races

# Contract:
$ openapi-spec-validator openapi.yaml     # OK (OpenAPI 3.1.0, version 1.1.0)
# route parity: 108/108 code routes documented; 1 spec-only path (/v1/validate-action — finding B)
```

```text
Live E2E (real binary + real Postgres 16, DB juris_live, 2026-10-17):
create / replay / key-mismatch            -> 201 / 201(same id) / 409
GET by id + TER Ref decode               -> 200 / HTTP 200 exact-field decode
rule create / activate / supersede       -> 201 DRAFT / 200 (approver+ref, ETag "2") / 200
gates: no-ref 400 · self-approval 403 · invalid 409 · stale If-Match 412
deactivate -> GET                        -> 200 -> 404 (fail closed)
envelope: write no-Idempotency-Key 400 · bad channel 400 · read write-strict admits (+header)
outbox rows (brokers set)                -> jurisdiction.created, jurisdiction.rule.updated (unpublished)
history                                  -> DRAFT@author / ACTIVE@approver+APR / SUPERSEDED@author
```

---

## 📝 Notes for Next Auditor

1. **All in-repo code is complete** — builds, 236+65+34 tests, race clean, `ForwardTo` wiring on the authz client, transactional outbox + consumer wired
2. **Service 10 of 17; Group 2 service 1/7** — `jurisdiction-rules-svc` on port 8082 (compose service `jurisdiction-svc`; Traefik route `PathPrefix /jurisdiction` + `gateway-auth`; compose line ~871)
3. **Migration set has duplicate numeric prefixes** (`000005/6/7` pairs etc., 25 up files for `000001–000020`) — apply **filename-sorted** (the test harness and live run both do)
4. **Live verification 2026-10-17** — scratch DB `juris_live` on the `gdl-test-pg` Postgres 16 container (host port 55432); statuses recorded above. Envelope headers used: `X-Tenant-Id`, `X-Principal-Id`, `X-Correlation-ID`, `X-Request-Id`, `X-Source-Channel` (valid: web/mobile/api/import/integration/scheduled_job/system), `Idempotency-Key`
5. **Maker-checker activation** — `X-Approval-Reference` header required for ACTIVE; the rule's author cannot activate their own rule (403); approver id + reference land in `rule_status_history`
6. **Envelope posture** — write-strict by default (`ZS_ENVELOPE_ENFORCEMENT` unset for this service): write enforcement on, reads admitted with `X-Envelope-Contract: violated`; promote to `strict` once all production callers send the full envelope
7. **Transactional outbox** — with empty `KAFKA_BROKERS` the publisher is a no-op (no rows); with brokers set, `event_outbox` rows persist (verified `unpublished=t, attempts=0, no error`)
8. **Resolver routes are absent, not 503, when disabled** — with `RESOLVER_ENABLED=false` the resolver router is never mounted (`404`), per startup log
9. **Phantom spec path** — `POST /v1/validate-action` exists only in `openapi.yaml`; consider implementing (it is the §8.2 synchronous validation API) or deleting it
10. **RFC 9457 compliant** — error bodies are `application/problem+json`; `jurisdiction-rules-svc` already compliant (reference point for other services)
11. **Entity consumer** — projects `entity.created` / `entity.jurisdiction.changed` from `tenant-entity-registry-svc` via Kafka; uses `ErrorLogger` + `WatchPartitionChanges`

---

# governance-decision-log-svc (Port 8083) — Service 11 · Group 2 (2/7) — Tier 0 Decision Logging Layer

**Group:** 2 — Governance Platform
**Status:** ✅ **ALL TESTS PASSING** (97 tests · 0 skip · 0 fail; store integration 26/26) · **ZS-QA-001 live audit: certification pending** (service 11 of 17; second of Group 2)
**Last Verified:** 2026-10-17 — unit suite + store integration vs real Postgres 16 (scratch DB, `NOSUPERUSER NOBYPASSRLS` app role); race suite in Docker; live boot round-trip (real binary + real Postgres)

---

## 🔬 ZS-QA-001 Formal Verification (2026-10-17)

**Live acceptance result:** ✅ **mod verify/build/vet/gofmt clean; unit + integration suites green; live append-only round trip green** (against Doc 03 Decision Logging Layer, its own `openapi.yaml`, and the Governance Platform audit 2026-09-28 §2/7).

One defect class found and fixed in this pass; no functional defects found:

| Finding | Root Cause | Fix |
|---------|-----------|-----|
| **A — genuine gofmt drift in three files (line-ending independent)** | Import ordering in `internal/authz/client.go`; 4-space indentation + missing trailing newline in `internal/envelope/contract.go`; misaligned const block in `internal/envelope/reason.go`. All remaining `gofmt -l` entries were CRLF-only artifacts of the Windows host, not real drift. | `gofmt -w` on the three files; now clean line-ending independent. CI does not gate on pre-existing CRLF drift (consistent with the service-10 precedent). |

**Verification evidence:**

- Static: `go mod verify` clean, `go build ./...` clean, `go vet ./...` clean, `gofmt -l` clean (line-ending independent)
- Unit suite: **97 pass / 0 skip / 0 fail** (`go test ./...` with `TEST_DATABASE_URL`)
- Store integration vs real Postgres 16: **26/26 pass** — harness self-applies migrations `000001–000015` and creates a `NOSUPERUSER NOBYPASSRLS` app role, so RLS is genuinely exercised, not bypassed
- Race suite (CI-equivalent, Docker `golang:1.26` + Postgres 16 on a dedicated network): all packages ok, **0 data races**
- Wiring: `internal/authz/client.go` line 242 `svcenvelope.ForwardTo(ctx, req)`; `internal/policyclient/client.go` line 73 `ForwardTo`; transactional outbox (`internal/events/publisher.go` builds the event, `internal/outbox/worker.go` relays to Kafka); `cmd/server/main.go` full wiring order
- Compose: service block `governance-svc` → port 8083, `AUTHZ_SERVICE_URL=http://authorization-svc:8089`, `AUTHZ_PLATFORM_SCOPE_ID=00000000-0000-0000-0000-00000000f001`, `POLICY_SERVICE_URL=http://policy-svc:8085`, `DECISION_LOG_READ_AUTHZ=warn`, `KAFKA_BROKERS=kafka:9094`, topic `zoiko.governance.events`
- Contract: `openapi.yaml` present with a contract test in `internal/handler/openapi_test.go` (handler package green)
- Live round trip (real binary against real Postgres 16):
  1. `POST /v1/decisions` #1 → **201**
  2. Replay same `Idempotency-Key` + body → **200** (original row returned, no duplicate)
  3. Same key + different body → **409 `IDEMPOTENCY_MISMATCH`**
  4. Same `decision_id`, new key, different content → **409 `DECISION_ID_CONFLICT`**
  5. `GET /v1/decisions/{id}` → **200** (original content preserved)
  6. `GET /v1/decisions` (filtered) → **200**, exactly 1 row
  7. DB: decision row + **exactly one** outbox row `governance.decision.recorded` (unpublished — relay deliberately not configured locally)
  8. Envelope negative: write missing `X-Request-Id` / `X-Source-Channel` → **400 `ENVELOPE_INCOMPLETE`** (correct enforcement)
  9. `legal_entity_id` must be a UUID or the `PLATFORM` sentinel; anything else → 400

---

## ✅ Completed Fixes

| Area | Issue | Resolution |
|------|-------|------------|
| **Build** | No build errors | Clean build |
| **Tests** | All pass with `TEST_DATABASE_URL` | 97/97 · store integration 26/26 vs real Postgres 16 |
| **AuthZ Client** | ForwardTo already present | `internal/authz/client.go` L242 — forwards the canonical envelope to authorization-svc |
| **Policy Client** | ForwardTo already present | `internal/policyclient/client.go` L73 |
| **Envelope Contract** | Already rolled out | Present in `internal/envelope/contract.go` (generated by rollout script) |
| **Events/Outbox** | Transactional outbox relay | Event `governance.decision.recorded` written in the same transaction as the decision; relay publishes when `KAFKA_BROKERS` configured |
| **gofmt drift** | 3 files with genuine (line-ending independent) drift | `gofmt -w`; remaining `gofmt -l` entries are CRLF-only host artifacts |

---

## ⚠️ Partial / Pending (Within This Repo)

| Item | Status | Details |
|------|--------|---------|
| **Kafka relay** | ⚠️ Config | No broker locally → outbox row stays queued (verified exactly 1 unpublished row). Relay path covered by outbox package tests; publishing in real infra requires `KAFKA_BROKERS` |
| **Read authz** | ⚠️ Config | `DECISION_LOG_READ_AUTHZ=warn` in compose — read refusals are logged, not enforced. Enforcement decision pending authorization-svc rollout |
| **Replay header** | ℹ️ By design | No `X-Idempotent-Replay` response header on this service; replay is distinguishable by **200 vs 201** (documented, not a defect) |
| **Migrations** | ℹ️ Note | Test harness self-migrates `000001–000015`; policy-svc's own test harness drops/recreates schema `public`, so shared scratch DBs must be re-migrated before re-running live GDL tests |

---

## ❌ External Dependencies (Not In This Repo)

These are **separate services/repos** owned by other teams. This service is wired to call them; they return errors/stubs until counterparties exist.

| Dependency | Owner Repo | Config Field | Why Not Done Here | What's Wired |
|------------|-----------|--------------|-------------------|--------------|
| **Authorization Service** | `authorization-svc` (Group 2) | `AUTHZ_SERVICE_URL` (+ `DECISION_LOG_READ_AUTHZ`) | In scope (Group 2, port 8089) but separate service | `authz.NewClient` with `ForwardTo` ✅ |
| **Policy Service** | `policy-svc` (Group 2, port 8085) | `POLICY_SERVICE_URL` | In scope (Group 2, service 12) but separate service | `policyclient` client with `ForwardTo` ✅ |
| **Kafka/Event Backbone** | Platform infra | `KAFKA_BROKERS` (`kafka:9094`), topic `zoiko.governance.events` | Infrastructure, not code | Producer + outbox worker wired; relay activates when broker present |
| **Schema Registry** | `schema-registry-svc` (Group 2) | — | Later Group 2 service | Event schema versioning pending registry rollout |

---

## 📋 Architecture Decisions Requiring Human Input

| # | Decision | Impact |
|---|----------|--------|
| 1 | **Read-authz enforcement** — promote `DECISION_LOG_READ_AUTHZ` warn → enforce in production | Once authorization-svc is live, read refusals become hard 403s |
| 2 | **Ledger event consumers** — which services consume `zoiko.governance.events` | Downstream audit/replay topology |
| 3 | **Retention/replay posture** — replay manifests + decision retention windows | Compliance sign-off item |

---

## 🔧 What Would Unblock Full Closure

| Blocker | Action Required | By Whom |
|---------|-----------------|---------|
| Authorization Service | Deploy `authorization-svc` (Group 2) | Group 2 team |
| Kafka brokers | Provision event backbone | Platform infra |
| Schema registry | Deploy `schema-registry-svc` (Group 2) | Group 2 team |

---

## 📊 Test Evidence

```bash
# Native (Windows host, Go 1.26.7) vs real Postgres 16 scratch container:
$ TEST_DATABASE_URL=postgres://postgres:secretpassword@localhost:55432/testdb?sslmode=disable \
  go test ./...
# 97 pass / 0 skip / 0 fail  (store integration: 26/26, NOSUPERUSER NOBYPASSRLS app role)

# CI-equivalent race verification (Docker: golang:1.26 + Postgres 16 on gdl-race-net):
$ docker run --rm --network <net> -v <svc>:/src -w /src \
  -e TEST_DATABASE_URL=... golang:1.26 sh -c "go test -race -count=1 -timeout=900s ./..."
# all packages ok, 0 data races
```

```text
Live boot round trip (real binary, 2026-10-17):
POST /v1/decisions                     -> 201
POST (replay, same key+body)           -> 200  (original row, no duplicate)
POST (same key, different body)        -> 409  IDEMPOTENCY_MISMATCH
POST (same decision_id, new key)       -> 409  DECISION_ID_CONFLICT
GET  /v1/decisions/{id}                -> 200  (original preserved)
GET  /v1/decisions?filter              -> 200  (1 row)
DB: 1 decision row + 1 unpublished outbox row (governance.decision.recorded)
Envelope negative (missing headers)    -> 400  ENVELOPE_INCOMPLETE
```

---

## 📝 Notes for Next Auditor

1. **All in-repo code is complete** — builds, 97/97 tests, race clean, `ForwardTo` wiring on both outbound clients
2. **Service 11 of 17; Group 2 service 2/7** — `governance-decision-log-svc` on port 8083 (compose service name `governance-svc`)
3. **Append-only evidence store** — `POST`/`GET /v1/decisions`; idempotent on tenant + `decision_id`; no `X-Idempotent-Replay` header (200 vs 201 distinguishes replay)
4. **Envelope enforcement** — writes require the full canonical contract; `X-Request-Id` / `X-Source-Channel` refusals observed live (400)
5. **Transactional outbox** — the decision and its `governance.decision.recorded` event commit atomically; relay publishes when Kafka is configured
6. **Read-authz warn in compose** — enforcement promotion is a pending config decision
7. **Migrations** `000001–000015`; harness applies them itself with a least-privilege app role, so RLS is genuinely exercised
8. **Live verification** performed 2026-10-17 on a scratch Postgres 16 (Docker) with the real binary — statuses recorded above

---

# policy-svc (Port 8085) — Service 12 · Group 2 (3/7) — Tier 0 Policy Engine

**Group:** 2 — Governance Platform
**Status:** ✅ **ALL TESTS PASSING** (116 tests · 0 skip · 0 fail) · **ZS-QA-001 live audit: certification pending** (service 12 of 17; third of Group 2)
**Last Verified:** 2026-10-17 — unit + integration suite vs real Postgres 16; race suite in Docker; full live E2E: GOV-05 lifecycle, evaluation, and evidence relay into a **real governance-decision-log-svc ledger**

---

## 🔬 ZS-QA-001 Formal Verification (2026-10-17)

**Live acceptance result:** ✅ **mod verify/build/vet/gofmt clean; suite green (116/0/0); full live E2E green** (against Doc 03 §8.1 Policy Engine, V-001 §10.1/§14.1, GCP §16 expected_version semantics, ZS-ARCH-SVC-001 v2.0 §4 envelope, and the Governance Platform audit 2026-09-28 §3/7).

One defect class found and fixed; **no functional defects found** — the live E2E matched the design on every pass:

| Finding | Root Cause | Fix |
|---------|-----------|-----|
| **A — genuine gofmt drift in two generated files (line-ending independent)** | `internal/envelope/contract.go` and `internal/envelope/reason.go` (rollout-generated envelope artifacts); all remaining `gofmt -l` entries were CRLF-only artifacts of the Windows host | `gofmt -w` on both; now clean line-ending independent (consistent with service-10/11 precedent) |
| **B — no functional defects observed** | — | Live E2E: create/replay idempotency, lifecycle state machine, SoD, optimistic concurrency, scheduled activation guard, suspend, atomic supersede, evaluation, replay, evidence relay to the real ledger — all green first pass |

**Verification evidence:**

- Static: `go mod verify` clean, `go build ./...` clean, `go vet ./...` clean, `gofmt -l` clean (line-ending independent)
- Suite: **116 pass / 0 skip / 0 fail** across 11 test-bearing packages (authz, cache, decisionlog, events, evidence, handler, identity, mtlslistener, outbox, resolve, store)
- Store integration vs real Postgres 16 as a `NOSUPERUSER NOBYPASSRLS` app role: green; harness self-migrates `000001–000008` (note: it does `DROP SCHEMA public CASCADE`, so shared scratch DBs are wiped)
- Race suite (CI-equivalent, Docker `golang:1.26` + Postgres 16): all packages ok, **0 data races**
- Wiring (`cmd/server/main.go`): identity verifier (X5 signed-envelope vs mTLS peer before tenant/principal are read) → envelope middleware (evaluate exempt from MaterialWrite, per audit C5; `LegalEntityID=NotRequired` with body reconciliation) → store + resolution cache → Kafka producer + transactional outbox worker → evidence relay (`decisionlog.HTTPClient` + `POLICY_LEDGER_PRINCIPAL_ID`) → activation scheduler → Kafka consumer → authz client (refuses placeholder URL outside `ENV=local`) → mTLS listener → backlog metrics (`policy_evidence_outbox`, `outbox`)
- `internal/decisionlog/client.go` line 140 `svcenvelope.ForwardTo(ctx, req)`; authz client forwards the caller's canonical envelope headers (tenant, request, channel, correlation, causation, per-action `Idempotency-Key`)
- Compose: `policy-svc` block → port 8085, `GOVERNANCE_DECISION_LOG_SERVICE_URL=http://governance-decision-log-svc:8083`; migrations mounted at `deployments/docker-compose.yml` (~line 53)
- **Live E2E (real policy-svc binary + real governance-decision-log-svc binary, real Postgres 16, 2026-10-17):**
  1. `POST /v1/policies` → **201**; replay same `Idempotency-Key`+body → **200**
  2. `POST .../versions` (threshold 10000) → **201**, `row_version=1`
  3. `submit-review` without `expected_version` → **428 `expected_version_required`**
  4. `submit-review` as author → **200** (`row_version=2`)
  5. `approve` as **author** → **403 `SOD_CONFLICT`** (`maker_checker_violation`: the author cannot approve/activate)
  6. `approve` as approver → **200** (`row_version=3`)
  7. `activate` with **stale** `expected_version` → **409 `CONCURRENCY_CONFLICT`**
  8. `activate` v1 (effective today) → **200 ACTIVE**
  9. v2 (effective tomorrow, threshold 20000) → activate → **SCHEDULED**; then activating v3 while v2 is scheduled → **409** "already scheduled in this scope; suspend or retire it first" (GOV-05 guard)
  10. `suspend` v2 (`reason_code` required) → **200 SCHEDULED→SUSPENDED** (full audit trail preserved: submitted_by/approved_by/activated_by + timestamps)
  11. `activate` v3 (effective today) → **200** — v3 ACTIVE, **v1 atomically SUPERSEDED** (not deleted); `GET versions` states: `v2:SUSPENDED v3:ACTIVE v1:SUPERSEDED`
  12. `POST /v1/policies/evaluate` amount 25000 vs effective threshold → **APPROVAL_REQUIRED / WITHIN_THRESHOLD** correct per the version in effect at `as_of` (time-aware resolution incl. SCHEDULED exclusion)
  13. Replay same `decision_id` → **200 `replayed=true`**, same `evidence_id` (single evidence row)
  14. `GET /v1/policy-evaluations/{evidence_id}` → **200**; `GET /v1/policies/resolve` → **200** (resolves the in-effect version only)
  15. `evaluate SOD_RULE` (nothing active) → **404 `no_applicable_policy`** (fail decision pushed to caller, per v1 design)
  16. Envelope negative: `POST /v1/policies` without `Idempotency-Key` → **400 `ENVELOPE_INCOMPLETE`** (`mandatory for material state changes`)
  17. Evidence relay → **real GDL ledger rows** via `GET :8083/v1/decisions/{decision_id}`: `APPROVAL_REQUIRED→ESCALATED`, `WITHIN_THRESHOLD→GRANTED`; `rule_basis=POL-LIVE-102:<policy_version_id>`; caller preserved as `evaluation_context.requested_by_principal_id`; `correlation_id` preserved; `policy_evaluations` joined to `policy_evidence_outbox` shows `delivered=t, attempts=1`
  18. DB audit: `policy_version_transitions` records every state change with actor (`DRAFT…SUPERSEDED` incl. `SCHEDULED→SUSPENDED`); outbox emitted 8 event types: `policy.created`, `policy.updated`, `policy.version.{submitted,approved,activated,scheduled,suspended}`, `policy.rule.retired` (supersede)

---

## ✅ Completed Fixes

| Area | Issue | Resolution |
|------|-------|------------|
| **Build** | No build errors | Clean build |
| **Tests** | All pass with `TEST_DATABASE_URL` | 116/116 · 0 skip; store integration green as least-privilege role |
| **Evidence relay** | Wired + verified live end-to-end | `internal/evidence/relay.go` + `internal/decisionlog/client.go` (L140 `ForwardTo`) → real GDL ledger rows with correct outcome mapping and delivery state |
| **Envelope Contract** | Already rolled out | `internal/envelope/contract.go`; main.go overrides `LegalEntityID=NotRequired` for families/global versions, handlers reconcile body vs header (mismatch → 400) |
| **GOV-05 lifecycle** | Full state machine verified live | submit-review → approve → activate/schedule → suspend → supersede; maker-checker (`SOD_CONFLICT`), optimistic concurrency via `row_version`/ETag (428/409), scheduled-scope conflict guard |
| **Events/Outbox** | Transactional outbox | 8 event types emitted; relay publishes when `KAFKA_BROKERS` configured |
| **gofmt drift** | 2 generated files with genuine drift | `gofmt -w`; remaining `gofmt -l` entries are CRLF-only host artifacts |

---

## ⚠️ Partial / Pending (Within This Repo)

| Item | Status | Details |
|------|--------|---------|
| **OpenAPI artifact** | ⚠️ Artifact | No `openapi.yaml` in this service (jurisdiction-rules-svc and governance-decision-log-svc both have one + contract test). ~28 routes exist; QA-024 contract-diff cannot be run until the spec artifact is added |
| **AuthZ gates** | ⚠️ Config | `POLICY_EVALUATE_AUTHZ`/`POLICY_READ_AUTHZ` default `enforce` in code; local verification ran `warn` (no authorization-svc yet). Enforce mode fails closed → every gated call answers 503 until authorization-svc exists |
| **Kafka relay** | ⚠️ Config | No broker locally → outbox rows queued (verified); relay path covered by outbox unit tests |
| **Consumed events** | ⚠️ Deferred | Consumer wired (`entity.created`, `role.updated`, `authority.delegated`) but producers are stubs elsewhere; documented deferral (context.md §13.5, §21) |
| **Policy-types** | ℹ️ By design | Only `APPROVAL_THRESHOLD` has an evaluator in v1; others → 501/404 (fail loud, documented) |
| **Tie-break rule** | ℹ️ Documented | Two distinct ACTIVE policies of the same type/scope tie-break `effective_from DESC` (documented v1 limitation) |

---

## ❌ External Dependencies (Not In This Repo)

These are **separate services/repos** owned by other teams. This service is wired to call them; they return errors/stubs until counterparties exist.

| Dependency | Owner Repo | Config Field | Why Not Done Here | What's Wired |
|------------|-----------|--------------|-------------------|--------------|
| **Authorization Service** | `authorization-svc` (Group 2) | `AUTHZ_SERVICE_URL`, `POLICY_EVALUATE_AUTHZ`, `POLICY_READ_AUTHZ` | In scope (Group 2, port 8089) but separate service | `authz.NewClient` forwards canonical envelope; refuses placeholder URL outside `ENV=local` ✅ |
| **Governance Decision Log** | `governance-decision-log-svc` (Group 2, port 8083) | `GOVERNANCE_DECISION_LOG_SERVICE_URL`, `POLICY_LEDGER_PRINCIPAL_ID` | Service 11, already verified | Evidence relay delivers every evaluation to the real ledger ✅ (verified live 2026-10-17) |
| **Identity Context** | `identity-context-svc` (Group 1) | `POLICY_IDENTITY_VERIFY`, `IDENTITY_JWKS_URL` | Group 1, already tested | Signed-envelope verification + JWKS cache wired; local runs `warn`, compose `enforce` ✅ |
| **Kafka/Event Backbone** | Platform infra | `KAFKA_BROKERS`, topic `zoiko.policy.events` | Infrastructure, not code | Producer + outbox worker wired; relay activates when broker present |
| **Workflow & Approvals** | `workflow-svc` (Group 2) | — | Later Group 2 service | `approval_reference` links approvals to workflow instances (data-level now) |

---

## 📋 Architecture Decisions Requiring Human Input

| # | Decision | Impact |
|---|----------|--------|
| 1 | **Authz enforcement rollout** — flip `POLICY_EVALUATE_AUTHZ`/`POLICY_READ_AUTHZ` warn → enforce once authorization-svc lives | Gates become hard refusals; requires authorization-svc deployed |
| 2 | **OpenAPI spec artifact** — add `openapi.yaml` + contract test | QA-024 contract diff becomes possible (~28 routes) |
| 3 | **Ledger principal provisioning** — production principal ID + credentials for `POLICY_LEDGER_PRINCIPAL_ID` | Evidence relay identity in the ledger (caller is preserved in `evaluation_context`) |
| 4 | **Tie-break rule** — business sign-off on same-type/same-scope ACTIVE tie-break (`effective_from DESC`) | Determinism today, but arbitrary business-wise |

---

## 🔧 What Would Unblock Full Closure

| Blocker | Action Required | By Whom |
|---------|-----------------|---------|
| Authorization Service | Deploy `authorization-svc` (Group 2) | Group 2 team |
| Kafka brokers | Provision event backbone | Platform infra |
| OpenAPI artifact | Add + review `openapi.yaml` for policy-svc | Engineering + QA |

---

## 📊 Test Evidence

```bash
# Native (Windows host, Go 1.26.7) vs real Postgres 16 scratch container:
$ TEST_DATABASE_URL=... go test ./...
ok  zoiko.io/policy-svc/internal/{authz,cache,decisionlog,events,evidence,handler,identity,mtlslistener,outbox,resolve,store}
# 116 pass / 0 skip / 0 fail  (store integration ~20s, real Postgres, least-privilege role)

# CI-equivalent race verification (Docker: golang:1.26 + Postgres 16):
$ docker run --rm --network <net> -v <svc>:/src -w /src \
  -e TEST_DATABASE_URL=... golang:1.26 sh -c "go test -race -count=1 -timeout=900s ./..."
# all packages ok, 0 data races
```

```text
Live E2E (real policy-svc + real governance-decision-log-svc, 2026-10-17):
POST /v1/policies                        -> 201   (replay -> 200)
POST .../versions                        -> 201   row_version=1
submit-review (no expected_version)      -> 428   expected_version_required
submit-review                            -> 200   row_version=2
approve as author                        -> 403   SOD_CONFLICT (maker_checker_violation)
approve as approver                      -> 200   row_version=3
activate (stale expected_version)        -> 409   CONCURRENCY_CONFLICT
activate v1 (+v2 -> SCHEDULED)           -> 200
activate v3 while v2 scheduled           -> 409   "already scheduled in this scope"
suspend v2                               -> 200   SCHEDULED -> SUSPENDED
activate v3                              -> 200   v3 ACTIVE, v1 SUPERSEDED (atomic)
GET .../versions                         -> 200   v2:SUSPENDED v3:ACTIVE v1:SUPERSEDED
POST /v1/policies/evaluate (above thr)   -> 200   APPROVAL_REQUIRED (version in effect)
POST /v1/policies/evaluate (below thr)   -> 200   WITHIN_THRESHOLD
replay same decision_id                  -> 200   replayed=true, same evidence_id
GET /v1/policy-evaluations/{evidence}    -> 200
evaluate SOD_RULE (none active)          -> 404   no_applicable_policy
POST /v1/policies (no Idempotency-Key)   -> 400   ENVELOPE_INCOMPLETE
GDL ledger GET (real :8083)              -> 200   ESCALATED / GRANTED, rule_basis=POL-LIVE-102:<vid>
DB: evidence delivered=t attempts=1 · transitions fully attributed · 8 outbox event types
```

---

## 📝 Notes for Next Auditor

1. **All in-repo code is complete** — builds, 116/116 tests, race clean, `ForwardTo` wiring on decisionlog client + canonical envelope on authz client
2. **Service 12 of 17; Group 2 service 3/7** — `policy-svc` on port 8085 (compose block ~line 1051)
3. **Evidence obligation closed and proven live** — every evaluation is durable in `policy_evaluations`, relayed to the real ledger (`delivered=t, attempts=1`); caller preserved as `requested_by_principal_id` while the ledger actor is policy-svc's own principal
4. **GOV-05 lifecycle proven live** — 428 (no expected_version) / 403 SOD / 409 stale-version / 409 scheduled-scope guard / suspend / atomic supersede; `policy_version_transitions` attributes every step
5. **Idempotency** — create/version replay = 200 (same key+body); evaluate replay = 200 with `replayed=true` and the same `evidence_id`; no duplicate ledger rows
6. **Envelope** — evaluate is exempt from MaterialWrite (audit C5); all other writes need `Idempotency-Key`; `LegalEntityID=NotRequired` with body reconciliation
7. **Authz in warn mode locally** — enforce is the code default and fails closed; verified posture only in `ENV=local` warn until authorization-svc exists
8. **OpenAPI spec absent** — the only contract artifact gap vs sibling services; add before QA-024 contract diff
9. **Live verification** performed 2026-10-17 on scratch Postgres 16 + real GDL instance — statuses recorded above; scratch DBs used were `policy_live` / `gdl_live`

---

# obligations-svc (Port 8088) — Service 13 · Group 2 (4/7) — Tier 0 Obligation Register

**Group:** 2 — Governance Platform
**Status:** ✅ **ALL TESTS PASSING** (58 tests · 0 skip · 0 fail · race clean · **2 defects found & fixed**: a committed compile break + an applicability write-path outage) · **ZS-QA-001 live audit: GREEN** (service 13 of 17; fourth of Group 2)
**Last Verified:** 2026-10-17 — static checks, unit + store suite vs real Postgres 16, race suite in Docker, and a full live round trip (real binary + real `jurisdiction-rules-svc` binary, scratch DB `obligations_live`)

---

## 🔬 ZS-QA-001 Formal Verification (2026-10-17)

**Live acceptance result:** ✅ **verify/build/vet/gofmt clean after fixes; suite green (58/0/0); full live E2E green** (against the ZS-QA-001 standard, Governance Platform audit 2026-09-28 §4/7, ZS-ARCH-SVC-001 v2.0 §4 envelope, and this service's canonical contract).

Findings in this pass:

| Finding | Root Cause | Fix / Status |
|---------|-----------|--------------|
| **A — HEAD did not compile (committed)** | `internal/store/pg_store.go:114` referenced `domain.ErrIdempotencyMismatch`, which is never defined in this module. The *usage* was introduced by commit `02a125c9` (2026-10-15, "Testing fixes for 16 services … build fixes") without the definition; working tree clean → HEAD does not build and CI (matrix includes obligations-svc) turned **red for this service since that commit** | ✅ Fixed: return the defined `domain.ErrIdempotencyKeyConflict` ("idempotency key already used with a different request"); `DomainErrorCodeMap` already maps its message → **409 `IDEMPOTENCY_KEY_CONFLICT`**, no handler change needed. **Proven live** (same key, different body → 409) |
| **B — every applicability-decision write returned 503** | Migration `000006` added NOT NULL `tenant_id` + FORCE RLS to `applicability_decisions`; the INSERT omitted `tenant_id` → SQLSTATE 23502 on every POST → `store_unavailable`. `List`/`FindCurrent` also ran outside a tenant transaction (non-owner role → silent empty results) | ✅ Fixed: `tenant_id` added to the INSERT and all three methods wrapped in `withTenantTx` (+ `tenant_id` predicate), mirroring the filing-requirement pattern in the same package. **Proven live** (201 create / 200 list / `NOT_APPLICABLE` current / 401 no principal / scoped `[]`; DB row carries tenant_id) |
| **C — genuine gofmt drift in 9 files** | `internal/{authz/client.go, circuitbreaker/circuitbreaker.go, envelope/contract.go, envelope/reason.go, errors/errors.go, handler/applicability_handler.go, handler/handler.go, handler/handler_test.go, telemetry/telemetry.go}` (import-grouping etc.). Caution: a naive `gofmt` comparison flags ~31 files — that is a PowerShell console-encoding artifact (em-dashes mangled by `Out-String`), not drift | ✅ `gofmt -w` on all 9 → **0 genuine drift** (byte-level verification, CRLF-tolerant method); `git diff -w` shows only import-group moves |
| **D — transactional outbox skeleton unwired** | `event_outbox` table (migration `000010`) + store methods `AddEventToOutbox` / `GetPendingOutboxEvents` / `MarkOutboxEvent{Published,Failed}` exist with **no callers and no relay worker** (dead code). Handlers publish directly, fire-and-forget; a publish failure is **logged only** (live with unreachable broker: errors logged, request still 2xx). Live DB audit: `event_outbox=0` after created/updated/overdue events | ⚠️ Open — wire the in-tx write path + relay loop, or delete the dead code (decision #1) |
| **E — minor API-surface drift** | (1) No `Location` header on 201 creates (audit Section A row confirmed live); (2) unknown JSON field → `400 INVALID_JSON` (`json: unknown field "foo"`) instead of the defined `UNKNOWN_FIELD` code; (3) duplicate filing-requirement POST replays with **201** (same id) rather than 200 (create path replays 200) | ⚠️ Open — cosmetic/estate-wide; align or document |
| **F — `progress.md` stale** | Still claims "No built-in scheduler … out of scope" (scheduler exists at `main.go:144` and was live-proven) and "No Authorization Service wiring" (wired + live-proven). Governance audit row 33 stands | ⚠️ Open — refresh `progress.md` |
| **G — no contract artifacts** | No `openapi.yaml`, no Postman collection (audit row 34); gateway route in `all-services.yml` is a prefix strip (`PathPrefix /obligations-svc`) → no per-route parity diff possible | ⚠️ Open — add spec for QA-024 |

**Governance audit §4/7 gaps closed in code and proven live:** object-level read authz (`ActionObligationRead` on get/list — gap #5); applicability reads require a principal (401 — top gap #1); legal-entity **header↔body reconciliation** (400 `legal_entity_mismatch` — gap #2 partially: registry check still absent); **overdue computed in-process** by the 1-minute scheduler (gap #7, proven after a live tick); **real idempotency** (gap #8: same key+body → 200 same id / same key+different body → 409); workflow mapping (migration `000007` + link/unlink routes live); optimistic locking (migration `000008` + `If-Match` live 412/200); filing dedup (migration `000005`, duplicate POST → same id); outbound authz carries the caller's full canonical envelope + `ForwardTo` (X6 closed). Still open from the audit's Section B (WFC-05 deadline clocks / supersession / escalation) — deliberately out of v1 scope.

**Verification evidence:**

- Static: `go mod verify` clean; `go build ./...` / `go vet ./...` clean **after finding-A fix**; `gofmt` clean (byte-level, line-ending independent)
- Suite: **58 pass / 0 skip / 0 fail** (`go test ./...` with `TEST_DATABASE_URL` → real Postgres 16, scratch DB `obligations_test`); test-bearing packages: `authz`, `events`, `handler`, `store` (store harness self-applies **all** migrations filename-sorted, then drops what it created; no embedded-PG harness, no tag-gated suites — unlike jurisdiction-rules-svc)
- Race suite (CI-equivalent, Docker `golang:1.26` + real Postgres 16 on `gdl-race-net`): all packages ok, **0 data races** (re-run clean after the finding-B fix)
- Wiring: envelope middleware (write-strict default; `ServicePolicy` `LegalEntityID: RequiredOnWrite`) → tenant context middleware → per-action authz (fail-closed; client refuses placeholder URL outside `ENV=local`; full envelope + `ForwardTo`) → store (`withTenantTx` + RLS, `app.tenant_id` per tx) → direct Kafka publisher → overdue scheduler (1-min ticker, per-tenant contexts, `system` actor); 12 business routes (`handler.go:232-239` + `applicability_handler.go:42-44`) + `/healthz` `/readyz` `/metrics`
- **Live E2E (real obligations-svc binary + real jurisdiction-rules-svc binary on :8082 with `juris_live`, scratch DB `obligations_live` with all 10 migrations, authz stub returning `GRANTED`, unreachable Kafka, 2026-10-17):**

```text
A1 POST /v1/obligations                    -> 201  (Location header ABSENT — finding E)
A2 replay same Idempotency-Key + body      -> 200  same obligation_id
A3 same Idempotency-Key, DIFFERENT body    -> 409  IDEMPOTENCY_KEY_CONFLICT   [finding-A fix proven]
A4 write without Idempotency-Key           -> 400  envelope_incomplete (INV-08)
A5 write without X-Legal-Entity-Id         -> 400  envelope_incomplete (INV-02)
A6 header vs body legal entity mismatch    -> 400  legal_entity_mismatch
A7 unknown JSON field                      -> 400  invalid_json (finding E, minor)
A8 nonexistent jurisdiction (sync check)   -> 404  jurisdiction_not_found
A9 INACTIVE jurisdiction (real svc)        -> 404  jurisdiction_not_found (fail-closed)
B1 list (+legal_entity_id param)           -> 200  [rows]
B2 get by id                               -> 200
B3 read without X-Tenant-Id                -> 401  tenant_missing + X-Envelope-Contract: violated
B4 malformed id                            -> 404  obligation_not_found
B5 GET ?limit=abc                          -> 400  invalid_limit
C1 filing requirement create               -> 201
C2 duplicate filing (new key)              -> 201  same filing_requirement_id (000005 dedup)
D1/D2 workflow link / re-link              -> 200 / 409 workflow_already_linked
D3/D4 workflow unlink / re-unlink          -> 200 / 409 workflow_not_linked
E1 applicability decision create           -> 201  [finding-B fix proven; was 503]
E2 applicability list                      -> 200  [decision]
E3 current decision (GB/ENT-LIVE-001)      -> 200  NOT_APPLICABLE
E4 current without X-Principal-Id          -> 401  identity_missing (audit #1 closed)
E5 list other scope                        -> 200  [] (tenant/scope isolation)
F1..F4 OB2 state machine                   -> 201 v1; OPEN->IN_PROGRESS 200; same again 200 (idempotent)
F5 If-Match "999" -> CLOSED                -> 412  VERSION_MISMATCH
F6 IN_PROGRESS -> OPEN                     -> 409  INVALID_TRANSITION
F7 If-Match "2" -> CLOSED                  -> 200  version=3
G  OB3 past due + 60s scheduler tick       -> OVERDUE @v2 (computed in-process, not caller-driven)
DB audit: 3 obligations exact states · idempotency_keys=3 · event_outbox=0 (finding D) ·
          filing_requirements=1 (dedup) · applicability_decisions=1 with tenant_id set
```

---

## ✅ Completed Fixes

| Area | Issue | Resolution |
|------|-------|------------|
| **Build** | **HEAD did not compile** — undefined `domain.ErrIdempotencyMismatch` (`pg_store.go:114`, usage committed 2026-10-15 in `02a125c9`) | One-line fix → defined `ErrIdempotencyKeyConflict`; 409 path live-proven |
| **Applicability write path** | Every POST → 503 (NULL `tenant_id` vs `000006` NOT NULL + FORCE RLS); reads non-tenant-scoped | `withTenantTx` + `tenant_id` in INSERT and reads; full live E2E green |
| **Tests** | Suite green with real Postgres | 58/58 · 0 skip; store harness replays all migrations |
| **gofmt drift** | 9 files genuine drift (line-ending independent) | `gofmt -w`; byte-level 0 drift after |
| **Envelope Contract** | Already rolled out | Write-strict default; `LegalEntityID: RequiredOnWrite`; header↔body reconciliation live (400 on mismatch) |
| **Authz gates** | Per-action checks, fail-closed | `ActionObligation{Create,Read,StatusUpdate}`, `ActionFilingRequirement{Add,Read}`, `ActionWorkflow{Link,Unlink}`, `ActionApplicabilityDecision*`; object-level read authz on get/list (audit #5 closed); full envelope + `ForwardTo` on outbound |
| **Overdue scheduler** | Audit gap #7 ("no overdue computation") | In-process 1-min ticker (`main.go:144`, 229-284); per-tenant contexts; live-proven transition + event |
| **Optimistic locking** | Audit gap (no If-Match) | Migration `000008` + `If-Match` parsing; live 412 stale / 200 correct |
| **Idempotency (writes)** | Audit gap #8 | Migrations `000006`/`000009`; live replay 200 same id, mismatch 409 |
| **Filing dedup** | Audit filing-dedup gap | Migration `000005` unique index + conflict-safe insert + lookup; live same-id replay |
| **Workflow mapping** | Audit "no obligation-to-workflow mapping" | Migration `000007` + link/unlink routes; live 200/409 semantics |

---

## ⚠️ Partial / Pending (Within This Repo)

| Item | Status | Details |
|------|--------|---------|
| **Outbox skeleton unwired** | ⚠️ Finding D | `event_outbox` + 4 store methods are dead code; direct publish is fire-and-forget (broker-down = event lost, logged only). Decide: wire (in-tx + relay) or delete |
| **Location header absent** | ⚠️ Finding E | Audit Section A row confirmed live; estate-wide pattern |
| **`UNKNOWN_FIELD` code unused** | ⚠️ Finding E | Unknown JSON fields surface as `invalid_json` with Go's decoder message |
| **Filing replay returns 201** | ⚠️ Finding E | Idempotent (same `filing_requirement_id`) but 201 vs 200 inconsistency with the obligation create path |
| **`progress.md` stale** | ⚠️ Finding F | Claims no scheduler / no authz wiring; both shipped + live-proven |
| **OpenAPI + Postman absent** | ⚠️ Finding G | QA-024 contract diff blocked for all routes (~12) |
| **Section B / WFC-05 scope** | ℹ️ By design | Deadline clocks, supersession, escalation, waiver flows not built in v1 (audit Section B unscored, deliberate) |

---

## ❌ External Dependencies (Not In This Repo)

These are **separate services/repos** owned by other teams. This service is wired to call them; behavior verified live where the counterparty exists.

| Dependency | Owner Repo | Config Field | Why Not Done Here | What's Wired |
|------------|-----------|--------------|-------------------|--------------|
| **Jurisdiction Rules** | `jurisdiction-rules-svc` (Group 2, service 10 ✅) | `JURISDICTION_RULES_URL` | Separate service, already verified | Synchronous fail-closed validation on create; **verified live against the real binary** (A8/A9: unknown + inactive → 404) |
| **Authorization Service** | `authorization-svc` (Group 2, next service) | `AUTHZ_SERVICE_URL` | Next in Group 2, not yet verified | `authz.NewClient` per-action, fail-closed outside `ENV=local`; live runs against a local responder stub (`decision_outcome: GRANTED`) |
| **Workflow & Approvals** | `workflow-svc` (Group 2, later) | — | Later Group 2 service | `workflow_id` mapping persists links/unlinks today (data-level, live) |
| **Kafka/Event Backbone** | Platform infra | `KAFKA_BROKERS`, topic `zoiko.obligations.events` | Infrastructure, not code | Direct publisher wired (not the outbox table — finding D) |

---

## 📋 Architecture Decisions Requiring Human Input

| # | Decision | Impact |
|---|----------|--------|
| 1 | **Outbox: wire or delete** — `event_outbox` + store methods are dead code; current publish path loses events when Kafka is down | Durability guarantee for `zoiko.obligations.events` |
| 2 | **OpenAPI spec artifact** — add `openapi.yaml` (+ Postman) for ~12 routes | QA-024 contract diff becomes possible |
| 3 | **API-standard alignment** — `Location` on 201 + use defined `UNKNOWN_FIELD` code | Estate-wide drift; align with sibling services |
| 4 | **WFC-05 ownership** — is obligations-svc the deadline-clock owner? Section B deliberately unbuilt | Scope for v2 |
| 5 | **`progress.md` refresh** — scheduler + authz sections out of date | Documentation hygiene vs governance audit row 33 |

---

## 🔧 What Would Unblock Full Closure

| Blocker | Action Required | By Whom |
|---------|-----------------|---------|
| Authorization Service | Deploy + verify `authorization-svc` (Group 2, :8089) | Group 2 team |
| Kafka brokers | Provision event backbone (and then exercise outbox if decision #1 = wire) | Platform infra |
| OpenAPI artifact | Add + review `openapi.yaml` for obligations-svc | Engineering + QA |
| WFC-05 decision | Product sign-off on Section B scope | Product + Architecture |

---

## 📊 Test Evidence

```bash
# Native (Windows host, Go 1.26.7) vs real Postgres 16 scratch container:
$ TEST_DATABASE_URL=... go test ./...
ok  zoiko.io/obligations-svc/internal/{authz,events,handler,store}
# 58 pass / 0 skip / 0 fail  (store harness self-applies all migrations, drops what it created)

# CI-equivalent race verification (Docker: golang:1.26 + Postgres 16 on gdl-race-net):
$ docker run --rm --network gdl-race-net -v <svc>:/src -w /src \
  -e TEST_DATABASE_URL=... golang:1.26 sh -c "go test -race -count=1 -timeout=900s ./..."
# all packages ok, 0 data races  (re-run clean after finding-B fix)
```

```text
Live E2E (real obligations-svc + real jurisdiction-rules-svc, 2026-10-17):
creates/replay/mismatch         -> 201 / 200 same id / 409 IDEMPOTENCY_KEY_CONFLICT
envelope negatives              -> 400 (idempotency_key, legal_entity, legal_entity_mismatch)
jurisdiction sync check         -> 404 unknown + 404 inactive (fail-closed, real svc)
reads                           -> 200 get/list; 401 without tenant; 404 malformed; 400 bad limit
filing dedup                    -> 201 first, 201 same filing_requirement_id on duplicate
workflow link/unlink            -> 200 / 409 already-linked / 200 / 409 not-linked
applicability                   -> 201 create (was 503) · 200 list · 200 NOT_APPLICABLE · 401 no principal
state machine OB2               -> 200 transitions, 200 idempotent repeat, 412 stale If-Match,
                                   409 INVALID_TRANSITION, 200 close @version=3
overdue scheduler OB3           -> OVERDUE @version=2 after 60s tick (in-process)
DB: obligations 3 · idempotency_keys=3 · event_outbox=0 · filing_requirements=1 ·
    applicability_decisions=1 (tenant_id set)
```

---

## 📝 Notes for Next Auditor

1. **All in-repo code is complete after two fixes** — builds, 58/58 tests, race clean, per-action authz + `ForwardTo`, scheduler live-proven
2. **Service 13 of 17; Group 2 service 4/7** — `obligations-svc` on port 8088 (compose block ~line 1219)
3. **Committed compile break** — commit `02a125c9` (2026-10-15) referenced an undefined symbol; CI was red for this service until the one-line fix in this pass. If bisecting CI failures, start there
4. **Applicability write path was fully broken before this pass** (503 on every POST — finding B); fixed + live-proven; reads now tenant-scoped
5. **Outbox table is dead code** — events publish directly, failures logged-only (`event_outbox=0` live). Decision #1 pending; audit gap #6 only partially closed
6. **Envelope facts** — `X-Request-Id` is mandatory (in addition to tenant/principal/LE/correlation/source-channel); reads are admitted under write-strict default with `X-Envelope-Contract: violated`, then the handler enforces tenant/principal (401 `tenant_missing` / `identity_missing`)
7. **List requires `legal_entity_id` query param** (authorization scoping — 400 without); applicability reads require `jurisdiction_code` + `entity_ref`
8. **`progress.md` is stale** — claims no scheduler / no authz wiring; both shipped and live-proven (governance audit row 33 stands until refreshed)
9. **Live verification** performed 2026-10-17 on scratch Postgres 16 with DBs `obligations_live` / `obligations_test` (plus `juris_live` for the real jurisdiction binary); authz stub on :18089; Kafka intentionally unreachable (publish errors log-only, requests still 2xx)

---

# authorization-svc (Port 8089) — Service 14 · Group 2 (5/7) — Tier 0 Authorization Engine

**Group:** 2 — Governance Platform
**Status:** ✅ **ALL TESTS PASSING** (749 tests · 1 by-design skip · 0 fail · race clean · **46/46 live probes green**; 12-file gofmt drift fixed · **`openapi.yaml` + route-parity/validation test added** · **1 flaky IT fixed**) · **ZS-QA-001 live audit: GREEN** (service 14 of 17; fifth of Group 2)
**Last Verified:** 2026-10-17 — static checks, full suite vs real Postgres 16 (least-privilege `app_authorization` role), race suite in Docker `golang:1.25`, and a full live E2E (real binary, scratch DB `authorization_live`, all 33 migrations; Kafka intentionally unreachable)

---

## 🔬 ZS-QA-001 Formal Verification (2026-10-17)

**Live acceptance result:** ✅ **mod verify/build/vet clean; suite green (749/0/1 by design); race clean; 46/46 live probes green** (against the ZS-QA-001 standard, Governance Platform audit 2026-09-28 §5/7, and the canonical-envelope contract; the audit's authz gap register was already closed at HEAD — 14 Oct "auth plane closure" — and its key claims were re-proven live in this pass).

Findings in this pass:

| Finding | Root Cause | Fix / Status |
|---------|-----------|--------------|
| **A — genuine gofmt drift in 12 files** | `cmd/server/main.go`, `internal/{cache/store_test.go, domain/types.go, envelope/contract.go, envelope/reason.go, handler/canonical_decisions.go, handler/handler.go, handler/handler_test.go, handler/phase4_canonical_decisions_test.go, handler/session_step_up_test.go, identity/gate_test.go, store/session_duration_test.go}` (import-grouping, blank lines). Byte-level verification (CRLF-tolerant method); `git diff -w` shows formatting-only changes | ✅ `gofmt -w` on all 12 → **0 genuine drift**; build/vet re-verified clean |
| **B — gofmt rewrites doc-comment quotes** | Go 1.19+ doc-comment reformatting converts `''` → `”` (U+201D) inside doc comments — reproduced with a minimal repro on Go 1.25; exactly 1 occurrence (`handler.go`) | ✅ Kept (file is gofmt-clean at the cost of one ASCII quote); documented for future byte-diff reviewers |
| **C — no functional defects observed live** | — | 46/46 probes green: RBAC deny-by-default, ABAC REQUIRE, static SoD, principal-suspension layer 0, step-up (authorize + JIT + break-glass), maker-checker (pending ≠ granted), delegation, platform-scope escalation semantics, tenantless C-01, cross-tenant A-02, idempotent replay, command-contract refusals, revocation, decision-log reads, outbox backlog |
| **D — IT-env sensitivity (harness, not service)** | Integration suites need the canonical per-service role `app_authorization` to exist **before** migrations (guarded `GRANT`s in 000011/000031/000032) + full DML grants; without them 27 ITs fail `42501 permission denied`, and the idempotency IT then hangs ~10 min on the fatal path (pool-close in test teardown) | ✅ Documented: bootstrap role first → migrations → grants; re-run green (106/0/1 across store+idempotency) |
| **E — no OpenAPI spec (QA-024 blocked)** | ~75 served operations had no machine-readable contract — only `ADMIN-API.md` (Markdown tables) and a Postman collection | ✅ **Fixed**: added `openapi.yaml` (OpenAPI 3.1.0 — conventions, all ops with permissions/status codes, and schemas for decision/role/assignment/session types) + `internal/handler/openapi_test.go` (kin-openapi v0.149.0 load + `doc.Validate` + `chi.Walk` route parity, the governance-decision-log-svc pattern). The `available-actions` route is served under two template spellings; OpenAPI forbids the twin ("conflicting paths" — kin enforces it), so the document carries the canonical spelling and the parity test exempts the shorthand (commented) |
| **F — flaky IT `TestFixesIT_RelayWritesABatchAtOnce`** | The relay claims **every** pending outbox row (`WHERE published_at IS NULL`, no tenant filter); the package suites run in parallel against one IT database, so a row concurrently written by another package's test lands in this test's batch and inflates the exact `n == 149` assertion (~2 of 4 full-suite runs failed `published 150, want 149`) | ✅ **Fixed**: assertions key-scoped — count this test's own 150 events in `w.sent` (message key = access_decision_id) instead of the whole batch; 3 consecutive full-suite runs clean after the fix |

**Verification evidence:**

- Static: `go mod verify` clean; `go build ./...` / `go vet ./...` clean; `gofmt` clean (byte-level) after finding-A
- Suite: **749 pass / 1 skip / 0 fail** across 13 packages (`go test ./...` with `TEST_DATABASE_URL` + `AUTHZ_IT_DSN` → real Postgres 16; IT DB `authorization_it` as `app_authorization` [NOBYPASSRLS], legacy store suite on `authorization_test`). The single skip is by design: `TestSecondPassIT_PrincipalStatusTableMissingFailsClosed` requires `AUTHZ_IT_PRINCIPAL_TABLE_GONE=1`. Includes the new `TestOpenAPI_MatchesTheRouter` (spec loads as real OpenAPI + every served route documented, and vice versa)
- Flake fix: `TestFixesIT_RelayWritesABatchAtOnce` was intermittently red under parallel package runs (see finding F); fixed and re-verified with 3 consecutive full-suite runs, 0 fail
- Race suite (CI-equivalent, Docker `golang:1.25` — matches `go.mod` `go 1.25.0` — on `gdl-race-net` with both DSNs): all packages ok, **0 data races** (store 173s)
- Audit cross-check: register §5/7 was already **CLOSED** at HEAD (82 ✅ / 2 ⚠️ / 1 ❌ as of 12 Oct; progress.md L2482 claims full closure 14 Oct). Live re-proofs: C-01 tenantless (403 `tenant_required`), A-02 cross-tenant (403 `tenant_scope_mismatch`), S-3/S-4 step-up (401 MFA on JIT + break-glass), N-01 layer-0 suspension (DENIED `principal_status:SUSPENDED`)
- **Live E2E (real authorization-svc binary on :18099, scratch DB `authorization_live` with all 33 migrations, `ENV=local`, `AUTHZ_IDENTITY_ASSERTION=off` [boot refuses without `AUTHZ_IDENTITY_JWKS_URL` otherwise], Kafka intentionally unreachable, 2026-10-17):**

```text
H1/H2 healthz / readyz                    -> 200 / 200
E1/E2 envelope negatives                  -> 400 envelope_incomplete (missing X-Source-Channel / Idempotency-Key)
D1 admin without iam.role.manage          -> 403 authorization_denied
D2 create role (Idempotency-Key)          -> 201; replay same key -> 200 same role_id; DB 1 row
D3 create bundle                          -> 201
D4 assign iam-carrying role               -> 202 PENDING_APPROVAL (maker-checker; requester lacks approve_privileged)
D4b/D4c independent approver approves     -> 200 APPROVED (approved_by recorded); DB status APPROVED
D5/D6 business-role assignments           -> 201 APPROVED (no approval needed)
D7/D8 tenant SoD rule / ABAC rule create  -> 201 (iam.sod_rule.manage / iam.abac_rule.manage)
A1 authorized grant                       -> 200 GRANTED basis rbac:role=R_APPROVE_LIVE
A2 deny-by-default                        -> 200 DENIED no_grant
A3 tenantless + unregistered entity       -> 403 tenant_required (C-01)
A3b tenantless + registry-resolved entity -> 200 DENIED (owner tenant resolved from registry)
A4 cross-tenant entity                    -> 403 tenant_scope_mismatch (A-02)
A5/A6 ABAC REQUIRE exists                 -> DENIED abac:require_failed=VENDOR_AMOUNT_APPROVED / GRANTED when attribute present
A7 static SoD (payment.approve vs invoice.approve) -> DENIED sod:conflict_with=invoice.approve
A8/A8b principal suspension layer 0       -> DENIED principal_status:SUSPENDED / GRANTED after removal (+5s cache TTL)
A9 step-up on period.reopen (no authn age)-> 200 STEP_UP basis assurance:recent_authn_required
A10 entity-scope validate                 -> 200 in_scope=true basis rbac:role=R_APPROVE_LIVE
G1 delegation without reason_code         -> 400 missing_field (command contract enforced)
G2/G3/G4 delegation create / authorize / evaluate -> 201 / GRANTED delegated:from=padmin-live / has_delegated_access=true
M1/M2 privileged grant pending            -> 202 PENDING_APPROVAL; pending grants nothing (DENIED no_grant)
M3/M4 independent approval then grant     -> 200 APPROVED; GRANTED rbac:role=R_ESC2_LIVE
P1a tenant-wide grant ≠ platform action   -> 403 authorization_denied (SOD_RULE_MANAGE_GLOBAL)
P1b platform-entity grant                 -> 201 global rule (tenant_id NULL)
P2 tenant SoD rule without tenant perm    -> 403
S1 JIT session without step-up            -> 401 step_up_required (MFA)
S2 break-glass without step-up            -> 401 step_up_required ("no_verified_identity", required_assurance=MFA)
L1/L2 access-decisions list / by id       -> 200 (page cap 100; fullRead via iam.policy.read; matched_grants + reason_codes)
R1/R2/R3 revoke no reason / with reason / after revoke -> 400 missing_field / 200 (effective_to set) / DENIED
DB audit: access_decision_log=250 · outbox_events unpublished=234 (Kafka down: relay retries, rows stay pending) ·
          idempotency_keys=26 · authz_config_history=76 · p7-live APPROVED
```

---

## ✅ Completed Fixes

| Area | Issue | Resolution |
|------|-------|------------|
| **gofmt drift** | 12 files genuine drift (line-ending independent) | `gofmt -w` on all 12; byte-level 0 drift; `git diff -w` formatting-only |
| **Doc-comment quote** | gofmt rewrites `''` → `”` (U+201D) in doc comments (Go 1.19+ doc-comment formatting) | Kept — reverting would make the file non-gofmt-clean; documented (finding B) |
| **Live acceptance** | Audit register already closed at HEAD; claims unproven in this pass until now | 46/46 probes green incl. C-01 / A-02 / S-3 / S-4 / N-01 re-proofs (finding C) |
| **OpenAPI artifact** | No `openapi.yaml`; QA-024 contract diff impossible (finding E) | Added `openapi.yaml` (OpenAPI 3.1.0; all served ops) + `internal/handler/openapi_test.go` (load + validate + route parity); `go.mod`/`go.sum` add only kin-openapi v0.149.0 (+16 lines) |
| **Flaky relay IT** | Intermittent `published 150, want 149` when a parallel package's outbox row joined the batch (finding F) | Assertions key-scoped to this test's own events; 3 consecutive full-suite runs clean |

---

## ⚠️ Partial / Pending (Within This Repo)

| Item | Status | Details |
|------|--------|---------|
| **`AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=false` default** | ⚠️ X-1 | C-01 branch (403 `tenant_required`) fires only when the entity registry also resolves no tenant; with the flag `true` the refusal is 400 `missing_tenant_scope` (code path read; callers must migrate first — tenantless calls are warn-logged) |
| **RFC 9457 error shape** | ⚠️ B-02 | Error bodies are the bespoke `{error, error_class, message}` envelope, not `application/problem+json` (estate-wide; external item) |
| **Break-glass action exclusion** | ℹ️ By design | Break-glass refuses payment/posting/filing powers in `requested_actions` (`400 invalid_field`, ZS-IAM-001 §14) — intentional; confirm exclusion list with product |
| **Cache staleness window** | ℹ️ By design | Evaluation reads cached ~5s (`AUTHZ_CACHE_TTL_SECONDS`, default 5; `0` disables). Out-of-band DB edits are invisible until expiry when Kafka invalidation events cannot flow |

---

## ❌ External Dependencies (Not In This Repo)

| Dependency | Owner Repo | Config Field | Why Not Done Here | What's Wired |
|------------|-----------|--------------|-------------------|--------------|
| **Identity Context** | `identity-context-svc` (Group 1 ✅) | `AUTHZ_IDENTITY_JWKS_URL`, `AUTHZ_IDENTITY_ASSERTION`, `AUTHZ_EVALUATION_MTLS` | Group 1 service; JWKS not provisioned in this rig | Assertion verification (off/warn/enforce; **boot refuses without JWKS unless ENV=local + off** — observed live); step-up evidence consumption (401 MFA live); EvaluationMTLS permit-in-local |
| **Kafka/Event Backbone** | Platform infra | `KAFKA_BROKERS` | Infrastructure, not code | Transactional outbox relay + 3 consumer groups (lifecycle, cache invalidator, delegation projector) wired; **backlog behavior proven live** (234 rows unpublished, rows stay pending, decisions still served) |
| **Entity registry** | `tenant-entity-registry-svc` (Group 1 ✅) | `AUTHZ_ENTITY_REGISTRY_URL` | Registry not deployed in this rig | Cross-tenant check + tenantless resolution + entity-status projection (`entity_status_projection`); **registry-resolution path proven live via the projection** (A3b: tenantless → owner tenant resolved → evaluated) |
| **Workflow & Approvals** | `workflow-svc` (Group 2, next) | — | Next Group 2 service | `approval_reference` on assignments/delegations persists today (live) |
| **PEP certs (require mode)** | Platform infra | `AUTHZ_PEP_ALLOWLIST`, `MTLS_ENABLED` | Certificates not issued | Require-mode refusal paths unit-tested; not live-exercised (M-02) |

---

## 📋 Architecture Decisions Requiring Human Input

| # | Decision | Impact |
|---|----------|--------|
| 1 | **`AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE` flip** — from `false` to `true` once tenantless callers are migrated (warn-log gives the worklist) | Tenantless evaluations become hard refusals (X-1) |
| 2 | **RFC 9457 alignment** — replace bespoke error envelope with `application/problem+json` (estate-wide, B-02) | Cross-service error contract consistency |
| 3 | **Break-glass exclusion list** — confirm the payment/posting/filing action exclusions (ZS-IAM-001 §14) with product | Emergency-path capability envelope |

---

## 🔧 What Would Unblock Full Closure

| Blocker | Action Required | By Whom |
|---------|-----------------|---------|
| Kafka brokers | Provision event backbone; exercise relay publish + cache invalidators end-to-end | Platform infra |
| JWKS / issuer | Provision identity JWKS + step-up integration | Group 1 / Identity team |
| PEP certificates | Issue mTLS PEP certs for require-mode evaluation | Security / Platform |

---

## 📊 Test Evidence

```bash
# Native (Windows host, Go 1.25) vs real Postgres 16 (least-privilege role for IT):
$ TEST_DATABASE_URL=... AUTHZ_IT_DSN=... go test ./...
ok  zoiko.io/authorization-svc/internal/{abac,cache,config,domain,envelope,events,handler,idempotency,identity,jurisdiction,mtls,outbox,registry,retention,siem,store,telemetry,workload}
# 749 pass / 1 skip (by design) / 0 fail  ·  store suite ~156s
# IT DB authorization_it runs as role app_authorization (NOBYPASSRLS); role must exist BEFORE migrations
# Includes TestOpenAPI_MatchesTheRouter (openapi.yaml loads as real OpenAPI + router parity);
# TestFixesIT_RelayWritesABatchAtOnce flake fixed (key-scoped assertions; finding F)

# CI-equivalent race verification (Docker: golang:1.25 + Postgres 16 on gdl-race-net):
$ docker run --rm --network gdl-race-net -v <svc>:/src -w /src \
  -e TEST_DATABASE_URL=... -e AUTHZ_IT_DSN=... golang:1.25 \
  sh -c "go test -race -count=1 -timeout=900s ./..."
# all packages ok, 0 data races  (store 173s)
```

```text
Live E2E (real authorization-svc binary on :18099, scratch DB authorization_live, 2026-10-17):
46/46 probes green — full transcript in the Formal Verification block above.
Key DB evidence: access_decision_log=250 rows; outbox_events unpublished=234 with Kafka down
(relay "rows stay pending", requests unaffected); idempotency_keys=26; authz_config_history=76
(every config change attributed); RLS via tenant tx (app.tenant_id) exercised on all writes.
```

---

## 📝 Notes for Next Auditor

1. **In-repo fixes in this pass** — gofmt drift (12 files), `openapi.yaml` + parity test (finding E), one flaky relay IT (finding F); suite now **749/0/1**, race clean, 46/46 live probes; no live functional defects found
2. **Service 14 of 17; Group 2 service 5/7** — `authorization-svc` on port 8089; there is a stray restart-looping `authorization-svc` container in the local Docker instance — leave it alone
3. **Boot refuses without `AUTHZ_IDENTITY_JWKS_URL`** unless `ENV=local` and `AUTHZ_IDENTITY_ASSERTION=off` (fatal "invalid configuration"); the live rig ran `off`, which is also why S1/S2 step-up probes report `no_verified_identity`
4. **Admin gates resolve at TENANT scope** — `checkPermission` passes the tenant id as the scope entity, so tenant-wide (NULL `legal_entity_id`) assignments grant admin powers and entity-scoped assignments do **not**. This detail bit the first live attempt (all admin 403s with an entity-scoped bootstrap grant)
5. **Maker-checker on IAM-carrying roles** — assigning any role whose bundle contains `iam.*`/platform actions returns **202 PENDING_APPROVAL**, grants nothing until an independent holder of `iam.assignment.approve_privileged` approves (approval requires `reason_code`/`purpose`); approver ≠ requester enforced (GOV-04)
6. **Command contract is enforced live** — privileged/destructive commands (delegation create, assignment approve/revoke) without `reason_code` or `purpose` → `400 missing_field`
7. **Platform scope semantics are exact** — a tenant-wide grant does **not** confer a platform action (P1a); it needs an assignment on `AUTHZ_PLATFORM_SCOPE_ENTITY_ID` (P1b); unset config → a platform act is refused with `403 platform_scope_not_configured` (fail-closed, observed in an earlier pass)
8. **PAM specifics** — JIT subject must hold `iam.pam.eligible` (tenant scope), break-glass subject `iam.break_glass.eligible`; both session types require step-up → `401 step_up_required` (MFA) without verified identity; break-glass refuses payment/posting/filing actions by design
9. **5s cache TTL is the staleness bound** — out-of-band DB edits (e.g. a manual `DELETE` of a suspension row) are not observed until expiry because the Kafka cache-invalidator cannot run in this rig; `AUTHZ_CACHE_TTL_SECONDS=0` disables the cache entirely. The A8b probe sleeps 6s to prove the recovery
10. **Live rig** — `ENV=local`, PORT 18099, DB `authorization_live` (all 33 migrations), `KAFKA_BROKERS=127.0.0.1:59092` intentionally unreachable; probe script `%TEMP%\opencode\authz_live.ps1` (idempotent seeds + cleanup); IT DBs `authorization_test` (legacy) and `authorization_it` (canonical `app_authorization` role)

---

| Service | Port | Group | Status | Tests |
|---------|------|-------|--------|-------|
| identity-context-svc | 8080 | 1 | ✅ Complete | 21/21 · 564 race · ZS-QA-001: 30 pass |
| tenant-entity-registry-svc | 8081 | 1 | ✅ Complete | 19/19 · 628 race · ZS-QA-001: 68 pass |
| configuration-feature-flag-svc | 8086 | 1 | ✅ Complete | 14/14 · 266 race · ZS-QA-001: 193 pass |
| secret-vault-integration-svc | 8087 | 1 | ✅ Complete | 12/12 · ZS-QA-001: 83 pass |
| gateway-auth-svc | 8092 | 1 | ✅ Complete | 10/10 · ZS-QA-001: 60 pass |
| search-indexer-svc | 8096 | 1 | ✅ Complete | 16/16 · ZS-QA-001: 111 pass |
| notification-svc | 8133 | 1 | ✅ Complete | 25/25 · ZS-QA-001: 95 met (86%, 92% weighted) · live 69/69 |
| delegated-authority-svc | 8136 | 1 | ✅ Complete | 106 tests · ZS-QA-001: 122 pass (100%) |
| access-control-svc | 8137 | 1 | ✅ Complete | 8 packages · ZS-QA-001: 158 pass (100%) |
| jurisdiction-rules-svc | 8082 | 2 | ✅ Complete | 236+65+34 tests · 0 skip · race clean · live E2E green (TER contract proven) |
| governance-decision-log-svc | 8083 | 2 | ✅ Complete | 97 tests · 0 skip · race clean · live audit green |
| policy-svc | 8085 | 2 | ✅ Complete | 116 tests · 0 skip · race clean · live E2E green |
| obligations-svc | 8088 | 2 | ✅ Complete | 58 tests · 0 skip · race clean · live E2E green (2 fixes: committed compile break + applicability 503) |
| authorization-svc | 8089 | 2 | ✅ Complete | 749 tests · 1 by-design skip · race clean · live E2E 46/46 green (12 gofmt files fixed · openapi.yaml + parity test added · 1 flaky IT fixed) |

**Completed:** 14/17 services (82.4%)
**Group 1:** **DONE** (9/9)
**Group 2:** **IN PROGRESS** (5/7 — jurisdiction-rules-svc, governance-decision-log-svc, policy-svc, obligations-svc, authorization-svc)

---
