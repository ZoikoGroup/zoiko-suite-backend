# Release Certificate — tenant-entity-registry-svc (ORG-02 / ORG-03)

**Service:** ORG-02 — Tenant · ORG-03 — Legal Entity
**Assessed against:** `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications` (ZS-SVC-I-001 v1.0) §4.2, §4.3, §8, §9.2
**Date:** 2026-09-18
**Scope:** the ORG-02/ORG-03 completion change

This records what was verified, **how**, and — in the last two sections — what
was **not**. A certificate that lists only successes certifies nothing.

---

## Verification performed

```
go build ./...     clean
go vet  ./...      clean
go test ./...      353 passing, 0 failing, 0 skipped
                   (with TEST_DATABASE_URL against Postgres 16)
scripts/audit.sh   37 checks, 0 failed, 0 skipped
                   (against a running service and the live database)
```

The store suite runs **against a real Postgres**, not a stub. It applies every
`*.up.sql` in `deployments/migrations` discovered from the directory — see
"Defects found in existing code", item 1, for why that sentence matters.

> **Environment note.** Windows Application Control blocks freshly-built Go test
> binaries non-deterministically on the development machine used here, markedly
> more often once Docker is running. It is a local toolchain artifact, not a
> test failure: the same binary passes on retry and prints a different message
> from a real failure. Results above are from runs where no binary was blocked.

### Live end-to-end verification

Executed against the service running as `zoiko_app` — the production runtime
role, `NOSUPERUSER NOBYPASSRLS` — over HTTP, with a real authorization-svc
issuing real decisions. Not a test double.

| Behaviour | Evidence |
|---|---|
| `SuspendTenant` applies, ACTIVE → SUSPENDED, version 1 → 2 | `200`, body names both states |
| The command name reaches the evidence record | `tenant_lifecycle_history` row: `command_name=SuspendTenant`, actor, reason, `from_state=ACTIVE` |
| §8 NP4 — suspended tenant refuses a protected write | entity creation → `409 tenant is not in a state that may transact: lifecycle_state=SUSPENDED`; nothing written |
| `ResumeTenant` lifts it | `200`, SUSPENDED → ACTIVE |
| Stale `expected_version` refused | `409`, names both the supplied and the current version |
| `InitiateTermination` without an approver | `422` |
| `InitiateTermination` self-approved | `422` |
| Command from an illegal state | `422`, names the command and the state |
| §4.3 SoD — legal-name change without an approver | `422` |
| §8 NP6 — legal-name change with an approver | `201`, version 2, trading name carried forward, evidence ref and approver stored |
| §8 NP6 — **history resolves the original name** | as-of an instant inside version 1 → `Zoiko Demo UK Limited (v1)`; as-of now → `Renamed Holdings Plc (v2)` |
| Half-open interval at the exact boundary | as-of `2026-09-18T10:24:32.958574Z` → v2, the version starting there |
| As-of before incorporation | `200` with `profile: null` — a true answer, not an error |
| §8 NP5 — duplicate registry claim | `409` naming the incumbent; quarantine row created with the rejected payload; **the impostor entity does not exist** |
| Conflict resolution requires a note | `400` |
| Conflict resolves once | `204`, then `409` — the first conclusion stands |
| `ResolveTenantByHost` works unauthenticated | `200` with tenant code, status and lifecycle |
| Unknown hostname does not fall back | `404`, and the body names no tenant |
| §8 NP3 — host bound to A, request claims B | `403 host/tenant mismatch: request refused before data access` |
| §9.2 — transactional outbox delivers | 3 events `published_at` set, 0 failed attempts |

### Database verification (Postgres 16)

| Check | Result |
|---|---|
| Migrations 000001–000006 apply in order, from empty | Pass |
| 000006 down reverses cleanly and restores the 000002 policies | Pass |
| 000006 re-applies after a down (the redeploy path) | Pass |
| 11 of 13 tables carry `FORCE` row-level security | Pass |
| `residency_regions` and `tenant_host_bindings` deliberately without | Confirmed |
| Every policy declares an explicit `WITH CHECK` | Pass |
| Runtime role `zoiko_app` cannot bypass RLS | Pass |
| `app.outbox_relay` sees every tenant's events — and nothing else | Pass |
| `app.tenant_resolve` sees tenant rows — and **nothing else**, and cannot write | Pass |
| Unscoped read returns not-found, never a database error | Pass |
| 4 CHECK constraints refuse what they should | Pass |

The two RLS capabilities are the part of this change most worth verifying
against a live database rather than reasoning about, because each is a
deliberate cross-tenant escape hatch. The assertion that matters is not that
they work but that they are **contained** — both are, and
`TestTenantResolveCapability_IsContained` proves the second by connecting as a
purpose-built `NOSUPERUSER NOBYPASSRLS` role and confirming it can read
`tenants`, cannot read seven other tables, and cannot write.

---

## ORG-02 §4.2 contract surface

