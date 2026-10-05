# Group 1 — Identity, Scope & Foundation: documentation-compliance audit

**Date:** 23 September 2026 (identity-context-svc re-audited 28 September 2026; tenant-entity-registry-svc re-audited live 29 September 2026; configuration-feature-flag-svc remediated 29 September 2026; secret-vault-integration-svc remediated 29 September 2026; gateway-auth-svc remediated and re-audited 29 September 2026; search-indexer-svc remediated, re-audited live and remediated again 30 September 2026; notification-svc remediated and verified live 30 September 2026, re-audited and six further defects fixed 5 October 2026)
**Scope:** all nine services of Group 1, audited one at a time.
**Source of truth:** the original `.docx` specifications in `docs/architecture/`, **not** the
per-service `openapi.yaml` / `asyncapi.yaml` artefacts. This is deliberate: auditing code
against a contract written alongside it cannot catch a case where both encode the same wrong
assumption.

**Method (per service):** extract the documented contract (endpoints, schemas, status codes,
auth, business rules, side effects, events) → extract the implemented contract from the code →
diff the two → report. Anything ambiguous in the documents is flagged **needs clarification
(❓)** rather than scored as a pass or a fail.

**Legend:** ✅ match · ⚠️ partial · ❌ missing · ❓ needs clarification

| # | Service | Port | Spec audited against | Full match | Weighted |
|---|---|---|---|---|---|
| 1 | identity-context-svc | 8080 | GOV-01 §4 | **95%** (was 73%) | **97%** (was 82%) |
| 2 | tenant-entity-registry-svc | 8081 | ORG-02 + ORG-03 | **98%** (was 75%) | **99%** (was 80%) |
| 3 | configuration-feature-flag-svc | 8086 | ZS-SVC-AA-001 (CFG-01…05) | **96%** (was 12%) | **98%** (was 19%) |
| 4 | secret-vault-integration-svc | 8087 | Security Standard §13, §9, §3.1 | **86%** (was 40%) | **93%** (was 52%) |
| 5 | gateway-auth-svc | 8092 | Security §8 + GOV-01 ingress | **95%** (was 68%; 82% at re-audit) | **98%** (was 80%) |
| 6 | search-indexer-svc | 8096 | ZS-SVC-AB-001 (ESR-01…05) | **96%** (was 86% recounted; published 90%) | **98%** (was 90%; published 93%) |
| 7 | notification-svc | 8133 | ZS-SVC-Y-001 (NCD-01…05) | **95%** (was 12%; 81% on 5 Oct re-audit of 43 rows, 95% after fixes) | **97%** (was 19%; 98% on 43 rows after fixes) |
| 8 | delegated-authority-svc | 8136 | ORG-06 (delegation half) | **100%** (23 table rows; was 62% published, 48% recounted) | **100%** (was 69%; 57% recounted) |
| 9 | access-control-svc | 8137 | Authorization Standard §9 | 18% | 23% |

---

# 1/9 — identity-context-svc (:8080) vs GOV-01, Governance & Control Plane spec (§4)

**Contract extracted from:** `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx`
(document ID **ZS-SVC-A-001**) §4 — contract table, Engineering Interaction Wireframe, Minimum
Negative-Path Acceptance — plus §1–§2 invariants.
**Code:** `services/identity-context-svc/`.

> **Status as of 28 September 2026 — CLOSED within service scope.** Re-audited against the same
> spec after the 23 Sep gap closure (commit `7c993c3`) and a second remediation pass on 28 Sep
> (migration `000009_source_input_provenance`). Every item this service can close on its own is
> closed and verified; the two ❓ items are ruled. What remains open depends on other services
> — see *Remaining cross-service dependencies* below. Each row keeps the original 23 Sep finding
> so the history stays readable.

## Named operation surface

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| ResolveTenantContext | QUERY, read-only, scoped, correlation required | `POST /v1/context/resolve` → 200 / 400 / 401 / 403 / 503 | ✅ | **FIXED 2026-09-23.** *Was:* classified a material write by the envelope middleware, so it demanded `Idempotency-Key` for an operation the spec calls a query. Now declassified in `cmd/server/main.go` and exempt from the replay store (recording its response would store a working credential) |
| GetEffectiveContext | QUERY | `GET /v1/context/session/{id}` → 200 `{envelope_jwt}` / 401 / 403 / 404 | ✅ | |
| ExplainContextResolution | QUERY | `GET /v1/context/session/{id}/explain` → 200 / 400 / 401 / 403 / 404 / 503 | ✅ | Supports `?as_of=`. Since 28 Sep also returns a `source_inputs` section (channel, workload, causation, ingress version, cache state, entitlement status — each with how it was established) |
| RefreshTenantContextCache | COMMAND | `POST /v1/context/cache/refresh` → 200 / 501 when unwired | ✅ | |
| InvalidateTenantContext | COMMAND | `POST /v1/context/tenant/invalidate` → 200 | ✅ | |
| AttachSupportContext (privileged) | COMMAND | `POST /v1/context/support` → 201 / 400 / 409 / 503 | ✅ | Authorized + SoD self-approval refusal + TTL ceiling (4h max, 1h default) |
| Path shape `/internal/v1/gov01/...` | "Illustrative contract surface" | `/v1/...` | ✅ | **RULED 2026-09-23: illustrative, not binding.** The spec column is headed "Illustrative contract surface"; `/internal/v1/gov01/` is a naming convention for the control plane, not a requirement on any one service. Current `/v1/...` paths are compliant and the operation names are preserved in OpenAPI `operationId`s where a contract test checks them |

## Mandatory controls

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Idempotency-Key on commands | "Idempotency-Key + expected_version where stateful" | `internal/idempotency` middleware + `idempotency_keys` table (migration 000008) | ✅ | **FIXED 2026-09-23, hardened 2026-09-28.** *Was:* no dedupe store; `ErrCodeIdempotencyMismatch` returned by nothing; a replayed `POST /v1/context/support` minted a second break-glass grant. Now a retry replays the first answer (`X-Idempotent-Replay: true`) and a different body gets `409 IDEMPOTENCY_MISMATCH`. 28 Sep: replay bound to the principal (defect R2) and stranded claims recovered (defect R3). **Verified live:** retry replayed, one grant created |
| expected_version | "where stateful" | absent everywhere | ✅ | **RULED 2026-09-23: not applicable to GOV-01.** The clause is conditional on statefulness. Sessions are append-only, cache refresh and tenant invalidate are idempotent by nature, and the only mutable object — the support context — is already guarded against double-revoke and double-review by its own append-only `revoked_at` / `reviewed_at` columns. Nothing here has a lost-update to protect |
| Correlation required (queries) | required | `requireCorrelation` on both read queries → 400 | ✅ | **FIXED 2026-09-23.** The envelope parses correlation but its default write-strict mode admits reads, so a bare `GET …/explain` succeeded. Now enforced in the handler. No consumer broke: the console's server-side hop goes through `apiGet` → `envelopeHeaders`, which always sets the header |
| Scoped (queries) | required | `requireTenant` + `requirePrincipal` → 401 | ✅ | |

## Server-resolved context & source inputs

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| tenant_id server-resolved | Invariant 2 — client hint never authoritative | Bound to `claims.TenantID` from the verified token; `X-Tenant-Id` ignored | ✅ | The spoof test decodes the JWT rather than asserting a refusal — correct test for an implementation that *ignores* rather than *rejects* |
| legal-entity scope / environment / residency ref / session assurance | required | present on `SessionContext` | ✅ | |
| **entitlement context reference** | listed as server-resolved; "commercial entitlement read model" listed as a dependency | `entitlement_context_ref` + `entitlement_context_status` recorded on every decision; `EntitlementResolver` seam | ❌ | **BLOCKED on COM-03 — service side done 2026-09-28.** The owning service is `ZoikoSuite_Commercial_Platform_Billing_Subscription_Entitlement_Detailed_Service_Specifications_v1_0.docx` §4.3 **COM-03 Entitlement** (`EntitlementSnapshot`; `GET effective-entitlements` / `POST /entitlements:evaluate`), unimplemented estate-wide. Every decision now records `UPSTREAM_NOT_CONFIGURED`, so "never resolved" is distinct from "resolved to nothing"; wiring COM-03 is one `WithEntitlementResolver` call. Non-blocking by design — login must not depend on the commercial plane. Deliberately no HTTP client: its API does not exist to code against |
| source channel | listed as *server-resolved* | Resolved against the verified principal's type; basis recorded (`source_channel_basis`) | ⚠️ | **Service side FIXED 2026-09-28; remainder cross-service.** *Was:* client's `X-Source-Channel` recorded verbatim. Now an assertion the principal type cannot make (a human claiming `scheduled_job` / `system`, a service account claiming `web`) is **not recorded** — §4 permits "preserve UNKNOWN state" — and machine principals get a server-derived channel. **Verified live:** a spoofed human request returned 200 with the channel recorded as unknown / `REJECTED_INCONSISTENT`. Still partial because web vs mobile for a human can only be known at the edge: **gateway-auth-svc must stamp the channel** |
| workload identity | listed as a required source input | Server-resolved: the verified principal for service accounts / API clients; basis recorded (`workload_id_basis`) | ✅ | **FIXED 2026-09-28.** *Was:* parsed from `X-Workload-Id`, unrecorded (000008 then recorded it verbatim). A machine principal authenticates *as* its workload, so its verified id is recorded and a header naming another workload is marked `REJECTED_INCONSISTENT`. A workload id on a human session cannot be attested here and is discarded (`UNVERIFIABLE_DISCARDED`). A DB CHECK forbids storing a discarded value. **Verified live** |
| Cache state model Fresh / Stale / Invalidated | tri-state | `domain.CacheFreshness`, enforced at ingress, recorded as `ingress_cache_state` | ✅ | **FIXED 2026-09-28.** *Was:* the states were computed and discarded, and `WithBindingTTL` was never called, so STALE was unreachable in any deployment. Now `INGRESS_BINDING_TTL_SECONDS` (default 24h — labels, never refuses; INVALIDATED is still refused under strict) and the state is on every decision and in explain. **Verified live:** a 2-day-old binding recorded `STALE`, version `registry-v7` |

## Evidence, events, negative paths

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| 3 named events | TenantContextCacheInvalidated / TenantContextResolutionFailed / SupportContextAttached | `identity.context.cache_invalidated`, `identity.context.resolution_failed`, `identity.support_context.attached` | ✅ | All via transactional outbox — invariant 10 holds. Since 28 Sep also `identity.support_context.reviewed` (defect R4) |
| Evidence incl. **causation** ID | "correlation/causation IDs" | `causation_id` persisted on every decision and shown by explain | ✅ | **FIXED 2026-09-23** (column, migration 000008); read back into explain 2026-09-28. *Was:* parsed by the envelope, never persisted |
| Evidence incl. policy/version refs | required | residency policy id + schema_version + **`ingress_binding_version`** | ✅ | **FIXED 2026-09-23.** The ingress binding's `source_version` was read on every resolution and discarded, so a decision could be replayed but not reproduced — you could see the ingress matched, not what it matched against. `IngressChecker.Evaluate` now returns it and the resolver records it |
| NP1 spoofed tenant header | ignored/rejected | ignored | ✅ | |
| NP2 unknown hostname no fallback | deny | `internal/context/ingress.go` refuses, no fallback tenant | ✅ | |
| **NP3 support context expires automatically** | required | `Resolver.verifySupportContext` → `SupportService.Verify` before any session is attributed | ✅ | **FIXED 2026-09-23.** *Was:* `X-Support-Context-Id` stamped on the session with no existence, tenant, revocation or expiry check. **Re-verified 2026-09-28 over HTTP:** fictional, expired, revoked, other-tenant and other-engineer grants → 401 `BREAK_GLASS_REQUIRED` / `BREAK_GLASS_EXPIRED` with no session saved; a live grant → 200 |
| NP4 invalidation drops stale privilege | required | session + tenant invalidation paths; event-driven revocation | ✅ | Manual paths passed on 23 Sep. **The event-driven half was broken and is FIXED 2026-09-28** — see defect R1 |
| Break-glass "followed by reconciliation/review" | §1 refinement + §2 invariant 8 | `RunReconciler` goroutine (cross-tenant, `SUPPORT_REVIEW_INTERVAL_MINUTES`, default 60) + gauge + alert; `POST /v1/context/support/{id}/review` | ✅ | **FIXED 2026-09-23, completed 2026-09-28.** *Was:* the documented reconciler goroutine did not exist and `MarkReviewed` had no route. Now the sweep sets `identity_context_support_contexts_unreviewed`, alerted in `deployments/prometheus-rules.yml`; the review route refuses the grantee and the approver (403 `SOD_CONFLICT`). 28 Sep: the review now emits its own event (defect R4) |
| support_context_id propagated to consumers | elevation must be attributable | `support_context_id` claim on `IdentityContextEnvelope` (omitted on ordinary sessions) | ✅ | **FIXED 2026-09-23.** *Was:* not a claim, so downstream services could not tell a support-elevated session from an ordinary one |

## Defects found on re-audit (28 September 2026) — all fixed

None of these were among the 44 scored items; each was a real hole the 23 Sep audit did not
see. All were verified against the running service, not only in unit tests.

| # | Defect | Fix | Verified |
|---|---|---|---|
| **R1** | **Event-driven session revocation never fired.** The Kafka reader subscribed only to `zoiko.identity.events`, but `authority.revoked` / `authority.expired` are published on `zoiko.delegated-authority.events`, `role.updated` on `zoiko.access-control.events` and `entity.updated` on `zoiko.entity.events`. Revoking a delegation, retiring a role or changing an entity ended nobody's session. Wider than the role-only finding under service 9/9 | `KAFKA_CONSUME_TOPICS` (default the four producer topics) via kafka-go `GroupTopics` + `WatchPartitionChanges`; compose and k8s updated. **Migration hazard handled:** subscribing the existing group starts the new topics at the first offset, which would replay every historic revocation against today's sessions, so revocations older than the envelope TTL + 1 min are skipped (no live session can predate them; legal holds exempt). Also removed an `actor_id` fallback that would have revoked the *revoker's* sessions. `asyncapi.yaml` corrected; a contract test holds its channels equal to the reader's topics | **Live, real Kafka:** a day-old `authority.revoked` was skipped; a fresh one ended 3 sessions (`DELEGATION_REVOKED`); `entity.updated` ended 4. `role.updated` rides the same reader and payload key but was not driven live |
| **R2** | **Idempotent replay skipped authorization.** Replays are answered before the handler, and the key was scoped to tenant + endpoint + body only, so a second principal with the first's key and body received the first principal's stored response (justification included) | The verified principal (`X-Principal-Id`) is folded into the request fingerprint → another principal gets `409 IDEMPOTENCY_MISMATCH`, which reveals nothing. No migration needed | **Live:** Alice's retry replayed (`X-Idempotent-Replay: true`); Mallory with the same key and body got 409 and no stored body |
| **R3** | **A crash or panic locked a key for 7 days.** A panicking handler never released its claim (Recoverer sits outside the middleware), and a dead process released nothing, so every retry got `IDEMPOTENCY_IN_FLIGHT` until the retention purge | Panic → claim released, then re-panicked for Recoverer. Crash → an identical request takes over a claim older than a 5-minute lease, atomically in the claiming `INSERT … ON CONFLICT` | **Live:** a planted 10-min-old claim was taken over and the command ran (201); a fresh claim still got 409. Plus 2 tests on Postgres 16 |
| **R4** | **A break-glass review left no audit event.** Attach and revoke emitted events; the review — the step that closes the loop — wrote two columns only, so the governance stream showed every elevation as permanently unreviewed | `identity.support_context.reviewed`, written atomically with the review through the outbox; a repeat review writes nothing and the first reviewer stands | **Live:** approver refused (403); third-party review → 204 and exactly one event, delivered to Kafka; a second review → still one event |

## Rulings on the undocumented surface (28 September 2026)

- **GOV-09 retention / disposition worker inside GOV-01 — RULED: keep.** It disposes only this
  service's own session evidence and honours GOV-10 legal holds. Moving it to GOV-09 would mean
  GOV-09 reaching into identity-context-svc's database, breaking data ownership.
- **Routes outside GOV-01 §4 — RULED: accepted local extensions.** `POST /v1/authenticate` (a
  local argon2 credential store standing in for the external identity provider the spec's
  "authenticated subject" assumes), `POST /v1/context/session/{id}/invalidate`, `GET` +
  `DELETE /v1/context/support/{id}`, the four `/v1/principals/*` routes and
  `GET /.well-known/jwks.json`. All are in `openapi.yaml`, and a contract test refuses any route
  that is not. `/v1/authenticate` is the console's only login path until an identity provider
  (OIDC/SAML) is connected; at that point the token check moves to that provider's JWKS and the
  local credential store is removed.

## Compliance

| | Full match | Weighted (partials at ½) | Needs clarification |
|---|---|---|---|
| 23 Sep 2026 (original audit) | 32 / 44 — **73%** | **82%** | 2 |
| **28 Sep 2026 (re-audit)** | **42 / 44 — 95%** | **97%** | **0** (both ruled) |

The two items short of full are both cross-service: **entitlement context reference** (❌,
blocked on COM-03) and **source channel** (⚠️, web vs mobile needs the edge). Neither can be
closed inside this service.

**Verification behind the 28 Sep figures:** all 17 Go packages pass, `go vet` clean; 59 store
tests against a throwaway Postgres 16; migration 000009 applied up → down → up; live runs
against the compose stack (only the containers this service needs) covering login, resolve,
explain, support attach / replay / review, and event-driven revocation over real Kafka.

**Deploy note:** apply migration `000009_source_input_provenance` **before** rolling out a
binary built after 28 Sep. It is additive and nullable, so the old binary is unaffected, but the
new binary writes its columns and would fail every resolve without it. Step documented in
`MANUAL-TEST-GUIDE.md`.

**Remaining cross-service dependencies**

1. **COM-03 Entitlement** does not exist — blocks the entitlement context reference.
2. **gateway-auth-svc** should stamp `X-Source-Channel` and strip client-supplied
   `X-Support-Context-Id`, `X-Workload-Id`, `X-Purpose-Context`, `X-Approval-Reference` and
   `X-Evidence-Refs` (group finding 1). This service now verifies or resolves the two it
   consumes, so it is safe here, but the headers are still self-asserted estate-wide.
3. **No service grants the six `IDENTITY_*` actions**, so the protected routes answer 403
   against a real authorization-svc until roles are provisioned. Estate-wide: authorization-svc
   seeds no action names for any service.
4. **Ingress bindings are never refreshed** from the tenant registry, so every binding will
   honestly read `STALE` once past the TTL.
5. **SoD service** is unset in compose, so SoD runs on the permit-all stand-in locally (refused
   in staging and production).

**Original top gaps by risk (23 Sep) — all closed:** support context accepted unverified ✅ ·
Idempotency-Key never honoured ✅ · break-glass reconciliation never ran ✅ · elevation invisible
downstream ✅ · entitlement reference missing ❌ blocked on COM-03, service side done ·
correlation / source channel / workload identity ✅ (source channel ⚠️ pending the edge).

---

# 2/9 — tenant-entity-registry-svc (:8081) vs ORG-02 (Tenant) + ORG-03 (Legal Entity)

**Audited:** 29 September 2026, live.
**Contract extracted from:**
`ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`
§4.2, §4.3 and their mandatory-engineering-controls blocks. §3 (shared contract),
§8 (negative paths) and §9.2 (Definition of Done) are also scored.
**Code:** `services/tenant-entity-registry-svc/`.

**How it was audited.** The service ran on :8081 as `zoiko_app`, rebuilt from
current source, with no dev compatibility flags set. It used a real
authorization-svc and a real jurisdiction-rules-svc. A temporary maker and checker
were granted in the platform scope and in a freshly provisioned tenant's scope,
then revoked (0 left). Every ✅ marked "live" below was exercised by HTTP
against the running service, not inferred from the code.

## Completion

| Measure | First audit (23 Sep) | **Now (29 Sep)** |
|---|---|---|
| ORG-02 + ORG-03, 48 items — fully met | 36 — 75% | **47 — 98%** |
| ORG-02 + ORG-03 — weighted (partial = ½) | 80% | **99%** |
| Partial / missing / needs clarification | ≈5 / ≈3 / 4 | **1 / 0 / 0** |
| Whole document (§3, §4.2, §4.3, §8, §9.2), 69 items — fully met | not scored | **66 — 96%** |
| Whole document — weighted | not scored | **98%** |
| Items this service can satisfy on its own | — | **100%** |
| `scripts/audit.sh` live checks | 37 | **68/68** |
| Live end-to-end flow with real grants | — | **81/81** |
| Tests (`go test ./...`) | — | **259, 0 fail** |

The one partial item, and the two partial §9.2 gates, are blocked on things
outside this service (see "Open — outside this service").

