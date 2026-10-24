# Spec deviations — capability-registry-svc (GOV-11 §14 / COM-03 §4.3)

## Provenance: this service cites a doc7 source that is not in the workspace

The service's package comments, migrations and tests say they implement
`docs/original_doc/zoiko_suite_doc7.txt` §7, §C1–C2, §Q1, §29 and §32.1
(`internal/domain/types.go`, `internal/store/pg_store.go`,
`deployments/migrations/000001_initial_schema.up.sql`). That file does not
exist anywhere in this workspace. A recursive search finds no
`zoiko_suite_doc7.txt`; even
`docs/architecture/doc7-acceptance-checklist-traceability.md` cites the missing
path at line 3. The only doc7 material present is the derived backlog and
traceability notes under `docs/architecture/`.

The governing documents that *are* present are the two DOCX files used for
this review:

- `docs/architecture/original_doc/ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx`
  — GOV-11 at §14.
- `docs/architecture/original_doc/ZoikoSuite_Commercial_Platform_Billing_Subscription_Entitlement_Detailed_Service_Specifications_v1_0.docx`
  — COM-03 at §4.3.

They are a different generation of specification from doc7. The master register
records that doc7 is a pre-register document (`ZSU-COM-OPS-001`, dated
2026-08-07) and that GOV-11 is only “thematically adjacent” to this service,
not a real object-level match:

> “`capability-registry-svc` … cites `docs/original_doc/zoiko_suite_doc7.txt`,
> a pre-register document … that predates and is distinct from the
> 24-Aug-2026 44-document register … GOV-11 ‘Feature Policy & Capability
> Exposure’ … [and GOV-12] are thematically adjacent to
> `capability-registry-svc`/`kill-switch-registry-svc` respectively, but neither
> section's own contract … matches these services' actual owned objects closely
> enough to call a real match rather than a thematic echo.”

Meanwhile the ownership register assigns this service a governing-spec cell
that mixes both newer specs with a catalogue data object:

> “| Commercial Plane | `capability-registry-svc` | … | GOV-11 capability
> exposure · COM-03 | Capability catalogue | commercial-account |”
> — `docs/ZoikoSuite Domain Ownership.md`, line 1285

That cell is the origin of this review, but it is not a build instruction and
it does not resolve which service is authoritative. The code is visibly a
doc7 §7 five-registry capability catalogue, not a GOV-11 feature-policy engine
and not a COM-03 entitlement decision service. The rest of this register
therefore evaluates the code against GOV-11 §14 and COM-03 §4.3 as requested,
while recording the ownership conflict as a finding rather than silently
picking a winner.

**Headline conclusion.** `CreateMarketRelease` + `SetReleaseState` +
`UpdateIntegrationHealth` do **not** cover GOV-11's
`EnableTenantFeature` / `DisableTenantFeature` / `EmergencyDisableFeature`.
`ResolveCapability` is **not** COM-03's `EvaluateCapability` and does **not**
implement fail-closed paid-only escalation. `policy-svc` exists but is never
called by this service. `commercial-account-svc` is the only implementation in
the tree with the COM-03 decision surface; the two services do not call each
other, and the ownership conflict is recorded in A1 rather than resolved here.

## Direct answers

### 1. Do the three commands cover GOV-11's tenant enable/disable/emergency commands?

No.

| GOV-11 command | Nearest code in this service | Verdict |
|---|---|---|
| `CreateFeaturePolicy` | none | Absent |
| `ApproveFeaturePolicy` | none | Absent |
| `AssignReleaseRing` | none | Absent |
| `EnableTenantFeature` | `CreateMarketRelease` (market-level), `SetReleaseState` (capability-global) | Not covered — no tenant parameter or tenant row |
| `DisableTenantFeature` | `SetReleaseState` to `DISABLED` | Not covered — global, not per tenant |
| `EmergencyDisableFeature` | `SetReleaseState` to `INCIDENT_RESTRICTED`/`DISABLED` | Partial global kill switch, not the specified emergency control |

