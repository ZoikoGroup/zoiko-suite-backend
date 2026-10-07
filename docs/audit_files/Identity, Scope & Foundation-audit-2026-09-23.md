# Group 1 — Identity, Scope & Foundation: documentation-compliance audit

**Date:** 23 September 2026 (identity-context-svc and tenant-entity-registry-svc re-audited 28 September 2026)
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
| 3 | configuration-feature-flag-svc | 8086 | ZS-SVC-AA-001 (CFG-01…05) | 12% | 19% |
| 4 | secret-vault-integration-svc | 8087 | Security Standard §13, §9, §3.1 | 40% | 52% |
| 5 | gateway-auth-svc | 8092 | Security §8 + GOV-01 ingress | 68% | 80% |
| 6 | search-indexer-svc | 8096 | ZS-SVC-AB-001 (ESR-01…05) | **90%** | **93%** |
| 7 | notification-svc | 8133 | ZS-SVC-Y-001 (NCD-01…05) | 12% | 19% |
| 8 | delegated-authority-svc | 8136 | ORG-06 (delegation half) | 62% | 69% |
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

**Contract extracted from:**
`ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`
§4.2, §4.3 and their Mandatory-engineering-controls blocks.
**Code:** `services/tenant-entity-registry-svc/`.

## ORG-02 — Tenant

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| 7 named commands | Create / Activate / Suspend / Resume / InitiateTermination / CompleteTermination / ChangeDefaultLocale | `POST /v1/tenants`, `POST /v1/tenants/{id}/commands/{command}`, `POST /v1/tenants/{id}/defaults` | ✅ | The command name reaches `tenant_lifecycle_history.command_name` and the event payload, so the "no generic path bypasses named governance commands" gate holds |
| 4 read surfaces | GetTenant, ResolveTenantByHost, ListTenantLifecycleHistory, GetTenantDefaults | all four present | ✅ | |
| 5 named events | TenantCreated … TenantTerminated | `tenant.created`, `.activated`, `.suspended`, `.termination.initiated`, `.terminated` | ✅ | Transactional outbox; `tenant.resumed` + `tenant.defaults.changed` are extras |
| Lifecycle states | Provisioning → Active → Suspended → Terminating → Terminated | ONBOARDING → ACTIVE → SUSPENDED → OFFBOARDING → TERMINATED | ✅ | Renaming documented at `internal/domain/enums.go:23` |
| **FailedProvisioning + compensating cleanup** | "Provisioning partial failure remains Provisioning/FailedProvisioning with compensating cleanup" | no such state, no cleanup path | ❌ | A failed provision leaves a tenant in ONBOARDING, indistinguishable from one still in progress |
| **Maker-checker on tenant creation** | "Tenant **creation**/termination and home-region changes require maker-checker" | `RequiresMakerChecker()` returns true for termination only | ❌ | Creation is excluded without comment; suspension's exclusion *is* reasoned (containment action during an incident) |
| Maker-checker on home-region change | required | no home-region change command exists | ❌ | The command is named only in the SoD row, not in "Named commands" — but the control is unimplementable as built |
| Maker-checker on termination | required | non-empty approver + `!= actor`, plus a DB `CHECK` | ⚠️ | See top gap 1 — the approver is self-asserted |
| **Idempotency** | "Create by approved onboarding correlation/external customer key" | none | ❌ | No dedupe key on `ProvisionTenant`; a retried onboarding creates a second tenant |
| expected_version | "lifecycle commands use expected_version" | supported; `0` means "no expectation" and is accepted | ⚠️ | Honest, documented drift — the mechanism exists but is optional |
| Authorization | platform provisioning / admin permissions | `authorize(ctx, "tenant", <action>)` per command, fail-closed on an authz outage | ✅ | |
| Tenant admin cannot change hard isolation identifiers | required | no explicit guard found | ❓ | Which fields count as "hard isolation identifiers" is not enumerated in the doc |
| Evidence / lineage | onboarding request, approval, home-region decision, configuration version, lifecycle actor/reason | actor, reason, approver, command name, version | ⚠️ | Onboarding-request and home-region-decision references absent |
| Acceptance: cross-tenant resolution impossible / suspended denied / termination retains records | required | RLS + lifecycle gate + no-hard-delete doctrine | ✅ | |

## ORG-03 — Legal Entity

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| 6 named commands | Create / AmendLegalProfile / ChangeRegisteredOffice / ChangeLegalName / Deactivate / **MergeDuplicateCandidate** | first five present; merge is not | ❓ | Deliberate: §1 says "destructive merge is prohibited", so the service records a conflict *conclusion* and merges nothing. §4.3 names the command, §1 forbids the act, and the doc never defines a non-destructive merge |
| 4 read surfaces | GetLegalEntity, FindByRegistryNumber, ListEntityVersions, GetLegalEntityAsOf | all four present | ✅ | |
| 4 named events | Created / ProfileAmended / StatusChanged / RegisteredOfficeChanged | `entity.created`, `entity.profile.amended`, `entity.status.changed`, `entity.registered_office.changed` | ✅ | `entity.legal_name.changed` is an extra narrowing |
| **Lifecycle states** | Draft → Verified → Active → Inactive/Dissolved | ACTIVE, DORMANT, SUSPENDED, DISSOLVED | ❌ | **No DRAFT and no VERIFIED.** `CreateLegalEntity` admits an entity straight to ACTIVE — the verification gate the spec puts before activation does not exist |
| Profile versions effective-dated | required | bitemporal: `effective_from/to` + `recorded_at`/`superseded_at`, `version_number` unique per entity | ✅ | |
| SoD: maker cannot approve legal-name / registry / jurisdiction change | required | enforced in `AmendLegalProfile` | ⚠️ | Same self-asserted-approver weakness |
| **SoD: no self-approval of merge** | required | `ResolveRegistryConflict` has **no approver field and no SoD check** | ❌ | Only an authz action and a mandatory resolution note |
| Registry collision quarantined | required | `registry_conflicts` + `entity.registry_conflict.quarantined` | ✅ | |
| Historical profile never overwritten / as-of exact | required | append-only versions, `GET /entities/{id}/as-of` | ✅ | |
| **LEI as external organizational identifier with source/status** | mandatory control | zero occurrences in the service | ❌ | |
| ISO 20275 / ELF code preserving local legal-form text | mandatory control | `legal_form_code`, `legal_form_source`, `legal_form_local_text` | ✅ | |
| Evidence / lineage | source refs, verified fields, legal-form code/source, effective/recorded time, approver | all columns present | ✅ | |
| Sensitive identifier access scoped | "sensitive identifier access scoped" | not distinguished from ordinary reads | ❓ | The doc never names which identifiers are sensitive |

## Surface beyond ORG-02 / ORG-03

Workspaces (4 routes), entity hierarchies (3), entity–jurisdiction assignments (3), residency
policies (2), residency regions (2), tax-identity bundles (4), tenant host bindings (2). These
plausibly belong to ORG-05, ORG-08/09, GOV-02 and TAX — ❓ ownership needs confirmation against
those sections; they are not in the two specs this service cites.

## Compliance

**36 of 48 scored items fully met — 75%** (partials at half: **80%**). 4 items need clarification.

**Top gaps by risk**

1. **Maker-checker is nominal across the whole service** (auth / SoD).
   `approved_by_principal_id` is a free string in the maker's own request body. Nothing checks
   that the named approver exists, is a principal in that tenant, holds approval authority, or
   ever saw the request. The only test is string inequality with the actor — in the service
   *and* in the DB `CHECK`. Every "independently approved" control in ORG-02 and ORG-03 is
   satisfiable by one person typing a second name.