## ORG-02 — Tenant (25 items)

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | Write ownership: Tenant, lifecycle, home-region ref, default-config ref | ✅ | Home region is the default residency policy's region |
| 2 | Non-ownership respected (billing, authz, residency decision) | ✅ | |
| 3 | Required inputs: legal/business name, primary jurisdiction, locale/timezone, defaults, residency preference, onboarding evidence | ✅ live | Missing onboarding key → 422; missing jurisdiction → 400 `VALIDATION_FAILED`; lineage persisted (000013) |
| 4 | Server-resolved context: regions, plan entitlement, uniqueness, onboarding policy, restricted jurisdictions | ✅ live | Unknown jurisdiction and unknown region → 400 `REFERENCE_RETIRED`; residency preference becomes the home region at birth; restricted list and plan entitlement checked (fail closed) |
| 5 | 7 named commands | ✅ live | Create, Activate, Suspend, Resume, InitiateTermination, CompleteTermination, ChangeDefaultLocale |
| 6 | 4 read surfaces | ✅ live | GetTenant, ResolveTenantByHost, ListTenantLifecycleHistory, GetTenantDefaults |
| 7 | Lifecycle incl. FailedProvisioning | ✅ | FAILED_PROVISIONING state; tests |
| 8 | Platform provisioning permission | ✅ live | Provisioning outside the platform scope → 403 `AUTHORIZATION_DENIED` |
| 9 | Tenant admin cannot change hard isolation identifiers | ✅ | Identifiers enumerated in SPEC_DEVIATIONS.md |
| 10 | Maker-checker on creation | ✅ live | Activate before approval → refused; maker self-approval → 403 `SOD_DENIED`; wrong fingerprint → 409; checker → 200; second approval → 409 |
| 11 | Maker-checker on termination | ✅ live | 202 + approval request; checker rejects; tenant stays ACTIVE |
| 12 | Maker-checker on home-region change | ✅ live | `ChangeHomeRegion` 202; maker self-approval → `SOD_DENIED`; checker → region moved |
| 13 | 5 named events | ✅ live | Every event of the run delivered to Kafka, no retries |
| 14 | Create by onboarding key | ✅ live | `Idempotency-Key` replay → same tenant; same key, different body → 409 `IDEMPOTENCY_MISMATCH` |
| 15 | Lifecycle commands use expected_version | ✅ live | Missing → 400; stale → 409 `VERSION_CONFLICT` |
| 16 | FailedProvisioning + compensating cleanup | ✅ | RetryProvisioning; AbandonProvisioning under maker-checker; tests |
| 17 | Evidence: onboarding request, approval, home-region decision, config version, actor/reason | ✅ live | Decision reference on lineage; the database refuses a home-region change without it (`tlh_home_region_evidenced`) |
| 18 | Acceptance: cross-tenant resolution impossible | ✅ live | Host of tenant A claiming tenant B → 403 `CONTEXT_INVALID`; cross-tenant read refused |
| 19 | Acceptance: suspended tenant denied | ✅ live | Write → 409 `INVALID_TRANSITION`; read → 200 |
| 20 | Acceptance: partially provisioned tenant not active | ✅ live | Activation refused until creation is approved |
| 21 | Acceptance: termination preserves records | ✅ | No hard-delete path |
| 22 | Control: protected changes need current version + validated scope | ✅ live | |
| 23 | Control: historical versions retrievable | ✅ live | Lifecycle history |
| 24 | Control: events carry stable object/version identity | ✅ live | `object_id`, `object_version`, `effective_at`, `recorded_at` on every event of the run |
| 25 | Control: evidence, actor, reason, approval chain on high-risk changes | ✅ live | |

## ORG-03 — Legal Entity (23 items)

| # | Item | Status | Evidence |
|---|---|---|---|
| 26 | Write ownership: entity, profile version, legal form, formation | ✅ | |
| 27 | Required inputs: legal name, entity type/legal form, incorporation jurisdiction, registry number/date, registered address, currency, fiscal calendar, supporting evidence | ✅ live | Missing registered address → 400 `VALIDATION_FAILED`; missing evidence → 400 `SOURCE_UNVERIFIED`; all recorded on profile version 1 |
| 28 | Server-resolved: jurisdiction validity, ISO 20275 mapping, duplicates, calendar/currency validity | ⚠️ | Jurisdiction (retired refused), ELF format + source + local text, and duplicates all live. **The fiscal calendar is format-checked only: REF-04 Fiscal Calendar does not exist in the estate** |
| 29 | 6 named commands incl. MergeDuplicateCandidate | ✅ live | Merge non-destructive (the duplicate still exists); unmerge restores |
| 30 | 4 read surfaces | ✅ live | GetLegalEntity, FindByRegistryNumber, ListEntityVersions, GetLegalEntityAsOf |
| 31 | Draft → Verified → Active → Inactive/Dissolved | ✅ live | Created as DRAFT; activate a DRAFT → refused; verification without evidence → 400; verified → ACTIVE |
| 32 | Profile versions effective-dated | ✅ live | No entity has two open-ended versions |
| 33 | Master administration permission | ✅ live | |
| 34 | Sensitive identifier access scoped | ✅ live | RESTRICTED bundle: no purpose → 403; with purpose → 200 |
| 35 | Material changes independently approved | ✅ live | |
| 36 | SoD: legal-name / registry / jurisdiction | ✅ live | Rename 202 → checker approves |
| 37 | SoD: no self-approval of merge (and of conflict resolution) | ✅ live | Maker self-approval → 403 `SOD_DENIED` for both |
| 38 | 4 named events | ✅ live | |
| 39 | Registry + jurisdiction is a dedup signal, not an identifier | ✅ | |
| 40 | Commands use UUID and expected_version | ✅ live | As item 15; a malformed id → 400 `VALIDATION_FAILED` |
| 41 | Conflicting registry identity quarantined | ✅ live | 409 `DUPLICATE_CANDIDATE`; conflict row; resolved once, under maker-checker |
| 42 | Historical profile never overwritten | ✅ live | |
| 43 | Evidence: source refs, verified fields, legal-form code/source, times, approver | ✅ live | |
| 44 | Acceptance: collision quarantined, history preserved, changes approved, as-of exact | ✅ live | As-of before a rename → original name; as-of now = GetEntity |
| 45 | Control: identity changes effective-dated and evidence-backed | ✅ live | |
| 46 | Control: LEI with source/status, not a key | ✅ live | Bad check digits → 400; valid → approval → stored with source and status |
| 47 | Control: ISO 20275 / ELF preserving local text | ✅ live | Invalid code → 400; normalised code, source and local text stored at creation and on amendment |
| 48 | Control: dissolution deletes nothing | ✅ | |

**ORG-02 + ORG-03: 47 of 48 fully met — 98%** (partials at half: **99%**).

## Rest of the document

| Section | Applicable | ✅ | ⚠️ | ❌ | Notes |
|---|---|---|---|---|---|
| §3 shared contract | 10 | 10 | 0 | 0 | Typed `error_code` on every error (malformed ids included), idempotent replay, event versions, purpose limitation |
| §8 negative paths NP3–NP6 | 4 | 4 | 0 | 0 | All four live |
| §9.2 Definition of Done | 7 | 5 | 2 | 0 | ✅ gates 1 (OpenAPI validated, breaking-change gate), 2 (outbox only), 3, 4, 8 (govulncheck clean). ⚠️ gates 6 and 7 — outside this service. Gate 5 not applicable |

**Whole document: 66 of 69 fully met — 96%; weighted 98%.**

## Gaps fixed since the first audit — all ✅

| Gap | Fix | Status |
|---|---|---|
| Maker-checker was nominal: the approver was a free string in the maker's own body | Server-side `approval_requests`; a different principal approves with the `payload_fingerprint` they reviewed; self-approval → `SOD_DENIED` | ✅ live |
| No maker-checker on tenant creation | Creation answers 202; activation is refused until approved | ✅ live |
| No home-region change command or maker-checker | `ChangeHomeRegion` (000012), platform authority, decision evidence enforced by a constraint | ✅ live |
| Legal entities had no Draft / Verified states | DRAFT → VERIFIED → ACTIVE with evidence-gated verification | ✅ live |
| Conflict resolution had no SoD | Maker-checker; maker self-approval → 403 | ✅ live |
| No onboarding-key idempotency | `external_customer_key` required; `Idempotency-Key` honoured on every write (000010) | ✅ live |
| No LEI | LEI with source and status, check digits validated, changes approved | ✅ live |
| No FailedProvisioning state | FAILED_PROVISIONING with retry and compensating abandon | ✅ |
| `expected_version` optional | Required on every protected command (000011) | ✅ live |
| Primary jurisdiction and residency not inputs; no entitlement or restricted-jurisdiction check | Required inputs (000013); restricted list, region availability and plan entitlement resolved server-side | ✅ live |
| No stable typed errors | `error_code` on every error body | ✅ live |
| A malformed id answered 500 on every route that takes one | `VALIDATION_FAILED` (400), mapped once at the store's RLS boundary | ✅ live |
| CreateEntity discarded the legal form, registered address and evidence (an invalid ELF code was accepted) | Accepted, validated and recorded on profile version 1; registered address and evidence required | ✅ live |
| Events lacked object version and recorded time; some bypassed the outbox | §7 envelope on every event; every event goes through the transactional outbox | ✅ live |
| Tenant creation answered 500; provisioning authorized in the caller's scope | New tenant scoped to itself; provisioning authorized in the platform scope | ✅ live |
| Backdated or same-instant amendments left overlapping versions or answered 500 | Interval rules in the store; as-of now always equals GetEntity | ✅ live |
| NP3 enforced on 2 of 46 routes | `/v1` middleware on every route | ✅ live |
| Evidence missing the onboarding request and home-region decision | Both persisted on tenant lineage | ✅ live |
| Four items needing clarification | Resolved in `SPEC_DEVIATIONS.md` (non-destructive merge, hard isolation identifiers, sensitive identifiers, routes outside ORG-02/03) | ✅ |
| No runbook, SLOs, alerts, restore drill, contract validation | `RUNBOOK.md`, `SLO.md`, six alert rules, restore drill 22/22, kin-openapi validation, oasdiff gate | ✅ |

Each fix has a regression test. The full list of defects is in the service's
`RELEASE_CERTIFICATE.md`.

## Open — outside this service

None of these needs a change in this service's code.

| Item | Why it is not ✅ | Owner of the fix |
|---|---|---|
| 28 — fiscal-calendar reference validity (§4.3) | REF-04 Fiscal Calendar does not exist; the id is format-checked, and there is nothing to resolve it against | REF-04 service (not built) |
| §9.2 gate 6 — dependents consume pinned versions | Every event carries `object_id`/`object_version`; each consuming service must persist them. identity-context-svc verified | each consuming service |
| §9.2 gate 7 — production certification | Runbook, SLOs, six alert rules and a 22/22 restore drill exist; RPO/RTO must be measured on the production database | platform / operations |

## Proof

| Check | Result |
|---|---|
| `scripts/audit.sh` (grant-free, live) | 68/68 |
| Live end-to-end flow (real grants, seeded and revoked) | 81/81 |
| `go build` / `go vet` | clean |
| `go test ./...` | 259 tests, 0 fail (store suite on a throwaway Postgres 16) |
| `scripts/backup_restore_drill.sh` | 22/22, restore 2 s |
| `scripts/contract_gate.sh` | 16 breaking changes since 24 Sep, all deliberate (SPEC_DEVIATIONS.md) |
| `govulncheck ./...` | 0 reachable vulnerabilities |

**Deployment assumption.** NP3 compares the request's `Host`. The ingress must
preserve the client's `Host`; a proxy that rewrites it to the service name would
silently disable NP3 (RUNBOOK §5.2).

**Frontend.** The console must adopt the 16 contract changes listed in
SPEC_DEVIATIONS.md. That work is separate and not scored here.

---

# 3/9 — configuration-feature-flag-svc (:8086) vs ZS-SVC-AA-001

**Contract extracted from:**
`ZS-SVC-AA-001_Platform_Configuration_Feature_Flag_Environment_Change_Control_Detailed_Service_Specifications_v1.0.docx`
— §0.1 canonical services, §3 (30 invariants), §10.1 (11 canonical APIs), §10.2 (8 events),
§14 (30 negative paths).
**Code:** `services/configuration-feature-flag-svc/`.

> **Status as of 29 September 2026 — CLOSED within service scope: 47 of 49 scored items met, 2
> partial pending other services.** Remediated in two passes on 29 Sep against the same spec. Each
> row keeps its 23 Sep finding as *Was:* so the history stays readable. Migrations now run to
> `000013`. Verified by **169 passing Go tests** (store suite against real Postgres, run in
> `golang:1.25-alpine`; it had never run green before), including a full governed-lifecycle test
> under a `NOSUPERUSER NOBYPASSRLS` role. Not yet deployed: the local `configuration_feature_flag`
> database is still at `000003`, and `scripts/audit.sh` (updated for 25 routes) has not been re-run
> against the live stack.

**Framing, kept from the baseline because it explains the starting number.** AA-001 specifies a
control plane of **five canonical services** — CFG-01 schema/version registry, CFG-02
resolution/override, CFG-03 flag/release/experiment, CFG-04 environment/secret-reference/promotion,
CFG-05 change/rollback/drift/emergency. On 23 Sep the service was a single effective-dated
key→value store with 6 routes and 1 event, built against `03-microservices.md §9.6`. The AA-001
surface was then implemented inside this one service (24–28 Sep, `b26c663`+) — but, as the next
section shows, it had never actually run.

## Defects found beyond the audit (fixed 2026-09-29)

The 24–28 Sep AA-001 work was present in the code and had never worked end to end. None of these
appear in the 23 Sep tables, because the audit predates the code they live in.

| Defect | Effect | Fix |
|---|---|---|
| Migrations `000006` / `000008` put `COALESCE(...)` (and a `WHERE`) inside a table `UNIQUE` constraint | Neither could apply on a clean database. `init-db.sh` runs with `ON_ERROR_STOP=1`, so a fresh stack init **aborted for every database in the stack**, not just this one; the store suite fataled before its first assertion | Rewritten as `CREATE UNIQUE INDEX` with the same names |
| `mintSnapshot` passed `$3` to `md5($3::text)` and to the `jsonb` column | Postgres typed `$3` as text; **every write that mints answered 503** | Explicit `$3::jsonb` |
| Value supersedes (`UpsertConfigEntry` / `UpsertFeatureFlag`) committed without minting | A changed value was recorded and reported saved **while every read kept serving the old one** — reads are snapshot-served | One `mintAndPublish` helper on every write path |
| Kill switch and release plan writes never minted | A kill switch or a release plan (targeting, schedule, variants) **never took effect**, while `flag.kill_switch.activated` / `flag.release.activated` announced that it had | Mint in the same transaction |
| `sweepEmergency` issued an `UPDATE` while its cursor was open | pgx `conn busy`: every sweep that found an expired emergency change failed | Read all due rows, then write |
| `config_entries`, `feature_flags`, `event_outbox` policies did not admit the `app.ops_sweep` escape the change/emergency paths run under | Under the real runtime role **every tenant-scoped change activation, tenant break-glass activation and revert answered 503**. Invisible to the suite, which connects as a superuser | Migration `000012`; new `aa001_approle_test.go` drives the lifecycle as a non-superuser |
| Bucketing salt defaulted to the flag's row id, which changes on every write | Raising a rollout 30%→60% **took the feature away from 246 of 606 users** who had it | Salt by flag key |
| Approve / activate / activate-emergency authorized `CONFIGURATION_WRITE` whatever the target | A tenant-scoped writer could **approve and activate a global (even C3) change** or activate a global break-glass change | Authorized by the target's scope (`TargetScope`) |
| `CreateChange` did not check each part's environment | A staging change — possibly needing no approval — could write production values | Parts must match the change's environment |
| `expected_before_hash` accepted and never checked | A change approved against one value silently overwrote another | Enforced at activation (`drift_detected`) |
| Store test teardown dropped only the original three tables | Only the first store test could ever pass | Drops every table and function by name |

