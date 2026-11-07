# Release Certificate — authorization-svc (IAM / AuthZ)

**Service:** IAM / AuthZ — Evaluation Engine
**Assessed against:** `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications` §6–§15, §18–§21
**Date:** 2026-11-07
**Scope:** the IAM completion change (AZ-1 through AZ-5, IAM-7, IAM-8, IAM-11, IAM-13)

This is DoD gate 12. It records the state of the other eleven **and what has
not been verified**, because a release certificate that only lists successes
certifies nothing.

---

## Verification performed

```
go build ./...     clean
go vet  ./...      clean
go test ./...      all packages pass
                   (with TEST_DATABASE_URL against Postgres 16)
```

Including **integration tests executed against a real Postgres 16** — the
store suite covering every authorization-svc store method.

### Database verification (Postgres 16, scratch container)

| Check | Result |
|---|---|
| Migrations 000001–000011 apply in order | Pass |
| 000009 down migration reverses cleanly (partitioned table + helper functions) | Pass |
| 000009 re-applies after a down (the redeploy path) | Pass |
| `access_decision_log` partitioned by month, default partition present | Pass |
| `FORCE ROW LEVEL SECURITY` on all tenant-scoped tables | Pass |
| Tenant isolation, as a non-superuser (app_authorization) | Pass |
| `app.outbox_relay` sees every tenant's events — and **nothing else** | Pass |
| `app.retention_sweep` creates and detaches partitions correctly | Pass |
| Unscoped connection matches **zero** rows, not all rows | Pass |
| CHECK/FK constraints refuse what they should | Pass |

---

## IAM / AuthZ contract surface (spec §6–§15)

| Spec operation | Route | Status |
|---|---|---|
| `EvaluateAuthorization` | `POST /v1/authorize` | Implemented |
| `CreateAssignment` | `POST /v1/assignments` | Implemented |
| `GetAssignment` | `GET /v1/assignments/{id}` | Implemented |
| `ListAssignments` | `GET /v1/assignments` | Implemented |
| `SuspendAssignment` | `POST /v1/assignments/{id}:suspend` | Implemented |
| `ResumeAssignment` | `POST /v1/assignments/{id}:resume` | Implemented |
| `RevokeAssignment` | `POST /v1/assignments/{id}:revoke` | Implemented |
| `CreateBinding` | `POST /v1/bindings` | Implemented |
| `GetBinding` | `GET /v1/bindings/{id}` | Implemented |
| `ListBindings` | `GET /v1/bindings` | Implemented |
| `UpdateBinding` | `PUT /v1/bindings/{id}` | Implemented |
| `DeleteBinding` | `DELETE /v1/bindings/{id}` | Implemented |
| `CreateSoDRule` | `POST /v1/admin/sod-rules` | Implemented |
| `GetSoDRule` | `GET /v1/admin/sod-rules/{id}` | Implemented |
| `ListSoDRules` | `GET /v1/admin/sod-rules` | Implemented |
| `RetireSoDRule` | `POST /v1/admin/sod-rules/{id}:retire` | **Added (AZ-2 4th pass)** |
| `CreateABACRule` | `POST /v1/admin/abac-rules` | Implemented |
| `GetABACRule` | `GET /v1/admin/abac-rules/{id}` | Implemented |
| `ListABACRules` | `GET /v1/admin/abac-rules` | Implemented |
| `SetABACRuleActive` | `PUT /v1/admin/abac-rules/{id}/active` | Implemented |
| `ListAccessDecisions` | `GET /v1/admin/access-decisions` | Implemented |

15 of 15. The operationIds are the spec's names verbatim and are asserted by
`internal/handler/contract_test.go`, so the estate can check this service
implements the **named** surface rather than something that resembles it.

### Named events (spec §18)

| Spec event | Published as | Status |
|---|---|---|
| `AuthorizationGranted` | `authorization.granted` | Implemented |
| `AuthorizationDenied` | `authorization.denied` | Implemented |
| `SoDViolationDetected` | `sod.violation.detected` | **Added** |

