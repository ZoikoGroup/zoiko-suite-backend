# Global Traffic & Residency Manager — Phase 1 Architecture Decision

**Component:** Global Traffic & Residency Manager (GTRM)
**Decision status:** ✅ Approved for Phase 1 build
**Version:** Final Approved — v1.2
**Date:** 10 July 2026
**Classification:** Internal Engineering / Architecture Control
**Supersedes:** [`global-traffic-residency-manager-design.md`](./global-traffic-residency-manager-design.md) (the DRAFT proposal that raised these decisions)

> **Decision scope:** Phase 1 technical proof — tenant residency routing, incident diversion, regional failover demonstration, fail-closed behaviour, and auditability.

> **Final instruction:** Proceed with Traefik-native enforcement, hostname-based tenant resolution, tenant-residency-controlled region selection, manual block-mode quarantine, manual sticky failback, compiled configuration, drift detection, backend region assertion, and simulated regional pools for Phase 1.

---

## 0. Revision Notes — Final v1.2

Incorporates the engineering team's four proposed defaults, the Revision B controls, and additional governance controls required to make the decision buildable without compliance or operational gaps. The v1.0 / v1.1 direction stands; v1.2 tightens:

- Hostname-based tenant resolution approved, with domain allow-listing, Host-header validation, and external header stripping at the edge.
- Tenant residency remains the routing authority; user physical location and client-supplied geo headers are explicitly non-authoritative.
- "Data-bearing traffic" is defined so routing tests cannot be interpreted loosely.
- The policy-to-Traefik path is a generated and validated pipeline, not hand-authored gateway configuration.
- Configuration drift detection is required (policy/config divergence is the main silent-failure mode).
- Quarantine is block-mode by default; isolated serve-mode requires region-scoped quarantine pools.
- Fail-closed behaviour is implemented at the gateway and backed by backend region assertion.
- Failback is manual and sticky in Phase 1 to avoid traffic flapping.
- Route logs must be versioned, structured, non-sensitive, and region-aware.
- Acceptance criteria now include negative tests for spoofed geo headers, bad routing maps, drift, wrong-policy misroutes, token/hostname mismatch, and header injection.

## 1. Executive Decision

Engineering is approved to proceed with the GTRM Phase 1 build, subject to the controls in this document.

| Control | Approved Phase 1 Decision |
|---|---|
| **Routing enforcement** | Traefik-native routing. Traefik is the enforcement layer only; it does not author residency policy. |
| **Routing authority** | Tenant residency policy. User location and Geo-IP cannot override the tenant's residency requirement. |
| **Tenant resolution** | Hostname-based resolution using a controlled tenant slug and approved environment domain. |
| **Configuration model** | Generated Traefik configuration from a versioned GTRM routing map; no hand-edited production-equivalent routing files. |
| **Quarantine** | Manual, block-mode by default, two-person approval, auditable, reversible. |
| **Failover** | Approved fallback only. No fallback is compiled where no approved fallback exists. |
| **Failback** | Manual and sticky in Phase 1. No automatic flap-back when a primary pool recovers. |
| **Proof standard** | Simulated regional pools in the development stack; real multi-region cloud is deferred. |

> **Build unblock:** A "yes" to this document means the team builds GTRM Phase 1 now using the approved defaults. Any deviation requires architecture review before implementation.

## 2. Non-Negotiable Architecture Principle

GTRM must not become random routing rules in Traefik. The architecture must separate policy authorship, policy compilation, enforcement, and backend defence:

```
Tenant Residency Policy   (source of truth — tenant/workspace records)
        ↓
GTRM Routing Map          (versioned, validated policy-to-routing model)
        ↓
Configuration Compiler    (generates + validates Traefik config)
        ↓
Traefik Routing Config    (enforcement artefact — never hand-edited)
        ↓
Regional Service Pool     (backend region assertion rejects misroutes)
```