## Canonical APIs (§10.1)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /config/definitions` | Create draft ConfigDefinition; schema, owner, scope and type validation; idempotency key | `POST /v1/config/definitions` | ✅ | **FIXED 2026-09-29.** Type, safety class, scopes, fallback, sensitivity validated; owner now enforced in the store, not only the handler; S2/S3 keys cannot declare `USER_PREFERENCE` (NP-07); optional `allowed_regions` (INV-26). *Was:* ❌ no definition registry; keys free-form |
| `POST /config/definitions/{id}/publish` | Publish immutable version; approval binding for S2/S3; digest/signature; no in-place edit | `POST /v1/config/definitions/{key}/publish` | ✅ | **FIXED 2026-09-29.** Immutable digested version; publishing an S2/S3 key requires `approval_reference` (body or `X-Approval-Reference`), stored on the version (`000011`) — else 409 `approval_reference_required`. Signing remains OD-03. *Was:* ❌ |
| `PUT /config/overrides/{scope}` | Effective-dated override set; scope allowlist; tenant isolation; impact/change classification | `PUT /v1/config/overrides/{scope}` | ✅ | **FIXED 2026-09-29.** All five layers (`environment`, `service`, `tenant`, `org_unit`, `user_preference`); allowlist enforced; tenancy fixed per layer; a user preference only by that user. Change classification: an S2/S3 key takes **no direct write** on any path (409 `material_key_requires_change`) — it changes through an approved change set. *Was:* ⚠️ `POST /v1/config`, two layers, no allowlist, no classification |
| `POST /config/resolve` | Resolve keys for trusted context; deterministic precedence; snapshot lineage | `POST /v1/config/resolve` | ✅ | **FIXED 2026-09-29.** Snapshot-pinned; five-layer precedence from gateway-supplied identities; lineage (snapshot id, epoch, digest, layer, reason) plus `context_hash`, `resolved_at`, `stale`; every requested key answered (`UNKNOWN_KEY` / `NO_VALUE` / `SAFE_DEFAULT` / `RESIDENCY` / `STALE_SNAPSHOT`). *Was:* ⚠️ one key, no lineage |
| `GET /config/snapshots/{id}` | Fetch immutable signed snapshot | `GET /v1/config/snapshots/{snapshot_id}` | ✅ | **FIXED 2026-09-29.** Platform scope only (a snapshot spans every tenant); `ETag` = digest, `Cache-Control: private, immutable`; snapshots append-only by trigger for every role (`000009`). Signing remains OD-03. *Was:* ❌ zero occurrences of "snapshot" |
| `POST /flags` | Create draft flag: class, owner, variants, fallback, expiry for temporary flags | Flag declarations via `POST /v1/config/definitions` (`flag_class`, `retirement_deadline`); values via `POST /v1/flags`; variants via release plans | ✅ | **FIXED 2026-09-24/29.** Temporary classes require a retirement deadline; an S2/S3 key cannot be an `EXPERIMENT` flag (INV-19). *Was:* ⚠️ `{enabled, rollout_percentage}` only |
| `POST /flags/{id}/release-plans` | Rollout/targeting plan; eligibility first; deterministic buckets | `POST /v1/flags/{key}/release-plans` + `POST /v1/flags/{key}/evaluate` | ✅ | **FIXED 2026-09-29.** Plans now take effect (they never minted); eligibility before percentage; deterministic, monotonic buckets (salted by key); unknown targeting attributes refused (INV-18); S2/S3 flags all-or-nothing, no variants (INV-19); plan eligibility reads the gateway's `X-Commercial-Plan`, never the body (NP-08). *Was:* ❌ `rollout_percentage` stored, never evaluated |
| `POST /changes` | Atomic ChangeSet with before/after, validation, approvals, rollout and rollback | `POST /v1/config/changes` (+ `/approve`, `/rollback`) | ✅ | **FIXED 2026-09-29.** Parts validated at proposal; class must cover each key (S2→C2, S3→C3); C2/C3 always approval-gated; parts cannot cross environments; before-snapshot pinned (an empty environment gets an empty imprint, so a material key's first value is settable); parts may target the extended layers; rollback computed from the before-snapshot. *Was:* ❌ |
| `POST /changes/{id}/activate` | Effective time, authorization, idempotency, monotonic version | `POST /v1/config/changes/{change_id}/activate` | ✅ | **FIXED 2026-09-29.** Refused before `planned_effective_at` (409 `change_not_yet_effective`); authorized by the change's scope; `expected_before_hash` enforced; monotonic epoch; `APPLYING` until the attesting fleet converges, then `VERIFIED` (NP-25). *Was:* ❌ |
| `POST /emergency-changes` | Break-glass change; restricted actors/keys, TTL, incident reference, retrospective required | `POST /v1/emergency-changes` (+ `/activate`, `/retrospective`) | ⚠️ | **FIXED 2026-09-29 except restricted actors.** S2/S3 keys only (403 `emergency_scope_denied`); expiry and incident mandatory; activates once, from OPEN, inside its window; expiry reverts exactly (only if still current) and opens a retrospective closed only with a review reference (`000010`); authorized by the target's scope. **Open:** restricting *who* may break glass needs a dedicated grant seeded in authorization-svc. *Was:* ❌ |
| `POST /runtime/attest` | Report observed snapshot/version/hash; workload identity; anti-replay; drift correlation | `POST /v1/runtime/attest` | ✅ | **FIXED 2026-09-29.** `runtime_id` must equal the gateway's `X-Workload-Id` (401/403); single-use attest key; observation classified against the current snapshot and recorded/emitted as drift. *Was:* ❌ |

**11 of 11 implemented; 10 fully, 1 partial pending authorization-svc.** *Was:* 0 of 11, 3 partial.

## Events (§10.2)

All eight are ✅ **emitted** through the transactional outbox: `config.version.published`,
`config.override.activated`, `config.snapshot.published`, `flag.release.activated`,
`flag.kill_switch.activated`, `config.change.verified` (now on verification, not activation),
`config.drift.detected`, `config.emergency.expired`. **FIXED 2026-09-24/29.** *Was:* all eight ❌
missing; only the undocumented `config.updated` was emitted (it still is, for existing consumers).

## Invariants (§3) — 30 scored

| Status | Count | Invariants |
|---|---|---|
| ✅ | 29 | INV-01, INV-03, INV-04, INV-14, INV-25, INV-30 (met at baseline) · **fixed:** INV-05 unknown keys refused on every write path and answered `UNKNOWN_KEY` on resolve · INV-06 type/owner/scopes/fallback/safety class mandatory in the store · INV-07 five layers, fixed precedence (user preference > org unit > tenant > service > environment, `000013`) · INV-08 allowlist on write and on read · INV-09 secret material refused on **every** key · INV-10 references bound to their environment (`secret://<env>/<path>`) · INV-11 no copy path, change sets cannot cross environments, a lower environment cannot reference a production store · INV-12 runtime reads served from immutable snapshots only · INV-13 stale snapshots withhold S2/S3 values; sweep refreshes before the deadline · INV-15 exact reversion at expiry + mandatory retrospective · INV-16 kill switch routed, predefined safe behaviours only, takes effect and lifts via mint · INV-17 rollback restores the before-state through the governed path · INV-18 closed targeting-attribute set · INV-19 no randomization for S2/S3 · INV-20 temporary flags need owner and retirement deadline · INV-21 retirement tombstones until verified removal · INV-22 monotonic epochs; older snapshots flagged as drift · INV-23 drift by exact epoch/digest · INV-24 detection never mutates configuration · INV-26 residency evaluated before delivery · INV-27 epoch, digest and freshness on every read · INV-28 declared safe defaults served as `SAFE_DEFAULT` · INV-29 immutable, fetchable snapshots + complete evaluation evidence |
| ⚠️ | 1 | **INV-02** — CFG side done: plan eligibility no longer trusts a caller-asserted plan and reads `X-Commercial-Plan` only. The entitlement itself must come from the commercial account service via the gateway |

*Was:* ✅ 6 · ⚠️ 4 (INV-02, 07, 09, 28) · ❌ 20.

## Negative-path matrix (§14)

The scenarios the audit named are now satisfied and each has a test: **NP-01** (tenant cannot
override a PLATFORM_ONLY key — no direct write to material keys, and the allowlist refuses the
tenant scope in a change set), **NP-02** (unknown key refused on write, answered `UNKNOWN_KEY` on
resolve), **NP-03** (boolean key refuses a string), **NP-05** (racing writers leave one current row;
the schema refuses a second), **NP-06** (unchanged: RLS), **NP-07** (S2/S3 cannot declare or take a
user preference), **NP-19** (temporary flag needs owner and deadline), **NP-20** (retired key
tombstoned until verified removal). Also now covered: NP-08 (client-asserted plan), NP-13/14/15
(no randomized targeting of material outcomes), NP-22/23/24/26 (staleness, older snapshot,
digest mismatch, silent runtime), NP-25 (not `VERIFIED` before convergence). *Was:* only NP-06
satisfiable.

## What the service does well

Kept from the baseline and still true: fail-closed authorization (503 on an authz outage),
gateway-verified tenant scope with 403 on a foreign `tenant_id`, append-only history with partial
unique indexes, 409 (not 503) on a lost first-write race, transactional outbox. Added: every read is
snapshot-pinned and reproducible; every write path mints in its own transaction; refusals are coded
and documented (`openapi.yaml` validated with `openapi-spec-validator`; every emitted code
documented).

## Compliance

**47 of 49 scored items fully met — 96%** (partials at half: **98%**). No items need clarification.
*Was:* 6 of 49 — 12% (19%).

**Top gaps by risk** — all six closed within service scope on 2026-09-29:

1. ~~**INV-12 violated structurally**~~ — **FIXED.** Reads are snapshot-served; snapshots are
   append-only for every role (`000009`) and fetchable by id; evaluation evidence carries snapshot
   identity, context hash, time and variant. A past decision replays from its evidence (tested
   after the value changed).
2. ~~**No key declaration**~~ — **FIXED.** Registry enforced on every write and read path; NP-01/02/03/05/07 each tested.
3. ~~**No kill switch, rollback, or emergency change**~~ — **FIXED.** Kill switch routed and
   effective; rollback from the before-snapshot; emergency changes revert at expiry and owe a
   retrospective. Restricted *actors* for break-glass remain open (authorization-svc).
4. ~~**No drift detection or runtime attestation**~~ — **FIXED.** Attestation classifies drift;
   the sweep flags stale and silent runtimes; findings recorded and emitted.
5. ~~**`rollout_percentage` never evaluated**~~ — **FIXED.** Server-side, deterministic and monotonic.
6. ~~**No secret-reference or promotion boundary**~~ — **FIXED.** Environment-bound references,
   material refused on every key, change sets confined to one environment.

## Remaining cross-service dependencies

1. **Gateway / edge** must set, and strip client-sent values of, the headers this service now
   trusts: `X-Workload-Id` (attestation, SERVICE layer), `X-Commercial-Plan` (plan eligibility),
   `X-Org-Unit-Id` (ORG_UNIT layer), `X-Jurisdiction-Context` (residency). Until then they are
   client-controlled — see cross-service finding 1 below.
2. **Commercial account service (COM)** must supply `X-Commercial-Plan` — closes INV-02.
3. **authorization-svc** must seed a dedicated break-glass grant — closes §10.1 restricted actors.
4. **Secret store** resolution of `secret://<env>/<path>` references (no service resolves them yet).

**Decisions taken where the spec is silent** (recorded, revisit when the spec names a value):
retrospective due 7 days after expiry; drift convergence window 15 minutes; snapshot refresh 12 h
before the 24 h freshness deadline (OD-04); "material" = safety class S2/S3; fixed layer
precedence as listed.

---

# 4/9 — secret-vault-integration-svc (:8087) vs the Security / Privacy / Cryptographic Architecture Standard

**Contract extracted from:**
`ZoikoSuite_Security_Privacy_Residency_Cryptographic_Architecture_Standard_Detailed_Engineering_Wireframe.docx`
§3.1 (25 SEC invariants), §5 classification, §9 workload identity, §13 secrets & credential
management, §16 secure logging.
**Code:** `services/secret-vault-integration-svc/`.

> **Status as of 29 September 2026 — CLOSED within service scope: 18 of 21 scored controls met,
> 3 partial pending other services or a decision.** Remediated on 29 Sep against the same
> standard. Each row keeps its 23 Sep finding as *Was:* so the history stays readable.
>
> Commit `2305df1` had already added inbound TLS, a rotation sweeper, break-glass retrieval, an
> exception register and signed lease tokens after the audit. **Every one of those still failed
> the negative path this audit names** — see *Defects found beyond the audit*. They were fixed,
> not just re-scored.
>
> Verified by **188 passing Go tests, 0 failed, 0 skipped**. The store suite ran against a real
> Postgres 16 in `golang:1.25-alpine`; it had only ever passed its first test (see defects). The
> SQL changes were also run by hand against the real migrations.
>
> **Not yet committed or deployed.** `scripts/audit.sh` has not been re-run against the live
> stack. Production needs the infrastructure listed under *Before production*.

**Caveat on the source of truth (kept from the baseline):** §13 is seven lines of prose. The
`.docx` defines **no API surface** for a secret broker — no named commands, no events, no
negative-path matrix. So the routes cannot be scored as "documented" or "undocumented" against
the `.docx`; the service is audited against the **controls** the `.docx` does mandate.

**Scoring note:** the 23 Sep compliance line said "10 of 25 scored", but its own controls table
has 21 rows (7 ✅, 6 ⚠️, 6 ❌, 2 ❓). That figure could not be reproduced, so this re-audit scores
the table's rows. On that basis the 23 Sep baseline was **7 of 19 — 37% (50% weighted)**.

## Implemented surface

17 routes. The original 12: `POST /v1/secret-policies` · `GET /v1/secret-policies` ·
`POST …/{id}/versions` · `POST …/{id}/versions/{vid}/activate` · `GET …/{id}/versions` ·
`POST …/{id}/rotate` · `POST …/{id}/material` · `POST /v1/secrets/broker` ·
`GET /v1/secrets/leases/{id}` · `GET /v1/secrets/leases` · `POST /v1/secrets/leases/{id}/revoke` ·
`GET /v1/secrets/audit`.

Added since the audit: `POST /v1/secrets/leases/{id}/verify` (token redemption) ·
`POST …/{id}/emergency-retrieval` (break-glass) · `POST /v1/shared-secret-exceptions` ·
`GET /v1/shared-secret-exceptions` · `POST /v1/shared-secret-exceptions/{id}/revoke`.
`openapi.yaml` documents all 17 and validates with `openapi-spec-validator`.

## Defects found beyond the audit (fixed 2026-09-29)

Present in the code after `2305df1`, and each one reopened the gap it was meant to close.

| Defect | Effect | Fix |
|---|---|---|
| Inbound certificate check read `X-Workload-Id` first; the broker authorizes `X-Principal-Id` first | A workload's **own valid certificate** proved `X-Workload-Id: svc-a` while the broker issued a lease to `X-Principal-Id: svc-b`. Gap 1 was still exploitable | The certificate must name **every** identity header present |
| Server-only TLS satisfied the production gate; `MTLS_IDENTITY_CHECK` defaulted off | Encrypted, but any caller that reached the port was accepted | Production/staging refuse to boot without mTLS **and** the identity check |
| Lease token bound only `{secret_path, expires_at}` | A **revoked** lease's token answered `valid: true` when presented with any other live lease on the same path | Token `ltk:v3` binds the lease's `request_id`; `v2` refused |
| A replayed broker call minted a token with the replay's expiry | The token outlived its lease by the retry delay | Replays re-mint against the original lease expiry |
| Sweeper request id embedded `time.Now()`; nothing claimed the row | Two replicas (or one slow pass) rotated the same secret twice, mass-revoking leases twice | Compare-and-swap claim on `next_rotation_at`; request id derived from the due slot |
| Exception query: `tenant_id IS NULL OR tenant_id = $1` joined unparenthesised with `AND status … AND secret_path …` | **Every global exception came back regardless of status or path**. A revoked exception for another secret unlocked break-glass on this one | Parenthesised; the handler re-checks path, status and expiry |
| Break-glass wrote its evidence **after** releasing material, and only logged a failure | A store error released raw material with no record it had left | Evidence first; `503 evidence_unavailable` and nothing released |
| Break-glass `reason` optional; the exception's approver could use it | "Privileged workflow and evidence" could be completed by one person with no stated reason | `reason` required; `403 exception_self_approval` |
| Lease ceiling checked only when a version was created | A version stored before the ceiling was lowered kept issuing its old duration | Clamped at issue time; `0` (no ceiling) refused in production |
| Outbound mTLS provisioning sent no `X-Mtls-Bootstrap-Token` and no envelope | mtls-management-svc refused every call, so `AUTHZ_MTLS_ENABLED=true` could never boot. No config ever enabled it, so nothing noticed | Token (re-read per renewal) and full envelope, matching authorization-svc |
| Store test setup dropped only the 4 tables from `000001` | Only the first store test could ever pass: the rest failed re-migrating `000005` ("already exists") | Drops every table the migrations create |

## Controls audit

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| No secrets in logs / DB / events (§13, SEC-INV-09, §16) | mandatory | Postgres holds metadata only; material lives behind the vault backend; the broker returns a lease token, never a value | ✅ | Unchanged. Break-glass is the one documented exception, gated below |
| Data classification (§5) | PUBLIC / INTERNAL / CONFIDENTIAL / RESTRICTED | validated on policy creation, 400 otherwise | ✅ | Unchanged |
| Deny by default (SEC-INV-01) | mandatory | admin routes and all reads call authorization-svc; the broker denies by absence of policy | ✅ | Unchanged |
| Authoritative tenant binding (SEC-INV-02) | mandatory | gateway-verified tenant; a body claiming a foreign tenant → 403 | ✅ | Unchanged |
| Fail closed (SEC-INV-14) | mandatory | authz, vault, **KMS** and store outages → 503 | ✅ | KMS unreachable at boot fails the boot |
| Evidence for material access (SEC-INV-15) | mandatory | REQUESTED / GRANTED / DENIED rows + events | ✅ | Break-glass evidence is now written before release |
| **Authenticated transport, unique workload identity (SEC-INV-05, §9)** | "Service-to-service calls use unique workload identity and authenticated transport" | `ListenAndServeTLS` with `RequireAndVerifyClientCert`; mandatory in production/staging (`ValidateInbound`) | ✅ | **FIXED 2026-09-29.** Server-only TLS refused. `internal/config` tests. *Was:* ❌ plain HTTP, no `ClientAuth` |
| **Receiving service revalidates forwarded context (§9)** | mandatory | body identity must equal the verified actor (403 `workload_identity_mismatch`); the certificate must name every identity header; only a listed forwarder (`MTLS_TRUSTED_FORWARDERS`) may forward another principal | ✅ | **FIXED 2026-09-29.** `inboundmtls` tests, including the svc-a/svc-b case that passed before the fix. *Was:* ❌ broker authorized `requested_by_principal_id` from the body |
| **Master key isolated from application workloads (SEC-INV-07)** | mandatory | `VAULT_KEK_PROVIDER=transit\|gcpkms`: envelope encryption, one data key per secret, wrapped by a KMS the service never reads the master key from; separate wrapped lease-signing key; in-process key refused in production | ✅ | **FIXED 2026-09-29.** `internal/vault/envelope.go`, `kek.go`; migration off the legacy store via `VAULT_MIGRATE_LEGACY_KEY_FILE`. Tests: store unreadable under another KMS key, KMS outage fails closed, record moved to another path fails. *Was:* ❌ AES key handed to the process as hex config |
| Privileged access JIT / time-bound / attributable (SEC-INV-12) | mandatory | signed `ltk:v3` token bound to path, lease and expiry; `POST …/verify` checks signature, expiry, lease binding and register status | ✅ | **FIXED 2026-09-29.** A revoked lease's token no longer verifies. *Was:* ⚠️ bookkeeping only; token never redeemed |
| Lease duration ceiling | "time-bound" | `MAX_LEASE_DURATION_SECONDS` (default 24h) at creation **and** at issue; `0` refused in production | ✅ | **FIXED 2026-09-29.** *Was:* ⚠️ no platform maximum |
| No implicit authorization from network position (SEC-INV-04) | mandatory | identity is certificate-bound and revalidated; the allowlist is evaluated against that identity | ✅ | **FIXED 2026-09-29** by the two §9 rows above. The broker still deliberately has no separate RBAC check (`TestBroker_NotRBACGated`). *Was:* ⚠️ reachability ≈ authorization |
| Automated rotation (§13) | "Rotation must be automated where provider capability permits" | `rotation_interval_seconds` per version; sweeper on by default (15 min); one claim per due slot | ✅ | **FIXED 2026-09-29.** Unit test + store test against Postgres. *Was:* ❌ `POST …/rotate` only |
| Emergency secret retrieval (§13) | "requires privileged workflow and evidence" | `POST …/emergency-retrieval`: platform-scoped action; ACTIVE exception for exactly this path; approver ≠ retriever; reason required; evidence before release | ✅ | **FIXED 2026-09-29.** *Was:* ❌ zero occurrences of emergency / break-glass |
| Shared production secrets need documented exception (§13) | mandatory | exception register (reason, evidence reference, expiry, revocable); broker refuses a secret with >1 allowed workload without an active exception (`REQUIRE_SHARED_SECRET_EXCEPTION`, on in production/staging) | ✅ | **FIXED 2026-09-29.** *Was:* ❌ no exception register |
| Dynamic credentials preferred over static (§13) | "Prefer identity federation and dynamic credentials" | static material; the broker leases a pointer to it | ⚠️ | **OPEN — O-3 below.** Needs a credential source and a delivery decision |
| Short-lived auto-rotated workload certs; identity bound to environment / region / audience (§9) | mandatory | outbound certificate 1-day, renewed in-process at half-life; inbound `MTLS_MAX_CLIENT_CERT_LIFETIME` / `MTLS_REQUIRED_URI_PREFIX` built but **off** | ⚠️ | **PARTIAL — O-1 below.** Short-lived + auto-rotated done; environment binding needs the issuer. *Was:* ❌ absent (90-day cert, provisioned once, never renewed) |
| Crypto versioned and replaceable (SEC-INV-22) | mandatory | every stored record names its algorithm (`envelope/aes-256-gcm/v1`) and KEK; token format versioned (`ltk:v3`); KMS provider swappable by config | ✅ | **FIXED 2026-09-29.** *Was:* ⚠️ AES-256-GCM hardcoded |
| Observable / testable control state (SEC-INV-25) | mandatory | metrics on every broker decision class; audit script incl. FE | ✅ | Unchanged |
| Secret scanning in pre-commit / CI (§13) | mandatory | `.pre-commit-config.yaml` + `.github/workflows/secret-scan.yml` (gitleaks v8.21.2, new commits only) | ✅ | **FIXED 2026-09-29** (repo level). Verified: a range containing a known leak fails, a clean range passes, a staged leak is caught; this service scans clean. *Was:* ❓ outside the service |
| Non-exportable workload credentials (§9) | "where platform capability permits" | KMS master key: Transit key refused at boot if exportable / plaintext-backup / not symmetric; Cloud KMS keys cannot be exported. Workload mTLS private key: **generated by the issuer and sent over the wire** | ⚠️ | **PARTIAL — O-2 below.** *Was:* ❓ n/a as built |

**Read routes (flagged on 23 Sep as a risk, not scored):** now resolved. `GET /v1/secrets/audit`,
`GET /v1/secrets/leases`, `GET /v1/secrets/leases/{id}`, `GET …/versions` and
`GET /v1/secret-policies` each call authorization-svc (`SECRET_AUDIT_READ`, `SECRET_LEASE_READ`,
`SECRET_POLICY_VERSION_LIST`, `SECRET_POLICY_LIST`).

## Compliance

**18 of 21 scored controls fully met — 86%** (partials at half: **93%**). No items need
clarification. *Was:* 7 of 19 on the table's own rows — 37% (50%); stated on 23 Sep as 10 of
25 — 40% (52%).

The two 23 Sep ❓ rows are now scored (one ✅, one ⚠️), which is why the denominator rose from
19 to 21.

**Top gaps by risk** — all five closed within service scope on 2026-09-29:

1. ~~**The broker trusts a body-supplied workload identity**~~ — **FIXED.** Body must match the
   verified actor, and the certificate must name every identity header.
2. ~~**No inbound mTLS**~~ — **FIXED.** Mandatory in production/staging, identity check on.
3. ~~**Master key lives in application config**~~ — **FIXED.** KMS envelope encryption; the
   in-process key is refused in production.
4. ~~**Leases expire and revoke only on paper**~~ — **FIXED.** Lease-bound signed tokens with a
   verify endpoint.
5. ~~**No automated rotation, no emergency retrieval workflow, no shared-secret exception
   register**~~ — **FIXED.** All three built, and the defects in the first versions fixed.

## Open gaps — re-check these when the domain is complete

Each entry says what the standard requires, what exists now, what is blocking it, and how to
re-check it. Re-run this list once mtls-management-svc and the KMS are in place.

### O-1 · Workload identity bound to environment / region / audience (§9) — ⚠️ partial

- **Requirement:** "short-lived auto-rotated workload certs; identity bound to environment /
  region / audience".
- **Done here:** this service's outbound certificate is requested at 1 day (`rotation_days=1`,
  `auto_rotate=true`) and renewed in-process at half-life (`internal/mtls`, `renewingCert`).
  Inbound, `inboundmtls.CertPolicy` can refuse long-lived certificates
  (`MTLS_MAX_CLIENT_CERT_LIFETIME`) and certificates without a URI SAN under a prefix
  (`MTLS_REQUIRED_URI_PREFIX`). Both are **off**.
- **Blocked on mtls-management-svc:** it issues CN + DNS SAN only, no URI SAN, in whole days
  (1–90, default 90). Turning the checks on today would refuse every certificate.
- **mtls-management-svc must provide:** a URI SAN `spiffe://zoiko/<env>/<service>` on every leaf,
  with `<env>` taken from the **issuer's own configuration, never the request** (otherwise
  staging could ask for a production identity).
- **Then change here:** set `MTLS_REQUIRED_URI_PREFIX=spiffe://zoiko/<env>/` and
  `MTLS_MAX_CLIENT_CERT_LIFETIME=24h`; make both mandatory in `config.ValidateInbound` for
  production/staging; match `MTLS_TRUSTED_FORWARDERS` and the identity check on the SPIFFE URI
  (`identityMatches` already reads URI SANs).
- **Rollout order:** issuer first → every caller re-provisions (about one day at 1-day
  lifetimes) → switch the checks on here. Reversed, every old certificate is refused.
- **Re-check:** a certificate for `spiffe://zoiko/staging/…` presented to a production instance
  → `403 client_certificate_policy` (`certificate_not_bound_to_this_environment`); a 90-day
  certificate → `403 … certificate_lifetime_exceeds_maximum`; the service refuses to boot in
  production with either variable unset. Existing tests:
  `TestCertPolicy_CertForAnotherEnvironmentRefused`, `TestCertPolicy_LongLivedCertRefused`.