2. **Tenant creation has no maker-checker at all** (SoD), though §4.2 names creation first.
3. **Legal entities have no Draft / Verified states** (data integrity). Entities are created
   active, so "material changes independently approved" has no pre-activation gate to hang on.
4. **Conflict resolution has no SoD** (SoD) — the one decision §4.3 singles out for "no
   self-approval of merge".
5. **No onboarding-key idempotency** (data integrity) — retried provisioning duplicates tenants.
6. **No LEI storage** and **no FailedProvisioning state** (contract gaps).

One cross-cutting note, not scored: authz actions here are lowercase dotted
(`tenant` / `lifecycle.transition`), while access-control-svc grants uppercase underscored
names. Worth one `access_decision_log` query against a live stack to confirm these actions are
actually grantable — that is exactly how the access-control-svc defect presented.

## Re-audit — 28 September 2026

Re-scored against the `.docx` §4.2, §4.3 and their mandatory-controls blocks, item by item, **on
a live stack**: the service on :8081 running as `zoiko_app`, a real authorization-svc issuing
real decisions (two temporary test principals, a maker and a checker, revoked afterwards), and
a real jurisdiction-rules-svc. Every ✅ below that says "live" was exercised by HTTP against
the running service, not inferred from the code.

The 23 Sep list was not preserved item by item, so this re-audit uses an explicit list of 48
items, one per spec field or acceptance/control clause. The count matches; the items are
written out here so the next audit can re-score the same list.

### Defects found by this re-audit — all fixed

Six, and none of them was visible to the unit tests, because the unit tests use an in-memory
store and a context with no tenant, and the database tests were skipping for want of a
`TEST_DATABASE_URL`.

1. **No tenant could be created with a full envelope** (500 on every `POST /v1/tenants`). The
   store scoped RLS to the caller's `X-Tenant-Id` (always set: the envelope makes it mandatory)
   rather than to the new tenant's own id, and the `tenants` WITH CHECK refused the row.
   `internal/registry/service.go`.
2. **Tenant provisioning was authorized in the caller's tenant, not the platform scope.** A
   tenant admin granted `TENANT_PROVISION` in their own tenant could create tenants. §4.2 makes
   it a platform permission. Same file.
3. **An amendment effective before an entity's first version left two open-ended profile
   versions**, so as-of "now" returned the *old* legal name while `GetEntity` returned the new
   one — §8 NP6 and "as-of reconstruction exact" broken. `internal/store/pg_store_org.go`.
4. **Two amendments at the same effective instant answered 500** (`lepv_interval_ordered`): the
   store closed the prior version at its own start. Same file — now superseded in record time.
5. **Every direct-to-Kafka event was racing the response and usually lost.** All eleven
   `go s.events.PublishX(ctx, …)` calls passed the *request* context, cancelled when the handler
   returned; `entity.created` was dropped live with `context canceled`. `internal/events/publisher.go`.
6. **§8 NP3 was enforced on 2 of 46 routes.** A request on tenant A's hostname claiming tenant B
   was served in full on every other route (`GET /v1/tenants/{B}` → 200). Now a `/v1`
   middleware. `internal/handler/handler.go`.

And one long-standing gap closed rather than a defect: **`Idempotency-Key` is now honoured**
(migration 000010, `internal/idempotency/`). Before, one key sent twice ran the command twice
— verified live: two `ChangeDefaultLocale` history rows, two version bumps.

Each fix has a regression test that fails on the old code (verified by running it against the
old code) and passes on the new.

### ORG-02 — Tenant (25 items)

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | Write ownership: Tenant, lifecycle, home-region ref, default-config ref | ✅ | Home region is the default residency-policy pointer |
| 2 | Non-ownership respected (billing, authz, residency decision) | ✅ | |
| 3 | Required inputs incl. primary jurisdiction and residency preference | ⚠️ | Neither is a provisioning input; residency is assigned after creation |
| 4 | Server-resolved context: regions, plan entitlement, uniqueness, onboarding policy, restricted jurisdictions | ⚠️ | Uniqueness only; no COM entitlement or restricted-jurisdiction check |
| 5 | 7 named commands | ✅ live | All seven driven |
| 6 | 4 read surfaces | ✅ live | |
| 7 | Lifecycle incl. FailedProvisioning | ✅ | Tests; states live |
| 8 | Platform provisioning permission | ✅ live | **Fixed today** (defect 2) |
| 9 | Tenant admin cannot change hard isolation identifiers | ✅ | Per SPEC_DEVIATIONS enumeration |
| 10 | Maker-checker on creation | ✅ live | Activate before approval → 422; maker self-approve → 403; wrong fingerprint → 409; checker → 200; twice → 409 |
| 11 | Maker-checker on termination | ✅ live | 202 + approval request; rejected by checker |
| 12 | Maker-checker on home-region change | ❌ | No command exists; doc decision pending (owner set aside) |
| 13 | 5 named events | ✅ live | Outbox rows published, 0 failed attempts |
| 14 | Create by onboarding key | ✅ live | No key → 422; replay → 200 same tenant; same key, different body → 409 |
| 15 | Lifecycle commands use expected_version | ⚠️ | Honoured when sent (stale → 409 live), optional on the wire — documented |
| 16 | FailedProvisioning + compensating cleanup | ✅ | Tests (retry, abandon under maker-checker) |
| 17 | Evidence: onboarding request, approval, home-region decision, config version, actor/reason | ⚠️ | All but the home-region decision |
| 18 | Acceptance: cross-tenant resolution impossible | ✅ live | NP3 403 on every route (**fixed today**, defect 6); cross-tenant read → 404 |
| 19 | Acceptance: suspended tenant denied | ✅ live | Write → 409, read → 200 |
| 20 | Acceptance: partially provisioned tenant not active | ✅ | Activation gated on creation approval (live) |
| 21 | Acceptance: termination preserves records | ✅ | No hard-delete path |
| 22 | Control: protected changes need current version + validated scope | ✅ live | |
| 23 | Control: historical versions retrievable | ✅ live | Lifecycle history |
| 24 | Control: events carry stable object/version identity | ⚠️ | Payloads carry `previous_version` but not the new `object_version`, and `emitted_at` but no `recorded_at` (§7 minimum payload) |
| 25 | Control: evidence, actor, reason, approval chain on high-risk changes | ✅ live | |

### ORG-03 — Legal Entity (23 items)

| # | Item | Status | Evidence |
|---|---|---|---|
| 26 | Write ownership: entity, profile version, legal form, formation | ✅ | |
| 27 | Required inputs | ✅ | |
| 28 | Server-resolved: jurisdiction validity, ISO 20275 mapping, duplicates, calendar/currency validity | ⚠️ | Jurisdiction validated live (real service) and duplicates quarantined; ELF code and fiscal-calendar id are accepted unvalidated |
| 29 | 6 named commands incl. MergeDuplicateCandidate | ✅ live | Merge non-destructive; unmerge restores |
| 30 | 4 read surfaces | ✅ live | |
| 31 | Draft → Verified → Active → Inactive/Dissolved | ✅ live | DRAFT activate → 422; DRAFT transact → 409; no evidence → 400 |
| 32 | Profile versions effective-dated | ✅ live | **Fixed today** (defects 3, 4) |
| 33 | Master administration permission | ✅ live | |
| 34 | Sensitive identifier access scoped | ✅ live | RESTRICTED bundle: no purpose → 403; with purpose → 200 |
| 35 | Material changes independently approved | ✅ live | |
| 36 | SoD: legal-name / registry / jurisdiction | ✅ live | Rename 202 → checker approves |
| 37 | SoD: no self-approval of merge (and of conflict resolution) | ✅ live | Both → 403 for the maker |
| 38 | 4 named events | ✅ live | |
| 39 | Registry + jurisdiction is a dedup signal, not an identifier | ✅ | |
| 40 | Commands use UUID and expected_version | ⚠️ | As item 15 |
| 41 | Conflicting registry identity quarantined | ✅ live | 409 naming the incumbent; conflict row; resolves once |
| 42 | Historical profile never overwritten | ✅ live | |
| 43 | Evidence: source refs, verified fields, legal-form code/source, times, approver | ✅ live | |
| 44 | Acceptance: collision quarantined, history preserved, changes approved, as-of exact | ✅ live | As-of before rename → original name; as-of now = GetEntity |
| 45 | Control: identity changes effective-dated and evidence-backed | ✅ live | |
| 46 | Control: LEI with source/status, not a key | ✅ live | Bad check digits → 400; valid → approval → stored |
| 47 | Control: ISO 20275 / ELF preserving local text | ✅ | |
| 48 | Control: dissolution deletes nothing | ✅ | |

