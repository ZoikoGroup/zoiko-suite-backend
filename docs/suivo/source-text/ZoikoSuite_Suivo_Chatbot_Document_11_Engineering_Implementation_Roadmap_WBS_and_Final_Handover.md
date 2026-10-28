<!-- Extracted from ZoikoSuite_Suivo_Chatbot_Document_11_Engineering_Implementation_Roadmap_WBS_and_Final_Handover.docx -->

ZOIKO GROUP  /  ZOIKOTECH INC.  /  ZOIKOSUITE

SUIVO  /  PUBLIC AI ASSISTANT

Engineering Implementation Roadmap,Work Breakdown Structure & Final Handover

ENGINEERING DOCUMENT 11   |   VERSION 1.0   |   24 OCTOBER 2026

A build-ready programme and formal handoff specification integrating Documents 01–10: authoritative scope, architectural reconciliation, prioritised engineering work packages, dependencies, RACI ownership, staged release strategy, test and security evidence, operational readiness, risks, open decisions, rollback, and final approval records.

| DOCUMENT STATUS — FOR APPROVAL, NOT PROOF OF IMPLEMENTATION PROPOSED ENGINEERING DELIVERY BASELINE. This document neither asserts that the architecture is deployed nor claims that the predecessor documents are legally, commercially or technically signed off. Scope, effort, schedules, suppliers, operational SLOs and residency remain conditional on repository discovery and named leadership approvals. |
|---|

| Control field | Definition |
|---|---|
| Document ID / series | ZS-SUIVO-ENG-011 / v1.0; controlled capstone to Documents 01–10. |
| Audience | Founder, Global CTO, product, engineering, AI/ML, security, DPO/legal, SRE, QA, design, knowledge operations, commercial/CRM and customer support. |
| Identity | Suivo = internal working public-assistant name. Stable technical ID: zoikosuite-public-ai. Public naming and approved logo/favicon require separate asset/legal sign-off. |
| Release recommendation | R0 = anonymous, public, source-grounded chat with official links. R1 = authenticated admin/knowledge operations plus separately gated support-case and CRM/calendar integrations. |
| Immutable boundary | No anonymous access to customers’ finance, HR, payroll, tax, legal or account data; no chatbot-originated filings, payments or transactional authority. |
| Document owner / acceptance | Programme owner + Global CTO; substantive acceptance by Product, Security, Privacy/Legal, Quality and SRE. Founder approves scope, brand and business release. |
| Delivery status | WORK PACKAGE PLAN / NOT STARTED BY THIS DOCUMENT. Any work reported as complete requires evidence links, owners and verified gate results. |

| INSTRUCTION TO IMPLEMENTATION LEADS Do not treat the illustrative URLs, APIs, test fixtures, estimates or configurations in the series as proof of existing infrastructure. Perform repository, deployment, licensing, security and commercial source-of-truth reconciliation at G0 before writing integration code. |
|---|

## 01  Executive determination: an evidence-led production programme

Recommendation: operate a single delivery backlog, a single architectural decision register, a single risk and exception register, and a single evidence index mapped to Documents 01–10. Build a minimally useful public assistant first, retain all P0 trust controls in that first release, and independently gate staff-console, CRM and scheduling additions. No release is complete solely because the UI renders, the model produces an answer, or automated tests show a high average score.

| Executive issue | Decision and acceptance effect |
|---|---|
| Scope creep | Use R0 / R1 boundaries; staff-console and CRM scope may not silently block or weaken a safe R0. |
| Competing taxonomies | Stable canonical module IDs and signed release mapping control all public domain labels; no feature taxonomy guessed by LLM. |
| Hallucinated commercial truth | Market, plan, trial, integration and trust/security facts use signed, active, approver-owned evidence. |
| Fragmented sign-offs | Gates require individual evidence records, owners, approvers and explicit stop/waiver disposition. |
| Implied transactions | Public chatbot routes only. Confirmed cases, leads and bookings require authoritative destination receipts. |
| Vendor lock-in | Model, CRM and calendar are adapters; supplier selection is a written decision, not a default assumption. |
| Operational fragility | Independently rollback application, prompt-policy, knowledge and commercial claim registry. |
| Test score laundering | Critical access, privacy, injection and material price errors are blockers irrespective of aggregate evaluation. |

## 02  Predecessor authority and non-duplication crosswalk

The work-breakdown register links to the controlled engineering baseline. Exact acceptance tests and normative schemas remain in their source document; this capstone assigns implementation and evidence ownership without reproducing or weakening them.

| Doc | Authority topic | Primary deliverable carried into WBS | Accountable function | WBS |
|---|---|---|---|---|
| 01 | Product, UX, wireframes | Widget states, journeys, public-only scope, feature priority | UX / Product | A,B |
| 02 | Conversations, prompts, intents | Prompt hierarchy, classifier, safe scripts, handoff copy | AI Lead / Product | B,C |
| 03 | Knowledge and governance | Signed corpus, taxonomy, citations, revocation, approvals | Knowledge Lead | A,D |
| 04 | Solution architecture | Service topology, model gateway, network denial, session | Solutions Architect | C,E,F,H |
| 05 | APIs and events | Public API contracts, claim resolver, outbox, replay | Backend Lead | D,E,J |
| 06 | Security and privacy | Threat model, DPIA/vendor gates, 60 security test IDs | Security + DPO | F |
| 07 | Quality and acceptance | 84 master acceptance scenarios and criteria | QA Lead | G |
| 08 | DevOps and production | 4-artifact releases, SLO, runbooks, 48 ops tests | SRE Lead | H,K |
| 09 | Admin and handoff | 12 staff screens, permissions, 60 acceptance tests | Admin Lead | I,D |
| 10 | Commercial conversion | Lead/booking receipts, CRM adapter, 72 tests | Commercial + Backend | J |

Conflict order: (1) written governance approval or binding legal requirement; (2) approved security/privacy policy; (3) signed source-of-truth content or formal architecture decision; (4) stable contract from Documents 04/05; (5) new proposals in this plan. An unresolved conflict blocks the affected work item; do not let a language model infer its resolution.

## 03  Critical review: corrections incorporated into execution design

| ID | Failure avoided | Refinement |
|---|---|---|
| CR-01 | No verified repository baseline | G0 discovery inventory blocks assumptions about language, hosting, tenancy and APIs. |
| CR-02 | Public/admin authority confusion | Physically separate staff IAM, service identities, network routes and data stores. |
| CR-03 | Commercial correctness not versioned | Four independent release artifacts with compatibility and rollback proofs. |
| CR-04 | UI/knowledge taxonomy drift | Canonical source IDs with immutable alias map and reviewed public labels. |
| CR-05 | Integration completion overstated | Explicit receipts for secure intake, provider booking, CRM sync and human cases. |
| CR-06 | AI scoring hides rare failures | Risk-weighted golden sets, holdouts, manual adjudication and zero-tolerance classes. |
| CR-07 | Knowledge revocation lags deployment | Revocation tested end-to-end; deny cached stale/invalid P1 claims. |
| CR-08 | Capacity plan becomes fictional commitment | Role-based effort estimates only after G0 baseline and skill ownership. |
| CR-09 | Vendor choice hardcoded in design | Adapter contracts, procurement gates, data transfer review, fallback operation. |
| CR-10 | Release rollback conflated with code rollback | Atomic compatibility plus 4 separately revocable digests. |
| CR-11 | Support routes misreported as tickets | Route-only vs case-accepted states verified by authoritative ACK. |
| CR-12 | Post-launch ownership assumed | Require signed on-call, content owners, privacy rights and commercial operators. |