`CreateMarketRelease` records that a capability is approved for a **market**
(jurisdiction/language), not for a **tenant**. `SetReleaseState` appends an
operational row for the **whole capability**. `UpdateIntegrationHealth` changes
a provider's health and can only *block* resolution; it can never enable a
tenant. None of the three writes a `FeatureDefinition`, `FeaturePolicyVersion`
or `ReleaseRingAssignment`, and none emits `TenantFeatureChanged` or
`FeatureEmergencyDisabled`.

### 2. Does `ResolveCapability` equal COM-03 `EvaluateCapability` or fail closed for paid-only escalation?

No on both counts.

`ResolveCapability(ctx, capabilityCode, marketCode)` composes only this
service's technical dimensions: capability existence, current operational
release state, active market release, and certified/healthy integrations. It
has no tenant, subscription, plan, add-on, quota, grace, restriction or policy
input. Its answer is a bool plus a technical reason code. It has no
`ALLOW_WITH_LIMIT`, `READ_ONLY`, `RESTRICTED` or `DENY` outcome.

Worse, it fails **open** for the COM-03 question. A capability that exists in
the catalogue and has no release row resolves to `Enabled: true`; with no
`market_code` the market dimension is skipped entirely; with no integration
rows the provider dimension is skipped. There is no plan or subscription check,
so a paid-only capability can return `enabled: true` to any caller. The new
legal-approval gate below closes one technical fail-open, not the commercial
one.

`commercial-account-svc` is the only place in the tree with the five COM-03
outcomes and a fail-closed no-subscription path. It never calls this service,
and this service never calls it.

### 3. Is GOV-11's IAM/SoD/jurisdiction prohibition honored?

Only in the narrow sense that this service never writes an IAM grant, role or
SoD rule. It does not affirmatively enforce the prohibition:

- **IAM:** no IAM write exists, so enablement cannot directly grant permission.
  Authorization is a `CheckAllowed` call against `authorization-svc`, not a
  grant. That is the good half.
- **SoD:** the only SoD check is
  `wording_owner_principal_id != approved_by_principal_id` on capability
  claims. Both values are caller-supplied strings; neither principal is
  authenticated by this service. `SetReleaseState` has no maker-checker at all,
  and the same `RELEASE_STATE_SET` action covers `GA` and
  `INCIDENT_RESTRICTED`/`DISABLED`.
- **Jurisdiction:** `market_code` is an unvalidated query/body string. There is
  no jurisdiction-pack client, no tenant jurisdiction lookup, and no
  `jurisdiction-rules` dependency. A jurisdiction-prohibited feature cannot be
  *detected*, let alone refused. The negative path in GOV-11 (“Jurisdiction-
  prohibited feature cannot be tenant-enabled”) is not implemented.
- **Tenant scoping:** the five resource tables have no `tenant_id` column
  (`000001_initial_schema.up.sql`). The global envelope requires a tenant
  header, but `ResolveCapability` ignores it; the store signature has no tenant
  argument.
- **Read authorization:** `ResolveCapability` has no service-level authz check.
  In production the envelope middleware requires `tenant_id` and actor headers
  on every request, but it does not check whether that actor may read the
  capability. The handler test router bypasses the envelope, so the existing
  “no principal required” test describes handler behavior, not deployed
  authorization.

### 4. Does `policy-svc` exist, and is it actually called?

`policy-svc` exists and mounts `/v1/policies` and `/v1/policies/evaluate`.
Its evaluator is deliberately narrow:

> “Scoped narrowly for v1: only policy_type=APPROVAL_THRESHOLD has real
> evaluation logic.”
> — `services/policy-svc/internal/handler/handler.go`

Every other policy type returns `501 policy_type_not_implemented`. This service
has no `policy-svc` client, no policy service URL in config, and no call site.
The domain comment that sends “security/privacy eligibility” to `policy-svc`
(`internal/domain/types.go`) is aspirational, not implemented.

## Verified controls

These are implemented and were checked in code and against a fresh database,
not inferred from comments:

