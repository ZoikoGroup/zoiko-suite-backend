# Global Traffic & Residency Manager — Design Proposal (DRAFT)

**Status: DRAFT — not approved. Requires a check-in with the CTO and
whoever owns `feat/api-gateway` before any implementation starts.** This
document exists because the source docs specify functional requirements
with no named technology and no precedent elsewhere in this repo — every
other Phase 1 component had at least a reference pattern (a sibling
service, a named tech) to mirror. This one doesn't, so the risk of
building something that gets unwound is real enough to design on paper
first.

## 1. Source requirements (verbatim, cited)

**`06-blueprint.md`, "Critical Addition" (Phase 1 service list):**
> The Global Traffic & Residency Manager must exist early enough to
> support:
> - region-aware ingress routing
> - residency-based request steering
> - regulated-region deployment readiness
> - EU / Middle East trust positioning from Day 0

Phase 1's own Exit Criteria include "region-aware routing is technically
**proven**" — proven, not "multi-region production traffic is live." That
distinction drives most of this proposal's scoping in §4.

**`05-security.md` §15.6, Global Traffic Control:**
> Ingress routing should support:
> - regional failover
> - controlled traffic diversion
> - quarantine routing patterns for incident containment
> - service-tier-aware continuity behavior

**`05-security.md` §15.5, Cross-Region Security** (DR/replication
concerns, not routing itself, but load-bearing context): replication must
be residency-aware, keys must be region-governed, restoration must be
authorization-controlled, replay/reprocessing must be idempotent.

No technology is named anywhere in this set — unlike Observability
Baseline, where OpenTelemetry is at least specified.

## 2. What already exists — don't design this in a vacuum

Two facts change the shape of this problem significantly, and neither was
obvious from the requirements text alone:

**a) The residency data model is already built and live**, in
`tenant-entity-registry-svc`:
- `ResidencyRegion` (`internal/domain/types.go`) — `region_code`,
  `region_name`, `cloud_provider`, `country_code`, `sovereign_flag`,
  `active_flag`. Explicitly "Platform-managed, IaC-provisioned. No write
  API per Q1 resolution" — read-only via `GET /residency-regions` and
  `GET /residency-regions/{regionID}`. Someone already decided regions
  are seeded by infrastructure code, not created through an API — this
  proposal inherits that decision rather than reopening it.
- `DataResidencyPolicy` — per-tenant, with `ResidencyMode`
  (`STRICT_REGION` / `PREFERRED_REGION`) and a `ConflictResolutionMode`.
  Every tenant gets a `DefaultDataResidencyPolicyID` at provisioning time
  (`ProvisionTenantRequest`).

**This means GTRM's job is not to invent a residency data model — it's to
consume this one at the ingress layer.** The open question is purely
*how a request's residency requirement gets resolved and enforced at
the edge*, not what residency even means for a tenant.

**b) The API Gateway is Traefik v3.1** (`feat/api-gateway` branch,
"chunk 1: routing only, no auth yet"). Traefik natively supports
header-based and label-based routing rules, weighted round-robin, and
health-check-driven failover — all of which map directly onto §15.6's
requirements without needing a second routing layer in front of it. Any
GTRM design that ignores this and proposes an independent routing tier
duplicates work the gateway owner is already doing.

## 3. The four questions this doc exists to force a decision on

None of these are answered below — each has a recommended default with
reasoning, flagged explicitly as a recommendation, not a decision.

**Q1 — Where does the routing decision live: in Traefik itself, or in a
separate GTRM component in front of/beside it?**
- *Option A*: Traefik-native. Residency/region routing expressed as
  Traefik dynamic-config rules (header match on a resolved region code →
  route to region-tagged backend pool). No new service.
- *Option B*: A dedicated `global-traffic-residency-svc` that Traefik
  calls out to (a "forward auth"-style middleware) for a routing
  decision per request, then routes based on the response.
- **Recommendation: A for Phase 1.** Building a whole new service to
  prove a pattern that Traefik's own primitives already express is
  over-engineering for the "technically proven" bar Phase 1 sets. B
  becomes worth it once routing logic outgrows what static/dynamic
  Traefik config can express — not before.

**Q2 — What resolves a request's target region: tenant config lookup, or
request origin (geo-IP), or both?**
- Given `DataResidencyPolicy` is a per-tenant setting (not per-request
  geography), residency steering is fundamentally **identity-based**
  (which tenant is this?), not geo-based (where did this packet come
  from?). Geo-IP-based routing (e.g., CDN edge routing to the nearest
  region) is a *performance* concern, separate from a *compliance*
  concern — conflating them risks routing a request to the "nearest"
  region even when that region violates the tenant's `STRICT_REGION`
  policy.
- **Real gap found during Phase 1 implementation planning, not assumed
  away**: `DataResidencyPolicy` (`tenant-entity-registry-svc/internal/domain/types.go`)
  has no `ResidencyRegionID` field. It stores `ResidencyMode`
  (`STRICT_REGION` / `PREFERRED_REGION` / `FOLLOW_ENTITY` — three values,
  corrected from an earlier draft of this doc which only listed two) and
  a `ConflictResolutionMode`, but nothing links a tenant to a specific
  entry in the `ResidencyRegion` catalog. So "resolve region from
  `DataResidencyPolicy`" isn't buildable as originally written — the
  policy says *how strictly* to enforce residency, not *which* region.