## 04  Release charter, exclusions and scope change rules

Figure 1 — Separate release trains with controls shared across both.

| Release | Must include | Explicitly outside this release |
|---|---|---|
| R0 – Public guidance | Responsive widget; public sessions/turns; typed model orchestrator; signed public knowledge; active claim resolver; official route-only CTA; safe refusals; UX states; test evidence; observability; rollback. | Staff management UI, CRM writes, calendar/provider bookings, customer account access and chat-collected personal financial data. |
| R1 – Governed operations | Staff SSO/RBAC, knowledge approvals, handoff queue and optional secure case creation; secure web lead intake + CRM adapter and booking provider once separately authorized; audit and privacy workflows. | A case or lead cannot be created from anonymous model output or without a secure authoritative receipt. |
| R2 – Separate programme | Authenticated tenant-bound financial assistant may be explored only through a NEW architecture, privacy, liability and authorization review. | Not authorized under Documents 01–11; not a hidden extension or implied feature flag. |

| CHANGE CONTROL An addition to R0 that handles personal customer data, creates external records, or confers transaction authority requires a new approved ADR, threat model, DPIA/legal review, UX sign-off and revised contract tests. Do not authorize through backlog prioritization alone. |
|---|

## 05  Non-negotiable platform invariants and negative space

| Invariant | How engineering proves enforcement |
|---|---|
| INV-01 Public access only | Network policies, API allowlist and integration tests fail all direct routes to private accounting, payroll, tax, HR, legal and customer support data. |
| INV-02 Verified high-impact claims | Current signed market-specific price, trial, integration and trust records mandatory; stale/conflicting claim causes abstention or official link. |
| INV-03 No model tool authority | Generation model receives permitted evidence and schema only, never general-purpose production credentials or unrestricted connectors. |
| INV-04 Canonical categories | All exposed product domain labels trace to release-controlled stable IDs; no unapproved dashboard taxonomy. |
| INV-05 Truthful outcomes | Only provider/supplier ACK yields CONFIRMED, ACCEPTED, SYNCED status; route click alone is not completion. |
| INV-06 Consent and data minimization | Anonymous chat does not require contact data; secure forms independently document notice, lawful basis and optional marketing choice. |
| INV-07 Redaction and least retention | No raw prompts in standard metrics/traces; provider and regional storage require contractual and DPO approval. |
| INV-08 Kill and rollback | Revocation and safe-static mode demonstrated for each of four released artifacts. |
| INV-09 Accessibility | WCAG 2.2 AA keyboard, screen reader, reduced-motion, responsive and error-state evidence. |
| INV-10 Traceability | Every P0 work item has requirements, commit, tests, risk disposition, owner and release artifact proof. |

## 06  Delivery outcomes, targets and measurement policy

Release success is a composite of business usefulness and trustworthy operation. Numeric nonfunctional thresholds in predecessor documents remain proposed until architecture discovery, representative traffic baselines, supplier quotas, legal consultation and signed SLO approval. An unsourced target is a planning hypothesis, not a production commitment.

| Measure | Measurement method / decision rule | Evidence owner |
|---|---|---|
| Answer grounding | Approved answer corpus; cite-to-source consistency, claim freshness and evaluated abstention. Critical wrong financial/commercial claims block. | AI Evaluation |
| P0 intent completion | Golden multi-turn journeys segmented by visitor persona, locale, accessibility mode and error state. | Product + QA |
| Unsafe-action suppression | Negative access tests and red-team prompts must show no customer data access or action execution. | Security |
| Web performance | RUM+synthetic UI start, first response, completion and mobile accessibility in realistic network conditions. | Frontend + SRE |
| Reliability and burn | API availability/latency, cache/retrieval health, alerts, safe fallback and provider controls. | SRE |
| Human follow-up | Only secure destinations produce authoritative case, booking or lead receipts; reconcile pending / failures. | Operations + Commercial |
| Evidence currency | All public KB and commercial source registries have owner, version, approval and next-review dates. | Knowledge Lead |
| Privacy compliance | DPIA disposition, DPA/subprocessor approvals, rights workflow test, data retention implementation. | DPO / Security |

## 07  G0 discovery: repository and source-of-truth verification

Before allocating implementation tasks, engineering must collect the actual source tree, environment architecture and production configuration. The prior document series was developed from product-design baselines and public references, not a verified line-by-line audit of the live repository. Discovery resolves existing-vs-proposed capabilities and creates the dependency graph.

| Discovery object | Mandatory artifact |
|---|---|
| Existing web frontend | Framework, hosting origin(s), navigation, CMS, widget embed constraints, CSP, UX asset library, locale switch and deployed routes. |
| Service catalogue | Code ownership, current API gateway, environment endpoints, domain owners, service authentication and event bus existence. |
| Existing KB and taxonomy | Canonical source map for four module groups / ten core modules; reconcile six homepage domains, service categories and plan catalogs. |
| Security/platform | Staff identity solution, secrets/KMS, regional hosting, network topology, SAST/SCA controls, backup capabilities. |
| Commercial providers | Real demo and trial destinations, pricing registry, lead forms, CRM, calendar/scheduler, marketing permissions and vendor ownership. |
| AI vendor and search | Legal basis, model gateway, provider contracting, availability, region, token logging, retriever, licensing and source revocation. |
| Delivery processes | Version-control locations, branching, CI/CD, environment drift, incident roster, privacy ticket routing and support model. |
| Brand/IP and content | Approved website logo/favicon SVG/PNG master, public assistant name display clearance, content approvers and language owners. |

| MANDATORY G0 OUTPUT An evidence-backed architecture delta report: EXISTING / REUSABLE / MISSING / REJECTED / BLOCKED for each service and workstream, with repository path or provider reference, accountable owner, confidence level and next action. Implementation estimates are approved only after this baseline. |
|---|

## 08  Governance structure and accountable RACI

