# Release Certificate — tenant-entity-registry-svc (ORG-02 / ORG-03)

**Service:** ORG-02 — Tenant · ORG-03 — Legal Entity
**Assessed against:** `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications` (ZS-SVC-I-001 v1.0) §4.2, §4.3, §8, §9.2
**Date:** 2026-09-18
**Scope:** the ORG-02/ORG-03 completion change

This records what was verified, **how**, and — in the last two sections — what
was **not**. A certificate that lists only successes certifies nothing.

---

## Re-audit — 29 September 2026

Supersedes the gap closure below. The service was re-audited live on :8081,
rebuilt from current source, with real authorization-svc decisions (temporary
maker and checker grants in the platform scope and a fresh tenant's scope,
revoked afterwards: 0 left) and a real jurisdiction-rules-svc. Every
approval-gated flow was driven end to end, not only the grant-free checks.

```
go build ./... && go vet ./...   clean
go test ./...                    all packages pass (253 → 259 top-level tests; store suite on a
                                 throwaway Postgres 16, the rest in golang:1.25-alpine because
                                 Windows Application Control blocked the handler test binary)
scripts/audit.sh                 68 live checks, 68 passing (was 66; section 16 added)
live end-to-end flow             81 checks, 81 passing (scratchpad e2e script, below)
scripts/backup_restore_drill.sh  22/22, restore 2 s
scripts/contract_gate.sh         2 breaking changes vs b9e244f, both deliberate (16 in total
                                 since 24 Sep — SPEC_DEVIATIONS.md)
govulncheck ./...                0 reachable vulnerabilities
```

**Two defects found by this re-audit, both fixed**

1. **Every route taking an id answered 500 to a malformed one.** `GET
   /v1/entities/not-a-uuid`, an empty approval id (a doubled slash) and the
   like reached Postgres as a failed uuid cast (SQLSTATE 22P02) that nothing
   mapped, so a client mistake counted against the availability SLO and read
   as an outage. Now `VALIDATION_FAILED` (400), mapped once at the store's RLS
   boundary (`internal/store/pg_store.go`, `asInputError`). Regression test
   fails on the old code.
2. **CreateEntity discarded §4.3 source inputs.** The create request had no
   legal-form, registry-authority, registered-office or evidence fields, so a
   client that sent them lost them silently, and an invalid ELF code (`bad!`)
   was accepted with a 201. The legal form could only arrive by a later
   amendment; `lei_verified_at`, documented in openapi.yaml, was dropped the
   same way. Now accepted, validated (the ELF control applies at creation) and
   recorded on profile version 1; the registered address and supporting
   evidence are required, as §4.3 lists them (`internal/registry/legal_form.go`,
   `entityProfileInputs`). Item 27 had been scored ✅ on 28 Sep; it was ⚠️.

**Live end-to-end, 81 checks** — provisioning inputs and refusals (no key,
no jurisdiction, unknown jurisdiction, unknown region, outside platform
scope); creation maker-checker (activate-before-approval, self-approval,
wrong fingerprint, double approval); `Idempotency-Key` replay and onboarding-key
mismatch; `expected_version` missing and stale; suspend/resume and the
suspended-tenant write refusal; ChangeDefaultLocale; ChangeHomeRegion with
decision evidence and SoD; termination proposed and rejected; entity
DRAFT → VERIFIED → ACTIVE with evidence and SoD; ELF, fiscal-calendar,
registered-office and evidence refusals at creation; rename and as-of
before/now; LEI check digits and approval; registry-conflict quarantine and
SoD'd resolution; non-destructive merge with SoD; host binding, resolve and
NP3 on `Host`; cross-tenant read; RESTRICTED tax bundle with and without
purpose; malformed ids; and every event of the run carrying the §7 fields,
delivered to Kafka with no retries.

**Deployment assumption made explicit:** NP3 compares the request's `Host`;
the ingress must preserve it (RUNBOOK §5.2).

**Still open — outside this service** (unchanged): REF-04 fiscal calendar;
§9.2 gate 6 (consumers pinning versions); §9.2 gate 7 (production RPO/RTO);
the Next.js console adopting the 16 contract changes.

---

## Gap closure — 28 September 2026 (second pass)

Superseded by the 29 Sep re-audit above; it superseded the re-certification below. Every open item from it was worked; what
remains open is outside this service and is named at the end.

```
go build ./... && go vet ./...   clean
go test ./...                    11 packages, 253 top-level tests pass, 0 fail
                                 (TEST_DATABASE_URL → throwaway Postgres 16; 1 skip = the
                                  live schema test, run separately against :8081 — passes)
scripts/audit.sh                 66 live checks, 66 passing (was 58)
scripts/backup_restore_drill.sh  22/22, restore 3 s, after migrations 000011–000013
scripts/contract_gate.sh         14 breaking changes vs 24 Sep, all deliberate (SPEC_DEVIATIONS.md)
govulncheck ./...                no reachable vulnerabilities (5 fixed by upgrading
                                 chi 5.3.0, grpc 1.83.1, x/text 0.39.0)
promtool check rules             6 alert rules valid
```

**Score:** ORG-02 + ORG-03, **47 of 48 fully met — 98%**, weighted **99%**.
Whole document (§3, §4.2, §4.3, §8, §9.2): **66 of 69 — 96%**, weighted **98%**.

**Closed in this pass**