- **Done when:** both checks are on and mandatory in production, and a live cross-environment
  certificate is refused.

### O-2 · Non-exportable workload credentials (§9) — ⚠️ partial

- **Requirement:** "non-exportable workload credentials where platform capability permits".
- **Done here:** the master key is non-exportable. `TransitKeyWrapper.VerifyNonExportable`
  refuses (fatal in production) a Transit key that is `exportable`, allows plaintext backup, or
  is not a symmetric encryption key; Cloud KMS keys cannot be exported.
- **Still open:** this service's **mTLS private key is generated by mtls-management-svc and
  returned in the response body** (`private_key_pem`). The key exists outside the workload before
  the workload ever uses it.
- **mtls-management-svc must provide:** an endpoint that signs a CSR and returns only the
  certificate, never a private key. It must accept the same bootstrap-token authentication.
- **Then change here:** generate an ECDSA P-256 key in process, send only the CSR, and renew by
  re-signing a new CSR (the renewal loop in `internal/mtls` stays). Optionally hold the key in a
  TPM or KMS where the platform supports it.
- **Re-check:** capture the provisioning exchange — no `private_key_pem` in any response; the
  certificate's public key matches a key generated locally; renewal produces a new key pair.
- **Done when:** no private key for this service ever crosses the network.

### O-3 · Dynamic credentials preferred over static (§13) — ⚠️ open, needs a decision

- **Requirement:** "Prefer identity federation and dynamic credentials".
- **Now:** material is static (seeded via `POST …/material`, replaced by rotation). The broker
  issues a signed pointer token; **no workload has a path to redeem it for a credential** — the
  only route that returns material is break-glass.
- **Blocked on:** (a) a running credential source — HashiCorp Vault with a secrets engine
  (database, cloud IAM), the same Vault needed for O-2's KMS; (b) a **delivery decision**:
  1. a Vault Agent sidecar injects short-lived credentials into the workload (this service stays
     a policy/lease broker), or
  2. the broker returns short-lived, per-lease credentials generated by Vault
     (`GET /v1/<engine>/creds/<role>`), with lease revoke mapped to Vault lease revoke and rotation
     handled by Vault's TTL.
- **Then change here:** add a per-policy credential mode (`STATIC` / `DYNAMIC` + role), wire
  revoke and rotation to the engine's lease API, and document the redemption contract.
- **Re-check:** two brokers for the same dynamic policy yield two different credentials; revoking
  the lease makes the credential fail at the target system; a credential stops working at the
  lease expiry without any action by this service.
- **Done when:** at least one secret class is issued dynamically end to end, and static material
  is the documented exception rather than the default.

## Before production (infrastructure, not code)

These are enforced by the code: production refuses to boot until they exist.

1. **KMS:** Vault Transit (`vault write -f transit/keys/<name> type=aes256-gcm96`) or a Cloud KMS
   symmetric key; `VAULT_KEK_PROVIDER` and its settings. Migrate existing material once with
   `VAULT_MIGRATE_LEGACY_KEY_FILE`.
2. **mTLS:** server certificate, key and client CA (`TLS_CERT_FILE`, `TLS_KEY_FILE`,
   `TLS_CLIENT_CA_FILE`); the gateway's client-certificate identity in `MTLS_TRUSTED_FORWARDERS`
   (otherwise console traffic is refused); `MTLS_BOOTSTRAP_TOKEN_PATH` if `AUTHZ_MTLS_ENABLED`.
3. **Shared secrets:** register an exception for every secret more than one workload brokers,
   before `REQUIRE_SHARED_SECRET_EXCEPTION` takes effect.

## Remaining cross-service dependencies

1. ~~**Gateway / edge** must strip client-sent `X-Workload-Id` (cross-service finding 1). Without
   a client certificate, the envelope actor falls back to it.~~ **Done 2026-09-29** in 5/9: it is
   stripped by `gtrm-edge-strip` and again by ForwardAuth on every tenant route.
2. **Gateway** needs its own client certificate for the gateway → service hop (O-1 forwarder).
3. **mtls-management-svc** — URI SANs (O-1) and CSR signing (O-2).
4. **Vault / KMS** — provisioning for the master key, and the credential source for O-3.
5. **Repo-wide secret scan:** a full-history gitleaks run reports 6 findings outside this
   service, for their owners to rotate or allowlist with a reason:
   `identity-context-svc/MANUAL-TEST-GUIDE.md:132` ·
   `identity-context-svc/internal/auth/jwt_test.go:17` and `:112` ·
   `search-indexer-svc/internal/projection/projector_test.go:103` ·
   `tools/servicectl/registry_gen.go:753` · `deployments/docker-compose.yml:520`.

**Decisions taken where the standard is silent** (recorded, revisit if the standard names a
rule): break-glass requires two people (approver ≠ retriever); "shared production secrets" means
environment (enforced in production/staging), not secret class; `ltk:v2` tokens are refused
rather than grandfathered; sweeper claim held 10 minutes; lease ceiling 24h by default.

---

# 5/9 — gateway-auth-svc (:8092) vs Security Standard §8 (Network/Edge) + GOV-01 §4 ingress duties

**Contract extracted from:** the Security wireframe `.docx` §8 (Internet edge / Ingress /
Service-to-service rows) and §3.1 invariants; the Governance Control Plane `.docx`
(= **ZS-SVC-A-001**) §4 GOV-01.
**Code:** `services/gateway-auth-svc/`, plus `deployments/docker-compose.yml` and
`deployments/gtrm/`, since the gateway's contract is half Traefik configuration.

> **Status as of 29 September 2026: CLOSED within service scope. 21 of 22 scored items met
> (95%, 98% weighted); 1 partial waits on other services.** Remediated and then re-audited on
> 29 Sep. The rows below give the current state and say what was wrong before.
>
> Commit `2305df1` (25 Sep) had already claimed all four top gaps fixed. **Every one of those
> fixes failed the negative path this audit names.** The same-day re-audit then found two more
> defects that no earlier pass had looked for, which dropped the score to 82% until they were
> fixed. All are listed under *Defects found beyond the audit*.
>
> Verified by **56 passing Go tests in gateway-auth-svc (plus 25 subtests) and 30 in the GTRM
> compiler, 0 failed, 0 skipped**, plus the GTRM drift check. Every new test was also run
> against the pre-fix code and failed there.
>
> **Not yet committed or deployed.** `scripts/audit.sh` has not been re-run against the live
> stack, and no live Traefik request was made. The edge fixes are proven on the compiled config,
> not on the wire.

Single functional route: `POST|GET /verify` (Traefik ForwardAuth), plus `/healthz`, `/readyz`,
`/metrics`. Exempt from the envelope middleware — correctly, and for the reason
`internal/envelope/policy.go:81` gives.

## Defects found beyond the audit (fixed 2026-09-29)

The first five were present after `2305df1` and each reopened the gap it was meant to close. The
last two predate it and were found by the re-audit.

| Defect | Effect | Fix |
|---|---|---|
| The edge stripped all 10 envelope headers the audit named, including 8 the §4 contract makes caller-written | Services that require purpose or book answered every console write with 400 `envelope_incomplete`. `X-Expected-Version` was deleted, so writes lost their optimistic-concurrency check without any error | Split policy: strip only `X-Workload-Id` and `X-Support-Context-Id`; the 8 caller assertions reach the owning service |
| GTRM ForwardAuth `authResponseHeaders` listed 4 names; the gateway sets 8 | On every tenant host a client's own `X-Legal-Entity-Id`, `X-Jurisdiction-Context`, `X-Residency-Policy-Id` (and 3 more) reached the backend as if verified | One 10-name list, identical in compose and GTRM; `TestEdgeContract_ForwardAuthListsMatchVerify` reads both files |
| The STEP_UP_MFA block was a list of named decisions | Any decision the list did not name (a new value, or a casing drift such as `step_up_mfa`) passed as ALLOW | Every answer except `ALLOW` blocks |
| The mTLS client sent no bootstrap token and no envelope | mtls-management-svc refused provisioning, so enabling mTLS would have exited the gateway at boot. No config enabled it, so nothing noticed | Ported the working client from secret-vault-integration-svc: bootstrap token, envelope, 1-day certificate renewed at half-life |
| `*_MTLS_URL` was read and never used | The mTLS client was handed the `http://` URL, so "enabled" still sent plaintext | Enabling a peer switches it to its `https://` URL; a non-https mTLS URL is refused at boot |
| **GTRM chain `[edge-strip, gateway-auth, ctx]`** (re-audit) | The ctx middleware set `X-Zoiko-Resolved-Tenant-Id` after `/verify` ran, so the NP-2 tenant/hostname check compared nothing on every tenant host. A token for tenant A on tenant B's hostname reached B's regional pool. The handler test passed only because it set the header itself | `[edge-strip, ctx, gateway-auth]`; checked against the compiled config by `TestEdgeContract_ResolvedTenantReachesVerify` |
| **`trustForwardHeader: true` and leftmost `X-Forwarded-For`** (re-audit) | Behind a trusted proxy, a client's `X-Forwarded-Method: GET` on a POST made the gateway score a write as a read. The client also chose the IP CARTA scored | Flag removed, so Traefik sets method, URI and IP from the real request; `clientIP` takes the rightmost hop |

## GOV-01 ingress duties

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Verify signed envelope | GOV-01 | JWKS fetch, `ParseWithClaims`, issuer + audience pinned | ✅ | 401 `outcomeNoToken` / `outcomeInvalidToken` |
| Incomplete claims refused | "ambiguous ⇒ deny" | empty `principal_id` or `tenant_id` → 401 `outcomeIncompleteClaims` | ✅ | |
| Hostname / tenant binding, no fallback | NP-2 | `X-Zoiko-Resolved-Tenant-Id ≠ claims.TenantID` → 403 | ✅ (fixed 29 Sep, after re-audit) | The re-audit found this **dead on every GTRM route**. The chain was `[gtrm-edge-strip, gateway-auth, gtrm-ctx-<slug>]`: the edge deleted the header, ForwardAuth called `/verify` with it absent, and only then did ctx set it. So a token for tenant A on tenant B's hostname reached B's pool, and acceptance test O was never enforced. **Now `[edge-strip, ctx, gateway-auth]`.** Proof: `TestEmit_RealMap_EveryDataBearingRouterIsForwardAuthed` (exact chain, and ctx sets a non-empty resolved tenant) and `TestEdgeContract_ResolvedTenantReachesVerify` (gateway-auth-svc, reads the compiled config). The latter fails on the pre-fix config for all 3 tenant routers |
| Server-resolved context forwarded | tenant_id, entity, jurisdiction, residency | sets `X-Principal-Id`, `X-Tenant-Id`, `X-Legal-Entity-Id`, `X-Correlation-Id`, `X-Jurisdiction-Context`, `X-Timezone`, `X-Residency-Policy-Id`, `X-Tenant-Context-Stale` | ✅ | The resolver is called directly on :8081, deliberately not through Traefik. Until 29 Sep only 2 of these 8 were on the GTRM ForwardAuth list, so on tenant hosts the other 6 arrived as whatever the client sent (see *Defects found beyond the audit*) |
| Bounded stale read | "stale context may be read only within bounded TTL" | `X-Tenant-Context-Stale: true` | ✅ | Staleness is declared to the upstream rather than hidden |
| Resolution unavailable → deny | fail closed | 503 + `X-Tenant-Context: unresolved`; tenant denied → 403 `denied` | ✅ | |
| Denial evidence | stable error code + evidence | `X-Auth-Denial-Reason`, per-outcome metrics, SIEM stream | ✅ | |

## §8 Ingress row

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **Authenticated routes** | Ingress control | every data-bearing GTRM router runs exactly `[gtrm-edge-strip, gtrm-ctx-<slug>, gateway-auth]`. The only router without them is the catch-all, which ends at the residency-neutral terminator | ✅ (29 Sep, doc drift) | **Checked against the GCP runbook, as this row asked.** Production ingress is the GTRM-compiled config and nothing else (runbook Step 13, gotcha 7: "Host()-based, ForwardAuth-enforced, with a fail-closed catch-all"; `all-services.yml` must never ship). The finding described the *local* compose, where services publish host ports for development. Proof: `TestEmit_RealMap_EveryDataBearingRouterIsForwardAuthed` compiles the real `routing-map.yaml`. It fails on every tenant router when `gateway-auth` is removed from the emitter (mutation run 29 Sep) |
| Hostname / tenant resolution | Ingress control | GTRM compiled per-tenant routers + the mismatch check | ✅ (fixed 29 Sep) | Resolution (hostname → pool, fail-closed catch-all) and the binding check both run. See NP-2 above |
| Protocol normalization | Ingress control | `X-Forwarded-Method` preferred over the method. `websecure` `:443` TLS entrypoint, and `web` `:80` permanently redirected to it | ✅ (fixed 29 Sep, after re-audit) | The re-audit found two holes. (1) The GTRM ForwardAuth set `trustForwardHeader: true`, so behind a trusted proxy (the GCP L7 LB in production) a client's `X-Forwarded-Method: GET` on a POST reached `/verify` and the write was scored as a read. (2) `clientIP` took the leftmost, client-written `X-Forwarded-For` entry, so the caller chose the IP CARTA scored. **Now `trustForwardHeader` is off, so Traefik sets the method, URI and IP from the real request, and `clientIP` takes the rightmost hop.** Proof: `TestEdgeContract_ResolvedTenantReachesVerify` (no `trustForwardHeader: true` in the compiled config) and `TestClientIP_IgnoresClientWrittenForwardedFor`. Both fail on the pre-fix code. **Trade-off:** behind the GCP LB the peer Traefik sees is the Google front end, so CARTA now scores the LB address rather than the real client there. The IP can't be spoofed, but it carries little signal. Recovering the real client IP needs a trusted-hop design at the edge; that is a CARTA signal-quality follow-up, not an ingress control |
| **Header sanitation** | Ingress control | the edge strips 8 `X-Zoiko-*` routing headers plus `X-Workload-Id` and `X-Support-Context-Id` (`deployments/gtrm/compiler/emit.go`). ForwardAuth `authResponseHeaders` is the same 10 names in compose and GTRM: it replaces the 8 headers Verify sets and strips the 2 it never sets | ✅ (29 Sep) | **Split policy, ruled 29 Sep.** The first audit asked for all 10 envelope headers to be stripped. That reading conflicts with the §4 envelope contract (`internal/envelope/envelope.go`), which names `X-Tenant-Id` and `X-Principal-Id` as the only headers the caller does not write. The other 8 (`X-Purpose-Context`, `X-Approval-Reference`, `X-Evidence-Refs`, `X-Causation-Id`, `X-Workflow-Instance-Id`, `X-Book-Id`, `X-Source-Channel`, `X-Expected-Version`) are the caller's own claims, checked by the service that owns the data, so they are deliberately passed through. The 25 Sep fix stripped them too: services that require purpose or book answered 400 `envelope_incomplete`, and the version check on writes was lost. The same fix also left 6 headers the gateway sets, including `X-Legal-Entity-Id`, out of the GTRM `authResponseHeaders` list, so a client's copy reached the backend unreplaced on every tenant host. Proof: `TestEmit_EnvelopeSanitation_SplitPolicy` (gtrm compiler) and `TestEdgeContract_ForwardAuthListsMatchVerify` (gateway-auth-svc, reads both edge configs). Both fail on the 25 Sep config |

## §8 Internet edge row

❓ **Still needs clarification (6 items: DDoS protection, WAF, rate limiting, bot/abuse controls,
TLS, request-size limits).** The facts have moved since 23 Sep, but the question has not been
answered:

- **Local compose (since `2305df1`, 25 Sep):** a `websecure` `:443` TLS entrypoint with `web`
  `:80` permanently redirected to it, and `ratelimit`, `buffering`, `security-headers` and
  `compress` middlewares on that entrypoint (`deployments/traefik-dynamic/middlewares.yml`).
- **GCP runbook (Step 13):** a global L7 load balancer with a Certificate Manager certificate for
  `*.zoikosuite.com` in front of Traefik, so production TLS terminates there. The runbook names no
  Cloud Armor (WAF/DDoS), bot controls or production request-size limits.

These are not scored until someone owns the decision on where the production edge controls live.
The "API gateway" control itself is ✅.

## Continuous risk (Doc 05 §3.11, via SEC-INV-14)

| Item | Implemented | Status | Notes |
|---|---|---|---|
| Risk assessment consulted per request | `carta.Evaluate` on every verify | ✅ | |
| ISOLATE / DENY enforced | 403 + `X-Carta-Decision` + SIEM | ✅ | |
| **STEP_UP_MFA** | 403 `carta_blocked` + `X-Carta-Decision: STEP_UP_MFA`, logged, counted, streamed to SIEM | ✅ (29 Sep) | No step-up factor exists anywhere in the estate to satisfy it, so it blocks. The 25 Sep fix added STEP_UP_MFA to a list of blocking decisions, which still let through any decision the list did not name (a new enum value, or a casing drift). Now every answer except `ALLOW` blocks. The carta-unreachable case (nil assessment) is unchanged, see the next row. Proof: `TestVerify_CartaStepUpMFA_Returns403` and `TestVerify_CartaUnrecognisedDecision_Returns403`. The latter fails on the 25 Sep handler |
| carta unreachable → proceed | `Evaluate` returns nil; documented as an "additive signal" | ❓ | SEC-INV-14 names "policy, identity or key status", and a risk score is arguably none of those. Deliberate and documented, so not called a failure |

## Relevant SEC invariants

✅ SEC-INV-02 (authoritative tenant binding), SEC-INV-04 (this service *is* the control that
stops network position implying authorization), SEC-INV-14 (identity/policy paths fail closed),
SEC-INV-18 (no tokens or sensitive values logged).
✅ SEC-INV-01 (29 Sep): every data-bearing production route is ForwardAuth-enforced by construction
(see Authenticated routes), and unmatched traffic falls through to the fail-closed catch-all.
✅ SEC-INV-13 (29 Sep): `X-Support-Context-Id` is stripped at the edge and again by ForwardAuth,
and identity-context-svc verifies it (1/9 gap 1).
⚠️ **SEC-INV-05** (29 Sep: the gateway's side is done, the peers' side is not). The 25 Sep mTLS
client could not have worked. It sent no bootstrap token and no envelope, so mtls-management-svc
refused provisioning and the gateway would have exited at boot. It also read `*_MTLS_URL` without
using it: the client was handed the `http://` URL, so "enabled" sent plaintext. And it asked for a
90-day certificate that was never renewed. It is replaced by the working pilot client from
secret-vault-integration-svc (1-day certificate renewed at half-life, bootstrap token, envelope).
Enabling a peer now switches its calls to the `https://` mTLS URL, and a non-https URL is refused
at boot (`internal/config/config_test.go`). **Still off in compose because neither identity-svc nor
tenant-entity-registry-svc has an mTLS listener.** Each needs a `:8449` listener with
`RequireAndVerifyClientCert` (the authorization-svc `internal/mtls` pattern) before its
`*_MTLS_ENABLED` can be set to true.

## Compliance

**21 of 22 scored items fully met — 95%** (partials at half: **98%**), after the re-audit's two
defects were fixed on 29 Sep. History: baseline 15 of 22 (68% / 80%). The remediation pass claimed
21 of 22. The re-audit found NP-2 dead and forwarded-header trust, which put it at 18 of 22
(82% / 89%). Both are now fixed and proven by tests that fail on the pre-fix code. The only open
item is SEC-INV-05 ⚠️, which waits on mTLS listeners in identity-svc and
tenant-entity-registry-svc. 7 items still need clarification (6 on the Internet-edge row, plus
carta-unreachable). Not run: `scripts/audit.sh` and a live Traefik request. The middleware-order
fix is proven on the compiled config, not on the wire.

**Top gaps by risk**

1. ✅ **Fixed 29 Sep under the split policy** (see the Header sanitation row). The identity-class
   headers are stripped at the edge. The 8 §4 caller-supplied fields are passed through for the
   service that owns the data to check. Original finding: **the governance envelope is not sanitized at the edge** (auth). This is the finding that
   ties the audit together: `X-Support-Context-Id` reaches identity-context-svc from the open
   internet, and that service stamped it onto a session without verifying it (gap 1 of service
   1/9 — **fixed there 2026-09-23**; identity-context-svc now verifies it, but the header is still
   unsanitized at the edge). `X-Purpose-Context` — the field §4 requires for "governed sensitive access" — is
   likewise self-asserted, as are `X-Approval-Reference` and `X-Evidence-Refs`. Fixing either
   end closes it; fixing the edge closes it for every service at once.
2. ✅ **Fixed 29 Sep.** Every CARTA answer except ALLOW blocks. Original finding: **STEP_UP_MFA is
   decided and discarded** (auth). The risk engine's middle answer had no effect.
3. ⚠️ **Gateway side fixed 29 Sep; waits on peer listeners.** Original finding: **no mTLS on the
   gateway's own upstream calls** (SEC-INV-05), matching the finding in 4/9.
4. ✅ **Doc drift, confirmed against the GCP runbook 29 Sep.** Production ForwardAuth covers every
   tenant route. Original finding: **ForwardAuth covers one route** (auth, deployment).
5. ✅ **Fixed 29 Sep (found by the re-audit).** The NP-2 hostname binding never ran, because the
   GTRM ctx middleware set `X-Zoiko-Resolved-Tenant-Id` after ForwardAuth. The chain is now
   `[edge-strip, ctx, gateway-auth]`.