| Decision or control | Accountable (A) | Responsible (R) | Consulted / veto |
|---|---|---|---|
| Scope and release boundary | Product Owner | TPM / Delivery Lead | Founder, Security (veto private-data drift) |
| Architecture / API contract | Global CTO | Solutions Architect + Backend Lead | Security, SRE, Frontend, Data owners |
| AI prompts / model output | AI Product Owner | AI / RAG Lead | QA, Legal, Knowledge owner |
| KB truth / signed releases | Knowledge Owner | Content Ops + KB Engineering | Product, Commercial, DPO, Security |
| Commercial claims / trial | Commercial Owner | Product Ops + Claim Registry Lead | Finance, Legal, Sales Ops |
| Privacy / supplier terms | DPO / Legal Lead | Privacy Engineering / Procurement | Security, region owner (veto) |
| Security controls / threat | Security Lead | AppSec + Backend / Infra | DPO, Global CTO (veto release) |
| UX / accessibility | Product Design Lead | Frontend Lead + Accessibility QA | Product, Support |
| QA/evaluation evidence | Quality Lead | Test Automation + AI Evaluation | All workstream leads (veto critical fails) |
| Deployment / recovery | SRE Lead | Platform Engineers | Security, CTO, incident commander |
| Staff admin / human cases | Support Operations Owner | Admin Lead + Support Lead | IAM, Privacy, Knowledge |
| CRM / scheduling | Commercial Operations Owner | Integration Lead + CRM Admin | Privacy, Security, Finance |
| Overall go / no-go | Release Approver / Founder | Programme Lead compiles evidence | CTO, Security, DPO, QA, SRE mandatory individual sign-offs |

RACI assignments are to accountable functions, not named employees. Actual people, backups and segregation-of-duties permissions must be recorded at G0. Security, privacy and release assurance may stop launch despite commercial readiness.

## 09  Capacity planning, team topology and estimation rules

| Workstream | Suggested capacity / participation | Estimate caveat |
|---|---|---|
| Product / TPM | 1 product owner + 1 delivery lead shared across phases | Decision authority and decision log required. |
| Architecture | 1 architect; backend technical lead part-time to full-time | Repository discoveries may reduce or expand requirements. |
| Frontend + design | 1–2 engineers; product designer and accessibility specialist as needed | Design systems reuse materially changes effort. |
| Backend / integrations | 2–3 developers for public API, claims, events, staff/CRM extensions | CRM, booking and case integrations may be deferred R1. |
| AI + data | 1–2 AI/RAG engineers; knowledge editor / owner | Model/provider choice and corpus maturity dominate effort. |
| QA / AI evaluation | 1–2 QA automation / model-eval engineers with specialist adversarial testing | Dedicated test capability, not a last-week add-on. |
| Platform / SRE / security | 1 SRE/DevOps with fractional AppSec; DPO/legal separately | Production on-call, vendor and privacy approvers cannot be skipped. |
| Operations / commercial | Named support/KB/CRM owners with staged availability | Business operations begin before pilot, not afterward. |

Planning rule: estimate each package using three-point (optimistic / most likely / pessimistic) effort and dependency confidence after G0; derive calendar duration from assigned availability, parallel paths, integration risk, and the number of independent approval gates. Do not commit to a fixed go-live date merely from these tables. The critical path is architecture + source governance + secured vertical slice + adversarial QA + operating proof + signed release decision.

## 10  Release roadmap, work streams and parallel execution

Figure 2 — Gated critical path; parallel workstreams cannot bypass predecessors.

| Stage | Entry / exit requirements | Primary outputs |
|---|---|---|
| S0 Discover & reconcile | Input Documents 01–10. Exit: repository/provider delta, source owners, open decision matrix and signed R0 scope. | G0 package, risk and resource baseline. |
| S1 Secure foundation | Exit: compatible API schemas, signed KB/claim design, staff/public trust zoning, vendor selections or no-vendor modes. | G1 ADRs, policy, IaC, contract fixtures. |
| S2 Vertical slice | Exit: responsive chat + session/turn API + controlled retrieval + model gateway + route-only + no-secret telemetry. | G2 demonstrable protected happy/unsafe paths. |
| S3 Broaden and harden | Exit: full P0 intents, signed release automation, revocation, fault injection, privacy, accessibility and multi-locale tests. | G3 reproducible AI/UX/API/Sec evidence. |
| S4 Operate and pilot | Exit: on-call, alerting, four-artifact rollback, incident drills, canary/pilot, production evidence. | G4/G5 approved R0 release decision. |
| S5 Governed R1 | Entry: separate scope, vendor contracts, authenticated staff IAM; exit: case/CRM/booking authoritative receipts and audit. | R1 admin, case and commercial proof; G6 sign-off. |
| S6 Close and improve | Operations ownership/knowledge cadence/audit evidence and accepted defects; no unmanaged P0 backlog. | G7 closure and BAU acceptance. |

## 11  Dependency model and work package control language

Each WBS task is a verifiable deliverable, not a vague activity. Priority P0 = cannot launch relevant release without passing. P1 = necessary for R1 or planned maturity; P2 = later improvement. Dependency IDs are hard prerequisites where specified; other tasks can proceed in parallel after source, security and API standards are approved.

| Ticket field | Minimum implementation standard |
|---|---|
| Work package ID | Stable SUIVO-11-A01 through SUIVO-11-Mxx; never recycle. CI and issue tracker share IDs. |
| Functional owner | One named accountable individual; RACI role and backup. No “team” without owner. |
| Traceability | At least one document/section/acceptance ID from 01–10; design/ADR/contract ref where applicable. |
| Definition of ready | Authority, dependency, approved source, mockups, threat and interfaces known; test acceptance unambiguous. |
| Definition of done | Merged peer-reviewed code/document, contract tests, redaction, observability, rollback, evidence links and owner sign-off. |
| Status vocabulary | PROPOSED, GROOMED, READY, IN_PROGRESS, BLOCKED, REVIEW, VERIFIED, RELEASED, REJECTED; VERIFIED not inferred from screenshots. |
| Sizing / reporting | 3-point effort and confidence; issue-level milestones; weekly risk/blocker/change summary. |

## 12  WBS execution register — controlled owner-level tasks

The following are separately estimable work packages. “P0/R0” means required for public launch; “P1/R1” means gated later capability. The IDs are newly defined here. Dependencies use the package suffix within its workstream (e.g. E04); G0 and G1 mean the approved gate artifact is also required.

### A  Programme and discovery  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| A01 | P0/R0 | Catalogue repositories, deployments, environments; cite commit hashes and service owners. | TPM + Architect | G0; D04/D08 |
| A02 | P0/R0 | Record actual public URLs, UX assets and widget embedding strategy; validate CSP. | Frontend Lead | A01; D01 |
| A03 | P0/R0 | Inventory APIs, IAM, event bus, model provider and search; classify EXISTING/PROPOSED. | Backend Lead | A01; D04/D05 |
| A04 | P0/R0 | Reconcile ten canonical modules, public domains, services and categories to stable IDs. | Product + KB | A01; D03 |
| A05 | P0/R0 | Confirm commercial claim data owners and active market-based plan/trial sources. | Commercial Owner | A01; D03/D10 |
| A06 | P0/R0 | Approve public-only R0 charter; document R1 deferrals, public name and asset clearance. | Product Owner | A01; D01/D06 |
| A07 | P0/R0 | Open risk, issues, assumptions, dependencies and decisions register (RAID) with owners. | TPM | A01; D11 |
| A08 | P0/R0 | Baseline release SLA/SLO proposals, traffic and model cost forecasts from real evidence. | SRE Lead | A01; D08 |
| A09 | P0/R0 | Record DPO, vendor-contract, privacy, residency and cookie legal gates. | DPO + Security | A01; D06 |
| A10 | P0/R0 | Establish backlog identifiers, traceability tooling and evidence workspace permissions. | TPM + QA | A01; D07 |