- **Update — the gap is now closed, real data, real endpoint**:
  migration `000003_add_residency_region_to_policies` added
  `residency_region_id` to `data_residency_policies` (nullable,
  unbackfilled — existing policies have no way to infer a correct region
  automatically), and `tenant-entity-registry-svc` now exposes
  `GET /v1/tenants/{tenantID}/residency-region`, which resolves
  Tenant → `DefaultDataResidencyPolicyID` → `DataResidencyPolicy.ResidencyRegionID`
  → `ResidencyRegion.RegionCode` against real rows — verified live: a
  tenant with a region-assigned policy resolves correctly (`200`, real
  `region_code`), a tenant whose policy has no region yet returns `409`
  (`ErrRegionUnresolved` — a real, distinct, expected state, not folded
  into `404`), and an unknown tenant returns `404`.
- **A real architectural constraint found implementing this, not a
  shortcut**: Traefik's request lifecycle matches a router (by Host/Path/
  Header rules on the *original* request) **before** any middleware for
  that router runs. A middleware like ForwardAuth can add headers to the
  request forwarded to the backend *that router already selected* — it
  cannot use its own response to pick a *different* backend pool for that
  same request. So a live per-request "call tenant-entity-registry-svc,
  get the region, route accordingly" dance cannot happen natively inside
  Traefik OSS in one hop. Concretely, this means: the caller (or a thin
  edge component in front of Traefik — not Traefik itself) must resolve
  the region via the new endpoint above and set `X-Tenant-Region` before
  the request reaches the gateway. The Phase 1 demo's header-setting is
  therefore not a placeholder to be automated away trivially — it's the
  correct shape for a Traefik-only architecture (Q1's Option A). True
  Traefik-internal dynamic backend selection would require Q1's Option B
  (a dedicated routing component Traefik defers to), which the design
  doc already flagged as the fallback if Option A proved insufficient —
  it did, for this specific piece, though Option A still holds for the
  routing/failover/quarantine mechanics themselves.

**Q3 — What does "quarantine routing" actually trigger on?**
- The source doc gives no mechanism, only intent ("quarantine routing
  patterns for incident containment"). Two shapes are plausible: (a) a
  manual operator action (flip a flag, Traefik picks up new dynamic
  config, traffic diverts to a maintenance/quarantine backend), or (b)
  an automated trigger tied to a health/security signal (which doesn't
  exist yet — there's no anomaly detection or security-event monitoring
  anywhere in this repo, and Observability Baseline's own alerting work
  is a separate, not-yet-built prerequisite for that).
- **Recommendation**: scope Phase 1 to (a) only — manual, operator-
  triggered diversion, proven end-to-end. (b) is meaningless to build
  before Observability Baseline's alertable-failure-states work exists to
  trigger it, and treating it as in-scope now would front-run other work
  Phase 1 is doing in parallel.

**Q4 — Single global stack with logical region tags, or genuinely
separate regional deployments?**
- Nothing in this repo runs in more than one place today. There is no
  Kubernetes base yet (a sibling Phase 1 item, not built), no multi-
  region cloud footprint, no CDN/WAF wiring beyond `01-backend.md`
  §16.2 naming it as a target. Actually deploying to multiple real
  regions is not a Phase 1 deliverable by any reading of the Exit
  Criteria — "technically proven" is achievable in one region with
  *simulated* region tags.
- **Recommendation**: Phase 1 proves the pattern in the existing single-
  region Docker Compose stack — tag backend service pools with a logical
  `region` label, route based on a resolved tenant residency policy,
  and demonstrate failover/diversion between two logically-tagged pools
  in that one environment. Real multi-region deployment is explicitly
  **out of scope** for this phase and belongs with whoever ends up
  owning the Kubernetes base + cloud footprint decision.

## 4. Proposed Phase 1 scope (pending sign-off)

**In scope:**
- Traefik dynamic-config rules that route based on a resolved region tag
  (Q1: Option A)
- A defined (not necessarily automated) lookup path from tenant →
  `DataResidencyPolicy` → target region tag (Q2)
- One demonstrated failover scenario and one demonstrated manual
  quarantine-diversion scenario, in the existing dev stack (Q3a, Q4)
- A short spec doc (this file, once approved) as the reference other
  services can point to, same role `jurisdiction-rules-svc`'s context.md
  plays for Tier 0 CRUD services

**Explicit non-goals for Phase 1** (real, named, not silently dropped):
- No real multi-region cloud deployment (Q4)
- No automated/security-signal-triggered quarantine (Q3b) — blocked on
  Observability Baseline's alerting work existing first
- No new `global-traffic-residency-svc` (Q1 Option B) unless Traefik's
  native rules prove insufficient
- No geo-IP-based routing (Q2) — this is a residency/compliance feature,
  not a latency-optimization feature, and conflating the two is a
  correctness risk, not just a scoping one

## 5. Before implementation starts

Check in on:
1. Confirm Q1–Q4's recommendations above, or override them — each has a
   real, named alternative if the default is wrong for a reason not
   visible from this repo alone (e.g., a cloud/infra decision already
   made elsewhere).
2. Coordinate with whoever owns `feat/api-gateway` — this proposal's
   entire Q1 recommendation depends on Traefik's dynamic config
   supporting the routing shape described; that should be confirmed with
   them directly, not assumed from reading Traefik's docs alone.
3. Confirm this is sequenced after (or alongside, not blocked by) the
   Observability Baseline work — Q3's quarantine scenario benefits from
   having central logging/tracing in place to actually observe the
   diversion happening, even though it isn't a hard dependency for Q1/Q2.
