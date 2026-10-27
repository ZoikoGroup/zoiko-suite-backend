# Suivo R0 — readiness record

**Decision: NO-GO / NOT VERIFIED.** No gate below has been approved. This file records the evidence the
engineering build has produced so far and what each gate still needs. It must never be edited to show a
gate as passed without the named approver's signed record (Doc 11 §15, §24).

Build: branch `suivo/r0-build`, service `services/public-ai-svc`. Evidence from the dev build is on
TEST-ONLY fixtures with the fake model; it proves mechanisms, not production behaviour.

## Gates (Doc 11 §15 is the master list)

| Gate | Approvers (Doc 11) | Status | Evidence produced | Still needed |
|---|---|---|---|---|
| G0 Discovery & reconcile | Product, CTO, Security, DPO | **PENDING** | `docs/suivo/g0-architecture-delta.md`, ADR-001..005, document register | Named approvers; v2.1 taxonomy (DEC-02); claim owners (DEC-03); hosting (DEC-01) |
| G1 Secure foundation | CTO, Architect, Security | **PENDING** | OpenAPI 3.1 contract, isolation test in CI, signed releases, migrations, separate compose stack/network | Vendor/region decision (DEC-05); TTLs (DEC-07); signing-key custody (DEC-09); threat-model sign-off |
| G2 Vertical slice | Backend, AI, QA, Security | **PENDING** | Pipeline tests (happy path, denial, grounded price, fallback, citation), e2e test, live smoke run with 0 log leaks | Run on staging with real (approved) content and an approved model |
| G3 Broaden & harden | QA, Security, DPO, Product | **PENDING** | Golden dev set 25/25 (0 P0 failures), redaction/injection/exception tests, revocation tests | Holdout set by independent authors (Doc 07 E); semantic + human grading; WCAG (widget not built); DPIA; pentest |
| G4 Operate & pilot | SRE, Security, QA | **PENDING** | Four-artifact signed manifest (`kbctl manifest`), drills DR-01..05 passing on memory and Postgres, RUNBOOK RB-01..04, rate/spend limits, kill switch | Named on-call and owners (DEC-16); SLOs (DEC-08); alerts/dashboards; drills repeated on staging |
| G5 R0 launch decision | Founder + CTO, Security, DPO, QA, SRE | **PENDING** | — | Every non-waivable stop cleared; permitted markets; brand clearance (DEC-04); pilot plan |

## Doc 11 §16 non-waivable stops

| Stop | Current state |
|---|---|
| Anonymous customer-data reachability | Code-level isolation tested (SEC-T049); network egress policy in the target environment not verified |
| Material commercial/trust hallucination | Prices/trials/certifications only from verified claims; numbers not in evidence are rejected; tested on fixtures only |
| Revoked/expired source still answerable | Tested (retrieval + commit re-check + rollback drill) |
| Unapproved model processing or residency | Only the fake model exists; any other profile refuses to start |
| Missing Security, SRE or QA sign-off | All PENDING |
| Rollback or kill switch cannot be executed | Drills pass in dev; not yet executed on staging |
| Handoff/CRM false-success state | Route-only; prohibited phrases blocked; no CRM/case code in R0 |
| P0 accessibility obstruction | Not assessed — widget not built (STEP 13, frontend repo) |

## Operational artefacts

- `services/public-ai-svc/Dockerfile` — non-root, read-only rootfs compatible, healthcheck, git SHA baked in
- `deployments/docker-compose.public-ai.yml` — isolated stack (own Postgres, own network), verified:
  not ready until signed releases are published, then serves; container healthy
- `services/public-ai-svc/RUNBOOK.md` — controls, RB-01..RB-04, drills, release record
- `cmd/drill`, `cmd/evalrun`, `kbctl manifest`

## Known gaps

- Widget (STEP 13) not built; WCAG, browser and responsive testing therefore not done.
- No load/latency measurement; proposed p95 ≤ 8 s is unverified.
- No alert rules or dashboards wired (OTel attributes are defined in Doc 08 App. B; exporter not added).
- Kafka publishing is implemented but only exercised with the log publisher locally.
- Template wording is a dev draft; Legal/Product review needed (Doc 06 App. B).
