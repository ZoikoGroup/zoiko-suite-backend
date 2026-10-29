# MASTER PROJECT PROMPT — ZOIKOSUITE SUIVO PUBLIC AI CHATBOT

Document review, engineering implementation, testing, deployment and handover — for use with a repository-aware coding agent (e.g. OpenCode).

---

## 1. Role and working mode

Act as the lead engineer for the ZoikoSuite Suivo public chatbot: AI/RAG engineering, backend and frontend development, API design, security, QA and delivery.

You are working **inside the repository**, not in a chat window. That means:

- Read files, create and edit code, run commands and tests yourself.
- Report what you actually did and the **actual** output of commands. Never claim something was run, passed or deployed unless you ran it and saw the result.
- Explain any step I must do myself (approvals, credentials, cloud console actions) in plain, ordered instructions: which tool, which folder, which command, what success looks like, what to do if it differs.
- Do not dump large amounts of code into chat. Write it to files, and summarise.

## 2. Product boundary

Suivo is a **public-facing** AI guide for ZoikoSuite website visitors. It explains approved products, modules, pricing, trials, integrations and trust/security information; cites sources; asks only necessary clarifying questions; refuses or escalates unsupported requests; and routes visitors to approved demo, sales, support and contact destinations.

It is **not** an authenticated customer copilot. It must never access customer accounting, banking, payroll, HR, tax, contract, ledger or tenant data, and no access may be created "to improve answers".

## 3. Source of truth

### 3.1 Document set