| Layer | Role | Phase 1 Rule |
|---|---|---|
| Tenant records | Source of truth | Own tenant residency policy, policy version, tenant slug, workspace mapping, permitted regions, and approved fallback. |
| GTRM routing map | Policy-to-routing model | Versioned, reviewable, validated and compiled. The only authored representation of routing policy. |
| Compiler | Integrity gate | Generates Traefik config; rejects invalid routes, invalid fallback, invalid quarantine, orphan routes and unsafe states. |
| Traefik | Enforcement point | Consumes compiled config only. Does not decide tenant policy; must not be manually edited. |
| Backend pool | Defence-in-depth guard | Rejects requests whose resolved-region marker or tenant policy context does not match the pool region. |

## 3. Decision 1 — Routing Engine

**Question:** Traefik's own routing rules, or a dedicated routing service / cloud routing tool / service mesh / CDN-dependent layer in Phase 1?

**Approved:** Use **Traefik-native routing** for Phase 1. No new routing microservice, cloud global load balancer, service mesh, or CDN-vendor routing dependency.

**Rationale:** Traefik is already present and can express deterministic routing to distinct backend pools. Phase 1 proves the residency-aware routing *mechanism*, not global cloud infrastructure. A generated-configuration model preserves the future option to replace Traefik-native enforcement with a dedicated service / mesh / cloud LB later.

**Boundary:** Traefik is the enforcement layer only. Business policy must not be authored in Traefik. Any route encoding tenant policy not present in the GTRM routing map is prohibited.

## 4. GTRM Routing Map and Configuration Compiler

### 4.1 Mandatory Routing Map
The single authored representation of routing policy (may be file-based initially):

```
schema_version
map_version              # monotonically increasing; stamped into logs
tenant_id
tenant_slug
workspace_id             # nullable where tenant-level routing is sufficient
data_residency_policy_id
policy_version
allowed_regions          # closed set; routes outside this set are invalid
primary_region
fallback_region          # nullable; absence = no failover configured
quarantine_mode          # NONE | BLOCK | ISOLATED_SERVE
quarantine_pool          # nullable; required and region-compatible for ISOLATED_SERVE
routing_status           # ACTIVE | SUSPENDED | PENDING_RESOLUTION
last_updated_at
updated_by
change_reference         # PR/change-ticket/approval reference
```

### 4.2 Compiler Requirements
Traefik config must be generated, validated, reviewed and deployed — never hand-authored for production-equivalent environments.
- **Schema validation** — reject missing fields, unknown region codes, invalid status/quarantine values, duplicated tenant slugs, malformed tenant IDs.
- **Allowed-region validation** — reject any route whose target pool is outside the tenant's `allowed_regions`.
- **Fallback validation** — where `fallback_region` is null, emit no Traefik failover service (restraint enforced by absence of config).
- **Quarantine validation** — BLOCK requires no tenant-data-processing pool; ISOLATED_SERVE requires a region-compatible quarantine pool.
- **Orphan detection** — reject routes with no map entry, and map entries generating no expected route.
- **Environment validation** — reject production-like route generation from a dev-only domain or test-only tenant slug.
- **Header policy generation** — generate edge rules that strip untrusted inbound tenant/region/map-version/geo headers before setting internal routing context.
- **Provenance** — stamp compiled artefacts with `map_version`, compiler version, commit SHA, generation timestamp.

### 4.3 Deployment and Drift Detection
- Deploy compiled config via GitOps where possible (peer review, approval, rollback, diff visibility).
- Unreviewed manual edits to production-equivalent routing files are prohibited.
- A drift check must compare live Traefik dynamic config against the current compiled output.
- Phase 1 default drift interval: detect within **15 minutes** in the dev stack, or within the CI/CD run if pipeline-driven.
- A drift event blocks further promotion until resolved and must be captured in the evidence pack.

## 5. Decision 2 — Region Selection Authority

**Question:** Route by the tenant's data residency policy, or the user's physical location / Geo-IP?