- **All six command paths use the migration-000002/000003 mechanisms.** There
  are 10 handlers, not 12: 6 commands and 4 queries. Each command calls
  `buildClaim`, and each corresponding store method calls `claimIdempotency`
  and `insertOutboxEvent` in the same `withTx` transaction. The matrix is
  complete; no command path omits either table.

  | Command | Operation | Idempotency claim | Outbox event |
  |---|---|---|---|
  | `POST /v1/capabilities` | `CreateCapability` | yes | `capability.created` |
  | `POST /v1/capabilities/{id}/market-releases` | `CreateMarketRelease` | yes | `market_release.created` |
  | `POST /v1/capabilities/{id}/integration-capabilities` | `CreateIntegrationCapability` | yes | `integration_capability.created` |
  | `PUT /v1/integration-capabilities/{id}/health` | `UpdateIntegrationHealth` | yes | `integration_capability.health_updated` |
  | `POST /v1/capabilities/{id}/release-state` | `SetReleaseState` | yes | `capability_release.state_changed` |
  | `POST /v1/capabilities/{id}/claims` | `CreateCapabilityClaim` | yes | `capability_claim.created` |

  The four queries are `GET /v1/capabilities/{id}`,
  `GET /v1/capabilities/{id}/integration-capabilities`,
  `GET /v1/capabilities/{id}/claims`, and
  `GET /v1/capability-resolution/{capabilityCode}`. They do not write, so they
  correctly have no claim or outbox row.

- **Replay semantics are real.** Same key + same body returns the originally
  persisted status/body with `Idempotent-Replayed: true`; same key + different
  body returns 422; replay does not create a second resource or a second outbox
  event. Claims are scoped by tenant, operation and principal. Concurrent
  same-key requests produce one write and one replay. Durability survives a
  pool restart. Outbox insert failure rolls back the business row and the
  claim together; the relay retries with the same event ID and payload and does
  not duplicate rows.

- **Write-time enum validation is present.** `execution_risk_class`,
  `legal_approval_status`, market-release state, operational release state and
  integration health are validated at the API boundary. `ResolveCapability`
  also fails closed on an unrecognized persisted release/market state or an
  unknown integration health value.

- **Authorization is present on 9 of 10 handlers.** Every command and every
  query except `ResolveCapability` requires `X-Principal-Id` and calls
  `authorization-svc`. The commands are platform-scoped
  (`00000000-0000-0000-0000-00000000f001`), consistent with a platform
  catalogue.

- **SQL error mapping is uniform on writes.** A nonexistent or malformed
  `capability_id` maps to `domain.ErrCapabilityNotFound` and HTTP 404 on all
  four writes that reference an existing capability_id. This pass added the
  missing case to `CreateRelease` / `SetReleaseState`; it was already present on
  `CreateMarketRelease`, `CreateIntegrationCapability` and
  `CreateCapabilityClaim`.

- **Legal approval now fails closed on read.** A customer-facing market release
  (`PILOT`, `BETA`, `GA`) whose `legal_approval_status` is not `APPROVED` no
  longer resolves enabled. `INTERNAL` is exempt because it is not a market
  activation. This is the first affirmative implementation of GOV-11's
  “cannot … activate unapproved contracts” clause in this service.

- **“Current” selection is deterministic.** `GetCurrentRelease` and
  `GetActiveMarketRelease` now order by `effective_from DESC, created_at DESC,
  <id> DESC`, so two rows sharing an `effective_from` cannot make resolution
  flip between requests.

- **No N+1 query was found.** `ResolveCapability` issues at most four
  sequential queries: capability by code, current release, active market
  release (only when `market_code` is supplied), and the integration list. The
  integration loop is in memory over the single list query. The two list
  endpoints each issue one query and iterate rows without issuing more. The
  cost concern is up to four round trips per resolution and no cache, not an
  N+1.

- **Idempotency/outbox schema is applied from empty.** `000002` creates
  `idempotency_keys` and `000003` creates `outbox_events`; the isolated test
  helper applies all three migrations in order to a fresh schema.

## A. Not implemented, weaker than the spec, or still open

### A1. Ownership is contradictory: GOV-11 and COM-03 each have more than one claimant

The row that assigns this review says:

> “| Commercial Plane | `capability-registry-svc` | … | GOV-11 capability
> exposure · COM-03 | Capability catalogue | commercial-account |”
> — ownership register, line 1285

But the same register also gives:

