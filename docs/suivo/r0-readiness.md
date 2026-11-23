# Suivo R0 — readiness record

**Decision: NO-GO / NOT VERIFIED.** No gate below has been approved. This file records the evidence the
engineering build has produced so far and what each gate still needs. It must never be edited to show a
gate as passed without the named approver's signed record (Doc 11 §15, §24).

Build: service `services/public-ai-svc` (branch `feat/public-ai-docs-01-11-round2`, last updated 2026-11-05). Evidence from the dev build is on
TEST-ONLY fixtures with the deterministic mock model; it proves mechanisms, not production behaviour. A Groq
provider is wired (`MODEL_PROVIDER=groq`) but has no approved DPA/region yet (DEC-05).

## Gates (Doc 11 §15 is the master list)

| Gate | Approvers (Doc 11) | Status | Evidence produced | Still needed |
|---|---|---|---|---|
| G0 Discovery & reconcile | Product, CTO, Security, DPO | **PENDING** | `docs/suivo/g0-architecture-delta.md`, ADR-001..005, document register | Named approvers; v2.1 taxonomy (DEC-02); claim owners (DEC-03); hosting (DEC-01) |
| G1 Secure foundation | CTO, Architect, Security | **PENDING** | OpenAPI 3.1 contract, isolation test in CI, signed releases, migrations, separate compose stack/network | Vendor/region decision (DEC-05); TTLs (DEC-07); signing-key custody (DEC-09); threat-model sign-off |
| G2 Vertical slice | Backend, AI, QA, Security | **PENDING** | Pipeline tests (happy path, denial, grounded price, fallback, citation), e2e test, live smoke run with 0 log leaks | Run on staging with real (approved) content and an approved model |
| G3 Broaden & harden | QA, Security, DPO, Product | **PENDING** | Golden dev set `golden-dev-v4` 42/42 against a committed baseline (0 regressions); the 127 visitor questions quoted in Docs 01–11 all reach their required outcome (`TestDocsQuestions`); redaction/injection/exception and revocation tests; browser suite 53/53 with axe scans | Holdout set by independent authors (Doc 07 E); semantic + human grading; manual screen-reader and contrast pass; DPIA; pentest |
| G4 Operate & pilot | SRE, Security, QA | **PENDING** | Four-artifact signed manifest (`kbctl manifest`), drills DR-01..07 in CI, RUNBOOK RB-01..10, rate/spend limits, kill switch, Prometheus alert rules with promtool tests, one Grafana dashboard | Named on-call and owners (DEC-16); SLOs (DEC-08); Alertmanager routing; the dashboard's datasource must point at the public-ai Prometheus; the other Doc 08 §15 dashboards; drills repeated on staging |
| G5 R0 launch decision | Founder + CTO, Security, DPO, QA, SRE | **PENDING** | — | Every non-waivable stop cleared; permitted markets; brand clearance (DEC-04); pilot plan |

## Doc 11 §16 non-waivable stops

| Stop | Current state |
|---|---|
| Anonymous customer-data reachability | Code-level isolation tested (SEC-T049); network egress policy in the target environment not verified |
| Material commercial/trust hallucination | Prices/trials/certifications only from verified claims; numbers not in evidence are rejected; tested on fixtures only |
| Revoked/expired source still answerable | Tested (retrieval + commit re-check + rollback drill) |
| Unapproved model processing or residency | Providers: the dev/test-only mock and Groq. Outside dev/test Groq refuses to start without `PUBLIC_AI_PROVIDER_APPROVAL_REF` and an allowlisted region (`TestA40`); no approval recorded yet (DEC-05) |
| Missing Security, SRE or QA sign-off | All PENDING |
| Rollback or kill switch cannot be executed | Drills pass in dev; not yet executed on staging |
| Handoff/CRM false-success state | Route-only; prohibited phrases blocked; no CRM/case code in R0 |
| P0 accessibility obstruction | Widget built in the admin console only (`zoiko-suite-frontend-platform`; product-owner decision — Docs 01/11 intend the public website). Keyboard, focus return, live region, 44 px controls, 320 px layout and axe scans run in the blocking Playwright suite; no manual screen-reader (NVDA/VoiceOver) pass and no text-only 200%/400% zoom test yet |

## Operational artefacts

- `services/public-ai-svc/Dockerfile` — non-root, read-only rootfs compatible, healthcheck, git SHA baked in
- `deployments/docker-compose.public-ai.yml` — isolated stack (own Postgres, own network), verified:
  not ready until signed releases are published, then serves; container healthy
- `services/public-ai-svc/RUNBOOK.md` — controls, RB-01..RB-10, drills, release record
- `cmd/drill`, `cmd/evalrun`, `kbctl manifest`

## Known gaps

- Widget built with automated browser tests (Playwright 53/53, axe); a manual WCAG 2.2 AA pass (screen readers, text-only zoom, contrast review) is not done.
- No load/latency measurement; proposed p95 ≤ 8 s is unverified.
- Alert rules are loaded and tested (promtool) but route to no one: no Alertmanager or on-call. One dashboard exists; its default datasource is the main Prometheus, which does not scrape public-ai-svc. No OTel collector in the compose stack.
- Kafka publishing is implemented but only exercised with the log publisher locally.
- Template wording is a dev draft; Legal/Product review needed (Doc 06 App. B).
- The full list of open items per document is in `docs/suivo/gap-list-2026-11-05.md`.