6. ✅ **Fixed 29 Sep (found by the re-audit).** Forwarded-header trust: `trustForwardHeader` is
   off, and `clientIP` takes the rightmost hop. Follow-up: behind the GCP LB, CARTA scores the LB
   address, so recovering the real client IP needs a trusted-hop design.

## Changed files and proof (29 Sep)

| File | Change | Proven by |
|---|---|---|
| `deployments/gtrm/compiler/emit.go` | Split strip list; one 10-name `gatewayAuthResponseHeaders`; chain `[edge-strip, ctx, gateway-auth]`; `trustForwardHeader` removed | `TestEmit_EnvelopeSanitation_SplitPolicy`, `TestEmit_RealMap_EveryDataBearingRouterIsForwardAuthed` (real routing map; mutation-tested) |
| `deployments/gtrm/compiled-traefik.yml` | Regenerated by the compiler, never hand-edited | GTRM drift check `--check` OK |
| `deployments/docker-compose.yml` | ForwardAuth list gains `X-Workload-Id`, `X-Support-Context-Id`; gateway gets the mTLS env (off) and the bootstrap-token volume | `docker compose config -q` OK; edge-contract test reads it |
| `internal/handler/handler.go` | Non-ALLOW CARTA blocks; `clientIP` takes the rightmost hop | `TestVerify_CartaUnrecognisedDecision_Returns403`, `TestClientIP_IgnoresClientWrittenForwardedFor` |
| `internal/handler/edge_contract_test.go` (new) | Reads both edge configs: header lists match what Verify sets, the ctx middleware runs before gateway-auth, and no forwarded-header trust | `TestEdgeContract_ForwardAuthListsMatchVerify`, `TestEdgeContract_ResolvedTenantReachesVerify` |
| `internal/mtls/mtls.go` + `mtls_test.go` | Replaced by the renewing client from secret-vault-integration-svc | 4 tests: short-lived cert, half-life renewal, issuer outage, token + envelope |
| `internal/config/config.go` + `config_test.go` (new) | mTLS enable switches the peer URL; non-https refused at boot; `MTLS_BOOTSTRAP_TOKEN_PATH` | 3 tests |
| `cmd/server/main.go` | One workload identity shared by both peers | `go vet`, build |

Every test named here, except the ported mTLS tests, was run against the pre-fix code and failed
there.

## Remaining cross-service dependencies

1. **identity-svc and tenant-entity-registry-svc need an mTLS listener** on `:8449` with
   `RequireAndVerifyClientCert` (the authorization-svc `internal/mtls` pattern). Until then,
   `IDENTITY_JWKS_MTLS_ENABLED` and `TENANT_REGISTRY_MTLS_ENABLED` must stay `false`. Turning
   either on first refuses every request in the estate. This is the one open item (SEC-INV-05).
2. **Owning services must validate the 8 pass-through envelope fields.** Under the split policy
   the edge deliberately lets `X-Purpose-Context`, `X-Approval-Reference`, `X-Evidence-Refs` and
   the rest through. For example, a service that acts on `X-Approval-Reference` has to confirm the
   approval exists and names this action.
3. **configuration-feature-flag-svc trusts `X-Commercial-Plan` and `X-Org-Unit-Id`, which the
   edge neither strips nor sets.** (`X-Jurisdiction-Context` is now overwritten by ForwardAuth, and
   `X-Workload-Id` is stripped.) Either the edge strips these two and a trusted component sets
   them, or that service stops treating them as server context. That needs a ruling, since
   neither is a §4 envelope field.
4. **Console → service calls bypass ForwardAuth.** The runbook points the console at in-cluster
   Services, and `lib/api/envelope.ts` states that nothing verifies a token on that path, so
   services trust the `X-*-Id` headers the console server sends. This needs its own audit item.
5. **CARTA client IP in production.** With forwarded-header trust off, CARTA scores the GCP front
   end's address rather than the real client's. Recovering it needs a trusted-hop design at the
   edge (Traefik `forwardedHeaders.trustedIPs` for the LB ranges, plus a gateway-side hop count).
   That is a signal-quality follow-up for carta-svc's owners, not an ingress control.

**Decisions taken** (recorded, revisit if the standard names a rule): envelope sanitation is the
split policy (ruled 29 Sep). STEP_UP_MFA blocks until a step-up factor exists. Carta unreachable
still proceeds (the ❓ row, untouched).

Two things this service does better than its siblings: every denial carries a distinct outcome
label and reaches SIEM, and the tenant/hostname mismatch is a first-class refusal. That second
one only became true on 29 Sep, when the check started receiving the header it compares.

---

# 6/9 — search-indexer-svc (:8096) vs ZS-SVC-AB-001 (ESR-01 … ESR-05)

**Contract extracted from:**
`ZS-SVC-AB-001_Enterprise_Search_Indexing_Query_Secure_Retrieval_Control_Detailed_Service_Specifications_v1.0.docx`
§2.2, §6.2–6.3, §7.1–7.3, §8.2–8.3, §10, §11.1 (8 APIs), §11.2 (8 events), §11.3 (20 reason
codes), §14 NP rows.
**Code:** `services/search-indexer-svc/`, plus `services/search-client/` (used only by this
service).

> **Status as of 30 September 2026: CLOSED within service scope. 47 of 49 scored items met
> (95.9%, 98.0% weighted); 2 partials wait on OD-10 (no embedding provider has been chosen).**
> Remediated, re-audited live, and remediated again on 30 Sep. The rows below give the current
> state and say what was wrong before.
>
> Commit `2305df1` (25 Sep) claimed four fixes: control-plane authorization, ESR-012 staleness,
> the R2 hydrator and staleness events. **Only the authorization change held.** The ESR-012 check
> could never fire (lag was computed as `time.Since(time.Now())`). The hydrator was wired to no
> source, called a path no service serves, sent an incomplete envelope and asserted its own
> identity. The facet "gap" had never been one (see Facet privacy). The live re-audit then found
> six defects no earlier pass, including the original audit, had looked for. The worst was an
> **erased record returning to search results** while its restriction still read VERIFIED. All
> of them are listed under *Defects found beyond the audit*.
>
> **Verified live** against real Postgres 16, Kafka and OpenSearch 2.17. The service ran in a
> golang container; authorization used the permit-all stub; small stubs stood in for the
> embedding provider and obligations-svc. **Verified in Docker:** all 13 packages pass `go vet`
> and `go test -race`. All **27 store integration tests pass against real Postgres**, with
> skipping disallowed (`REQUIRE_DB_TESTS=1`); 9 of them are new, and the RLS tests run as an
> unprivileged role. Migrations 000001–000004 apply to a fresh database and again on top of
> themselves, and upgrade a copy of the existing `search_indexer` database cleanly. openapi.yaml
> is valid under redocly and openapi-spec-validator.
>
> **Not yet committed.** The real `search_indexer` database was backed up and upgraded with 000003
> and 000004 on 30 Sep, and `scripts/audit.sh` (updated for the remediation, with a new section 16)
> ran against the rebuilt compose stack: **51 pass, 1 fail, 1 skip**. That run found one more
> in-service defect (gauges exposing no series before a scope is live), since fixed. The one failure,
> and the audit sections behind it, are **authorization-svc's**: its database is behind its code
> (`column pra.book_id does not exist`), so every `/v1/authorize` answers 503 and this service
> correctly fails closed with ESR-008. Not verified: authorization grants end to end (blocked on
> that), the real obligations-svc, a real embedding provider.

**Scoring note.** The rows of the 23 Sep version of this section add up to 49 scored items (8 APIs
+ 8 events + 20 reason codes + 13 controls), not the 48 its headline used, and they gave 42 full,
not 43. The baseline recounted from its own rows is **42 of 49 (85.7%, 89.8% weighted)**, and
every figure below uses that basis.

## Defects found beyond the audit (fixed 2026-09-30)

The first four were the 25 Sep "fixes", each of which left its gap open. The rest were found by
the live re-audit. Every one was reproduced live before it was fixed and re-checked live after.

| Defect | Effect | Fix |
|---|---|---|
| ESR-012 lag was `time.Since(latestCommittedAt())`, and `latestCommittedAt` returned `time.Now()` | Lag was always ~0. A consumer hours behind read CURRENT; STALE was dead code. ESR-012 fired only on UNKNOWN, as HTTP 400 | Lag measured at the broker (`internal/kafka/lag.go`): the group's committed offsets against the log end, plus the timestamp of the oldest unconsumed message. Live: a stalled consumer went LAGGING, then STALE at 29 s of real lag |
| `esr.index_checkpoint.advanced` emitted when the engine count failed | DQC was told the index advanced when nothing had | Emitted only when the broker watermark increases |
| R2 hydrator unwired and unusable | No `SOURCE_SERVICE_URL_*` anywhere; built `/v1/obligation/{id}` against a service that serves `/v1/obligations/{id}`; no `X-Request-Id`/`X-Correlation-ID`, so every strict source refused it 400; asserted `X-Principal-Id: system:search-hydrator` and `X-Workload-Id` on a direct call no edge sees | Configured record collection per source type; forwards the caller's full envelope and asserts no identity of its own; 404/410 → ESR-010. Live: the source saw `/v1/obligations/r3` with the caller's envelope and no workload header |
| Hydrated objects read by contract field name | An R2 result came back with no fields whenever source path ≠ field name | Re-projected through the returnable allowlist by source path |
| **CRITICAL — `POST /v1/restrictions` never wrote the projection ledger** (live) | The ledger kept epoch 0, so the next ordinary update event passed the compare-and-set and **re-indexed an erased record**, content and vector, while its restriction still read VERIFIED. The verifier never re-checks VERIFIED rows. Also left the scope LAGGING forever (ledger ≠ engine). The Kafka restriction lane was safe | The ledger is stamped tombstoned at the restriction epoch before the index is touched, and every serving or candidate generation is tombstoned. Live: erased r3 stayed erased after a later update; scope CURRENT (2 live, 1 tombstoned) |
| **Activating a rebuild emptied the scope** (live) | The indexer wrote only into the ACTIVE generation, so a new one received nothing. Validation exempted an empty index ("a new generation legitimately holds nothing") and passed `engine=0 ledger=4`. Activation replaced a populated index with an empty one (INV-21, INV-22, NP-18) | New generations are backfilled by replaying the source topic, with the ledger as the authority for each record's state; writes are create-only so a replay never overwrites a live write. Live events go to candidates in parallel. READY needs `backfill_state = COMPLETE`, and an empty index over a populated ledger fails. Live: rebuild validated `engine=2 ledger=2 vectors=2`, results identical after cutover, erased record still erased |
| **Model migration could not work** (live) | The indexer projected the ACTIVE generation with the scope's PUBLISHED contract, and only one version can be published. Publishing v2 wrote v2's fields and embedding pin into v1's strict mapping (quarantining every event) and paired v2 with v1 at search time; v2's generation never filled, so certification could never pass | Every generation is projected and searched with its own contract; publishing v2 retires v1 in the same transaction. Live: v1 served under `m@1` while v2 built; READY refused until certified; recall 1.0; cut over to `m@2` |
| **Consumer subscribed before its topic existed never recovered** (live) | kafka-go got "Unknown Topic Or Partition" and the partition watch never picked the topic up; the scope indexed nothing until a restart | A reader is created only once the topic exists; retried every 30 s. Live: indexed 9 s after the topic appeared, no restart |
| **NP-41 contamination check counted every document** (live) | It asked `CountProjections(tenant_id = "")`, which skips empty terms, so every populated generation was reported untenanted and refused READY. Masked only because generations were always empty at validation. Also failed open on a count error | `CountMissing` (`must_not exists`); fails closed |
| First quarantined message per topic lost (live) | The DLQ write failed while the DLQ topic auto-created, and the message was committed anyway. That is the message an NP-35 recovery replays | DLQ write retried; not committed until copied. Live: the first quarantine landed in a DLQ that did not exist beforehand |
| Checkpoint watermark changed units | Pre-upgrade wall-clock values (~1.8e12) would pin `GREATEST()` above every real offset, stopping `checkpoint.advanced` forever | Migration 000004 resets them (tested) |
| Retrieval evaluation was a cross-tenant existence oracle | A platform operator could name any tenant and learn whether given records sit near a topic, 200 probes a call | Runs in the caller's verified tenant only, with FAILED-restriction exclusion |
| `000002_add_rls` was not re-runnable | The runbook's hand-apply loop stopped on "policy already exists" | Policies dropped before they are created |
| Store integration suite applied only 000001–000002, and skipped silently | It tested a schema the code no longer matched | Applies every migration; 27 of 27 pass with `REQUIRE_DB_TESTS=1` |
| Restriction-backlog and freshness gauges exposed no series until a scope was live (found by `audit.sh` on the real stack) | On a fresh deployment, or while the only scope is mid-rebuild, an alert on "backlog > 0" or "freshness STALE" had nothing to evaluate | `bootstrap` placeholder series, as the counters already had; ESR-012/018/019 rejection series pre-initialised too. Live: all three gauges on `/metrics` |
| `scripts/audit.sh` raced the backfill | It moved a new generation straight to READY, which now answers 409 `backfill_incomplete` until the backfill completes | Waits for `backfill_state = COMPLETE` and asserts it |

Smaller fixes from the same passes: the migration gate fails closed and is re-checked at ACTIVE;
5xx refusals no longer go to the `security_filter.denied` abuse stream; 409/429 answers are not
cached by idempotency; `X-Source-System` and `X-Timezone` are forwarded to sources; two lag
corners (no topic; backlog with no timestamp) read UNKNOWN; semantic evidence records the model.

## Canonical APIs (§11.1)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /v1/search` | governed search | `POST /v1/search` | ✅ | Now also gated by scope health (freshness, restriction safety) and carries `index_freshness` / `index_lag_ms` |
| `POST /v1/search/semantic` | semantic / hybrid retrieval; embedding model fixed by scope | `POST /v1/search/semantic` | ⚠️ (30 Sep; waits on OD-10) | Was ❌: zero occurrences of `vector` or `embedding`. **Built and verified live** with a stub provider: same trusted context, planner ordering, scope-health gate and per-hit re-authorization as lexical search; semantic and hybrid modes; the model pinned by the contract; caller-supplied vectors impossible; the query is embedded only after the plan compiled and the gate passed. **Partial only because no provider exists** (OD-10): until `EMBEDDING_PROVIDER_URL` is set it answers 503 ESR-019, loudly, never a lexical fallback |
| `POST /v1/retrieve` | re-authorize candidate refs; bulk limits | `POST /v1/retrieve` | ✅ | Same health gate as search |
| `POST /v1/index-contracts` | privileged control-plane API; immutable after publication | present, plus `GET` and `/{id}/state` | ✅ | Embedding pin validated and part of `schema_digest`. Publishing a version supersedes the previous one atomically |
| `POST /v1/index-generations` | generation-ID idempotency; published contract required | present, plus `GET`, `/{id}/state`, `/{id}/retrieval-evaluations` | ✅ (re-established 30 Sep) | The 23 Sep ✅ did not hold: a rebuild was never filled and its activation emptied the scope (see *Defects*). Now backfilled, validated against the ledger, and verified live end to end |
| `POST /v1/restrictions` | source-event idempotency; older writes cannot resurrect visibility | present, plus `GET`; 409 `stale_restriction_epoch` | ✅ (re-established 30 Sep) | The 23 Sep ✅ did not hold for this lane: a later update resurrected an erased record. Now stamped into the ledger first |
| `GET /v1/checkpoints` | index freshness / checkpoint health | present | ✅ | Watermark and lag now broker-measured. Platform-scoped read (from `2305df1`, consistent with "operational access") |
| `POST /v1/search-exports` | separate export permission, purpose, size and evidence | present; ESR-016 enforced | ✅ | `Idempotency-Key` now honoured: a retry no longer records a second authorization |

Extra surface: `GET /v1/scopes` (now reports the SERVING contract, plus `semantic` /
`embedding_model`), `GET /v1/search-evidence`, `POST|GET /v1/search-sources`,
`POST|GET /v1/index-generations/{id}/retrieval-evaluations`.

## Events (§11.2) — 8 of 8 present

`esr.index_generation.ready`, `esr.index_generation.activated`, `esr.index_checkpoint.advanced`,
`esr.restriction.propagated`, `esr.restriction.failed`, `esr.search.degraded`,
`esr.security_filter.denied`, `esr.reindex.failed`. ✅ Complete. Since 30 Sep: `checkpoint.advanced`
fires only when the broker watermark moves; `search.degraded` also announces a scope's transition
into STALE/UNKNOWN once (seen live); `reindex.failed` also covers a failed retrieval evaluation;
`security_filter.denied` carries only request-caused refusals.

## Reason codes (§11.3) — 20 of 20 declared; 20 reachable

| Code | Status | Notes |
|---|---|---|
| ESR-001 … 011, 013 … 017, 020 | ✅ | All returned from real paths |
| **ESR-012 INDEX_STALE_FOR_SCOPE** | ✅ (30 Sep) | Was ❌, and still unreachable after `2305df1`. Now: a STALE or UNKNOWN active generation refuses R1+ scopes with **503**, and R0 scopes answer 206 DEGRADED/UNKNOWN with ESR-012 in `reason_codes` while withholding any protected document (policy chosen 30 Sep: "block protected, flag R0", §8.3). A checkpoint older than 3× the sweep interval reads UNKNOWN. Live: STALE at 29 s of real lag; UNKNOWN with Kafka stopped |
| **ESR-018 RESTRICTION_PROPAGATION_FAILED** | ✅ (30 Sep) | Was ⚠️ (event only). Now returned to callers (NP-59): a tenant's FAILED restrictions in the scope are excluded by id and the answer is 206 DEGRADED with ESR-018; past 500 the scope is refused 503. Live: a still-visible record was marked FAILED by the verifier and excluded from lexical and semantic results |
| **ESR-019 SEMANTIC_MODEL_MISMATCH** | ✅ (30 Sep) | Was ⚠️ (unreachable). 409 when a caller expects another model; 503 when the provider answers with another model or width (NP-35) or no provider exists. Live: both |

## Controls

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Mandatory filter compilation (§6.3) | server-injected, non-overridable | `Planner.mandatoryFilters(tc, contract)` applied separately from `userFilters` | ✅ | Also inside the k-NN walk's own filter for semantic plans |
| Query safety / complexity (§6.2) | forbidden operators, complexity bound | `checkOperators` → ESR-003; `complexity()` vs `MaxComplexityScore` → ESR-004 | ✅ | Semantic queries bounded to 2 000 characters |
| Field policy (§6.2) | searchable vs returnable | `userFilters` → ESR-005, `requestedFields` → ESR-006 | ✅ | |
| Result window (§7) | bounded | ESR-015; signed cursor bound to tenant + scope + **plan digest** | ✅ | Semantic: one bounded page, `k` ≤ window, cursor refused |
| R0 metadata-safe (§7.1) | retrieval class | implemented | ✅ | |
| R1 re-authorized (§7.1) | current resource authorization per candidate | implemented via the authz client | ✅ | |
| **R2 source-hydrated (§7.1)** | index content is not trusted as the current display value | `internal/hydrator`, wired for `obligation` | ✅ (30 Sep) | Was ⚠️ (no hydrator). See *Defects*: the 25 Sep hydrator could not have worked. Verified live against a source stub: correct collection path, caller's envelope, fields re-projected by source path, a deleted record suppressed as ESR-010. Other source types need a `SOURCE_SERVICE_URL_<TYPE>`; without one their R2 results are suppressed with ESR-014, never served from the index |
| R3 explicit export (§7.1) | separate authorization, purpose, evidence | ESR-016 + `/v1/search-exports` + evidence log | ✅ | |
| Snippet field-policy awareness (§7.2) | a matching sensitive term does not authorize disclosure | `safeSnippets(highlights, allowed)` | ✅ | |
| Facet privacy (§7.3) | facets over the caller-eligible set only | `Planner.facets` inside the compiled plan; `min_doc_count` ≥ 2 at the engine, re-applied at decode | ✅ | Was ⚠️ ("minimum-cell not evident"). **That was an audit miss**: the minimum-cell rule has been enforced since `4dd135c` (21 Sep): `FACET_MIN_COUNT` ≥ 2, validated at boot |
| Restriction epoch, no resurrection (§8.2) | older writes cannot resurrect visibility | `restriction_epoch` throughout; ESR-013; 409 on a stale epoch; HTTP lane now stamps the ledger | ✅ (re-established 30 Sep) | **The 23 Sep ✅ was wrong for the HTTP lane** (critical defect above). Proven live and by `TestLedger_RestrictionEpochRefusesLaterOrdinaryUpdates` against real Postgres |
| Degradation surfaced (§9) | completeness state | `esr.search.degraded` + ESR-020 + **206 Partial Content** | ✅ | Plus freshness (`index_freshness`) and restriction safety (ESR-018) |
| Vector index controls (§10.1) | 6 controls: pinned model, lineage, server-selected partitions, similarity ≠ authorization, tombstone propagation, migration certification | all six | ⚠️ (30 Sep; waits on OD-10) | Was ❌. **All six verified live:** (1) model, version, width, preprocessing and space pinned in the contract, checked on every provider answer; (2) the vector lives in the same document as the lexical projection, so it inherits tenant, residency, epoch and tombstone (INV-26); (3) Lucene filtered k-NN walks only the caller's eligible set; tenant B's nearest document never reached tenant A (NP-33); (4) every hit goes through the same re-authorization and hydration; (5) a tombstone carries no vector, and the verifier treats a vector on a tombstone as still visible (NP-34); (6) a model migration cannot reach READY or ACTIVE without a passing recall@k evaluation. Partial only because no provider is configured |
| RAG contract (§10.2) | 5 boundary rules | — | ❓ | §10.2 assigns most of these to AIG, not ESR. The one ESR-side rule ("may not substitute a broader platform identity") is now met by the hydrator |

