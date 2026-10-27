# ROADMAP & WBS - ZoikoSuite Suivo Chatbot
Generated: 2026-10-27 11:52:42
Branch: suivo/phase-0-audit

## Work packages (from MASTER_PROMPT)
WP01 Discovery, document audit and requirements baseline (current)
WP02 Product scope, UX, conversation design and approved prompts
WP03 Public knowledge inventory, taxonomy, source governance and claim registry
WP04 Technical architecture, repo structure, isolation and infrastructure decisions
WP05 Public API, sessions, turns, response schemas and events
WP06 Retrieval, structured claim lookup, evidence filtering and citations
WP07 Frontend widget and accessible UI states
WP08 Security, privacy, abuse prevention and provider integration
WP09 QA, automated tests, red-team testing and acceptance
WP10 CI/CD, staging, deployment, monitoring and incident response
WP11 Admin console, source approvals, release management and human handoff
WP12 Commercial conversion, lead intake, CRM adapter and booking lifecycle (blocked)
WP13 Production readiness, pilot, launch, operational ownership and handover

## Phases 0-16 (from MASTER_PROMPT)
0. Document audit and requirements traceability (in progress)
1. Existing-system discovery, isolation design and ADRs
2. Repository setup, dev environment, coding standards, isolation checks
3. Frontend widget skeleton and accessible states (needs frontend repo)
4. Public API, session lifecycle, validation and error contracts
5. Approved public knowledge ingestion and source registry
6. Retrieval, structured claim lookup, evidence filtering and citations
7. Model adapter, prompt layers, generation and output validation (stub until provider approval)
8. End-to-end vertical slice
9. Pricing, trial, product, integration, security and handoff intents
10. Privacy, abuse prevention and threat mitigations
11. Admin console, approvals, release management and revocation
12. Commercial conversion and CRM adapter (blocked until approvals)
13. QA: golden set, security, accessibility and performance tests
14. CI/CD, staging, monitoring, alerting, rollback and kill switch
15. UAT, controlled pilot, readiness review and launch (approval-gated)
16. Documentation, runbooks, training and final handover

## Critical path
Phase 0 -> Phase 1 (discovery/isolation/ADRs) -> Phase 2 (setup/isolation checks) -> Phase 4 (API) -> Phase 5/6 (knowledge/retrieval) -> Phase 7 (model stub) -> Phase 8 (vertical slice) -> Phase 13 (QA) -> Phase 14 (staging/ops) -> Phase 15 (pilot/launch gated)

## What can start now
- Phase 0 complete (steps 0.1-0.6 done) - stop and wait for approval before Phase 1
- Create approvals-log.md structure
- Inspect repo for existing patterns (services/, assistant-rag-svc) - can do in Phase 1

## Blocked
- Phase 3 needs frontend repo location
- Phase 7 provider choice needs approval (use stub)
- Phase 12 needs Doc10 approvals + receiving systems
- Phase 15 needs all approvals + evidence

## Minimum evidence/approvals needed before next phases
- Approval to proceed to Phase 1
- Named approvers for roles (or continue as pending)
- Frontend repo location (or defer Phase 3)
- Provider/subprocessor approval (or continue with stub)
- CRM/booking approval + contracts (or skip Phase 12)