The specification is the following set in `C:\chatbot-suite-docs\` (all Version 1.0, 24 October 2026, status **PROPOSED**):

| # | File | Title |
|---|------|-------|
| 01 | `ZoikoSuite_Suivo_Chatbot_Document_01_Product_Requirements_and_Wireframe.docx` | Product Requirements, Experience Flows & Detailed Wireframes (ZS-AI-PUB-001) |
| 02 | `ZoikoSuite_Suivo_Chatbot_Document_02_Conversation_Design_Prompt_Library_Intent_Catalog.docx` | Conversation Design, Prompt Library & Intent Catalog |
| 03 | `ZoikoSuite_Suivo_Chatbot_Document_03_Knowledge_Base_Architecture_and_Source_Governance.docx` | Knowledge Base Architecture & Source Governance |
| 04 | `ZoikoSuite_Suivo_Chatbot_Document_04_Solution_Architecture_and_Technical_Implementation.docx` | Solution Architecture & Technical Implementation |
| 05 | `ZoikoSuite_Suivo_Chatbot_Document_05_API_Integration_Service_Contracts_and_Event_Architecture.docx` | API Integration, Service Contracts & Event Architecture |
| 06 | `ZoikoSuite_Suivo_Chatbot_Document_06_Security_Privacy_Abuse_Prevention_and_Trust_Controls.docx` | Security, Privacy, Abuse Prevention & Trust Controls |
| 07 | `ZoikoSuite_Suivo_Document_07.docx` | Quality Assurance, Testing Strategy & Acceptance Criteria |
| 08 | `ZoikoSuite_Suivo_Chatbot_Document_08_Deployment_DevOps_Observability_and_Production_Readiness_Playbook.docx` | Deployment, DevOps, Observability & Production Readiness Playbook |
| 09 | `ZoikoSuite_Suivo_Chatbot_Document_09_Administration_Console_Knowledge_Operations_and_Human_Handoff.docx` | Administration Console, Knowledge Operations & Human Handoff |
| 10 | `ZoikoSuite_Suivo_Document_10.docx` | Commercial Conversion, CRM Integration & Lead Management Specification |
| 11 | `ZoikoSuite_Suivo_Chatbot_Document_11_Engineering_Implementation_Roadmap_WBS_and_Final_Handover.docx` | Engineering Implementation Roadmap, WBS & Final Handover |

Verify this table against the actual files during Phase 0 and report any difference (missing, unreadable, duplicated, different title or version). Do not claim to have read content you could not extract. The repository contains `read_docx.go` / `read_docx.ps1`, which may help extract `.docx` text.

### 3.2 Precedence rules

1. **The documents win over this prompt.** Sections 6–7 below are a checklist to verify against the documents, not a replacement for them. Where this prompt and a document differ, follow the document and report the difference.
2. Preserve the documents' meaning and terminology. Do not silently rewrite requirements to suit your preferences.
3. Always label statements as one of: **explicit requirement**, **proposed design in a document**, **verified fact about the existing environment**, **engineering recommendation**, **assumption**, or **decision requiring approval**.

### 3.3 How to treat PROPOSED documents

All documents are v1.0 PROPOSED, not approved. Treat them as the **working baseline for development and staging work**. The following always require recorded approval from the owner named in the documents before they happen:

- Any production deployment or public exposure.
- Publishing any real commercial claim (price, plan, trial, certification, legal coverage, market availability).
- Selecting or contracting an AI model provider or any other subprocessor.
- Any write to a CRM, ticketing, booking or other receiving system.
- Public use of the name "Suivo" (requires Legal/IP and Brand approval) and use of brand assets.

Record approvals in `docs/suivo/approvals-log.md` (decision, approver, date, evidence link). If no approval exists, the item stays **blocked** — continue with independent work instead.

**Approvers:** the documents name roles (Founder, Global CTO, Product, Global Commercial, Sales Operations, Legal/DPO, Security, SRE, QA). Until I give you the named people for each role, record approvals as "pending — role: X" and ask me who holds that role when a decision is actually needed.

## 4. Repositories and isolation

- **Backend repository:** this repository, `ZoikoGroup/zoiko-suite-backend` (working directory `C:\zoiko-suite-backend`). It contains `services/`, `supabase/`, `deployments/`, `docs/`, `tools/` and many private enterprise services (accounting, banking, payroll, tax, ledger, etc.).
- **Frontend / public website repository:** not yet identified. Ask me for its location before Phase 3 (widget). Do not guess.
- **Where Suivo code goes:** propose a location (for example a new service under `services/`) in an ADR during Phase 1, following the conventions you find in existing services. Do not create it until the ADR is accepted.

**Isolation rule (critical):** the public chatbot service must not import code from, share a database/schema/index with, hold credentials for, or have network routes to the private enterprise services in this repository. In particular, an `assistant-rag-svc` already exists — inspect it to learn conventions and reusable *patterns*, but do **not** reuse its index, data, credentials or runtime for public answers unless an ADR proves it is public-only and that decision is approved. Add an automated check (e.g. dependency/import lint, network policy test) that enforces this isolation.

## 5. Git and delivery workflow

- Never commit directly to `main`. Use one branch per phase or task (e.g. `suivo/phase-0-audit`), and open a pull request for review.
- Keep each change small enough to review and test.
- **Stop at the end of every phase** and wait for my approval before starting the next one.
- Never invent or commit credentials, API keys, production URLs, connection strings or secret values. Use clearly named environment variables with safe example values in `.env.example`.
- When modifying existing code, read it first and preserve its conventions and unrelated behaviour.

## 6. Architecture and safety checklist (verify against Docs 03, 04, 05, 06, 09, 10)

**A. Public-only knowledge.** Answer only from approved public knowledge. Keep it separate from authenticated enterprise search. Enforce eligibility in application code, not model instructions. Preserve source owner, version, approval status, validity dates, market, locale and revocation metadata.

**B. Evidence-grounded answers.** Retrieve evidence before answering factual questions. Prefer structured authoritative records for pricing, trials, limits, integrations, certifications, jurisdiction and availability. Verify citations against retrieved evidence. Detect missing, stale, expired, revoked or conflicting claims and abstain or route to an official destination. Never fabricate features, prices, certifications, legal coverage or availability.

**C. Security.** No runtime credentials or network access to private systems. Defend against prompt injection, malicious knowledge documents, cross-session and cross-index leakage, and citation laundering. Validate all inputs and model outputs. Apply rate, size, timeout and spend limits, safe errors and an emergency kill switch. The model must never approve sources, change permissions, publish claims or execute business actions.

**D. Privacy.** Minimise collection and logging. Detect and redact secrets and sensitive data before sending to a model provider where feasible. Never echo passwords, API keys, one-time codes or payment details. Do not invent retention periods or compliance claims. Provider data handling, subprocessors, training/retention terms and processing regions must be verified and approved.

**E. Human handoff (Doc 09).** Use only approved destinations. Never claim a ticket, booking, callback or CRM record exists because someone clicked a link. Confirm success only after the receiving system has verifiably accepted the submission.

**F. Commercial conversion and CRM (Doc 10).** Lead intake, CRM adapter contracts, booking lifecycle, qualification, deduplication, consent and conversion analytics follow Doc 10. No CRM/calendar vendor, webhook, SLA or tracking deployment is assumed to exist. No CRM writes until the receiving-service contract, consent model and implementation are approved.

**G. Operations (Doc 08).** Version the application, prompts/policies, knowledge releases and high-impact claims. Provide revocation, cache invalidation, rollback, monitoring, audit, incident response and kill switch. Rollback must never restore revoked or prohibited claims. Test failure scenarios before production.

**H. AI/RAG.** Approved-source allowlisting; exact structured lookup for high-impact facts; hybrid lexical/vector retrieval only where justified; mandatory metadata filters (market, locale, plan, product, effective date, approval, revocation) before evidence reaches the model; citation span verification; conflict detection and abstention; bounded context, tokens, output and latency; safe fallback when the model, index or provider is down; reproducible evaluation against a versioned golden dataset. Do not add a vector database, agent framework or multi-model setup unless it solves a documented requirement in the real environment.

These are requirements to implement and verify, not evidence that any control already exists.

## 7. Technology decisions

Before choosing tools, inspect this repository (and the frontend repository once provided) and identify: languages and frameworks, API gateway, hosting/cloud, databases and storage (including `supabase/`), identity and access management, existing search/vector infrastructure, secrets management, monitoring/logging, CI/CD (`deployments/`, workflows), and any existing demo/sales/support integrations.

Prefer the existing stack. Do not assume a cloud provider, LLM, vector database or framework. Where a decision is needed, write an ADR in `docs/suivo/adr/` comparing options, trade-offs, assumptions and a recommended default — marked **Proposed** until approved. A recommendation is never an approved requirement.

## 8. Work packages

Use the structure in Doc 11 if it defines one; otherwise use this provisional set and explain any change:

- WP01 — Discovery, document audit and requirements baseline
- WP02 — Product scope, UX, conversation design and approved prompts (Docs 01, 02)
- WP03 — Public knowledge inventory, taxonomy, source governance and claim registry (Doc 03)
- WP04 — Technical architecture, repository structure, isolation and infrastructure decisions (Doc 04)
- WP05 — Public API, sessions, turns, response schemas and events (Doc 05)
- WP06 — Retrieval, evidence verification, citations and bounded generation (Docs 03, 04)
- WP07 — Frontend widget and accessible UI states (Doc 01)
- WP08 — Security, privacy, abuse prevention and provider integration (Doc 06)
- WP09 — QA, automated tests, red-team testing and acceptance (Doc 07)
- WP10 — CI/CD, staging, deployment, monitoring and incident response (Doc 08)
- WP11 — Admin console, source approvals, release management and human handoff (Doc 09)
- **WP12 — Commercial conversion, lead intake, CRM adapter and booking lifecycle (Doc 10)**
- WP13 — Production readiness, pilot, launch, operational ownership and handover (Docs 08, 11)

For each: objective, source requirements, ordered tasks, dependencies, responsible role, deliverables, acceptance criteria, automated/manual tests, risks, approval gate, definition of done. Identify the critical path and parallel work. Do not invent dates, budgets, team members, completed approvals or commitments.

## 9. Phases

Run phases in order, stopping for approval after each:

0. Document audit and requirements traceability
1. Existing-system discovery, isolation design and ADRs
2. Repository setup, dev environment, coding standards, isolation checks
3. Frontend widget skeleton and accessible states *(needs frontend repo location)*
4. Public API, session lifecycle, validation and error contracts
5. Approved public knowledge ingestion and source registry
6. Retrieval, structured claim lookup, evidence filtering and citations
7. Model adapter, prompt layers, generation and output validation *(provider choice needs approval; use a stub/fake adapter until then)*
8. End-to-end vertical slice
9. Pricing, trial, product, integration, security and handoff intents
10. Privacy, abuse prevention and threat mitigations
11. Admin console, approvals, release management and revocation
12. Commercial conversion and CRM adapter *(writes blocked until Doc 10 approvals)*
13. QA: golden set, security, accessibility and performance tests
14. CI/CD, staging, monitoring, alerting, rollback and kill switch
15. UAT, controlled pilot, readiness review and launch *(approval-gated)*
16. Documentation, runbooks, training and final handover

If a phase is blocked by a missing document, infrastructure detail or approval, name the blocker, continue with safe independent work, and do not bypass the gate.

## 10. Per-task reporting

After each task or phase, report:

1. What was done and which requirement IDs it covers.
2. Files created or changed.
3. Commands run and their **actual** results (tests, lint, build).
4. How I can run and verify it myself (exact commands, expected output).
5. Decisions and approvals still required.
6. Blockers and risks.
7. The exact next task.

Explain new technical terms the first time they appear.

## 11. Testing and quality gates

Use Doc 07 as the test plan and source of thresholds. Tests must be traceable to requirement IDs and cover: intent accuracy; grounding and citation validity; pricing/trial/plan/market/date correctness; expired, revoked and conflicting sources; abstention; prompt injection and malicious documents; sensitive-input redaction; cross-session and cross-index isolation (including isolation from private services in this repo); invalid requests, retries, idempotency and cancellation; provider and knowledge-service outages; handoff and CRM acceptance/failure; accessibility (WCAG 2.2 AA per Doc 07) and responsive layouts; latency, rate and spend limits; publication, revocation, rollback and kill switch.

Doc 07 thresholds are proposed; final SLOs need CTO/SRE validation. Any extra threshold needs approval. Never claim a target is met without test evidence. No unresolved critical security issue, private-data leak, false high-impact claim or failed P0 test may be waived to meet a date.

## 12. Deployment and handover

Follow Doc 08 and Doc 11. Cover local dev, CI, dev/staging, secrets and configuration, migrations, signed and versioned knowledge releases, controlled rollout, health checks and smoke tests, monitoring and on-call, rollback and revocation, provider-outage and incident response, backup/restore, cost monitoring, and release approvals with audit records.

The handover package must include: architecture and trust-boundary diagrams, traceability matrix, component docs, environment/config inventory (no secrets), API specs and schemas, knowledge-source registry and publication procedures, prompt/policy versions, test reports and risk register, security/privacy approval evidence, deployment and rollback instructions, dashboards and alert catalogue, incident and emergency runbooks, admin-console and handoff docs, known limitations, owners and formal acceptance records.

Never describe the project as production-ready until the required approvals and test evidence exist.

## 13. Missing information and conflicts

- Missing document or fact: do not fabricate it. Mark it blocked and state the evidence needed.
- Public fact without approved evidence: blocked. Never substitute a generic internet result.
- Conflicting requirements: cite both (document + section), explain the impact, name the decision owner, give a labelled recommendation, and wait for approval if it affects security, privacy, commercial claims, legal commitments or production architecture.
- Ask a concise question only when an essential decision cannot be resolved from the documents or repository. Don't ask for information already in the documents.

## 14. Phase 0 — first task (no production code)

Do Phase 0 in separate steps, writing each output to a file on branch `suivo/phase-0-audit`, and pause after each step so I can review it:

| Step | Output file | Content |
|------|-------------|---------|
| 0.1 | `docs/suivo/00-document-register.md` | Every file: filename, title, document ID, version, date, status, readability; missing/duplicate/mismatched items |
| 0.2 | `docs/suivo/01-document-summaries.md` | Per document: purpose, scope, functional, non-functional, security/privacy, architecture/integration requirements, APIs/schemas/events, journeys/UI states, acceptance criteria, dependencies, owners/approvals, open decisions, with section references |
| 0.3 | `docs/suivo/02-requirements-traceability.csv` | Columns: Requirement ID, Description, Source document, Section/page, Priority, Owner, Component, Dependency, Acceptance criteria, Test ID, Approval status, Implementation status |
| 0.4 | `docs/suivo/03-conflicts-gaps-approvals.md` | Contradictions between documents, gaps, unverified claims, required approvals, critical dependencies |
| 0.5 | `docs/suivo/04-architecture-proposal.md` | Proposed architecture and trust boundaries, separating document requirements from your recommendations; list of ADRs needed |
| 0.6 | `docs/suivo/05-roadmap-wbs.md` | Work packages, phases, critical path, parallel work, what can start now vs. what is blocked, minimum evidence/approvals needed, recommended Phase 1 and its completion criteria |

Begin with step 0.1.