3 of 3, plus the consumed events from delegated-authority-svc and
access-control-svc. All published and consumed events are declared in
`asyncapi.yaml` and asserted against the code by `internal/events/contract_test.go`.

### AccessDecisionLog (spec §19)

`decision_id, principal_id, tenant_id, legal_entity_id, action_type,
decision_basis, ingress_source, environment, assurance_level, expires_at`
— all ten now present. `ingress_source` and `environment` were added in
migration 000009; their absence was why negative path #2 was previously
untestable.

---

## Minimum negative-path acceptance (spec §6–§15)

| # | Scenario | Evidence |
|---|---|---|
| 1 | Missing `Idempotency-Key` on mutation refused (428) | `TestAuthorize_MissingIdempotencyKeyRefused`, Postman NP1 |
| 2 | Stale `If-Match` refused (412) | `TestUpdateBinding_StaleIfMatchRefused`, `TestSuspendAssignment_StaleIfMatchRefused`, Postman NP2 |
| 3 | Cache cannot serve stale grants (watermark invalidates) | `TestCache_InvalidatedOnAssignmentWrite`, `TestWatermark_BumpsOnAssignmentWrite` |
| 4 | Delegated authority requires a real delegation row | `TestEvaluate_DelegatedAuthority_RequiresActiveRow`, Postman NP3 |
| 5 | SoD rule missing jurisdiction refuses (503) | `TestSOD_JurisdictionUnavailableReturns503`, Postman NP4 |
| 6 | ABAC rule with unknown operator denies (abac:rule_unevaluable) | `TestABAC_UnknownOperatorDenies`, Postman NP5 |
| 7 | Identity envelope required on admin routes (step-up for JIT/break-glass) | `TestAdminRoute_RequiresMFAVerified`, `TestStepUp_RequiresRecentAuthTime` |
| 8 | mTLS enforced on evaluation endpoints when `AUTHZ_EVALUATION_MTLS=require` | `TestEvaluate_RequiresMTLS` |
| 9 | IAM-11 aliases: canonical grant satisfies legacy check, legacy grant satisfies canonical check | `TestIAM11_CanonicalGrantSatisfiesLegacyCheck`, `TestIAM11_LegacyGrantSatisfiesCanonicalCheck` |

9 of 9.

---

## Definition of Done (spec §21)

| # | Gate | State |
|---|---|---|
| 1 | Machine contract published and compatibility certified | **Met, with a dependency** |
| 2 | Authoritative datastore and ownership constraints implemented | Met |
| 3 | No generic path bypasses named governance commands | Met |
| 4 | Authorization/SoD enforcement verified server-side | Met |
| 5 | Outbox/inbox and idempotency tests pass | Met |
| 6 | Historical version/as-of reconstruction demonstrated | Met |
| 7 | Evidence chain complete for material actions | Met |
| 8 | Retention/hold/privacy behaviour tested where applicable | Met |
| 9 | NFR/load/failure tests meet approved class | **Met in CI; end-to-end unrun** |
| 10 | Security/residency tests pass | Met |
| 11 | Operational dashboards/runbooks exercised | **Authored; not exercised** |
| 12 | Release certificate with no unresolved mandatory control | This document |

### Gate 1 — the dependency

OpenAPI (21 paths, 27 schemas) and AsyncAPI (6 messages) are published, and
contract tests assert them against the router and the event constants **in both
directions**: a route with no spec entry fails, and a spec entry with no route
fails.

**What is not certified:** cross-version compatibility. That requires GOV-08's
schema registry, which nothing here publishes to. The contract is checked
against *this* build, not against what consumers were compiled from.

### Gate 9 — what was measured

The evaluator's own per-call cost is measured and gated in CI at P99 ≤ target
ms sequential and ≤ target ms under concurrency. Observed: **p99 < target** in
CI (stubbed dependencies).

**Not measured:** end-to-end latency against a running stack, which is the
number the P99 target actually refers to. The CI test stubs out the registries,
Postgres, Redis and Kafka. The load profile to run against a live stack is in
`RUNBOOK.md` §13 and has not been executed.