### Compliance (28 September 2026)

**40 of 48 fully met — 83%** (was 36 — 75%). Partials at half: **91%** (was 80%).
7 partial, 1 missing, 0 needing clarification (was 4).

Beyond §4.2/§4.3, against the rest of the document that binds this service:

| Section | Applicable | ✅ | ⚠️ | ❌ | Notes |
|---|---|---|---|---|---|
| §3 shared contract | 10 | 7 | 2 | 1 | ❌ **stable typed errors** (`VERSION_CONFLICT`, `SOD_DENIED`, …) — bodies are free text; ⚠️ events (item 24), privacy (no field masking beyond tax bundles). Idempotency now ✅ |
| §8 negative paths NP3–NP6 | 4 | 4 | 0 | 0 | All four live |
| §9.2 Definition of Done | 7 | 2 | 3 | 2 | ✅ gates 3, 4. ⚠️ 1 (no generated-client validation), 2 (older write paths still publish directly, not via outbox), 8. ❌ 6 (no consumer run against the events), 7 (runbooks, SLOs, alerts, backup/restore). Gate 5 not applicable |

**Whole document: 53 of 69 fully met — 77%; weighted 86%.**

**Remaining gaps, by risk**

1. **Stable typed error codes** (§3) — clients must parse free text; a stale version reads
   `conflict: resource already exists: …`, which is wrong as well as untyped.
2. **Event payloads lack the new object version and recorded time** (§7) — a consumer cannot
   pin the version it was told about.
3. **Older write paths publish directly to Kafka, outside the transaction** (§9.2 gate 2) — now
   no longer racing the request, but still able to lose an event on a crash after commit.
4. **Home-region maker-checker** — needs the doc to name a command.
5. **Operational certification** (§9.2 gates 6, 7) — not code.

`scripts/audit.sh` now runs **58 live checks, 58 passing** (was 37), including the 24 Sep work
it did not cover and the 28 Sep fixes. `go test ./...` against Postgres 16: all 9 packages
pass, 214 top-level tests, 0 skipped.

### Gap closure — 28 September 2026 (second pass)

Every ⚠️ and ❌ above was worked the same day and re-verified live on :8081
(temporary maker/checker grants, revoked afterwards). Items that changed:

| # | Item | Was | Now | Evidence |
|---|---|---|---|---|
| 3 | Required inputs incl. primary jurisdiction and residency preference | ⚠️ | ✅ live | required at CreateTenant (000013); missing → `VALIDATION_FAILED` / `SOURCE_UNVERIFIED` |
| 4 | Server-resolved context | ⚠️ | ✅ | region active (live), restricted list, plan entitlement via commercial-account-svc (HTTP client tested against a real server; the local stack runs the stub because that service is not started), uniqueness, onboarding key |
| 12 | Maker-checker on home-region change | ❌ | ✅ live | `ChangeHomeRegion` (000012): platform authority, maker self-approve → `SOD_DENIED`, checker → applied |
| 15 | expected_version on lifecycle commands | ⚠️ | ✅ live | missing → 400, stale → `VERSION_CONFLICT` |
| 17 | Evidence incl. home-region decision | ⚠️ | ✅ live | `home_region_decision_ref` on lineage; constraint `tlh_home_region_evidenced` |
| 24 | Events carry object/version identity | ⚠️ | ✅ live | §7 fields on every envelope; read back from Kafka |
| 28 | Server-resolved: jurisdiction, ISO 20275, duplicates, calendar/currency | ⚠️ | ⚠️ | jurisdiction now refuses retired ones; ELF format + source + local text enforced; **fiscal calendar is format-checked only — REF-04 does not exist in the estate** |
| 40 | Commands use UUID and expected_version | ⚠️ | ✅ live | as 15 |

**ORG-02 + ORG-03: 47 of 48 fully met — 98%** (partials at half: **99%**). 1 partial,
0 missing.

| Section | Applicable | ✅ | ⚠️ | ❌ | Change |
|---|---|---|---|---|---|
| §3 shared contract | 10 | 10 | 0 | 0 | typed errors, event versions, purpose limitation on quarantine and approval reads |
| §8 NP3–NP6 | 4 | 4 | 0 | 0 | — |
| §9.2 DoD | 7 | 5 | 2 | 0 | gate 1 (kin-openapi validation, live body validation, breaking-change gate), gate 2 (no direct publish path), gate 8 (govulncheck clean) now ✅; gates 6 and 7 ⚠️ |

**Whole document: 66 of 69 fully met — 96%; weighted 98%.**

What is left is outside this service's code: REF-04 (item 28); dependent
services pinning the published versions (gate 6 — identity-context-svc verified
consuming an outbox-delivered `entity.updated` live); and production
certification of recovery (gate 7 — runbook, SLOs, six promtool-valid alert
rules and a 22/22 restore drill exist, measured locally).

Nine further defects surfaced while closing the gaps and were fixed — listed in
the service's `RELEASE_CERTIFICATE.md`. `scripts/audit.sh`: **66/66**. Tests:
**253** pass, 0 fail.

### Backend status — no open gaps in this service (28 September 2026)

Re-verified after the gap closure: `scripts/audit.sh` 66/66 against :8081,
`go build` and `go vet` clean, 253 tests passing. **Every item that this
service's backend can satisfy is met.** The three items not scored ✅ are each
blocked on something outside this service, and none needs a change here:

| Item | Why it is not ✅ | Owner of the fix |
|---|---|---|
| 28 — fiscal-calendar reference validity (§4.3) | REF-04 Fiscal Calendar does not exist in the estate; the id is format-checked, and there is nothing to resolve it against | REF-04 service (not built) |
| §9.2 gate 6 — dependents consume pinned versions | this service publishes `object_id`/`object_version` on every event; each consuming service must persist them. identity-context-svc verified live | each consuming service |
| §9.2 gate 7 — production certification | runbook, SLOs, six alert rules and a 22/22 restore drill exist; RPO/RTO must be re-measured on the production database | platform / operations |

| Scope | Score |
|---|---|
| This service's backend (items it can satisfy on its own) | **100%** — 66 of 66 |
| Spec as written, ORG-02 + ORG-03 (48 items) | 98% full, 99% weighted |
| Spec as written, whole document (69 items) | 96% full, 98% weighted |

Frontend adoption of the 14 deliberate contract changes (SPEC_DEVIATIONS.md,
`scripts/contract_gate.sh`) is separate work and not scored here.

---

# 3/9 — configuration-feature-flag-svc (:8086) vs ZS-SVC-AA-001

**Contract extracted from:**
`ZS-SVC-AA-001_Platform_Configuration_Feature_Flag_Environment_Change_Control_Detailed_Service_Specifications_v1.0.docx`
— §0.1 canonical services, §3 (30 invariants), §10.1 (11 canonical APIs), §10.2 (8 events),
§14 (30 negative paths).
**Code:** `services/configuration-feature-flag-svc/`.