### B  Public experience and web integration  |  8 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| B01 | P0/R0 | Implement launcher, panel and desktop/mobile responsive layouts using approved brand assets. | Frontend Lead | A02/A06; D01 |
| B02 | P0/R0 | Implement keyboard focus, screen reader status, contrast, reduced motion and error repair. | Frontend + A11y | B01; D01/D07 |
| B03 | P0/R0 | Bind visitor states: welcome, compose, generating, cited answer, refusal, handoff, error. | Frontend | B01/C04; D01/D02 |
| B04 | P0/R0 | Implement official URL route launcher with verified allowlist and external-context warning. | Frontend + Security | E08/F06; D01/D05 |
| B05 | P0/R0 | Display citations and P1 claim evidence metadata without leaking internal controls. | Frontend | D05/E06; D03/D05 |
| B06 | P0/R0 | Provide minimal anonymous session expiry, clear-chat and privacy notice controls. | Frontend | E03/F04; D01/D06 |
| B07 | P0/R0 | Run brand/locale responsive visual regression across common browsers. | Design + QA | B01-B06; D07 |
| B08 | P1/R1 | Wire staff-contact destinations only after verified secure handoff receipt definitions. | Frontend | I05; D09 |

Every deliverable also requires the baseline ticket Definition of Done in Section 11; workstream sections are not separate production authorizations.

### C  Conversation and model control  |  8 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| C01 | P0/R0 | Version system prompts, classifier and policy hierarchy with signed review history. | AI Lead | A06; D02/D08 |
| C02 | P0/R0 | Implement typed intent routing for P0 public journeys and deterministic deny categories. | AI Lead | C01; D02 |
| C03 | P0/R0 | Implement safe clarification, multi-turn state and ambiguity limits. | AI Engineer | C02; D02 |
| C04 | P0/R0 | Build model gateway adapter, context budget, deadlines, usage guardrails and provider errors. | AI + Backend | E01/F05; D04 |
| C05 | P0/R0 | Constrain model to grounded evidence and output schema; no privileged model tools. | AI + Security | C04/D05; D02/D04 |
| C06 | P0/R0 | Implement pre/post response policy checks for PII, disallowed promises and unsafe URLs. | AI + Security | C05; D06 |
| C07 | P0/R0 | Return transparent safe fallback on missing source, low confidence or provider outage. | AI Engineer | C05/E08; D03/D08 |
| C08 | P0/R0 | Create conversation prompt regression fixtures and version comparison pipeline. | AI Eval | C01/J01; D07 |

### D  Public knowledge and commercial truth  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| D01 | P0/R0 | Build source registry with classification, owner, jurisdiction, dates and approval state. | Knowledge Lead | A04/A05; D03 |
| D02 | P0/R0 | Implement controlled ingest, chunk extraction, provenance and source identity. | KB Engineer | D01; D03 |
| D03 | P0/R0 | Implement approval / 4-eyes publishing and signed immutable public KB releases. | KB Eng + Ops | D01; D03/D09 |
| D04 | P0/R0 | Build taxonomy aliases, canonical IDs, and source-version compatibility validation. | KB Engineer | A04/D02; D03 |
| D05 | P0/R0 | Implement hybrid evidence retriever with public prefilters, ranked provenance and cites. | KB Engineer | D02/D03; D03/D05 |
| D06 | P0/R0 | Build signed market/plan/trial/integration/security claim resolver with expiry. | Backend + Biz | A05/D03; D03/D05 |
| D07 | P0/R0 | Implement contradictions, invalid effective windows and authoritative abstentions. | KB + Backend | D05/D06; D03 |
| D08 | P0/R0 | Make source revocation atomic across caches, search and active claim decisions. | KB + SRE | D03/D05; D03/D08 |
| D09 | P0/R0 | Prove source release / claim digest pinning with rollback and tests. | KB + QA | D03/D06; D08 |
| D10 | P1/R1 | Expose authenticated knowledge editing/review queues only behind staff IAM. | Admin + KB | I02; D09 |

### E  Architecture, public APIs and events  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| E01 | P0/R0 | Approve C4 architecture, threats, service ownership and egress deny-list as ADRs. | Architect | A01/A03/A09; D04 |
| E02 | P0/R0 | Freeze public API base path and machine-validated OpenAPI schema. | Backend Lead | E01; D05 |
| E03 | P0/R0 | Implement anonymous session create, TTL, encryption and one-active-turn rule. | Backend | E02; D04/D05 |
| E04 | P0/R0 | Implement turn submission, typed response envelope and RFC 9457 problems. | Backend | E03; D05 |
| E05 | P0/R0 | Implement idempotency keys, lock contention, replay and cancellation semantics. | Backend | E03/E04; D05 |
| E06 | P0/R0 | Integrate policy, search, claims and model through controlled internal interfaces. | Backend | C05/D05/D06/E04; D04/D05 |
| E07 | P0/R0 | Publish event schema registry, outbox/inbox dedupe, DLQ and replay policies. | Backend | E02; D05 |
| E08 | P0/R0 | Expose public config/route resolver with signed redirects and cache rules. | Backend | E02/D06; D05 |
| E09 | P0/R0 | Write API compatibility, concurrency, contract and negative access tests. | Backend + QA | E03-E08; D05/D07 |
| E10 | P1/R1 | Permit only scoped internal event adapters for verified cases / CRM / booking. | Integration Lead | I05/J03; D05/D09/D10 |

Every deliverable also requires the baseline ticket Definition of Done in Section 11; workstream sections are not separate production authorizations.

### F  Security, privacy and supplier governance  |  11 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| F01 | P0/R0 | Complete STRIDE + OWASP LLM/API threat scenarios with control owners. | AppSec | E01; D06 |
| F02 | P0/R0 | Enforce network denial of private ZoikoSuite business-system routes and credentials. | Security + SRE | E01; D04/D06 |
| F03 | P0/R0 | Implement WAF/schema/abuse quotas and prompt/token spending ceilings. | Security + Backend | E02/C04; D06 |
| F04 | P0/R0 | Approve public retention/data-inventory and DPO/DPIA disposition; implement TTL/deletion. | DPO + Backend | A09; D06 |
| F05 | P0/R0 | Approve model/search supplier processing, cross-border transfer and subprocessor terms. | DPO + Procurement | A09; D06 |
| F06 | P0/R0 | Implement validated output links, prompt-injection controls and public citation filtering. | AppSec + AI | C05/D05; D06 |
| F07 | P0/R0 | Apply secret scanning, dependency policy, SBOM/provenance and build attestations. | Security + SRE | E01; D08 |
| F08 | P0/R0 | Implement privacy-safe logs, access audits and secure incident handling. | Security + SRE | F04/E03; D06/D08 |
| F09 | P0/R0 | Test rights workflow, deletion propagation and provider purge according to approved policy. | DPO + QA | F04/F05; D06/D07 |
| F10 | P0/R0 | Complete adversarial red-team and high-impact breach simulations; resolve P0 issues. | AppSec + QA | C06/E09/D08; D06/D07 |
| F11 | P1/R1 | Approve staff IAM, least privilege, step-up controls and separation of duties. | IAM + AppSec | A03/E01; D09 |