**Approved:** Route **data-bearing traffic by tenant residency policy**. The tenant's contractual, regulatory and platform residency policy overrides the user's current location.

> **Approved scenario:** an employee of an EU-residency tenant logging in from the US must have data-bearing traffic routed to the EU pool.
> `EU-residency tenant + US-based user session = EU data-bearing route`
> `EU-residency tenant + US-based user session ≠ US route`

### 5.1 Route Resolution Order
1. Resolve tenant/workspace identity at the edge from the hostname.
2. Read `data_residency_policy_id` and `policy_version` from the compiled routing map.
3. Limit candidate regions to `allowed_regions`.
4. Route to `primary_region` where healthy and available.
5. Route to `fallback_region` only if an approved fallback exists and the compiler emitted a fallback service.
6. Apply manual quarantine override only where authorised, compiled, and residency-safe.

Geo-IP must never appear before tenant residency policy in the decision chain.

### 5.2 Geo-IP Rules
| Permitted Use | Prohibited Use |
|---|---|
| Logging, telemetry, security anomaly detection | Data-bearing routing that conflicts with tenant residency |
| Fraud / risk scoring | DB write routing outside `allowed_regions` |
| Display localisation and language hints | Document/payroll/tax/HR/identity/accounting/invoice/ledger/compliance/audit routing outside `allowed_regions` |
| Non-data-bearing static content routing | Silent fallback to a non-compliant region |
| Future latency optimisation *inside* a compliant region set | Any route decision based on client-supplied geo headers |

## 6. Tenant Resolution and Edge Hardening

### 6.1 Phase 1 Tenant Resolution Standard
Hostname-based; the tenant slug in the Host header is the routing key:
```
{tenant_slug}.zoikosuite.{env-domain}      e.g. acme.zoikosuite.dev.internal
```
- Tenant slugs must be canonical, unique, immutable or change-controlled, generated from tenant records.
- The compiler rejects duplicated, malformed or reserved slugs.
- The edge allows only approved environment domains; unknown hostnames terminate at the safe resolution path.
- Token/JWT-based edge tenant resolution is deferred; it must not block Phase 1.

### 6.2 Edge Hardening Requirements
| Risk | Mandatory Control |
|---|---|
| Host-header spoofing | Allow-list environment domains; reject malformed Host headers; never route unknown/ambiguous hostnames to a default pool. |
| Trusted header injection | Strip inbound `X-Zoiko-Tenant`, `X-Zoiko-Resolved-Region`, `X-Zoiko-GTRM-Map-Version`, `X-Zoiko-Residency-Policy` at external ingress before setting internal values. |
| Forwarded-host confusion | Do not trust `X-Forwarded-Host` from the public internet; trust only known upstream proxies if explicitly configured. |
| Client-supplied geo spoofing | Treat client geo headers as untrusted — zero influence on data-bearing routing. |
| Auth/host mismatch | After auth, the app must reject a session token whose tenant/workspace claim ≠ hostname-resolved tenant. |
| SNI/TLS ambiguity | Use environment-appropriate TLS and wildcard/cert coverage; never route over an unvalidated host pattern. |

> **Security rule:** the edge may set internal routing-context headers *after* stripping external copies. Backends may trust only headers injected by the trusted edge path, never headers received directly from clients.

## 7. Traffic Classification

| Class | Definition | Routing Rule |
|---|---|---|
| **Data-bearing** | Any request that reads/writes/processes tenant data — authenticated APIs, DB-backed pages, document/payroll/tax/HR/identity/accounting/invoice/ledger/compliance/audit traffic. | Residency-enforced. Route only within `allowed_regions`. |
| **Non-data-bearing** | Marketing pages, public assets, status pages, pre-tenant-resolution bootstrap that expose/process/mutate no tenant data. | May be residency-neutral, but must not leak tenant context or load tenant-specific data. |
| **Ambiguous** | Anything that could plausibly involve tenant data or identity context. | Classify as data-bearing by default. |