**Framing first, because it changes how you read the numbers.** AA-001 specifies a control
plane of **five canonical services** — CFG-01 schema/version registry, CFG-02
resolution/override, CFG-03 flag/release/experiment, CFG-04
environment/secret-reference/promotion, CFG-05 change/rollback/drift/emergency. The implemented
service is a single effective-dated key→value store with 6 routes and 1 event. Its own
`openapi.yaml` cites `03-microservices.md §9.6`, which is a **two-sentence paragraph**, not
AA-001. So this is a scope gap between two documentation layers, not careless implementation —
the service is internally coherent and (per its own 146-check audit) complete against what it
was built to.

## Canonical APIs (§10.1)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /config/definitions` | Create draft ConfigDefinition; schema, owner, scope and type validation; idempotency key | — | ❌ | No definition registry exists; keys are free-form strings |
| `POST /config/definitions/{id}/publish` | Publish immutable version; approval binding for S2/S3; digest/signature; no in-place edit | — | ❌ | |
| `PUT /config/overrides/{scope}` | Effective-dated override set; scope allowlist; tenant isolation; impact/change classification | `POST /v1/config` | ⚠️ | Writes an effective-dated value at global-per-environment or tenant scope. No scope allowlist, no impact/change classification |
| `POST /config/resolve` | Resolve keys for trusted context; deterministic precedence; snapshot lineage | `GET /v1/config/{key}` | ⚠️ | One key at a time; no lineage, no snapshot identity, no reason codes |
| `GET /config/snapshots/{id}` | Fetch immutable signed snapshot | — | ❌ | Zero occurrences of "snapshot" in the service |
| `POST /flags` | Create draft flag: class, owner, variants, fallback, expiry for temporary flags | `POST /v1/flags` | ⚠️ | A flag is `{enabled bool, rollout_percentage int}`. No class, variants, fallback or expiry |
| `POST /flags/{id}/release-plans` | Rollout/targeting plan; eligibility first; deterministic buckets | — | ❌ | `rollout_percentage` is stored but never evaluated — there is no evaluation endpoint, so bucketing is left to each caller, which cannot be deterministic across services |
| `POST /changes` | Atomic ChangeSet with before/after, validation, approvals, rollout and rollback | — | ❌ | |
| `POST /changes/{id}/activate` | Effective time, authorization, idempotency, monotonic version | — | ❌ | |
| `POST /emergency-changes` | Break-glass change; restricted actors/keys, TTL, incident reference, retrospective required | — | ❌ | |
| `POST /runtime/attest` | Report observed snapshot/version/hash; workload identity; anti-replay; drift correlation | — | ❌ | |

**0 of 11 implemented as specified; 3 have partial equivalents.**

## Events (§10.2)

All eight — `config.version.published`, `config.override.activated`,
`config.snapshot.published`, `flag.release.activated`, `flag.kill_switch.activated`,
`config.change.verified`, `config.drift.detected`, `config.emergency.expired` — are
❌ **missing**. The service emits exactly one event, `config.updated`, which appears nowhere in
the spec (undocumented surface). It does go through a transactional outbox with a relay, so the
delivery mechanism is sound; only the contract is absent.

## Invariants (§3) — 30 scored

| Status | Count | Invariants |
|---|---|---|
| ✅ | 6 | INV-01 (a configuration value grants no IAM authorization), INV-03 (no PDC/PRV override), INV-04 (published versions immutable — new row plus `effective_to = NOW()`, no in-place value edit), INV-14 (attributable actor + version history), INV-25 (a tenant override cannot cross tenants — RLS plus a 403 on a foreign `?tenant_id=`), INV-30 (history never rewritten) |
| ⚠️ | 4 | INV-02 (no COM entitlement check — left to consumers), INV-07 (precedence is deterministic but hardcoded tenant-over-global; the spec names **five** layers — environment, service, tenant, organizational unit, user preference — and only two exist), INV-09 (nothing prevents a secret being written as a config value), INV-28 (the service fails closed with 503, but **no per-key declared fallback exists**, so a consumer has nothing to fall back *to*) |
| ❌ | 20 | INV-05 unknown keys accepted · INV-06 no type/owner/scope/fallback/safety-class declaration · INV-08 no scope allowlist · INV-10 no secret references · INV-11 no promotion boundary · **INV-12 runtime reads mutable admin rows as execution truth — exactly what the invariant forbids** · INV-13 no staleness model · INV-15 no emergency expiry/retrospective · INV-16 no kill switch · INV-17 no rollback · INV-18 no targeting controls · INV-19 no experiment restrictions · INV-20 no owner/expiry on temporary flags · INV-21 no retirement model · INV-22 no monotonic distribution · INV-23 / INV-24 no drift detection or bounded remediation · INV-26 no residency evaluation · INV-27 no cache version/freshness metadata · INV-29 no evaluation evidence |

## Negative-path matrix (§14)

Of the 30 certified scenarios, the service can demonstrably satisfy **NP-06** (tenant A's
override appearing in tenant B's resolution — blocked by RLS). **NP-01** (tenant overrides a
PLATFORM_ONLY safety key), **NP-02** (unknown key, invented default), **NP-03** (boolean key
published with a string value), **NP-05** (two equal-precedence overrides for the same
scope/time), **NP-07** (user preference modifies an authority-bearing S2 key), **NP-19**
(temporary flag with no owner or expiry) and **NP-20** (retired flag key immediately reused) are
**unsatisfiable as built** — each depends on a key declaration, safety class or retirement model
that does not exist. `Value` is `json.RawMessage` with no schema, so NP-03 passes silently by
construction.

## What the service does do well

Writes are authorized against authorization-svc and fail closed (503) on an authz outage;
tenant scope is taken from the gateway-verified header, and a body claiming a foreign tenant
gets 403; versions are append-only with a partial unique index, and a lost first-write race is
correctly reported as 409 rather than 503; events go through a transactional outbox. Status
codes are coherent: 200 / 201 / 400 / 401 / 403 / 404 / 409 / 413 / 503.

## Compliance

**6 of 49 scored items fully met — 12%** (partials at half: **19%**). No items need
clarification; the spec is unusually precise.

**Top gaps by risk**

1. **INV-12 is violated structurally** (data integrity). Runtime evaluation reads the current
   mutable row. The spec's entire snapshot/epoch/digest apparatus exists so an execution
   decision can be reproduced later; none of it is present, so **INV-29 evaluation evidence is
   unreachable too** — you cannot reconstruct why a value was selected at a past moment.
2. **No key declaration** (data integrity, breadth). INV-05, INV-06, INV-08 and
   NP-01/02/03/05/07 all fall out of this one missing entity. Any service can invent any key,
   at any type, in any scope.
3. **No kill switch, rollback, or emergency change** (operational safety). §0's stated reason
   for the control plane's existence is that a bad value must be *reversible*; there is no
   reversal mechanism beyond writing another value.
4. **No drift detection or runtime attestation** (operational). A fleet running stale config is
   undetectable.
5. **`rollout_percentage` is stored but never evaluated** (correctness). Each consumer must
   bucket independently, so two services will disagree about whether the same principal is in
   the rollout — the opposite of INV-07's determinism.
6. **No secret-reference or promotion boundary** (security) — INV-09/10/11 rest on capabilities
   that do not exist.

**Recommendation before acting on this one:** confirm whether AA-001 was ever intended as this
service's contract, or whether CFG-01/04/05 are planned as separate services. That decision
changes this from "a service at 12%" to "one of five services, sized correctly, with CFG-02 and
CFG-03 partially built".

---

# 4/9 — secret-vault-integration-svc (:8087) vs the Security / Privacy / Cryptographic Architecture Standard