> “| Commercial Plane | `commercial-account-svc` | … | COM-01..05 (ZS-SVC-Q) ·
> ZS-COM-PFE-001 | Plans, prices, subscriptions, entitlements, usage, platform
> invoices | … |”
> — line 1284

> “| Platform Services | `configuration-feature-flag-svc` | … | Doc03 §9.6 ·
> CFG-01..05 (ZS-SVC-AA) · GOV-11 | ConfigVersion, FeatureFlag, overrides | … |”
> — line 1200

So COM-03 is claimed by both `capability-registry-svc` and
`commercial-account-svc`, and GOV-11 is claimed by both
`capability-registry-svc` and `configuration-feature-flag-svc`.
`kill-switch-registry-svc` implements the emergency kill-switch mechanism, but
its own register row cites `CFG-03/05`, not GOV-11. The specs themselves do not
permit a silent split. COM-03 says:

> “COM-03 is the sole runtime entitlement decision service for commercial plan
> access/limits. Clients may display entitlements but cannot self-assert them.”
> — COM-03 preamble, line 32

and the commercial-vs-authorization table separates the layers explicitly:

> “Commercial entitlement | Is this feature/limit included in the customer's
> effective commercial package? | COM-03”
>
> “Feature exposure | Should capability be visible/rolled out in this
> environment/market? | GOV-11 Feature Policy”
> — COM-03 §4.3 table

Two services cannot both be the sole COM-03 decision service, and two cannot
both own GOV-11's `FeaturePolicyVersion`/`ReleaseRingAssignment`. This register
does not choose. An ADR is required that selects one of: (a) `capability-
registry-svc` is the capability/feature catalogue and a *dependency* of
COM-03, to be wired into it; (b) `capability-registry-svc` is only the doc7
catalogue and GOV-11/COM-03 are removed from its ownership; or (c) the newer
services are authoritative and this service is retired or reduced. Until that
decision, “GOV-11 compliant” and “COM-03 compliant” have no single owner.

### A2. GOV-11's owned objects and commands are absent

GOV-11 says:

> “Authoritative ownership | FeatureDefinition, FeaturePolicyVersion,
> ReleaseRingAssignment, EmergencyDisable state.”
>
> “Commands | CreateFeaturePolicy; ApproveFeaturePolicy; AssignReleaseRing;
> EnableTenantFeature; DisableTenantFeature; EmergencyDisableFeature.”
> — GOV-11 §14

None of those objects or commands exists. The five resource tables are
`capabilities`, `market_releases`, `integration_capabilities`, `releases` and
`capability_claims`; none has a `tenant_id` (the idempotency table does, but
that is replay scoping, not exposure scoping). There is no feature definition,
no policy version, no ring assignment, and no per-tenant enablement row. The
nearest writes are:

- `CreateMarketRelease`: writes a market/jurisdiction approval. It takes
  `market_code`, `language_code`, `legal_approval_status`, state and an
  effective window. It has no tenant or user/cohort dimension.
- `SetReleaseState`: appends a global operational state for one capability. It
  takes no tenant, no ring and no expected version.
- `UpdateIntegrationHealth`: changes a provider's health. It is a technical
  readiness input, not an exposure command.

The GOV-11 minimum negative paths therefore cannot be exercised:

> “Disabled feature backend command remains unauthorized” — this service does
> not gate backend commands; that remains `authorization-svc`'s job.
>
> “Jurisdiction-prohibited feature cannot be tenant-enabled” — there is no
> tenant-enable command and no jurisdiction check.
>
> “Entitlement loss removes exposure” — there is no entitlement input.
>
> “Emergency kill switch does not mutate business records” — `SetReleaseState`
> is append-only and does not mutate business records, which satisfies this
> narrow property, but it is not an emergency-specific control (A4).

### A3. GOV-11's queries, state model and events are absent

GOV-11 requires:

> “Queries / read contracts | EvaluateFeaturePolicy; ListEffectiveFeatures;
> ExplainFeatureDecision.”
>
> “State model | Feature policy Draft/Approved/Active/Suspended/Retired;
> exposure Off/Pilot/Canary/GA/Disabled.”
>
> “Events produced | FeaturePolicyActivated; TenantFeatureChanged;
> FeatureEmergencyDisabled.”
> — GOV-11 §14