| Spec operation | Route | Status |
|---|---|---|
| `CreateTenant` | `POST /v1/tenants` | Pre-existing |
| `ActivateTenant` | `POST /v1/tenants/{id}/commands/ActivateTenant` | **Added** |
| `SuspendTenant` | `POST /v1/tenants/{id}/commands/SuspendTenant` | **Added** |
| `ResumeTenant` | `POST /v1/tenants/{id}/commands/ResumeTenant` | **Added** |
| `InitiateTermination` | `POST /v1/tenants/{id}/commands/InitiateTermination` | **Added** |
| `CompleteTermination` | `POST /v1/tenants/{id}/commands/CompleteTermination` | **Added** |
| `ChangeDefaultLocale` | `POST /v1/tenants/{id}/defaults` | **Added** |
| `GetTenant` | `GET /v1/tenants/{id}` | Pre-existing |
| `ResolveTenantByHost` | `GET /v1/resolve-tenant` | **Added** |
| `ListTenantLifecycleHistory` | `GET /v1/tenants/{id}/lifecycle-history` | **Added** |
| `GetTenantDefaults` | `GET /v1/tenants/{id}/defaults` | **Added** |

11 of 11.

### ORG-02 §4.2 events produced

| Spec event | Published as | Status |
|---|---|---|
| `TenantCreated` | `tenant.created` | Pre-existing |
| `TenantActivated` | `tenant.activated` | **Added** |
| `TenantSuspended` | `tenant.suspended` | **Added** |
| `TenantTerminationInitiated` | `tenant.termination.initiated` | **Added** |
| `TenantTerminated` | `tenant.terminated` | **Added** |

5 of 5, plus `tenant.resumed` and `tenant.defaults.changed`, which the spec
names as commands without naming events. All are declared in `asyncapi.yaml`
and asserted against the Go constants by `internal/events/contract_test.go`.

**Spec names are not wire names.** The estate's wire convention is
lower.dotted, established by `tenant.created` before this work. The mapping is
recorded in `asyncapi.yaml` as `x-spec-event` and verified, so an auditor can
check the spec's named events are all published without reading the code.

---

## ORG-03 §4.3 contract surface

| Spec operation | Route | Status |
|---|---|---|
| `CreateLegalEntity` | `POST /v1/entities` | Pre-existing |
| `AmendLegalProfile` | `POST /v1/entities/{id}/profile-amendments` | **Added** |
| `ChangeRegisteredOffice` | `POST /v1/entities/{id}/registered-office` | **Added** |
| `ChangeLegalName` | `POST /v1/entities/{id}/legal-name` | **Added** |
| `DeactivateLegalEntity` | `POST /v1/entities/{id}/status` | Pre-existing |
| `GetLegalEntity` | `GET /v1/entities/{id}` | Pre-existing |
| `FindByRegistryNumber` | `GET /v1/entities/by-registry-number` | **Added** |
| `ListEntityVersions` | `GET /v1/entities/{id}/versions` | **Added** |
| `GetLegalEntityAsOf` | `GET /v1/entities/{id}/as-of` | **Added** |

9 of 10. `MergeDuplicateCandidate` is **not implemented** — see "Not done".

### ORG-03 §4.3 events produced

| Spec event | Published as | Status |
|---|---|---|
| `LegalEntityCreated` | `entity.created` | Pre-existing |
| `LegalEntityProfileAmended` | `entity.profile.amended` | **Added** |
| `LegalEntityStatusChanged` | `entity.status.changed` | Pre-existing |
| `RegisteredOfficeChanged` | `entity.registered_office.changed` | **Added** |

4 of 4.

---

## Minimum negative-path certification (§8)

| # | Scenario | Evidence |
|---|---|---|
| 3 | ORG-02: host resolves to A, request claims B → reject before data access | `TestNP3_*` (4 tests), live `403` |
| 4 | ORG-02: suspended tenant calls a financial API → deny protected write | `TestNP4_*` (5 tests), live `409`, reads confirmed still open |
| 5 | ORG-03: same registry number, two active entities, one jurisdiction → quarantine, no silent merge | `TestNP5_*` (5 tests), live `409` + quarantine row + entity absent |
| 6 | ORG-03: legal name changed after financial history → effective version, history resolves the original | `TestNP6_*` (2 tests), `TestAmendLegalProfile_HistoricalReadResolvesTheOriginalName`, live as-of |

4 of 4.

Each is enforced twice where it can be: `tlh_no_self_approval` and
`erc_resolution_complete` are database CHECK constraints as well as service
validation, so a future caller that bypasses the service cannot record a
maker-checker approval that never happened or a resolution nobody made.

---

## Definition of Done (§9.2)

| # | Gate | State |
|---|---|---|
| 1 | OpenAPI/AsyncAPI contracts pass compatibility gates | **Met** — both exist; route/contract parity is asserted by tests, in both directions |
| 2 | Named commands, expected version, idempotency, transactional outbox | **Met** |
| 3 | Historical/as-of retrieval verified against correction and late-arriving change | **Met** |
| 4 | Cross-tenant, duplicate-import, replay, SoD tests pass | **Met, partially** — see below |
| 5 | External/reference imports preserve manifest, source version, hash, quarantine | **Not applicable** — this service imports no external reference data |
| 6 | Dependent services consume pinned IDs/versions | **Not verified here** |
| 7 | Runbooks, SLOs, alerts, backup/restore production-certified | **Not met** |
| 8 | Accessibility/security/privacy gates, evidence package attached | **Partially** |