**Contract extracted from:**
`ZoikoSuite_Security_Privacy_Residency_Cryptographic_Architecture_Standard_Detailed_Engineering_Wireframe.docx`
§3.1 (25 SEC invariants), §5 classification, §9 workload identity, §13 secrets & credential
management, §16 secure logging.
**Code:** `services/secret-vault-integration-svc/`.

**Caveat on the source of truth:** §13 is seven lines of prose. This `.docx` defines **no API
surface** for a secret broker — no named commands, no events, no negative-path matrix. The
service's own `openapi.yaml` cites `03-microservices.md §9.5` and `05-security.md §3.7-3.9`,
which are markdown. So the 12 implemented routes cannot be scored as "documented" or
"undocumented" against the `.docx`; the service has been audited against the **controls** the
`.docx` does mandate.

## Implemented surface

`POST /v1/secret-policies` · `GET /v1/secret-policies` · `POST …/{id}/versions` ·
`POST …/{id}/versions/{vid}/activate` · `GET …/{id}/versions` · `POST …/{id}/rotate` ·
`POST …/{id}/material` · `POST /v1/secrets/broker` · `GET /v1/secrets/leases/{id}` ·
`GET /v1/secrets/leases` · `POST /v1/secrets/leases/{id}/revoke` · `GET /v1/secrets/audit`.

## Controls audit

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| No secrets in logs / DB / events (§13, SEC-INV-09, §16) | mandatory | Postgres holds **metadata only**; material lives behind `VaultBackend`; the broker returns an opaque lease token, never a value | ✅ | Cleanly separated — this is the service's strongest property |
| Data classification (§5) | PUBLIC / INTERNAL / CONFIDENTIAL / RESTRICTED | validated on policy creation, 400 otherwise | ✅ | |
| Deny by default (SEC-INV-01) | mandatory | 6 admin routes call authorization-svc; the broker denies by absence of policy | ✅ | |
| Authoritative tenant binding (SEC-INV-02) | mandatory | gateway-verified tenant; a body claiming a foreign tenant → 403 | ✅ | Fixed a real bug where a body `tenant_id` chose the governing policy and the audit tenant |
| Fail closed (SEC-INV-14) | mandatory | authz outage → 503 `authz_unavailable`; vault outage → 503; no policy → 404 deny | ✅ | |
| Evidence for material access (SEC-INV-15) | mandatory | REQUESTED / GRANTED / DENIED audit rows + events; denials recorded as fully as grants | ✅ | |
| **Authenticated transport, unique workload identity (SEC-INV-05, §9)** | "Service-to-service calls use unique workload identity and authenticated transport" | the server is plain HTTP — no `ListenAndServeTLS`, no `ClientAuth`, no peer-certificate handling. `internal/mtls` builds an **outbound** client for calling authorization-svc only | ❌ | |
| **Receiving service revalidates forwarded context (§9)** | mandatory | `POST /v1/secrets/broker` authorizes by comparing the policy's `allowed_workload_ids` against **`requested_by_principal_id` taken from the JSON body** (`internal/handler/handler.go:749`) | ❌ | Nothing cross-checks it against a peer certificate, the envelope's `X-Workload-Id`, or the gateway-verified principal |
| **Master key isolated from application workloads (SEC-INV-07)** | mandatory | `NewLocalFileVaultBackend(cfg.VaultKeyPath, cfg.VaultMasterKeyHex)` — AES-256-GCM with the master key handed to the process as hex config | ❌ | The only backend that exists. `internal/config/config.go:38` names the intended replacement |
| Privileged access JIT / time-bound / attributable (SEC-INV-12) | mandatory | lease rows carry `expires_at`; the read projection reports EXPIRED; `revoke` transitions state | ⚠️ | **Bookkeeping only** — the token is `"local-lease:" + 24 random bytes` with no bound expiry, and nothing ever redeems it, so an expired or revoked lease token is not actually invalidated |
| Lease duration ceiling | "time-bound" | `max_lease_duration_seconds` must be > 0 | ⚠️ | No platform maximum — a policy may declare any duration |
| No implicit authorization from network position (SEC-INV-04) | mandatory | the broker has no authorization-svc check at all; the allowlist is the whole gate | ⚠️ | Combined with the body-supplied identity, reachability ≈ authorization |
| Automated rotation (§13) | "Rotation must be automated where provider capability permits" | `POST …/rotate` only; no scheduler, no `rotate_by` sweep | ❌ | |
| Emergency secret retrieval (§13) | "requires privileged workflow and evidence" | zero occurrences of emergency / break-glass | ❌ | |
| Shared production secrets need documented exception (§13) | mandatory | no exception register | ❌ | |
| Dynamic credentials preferred over static (§13) | "Prefer identity federation and dynamic credentials" | `PUT material` stores static material; the broker leases a pointer to it | ⚠️ | No dynamic credential generation |
| Short-lived auto-rotated workload certs; identity bound to environment / region / audience (§9) | mandatory | absent | ❌ | |
| Crypto versioned and replaceable (SEC-INV-22) | mandatory | AES-256-GCM hardcoded in the local backend, but the `Backend` interface is narrow and swappable | ⚠️ | |
| Observable / testable control state (SEC-INV-25) | mandatory | metrics on every broker decision class; 83-check audit including the front end | ✅ | |
| Secret scanning in pre-commit / CI (§13) | mandatory | — | ❓ | Outside this service; belongs to repo CI configuration |
| Non-exportable workload credentials (§9) | "where platform capability permits" | n/a as built | ❓ | |

**Read routes not authorized:** `GET /v1/secrets/audit`, `GET /v1/secrets/leases`,
`GET …/versions` and `GET /v1/secret-policies` are tenant-scoped but make no authorization call.
Any principal in a tenant can read that tenant's **complete secret-access history**. Not a
`.docx` violation that can be pointed at directly — §5 says restricted data access "remains
authorization-required" under masking, which arguably covers this, but the doc does not name
these reads. Flagged as a risk rather than scored.

## Compliance

**10 of 25 scored controls fully met — 40%** (partials at half: **52%**). 2 need clarification.

**Top gaps by risk**

1. **The broker trusts a body-supplied workload identity** (auth). This is the
   credential-issuing endpoint of the platform. Any caller who reaches it inside the right
   tenant and knows a name from `allowed_workload_ids` gets a lease — and the audit trail then
   records that name as the requester, so the evidence is wrong in exactly the case you would
   need it to be right.
2. **No inbound mTLS** (auth). §9 requires the receiving service to authenticate transport and
   revalidate forwarded context; neither happens. This is what makes gap 1 exploitable rather
   than theoretical.
3. **Master key lives in application config** (crypto). SEC-INV-07 is violated by construction
   for any real deployment, and the local file backend is the only one implemented.
4. **Leases expire and revoke only on paper** (auth). Nothing consumes the token, so neither
   control has effect until a real vault backend issues it.
5. **No automated rotation, no emergency retrieval workflow, no shared-secret exception
   register** (§13 gaps).

---

# 5/9 — gateway-auth-svc (:8092) vs Security Standard §8 (Network/Edge) + GOV-01 §4 ingress duties

**Contract extracted from:** the Security wireframe `.docx` §8 (Internet edge / Ingress /
Service-to-service rows) and §3.1 invariants; the Governance Control Plane `.docx`
(= **ZS-SVC-A-001**) §4 GOV-01.
**Code:** `services/gateway-auth-svc/`, plus `deployments/docker-compose.yml` and
`deployments/gtrm/`, since the gateway's contract is half Traefik configuration.

Single functional route: `POST|GET /verify` (Traefik ForwardAuth), plus `/healthz`, `/readyz`,
`/metrics`. Exempt from the envelope middleware — correctly, and for the reason
`internal/envelope/policy.go:81` gives.