### G  Quality assurance and AI evaluation  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| G01 | P0/R0 | Map predecessor IDs and WBS packages to one requirements-test-evidence matrix. | QA Lead | A10; D07 |
| G02 | P0/R0 | Build schema/API and browser smoke tests with deterministic test fixtures. | QA Automation | E02/B01; D05/D07 |
| G03 | P0/R0 | Establish holdout and golden datasets with provenance and rubric versioning. | AI Eval | C01/D05; D07 |
| G04 | P0/R0 | Score groundedness, claim validity, abstention and multi-turn behaviour with adjudication. | AI Eval | G03/E06; D07 |
| G05 | P0/R0 | Run OWASP/security, injection, leakage and network-negative test suites. | QA + AppSec | F10; D06/D07 |
| G06 | P0/R0 | Execute WCAG 2.2 AA, keyboard, assistive tech, responsive and language tests. | Accessibility QA | B07; D07 |
| G07 | P0/R0 | Run load, dependency-outage, cache-revocation, retry and recovery simulations. | QA + SRE | E09/D08/H04; D07/D08 |
| G08 | P0/R0 | Test public routes, P1 commercial claims, eligibility and no false confirmations. | QA + Biz | D06/E08; D07/D10 |
| G09 | P0/R0 | Record signed non-waivable failure classes, exceptions and all golden results. | QA Lead | G04-G08; D07 |
| G10 | P1/R1 | Add staff RBAC, case creation, CRM/booking and reconciliation E2E suites. | QA Lead | I08/J09; D09/D10 |

### H  DevOps, SRE and release control  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| H01 | P0/R0 | Configure IaC, secrets, no-public-admin endpoints and isolated runtime environments. | SRE Lead | E01/F02; D08 |
| H02 | P0/R0 | Implement four-artifact manifest: app, prompt, KB, claim registry with digests. | SRE + AI/KB | C01/D03/D06; D08 |
| H03 | P0/R0 | Implement CI/CD unit, contract, AI eval, security, a11y, provenance gates. | SRE + QA | G01/H02; D07/D08 |
| H04 | P0/R0 | Build redacted OpenTelemetry traces, SLI dashboards, budget and alert routing. | SRE | E03/C04/F08; D08 |
| H05 | P0/R0 | Perform independent artifact rollback, kill-switch and safe-static drills. | SRE + Sec | H02/H03/D08; D08 |
| H06 | P0/R0 | Run provider failure, commercial stale-claim and privacy-incident runbooks. | SRE + Sec | H04/H05; D08 |
| H07 | P0/R0 | Confirm backup/restore, provider quota and regional service targets. | SRE | H01; D08 |
| H08 | P0/R0 | Run staged canary, telemetry review and signed reversal decision process. | SRE + Product | G09/H06; D08 |
| H09 | P0/R0 | Record production on-call rota, escalation targets, change freeze and incident command. | SRE Owner | H04; D08 |
| H10 | P0/R0 | Compile immutable release evidence, incident simulation proof and final four digests. | SRE + TPM | H02-H09; D08 |

Every deliverable also requires the baseline ticket Definition of Done in Section 11; workstream sections are not separate production authorizations.

### I  Staff administration and verified handoff (R1)  |  8 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| I01 | P1/R1 | Confirm staff app origin, SSO/SCIM/MFA choice and separation from public API. | Admin Lead | A03/F11; D09 |
| I02 | P1/R1 | Implement staff RBAC / ABAC roles, approvals and authorization audit. | Admin + IAM | I01; D09 |
| I03 | P1/R1 | Build ADM command center, work queue and case detail responsive states. | Admin Frontend | I02; D09 |
| I04 | P1/R1 | Build knowledge review / publish / source revoke staff workflow. | Admin + KB | I02/D03; D09 |
| I05 | P1/R1 | Implement route-only, accepted-case and rejected/unavailable handoff states. | Case Backend | I02; D09 |
| I06 | P1/R1 | Bind case lifecycle, assignment, SLA policy and verified ticket receipt. | Support Ops | I05; D09 |
| I07 | P1/R1 | Implement redacted staff analytics, audit search and governance controls. | Admin + SRE | I02; D09 |
| I08 | P1/R1 | Run role, staff-source approval, tamper, recovery, SLA and case-workflow tests. | QA + IAM | I03-I07; D09 |

### J  Commercial intake, CRM and booking (R1)  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| J01 | P1/R1 | Select secure first-party demo/trial/lead routes and authoritative forms. | Commercial | A05; D10 |
| J02 | P1/R1 | Approve privacy form fields, B2B marketing rules and regional suppression. | DPO + Commercial | F04/J01; D10 |
| J03 | P1/R1 | Build secure server-side intake with purpose evidence and receipt, no model writes. | Backend | J02/E07; D10 |
| J04 | P1/R1 | Implement per-submission idempotency, organization matching and review queue. | Backend + CRM | J03; D10 |
| J05 | P1/R1 | Select CRM provider and contract, map fields/permissions, outbox and sync adapter. | CRM Lead | J03/F05; D10 |
| J06 | P1/R1 | Select scheduling provider; bind signed webhook and actual event confirmation. | Integration Lead | J03/F05; D10 |
| J07 | P1/R1 | Build CRM retry/reconciliation, provider failure and dead-letter operations. | Integration + SRE | J05/J06; D10 |
| J08 | P1/R1 | Build consent withdrawal / suppression propagation and rights evidence. | DPO + Backend | J02/J05; D10 |
| J09 | P1/R1 | Prove lead accepted vs booking confirmed vs CRM synced independent lifecycles. | QA + Commercial | J03-J08; D10 |
| J10 | P1/R1 | Approve release with explicit CRM/calendar/vendor and Ops readiness evidence. | Product + Ops | J09/G10; D10 |

### K  Readiness, change and business-as-usual handover  |  10 work packages

| ID | Gate | Deliverable / verifiable completion | Owner | Dependencies / source |
|---|---|---|---|---|
| K01 | P0/R0 | Sign launch content ownership and revision cadence for taxonomy and commercial truth. | Product + Knowledge | D09; D03/D08 |
| K02 | P0/R0 | Verify runbooks, owner roster, P0 support contacts and security escalation. | SRE + Support | H09; D08 |
| K03 | P0/R0 | Reconcile open high/critical defects, risks, exceptions and rollback permissions. | QA + CTO | G09/H10; D07/D08 |
| K04 | P0/R0 | Approve legal/IP/privacy/vendor checklist; record any deployment limitations. | Legal + DPO | F05/K01; D06 |
| K05 | P0/R0 | Hold independent security, QA, SRE and product go/no-go with versioned evidence. | Release Board | K02-K04; D07/D08 |
| K06 | P0/R0 | Pilot/observe, sign canary results, record incident or rollback and updated SLOs. | SRE + Product | K05/H08; D08 |
| K07 | P0/R0 | Transition to BAU: on-call, backups, knowledge/editor cadence, cost/quality reviews. | Operations | K06; D08 |
| K08 | P1/R1 | Obtain separate admin/CRM/booking readiness evidence before related enablement. | Release Board | I08/J10; D09/D10 |
| K09 | P0/R0 | Archive complete signed requirements-to-release dossier and final defects list. | TPM + QA | K07; D11 |
| K10 | P0/R0 | Obtain final ownership transfer acknowledgment from receiving operating teams. | Global CTO | K09; D11 |

