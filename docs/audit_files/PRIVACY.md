# PRIVACY DOMAIN — AUDIT RESULTS

This file consolidates **all completed audits** run against the Privacy domain's 5 services in this engagement. Two distinct audit passes were performed, with different methodologies and different scoring dimensions — both are preserved in full, unedited, below.

- **Part 1 — Implementation Completeness Audit** (Frontend / Backend / Integration % against intended architecture, Docker/service health)
- **Part 2 — Documentation Compliance Audit** (formal spec contract vs. actual code, endpoint-by-endpoint, business-rule-by-business-rule)

Services covered (all port numbers verified against `deployments/docker-compose.yml`):

| Service | Port | PRV ID |
|---|---|---|
| privacy-purpose-registry-svc | 8151 | PRV-01 |
| privacy-consent-svc | 8152 | PRV-02 |
| privacy-decision-svc | 8153 | PRV-03 |
| privacy-rights-svc | 8154 | PRV-04 |
| privacy-transfer-svc | 8155 | PRV-05 |

---

# PART 1 — IMPLEMENTATION COMPLETENESS AUDIT

**Methodology:** read-only inspection of backend Go code, frontend admin UI, and Docker Compose wiring for each service, scored across Frontend / Backend / Integration completion percentages against each service's intended architecture.

**Methodology note (as reported by the auditing agent):** `services/README.md`'s top-level table does not list any of the 5 privacy-* services. Intended architecture for these 5 services was reconstructed from the detailed inline comments in `deployments/docker-compose.yml` (the five `# ── privacy-*-svc (Port: 815x)` blocks; the compose file keeps growing, so these anchors are comment headers, not fixed line numbers — at the 2026-10-21 re-audit the blocks ran from the `privacy-purpose-registry-svc (Port: 8151)` header at line 4411 to line 4651) and from each `handler.go`'s own doc comments referencing spec "ZS-SVC-W-001" (PRV-01..PRV-05). None of the 5 services have their own README.md file. All 5 `go vet ./...` clean and all 5 `go test ./...` pass.

## PRV-01 — `privacy-purpose-registry-svc`

**Port:** `8151`

**Classification:** Interlinked — depended on by `privacy-consent-svc`, `privacy-decision-svc`, and `privacy-transfer-svc`; depends on `authorization-svc`.

> **Correction notice.** An earlier version of this section scored the service **100% / Ready for Production** with "no remaining service-owned gaps". A documentation-compliance audit against ZS-SVC-W-001 found that claim unsupportable (it scored 56.3%): the five GET routes required no authentication, a missing `X-Tenant-Id` silently became platform scope, platform records were writable from any tenant context, idempotency was optional and replayed before authorization, every lifecycle event was published twice, several §8.2 gates were only partly enforced, `effective_from` was caller-controlled and backdatable, and the Part 2 gate/error-code table contradicted the code. This section replaces it and describes the service **as remediated and verified in this pass**. The remediation is service-owned only; the integration, frontend and provisioning items it depends on are listed under *Dependency-blocked items* and are **not** claimed as done.

### Status after the remediation pass

| Area | Assessment | Basis |
|---|---|---|
| **Backend (service-owned spec compliance)** | **No % asserted** (earlier ≈ 90.6% re-score superseded by the domain audit: 87.8%, 36 of 41); service-owned gaps fixed in the latest pass (incl. suspension time-slicing); the remaining partial items are external reference verification | Re-scored against the same 34-row audit table that produced 56.3%. The 3 partial items are named under *Remaining gaps*. |
| **Frontend** | **Not updated in this pass — NOT compliant with the new contract** | The workbench does not send `privacy_owner`, `necessity_rationale`, `special_condition_refs` or `processor_refs`, so a version created from the UI cannot pass gates 1/3/4/6; and its activation call sends a free `effective_from`, which is now refused unless it equals "now" (±60 s). |
| **Integration** | **BLOCKED — DEPENDENCY (unverified end to end)** | `privacy-consent-svc`, `privacy-decision-svc` and `privacy-transfer-svc` forward the calling request's `X-Tenant-Id` **and** its `X-Principal-Id` (each client sets the principal header whenever its caller has one — `internal/purposeregistry/client.go` in each service), so principal propagation no longer causes a `401`. Still open and external: that the forwarded principal holds `PRIVACY_PURPOSE_READ` / `PRIVACY_ACTIVITY_READ` in `authorization-svc` (provisioning is not done here), a defined service-to-service identity for calls with no end-user principal, and a live-stack run. This service takes `X-Principal-Id` as supplied and asks `authorization-svc` for the grant; it performs no signature verification of the header. |
| **Overall** | **Not 100%** | — |
| **Production readiness** | **NOT READY** | Blocked on the dependent clients, the frontend, provisioning of the new action codes in `authorization-svc`, and a live-stack run (none was performed — see *Verification*). |

### What the service does (current behaviour)

**Pre-flight, identical on every route.** Authentication and authorization strictly precede any idempotent replay.

| Step | Check | Refusal |
|---|---|---|
| 1 | `X-Principal-Id` present | `401 AUTHENTICATION_FAILED` |
| 2 | `X-Tenant-Id` present — a missing tenant is **never** platform scope | `401 TENANT_SCOPE_REQUIRED` |
| 3 | `authorization-svc` grant for the route's action, in the verified scope | `403 AUTHORIZATION_DENIED`; `503 AUTHORIZATION_SERVICE_UNAVAILABLE` (fail closed) |
| 4 | *mutating only:* `X-Correlation-ID` present (echoed in the response) | `400 CORRELATION_ID_REQUIRED` |
| 5 | *mutating only:* `Idempotency-Key` present, then replay lookup | `400 IDEMPOTENCY_KEY_REQUIRED`; same key + different method/path/body → `409 IDEMPOTENCY_MISMATCH`; lookup failure → `503` |

**Routes and the authorization action each demands**

| Route(s) | Action |
|---|---|
| `POST /privacy/purposes`, `POST /privacy/purposes/{id}/versions` | `PRIVACY_PURPOSE_CREATE` |
| `POST /privacy/purposes/{id}/versions/{vid}/publish` | `PRIVACY_PURPOSE_PUBLISH` |
| `GET /privacy/purposes`, `GET /privacy/purposes/{id}` | `PRIVACY_PURPOSE_READ` |
| `POST /privacy/processing-activities`, `…/{id}/versions` | `PRIVACY_ACTIVITY_CREATE` |
| `POST …/{id}[/versions/{vid}]/validate \| submit \| approve \| reject \| activate \| suspend \| resume \| retire` (canonical latest-version and explicit-version forms) | `PRIVACY_ACTIVITY_VALIDATE \| _SUBMIT \| _APPROVE \| _REJECT \| _ACTIVATE \| _SUSPEND \| _RESUME \| _RETIRE` |
| `GET …/{id}?as_of=&known_as_of=`, `GET …/{id}/versions/{vid}` | `PRIVACY_ACTIVITY_READ` |
| `GET /privacy/ropa?role=&jurisdiction=` | `PRIVACY_ROPA_READ` |

The three `*_READ` actions are new; §8.1 limits callers to privacy admins, approved domain owners, the change pipeline and compliance services but names no permission, so the action names are the service's. Canonical latest-version routes resolve the version **after** authentication and authorization, so an anonymous caller cannot probe activity ids.

**Scope model.** Every request carries `X-Tenant-Id`. A tenant id addresses that tenant's records. The **platform plane** (Zoiko as independent controller, §23.1/§24) is addressed only by sending the explicit platform-scope id `00000000-0000-0000-0000-00000000f001`, which also becomes the authorization scope. A tenant may mutate only its own records, the platform plane only platform records (`403 PLATFORM_SCOPE_REQUIRED` otherwise; a tenant's record is invisible from the platform plane, `404`). Underneath, PostgreSQL enforces the same rule (migration 000006): writes to a `tenant_id IS NULL` row require the transaction-local capability `app.platform_scope = 'true'`, which the store sets only for the sentinel; a context with no tenant can write nothing; a platform context cannot write a tenant row; there is no DELETE policy and a trigger rejects DELETE even for a superuser. Platform rows remain **readable** by every authorized tenant (a shared baseline), tenant rows only by their owner.

**Activation gates (§8.2).** Evaluated at `/validate` (DRAFT → VALIDATED) and **re-evaluated at `/activate`** against the live registries; any finding keeps/returns the version to a non-active state and activation answers `422` with every finding and its §32 code. §32 fixes each code's *meaning* but does not map gates to codes — the mapping below is the service's.

| Gate | Requirement | Finding code | Field |
|---|---|---|---|
| 1 | Accountable **business owner** and **privacy owner** | `PRV-003` | `owner`, `privacy_owner` |
| 2 | Role resolved (`CONTROLLER`/`PROCESSOR`/`JOINT_CONTROLLER`) | `PRV-003` | `privacy_role` |
| 3 | ≥1 linked purpose; each registered, published, in force, with a **necessity/minimization rationale** | `PRV-001` | `purpose_ids` |
| 4 | Each linked purpose carries a lawful-basis reference | `PRV-005` | `purpose_ids` |
| 4 | Sensitive/special-category data ⇒ ≥1 linked purpose carries a special-condition reference | `PRV-010` | `special_condition_refs` |
| 5 | Jurisdictions enumerated and in the registry (ISO 3166-1 alpha-2 — all 249 codes — or `EU`/`EEA`/`GLOBAL`; "ZZ", "UK", free text rejected) | `PRV-004` | `jurisdictions` |
| 5 | Subject classes enumerated; data categories enumerated and in the taxonomy | `PRV-010` | `subject_classes`, `data_categories` |
| 5 | Sources and recipients enumerated | `PRV-019` | `sources`, `recipients` |
| 6 | Retention rule references | `PRV-014` | `retention_rule_refs` |
| 6 | More than one jurisdiction ⇒ transfer references | `PRV-015` | `transfer_refs` |
| 6 | `PROCESSOR` role ⇒ processor-instruction reference | `PRV-017` | `processor_refs` |
| 7 | Notice/consent dependency explicitly `REQUIRED`/`NOT_REQUIRED`/`CONDITIONAL` | `PRV-006` | `notice_consent_dependency` |
| 8 | DPIA/TIA `RESOLVED`, `NOT_REQUIRED` or marked `REVIEW_REQUIRED` | `PRV-016` | `dpia_tia_status` |

Blank list entries are findings. Registry codes are stored canonicalised (trimmed, upper-case). **Correction to the previous Part 2 table:** that table listed gate codes as `PRV-010`/`PRV-003`/`PRV-004`/`PRV-006` and said gate 8 requires "`RESOLVED` or `EXEMPT`"; neither matched the code. Gate 8 accepts `RESOLVED`, `NOT_REQUIRED` or `REVIEW_REQUIRED` (§8.2: "resolved by policy or marked review-required"); there is no `EXEMPT` value.

**Lifecycle and evidence.** `DRAFT → VALIDATED → SUBMITTED → APPROVED → ACTIVE ⇄ SUSPENDED → RETIRED`, with `SUBMITTED → REJECTED` (terminal; remediation is a successor draft naming the rejected version as parent). Maker-checker: the creator cannot approve, reject or publish their own version (`403 SOD_DENIED`). Every transition writes, **in the same transaction**, an append-only `activity_lifecycle_audit` row (action, from/to status, acting principal — mandatory, correlation id, reason, superseding version) **and** the outbox event. Approver, rejecter and activator are also recorded on the version row.

**Activation and the one-ACTIVE invariant.** Activation locks the activity row, checks the target is `APPROVED` *before* touching anything, retires every previously `ACTIVE` **or `SUSPENDED`** version of the activity at the same instant (so windows abut — no gap, no overlap), each with its own `privacy.processing_activity.retired` event and `SUPERSEDE` audit row, then activates the target. A partial unique index (`uq_activity_versions_one_active`) makes a second ACTIVE row impossible even for a superuser; resuming a SUSPENDED version into a collision is a `409`, not an outage.

**Effective time (item 6).** Activation takes effect at the **server's** instant, recorded as `effective_from`. A caller-supplied `effective_from` is accepted only as an assertion of "now" within ±60 s; backdating and post-dating are `422 EFFECTIVE_TIME_INVALID`. (§2.1 leaves timers to WFC: scheduled future activation would also leave the prior version retired while the new one is not yet in force.) Publishing a purpose clamps its `effective_from` to the publish instant if it would be earlier; a deliberate future date is preserved.

**Bitemporality (§9.1, §20.3).** `GET …/{id}?as_of=T&known_as_of=K` returns the version that governed at `T` (`effective_from ≤ T < effective_to`) **and** that the platform had approved by `K` (`approved_at`). With neither parameter it answers "what governs now". No governing version → `404 PRV-002`. An `as_of`/`known_as_of` that is present but not RFC 3339 is `400 INVALID_AS_OF` — it is never silently replaced by "now". `GET /privacy/purposes/{id}` resolves the same way (`published_at` is its knowledge time; unknown purpose → `404 PRV-001`). RoPA lists only `ACTIVE` versions whose `effective_from` has been reached.