## GOV-01 ingress duties

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Verify signed envelope | GOV-01 | JWKS fetch, `ParseWithClaims`, issuer + audience pinned | ✅ | 401 `outcomeNoToken` / `outcomeInvalidToken` |
| Incomplete claims refused | "ambiguous ⇒ deny" | empty `principal_id` or `tenant_id` → 401 `outcomeIncompleteClaims` | ✅ | |
| Hostname / tenant binding, no fallback | NP-2 | `X-Zoiko-Resolved-Tenant-Id ≠ claims.TenantID` → 403 | ✅ | The GTRM per-tenant route sets it *after* the edge strips any client copy |
| Server-resolved context forwarded | tenant_id, entity, jurisdiction, residency | sets `X-Principal-Id`, `X-Tenant-Id`, `X-Legal-Entity-Id`, `X-Correlation-Id`, `X-Jurisdiction-Context`, `X-Timezone`, `X-Residency-Policy-Id` | ✅ | The resolver is called direct on :8081, deliberately not through Traefik |
| Bounded stale read | "stale context may be read only within bounded TTL" | `X-Tenant-Context-Stale: true` | ✅ | Staleness is declared to the upstream rather than hidden |
| Resolution unavailable → deny | fail closed | 503 + `X-Tenant-Context: unresolved`; tenant denied → 403 `denied` | ✅ | |
| Denial evidence | stable error code + evidence | `X-Auth-Denial-Reason`, per-outcome metrics, SIEM stream | ✅ | |

## §8 Ingress row

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **Authenticated routes** | Ingress control | the compose defines **one** Traefik router (`jurisdiction`) and it is the only one carrying the `gateway-auth` middleware | ⚠️ | Every other service is published on its own host port and reached directly, so the gateway fronts 1 route, not the estate. Each service still enforces its own envelope + authz, so this is not "open" — but the ForwardAuth guarantee covers one service |
| Hostname / tenant resolution | Ingress control | GTRM compiled per-tenant routers + the mismatch check | ✅ | |
| Protocol normalization | Ingress control | `X-Forwarded-Method` preferred over the method | ⚠️ | Traefik has only `--entrypoints.web.address=:80` |
| **Header sanitation** | Ingress control | the edge strips 8 `X-Zoiko-*` routing headers (`deployments/gtrm/compiler/emit.go:52`); `authResponseHeaders` replaces 8 identity headers | ⚠️ | **The governance envelope is not sanitized.** `X-Workload-Id`, `X-Support-Context-Id`, `X-Purpose-Context`, `X-Causation-Id`, `X-Approval-Reference`, `X-Workflow-Instance-Id`, `X-Evidence-Refs`, `X-Book-Id`, `X-Source-Channel` and `X-Expected-Version` all pass from client to service untouched |

## §8 Internet edge row

DDoS protection, WAF, rate limiting, bot/abuse controls, TLS and request-size limits are **not
present in `deployments/docker-compose.yml`** — no `rateLimit`, `inFlightReq` or `buffering`
middleware, and no TLS entrypoint. ❓ **Needs clarification**: this is the local development
compose, and `ZoikoSuite-GCP-Deployment-Runbook.pdf` may place these at a cloud load balancer.
They are not scored as failures without sight of the production edge. The "API gateway" control
itself is ✅.

## Continuous risk (Doc 05 §3.11, via SEC-INV-14)

| Item | Implemented | Status | Notes |
|---|---|---|---|
| Risk assessment consulted per request | `carta.Evaluate` on every verify | ✅ | |
| ISOLATE / DENY enforced | 403 + `X-Carta-Decision` + SIEM | ✅ | |
| **STEP_UP_MFA** | in the enum, logged and streamed to SIEM — **then the request proceeds** | ❌ | Only `ISOLATE` and `DENY` block. A risk engine asking for step-up is answered by letting the request through |
| carta unreachable → proceed | `Evaluate` returns nil; documented as an "additive signal" | ❓ | SEC-INV-14 names "policy, identity or key status", and a risk score is arguably none of those. Deliberate and documented, so not called a failure |

## Relevant SEC invariants

✅ SEC-INV-02 (authoritative tenant binding), SEC-INV-04 (this service *is* the control that
stops network position implying authorization), SEC-INV-14 (identity/policy paths fail closed),
SEC-INV-18 (no tokens or sensitive values logged).
⚠️ SEC-INV-01 (deny-by-default holds per service, but gateway coverage is per-route opt-in).
⚠️ SEC-INV-13 ("support access never uses silent user impersonation" — `X-Support-Context-Id` is
neither stripped here nor verified downstream).
❌ **SEC-INV-05** — gateway→identity-svc and gateway→tenant-svc calls are plain HTTP; no mTLS,
no workload identity.

## Compliance

**15 of 22 scored items fully met — 68%** (partials at half: **80%**). 7 items need
clarification, 6 of them the Internet-edge row.

**Top gaps by risk**

1. **The governance envelope is not sanitized at the edge** (auth). This is the finding that
   ties the audit together: `X-Support-Context-Id` reaches identity-context-svc from the open
   internet, and that service stamped it onto a session without verifying it (gap 1 of service
   1/9 — **fixed there 2026-09-23**; identity-context-svc now verifies it, but the header is still
   unsanitized at the edge). `X-Purpose-Context` — the field §4 requires for "governed sensitive access" — is
   likewise self-asserted, as are `X-Approval-Reference` and `X-Evidence-Refs`. Fixing either
   end closes it; fixing the edge closes it for every service at once.
2. **STEP_UP_MFA is decided and discarded** (auth). The risk engine's middle answer has no effect.
3. **No mTLS on the gateway's own upstream calls** (SEC-INV-05), matching the finding in 4/9.
4. **ForwardAuth covers one route** (auth, deployment). Worth confirming against the GCP runbook
   before treating it as a defect.

Two things this service does better than its siblings: every denial carries a distinct outcome
label and reaches SIEM, and the tenant/hostname mismatch is a first-class refusal rather than an
afterthought.

---

# 6/9 — search-indexer-svc (:8096) vs ZS-SVC-AB-001 (ESR-01 … ESR-05)

**Contract extracted from:**
`ZS-SVC-AB-001_Enterprise_Search_Indexing_Query_Secure_Retrieval_Control_Detailed_Service_Specifications_v1.0.docx`
§6.2–6.3, §7.1–7.3, §8.2, §10, §11.1 (8 APIs), §11.2 (8 events), §11.3 (20 reason codes).
**Code:** `services/search-indexer-svc/`.

This is the most faithful implementation in the group.

## Canonical APIs (§11.1)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `POST /v1/search` | governed search | `POST /v1/search` | ✅ | |
| **`POST /v1/search/semantic`** | semantic / hybrid retrieval; embedding model fixed by scope | — | ❌ | **ESR-02's vector half does not exist.** Zero occurrences of `vector` or `embedding` in the service |
| `POST /v1/retrieve` | re-authorize candidate refs; bulk limits | `POST /v1/retrieve` | ✅ | |
| `POST /v1/index-contracts` | privileged control-plane API; immutable after publication | present, plus `GET` and `/{id}/state` | ✅ | |
| `POST /v1/index-generations` | generation-ID idempotency; published contract required | present, plus `GET` and `/{id}/state` | ✅ | |
| `POST /v1/restrictions` | source-event idempotency; older writes cannot resurrect visibility | present, plus `GET`; 409 `stale_restriction_epoch` | ✅ | |
| `GET /v1/checkpoints` | index freshness / checkpoint health | present | ✅ | |
| `POST /v1/search-exports` | separate export permission, purpose, size and evidence | present; ESR-016 enforced | ✅ | |