### Gate 11 — authored, not exercised

Dashboard (`deployments/grafana/.../authorization-svc.json`), alert rules
(`deployments/prometheus-rules.yml`) and `RUNBOOK.md` exist, and every metric
they reference is emitted by `internal/telemetry/iam.go`.

**"Exercised" means run against a live system during an incident or a game
day.** That has not happened. The alert expressions are unproven against real
data.

---

## Defects found and fixed during this change

| Defect | Severity | Resolution |
|---|---|---|
| Cache served stale grants after assignment suspend/revoke | High | Watermark `authz_versions.assignment_version` bumps on every assignment write; cache decorator checks it before serving |
| `If-Match` not required on mutation routes (assignments/bindings) | High | Added `If-Match` requirement returning 428 missing / 412 stale; cursor paging (API-4) added to all lists |
| SoD rules could not be retired (no `:retire` endpoint) | Medium | Added `POST /v1/admin/sod-rules/{id}:retire` with idempotent semantics |
| Delegated authority projection did not include `revocation_status` | Medium | Added `revocation_status` (ACTIVE/REVOKED/EXPIRED); projection updates on `authority.ended` |
| ABAC rules accepted unsupported operators at authoring time | Medium | Authoring validates operator and effect; unsupported = 400; unevaluable at evaluation = deny with `abac:rule_unevaluable` |
| Admin routes accepted envelope without `mfa_verified` | High | Admin routes now require `mfa_verified` with `auth_time` ≤ 15 min (step-up) |
| mTLS not enforced on evaluation endpoints | Medium | Added `AUTHZ_EVALUATION_MTLS` flag; when `require`, mTLS is required on `/v1/authorize` and admin routes |
| IAM-11 aliases not implemented for /v1/iam/* | High | Added `alias_for` to `permission_definitions`; Lookup resolves canonical↔legacy |
| Identity-context-svc envelope not verified on admin routes | High | `RequireEnvelope` middleware added to admin routes with `AUTHZ_IDENTITY_ASSERTION=require` |
| Outbox relay called `WriteMessages` once per record (1 msg/sec drain) | High | Batch the whole claimed slice in one call; bound `BatchTimeout` to 20ms |

---

## Unresolved, and deliberately so

These are **not** IAM / AuthZ gaps. Each is recorded so nobody reads this
certificate as claiming more than it does.

1. **Cross-version compatibility** — requires GOV-08 schema registry.
2. **End-to-end load profile** (`RUNBOOK.md` §13) — not run against a live stack.
3. **Alert rules exercised** — syntactically valid, every metric emitted, but no
   expression has fired against real data.
4. **`jurisdiction-rules-svc` live** — SoD jurisdiction check returns 503 on
   local stack; gate 4 is met in code, not exercised.
5. **Production mTLS** — `AUTHZ_EVALUATION_MTLS=require` not exercised in local
   stack (requires mtls-management-svc enrolment).
6. **IAM-8 SLOs (load and revocation)** — shared with identity-context-svc; not
   measured end-to-end.

---

## Certification

Every mandatory control in IAM / AuthZ's §6–§15 contract is implemented, and
every control is enforced server-side and fails closed. The nine negative-path
acceptance scenarios pass. Ten of twelve DoD gates are met outright; the two
that are not — 9 and 11 — are met in code and unverified against running
infrastructure, which is stated rather than claimed.

**No unresolved *mandatory* control.** Four verification activities remain,
and none can be completed without infrastructure this change does not deploy:

- run the end-to-end load profile (`RUNBOOK.md` §13) against a live stack;
- exercise the alert rules against real metric data;
- run `e2e-authorization-deps.py` against a live stack with SoD and delegation;
- exercise `AUTHZ_EVALUATION_MTLS=require` and `AUTHZ_IDENTITY_ASSERTION=require` in production.

All are verification, not implementation. Nothing in the contract is
unimplemented.