# Blocks 6-10: canonical service matrix and port-conflict analysis

Status: **historical analysis (2026-10-28).** The resolution has since been implemented on `fix/blocks-6-10-final`:
- The 20 phase-only services were added to the main compose.
- 16 host ports were moved to 8190–8205; container ports are unchanged.
- filing-preparation is on 8176 and compliance-risk-scoring on 8177, as on main.
- All 37 services have gateway routes.

Current mapping: `deployments/traefik-dynamic/ROUTES.md`. Evidence: `blocks-6-10-verification-2026-11-02.md`.
Evidence collected 2026-10-28 from `origin/main` @ `3552ddba` (backend) and `7281b88` (frontend).

## Sources compared

| Column | Source |
|---|---|
| Container port | each service's `internal/config` default for `PORT` (what the binary listens on) |
| Main stack host port | `deployments/docker-compose.yml` (`docker compose config --format json`) |
| Phase stack host port | `deployments/docker-compose.phase5/6/7.yml` |
| Brief | frontend `docs/ZOIKOSUITE_PLATFORM_BRIEF.md` §3.6-§3.10 |
| Frontend direct-mode | frontend `lib/api/config.ts` `DEFAULTS` |
| Gateway route | `deployments/traefik-dynamic/all-services.yml` (generated from `docker-compose.yml` by `deployments/scripts/gen-gateway-routes.mjs`) |

## Matrix

| Block | Service (container name) | Container port | Main stack host | Phase stack host | Brief | Frontend direct-mode | Gateway route |
|---|---|---|---|---|---|---|---|
| 6 | `contract-lifecycle-svc` | 8119 | 8119 | phase5:8119 | 8119 | 8119 | yes |
| 6 | `clause-template-svc` | 8120 | 8120 | phase5:8120 | 8120 | 8120 | yes |
| 6 | `obligation-tracking-svc` | 8121 | 8121 | phase5:8121 | 8121 | 8121 | yes |
| 6 | `board-resolutions-svc` | 8122 | 8122 | phase5:8122 | 8122 | 8122 | yes |
| 6 | `corporate-actions-svc` | 8123 | 8123 | phase5:8123 | 8123 | 8123 | yes |
| 6 | `counterparty-management-svc` | 8124 | 8124 | phase5:8124 | 8124 | 8124 | yes |
| 6 | `purchase-request-svc` | 8100 | 8100 | — | 8100 | 8100 | yes |
| 6 | `purchase-order-svc` | 8129 | 8129 | — | 8129 | **8150** | yes |
| 6 | `spend-controls-svc` | 8131 | 8131 | — | 8131 | 8131 | yes |
| 6 | `vendor-due-diligence-svc` | 8135 | 8135 | — | 8135 | **8132** | yes |
| 6 | `procurement-workflow-svc` | 8134 | 8134 | — | 8134 | 8134 | yes |
| 7 | `tax-rules-svc` | 8125 | 8125 | phase5:8125 | 8125 | 8125 | yes |
| 7 | `tax-determination-svc` | 8126 | 8126 | phase5:8126 | 8126 | 8126 | yes |
| 7 | `vat-gst-svc` | 8127 | 8127 | phase5:8127 | 8127 | 8127 | yes |
| 7 | `corporate-tax-svc` | 8128 | 8128 | phase5:8128 | 8128 | 8128 | yes |
| 7 | `withholding-tax-svc` | 8129 | — | phase5:8129 | - | 8129 | **no** |
| 7 | `filing-preparation-svc` | 8130 | — | phase5:8130 | - | 8130 | **no** |
| 7 | `filing-tracker-svc` | 8131 | — | phase5:8131 | - | **8151** | **no** |
| 7 | `compliance-status-svc` | 8132 | — | phase5:8132 | - | **8152** | **no** |
| 7 | `exception-escalation-svc` | 8133 | — | phase5:8133 | - | 8133 | **no** |
| 8 | `anomaly-detection-svc` | 8134 | — | phase6:8134 | 8134 | **8153** | **no** |
| 8 | `forecasting-svc` | 8135 | — | phase6:8135 | 8135 | 8135 | **no** |
| 8 | `compliance-risk-scoring-svc` | 8136 | — | phase6:8136 | 8136 | 8136 | **no** |
| 8 | `reconciliation-intelligence-svc` | 8137 | — | phase6:8137 | 8137 | 8137 | **no** |
| 8 | `reporting-orchestration-svc` | 8138 | — | phase6:8138 | 8138 | 8138 | **no** |
| 8 | `decision-support-svc` | 8138 | 8138 | — | 8138 | **8154** | yes |
| 8 | `migration-integrity-svc` | 8139 | — | phase6:8139 | 8139 | 8139 | **no** |
| 9 | `mtls-management-svc` | 8140 | 8140 | phase6:8140 | 8140 | 8140 | yes |
| 9 | `siem-integration-svc` | 8141 | — | phase6:8141 | 8141 | 8141 | **no** |
| 9 | `carta-svc` | 8142 | — | phase6:8142 | 8142 | 8142 | **no** |
| 9 | `key-management-svc` | 8143 | — | phase6:8143 | 8143 | 8143 | **no** |
| 10 | `connectivity-api-bridge-svc` | 8144 | — | phase7:8144 | 8144 | 8144 | **no** |
| 10 | `banking-connector-svc` | 8145 | — | phase7:8145 | 8145 | 8145 | **no** |
| 10 | `hris-connector-svc` | 8146 | — | phase7:8146 | 8146 | 8146 | **no** |
| 10 | `tax-authority-interface-svc` | 8147 | — | phase7:8147 | 8147 | 8147 | **no** |
| 10 | `esignature-integration-svc` | 8148 | — | phase7:8148 | 8148 | 8148 | **no** |
| 10 | `external-data-feed-svc` | 8149 | — | phase7:8149 | 8149 | 8149 | **no** |