Login discovery is permitted before tenant resolution only as a non-data-bearing bootstrap. Once a tenant is identified or an authenticated session begins, the route becomes residency-enforced.

## 8. Fail-Closed, Failover and Failback

### 8.1 Fail-Closed Behaviour
Unknown residency, unresolved tenant, `SUSPENDED`/`PENDING_RESOLUTION` status, invalid compiled policy, or unsafe host resolution must **not** route to a default regional pool.
```
Unknown tenant residency                    = no data-bearing regional route
Unknown tenant hostname                      = safe resolution path
routing_status = SUSPENDED|PENDING_RESOLUTION = safe resolution path
```
- An explicit lowest-priority catch-all router terminates unresolved traffic at a safe endpoint.
- The safe endpoint exposes/processes/mutates no tenant data.
- It emits a structured log with `route_decision_reason = residency_unresolved | host_unresolved | tenant_suspended | policy_invalid`.
- It must not silently route to EU, US or any fallback pool.

### 8.2 Backend Region Assertion
Each regional pool knows its own region identity and rejects requests whose resolved-region marker / tenant residency context / gateway-set route context does not match. Second line of defence against compiler bugs, config drift and direct-to-backend misroutes.
```
Pool identity: eu-pool
Request marker: X-Zoiko-Resolved-Region: us
Expected result: reject, log violation, no tenant data processed
```

### 8.3 Failover
- Allowed only where `fallback_region` is configured and validated.
- Tenants with `fallback_region = null` get no failover service emitted.
- Health checks may trigger failover only within the compiled approved fallback path.
- Non-compliant fallback prevention must be tested (disable primary, leave a non-approved pool healthy).

### 8.4 Failback
Manual and sticky in Phase 1. When a primary pool recovers, traffic must not auto flap back. An operator applies a reviewed config change to restore primary routing; the transition is logged.

## 9. Decision 3 — Quarantine / Incident Diversion

**Question:** Manual or automatically triggered by a security alert?

**Approved:** **Manual** quarantine diversion for Phase 1. Automated alert-to-routing diversion is deferred until the observability, incident and approval model exists.

### 9.1 Quarantine Modes
| Mode | Description | Residency Control | Phase 1 Status |
|---|---|---|---|
| **BLOCK** | Traffic terminated at a residency-neutral incident endpoint (e.g. 503 with incident reference). No tenant data processed. | May use a shared terminator (no tenant data processed). | **Default and required.** |
| **ISOLATED_SERVE** | Traffic served by an isolated pool during an incident. | Must use region-scoped pools (`eu-quarantine-pool` / `us-quarantine-pool`); compiler validates compatibility with `allowed_regions`. | Optional. Not required for Phase 1. |

### 9.2 Manual Control
The Phase 1 switch is a reviewed change to the routing map through GitOps: operator raises the change → second authorised person approves → compiler validates → pipeline applies. No out-of-band edits.
```
target_service / tenant_scope / region_scope / quarantine_mode (BLOCK|ISOLATED_SERVE)
quarantine_pool (ISOLATED_SERVE only) / reason_code / operator_id / approver_id
timestamp / rollback_reference / status
```

### 9.3 Quarantine States
```
NORMAL → QUARANTINE_PENDING → QUARANTINED → ROLLBACK_PENDING → RESTORED
```

### 9.4 Safeguards
- Two-person approval mandatory for activation and rollback.
- Long-lived connections (WebSockets/streaming) terminated or drained on config apply so pre-existing sessions can't bypass quarantine.
- Quarantine reason is an internal log field only — never returned in client-visible headers.
- Tenant-scoped and service-scoped diversion must be supported.
- Rollback to RESTORED must be tested and evidenced.

## 10. Decision 4 — Proof Standard

**Question:** Real multi-region cloud deployment, or simulated regional pools in the dev stack?

**Approved:** **Simulated regional pools** in the development stack. Real multi-region cloud deployment is out of scope for Phase 1.

