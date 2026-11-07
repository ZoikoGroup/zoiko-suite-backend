# Release evidence index (Doc 11 §20)

Doc 11 §20 asks for audit-ready evidence folders 00–09. The evidence itself lives where it is
produced and maintained, either in the repository or as CI artifacts. This index points each folder
at its sources. Generated reports (eval, drill, test receipts, link check, ZAP) are CI artifacts
and are attached to a release with `kbctl manifest`, which records their sha256.

**Status:** engineering evidence is in place. Every "Missing" row needs an owner, a decision or
real data. No gate has been approved (`../r0-readiness.md`).

| Folder | Evidence in this repository / CI | Missing |
|---|---|---|
| 00-governance | `../adr/` ADR-001…009, `../g0-architecture-delta.md`, `../00-document-register.md`, `../handover-registers.md` (risks, decisions, changes), `../approvals-log.md`, `../gap-list-2026-11-05.md` | Signed scope approval and R0/R1 charter; named owner records (DEC-16) |
| 01-product-and-ux | Widget `zoiko-suite-frontend-platform/components/assistant`, Playwright states and visual baselines (`tests/e2e/ui.spec.ts-snapshots`), Doc 02 copy registry `services/public-ai-svc/internal/policy/microcopy.go` | Signed design assets; canonical v2.1 taxonomy (DEC-02); locale registry beyond en-US |
| 02-api-and-model | `services/public-ai-svc/api/openapi.yaml`, `asyncapi.yaml`, `api/schemas/*.v1.schema.json`, frozen `api/compat/`, prompts `internal/model/prompts.go`, policy digest (`kbctl policy-sign`) | Model provider contract (DEC-05) |
| 03-knowledge-and-claims | `data/public-kb` (sources, `release.json`), signed releases (`kbctl build-kb/build-claims/publish`), revocation ledger, `kbctl conflicts`, `../doc03-e-suites.md` | Real approved sources and claims (D03-04); owner approvals |
| 04-security-privacy | `../threat-model.md`, `../owasp-llm-map.md`, `../sec-t-traceability.csv`, `../data-inventory.md`, CI gosec/govulncheck/Trivy/SBOM/gitleaks, negative tests (`internal/httpapi/negative_test.go`), fuzz targets, nightly ZAP report | DPIA, vendor/DPA disposition, retention values (DEC-06), pen test |
| 05-evaluation-quality | `testdata/golden/golden-v1.json` + `baseline.json`, `eval-report.json` (CI), `testdata/docs-questions.json` (127 cases), Doc 02 paraphrases, `test.json` receipts, `cmd/loadtest`, axe and zoom tests | Holdout set by independent authors; human grading lane; screen-reader pass; SRE-approved load profile |
| 06-operations | `RUNBOOK.md` RB-01…12, `deployments/prometheus-rules-public-ai.yml` (+ promtool tests), dashboards, `drill-report.json` (CI), signed release manifests (`kbctl manifest`, `verify-manifest`) | On-call roster and Alertmanager routing; SLO approval (DEC-08); staging drills |
| 07-commercial-and-handoff | Official route registry (`data/public-kb/release.json` routes), route-only handoff (`POST /handoffs`), Doc 10 flags hard-off | Real route URLs; lead/CRM/booking contracts and consent model (Doc 10, D10-03…05) |
| 08-release-decision | `../r0-readiness.md` (NO-GO), manifest sign-offs (`kbctl signoff`, audited) | Every mandatory signature; exception register entries; change ticket; GO decision |
| 09-bau-acceptance | Nightly replay and drift checks (`.github/workflows/public-ai-nightly.yml`), runbooks, `kbctl audit -verify` | Owner handover; knowledge review cadence; residual-risk sign-off |