Extra surface: `GET /v1/scopes`, `GET /v1/search-evidence`, `POST|GET /v1/search-sources`.
Undocumented in §11.1 but consistent with §4.1 search-source registration.

## Events (§11.2) — 8 of 8 present

`esr.index_generation.ready`, `esr.index_generation.activated`, `esr.index_checkpoint.advanced`,
`esr.restriction.propagated`, `esr.restriction.failed`, `esr.search.degraded`,
`esr.security_filter.denied`, `esr.reindex.failed`. ✅ Complete, including the actor-hash salting
on `security_filter.denied` that §11.2 asks for.

## Reason codes (§11.3) — 20 of 20 declared; 17 reachable

| Code | Status | Notes |
|---|---|---|
| ESR-001 … 011, 013 … 017, 020 | ✅ | All returned from real paths |
| **ESR-012 INDEX_STALE_FOR_SCOPE** | ❌ | Declared, **never returned**. Checkpoint watermarks are tracked and exposed on `GET /v1/checkpoints`, but a search against a scope whose index is hours behind returns results with no staleness signal — see top gap 2 |
| ESR-018 RESTRICTION_PROPAGATION_FAILED | ⚠️ | Never returned to a caller; the condition is reported via the `esr.restriction.failed` event instead. Defensible — propagation is asynchronous, so no caller is waiting |
| ESR-019 SEMANTIC_MODEL_MISMATCH | ⚠️ | Unreachable because semantic search does not exist |

## Controls

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Mandatory filter compilation (§6.3) | server-injected, non-overridable | `Planner.mandatoryFilters(tc, contract)` applied separately from `userFilters` | ✅ | |
| Query safety / complexity (§6.2) | forbidden operators, complexity bound | `checkOperators` → ESR-003; `complexity()` vs `MaxComplexityScore` → ESR-004 | ✅ | |
| Field policy (§6.2) | searchable vs returnable | `userFilters` → ESR-005, `requestedFields` → ESR-006 | ✅ | |
| Result window (§7) | bounded | ESR-015; signed cursor bound to tenant + scope + **plan digest** | ✅ | A caller cannot page a cursor into a different plan |
| R0 metadata-safe (§7.1) | retrieval class | implemented | ✅ | |
| R1 re-authorized (§7.1) | current resource authorization per candidate | implemented via the authz client | ✅ | |
| **R2 source-hydrated (§7.1)** | index content is not trusted as the current display value | class implemented; `Hydrator` is **nil in this deployment** | ⚠️ | Deliberate and correct-by-refusal: an R2 scope with no hydrator answers ESR-014 rather than silently downgrading (`cmd/server/main.go:188`). But §12's "current source hydration for material amounts" is unavailable until one is wired |
| R3 explicit export (§7.1) | separate authorization, purpose, evidence | ESR-016 + `/v1/search-exports` + evidence log | ✅ | |
| Snippet field-policy awareness (§7.2) | a matching sensitive term does not authorize disclosure | `safeSnippets(highlights, allowed)` | ✅ | |
| Facet privacy (§7.3) | facets over the caller-eligible set only | `Planner.facets` runs inside the compiled plan with mandatory filters applied | ⚠️ | Eligible-set scoping ✅; **minimum-cell / suppression rules** not evident |
| Restriction epoch, no resurrection (§8.2) | older writes cannot resurrect visibility | `restriction_epoch` throughout; ESR-013; 409 on a stale epoch | ✅ | |
| Degradation surfaced (§9) | completeness state | `esr.search.degraded` + ESR-020 + **206 Partial Content** | ✅ | |
| Vector index controls (§10.1) | 6 controls: pinned model, lineage, server-selected partitions, similarity ≠ authorization, tombstone propagation, migration certification | — | ❌ | All contingent on semantic search |
| RAG contract (§10.2) | 5 boundary rules | — | ❓ | §10.2 assigns most of these to AIG, not ESR |

## Compliance

**43 of 48 scored items fully met — 90%** (partials at half: **93%**). 1 item needs clarification.

**Top gaps by risk**

1. **ESR-02's semantic / vector half is entirely absent** (contract gap). One canonical API, six
   §10.1 controls and one reason code all rest on it. This is a scope decision to confirm, not a
   defect — but it is the single largest missing piece.
2. **Index staleness never reaches the caller** (data integrity). ESR-012 is declared and
   unreachable; checkpoint lag is visible only to whoever calls `GET /v1/checkpoints`. A search
   against a lagging index looks identical to one against a current index, which is exactly the
   condition §2.3 and §12 (Accounting, Records) need surfaced.
3. **No R2 hydrator wired** (data integrity, deferred). Correct refusal behaviour, so no wrong
   data is served — the capability is simply unavailable.
4. **Facet minimum-cell suppression not evident** (privacy). Facets are correctly scoped to the
   eligible set, which handles most of §7.3; the small-cell rule is the remaining half.

---

# 7/9 — notification-svc (:8133) vs ZS-SVC-Y-001 (NCD-01 … NCD-05)

**Contract extracted from:**
`ZS-SVC-Y-001_Notification_Communication_Delivery_Control_Detailed_Service_Specifications_v1.0.docx`
§2.1 (five canonical services), §3.3 (sent semantics), §3.4 (idempotency), §4.5 and §5.5 (API
surfaces), §6–§7.
**Code:** `services/notification-svc/`.

Same shape as service 3: the `.docx` specifies a **five-service control plane**; the
implementation is a 6-route in-app + SMTP notifier.

Implemented: `POST /v1/notifications` · `GET /v1/notifications` ·
`GET /v1/notifications/unread-count` · `GET /v1/notifications/templates` ·
`GET /v1/notifications/{id}` · `POST /v1/notifications/{id}/read`.
Events: `notification.sent`, `notification.failed`.

## NCD-01 Intent, Template & Content Registry (§4.5)

All seven operations — `POST /communication-intents`, `POST /templates`, `/{id}/validate`,
`/{id}/approve`, `/{id}/publish`, `POST /render-previews`, `GET /intents/{id}/effective` — are
❌ **missing**. There is no intent registry, no template version lifecycle, no approval with SoD,
no effective-dated publication, and no locale dimension (zero occurrences of `locale`).
`GET /v1/notifications/templates` returns a compiled-in catalogue; it is extra surface, not any
of the seven.

## NCD-02 Recipient, Channel, Preference & Suppression (§5.5)

All five operations — `POST /recipient-resolution`, `POST /channel-decision`,
`GET /suppressions`, `POST /preferences`, `POST /revalidate` — are ❌ **missing**. Zero
occurrences of `suppression` or `preference`; `quiet` appears 3 times, in comments. §5.3's
"preference versus permission" separation and §5.4's suppression precedence have nothing to
enforce them.

## NCD-03 Delivery Orchestration (§6)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Durable delivery job | §6.1 job contract | the `notifications` row doubles as the job | ⚠️ | |
| Attempt state machine | §6.2 | `PENDING` (with `next_attempt_at`) → `SENT` \| `FAILED`, concluding exactly once, with backoff | ⚠️ | Well-built for what it models — but see §3.4 below |
| Provider routing | §6.3 certified providers | single SMTP path | ❌ | |
| Multi-channel fallback | §6.4 governed new attempt on the same communication_id | — | ❌ | |
| Rate / abuse / storm controls | §6.5 | — | ❌ | |
| Cancellation & material change | §6.6 | — | ❌ | |

## NCD-04 Evidence, Bounce, Complaint & Suppression (§7)

Evidence normalization is ⚠️ — `provider_response` is captured with exactly the right semantics
in the data model ("a provider took it", never "it arrived"). Bounce handling, complaint
handling, channel reputation and suppression state are all ❌ **missing**.