### 10.1 Minimum Simulation Environment
```
eu-pool
us-pool
quarantine-terminator      # BLOCK mode
eu-quarantine-pool         # only if ISOLATED_SERVE is exercised
us-quarantine-pool         # only if ISOLATED_SERVE is exercised
```
- Each pool independently identifiable in logs and proof responses.
- Each regional pool implements backend region assertion.
- The environment must simulate: primary pool failure, missing policy, invalid policy, quarantine activation, quarantine rollback, and manual failback.

### 10.2 Proof Header Hygiene
Proof headers are for dev/test only; stripped or gated behind a secure internal debug mechanism in production-equivalent environments.
```
X-Zoiko-Resolved-Tenant: tenant_acme_eu
X-Zoiko-Residency-Policy: EU
X-Zoiko-Resolved-Region: eu
X-Zoiko-Route-Decision: tenant_residency
X-Zoiko-GTRM-State: normal
X-Zoiko-GTRM-Map-Version: 47
```
Do not expose quarantine reasons, internal region policy, compiler metadata, tenant/workspace IDs, security states or incident causes to clients without a security review.

## 11. Required Phase 1 Acceptance Criteria

Phase 1 is **not complete** until all tests A–P pass **and** the evidence pack is produced.

| ID | Test | Scenario | Expected Result |
|---|---|---|---|
| A | Tenant residency routing | EU-residency tenant, user in US | Route = eu-pool |
| B | US tenant routing | US-residency tenant, user in EU | Route = us-pool |
| C | Unknown residency fail-closed | Known tenant, missing `data_residency_policy_id` | Safe resolution path; data-bearing denied; log `residency_unresolved` |
| D | Non-compliant fallback prevention | EU tenant; eu-pool down; us-pool up; no approved fallback | No route to us-pool; no failover service in compiled config |
| E | Approved fallback | Multi-region tenant; primary EU down; approved fallback configured | Route = approved fallback only; `fallback_used = true` |
| F | Manual quarantine | EU accounting-ledger-api scope; BLOCK enabled | Traffic terminated at quarantine terminator; no tenant data processed; operator/approver logged |
| G | Auditability | Any data-bearing request | Structured decision log with tenant, policy, map version, region, reason, fallback, state |
| H | Configuration validation | Bad map: route outside `allowed_regions`, invalid failover, invalid quarantine pool | Compiler rejects all invalid entries and blocks deployment |
| I | Drift detection | Manual out-of-band edit to live Traefik config | Drift check detects and alerts within defined interval |
| J | Geo-IP non-influence | EU tenant request with spoofed US geo headers | Route remains eu-pool; client location inert |
| K | Quarantine rollback | Current state = QUARANTINED | QUARANTINED → ROLLBACK_PENDING → RESTORED; normal route resumes |
| L | Manual failback | Primary recovers while approved fallback active | Traffic stays on fallback until reviewed failback change applied |
| M | Wrong-policy defence | EU-tenant-marked request sent directly to us-pool | Backend region assertion rejects and logs violation |
| N | Header injection defence | External request with spoofed `X-Zoiko-Tenant` / `X-Zoiko-Resolved-Region` | Edge strips inbound headers; sets trusted values only after resolving tenant |
| O | Token/hostname mismatch | Token claims tenant A; hostname resolves tenant B | App rejects session; no tenant data processed |
| P | Log hygiene and residency | Routing decision generates logs | Logs include required fields, exclude secrets/content, stored per approved logging policy |

### 11.1 Required Log Fields
```
request_id / tenant_id / workspace_id / data_residency_policy_id / policy_version
gtrm_map_version / resolved_region / route_decision_reason / fallback_used
quarantine_state / routing_status / compiler_version / timestamp
```
Logs must not contain secrets, session tokens, document/payroll/tax/ledger contents, personal data beyond necessary operational identifiers, or incident details unsuitable for routine logs.

## 12. Engineering Build Instructions