## Compliance

**47 of 49 scored items fully met — 95.9%** (partials at half: **98.0%**). History, on the recounted
basis: baseline 42 of 49 (85.7% / 89.8%, published as 43 of 48, 90% / 93%). The 30 Sep
remediation took it to 43 (87.8% / 91.8%). The live re-audit found two 23 Sep ✅ rows that did not
hold, which put it at 46 with three partials (93.9% / 96.9%). Everything the re-audit found is now
fixed and verified live. The two open items are ⚠️ only because no embedding provider exists (OD-10).
At code level, with a provider configured, it is 49 of 49. 1 item needs clarification (RAG
contract).

**Top gaps by risk** (from the 23 Sep audit)

1. ⚠️ **Built 30 Sep; waits on OD-10.** Semantic retrieval, all six §10.1 controls and ESR-019, all
   verified live with a stub provider. Original finding: **ESR-02's semantic / vector half is
   entirely absent** (contract gap).
2. ✅ **Fixed 30 Sep.** Broker-measured lag; STALE/UNKNOWN refuse protected scopes and flag R0;
   freshness on every response. Original finding: **index staleness never reaches the caller**
   (data integrity). Not fixed by `2305df1`.
3. ✅ **Fixed 30 Sep.** Hydrator wired, caller's envelope, source-path projection. Original finding:
   **no R2 hydrator wired** (data integrity, deferred). Not fixed by `2305df1`.
4. ✅ **Audit miss, confirmed 30 Sep.** Minimum-cell suppression was enforced from the start.
   Original finding: **facet minimum-cell suppression not evident** (privacy).
5. ✅ **Fixed 30 Sep (found by the live re-audit).** HTTP restrictions resurrected by later updates.
6. ✅ **Fixed 30 Sep (found by the live re-audit).** Rebuilds never filled; activation emptied the
   scope; model migration impossible; NP-41 check rejected every populated generation.

## Changed files and proof (30 Sep)

| Area | Change | Proven by |
|---|---|---|
| `internal/kafka/lag.go`, `internal/indexer/indexer.go` | Broker lag probe; freshness = worst of lag, population, measurability; `advanced` only on progress; STALE/UNKNOWN transition event | `TestCheckpoint_*` (7 tests); live STALE / UNKNOWN / recovery |
| `internal/handler/health.go`, `internal/retrieval/retrieval.go` | Scope-health gate (ESR-012, ESR-018); per-hit suppression; freshness on responses | `TestSearch_Stale*`, `TestSearch_FailedRestrictions*`, `TestExecute_*Stale*`; live |
| `internal/handler/handler.go` (`ApplyRestriction`) | Ledger stamped at the restriction epoch first; every generation tombstoned | `TestRestriction_StampsTheLedgerAtItsEpoch`; `TestLedger_RestrictionEpochRefusesLaterOrdinaryUpdates` (real Postgres); live |
| `internal/indexer/backfill.go`, `internal/kafka/replay.go`, `Reload`/`Apply` | Per-generation contracts; dual writes to candidates; ledger-authoritative, create-only topic replay | `TestBackfill_*`, `TestReload_EachGenerationUsesItsOwnContract…`; live rebuild and migration |
| `internal/handler/admin.go` | Backfill gate at READY; no empty-index exemption; `CountMissing` for NP-41; migration certification (fail-closed, re-checked at ACTIVE); publish supersedes | `TestTransitionGeneration_*`, `TestValidation_CountsOnlyGenuinelyUntenantedDocuments`, `TestIndexContracts_OnlyOnePublishedPerScope` (real Postgres) |
| `internal/query/semantic.go`, `internal/handler/semantic.go`, `internal/embedding/`, `search-client` | Semantic/hybrid search, pinned models, filtered k-NN, retrieval evaluation in the caller's tenant | `TestPlanSemantic_*`, `TestSemantic_*`, `TestEvaluateRetrieval_*`, `vector_test.go`; live |
| `internal/hydrator/` | Configured collections, caller envelope, ESR-010 on 404/410 | `TestHydrate_*`; live |
| `internal/idempotency/`, `internal/store/idempotency.go` | Idempotency-Key honoured (identity-context-svc pattern) | middleware tests; `TestIdempotency_*` under RLS as an unprivileged role; live replay and mismatch |
| `internal/kafka/runner.go` | Subscribe waits for the topic; DLQ retried, no commit without it | `TestSubscribe_WaitsForTheTopicToExist`; live |
| `deployments/migrations/000003`, `000004`, `000002` | Embedding pin, evaluations, idempotency keys, ESR-018 index; backfill state, watermark reset, evidence model; 000002 re-runnable | applied twice to a fresh database and to a copy of `search_indexer`; `TestMigration000004_ResetsWallClockWatermarks` |
| `openapi.yaml`, `README.md`, `RUNBOOK.md`, `progress.md`, `context.md` | New routes, fields and status codes; runbook §3a rebuilds and §9 semantic operations | redocly + openapi-spec-validator: valid |
| `deployments/docker-compose.yml` (search-indexer block only) | `SOURCE_SERVICE_URL_OBLIGATION`; `EMBEDDING_PROVIDER_URL` left unset | `docker compose` parses it; the stack was rebuilt and started from it |
| `internal/telemetry/telemetry.go` | Freshness gauges; `bootstrap` series for the backlog and freshness gauges and the ESR-012/018/019 rejections | `/metrics` on the live service |
| `scripts/audit.sh` | Waits for backfill; new section 16 (freshness on responses, checkpoint on activation, ledger stamp, idempotency replay and mismatch, semantic refusal on a lexical scope, rebuild and cutover without resurrection); new gauges in section 5; openapi validated by openapi-spec-validator; Go suite re-run in golang:1.25 with `-race` when host Application Control keeps blocking binaries | Run against the real stack: **51 pass, 1 fail (authorization-svc), 1 skip** |

## Remaining cross-service dependencies

1. **authorization-svc's database is behind its code** (`column pra.book_id does not exist`;
   permission denied on `create_access_decision_log_partition`), so every `/v1/authorize` answers
   503 `store_unavailable`. This service then correctly refuses control-plane writes (ESR-008), and
   `audit.sh` sections 7–12 and 16 cannot run, nor can `seed-demo-rbac.ps1`. **Owner:
   authorization-svc.** Once its migrations are applied, re-seed and re-run `audit.sh` to cover the
   grant-dependent sections end to end.
2. **OD-10: an embedding provider and model.** Until one is chosen and `EMBEDDING_PROVIDER_URL`
   set, semantic contracts cannot build a generation and `/v1/search/semantic` answers ESR-019.
   The provider protocol is in `internal/embedding`. This is the only reason for the two ⚠️ rows.
3. **OD-05: which scopes are R2.** Hydration is wired for `obligation`; any other R2 source type
   needs its `SOURCE_SERVICE_URL_<TYPE>`, and must serve `GET <collection>/{id}` under the caller's
   envelope.
4. **Rebuilds are bounded by Kafka retention.** A record whose only event has aged out cannot be
   replayed, so the generation correctly fails completeness. Rebuilding past retention needs a
   replay from the owning domain service.
5. ✅ **Done 30 Sep.** `search_indexer` was backed up (`pg_dump -Fc`) and upgraded with 000003 and
   000004; the service runs healthy against it in the compose stack.

**Decisions taken** (recorded, revisit if the standard names a rule): under STALE or UNKNOWN,
protected (R1+) scopes block and R0 scopes answer flagged (§8.3). Semantic search fails closed
without a provider rather than falling back to lexical. LAGGING (past half of
`max_lag_seconds`) is surfaced but does not block.

---

# 7/9 — notification-svc (:8133) vs ZS-SVC-Y-001 (NCD-01 … NCD-05)

**Contract extracted from:**
`ZS-SVC-Y-001_Notification_Communication_Delivery_Control_Detailed_Service_Specifications_v1.0.docx`
§2.1 (five canonical services), §3.3 (sent semantics), §3.4 (idempotency), §4.5 and §5.5 (API
surfaces), §6–§8, §10 (APIs, events, reason codes), §14 (NP-01…60), §15 (INV-01…30).
**Code:** `services/notification-svc/`.

> **Status as of 30 September 2026 — CLOSED within service scope: 35 of 37 scored items met, 2
> partial pending services that do not exist yet (PRV, DRC).** Remediated on 30 Sep the way
> service 3 was: the **five-service Y-001 control plane is implemented inside this one service**
> (`internal/ncd`, migrations `000015`–`000021`), next to the legacy register it now also governs.
> Each row keeps its 23 Sep finding as *Was:*. Verified three ways: **45 new Go tests** (40 store
> integration tests run as a `NOSUPERUSER NOBYPASSRLS` role, 2 HTTP-level through the real
> router and middleware, 3 unit), the whole service suite green (392 tests); **57/57 live checks**
> (`scripts/ncd_live_check.py`) against the rebuilt image on the real `notification` database with
> real SMTP (mailpit); and **21/21 Playwright** for the console after the §3.3 change.
> **Live stand-ins, stated:** authorization-svc and identity-context-svc were small local stubs
> for the live run (grant all but one principal; resolve every principal but one); the service's
> own authz client, identity client, envelope, idempotency, RLS and SMTP path were the real ones.

**Framing, kept from the baseline because it explains the starting number.** The `.docx`
specifies a control plane of **five canonical services**; on 23 Sep this was a 6-route in-app +
SMTP notifier. The 23 Sep text scored "34 items" without enumerating them; this section scores the
**37 rows of its own tables** (the 33 NCD/§3 rows plus the 4 cross-cutting rows), as service 4 was
rescored on its table's rows.

## Defects found beyond the audit (fixed 2026-09-30)

None of these appear in the 23 Sep tables — most are in code merged after it (`7ac04fc4`).

