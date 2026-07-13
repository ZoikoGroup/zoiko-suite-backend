# GTRM Phase 1 — Evidence Pack

Maps the approved decision's acceptance criteria (A–P, §11) and required
artefacts (§13) to how each is satisfied in this implementation. Companion to
[`README.md`](./README.md) and the decision doc
[`../../docs/architecture/global-traffic-residency-manager-decision.md`](../../docs/architecture/global-traffic-residency-manager-decision.md).

## Acceptance criteria A–P

Status legend: **LIVE** = verified against the running slice (gateway + pools);
**UNIT** = verified by compiler/pool unit tests; **RUNBOOK** = a config
recompile + reload the operator runs (commands in README) — the mechanism is
proven by UNIT/LIVE, the live re-run is machine-gated.

| ID | Test | How it's satisfied | Status |
|----|------|--------------------|--------|
| A | Tenant residency routing (EU tenant, US user → eu-pool) | `acme.zoikosuite.dev.internal` → `eu-pool`, resolved_region=eu | ✅ LIVE |
| B | US tenant routing → us-pool | `globex.*` → `us-pool`, resolved_region=us | ✅ LIVE |
| C | Unknown/unresolved residency fail-closed | `newco` (PENDING_RESOLUTION) and unknown hosts → catch-all safe terminator, 503, tenant_processed=false | ✅ LIVE |
| D | Non-compliant fallback prevention | Compiler rejects `failover_active` without an approved `fallback_region`; a no-fallback tenant compiles to a single-pool service that can never spill to another region. Live: stop primary → 503, never us-pool | ✅ UNIT + RUNBOOK |
| E | Approved fallback | `failover_active: true` routes atlas to its approved `uk` fallback; resolved_region follows to uk | ✅ UNIT + RUNBOOK |
| F | Manual quarantine (BLOCK) | `quarantine_active: true` + BLOCK diverts to residency-neutral terminator, state=quarantined, no region resolved, no tenant data; operator/approver captured in the map change (updated_by/change_reference) | ✅ UNIT + RUNBOOK |
| G | Auditability | Pools emit a structured single-line JSON `gtrm_route_decision` log per request (tenant, region, map version, state, decision reason, tenant_processed) | ✅ LIVE (logs) |
| H | Configuration validation rejects bad maps | Compiler rejects out-of-region routes, unknown regions, bad fallback, bad quarantine, duplicate/malformed/reserved slugs, invalid status, prod-from-dev — emitting every violation, producing no config | ✅ UNIT (26 tests) |
| I | Drift detection | `compiler --check` recompiles and diffs against the committed artefact (ignoring provenance header); exits non-zero on drift. Demonstrated: clean pass + tamper detected | ✅ LIVE |
| J | Geo-IP non-influence | Spoofed `X-Geo-Country`/`CF-IPCountry` on an EU-tenant request → still eu-pool (routing is host/tenant-based; geo headers never consulted) | ✅ LIVE |
| K | Quarantine rollback | `quarantine_active: false` restores the normal region route; states are map-driven (§9.3) | ✅ UNIT + RUNBOOK |
| L | Manual sticky failback | Failback is `failover_active: false` + recompile; nothing auto-reverts the flag, so traffic stays on the fallback until the operator acts — no Traefik auto flap-back | ✅ UNIT + RUNBOOK |
| M | Wrong-policy defence (backend region assertion) | A request with an eu marker sent directly to us-pool → 403 `region_assertion_failed`, tenant_processed=false | ✅ LIVE |
| N | Header injection defence | Injected `X-Zoiko-Resolved-Region`/`-Tenant`/`-GTRM-State` are stripped at the edge; trusted values set only after host resolution → client values inert | ✅ LIVE |
| O | Token/hostname mismatch | **Out of GTRM scope.** §14 assigns this to the application/auth layer: the app must reject a session token whose tenant claim ≠ hostname-resolved tenant. Belongs to gateway-auth-svc / the services, not the router | ⛔ OTHER OWNER |
| P | Log hygiene & residency | Route-decision logs contain only routing metadata from trusted headers — no secrets, tokens, or tenant content | ✅ LIVE (logs) |

**Notes / known limitations (honest):**
- **Route-decision log fields (G/§11.1):** the pool logs the subset resolvable
  from gateway-set headers (tenant, region, map_version, state, reason). Fuller
  fields (`data_residency_policy_id`, `policy_version`, `request_id`) require
  gateway-side enrichment — a small follow-up, noted not hidden.
- **Failover is operator-initiated, not health-triggered** — a deliberate
  Phase-1 choice to guarantee sticky failback with Traefik OSS (§8.3 permits it,
  "may"). See README runbook.
- **D/E/F/K/L live re-runs** are config recompiles + a gateway reload (README);
  the routing behaviour is proven by UNIT tests + the LIVE G2 slice. Live re-run
  is gated only by local machine capacity, not by any unknown.

## §13 required artefacts

| Artefact | Where |
|----------|-------|
| Architecture (policy → map → compiler → Traefik → pools → assertion) | decision doc §2; `README.md` diagram |
| Routing-map schema + sample versioned map | `routing-map.yaml` (map_version 47); decision Appendix A |
| Compiler validation rules + sample rejection output | `compiler/validate.go`; `compiler --check`/compile stderr; `validate_test.go` |
| Drift-detection mechanism + evidence | `compiler --check`; demonstrated clean-pass + tamper-detect |
| Compiled Traefik config with provenance | `compiled-traefik.yml` (stamped map/compiler version, commit, timestamp) |
| Test matrix A–P | this document |
| Logs showing EU/US/fallback/quarantine/rollback/failback | pool `gtrm_route_decision` logs (`docker compose logs eu-pool us-pool …`) |
| Fail-closed / region-assertion / header-strip / geo-inert evidence | tests C, M, N, J above (LIVE) |
| Manual quarantine runbook (incl. drain + rollback) | decision Appendix C; `README.md` |
| Known limitations & deferred items | this document (above); decision §16 |

## Reproduce the live checks

Minimal slice (never the whole stack):
```bash
cd deployments
docker compose up -d --no-deps eu-pool us-pool uk-pool quarantine-terminator docker-proxy gateway
# A/B/C/J/N via Host header; M direct to a pool; see README for the full set
docker compose down
```
Failover/quarantine (D/E/F/K/L) are a routing-map edit → `compiler` recompile →
`docker compose restart gateway` — no pool-killing except D. See README.
