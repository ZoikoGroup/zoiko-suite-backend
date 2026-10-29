# CONFLICTS, GAPS, APPROVALS - ZoikoSuite Suivo Chatbot
Generated: 2026-10-27 11:52:14
Branch: suivo/phase-0-audit

## Location difference
- MASTER_PROMPT.md states docs in C:\chatbot-suite-docs\ (section 3.1). Actual files found in C:\zoiko-suite-backend\docs\chatbot-suite-docs\. Treat actual location as source of truth for this repo context.

## Document extraction limitation
- read_docx.ps1/go present but OpenXML assembly missing - cannot extract detailed text. All detailed requirements/APIs/schemas/events/acceptance criteria from docs 01-11 need to be read once deps available or manually reviewed. Marked as gaps until extracted.

## Key approvals required (per MASTER_PROMPT)
| Item | Required by | Status | Notes |
|---|------------|--------|-------|
| Production deployment/public exposure | Owner roles per docs | Pending | Blocked until approval logged in docs/suivo/approvals-log.md |
| Publishing real commercial claim (price/plan/trial/cert/legal/availability) | Owner roles per docs | Pending | Blocked |
| Selecting/contracting AI model provider or subprocessors | Owner roles per docs | Pending | Blocked |
| Any write to CRM/ticketing/booking/receiving system | Owner roles per docs | Pending | Blocked (Doc10) |
| Public use of name 'Suivo' and brand assets | Legal/IP and Brand | Pending | Blocked |
| SLO/threshold changes beyond Doc07 | CTO/SRE | Pending | Needs validation |
| Adding vector DB/agent framework/multi-model | Architecture/CTO | Pending | Must solve documented requirement in real environment |

## Critical gaps
- Frontend repo location unknown (needed for Phase 3 widget) - ask user
- Named approvers for roles not provided - record as pending-role until specified
- Provider selection not made - use stub/fake adapter until approval
- CRM/booking receiving systems unknown - no assumptions
- Isolation from assistant-rag-svc needs inspection (exists in repo per prompt) - must verify no reuse of index/data/creds/routes
- Existing stack details not yet inventoried (Phase 1)

## Potential conflicts to watch for
- Need to reconcile any differences between prompt checklist (sections 6-7) and actual doc content once extracted
- Ensure isolation rule is enforced even if patterns seem reusable
- CRM writes strictly gated per Doc10