1. Implement the GTRM routing map (incl. `schema_version`, `map_version`, `policy_version`, `change_reference`).
2. Bind tenant/workspace identity to residency policy; implement hostname-based tenant resolution.
3. Implement edge hardening: Host allow-listing, external header stripping, trusted proxy rules, no default regional route.
4. Build the configuration compiler and validation gates (§4).
5. Configure Traefik as the enforcement layer, consuming only compiled config.
6. Create simulated `eu-pool`, `us-pool`, `quarantine-terminator`; add region-scoped quarantine pools only if ISOLATED_SERVE is exercised.
7. Implement backend region assertion in every simulated regional pool.
8. Implement the safe resolution endpoint and lowest-priority catch-all router.
9. Implement manual quarantine activation and rollback via the GitOps two-person flow.
10. Implement manual sticky failback via reviewed routing-map change.
11. Add deterministic tests covering acceptance criteria A–P.
12. Produce the Phase 1 evidence pack and obtain architecture sign-off before calling Phase 1 complete.

## 13. Evidence Pack Required Before Sign-Off

- Architecture diagram (tenant records, routing map, compiler, Traefik, pools, quarantine terminator, safe endpoint, backend assertion).
- Routing-map schema and a sample versioned map.
- Compiler validation rules and sample rejection output (invalid routes / fallback / quarantine).
- Drift-detection mechanism description and alert evidence.
- Compiled Traefik routing configuration with generation provenance.
- Test matrix and execution output for A–P.
- Logs/screenshots showing EU, US, fallback, quarantine, rollback and manual failback routes.
- Evidence of fail-closed behaviour, backend region assertion, header stripping, Geo-IP non-influence, token/hostname mismatch rejection.
- Manual quarantine runbook (connection-drain/termination behaviour, rollback steps).
- Known limitations, deferred items, and Phase 2 recommendations.

## 14. Ownership and Control Responsibilities

| Area | Owner | Approver / Reviewer | Required Output |
|---|---|---|---|
| Tenant residency policy | Product / Platform Governance | Legal / Compliance / Architecture | Authoritative tenant policy + approved fallback (if any) |
| Routing map | Platform Engineering | Architecture | Versioned map with change reference and policy version |
| Compiler and validation | Platform Engineering | Architecture / Security | Compiler, validation gates, rejection output, provenance stamping |
| Traefik enforcement | Infrastructure Engineering | Architecture | Compiled routes only, no hand-edited policy routes |
| Edge security | Security Engineering | Infrastructure Engineering | Header stripping, Host validation, trusted proxy policy, debug-header hygiene |
| Backend region assertion | Application Engineering | Security / Architecture | Pool-level rejection of misrouted requests |
| Quarantine activation | Operations | Second authorised approver + engineering leadership visibility | Two-person reviewed change and rollback reference |
| Evidence pack | Engineering Lead | Architecture | Complete acceptance evidence before Phase 1 sign-off |

## 15. Prohibited Phase 1 Work

- Custom global routing microservice.
- Cloud-vendor-specific global load balancing dependency.
- Service mesh dependency solely for GTRM.
- CDN-vendor routing dependency as the Phase 1 standard.
- Routing by user physical location where it conflicts with tenant residency.
- Client-supplied location or tenant headers influencing data-bearing routing.
- Hand-edited Traefik rules bypassing the compiler.
- Automatic quarantine triggered by security alerts.
- Automatic failback.
- Single shared serve-mode quarantine pool for data-bearing tenant traffic.
- Default routing of unresolved tenant traffic to any regional pool.
- Client-visible disclosure of quarantine reason, internal route policy, tenant identifiers or security state.

## 16. Phase 2 / Later-Phase Considerations (deferred — must not block Phase 1)

Real multi-region cloud deployment · cloud-native global load balancer / global traffic manager · dedicated GTRM routing service · service mesh routing · automated security-triggered quarantine · automatic failback · token/JWT-based edge tenant resolution · customer-configurable residency rules · cross-region data replication and region-specific DB write routing · production-grade sovereign-region deployment · full DR SLOs · production-grade ISOLATED_SERVE quarantine.