There is no `EvaluateFeaturePolicy`, no `ListEffectiveFeatures`, and no
`ExplainFeatureDecision`. `ResolveCapability` is a different read contract:
technical availability for `(capability_code, market_code)`, not a feature
policy decision for `(tenant, user, feature, jurisdiction, release channel)`.

The state vocabulary is also different. The service has operational release
states `GA/BETA/PILOT/INTERNAL/DISABLED/INCIDENT_RESTRICTED` and market-release
states `INTERNAL/PILOT/BETA/GA/RESTRICTED/SUSPENDED/RETIRED`. It has no policy
lifecycle (`Draft/Approved/Active/Suspended/Retired`) and no exposure state
`Off` or `Canary`. Its six outbox event types are the write events in the
verified-controls table; none is a GOV-11 event.

The GOV-11 wireframe also requires `/internal/v1/gov11/...` routes with
“Read-only; scoped; correlation required” queries and “Idempotency-Key +
expected_version where stateful” commands. This service exposes public
`/v1/capabilities` routes and has no `expected_version` concurrency parameter.

### A4. `EmergencyDisableFeature` is only a partial global kill switch, and the dedicated kill-switch service is not wired

GOV-11 says:

> “Authorization / permissions | Product/release owners; tenant admins only for
> tenant-configurable dimensions; emergency disable privileged.”
>
> “Failure / degradation semantics | Unknown policy => disabled for protected
> capability; emergency disable must fail safe and propagate quickly.”
>
> “Events produced | … FeatureEmergencyDisabled.”
> — GOV-11 §14

`SetReleaseState` can write `INCIDENT_RESTRICTED` or `DISABLED`, and
`ResolveCapability` checks the latest row before anything else, so the *read*
side fails safe for that capability. That is a genuine partial control. It is
not `EmergencyDisableFeature` as specified:

- The same `RELEASE_STATE_SET` authorization action covers `GA` and emergency
  restriction; there is no privileged emergency permission.
- There is no emergency ticket, incident reference, rollout reason or
  post-action reconciliation field. `reason` is free text.
- There is no `FeatureEmergencyDisabled` event.
- There is no cache or invalidation layer, so “propagate quickly” is just
  “every read hits Postgres”; there is no propagation SLO.
- It is global to the capability, not tenant-scoped or ring-scoped.
- `kill-switch-registry-svc` is the dedicated service for tenant/platform kill
  switches and is already consumed by `ai-governance-svc`. This service does
  not call it, and the two controls are not composed.

### A5. The IAM/SoD/jurisdiction prohibition is not affirmatively enforced

GOV-11 states the prohibition:

> “Feature enablement cannot grant IAM permission, override SoD, bypass
> jurisdiction or activate unapproved contracts.”
> — GOV-11 §14, Segregation of duties

The first clause is respected only because no IAM write exists. The other three
are not implemented as controls:

- **SoD.** The claim handler checks that two caller-supplied strings differ:
  `wording_owner_principal_id != approved_by_principal_id`. It does not
  authenticate either principal, does not verify that the approver has a legal
  approval role, and does not require an independent approval action. The
  release-state command has no maker-checker at all.
- **Jurisdiction.** `market_code` is not validated against a jurisdiction
  pack or the tenant's jurisdiction assignment. `CreateMarketRelease` will
  store any non-empty `market_code`. `ResolveCapability` treats the caller's
  query parameter as the market. There is no `jurisdiction-rules` client.
- **Unapproved contracts.** This pass added a read-side gate: a non-`INTERNAL`
  market release whose `legal_approval_status` is `PENDING`/`REJECTED` now
  returns `MARKET_BLOCKED`. The write side still accepts a `GA` row with
  `PENDING` legal status (which is reasonable for a pending approval record),
  but there is no contract object, no contract activation check and no
  separation between “market release approved” and “contract approved.”
- **Tenant admins.** No route is tenant-admin scoped; every command uses the
  platform scope constant. There is no tenant-configurable dimension for a
  tenant admin to control.

### A6. COM-03's decision contract is absent

COM-03 requires:

