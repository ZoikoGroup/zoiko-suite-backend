# Doc 11 handover registers: ZoikoSuite Assistant (R0)

Doc 11 §08, §17, §18, §19 and §23 ask for one handover checklist, one RAID register, one decision register, one change register and an owner roster. This file holds all five. Engineering evidence is linked where it exists. **Nothing here is a sign-off**: every approval in Doc 11 §24 is still PENDING.

Status values: **Evidence ready** (engineering proof exists; the named approver still has to accept it), **Partial**, **Open** (needs people, infrastructure or a decision).

Last updated 2026-11-05.

## 1. Engineering handover checklist (Doc 11 §23)

| ID | Check | Status | Evidence / what is missing |
|---|---|---|---|
| H01 | Scope, R0/R1 definitions, owner and market/locale matrix signed | Open | R0 public-only scope is built; market/locale matrix needs DEC-15, owners need §5 below |
| H02 | Codebase/infrastructure audit reconciled to C4 and ADRs | Partial | ADR-001…008 in `docs/suivo/adr`; infrastructure inventory needs DEC-01 |
| H03 | Public API and staff/admin routes isolated by IAM/network tests | Partial | `TestT12_NoRouteToPrivateSystems`, `TestA34_NoAdminRoute`; network negative proof needs the deployment (`TestA35` allowlisted) |
| H04 | Official logo, public name clearance and text assets signed | Open | Neutral name "ZoikoSuite Assistant" in use; DEC-04 |
| H05 | Canonical ten-module taxonomy reconciled and approved | Open | `data/public-kb/taxonomy-crosswalk.json` is UNRECONCILED; DEC-02 |
| H06 | KB source registry, signed digest, citations and revocation drill | Evidence ready | Signed releases, `TestKB010`, `TestKB011`, `TestKB030`, `cmd/drill` |
| H07 | Commercial claim registry signed, fresh and tested | Partial | Claims carry owners and review dates; resolver tests (`TestKB028`, `TestG05`); real claims need DEC-03 (data is SYNTHETIC) |
| H08 | No general-purpose tools or business-system credentials | Evidence ready | `TestGatewayDeadlineAndNoTools`, `TestA20`, `TestT12_NoRouteToPrivateSystems` |
| H09 | Session/turn APIs, concurrency, retries, typed errors, schemas | Evidence ready | Doc 05 CT tests, `TestCT011`, `TestIdleExpiry`, `TestGrantBoundToOrigin`, OpenAPI |
| H10 | Signed policies, prompt versions and provider terms approved | Partial | Policy digest gate (`TestPolicyDigestCoversRulesAndIsEnforced`); provider approval gate (`TestA40`); terms need DEC-05 |
| H11 | Threat model, access tests and prompt-injection evidence closed | Evidence ready | `threat-model.md`, `TestSafetyFoldDefeatsEncodingBypasses`, KB-012, A19–A23 |
| H12 | DPIA, transfer, retention and deletion decisions recorded | Open | Retention placeholder `[period from signed policy]`; DEC-05, DEC-06 |
| H13 | WCAG 2.2 AA journeys and accessible states tested | Partial | Playwright axe/keyboard, reduced-motion typing; manual screen-reader pass open |
| H14 | Golden corpus, holdouts, adjudication, P0 gates signed | Partial | `evalrun` verdicts and CI gate (`qa-t-traceability.md`); holdout and human grading need reviewers |
| H15 | Load, resilience and provider-offline fallback approved | Partial | `TestA29`–`TestA31`, `TestProviderFailuresFallBackSafely`; load needs DEC-08 and staging |
| H16 | Four-artifact manifest and independent rollback demonstrated | Partial | `release.BuildManifest` (image digest required, model profile must match the eval report, evidence recorded by sha256: `TestManifest*`), `TestKB011`, audited `kbctl promote -rollback`; compatibility ranges built (OPR-004: `compatible_policy_versions`, `release.TestIncompatibleReleaseRefusedAtLoad`); releases immutable in the database (migration 007); restore drill DR-06; staging demonstration open |
| H17 | SLIs, dashboards, alerts, incident and abuse runbooks linked | Partial | `/metrics` (`TestMetricsEndpoint`), multi-window burn-rate alert rules unit-tested with promtool in CI, every alert annotated with its RB-xx runbook (RUNBOOK RB-01…10 and alert map), Grafana dashboard `public-ai-svc.json` (its default datasource is the main Prometheus, which does not scrape public-ai-svc: point it at the public-ai Prometheus at deploy time), canary textfile metric; paging targets (Alertmanager) and SLO approval need SRE |
| H18 | Support destinations route-only, no false ticket acknowledgement | Evidence ready | `TestFalseConfirmationsRejected`, `TestDemoIsRouteOnly`; real URLs need DEC-10 |
| H19 | R1 staff roles and cases only after RBAC audit | Open (R1) | No staff console in R0 |
| H20 | R1 lead, CRM and booking only after vendor and privacy gates | Evidence ready (gate) | Doc 10 flags hard-off (`TestDoc10GatesAreHardOff`) |
| H21 | No unresolved critical/P0 defects | Evidence ready | All automated P0 checks, the ten Doc 02 scripts, the 42-case golden set (`golden-dev-v4`, 0 regressions against the baseline) and all 127 visitor questions quoted in Docs 01–11 pass; govulncheck reports no reachable vulnerability |
| H22 | Pilot GO/NO-GO, canary, reversal and after-action records | Open | Needs a pilot |
| H23 | Source editors, responders, incident commander, owner roster accepted | Open | §5 below has no named people yet |
| H24 | Operations and commercial teams sign maintenance cadence | Open | Needs the teams |

