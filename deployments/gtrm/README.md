# GTRM — Global Traffic & Residency Manager (Phase 1)

Implements the residency-aware routing pipeline approved in
[`docs/architecture/global-traffic-residency-manager-decision.md`](../../docs/architecture/global-traffic-residency-manager-decision.md)
(Final Approved v1.2).

This directory holds the **policy → compiler → enforcement** pipeline the
decision mandates. GTRM is **not** a runtime microservice (that's explicitly
prohibited for Phase 1, §15) — routing policy is *authored* here and *compiled*
into Traefik configuration.

```
routing-map.yaml   ── authored routing policy (single source, §4.1)
regions.yaml       ── closed region catalog (§4.2 "unknown region codes")
        │
        ▼
compiler/          ── validates the map, emits Traefik dynamic config (§4.2)
        │
        ▼
compiled-traefik.yml (generated — never hand-edited, §4.3)
        │
        ▼
Traefik file provider  ── enforcement only (wired in a later chunk)
```

## What's here (chunk G1)

- **`routing-map.yaml`** — the authored policy: per-tenant residency, allowed
  regions, primary/fallback, quarantine mode, routing status.
- **`regions.yaml`** — the closed set of valid region codes → logical pools.
- **`compiler/`** — a Go tool (the "integrity gate", §2) that:
  - validates the map against every §4.2 gate (schema, allowed-region,
    fallback, quarantine, orphan/duplicate, environment, header policy) and
    **rejects the whole map** on any violation, emitting every error at once;
  - emits Traefik dynamic config — one router per tenant slug, header-stripping
    + trusted-context middlewares, failover services **only** where an approved
    fallback exists, and a lowest-priority fail-closed catch-all to a
    residency-neutral safe endpoint (§8.1);
  - stamps provenance (map version, compiler version, commit, timestamp).

Fail-closed is enforced structurally: a tenant whose `routing_status` is not
`ACTIVE` gets **no** data-bearing router, so its traffic falls through to the
safe catch-all rather than any regional pool.

## Usage

```bash
cd compiler
go test ./...                                    # validation + emission tests
go run . --map ../routing-map.yaml \
         --regions ../regions.yaml \
         --commit "$(git rev-parse --short HEAD)" \
         --out ../compiled-traefik.yml           # compile

# CI / prod safety: refuse to generate prod-equivalent config from a non-prod map
go run . --map ../routing-map.yaml --regions ../regions.yaml --require-prod-safe
```

A bad map exits non-zero and prints every violation, producing **no** config
(acceptance test H).

## Not yet built (later chunks)

- **G2** — wire the compiled config into the Traefik gateway; simulated
  `eu-pool` / `us-pool` / `quarantine-terminator`; edge host allow-listing.
- **G3** — backend region assertion in each pool; approved-fallback failover
  and manual sticky failback demos.
- **G4** — manual BLOCK quarantine via GitOps two-person flow; drift detection;
  route-decision logs; the full A–P acceptance suite + evidence pack.