| Gap | How | Proof |
|---|---|---|
| §3 typed errors | `error_code` on every error body (the nine §3 codes + 8 extensions) | handler test over 25 sentinels; live |
| §7 event minimum payload | `object_id`, `object_version`, `effective_at`, `recorded_at`, `evidence_ref`; `BuildRecord` refuses a record without id and version | events tests; asyncapi Envelope parity test; live outbox and Kafka |
| §9.2 gate 2 — direct publishes | the direct publisher is removed; all 10 legacy write paths and the registry-conflict events write through the outbox in their own transaction | store tests on Postgres; audit §15 |
| §4.2 home-region maker-checker | `ChangeHomeRegion` (000012), platform authority, independent approval, decision evidence enforced by a constraint | registry, store and live tests |
| §4.2 required inputs + server-resolved context | jurisdiction (real service, retired refused), restricted list, active region → home region at birth, plan entitlement (commercial-account-svc), onboarding evidence (000013) | provisioning tests; live |
| §4.2/§4.3 expected_version | required on every named command | versions tests; live |
| §4.3 legal form | ELF code format + source + local text | controls tests; live |
| §3 purpose limitation | conflict quarantine and approval reads permissioned | controls tests; live |
| §9.2 gate 1 | spec validated by kin-openapi; every route's status checked; live bodies validated against schemas; breaking-change gate | handler tests; `contract_gate.sh` |
| §9.2 gate 7 | RUNBOOK.md, SLO.md, 6 alert rules, outbox delivery gauges, backup/restore drill | promtool; drill |
| §9.2 gate 8 | vulnerability scan clean; this evidence | govulncheck |

**Defects found in this pass, all fixed**

1. The registry-conflict quarantine event was rendered, logged and never sent; the resolution event was declared and never emitted.
2. The jurisdiction validator treated any 200 as valid, so a retired or deactivated jurisdiction was accepted.
3. The stub jurisdiction validator (accepts everything) was selected silently in any environment whose URL was unset or default — including production.
4. `CreateEntity` wrote profile version 1 as a second write whose failure was only logged.
5. The generic entity status route did not bump `record_version` and sent `previous_status: ""`.
6. End-dating a hierarchy or jurisdiction assignment that did not exist, or was already closed, answered 204 and emitted an event.
7. Five legacy write handlers dropped the envelope correlation id, so their events could not be traced (seen in identity-context-svc's log).
8. openapi.yaml was not a valid OpenAPI document: unquoted commas in 36 flow-style descriptions parsed as stray fields; the `command_name` enum lacked a served value.
9. A stale version was reported as "conflict: resource already exists".

**Downstream consumption, live:** identity-context-svc consumed an outbox-delivered
`entity.updated` (object_version 7) and acted on it; consumer lag on
`zoiko.entity.events` 0.

**Still open — outside this service**

- §4.3 fiscal-calendar reference validity: REF-04 Fiscal Calendar does not exist in the estate; the id is format-checked only.
- §9.2 gate 6: accounting/tax/payment/reporting services must pin the versions this service now publishes; one consumer (identity-context-svc) verified.
- §9.2 gate 7: RPO/RTO must be re-measured on the production database (SLO.md).
- Plan entitlement was exercised against a real HTTP server in tests, not against a running commercial-account-svc.
- The Next.js console must send the new required fields (SPEC_DEVIATIONS.md).

---

## Re-certification — 28 September 2026

Supersedes the scores below; the 18 Sep record is kept as history. The full
item-by-item re-audit (48 items against §4.2/§4.3, plus §3, §8 and §9.2) is in
`docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md`, section 2/9,
"Re-audit — 28 September 2026".

```
go build ./... && go vet ./...   clean
go test ./...                    9 packages pass, 214 top-level tests, 0 skipped
                                 (TEST_DATABASE_URL → a throwaway Postgres 16 database;
                                  the store suite wipes its target — never point it at a live one)
scripts/audit.sh                 58 checks, 58 passing (was 37)
```

**Live**, against :8081 with a real authorization-svc (temporary maker and checker
grants, revoked afterwards) and a real jurisdiction-rules-svc: every ORG-02 command,
the creation/termination/verification/rename/LEI/merge/unmerge/conflict approval
chains including every self-approval refusal, NP3–NP6, sensitive-identifier
scoping, and Idempotency-Key replay.

**Score:** ORG-02 + ORG-03, 40 of 48 fully met — **83%**, weighted **91%** (was 75% /
80%). Whole document including §3, §8, §9.2: 53 of 69 — **77%**, weighted **86%**.

**Six defects found live and fixed**, none visible to the previous test run (in-memory
store, tenantless context, database tests skipped):

1. `POST /v1/tenants` answered 500 for every caller with a full envelope — RLS scoped to
   the caller's tenant, not the new one.
2. Provisioning was authorized in the caller's tenant scope instead of the platform scope.
3. An amendment effective before the first version left two open profile versions; as-of
   "now" disagreed with `GetEntity` (NP6).
4. A same-instant amendment answered 500 (`lepv_interval_ordered`).
5. Every direct Kafka publish used the request context and was usually cancelled — events
   silently dropped.
6. NP3 host/tenant check ran on 2 of 46 routes.

**Closed:** Idempotency-Key replay protection (migration 000010, `internal/idempotency/`).
Migration 000010 must be applied to existing databases by hand — the compose stack only
runs migrations when Postgres first initialises.

**Still open:** typed error codes (§3); event payloads missing new object version and
`recorded_at` (§7); older write paths still publish outside the transaction; home-region
maker-checker (doc decision); §9.2 gates 6 and 7.

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

> *28 Sep 2026: `MergeDuplicateCandidate` has since been implemented non-destructively
> (000008, see SPEC_DEVIATIONS.md) and Idempotency-Key deduplication added (000010). The
> two bullets below are the 18 Sep position.*

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