## 2. RAID register (Doc 11 §17)

| ID | Rating | Risk | Control in the code today | Owner (function) |
|---|---|---|---|---|
| R-01 | CRITICAL | API route into financial/private tenants | No private routes or credentials; `TestT12_NoRouteToPrivateSystems`, `TestA34`; egress policy is deployment work | Security + CTO |
| R-02 | HIGH | Public claims outdated or mismapped by market | Signed claims, effective dates, review deadlines, owner cases (`TestKB005`, `TestKB028`) | Commercial + KB |
| R-03 | HIGH | LLM injection via documents or chat | Safety fold, ingest quarantine (titles, synonyms, stuffing), no tools, output validation; an injected opener is stripped and recorded, extraction or action requests stay refused | AI + AppSec |
| R-04 | HIGH | Production repo differs from architecture | ADR delta; DEC-01 open | Architect |
| R-05 | HIGH | AI supplier transfer/retention unresolved | Provider approval + region allowlist gate (`TestA40`); DEC-05 open | DPO |
| R-06 | HIGH | False lead, case or booking confirmation | Output validator (`TestFalseConfirmationsRejected`); route-only handoff | CRM + Support |
| R-07 | HIGH | Four artifacts mutually incompatible | Manifest digests; releases name `compatible_policy_versions` and an incompatible release is refused at load (OPR-004, `TestIncompatibleReleaseRefusedAtLoad`); manifest refuses an eval report for another model profile | SRE |
| R-08 | HIGH | Admin role leaks PII or edits + approves | No admin role in R0; promotion gate enforces two approvers and an independent promoter | IAM + AppSec |
| R-09 | MEDIUM | Prompt/model update degrades quality | Golden set in CI, repeats, baseline regression (NO_GO) | AI Eval |
| R-10 | MEDIUM | Unsupported market or taxonomy copy | Doc 02 menu from canonical intents; DEC-02 open | Product + KB |
| R-11 | MEDIUM | Provider failure or token-cost spike | Timeouts, circuit breaker, staged throttling, session token cap | SRE |
| R-12 | HIGH | No staffed response or incident commander | Open: rota needed | Support + SRE |
| R-13 | MEDIUM | Accessibility regressions | Vitest + Playwright axe/keyboard gates | Frontend + QA |
| R-14 | HIGH | Personal data kept beyond policy | Contact masking, sensitive-input redaction, retention purge; DEC-06 open | DPO + Security |
| R-15 | MEDIUM | R1 CRM procurement delays R0 | R1 gates hard-off; R0 ships route-only | TPM + Commercial |
| R-16 | HIGH | Brand name used before clearance | Neutral display name; DEC-04 open | Brand + Legal |

## 3. Decision register (Doc 11 §18)

| ID | Decision | Accountable | Status | Effect in the code until decided |
|---|---|---|---|---|
| DEC-01 | Real repository, hosting, gateway, identity | CTO / Architect | Open | Service lives in this monorepo (ADR-001) |
| DEC-02 | Authoritative taxonomy | Product | Open | Crosswalk marked UNRECONCILED |
| DEC-03 | Who approves claims | Commercial + Legal | Open | Claims SYNTHETIC; owners assigned by claim type |
| DEC-04 | Public name and brand assets | Brand + Legal | Open | "ZoikoSuite Assistant" |
| DEC-05 | LLM vendor, model and region | DPO + Security | Open | Mock provider in dev; production refuses without approval ref and approved region |
| DEC-06 | Retention, deletion, cookies | DPO | Open | Production refuses without `PUBLIC_AI_RETENTION`; chat history stays in the browser tab |
| DEC-07 | Endpoint path, transport, TTLs | Backend + Sec | Proposed | `/public-ai/v1`, 30 min session, 15 min idle (ADR-005) |
| DEC-08 | Traffic profile, error budget, SLOs | SRE + Product | Open | Proposed 99.9 % objective in alert rules |
| DEC-09 | Owner of signed knowledge and revocation | Knowledge + SRE | Proposed | public-ai-svc release runtime |
| DEC-10 | Handoff destinations | Support Ops | Open | Route registry empty: no links shown |
| DEC-11 | Staff IAM and permissions | IAM + Security | Open (R1) | No staff console |
| DEC-12 | Helpdesk and case creation | Support Ops | Open (R1) | Route-only |
| DEC-13 | CRM and routing | Commercial + DPO | Open (R1) | `crm_sync_enabled` hard-off |
| DEC-14 | Scheduler and webhooks | Commercial + Sec | Open (R1) | `booking_sync_enabled` hard-off |
| DEC-15 | Jurisdiction and language matrix | Product + Legal | Open | en-US only; markets only from signed claims |
| DEC-16 | Who declares GO, STOP, rollback, revoke | Founder + CTO | Open | `kbctl` requires actor + approval for every control action and audits it |