> “Purpose | Authoritative runtime decision for whether a tenant/user/workspace
> may use a commercial capability and at what limit under the effective
> subscription.”
>
> “Owns | EntitlementPolicyBinding, EntitlementSnapshot, LimitDecision,
> RestrictionDecision, GracePolicyOutcome, CommercialCapabilityExposure.”
>
> “Commands | RecomputeEntitlements; ApplyCommercialRestriction;
> RemoveRestriction; CreateSnapshot. Runtime access is generally query/decision
> based.”
>
> “Queries | EvaluateCapability; GetEffectiveEntitlements; GetLimit;
> ExplainDecision; GetEntitlementHistory.”
>
> “State | Decision outcome: Allow/Deny/AllowWithLimit/ReadOnly/Restricted.”
> — COM-03 §4.3

None of the owned objects, commands or queries exists here. There is no
`CommercialCapabilityExposure` table, no `EntitlementSnapshot`, no limit or
restriction object, and no grace policy. `ResolveCapability` is a bool plus a
technical reason code, not a decision outcome with a limit.

The nearest read is not an equivalent:

| COM-03 query | Nearest code here | Difference |
|---|---|---|
| `EvaluateCapability` | `ResolveCapability` | No tenant, subscription, plan, quota, grace or restriction input; no five outcomes |
| `GetEffectiveEntitlements` | none | Absent |
| `GetLimit` | none | Absent |
| `ExplainDecision` | `ResolveCapability` reason code | Explains only technical state, not a commercial decision |
| `GetEntitlementHistory` | none | Absent |

### A7. COM-03 fail-closed paid-only escalation is not implemented; the resolver defaults to enabled

COM-03 requires:

> “Failure semantics | Fail closed for paid-only capability escalation; safe
> cached last-known decision may be used only within approved freshness window
> for non-irreversible access.”
> — COM-03 §4.3

This service cannot evaluate “paid-only” because it has no subscription or plan
input. It also does not fail closed by default. In
`PgStore.ResolveCapability`:

- a known capability with no `releases` row falls through to the integration
  check and then returns `Enabled: true`;
- when `market_code` is empty, the entire market dimension is skipped;
- when there are no integration rows, the provider dimension is skipped;
- `PILOT`, `BETA` and `INTERNAL` release/market states are treated as
  non-blocking for every caller, because there is no tenant or ring context.

The result is that a paid-only capability can resolve `enabled: true` to an
unentitled caller. The new legal-approval gate blocks an unapproved *market*
release, but it does not add a commercial check. The only true no-subscription
`DENY` in the tree is in `commercial-account-svc`
(`com03_entitlement.go`: “no subscription exists for this organization”).

This is the most important residual fail-open in the service. It is not fixed
in this pass because a correct fix requires the COM-03 inputs (tenant,
effective subscription, plan capability matrix, limits, restrictions, grace),
not a local condition.

### A8. COM-03 is implemented elsewhere and the two services are not integrated

The real COM-03 decision path is:

- `commercial-account-svc` registers
  `POST /v1/commercial/entitlements:evaluate` (plus `:explain`,
  `:recompute`, `:snapshot`, `/effective`, `/limit`, `/history`).
- Its domain `EvaluateCapability` returns `ALLOW`, `ALLOW_WITH_LIMIT`,
  `READ_ONLY`, `RESTRICTED` or `DENY`; a missing subscription is `DENY` with
  “no subscription exists for this organization.”

That service has no `capability-registry` client and does not call
`ResolveCapability`. This service has no commercial-account client and does not
call `EvaluateCapability`. The ownership register lists
`commercial-account` as this service's dependency, but the dependency is not
implemented in code.

The consequence is a two-sided gap:

- COM-03 can allow a capability key that is absent from this catalogue, because
  it never consults it.
- The catalogue can report a capability enabled even when COM-03 would deny it
  for that tenant.

A caller would have to call both and know which one wins, and the ownership
register does not say. The master register also records this service among the
“zero real callers” services:

> “The other 6 zero-caller services remain unwired … `capability-registry-svc`
> → `commercial-account-svc` …”
> — `master-register-findings-2026-08-27.md`