## 13  Build sequencing: first end-to-end slice and integration contract freeze

Demonstrate an R0 “thin vertical slice” before parallelising the full interface. The minimum slice must execute: open widget -> establish anonymous session -> classify public intent -> query permitted signed knowledge -> validate commercial evidence if relevant -> bounded model generation -> post-gate output -> render cite and official CTA -> emit redacted telemetry. Also exercise denied private-data intent, cache revocation and provider-offline fallback in the same slice.

| Sequence | Exit test | Stop rule |
|---|---|---|
| Architecture contract freeze | Service C4, IAM/egress boundaries, OpenAPI public path and model provider control agreed. | Stop if actual deployed service cannot meet public/private separation. |
| Signed public-source slice | One approved source + active claim + revoke/rollback; citation retains immutable release ID. | Stop if private document can appear in retrieval. |
| Typed session/turn slice | Idempotent POST, one-active-generation, schema errors and redacted request tracing. | Stop if uncontrolled model tool or private API invocation exists. |
| UX flow and response gating | Desktop/mobile answer, refuse, handoff route and error states match designs. | Stop if paid/trial/security claim is unsupported. |
| CI feedback loop | All source IDs and test fixtures attached to PR, automated policy and contract run visible. | Stop if P0 evidence absent; do not defer safety to pilot. |

## 14  Test, risk and compliance evidence inheritance

Predecessor acceptance tests are retained under their original IDs and versions. This capstone adds an ownership/evidence mapping; it does not claim that existing catalogues have been executed. New cases from this plan should use WBS-AT-xxx, reference predecessor IDs and avoid duplicate wording for the same behaviour.

| Source | Existing acceptance coverage | Handover requirement |
|---|---|---|
| Doc 01 | Product functional and wireframe requirements | Approved UI version, module categories, persona coverage and no private-data access. |
| Doc 02 | 36 golden / conversation scenarios and intent expectations | Prompt version, negative routing, exact approved user copy and observed runs. |
| Doc 03 | 30 KB/source acceptance scenarios | Source registry, signed release digest, redaction, taxonomy and revocation proofs. |
| Doc 04 | 40 architecture acceptance scenarios | Network/identity tests, model adapter, runtime isolation and trace diagrams. |
| Doc 05 | 52 API/event contract tests (CT-001–052) | Versioned OpenAPI/schema, retry, concurrency, event/deduplication and webhook tests. |
| Doc 06 | 60 security tests (SEC-T001–060) | Threat disposition, direct forbidden routes, prompt-injection and privacy rights. |
| Doc 07 | 84 master QA scenarios and scoring rubric | Execution coverage, immutable failures, holds, rubric and named quality sign-off. |
| Doc 08 | 48 operational tests (OPR-001–048) | Canary, release manifest, monitoring, rollback and incident tabletop proof. |
| Doc 09 | 60 staff admin / handoff scenarios | Staff identity, admin approval workflow and authoritative case receipts. |
| Doc 10 | 72 commercial conversion tests | Lead receipt, CRM sync, booking lifecycle, consent and no implied subscriptions. |

## 15  Gate review: mandatory entry, evidence and approval rights

| Gate | Exit condition | Evidence bundle | Required approvers |
|---|---|---|---|
| G0 | Requirements and reality reconciled | Repository/service inventory, architecture delta, consent/vendor/data owners, R0 and R1 scope. | Product, CTO, Security, DPO |
| G1 | Architecture and contractual foundations | ADR decisions, public/private network proof, API version decision, RAG/claim design and vendor mode. | CTO, Architect, Security |
| G2 | Protected vertical slice | CI run with public happy path, denial, grounded price, safe fallback, citation and logs. | Backend, AI, QA, Security |
| G3 | Full engineering assurance | All P0/Doc07 required suites, accessible UX, severe risk remediation and DPO disposition. | QA, Security, DPO, Product |
| G4 | Operational readiness | 4 digests, rollback drill, SRE alerts, rate/cost limits, incident runbooks, SLO owner. | SRE, Security, QA |
| G5 | R0 production GO/NO-GO | Signed evidence manifest, critical backlog nil, pilot plan, permitted markets, brand and source state. | Founder/release approver; CTO/Sec/DPO/QA/SRE |
| G6 | Separate R1 enablement | Admin RBAC, staff cases, approved CRM/scheduler, data protection, vendor receipt tests. | Product, CTO, Security, DPO, Ops |
| G7 | BAU closeout | Handover acceptances, cost/quality cadence, on-call, defects, audits and change owner. | Operations Owner, SRE, Product |

A GO is valid only for its explicit release manifest, markets, features and time window. CONDITIONAL GO requires an approved non-critical exception with owner, deadline and rollback trigger; no P0/critical security, privacy or fabricated material commercial claim can be waived through ordinary change management.

## 16  Release decision record and non-waivable launch stops

| Trigger | Mandatory resolution before release |
|---|---|
| Any anonymous customer-data reachability | Prove route blocked in production-equivalent network environment; re-run security suite after change. |
| Material commercial/trust hallucination | Fix source, claim resolver or policy; rerun adverse and affected-regression corpora. |
| Revoked/expired source remains answerable | Stop/pause relevant claim route, repair cache and re-run revocation propagation. |
| Unapproved model processing / residency | DPO and Security must complete vendor/transfer controls; do not send personal data. |
| Missing security, SRE or QA signoff | No release; responsible leader cannot self-sign missing functions. |
| Rollback or kill switch cannot be executed | No production change; test four-artifact restoration, call tree and fail-safe mode. |
| Handoff/CRM false-success state | Keep affected capability disabled; evidence-backed server/provider ACK required. |
| P0 accessibility obstruction | Fix keyboard focus, accessible state, error recovery or affected journey before exposure. |

## 17  RAID register: top risks, dependencies and actionable mitigations

