# Release Certificate — authorization-svc (IAM / AuthZ)

**Assessed against:** ZS-IAM-001 (Global Authorization, RBAC/ABAC & SoD Standard) §19–§29;
Governance Control Plane GOV-03 / GOV-04 / GOV-12; Doc 03 §8.3; ORG-06 (role half)
**Date:** 2026-11-10 (replaces the 7 Nov certificate)
**Build:** working tree on `shashi-changes` after the 10 Nov gap closure (AZ-1, AZ-4 delegation
half, AZ-5, IAM-8, §26 telemetry, pipeline hardening)

The 7 Nov certificate cited routes (`/v1/bindings`, `/v1/assignments`, `PUT …`), tests
(`TestIAM11_*`, `TestUpdateBinding_*`, `contract_test.go`), a telemetry file, a dashboard and
alert rules that did not exist. Every line below was produced by a command run on 10 Nov 2026
and can be re-run.

---

## Verification performed

| Check | Command | Result |
|---|---|---|
| Build, vet | `go build ./... && go vet ./...` | clean |
| Unit + integration suites, all packages | `go test -count=1 -p 1 -v ./...` in `golang:1.26-alpine`, `AUTHZ_IT_DSN` as `app_authorization` (NOBYPASSRLS) and `TEST_DATABASE_URL` as owner, Postgres 16 | 15 packages ok; **900 tests pass, 0 fail, 1 skip** (`TestSecondPassIT_PrincipalStatusTableMissingFailsClosed`, by design) |
| Mutation checks on the 10 Nov controls | 10 mutants in a throwaway copy | **10 / 10 killed** (fact-hash ×2, currency ×2, If-Match ×2, cursor scope, FX config, record-failure metric, §21 alias) |
| OpenAPI ↔ router, both directions | `TestOpenAPI_MatchesTheRouter` (kin-openapi validated) | pass — 86 paths, 101 operations |
| AsyncAPI ↔ code, both directions | `internal/events/contract_test.go` (declared vs `ProducedEventTypes` / consumers; every literal emitter in Go and SQL scanned) | pass — 35 produced, 19 consumed |
| Alert rules | `promtool check rules deployments/prometheus-rules.yml` | SUCCESS, 79 rules (8 in group `authorization-svc`) |
| Live audit | `SKIP_SUITES=1 bash scripts/audit.sh` against the compose stack | **49 / 49** |
| Persona scenarios | `deployments/scripts/e2e-authorization-scenarios.py` | **78 / 78** |
| Dependency E2E | `deployments/scripts/e2e-authorization-deps.py` | 28 / 32 — the 4 not passed are siem-integration-svc and Jaeger, not started (Docker VM memory) |
| Strict modes | identity `require`, step-up unknown age `step_up`, tenant enforce; evaluation mTLS `require` + allow-list | 7 / 7 and 2 / 2 |
| access-control-svc (caller change) | `go test ./...` | all packages ok |
| Console (caller change) | `tsc --noEmit`, `eslint` on changed files | clean (one pre-existing parse error in another session's uncommitted `lib/api/governance.ts`) |

Stack for the live checks: postgres, kafka, redis, mtls-management-svc, identity-context-svc,
tenant-entity-registry-svc, access-control-svc, authorization-svc (+ jurisdiction-rules-svc and
delegated-authority-svc for the dependency E2E).

---

## Contract surface

86 paths / 101 operations. The `/admin/v1/…` and `/v1/admin/…` session spellings, the two
available-actions spellings and the §21 delegation aliases are distinct routes; unique
capabilities are fewer.

| ZS-IAM-001 §21 | Served |
|---|---|
| `POST /internal/authorization/decisions` | yes (and `POST /v1/authorize`) |
| `GET /v1/me/capabilities` | yes |
| `GET /v1/{resource}/{id}/available-actions` | yes |
| `POST /v1/iam/access-assignments`, `…:revoke` | access-control-svc (provisions into `/v1/admin/role-assignments`) |
| `POST /v1/iam/delegations`, `…/{id}:revoke` | **yes, since 10 Nov** (aliases; colon and slash forms) |
| `POST /admin/v1/privileged-sessions`, `/admin/v1/break-glass-sessions`, `/v1/support/sessions` | yes |
| `GET /v1/iam/access-reviews` | access-control-svc |

---

## What changed on 10 Nov 2026

| Area | Change | Evidence |
|---|---|---|
| API-3 (AZ-1) | If-Match **required** on assignment approve / reject / revoke / suspend / resume and workload-binding deactivate / reactivate (428 / 412). Callers updated: access-control-svc sends `If-Match: *` on its monotonic commands (and on role / bundle retire, which already required it and was failing with 428); the console sends each row's version. | `TestRevokeRoleAssignment_IfMatchRequired`, `TestWorkloadBindings_IfMatchRequired`, `TestAuthzAdmin_CommandsSendIfMatchStar`; scenarios "If-Match: …" |
| API-4 (AZ-1) | Opt-in cursor paging on 15 admin lists (`page_size`, `cursor` → `{records, next_cursor}`); assignments keyset in SQL. | `paging_test.go` (6), `TestAZ1IT_AssignmentKeysetTraversal`; scenarios "cursor …" |
| §21 (AZ-4, delegation half) | `/v1/iam/delegations` aliases. | `iam_delegations_test.go` (3); scenarios "§21 …" |
| §26 | Metrics: decisions by decision / outcome / layer and by action, pipeline latency, record failures, cache hits / misses / invalidations / entries, watermark age, outbox pending / oldest / stuck, sessions by kind / state and expiring, projection events and lag, SIEM drops. Dashboard `authorization-svc.json`; 8 alert rules. | `telemetry/authz_test.go`, `metrics_test.go`, `observer_test.go`, `TestOpsIT_OperationalSnapshotReads`; audit §7–8 |
| A10 fail-closed | Revalidation needs both hashes: approved without current → DENY `CURRENT_FACT_HASH_REQUIRED`; requested without approved → DENY `APPROVED_FACT_HASH_REQUIRED`. | `TestRevalidation_*` (3), mutants M1/M2 |
| A09 basis | No currency against a currency-bearing limit or delegation ceiling → DENY `CURRENCY_REQUIRED` (was read as GBP / the cap's currency). Reference rates configurable (`AUTHZ_FX_REFERENCE_RATES`); converted decisions carry `FX_BASIS:REFERENCE_RATES:<from>/<to>` and `FX_RATE:<r>`. | `TestAuthorityLimit_*` (3), `TestParseFXReferenceRates`, `TestDelegationCeiling`; mutants M3/M4/M8 |
| AZ-5 artefacts | `asyncapi.yaml` (complete), `RUNBOOK.md`, `SPEC_DEVIATIONS.md`, `scripts/audit.sh` rewritten against the code. | contract test; audit 49/49 |
| Housekeeping | Untracked 18 MB `handler_test.exe` removed. access-control-svc migration 000048 rewritten so it can apply (it could not on any database). | `migrate.py up --db access_control` applied; re-run idempotent |

---

## Measured (IAM-8), local stack only

| Measurement | Result |
|---|---|
| `/v1/authorize`, 200 concurrent × 60 s (RUNBOOK §11) | 19,467 decisions, 0 errors, 321.6/s; p50 578 ms, p95 1011 ms, p99 1376 ms |
| `/v1/authorize`, 50 concurrent × 30 s | 10,263 decisions, 0 errors, 341.0/s; p50 126 ms, p95 208 ms, p99 297 ms |
| Revocation through the admin API → next decision DENY | 9–35 ms (6 runs) |
| Projection lag over Kafka (`authorization_projection_lag_seconds`) | delegation 0.23 s mean (2 events), lifecycle 0.42 s mean (5 events) |

One replica, `cpus: 0.5`, `mem_limit: 256m`, a 2 GB Docker VM on a laptop. Throughput is
CPU-bound; latency above 50 concurrent is queueing. **These are not production SLO
measurements** and certify nothing about a production class.

---

## Definition of Done (ZS-IAM-001 §29)

| Gate | State |
|---|---|
| PEP enforcement — one logical policy contract | Met (one pipeline for `/v1/authorize`, the canonical API and available actions) |
| No UI bypass | Met (server-side; available actions computed by the same pipeline) |
| Scope isolation | Met (RLS NOBYPASSRLS suites; cross-tenant entity 403 live) |
| SoD incl. dynamic own-object | Met (unit + live scenarios) |
| Revocation measured end to end | **Met locally**, not against a production objective |
| Evidence reconstructable with policy / assignment versions | Met (`policy_set_version`, `matched_grants`, history tables) |
| Load / performance against an approved SLO | **Local measurement only** |
| Security: session / token tests; secrets absent from logs | Met in code and unit tests; production mTLS / identity `require` not exercised outside the local proof |
| Provisioning (SCIM / federation) | Not this service |
| Privileged access controls and retrospective evidence | Met in code and tests; live run proves refusals only |
| Available-action / denial UX | Console: commands send versions; resume / as-of screens not built (AZ-FE-1) |
| Operational readiness: dashboards, alerts, runbook | **Authored and validated**; not exercised in an incident or game day |

---

## Unresolved, stated

1. Cross-version contract compatibility (GOV-08 schema registry).
2. Alert rules have never fired against production data.
3. Production load and revocation SLOs (only the local numbers above exist).
4. siem-integration-svc and Jaeger edges not run on 10 Nov.
5. JIT / break-glass approval with a real TOTP step-up not re-run on 10 Nov (14 Oct run only).
6. Compose keeps identity `warn`, mTLS `permit`, step-up unknown age `permit`, tenant enforce
   `false` until callers migrate; each strict mode was proven locally.
7. Console screens for suspend / resume, suspension history and as-of (AZ-FE-1) and the UI
   E2E (AZ-FE-2).
8. Quorum, cooling window, related party and resource state remain PEP-asserted
   (SPEC_DEVIATIONS #5).

No mandatory control in this service's contract is unimplemented; the open items are
verification against infrastructure this stack does not run, cross-team migrations, and
console screens.
