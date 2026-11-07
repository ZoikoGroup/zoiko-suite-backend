# ARCHITECTURE PROPOSAL - ZoikoSuite Suivo Chatbot
Generated: 2026-10-27 11:52:26
Branch: suivo/phase-0-audit
Status: Proposed (separate from doc requirements)

## Trust boundaries (proposed)
- **Public boundary:** Widget on public website -> Public API (suivo public service). No direct access to private enterprise services.
- **Knowledge boundary:** Public knowledge store/index only. Separate from authenticated enterprise search. Eligibility enforced in app code.
- **Model boundary:** Model provider calls go through adapter with redaction, rate/spend limits, timeouts.
- **Handoff boundary:** Only approved destinations (Doc09); CRM writes only if approved (Doc10).

## Proposed components (to be validated against docs)
- **suivo-api** (public service) - sessions/turns, validation, orchestration
- **suivo-rag** - retrieval, evidence filtering, citations, abstention
- **suivo-prompt** - prompt layers, guardrails
- **suivo-model-adapter** - provider abstraction (stub first)
- **suivo-knowledge-ingest** - ingestion pipeline with approvals
- **suivo-admin** (internal) - approvals, releases, revocation
- **suivo-widget** (frontend) - in public website repo

## Isolation requirements
- No imports from private enterprise services (accounting/banking/payroll/tax/ledger/HR/etc.)
- No shared DB/schema/index/creds/routes
- Inspect assistant-rag-svc - learn patterns only, no reuse
- Add automated isolation check

## ADRs needed
- ADR-001: Service location/structure in backend repo
- ADR-002: Public knowledge store/index technology (separate from existing)
- ADR-003: Model provider selection (blocked until approval)
- ADR-004: Frontend integration approach (needs frontend repo)
- ADR-005: CRM/booking adapter approach (blocked until Doc10 approvals)

## Notes
- All decisions marked Proposed until approved per MASTER_PROMPT
- Prefer existing stack - will inspect in Phase 1