So even the intended catalogue-to-COM-03 dependency is not live.

### A9. `policy-svc` exists but is too narrow and is never called

GOV-11 lists “tenant policy” as a required input and “Commercial entitlement,
jurisdiction pack, authorization, release management, service health” as
dependencies. This service has no policy client and no policy call site. The
domain comment claims:

> “security/privacy eligibility -> policy engine (policy-svc)”
> — `internal/domain/types.go`

but `policy-svc` only implements `policy_type=APPROVAL_THRESHOLD`; every other
type returns 501. Wiring it in as-is would let this service claim a policy
dimension it does not actually evaluate. The closest per-tenant configuration
surface in the tree is `configuration-feature-flag-svc` (tenant-scoped config
entries, feature flags, release plans, jurisdiction context), which this
service also does not call.

### A10. GOV-11/COM-03 evidence, lineage and NFR requirements are absent

GOV-11 requires:

> “Evidence / lineage | entitlement/policy versions, cohort, actor, rollout
> reason, emergency ticket, effective timestamps.”
>
> “NFR / recovery profile | C1 on gating; low latency/cacheable with
> invalidation; fast global emergency disable.”
> — GOV-11 §14

COM-03 requires:

> “Evidence/NFR | Decision input versions, policy version, reason code, source
> subscription version, usage snapshot ID, target p95 decision latency and
> cache revocation SLA defined per capability.”
> — COM-03 §4.3

The service stores actor and timestamps on releases/claims, and it has a free-
text `reason`. It does not store a policy version, cohort, entitlement version,
subscription version, usage snapshot, emergency ticket or rollout reason in a
structured field. It emits no decision evidence for `ResolveCapability` at all
— the read has no decision ID, no input versions and no audit record. It has no
cache, no cache-invalidation path, and no p95/cache-revocation SLO. The read is
up to four sequential Postgres round trips; that is not a latency defect by
itself, but it is not the specified cacheable/invalidatable profile.

### A11. `ResolveCapability` is not tenant-scoped and has no authorization check

GOV-11's wireframe requires every query to be “Read-only; scoped; correlation
required.” The global envelope middleware does require `tenant_id`,
actor, request ID and correlation ID, so a production caller cannot reach the
handler with no identity headers. But:

- `ResolveCapability` ignores the tenant in the request context; the store
  signature is `(capabilityCode, marketCode)` only.
- There is no authorization call, so any identified actor can read any
  capability's technical availability.
- The five resource tables have no tenant column, so even a future authz check
  could not scope the answer per tenant without a schema/model change.
- The handler-level test that asserts “no principal required” runs on a router
  without the envelope, so it does not prove the deployed posture.

This is a read-scoping gap, not an IAM write. It matters because GOV-11's
purpose is tenant/user exposure, and this endpoint cannot answer a tenant/user
question.

## B. Interpretations and additions (decisions a reviewer may reverse)

- **B1. The doc7 model is treated as superseded for this review, not as the
  governing contract.** The code's own comments cite a missing doc7 file; the
  ownership register and the available DOCX files assign GOV-11/COM-03. This
  register evaluates against the available GOV-11/COM-03 text. A reviewer could
  instead rule that doc7 §7 is the intended contract and that the GOV-11/COM-03
  assignment is a register error; that would make A2–A8 ownership findings
  rather than implementation defects. The provenance problem is real either
  way.

- **B2. `ResolveCapability` is a technical-availability resolver, not an
  entitlement decision.** It is useful as a catalogue read (“does this
  capability exist, is it operationally released, is its market active, are its
  providers healthy?”). It should not be used as the COM-03 answer. This is a
  reversible architectural interpretation: if the business wants one combined
  answer, the service needs the COM-03 inputs and the five outcomes, which is a
  new build.

- **B3. The legal-approval gate is read-side only, and `INTERNAL` is exempt.**
  Changed in this pass. A non-`INTERNAL` market release must have
  `legal_approval_status = APPROVED` to resolve enabled. `INTERNAL` is exempt
  because it is not a market activation. A reviewer could require `APPROVED`
  for `INTERNAL` too, or allow `PILOT`/`BETA` with pending legal approval under
  a time-boxed exception. The current choice is the conservative reading of
  “cannot … activate unapproved contracts.”