**Events — transactional outbox only.** The handler holds no publisher. Each state change enqueues its event inside its own transaction; a rolled-back change leaves no event, a committed one leaves exactly one. The relay (the only component that talks to Kafka) retries with backoff, records each failure durably, requires full broker acknowledgement (`RequireAll`), and verifies that its own bookkeeping updated exactly one row. Events: `privacy.purpose.published`, `privacy.processing_activity.{submitted,approved,rejected,activated,suspended,resumed,retired}`; `validate` is audited but is not a §19 event. The activity-event payload embeds the full version (tenant, activity/version, effective window, role and purpose scope) plus `previous_status`, **`policy_refs`** (lawful-basis and special-condition refs of the linked published purposes + the version's retention, transfer and processor refs) and, on a superseded version's retired event, `superseded_by_version_id`. Platform events carry an empty tenant.

**Error contract (§32).** Every error is `{"error", "error_code"}` (activation refusals add `findings`). §32 codes keep their §32 meaning: `PRV-001` unknown/unpublished purpose, `PRV-002` no governing activity version, `PRV-020` re-publishing a published (immutable) purpose, and the finding codes above. The rest are local symbolic codes (`AUTHENTICATION_FAILED`, `AUTHORIZATION_DENIED`, `TENANT_SCOPE_REQUIRED`, `TENANT_SCOPE_MISMATCH`, `PLATFORM_SCOPE_REQUIRED`, `IDEMPOTENCY_KEY_REQUIRED`, `IDEMPOTENCY_MISMATCH`, `CORRELATION_ID_REQUIRED`, `INVALID_TRANSITION`, `SOD_DENIED`, `INVALID_AS_OF`, `EFFECTIVE_TIME_INVALID`, `VALIDATION_FAILED`, `NOT_FOUND`, `METHOD_NOT_ALLOWED`, `STORE_UNAVAILABLE`, `INTERNAL_ERROR`) because §32 names no code for them. Transport-level errors use the same envelope: unknown route `404 NOT_FOUND`, unsupported method `405 METHOD_NOT_ALLOWED` (with `Allow`), a recovered handler panic `500 INTERNAL_ERROR`, a failed `/readyz` `503 STORE_UNAVAILABLE`; only a request Go's `net/http` rejects before routing keeps net/http's plain-text reply — the same convention `privacy-consent-svc` uses.

**Migrations (corrected list).** `000001_initial_schema`; `000002_immutability` (published-purpose and non-DRAFT-activity content triggers); `000003_add_rls`; `000004_activation_gates_and_idempotency` (`notice_consent_dependency`, `dpia_tia_status`, `purpose_registry_idempotency_keys`); `000005_fix_gaps` (one-ACTIVE unique index, per-operation RLS policies, approver/rejecter/activator columns, `event_outbox`); `000006_scope_audit_and_activation_integrity` (`privacy_owner`, `processor_refs`, `effective_to`, `necessity_rationale`, `special_condition_refs`, `activity_lifecycle_audit`, DELETE-rejecting triggers, platform-capability write policies, tightened outbox/idempotency policies). The earlier README named `000002_immutability_triggers` and omitted 000005.

### Fixed gaps (this pass), by audit item

| # | Gap | Fix | Proven by |
|---|---|---|---|
| 1 | Five GET routes unauthenticated | Principal + tenant + fail-closed grant on all five (and `GET /privacy/purposes`) | `TestReads_*`; mutation "authorization disabled" fails the read-authorization and replay-after-revocation tests |
| 2 | Missing tenant became platform scope | `401 TENANT_SCOPE_REQUIRED` everywhere; platform only via the explicit sentinel; authz, request context and RLS all use that one verified scope | `TestMissingTenant_*`, `TestAuthorizationUsesTheVerifiedScope`, store RLS tests |
| 3 | Platform records writable from tenant context | `PLATFORM_SCOPE_REQUIRED` in the handler **and** capability-gated RLS write policies | `TestTenantCannotMutatePlatformRecords`, `TestRLS_PlatformRecordsAreWritable…` (real Postgres, restricted role) |
| 4 | Idempotency optional; replay before authz | Mandatory key; replay only after authn+authz; scoped to tenant/plane + principal; fail closed on lookup error | `TestIdempotency_*`, `TestE2E_Replay…` |
| 5 | Correlation id | Required on every mutating route | `TestMutations_MissingCorrelationID_Rejected` |
| 6 | Gates partial; `effective_from` free | All §8.2 gates as above, re-run at activation; ISO registry; server-instant activation | `TestActivationGates_*`, `TestActivation_*` |
| 7 | One ACTIVE version | Atomic supersede incl. SUSPENDED siblings, retired event + audit per superseded version, unique index | `TestActivation_*`, concurrent-activation test, direct-SQL index test |
| 8 | Lifecycle/WFC | Behaviour unchanged and now documented as dependency-blocked (below); maker-checker preserved | `TestSoD_*` |
| 9 | Duplicate/droppable events | Outbox only; exactly one event per committed change; survives broker outage | `TestEvents_ExactlyOne…`, `TestOutbox_*` |
| 10 | Audit evidence | `activity_lifecycle_audit`, append-only, mandatory actor | `TestAudit_*` |
| 11 | Untyped HTTP errors | Stable `error_code` on every error | `TestErrorContract_*` |
| 12 | `as_of` fell back to now | Strict RFC 3339; valid-time window + knowledge time | `TestAsOf_*`, `TestRetire_*` |
| 13 | Documentation | This section, Part 2, README | — |
| 14 | Tests | See *Verification* | — |

Additional defects found and fixed while doing the above: the authorization client substituted one hard-coded tenant UUID for every platform-scope check (it now forwards the caller's verified tenant); the `event_outbox` policy allowed any tenant-less session to read and write every tenant's pending events; the relay did not check that its bookkeeping UPDATE affected a row; the Kafka writer used fire-and-forget acknowledgement; startup logged a warning and continued with a nil database pool.

### Latest pass (domain-audit remediation)

| # | Gap | Result | Proven by |
|---|---|---|---|
| 1/2 | Tenant isolation / read routes | **No change required** — strict tenant/platform RLS and principal + tenant + grant on every GET were already in place and still pass. | store RLS tests, handler pre-flight tests |
| 3 | Idempotency saved after commit, save errors swallowed, concurrent duplicates | **Fixed.** `store.RunIdempotent`: claim, change, audit row, outbox event and replay response are one transaction (every store call joins it); a failed save fails the operation and persists nothing; concurrent identical requests execute once; refused operations consume no key; scope (tenant-or-platform, principal, key) and the method + path + body hash are unchanged. | store `TestRunIdempotent_*` (real PostgreSQL: rollback with injected failure, 8-way race), handler `TestIdempotency_SaveFailure_*`; mutation checks (not joining the transaction; proceeding on a lookup failure) fail the intended tests |
| 5 | Reference validation (lawful basis, retention, transfer, processor) | **Not changed — CROSS-SERVICE / EXTERNAL DEPENDENCY.** §8.2 asks for PDC-approved basis references and for retention/transfer/processor dependencies to be *identified*; existence/approval belongs to PDC, DRC and PRV-05 and would require callable contracts and invented rules here. Presence and well-formedness remain enforced. | — |
| 8 | Documentation | `owner` required at draft creation documented; counts and idempotency text corrected. | — |

### Latest pass (documentation-compliance remediation, 2026-10-21)

Scope: the four service-owned gaps of the latest documentation-compliance audit (42/49). `authorization-svc` and every other service were left untouched.

| Audit row | Gap | Result | Proven by |
|---|---|---|---|
| 4 | `GET /privacy/purposes` returned every PUBLISHED version row (superseded, future-dated, several per purpose) | **Fixed.** Returns one row per purpose: the published version in force now — the version `GET /privacy/purposes/{id}` resolves with no parameters (same filter and ordering). ZS-SVC-W-001 defines no list route; "currently published" is read through the spec's effective-dated supersession doctrine (§8.1) and that existing resolve rule, not through a new rule. | store `TestListPurposes_*` (real PostgreSQL, restricted role: current version, superseded, future-dated, multiple versions, draft, tenant isolation/platform baseline, a version entering force when its time arrives); handler `TestListPurposes_*`; mutations (list back to all published rows; future-dating filter removed) fail the intended tests |
| 35 | Router 404/405, recovered panic 500 and `/readyz` 503 bypassed the `{error, error_code}` envelope | **Fixed.** `404 NOT_FOUND`, `405 METHOD_NOT_ALLOWED` (+`Allow`), `500 INTERNAL_ERROR` (panic value never sent to the caller), `503 STORE_UNAVAILABLE`; `handler.NewRouter` is the single router `cmd/server` and the tests share. The README is the contract that says "every error"; ZS-SVC-W-001 §32 fixes code meanings only and names no transport behaviour, so the new local code `METHOD_NOT_ALLOWED` is the service's own. Requests Go's `net/http` rejects before routing keep its plain-text reply (documented boundary). | handler `TestErrorContract_*` (unknown route, unsupported method, recovered panic, readiness failure, healthz unchanged, pre-flight unchanged on registered routes); mutations (handlers not installed, plain recoverer, plain readiness body) fail the intended tests |
| 50 | This file said the dependent clients send `X-Tenant-Id` only | **Fixed (documentation).** Verified in the three `internal/purposeregistry/client.go` files: tenant and, when present, principal are forwarded. Wording now separates propagation, `authorization-svc` provisioning and verification (none claimed). | code read |
| 51 | Stale `pg_store.go` lines, compose range, test counts | **Fixed (documentation).** Citations now point at the current code or are anchored to comment headers; test counts are 95. | `go test` |

Not changed, still external or unresolved: row 11 (WFC submit hand-off), row 30 (gate 4 / PDC), row 32 (gate 6 / DRC / PRV-05) — **CROSS-SERVICE / EXTERNAL BLOCKERS**; rows 15, 34, 37 and 52 remain **needs clarification**.

### Verification

Run from `services/privacy-purpose-registry-svc` with `REQUIRE_DB_TESTS=1` (an unreachable database is a failure, not a skip) against a throwaway PostgreSQL 16 where every test gets a fresh database with all migrations applied and connects as a `NOSUPERUSER NOBYPASSRLS` role (the harness aborts if the role could bypass RLS):

- `go test ./...` — **pass**: 95 top-level tests (domain 4, handler 62 — 4 of them end-to-end HTTP→handler→PostgreSQL —, store 29), plus subtests. No test skipped when the database is reachable (unreachable ⇒ skip).
- `go vet ./...` — clean. `go build ./...` — clean.
- **Mutation checks** (each reverted afterwards): forcing the platform capability on, disabling the retire step, enqueuing a duplicate event, dropping `effective_to`, restoring the old outbox policy, and disabling authorization each make the intended tests fail.
- The old handler test files (`handler_test.go`, `gaps_test.go`) were replaced: they referenced the removed event publisher and the previous contract.
- **Not performed:** no Docker image rebuild, no run against the live compose stack, and no check of `authorization-svc` platform-scope grants. The authorization client now sends the sentinel as the caller's tenant for platform checks; that interaction is unverified against the live service.

### Remaining gaps

1. *(fixed)* Suspension is now correctly time-sliced for `as_of` historical resolution. The returned `version_status` is the status at the requested instants, derived from the append-only `SUSPEND`/`RESUME` rows of `activity_lifecycle_audit` (`recorded_at ≤ as_of` and `≤ known_as_of`, half-open intervals); current reads (no `as_of`) are unchanged. A suspended instant resolves with `version_status: SUSPENDED` (the specification names no other status code). A version suspended before the audit table existed has no row to slice and stays SUSPENDED. See the README, "Bitemporality".
2. **Gate 4/6 reference *existence* is not verified.** Lawful-basis, special-condition, retention, transfer and processor references are checked for presence and well-formedness only; that they resolve to real PDC/DRC/PRV-05 records cannot be checked until those catalogues are callable (dependency-blocked).
3. **Idempotency** — fixed in the latest pass (below): the record is committed in the same transaction as the change. Replay bodies are still read back from a `JSONB` column, so they are JSON-equivalent to the original, not byte-identical.
4. Existing ACTIVE versions created before this pass have no `privacy_owner`; they stay ACTIVE and are only re-checked if a successor is validated.

### Dependency-blocked items

- **BLOCKED — DEPENDENCY: `privacy-consent-svc`, `privacy-decision-svc`, `privacy-transfer-svc`.** Their purpose-registry clients forward `X-Tenant-Id` and, when the caller has one, `X-Principal-Id` (verified in each `internal/purposeregistry/client.go`). Principal and tenant propagation are therefore in place; what is not is `authorization-svc` provisioning of `PRIVACY_PURPOSE_READ` / `PRIVACY_ACTIVITY_READ` for those principals (until then the lookups get `403`), a defined service-to-service identity for calls with no end-user principal (the lookup gets `401` without a principal), and any end-to-end run against live services. Service-to-service identity is a platform decision; no bypass was added here.
- **BLOCKED — DEPENDENCY: `authorization-svc` provisioning.** The new actions `PRIVACY_PURPOSE_READ`, `PRIVACY_ACTIVITY_READ`, `PRIVACY_ROPA_READ` must be granted (and the platform-scope grant for platform-plane principals). Until then the reads fail closed with `403`.
- **BLOCKED — DEPENDENCY: frontend.** Needs the four new content fields, the `X-Tenant-Id` platform-scope option, and removal of its free `effective_from` on activation.
- **WFC (§2.1/§9.1).** *Submit* is a real, audited transition but does not hand the version to a WFC approval workflow, and approval is a local, separately-authorized, maker-checker-gated call. WFC owns timers, approval orchestration and escalation; none is integrated. This is documented, not hidden.
- **PDC jurisdiction rules.** Jurisdiction-specific legal evaluation is external; gates are structural.

### Needs clarification (not scored)

- **Gate 3 rationale** is now a schema field and an enforced gate, but §7/§8.2 do not say whether the rationale belongs to the purpose version (as implemented) or the activity.
- **`processor_refs`** is this service's representation of §8.2 gate 6's "processor dependencies"; §7's field list does not name it.
- **Gate-to-§32 code mapping** is the service's (table above); the spec fixes meanings, not the mapping.
- **Read-route action names** (`PRIVACY_*_READ`) are the service's; the spec names no permission.

### Final assessment — PRV-01

| Area | Status |
|---|---|
| Backend | Service-owned gaps fixed (no % asserted) — see *Remaining gaps* |
| Frontend | **Not updated — not compliant with the new contract** |
| Integration | **BLOCKED — DEPENDENCY** |
| Overall | **Not 100%** |
| Tenant isolation | Enforced in handler and in RLS; verified on real PostgreSQL |
| Production readiness | **NOT READY** |



---

## Service: privacy-consent-svc
**Port:** 8152
**Classification:** Interlinked (depends on privacy-purpose-registry-svc + authorization-svc; depended on by privacy-decision-svc)

> **Correction notice.** Earlier versions of this section said **100% / Production Ready / 19 tests / no remaining gaps**, and Part 2 separately gave different figures. Neither was supportable: a documentation-compliance audit against ZS-SVC-W-001 first scored the service **64.7%** (22 of 34), a first remediation pass re-scored it ≈ 93.9% on the same table, and a later domain audit scored it **75.0% (21 of 28 comparable items)**. That later audit found: seven mutating routes validated the body (400) before authenticating the caller; some routes ran authorization before the correlation-id check and the tenant check before the principal check; the presentation-receipt response had no §18 `receipt_id`; idempotency was scoped to (tenant, key) only, hashed the body alone, and was saved **after** the operation committed; the purpose-registry client forwarded no principal; and the documented route count and test counts were stale. This section replaces the earlier claims and describes the service **as remediated and verified in this pass**. **No compliance percentage is asserted here.**

### Status

| Area | Assessment | Basis |
|---|---|---|
| **Backend (service-owned)** | **Service-owned gaps fixed; remaining items are external or need clarification** | See the two tables below. |
| **Frontend** | **Not assessed in this pass** | No frontend file was read or changed. |
| **Integration** | **BLOCKED — DEPENDENCY** | The registry-side grant for the forwarded principal (`authorization-svc`) and a service-to-service identity are not provided; not verified against live services. |
| **Production readiness** | **NOT READY** | Code is covered by tests; the integrated system is not safe to roll out until the dependencies below are resolved. |

### This pass (domain-audit remediation)

| # | Gap | Result | Proven by |
|---|---|---|---|
| 1 | Tenant isolation | **No change required.** Strict RLS (migration `000006`) and `401 TENANT_SCOPE_REQUIRED` were already in place; the existing real-PostgreSQL tests still pass. | store `TestGap2_*` |
| 2 | Read-route authentication | **Already compliant** (`GET /privacy/consents`, `/privacy/preferences`: principal, tenant, grant); the parameter validation that preceded authentication is fixed (see 6). `GetNotice` stays tenant-scoped and principal-less by design. | handler `TestReadsValidateParametersOnlyAfterAuthentication` |
| 3 | Idempotency | **Fixed.** Scope (tenant, principal, key) — migration `000007`; hash covers method + path + body; the claim, evidence rows, outbox event and replay response are **one transaction** (`store.RunIdempotent`, every store call joins it); a failed save persists nothing and the retry re-executes; concurrent identical requests execute once; lookup failure → 503 (unchanged). The `500 IDEMPOTENCY_RECORD_NOT_SAVED` outcome is removed. | store `TestRunIdempotent_*` (real PostgreSQL), handler `TestIdempotency_*`, `TestGap7_*`; mutation check: not joining the transaction fails the rollback test |
| 5 | Registry reference | **Service-owned part fixed:** the caller's principal and tenant are forwarded to PRV-01. **BLOCKED — EXTERNAL DEPENDENCY:** the registry-side grant in `authorization-svc` and service-to-service identity. | client tests (stub registry) |
| 6 | API / error contract | **Fixed.** Order is principal → tenant → correlation → authorization → idempotency → validation → resolve on every mutating route; `receipt_id` added (additively) to the presentation receipt. | handler `TestValidationNeverPrecedesAuthentication`, `TestPresentationReceipt_CarriesTheSection18ReceiptID` |
| 8 | Documentation | Rewritten (this section, PRV-02, README); route count (17), test counts and the idempotency/external-dependency text corrected. | — |

### Latest pass (documentation-compliance remediation, 2026-10-21)

| Audit row | Gap | Result | Proven by |
|---|---|---|---|
| 45 | A refused notice lifecycle transition (approve/publish/withdraw) answered `409` with `error_code: IDEMPOTENCY_MISMATCH` (status-derived fallback) | **Fixed.** `409 INVALID_TRANSITION` on all three routes (explicit and canonical); the fallback no longer derives an idempotency code from a status; a real key reuse is still `409 IDEMPOTENCY_MISMATCH`, written with its own code. HTTP statuses are unchanged. The README now has one error table and states which responses are outside the `{error, error_code}` envelope (router 404/405, panic recovery, `/readyz`, net/http-level rejections). | handler `TestInvalidNoticeTransitions_AnswerInvalidTransition`, `TestIdempotencyMismatch_StillAnswersIdempotencyMismatch`, `TestInvalidNoticeTransition_DoesNotConsumeTheKey`, `TestValidNoticeTransitions_StillSucceed`, `TestInvalidTransition_AuthorizationStillPrecedesLookupAndState`, `TestInvalidTransition_PreflightUnchanged`; mutation (old behaviour restored) fails the intended tests |
| 48 | README carried two contradictory idempotency sections (one still described `500 IDEMPOTENCY_RECORD_NOT_SAVED` / `committed: true`) | **Fixed (documentation).** Stale section removed; one description remains: mandatory key, authorization before replay, (tenant, principal, key) scope, method + path + body binding, one-transaction atomicity, fail-closed lookup, a failed or refused operation stores nothing so the key can be retried; `IDEMPOTENCY_RECORD_NOT_SAVED` no longer exists. | existing store `TestRunIdempotent_*`, handler `TestIdempotency_*` |
| — | At most one PUBLISHED version per notice is not enforced | **Investigated, not changed — NEEDS CLARIFICATION / DESIGN DECISION.** ZS-SVC-W-001 does not state the invariant. A throwaway probe on real PostgreSQL reproduced two PUBLISHED versions in 24 of 25 concurrent-publish rounds (probe deleted; no schema or locking change made). | probe run, documented in the README "Remaining limitations" |

### Earlier remediation (12 audited gaps, kept for history)

Re-grant after INVALIDATED/EXPIRED (replay only while the receipt is the live latest outcome); RLS `NULL` exception removed (migration `000006`); workforce consent not implemented (PDC-owned, PRV-N10); offline-import provenance enforced; receipt integrity v2; §18 response schema (additive); `privacy.consent.changed` payload with derived old/new state through the transactional outbox; latest-version routes do not reveal existence; permission-provisioning claim removed; decision-svc lookup left as a documented dependency. Details are in the service README.

### Verification

Run from `services/privacy-consent-svc`:
- `go test ./...` with `REQUIRE_DB_TESTS=1` against a throwaway PostgreSQL 16 (fresh database per test, migrations 000001–000008, app role `NOSUPERUSER NOBYPASSRLS`): **147 top-level tests pass, none skipped** (domain 7, handler 94, outbox 10, purposeregistry 3, store 33).
- **Without** a database: 109 pass and **38 are skipped** (RLS, outbox, notice-binding, store-level evidence and idempotency tests) — a run without a database does **not** prove tenant isolation or atomicity.
- `go vet ./...` and `go build ./...`: clean.
- **Mutation checks** (each reverted): not joining the idempotency transaction, proceeding on a lookup failure, and dropping the presentation `receipt_id` each make the intended tests fail.
- **Not performed:** Docker rebuild, live compose stack, live `authorization-svc` / purpose registry, frontend.

### External dependencies

1. **Purpose registry (PRV-01):** the call now forwards the caller's principal and tenant, but needs a `PRIVACY_PURPOSE_READ` grant for that principal in `authorization-svc` (not provisioned by this repository); until then it is `401` → `503 UPSTREAM_UNAVAILABLE` (fail closed).
2. **Decision → consent:** this service requires a principal and `PRIVACY_CONSENT_READ`; what the decision service presents is that service's concern.
3. **Service-to-service identity:** `docs/architecture/05-security.md` §12.4 lists the goal but defines no mechanism, so none was invented.
4. **Grants are not provisioned by this repository** — `authorization-svc` holds them as runtime data.
5. **Workforce consent (§11.1)** is policy/PDC-owned; **WFC (§2.1)**, **representative-authority verification (§11.1)** and **PDC** are dependency-blocked.

### Remaining limitations

- *(fixed)* `PreferenceAssertion` now has the §10.1 separate effective time (`effective_at`, migration `000008`): optional RFC 3339 on write, UTC/µs, default = recording instant, returned on write and read, and used to resolve the assertion in force now. The specification names no field name, default or resolution rule, so those are the service's smallest reading (see the README, "Preference effective time").
- Legacy rows with a NULL `tenant_id` are invisible to the application role after `000006` (not deleted); the migration warns with counts and an operator must assign owners.
- Receipts written before the v2 digest keep the `v1-legacy` digest; idempotency rows written before `000007` are inert.
- `GetNotice` treats an unparseable `as_of` as "now" (needs clarification).

### Final assessment — PRV-02

| Area | Status |
|---|---|
| Backend (service-owned) | Fixed (see above); no % asserted |
| Frontend | **Not assessed** |
| Integration | **BLOCKED — DEPENDENCY** |
| Tenant isolation | Strict in code and RLS; verified on real PostgreSQL |
| Production readiness | **NOT READY** |

---

## Service: privacy-decision-svc
**Port:** 8153
**Classification:** Interlinked (calls privacy-purpose-registry-svc, privacy-consent-svc, retention-registry-svc, privacy-transfer-svc)

> **Correction notice.** Earlier versions of this section claimed **100% (34 of 34) / Production Ready**. That claim was withdrawn: a read-only audit against ZS-SVC-W-001 scored the service **66.7% (28/42 comparable items)**, and found, among other things, that migration `000005` deleted evidence the append-only trigger forbids deleting, the idempotency record was saved after commit, the input fingerprint omitted decision-affecting inputs, registered scope and model training were not enforced, the transfer gate failed open on unknown results, free-text secondary purposes were stored as purpose ids, requested version ids were ignored, `expires_at` was never populated, the correlation id was not echoed, and several documented behaviours (correlation echo, model-training BLOCK, "all dimensions evaluated", `IDEMPOTENCY_SAVE_FAILED`, `urn:`/`sha256:` formats) did not match the code. This section replaces those claims. **No compliance percentage is asserted here**; see `services/privacy-decision-svc/README.md` for behaviour.

### Status

| Area | Assessment | Basis |
|---|---|---|
| **Backend (service-owned)** | **Service-owned gaps fixed; five items partly BLOCKED on other services** | 16 audited gaps: 11 fixed in code+tests, 4 fixed for the service-owned part with the remainder `BLOCKED — EXTERNAL DEPENDENCY` (gaps 5, 6, 8, 9), 1 fixed with a legacy-data sub-item `BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING` (gap 1). |
| **Frontend** | **Not assessed / not service-owned** | Workbench must send `X-Correlation-ID`. |
| **Integration** | **BLOCKED — DEPENDENCY** | No service-to-service identity (callers forward the end-user principal); purpose-registry/PDC capabilities missing (below). |
| **Production readiness** | **NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY** | Service identity, PDC/registry capabilities, legacy tenant mapping. |

### What changed in the remediation (16 gaps)

| # | Gap | Result |
|---|---|---|
| 1 | Migration `000005` deleted NULL-tenant evidence | **Fixed.** No row is deleted or rewritten; `CHECK (tenant_id IS NOT NULL) NOT VALID` rejects new rows; `NOT NULL` is set only when no legacy NULL row exists; strict RLS + FORCE RLS kept. Legacy NULL rows (if any) are preserved and invisible: **BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING.** |
| 2 | Save failure after commit | **Fixed.** Idempotency claim + decision + replay body in one transaction; failure persists nothing (`503 STORE_UNAVAILABLE`); retry yields one decision; concurrent same key → one decision. |
| 3 | Incomplete fingerprint | **Fixed.** `v2:` fingerprint covers consent/hold/transfer checks and requested version ids; reflection guard + per-field tests. |
| 4 | Registered scope not enforced | **Fixed.** Categories ⊆ registered, subject class ∈ registered, sensitivity derived from registered ∪ requested ∪ flags (caller cannot downgrade). Taxonomy is a local copy of the registry's (no endpoint): drift = needs clarification. |
| 5 | Model training | **Service-owned part fixed:** `TRAIN_MODEL` never auto-permitted (`REVIEW_REQUIRED` `PRV-009`); real tests added (the old "Safeguard 4" test only exercised purpose binding and was renamed). **BLOCKED — EXTERNAL DEPENDENCY:** verifying a dedicated, human-approved training purpose (no registry attribute, no PDC). |
| 6 | Legal hold / retention | **Service-owned part fixed:** `retention_rule_refs` mapping removed; `record_class` caller-supplied; `DELETE`/`ANONYMIZE` without hold info, registry error, empty reply, inconsistent state → `INDETERMINATE`; held → `BLOCK` `PRV-014` (`RETAIN` records the hold and continues). **BLOCKED — EXTERNAL DEPENDENCY:** retention-period conformance (registry returns hold blocks only). |
| 7 | Transfer gate fails open | **Fixed.** Explicit switch; empty/unknown/nil/error/no client → `INDETERMINATE`. |
| 8 | Secondary purpose free text | **Service-owned part fixed:** free text → `BLOCK` `PRV-001`, never stored; registered id must be published and bound; never auto-permitted. **BLOCKED — EXTERNAL DEPENDENCY:** compatibility evaluation (PDC). |
| 9 | Requested versions ignored | **Activity fixed** (exact registry version route; never substituted). **Purpose:** registry has no exact historical purpose-version lookup — a non-current `purpose_version_id` fails closed (`INDETERMINATE`) — **BLOCKED — EXTERNAL DEPENDENCY.** As-of consent/hold/transfer also blocked. |
| 10 | Unknown values | **Fixed.** Age band, classification, source, flags, recipient type, DPIA status, notice/consent dependency, consent/transfer results, jurisdiction fail closed; regression table. |
| 11 | `expires_at` never set | **Fixed** without inventing a TTL: earliest of consent-receipt and transfer-decision expiry; persisted, returned, replay-preserved. Default TTL = needs clarification. |
| 12 | Correlation id not echoed | **Fixed** (POST first response and replay; GET when supplied). |
| 13 | Error/status mismatch | **Fixed.** `IDEMPOTENCY_RECORD_NOT_SAVED`/`IDEMPOTENCY_SAVE_FAILED` removed (atomic); full table in README with a test over every error path. Spec defines no transport codes. The "every error" statement is scoped to handler/middleware errors; framework/server responses are listed as exceptions (see the latest pass). |
| 14 | `privacy.decision.evaluated` | **Removed.** §19 lists no such event; no consumer exists in the repository. |
| 15 | Idempotency principal scope | **Fixed** (migration `000006`): scope is (tenant, principal, key). Spec silent → needs clarification. |
| 16 | Formats | **Made consistent:** `evidence_ref = prv-dec:<id>`; `input_fingerprint = context_hash = v2:<hex>`. Spec silent → needs clarification. |

### Latest pass (documentation-compliance remediation, 2026-10-21)

| Audit item | Gap | Result | Proven by |
|---|---|---|---|
| Row 8 | README said "All errors are `{error, error_code}`", but chi's 404/405, the panic recoverer, `/readyz` and net/http rejections are not in that envelope | **Fixed in documentation.** The guarantee now covers errors produced by handlers and the middleware; the README lists the framework/server responses and exactly what each returns (observed with a throwaway probe, deleted). No code, error code or router behaviour was changed. | probe run |
| Unscored hardening | `POST /privacy/decisions` read its body with an unbounded `io.ReadAll` | **Fixed in code.** The body is read through `http.MaxBytesReader` at 1 MiB (the sibling services' limit), after the four header checks and before the replay lookup. Over the limit → `400 VALIDATION_FAILED` ("unreadable body", the established refusal; no new code); nothing is evaluated, committed or stored, and the `Idempotency-Key` is not consumed. | handler `TestBodyLimit_*` (exactly 1 MiB accepted; 1 MiB+1 and 4 MiB refused before the first registry call and before any commit; key reusable; header checks still first; replay unchanged; real PostgreSQL: no decision or idempotency row left behind); mutations (no cap, cap halved, cap doubled) fail the intended tests |

Not changed: authorization (the specification names no permission), and every clarification and external item listed below.

### Verification

From `services/privacy-decision-svc`:
- **With** `REQUIRE_DB_TESTS=1` and a throwaway PostgreSQL superuser URL: **70 top-level tests pass, 0 skipped** (domain 5, handler 50, store 13, consent client 2). The app role is `NOSUPERUSER NOBYPASSRLS`; each DB test gets a fresh database; migration tests cover a clean database and an "existing data" scenario (legacy NULL-tenant evidence preserved, not deleted).
- **Without** a database: 54 pass, **16 skipped** — such a run does not prove RLS, atomicity or migrations.
- `go vet ./...`, `go build ./...`: clean.
- **Mutation checks** (each reverted): restoring the `DELETE` of NULL-tenant rows, fail-open transfer default, omitting `consent_check` from the fingerprint, and saving the idempotency response outside the transaction each make the intended tests fail.
- **Not performed:** Docker rebuild, live compose stack, frontend check, live downstream calls (downstream services are stubbed).

### External dependencies & blockers

1. **Service-to-service identity / RBAC grants.** Callers forward the end-user principal; `docs/architecture/05-security.md` §12.4 names goals but no mechanism, so none was invented.
2. **purpose-registry / PDC capabilities:** model-training attribute and human-approved AI policy, secondary-purpose compatibility evaluation, exact historical purpose-version lookup, sensitive-category taxonomy endpoint.
3. **retention-registry / DRC:** retention-period conformance; **consent/retention/transfer:** as-of state.
4. **Legacy NULL-tenant rows** need an operator-supplied tenant mapping.
5. **Frontend:** must send `X-Correlation-ID` (not service-owned).
6. **Needs clarification:** authorization-svc permission for POST/GET; default `expires_at`; identifier formats; idempotency principal scope; taxonomy drift.

### Final assessment — PRV-03

| Area | Status |
|---|---|
| Service-owned code and tests | **READY** for the service-owned items (see the 16-gap table) |
| Blocked items | 5, 6, 8, 9 (remainder) and gap 1 legacy data |
| Frontend | **Not assessed / not service-owned** |
| Integration | **BLOCKED — DEPENDENCY** |
| Overall production status | **NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY** |

---

## Service: privacy-rights-svc
**Port:** 8154
**Classification:** Independent (depends on authorization-svc; workflow-svc integration is caller-supplied reference per §14.1; self-contained evidence service)

> **Correction notice.** Earlier versions of this section claimed **100% / Ready for Production / 0 gaps**. A documentation-compliance audit scored the service **70.8% (17 of 24 comparable items)** and found: the three read routes required no principal and no grant; the row-level-security policies still carried the `tenant_id IS NULL OR …` exception, so a row with no tenant was visible to (and writable by) every tenant; idempotency proceeded when its lookup failed, ignored save failures, and accepted `X-Idempotency-Key` in the mandatory check but not in the replay lookup; authorization ran only after the case had been looked up (a 404-vs-403 existence oracle); the §15.2 gate trusted a denormalised flag and any non-empty manifest hash; error bodies carried no `error_code`; and the documented test and README counts were stale. This section replaces those claims and describes the service **as remediated and verified in this pass**. The "events `privacy.rights_request.received` / `.closed`" claim is **withdrawn**: §19 of ZS-SVC-W-001 declares no such event and the service (deliberately) emits none.

### Status

| Area | Assessment | Basis |
|---|---|---|
| **Backend (service-owned)** | **Service-owned gaps fixed; one data item and the non-verifiable §15.2 elements are external** | See the table below. No compliance percentage is asserted. |
| **Frontend** | **Not assessed** | Not part of this pass. |
| **Integration** | **BLOCKED — DEPENDENCY** | Grants for the three actions must be provisioned in `authorization-svc`; not verified against a live instance. |
| **Production readiness** | **NOT READY** | External dependencies below; legacy NULL-tenant rows, if any, need an operator mapping. |

### What changed in this pass

| # | Gap | Result |
|---|---|---|
| 1 | RLS `tenant_id IS NULL OR …` on every table | **Fixed** by migration `000005`: exception removed from all four policies; `CHECK (tenant_id IS NOT NULL) NOT VALID` for new rows; `NOT NULL` set only when no legacy NULL row exists. Nothing is deleted or rewritten; legacy NULL-tenant rows are preserved, invisible, and warned about — **BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING.** The store also refuses to run without a verified tenant. (`X-Tenant-Id` was already required by the middleware.) |
| 2 | Read routes unauthenticated | **Fixed.** Principal (401), tenant (401) and an `authorization-svc` grant (403/503) on all three GETs. Authorization precedes any lookup on reads *and* mutations, so existing and missing ids answer alike. The specification names no read permission; none was invented — reads reuse `PRIVACY_RIGHTS_REQUEST_PROCESS` (needs clarification). |
| 3 | Idempotency | **Fixed.** Scope (tenant, principal, key); hash binds method + path + body; lookup failure → 503; claim + business change + stored response are one transaction (failure persists nothing, concurrent identical requests execute once); `X-Idempotency-Key` honoured everywhere. |
| 4 | §15.2 gate | **Fixed for what is verifiable:** a verified identity event must exist (a forged flag or a failed attempt does not pass); manifests must carry a SHA-256 digest and a non-negative candidate count. **External (WFC / reviewers / adapters):** applicable-right determination, scope review, exemptions, third-party protection, redaction, approved response assembly — the gate is necessary, not sufficient. |
| 6 | API / error contract | **Fixed.** Every error the handlers and the tenant middleware produce carries `error_code` (chi/HTTP-server responses are documented exceptions — see the README, §7); body validation happens after authentication/authorization; tenant mismatch is `403 TENANT_SCOPE_MISMATCH` (it had borrowed `PRV-003`). §32 messages and statuses are unchanged. |
| 7 | Events | **No change required.** §19 declares no rights intake/closure event. `privacy.rights.action_required` is not emitted; who owns it is not stated — needs clarification. |
| 8 | Documentation | Rewritten (this section, PRV-04, README). |

### Latest pass (documentation-compliance remediation, 2026-10-21)

| Audit item | Gap | Result | Proven by |
|---|---|---|---|
| Row 20 | Documented pre-flight puts the principal check before the tenant check, but the tenant middleware runs before the handler, so a request with neither header answered `TENANT_SCOPE_REQUIRED` | **Fixed in code.** `TenantContext` now applies the principal check first (`401 AUTHENTICATION_FAILED`), then the tenant check (`401 TENANT_SCOPE_REQUIRED`), still before any handler and still fail-closed. Statuses are unchanged (both 401). | middleware `TestTenantContext_*`; handler `TestPreflightOrder_*` (all 16 route/prefix combinations); mutations (principal check removed, tenant check removed) fail the intended tests |
| Row 28 | "Every error is `{error, error_code}`" overstated: chi's 404/405, the panic recoverer, `/readyz` and net/http rejections are not in the envelope | **Fixed in documentation.** The README now states the guarantee covers errors produced by handlers and the middleware, and lists the framework/server responses and exactly what they return (verified by a throwaway probe). No error code was invented for them and no framework behaviour was changed. | probe run (deleted) |
| Unscored risk | The authorization client sent the `/v1/authorize` body with no identity or envelope headers | **Fixed in code and documented.** It now sends `X-Principal-Id`, `X-Legal-Entity-Id`, `X-Tenant-Id` (context), `X-Request-Id`, `X-Correlation-ID` (caller's when present), `X-Source-Channel: web` and a fresh `Idempotency-Key`, matching the sibling privacy services. No permission was added; `authorization-svc` was not touched or contacted. | authz `TestCheckAllowed_*`; handler `TestPreflight_AuthorizationCallCarriesTheVerifiedContext`; mutations (tenant, principal, correlation, idempotency headers) fail the intended tests |

Not changed: the eight clarification items and the other unscored observations (see below).

### Verification

Run from `services/privacy-rights-svc`:
- `go test ./...` with `REQUIRE_DB_TESTS=1` against a throwaway PostgreSQL (fresh database per test, migrations 000001–000006, app role `NOSUPERUSER NOBYPASSRLS`): **66 top-level tests pass, none skipped** (handler 50, store 8, authz 6, middleware 2). Without a database: 58 pass, 8 skipped.
- `go vet ./...` and `go build ./...`: clean.
- **Mutation checks** (each reverted): restoring the NULL-tenant policy exception, trusting the denormalised identity flag, and proceeding on an idempotency lookup failure each make the intended tests fail.
- Migration tests cover a clean database and an existing-data scenario (legacy NULL-tenant case and evidence preserved, not deleted, invisible, new NULL rows refused, append-only trigger still enforced).
- **Not performed:** Docker rebuild, live compose stack, live `authorization-svc`, frontend.

### External dependencies

`authorization-svc` grants; WFC / reviewers / domain adapters (§14.1, §15.2); operator tenant mapping; needs clarification: read-route action, `privacy.rights.action_required` ownership, asymmetric manifest signatures.

### Final Assessment — PRV-04

| Area | Status |
|---|---|
| Backend (service-owned) | Fixed (see above) |
| Tenant isolation | Strict in code and RLS; verified on real PostgreSQL |
| Frontend | Not assessed |
| Integration | **BLOCKED — DEPENDENCY** |
| Production readiness | **NOT READY** |

---

## Service: privacy-transfer-svc
**Port:** 8155
**Classification:** Interlinked (depends on privacy-purpose-registry-svc + authorization-svc)

> **Correction notice.** Earlier versions of this section claimed **100% / Ready for Production**, listed the event `privacy.transfer_decision.evaluated`, said the README had 114 lines and the suite 24 tests, and said runtime isolation failed because services connect as the `postgres` superuser. A documentation-compliance audit scored the service **63.6% (14 of 22 comparable items)** and found: every read route (and the trigger-evaluation route) required no principal and no grant; the row-level-security policies still carried the `tenant_id IS NULL OR …` exception; authorization ran only after the record was looked up (a 404-vs-403 existence oracle); idempotency proceeded when its lookup failed, ignored save failures and was not atomic with the change; the §19 events were published after commit with errors discarded; reassessment triggers 2/5/7 were purely caller-declared while the documentation said "all 8 automated", the sensitive-category check was a substring match that missed registered categories, a declared trigger string was echoed into a reason code, and the purpose-registry client forwarded no principal. This section replaces those claims and describes the service **as remediated and verified in this pass**. The superuser statement is corrected: the compose default is `zoiko_app`; whether production runs as a superuser is a deployment matter (a superuser bypasses RLS regardless of policy).

### Status

| Area | Assessment | Basis |
|---|---|---|
| **Backend (service-owned)** | **Service-owned gaps fixed; three triggers and registry end-to-end verification are external** | See the table below. No compliance percentage is asserted. |
| **Frontend** | **Not assessed** | Not part of this pass. |
| **Integration** | **BLOCKED — DEPENDENCY** | PRV-01 requires a read grant for the forwarded principal in `authorization-svc`; not verified against live services. |
| **Production readiness** | **NOT READY** | External dependencies below; legacy NULL-tenant rows, if any, need an operator mapping. |

### What changed in this pass

| # | Gap | Result |
|---|---|---|
| 1 | RLS `tenant_id IS NULL OR …` | **Fixed** by migration `000005_strict_tenant_isolation` on all five evidence tables: exception removed, `CHECK (tenant_id IS NOT NULL) NOT VALID` for new rows, `NOT NULL` only when no legacy NULL row exists. Nothing is deleted or rewritten; legacy NULL-tenant rows are preserved, invisible and warned about — **BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING.** The store refuses to run without a verified tenant. (`X-Tenant-Id` was already required by the middleware.) |
| 2 | Read routes unauthenticated | **Fixed.** Principal (401), tenant (401) and an `authorization-svc` grant (403/503) on every GET and on trigger evaluation, **before** any lookup. The specification names no read permission; none was invented — each read reuses the existing action of the resource it exposes (needs clarification). |
| 3 | Idempotency | **Fixed.** Scope (tenant, principal, key); hash binds method + path + body; lookup failure → 503; claim + change + event + stored response are one transaction (failure persists nothing; concurrent identical requests execute once). |
| 4 | Reassessment triggers | **Fixed for what is derivable:** one shared evaluation for `EvaluateTransfer` and the trigger endpoint; exact sensitive-taxonomy membership; a subprocessor attached after the latest assessment is a trigger; lookup failures fail closed; a declared trigger must be one of the eight §17.1 names. **External:** triggers 2 (automated decisioning), 5 (architecture change) and 7 (incident) concern facts recorded in other systems, so they remain caller-declared. |
| 5 | Registry integration | **Service-owned part fixed:** the caller's principal and tenant are forwarded to PRV-01. **BLOCKED — EXTERNAL DEPENDENCY:** the read grant in `authorization-svc`. |
| 7 | Events | **Fixed.** The three §19 events (`privacy.processor.changed`, `privacy.transfer.authorization_changed`, `privacy.assessment.expiring`) go through a transactional outbox in the same transaction as the change; a relay is the only Kafka producer. (`privacy.transfer_decision.evaluated` in older text was never a §19 event and is not emitted.) |
| 8 | Documentation | Rewritten (this section, PRV-05, README). |

### Latest pass (documentation-compliance remediation, 2026-10-21)

| Audit row | Gap | Result | Proven by |
|---|---|---|---|
| 17 | The README said `EvaluateTransfer` and the trigger endpoint "cannot disagree", but `EvaluateTransfer` read the latest assessment only when `assessment_required` was true, so triggers 3 (new subprocessor), 4 and 8 were invisible to it otherwise: with an approved assessment and a newly attached subprocessor it returned `AUTHORIZED` while the endpoint reported reassessment needed | **Fixed in code.** `EvaluateTransfer` now reads the latest assessment as trigger evidence regardless of `assessment_required`, feeds the same `computeTriggers`, and also honours trigger 8 (`ASSESSMENT_EXPIRED`) in the trigger loop. `assessment_required` still controls only the assessment gate (missing / REJECT / REMEDIATE / expired) and what enters the decision (measures, `assessment_id`, review date). A failing assessment lookup fails closed (`REVIEW_REQUIRED`, `PRV-019`). No new reason code, no duplicated trigger logic. ZS-SVC-W-001 §17.1 makes the eight triggers mandatory and makes none depend on a caller flag. | handler `TestTriggerConsistency_*` (the audited scenario for both flag values; a matrix of seven trigger scenarios × both flag values with POST and GET endpoints; `assessment_required` semantics unchanged; lookup failure; real PostgreSQL); mutations (old evidence gating, trigger 8 dropped, lookup failure ignored) fail the intended tests |
| 25 | README said principal is checked before tenant; the tenant middleware actually runs first | **Fixed in documentation (no code change).** §18.1 requires both headers and prescribes no order, so the working fail-closed behaviour was kept and the docs now state the real order: tenant (middleware) → principal → correlation → idempotency key → authorization. A request with neither header is refused for the tenant; both are `401`. | handler `TestPreflightOrder_*` |
| 38 | "Migration 000005" / "migrations 000001–000006" while eight files exist with two shared prefixes | **Fixed in documentation (no rename).** The README has a Migrations section listing the eight files in application order. The loader sorts by file name and tracks by full file name, so the layout is safe, and renaming applied files would make them re-run. | `deployments/migrate.py`, `internal/testdb` (both sort file names) |

Not changed: the privacy-decision-svc client (it omits the mandatory `X-Correlation-ID` and `Idempotency-Key`; that is the caller's to fix), authorization, and every clarification and external item below.

### Verification

Run from `services/privacy-transfer-svc`:
- `go test ./...` with `REQUIRE_DB_TESTS=1` against a throwaway PostgreSQL (fresh database per test, all eight migration files — `000001`–`000006`, two files each sharing the `000005` and `000006` prefixes —, app role `NOSUPERUSER NOBYPASSRLS`): **62 top-level tests pass, none skipped** (handler 54, store 8). Without a database: 53 pass, 9 skipped.
- `go vet ./...` and `go build ./...`: clean.
- **Mutation checks** (each reverted): restoring the NULL-tenant exception, dropping the decision event, proceeding on an idempotency lookup failure, and reverting to a substring sensitivity match each make the intended tests fail.
- Migration tests cover a clean database and an existing-data scenario (legacy NULL-tenant rows preserved, invisible, new NULL rows refused, append-only trigger still enforced).
- **Not performed:** Docker rebuild, live compose stack, live `authorization-svc` / purpose registry, frontend.

### External dependencies

`authorization-svc` grants (including PRV-01's read grant for the forwarded principal); the systems that originate triggers 2, 5 and 7; operator tenant mapping; needs clarification: read-route actions, taxonomy drift.

### Final Assessment — PRV-05

| Area | Status |
|---|---|
| Backend (service-owned) | Fixed (see above) |
| Tenant isolation | Strict in code and RLS; verified on real PostgreSQL |
| Frontend | Not assessed |
| Integration | **BLOCKED — DEPENDENCY** |
| Production readiness | **NOT READY** |

---

## PART 1 DOMAIN SUMMARY TABLE

| Service | FE % | BE % | Integration % | Overall % | Health | Readiness |
|---------|------|------|---------------|-----------|--------|-----------|
| privacy-purpose-registry-svc | **Not updated** | No % asserted (earlier ≈ 90.6 superseded; domain audit scored 87.8%) | **BLOCKED — DEPENDENCY** | **Not 100** | Working | **NOT READY — the earlier "100 / Ready for Production" predated the remediation; see the privacy-purpose-registry-svc and PRV-01 sections** |
| privacy-consent-svc | Not assessed | No % asserted (earlier ≈ 93.9 withdrawn; domain audit scored 75.0% (21/28) before this remediation) | **BLOCKED — DEPENDENCY** | Not 100 | Working | **NOT READY — the earlier "100 / Ready for Production" was unsupported; see the privacy-consent-svc and PRV-02 sections** |
| privacy-decision-svc | **Not assessed** | No % asserted (earlier "100 / 34 of 34" withdrawn; audit scored 66.7% before remediation) | **BLOCKED — DEPENDENCY** | **Not 100** | Working | **NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY; see the privacy-decision-svc and PRV-03 sections for what is fixed and what is blocked (service identity, purpose-registry/PDC capabilities, legacy tenant mapping, frontend correlation header)** |
| privacy-rights-svc | Not assessed | No % asserted (earlier 100 withdrawn; audit scored 70.8% before remediation) | BLOCKED — DEPENDENCY | Not 100 | Working | NOT READY — see the privacy-rights-svc and PRV-04 sections |
| privacy-transfer-svc | Not assessed | No % asserted (earlier 95/90/88/91 withdrawn; audit scored 63.6% before remediation) | BLOCKED — DEPENDENCY | Not 100 | Working | NOT READY — see the privacy-transfer-svc and PRV-05 sections |

**Average Frontend Completion %:** 91.6%
**Average Backend Completion %:** 93.0%
**Average Integration Completion %:** 89.6%
**Overall Domain Completion %:** 90.8%
**Total Services Audited:** 5
**Services Ready:** 1
**Services with Minor Gaps:** 1
**Services Not Ready:** 3

> **Note (PRV-01):** the averages and counts above were computed before the PRV-01 correction and still include its earlier 100/100/100 row; they are not current for that service.

## PART 1 — KEY EVIDENCE / CROSS-CUTTING FINDINGS

1. This is the most mature domain audited by code volume-to-quality ratio: no TODO/FIXME/"not implemented"/`panic("unimplemented")` markers anywhere in `internal/` or `cmd/` across all 5 services (only comments describing intentional in-memory test doubles, e.g. the test-only `stubStore` in `privacy-purpose-registry-svc/internal/handler/stub_store_test.go` and `privacy-transfer-svc/internal/store/pg_store.go:36` — these are unit-test stubs, never used in the production binary, which always wires `store.NewPgStore(pool, ...)` in `cmd/server/main.go`. **Correction (PRV-01):** the `pg_store.go:28,36` citation for this service was stale; those lines are now the `isInvalidUUID`/`isUniqueViolation` helpers).
2. `go vet ./...` and `go test ./...` are clean/passing for all 5 services (handler package tests pass; no test files exist for store/config/authz/events packages, meaning DB-layer logic is exercised only via handler tests against an in-memory stub store, not against real Postgres — a real integration-test gap common to all 5). **Correction (PRV-01):** `privacy-purpose-registry-svc` now has a real-PostgreSQL integration suite (store package) and handler tests that run end-to-end against PostgreSQL under RLS as a non-superuser role; the "DB-layer exercised only via a stub" statement no longer applies to it.
3. Fail-closed authorization is consistent and real: every mutating route calls a live HTTP client to authorization-svc (`POST /v1/authorize`), unreachable → 503, denied → 403, with a bounded 5s in-process decision cache (mirrored across all 5 `authz/client.go` files).
4. Row-Level Security is not just declared but actually activated: `pg_store.go` wraps every transaction with `SELECT set_config('app.tenant_id', $1, true)` (originally confirmed at `privacy-purpose-registry-svc/internal/store/pg_store.go:81-82`) before touching tables with `FORCE ROW LEVEL SECURITY` policies (migration `000003_add_rls.up.sql`). **Correction (PRV-01):** the session variables are now `app.tenant_id` **and** `app.platform_scope` (migration 000006); `withTenant` moved: it is now `PgStore.withTenant` in `pg_store.go` (the `set_config` call is at line 127 at the 2026-10-21 re-audit), so the originally quoted line numbers are stale for this service.
5. Cross-service dependency chain (privacy-purpose-registry-svc ← privacy-consent-svc ← privacy-decision-svc, plus privacy-transfer-svc → privacy-purpose-registry-svc, plus privacy-decision-svc → retention-registry-svc) is real in both docker-compose `depends_on`/env vars AND in application code (dedicated `internal/purposeregistry`, `internal/consentregistry`, `internal/retentionregistry` client packages performing genuine HTTP calls, not mocks) — this is genuinely an Interlinked domain, with privacy-rights-svc as the sole Independent service.
6. Frontend is server-action-based (Next.js "use server"), never calls the backend from the browser (client.ts:1-6 explains this is deliberate — Go services ship no CORS middleware), and every `lib/api/privacy-*.ts` client targets the real localhost ports (8151-8155) per `lib/api/config.ts:108-112`, with a documented gateway path alternative at lines 297-301.
7. ~~Biggest recurring gap across the domain: known, documented, and intentional decision-outcome incompleteness — PRV-03 never produces RESTRICT/REVIEW_REQUIRED (only PERMIT/BLOCK/INDETERMINATE) and PRV-05 never produces CONDITIONAL — both because no jurisdiction-specific lawful-basis/PDC rules engine exists anywhere in the platform yet.~~ **Correction:** stale — both services now produce all documented outcomes. PRV-03 produces all 5 (PERMIT, RESTRICT, BLOCK, REVIEW_REQUIRED, INDETERMINATE; RESTRICT via the PRV-05 CONDITIONAL transfer integration — exercised against a stubbed transfer client in this service's tests, not live-verified in the latest pass). PRV-05 produces CONDITIONAL (see the PRV-05 section's Gap 1). This is no longer a gap in either service.
8. ~~Smaller UI gaps: privacy-rights-svc's AttachWFCProcessRef is backend-complete but not UI-complete.~~ **Correction:** stale — `Sub-Form 2D`, `attachWFCProcessRefAction`, and Case Inspector display of `wfc_process_ref` are all implemented and wired in `PrivacyRightsWorkbench.tsx` (PRV-04 Gap 6). This point previously also named privacy-consent-svc's notice approve/publish/withdraw/presentation-receipt lifecycle as backend-complete-but-not-UI-complete; that was likewise stale — Gap 1 in the PRV-02 section documents these UI actions as implemented and wired into `PrivacyPanels.tsx`.
9. None of the 5 services has its own README.md (top-level `services/README.md` also omits all 5 from its architecture table) — documentation for intended behavior lives entirely in code/handler doc-comments and docker-compose.yml inline comments, which are unusually thorough but non-standard as a source of truth. **Correction (PRV-01):** `privacy-purpose-registry-svc` now has a `README.md`.

---

# PART 2 — DOCUMENTATION COMPLIANCE AUDIT (ZS-SVC-W-001)

**Methodology:** the formal specification `docs/architecture/ZS-SVC-W-001_Privacy_Consent_Purpose_Data_Rights_Control_Detailed_Service_Specifications_v1.0.docx` was located, extracted (via unzip + XML parse, since no .docx reader tool was available), and used as the single documented source of truth. Every documented endpoint, business rule, schema, invariant, negative-path scenario, and error code was extracted first, then compared line-by-line against the actual Go implementation (handler.go, domain/types.go, pg_store.go, migrations) for each of the 5 services. Read-only — no code was modified during this audit.

**Docs location:** `docs/architecture/ZS-SVC-W-001_Privacy_Consent_Purpose_Data_Rights_Control_Detailed_Service_Specifications_v1.0.docx` (all 5 services; no service has its own separate spec file).
**Code location (per service):** `services/<service-name>/internal/{handler,domain,store}/`, `services/<service-name>/deployments/migrations/`.

## Document structure (as extracted)

| Spec section | Content |
|---|---|
| §0-7 | Governing doctrine, architecture decision, canonical service catalogue (PRV-01..PRV-05), authority boundaries, core conceptual model |
| §8-9 | PRV-01 service contract, activation gates, lifecycle, canonical API table (§9.1 — the ONLY per-service endpoint table in the whole document) |
| §10-11 | PRV-02 objects, dark-pattern resistance rules, lifecycle, technology/channel scope, withdrawal semantics |
| §12-13 | PRV-03 decision input/output contract, decision sequence, mandatory safeguards, decision durability |
| §14-15 | PRV-04 rights request model, right families, identity/discovery/third-party controls, disclosure gate |
| §16-17 | PRV-05 processor/subprocessor/transfer object model, separation from routing, DPIA/TIA gate, reassessment triggers |
| §18 | **Consolidated Canonical API Contracts** — endpoint table spanning PRV-01 through PRV-05 (found later in the document; corrects an earlier stated "no endpoint documentation exists" claim for PRV-02/03/04/05 — see correction note below) |
| §18.1 | Idempotency and concurrency — mandatory for ALL mutating APIs across all 5 services |
| §19 | Event contracts and downstream enforcement |
| §20-27 | Persistence/lineage architecture, domain adoption patterns (HR/payroll, accounting/tax/legal), multi-tenant patterns, security/residency controls, inter-control-plane contracts, operational runbooks, observability |
| §28-30 | 60 Negative-Path Certification Scenarios (PRV-N01 through PRV-N60) |
| §31 | 30 Non-Bypassable Privacy Invariants (PRV-I01 through PRV-I30) |
| §32 | Stable Error and Reason Code Contract (PRV-001 through PRV-020) |
| §33-37 | Open decisions before production, Definition of Ready/Done, 8-wave implementation sequence, control traceability matrix, standards references, final doctrine |

---

## PRV-01 — privacy-purpose-registry-svc (port 8151)

> **Correction.** This section previously scored **100%** with "none remaining" and listed gate codes and a gate-8 value that did not match the code. A later compliance audit scored the service **56.3%** (18 of 32 comparable items). The remediation pass described in the first PRV-01 section above re-scores it **29 of 32 (≈ 90.6%)** on the same table. The table below is the corrected comparison. The full behavioural description, the pre-flight, scope model, gate table and verification evidence are in that section; this one keeps only the contract-vs-code diff.

### 1. Extracted contract (§8-9)

**Canonical APIs (§9.1):** `POST /privacy/processing-activities` (draft identity + initial version); `POST …/{id}/versions` (successor draft from an explicit parent); `POST …/{id}/validate`; `POST …/{id}/submit` (send to the governed WFC approval workflow); `POST …/{id}/activate` (only an approved version, at the governed effective time); `GET …/{id}?as_of=` (exact valid/known version); `GET /privacy/ropa` (role/jurisdiction-filtered projection).

**Lifecycle (Figure 4):** `DRAFT → VALIDATE → PRIVACY/DOMAIN REVIEW → APPROVED → ACTIVE → SUSPENDED / RETIRED`, reject/fix loop back to DRAFT.

**Minimum activation gates (§8.2):** (1) named accountable business owner **and** privacy owner; (2) controller/processor role resolved; (3) specific purpose statement **and** necessity/minimization rationale; (4) approved lawful-basis and, where applicable, special-category/other condition references from PDC; (5) subjects, categories, **sources, recipients**, jurisdictions enumerated; (6) retention/DRC references **and transfer/processor dependencies**; (7) notice/consent dependency explicit; (8) DPIA/TIA resolved by policy **or marked review-required**.

**Non-bypassable invariants (§9.2):** an ACTIVE activity resolves one exact approved version at any effective/knowledge time; material change forces a new version; RoPA is a read-only projection. **§18.1:** all mutating APIs require tenant scope, actor, idempotency key and correlation id; evidence is append-only. **§19/§19.1:** events describe committed state and survive a bus outage. **§20.3:** preserve when a fact became effective **and** when ZoikoSuite learned/approved it. **§32:** stable code contract. **§2.1:** WFC is authoritative for workflow timers, maker-checker orchestration, approvals and escalation.

### 2. Implementation

See the first PRV-01 section: *What the service does (current behaviour)*. 26 routes (5 purpose + 20 activity + RoPA, canonical and explicit-version), every one behind the same pre-flight; no `/v1` aliases are documented or mounted.

### 3. Diff table (corrected; same rows as the audit that scored 56.3%)

| # | Item | Documented | Implemented now | Status | Notes |
|---|---|---|---|---|---|
| 1 | Routes | §9.1 + purposes + RoPA | All mounted | ✅ Match | |
| 2 | `POST …/processing-activities` | Identity + initial version | Same | ✅ Match | |
| 3 | `POST …/{id}/versions` | Successor from explicit parent | Parent required and must exist in the caller's plane | ✅ Match | |
| 4 | `validate` | Deterministic findings, no activation | DRAFT only; VALIDATED or stays DRAFT with findings | ✅ Match | |
| 5 | `submit` | Send to governed WFC workflow | Local audited transition to SUBMITTED; no WFC hand-off | ⚠️ Needs Clarification | WFC not integrated (dependency-blocked) |
| 6 | `activate` effective time | "Governed effective time" | Server instant; caller value only as "now ± 60 s"; backdate/post-date `422` | ✅ Match | Scheduled activation is WFC's (§2.1) |
| 7 | `GET …/{id}?as_of=` | Exact valid/known version | Valid-time window + `known_as_of`; invalid value `400 INVALID_AS_OF` | ✅ Match | |
| 8 | Effective **and knowledge** time (§20.3) | Both preserved | `effective_from/to`, `approved_at`, `known_as_of` | ✅ Match | Suspension time-sliced from `activity_lifecycle_audit`; the 200-with-status behaviour at a suspended instant is the service's reading (spec silent) |
| 9 | `GET /privacy/ropa` | Filtered read-only projection | `PRIVACY_ROPA_READ`; ACTIVE and in force only | ✅ Match | |
| 10 | Gate 1 owners | Business owner **and** privacy owner | Both required (`PRV-003`) | ✅ Match | `privacy_owner` added |
| 11 | Gate 2 role | Resolved | `PRV-003` | ✅ Match | |
| 12 | Gate 3 rationale | Statement + necessity rationale | `necessity_rationale` field and gate (`PRV-001`) | ⚠️ Needs Clarification | Spec does not say purpose vs activity owns it |
| 13 | Gate 4 lawful basis / special conditions | Approved, from PDC | Presence checked: `PRV-005`, `PRV-010` | ⚠️ Partial | Existence in PDC not verifiable (dependency) |
| 14 | Gate 5 enumerations | Subjects, categories, sources, recipients, jurisdictions | All required; registries enforced (`PRV-004`/`010`/`019`) | ✅ Match | Full ISO 3166-1 list |
| 15 | Gate 6 dependencies | Retention, transfer, processor | Required (`PRV-014`/`015`/`017`); existence not verified | ⚠️ Partial | DRC/PRV-05 dependency |
| 16 | Gate 7 | Explicit | `PRV-006` | ✅ Match | |
| 17 | Gate 8 | Resolved **or review-required** | `RESOLVED`/`NOT_REQUIRED`/`REVIEW_REQUIRED` (`PRV-016`) | ✅ Match | Earlier table's "`EXEMPT`" was wrong |
| 18 | Block activation on bad jurisdiction/category | §8.1 failure posture | Full gate set re-run at activation, `422` with findings | ✅ Match | |
| 19 | One ACTIVE version | §9.2 | Atomic supersede + unique index; SUSPENDED siblings retired too | ✅ Match | Concurrency tested |
| 20 | History preserved / immutable | §8.1 | Update triggers + DELETE-rejecting triggers | ✅ Match | |
| 21 | Principal required | §18.1 | `401` on every route | ✅ Match | |
| 22 | Fail-closed RBAC | `authorization-svc` | Action per route, reads included | ✅ Match | |
| 23 | Maker-checker | §18, §2.1 | `403 SOD_DENIED` on approve/reject/publish | ✅ Match | |
| 24 | Read-route auth | §8.1 caller set | `401`/`403` on all GETs | ✅ Match | |
| 25 | Tenant scope mandatory | §18.1 | `401 TENANT_SCOPE_REQUIRED`; platform only via sentinel | ✅ Match | |
| 26 | Tenant isolation | §24, §23.1 | Handler plane check + capability-gated RLS | ✅ Match | Real-Postgres verified |
| 27 | Idempotency key | §18.1 mandatory | Mandatory; replay after authn+authz; committed atomically with the change | ✅ Match | |
| 28 | Correlation id | §18.1 | `400` if missing, echoed | ✅ Match | |
| 29 | Transactional outbox | §19.1 | Store enqueues in-transaction; relay retries, full acks | ✅ Match | Outage test |
| 30 | One event per change | §19 | Handler publishes nothing | ✅ Match | |
| 31 | Superseded version's event | §9.2/§19 | `retired` event + audit row per superseded version | ✅ Match | |
| 32 | §32 error contract | Stable codes | `error_code` on every error | ✅ Match | |
| 33 | Tests | — | 95 top-level, real Postgres, mutation-checked | ✅ Match | |
| 34 | Documentation accuracy | — | This update | ✅ Match | |

### 4. Compliance score

- **Latest documentation-compliance re-audit (2026-10-21; 49 comparable items — a different, finer-grained item list than the older 34-row table and the 36/41 domain score below, which are historical):** before this pass 42 ✅ / 7 ⚠️ / 0 ❌ = 42/49 = 85.7%. Rows 4, 35, 50 and 51 are now ✅ → **46 ✅ / 3 ⚠️ / 0 ❌ = 46/49 = 93.9%**. The three remaining partials are rows 11, 30 and 32 (WFC, PDC, DRC/PRV-05 — external). Needs clarification and excluded: rows 15, 34, 37, 52. This is a compliance percentage only: **not production ready** — see the blockers and the unverified items above.
- **Historical:** 29 of 32 comparable items (≈ 90.6%, was 18/32 = 56.3%) before the latest pass; the domain audit then scored 87.8% (36/41). **No percentage is asserted now.** Row 8 (suspension time-slicing) is now fixed; rows 13 and 15 (reference existence — external) remain partial; row 27 (idempotency) is now a full match.
- **Backend:** service-owned gaps fixed. **Frontend:** not updated, not compliant with the new contract. **Integration:** BLOCKED — DEPENDENCY on three clients, the frontend and `authorization-svc` provisioning. **Overall:** not 100%. **Production readiness: NOT READY.**

**Top remaining gaps by risk:**
1. **Integration (blocking, external):** the three clients forward `X-Tenant-Id` and `X-Principal-Id`; whether those principals hold the new read grants in `authorization-svc`, how a call with no end-user principal is identified, and end-to-end behaviour on the live stack are unverified. Dependency-blocked.
2. **Provisioning:** new actions `PRIVACY_PURPOSE_READ`, `PRIVACY_ACTIVITY_READ`, `PRIVACY_ROPA_READ` and platform-plane grants must exist in `authorization-svc`; platform-scope authorization unverified against the live service.
3. **Frontend** cannot satisfy gates 1/3/4/6 or the new activation rule.
4. Reference existence unverifiable (rows 13, 15 — external). Suspension time-slicing (row 8) and atomic idempotency are fixed.

---

## PRV-02 — privacy-consent-svc (port 8152)

> **Correction.** This section previously scored **100%**, then ≈ 93.9% (31/33), and the table's route count was stale. A later domain audit scored the service **75.0% (21/28)**; the remediation described in the "Service: privacy-consent-svc" section above addresses its service-owned findings. The table below is the corrected comparison against the code as it is now; it carries **no percentage**, and items that depend on other services are not counted as matches.

### 1. Extracted contract (§10-11, §18, §19)

**§18:** `POST /privacy/consents` → `receipt_id`, purpose/scope, `notice_version`, `status`, `evidence_hash`; `POST /privacy/consents/{id}/withdraw` → `withdrawal_id`, `effective_at`, `propagation_correlation`; `POST /privacy/notices/{id}/presentation-receipts` → `receipt_id`, subject/session, version, timestamp, channel/template.

**§10.1 objects:** NoticeVersion, PresentationReceipt, ConsentRequest, ConsentReceipt (receipt integrity), WithdrawalReceipt, PreferenceAssertion (source, **effective time**). **§11.1:** workforce, proxy, minors, offline/imported, tracking. **§11.2:** withdrawal prospective, never deletes evidence. **§18.1:** tenant scope, actor, idempotency key and correlation id on every mutating API; append-only; replay returns the original. **§19/§19.1:** `privacy.consent.changed` from a transactional outbox. **§33 OD-06:** the integrity *scheme* is open.

### 2. Implementation (current)

17 routes: notices 11 (create, get, create-version, canonical latest approve/publish/withdraw/presentation-receipts, explicit-version equivalents), consents 4 (record, get status, withdraw, **invalidate** — `PRIVACY_CONSENT_INVALIDATE`), preferences 2. Mutating pre-flight: principal → tenant → correlation → authorization → idempotency (key + replay) → validation → resolve. Idempotency scoped to (tenant, principal, key), bound to method + path + body, atomic with the evidence and the outbox event.

### 3. Diff table (fresh audit of the remediated service)

| # | Item | Status | Notes |
|---|---|---|---|
| 1 | Routes (17) | ✅ Match | Earlier "13" was stale |
| 2 | `POST /consents` §18 fields | ✅ Match | |
| 3 | Withdraw §18 fields | ✅ Match | |
| 4 | Presentation-receipt §18 fields | ✅ Match | `receipt_id` added beside `presentation_receipt_id` |
| 5 | Notice lifecycle and segregation of duties | ✅ Match | |
| 6 | ConsentRequest object | ✅ Match | |
| 7 | Receipt integrity | ✅ Match | v2; scheme is OD-06 (open) |
| 8 | Offline-import provenance | ✅ Match | |
| 9 | Minors / age band | ✅ Match | |
| 10 | PRV-008 notice binding | ✅ Match | |
| 11 | Replay (PRV-N04) | ✅ Match | |
| 12 | Withdrawal / replay (PRV-N05) | ✅ Match | |
| 13 | Invalidation | ✅ Match | |
| 14 | Append-only evidence | ✅ Match | |
| 15 | Mandatory request context | ✅ Match | |
| 16 | Pre-flight order | ✅ Match | Validation follows authentication and authorization on every route |
| 17 | RBAC on every route | ✅ Match | `GetNotice` intentionally principal-less; grants are external |
| 18 | Strict RLS | ✅ Match | Verified on real PostgreSQL |
| 19 | Foreign receipt → 404 after authorization | ✅ Match | |
| 20 | Reads gated | ✅ Match | |
| 21 | Purpose-registry integration | ⛔ BLOCKED — EXTERNAL DEPENDENCY | Principal and tenant forwarded; the registry grant is external |
| 22 | Outbox events and payload | ✅ Match | |
| 23 | Typed errors | ✅ Match | Invalid notice transitions are `409 INVALID_TRANSITION`; idempotency conflicts `409 IDEMPOTENCY_MISMATCH`; framework-level responses are documented as outside the envelope |
| 24 | Idempotency: scope, binding, atomicity, fail-closed | ✅ Match | |
| 25 | Latest-version routes | ✅ Match | |
| 26 | Preference effective time | ✅ Match | `effective_at` persisted, returned and used for resolution; field name/default/resolution are the service's reading of §10.1 (spec silent) |
| 27 | Documentation accuracy | ✅ Match | One idempotency description in the README; counts current |

Needs clarification / external (not scored): proxy authority verification, workforce consent (PDC), grant provisioning, WFC boundary, `GetNotice` invalid `as_of`. at most one PUBLISHED notice version per notice (design decision; not enforced, not stated by the spec).

**Latest documentation-compliance re-audit (2026-10-21; 48 comparable items):** before this pass 46 ✅ / 2 ⚠️ / 0 ❌ = 95.8%. Rows 45 and 48 are now ✅ → **48 ✅ / 0 ⚠️ / 0 ❌ = 48/48 = 100% of the comparable documented items.** This is a documentation-compliance figure only: eight clarification items and the one-PUBLISHED design decision are excluded, external dependencies are unresolved, and the service is **NOT production ready**.

**Backend (service-owned): fixed · Frontend not assessed · Integration BLOCKED — DEPENDENCY · Production readiness NOT READY.** No compliance percentage is asserted.

**Top remaining items by risk:**
1. **Integration:** the registry grant for the forwarded principal and a service-to-service identity (external).
2. **Provisioning:** the actions this service checks are not granted by anything in the repository.
3. Legacy NULL-tenant rows need an operator-assigned owner.

---

## PRV-03 — privacy-decision-svc (port 8153)

> **Correction.** This section previously claimed **100% (34 of 34)**. That figure is withdrawn: the read-only audit scored **66.7% (28/42)**, and the remediation described in Part 1 changed the code. The table below is a fresh comparison of the spec contract with the code **as it is now**; it carries **no percentage**, and items that depend on other services are `BLOCKED — EXTERNAL DEPENDENCY`, never "match". Behaviour is documented in `services/privacy-decision-svc/README.md`.

### 1. Extracted contract (§12-13, §13.1, §13.2, §18, §18.1, §19, §24, §32)

- **§12.1 inputs:** actor/service context; subject context (ref, class, age band, residency/jurisdiction, relationship); data context (category, sensitivity, source, classification); current purpose (activity + purpose/version); proposed operation (12 values); secondary purpose; recipient/destination; consent/preference evidence; policy context (PDC, DRC hold, PRV-05 transfer).
- **§12.2 results:** `PERMIT`, `RESTRICT`, `BLOCK`, `REVIEW_REQUIRED`, `INDETERMINATE`.
- **§13 sequence:** resolve exact activity + purpose → validate context → resolve PDC at effective/knowledge time → resolve notice/consent, retention/hold, transfer → evaluate compatibility/minimization/recipient/sensitive data → return result + evidence id.
- **§13.1 safeguards:** no same-tenant exemption; paid entitlement ≠ permission; public ≠ unrestricted; no model-training/analytics inherited from telemetry unless separately registered and permitted; pseudonymized ≠ anonymous; no anonymous claim without approved de-identification control; RESTRICT constraints machine-enforceable.
- **§13.2 durability:** normalized input fingerprint, exact PRV/PDC versions, consent/notice refs, result, constraints, reason codes, correlation.
- **§18 / §18.1:** response `decision_id, result, constraints, reason_codes, policy_refs, evidence_ref, expires_at/context_hash`; tenant scope, actor, idempotency key and correlation id on mutating APIs; append-only; replay returns the original.
- **§19:** lists no `privacy.decision.*` event. **§32:** `PRV-001`…`PRV-020`. **Invariants:** I13 (unknown never `PERMIT`), I14, I15, I19, I29.
- **The specification does not define:** identifier formats, transport status codes, the idempotency scope, a default `expires_at`, or an authorization permission for these routes.

### 2. Implementation (current)

See the README. Key points: atomic decision + idempotency commit; `(tenant, principal, key)` idempotency scope; `v2:` complete fingerprint; registered-scope enforcement; exact activity-version resolution; fail-closed transfer/hold/consent/policy handling; `TRAIN_MODEL` never auto-permitted; correlation id echoed; no event published; migration `000005` preserves evidence and `000006` adds principal scope.

### 3. Diff table (fresh audit of the remediation targets)

| # | Item | Status | Notes |
|---|---|---|---|
| 1 | Migration 000005 preserves append-only evidence; NOT NULL / strict RLS / FORCE RLS | ✅ Match | Verified on real PostgreSQL, clean DB and existing-data scenario |
| 1b | Legacy NULL-tenant rows | ⛔ BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING | Rows preserved, invisible, warning raised |
| 2 | Decision + idempotency persisted atomically; retry safe; no duplicate evidence | ✅ Match | Fault-injection and concurrency tests |
| 3 | Fingerprint covers all decision-affecting inputs | ✅ Match | Reflection guard |
| 4 | Registered categories / subject classes enforced; sensitivity not downgradable | ✅ Match | Taxonomy is a local copy — needs clarification |
| 5 | `TRAIN_MODEL` never auto-permitted | ✅ Match | |
| 5b | Verified dedicated/human-approved training purpose (PRV-C19, N50) | ⛔ BLOCKED — EXTERNAL DEPENDENCY | purpose-registry / PDC |
| 6 | Hold gate fail-closed; `retention_rule_refs` mapping removed | ✅ Match | |
| 6b | Retention-period conformance (DRC) | ⛔ BLOCKED — EXTERNAL DEPENDENCY | retention-registry |
| 7 | Transfer result handling fails closed | ✅ Match | |
| 8 | Free-text secondary purpose rejected; registered one validated, never auto-permitted | ✅ Match | |
| 8b | Secondary-purpose compatibility evaluation (I14) | ⛔ BLOCKED — EXTERNAL DEPENDENCY | PDC |
| 9 | Exact activity-version resolution | ✅ Match | |
| 9b | Exact historical purpose-version; as-of consent/hold/transfer (I29) | ⛔ BLOCKED — EXTERNAL DEPENDENCY | purpose-registry; consent/retention/transfer |
| 10 | Unknown values fail closed | ✅ Match | Regression table |
| 11 | `expires_at` from upstream evidence, persisted/returned/replayed | ✅ Match | Default TTL — needs clarification |
| 12 | Correlation id echoed (first, replay, GET) | ✅ Match | |
| 13 | Error/status contract documented and tested on every path | ✅ Match | Spec defines no transport codes; framework/server responses documented as outside the envelope; bodies over 1 MiB are `400 VALIDATION_FAILED` |
| 14 | No undocumented decision event | ✅ Match | §19 lists none |
| 15 | Idempotency scope (tenant, principal, key) | ✅ Match | Spec silent — needs clarification |
| 16 | Formats consistent between code and docs | ✅ Match | Spec silent — needs clarification |
| 17 | authorization-svc RBAC on POST/GET | ⚠️ Needs Clarification | Spec names no permission |
| 18 | Service-to-service identity | ⛔ BLOCKED — EXTERNAL DEPENDENCY | Platform |
| 19 | Frontend `X-Correlation-ID` | 🚫 NOT SERVICE-OWNED | |
| 20 | Documentation accuracy | ✅ Match | This update; stale `handler.go:NN` references removed |

**Latest documentation-compliance re-audit (2026-10-21):** the earlier figures above are historical. In the latest audit the service scored 36/37 = 97.3% (one partial: the error-envelope statement); that statement is now accurate, so 37/37 = 100% of the comparable documented items. The 1 MiB body limit is a hardening change, not a scored row. Nine clarification items are excluded, external dependencies are unresolved, and the service is **NOT production ready**.

### 4. Result

- **Service-owned items fixed: 16 of 16 audited gaps addressed; items 5, 6, 8, 9 and the legacy-data part of 1 carry BLOCKED remainders.** No percentage is asserted.
- **Backend service-owned: READY · Frontend not assessed · Integration BLOCKED — DEPENDENCY · Overall production: NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY.**

**Top remaining items by risk:**
1. Service-to-service identity (calls forward the end-user principal; consent-svc and purpose-registry refuse unauthenticated service calls).
2. purpose-registry/PDC capabilities: training purpose verification, compatibility evaluation, exact historical purpose version.
3. Legacy NULL-tenant evidence needs an operator tenant mapping.
4. Retention-period conformance and as-of consent/hold/transfer state.

---

## PRV-04 — privacy-rights-svc (port 8154)

> **Correction.** This section previously scored **100%** and stated events `privacy.rights_request.received` / `.closed`. A later audit scored the service **70.8% (17/24)**; the remediation described in the "Service: privacy-rights-svc" section above addresses its service-owned findings. The earlier events claim is withdrawn (§19 declares none). Behaviour is documented in `services/privacy-rights-svc/README.md`; this section keeps the contract-versus-code view.

### 1. Extracted contract (§14–15, §18, §18.1, §19, §31, §32)

- **§18:** `POST /privacy/rights-requests` → `request_id`, `classification`, `identity_state`, `WFC_process_ref`; `POST …/{id}/discovery-manifests` → `manifest_id`, `domain`, `hash`, `candidate_count`, `evidence_ref`.
- **§14.2:** eight right families. **§15.1:** identity assurance, signed/hashed discovery manifests, third-party redaction, legal-privilege review, legal-hold interaction, response-package binding, late discovery, complaint clock (WFC).
- **§15.2:** *"Finding a record is only discovery. Disclosure requires identity assurance, applicable-right determination, scope review, exemptions, third-party protection, redaction and approved response assembly."*
- **§18.1:** tenant scope, actor, idempotency key and correlation id on every mutating API; evidence append-only. **§19:** no rights intake/closure event; `privacy.rights.action_required` exists. **Invariants:** I17–I21.

### 2. Implementation (current)

Eight routes (× `/v1` alias). Pre-flight on every route: principal → tenant → (mutating) correlation and idempotency key → authorization on the verified tenant → (mutating) replay lookup → validation → resolve. Atomic idempotency (claim, change and stored response in one transaction); strict RLS; closed-case and evidence immutability triggers; the disclosure gate verifies a **verified identity event** and a SHA-256-digested manifest; typed `error_code` envelope; no events.

### 3. Diff table (fresh audit of the remediated service)

| # | Item | Status | Notes |
|---|---|---|---|
| 1 | Routes | ✅ Match | 8 routes × 2 prefixes |
| 2 | Create response fields | ✅ Match | |
| 3 | Manifest response fields | ✅ Match | |
| 4 | Eight right families | ✅ Match | |
| 5 | Disclosure gate: verified identity event + manifest | ✅ Match | Atomic, row-locked |
| 6 | §15.2 elements the service cannot verify | ⛔ BLOCKED — EXTERNAL DEPENDENCY | WFC / reviewers / adapters |
| 7 | Lifecycle | ✅ Match | |
| 8 | Identity evidence (failed attempts recorded, not advancing) | ✅ Match | |
| 9 | Requester vs subject | ✅ Match | |
| 10 | Response package versioning (I21) | ✅ Match | |
| 11 | Closed-case immutability + `409 PRV-020` | ✅ Match | Now also checked under the same lock as the write |
| 12 | Append-only evidence | ✅ Match | |
| 13 | Principal on every route | ✅ Match | Reads included; principal checked before tenant, in the middleware and again in each handler |
| 14 | RBAC | ✅ Match | Read routes reuse `PROCESS` — needs clarification |
| 15 | Correlation id and idempotency key mandatory | ✅ Match | |
| 16 | Idempotency: scope, binding, atomicity, fail-closed | ✅ Match | |
| 17 | Tenant isolation / strict RLS | ✅ Match | Legacy NULL rows: blocked data migration |
| 18 | Read-route access control | ✅ Match | |
| 19 | Events | ✅ Match | None emitted (§19) |
| 20 | Error contract | ✅ Match | `error_code` on every handler/middleware error; chi/HTTP-server responses documented as exceptions |
| 21 | Documentation accuracy | ✅ Match | This update; pre-flight order and error-envelope scope now exact |
| 22 | WFC process reference | ✅ Match | |
| 23 | Authorization client envelope | ✅ Match | New row (documented this pass): identity/envelope headers forwarded as in the sibling services |

**Latest documentation-compliance re-audit (2026-10-21):** before this pass 32 ✅ / 2 ⚠️ / 0 ❌ = 32/34 = 94.1% (rows 20 and 28 partial; the table above numbers the same items differently). Both partials are now ✅ → 34/34 = 100%. With the authorization-client row documented in this pass the comparable set is 35/35 = 100%. This is a documentation-compliance figure only: eight clarification items are excluded, external dependencies are unresolved, and the service is **NOT production ready**.

Needs clarification (not scored): read-route action; owner of `privacy.rights.action_required`; asymmetric manifest signatures; live-container verification (not performed). **No compliance percentage is asserted.**

---

## PRV-05 — privacy-transfer-svc (port 8155)

> **Correction.** This section previously scored **100%**, named the event `privacy.transfer_decision.evaluated`, and carried an "❌ Not Fixed — superuser bypasses RLS" row. A later audit scored the service **63.6% (14/22)**; the remediation described in the "Service: privacy-transfer-svc" section above addresses its service-owned findings. Behaviour is documented in `services/privacy-transfer-svc/README.md`; this section keeps the contract-versus-code view.

### 1. Extracted contract (§16–17, §18, §18.1, §19, §32)

- **§18:** `POST /privacy/transfer-decisions` → `authorization_id`, `result`, `conditions`, mechanism/assessment refs, `expiry`.
- **§16.1 objects:** ProcessorRelationship, Subprocessor, TransferContext, TransferMechanism, TransferAssessment (TIA/risk inputs incl. government-access/technical/organizational measures), TransferAuthorization (`AUTHORIZED | CONDITIONAL | BLOCKED | REVIEW_REQUIRED`).
- **§16.2 (I23):** technical reachability ≠ authorization. **§17.2:** missing/expired/conflicted DPIA/TIA or mechanism approval → `BLOCKED` or `REVIEW_REQUIRED`. **§17.1:** eight mandatory reassessment triggers.
- **§18.1:** tenant scope, actor, idempotency key, correlation id on every mutating API; evidence append-only. **§19/§19.1:** `privacy.processor.changed`, `privacy.transfer.authorization_changed`, `privacy.assessment.expiring`; events communicate committed state and survive a bus outage.

### 2. Implementation (current)

14 logical routes (× `/v1` alias). Pre-flight on every route: tenant (middleware) → principal → (mutating) correlation and key → authorization on the verified tenant → (mutating) replay lookup → validation → resolve. Atomic idempotency; strict RLS; append-only evidence triggers; a single trigger evaluation shared by `EvaluateTransfer` and the trigger endpoint, over the same stored evidence regardless of `assessment_required`; the three §19 events through a transactional outbox; caller principal forwarded to PRV-01.

### 3. Diff table (fresh audit of the remediated service)

| # | Item | Status | Notes |
|---|---|---|---|
| 1 | Routes | ✅ Match | |
| 2 | ProcessorRelationship fields | ✅ Match | |
| 3 | Subprocessor fields | ✅ Match | |
| 4 | Mechanism validity window | ✅ Match | |
| 5 | Fail-closed missing/expired/rejected assessment | ✅ Match | |
| 6 | CONDITIONAL outcome | ✅ Match | |
| 7 | Assessment measures fields | ✅ Match | |
| 8 | Triggers derivable from evidence (1, 3, 4, 6, 8), shared evaluation and evidence, validated declarations | ✅ Match | Fixed 2026-10-21: both paths read the latest assessment whatever `assessment_required` says |
| 8b | Triggers 2, 5, 7 | ⛔ BLOCKED — EXTERNAL DEPENDENCY | Facts originate in other systems; caller-declared only |
| 9 | Idempotency: scope, binding, atomicity, fail-closed | ✅ Match | |
| 10 | §32 reason codes | ✅ Match | |
| 11 | Append-only evidence | ✅ Match | |
| 12 | RBAC on mutating routes incl. `EvaluateTransfer` | ✅ Match | |
| 13 | Principal / correlation / key on mutating routes | ✅ Match | |
| 14 | Tenant isolation / strict RLS | ✅ Match | Legacy NULL rows: blocked data migration |
| 15 | Read-route access control | ✅ Match | Reuse of existing actions — needs clarification |
| 16 | §19 events, transactional | ✅ Match | |
| 17 | Decision sequence documented incl. instruction (PRV-017), subprocessor approval (PRV-018) and triggers | ✅ Match | README |
| 18 | Registry integration | ⛔ BLOCKED — EXTERNAL DEPENDENCY | Principal forwarded; PRV-01 read grant is external |
| 19 | §18 decision fields | ✅ Match | |
| 20 | Documentation accuracy | ✅ Match | This update |

**Latest documentation-compliance re-audit (2026-10-21):** in the latest audit the service scored 35/38 = 92.1% (three partials: shared-evaluation claim, pre-flight order, migration numbering). The first is fixed in code, the other two in documentation, so 38/38 = 100% of the comparable documented items (the earlier figures above are historical). Five clarification items are excluded, external dependencies are unresolved, and the service is **NOT production ready**. Separately, privacy-decision-svc's call to `POST /privacy/transfer-decisions` omits the mandatory `X-Correlation-ID` and `Idempotency-Key` and is refused with 400; that caller-owned defect is outside this service.

Needs clarification (not scored): read-route actions; the DB role in production; live-container verification (not performed). **No compliance percentage is asserted.**

---

## CORRECTIONS ISSUED DURING THIS AUDIT

While reading further into the spec for PRV-04/05, the auditor found **§18 (consolidated API table), §18.1 (idempotency rule), §32 (error-code contract), 60 negative-path scenarios, and 30 invariants** that had not yet been reached when PRV-01/02/03 were first audited — the initial claim that "no endpoint documentation exists" for PRV-02/03 was **incorrect** and is corrected here, verified directly against code:

| Cross-cutting item | Applies to | Verified in code |
|---|---|---|
| **§18.1: idempotency key mandatory on every mutating API** | All 5 services | **Absent in all 5** — grepped for `Idempotency` in every handler/store package, zero hits anywhere |
| **PRV-N04: replayed consent must return original receipt, not duplicate** | PRV-02 | **Historical finding, since fixed** — at the time of this audit pass, no unique constraint existed on `(subject_ref, purpose_id, action)` and a replayed consent submission created a duplicate legal-evidence row. ✅ Fixed and live-verified in the PRV-02 section's Gap 4 — deduplication now queries the existing unwithdrawn receipt and returns it without inserting a duplicate row. |
| **PRV-N05: replayed withdrawal must be idempotent** | PRV-02 | **Historical finding, since fixed** — at the time of this audit pass this was ⚠️ Partial (409 rejection instead of returning the original withdrawal). ✅ Fixed and live-verified in the PRV-02 section's Gap 5 — a replayed withdrawal now returns the original withdrawal receipt with 200 OK. |
| **§32 error-code contract (PRV-001 through PRV-020)** | All 5 services | Only PRV-01 and PRV-02 use it, and only for 2 of 20 codes (`PRV-001`, `PRV-004`). **PRV-01 also has one confirmed code-misuse**: `runStructuralValidation` emits `PRV-019` (documented meaning: `PRIVACY_CONTEXT_INDETERMINATE`) for plain "required field missing" checks on `subject_classes`/`data_categories` — the wrong code for that condition. PRV-03/04/05 use **no numbered codes at all**, only custom strings. |

These don't change the compliance percentages materially (they were already reflected in the gap lists above as partial items), but they add one **new, concrete, previously-unflagged defect** (the PRV-019 code misuse in PRV-01) and elevate the idempotency gap from a suspected omission to a **confirmed, domain-wide, spec-violating gap affecting all 5 services**.

---

## PART 2 — PRIVACY DOMAIN FINAL SUMMARY (Documentation Compliance Audit)

| Service | Compliance % | Top risk |
|---|---|---|
| privacy-purpose-registry-svc (PRV-01) | No % asserted (earlier ≈ 90.6% superseded; domain audit 87.8%) | **Correction:** the earlier 100% was unsupported. Service-owned gaps fixed (incl. atomic idempotency); remaining: reference existence (PDC/DRC/PRV-05), provisioning of the new actions, frontend. See the PRV-01 sections. **NOT READY.** |
| privacy-consent-svc (PRV-02) | No % asserted (earlier ≈ 93.9% withdrawn; pre-remediation domain audit 75.0%) | **Correction:** the earlier 100% was unsupported. Service-owned gaps fixed (authentication-before-validation ordering, atomic principal-scoped idempotency, §18 presentation `receipt_id`, principal forwarded to the registry); remaining: registry grant and service-to-service identity, provisioning, legacy NULL-tenant rows. See the PRV-02 sections. **NOT READY.** |
| privacy-decision-svc (PRV-03) | No % asserted (earlier 100% / 34-of-34 withdrawn; pre-remediation audit 66.7%) | **Correction:** the earlier 100% / Ready for Production was unsupported. Service-owned gaps are fixed; remaining risk is external: service-to-service identity, purpose-registry/PDC capabilities (training-purpose verification, secondary-purpose compatibility, exact historical purpose version), retention/as-of state, legacy NULL-tenant rows needing a tenant mapping, frontend correlation header. See the PRV-03 sections. **NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY.** |
| privacy-rights-svc (PRV-04) | No % asserted (earlier 100% withdrawn; pre-remediation audit 70.8%) | **Correction:** the earlier 100% / Ready for Production was unsupported. Service-owned gaps fixed (strict RLS, authenticated reads, atomic idempotency, verified §15.2 gate, typed errors); remaining: authorization-svc grants, WFC/reviewer-owned disclosure elements, legacy NULL-tenant rows. See the PRV-04 sections. **NOT READY.** |
| privacy-transfer-svc (PRV-05) | No % asserted (earlier 100% withdrawn; pre-remediation audit 63.6%) | **Correction:** the earlier 100% was unsupported. Service-owned gaps fixed (strict RLS, authenticated reads, atomic idempotency, shared trigger evaluation, transactional §19 events, principal forwarded to PRV-01); remaining: PRV-01 read grant in authorization-svc, systems originating triggers 2/5/7, legacy NULL-tenant rows. See the PRV-05 sections. **NOT READY.** |

**Domain average: ~68%**

**Domain-wide risk ranking (auth/security → data integrity → functional → API/schema → cosmetic):**
1. **(Auth/security)** No idempotency-key enforcement anywhere — a network retry or double-click on any mutating call in any of the 5 services can silently create duplicate legal evidence (grants, withdrawals, rights requests, transfer decisions) — confirmed live against explicit negative-path test scenarios (PRV-N04/N05) the spec itself defines.
2. **(Auth/security)** No Segregation-of-Duties / self-approval checks anywhere in the domain — the same principal who creates/submits a record can also approve/publish it in PRV-01 and PRV-02. **Correction (PRV-02):** this is stale for privacy-consent-svc specifically — notice approval/publishing enforces maker-checker SoD (Gap 2: maker cannot self-approve or self-publish, 403 Forbidden), live-verified.
3. ~~**(Data integrity)** privacy-rights-svc has no append-only/immutability trigger at all — the odd one out among 5 services that otherwise consistently enforce this at the DB layer.~~ **Correction:** stale — `privacy-rights-svc` has 3 enforced triggers (`rights_requests_closed_immutable`, `identity_verification_events_append_only`, `discovery_manifests_append_only`, migration 000002), reconfirmed live and installed via `pg_trigger` in the PRV-04 re-audit. This service is not the odd one out.
4. ~~**(Data integrity)** privacy-consent-svc has zero proxy/representative modeling despite an explicitly named requirement.~~ **Correction:** stale — proxy/authorized-representative handling is fully implemented (schema, validation, UI) and verified (PRV-02 Gap 3).
5. ~~**(Functional)** privacy-decision-svc never checks transfer authorization (PRV-05) despite it being a named input dimension — two "runtime decision" services exist in this platform and don't cross-check each other.~~ **Correction:** stale — `privacy-decision-svc` does check transfer authorization via a real HTTP call to `privacy-transfer-svc:8155` (§13 step 4 / PRV-05 integration). Live-verified: the call returns a populated `transfer_decision_id` matching the corresponding record in `privacy-transfer-svc`'s own database, and CONDITIONAL transfer decisions correctly surface as `RESTRICT`. See the PRV-03 section's Gap 2.
6. **(Functional)** Three of five services' Approve/Reject-equivalent or reassessment logic silently substitutes simpler self-contained gates for the spec's WFC-orchestrated or trigger-automated designs — all honestly self-documented in code, not hidden, but still real deviations.
7. **(API/schema)** §32's 20-code error contract is used correctly in ~2 places, misused once, and ignored everywhere else — every service invented its own string vocabulary instead.
8. **(Cosmetic)** RESTRICT/REVIEW_REQUIRED outcomes reserved-but-unreachable across PRV-01/03, uniformly because no PDC (jurisdiction rules) service exists anywhere in the platform. **Correction:** PRV-05's `CONDITIONAL` outcome is reachable and verified (see the PRV-05 section's Gap 1 and diff table) — the earlier blanket claim that CONDITIONAL was unreachable across PRV-01/03/05 was stale and is corrected here; only PRV-01/03's RESTRICT/REVIEW_REQUIRED remain in that state.

**Needs clarification (not scored as pass or fail):** notice "effective window" semantics; whether activity-level lawful-basis is meant to be independent of purpose-level; whether privacy-consent-svc or privacy-decision-svc owns enforcing consent-withdrawal's "blocks future processing" invariant; identity-assurance "signed" manifest requirement (hash vs. cryptographic signature); exact auth model since the spec never names concrete roles/permissions to check code against.

---

*End of Privacy domain audit file. Generated from a read-only audit — no application code was modified in producing these results.*

---

# Privacy Domain Audit — Second Pass
**Date:** 2026-10-21
**Scope:** Privacy services, ports 8151–8155

## Overall Result

**Overall compliance: 98.6% on the service-level audits (204 of 207 comparable items), unchanged from the last pass.**

The domain-level section of `docs/audit_files/PRIVACY.md` remains stale at 1 of 7. The five Privacy services are **not production ready**.

This was a **read-only audit**. No code or documentation was changed during the audit. One agent checked the services one after another. For each service, `go vet`, `go build`, and the full test suite were re-run against the throwaway PostgreSQL database `pcs-test-pg`, and the results were compared with the README. Every handler line was not re-read; the contract and implementation extraction used the per-service audits completed earlier.

Since the previous pass, `git status` showed one new functional change: the **decision → transfer client fix**.

## Per-Service Results

| Service | Port | Tests (with DB) | Tests (no DB) | README says | Vet / build | Compliance | Open partials |
|---|---:|---:|---:|---:|---|---:|---:|
| privacy-purpose-registry-svc | 8151 | 95 pass | 95 pass (falls back to the local DB) | 95 | clean / clean | **46/49 = 93.9%** | 3 external |
| privacy-consent-svc | 8152 | 147 pass | 109 pass, 38 skip | 147 | clean / clean | **48/48 = 100%** | 0 |
| privacy-decision-svc | 8153 | 78 pass | 62 pass, 16 skip | 78 | clean / clean | **37/37 = 100%** | 0 |
| privacy-rights-svc | 8154 | 66 pass | 58 pass, 8 skip | 66 | clean / clean | **35/35 = 100%** | 0 |
| privacy-transfer-svc | 8155 | 62 pass | 53 pass, 9 skip | 62 | clean / clean | **38/38 = 100%** | 0 |
| **Total** | | | | | | **204/207 = 98.6%** | **3** |

### Service-Level Findings

- **privacy-purpose-registry-svc:** The three partials are the same documented external items:
  - submit has no WFC hand-off;
  - gate 4 checks the PDC reference for presence only;
  - gate 6 checks the DRC/PRV-05 reference for presence only.

- **privacy-consent-svc:** No open service-level partials.

- **privacy-decision-svc:** No open service-level partials.

- **privacy-rights-svc:** No open service-level partials.

- **privacy-transfer-svc:** No open service-level partials.

## Changes Since the Previous Pass

- Decision's README now documents the call to transfer.
- Decision's test count increased from 70 to 78.
- The **decision → transfer HTTP 400 integration defect is fixed**.
- The decision client now sends:
  - `X-Tenant-Id`
  - `X-Principal-Id`
  - `X-Correlation-ID`
  - deterministic `Idempotency-Key`
- Four matching header references are present in the client.
- The new header set was checked against the real transfer handler: the old header set returned 400, while the new header set returned 200, with replay returning the same decision.
- README test counts for all five Privacy services now match the code.

## Routes

Route counts remain unchanged:

- Registry: 26
- Consent: 17
- Decision: 2 × `/v1`
- Rights: 8 × `/v1`
- Transfer: 14 × `/v1`

## Error Envelope

- Registry's 404, 405, panic, and `/readyz` responses use `{error, error_code}`.
- The other four services document those framework responses as outside the application error envelope.

## Domain-Level Documentation Status

The following stale items remain in `PRIVACY.md`:

| # | Item | Status |
|---|---|---|
| D1 | Per-service sections (counts, README references, migrations) | ✅ Match, except decision, below |
| D2 | Part 1 summary table and averages ("Services Ready: 1") | ⚠️ stale |
| D3 | Key evidence #2: "no test files exist… DB layer only exercised via a stub" | ⚠️ stale; all five have real-PostgreSQL tests |
| D4 | Key evidence #3: authorization decision cache "mirrored across all 5" `authz/client.go` files | ⚠️ inaccurate; only registry and consent cache |
| D5 | Key evidence #9 and methodology note: "none of the 5 services has its own README" | ⚠️ stale; all five have one |
| D6 | Part 2 summary: "Domain average ~68%" and the risk ranking | ⚠️ stale |
| D7 | PRV-03 section still says 70 top-level tests and does not mention the transfer-call fix | ⚠️ stale; code and README say 78 |

## Top Gaps — Ranked by Risk

### Security and Authorization

1. **privacy-decision-svc has no `authorization-svc` check on either route.** Any caller with a principal header and tenant header can create and read decisions. The specification names no permission, so this is excluded from the compliance score. A permission needs to be defined in `authorization-svc`.

2. **Identity headers are accepted as supplied in all five services.** None verifies a signature on `X-Principal-Id` or `X-Tenant-Id`. The documentation treats this as the gateway's responsibility, and the gateway was not verified during this audit.

3. **Grants are not provisioned anywhere in this repository.** No service-to-service identity is defined, so cross-service calls forward the end-user principal. Nothing was verified against a live `authorization-svc`.

4. Consent's `GET /notices/{id}` takes no principal and only requires a tenant. This is documented and intentional.

### Data Integrity

5. Consent does not enforce at most one `PUBLISHED` version per notice. A probe produced two published versions in 24 of 25 concurrent-publish rounds. The specification does not require this, so it remains a design decision.

6. Rights discards the `reason` sent on close, does not format-check `response_evidence_hash`, and has no reopen or supplement path. All three require clarification.

7. Registry gates 4 and 6 accept references that may not exist, and submit does not go through WFC.

8. Transfer triggers 2, 5, and 7 can only be declared by the caller.

9. Legacy NULL-tenant rows in consent, decision, rights, and transfer are preserved but invisible. They require an operator tenant mapping.

### Cosmetic / Documentation

- D2–D7 above.
- Shared migration prefixes in transfer are documented and harmless.
- Decision returns 400 for a missing tenant while the other services return 401.
- Stray `.exe` files exist in the rights directory.

## Decision → Transfer Integration

The decision → transfer integration defect identified in the previous audit is **no longer open**.

The corrected client sends the required tenant, principal, correlation, and deterministic idempotency headers. The real transfer handler accepts the request with the corrected header set, returning 200, and replay returns the same decision.

## Needs Clarification — Excluded From Scores

The following clarification items remain unchanged from the earlier per-service audits.

### Registry

- "latest version" for the canonical routes
- §32 gate code mapping
- suspended `as_of` response
- frontend status

### Consent

- invalid `as_of`
- duplicate withdrawal evidence
- preference `source`
- presentation-receipt fields
- default `affirmative_action_type`
- `invalidate` caller
- purpose-change carry-forward
- OD-06

### Decision

- `authorization-svc` permission
- default `expires_at`
- identifier formats
- idempotency principal scope
- taxonomy drift
- preference evidence
- analytics safeguard
- actor and service context
- secondary-purpose version
- missing-tenant status

### Rights

- `wfc_process_ref` on create
- proxy authority evidence
- late-discovered data and re-versioning
- response-package binding
- close `reason`
- manifest signatures
- who emits `privacy.rights.action_required`
- read-route action

### Transfer

- read-route actions
- taxonomy drift
- mechanism types
- activity-reference requirements
- `privacy.processor.changed` payload

## Verdict

All five Privacy services match their own documentation closely, and the full suites pass.

The only service-level partials are the Registry's three external dependencies. The **decision → transfer integration, which was the main functional blocker in the previous pass, now works**.

The Privacy domain is **not production ready**. The remaining production blockers are external/dependency-level concerns:

- missing authorization check and grants;
- WFC dependency;
- PDC and DRC dependencies;
- tenant mapping for legacy NULL-tenant rows;
- no end-to-end validation on the live stack.

The domain-level `PRIVACY.md` summary is stale and should be refreshed separately as a documentation-only task.

**Audit classification:** Second-pass read-only audit
**Audit date:** 2026-10-21
**Code changes made by this audit:** None
**Documentation changes made by this audit:** None during audit; this section is being appended afterward as the audit record.
**Production readiness:** ❌ Not Ready
**Service-level compliance:** **204/207 = 98.6%**
**Open service-level partials:** **3 external**
**Decision → Transfer integration:** ✅ Fixed
**Domain documentation:** ⚠️ Stale
**End-to-end live-stack validation:** ❌ Not completed