| Risk | Rating | Exposure | Control / response | Owner |
|---|---|---|---|---|
| R-01 | CRITICAL | Unexpected API route into financial/private tenants | Egress deny, IAM proof, negative tests; stop line. | Security + CTO |
| R-02 | HIGH | Public claims outdated or mismapped by market | Claim registry sign-off, effective dates and source conflict alarms. | Commercial + KB |
| R-03 | HIGH | LLM injection via documents or chat | Source prefilter, policy hierarchy, red-team corpus and outputs check. | AI + AppSec |
| R-04 | HIGH | Production repo differs from proposed architecture | G0 inventory, ADR delta, service/owner contract freeze. | Architect |
| R-05 | HIGH | External AI supplier data transfer/retention unresolved | Vendor/DPA, residency, logs, subprocessor and DPIA review. | DPO |
| R-06 | HIGH | False lead, case or booking confirmation | Authoritative signed receipt; route-only until then. | CRM + Support |
| R-07 | HIGH | Four artifact versions mutually incompatible | Manifest compatibility validator, canary and revert drill. | SRE |
| R-08 | HIGH | Admin role leaks PII or grants content edit+approve | Separation of duties, redacted views, policy tests. | IAM + AppSec |
| R-09 | MEDIUM | Prompt/model updates degrade answer quality | Golden+holdout eval and rollback windows. | AI Eval |
| R-10 | MEDIUM | Unsupported market or service taxonomy copy | Canonical mapping owner, release approval and UX review. | Product + KB |
| R-11 | MEDIUM | Provider failure or unexpected token-cost spike | Timeouts, quotas, circuit breakers and static fallback. | SRE |
| R-12 | HIGH | No staffed response or incident commander | Written duty rota, escalations, crisis rehearsal before pilot. | Support + SRE |
| R-13 | MEDIUM | Accessibility regressions during release | Automated + assistive tech gates and design-system assets. | Frontend + QA |
| R-14 | HIGH | Personal information retained beyond policy | Inventory/TTL, erasure tests, backups and processor evidence. | DPO + Security |
| R-15 | MEDIUM | R1 CRM/scheduler procurement delays R0 | Adapter-off / route-only first; vendor selection separate gates. | TPM + Commercial |
| R-16 | HIGH | Brand/IP name used publicly before clearance | Internal display token and release name/legal gate. | Brand + Legal |

## 18  Decision register: blockers, owners and evidence before activation

| ID | Open decision | Accountable | Gate | Required evidence |
|---|---|---|---|---|
| DEC-01 | What repository, hosting, gateway and identity services are real? | CTO / Architect | G0 | Code+environment inventory |
| DEC-02 | What is the authoritative ZoikoSuite taxonomy/version? | Product | G0 | Approved canonical mapping |
| DEC-03 | Who approves market, plan, trial and integration claims? | Commercial + Legal | G0 | Signed governance matrix |
| DEC-04 | Which public chatbot name and brand asset may go live? | Brand + Legal | G5 | Cleared release assets |
| DEC-05 | Which LLM/search vendor, model and region are contractually approved? | DPO + Security | G1 | Contracts / model profile |
| DEC-06 | What retention/deletion schedule and cookie choices are lawful? | DPO | G1 | DPIA/notice disposition |
| DEC-07 | Which public endpoint path, transport, session and replay TTL? | Backend + Sec | G1 | OpenAPI/ADR |
| DEC-08 | What traffic profile, error budget and SLOs are supported? | SRE + Product | G4 | Capacity/load baselines |
| DEC-09 | Which service owns signed public knowledge and cache revocation? | Knowledge + SRE | G1 | Release schema |
| DEC-10 | Who receives visitors via official handoff routes? | Support Ops | G2 | Verified destination registry |
| DEC-11 | Which staff IAM, administration tenant and permission model? | IAM + Security | G6 | RBAC decision |
| DEC-12 | Which helpdesk vendor; when is case creation enabled? | Support Ops | G6 | Case ACK+security contract |
| DEC-13 | Which CRM, field mapping, owner routing and suppression rules? | Commercial + DPO | G6 | Procurement/CRM contract |
| DEC-14 | Which scheduler and webhook verification agreement? | Commercial + Sec | G6 | Provider ACK contract |
| DEC-15 | What jurisdiction and language launch matrix is permitted? | Product + Legal | G5 | Market/locale register |
| DEC-16 | Who can declare GO, STOP, rollback and revoke sources? | Founder + CTO | G4 | Authority delegation |

## 19  Change management and architectural exception policy

Any change that modifies an API major schema, P1 claim rule, source classification, data flow, model provider, official destination, geographic processing, security boundary or success receipt semantics must be submitted as a numbered change request. Review includes affected predecessor clauses, integration tests, policy/knowledge versions, privacy screening and rollback. No “minor UI fix” may silently bypass evidence when it changes behavior.

| Change tier | Examples | Approval and evidence |
|---|---|---|
| C0 editorial safe | Approved spelling adjustment with no claim, role or process impact. | Content Owner, preview tests, version and cache refresh. |
| C1 compatible engineering | Add optional field or diagnostic metric without payloads, unchanged authority boundary. | Owning tech lead, contract tests, SRE review. |
| C2 material product/control | New plan assertion, provider integration, role permission or data collection field. | Product, Security, DPO/Legal if applicable, QA, ADR and rollout. |
| C3 new regulated authority | Customer financial data access, payments/filings, authenticated account operations. | Separate programme and new full architecture, legal and security gating. |

## 20  Final evidence package: minimum immutable manifest

Figure 3 — Audit-ready release evidence, not a narrative-only status report.

| Artifact / folder | Required contents / verification |
|---|---|
| 00-governance | Scope approval, R0/R1 charter, ADRs, architecture delta, risk/decision register, named owner records. |
| 01-product-and-ux | Signed desktop/mobile/accessible assets; canonical taxonomy; content and locale registry; UI states. |
| 02-api-and-model | OpenAPI/AsyncAPI or equivalent typed schemas; prompts/policies; model/KB specs and gateway contracts. |
| 03-knowledge-and-claims | Source registry, approvals, signed release, source revocations, claim proof, conflict audit. |
| 04-security-privacy | Threat model, dependency/SBOM, negative tests, DPIA/vendor disposition, retention and deletion test. |
| 05-evaluation-quality | Test/run versions, golden+holdout sets, scoring rubric, failure findings, accessibility and performance proof. |
| 06-operations | Release manifests/digests, dashboards, SLO owner, alert evidence, rollback drill, on-call and runbooks. |
| 07-commercial-and-handoff | Official route registry; later staff/lead/booking receipts, vendor contracts, consent and suppression proof. |
| 08-release-decision | Exception register, mandatory signatures, rollout scope, change ticket, GO/NO-GO and restoration pointers. |
| 09-bau-acceptance | Owner handover, knowledge cadence, ongoing model drift review, incident/change runbook and final residual risks. |

## 21  Evidence and handover machine-readable schemas

Release manifest (illustrative JSON skeleton; values are not live releases)

{  "release_id": "suivo-r0-YYYYMMDD-0001",  "scope": "R0_PUBLIC_GUIDANCE",  "environment": "production-candidate",  "artifacts": {    "application_sha256": "<sha256>",    "policy_bundle_sha256": "<sha256>",    "public_knowledge_sha256": "<sha256>",    "claim_registry_sha256": "<sha256>"  },  "compatibility": {    "schema_major": 1,    "canonical_taxonomy_version": "<approved-id>",    "public_api_version": "v1"  },  "assurance": {    "functional_run": "<evidence-ref>",    "ai_eval_run": "<evidence-ref>",    "security_run": "<evidence-ref>",    "accessibility_run": "<evidence-ref>",    "rollback_drill": "<evidence-ref>"  },  "approvals": {    "product": "PENDING",    "cto": "PENDING",    "security": "PENDING",    "dpo": "PENDING",    "qa": "PENDING",    "sre": "PENDING"  },  "decision": "NO_GO_UNTIL_SIGNED",  "market_scope": [],  "exception_refs": [],  "support_owner": "<staff-role>"}