| Defect | Effect | Fix |
|---|---|---|
| The legacy send path (`POST /v1/notifications`, its resend, the retry worker) consulted **no suppression list at all** | A hard-bounced, complained or security-held address was mailed again on every send; the `email_suppressions` rows the webhook processor wrote were read only by the ledger pipeline | `ncd.GatedDeliverer` in front of the provider on all three paths — canonical + legacy suppression, fail closed if unreadable (NP-56). Verified live: a held address now concludes `FAILED` with `NCD-010` |
| `POST /v1/notifications/webhooks/{provider}` was **unauthenticated** (and envelope-exempt) | One forged POST could suppress any address in any tenant, or mark a message delivered | HMAC-SHA256 over `<timestamp>.<body>` with a per-provider secret, 5-minute replay window; no secret ⇒ refuse everything (INV-27). Verified live: unsigned → 401 |
| The live `notification` database was on the **pre-merge migration lineage** (its `000005` was our old `event_outbox`; main's `000005`–`000014` were never applied) | The running code referenced tables and columns that did not exist — every template, attempt, resend and suppression write would have failed | Rebuilt (it held **0 rows**) from `000001`–`000021`; grants restored for `app_notification` |
| `Idempotency-Key` demanded by the envelope, **read by nothing** (cross-service finding 2) | A retried resend recorded a second reasoned resend; a retried approval answered 409 because the first had succeeded | `internal/idempotency` (the identity-context/search-indexer pattern, principal-bound fingerprint, crash takeover), `000015`. Verified live: replay returns the first response with `X-Idempotent-Replay: true` |
| The dev RBAC seed granted only `NOTIFICATION_SEND`/`VIEW` | The four other actions the service authorizes — `TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS`, `NOTIFICATION_RESOLVE_OUTCOME` — were held by nobody: every template authoring, approval, suppression and outcome resolution was a 403 on the stack | Added to `NOTIFICATION_FULL` in `seed-demo-rbac.ps1`; maker-checker is enforced by the service (author ≠ approver), not by withholding a grant |
| `openapi.yaml` and `scripts/audit.sh` were **lost in the merge** | No contract artefact for the service | `openapi.yaml` regenerated from the registered routes (76 operations, validated with `openapi-spec-validator`); `scripts/ncd_live_check.py` replaces the audit script |
| Found during the live run, in the new plane: provider-event and provider-message dedupe keyed on the binding **without the tenant** | An event id seen in one tenant silently swallowed the same id in another, reported `DUPLICATE` against a row the caller cannot see under RLS | Keys are `(tenant, binding, id)` (§7.5 "within tenant/provider scope"); regression test, with a negative control that fails on the old index |

## NCD-01 Intent, Template & Content Registry (§4.5)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /communication-intents` | Create draft intent; server assigns id | `POST /v1/communication-intents` (+ `/versions`, `/activate`, `/retire`, `GET`) | ✅ | **FIXED 2026-09-30.** Server UUID + version; purpose class, domain owner, S/U/E dimensions, allowed channels, fallback, marketing/mandatory flags, typed variable contract, attachment contract, approved URL domains. §4.2 rules in code *and* as DB CHECKs (no marketing in security/regulated, a marketing intent is never mandatory). Activation by a second principal (SoD CHECK); an active version is immutable by trigger — a change is a new version. *Was:* ❌ |
| `POST /templates` | Draft version bound to intent/channel/locale | `POST /v1/templates` | ✅ | **FIXED.** Stable `template_id` per variant, monotonic versions, content and schema hashes; content immutable from the moment it is written (trigger), so an approval certifies exactly the bytes sent. *Was:* ❌ |
| `POST /templates/{id}/validate` | Schema, content, security, accessibility, policy | `POST /v1/templates/{id}/validate` | ✅ | **FIXED.** 15 checks, each listed in the report: undeclared variables, CRLF in subject, S2/S3 in subjects (NP-32) and on non-in-app bodies (NP-31), script/event handlers/embeds, external images and tracking pixels, alt text, https + approved-domain links, promotional blocks in non-marketing intents (NP-48), locale format. A failing report keeps the version DRAFT and answers 422. *Was:* ❌ |
| `POST /templates/{id}/approve` | Cannot approve own material change | `/approve` (+ `/reject`) | ✅ | **FIXED.** Needs a passing report; approver ≠ author in code and by DB CHECK. *Was:* ❌ |
| `POST /templates/{id}/publish` | Effective-dated immutable publication | `/publish` (+ `/retire`) | ✅ | **FIXED.** `effective_from` (never in the past) + knowledge time; NP-05 in-place edit refused by the database (verified live). *Was:* ❌ |
| `POST /render-previews` | Side-effect free, synthetic/redacted data | `POST /v1/render-previews` | ✅ | **FIXED.** Synthetic values from the contract by default; any supplied S2/S3 value is redacted; writes nothing (tested); exempt from the idempotency requirement as a query. *Was:* ❌ |
| `GET /intents/{id}/effective` | By transaction time and knowledge time | `GET /v1/intents/{id}/effective?transaction_time&knowledge_time` | ✅ | **FIXED.** Bitemporal for intents and templates; historical sets reconstruct exactly (tested: as-known-then returns the old version); NCD-001 / NCD-002. *Was:* ❌ |

## NCD-02 Recipient, Channel, Preference & Suppression (§5.5)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /recipient-resolution` | Plan id, party/contact refs, endpoint snapshots, locale/time zone | `POST /v1/recipient-resolution` | ✅ | **FIXED.** IAM-verified endpoints from identity-context-svc (provenance `IDENTITY_CONTEXT`), the authenticated inbox (`PLATFORM_INBOX`); addresses hashed and masked in every response; NP-11 free-text endpoint for regulated/mandatory/security only under a controlled exception naming a reviewer ≠ caller; NP-12 cross-tenant refused (`NCD-020`) — no MDM relationship registry exists, so none is authorized. *Was:* ❌ |
| `POST /channel-decision` | Ordered channels, restrictions, quiet-hour timing, evidence requirement, fallback | `POST /v1/channel-decision` | ⚠️ | **FIXED except PRV.** Pure, replayable decision: privacy DENY blocks even mandatory notices (NP-18), INDETERMINATE → review (NP-17), marketing needs a PERMIT (NP-47), §5.4 precedence, mute override only for mandatory policy with evidence (NP-15/16), quiet hours in the recipient's IANA zone with DST (NP-19, TC-08), unknown zone → review (NP-20), evidence class, residency (NP-30). **Open:** the PRV decision is a *reference the calling domain supplies*; with no PRV service, a non-marketing send that supplies none proceeds on the recorded absence. *Was:* ❌ |
| `GET /suppressions` | Purpose/channel-scoped effective facts | `GET /v1/suppressions` (+ `POST /v1/suppressions`, `/lift`) | ✅ | **FIXED.** Canonical table keyed on subject or endpoint **hash** (never the address, §7.3); legacy `email_suppressions` projected in (NP-45); rows never deleted; hard-bounce/complaint/legal/security lifts need a second principal's approval (§7.3). *Was:* ❌ |
| `POST /preferences` | Governed preference; cannot mutate consent | `POST /v1/preferences`, `GET /v1/preferences/me` | ✅ | **FIXED.** Caller's own profile only, versioned with append-only history; quiet hours require an IANA zone; a consent-shaped field is a 400 (unknown fields refused), so NP-60's "disable unsubscribe" has nowhere to go. *Was:* ❌ |
| `POST /revalidate` | Re-evaluate before first submit / after change | `POST /v1/revalidate` | ✅ | **FIXED.** Fresh plan + decision; an unsubmitted job takes the new routes or is cancelled; submitted attempts are never mutated. The final gate also re-checks suppression and preference atomically before every submission (INV-24). *Was:* ❌ |

## NCD-03 Delivery Orchestration (§6)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Durable delivery job | §6.1 job contract | `ncd_delivery_jobs` | ✅ | **FIXED.** Route plan, not_before/expires_at, priority, stream, per-route attempt budget, lease; dispatch runs the atomic final recheck in the transaction that creates it. *Was:* ⚠️ the `notifications` row doubled as the job |
| Attempt state machine | §6.2 | `ncd_attempts` + `ncd_attempt_transition` trigger | ✅ | **FIXED.** Exactly the §6.2 graph, enforced by the database; identity, content hash, render and endpoint snapshot immutable; NP-25 a callback that overtook the API response is not rolled back (verified live). *Was:* ⚠️ PENDING→SENT/FAILED |
| Provider routing | §6.3 certified providers | `ncd_provider_bindings` + health | ✅ | **FIXED.** Channel capability, evidence class, residency, health (automatic circuit breaker with half-open, operator circuit control), cost only among compliant routes, failover only within the certified group (NP-28 verified: an outage defers, never reroutes). Provider catalogue is email (SMTP) + in-app, the OD-01 baseline; SMS/push await OD-03/OD-04. *Was:* ❌ single SMTP path |
| Multi-channel fallback | §6.4 governed new attempt, same communication_id | job route advance | ✅ | **FIXED.** Only if the intent allows it, the next route keeps the evidence class (NP-29) and residency, and nothing is UNKNOWN; otherwise an exception. Verified live: a hard bounce falls back to in-app under the same communication. *Was:* ❌ |
| Rate / abuse / storm | §6.5 | quotas, priority, streams, bulk | ✅ | **FIXED.** Per-tenant/intent/recipient-channel hourly quotas that **defer, never drop** (NP-51); security traffic exempt but bounded per recipient; priority queue (NP-53); storm dedupe by purpose-scoped key; governed bulk sends with audience count + hash, maker-checker at a threshold, dispatch refuses a changed audience (NP-49/50). *Was:* ❌ |
| Cancellation & material change | §6.6 | `/cancel`, `/misdelivery`, corrections | ✅ | **FIXED.** Queued jobs cancelled with evidence; jobs bound to the exact pinned render; corrections are new communications linked `supersedes_communication_id` + reason (`communication.correction.issued`); misdelivery (NP-44) stops sends, security-holds the endpoint, opens an incident, deletes nothing. *Was:* ❌ |

## NCD-04 Evidence, Bounce, Complaint & Suppression (§7)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Evidence normalization | §7.1 | `ncd_delivery_evidence` (append-only) | ✅ | **FIXED.** Every fact carries source, confidence and what it **does not prove**; corrections append with lineage (NP-59); open pixels are LOW-confidence signals, never display or acknowledgment (NP-37/38, verified live). *Was:* ⚠️ `provider_response` only |
| Bounce handling | §7.2 | `POST /v1/provider-events/{binding}` | ✅ | **FIXED.** Authenticated (HMAC, replay window, NP-26), deduplicated (NP-24), binding-scoped; hard bounce → canonical endpoint suppression + remediation exception + fallback; three soft bounces in 72h promote to hard. *Was:* ❌ |
| Complaint handling | §7.2 | same | ✅ | **FIXED.** Complaint → marketing-scoped suppression immediately + review exception. *Was:* ❌ |
| Channel reputation | §7.4 | `GET /v1/reputation`, stream controls | ✅ | **FIXED.** Per tenant × stream × binding; a complaint spike pauses MARKETING, a hard-bounce spike pauses the stream — CRITICAL is never paused, only alerted; paused traffic is deferred (tested). *Was:* ❌ |
| Suppression state | §7.3 | canonical suppressions | ✅ | **FIXED.** Idempotent, effective immediately, cross-provider (NP-45), lifts governed. *Was:* ❌ |

## NCD-05 Regulated Notice & Acknowledgment (§8)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Notice packages | §8.1 | `POST /v1/regulated-notices`, `ncd_regulated_notices` | ✅ | **FIXED.** PDC basis reference, recipient capacity, delivery methods, content-package hash over the pinned renders + attachments, WFC clock, ack requirement; package immutable by trigger; a regulated communication cannot dispatch without one (`NCD-018`); `legal_sufficiency` is always `NOT_DETERMINED_BY_NCD` (§8.5). *Was:* ❌ |
| Acknowledgment requirements | §8.3 | `POST /v1/acknowledgments` | ✅ | **FIXED.** The authenticated recipient only; bound to the exact notice version and the content hash they were shown (NP-40); superseded versions refused; operator-recorded evidence only through maker-checker with an artifact (NP-41). *Was:* ❌ |
| Escalation evidence | §8.2 | deadline sweep | ✅ | **FIXED.** `notice.deadline.at_risk` inside the at-risk window and at expiry; expiry never fabricates an acknowledgment (NP-42); delivery failure escalates the notice. *Was:* ❌ |
| DRC handoff | §8.1 record declaration | `POST /v1/regulated-notices/{id}/record-declaration` | ⚠️ | **FIXED except DRC.** Record requirement tracked (`PENDING` → `DECLARED`, `communication.record.declared`), an overdue declaration escalates (NP-58). **Open:** there is no DRC service to push the package to; the declaration arrives by callback. *Was:* ❌ |

## §3.3 and §3.4 — the non-bypassable rules

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **"Sent" is never overloaded** | §3.3 | precise delivery states everywhere | ✅ | **FIXED.** The plane has no SENT state; claims are separate fields (provider accepted / delivered / opened / acknowledged / `legally_served: NOT_DETERMINED_BY_NCD`). The legacy register now exposes `PROVIDER_ACCEPTED` or `DELIVERED_TO_INBOX` as `status` (column kept as `stored_status`, filter accepts both), and `notification.sent` carries `delivery_state`. Console updated (21/21 Playwright). *Was:* ❌ |
| Stable `communication_id` | §3.4 | `ncd_communications` | ✅ | Unchanged at baseline for the register; the plane's communication carries every job, attempt and fallback (INV-02). |
| Purpose-scoped idempotency key from the business event | §3.4 | `(tenant, idempotency_key)` unique | ✅ | **FIXED.** Key = hash(tenant, entity, intent, source event, recipient); a replayed source event returns the existing communication with `NCD-019` (NP-21, verified live with a different `Idempotency-Key`). *Was:* ⚠️ correlation-only |
| **Durable `attempt_id` per provider submission** | §3.4 | `ncd_attempts` | ✅ | **FIXED.** One immutable row per submission with its own provider idempotency token (sent as `X-Zoiko-Idempotency-Token`); one provider message id per attempt within tenant/provider scope (NP-27 collision → quarantine exception). *Was:* ❌ counter |
| **Timeout after submit → UNKNOWN, reconcile first** | §3.4 | UNKNOWN + trigger | ✅ | **FIXED.** Ambiguous submit → UNKNOWN with a resolution deadline; an attempt left SUBMITTING by a crash becomes UNKNOWN; **the database refuses any new attempt of the communication while one is UNKNOWN** (verified live); resolution only against the original with evidence; past its deadline → human exception. *Was:* ❌ re-sent |
| Resend carries a reason, preserves the chain | §3.4 | `/resend` | ✅ | **FIXED.** Reason mandatory, new job under the same communication, original attempts untouched; high-impact (regulated/mandatory/security) resends go through maker-checker (§11.3). *Was:* ❌ |

## Cross-cutting platform controls (done well)

✅ Tenant isolation via RLS on every table, now **proven as a non-superuser role** (the suite
connects as `NOSUPERUSER NOBYPASSRLS`); cross-tenant discovery only through the SELECT-only
`app.platform_scope` hatch projecting ids; platform binding writes under their own
`app.binding_admin` flag. ✅ Transactional outbox for all ten §10.2 events (`000021`), drained by the
relay (verified live). ✅ Channel enum validated at the request boundary. ✅ `GET
/v1/notifications/templates` requires principal + tenant.

## Compliance

**35 of 37 scored items fully met — 95%** (partials at half: **97%**). *Was:* 4 of 34 — 12% (19%).

**Top gaps by risk** — all six closed within service scope on 2026-09-30:

1. ~~**A post-submit timeout re-sends**~~ — **FIXED.** UNKNOWN is a state, a database trigger forbids
   a second attempt while one exists, and only reconciliation of the original releases it.
2. ~~**`SENT` is exposed as a single generic status**~~ — **FIXED** in the plane, the legacy API, the
   event payload and the console.
3. ~~**No suppression, preference or channel-decision layer**~~ — **FIXED**, and the legacy send path
   is gated by it too (it was not gated by anything).
4. ~~**No bounce or complaint ingestion**~~ — **FIXED**, authenticated; the legacy webhook is now
   authenticated as well.
5. ~~**No template registry, approval or effective-dated publication**~~ — **FIXED.**
6. ~~**No regulated-notice / acknowledgement path**~~ — **FIXED**; DRC handoff waits on DRC.

**Still true, and worth stating: the legacy path is not an intent.** `POST /v1/notifications` and
`POST /v1/notifications/events/ingest` still accept sends that name no communication intent — now
suppression-gated and §3.3-correct, but not purpose-classified. INV-01 ("no production
person-directed message bypasses NCD") holds only once callers move to `POST /v1/communications`;
progress.md (8 Sep) recorded that nothing on the estate sends a notification yet, so this is the
moment to move callers.

## Remaining cross-service dependencies

1. **PRV** — authoritative privacy/marketing permission decisions (closes the channel-decision ⚠️).
2. **DRC** — record declaration of the notice evidence package (closes the DRC-handoff ⚠️).
3. **XIC / secret store** — provider bindings and callback secrets live here as registry rows and
   environment variables until XIC and the vault own them (INV-26 holds: no secret in a row).
4. **MDM** — external-party contacts and authorized cross-tenant relationships (refused until then).
5. **authorization-svc** — the four notification actions now in the dev seed must be granted in
   real deployments.

**Decisions taken where the spec is silent** (OD-06, OD-13, OD-15; revisit when ratified): E4 needs
an E3-capable route plus an NCD-05 acknowledgment; UNKNOWN resolution deadline 30 min; evidence
window 15 min; quotas 5000/tenant, 2000/intent, 20/recipient-channel per hour (security 30/recipient);
bulk approval at 50 recipients or any S3 intent; reputation spike thresholds 0.3% complaints / 5%
hard bounces over ≥20 attempts in 24h; three soft bounces in 72h promote to hard; platform default
quiet window 21:00–08:00 for marketing only.

## Re-audit, 5 October 2026: six defects outside the scored rows, all fixed

The 30 Sep score covered the 37 API and table rows above. A re-audit against the NP-01…60 and
INV-01…30 matrices, and against the legacy code that runs beside the plane, found six defects.
None is in a scored row, so the 95% stands for those rows. Two of the six were proven live
before the fix. Detail and evidence are in `services/notification-svc/progress.md`.

| # | Defect | Spec | Fixed by |
|---|---|---|---|
| 1 | Anonymous `POST /v1/notifications/unsubscribe` wrote suppressions for any tenant/address, and the legacy upsert rewrote HARD_BOUNCE as UNSUBSCRIBE, re-opening transactional and security mail to a dead address (**proven live**) | §7.3, INV-10, NP-13, NP-46 | Sealed AES-GCM unsubscribe token (`internal/unsubscribe`); upsert never weakens a reason |
| 2 | `DELETE /v1/notifications/suppression/{email}`: one-principal hard delete | §7.3 | Route removed; `000022` governed lift for legacy rows, DELETE refused by trigger |
| 3 | 90-day housekeeping purge deleted delivery evidence through FK cascade | §8.4, §9.2, INV-28 | Purge removed; `000023` evidence tables refuse DELETE |
| 4 | Plane marketing mail had no unsubscribe link; legacy link hard-coded a host and the raw address | §11.1, §11.2, §11.4, INV-25 | Sealed RFC 8058 header on both paths; marketing refused if no link can be issued |
| 5 | Plane exported no metrics | §13.1, §13.3 | `notification_ncd_*` counters and DB-sourced backlog gauges; label allow-list |
| 6 | No DKIM; no response to broken sender authentication (**NP-55 proven live after fix**) | §11.1, NP-55 | DKIM signing + DNS monitor that holds email as retryable (`internal/senderauth`) |

**Verified:** all packages green; store suite 128 pass / 1 skip / 0 fail against Postgres 16;
live check 69/69 on three consecutive runs (it was racy before, 53/57 on one run, and its README
and stand-ins are now committed); three negative controls fail as expected (upsert rank, `000023`,
backlog without platform scope).

### Compliance, before and after 5 October

The six defects are scored as six more rows beside the 37 above, so 43 rows in all. Partials
count as half in the weighted column.

| Basis | Before (30 Sep work, re-audited 5 Oct) | After (5 Oct fixes) |
|---|---|---|
| The 37 rows scored on 30 Sep | 35 met, 2 partial: **95%** (97% weighted) | unchanged: **95%** (97% weighted) |
| 37 rows + the 6 defects (43 rows) | 35 met, 2 partial, 6 not met: **81%** (84% weighted) | 41 met, 2 partial: **95%** (98% weighted) |

The 30 Sep 95% was accurate for the rows it scored, but it never looked at the legacy code beside
the plane. The 81% is the honest "before". These percentages are **not** a score against the
whole specification: NP-01…60, INV-01…30 and the §18.2 Definition of Done have not been scored
row by row (gap G-10).

### Remaining gaps

**Blocked on services that do not exist**

| # | Gap | Spec | Effect today |
|---|---|---|---|
| G-1 | **No PRV service.** The privacy and marketing permission is a reference the calling domain supplies. | §5.3, NP-17, NP-47, INV-30 | A non-marketing send that supplies no PRV decision goes ahead on the recorded absence. One of the two partial rows. |
| G-2 | **No DRC service.** The record declaration arrives by callback; nothing receives the evidence package. | §8.1, NP-58, TC-20 | Overdue declarations escalate, but no record is ever declared automatically. The other partial row. Evidence retention (now delete-proof) waits on DRC for a disposition policy. |
| G-3 | **No XIC / vault for provider bindings and secrets.** Bindings are registry rows. Callback, webhook, unsubscribe and DKIM secrets are environment variables. | INV-26, §6.3 | Holds as long as deployments source them from the secret store. Nothing enforces that. |
| G-4 | **No MDM.** No external-party contacts or authorized cross-tenant relationships exist. | §5.2, NP-12, INV-16 | Every cross-tenant recipient is refused (NCD-020). Safe, but external-party notices cannot be sent. |
| G-5 | **No PDC or WFC integration.** Legal basis and deadlines are references supplied by the caller. | §8.1, §8.2 | Notice clocks run on caller-supplied deadlines. Legal sufficiency is always `NOT_DETERMINED_BY_NCD`, which is correct. |

**Inside the estate, outside this service**

| # | Gap | Spec | Effect today |
|---|---|---|---|
| G-6 | **No caller anywhere in the estate.** No service calls notification-svc or publishes events it consumes. | INV-01, §18.2 | No workflow sends a governed notification. The plane is proven but unused. |
| G-7 | **Legacy send path still open.** `POST /v1/notifications` and `/events/ingest` accept free-text subject, body and address with no intent. They are suppression-gated and use precise statuses, but bypass NCD-01 content controls. | INV-01, INV-04, NP-11 | Becomes moot once callers move to `POST /v1/communications` (G-6). The console's send form uses this path. |
| G-8 | **Console covers only the legacy register** (6 routes). There are no screens for intents, template approval, suppressions, preferences, regulated notices, evidence, exceptions or reputation, and no recipient acknowledgment screen. | §8.3, §10.1 | Regulated-notice acknowledgment is API-only. A real recipient has nowhere to acknowledge a notice. |
| G-9 | **authorization-svc grants.** The four notification actions (TEMPLATE_MANAGE/APPROVE, NOTIFICATION_SUPPRESS, NOTIFICATION_RESOLVE_OUTCOME) are in the dev seed only. | — | Real deployments answer 403 until they are granted. |

**In this service**

| # | Gap | Spec | Effect today |
|---|---|---|---|
| G-10 | **Certification matrix not scored row by row.** NP-01…60, INV-01…30 and TC-01…20 have tests and live checks for many items, but no per-row certification table exists. | §14, §15, §16, §21.1 | No defensible percentage against the full spec yet. |
| G-11 | **Channels limited to EMAIL and IN_APP.** No SMS, push or voice. | OD-01, OD-03, OD-04 | Matches the OD-01 baseline. SMS/push await provider decisions. |
| G-12 | **Open exceptions are not in the backlog metrics.** `ncd_exceptions` has no platform-read policy. | §13.1 | Exceptions are visible per tenant (`GET /v1/exceptions`), not as an alertable gauge. |
| G-13 | **No runbook artefacts.** §13.2 lists 10 runbooks; the code supports them (circuit control, stream pause, UNKNOWN resolution, misdelivery), but no written runbooks exist. | §13.2, §18.2 | Operators have the controls but no procedures. |
| G-14 | **Legacy-table RLS not proven as an unprivileged role.** The legacy store tests run as superuser; one isolation test skips for that reason. NCD tables are proven as `NOSUPERUSER NOBYPASSRLS`. | §11.3 | Legacy-table isolation rests on code review, not a test. |
| G-15 | **Spec decisions taken in code, awaiting ratification:** E4 rule, 30-min UNKNOWN deadline, quotas, bulk threshold, reputation thresholds, marketing quiet window (see "Decisions taken" above). | OD-06, OD-13, OD-15 | Revisit when the open decisions close. |
| G-16 | **62 pre-existing Go files are not gofmt-clean.** CI does not check this. | — | Cosmetic. |

---

# 8/9 — delegated-authority-svc (:8136) vs ORG-06 (Role & Delegation)

**Contract extracted from:**
`ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`
§4.6 and its mandatory-controls block.
**Code:** `services/delegated-authority-svc/`.

**Scope note first:** ORG-06 bundles **role assignment and delegation** into one specification.
This service implements the delegation half; role assignment lives in access-control-svc (9/9).
Both numbers are given below.

Implemented: `POST /v1/delegations` · `GET /v1/delegations` · `GET /v1/delegations/{id}` ·
`POST /v1/delegations/{id}/revoke`.
Events: `authority.delegated`, `authority.revoked`, `authority.expired`.

## Segregation of duties — the part that matters most, and it is right

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **Delegation cannot exceed delegator authority** | §4.6 SoD | a synchronous authorization-svc check that the **delegator** holds a GRANTED decision for the exact `action_type` on the target entity — never trusted from the body | ✅ | |
| **Self-approval prohibited** | §4.6 SoD | the delegator is bound to the caller. If the caller is not the delegator, naming **themselves** as delegate is refused as `self_dealing` (403) even with the administer grant; otherwise `DELEGATION_ADMINISTER` is required | ✅ | This closes a genuine privilege-escalation primitive: previously both existing checks passed while a holder of `DELEGATION_CREATE` could name any colleague as delegator and themselves as delegate |
| Delegate ≠ delegator | implied | 400 `delegate_is_delegator` | ✅ | |
| **Cannot delegate around SoD** | §4.6 SoD | no GOV-04 SoD service call anywhere (`sod` = 0 occurrences) | ❌ | Authorization is checked; the SoD conflict engine is not consulted |

## Commands, reads, states, events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| DelegateAuthority | named command | `POST /v1/delegations` | ✅ | |
| RevokeDelegation | named command | `POST /v1/delegations/{id}/revoke` | ✅ | |
| **ExtendDelegation** | named command | — | ❌ | Extending requires revoke + recreate, which breaks the evidence chain §4.6 asks for |
| GetEffectiveDelegations | read surface | `GET /v1/delegations` with a status filter | ⚠️ | An unknown status filter is refused rather than returning empty — the right call on a governance register |
| **ExplainDelegationChain** | read surface | — | ❌ | |
| State `Active` / `Expired` / `Revoked` | lifecycle | all three | ✅ | |
| **State `Proposed`** | lifecycle | — | ❌ | Delegations are born ACTIVE; there is no pre-activation state for the approval §4.6 implies |
| **State `Suspended`** | lifecycle | — | ❌ | |
| DelegationActivated / DelegationRevoked / AssignmentExpired | events | `authority.delegated` / `.revoked` / `.expired`, all via outbox | ✅ | |
| **`AuthorityLimit` write ownership** | "Authoritative write ownership: … AuthorityLimit"; required source input "monetary/quantitative authority limit" | **no such entity** — the only `Limit` in the domain is a paging bound | ❌ | Delegations are action-type-scoped with no monetary ceiling, so "approve payments up to £50k" is not expressible |
| Idempotency scoped by principal + role + scope + validity | §4.6 | idempotent on `(tenant_id, correlation_id)` | ⚠️ | |
| **Duplicate effective overlap prevented** | §4.6 | no overlap constraint, no `EXCLUDE` | ❌ | Two overlapping active delegations of the same action to the same delegate can coexist |
| Ambiguous / expired excluded from authorization; no grace extension | failure semantics | a **cross-tenant background sweeper** (`internal/expiry`, admitted by a named `app.expiry_sweeper` GUC) | ✅ | This fixed a serious defect: expiry used to run only from HTTP handlers scoped to the requesting tenant, so a tenant whose register nobody opened expired **nothing** — grants stayed ACTIVE past their window indefinitely and `authority.expired` never fired |
| `expired_at` semantics | evidence | now `effective_to` (when authority lapsed), with `updated_at` as the observation time | ✅ | Previously `now()`, so a grant that lapsed Friday and was swept Monday claimed it ran all weekend |
| Evidence: delegator, authority basis, scope, valid time, revocation reason | §4.6 | present | ✅ | |
| Evidence: **limits, approval chain** | §4.6 | absent | ❌ | |
| Refused escalations durably evidenced | §4.6 / GOV-07 | `self_dealing` and `delegator_mismatch` exist as a **metric label and a log line only** | ⚠️ | Counted separately on purpose so the rate can be alerted on — but there is no durable evidence row for an attempted escalation |
| Protected changes require authoritative current version | mandatory control | no `expected_version` | ⚠️ | |
| History retrievable / stable event identity | mandatory controls | append-only + outbox | ✅ | |

**Not implemented here because access-control-svc owns them:** `AssignBusinessRole`,
`SuspendAssignment`, `ListPrincipalAssignments`, `GetAssignmentsAsOf`, `RoleAssigned`,
`BusinessRoleAssignment`.

## Compliance

**Delegation half: 23 of 37 scored items met — 62%** (partials at half: **69%**).
**Whole ORG-06 row (including the role half this service does not own): 23 of 43 — 53%.**

**Top gaps by risk**

1. **No SoD engine consultation** (auth). "Cannot delegate around SoD" is a named invariant with
   no implementation; authorization alone cannot see a duties conflict.
2. **No `AuthorityLimit`** (data integrity, business impact). A delegation carries an action but
   no monetary or quantitative ceiling, so the one thing a finance delegation usually needs to
   say cannot be said. ORG-06 names it as authoritative write ownership.
3. **No overlap prevention** (data integrity). Duplicate concurrent grants of the same authority
   are possible; revoking one leaves the other live, which reads as a failed revocation.
4. **No `Proposed` / `Suspended` states and no `ExtendDelegation`** (governance). Every change is
   a revoke-and-recreate, and there is no approval-bound pre-activation state — Doc 03 §9.3 calls
   these chains "approval-bound", but only *authorization* is bound.
5. **Refused escalation attempts leave no durable evidence** (audit) — a metric and a log line,
   both of which age out.
6. No `ExplainDelegationChain` (operability).

This service and search-indexer-svc are the two where the hard invariants are genuinely enforced
rather than described.

## Re-audit and remediation, 5 October 2026

**Re-audit (verification only).** Commit `b9e244fc` (28 Sep) claimed five fixes for this service.
The section above was never updated after it. A re-audit against the `.docx` and the current code
found:

- **One regression.** `DelegateAuthority` (✅ above) returned **503 on every create** in a stack
  wired to the real authorization-svc. The new SoD client posted to `/v1/sod/check`, which nothing
  serves. Its request and response shapes did not match authorization-svc's `/v1/sod/validate`
  either, so fixing only the path would have failed open. Unit tests passed because they injected
  a stub SoD client.
- **Several "fixed" items not closed:**
  - **ExtendDelegation.** Every extend returned 409, because the grant overlapped its own window.
    The outbox CHECK also refused `authority.extended`, and authorization-svc ignored the event.
  - **AuthorityLimit.** Stored, but not checked against the delegator (negative case 11). The
    consumer also dropped the limit fields, so delegated authority was uncapped at decision time.
  - **ExplainDelegationChain.** Returned only the final hop.
  - **PROPOSED / SUSPENDED.** Declared as Go constants only.
  - **expected_version.** Optional, and silently ignored when malformed.
- **One ✅ above was wrong.** "Revocation reason" was never recorded.
- **Deployment gap.** The dev database was still at migration 000004.

On this section's own 23 table rows the 23 Sep state was 11 ✅ / 4 ⚠️ / 8 ❌: **48%** (57%
weighted). The published "23 of 37" could not be reproduced, because the 37 were never listed.

**Remediation (same day).** Every row closed, verified by tests, database checks and a live run.

| Item | Was | Now | How it was closed |
|---|---|---|---|
| Cannot delegate around SoD | ❌ (regressed create to 503) | ✅ | Client speaks `/v1/sod/validate`'s real contract, checks the **delegate's** holdings, and fails closed on an unreadable answer; contract tests run against a fake that serves only real routes |
| DelegateAuthority | ✅ → ❌ | ✅ | Proven live against the real authorization-svc |
| ExtendDelegation | ❌ | ✅ | Overlap check excludes the grant by id; re-checks authority, limit and SoD; refuses a lapsed window; outbox CHECK admits the event; authorization-svc re-projects it |
| GetEffectiveDelegations | ⚠️ | ✅ | `?as_of=` reconstructs from an append-only `delegation_history` |
| ExplainDelegationChain | ❌ | ✅ | Full shortest path, window-checked, cycle-safe |
| States `Proposed` / `Suspended` | ❌ / ❌ | ✅ / ✅ | Activate (maker-checker), suspend and resume, with DB CHECKs and events |
| AuthorityLimit | ❌ | ✅ | Validated; negative case 11 via `/v1/authorize` at the ceiling; authorization-svc stores it and denies amounts or quantities over it on every delegation-based decision |
| Duplicate overlap prevented | ❌ | ✅ | `EXCLUDE` on `[from, to)`, matching expiry and authorization-svc |
| Idempotency scope | ⚠️ | ✅ | Exclusion constraint, plus an `Idempotency-Key` replay store |
| Evidence: limits, approval chain | ❌ | ✅ | Approval who/when/method, creation reason, suspension and revocation reasons |
| Refused escalations durable | ⚠️ | ✅ | Written for 11 named reasons; delegate = delegator no longer mislabelled |
| Protected changes require current version | ⚠️ | ✅ | Required on every transition: 428 without it, 409 on mismatch |
| Revocation reason (was scored ✅) | ❌ in fact | ✅ | Required and recorded |

**Design decisions taken where the specification is open** (they settle the ❓ items):

- A delegator delegating their own authority is the approval, so the grant is ACTIVE at once.
  A grant made on someone else's behalf is PROPOSED until the delegator, or an administrator who
  is neither its maker nor its delegate, approves it.
- Negative case 11 is enforced through authorization-svc's existing authority limits.
- Intervals are half-open `[from, to)`, so back-to-back grants are not an overlap.
- A capped delegation used with no amount stated is denied.

**Verified:**

- **delegated-authority-svc:** 7 packages green. Store suite 24/24 as a non-superuser role (9 new
  DB tests). Handler suite 42/42 (15 new). Client contract tests 5/5.
- **authorization-svc:**
  - Store suite 66 pass / 0 fail (HEAD baseline 65 / 0).
  - Ceiling unit tests 13/13.
  - Consumer tests green, with negative controls.
- **identity-context-svc:** a suspension ends the delegate's sessions; a resume does not.
- **Console:** delegation e2e 9/9.
- **Live:** `scripts/live_lifecycle_check.py` 19/19 against a real authorization-svc. Migrations
  000005–000010 applied to the dev database.

**Compliance now: 23 of 23 table rows — 100% (weighted 100%)**, delegation half of ORG-06. The
role half (`AssignBusinessRole`, `SuspendAssignment`, …) belongs to access-control-svc (9/9).

**Follow-up, 5 Oct 2026: the five items left open above, all closed.**

| # | Was open | Closed by |
|---|---|---|
| 1 | identity-context-svc SoD unwired: it called `/v1/sod/evaluate` (not served), with `SOD_SERVICE_URL` empty, so it used a permit-everything stub | `POST /v1/sod/evaluate` added to authorization-svc (decision: no GOV-04 service exists, and authz owns the SoD rules). It answers CONFLICT for maker = checker, checker = subject, maker = subject, or a maker/checker duty conflicting with the action under `sod_rules`, and 503 (never NO_CONFLICT) on failure. The client now sends the canonical envelope; compose sets `SOD_SERVICE_URL=http://authorization-svc:8089` |
| 2 | Authority limits trusted a caller-supplied `fx_rate` / `exchange_rate` | Reference rates only (decision); an unconvertible pair still fails closed. Regression test: `fx_rate=0.0001` no longer brings 100,000 USD under a 50,000 GBP limit |
| 3 | 41 authorization-svc handler tests failing; the package did not compile on HEAD | Test calls updated to the current `handler.New`. The stub grants the tenant-scope admin permissions the `/v1/admin/*` gate checks, and a new test proves the gate refuses non-administrators (none did before). 239 pass / 0 fail |
| 4 | The new events had not been run through Kafka live | `scripts/live_kafka_projection_check.py` 16/16 through a real broker. Delegation, extension, suspension, resume and revocation all change what `/v1/authorize` answers; the delegation ceiling and reference-only FX hold at decision time; `/v1/sod/evaluate` answers as identity-context calls it |
| 5 | `init-db.sh` migrated only fresh volumes, and nothing recorded what ran | `deployments/migrate.py` (status / baseline / up, `schema_migrations` per database). Baseline detects applied migrations from the objects they create and refuses gaps. `init-db.sh` records into the same table. All 84 dev databases baselined and brought current (88 files across 25 databases; `comments_collaboration` created). A fresh volume initialises: 398 files across 84 databases, all tracked |

**Found while closing item 5:**

- **jurisdiction-rules-svc broke fresh-volume init.** Migrations 000005–000009 granted to
  `jurisdiction_rules_app`, a role nothing creates (the real role is `app_jurisdiction_rules`).
  000006 also used `tsrange` on `timestamptz` columns and needed `btree_gist`. Under
  `set -euo pipefail` either defect stopped Postgres initialisation, so a clean `compose up` could
  not start the stack. Fixed: guarded grants to the real role, `tstzrange`, and the extension.
- **Out-of-order history in three databases, resolved explicitly at baseline:**
  - `policy` and `accounts_receivable` each contain two migrations with the same number; one of
    each pair had never run.
  - `configuration_feature_flag` was really at 000003.

Cross-service finding 4 (SoD unwired) is now closed for all three callers: delegated-authority-svc,
access-control-svc and identity-context-svc.

---

# 9/9 — access-control-svc (:8137) vs the Global Authorization RBAC/ABAC/SoD Standard §9

**Contract extracted from:**
`ZoikoSuite_Global_Authorization_RBAC_ABAC_Segregation_of_Duties_Standard_Detailed_Engineering_Wireframe.docx`
§5 (permission taxonomy), §9 + §9.1 (role architecture, 21 archetypes), §10.1 (static conflict
matrix), §19 (caching / revocation), §20 (evidence).
**Code:** `services/access-control-svc/`.

Implemented: `POST|GET /v1/role-definitions` · `GET|PATCH /v1/role-definitions/{id}` ·
`POST|GET /v1/role-definitions/{id}/permission-bundles` · `GET|PATCH|DELETE …/{bundle_id}` ·
`GET /v1/permission-bundles`.
Events: `role.created`, `role.updated`.

**Ownership finding, before the table.** authorization-svc **already implements**
`/v1/admin/roles`, `/v1/admin/roles/{id}/permission-bundles`, `/v1/admin/role-assignments`,
`/v1/admin/sod-rules`, `/v1/admin/abac-rules` and `/v1/sod/validate`. access-control-svc is a
second, thinner catalogue over the same objects. ❓ **Needs clarification** — the `.docx`
describes one role architecture, not two services sharing it, and a shared object with two write
owners is what produced the bundle-overwrite defect recorded below.

## §9 objects

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **System role template** | "Versioned; customers cannot mutate the canonical template silently" | no template or system-role concept; every role is a tenant row | ❌ | Zero occurrences of `template` or `system_role` |
| Tenant custom role | "Cannot include protected platform-admin permissions" | roles exist; `permitted_actions` is a free `[]string` | ❌ | **No protected-permission guard.** Zero occurrences of `protected` or `platform_admin`. A tenant role may name any action string, including platform-admin actions |
| Access assignment | subject/group + role/policy + scope + effective dates | — (authorization-svc owns it) | ❌ | Not in this service |
| Assignment request | governed; may require manager / data-owner / security approval by risk | — | ❌ | |
| **Assignment review / attestation** | "Periodic or event-triggered attestation; stale/unowned assignments must be removable" | — | ❌ | Zero occurrences of `attestation` |
| **Revocation invalidates decision cache / session claims** | §9 + §19 | `role.updated` is emitted to `zoiko.access-control.events`; identity-context-svc has `handleRoleUpdated` to end sessions — but its Kafka reader is configured with a **single topic, `zoiko.identity.events`** | ❌ | **Verified live, still open.** The event's payload key was fixed (it now carries both `role_id` and `role_definition_id`), but the consumer never subscribes to the producing topic, so retiring a role still does not end the sessions holding it. **Update 2026-09-28: consumer side FIXED in identity-context-svc** (defect R1 of service 1/9) — the reader now subscribes to `zoiko.access-control.events` and the payload key matches. `role.updated` itself was not driven live, so this row is left unscored pending a live check from this service's audit |
| §9.1 21 role archetypes | named baseline | none seeded | ❌ | |
| §5 stable permission taxonomy | "stable business capabilities" | free-form action strings, not validated against capability-registry-svc | ⚠️ | |
| §10.1 static conflict matrix (8 baseline rules) | minimum baseline | not here; the engine **does** exist in authorization-svc (`CheckSoDConflict`, `CheckOwnObjectSoD`, `/v1/sod/validate`) — access-control-svc never calls it | ❌ | A role and a bundle can be defined that combine Payment Preparer with Payment Releaser with nothing objecting |
| §20 evidence / explainability | actor, correlation, version | `created_by_principal_id`, `updated_by_principal_id`, correlation on every row; transactional outbox (migration 000004) with a named `app.outbox_relay` policy disjunct | ✅ | |

## Defects previously found and now verified fixed

✅ The authorization action was renamed from `ACCESS_ROLE_MANAGE` to the estate-wide
`ROLE_MANAGE` — every write used to 403 while every read worked, which reads as an
under-granted operator rather than a broken service.
✅ Events moved from fire-and-forget post-commit to a transactional outbox.
✅ A unique `(tenant, role, bundle_code)` was added — without it, authorization-svc's bundle
attach is an upsert-REPLACE on `(role_id, bundle_code)`, so two local bundles sharing a code
overwrote each other's actions there and detaching either retired both.
✅ Duplicate code and malformed UUID now answer 409 / 400 instead of 503 with a raw SQLSTATE.
✅ A replay answers 200 rather than 201.

## Compliance

**2 of 11 scored items fully met — 18%** (partials at half: **23%**). 1 item needs clarification.

**Top gaps by risk**

1. **Role revocation does not end sessions** (auth). Producer and consumer are on different
   topics. A retired or narrowed role stays live in every held session until it expires — the
   single most consequential open defect in the group, and it spans two services so neither
   one's own audit catches it.
2. **No protected-permission guard** (auth). §9 states plainly that a tenant custom role "cannot
   include protected platform-admin permissions", and nothing enforces it.
3. **No SoD consultation** (SoD). The engine exists next door and is never called.
4. **No system role templates, archetypes, assignment requests or attestation** (governance).
   Access grants cannot be reviewed or attested, so §9's "stale/unowned assignments must be
   removable" has no mechanism.
5. **Duplicate authority with authorization-svc** (architecture) — resolve before building
   further on either.

---

# Group 1 — overall

| # | Service | Spec audited against | Scored items | Full match | Weighted |
|---|---|---|---|---|---|
| 1 | identity-context-svc | GOV-01 §4 | 44 | 42 (**95%**) — was 32 (73%) | **97%** — was 82% |
| 2 | tenant-entity-registry-svc | ORG-02 + ORG-03 | 48 | 47 (**98%**) — was 36 (75%) | **99%** — was 80% |
| 3 | configuration-feature-flag-svc | ZS-SVC-AA-001 | 49 | 47 (**96%**) — was 6 (12%) | **98%** — was 19% |
| 4 | secret-vault-integration-svc | Security Standard §13, §9, §3.1 | 21 | 18 (**86%**) — was 10 of 25 (40%) as stated; 7 of 19 on its table | **93%** — was 52% |
| 5 | gateway-auth-svc | Security §8 + GOV-01 ingress | 22 | 21 (**95%**) — was 15 (68%); 18 at re-audit | **98%** — was 80% |
| 6 | search-indexer-svc | ZS-SVC-AB-001 | 49 | 47 (**96%**) — was 42 of 49 (86%) recounted; published 43 of 48 (90%) | **98%** — was 90%; published 93% |
| 7 | notification-svc | ZS-SVC-Y-001 | 37 (43 incl. 5 Oct defects) | 35 (**95%**) — was 4 of 34 (12%); on 43 rows 81% → 95% after 5 Oct fixes | **97%** — was 19%; 98% on 43 rows |
| 8 | delegated-authority-svc | ORG-06 (delegation half) | 23 (table rows; was 37 unenumerated) | 23 (**100%**) — was 23 of 37 (62%) published; 11 of 23 (48%) recounted 5 Oct | **100%** — was 69%; 57% recounted |
| 9 | access-control-svc | Authorization Standard §9 | 11 | 2 (18%) | 23% |
| | **Group total** | | **304** | **282 — 93%** (5 Oct, service 8 rescored on its 23 rows and remediated; was 282 of 318 — 89%; 251 of 315 — 80%; 247 of 314 — 79%; 241 — 77%; 233 — 73%; 192 — 60%; 171 — 54% at baseline) | **≈95%** (was ≈92%; ≈83%; ≈82%; ≈81%; ≈78%; ≈66%; 61% at baseline) |

21 items scored partial and **9 need clarification** (was 24 before the notification-svc remediation, which also rescored it on its tables' 37 rows rather than the unenumerated 34; 25 before the search-indexer-svc remediation; 29 before the gateway-auth-svc remediation and re-audit; 32 and 11 before the secret-vault-integration-svc remediation; 45 and 17 at baseline, before the re-audits of identity-context-svc and tenant-entity-registry-svc and the remediation of configuration-feature-flag-svc). secret-vault-integration-svc is now scored on its table's 21 rows rather than the unreproducible 25, and search-indexer-svc on its table's 49 rows rather than the 48 its first headline used. The group total is an unweighted item
count across services of very different sizes; the per-service figures are the ones to act on.

Service 7 used to pull the mean down because it implemented one slice of a five-service control
plane. It was resolved on 30 Sep the way service 3 was on 29 Sep: the Y-001 plane implemented inside the
one service (95%). The weakest remaining services are now 8 and 9.

## The four findings that cross service boundaries

1. **The governance envelope is unsanitized end to end.** The edge strips 8 `X-Zoiko-*` routing
   headers and Traefik replaces 8 identity headers. `X-Support-Context-Id`, `X-Purpose-Context`,
   `X-Workload-Id`, `X-Approval-Reference`, `X-Evidence-Refs` and `X-Causation-Id` travel from
   the client untouched — and identity-context-svc stamped the support-context one onto a session
   without verifying it (**fixed there 2026-09-23**; it also now resolves channel and workload
   against the verified principal), while secret-vault-integration-svc authorized its broker against a
   body-supplied workload id (**fixed there 2026-09-29**: body must match the verified actor and
   the client certificate must name every identity header — but without a certificate the actor
   still falls back to `X-Workload-Id`, so the edge must strip it). configuration-feature-flag-svc now also trusts `X-Workload-Id`
   (attestation, SERVICE layer), `X-Commercial-Plan`, `X-Org-Unit-Id` and
   `X-Jurisdiction-Context` (29 Sep) — correct only once the edge sets and strips them. One fix at
   the edge closes several service-level holes at once.

   **Edge side fixed 2026-09-29 (gateway-auth-svc, 5/9), under the split policy.**
   `X-Workload-Id` and `X-Support-Context-Id` are now stripped at the edge and again by
   ForwardAuth. The 8 headers the gateway sets (including `X-Legal-Entity-Id` and
   `X-Jurisdiction-Context`) are overwritten on every tenant host. The 8 §4 caller assertions
   (`X-Purpose-Context`, `X-Approval-Reference`, `X-Evidence-Refs`, `X-Causation-Id`, …) pass
   through by design, for the owning service to validate. **Still open:** `X-Commercial-Plan` and
   `X-Org-Unit-Id`, which configuration-feature-flag-svc trusts, are neither stripped nor set at
   the edge (5/9 dependency 3).

   **search-indexer-svc (6/9), 30 Sep:** its R2 hydrator (added 25 Sep) was a new instance of this
   finding — it asserted `X-Principal-Id: system:search-hydrator` and `X-Workload-Id` on direct
   service-to-service calls that never pass the edge. It now forwards the caller's own envelope and
   asserts no identity (verified live).

2. **`Idempotency-Key` is demanded and never honoured.** The envelope middleware requires it on
   every material write across the estate; no service has a dedupe store. identity-context-svc
   declared `IDEMPOTENCY_MISMATCH` and returned it from nowhere (**fixed there**: dedupe store
   2026-09-23, replay bound to the principal and crash recovery 2026-09-28 — the pattern to copy;
   **also fixed in tenant-entity-registry-svc**, so a replayed tenant provision no longer creates a
   second tenant; **also fixed in search-indexer-svc 2026-09-30** — every command now honours the key,
   verified live and under RLS as an unprivileged role; **also fixed in notification-svc 2026-09-30** — the
   middleware on every write, verified live). Replaying a break-glass grant elsewhere still
   creates a second one.

3. **Maker-checker must be server-side, not self-asserted.** tenant-entity-registry-svc used to
   take `approved_by_principal_id` from the maker's own request body; it now files an approval
   request that a different, authorized principal must release against the fingerprint they
   reviewed (**fixed**, verified live). delegated-authority-svc binds the delegator to the
   verified caller and refuses self-dealing outright. Those are the patterns to copy.

4. **SoD exists but is unwired.** *(5 Oct: delegated-authority-svc now calls the real `/v1/sod/validate` contract, verified live; access-control-svc calls the right path; identity-context-svc now reaches authorization-svc's new `/v1/sod/evaluate` (maker-checker), and `SOD_SERVICE_URL` is set in compose — closed for all three callers.)* authorization-svc implements SoD rules, `CheckSoDConflict`,
   `CheckOwnObjectSoD` and `/v1/sod/validate`. `SOD_SERVICE_URL` is empty in the compose, so
   identity-context-svc falls back to `PermitAllChecker` locally (it correctly refuses to boot in
   staging or production without it), and neither delegated-authority-svc nor access-control-svc
   calls it at all.

## Where to start

Ranked by risk across all nine services:

1. Header sanitation at the edge — **fixed in gateway-auth-svc 2026-09-29** (split policy); `X-Commercial-Plan` / `X-Org-Unit-Id` still open
2. Role-revocation topic mismatch (access-control-svc → identity-context-svc) — **consumer side fixed 2026-09-28**
3. Broker workload identity (secret-vault-integration-svc) — **fixed in service 2026-09-29**; the edge strips `X-Workload-Id` since 2026-09-29 (5/9)
4. Notification post-submit retry duplication — **fixed in notification-svc 2026-09-30** (UNKNOWN state; the database refuses a second attempt while one is UNKNOWN)
5. Idempotency replay protection — **done in identity-context-svc, tenant-entity-registry-svc, search-indexer-svc and notification-svc**; other services still to follow
6. Maker-checker approver verification — **done in tenant-entity-registry-svc**

## Open questions — decisions, not further searching

- **OD-10 — which embedding provider and model?** search-indexer-svc's semantic search is built and
  verified with a stub; until a provider is chosen its two semantic rows stay ⚠️ (6/9).

- ~~Are CFG-01 / CFG-04 / CFG-05 and NCD-01 / NCD-02 / NCD-04 / NCD-05 planned as separate services?~~
  Resolved in practice: both control planes are implemented inside their one service (3/9 on 29 Sep, 7/9 on 30 Sep).
- Which services own PRV, DRC, XIC and MDM? notification-svc consumes all four by reference and has two items
  waiting on PRV and DRC (7/9).
- Are access-control-svc and authorization-svc meant to share role ownership?
- Do the §8 Internet-edge controls (DDoS, WAF, rate limiting, TLS, request-size limits) live in
  the GCP deployment runbook? *Partly answered 29 Sep:* the runbook puts TLS at the GCP L7 load
  balancer. It names no Cloud Armor, bot controls or production request-size limits (5/9).
- Should `X-Commercial-Plan` and `X-Org-Unit-Id` be edge-set server context (stripped from
  clients) or caller assertions? configuration-feature-flag-svc currently trusts both (5/9).
- ~~Is ORG-03's `MergeDuplicateCandidate` reachable, given §1's prohibition on destructive merge?~~
  Resolved: implemented as a non-destructive merge (tenant-entity-registry-svc SPEC_DEVIATIONS.md).
- ~~What do "hard isolation identifiers" (ORG-02) and "sensitive identifiers" (ORG-03) enumerate to?~~
  Resolved: enumerated in tenant-entity-registry-svc SPEC_DEVIATIONS.md.