## 4. Change register (Doc 11 §19)

Changes that alter an API schema, claim rule, data flow, provider, destination or security boundary:

| Date | Change | Tier / record |
|---|---|---|
| 2026-10-30 | Structured responses: `GET /menu`, `followups`, `cta`, `outcome`, citation titles (behind `structured_response_enabled`, default off) | API additive; ADR-008, OpenAPI |
| 2026-10-30 | New reason `SPEND_CEILING`; staged throttling and per-session token cap | API enum addition; OpenAPI |
| 2026-10-30 | Session grant bound to its origin; idle expiry | Security boundary; Doc 05 §08, Doc 06 §17 |
| 2026-10-30 | `PUBLIC_AI_ENVIRONMENT` required; policy digest and provider region gates | Deployment config; compose updated |
| 2026-10-30 | Every ACTIVE claim needs an owner; overdue review = expired | P1 claim rule; Doc 03 §10–11 |
| 2026-10-30 | Event sources per producer, UUIDv4 ids, `control_epoch` on kill-switch events | Event schema; AsyncAPI |
| 2026-10-30 | Contact details masked before retrieval and the model | Data flow; Doc 10 §21 |
| 2026-10-30 | A country the visitor names is stored on the session (`market_code`) and reused | Data flow; Doc 02 §10 |
| 2026-10-30 | New reasons `GLOSSARY`, `CONTEXT_NOTED`; `LIVE_PLATFORM_DATA`, `NO_APPROVED_INFO` added to the OpenAPI enum | API enum addition; OpenAPI |
| 2026-10-30 | Rate-limit keys are salted hashes; `PUBLIC_AI_ALLOWED_HOSTS` required outside dev/test | Security boundary; Doc 06 §11, §15 |
| 2026-11-05 | `page_context` accepts `trust` (Doc 02 §22 Trust page chips); other values still rejected | API enum addition; OpenAPI |
| 2026-11-05 | An unsupported `locale` is served in the default locale (en-US) instead of abstaining | Data flow; Doc 05 §08–09 |
| 2026-11-05 | A claim owner may not be one of its approvers (load fails) | P1 claim rule; Doc 03 §10 |
| 2026-11-05 | OpenAPI/AsyncAPI aligned with the code (`suggestions`, `draft`, `VERIFIED`, UUID turn key, declared problems, `TOOL_INSTRUCTION`, required event fields) and guarded by `internal/specs` tests | API documentation; Doc 05 App. A/B |
| 2026-11-05 | Opt-in device chat history (24 h) and tab history in the browser | Data flow; Doc 06 §11, `data-inventory.md` |
| 2026-10-30 | Dependency upgrades (OpenTelemetry 1.45, x/net 0.60, x/text 0.42, gRPC 1.83.1), toolchain go1.27.3, runtime alpine 3.22 | Supply chain; Doc 06 §12 |

## 5. Owner roster (Doc 11 §08, H23)

RACI is to functions. Named people, backups and segregation-of-duties permissions are recorded at G0 and are all **PENDING**.

| Control | Accountable | Responsible | Named person / backup |
|---|---|---|---|
| Scope and release boundary | Product Owner | TPM / Delivery Lead | PENDING |
| Architecture / API contract | Global CTO | Solutions Architect + Backend Lead | PENDING |
| AI prompts / model output | AI Product Owner | AI / RAG Lead | PENDING |
| KB truth / signed releases | Knowledge Owner | Content Ops + KB Engineering | PENDING |
| Commercial claims / trial | Commercial Owner | Product Ops + Claim Registry Lead | PENDING |
| Privacy / supplier terms | DPO / Legal Lead | Privacy Engineering | PENDING |
| Security controls / threat | Security Lead | AppSec + Backend | PENDING |
| UX / accessibility | Product Design Lead | Frontend Lead + Accessibility QA | PENDING |
| QA / evaluation evidence | Quality Lead | Test Automation + AI Evaluation | PENDING |
| Deployment / recovery | SRE Lead | Platform Engineers | PENDING |
| Incident commander | SRE Lead | On-call rota | PENDING |
| Overall go / no-go | Release Approver / Founder | Programme Lead | PENDING |

Claim owner teams in the data (`owner_id`): commercial-ops (price, trial, plans), regional-product (markets), security-trust (certifications), privacy (residency), integration-owner (connectors).