Revisit after the Kubernetes/cloud regional footprint, observability layer, incident workflow, data replication model, compliance controls and operational SLOs are mature enough.

## 17. Final Approval Instruction

> Engineering is authorised to proceed with GTRM Phase 1 using Traefik-native enforcement, tenant-residency-controlled routing, hostname-based tenant resolution, compiled configuration, drift detection, manual block-mode quarantine, manual sticky failback, backend region assertion and simulated regional pools.
>
> Conditional on preserving the separation between tenant residency policy, the GTRM routing map, the compiler, Traefik enforcement configuration and backend region assertion.
>
> The Phase 1 objective is **not** to build global cloud infrastructure — it is to prove that ZoikoSuite can **make, enforce, audit and safely fail** residency-aware routing decisions.

---

## Appendix A — Sample GTRM Routing Map

```yaml
schema_version: 1
map_version: 47
compiler_min_version: 0.1.0
tenants:
  - tenant_id: tenant_acme_eu
    tenant_slug: acme
    workspace_id: workspace_acme_primary
    data_residency_policy_id: residency_eu_001
    policy_version: 12
    allowed_regions: [eu]
    primary_region: eu
    fallback_region: null
    quarantine_mode: BLOCK
    quarantine_pool: null
    routing_status: ACTIVE
    last_updated_at: 2026-07-10T12:00:00Z
    updated_by: platform-governance
    change_reference: GTRM-47
  - tenant_id: tenant_atlas_multi
    tenant_slug: atlas
    workspace_id: workspace_atlas_primary
    data_residency_policy_id: residency_multi_002
    policy_version: 6
    allowed_regions: [eu, uk]
    primary_region: eu
    fallback_region: uk
    quarantine_mode: BLOCK
    quarantine_pool: null
    routing_status: ACTIVE
    last_updated_at: 2026-07-10T12:05:00Z
    updated_by: platform-governance
    change_reference: GTRM-48
```

## Appendix B — Sample Route Decision Log

```json
{
  "request_id": "req_01JZK9A7GTRM",
  "tenant_id": "tenant_acme_eu",
  "workspace_id": "workspace_acme_primary",
  "data_residency_policy_id": "residency_eu_001",
  "policy_version": 12,
  "gtrm_map_version": 47,
  "resolved_region": "eu",
  "route_decision_reason": "tenant_residency",
  "fallback_used": false,
  "quarantine_state": "NORMAL",
  "routing_status": "ACTIVE",
  "compiler_version": "0.1.0",
  "timestamp": "2026-07-10T12:30:20Z"
}
```

## Appendix C — Manual Quarantine Runbook Control Points

1. Identify affected `tenant_scope`, `service_scope`, `region_scope`.
2. Select BLOCK mode unless architecture/security has explicitly approved ISOLATED_SERVE for the tenant's allowed region set.
3. Create routing-map change with `reason_code` and `rollback_reference`.
4. Obtain second authorised approval.
5. Run compiler validation; confirm no residency violation.
6. Deploy compiled config through the approved pipeline.
7. Terminate or drain long-lived connections per service class.
8. Verify traffic terminates at quarantine terminator; no tenant data processed.
9. Log QUARANTINED state; notify engineering leadership.
10. Rollback only through reviewed routing-map change; verify RESTORED state and normal routing.

## Appendix D — Minimal Traefik Implementation Notes

Illustrative only. Binding implementation may use file-based dynamic config or Traefik Kubernetes CRD / IngressRoute depending on the current dev stack.
- One compiled router per tenant slug and service class is acceptable for Phase 1.
- Use priority ordering so explicit tenant routes win before catch-all safe resolution routes.
- Generate middleware for external header stripping before internal routing context is set.
- Emit failover service definitions only when `fallback_region` is approved.
- Use health checks for approved fallback only; do not use health checks to discover unapproved regions.
- Record `map_version` in route decision logs and proof headers in non-production environments.