- **B4. The legal gate uses the market release's own legal status, not a
  contract object.** There is no contract entity in this service. If “contract”
  means a separate commercial agreement in another service, this gate is only a
  proxy. A reviewer could require a real contract reference before `GA`.

- **B5. `SetReleaseState` now maps a bad capability ID to 404 instead of 500.**
  Changed in this pass. `CreateRelease` maps Postgres 23503 and 22P02 to
  `domain.ErrCapabilityNotFound`; the handler maps that to 404. This mirrors
  the three other capability-scoped writes and does not change valid behavior.

- **B6. “Current” release/market-release selection now has deterministic
  tie-breakers.** Changed in this pass. The queries order by `effective_from
  DESC, created_at DESC, <id> DESC`. This is an implementation correction, not
  a spec policy; it prevents a tied `effective_from` from making resolution
  nondeterministic. A reviewer could instead add a uniqueness constraint or a
  partial index that makes ties impossible.

- **B7. No tenant, ring or feature-policy model was added.** A2 records the
  gap. Adding `FeatureDefinition`, `FeaturePolicyVersion`,
  `ReleaseRingAssignment`, tenant enablement and emergency-disable semantics is
  a new service design with migrations, APIs, events and tests — not a
  reasonably scoped defect fix. It should be designed with the ADR in A1.

- **B8. No `policy-svc` wiring was added.** A9 records the gap. Wiring a
  service that only evaluates `APPROVAL_THRESHOLD` would not provide feature
  policy evaluation, and would create a misleading “policy checked” claim. The
  real policy dependency needs a feature-policy contract first.

- **B9. No synchronous `commercial-account-svc` call was added to
  `ResolveCapability`.** A8 records the integration gap. A synchronous call on
  every catalogue read would couple the catalogue to the commercial plane and
  raise availability/fail-open-or-closed questions that the ownership register
  does not answer. The correct fix is either a composed read model with an
  explicit precedence rule or a caller-side composition contract, decided in
  the ADR.

- **B10. `ResolveCapability` remains without a service-level authz check.** A11
  records the gap. Changing it would break the documented cheap-read posture
  and require every caller to carry a capability-read grant. The global
  envelope still requires identity headers in production. A reviewer could
  require `CAPABILITY_RESOLVE` authorization; that is a contract change, not a
  local patch.

- **B11. No caching layer was added.** A10 records the NFR gap. The resolver
  issues at most four sequential queries, and a cache would need an invalidation
  path tied to release, market-release and integration-health writes. Adding an
  invalidatable cache is a separate change; the current code is correct but
  not cacheable as specified.

## Verification

Verified in `services/capability-registry-svc` against a fresh disposable
PostgreSQL 16 container, not the shared development database:

- Image: `postgres:16-alpine`
- Container: `capability-registry-test-20261024115136`
- Database: `capability_registry_test`
- `TEST_DATABASE_URL` pointed at the container's mapped port
- All migrations `000001`–`000003` applied from empty by the isolated test
  helper; each test uses a unique schema and drops it afterward

Commands and results:

```text
go build ./...        pass
go vet ./...          pass
go test ./... -count=1
  internal/authz       ok
  internal/events      ok
  internal/handler     ok
  internal/store       ok
  (other packages: no test files)
```

The store package ran against PostgreSQL rather than skipping. New tests added
in this pass:

- `TestPgStoreCreateRelease_NonexistentCapabilityMapsToNotFound` (23503 and
  22P02 → `ErrCapabilityNotFound`)
- `TestPgStoreResolveCapability_LegalApprovalFailsClosed` (`PENDING` and
  `REJECTED`; then `APPROVED` enables)
- `TestPgStoreResolveCapability_ReleaseTieBreakerIsDeterministic`
- `TestResolveCapability_UnapprovedMarketReleaseFailsClosed` (handler)
- `TestResolveCapability_ApprovedMarketReleaseEnabled` (handler)
- `TestSetReleaseState_InvalidCapabilityID_Returns404` (handler)

No production database, shared schema or shared container was used. The
container was disposable and was removed after verification.