## NCD-05 Regulated Notice & Acknowledgment

❌ Missing entirely — no notice packages, acknowledgement requirements, escalation evidence or
DRC handoff.

## §3.3 and §3.4 — the non-bypassable rules

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **"Sent" is never overloaded** | §3.3 **non-bypassable semantic rule**: "NCD must never expose one generic `sent=true` status". Five distinct claims are enumerated | the DB status enum is exactly `PENDING`, `SENT`, `FAILED`, and `SENT` is what `GET /v1/notifications` returns | ❌ | The data model's comments show the distinction was understood; the *exposed status* collapses it anyway |
| Stable `communication_id` | §3.4 | the notification id | ✅ | |
| Purpose-scoped idempotency key derived from the originating business event | §3.4 | unique index on `(tenant_id, correlation_id)` | ⚠️ | Close, but not purpose-scoped — two different communications sharing a correlation collide |
| **Durable `attempt_id` per provider submission** | §3.4 | `delivery_attempts INT` — a counter, not records | ❌ | |
| **Timeout after submit becomes UNKNOWN, not FAILED; reconcile before re-attempting** | §3.4 | no UNKNOWN state exists. A post-submit timeout is a retryable error, so the row stays `PENDING` and the worker **sends again** | ❌ | Duplicate delivery on exactly the failure §3.4 was written to prevent |
| Resend carries an explicit reason and preserves the evidence chain | §3.4 | — | ❌ | |

## Cross-cutting platform controls (done well)

✅ Tenant isolation via RLS with two *separately named* GUCs (`app.platform_scope`, read-only,
for the retry worker; `app.outbox_relay` for the relay) — reusing one name would have silently
widened the retry hatch to writes across every tenant's message bodies.
✅ Transactional outbox with `CompleteDelivery` taking the sealed event as a **required**
argument, so "conclude and tell nobody" is unrepresentable.
✅ Channel enum validated at the request boundary.
⚠️ `GET /v1/notifications/templates` now requires principal + tenant — an earlier
unauthenticated-read defect is **fixed** (verified in code, not assumed).

## Compliance

**4 of 34 scored items fully met — 12%** (partials at half: **19%**).

**Top gaps by risk**

1. **A post-submit timeout re-sends** (data integrity, user-visible). No UNKNOWN state and no
   provider reconciliation, so a network break after the SMTP server accepted produces a second
   delivery. For a payslip or a legal notice that is a real-world incident, and §3.4 calls it
   out by name.
2. **`SENT` is exposed as a single generic status** (correctness, legal). §3.3 is labelled
   non-bypassable precisely because "provider accepted" and "legally served" are different
   propositions. Downstream consumers and the UI currently cannot tell them apart.
3. **No suppression, preference or channel-decision layer** (privacy / compliance). Nothing
   stops a communication going to a recipient who has opted out or is suppressed — NCD-02 is the
   control that prevents it and it does not exist.
4. **No bounce or complaint ingestion** (deliverability + compliance). Channel reputation
   degrades invisibly.
5. **No template registry, approval or effective-dated publication** (governance). Content is
   compiled in, so a content change is a code deploy with no approval trail.
6. **No regulated-notice / acknowledgement path** (legal) — NCD-05 absent.

As with service 3, confirm whether Y-001 was ever this service's contract before treating 12% as
a verdict. If NCD-01…05 are planned as separate services, this one is a reasonable NCD-03 core
with the §3.3 / §3.4 defects being the genuine bugs.

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
| 3 | configuration-feature-flag-svc | ZS-SVC-AA-001 | 49 | 6 (12%) | 19% |
| 4 | secret-vault-integration-svc | Security Standard §13, §9, §3.1 | 25 | 10 (40%) | 52% |
| 5 | gateway-auth-svc | Security §8 + GOV-01 ingress | 22 | 15 (68%) | 80% |
| 6 | search-indexer-svc | ZS-SVC-AB-001 | 48 | 43 (**90%**) | **93%** |
| 7 | notification-svc | ZS-SVC-Y-001 | 34 | 4 (12%) | 19% |
| 8 | delegated-authority-svc | ORG-06 (delegation half) | 37 | 23 (62%) | 69% |
| 9 | access-control-svc | Authorization Standard §9 | 11 | 2 (18%) | 23% |
| | **Group total** | | **318** | **192 — 60%** (was 171 — 54%) | **≈66%** (was 61%) |

34 items scored partial and **11 need clarification** (was 45 and 17, before the 28 Sep re-audits of identity-context-svc and tenant-entity-registry-svc). The group total is an unweighted item
count across services of very different sizes; the per-service figures are the ones to act on.

Two services (3 and 7) pull the mean down because each implements one slice of a five-service
control plane. **That is the first thing to resolve**, since it decides whether they are
12%-complete services or correctly-sized components of planes that were never built.

## The four findings that cross service boundaries

1. **The governance envelope is unsanitized end to end.** The edge strips 8 `X-Zoiko-*` routing
   headers and Traefik replaces 8 identity headers. `X-Support-Context-Id`, `X-Purpose-Context`,
   `X-Workload-Id`, `X-Approval-Reference`, `X-Evidence-Refs` and `X-Causation-Id` travel from
   the client untouched — and identity-context-svc stamped the support-context one onto a session
   without verifying it (**fixed there 2026-09-23**; it also now resolves channel and workload
   against the verified principal), while secret-vault-integration-svc authorizes its broker against a
   body-supplied workload id. One fix at the edge closes several service-level holes at once.

2. **`Idempotency-Key` is demanded and never honoured.** The envelope middleware requires it on
   every material write across the estate; no service has a dedupe store. identity-context-svc
   declared `IDEMPOTENCY_MISMATCH` and returned it from nowhere (**fixed there**: dedupe store
   2026-09-23, replay bound to the principal and crash recovery 2026-09-28 — the pattern to copy). Replaying a break-glass grant or
   a tenant provision creates a second one.

3. **Maker-checker is self-asserted wherever it exists.** tenant-entity-registry-svc takes
   `approved_by_principal_id` from the maker's own request body and checks only that it differs
   from the actor — in the service and in the DB constraint. delegated-authority-svc is the
   exception: it binds the delegator to the verified caller and refuses self-dealing outright.
   That is the pattern the other services should copy.

4. **SoD exists but is unwired.** authorization-svc implements SoD rules, `CheckSoDConflict`,
   `CheckOwnObjectSoD` and `/v1/sod/validate`. `SOD_SERVICE_URL` is empty in the compose, so
   identity-context-svc falls back to `PermitAllChecker` locally (it correctly refuses to boot in
   staging or production without it), and neither delegated-authority-svc nor access-control-svc
   calls it at all.

## Where to start

Ranked by risk across all nine services:

1. Header sanitation at the edge
2. Role-revocation topic mismatch (access-control-svc → identity-context-svc) — **consumer side fixed 2026-09-28**
3. Broker workload identity (secret-vault-integration-svc)
4. Notification post-submit retry duplication
5. Idempotency replay protection — **done in identity-context-svc**; other services still to follow
6. Maker-checker approver verification

## Open questions — decisions, not further searching

- Are CFG-01 / CFG-04 / CFG-05 and NCD-01 / NCD-02 / NCD-04 / NCD-05 planned as separate services?
- Are access-control-svc and authorization-svc meant to share role ownership?
- Do the §8 Internet-edge controls (DDoS, WAF, rate limiting, TLS, request-size limits) live in
  the GCP deployment runbook?
- Is ORG-03's `MergeDuplicateCandidate` reachable at all, given §1's prohibition on destructive
  merge?
- What do "hard isolation identifiers" (ORG-02) and "sensitive identifiers" (ORG-03) enumerate to?