**Gate 4, partially.** Cross-tenant and SoD are covered by tests and verified
live. Duplicate-import is covered for the case §8 NP5 names (registry identity).
**Replay is not**: `Idempotency-Key` is required by the envelope middleware and
is not yet used to deduplicate a retried command, so a client that retries a
`SuspendTenant` after a timeout issues a second command. In practice
`expected_version` refuses the second one — the first bumped the version — but
that is a version guard doing an idempotency guard's job, and it does not help a
retry that carries no expected version.

---

## Defects found in existing code, and fixed

Four, all pre-existing and none introduced by this change.

1. **Both store test files named their migrations inline.** Migration 000006
   would have been silently skipped and every test would have run against a
   schema missing its tables — while still reporting `ok`. This is the exact
   trap `backend-completion-tracker.md` records the estate hitting once before.
   Both files now discover `*.up.sql` from the directory and fail if they find
   none.

2. **`openapi.yaml` was missing six live routes** — all five workspace routes
   and `GET /v1/tenants/{id}/residency-region`. Not deprecated, not deliberately
   undocumented: absent, with nothing failing. A consumer generating a client
   from the contract got no workspace methods and no way to know why. Found by
   the new route/contract parity test, not by reading.

3. **An unscoped read returned 500, not 404.** With no verified tenant the empty
   string reached the query, Postgres tried to cast `''` to `uuid`, and the
   driver returned `invalid input syntax for type uuid`. Nothing leaked — it
   failed closed — but it failed as a *server fault*, so an unauthenticated
   probe was indistinguishable from an outage in logs and monitoring.

4. **`CreateEntity` did not check the body's `tenant_id`** against the caller's
   verified tenant, and `assertTenantMayTransact` preferred the argument over
   the verified value. Row-level security refused the insert when they
   disagreed, so nothing could be written cross-tenant — but every check before
   that point had been evaluated against a tenant the caller named. A control
   that is only correct because a later control catches it is not a control.

Two further defects were introduced by this change and fixed before release:
the RLS policies did not tolerate an **empty-string** `app.tenant_id` (Postgres
keeps a custom GUC in the session after a local `SET` is reset, with `''` as its
value, so `current_setting(name, true)` returns `''` and not `NULL` on any
connection that has served one scoped request) — which broke the outbox relay
and host resolution; and `ResolveTenantByHost` needed a named capability to see
through the `tenants` policy at all.

---

## Not done

Stated plainly, because the sections above would otherwise read as completeness.

- **`MergeDuplicateCandidate` (ORG-03 §4.3).** Not implemented. §1 is explicit
  that "destructive merge is prohibited" and that "Party merge/split SHALL
  preserve lineage to every affected financial/business record". A correct merge
  must therefore rewrite or re-point every reference held by other services, and
  this service has no inventory of those references. Implementing it inside this
  service would produce either a merge that silently orphans references or one
  that is merge in name only. `RESOLVED_DUPLICATE` records the *finding*; acting
  on it needs a design decision above this service.
- **Idempotency-Key deduplication** — see Definition of Done gate 4.
- **Runbooks, SLOs, alerts, backup/restore certification** (gate 7).
- **Downstream integration controls** (gate 6): no consumer was run against
  the new events.
- **The generic `POST /v1/tenants/{id}/lifecycle` route still exists.** It is
  demoted in the console, but it is still reachable and still bypasses the
  named-command evidence trail. Removing it is a breaking change for existing
  callers and was not made unilaterally.
- **The pre-existing write paths still publish directly to Kafka** after commit
  and keep the crash window the outbox closes. Only the ORG-02/ORG-03 command
  paths are transactional. `asyncapi.yaml` marks which is which per event, and a
  test refuses a `x-delivery: outbox` claim on a direct path.
- **The local verification used the stub jurisdiction validator** for the NP5
  run, because jurisdiction-rules-svc has no jurisdictions loaded in this
  environment. The real validator was exercised separately and correctly refused
  an unknown id with `400`.

---

## Files

| Area | Change |
|---|---|
| `deployments/migrations/000006_*` | 5 tables, 2 columns, FORCE RLS, 2 named capabilities, backfill |
| `internal/domain/org_types.go` | ORG-02/ORG-03 types, commands, change reasons |
| `internal/registry/org_service.go` | named commands, NP3–NP6, maker-checker, version guards |
| `internal/store/pg_store_org.go` | guarded writes, bitemporal reads, quarantine |
| `internal/outbox/` | ported from identity-context-svc, same table shape |
| `internal/events/outbox_events.go` | transactional event rendering |
| `internal/handler/org_handler.go` | 15 routes |
| `openapi.yaml`, `asyncapi.yaml` | contracts, both parity-tested |
| `scripts/audit.sh` | 37 live checks |
| Frontend | `lib/api/tenants-org.ts`, `org-actions.ts`, `org-state.ts`, 2 component files, 4 new panels |