Bold = differs from the container port, or missing.

## Findings

1. **No two services in one stack share a container port.** Every container has its own network namespace; the
   "duplicate ports" in the domain sheet (8129, 8131, 8132, 8134, 8135, 8138) are the *code defaults* of a service
   in the main stack and a different service that exists only in a phase stack. They only collide when both stacks
   publish host ports on the same machine.
2. **20 of the 37 services exist only in the phase5/6/7 compose files**, so the gateway generator — which reads
   `docker-compose.yml` only — produces no route for them. Gateway mode in the frontend therefore cannot reach them.
3. **The frontend direct-mode registry points 6 services at ports nothing listens on in either stack:**
   purchase-order 8150 (container/host 8129), vendor-due-diligence 8132 (8135), filing-tracker 8151 (8131),
   compliance-status 8152 (8132), anomaly-detection 8153 (8134), decision-support 8154 (8138).
   In a stack where 8150 is published by source-authority-svc, purchase-order calls reach the wrong service.
4. **decision-support-svc and reporting-orchestration-svc both default to 8138.** In the main stack only
   decision-support exists; in phase6 only reporting-orchestration. Same container port is legal; the conflict is
   only a host-publishing one.

## Recommended resolution (needs approval before it is applied)

- Keep every container port as it is (no code change; the brief and code agree for all services listed in it).
- Add the 20 phase-only services to `deployments/docker-compose.yml` with their container port, app-role DB users
  and health checks, then regenerate the gateway routes. Gateway mode then reaches all 37 by `/<container-name>`.
- For host publishing in the single main stack, give the five colliding newcomers distinct host ports and use
  those same ports in the frontend direct-mode registry, e.g. withholding-tax 8151, filing-tracker 8152,
  anomaly-detection 8153, forecasting 8155, reporting-orchestration 8156 (exact numbers to be agreed; each must be
  checked against every other published port first).
- Point the frontend direct-mode registry at the main-stack host ports: purchase-order 8129, vendor-due-diligence
  8135, compliance-status 8132, decision-support 8138, and the agreed host ports above.
- Retire (or clearly mark as legacy) the phase5/6/7 compose files once the main stack carries all 37.