Work-package change / acceptance receipt (illustrative)

{  "work_package_id": "SUIVO-11-E06",  "source_clauses": [    "ENG-04/08",    "ENG-05/18"  ],  "owner_role": "BACKEND_LEAD",  "dependency_ids": [    "C05",    "D05",    "D06",    "E04"  ],  "git_ref": "<reviewed-commit>",  "contract_test_ids": [    "CT-<approved-id>"  ],  "negative_test_ids": [    "SEC-T<approved-id>"  ],  "release_compatibility_ref": "<manifest-version>",  "qa_result": "NOT_EXECUTED",  "evidence_uris": [],  "exceptions": [],  "acceptor": "<named-engineer>"}

## 22  Pilot, adoption and operational transfer obligations

| Operating cadence | Accountable function | Required output |
|---|---|---|
| Daily during canary | SRE + QA | Fresh P0 incidents, availability, error, fallback, cost, claim freshness and kill-switch status. |
| Weekly after pilot | Product + Knowledge | Top misunderstood intents, answer gap register, newly approved/retired sources and taxonomy changes. |
| Weekly during active integrations | Support + Commercial | Ticket intake and CRM sync reconciliations; no route click counted as accepted receipt. |
| Monthly operations | CTO + Security | Dependency posture, vendor/service changes, access recertification and audit exceptions. |
| Quarterly governance review | Product + DPO + QA | Privacy/data-map review, adverse test refresh, model drift findings and brand/market scope. |
| Every high-impact change | Release Board | Scoped sign-off and regression evidence on claim, model, policy, knowledge or integration alteration. |

BAU acceptance requires a named 24/7 or business-hours on-call model consistent with approved service objectives; no specific coverage level is presumed. Business and engineering escalation channels must be tested, not merely listed.

## 23  Formal engineering handover checklist

| ID | Check / evidence required | Status |
|---|---|---|
| H01 | Scope and R0/R1 definitions, owner and market/locale matrix signed. | OPEN — verify |
| H02 | Codebase/infrastructure audit reconciled to target C4 and accepted ADRs. | OPEN — verify |
| H03 | Public API and staff/admin routes physically isolated by IAM/network tests. | OPEN — verify |
| H04 | Official logo/favicon, Suivo public-name clearance and text assets signed. | OPEN — verify |
| H05 | Canonical ten-module taxonomy/source version reconciled and reviewer-approved. | OPEN — verify |
| H06 | Public KB source registry, signed digest, citations and revocation drill stored. | OPEN — verify |
| H07 | Commercial claim registry signed, fresh and tested for market/plan/trial/claims. | OPEN — verify |
| H08 | Zero general-purpose model execution tools or customer business-system credentials. | OPEN — verify |
| H09 | Session/turn APIs, concurrency, retries, typed errors and schema versions verified. | OPEN — verify |
| H10 | Signed policies, prompt versions and model-provider processing terms approved. | OPEN — verify |
| H11 | Security threat model, positive/negative access tests and prompt injection evidence closed. | OPEN — verify |
| H12 | DPIA/privacy/data transfer/retention/deletion decisions recorded by DPO. | OPEN — verify |
| H13 | WCAG 2.2 AA user journeys and accessible error/notification states tested. | OPEN — verify |
| H14 | Golden corpus, holdouts, adjudication scores and all P0 QA gates signed. | OPEN — verify |
| H15 | Load, resilience and provider-offline safe fallback evidence approved. | OPEN — verify |
| H16 | Four artifact manifest and independent rollback runbook demonstrated. | OPEN — verify |
| H17 | Operational SLIs, dashboard alerts, incident and abuse runbooks linked. | OPEN — verify |
| H18 | Support destination and route-only behavior confirmed; no false ticket acknowledgment. | OPEN — verify |
| H19 | R1 admin/staff roles and cases enabled only after staff RBAC audit and receipts. | OPEN — verify |
| H20 | R1 secure lead, CRM and booking enabled only after vendor and privacy gates. | OPEN — verify |
| H21 | No unresolved critical/P0 defects; lesser exceptions are owner/time/mitigation-signed. | OPEN — verify |
| H22 | Pilot GO/NO-GO, production canary, reversal and after-action records filed. | OPEN — verify |
| H23 | Source editors, responders, incident commander and owner roster accepted. | OPEN — verify |
| H24 | Operations and commercial receiving teams sign maintenance/response cadence. | OPEN — verify |

## 24  Approval and sign-off template — no automatic authorisation

| Functional approval | Name / reference | Decision / date |
|---|---|---|
| Founder / commercial scope | ____________________________ | PENDING / ______ |
| Global CTO / architecture | ____________________________ | PENDING / ______ |
| Product Owner / brand | ____________________________ | PENDING / ______ |
| Security / AppSec | ____________________________ | PENDING / ______ |
| DPO / Legal | ____________________________ | PENDING / ______ |
| Quality and AI evaluation | ____________________________ | PENDING / ______ |
| SRE / Incident Operations | ____________________________ | PENDING / ______ |
| Knowledge and claims owners | ____________________________ | PENDING / ______ |
| Support / Commercial (R1 if applicable) | ____________________________ | PENDING / ______ |

| FINAL DECISION RULE The release record must state R0 or R1, approved markets, enabled feature flags, exact four artifact digests, required test and assurance run IDs, residual-risk dispositions, decision result, approver identities, rollback owner and follow-up review. Without this evidence the correct recorded outcome is NO-GO / NOT VERIFIED. |
|---|

## 25  Standards, documentary references and controlled next step

NIST Secure Software Development Framework SP 800-218 (final v1.1) — https://csrc.nist.gov/pubs/sp/800/218/final

NIST SSDF version 1.2, initial public draft (not a final replacement) — https://csrc.nist.gov/pubs/sp/800/218/r1/ipd

OWASP Top 10 for LLM Applications 2025 — https://genai.owasp.org/resource/owasp-top-10-for-llm-applications-2025/

W3C Web Content Accessibility Guidelines WCAG 2.2 — https://www.w3.org/TR/WCAG22/

Google SRE Workbook / production readiness reviews — https://sre.google/workbook/production-readiness/

OpenTelemetry specifications — https://opentelemetry.io/docs/specs/

OpenAPI specification — https://spec.openapis.org/oas/latest.html

AsyncAPI specification — https://www.asyncapi.com/docs/reference/specification/latest

CloudEvents specification — https://cloudevents.io/

ZoikoSuite current public site — https://zoikosuite.com/

ZoikoSuite core modules — https://www.zoikosuite.com/core-modules

Predecessor Docs 01–10 — Controlled ZoikoSuite Suivo chatbot engineering Word documents, series dated 24 October 2026

Standards are reference frameworks, not evidence of certification or compliance. Public content changes over time. Internal source releases, contract owners and signed project architecture govern live claims.

CLOSE OF DOCUMENT 11. Submit this roadmap for individual Founder/Global CTO approval. Do not initiate additional document series or represent tasks as completed solely because this handover plan is issued.
